package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// seedRunningAgent inserts a StatusRunning record with the given container ID
// stamped, without going through the create flow.
func seedRunningAgent(t *testing.T, m *Manager, containerID string) store.Agent {
	t.Helper()
	ctx := context.Background()
	a, err := m.store.Create(ctx, store.CreateSpec{ID: "agt_running"})
	if err != nil {
		t.Fatalf("seeding a record: %v", err)
	}
	a, err = m.store.Update(ctx, a.ID, func(ag *store.Agent) error {
		ag.ContainerID = containerID
		ag.Status = store.StatusRunning
		return nil
	})
	if err != nil {
		t.Fatalf("stamping the record: %v", err)
	}
	return a
}

// TestMarkUnexpectedExit_IgnoresMismatchedContainerID proves the events race
// guard: a die/stop event whose actor is not the record's current container
// (the OLD container an in-place update just recreated away) is a no-op.
func TestMarkUnexpectedExit_IgnoresMismatchedContainerID(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := seedRunningAgent(t, m, "new-container-id")

	if err := m.MarkUnexpectedExit(ctx, a.ID, "old-container-id", store.StatusError, "container die unexpectedly"); err != nil {
		t.Fatalf("MarkUnexpectedExit: %v", err)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q (a mismatched-container event must not flip the record)", got.Status, store.StatusRunning)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", got.ErrorMessage)
	}
}

// TestMarkUnexpectedExit_MatchingContainerIDStillWorks proves the guard does
// not break the normal case: a die event on the record's current container
// still flips it to the new status.
func TestMarkUnexpectedExit_MatchingContainerIDStillWorks(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := seedRunningAgent(t, m, "the-container-id")

	if err := m.MarkUnexpectedExit(ctx, a.ID, "the-container-id", store.StatusError, "container die unexpectedly"); err != nil {
		t.Fatalf("MarkUnexpectedExit: %v", err)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusError)
	}
	if got.ErrorMessage != "container die unexpectedly" {
		t.Errorf("ErrorMessage = %q, want the event message", got.ErrorMessage)
	}
}

// TestMarkUnexpectedExit_EmptyContainerIDDisablesGuard proves an event source
// that names no actor still corrects the status (the guard needs both IDs).
func TestMarkUnexpectedExit_EmptyContainerIDDisablesGuard(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := seedRunningAgent(t, m, "the-container-id")

	if err := m.MarkUnexpectedExit(ctx, a.ID, "", store.StatusStopped, "container stop unexpectedly"); err != nil {
		t.Fatalf("MarkUnexpectedExit: %v", err)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusStopped)
	}
}
