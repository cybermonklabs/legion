package exp7

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings. They are short on purpose: the whole run, kill and recovery
// included, has to finish in well under two minutes.
const (
	// Hold bounds the block inside the step. It must outlast the kill, the replacement
	// process's start and the driver's release, and still end a run whose driver died.
	Hold = 70 * time.Second

	workerStartWait = 40 * time.Second
	effectWait      = 25 * time.Second
	recoveryWait    = 45 * time.Second
	completionWait  = 30 * time.Second
	// otherExecutorObserve is how long a process under a different executor id is left
	// running beside the dead one's work, to see whether it takes it.
	otherExecutorObserve = 6 * time.Second
	settle               = 500 * time.Millisecond
	shutdownTimeout      = 5 * time.Second
	// OtherExecutorID is that process's identity: a live DBOS process of the same
	// application, the same version and the same code, differing from the dead one in
	// nothing but its executor id.
	OtherExecutorID = "exp7-other-host"
)

// Run is experiment 7. It starts a worker process, lets its workflow perform a side
// effect and block inside the step, kills that process with SIGKILL, and then measures
// what DBOS did: what its tables say at each stage, whether anything recovered the
// workflow, and above all how many times the side effect happened.
//
// suppressKill is the run's control: with it the driver sends no signal, so nothing is
// interrupted and nothing is recovered, and the experiment must report that it measured
// nothing.
func Run(ctx context.Context, cfg harness.Config, suppressKill bool) error {
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

	schema := dbosx.SchemaName("exp7")
	observer, err := dbosx.Open(ctx, dbosx.Options{
		DSN: cfg.PostgresDSN, Schema: schema, AppName: AppName,
		AppVersion: AppVersion, ExecutorID: "exp7-driver",
	})
	if err != nil {
		return err
	}
	pool, err := dbosx.Pool(observer)
	if err != nil {
		return err
	}
	defer func() {
		// The driver's context was never launched, so it runs nothing and its pool is
		// still open here: the schema goes away before the connection that drops it.
		if err := dbosx.DropSchema(context.WithoutCancel(ctx), pool, schema); err != nil {
			harness.Fact("schema-dropped", err)
		} else {
			harness.Fact("schema-dropped", schema)
		}
		_ = dbos.Shutdown(observer, shutdownTimeout)
	}()

	workflowID := fmt.Sprintf("exp7-prompt-%d", time.Now().UnixNano())
	harness.Fact("system-database", dbosx.EvalDatabase)
	harness.Fact("system-database-schema", schema)
	harness.Fact("application-name", AppName)
	harness.Fact("application-version", AppVersion)
	harness.Fact("executor-id", ExecutorID)
	harness.Fact("workflow-id", workflowID)
	harness.Fact("side-effect-file", sideEffects)

	// 1. The process that will be killed.
	first, err := startWorker(ctx, "worker1", worker, cfg, schema, workflowID, workDir, true)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("worker1-pid", first.PID())

	// 2. Wait until the step has performed its side effect and is blocked in it. Only
	// then can a kill land inside the step rather than before or after it.
	lines, err := dbosx.WaitForLines(ctx, sideEffects, 1, effectWait)
	if err != nil {
		return fmt.Errorf("the step never performed its side effect: %w", err)
	}
	harness.Fact("side-effect-1", lines[0])
	harness.Fact("release-file-exists-while-step-blocked", dbosx.Exists(release))

	before, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	beforeSteps, err := dbosx.ReadSteps(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("sql-before-kill-workflow_status", before)
	harness.Fact("sql-before-kill-operation_outputs", dbosx.StepNames(beforeSteps))
	harness.Fact("sql-before-kill-recorded-step-count", len(beforeSteps))

	// 3. The kill itself, by the exact pid the driver recorded.
	killSent := false
	killedAt := time.Now()
	switch {
	case suppressKill:
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
		harness.Fact("kill-suppressed", true)
	case !first.Alive():
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
		harness.Fact("worker1-already-gone", true)
	default:
		killedAt = time.Now()
		if err := first.Signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("kill worker1: %w", err)
		}
		killSent = true
		harness.Fact("kill-sent", true)
		harness.Fact("killed-pid", first.PID())
		harness.Fact("kill-signal", "SIGKILL")
		harness.Fact("killed-process-still-alive", first.Alive())
	}

	// 4. What the model holds about a step that ran and whose outcome nobody recorded.
	afterKill, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	afterKillSteps, err := dbosx.ReadSteps(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-kill-workflow_status", afterKill)
	harness.Fact("sql-after-kill-operation_outputs", dbosx.StepNames(afterKillSteps))
	harness.Fact("sql-after-kill-recorded-step-count", len(afterKillSteps))
	harness.Fact("state-for-a-step-that-ran-with-its-outcome-unknown",
		"none: DBOS writes an operation_outputs row only when a step returns, so a step killed inside its body leaves no row at all, and workflow_status reads PENDING -- the same two readings a workflow that never entered the step gives")
	harness.Fact("in-flight-step-marker-in-the-schema",
		"none: the system schema has no started_at, no lease and no attempt row per step; workflow_status.started_at_epoch_ms and owner_xid are per workflow, not per step")

	// 5. Is the work bound to the dead process? A second live process of the same
	// application, the same version and the same code, differing only in its executor
	// id, is given the chance to take it.
	if killSent {
		if err := probeOtherExecutor(ctx, cfg, schema, workflowID, pool, sideEffects); err != nil {
			return err
		}
	} else {
		harness.Fact("other-executor-probe", "not measured: nothing was killed")
	}

	// 6. The replacement process, under the dead process's executor id.
	if killSent {
		second, err := startWorker(ctx, "worker2", worker, cfg, schema, workflowID, workDir, false)
		if err != nil {
			return err
		}
		defer second.Stop()
		harness.Fact("replacement-pid", second.PID())
		lines, err = dbosx.WaitForLines(ctx, sideEffects, 2, recoveryWait)
		if err != nil {
			harness.Fact("workflow-recovered", false)
			harness.Fact("recovery-wait-error", err)
		} else {
			harness.Fact("workflow-recovered", true)
			harness.Fact("recovered-by", "the replacement process itself, inside dbos.Launch: it re-enqueues the PENDING workflows whose executor_id is its own and whose application_version is its own, then its queue runner dequeues them")
			harness.Fact("recovery-latency-seconds", fmt.Sprintf("%.3f", time.Since(killedAt).Seconds()))
			harness.Fact("side-effect-2", lines[1])
			inFlight, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
			if err != nil {
				return err
			}
			harness.Fact("sql-during-recovered-attempt-workflow_status", inFlight)
		}
	}

	// 7. Release the step, so the attempt that is running now can finish and record
	// what it did. Nothing below is reached until the workflow is terminal.
	if err := dbosx.Touch(release); err != nil {
		return err
	}
	final, err := waitTerminal(ctx, pool, schema, workflowID, completionWait)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-workflow_status", final)
	finalSteps, err := dbosx.ReadSteps(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-operation_outputs", dbosx.StepNames(finalSteps))
	for _, step := range finalSteps {
		harness.Fact("sql-final-step", step)
		if step.Output != nil {
			harness.Fact("sql-final-step-output-decoded", dbosx.Decode(observer, *step.Output, step.Serialization))
		}
	}
	if final.Output != nil {
		harness.Fact("sql-final-workflow-output-decoded", dbosx.Decode(observer, *final.Output, ""))
	}

	// 8. The number the experiment exists to produce.
	lines, err = dbosx.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	effects := len(lines)
	for i, line := range lines {
		harness.Fact(fmt.Sprintf("side-effect-line-%d", i+1), line)
	}
	harness.Fact("side-effect-performed-times", effects)
	harness.Fact("steps-recorded-in-postgres", len(finalSteps))
	if effects > 1 {
		harness.Fact("what-that-means",
			"a step whose outcome DBOS never recorded is run again by the replacement: DBOS's guarantee for a step is at-least-once, and exactly-once only for a step whose outcome did reach operation_outputs")
	} else {
		harness.Fact("what-that-means",
			"nothing was interrupted, so this run saw only that an uninterrupted step runs once; what an interrupted one does is what the kill measures")
	}

	// 9. What binding the work to the dead process would mean.
	harness.Fact("work-bound-to-the-dead-process-by", "workflow_status.executor_id, written at dispatch")
	harness.Fact("binding-is-exclusive-until-a-process-of-that-id-returns",
		"yes for recovery: launch recovers only its own executor id, so no other process picks the work up on its own")
	harness.Fact("if-that-process-never-comes-back",
		"the row stays PENDING for ever: nothing in the OSS library times an executor out, so the work waits for a process that opens DBOS under the same executor id and the same application version, or for an operator calling ResumeWorkflow, RewindWorkflow or ForkWorkflow")

	harness.Fact("limit", "one workflow, one step, one kill, on one PostgreSQL: it shows what DBOS records and re-runs when the process inside a step dies, not how a fleet of executors behaves, nor what happens when the database rather than the process is the thing that fails")

	// 10. The verdict. A run that killed nothing measured nothing, whatever else it saw.
	switch {
	case !killSent:
		return fmt.Errorf("no process was killed, so this run measured nothing about what DBOS does to a step that was interrupted: the side effect happened %d time(s) and no recovery took place", effects)
	case effects < 2:
		return fmt.Errorf("the side effect happened %d time(s): the kill landed but nothing re-ran the step, so the recovery outcome was not measured", effects)
	case final.Status != string(dbos.WorkflowStatusSuccess):
		return fmt.Errorf("the recovered workflow ended %s, not SUCCESS, so what DBOS recorded for the step was not read back", final.Status)
	case len(finalSteps) == 0:
		return fmt.Errorf("PostgreSQL holds no operation_outputs row for the step, so its recorded outcome was not read back")
	}
	return nil
}

// startWorker starts one cmd/dbosworker process in experiment 7's mode.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, workflowID, workDir string, start bool) (*dbosx.Child, error) {
	args := []string{
		"-mode", "exp7",
		"-postgres-dsn", cfg.PostgresDSN,
		"-schema", schema,
		"-app-name", AppName,
		"-app-version", AppVersion,
		"-executor-id", ExecutorID,
		"-workflow-id", workflowID,
		"-work-dir", workDir,
	}
	if start {
		args = append(args, "-start")
	}
	return dbosx.StartChild(ctx, name, binary, args, workDir, workerStartWait)
}

// probeOtherExecutor runs a second DBOS process of the same application, version and
// code, under a different executor id, beside the dead process's work. It registers the
// workflow, so if DBOS handed it the work it would really run it, and the side effect
// would show.
func probeOtherExecutor(ctx context.Context, cfg harness.Config, schema, workflowID string, pool dbos.Pool, sideEffects string) error {
	other, err := dbosx.Open(ctx, dbosx.Options{
		DSN: cfg.PostgresDSN, Schema: schema, AppName: AppName,
		AppVersion: AppVersion, ExecutorID: OtherExecutorID, SkipMigrations: true,
	})
	if err != nil {
		return err
	}
	dbos.RegisterWorkflow(other, PromptWorkflow, dbos.WithWorkflowName(WorkflowName))
	if err := dbos.Launch(other); err != nil {
		return err
	}
	harness.Fact("other-executor-id", OtherExecutorID)
	harness.Fact("other-executor-observed-for", otherExecutorObserve.String())
	select {
	case <-ctx.Done():
		_ = dbos.Shutdown(other, shutdownTimeout)
		return ctx.Err()
	case <-time.After(otherExecutorObserve):
	}
	lines, err := dbosx.ReadLines(sideEffects)
	if err != nil {
		_ = dbos.Shutdown(other, shutdownTimeout)
		return err
	}
	during, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
	if err != nil {
		_ = dbos.Shutdown(other, shutdownTimeout)
		return err
	}
	harness.Fact("other-executor-ran-the-step", len(lines) > 1)
	harness.Fact("sql-with-only-another-executor-alive", during)
	harness.Fact("side-effects-while-only-another-executor-was-alive", len(lines))
	// The probe is shut down before the replacement starts, so nothing below can be
	// confused about which process recovered the work.
	return dbos.Shutdown(other, shutdownTimeout)
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
