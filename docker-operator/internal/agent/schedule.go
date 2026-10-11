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

func (m *Manager) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	ref := agentContainerName(run.AgentID)
	out, err := m.execOneShot(ctx, ref, scheduledRunCheckCmd)
	if err != nil {
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
		return m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout)
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
