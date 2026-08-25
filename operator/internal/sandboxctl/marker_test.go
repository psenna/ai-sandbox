package sandboxctl

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestTeardownMarker_PodsRoundTripAndMarkdown(t *testing.T) {
	marker := TeardownMarker{
		SchemaVersion: 1,
		Seq:           7,
		Engine:        "k8s-native",
		Destroyed: Destroyed{
			// Containers is left nil: k8s-native populates Pods, not Containers,
			// and a nil []string marshals to `null` (the production form -- a
			// consumer must tolerate null, not assume []).
			Pods:            []string{"db", "python"},
			ImageLayerCache: true,
			Notes:           []string{"k8s-native: 2 pod(s) present at freeze"},
		},
	}

	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Back-compatible: the existing `containers` key is still present. For the
	// k8s-native path Containers is nil, which marshals to `null` (the real
	// production form -- snapshot.go sets Containers: report.Containers, nil
	// for k8s-native). Asserting `null` here matches what consumers will see.
	if !bytes.Contains(raw, []byte(`"containers":null`)) {
		t.Errorf("expected containers:null for the k8s-native path, got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"pods":["db","python"]`)) {
		t.Errorf("expected pods list in JSON, got %s", raw)
	}

	var got TeardownMarker
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Destroyed.Pods, marker.Destroyed.Pods) {
		t.Errorf("Pods round-trip = %v, want %v", got.Destroyed.Pods, marker.Destroyed.Pods)
	}

	md := marker.RenderMarkdown()
	for _, want := range []string{"Pods:", "db", "python"} {
		if !strings.Contains(md, want) {
			t.Errorf("RenderMarkdown missing %q in:\n%s", want, md)
		}
	}
}
