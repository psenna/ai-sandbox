package sandboxctl

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testMarker() TeardownMarker {
	return TeardownMarker{
		SchemaVersion: 1,
		Seq:           2,
		FrozenAt:      time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		Trigger:       "agent-wait",
		Reason:        "waiting on CI",
		Engine:        "none",
		Destroyed: Destroyed{
			Containers:      nil,
			ImageLayerCache: true,
			Notes:           []string{"engine.type=none: no workload containers to tear down"},
		},
		Preserved:   []string{"/workspace in full", "the session transcript"},
		SnapshotURI: "s3://bucket/default/ns/env/uid/snapshots/00002-2026-08-16T12:00:00Z",
	}
}

func TestTeardownMarker_MarshalIsDeterministic(t *testing.T) {
	m := testMarker()
	b1, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b2, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("Marshal is not deterministic:\n%s\nvs\n%s", b1, b2)
	}
	if len(b1) == 0 || b1[len(b1)-1] != '\n' {
		t.Error("Marshal output does not end with a trailing newline")
	}
}

func TestTeardownMarker_RenderMarkdownMentionsContract(t *testing.T) {
	m := testMarker()
	md := m.RenderMarkdown()
	for _, want := range []string{"destroyed", "survived", "image/layer cache"} {
		if !bytes.Contains([]byte(md), []byte(want)) {
			t.Errorf("RenderMarkdown output does not mention %q:\n%s", want, md)
		}
	}
}

func TestWriteMarkers_WritesBothRootsAndSkipsEmptyAgentHome(t *testing.T) {
	ws := t.TempDir()
	ah := t.TempDir()
	m := testMarker()

	if err := WriteMarkers(ws, ah, m); err != nil {
		t.Fatalf("WriteMarkers: %v", err)
	}
	for _, p := range []string{
		filepath.Join(ws, ".sandbox", "last-freeze.json"),
		filepath.Join(ws, ".sandbox", "RESUME.md"),
		filepath.Join(ah, "last-freeze.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}

	// Empty agentHome (the recovery-Job case) writes only into workspaceRoot.
	ws2 := t.TempDir()
	if err := WriteMarkers(ws2, "", m); err != nil {
		t.Fatalf("WriteMarkers (no agent home): %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws2, ".sandbox", "last-freeze.json")); err != nil {
		t.Errorf("expected workspace marker to exist: %v", err)
	}
}
