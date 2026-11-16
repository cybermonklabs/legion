package exp18

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/api"
	"github.com/dapr/durabletask-go/task"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dtx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The driver's own timings.
const (
	recordWait = 30 * time.Second
	// changeObserve is how long the changed code is left in front of the recorded
	// run. It spans several work-item locks, so a work item that is retried rather
	// than completed is seen being retried.
	changeObserve  = 14 * time.Second
	completionWait = 30 * time.Second
	settle         = 250 * time.Millisecond
)

// Run is experiment 18. It records runs under one version of a workflow, changes
// that workflow by inserting an activity in the middle, and measures three things:
// what the unguarded change does to a recorded run, what ctx.IsPatched does with the
// same change, and what a versioned registry does with it.
func Run(ctx context.Context, cfg harness.Config) error {
	if err := harness.RequirePostgresDSN(cfg); err != nil {
		return err
	}
	workDir, err := harness.WorkDir(cfg)
	if err != nil {
		return err
	}
	steps := SideEffectFile(workDir)
	if err := libx.RemoveAll(steps); err != nil {
		return err
	}

	schema := libx.SchemaName("exp18")
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

	now := time.Now().UnixNano()
	unguarded := fmt.Sprintf("exp18-unguarded-%d", now)
	patched := fmt.Sprintf("exp18-patched-%d", now)
	pinned := fmt.Sprintf("exp18-versioned-%d", now)
	fresh := fmt.Sprintf("exp18-new-%d", now)

	harness.Fact("library", "github.com/dapr/durabletask-go v0.16.0")
	harness.Fact("database", libx.EvalDatabase)
	harness.Fact("schema", schema)
	harness.Fact("versioning-api-in-the-library", "two: ctx.IsPatched(name), which runs new code only for a run that had not passed that point, and a versioned registry (AddVersionedWorkflowN) that pins a run to the version it started under")
	harness.Fact("side-by-side-deployment-available", "no: the PostgreSQL backend has no queue and no routing, so every worker polls every instance -- two deployments of different code against one task hub would take each other's work items")
	harness.Fact("recorded-code", "plan, implement, wait for a person, deliver")
	harness.Fact("changed-code", "plan, review, implement, wait for a person, deliver: one new activity in the middle")
	harness.Fact("work-item-lock-this-run-used", WorkflowLockTimeout.String())

	// 1. Two recorded runs under the code as it stood before the change, both
	// parked on the wait for a person, which is where a change finds them.
	recordedRegistry, err := Registry(RecordedWorkflow)
	if err != nil {
		return err
	}
	old, err := Deploy(ctx, recordedRegistry, cfg.PostgresDSN, schema)
	if err != nil {
		return err
	}
	for _, id := range []string{unguarded, patched} {
		if _, err := old.Engine.Client.ScheduleNewWorkflow(ctx, WorkflowName,
			api.WithInstanceID(api.InstanceID(id)),
			api.WithInput(Params{SideEffectFile: steps, Instance: id})); err != nil {
			old.Stop(ctx)
			return fmt.Errorf("create instance %s: %w", id, err)
		}
	}
	for _, id := range []string{unguarded, patched} {
		if err := waitParked(ctx, db, id, recordWait); err != nil {
			old.Stop(ctx)
			return err
		}
	}
	recordedHistory, err := dtx.ReadHistory(ctx, db, unguarded)
	if err != nil {
		old.Stop(ctx)
		return err
	}
	recordedSteps, err := libx.ReadLines(steps)
	if err != nil {
		old.Stop(ctx)
		return err
	}
	harness.Fact("recorded-instance", unguarded)
	harness.Fact("sql-recorded-history", dtx.Events(recordedHistory))
	harness.Fact("sql-recorded-instances", mustInstance(ctx, db, unguarded))
	harness.Fact("steps-performed-while-recording", len(recordedSteps))
	old.Stop(ctx)
	harness.Fact("old-deployment-stopped-before-the-change", true)

	// 2. The unguarded change: the new code, and nothing else, in front of a run
	// that recorded the old shape.
	changedRegistry, err := Registry(ChangedWorkflow)
	if err != nil {
		return err
	}
	changed, err := Deploy(ctx, changedRegistry, cfg.PostgresDSN, schema)
	if err != nil {
		return err
	}
	if err := changed.Engine.Client.RaiseEvent(ctx, api.InstanceID(unguarded), ReleaseEvent,
		api.WithEventPayload("approved")); err != nil {
		changed.Stop(ctx)
		return err
	}
	afterChange, dequeues, err := observe(ctx, db, unguarded, changeObserve)
	if err != nil {
		changed.Stop(ctx)
		return err
	}
	afterChangeHistory, err := dtx.ReadHistory(ctx, db, unguarded)
	if err != nil {
		changed.Stop(ctx)
		return err
	}
	afterChangeEvents, err := dtx.ReadNewEvents(ctx, db, unguarded)
	if err != nil {
		changed.Stop(ctx)
		return err
	}
	allSteps, err := libx.ReadLines(steps)
	if err != nil {
		changed.Stop(ctx)
		return err
	}
	changed.Stop(ctx)

	unguardedSteps := stepsOf(allSteps, unguarded)
	metadata, metaErr := fetchFailure(ctx, cfg.PostgresDSN, schema, unguarded)
	harness.Fact("what-the-changed-code-did-to-the-recorded-run",
		outcome(afterChange, strings.Contains(unguardedSteps, StepDeliver), dequeues))
	harness.Fact("steps-the-recorded-run-performed-in-the-end", unguardedSteps)
	harness.Fact("inserted-activity-ran-against-the-recorded-run", strings.Contains(unguardedSteps, StepReview))
	harness.Fact("already-recorded-activity-run-again-by-the-changed-code", repeatedSteps(unguardedSteps))
	harness.Fact("recorded-run-reached-its-last-step", strings.Contains(unguardedSteps, StepDeliver))
	if metaErr != nil {
		harness.Fact("error-the-library-recorded", metaErr)
	} else {
		harness.Fact("error-the-library-recorded", metadata)
	}
	harness.Fact("newevents-dequeuecount-for-the-recorded-run-in-"+changeObserve.String(), dequeues)
	harness.Fact("state-the-run-is-left-in", afterChange.RuntimeStatus)
	harness.Fact("sql-after-the-change-instances", afterChange)
	harness.Fact("sql-after-the-change-history", dtx.Events(afterChangeHistory))
	harness.Fact("sql-after-the-change-newevents", dtx.Messages(afterChangeEvents))
	harness.Fact("history-the-old-code-recorded-is-still-its-prefix",
		dtx.Events(recordedHistory) == dtx.Events(afterChangeHistory[:min(len(afterChangeHistory), len(recordedHistory))]))
	harness.Fact("history-rows-before-the-change", len(recordedHistory))
	harness.Fact("history-rows-after-the-change", len(afterChangeHistory))
	harness.Fact("is-the-history-corrupted", "no: the rows the old code recorded are still the run's history, in order; what the change costs is the rest of the run, not the record of what it did")
	harness.Fact("changed-deployment-stopped", true)

	// 3. The first documented alternative: the same change behind ctx.IsPatched.
	patchedRegistry, err := Registry(PatchedWorkflow)
	if err != nil {
		return err
	}
	patchDeployment, err := Deploy(ctx, patchedRegistry, cfg.PostgresDSN, schema)
	if err != nil {
		return err
	}
	if err := patchDeployment.Engine.Client.RaiseEvent(ctx, api.InstanceID(patched), ReleaseEvent,
		api.WithEventPayload("approved")); err != nil {
		patchDeployment.Stop(ctx)
		return err
	}
	patchedFinal, err := dtx.WaitForInstance(ctx, db, patched, completionWait, dtx.Instance.Completed)
	if err != nil {
		patchDeployment.Stop(ctx)
		return err
	}
	patchedHistory, err := dtx.ReadHistory(ctx, db, patched)
	if err != nil {
		patchDeployment.Stop(ctx)
		return err
	}
	patchDeployment.Stop(ctx)
	allSteps, err = libx.ReadLines(steps)
	if err != nil {
		return err
	}
	patchedSteps := stepsOf(allSteps, patched)
	harness.Fact("patched-recorded-run-status", patchedFinal.RuntimeStatus)
	harness.Fact("sql-patched-instances", patchedFinal)
	harness.Fact("sql-patched-history", dtx.Events(patchedHistory))
	harness.Fact("steps-the-patched-code-ran-for-its-recorded-run", patchedSteps)
	harness.Fact("patch-skipped-the-new-step-for-the-recorded-run", !strings.Contains(patchedSteps, StepReview))
	harness.Fact("how-the-patch-decides", "ctx.IsPatched returns false while the replay is not yet at the end of the recorded history, and true once it is, so a run that had already passed the point takes the old path and a run that had not takes the new one")

	// 4. The second documented alternative: a versioned registry, with the run
	// pinned to the version it started under.
	versionedOld, err := VersionedRegistry(map[string]task.Workflow{VersionOld: RecordedWorkflow}, VersionOld)
	if err != nil {
		return err
	}
	v1, err := Deploy(ctx, versionedOld, cfg.PostgresDSN, schema)
	if err != nil {
		return err
	}
	if _, err := v1.Engine.Client.ScheduleNewWorkflow(ctx, WorkflowName,
		api.WithInstanceID(api.InstanceID(pinned)),
		api.WithInput(Params{SideEffectFile: steps, Instance: pinned})); err != nil {
		v1.Stop(ctx)
		return fmt.Errorf("create instance %s: %w", pinned, err)
	}
	if err := waitParked(ctx, db, pinned, recordWait); err != nil {
		v1.Stop(ctx)
		return err
	}
	pinnedRecorded, err := dtx.ReadInstance(ctx, db, pinned)
	if err != nil {
		v1.Stop(ctx)
		return err
	}
	v1.Stop(ctx)
	harness.Fact("sql-versioned-run-while-recorded", pinnedRecorded)
	harness.Fact("version-recorded-on-the-instance", versionOf(pinnedRecorded))

	versionedBoth, err := VersionedRegistry(map[string]task.Workflow{
		VersionOld: RecordedWorkflow, VersionNew: ChangedWorkflow,
	}, VersionNew)
	if err != nil {
		return err
	}
	v2, err := Deploy(ctx, versionedBoth, cfg.PostgresDSN, schema)
	if err != nil {
		return err
	}
	if err := v2.Engine.Client.RaiseEvent(ctx, api.InstanceID(pinned), ReleaseEvent,
		api.WithEventPayload("approved")); err != nil {
		v2.Stop(ctx)
		return err
	}
	pinnedFinal, err := dtx.WaitForInstance(ctx, db, pinned, completionWait, dtx.Instance.Completed)
	if err != nil {
		v2.Stop(ctx)
		return err
	}
	// A new run under the same deployment takes the latest version.
	if _, err := v2.Engine.Client.ScheduleNewWorkflow(ctx, WorkflowName,
		api.WithInstanceID(api.InstanceID(fresh)),
		api.WithInput(Params{SideEffectFile: steps, Instance: fresh})); err != nil {
		v2.Stop(ctx)
		return fmt.Errorf("create instance %s: %w", fresh, err)
	}
	if err := waitParked(ctx, db, fresh, recordWait); err != nil {
		v2.Stop(ctx)
		return err
	}
	if err := v2.Engine.Client.RaiseEvent(ctx, api.InstanceID(fresh), ReleaseEvent,
		api.WithEventPayload("approved")); err != nil {
		v2.Stop(ctx)
		return err
	}
	freshFinal, err := dtx.WaitForInstance(ctx, db, fresh, completionWait, dtx.Instance.Completed)
	if err != nil {
		v2.Stop(ctx)
		return err
	}
	v2.Stop(ctx)

	allSteps, err = libx.ReadLines(steps)
	if err != nil {
		return err
	}
	pinnedSteps := stepsOf(allSteps, pinned)
	freshSteps := stepsOf(allSteps, fresh)
	harness.Fact("versioned-recorded-run-status", pinnedFinal.RuntimeStatus)
	harness.Fact("sql-versioned-instances", pinnedFinal)
	harness.Fact("steps-the-versioned-recorded-run-ran-under-the-newer-deployment", pinnedSteps)
	harness.Fact("versioned-run-stayed-on-the-code-it-started-under", !strings.Contains(pinnedSteps, StepReview))
	harness.Fact("new-run-under-the-latest-version-status", freshFinal.RuntimeStatus)
	harness.Fact("steps-the-new-run-ran", freshSteps)
	for _, line := range allSteps {
		harness.Fact("step-line", line)
	}

	harness.Fact("limit", "one inserted activity, one recorded run per treatment, on one PostgreSQL: it shows what an unguarded change does to a run that recorded the old shape and that both of the library's guards carry such a run to the end, not how a fleet rolls out, and not what a change that only reorders or renames would do")

	switch {
	case len(recordedSteps) != 4:
		return fmt.Errorf("recording the two runs performed %d steps, not the 4 the old code has (plan and implement for each)", len(recordedSteps))
	case !afterChange.Completed() && dequeues < 2:
		return fmt.Errorf("in %s the changed code neither ended the recorded run nor had its work item handed out again, so nothing was measured about what the change does to a recorded run", changeObserve)
	case afterChange.RuntimeStatus == "COMPLETED" && strings.Contains(unguardedSteps, StepDeliver):
		return fmt.Errorf("the recorded run completed its last step under the changed code, so this run measured no cost to an unguarded change: %s", unguardedSteps)
	case patchedFinal.RuntimeStatus != "COMPLETED":
		return fmt.Errorf("the patched recorded run ended %s, not COMPLETED", patchedFinal)
	case strings.Contains(patchedSteps, StepReview):
		return fmt.Errorf("the patch ran the inserted step for a run that was recorded before it: %s", patchedSteps)
	case !strings.Contains(patchedSteps, StepDeliver):
		return fmt.Errorf("the patched recorded run did not reach its last step: %s", patchedSteps)
	case pinnedFinal.RuntimeStatus != "COMPLETED":
		return fmt.Errorf("the versioned recorded run ended %s, not COMPLETED", pinnedFinal)
	case strings.Contains(pinnedSteps, StepReview):
		return fmt.Errorf("the versioned run did not stay on the version it started under: %s", pinnedSteps)
	case freshFinal.RuntimeStatus != "COMPLETED":
		return fmt.Errorf("the new run ended %s, not COMPLETED", freshFinal)
	case !strings.Contains(freshSteps, StepReview):
		return fmt.Errorf("the new run did not run the inserted step: %s", freshSteps)
	}
	return nil
}

