package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshFSBackend returns a fresh, empty FSBackend rooted at a fresh
// t.TempDir().
func freshFSBackend(t *testing.T) Backend {
	t.Helper()
	b, err := NewFS(FSConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return b
}

// brokenFSBackend returns an FSBackend whose root is a regular file, not a
// directory -- every real filesystem operation through it fails with
// ENOTDIR. Constructed directly (bypassing NewFS, whose FSConfig.Validate
// requires Root to already be a directory) since white-box access to the
// unexported struct is exactly what's needed to build a "structurally
// unusable" instance the same way brokenS3Backend does with a dead
// endpoint: both must construct successfully and fail per-operation, not
// fail at construction.
func brokenFSBackend(t *testing.T) Backend {
	t.Helper()
	dir := t.TempDir()
	rootFile := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(rootFile, []byte("i am a file, not a directory"), 0o600); err != nil {
		t.Fatalf("seeding broken root file: %v", err)
	}
	return &FSBackend{root: rootFile, dirPerm: 0o755, filePerm: 0o644}
}

func TestNewFS_RequiresExistingDirectoryRoot(t *testing.T) {
	if _, err := NewFS(FSConfig{Root: ""}); !IsInvalid(err) {
		t.Errorf("empty root: err = %v, want ErrInvalid", err)
	}
	if _, err := NewFS(FSConfig{Root: "relative/path"}); !IsInvalid(err) {
		t.Errorf("relative root: err = %v, want ErrInvalid", err)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := NewFS(FSConfig{Root: file}); !IsInvalid(err) {
		t.Errorf("root is a file: err = %v, want ErrInvalid", err)
	}

	missing := filepath.Join(dir, "does-not-exist")
	if _, err := NewFS(FSConfig{Root: missing}); !IsInvalid(err) {
		t.Errorf("missing root: err = %v, want ErrInvalid", err)
	}
}

func TestFSBackend_KeyToPathRejectsEscapes(t *testing.T) {
	b, err := NewFS(FSConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()

	for _, key := range []string{"", "/abs/key", "../escape", "a/../../escape", "a/../b"} {
		if _, statErr := b.Stat(ctx, key); !IsInvalid(statErr) {
			t.Errorf("Stat(%q): err = %v, want ErrInvalid", key, statErr)
		}
	}
}

func TestFSBackend_PutIsAtomicNoPartialFileOnFailure(t *testing.T) {
	root := t.TempDir()
	b, err := NewFS(FSConfig{Root: root})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()

	failingReader := &erroringReader{errAfter: 4}
	if _, err := b.Put(ctx, "some/key", failingReader, PutOptions{}); err == nil {
		t.Fatal("Put with a failing reader: want error, got nil")
	}

	// No temp file should remain, and the final key must not exist.
	entries, err := os.ReadDir(filepath.Join(root, "some"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	if _, err := b.Stat(ctx, "some/key"); !IsNotFound(err) {
		t.Errorf("Stat after failed Put: err = %v, want ErrNotFound", err)
	}
}

type erroringReader struct {
	n        int
	errAfter int
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.n >= r.errAfter {
		return 0, os.ErrClosed
	}
	for i := range p {
		if r.n >= r.errAfter {
			return i, nil
		}
		p[i] = 'x'
		r.n++
	}
	return len(p), nil
}

func TestFSBackend_DeletePrunesEmptyParentDirs(t *testing.T) {
	root := t.TempDir()
	b, err := NewFS(FSConfig{Root: root})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()

	if _, err := b.Put(ctx, "a/b/c/file.txt", strings.NewReader("data"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, "a/b/c/file.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Errorf("expected empty parent dirs to be pruned up to root, but %q still exists (err=%v)", filepath.Join(root, "a"), err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root itself must survive pruning: %v", err)
	}
}
