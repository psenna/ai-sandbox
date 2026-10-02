package dockerclient

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("netip.ParseAddr(%q): %v", s, err)
	}
	return addr
}

func TestEnvSlice(t *testing.T) {
	if got := envSlice(nil); got != nil {
		t.Errorf("envSlice(nil) = %#v, want nil", got)
	}
	if got := envSlice(map[string]string{}); got != nil {
		t.Errorf("envSlice(empty) = %#v, want nil", got)
	}
	got := envSlice(map[string]string{"B": "2", "A": "1", "C": "3"})
	want := []string{"A=1", "B=2", "C=3"}
	if !equalStrings(got, want) {
		t.Errorf("envSlice = %v, want %v", got, want)
	}
}

func TestLabelFilter(t *testing.T) {
	if f := labelFilter(nil); f != nil {
		t.Errorf("labelFilter(nil) = %#v, want nil", f)
	}
	if f := labelFilter(map[string]string{}); f != nil {
		t.Errorf("labelFilter(empty) = %#v, want nil", f)
	}
	f := labelFilter(map[string]string{"a": "1", "b": "2"})
	if f == nil {
		t.Fatal("labelFilter(non-empty) = nil, want a filter")
	}
	got := f["label"]
	want := map[string]bool{"a=1": true, "b=2": true}
	if len(got) != len(want) {
		t.Fatalf("label terms = %v, want %v", got, want)
	}
	for term := range got {
		if !want[term] {
			t.Errorf("unexpected label term %q", term)
		}
	}
}

