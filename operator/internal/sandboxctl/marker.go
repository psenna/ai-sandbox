package sandboxctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TeardownMarker records, honestly, what a freeze actually destroyed and
// what survived. Written into BOTH archived roots (workspace and agent
// home) before either is archived, so it lands inside every snapshot and
// survives either archive alone -- including the recovery-Job path, where
// the agent-home emptyDir is gone and only the workspace PVC survives.
type TeardownMarker struct {
	SchemaVersion int       `json:"schemaVersion"` // 1
	Seq           int       `json:"seq"`
	FrozenAt      time.Time `json:"frozenAt"`
	Trigger       string    `json:"trigger"` // "agent-wait" | "suspend-or-timeout"
	Reason        string    `json:"reason"`  // status.waitFor.reason, if any
	Engine        string    `json:"engine"`
	Destroyed     Destroyed `json:"destroyed"`
	Preserved     []string  `json:"preserved"`
	SnapshotURI   string    `json:"snapshotURI"`
}

// Destroyed enumerates what teardown actually removed.
type Destroyed struct {
	Containers      []string `json:"containers"`
	ImageLayerCache bool     `json:"imageLayerCache"` // always true: excluded by design
	Notes           []string `json:"notes"`
}

// Marshal serializes m deterministically: 2-space indent, HTML escaping
// disabled, a single trailing newline -- matching internal/storage's
// Manifest.Marshal conventions, so the same struct always produces
// byte-identical output.
func (m TeardownMarker) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("sandboxctl: marshaling teardown marker: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderMarkdown renders the agent-readable RESUME.md, generated from the
// same struct as Marshal so the two can never disagree. States the contract
// in a fixed order: what was destroyed, what survived, and what the agent
// must do about it.
func (m TeardownMarker) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# This environment was frozen\n\n")
	fmt.Fprintf(&b, "Snapshot seq %d, taken %s (trigger: %s).\n\n", m.Seq, m.FrozenAt.UTC().Format(time.RFC3339), m.Trigger)
	if m.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n\n", m.Reason)
	}

	fmt.Fprintf(&b, "## What was destroyed\n\n")
	if len(m.Destroyed.Containers) == 0 {
		fmt.Fprintf(&b, "- No workload containers were running (engine: %s).\n", m.Engine)
	} else {
		for _, c := range m.Destroyed.Containers {
			fmt.Fprintf(&b, "- Container %q was stopped and removed.\n", c)
		}
	}
	if m.Destroyed.ImageLayerCache {
		fmt.Fprintf(&b, "- The container image/layer cache was destroyed (never snapshotted, by design).\n")
	}
	fmt.Fprintf(&b, "- Everything outside /workspace and the agent home: installed packages, /tmp, background processes, listening ports, environment mutations.\n")
	for _, n := range m.Destroyed.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}

	fmt.Fprintf(&b, "\n## What survived\n\n")
	if len(m.Preserved) == 0 {
		fmt.Fprintf(&b, "- /workspace in full (including .git, .claude/, and this marker).\n- The session transcript.\n")
	} else {
		for _, p := range m.Preserved {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}

	fmt.Fprintf(&b, "\n## What to do\n\n")
	fmt.Fprintf(&b, "Re-create any service you need (containers, background processes, listening ports) before relying on it. Do not assume anything outside /workspace is still there.\n")

	return b.String()
}

const (
	markerDirName      = ".sandbox"
	markerJSONName     = "last-freeze.json"
	markerMarkdownName = "RESUME.md"
	markerDirPerm      = 0o750
	markerFilePerm     = 0o640
)

// WriteMarkers writes the teardown marker (JSON + Markdown) into
// <workspaceRoot>/.sandbox/, and mirrors the JSON form into
// <agentHome>/last-freeze.json when agentHome is non-empty (skipped
// entirely for the recovery-Job case, where there is no agent home to
// write into).
func WriteMarkers(workspaceRoot, agentHome string, m TeardownMarker) error {
	jsonBytes, err := m.Marshal()
	if err != nil {
		return err
	}
	mdBytes := []byte(m.RenderMarkdown())

	dir := filepath.Join(workspaceRoot, markerDirName)
	if err := os.MkdirAll(dir, markerDirPerm); err != nil { //nolint:gosec // G301: 0750, deliberately non-world-readable marker directory
		return fmt.Errorf("sandboxctl: creating marker directory %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerJSONName), jsonBytes, markerFilePerm); err != nil { //nolint:gosec // G306: 0640, non-secret but not world-readable
		return fmt.Errorf("sandboxctl: writing %s: %w", markerJSONName, err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerMarkdownName), mdBytes, markerFilePerm); err != nil { //nolint:gosec // G306: 0640, non-secret but not world-readable
		return fmt.Errorf("sandboxctl: writing %s: %w", markerMarkdownName, err)
	}

	if agentHome == "" {
		return nil
	}
	if err := os.MkdirAll(agentHome, markerDirPerm); err != nil { //nolint:gosec // G301: 0750
		return fmt.Errorf("sandboxctl: creating agent home %s: %w", agentHome, err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, markerJSONName), jsonBytes, markerFilePerm); err != nil { //nolint:gosec // G306: 0640
		return fmt.Errorf("sandboxctl: writing agent-home %s: %w", markerJSONName, err)
	}
	return nil
}
