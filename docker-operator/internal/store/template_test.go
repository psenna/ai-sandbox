package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// mustCreateTemplate creates a template named name (id derived from name)
// with a couple of distinguishing infra fields set, mirroring mustCreate's
// role for agents.
func mustCreateTemplate(t *testing.T, s *Store, id, name string) Template {
	t.Helper()
	tmpl, err := s.CreateTemplate(context.Background(), TemplateCreateSpec{
		ID:      id,
		Name:    name,
		Backend: "ollama",
		Repo:    "owner/repo.git",
	})
	if err != nil {
		t.Fatalf("CreateTemplate(%q, %q): %v", id, name, err)
	}
	return tmpl
}

func templateIDs(templates []Template) []string {
	out := make([]string, 0, len(templates))
	for _, tmpl := range templates {
		out = append(out, tmpl.ID)
	}
	return out
}

func TestOpen_CreatesTheTemplatesBucket(t *testing.T) {
	// CreateTemplate itself fails with "bucket is missing" if Open didn't
	// create it, so a successful create is sufficient proof.
	s := newStore(t, 1)
	if _, err := s.CreateTemplate(context.Background(), TemplateCreateSpec{ID: "tpl_00000001", Name: "t"}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
}

func TestNewTemplateID_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewTemplateID()
		if err != nil {
			t.Fatalf("NewTemplateID: %v", err)
		}
		if len(id) != len(templateIDPrefix)+8 || id[:len(templateIDPrefix)] != templateIDPrefix {
			t.Fatalf("NewTemplateID = %q, want prefix %q + 8 hex chars", id, templateIDPrefix)
		}
		if seen[id] {
			t.Fatalf("NewTemplateID returned %q twice in %d calls", id, i+1)
		}
		seen[id] = true
	}
}

