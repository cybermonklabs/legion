package exp16

import (
	"context"
	"database/sql"
	"fmt"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dtx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The driver's own timings. They are short on purpose: the whole run, kill, lock
// lapse and recovery included, has to finish in well under two minutes.
const (
	workerStartWait = 40 * time.Second
	effectWait      = 25 * time.Second
	// lockedWindowObserve is how long the replacement process is watched while the
	// dead process's lock has not yet lapsed. It must be shorter than
	// WorkflowLockTimeout, or the lapse would happen inside the window and the
	// window would prove nothing.
	lockedWindowObserve = 4 * time.Second
	recoveryWait        = 40 * time.Second
	completionWait      = 30 * time.Second
)

// Run is experiment 16. It starts a worker process, lets its activity perform a side
// effect and block inside the activity, kills that process with SIGKILL, and then
// measures what durabletask-go did: what its tables say at each stage, whether
// anything re-ran the activity, and above all how many times the side effect
// happened.
//
// suppressKill is the run's control: with it the driver sends no signal, so nothing
// is interrupted and nothing is recovered, and the experiment must report that it
// measured nothing.
func Run(ctx context.Context, cfg harness.Config, suppressKill bool) error {
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
	sideEffects := SideEffectFile(workDir)
	release := ReleaseFile(workDir)
	if err := libx.RemoveAll(sideEffects, release); err != nil {
		return err
	}

	schema := libx.SchemaName("exp16")
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

	instanceID := fmt.Sprintf("exp16-prompt-%d", time.Now().UnixNano())
	harness.Fact("library", "github.com/dapr/durabletask-go v0.16.0, an embedded Go engine on the application's own PostgreSQL")
	harness.Fact("database", libx.EvalDatabase)
	harness.Fact("schema", schema)
	harness.Fact("instance-id", instanceID)
	harness.Fact("workflow-name", WorkflowName)
	harness.Fact("activity-name", ActivityName)
	harness.Fact("side-effect-file", sideEffects)
	harness.Fact("lock-documented-default", "2 minutes for both work-item kinds, in Instances.LockExpiration for a workflow and NewTasks.LockExpiration for an activity")
	harness.Fact("lock-this-run-used", fmt.Sprintf("%s, set through PostgresOptions.WorkflowLockTimeout: this run waits the lapse out rather than waiting two minutes for it", WorkflowLockTimeout))
	harness.Fact("activity-lock-setting-read-from-the-library", "PostgresOptions.ActivityLockTimeout exists and v0.16.0's PostgreSQL backend never reads it: getActivityWorkItem computes NewTasks.LockExpiration from WorkflowLockTimeout, so an activity's lock is the workflow lock")
	harness.Fact("heartbeat-while-an-activity-runs", "none: the PostgreSQL backend has no extend for a locked work item, so the lock is a fixed deadline from the moment it was taken, running whether the activity is working or dead")

	// 1. The process that will be killed.
	first, err := startWorker(ctx, "worker1", workerBinary, cfg, schema, instanceID, workDir, true)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("worker1-pid", first.PID())

	tables, err := libx.ListTables(ctx, db, schema)
	if err != nil {
		return err
	}
	harness.Fact("tables-the-library-created", tables)
	harness.Fact("migration-table", "none: the library has no migration table, it runs one script of CREATE TABLE IF NOT EXISTS every time a worker starts")

	// 2. Wait until the activity has performed its side effect and is blocked in it.
	// Only then can a kill land inside the activity rather than before or after it.
	lines, err := libx.WaitForLines(ctx, sideEffects, 1, effectWait)
	if err != nil {
		return fmt.Errorf("the activity never performed its side effect: %w", err)
	}
	harness.Fact("side-effect-1", lines[0])
	harness.Fact("release-file-exists-while-activity-blocked", libx.Exists(release))
	if err := report(ctx, db, instanceID, "before-kill"); err != nil {
		return err
	}
	beforeKill, err := dtx.ReadNewTasks(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("newtasks-dequeuecount-before-kill", dequeueCount(beforeKill))
	harness.Fact("newtasks-lockedby-before-kill", lockedBy(beforeKill))

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

	// 4. What the model holds about an activity that ran and whose outcome nobody
	// recorded.
	if err := report(ctx, db, instanceID, "after-kill"); err != nil {
		return err
	}
	harness.Fact("state-for-an-activity-that-ran-with-its-outcome-unknown",
		"none that says it ran: the NewTasks row is written when the activity is scheduled and deleted when it completes, so a killed activity leaves the same row as one that was never picked up. LockedBy and LockExpiration say a worker claimed it and DequeueCount says how often it was handed out, but nothing says the body did anything")
	harness.Fact("nearest-thing-to-an-attempt-counter-on-the-row",
		"NewTasks.DequeueCount for an activity and NewEvents.DequeueCount for a workflow work item: both count hand-outs, not performances, and neither is visible to the workflow code")

	// 5. The replacement process, and what the lock does while it waits.
	effects := 1
	if killSent {
		second, err := startWorker(ctx, "worker2", workerBinary, cfg, schema, instanceID, workDir, false)
		if err != nil {
			return err
		}
		defer second.Stop()
		harness.Fact("replacement-pid", second.PID())

		// While the dead process's lock has not lapsed, the replacement is polling
		// and must not be given the activity. That is what pins the work: the lock,
		// not the process.
		if err := libx.Sleep(ctx, lockedWindowObserve); err != nil {
			return err
		}
		during, err := libx.ReadLines(sideEffects)
		if err != nil {
			return err
		}
		harness.Fact("side-effects-while-the-dead-process-lock-was-still-held", len(during))
		harness.Fact("replacement-waited-for-the-lapse-seconds", lockedWindowObserve.String())
		if err := report(ctx, db, instanceID, "while-lock-held-by-dead-process"); err != nil {
			return err
		}

		lines, err = libx.WaitForLines(ctx, sideEffects, 2, recoveryWait)
		if err != nil {
			harness.Fact("activity-re-run", false)
			harness.Fact("recovery-wait-error", err)
		} else {
			harness.Fact("activity-re-run", true)
			harness.Fact("seconds-from-kill-to-the-second-performance", fmt.Sprintf("%.3f", time.Since(killedAt).Seconds()))
			harness.Fact("what-the-lock-expiry-does-when-the-holder-never-returns",
				fmt.Sprintf("it is what releases the work: the NewTasks row keeps LockedBy and LockExpiration exactly as the dead process left them, and the first poller to find LockExpiration in the past takes the row, raises its DequeueCount and runs the activity body again -- about %s after the kill here", WorkflowLockTimeout))
			harness.Fact("side-effect-2", lines[1])
			if err := report(ctx, db, instanceID, "during-the-second-performance"); err != nil {
				return err
			}
			reRun, err := dtx.ReadNewTasks(ctx, db, instanceID)
			if err != nil {
				return err
			}
			harness.Fact("newtasks-dequeuecount-during-the-second-performance", dequeueCount(reRun))
			harness.Fact("newtasks-lockedby-during-the-second-performance", lockedBy(reRun))
			harness.Fact("newtasks-row-is-the-same-row-handed-to-another-worker", sameRow(beforeKill, reRun))
		}
	}

	// 6. Release the activity, so the attempt that is running now can finish and
	// record what it did.
	if err := libx.Touch(release); err != nil {
		return err
	}
	final, err := dtx.WaitForInstance(ctx, db, instanceID, completionWait, dtx.Instance.Completed)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-instances", final)
	finalHistory, err := dtx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	finalTasks, err := dtx.ReadNewTasks(ctx, db, instanceID)
	if err != nil {
		return err
	}
	finalEvents, err := dtx.ReadNewEvents(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-history", dtx.Events(finalHistory))
	harness.Fact("sql-final-newtasks", dtx.Messages(finalTasks))
	harness.Fact("sql-final-newevents", dtx.Messages(finalEvents))
	harness.Fact("sql-final-taskscheduled-events", dtx.CountEvents(finalHistory, "TaskScheduled("+ActivityName+")"))
	harness.Fact("sql-final-taskcompleted-events", dtx.CountEvents(finalHistory, "TaskCompleted"))
	harness.Fact("sql-final-taskfailed-events", dtx.CountEvents(finalHistory, "TaskFailed"))

	// 7. The number the experiment exists to produce.
	lines, err = libx.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	effects = len(lines)
	for i, line := range lines {
		harness.Fact(fmt.Sprintf("side-effect-line-%d", i+1), line)
	}
	harness.Fact("side-effect-performed-times", effects)
	harness.Fact("activity-completions-recorded-in-postgres", dtx.CountEvents(finalHistory, "TaskCompleted"))
	if effects > 1 {
		harness.Fact("what-that-means",
			"an activity whose outcome durabletask-go never recorded is run again once its lock lapses: the library's guarantee for an activity is at-least-once, and the history records only the attempt that returned, so a side effect performed by a killed attempt is invisible to the workflow")
	} else {
		harness.Fact("what-that-means",
			"nothing was interrupted, so this run saw only that an uninterrupted activity runs once; what an interrupted one does is what the kill measures")
	}

	// 8. What binding the work to one process would mean.
	harness.Fact("can-the-activity-be-pinned-to-one-process",
		"no: the PostgreSQL backend's getActivityWorkItem takes the first NewTasks row whose lock has lapsed, with no queue, no worker filter and no routing of any kind -- LockedBy is written when a worker takes the row rather than when the activity is scheduled")
	harness.Fact("what-holds-the-work-to-the-dead-process", "NewTasks.LockExpiration alone, for as long as it is in the future")
	harness.Fact("if-that-process-never-comes-back",
		fmt.Sprintf("the lock lapses %s after it was taken and the next poller runs the activity body again, with DequeueCount one higher: no process identity, no maximum and no operator action is involved", WorkflowLockTimeout))

	harness.Fact("limit", "one instance, one activity, one kill, on one PostgreSQL, with the lock shortened from two minutes to "+WorkflowLockTimeout.String()+": it shows what durabletask-go records and re-runs when the process inside an activity dies, not how a fleet of workers behaves, nor what happens when the database rather than the process is the thing that fails")

	// 9. The verdict. A run that killed nothing measured nothing, whatever else it
	// saw.
	switch {
	case !killSent:
		return fmt.Errorf("no process was killed, so this run measured nothing about what durabletask-go does to an activity that was interrupted: the side effect happened %d time(s) and nothing re-ran it", effects)
	case effects < 2:
		return fmt.Errorf("the side effect happened %d time(s): the kill landed but nothing re-ran the activity, so the recovery outcome was not measured", effects)
	case final.RuntimeStatus != "COMPLETED":
		return fmt.Errorf("the instance ended %s, not COMPLETED, so what durabletask-go recorded for the activity was not read back", final)
	case dtx.CountEvents(finalHistory, "TaskCompleted") == 0:
		return fmt.Errorf("PostgreSQL holds no TaskCompleted event for the instance, so the recorded outcome was not read back")
	}
	return nil
}

// report prints what the library's own tables say about the instance at one stage of
// the run, every line read with SQL.
func report(ctx context.Context, db *sql.DB, instanceID, stage string) error {
	instance, err := dtx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return err
	}
	tasks, err := dtx.ReadNewTasks(ctx, db, instanceID)
	if err != nil {
		return err
	}
	events, err := dtx.ReadNewEvents(ctx, db, instanceID)
	if err != nil {
		return err
	}
	history, err := dtx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-"+stage+"-instances", instance)
	harness.Fact("sql-"+stage+"-newtasks", dtx.Messages(tasks))
	harness.Fact("sql-"+stage+"-newevents", dtx.Messages(events))
	harness.Fact("sql-"+stage+"-history", dtx.Events(history))
	return nil
}

// dequeueCount is the DequeueCount of the instance's single outstanding activity row.
func dequeueCount(tasks []dtx.Message) string {
	if len(tasks) == 0 {
		return "no NewTasks row"
	}
	return fmt.Sprint(tasks[0].DequeueCount)
}

// lockedBy is the LockedBy of that row, which the library writes as hostname,pid,uuid.
func lockedBy(tasks []dtx.Message) string {
	if len(tasks) == 0 {
		return "no NewTasks row"
	}
	if tasks[0].LockedBy == nil {
		return "null"
	}
	return *tasks[0].LockedBy
}

// sameRow reports whether the row the second worker holds is the row the first one
// held, which is how the experiment knows the work was handed over rather than
// scheduled afresh.
func sameRow(before, after []dtx.Message) bool {
	return len(before) == 1 && len(after) == 1 && before[0].SequenceNumber == after[0].SequenceNumber
}

// startWorker starts one cmd/dtworker process in experiment 16's mode.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, instanceID, workDir string, start bool) (*libx.Child, error) {
	args := []string{
		"-mode", "exp16",
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
