// Package agent is the docker-operator's reconciler: the Create, Delete and
// Reconcile flows that turn a store record into the concrete set of Docker
// containers, volumes and networks that make up one agent, and that
// conservatively reconciles drift and orphans back to the record.
//
// This mirrors the Kubernetes operator's observe -> decide -> act -> status
// shape at much smaller scale, with internal/store standing in for the API
// server as the source of desired state.
//
// Create (#65), Delete (#66) and the tmux-boot.sh Cmd wiring (#69) are
// implemented: Manager.Create reserves a MAX_AGENTS slot, names all six
// per-agent Docker resources up front, ensures both images are present,
// builds the volumes/dinernet/DinD sidecar/agent container in dependency
// order and confirms the tmux session before marking the record running, with
// automatic rollback on any failure; Manager.Delete and the shared teardown
// routine reverse that (containers, then the network, then the volumes) and
// are idempotent; Manager.Reconcile is the startup pass that tears down
// records stuck mid-create/delete and reports -- without touching -- any
// managed-labelled Docker resource no store record claims.
package agent
