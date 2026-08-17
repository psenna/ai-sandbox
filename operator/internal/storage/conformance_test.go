package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// backendFactory names a Backend implementation and how to build a fresh,
// empty, isolated instance of it, or one that is structurally unusable
// (dead endpoint / broken root).
type backendFactory struct {
	name   string
	fresh  func(t *testing.T) Backend
	broken func(t *testing.T) Backend
}

func backendFactories() []backendFactory {
	return []backendFactory{
		{name: "fs", fresh: freshFSBackend, broken: brokenFSBackend},
		{name: "s3", fresh: freshS3Backend, broken: brokenS3Backend},
	}
}

type conformanceCase struct {
	name string
	long bool
	run  func(t *testing.T, f backendFactory)
}

// TestBackendConformance is the literal acceptance criterion for this
// issue: both the "fs" and "s3" backends must satisfy an identical
// behavioral contract.
func TestBackendConformance(t *testing.T) {
	for _, f := range backendFactories() {
		t.Run(f.name, func(t *testing.T) {
			for _, c := range conformanceCases {
				t.Run(c.name, func(t *testing.T) {
					if c.long && testing.Short() {
						t.Skip("long case; -short")
					}
					c.run(t, f)
				})
			}
		})
	}
}

func mustPut(t *testing.T, ctx context.Context, b Backend, key, data string) ObjectInfo {
	t.Helper()
	info, err := b.Put(ctx, key, strings.NewReader(data), PutOptions{})
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	return info
}

