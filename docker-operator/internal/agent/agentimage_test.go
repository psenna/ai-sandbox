package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
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
		// A domain-less reference is NORMALIZED to Docker Hub, which is exactly
		// the form this repo then hands to ImageList -- the #205 seam.
		"myorg/agent-image": "docker.io/myorg/agent-image",
		"busybox":           "docker.io/library/busybox",
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

// TestOfferedImageTagsAreFlatNewestFirst is its own function rather than a
// subtest of TestOfferedImageTags so that function stays under the gocyclo
// limit -- what it asserts is also a different axis (ordering) from that
// one's (membership and Present).
func TestOfferedImageTagsAreFlatNewestFirst(t *testing.T) {
	// The host holds a NEWER tag than anything published. Ordering by
	// provenance -- published first, then local -- would sort the 20251201
	// published tag above the host's 20260101 one, so the chooser read as
	// almost-sorted. The whole result must be ordered by tag name alone.
	got := OfferedImageTags(
		[]string{"20250901-000000", "20251201-000000"},
		[]string{"20260101-000000", "latest"},
		"",
	)
	want := []string{"latest", "20260101-000000", "20251201-000000", "20250901-000000"}
	if len(got) != len(want) {
		t.Fatalf("Options = %+v, want %d entries", got, len(want))
	}
	for i, tag := range want {
		if got[i].Tag != tag {
			t.Errorf("Options[%d].Tag = %q, want %q (full: %+v)", i, got[i].Tag, tag, got)
		}
	}
	if !got[1].Present || got[2].Present {
		t.Errorf("Present flags = %+v, want only the host-held tag true", got)
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

	t.Run("a domain-less repo still finds the host's images (#205)", func(t *testing.T) {
		cfg := testConfig(5)
		cfg.AgentImageClaudeCode = "myorg/agent-image"
		m, f, _ := newTestManagerCfg(t, cfg)

		f.AddImage("myorg/agent-image:20260101-120000")
		// A decoy from another registry -- a form-blind comparison could
		// confuse it for the same repository, and it must not leak in.
		f.AddImage("other.example.com/other-image:20260301-000000")

		got := m.localImageTags(ctx, config.HarnessClaudeCode)
		want := []string{"20260101-120000"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("localImageTags = %v, want %v", got, want)
		}

		// The host-present tag must win over the :latest fallback.
		wantRef := "docker.io/myorg/agent-image:20260101-120000"
		ref, err := m.resolveAgentImageRef(ctx, "", config.HarnessClaudeCode)
		if err != nil || ref != wantRef {
			t.Errorf("resolveAgentImageRef(\"\") = (%q, %v), want (%q, nil)", ref, err, wantRef)
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

// --- image tag manager: delete, cleanup, pull (#the tag manager) -----------

// newTagManager builds a Manager against a fresh Fake and store with the
// registries supplied by the caller, so a pull test can seed a registry with
// published tags. Everything else mirrors newTestManagerCfg.
func newTagManager(t *testing.T, regs map[string]registry.Client) (*Manager, *dockerclienttest.Fake, *store.Store) {
	t.Helper()
	cfg := testConfig(5)
	f := dockerclienttest.New()
	f.AutoHealthy = true
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	st := newTestStore(t, cfg.MaxAgents)
	m := NewManager(f, regs, st, cfg, testLogger(), testOptions())
	return m, f, st
}

// seedTagManagerImages seeds the claude-code test repository with a tag set
// that exercises every removal rule at once:
//
//	20260101-000000  named by agt_a's Image        -> in use by reference
//	20260102-000000  alias of agt_a's image ID     -> in use by ID
//	20260103-000000  the newest local date-time tag -> cleanup's always-kept
//	latest           unreferenced                 -> deletable
//
// and returns the repo.
func seedTagManagerImages(f *dockerclienttest.Fake) string {
	const repo = "test.example.com/agent-image"
	f.AddImageWithID(repo+":20260101-000000", "sha256:shared")
	f.AddImageWithID(repo+":20260102-000000", "sha256:shared")
	f.AddImageWithID(repo+":20260103-000000", "sha256:20260103")
	f.AddImage(repo + ":latest")
	return repo
}

// seedTagManagerAgent inserts a record whose Image names the 20260101 tag and
// whose container last started from the image both date-time tags share.
func seedTagManagerAgent(t *testing.T, st *store.Store, repo string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Create(ctx, store.CreateSpec{ID: "agt_tagmgr", Harness: config.HarnessClaudeCode}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Update(ctx, "agt_tagmgr", func(a *store.Agent) error {
		a.Image = repo + ":20260101-000000"
		a.ImageID = "sha256:shared"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestDeleteAgentImageTag(t *testing.T) {
	t.Run("refuses a tag an agent record was created against", func(t *testing.T) {
		m, f, st := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f)
		seedTagManagerAgent(t, st, repo)

		err := m.DeleteAgentImageTag(context.Background(), config.HarnessClaudeCode, "20260101-000000")
		if !IsImageTagInUse(err) {
			t.Fatalf("DeleteAgentImageTag(named tag) = %v, want IsImageTagInUse", err)
		}
		if containsOp(f.Calls(), dockerclienttest.OpImageRemove) {
			t.Errorf("an in-use tag must not be removed; calls: %+v", f.Calls())
		}
	})

	t.Run("refuses a tag aliasing an image an agent runs", func(t *testing.T) {
		m, f, st := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f)
		seedTagManagerAgent(t, st, repo)

		// 20260102 is not named by any record, but points at the same image
		// (sha256:shared) the record's ImageID names.
		err := m.DeleteAgentImageTag(context.Background(), config.HarnessClaudeCode, "20260102-000000")
		if !IsImageTagInUse(err) {
			t.Fatalf("DeleteAgentImageTag(aliased tag) = %v, want IsImageTagInUse", err)
		}
		if containsOp(f.Calls(), dockerclienttest.OpImageRemove) {
			t.Errorf("an aliased in-use tag must not be removed; calls: %+v", f.Calls())
		}
	})

	t.Run("removes an unreferenced tag, including the newest and :latest", func(t *testing.T) {
		m, f, _ := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f)

		// The manual delete is the scalpel: no newest-date-time protection,
		// only the in-use one. Both tags here are unreferenced.
		for _, tag := range []string{"20260103-000000", "latest"} {
			if err := m.DeleteAgentImageTag(context.Background(), config.HarnessClaudeCode, tag); err != nil {
				t.Fatalf("DeleteAgentImageTag(%q) = %v, want nil", tag, err)
			}
		}
		if _, err := f.ImageInspect(context.Background(), repo+":latest"); !dockerclient.IsNotFound(err) {
			t.Errorf("ImageInspect(%s:latest) after delete = %v, want not found", repo, err)
		}
	})

	t.Run("a tag the daemon does not hold is ErrImageTagMissing", func(t *testing.T) {
		m, f, _ := newTagManager(t, newTestRegistries())
		seedTagManagerImages(f)

		err := m.DeleteAgentImageTag(context.Background(), config.HarnessClaudeCode, "20260909-000000")
		if !IsImageTagMissing(err) {
			t.Fatalf("DeleteAgentImageTag(absent tag) = %v, want IsImageTagMissing", err)
		}
	})

	t.Run("a syntactically invalid tag is ErrInvalidImageTag", func(t *testing.T) {
		m, _, _ := newTagManager(t, newTestRegistries())

		err := m.DeleteAgentImageTag(context.Background(), config.HarnessClaudeCode, "not a tag!")
		if !IsInvalidImageTag(err) {
			t.Fatalf("DeleteAgentImageTag(invalid tag) = %v, want IsInvalidImageTag", err)
		}
	})

	t.Run("a failed store read refuses to remove anything", func(t *testing.T) {
		m, f, st := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f)
		_ = repo
		if err := st.Close(); err != nil {
			t.Fatalf("closing the store: %v", err)
		}

		err := m.DeleteAgentImageTag(context.Background(), config.HarnessClaudeCode, "latest")
		if err == nil {
			t.Fatal("DeleteAgentImageTag with an unreadable store = nil, want an error -- fail closed")
		}
		if containsOp(f.Calls(), dockerclienttest.OpImageRemove) {
			t.Errorf("a failed usage read must not remove anything; calls: %+v", f.Calls())
		}
	})
}

func TestCleanupAgentImages(t *testing.T) {
	t.Run("removes only the unreferenced tags, keeping in-use and newest", func(t *testing.T) {
		m, f, st := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f)
		seedTagManagerAgent(t, st, repo)

		report, err := m.CleanupAgentImages(context.Background(), config.HarnessClaudeCode)
		if err != nil {
			t.Fatalf("CleanupAgentImages: %v", err)
		}
		// latest is the only unreferenced, non-newest tag in the seed set.
		if !reflect.DeepEqual(report.Removed, []string{"latest"}) {
			t.Errorf("Removed = %v, want [latest]", report.Removed)
		}
		for _, tag := range []string{"20260101-000000", "20260102-000000", "20260103-000000"} {
			if _, err := f.ImageInspect(context.Background(), repo+":"+tag); err != nil {
				t.Errorf("ImageInspect(%s:%s) after cleanup = %v, want kept", repo, tag, err)
			}
		}
	})

	t.Run("with no agent records everything but the newest local tag goes", func(t *testing.T) {
		m, f, _ := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f)

		report, err := m.CleanupAgentImages(context.Background(), config.HarnessClaudeCode)
		if err != nil {
			t.Fatalf("CleanupAgentImages: %v", err)
		}
		// Sorted walk: latest < 20260101 < 20260102 < 20260103(newest, kept).
		if !reflect.DeepEqual(report.Removed, []string{"20260101-000000", "20260102-000000", "latest"}) {
			t.Errorf("Removed = %v, want the three unreferenced tags", report.Removed)
		}
		if _, err := f.ImageInspect(context.Background(), repo+":20260103-000000"); err != nil {
			t.Errorf("ImageInspect(newest) after cleanup = %v, want kept", err)
		}
	})

	t.Run("a per-tag daemon refusal is recorded and the rest still removed", func(t *testing.T) {
		m, f, _ := newTagManager(t, newTestRegistries())
		const repo = "test.example.com/agent-image"
		// Two unreferenced non-date-time tags; the walk is sorted, so the
		// FIRST removal (aaa) is the one the injected failure hits.
		f.AddImage(repo + ":aaa")
		f.AddImage(repo + ":zzz")
		f.FailOnce(dockerclienttest.OpImageRemove, errors.New("daemon says no"))

		report, err := m.CleanupAgentImages(context.Background(), config.HarnessClaudeCode)
		if err != nil {
			t.Fatalf("CleanupAgentImages: %v", err)
		}
		if _, ok := report.Failed["aaa"]; !ok {
			t.Errorf("Failed = %v, want an entry for aaa", report.Failed)
		}
		if !reflect.DeepEqual(report.Removed, []string{"zzz"}) {
			t.Errorf("Removed = %v, want [zzz] -- one refusal must not stop the rest", report.Removed)
		}
	})

	t.Run("a failed image list removes nothing and returns an error", func(t *testing.T) {
		m, f, _ := newTagManager(t, newTestRegistries())
		seedTagManagerImages(f)
		f.Fail(dockerclienttest.OpImageList, errors.New("daemon says no"))

		report, err := m.CleanupAgentImages(context.Background(), config.HarnessClaudeCode)
		if err == nil {
			t.Fatal("CleanupAgentImages with a failed image list = nil error, want fail closed")
		}
		if len(report.Removed) != 0 {
			t.Errorf("Removed = %v, want empty", report.Removed)
		}
	})

	t.Run("one harness's repository is never touched from the other", func(t *testing.T) {
		m, f, _ := newTagManager(t, newTestRegistries())
		repo := seedTagManagerImages(f) // claude-code only
		f.AddImage("test.example.com/agent-image-opencode:20260101-000000")

		if _, err := m.CleanupAgentImages(context.Background(), config.HarnessClaudeCode); err != nil {
			t.Fatalf("CleanupAgentImages: %v", err)
		}
		if _, err := f.ImageInspect(context.Background(), "test.example.com/agent-image-opencode:20260101-000000"); err != nil {
			t.Errorf("the opencode repository was touched by a claude-code cleanup: %v", err)
		}
		_ = repo
	})
}

func TestPullLatestAgentImage(t *testing.T) {
	ctx := context.Background()

	t.Run("pulls the newest published date-time tag", func(t *testing.T) {
		regs := map[string]registry.Client{
			config.HarnessClaudeCode: &registrytest.Fake{Tags: []string{"20260101-000000", "20260102-000000", "latest"}},
		}
		m, f, _ := newTagManager(t, regs)

		report := m.PullLatestAgentImage(ctx, config.HarnessClaudeCode)
		if report.Error != "" {
			t.Fatalf("report.Error = %q, want empty", report.Error)
		}
		if report.Tag != "20260102-000000" {
			t.Errorf("report.Tag = %q, want the newest published tag", report.Tag)
		}
		var pulled bool
		for _, c := range f.Calls() {
			if c.Op == dockerclienttest.OpImagePull && c.Target == "test.example.com/agent-image:20260102-000000" {
				pulled = true
			}
		}
		if !pulled {
			t.Errorf("no OpImagePull of the newest tag recorded; calls: %+v", f.Calls())
		}
	})

	t.Run("no registry client is a report error, not a returned one", func(t *testing.T) {
		m, _, _ := newTagManager(t, map[string]registry.Client{})

		report := m.PullLatestAgentImage(ctx, config.HarnessClaudeCode)
		if report.Error == "" {
			t.Error("report.Error = \"\", want it to say no registry client is configured")
		}
	})

	t.Run("nothing published yet is a report error", func(t *testing.T) {
		regs := map[string]registry.Client{
			config.HarnessClaudeCode: &registrytest.Fake{Tags: []string{"latest"}},
		}
		m, _, _ := newTagManager(t, regs)

		report := m.PullLatestAgentImage(ctx, config.HarnessClaudeCode)
		if report.Error == "" {
			t.Error("report.Error = \"\", want it to say no published date-time tag is known")
		}
	})

	t.Run("a failed pull is a report error", func(t *testing.T) {
		regs := map[string]registry.Client{
			config.HarnessClaudeCode: &registrytest.Fake{Tags: []string{"20260101-000000"}},
		}
		m, f, _ := newTagManager(t, regs)
		f.Fail(dockerclienttest.OpImagePull, errors.New("registry unreachable"))

		report := m.PullLatestAgentImage(ctx, config.HarnessClaudeCode)
		if report.Error == "" {
			t.Error("report.Error = \"\", want the pull failure in it")
		}
		if report.Tag != "" {
			t.Errorf("report.Tag = %q, want empty on failure", report.Tag)
		}
	})
}

// containsOp reports whether the fake's call log holds at least one call of op.
func containsOp(calls []dockerclienttest.Call, op dockerclienttest.Op) bool {
	for _, c := range calls {
		if c.Op == op {
			return true
		}
	}
	return false
}
