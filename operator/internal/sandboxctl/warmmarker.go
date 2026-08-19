package sandboxctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// WarmMarker records, honestly, that a verified snapshot matching the
// workspace root's current tree exists in the backend. Written by BOTH
// SnapshotHook.Freeze (as a new final step, after latest.json) and the
// restore container (after a successful cold restore) -- never before
// either has actually landed a manifest, unlike TeardownMarker (marker.go),
// which is written BEFORE archiving and describes what was destroyed, not
// what was uploaded.
//
// A WarmMarker's presence alone proves nothing: ValidateWarmMarker
// cross-checks it against the manifest it claims to describe (and the
// teardown marker's own recorded seq, a second witness) before any restore
// is allowed to skip the download. Any mismatch means fail-safe fallback to
// a full cold restore -- never a silent "probably fine".
type WarmMarker struct {
	SchemaVersion  int                 `json:"schemaVersion"`
	EnvUID         string              `json:"envUID"`
	Seq            int                 `json:"seq"`
	SnapshotID     string              `json:"snapshotID"`
	Root           string              `json:"root"`
	ManifestSHA256 string              `json:"manifestSHA256"`
	Files          []storage.FileEntry `json:"files"`
	WrittenAt      time.Time           `json:"writtenAt"`
	WrittenBy      string              `json:"writtenBy"` // "freeze" | "restore"
}

// WarmMarkerSchemaVersion is the only schema version this package writes or
// accepts.
const WarmMarkerSchemaVersion = 1

const (
	warmMarkerFileName = "warm-cache.json"
)

// Marshal serializes m deterministically: 2-space indent, HTML escaping
// disabled, a single trailing newline -- matching marker.go's
// TeardownMarker.Marshal conventions exactly.
func (m WarmMarker) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("sandboxctl: marshaling warm marker: %w", err)
	}
	return buf.Bytes(), nil
}

// ParseWarmMarker decodes a WarmMarker from r. Unknown fields are rejected
// so a marker written by a newer schema version fails loudly (surfaced as
// missReason "MarkerUnreadable") instead of being silently misread.
func ParseWarmMarker(r io.Reader) (WarmMarker, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var m WarmMarker
	if err := dec.Decode(&m); err != nil {
		return WarmMarker{}, fmt.Errorf("sandboxctl: parsing warm marker: %w", err)
	}
	return m, nil
}

// warmMarkerPath is <workspaceRoot>/.sandbox/warm-cache.json.
func warmMarkerPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, markerDirName, warmMarkerFileName)
}

// WriteWarmMarker writes m to <workspaceRoot>/.sandbox/warm-cache.json,
// matching marker.go's WriteMarkers permission conventions (0750 dir, 0640
// file).
func WriteWarmMarker(workspaceRoot string, m WarmMarker) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Join(workspaceRoot, markerDirName)
	if err := os.MkdirAll(dir, markerDirPerm); err != nil { //nolint:gosec // G301: 0750, deliberately non-world-readable marker directory
		return fmt.Errorf("sandboxctl: creating marker directory %s: %w", dir, err)
	}
	if err := os.WriteFile(warmMarkerPath(workspaceRoot), b, markerFilePerm); err != nil { //nolint:gosec // G306: 0640, non-secret but not world-readable
		return fmt.Errorf("sandboxctl: writing %s: %w", warmMarkerFileName, err)
	}
	return nil
}

// ReadWarmMarker reads and parses <workspaceRoot>/.sandbox/warm-cache.json.
func ReadWarmMarker(workspaceRoot string) (WarmMarker, error) {
	f, err := os.Open(warmMarkerPath(workspaceRoot)) //nolint:gosec // G304: workspaceRoot is caller-controlled and trusted, matching marker.go's own file reads
	if err != nil {
		return WarmMarker{}, err
	}
	defer func() { _ = f.Close() }()
	return ParseWarmMarker(f)
}

