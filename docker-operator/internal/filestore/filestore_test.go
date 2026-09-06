package filestore

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNew(t *testing.T) {
	t.Run("creates a missing root under an existing parent", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "store")
		s, err := New(root)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = s.Close() }()
		if fi, statErr := os.Stat(root); statErr != nil || !fi.IsDir() {
			t.Fatalf("root not created as a directory: %v", statErr)
		}
		if s.Root() != root {
			t.Errorf("Root() = %q, want %q", s.Root(), root)
		}
	})

	t.Run("creates the shared/ common area", func(t *testing.T) {
		s := newStore(t)
		fi, err := os.Stat(filepath.Join(s.Root(), SharedDir))
		if err != nil || !fi.IsDir() {
			t.Fatalf("shared/ not created: %v", err)
		}
		// Idempotent, and does not clobber contents.
		if err := os.WriteFile(filepath.Join(s.Root(), SharedDir, "x"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureSharedDir(); err != nil {
			t.Fatalf("EnsureSharedDir (idempotent): %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(s.Root(), SharedDir, "x")); string(b) != "keep" {
			t.Errorf("EnsureSharedDir clobbered shared/x")
		}
	})

	t.Run("rejects a regular-file root", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(p); err == nil {
			t.Fatal("New on a regular file: want an error")
		}
	})

	t.Run("rejects a non-writable root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: the write probe cannot be made to fail with a mode change")
		}
		root := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(root, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := New(root); err == nil {
			t.Fatal("New on a non-writable dir: want an error")
		}
	})
}

func TestPathHardening(t *testing.T) {
	s := newStore(t)
	bad := []string{
		"../x",
		"a/../../b",
		"/abs",
		"a//b",
		"a/./b",
		"a\\b",
		"a\x00b",
		"a\x1fb",
		strings.Repeat("x", 256),
		strings.Repeat("a/", 700) + "z",
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			if _, err := s.List(p); !IsInvalidPath(err) {
				t.Errorf("List(%q) err = %v, want ErrInvalidPath", p, err)
			}
			if _, _, err := s.Open(p); !IsInvalidPath(err) {
				t.Errorf("Open(%q) err = %v, want ErrInvalidPath", p, err)
			}
			if _, err := s.Save(p, strings.NewReader("x"), 1<<20); !IsInvalidPath(err) {
				t.Errorf("Save(%q) err = %v, want ErrInvalidPath", p, err)
			}
			if err := s.Mkdir(p); !IsInvalidPath(err) {
				t.Errorf("Mkdir(%q) err = %v, want ErrInvalidPath", p, err)
			}
			if err := s.Remove(p); !IsInvalidPath(err) {
				t.Errorf("Remove(%q) err = %v, want ErrInvalidPath", p, err)
			}
		})
	}
}

