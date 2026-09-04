// Package agent is the docker-operator's reconciler: the Create, Delete and
// Reconcile flows that turn a store record into the concrete set of Docker
// containers, volumes and networks that make up one agent, and that
// conservatively reconciles drift and orphans back to the record.
//
// This mirrors the Kubernetes operator's observe -> decide -> act -> status
// shape at much smaller scale, with internal/store standing in for the API
// server as the source of desired state.
//
// Scaffold only (issue #61); the Create flow lands in task 5 and the
// Delete/reconcile flow in task 6.
package agent
