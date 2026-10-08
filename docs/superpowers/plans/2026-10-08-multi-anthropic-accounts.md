# Multiple Named Anthropic Accounts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace docker-operator's single shared Anthropic credential with a
store of named Anthropic accounts, selectable per agent, with a default that
pre-fills the create form, managed from a new "Anthropic Accounts" Settings
panel.

**Architecture:** A new BoltDB bucket (`internal/store`) holds named
accounts, CRUD'd like agent records, not like the singleton it replaces. A
new `existingAccountID` parameter threads through `resolveBackend` so Create
pins a concrete account at creation time and Update re-resolves the agent's
existing pin (never a request field). Changing an existing agent's account
goes through the existing no-recreate `Rename`/`PATCH /api/agents/{id}`
path, never through the full recreate flow. A one-time migration inside
`store.Open` promotes any pre-existing shared credential into an account
named "Default". The web UI gets a new Settings panel (list/add/remove/
set-default) and an account `<select>` in the create/update forms.

**Tech Stack:** Go (BoltDB via `go.etcd.io/bbolt`), vanilla JS web UI (no
framework), `node --test` for JS tests, Go's standard `testing` package.

**Spec:** `docs/superpowers/specs/2026-10-08-multi-anthropic-accounts-design.md`
(read this first — it has the full rationale for every decision below;
this plan implements it task by task and does not repeat the "why").

## Global Constraints

- Tokens (`AnthropicAccount.Value`) are NEVER serialized by any HTTP
  response type — every response/view type in this feature omits it by
  construction, not by a redaction step.
