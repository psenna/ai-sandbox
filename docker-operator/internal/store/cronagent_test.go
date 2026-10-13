package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func mustCreateCronAgent(t *testing.T, s *Store, id, name, templateID string) CronAgent {
	t.Helper()
	ca, err := s.CreateCronAgent(context.Background(), CronAgentCreateSpec{
		ID: id, Name: name, TemplateID: templateID, Schedule: "0 9 * * *", Enabled: true, Task: "do the thing", OnComplete: "stop",
	})
	if err != nil {
		t.Fatalf("CreateCronAgent(%q, %q): %v", id, name, err)
	}
	return ca
}

func TestOpen_CreatesTheCronAgentsBucket(t *testing.T) {
	s := newStore(t, 1)
	if _, err := s.ListCronAgents(context.Background()); err != nil {
		t.Fatalf("ListCronAgents on a fresh store: %v, want the bucket to already exist", err)
	}
}

func TestNewCronAgentID_FormatAndUniqueness(t *testing.T) {
	a, err := NewCronAgentID()
	if err != nil {
		t.Fatalf("NewCronAgentID: %v", err)
	}
	if len(a) != len(cronAgentIDPrefix)+2*cronAgentIDBytes {
		t.Errorf("NewCronAgentID() = %q, wrong length", a)
	}
	b, err := NewCronAgentID()
	if err != nil {
		t.Fatalf("NewCronAgentID: %v", err)
	}
	if a == b {
		t.Errorf("NewCronAgentID returned the same ID twice: %q", a)
	}
}

func TestCreateCronAgent_InsertsARecord(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)

	got, err := s.CreateCronAgent(ctx, CronAgentCreateSpec{
		ID: "ca_7f3a9c2d", Name: "nightly report", TemplateID: "tpl_1",
		Schedule: "0 9 * * *", Enabled: true, Task: "write today's report", OnComplete: "delete",
	})
	if err != nil {
		t.Fatalf("CreateCronAgent: %v", err)
	}

	want := CronAgent{
		ID: "ca_7f3a9c2d", Name: "nightly report", TemplateID: "tpl_1",
		Schedule: "0 9 * * *", Enabled: true, Task: "write today's report", OnComplete: "delete",
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateCronAgent returned %+v, want %+v", got, want)
	}

	stored, err := s.GetCronAgent(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetCronAgent: %v", err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("GetCronAgent returned %+v, want %+v (every field must round-trip through JSON)", stored, want)
	}
}

func TestCreateCronAgent_RejectsAnEmptyID(t *testing.T) {
	_, err := newStore(t, 1).CreateCronAgent(context.Background(), CronAgentCreateSpec{Name: "x", TemplateID: "tpl_1"})
	if err == nil {
		t.Fatal("CreateCronAgent with an empty ID returned nil, want an error")
	}
}

func TestCreateCronAgent_RejectsADuplicateID(t *testing.T) {
	s := newStore(t, 1)
	mustCreateCronAgent(t, s, "ca_00000001", "first", "tpl_1")

	_, err := s.CreateCronAgent(context.Background(), CronAgentCreateSpec{ID: "ca_00000001", Name: "second", TemplateID: "tpl_1"})
	if !IsCronAgentExists(err) {
		t.Fatalf("CreateCronAgent with a duplicate ID: err = %v, want one satisfying IsCronAgentExists", err)
	}
}

func TestGetCronAgent_NotFound(t *testing.T) {
	_, err := newStore(t, 1).GetCronAgent(context.Background(), "ca_missing")
	if !IsCronAgentNotFound(err) {
		t.Fatalf("GetCronAgent(missing): err = %v, want IsCronAgentNotFound", err)
	}
}

func TestListCronAgents_EmptyStoreReturnsANonNilSlice(t *testing.T) {
	got, err := newStore(t, 1).ListCronAgents(context.Background())
	if err != nil {
		t.Fatalf("ListCronAgents: %v", err)
	}
	if got == nil {
		t.Error("ListCronAgents on an empty store returned nil, want a non-nil empty slice")
	}
}

