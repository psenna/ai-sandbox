package render

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// MaxNameLen is the Kubernetes object-name (and label-value) length
	// limit that every rendered child name and label value must satisfy.
	MaxNameLen = 63
	hashLen    = 8

	SuffixWorkspace      = "-workspace" // PVC
	SuffixServiceAccount = "-agent"     // ServiceAccount
	SuffixSecret         = "-env"       // Secret
	SuffixConfigMap      = "-config"    // ConfigMap
	SuffixSidecarRBAC    = "-sidecar"   // Role AND RoleBinding (different kinds, same name is legal)
	SuffixPod            = "-run"       // Pod -- NOT "-agent", already taken by SuffixServiceAccount
	SuffixSnapshotSecret = "-snapshot"  // Secret (S3 snapshot credentials projection, #28)
	SuffixSnapshotJob    = "-freeze"    // Job (recovery snapshot Job, #28)
	SuffixNetworkPolicy  = "-netpolicy" // NetworkPolicy (#31)
)

// Names holds every child object name deterministically derived from one
// environment's name.
type Names struct {
	PVC            string
	ServiceAccount string
	Secret         string
	ConfigMap      string
	Role           string
	RoleBinding    string
	Pod            string
	SnapshotSecret string
	SnapshotJob    string
	NetworkPolicy  string
}

// ChildNames computes every child object name for envName.
func ChildNames(envName string) Names {
	return Names{
		PVC:            childName(envName, SuffixWorkspace),
		ServiceAccount: childName(envName, SuffixServiceAccount),
		Secret:         childName(envName, SuffixSecret),
		ConfigMap:      childName(envName, SuffixConfigMap),
		Role:           childName(envName, SuffixSidecarRBAC),
		RoleBinding:    childName(envName, SuffixSidecarRBAC),
		Pod:            childName(envName, SuffixPod),
		SnapshotSecret: childName(envName, SuffixSnapshotSecret),
		SnapshotJob:    childName(envName, SuffixSnapshotJob),
		NetworkPolicy:  childName(envName, SuffixNetworkPolicy),
	}
}

// EnvironmentLabelValue truncates envName to a valid, <=63-char label value
// using the same scheme as childName, with an empty suffix.
func EnvironmentLabelValue(envName string) string {
	return childName(envName, "")
}

// childName deterministically derives a child object name (or label value)
// from envName and suffix, guaranteeing the result is <=63 characters:
//
//   - if envName+suffix already fits, it is returned unchanged.
//   - otherwise, envName is truncated and an 8-hex-character SHA-256 hash of
//     the FULL (untruncated) envName is appended before suffix, so two long
//     names sharing a common prefix still produce different results.
func childName(envName, suffix string) string {
	if len(envName)+len(suffix) <= MaxNameLen {
		return envName + suffix
	}
	sum := sha256.Sum256([]byte(envName))
	h := hex.EncodeToString(sum[:])[:hashLen]
	keep := MaxNameLen - len(suffix) - 1 - hashLen
	if keep < 0 {
		keep = 0
	}
	base := envName
	if keep < len(base) {
		base = base[:keep]
	}
	base = strings.TrimRight(base, "-._")
	return base + "-" + h + suffix
}
