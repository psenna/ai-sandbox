package agent

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ErrNoAnthropicAuth is returned by Create for a backend=anthropic request
// when no Anthropic credential has been stored yet. internal/api maps it to
// a 409 ("configure the Anthropic account first").
var ErrNoAnthropicAuth = errors.New("no Anthropic credential is configured")

// ErrInvalidBackend is returned by Create for a backend that is neither
// config.BackendOllama nor config.BackendAnthropic.
var ErrInvalidBackend = errors.New("invalid agent backend")

// ErrInvalidRepo is returned by Create for a non-empty CreateRequest.Repo
// that is not a plausible owner/repo(.git) reference.
var ErrInvalidRepo = errors.New("invalid agent repo")

// IsNoAnthropicAuth / IsInvalidBackend / IsInvalidRepo let internal/api map
// the create-time request errors without importing the sentinels by name.
func IsNoAnthropicAuth(err error) bool { return errors.Is(err, ErrNoAnthropicAuth) }
func IsInvalidBackend(err error) bool  { return errors.Is(err, ErrInvalidBackend) }
func IsInvalidRepo(err error) bool     { return errors.Is(err, ErrInvalidRepo) }

// resolvedBackend is everything about an agent's LLM backend that its
// container environment needs, worked out once in Create from the request,
// the operator config and -- for the anthropic backend -- the stored shared
// credential. It is threaded through the build sequence rather than re-read,
// so a credential change mid-create cannot half-apply.
type resolvedBackend struct {
	kind      string // config.BackendOllama | config.BackendAnthropic
	model     string // ollama only: the default/opus tier
	fastModel string // ollama only: the sonnet/haiku tier
	// anthropic only: exactly one is non-empty.
	apiKey     string
	oauthToken string
}

// dindInitScript is scripts/dind-init.sh, embedded so the operator can hand it
// to each DinD sidecar as an argument instead of bind-mounting it.
//
// A bind mount would be resolved by the DAEMON against the HOST filesystem,
// which a containerised operator has no way to name or verify; passing the
// script inline removes the filesystem from the picture entirely. go:embed
// cannot reach outside this package's directory and refuses symlinks, so this
// is a real copy of ../../../scripts/dind-init.sh -- `make dind-init-check`
// fails the build if the two ever drift.
//
//go:embed dind-init.sh
var dindInitScript string

// The labels every Docker resource the operator creates carries. They are the
// orphan-recovery mechanism -- the direct analog of the Kubernetes operator's
// owner references -- and the only thing that lets the startup reconcile pass
// tell an operator-managed container from anything else on a shared host.
const (
	// LabelManaged marks a resource as belonging to some docker-operator.
	LabelManaged = "ai-sandbox.docker-operator/managed"
	// LabelAgentID names the agent record the resource belongs to.
	LabelAgentID = "ai-sandbox.docker-operator/agent-id"
	// LabelRole says what the resource is within that agent; see Role.
	LabelRole = "ai-sandbox.docker-operator/role"

	// LabelManagedValue is the only value LabelManaged ever takes.
	LabelManagedValue = "true"
)

// Role is a resource's function within one agent, recorded in LabelRole.
type Role string

// The roles one agent's six Docker resources take.
const (
	RoleAgent           Role = "agent"
	RoleDind            Role = "dind"
	RoleDinernet        Role = "dinernet"
	RoleWorkspaceVolume Role = "workspace-volume"
	RoleConfigVolume    Role = "config-volume"
	RoleDindCacheVolume Role = "dind-cache-volume"
)

const (
	// resourcePrefix is the common prefix of every per-agent resource name,
	// matching the plan's resource-naming section.
	resourcePrefix = "docker-operator"

	// dindImage is the Docker-in-Docker sidecar image. It is a constant and
	// not configuration on purpose: the whole security model (an unprivileged
	// inner daemon under sysbox-runc) is a property of this specific image
	// plus dind-init.sh, and swapping it is not a supported configuration.
	dindImage = "docker:27-dind"

	// dindAlias is the DNS name the sidecar answers to on its agent's
	// dinernet, so the reused entrypoint.sh and use-docker skill's
	// DOCKER_HOST=tcp://docker:2375 keep working per agent, unchanged.
	dindAlias   = "docker"
	dindTCPPort = "2375"

	// The mount points, mirroring docker-compose.yaml's claude and docker
	// services. workspace is mounted into BOTH containers: it is the exchange
	// point between the agent and the workload containers its daemon runs.
	workspaceMount = "/workspace"
	configMount    = "/home/node/.claude-sandbox"
	dindCacheMount = "/var/lib/docker"

	// agentStoreMount is where the centralized file store's per-agent
	// subpath (agents/<id>/ inside the shared filestore volume) is mounted
	// in the agent container. Handed to the agent as AGENT_STORE_DIR. Only
	// present when the file store is enabled.
	agentStoreMount = "/workspace/store"

	// tmuxBootPath is where agent/Dockerfile bakes tmux-boot.sh, and what the
	// agent container's Cmd is overridden to at create time.
	tmuxBootPath = "/usr/local/bin/tmux-boot.sh"
	// tmuxSession is the session name tmux-boot.sh creates and that the
	// terminal bridge later attaches to.
	tmuxSession = "main"
)

