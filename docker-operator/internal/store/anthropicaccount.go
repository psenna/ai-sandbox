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
