package storage

import (
	"fmt"
	"strings"
	"time"
)

// Path-layout constants -- the fixed directory/file names used under every
// environment's Root(). See doc.go for the full layout diagram.
const (
	DirSnapshots = "snapshots"
	DirArchive   = "archive"

	FileWorkspace = "workspace.tar.zst"
	FileAgentHome = "agent-home.tar.zst"
	FileManifest  = "manifest.json"
	FileRun       = "run.json"
	FileContext   = "context.tar.zst"
	FileLatest    = "latest.json"
)

const (
	maxTotalKeyLen = 1024
	maxSegmentLen  = 255
)

// Identity names one SandboxEnvironment uniquely enough to lay out a stable,
// collision-free object-key tree. EnvUID is mandatory: it is what makes a
// deleted-and-recreated environment (same ClusterID/Namespace/EnvName) land
// on a disjoint Root() rather than silently reusing -- or worse, appearing
// to inherit -- a previous incarnation's snapshots.
type Identity struct {
	ClusterID string
	Namespace string
	EnvName   string
	EnvUID    string
}

// Layout computes every object key this package writes or reads, for one
// environment, under a caller-supplied prefix. Layout is a pure value type;
// constructing one never touches storage.
type Layout struct {
	Prefix   string
	Identity Identity
}

// NewLayout constructs a Layout and validates it.
func NewLayout(prefix string, id Identity) (Layout, error) {
	l := Layout{Prefix: prefix, Identity: id}
	if err := l.Validate(); err != nil {
		return Layout{}, err
	}
	return l, nil
}

// Validate checks that every identity segment (and the prefix, if any) is
// safe to use as a path/object-key component: non-empty (for the mandatory
// identity fields), free of "/", "\", NUL and other control characters, not
// "." or "..", within the per-segment and total-key length limits.
func (l Layout) Validate() error {
	if l.Identity.ClusterID == "" {
		return fmt.Errorf("%w: clusterID is required", ErrInvalid)
	}
	if l.Identity.Namespace == "" {
		return fmt.Errorf("%w: namespace is required", ErrInvalid)
	}
	if l.Identity.EnvName == "" {
		return fmt.Errorf("%w: envName is required", ErrInvalid)
	}
	if l.Identity.EnvUID == "" {
		return fmt.Errorf("%w: envUID is required", ErrInvalid)
	}

	segments := []string{l.Identity.ClusterID, l.Identity.Namespace, l.Identity.EnvName, l.Identity.EnvUID}
	for _, seg := range segments {
		if err := validateSegment(seg); err != nil {
			return err
		}
	}
	if l.Prefix != "" {
		for _, seg := range strings.Split(l.Prefix, "/") {
			if err := validateSegment(seg); err != nil {
				return fmt.Errorf("prefix: %w", err)
			}
		}
	}

	if n := len(l.Root()); n > maxTotalKeyLen {
		return fmt.Errorf("%w: root key length %d exceeds %d bytes", ErrInvalid, n, maxTotalKeyLen)
	}
	return nil
}

func validateSegment(seg string) error {
	if seg == "" {
		return fmt.Errorf("%w: path segment must not be empty", ErrInvalid)
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("%w: path segment %q is not allowed", ErrInvalid, seg)
	}
	if len(seg) > maxSegmentLen {
		return fmt.Errorf("%w: path segment length %d exceeds %d bytes", ErrInvalid, len(seg), maxSegmentLen)
	}
	for _, r := range seg {
		if r == '/' || r == '\\' || r == 0 || r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: path segment %q contains a disallowed character", ErrInvalid, seg)
		}
	}
	return nil
}

// join builds a key from parts with "/", never path.Join or filepath.Join:
// path.Join calls Clean, which silently collapses a ".." segment instead of
// letting Validate reject it, and filepath.Join is OS-dependent (backslash
// on Windows). Every Layout method that builds a key goes through this.
func join(parts ...string) string {
	nonEmpty := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "/")
}