- No new encryption at rest: accounts are plaintext JSON in BoltDB, exactly
  like the singleton they replace (spec decision #8).
- `DeleteAnthropicAccount` is unconditional and idempotent: it never checks
  whether an agent references the account, and deleting an absent ID is
  success (matches `store.Delete`/`handleDelete`/`handleDeleteTemplate`'s
  existing convention in this codebase — verified directly, not assumed).
- A dangling account pin (the agent's pinned account was deleted) fails
  closed with the existing `ErrNoAnthropicAuth` → 409 shape the next time
  the agent needs new env vars — never a silent fallback to the current
  default (spec decision #3).
- An agent's account pin is changed ONLY through `PATCH /api/agents/{id}`
  (`Rename`, no container recreate) — never through `POST
  /api/agents/{id}/update` (`UpdateRequest` carries no `account_id` field at
  all, by construction, not by convention).
- No in-place re-authentication (spec decision #6): there is no "refresh
  this account's token" action anywhere in this plan.
- `internal/agent/anthropiclogin.go` and its two routes
  (`GET/POST/DELETE /api/anthropic/login`) are NOT touched by this plan at
  all (spec's "Login / add-account flow" section).
- After any edit to `docker-operator/web/*`, run `make sync-web-embed` and
  `make web-embed-check` before considering that task done (committed-copy
  rule in `docker-operator/CLAUDE.md`).
- Never run `go`/`make`/`node` directly on the agent container — every
  command below runs inside a Docker container against the DinD daemon (see
  the `use-docker` skill); the `go vet`/`golangci-lint` commands shown use
  the project's existing Go/lint container conventions.

## Review Focus

- **Zero accounts configured, create a `backend=anthropic` agent:** must
  fail with the existing 409 `no_anthropic_auth` shape, not a panic or a
  500 from an empty-string account lookup. (Task 3's tests.)
- **`PATCH /api/agents/{id} {account_id}` on a `backend=ollama` agent:**
  must 400 with the record completely unchanged — a careless implementation
  could silently pin an ollama agent to an account it will never use.
  (Task 4's tests.)
- **Migration idempotency across a second `store.Open`:** opening an
  already-migrated store (or a store where the user already created their
  own accounts) must never create a second "Default" account or re-pin an
  agent the user already repointed elsewhere. (Task 2's tests.)
- **Duplicate account name on create:** must 409, not silently create two
  accounts sharing a name (which would make the create-form `<select>`
  genuinely ambiguous to a human picking by name). (Task 1's tests.)
- **Deleting the current default account:** `DefaultAnthropicAccountID`
  must read back `""` afterward, not the deleted ID — a stale pointer here
  would silently resurrect a deleted account's credential the next time
  some code resolves "the default". (Task 1's tests.)

---

## Task 1: Store — `AnthropicAccount` CRUD and the default-account setting

**Files:**
- Create: `docker-operator/internal/store/anthropicaccount.go`
- Create: `docker-operator/internal/store/anthropicaccount_test.go`
- Modify: `docker-operator/internal/store/store.go:291-304` (bucket list and
  key vars)

**Interfaces:**
- Produces (consumed by Tasks 2, 3, 4, 5):
  ```go
  type AnthropicAccount struct {
      ID        string
      Name      string
      Kind      string // store.AnthropicKindAPIKey | store.AnthropicKindOAuth
      Value     string
      CreatedAt time.Time
      UpdatedAt time.Time
  }
  var ErrAnthropicAccountNotFound error
  var ErrAnthropicAccountNameTaken error
  func IsAnthropicAccountNotFound(err error) bool
  func IsAnthropicAccountNameTaken(err error) bool
  func (s *Store) CreateAnthropicAccount(ctx context.Context, name, kind, value string) (AnthropicAccount, error)
  func (s *Store) GetAnthropicAccount(ctx context.Context, id string) (AnthropicAccount, error)
  func (s *Store) ListAnthropicAccounts(ctx context.Context) ([]AnthropicAccount, error)
  func (s *Store) DeleteAnthropicAccount(ctx context.Context, id string) error
  func (s *Store) DefaultAnthropicAccountID(ctx context.Context) (string, error)
  func (s *Store) SetDefaultAnthropicAccount(ctx context.Context, id string) error
  ```

- [ ] **Step 1: Add the new bucket and settings key to `store.go`**

In `internal/store/store.go`, change the var block at lines 291-304 from:

```go
var (
	bucketAgents   = []byte("agents")
	bucketSettings = []byte("settings")

	keySettingsAnthropicAuth  = []byte("anthropic_auth")
	keySettingsAgentImageTags = []byte("agent_image_tags")
```

to:

```go
var (
	bucketAgents            = []byte("agents")
	bucketSettings          = []byte("settings")
	bucketAnthropicAccounts = []byte("anthropic_accounts")

	// keySettingsAnthropicAuth is the LEGACY single shared credential this
	// feature replaces. It is read (and deleted) exactly once, by
	// migrateLegacyAnthropicAuth in Open -- nothing else reads or writes it.
	keySettingsAnthropicAuth = []byte("anthropic_auth")
	// keySettingsDefaultAnthropicAccount holds the id (a bucketAnthropicAccounts
	// key) of the account that pre-fills the create form. Empty/missing means
	// no default is set.
	keySettingsDefaultAnthropicAccount = []byte("default_anthropic_account_id")
	keySettingsAgentImageTags          = []byte("agent_image_tags")
```

Then change the `Open` function's bucket-creation loop (around line 392) from:

```go
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates} {
```

to:

```go
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates, bucketAnthropicAccounts} {
```

Leave everything else in `store.go` (including the `AnthropicAuth` type and
`GetAnthropicAuth`/`SetAnthropicAuth`/`ClearAnthropicAuth` methods) in place
for now — Task 2 removes them, once migration no longer needs them AND
every caller has moved off them (Tasks 3/4).

- [ ] **Step 2: Write the failing tests for account CRUD**

Create `internal/store/anthropicaccount_test.go`:

```go
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
```

- [ ] **Step 2b: Run the tests to verify they fail**

Run (from `docker-operator/`, inside the Go container per the `use-docker`
skill):

```sh
go test ./internal/store/... -run TestAnthropicAccount -v
```

Expected: compile failure — `AnthropicAccount`, `CreateAnthropicAccount`,
etc. do not exist yet.

- [ ] **Step 3: Implement `internal/store/anthropicaccount.go`**

```go
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"go.etcd.io/bbolt"
)

// ErrAnthropicAccountNotFound reports that no anthropic account with the
// given ID exists. GetAnthropicAccount and SetDefaultAnthropicAccount wrap
// it; callers test with IsAnthropicAccountNotFound.
//
// DeleteAnthropicAccount deliberately does NOT return it: removing an
// account that is already gone is success, matching this store's existing
// idempotent-delete convention (see Store.Delete's own doc comment).
var ErrAnthropicAccountNotFound = errors.New("anthropic account not found")

// ErrAnthropicAccountNameTaken reports that CreateAnthropicAccount was given
// a name another stored account already has. Account names are shown
// without their IDs in the UI (a create-form <select> by name, a Settings
// list), so two accounts sharing a name would be genuinely ambiguous to
// pick between.
var ErrAnthropicAccountNameTaken = errors.New("an anthropic account with that name already exists")

// IsAnthropicAccountNotFound reports whether err was caused by an unknown
// anthropic account ID.
func IsAnthropicAccountNotFound(err error) bool { return errors.Is(err, ErrAnthropicAccountNotFound) }

// IsAnthropicAccountNameTaken reports whether err was caused by a duplicate
// anthropic account name.
func IsAnthropicAccountNameTaken(err error) bool { return errors.Is(err, ErrAnthropicAccountNameTaken) }

// AnthropicAccount is one named, stored Anthropic credential. Unlike the
// legacy AnthropicAuth singleton it replaces, a store can hold any number of
// these; store.Agent.AnthropicAccountID pins one agent to one of them.
//
// Value is a secret: the store returns it (internal/agent needs the
// plaintext to put on a container's environment), but no layer above
// internal/agent ever serializes it to a client -- see
// docs/superpowers/specs/2026-10-08-multi-anthropic-accounts-design.md's
// "No new encryption-at-rest" decision for why this is a continuation of,
// not a new risk beyond, the singleton's existing trust boundary.
type AnthropicAccount struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // AnthropicKindAPIKey | AnthropicKindOAuth
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// anthropicAccountIDPrefix marks a value as an anthropic account ID
// wherever it turns up out of context, mirroring idPrefix for agent IDs.
const anthropicAccountIDPrefix = "anc_"

// newAnthropicAccountID returns a fresh account ID of the form
// "anc_7f3a9c2d", mirroring NewID's shape and randomness for agent IDs.
func newAnthropicAccountID() (string, error) {
	var b [idBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating an anthropic account id: %w", err)
	}
	return anthropicAccountIDPrefix + hex.EncodeToString(b[:]), nil
}

// CreateAnthropicAccount stores a new named Anthropic credential. name must
// be unique among existing accounts (IsAnthropicAccountNameTaken otherwise);
// kind must be AnthropicKindAPIKey or AnthropicKindOAuth; value must be
// non-empty. If this is the first account ever created, it is automatically
// also made the default (SetDefaultAnthropicAccount is otherwise the only
// way to change the default) -- otherwise a create form would have nothing
// to pre-fill until a human opened Settings and picked one.
func (s *Store) CreateAnthropicAccount(ctx context.Context, name, kind, value string) (AnthropicAccount, error) {
	if err := ctx.Err(); err != nil {
		return AnthropicAccount{}, err
	}
	if name == "" {
		return AnthropicAccount{}, errors.New("creating an anthropic account: the name must not be empty")
	}
	if !ValidAnthropicKind(kind) {
		return AnthropicAccount{}, fmt.Errorf("creating an anthropic account: %q is not a valid kind (want %q or %q)", kind, AnthropicKindAPIKey, AnthropicKindOAuth)
	}
	if value == "" {
		return AnthropicAccount{}, errors.New("creating an anthropic account: the value must not be empty")
	}

	id, err := newAnthropicAccountID()
	if err != nil {
		return AnthropicAccount{}, fmt.Errorf("creating an anthropic account: %w", err)
	}
	now := s.now()
	account := AnthropicAccount{ID: id, Name: name, Kind: kind, Value: value, CreatedAt: now, UpdatedAt: now}

	err = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAnthropicAccounts)
		if b == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketAnthropicAccounts, s.path)
		}
		first := true
		if err := b.ForEach(func(_, v []byte) error {
			first = false
			var existing AnthropicAccount
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == name {
				return ErrAnthropicAccountNameTaken
			}
			return nil
		}); err != nil {
			return err
		}
		raw, err := json.Marshal(account)
		if err != nil {
			return fmt.Errorf("encoding anthropic account %q: %w", name, err)
		}
		if err := b.Put([]byte(id), raw); err != nil {
			return fmt.Errorf("writing anthropic account %q: %w", name, err)
		}
		if first {
			sb := tx.Bucket(bucketSettings)
			if sb == nil {
				return fmt.Errorf("the %q bucket is missing from the state database %q", bucketSettings, s.path)
			}
			if err := sb.Put(keySettingsDefaultAnthropicAccount, []byte(id)); err != nil {
				return fmt.Errorf("setting the first anthropic account as default: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return AnthropicAccount{}, fmt.Errorf("creating anthropic account %q: %w", name, err)
	}
	return account, nil
}

// GetAnthropicAccount returns the account with the given ID, or an error
// satisfying IsAnthropicAccountNotFound.
func (s *Store) GetAnthropicAccount(ctx context.Context, id string) (AnthropicAccount, error) {
	if err := ctx.Err(); err != nil {
		return AnthropicAccount{}, err
	}
	var account AnthropicAccount
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAnthropicAccounts)
		if b == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketAnthropicAccounts, s.path)
		}
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting anthropic account %q: %w", id, ErrAnthropicAccountNotFound)
		}
		return json.Unmarshal(raw, &account)
	})
	if err != nil {
		return AnthropicAccount{}, err
	}
	return account, nil
}

// ListAnthropicAccounts returns every stored account, in bbolt's key
// (ID) order -- NOT sorted by name. A caller that displays these by name
// (the web UI) sorts for display itself, the same division of labor
// ListTemplates/renderTemplateBar already use for templates. The slice is
// never nil, so an empty store encodes as JSON "[]", not null.
func (s *Store) ListAnthropicAccounts(ctx context.Context) ([]AnthropicAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	accounts := make([]AnthropicAccount, 0)
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAnthropicAccounts)
		if b == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketAnthropicAccounts, s.path)
		}
		return b.ForEach(func(_, v []byte) error {
			var a AnthropicAccount
			if err := json.Unmarshal(v, &a); err != nil {
				return err
			}
			accounts = append(accounts, a)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing anthropic accounts: %w", err)
	}
	return accounts, nil
}

// DeleteAnthropicAccount removes the account and, if it was the default,
// clears the default (DefaultAnthropicAccountID then reads back ""). Like
// Store.Delete for agents and handleDeleteTemplate for templates, removing
// an account that does not exist is success, not an error -- this method
// does NOT check whether any agent is currently pinned to id (that is the
// explicit, deliberate "delete is unconditional" decision: see the spec).
func (s *Store) DeleteAnthropicAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAnthropicAccounts)
		if b == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketAnthropicAccounts, s.path)
		}
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("deleting anthropic account %q: %w", id, err)
		}
		sb := tx.Bucket(bucketSettings)
		if sb == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketSettings, s.path)
		}
		if string(sb.Get(keySettingsDefaultAnthropicAccount)) == id {
			if err := sb.Delete(keySettingsDefaultAnthropicAccount); err != nil {
				return fmt.Errorf("clearing the default after deleting it: %w", err)
			}
		}
		return nil
	})
}

// DefaultAnthropicAccountID returns the id of the account that pre-fills the
// create form, or "" if none is set (a fresh store, or the default account
// was since deleted).
func (s *Store) DefaultAnthropicAccountID(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var id string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSettings)
		if b == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketSettings, s.path)
		}
		id = string(b.Get(keySettingsDefaultAnthropicAccount))
		return nil
	})
	return id, err
}

// SetDefaultAnthropicAccount marks id as the default account.
// IsAnthropicAccountNotFound if no such account exists -- unlike delete,
// this IS validated: pointing the default at nothing would leave the
// create form with nothing to pre-fill and no visible error.
func (s *Store) SetDefaultAnthropicAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return errors.New("setting the default anthropic account: the id must not be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		ab := tx.Bucket(bucketAnthropicAccounts)
		if ab == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketAnthropicAccounts, s.path)
		}
		if ab.Get([]byte(id)) == nil {
			return fmt.Errorf("setting the default anthropic account: %w: %q", ErrAnthropicAccountNotFound, id)
		}
		sb := tx.Bucket(bucketSettings)
		if sb == nil {
			return fmt.Errorf("the %q bucket is missing from the state database %q", bucketSettings, s.path)
		}
		return sb.Put(keySettingsDefaultAnthropicAccount, []byte(id))
	})
}
```

Add `"time"` to this file's import block (used by `AnthropicAccount`'s
`CreatedAt`/`UpdatedAt` fields).

- [ ] **Step 4: Run the tests to verify they pass**

```sh
go test ./internal/store/... -run TestAnthropicAccount -v
gofmt -l internal/store/anthropicaccount.go internal/store/anthropicaccount_test.go
```

Expected: every `TestAnthropicAccount_*` test PASSes; `gofmt -l` prints
nothing (no unformatted files).

- [ ] **Step 5: Commit**

```sh
git add internal/store/anthropicaccount.go internal/store/anthropicaccount_test.go internal/store/store.go
git commit -m "$(cat <<'EOF'
store: add named Anthropic account CRUD and a default-account setting

Adds a new bucketAnthropicAccounts, keyed like agent records (not the
settings-bucket singleton it will replace), plus a
default_anthropic_account_id settings key. CreateAnthropicAccount
auto-defaults the first account ever created; delete is idempotent and
unconditional, matching this store's existing conventions for agents
and templates.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Store — `Agent.AnthropicAccountID` and migrating the legacy singleton

**Files:**
- Modify: `docker-operator/internal/store/store.go` (the `Agent` and
  `CreateSpec` structs, `Create`'s struct literal, and `Open`)
- Create: `docker-operator/internal/store/migration_test.go`

**Interfaces:**
- Consumes: Task 1's `AnthropicAccount`, `bucketAnthropicAccounts`,
  `keySettingsDefaultAnthropicAccount`, `newAnthropicAccountID`.
- Produces (consumed by Tasks 3, 4, 5, 6, 9):
  ```go
  // Agent gains:
  AnthropicAccountID string `json:"anthropic_account_id,omitempty"`
  // CreateSpec gains:
  AnthropicAccountID string
  ```
  `Open` now runs the migration automatically.

**IMPORTANT — this task does NOT remove the old `AnthropicAuth` surface.**
`GetAnthropicAuth`/`SetAnthropicAuth`/`ClearAnthropicAuth` and the
`AnthropicAuth` type stay in `store.go`, untouched, through Tasks 2, 3 and 4
— `internal/agent` (Task 3, Task 4) and `internal/store/store_test.go`'s
existing tests still call them, and removing them here would break both
until every caller moves off, which doesn't finish until Task 4. The
removal is entirely **Task 5's** responsibility (its own step, covering
`store.go`, `internal/agent/query.go` and `internal/store/store_test.go`
together in one commit) — see Task 5's new removal step. This task only
ADDS: the new `Agent`/`CreateSpec` field (needed by this task's own
migration code) and the migration itself.

- [ ] **Step 1: Add `AnthropicAccountID` to `store.Agent` and `store.CreateSpec`**

In `internal/store/store.go`'s `Agent` struct (around line 234, right after
the `WorkspaceVolume`/`ClaudeConfigVolume`/`DindCacheVolume` block), add:

```go
	// AnthropicAccountID pins this agent to one stored AnthropicAccount
	// (empty for a backend=ollama agent). Resolved and stamped once at
	// create time (Manager.resolveBackend, Task 3), and changed afterward
	// ONLY through Manager.Rename's PATCH path (Task 4) -- never through
	// Update's full container recreate. See
	// docs/superpowers/specs/2026-10-08-multi-anthropic-accounts-design.md.
	AnthropicAccountID string `json:"anthropic_account_id,omitempty"`
```

In `CreateSpec` (around line 276-285), add `AnthropicAccountID string` to the
field list, with a one-line comment mirroring `Backend`'s own ("recorded on
the new agent verbatim"). In `Store.Create`'s agent-literal construction
(around line 457-474), add `AnthropicAccountID: spec.AnthropicAccountID,` to
the struct literal.

This field is added HERE, in Task 2, rather than in Task 3 (which is where
it is actually consumed by `resolveBackend`) because this task's own
migration code (Step 4 below) needs to read and write it to pin pre-existing
agents onto the migrated "Default" account — Task 3 only ever reads a field
that already exists by the time it runs.

- [ ] **Step 2: Write the failing migration tests**

Create `internal/store/migration_test.go`:

```go
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
```

- [ ] **Step 3: Run the tests to verify they fail**

```sh
go test ./internal/store/... -run TestMigrateLegacyAnthropicAuth -v
```

Expected: FAIL — `AnthropicAccountID` doesn't exist on `Agent`/`CreateSpec`
yet if Step 1 wasn't done first (compile failure), or, once Step 1 is done,
migration doesn't exist yet so `ListAnthropicAccounts` after seeding legacy
data returns 0 accounts, not 1.

- [ ] **Step 4: Add the migration to `Open`**

In `internal/store/store.go`, change the `Open` function's bucket-creation
block from:

```go
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates, bucketAnthropicAccounts} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("creating the %q bucket: %w", name, err)
			}
		}
		return nil
	}); err != nil {
```

to:

```go
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates, bucketAnthropicAccounts} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("creating the %q bucket: %w", name, err)
			}
		}
		return migrateLegacyAnthropicAuth(tx, func() time.Time { return time.Now().UTC() })
	}); err != nil {
```

Then add this function to `internal/store/anthropicaccount.go` (it belongs
next to the type it populates, not in `store.go`):

```go
// migrateLegacyAnthropicAuth is a one-time step, run inside Open's own
// bucket-creation transaction: it promotes the pre-accounts single shared
// Anthropic credential (bucketSettings[keySettingsAnthropicAuth]) into the
// first AnthropicAccount, named "Default" and marked default, then pins
// every existing backend=anthropic agent that has no AnthropicAccountID yet
// onto it. A store that already holds ANY account -- this one, from a
// previous Open, or one the user created independently -- skips this
// entirely, so it runs at most once ever, even across repeated restarts.
func migrateLegacyAnthropicAuth(tx *bbolt.Tx, now func() time.Time) error {
	accounts := tx.Bucket(bucketAnthropicAccounts)
	settings := tx.Bucket(bucketSettings)
	agents := tx.Bucket(bucketAgents)

	if k, _ := accounts.Cursor().First(); k != nil {
		return nil // already migrated, or the user already created an account
	}
	raw := settings.Get(keySettingsAnthropicAuth)
	if raw == nil {
		return nil // fresh store, nothing to migrate
	}
	var legacy struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return fmt.Errorf("decoding the legacy Anthropic credential: %w", err)
	}

	id, err := newAnthropicAccountID()
	if err != nil {
		return fmt.Errorf("generating the migrated account id: %w", err)
	}
	t := now()
	account := AnthropicAccount{ID: id, Name: "Default", Kind: legacy.Kind, Value: legacy.Value, CreatedAt: t, UpdatedAt: t}
	accountRaw, err := json.Marshal(account)
	if err != nil {
		return fmt.Errorf("encoding the migrated anthropic account: %w", err)
	}
	if err := accounts.Put([]byte(id), accountRaw); err != nil {
		return fmt.Errorf("writing the migrated anthropic account: %w", err)
	}
	if err := settings.Put(keySettingsDefaultAnthropicAccount, []byte(id)); err != nil {
		return fmt.Errorf("setting the migrated account as default: %w", err)
	}
	if err := settings.Delete(keySettingsAnthropicAuth); err != nil {
		return fmt.Errorf("removing the legacy anthropic credential: %w", err)
	}

	// Pin every agent that was implicitly using the shared credential.
	// Collected first, applied after: bbolt forbids mutating a bucket while
	// ForEach is iterating it.
	type pin struct{ key, value []byte }
	var pins []pin
	if err := agents.ForEach(func(k, v []byte) error {
		var a Agent
		if err := json.Unmarshal(v, &a); err != nil {
			return fmt.Errorf("decoding agent %q during anthropic-account migration: %w", k, err)
		}
		// The literal "anthropic" (not config.BackendAnthropic): this
		// package depends on no other internal package, the same reason
		// harnessOpenCode above duplicates config.HarnessOpenCode's value.
		if a.Backend != "anthropic" || a.AnthropicAccountID != "" {
			return nil
		}
		a.AnthropicAccountID = id
		raw, err := json.Marshal(a)
		if err != nil {
			return fmt.Errorf("encoding agent %q during anthropic-account migration: %w", k, err)
		}
		// k is only valid for the duration of this callback -- bbolt reuses
		// the underlying buffer across ForEach -- so copy it before saving.
		pins = append(pins, pin{key: append([]byte(nil), k...), value: raw})
		return nil
	}); err != nil {
		return err
	}
	for _, p := range pins {
		if err := agents.Put(p.key, p.value); err != nil {
			return fmt.Errorf("pinning a migrated agent onto the Default anthropic account: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```sh
go test ./internal/store/... -v
gofmt -l internal/store/
```

Expected: every test in `internal/store` PASSes, including every pre-existing
one (the `AnthropicAuth` surface is untouched so far) and every new
`TestMigrateLegacyAnthropicAuth_*`/`TestAnthropicAccount_*` test.

- [ ] **Step 6: Commit**

```sh
git add internal/store/anthropicaccount.go internal/store/migration_test.go internal/store/store.go
git commit -m "$(cat <<'EOF'
store: agent account pin field, and migrate the legacy shared credential

Agent/CreateSpec gain AnthropicAccountID. One-time, idempotent step
inside Open: promotes the pre-accounts single credential into an
account named "Default", marks it default, and pins every
pre-existing backend=anthropic agent onto it. The old AnthropicAuth
surface stays in place -- its removal is Task 5's responsibility, once
every caller (Tasks 3 and 4) has moved off it.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Agent — account-aware `resolveBackend`, `CreateRequest.AccountID`

**Files:**
- Modify: `docker-operator/internal/agent/create.go`
- Modify: `docker-operator/internal/agent/update.go:119`
- Modify: `docker-operator/internal/agent/reconcile.go:471-481`
  (`resolveBackendFromAgent`, the wake-agent path's direct
  `resolveBackend` caller — confirmed by direct inspection, not a maybe)
- Modify: `docker-operator/internal/agent/backend_test.go`
- Modify: `docker-operator/internal/agent/harness_test.go` (confirmed: seeds
  `SetAnthropicAuth` and calls `resolveSpec` with 2 args, at lines 21, 32,
  45-48, 58-59, 65, 73, 145)
- Modify: `docker-operator/internal/agent/create_test.go` (if it seeds a
  `store.Store` via `SetAnthropicAuth` anywhere — replace with
  `CreateAnthropicAccount`)
- Modify: `docker-operator/internal/agent/update_test.go` (confirmed: seeds
  `SetAnthropicAuth` at line 245, calls `resolveBackend` directly with 2
  args at lines 263 and 467)

**Interfaces:**
- Consumes: Task 1's `store.CreateAnthropicAccount`,
  `store.GetAnthropicAccount`, `store.DefaultAnthropicAccountID`,
  `store.IsAnthropicAccountNotFound`.
- Produces (consumed by Task 4, Task 5):
  ```go
  // CreateRequest gains:
  AccountID string // new field

  var ErrUnknownAnthropicAccount error
  func IsUnknownAnthropicAccount(err error) bool

  // resolvedBackend gains:
  accountID string // anthropic only

  func (m *Manager) resolveBackend(ctx context.Context, req CreateRequest, existingAccountID string) (resolvedBackend, error)
  func (m *Manager) resolveSpec(ctx context.Context, req CreateRequest, existingAccountID string) (resolvedSpec, error)
  ```
  `store.CreateSpec.AnthropicAccountID` and
  `store.Agent.AnthropicAccountID` already exist — Task 2 added both (its
  own migration code needs them before this task runs).

This task also fixes the THIRD caller of `resolveBackend` that is easy to
miss: `internal/agent/reconcile.go`'s `resolveBackendFromAgent` (used by the
periodic-reconcile/wake-agent path) calls `resolveBackend` directly, not
through `resolveSpec`. A grep for every direct caller BEFORE you start
confirms the complete list this task must update:

```sh
grep -rn "resolveBackend(ctx\|resolveSpec(ctx" internal/agent/*.go
```

As of this plan's writing that prints exactly four non-test call sites —
`create.go` (the `resolveSpec`→`resolveBackend` chain, and `Create`'s own
call to `resolveSpec`), `update.go`'s call to `resolveSpec`, and
`reconcile.go:478`'s direct call to `resolveBackend` — plus test-file
callers in `backend_test.go`, `harness_test.go`, `update_test.go`. All of
them are addressed by name in the steps below; if your grep finds a FIFTH
non-test call site this plan does not mention, treat that as a `NEEDS_CONTEXT`
condition and report it rather than guessing at the right `existingAccountID`
to pass.

- [ ] **Step 1: Write the failing `resolveBackend` tests**

In `internal/agent/backend_test.go`, add (adjust the existing test file's
helper names — e.g. its own store-opening helper — to match what is
actually there; the shape below is what each test must assert regardless of
helper names):

```go
func TestResolveBackend_AnthropicNoAccountsIs409Shape(t *testing.T) {
	m := newTestManager(t) // use this file's existing manager-construction helper
	_, err := m.resolveBackend(context.Background(), CreateRequest{Backend: "anthropic"}, "")
	if !IsNoAnthropicAuth(err) {
		t.Fatalf("resolveBackend with no accounts error = %v; want IsNoAnthropicAuth", err)
	}
}

func TestResolveBackend_AnthropicExplicitAccountID(t *testing.T) {
	m := newTestManager(t)
	a, err := m.store.CreateAnthropicAccount(context.Background(), "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	b, err := m.store.CreateAnthropicAccount(context.Background(), "Personal", store.AnthropicKindAPIKey, "sk-ant-personal")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}

	rb, err := m.resolveBackend(context.Background(), CreateRequest{Backend: "anthropic", AccountID: b.ID}, "")
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if rb.accountID != b.ID || rb.apiKey != "sk-ant-personal" {
		t.Fatalf("resolveBackend(explicit %q) = %+v; want accountID=%q apiKey=sk-ant-personal", b.ID, rb, b.ID)
	}
	_ = a
}

func TestResolveBackend_AnthropicExplicitUnknownAccountID(t *testing.T) {
	m := newTestManager(t)
	_, err := m.resolveBackend(context.Background(), CreateRequest{Backend: "anthropic", AccountID: "anc_missing"}, "")
	if !IsUnknownAnthropicAccount(err) {
		t.Fatalf("resolveBackend(unknown explicit account) error = %v; want IsUnknownAnthropicAccount", err)
	}
}

func TestResolveBackend_AnthropicOmittedUsesDefault(t *testing.T) {
	m := newTestManager(t)
	a, err := m.store.CreateAnthropicAccount(context.Background(), "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	rb, err := m.resolveBackend(context.Background(), CreateRequest{Backend: "anthropic"}, "")
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if rb.accountID != a.ID {
		t.Fatalf("resolveBackend(omitted) accountID = %q; want the default %q", rb.accountID, a.ID)
	}
}

// TestResolveBackend_PinAtCreationSurvivesDefaultChange is a Review Focus
// case for spec decision #7: changing the default afterward must not move
// an already-resolved pin.
func TestResolveBackend_PinAtCreationSurvivesDefaultChange(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	a, _ := m.store.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	b, _ := m.store.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindAPIKey, "sk-ant-personal")

	rb, err := m.resolveBackend(ctx, CreateRequest{Backend: "anthropic"}, "")
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if rb.accountID != a.ID {
		t.Fatalf("first resolveBackend accountID = %q; want %q", rb.accountID, a.ID)
	}

	if err := m.store.SetDefaultAnthropicAccount(ctx, b.ID); err != nil {
		t.Fatalf("SetDefaultAnthropicAccount: %v", err)
	}
	// existingAccountID simulates an Update call for the agent that was
	// already pinned to `a` -- it must NOT follow the new default.
	rb2, err := m.resolveBackend(ctx, CreateRequest{Backend: "anthropic"}, a.ID)
	if err != nil {
		t.Fatalf("second resolveBackend: %v", err)
	}
	if rb2.accountID != a.ID {
		t.Fatalf("resolveBackend with existingAccountID=%q accountID = %q; want unchanged %q, not the new default %q", a.ID, rb2.accountID, a.ID, b.ID)
	}
}

// TestResolveBackend_DanglingExistingAccountFailsClosed is a Review Focus
// case for spec decision #3.
func TestResolveBackend_DanglingExistingAccountFailsClosed(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	a, _ := m.store.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	if err := m.store.DeleteAnthropicAccount(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAnthropicAccount: %v", err)
	}
	_, err := m.resolveBackend(ctx, CreateRequest{Backend: "anthropic"}, a.ID)
	if !IsNoAnthropicAuth(err) {
		t.Fatalf("resolveBackend with a deleted existingAccountID error = %v; want IsNoAnthropicAuth (fail closed)", err)
	}
}
```

If `backend_test.go` does not already have a `newTestManager(t) *Manager`
helper that wires a real `*store.Store` (via `store.Open` against
`t.TempDir()`) into a `Manager`, read the file first to find whatever
existing helper constructs a `Manager` for its current tests (it already
has tests calling `m.resolveBackend`, per the file's existence) and reuse
that exact helper name instead of inventing `newTestManager` — the name
above is illustrative, not prescriptive.

- [ ] **Step 2: Run the tests to verify they fail**

```sh
go test ./internal/agent/... -run TestResolveBackend -v
```

Expected: compile failure (`resolveBackend` takes 2 args today, these calls
pass 3; `CreateRequest.AccountID` does not exist; `ErrUnknownAnthropicAccount`/
`IsUnknownAnthropicAccount` do not exist).

- [ ] **Step 3: Implement the account-aware `resolveBackend`**

In `internal/agent/create.go`:

1. Add to the sentinel-error block (near `ErrNoAnthropicAuth`, around line
   27):

```go
// ErrUnknownAnthropicAccount is returned by Create for an explicit
// CreateRequest.AccountID that names no stored account, and by Rename for a
// PATCH account_id that names no stored account. internal/api maps it to a
// 400.
var ErrUnknownAnthropicAccount = errors.New("unknown anthropic account")
```

and to the `Is*` block (around line 63-72):

```go
func IsUnknownAnthropicAccount(err error) bool { return errors.Is(err, ErrUnknownAnthropicAccount) }
```

2. Add to `CreateRequest` (end of the field list, after `AutoMode`, around
   line 401):

```go
	// AccountID pins this one agent to a specific stored Anthropic account
	// (store.AnthropicAccount.ID). Only meaningful for the anthropic
	// backend; ignored for ollama. Empty means "use the operator's current
	// default account" -- Create resolves and stamps a CONCRETE id onto the
	// record at creation time (never "follow the default forever"). An
	// existing agent's account is changed through Rename instead -- Update
	// never reads this field (see resolveBackend's existingAccountID
	// parameter).
	AccountID string
```

3. Add `accountID string // anthropic only` to the `resolvedBackend` struct
   (around line 79-87, next to `apiKey`/`oauthToken`).

4. Replace `resolveBackend` (lines 559-598) with:

```go
// resolveBackend turns a CreateRequest's backend fields + the operator
// config + the resolved Anthropic account into a resolvedBackend, or an
// error the caller can map to a 4xx (ErrInvalidBackend, ErrNoAnthropicAuth,
// ErrUnknownAnthropicAccount).
//
// existingAccountID is the account already pinned on the record, or "" when
// there is none yet: Create always passes "" (no record exists yet); Update
// passes the record's current AnthropicAccountID, so an in-place update
// NEVER changes which account an agent is pinned to -- that happens through
// Rename's separate, no-recreate PATCH path instead -- except when the
// update is switching the agent from ollama to anthropic for the first
// time, where there is no existing pin to preserve and this falls through
// to the same default-resolution Create uses.
func (m *Manager) resolveBackend(ctx context.Context, req CreateRequest, existingAccountID string) (resolvedBackend, error) {
	kind := req.Backend
	if kind == "" {
		kind = m.cfg.DefaultBackend
	}
	if !config.ValidBackend(kind) {
		return resolvedBackend{}, fmt.Errorf("%w: %q", ErrInvalidBackend, kind)
	}

	rb := resolvedBackend{kind: kind}
	switch kind {
	case config.BackendOllama:
		rb.model = firstNonEmpty(req.Model, m.cfg.AgentModel)
		rb.fastModel = firstNonEmpty(req.FastModel, m.cfg.AgentFastModel)
		rb.ollamaURL = firstNonEmpty(req.OllamaURL, m.cfg.OllamaURL)
		if rb.ollamaURL != "" && !config.ValidOllamaURL(rb.ollamaURL) {
			return resolvedBackend{}, fmt.Errorf("%w: %q", ErrInvalidOllamaURL, rb.ollamaURL)
		}
	case config.BackendAnthropic:
		accountID := existingAccountID
		fromRequest := false
		if accountID == "" {
			accountID = req.AccountID
			fromRequest = accountID != ""
		}
		if accountID == "" {
			id, err := m.store.DefaultAnthropicAccountID(ctx)
			if err != nil {
				return resolvedBackend{}, fmt.Errorf("reading the default Anthropic account: %w", err)
			}
			accountID = id
		}
		if accountID == "" {
			return resolvedBackend{}, ErrNoAnthropicAuth
		}
		account, err := m.store.GetAnthropicAccount(ctx, accountID)
		if err != nil {
			if store.IsAnthropicAccountNotFound(err) {
				if fromRequest {
					return resolvedBackend{}, fmt.Errorf("%w: %q", ErrUnknownAnthropicAccount, accountID)
				}
				// existingAccountID (or the resolved default) no longer
				// exists -- the dangling-pin case: fail exactly like "no
				// credential configured" rather than silently falling back
				// to whatever the CURRENT default happens to be.
				return resolvedBackend{}, fmt.Errorf("%w: the pinned account %q no longer exists", ErrNoAnthropicAuth, accountID)
			}
			return resolvedBackend{}, fmt.Errorf("reading the Anthropic account %q: %w", accountID, err)
		}
		rb.accountID = accountID
		switch account.Kind {
		case store.AnthropicKindAPIKey:
			rb.apiKey = account.Value
		case store.AnthropicKindOAuth:
			rb.oauthToken = account.Value
		default:
			return resolvedBackend{}, fmt.Errorf("the Anthropic account %q has an unknown kind %q", accountID, account.Kind)
		}
	}
	return rb, nil
}
```

5. Change `resolveSpec`'s signature (line 504) from
   `func (m *Manager) resolveSpec(ctx context.Context, req CreateRequest) (resolvedSpec, error)`
   to
   `func (m *Manager) resolveSpec(ctx context.Context, req CreateRequest, existingAccountID string) (resolvedSpec, error)`,
   and its one call to `resolveBackend` (line 507) from
   `rb, err := m.resolveBackend(ctx, req)` to
   `rb, err := m.resolveBackend(ctx, req, existingAccountID)`.

6. In `Create` (line 453), change `rs, err := m.resolveSpec(ctx, req)` to
   `rs, err := m.resolveSpec(ctx, req, "")`, and add
   `AnthropicAccountID: rs.rb.accountID,` to the `store.CreateSpec{...}`
   literal (around line 467-475).

- [ ] **Step 4: Wire `Update` to preserve the existing pin**

In `internal/agent/update.go`:

1. Line 119: change `rs, err := m.resolveSpec(ctx, req.CreateRequest)` to
   `rs, err := m.resolveSpec(ctx, req.CreateRequest, a.AnthropicAccountID)`
   (`a` is already in scope from the `store.Get` call at line 111).

2. In the second `store.Update` mutator (lines 205-227, the one that
   rewrites `ag.Backend`/`ag.Model`/etc.), add one line:
   `ag.AnthropicAccountID = rs.rb.accountID`. This is correct unconditionally
   — `rs.rb.accountID` is only ever non-empty when `rs.rb.kind` is
   `anthropic` (the ollama branch of `resolveBackend` never sets it), so an
   agent switched to `ollama` correctly gets `""` here.

- [ ] **Step 5: Wire `reconcile.go`'s wake-agent path, and fix every other
      caller this signature change breaks**

In `internal/agent/reconcile.go`, `resolveBackendFromAgent` (lines 471-481)
re-derives a resolvedBackend for `wakeAgent` — the one path that recreates
an agent container with NO caller-supplied `CreateRequest`, from the
record alone. It must preserve the record's OWN pin, exactly like `Update`
does, not resolve a fresh one — otherwise a routine wake-up would silently
re-pin the agent onto whatever is CURRENTLY default, which is precisely the
bug spec decision #7 exists to prevent. Change:

```go
func (m *Manager) resolveBackendFromAgent(ctx context.Context, a store.Agent) (resolvedBackend, error) {
	return m.resolveBackend(ctx, CreateRequest{
		Backend: a.Backend, Model: a.Model, FastModel: a.FastModel, OllamaURL: a.OllamaURL,
	})
}
```

to:

```go
func (m *Manager) resolveBackendFromAgent(ctx context.Context, a store.Agent) (resolvedBackend, error) {
	return m.resolveBackend(ctx, CreateRequest{
		Backend: a.Backend, Model: a.Model, FastModel: a.FastModel, OllamaURL: a.OllamaURL,
	}, a.AnthropicAccountID)
}
```

and update its doc comment (lines 471-476) to say it preserves the record's
existing Anthropic account pin rather than "the CURRENT shared credential"
(stale wording from before this feature).

Then fix every remaining caller your preflight grep found:

- `internal/agent/harness_test.go`: at lines 21 and 32, change
  `m.resolveSpec(ctx, CreateRequest{...})` to
  `m.resolveSpec(ctx, CreateRequest{...}, "")`. At lines 45-46, 58-59 and 145,
  replace `st.SetAnthropicAuth(ctx, store.AnthropicKindAPIKey, "apikey-xyz")`
  with `if _, err := st.CreateAnthropicAccount(ctx, "test", store.AnthropicKindAPIKey, "apikey-xyz"); err != nil { t.Fatalf("CreateAnthropicAccount: %v", err) }`
  (same test intent: make the anthropic branch resolvable). At lines 48, 65
  and 73, add `, ""` to the `m.resolveSpec(ctx, CreateRequest{...})` calls
  the same way.
- `internal/agent/update_test.go`: at line 245, replace the
  `st.SetAnthropicAuth(...)` call the same way as above. At lines 263 and
  467, change the direct `m.resolveBackend(ctx, CreateRequest{...})` calls
  to add a third argument — `""` if the test is exercising fresh resolution,
  or the test's own agent record's `AnthropicAccountID` if the test is
  specifically about Update-path preservation (read each call's surrounding
  context to tell which; if genuinely ambiguous, `""` is the safer default
  since it is Create's own semantics and every existing test at these two
  lines predates the existingAccountID concept entirely).
- Re-run your Step 0 grep (`grep -rn "resolveBackend(ctx\|resolveSpec(ctx" internal/agent/*.go`)
  after these edits: every remaining match must now pass three arguments.

- [ ] **Step 6: Run the full `internal/agent` test suite**

```sh
go build ./...
go test ./internal/agent/... -v
gofmt -l internal/agent/
```

Expected: every test PASSes (including every pre-existing one you touched
in Step 5), `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```sh
git add internal/agent/create.go internal/agent/update.go internal/agent/reconcile.go internal/agent/backend_test.go internal/agent/harness_test.go internal/agent/create_test.go internal/agent/update_test.go
git commit -m "$(cat <<'EOF'
agent: resolve and pin a concrete Anthropic account at create/update time

resolveBackend now takes the agent's existing account pin (empty for
Create, the record's AnthropicAccountID for Update and for the
wake-agent path in reconcile.go) and resolves CreateRequest.AccountID
or the store's default against the new accounts store, pinning a
concrete id rather than "follow the default". A dangling pin (the
account was since deleted) fails closed exactly like the old "no
credential configured" case. Fixes every existing direct caller of the
old 2-arg signature, including the wake-agent path, which previously
would have silently re-resolved to whatever is currently default on
every wake-up instead of preserving the agent's own pin.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Agent — `Rename` gains `accountID`; Manager account passthroughs

**Files:**
- Modify: `docker-operator/internal/agent/query.go`
- Modify: `docker-operator/internal/agent/query_test.go`
- Create: `docker-operator/internal/agent/anthropicaccount.go`

**Interfaces:**
- Consumes: Task 1's store methods; Task 3's `ErrUnknownAnthropicAccount`/
  `IsUnknownAnthropicAccount`.
- Produces (consumed by Task 5):
  ```go
  var ErrAgentNotAnthropic error
  func IsAgentNotAnthropic(err error) bool
  func (m *Manager) Rename(ctx context.Context, id string, name, description, accountID *string) (store.Agent, error)
  func (m *Manager) ListAnthropicAccounts(ctx context.Context) ([]store.AnthropicAccount, error)
  func (m *Manager) CreateAnthropicAccount(ctx context.Context, name, kind, value string) (store.AnthropicAccount, error)
  func (m *Manager) DeleteAnthropicAccount(ctx context.Context, id string) error
  func (m *Manager) DefaultAnthropicAccountID(ctx context.Context) (string, error)
  func (m *Manager) SetDefaultAnthropicAccount(ctx context.Context, id string) error
  ```
  `AnthropicAuthStatus`/`SetAnthropicAuth`/`ClearAnthropicAuth` (the old
  Manager-level wrappers) are LEFT IN PLACE, untouched, by this task — they
  are not removed until Task 5 (see that task's dedicated removal step).
  Task 5 must not assume this task removed them.

- [ ] **Step 1: Write the failing `Rename` tests**

In `internal/agent/query_test.go`, add:

```go
func TestRename_AccountIDOnAnthropicAgent(t *testing.T) {
	m := newTestManager(t) // reuse this file's existing manager-construction helper
	ctx := context.Background()
	a, err := m.store.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	b, err := m.store.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindAPIKey, "sk-ant-personal")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount: %v", err)
	}
	agentID := "agt_test1"
	if _, err := m.store.Create(ctx, store.CreateSpec{ID: agentID, Backend: "anthropic", AnthropicAccountID: a.ID}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	newID := b.ID
	updated, err := m.Rename(ctx, agentID, nil, nil, &newID)
	if err != nil {
		t.Fatalf("Rename(accountID=%q): %v", newID, err)
	}
	if updated.AnthropicAccountID != b.ID {
		t.Fatalf("Rename result AnthropicAccountID = %q; want %q", updated.AnthropicAccountID, b.ID)
	}
	// Re-fetch to confirm it was actually PERSISTED, not just returned.
	got, err := m.store.Get(ctx, agentID)
	if err != nil || got.AnthropicAccountID != b.ID {
		t.Fatalf("Get after Rename = %+v, %v; want AnthropicAccountID=%q", got, err, b.ID)
	}
}

// TestRename_AccountIDOnOllamaAgentIs400Shape is a Review Focus case.
func TestRename_AccountIDOnOllamaAgentIs400Shape(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	a, _ := m.store.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	agentID := "agt_test2"
	if _, err := m.store.Create(ctx, store.CreateSpec{ID: agentID, Backend: "ollama"}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	accountID := a.ID
	_, err := m.Rename(ctx, agentID, nil, nil, &accountID)
	if !IsAgentNotAnthropic(err) {
		t.Fatalf("Rename(accountID) on an ollama agent error = %v; want IsAgentNotAnthropic", err)
	}
	got, err := m.store.Get(ctx, agentID)
	if err != nil || got.AnthropicAccountID != "" {
		t.Fatalf("ollama agent after a rejected Rename = %+v, %v; want AnthropicAccountID unchanged (empty)", got, err)
	}
}

func TestRename_UnknownAccountIDIs400Shape(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	a, _ := m.store.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	agentID := "agt_test3"
	if _, err := m.store.Create(ctx, store.CreateSpec{ID: agentID, Backend: "anthropic", AnthropicAccountID: a.ID}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	unknown := "anc_missing"
	_, err := m.Rename(ctx, agentID, nil, nil, &unknown)
	if !IsUnknownAnthropicAccount(err) {
		t.Fatalf("Rename(unknown accountID) error = %v; want IsUnknownAnthropicAccount", err)
	}
}
```

If `query_test.go` has no `newTestManager` helper (Task 3 may have
introduced one, or the file may already have its own differently-named
one), use whichever one already exists in that file for constructing a
`*Manager` with a real `*store.Store` — do not introduce a second,
differently-behaved helper.

- [ ] **Step 2: Run the tests to verify they fail**

```sh
go test ./internal/agent/... -run TestRename_ -v
```

Expected: compile failure (`Rename` takes 3 args today, these calls pass 4;
`ErrAgentNotAnthropic`/`IsAgentNotAnthropic` do not exist).

- [ ] **Step 3: Implement the extended `Rename`**

In `internal/agent/query.go`, add near the top (after the existing
doc-commented vars, or directly above `Rename`):

```go
// ErrAgentNotAnthropic is returned by Rename for a non-nil accountID against
// an agent whose Backend is not config.BackendAnthropic -- there is nothing
// to pin on an ollama agent. internal/api maps it to a 400.
var ErrAgentNotAnthropic = errors.New("agent is not on the anthropic backend")

// IsAgentNotAnthropic reports whether err was caused by a PATCH account_id
// against a non-anthropic agent.
func IsAgentNotAnthropic(err error) bool { return errors.Is(err, ErrAgentNotAnthropic) }
```

(Add `"errors"` to this file's imports if not already present.)

Replace `Rename` (lines 158-178) with:

```go
// Rename updates an agent's Name, Description and/or its pinned Anthropic
// account. A nil pointer leaves the corresponding field unchanged; a
// non-nil pointer sets it (including to an empty string, for name/
// description). At least one of the three must be non-nil.
//
// This never touches Docker: all three are either cosmetic UI-facing
// fields or a plain store pin change, so this is a direct store.Update
// rather than a step in the Create/Update/Delete lifecycle -- changing
// which account an agent uses does NOT recreate its container; the new
// credential applies starting at the agent's next restart/wake.
//
// accountID is validated BEFORE the store.Update call, not inside its
// mutator: a deleted-between-check-and-write race just means the
// fail-closed dangling-pin path in resolveBackend catches it at the
// agent's next restart/wake -- an already-accepted outcome, not a new hole
// this adds.
func (m *Manager) Rename(ctx context.Context, id string, name, description, accountID *string) (store.Agent, error) {
	if name == nil && description == nil && accountID == nil {
		return store.Agent{}, fmt.Errorf("renaming agent %q: at least one of name, description or account_id must be provided", id)
	}
	if accountID != nil {
		a, err := m.store.Get(ctx, id)
		if err != nil {
			return store.Agent{}, err
		}
		if a.Backend != config.BackendAnthropic {
			return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, ErrAgentNotAnthropic)
		}
		if _, err := m.store.GetAnthropicAccount(ctx, *accountID); err != nil {
			if store.IsAnthropicAccountNotFound(err) {
				return store.Agent{}, fmt.Errorf("renaming agent %q: %w: %q", id, ErrUnknownAnthropicAccount, *accountID)
			}
			return store.Agent{}, fmt.Errorf("renaming agent %q: checking the anthropic account: %w", id, err)
		}
	}
	return m.store.Update(ctx, id, func(a *store.Agent) error {
		if name != nil {
			a.Name = *name
		}
		if description != nil {
			a.Description = *description
		}
		if accountID != nil {
			a.AnthropicAccountID = *accountID
		}
		return nil
	})
}
```

**Leave the old `AnthropicAuthStatus`, `SetAnthropicAuth`, `ClearAnthropicAuth`
methods (lines 74-96) in this file untouched for now** — do NOT remove them
in this task. They still call `m.store.GetAnthropicAuth`/`SetAnthropicAuth`/
`ClearAnthropicAuth`, which still exist in `store.go` (Task 2 deliberately
did not remove them either). Removing all three layers together is Task 5's
job, in its own dedicated step, once the `AgentManager` interface it also
owns is being edited in the same commit — removing them here would leave
`internal/api`'s interface (unedited until Task 5) requiring methods
`*agent.Manager` no longer has, breaking the whole module's build in the
gap between this task and Task 5.

- [ ] **Step 4: Add the Manager-level account passthroughs**

Create `internal/agent/anthropicaccount.go`:

```go
package agent

import (
	"context"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ListAnthropicAccounts, CreateAnthropicAccount, DeleteAnthropicAccount,
// DefaultAnthropicAccountID and SetDefaultAnthropicAccount are thin
// pass-throughs to the store -- none of them touch Docker, the same shape
// AnthropicAuthStatus/SetAnthropicAuth/ClearAnthropicAuth had before this
// feature replaced the single shared credential with named accounts.

func (m *Manager) ListAnthropicAccounts(ctx context.Context) ([]store.AnthropicAccount, error) {
	return m.store.ListAnthropicAccounts(ctx)
}

func (m *Manager) CreateAnthropicAccount(ctx context.Context, name, kind, value string) (store.AnthropicAccount, error) {
	return m.store.CreateAnthropicAccount(ctx, name, kind, value)
}

func (m *Manager) DeleteAnthropicAccount(ctx context.Context, id string) error {
	return m.store.DeleteAnthropicAccount(ctx, id)
}

func (m *Manager) DefaultAnthropicAccountID(ctx context.Context) (string, error) {
	return m.store.DefaultAnthropicAccountID(ctx)
}

func (m *Manager) SetDefaultAnthropicAccount(ctx context.Context, id string) error {
	return m.store.SetDefaultAnthropicAccount(ctx, id)
}
```

- [ ] **Step 5: Run the full `internal/agent` suite**

```sh
go build ./...
go test ./... -v
gofmt -l internal/agent/
```

Expected: everything PASSes. The old `AnthropicAuthStatus`/`SetAnthropicAuth`/
`ClearAnthropicAuth` methods are still present and still compile (they are
now simply unused by anything new — Task 5 removes them).

- [ ] **Step 6: Commit**

```sh
git add internal/agent/query.go internal/agent/query_test.go internal/agent/anthropicaccount.go
git commit -m "$(cat <<'EOF'
agent: Rename can re-pin an agent's Anthropic account without a recreate

Extends the existing no-Docker-touch PATCH path (Rename) with an
optional account_id, validated against the accounts store before the
write. Adds pass-throughs to the new accounts CRUD alongside (not yet
replacing) the old AnthropicAuthStatus/SetAnthropicAuth/
ClearAnthropicAuth Manager wrappers -- their removal is Task 5's, once
the AgentManager interface stops requiring them too.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: API — `/api/anthropic/accounts` CRUD endpoints

**Files:**
- Modify: `docker-operator/internal/api/handlers.go`
- Modify: `docker-operator/internal/api/handlers_test.go`
- Rewrite: `docker-operator/internal/api/anthropic_test.go`
- Modify: `docker-operator/internal/store/store.go` (remove the old
  `AnthropicAuth` surface)
- Modify: `docker-operator/internal/store/store_test.go` (remove its now-dead
  tests for that surface)
- Modify: `docker-operator/internal/agent/query.go` (remove the old Manager
  wrappers)

**Interfaces:**
- Consumes: Task 4's Manager methods.
- Produces (consumed by Task 6, Task 7):
  ```go
  type anthropicAccountView struct {
      ID        string     `json:"id"`
      Name      string     `json:"name"`
      Kind      string     `json:"kind"`
      CreatedAt time.Time  `json:"created_at"`
      UpdatedAt time.Time  `json:"updated_at"`
      IsDefault bool       `json:"is_default"`
  }
  // GET  /api/anthropic/accounts            -> {"accounts": [anthropicAccountView, ...]}
  // POST /api/anthropic/accounts <- {"name","kind","value"} -> 201 anthropicAccountView
  // DELETE /api/anthropic/accounts/{id}     -> 200 always
  // PUT  /api/anthropic/accounts/{id}/default -> 204
  ```

This task is also where the old single-credential surface is finally
removed, now that nothing outside it will be left referencing it. Do this
FIRST, as its own step, before touching the new endpoints — it is a clean,
independently-verifiable deletion (confirm with the grep below, then
`go build`/`go test` before moving on), and every later step in this task
builds on a tree that already compiles without it.

- [ ] **Step 1: Remove the old single-credential `AnthropicAuth` surface**

Confirm the removal is safe before doing it:

```sh
grep -rn "GetAnthropicAuth\|SetAnthropicAuth\|ClearAnthropicAuth\|\bAnthropicAuth\b\|AnthropicAuthStatus" --include=*.go .
```

This should print matches ONLY in three places: `internal/store/store.go`
(the surface itself), `internal/store/store_test.go` (its tests), and
`internal/agent/query.go` (the Manager-level wrappers Task 4 deliberately
left in place) — if it prints a match anywhere else (a caller Task 3 or 4
missed), stop and report `NEEDS_CONTEXT` rather than deleting out from under
a live caller.

Once confirmed, delete:

1. From `internal/store/store.go`: the `AnthropicAuth` type (the exported
   struct with `Kind`/`Value`/`UpdatedAt`) and the `GetAnthropicAuth`,
   `SetAnthropicAuth`, `ClearAnthropicAuth` methods. Leave the
   `keySettingsAnthropicAuth` var and its doc comment in place — migration
   (Task 2) reads it forever, on every `Open`, for however long an
   un-migrated pre-feature database might still exist.
2. From `internal/store/store_test.go`: `TestAnthropicAuth_RoundTrip`,
   `TestSetAnthropicAuth_Rejects` in full, and the three
   `GetAnthropicAuth`/`SetAnthropicAuth`/`ClearAnthropicAuth`-after-Close
   assertions (around lines 224-231) plus their three table-driven entries
   (around lines 746-748) — read the surrounding table/function first so
   you remove exactly these entries and leave every unrelated case in the
   same table/test untouched.
3. From `internal/agent/query.go`: the `AnthropicAuthStatus`, `SetAnthropicAuth`,
   `ClearAnthropicAuth` methods (around lines 74-96 as of Task 4's state).
   This was `query.go`'s only use of the `"time"` import — remove `"time"`
   from its import block too, or `go build` fails with "imported and not
   used".
4. **(Controller ruling from Task 2's review — do this too, in this same
   step):** restore the legacy-key deletion that Task 2 deliberately left
   out. In `internal/store/anthropicaccount.go`'s `migrateLegacyAnthropicAuth`,
   immediately after the `settings.Put(keySettingsDefaultAnthropicAccount, []byte(id))`
   call, add:
   ```go
   if err := settings.Delete(keySettingsAnthropicAuth); err != nil {
       return fmt.Errorf("removing the legacy anthropic credential: %w", err)
   }
   ```
   Task 2 could not add this: `store_test.go`'s `TestAnthropicAuth_RoundTrip`
   (which you are deleting in step 2 above, in this same commit) reopens a
   store with the legacy key set but no account yet created, which would
   have triggered migration mid-test and deleted the key out from under its
   own next assertion. Doing both in the same commit — deleting the
   conflicting test AND restoring the delete call — resolves the conflict
   cleanly instead of leaving a secret permanently orphaned in the database
   once this step also removes the only code that could ever read or write
   that key again.

```sh
go build ./...
go test ./... -v
gofmt -l internal/store/ internal/agent/
git add internal/store/store.go internal/store/store_test.go internal/store/anthropicaccount.go internal/agent/query.go
git commit -m "$(cat <<'EOF'
store,agent: remove the superseded single-credential AnthropicAuth surface

Deletes store.AnthropicAuth and its three methods, their tests, and
the now-unused Manager-level wrappers in internal/agent/query.go --
everything that called them moved onto the accounts store in the
prior three tasks. Also restores migrateLegacyAnthropicAuth's deletion
of the legacy settings key, deferred from Task 2 because the test
this commit also deletes (TestAnthropicAuth_RoundTrip) would otherwise
have broken on a reopen mid-test; migrateLegacyAnthropicAuth is now
the only remaining reader of that key, and this is its last use of it.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 2: Update the `AgentManager` interface**

In `internal/api/handlers.go`, replace the block at lines 103-109:

```go
	// AnthropicAuthStatus reports whether a shared Anthropic credential is
	// configured, its kind and when it was last set -- never its value.
	// SetAnthropicAuth stores (replacing) it; ClearAnthropicAuth removes it
	// (idempotent).
	AnthropicAuthStatus(ctx context.Context) (kind string, updatedAt time.Time, configured bool, err error)
	SetAnthropicAuth(ctx context.Context, kind, value string) error
	ClearAnthropicAuth(ctx context.Context) error
```

with:

```go
	// ListAnthropicAccounts/CreateAnthropicAccount/DeleteAnthropicAccount/
	// DefaultAnthropicAccountID/SetDefaultAnthropicAccount back the
	// Settings "Anthropic Accounts" panel and the create/update forms'
	// account picker. CreateAnthropicAccount never returns an account's
	// Value to a caller beyond this process boundary -- handlers.go's own
	// response types simply omit the field.
	ListAnthropicAccounts(ctx context.Context) ([]store.AnthropicAccount, error)
	CreateAnthropicAccount(ctx context.Context, name, kind, value string) (store.AnthropicAccount, error)
	DeleteAnthropicAccount(ctx context.Context, id string) error
	DefaultAnthropicAccountID(ctx context.Context) (string, error)
	SetDefaultAnthropicAccount(ctx context.Context, id string) error
```

Leave the `StartAnthropicLogin`/`StopAnthropicLogin`/`AnthropicLoginActive`
lines (111-116) untouched.

- [ ] **Step 3: Remove the old singleton routes and add the new ones**

Replace, at lines 211-213:

```go
	mux.HandleFunc("GET /api/anthropic/auth", h.handleAnthropicAuthGet)
	mux.HandleFunc("PUT /api/anthropic/auth", h.handleAnthropicAuthPut)
	mux.HandleFunc("DELETE /api/anthropic/auth", h.handleAnthropicAuthDelete)
```

with:

```go
	mux.HandleFunc("GET /api/anthropic/accounts", h.handleAnthropicAccountsList)
	mux.HandleFunc("POST /api/anthropic/accounts", h.handleAnthropicAccountsCreate)
	mux.HandleFunc("DELETE /api/anthropic/accounts/{id}", h.handleAnthropicAccountDelete)
	mux.HandleFunc("PUT /api/anthropic/accounts/{id}/default", h.handleAnthropicAccountSetDefault)
```

and at line 239 (the bare method-not-allowed registration), replace:

```go
	mux.HandleFunc("/api/anthropic/auth", methodNotAllowed)
```

with:

```go
	mux.HandleFunc("/api/anthropic/accounts", methodNotAllowed)
	mux.HandleFunc("/api/anthropic/accounts/{id}", methodNotAllowed)
	mux.HandleFunc("/api/anthropic/accounts/{id}/default", methodNotAllowed)
```

Leave the `/api/anthropic/login` routes (214-216, 240) untouched.

- [ ] **Step 4: Replace the request/response types**

Replace `anthropicAuthRequest`/`anthropicAuthResponse` (lines 862-875) with:

```go
// anthropicAccountCreateRequest is the POST /api/anthropic/accounts body.
type anthropicAccountCreateRequest struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// anthropicAccountView is every GET/POST /api/anthropic/accounts response
// element. It never carries Value -- only an account's name, kind, and
// timestamps, plus whether it is the current default.
type anthropicAccountView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	IsDefault bool      `json:"is_default"`
}

// anthropicAccountsListResponse is the GET /api/anthropic/accounts body.
// Accounts is never nil (JSON "[]" for none), matching every other list
// response in this package (agentListResponse, templateListResponse).
type anthropicAccountsListResponse struct {
	Accounts []anthropicAccountView `json:"accounts"`
}
```

Leave `anthropicLoginResponse` (877-882) untouched.

- [ ] **Step 5: Replace the handlers**

Replace `handleAnthropicAuthGet`/`handleAnthropicAuthPut`/
`handleAnthropicAuthDelete`/`anthropicAuthStatusBody` (lines 1087-1169,
1203-1210) with:

```go
func (h *Handler) handleAnthropicAccountsList(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.mgr.ListAnthropicAccounts(r.Context())
	if err != nil {
		h.internalError(w, "listing anthropic accounts", err)
		return
	}
	defaultID, err := h.mgr.DefaultAnthropicAccountID(r.Context())
	if err != nil {
		h.internalError(w, "reading the default anthropic account", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicAccountsListResponse{Accounts: toAnthropicAccountViews(accounts, defaultID)})
}

// handleAnthropicAccountsCreate serves the direct API-key path AND the
// `claude setup-token` terminal's finish step (the web UI's "Add account"
// modal collects the name BEFORE either sub-flow starts and carries it
// client-side -- see
// docs/superpowers/specs/2026-10-08-multi-anthropic-accounts-design.md's
// "Login / add-account flow"). The shape checks mirror the retired
// handleAnthropicAuthPut's exactly.
func (h *Handler) handleAnthropicAccountsCreate(w http.ResponseWriter, r *http.Request) {
	var req anthropicAccountCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name")
		return
	}
	if !store.ValidAnthropicKind(req.Kind) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, `"kind" must be "api_key" or "oauth"`, "kind")
		return
	}
	// See handleAnthropicAuthPut's retired doc comment for why interior
	// whitespace is rejected rather than just the surrounding trim: a
	// terminal-pasted token that got line-wrapped or truncated must not be
	// silently stored as an unusable value.
	req.Value = strings.TrimSpace(req.Value)
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, CodeMissingField, `"value" must not be empty`, "value")
		return
	}
	if strings.ContainsFunc(req.Value, unicode.IsSpace) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, `"value" must not contain whitespace (a wrapped or truncated paste?)`, "value")
		return
	}
	switch req.Kind {
	case store.AnthropicKindAPIKey:
		if !strings.HasPrefix(req.Value, "sk-ant-") {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `an Anthropic API key starts with "sk-ant-"`, "value")
			return
		}
	case store.AnthropicKindOAuth:
		if !strings.HasPrefix(req.Value, "sk-ant-oat01-") {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `a Claude Code OAuth token (from "claude setup-token") starts with "sk-ant-oat01-"`, "value")
			return
		}
	}

	account, err := h.mgr.CreateAnthropicAccount(r.Context(), req.Name, req.Kind, req.Value)
	if err != nil {
		if store.IsAnthropicAccountNameTaken(err) {
			writeError(w, http.StatusConflict, CodeDuplicateName, "an anthropic account with that name already exists", "name")
			return
		}
		h.internalError(w, "creating anthropic account", err)
		return
	}
	// A credential is now stored, so a running `claude setup-token` helper
	// (if this was its finish step) has done its job -- tear it down.
	// Best-effort and harmless when this was the direct-paste path instead
	// (no login container exists, so this is a no-op).
	if err := h.mgr.StopAnthropicLogin(r.Context()); err != nil {
		h.log.Warn("could not tear down the Anthropic-login helper after creating an account", "error", err)
	}
	defaultID, err := h.mgr.DefaultAnthropicAccountID(r.Context())
	if err != nil {
		h.internalError(w, "reading back the default anthropic account", err)
		return
	}
	w.Header().Set("Location", "/api/anthropic/accounts/"+account.ID)
	writeJSON(w, http.StatusCreated, toAnthropicAccountView(account, defaultID))
}

// handleAnthropicAccountDelete always answers 200, whether or not the
// account existed -- Manager.DeleteAnthropicAccount is itself idempotent
// and deliberately does not check whether any agent references the id
// (see the spec's "Delete is unconditional" decision).
func (h *Handler) handleAnthropicAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.mgr.DeleteAnthropicAccount(r.Context(), id); err != nil {
		h.internalError(w, "deleting anthropic account "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
}

func (h *Handler) handleAnthropicAccountSetDefault(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.mgr.SetDefaultAnthropicAccount(r.Context(), id); err != nil {
		if store.IsAnthropicAccountNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such anthropic account", "")
			return
		}
		h.internalError(w, "setting the default anthropic account", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toAnthropicAccountView(a store.AnthropicAccount, defaultID string) anthropicAccountView {
	return anthropicAccountView{
		ID: a.ID, Name: a.Name, Kind: a.Kind,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		IsDefault: defaultID != "" && a.ID == defaultID,
	}
}

func toAnthropicAccountViews(accounts []store.AnthropicAccount, defaultID string) []anthropicAccountView {
	views := make([]anthropicAccountView, 0, len(accounts))
	for _, a := range accounts {
		views = append(views, toAnthropicAccountView(a, defaultID))
	}
	return views
}
```

Leave `handleAnthropicLoginGet`/`Start`/`Stop`/`anthropicLoginBody` (lines
1170-1201) untouched.

- [ ] **Step 6: Update `fakeManager` in `handlers_test.go`**

Replace the Anthropic-auth fields in the `fakeManager` struct (lines 57-63):

```go
	anthropicKind      string
	anthropicValue     string
	anthropicUpdatedAt time.Time
	anthropicSet       bool
	anthropicGetErr    error
	anthropicSetErr    error
	anthropicClearErr  error
```

with:

```go
	anthropicAccounts      []store.AnthropicAccount
	anthropicDefaultID     string
	anthropicListErr       error
	anthropicCreateErr     error
	anthropicDeleteErr     error
	anthropicSetDefaultErr error
```

Replace the `AnthropicAuthStatus`/`SetAnthropicAuth`/`ClearAnthropicAuth`
fake methods (lines 365-404) with:

```go
func (f *fakeManager) ListAnthropicAccounts(_ context.Context) ([]store.AnthropicAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicListErr != nil {
		return nil, f.anthropicListErr
	}
	out := make([]store.AnthropicAccount, len(f.anthropicAccounts))
	copy(out, f.anthropicAccounts)
	return out, nil
}

func (f *fakeManager) CreateAnthropicAccount(_ context.Context, name, kind, value string) (store.AnthropicAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicCreateErr != nil {
		return store.AnthropicAccount{}, f.anthropicCreateErr
	}
	for _, a := range f.anthropicAccounts {
		if a.Name == name {
			return store.AnthropicAccount{}, fmt.Errorf("creating anthropic account %q: %w", name, store.ErrAnthropicAccountNameTaken)
		}
	}
	a := store.AnthropicAccount{
		ID: fmt.Sprintf("anc_fake%d", len(f.anthropicAccounts)+1),
		Name: name, Kind: kind, Value: value,
		UpdatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
	f.anthropicAccounts = append(f.anthropicAccounts, a)
	if f.anthropicDefaultID == "" {
		f.anthropicDefaultID = a.ID
	}
	return a, nil
}

func (f *fakeManager) DeleteAnthropicAccount(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicDeleteErr != nil {
		return f.anthropicDeleteErr
	}
	kept := f.anthropicAccounts[:0]
	for _, a := range f.anthropicAccounts {
		if a.ID != id {
			kept = append(kept, a)
		}
	}
	f.anthropicAccounts = kept
	if f.anthropicDefaultID == id {
		f.anthropicDefaultID = ""
	}
	return nil
}

func (f *fakeManager) DefaultAnthropicAccountID(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.anthropicDefaultID, nil
}

func (f *fakeManager) SetDefaultAnthropicAccount(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicSetDefaultErr != nil {
		return f.anthropicSetDefaultErr
	}
	for _, a := range f.anthropicAccounts {
		if a.ID == id {
			f.anthropicDefaultID = id
			return nil
		}
	}
	return fmt.Errorf("setting default anthropic account %q: %w", id, store.ErrAnthropicAccountNotFound)
}
```

Update `fakeManager.Rename` (lines 432-447) to take and apply the fourth
parameter:

```go
func (f *fakeManager) Rename(_ context.Context, id string, name, description, accountID *string) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, store.ErrNotFound)
	}
	if accountID != nil {
		if a.Backend != config.BackendAnthropic {
			return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, agent.ErrAgentNotAnthropic)
		}
		found := false
		for _, acc := range f.anthropicAccounts {
			if acc.ID == *accountID {
				found = true
				break
			}
		}
		if !found {
			return store.Agent{}, fmt.Errorf("renaming agent %q: %w: %q", id, agent.ErrUnknownAnthropicAccount, *accountID)
		}
		a.AnthropicAccountID = *accountID
	}
	if name != nil {
		a.Name = *name
	}
	if description != nil {
		a.Description = *description
	}
	f.agents[id] = a
	return a, nil
}
```

- [ ] **Step 7: Rewrite `internal/api/anthropic_test.go`'s account tests**

Read the current file first (343 lines) — it has 18 test functions. Keep
every `TestAnthropicLogin_*` test (5 functions: `StartStopStatus`,
`StartErrorIs500`, `MethodNotAllowed`) completely UNCHANGED — the login
endpoints did not change. Keep `TestCreate_*` tests (the first 5 functions)
largely as-is, but they construct a `fakeManager` with the old
`anthropicSet`/`anthropicGetErr` fields — update those construction sites to
use `anthropicAccounts`/`anthropicDefaultID`/`anthropicListErr` instead,
matching Step 5's new field names (e.g. `TestCreate_NoAnthropicAuthIs409`
seeds a fake with NO accounts and an empty `anthropicDefaultID`, rather than
`anthropicSet: false`).

Replace every `TestAnthropicAuth_*` function (the 8 functions:
`GetBeforeSet`, `PutThenGet`, `PutValidation`, `PutAcceptsAValidAPIKey`,
`PutTrimsSurroundingWhitespace`, `Delete`, `MethodNotAllowed`,
`GetErrorIs500`, plus `PutTearsDownLogin`/`PutSucceedsEvenIfLoginTeardownFails`)
with equivalent tests against the new endpoints. Two fully worked examples
to anchor the rest:

```go
func TestAnthropicAccounts_ListEmpty(t *testing.T) {
	h, _ := newTestHandler(t) // use this file's existing handler-construction helper
	rr := httptest.NewRequest(http.MethodGet, "/api/anthropic/accounts", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, rr)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/anthropic/accounts status = %d; want 200", w.Code)
	}
	var body anthropicAccountsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.Accounts) != 0 {
		t.Fatalf("Accounts = %v; want empty", body.Accounts)
	}
}

func TestAnthropicAccounts_CreateThenList(t *testing.T) {
	h, fm := newTestHandler(t)
	reqBody := `{"name":"Work","kind":"api_key","value":"sk-ant-abc123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/anthropic/accounts", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/anthropic/accounts status = %d, body = %s; want 201", w.Code, w.Body.String())
	}
	var created anthropicAccountView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if created.Name != "Work" || created.Kind != "api_key" || !created.IsDefault {
		t.Fatalf("created account = %+v; want Name=Work Kind=api_key IsDefault=true (first account)", created)
	}
	// Never leak the value.
	if strings.Contains(w.Body.String(), "sk-ant-abc123") {
		t.Fatalf("response body leaked the account value: %s", w.Body.String())
	}
	if len(fm.anthropicAccounts) != 1 || fm.anthropicAccounts[0].Value != "sk-ant-abc123" {
		t.Fatalf("fakeManager.anthropicAccounts = %+v; want one account with the real value stored server-side", fm.anthropicAccounts)
	}
}
```

Write the remaining equivalents the same way, against `newTestHandler`'s
real request/response cycle (not calling `fakeManager` methods directly —
these are HTTP-handler tests):
- Validation (missing name, bad kind, empty/whitespace/wrong-prefix value)
  → 400, mirroring `TestAnthropicAuth_PutValidation`'s table of cases.
- A valid `api_key` create succeeds; trims surrounding whitespace from
  `value` before the prefix check.
- `DELETE /api/anthropic/accounts/{id}` on both an existing and a
  never-existed id → 200 both times (idempotent — this replaces
  `TestAnthropicAuth_Delete`, and is itself one of the plan's Global
  Constraints, so assert it explicitly here too).
- `PUT /api/anthropic/accounts/{id}/default` on an unknown id → 404; on a
  real id → 204 and a subsequent `GET` shows `is_default: true` on it and
  `false` on whichever was previously default.
- Method-not-allowed on `/api/anthropic/accounts` (e.g. `PATCH`) → 405,
  mirroring `TestAnthropicAuth_MethodNotAllowed`.
- A manager error from `ListAnthropicAccounts`/`CreateAnthropicAccount` →
  500, mirroring `TestAnthropicAuth_GetErrorIs500`.
- `TestAnthropicAccountsCreate_TearsDownLogin` /
  `_SucceedsEvenIfLoginTeardownFails`: same two scenarios as the retired
  `TestAnthropicAuth_PutTearsDownLogin`/
  `PutSucceedsEvenIfLoginTeardownFails`, now against
  `POST /api/anthropic/accounts`.

- [ ] **Step 8: Run the full API test suite**

```sh
go build ./...
go test ./internal/api/... -v
gofmt -l internal/api/
```

Expected: every test PASSes, `gofmt -l` prints nothing.

- [ ] **Step 9: Commit**

```sh
git add internal/api/handlers.go internal/api/handlers_test.go internal/api/anthropic_test.go
git commit -m "$(cat <<'EOF'
api: replace the single shared Anthropic credential endpoints with accounts

GET/POST /api/anthropic/accounts, DELETE /api/anthropic/accounts/{id}
and PUT .../{id}/default replace GET/PUT/DELETE /api/anthropic/auth.
POST serves both the direct API-key path and the claude setup-token
terminal's finish step (the retired PUT handler's shape checks, kept
verbatim). The login endpoints are untouched.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: API — `PATCH account_id`, create-time `account_id`, and the default-id in `GET /api/agents`

**Files:**
- Modify: `docker-operator/internal/api/handlers.go`
- Modify: `docker-operator/internal/api/handlers_test.go`

**Interfaces:**
- Consumes: Task 3's `CreateRequest.AccountID`/`IsUnknownAnthropicAccount`;
  Task 4's extended `Rename`/`IsAgentNotAnthropic`.
- Produces: `GET /api/agents` response gains `default_anthropic_account_id`;
  `POST /api/agents` accepts `account_id`; `PATCH /api/agents/{id}` accepts
  `account_id`.

- [ ] **Step 1: Write the failing handler tests**

In `internal/api/handlers_test.go` (or a new `anthropic_account_patch_test.go`
in the same package if that keeps `handlers_test.go` from growing further —
either is fine, pick whichever this codebase's existing file-size pattern
suggests by looking at how large `handlers_test.go` already is):

```go
func TestPatchAgent_AccountID(t *testing.T) {
	h, fm := newTestHandler(t)
	acc := store.AnthropicAccount{ID: "anc_work", Name: "Work", Kind: "api_key", Value: "sk-ant-work"}
	fm.anthropicAccounts = []store.AnthropicAccount{acc}
	fm.anthropicDefaultID = acc.ID
	fm.seed(store.Agent{ID: "agt_1", Backend: "anthropic", AnthropicAccountID: acc.ID, Status: store.StatusRunning})

	other := store.AnthropicAccount{ID: "anc_personal", Name: "Personal", Kind: "api_key", Value: "sk-ant-personal"}
	fm.anthropicAccounts = append(fm.anthropicAccounts, other)

	body := `{"account_id":"anc_personal"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/agents/agt_1", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH account_id status = %d, body = %s; want 200", w.Code, w.Body.String())
	}
	var got store.Agent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.AnthropicAccountID != "anc_personal" {
		t.Fatalf("AnthropicAccountID = %q; want anc_personal", got.AnthropicAccountID)
	}
}

