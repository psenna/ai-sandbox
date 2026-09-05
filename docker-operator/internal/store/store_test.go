package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// baseTime is the instant the injectable clock starts from in tests that
// need deterministic timestamps. It is UTC so it round-trips through JSON
// byte-for-byte and compares equal under reflect.DeepEqual.
var baseTime = time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)

// clockAt returns a clock yielding base, base+step, base+2*step, ... It is
// not safe for concurrent use and is never installed in a concurrency test.
func clockAt(base time.Time, step time.Duration) func() time.Time {
	var n int64
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// newStore opens a store on a throwaway database under t.TempDir. bbolt is
// pure Go and needs nothing but a writable file, so these tests need neither
// a Docker daemon nor cgo.
func newStore(t *testing.T, maxAgents int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), maxAgents)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func mustCreate(t *testing.T, s *Store, id string) Agent {
	t.Helper()
	a, err := s.Create(context.Background(), CreateSpec{ID: id, Name: id, Description: "d-" + id})
	if err != nil {
		t.Fatalf("Create(%q): %v", id, err)
	}
	return a
}

func mustSetStatus(t *testing.T, s *Store, id string, status Status) {
	t.Helper()
	if _, err := s.Update(context.Background(), id, func(a *Agent) error {
		a.Status = status
		return nil
	}); err != nil {
		t.Fatalf("Update(%q -> %s): %v", id, status, err)
	}
}

func agentIDs(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.ID)
	}
	return out
}

// --- Open / Close ---------------------------------------------------------

func TestOpen_CreatesTheFileAndItsParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.db")

	s, err := Open(path, 3)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
	if got := s.MaxAgents(); got != 3 {
		t.Fatalf("MaxAgents() = %d, want 3", got)
	}
}

func TestOpen_Rejects(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(regular, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		name      string
		path      string
		maxAgents int
	}{
		{"empty path", "", 1},
		{"zero cap", filepath.Join(dir, "a.db"), 0},
		{"negative cap", filepath.Join(dir, "b.db"), -1},
		{"parent is a regular file", filepath.Join(regular, "state.db"), 1},
		{"path is a directory", dir, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(tc.path, tc.maxAgents)
			if err == nil {
				_ = s.Close()
				t.Fatalf("Open(%q, %d) succeeded, want an error", tc.path, tc.maxAgents)
			}
		})
	}
}

// TestOpen_SecondOpenerIsLockedOut proves the single-writer guarantee holds
// across processes too: bbolt's exclusive file lock, not just its in-process
// transaction lock, is what makes the MAX_AGENTS reservation safe.
func TestOpen_SecondOpenerIsLockedOut(t *testing.T) {
	prev := openTimeout
	openTimeout = 50 * time.Millisecond
	t.Cleanup(func() { openTimeout = prev })

	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(path, 1)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	second, err := Open(path, 1)
	if err == nil {
		_ = second.Close()
		t.Fatal("second Open succeeded while the first holds the lock, want an error")
	}
}