func TestCreateTemplate_InsertsARecord(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)

	got, err := s.CreateTemplate(ctx, TemplateCreateSpec{
		ID:                   "tpl_7f3a9c2d",
		Name:                 "my template",
		Description:          "a description",
		Backend:              "ollama",
		Model:                "opus-model",
		FastModel:            "haiku-model",
		OllamaURL:            "http://ollama:11434",
		Repo:                 "owner/repo.git",
		AutoCompactThreshold: "80",
		MaxContextTokens:     "100000",
		ImageTag:             "latest",
		AutoMode:             "on",
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	want := Template{
		ID:                   "tpl_7f3a9c2d",
		Name:                 "my template",
		Description:          "a description",
		Backend:              "ollama",
		Model:                "opus-model",
		FastModel:            "haiku-model",
		OllamaURL:            "http://ollama:11434",
		Repo:                 "owner/repo.git",
		AutoCompactThreshold: "80",
		MaxContextTokens:     "100000",
		ImageTag:             "latest",
		AutoMode:             "on",
		CreatedAt:            baseTime,
		UpdatedAt:            baseTime,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateTemplate returned %+v, want %+v", got, want)
	}

	stored, err := s.GetTemplate(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("GetTemplate returned %+v, want %+v (every field must round-trip through JSON)", stored, want)
	}
}

func TestCreateTemplate_RejectsAnEmptyID(t *testing.T) {
	_, err := newStore(t, 1).CreateTemplate(context.Background(), TemplateCreateSpec{Name: "t"})
	if err == nil {
		t.Fatal("CreateTemplate with an empty ID returned nil, want an error")
	}
}

func TestCreateTemplate_RejectsADuplicateID(t *testing.T) {
	s := newStore(t, 1)
	mustCreateTemplate(t, s, "tpl_00000001", "first")

	_, err := s.CreateTemplate(context.Background(), TemplateCreateSpec{ID: "tpl_00000001", Name: "second"})
	if !IsTemplateExists(err) {
		t.Fatalf("CreateTemplate with a duplicate ID: err = %v, want one satisfying IsTemplateExists", err)
	}
}

func TestCreateTemplate_RejectsADuplicateName(t *testing.T) {
	s := newStore(t, 1)
	mustCreateTemplate(t, s, "tpl_00000001", "shared-name")

	_, err := s.CreateTemplate(context.Background(), TemplateCreateSpec{ID: "tpl_00000002", Name: "shared-name"})
	if !IsTemplateNameExists(err) {
		t.Fatalf("CreateTemplate with a duplicate name: err = %v, want one satisfying IsTemplateNameExists", err)
	}

	// The rejected create must not have left a partial record behind.
	if _, err := s.GetTemplate(context.Background(), "tpl_00000002"); !IsTemplateNotFound(err) {
		t.Fatalf("GetTemplate after a rejected create: err = %v, want one satisfying IsTemplateNotFound", err)
	}
}

func TestCreateTemplate_NameUniquenessIgnoresSurroundingWhitespace(t *testing.T) {
	s := newStore(t, 1)
	mustCreateTemplate(t, s, "tpl_00000001", "shared-name")

	_, err := s.CreateTemplate(context.Background(), TemplateCreateSpec{ID: "tpl_00000002", Name: "  shared-name  "})
	if !IsTemplateNameExists(err) {
		t.Fatalf("CreateTemplate with a whitespace-padded duplicate name: err = %v, want one satisfying IsTemplateNameExists", err)
	}
}

func TestGetTemplate_NotFound(t *testing.T) {
	_, err := newStore(t, 1).GetTemplate(context.Background(), "tpl_missing")
	if !IsTemplateNotFound(err) {
		t.Fatalf("GetTemplate on a missing id: err = %v, want one satisfying IsTemplateNotFound", err)
	}
}

func TestListTemplates_EmptyStoreReturnsANonNilSlice(t *testing.T) {
	got, err := newStore(t, 1).ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if got == nil {
		t.Fatal("ListTemplates returned nil; it must return an empty slice so the API encodes [] and not null")
	}
	if len(got) != 0 {
		t.Fatalf("ListTemplates returned %v, want empty", templateIDs(got))
	}
}

func TestListTemplates_ReturnsEverySortedByID(t *testing.T) {
	s := newStore(t, 1)
	for i, id := range []string{"tpl_cccccccc", "tpl_aaaaaaaa", "tpl_bbbbbbbb"} {
		mustCreateTemplate(t, s, id, "name"+string(rune('a'+i)))
	}

	got, err := s.ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	want := []string{"tpl_aaaaaaaa", "tpl_bbbbbbbb", "tpl_cccccccc"}
	if !reflect.DeepEqual(templateIDs(got), want) {
		t.Fatalf("ListTemplates returned %v, want %v", templateIDs(got), want)
	}
}

func TestUpdateTemplate_StampsUpdatedAtAndOverwritesFields(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)
	created := mustCreateTemplate(t, s, "tpl_7f3a9c2d", "original")

	got, err := s.UpdateTemplate(ctx, created.ID, func(tmpl *Template) error {
		tmpl.Name = "renamed"
		tmpl.Description = "new description"
		tmpl.Backend = "anthropic"
		tmpl.Repo = "owner/other.git"
		// Deliberately forged: UpdateTemplate must overwrite it with its own clock.
		tmpl.UpdatedAt = time.Unix(0, 0).UTC()
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}

	want := Template{
		ID:          created.ID,
		Name:        "renamed",
		Description: "new description",
		Backend:     "anthropic",
		Repo:        "owner/other.git",
		CreatedAt:   baseTime,
		UpdatedAt:   baseTime.Add(time.Minute),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UpdateTemplate returned %+v, want %+v", got, want)
	}

	stored, err := s.GetTemplate(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("GetTemplate returned %+v, want %+v", stored, want)
	}
}

func TestUpdateTemplate_NotFound(t *testing.T) {
	_, err := newStore(t, 1).UpdateTemplate(context.Background(), "tpl_missing", func(*Template) error { return nil })
	if !IsTemplateNotFound(err) {
		t.Fatalf("UpdateTemplate on a missing id: err = %v, want one satisfying IsTemplateNotFound", err)
	}
}

func TestUpdateTemplate_RejectsANilMutator(t *testing.T) {
	s := newStore(t, 1)
	created := mustCreateTemplate(t, s, "tpl_00000001", "t")
	if _, err := s.UpdateTemplate(context.Background(), created.ID, nil); err == nil {
		t.Fatal("UpdateTemplate with a nil mutator returned nil, want an error")
	}
}

// TestUpdateTemplate_MutatorFailuresRollBack covers both a mutator that
// returns an error and every invariant the store enforces. In all cases the
// stored record must be exactly what it was.
func TestUpdateTemplate_MutatorFailuresRollBack(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("mutator said no")

	cases := []struct {
		name    string
		mutate  func(*Template) error
		wantErr func(error) bool
	}{
		{
			name:    "mutator error",
			mutate:  func(*Template) error { return sentinel },
			wantErr: func(err error) bool { return errors.Is(err, sentinel) },
		},
		{
			name:    "changed ID",
			mutate:  func(tmpl *Template) error { tmpl.ID = "tpl_different"; return nil },
			wantErr: func(err error) bool { return err != nil },
		},
		{
			name:    "changed CreatedAt",
			mutate:  func(tmpl *Template) error { tmpl.CreatedAt = tmpl.CreatedAt.Add(time.Hour); return nil },
			wantErr: func(err error) bool { return err != nil },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t, 1)
			created := mustCreateTemplate(t, s, "tpl_00000001", "t")

			_, err := s.UpdateTemplate(ctx, created.ID, tc.mutate)
			if !tc.wantErr(err) {
				t.Fatalf("UpdateTemplate: err = %v, want a matching error", err)
			}

			stored, getErr := s.GetTemplate(ctx, created.ID)
			if getErr != nil {
				t.Fatalf("GetTemplate: %v", getErr)
			}
			if !reflect.DeepEqual(stored, created) {
				t.Fatalf("record changed despite the rejected update: got %+v, want %+v", stored, created)
			}
		})
	}
}