// TestFamiliarRepo pins the canonicalization #205 relies on: a repo in either
// the normalized or the familiar form comes back as the FAMILIAR form the
// daemon's images/json API matches and reports. A repo that does not parse
// comes back unchanged (matching agent.RepoWithoutTag's fallback), so a
// malformed input fails visibly by matching nothing.
func TestFamiliarRepo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare Docker Hub official name", "alpine", "alpine"},
		{"library-namespaced", "library/alpine", "alpine"},
		{"normalized Docker Hub official name", "docker.io/library/alpine", "alpine"},
		{"familiar org image", "myorg/agent-image", "myorg/agent-image"},
		{"normalized org image", "docker.io/myorg/agent-image", "myorg/agent-image"},
		{"third-party registry is already familiar", "ghcr.io/psenna/ai-sandbox-agent", "ghcr.io/psenna/ai-sandbox-agent"},
		{"registry with a port", "host:5000/team/img", "host:5000/team/img"},
		{"tag is stripped", "ghcr.io/psenna/x:tag", "ghcr.io/psenna/x"},
		{"unparseable is returned unchanged", "not a ref!!", "not a ref!!"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FamiliarRepo(tc.in); got != tc.want {
				t.Errorf("FamiliarRepo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReferenceFilter(t *testing.T) {
	if f := referenceFilter(""); f != nil {
		t.Errorf("referenceFilter(\"\") = %#v, want nil", f)
	}
	f := referenceFilter("myapp")
	if f == nil {
		t.Fatal("referenceFilter(myapp) = nil, want a filter")
	}
	if !f["reference"]["myapp:*"] {
		t.Errorf("reference terms = %v, want to contain myapp:*", f["reference"])
	}

	// A NORMALIZED repo canonicalized through FamiliarRepo produces the filter
	// the daemon actually matches (#205): the daemon matches on the familiar
	// name, so "docker.io/myorg/agent-image:*" would match nothing.
	fNorm := referenceFilter(FamiliarRepo("docker.io/myorg/agent-image"))
	if fNorm == nil || !fNorm["reference"]["myorg/agent-image:*"] {
		t.Errorf("referenceFilter(FamiliarRepo(docker.io/myorg/agent-image)) = %#v, want to contain myorg/agent-image:*", fNorm)
	}
}

func TestKeepRepoTags(t *testing.T) {
	if got := keepRepoTags("myapp", nil); got != nil {
		t.Errorf("keepRepoTags(nil) = %#v, want nil", got)
	}

	in := []string{
		"myapp:v1",
		"myapp:v2",
		"myapp-other:v1", // same-named prefix, different repo -- must be excluded
		"otherapp:v1",
		"<none>:<none>", // dangling -- must be dropped
		"registry.example.com:5000/myapp:v1",
	}
	got := keepRepoTags("myapp", in)
	want := []string{"myapp:v1", "myapp:v2"}
	if !equalStrings(got, want) {
		t.Errorf("keepRepoTags(myapp) = %v, want %v", got, want)
	}

	// An empty repo keeps every genuinely tagged entry, dropping only the
	// dangling "<none>:<none>" form.
	gotAll := keepRepoTags("", in)
	wantAll := []string{"myapp:v1", "myapp:v2", "myapp-other:v1", "otherapp:v1", "registry.example.com:5000/myapp:v1"}
	if !equalStrings(gotAll, wantAll) {
		t.Errorf("keepRepoTags(\"\") = %v, want %v", gotAll, wantAll)
	}

	if got := keepRepoTags("unknown/repo", in); got != nil {
		t.Errorf("keepRepoTags(unknown repo) = %v, want nil", got)
	}

	// A NORMALIZED repo, canonicalized through FamiliarRepo first, keeps only
	// the familiar-form entry: the daemon reports RepoTags in the familiar form
	// (#205), so the normalized "docker.io/myorg/agent-image:v2" is a different
	// repository and must be excluded.
	gotFam := keepRepoTags(FamiliarRepo("docker.io/myorg/agent-image"), []string{
		"myorg/agent-image:v1",
		"docker.io/myorg/agent-image:v2",
		"other:v1",
	})
	wantFam := []string{"myorg/agent-image:v1"}
	if !equalStrings(gotFam, wantFam) {
		t.Errorf("keepRepoTags(FamiliarRepo(docker.io/myorg/agent-image)) = %v, want %v", gotFam, wantFam)
	}
}

func TestEventFilter(t *testing.T) {
	if f := eventFilter(EventFilter{}); f != nil {
		t.Errorf("eventFilter(empty) = %#v, want nil", f)
	}
	f := eventFilter(EventFilter{
		Types:  []EventType{EventTypeContainer, EventTypeNetwork},
		Labels: map[string]string{"a": "1"},
	})
	if f == nil {
		t.Fatal("eventFilter(non-empty) = nil, want a filter")
	}
	wantTypes := map[string]bool{"container": true, "network": true}
	gotTypes := f["type"]
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("type terms = %v, want %v", gotTypes, wantTypes)
	}
	for term := range gotTypes {
		if !wantTypes[term] {
			t.Errorf("unexpected type term %q", term)
		}
	}
	if !f["label"]["a=1"] {
		t.Errorf("label terms = %v, want to contain a=1", f["label"])
	}

	// Types alone, no labels, must still produce a filter.
	f2 := eventFilter(EventFilter{Types: []EventType{EventTypeVolume}})
	if f2 == nil || !f2["type"]["volume"] {
		t.Errorf("eventFilter(types only) = %#v, want a type=volume filter", f2)
	}
}

func TestToMounts(t *testing.T) {
	if got := toMounts(nil); got != nil {
		t.Errorf("toMounts(nil) = %#v, want nil", got)
	}
	in := []Mount{
		{Type: MountTypeVolume, Source: "vol1", Target: "/data", ReadOnly: true},
		{Type: MountTypeBind, Source: "/host", Target: "/container"},
		{Type: MountTypeVolume, Source: "fs", Target: "/workspace/store", Subpath: "agents/agt_1"},
		{Type: MountTypeVolume, Source: "fs", Target: "/whole", Subpath: ""},
		{Type: MountTypeVolume, Source: "fs", Target: "/workspace/shared", Subpath: "shared", ReadOnly: true},
	}
	got := toMounts(in)
	if len(got) != 5 {
		t.Fatalf("len(toMounts) = %d, want 5", len(got))
	}
	if string(got[0].Type) != "volume" || got[0].Source != "vol1" || got[0].Target != "/data" || !got[0].ReadOnly {
		t.Errorf("toMounts[0] = %#v", got[0])
	}
	// Existing mounts must keep VolumeOptions nil -- a non-nil one changes
	// the daemon wire format and breaks a plain volume/bind mount.
	if got[0].VolumeOptions != nil {
		t.Errorf("toMounts[0].VolumeOptions = %#v, want nil", got[0].VolumeOptions)
	}
	if string(got[1].Type) != "bind" || got[1].Source != "/host" || got[1].ReadOnly {
		t.Errorf("toMounts[1] = %#v", got[1])
	}
	if got[1].VolumeOptions != nil {
		t.Errorf("toMounts[1].VolumeOptions = %#v, want nil", got[1].VolumeOptions)
	}
	// A non-empty Subpath yields a VolumeOptions carrying exactly it.
	if got[2].VolumeOptions == nil || got[2].VolumeOptions.Subpath != "agents/agt_1" {
		t.Errorf("toMounts[2].VolumeOptions = %#v, want Subpath=agents/agt_1", got[2].VolumeOptions)
	}
	// An empty Subpath leaves VolumeOptions nil.
	if got[3].VolumeOptions != nil {
		t.Errorf("toMounts[3].VolumeOptions = %#v, want nil for an empty Subpath", got[3].VolumeOptions)
	}
	// A read-only subpath mount (the shared/ common area) carries both the
	// Subpath and ReadOnly.
	if !got[4].ReadOnly || got[4].VolumeOptions == nil || got[4].VolumeOptions.Subpath != "shared" {
		t.Errorf("toMounts[4] = %#v, want ReadOnly with Subpath=shared", got[4])
	}
}

func TestToHealthConfig(t *testing.T) {
	if got := toHealthConfig(nil); got != nil {
		t.Errorf("toHealthConfig(nil) = %#v, want nil", got)
	}
	h := &Healthcheck{
		Test:        []string{"CMD", "true"},
		Interval:    5 * time.Second,
		Timeout:     2 * time.Second,
		Retries:     3,
		StartPeriod: time.Second,
	}
	got := toHealthConfig(h)
	if got == nil {
		t.Fatal("toHealthConfig(non-nil) = nil")
	}
	if len(got.Test) != 2 || got.Test[0] != "CMD" || got.Interval != h.Interval ||
		got.Timeout != h.Timeout || got.Retries != h.Retries || got.StartPeriod != h.StartPeriod {
		t.Errorf("toHealthConfig = %#v, want fields matching %#v", got, h)
	}
}

func TestToContainer(t *testing.T) {
	t.Run("zero value has no nil panics and sane defaults", func(t *testing.T) {
		got := toContainer(container.InspectResponse{ID: "abc", Name: "/foo"})
		if got.ID != "abc" || got.Name != "foo" {
			t.Errorf("ID/Name = %q/%q, want abc/foo", got.ID, got.Name)
		}
		if got.Health != HealthNone {
			t.Errorf("Health = %q, want %q", got.Health, HealthNone)
		}
		if got.Networks == nil {
			t.Errorf("Networks = nil, want an empty non-nil map")
		}
	})

	t.Run("full response", func(t *testing.T) {
		resp := container.InspectResponse{
			ID:    "abc123",
			Name:  "/my-container",
			Image: "sha256:ignored",
			Config: &container.Config{
				Image:  "alpine:latest",
				Labels: map[string]string{"k": "v"},
			},
			State: &container.State{
				Status:   container.StateRunning,
				ExitCode: 7,
				Health:   &container.Health{Status: container.Healthy},
			},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"dinernet": {IPAddress: mustAddr(t, "172.31.1.2")},
					"nilentry": nil,
				},
			},
		}
		got := toContainer(resp)
		if got.ID != "abc123" || got.Name != "my-container" {
			t.Errorf("ID/Name = %q/%q", got.ID, got.Name)
		}
		if got.Image != "alpine:latest" {
			t.Errorf("Image = %q, want alpine:latest (from Config, not top-level)", got.Image)
		}
		if got.Labels["k"] != "v" {
			t.Errorf("Labels = %v, want k=v", got.Labels)
		}
		if got.State != StateRunning || got.ExitCode != 7 || got.Health != HealthHealthy {
			t.Errorf("State/ExitCode/Health = %v/%d/%v", got.State, got.ExitCode, got.Health)
		}
		if len(got.Networks) != 1 {
			t.Fatalf("Networks = %v, want exactly 1 entry (nil endpoint skipped)", got.Networks)
		}
		if got.Networks["dinernet"].String() != "172.31.1.2" {
			t.Errorf("Networks[dinernet] = %v, want 172.31.1.2", got.Networks["dinernet"])
		}
	})
}

