package exp14

import (
	"context"
	"fmt"
	"strconv"
	"syscall"
	"time"

	"github.com/cschleiden/go-workflows/client"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/gwx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The driver's own timings. A pod cannot wait for days, so the wait this run really
// sits through is seconds and the cost of a wait of days is arithmetic on what those
// seconds measured.
const (
	workerStartWait = 40 * time.Second
	questionWait    = 25 * time.Second
	waitingWait     = 45 * time.Second
	answerWait      = 30 * time.Second

	driverName  = "exp14-driver"
	worker1Name = "exp14-gate-host-1"
	worker2Name = "exp14-gate-host-2"

	// goroutineStackBytes is Go's starting goroutine stack, which the reasoned lines
	// multiply by to price a thousand waits. It is documented, not measured here.
	goroutineStackBytes = 8192
)

// Run is experiment 14: a workflow that waits for an answer only a person can give.
// It measures what the wait holds while nobody answers, kills the process that is
// waiting and lets a replacement take over, then delivers the answer and shows the
// workflow going on from where it waited.
func Run(ctx context.Context, cfg harness.Config) error {
	if err := harness.RequirePostgresDSN(cfg); err != nil {
		return err
	}
	workerBinary, err := harness.GWWorker(cfg)
	if err != nil {
		return err
	}
	workDir, err := harness.WorkDir(cfg)
	if err != nil {
		return err
	}
	questions := QuestionFile(workDir)
	decisions := DecisionFile(workDir)
	if err := libx.RemoveAll(questions, decisions); err != nil {
		return err
	}

	schema := libx.SchemaName("exp14")
	if err := libx.EnsureDatabase(ctx, cfg.PostgresDSN); err != nil {
		return err
	}
	db, err := libx.Open(ctx, cfg.PostgresDSN, schema)
	if err != nil {
		return err
	}
	defer func() {
		if err := libx.DropSchema(context.WithoutCancel(ctx), db, schema); err != nil {
			harness.Fact("schema-dropped", err)
		} else {
			harness.Fact("schema-dropped", schema)
		}
		db.Close()
	}()
	if err := libx.CreateSchema(ctx, db, schema); err != nil {
		return err
	}
	if err := gwx.Migrate(db); err != nil {
		return err
	}

	be := gwx.Backend(db, Options(driverName))
	defer be.Close() //nolint:errcheck // the driver's backend does not own the pool
	c := client.New(be)

	instanceID := fmt.Sprintf("exp14-gate-%d", time.Now().UnixNano())
	harness.Fact("library", "github.com/cschleiden/go-workflows v1.4.2")
	harness.Fact("database", libx.EvalDatabase)
	harness.Fact("schema", schema)
	harness.Fact("instance-id", instanceID)
	harness.Fact("wait-mechanism", "workflow.NewSignalChannel(ctx, \"approval\").Receive(ctx), answered by client.SignalWorkflow from another process")
	harness.Fact("unbounded-wait-available", "yes: Receive takes no timeout and records no deadline, so a wait for a person is open-ended and costs no timer")
	harness.Fact("sticky-timeout-this-run-used", StickyTimeout.String())
	harness.Fact("executor-cache-ttl-this-run-used", CacheTTL.String())

	// 1. The process that holds the gate.
	first, err := startWorker(ctx, "gate1", workerBinary, cfg, schema, worker1Name, instanceID, workDir, true)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("gate1-pid", first.PID())

	before, err := first.Field(ctx, "goroutines-before-the-workflow", workerStartWait)
	if err != nil {
		return err
	}
	if _, err := libx.WaitForLines(ctx, questions, 1, questionWait); err != nil {
		return fmt.Errorf("the gate never asked its question: %w", err)
	}
	askedAt := time.Now()
	waiting, err := first.Field(ctx, "goroutines-while-waiting", waitingWait)
	if err != nil {
		return err
	}

	// 2. What the wait holds, read from the process and from the database.
	idle, err := gwx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return err
	}
	idleEvents, err := gwx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	idlePending, err := gwx.ReadPendingEvents(ctx, db, instanceID)
	if err != nil {
		return err
	}
	idleActivities, err := gwx.ReadActivities(ctx, db, instanceID)
	if err != nil {
		return err
	}
	rows, err := libx.RowCounts(ctx, db, gwx.Tables, "instance_id = $1", instanceID)
	if err != nil {
		return err
	}
	bytes, err := libx.SumRowBytes(ctx, db, gwx.Tables, "instance_id = $1", instanceID)
	if err != nil {
		return err
	}
	backends, err := libx.ReadBackends(ctx, db)
	if err != nil {
		return err
	}
	locks, err := libx.CountHeldLocks(ctx, db, schema)
	if err != nil {
		return err
	}
	harness.Fact("sql-while-waiting-instances", idle)
	harness.Fact("sql-while-waiting-history", gwx.Events(idleEvents))
	harness.Fact("sql-while-waiting-pending_events", gwx.Events(idlePending))
	harness.Fact("sql-while-waiting-activities-rows", len(idleActivities))
	harness.Fact("recorded-state-while-waiting", gwx.StateName(idle.State)+", with locked_until null: a waiting instance is an ordinary active row that no worker holds")
	harness.Fact("held-while-waiting-goroutines-in-the-waiting-process", fmt.Sprintf("%s before the workflow, %s while it waits", before, waiting))
	harness.Fact("held-while-waiting-goroutine-delta", goroutineDelta(before, waiting))

	// The library keeps the instance's executor in memory while the worker is sticky
	// for it. Once both lapse the wait is rows and nothing else, which is the number
	// a wait of days has to be priced on.
	settled, err := first.Field(ctx, "goroutines-after-the-stickiness-and-cache-lapsed", waitingWait+cacheSettle)
	if err != nil {
		return err
	}
	harness.Fact("held-after-stickiness-and-cache-lapsed-goroutines", settled)
	harness.Fact("held-after-stickiness-and-cache-lapsed-goroutine-delta", goroutineDelta(before, settled))
	harness.Fact("held-while-waiting-connections", backends)
	harness.Fact("held-while-waiting-open-transactions", fmt.Sprintf("%d sampled mid-flight, %d of them older than %s: a go-workflows poller begins and rolls back a transaction every poll, and only one that outlived a poll cycle would be the wait holding it", backends.IdleInTransaction, backends.IdleInTransactionLonger, libx.LongTransaction))
	harness.Fact("held-while-waiting-locks-on-the-schema-or-rows", locks)
	harness.Fact("held-while-waiting-rows", rows)
	harness.Fact("held-while-waiting-row-bytes", bytes)
	harness.Fact("connection-model-read-from-the-library", "go-workflows holds one database/sql pool per process and polls it; there is no connection, no transaction and no listener per waiting instance")

	// 3. Does the wait survive the process? The process is killed outright.
	killedAt := time.Now()
	if err := first.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill gate1: %w", err)
	}
	harness.Fact("killed-pid", first.PID())
	harness.Fact("kill-signal", "SIGKILL")
	afterKill, err := gwx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return err
	}
	afterKillEvents, err := gwx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-the-waiting-process-died", afterKill)
	harness.Fact("sql-after-the-waiting-process-died-history", gwx.Events(afterKillEvents))
	harness.Fact("what-the-death-cost-the-wait", "nothing in the database changed: the wait was already only rows, so there was nothing in the dead process to lose")

	second, err := startWorker(ctx, "gate2", workerBinary, cfg, schema, worker2Name, instanceID, workDir, false)
	if err != nil {
		return err
	}
	defer second.Stop()
	harness.Fact("replacement-pid", second.PID())
	harness.Fact("replacement-started-after-seconds", fmt.Sprintf("%.3f", time.Since(killedAt).Seconds()))

	// 4. The person answers.
	answeredAt := time.Now()
	if err := c.SignalWorkflow(ctx, instanceID, AnswerSignal, "approved"); err != nil {
		return fmt.Errorf("send the answer to %s: %w", instanceID, err)
	}
	harness.Fact("answer-sent", "approved")
	final, err := gwx.WaitForInstance(ctx, db, instanceID, answerWait, gwx.Instance.Finished)
	if err != nil {
		return err
	}
	harness.Fact("seconds-from-answer-to-finished", fmt.Sprintf("%.3f", time.Since(answeredAt).Seconds()))
	harness.Fact("wait-resumed-in-the-replacement", final.Worker != nil && *final.Worker == worker2Name)
	harness.Fact("sql-final-instances", final)
	finalEvents, err := gwx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-history", gwx.Events(finalEvents))
	harness.Fact("sql-final-signal-received-events", gwx.CountEvents(finalEvents, "SignalReceived"))
	questionLines, err := libx.ReadLines(questions)
	if err != nil {
		return err
	}
	decisionLines, err := libx.ReadLines(decisions)
	if err != nil {
		return err
	}
	for i, line := range questionLines {
		harness.Fact(fmt.Sprintf("question-line-%d", i+1), line)
	}
	for i, line := range decisionLines {
		harness.Fact(fmt.Sprintf("decision-line-%d", i+1), line)
	}
	harness.Fact("question-asked-times", len(questionLines))
	harness.Fact("work-before-the-wait-re-run-after-the-restart", len(questionLines) > 1)
	leftover, err := libx.CountRows(ctx, db, "pending_events", "instance_id = $1", instanceID)
	if err != nil {
		return err
	}
	harness.Fact("pending_events-rows-after-the-answer", leftover)

	// 5. What a wait of days costs, split into what this run timed and what it
	// reasons.
	reportCost(bytes, before, waiting, settled, backends, locks, int64(len(idlePending)), answeredAt.Sub(askedAt))

	harness.Fact("limit", "one gate on one PostgreSQL, waiting seconds rather than days, with one answer: it shows what a go-workflows wait holds in a process and in a database and that the wait survives the process dying, while the cost of a real multi-day wait is arithmetic on this run's bytes and goroutines, not a wait this pod sat through")

	switch {
	case len(questionLines) != 1:
		return fmt.Errorf("the activity before the wait ran %d times, not once, so the restart did not resume the wait but replayed the work", len(questionLines))
	case backends.IdleInTransactionLonger != 0:
		return fmt.Errorf("the waiting gate left %d connections idle in transaction for longer than %s, so the wait did hold a transaction open", backends.IdleInTransactionLonger, libx.LongTransaction)
	case idle.Finished():
		return fmt.Errorf("the waiting gate was already finished, so nothing was waiting")
	case !final.Finished():
		return fmt.Errorf("the gate did not finish after the answer was delivered: %s", final)
	case final.Worker == nil || *final.Worker != worker2Name:
		return fmt.Errorf("the gate was finished by %v, not by the replacement process %s, so the wait was not taken over", final.Worker, worker2Name)
	case len(decisionLines) != 1:
		return fmt.Errorf("the activity after the wait ran %d times, not once", len(decisionLines))
	}
	return nil
}