func TestSymlinkEscape(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("agents/a"); err != nil {
		t.Fatal(err)
	}
	root := s.Root()

	// A directory OUTSIDE the store, standing in for /etc: every operation
	// below must fail to reach it, and if os.Root's guarantee ever regressed
	// the blast radius is this t.TempDir(), not the real filesystem.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The three shapes an agent (uid 1000, with write access to its own
	// subtree) can plant: an absolute symlink to a file, an absolute symlink
	// to a directory, and a relative one that climbs out with "..".
	for name, target := range map[string]string{
		"leak": "/etc/passwd",
		"file": secret,
		"dir":  outside,
		"up":   "../../..",
	} {
		if err := os.Symlink(target, filepath.Join(root, "agents", "a", name)); err != nil {
			t.Fatal(err)
		}
	}

	// Open must refuse both the absolute file symlink and a traversal
	// through a relative one.
	for _, p := range []string{"agents/a/leak", "agents/a/file", "agents/a/dir/secret.txt", "agents/a/up/x"} {
		f, _, err := s.Open(p)
		if err == nil {
			b, _ := io.ReadAll(f)
			_ = f.Close()
			t.Errorf("Open(%q) succeeded and read %q; the symlink escaped the root", p, b)
			continue
		}
		// A refused escape is a 400 (bad path), never a 500.
		if !IsInvalidPath(err) && !IsNotFound(err) {
			t.Errorf("Open(%q) err = %v, want ErrInvalidPath or ErrNotFound", p, err)
		}
	}

	// Every other entrypoint must be rooted too, not just Open: a store that
	// hardened only the download path would still let the browser list,
	// write into and delete an escaped directory.
	if entries, err := s.List("agents/a/dir"); err == nil {
		t.Errorf("List(agents/a/dir) succeeded with %+v; the symlink escaped the root", entries)
	}
	if _, err := s.Save("agents/a/dir/pwned", strings.NewReader("x"), 1<<20); err == nil {
		t.Error("Save through an escaping symlink succeeded")
	}
	// Save onto the symlink itself must not write THROUGH it: it is temp-file
	// + rename, so the link is replaced by a regular file in the store and
	// the outside target keeps its content (asserted below).
	if _, err := s.Save("agents/a/file", strings.NewReader("x"), 1<<20); err != nil {
		t.Errorf("Save over a symlink = %v, want it to replace the link", err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "agents", "a", "file")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("agents/a/file after Save = %v / %v, want a regular file (the link replaced)", fi.Mode(), err)
	}
	if err := s.Mkdir("agents/a/dir/pwned"); err == nil {
		t.Error("Mkdir through an escaping symlink succeeded")
	}
	if err := s.Remove("agents/a/dir/secret.txt"); err == nil {
		t.Error("Remove through an escaping symlink succeeded")
	}

	// Nothing outside the root may have been created, rewritten or deleted.
	b, err := os.ReadFile(secret) //nolint:gosec // G304: a path this test itself created under t.TempDir()
	if err != nil || string(b) != "top-secret" {
		t.Errorf("the file outside the root = %q / %v, want it untouched", b, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Errorf("a write escaped the root into %q: %v", outside, err)
	}
}

func TestList(t *testing.T) {
	s := newStore(t)

	// A fresh store's root holds exactly the shared/ common area New made.
	got, err := s.List("")
	if err != nil {
		t.Fatalf("List(root): %v", err)
	}
	if got == nil {
		t.Fatal("List(root) = nil, want a non-nil slice")
	}
	if len(got) != 1 || got[0].Name != SharedDir || !got[0].IsDir {
		t.Fatalf("List(root) = %v, want just the shared/ dir", got)
	}

	if err := s.Mkdir("agents/agt_1/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("agents/agt_1/a.txt", strings.NewReader("hello"), 1<<20); err != nil {
		t.Fatal(err)
	}

	entries, err := s.List("agents/agt_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List = %+v, want 2 entries", entries)
	}
	if !entries[0].IsDir || entries[0].Name != "sub" {
		t.Errorf("entries[0] = %+v, want the dir first", entries[0])
	}
	if entries[1].Name != "a.txt" || entries[1].Path != "agents/agt_1/a.txt" {
		t.Errorf("entries[1] = %+v, want a.txt with a slash-joined path", entries[1])
	}
	if entries[1].Size != 5 || entries[1].ModTime.IsZero() {
		t.Errorf("entries[1] size/modtime = %d/%v, want populated", entries[1].Size, entries[1].ModTime)
	}
}

