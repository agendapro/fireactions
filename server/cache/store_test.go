package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	logger := zerolog.Nop()

	s, err := NewStore(&logger, t.TempDir())
	require.NoError(t, err)

	return s
}

func TestStore_Path_InvalidKind(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name string
		kind string
	}{
		{"empty", ""},
		{"contains_slash", "a/b"},
		{"dotdot", ".."},
		{"uppercase", "Foo"},
		{"too_long", strings.Repeat("a", 33)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := s.Path(tc.kind, "key")
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidKey)
			assert.Empty(t, full)
		})
	}
}

func TestStore_Path_InvalidKey(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"dotdot", ".."},
		{"traversal_leading", "../../etc/passwd"},
		{"traversal_mixed", "a/../../../etc/passwd"},
		{"absolute", "/etc/passwd"},
		{"nul_byte", "a\x00b"},
		{"hidden_segment", "a/.hidden/b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := s.Path("gomod", tc.key)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidKey)
			assert.Empty(t, full)
		})
	}
}

// TestStore_Path_TraversalNeverEscapesTempDir pins the containment
// guarantee at the Open layer too, not just Path in isolation: a
// traversal-shaped key is refused before any filesystem access is
// attempted, so no file outside t.TempDir() is ever opened.
func TestStore_Path_TraversalNeverEscapesTempDir(t *testing.T) {
	s := newTestStore(t)

	f, fi, err := s.Open("gomod", "../../etc/passwd")
	assert.Nil(t, f)
	assert.Nil(t, fi)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidKey)
}

func TestStore_Path_Valid(t *testing.T) {
	s := newTestStore(t)

	full, err := s.Path("gomod", "a/b/c")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(s.dataDir, "gomod", "a", "b", "c"), full)

	doubled, err := s.Path("gomod", "a//b")
	require.NoError(t, err)

	single, err := s.Path("gomod", "a/b")
	require.NoError(t, err)

	assert.Equal(t, single, doubled)
}

func TestStore_PutOpen_RoundTrip(t *testing.T) {
	s := newTestStore(t)

	content := []byte("hello world")

	err := s.Put("gomod", "example.com/mod/@v/list", strings.NewReader(string(content)))
	require.NoError(t, err)

	f, fi, err := s.Open("gomod", "example.com/mod/@v/list")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, int64(len(content)), fi.Size())
}

func TestStore_Put_NoLeftoverTempFile(t *testing.T) {
	s := newTestStore(t)

	err := s.Put("gomod", "a/b", strings.NewReader("data"))
	require.NoError(t, err)

	dir := filepath.Join(s.dataDir, "gomod", "a")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tmp-"), "leftover tmp file: %s", e.Name())
	}
}

func TestStore_Put_CreatesIntermediateDirs(t *testing.T) {
	s := newTestStore(t)

	err := s.Put("gomod", "a/b/c/d", strings.NewReader("x"))
	require.NoError(t, err)

	full, err := s.Path("gomod", "a/b/c/d")
	require.NoError(t, err)

	_, statErr := os.Stat(full)
	assert.NoError(t, statErr)
}

// TestStore_Put_FreshMtimeEvenWithOldSourceReader is Pitfall 8: content
// copied from an old source file must not look old to the eviction sweep.
func TestStore_Put_FreshMtimeEvenWithOldSourceReader(t *testing.T) {
	s := newTestStore(t)

	oldFile := filepath.Join(t.TempDir(), "old.txt")
	require.NoError(t, os.WriteFile(oldFile, []byte("old content"), 0o600))

	oldTime := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(oldFile, oldTime, oldTime))

	src, err := os.Open(oldFile)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	err = s.Put("gomod", "fresh", src)
	require.NoError(t, err)

	full, err := s.Path("gomod", "fresh")
	require.NoError(t, err)

	info, err := os.Stat(full)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), info.ModTime(), 5*time.Second)
}

