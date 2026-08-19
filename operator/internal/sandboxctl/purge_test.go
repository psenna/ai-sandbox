package sandboxctl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeRoot_RemovesEverythingExceptKeep(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "a")
	mustWriteFile(t, filepath.Join(root, "keepme.json"), "k")
	if err := os.MkdirAll(filepath.Join(root, "sub", "nested"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "sub", "nested", "b.txt"), "b")
	if err := os.MkdirAll(filepath.Join(root, "lost+found"), 0o700); err != nil {
		t.Fatalf("mkdir lost+found: %v", err)
	}

	removed, err := purgeRoot(root, "keepme.json")
	if err != nil {
		t.Fatalf("purgeRoot: %v", err)
	}
	if removed != 2 { // a.txt, sub
		t.Errorf("removed = %d, want 2", removed)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["keepme.json"] {
		t.Error("keepme.json was removed, want kept")
	}
	if !names["lost+found"] {
		t.Error("lost+found was removed, want implicitly kept")
	}
	if names["a.txt"] || names["sub"] {
		t.Errorf("purge left entries behind: %v", names)
	}
}

func TestPurgeRoot_MissingRootIsNoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	removed, err := purgeRoot(root)
	if err != nil {
		t.Fatalf("purgeRoot on missing root: unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestPurgeRoot_EmptyRootIsNoop(t *testing.T) {
	root := t.TempDir()
	removed, err := purgeRoot(root)
	if err != nil {
		t.Fatalf("purgeRoot on empty root: unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}
