package dockerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// ErrNotFound reports that the named Docker object does not exist. Every
// Inspect method wraps it, so callers test with IsNotFound rather than
// matching daemon message strings.
//
// The Remove methods deliberately do NOT return it: removing an object that
// is already gone is success (see VolumeRemove).
var ErrNotFound = errors.New("not found")

// IsNotFound reports whether err was caused by a missing Docker object.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Client is the entire Docker surface the rest of docker-operator is allowed
// to use. Nothing outside this package may import the moby SDK; every
// consumer (internal/agent, internal/wsbridge, cmd/docker-operator) depends
// on this interface so it can be unit-tested against dockerclienttest.Fake.
//
// It is composed of the same per-resource interfaces the moby SDK composes
// its own client.APIClient from, so a consumer that only lists resources can
// depend on just VolumeClient+NetworkClient+ContainerClient.
type Client interface {
	Pinger
	VolumeClient
	NetworkClient
	ContainerClient
	ExecClient
	EventClient

	// Close releases the client's idle connections. It does not touch any
	// Docker object.
	Close() error
}

// Pinger reports daemon reachability. cmd/docker-operator calls Ping once at
// startup and exits non-zero on failure: an operator that cannot reach Docker
// has nothing useful to do, and failing here turns every later "connection
// refused" into one legible startup error.
type Pinger interface {
	// Ping round-trips /_ping and negotiates the API version to use for the
	// rest of the process's life.
	Ping(ctx context.Context) error
}

// VolumeClient manages the per-agent named volumes (workspace,
// claude-config, dind-cache).
type VolumeClient interface {
	// VolumeCreate creates a named volume. Creating a volume that already
	// exists is a no-op on the daemon and returns the existing volume.
	VolumeCreate(ctx context.Context, spec VolumeSpec) (Volume, error)

	// VolumeInspect returns one volume by name, or an error satisfying
	// IsNotFound.
	VolumeInspect(ctx context.Context, name string) (Volume, error)

	// VolumeList returns every volume carrying all of labels, sorted by name.
	// A nil or empty labels map lists every volume on the daemon.
	VolumeList(ctx context.Context, labels map[string]string) ([]Volume, error)

	// VolumeRemove removes a volume by name and reports success when the
	// volume is already gone, which is what lets the delete flow double as
	// create-failure rollback. It never forces: Docker refuses to remove a
	// volume a container still references, and that refusal is a real signal
	// that the caller removed things out of order.
	VolumeRemove(ctx context.Context, name string) error
}

// NetworkClient manages the shared proxynet/dbnet and the per-agent dinernet.
type NetworkClient interface {
	// NetworkCreate creates a user-defined bridge network. Subnets are left
	// to Docker's IPAM: per-agent networks are allocated dynamically, which
	// is exactly why NetworkConnect reads the assigned address back.
	NetworkCreate(ctx context.Context, spec NetworkSpec) (Network, error)

	// NetworkInspect returns one network by name, or an error satisfying
	// IsNotFound.
	NetworkInspect(ctx context.Context, name string) (Network, error)

	// NetworkList returns every network carrying all of labels, sorted by name.
	NetworkList(ctx context.Context, labels map[string]string) ([]Network, error)

	// NetworkRemove removes a network by name, reporting success when it is
	// already gone.
	NetworkRemove(ctx context.Context, name string) error

	// NetworkConnect attaches a container to a network and returns the
	// address IPAM assigned it there -- the value #65 writes into the agent
	// container as DEPENDAPROXY_DINERNET_IP.
	//
	// The Docker connect response carries no body, so this performs a second
	// round-trip (ContainerInspect) to read the address back. The container
	// must be RUNNING: a created-but-not-started container has no address yet
	// and this returns an error saying so. networkName must be a name, not an
	// ID, because the address is looked up in the inspect response's
	// name-keyed network map.
	NetworkConnect(ctx context.Context, networkName, containerID string) (netip.Addr, error)

	// NetworkDisconnect detaches a container, reporting success when it is
	// already detached or either object is gone.
	NetworkDisconnect(ctx context.Context, networkName, containerID string) error
}

