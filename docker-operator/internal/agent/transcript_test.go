package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
)

// newSeededFake returns a *dockerclienttest.Fake set up exactly like
// newTestManagerCfg's: AutoHealthy on, the shared dependaproxy container
// running, and both harness images present. Tests that need to observe or
// intercept calls (a ctx capture, an injected panic) wrap this fake --
// embedding it and overriding one method -- instead of using
// newTestManagerCfg, which owns a plain *dockerclienttest.Fake outright.
func newSeededFake(t *testing.T, cfg config.Config) *dockerclienttest.Fake {
	t.Helper()
	f := dockerclienttest.New()
	f.AutoHealthy = true
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImageClaudeCode)
	f.AddImage(cfg.AgentImageOpenCode)
	return f
}

// newTestManagerWith builds a Manager like newTestManagerCfg, but with a
// caller-supplied docker client and logger -- for tests that need to wrap
// the client or capture log output in ways newTestManagerCfg's fixed
// choices don't allow.
func newTestManagerWith(t *testing.T, cfg config.Config, docker dockerclient.Client, log *slog.Logger) (*Manager, *store.Store) {
	t.Helper()
	st := newTestStore(t, cfg.MaxAgents)
	return NewManager(docker, newTestRegistries(), st, cfg, log, testOptions()), st
}

// ctxCapturingDocker wraps a *dockerclienttest.Fake and records the ctx
// archiveTranscript's CopyFromContainer call actually received, to prove
// the call is bounded by its own timeout rather than running under the
// caller's ctx unbounded.
type ctxCapturingDocker struct {
	*dockerclienttest.Fake
	gotCtx context.Context
}

func (c *ctxCapturingDocker) CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, error) {
	c.gotCtx = ctx
	return c.Fake.CopyFromContainer(ctx, id, path)
}

// panickingDocker wraps a *dockerclienttest.Fake whose CopyFromContainer
// always panics, to prove archiveTranscript recovers rather than letting
// the panic propagate through teardown and leave Delete stuck.
type panickingDocker struct {
	*dockerclienttest.Fake
}

