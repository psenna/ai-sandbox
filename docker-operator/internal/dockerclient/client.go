package dockerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
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
	ImageClient

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

	// VolumeUsage returns the disk usage of every volume on the daemon, sorted
	// by name, in ONE GET /system/df round-trip with {Volumes: true, Verbose:
	// true}. One call covers every agent's three volumes at once; index the
	// result by Name. It is deliberately NOT folded into VolumeInspect/List,
	// where the daemon's UsageData is always absent -- see VolumeUsage's own
	// doc comment.
	VolumeUsage(ctx context.Context) ([]VolumeUsage, error)
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

	// ContainerStats returns one sample of the container's current resource
	// use: CPU percentage, memory used/limit and process count. It is
	// one-shot with the daemon's previous sample included (the SDK's only
	// one-shot form in client v0.5.1) and costs ~1s of daemon-side sampling
	// -- there is no cheaper honest CPU number. Only a RUNNING container has
	// stats: a stopped one yields an error, not zeros.
	ContainerStats(ctx context.Context, id string) (Stats, error)
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

// ImageClient manages the images agent containers are built from: ensuring
// they are present, discovering which tags the daemon holds, and removing
// tags the operator no longer needs. It deliberately does not build, tag,
// push, or prune the whole image store.
type ImageClient interface {
	// ImageInspect returns one image by reference (name:tag or ID), or an
	// error satisfying IsNotFound when the daemon holds no such image. It
	// never contacts a registry.
	ImageInspect(ctx context.Context, ref string) (Image, error)

	// ImagePull pulls ref and blocks until the pull has finished, discarding
	// the progress stream. Pulling an image that is already present is a
	// cheap no-op on the daemon.
	//
	// No registry credentials are ever sent: docker-operator's images are
	// either public (docker:27-dind) or built locally (the agent image), and
	// a registry credential the operator could leak would buy nothing.
	ImagePull(ctx context.Context, ref string) error

	// ImageList returns every image on the daemon whose repository is repo
	// (any tag). An empty repo lists every image. It never contacts a
	// registry.
	//
	// "Repository" means the reference with no tag and no digest, e.g.
	// "ghcr.io/psenna/ai-sandbox-agent". repo may be given in EITHER the
	// normalized form ("docker.io/myorg/agent-image") or the familiar form
	// ("myorg/agent-image"); it is canonicalized to the familiar form, which
	// is the form the daemon's images/json API speaks -- see FamiliarRepo.
	// Entries the daemon returns that carry no tag inside repo (dangling
	// "<none>:<none>" layers, other repositories a loose daemon-side filter
	// let through) are dropped, so every returned Image has at least one
	// RepoTags entry inside repo.
	ImageList(ctx context.Context, repo string) ([]Image, error)

	// ImageRemove removes ref from the daemon. When ref is a repo:tag whose
	// image carries other tags, only the TAG is removed (the daemon
	// "untags") and the image stays; when it is the image's last tag, the
	// image and its now-unreferenced parent layers go too. Removing an image
	// that is already gone is success, like every Remove method here.
	//
	// It never forces: the daemon refuses to remove an image a container
	// still references, and that refusal is a real signal that the caller
	// removed things out of order -- the same stance as VolumeRemove.
	ImageRemove(ctx context.Context, ref string) error
}

