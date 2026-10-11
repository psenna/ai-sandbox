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
	ListCronAgents(ctx context.Context) ([]store.CronAgent, error)
	GetCronAgent(ctx context.Context, id string) (store.CronAgent, error)
	GetTemplate(ctx context.Context, id string) (store.Template, error)
	ListScheduleRunsByCronAgent(ctx context.Context, cronAgentID string) ([]store.ScheduleRun, error)
	ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error)
	CreateScheduleRun(ctx context.Context, cronAgentID, agentID string) (store.ScheduleRun, error)
	FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error)
	CreateFromCronAgent(ctx context.Context, ca store.CronAgent) (store.Agent, error)
	CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (status string, err error)
	ApplyOnComplete(ctx context.Context, ca store.CronAgent, run store.ScheduleRun) error
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

// runSchedulingTickOnce is one tick's body: fire every due, enabled cron
// agent, then check every in-flight run for completion. Each half
// processes every eligible item regardless of an earlier one's failure --
// one cron agent's CreateFromCronAgent error, or one run's completion-check
// error, is logged and the loop continues, mirroring teardown's own
// "collect every error, never stop at the first" stance.
func runSchedulingTickOnce(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	fireDueCronAgents(ctx, deps, log)
	checkRunningSchedules(ctx, deps, log)
}

func fireDueCronAgents(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	cronAgents, err := deps.ListCronAgents(ctx)
	if err != nil {
		log.Warn("scheduling tick: listing cron agents", "error", err)
		return
	}
	now := time.Now()
	for _, ca := range cronAgents {
		if ca.Schedule == "" || !ca.Enabled {
			continue
		}
		runs, err := deps.ListScheduleRunsByCronAgent(ctx, ca.ID)
		if err != nil {
			log.Warn("scheduling tick: listing runs for cron agent", "cron_agent_id", ca.ID, "error", err)
			continue
		}
		var lastRun *store.ScheduleRun
		if len(runs) > 0 {
			lastRun = &runs[0] // newest first, per ListScheduleRunsByCronAgent's contract
		}
		if !agent.CronAgentDue(ca, lastRun, now) {
			continue
		}

		// Resolve the referenced Template to run the same opencode-defensive
		// check #233 had on Template.Harness directly -- CronAgent itself
		// carries no Harness field, it inherits one transitively through
		// the Template it references.
		tmpl, err := deps.GetTemplate(ctx, ca.TemplateID)
		if err != nil {
			log.Warn("scheduling tick: loading template for cron agent", "cron_agent_id", ca.ID, "template_id", ca.TemplateID, "error", err)
			continue
		}
		if tmpl.Harness == config.HarnessOpenCode {
			log.Warn("scheduling tick: cron agent's template has the opencode harness, which scheduling does not support; skipping", "cron_agent_id", ca.ID)
			continue
		}

		a, err := deps.CreateFromCronAgent(ctx, ca)
		if err != nil {
			// A failed attempt still gets a ScheduleRun (with an empty
			// AgentID), finalized as an error right away: it shows up in
			// the run history, and it advances CronAgentDue's anchor so a
			// persistently-failing cron agent is retried at its next cron
			// fire, not on every single tick.
			log.Warn("scheduling tick: creating agent from cron agent", "cron_agent_id", ca.ID, "error", err)
			run, rerr := deps.CreateScheduleRun(ctx, ca.ID, "")
			if rerr != nil {
				log.Warn("scheduling tick: recording a failed schedule run", "cron_agent_id", ca.ID, "error", rerr)
				continue
			}
			if _, ferr := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); ferr != nil {
				log.Warn("scheduling tick: finalizing a failed schedule run", "run_id", run.ID, "error", ferr)
			}
			continue
		}
		if _, err := deps.CreateScheduleRun(ctx, ca.ID, a.ID); err != nil {
			log.Warn("scheduling tick: recording schedule run", "cron_agent_id", ca.ID, "agent_id", a.ID, "error", err)
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
			finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusError)
			continue
		case "gone":
			if _, err := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); err != nil {
				log.Warn("scheduling tick: finalizing a gone run as error", "run_id", run.ID, "error", err)
			}
			continue
		}
		// status == "done"
		finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusSuccess)
	}
}

// finishScheduledRun applies run's cron agent's OnComplete policy, then
// finalizes run with status. Shared by the done, failed and timed-out
// paths, which differ only in that final status.
func finishScheduledRun(ctx context.Context, deps schedulingDeps, log *slog.Logger, run store.ScheduleRun, status string) {
	// A CronAgent deleted while this run was in flight -- allowed, since a
	// CronAgent's own lifecycle is independent of both its runs and their
	// agents -- leaves ca as the zero value here, whose OnComplete ("")
	// ApplyOnComplete's switch treats as "leave" (its safest no-op
	// default). The run still gets finalized below either way; only the
	// cleanup policy silently becomes "leave" instead of whatever the
	// deleted cron agent specified.
	ca, err := deps.GetCronAgent(ctx, run.CronAgentID)
	if err != nil && !store.IsCronAgentNotFound(err) {
		log.Warn("scheduling tick: loading cron agent for finished run", "run_id", run.ID, "cron_agent_id", run.CronAgentID, "error", err)
		return
	}
	if err := deps.ApplyOnComplete(ctx, ca, run); err != nil {
		log.Warn("scheduling tick: applying on-complete policy", "run_id", run.ID, "error", err)
	}
	if _, err := deps.FinishScheduleRun(ctx, run.ID, status); err != nil {
		log.Warn("scheduling tick: finalizing schedule run", "run_id", run.ID, "error", err)
	}
}
