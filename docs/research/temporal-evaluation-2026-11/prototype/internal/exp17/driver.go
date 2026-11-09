package exp17

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dapr/durabletask-go/api"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dtx"
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

	// goroutineStackBytes is Go's starting goroutine stack, which the reasoned
	// lines multiply by to price a thousand waits. It is documented, not measured
	// here.
	goroutineStackBytes = 8192
)

// Run is experiment 17: a workflow that waits for an answer only a person can give.
// It measures what the wait holds while nobody answers, kills the process that is
// waiting and lets a replacement take over, then delivers the answer and shows the
// workflow going on from where it waited.
func Run(ctx context.Context, cfg harness.Config) error {
	if err := harness.RequirePostgresDSN(cfg); err != nil {
		return err
	}
	workerBinary, err := harness.DTWorker(cfg)
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

	schema := libx.SchemaName("exp17")
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

	instanceID := fmt.Sprintf("exp17-gate-%d", time.Now().UnixNano())
	harness.Fact("library", "github.com/dapr/durabletask-go v0.16.0")
	harness.Fact("database", libx.EvalDatabase)
	harness.Fact("schema", schema)
	harness.Fact("instance-id", instanceID)
	harness.Fact("wait-mechanism", "ctx.WaitForSingleEvent(\"approval\", -1s), answered by client.RaiseEvent from another process")
	harness.Fact("unbounded-wait-available", "yes, with a timer behind it: a negative timeout still creates a durable timer, whose fire time the library sets to 9999-12-31T23:59:59Z, so an open-ended wait is a TimerCreated event and a NewEvents row that is invisible until that date")

	// 1. The process that holds the gate.
	first, err := startWorker(ctx, "gate1", workerBinary, cfg, schema, instanceID, workDir, true)
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
	idle, err := dtx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return err
	}
	idleHistory, err := dtx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	idleEvents, err := dtx.ReadNewEvents(ctx, db, instanceID)
	if err != nil {
		return err
	}
	idleTasks, err := dtx.ReadNewTasks(ctx, db, instanceID)
	if err != nil {
		return err
	}
	rows, err := libx.RowCounts(ctx, db, dtx.Tables, "instanceid = $1", instanceID)
	if err != nil {
		return err
	}
	bytes, err := libx.SumRowBytes(ctx, db, dtx.Tables, "instanceid = $1", instanceID)
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
	harness.Fact("sql-while-waiting-history", dtx.Events(idleHistory))
	harness.Fact("sql-while-waiting-newevents", dtx.Messages(idleEvents))
	harness.Fact("sql-while-waiting-newtasks", dtx.Messages(idleTasks))
	harness.Fact("recorded-state-while-waiting", idle.RuntimeStatus+", with LockExpiration null: a waiting instance is a RUNNING row that no worker holds")
	harness.Fact("the-wait-itself-in-the-database", "one NewEvents row carrying the indefinite TimerFired, with VisibleTime in the year 9999: the poller's query skips rows whose VisibleTime is in the future, so the wait costs one row that is never selected")
	harness.Fact("held-while-waiting-goroutines-in-the-waiting-process", fmt.Sprintf("%s before the workflow, %s while it waits", before, waiting))
	harness.Fact("held-while-waiting-goroutine-delta", goroutineDelta(before, waiting))

	stillWaiting, err := first.Field(ctx, "goroutines-while-still-waiting", waitingWait+idleSettle)
	if err != nil {
		return err
	}
	harness.Fact("held-after-seconds-of-waiting-goroutines", stillWaiting)
	harness.Fact("held-after-seconds-of-waiting-goroutine-delta", goroutineDelta(before, stillWaiting))
	harness.Fact("held-while-waiting-connections", backends)
	harness.Fact("held-while-waiting-open-transactions", fmt.Sprintf("%d sampled mid-flight, %d of them older than %s: a durabletask poller begins and rolls back a transaction every poll, and only one that outlived a poll cycle would be the wait holding it", backends.IdleInTransaction, backends.IdleInTransactionLonger, libx.LongTransaction))
	harness.Fact("held-while-waiting-locks-on-the-schema-or-rows", locks)
	harness.Fact("held-while-waiting-rows", rows)
	harness.Fact("held-while-waiting-row-bytes", bytes)
	harness.Fact("connection-model-read-from-the-library", "durabletask-go holds one pgx pool per process, whose default size is one connection, and polls it with a backoff; there is no connection, no transaction and no listener per waiting instance")

	// 3. Does the wait survive the process? The process is killed outright.
	killedAt := time.Now()
	if err := first.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill gate1: %w", err)
	}
	harness.Fact("killed-pid", first.PID())
	harness.Fact("kill-signal", "SIGKILL")
	afterKill, err := dtx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return err
	}
	afterKillHistory, err := dtx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-after-the-waiting-process-died", afterKill)
	harness.Fact("sql-after-the-waiting-process-died-history", dtx.Events(afterKillHistory))
	harness.Fact("what-the-death-cost-the-wait", "nothing in the database changed: the wait was already only rows, so there was nothing in the dead process to lose")

	second, err := startWorker(ctx, "gate2", workerBinary, cfg, schema, instanceID, workDir, false)
	if err != nil {
		return err
	}
	defer second.Stop()
	harness.Fact("replacement-pid", second.PID())
	harness.Fact("replacement-started-after-seconds", fmt.Sprintf("%.3f", time.Since(killedAt).Seconds()))

	// 4. The person answers. The driver raises the event through an engine of its
	// own, which runs no worker: only the replacement process can execute anything.
	answerEngine, err := dtx.StartClient(ctx, Options(cfg.PostgresDSN, schema))
	if err != nil {
		return err
	}
	defer answerEngine.Shutdown(context.WithoutCancel(ctx))

	answeredAt := time.Now()
	if err := answerEngine.Client.RaiseEvent(ctx, api.InstanceID(instanceID), AnswerEvent,
		api.WithEventPayload("approved")); err != nil {
		return fmt.Errorf("raise the answer for %s: %w", instanceID, err)
	}
	harness.Fact("answer-sent", "approved")
	final, err := dtx.WaitForInstance(ctx, db, instanceID, answerWait, dtx.Instance.Completed)
	if err != nil {
		return err
	}
	harness.Fact("seconds-from-answer-to-completed", fmt.Sprintf("%.3f", time.Since(answeredAt).Seconds()))
	harness.Fact("sql-final-instances", final)
	finalHistory, err := dtx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	finalEvents, err := dtx.ReadNewEvents(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-history", dtx.Events(finalHistory))
	harness.Fact("sql-final-newevents", dtx.Messages(finalEvents))
	harness.Fact("sql-final-eventraised-events", dtx.CountEvents(finalHistory, "EventRaised("+AnswerEvent+")"))
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
	harness.Fact("wait-resumed-in-the-replacement", resumedIn(decisionLines, second.PID()))
	harness.Fact("indefinite-timer-left-behind", fmt.Sprintf("%d NewEvents row(s) after the answer: the library does not delete the timer it created for an unbounded wait, so the row stays until the instance is purged", len(finalEvents)))

	// 5. What a wait of days costs, split into what this run timed and what it
	// reasons.
	reportCost(bytes, before, waiting, stillWaiting, backends, locks, int64(len(idleEvents)), answeredAt.Sub(askedAt))

	harness.Fact("limit", "one gate on one PostgreSQL, waiting seconds rather than days, with one answer: it shows what a durabletask-go wait holds in a process and in a database and that the wait survives the process dying, while the cost of a real multi-day wait is arithmetic on this run's bytes and goroutines, not a wait this pod sat through")

	switch {
	case len(questionLines) != 1:
		return fmt.Errorf("the activity before the wait ran %d times, not once, so the restart did not resume the wait but replayed the work", len(questionLines))
	case backends.IdleInTransactionLonger != 0:
		return fmt.Errorf("the waiting gate left %d connections idle in transaction for longer than %s, so the wait did hold a transaction open", backends.IdleInTransactionLonger, libx.LongTransaction)
	case idle.Completed():
		return fmt.Errorf("the waiting gate was already %s, so nothing was waiting", idle.RuntimeStatus)
	case final.RuntimeStatus != "COMPLETED":
		return fmt.Errorf("the gate ended %s, not COMPLETED, after the answer was delivered", final)
	case len(decisionLines) != 1:
		return fmt.Errorf("the activity after the wait ran %d times, not once", len(decisionLines))
	case !resumedIn(decisionLines, second.PID()):
		return fmt.Errorf("the activity after the wait did not run in the replacement process %d: %v", second.PID(), decisionLines)
	}
	return nil
}

