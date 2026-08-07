package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// ErrInvalidKey is returned by Path (and anything routed through it) when
// kind or key fail containment validation. The HTTP layer maps it to 400,
// distinguishing a malformed request from a genuine miss (os.ErrNotExist).
var ErrInvalidKey = errors.New("invalid key")

// kindPattern bounds the kind path segment to a short, lowercase,
// alphanumeric-plus-hyphen token. This makes kind incapable of carrying a
// path-traversal shape itself and keeps it from colliding with the store's
// own reserved ".tmp-*" scratch-file prefix.
var kindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Store is a bounded, disk-persistent key/value store rooted at a single
// data directory. It has no upstream fetch path and accepts no credentials
// (CORE-07): every method here is a plain filesystem operation scoped
// under dataDir. Phases 2-4 must add their fetchers above this store, never
// inside it, so this structural guarantee holds for every future cache
// kind.
type Store struct {
	logger  *zerolog.Logger
	dataDir string
}

// NewStore creates a Store rooted at dataDir, creating the directory
// (mode 0o750) if it does not already exist.
func NewStore(logger *zerolog.Logger, dataDir string) (*Store, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("cache: store: resolving data dir %s: %w", dataDir, err)
	}

	clean := filepath.Clean(abs)

	if err := os.MkdirAll(clean, 0o750); err != nil {
		return nil, fmt.Errorf("cache: store: creating data dir %s: %w", clean, err)
	}

	return &Store{logger: logger, dataDir: clean}, nil
}

// Path resolves kind and key to an absolute filesystem path rooted at
// dataDir. It is the single choke point where untrusted key material is
// converted to a filesystem path — Put and Open must both route through
// it.
//
// Validation happens on the raw, uncleaned key before any normalisation:
// path.Clean collapses a leading ".." against a rooted path back to the
// root rather than rejecting it, which would otherwise let a traversal
// attempt like "../../etc/passwd" resolve "successfully" to a contained
// but attacker-chosen path. Rejecting any raw "." or ".."-prefixed segment
// — and any leading "/" — up front closes that gap; path.Clean is used
// only afterwards, purely to normalise things like a doubled "//".
func (s *Store) Path(kind, key string) (string, error) {
	if !kindPattern.MatchString(kind) {
		return "", fmt.Errorf("cache: store: invalid kind %q: %w", kind, ErrInvalidKey)
	}

	if key == "" || strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("cache: store: invalid key: %w", ErrInvalidKey)
	}

	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("cache: store: key %q must not be absolute: %w", key, ErrInvalidKey)
	}

	for _, seg := range strings.Split(key, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("cache: store: key %q has a segment beginning with \".\": %w", key, ErrInvalidKey)
		}
	}

	cleaned := path.Clean(key)
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("cache: store: invalid key %q: %w", key, ErrInvalidKey)
	}

	base := filepath.Join(s.dataDir, kind)
	full := filepath.Join(base, filepath.FromSlash(cleaned))

	if !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("cache: store: key %q escapes data dir: %w", key, ErrInvalidKey)
	}

	return full, nil
}

// Put stores the content read from r under kind/key, replacing any
// existing content atomically: the copy is written to a scratch file in
// the same directory and renamed into place, which is atomic on the same
// filesystem. A crash mid-write can leave an orphaned ".tmp-*" scratch
// file behind (reaped by plan 01-04's sweep), but it can never leave a
// truncated file at the final path.
func (s *Store) Put(kind, key string, r io.Reader) error {
	full, err := s.Path(kind, key)
	if err != nil {
		return err
	}

	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("cache: store: creating dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cache: store: creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename below has succeeded

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("cache: store: writing %s: %w", full, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache: store: closing temp file for %s: %w", full, err)
	}

	if err := os.Rename(tmp.Name(), full); err != nil {
		return fmt.Errorf("cache: store: renaming into place %s: %w", full, err)
	}

	return nil
}

// Open resolves kind/key, verifies the result is a regular file — never
// following a symlink, which is what stops a symlink planted inside
// dataDir from being used to read a file outside it — and returns the
// opened file plus its os.FileInfo for the caller to serve (e.g. via
// http.ServeContent). The caller is responsible for closing the file.
//
// A successful Open also touches the file's mtime as a best-effort LRU
// recency signal, since atime is unusable on noatime-mounted filesystems.
func (s *Store) Open(kind, key string) (*os.File, os.FileInfo, error) {
	full, err := s.Path(kind, key)
	if err != nil {
		return nil, nil, err
	}

	info, err := os.Lstat(full)
	if err != nil {
		return nil, nil, fmt.Errorf("cache: store: stat %s: %w", full, err)
	}

	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("cache: store: %s is not a regular file", full)
	}

	f, err := os.Open(full)
	if err != nil {
		return nil, nil, fmt.Errorf("cache: store: opening %s: %w", full, err)
	}

	now := time.Now()
	if err := os.Chtimes(full, now, now); err != nil {
		// Best-effort: a missed touch just means slightly stale LRU recency,
		// not a failed read.
		s.logger.Debug().Err(err).Str("path", full).Msg("cache: store: failed to touch mtime on read")
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, nil, fmt.Errorf("cache: store: stat opened file %s: %w", full, err)
	}

	return f, fi, nil
}

// Usage returns the total size in bytes of every regular file under
// dataDir, summed across all kind subdirectories. It tolerates an entry
// disappearing between the walk and the stat — a concurrent eviction can
// remove a file mid-walk — by skipping it rather than aborting.
func (s *Store) Usage() (int64, error) {
	var total int64

	err := filepath.WalkDir(s.dataDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return err
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cache: store: usage: %w", err)
	}

	return total, nil
}

