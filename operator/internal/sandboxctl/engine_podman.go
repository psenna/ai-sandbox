package sandboxctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

const (
	podmanDockerAPIVersion       = "v1.41"
	podmanTeardownStopSeconds    = 5
	podmanTeardownAttempts       = 3
	podmanTeardownAttemptDelay   = 2 * time.Second
	podmanTeardownTotalTimeout   = 30 * time.Second
	podmanTeardownRequestTimeout = 15 * time.Second
)

// podmanTeardown stops and removes every workload container the pod's rootless
// podman engine (#24) is running, over podman's Docker-COMPATIBLE REST API on
// pod loopback. Plain net/http against the documented v1.41 Docker API, NOT
// the docker/podman client SDKs: this binary ships in a distroless image and
// the operator module's go.mod has no container-client dependency, and the
// three calls needed here (list, stop, remove) are trivial.
//
// Explicitly OUT OF SCOPE, not an oversight: pruning images, volumes and
// networks. The graph root is an emptyDir that dies with the pod, and named
// volumes/networks are not archived.
type podmanTeardown struct {
	baseURL string // http://127.0.0.1:2375
	client  *http.Client
	log     logr.Logger
	sleep   func(context.Context, time.Duration) error // injectable; tests pass a no-op
}

// dockerContainer is the subset of the Docker API's /containers/json entry
// this teardown needs.
type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
}

// newPodmanTeardown parses endpoint ("tcp://host:port", also accepting
// "http://"/"https://") into a podmanTeardown. Returns an error on an
// unparseable or schemeless endpoint -- NewEngineTeardown wraps that in
// failedEngineTeardown, so the pod's Teardown call fails closed rather than
// panicking or silently no-oping.
func newPodmanTeardown(endpoint string, log logr.Logger) (EngineTeardown, error) {
	base, err := podmanBaseURL(endpoint)
	if err != nil {
		return nil, err
	}
	return &podmanTeardown{
		baseURL: base,
		client:  &http.Client{Timeout: podmanTeardownRequestTimeout},
		log:     log,
		sleep:   defaultSleep,
	}, nil
}

// podmanBaseURL normalizes endpoint into an http(s):// base URL with no
// trailing slash. "tcp://" (what render.PodmanDockerHost and
// sidecarSnapshotArgs' --engine-endpoint actually emit) maps to "http://";
// "http://"/"https://" pass through unchanged for direct/test use.
func podmanBaseURL(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("sandboxctl: engine-endpoint is required for the rootless-podman engine")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("sandboxctl: parsing engine-endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "tcp":
		u.Scheme = "http"
	case "http", "https":
		// pass through
	default:
		return "", fmt.Errorf("sandboxctl: engine-endpoint %q has unsupported scheme %q (want tcp:// or http(s)://)", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("sandboxctl: engine-endpoint %q has no host", endpoint)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// Teardown lists every container the pod's podman engine knows about, stops
// and removes each in sorted-name order, and reports what happened.
func (p *podmanTeardown) Teardown(ctx context.Context) (TeardownReport, error) {
	ctx, cancel := context.WithTimeout(ctx, podmanTeardownTotalTimeout)
	defer cancel()

	containers, err := p.listContainers(ctx)
	if err != nil {
		// Connection refused / no listener: podman workload containers
		// cannot outlive the service that started them (they run inside
		// podman's own netns/userns, torn down with it), so there is
		// genuinely nothing to tear down. This is what makes the recovery
		// Job path (which renders --engine=none and never reaches this code
		// at all) and "freeze before the podman sidecar ever started" safe.
		// Any OTHER error (timeout, 5xx, malformed body) means the engine IS
		// alive but wedged or misbehaving, and a wedged-but-alive engine may
		// still have containers writing to /workspace -- fail closed.
		if isConnRefused(err) {
			return TeardownReport{
				Engine: "rootless-podman",
				Note:   fmt.Sprintf("engine API at %s is not listening; podman workload containers cannot outlive the service that started them, so there is nothing to tear down", p.baseURL),
			}, nil
		}
		return TeardownReport{}, fmt.Errorf("sandboxctl: listing podman containers: %w", err)
	}

	sort.Slice(containers, func(i, j int) bool {
		return containerName(containers[i]) < containerName(containers[j])
	})

	names := make([]string, 0, len(containers))
	for _, c := range containers {
		name := containerName(c)
		if err := p.stopContainer(ctx, c.ID); err != nil {
			return TeardownReport{}, fmt.Errorf("sandboxctl: stopping container %s: %w", name, err)
		}
		if err := p.removeContainer(ctx, c.ID); err != nil {
			return TeardownReport{}, fmt.Errorf("sandboxctl: removing container %s: %w", name, err)
		}
		names = append(names, name)
	}

	note := fmt.Sprintf("stopped and removed %d workload container(s) via the podman Docker-compatible API at %s", len(names), p.baseURL)
	if len(names) == 0 {
		note = fmt.Sprintf("no workload containers found via the podman Docker-compatible API at %s", p.baseURL)
	}
	return TeardownReport{Engine: "rootless-podman", Containers: names, Note: note}, nil
}

// containerName strips the Docker API's leading "/" from a container's
// first name, falling back to its ID if it somehow has no name.
func containerName(c dockerContainer) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// listContainers GETs /containers/json?all=true, retrying up to
// podmanTeardownAttempts times on any error EXCEPT connection refused, which
// returns immediately (see Teardown's doc comment on why).
func (p *podmanTeardown) listContainers(ctx context.Context) ([]dockerContainer, error) {
	path := fmt.Sprintf("/%s/containers/json?all=true", podmanDockerAPIVersion)
	var lastErr error
	for attempt := 0; attempt < podmanTeardownAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			if isConnRefused(err) {
				return nil, err
			}
			lastErr = err
		} else {
			containers, decodeErr := decodeContainerList(resp)
			if decodeErr == nil {
				return containers, nil
			}
			lastErr = decodeErr
		}
		if attempt < podmanTeardownAttempts-1 {
			if serr := p.sleep(ctx, podmanTeardownAttemptDelay); serr != nil {
				return nil, serr
			}
		}
	}
	return nil, lastErr
}

func decodeContainerList(resp *http.Response) ([]dockerContainer, error) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("containers/json: unexpected status %d", resp.StatusCode)
	}
	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("decoding containers/json response: %w", err)
	}
	return containers, nil
}

