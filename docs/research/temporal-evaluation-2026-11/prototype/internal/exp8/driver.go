package exp8

import (
	"context"
	"fmt"
	"strconv"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings. A pod cannot wait for days, so the wait this run really
// sits through is seconds and the cost of a wait of days is arithmetic on what those
// seconds measured.
const (
	// Wait is the bound the gate's Recv is given. DBOS has no unbounded receive: the
	// call takes a timeout and records the deadline it computes, so this number is
	// what a wait for a person would have to be set to.
	Wait = 100 * time.Second

	workerStartWait = 40 * time.Second
	questionWait    = 25 * time.Second
	waitingWait     = 45 * time.Second
	answerWait      = 30 * time.Second
	// idleObserve is how long the gate is left standing with nobody answering, so the
	// driver can read what a waiting workflow holds.
	idleObserve     = 3 * time.Second
	settle          = 250 * time.Millisecond
	shutdownTimeout = 5 * time.Second

	// goroutineStackBytes is Go's starting goroutine stack, which the reasoned lines
	// multiply by to price a thousand waits. It is documented, not measured here.
	goroutineStackBytes = 8192
)

// Run is experiment 8: a workflow that waits for an answer only a person can give. It
// measures what the wait holds while nobody answers, kills the process that is waiting
// and lets a replacement resume the wait, then delivers the answer and shows the
// workflow going on from where it waited.
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
	questions := QuestionFile(workDir)
	decisions := DecisionFile(workDir)
	if err := dbosx.RemoveAll(questions, decisions); err != nil {
		return err
	}

	schema := dbosx.SchemaName("exp8")
	observer, err := dbosx.Open(ctx, dbosx.Options{
		DSN: cfg.PostgresDSN, Schema: schema, AppName: AppName,
		AppVersion: AppVersion, ExecutorID: "exp8-driver",
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

	workflowID := fmt.Sprintf("exp8-gate-%d", time.Now().UnixNano())
	harness.Fact("system-database", dbosx.EvalDatabase)
	harness.Fact("system-database-schema", schema)
	harness.Fact("application-version", AppVersion)
	harness.Fact("executor-id", ExecutorID)
	harness.Fact("workflow-id", workflowID)
	harness.Fact("wait-mechanism", "dbos.Recv on a topic, answered by dbos.Send from another process")
	harness.Fact("unbounded-wait-available", "no: Recv takes a required timeout and checkpoints the absolute deadline it computes as a DBOS.sleep step, so an open-ended wait is a timeout long enough to outlast the person")
	harness.Fact("wait-bound-this-run-used", Wait.String())

	// 1. The process that holds the gate.
	first, err := startWorker(ctx, "gate1", worker, cfg, schema, workflowID, workDir, true)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("gate1-pid", first.PID())

	before, err := first.Field(ctx, "goroutines-before-the-workflow", workerStartWait)
	if err != nil {
		return err
	}
	if _, err := dbosx.WaitForLines(ctx, questions, 1, questionWait); err != nil {
		return fmt.Errorf("the gate never asked its question: %w", err)
	}
	askedAt := time.Now()
	waiting, err := first.Field(ctx, "goroutines-while-waiting", waitingWait)
	if err != nil {
		return err
	}

	// 2. What the wait holds, read from the process and from the database.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(idleObserve):
	}
	idle, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	idleSteps, err := dbosx.ReadSteps(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	_, pending, err := dbosx.CountNotifications(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	rows, err := dbosx.RowCounts(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	backends, err := dbosx.ReadBackends(ctx, pool)
	if err != nil {
		return err
	}
	locks, err := dbosx.CountHeldLocks(ctx, pool, schema)
	if err != nil {
		return err
	}
	bytes, err := dbosx.RowBytes(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("sql-while-waiting-workflow_status", idle)
	harness.Fact("sql-while-waiting-operation_outputs", dbosx.StepNames(idleSteps))
	for _, step := range idleSteps {
		harness.Fact("sql-while-waiting-step", step)
		if step.Name == sleepStepName && step.Output != nil {
			harness.Fact("recorded-wait-deadline", deadlineOf(observer, step))
		}
	}
	harness.Fact("sql-while-waiting-unconsumed-notifications", pending)
	harness.Fact("recorded-state-while-waiting", idle.Status)
	harness.Fact("held-while-waiting-goroutines-in-the-waiting-process", fmt.Sprintf("%s before the workflow, %s while it waits", before, waiting))
	harness.Fact("held-while-waiting-goroutine-delta", goroutineDelta(before, waiting))
	harness.Fact("held-while-waiting-connections", backends)
	harness.Fact("held-while-waiting-open-transactions", backends.IdleInTransaction)
	harness.Fact("held-while-waiting-locks-on-the-schema-or-rows", locks)
	harness.Fact("held-while-waiting-rows", rows)
	harness.Fact("held-while-waiting-row-bytes", bytes)
	harness.Fact("connection-model-read-from-the-library", "DBOS holds one pool for the process and one further connection for the LISTEN that wakes a waiting Recv; neither is per waiting workflow, and no connection is held inside the wait")

	// 3. Does the wait survive the process? The process is killed outright.
	killedAt := time.Now()
	if err := first.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill gate1: %w", err)
	}
	harness.Fact("killed-pid", first.PID())
	harness.Fact("kill-signal", "SIGKILL")
	afterKill, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-the-waiting-process-died", afterKill)

	second, err := startWorker(ctx, "gate2", worker, cfg, schema, workflowID, workDir, false)
	if err != nil {
		return err
	}
	defer second.Stop()
	harness.Fact("replacement-pid", second.PID())
	resumedWait, err := second.Field(ctx, "goroutines-while-waiting", waitingWait)
	if err != nil {
		harness.Fact("wait-resumed-after-restart", false)
		return fmt.Errorf("the replacement process never reached the wait: %w", err)
	}
	harness.Fact("wait-resumed-after-restart", true)
	harness.Fact("wait-resumed-after-seconds", fmt.Sprintf("%.3f", time.Since(killedAt).Seconds()))
	harness.Fact("goroutines-in-the-replacement-while-waiting", resumedWait)
	resumed, err := dbosx.ReadWorkflow(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	resumedSteps, err := dbosx.ReadSteps(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	questionLines, err := dbosx.ReadLines(questions)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-the-replacement-resumed-the-wait", resumed)
	harness.Fact("sql-after-the-replacement-operation_outputs", dbosx.StepNames(resumedSteps))
	harness.Fact("question-asked-times", len(questionLines))
	harness.Fact("step-before-the-wait-re-run-on-recovery", len(questionLines) > 1)

	// 4. The person answers.
	answeredAt := time.Now()
	if err := dbos.Send(observer, workflowID, "approved", AnswerTopic); err != nil {
		return fmt.Errorf("send the answer to %s: %w", workflowID, err)
	}
	harness.Fact("answer-sent", "approved")
	final, err := waitTerminal(ctx, pool, schema, workflowID, answerWait)
	if err != nil {
		return err
	}
	harness.Fact("seconds-from-answer-to-terminal", fmt.Sprintf("%.3f", time.Since(answeredAt).Seconds()))
	harness.Fact("sql-final-workflow_status", final)
	finalSteps, err := dbosx.ReadSteps(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-operation_outputs", dbosx.StepNames(finalSteps))
	if final.Output != nil {
		harness.Fact("sql-final-workflow-output-decoded", dbosx.Decode(observer, *final.Output, ""))
	}
	decisionLines, err := dbosx.ReadLines(decisions)
	if err != nil {
		return err
	}
	for i, line := range decisionLines {
		harness.Fact(fmt.Sprintf("decision-line-%d", i+1), line)
	}
	leftover, stillWaiting, err := dbosx.CountNotifications(ctx, pool, schema, workflowID)
	if err != nil {
		return err
	}
	harness.Fact("notifications-rows-after-the-answer", fmt.Sprintf("%d row(s), %d of them unconsumed: DBOS marks a delivered message consumed rather than deleting it", leftover, stillWaiting))

	// 5. What a wait of days costs, split into what this run timed and what it reasons.
	reportCost(bytes, before, waiting, backends, locks, pending, answeredAt.Sub(askedAt))

	harness.Fact("limit", "one gate on one PostgreSQL, waiting seconds rather than days, with one answer: it shows what a DBOS wait holds in a process and in a database and that the wait survives the process dying, while the cost of a real multi-day wait is arithmetic on this run's bytes and goroutines, not a wait this pod sat through")

	switch {
	case len(questionLines) != 1:
		return fmt.Errorf("the step before the wait ran %d times, not once, so the restart did not resume the wait but replayed the work", len(questionLines))
	case backends.IdleInTransaction != 0:
		return fmt.Errorf("the waiting gate left %d connections idle in transaction, so the wait did hold a transaction open", backends.IdleInTransaction)
	case idle.Status != string(dbos.WorkflowStatusPending):
		return fmt.Errorf("the waiting gate was recorded as %s, not PENDING", idle.Status)
	case resumed.RecoveryAttempts < 2:
		return fmt.Errorf("the gate was dispatched %d time(s), so the replacement process did not take the wait over", resumed.RecoveryAttempts)
	case final.Status != string(dbos.WorkflowStatusSuccess):
		return fmt.Errorf("the gate ended %s, not SUCCESS, after the answer was delivered", final.Status)
	case len(decisionLines) != 1:
		return fmt.Errorf("the step after the wait ran %d times, not once", len(decisionLines))
	}
	return nil
}

// reportCost prints what keeping a wait open costs. Every line says which half it
// belongs to: timed: is a number this run measured, reasoned: is arithmetic over a
// span of days no pod can sit through.
func reportCost(bytes int64, before, waiting string, backends dbosx.Backends, locks, pending int64, waited time.Duration) {
	delta := goroutineDelta(before, waiting)
	harness.Fact("timed: row-bytes-for-one-open-wait", bytes)
	harness.Fact("timed: row-bytes-source", "sum(pg_column_size(row)) over workflow_status, workflow_input, workflow_output and operation_outputs for this workflow")
	harness.Fact("timed: goroutines-added-by-one-open-wait", delta)
	harness.Fact("timed: connections-to-the-database-while-one-wait-was-open", backends.Total)
	harness.Fact("timed: connections-idle-in-transaction-while-waiting", backends.IdleInTransaction)
	harness.Fact("timed: locks-held-while-waiting", locks)
	harness.Fact("timed: undelivered-message-rows-while-waiting", pending)
	harness.Fact("timed: wall-clock-this-gate-actually-waited", fmt.Sprintf("%.3fs, from the question being asked to the answer being sent", waited.Seconds()))

	for _, days := range []int{1, 7, 30} {
		harness.Fact(fmt.Sprintf("reasoned: storage-bytes-for-one-gate-open-%d-days", days),
			fmt.Sprintf("%d (the rows do not grow while nothing happens: a wait writes nothing per day, so %d days cost the same %d bytes as one minute)", bytes, days, bytes))
	}
	harness.Fact("reasoned: storage-bytes-for-1000-gates-open-30-days",
		fmt.Sprintf("%d (1000 x %d bytes, on the PostgreSQL Legion already runs, not a metered store)", 1000*bytes, bytes))
	if delta > 0 {
		harness.Fact("reasoned: goroutine-stacks-for-1000-open-gates-in-one-process",
			fmt.Sprintf("%d goroutines, about %d bytes of initial stack (1000 x %d x %d bytes)",
				1000*delta, 1000*delta*goroutineStackBytes, delta, goroutineStackBytes))
	}
	harness.Fact("reasoned: what-a-waiting-gate-costs-per-day",
		"no workflow task, no poll and no write: the waiting process holds its goroutine and the row sits still until a Send writes a notifications row and the LISTEN wakes the waiter")
	harness.Fact("reasoned: what-the-bound-costs",
		fmt.Sprintf("the wait's deadline is recorded when the wait starts, so a gate of days needs its Recv timeout set to days up front (%s here); past it the Recv returns a timeout error and the workflow, not DBOS, decides what an unanswered question means", Wait))
	harness.Fact("reasoned: what-a-pod-cannot-do",
		"this run waited seconds; the lines above extend its measured bytes and goroutines over days by arithmetic, and nothing here observed a gate that really stood for days")
}

// deadlineOf reads the absolute deadline DBOS recorded for the wait.
func deadlineOf(observer dbos.Context, step dbosx.Step) string {
	decoded, err := dbosx.DecodeAny(observer, *step.Output, step.Serialization)
	if err != nil {
		return fmt.Sprintf("undecodable: %v", err)
	}
	ms, ok := decoded.(float64)
	if !ok {
		return fmt.Sprintf("%v (raw %s)", decoded, *step.Output)
	}
	at := time.UnixMilli(int64(ms)).UTC()
	return fmt.Sprintf("%s (epoch-ms %d, recorded as step %d %q)", at.Format(time.RFC3339), int64(ms), step.ID, step.Name)
}

// goroutineDelta is how many goroutines the one open wait added to its process.
func goroutineDelta(before, waiting string) int {
	b, errB := strconv.Atoi(before)
	w, errW := strconv.Atoi(waiting)
	if errB != nil || errW != nil {
		return 0
	}
	return w - b
}

// startWorker starts one cmd/dbosworker process in experiment 8's mode.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, workflowID, workDir string, start bool) (*dbosx.Child, error) {
	args := []string{
		"-mode", "exp8",
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
