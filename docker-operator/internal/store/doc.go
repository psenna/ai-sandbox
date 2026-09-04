// Package store persists Agent records -- id, name, description, status, the
// names and IDs of the Docker resources belonging to the agent, and
// timestamps -- in a single BoltDB file, and enforces the MAX_AGENTS cap.
//
// The cap is enforced by Create alone: counting the agents that currently
// hold a slot and inserting the new record happen inside one bbolt
// read-write transaction. bbolt allows exactly one read-write transaction at
// a time, so that check-then-insert is atomic against every other Create in
// the process without any additional mutex, and bbolt's exclusive file lock
// keeps a second process from opening the same database at all. This is the
// capacity accounting the rest of the operator relies on -- the
// docker-operator's analog of the K8s operator's cluster-wide slot
// scheduler, minus the leader election a single-process operator does not
// need.
//
// This package is deliberately independent of internal/dockerclient: it
// stores the names and IDs of Docker objects as plain values and never
// consults a daemon. That independence is what lets the reconcile pass treat
// the store and the daemon as two sources of truth to cross-reference. It is
// also why this package defines its own ErrNotFound -- "no agent record with
// this ID" and "no such Docker object" are different facts, and a caller
// that conflated them would delete data.
//
// Records are JSON-encoded values in one flat "agents" bucket keyed by agent
// ID. MAX_AGENTS defaults to 5 and is never large, so a full scan is the
// cheapest possible index, JSON keeps the file inspectable with bbolt's own
// CLI, and a field added later decodes as its zero value in an older record.
package store
