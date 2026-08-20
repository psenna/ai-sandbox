package sandboxctl

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGitStateFile(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, gitStateFileName)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestReadGitStateFile_Missing(t *testing.T) {
	dir := t.TempDir()
	g, ok := ReadGitStateFile(dir)
	if ok || g != nil {
		t.Fatalf("ReadGitStateFile = (%v, %v), want (nil, false) for a missing file", g, ok)
	}
}

func TestReadGitStateFile_Valid(t *testing.T) {
	dir := t.TempDir()
	sha := "0123456789abcdef0123456789abcdef01234567"
	writeGitStateFile(t, dir, `{"branch":"feat/x","headSHA":"`+sha+`","pullRequest":{"repo":"owner/repo","number":7}}`)

	g, ok := ReadGitStateFile(dir)
	if !ok || g == nil {
		t.Fatalf("ReadGitStateFile: want ok, got (%v, %v)", g, ok)
	}
	if g.Branch != "feat/x" || g.HeadSHA != sha {
		t.Errorf("got Branch=%q HeadSHA=%q", g.Branch, g.HeadSHA)
	}
	if g.PullRequest == nil || g.PullRequest.Repo != "owner/repo" || g.PullRequest.Number != 7 {
		t.Errorf("got PullRequest=%+v", g.PullRequest)
	}
}

func TestReadGitStateFile_ValidNoPullRequest(t *testing.T) {
	dir := t.TempDir()
	sha := "0123456789abcdef0123456789abcdef01234567"
	writeGitStateFile(t, dir, `{"branch":"main","headSHA":"`+sha+`"}`)

	g, ok := ReadGitStateFile(dir)
	if !ok || g == nil {
		t.Fatalf("ReadGitStateFile: want ok, got (%v, %v)", g, ok)
	}
	if g.PullRequest != nil {
		t.Errorf("PullRequest = %+v, want nil", g.PullRequest)
	}
}

func TestReadGitStateFile_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	writeGitStateFile(t, dir, `{not json`)
	if g, ok := ReadGitStateFile(dir); ok || g != nil {
		t.Fatalf("ReadGitStateFile = (%v, %v), want (nil, false) for malformed JSON", g, ok)
	}
}

func TestReadGitStateFile_UnknownField(t *testing.T) {
	dir := t.TempDir()
	writeGitStateFile(t, dir, `{"branch":"main","unknownField":"x"}`)
	if g, ok := ReadGitStateFile(dir); ok || g != nil {
		t.Fatalf("ReadGitStateFile = (%v, %v), want (nil, false) for an unknown field", g, ok)
	}
}

func TestReadGitStateFile_MalformedHeadSHA(t *testing.T) {
	dir := t.TempDir()
	writeGitStateFile(t, dir, `{"branch":"main","headSHA":"not-a-sha"}`)
	if g, ok := ReadGitStateFile(dir); ok || g != nil {
		t.Fatalf("ReadGitStateFile = (%v, %v), want (nil, false) for a malformed headSHA", g, ok)
	}
}

func TestReadGitStateFile_EmptyHeadSHAAllowed(t *testing.T) {
	dir := t.TempDir()
	writeGitStateFile(t, dir, `{"branch":"main"}`)
	g, ok := ReadGitStateFile(dir)
	if !ok || g == nil {
		t.Fatalf("ReadGitStateFile: want ok (empty headSHA is allowed), got (%v, %v)", g, ok)
	}
	if g.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty", g.HeadSHA)
	}
}

func TestReadGitStateFile_PullRequestMissingRepo(t *testing.T) {
	dir := t.TempDir()
	writeGitStateFile(t, dir, `{"branch":"main","pullRequest":{"number":3}}`)
	if g, ok := ReadGitStateFile(dir); ok || g != nil {
		t.Fatalf("ReadGitStateFile = (%v, %v), want (nil, false) for a PR with empty repo", g, ok)
	}
}

func TestReadGitStateFile_PullRequestBadNumber(t *testing.T) {
	dir := t.TempDir()
	writeGitStateFile(t, dir, `{"branch":"main","pullRequest":{"repo":"owner/repo","number":0}}`)
	if g, ok := ReadGitStateFile(dir); ok || g != nil {
		t.Fatalf("ReadGitStateFile = (%v, %v), want (nil, false) for a PR with number < 1", g, ok)
	}
}
