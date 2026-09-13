package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// ErrTemplateNotFound reports that no template record with the given ID
// exists. GetTemplate and UpdateTemplate wrap it; callers test with
// IsTemplateNotFound.
//
// DeleteTemplate deliberately does NOT return it, for the same reason
// Delete doesn't for an agent: removing a record that is already gone is
// success.
var ErrTemplateNotFound = errors.New("template not found")

// ErrTemplateNameExists reports that CreateTemplate or UpdateTemplate was
// given a name already held by a different template. Template names are a
// display/selection key (the create-form's dropdown), so unlike Agent.Name
// -- which has no uniqueness constraint -- a template's Name must be unique.
var ErrTemplateNameExists = errors.New("template name already in use")

// ErrTemplateExists reports that CreateTemplate was given an ID that is
// already in the store -- the template analog of ErrExists, kept as its own
// sentinel rather than reused so a caller catching one never mistakes it for
// the other record type.
var ErrTemplateExists = errors.New("template already exists")

// IsTemplateNotFound reports whether err was caused by a missing template
// record.
func IsTemplateNotFound(err error) bool { return errors.Is(err, ErrTemplateNotFound) }

// IsTemplateNameExists reports whether err was caused by a duplicate
// template name.
func IsTemplateNameExists(err error) bool { return errors.Is(err, ErrTemplateNameExists) }

// IsTemplateExists reports whether err was caused by a duplicate template ID.
func IsTemplateExists(err error) bool { return errors.Is(err, ErrTemplateExists) }

const (
	// templateIDPrefix marks a value as a template ID wherever it turns up
	// out of context, mirroring idPrefix for agents.
	templateIDPrefix = "tpl_"
	// templateIDBytes matches idBytes: four bytes of randomness is plenty
	// against the handful of templates a single operator will ever hold.
	templateIDBytes = 4
)

// bucketTemplates holds every saved agent-creation template, keyed by
// template ID. A template is a named, reusable bundle of the same fields
// createAgentRequest accepts, so a user can prefill the create-agent form
// from one instead of retyping it every time.
var bucketTemplates = []byte("templates")

// Template is a saved, reusable bundle of agent-creation parameters.
//
// Its Backend/Model/.../AutoMode fields deliberately share their JSON tags
// with Agent's own settable fields (see CreateSpec) so that a Template
// round-trips through the web UI's renderCreateForm(defaults, {values:...})
// with no field-name translation. Name and Description are the template's
// OWN identity (shown in the template-picker dropdown) -- they are NOT the
// agent's Name/Description and are never used to prefill them.
type Template struct {
	// ID is the primary key. Immutable: UpdateTemplate rejects a mutator
	// that changes it.
	ID string `json:"id"`
	// Name is the label shown in the template dropdown. Required (the API
	// layer rejects an empty one) and, unlike Agent.Name, must be unique --
	// two ambiguous same-named entries in a selection dropdown would be a
	// real usability problem, not just a cosmetic one.
	Name string `json:"name"`
	// Description is free-form text about the template itself. May be empty.
	Description string `json:"description,omitempty"`

	// Backend, Model, FastModel, OllamaURL, Repo, AutoCompactThreshold,
	// MaxContextTokens, ImageTag and AutoMode are recorded verbatim, exactly
	// as submitted -- the store performs no validation. internal/api
	// validates them (reusing validateAgentFields) before a create/update,
	// the same way it does for an agent. ImageTag's docker-tag-regex check
	// in particular happens only when the tag is actually used to create or
	// update an agent, not at template-save time.
	Backend              string `json:"backend,omitempty"`
	Model                string `json:"model,omitempty"`
	FastModel            string `json:"fast_model,omitempty"`
	OllamaURL            string `json:"ollama_url,omitempty"`
	Repo                 string `json:"repo,omitempty"`
	AutoCompactThreshold string `json:"auto_compact_threshold,omitempty"`
	MaxContextTokens     string `json:"max_context_tokens,omitempty"`
	ImageTag             string `json:"image_tag,omitempty"`
	AutoMode             string `json:"auto_mode,omitempty"`

	// CreatedAt is when CreateTemplate inserted the record, in UTC.
	// Immutable.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the record last changed, in UTC. CreateTemplate sets
	// it equal to CreatedAt, and every UpdateTemplate stamps it.
	UpdatedAt time.Time `json:"updated_at"`
}

