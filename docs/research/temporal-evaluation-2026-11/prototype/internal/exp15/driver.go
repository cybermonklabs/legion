package exp15

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/registry"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/gwx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The driver's own timings.
const (
	recordWait = 30 * time.Second
	// changeObserve is how long the changed code is left in front of the recorded
	// run. It spans several workflow task locks, so a task that is retried rather
	// than completed is seen being retried.
	changeObserve  = 14 * time.Second
	completionWait = 30 * time.Second
	settle         = 250 * time.Millisecond
)

// Run is experiment 15. It records a run under one version of a workflow, changes
// that workflow by inserting an activity in the middle, and lets the changed code
// meet the recorded run. Then it does what the library documents instead --
// side-by-side deployment, with the old code still serving the runs it recorded --
// and measures whether that carries the same kind of run to the end.
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

	schema := libx.SchemaName("exp15")
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

	now := time.Now().UnixNano()
	changed := fmt.Sprintf("exp15-unguarded-%d", now)
	sideBySide := fmt.Sprintf("exp15-side-by-side-%d", now)
	fresh := fmt.Sprintf("exp15-new-%d", now)

	harness.Fact("library", "github.com/cschleiden/go-workflows v1.4.2")
	harness.Fact("database", libx.EvalDatabase)
	harness.Fact("schema", schema)
	harness.Fact("versioning-api-in-the-library", "none: go-workflows has no workflow.Version and no patch; its FAQ says to rely on side-by-side deployments, and to use queues to route instances to the workers that can run them")
	harness.Fact("recorded-code", "plan, implement, wait for a person, deliver")
	harness.Fact("changed-code", "plan, review, implement, wait for a person, deliver: one new activity in the middle")
	harness.Fact("workflow-task-lock-this-run-used", WorkflowLockTimeout.String())

	// 1. The recorded runs, under the code as it stood before the change. Both are
	// parked on the wait for a person, which is where a change finds them.
	recorder := gwx.Backend(db, Options("exp15-old-deployment"))
	defer recorder.Close() //nolint:errcheck // the backend does not own the pool
	recorderClient := client.New(recorder)

	recorderCtx, stopRecorder := context.WithCancel(ctx)
	defer stopRecorder()
	if err := startWorker(recorderCtx, recorder, RecordedWorkflow,
		[]workflow.Queue{QueueChanged, QueueSideBySide}); err != nil {
		return err
	}
	for id, queue := range map[string]workflow.Queue{changed: QueueChanged, sideBySide: QueueSideBySide} {
		if _, err := recorderClient.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{
			InstanceID: id, Queue: queue,
		}, WorkflowName, Params{SideEffectFile: steps, Instance: id}); err != nil {
			return fmt.Errorf("create instance %s: %w", id, err)
		}
	}
	for _, id := range []string{changed, sideBySide} {
		if err := waitParked(ctx, db, id, recordWait); err != nil {
			return err
		}
	}
	recordedHistory, err := gwx.ReadHistory(ctx, db, changed)
	if err != nil {
		return err
	}
	harness.Fact("recorded-instance", changed)
	harness.Fact("sql-recorded-history", gwx.Events(recordedHistory))
	harness.Fact("sql-recorded-instances", mustInstance(ctx, db, changed))
	recordedSteps, err := libx.ReadLines(steps)
	if err != nil {
		return err
	}
	harness.Fact("steps-performed-while-recording", len(recordedSteps))
	for _, line := range recordedSteps {
		harness.Fact("recorded-step", line)
	}

	// The old deployment goes away, as it would in an ordinary rollout.
	stopRecorder()
	harness.Fact("old-deployment-stopped-before-the-change", true)

	// 2. The unguarded change: the new code, and nothing else, in front of a run
	// that recorded the old shape.
	logs := &captureHandler{}
	changedBackend := gwx.Backend(db, gwx.Options{
		WorkerName:          "exp15-changed-deployment",
		ActivityLockTimeout: ActivityLockTimeout,
		WorkflowLockTimeout: WorkflowLockTimeout,
		StickyTimeout:       StickyTimeout,
		Logger:              slog.New(logs),
	})
	defer changedBackend.Close() //nolint:errcheck // the backend does not own the pool
	changedCtx, stopChanged := context.WithCancel(ctx)
	defer stopChanged()
	if err := startWorker(changedCtx, changedBackend, ChangedWorkflow, []workflow.Queue{QueueChanged}); err != nil {
		return err
	}
	if err := client.New(changedBackend).SignalWorkflow(ctx, changed, ReleaseSignal, "approved"); err != nil {
		return fmt.Errorf("signal %s: %w", changed, err)
	}
	harness.Fact("recorded-run-woken-under-the-changed-code", true)

	// Watch what the changed code does to the recorded run for several task locks.
	deadline := time.Now().Add(changeObserve)
	locks := map[string]bool{}
	finished := false
	for time.Now().Before(deadline) {
		instance, err := gwx.ReadInstance(ctx, db, changed)
		if err != nil {
			return err
		}
		if instance.LockedUntil != nil {
			locks[instance.LockedUntil.String()] = true
		}
		if instance.Finished() {
			finished = true
			break
		}
		if err := libx.Sleep(ctx, settle); err != nil {
			return err
		}
	}
	afterChange, err := gwx.ReadInstance(ctx, db, changed)
	if err != nil {
		return err
	}
	afterChangeHistory, err := gwx.ReadHistory(ctx, db, changed)
	if err != nil {
		return err
	}
	afterChangePending, err := gwx.ReadPendingEvents(ctx, db, changed)
	if err != nil {
		return err
	}
	changedSteps, err := libx.ReadLines(steps)
	if err != nil {
		return err
	}
	finishPayloads, err := gwx.ReadEventPayloads(ctx, db, changed, "WorkflowExecutionFinished")
	if err != nil {
		return err
	}
	failures, firstError := logs.errors()
	unguardedSteps := stepsOf(changedSteps, changed)
	deliverRan := strings.Contains(unguardedSteps, StepDeliver)

	harness.Fact("what-the-changed-code-did-to-the-recorded-run", outcome(finished, deliverRan, failures))
	harness.Fact("steps-the-recorded-run-performed-in-the-end", unguardedSteps)
	harness.Fact("inserted-activity-ran-against-the-recorded-run", strings.Contains(unguardedSteps, StepReview))
	harness.Fact("already-recorded-activity-run-again-by-the-changed-code", repeatedSteps(unguardedSteps))
	harness.Fact("recorded-run-reached-its-last-step", deliverRan)
	harness.Fact("error-the-library-logged", orNone(firstError))
	harness.Fact("workflow-tasks-the-changed-code-failed-in-"+changeObserve.String(), failures)
	harness.Fact("workflow-task-locks-taken-on-the-run-in-that-window", len(locks))
	harness.Fact("state-the-run-is-left-in", gwx.StateName(afterChange.State))
	for _, payload := range finishPayloads {
		harness.Fact("sql-after-the-change-finish-event-attributes", payload)
	}
	harness.Fact("sql-after-the-change-instances", afterChange)
	harness.Fact("sql-after-the-change-history", gwx.Events(afterChangeHistory))
	harness.Fact("sql-after-the-change-pending_events", gwx.Events(afterChangePending)+" -- events left undelivered because the instance was already finished when they arrived")
	harness.Fact("history-the-old-code-recorded-is-still-its-prefix", gwx.Events(recordedHistory) == gwx.Events(afterChangeHistory[:min(len(afterChangeHistory), len(recordedHistory))]))
	harness.Fact("history-rows-before-the-change", len(recordedHistory))
	harness.Fact("history-rows-after-the-change", len(afterChangeHistory))
	harness.Fact("steps-performed-by-the-changed-code-on-the-recorded-run", len(changedSteps)-len(recordedSteps))
	harness.Fact("is-the-history-corrupted", "no: the rows the old code recorded are still the run's history, in order, and the changed code appended to them rather than rewriting them -- what the change cost is the rest of the run, not the record of what it did")
	stopChanged()
	harness.Fact("changed-deployment-stopped", true)

	// 3. The documented alternative: side-by-side. The old code is still deployed on
	// its own queue and finishes the runs it recorded; the new code takes new runs.
	oldStill := gwx.Backend(db, Options("exp15-old-deployment-kept"))
	defer oldStill.Close() //nolint:errcheck // the backend does not own the pool
	oldCtx, stopOld := context.WithCancel(ctx)
	defer stopOld()
	if err := startWorker(oldCtx, oldStill, RecordedWorkflow, []workflow.Queue{QueueSideBySide}); err != nil {
		return err
	}
	newCode := gwx.Backend(db, Options("exp15-new-deployment"))
	defer newCode.Close() //nolint:errcheck // the backend does not own the pool
	newCtx, stopNew := context.WithCancel(ctx)
	defer stopNew()
	if err := startWorker(newCtx, newCode, ChangedWorkflow, []workflow.Queue{QueueNew}); err != nil {
		return err
	}

	if err := client.New(oldStill).SignalWorkflow(ctx, sideBySide, ReleaseSignal, "approved"); err != nil {
		return fmt.Errorf("signal %s: %w", sideBySide, err)
	}
	oldFinal, err := gwx.WaitForInstance(ctx, db, sideBySide, completionWait, gwx.Instance.Finished)
	if err != nil {
		return err
	}
	harness.Fact("side-by-side-recorded-run-finished-under-the-old-code", oldFinal.Finished())
	harness.Fact("sql-side-by-side-instances", oldFinal)
	harness.Fact("sql-side-by-side-history", gwx.Events(mustHistory(ctx, db, sideBySide)))

	freshClient := client.New(newCode)
	if _, err := freshClient.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{
		InstanceID: fresh, Queue: QueueNew,
	}, WorkflowName, Params{SideEffectFile: steps, Instance: fresh}); err != nil {
		return fmt.Errorf("create instance %s: %w", fresh, err)
	}
	if err := waitParked(ctx, db, fresh, recordWait); err != nil {
		return err
	}
	if err := freshClient.SignalWorkflow(ctx, fresh, ReleaseSignal, "approved"); err != nil {
		return fmt.Errorf("signal %s: %w", fresh, err)
	}
	freshFinal, err := gwx.WaitForInstance(ctx, db, fresh, completionWait, gwx.Instance.Finished)
	if err != nil {
		return err
	}
	harness.Fact("new-run-under-the-changed-code-finished", freshFinal.Finished())
	harness.Fact("sql-new-run-history", gwx.Events(mustHistory(ctx, db, fresh)))

	allSteps, err := libx.ReadLines(steps)
	if err != nil {
		return err
	}
	for _, line := range allSteps {
		harness.Fact("step-line", line)
	}
	oldSteps := stepsOf(allSteps, sideBySide)
	newSteps := stepsOf(allSteps, fresh)
	harness.Fact("steps-the-old-code-ran-for-its-recorded-run", oldSteps)
	harness.Fact("steps-the-new-code-ran-for-a-new-run", newSteps)
	harness.Fact("side-by-side-cost", "two deployments and a queue per version, for as long as the oldest unfinished run lives: the library offers nothing that lets one deployment run both shapes")

	harness.Fact("limit", "one inserted activity, one recorded run per treatment, on one PostgreSQL: it shows what an unguarded change does to a run that recorded the old shape and that side-by-side deployment carries such a run to the end, not how a fleet rolls out, and not what a change that only reorders or renames would do")

	switch {
	case len(recordedSteps) != 4:
		return fmt.Errorf("recording the two runs performed %d steps, not the 4 the old code has (plan and implement for each)", len(recordedSteps))
	case !finished && failures == 0:
		return fmt.Errorf("in %s the changed code neither finished the recorded run nor failed a workflow task for it, so nothing was measured about what the change does to a recorded run", changeObserve)
	case finished && deliverRan:
		return fmt.Errorf("the recorded run reached its last step under the changed code, so this run measured no cost to an unguarded change and the comparison with side-by-side deployment means nothing: %s", unguardedSteps)
	case len(afterChangeHistory) < len(recordedHistory):
		return fmt.Errorf("the recorded run's history shrank from %d rows to %d, which this experiment cannot read back", len(recordedHistory), len(afterChangeHistory))
	case !oldFinal.Finished():
		return fmt.Errorf("the side-by-side run did not finish under the old code: %s", oldFinal)
	case !freshFinal.Finished():
		return fmt.Errorf("the new run did not finish under the changed code: %s", freshFinal)
	case !strings.Contains(newSteps, StepReview):
		return fmt.Errorf("the new run did not run the inserted step: %s", newSteps)
	case strings.Contains(oldSteps, StepReview):
		return fmt.Errorf("the old code ran the inserted step for its recorded run: %s", oldSteps)
	}
	return nil
}

