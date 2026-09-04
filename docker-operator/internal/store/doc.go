// Package store persists Agent records (id, name, description, status and
// the names of the Docker resources belonging to the agent) in BoltDB,
// including the atomic reserve-under-capacity Create that enforces
// MAX_AGENTS without a cluster-wide scheduler.
//
// Scaffold only (issue #61); the schema, CRUD and the capacity reservation
// land in task 4.
package store
