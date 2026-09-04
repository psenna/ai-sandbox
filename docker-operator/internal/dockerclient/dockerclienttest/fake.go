// Package dockerclienttest provides an in-memory fake of
// dockerclient.Client for tests that must not touch a Docker daemon.
//
// It is deliberately faithful about the behaviours the agent lifecycle
// depends on: removes are idempotent, inspects of missing objects return
// dockerclient.ErrNotFound, list results are label-filtered and name-sorted,
// and NetworkConnect assigns an address only to a RUNNING container.
// Anything less faithful would let later create/delete-flow tests pass
// against a fake that the real daemon disagrees with.
package dockerclienttest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

// errConflict is returned by ContainerCreate when the requested name is
// already in use. It deliberately does not satisfy dockerclient.IsNotFound:
// a name conflict and a missing object are different failures.
var errConflict = errors.New("already exists")

// Op names one Fake method, for use with Fail, FailOnce and in a Call
// recorded by Calls.
type Op string

// The complete set of operations the Fake records and can be told to fail.
const (
	OpPing              Op = "Ping"
	OpClose             Op = "Close"
	OpVolumeCreate      Op = "VolumeCreate"
	OpVolumeInspect     Op = "VolumeInspect"
	OpVolumeList        Op = "VolumeList"
	OpVolumeRemove      Op = "VolumeRemove"
	OpNetworkCreate     Op = "NetworkCreate"
	OpNetworkInspect    Op = "NetworkInspect"
	OpNetworkList       Op = "NetworkList"
	OpNetworkRemove     Op = "NetworkRemove"
	OpNetworkConnect    Op = "NetworkConnect"
	OpNetworkDisconnect Op = "NetworkDisconnect"
	OpContainerCreate   Op = "ContainerCreate"
	OpContainerStart    Op = "ContainerStart"
	OpContainerStop     Op = "ContainerStop"
	OpContainerRemove   Op = "ContainerRemove"
	OpContainerInspect  Op = "ContainerInspect"
	OpContainerList     Op = "ContainerList"
	OpExecCreate        Op = "ExecCreate"
	OpExecAttach        Op = "ExecAttach"
	OpExecResize        Op = "ExecResize"
	OpExecInspect       Op = "ExecInspect"
	OpEvents            Op = "Events"
)

// Call is one recorded invocation of a Fake method. Target is the object the
// call concerns -- a name or ID, or "" for calls that name nothing (Ping,
// Close, the List methods).
type Call struct {
	Op     Op
	Target string
}

// volumeRecord is a Fake's internal representation of a volume.
type volumeRecord struct {
	name       string
	labels     map[string]string
	mountpoint string
}

func (v *volumeRecord) toVolume() dockerclient.Volume {
	return dockerclient.Volume{Name: v.name, Labels: copyLabels(v.labels), Mountpoint: v.mountpoint}
}

// networkRecord is a Fake's internal representation of a network. idx and
// nextEndpoint feed the deterministic address scheme documented on
// Fake.NetworkConnect.
type networkRecord struct {
	id           string
	name         string
	labels       map[string]string
	idx          int
	nextEndpoint int
}

func (n *networkRecord) toNetwork() dockerclient.Network {
	return dockerclient.Network{ID: n.id, Name: n.name, Labels: copyLabels(n.labels)}
}

// containerRecord is a Fake's internal representation of a container.
type containerRecord struct {
	id       string
	name     string
	image    string
	spec     dockerclient.ContainerSpec
	state    dockerclient.ContainerState
	health   dockerclient.HealthStatus
	exitCode int
	labels   map[string]string
	networks map[string]netip.Addr
}

func (c *containerRecord) toContainer() dockerclient.Container {
	return dockerclient.Container{
		ID:       c.id,
		Name:     c.name,
		Image:    c.image,
		State:    c.state,
		Health:   c.health,
		ExitCode: c.exitCode,
		Labels:   copyLabels(c.labels),
		Networks: copyAddrs(c.networks),
	}
}