// ContainerClient manages agent containers and their DinD sidecars.
type ContainerClient interface {
	// ContainerCreate creates a container and returns its ID. Daemon-side
	// creation warnings are discarded: they are advisory (deprecated fields,
	// platform mismatches) and the operator has no action to take on them.
	ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error)

	// ContainerStart starts a created container.
	ContainerStart(ctx context.Context, id string) error

	// ContainerStop stops a container, reporting success when it is already
	// stopped or gone. A timeout <= 0 uses the engine default; otherwise the
	// container is SIGKILLed after timeout.
	ContainerStop(ctx context.Context, id string, timeout time.Duration) error

	// ContainerRemove force-removes a container (killing it if it is still
	// running) and reports success when it is already gone. Force is not
	// optional: every caller -- delete, create-rollback, reconcile -- has
	// already decided the container must go.
	ContainerRemove(ctx context.Context, id string) error

	// ContainerInspect returns the full state of one container by ID or name,
	// or an error satisfying IsNotFound.
	ContainerInspect(ctx context.Context, id string) (Container, error)

	// ContainerList returns every container carrying all of labels -- running
	// or not, since a stopped orphan is still an orphan -- sorted by name.
	//
	// Health and ExitCode are NOT populated: Docker's list endpoint does not
	// return them. Call ContainerInspect when they matter.
	ContainerList(ctx context.Context, labels map[string]string) ([]Container, error)
}

// ExecClient runs processes inside an existing container: the terminal bridge
// (#72), the tmux-session check (#69) and the output endpoint (#70).
type ExecClient interface {
	// ExecCreate prepares a process and returns its exec ID. Stdout and
	// stderr are always attached; without them an exec is unobservable.
	ExecCreate(ctx context.Context, containerID string, spec ExecSpec) (string, error)

	// ExecAttach starts the exec and returns its stream. The caller owns the
	// stream and must Close it; closing ends only the exec, never the
	// container.
	//
	// When the exec was created with TTY the stream is raw PTY bytes. When it
	// was not, the stream is Docker's multiplexed stdout/stderr framing --
	// pass it through DemuxStream.
	ExecAttach(ctx context.Context, execID string) (ExecStream, error)

	// ExecResize resizes an exec's TTY. Called on every resize control frame
	// from the browser terminal.
	ExecResize(ctx context.Context, execID string, size TTYSize) error

	// ExecInspect reports whether an exec is still running and, once it is
	// not, its exit code -- the only way to get an exec's exit status.
	ExecInspect(ctx context.Context, execID string) (ExecStatus, error)
}

// EventClient subscribes to the daemon event stream, which is how
// cmd/docker-operator keeps agent status honest without polling.
type EventClient interface {
	// Events streams events matching filter until ctx is cancelled or the
	// stream breaks. Exactly one of the channels is written before both are
	// closed on termination; a caller that only reads Event will block
	// forever on a broken stream, so read both.
	Events(ctx context.Context, filter EventFilter) (<-chan Event, <-chan error)
}

// ExecStream is a bidirectional attachment to a running exec process: Read
// yields process output, Write feeds its stdin.
type ExecStream interface {
	io.ReadWriteCloser

	// CloseWrite half-closes the write side, sending EOF to the process's
	// stdin without tearing down the read side.
	CloseWrite() error
}

// VolumeSpec describes a volume to create.
type VolumeSpec struct {
	// Name is the volume's daemon-wide unique name.
	Name string
	// Labels are the ai-sandbox.docker-operator/* labels that make the volume
	// discoverable by the reconcile pass.
	Labels map[string]string
}

// Volume is an existing named volume.
type Volume struct {
	Name       string
	Labels     map[string]string
	Mountpoint string
}

// NetworkSpec describes a network to create. The driver is always "bridge".
type NetworkSpec struct {
	Name   string
	Labels map[string]string
}

// Network is an existing user-defined network.
type Network struct {
	ID     string
	Name   string
	Labels map[string]string
}

