package exp13

import (
	"context"
	"database/sql"
	"fmt"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/gwx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The driver's own timings. They are short on purpose: the whole run, kill, lock lapse
// and recovery included, has to finish in well under two minutes.
const (
	workerStartWait = 40 * time.Second
	effectWait      = 25 * time.Second
	// lockedWindowObserve is how long the replacement process is watched while the
	// dead process's activity lock has not yet lapsed. It must be shorter than
	// ActivityLockTimeout, or the lapse would happen inside the window and the
	// window would prove nothing.
	lockedWindowObserve = 4 * time.Second
	recoveryWait        = 40 * time.Second
	completionWait      = 30 * time.Second
	// The two worker names. They differ so that a row's worker column says which
	// process holds it.
	worker1Name = "exp13-session-host-1"
	worker2Name = "exp13-session-host-2"
)

// Run is experiment 13. It starts a worker process, lets its activity perform a side
// effect and block inside the activity, kills that process with SIGKILL, and then
// measures what go-workflows did: what its tables say at each stage, whether anything
// re-ran the activity, and above all how many times the side effect happened.
//
// suppressKill is the run's control: with it the driver sends no signal, so nothing is
// interrupted and nothing is recovered, and the experiment must report that it
// measured nothing.
func Run(ctx context.Context, cfg harness.Config, suppressKill bool) error {
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
	sideEffects := SideEffectFile(workDir)
	release := ReleaseFile(workDir)
	if err := libx.RemoveAll(sideEffects, release); err != nil {
		return err
	}

	schema := libx.SchemaName("exp13")
	db, err := openSchema(ctx, cfg.PostgresDSN, schema)
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

	instanceID := fmt.Sprintf("exp13-prompt-%d", time.Now().UnixNano())
	tables, err := libx.ListTables(ctx, db, schema)
	if err != nil {
		return err
	}
	harness.Fact("library", "github.com/cschleiden/go-workflows v1.4.2, an embedded Go library on the application's own PostgreSQL")
	harness.Fact("database", libx.EvalDatabase)
	harness.Fact("schema", schema)
	harness.Fact("tables-the-library-created", tables)
	harness.Fact("instance-id", instanceID)
	harness.Fact("workflow-name", WorkflowName)
	harness.Fact("activity-name", ActivityName)
	harness.Fact("side-effect-file", sideEffects)
	harness.Fact("activity-lock-documented-default", "2 minutes, held in activities.locked_until and extended by the holder's heartbeat")
	harness.Fact("activity-lock-this-run-used", fmt.Sprintf("%s, shortened with backend.WithActivityLockTimeout: this run waits the lapse out rather than waiting two minutes for it", ActivityLockTimeout))
	harness.Fact("activity-heartbeat-interval-this-run-used", ActivityHeartbeatInterval.String())
	harness.Fact("workflow-task-lock-this-run-used", WorkflowLockTimeout.String())
	harness.Fact("sticky-timeout-this-run-used", StickyTimeout.String())

	// 1. The process that will be killed.
	first, err := startWorker(ctx, "worker1", workerBinary, cfg, schema, worker1Name, instanceID, workDir, true)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("worker1-pid", first.PID())
	harness.Fact("worker1-name", worker1Name)

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
	beforeKill, err := gwx.ReadActivities(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("activity-row-worker-before-kill", workerOf(beforeKill))

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
		"none that says it ran: the activities row is written when the activity is scheduled and deleted when it completes, so a killed activity leaves the same row as one that was never picked up, differing only in locked_until and worker -- which say a worker claimed it, not that its body did anything")
	harness.Fact("in-flight-marker-in-the-schema",
		"activities.locked_until and activities.worker, and nothing else: there is no attempt count, no started_at and no outcome-unknown state on the row, and history gets an ActivityCompleted or ActivityFailed event only once an attempt returns")

	// 5. The replacement process, and what the lock does while it waits.
	effects := 1
	if killSent {
		second, err := startWorker(ctx, "worker2", workerBinary, cfg, schema, worker2Name, instanceID, workDir, false)
		if err != nil {
			return err
		}
		defer second.Stop()
		harness.Fact("replacement-pid", second.PID())
		harness.Fact("replacement-worker-name", worker2Name)

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
				fmt.Sprintf("it is what releases the work: the row keeps locked_until and worker exactly as the dead process left them, and the first poller to find locked_until in the past takes the activity and runs its body again -- about %s after the kill here, which is the %s lock minus the time since the dead process's last heartbeat", ActivityLockTimeout, ActivityLockTimeout))
			harness.Fact("side-effect-2", lines[1])
			if err := report(ctx, db, instanceID, "during-the-second-performance"); err != nil {
				return err
			}
			reRun, err := gwx.ReadActivities(ctx, db, instanceID)
			if err != nil {
				return err
			}
			harness.Fact("activity-row-worker-after-the-lapse", workerOf(reRun))
			harness.Fact("activity-row-is-the-same-row-handed-to-another-worker", sameActivity(beforeKill, reRun))
		}
	}

	// 6. Release the activity, so the attempt that is running now can finish and
	// record what it did.
	if err := libx.Touch(release); err != nil {
		return err
	}
	final, err := gwx.WaitForInstance(ctx, db, instanceID, completionWait, gwx.Instance.Finished)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-instances", final)
	finalHistory, err := gwx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	finalActivities, err := gwx.ReadActivities(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-final-history", gwx.Events(finalHistory))
	harness.Fact("sql-final-activities-rows", len(finalActivities))
	harness.Fact("sql-final-activity-scheduled-events", gwx.CountEvents(finalHistory, "ActivityScheduled"))
	harness.Fact("sql-final-activity-completed-events", gwx.CountEvents(finalHistory, "ActivityCompleted"))
	harness.Fact("sql-final-activity-failed-events", gwx.CountEvents(finalHistory, "ActivityFailed"))

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
	harness.Fact("activity-completions-recorded-in-postgres", gwx.CountEvents(finalHistory, "ActivityCompleted"))
	if effects > 1 {
		harness.Fact("what-that-means",
			"an activity whose outcome go-workflows never recorded is run again once its lock lapses: the library's guarantee for an activity is at-least-once, and the history records only the attempt that returned, so a side effect performed by a killed attempt is invisible to the workflow")
	} else {
		harness.Fact("what-that-means",
			"nothing was interrupted, so this run saw only that an uninterrupted activity runs once; what an interrupted one does is what the kill measures")
	}

	// 8. What binding the work to one process would mean.
	harness.Fact("can-the-activity-be-pinned-to-one-process",
		"no: the only routing go-workflows has is a queue, which is a name several workers may poll, and the activities row's worker column is written when a worker takes the lock rather than when the activity is scheduled -- nothing addresses a particular process")
	harness.Fact("what-holds-the-work-to-the-dead-process", "activities.locked_until alone, for as long as it is in the future")
	harness.Fact("if-that-process-never-comes-back",
		fmt.Sprintf("the lock lapses %s after its last heartbeat and the next poller runs the activity body again: no process identity, no attempt count and no operator action is involved", ActivityLockTimeout))

	harness.Fact("limit", "one instance, one activity, one kill, on one PostgreSQL, with the activity lock shortened from two minutes to "+ActivityLockTimeout.String()+": it shows what go-workflows records and re-runs when the process inside an activity dies, not how a fleet of workers behaves, nor what happens when the database rather than the process is the thing that fails")

	// 9. The verdict. A run that killed nothing measured nothing, whatever else it saw.
	switch {
	case !killSent:
		return fmt.Errorf("no process was killed, so this run measured nothing about what go-workflows does to an activity that was interrupted: the side effect happened %d time(s) and nothing re-ran it", effects)
	case effects < 2:
		return fmt.Errorf("the side effect happened %d time(s): the kill landed but nothing re-ran the activity, so the recovery outcome was not measured", effects)
	case !final.Finished():
		return fmt.Errorf("the instance ended %s, not Finished, so what go-workflows recorded for the activity was not read back", final)
	case gwx.CountEvents(finalHistory, "ActivityCompleted") == 0:
		return fmt.Errorf("PostgreSQL holds no ActivityCompleted event for the instance, so the recorded outcome was not read back")
	}
	return nil
}

