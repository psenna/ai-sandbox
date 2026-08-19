package sandboxctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WakeMarker is purely informational for the agent (surfaced via the
// unfreeze skill's "how to reconcile against reality" table) -- never
// enforced or re-read by the operator or this package itself. Written by
// the restore container after every wake, warm or cold, so the agent
// always has an honest, self-contained account of what just happened to
// it.
type WakeMarker struct {
	SchemaVersion      int       `json:"schemaVersion"`
	Seq                int       `json:"seq"`
	SnapshotID         string    `json:"snapshotID"`
	Source             string    `json:"source"` // "Warm" | "Cold"
	WarmMissReason     string    `json:"warmMissReason,omitempty"`
	RestoredAt         time.Time `json:"restoredAt"`
	BytesDownloaded    int64     `json:"bytesDownloaded"`
	SnapshotAgentImage string    `json:"snapshotAgentImage,omitempty"`
	CurrentAgentImage  string    `json:"currentAgentImage,omitempty"`
	SnapshotSpecHash   string    `json:"snapshotSpecHash,omitempty"`
	CurrentSpecHash    string    `json:"currentSpecHash,omitempty"`
	SpecChanged        bool      `json:"specChanged"`
}

// WakeMarkerSchemaVersion is the only schema version this package writes.
const WakeMarkerSchemaVersion = 1

const wakeMarkerFileName = "last-wake.json"

// Marshal serializes m deterministically, matching WarmMarker.Marshal's and
// marker.go's TeardownMarker.Marshal's conventions exactly.
func (m WakeMarker) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("sandboxctl: marshaling wake marker: %w", err)
	}
	return buf.Bytes(), nil
}

// wakeMarkerPath is <workspaceRoot>/.sandbox/last-wake.json.
func wakeMarkerPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, markerDirName, wakeMarkerFileName)
}

// WriteWakeMarker writes m to <workspaceRoot>/.sandbox/last-wake.json.
func WriteWakeMarker(workspaceRoot string, m WakeMarker) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Join(workspaceRoot, markerDirName)
	if err := os.MkdirAll(dir, markerDirPerm); err != nil { //nolint:gosec // G301: 0750, deliberately non-world-readable marker directory
		return fmt.Errorf("sandboxctl: creating marker directory %s: %w", dir, err)
	}
	if err := os.WriteFile(wakeMarkerPath(workspaceRoot), b, markerFilePerm); err != nil { //nolint:gosec // G306: 0640, non-secret but not world-readable
		return fmt.Errorf("sandboxctl: writing %s: %w", wakeMarkerFileName, err)
	}
	return nil
}
