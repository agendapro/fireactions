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
