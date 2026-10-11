package store

import (
	"context"
	"testing"
)

func TestScheduleRun_CreateAndFinish(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	run, err := st.CreateScheduleRun(ctx, "ca_1", "agt_1")
	if err != nil {
		t.Fatalf("CreateScheduleRun: %v", err)
	}
	if run.CronAgentID != "ca_1" || run.AgentID != "agt_1" || run.Status != ScheduleRunStatusRunning {
		t.Errorf("ScheduleRun = %+v, want CronAgentID/AgentID set and Status running", run)
	}
	if run.StartedAt.IsZero() {
		t.Error("StartedAt is zero, want the time CreateScheduleRun ran")
	}
	if run.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil before finishing", run.FinishedAt)
	}

	finished, err := st.FinishScheduleRun(ctx, run.ID, ScheduleRunStatusSuccess)
	if err != nil {
		t.Fatalf("FinishScheduleRun: %v", err)
	}
	if finished.Status != ScheduleRunStatusSuccess {
		t.Errorf("Status = %q, want %q", finished.Status, ScheduleRunStatusSuccess)
	}
	if finished.FinishedAt == nil || finished.FinishedAt.IsZero() {
		t.Error("FinishedAt not set after FinishScheduleRun")
	}

	got, err := st.GetScheduleRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetScheduleRun: %v", err)
	}
	// FinishedAt is a pointer, so compare the struct field by field rather
	// than with ==, which would compare pointer identity.
	if got.ID != finished.ID || got.CronAgentID != finished.CronAgentID ||
		got.AgentID != finished.AgentID || got.Status != finished.Status ||
		!got.StartedAt.Equal(finished.StartedAt) ||
		got.FinishedAt == nil || !got.FinishedAt.Equal(*finished.FinishedAt) {
		t.Errorf("GetScheduleRun = %+v, want %+v", got, finished)
	}
}

func TestScheduleRun_FinishMissingIsNotFound(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	_, err := st.FinishScheduleRun(ctx, "run_missing", ScheduleRunStatusError)
	if !IsScheduleRunNotFound(err) {
		t.Errorf("FinishScheduleRun(missing) = %v, want IsScheduleRunNotFound", err)
	}
}

// TestScheduleRun_ListByCronAgentNewestFirst backs the UI's run-history list
// and the ticker's "anchor on the most recent run" lookup.
func TestScheduleRun_ListByCronAgentNewestFirst(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	first, err := st.CreateScheduleRun(ctx, "ca_a", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScheduleRun(ctx, "ca_b", "agt_2"); err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateScheduleRun(ctx, "ca_a", "agt_3")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.ListScheduleRunsByCronAgent(ctx, "ca_a")
	if err != nil {
		t.Fatalf("ListScheduleRunsByCronAgent: %v", err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Errorf("ListScheduleRunsByCronAgent(ca_a) = %+v, want [second, first] newest-first", got)
	}
}

func TestScheduleRun_ListByStatus(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	running, err := st.CreateScheduleRun(ctx, "ca_a", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	finishedOne, err := st.CreateScheduleRun(ctx, "ca_a", "agt_2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishScheduleRun(ctx, finishedOne.ID, ScheduleRunStatusSuccess); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListScheduleRunsByStatus(ctx, ScheduleRunStatusRunning)
	if err != nil {
		t.Fatalf("ListScheduleRunsByStatus: %v", err)
	}
	if len(got) != 1 || got[0].ID != running.ID {
		t.Errorf("ListScheduleRunsByStatus(running) = %+v, want only %q", got, running.ID)
	}
}

// TestDeleteTemplate_DoesNotCascadeScheduleRuns pins the Review Focus
// item: a ScheduleRun's history is independent of both the CronAgent and
// the Template lifecycle -- deleting a Template with no CronAgent
// referencing it must not touch ScheduleRun records that happen to exist.
func TestDeleteTemplate_DoesNotCascadeScheduleRuns(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	tmpl, err := st.CreateTemplate(ctx, TemplateCreateSpec{ID: "tpl_doomed", Name: "doomed"})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	run, err := st.CreateScheduleRun(ctx, "ca_unrelated", "agt_1")
	if err != nil {
		t.Fatalf("CreateScheduleRun: %v", err)
	}

	if err := st.DeleteTemplate(ctx, tmpl.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	got, err := st.GetScheduleRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetScheduleRun after DeleteTemplate: %v, want the run to survive", err)
	}
	if got.CronAgentID != "ca_unrelated" {
		t.Errorf("surviving run's CronAgentID = %q, want %q", got.CronAgentID, "ca_unrelated")
	}
}