// MountType selects the kind of storage backing a Mount.
type MountType string

// Supported mount types. Tmpfs is deliberately absent: no consumer needs it.
const (
	MountTypeVolume MountType = "volume"
	MountTypeBind   MountType = "bind"
)

// Mount attaches storage to a path inside a container. Source is a volume
// name for MountTypeVolume and a host path for MountTypeBind.
type Mount struct {
	Type     MountType
	Source   string
	Target   string
	ReadOnly bool
}

// NetworkAttachment joins a container to a network at creation time. Aliases
// are extra DNS names the container answers to on that network -- how the
// per-agent DinD sidecar keeps answering to "docker" so the reused
// entrypoint.sh and use-docker skill's DOCKER_HOST=tcp://docker:2375 works
// unchanged.
type NetworkAttachment struct {
	Name    string
	Aliases []string
}

// Healthcheck is a container-level health probe. The DinD sidecar needs one
// declared at create time (the docker:dind image ships none) so #65 can wait
// for it to become healthy before starting the agent container.
type Healthcheck struct {
	// Test is Docker's healthcheck form, e.g. {"CMD", "docker", "info"}.
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// ContainerSpec describes a container to create. It covers exactly what the
// compose stack's `docker` and `claude` services configure, since those are
// what the per-agent DinD sidecar and agent container are templated from.
type ContainerSpec struct {
	Name       string
	Image      string
	Entrypoint []string // overrides the image ENTRYPOINT (dind-init.sh)
	Cmd        []string // overrides the image CMD (tmux-boot.sh, dockerd args)

	// Env is rendered to KEY=VALUE in sorted order, so an identical spec
	// always produces an identical container config.
	Env    map[string]string
	Labels map[string]string

	Mounts   []Mount
	Networks []NetworkAttachment

	// Runtime is the OCI runtime, e.g. "sysbox-runc" for the DinD sidecar.
	// Empty uses the daemon default.
	Runtime string

	TTY       bool
	OpenStdin bool

	Healthcheck *Healthcheck
}

// ContainerState is the daemon's lifecycle state for a container.
type ContainerState string

// Container lifecycle states, as reported by the daemon.
const (
	StateCreated    ContainerState = "created"
	StateRunning    ContainerState = "running"
	StatePaused     ContainerState = "paused"
	StateRestarting ContainerState = "restarting"
	StateRemoving   ContainerState = "removing"
	StateExited     ContainerState = "exited"
	StateDead       ContainerState = "dead"
)

// HealthStatus is the result of a container's healthcheck.
type HealthStatus string

// Health states. HealthNone means the container declares no healthcheck.
const (
	HealthNone      HealthStatus = "none"
	HealthStarting  HealthStatus = "starting"
	HealthHealthy   HealthStatus = "healthy"
	HealthUnhealthy HealthStatus = "unhealthy"
)

// Container is an existing container. Networks maps network name to the
// address assigned there; an entry may hold the zero Addr for a container
// that has not started yet.
type Container struct {
	ID       string
	Name     string
	Image    string
	State    ContainerState
	Health   HealthStatus
	ExitCode int
	Labels   map[string]string
	Networks map[string]netip.Addr
}

// ExecSpec describes a process to run inside a container.
type ExecSpec struct {
	Cmd        []string
	Env        map[string]string
	User       string
	WorkingDir string

	// TTY allocates a pseudo-terminal. True for the tmux attach the terminal
	// bridge runs; false for one-shot commands whose output is demultiplexed.
	TTY bool

	// Stdin attaches the process's stdin to the ExecStream's write side.
	Stdin bool
}

// TTYSize is a terminal geometry in character cells. uint16 mirrors the
// kernel's struct winsize and makes every conversion in this package a
// widening one.
type TTYSize struct {
	Cols uint16
	Rows uint16
}

// ExecStatus is the observable state of an exec process. ExitCode is
// meaningful only once Running is false.
type ExecStatus struct {
	Running  bool
	ExitCode int
}

// EventType is the kind of object an event concerns.
type EventType string

// Event object types the operator cares about.
const (
	EventTypeContainer EventType = "container"
	EventTypeNetwork   EventType = "network"
	EventTypeVolume    EventType = "volume"
)

// EventAction is what happened to the object. The constants cover the actions
// the status-sync goroutine reacts to; Action carries the daemon's raw string
// for everything else (including "health_status: healthy" and friends).
type EventAction string

// Container lifecycle actions.
const (
	ActionCreate  EventAction = "create"
	ActionStart   EventAction = "start"
	ActionStop    EventAction = "stop"
	ActionKill    EventAction = "kill"
	ActionDie     EventAction = "die"
	ActionOOM     EventAction = "oom"
	ActionDestroy EventAction = "destroy"
)

// EventFilter narrows the event stream. Both fields are applied by the
// daemon, not client-side: an unfiltered stream on a busy host is a firehose.
type EventFilter struct {
	// Types restricts the stream to these object types.
	Types []EventType
	// Labels restricts it to objects carrying all of these labels.
	Labels map[string]string
}

// Event is one daemon event. Attributes carries the object's labels plus
// daemon-supplied keys such as "name" and "exitCode".
type Event struct {
	Type       EventType
	Action     EventAction
	ActorID    string
	Attributes map[string]string
	Time       time.Time
}

// Docker is the moby-SDK-backed Client. It is the only type in the repository
// that holds a moby client.
type Docker struct {
	api *client.Client
}

var _ Client = (*Docker)(nil)

// NewFromEnv builds a Docker client from the standard Docker environment
// variables (DOCKER_HOST, DOCKER_API_VERSION, DOCKER_CERT_PATH,
// DOCKER_TLS_VERIFY), falling back to the platform's default socket.
//
// It performs no I/O: nothing is dialled until the first call, and the API
// version is negotiated on that first call. Call Ping to fail fast instead.
//
// There is no host-override constructor because internal/config carries no
// DockerHost field; add client.WithHost here if that changes.
func NewFromEnv() (*Docker, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("building docker client from the environment: %w", err)
	}
	return &Docker{api: api}, nil
}