// Image is an image present on the daemon.
type Image struct {
	ID       string
	RepoTags []string
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

// VolumeUsage is the disk usage of one named volume, as ONLY the daemon's
// GET /system/df endpoint reports it (the moby type's own doc: "used by the
// GET /system/df endpoint, and omitted in other endpoints" -- so
// VolumeInspect/VolumeList never carry it). It is a parallel type rather than a
// field on Volume for exactly that reason: Volume is returned by Create,
// Inspect AND List, where UsageData is always absent, so a usage field there
// would be misleading on three of four paths. The Stats type is the same
// precedent. Size is the recursive content size of the volume's mountpoint in
// bytes: 0 is a REAL reading (a never-written "local"-driver volume reports 0
// -- verified against Docker 27.5.1), a NEGATIVE value (or the daemon omitting
// UsageData) means the daemon could not compute it and must not be summed or
// rendered.
type VolumeUsage struct {
	Name string
	Size int64
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

	// Subpath mounts only this path from inside the volume, instead of the
	// whole volume. It is relative to the volume root and carries no leading
	// slash. The daemon requires the subpath to already exist inside the
	// volume (internal/agent pre-creates it through internal/filestore before
	// the container is created). It is only meaningful for MountTypeVolume.
	//
	// Requires Docker Engine >= 26.0 (API v1.45): an older daemon SILENTLY
	// IGNORES it and mounts the whole volume, which for the per-agent
	// file-store mount would let every agent see every other agent's files.
	// Empty leaves the mount unchanged (the whole volume, the pre-Subpath
	// behaviour).
	Subpath string
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

// Stats is one sample of a running container's resource usage, the moby-free
// mirror of the daemon's stats document. It is always a REAL sample: the
// daemon takes two samples one second apart (IncludePreviousSample), which
// is the only way a CPU percentage exists -- a single sample has nothing to
// delta against. That second of daemon-side sampling is the honest price of
// the number and is paid on every call.
type Stats struct {
	// Read is when the daemon took this (the second, i.e. current) sample.
	Read time.Time
	// CPUPercent is CPU use over the ~1s sampling window as a percentage of
	// ALL cpus: 100.0 is one fully-used cpu, 400.0 is four on an 8-cpu host.
	// Computed HERE rather than handed up as raw counters: the calculation
	// needs BOTH samples and guards the two known quirks (OnlineCPUs can be
	// 0, and SystemUsage can go backwards across a container restart). Zero
	// when the deltas are degenerate (just started, counters rewound, no
	// predecessor).
	CPUPercent float64
	// MemoryUsed is resident memory: the daemon's usage MINUS the page cache
	// the kernel can reclaim (memory.stats' inactive_file) -- the same
	// subtraction `docker stats` performs. Raw usage when the key is absent.
	MemoryUsed uint64
	// MemoryLimit is the cgroup limit. With no limit configured (this
	// operator configures none) the daemon reports the host's total memory,
	// so this reads as "against host memory".
	MemoryLimit uint64
	// Pids is the number of processes currently in the container. The
	// daemon's pids LIMIT is deliberately not mirrored: it is uint64-max when
	// unlimited, which carries no information.
	Pids uint64
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

// VolumeUsage returns the disk usage of EVERY volume on the daemon, sorted by
// name, in one GET /system/df round-trip.
//
// Verbose is REQUIRED on API >= 1.52 (the per-volume Items are the only place
// UsageData appears, and the SDK populates them only when verbose) and ignored
// by legacy daemons (API < 1.52 always populate Items -- which is what this
// deployment's daemon does: Docker 27.5.1 / API 1.47, verified), so that
// forward-compatible path is not exercised against this host and a
// conformance case must not pretend it is.
//
// Volumes:true asks the daemon to du volumes ONLY -- the type filter is
// server-side, so this does not walk the image store. There is no name
// filtering: the value of the single call is that it covers the whole fleet,
// and filtering by name is the caller's one map lookup. RefCount is
// deliberately not mirrored (no consumer has an action to take on it).
func (d *Docker) VolumeUsage(ctx context.Context) ([]VolumeUsage, error) {
	res, err := d.api.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return nil, fmt.Errorf("reading volume disk usage: %w", err)
	}
	out := make([]VolumeUsage, 0, len(res.Volumes.Items))
	for _, v := range res.Volumes.Items {
		out = append(out, toVolumeUsage(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
// so this checks the daemon's message text instead.
//
// The daemon words this condition TWO ways, and which one comes back depends
// on whether the network itself still exists -- so both must be matched:
//
//	container X is not connected to network Y      -- network exists, container
//	                                                  is simply not attached to it
//	container X is not connected to the network Y  -- the network is GONE
//
// The second wording is the dangerous one. It is the case teardown hits when
// an agent's dinernet has already been removed out from under a half-finished
// delete, and it is a 500 like the first -- NOT the 404 a missing network earns
// on most other routes (NetworkRemove, for one), so cerrdefs.IsNotFound does
// not catch it. Matching only the first wording left NetworkDisconnect failing
// there, which is not the benign "stops being idempotent" the old comment here
// assumed: teardown is what both Delete and Reconcile run before they remove
// the agent record, so the failure wedged the record in StatusDeleting
// permanently -- every retry and every operator restart failed identically,
// with the containers and volumes already gone (teardown collects errors and
// keeps going) and so nothing left on the daemon for a human to clean up by
// hand. See the DisconnectMissingNetwork conformance case.
func isNotConnectedError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "is not connected to network") ||
		strings.Contains(msg, "is not connected to the network")
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

// ContainerStats returns one sample of the container's resource usage. The
// daemon takes two samples one second apart; the call therefore blocks ~1s.
func (d *Docker) ContainerStats(ctx context.Context, id string) (Stats, error) {
	res, err := d.api.ContainerStats(ctx, id, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	if err != nil {
		return Stats{}, wrapErr("container", id, err)
	}
	defer func() { _ = res.Body.Close() }()
	var sr container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&sr); err != nil {
		// A zero-length body (some daemon versions answer a non-running
		// container this way) also lands here as EOF.
		return Stats{}, fmt.Errorf("reading stats for container %q: %w", id, err)
	}
	// A non-running container does NOT error on the wire: verified against
	// Docker 27.5.1, the daemon answers 200 with an all-zero document whose
	// read is the year-1 zero time. That is "no stats", not a sample of
	// zeros -- surface it as an error so a stopped container never reads as
	// a silently idle one.
	if sr.Read.IsZero() {
		return Stats{}, fmt.Errorf("reading stats for container %q: container is not running", id)
	}
	return toStats(sr), nil
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

// ImageInspect returns one image by reference.
func (d *Docker) ImageInspect(ctx context.Context, ref string) (Image, error) {
	res, err := d.api.ImageInspect(ctx, ref)
	if err != nil {
		return Image{}, wrapErr("image", ref, err)
	}
	return Image{ID: res.ID, RepoTags: append([]string(nil), res.RepoTags...)}, nil
}

// ImagePull pulls ref and blocks until the pull has finished.
func (d *Docker) ImagePull(ctx context.Context, ref string) error {
	res, err := d.api.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", ref, err)
	}
	defer func() { _ = res.Close() }()
	if err := res.Wait(ctx); err != nil {
		return fmt.Errorf("pulling image %q: %w", ref, err)
	}
	return nil
}

// ImageList returns the images present on the daemon for one repository.
//
// repo may be given in either the normalized or the familiar form; it is
// canonicalized with FamiliarRepo before building the daemon-side filter and
// before keepRepoTags compares RepoTags against it, because the daemon speaks
// only the familiar form.
func (d *Docker) ImageList(ctx context.Context, repo string) ([]Image, error) {
	repo = FamiliarRepo(repo)
	res, err := d.api.ImageList(ctx, client.ImageListOptions{Filters: referenceFilter(repo)})
	if err != nil {
		return nil, fmt.Errorf("listing images for repository %q: %w", repo, err)
	}
	out := make([]Image, 0, len(res.Items))
	for _, s := range res.Items {
		tags := keepRepoTags(repo, s.RepoTags)
		if len(tags) == 0 {
			continue
		}
		out = append(out, Image{ID: s.ID, RepoTags: tags})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepoTags[0] < out[j].RepoTags[0] })
	return out, nil
}

// ImageRemove removes image ref from the daemon: untagging when the image
// carries other tags, deleting the image and pruning its now-unreferenced
// parent layers when it does not. Removing an image that is already gone is
// success, and it never forces -- see the interface comment.
//
// PruneChildren must be passed explicitly: the SDK maps the zero value to
// noprune=1, which would untag and leave every layer on disk -- and
// reclaiming that disk is the whole reason the operator removes images at
// all. The result items (which tags were untaged vs deleted) are discarded,
// the same way ImagePull discards its progress stream.
func (d *Docker) ImageRemove(ctx context.Context, ref string) error {
	_, err := d.api.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("removing image %q: %w", ref, err)
	}
	return nil
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

// FamiliarRepo returns repo in the form the daemon's images/json API speaks:
// the FAMILIAR name -- no "docker.io/" domain, no "library/" prefix -- e.g.
// "myorg/agent-image" for both "myorg/agent-image" and
// "docker.io/myorg/agent-image", and "alpine" for "docker.io/library/alpine".
// WHY (#205): the daemon matches the images/json "reference" filter and
// reports RepoTags in the familiar form (verified against a real daemon,
// Docker 27.5.1 / API 1.47: filters={"reference":["docker.io/library/busybox:*"]}
// matches nothing while ["busybox:*"] matches busybox:1.36.1), while callers
// like agent.localImageTags hold repos in the NORMALIZED form
// (reference.ParseNormalizedNamed, "docker.io/..."). Handing the normalized
// form to the daemon therefore returns zero images for a Docker-Hub-hosted
// repository, silently degrading "prefer what's on the host" to "always use
// the newest published tag". Canonicalizing here -- the only package that
// talks to the daemon -- means no caller has to remember which form to use.
// A repo that does not parse is returned unchanged (the same fallback
// agent.RepoWithoutTag applies), so a malformed input fails visibly by
// matching nothing rather than being silently rewritten.
func FamiliarRepo(repo string) string {
	named, err := reference.ParseNormalizedNamed(repo)
	if err != nil {
		return repo
	}
	return reference.FamiliarName(reference.TrimNamed(named))
}

// referenceFilter builds the images/json "reference" filter for one
// repository. repo must be in the familiar form FamiliarRepo produces;
// Docker.ImageList canonicalizes before calling. The pattern is "<repo>:*",
// made explicit even though a bare "<repo>" matches the same set (moby's
// reference.FamiliarMatch falls back to matching the familiar NAME when the
// full pattern match fails, so a tagless pattern is not "no tag" -- it's
// "every tag", same as ":*"). Confirmed by direct experiment that this filter
// does real work (it also prunes the RepoTags the daemon returns per image,
// not just which images are listed) but is still not the correctness
// boundary: keepRepoTags re-checks every entry regardless, so a daemon that
// ignored this filter entirely would still produce correct results. An empty
// repo returns no filter, since ImageList(ctx, "") lists every image.
func referenceFilter(repo string) client.Filters {
	if repo == "" {
		return nil
	}
	f := make(client.Filters)
	f.Add("reference", repo+":*")
	return f
}

// keepRepoTags returns the entries of repoTags that name a genuine tag
// (dropping the dangling "<none>:<none>" form a daemon-side filter can still
// let through) and, when repo is non-empty, that belong to repo. repo must be
// in the familiar form FamiliarRepo produces, matching the form the daemon
// reports RepoTags in; Docker.ImageList canonicalizes before calling. A colon
// with a later slash is a registry port, not a tag separator, so it is not
// treated as one.
func keepRepoTags(repo string, repoTags []string) []string {
	var out []string
	for _, rt := range repoTags {
		if rt == "<none>:<none>" {
			continue
		}
		i := strings.LastIndex(rt, ":")
		if i <= 0 || strings.Contains(rt[i+1:], "/") {
			continue
		}
		if repo != "" && rt[:i] != repo {
			continue
		}
		out = append(out, rt)
	}
	return out
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
		mm := mount.Mount{
			Type:     mount.Type(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}
		// Set VolumeOptions ONLY when a subpath is requested: a non-nil
		// VolumeOptions on the existing bind/volume mounts changes their wire
		// format and breaks them.
		if m.Subpath != "" {
			mm.VolumeOptions = &mount.VolumeOptions{Subpath: m.Subpath}
		}
		out = append(out, mm)
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

// toVolumeUsage maps one df volume entry to VolumeUsage. A nil UsageData (the
// daemon could not compute it, or a driver that does not report size) becomes
// -1, NOT 0: the caller must tell "unknown" from "a real 0 B", so -1 is passed
// through rather than clamped. The daemon's own Size is not adjusted -- a real
// 0 from a never-written local volume stays 0.
func toVolumeUsage(v volume.Volume) VolumeUsage {
	u := VolumeUsage{Name: v.Name, Size: -1}
	if v.UsageData != nil {
		u.Size = v.UsageData.Size
	}
	return u
}

func toStats(sr container.StatsResponse) Stats {
	s := Stats{
		Read:        sr.Read,
		MemoryUsed:  sr.MemoryStats.Usage,
		MemoryLimit: sr.MemoryStats.Limit,
		Pids:        sr.PidsStats.Current,
	}
	if inactive, ok := sr.MemoryStats.Stats["inactive_file"]; ok && inactive <= s.MemoryUsed {
		s.MemoryUsed -= inactive // the same subtraction `docker stats` performs
	}
	s.CPUPercent = cpuPercent(sr.CPUStats, sr.PreCPUStats)
	return s
}

// cpuPercent computes the daemon's formula over the sample pair, guarding the
// two known quirks rather than dividing blindly: OnlineCPUs can be 0 (fall
// back to the per-cpu slice length, then to 1), and either counter can go
// BACKWARDS across a container restart -- a plain cur-pre on uint64 would
// underflow to ~2^64 and report an absurd percentage, so every delta is
// signed-compared first. A missing predecessor sample reads as zero.
func cpuPercent(cur, pre container.CPUStats) float64 {
	if pre.CPUUsage.TotalUsage == 0 || cur.CPUUsage.TotalUsage < pre.CPUUsage.TotalUsage {
		return 0
	}
	if cur.SystemUsage <= pre.SystemUsage {
		return 0
	}
	cpus := float64(cur.OnlineCPUs)
	if cpus == 0 {
		if n := len(cur.CPUUsage.PercpuUsage); n > 0 {
			cpus = float64(n)
		} else {
			cpus = 1
		}
	}
	cpuDelta := float64(cur.CPUUsage.TotalUsage - pre.CPUUsage.TotalUsage)
	sysDelta := float64(cur.SystemUsage - pre.SystemUsage)
	return cpuDelta / sysDelta * cpus * 100
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