// TemplateCreateSpec is everything a caller supplies to CreateTemplate (and,
// as a full-record replacement, to UpdateTemplate) -- a struct rather than a
// long parameter list for the same reason CreateSpec is: no call site can
// transpose two fields silently.
type TemplateCreateSpec struct {
	// ID is required and must be unique. Generate it with NewTemplateID.
	ID          string
	Name        string
	Description string

	Backend              string
	Model                string
	FastModel            string
	OllamaURL            string
	Repo                 string
	AutoCompactThreshold string
	MaxContextTokens     string
	ImageTag             string
	AutoMode             string
}

// NewTemplateID returns a fresh template ID of the form "tpl_7f3a9c2d",
// mirroring NewID for agents.
func NewTemplateID() (string, error) {
	var b [templateIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a template id: %w", err)
	}
	return templateIDPrefix + hex.EncodeToString(b[:]), nil
}

// CreateTemplate inserts a new template record. It rejects a blank ID, a
// duplicate ID (ErrTemplateExists), and a name already held by another
// template (ErrTemplateNameExists).
func (s *Store) CreateTemplate(ctx context.Context, spec TemplateCreateSpec) (Template, error) {
	if spec.ID == "" {
		return Template{}, errors.New("creating a template: the ID must not be empty, generate one with NewTemplateID")
	}

	now := s.now()
	tmpl := Template{
		ID:                   spec.ID,
		Name:                 spec.Name,
		Description:          spec.Description,
		Backend:              spec.Backend,
		Model:                spec.Model,
		FastModel:            spec.FastModel,
		OllamaURL:            spec.OllamaURL,
		Repo:                 spec.Repo,
		AutoCompactThreshold: spec.AutoCompactThreshold,
		MaxContextTokens:     spec.MaxContextTokens,
		ImageTag:             spec.ImageTag,
		AutoMode:             spec.AutoMode,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	err := s.updateTemplates(ctx, func(b *bbolt.Bucket) error {
		if b.Get([]byte(spec.ID)) != nil {
			return fmt.Errorf("creating template %q: %w", spec.ID, ErrTemplateExists)
		}
		if err := checkTemplateNameFree(b, spec.ID, spec.Name); err != nil {
			return fmt.Errorf("creating template %q: %w", spec.ID, err)
		}
		return putTemplate(b, tmpl)
	})
	if err != nil {
		return Template{}, err
	}
	return tmpl, nil
}

// GetTemplate returns the template with the given ID, or an error
// satisfying IsTemplateNotFound.
func (s *Store) GetTemplate(ctx context.Context, id string) (Template, error) {
	var tmpl Template
	err := s.viewTemplates(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting template %q: %w", id, ErrTemplateNotFound)
		}
		return decodeTemplate(id, raw, &tmpl)
	})
	if err != nil {
		return Template{}, err
	}
	return tmpl, nil
}

// ListTemplates returns every template, sorted by ID because bbolt iterates
// keys in byte order (the web UI sorts by Name for display itself). The
// slice is never nil, so an empty store encodes as a JSON [] and not null.
func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	templates := make([]Template, 0)
	err := s.viewTemplates(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var tmpl Template
			if err := decodeTemplate(string(k), v, &tmpl); err != nil {
				return err
			}
			templates = append(templates, tmpl)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing templates: %w", err)
	}
	return templates, nil
}

// UpdateTemplate applies mutate to the template with the given ID and writes
// the whole record back, all inside one read-write transaction, mirroring
// Update's mutator pattern for the same read-modify-write safety. ID and
// CreatedAt are immutable; a mutator that renames the template to a name
// another template already holds gets ErrTemplateNameExists and changes
// nothing. UpdatedAt is stamped by this method after the mutator runs.
func (s *Store) UpdateTemplate(ctx context.Context, id string, mutate func(*Template) error) (Template, error) {
	if mutate == nil {
		return Template{}, fmt.Errorf("updating template %q: the mutator must not be nil", id)
	}

	var updated Template
	err := s.updateTemplates(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("updating template %q: %w", id, ErrTemplateNotFound)
		}
		var tmpl Template
		if err := decodeTemplate(id, raw, &tmpl); err != nil {
			return err
		}

		before := tmpl
		if err := mutate(&tmpl); err != nil {
			return fmt.Errorf("updating template %q: %w", id, err)
		}
		if err := checkTemplateInvariants(before, tmpl); err != nil {
			return fmt.Errorf("updating template %q: %w", id, err)
		}
		if err := checkTemplateNameFree(b, id, tmpl.Name); err != nil {
			return fmt.Errorf("updating template %q: %w", id, err)
		}

		tmpl.UpdatedAt = s.now()
		if err := putTemplate(b, tmpl); err != nil {
			return err
		}
		updated = tmpl
		return nil
	})
	if err != nil {
		return Template{}, err
	}
	return updated, nil
}

