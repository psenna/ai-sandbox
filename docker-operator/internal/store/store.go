package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// ErrNotFound reports that no agent record with the given ID exists. Get and
// Update wrap it; callers test with IsNotFound.
//
// Delete deliberately does NOT return it: removing a record that is already
// gone is success, matching dockerclient's remove-is-idempotent convention
// and letting the delete flow double as create-failure rollback.
var ErrNotFound = errors.New("agent not found")

// ErrAtCapacity reports that MAX_AGENTS agents already hold a slot, so
// Create refused to reserve another. internal/api turns this into a 409:
// there is no queue, a create over the cap fails immediately.
var ErrAtCapacity = errors.New("at agent capacity")

// ErrExists reports that Create was given an ID that is already in the
// store. It is also the guard against an (astronomically unlikely) NewID
// collision, which a caller may retry with a fresh ID.
var ErrExists = errors.New("agent already exists")

// IsNotFound reports whether err was caused by a missing agent record.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsAtCapacity reports whether err was caused by the MAX_AGENTS cap.
func IsAtCapacity(err error) bool { return errors.Is(err, ErrAtCapacity) }

// IsExists reports whether err was caused by a duplicate agent ID.
func IsExists(err error) bool { return errors.Is(err, ErrExists) }

// Status is an agent's lifecycle state as the operator understands it. It is
// the operator's own state machine, not the Docker container state: an agent
// whose container is running but whose tmux session never appeared is
// StatusError, and a StatusDeleting agent may still have every one of its
// containers alive.
type Status string

// The agent lifecycle states.
const (
	// StatusCreating is the state Create inserts. The agent holds a slot and
	// its Docker resources are being built.
	StatusCreating Status = "creating"
	// StatusRunning means every resource exists and the tmux session
	// answered.
	StatusRunning Status = "running"
	// StatusStopped means the containers exist but are not running, e.g.
	// after the daemon or the host restarted.
	StatusStopped Status = "stopped"
	// StatusError means the create flow failed or a container died
	// unexpectedly; ErrorMessage says why.
	StatusError Status = "error"
	// StatusDeleting means teardown has begun. It is set first, before any
	// resource is touched, which is what frees the slot immediately.
	StatusDeleting Status = "deleting"
)

// Valid reports whether s is one of the defined states. Update rejects a
// mutator that leaves an agent in any other state, so a typo cannot quietly
// make a record invisible to the capacity count.
func (s Status) Valid() bool {
	switch s {
	case StatusCreating, StatusRunning, StatusStopped, StatusError, StatusDeleting:
		return true
	default:
		return false
	}
}

// CountsTowardCapacity reports whether an agent in this state occupies one
// of the MAX_AGENTS slots.
//
// Only creating and running do. Deleting is excluded on purpose, so a delete
// frees the slot the moment it is marked rather than after the slow Docker
// teardown. Stopped and error are excluded because an agent in either state
// is waiting on a human to delete or retry it and should not hold the host's
// last slot hostage; note this means the cap bounds *live* agents, not the
// volumes a stopped agent still occupies on disk.
func (s Status) CountsTowardCapacity() bool {
	return s == StatusCreating || s == StatusRunning
}