func mustGetString(t *testing.T, ctx context.Context, b Backend, key string) (string, ObjectInfo) {
	t.Helper()
	rc, info, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading Get(%q) body: %v", key, err)
	}
	return string(data), info
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var conformanceCases = []conformanceCase{
	{name: "PutGetRoundTrip", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "a/b/c.txt", "hello world")
		got, _ := mustGetString(t, ctx, b, "a/b/c.txt")
		if got != "hello world" {
			t.Errorf("got %q, want %q", got, "hello world")
		}
	}},

	{name: "PutReturnsSizeAndSHA256", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		const data = "some bytes to hash"
		info := mustPut(t, ctx, b, "k", data)
		if info.Size != int64(len(data)) {
			t.Errorf("Size = %d, want %d", info.Size, len(data))
		}
		if info.SHA256 != sha256Hex(data) {
			t.Errorf("SHA256 = %s, want %s", info.SHA256, sha256Hex(data))
		}
	}},

	{name: "PutOverwritesExistingObject", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "k", "version-1-longer-content")
		mustPut(t, ctx, b, "k", "v2")
		got, info := mustGetString(t, ctx, b, "k")
		if got != "v2" {
			t.Errorf("got %q, want %q (overwrite must fully replace, not append)", got, "v2")
		}
		if info.Size != 2 {
			t.Errorf("Size = %d, want 2", info.Size)
		}
	}},

	{name: "PutEmptyObject", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		info := mustPut(t, ctx, b, "empty", "")
		if info.Size != 0 {
			t.Errorf("Size = %d, want 0", info.Size)
		}
		got, _ := mustGetString(t, ctx, b, "empty")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	}},

	{name: "GetMissingIsNotFound", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		_, _, err := b.Get(context.Background(), "does/not/exist")
		if !IsNotFound(err) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	}},

	{name: "StatMissingIsNotFound", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		_, err := b.Stat(context.Background(), "does/not/exist")
		if !IsNotFound(err) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	}},

	{name: "StatReturnsSizeWithoutBody", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "k", "twelve bytes")
		info, err := b.Stat(ctx, "k")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != 12 {
			t.Errorf("Size = %d, want 12", info.Size)
		}
	}},

	{name: "ListByPrefix", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "dir/one", "1")
		mustPut(t, ctx, b, "dir/two", "2")
		mustPut(t, ctx, b, "other/three", "3")

		objs, err := b.List(ctx, "dir/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(objs) != 2 {
			t.Fatalf("List(\"dir/\") returned %d objects, want 2: %+v", len(objs), objs)
		}
	}},

	{name: "ListPrefixIsStringNotDirectory", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "ab/x", "1")
		mustPut(t, ctx, b, "abc/y", "2")

		objs, err := b.List(ctx, "ab")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(objs) != 2 {
			t.Fatalf("List(\"ab\") returned %d objects, want 2 (string prefix, not directory boundary): %+v", len(objs), objs)
		}
	}},

	{name: "ListMissingPrefixIsEmptyNotError", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		objs, err := b.List(context.Background(), "nothing/here/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(objs) != 0 {
			t.Errorf("List = %+v, want empty", objs)
		}
	}},

	{name: "ListResultsSortedByKey", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		for _, k := range []string{"p/c", "p/a", "p/b"} {
			mustPut(t, ctx, b, k, "x")
		}
		objs, err := b.List(ctx, "p/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !sort.SliceIsSorted(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key }) {
			t.Errorf("List results not sorted: %+v", objs)
		}
	}},

	{name: "ListSeesEveryPutKey", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		want := []string{"q/1", "q/2", "q/3", "q/4", "q/5"}
		for _, k := range want {
			mustPut(t, ctx, b, k, "x")
		}
		objs, err := b.List(ctx, "q/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(objs) != len(want) {
			t.Fatalf("List returned %d objects, want %d: %+v", len(objs), len(want), objs)
		}
	}},

	{name: "DeleteRemovesObject", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "k", "x")
		if err := b.Delete(ctx, "k"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := b.Stat(ctx, "k"); !IsNotFound(err) {
			t.Errorf("Stat after Delete: err = %v, want ErrNotFound", err)
		}
	}},

	{name: "DeleteMissingIsNotAnError", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		if err := b.Delete(context.Background(), "never/existed"); err != nil {
			t.Errorf("Delete of a missing key: err = %v, want nil", err)
		}
	}},

	{name: "DeletePrefixRemovesAllAndCounts", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		for _, k := range []string{"r/1", "r/2", "r/3"} {
			mustPut(t, ctx, b, k, "x")
		}
		n, err := b.DeletePrefix(ctx, "r/")
		if err != nil {
			t.Fatalf("DeletePrefix: %v", err)
		}
		if n != 3 {
			t.Errorf("DeletePrefix returned count %d, want 3", n)
		}
		objs, err := b.List(ctx, "r/")
		if err != nil {
			t.Fatalf("List after DeletePrefix: %v", err)
		}
		if len(objs) != 0 {
			t.Errorf("objects remain after DeletePrefix: %+v", objs)
		}
	}},

	{name: "DeletePrefixRejectsEmptyPrefix", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		_, err := b.DeletePrefix(context.Background(), "")
		if !IsInvalid(err) {
			t.Errorf("DeletePrefix(\"\"): err = %v, want ErrInvalid", err)
		}
	}},

	{name: "DeletePrefixLeavesSiblingsAlone", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		mustPut(t, ctx, b, "target/a", "x")
		mustPut(t, ctx, b, "sibling/b", "y")

		if _, err := b.DeletePrefix(ctx, "target/"); err != nil {
			t.Fatalf("DeletePrefix: %v", err)
		}
		if _, err := b.Stat(ctx, "sibling/b"); err != nil {
			t.Errorf("sibling object was affected: %v", err)
		}
	}},

	{name: "AwkwardKeyCharacters", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		keys := []string{
			"awkward/with spaces.txt",
			"awkward/unicode-éà中文.txt",
			"awkward/weird#chars?&=+.txt",
			"awkward/.hidden",
			"awkward/UPPERCASE.TXT",
		}
		for _, k := range keys {
			mustPut(t, ctx, b, k, "data-for-"+k)
			got, _ := mustGetString(t, ctx, b, k)
			if got != "data-for-"+k {
				t.Errorf("key %q: got %q", k, got)
			}
		}
	}},

	{name: "DeeplyNestedKeys", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		key := "a/b/c/d/e/f/g/h/i/j/deep.txt"
		mustPut(t, ctx, b, key, "deep")
		got, _ := mustGetString(t, ctx, b, key)
		if got != "deep" {
			t.Errorf("got %q, want deep", got)
		}
	}},

	{name: "ContextCancellationPropagates", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := b.Stat(ctx, "k"); !errors.Is(err, context.Canceled) {
			t.Errorf("Stat with cancelled ctx: err = %v, want context.Canceled", err)
		}
	}},

	{name: "UnreachableIsNotNotFound", run: func(t *testing.T, f backendFactory) {
		b := f.broken(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := b.Stat(ctx, "k")
		if err == nil {
			t.Fatal("Stat against a broken backend: want error, got nil")
		}
		if !IsUnreachable(err) {
			t.Errorf("err = %v, want ErrUnreachable", err)
		}
		if IsNotFound(err) {
			t.Errorf("err = %v must NOT also be ErrNotFound", err)
		}
	}},

	{name: "CorruptedObjectFailsVerification", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		original := mustPut(t, ctx, b, "k", "original content")
		// Overwrite with different content -- a manifest recorded against
		// the original Put must now fail Verify against what's actually
		// stored.
		mustPut(t, ctx, b, "k", "different content, different hash")

		actual, gotInfo := mustGetString(t, ctx, b, "k")
		m, err := NewManifestBuilder(0, time.Now(), Source{ClusterID: "c", Namespace: "n", Name: "e", UID: "u"}, "sha256:"+strings.Repeat("a", 64), "img").
			AddFile(original, "k").Build()
		if err != nil {
			t.Fatalf("building manifest: %v", err)
		}
		// The manifest recorded the FIRST Put's size/hash; verifying it
		// against what's actually stored now (after the second Put
		// overwrote it) must detect the mismatch as corruption, never
		// report it as a successful match.
		if err := m.Verify("k", gotInfo.Size, sha256Hex(actual)); !IsCorrupt(err) {
			t.Errorf("Verify after silent overwrite: err = %v, want ErrCorrupt", err)
		}
	}},

	{name: "ArchiveRoundTripThroughBackend", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		src := t.TempDir()
		writeTree(t, src, map[string]string{
			"file1.txt":        "hello",
			"nested/file2.txt": "world",
		})

		info, err := ArchiveTo(ctx, b, "archives/x.tar.zst", src, ArchiveOptions{})
		if err != nil {
			t.Fatalf("ArchiveTo: %v", err)
		}

		dst := t.TempDir()
		want := FileEntry{Name: "x.tar.zst", Size: info.Size, SHA256: info.SHA256}
		if err := RestoreFrom(ctx, b, "archives/x.tar.zst", dst, want, ExtractOptions{}); err != nil {
			t.Fatalf("RestoreFrom: %v", err)
		}
		assertTree(t, dst, map[string]string{
			"file1.txt":        "hello",
			"nested/file2.txt": "world",
		})
	}},

	{name: "ManifestRoundTripThroughBackend", run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		m, err := NewManifestBuilder(1, time.Now(), Source{ClusterID: "c", Namespace: "n", Name: "e", UID: "u"},
			"sha256:"+strings.Repeat("a", 64), "img:v1").
			AddFile(ObjectInfo{Size: 5, SHA256: sha256Hex("hello")}, "f").Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		data, err := m.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if _, err := b.Put(ctx, "manifests/m.json", bytes.NewReader(data), PutOptions{}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, _, err := b.Get(ctx, "manifests/m.json")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer func() { _ = rc.Close() }()
		parsed, err := ParseManifest(rc)
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		if parsed.Seq != 1 || len(parsed.Files) != 1 || parsed.Files[0].Name != "f" {
			t.Errorf("round-tripped manifest mismatch: %+v", parsed)
		}
	}},

	{name: "LargeStreamingObject", long: true, run: func(t *testing.T, f backendFactory) {
		b := f.fresh(t)
		ctx := context.Background()
		const size = 512 << 20 // 512 MiB

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		info, err := b.Put(ctx, "large/obj", io.LimitReader(zeroReader{}, size), PutOptions{})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if info.Size != size {
			t.Fatalf("Size = %d, want %d", info.Size, size)
		}

		rc, _, err := b.Get(ctx, "large/obj")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		n, err := io.Copy(io.Discard, rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("reading large object: %v", err)
		}
		if n != size {
			t.Fatalf("read %d bytes, want %d", n, size)
		}

		runtime.GC()
		runtime.ReadMemStats(&after)
		const bound = 200 << 20 // 200 MiB
		if after.HeapAlloc > before.HeapAlloc && after.HeapAlloc-before.HeapAlloc > bound {
			t.Errorf("heap grew by %d bytes streaming a %d-byte object, want < %d", after.HeapAlloc-before.HeapAlloc, size, bound)
		}
	}},
}

// zeroReader is an io.Reader producing an endless stream of zero bytes,
// used only to synthesize a large object without allocating it in memory.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		writeFile(t, root, rel, content)
	}
}

func assertTree(t *testing.T, root string, want map[string]string) {
	t.Helper()
	for rel, content := range want {
		got := readFile(t, root, rel)
		if got != content {
			t.Errorf("file %q: got %q, want %q", rel, got, content)
		}
	}
}
