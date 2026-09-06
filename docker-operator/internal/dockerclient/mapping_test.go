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
	}
	got := toMounts(in)
	if len(got) != 4 {
		t.Fatalf("len(toMounts) = %d, want 4", len(got))
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
