package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// seedLegacyAnthropicAuth writes the pre-accounts singleton credential
// directly into a fresh BoltDB file at path, bypassing the store package
// entirely (which no longer exposes a way to write it) -- this is the only
// way old-format data can still exist on disk: a store.db from before this
// feature shipped.
func seedLegacyAnthropicAuth(t *testing.T, path string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("opening a raw bbolt file: %v", err)
	}
	defer db.Close()
	raw := `{"kind":"api_key","value":"sk-ant-legacy","updated_at":"2026-01-01T00:00:00Z"}`
	if err := db.Update(func(tx *bbolt.Tx) error {
		settings, err := tx.CreateBucketIfNotExists([]byte("settings"))
		if err != nil {
			return err
		}
		agents, err := tx.CreateBucketIfNotExists([]byte("agents"))
		if err != nil {
			return err
		}
		if err := settings.Put([]byte("anthropic_auth"), []byte(raw)); err != nil {
			return err
		}
		// A pre-existing agent that was implicitly using the shared
		// credential: backend=anthropic, no anthropic_account_id field at
		// all (it predates the field).
		agent := `{"id":"agt_legacy1","backend":"anthropic","status":"stopped","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
		return agents.Put([]byte("agt_legacy1"), []byte(agent))
	}); err != nil {
		t.Fatalf("seeding legacy data: %v", err)
	}
}

func TestMigrateLegacyAnthropicAuth_PromotesToDefaultAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seedLegacyAnthropicAuth(t, path)

	s, err := store.Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	accounts, err := s.ListAnthropicAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAnthropicAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("ListAnthropicAccounts after migration = %d accounts; want 1", len(accounts))
	}
	got := accounts[0]
	if got.Name != "Default" || got.Kind != store.AnthropicKindAPIKey || got.Value != "sk-ant-legacy" {
		t.Fatalf("migrated account = %+v; want Name=Default Kind=api_key Value=sk-ant-legacy", got)
	}

	defaultID, err := s.DefaultAnthropicAccountID(ctx)
	if err != nil || defaultID != got.ID {
		t.Fatalf("DefaultAnthropicAccountID = %q, %v; want %q, nil", defaultID, err, got.ID)
	}

	legacyAgent, err := s.Get(ctx, "agt_legacy1")
	if err != nil {
		t.Fatalf("Get(agt_legacy1): %v", err)
	}
	if legacyAgent.AnthropicAccountID != got.ID {
		t.Fatalf("legacy agent AnthropicAccountID = %q; want %q", legacyAgent.AnthropicAccountID, got.ID)
	}
}

// TestMigrateLegacyAnthropicAuth_IsIdempotent is a Review Focus case: a
// second Open (the operator process restarting) must not create a second
// "Default" account or re-pin an agent the user already repointed
// elsewhere.
func TestMigrateLegacyAnthropicAuth_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seedLegacyAnthropicAuth(t, path)

	s1, err := store.Open(path, 10)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	ctx := context.Background()
	accounts, _ := s1.ListAnthropicAccounts(ctx)
	migratedID := accounts[0].ID

	// Simulate the user repointing the legacy agent at a second,
	// independently created account before the next restart.
	other, err := s1.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindAPIKey, "sk-ant-personal")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	if _, err := s1.Update(ctx, "agt_legacy1", func(a *store.Agent) error {
		a.AnthropicAccountID = other.ID
		return nil
	}); err != nil {
		t.Fatalf("repointing the legacy agent: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("closing before reopening: %v", err)
	}

	s2, err := store.Open(path, 10)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	accounts, err = s2.ListAnthropicAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAnthropicAccounts after reopen: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("ListAnthropicAccounts after reopen = %d accounts; want 2 (no re-migration)", len(accounts))
	}
	legacyAgent, err := s2.Get(ctx, "agt_legacy1")
	if err != nil {
		t.Fatalf("Get(agt_legacy1) after reopen: %v", err)
	}
	if legacyAgent.AnthropicAccountID != other.ID {
		t.Fatalf("legacy agent AnthropicAccountID after reopen = %q; want unchanged %q (not re-pinned to %q)", legacyAgent.AnthropicAccountID, other.ID, migratedID)
	}
}

func TestMigrateLegacyAnthropicAuth_NoLegacyDataIsANoop(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	accounts, err := s.ListAnthropicAccounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("ListAnthropicAccounts on a fresh store = %v, %v; want empty, nil", accounts, err)
	}
}

var _ = time.Time{} // keep the "time" import if later assertions need it