// Agent is one persisted agent record.
//
// Every Docker field starts empty: Create inserts the record BEFORE any
// resource exists, so that a crash mid-create leaves proof of the attempt
// for the reconcile pass, and the create flow stamps each name and ID in
// with an Update as it goes.
type Agent struct {
	// ID is the primary key and the infix of every Docker resource name.
	// Immutable: Update rejects a mutator that changes it.
	ID string `json:"id"`
	// Name is the human label shown in the UI. Editable via PATCH, may be
	// empty, and carries no uniqueness constraint.
	Name string `json:"name"`
	// Description is free-form text shown in the UI. Editable via PATCH.
	Description string `json:"description"`
	// Status is the lifecycle state; see Status.
	Status Status `json:"status"`
	// ErrorMessage explains a StatusError agent. Empty in every other state.
	ErrorMessage string `json:"error_message,omitempty"`

	// ContainerName is the agent container's name
	// (docker-operator-agent-<id>). It is derivable from the ID, but it is
	// recorded so teardown never depends on the naming convention still
	// matching what created the resource.
	ContainerName string `json:"container_name,omitempty"`
	// ContainerID is the agent container's daemon-assigned ID, set once it
	// has been created.
	ContainerID string `json:"container_id,omitempty"`
	// DindContainerName is the Docker-in-Docker sidecar's name
	// (docker-operator-dind-<id>).
	DindContainerName string `json:"dind_container_name,omitempty"`
	// DindContainerID is the sidecar's daemon-assigned ID.
	DindContainerID string `json:"dind_container_id,omitempty"`
	// DinernetName is the agent's private network
	// (docker-operator-agent-<id>-dinernet).
	DinernetName string `json:"dinernet_name,omitempty"`
	// DinernetID is that network's daemon-assigned ID.
	DinernetID string `json:"dinernet_id,omitempty"`

	// WorkspaceVolume, ClaudeConfigVolume and DindCacheVolume are the three
	// per-agent named volumes. A volume has no separate daemon ID -- its
	// name is its identity -- so there is no ...VolumeID counterpart.
	WorkspaceVolume    string `json:"workspace_volume,omitempty"`
	ClaudeConfigVolume string `json:"claude_config_volume,omitempty"`
	DindCacheVolume    string `json:"dind_cache_volume,omitempty"`

	// DependaproxyDinernetIP is the address IPAM gave the shared
	// dependaproxy container on this agent's dinernet, read back at create
	// time and templated into the agent container as
	// DEPENDAPROXY_DINERNET_IP. The zero Addr means "not connected yet"; it
	// round-trips through JSON as an empty string.
	DependaproxyDinernetIP netip.Addr `json:"dependaproxy_dinernet_ip"`

	// CreatedAt is when Create inserted the record, in UTC. Immutable.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the record last changed, in UTC. Create sets it
	// equal to CreatedAt, and every Update stamps it: a mutator can neither
	// forget it nor forge it.
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateSpec is everything a caller supplies to Create. It is a struct
// rather than three string parameters so that no call site can transpose the
// name and the description silently.
type CreateSpec struct {
	// ID is required and must be unique. Generate it with NewID; taking it
	// from the caller keeps IDs deterministic in tests and lets the create
	// flow log and name its Docker resources before it touches the store.
	ID string
	// Name is the initial human label. May be empty.
	Name string
	// Description is the initial free-form description. May be empty.
	Description string
}

// bucketAgents is the single bucket holding every record, keyed by agent ID.
var bucketAgents = []byte("agents")

const (
	// dbFileMode is the mode Open creates the database file with. It is the
	// operator's private state; nothing else has any business reading it.
	dbFileMode = 0o600
	// dbDirMode is the mode Open creates missing parent directories with.
	dbDirMode = 0o750

	// idPrefix marks a value as an agent ID wherever it turns up out of
	// context -- a container name, a log line, a URL path.
	idPrefix = "agt_"
	// idBytes is how much randomness NewID draws. Four bytes (eight hex
	// characters) keeps an ID short enough to type and to embed in Docker
	// resource names; against MAX_AGENTS live records a collision is
	// negligible, and Create's ErrExists catches one if it ever happens.
	idBytes = 4
)

// openTimeout bounds how long Open waits for bbolt's exclusive file lock
// before giving up, so a second operator process started against the same
// state file fails with a legible error instead of hanging forever. It is a
// var only so the tests can shorten it.
var openTimeout = 5 * time.Second

// Store is the BoltDB-backed agent record store. It is safe for concurrent
// use: every method runs inside a bbolt transaction, and bbolt serialises
// writers itself.
type Store struct {
	db   *bbolt.DB
	path string

	// maxAgents is the MAX_AGENTS cap, fixed for the store's lifetime.
	//
	// It is a construction parameter rather than a Create argument
	// deliberately: the cap is a process-wide invariant sourced from
	// config.Config.MaxAgents, and a per-call parameter would let two call
	// sites disagree about it and quietly break the one guarantee this
	// package exists to provide.
	maxAgents int

	// now is the clock, injectable so tests get deterministic timestamps.
	// It returns UTC times, which also strips the monotonic reading that
	// would otherwise survive in memory but not through JSON.
	now func() time.Time
}

// Open opens -- creating it if needed -- the BoltDB file at path and returns
// a Store enforcing a cap of maxAgents concurrently live agents.
//
// Missing parent directories are created, as internal/config's StateDBPath
// documentation promises: the default path lives under /var/lib and the
// operator normally starts with an empty named volume mounted there.
//
// Open takes bbolt's exclusive file lock and holds it until Close, so a
// second process pointed at the same file fails instead of corrupting it.
func Open(path string, maxAgents int) (*Store, error) {
	if path == "" {
		return nil, errors.New("opening the state database: the path must not be empty")
	}
	if maxAgents < 1 {
		return nil, fmt.Errorf("opening the state database %q: max agents must be >= 1, got %d", path, maxAgents)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dbDirMode); err != nil {
			return nil, fmt.Errorf("creating the state directory %q: %w", dir, err)
		}
	}

	db, err := bbolt.Open(path, dbFileMode, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("opening the state database %q: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketAgents)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating the %q bucket in %q: %w", bucketAgents, path, err)
	}

	return &Store{
		db:        db,
		path:      path,
		maxAgents: maxAgents,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

// Close flushes the database and releases the file and its lock. It is
// idempotent.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing the state database %q: %w", s.path, err)
	}
	return nil
}