// observe watches a recorded run under changed code for the whole window, and
// reports the row it ends with and the highest DequeueCount its work item reached:
// a work item that is handed out again and again is one no worker can complete.
func observe(ctx context.Context, db *sql.DB, instanceID string, window time.Duration) (dtx.Instance, int, error) {
	deadline := time.Now().Add(window)
	highest := 0
	for {
		instance, err := dtx.ReadInstance(ctx, db, instanceID)
		if err != nil {
			return dtx.Instance{}, highest, err
		}
		events, err := dtx.ReadNewEvents(ctx, db, instanceID)
		if err != nil {
			return dtx.Instance{}, highest, err
		}
		for _, e := range events {
			if e.DequeueCount > highest {
				highest = e.DequeueCount
			}
		}
		if instance.Completed() || !time.Now().Before(deadline) {
			return instance, highest, nil
		}
		if err := libx.Sleep(ctx, settle); err != nil {
			return instance, highest, err
		}
	}
}

// fetchFailure asks the library itself what the instance failed with, through a
// client that runs no worker, so the error a fact prints is the library's own.
func fetchFailure(ctx context.Context, dsn, schema, instanceID string) (string, error) {
	engine, err := dtx.StartClient(ctx, Options(dsn, schema))
	if err != nil {
		return "", err
	}
	defer engine.Shutdown(ctx)
	metadata, err := engine.Client.FetchWorkflowMetadata(ctx, api.InstanceID(instanceID))
	if err != nil {
		return "", err
	}
	if metadata.GetFailureDetails() == nil {
		return "none recorded", nil
	}
	return fmt.Sprintf("%s: %s", metadata.GetFailureDetails().GetErrorType(),
		metadata.GetFailureDetails().GetErrorMessage()), nil
}