// execRecord is a Fake's internal representation of a prepared exec. Its own
// mutex is separate from Fake.mu: reads and writes against the stream happen
// without holding the Fake-wide lock.
type execRecord struct {
	mu       sync.Mutex
	output   []byte
	cursor   int
	exitCode int
	running  bool
	stdin    []byte
}

// subscriber is one live Events call. done is closed when its goroutine
// exits, so Emit never blocks forever delivering to a subscriber whose
// context has already been cancelled.
type subscriber struct {
	filter dockerclient.EventFilter
	ch     chan dockerclient.Event
	done   chan struct{}
}

// Fake is an in-memory, concurrency-safe implementation of
// dockerclient.Client.
type Fake struct {
	mu sync.Mutex

	idSeq  int
	netSeq int

	volumes    map[string]*volumeRecord
	networks   map[string]*networkRecord
	containers map[string]*containerRecord
	execs      map[string]*execRecord

	calls []Call

	failSticky map[Op]error
	failOnce   map[Op][]error

	subs []*subscriber

	// ExecOutput seeds the bytes ExecAttach's stream yields on Read, keyed by
	// strings.Join(spec.Cmd, " "). A command with no entry produces no output
	// (an immediate EOF).
	ExecOutput map[string][]byte
	// ExecExit seeds the exit code ExecInspect reports once the exec's
	// output has been fully read, keyed the same way as ExecOutput. A
	// command with no entry exits 0.
	ExecExit map[string]int
}

var _ dockerclient.Client = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{
		volumes:    map[string]*volumeRecord{},
		networks:   map[string]*networkRecord{},
		containers: map[string]*containerRecord{},
		execs:      map[string]*execRecord{},
		ExecOutput: map[string][]byte{},
		ExecExit:   map[string]int{},
	}
}

// Fail makes every future call to op return err, until cleared by a later
// call to Fail(op, nil). It takes priority below any queued FailOnce error.
func (f *Fake) Fail(op Op, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failSticky, op)
		return
	}
	if f.failSticky == nil {
		f.failSticky = map[Op]error{}
	}
	f.failSticky[op] = err
}

// FailOnce queues err to be returned by the next call to op only. Multiple
// queued errors for the same op are consumed in the order they were queued.
func (f *Fake) FailOnce(op Op, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce == nil {
		f.failOnce = map[Op][]error{}
	}
	f.failOnce[op] = append(f.failOnce[op], err)
}

// Calls returns every call made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// Volumes returns a snapshot of every volume, sorted by name.
func (f *Fake) Volumes() []dockerclient.Volume {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerclient.Volume, 0, len(f.volumes))
	for _, v := range f.volumes {
		out = append(out, v.toVolume())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Networks returns a snapshot of every network, sorted by name.
func (f *Fake) Networks() []dockerclient.Network {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerclient.Network, 0, len(f.networks))
	for _, n := range f.networks {
		out = append(out, n.toNetwork())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Containers returns a snapshot of every container, sorted by name.
func (f *Fake) Containers() []dockerclient.Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerclient.Container, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, c.toContainer())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ExecInput returns everything written to the given exec's stdin so far.
func (f *Fake) ExecInput(execID string) []byte {
	f.mu.Lock()
	e, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]byte, len(e.stdin))
	copy(out, e.stdin)
	return out
}

// Emit publishes ev to every live Events subscriber whose filter matches it.
// It exists so tests can simulate daemon-originated events -- an unexpected
// container death, an out-of-band health transition -- that no Fake method
// call would otherwise produce.
func (f *Fake) Emit(ev dockerclient.Event) {
	f.mu.Lock()
	subs := make([]*subscriber, len(f.subs))
	copy(subs, f.subs)
	f.mu.Unlock()
	for _, s := range subs {
		if !matchesFilter(s.filter, ev) {
			continue
		}
		select {
		case s.ch <- ev:
		case <-s.done:
		}
	}
}

