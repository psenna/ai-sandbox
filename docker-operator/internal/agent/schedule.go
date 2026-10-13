package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

const taskCompleteSentinelPath = "/workspace/.task-complete"
const taskFailedSentinelPath = "/workspace/.task-failed"

var scheduledRunCheckCmd = []string{"sh", "-c",
	"if [ -f '" + taskCompleteSentinelPath + "' ]; then echo complete; " +
		"elif [ -f '" + taskFailedSentinelPath + "' ]; then echo failed; fi"}

func ParseSchedule(expr string) (cron.Schedule, error) {
	return cron.ParseStandard(expr)
}

// CronAgentDue reports whether ca is due to fire at now, anchored on
// whichever of ca.CreatedAt, ca.UpdatedAt, or lastRun.StartedAt is most
// recent -- a CronAgent created long ago whose schedule was just edited (or
// re-enabled) waits for its next real cron fire instead of firing on the
// very next tick off a stale CreatedAt. Named for the entity it now
// operates on (CronAgent, not Template, since #233's own TemplateDue took
// a store.Template) -- keeping the old name on a changed parameter type
// would mislead every future reader of this file.
func CronAgentDue(ca store.CronAgent, lastRun *store.ScheduleRun, now time.Time) bool {
	if !ca.Enabled || ca.Schedule == "" {
		return false
	}
	sched, err := ParseSchedule(ca.Schedule)
	if err != nil {
		return false
	}
	anchor := ca.CreatedAt
	if ca.UpdatedAt.After(anchor) {
		anchor = ca.UpdatedAt
	}
	if lastRun != nil && lastRun.StartedAt.After(anchor) {
		anchor = lastRun.StartedAt
	}
	next := sched.Next(anchor)
	return !next.After(now)
}

// CreateFromCronAgent resolves ca.TemplateID to its Template and creates an
// agent from the Template's config, with Name and OneShotTask coming from
// the CronAgent instead -- the two fields that moved off Template (spec
// decision 1 and 4).
func (m *Manager) CreateFromCronAgent(ctx context.Context, ca store.CronAgent) (store.Agent, error) {
	tmpl, err := m.store.GetTemplate(ctx, ca.TemplateID)
	if err != nil {
		return store.Agent{}, err
	}
	return m.Create(ctx, CreateRequest{
		Name: ca.Name, Backend: tmpl.Backend, Harness: tmpl.Harness, Model: tmpl.Model,
		FastModel: tmpl.FastModel, OllamaURL: tmpl.OllamaURL, Repo: tmpl.Repo,
		AutoCompactThreshold: tmpl.AutoCompactThreshold, MaxContextTokens: tmpl.MaxContextTokens,
		ImageTag: tmpl.ImageTag, AutoMode: tmpl.AutoMode, OneShotTask: ca.Task,
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

// ApplyOnComplete applies ca's completion policy to run's agent. Its body
// is unchanged from #233's own Template-typed version -- it only ever read
// .OnComplete, which now lives on CronAgent instead of Template.
func (m *Manager) ApplyOnComplete(ctx context.Context, ca store.CronAgent, run store.ScheduleRun) error {
	switch ca.OnComplete {
	case "stop":
		if _, err := m.store.Update(ctx, run.AgentID, func(a *store.Agent) error {
			a.Status = store.StatusStopped
			return nil
		}); err != nil {
			return err
		}
		var errs []error
		if err := m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout); err != nil {
			errs = append(errs, err)
		}
		if err := m.docker.ContainerStop(ctx, dindContainerName(run.AgentID), m.opts.StopTimeout); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	case "delete":
		return m.Delete(ctx, run.AgentID)
	default:
		return nil
	}
}

func (m *Manager) ListScheduleRunsByCronAgent(ctx context.Context, cronAgentID string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByCronAgent(ctx, cronAgentID)
}
func (m *Manager) ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByStatus(ctx, status)
}
func (m *Manager) CreateScheduleRun(ctx context.Context, cronAgentID, agentID string) (store.ScheduleRun, error) {
	return m.store.CreateScheduleRun(ctx, cronAgentID, agentID)
}
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