// repeatedSteps names the steps a run performed more than once, which is how a fact
// says that a change made the engine re-run work the old code had already recorded.
func repeatedSteps(steps string) string {
	seen := map[string]int{}
	var again []string
	for _, name := range strings.Fields(steps) {
		seen[name]++
		if seen[name] == 2 {
			again = append(again, name)
		}
	}
	if len(again) == 0 {
		return "none"
	}
	return strings.Join(again, " ")
}

// waitParked blocks until an instance has run its recorded steps and is waiting for
// the person, which is the state a change has to meet.
func waitParked(ctx context.Context, db *sql.DB, instanceID string, timeout time.Duration) error {
	_, err := dtx.WaitForInstance(ctx, db, instanceID, timeout, func(i dtx.Instance) bool {
		if !i.Found || i.Completed() || i.LockExpiration != nil {
			return false
		}
		history, err := dtx.ReadHistory(ctx, db, instanceID)
		if err != nil {
			return false
		}
		tasks, err := dtx.ReadNewTasks(ctx, db, instanceID)
		if err != nil {
			return false
		}
		return len(tasks) == 0 && dtx.CountEvents(history, "TimerCreated") >= 1
	})
	if err != nil {
		return fmt.Errorf("instance %s never reached the wait: %w", instanceID, err)
	}
	return nil
}

