package storage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	b, err := os.ReadFile(full) //nolint:gosec // G304: full is filepath.Join(root, rel) inside a test's own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", rel, err)
	}
	return string(b)
}

func TestArchiveRoundTrip_AwkwardNamesAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on windows")
	}
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"with spaces.txt":                 "a",
		"unicode-éà中文.txt":                "b",
		strings.Repeat("x", 200) + ".txt": "long name",
		"weird#chars?&=+.txt":             "c",
		".hidden":                         "d",
		"empty-dir/.keep":                 "keep-file-so-dir-is-created",
	})
	if err := os.MkdirAll(filepath.Join(src, "truly-empty-dir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink("with spaces.txt", filepath.Join(src, "a-symlink")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	var buf bytes.Buffer
	if _, err := WriteArchive(context.Background(), src, &buf, ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	dst := t.TempDir()
	if _, err := ReadArchive(context.Background(), bytes.NewReader(buf.Bytes()), dst, ExtractOptions{}); err != nil {
		t.Fatalf("ReadArchive: %v", err)
	}

	assertTree(t, dst, map[string]string{
		"with spaces.txt":                 "a",
		"unicode-éà中文.txt":                "b",
		strings.Repeat("x", 200) + ".txt": "long name",
		"weird#chars?&=+.txt":             "c",
		".hidden":                         "d",
		"empty-dir/.keep":                 "keep-file-so-dir-is-created",
	})

	if fi, err := os.Lstat(filepath.Join(dst, "a-symlink")); err != nil {
		t.Errorf("symlink missing after restore: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("a-symlink was not restored as a symlink")
	}
	target, err := os.Readlink(filepath.Join(dst, "a-symlink"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "with spaces.txt" {
		t.Errorf("symlink target = %q, want %q", target, "with spaces.txt")
	}

	if fi, err := os.Stat(filepath.Join(dst, "truly-empty-dir")); err != nil || !fi.IsDir() {
		t.Errorf("empty directory not preserved: err=%v", err)
	}
}

func TestReadArchive_ZipSlipRejected(t *testing.T) {
	cases := []struct {
		name string
		hdr  func() *tarHeaderSpec
	}{
		{"parent escape", func() *tarHeaderSpec { return &tarHeaderSpec{name: "../escape.txt", content: "x"} }},
		{"nested parent escape", func() *tarHeaderSpec { return &tarHeaderSpec{name: "a/../../escape.txt", content: "x"} }},
		{"absolute path", func() *tarHeaderSpec { return &tarHeaderSpec{name: "/abs/escape.txt", content: "x"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.hdr()
			archive := buildTarZst(t, []tarHeaderSpec{*spec})
			dst := t.TempDir()
			_, err := ReadArchive(context.Background(), bytes.NewReader(archive), dst, ExtractOptions{})
			if !IsInvalid(err) {
				t.Errorf("ReadArchive with %s: err = %v, want ErrInvalid", tc.name, err)
			}
		})
	}
}

func TestReadArchive_SymlinkEscapeRejected(t *testing.T) {
	archive := buildTarZst(t, []tarHeaderSpec{
		{name: "evil-link", linkname: "../../../etc/passwd", symlink: true},
	})
	dst := t.TempDir()
	_, err := ReadArchive(context.Background(), bytes.NewReader(archive), dst, ExtractOptions{})
	if !IsInvalid(err) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestReadArchive_RefusesUnsupportedEntryTypes(t *testing.T) {
	archive := buildTarZstRaw(t, func(tw *tarWriterHelper) {
		tw.writeHardlink("hardlink", "somewhere")
	})
	dst := t.TempDir()
	_, err := ReadArchive(context.Background(), bytes.NewReader(archive), dst, ExtractOptions{})
	if !IsInvalid(err) {
		t.Errorf("hard link: err = %v, want ErrInvalid", err)
	}
}

func TestReadArchive_TruncatedZstdIsCorrupt(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f.txt": strings.Repeat("data", 1000)})
	var buf bytes.Buffer
	if _, err := WriteArchive(context.Background(), src, &buf, ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	truncated := buf.Bytes()[:buf.Len()/2]
	dst := t.TempDir()
	_, err := ReadArchive(context.Background(), bytes.NewReader(truncated), dst, ExtractOptions{})
	if err == nil {
		t.Fatal("ReadArchive of a truncated stream: want error, got nil")
	}
	if !IsCorrupt(err) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

func TestReadArchive_FlippedByteIsCorrupt(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f.txt": strings.Repeat("data", 1000)})
	var buf bytes.Buffer
	if _, err := WriteArchive(context.Background(), src, &buf, ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	corrupted := append([]byte(nil), buf.Bytes()...)
	// Flip a byte well past the zstd frame header, inside the compressed
	// block, so the frame is still recognized but its checksum/decode
	// fails.
	idx := len(corrupted) / 2
	corrupted[idx] ^= 0xFF

	dst := t.TempDir()
	_, err := ReadArchive(context.Background(), bytes.NewReader(corrupted), dst, ExtractOptions{})
	if err == nil {
		t.Fatal("ReadArchive of a corrupted stream: want error, got nil")
	}
	if !IsCorrupt(err) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

func TestReadArchive_MaxEntriesEnforced(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a": "1", "b": "2", "c": "3"})
	var buf bytes.Buffer
	if _, err := WriteArchive(context.Background(), src, &buf, ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	dst := t.TempDir()
	_, err := ReadArchive(context.Background(), bytes.NewReader(buf.Bytes()), dst, ExtractOptions{MaxEntries: 1})
	if !IsInvalid(err) {
		t.Errorf("err = %v, want ErrInvalid (MaxEntries exceeded)", err)
	}
}

func TestReadArchive_MaxTotalBytesEnforced(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"big.txt": strings.Repeat("x", 10000)})
	var buf bytes.Buffer
	if _, err := WriteArchive(context.Background(), src, &buf, ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	dst := t.TempDir()
	_, err := ReadArchive(context.Background(), bytes.NewReader(buf.Bytes()), dst, ExtractOptions{MaxTotalBytes: 100})
	if !IsInvalid(err) {
		t.Errorf("err = %v, want ErrInvalid (MaxTotalBytes exceeded)", err)
	}
}

func TestWriteArchive_ContextCancellationAbortsPromptly(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 500; i++ {
		writeFile(t, src, filepathIndex(i), "x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := WriteArchive(ctx, src, io.Discard, ArchiveOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestReadArchive_ContextCancellationAbortsPromptly(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 500; i++ {
		writeFile(t, src, filepathIndex(i), "x")
	}
	var buf bytes.Buffer
	if _, err := WriteArchive(context.Background(), src, &buf, ArchiveOptions{}); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := t.TempDir()
	_, err := ReadArchive(ctx, bytes.NewReader(buf.Bytes()), dst, ExtractOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func filepathIndex(i int) string {
	return fmt.Sprintf("many/%c-%d.txt", 'a'+rune(i%26), i)
}

func TestVerifyingReader(t *testing.T) {
	vr := newVerifyingReader(strings.NewReader("hello"))
	data, err := io.ReadAll(vr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}
	if vr.Size() != 5 {
		t.Errorf("Size() = %d, want 5", vr.Size())
	}
	if vr.SHA256() != sha256Hex("hello") {
		t.Errorf("SHA256() = %s, want %s", vr.SHA256(), sha256Hex("hello"))
	}
	if err := vr.Verify("Get", "k", FileEntry{Size: 5, SHA256: sha256Hex("hello")}); err != nil {
		t.Errorf("Verify with matching entry: %v", err)
	}
	if err := vr.Verify("Get", "k", FileEntry{Size: 999, SHA256: sha256Hex("hello")}); !IsCorrupt(err) {
		t.Errorf("Verify with wrong size: err = %v, want ErrCorrupt", err)
	}
}

// --- small tar-building test helpers (used only by the zip-slip tests) ---

type tarHeaderSpec struct {
	name     string
	content  string
	linkname string
	symlink  bool
}

func buildTarZst(t *testing.T, specs []tarHeaderSpec) []byte {
	t.Helper()
	return buildTarZstRaw(t, func(tw *tarWriterHelper) {
		for _, s := range specs {
			if s.symlink {
				tw.writeSymlink(s.name, s.linkname)
			} else {
				tw.writeFile(s.name, s.content)
			}
		}
	})
}

func buildTarZstRaw(t *testing.T, fn func(tw *tarWriterHelper)) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	tw := newTarWriterHelper(t, enc)
	fn(tw)
	tw.close()
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd Close: %v", err)
	}
	return buf.Bytes()
}

// tarWriterHelper is a minimal hand-rolled tar builder used only by the
// zip-slip / unsupported-entry-type tests above, which need to construct
// deliberately malicious archives that WriteArchive itself would never
// produce (WriteArchive only ever walks a real, local, non-malicious
// directory tree).
type tarWriterHelper struct {
	t  *testing.T
	tw *tar.Writer
}

func newTarWriterHelper(t *testing.T, w io.Writer) *tarWriterHelper {
	return &tarWriterHelper{t: t, tw: tar.NewWriter(w)}
}

func (h *tarWriterHelper) writeFile(name, content string) {
	h.t.Helper()
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
	if err := h.tw.WriteHeader(hdr); err != nil {
		h.t.Fatalf("WriteHeader(%s): %v", name, err)
	}
	if _, err := h.tw.Write([]byte(content)); err != nil {
		h.t.Fatalf("Write(%s): %v", name, err)
	}
}

func (h *tarWriterHelper) writeSymlink(name, linkname string) {
	h.t.Helper()
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: linkname}
	if err := h.tw.WriteHeader(hdr); err != nil {
		h.t.Fatalf("WriteHeader(%s): %v", name, err)
	}
}

func (h *tarWriterHelper) writeHardlink(name, linkname string) {
	h.t.Helper()
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeLink, Mode: 0o644, Linkname: linkname}
	if err := h.tw.WriteHeader(hdr); err != nil {
		h.t.Fatalf("WriteHeader(%s): %v", name, err)
	}
}

func (h *tarWriterHelper) close() {
	h.t.Helper()
	if err := h.tw.Close(); err != nil {
		h.t.Fatalf("tar Close: %v", err)
	}
}