func TestStore_Open_MissingKey(t *testing.T) {
	s := newTestStore(t)

	f, fi, err := s.Open("gomod", "does/not/exist")
	assert.Nil(t, f)
	assert.Nil(t, fi)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestStore_Open_UpdatesMtimeOnRead(t *testing.T) {
	s := newTestStore(t)

	err := s.Put("gomod", "touchme", strings.NewReader("data"))
	require.NoError(t, err)

	full, err := s.Path("gomod", "touchme")
	require.NoError(t, err)

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(full, old, old))

	before, err := os.Stat(full)
	require.NoError(t, err)

	f, _, err := s.Open("gomod", "touchme")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	after, err := os.Stat(full)
	require.NoError(t, err)

	assert.True(t, after.ModTime().After(before.ModTime()),
		"expected mtime to advance after Open, before=%v after=%v", before.ModTime(), after.ModTime())
}

// TestStore_Open_RefusesSymlinkEscape writes a real file outside dataDir
// and a symlink inside dataDir pointing at it: Open must refuse the
// symlink and must never return the target's content.
func TestStore_Open_RefusesSymlinkEscape(t *testing.T) {
	s := newTestStore(t)

	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretPath, []byte("top secret"), 0o600))

	linkDir := filepath.Join(s.dataDir, "gomod")
	require.NoError(t, os.MkdirAll(linkDir, 0o750))

	linkPath := filepath.Join(linkDir, "escape")
	require.NoError(t, os.Symlink(secretPath, linkPath))

	f, fi, err := s.Open("gomod", "escape")
	assert.Nil(t, f)
	assert.Nil(t, fi)
	require.Error(t, err)
}

func TestStore_Usage_SumsRegularFilesIgnoresDirs(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.Put("gomod", "a", strings.NewReader("12345")))
	require.NoError(t, s.Put("npm", "b/c", strings.NewReader("1234567890")))

	usage, err := s.Usage()
	require.NoError(t, err)
	assert.Equal(t, int64(15), usage)
}

// TestStore_Restart_ReadsContentFromPreviousStore is D-10: disk is the
// only source of truth, so a second Store constructed over the same
// directory must read content written by the first with no rebuild step.
func TestStore_Restart_ReadsContentFromPreviousStore(t *testing.T) {
	dataDir := t.TempDir()
	logger := zerolog.Nop()

	s1, err := NewStore(&logger, dataDir)
	require.NoError(t, err)

	err = s1.Put("gomod", "example.com/mod/@v/v1.0.0.info", strings.NewReader("v1 content"))
	require.NoError(t, err)

	s2, err := NewStore(&logger, dataDir)
	require.NoError(t, err)

	f, _, err := s2.Open("gomod", "example.com/mod/@v/v1.0.0.info")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "v1 content", string(got))
}

func TestStore_NotRegularFile_ErrorDoesNotClaimMissing(t *testing.T) {
	s := newTestStore(t)

	// A directory at the resolved path is neither missing nor a regular
	// file; Open must still refuse it without panicking.
	dirPath := filepath.Join(s.dataDir, "gomod", "adir")
	require.NoError(t, os.MkdirAll(dirPath, 0o750))

	f, fi, err := s.Open("gomod", "adir")
	assert.Nil(t, f)
	assert.Nil(t, fi)
	require.Error(t, err)
	assert.False(t, errors.Is(err, os.ErrNotExist))
}

// writeSweepTestFile writes size bytes of real content at kind/key with an
// explicit mtime, bypassing Put so tests can control mtime deterministically
// (real wall-clock ordering between fast successive writes can collide at
// filesystem mtime resolution).
func writeSweepTestFile(t *testing.T, s *Store, kind, key string, size int, mtime time.Time) string {
	t.Helper()

	full, err := s.Path(kind, key)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(strings.Repeat("a", size)), 0o600))
	require.NoError(t, os.Chtimes(full, mtime, mtime))

	return full
}