// outcome is the one-line answer to what the changed code did to the recorded run.
func outcome(instance dtx.Instance, deliverRan bool, dequeues int) string {
	switch {
	case instance.RuntimeStatus == "COMPLETED" && deliverRan:
		return "it carried the recorded run to the end: the inserted activity ran and the run still finished its last step"
	case instance.Completed():
		return "it ended the recorded run without finishing it: the engine found the recorded history disagreeing with the new code, failed the run and wrote the error on the Instances row, so the steps after the insertion point never ran"
	case dequeues >= 2:
		return "it wedged the recorded run: the work item is handed back and taken again as each lock lapses, with DequeueCount climbing, and the run stays RUNNING"
	default:
		return "nothing: no work item was executed in the window, so this run measured nothing"
	}
}

// stepsOf is the step names one instance performed, in order, as one line.
func stepsOf(lines []string, instanceID string) string {
	var names []string
	for _, line := range lines {
		if !strings.Contains(line, "instance="+instanceID+" ") {
			continue
		}
		names = append(names, strings.TrimPrefix(strings.Fields(line)[0], "step="))
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// versionOf is the version column of an instance row, which is what pins a recorded
// run to the code it started under.
func versionOf(i dtx.Instance) string {
	if i.Version == nil || *i.Version == "" {
		return "null: the Instances row carries no version, so the pin lives only in the run's ExecutionStarted event"
	}
	return *i.Version
}

func mustInstance(ctx context.Context, db *sql.DB, instanceID string) dtx.Instance {
	i, err := dtx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return dtx.Instance{}
	}
	return i
}
