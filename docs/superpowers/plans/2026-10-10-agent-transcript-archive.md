# Agent Transcript Archive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When an agent is deleted, copy its session history (Claude Code's
JSONL transcripts, or opencode's raw output log) into a new `transcripts/<id>/`
area of the centralized file store, alongside a `metadata.json` snapshot of
the agent's identity, so the history survives for later human review and
eventual automated learning.

**Architecture:** One new thin wrapper in `internal/dockerclient`
(`CopyFromContainer`, mirroring the package's existing `VolumeUsage`/
`ContainerStats` wrapper style) exposes Docker's copy-from-container API,
which reads a container's filesystem via a tar stream with no running
process required. One new file in `internal/agent`
(`transcript.go`) adds a single best-effort step,
`archiveTranscript`, wired into `teardown()` before any Docker resource is
removed. It extracts the tar Docker returns into the file store via
`internal/filestore`'s existing `Save`/`Mkdir` (both already path-generic;
no new filestore methods needed beyond one new path constant).

**Tech Stack:** Go 1.26, the vendored `github.com/moby/moby/client` v0.5.1,
`archive/tar` (stdlib), the existing `internal/filestore.Store` (an
`os.Root`-hardened file store), the existing `dockerclienttest.Fake` +
`TestConformance` dual-leg test harness.

**Spec:**
`docs/superpowers/specs/2026-10-10-agent-transcript-archive-design.md`

## Global Constraints

