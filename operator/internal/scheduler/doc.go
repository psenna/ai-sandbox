// Package scheduler decides which queued SandboxEnvironments are admitted
// to an execution slot, as a pure function of a snapshot of environments
// and a fixed capacity (see Admit). It never performs I/O and never reads
// the clock -- GrantedAt is stamped by the caller (internal/controller's
// SlotScheduler), which also owns the actual List/write against the API
// server. This package (including its tests) MUST import only "sort",
// "k8s.io/apimachinery/pkg/types", "k8s.io/apimachinery/pkg/apis/meta/v1",
// and github.com/psenna/ai-sandbox/operator/api/v1alpha1. It must NEVER
// import sigs.k8s.io/controller-runtime or any other client-runtime
// package.
//
// This package only ever GRANTS slots. Release is entirely
// internal/lifecycle.Apply's job (SlotWanted=false zeroes status.slot) --
// duplicating that logic here would create two sources of truth for the
// same state.
package scheduler