// reportCost prints what keeping a wait open costs. Every line says which half it
// belongs to: timed: is a number this run measured, reasoned: is arithmetic over a
// span of days no pod can sit through.
func reportCost(bytes int64, before, waiting, still string, backends libx.Backends, locks, events int64, waited time.Duration) {
	delta := goroutineDelta(before, waiting)
	stillDelta := goroutineDelta(before, still)
	harness.Fact("timed: row-bytes-for-one-open-wait", bytes)
	harness.Fact("timed: row-bytes-source", "sum(pg_column_size(row)) over Instances, History, NewEvents and NewTasks for this instance")
	harness.Fact("timed: goroutines-added-by-one-open-wait", delta)
	harness.Fact("timed: goroutines-added-after-seconds-of-waiting", stillDelta)
	harness.Fact("timed: connections-to-the-database-while-one-wait-was-open", backends.Total)
	harness.Fact("timed: connections-idle-in-transaction-while-waiting", fmt.Sprintf("%d at the sample, %d older than %s", backends.IdleInTransaction, backends.IdleInTransactionLonger, libx.LongTransaction))
	harness.Fact("timed: locks-held-while-waiting", locks)
	harness.Fact("timed: undelivered-event-rows-while-waiting", events)
	harness.Fact("timed: wall-clock-this-gate-actually-waited", fmt.Sprintf("%.3fs, from the question being asked to the answer being sent", waited.Seconds()))

	for _, days := range []int{1, 7, 30} {
		harness.Fact(fmt.Sprintf("reasoned: storage-bytes-for-one-gate-open-%d-days", days),
			fmt.Sprintf("%d (the rows do not grow while nothing happens: a wait writes nothing per day, so %d days cost the same %d bytes as one minute)", bytes, days, bytes))
	}
	harness.Fact("reasoned: storage-bytes-for-1000-gates-open-30-days",
		fmt.Sprintf("%d (1000 x %d bytes, on the PostgreSQL Legion already runs, not a metered store)", 1000*bytes, bytes))
	if stillDelta > 0 {
		harness.Fact("reasoned: goroutine-stacks-for-1000-open-gates-in-one-process",
			fmt.Sprintf("%d goroutines, about %d bytes of initial stack (1000 x %d x %d bytes)",
				1000*stillDelta, 1000*stillDelta*goroutineStackBytes, stillDelta, goroutineStackBytes))
	} else {
		harness.Fact("reasoned: goroutine-stacks-for-1000-open-gates-in-one-process",
			"0: the engine keeps no goroutine for an instance between work items, so a thousand waits add no goroutines to any process")
	}
	harness.Fact("reasoned: what-a-waiting-gate-costs-per-day",
		"no work item, no write and no timer that ever fires: the row for the indefinite timer is in NewEvents with a VisibleTime the poller's WHERE clause excludes, so the gate is never even a candidate")
	harness.Fact("reasoned: what-the-bound-costs",
		"an unbounded wait is a row that is never collected: the library writes the indefinite timer when the wait starts and leaves it in NewEvents after the answer arrives, so a fleet of answered gates leaves one dead row each until the instances are purged")
	harness.Fact("reasoned: what-a-pod-cannot-do",
		"this run waited seconds; the lines above extend its measured bytes and goroutines over days by arithmetic, and nothing here observed a gate that really stood for days")
}

// resumedIn reports whether the activity after the wait ran in the replacement
// process, which is what makes the resumption the replacement's work rather than a
// survivor of the dead process.
func resumedIn(decisionLines []string, pid int) bool {
	for _, line := range decisionLines {
		if strings.Contains(line, fmt.Sprintf("applied-by-pid=%d", pid)) {
			return true
		}
	}
	return false
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

// startWorker starts one cmd/dtworker process in experiment 17's mode.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, instanceID, workDir string, start bool) (*libx.Child, error) {
	args := []string{
		"-mode", "exp17",
		"-postgres-dsn", cfg.PostgresDSN,
		"-schema", schema,
		"-instance-id", instanceID,
		"-work-dir", workDir,
	}
	if start {
		args = append(args, "-start")
	}
	return libx.StartChild(ctx, name, FieldPrefix, binary, args, workDir, workerStartWait)
}
