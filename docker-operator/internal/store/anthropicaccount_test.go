package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAnthropicAccount_CreateGetList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if accs, err := s.ListAnthropicAccounts(ctx); err != nil || len(accs) != 0 {
		t.Fatalf("ListAnthropicAccounts on empty store = %v, %v; want empty, nil", accs, err)
	}

	a, err := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-xyz")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	if a.ID == "" || a.Name != "Work" || a.Kind != store.AnthropicKindAPIKey || a.Value != "sk-ant-xyz" {
		t.Fatalf("CreateAnthropicAccount = %+v; want populated Work/api_key record", a)
	}

	got, err := s.GetAnthropicAccount(ctx, a.ID)
	if err != nil || got != a {
		t.Fatalf("GetAnthropicAccount(%q) = %+v, %v; want %+v, nil", a.ID, got, err, a)
	}

	accs, err := s.ListAnthropicAccounts(ctx)
	if err != nil || len(accs) != 1 || accs[0] != a {
		t.Fatalf("ListAnthropicAccounts = %+v, %v; want [%+v], nil", accs, err, a)
	}
}

func TestAnthropicAccount_GetUnknownIsNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetAnthropicAccount(context.Background(), "anc_missing"); !store.IsAnthropicAccountNotFound(err) {
		t.Fatalf("GetAnthropicAccount(missing) error = %v; want IsAnthropicAccountNotFound", err)
	}
}

// TestAnthropicAccount_DuplicateNameIsRejected is a Review Focus case: two
// accounts sharing a name would make the create-form's name-only <select>
// genuinely ambiguous to a human picking by name.
func TestAnthropicAccount_DuplicateNameIsRejected(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-one"); err != nil {
		t.Fatalf("first CreateAnthropicAccount: %v", err)
	}
	if _, err := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-two"); !store.IsAnthropicAccountNameTaken(err) {
		t.Fatalf("duplicate-name CreateAnthropicAccount error = %v; want IsAnthropicAccountNameTaken", err)
	}
	if accs, _ := s.ListAnthropicAccounts(ctx); len(accs) != 1 {
		t.Fatalf("ListAnthropicAccounts after rejected duplicate = %d accounts; want 1", len(accs))
	}
}

func TestAnthropicAccount_FirstCreateIsAutoDefault(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if id, err := s.DefaultAnthropicAccountID(ctx); err != nil || id != "" {
		t.Fatalf("DefaultAnthropicAccountID on empty store = %q, %v; want \"\", nil", id, err)
	}

	first, err := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-one")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	if id, err := s.DefaultAnthropicAccountID(ctx); err != nil || id != first.ID {
		t.Fatalf("DefaultAnthropicAccountID after first create = %q, %v; want %q, nil", id, err, first.ID)
	}

	second, err := s.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindOAuth, "sk-ant-oat01-two")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	// A SECOND account must NOT become the default automatically.
	if id, err := s.DefaultAnthropicAccountID(ctx); err != nil || id != first.ID {
		t.Fatalf("DefaultAnthropicAccountID after second create = %q, %v; want unchanged %q, nil", id, err, first.ID)
	}
	_ = second
}

func TestAnthropicAccount_SetDefault(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-one")
	b, _ := s.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindAPIKey, "sk-ant-two")

	if err := s.SetDefaultAnthropicAccount(ctx, b.ID); err != nil {
		t.Fatalf("SetDefaultAnthropicAccount: %v", err)
	}
	if id, err := s.DefaultAnthropicAccountID(ctx); err != nil || id != b.ID {
		t.Fatalf("DefaultAnthropicAccountID = %q, %v; want %q, nil", id, err, b.ID)
	}

	if err := s.SetDefaultAnthropicAccount(ctx, "anc_missing"); !store.IsAnthropicAccountNotFound(err) {
		t.Fatalf("SetDefaultAnthropicAccount(missing) error = %v; want IsAnthropicAccountNotFound", err)
	}
	if id, _ := s.DefaultAnthropicAccountID(ctx); id != b.ID {
		t.Fatalf("DefaultAnthropicAccountID after a rejected set = %q; want unchanged %q", id, b.ID)
	}
	_ = a
}

// TestAnthropicAccount_DeleteDefaultClearsThePointer is a Review Focus case:
// a stale default pointer after the account it names is gone would silently
// resurrect a deleted account's credential the next time something resolves
// "the default".
func TestAnthropicAccount_DeleteDefaultClearsThePointer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-one")

	if err := s.DeleteAnthropicAccount(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAnthropicAccount: %v", err)
	}
	if id, err := s.DefaultAnthropicAccountID(ctx); err != nil || id != "" {
		t.Fatalf("DefaultAnthropicAccountID after deleting the default = %q, %v; want \"\", nil", id, err)
	}
	if _, err := s.GetAnthropicAccount(ctx, a.ID); !store.IsAnthropicAccountNotFound(err) {
		t.Fatalf("GetAnthropicAccount after delete error = %v; want IsAnthropicAccountNotFound", err)
	}
}

// TestAnthropicAccount_DeleteIsIdempotent matches store.Delete's (agents)
// and handleDeleteTemplate's existing convention in this codebase: removing
// an absent id is success, never an error.
func TestAnthropicAccount_DeleteIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteAnthropicAccount(context.Background(), "anc_never_existed"); err != nil {
		t.Fatalf("DeleteAnthropicAccount(never existed) = %v; want nil", err)
	}
}

func TestAnthropicAccount_DeleteNonDefaultLeavesDefaultAlone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-one")
	b, _ := s.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindAPIKey, "sk-ant-two")

	if err := s.DeleteAnthropicAccount(ctx, b.ID); err != nil {
		t.Fatalf("DeleteAnthropicAccount: %v", err)
	}
	if id, err := s.DefaultAnthropicAccountID(ctx); err != nil || id != a.ID {
		t.Fatalf("DefaultAnthropicAccountID after deleting a non-default account = %q, %v; want unchanged %q, nil", id, err, a.ID)
	}
}