// MaxAgents returns the cap this store enforces, so the API can report
// "3 of 5 slots in use" without duplicating the configuration.
func (s *Store) MaxAgents() int { return s.maxAgents }

// NewID returns a fresh agent ID of the form "agt_7f3a9c2d".
//
// ID generation lives here, next to the key space whose uniqueness it
// protects, but Create still takes the ID as input: that keeps tests
// deterministic and lets internal/agent log and name resources before it
// touches the store. The error is whatever crypto/rand reports and in
// practice cannot happen; it is returned rather than swallowed.
func NewID() (string, error) {
	var b [idBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating an agent id: %w", err)
	}
	return idPrefix + hex.EncodeToString(b[:]), nil
}

// Create atomically reserves a slot under the MAX_AGENTS cap and inserts a
// StatusCreating record for spec.ID.
//
// The capacity count and the insert share one bbolt read-write transaction,
// and bbolt permits only one such transaction at a time, so N concurrent
// Creates against a cap of 1 produce exactly one success and N-1 errors
// satisfying IsAtCapacity -- no external mutex is involved or needed.
//
// The returned Agent has every Docker field empty; the create flow fills
// them in with Update as each resource comes up. A create that fails later
// must Delete the record, so that a failed attempt never permanently
// consumes a slot.
func (s *Store) Create(ctx context.Context, spec CreateSpec) (Agent, error) {
	if spec.ID == "" {
		return Agent{}, errors.New("creating an agent: the ID must not be empty, generate one with NewID")
	}

	now := s.now()
	agent := Agent{
		ID:          spec.ID,
		Name:        spec.Name,
		Description: spec.Description,
		Status:      StatusCreating,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	err := s.update(ctx, func(b *bbolt.Bucket) error {
		if b.Get([]byte(spec.ID)) != nil {
			return fmt.Errorf("creating agent %q: %w", spec.ID, ErrExists)
		}
		used, err := countReserved(b)
		if err != nil {
			return fmt.Errorf("creating agent %q: %w", spec.ID, err)
		}
		if used >= s.maxAgents {
			return fmt.Errorf("creating agent %q: %d of %d agent slots in use: %w",
				spec.ID, used, s.maxAgents, ErrAtCapacity)
		}
		return put(b, agent)
	})
	if err != nil {
		return Agent{}, err
	}
	return agent, nil
}

// Get returns the agent with the given ID, or an error satisfying
// IsNotFound.
func (s *Store) Get(ctx context.Context, id string) (Agent, error) {
	var agent Agent
	err := s.view(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting agent %q: %w", id, ErrNotFound)
		}
		return decode(id, raw, &agent)
	})
	if err != nil {
		return Agent{}, err
	}
	return agent, nil
}