func TestSummaryToContainer(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		got := summaryToContainer(container.Summary{ID: "abc"})
		if got.ID != "abc" || got.Name != "" {
			t.Errorf("ID/Name = %q/%q", got.ID, got.Name)
		}
		if got.Health != HealthNone {
			t.Errorf("Health = %q, want %q (list never populates it)", got.Health, HealthNone)
		}
		if got.Networks == nil {
			t.Errorf("Networks = nil, want an empty non-nil map")
		}
	})

	t.Run("full summary", func(t *testing.T) {
		s := container.Summary{
			ID:     "def456",
			Names:  []string{"/my-container", "/alias"},
			Image:  "alpine:latest",
			State:  "exited",
			Labels: map[string]string{"k": "v"},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{
					"dinernet": {IPAddress: mustAddr(t, "172.31.1.3")},
				},
			},
		}
		got := summaryToContainer(s)
		if got.Name != "my-container" {
			t.Errorf("Name = %q, want my-container (from Names[0], slash stripped)", got.Name)
		}
		if got.State != StateExited {
			t.Errorf("State = %q, want exited", got.State)
		}
		if got.Networks["dinernet"].String() != "172.31.1.3" {
			t.Errorf("Networks[dinernet] = %v, want 172.31.1.3", got.Networks["dinernet"])
		}
	})
}