// Options tunes the lifecycle flows' timing. The zero value is valid: every
// field falls back to a sane default, and tests shorten them.
type Options struct {
	// DindHealthTimeout bounds the wait for the DinD sidecar's healthcheck to
	// report healthy. Generous by default: the sidecar starts a whole Docker
	// daemon and then resolves and blocks a dozen registry hostnames.
	DindHealthTimeout time.Duration
	// TmuxReadyTimeout bounds the wait for tmux-boot.sh to create the main
	// session after the agent container starts.
	TmuxReadyTimeout time.Duration
	// ExecTimeout bounds one one-shot exec (the tmux session check).
	ExecTimeout time.Duration
	// PollInterval is how often the two waits above re-check.
	PollInterval time.Duration
	// StopTimeout is the grace period a container gets before SIGKILL during
	// teardown.
	StopTimeout time.Duration
	// TeardownTimeout bounds create-failure rollback, which deliberately runs
	// on a context detached from the caller's: the usual reason a create fails
	// is that its context was cancelled, and rollback must still happen.
	TeardownTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.DindHealthTimeout <= 0 {
		o.DindHealthTimeout = 3 * time.Minute
	}
	if o.TmuxReadyTimeout <= 0 {
		o.TmuxReadyTimeout = 60 * time.Second
	}
	if o.ExecTimeout <= 0 {
		o.ExecTimeout = 30 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = 10 * time.Second
	}
	if o.TeardownTimeout <= 0 {
		o.TeardownTimeout = 2 * time.Minute
	}
	return o
}

// Manager is the agent reconciler: it turns store records into the concrete
// Docker resources that make up an agent, and back again.
//
// It holds no lock of its own. MAX_AGENTS is enforced by store.Create's
// single-bbolt-transaction reservation, which is already atomic across
// concurrent callers, so a mutex here would only duplicate a guarantee that
// already holds -- and would be the wrong place for it, since the store file
// is the thing being protected.
type Manager struct {
	docker dockerclient.Client
	store  *store.Store
	cfg    config.Config
	log    *slog.Logger
	opts   Options
	// files is the centralized per-agent file store. nil when the file
	// store is disabled (config.FilestoreDir == "") or could not be opened.
	files *filestore.Store
}

// NewManager returns a Manager. A nil log falls back to slog.Default.
//
// When cfg enables the centralized file store (cfg.FilestoreDir != "") the
// store is opened here; a failure is logged and the manager keeps running
// without it (agents are then created with no /workspace/store mount).
func NewManager(docker dockerclient.Client, st *store.Store, cfg config.Config, log *slog.Logger, opts Options) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{docker: docker, store: st, cfg: cfg, log: log, opts: opts.withDefaults()}
	if cfg.FilestoreDir != "" {
		fs, err := filestore.New(cfg.FilestoreDir)
		if err != nil {
			log.Warn("the centralized file store is configured but unusable; agents will be created without /workspace/store",
				"filestore_dir", cfg.FilestoreDir, "error", err)
		} else {
			m.files = fs
		}
	}
	return m
}

// CreateRequest is the caller-supplied part of a new agent: everything else is
// derived from configuration or generated.
type CreateRequest struct {
	// Name is the human label shown in the UI. May be empty.
	Name string
	// Description is free-form text shown in the UI. May be empty.
	Description string
	// Backend is "ollama", "anthropic", or "" to use the operator's
	// DefaultBackend. Create validates it.
	Backend string
	// Model and FastModel override the operator's default Ollama models for
	// this one agent (default/opus tier, and sonnet/haiku tier). Empty means
	// "use the operator default". Ignored for the anthropic backend.
	Model     string
	FastModel string
	// Repo is the owner/repo.git this agent works, overriding the operator's
	// GithubRepo default for this one agent. Empty falls back to that
	// default; if that is empty too the agent boots with no repo. Nothing
	// clones it. Create validates its shape when non-empty.
	Repo string
}

