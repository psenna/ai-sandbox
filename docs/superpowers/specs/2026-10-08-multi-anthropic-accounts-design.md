# Design: multiple named Anthropic accounts

**Status:** design, not yet implemented.

**Branch:** `feat/multi-anthropic-accounts`, branched from `main`.

## Why

Today `docker-operator` stores exactly **one** shared Anthropic credential
(`internal/store/store.go`, `bucketSettings` singleton key `anthropic_auth`),
set either by pasting an API key or by running `claude setup-token`
interactively in a one-off container with a browser-attached terminal
(`internal/agent/anthropiclogin.go`). Every `backend=anthropic` agent uses
that one credential implicitly — there is no per-agent selection anywhere.

The user wants multiple named Anthropic accounts: store several
`(name, token)` pairs, pick which one a given agent uses on the new-agent and
update-agent forms, mark one as the default that pre-fills the create form,
and manage the set (login, list by name only, remove, set default) from a
Settings panel. Tokens must never be sent to the web UI.

## Decisions locked during brainstorming

1. **Delete is unconditional.** Removing an account always succeeds
   immediately, even if it's the default or agents reference it. No
   reference-counting, no blocking.
2. **Migration: auto-promote the existing singleton.** On first boot after
   this ships, the existing shared credential (if any) becomes an account
   named "Default" and is marked default. Existing agents are pinned to it.
   Zero user action required.
3. **Dangling pin fails closed.** If an agent's pinned account is later
   deleted, the agent errors (same fail-closed shape as today's
   `ErrNoAnthropicAuth`, HTTP 409) the next time it needs new env vars
   (create, restart, wake) — it does not silently fall back to whatever is
   currently default.
4. **Account changes apply on next natural restart/wake, not immediately.**
   Editing a running agent's pinned account only changes the stored value;
   the operator does not stop/recreate the container to apply it early.
5. **Naming happens before the credential flow.** "Add account" asks for a
   name first (small modal: name + choice of paste-API-key vs. OAuth login),
   then proceeds into the existing prompt or terminal flow unchanged.
6. **No in-place re-authentication in v1.** There is no "refresh this
   account's token" action. An expired/bad token means delete the account and
   add a new one (same name is fine). YAGNI — not in the user's feature list.
7. **Pin resolves "default" at the moment of use, not dynamically.** Both new
   agents created without an explicit account, and legacy agents migrated
   from the old singleton, get a concrete `AnthropicAccountID` stamped once.
   Changing the default account later only affects agents created
   afterward — it never silently moves an existing agent's credential.
8. **No new encryption-at-rest.** Accounts are stored the same way the
   existing singleton credential is: plaintext JSON in BoltDB, 0600 file
   mode. This is a straight continuation of today's trust boundary, not a
   new risk introduced by going from one credential to several. Real
   encryption at rest would need a key-management story (where does the key
   live?) that doesn't exist anywhere in this stack — out of scope.

## Data model

New BoltDB bucket `bucketAnthropicAccounts` (sibling to `bucketAgents`,
`bucketSettings`, `bucketTemplates` in `internal/store/store.go`), keyed by
ID. CRUD mirrors the existing per-record pattern used for agents (`Create`,
`Get`, `List`, `Update`, `Delete`, transactional mutate-and-stamp
`UpdatedAt`), **not** the singleton-in-`bucketSettings` pattern it replaces.