// openSchema makes the run's own schema in the evaluation database and lets the
// library migrate its tables into it.
func openSchema(ctx context.Context, dsn, schema string) (*sql.DB, error) {
	if err := libx.EnsureDatabase(ctx, dsn); err != nil {
		return nil, err
	}
	db, err := libx.Open(ctx, dsn, schema)
	if err != nil {
		return nil, err
	}
	if err := libx.CreateSchema(ctx, db, schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := gwx.Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// report prints what the library's own tables say about the instance at one stage of
// the run, every line read with SQL.
func report(ctx context.Context, db *sql.DB, instanceID, stage string) error {
	instance, err := gwx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return err
	}
	activities, err := gwx.ReadActivities(ctx, db, instanceID)
	if err != nil {
		return err
	}
	events, err := gwx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return err
	}
	pending, err := gwx.ReadPendingEvents(ctx, db, instanceID)
	if err != nil {
		return err
	}
	harness.Fact("sql-"+stage+"-instances", instance)
	if len(activities) == 0 {
		harness.Fact("sql-"+stage+"-activities", "no row")
	}
	for _, a := range activities {
		harness.Fact("sql-"+stage+"-activities", a)
	}
	harness.Fact("sql-"+stage+"-history", gwx.Events(events))
	harness.Fact("sql-"+stage+"-pending_events", gwx.Events(pending))
	return nil
}

// workerOf is the worker column of the instance's single activity row, for a fact.
func workerOf(activities []gwx.Activity) string {
	if len(activities) == 0 {
		return "no activities row"
	}
	if activities[0].Worker == nil {
		return "null"
	}
	return *activities[0].Worker
}

// sameActivity reports whether the row the second worker holds is the row the first
// one held, which is how the experiment knows the work was handed over rather than
// scheduled afresh.
func sameActivity(before, after []gwx.Activity) bool {
	return len(before) == 1 && len(after) == 1 && before[0].ActivityID == after[0].ActivityID
}

// startWorker starts one cmd/gwworker process in experiment 13's mode.
func startWorker(ctx context.Context, name, binary string, cfg harness.Config, schema, workerName, instanceID, workDir string, start bool) (*libx.Child, error) {
	args := []string{
		"-mode", "exp13",
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