func (c *panickingDocker) CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, error) {
	panic("boom: simulated panic inside CopyFromContainer")
}

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
	got, err := os.ReadFile(filepath.Join(dir, "projects", "enc-cwd", "session-1.jsonl")) //nolint:gosec // G304: test reads back a file archiveTranscript itself just wrote under a t.TempDir() root
	if err != nil {
		t.Fatalf("reading archived transcript: %v", err)
	}
	if string(got) != `{"type":"message"}` {
		t.Errorf("archived content = %q, want the seeded session content", got)
	}

	metaBytes, err := os.ReadFile(filepath.Join(dir, "metadata.json")) //nolint:gosec // G304: test reads back a file archiveTranscript itself just wrote under a t.TempDir() root
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
	got, err := os.ReadFile(filepath.Join(dir, "output.log")) //nolint:gosec // G304: test reads back a file archiveTranscript itself just wrote under a t.TempDir() root
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
// partial archive is still diagnosable. The logger only records Warn+, so
// a non-empty buffer here can only be the archiving failure itself -- not
// Delete's own unrelated Info-level "agent deleted" line.
func TestArchiveTranscript_CopyFailureNeverBlocksDelete(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	f := newSeededFake(t, cfg)
	var logBuf bytes.Buffer
	m, _ := newTestManagerWith(t, cfg, f, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
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
	if logBuf.Len() == 0 {
		t.Error("no Warn logged for a real copy failure -- a genuine error must be observable, unlike the quiet IsNotFound case")
	}
}

// TestArchiveTranscript_MissingSourceIsQuiet pins the Review Focus item: a
// CopyFromContainer IsNotFound result (nothing was ever written) is the
// unremarkable common case, not a failure -- Delete must still succeed,
// still write metadata.json, and (the actual "quiet" assertion) never emit
// a Warn. The logger only records Warn+, so any other Info-level logging
// from Create/Delete itself cannot make this assertion pass by accident.
func TestArchiveTranscript_MissingSourceIsQuiet(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	f := newSeededFake(t, cfg)
	var logBuf bytes.Buffer
	m, _ := newTestManagerWith(t, cfg, f, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
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
	if logBuf.Len() != 0 {
		t.Errorf("log output = %q, want empty (an IsNotFound source must never warn)", logBuf.String())
	}
}

// TestArchiveTranscript_NoContainerRefIsNoop pins the ref == "" guard
// itself: called directly, bypassing teardown's withDerivedNames, a bare
// record with no ContainerID and no ContainerName at all must still no-op
// cleanly rather than calling CopyFromContainer with an empty ref.
func TestArchiveTranscript_NoContainerRefIsNoop(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	m.archiveTranscript(ctx, store.Agent{ID: "agt_bare_direct"})

	if _, err := os.Stat(filepath.Join(cfg.FilestoreDir, "transcripts", "agt_bare_direct")); !os.IsNotExist(err) {
		t.Errorf("transcripts/agt_bare_direct created despite no container reference at all: %v", err)
	}
}

// TestDelete_DerivedNamesFromIDOnlyRecord_ArchivesTranscript is
// TestDelete_DerivedNamesFromIDOnlyRecord (delete_test.go) with the file
// store enabled: the real production path for "a record that crashed
// before Create stamped anything" resolves through withDerivedNames
// deriving a real container name, then CopyFromContainer's IsNotFound
// (nothing was ever seeded for it) -- a different mechanism from the
// ref == "" guard above, and the one archiveTranscript actually takes
// through teardown. Delete must still succeed and still write
// metadata.json for this record.
func TestDelete_DerivedNamesFromIDOnlyRecord_ArchivesTranscript(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	f := dockerclienttest.New()
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	st := newTestStore(t, cfg.MaxAgents)
	m := NewManager(f, nil, st, cfg, testLogger(), testOptions())
	ctx := context.Background()

	const id = "agt_bareid_fs"
	if _, err := st.Create(ctx, store.CreateSpec{ID: id}); err != nil {
		t.Fatalf("seeding a bare (ID-only) record: %v", err)
	}
	// Pre-create every resource under the names Create WOULD have used, as
	// if the process had crashed after touching Docker but before the
	// first stamp landed -- see TestDelete_DerivedNamesFromIDOnlyRecord.
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: workspaceVolumeName(id)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: claudeConfigVolumeName(id)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: dindCacheVolumeName(id)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: dinernetName(id)}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	if _, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: agentContainerName(id)}); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	if err := m.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	dir := filepath.Join(cfg.FilestoreDir, "transcripts", id)
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); err != nil {
		t.Errorf("metadata.json missing for a bare-ID-only record: %v", err)
	}
}

// TestArchiveTranscript_BoundsCopyWithATimeout pins that the copy is not
// run under the caller's ctx unbounded: a slow or stuck daemon must not be
// able to stall Delete indefinitely.
func TestArchiveTranscript_BoundsCopyWithATimeout(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	wrapped := &ctxCapturingDocker{Fake: newSeededFake(t, cfg)}
	m, _ := newTestManagerWith(t, cfg, wrapped, testLogger())
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if wrapped.gotCtx == nil {
		t.Fatal("CopyFromContainer was never called")
	}
	if _, ok := wrapped.gotCtx.Deadline(); !ok {
		t.Error("CopyFromContainer's ctx has no deadline, want archiveTranscript to bound the copy with its own timeout")
	}
}