// Close releases idle connections.
func (d *Docker) Close() error { return d.api.Close() }

// Ping verifies the daemon is reachable and negotiates the API version.
func (d *Docker) Ping(ctx context.Context) error {
	if _, err := d.api.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return fmt.Errorf("pinging the docker daemon at %s: %w", d.api.DaemonHost(), err)
	}
	return nil
}

// VolumeCreate creates a named volume.
func (d *Docker) VolumeCreate(ctx context.Context, spec VolumeSpec) (Volume, error) {
	res, err := d.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: spec.Name, Labels: spec.Labels})
	if err != nil {
		return Volume{}, fmt.Errorf("creating volume %q: %w", spec.Name, err)
	}
	return toVolume(res.Volume), nil
}

// VolumeInspect returns one volume by name.
func (d *Docker) VolumeInspect(ctx context.Context, name string) (Volume, error) {
	res, err := d.api.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return Volume{}, wrapErr("volume", name, err)
	}
	return toVolume(res.Volume), nil
}

// VolumeList returns volumes carrying every label in labels.
func (d *Docker) VolumeList(ctx context.Context, labels map[string]string) ([]Volume, error) {
	res, err := d.api.VolumeList(ctx, client.VolumeListOptions{Filters: labelFilter(labels)})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, toVolume(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// VolumeRemove removes a volume, ignoring a volume that is already gone.
func (d *Docker) VolumeRemove(ctx context.Context, name string) error {
	if _, err := d.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("removing volume %q: %w", name, err)
	}
	return nil
}

// NetworkCreate creates a user-defined bridge network.
func (d *Docker) NetworkCreate(ctx context.Context, spec NetworkSpec) (Network, error) {
	res, err := d.api.NetworkCreate(ctx, spec.Name, client.NetworkCreateOptions{Driver: "bridge", Labels: spec.Labels})
	if err != nil {
		return Network{}, fmt.Errorf("creating network %q: %w", spec.Name, err)
	}
	return Network{ID: res.ID, Name: spec.Name, Labels: copyLabels(spec.Labels)}, nil
}

// NetworkInspect returns one network by name.
func (d *Docker) NetworkInspect(ctx context.Context, name string) (Network, error) {
	res, err := d.api.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return Network{}, wrapErr("network", name, err)
	}
	return Network{ID: res.Network.ID, Name: res.Network.Name, Labels: copyLabels(res.Network.Labels)}, nil
}