// SweepResult summarizes what a single Sweep pass did.
type SweepResult struct {
	BytesBefore int64
	BytesAfter  int64
	Evicted     int
	TempReaped  int
}

const (
	// sweepLowWaterPercent is the fraction of maxBytes Sweep targets once
	// usage is found to exceed the cap (D-09: evict down to roughly 90
	// percent of the cap, not all the way to zero, so overshoot between
	// sweeps stays cheap to absorb on the next pass).
	sweepLowWaterPercent = 90

	// tempFileMaxAge is how long a ".tmp-*" scratch file may sit on disk
	// before Sweep treats it as orphaned by an interrupted Put and reaps it.
	tempFileMaxAge = 1 * time.Hour
)

// tempFilePrefix identifies Put's own scratch files (see os.CreateTemp in
// Put) so Sweep can reap them separately from real cache content.
const tempFilePrefix = ".tmp-"

// sweepFile is a regular-file candidate collected during Sweep's walk.
type sweepFile struct {
	path    string
	size    int64
	modTime time.Time
}

// Sweep walks dataDir once, measuring total usage across every kind
// subdirectory as a single pool (D-08 — no per-kind quotas), and evicts
// least-recently-used content down to sweepLowWaterPercent of maxBytes when
// usage exceeds it. It also reaps orphaned ".tmp-*" scratch files older than
// tempFileMaxAge and prunes any kind directory left empty by eviction —
// dataDir itself is never removed. A single failed deletion is logged at
// Warn and skipped rather than aborting the pass, so one unlink error can't
// leave the cache permanently over cap.
func (s *Store) Sweep(maxBytes int64) (SweepResult, error) {
	files, dirs, tempReaped, err := s.sweepCollect()
	if err != nil {
		return SweepResult{}, err
	}

	var bytesBefore int64
	for _, f := range files {
		bytesBefore += f.size
	}

	result := SweepResult{BytesBefore: bytesBefore, BytesAfter: bytesBefore, Evicted: 0, TempReaped: tempReaped}

	if bytesBefore > maxBytes {
		target := maxBytes * sweepLowWaterPercent / 100
		result.BytesAfter, result.Evicted = s.evictLRU(files, bytesBefore, target)
	}

	pruneEmptyDirs(dirs)

	return result, nil
}

// sweepCollect walks dataDir once, returning every regular cache-content
// file plus every directory found (dataDir itself excluded from dirs), and
// reaping any ".tmp-*" scratch file older than tempFileMaxAge along the way.
// Non-regular entries (symlinks, devices, ...) are skipped entirely: d.Type()
// reflects the raw directory-entry type from the walk itself, not a followed
// stat, so this check alone keeps a symlink escape out of consideration for
// accounting or deletion.
func (s *Store) sweepCollect() ([]sweepFile, []string, int, error) {
	var (
		files      []sweepFile
		dirs       []string
		tempReaped int
	)

	walkErr := filepath.WalkDir(s.dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return s.sweepEntryErr(p, err)
		}

		if d.IsDir() {
			if p != s.dataDir {
				dirs = append(dirs, p)
			}

			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return s.sweepEntryErr(p, infoErr)
		}

		if strings.HasPrefix(filepath.Base(p), tempFilePrefix) {
			if s.reapTempFileIfStale(p, info) {
				tempReaped++
			}

			return nil
		}

		files = append(files, sweepFile{path: p, size: info.Size(), modTime: info.ModTime()})

		return nil
	})
	if walkErr != nil {
		return nil, nil, 0, fmt.Errorf("cache: store: sweep: %w", walkErr)
	}

	return files, dirs, tempReaped, nil
}

// sweepEntryErr classifies an error found while visiting p during the walk:
// dataDir vanishing entirely is fatal (propagated), an individual entry
// vanishing between the walk and a stat is tolerated (a concurrent eviction
// or Put can remove/replace a file mid-walk), and anything else is
// propagated as-is.
func (s *Store) sweepEntryErr(p string, err error) error {
	if p == s.dataDir {
		return err
	}

	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

// reapTempFileIfStale deletes p if it is a ".tmp-*" scratch file older than
// tempFileMaxAge, returning true if it was reaped. A deletion failure is
// logged at Warn and treated as not-reaped, not returned — one unlink error
// must not abort the sweep.
func (s *Store) reapTempFileIfStale(p string, info fs.FileInfo) bool {
	if time.Since(info.ModTime()) <= tempFileMaxAge {
		return false
	}

	if err := os.Remove(p); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn().Err(err).Str("path", p).Msg("cache: store: sweep: failed to reap orphaned temp file")
		}

		return false
	}

	return true
}

// evictLRU sorts files by mtime ascending and deletes from the front until
// the running total is at or below target, returning the bytes remaining
// and the count evicted. A single failed deletion is logged at Warn and
// skipped, not returned — one unlink error must not abort the pass.
func (s *Store) evictLRU(files []sweepFile, bytesBefore, target int64) (int64, int) {
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })

	running := bytesBefore
	evicted := 0

	for _, f := range files {
		if running <= target {
			break
		}

		if err := os.Remove(f.path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.logger.Warn().Err(err).Str("path", f.path).Msg("cache: store: sweep: failed to evict")
			}

			continue
		}

		running -= f.size
		evicted++

		s.logger.Debug().Str("path", f.path).Int64("size", f.size).Msg("cache: store: sweep: evicted")
	}

	return running, evicted
}

// pruneEmptyDirs removes any directory in dirs that is now empty, deepest
// first (longest path) so a nested kind directory is attempted before its
// parent. os.Remove failing on a non-empty directory is the expected outcome
// for a directory that still holds content, not an error worth surfacing.
func pruneEmptyDirs(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
}