func TestStore_Sweep_UnderCap_NoEviction(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().Add(-time.Hour)
	paths := make([]string, 5)

	for i := range paths {
		paths[i] = writeSweepTestFile(t, s, "gomod", fmt.Sprintf("file-%d", i), 1000, base.Add(time.Duration(i)*time.Minute))
	}

	result, err := s.Sweep(100000)
	require.NoError(t, err)
	assert.Equal(t, int64(5000), result.BytesBefore)
	assert.Equal(t, int64(5000), result.BytesAfter)
	assert.Equal(t, 0, result.Evicted)

	for _, p := range paths {
		_, statErr := os.Stat(p)
		assert.NoError(t, statErr)
	}
}

func TestStore_Sweep_OverCap_EvictsOldestDownToLowWater(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().Add(-time.Hour)
	names := make([]string, 10)

	for i := range names {
		name := fmt.Sprintf("file-%02d", i)
		names[i] = name
		writeSweepTestFile(t, s, "gomod", name, 1000, base.Add(time.Duration(i)*time.Minute))
	}

	result, err := s.Sweep(5000)
	require.NoError(t, err)
	assert.Equal(t, int64(10000), result.BytesBefore)
	assert.LessOrEqual(t, result.BytesAfter, int64(4500))
	assert.Equal(t, 6, result.Evicted)
	assert.Equal(t, int64(4000), result.BytesAfter)

	// The 6 oldest files must be gone; the 4 newest must survive — named
	// explicitly so an off-by-one in the sort direction can't pass.
	for i := range 6 {
		full, pathErr := s.Path("gomod", names[i])
		require.NoError(t, pathErr)
		_, statErr := os.Stat(full)
		assert.True(t, os.IsNotExist(statErr), "expected %s evicted", names[i])
	}

	for i := 6; i < 10; i++ {
		full, pathErr := s.Path("gomod", names[i])
		require.NoError(t, pathErr)
		_, statErr := os.Stat(full)
		assert.NoError(t, statErr, "expected %s to survive", names[i])
	}
}

// TestStore_Sweep_EvictionOrderByMtimeNotWriteOrder pins eviction order to
// mtime, not filesystem-visit/write order: the file written first is given
// the newest mtime and must be the one that survives.
func TestStore_Sweep_EvictionOrderByMtimeNotWriteOrder(t *testing.T) {
	s := newTestStore(t)

	now := time.Now()

	writeSweepTestFile(t, s, "gomod", "first", 1000, now)
	writeSweepTestFile(t, s, "gomod", "second", 1000, now.Add(-2*time.Hour))
	writeSweepTestFile(t, s, "gomod", "third", 1000, now.Add(-time.Hour))

	result, err := s.Sweep(2000) // low-water target = 1800
	require.NoError(t, err)
	assert.Equal(t, 2, result.Evicted)

	fullFirst, err := s.Path("gomod", "first")
	require.NoError(t, err)
	_, statErr := os.Stat(fullFirst)
	assert.NoError(t, statErr, "expected the file with the newest mtime (written first) to survive")

	fullSecond, err := s.Path("gomod", "second")
	require.NoError(t, err)
	_, statErr = os.Stat(fullSecond)
	assert.True(t, os.IsNotExist(statErr))

	fullThird, err := s.Path("gomod", "third")
	require.NoError(t, err)
	_, statErr = os.Stat(fullThird)
	assert.True(t, os.IsNotExist(statErr))
}

// TestStore_Sweep_SpansKindSubdirectories is D-08: all kind subdirectories
// are one pool against a single global cap, not per-kind quotas.
func TestStore_Sweep_SpansKindSubdirectories(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().Add(-time.Hour)
	writeSweepTestFile(t, s, "gomod", "old", 1000, base)
	writeSweepTestFile(t, s, "docker", "new", 1000, base.Add(time.Hour))

	result, err := s.Sweep(1500) // low-water target = 1350
	require.NoError(t, err)
	assert.Equal(t, 1, result.Evicted)
	assert.LessOrEqual(t, result.BytesAfter, int64(1350))

	fullOld, err := s.Path("gomod", "old")
	require.NoError(t, err)
	_, statErr := os.Stat(fullOld)
	assert.True(t, os.IsNotExist(statErr), "expected the oldest file across kind subdirectories to be evicted")

	fullNew, err := s.Path("docker", "new")
	require.NoError(t, err)
	_, statErr = os.Stat(fullNew)
	assert.NoError(t, statErr, "expected the newer file in a different kind subdirectory to survive")
}

