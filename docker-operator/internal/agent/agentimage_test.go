package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/registry"
	"github.com/psenna/ai-sandbox/docker-operator/internal/registry/registrytest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestIsDateTimeTag(t *testing.T) {
	yes := []string{"20260101-000000", "19991231-235959"}
	no := []string{"", "latest", "2026010-000000", "20260101-00000", "20260101_000000", "20260101-000000-rc1"}
	for _, s := range yes {
		if !IsDateTimeTag(s) {
			t.Errorf("IsDateTimeTag(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsDateTimeTag(s) {
			t.Errorf("IsDateTimeTag(%q) = true, want false", s)
		}
	}
}

func TestFilterDateTimeTags(t *testing.T) {
	in := []string{"latest", "20260101-120000", "main", "20260101-120000", "20251231-090000", ""}
	got := FilterDateTimeTags(in)
	want := []string{"20260101-120000", "20251231-090000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilterDateTimeTags(%v) = %v, want %v (matches only, de-duplicated)", in, got, want)
	}
}

func TestSortTagsNewestFirst(t *testing.T) {
	in := []string{"20251231-090000", "20260101-120000", "20260101-115959"}
	got := SortTagsNewestFirst(in)
	want := []string{"20260101-120000", "20260101-115959", "20251231-090000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SortTagsNewestFirst = %v, want %v", got, want)
	}
	// The input is not mutated.
	if in[0] != "20251231-090000" {
		t.Errorf("SortTagsNewestFirst mutated its input: %v", in)
	}
}

func TestNewestDateTimeTag(t *testing.T) {
	if got := NewestDateTimeTag(nil); got != "" {
		t.Errorf("NewestDateTimeTag(nil) = %q, want \"\"", got)
	}
	if got := NewestDateTimeTag([]string{"latest", "main"}); got != "" {
		t.Errorf("NewestDateTimeTag(no date-time tags) = %q, want \"\"", got)
	}
	got := NewestDateTimeTag([]string{"20251231-090000", "latest", "20260101-120000"})
	if got != "20260101-120000" {
		t.Errorf("NewestDateTimeTag = %q, want the newest date-time tag", got)
	}
}

func TestUpgradeAvailable(t *testing.T) {
	cases := []struct {
		name    string
		current string
		tags    []string
		want    bool
	}{
		{"older than newest in list", "20260101-120000", []string{"20260101-120000", "20260201-090000"}, true},
		{"equal to newest", "20260201-090000", []string{"20260101-120000", "20260201-090000"}, false},
		{"newer than everything", "20260301-000000", []string{"20260101-120000", "20260201-090000"}, false},
		{"empty current tag", "", []string{"20260201-090000"}, false},
		{"latest current tag", "latest", []string{"20260201-090000"}, false},
		{"list has no date-time tags", "20260101-120000", []string{"latest", "main"}, false},
		{"nil list", "20260101-120000", nil, false},
		{"empty list", "20260101-120000", []string{}, false},
		{"list is current plus older", "20260201-090000", []string{"20260201-090000", "20260101-120000", "20251231-235959"}, false},
		{"unsorted list with a newer entry", "20260101-120000", []string{"20251231-000000", "latest", "20260601-000000", "20260101-120000"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpgradeAvailable(tc.current, tc.tags); got != tc.want {
				t.Errorf("UpgradeAvailable(%q, %v) = %v, want %v", tc.current, tc.tags, got, tc.want)
			}
		})
	}
}