func TestToEvent(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	m := events.Message{
		Type:   events.Type("container"),
		Action: events.Action("start"),
		Actor: events.Actor{
			ID:         "abc",
			Attributes: map[string]string{"name": "foo"},
		},
		TimeNano: now.UnixNano(),
	}
	got := toEvent(m)
	if got.Type != EventTypeContainer || got.Action != ActionStart || got.ActorID != "abc" {
		t.Errorf("Type/Action/ActorID = %v/%v/%v", got.Type, got.Action, got.ActorID)
	}
	if got.Attributes["name"] != "foo" {
		t.Errorf("Attributes = %v, want name=foo", got.Attributes)
	}
	if !got.Time.Equal(now) {
		t.Errorf("Time = %v, want %v", got.Time, now)
	}
}

func TestWrapErr(t *testing.T) {
	if err := wrapErr("volume", "v", nil); err != nil {
		t.Errorf("wrapErr(nil) = %v, want nil", err)
	}

	notFound := fmt.Errorf("daemon says gone: %w", cerrdefs.ErrNotFound)
	got := wrapErr("volume", "myvol", notFound)
	if !IsNotFound(got) {
		t.Errorf("wrapErr(not-found) = %v, want IsNotFound", got)
	}

	other := errors.New("connection reset")
	got2 := wrapErr("volume", "myvol", other)
	if IsNotFound(got2) {
		t.Errorf("wrapErr(other) = %v, want !IsNotFound", got2)
	}
	if !errors.Is(got2, other) {
		t.Errorf("wrapErr(other) = %v, want it to wrap %v", got2, other)
	}
}

// TestIsNotConnectedError pins BOTH wordings the daemon uses for "this
// container is not on that network", verbatim as captured from a real daemon
// (moby API 1.47, Docker 27.5.1). Matching only the first one is what wedged
// an agent record in StatusDeleting forever: teardown disconnects from the
// dinernet before removing it, so when the dinernet is already gone -- the
// second wording -- the disconnect looked like a real failure and neither
// Delete nor Reconcile would go on to remove the record.
//
// These are unit-testable only here, as literals: the fake client answers
// NetworkDisconnect from its own model of the network and can never reproduce
// the daemon's wording. The DisconnectMissingNetwork conformance case covers
// the real client against a real daemon.
func TestIsNotConnectedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "network exists, container not attached",
			err:  errors.New(`Error response from daemon: container 902e59fd95d9ad06105b846b5f6199d852a93851e8e84252dbe78aeb5d714da7 is not connected to network probe-dc-9191`),
			want: true,
		},
		{
			name: "network itself is gone",
			err:  errors.New(`Error response from daemon: container f84f592978563a200ebd7ed6a91fc796464bbcdcca8e38b3da54d1c98dc95f55 is not connected to the network docker-operator-agent-agt_e1d84b5b-dinernet`),
			want: true,
		},
		{
			name: "a genuine failure is not swallowed",
			err:  errors.New(`Error response from daemon: network mynet not found`),
			want: false,
		},
		{
			name: "a genuine internal error is not swallowed",
			err:  errors.New(`Error response from daemon: failed to disconnect container from network: i/o timeout`),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotConnectedError(tt.err); got != tt.want {
				t.Errorf("isNotConnectedError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