func TestListCronAgents_ReturnsEverySortedByID(t *testing.T) {
	s := newStore(t, 1)
	mustCreateCronAgent(t, s, "ca_2", "two", "tpl_1")
	mustCreateCronAgent(t, s, "ca_1", "one", "tpl_1")

	got, err := s.ListCronAgents(context.Background())
	if err != nil {
		t.Fatalf("ListCronAgents: %v", err)
	}
	if len(got) != 2 || got[0].ID != "ca_1" || got[1].ID != "ca_2" {
		t.Errorf("ListCronAgents = %+v, want [ca_1, ca_2] (byte order)", got)
	}
}

func TestUpdateCronAgent_StampsUpdatedAtAndOverwritesFields(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)
	ca := mustCreateCronAgent(t, s, "ca_1", "nightly", "tpl_1")

	updated, err := s.UpdateCronAgent(ctx, ca.ID, func(c *CronAgent) error {
		c.Name = "renamed"
		c.Schedule = "0 10 * * *"
		c.Enabled = false
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateCronAgent: %v", err)
	}
	if updated.Name != "renamed" || updated.Schedule != "0 10 * * *" || updated.Enabled {
		t.Errorf("UpdateCronAgent returned %+v, want the mutated fields applied", updated)
	}
	if !updated.UpdatedAt.After(ca.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want it stamped after the original %v", updated.UpdatedAt, ca.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(ca.CreatedAt) {
		t.Errorf("CreatedAt changed from %v to %v, want it immutable", ca.CreatedAt, updated.CreatedAt)
	}
}

func TestUpdateCronAgent_NotFound(t *testing.T) {
	_, err := newStore(t, 1).UpdateCronAgent(context.Background(), "ca_missing", func(c *CronAgent) error { return nil })
	if !IsCronAgentNotFound(err) {
		t.Fatalf("UpdateCronAgent(missing): err = %v, want IsCronAgentNotFound", err)
	}
}

func TestUpdateCronAgent_RejectsANilMutator(t *testing.T) {
	s := newStore(t, 1)
	ca := mustCreateCronAgent(t, s, "ca_1", "nightly", "tpl_1")
	if _, err := s.UpdateCronAgent(context.Background(), ca.ID, nil); err == nil {
		t.Fatal("UpdateCronAgent(nil mutator) = nil error, want an error")
	}
}

func TestDeleteCronAgent_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	ca := mustCreateCronAgent(t, s, "ca_1", "nightly", "tpl_1")

	if err := s.DeleteCronAgent(ctx, ca.ID); err != nil {
		t.Fatalf("DeleteCronAgent: %v", err)
	}
	if err := s.DeleteCronAgent(ctx, ca.ID); err != nil {
		t.Fatalf("DeleteCronAgent again: %v, want idempotent success", err)
	}
	if _, err := s.GetCronAgent(ctx, ca.ID); !IsCronAgentNotFound(err) {
		t.Errorf("GetCronAgent after delete: err = %v, want IsCronAgentNotFound", err)
	}
}

func TestCronAgentsBucketMissingSurfacesAsAnError(t *testing.T) {
	s := newStore(t, 1)
	if err := s.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(bucketCronAgents) }); err != nil {
		t.Fatalf("deleting the cron_agents bucket to simulate corruption: %v", err)
	}
	if _, err := s.ListCronAgents(context.Background()); err == nil {
		t.Error("ListCronAgents with a missing bucket = nil error, want an error")
	}
}

func TestCorruptCronAgentRecordSurfacesAsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	if err := s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		return b.Put([]byte("ca_corrupt"), []byte("not json"))
	}); err != nil {
		t.Fatalf("seeding a corrupt record: %v", err)
	}
	if _, err := s.GetCronAgent(ctx, "ca_corrupt"); err == nil {
		t.Error("GetCronAgent(corrupt record) = nil error, want an error")
	}
}