// NetworkList returns networks carrying every label in labels.
func (d *Docker) NetworkList(ctx context.Context, labels map[string]string) ([]Network, error) {
	res, err := d.api.NetworkList(ctx, client.NetworkListOptions{Filters: labelFilter(labels)})
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	out := make([]Network, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, Network{ID: n.ID, Name: n.Name, Labels: copyLabels(n.Labels)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// NetworkRemove removes a network, ignoring one that is already gone.
func (d *Docker) NetworkRemove(ctx context.Context, name string) error {
	if _, err := d.api.NetworkRemove(ctx, name, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("removing network %q: %w", name, err)
	}
	return nil
}

// NetworkConnect attaches a running container to a network and reads back the
// address IPAM assigned to it there.
func (d *Docker) NetworkConnect(ctx context.Context, networkName, containerID string) (netip.Addr, error) {
	if _, err := d.api.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{Container: containerID}); err != nil {
		return netip.Addr{}, fmt.Errorf("connecting container %q to network %q: %w", containerID, networkName, err)
	}
	c, err := d.ContainerInspect(ctx, containerID)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reading back the address of %q on %q: %w", containerID, networkName, err)
	}
	addr, ok := c.Networks[networkName]
	if !ok || !addr.IsValid() {
		return netip.Addr{}, fmt.Errorf("container %q joined network %q but the daemon reported no address for it (is the container running?)", containerID, networkName)
	}
	return addr, nil
}

// NetworkDisconnect detaches a container, ignoring one already detached.
func (d *Docker) NetworkDisconnect(ctx context.Context, networkName, containerID string) error {
	_, err := d.api.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{Container: containerID, Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) && !isNotConnectedError(err) {
		return fmt.Errorf("disconnecting container %q from network %q: %w", containerID, networkName, err)
	}
	return nil
}

// isNotConnectedError reports whether err is the daemon's response to
// disconnecting a container that is already not connected to the network.
// Verified against a real daemon (moby API 1.47): the daemon answers this
// case with a 500, which the SDK maps to cerrdefs.ErrInternal rather than a
// 404/409 -- indistinguishable from a genuine internal error by class alone,
// so this checks the daemon's message text instead. If the daemon's wording
// ever changes, the worst case is NetworkDisconnect stops being idempotent
// again, not a false "success" on a real failure (the message is specific
// enough that no other error is expected to contain it).
func isNotConnectedError(err error) bool {
	return strings.Contains(err.Error(), "is not connected to network")
}

// ContainerCreate creates a container and returns its ID.
func (d *Docker) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	res, err := d.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:       spec.Image,
			Entrypoint:  spec.Entrypoint,
			Cmd:         spec.Cmd,
			Env:         envSlice(spec.Env),
			Labels:      copyLabels(spec.Labels),
			Tty:         spec.TTY,
			OpenStdin:   spec.OpenStdin,
			Healthcheck: toHealthConfig(spec.Healthcheck),
		},
		HostConfig: &container.HostConfig{
			Runtime: spec.Runtime,
			Mounts:  toMounts(spec.Mounts),
		},
		NetworkingConfig: toNetworkingConfig(spec.Networks),
	})
	if err != nil {
		return "", fmt.Errorf("creating container %q: %w", spec.Name, err)
	}
	return res.ID, nil
}

// ContainerStart starts a created container.
func (d *Docker) ContainerStart(ctx context.Context, id string) error {
	if _, err := d.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return wrapErr("container", id, err)
	}
	return nil
}