- Go 1.26.9 (this module's `go.mod`); no new third-party dependencies — this
  plan only adds stdlib (`archive/tar`) usage on top of already-vendored
  `github.com/moby/moby/client`.
- Nothing outside `internal/dockerclient` may import the moby SDK directly
  (package doc comment on `Client`) — the new method lives entirely behind
  the existing `Client`/`ContainerClient` interface.
- `archiveTranscript` must never return an error to its caller and must
  never block or fail `Delete` — any failure is logged at `Warn` and
  swallowed (spec decision 7).
- Transcripts live in a new top-level file-store area, `transcripts/<id>/`,
  never under `agents/<id>/` — `purge_files=true` must never touch them
  (spec decision 4).
- Claude Code: copy the *whole* `$CLAUDE_CONFIG_DIR/projects/` directory
  wholesale, not a single cwd-encoded subdirectory (spec, "What gets
  captured, and how"). opencode: copy `wsbridge.OutputLogPath` only (spec
  decision 2).
- Archiving is skipped entirely when the file store is disabled (`m.files ==
  nil`) — the same governance every other file-store-dependent feature in
  this codebase already follows (spec decision 9).
- Run `make all` (or at least `make web-embed-check`) before any PR — N/A
  here since no `web/` or `scripts/dind-init.sh` file is touched by this
  plan, but the project's `make all` gate (`vet fmt-check lint test
  skill-check dind-init-check web-test web-embed-check`) still must pass.

## Review Focus

- **A tar stream with zero entries (an agent deleted before writing any
  session or output).** `extractTranscriptDir`/`extractTranscriptFile` must
  return cleanly with nothing written, not hang or error — covered in Task
  2's empty-tar unit test.
- **`CopyFromContainer` returning `IsNotFound`** (the source path was never
  created — common for a freshly-created, immediately-deleted agent).
  `archiveTranscript` must treat this as the unremarkable common case (no
  Warn log), distinct from a real I/O error — covered in Task 2.
- **A tar entry whose type is not a regular file** (a directory header, a
  symlink — Docker's directory copy always includes at least one directory
  header for the copied directory itself). The extractor must skip these
  silently rather than trying to `Save` a directory as a file — covered in
  Task 1's fake-leg assertions and Task 2's hand-built-tar unit test.
- **Two different agents' containers, same relative tar paths.** Since each
  agent's `ClaudeConfigVolume` is a separate Docker volume, and the
  destination is always scoped under `transcripts/<a.ID>/`, there is no
  cross-agent collision — but Task 2's test must create two agents and
  assert each one's archive is independent, since this is exactly the kind
  of invariant that silently breaks if a path ever gets de-scoped by
  accident.
- **`Delete` called on an agent with no `ContainerID` *and* no
  `ContainerName`** (a record that crashed before `Create` stamped
  anything). `archiveTranscript` must no-op (nothing to copy from), not
  error or panic — covered in Task 2.
- **`PurgeAgentFiles` called after `Delete`.** It must never remove
  `transcripts/<id>/` — that would silently defeat the whole feature the
  first time a caller passes `purge_files=true` — covered in Task 2.

---

## Task 1: `internal/dockerclient.CopyFromContainer`

**Files:**
- Modify: `internal/dockerclient/client.go` (add `CopyFromContainer` to the
  `ContainerClient` interface, ~line 172; add `(*Docker) CopyFromContainer`
  near `ContainerInspect`, ~line 801)
- Modify: `internal/dockerclient/dockerclienttest/fake.go` (add
  `copyPaths` field to `containerRecord`, ~line 123; add
  `SetContainerFile`, `SetContainerDir`, `CopyFromContainer`,
  `buildFileTar`, `buildDirTar`; add `OpCopyFromContainer` to the `Op` const
  block, ~line 65)
- Modify: `internal/dockerclient/conformance_test.go` (add
  `CopyFromContainerLifecycle` to `conformanceCases`, ~line 701; add
  `assertTarEntry` helper)

**Interfaces:**
- Produces: `dockerclient.Client.CopyFromContainer(ctx context.Context, id,
  path string) (io.ReadCloser, error)` — returned by both `*Docker` and
  `*dockerclienttest.Fake`. Always returns a tar stream (never raw bytes),
  satisfying `dockerclient.IsNotFound` for a missing container or an
  unseeded/nonexistent path. The caller owns and must `Close` the returned
  `io.ReadCloser`.
- Produces (test-only, `dockerclienttest` package):
  `(*Fake) SetContainerFile(idOrName, path string, content []byte) error`
  and `(*Fake) SetContainerDir(idOrName, path string, files map[string]string)
  error` — seed what `CopyFromContainer` returns for a given container +
  path. Task 2's tests call these directly.
- Consumes: nothing new — `d.api` is already the moby SDK client embedded in
  `*Docker` (see `VolumeUsage`'s `d.api.DiskUsage(...)` for the calling
  convention), and `wrapErr(kind, name string, err error) error` already
  exists in `client.go`.

- [ ] **Step 1: Write the failing conformance test**

Add this case to the `conformanceCases` slice in
`internal/dockerclient/conformance_test.go`, right after
`VolumeUsageLifecycle` (the last entry, just before the closing `}`):

```go
	// CopyFromContainerLifecycle pins the contract internal/agent's
	// transcript-archive feature is built on
	// (docs/superpowers/specs/2026-10-10-agent-transcript-archive-design.md):
	// reading a file or directory out of a container via the same mechanism
	// `docker cp` uses, which (unlike every other in-container read in this
	// codebase) needs no running process -- it works identically on a
	// RUNNING and a STOPPED container -- and which (confirmed against
	// Docker 27.5.1) always returns a tar stream, with a copied directory's
	// entries prefixed by that directory's OWN basename (copying
	// ".../testdir" yields an entry named "testdir/file.txt", not a bare
	// "file.txt"). internal/agent's extraction logic depends on that exact
	// prefixing convention to land files at the right destination without
	// doubling the directory name -- this is the one case in this suite
	// asserting an EXACT tar shape rather than plausibility-only, because
	// that shape is a real dependency, not incidental.
	{name: "CopyFromContainerLifecycle", run: func(t *testing.T, f factory, c dockerclient.Client) {
		ctx := context.Background()
		ctrName := uniqueName(f, t) + "-ctr"

		spec := dockerclient.ContainerSpec{Name: ctrName, Image: "alpine:latest", Cmd: []string{"sleep", "300"}}
		if f.name == "docker" {
			// The fake does not execute Cmd; the real daemon leg needs a
			// real file on disk, so write it as the container's own entry
			// point before anything else can race it.
			spec.Cmd = []string{"sh", "-c", "mkdir -p /tmp/testdir && printf hello-transcript > /tmp/testdir/file.txt && sleep 300"}
		}
		id, err := c.ContainerCreate(ctx, spec)
		if err != nil {
			if isImageNotFoundErr(err) {
				t.Skipf("alpine:latest not available and this client cannot pull images (by design, #63): %v", err)
			}
			t.Fatalf("ContainerCreate: %v", err)
		}
		t.Cleanup(func() { _ = c.ContainerRemove(context.Background(), id) })

		if f.name == "fake" {
			fake := c.(*dockerclienttest.Fake)
			if err := fake.SetContainerDir(id, "/tmp/testdir", map[string]string{"file.txt": "hello-transcript"}); err != nil {
				t.Fatalf("SetContainerDir: %v", err)
			}
		}

		if err := c.ContainerStart(ctx, id); err != nil {
			t.Fatalf("ContainerStart: %v", err)
		}

		// Real: the shell write races container startup; retry briefly
		// rather than a single arbitrary sleep. Fake: already seeded above,
		// so this succeeds on the first attempt.
		var content io.ReadCloser
		for attempt := 0; attempt < 20; attempt++ {
			content, err = c.CopyFromContainer(ctx, id, "/tmp/testdir")
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("CopyFromContainer(running, existing dir): %v", err)
		}
		assertTarEntry(t, content, "testdir/file.txt", "hello-transcript")

		if err := c.ContainerStop(ctx, id, 5*time.Second); err != nil {
			t.Fatalf("ContainerStop: %v", err)
		}

		// The whole point of this method: it must still work on a STOPPED
		// container.
		content, err = c.CopyFromContainer(ctx, id, "/tmp/testdir")
		if err != nil {
			t.Fatalf("CopyFromContainer(stopped, existing dir): %v", err)
		}
		assertTarEntry(t, content, "testdir/file.txt", "hello-transcript")

		if _, err := c.CopyFromContainer(ctx, id, "/tmp/testdir/does-not-exist.txt"); !dockerclient.IsNotFound(err) {
			t.Errorf("CopyFromContainer(missing path) error = %v, want IsNotFound", err)
		}

		if _, err := c.CopyFromContainer(ctx, uniqueName(f, t)+"-missing", "/tmp/testdir"); !dockerclient.IsNotFound(err) {
			t.Errorf("CopyFromContainer(missing container) error = %v, want IsNotFound", err)
		}
	}},