func TestOpen_ReopenPreservesRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path, 2)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	created := mustCreate(t, first, "agt_00000001")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path, 2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	got, err := second.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !reflect.DeepEqual(got, created) {
		t.Fatalf("Get after reopen = %+v, want %+v", got, created)
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	s := newStore(t, 1)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestMethodsAfterCloseReturnErrors(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	mustCreate(t, s, "agt_00000001")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := s.Create(ctx, CreateSpec{ID: "agt_00000002"}); err == nil {
		t.Error("Create after Close returned nil, want an error")
	}
	if _, err := s.Get(ctx, "agt_00000001"); err == nil {
		t.Error("Get after Close returned nil, want an error")
	}
	if _, err := s.List(ctx); err == nil {
		t.Error("List after Close returned nil, want an error")
	}
	if _, err := s.Update(ctx, "agt_00000001", func(*Agent) error { return nil }); err == nil {
		t.Error("Update after Close returned nil, want an error")
	}
	if err := s.Delete(ctx, "agt_00000001"); err == nil {
		t.Error("Delete after Close returned nil, want an error")
	}
	if _, _, err := s.GetAnthropicAuth(ctx); err == nil {
		t.Error("GetAnthropicAuth after Close returned nil, want an error")
	}
	if err := s.SetAnthropicAuth(ctx, AnthropicKindOAuth, "x"); err == nil {
		t.Error("SetAnthropicAuth after Close returned nil, want an error")
	}
	if err := s.ClearAnthropicAuth(ctx); err == nil {
		t.Error("ClearAnthropicAuth after Close returned nil, want an error")
	}
}

// --- NewID ------------------------------------------------------------

func TestNewID_FormatAndUniqueness(t *testing.T) {
	shape := regexp.MustCompile(`^agt_[0-9a-f]{8}$`)
	seen := make(map[string]bool, 256)
	for range 256 {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if !shape.MatchString(id) {
			t.Fatalf("NewID() = %q, want a match for %s", id, shape)
		}
		if seen[id] {
			t.Fatalf("NewID() returned the duplicate %q", id)
		}
		seen[id] = true
	}
}

// --- Create -----------------------------------------------------------

func TestCreate_InsertsACreatingRecord(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)
	s.now = clockAt(baseTime, time.Second)

	got, err := s.Create(ctx, CreateSpec{ID: "agt_7f3a9c2d", Name: "alpha", Description: "the first one"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := Agent{
		ID:          "agt_7f3a9c2d",
		Name:        "alpha",
		Description: "the first one",
		Status:      StatusCreating,
		CreatedAt:   baseTime,
		UpdatedAt:   baseTime,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Create returned %+v, want %+v", got, want)
	}

	// Every Docker field must start empty: the record is inserted before any
	// resource exists.
	stored, err := s.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("Get returned %+v, want %+v (the record must round-trip through JSON unchanged)", stored, want)
	}
	if stored.DependaproxyDinernetIP.IsValid() {
		t.Fatalf("DependaproxyDinernetIP = %v, want the zero Addr", stored.DependaproxyDinernetIP)
	}
}

func TestCreate_RejectsAnEmptyID(t *testing.T) {
	s := newStore(t, 1)
	if _, err := s.Create(context.Background(), CreateSpec{Name: "no id"}); err == nil {
		t.Fatal("Create with an empty ID returned nil, want an error")
	}
}

func TestCreate_RejectsADuplicateID(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)
	first := mustCreate(t, s, "agt_00000001")

	_, err := s.Create(ctx, CreateSpec{ID: first.ID, Name: "impostor", Description: "clobber me"})
	if !IsExists(err) {
		t.Fatalf("Create with a duplicate ID: err = %v, want one satisfying IsExists", err)
	}

	stored, err := s.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(stored, first) {
		t.Fatalf("the rejected Create modified the record: %+v, want %+v", stored, first)
	}
}

func TestCreate_EnforcesTheCap(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 2)
	mustCreate(t, s, "agt_00000001")
	mustCreate(t, s, "agt_00000002")

	_, err := s.Create(ctx, CreateSpec{ID: "agt_00000003"})
	if !IsAtCapacity(err) {
		t.Fatalf("Create over the cap: err = %v, want one satisfying IsAtCapacity", err)
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("error %q does not report the slot usage", err)
	}
	if _, err := s.Get(ctx, "agt_00000003"); !IsNotFound(err) {
		t.Fatalf("a rejected Create left a record behind: err = %v", err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %v, want exactly the 2 accepted agents", agentIDs(list))
	}
}