// Create builds one agent end to end: reserve a slot under MAX_AGENTS, create
// the three named volumes, create the private dinernet, create and start the
// DinD sidecar and wait for it to report healthy, connect the shared
// dependaproxy container to the new dinernet and read back the address IPAM
// gave it, create and start the agent container (Cmd overridden to
// tmux-boot.sh), confirm the tmux session answered, and mark the record
// running.
//
// A failure at ANY step after the slot is reserved runs the same teardown
// routine Delete uses and then removes the store record outright, so a failed
// attempt never permanently consumes one of the MAX_AGENTS slots. Rollback is
// best-effort and logged; whatever it cannot remove is picked up by the next
// startup reconcile pass, because the record is only deleted after teardown is
// attempted and the resources still carry the managed labels either way.
//
// Create is synchronous, including a cold image pull on a fresh host. The
// caller's context bounds the whole thing.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (store.Agent, error) {
	id, err := store.NewID()
	if err != nil {
		return store.Agent{}, fmt.Errorf("creating an agent: %w", err)
	}

	// Resolve the backend before reserving a slot: an invalid backend, or a
	// backend=anthropic request with no stored credential, is the caller's
	// mistake and must not consume a slot even briefly.
	rb, err := m.resolveBackend(ctx, req)
	if err != nil {
		return store.Agent{}, fmt.Errorf("creating an agent: %w", err)
	}

	// Same reasoning for the repo: a malformed one is a bad request, not a
	// reason to burn a slot. Empty (no per-agent repo, no operator default)
	// is fine -- the agent boots as a bare terminal.
	repo := firstNonEmpty(req.Repo, m.cfg.GithubRepo)
	if repo != "" && !config.ValidGithubRepo(repo) {
		return store.Agent{}, fmt.Errorf("creating an agent: %w: %q", ErrInvalidRepo, repo)
	}

	// Reserve the slot FIRST. store.Create both counts and inserts inside one
	// bbolt read-write transaction, so N racing creates against a cap of N-1
	// produce exactly N-1 successes. The error satisfies store.IsAtCapacity,
	// which internal/api maps to 409.
	a, err := m.store.Create(ctx, store.CreateSpec{
		ID: id, Name: req.Name, Description: req.Description,
		Backend: rb.kind, Model: rb.model, FastModel: rb.fastModel,
		Repo: repo,
	})
	if err != nil {
		return store.Agent{}, fmt.Errorf("creating agent %q: %w", id, err)
	}

	if err := m.build(ctx, &a, rb); err != nil {
		m.rollback(ctx, a, err)
		return store.Agent{}, fmt.Errorf("creating agent %q: %w", id, err)
	}
	return a, nil
}

// resolveBackend turns a CreateRequest's backend fields + the operator config
// + the stored Anthropic credential into a resolvedBackend, or an error the
// caller can map to a 4xx (ErrInvalidBackend, ErrNoAnthropicAuth).
func (m *Manager) resolveBackend(ctx context.Context, req CreateRequest) (resolvedBackend, error) {
	kind := req.Backend
	if kind == "" {
		kind = m.cfg.DefaultBackend
	}
	if !config.ValidBackend(kind) {
		return resolvedBackend{}, fmt.Errorf("%w: %q", ErrInvalidBackend, kind)
	}

	rb := resolvedBackend{kind: kind}
	switch kind {
	case config.BackendOllama:
		rb.model = firstNonEmpty(req.Model, m.cfg.AgentModel)
		rb.fastModel = firstNonEmpty(req.FastModel, m.cfg.AgentFastModel)
	case config.BackendAnthropic:
		auth, ok, err := m.store.GetAnthropicAuth(ctx)
		if err != nil {
			return resolvedBackend{}, fmt.Errorf("reading the stored Anthropic credential: %w", err)
		}
		if !ok {
			return resolvedBackend{}, ErrNoAnthropicAuth
		}
		switch auth.Kind {
		case store.AnthropicKindAPIKey:
			rb.apiKey = auth.Value
		case store.AnthropicKindOAuth:
			rb.oauthToken = auth.Value
		default:
			return resolvedBackend{}, fmt.Errorf("the stored Anthropic credential has an unknown kind %q", auth.Kind)
		}
	}
	return rb, nil
}

// build runs the create sequence against a record that already holds a slot.
// Every step is ordered by a real dependency, not by taste: volumes before the
// containers that mount them, the network before the containers that join it,
// the sidecar healthy before the agent container that talks to it, and the
// dependaproxy address read back before the agent container whose environment
// carries it.
func (m *Manager) build(ctx context.Context, a *store.Agent, rb resolvedBackend) error {
	if err := m.stampNames(ctx, a); err != nil {
		return err
	}
	if err := m.ensureImages(ctx); err != nil {
		return err
	}
	if err := m.createVolumes(ctx, *a); err != nil {
		return err
	}
	if err := m.ensureAgentFiles(ctx, *a); err != nil {
		return err
	}
	if err := m.createDinernet(ctx, a); err != nil {
		return err
	}
	if err := m.startDind(ctx, a); err != nil {
		return err
	}
	if err := m.connectDependaproxy(ctx, a); err != nil {
		return err
	}
	if err := m.startAgentContainer(ctx, a, rb); err != nil {
		return err
	}
	if err := m.waitTmuxSession(ctx, *a); err != nil {
		return err
	}
	return m.markRunning(ctx, a)
}

// stampNames records all six resource names before a single resource exists.
//
// Doing it up front, rather than as each resource comes up, is what makes
// teardown independent of how far create got: rollback and the reconcile pass
// read names out of the record instead of re-deriving them, and a crash
// between any two steps still leaves a record that names everything that could
// possibly have been created.
func (m *Manager) stampNames(ctx context.Context, a *store.Agent) error {
	id := a.ID
	return m.stamp(ctx, a, "the resource names", func(ag *store.Agent) {
		ag.ContainerName = agentContainerName(id)
		ag.DindContainerName = dindContainerName(id)
		ag.DinernetName = dinernetName(id)
		ag.WorkspaceVolume = workspaceVolumeName(id)
		ag.ClaudeConfigVolume = claudeConfigVolumeName(id)
		ag.DindCacheVolume = dindCacheVolumeName(id)
	})
}