```

Add this helper to the same file, near `findVolumeUsage` at the bottom:

```go
// assertTarEntry reads r as a tar stream (closing it when done) and fails t
// unless it contains a regular-file entry named exactly wantName with
// content wantContent.
func assertTarEntry(t *testing.T, r io.ReadCloser, wantName, wantContent string) {
	t.Helper()
	defer r.Close()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("tar stream has no entry named %q", wantName)
		}
		if err != nil {
			t.Fatalf("reading tar stream: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != wantName {
			continue
		}
		got, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading tar entry %q: %v", hdr.Name, err)
		}
		if string(got) != wantContent {
			t.Fatalf("tar entry %q content = %q, want %q", hdr.Name, got, wantContent)
		}
		return
	}
}
```

Add `"archive/tar"` to the import block at the top of
`conformance_test.go` (alongside the existing `"context"`, `"slices"`, etc).

- [ ] **Step 2: Run it to verify it fails**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host "dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e DOCKER_HOST=tcp://docker:2375 \
  golang:1.26 go test ./internal/dockerclient/... -run TestConformance/docker/CopyFromContainerLifecycle -v
```

Expected: a compile error (`c.CopyFromContainer undefined`,
`fake.SetContainerDir undefined`) — the method does not exist yet.

- [ ] **Step 3: Add `CopyFromContainer` to the `ContainerClient` interface
  and implement it on `*Docker`**