// TestCreate_OnlyCreatingAndRunningHoldSlots pins the capacity policy: an
// agent being torn down, stopped or broken must not keep the host's last
// slot occupied.
func TestCreate_OnlyCreatingAndRunningHoldSlots(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		status    Status
		holdsSlot bool
	}{
		{StatusCreating, true},
		{StatusRunning, true},
		{StatusStopped, false},
		{StatusError, false},
		{StatusDeleting, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			s := newStore(t, 1)
			mustCreate(t, s, "agt_00000001")
			mustSetStatus(t, s, "agt_00000001", tc.status)

			_, err := s.Create(ctx, CreateSpec{ID: "agt_00000002"})
			switch {
			case tc.holdsSlot && !IsAtCapacity(err):
				t.Fatalf("Create with one %s agent: err = %v, want IsAtCapacity", tc.status, err)
			case !tc.holdsSlot && err != nil:
				t.Fatalf("Create with one %s agent: %v, want success", tc.status, err)
			}
		})
	}
}

// TestCreate_ConcurrentCreatesNeverExceedTheCap is the acceptance test for
// the atomic reservation: N goroutines released from one barrier all race
// Create, and exactly maxAgents of them may win. The start channel is what
// makes the contention real -- without it the first goroutines routinely
// finish before the last ones are even scheduled.
func TestCreate_ConcurrentCreatesNeverExceedTheCap(t *testing.T) {
	const goroutines = 20

	for _, maxAgents := range []int{1, 3} {
		t.Run(fmt.Sprintf("max=%d", maxAgents), func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t, maxAgents)

			start := make(chan struct{})
			errs := make([]error, goroutines)
			wantName := make(map[string]string, goroutines)
			var wg sync.WaitGroup
			for i := range goroutines {
				wantName[fmt.Sprintf("agt_%08d", i)] = fmt.Sprintf("racer-%d", i)
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, err := s.Create(ctx, CreateSpec{
						ID:   fmt.Sprintf("agt_%08d", i),
						Name: fmt.Sprintf("racer-%d", i),
					})
					errs[i] = err
				}(i)
			}
			close(start)
			wg.Wait()

			won := make(map[string]bool)
			for i, err := range errs {
				id := fmt.Sprintf("agt_%08d", i)
				switch {
				case err == nil:
					won[id] = true
				case IsAtCapacity(err):
				default:
					t.Fatalf("Create(%s): unexpected error %v, want nil or IsAtCapacity", id, err)
				}
			}
			if len(won) != maxAgents {
				t.Fatalf("%d of %d creates succeeded, want exactly %d", len(won), goroutines, maxAgents)
			}

			// No partial, duplicated or half-written records: the store must
			// hold exactly the winners, each a well-formed creating record.
			list, err := s.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != maxAgents {
				t.Fatalf("List returned %v, want exactly the %d winners", agentIDs(list), maxAgents)
			}
			for _, a := range list {
				if !won[a.ID] {
					t.Fatalf("List returned %q, which no successful Create returned", a.ID)
				}
				if a.Status != StatusCreating {
					t.Fatalf("agent %q has status %q, want %q", a.ID, a.Status, StatusCreating)
				}
				if a.Name != wantName[a.ID] {
					t.Fatalf("agent %q has name %q, want %q -- the record was written by a different goroutine", a.ID, a.Name, wantName[a.ID])
				}
				if a.CreatedAt.IsZero() || !a.CreatedAt.Equal(a.UpdatedAt) {
					t.Fatalf("agent %q has CreatedAt=%v UpdatedAt=%v, want both set and equal",
						a.ID, a.CreatedAt, a.UpdatedAt)
				}
			}
		})
	}
}

// --- Get / List ---------------------------------------------------------

func TestGet_NotFound(t *testing.T) {
	_, err := newStore(t, 1).Get(context.Background(), "agt_missing")
	if !IsNotFound(err) {
		t.Fatalf("Get on a missing id: err = %v, want one satisfying IsNotFound", err)
	}
}

func TestList_EmptyStoreReturnsANonNilSlice(t *testing.T) {
	got, err := newStore(t, 1).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("List returned nil; it must return an empty slice so the API encodes [] and not null")
	}
	if len(got) != 0 {
		t.Fatalf("List returned %v, want empty", agentIDs(got))
	}
}