func TestUpdateTemplate_RejectsRenamingToAnotherTemplatesName(t *testing.T) {
	s := newStore(t, 1)
	mustCreateTemplate(t, s, "tpl_00000001", "taken")
	other := mustCreateTemplate(t, s, "tpl_00000002", "other")

	_, err := s.UpdateTemplate(context.Background(), other.ID, func(tmpl *Template) error {
		tmpl.Name = "taken"
		return nil
	})
	if !IsTemplateNameExists(err) {
		t.Fatalf("UpdateTemplate renaming to an in-use name: err = %v, want one satisfying IsTemplateNameExists", err)
	}

	stored, getErr := s.GetTemplate(context.Background(), other.ID)
	if getErr != nil {
		t.Fatalf("GetTemplate: %v", getErr)
	}
	if stored.Name != "other" {
		t.Fatalf("record was renamed despite the rejected update: Name = %q, want %q", stored.Name, "other")
	}
}

func TestUpdateTemplate_KeepingItsOwnNameIsNotADuplicate(t *testing.T) {
	s := newStore(t, 1)
	created := mustCreateTemplate(t, s, "tpl_00000001", "same-name")

	got, err := s.UpdateTemplate(context.Background(), created.ID, func(tmpl *Template) error {
		tmpl.Description = "edited, name unchanged"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if got.Name != "same-name" {
		t.Fatalf("Name = %q, want %q", got.Name, "same-name")
	}
}

func TestDeleteTemplate_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	created := mustCreateTemplate(t, s, "tpl_00000001", "t")

	if err := s.DeleteTemplate(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := s.GetTemplate(ctx, created.ID); !IsTemplateNotFound(err) {
		t.Fatalf("GetTemplate after delete: err = %v, want one satisfying IsTemplateNotFound", err)
	}
	// Deleting again must still succeed.
	if err := s.DeleteTemplate(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTemplate (second call): %v", err)
	}
	if err := s.DeleteTemplate(ctx, "tpl_never_existed"); err != nil {
		t.Fatalf("DeleteTemplate on an unknown id: %v", err)
	}
}

func TestDeleteTemplate_FreesItsNameForReuse(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	created := mustCreateTemplate(t, s, "tpl_00000001", "reusable-name")

	if err := s.DeleteTemplate(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	if _, err := s.CreateTemplate(ctx, TemplateCreateSpec{ID: "tpl_00000002", Name: "reusable-name"}); err != nil {
		t.Fatalf("CreateTemplate reusing a deleted template's name: %v", err)
	}
}

// TestTemplatesBucketMissingSurfacesAsAnError mirrors
// TestMissingBucketSurfacesAsAnError for the templates bucket: a state file
// that is not this program's database must produce a legible error, not a
// nil-pointer panic.
func TestTemplatesBucketMissingSurfacesAsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.DeleteBucket(bucketTemplates)
	}); err != nil {
		t.Fatalf("deleting the bucket: %v", err)
	}

	if _, err := s.CreateTemplate(ctx, TemplateCreateSpec{ID: "tpl_00000001", Name: "t"}); err == nil {
		t.Error("CreateTemplate without the bucket returned nil, want an error")
	}
	if _, err := s.GetTemplate(ctx, "tpl_00000001"); err == nil {
		t.Error("GetTemplate without the bucket returned nil, want an error")
	}
	if _, err := s.ListTemplates(ctx); err == nil {
		t.Error("ListTemplates without the bucket returned nil, want an error")
	}
	if _, err := s.UpdateTemplate(ctx, "tpl_00000001", func(*Template) error { return nil }); err == nil {
		t.Error("UpdateTemplate without the bucket returned nil, want an error")
	}
	if err := s.DeleteTemplate(ctx, "tpl_00000001"); err == nil {
		t.Error("DeleteTemplate without the bucket returned nil, want an error")
	}
}

// TestCorruptTemplateRecordSurfacesAsAnError mirrors
// TestCorruptRecordSurfacesAsAnError: a garbled value must fail loudly on
// every path that decodes one, including the name-uniqueness scan Create and
// Update run over the whole bucket.
func TestCorruptTemplateRecordSurfacesAsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketTemplates).Put([]byte("tpl_corrupt"), []byte("{not json"))
	}); err != nil {
		t.Fatalf("seeding a corrupt record: %v", err)
	}

	if _, err := s.GetTemplate(ctx, "tpl_corrupt"); err == nil {
		t.Error("GetTemplate on a corrupt record returned nil, want an error")
	}
	if _, err := s.ListTemplates(ctx); err == nil {
		t.Error("ListTemplates with a corrupt record returned nil, want an error")
	}
	if _, err := s.UpdateTemplate(ctx, "tpl_corrupt", func(*Template) error { return nil }); err == nil {
		t.Error("UpdateTemplate on a corrupt record returned nil, want an error")
	}
	if _, err := s.CreateTemplate(ctx, TemplateCreateSpec{ID: "tpl_00000001", Name: "t"}); err == nil {
		t.Error("CreateTemplate with a corrupt record present (uniqueness scan) returned nil, want an error")
	}
}
