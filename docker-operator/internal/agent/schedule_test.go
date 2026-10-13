package agent

import (
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestParseSchedule_Valid(t *testing.T) {
	sched, err := ParseSchedule("0 9 * * *")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	anchor := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	next := sched.Next(anchor)
	want := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", anchor, next, want)
	}
}

func TestParseSchedule_Invalid(t *testing.T) {
	if _, err := ParseSchedule("not a cron expression"); err == nil {
		t.Error("ParseSchedule(garbage) = nil error, want non-nil")
	}
	if _, err := ParseSchedule("* * * * * *"); err == nil {
		t.Error("ParseSchedule(6 fields, with seconds) = nil error, want non-nil (standard parser is 5 fields)")
	}
}

func TestCronAgentDue_NeverRun(t *testing.T) {
	ca := store.CronAgent{
		Schedule:  "0 9 * * *",
		Enabled:   true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !CronAgentDue(ca, nil, now) {
		t.Error("CronAgentDue = false, want true (9am fired since CreatedAt at midnight, now is 10am)")
	}
	if CronAgentDue(ca, nil, time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = true, want false (9am has not happened yet)")
	}
}

func TestCronAgentDue_AnchorsOnLastRun(t *testing.T) {
	ca := store.CronAgent{Schedule: "0 9 * * *", Enabled: true}
	lastRun := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	if CronAgentDue(ca, lastRun, time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = true, want false (already ran today)")
	}
	if !CronAgentDue(ca, lastRun, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = false, want true (tomorrow's 9am has arrived)")
	}
}

func TestCronAgentDue_AnchorsOnUpdatedAt(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	ca := store.CronAgent{
		Schedule: "0 9 * * *", Enabled: true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: now.Add(-time.Minute),
	}
	if CronAgentDue(ca, nil, now) {
		t.Error("CronAgentDue = true, want false (next 9am after UpdatedAt is tomorrow)")
	}
	if !CronAgentDue(ca, nil, time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = false, want true (tomorrow's 9am has arrived)")
	}
	stale := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	if CronAgentDue(ca, stale, now) {
		t.Error("CronAgentDue = true with a stale lastRun, want false (UpdatedAt is newer)")
	}
}

func TestCronAgentDue_DisabledNeverFires(t *testing.T) {
	ca := store.CronAgent{Schedule: "0 9 * * *", Enabled: false}
	if CronAgentDue(ca, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue(disabled) = true, want false regardless of how far in the future now is")
	}
}

func TestCronAgentDue_NoScheduleNeverFires(t *testing.T) {
	ca := store.CronAgent{Schedule: "", Enabled: true}
	if CronAgentDue(ca, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue(no schedule) = true, want false")
	}
}
