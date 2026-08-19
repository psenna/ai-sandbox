package sandboxctl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

func testWarmMarker() WarmMarker {
	return WarmMarker{
		SchemaVersion:  WarmMarkerSchemaVersion,
		EnvUID:         "uid-a",
		Seq:            3,
		SnapshotID:     "00003-2026-01-01T00:00:00Z",
		Root:           "workspace",
		ManifestSHA256: strings.Repeat("a", 64),
		Files: []storage.FileEntry{
			{Name: "workspace.tar.zst", Size: 100, SHA256: strings.Repeat("b", 64)},
		},
		WrittenAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WrittenBy: "freeze",
	}
}

func TestWarmMarker_MarshalIsDeterministic(t *testing.T) {
	m := testWarmMarker()
	b1, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b2, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal (2nd): %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("Marshal is not deterministic:\n%s\nvs\n%s", b1, b2)
	}
	if !bytes.HasSuffix(b1, []byte("\n")) {
		t.Error("Marshal output does not end with a single trailing newline")
	}
	want := `{
  "schemaVersion": 1,
  "envUID": "uid-a",
  "seq": 3,
  "snapshotID": "00003-2026-01-01T00:00:00Z",
  "root": "workspace",
  "manifestSHA256": "` + strings.Repeat("a", 64) + `",
  "files": [
    {
      "name": "workspace.tar.zst",
      "size": 100,
      "sha256": "` + strings.Repeat("b", 64) + `"
    }
  ],
  "writtenAt": "2026-01-01T00:00:00Z",
  "writtenBy": "freeze"
}
`
	if string(b1) != want {
		t.Errorf("Marshal golden mismatch:\n--- want ---\n%s\n--- got ---\n%s", want, b1)
	}
}

func TestWarmMarker_ParseRoundTrip(t *testing.T) {
	m := testWarmMarker()
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseWarmMarker(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseWarmMarker: %v", err)
	}
	if got.EnvUID != m.EnvUID || got.Seq != m.Seq || got.SnapshotID != m.SnapshotID || got.ManifestSHA256 != m.ManifestSHA256 {
		t.Errorf("ParseWarmMarker round-trip mismatch: got %+v, want %+v", got, m)
	}
}

func TestParseWarmMarker_RejectsUnknownFields(t *testing.T) {
	_, err := ParseWarmMarker(strings.NewReader(`{"schemaVersion":1,"bogus":"field"}`))
	if err == nil {
		t.Fatal("ParseWarmMarker: want error on unknown field, got nil")
	}
}

