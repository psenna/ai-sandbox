package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// maxScheduledRunDuration is a backstop independent of the completion
// sentinels: a run in flight longer than this is finalized as an error
// (after its OnComplete policy runs), so no failure mode -- a hung claude,
// a sentinel never written -- can leak a run and its agent slot forever.
const maxScheduledRunDuration = 2 * time.Hour

// schedulingDeps is the Manager/Store surface runSchedulingTickOnce needs,
// as an interface seam -- the same role reconciler plays for
// startPeriodicReconcile -- so schedule_test.go can drive it with an
// in-memory fake instead of a real Manager and BoltDB file.
type schedulingDeps interface {
	ListTemplates(ctx context.Context) ([]store.Template, error)
	GetTemplate(ctx context.Context, id string) (store.Template, error)
	ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error)
	ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error)
	CreateScheduleRun(ctx context.Context, templateID, agentID string) (store.ScheduleRun, error)
	FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error)
	CreateFromTemplate(ctx context.Context, tmpl store.Template) (store.Agent, error)
	CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (status string, err error)
	ApplyOnComplete(ctx context.Context, tmpl store.Template, run store.ScheduleRun) error
}

// startSchedulingTicker fires due schedules and checks in-flight runs for
// completion every interval, modeled directly on startPeriodicReconcile:
// a cancel-based stop function, overlap-guarded the same structural way
// (a tick never re-enters while the previous one is still inline).
func startSchedulingTicker(deps schedulingDeps, interval time.Duration, log *slog.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		var running atomic.Bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !running.CompareAndSwap(false, true) {
					log.Info("scheduling tick: the previous tick is still running; skipping this one")
					continue
				}
				runSchedulingTickOnce(ctx, deps, log)
				running.Store(false)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// runSchedulingTickOnce is one tick's body: fire every due, enabled
// template, then check every in-flight run for completion. Each half
// processes every eligible item regardless of an earlier one's failure --
// one template's CreateFromTemplate error, or one run's completion-check
// error, is logged and the loop continues, mirroring teardown's own
// "collect every error, never stop at the first" stance.
func runSchedulingTickOnce(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	fireDueTemplates(ctx, deps, log)
	checkRunningSchedules(ctx, deps, log)
}

func fireDueTemplates(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	templates, err := deps.ListTemplates(ctx)
	if err != nil {
		log.Warn("scheduling tick: listing templates", "error", err)
		return
	}
	now := time.Now()
	for _, tmpl := range templates {
		if tmpl.Schedule == "" || !tmpl.Enabled {
			continue
		}
		runs, err := deps.ListScheduleRunsByTemplate(ctx, tmpl.ID)
		if err != nil {
			log.Warn("scheduling tick: listing runs for template", "template_id", tmpl.ID, "error", err)
			continue
		}
		var lastRun *store.ScheduleRun
		if len(runs) > 0 {
			lastRun = &runs[0] // newest first, per ListScheduleRunsByTemplate's contract
		}
		if !agent.TemplateDue(tmpl, lastRun, now) {
			continue
		}

		// Defensive only: internal/api's create/update validation is the
		// real enforcement of "scheduling is Claude Code only in v1,"
		// rejecting this combination before a template can ever be saved.
		// This check exists for a record saved before that validation
		// existed, or reached this state some other inconsistent way.
		if tmpl.Harness == config.HarnessOpenCode {
			log.Warn("scheduling tick: template has the opencode harness, which scheduling does not support; skipping", "template_id", tmpl.ID)
			continue
		}

		a, err := deps.CreateFromTemplate(ctx, tmpl)
		if err != nil {
			// A failed attempt still gets a ScheduleRun (with an empty
			// AgentID), finalized as an error right away: it shows up in
			// the run history, and it advances TemplateDue's anchor so a
			// persistently-failing template is retried at its next cron
			// fire, not on every single tick. It is never "running", so
			// checkRunningSchedules never sees its empty AgentID.
			log.Warn("scheduling tick: creating agent from template", "template_id", tmpl.ID, "error", err)
			run, rerr := deps.CreateScheduleRun(ctx, tmpl.ID, "")
			if rerr != nil {
				log.Warn("scheduling tick: recording a failed schedule run", "template_id", tmpl.ID, "error", rerr)
				continue
			}
			if _, ferr := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); ferr != nil {
				log.Warn("scheduling tick: finalizing a failed schedule run", "run_id", run.ID, "error", ferr)
			}
			continue
		}
		if _, err := deps.CreateScheduleRun(ctx, tmpl.ID, a.ID); err != nil {
			log.Warn("scheduling tick: recording schedule run", "template_id", tmpl.ID, "agent_id", a.ID, "error", err)
		}
	}
}

func checkRunningSchedules(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	runs, err := deps.ListScheduleRunsByStatus(ctx, store.ScheduleRunStatusRunning)
	if err != nil {
		log.Warn("scheduling tick: listing running schedule runs", "error", err)
		return
	}
	for _, run := range runs {
		if time.Since(run.StartedAt) > maxScheduledRunDuration {
			log.Warn("scheduling tick: run exceeded the maximum duration; finalizing as error", "run_id", run.ID, "started_at", run.StartedAt)
			finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusError)
			continue
		}

		status, err := deps.CheckScheduledRunStatus(ctx, run)
		if err != nil {
			log.Warn("scheduling tick: checking run status", "run_id", run.ID, "error", err)
			continue
		}

		switch status {
		case "running":
			continue
		case "failed":
			// The one-shot task exited non-zero. The template's cleanup
			// policy still applies; only the run's outcome differs.
			finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusError)
			continue
		case "gone":
			// The agent is gone (deleted out-of-band, or by an earlier,
			// crashed OnComplete pass) -- finalize as an error rather than
			// leaving this run "running" forever, re-checked every tick
			// with nothing ever able to finish it.
			if _, err := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); err != nil {
				log.Warn("scheduling tick: finalizing a gone run as error", "run_id", run.ID, "error", err)
			}
			continue
		}
		// status == "done"
		finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusSuccess)
	}
}

// finishScheduledRun applies run's template's OnComplete policy, then
// finalizes run with status. Shared by the done, failed and timed-out
// paths, which differ only in that final status.
func finishScheduledRun(ctx context.Context, deps schedulingDeps, log *slog.Logger, run store.ScheduleRun, status string) {
	// A template deleted while this run was in flight -- allowed, since a
	// template's own lifecycle is independent of both its runs and their
	// agents -- leaves tmpl as the zero value here, whose OnComplete ("")
	// ApplyOnComplete's switch treats as "leave" (its safest no-op
	// default). The run still gets finalized below either way; only the
	// cleanup policy silently becomes "leave" instead of whatever the
	// deleted template specified.
	tmpl, err := deps.GetTemplate(ctx, run.TemplateID)
	if err != nil && !store.IsTemplateNotFound(err) {
		log.Warn("scheduling tick: loading template for finished run", "run_id", run.ID, "template_id", run.TemplateID, "error", err)
		return
	}
	if err := deps.ApplyOnComplete(ctx, tmpl, run); err != nil {
		log.Warn("scheduling tick: applying on-complete policy", "run_id", run.ID, "error", err)
	}
	if _, err := deps.FinishScheduleRun(ctx, run.ID, status); err != nil {
		log.Warn("scheduling tick: finalizing schedule run", "run_id", run.ID, "error", err)
	}
}