// ensureImages makes sure both images exist on the daemon before anything is
// created, so a missing image fails as one legible error rather than as a
// half-built agent.
func (m *Manager) ensureImages(ctx context.Context) error {
	for _, ref := range []string{dindImage, m.cfg.AgentImage} {
		if err := m.ensureImage(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// ensureImage pulls ref only if the daemon does not already have it, which is
// what `docker run` does. The dominant case -- a locally built agent image
// tagged :dev -- is a single cheap inspect with no registry traffic at all.
func (m *Manager) ensureImage(ctx context.Context, ref string) error {
	_, err := m.docker.ImageInspect(ctx, ref)
	switch {
	case err == nil:
		return nil
	case !dockerclient.IsNotFound(err):
		return fmt.Errorf("inspecting image %q: %w", ref, err)
	}

	m.log.InfoContext(ctx, "image not present on the daemon; pulling", "image", ref)
	if err := m.docker.ImagePull(ctx, ref); err != nil {
		return fmt.Errorf("image %q is not on the docker daemon and pulling it failed "+
			"(a locally built agent image is expected to come from `make agent-image`, not a registry): %w", ref, err)
	}
	return nil
}

// createVolumes creates the agent's three isolated named volumes. Volume
// creation is idempotent on the daemon, so a retried create reuses them.
func (m *Manager) createVolumes(ctx context.Context, a store.Agent) error {
	for _, v := range []struct {
		name string
		role Role
	}{
		{a.WorkspaceVolume, RoleWorkspaceVolume},
		{a.ClaudeConfigVolume, RoleConfigVolume},
		{a.DindCacheVolume, RoleDindCacheVolume},
	} {
		if _, err := m.docker.VolumeCreate(ctx, dockerclient.VolumeSpec{
			Name:   v.name,
			Labels: labelsFor(a.ID, v.role),
		}); err != nil {
			return fmt.Errorf("creating volume %q: %w", v.name, err)
		}
	}
	return nil
}

// ensureAgentFiles pre-creates agents/<id>/ under the shared file-store
// directory, so the daemon can resolve the per-agent subpath when it mounts
// it into the agent container. A no-op when the file store is disabled.
//
// It runs after the volumes and before the network/containers: the directory
// must exist before agentSpec's subpath mount is created, and leaving it out
// of the container-dependency chain keeps a file-store failure from ever
// half-building a container.
func (m *Manager) ensureAgentFiles(_ context.Context, a store.Agent) error {
	if m.files == nil {
		return nil
	}
	if err := m.files.EnsureAgentDir(a.ID); err != nil {
		return fmt.Errorf("creating the file-store directory for agent %q (dir %q, volume %q): %w",
			a.ID, m.cfg.FilestoreDir, m.cfg.FilestoreVolume, err)
	}
	return nil
}

// createDinernet creates the agent's private bridge network -- the isolation
// boundary between agents, and the seam a V2 egress proxy would be inserted
// at. The subnet is left to Docker's IPAM, which is exactly why
// connectDependaproxy has to read the assigned address back.
func (m *Manager) createDinernet(ctx context.Context, a *store.Agent) error {
	n, err := m.docker.NetworkCreate(ctx, dockerclient.NetworkSpec{
		Name:   a.DinernetName,
		Labels: labelsFor(a.ID, RoleDinernet),
	})
	if err != nil {
		return fmt.Errorf("creating network %q: %w", a.DinernetName, err)
	}
	return m.stamp(ctx, a, "the dinernet id", func(ag *store.Agent) { ag.DinernetID = n.ID })
}

// startDind creates and starts the Docker-in-Docker sidecar and waits for its
// healthcheck to pass before returning.
func (m *Manager) startDind(ctx context.Context, a *store.Agent) error {
	id, err := m.docker.ContainerCreate(ctx, m.dindSpec(*a))
	if err != nil {
		return fmt.Errorf("creating the dind sidecar %q: %w", a.DindContainerName, err)
	}
	if err := m.stamp(ctx, a, "the dind container id", func(ag *store.Agent) { ag.DindContainerID = id }); err != nil {
		return err
	}
	if err := m.docker.ContainerStart(ctx, id); err != nil {
		return fmt.Errorf("starting the dind sidecar %q: %w", a.DindContainerName, err)
	}
	return m.waitHealthy(ctx, id, a.DindContainerName)
}

// dindSpec templates docker-compose.yaml's `docker` service for one agent.
func (m *Manager) dindSpec(a store.Agent) dockerclient.ContainerSpec {
	return dockerclient.ContainerSpec{
		Name:  a.DindContainerName,
		Image: dindImage,

		// dind-init.sh, inline. `sh -c SCRIPT $0 $1 ...` sets $0 to the name
		// and $1.. to the dockerd args, so the script's own `dockerd "$@"`
		// behaves exactly as it does under compose's
		// entrypoint: ["sh","/dind-init.sh"] + command: [dockerd args].
		Entrypoint: []string{"sh", "-c", dindInitScript, "dind-init.sh"},
		Cmd: []string{
			"--host=tcp://0.0.0.0:2375",
			"--host=unix:///var/run/docker.sock",
		},

		// docker:dind defaults DOCKER_TLS_CERTDIR=/certs, which starts dockerd
		// with --tlsverify on the TCP host while the agent's client speaks
		// plain HTTP ("Client sent an HTTP request to an HTTPS server").
		// Empty disables it. Safe: the daemon is reachable only from this one
		// agent's private dinernet, and sysbox user-namespacing -- not TLS --
		// is the security boundary here.
		Env:    map[string]string{"DOCKER_TLS_CERTDIR": ""},
		Labels: labelsFor(a.ID, RoleDind),

		Mounts: []dockerclient.Mount{
			// The image/layer cache, so an agent's workload images survive a
			// sidecar restart instead of being re-pulled into a throwaway
			// writable layer.
			{Type: dockerclient.MountTypeVolume, Source: a.DindCacheVolume, Target: dindCacheMount},
			// The exchange point: the same workspace the agent container sees,
			// so a workload container can be given the agent's files.
			{Type: dockerclient.MountTypeVolume, Source: a.WorkspaceVolume, Target: workspaceMount},
		},
		Networks: []dockerclient.NetworkAttachment{
			{Name: a.DinernetName, Aliases: []string{dindAlias}},
		},

		// sysbox-runc is what makes an unprivileged inner daemon possible.
		Runtime: m.cfg.DockerRuntime,

		// docker:dind ships no healthcheck, so one is declared here -- it is
		// the signal create waits on before starting the agent container.
		Healthcheck: &dockerclient.Healthcheck{
			Test:        []string{"CMD", "docker", "info"},
			Interval:    5 * time.Second,
			Timeout:     3 * time.Second,
			Retries:     30,
			StartPeriod: 10 * time.Second,
		},
	}
}

// waitHealthy polls until the container's healthcheck passes, it dies, or the
// timeout expires. Polling rather than subscribing to the event stream keeps
// this self-contained: there is exactly one waiter, and the event goroutine
// (task 13) has a different job.
func (m *Manager) waitHealthy(ctx context.Context, id, name string) error {
	deadline := time.Now().Add(m.opts.DindHealthTimeout)
	for {
		c, err := m.docker.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspecting the dind sidecar %q: %w", name, err)
		}
		switch {
		case c.Health == dockerclient.HealthHealthy:
			return nil
		case c.State == dockerclient.StateExited || c.State == dockerclient.StateDead:
			return fmt.Errorf("the dind sidecar %q exited with code %d before becoming healthy "+
				"(is the %q runtime installed on this host?)", name, c.ExitCode, m.cfg.DockerRuntime)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the dind sidecar %q did not become healthy within %s (state %q, health %q)",
				name, m.opts.DindHealthTimeout, c.State, c.Health)
		}
		if err := sleepCtx(ctx, m.opts.PollInterval); err != nil {
			return err
		}
	}
}

// connectDependaproxy attaches the SHARED dependaproxy container to this
// agent's private dinernet and records the address IPAM gave it there.
//
// This is the price of per-agent networks: there is no longer one static
// dependaproxy address every agent can hardcode, so each agent gets its own
// and entrypoint.sh writes it to /workspace/dependaproxy-ip, which the
// use-docker skill's examples splice into --add-host (the inner daemon has no
// DNS for sibling container names).
//
// dependaproxy must be running for this to yield an address; a stopped one
// produces a clear error naming it.
func (m *Manager) connectDependaproxy(ctx context.Context, a *store.Agent) error {
	addr, err := m.docker.NetworkConnect(ctx, a.DinernetName, m.cfg.DependaproxyContainer)
	if err != nil {
		return fmt.Errorf("connecting the shared dependaproxy container %q to network %q: %w",
			m.cfg.DependaproxyContainer, a.DinernetName, err)
	}
	return m.stamp(ctx, a, "the dependaproxy dinernet address", func(ag *store.Agent) {
		ag.DependaproxyDinernetIP = addr
	})
}

// startAgentContainer creates and starts the agent container itself.
func (m *Manager) startAgentContainer(ctx context.Context, a *store.Agent, rb resolvedBackend) error {
	id, err := m.docker.ContainerCreate(ctx, m.agentSpec(*a, rb))
	if err != nil {
		return fmt.Errorf("creating the agent container %q: %w", a.ContainerName, err)
	}
	if err := m.stamp(ctx, a, "the agent container id", func(ag *store.Agent) { ag.ContainerID = id }); err != nil {
		return err
	}
	if err := m.docker.ContainerStart(ctx, id); err != nil {
		return fmt.Errorf("starting the agent container %q: %w", a.ContainerName, err)
	}
	return nil
}

// agentSpec templates docker-compose.yaml's `claude` service for one agent.
//
// Entrypoint is NOT overridden: the image's /entrypoint.sh has to run (it
// writes the git config, the skills, .npmrc, pip.env, go.env and
// /workspace/dependaproxy-ip) and ends in `exec "$@"`. Cmd is what is
// overridden, to tmux-boot.sh -- which is why the image's own CMD can stay
// ["bash"] and a plain `docker run` of it remains an ordinary shell.
func (m *Manager) agentSpec(a store.Agent, rb resolvedBackend) dockerclient.ContainerSpec {
	spec := dockerclient.ContainerSpec{
		Name:   a.ContainerName,
		Image:  m.cfg.AgentImage,
		Cmd:    []string{tmuxBootPath},
		Env:    m.agentEnv(a, rb),
		Labels: labelsFor(a.ID, RoleAgent),
		Mounts: []dockerclient.Mount{
			{Type: dockerclient.MountTypeVolume, Source: a.WorkspaceVolume, Target: workspaceMount},
			{Type: dockerclient.MountTypeVolume, Source: a.ClaudeConfigVolume, Target: configMount},
		},
		Networks: []dockerclient.NetworkAttachment{
			// proxynet: the shared ollama, git-proxy and dependaproxy.
			{Name: m.cfg.ProxynetName},
			// this agent's private dinernet: its own DinD sidecar, and
			// nothing belonging to any other agent.
			{Name: a.DinernetName},
		},
		// Mirrors compose's stdin_open + tty. tmux itself does not need them
		// (the session gets its own pty), but they keep `docker attach` and an
		// interactive `docker exec` behaving the way an operator expects.
		TTY:       true,
		OpenStdin: true,
	}
	// The centralized file store, mounted as a per-agent subpath of the
	// shared filestore volume (ensureAgentFiles pre-created the subpath).
	// Appended AFTER the two volume mounts above.
	if m.files != nil {
		spec.Mounts = append(spec.Mounts, dockerclient.Mount{
			Type:    dockerclient.MountTypeVolume,
			Source:  m.cfg.FilestoreVolume,
			Target:  agentStoreMount,
			Subpath: filestore.AgentSubpath(a.ID),
		})
	}
	return spec
}

// agentEnv assembles the agent container's environment.
//
// This is the one sanctioned place config.Secret.Reveal is called: the values
// have to reach the container as plain strings, and every other path a Secret
// can take -- fmt, encoding/json, log/slog -- stays redacted.
func (m *Manager) agentEnv(a store.Agent, rb resolvedBackend) map[string]string {
	token := m.cfg.AgentToken.Reveal()

	env := map[string]string{
		// git-proxy legs, consumed by entrypoint.sh and the use-git-proxy
		// skill. This Bearer is NOT the GitHub PAT: git-proxy consumes it for
		// authentication and never forwards it.
		"AGENT_TOKEN": token,
		// The per-agent repo resolved at create time (request value, else
		// the operator's GithubRepo default, else empty for a bare agent).
		"GITHUB_REPO":          a.Repo,
		"GIT_PROXY_URL":        m.cfg.GitProxyURL,
		"GIT_PROXY_BROKER_URL": m.cfg.GitProxyBrokerURL,
		"GIT_PROXY_TOKEN":      token,
		"GIT_PROXY_HEADER":     "http.extraheader=Authorization: Bearer " + token,

		// DependaProxy. The *_URL values are what entrypoint.sh derives
		// .npmrc / pip.env / go.env from; the three below are what this
		// container's own npm/pip/go clients read directly.
		"DEPENDAPROXY_URL":         m.cfg.DependaproxyURL,
		"DEPENDAPROXY_PYPI_URL":    m.cfg.DependaproxyPyPIURL,
		"DEPENDAPROXY_GOPROXY_URL": m.cfg.DependaproxyGoproxyURL,
		"NPM_CONFIG_REGISTRY":      m.cfg.DependaproxyURL,
		"PIP_INDEX_URL":            m.cfg.DependaproxyPyPIURL + "/simple",
		"GOPROXY":                  m.cfg.DependaproxyGoproxyURL,

		// The docker client talks to THIS agent's sidecar, by its dinernet
		// alias, over plain HTTP.
		"DOCKER_HOST":       "tcp://" + dindAlias + ":" + dindTCPPort,
		"DOCKER_TLS_VERIFY": "",

		// Claude Code. CLAUDE_CONFIG_DIR is the claude-config volume, so
		// sessions and plugins survive a container recreate.
		"CLAUDE_CONFIG_DIR":              configMount,
		"CLAUDE_CODE_ATTRIBUTION_HEADER": "0",

		// tmux needs a terminal type even for a detached session.
		"TERM": "xterm-256color",
	}

	if a.DependaproxyDinernetIP.IsValid() {
		env["DEPENDAPROXY_DINERNET_IP"] = a.DependaproxyDinernetIP.String()
	}

	// The centralized file store's mount point, only when it is enabled.
	// entrypoint.sh keys the store-file skill install off this variable.
	if m.files != nil {
		env["AGENT_STORE_DIR"] = agentStoreMount
	}

	m.applyBackendEnv(env, rb)
	return env
}

// applyBackendEnv writes the model-routing / credential half of the agent's
// environment, which is the only part that differs between the two backends.
//
//   - ollama: point Claude Code at the shared Ollama daemon and map EVERY
//     model tier to this agent's models -- leaving sonnet/opus unmapped is
//     what makes Task and Explore subagents fail with "model may not exist"
//     against a non-Anthropic backend. ANTHROPIC_API_KEY is set empty so
//     Claude Code cannot silently fall back to the real cloud.
//   - anthropic: no base URL, no model overrides (real Anthropic defaults),
//     and ONLY the one of ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN the
//     operator's stored shared credential actually populates. The other is
//     left unset, not set empty: unlike the ollama path (where an empty
//     ANTHROPIC_API_KEY is deliberate, to block a silent cloud fallback), an
//     empty ANTHROPIC_API_KEY="" sitting next to a real CLAUDE_CODE_OAUTH_TOKEN
//     makes Claude Code skip the token and drop to an interactive login.
//
// The one carried-over quirk: if the operator cleared OllamaURL entirely
// (the historical "just use the real cloud with a static key" escape hatch),
// an ollama agent falls back to cfg.AnthropicAPIKey with no routing -- so
// that deployment shape keeps working without anyone having to switch every
// agent to the anthropic backend.
func (m *Manager) applyBackendEnv(env map[string]string, rb resolvedBackend) {
	switch rb.kind {
	case config.BackendAnthropic:
		// resolveBackend guarantees exactly one of these is non-empty. Set
		// only that one -- see this function's header for why a blank var is
		// not harmless on the anthropic path.
		if rb.apiKey != "" {
			env["ANTHROPIC_API_KEY"] = rb.apiKey
		}
		if rb.oauthToken != "" {
			env["CLAUDE_CODE_OAUTH_TOKEN"] = rb.oauthToken
		}

	case config.BackendOllama:
		if m.cfg.OllamaURL == "" {
			env["ANTHROPIC_API_KEY"] = m.cfg.AnthropicAPIKey.Reveal()
			return
		}
		env["ANTHROPIC_API_KEY"] = ""
		env["ANTHROPIC_BASE_URL"] = m.cfg.OllamaURL
		env["ANTHROPIC_AUTH_TOKEN"] = m.cfg.AnthropicAuthToken.Reveal()
		env["ANTHROPIC_MODEL"] = rb.model
		env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = rb.model
		env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = rb.fastModel
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = rb.fastModel
	}
}

// waitTmuxSession polls `tmux has-session -t main` inside the agent container
// until it answers 0.
//
// This is the create flow's real readiness signal. A running container proves
// nothing: entrypoint.sh may still be writing configuration, or tmux-boot.sh
// may have failed outright. An agent whose container runs but whose session
// never appeared is a broken agent, and marking it running would be a lie the
// UI would then repeat.
func (m *Manager) waitTmuxSession(ctx context.Context, a store.Agent) error {
	cmd := []string{"tmux", "has-session", "-t", tmuxSession}
	deadline := time.Now().Add(m.opts.TmuxReadyTimeout)
	var last string
	for {
		code, out, err := m.runExec(ctx, a.ContainerID, cmd)
		switch {
		case err != nil:
			last = err.Error()
		case code == 0:
			return nil
		default:
			last = fmt.Sprintf("exit code %d: %s", code, strings.TrimSpace(out))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the tmux %q session did not appear in agent container %q within %s (last check: %s)",
				tmuxSession, a.ContainerName, m.opts.TmuxReadyTimeout, last)
		}
		if err := sleepCtx(ctx, m.opts.PollInterval); err != nil {
			return err
		}
	}
}