func TestList_ReturnsEveryAgentSortedByID(t *testing.T) {
	s := newStore(t, 5)
	for _, id := range []string{"agt_cccccccc", "agt_aaaaaaaa", "agt_bbbbbbbb"} {
		mustCreate(t, s, id)
	}

	got, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"agt_aaaaaaaa", "agt_bbbbbbbb", "agt_cccccccc"}
	if !reflect.DeepEqual(agentIDs(got), want) {
		t.Fatalf("List returned %v, want %v", agentIDs(got), want)
	}
}

// --- Update -------------------------------------------------------------

func TestUpdate_StampsEveryFieldAndAdvancesUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)
	created := mustCreate(t, s, "agt_7f3a9c2d")

	ip := netip.MustParseAddr("172.23.0.10")
	got, err := s.Update(ctx, created.ID, func(a *Agent) error {
		a.Name = "renamed"
		a.Description = "edited"
		a.Status = StatusRunning
		a.ContainerName = "docker-operator-agent-agt_7f3a9c2d"
		a.ContainerID = "c0ffee"
		a.DindContainerName = "docker-operator-dind-agt_7f3a9c2d"
		a.DindContainerID = "deadbeef"
		a.DinernetName = "docker-operator-agent-agt_7f3a9c2d-dinernet"
		a.DinernetID = "net123"
		a.WorkspaceVolume = "docker-operator-agent-agt_7f3a9c2d-workspace"
		a.ClaudeConfigVolume = "docker-operator-agent-agt_7f3a9c2d-claude-config"
		a.DindCacheVolume = "docker-operator-agent-agt_7f3a9c2d-dind-cache"
		a.DependaproxyDinernetIP = ip
		// Deliberately forged: Update must overwrite it with its own clock.
		a.UpdatedAt = time.Unix(0, 0).UTC()
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	want := Agent{
		ID:                     created.ID,
		Name:                   "renamed",
		Description:            "edited",
		Status:                 StatusRunning,
		ContainerName:          "docker-operator-agent-agt_7f3a9c2d",
		ContainerID:            "c0ffee",
		DindContainerName:      "docker-operator-dind-agt_7f3a9c2d",
		DindContainerID:        "deadbeef",
		DinernetName:           "docker-operator-agent-agt_7f3a9c2d-dinernet",
		DinernetID:             "net123",
		WorkspaceVolume:        "docker-operator-agent-agt_7f3a9c2d-workspace",
		ClaudeConfigVolume:     "docker-operator-agent-agt_7f3a9c2d-claude-config",
		DindCacheVolume:        "docker-operator-agent-agt_7f3a9c2d-dind-cache",
		DependaproxyDinernetIP: ip,
		CreatedAt:              baseTime,
		UpdatedAt:              baseTime.Add(time.Minute),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Update returned %+v, want %+v", got, want)
	}

	stored, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("Get returned %+v, want %+v (every field must round-trip through JSON)", stored, want)
	}
}