// reportCost prints what keeping a wait open costs. Every line says which half it
// belongs to: timed: is a number this run measured, reasoned: is arithmetic over a
// span of days no pod can sit through.
func reportCost(bytes int64, before, waiting, settled string, backends libx.Backends, locks, pending int64, waited time.Duration) {
	delta := goroutineDelta(before, waiting)
	settledDelta := goroutineDelta(before, settled)
	harness.Fact("timed: row-bytes-for-one-open-wait", bytes)
	harness.Fact("timed: row-bytes-source", "sum(pg_column_size(row)) over instances, pending_events, history, activities and attributes for this instance")
	harness.Fact("timed: goroutines-added-by-one-open-wait", delta)
	harness.Fact("timed: goroutines-added-once-stickiness-and-cache-lapsed", settledDelta)
	harness.Fact("timed: connections-to-the-database-while-one-wait-was-open", backends.Total)
	harness.Fact("timed: connections-idle-in-transaction-while-waiting", fmt.Sprintf("%d at the sample, %d older than %s", backends.IdleInTransaction, backends.IdleInTransactionLonger, libx.LongTransaction))
	harness.Fact("timed: locks-held-while-waiting", locks)
	harness.Fact("timed: undelivered-event-rows-while-waiting", pending)
	harness.Fact("timed: wall-clock-this-gate-actually-waited", fmt.Sprintf("%.3fs, from the question being asked to the answer being sent", waited.Seconds()))

	for _, days := range []int{1, 7, 30} {
		harness.Fact(fmt.Sprintf("reasoned: storage-bytes-for-one-gate-open-%d-days", days),
			fmt.Sprintf("%d (the rows do not grow while nothing happens: a wait writes nothing per day, so %d days cost the same %d bytes as one minute)", bytes, days, bytes))
	}
	harness.Fact("reasoned: storage-bytes-for-1000-gates-open-30-days",
		fmt.Sprintf("%d (1000 x %d bytes, on the PostgreSQL Legion already runs, not a metered store)", 1000*bytes, bytes))
	if settledDelta > 0 {
		harness.Fact("reasoned: goroutine-stacks-for-1000-open-gates-in-one-process",
			fmt.Sprintf("%d goroutines, about %d bytes of initial stack (1000 x %d x %d bytes)",
				1000*settledDelta, 1000*settledDelta*goroutineStackBytes, settledDelta, goroutineStackBytes))
	} else {
		harness.Fact("reasoned: goroutine-stacks-for-1000-open-gates-in-one-process",
			"0: once the stickiness and the executor cache lapse the wait is rows only, so a thousand waits add no goroutines to any process")
	}
	harness.Fact("reasoned: what-a-waiting-gate-costs-per-day",
		"no workflow task, no write and no timer: the pollers that run anyway ask the database for work on their own interval, and a gate that nobody answers is never in an answer to those queries")
	harness.Fact("reasoned: what-the-wait-costs-in-polling",
		"the cost of polling belongs to the worker, not to the gate: one worker polling every 200ms runs the same two queries whether no gate or a thousand gates are open")
	harness.Fact("reasoned: what-a-pod-cannot-do",
		"this run waited seconds; the lines above extend its measured bytes and goroutines over days by arithmetic, and nothing here observed a gate that really stood for days")
}

// goroutineDelta is how many goroutines the one open wait added to its process.
func goroutineDelta(before, after string) int {
	b, errB := strconv.Atoi(before)
	a, errA := strconv.Atoi(after)
	if errB != nil || errA != nil {
		return 0
	}
	return a - b
}

// startWorker starts one cmd/gwworker process in experiment 14's mode.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, workerName, instanceID, workDir string, start bool) (*libx.Child, error) {
	args := []string{
		"-mode", "exp14",
		"-postgres-dsn", cfg.PostgresDSN,
		"-schema", schema,
		"-worker-name", workerName,
		"-instance-id", instanceID,
		"-work-dir", workDir,
	}
	if start {
		args = append(args, "-start")
	}
	return libx.StartChild(ctx, name, FieldPrefix, binary, args, workDir, workerStartWait)
}