// stopContainer POSTs .../stop?t=<podmanTeardownStopSeconds>. 204 and 304
// (already stopped) are success; 404 (already gone) is treated as success
// too -- a container that disappeared between list and stop needs no more
// tearing down.
func (p *podmanTeardown) stopContainer(ctx context.Context, id string) error {
	path := fmt.Sprintf("/%s/containers/%s/stop?t=%d", podmanDockerAPIVersion, id, podmanTeardownStopSeconds)
	return p.doWithRetry(ctx, http.MethodPost, path, []int{http.StatusNoContent, http.StatusNotModified, http.StatusNotFound})
}

// removeContainer DELETEs with force=true (kill if still running) and
// v=true (remove anonymous volumes too). 204 and 404 (already gone) are
// both success.
func (p *podmanTeardown) removeContainer(ctx context.Context, id string) error {
	path := fmt.Sprintf("/%s/containers/%s?force=true&v=true", podmanDockerAPIVersion, id)
	return p.doWithRetry(ctx, http.MethodDelete, path, []int{http.StatusNoContent, http.StatusNotFound})
}

// doWithRetry issues method against path (relative to baseURL), retrying up
// to podmanTeardownAttempts times on a network error or a status not in
// okStatuses.
func (p *podmanTeardown) doWithRetry(ctx context.Context, method, path string, okStatuses []int) error {
	var lastErr error
	for attempt := 0; attempt < podmanTeardownAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, nil)
		if err != nil {
			return err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			status := resp.StatusCode
			_ = resp.Body.Close()
			if statusIn(status, okStatuses) {
				return nil
			}
			lastErr = fmt.Errorf("%s %s: unexpected status %d", method, path, status)
		}
		if attempt < podmanTeardownAttempts-1 {
			if serr := p.sleep(ctx, podmanTeardownAttemptDelay); serr != nil {
				return serr
			}
		}
	}
	return lastErr
}

func statusIn(status int, ok []int) bool {
	for _, s := range ok {
		if status == s {
			return true
		}
	}
	return false
}

// isConnRefused reports whether err is (or wraps) syscall.ECONNREFUSED --
// net/http's client wraps dial errors through *url.Error and *net.OpError,
// both of which implement Unwrap, so errors.Is traverses the whole chain.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
