package agent

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// taskCompleteSentinelPath is the file claude-supervisor.sh writes right
// after a clean (status 0) one-shot exit -- see agent/claude-supervisor.sh.
// Same directory and naming convention as wsbridge.OutputLogPath's
// /workspace/.agent-output.log.
const taskCompleteSentinelPath = "/workspace/.task-complete"

// taskFailedSentinelPath is the file claude-supervisor.sh writes when a
// one-shot (`-p`) claude exits non-zero -- the supervisor never resumes a
// one-shot run, so this is final.
const taskFailedSentinelPath = "/workspace/.task-failed"

// scheduledRunCheckCmd prints "complete" or "failed" for whichever
// sentinel is present (complete wins if somehow both are), or nothing
// while the task is still running.
var scheduledRunCheckCmd = []string{"sh", "-c",
	"if [ -f '" + taskCompleteSentinelPath + "' ]; then echo complete; " +
		"elif [ -f '" + taskFailedSentinelPath + "' ]; then echo failed; fi"}

// ParseSchedule parses a standard 5-field cron expression. Used both by
// the ticker (via TemplateDue, below) and by internal/api's template
// validation, so a Template's Schedule is checked with the exact same
// parser it will later be run against.
func ParseSchedule(expr string) (cron.Schedule, error) {
	return cron.ParseStandard(expr)
}

// TemplateDue reports whether tmpl's schedule has a fire time in
// (anchor, now], where anchor is the most recent of tmpl.CreatedAt,
// tmpl.UpdatedAt and lastRun's StartedAt (if non-nil). UpdatedAt matters:
// adding a schedule to an old template, or re-enabling a paused one, must
// wait for the next real cron fire, not fire immediately off a stale
// CreatedAt whose Next() is long past. A disabled or unscheduled template is never due. An
// unparseable Schedule (should not happen past internal/api's validation,
// but this function makes no assumption about that) is treated as never
// due, not a panic.
func TemplateDue(tmpl store.Template, lastRun *store.ScheduleRun, now time.Time) bool {
	if !tmpl.Enabled || tmpl.Schedule == "" {
		return false
	}
	sched, err := ParseSchedule(tmpl.Schedule)
	if err != nil {
		return false
	}
	anchor := tmpl.CreatedAt
	if tmpl.UpdatedAt.After(anchor) {
		anchor = tmpl.UpdatedAt
	}
	if lastRun != nil && lastRun.StartedAt.After(anchor) {
		anchor = lastRun.StartedAt
	}
	next := sched.Next(anchor)
	return !next.After(now)
}

// CreateFromTemplate creates a fresh agent from tmpl's fields, running
// tmpl.Task as a one-shot invocation. This translation is NEW: the only
// existing Template -> create-agent-fields mapping before this feature was
// web/app.js's applyTemplateToForm, pure client-side JS that prefills the
// create form for a human to then submit -- there was no server-side
// equivalent, since nothing before this feature ever created an agent from
// a template without a human in the loop.
func (m *Manager) CreateFromTemplate(ctx context.Context, tmpl store.Template) (store.Agent, error) {
	return m.Create(ctx, CreateRequest{
		Backend:              tmpl.Backend,
		Harness:              tmpl.Harness,
		Model:                tmpl.Model,
		FastModel:            tmpl.FastModel,
		OllamaURL:            tmpl.OllamaURL,
		Repo:                 tmpl.Repo,
		AutoCompactThreshold: tmpl.AutoCompactThreshold,
		MaxContextTokens:     tmpl.MaxContextTokens,
		ImageTag:             tmpl.ImageTag,
		AutoMode:             tmpl.AutoMode,
		OneShotTask:          tmpl.Task,
	})
}