In `internal/dockerclient/client.go`, add to the `ContainerClient` interface
(right after `ContainerStats`, the interface's last method):

```go
	// CopyFromContainer reads path out of the container's filesystem and
	// returns it as a tar stream -- the same mechanism `docker cp` uses.
	// Unlike every exec-based read in this package, it needs no running
	// process: it works identically whether the container is running or
	// stopped. The result ALWAYS tar-wraps its content, even when path names
	// a single file (confirmed against the vendored
	// github.com/moby/moby/client v0.5.1's CopyFromContainer). The caller
	// owns the returned stream and must Close it. A missing container or a
	// path the container does not have satisfies IsNotFound.
	CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, error)
```

Implement it on `*Docker`, right after `ContainerInspect`:

```go
// CopyFromContainer reads path out of id's filesystem and returns it as a
// tar stream. See the ContainerClient interface doc for the full contract.
func (d *Docker) CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, error) {
	res, err := d.api.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, wrapErr("container", id, err)
	}
	return res.Content, nil
}
```

- [ ] **Step 4: Implement the fake**

In `internal/dockerclient/dockerclienttest/fake.go`:

Add to the `Op` const block (after `OpImageRemove`):

```go
	OpCopyFromContainer Op = "CopyFromContainer"
```

Add a field to `containerRecord` (after `stats`):

```go
	// copyPaths holds the pre-built tar bytes CopyFromContainer returns for
	// a given path, seeded by SetContainerFile/SetContainerDir. nil/missing
	// means "not seeded", which CopyFromContainer reports as ErrNotFound --
	// the fake has no real filesystem to consult, so every path a test
	// cares about must be seeded explicitly.
	copyPaths map[string][]byte
```

Add these methods and helpers, near `SetStats`:

```go
// SetContainerFile seeds CopyFromContainer's response for path on an
// existing container, found by ID or name, as if path named a single file:
// the returned tar carries one entry named path's base, holding content --
// mirroring the real daemon's single-file copy-out convention (pinned by
// the conformance suite's CopyFromContainerLifecycle case).
func (f *Fake) SetContainerFile(idOrName, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.resolveContainer(idOrName)
	if !ok {
		return fmt.Errorf("container %q: %w", idOrName, dockerclient.ErrNotFound)
	}
	if c.copyPaths == nil {
		c.copyPaths = map[string][]byte{}
	}
	c.copyPaths[path] = buildFileTar(stdpath.Base(path), content)
	return nil
}

// SetContainerDir seeds CopyFromContainer's response for path on an
// existing container, found by ID or name, as if path named a directory:
// the returned tar carries one entry per entry in files, each named
// path's-basename + "/" + <key>, holding <value> -- mirroring the real
// daemon's directory copy-out convention of prefixing every entry with the
// copied directory's own basename (pinned by the conformance suite's
// CopyFromContainerLifecycle case).
func (f *Fake) SetContainerDir(idOrName, path string, files map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.resolveContainer(idOrName)
	if !ok {
		return fmt.Errorf("container %q: %w", idOrName, dockerclient.ErrNotFound)
	}
	if c.copyPaths == nil {
		c.copyPaths = map[string][]byte{}
	}
	c.copyPaths[path] = buildDirTar(stdpath.Base(path), files)
	return nil
}

// CopyFromContainer returns the tar stream seeded for path on an existing
// container by SetContainerFile or SetContainerDir, found by ID or name.
func (f *Fake) CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, error) {
	if err := f.call(OpCopyFromContainer, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.resolveContainer(id)
	if !ok {
		return nil, fmt.Errorf("container %q: %w", id, dockerclient.ErrNotFound)
	}
	data, ok := c.copyPaths[path]
	if !ok {
		return nil, fmt.Errorf("path %q in container %q: %w", path, id, dockerclient.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// buildFileTar returns a tar archive with one regular-file entry named name
// holding content -- the shape CopyFromContainer always returns, even for a
// single-file source.
func buildFileTar(name string, content []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write(content)
	_ = tw.Close()
	return buf.Bytes()
}

// buildDirTar returns a tar archive with one regular-file entry per entry
// in files, each named dirName+"/"+<key>, written in sorted key order for a
// deterministic stream.
func buildDirTar(dirName string, files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		_ = tw.WriteHeader(&tar.Header{Name: dirName + "/" + name, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	return buf.Bytes()
}
```

Add `"archive/tar"`, `"bytes"`, and `"path"` (aliased `stdpath` to avoid any
future collision with a local variable named `path`, matching this file's
existing style of aliasing when a stdlib name is common) to the import
block:

```go
	"archive/tar"
	"bytes"
	stdpath "path"
```

- [ ] **Step 5: Run the conformance test and verify it passes on both legs**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host "dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e DOCKER_HOST=tcp://docker:2375 \
  golang:1.26 go test ./internal/dockerclient/... -run TestConformance/.*/CopyFromContainerLifecycle -v
```

Expected: `PASS` for both `TestConformance/fake/CopyFromContainerLifecycle`
and `TestConformance/docker/CopyFromContainerLifecycle` (the latter requires
`alpine:latest` present on the daemon; if it `SKIP`s with "not available",
run `docker pull alpine:latest` against the DinD daemon first, then re-run).

- [ ] **Step 6: Run the full package test suite and vet**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host "dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e DOCKER_HOST=tcp://docker:2375 \
  golang:1.26 sh -c "go vet ./... && go test ./internal/dockerclient/... -race"
```

Expected: `PASS`, no vet warnings.

- [ ] **Step 7: Commit**

```bash
git add internal/dockerclient/client.go internal/dockerclient/dockerclienttest/fake.go internal/dockerclient/conformance_test.go
git commit -m "$(cat <<'EOF'
feat(dockerclient): add CopyFromContainer

Thin wrapper over the moby SDK's copy-from-container API (the mechanism
behind `docker cp`), which reads a container's filesystem via a tar
stream with no running process required -- needed to archive an
agent's session transcripts at delete time, since Delete is valid from
a stopped or errored container, not just a running one.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `internal/agent.archiveTranscript`

**Files:**
- Create: `internal/agent/transcript.go`
- Create: `internal/agent/transcript_test.go`
- Modify: `internal/agent/delete.go` (wire `archiveTranscript` into
  `teardown`, ~line 97)
- Modify: `internal/filestore/filestore.go` (add `TranscriptsDir` constant,
  ~line 61)

**Interfaces:**
- Consumes: `dockerclient.Client.CopyFromContainer` and
  `dockerclient.IsNotFound` from Task 1; `filestore.Store.Mkdir(rel string)
  error` and `filestore.Store.Save(rel string, r io.Reader, maxBytes int64)
  (filestore.Entry, error)` (both already exist, unchanged); `HarnessOf(a
  store.Agent) string` and `firstNonEmpty(vals ...string) string` (both
  already exist in `internal/agent/harness.go` / `delete.go`); `configMount`
  (already exists in `internal/agent/create.go`, `"/home/node/.claude-sandbox"`);
  `wsbridge.OutputLogPath` (already exists,
  `"/workspace/.agent-output.log"`).
- Produces: `(m *Manager) archiveTranscript(ctx context.Context, a
  store.Agent)` — no return value, by design (spec decision 7: a function
  with nothing to return cannot be made to block its caller on failure).
  `filestore.TranscriptsDir` (the constant `"transcripts"`, alongside the
  existing `AgentsDir`/`SharedDir`).

- [ ] **Step 1: Add the `TranscriptsDir` constant**

In `internal/filestore/filestore.go`, change:

```go
// AgentsDir is the top-level directory under the store root that holds every
// agent's private files, at AgentsDir/<id>/. SharedDir is the other top-level
// directory: a common area mounted read-only into every agent and writable
// only by the operator (through this package's own API).
const (
	AgentsDir = "agents"
	SharedDir = "shared"
)
```

to:

```go
// AgentsDir is the top-level directory under the store root that holds every
// agent's private files, at AgentsDir/<id>/. SharedDir is the other top-level
// directory: a common area mounted read-only into every agent and writable
// only by the operator (through this package's own API). TranscriptsDir is a
// third, separate top-level directory, at TranscriptsDir/<id>/, holding a
// deleted agent's archived session history -- deliberately NOT inside
// AgentsDir, so `purge_files=true` (which only ever reaches AgentsDir/<id>/)
// can never delete it.
const (
	AgentsDir       = "agents"
	SharedDir       = "shared"
	TranscriptsDir  = "transcripts"
)
```

(Run `gofmt` in Step 6 below to fix the const block's alignment.)

- [ ] **Step 2: Write the failing tests**

Create `internal/agent/transcript_test.go`:

```go
package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
)