// ContainerStop stops a container.
func (d *Docker) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	opts := client.ContainerStopOptions{}
	if timeout > 0 {
		secs := int(timeout.Seconds())
		opts.Timeout = &secs
	}
	if _, err := d.api.ContainerStop(ctx, id, opts); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("stopping container %q: %w", id, err)
	}
	return nil
}

// ContainerRemove force-removes a container, ignoring one already gone.
func (d *Docker) ContainerRemove(ctx context.Context, id string) error {
	_, err := d.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("removing container %q: %w", id, err)
	}
	return nil
}

// ContainerInspect returns the full state of one container.
func (d *Docker) ContainerInspect(ctx context.Context, id string) (Container, error) {
	res, err := d.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Container{}, wrapErr("container", id, err)
	}
	return toContainer(res.Container), nil
}

// ContainerList returns containers carrying every label in labels.
func (d *Docker) ContainerList(ctx context.Context, labels map[string]string) ([]Container, error) {
	res, err := d.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: labelFilter(labels)})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, summaryToContainer(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ExecCreate prepares a process to run inside a container.
func (d *Docker) ExecCreate(ctx context.Context, containerID string, spec ExecSpec) (string, error) {
	res, err := d.api.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          spec.Cmd,
		Env:          envSlice(spec.Env),
		User:         spec.User,
		WorkingDir:   spec.WorkingDir,
		TTY:          spec.TTY,
		AttachStdin:  spec.Stdin,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", wrapErr("container", containerID, err)
	}
	return res.ID, nil
}

// ExecAttach starts the exec and returns its stream.
func (d *Docker) ExecAttach(ctx context.Context, execID string) (ExecStream, error) {
	res, err := d.api.ExecAttach(ctx, execID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return nil, wrapErr("exec", execID, err)
	}
	return &hijackedStream{resp: res.HijackedResponse}, nil
}

// ExecResize resizes an exec TTY.
func (d *Docker) ExecResize(ctx context.Context, execID string, size TTYSize) error {
	_, err := d.api.ExecResize(ctx, execID, client.ExecResizeOptions{Height: uint(size.Rows), Width: uint(size.Cols)})
	if err != nil {
		return wrapErr("exec", execID, err)
	}
	return nil
}

// ExecInspect reports whether an exec is still running and its exit code.
func (d *Docker) ExecInspect(ctx context.Context, execID string) (ExecStatus, error) {
	res, err := d.api.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return ExecStatus{}, wrapErr("exec", execID, err)
	}
	return ExecStatus{Running: res.Running, ExitCode: res.ExitCode}, nil
}

// Events streams daemon events matching filter.
func (d *Docker) Events(ctx context.Context, filter EventFilter) (<-chan Event, <-chan error) {
	res := d.api.Events(ctx, client.EventsListOptions{Filters: eventFilter(filter)})
	out := make(chan Event)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case err, ok := <-res.Err:
				if ok && err != nil {
					errs <- err
				}
				return
			case m, ok := <-res.Messages:
				if !ok {
					return
				}
				select {
				case out <- toEvent(m):
				case <-ctx.Done():
					errs <- ctx.Err()
					return
				}
			}
		}
	}()
	return out, errs
}

// DemuxStream copies a non-TTY exec stream from src, writing the process's
// stdout to stdout and its stderr to stderr. A TTY exec stream is raw and
// must not be passed here.
//
// It exists so consumers can read a one-shot exec's output (the tmux
// has-session check in #69, the output endpoint in #70) without importing the
// moby SDK themselves.
func DemuxStream(stdout, stderr io.Writer, src io.Reader) error {
	_, err := stdcopy.StdCopy(stdout, stderr, src)
	return err
}

// hijackedStream adapts the SDK's hijacked connection to ExecStream: the SDK
// splits it into a buffered Reader and a raw Conn, and its own Close returns
// no error.
type hijackedStream struct {
	resp client.HijackedResponse
}