```go
// internal/store/anthropicaccount.go (new file)

type AnthropicAccount struct {
    ID        string    `json:"id"`         // "anc_" + 4 random bytes hex, via NewID()-style helper
    Name      string    `json:"name"`       // must be unique (enforced on Create/Update)
    Kind      string    `json:"kind"`       // "api_key" | "oauth" — reuses AnthropicKindAPIKey/AnthropicKindOAuth
    Value     string    `json:"value"`      // the secret; NEVER serialized over the HTTP API
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

A new singleton settings key `default_anthropic_account_id` (string, empty
meaning "no default set") lives in the existing `bucketSettings` bucket —
this one genuinely is a singleton (there is exactly one current default), so
it keeps the pattern `AnthropicAuth` used to use.

`store.Agent` gains one field:

```go
AnthropicAccountID string `json:"anthropic_account_id,omitempty"` // empty for backend=ollama agents
```

persisted and round-tripped exactly like `Harness`/`Backend` today.

## Resolution semantics

`internal/agent/create.go`:

- `CreateRequest`/`UpdateRequest` gain `AccountID string` (JSON
  `account_id`, omitted/empty meaning "use current default").
- `resolveBackend`, for `kind == config.BackendAnthropic`:
  - If `AccountID` is non-empty: look it up via the new store method. Unknown
    ID → a new sentinel `ErrUnknownAnthropicAccount` → `IsUnknownAnthropicAccount`
    → HTTP 400 ("unknown Anthropic account"). This mirrors the existing
    `ErrNoAnthropicAuth`/`IsNoAnthropicAuth` → 409 wiring at
    `handlers.go:922-923`.
  - If `AccountID` is empty: read `default_anthropic_account_id`. If unset or
    it points at a since-deleted account, fail the same way today's
    "no credential configured" case does — `ErrNoAnthropicAuth` → 409. If it
    resolves, **stamp that concrete account ID onto the agent now** (decision
    #7) rather than storing "use default."
  - On success, `resolveBackend` fetches that account's `(Kind, Value)` and
    builds `rb.apiKey`/`rb.oauthToken` for `applyBackendEnv` exactly as it
    does today from the singleton — `applyBackendEnv` itself is unchanged.
- `internal/agent/update.go`: an `UpdateRequest.AccountID` change only
  rewrites `Agent.AnthropicAccountID` in the store (decision #4) — no
  container recreate is triggered by this field, same as how other
  non-structural fields behave today.
- When an agent with a pinned `AnthropicAccountID` next needs env vars
  (create, restart, wake) and that account has been deleted in the
  meantime, the lookup fails and surfaces as the existing
  `ErrNoAnthropicAuth`-shaped 409 (decision #3) — the message should name
  the missing account so it's debuggable, not just "no credential
  configured."

## Store methods (new, `internal/store/anthropicaccount.go`)

```go
func (s *Store) CreateAnthropicAccount(ctx context.Context, name, kind, value string) (AnthropicAccount, error)
func (s *Store) GetAnthropicAccount(ctx context.Context, id string) (AnthropicAccount, error)
func (s *Store) ListAnthropicAccounts(ctx context.Context) ([]AnthropicAccount, error)
func (s *Store) DeleteAnthropicAccount(ctx context.Context, id string) error // always succeeds if present; ErrNotFound if not
func (s *Store) DefaultAnthropicAccountID(ctx context.Context) (string, error) // "" if unset
func (s *Store) SetDefaultAnthropicAccount(ctx context.Context, id string) error // validates id exists
```

`DeleteAnthropicAccount` does not check agent references (decision #1); if
the deleted ID was the stored default, `DefaultAnthropicAccountID` simply
returns `""` afterward (no cleanup needed — it's just a string that no
longer resolves to anything, read lazily).

Uniqueness: `CreateAnthropicAccount` lists existing accounts and rejects a
duplicate `Name` with a new sentinel (`ErrAnthropicAccountNameTaken` → 409 or
400, consistent with other validation errors in this file).

## Login / add-account flow

`internal/agent/anthropiclogin.go` keeps the same singleton login container
(`docker-operator-anthropic-login`) and idle janitor — only one add-account
flow in flight across the whole web UI at a time, consistent with this being
an admin action, not a per-agent one.

Changes:

- `StartAnthropicLogin` gains a `name string` parameter, stashed in-memory on
  the `Manager` alongside the existing container-lifecycle state (not
  persisted — if the operator restarts mid-login, the login is simply lost,
  same as today's existing login-state handling implicitly assumes).
- The finish step (today's `PUT /api/anthropic/auth` after a successful
  paste-or-terminal flow) becomes "create a new account with the stashed
  name, this kind, this value" via `store.CreateAnthropicAccount`, instead of
  overwriting the old singleton via `SetAnthropicAuth`.
- The direct-paste path (no terminal) also needs the name up front, since the
  web UI's "Add account" modal asks for it before either sub-flow starts.
- If this is the very first account ever created, it is automatically also
  made the default (otherwise the create form would have nothing to
  pre-fill).

## API (`internal/api/handlers.go`)

Replacing the old `GET/PUT/DELETE /api/anthropic/auth`:

```
GET    /api/anthropic/accounts            -> [{id, name, kind, created_at, updated_at, is_default}]
POST   /api/anthropic/accounts            <- {name, kind, value}     (direct API-key path)
DELETE /api/anthropic/accounts/{id}       -> 204 always (404 only if id never existed)
PUT    /api/anthropic/accounts/{id}/default -> 204, sets default
```

Unchanged shape, `name` added to the request body:

```
GET    /api/anthropic/login               -> {active, ws}
POST   /api/anthropic/login               <- {name}
DELETE /api/anthropic/login               -> stop/remove (same as today)
```

The list response's `is_default` is computed from
`DefaultAnthropicAccountID` so the UI never needs a second round trip. No
response here, or anywhere else in this feature, ever includes `value`.

The old `/api/anthropic/auth` endpoints and `store.AnthropicAuth` type /
`GetAnthropicAuth`/`SetAnthropicAuth`/`ClearAnthropicAuth` methods are
removed outright once migration (below) has run — this is an
operator-internal API with no external consumers to keep compatible.

## UI (`web/render.js`, `web/app.js`)

- `SETTINGS_SECTIONS` entry renamed from "Anthropic account" to "Anthropic
  Accounts". Its panel becomes a table: Name / Kind / Updated / default
  marker / "Set default" / "Remove", plus an "Add account" button that opens
  a small modal (name field, then a choice between "Paste API key" and
  "Log in with subscription") feeding into the existing prompt/terminal
  flows, now carrying `name`.
- `renderCreateForm` (`web/render.js`), Model section: when
  `backend === 'anthropic'` (and therefore `harness === 'claude-code'` —
  opencode cannot select this backend at all, unchanged), render a
  `<select>` of accounts by name. Pre-selected to:
  - the operator's current default, read from a new `default_anthropic_account_id`
    field in the `GET /api/agents` defaults payload, on the **create** form;
  - the agent's actual stored `anthropic_account_id` on the **update** form
    (falls back to showing nothing selected if that account was deleted,
    with an inline note, consistent with decision #3's fail-closed intent —
    the user has to pick a live one before saving).
  - Hidden entirely when `backend !== 'anthropic'`, same show/hide mechanism
    as the existing `.create-form__anthropic-note` toggle
    (`syncBackendAndHarness` in `web/app.js`).
- Submit handler: include `account_id` in the request body only when the
  user changed it away from the pre-selected value, following the existing
  "send only when it differs from default" convention used for the other
  optional fields.

## Migration

A one-time step inside `store.Open` (`internal/store/store.go`), after
buckets are created: if the old `anthropic_auth` key is present in
`bucketSettings` and `bucketAnthropicAccounts` is empty, then in one
transaction:

1. Create an `AnthropicAccount` named `"Default"` from the old
   `AnthropicAuth{Kind, Value}`.
2. Set it as the default account.
3. Walk `bucketAgents`; for every agent with `Backend == "anthropic"` and an
   empty `AnthropicAccountID`, set it to the new account's ID.
4. Delete the old `anthropic_auth` key.

This runs once; subsequent opens see a non-empty accounts bucket and skip
it. No separate CLI flag or manual step.

## Error handling summary

| Situation | Result |
|---|---|
| Create agent, `backend=anthropic`, explicit unknown `account_id` | 400 |
| Create agent, `backend=anthropic`, no `account_id`, no default set | 409 (today's "no credential configured" shape) |
| Create agent, `backend=anthropic`, no `account_id`, default resolves | 201, agent pinned to that concrete account ID |
| Delete an account that's referenced by agents | 204, always |
| Delete the default account | 204; default becomes unset |
| Agent's pinned account deleted, agent later restarted/woken | 409, fail closed, names the missing account |
| `CreateAnthropicAccount` with a duplicate name | 409 |
| Update agent's `account_id` while running | 200, stored only; container unaffected until next restart/wake |

## Testing

- `internal/store/anthropicaccount_test.go` (new): CRUD, uniqueness,
  default get/set/unset-on-delete — mirrors existing agent-store test
  shape.
- `internal/agent/backend_test.go` additions: `resolveBackend` with explicit
  valid/invalid `account_id`; omitted with/without a default; pin-at-creation
  (changing default afterward doesn't move an already-created agent).
  Mirrors existing `SetAnthropicAuth`/`IsNoAnthropicAuth` test patterns.
- `internal/agent/anthropiclogin_test.go` additions: name flows through
  start → finish → `CreateAnthropicAccount`; first-account auto-default.
- Migration test in `internal/store`: seed the old singleton + a legacy
  agent with `Backend=anthropic`/empty `AnthropicAccountID`, open the store,
  assert a "Default" account exists, is default, old key gone, legacy agent
  now pinned.
- Dangling-account test in `internal/agent`: pin agent to account, delete
  the account, attempt the next env-var build (restart/wake path) → 409.
- `web/render.test.js` / `web/app.js` wiring: new Settings table renders
  accounts, default marker, remove/set-default buttons wired; create/update
  form's account `<select>` populated and pre-selected correctly; hidden for
  `backend=ollama`.

Full gate before merge: `vet + fmt + lint + vuln + test` (Go, including the
real-daemon conformance leg) plus the `node --test web/*.test.js` suite, and
`make sync-web-embed` + drift check after any `web/` edit — same as every
prior feature in this series.