// DeleteTemplate removes the template record. Deleting one that is not
// there is success, matching Delete's idempotent stance for an agent.
func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	return s.updateTemplates(ctx, func(b *bbolt.Bucket) error {
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("deleting template %q: %w", id, err)
		}
		return nil
	})
}

// viewTemplates runs fn against the templates bucket inside a read-only
// transaction, mirroring view's shape for the agents bucket.
func (s *Store) viewTemplates(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		b, err := s.templatesBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

// updateTemplates runs fn against the templates bucket inside a read-write
// transaction, mirroring update's shape for the agents bucket. Sharing one
// bbolt.DB with the agents bucket is enough to serialise writers process-wide
// -- a template write and an agent write can never interleave either.
func (s *Store) updateTemplates(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := s.templatesBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

// templatesBucket returns the templates bucket. Open creates it, so a nil
// here means the file was truncated or was never this program's database.
func (s *Store) templatesBucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	b := tx.Bucket(bucketTemplates)
	if b == nil {
		return nil, fmt.Errorf("the %q bucket is missing from the state database %q", bucketTemplates, s.path)
	}
	return b, nil
}

// checkTemplateNameFree scans the bucket for a template other than
// excludeID already using name (case-sensitively, after trimming), and
// returns ErrTemplateNameExists if one exists. Called inside the same
// read-write transaction as the insert/rename it guards, so there is no
// window for a second writer to race it -- bbolt permits only one such
// transaction at a time.
//
// A full-bucket scan is the same trade-off countReserved already makes for
// agents: the record count here is a handful, so the scan costs
// microseconds, and a secondary name index would be one more thing that can
// silently disagree with the records themselves.
func checkTemplateNameFree(b *bbolt.Bucket, excludeID, name string) error {
	name = strings.TrimSpace(name)
	return b.ForEach(func(k, v []byte) error {
		if string(k) == excludeID {
			return nil
		}
		var existing Template
		if err := decodeTemplate(string(k), v, &existing); err != nil {
			return err
		}
		if strings.TrimSpace(existing.Name) == name {
			return ErrTemplateNameExists
		}
		return nil
	})
}

// checkTemplateInvariants rejects a mutation that broke an immutable field.
// Templates have no Status to validate, unlike an agent.
func checkTemplateInvariants(before, after Template) error {
	if after.ID != before.ID {
		return fmt.Errorf("the mutator changed the ID to %q; a template's ID is immutable", after.ID)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		return fmt.Errorf("the mutator changed CreatedAt to %s; it is immutable",
			after.CreatedAt.Format(time.RFC3339Nano))
	}
	return nil
}

// putTemplate JSON-encodes tmpl and stores it under its own ID.
func putTemplate(b *bbolt.Bucket, tmpl Template) error {
	raw, err := json.Marshal(tmpl)
	if err != nil {
		return fmt.Errorf("encoding template %q: %w", tmpl.ID, err)
	}
	if err := b.Put([]byte(tmpl.ID), raw); err != nil {
		return fmt.Errorf("writing template %q: %w", tmpl.ID, err)
	}
	return nil
}

// decodeTemplate JSON-decodes one record, mirroring decode's contract for an
// agent: the raw bytes belong to the transaction, but json.Unmarshal copies
// everything it keeps into tmpl, so the decoded value safely outlives it.
func decodeTemplate(id string, raw []byte, tmpl *Template) error {
	if err := json.Unmarshal(raw, tmpl); err != nil {
		return fmt.Errorf("decoding template %q from the state database: %w", id, err)
	}
	return nil
}