// Root is the top-level key prefix unique to this environment:
// "<prefix>/<clusterID>/<namespace>/<envName>/<envUID>", with no leading
// "/" when Prefix is empty and no trailing "/".
func (l Layout) Root() string {
	return join(l.Prefix, l.Identity.ClusterID, l.Identity.Namespace, l.Identity.EnvName, l.Identity.EnvUID)
}

// SnapshotID formats a deterministic, lexically-sortable snapshot directory
// name: "<seq:05d>-<RFC3339 timestamp>", with at first normalized to UTC and
// truncated to whole seconds (so two calls describing "the same instant" at
// different sub-second precision produce the same ID).
//
// Precondition: seq must be >= 0. Unlike Identity's segments (which
// ultimately derive from Kubernetes object metadata an attacker-adjacent
// caller could influence), seq is always an internal, monotonically
// increasing counter this package's own callers control -- so this
// function, matching its mandated pure/error-free signature, does not
// validate it. A negative seq still produces a well-formed (if unusual,
// sign-prefixed) key rather than a path-traversal or injection risk; it is
// simply not a supported input.
func SnapshotID(seq int, at time.Time) string {
	at = at.UTC().Truncate(time.Second)
	return fmt.Sprintf("%05d-%s", seq, at.Format(time.RFC3339))
}

// SnapshotsPrefix is the key prefix under which every snapshot directory for
// this environment lives.
func (l Layout) SnapshotsPrefix() string {
	return join(l.Root(), DirSnapshots)
}

// SnapshotDir is the key prefix for one specific snapshot.
func (l Layout) SnapshotDir(seq int, at time.Time) string {
	return join(l.SnapshotsPrefix(), SnapshotID(seq, at))
}

// SnapshotFile is the key for a named file within one specific snapshot.
func (l Layout) SnapshotFile(seq int, at time.Time, name string) string {
	return join(l.SnapshotDir(seq, at), name)
}

// SnapshotFileByID is SnapshotFile for a snapshot named by its already-
// formatted ID (SnapshotID's output, or a Latest pointer's SnapshotID
// field) rather than by (seq, at). Unlike SnapshotFile's (seq, at) --
// always internally computed -- id can arrive from outside this process
// (the restore init container's --restore-snapshot-id flag), so it is
// validated as a path segment, never trusted: an empty id, "." / "..", a
// "/" or a control character is rejected with an ErrInvalid-kinded error
// rather than silently escaping the environment's own Root().
func (l Layout) SnapshotFileByID(id, name string) (string, error) {
	if err := validateSegment(id); err != nil {
		return "", fmt.Errorf("snapshot id: %w", err)
	}
	return join(l.SnapshotsPrefix(), id, name), nil
}

// Workspace is the key for a snapshot's workspace archive.
func (l Layout) Workspace(seq int, at time.Time) string {
	return l.SnapshotFile(seq, at, FileWorkspace)
}

// AgentHome is the key for a snapshot's agent-home archive.
func (l Layout) AgentHome(seq int, at time.Time) string {
	return l.SnapshotFile(seq, at, FileAgentHome)
}

// Manifest is the key for a snapshot's manifest.
func (l Layout) Manifest(seq int, at time.Time) string {
	return l.SnapshotFile(seq, at, FileManifest)
}

// ArchivePrefix is the key prefix for this environment's terminal archive.
func (l Layout) ArchivePrefix() string {
	return join(l.Root(), DirArchive)
}

// ArchiveRun is the key for the terminal archive's run record.
func (l Layout) ArchiveRun() string {
	return join(l.ArchivePrefix(), FileRun)
}

// ArchiveContext is the key for the terminal archive's context bundle.
func (l Layout) ArchiveContext() string {
	return join(l.ArchivePrefix(), FileContext)
}

// Latest is the key for this environment's pointer to its most recent
// snapshot.
func (l Layout) Latest() string {
	return join(l.Root(), FileLatest)
}