// RemoveWarmMarker deletes <workspaceRoot>/.sandbox/warm-cache.json. Not
// finding the file is success, not an error: a first-ever freeze, or a
// workspace that was never warm, has no marker to remove.
func RemoveWarmMarker(workspaceRoot string) error {
	err := os.Remove(warmMarkerPath(workspaceRoot))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sandboxctl: removing warm marker: %w", err)
	}
	return nil
}

// Warm-miss reasons -- the exhaustive set ValidateWarmMarker can return
// alongside ok=false. Every mismatch gets a DISTINCT reason so a human (or
// #33's future metrics) can tell "there was never a marker" apart from "the
// marker was there but wrong".
const (
	WarmMissNone                   = "" // ok == true
	WarmMissNoMarker               = "NoMarker"
	WarmMissMarkerUnreadable       = "MarkerUnreadable"
	WarmMissSchemaVersion          = "SchemaVersion"
	WarmMissEnvUIDMismatch         = "EnvUIDMismatch"
	WarmMissSnapshotIDMismatch     = "SnapshotIDMismatch"
	WarmMissManifestDigestMismatch = "ManifestDigestMismatch"
	WarmMissFileSetMismatch        = "FileSetMismatch"
	WarmMissTeardownMarkerMismatch = "TeardownMarkerMismatch"
)

// unreadableWarmMarkerSchemaVersion is the sentinel SchemaVersion value
// loadWarmMarker (restore.go) uses to signal "a warm-cache.json file
// exists, but failed to parse" into ValidateWarmMarker, distinct from the
// ordinary zero value (WarmMarker{}), which signals "no marker file at
// all". Both are legitimate inputs from a caller that never successfully
// obtained a real WarmMarker to validate.
const unreadableWarmMarkerSchemaVersion = -1

// ValidateWarmMarker reports whether m proves the retained PVC already
// holds wantID/wantSeq for envUID, cross-checked against the snapshot's
// actual manifest (manifestSHA256 the hash of the manifest.json bytes,
// wantFiles its parsed file list) and independently against the teardown
// marker's own recorded seq (a second witness). ANY mismatch means "not
// warm" -- never silently proceed as if it were.
//
// m is the zero value (WarmMarker{}) when no marker file exists at all, or
// carries SchemaVersion==unreadableWarmMarkerSchemaVersion when a marker
// file exists but failed to parse -- see loadWarmMarker in restore.go,
// which is the only production caller and constructs both sentinels.
func ValidateWarmMarker(m WarmMarker, envUID, wantID string, wantSeq int, manifestSHA256 string, wantFiles []storage.FileEntry, teardownSeq int, teardownOK bool) (ok bool, missReason string) {
	if m.SchemaVersion == unreadableWarmMarkerSchemaVersion {
		return false, WarmMissMarkerUnreadable
	}
	if m.SchemaVersion == 0 && m.EnvUID == "" && m.SnapshotID == "" {
		// Zero value: caller found no marker at all.
		return false, WarmMissNoMarker
	}
	if m.SchemaVersion != WarmMarkerSchemaVersion {
		return false, WarmMissSchemaVersion
	}
	if m.EnvUID != envUID {
		return false, WarmMissEnvUIDMismatch
	}
	if m.SnapshotID != wantID || m.Seq != wantSeq {
		return false, WarmMissSnapshotIDMismatch
	}
	if m.ManifestSHA256 != manifestSHA256 {
		return false, WarmMissManifestDigestMismatch
	}
	if !sameFileSet(m.Files, wantFiles) {
		return false, WarmMissFileSetMismatch
	}
	if !teardownOK || teardownSeq != wantSeq {
		return false, WarmMissTeardownMarkerMismatch
	}
	return true, WarmMissNone
}

// sameFileSet reports whether got and want describe the same set of files
// (name/size/sha256), independent of order.
func sameFileSet(got, want []storage.FileEntry) bool {
	if len(got) != len(want) {
		return false
	}
	index := make(map[string]storage.FileEntry, len(want))
	for _, f := range want {
		index[f.Name] = f
	}
	for _, g := range got {
		w, ok := index[g.Name]
		if !ok || w.Size != g.Size || w.SHA256 != g.SHA256 {
			return false
		}
	}
	return true
}
