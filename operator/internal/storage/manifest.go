package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"
)

// ManifestSchemaVersion is the only schema version this package understands.
// ParseManifest rejects any other value.
const ManifestSchemaVersion = 1

// Manifest describes one snapshot's contents: which files it contains, their
// sizes and checksums, and enough provenance (Source, SpecHash, AgentImage)
// to know whether a later restore is compatible with the environment it
// would be restored into.
type Manifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Seq           int       `json:"seq"`
	CreatedAt     time.Time `json:"createdAt"`
	Source        Source    `json:"source"`
	// SpecHash is "sha256:<hex>" -- see spechash.go.
	SpecHash string `json:"specHash"`
	// AgentImage is the agent container image this snapshot was produced
	// under. Required.
	AgentImage string `json:"agentImage"`
	// AgentImageDigest is the resolved image digest, if known. Optional --
	// this package never resolves a tag against a registry itself (gap G4).
	AgentImageDigest string      `json:"agentImageDigest,omitempty"`
	Files            []FileEntry `json:"files"`
}

// Source identifies which SandboxEnvironment a manifest belongs to.
type Source struct {
	ClusterID  string `json:"clusterID"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation,omitempty"`
}

// FileEntry describes one file within a snapshot.
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hex40 matches a 40-hex-character lowercase SHA-1 (a git commit-ish).
var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Marshal serializes m deterministically: Files sorted by Name, 2-space
// indent, HTML escaping disabled, a single trailing newline. Calling
// Marshal twice on equal values always produces byte-identical output --
// this is what makes manifest.json golden-testable and safe to
// content-address.
func (m Manifest) Marshal() ([]byte, error) {
	sorted := make([]FileEntry, len(m.Files))
	copy(sorted, m.Files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	m.Files = sorted

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("storage: marshaling manifest: %w", err)
	}
	// json.Encoder.Encode already appends exactly one trailing newline.
	return buf.Bytes(), nil
}

// ParseManifest decodes and validates a manifest from r. Unknown fields are
// rejected (DisallowUnknownFields) so a manifest written by a newer schema
// version fails loudly instead of being silently truncated.
func ParseManifest(r io.Reader) (Manifest, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("storage: parsing manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks structural well-formedness: schema version, non-negative
// seq, non-empty Source fields, non-empty SpecHash/AgentImage, at least one
// file, and every file entry has a non-empty name, a non-negative size, a
// 64-hex-character SHA-256, and no duplicate names.
func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("%w: manifest schemaVersion %d, want %d", ErrInvalid, m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.Seq < 0 {
		return fmt.Errorf("%w: manifest seq %d must be >= 0", ErrInvalid, m.Seq)
	}
	if m.Source.ClusterID == "" || m.Source.Namespace == "" || m.Source.Name == "" || m.Source.UID == "" {
		return fmt.Errorf("%w: manifest source fields (clusterID, namespace, name, uid) must all be non-empty", ErrInvalid)
	}
	if m.SpecHash == "" {
		return fmt.Errorf("%w: manifest specHash must not be empty", ErrInvalid)
	}
	if m.AgentImage == "" {
		return fmt.Errorf("%w: manifest agentImage must not be empty", ErrInvalid)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("%w: manifest must list at least one file", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		if f.Name == "" {
			return fmt.Errorf("%w: manifest file entry has empty name", ErrInvalid)
		}
		if f.Size < 0 {
			return fmt.Errorf("%w: manifest file %q has negative size %d", ErrInvalid, f.Name, f.Size)
		}
		if !hex64.MatchString(f.SHA256) {
			return fmt.Errorf("%w: manifest file %q has malformed sha256 %q", ErrInvalid, f.Name, f.SHA256)
		}
		if _, dup := seen[f.Name]; dup {
			return fmt.Errorf("%w: manifest has duplicate file name %q", ErrInvalid, f.Name)
		}
		seen[f.Name] = struct{}{}
	}
	return nil
}

// File returns the entry named name, if present.
func (m Manifest) File(name string) (FileEntry, bool) {
	for _, f := range m.Files {
		if f.Name == name {
			return f, true
		}
	}
	return FileEntry{}, false
}

// Verify checks a just-read file's actual size/SHA256 against what the
// manifest recorded for name. Returns an ErrNotFound-kinded error if name
// isn't listed in the manifest, an ErrCorrupt-kinded error on any mismatch,
// and nil only when both match exactly. Never reports a mismatch as
// success.
func (m Manifest) Verify(name string, gotSize int64, gotSHA256 string) error {
	want, ok := m.File(name)
	if !ok {
		return &Error{Op: "Verify", Backend: "manifest", Key: name, Kind: ErrNotFound}
	}
	if want.Size != gotSize || want.SHA256 != gotSHA256 {
		return &Error{Op: "Verify", Backend: "manifest", Key: name, Kind: ErrCorrupt,
			Err: fmt.Errorf("want size=%d sha256=%s, got size=%d sha256=%s", want.Size, want.SHA256, gotSize, gotSHA256)}
	}
	return nil
}

// ManifestBuilder incrementally builds a Manifest. File order in the built
// Manifest is independent of AddFile call order -- Marshal always sorts by
// Name.
type ManifestBuilder struct {
	m Manifest
}

// NewManifestBuilder starts a builder for a manifest with the given seq,
// creation time, source, spec hash and agent image.
func NewManifestBuilder(seq int, at time.Time, src Source, specHash, agentImage string) *ManifestBuilder {
	return &ManifestBuilder{m: Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Seq:           seq,
		CreatedAt:     at.UTC().Truncate(time.Second),
		Source:        src,
		SpecHash:      specHash,
		AgentImage:    agentImage,
	}}
}

// AddFile appends a file entry derived from info, stored under name.
func (b *ManifestBuilder) AddFile(info ObjectInfo, name string) *ManifestBuilder {
	b.m.Files = append(b.m.Files, FileEntry{Name: name, Size: info.Size, SHA256: info.SHA256})
	return b
}

// WithAgentImageDigest sets the optional resolved image digest.
func (b *ManifestBuilder) WithAgentImageDigest(d string) *ManifestBuilder {
	b.m.AgentImageDigest = d
	return b
}

// Build validates and returns the finished Manifest.
func (b *ManifestBuilder) Build() (Manifest, error) {
	if err := b.m.Validate(); err != nil {
		return Manifest{}, err
	}
	return b.m, nil
}

// Latest is the small pointer object written to Layout.Latest(), letting a
// caller find the most recent snapshot without a List call.
type Latest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Seq           int       `json:"seq"`
	SnapshotID    string    `json:"snapshotID"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Marshal serializes l deterministically (2-space indent, HTML escaping
// disabled, single trailing newline), matching Manifest.Marshal's
// conventions.
func (l Latest) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(l); err != nil {
		return nil, fmt.Errorf("storage: marshaling latest pointer: %w", err)
	}
	return buf.Bytes(), nil
}

// ParseLatest decodes a Latest pointer from r.
func ParseLatest(r io.Reader) (Latest, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var l Latest
	if err := dec.Decode(&l); err != nil {
		return Latest{}, fmt.Errorf("storage: parsing latest pointer: %w", err)
	}
	if l.SchemaVersion != ManifestSchemaVersion {
		return Latest{}, fmt.Errorf("%w: latest pointer schemaVersion %d, want %d", ErrInvalid, l.SchemaVersion, ManifestSchemaVersion)
	}
	return l, nil
}
