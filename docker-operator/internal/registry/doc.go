// Package registry is a minimal read-only client for the Distribution
// (Docker Registry HTTP API v2) tag-list endpoint, used by the operator to
// discover the date-time tags published for its agent image.
//
// It implements exactly one operation -- list every tag of one repository --
// against a GHCR-style v2 API: the anonymous Bearer-token challenge dance,
// Link-header pagination, and an error classification (ErrOffline, ErrAuth,
// ErrRepoNotFound) the caller can branch on. It never pulls a manifest, never
// writes, and never logs a credential.
package registry
