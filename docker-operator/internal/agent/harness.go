package agent

import (
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// harnessOf returns a's harness, treating "" (a record written before the
// Harness field existed, or a zero-value store.Agent in a test) as
// config.HarnessClaudeCode -- the built-in default resolveSpec applies to a
// create/update request that names no harness.
func harnessOf(a store.Agent) string {
	return firstNonEmpty(a.Harness, config.HarnessClaudeCode)
}

// agentImageFor returns the operator's default image for harness: the
// opencode image for config.HarnessOpenCode, else the ordinary claude-code
// AgentImage. It does not consider any per-agent override -- see
// agentImageRef for that.
func (m *Manager) agentImageFor(harness string) string {
	if harness == config.HarnessOpenCode {
		return m.cfg.AgentImageOpenCode
	}
	return m.cfg.AgentImage
}

// agentImageRef returns the image reference a's container should run: a's
// own pinned Image when set, else the operator's default for a's harness.
func (m *Manager) agentImageRef(a store.Agent) string {
	return firstNonEmpty(a.Image, m.agentImageFor(harnessOf(a)))
}

// configVolumeMount returns where a's per-agent config volume
// (store.Agent.ClaudeConfigVolume) is mounted inside a's container: the
// opencode XDG data dir for a HarnessOpenCode agent, else the ordinary
// Claude Code config mount.
func configVolumeMount(a store.Agent) string {
	if harnessOf(a) == config.HarnessOpenCode {
		return opencodeDataMount
	}
	return configMount
}

// sessionStart names the three ways an agent's container comes up, which
// decides what session-resumption flag (if any) firstInvocationArgs adds.
type sessionStart int

const (
	sessionFresh    sessionStart = iota // Create: a brand new agent, no prior session.
	sessionContinue                     // Update: the container is recreated in place.
	sessionResume                       // wakeAgent: the old container did not survive.
)

// autoModeArgs returns the harness-specific auto-mode CLI flag(s), or nil
// when auto mode is off (a.AutoMode is not config.AutoModeOn). Claude Code
// takes "--permission-mode auto"; opencode takes "--auto".
func autoModeArgs(a store.Agent) []string {
	if a.AutoMode != config.AutoModeOn {
		return nil
	}
	if harnessOf(a) == config.HarnessOpenCode {
		return []string{"--auto"}
	}
	return []string{"--permission-mode", "auto"}
}

// firstInvocationArgs builds the CLI args appended to tmux-boot.sh's Cmd for
// a's harness, given how the container's session is starting.
//
// This is the one place a session-resumption flag is decided. claude-code
// takes "--continue" (Update) or "--resume" (wakeAgent, whose old tmux
// session did not survive the container stopping). opencode takes
// "--continue" for BOTH -- never "--resume", which exits 1 immediately on
// opencode (verified live in issue #181's review) -- and "--continue" is
// safe there even against an empty session store.
func firstInvocationArgs(a store.Agent, start sessionStart) []string {
	args := autoModeArgs(a)
	if harnessOf(a) == config.HarnessOpenCode {
		if start != sessionFresh {
			args = append(args, "--continue")
		}
		return args
	}
	switch start {
	case sessionContinue:
		args = append(args, "--continue")
	case sessionResume:
		args = append(args, "--resume")
	}
	return args
}