// List returns every agent, sorted by ID because bbolt iterates keys in byte
// order. The slice is never nil, so an empty store encodes as a JSON [] and
// not null.
func (s *Store) List(ctx context.Context) ([]Agent, error) {
	agents := make([]Agent, 0)
	err := s.view(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var agent Agent
			if err := decode(string(k), v, &agent); err != nil {
				return err
			}
			agents = append(agents, agent)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	return agents, nil
}

// Update applies mutate to the agent with the given ID and writes the result
// back, all inside one read-write transaction, and returns the stored
// record.
//
// The mutator form is what makes read-modify-write safe here: a caller
// cannot read a record, lose a race and then write back a stale copy of the
// fields it never meant to touch, because no other writer can interleave
// between the read and the write. A mutator that returns an error rolls the
// whole transaction back and leaves the record untouched.
//
// ID and CreatedAt are immutable, and Status must stay a defined state; a
// mutator that violates any of those gets an error and changes nothing.
// UpdatedAt is stamped by this method after the mutator runs.
func (s *Store) Update(ctx context.Context, id string, mutate func(*Agent) error) (Agent, error) {
	if mutate == nil {
		return Agent{}, fmt.Errorf("updating agent %q: the mutator must not be nil", id)
	}

	var updated Agent
	err := s.update(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("updating agent %q: %w", id, ErrNotFound)
		}
		var agent Agent
		if err := decode(id, raw, &agent); err != nil {
			return err
		}

		before := agent
		if err := mutate(&agent); err != nil {
			return fmt.Errorf("updating agent %q: %w", id, err)
		}
		if err := checkInvariants(before, agent); err != nil {
			return fmt.Errorf("updating agent %q: %w", id, err)
		}

		agent.UpdatedAt = s.now()
		if err := put(b, agent); err != nil {
			return err
		}
		updated = agent
		return nil
	})
	if err != nil {
		return Agent{}, err
	}
	return updated, nil
}

// Delete removes the agent record. Deleting a record that is not there is
// success, which is what lets the delete flow run twice and lets it double
// as create-failure rollback.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.update(ctx, func(b *bbolt.Bucket) error {
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("deleting agent %q: %w", id, err)
		}
		return nil
	})
}

// view runs fn against the agents bucket inside a read-only transaction.
//
// bbolt has no notion of a context, so ctx is checked once before the
// transaction opens and not again inside it. That is honest for this
// workload: a transaction touches at most MAX_AGENTS small records and
// cannot block on anything but the page cache.
func (s *Store) view(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		b, err := s.bucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

// update runs fn against the agents bucket inside a read-write transaction.
// bbolt allows one of those at a time process-wide, which is the whole
// serialisation mechanism behind Create's capacity reservation.
func (s *Store) update(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := s.bucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

// bucket returns the agents bucket. Open creates it, so a nil here means the
// file was truncated or was never this program's database; saying so beats a
// nil-pointer panic.
func (s *Store) bucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	b := tx.Bucket(bucketAgents)
	if b == nil {
		return nil, fmt.Errorf("the %q bucket is missing from the state database %q", bucketAgents, s.path)
	}
	return b, nil
}

// countReserved returns how many records currently hold a slot.
//
// It decodes every record rather than consulting a status index: MAX_AGENTS
// is a handful, so the scan costs microseconds, and a secondary index would
// be one more thing that can silently disagree with the records themselves
// -- on exactly the code path where disagreement means over-admitting agents.
func countReserved(b *bbolt.Bucket) (int, error) {
	n := 0
	err := b.ForEach(func(k, v []byte) error {
		var agent Agent
		if err := decode(string(k), v, &agent); err != nil {
			return err
		}
		if agent.Status.CountsTowardCapacity() {
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// checkInvariants rejects a mutation that broke an immutable field or left
// the record in an undefined state.
func checkInvariants(before, after Agent) error {
	if after.ID != before.ID {
		return fmt.Errorf("the mutator changed the ID to %q; an agent's ID is immutable", after.ID)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		return fmt.Errorf("the mutator changed CreatedAt to %s; it is immutable",
			after.CreatedAt.Format(time.RFC3339Nano))
	}
	if !after.Status.Valid() {
		return fmt.Errorf("the mutator set the unknown status %q", after.Status)
	}
	return nil
}

// put JSON-encodes agent and stores it under its own ID.
func put(b *bbolt.Bucket, agent Agent) error {
	raw, err := json.Marshal(agent)
	if err != nil {
		return fmt.Errorf("encoding agent %q: %w", agent.ID, err)
	}
	if err := b.Put([]byte(agent.ID), raw); err != nil {
		return fmt.Errorf("writing agent %q: %w", agent.ID, err)
	}
	return nil
}

// decode JSON-decodes one record. The raw bytes belong to the transaction
// and are invalid after it ends, but json.Unmarshal copies everything it
// keeps into agent, so the decoded value safely outlives the transaction.
func decode(id string, raw []byte, agent *Agent) error {
	if err := json.Unmarshal(raw, agent); err != nil {
		return fmt.Errorf("decoding agent %q from the state database: %w", id, err)
	}
	return nil
}
