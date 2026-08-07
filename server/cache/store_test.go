package cache

import (
	"errors"
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