// runExec runs one short command in a container and returns its exit code and
// combined output.
//
// The exec only STARTS on attach, and it only finishes once its output has
// been drained, so both are mandatory even when the output is not wanted.
func (m *Manager) runExec(ctx context.Context, containerID string, cmd []string) (int, string, error) {
	execID, err := m.docker.ExecCreate(ctx, containerID, dockerclient.ExecSpec{Cmd: cmd})
	if err != nil {
		return 0, "", fmt.Errorf("preparing %v in container %q: %w", cmd, containerID, err)
	}
	stream, err := m.docker.ExecAttach(ctx, execID)
	if err != nil {
		return 0, "", fmt.Errorf("attaching to %v in container %q: %w", cmd, containerID, err)
	}
	raw, readErr := io.ReadAll(stream)
	_ = stream.Close()
	if readErr != nil {
		return 0, "", fmt.Errorf("reading the output of %v in container %q: %w", cmd, containerID, readErr)
	}

	status, err := m.awaitExec(ctx, execID)
	if err != nil {
		return 0, "", err
	}
	return status.ExitCode, demuxBestEffort(raw), nil
}

// awaitExec polls until the exec process has finished, which is the only way
// to get its exit status.
func (m *Manager) awaitExec(ctx context.Context, execID string) (dockerclient.ExecStatus, error) {
	deadline := time.Now().Add(m.opts.ExecTimeout)
	for {
		st, err := m.docker.ExecInspect(ctx, execID)
		if err != nil {
			return dockerclient.ExecStatus{}, fmt.Errorf("inspecting exec %q: %w", execID, err)
		}
		if !st.Running {
			return st, nil
		}
		if time.Now().After(deadline) {
			return dockerclient.ExecStatus{}, fmt.Errorf("exec %q did not finish within %s", execID, m.opts.ExecTimeout)
		}
		if err := sleepCtx(ctx, m.opts.PollInterval); err != nil {
			return dockerclient.ExecStatus{}, err
		}
	}
}

