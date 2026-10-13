package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// ErrCronAgentNotFound reports that no CronAgent with the given ID exists.
// GetCronAgent and UpdateCronAgent wrap it; callers test with
// IsCronAgentNotFound.
var ErrCronAgentNotFound = errors.New("cron agent not found")

// IsCronAgentNotFound reports whether err was caused by a missing CronAgent
// record.
func IsCronAgentNotFound(err error) bool { return errors.Is(err, ErrCronAgentNotFound) }

const (
	cronAgentIDPrefix = "ca_"
	cronAgentIDBytes  = 4
)

// bucketCronAgents holds every saved recurring-job definition, keyed by
// CronAgent ID. Separate from bucketTemplates: the same Template can back
// any number of differently-scheduled CronAgents.
var bucketCronAgents = []byte("cron_agents")

// CronAgent is a recurring job definition: "run this Template's config on
// this schedule, with this task, cleaned up this way." It references a
// Template by ID rather than embedding that config, so one Template can
// back any number of differently-scheduled CronAgents.
type CronAgent struct {
	// ID is the primary key. Immutable: UpdateCronAgent rejects a mutator
	// that changes it.
	ID string `json:"id"`
	// Name identifies this CronAgent in the Cron Agents list AND becomes
	// store.Agent.Name on every agent CreateFromCronAgent creates for it --
	// today a template-created agent has no name at all, so this closes
	// that gap specifically for scheduled agents.
	Name string `json:"name"`
	// TemplateID references the Template whose config (backend, model,
	// harness, ...) this job's agents are created from. DeleteTemplate
	// refuses (ErrTemplateInUse) while any CronAgent still references it.
	TemplateID string `json:"template_id"`
	// Schedule is a standard 5-field cron expression ("0 9 * * *"), parsed
	// with github.com/robfig/cron/v3's ParseStandard.
	Schedule string `json:"schedule"`
	// Enabled gates whether Schedule actually fires.
	Enabled bool `json:"enabled"`
	// Task is the prompt text passed to the harness's one-shot invocation
	// (`claude -p <Task>`) on each triggered run.
	Task string `json:"task"`
	// OnComplete decides what happens to a triggered run's agent once its
	// task finishes: "stop", "delete", or "leave".
	OnComplete string `json:"on_complete"`

	// CreatedAt is when CreateCronAgent inserted the record, in UTC.
	// Immutable.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the record last changed, in UTC. CreateCronAgent
	// sets it equal to CreatedAt, and every UpdateCronAgent stamps it.
	UpdatedAt time.Time `json:"updated_at"`
}

// CronAgentCreateSpec is everything a caller supplies to CreateCronAgent
// (and, as a full-record replacement, to UpdateCronAgent).
type CronAgentCreateSpec struct {
	// ID is required and must be unique. Generate it with NewCronAgentID.
	ID         string
	Name       string
	TemplateID string
	Schedule   string
	Enabled    bool
	Task       string
	OnComplete string
}

// NewCronAgentID returns a fresh CronAgent ID of the form "ca_7f3a9c2d",
// mirroring NewTemplateID/NewID.
func NewCronAgentID() (string, error) {
	var b [cronAgentIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a cron agent id: %w", err)
	}
	return cronAgentIDPrefix + hex.EncodeToString(b[:]), nil
}

// CreateCronAgent inserts a new CronAgent record. It rejects a blank ID and
// a duplicate ID (ErrCronAgentExists). Unlike Template, a CronAgent's Name
// has no uniqueness constraint -- it is a list label, not a selection key.
var ErrCronAgentExists = errors.New("cron agent already exists")

// IsCronAgentExists reports whether err was caused by a duplicate CronAgent
// ID.
func IsCronAgentExists(err error) bool { return errors.Is(err, ErrCronAgentExists) }

func (s *Store) CreateCronAgent(ctx context.Context, spec CronAgentCreateSpec) (CronAgent, error) {
	if spec.ID == "" {
		return CronAgent{}, errors.New("creating a cron agent: the ID must not be empty, generate one with NewCronAgentID")
	}

	now := s.now()
	ca := CronAgent{
		ID:         spec.ID,
		Name:       spec.Name,
		TemplateID: spec.TemplateID,
		Schedule:   spec.Schedule,
		Enabled:    spec.Enabled,
		Task:       spec.Task,
		OnComplete: spec.OnComplete,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	err := s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		if b.Get([]byte(spec.ID)) != nil {
			return fmt.Errorf("creating cron agent %q: %w", spec.ID, ErrCronAgentExists)
		}
		return putCronAgent(b, ca)
	})
	if err != nil {
		return CronAgent{}, err
	}
	return ca, nil
}

