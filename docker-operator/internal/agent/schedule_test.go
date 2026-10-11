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

func TestTemplateDue_NeverRun(t *testing.T) {
	tmpl := store.Template{
		Schedule:  "0 9 * * *",
		Enabled:   true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !TemplateDue(tmpl, nil, now) {
		t.Error("TemplateDue = false, want true (9am fired since CreatedAt at midnight, now is 10am)")
	}
	if TemplateDue(tmpl, nil, time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = true, want false (9am has not happened yet)")
	}
}

func TestTemplateDue_AnchorsOnLastRun(t *testing.T) {
	tmpl := store.Template{Schedule: "0 9 * * *", Enabled: true}
	lastRun := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	// The next 9am after the last run's 9am is the FOLLOWING day -- today's
	// 9am already fired and produced lastRun.
	if TemplateDue(tmpl, lastRun, time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = true, want false (already ran today)")
	}
	if !TemplateDue(tmpl, lastRun, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = false, want true (tomorrow's 9am has arrived)")
	}
}

// TestTemplateDue_AnchorsOnUpdatedAt pins Fix 5: a template created long
// ago whose schedule was just added (or that was just re-enabled) anchors
// on UpdatedAt, so it waits for its next real cron fire instead of firing
// on the very next tick off a stale CreatedAt.
func TestTemplateDue_AnchorsOnUpdatedAt(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	tmpl := store.Template{
		Schedule: "0 9 * * *", Enabled: true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: now.Add(-time.Minute), // schedule added just now, after today's 9am
	}
	if TemplateDue(tmpl, nil, now) {
		t.Error("TemplateDue = true, want false (next 9am after UpdatedAt is tomorrow)")
	}
	if !TemplateDue(tmpl, nil, time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = false, want true (tomorrow's 9am has arrived)")
	}
	// A last run older than UpdatedAt does not pull the anchor back.
	stale := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	if TemplateDue(tmpl, stale, now) {
		t.Error("TemplateDue = true with a stale lastRun, want false (UpdatedAt is newer)")
	}
}

func TestTemplateDue_DisabledNeverFires(t *testing.T) {
	tmpl := store.Template{Schedule: "0 9 * * *", Enabled: false}
	if TemplateDue(tmpl, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue(disabled) = true, want false regardless of how far in the future now is")
	}
}

func TestTemplateDue_NoScheduleNeverFires(t *testing.T) {
	tmpl := store.Template{Schedule: "", Enabled: true}
	if TemplateDue(tmpl, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue(no schedule) = true, want false")
	}
}
