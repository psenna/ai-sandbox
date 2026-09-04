package dockerclient_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
)

// factory names one dockerclient.Client implementation and how to build a
// fresh instance of it for a single test, skipping gracefully (never
// t.Fatal) when that implementation is unavailable.
type factory struct {
	name   string
	prefix string
	new    func(t *testing.T) dockerclient.Client
}

func factories() []factory {
	return []factory{
		{name: "fake", prefix: "fake", new: func(t *testing.T) dockerclient.Client {
			return dockerclienttest.New()
		}},
		{name: "docker", prefix: "dct", new: newRealDockerClient},
	}
}

// newRealDockerClient builds a Docker client against the daemon named by the
// standard Docker environment variables and skips the test if it cannot
// reach one -- this environment's DOCKER_HOST=tcp://docker:2375 is expected
// to be reachable (see CLAUDE.md), so this leg normally runs for real.
func newRealDockerClient(t *testing.T) dockerclient.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("real docker daemon leg skipped: -short")
	}
	c, err := dockerclient.NewFromEnv()
	if err != nil {
		t.Skipf("building docker client from environment: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// uniqueName returns a Docker-name-safe, collision-resistant name for a test
// object: <factory prefix>-<test name>-<nanosecond timestamp>.
func uniqueName(f factory, t *testing.T) string {
	raw := f.prefix + "-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return sanitizeName(raw)
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for len(out) > 0 && !isAlnum(out[0]) {
		out = out[1:]
	}
	if out == "" {
		out = "x"
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// isImageNotFoundErr reports whether err looks like the daemon rejecting
// ContainerCreate because the image isn't present locally -- this client has
// no ImagePull (out of scope for #63; see #65), so a maintainer running this
// suite for the first time needs to pull it themselves.
func isImageNotFoundErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such image") || (strings.Contains(s, "not found") && strings.Contains(s, "image"))
}

func containsVolumeName(list []dockerclient.Volume, name string) bool {
	for _, v := range list {
		if v.Name == name {
			return true
		}
	}
	return false
}

func containsNetworkName(list []dockerclient.Network, name string) bool {
	for _, n := range list {
		if n.Name == name {
			return true
		}
	}
	return false
}

func containsContainerID(list []dockerclient.Container, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

type conformanceCase struct {
	name string
	run  func(t *testing.T, f factory, c dockerclient.Client)
}

// TestConformance runs an identical behavioral contract against every
// dockerclient.Client implementation: the in-memory fake and (when a daemon
// is reachable) the real moby-SDK-backed client.
func TestConformance(t *testing.T) {
	for _, f := range factories() {
		t.Run(f.name, func(t *testing.T) {
			for _, cs := range conformanceCases {
				t.Run(cs.name, func(t *testing.T) {
					c := f.new(t)
					cs.run(t, f, c)
				})
			}
		})
	}
}

var conformanceCases = []conformanceCase{
	{name: "VolumeLifecycle", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		name := uniqueName(f, t)
		labels := map[string]string{"dockerclienttest.probe": name}

		v, err := c.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: name, Labels: labels})
		if err != nil {
			t.Fatalf("VolumeCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.VolumeRemove(context.Background(), name) })
		if v.Name != name {
			t.Errorf("Volume.Name = %q, want %q", v.Name, name)
		}
		if v.Labels["dockerclienttest.probe"] != name {
			t.Errorf("Volume.Labels = %v", v.Labels)
		}

		got, err := c.VolumeInspect(ctx, name)
		if err != nil {
			t.Fatalf("VolumeInspect: %v", err)
		}
		if got.Name != name || got.Labels["dockerclienttest.probe"] != name {
			t.Errorf("VolumeInspect = %+v", got)
		}

		list, err := c.VolumeList(ctx, labels)
		if err != nil {
			t.Fatalf("VolumeList: %v", err)
		}
		if len(list) != 1 || list[0].Name != name {
			t.Errorf("VolumeList(by label) = %v, want exactly [%q]", list, name)
		}

		if err := c.VolumeRemove(ctx, name); err != nil {
			t.Fatalf("VolumeRemove: %v", err)
		}
		if _, err := c.VolumeInspect(ctx, name); !dockerclient.IsNotFound(err) {
			t.Errorf("VolumeInspect after remove: err = %v, want IsNotFound", err)
		}
		if err := c.VolumeRemove(ctx, name); err != nil {
			t.Errorf("VolumeRemove (again) = %v, want nil", err)
		}
	}},

	{name: "NetworkLifecycle", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		name := uniqueName(f, t)
		labels := map[string]string{"dockerclienttest.probe": name}

		n, err := c.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: name, Labels: labels})
		if err != nil {
			t.Fatalf("NetworkCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.NetworkRemove(context.Background(), name) })
		if n.Name != name {
			t.Errorf("Network.Name = %q, want %q", n.Name, name)
		}
		if n.Labels["dockerclienttest.probe"] != name {
			t.Errorf("Network.Labels = %v", n.Labels)
		}

		got, err := c.NetworkInspect(ctx, name)
		if err != nil {
			t.Fatalf("NetworkInspect: %v", err)
		}
		if got.Name != name {
			t.Errorf("NetworkInspect.Name = %q, want %q", got.Name, name)
		}

		list, err := c.NetworkList(ctx, labels)
		if err != nil {
			t.Fatalf("NetworkList: %v", err)
		}
		if !containsNetworkName(list, name) {
			t.Errorf("NetworkList(by label) did not contain %q (got %v)", name, list)
		}

		if err := c.NetworkRemove(ctx, name); err != nil {
			t.Fatalf("NetworkRemove: %v", err)
		}
		if _, err := c.NetworkInspect(ctx, name); !dockerclient.IsNotFound(err) {
			t.Errorf("NetworkInspect after remove: err = %v, want IsNotFound", err)
		}
		if err := c.NetworkRemove(ctx, name); err != nil {
			t.Errorf("NetworkRemove (again) = %v, want nil", err)
		}
	}},

	{name: "ContainerLifecycle", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		volName := uniqueName(f, t) + "-vol"
		netName := uniqueName(f, t) + "-net"
		ctrName := uniqueName(f, t) + "-ctr"
		labels := map[string]string{"dockerclienttest.probe": ctrName}

		if _, err := c.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: volName}); err != nil {
			t.Fatalf("VolumeCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.VolumeRemove(context.Background(), volName) })

		if _, err := c.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: netName}); err != nil {
			t.Fatalf("NetworkCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.NetworkRemove(context.Background(), netName) })

		spec := dockerclient.ContainerSpec{
			Name:     ctrName,
			Image:    "alpine:latest",
			Cmd:      []string{"sleep", "300"},
			Labels:   labels,
			Mounts:   []dockerclient.Mount{{Type: dockerclient.MountTypeVolume, Source: volName, Target: "/data"}},
			Networks: []dockerclient.NetworkAttachment{{Name: netName}},
		}
		id, err := c.ContainerCreate(ctx, spec)
		if err != nil {
			if isImageNotFoundErr(err) {
				t.Skipf("alpine:latest not available on the daemon and this client cannot pull images (by design, #63) -- run `docker pull alpine:latest` first: %v", err)
			}
			t.Fatalf("ContainerCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.ContainerRemove(context.Background(), id) })

		got, err := c.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect (created): %v", err)
		}
		if got.State != dockerclient.StateCreated {
			t.Errorf("State = %q, want %q", got.State, dockerclient.StateCreated)
		}

		if err := c.ContainerStart(ctx, id); err != nil {
			t.Fatalf("ContainerStart: %v", err)
		}
		got, err = c.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect (running): %v", err)
		}
		if got.State != dockerclient.StateRunning {
			t.Errorf("State = %q, want %q", got.State, dockerclient.StateRunning)
		}
		if addr, ok := got.Networks[netName]; !ok || !addr.IsValid() {
			t.Errorf("Networks[%q] = %v, want a valid address", netName, got.Networks[netName])
		}

		list, err := c.ContainerList(ctx, labels)
		if err != nil {
			t.Fatalf("ContainerList: %v", err)
		}
		if !containsContainerID(list, id) {
			t.Errorf("ContainerList(by label) did not contain %q", id)
		}

		if err := c.ContainerStop(ctx, id, 5*time.Second); err != nil {
			t.Fatalf("ContainerStop: %v", err)
		}
		if err := c.ContainerRemove(ctx, id); err != nil {
			t.Fatalf("ContainerRemove: %v", err)
		}
		if _, err := c.ContainerInspect(ctx, id); !dockerclient.IsNotFound(err) {
			t.Errorf("ContainerInspect after remove: err = %v, want IsNotFound", err)
		}
		if err := c.ContainerRemove(ctx, id); err != nil {
			t.Errorf("ContainerRemove (again) = %v, want nil", err)
		}
	}},

	{name: "ConnectReadsBackAddress", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		netName := uniqueName(f, t) + "-net"
		ctrName := uniqueName(f, t) + "-ctr"

		if _, err := c.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: netName}); err != nil {
			t.Fatalf("NetworkCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.NetworkRemove(context.Background(), netName) })

		id, err := c.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: ctrName, Image: "alpine:latest", Cmd: []string{"sleep", "300"}})
		if err != nil {
			if isImageNotFoundErr(err) {
				t.Skipf("alpine:latest not available -- run `docker pull alpine:latest` first: %v", err)
			}
			t.Fatalf("ContainerCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.ContainerRemove(context.Background(), id) })
		if err := c.ContainerStart(ctx, id); err != nil {
			t.Fatalf("ContainerStart: %v", err)
		}

		addr, err := c.NetworkConnect(ctx, netName, id)
		if err != nil {
			t.Fatalf("NetworkConnect: %v", err)
		}
		if !addr.IsValid() {
			t.Fatalf("NetworkConnect returned an invalid address")
		}

		got, err := c.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if got.Networks[netName] != addr {
			t.Errorf("ContainerInspect().Networks[%q] = %v, want %v", netName, got.Networks[netName], addr)
		}

		if err := c.NetworkDisconnect(ctx, netName, id); err != nil {
			t.Fatalf("NetworkDisconnect: %v", err)
		}
		if err := c.NetworkDisconnect(ctx, netName, id); err != nil {
			t.Errorf("NetworkDisconnect (again) = %v, want nil", err)
		}
	}},

	{name: "ConnectStoppedContainerFails", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		netName := uniqueName(f, t) + "-net"
		ctrName := uniqueName(f, t) + "-ctr"

		if _, err := c.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: netName}); err != nil {
			t.Fatalf("NetworkCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.NetworkRemove(context.Background(), netName) })

		id, err := c.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: ctrName, Image: "alpine:latest", Cmd: []string{"sleep", "300"}})
		if err != nil {
			if isImageNotFoundErr(err) {
				t.Skipf("alpine:latest not available -- run `docker pull alpine:latest` first: %v", err)
			}
			t.Fatalf("ContainerCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.ContainerRemove(context.Background(), id) })
		// Deliberately not started.

		if _, err := c.NetworkConnect(ctx, netName, id); err == nil {
			t.Errorf("NetworkConnect(stopped container) = nil error, want an error")
		}
	}},

	{name: "ListIsLabelScoped", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		name := uniqueName(f, t)

		if _, err := c.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: name}); err != nil {
			t.Fatalf("VolumeCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.VolumeRemove(context.Background(), name) })

		list, err := c.VolumeList(ctx, map[string]string{"dockerclienttest.probe": name})
		if err != nil {
			t.Fatalf("VolumeList: %v", err)
		}
		if containsVolumeName(list, name) {
			t.Errorf("VolumeList(unrelated label) contains %q, want it excluded", name)
		}
	}},

	{name: "InspectMissingIsNotFound", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		missing := uniqueName(f, t) + "-missing"
		if _, err := c.VolumeInspect(ctx, missing); !dockerclient.IsNotFound(err) {
			t.Errorf("VolumeInspect(missing) err = %v, want IsNotFound", err)
		}
		if _, err := c.NetworkInspect(ctx, missing); !dockerclient.IsNotFound(err) {
			t.Errorf("NetworkInspect(missing) err = %v, want IsNotFound", err)
		}
		if _, err := c.ContainerInspect(ctx, missing); !dockerclient.IsNotFound(err) {
			t.Errorf("ContainerInspect(missing) err = %v, want IsNotFound", err)
		}
	}},
}