func TestImageTagOf(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/psenna/ai-sandbox-agent:latest":          "latest",
		"ghcr.io/psenna/ai-sandbox-agent:20260101-120000": "20260101-120000",
		"ghcr.io/psenna/ai-sandbox-agent":                 "",
		"host:5000/team/img":                              "",
		"host:5000/team/img:v2":                           "v2",
		"ghcr.io/x/y@sha256:" + hex64:                     "",
		"not a ref!!":                                     "",
	}
	for ref, want := range cases {
		if got := ImageTagOf(ref); got != want {
			t.Errorf("ImageTagOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestRepoWithoutTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/psenna/ai-sandbox-agent:latest":          "ghcr.io/psenna/ai-sandbox-agent",
		"ghcr.io/psenna/ai-sandbox-agent@sha256:" + hex64: "ghcr.io/psenna/ai-sandbox-agent",
		"host:5000/team/img:v2":                           "host:5000/team/img",
	}
	for ref, want := range cases {
		if got := RepoWithoutTag(ref); got != want {
			t.Errorf("RepoWithoutTag(%q) = %q, want %q", ref, got, want)
		}
	}
}

const hex64 = "0000000000000000000000000000000000000000000000000000000000000000"

func TestResolveAgentImageRef(t *testing.T) {
	m, _, _ := newTestManager(t, 1)

	if got, err := m.resolveAgentImageRef(""); err != nil || got != m.cfg.AgentImage {
		t.Errorf("resolveAgentImageRef(\"\") = (%q, %v), want (%q, nil)", got, err, m.cfg.AgentImage)
	}

	wantValid := RepoWithoutTag(m.cfg.AgentImage) + ":20260101-120000"
	if got, err := m.resolveAgentImageRef("20260101-120000"); err != nil || got != wantValid {
		t.Errorf("resolveAgentImageRef(valid) = (%q, %v), want (%q, nil)", got, err, wantValid)
	}

	if _, err := m.resolveAgentImageRef("bad tag!!"); !IsInvalidImageTag(err) {
		t.Errorf("resolveAgentImageRef(invalid) err = %v, want IsInvalidImageTag", err)
	}
}

func TestRefreshAgentImageTags_StoresFilteredSortedDesc(t *testing.T) {
	m, _, st := newTestManager(t, 1)
	m.registry = &registrytest.Fake{Tags: []string{
		"latest", "20251231-090000", "main", "20260101-120000", "20251231-090000",
	}}

	if err := m.RefreshAgentImageTags(context.Background()); err != nil {
		t.Fatalf("RefreshAgentImageTags: %v", err)
	}

	got, ok, err := st.GetAgentImageTags(context.Background())
	if err != nil || !ok {
		t.Fatalf("GetAgentImageTags = (_, %v, %v)", ok, err)
	}
	want := []string{"20260101-120000", "20251231-090000"}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("stored Tags = %v, want %v (filtered, de-duplicated, newest-first)", got.Tags, want)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty after a successful refresh", got.LastError)
	}
	if got.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero, want it stamped")
	}
}

func TestRefreshAgentImageTags_KeepsLastListOnError(t *testing.T) {
	m, _, st := newTestManager(t, 1)
	ctx := context.Background()

	seed := store.AgentImageTags{Tags: []string{"20260101-120000", "20251231-090000"}}
	if err := st.SetAgentImageTags(ctx, seed); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	m.registry = &registrytest.Fake{Err: registry.ErrOffline}
	err := m.RefreshAgentImageTags(ctx)
	if !errors.Is(err, registry.ErrOffline) {
		t.Fatalf("RefreshAgentImageTags err = %v, want it to wrap registry.ErrOffline", err)
	}

	got, _, gerr := st.GetAgentImageTags(ctx)
	if gerr != nil {
		t.Fatalf("GetAgentImageTags: %v", gerr)
	}
	if !reflect.DeepEqual(got.Tags, seed.Tags) {
		t.Errorf("Tags = %v, want the last-known list %v unchanged", got.Tags, seed.Tags)
	}
	if got.LastError == "" {
		t.Error("LastError is empty, want the poll error recorded")
	}
	if got.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero, want it advanced even on a failed poll")
	}
}

func TestRefreshAgentImageTags_NilRegistry(t *testing.T) {
	m, _, st := newTestManager(t, 1)
	m.registry = nil
	ctx := context.Background()

	seed := store.AgentImageTags{Tags: []string{"20260101-120000"}}
	if err := st.SetAgentImageTags(ctx, seed); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := m.RefreshAgentImageTags(ctx); err != nil {
		t.Fatalf("RefreshAgentImageTags with a nil registry = %v, want nil", err)
	}
	got, _, err := st.GetAgentImageTags(ctx)
	if err != nil {
		t.Fatalf("GetAgentImageTags: %v", err)
	}
	if !reflect.DeepEqual(got.Tags, seed.Tags) {
		t.Errorf("Tags = %v, want the last-known list kept", got.Tags)
	}
	if got.LastError != "registry client not configured" {
		t.Errorf("LastError = %q, want %q", got.LastError, "registry client not configured")
	}
}

func TestCreate_StampsImage(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	cfg.AgentImage = "ghcr.io/psenna/ai-sandbox-agent:latest"
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage("ghcr.io/psenna/ai-sandbox-agent:20260101-120000")
	st := newTestStore(t, 5)
	m := NewManager(f, nil, st, cfg, testLogger(), testOptions())

	before := len(f.Calls())
	got, err := m.Create(context.Background(), CreateRequest{ImageTag: "20260101-120000"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const wantRef = "ghcr.io/psenna/ai-sandbox-agent:20260101-120000"
	if got.Image != wantRef {
		t.Errorf("record Image = %q, want %q", got.Image, wantRef)
	}
	if spec := m.agentSpec(got, resolvedBackend{kind: "ollama"}); spec.Image != wantRef {
		t.Errorf("agentSpec Image = %q, want %q", spec.Image, wantRef)
	}
	inspectedTag := false
	for _, c := range f.Calls()[before:] {
		if c.Op == dockerclienttest.OpImageInspect && c.Target == wantRef {
			inspectedTag = true
		}
	}
	if !inspectedTag {
		t.Errorf("no ImageInspect targeted %q; calls: %v", wantRef, f.Calls()[before:])
	}
}
