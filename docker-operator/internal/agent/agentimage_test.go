package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
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
	ctx := context.Background()

	// Empty tag: newTestManagerCfg pre-seeds the daemon with exactly
	// m.cfg.AgentImageClaudeCode (tag included), so the local-default path
	// resolves back to that same reference.
	if got, err := m.resolveAgentImageRef(ctx, "", config.HarnessClaudeCode); err != nil || got != m.cfg.AgentImageClaudeCode {
		t.Errorf("resolveAgentImageRef(\"\") = (%q, %v), want (%q, nil)", got, err, m.cfg.AgentImageClaudeCode)
	}

	wantValid := RepoWithoutTag(m.cfg.AgentImageClaudeCode) + ":20260101-120000"
	if got, err := m.resolveAgentImageRef(ctx, "20260101-120000", config.HarnessClaudeCode); err != nil || got != wantValid {
		t.Errorf("resolveAgentImageRef(valid) = (%q, %v), want (%q, nil)", got, err, wantValid)
	}

	if _, err := m.resolveAgentImageRef(ctx, "bad tag!!", config.HarnessClaudeCode); !IsInvalidImageTag(err) {
		t.Errorf("resolveAgentImageRef(invalid) err = %v, want IsInvalidImageTag", err)
	}
}

func TestRefreshAgentImageTags_StoresFilteredSortedDesc(t *testing.T) {
	m, _, st := newTestManager(t, 1)
	m.registries = map[string]registry.Client{config.HarnessClaudeCode: &registrytest.Fake{Tags: []string{
		"latest", "20251231-090000", "main", "20260101-120000", "20251231-090000",
	}}}

	if err := m.RefreshAgentImageTags(context.Background(), config.HarnessClaudeCode); err != nil {
		t.Fatalf("RefreshAgentImageTags: %v", err)
	}

	got, ok, err := st.GetAgentImageTags(context.Background(), "claude-code")
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
	if err := st.SetAgentImageTags(ctx, "claude-code", seed); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	m.registries = map[string]registry.Client{config.HarnessClaudeCode: &registrytest.Fake{Err: registry.ErrOffline}}
	err := m.RefreshAgentImageTags(ctx, config.HarnessClaudeCode)
	if !errors.Is(err, registry.ErrOffline) {
		t.Fatalf("RefreshAgentImageTags err = %v, want it to wrap registry.ErrOffline", err)
	}

	got, _, gerr := st.GetAgentImageTags(ctx, "claude-code")
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
	m.registries = nil
	ctx := context.Background()

	seed := store.AgentImageTags{Tags: []string{"20260101-120000"}}
	if err := st.SetAgentImageTags(ctx, "claude-code", seed); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := m.RefreshAgentImageTags(ctx, config.HarnessClaudeCode); err != nil {
		t.Fatalf("RefreshAgentImageTags with a nil registry = %v, want nil", err)
	}
	got, _, err := st.GetAgentImageTags(ctx, "claude-code")
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
	cfg.AgentImageClaudeCode = "ghcr.io/psenna/ai-sandbox-agent:latest"
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
	spec, err := m.agentSpec(got, resolvedBackend{kind: "ollama"})
	if err != nil {
		t.Fatalf("agentSpec: %v", err)
	}
	if spec.Image != wantRef {
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

func TestDefaultImageTagFrom(t *testing.T) {
	cases := []struct {
		name     string
		local    []string
		registry []string
		want     string
	}{
		{"local date-time tag wins over a newer registry tag", []string{"20260101-120000"}, []string{"20260301-000000"}, "20260101-120000"},
		{"local newest among several date-time tags", []string{"20260101-120000", "20260201-090000"}, nil, "20260201-090000"},
		{"local non-date-time tag wins when no local date-time tag", []string{"dev", "staging"}, []string{"20260301-000000"}, "staging"},
		{"registry newest date-time tag wins when local has nothing", nil, []string{"20260101-120000", "20260301-000000"}, "20260301-000000"},
		{"fallback to latest when both are empty", nil, nil, defaultImageTagFallback},
		{"fallback to latest when local/registry hold only non-date-time and empty entries", []string{""}, []string{"", "latest"}, defaultImageTagFallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultImageTagFrom(tc.local, tc.registry); got != tc.want {
				t.Errorf("defaultImageTagFrom(%v, %v) = %q, want %q", tc.local, tc.registry, got, tc.want)
			}
		})
	}
}

func TestOfferedImageTags(t *testing.T) {
	t.Run("only the newest offeredImageTagLimit published tags survive", func(t *testing.T) {
		var registryTags []string
		for i := 0; i < offeredImageTagLimit+3; i++ {
			registryTags = append(registryTags, fmt.Sprintf("2026%02d01-000000", i+1))
		}
		got := OfferedImageTags(registryTags, nil, "")
		if len(got) != offeredImageTagLimit {
			t.Fatalf("len(OfferedImageTags) = %d, want %d: %+v", len(got), offeredImageTagLimit, got)
		}
		want := SortTagsNewestFirst(registryTags)[:offeredImageTagLimit]
		for i, o := range got {
			if o.Tag != want[i] || o.Present {
				t.Errorf("Options[%d] = %+v, want {%q false}", i, o, want[i])
			}
		}
	})

	t.Run("an old local tag survives even when it fell out of the published top N", func(t *testing.T) {
		var registryTags []string
		for i := 0; i < offeredImageTagLimit+2; i++ {
			registryTags = append(registryTags, fmt.Sprintf("2026%02d01-000000", i+1))
		}
		const oldLocal = "20200101-000000"
		got := OfferedImageTags(registryTags, []string{oldLocal}, "")
		var found *ImageTagOption
		for i := range got {
			if got[i].Tag == oldLocal {
				found = &got[i]
			}
		}
		if found == nil {
			t.Fatalf("old local tag %q missing from Options: %+v", oldLocal, got)
		}
		if !found.Present {
			t.Errorf("old local tag Present = false, want true")
		}
	})

	t.Run("a tag both published and local appears once with Present true", func(t *testing.T) {
		const tag = "20260101-120000"
		got := OfferedImageTags([]string{tag}, []string{tag}, "")
		count := 0
		for _, o := range got {
			if o.Tag == tag {
				count++
				if !o.Present {
					t.Errorf("Present = false, want true")
				}
			}
		}
		if count != 1 {
			t.Errorf("tag %q appeared %d times in %+v, want exactly 1", tag, count, got)
		}
	})

	t.Run("a published-only tag has Present false", func(t *testing.T) {
		got := OfferedImageTags([]string{"20260101-120000"}, nil, "")
		if len(got) != 1 || got[0].Present {
			t.Errorf("Options = %+v, want one entry with Present false", got)
		}
	})

	t.Run("duplicates within registryTags and localTags are each collapsed to one entry", func(t *testing.T) {
		got := OfferedImageTags(
			[]string{"20260101-120000", "20260101-120000"},
			[]string{"dev", "dev"},
			"",
		)
		counts := map[string]int{}
		for _, o := range got {
			counts[o.Tag]++
		}
		for tag, n := range counts {
			if n != 1 {
				t.Errorf("tag %q appeared %d times in %+v, want 1", tag, n, got)
			}
		}
	})

	t.Run("a non-date-time local tag is offered", func(t *testing.T) {
		got := OfferedImageTags(nil, []string{"dev"}, "")
		if len(got) != 1 || got[0].Tag != "dev" || !got[0].Present {
			t.Errorf("Options = %+v, want [{dev true}]", got)
		}
	})

	t.Run("defaultTag alone produces exactly one entry", func(t *testing.T) {
		got := OfferedImageTags(nil, nil, "latest")
		if len(got) != 1 || got[0].Tag != "latest" || got[0].Present {
			t.Errorf("Options = %+v, want [{latest false}]", got)
		}
	})

	t.Run("all-nil/empty inputs produce an empty slice", func(t *testing.T) {
		got := OfferedImageTags(nil, nil, "")
		if len(got) != 0 {
			t.Errorf("Options = %+v, want empty", got)
		}
	})
}

func TestLocalImageTags(t *testing.T) {
	ctx := context.Background()

	t.Run("only the harness's own repo tags come back", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		repo := RepoWithoutTag(m.cfg.AgentImageClaudeCode)
		f.AddImage(repo + ":20260101-120000")
		f.AddImage(repo + ":20260201-090000")
		// A different repo entirely -- must not leak into the harness's tags.
		f.AddImage("other.example.com/other-image:20260301-000000")

		got := m.localImageTags(ctx, config.HarnessClaudeCode)
		// "dev" comes from newTestManagerCfg's own pre-seeding of
		// cfg.AgentImageClaudeCode.
		want := []string{"dev", "20260101-120000", "20260201-090000"}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("localImageTags = %v, want %v", got, want)
		}
	})

	t.Run("a docker error degrades to nothing present, no panic", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		f.Fail(dockerclienttest.OpImageList, errors.New("boom"))

		got := m.localImageTags(ctx, config.HarnessClaudeCode)
		if got != nil {
			t.Errorf("localImageTags on a docker error = %v, want nil", got)
		}
	})
}