// startWorker starts one worker on be with body registered under the workflow's one
// name, and the four step activities beside it.
func startWorker(ctx context.Context, be backend.Backend, body workflow.Workflow, queues []workflow.Queue) error {
	w := worker.New(be, &worker.Options{
		WorkflowWorkerOptions: worker.WorkflowWorkerOptions{
			WorkflowPollers:           1,
			WorkflowHeartbeatInterval: WorkflowHeartbeatInterval,
			WorkflowQueues:            queues,
		},
		ActivityWorkerOptions: worker.ActivityWorkerOptions{
			ActivityPollers:           1,
			ActivityHeartbeatInterval: ActivityHeartbeatInterval,
			ActivityQueues:            queues,
		},
	})
	if err := w.RegisterWorkflow(body, registry.WithName(WorkflowName)); err != nil {
		return fmt.Errorf("register the workflow: %w", err)
	}
	for _, name := range []string{StepPlan, StepReview, StepImplement, StepDeliver} {
		if err := w.RegisterActivity(Step, registry.WithName(name)); err != nil {
			return fmt.Errorf("register activity %s: %w", name, err)
		}
	}
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("start a worker: %w", err)
	}
	return nil
}

// waitParked blocks until an instance has run its recorded steps and is waiting for
// the person, which is the state a change has to meet.
func waitParked(ctx context.Context, db *sql.DB, instanceID string, timeout time.Duration) error {
	_, err := gwx.WaitForInstance(ctx, db, instanceID, timeout, func(i gwx.Instance) bool {
		if !i.Found || i.Finished() || i.LockedUntil != nil {
			return false
		}
		events, err := gwx.ReadHistory(ctx, db, instanceID)
		if err != nil {
			return false
		}
		pending, err := gwx.ReadPendingEvents(ctx, db, instanceID)
		if err != nil {
			return false
		}
		return len(pending) == 0 && gwx.CountEvents(events, "ActivityCompleted") >= 2
	})
	if err != nil {
		return fmt.Errorf("instance %s never reached the wait: %w", instanceID, err)
	}
	return nil
}