// TestPatchAgent_AccountIDOnOllamaAgentIs400 is a Review Focus case.
func TestPatchAgent_AccountIDOnOllamaAgentIs400(t *testing.T) {
	h, fm := newTestHandler(t)
	fm.seed(store.Agent{ID: "agt_1", Backend: "ollama", Status: store.StatusRunning})

	body := `{"account_id":"anc_work"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/agents/agt_1", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH account_id on an ollama agent status = %d, body = %s; want 400", w.Code, w.Body.String())
	}
}

func TestCreateAgent_WithExplicitAccountID(t *testing.T) {
	h, fm := newTestHandler(t)
	acc := store.AnthropicAccount{ID: "anc_personal", Name: "Personal", Kind: "api_key", Value: "sk-ant-personal"}
	fm.anthropicAccounts = []store.AnthropicAccount{acc}
	fm.anthropicDefaultID = acc.ID

	body := `{"backend":"anthropic","account_id":"anc_personal"}`
	req := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/agents status = %d, body = %s; want 201", w.Code, w.Body.String())
	}
}

func TestListAgents_IncludesDefaultAnthropicAccountID(t *testing.T) {
	h, fm := newTestHandler(t)
	fm.anthropicDefaultID = "anc_work"
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body agentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// agentListResponse needs a DefaultAnthropicAccountID field for this to
	// compile at all -- see Step 3 below.
	if body.DefaultAnthropicAccountID != "anc_work" {
		t.Fatalf("DefaultAnthropicAccountID = %q; want anc_work", body.DefaultAnthropicAccountID)
	}
}
```

Use whatever `newTestHandler(t)`/`fm.seed(...)` helpers `handlers_test.go`
already has (it clearly has a `seed` method on `fakeManager`, confirmed by
Task 5's Step 5 editing that same file) rather than inventing new ones.

- [ ] **Step 2: Run the tests to verify they fail**

```sh
go test ./internal/api/... -run "TestPatchAgent_AccountID|TestCreateAgent_WithExplicitAccountID|TestListAgents_IncludesDefaultAnthropicAccountID" -v
```

Expected: compile failure (`store.Agent` has no `AnthropicAccountID` field
visible here until Task 3 lands; `createAgentRequest`/`patchAgentRequest`
have no `account_id` field; `agentListResponse` has no
`DefaultAnthropicAccountID` field).

- [ ] **Step 3: Add `default_anthropic_account_id` to the list response**

In `internal/api/handlers.go`'s `agentListResponse` struct (around line
799-819), add after `DefaultAutoMode`:

```go
	// DefaultAnthropicAccountID is the account id that pre-fills the
	// create form's account <select>, or "" if no default is set (a fresh
	// operator, or the default account was since deleted).
	DefaultAnthropicAccountID string `json:"default_anthropic_account_id"`
```

In `handleList` (around line 886-904), add to the `agentListResponse{...}`
literal:

```go
		DefaultAnthropicAccountID: h.defaultAnthropicAccountIDOrEmpty(r.Context()),
```

and add this helper right after `handleList`:

```go
// defaultAnthropicAccountIDOrEmpty degrades to "" on a store read failure
// rather than failing the whole agent list over a create-form prefill
// nicety -- the same best-effort spirit diskUsageFor already uses for the
// Activity page's per-agent disk figures.
func (h *Handler) defaultAnthropicAccountIDOrEmpty(ctx context.Context) string {
	id, err := h.mgr.DefaultAnthropicAccountID(ctx)
	if err != nil {
		h.log.Warn("could not read the default anthropic account for the agent list", "error", err)
		return ""
	}
	return id
}
```

- [ ] **Step 4: Add `account_id` to the create request**

In `createAgentRequest` (around line 260-301), add after `AutoMode`:

```go
	// AccountID pins this one agent to a specific stored Anthropic account.
	// Only meaningful for the anthropic backend; empty means "use the
	// operator's current default account". See agent.CreateRequest.AccountID.
	AccountID string `json:"account_id"`
```

In `toCreateRequest` (around line 351-361), add `AccountID: req.AccountID,`
to the returned `agent.CreateRequest{...}` literal.

In `handleCreate`'s error switch (around line 919-940), add a case right
after the existing `agent.IsNoAnthropicAuth(err)` one:

```go
		case agent.IsUnknownAnthropicAccount(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "unknown anthropic account", "account_id")
```

**Controller ruling (found during Task 3's review, before Task 6 was ever
dispatched): this step creates a real embedding hazard you must close in
the SAME step.** `updateAgentRequest` embeds `createAgentRequest` (it is
defined as `struct { createAgentRequest; ImageTag string }`), so adding
`AccountID` to `createAgentRequest` here means Go's JSON field promotion
makes `account_id` a legal key on `POST /api/agents/{id}/update`'s body
too — for free, silently — even though this plan's Global Constraints
explicitly require that `UpdateRequest` never reads `account_id` from the
update request body at all. Today that would be harmless for an agent
already `backend=anthropic` (`resolveBackend`'s `existingAccountID` wins
over `req.AccountID` whenever it's non-empty), but for the ONE case where
`existingAccountID` is empty — an agent being switched from `ollama` to
`anthropic` by this very update — an attacker or careless client could
smuggle a specific `account_id` through the update body instead of the
intended "always lands on the operator's current default" behavior
(`internal/agent/update.go`'s call site passes `a.AnthropicAccountID`,
which is `""` for a record that was `ollama`, so `req.AccountID` becomes
load-bearing exactly in this one gap).

Close it in `handleUpdate` (around line 1282, where `toCreateRequest` is
called): explicitly clear the mapped field before building `UpdateRequest`:

```go
	createReq := toCreateRequest(req.createAgentRequest)
	createReq.AccountID = "" // see Task 6's plan note: never honor account_id on the update path
	a, err := h.mgr.Update(r.Context(), id, agent.UpdateRequest{
		CreateRequest: createReq,
		ImageTag:      req.ImageTag,
	})
```

(adjust to the exact surrounding variable names you find in the live file —
the shape above is the fix, not a literal patch). Add a test asserting that
a `POST /api/agents/{id}/update` body containing `"account_id"` has zero
effect on the resolved account for an ollama→anthropic switch (the switch
still lands on the operator's current default regardless of what
`account_id` the body named).

- [ ] **Step 5: Add `account_id` to the PATCH request**

Replace `patchAgentRequest` (lines 367-370):

```go
type patchAgentRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}
```

with:

```go
type patchAgentRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	// AccountID re-pins an existing anthropic-backend agent's Anthropic
	// account WITHOUT recreating its container -- the new credential
	// applies starting at the agent's next restart/wake. Rejected (400) if
	// the agent is not currently backend=anthropic.
	AccountID *string `json:"account_id"`
}
```

Replace `handleRename` (lines 1243-1263):

```go
func (h *Handler) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req patchAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if req.Name == nil && req.Description == nil && req.AccountID == nil {
		writeError(w, http.StatusBadRequest, CodeMissingField,
			"provide at least one of \"name\", \"description\" or \"account_id\" to update", "")
		return
	}

	a, err := h.mgr.Rename(r.Context(), id, req.Name, req.Description, req.AccountID)
	if err != nil {
		switch {
		case store.IsNotFound(err):
			writeError(w, http.StatusNotFound, CodeNotFound, "no such agent", "")
		case agent.IsAgentNotAnthropic(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "the agent is not on the anthropic backend", "account_id")
		case agent.IsUnknownAnthropicAccount(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "unknown anthropic account", "account_id")
		default:
			h.internalError(w, "renaming agent "+id, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, a)
}
```

- [ ] **Step 6: Run the full API suite**

```sh
go build ./...
go test ./internal/api/... -v
gofmt -l internal/api/
```

Expected: every test PASSes.

- [ ] **Step 7: Commit**

```sh
git add internal/api/handlers.go internal/api/handlers_test.go
git commit -m "$(cat <<'EOF'
api: account_id on create and PATCH, default id on the agent list

POST /api/agents accepts an optional account_id (unknown -> 400);
GET /api/agents carries default_anthropic_account_id for the create
form's prefill (best-effort, degrades to "" on a store read failure
rather than failing the whole list); PATCH /api/agents/{id} accepts
account_id to re-pin an existing anthropic agent without recreating
its container (400 if the agent isn't on the anthropic backend, or
the account is unknown).

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Web (render.js) — Accounts panel markup and the create-form `<select>`

**Files:**
- Modify: `docker-operator/web/render.js`
- Modify: `docker-operator/web/render.test.js`
- Modify: `docker-operator/web/style.css`

**Interfaces:**
- Produces (consumed by Task 8, Task 9):
  ```js
  Render.renderAnthropicAccountsPanel(accounts, opts) // opts: {error, busy}
  // renderCreateForm(defaults, opts) now reads defaults.anthropicAccounts
  // (array of {id, name, kind, is_default, ...}) and
  // defaults.defaultAnthropicAccountId, and values.anthropic_account_id,
  // rendering a <select class="create-form__anthropic-account"> inside the
  // existing .create-form__anthropic-note block.
  ```

- [ ] **Step 1: Write the failing render tests**

In `web/render.test.js`, find the existing `describe`/`test` block(s) for
`renderAnthropicStatus` (per the earlier research, this function renders
the OLD single-credential status line) and the `renderCreateForm` tests.
Add:

```js
test('renderAnthropicAccountsPanel lists accounts with a default marker', () => {
  const html = Render.renderAnthropicAccountsPanel([
    { id: 'anc_1', name: 'Work', kind: 'api_key', updated_at: '2026-09-01T00:00:00Z', is_default: true },
    { id: 'anc_2', name: 'Personal', kind: 'oauth', updated_at: '2026-09-02T00:00:00Z', is_default: false },
  ], {});
  assert.match(html, /Work/);
  assert.match(html, /Personal/);
  assert.match(html, /anthropic-accounts__default-badge/);
  // The non-default row (Personal) gets a Set-default button; the default
  // row (Work) does not.
  assert.ok(html.includes('anthropic-accounts__set-default" type="button" data-id="anc_2"'));
  assert.ok(!html.includes('anthropic-accounts__set-default" type="button" data-id="anc_1"'));
});

test('renderAnthropicAccountsPanel renders an empty state with no accounts', () => {
  const html = Render.renderAnthropicAccountsPanel([], {});
  assert.match(html, /No Anthropic accounts yet/);
});

test('renderAnthropicAccountsPanel surfaces an error', () => {
  const html = Render.renderAnthropicAccountsPanel([], { error: 'boom' });
  assert.match(html, /boom/);
});

test('renderCreateForm renders the anthropic account select, pre-selected to the default', () => {
  const defaults = {
    backend: 'anthropic',
    anthropicAccounts: [
      { id: 'anc_1', name: 'Work' },
      { id: 'anc_2', name: 'Personal' },
    ],
    defaultAnthropicAccountId: 'anc_2',
  };
  const html = Render.renderCreateForm(defaults, {});
  assert.match(html, /create-form__anthropic-account/);
  assert.match(html, /<option value="anc_2" selected>Personal<\/option>/);
});

test('renderCreateForm pre-selects the AGENT\'S pinned account on the update form, not the operator default', () => {
  const defaults = {
    anthropicAccounts: [
      { id: 'anc_1', name: 'Work' },
      { id: 'anc_2', name: 'Personal' },
    ],
    defaultAnthropicAccountId: 'anc_2',
  };
  const html = Render.renderCreateForm(defaults, { values: { backend: 'anthropic', anthropic_account_id: 'anc_1' } });
  assert.match(html, /<option value="anc_1" selected>Work<\/option>/);
});
```

- [ ] **Step 2: Run the tests to verify they fail**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js
```

Expected: FAIL — `Render.renderAnthropicAccountsPanel` does not exist; the
create-form tests fail because no `.create-form__anthropic-account` select
is rendered yet.

- [ ] **Step 3: Add `renderAnthropicAccountsPanel`**

In `web/render.js`, keep `renderAnthropicStatus` (lines 561-573) for now —
Task 8 removes its call site once the new panel replaces it; deleting it
here would leave a dangling reference until Task 8 lands. Add right after
it:

```js
	// renderAnthropicAccountsPanel renders the Settings "Anthropic Accounts"
	// panel's table (one row per stored account: name, kind, last updated,
	// a Set-default button for non-default rows, a Remove button for every
	// row) plus the two "Add account" buttons. opts.error, if set, is shown
	// above the table (a failed list/create/delete/set-default); opts.busy
	// disables the add buttons while one of those requests is in flight.
	function renderAnthropicAccountsPanel(accounts, opts) {
		opts = opts || {};
		accounts = accounts || [];
		// Sorted for DISPLAY only -- the store itself stays ID-ordered, the
		// same division of labor renderTemplateBar already uses for templates.
		var sorted = accounts.slice().sort(function (a, b) {
			return String(a.name).localeCompare(String(b.name));
		});
		var rows = sorted.map(function (a) {
			var kindLabel = a.kind === 'oauth' ? 'OAuth token' : 'API key';
			var when = a.updated_at ? new Date(a.updated_at) : null;
			var whenText = when && !isNaN(when.getTime()) ? when.toISOString().slice(0, 10) : '—';
			return (
				'<tr class="anthropic-accounts__row">' +
					'<td>' + escapeHTML(a.name) +
						(a.is_default ? ' <span class="anthropic-accounts__default-badge">Default</span>' : '') +
					'</td>' +
					'<td>' + escapeHTML(kindLabel) + '</td>' +
					'<td>' + escapeHTML(whenText) + '</td>' +
					'<td>' + (a.is_default ? '' :
						'<button class="anthropic-accounts__set-default btn btn--ghost btn--sm" type="button" data-id="' + escapeHTML(a.id) + '">Set default</button>'
					) + '</td>' +
					'<td><button class="anthropic-accounts__remove btn btn--danger btn--sm" type="button" data-id="' + escapeHTML(a.id) + '" data-name="' + escapeHTML(a.name) + '">Remove</button></td>' +
				'</tr>'
			);
		}).join('');
		var empty = accounts.length === 0
			? '<tr><td colspan="5" class="anthropic-accounts__empty">No Anthropic accounts yet.</td></tr>'
			: '';
		return (
			(opts.error ? '<p class="anthropic-accounts__error" role="alert">' + escapeHTML(opts.error) + '</p>' : '') +
			'<table class="anthropic-accounts__table">' +
				'<thead><tr><th>Name</th><th>Kind</th><th>Updated</th><th></th><th></th></tr></thead>' +
				'<tbody>' + rows + empty + '</tbody>' +
			'</table>' +
			'<div class="anthropic-accounts__actions">' +
				'<button class="anthropic-accounts__add-apikey btn btn--ghost btn--sm" type="button"' + (opts.busy ? ' disabled' : '') + '>Add via API key</button>' +
				'<button class="anthropic-accounts__add-login btn btn--ghost btn--sm" type="button"' + (opts.busy ? ' disabled' : '') + '>Add via login</button>' +
			'</div>'
		);
	}
```

- [ ] **Step 4: Rename the Settings section and wire the new panel in**

Change `SETTINGS_SECTIONS` (line 514-517):

```js
	var SETTINGS_SECTIONS = [
		{ id: 'anthropic-account', title: 'Anthropic account' },
		{ id: 'agent-image', title: 'Agent image' },
	];
```

to:

```js
	var SETTINGS_SECTIONS = [
		{ id: 'anthropic-account', title: 'Anthropic Accounts' },
		{ id: 'agent-image', title: 'Agent image' },
	];
```

(The `id` stays `'anthropic-account'` on purpose — it is the
`data-settings-body`/`data-settings-nav` key app.js already queries by;
only the displayed `title` changes.)

- [ ] **Step 5: Add the account `<select>` to `renderCreateForm`**

In `renderCreateForm` (around line 234-402), add right after the `pick`
helper is defined (line 242), alongside the other `pick`-derived values:

```js
		var accountID = pick(values.anthropic_account_id, defaults.defaultAnthropicAccountId);
		var accounts = defaults.anthropicAccounts || [];
		var sortedAccounts = accounts.slice().sort(function (a, b) {
			return String(a.name).localeCompare(String(b.name));
		});
		var accountOptionsHTML = sortedAccounts.length === 0
			? '<option value="">(no accounts configured)</option>'
			: sortedAccounts.map(function (a) {
				return '<option value="' + escapeHTML(a.id) + '"' + (a.id === accountID ? ' selected' : '') + '>' + escapeHTML(a.name) + '</option>';
			}).join('');
```

Then replace the single note paragraph (line 363):

```js
					'<p class="create-form__anthropic-note" hidden>Uses the shared Anthropic login (set it in Settings first).</p>' +
```

with:

```js
					'<div class="create-form__anthropic-note" hidden>' +
						'<label class="create-form__row">Anthropic account' +
							'<select class="create-form__anthropic-account">' + accountOptionsHTML + '</select>' +
						'</label>' +
						'<span class="create-form__help">Manage accounts in Settings.</span>' +
					'</div>' +
```

Keeping the class name `create-form__anthropic-note` unchanged means
`web/app.js`'s and `web/terminal.js`'s existing hide/show toggles (which
query that exact class) need NO changes for visibility — only their submit
bodies change, in Tasks 8 and 9.

- [ ] **Step 6: Export the new function**

In the module's export object near the end of the file (around line
1156-1168), add `renderAnthropicAccountsPanel: renderAnthropicAccountsPanel,`
next to the existing `renderAnthropicStatus: renderAnthropicStatus,` line
(leave that old export in place — Task 8 removes it once nothing calls it).

- [ ] **Step 7: Add the CSS**

In `web/style.css`, add after the existing `.anthropic-panel__actions` block
(around line 625-629) — mirroring `.agent-image-panel__tags`'s existing
table styling exactly (lines 657-667):

```css
.anthropic-accounts__table { width: 100%; border-collapse: collapse; margin-bottom: var(--space-3); }
.anthropic-accounts__table th {
	text-align: left;
	font-size: var(--font-xs);
	font-weight: 600;
	text-transform: uppercase;
	letter-spacing: 0.04em;
	color: var(--fg-dim);
	padding: 2px 8px 6px 0;
}
.anthropic-accounts__table td { padding: 4px 8px 4px 0; vertical-align: middle; }
.anthropic-accounts__table td:last-child,
.anthropic-accounts__table td:nth-last-child(2) { text-align: right; white-space: nowrap; }
.anthropic-accounts__default-badge {
	font-size: var(--font-xs);
	color: var(--status-running);
	border: 1px solid var(--status-running);
	border-radius: 3px;
	padding: 0 4px;
}
.anthropic-accounts__empty { color: var(--fg-dim); font-style: italic; padding: 4px 0; }
.anthropic-accounts__error { margin: 0 0 var(--space-3); color: var(--status-error); }
.anthropic-accounts__actions { display: flex; flex-wrap: wrap; gap: 4px; }
```

- [ ] **Step 8: Run the tests, sync the embed**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js
```

Expected: every test PASSes, including every pre-existing `renderCreateForm`
test (double-check none of them assert the EXACT old
`<p class="create-form__anthropic-note"...>` string, which no longer
exists — if one does, update its assertion to match the new `<div>` wrapper
rather than weakening what it checks).

Then, from `docker-operator/`:

```sh
make sync-web-embed
make web-embed-check
```

- [ ] **Step 9: Commit**

```sh
git add web/render.js web/render.test.js web/style.css internal/webui/web/render.js
git commit -m "$(cat <<'EOF'
web: Anthropic Accounts panel markup and the create-form account select

renderAnthropicAccountsPanel renders the Settings table (name, kind,
updated, set-default, remove) with an empty state and error line,
mirroring the agent-image panel's existing table CSS. renderCreateForm
gains an account <select> inside the existing
.create-form__anthropic-note block, pre-selected to the operator
default on create or the agent's own pin on update -- the toggle
mechanism app.js/terminal.js already use for that block is untouched.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Web (app.js) — Accounts panel wiring, create-form submit, polling

**Files:**
- Modify: `docker-operator/web/app.js`
- Modify: `docker-operator/web/render.js` (remove the now-dead
  `renderAnthropicStatus`, per Step 3 below)
- Modify: `docker-operator/web/render.test.js` (remove its now-dead test,
  per Step 3 below)

**Interfaces:**
- Consumes: Task 7's `Render.renderAnthropicAccountsPanel`.
- Produces: `state.anthropicAccounts`; `window.getAnthropicAccounts()`
  (consumed by Task 9); `state.defaults.defaultAnthropicAccountId`.

Task 7 deliberately left `render.js`'s old `renderAnthropicStatus` function
and its export in place, since app.js (Task 7 doesn't touch it) was still
calling it at the time. This task's Step 3 deletes BOTH the app.js call
sites AND, since that was its only caller, `renderAnthropicStatus` itself
(the function, its line in the exports object, and
`render.test.js`'s test for it) — leaving it would be dead code testing a
function nothing calls, the kind of thing a final review flags.

This task has no isolated unit tests of its own (app.js's DOM-wiring
functions are not covered by `render.test.js`, consistent with how
`refreshAnthropicPanel`/`wireAgentImagePanel` aren't either today) — verify
it via the full `node --test web/*.test.js` run (nothing should regress)
plus the `run` skill against a live operator once Task 10's final
integration step is reached. There is no isolated TDD red/green cycle here;
go straight to implementation, matching this codebase's existing practice
for app.js wiring code.

- [ ] **Step 1: Add `state.anthropicAccounts` and its getter**

Near the top of the module where `state` is declared (the object literal
that already holds `agents`, `maxAgents`, `defaults`, etc. — search for
`var state = {`), add `anthropicAccounts: [],` to its initial value.

Near `window.getAgentDefaults` (around line 891), add right after it:

```js
	window.getAnthropicAccounts = function () { return state.anthropicAccounts; };
```

- [ ] **Step 2: Update `buildFormDefaults`**

Change (line 148-150):

```js
	function buildFormDefaults() {
		return Object.assign({}, state.defaults, { agentImage: state.agentImage });
	}
```

to:

```js
	function buildFormDefaults() {
		return Object.assign({}, state.defaults, { agentImage: state.agentImage, anthropicAccounts: state.anthropicAccounts });
	}
```

- [ ] **Step 3: Replace the Anthropic Settings-panel functions**

In `web/app.js`, delete `refreshAnthropicPanel`, `renderAnthropicPanel`,
`putAnthropicAuth`, and the old no-argument `startAnthropicLogin` (lines
573-614, 754-787). Replace with:

```js
	// --- Settings section: Anthropic Accounts -------------------------------

	var anthropicAccountBusy = false;
	var anthropicAccountError = null;

	function renderAnthropicAccountsPanelNow() {
		var body = settingsBody('anthropic-account');
		if (!body) return;
		body.innerHTML = window.Render.renderAnthropicAccountsPanel(state.anthropicAccounts, {
			busy: anthropicAccountBusy,
			error: anthropicAccountError,
		});
		wireAnthropicAccountsPanel();
	}

	// refreshAnthropicAccounts ALWAYS updates state.anthropicAccounts (so the
	// create/update forms' account <select> stays fresh via the 3s poll
	// whether or not Settings is open), and only then repaints the section
	// body if it is on screen -- the same division of labor
	// refreshAgentImagePanel already uses for state.agentImage.
	function refreshAnthropicAccounts() {
		return fetchJSON('/api/anthropic/accounts')
			.then(function (data) {
				state.anthropicAccounts = (data && data.accounts) || [];
				anthropicAccountError = null;
				renderAnthropicAccountsPanelNow();
			})
			.catch(function (e) {
				anthropicAccountError = 'unavailable: ' + e.message;
				var body = settingsBody('anthropic-account');
				if (!body) return;
				renderAnthropicAccountsPanelNow();
			});
	}

	function wireAnthropicAccountsPanel() {
		var body = settingsBody('anthropic-account');
		if (!body) return;

		var apikeyBtn = body.querySelector('.anthropic-accounts__add-apikey');
		if (apikeyBtn) apikeyBtn.addEventListener('click', function () {
			var name = window.prompt('Name this account:');
			if (!name || !name.trim()) return;
			var key = window.prompt('Paste the Anthropic API key (starts with sk-ant-):');
			if (!key) return;
			createAnthropicAccount({ name: name.trim(), kind: 'api_key', value: key.trim() });
		});

		var loginBtn = body.querySelector('.anthropic-accounts__add-login');
		if (loginBtn) loginBtn.addEventListener('click', function () {
			var name = window.prompt('Name this account:');
			if (!name || !name.trim()) return;
			startAnthropicLogin(name.trim());
		});

		body.querySelectorAll('.anthropic-accounts__set-default').forEach(function (btn) {
			btn.addEventListener('click', function () {
				var id = btn.getAttribute('data-id');
				fetchJSON('/api/anthropic/accounts/' + encodeURIComponent(id) + '/default', { method: 'PUT' })
					.then(refreshAnthropicAccounts)
					.catch(alertErr('Could not set the default account'));
			});
		});

		body.querySelectorAll('.anthropic-accounts__remove').forEach(function (btn) {
			btn.addEventListener('click', function () {
				var id = btn.getAttribute('data-id');
				var name = btn.getAttribute('data-name');
				window.OperatorConfirm.show(
					'Agents already pinned to "' + name + '" will fail to start again (until repointed to a different account) the next time they restart or wake.',
					{ title: 'Remove the account "' + name + '"?', confirmLabel: 'Remove', danger: true }
				).then(function (confirmed) {
					if (!confirmed) return;
					fetchJSON('/api/anthropic/accounts/' + encodeURIComponent(id), { method: 'DELETE' })
						.then(refreshAnthropicAccounts)
						.catch(alertErr('Could not remove the account'));
				});
			});
		});
	}

	function createAnthropicAccount(payload) {
		return fetchJSON('/api/anthropic/accounts', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(payload),
		})
			.then(refreshAnthropicAccounts)
			.catch(alertErr('Could not create the account'));
	}

	function startAnthropicLogin(name) {
		fetchJSON('/api/anthropic/login', { method: 'POST' })
			.then(function () {
				if (typeof window.renderAnthropicLogin === 'function') {
					// The login terminal claims the main area, so the Settings
					// modal has to come down first -- otherwise its scrim sits
					// over the very terminal the user is meant to type into.
					closeSettings();
					window.renderAnthropicLogin(mainArea, {
						submitToken: function (token) {
							return createAnthropicAccount({ name: name, kind: 'oauth', value: token });
						},
						onClose: function () {
							fetchJSON('/api/anthropic/login', { method: 'DELETE' }).catch(function () { /* best effort */ });
							// Back to Settings, where the login was started --
							// it shows the freshly-created account.
							openSettings();
						},
					});
				}
			})
			.catch(alertErr('Could not start the login helper'));
	}
```

That was `renderAnthropicPanel`'s only caller of `window.Render.renderAnthropicStatus`
in this codebase — now go clean up its source. In `web/render.js`, delete
the `renderAnthropicStatus` function (the one rendering the old single
status line — do NOT confuse it with `renderAnthropicAccountsPanel`, Task 7's
new function, which stays) and remove its
`renderAnthropicStatus: renderAnthropicStatus,` line from the module's
export object. In `web/render.test.js`, delete whichever test(s) exercise
`renderAnthropicStatus` directly.

```sh
grep -n "renderAnthropicStatus" web/render.js web/render.test.js web/app.js web/terminal.js
```

should print nothing after this step (confirm before moving on — a leftover
match in `app.js`/`terminal.js` means a caller was missed, not that
`render.js`'s definition is still allowed to exist).

- [ ] **Step 4: Update every call site**

In `openSettings` (around line 554), change:

```js
		refreshAnthropicPanel();
```

to:

```js
		refreshAnthropicAccounts();
```

In `refreshAgents` (around line 99-108), add
`defaultAnthropicAccountId: data.default_anthropic_account_id || '',` to the
`state.defaults = {...}` literal.

At initial load (around line 897-901) and in the `setInterval` (around line
906-909), add `refreshAnthropicAccounts()` alongside the existing
`refreshAgentImagePanel()` calls:

```js
	refreshAgentImagePanel();
	refreshAnthropicAccounts();
	...
	setInterval(function () {
		refreshAgents().catch(function () { /* transient failure; retried next tick */ });
		refreshAgentImagePanel().catch(function () { /* transient failure; retried next tick */ });
		refreshAnthropicAccounts().catch(function () { /* transient failure; retried next tick */ });
	}, 3000);
```

- [ ] **Step 5: Add `account_id` to the create-form submit**

In `wireCreateForm`'s submit handler (around line 262-267), right after the
existing `if (backend === 'ollama') { ... }` block, add:

```js
			if (backend === 'anthropic') {
				var accountSel = form.querySelector('.create-form__anthropic-account');
				var defaultAccountId = state.defaults.defaultAnthropicAccountId || '';
				if (accountSel && accountSel.value && accountSel.value !== defaultAccountId) {
					body.account_id = accountSel.value;
				}
			}
```

- [ ] **Step 6: Run the full JS suite**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
```

Expected: every test PASSes. `terminal.test.js`/`auth.test.js`/
`files.test.js` must simply keep passing unmodified (app.js itself has no
dedicated test file per this codebase's existing layout); `render.test.js`
passes WITHOUT the test you removed for `renderAnthropicStatus` in Step 3.

```sh
make sync-web-embed
make web-embed-check
```

- [ ] **Step 7: Commit**

```sh
git add web/app.js web/render.js web/render.test.js internal/webui/web/app.js internal/webui/web/render.js
git commit -m "$(cat <<'EOF'
web: wire the Anthropic Accounts Settings panel and create-form submit

Replaces the single-credential panel wiring (refreshAnthropicPanel/
putAnthropicAuth) with list/create/delete/set-default against
/api/anthropic/accounts, polled every 3s like the agent-image tags
already are. The create form sends account_id only when it differs
from the operator's current default, the same convention every other
optional field in that form already follows. Removes render.js's
renderAnthropicStatus now that this was its last caller.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Web (terminal.js) — Update-form account select, split PATCH submit

**Files:**
- Modify: `docker-operator/web/terminal.js`

**Interfaces:**
- Consumes: Task 8's `window.getAnthropicAccounts()`.

Like Task 8, this has no isolated unit test — verify via the existing
`web/terminal.test.js` suite (must keep passing) plus the `run` skill at
Task 10.

- [ ] **Step 1: Carry `anthropicAccounts` into the update form's defaults**

In `openUpdateForm` (around line 606-607):

```js
			var opDefaults = (typeof window.getAgentDefaults === 'function' && window.getAgentDefaults()) || {};
			var defaults = Object.assign({}, opDefaults, { agentImage: byHarness });
```

change the second line to:

```js
			var defaults = Object.assign({}, opDefaults, {
				agentImage: byHarness,
				anthropicAccounts: (typeof window.getAnthropicAccounts === 'function' && window.getAnthropicAccounts()) || [],
			});
```

- [ ] **Step 2: Split the submit — account change via PATCH, everything else via the existing recreate**

In `openUpdateForm`'s submit handler (around line 640-689), the `agent`
variable from the outer `.then(function (res) { var agent = res[0]; ... })`
closure (line 597) is already in scope here. Add, right after the existing
`body` object is fully built (after the `if (backend === 'ollama') { ... }`
block, before the `window.OperatorConfirm.show(...)` call):

```js
				// The Anthropic account selector is submitted SEPARATELY, through
				// PATCH (store-only, no container recreate) -- see
				// docs/superpowers/specs/2026-10-08-multi-anthropic-accounts-design.md.
				// It is never part of `body`. It only applies when the agent was
				// ALREADY backend=anthropic before this submit: switching an
				// ollama agent to anthropic in this same submit always lands on
				// the operator's current default account (resolveBackend's own
				// fallback) -- a second update-form visit, once backend=anthropic
				// is persisted, is what lets the user then pick a different one.
				var accountSel = form.querySelector('.create-form__anthropic-account');
				var wasAnthropic = agent.backend === 'anthropic';
				var newAccountID = (wasAnthropic && accountSel) ? accountSel.value : '';
				var accountChanged = wasAnthropic && newAccountID && newAccountID !== (agent.anthropic_account_id || '');
```

Then change the confirm/submit continuation from:

```js
				window.OperatorConfirm.show(
					'It recreates the container (ending the running session) but keeps the volumes and history.',
					{ title: 'Update this agent?', confirmLabel: 'Update' }
				).then(function (confirmed) {
					if (!confirmed) return;

					errorEl.hidden = true;
					submitBtn.disabled = true;
					fetchJSON('/api/agents/' + encodeURIComponent(agentID) + '/update', {
						method: 'POST',
						headers: { 'Content-Type': 'application/json' },
						body: JSON.stringify(body),
					})
						.then(function () {
							if (typeof window.onAgentUpdated === 'function') window.onAgentUpdated(agentID);
							else renderAgentDetail(mainArea, agentID);
						})
						.catch(function (e) {
							submitBtn.disabled = false;
							errorEl.textContent = e && e.message ? e.message : String(e);
							errorEl.hidden = false;
						});
				});
```

to:

```js
				window.OperatorConfirm.show(
					'It recreates the container (ending the running session) but keeps the volumes and history.',
					{ title: 'Update this agent?', confirmLabel: 'Update' }
				).then(function (confirmed) {
					if (!confirmed) return;

					errorEl.hidden = true;
					submitBtn.disabled = true;

					// The account PATCH runs first and independently: it never
					// touches Docker, so even if the recreate below fails, the
					// account pin is already correct for the next retry.
					var accountPatch = accountChanged
						? fetchJSON('/api/agents/' + encodeURIComponent(agentID), {
							method: 'PATCH',
							headers: { 'Content-Type': 'application/json' },
							body: JSON.stringify({ account_id: newAccountID }),
						})
						: Promise.resolve();

					accountPatch
						.then(function () {
							return fetchJSON('/api/agents/' + encodeURIComponent(agentID) + '/update', {
								method: 'POST',
								headers: { 'Content-Type': 'application/json' },
								body: JSON.stringify(body),
							});
						})
						.then(function () {
							if (typeof window.onAgentUpdated === 'function') window.onAgentUpdated(agentID);
							else renderAgentDetail(mainArea, agentID);
						})
						.catch(function (e) {
							submitBtn.disabled = false;
							errorEl.textContent = e && e.message ? e.message : String(e);
							errorEl.hidden = false;
						});
				});
```

- [ ] **Step 3: Run the full JS suite**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
```

Expected: every test PASSes.

```sh
make sync-web-embed
make web-embed-check
```

- [ ] **Step 4: Commit**

```sh
git add web/terminal.js internal/webui/web/terminal.js
git commit -m "$(cat <<'EOF'
web: update-form account changes go through PATCH, not the recreate

The account <select> in the update-agent form is excluded from the
POST .../update body entirely; a changed selection (only meaningful
when the agent is already backend=anthropic) is submitted as its own
PATCH /api/agents/{id} {account_id} request, run before the recreate
so the pin is already correct even if the recreate itself fails.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Final gate, PR, CI, merge

**Files:** none (verification and process only).

- [ ] **Step 1: Run the full Go gate**

From `docker-operator/`, inside the project's Go/lint containers (per
`use-docker` and the memory note on golangci-lint via its prebuilt image):

```sh
go vet ./...
gofmt -l .
go test ./... -race
docker run --rm -v /workspace/ai-sandbox/docker-operator:/app -w /app golangci/golangci-lint:v2.12.2 golangci-lint run ./...
govulncheck ./...
```

Fix anything red before proceeding. If the ONLY failure is
`internal/wsbridge`'s `TestIntegrationTerminalReconnectShowsScrollback`
timing out, that is the known flake recorded in this project's memory —
re-run once before treating it as a regression.

- [ ] **Step 2: Run the full web gate and the embed check**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
make web-embed-check
```

**Controller-ruling cleanup (found during Task 8's review, no task owned it):**
`web/style.css` lines ~621-629 (`.anthropic-panel__status`, `.anthropic-panel__status--set`,
`.anthropic-panel__status--unset`, `.anthropic-panel__actions`) are dead —
the last JS reference to them (`renderAnthropicStatus`/`renderAnthropicPanel`)
was removed in Task 8. Delete these four rules now, from both `web/style.css`
and `internal/webui/web/style.css` (`make sync-web-embed` after editing the
source, as usual), and fold that into this step's verification pass before
moving to `make all`.

- [ ] **Step 3: `make all`**

```sh
cd docker-operator && make all
```

This runs `vet fmt-check lint test skill-check dind-init-check web-test
web-embed-check` in one pass — the cheapest point to catch anything the
per-task steps above missed before pushing.

- [ ] **Step 4: Smoke-check the migration path manually (no code change)**

This plan's migration (Task 2) only has automated coverage against a
synthetic seeded file (`migration_test.go`). Before merging, if a real
pre-feature `state.db` with a configured shared credential is available
(e.g. from this deployment's own history), copy it aside, point a local
`docker-operator` binary at the copy via `STATE_DB_PATH`, start it, and
confirm: exactly one account named "Default" appears in
`GET /api/anthropic/accounts`, it is marked default, and any pre-existing
`backend=anthropic` agent's record shows the matching
`anthropic_account_id`. If no such file is available, note that explicitly
in the PR description rather than skipping silently.

- [ ] **Step 5: Push, open the PR, wait for CI, merge**

```sh
git push -u origin feat/multi-anthropic-accounts
```

Open a PR via git-proxy (see the `use-git-proxy` skill) with a description
summarizing the feature and linking
`docs/superpowers/specs/2026-10-08-multi-anthropic-accounts-design.md`.
Wait for CI. On green, merge (per the user's standing "merge each on green"
authorization for this kind of work, if it still applies to this session —
otherwise ask before merging).

After merge, tell the user explicitly: this requires
`docker compose up -d --build docker-operator` to take effect, and the new
Settings "Anthropic Accounts" panel, the create/update forms' account
picker, and the migration of any existing shared credential need a human's
eyes in a browser.
