package sandboxctl

import (
	"flag"
	"fmt"
	"time"
)

// RestoreConfig is how the restore init container learns which snapshot to
// restore and how. Like SnapshotConfig, every value here is projected via
// CLI flags (see internal/render/pod.go's restoreArgs) -- the restore
// container mounts no ConfigMap and has no additional RBAC beyond what the
// sidecar already has.
type RestoreConfig struct {
	// SnapshotID is the snapshot directory name to restore
	// ("<seq:05d>-<RFC3339>"), computed operator-side from status.snapshot
	// via storage.SnapshotID (internal/controller's restorePlanFor).
	SnapshotID string
	// Seq is the snapshot's sequence number. Cross-checked against the
	// manifest's own recorded Seq before anything is restored.
	Seq int
	// Warm, when true, attempts a warm restore first (validating the
	// retained PVC's warm-cache.json marker) before falling back to a cold
	// restore. false forces a cold restore unconditionally.
	Warm bool

	// Retries bounds retryStep's retry loop for each restore step.
	Retries int
	// StepTimeout bounds each individual manifest-get/archive-restore
	// attempt inside retryStep.
	StepTimeout time.Duration

	// MaxEntries and MaxTotalBytes cap storage.ExtractOptions for every
	// restored root; 0 means unlimited.
	MaxEntries    int64
	MaxTotalBytes int64
}

const (
	defaultRestoreRetries     = 4
	defaultRestoreStepTimeout = 2 * time.Minute
)

// registerRestoreFlags registers RestoreConfig's flags on fs, with
// environment-variable fallbacks matching config.go's envOr/envOrDuration
// convention (SANDBOX_SIDECAR_RESTORE_* names).
func registerRestoreFlags(fs *flag.FlagSet, c *RestoreConfig, getenv func(string) string) {
	fs.StringVar(&c.SnapshotID, "restore-snapshot-id", envOr(getenv, "RESTORE_SNAPSHOT_ID", ""),
		"snapshot directory name to restore (\"<seq:05d>-<RFC3339>\")")
	fs.IntVar(&c.Seq, "restore-seq", envOrInt(getenv, "RESTORE_SEQ", -1),
		"snapshot sequence number to restore; cross-checked against the manifest's own recorded seq")
	fs.BoolVar(&c.Warm, "restore-warm", envOrBool(getenv, "RESTORE_WARM", true),
		"attempt a warm restore (validating the retained PVC's warm-cache marker) before falling back to a cold restore")
	fs.IntVar(&c.Retries, "restore-retries", envOrInt(getenv, "RESTORE_RETRIES", defaultRestoreRetries),
		"number of retries for each restore step")
	fs.DurationVar(&c.StepTimeout, "restore-step-timeout", envOrDuration(getenv, "RESTORE_STEP_TIMEOUT", defaultRestoreStepTimeout),
		"per-attempt timeout for each restore step")
	fs.Int64Var(&c.MaxEntries, "restore-max-entries", envOrInt64(getenv, "RESTORE_MAX_ENTRIES", 0),
		"cap on the number of tar entries restored per root; 0 means unlimited")
	fs.Int64Var(&c.MaxTotalBytes, "restore-max-bytes", envOrInt64(getenv, "RESTORE_MAX_BYTES", 0),
		"cap on the total uncompressed bytes restored per root; 0 means unlimited")
}

// Validate checks RestoreConfig for internal consistency.
func (c RestoreConfig) Validate() error {
	if c.SnapshotID == "" {
		return fmt.Errorf("restore-snapshot-id: required")
	}
	if err := validateSnapshotIDSegment(c.SnapshotID); err != nil {
		return fmt.Errorf("restore-snapshot-id: %w", err)
	}
	if c.Seq < 0 {
		return fmt.Errorf("restore-seq: must be >= 0, got %d", c.Seq)
	}
	return nil
}

// validateSnapshotIDSegment applies the same safe-path-segment rules
// storage's internal validateSegment does (empty, ".", "..", a "/" or "\",
// a control character, or an over-length value are all rejected), so an
// invalid --restore-snapshot-id is caught at config-validation time rather
// than surfacing as a confusing storage.ErrInvalid deep inside restore.go's
// first backend call. Duplicated rather than exported from internal/storage
// because that package deliberately keeps validateSegment private (see
// path.go); storage.Layout.SnapshotFileByID performs the authoritative
// check regardless, so this is a fail-fast convenience, not the sole
// enforcement point.
func validateSnapshotIDSegment(seg string) error {
	const maxSegmentLen = 255
	if seg == "" {
		return fmt.Errorf("must not be empty")
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("%q is not allowed", seg)
	}
	if len(seg) > maxSegmentLen {
		return fmt.Errorf("length %d exceeds %d bytes", len(seg), maxSegmentLen)
	}
	for _, r := range seg {
		if r == '/' || r == '\\' || r == 0 || r < 0x20 || r == 0x7f {
			return fmt.Errorf("%q contains a disallowed character", seg)
		}
	}
	return nil
}

func envOrInt64(getenv func(string) string, name string, def int64) int64 {
	v := getenv(envPrefix + name)
	if v == "" {
		return def
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}