// GetCronAgent returns the CronAgent with the given ID, or an error
// satisfying IsCronAgentNotFound.
func (s *Store) GetCronAgent(ctx context.Context, id string) (CronAgent, error) {
	var ca CronAgent
	err := s.viewCronAgents(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting cron agent %q: %w", id, ErrCronAgentNotFound)
		}
		return decodeCronAgent(id, raw, &ca)
	})
	if err != nil {
		return CronAgent{}, err
	}
	return ca, nil
}

// ListCronAgents returns every CronAgent, sorted by ID because bbolt
// iterates keys in byte order (the web UI sorts by Name for display
// itself). The slice is never nil.
func (s *Store) ListCronAgents(ctx context.Context) ([]CronAgent, error) {
	cronAgents := make([]CronAgent, 0)
	err := s.viewCronAgents(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var ca CronAgent
			if err := decodeCronAgent(string(k), v, &ca); err != nil {
				return err
			}
			cronAgents = append(cronAgents, ca)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing cron agents: %w", err)
	}
	return cronAgents, nil
}

// UpdateCronAgent applies mutate to the CronAgent with the given ID and
// writes the whole record back, mirroring UpdateTemplate's mutator
// pattern. ID and CreatedAt are immutable. UpdatedAt is stamped by this
// method after the mutator runs.
func (s *Store) UpdateCronAgent(ctx context.Context, id string, mutate func(*CronAgent) error) (CronAgent, error) {
	if mutate == nil {
		return CronAgent{}, fmt.Errorf("updating cron agent %q: the mutator must not be nil", id)
	}

	var updated CronAgent
	err := s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("updating cron agent %q: %w", id, ErrCronAgentNotFound)
		}
		var ca CronAgent
		if err := decodeCronAgent(id, raw, &ca); err != nil {
			return err
		}

		before := ca
		if err := mutate(&ca); err != nil {
			return fmt.Errorf("updating cron agent %q: %w", id, err)
		}
		if ca.ID != before.ID {
			return fmt.Errorf("updating cron agent %q: the mutator changed the ID to %q; it is immutable", id, ca.ID)
		}
		if !ca.CreatedAt.Equal(before.CreatedAt) {
			return fmt.Errorf("updating cron agent %q: the mutator changed CreatedAt; it is immutable", id)
		}

		ca.UpdatedAt = s.now()
		if err := putCronAgent(b, ca); err != nil {
			return err
		}
		updated = ca
		return nil
	})
	if err != nil {
		return CronAgent{}, err
	}
	return updated, nil
}

// DeleteCronAgent removes the CronAgent record. Deleting one that is not
// there is success, matching Delete's idempotent stance for an agent. Its
// ScheduleRun history survives (ScheduleRun lives in its own bucket, same
// reasoning as a Template's deletion not cascading its runs).
func (s *Store) DeleteCronAgent(ctx context.Context, id string) error {
	return s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("deleting cron agent %q: %w", id, err)
		}
		return nil
	})
}

func (s *Store) viewCronAgents(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		b, err := s.cronAgentsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

func (s *Store) updateCronAgents(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := s.cronAgentsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

// cronAgentsBucket returns the cron_agents bucket. Open creates it, so a
// nil here means the file was truncated or was never this program's
// database.
func (s *Store) cronAgentsBucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	b := tx.Bucket(bucketCronAgents)
	if b == nil {
		return nil, fmt.Errorf("the %q bucket is missing from the state database %q", bucketCronAgents, s.path)
	}
	return b, nil
}

func putCronAgent(b *bbolt.Bucket, ca CronAgent) error {
	raw, err := json.Marshal(ca)
	if err != nil {
		return fmt.Errorf("encoding cron agent %q: %w", ca.ID, err)
	}
	if err := b.Put([]byte(ca.ID), raw); err != nil {
		return fmt.Errorf("writing cron agent %q: %w", ca.ID, err)
	}
	return nil
}

func decodeCronAgent(id string, raw []byte, ca *CronAgent) error {
	if err := json.Unmarshal(raw, ca); err != nil {
		return fmt.Errorf("decoding cron agent %q from the state database: %w", id, err)
	}
	return nil
}