// TestArchiveTranscript_PanicDoesNotBlockDelete pins the "never blocks
// Delete" contract against the one failure mode a logged-and-swallowed
// error cannot cover: a panic. Without a recover, this test would crash
// the whole test binary rather than fail cleanly.
func TestArchiveTranscript_PanicDoesNotBlockDelete(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	wrapped := &panickingDocker{Fake: newSeededFake(t, cfg)}
	m, st := newTestManagerWith(t, cfg, wrapped, testLogger())
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v, want nil (a panic inside archiving must never block delete)", err)
	}
	if _, err := st.Get(ctx, a.ID); !store.IsNotFound(err) {
		t.Errorf("agent record still present after Delete recovered from a panic: %v", err)
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

// TestExtractTranscriptDir_ContinuesPastPerEntryErrors pins that one bad
// entry (an invalid name, an oversized file, anything filestore.Save/Mkdir
// rejects) must not discard the rest of the archive -- tar order is the
// daemon's readdir order, so a single unlucky or hostile early entry must
// never truncate every entry after it.
func TestExtractTranscriptDir_ContinuesPastPerEntryErrors(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("A"))
	// A backslash makes this one segment invalid (filestore.validSegment
	// rejects it), so Save fails for this entry only.
	_ = tw.WriteHeader(&tar.Header{Name: "bad\\name.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("X"))
	_ = tw.WriteHeader(&tar.Header{Name: "b.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("B"))
	_ = tw.Close()

	if err := m.files.Mkdir("transcripts/agt_partial"); err != nil {
		t.Fatal(err)
	}
	if err := m.extractTranscriptDir(&buf, "transcripts/agt_partial"); err == nil {
		t.Fatal("extractTranscriptDir: want a non-nil aggregate error reporting the bad entry")
	}

	gotA, err := os.ReadFile(filepath.Join(cfg.FilestoreDir, "transcripts", "agt_partial", "a.txt")) //nolint:gosec // G304: test reads back a file this same test just wrote under a t.TempDir() root
	if err != nil || string(gotA) != "A" {
		t.Errorf("a.txt = %q, %v, want %q, nil", gotA, err, "A")
	}
	gotB, err := os.ReadFile(filepath.Join(cfg.FilestoreDir, "transcripts", "agt_partial", "b.txt")) //nolint:gosec // G304: test reads back a file this same test just wrote under a t.TempDir() root
	if err != nil || string(gotB) != "B" {
		t.Errorf("b.txt (written after the bad entry) = %q, %v, want %q, nil -- a per-entry error must not abort the rest of the archive", gotB, err, "B")
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

// TestExtractTranscriptDir_RejectsPathEscape guards against a tar-slip: a
// crafted entry name like "../../escaped.txt" must never land outside
// destRoot. A malicious or compromised agent controls every file under its
// own $CLAUDE_CONFIG_DIR/projects/, so a crafted name here must be assumed
// possible, not dismissed as something only a well-behaved Claude Code
// process would ever write.
func TestExtractTranscriptDir_RejectsPathEscape(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// One level up from destRoot: lands at transcripts/escaped.txt, a
	// sibling of every agent's own subdirectory -- a real, silent escape
	// (no error), unlike a name that resolves above the file-store root
	// entirely, which filestore's own path validation happens to reject.
	_ = tw.WriteHeader(&tar.Header{Name: "../escaped.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5})
	_, _ = tw.Write([]byte("pwned"))
	_ = tw.Close()

	if err := m.files.Mkdir("transcripts/agt_victim"); err != nil {
		t.Fatal(err)
	}
	if err := m.extractTranscriptDir(&buf, "transcripts/agt_victim"); err != nil {
		t.Fatalf("extractTranscriptDir: %v, want nil (a hostile entry is skipped, not a hard failure)", err)
	}

	if _, err := os.Stat(filepath.Join(cfg.FilestoreDir, "transcripts", "escaped.txt")); !os.IsNotExist(err) {
		t.Errorf("escaped.txt written outside transcripts/agt_victim/, at transcripts/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.FilestoreDir, "transcripts", "agt_victim", "escaped.txt")); !os.IsNotExist(err) {
		t.Errorf("escaped.txt landed inside destRoot unexpectedly: %v", err)
	}
}