// demuxBestEffort renders an exec's captured bytes as text. A non-TTY exec
// stream is Docker's multiplexed framing; anything else (a TTY exec, an
// in-memory test double) is already plain. The output here is only ever a
// diagnostic in an error message, so an unrecognised framing falls back to the
// raw bytes rather than failing the operation that produced it.
//
// stdcopy.StdCopy's own contract is that a source shorter than one 8-byte
// frame header is treated as a clean EOF: it returns zero bytes written and a
// NIL error, not an error the case above can catch. Confirmed against a real
// daemon: dockerclient.Docker.ExecAttach always attaches with Tty:true
// regardless of the exec's own creation-time TTY, so a short-output, non-TTY
// exec (like reading back a single pane's pid) gets streamed raw rather than
// multiplexed -- exactly the input this silently-empty case swallows. Without
// this fallback, that real output is lost rather than merely unparsed.
func demuxBestEffort(raw []byte) string {
	var stdout, stderr bytes.Buffer
	if err := dockerclient.DemuxStream(&stdout, &stderr, bytes.NewReader(raw)); err != nil {
		return string(raw)
	}
	if out := stdout.String() + stderr.String(); out != "" || len(raw) == 0 {
		return out
	}
	return string(raw)
}

// markRunning is the last create step: every resource exists and the tmux
// session answered.
func (m *Manager) markRunning(ctx context.Context, a *store.Agent) error {
	return m.stamp(ctx, a, "the running status", func(ag *store.Agent) {
		ag.Status = store.StatusRunning
		ag.ErrorMessage = ""
	})
}