func TestSave(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("agents/agt_1"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Save("agents/agt_1/f", strings.NewReader("first"), 1<<20); err != nil {
		t.Fatalf("Save: %v", err)
	}
	f, _, err := s.Open("agents/agt_1/f")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "first" {
		t.Errorf("content = %q, want first", b)
	}

	if _, err := s.Save("agents/agt_1/f", strings.NewReader("second-longer"), 1<<20); err != nil {
		t.Fatalf("overwrite Save: %v", err)
	}
	f, _, _ = s.Open("agents/agt_1/f")
	b, _ = io.ReadAll(f)
	_ = f.Close()
	if string(b) != "second-longer" {
		t.Errorf("content after overwrite = %q", b)
	}

	if _, err := s.Save("agents/agt_1/big", bytes.NewReader(make([]byte, 100)), 10); !IsTooLarge(err) {
		t.Errorf("Save over cap err = %v, want ErrTooLarge", err)
	}
	entries, _ := s.List("agents/agt_1")
	for _, e := range entries {
		if e.Name == "big" || strings.HasPrefix(e.Name, ".upload-") {
			t.Errorf("over-cap Save left %q behind", e.Name)
		}
	}

	if _, err := s.Save("agents/missing/f", strings.NewReader("x"), 1<<20); !IsNotFound(err) {
		t.Errorf("Save into a missing dir err = %v, want ErrNotFound", err)
	}
}

func TestOpen(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("agents/agt_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("agents/agt_1/f", strings.NewReader("data"), 1<<20); err != nil {
		t.Fatal(err)
	}

	f, entry, err := s.Open("agents/agt_1/f")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "data" || entry.Name != "f" || entry.IsDir {
		t.Errorf("Open round-trip = %q / %+v", b, entry)
	}

	if _, _, err := s.Open("agents/agt_1"); err == nil || !isIsDir(err) {
		t.Errorf("Open(dir) err = %v, want ErrIsDir", err)
	}
	if _, _, err := s.Open("agents/agt_1/missing"); !IsNotFound(err) {
		t.Errorf("Open(missing) err = %v, want ErrNotFound", err)
	}
}

func isIsDir(err error) bool { return err != nil && strings.Contains(err.Error(), "is a directory") }

// TestListOnRegularFile: pointing List at a file (rather than a directory)
// is a client mistake -> ErrNotDir -> 400, not an unwrapped ENOTDIR -> 500.
func TestListOnRegularFile(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("agents/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("agents/a/f", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List("agents/a/f"); !IsNotDir(err) {
		t.Errorf("List(regular file) err = %v, want ErrNotDir", err)
	}
}

// TestOpenNonRegularFile: an agent can mkfifo inside its own store dir. Open
// must return immediately (O_NONBLOCK) with ErrNotFound rather than blocking
// the request goroutine forever waiting for a writer.
func TestOpenNonRegularFile(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("agents/a"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(s.Root(), "agents", "a", "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, _, err := s.Open("agents/a/pipe")
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !IsNotFound(err) {
			t.Errorf("Open(fifo) err = %v, want ErrNotFound", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open(fifo) blocked; O_NONBLOCK / mode check missing")
	}
}

func TestMkdir(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("a/b/c"); err != nil {
		t.Fatalf("Mkdir nested: %v", err)
	}
	if err := s.Mkdir("a/b/c"); err != nil {
		t.Fatalf("Mkdir idempotent: %v", err)
	}
	if _, err := s.Save("a/b/file", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir("a/b/file/nope"); err == nil {
		t.Error("Mkdir through a file: want an error")
	}
}

func TestRemove(t *testing.T) {
	s := newStore(t)
	if err := s.Mkdir("agents/agt_1/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("agents/agt_1/f", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("agents/agt_1/f"); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if err := s.Remove("agents/agt_1"); err != nil {
		t.Fatalf("Remove non-empty tree: %v", err)
	}
	if err := s.Remove("agents/agt_1"); err != nil {
		t.Fatalf("Remove missing: %v, want nil (idempotent)", err)
	}
	if err := s.Remove(""); !IsInvalidPath(err) {
		t.Errorf("Remove(\"\") err = %v, want ErrInvalidPath", err)
	}
	if err := s.Remove("agents"); !IsInvalidPath(err) {
		t.Errorf("Remove(\"agents\") err = %v, want ErrInvalidPath", err)
	}
	if err := s.Remove("shared"); !IsInvalidPath(err) {
		t.Errorf("Remove(\"shared\") err = %v, want ErrInvalidPath", err)
	}
	// Contents of shared/ are still removable.
	if _, err := s.Save("shared/note.txt", strings.NewReader("hi"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("shared/note.txt"); err != nil {
		t.Errorf("Remove(shared/note.txt) = %v, want nil", err)
	}
}

func TestEnsureAgentDir(t *testing.T) {
	s := newStore(t)
	if err := s.EnsureAgentDir("agt_1"); err != nil {
		t.Fatalf("EnsureAgentDir: %v", err)
	}
	fi, err := os.Stat(filepath.Join(s.Root(), "agents", "agt_1"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o777 {
		t.Errorf("perm = %o, want 0777 (umask regression)", perm)
	}

	// Idempotent, does not clobber existing contents.
	if _, err := s.Save("agents/agt_1/keep", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureAgentDir("agt_1"); err != nil {
		t.Fatalf("EnsureAgentDir (again): %v", err)
	}
	if _, _, err := s.Open("agents/agt_1/keep"); err != nil {
		t.Errorf("EnsureAgentDir clobbered existing contents: %v", err)
	}

	for _, bad := range []string{"..", "a/b", ""} {
		if err := s.EnsureAgentDir(bad); !IsInvalidPath(err) {
			t.Errorf("EnsureAgentDir(%q) err = %v, want ErrInvalidPath", bad, err)
		}
	}
}

func TestRemoveAgentDir(t *testing.T) {
	s := newStore(t)
	if err := s.EnsureAgentDir("agt_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("agents/agt_1/f", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAgentDir("agt_1"); err != nil {
		t.Fatalf("RemoveAgentDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "agents", "agt_1")); !os.IsNotExist(err) {
		t.Errorf("agents/agt_1 still present: %v", err)
	}
	if err := s.RemoveAgentDir("agt_1"); err != nil {
		t.Errorf("RemoveAgentDir missing = %v, want nil", err)
	}
}

func TestAgentSubpath(t *testing.T) {
	if got := AgentSubpath("agt_1"); got != "agents/agt_1" {
		t.Errorf("AgentSubpath = %q, want agents/agt_1", got)
	}
}
