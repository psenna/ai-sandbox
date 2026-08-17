package storage

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updatePath = flag.Bool("update", false, "rewrite golden files under testdata/ (shared across this package's test files)")

func assertGoldenPath(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updatePath {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: path is always filepath.Join("testdata", name) for a name literal passed by a test in this package
	if err != nil {
		t.Fatalf("reading golden file %s (run with -update to create it): %v", path, err)
	}
	if string(want) != string(got) {
		t.Errorf("golden mismatch for %s (run with -update to refresh):\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func validIdentity() Identity {
	return Identity{ClusterID: "cluster-a", Namespace: "ns-a", EnvName: "env-a", EnvUID: "uid-a"}
}

func TestNewLayout_ValidatesInputs(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		id      Identity
		wantErr bool
	}{
		{"valid no prefix", "", validIdentity(), false},
		{"valid with prefix", "backups", validIdentity(), false},
		{"valid nested prefix", "backups/sandbox", validIdentity(), false},
		{"leading slash prefix", "/backups", validIdentity(), true},
		{"trailing slash prefix", "backups/", validIdentity(), true},
		{"missing clusterID", "", Identity{Namespace: "ns", EnvName: "e", EnvUID: "u"}, true},
		{"missing namespace", "", Identity{ClusterID: "c", EnvName: "e", EnvUID: "u"}, true},
		{"missing envName", "", Identity{ClusterID: "c", Namespace: "ns", EnvUID: "u"}, true},
		{"missing envUID", "", Identity{ClusterID: "c", Namespace: "ns", EnvName: "e"}, true},
		{"empty everything", "", Identity{}, true},
		{"slash in segment", "", Identity{ClusterID: "c/x", Namespace: "ns", EnvName: "e", EnvUID: "u"}, true},
		{"backslash in segment", "", Identity{ClusterID: "c", Namespace: `n\x`, EnvName: "e", EnvUID: "u"}, true},
		{"NUL in segment", "", Identity{ClusterID: "c", Namespace: "n\x00x", EnvName: "e", EnvUID: "u"}, true},
		{"dot segment", "", Identity{ClusterID: ".", Namespace: "ns", EnvName: "e", EnvUID: "u"}, true},
		{"dotdot segment", "", Identity{ClusterID: "..", Namespace: "ns", EnvName: "e", EnvUID: "u"}, true},
		{"awkward but legal: spaces", "", Identity{ClusterID: "cluster a", Namespace: "ns", EnvName: "e", EnvUID: "u"}, false},
		{"awkward but legal: punctuation", "", Identity{ClusterID: "c#?&=+", Namespace: "ns", EnvName: "e", EnvUID: "u"}, false},
		{"awkward but legal: unicode", "", Identity{ClusterID: "unicode-éà中文", Namespace: "ns", EnvName: "e", EnvUID: "u"}, false},
		{"awkward but legal: uppercase", "", Identity{ClusterID: "CLUSTER", Namespace: "ns", EnvName: "e", EnvUID: "u"}, false},
		{"awkward but legal: leading dot", "", Identity{ClusterID: ".hidden", Namespace: "ns", EnvName: "e", EnvUID: "u"}, false},
		{"255-byte segment ok", "", Identity{ClusterID: strings.Repeat("a", 255), Namespace: "ns", EnvName: "e", EnvUID: "u"}, false},
		{"256-byte segment rejected", "", Identity{ClusterID: strings.Repeat("a", 256), Namespace: "ns", EnvName: "e", EnvUID: "u"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLayout(tc.prefix, tc.id)
			if tc.wantErr && err == nil {
				t.Fatalf("NewLayout(%q, %+v): want error, got nil", tc.prefix, tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("NewLayout(%q, %+v): unexpected error: %v", tc.prefix, tc.id, err)
			}
			if tc.wantErr && err != nil && !IsInvalid(err) {
				t.Errorf("NewLayout(%q, %+v): error %v is not ErrInvalid-kinded", tc.prefix, tc.id, err)
			}
		})
	}
}

func TestLayout_TotalKeyLengthLimit(t *testing.T) {
	id := Identity{
		ClusterID: strings.Repeat("a", 255),
		Namespace: strings.Repeat("b", 255),
		EnvName:   strings.Repeat("c", 255),
		EnvUID:    strings.Repeat("d", 255),
	}
	// Root() alone is ~4*255 + 3 separators = 1023 bytes, safely under 1024;
	// adding any real prefix pushes the total over the limit.
	if _, err := NewLayout("", id); err != nil {
		t.Fatalf("NewLayout with maximal segments and no prefix: unexpected error: %v", err)
	}
	if _, err := NewLayout(strings.Repeat("p", 200), id); err == nil {
		t.Fatalf("NewLayout with maximal segments plus a long prefix: want error, got nil")
	}
}

func TestLayout_Root(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		id     Identity
		want   string
	}{
		{"no prefix", "", validIdentity(), "cluster-a/ns-a/env-a/uid-a"},
		{"simple prefix", "backups", validIdentity(), "backups/cluster-a/ns-a/env-a/uid-a"},
		{"nested prefix", "backups/sandbox/v1", validIdentity(), "backups/sandbox/v1/cluster-a/ns-a/env-a/uid-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLayout(tc.prefix, tc.id)
			if err != nil {
				t.Fatalf("NewLayout: %v", err)
			}
			if got := l.Root(); got != tc.want {
				t.Errorf("Root() = %q, want %q", got, tc.want)
			}
			if strings.HasPrefix(l.Root(), "/") {
				t.Errorf("Root() must not have a leading slash when prefix is empty: %q", l.Root())
			}
			if strings.HasSuffix(l.Root(), "/") {
				t.Errorf("Root() must not have a trailing slash: %q", l.Root())
			}
		})
	}
}

// TestLayout_RecreatedUIDCollision verifies that two environments sharing
// ClusterID/Namespace/EnvName but differing only in EnvUID (the
// deleted-and-recreated-environment case) produce disjoint roots, neither a
// prefix of the other -- this is the entire reason EnvUID is mandatory.
func TestLayout_RecreatedUIDCollision(t *testing.T) {
	id1 := Identity{ClusterID: "c", Namespace: "ns", EnvName: "env", EnvUID: "uid-1"}
	id2 := Identity{ClusterID: "c", Namespace: "ns", EnvName: "env", EnvUID: "uid-2"}
	l1, err := NewLayout("", id1)
	if err != nil {
		t.Fatalf("NewLayout(id1): %v", err)
	}
	l2, err := NewLayout("", id2)
	if err != nil {
		t.Fatalf("NewLayout(id2): %v", err)
	}
	r1, r2 := l1.Root(), l2.Root()
	if r1 == r2 {
		t.Fatalf("recreated environment must not collide: both roots are %q", r1)
	}
	if strings.HasPrefix(r2, r1) || strings.HasPrefix(r1, r2) {
		t.Errorf("neither root may be a prefix of the other: %q vs %q", r1, r2)
	}
}

func TestSnapshotID(t *testing.T) {
	at := time.Date(2026, 8, 16, 12, 30, 0, 0, time.FixedZone("UTC+2", 2*3600))
	cases := []struct {
		seq  int
		want string
	}{
		{0, "00000-2026-08-16T10:30:00Z"},
		{99999, "99999-2026-08-16T10:30:00Z"},
		{100000, "100000-2026-08-16T10:30:00Z"}, // not truncated
	}
	for _, tc := range cases {
		if got := SnapshotID(tc.seq, at); got != tc.want {
			t.Errorf("SnapshotID(%d, ..) = %q, want %q", tc.seq, got, tc.want)
		}
	}
}

// TestSnapshotID_UTCNormalizationAndSubSecondTruncation verifies two calls
// describing "the same instant" (same UTC second, different zone/sub-second
// precision) produce byte-identical IDs.
func TestSnapshotID_UTCNormalizationAndSubSecondTruncation(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	zoned := utc.In(time.FixedZone("X", -5*3600))
	withNanos := utc.Add(999 * time.Millisecond)

	base := SnapshotID(1, utc)
	if got := SnapshotID(1, zoned); got != base {
		t.Errorf("zone-shifted time produced a different ID: %q vs %q", got, base)
	}
	if got := SnapshotID(1, withNanos); got != base {
		t.Errorf("sub-second time was not truncated: %q vs %q", got, base)
	}
}

func TestSnapshotID_NegativeSeq(t *testing.T) {
	// SnapshotID's mandated signature returns no error (seq is an
	// internal-only counter, never untrusted input -- see path.go's doc
	// comment). A negative seq still produces a well-formed, sign-prefixed
	// string rather than panicking or corrupting later segments.
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := SnapshotID(-1, at)
	if !strings.HasPrefix(got, "-") {
		t.Errorf("SnapshotID(-1, ..) = %q, want a sign-prefixed string", got)
	}
}

func TestLayout_SnapshotAndArchiveKeys(t *testing.T) {
	l, err := NewLayout("prefix", validIdentity())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	at := time.Date(2026, 8, 16, 10, 30, 0, 0, time.UTC)
	seq := 3

	wantDir := l.Root() + "/snapshots/00003-2026-08-16T10:30:00Z"
	if got := l.SnapshotDir(seq, at); got != wantDir {
		t.Errorf("SnapshotDir = %q, want %q", got, wantDir)
	}
	if got := l.Workspace(seq, at); got != wantDir+"/workspace.tar.zst" {
		t.Errorf("Workspace = %q", got)
	}
	if got := l.AgentHome(seq, at); got != wantDir+"/agent-home.tar.zst" {
		t.Errorf("AgentHome = %q", got)
	}
	if got := l.Manifest(seq, at); got != wantDir+"/manifest.json" {
		t.Errorf("Manifest = %q", got)
	}
	if got := l.SnapshotsPrefix(); got != l.Root()+"/snapshots" {
		t.Errorf("SnapshotsPrefix = %q", got)
	}
	if got := l.ArchivePrefix(); got != l.Root()+"/archive" {
		t.Errorf("ArchivePrefix = %q", got)
	}
	if got := l.ArchiveRun(); got != l.Root()+"/archive/run.json" {
		t.Errorf("ArchiveRun = %q", got)
	}
	if got := l.ArchiveContext(); got != l.Root()+"/archive/context.tar.zst" {
		t.Errorf("ArchiveContext = %q", got)
	}
	if got := l.Latest(); got != l.Root()+"/latest.json" {
		t.Errorf("Latest = %q", got)
	}
}

// TestPathLayout_Golden pins down the full set of keys for one fixed
// identity/prefix/seq/time combination, so an accidental layout change is
// caught even if no individual assertion above happens to notice it.
func TestPathLayout_Golden(t *testing.T) {
	l, err := NewLayout("acme-prod", Identity{
		ClusterID: "cl-1", Namespace: "team-a", EnvName: "review-42", EnvUID: "a1b2c3d4",
	})
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	var sb strings.Builder
	sb.WriteString("Root:            " + l.Root() + "\n")
	sb.WriteString("SnapshotsPrefix: " + l.SnapshotsPrefix() + "\n")
	sb.WriteString("SnapshotDir:     " + l.SnapshotDir(7, at) + "\n")
	sb.WriteString("Workspace:       " + l.Workspace(7, at) + "\n")
	sb.WriteString("AgentHome:       " + l.AgentHome(7, at) + "\n")
	sb.WriteString("Manifest:        " + l.Manifest(7, at) + "\n")
	sb.WriteString("ArchivePrefix:   " + l.ArchivePrefix() + "\n")
	sb.WriteString("ArchiveRun:      " + l.ArchiveRun() + "\n")
	sb.WriteString("ArchiveContext:  " + l.ArchiveContext() + "\n")
	sb.WriteString("Latest:          " + l.Latest() + "\n")

	assertGoldenPath(t, "path_layout.txt", []byte(sb.String()))
}
