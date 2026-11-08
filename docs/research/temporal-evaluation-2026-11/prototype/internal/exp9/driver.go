package exp9

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings.
const (
	// Hold bounds the deliver step's wait for its release, so no run of this
	// experiment can hang a process.
	Hold = 70 * time.Second

	workerStartWait = 40 * time.Second
	effectWait      = 25 * time.Second
	terminalWait    = 40 * time.Second
	// gateObserve is how long a live process of the changed code is left beside the
	// stranded run, to see whether it ever takes it.
	gateObserve     = 6 * time.Second
	settle          = 250 * time.Millisecond
	shutdownTimeout = 5 * time.Second
)

// Run is experiment 9: a run recorded before the workflow's code changed, and a
// process running the changed code let loose on it. It measures what DBOS does --
// refuse, re-run, skip, or keep the two apart behind a version gate -- reading
// DBOS's own tables at every stage, and then shows the supported way to make the
// same change.
func Run(ctx context.Context, cfg harness.Config) error {
	if err := harness.RequirePostgresDSN(cfg); err != nil {
		return err
	}
	worker, err := harness.DBOSWorker(cfg)
	if err != nil {
		return err
	}
	workDir, err := harness.WorkDir(cfg)
	if err != nil {
		return err
	}
	sideEffects := SideEffectFile(workDir)
	release := ReleaseFile(workDir)
	if err := dbosx.RemoveAll(sideEffects, release); err != nil {
		return err
	}

	schema := dbosx.SchemaName("exp9")
	observer, err := dbosx.Open(ctx, dbosx.Options{
		DSN: cfg.PostgresDSN, Schema: schema, AppName: AppName,
		AppVersion: RecordedVersion, ExecutorID: "exp9-driver",
	})
	if err != nil {
		return err
	}
	pool, err := dbosx.Pool(observer)
	if err != nil {
		return err
	}
	defer func() {
		if err := dbosx.DropSchema(context.WithoutCancel(ctx), pool, schema); err != nil {
			harness.Fact("schema-dropped", err)
		} else {
			harness.Fact("schema-dropped", schema)
		}
		_ = dbos.Shutdown(observer, shutdownTimeout)
	}()

	recorded := fmt.Sprintf("exp9-phase-%d", time.Now().UnixNano())
	harness.Fact("system-database", dbosx.EvalDatabase)
	harness.Fact("system-database-schema", schema)
	harness.Fact("workflow-name", WorkflowName)
	harness.Fact("recorded-run-workflow-id", recorded)
	harness.Fact("code-before-the-change", strings.Join([]string{StepPlan, StepImplement, StepDeliver}, ", "))
	harness.Fact("code-after-the-change", strings.Join([]string{StepPlan, StepReview, StepImplement, StepDeliver}, ", "))
	harness.Fact("the-change", "one new step, "+StepReview+", inserted in the middle")

	// 1. Record a run under the old code, and leave it unfinished.
	child, err := startWorker(ctx, "oldcode", worker, cfg, schema, recorded, workDir)
	if err != nil {
		return err
	}
	defer child.Stop()
	harness.Fact("old-code-pid", child.PID())
	if _, err := dbosx.WaitForLines(ctx, sideEffects, 3, effectWait); err != nil {
		return fmt.Errorf("the recorded run never reached its last step: %w", err)
	}
	beforeKill, err := dbosx.ReadWorkflow(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	beforeSteps, err := dbosx.ReadSteps(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	harness.Fact("sql-recorded-run-before-kill", beforeKill)
	harness.Fact("sql-recorded-run-steps-before-kill", dbosx.StepNames(beforeSteps))
	if err := child.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill the old-code process: %w", err)
	}
	harness.Fact("old-code-process-killed", child.PID())
	stranded, err := dbosx.ReadWorkflow(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	strandedSteps, err := dbosx.ReadSteps(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	harness.Fact("sql-stranded-run", stranded)
	harness.Fact("sql-stranded-run-steps", dbosx.StepNames(strandedSteps))
	// Every run from here on finishes its last step at once.
	if err := dbosx.Touch(release); err != nil {
		return err
	}

	// 2. A process running the changed code, under the same executor id.
	changed, err := dbosx.Open(ctx, dbosx.Options{
		DSN: cfg.PostgresDSN, Schema: schema, AppName: AppName,
		AppVersion: ChangedVersion, ExecutorID: ExecutorID, SkipMigrations: true,
	})
	if err != nil {
		return err
	}
	changedDown := false
	defer func() {
		if !changedDown {
			_ = dbos.Shutdown(changed, shutdownTimeout)
		}
	}()
	dbos.RegisterWorkflow(changed, ChangedWorkflow, dbos.WithWorkflowName(WorkflowName))
	if err := dbos.Launch(changed); err != nil {
		return err
	}
	harness.Fact("changed-code-executor-id", ExecutorID)
	harness.Fact("changed-code-application-version", ChangedVersion)
	if err := observe(ctx, gateObserve); err != nil {
		return err
	}
	afterLaunch, err := dbosx.ReadWorkflow(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	linesAfterLaunch, err := dbosx.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	harness.Fact("changed-code-observed-for", gateObserve.String())
	harness.Fact("sql-after-the-changed-code-launched", afterLaunch)
	harness.Fact("changed-code-recovered-the-recorded-run", afterLaunch.Status != string(dbos.WorkflowStatusPending))
	harness.Fact("side-effect-lines-after-the-changed-code-launched", len(linesAfterLaunch))
	harness.Fact("version-gate-exists", true)
	harness.Fact("version-gate-where",
		"two places: launch-time recovery re-enqueues only PENDING rows whose application_version equals the process's own, and a dequeue takes only rows of its own version (plus rows with no version at all, when the process is on the latest registered version)")
	harness.Fact("version-gate-keyed-on",
		"workflow_status.application_version, written at dispatch from the process's application version: Config.ApplicationVersion, or DBOS__APPVERSION, or -- when neither is set -- sha256 of the running executable's hash and the application name, so an ordinary redeploy of changed code is a new version")
	harness.Fact("why-not-recovered-here",
		"the executor id matched; only the version differed, and experiment 7 shows the same executor id with a matching version does recover")

	// 3. The operator's obvious move: resume it.
	if _, err := dbos.ResumeWorkflow[string](observer, recorded); err != nil {
		return fmt.Errorf("resume %s: %w", recorded, err)
	}
	afterResume, err := dbosx.ReadWorkflow(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	harness.Fact("sql-right-after-resume", afterResume)
	if err := observe(ctx, gateObserve); err != nil {
		return err
	}
	resumeSettled, err := dbosx.ReadWorkflow(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	resumeLines, err := dbosx.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-resume-and-waiting", resumeSettled)
	harness.Fact("resumed-run-was-dispatched", resumeSettled.Status != string(dbos.WorkflowStatusEnqueued))
	harness.Fact("side-effect-lines-after-resume", len(resumeLines))
	harness.Fact("what-happens-to-a-pending-workflow-whose-code-moved-on",
		"it sits: resume puts it back on the internal queue under its recorded version, and a process on another version never dequeues it, so it waits for a process on its own version or for an operator to restamp it")

	// 4. Restamping it onto the running version, keeping the recorded prefix.
	_, rewindWhileLive := dbos.RewindWorkflow[string](changed, recorded,
		dbos.WithRewindStartStep(2), dbos.WithRewindApplicationVersion(ChangedVersion))
	harness.Fact("rewind-refused-while-not-terminal", errText(rewindWhileLive))
	if err := dbos.CancelWorkflow(observer, recorded); err != nil {
		return fmt.Errorf("cancel %s: %w", recorded, err)
	}
	cancelled, err := dbosx.ReadWorkflow(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-cancel", cancelled)
	if _, err := dbos.RewindWorkflow[string](changed, recorded,
		dbos.WithRewindStartStep(2), dbos.WithRewindApplicationVersion(ChangedVersion)); err != nil {
		return fmt.Errorf("rewind %s onto %s: %w", recorded, ChangedVersion, err)
	}
	harness.Fact("restamped-onto", fmt.Sprintf("%s, keeping the first two recorded steps", ChangedVersion))
	replayed, err := waitTerminal(ctx, pool, schema, recorded, terminalWait)
	if err != nil {
		return err
	}
	replayedSteps, err := dbosx.ReadSteps(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	replayLines, err := dbosx.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-the-changed-code-replayed-it", replayed)
	harness.Fact("sql-after-the-changed-code-steps", dbosx.StepNames(replayedSteps))
	harness.Fact("changed-code-replay-outcome", replayed.Status)
	harness.Fact("changed-code-replay-error-text", listedError(changed, recorded))
	harness.Fact("side-effect-lines-after-the-changed-code-replayed-it", len(replayLines))
	harness.Fact("what-the-changed-code-did",
		"it refused: DBOS compares the step name the code presents at a step id with the function_name recorded there, and a step inserted in the middle shifts every later id, so the first shifted step is rejected before its body runs")
	harness.Fact("did-it-re-run-the-steps-before-the-change", "no: they were returned from their records, unchanged")
	harness.Fact("did-it-skip-the-new-step", "no: the new step never ran, and nothing was written for it")

	// 5. The supported way: the same change made behind a patch.
	if err := dbos.Shutdown(changed, shutdownTimeout); err != nil {
		return fmt.Errorf("shut down the changed-code process: %w", err)
	}
	changedDown = true
	patched, err := dbosx.Open(ctx, dbosx.Options{
		DSN: cfg.PostgresDSN, Schema: schema, AppName: AppName,
		AppVersion: PatchedVersion, ExecutorID: ExecutorID, SkipMigrations: true,
	})
	if err != nil {
		return err
	}
	defer dbos.Shutdown(patched, shutdownTimeout) //nolint:errcheck // the verdict is printed by then
	dbos.RegisterWorkflow(patched, PatchedWorkflow, dbos.WithWorkflowName(WorkflowName))
	if err := dbos.Launch(patched); err != nil {
		return err
	}
	harness.Fact("supported-way", "dbos.Patch: the inserted step runs only for a workflow that had not already recorded a step at that id, and dbos.DeprecatePatch removes the guard later")
	harness.Fact("patched-code-application-version", PatchedVersion)

	if _, err := dbos.RewindWorkflow[string](patched, recorded,
		dbos.WithRewindStartStep(2), dbos.WithRewindApplicationVersion(PatchedVersion)); err != nil {
		return fmt.Errorf("rewind %s onto %s: %w", recorded, PatchedVersion, err)
	}
	oldRun, err := waitTerminal(ctx, pool, schema, recorded, terminalWait)
	if err != nil {
		return err
	}
	oldRunSteps, err := dbosx.ReadSteps(ctx, pool, schema, recorded)
	if err != nil {
		return err
	}
	harness.Fact("sql-old-run-under-the-patched-code", oldRun)
	harness.Fact("sql-old-run-steps-under-the-patched-code", dbosx.StepNames(oldRunSteps))
	harness.Fact("old-run-took-the-new-step", tookReview(oldRunSteps))

	fresh := fmt.Sprintf("exp9-fresh-%d", time.Now().UnixNano())
	freshHandle, err := dbos.RunWorkflow(patched, PatchedWorkflow, NewParams(workDir, PatchedVersion),
		dbos.WithWorkflowID(fresh))
	if err != nil {
		return fmt.Errorf("start %s: %w", fresh, err)
	}
	if _, err := freshHandle.GetResult(); err != nil {
		return fmt.Errorf("the new run under the patched code failed: %w", err)
	}
	freshRow, err := dbosx.ReadWorkflow(ctx, pool, schema, fresh)
	if err != nil {
		return err
	}
	freshSteps, err := dbosx.ReadSteps(ctx, pool, schema, fresh)
	if err != nil {
		return err
	}
	harness.Fact("new-run-workflow-id", fresh)
	harness.Fact("sql-new-run-under-the-patched-code", freshRow)
	harness.Fact("sql-new-run-steps-under-the-patched-code", dbosx.StepNames(freshSteps))
	harness.Fact("new-run-took-the-new-step", tookReview(freshSteps))

	allLines, err := dbosx.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	for i, line := range allLines {
		harness.Fact(fmt.Sprintf("side-effect-line-%d", i+1), line)
	}
	harness.Fact("side-effect-lines-total", len(allLines))
	harness.Fact("limit", "one workflow of four steps, one inserted step, one stranded run, on one PostgreSQL: it shows the version gate, what an unguarded change does to a recorded prefix and what a patch does instead, not what a fleet half-way through a deployment does, nor what happens to a change that reorders or removes a step rather than inserting one")

	switch {
	case len(strandedSteps) < 2:
		return fmt.Errorf("the recorded run left %d steps behind, so there was no recorded prefix to replay against", len(strandedSteps))
	case afterLaunch.Status != string(dbos.WorkflowStatusPending):
		return fmt.Errorf("the recorded run was %s after the changed code launched, so the version gate was not observed", afterLaunch.Status)
	case resumeSettled.Status != string(dbos.WorkflowStatusEnqueued):
		return fmt.Errorf("the resumed run was %s, not left ENQUEUED, so the dequeue gate was not observed", resumeSettled.Status)
	case rewindWhileLive == nil:
		return fmt.Errorf("rewinding a live workflow was accepted, so what DBOS requires before a restamp was not observed")
	case replayed.Status != string(dbos.WorkflowStatusError):
		return fmt.Errorf("the changed code's replay ended %s, not ERROR, so what an unguarded change does was not observed", replayed.Status)
	case oldRun.Status != string(dbos.WorkflowStatusSuccess):
		return fmt.Errorf("the old run under the patched code ended %s, not SUCCESS", oldRun.Status)
	case tookReview(oldRunSteps):
		return fmt.Errorf("the old run took the inserted step, so the patch did not hold the change back from it")
	case !tookReview(freshSteps):
		return fmt.Errorf("the new run did not take the inserted step, so the patch did not apply the change at all")
	}
	return nil
}

// tookReview reports whether a run really ran the inserted step.
func tookReview(steps []dbosx.Step) bool {
	for _, step := range steps {
		if step.Name == StepReview {
			return true
		}
	}
	return false
}

// listedError is the error DBOS recorded for a workflow, as the SDK hands it back.
func listedError(ctx dbos.Context, workflowID string) string {
	list, err := dbos.ListWorkflows(ctx, dbos.WithFilterWorkflowIDs(workflowID))
	if err != nil {
		return fmt.Sprintf("unreadable: %v", err)
	}
	if len(list) == 0 {
		return "no row"
	}
	return errText(list[0].Error)
}

func errText(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

// observe waits, so the experiment can say that it gave DBOS time to act and DBOS did
// not act.
func observe(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// startWorker starts the one cmd/dbosworker process of this experiment: the process
// running the code as it stood before the change.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, workflowID, workDir string) (*dbosx.Child, error) {
	args := []string{
		"-mode", "exp9",
		"-postgres-dsn", cfg.PostgresDSN,
		"-schema", schema,
		"-app-name", AppName,
		"-app-version", RecordedVersion,
		"-executor-id", ExecutorID,
		"-workflow-id", workflowID,
		"-work-dir", workDir,
		"-start",
	}
	return dbosx.StartChild(ctx, name, binary, args, workDir, workerStartWait)
}

// waitTerminal blocks until the workflow reaches a status it cannot leave on its own.
func waitTerminal(ctx context.Context, pool dbos.Pool, schema, workflowID string, timeout time.Duration) (dbosx.Workflow, error) {
	deadline := time.Now().Add(timeout)
	for {
		w, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
		if err != nil {
			return dbosx.Workflow{}, err
		}
		switch w.Status {
		case string(dbos.WorkflowStatusSuccess), string(dbos.WorkflowStatusError),
			string(dbos.WorkflowStatusCancelled), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
			return w, nil
		}
		if !time.Now().Before(deadline) {
			return w, fmt.Errorf("workflow %s was still %s after %s", workflowID, w.Status, timeout)
		}
		select {
		case <-ctx.Done():
			return w, ctx.Err()
		case <-time.After(settle):
		}
	}
}