// TestResolveAgentImageRef_EmptyTagUsesLocalDefault proves resolveAgentImageRef's
// empty-tag path prefers what the daemon already holds over the registry's
// published tags, and falls back to :latest when neither has anything.
// Deliberately builds its own bare Manager (not newTestManager, whose
// newTestManagerCfg helper pre-seeds a "dev" image that would mask the
// no-local-tag case below).
func TestResolveAgentImageRef_EmptyTagUsesLocalDefault(t *testing.T) {
	ctx := context.Background()
	repo := RepoWithoutTag(testAgentImageClaudeCode)

	newBareManager := func(t *testing.T) (*Manager, *dockerclienttest.Fake, *store.Store) {
		t.Helper()
		f := dockerclienttest.New()
		cfg := testConfig(5)
		st := newTestStore(t, cfg.MaxAgents)
		return NewManager(f, nil, st, cfg, testLogger(), testOptions()), f, st
	}

	t.Run("a local date-time tag wins over a newer registry tag", func(t *testing.T) {
		m, f, st := newBareManager(t)
		f.AddImage(repo + ":20260101-120000")
		if err := st.SetAgentImageTags(ctx, config.HarnessClaudeCode, store.AgentImageTags{Tags: []string{"20260301-000000"}}); err != nil {
			t.Fatalf("seeding registry snapshot: %v", err)
		}
		want := repo + ":20260101-120000"
		got, err := m.resolveAgentImageRef(ctx, "", config.HarnessClaudeCode)
		if err != nil || got != want {
			t.Errorf("resolveAgentImageRef(\"\") = (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("no local tag: the registry's newest date-time tag wins", func(t *testing.T) {
		m, _, st := newBareManager(t)
		if err := st.SetAgentImageTags(ctx, config.HarnessClaudeCode, store.AgentImageTags{Tags: []string{"20260201-000000", "20260301-000000"}}); err != nil {
			t.Fatalf("seeding registry snapshot: %v", err)
		}
		want := repo + ":20260301-000000"
		got, err := m.resolveAgentImageRef(ctx, "", config.HarnessClaudeCode)
		if err != nil || got != want {
			t.Errorf("resolveAgentImageRef(\"\") = (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("neither local nor registry: falls back to :latest", func(t *testing.T) {
		m, _, _ := newBareManager(t)
		want := repo + ":latest"
		got, err := m.resolveAgentImageRef(ctx, "", config.HarnessClaudeCode)
		if err != nil || got != want {
			t.Errorf("resolveAgentImageRef(\"\") = (%q, %v), want (%q, nil)", got, err, want)
		}
	})
}

// TestAgentImageInventory proves AgentImageInventory's every field matches
// what the two pure functions (defaultImageTagFrom, OfferedImageTags) would
// produce given the same registry snapshot and local images.
func TestAgentImageInventory(t *testing.T) {
	ctx := context.Background()
	m, f, st := newTestManager(t, 5)

	repo := RepoWithoutTag(m.cfg.AgentImageClaudeCode)
	f.AddImage(repo + ":20260101-120000")
	registryTags := []string{"20260201-090000", "20260301-000000"}
	if err := st.SetAgentImageTags(ctx, config.HarnessClaudeCode, store.AgentImageTags{Tags: registryTags, LastError: "boom"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	got, err := m.AgentImageInventory(ctx, config.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("AgentImageInventory: %v", err)
	}

	local := m.localImageTags(ctx, config.HarnessClaudeCode)
	wantDefault := defaultImageTagFrom(local, registryTags)
	wantOptions := OfferedImageTags(registryTags, local, wantDefault)

	if got.Harness != config.HarnessClaudeCode {
		t.Errorf("Harness = %q, want %q", got.Harness, config.HarnessClaudeCode)
	}
	if got.Repo != repo {
		t.Errorf("Repo = %q, want %q", got.Repo, repo)
	}
	if !reflect.DeepEqual(got.RegistryTags, registryTags) {
		t.Errorf("RegistryTags = %v, want %v", got.RegistryTags, registryTags)
	}
	sortedGotLocal := append([]string{}, got.LocalTags...)
	sortedWantLocal := append([]string{}, local...)
	sort.Strings(sortedGotLocal)
	sort.Strings(sortedWantLocal)
	if !reflect.DeepEqual(sortedGotLocal, sortedWantLocal) {
		t.Errorf("LocalTags = %v, want %v", got.LocalTags, local)
	}
	if got.DefaultTag != wantDefault {
		t.Errorf("DefaultTag = %q, want %q", got.DefaultTag, wantDefault)
	}
	if !reflect.DeepEqual(got.Options, wantOptions) {
		t.Errorf("Options = %+v, want %+v", got.Options, wantOptions)
	}
	if got.LastError != "boom" {
		t.Errorf("LastError = %q, want %q", got.LastError, "boom")
	}
}

// TestRefreshAgentImageTags_PerHarnessIsolation proves refreshing one
// harness's tags never calls the OTHER harness's registry client and never
// touches the other harness's stored snapshot, in both directions.
func TestRefreshAgentImageTags_PerHarnessIsolation(t *testing.T) {
	ctx := context.Background()

	t.Run("refreshing claude-code leaves opencode's registry and snapshot untouched", func(t *testing.T) {
		m, _, st := newTestManager(t, 1)
		ccFake := &registrytest.Fake{Tags: []string{"20260101-120000"}}
		ocFake := &registrytest.Fake{Tags: []string{"20260201-120000"}}
		m.registries = map[string]registry.Client{
			config.HarnessClaudeCode: ccFake,
			config.HarnessOpenCode:   ocFake,
		}
		seed := store.AgentImageTags{Tags: []string{"20250101-000000"}}
		if err := st.SetAgentImageTags(ctx, config.HarnessOpenCode, seed); err != nil {
			t.Fatalf("seeding opencode snapshot: %v", err)
		}

		if err := m.RefreshAgentImageTags(ctx, config.HarnessClaudeCode); err != nil {
			t.Fatalf("RefreshAgentImageTags(claude-code): %v", err)
		}

		if got := ocFake.Calls(); got != 0 {
			t.Errorf("opencode's registry was called %d times, want 0", got)
		}
		ocGot, _, err := st.GetAgentImageTags(ctx, config.HarnessOpenCode)
		if err != nil {
			t.Fatalf("GetAgentImageTags(opencode): %v", err)
		}
		if !reflect.DeepEqual(ocGot.Tags, seed.Tags) {
			t.Errorf("opencode snapshot Tags = %v, want untouched %v", ocGot.Tags, seed.Tags)
		}
	})

	t.Run("refreshing opencode leaves claude-code's registry and snapshot untouched", func(t *testing.T) {
		m, _, st := newTestManager(t, 1)
		ccFake := &registrytest.Fake{Tags: []string{"20260101-120000"}}
		ocFake := &registrytest.Fake{Tags: []string{"20260201-120000"}}
		m.registries = map[string]registry.Client{
			config.HarnessClaudeCode: ccFake,
			config.HarnessOpenCode:   ocFake,
		}
		seed := store.AgentImageTags{Tags: []string{"20250101-000000"}}
		if err := st.SetAgentImageTags(ctx, config.HarnessClaudeCode, seed); err != nil {
			t.Fatalf("seeding claude-code snapshot: %v", err)
		}

		if err := m.RefreshAgentImageTags(ctx, config.HarnessOpenCode); err != nil {
			t.Fatalf("RefreshAgentImageTags(opencode): %v", err)
		}

		if got := ccFake.Calls(); got != 0 {
			t.Errorf("claude-code's registry was called %d times, want 0", got)
		}
		ccGot, _, err := st.GetAgentImageTags(ctx, config.HarnessClaudeCode)
		if err != nil {
			t.Fatalf("GetAgentImageTags(claude-code): %v", err)
		}
		if !reflect.DeepEqual(ccGot.Tags, seed.Tags) {
			t.Errorf("claude-code snapshot Tags = %v, want untouched %v", ccGot.Tags, seed.Tags)
		}
	})
}
