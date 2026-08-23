package sandboxctl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// TestSnapshotExclude_RoundTrip builds a tree containing exactly the
// hazards this issue's planning found in internal/storage: a FIFO (which
// ReadArchive refuses to restore) and a lost+found directory (which, if
// unreadable, would abort the whole WriteArchive walk), plus a fake
// container-cache path and ordinary files. It runs a REAL WriteArchive ->
// ReadArchive round-trip through internal/storage's actual functions and
// proves SnapshotExclude is load-bearing: the archive restores cleanly WITH
// the exclude func, and fails WITHOUT it.
func TestSnapshotExclude_RoundTrip(t *testing.T) {
	root := t.TempDir()

	mustWriteFile(t, filepath.Join(root, "workspace.txt"), "hello")
	mustWriteFile(t, filepath.Join(root, "sub", "nested.txt"), "nested")

	// A FIFO under the tree: ReadArchive refuses anything that isn't
	// TypeReg/TypeDir/TypeSymlink, so an archive containing one can never be
	// restored unless it is excluded first.
	fifoPath := filepath.Join(root, "a.sock")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	// lost+found: root-owned 0700 on a real ext4 PVC. We can't easily
	// simulate "unreadable by this UID" in a unit test running as the
	// current user, but we CAN prove the exclusion actually removes it from
	// the restored tree (the walk-abort hazard is structural: WalkDir stops
	// on the first Lstat/ReadDir error under a directory it cannot read,
	// which lost+found being excluded entirely sidesteps).
	if err := os.MkdirAll(filepath.Join(root, "lost+found"), 0o750); err != nil {
		t.Fatalf("MkdirAll lost+found: %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "lost+found", "junk"), "junk")

	// A fake container image/layer cache path.
	mustWriteFile(t, filepath.Join(root, ".local", "share", "containers", "storage", "blob"), "big-cache-blob")

	ctx := context.Background()

	// WITH the exclude func: round-trip succeeds and only the expected
	// entries are restored.
	var buf bytes.Buffer
	if _, err := storage.WriteArchive(ctx, root, &buf, storage.ArchiveOptions{Exclude: SnapshotExclude}); err != nil {
		t.Fatalf("WriteArchive (excluded): %v", err)
	}
	restoreRoot := t.TempDir()
	if _, err := storage.ReadArchive(ctx, bytes.NewReader(buf.Bytes()), restoreRoot, storage.ExtractOptions{}); err != nil {
		t.Fatalf("ReadArchive (excluded): %v", err)
	}

	got := listFiles(t, restoreRoot)
	want := []string{"sub/nested.txt", "workspace.txt"}
	if !equalStrings(got, want) {
		t.Errorf("restored entries = %v, want %v", got, want)
	}

	// WITHOUT the exclude func: the FIFO makes the round-trip fail --
	// proving the guard is load-bearing, not vacuous.
	var buf2 bytes.Buffer
	if _, err := storage.WriteArchive(ctx, root, &buf2, storage.ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive (unexcluded): %v", err)
	}
	restoreRoot2 := t.TempDir()
	if _, err := storage.ReadArchive(ctx, bytes.NewReader(buf2.Bytes()), restoreRoot2, storage.ExtractOptions{}); err == nil {
		t.Fatal("ReadArchive (unexcluded) succeeded, want an error from the unrestorable FIFO entry")
	}
}

// TestSnapshotExclude_WarmMarkerAndRestoreSentinel verifies the two #29
// exclusions directly: a stale warm-cache.json must never be baked into a
// new archive, and the restore-in-progress sentinel must never be restored
// into a fresh workspace.
func TestSnapshotExclude_WarmMarkerAndRestoreSentinel(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{".sandbox/warm-cache.json", true},
		{".sandbox/restore-in-progress", true},
		{".sandbox/last-freeze.json", false},
		{".sandbox/RESUME.md", false},
		{"workspace.txt", false},
	}
	for _, tc := range cases {
		fi := fakeFileInfo{}
		if got := SnapshotExclude(tc.rel, fi); got != tc.want {
			t.Errorf("SnapshotExclude(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// TestSnapshotExclude_PodmanGraphRootUnderEveryPlausibleRoot proves the
// belt-and-braces filter still covers #24's graph root now that the engine
// actually exists. The PRIMARY guarantee is structural (the emptyDir is
// mounted only into the podman sidecar, at a path under neither
// WorkspacePath nor AgentHomePath) and is asserted at render level by
// TestPodmanEngine_GraphRootIsAnEmptyDirOutsideEverySnapshottedPath
// (internal/render/engine_podman_test.go). This is the second, independent
// guard.
func TestSnapshotExclude_PodmanGraphRootUnderEveryPlausibleRoot(t *testing.T) {
	cases := []string{
		".local/share/containers",
		".local/share/containers/storage/overlay/abc123/diff/usr/bin/podman",
		".config/containers",
		"var/lib/containers/storage/overlay-layers/layers.json",
	}
	for _, rel := range cases {
		fi := fakeFileInfo{}
		if !SnapshotExclude(rel, fi) {
			t.Errorf("SnapshotExclude(%q) = false, want true (podman graph root path)", rel)
		}
	}
}

// fakeFileInfo is a minimal fs.FileInfo satisfying SnapshotExclude's use of
// Mode()/IsDir() for a regular file, avoiding a real filesystem entry.
type fakeFileInfo struct{}

func (fakeFileInfo) Name() string           { return "" }
func (fakeFileInfo) Size() int64            { return 0 }
func (fakeFileInfo) Mode() os.FileMode      { return 0o644 }
func (fakeFileInfo) ModTime() (t time.Time) { return t }
func (fakeFileInfo) IsDir() bool            { return false }
func (fakeFileInfo) Sys() any               { return nil }

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