// CheckScheduledRunStatus reports what state run's agent is actually in,
// by execing a check for taskCompleteSentinelPath / taskFailedSentinelPath
// via execOneShot:
//
//   - "running": the agent exists and neither sentinel is there yet.
//   - "done": the completion sentinel is present -- the one-shot task
//     finished cleanly.
//   - "failed": the failure sentinel is present -- the one-shot claude
//     exited non-zero (the supervisor never resumes a one-shot run).
//   - "gone": the container no longer exists, or exists but is not running
//     (deleted or stopped out-of-band, or by an earlier, crashed OnComplete
//     pass) -- either way nothing is going to finish this task.
//
// "gone" is reported with a nil error, deliberately: it is NOT a transient
// failure worth retrying, it is the ticker's cue to finalize that
// ScheduleRun as an error itself -- collapsing it into the same result as
// "running" (as an earlier draft of this function did, returning a bare
// bool) would leave a run whose agent is gone stuck "running" forever,
// re-checked every tick with nothing ever finalizing it.
func (m *Manager) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	ref := agentContainerName(run.AgentID)
	out, err := m.execOneShot(ctx, ref, scheduledRunCheckCmd)
	if err != nil {
		// A stopped-but-present container answers exec with a conflict,
		// not a not-found, so inspect to tell "gone" from a real failure.
		c, ierr := m.docker.ContainerInspect(ctx, ref)
		if dockerclient.IsNotFound(ierr) || (ierr == nil && c.State != dockerclient.StateRunning) {
			return "gone", nil
		}
		return "", err
	}
	switch strings.TrimSpace(string(out)) {
	case "complete":
		return "done", nil
	case "failed":
		return "failed", nil
	default:
		return "running", nil
	}
}

// ApplyOnComplete runs tmpl's completion policy for run's agent: "stop"
// (record marked StatusStopped first, then ContainerStop, left
// inspectable -- the shield keeps the container's `die` event, which
// MarkUnexpectedExit applies only to a StatusRunning record, from turning
// a deliberate stop into a false StatusError), "delete" (Delete, which archives the
// transcript first per the already-shipped internal/agent/transcript.go,
// regardless of this feature), or "leave" (nothing further). An unknown or
// empty OnComplete is treated as "leave" -- the safest no-op default,
// never reachable past internal/api's validation but defensive here too.
func (m *Manager) ApplyOnComplete(ctx context.Context, tmpl store.Template, run store.ScheduleRun) error {
	switch tmpl.OnComplete {
	case "stop":
		if _, err := m.store.Update(ctx, run.AgentID, func(a *store.Agent) error {
			a.Status = store.StatusStopped
			return nil
		}); err != nil {
			return err
		}
		return m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout)
	case "delete":
		return m.Delete(ctx, run.AgentID)
	default:
		return nil
	}
}

// ListScheduleRunsByTemplate returns every run for templateID, newest
// first, mirroring ListTemplates's thin pass-through shape. Added here,
// not in internal/api's task, because cmd/docker-operator's
// schedulingDeps interface (next task) needs this on Manager before that
// task ever runs -- internal/api's own task only calls it, it does not
// define it.
func (m *Manager) ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByTemplate(ctx, templateID)
}

// ListScheduleRunsByStatus returns every ScheduleRun with the given status,
// regardless of template -- the same thin pass-through shape as
// ListScheduleRunsByTemplate, and added alongside it for the same reason:
// cmd/docker-operator's schedulingDeps interface needs this on Manager, not
// on *store.Store directly.
func (m *Manager) ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByStatus(ctx, status)
}

// CreateScheduleRun records a new in-flight run (status
// store.ScheduleRunStatusRunning) for templateID's agentID. Thin
// pass-through, mirroring ListScheduleRunsByTemplate.
func (m *Manager) CreateScheduleRun(ctx context.Context, templateID, agentID string) (store.ScheduleRun, error) {
	return m.store.CreateScheduleRun(ctx, templateID, agentID)
}

// FinishScheduleRun sets a ScheduleRun's terminal status (success or error)
// and its FinishedAt. Thin pass-through, mirroring
// ListScheduleRunsByTemplate.
func (m *Manager) FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error) {
	return m.store.FinishScheduleRun(ctx, id, status)
}

// execOneShot runs cmd inside the container named ref via one exec,
// bounded by its own timeout (m.opts.ExecTimeout), and returns everything
// the command wrote to stdout/stderr. A missing container satisfies
// dockerclient.IsNotFound. Deliberately independent of waitTmuxSession's
// own exec-and-poll-ExecInspect shape (internal/agent/create.go) rather
// than refactored out of it: that function's exit-status polling is more
// than this helper's callers need, and changing it risks a regression in
// an already-working path for no benefit this feature requires.
func (m *Manager) execOneShot(ctx context.Context, ref string, cmd []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.ExecTimeout)
	defer cancel()

	execID, err := m.docker.ExecCreate(ctx, ref, dockerclient.ExecSpec{Cmd: cmd})
	if err != nil {
		return nil, err
	}
	stream, err := m.docker.ExecAttach(ctx, execID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	return io.ReadAll(stream)
}