func TestArchiveTranscript_ClaudeCode(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, f, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.SetContainerDir(a.ContainerID, configMount+"/projects", map[string]string{
		"enc-cwd/session-1.jsonl": `{"type":"message"}`,
	}); err != nil {
		t.Fatalf("SetContainerDir: %v", err)
	}

	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	dir := filepath.Join(cfg.FilestoreDir, "transcripts", a.ID)
	got, err := os.ReadFile(filepath.Join(dir, "projects", "enc-cwd", "session-1.jsonl"))
	if err != nil {
		t.Fatalf("reading archived transcript: %v", err)
	}
	if string(got) != `{"type":"message"}` {
		t.Errorf("archived content = %q, want the seeded session content", got)
	}

	metaBytes, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatalf("reading metadata.json: %v", err)
	}
	var meta transcriptMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("unmarshal metadata.json: %v", err)
	}
	if meta.ID != a.ID || meta.Harness != config.HarnessClaudeCode {
		t.Errorf("metadata = %+v, want ID=%q Harness=%q", meta, a.ID, config.HarnessClaudeCode)
	}
}

func TestArchiveTranscript_OpenCode(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, f, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{Harness: config.HarnessOpenCode})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.SetContainerFile(a.ContainerID, wsbridge.OutputLogPath, []byte("agent output\n")); err != nil {
		t.Fatalf("SetContainerFile: %v", err)
	}

	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	dir := filepath.Join(cfg.FilestoreDir, "transcripts", a.ID)
	got, err := os.ReadFile(filepath.Join(dir, "output.log"))
	if err != nil {
		t.Fatalf("reading archived output.log: %v", err)
	}
	if string(got) != "agent output\n" {
		t.Errorf("archived content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects")); !os.IsNotExist(err) {
		t.Errorf("opencode archive has a projects/ dir, want none: %v", err)
	}
}