func (s *hijackedStream) Read(p []byte) (int, error)  { return s.resp.Reader.Read(p) }
func (s *hijackedStream) Write(p []byte) (int, error) { return s.resp.Conn.Write(p) }
func (s *hijackedStream) Close() error                { return s.resp.Conn.Close() }
func (s *hijackedStream) CloseWrite() error           { return s.resp.CloseWrite() }

// wrapErr names the object in the error and normalises the SDK's not-found
// errors to ErrNotFound, so callers never string-match daemon messages.
func wrapErr(kind, name string, err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%s %q: %w", kind, name, ErrNotFound)
	}
	return fmt.Errorf("%s %q: %w", kind, name, err)
}

func labelFilter(labels map[string]string) client.Filters {
	if len(labels) == 0 {
		return nil
	}
	f := make(client.Filters)
	for k, v := range labels {
		f.Add("label", k+"="+v)
	}
	return f
}

func eventFilter(filter EventFilter) client.Filters {
	if len(filter.Types) == 0 && len(filter.Labels) == 0 {
		return nil
	}
	f := labelFilter(filter.Labels)
	if f == nil {
		f = make(client.Filters)
	}
	for _, t := range filter.Types {
		f.Add("type", string(t))
	}
	return f
}

// envSlice renders env as sorted KEY=VALUE pairs so an identical spec always
// produces an identical container config.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toMounts(in []Mount) []mount.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]mount.Mount, 0, len(in))
	for _, m := range in {
		out = append(out, mount.Mount{
			Type:     mount.Type(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func toNetworkingConfig(in []NetworkAttachment) *network.NetworkingConfig {
	if len(in) == 0 {
		return nil
	}
	eps := make(map[string]*network.EndpointSettings, len(in))
	for _, n := range in {
		eps[n.Name] = &network.EndpointSettings{Aliases: n.Aliases}
	}
	return &network.NetworkingConfig{EndpointsConfig: eps}
}

func toHealthConfig(h *Healthcheck) *container.HealthConfig {
	if h == nil {
		return nil
	}
	return &container.HealthConfig{
		Test:        h.Test,
		Interval:    h.Interval,
		Timeout:     h.Timeout,
		Retries:     h.Retries,
		StartPeriod: h.StartPeriod,
	}
}

func toVolume(v volume.Volume) Volume {
	return Volume{Name: v.Name, Labels: copyLabels(v.Labels), Mountpoint: v.Mountpoint}
}

func toContainer(c container.InspectResponse) Container {
	out := Container{
		ID:       c.ID,
		Name:     strings.TrimPrefix(c.Name, "/"),
		Image:    c.Image,
		Health:   HealthNone,
		Networks: map[string]netip.Addr{},
	}
	if c.Config != nil {
		out.Image = c.Config.Image
		out.Labels = copyLabels(c.Config.Labels)
	}
	if c.State != nil {
		out.State = ContainerState(c.State.Status)
		out.ExitCode = c.State.ExitCode
		if c.State.Health != nil {
			out.Health = HealthStatus(c.State.Health.Status)
		}
	}
	if c.NetworkSettings != nil {
		for name, ep := range c.NetworkSettings.Networks {
			if ep != nil {
				out.Networks[name] = ep.IPAddress
			}
		}
	}
	return out
}

func summaryToContainer(s container.Summary) Container {
	out := Container{
		ID:       s.ID,
		Image:    s.Image,
		State:    ContainerState(s.State),
		Health:   HealthNone,
		Labels:   copyLabels(s.Labels),
		Networks: map[string]netip.Addr{},
	}
	if len(s.Names) > 0 {
		out.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	if s.NetworkSettings != nil {
		for name, ep := range s.NetworkSettings.Networks {
			if ep != nil {
				out.Networks[name] = ep.IPAddress
			}
		}
	}
	return out
}

func toEvent(m events.Message) Event {
	return Event{
		Type:       EventType(m.Type),
		Action:     EventAction(m.Action),
		ActorID:    m.Actor.ID,
		Attributes: copyLabels(m.Actor.Attributes),
		Time:       time.Unix(0, m.TimeNano),
	}
}