// captureHandler is the slog handler the changed deployment logs into, so the error
// the library reports for a recorded run is a measured string rather than a guess.
type captureHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, b.String())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// errors is how many workflow tasks the worker failed, and the first failure's text.
func (h *captureHandler) errors() (int, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	first := ""
	for _, line := range h.lines {
		if strings.Contains(line, "error handling task") {
			n++
			if first == "" {
				first = line
			}
		}
	}
	return n, first
}

// outcome is the one-line answer to what the changed code did to the recorded run.
func outcome(finished, deliverRan bool, failures int) string {
	switch {
	case finished && deliverRan:
		return "it carried the recorded run to the end: the inserted activity ran and the run still finished its last step"
	case finished:
		return "it ended the recorded run without finishing it: the changed code ran the inserted activity against a history that did not have it, the replay error went into the run's WorkflowExecutionFinished event, and the steps after the insertion point never ran -- the signal and the inserted activity's result were left undelivered in pending_events"
	case failures > 0:
		return "it wedged the recorded run: every workflow task the changed code takes for that instance fails on replay and is never completed, so the instance stays active, re-locked and re-attempted as each task lock lapses"
	default:
		return "nothing: no workflow task was executed in the window, so this run measured nothing"
	}
}

// stepsOf is the step names one instance performed, in order, as one line.
func stepsOf(lines []string, instanceID string) string {
	var names []string
	for _, line := range lines {
		if !strings.Contains(line, "instance="+instanceID+" ") {
			continue
		}
		name := strings.TrimPrefix(strings.Fields(line)[0], "step=")
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// repeatedSteps names the steps a run performed more than once, which is how a fact
// says that a change made the library re-run work the old code had already recorded.
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

func mustInstance(ctx context.Context, db *sql.DB, instanceID string) gwx.Instance {
	i, err := gwx.ReadInstance(ctx, db, instanceID)
	if err != nil {
		return gwx.Instance{}
	}
	return i
}

func mustHistory(ctx context.Context, db *sql.DB, instanceID string) []string {
	events, err := gwx.ReadHistory(ctx, db, instanceID)
	if err != nil {
		return nil
	}
	return events
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