func TestWriteReadRemoveWarmMarker_RoundTrip(t *testing.T) {
	root := t.TempDir()
	m := testWarmMarker()
	if err := WriteWarmMarker(root, m); err != nil {
		t.Fatalf("WriteWarmMarker: %v", err)
	}

	path := filepath.Join(root, ".sandbox", "warm-cache.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("warm marker file not written: %v", err)
	}

	got, err := ReadWarmMarker(root)
	if err != nil {
		t.Fatalf("ReadWarmMarker: %v", err)
	}
	if got.SnapshotID != m.SnapshotID {
		t.Errorf("ReadWarmMarker: got %+v, want SnapshotID=%q", got, m.SnapshotID)
	}

	if err := RemoveWarmMarker(root); err != nil {
		t.Fatalf("RemoveWarmMarker: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("warm marker file still exists after RemoveWarmMarker: err=%v", err)
	}

	// Removing again (already gone) must not error.
	if err := RemoveWarmMarker(root); err != nil {
		t.Errorf("RemoveWarmMarker on an already-removed marker: unexpected error: %v", err)
	}
}

func TestReadWarmMarker_MissingFile(t *testing.T) {
	root := t.TempDir()
	_, err := ReadWarmMarker(root)
	if err == nil {
		t.Fatal("ReadWarmMarker on a workspace with no marker: want error, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("ReadWarmMarker error = %v, want an os.IsNotExist error", err)
	}
}

// TestValidateWarmMarker_EveryMissReason exercises each of the eight
// documented missReason outcomes, one independently-violated condition at a
// time, plus the fully-matching ok==true case.
func TestValidateWarmMarker_EveryMissReason(t *testing.T) {
	const envUID = "uid-a"
	const wantID = "00003-2026-01-01T00:00:00Z"
	const wantSeq = 3
	const manifestSHA = "deadbeef"
	wantFiles := []storage.FileEntry{{Name: "workspace.tar.zst", Size: 100, SHA256: strings.Repeat("c", 64)}}

	valid := func() WarmMarker {
		return WarmMarker{
			SchemaVersion: WarmMarkerSchemaVersion, EnvUID: envUID, Seq: wantSeq, SnapshotID: wantID,
			ManifestSHA256: manifestSHA, Files: append([]storage.FileEntry{}, wantFiles...),
		}
	}

	cases := []struct {
		name           string
		m              WarmMarker
		teardownSeq    int
		teardownOK     bool
		wantMissReason string
	}{
		{"ok", valid(), wantSeq, true, WarmMissNone},
		{"no marker (zero value)", WarmMarker{}, wantSeq, true, WarmMissNoMarker},
		{"unreadable marker", WarmMarker{SchemaVersion: unreadableWarmMarkerSchemaVersion}, wantSeq, true, WarmMissMarkerUnreadable},
		{"schema version", func() WarmMarker { m := valid(); m.SchemaVersion = 99; return m }(), wantSeq, true, WarmMissSchemaVersion},
		{"env uid mismatch", func() WarmMarker { m := valid(); m.EnvUID = "other"; return m }(), wantSeq, true, WarmMissEnvUIDMismatch},
		{"snapshot id mismatch", func() WarmMarker { m := valid(); m.SnapshotID = "wrong"; return m }(), wantSeq, true, WarmMissSnapshotIDMismatch},
		{"seq mismatch", func() WarmMarker { m := valid(); m.Seq = 99; return m }(), wantSeq, true, WarmMissSnapshotIDMismatch},
		{"manifest digest mismatch", func() WarmMarker { m := valid(); m.ManifestSHA256 = "different"; return m }(), wantSeq, true, WarmMissManifestDigestMismatch},
		{"file set mismatch (size)", func() WarmMarker {
			m := valid()
			m.Files = []storage.FileEntry{{Name: "workspace.tar.zst", Size: 999, SHA256: strings.Repeat("c", 64)}}
			return m
		}(), wantSeq, true, WarmMissFileSetMismatch},
		{"file set mismatch (count)", func() WarmMarker {
			m := valid()
			m.Files = append(m.Files, storage.FileEntry{Name: "extra", Size: 1, SHA256: strings.Repeat("d", 64)})
			return m
		}(), wantSeq, true, WarmMissFileSetMismatch},
		{"teardown marker missing", valid(), wantSeq, false, WarmMissTeardownMarkerMismatch},
		{"teardown marker seq mismatch", valid(), 999, true, WarmMissTeardownMarkerMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, missReason := ValidateWarmMarker(tc.m, envUID, wantID, wantSeq, manifestSHA, wantFiles, tc.teardownSeq, tc.teardownOK)
			wantOK := tc.wantMissReason == WarmMissNone
			if ok != wantOK {
				t.Errorf("ok = %v, want %v", ok, wantOK)
			}
			if missReason != tc.wantMissReason {
				t.Errorf("missReason = %q, want %q", missReason, tc.wantMissReason)
			}
		})
	}
}

// TestValidateWarmMarker_FileSetOrderIndependent verifies the file-set
// comparison does not depend on slice order.
func TestValidateWarmMarker_FileSetOrderIndependent(t *testing.T) {
	files := []storage.FileEntry{
		{Name: "a", Size: 1, SHA256: strings.Repeat("1", 64)},
		{Name: "b", Size: 2, SHA256: strings.Repeat("2", 64)},
	}
	reversed := []storage.FileEntry{files[1], files[0]}

	m := WarmMarker{
		SchemaVersion: WarmMarkerSchemaVersion, EnvUID: "u", Seq: 1, SnapshotID: "id",
		ManifestSHA256: "sha", Files: files,
	}
	ok, missReason := ValidateWarmMarker(m, "u", "id", 1, "sha", reversed, 1, true)
	if !ok {
		t.Errorf("ValidateWarmMarker: want ok=true regardless of file order, got missReason=%q", missReason)
	}
}