func TestUpdate_RecordsAnErrorMessage(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	created := mustCreate(t, s, "agt_00000001")

	got, err := s.Update(ctx, created.ID, func(a *Agent) error {
		a.Status = StatusError
		a.ErrorMessage = "dind sidecar never became healthy"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Status != StatusError || got.ErrorMessage != "dind sidecar never became healthy" {
		t.Fatalf("Update returned status %q message %q", got.Status, got.ErrorMessage)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	_, err := newStore(t, 1).Update(context.Background(), "agt_missing", func(*Agent) error { return nil })
	if !IsNotFound(err) {
		t.Fatalf("Update on a missing id: err = %v, want one satisfying IsNotFound", err)
	}
}

func TestUpdate_RejectsANilMutator(t *testing.T) {
	s := newStore(t, 1)
	created := mustCreate(t, s, "agt_00000001")
	if _, err := s.Update(context.Background(), created.ID, nil); err == nil {
		t.Fatal("Update with a nil mutator returned nil, want an error")
	}
}

// TestUpdate_MutatorFailuresRollBack covers both a mutator that returns an
// error and every invariant the store enforces on the mutator's result. In
// all cases the stored record must be exactly what it was.
func TestUpdate_MutatorFailuresRollBack(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("mutator said no")

	cases := []struct {
		name   string
		mutate func(*Agent) error
		check  func(*testing.T, error)
	}{
		{
			name:   "mutator error",
			mutate: func(a *Agent) error { a.Name = "half-written"; return sentinel },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, sentinel) {
					t.Fatalf("err = %v, want it to wrap the mutator's own error", err)
				}
			},
		},
		{
			name:   "changed id",
			mutate: func(a *Agent) error { a.ID = "agt_somethingelse"; return nil },
		},
		{
			name:   "changed created at",
			mutate: func(a *Agent) error { a.CreatedAt = a.CreatedAt.Add(time.Hour); return nil },
		},
		{
			name:   "unknown status",
			mutate: func(a *Agent) error { a.Status = Status("runnning"); return nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t, 1)
			created := mustCreate(t, s, "agt_00000001")

			got, err := s.Update(ctx, created.ID, tc.mutate)
			if err == nil {
				t.Fatalf("Update returned %+v and no error, want an error", got)
			}
			if tc.check != nil {
				tc.check(t, err)
			}
			if !reflect.DeepEqual(got, Agent{}) {
				t.Errorf("Update returned %+v alongside an error, want the zero Agent", got)
			}

			stored, err := s.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !reflect.DeepEqual(stored, created) {
				t.Fatalf("the failed Update was not rolled back: %+v, want %+v", stored, created)
			}
		})
	}
}

func TestUpdate_MarkingDeletingFreesTheSlotImmediately(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	mustCreate(t, s, "agt_00000001")

	if _, err := s.Create(ctx, CreateSpec{ID: "agt_00000002"}); !IsAtCapacity(err) {
		t.Fatalf("Create at the cap: err = %v, want IsAtCapacity", err)
	}
	mustSetStatus(t, s, "agt_00000001", StatusDeleting)
	if _, err := s.Create(ctx, CreateSpec{ID: "agt_00000002"}); err != nil {
		t.Fatalf("Create after the slot was freed: %v", err)
	}
}

// --- Delete ---------------------------------------------------------------

func TestDelete_IsIdempotentAndFreesTheSlot(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	mustCreate(t, s, "agt_00000001")

	if err := s.Delete(ctx, "agt_00000001"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "agt_00000001"); err != nil {
		t.Fatalf("second Delete: %v, want nil (deleting a missing record is success)", err)
	}
	if err := s.Delete(ctx, "agt_neverexisted"); err != nil {
		t.Fatalf("Delete of an unknown id: %v, want nil", err)
	}
	if _, err := s.Get(ctx, "agt_00000001"); !IsNotFound(err) {
		t.Fatalf("Get after Delete: err = %v, want IsNotFound", err)
	}
	if _, err := s.Create(ctx, CreateSpec{ID: "agt_00000002"}); err != nil {
		t.Fatalf("Create after Delete freed the slot: %v", err)
	}
}

// --- Status ---------------------------------------------------------------

func TestStatus(t *testing.T) {
	cases := []struct {
		status    Status
		valid     bool
		holdsSlot bool
	}{
		{StatusCreating, true, true},
		{StatusRunning, true, true},
		{StatusStopped, true, false},
		{StatusError, true, false},
		{StatusDeleting, true, false},
		{Status(""), false, false},
		{Status("Running"), false, false},
		{Status("terminating"), false, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.status.CountsTowardCapacity(); got != tc.holdsSlot {
				t.Errorf("CountsTowardCapacity() = %v, want %v", got, tc.holdsSlot)
			}
		})
	}
}

// --- context --------------------------------------------------------------

