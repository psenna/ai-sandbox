package sandboxctl

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// gitStateFileName is the workspace-relative path the agent's skill writes
// its final git state to (branch, HEAD SHA, PR reference). The freeze hook
// reads it so run.json can record "the final git state" (#32) even after the
// workspace is gone; the agent container itself holds no Kubernetes
// credential, so the file under /workspace/.sandbox/ is the only channel.
const gitStateFileName = ".sandbox/git-state.json"

// hex40 matches a 40-hex-character lowercase SHA-1 (a git commit-ish),
// mirroring the CRD's +kubebuilder:validation:Pattern on
// v1alpha1.GitStateStatus.HeadSHA. Deliberately restated here (storage's
// hex40 is unexported) and kept in sync with both.
var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ReadGitStateFile best-effort reads <workspacePath>/.sandbox/git-state.json
// into a status.gitState-shaped value. The file is an OPTIONAL, ad-hoc input
// the agent's skill may or may not have written, so every failure mode --
// missing file, unreadable, unknown field, malformed headSHA, PR with an
// empty repo or number < 1 -- returns (nil, false) rather than an error:
// the caller (the freeze hook) simply omits git state from the patch, which
// is always schema-valid. Unknown fields are rejected (DisallowUnknownFields)
// so a future, incompatible skill file fails loudly instead of being silently
// truncated.
func ReadGitStateFile(workspacePath string) (*v1alpha1.GitStateStatus, bool) {
	p := filepath.Join(workspacePath, gitStateFileName)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var g v1alpha1.GitStateStatus
	if err := dec.Decode(&g); err != nil {
		return nil, false
	}
	if g.HeadSHA != "" && !hex40.MatchString(g.HeadSHA) {
		return nil, false
	}
	if g.PullRequest != nil && (g.PullRequest.Repo == "" || g.PullRequest.Number < 1) {
		return nil, false
	}
	return &g, true
}
