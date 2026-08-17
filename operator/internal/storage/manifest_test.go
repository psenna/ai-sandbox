package storage

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testManifest(t *testing.T) Manifest {
	t.Helper()
	m, err := NewManifestBuilder(7, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Source{ClusterID: "cl", Namespace: "ns", Name: "env", UID: "uid-1", Generation: 3},
		"sha256:"+strings.Repeat("a", 64), "ghcr.io/example/agent:v1").
		AddFile(ObjectInfo{Size: 200, SHA256: strings.Repeat("b", 64)}, "workspace.tar.zst").
		AddFile(ObjectInfo{Size: 100, SHA256: strings.Repeat("c", 64)}, "agent-home.tar.zst").
		WithAgentImageDigest("sha256:" + strings.Repeat("d", 64)).
		Build()
	if err != nil {
		t.Fatalf("building test manifest: %v", err)
	}
	return m
}

func TestManifest_GoldenMarshal(t *testing.T) {
	m := testManifest(t)
	got, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	assertGoldenPath(t, "manifest.json", got)
}

func TestManifest_MarshalParseRoundTrip(t *testing.T) {
	m := testManifest(t)
	b1, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := ParseManifest(bytes.NewReader(b1))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	b2, err := parsed.Marshal()
	if err != nil {
		t.Fatalf("Marshal (2nd): %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("Marshal -> Parse -> Marshal not byte-identical:\n--- 1 ---\n%s\n--- 2 ---\n%s", b1, b2)
	}
}

func TestManifest_FileOrderIndependentOfAddOrder(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	src := Source{ClusterID: "c", Namespace: "n", Name: "e", UID: "u"}

	m1, err := NewManifestBuilder(1, base, src, "sha256:"+strings.Repeat("1", 64), "img:v1").
		AddFile(ObjectInfo{Size: 1, SHA256: strings.Repeat("a", 64)}, "z-file").
		AddFile(ObjectInfo{Size: 2, SHA256: strings.Repeat("b", 64)}, "a-file").
		Build()
	if err != nil {
		t.Fatalf("build m1: %v", err)
	}
	m2, err := NewManifestBuilder(1, base, src, "sha256:"+strings.Repeat("1", 64), "img:v1").
		AddFile(ObjectInfo{Size: 2, SHA256: strings.Repeat("b", 64)}, "a-file").
		AddFile(ObjectInfo{Size: 1, SHA256: strings.Repeat("a", 64)}, "z-file").
		Build()
	if err != nil {
		t.Fatalf("build m2: %v", err)
	}

	b1, err := m1.Marshal()
	if err != nil {
		t.Fatalf("Marshal m1: %v", err)
	}
	b2, err := m2.Marshal()
	if err != nil {
		t.Fatalf("Marshal m2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("manifests built with files added in different order marshaled differently:\n%s\nvs\n%s", b1, b2)
	}
}

func TestManifest_Validate(t *testing.T) {
	valid := func() Manifest { return testManifest(t) }

	cases := []struct {
		name    string
		mutate  func(m Manifest) Manifest
		wantErr bool
	}{
		{"valid", func(m Manifest) Manifest { return m }, false},
		{"bad schema version", func(m Manifest) Manifest { m.SchemaVersion = 2; return m }, true},
		{"negative seq", func(m Manifest) Manifest { m.Seq = -1; return m }, true},
		{"empty clusterID", func(m Manifest) Manifest { m.Source.ClusterID = ""; return m }, true},
		{"empty namespace", func(m Manifest) Manifest { m.Source.Namespace = ""; return m }, true},
		{"empty name", func(m Manifest) Manifest { m.Source.Name = ""; return m }, true},
		{"empty uid", func(m Manifest) Manifest { m.Source.UID = ""; return m }, true},
		{"empty specHash", func(m Manifest) Manifest { m.SpecHash = ""; return m }, true},
		{"empty agentImage", func(m Manifest) Manifest { m.AgentImage = ""; return m }, true},
		{"no files", func(m Manifest) Manifest { m.Files = nil; return m }, true},
		{"empty file name", func(m Manifest) Manifest { m.Files[0].Name = ""; return m }, true},
		{"negative file size", func(m Manifest) Manifest { m.Files[0].Size = -1; return m }, true},
		{"malformed sha256", func(m Manifest) Manifest { m.Files[0].SHA256 = "not-hex"; return m }, true},
		{"short sha256", func(m Manifest) Manifest { m.Files[0].SHA256 = "abc"; return m }, true},
		{"duplicate file name", func(m Manifest) Manifest {
			m.Files = append(m.Files, m.Files[0])
			return m
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.mutate(valid())
			err := m.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate(): want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate(): unexpected error: %v", err)
			}
			if tc.wantErr && !IsInvalid(err) {
				t.Errorf("Validate(): error %v is not ErrInvalid-kinded", err)
			}
		})
	}
}

func TestManifest_Verify(t *testing.T) {
	m := testManifest(t)

	if err := m.Verify("workspace.tar.zst", 200, strings.Repeat("b", 64)); err != nil {
		t.Errorf("Verify with matching size/sha256: unexpected error: %v", err)
	}

	err := m.Verify("workspace.tar.zst", 999, strings.Repeat("b", 64))
	if !IsCorrupt(err) {
		t.Errorf("Verify with wrong size: want ErrCorrupt, got %v", err)
	}

	err = m.Verify("workspace.tar.zst", 200, strings.Repeat("f", 64))
	if !IsCorrupt(err) {
		t.Errorf("Verify with wrong sha256: want ErrCorrupt, got %v", err)
	}

	err = m.Verify("does-not-exist", 1, strings.Repeat("a", 64))
	if !IsNotFound(err) {
		t.Errorf("Verify with unknown name: want ErrNotFound, got %v", err)
	}
}

func TestParseManifest_RejectsWrongSchemaVersionAndUnknownFields(t *testing.T) {
	m := testManifest(t)
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	bumped := bytes.Replace(b, []byte(`"schemaVersion": 1`), []byte(`"schemaVersion": 2`), 1)
	if bytes.Equal(bumped, b) {
		t.Fatalf("test setup: schemaVersion replacement did not match golden manifest shape")
	}
	if _, err := ParseManifest(bytes.NewReader(bumped)); err == nil {
		t.Error("ParseManifest with schemaVersion 2: want error, got nil")
	}

	withExtra := bytes.Replace(b, []byte(`"schemaVersion": 1,`), []byte(`"schemaVersion": 1, "unknownField": true,`), 1)
	if bytes.Equal(withExtra, b) {
		t.Fatalf("test setup: unknown-field injection did not match golden manifest shape")
	}
	if _, err := ParseManifest(bytes.NewReader(withExtra)); err == nil {
		t.Error("ParseManifest with an unknown field: want error, got nil")
	}
}

func TestManifest_File(t *testing.T) {
	m := testManifest(t)
	if _, ok := m.File("workspace.tar.zst"); !ok {
		t.Error("File(\"workspace.tar.zst\"): want found")
	}
	if _, ok := m.File("nope"); ok {
		t.Error("File(\"nope\"): want not found")
	}
}

func TestLatest_MarshalParseRoundTrip(t *testing.T) {
	l := Latest{SchemaVersion: ManifestSchemaVersion, Seq: 4, SnapshotID: "00004-2026-01-01T00:00:00Z", UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	b, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := ParseLatest(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseLatest: %v", err)
	}
	if parsed != l {
		t.Errorf("round trip mismatch: got %+v, want %+v", parsed, l)
	}
}

func TestParseLatest_RejectsWrongSchemaVersion(t *testing.T) {
	l := Latest{SchemaVersion: 2, Seq: 1, SnapshotID: "x", UpdatedAt: time.Now().UTC()}
	b, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseLatest(bytes.NewReader(b)); err == nil {
		t.Error("ParseLatest with schemaVersion 2: want error, got nil")
	}
}
