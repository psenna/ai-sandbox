package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

// ErrScheduleRunNotFound reports that no ScheduleRun with the given ID
// exists. GetScheduleRun and FinishScheduleRun wrap it; callers test with
// IsScheduleRunNotFound.
var ErrScheduleRunNotFound = errors.New("schedule run not found")

// IsScheduleRunNotFound reports whether err was caused by a missing
// ScheduleRun record.
func IsScheduleRunNotFound(err error) bool { return errors.Is(err, ErrScheduleRunNotFound) }

const (
	scheduleRunIDPrefix = "run_"
	scheduleRunIDBytes  = 4
)

// ScheduleRunStatusRunning/Success/Error are the only three valid values
// for ScheduleRun.Status.
const (
	ScheduleRunStatusRunning = "running"
	ScheduleRunStatusSuccess = "success"
	ScheduleRunStatusError   = "error"
)

// bucketScheduleRuns holds one record per triggered scheduled run, keyed by
// ScheduleRun ID. Deliberately a separate bucket from bucketTemplates: a
// run's history outlives both the agent it created (which may be deleted,
// per the template's own OnComplete policy) and the template itself (which
// a human may delete later) -- see ScheduleRun's own doc comment.
var bucketScheduleRuns = []byte("schedule_runs")

// ScheduleRun is a durable record of one triggered run of a scheduled
// Template, independent of both the Template's and the Agent's own
// lifecycle: querying live store.Agent records cannot answer "what ran
// under this schedule last week" once an agent has been deleted (the
// common case for OnComplete: "delete"), and deleting the Template itself
// must not take this history down with it.
type ScheduleRun struct {
	ID         string `json:"id"`
	TemplateID string `json:"template_id"`
	AgentID    string `json:"agent_id"`
	// StartedAt is when CreateScheduleRun ran, i.e. when the trigger fired.
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is nil while Status is ScheduleRunStatusRunning; set by
	// FinishScheduleRun.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Status: ScheduleRunStatusRunning | ScheduleRunStatusSuccess |
	// ScheduleRunStatusError.
	Status string `json:"status"`
}

// NewScheduleRunID returns a fresh ID of the form "run_7f3a9c2d", mirroring
// NewTemplateID/NewID.
func NewScheduleRunID() (string, error) {
	var b [scheduleRunIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a schedule run id: %w", err)
	}
	return scheduleRunIDPrefix + hex.EncodeToString(b[:]), nil
}

// CreateScheduleRun inserts a new ScheduleRun record with Status
// ScheduleRunStatusRunning and StartedAt now, for templateID's trigger that
// just created agentID.
func (s *Store) CreateScheduleRun(ctx context.Context, templateID, agentID string) (ScheduleRun, error) {
	id, err := NewScheduleRunID()
	if err != nil {
		return ScheduleRun{}, fmt.Errorf("creating a schedule run: %w", err)
	}
	run := ScheduleRun{
		ID:         id,
		TemplateID: templateID,
		AgentID:    agentID,
		StartedAt:  s.now(),
		Status:     ScheduleRunStatusRunning,
	}
	if err := s.updateScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		return putScheduleRun(b, run)
	}); err != nil {
		return ScheduleRun{}, err
	}
	return run, nil
}

// FinishScheduleRun sets the run's Status and FinishedAt (to now), or
// returns an error satisfying IsScheduleRunNotFound. Deliberately not a
// generic mutator like UpdateTemplate's: a run has exactly one transition
// (running -> finished), so a direct two-field write is simpler and
// carries no risk of a caller forgetting to preserve an unrelated field.
func (s *Store) FinishScheduleRun(ctx context.Context, id, status string) (ScheduleRun, error) {
	var updated ScheduleRun
	err := s.updateScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("finishing schedule run %q: %w", id, ErrScheduleRunNotFound)
		}
		var run ScheduleRun
		if err := decodeScheduleRun(id, raw, &run); err != nil {
			return err
		}
		now := s.now()
		run.FinishedAt = &now
		run.Status = status
		if err := putScheduleRun(b, run); err != nil {
			return err
		}
		updated = run
		return nil
	})
	if err != nil {
		return ScheduleRun{}, err
	}
	return updated, nil
}

// GetScheduleRun returns the run with the given ID, or an error satisfying
// IsScheduleRunNotFound.
func (s *Store) GetScheduleRun(ctx context.Context, id string) (ScheduleRun, error) {
	var run ScheduleRun
	err := s.viewScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting schedule run %q: %w", id, ErrScheduleRunNotFound)
		}
		return decodeScheduleRun(id, raw, &run)
	})
	if err != nil {
		return ScheduleRun{}, err
	}
	return run, nil
}

// ListScheduleRunsByTemplate returns every run for templateID, newest
// StartedAt first -- the order both the UI's run-history list and the
// ticker's "anchor on the most recent run" lookup want.
func (s *Store) ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]ScheduleRun, error) {
	runs := make([]ScheduleRun, 0)
	err := s.viewScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var run ScheduleRun
			if err := decodeScheduleRun(string(k), v, &run); err != nil {
				return err
			}
			if run.TemplateID == templateID {
				runs = append(runs, run)
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing schedule runs for template %q: %w", templateID, err)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

// ListScheduleRunsByStatus returns every run with the given Status, in no
// particular order -- the ticker's completion check iterates every
// ScheduleRunStatusRunning record regardless of which template it belongs
// to.
func (s *Store) ListScheduleRunsByStatus(ctx context.Context, status string) ([]ScheduleRun, error) {
	runs := make([]ScheduleRun, 0)
	err := s.viewScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var run ScheduleRun
			if err := decodeScheduleRun(string(k), v, &run); err != nil {
				return err
			}
			if run.Status == status {
				runs = append(runs, run)
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing schedule runs with status %q: %w", status, err)
	}
	return runs, nil
}

func (s *Store) viewScheduleRuns(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		b, err := s.scheduleRunsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

func (s *Store) updateScheduleRuns(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := s.scheduleRunsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

func (s *Store) scheduleRunsBucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	b := tx.Bucket(bucketScheduleRuns)
	if b == nil {
		return nil, fmt.Errorf("the %q bucket is missing from the state database %q", bucketScheduleRuns, s.path)
	}
	return b, nil
}

func putScheduleRun(b *bbolt.Bucket, run ScheduleRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("encoding schedule run %q: %w", run.ID, err)
	}
	if err := b.Put([]byte(run.ID), raw); err != nil {
		return fmt.Errorf("writing schedule run %q: %w", run.ID, err)
	}
	return nil
}

func decodeScheduleRun(id string, raw []byte, run *ScheduleRun) error {
	if err := json.Unmarshal(raw, run); err != nil {
		return fmt.Errorf("decoding schedule run %q from the state database: %w", id, err)
	}
	return nil
}