// call records one invocation and reports the error, if any, it should
// return: a queued FailOnce error takes priority, then a sticky Fail error.
func (f *Fake) call(op Op, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Op: op, Target: target})
	if q := f.failOnce[op]; len(q) > 0 {
		err := q[0]
		f.failOnce[op] = q[1:]
		return err
	}
	if err, ok := f.failSticky[op]; ok {
		return err
	}
	return nil
}

// nextID returns a deterministic, monotonically increasing ID. Callers must
// hold f.mu.
func (f *Fake) nextID() string {
	f.idSeq++
	return fmt.Sprintf("%064x", f.idSeq)
}

// Ping always succeeds unless told to Fail.
func (f *Fake) Ping(ctx context.Context) error {
	return f.call(OpPing, "")
}

// Close always succeeds unless told to Fail.
func (f *Fake) Close() error {
	return f.call(OpClose, "")
}

// VolumeCreate creates a volume, or returns the existing one of the same
// name unchanged.
func (f *Fake) VolumeCreate(ctx context.Context, spec dockerclient.VolumeSpec) (dockerclient.Volume, error) {
	if err := f.call(OpVolumeCreate, spec.Name); err != nil {
		return dockerclient.Volume{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.volumes[spec.Name]; ok {
		return v.toVolume(), nil
	}
	v := &volumeRecord{
		name:       spec.Name,
		labels:     copyLabels(spec.Labels),
		mountpoint: "/var/lib/docker/volumes/" + spec.Name + "/_data",
	}
	f.volumes[spec.Name] = v
	return v.toVolume(), nil
}

// VolumeInspect returns one volume by name.
func (f *Fake) VolumeInspect(ctx context.Context, name string) (dockerclient.Volume, error) {
	if err := f.call(OpVolumeInspect, name); err != nil {
		return dockerclient.Volume{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[name]
	if !ok {
		return dockerclient.Volume{}, fmt.Errorf("volume %q: %w", name, dockerclient.ErrNotFound)
	}
	return v.toVolume(), nil
}

// VolumeList returns volumes carrying every label in labels.
func (f *Fake) VolumeList(ctx context.Context, labels map[string]string) ([]dockerclient.Volume, error) {
	if err := f.call(OpVolumeList, ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dockerclient.Volume
	for _, v := range f.volumes {
		if labelSubset(labels, v.labels) {
			out = append(out, v.toVolume())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// VolumeRemove removes a volume, ignoring one already gone.
func (f *Fake) VolumeRemove(ctx context.Context, name string) error {
	if err := f.call(OpVolumeRemove, name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
	return nil
}

// NetworkCreate creates a network, or returns the existing one of the same
// name unchanged (this fake's networks are keyed by name, like its volumes;
// the real daemon allows duplicate network names, which no consumer of this
// package relies on).
func (f *Fake) NetworkCreate(ctx context.Context, spec dockerclient.NetworkSpec) (dockerclient.Network, error) {
	if err := f.call(OpNetworkCreate, spec.Name); err != nil {
		return dockerclient.Network{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.networks[spec.Name]; ok {
		return n.toNetwork(), nil
	}
	f.netSeq++
	n := &networkRecord{id: f.nextID(), name: spec.Name, labels: copyLabels(spec.Labels), idx: f.netSeq}
	f.networks[spec.Name] = n
	return n.toNetwork(), nil
}

// NetworkInspect returns one network by name.
func (f *Fake) NetworkInspect(ctx context.Context, name string) (dockerclient.Network, error) {
	if err := f.call(OpNetworkInspect, name); err != nil {
		return dockerclient.Network{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return dockerclient.Network{}, fmt.Errorf("network %q: %w", name, dockerclient.ErrNotFound)
	}
	return n.toNetwork(), nil
}

// NetworkList returns networks carrying every label in labels.
func (f *Fake) NetworkList(ctx context.Context, labels map[string]string) ([]dockerclient.Network, error) {
	if err := f.call(OpNetworkList, ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dockerclient.Network
	for _, n := range f.networks {
		if labelSubset(labels, n.labels) {
			out = append(out, n.toNetwork())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// NetworkRemove removes a network, ignoring one already gone.
func (f *Fake) NetworkRemove(ctx context.Context, name string) error {
	if err := f.call(OpNetworkRemove, name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, name)
	return nil
}

// NetworkConnect attaches a running container to a network, assigning it a
// deterministic address of the form 172.31.<network index>.<endpoint
// index>. Reconnecting an already-connected container returns its existing
// address unchanged.
func (f *Fake) NetworkConnect(ctx context.Context, networkName, containerID string) (netip.Addr, error) {
	if err := f.call(OpNetworkConnect, networkName+"/"+containerID); err != nil {
		return netip.Addr{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[networkName]
	if !ok {
		return netip.Addr{}, fmt.Errorf("network %q: %w", networkName, dockerclient.ErrNotFound)
	}
	c, ok := f.containers[containerID]
	if !ok {
		return netip.Addr{}, fmt.Errorf("container %q: %w", containerID, dockerclient.ErrNotFound)
	}
	if c.state != dockerclient.StateRunning {
		return netip.Addr{}, fmt.Errorf("container %q joined network %q but the daemon reported no address for it (is the container running?)", containerID, networkName)
	}
	if addr, already := c.networks[networkName]; already {
		return addr, nil
	}
	n.nextEndpoint++
	addr := allocAddr(n.idx, n.nextEndpoint)
	c.networks[networkName] = addr
	return addr, nil
}

// NetworkDisconnect detaches a container, ignoring one already detached or
// either object being gone.
func (f *Fake) NetworkDisconnect(ctx context.Context, networkName, containerID string) error {
	if err := f.call(OpNetworkDisconnect, networkName+"/"+containerID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[containerID]
	if !ok {
		return nil
	}
	delete(c.networks, networkName)
	return nil
}

// ContainerCreate creates a container and returns its ID, or a conflict
// error if the name is already in use.
func (f *Fake) ContainerCreate(ctx context.Context, spec dockerclient.ContainerSpec) (string, error) {
	if err := f.call(OpContainerCreate, spec.Name); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.name == spec.Name {
			return "", fmt.Errorf("container %q: %w", spec.Name, errConflict)
		}
	}
	id := f.nextID()
	f.containers[id] = &containerRecord{
		id:       id,
		name:     spec.Name,
		image:    spec.Image,
		spec:     spec,
		state:    dockerclient.StateCreated,
		health:   dockerclient.HealthNone,
		labels:   copyLabels(spec.Labels),
		networks: map[string]netip.Addr{},
	}
	return id, nil
}

// ContainerStart starts a created container: its state becomes running, it
// is assigned an address on every network its spec attached it to, and a
// "start" event is emitted.
func (f *Fake) ContainerStart(ctx context.Context, id string) error {
	if err := f.call(OpContainerStart, id); err != nil {
		return err
	}
	f.mu.Lock()
	c, ok := f.containers[id]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("container %q: %w", id, dockerclient.ErrNotFound)
	}
	c.state = dockerclient.StateRunning
	for _, na := range c.spec.Networks {
		if _, already := c.networks[na.Name]; already {
			continue
		}
		n, ok := f.networks[na.Name]
		if !ok {
			continue
		}
		n.nextEndpoint++
		c.networks[na.Name] = allocAddr(n.idx, n.nextEndpoint)
	}
	labels := copyLabels(c.labels)
	f.mu.Unlock()
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeContainer, Action: dockerclient.ActionStart, ActorID: id, Attributes: labels, Time: time.Now()})
	return nil
}

// ContainerStop stops a container: its state becomes exited, and "die" then
// "stop" events are emitted. A missing container is a no-op.
func (f *Fake) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	if err := f.call(OpContainerStop, id); err != nil {
		return err
	}
	f.mu.Lock()
	c, ok := f.containers[id]
	if !ok {
		f.mu.Unlock()
		return nil
	}
	c.state = dockerclient.StateExited
	labels := copyLabels(c.labels)
	f.mu.Unlock()
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeContainer, Action: dockerclient.ActionDie, ActorID: id, Attributes: labels, Time: time.Now()})
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeContainer, Action: dockerclient.ActionStop, ActorID: id, Attributes: labels, Time: time.Now()})
	return nil
}

// ContainerRemove drops the container record and emits a "destroy" event.
// A missing container is a no-op.
func (f *Fake) ContainerRemove(ctx context.Context, id string) error {
	if err := f.call(OpContainerRemove, id); err != nil {
		return err
	}
	f.mu.Lock()
	c, ok := f.containers[id]
	if !ok {
		f.mu.Unlock()
		return nil
	}
	delete(f.containers, id)
	labels := copyLabels(c.labels)
	f.mu.Unlock()
	f.Emit(dockerclient.Event{Type: dockerclient.EventTypeContainer, Action: dockerclient.ActionDestroy, ActorID: id, Attributes: labels, Time: time.Now()})
	return nil
}

// ContainerInspect returns the full state of one container by ID or name.
func (f *Fake) ContainerInspect(ctx context.Context, id string) (dockerclient.Container, error) {
	if err := f.call(OpContainerInspect, id); err != nil {
		return dockerclient.Container{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.resolveContainer(id)
	if !ok {
		return dockerclient.Container{}, fmt.Errorf("container %q: %w", id, dockerclient.ErrNotFound)
	}
	return c.toContainer(), nil
}

// resolveContainer looks a container up by ID first, then by name -- the
// same fallback the real daemon applies, and which every consumer that
// passes ContainerCreate's returned ID relies on implicitly. Callers must
// hold f.mu.
func (f *Fake) resolveContainer(idOrName string) (*containerRecord, bool) {
	if c, ok := f.containers[idOrName]; ok {
		return c, true
	}
	for _, c := range f.containers {
		if c.name == idOrName {
			return c, true
		}
	}
	return nil, false
}

// ContainerList returns containers carrying every label in labels. Health
// and ExitCode are left zero, matching the real daemon's list endpoint.
func (f *Fake) ContainerList(ctx context.Context, labels map[string]string) ([]dockerclient.Container, error) {
	if err := f.call(OpContainerList, ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dockerclient.Container
	for _, c := range f.containers {
		if !labelSubset(labels, c.labels) {
			continue
		}
		cc := c.toContainer()
		cc.Health = dockerclient.HealthNone
		cc.ExitCode = 0
		out = append(out, cc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ExecCreate prepares a process, seeding its output and exit code from
// ExecOutput/ExecExit if the caller populated them for spec.Cmd.
func (f *Fake) ExecCreate(ctx context.Context, containerID string, spec dockerclient.ExecSpec) (string, error) {
	if err := f.call(OpExecCreate, containerID); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.resolveContainer(containerID); !ok {
		return "", fmt.Errorf("container %q: %w", containerID, dockerclient.ErrNotFound)
	}
	key := strings.Join(spec.Cmd, " ")
	id := f.nextID()
	f.execs[id] = &execRecord{
		output:   append([]byte(nil), f.ExecOutput[key]...),
		exitCode: f.ExecExit[key],
		running:  true,
	}
	return id, nil
}

// ExecAttach returns an in-memory stream: reads drain the exec's seeded
// output and then EOF (at which point the exec is considered finished, and
// ExecInspect reports its seeded exit code); writes are recorded for
// ExecInput.
func (f *Fake) ExecAttach(ctx context.Context, execID string) (dockerclient.ExecStream, error) {
	if err := f.call(OpExecAttach, execID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	e, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("exec %q: %w", execID, dockerclient.ErrNotFound)
	}
	return &fakeStream{e: e}, nil
}

// ExecResize is a no-op beyond confirming the exec exists.
func (f *Fake) ExecResize(ctx context.Context, execID string, size dockerclient.TTYSize) error {
	if err := f.call(OpExecResize, execID); err != nil {
		return err
	}
	f.mu.Lock()
	_, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("exec %q: %w", execID, dockerclient.ErrNotFound)
	}
	return nil
}

// ExecInspect reports whether an exec is still running and its exit code.
func (f *Fake) ExecInspect(ctx context.Context, execID string) (dockerclient.ExecStatus, error) {
	if err := f.call(OpExecInspect, execID); err != nil {
		return dockerclient.ExecStatus{}, err
	}
	f.mu.Lock()
	e, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return dockerclient.ExecStatus{}, fmt.Errorf("exec %q: %w", execID, dockerclient.ErrNotFound)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return dockerclient.ExecStatus{Running: e.running, ExitCode: e.exitCode}, nil
}

// Events streams events matching filter to a fresh subscriber. One goroutine
// per subscriber forwards Emit-ed events until ctx is cancelled, at which
// point both channels are closed and the subscriber is dropped.
func (f *Fake) Events(ctx context.Context, filter dockerclient.EventFilter) (<-chan dockerclient.Event, <-chan error) {
	if err := f.call(OpEvents, ""); err != nil {
		errs := make(chan error, 1)
		errs <- err
		close(errs)
		out := make(chan dockerclient.Event)
		close(out)
		return out, errs
	}

	sub := &subscriber{filter: filter, ch: make(chan dockerclient.Event), done: make(chan struct{})}
	f.mu.Lock()
	f.subs = append(f.subs, sub)
	f.mu.Unlock()

	out := make(chan dockerclient.Event)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		defer close(sub.done)
		defer f.removeSub(sub)
		for {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case ev := <-sub.ch:
				select {
				case out <- ev:
				case <-ctx.Done():
					errs <- ctx.Err()
					return
				}
			}
		}
	}()
	return out, errs
}

func (f *Fake) removeSub(sub *subscriber) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.subs {
		if s == sub {
			f.subs = append(f.subs[:i], f.subs[i+1:]...)
			return
		}
	}
}

// fakeStream is the ExecStream ExecAttach returns.
type fakeStream struct {
	e *execRecord
}

func (s *fakeStream) Read(p []byte) (int, error) {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	if s.e.cursor >= len(s.e.output) {
		s.e.running = false
		return 0, io.EOF
	}
	n := copy(p, s.e.output[s.e.cursor:])
	s.e.cursor += n
	return n, nil
}

func (s *fakeStream) Write(p []byte) (int, error) {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	s.e.stdin = append(s.e.stdin, p...)
	return len(p), nil
}

func (s *fakeStream) Close() error      { return nil }
func (s *fakeStream) CloseWrite() error { return nil }

// matchesFilter reports whether ev satisfies filter, using the same
// all-labels-must-match, any-type-matches semantics as the real daemon.
func matchesFilter(filter dockerclient.EventFilter, ev dockerclient.Event) bool {
	if len(filter.Types) > 0 {
		found := false
		for _, t := range filter.Types {
			if t == ev.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for k, v := range filter.Labels {
		if ev.Attributes[k] != v {
			return false
		}
	}
	return true
}

// labelSubset reports whether have carries every key/value pair in want.
func labelSubset(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// allocAddr deterministically assigns 172.31.<netIdx>.<endpointIdx>. Both
// indices are clamped to a byte's range first: no test in this repository
// creates anywhere near 255 networks or endpoints, so clamping (rather than
// wrapping) just keeps the conversion provably safe for gosec.
func allocAddr(netIdx, endpointIdx int) netip.Addr {
	return netip.AddrFrom4([4]byte{172, 31, clampByte(netIdx), clampByte(endpointIdx)})
}

func clampByte(n int) byte {
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return byte(n)
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

func copyAddrs(in map[string]netip.Addr) map[string]netip.Addr {
	out := make(map[string]netip.Addr, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
