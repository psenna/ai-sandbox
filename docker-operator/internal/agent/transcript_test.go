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