func TestEveryMethodHonoursACancelledContext(t *testing.T) {
	s := newStore(t, 1)
	created := mustCreate(t, s, "agt_00000001")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { _, err := s.Create(ctx, CreateSpec{ID: "agt_00000002"}); return err }},
		{"Get", func() error { _, err := s.Get(ctx, created.ID); return err }},
		{"List", func() error { _, err := s.List(ctx); return err }},
		{"Update", func() error { _, err := s.Update(ctx, created.ID, func(*Agent) error { return nil }); return err }},
		{"Delete", func() error { return s.Delete(ctx, created.ID) }},
		{"GetAnthropicAuth", func() error { _, _, err := s.GetAnthropicAuth(ctx); return err }},
		{"SetAnthropicAuth", func() error { return s.SetAnthropicAuth(ctx, AnthropicKindAPIKey, "x") }},
		{"ClearAnthropicAuth", func() error { return s.ClearAnthropicAuth(ctx) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want one wrapping context.Canceled", err)
			}
		})
	}

	// None of the cancelled calls may have changed anything.
	if _, err := s.Get(context.Background(), created.ID); err != nil {
		t.Fatalf("Get after the cancelled calls: %v", err)
	}
}

// --- damaged database -------------------------------------------------

// TestCorruptRecordSurfacesAsAnError proves a garbled value fails loudly on
// every path that decodes one -- in particular Create, whose capacity count
// must never silently under-count because one record would not parse.
func TestCorruptRecordSurfacesAsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAgents).Put([]byte("agt_corrupt"), []byte("{not json"))
	}); err != nil {
		t.Fatalf("seeding a corrupt record: %v", err)
	}

	if _, err := s.Get(ctx, "agt_corrupt"); err == nil {
		t.Error("Get on a corrupt record returned nil, want an error")
	}
	if _, err := s.List(ctx); err == nil {
		t.Error("List with a corrupt record returned nil, want an error")
	}
	if _, err := s.Update(ctx, "agt_corrupt", func(*Agent) error { return nil }); err == nil {
		t.Error("Update on a corrupt record returned nil, want an error")
	}
	if _, err := s.Create(ctx, CreateSpec{ID: "agt_00000001"}); err == nil {
		t.Error("Create with a corrupt record present returned nil, want an error")
	}
}

// TestMissingBucketSurfacesAsAnError covers the defensive nil-bucket branch:
// a state file that is not this program's database must produce a legible
// error rather than a nil-pointer panic.
func TestMissingBucketSurfacesAsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.DeleteBucket(bucketAgents)
	}); err != nil {
		t.Fatalf("deleting the bucket: %v", err)
	}

	if _, err := s.Create(ctx, CreateSpec{ID: "agt_00000001"}); err == nil {
		t.Error("Create without the bucket returned nil, want an error")
	}
	if _, err := s.Get(ctx, "agt_00000001"); err == nil {
		t.Error("Get without the bucket returned nil, want an error")
	}
	if _, err := s.List(ctx); err == nil {
		t.Error("List without the bucket returned nil, want an error")
	}
	if _, err := s.Update(ctx, "agt_00000001", func(*Agent) error { return nil }); err == nil {
		t.Error("Update without the bucket returned nil, want an error")
	}
	if err := s.Delete(ctx, "agt_00000001"); err == nil {
		t.Error("Delete without the bucket returned nil, want an error")
	}
}

func TestCreate_PersistsBackendAndModels(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)
	s.now = clockAt(baseTime, time.Second)

	spec := CreateSpec{
		ID:        "agt_backend01",
		Name:      "ollama-agent",
		Backend:   "ollama",
		Model:     "glm-5.3:cloud",
		FastModel: "glm-5.3-flash:cloud",
	}
	got, err := s.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Backend != "ollama" || got.Model != "glm-5.3:cloud" || got.FastModel != "glm-5.3-flash:cloud" {
		t.Fatalf("Create returned backend/model/fast_model = %q/%q/%q, want the spec's values", got.Backend, got.Model, got.FastModel)
	}

	// Round-trips through JSON + a reopen unchanged.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(s.path, 5)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	stored, err := s2.Get(ctx, spec.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if stored.Backend != "ollama" || stored.Model != "glm-5.3:cloud" || stored.FastModel != "glm-5.3-flash:cloud" {
		t.Fatalf("after reopen backend/model/fast_model = %q/%q/%q, want the spec's values", stored.Backend, stored.Model, stored.FastModel)
	}
}