// TestStore_Sweep_ReapsOrphanedTempFiles is Pitfall/Pattern 3: a stale
// ".tmp-*" scratch file from an interrupted Put is reaped once older than
// tempFileMaxAge; a fresh one survives, and neither ever counts as usage.
func TestStore_Sweep_ReapsOrphanedTempFiles(t *testing.T) {
	s := newTestStore(t)

	kindDir := filepath.Join(s.dataDir, "gomod")
	require.NoError(t, os.MkdirAll(kindDir, 0o750))

	oldTmp := filepath.Join(kindDir, ".tmp-abc")
	require.NoError(t, os.WriteFile(oldTmp, []byte("orphaned"), 0o600))
	oldTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(oldTmp, oldTime, oldTime))

	freshTmp := filepath.Join(kindDir, ".tmp-xyz")
	require.NoError(t, os.WriteFile(freshTmp, []byte("in-progress"), 0o600))

	result, err := s.Sweep(100000)
	require.NoError(t, err)
	assert.Equal(t, 1, result.TempReaped)
	assert.Equal(t, int64(0), result.BytesBefore)

	_, statErr := os.Stat(oldTmp)
	assert.True(t, os.IsNotExist(statErr), "expected the 2-hour-old temp file to be reaped")

	_, statErr = os.Stat(freshTmp)
	assert.NoError(t, statErr, "expected the fresh temp file to survive")
}

// TestStore_Sweep_PrunesEmptyDirectories asserts an emptied nested kind
// directory is removed, but dataDir itself is never removed.
func TestStore_Sweep_PrunesEmptyDirectories(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().Add(-time.Hour)
	writeSweepTestFile(t, s, "gomod", "nested/only-file", 1000, base)

	nestedDir := filepath.Join(s.dataDir, "gomod", "nested")
	_, err := os.Stat(nestedDir)
	require.NoError(t, err)

	result, err := s.Sweep(0) // force full eviction: low-water target is 0
	require.NoError(t, err)
	assert.Equal(t, 1, result.Evicted)

	_, err = os.Stat(nestedDir)
	assert.True(t, os.IsNotExist(err), "expected the emptied nested kind directory to be pruned")

	_, err = os.Stat(s.dataDir)
	assert.NoError(t, err, "data_dir itself must never be removed")
}

func TestStore_Sweep_EmptyStore(t *testing.T) {
	s := newTestStore(t)

	result, err := s.Sweep(1000)
	require.NoError(t, err)
	assert.Equal(t, SweepResult{BytesBefore: 0, BytesAfter: 0, Evicted: 0, TempReaped: 0}, result)
}

func TestStore_Sweep_DataDirDeleted_ReturnsErrorNotPanic(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, os.RemoveAll(s.dataDir))

	assert.NotPanics(t, func() {
		_, err := s.Sweep(1000)
		assert.Error(t, err)
	})
}

// TestStore_Sweep_NeverFollowsSymlinkOutsideDataDir places a symlink inside
// dataDir pointing at a file outside it: Sweep must never account for or
// delete the symlink target, even under an aggressively low cap.
func TestStore_Sweep_NeverFollowsSymlinkOutsideDataDir(t *testing.T) {
	s := newTestStore(t)

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("do not touch"), 0o600))

	linkDir := filepath.Join(s.dataDir, "gomod")
	require.NoError(t, os.MkdirAll(linkDir, 0o750))
	linkPath := filepath.Join(linkDir, "escape")
	require.NoError(t, os.Symlink(outsideFile, linkPath))

	result, err := s.Sweep(0) // force aggressive eviction
	require.NoError(t, err)
	assert.Equal(t, 0, result.Evicted) // the symlink itself is never accounted or evicted

	_, statErr := os.Stat(outsideFile)
	assert.NoError(t, statErr, "expected the symlink target outside data_dir to survive")
}