// TestArchiveTranscript_CopyFailureNeverBlocksDelete pins spec decision 7:
// a CopyFromContainer failure is logged and swallowed, never propagated.
// metadata.json (written before the copy is attempted) still lands, so a
// partial archive is still diagnosable.
func TestArchiveTranscript_CopyFailureNeverBlocksDelete(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, f, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Fail(dockerclienttest.OpCopyFromContainer, errors.New("boom"))

	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v, want nil (archiving must never block delete)", err)
	}

	dir := filepath.Join(cfg.FilestoreDir, "transcripts", a.ID)
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); err != nil {
		t.Errorf("metadata.json missing after a copy failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects")); !os.IsNotExist(err) {
		t.Errorf("projects/ present despite the injected copy failure: %v", err)
	}
}

// TestArchiveTranscript_MissingSourceIsQuiet pins the Review Focus item: a
// CopyFromContainer IsNotFound result (nothing was ever written) is the
// unremarkable common case, not a failure -- Delete must still succeed and
// still write metadata.json.
func TestArchiveTranscript_MissingSourceIsQuiet(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No SetContainerDir/SetContainerFile call: the fake has nothing seeded
	// for configMount+"/projects", so CopyFromContainer reports IsNotFound.

	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	dir := filepath.Join(cfg.FilestoreDir, "transcripts", a.ID)
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); err != nil {
		t.Errorf("metadata.json missing: %v", err)
	}
}

// TestArchiveTranscript_FilestoreDisabledIsNoop pins spec decision 9.
func TestArchiveTranscript_FilestoreDisabledIsNoop(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Nothing to assert beyond "did not panic and did not block Delete" --
	// there is no file store to inspect.
}