func TestCreate_AnthropicAgentLeavesModelFieldsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)

	got, err := s.Create(ctx, CreateSpec{ID: "agt_anthropic1", Backend: "anthropic"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Backend != "anthropic" {
		t.Fatalf("Backend = %q, want %q", got.Backend, "anthropic")
	}
	if got.Model != "" || got.FastModel != "" {
		t.Fatalf("Model/FastModel = %q/%q, want both empty for an anthropic agent", got.Model, got.FastModel)
	}
}

func TestAnthropicAuth_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)
	s.now = clockAt(baseTime, time.Minute)

	// Nothing configured yet.
	if _, ok, err := s.GetAnthropicAuth(ctx); err != nil || ok {
		t.Fatalf("GetAnthropicAuth on a fresh store = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	// Set an API key.
	if err := s.SetAnthropicAuth(ctx, AnthropicKindAPIKey, "sk-ant-secret"); err != nil {
		t.Fatalf("SetAnthropicAuth(api_key): %v", err)
	}
	auth, ok, err := s.GetAnthropicAuth(ctx)
	if err != nil || !ok {
		t.Fatalf("GetAnthropicAuth after set = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if auth.Kind != AnthropicKindAPIKey || auth.Value != "sk-ant-secret" {
		t.Fatalf("stored auth = %+v, want kind=api_key value=sk-ant-secret", auth)
	}
	if auth.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt is zero, want it stamped from the store clock")
	}

	// Replacing with an OAuth token overwrites, not appends.
	if err := s.SetAnthropicAuth(ctx, AnthropicKindOAuth, "oat-token"); err != nil {
		t.Fatalf("SetAnthropicAuth(oauth): %v", err)
	}
	auth, _, err = s.GetAnthropicAuth(ctx)
	if err != nil {
		t.Fatalf("GetAnthropicAuth: %v", err)
	}
	if auth.Kind != AnthropicKindOAuth || auth.Value != "oat-token" {
		t.Fatalf("after replace stored auth = %+v, want kind=oauth value=oat-token", auth)
	}

	// Survives a reopen.
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(path, 5)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if auth, ok, err := s2.GetAnthropicAuth(ctx); err != nil || !ok || auth.Value != "oat-token" {
		t.Fatalf("after reopen GetAnthropicAuth = (%+v, %v, %v), want the oauth token", auth, ok, err)
	}

	// Clear, twice -- the second is a no-op, not an error.
	if err := s2.ClearAnthropicAuth(ctx); err != nil {
		t.Fatalf("ClearAnthropicAuth: %v", err)
	}
	if err := s2.ClearAnthropicAuth(ctx); err != nil {
		t.Fatalf("ClearAnthropicAuth (second call): %v", err)
	}
	if _, ok, err := s2.GetAnthropicAuth(ctx); err != nil || ok {
		t.Fatalf("GetAnthropicAuth after clear = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}

func TestSetAnthropicAuth_Rejects(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 5)

	if err := s.SetAnthropicAuth(ctx, "bearer", "x"); err == nil {
		t.Error("SetAnthropicAuth with an unknown kind returned nil, want an error")
	}
	if err := s.SetAnthropicAuth(ctx, AnthropicKindAPIKey, ""); err == nil {
		t.Error("SetAnthropicAuth with an empty value returned nil, want an error")
	}
	// A rejected call stores nothing.
	if _, ok, err := s.GetAnthropicAuth(ctx); err != nil || ok {
		t.Fatalf("GetAnthropicAuth after rejected sets = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}