// rollback undoes a failed create and gives the slot back.
//
// It runs on a context detached from the caller's, with its own timeout: the
// most common reason a create fails is that its context was cancelled, and a
// rollback that inherited that cancellation would leak every resource it was
// created to remove. Failures here are logged, never returned -- the caller
// already has the error that matters, and anything rollback could not remove
// still carries the managed labels for the next reconcile pass to find.
func (m *Manager) rollback(ctx context.Context, a store.Agent, cause error) {
	m.log.ErrorContext(ctx, "agent create failed; tearing down and releasing the slot",
		"agent_id", a.ID, "error", cause)

	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.opts.TeardownTimeout)
	defer cancel()

	if err := m.teardown(tctx, a); err != nil {
		m.log.ErrorContext(tctx, "create rollback left docker resources behind; the next reconcile pass will retry them",
			"agent_id", a.ID, "error", err)
	}
	// The record goes LAST and unconditionally: a failed attempt must never
	// permanently consume one of the MAX_AGENTS slots.
	if err := m.store.Delete(tctx, a.ID); err != nil {
		m.log.ErrorContext(tctx, "create rollback could not delete the agent record; its slot stays reserved until the next reconcile",
			"agent_id", a.ID, "error", err)
	}
}

// stamp applies mutate to the stored record and refreshes a in place, so the
// caller always holds what is actually persisted.
func (m *Manager) stamp(ctx context.Context, a *store.Agent, what string, mutate func(*store.Agent)) error {
	updated, err := m.store.Update(ctx, a.ID, func(ag *store.Agent) error {
		mutate(ag)
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording %s for agent %q: %w", what, a.ID, err)
	}
	*a = updated
	return nil
}

// labelsFor is the label set every resource this package creates carries.
func labelsFor(id string, role Role) map[string]string {
	return map[string]string{
		LabelManaged: LabelManagedValue,
		LabelAgentID: id,
		LabelRole:    string(role),
	}
}

// managedSelector matches every resource any docker-operator manages,
// regardless of which agent it belongs to. It is what the reconcile pass
// lists by.
func managedSelector() map[string]string {
	return map[string]string{LabelManaged: LabelManagedValue}
}

// The per-agent resource names, matching the plan's resource-naming section.
// They are pure functions of the agent ID so that teardown still works for a
// record that crashed before its names were stamped in.

func agentContainerName(id string) string     { return resourcePrefix + "-agent-" + id }
func dindContainerName(id string) string      { return resourcePrefix + "-dind-" + id }
func dinernetName(id string) string           { return agentContainerName(id) + "-dinernet" }
func workspaceVolumeName(id string) string    { return agentContainerName(id) + "-workspace" }
func claudeConfigVolumeName(id string) string { return agentContainerName(id) + "-claude-config" }
func dindCacheVolumeName(id string) string    { return agentContainerName(id) + "-dind-cache" }

// sleepCtx sleeps for d, or returns early with ctx's error if it is cancelled
// first. Every poll loop in this package uses it so a cancelled create stops
// promptly instead of at the next tick.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