// TestArchiveTranscript_TwoAgentsStayIndependent guards against a path
// accidentally de-scoped to something shared across agents.
func TestArchiveTranscript_TwoAgentsStayIndependent(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, f, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a1, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create a1: %v", err)
	}
	if err := f.SetContainerDir(a1.ContainerID, configMount+"/projects", map[string]string{"x/one.jsonl": "one"}); err != nil {
		t.Fatal(err)
	}
	a2, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create a2: %v", err)
	}
	if err := f.SetContainerDir(a2.ContainerID, configMount+"/projects", map[string]string{"x/two.jsonl": "two"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Delete(ctx, a1.ID); err != nil {
		t.Fatalf("Delete a1: %v", err)
	}
	if err := m.Delete(ctx, a2.ID); err != nil {
		t.Fatalf("Delete a2: %v", err)
	}

	got1, err := os.ReadFile(filepath.Join(cfg.FilestoreDir, "transcripts", a1.ID, "projects", "x", "one.jsonl"))
	if err != nil || string(got1) != "one" {
		t.Errorf("a1 archive = %q, %v, want %q, nil", got1, err, "one")
	}
	got2, err := os.ReadFile(filepath.Join(cfg.FilestoreDir, "transcripts", a2.ID, "projects", "x", "two.jsonl"))
	if err != nil || string(got2) != "two" {
		t.Errorf("a2 archive = %q, %v, want %q, nil", got2, err, "two")
	}
}

// TestArchiveTranscript_SurvivesPurgeFiles pins the spec's "Interaction with
// purge_files=true: none, by construction" section -- PurgeAgentFiles only
// ever reaches agents/<id>/, a different top-level area from
// transcripts/<id>/, so purging an agent's own files must never remove its
// archived transcript.
func TestArchiveTranscript_SurvivesPurgeFiles(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, f, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.SetContainerDir(a.ContainerID, configMount+"/projects", map[string]string{"x/one.jsonl": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.PurgeAgentFiles(ctx, a.ID); err != nil {
		t.Fatalf("PurgeAgentFiles: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(cfg.FilestoreDir, "transcripts", a.ID, "projects", "x", "one.jsonl"))
	if err != nil || string(got) != "one" {
		t.Errorf("transcript archive = %q, %v, want %q, nil (purge_files must never touch it)", got, err, "one")
	}
}

// TestExtractTranscriptDir_SkipsNonRegularEntries and
// TestExtractTranscriptDir_EmptyTarIsNoop exercise the tar-walking helper
// directly with a hand-built stream, independent of any belief about what
// the real daemon's exact tar shape is (that belief is pinned separately,
// by internal/dockerclient's CopyFromContainerLifecycle conformance case).
func TestExtractTranscriptDir_SkipsNonRegularEntries(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "projects/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "projects/file.jsonl", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()

	if err := m.files.Mkdir("transcripts/agt_x"); err != nil {
		t.Fatal(err)
	}
	if err := m.extractTranscriptDir(&buf, "transcripts/agt_x"); err != nil {
		t.Fatalf("extractTranscriptDir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.FilestoreDir, "transcripts", "agt_x", "projects", "file.jsonl"))
	if err != nil || string(got) != "hello" {
		t.Errorf("extracted content = %q, %v, want %q, nil", got, err, "hello")
	}
}

func TestExtractTranscriptDir_EmptyTarIsNoop(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.Close()

	if err := m.files.Mkdir("transcripts/agt_empty"); err != nil {
		t.Fatal(err)
	}
	if err := m.extractTranscriptDir(&buf, "transcripts/agt_empty"); err != nil {
		t.Fatalf("extractTranscriptDir(empty): %v, want nil", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host "dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e DOCKER_HOST=tcp://docker:2375 \
  golang:1.26 go test ./internal/agent/... -run TestArchiveTranscript -v
```

Expected: FAIL to compile (`m.extractTranscriptDir undefined`,
`transcriptMetadata undefined`, etc).

- [ ] **Step 4: Implement `internal/agent/transcript.go`**

```go
package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
)

// maxTranscriptFileBytes caps any single file extracted into a transcript
// archive -- generous for a session JSONL file or the raw output log, but
// not unbounded against a corrupted or hostile tar stream.
const maxTranscriptFileBytes = 512 << 20

// transcriptMetadata is written as transcripts/<id>/metadata.json: once an
// agent is deleted its bare ID tells you nothing, so this is the only
// record of its identity an archive carries on its own.
type transcriptMetadata struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Harness     string    `json:"harness"`
	Backend     string    `json:"backend"`
	Repo        string    `json:"repo"`
	Image       string    `json:"image"`
	CreatedAt   time.Time `json:"created_at"`
	DeletedAt   time.Time `json:"deleted_at"`
}

// archiveTranscript copies a's session history into transcripts/<a.ID>/ in
// the centralized file store. It never returns an error: any failure (no
// file store, no container reference, a copy or write error) is logged at
// Warn and swallowed, so an archiving failure can never hold up Delete. See
// docs/superpowers/specs/2026-10-10-agent-transcript-archive-design.md.
//
// a must already have its derived names filled in (withDerivedNames) --
// teardown calls this after that step, so the container reference below
// mirrors teardown's own firstNonEmpty(a.ContainerID, a.ContainerName)
// pattern.
func (m *Manager) archiveTranscript(ctx context.Context, a store.Agent) {
	if m.files == nil {
		return
	}
	if a.ID == "" {
		return
	}
	ref := firstNonEmpty(a.ContainerID, a.ContainerName)
	if ref == "" {
		return
	}

	dest := path.Join(filestore.TranscriptsDir, a.ID)
	if err := m.files.Mkdir(dest); err != nil {
		m.log.WarnContext(ctx, "archiving transcript: creating destination directory", "agent_id", a.ID, "error", err)
		return
	}

	meta := transcriptMetadata{
		ID:          a.ID,
		Name:        a.Name,
		Description: a.Description,
		Harness:     HarnessOf(a),
		Backend:     a.Backend,
		Repo:        a.Repo,
		Image:       a.Image,
		CreatedAt:   a.CreatedAt,
		DeletedAt:   time.Now(),
	}
	if err := m.saveTranscriptMetadata(dest, meta); err != nil {
		// Keep going: the source copy below is the valuable part. A
		// missing metadata.json still leaves a diagnosable partial archive.
		m.log.WarnContext(ctx, "archiving transcript: writing metadata.json", "agent_id", a.ID, "error", err)
	}

	var srcPath string
	var extract func(io.Reader) error
	if HarnessOf(a) == config.HarnessOpenCode {
		srcPath = wsbridge.OutputLogPath
		extract = func(r io.Reader) error { return m.extractTranscriptFile(r, path.Join(dest, "output.log")) }
	} else {
		srcPath = configMount + "/projects"
		extract = func(r io.Reader) error { return m.extractTranscriptDir(r, dest) }
	}

	rc, err := m.docker.CopyFromContainer(ctx, ref, srcPath)
	if err != nil {
		if !dockerclient.IsNotFound(err) {
			m.log.WarnContext(ctx, "archiving transcript: copying from container", "agent_id", a.ID, "path", srcPath, "error", err)
		}
		// IsNotFound (nothing was ever written -- e.g. an agent deleted
		// before its first session) is the unremarkable common case, not a
		// failure worth a warning.
		return
	}
	defer rc.Close()

	if err := extract(rc); err != nil {
		m.log.WarnContext(ctx, "archiving transcript: extracting archive", "agent_id", a.ID, "error", err)
	}
}

func (m *Manager) saveTranscriptMetadata(dest string, meta transcriptMetadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	_, err = m.files.Save(path.Join(dest, "metadata.json"), bytes.NewReader(b), int64(len(b)))
	return err
}

// extractTranscriptDir walks r as a tar stream produced by copying a whole
// directory out of a container (CopyFromContainer with a directory
// SourcePath; see internal/dockerclient's CopyFromContainerLifecycle
// conformance case for the exact entry-naming convention this depends on)
// and writes every regular-file entry into the file store under destRoot,
// preserving the tar's own relative paths. Non-regular entries (directory
// headers, symlinks) are skipped. destRoot's parent must already exist;
// Mkdir(path.Dir(...)) per file creates every other directory as needed, so
// an empty tar is a correct no-op.
func (m *Manager) extractTranscriptDir(r io.Reader, destRoot string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dest := path.Join(destRoot, path.Clean(hdr.Name))
		if err := m.files.Mkdir(path.Dir(dest)); err != nil {
			return err
		}
		if _, err := m.files.Save(dest, tr, maxTranscriptFileBytes); err != nil {
			return err
		}
	}
}

// extractTranscriptFile reads the single regular-file entry r's tar stream
// carries -- CopyFromContainer always tar-wraps, even for a one-file source
// -- and writes it into the file store at dest, under the name dest names,
// not the tar entry's own name: Docker tars a single-file source as just
// its basename (e.g. ".agent-output.log"), but this package's own
// destination convention is "output.log". dest's parent directory must
// already exist.
func (m *Manager) extractTranscriptFile(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil
	}
	_, err = m.files.Save(dest, tr, maxTranscriptFileBytes)
	return err
}
```

- [ ] **Step 5: Wire `archiveTranscript` into `teardown`**

In `internal/agent/delete.go`, change:

```go
func (m *Manager) teardown(ctx context.Context, a store.Agent) error {
	a = withDerivedNames(a)
	var errs []error
```

to:

```go
func (m *Manager) teardown(ctx context.Context, a store.Agent) error {
	a = withDerivedNames(a)
	m.archiveTranscript(ctx, a) // best-effort: logs and continues on any error
	var errs []error
```

- [ ] **Step 6: Run the tests, `gofmt`, and `go vet`**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host "dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e DOCKER_HOST=tcp://docker:2375 \
  golang:1.26 sh -c "gofmt -l . && go vet ./... && go test ./internal/agent/... ./internal/filestore/... -race"
```

Expected: `gofmt -l` prints nothing (fix with `gofmt -w` if the
`TranscriptsDir` const block needs realigning), no vet warnings, all tests
`PASS`.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/transcript.go internal/agent/transcript_test.go internal/agent/delete.go internal/filestore/filestore.go
git commit -m "$(cat <<'EOF'
feat(agent): archive session transcripts at delete time

Delete now copies a Claude Code agent's full projects/ session
history, or an opencode agent's raw output log, into transcripts/<id>/
in the centralized file store before any Docker resource is torn
down, alongside a metadata.json snapshot of the agent's identity.
Best-effort: a failure is logged and never blocks Delete.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Full gate and manual verification

**Files:** none (verification only).

**Interfaces:** none new.

- [ ] **Step 1: Run the project's full gate**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host "dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e DOCKER_HOST=tcp://docker:2375 \
  golang:1.26 sh -c "go vet ./... && gofmt -l . && go test ./... -race"
```

Expected: no vet warnings, no `gofmt -l` output, all tests `PASS` (the
conformance suite's `docker` leg requires a reachable daemon, already true
in this environment per `CLAUDE.md`).

- [ ] **Step 2: Run golangci-lint**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" \
  -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  golangci/golangci-lint:latest golangci-lint run ./...
```

Expected: no findings. (Per memory: `go install` of the linter is
DependaProxy-blocked; this prebuilt image is the established workaround.)

- [ ] **Step 3: Manual verification in the browser**

No web/UI code changed (the existing Files browser is fully generic over
new top-level directories), but the spec's Testing section calls for a
human confirming the end result is actually visible: create a real agent,
let it run a session, delete it, then open the Files browser in the web UI
and confirm `transcripts/<id>/` appears with `metadata.json` and either
`projects/...` or `output.log` inside it, and that the files download
correctly.

- [ ] **Step 4: Push and open a PR**

```sh
git push origin feat/agent-transcript-archive
```

Then open a PR against `main` describing the feature, linking the spec at
`docs/superpowers/specs/2026-10-10-agent-transcript-archive-design.md`, per
the `use-git-proxy` skill's broker API.
