// Package storage implements the two snapshot/archive storage backends a
// SandboxClass can select (BackendSpec.Type: "s3" or "pvc"), the object-key
// path layout both backends share, and the snapshot manifest format that
// makes a restore verifiable rather than merely hopeful.
//
// # Purity contract
//
// This package MUST NOT import "sigs.k8s.io/controller-runtime" or any
// client.Client-related package, and it never talks to the Kubernetes API
// server. It MAY import "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
// (config.go's CRD adapters read CRD field shapes) and
// "github.com/go-logr/logr" (unused today, reserved so a future caller can
// pass a logger through without this package needing an import-graph
// change). Unlike internal/render and internal/lifecycle, this package DOES
// perform real I/O -- that is its entire job, streaming bytes to and from
// S3 or a mounted filesystem -- but the I/O is always against the
// configured backend or the local filesystem, never against a cluster.
//
// # No-logging rule
//
// Nothing in this package logs anything, ever. It only returns errors.
// This is a deliberate, structural decision, not an oversight: a Backend
// method's error can legitimately need to describe a credential-adjacent
// failure (a 403 from S3, a permission-denied on a mounted volume), and the
// single most reliable way to guarantee a credential never leaks into a log
// line is to make sure this package never has a logging call site at all.
// Compare this to Secret's and Credentials' redaction machinery in
// credentials.go, which is the belt to this rule's suspenders: even a
// future caller that DOES log a Credentials value (in internal/controller,
// once #28/#29 resolves one) gets structural redaction for free.
//
// # Credential-resolution boundary
//
// This package never reads a Kubernetes Secret. Backend construction
// (NewS3) takes an already-resolved Credentials value; the CALLER is
// responsible for reading S3Backend.CredentialsSecretRef.Name out of a
// Secret and populating Credentials from it. This exactly mirrors two
// existing precedents in this operator: internal/render.Credentials
// (populated by internal/controller's resolveCredentials from a
// class-referenced Secret, and never read by internal/render itself) and
// internal/controller/resources.go's resolveCredentials function itself.
// Wiring an actual Secret-resolver that produces a storage.Credentials is
// explicitly out of scope for this issue -- that lands with the reconciler
// work in #28/#29.
//
// # Path layout
//
// Every object this package writes lives under one environment's Root():
//
//	<prefix>/<clusterID>/<namespace>/<envName>/<envUID>/
//	  snapshots/
//	    <seq:05d>-<RFC3339 timestamp>/
//	      workspace.tar.zst
//	      agent-home.tar.zst
//	      manifest.json
//	  archive/
//	    run.json
//	    context.tar.zst
//	  latest.json
//
// EnvUID is mandatory and is what makes a deleted-and-recreated environment
// (same cluster/namespace/name) land on a disjoint Root(): the UID always
// changes on recreation, so a stale snapshot from a previous incarnation of
// "the same" environment can never be silently picked up by the new one.
// Every key is built with strings.Join(parts, "/") -- never path.Join
// (which calls Clean and would silently swallow a ".." segment instead of
// letting Layout.Validate reject it) and never filepath.Join (OS-dependent
// separator). See path.go for the full rule set path segments must satisfy.
//
// # NotFound-vs-Unreachable contract
//
// Every Backend method distinguishes "the object definitely does not
// exist" (ErrNotFound) from "the backend could not be reached, or answered
// ambiguously" (ErrUnreachable) -- see errors.go. This distinction is the
// entire reason IsRetryable exists: retrying a definite NotFound can only
// waste time, but treating a genuinely transient failure (a dial refusal, a
// 5xx, an ENOTDIR on a not-yet-mounted PVC) as a definite NotFound risks a
// caller concluding "there is nothing to restore" and silently producing an
// empty workspace instead of surfacing the real problem. Both backends'
// classifiers (classifyS3Error, classifyFSError) are fail-safe: an error
// they don't specifically recognize defaults to ErrUnreachable, never to
// ErrNotFound.
//
// # Why Put isn't retried
//
// retryDo (retry.go) wraps Get/Stat/List/Delete/DeletePrefix but never Put.
// A Backend.Put's io.Reader argument is consumed as it is read; once the
// first attempt has read any bytes from it, the reader cannot be rewound
// for a second attempt without this package buffering the entire object
// first -- which would defeat the streaming design this package exists to
// provide (see backend.go: "no []byte in any signature"). A truncated
// retry-after-partial-read would silently upload a corrupt object with a
// plausible-looking success response. Any Put-level retry belongs at the
// transport layer instead (the AWS SDK's own retryer, or
// manager.Uploader's per-part retry), which retries individual wire
// requests, not the caller-supplied stream itself.
//
// # Known gaps between this issue and the CRD (G1-G5)
//
// These are real, verified mismatches between the storage design this
// package implements and api/v1alpha1's current field set. Closing any of
// them requires an API change, which is explicitly out of scope for this
// issue -- they are documented here (and restated in the PR description)
// for a maintainer decision, not silently worked around.
//
//   - G1: S3Backend.CredentialsSecretRef is a single-key SecretKeyRef{Name,
//     Key}, but S3 credentials need two or three values (access key ID,
//     secret access key, optional session token). Workaround: this package
//     exports fixed data-key constants (SecretKeyAccessKeyID,
//     SecretKeySecretAccessKey, SecretKeySessionToken, see credentials.go)
//     that a future Secret-resolver reads from the Secret named by
//     CredentialsSecretRef.Name; CredentialsSecretRef.Key is ignored for S3.
//   - G2: the CRD has no TLS-skip-verify field. S3Config.InsecureSkipTLSVerify
//     exists in this package's Go API for future use; nothing populates it
//     from a CR today.
//   - G3: the CRD/status has no specHash field anywhere. SpecHash
//     (spechash.go) is provided as a pure helper, and Manifest.SpecHash is
//     always populated, but surfacing a spec hash on SandboxEnvironment's
//     status is a later issue's job.
//   - G4: the CRD has no resolved agent-image-digest field.
//     Manifest.AgentImage is required; Manifest.AgentImageDigest is
//     optional and never populated by this package itself -- resolving a
//     tag against a registry is out of scope here.
//   - G5: config/manager/manager.yaml declares no volumes/volumeMounts for
//     a PVC backend. FSConfig.Root simply takes an already-mounted absolute
//     path; wiring an actual PersistentVolumeClaim mount into the operator
//     Deployment is deploy-config work for a later issue.
package storage
