package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestCreateTemplate_GeneratesAnID(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()

	got, err := m.CreateTemplate(ctx, store.TemplateCreateSpec{Name: "my template", Backend: "ollama"})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if got.ID == "" {
		t.Fatal("CreateTemplate returned a record with an empty ID")
	}
	if got.Name != "my template" || got.Backend != "ollama" {
		t.Fatalf("CreateTemplate returned %+v, want Name=%q Backend=%q", got, "my template", "ollama")
	}
}

func TestCreateTemplate_IgnoresACallerSuppliedID(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	got, err := m.CreateTemplate(ctx, store.TemplateCreateSpec{ID: "tpl_should_be_ignored", Name: "t"})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if got.ID == "tpl_should_be_ignored" {
		t.Fatalf("CreateTemplate used the caller-supplied ID %q instead of generating one", got.ID)
	}
	if _, err := st.GetTemplate(ctx, got.ID); err != nil {
		t.Fatalf("the generated ID was not actually persisted: GetTemplate: %v", err)
	}
}

func TestListTemplates_ReturnsEveryCreatedTemplate(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	if _, err := m.CreateTemplate(ctx, store.TemplateCreateSpec{Name: "one"}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if _, err := m.CreateTemplate(ctx, store.TemplateCreateSpec{Name: "two"}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	got, err := m.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTemplates returned %d templates, want 2", len(got))
	}
}

func TestGetTemplate_NotFound(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	if _, err := m.GetTemplate(context.Background(), "tpl_missing"); !store.IsTemplateNotFound(err) {
		t.Fatalf("GetTemplate: err = %v, want one satisfying store.IsTemplateNotFound", err)
	}
}

func TestUpdateTemplate_OverwritesEveryField(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	created, err := m.CreateTemplate(ctx, store.TemplateCreateSpec{
		Name: "original", Backend: "ollama", Repo: "owner/repo.git",
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	got, err := m.UpdateTemplate(ctx, created.ID, store.TemplateCreateSpec{
		ID:          "tpl_ignored_on_update_too",
		Name:        "renamed",
		Description: "new description",
		Backend:     "anthropic",
		Repo:        "owner/other.git",
		AutoMode:    "on",
	})
	if err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("UpdateTemplate changed the ID to %q, want it to stay %q", got.ID, created.ID)
	}
	if got.Name != "renamed" || got.Description != "new description" || got.Backend != "anthropic" ||
		got.Repo != "owner/other.git" || got.AutoMode != "on" {
		t.Fatalf("UpdateTemplate returned %+v, unexpected field values", got)
	}
}

func TestUpdateTemplate_NotFound(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	_, err := m.UpdateTemplate(context.Background(), "tpl_missing", store.TemplateCreateSpec{Name: "t"})
	if !store.IsTemplateNotFound(err) {
		t.Fatalf("UpdateTemplate: err = %v, want one satisfying store.IsTemplateNotFound", err)
	}
}

func TestDeleteTemplate_IsIdempotent(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	created, err := m.CreateTemplate(ctx, store.TemplateCreateSpec{Name: "t"})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	if err := m.DeleteTemplate(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if err := m.DeleteTemplate(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTemplate (second call): %v", err)
	}
	if _, err := m.GetTemplate(ctx, created.ID); !store.IsTemplateNotFound(err) {
		t.Fatalf("GetTemplate after delete: err = %v, want one satisfying store.IsTemplateNotFound", err)
	}
}
