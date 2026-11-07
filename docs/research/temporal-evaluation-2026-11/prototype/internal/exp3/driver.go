package exp3

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings. They are short on purpose: the whole run, kill and recovery
// included, has to finish in well under two minutes.
const (
	// sessionHold is how long the session activity occupies its worker. It must be far
	// longer than the time the driver needs to see the activity start and send the kill.
	sessionHold = 30 * time.Second
	// creationTimeout bounds creating a session, sessionExecution bounds its whole life.
	creationTimeout  = 15 * time.Second
	sessionExecution = 55 * time.Second
	// sessionHeartbeat is how long the server waits for the session's heartbeat before
	// declaring the host gone. It sets how fast a killed worker becomes a failed session.
	sessionHeartbeat = 4 * time.Second

	workflowTimeout  = 80 * time.Second
	workerStartWait  = 30 * time.Second
	activityRunWait  = 20 * time.Second
	workflowWait     = 45 * time.Second
	restartObserve   = 8 * time.Second
	freshSessionHold = 1 * time.Second
)

// Run is experiment 3: it starts a session worker, runs a workflow that puts a long
// activity on a Temporal session that worker holds, kills that worker with SIGKILL while
// the activity runs, and reports what the workflow saw.
//
// suppressKill is the run's control: with it the driver sends no signal, so nothing
// should fail, and the experiment must report that it measured nothing.
//
// Run prints its measured facts as they are established and returns nil only when the
// workflow observed the session's failure after a kill that was really sent.
func Run(ctx context.Context, cfg harness.Config, suppressKill bool) error {
	if cfg.SessionWorker == "" {
		return fmt.Errorf("-session-worker is required by experiment 3")
	}
	if !filepath.IsAbs(cfg.SessionWorker) {
		return fmt.Errorf("-session-worker %q must be an absolute path", cfg.SessionWorker)
	}
	if info, err := os.Stat(cfg.SessionWorker); err != nil {
		return fmt.Errorf("-session-worker: %w", err)
	} else if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("-session-worker %q is not an executable file", cfg.SessionWorker)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = os.TempDir()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("-work-dir: %w", err)
	}

	c, err := harness.Dial(ctx, cfg, 30*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()

	// One pair of queues per run, so a rerun never meets the last run's leftovers.
	stamp := time.Now().UnixNano()
	sessionQueue := fmt.Sprintf("exp3-session-%d", stamp)
	workflowQueue := fmt.Sprintf("exp3-workflow-%d", stamp)
	harness.Fact("session-task-queue", sessionQueue)
	harness.Fact("workflow-task-queue", workflowQueue)

	// The workflow runs inside this process. Only the activities run on the worker that
	// gets killed, so the kill cannot take the driver's own workflow worker with it.
	wfWorker := worker.New(c, workflowQueue, worker.Options{})
	wfWorker.RegisterWorkflowWithOptions(SessionWorkflow, workflow.RegisterOptions{Name: WorkflowName})
	if err := wfWorker.Start(); err != nil {
		return fmt.Errorf("start workflow worker: %w", err)
	}
	defer wfWorker.Stop()

	params := Params{
		TaskQueue:        sessionQueue,
		Hold:             sessionHold,
		CreationTimeout:  creationTimeout,
		ExecutionTimeout: sessionExecution,
		HeartbeatTimeout: sessionHeartbeat,
	}

	// 1. The worker that will be killed.
	first, err := startChild(ctx, "worker1", cfg.SessionWorker, cfg.Address, cfg.Namespace, sessionQueue, workDir, workerStartWait)
	if err != nil {
		return err
	}
	defer first.stop()
	harness.Fact("worker1-started-pid", first.pid)
	harness.Fact("worker1-reported-pid", first.reportedPID)

	// 2. The workflow: a session on that worker, and one long activity on the session.
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp3-kill-%d", stamp),
		TaskQueue:                workflowQueue,
		WorkflowExecutionTimeout: workflowTimeout,
		WorkflowTaskTimeout:      10 * time.Second,
	}, WorkflowName, params)
	if err != nil {
		return fmt.Errorf("start workflow: %w", err)
	}
	harness.Fact("workflow-id", run.GetID())
	harness.Fact("run-id", run.GetRunID())

	// 3. Wait until the activity is really running on that worker, so the kill lands
	// inside it rather than before it or after it.
	running := waitActivityRunning(ctx, c, run.GetID(), run.GetRunID(), SessionActivityName, activityRunWait)
	harness.Fact("session-activity-running-before-kill", running == nil)
	if running != nil {
		harness.Fact("session-activity-wait-error", running)
	}

	// 4. The kill itself, by the exact pid the driver recorded.
	killSent := false
	killedAt := time.Now()
	switch {
	case suppressKill:
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
		harness.Fact("kill-suppressed", true)
	case running != nil:
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
	case !first.alive():
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
		harness.Fact("worker1-already-gone", true)
	default:
		killedAt = time.Now()
		if err := first.signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("kill worker1: %w", err)
		}
		killSent = true
		harness.Fact("kill-sent", true)
		harness.Fact("killed-pid", first.pid)
		harness.Fact("kill-signal", "SIGKILL")
	}

	// 5. What the workflow saw.
	waitCtx, cancelWait := context.WithTimeout(ctx, workflowWait)
	defer cancelWait()
	var res Result
	if err := run.Get(waitCtx, &res); err != nil {
		return fmt.Errorf("workflow %s did not return: %w", run.GetID(), err)
	}
	reportResult(res)

	hist, err := inspectHistory(ctx, c, run.GetID(), run.GetRunID(), killedAt)
	if err != nil {
		return err
	}
	harness.Fact("history-event-count", hist.events)
	harness.Fact("session-activity-start-count", hist.starts)
	harness.Fact("session-activity-starts-after-kill", hist.startsAfterKill)
	harness.Fact("session-activity-final-event", hist.final)

	observed := res.SessionCreated &&
		res.ActivityResult == "" &&
		res.WaitErrorText != "" &&
		res.SessionStateAfter == int(workflow.SessionStateFailed) &&
		res.ProbeIsSessionFailed
	harness.Fact("workflow-observed-session-failure", observed)

	// 6. Does the session come back on its own once a worker is there again?
	if killSent && observed {
		if err := measureRecovery(ctx, c, cfg, sessionQueue, workflowQueue, workDir, params, run, killedAt); err != nil {
			return err
		}
	} else {
		harness.Fact("second-worker-started", false)
		harness.Fact("session-activity-resumed-after-restart", "not measured")
		harness.Fact("new-session-after-restart", "not measured")
	}

	// 7. The verdict. A run that killed nothing measured nothing, whatever else it saw.
	if !killSent {
		return fmt.Errorf("no worker was killed, so this run measured nothing about session failure")
	}
	if !observed {
		return fmt.Errorf("the workflow did not observe the session's failure: created=%t activity-result=%q wait-error=%q session-state=%d probe-is-ErrSessionFailed=%t",
			res.SessionCreated, res.ActivityResult, res.WaitErrorText, res.SessionStateAfter, res.ProbeIsSessionFailed)
	}
	return nil
}

// reportResult prints, verbatim, what the workflow recorded. The error texts are the
// evidence the write-up quotes, so they are printed as the workflow saw them.
func reportResult(res Result) {
	harness.Fact("session-created", res.SessionCreated)
	if res.CreateError != "" {
		harness.Fact("session-create-error", res.CreateError)
	}
	harness.Fact("session-id", res.SessionID)
	harness.Fact("session-host", res.SessionHost)
	harness.Fact("session-activity-result", orNone(res.ActivityResult))
	harness.Fact("workflow-wait-error-type", orNone(res.WaitErrorType))
	harness.Fact("workflow-wait-error", orNone(res.WaitErrorText))
	harness.Fact("workflow-wait-is-ErrSessionFailed", res.WaitIsSessionFailed)
	harness.Fact("workflow-wait-is-cancellation", res.WaitIsCanceled)
	harness.Fact("session-state-after-wait", sessionStateName(res.SessionStateAfter))
	harness.Fact("session-context-error", orNone(res.SessionCtxError))
	harness.Fact("session-probe-ran", res.ProbeScheduled)
	harness.Fact("session-probe-error-type", orNone(res.ProbeErrorType))
	harness.Fact("session-probe-error", orNone(res.ProbeErrorText))
	harness.Fact("session-probe-is-ErrSessionFailed", res.ProbeIsSessionFailed)
}

// measureRecovery starts a second worker on the same task queue and records both halves
// of "does the session come back": whether the dead session's activity resumes anywhere,
// and whether a session created afresh on that queue succeeds.
func measureRecovery(
	ctx context.Context,
	c client.Client,
	cfg harness.Config,
	sessionQueue, workflowQueue, workDir string,
	params Params,
	killedRun client.WorkflowRun,
	killedAt time.Time,
) error {
	second, err := startChild(ctx, "worker2", cfg.SessionWorker, cfg.Address, cfg.Namespace, sessionQueue, workDir, workerStartWait)
	if err != nil {
		return err
	}
	defer second.stop()
	harness.Fact("second-worker-started", true)
	harness.Fact("worker2-started-pid", second.pid)

	// Give the new worker time to pick the old session's activity up, if it ever would.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(restartObserve):
	}
	hist, err := inspectHistory(ctx, c, killedRun.GetID(), killedRun.GetRunID(), killedAt)
	if err != nil {
		return err
	}
	harness.Fact("history-event-count-after-restart", hist.events)
	harness.Fact("session-activity-start-count-after-restart", hist.starts)
	harness.Fact("session-activity-final-event-after-restart", hist.final)
	harness.Fact("session-activity-resumed-after-restart", hist.startsAfterKill > 0)
	desc, err := c.DescribeWorkflowExecution(ctx, killedRun.GetID(), killedRun.GetRunID())
	if err != nil {
		return fmt.Errorf("describe %s: %w", killedRun.GetID(), err)
	}
	harness.Fact("killed-run-status-after-restart", desc.GetWorkflowExecutionInfo().GetStatus())

	// A brand new session on the same queue: does the queue still work at all?
	fresh := params
	fresh.Hold = freshSessionHold
	freshRun, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp3-fresh-%d", time.Now().UnixNano()),
		TaskQueue:                workflowQueue,
		WorkflowExecutionTimeout: workflowTimeout,
		WorkflowTaskTimeout:      10 * time.Second,
	}, WorkflowName, fresh)
	if err != nil {
		return fmt.Errorf("start fresh-session workflow: %w", err)
	}
	freshCtx, cancel := context.WithTimeout(ctx, workflowWait)
	defer cancel()
	var freshRes Result
	if err := freshRun.Get(freshCtx, &freshRes); err != nil {
		harness.Fact("new-session-after-restart", fmt.Sprintf("failed: %v", err))
		return nil
	}
	switch {
	case freshRes.SessionCreated && freshRes.ActivityResult != "":
		harness.Fact("new-session-after-restart", "succeeded")
		harness.Fact("new-session-activity-ran-on", freshRes.ActivityResult)
		harness.Fact("new-session-host", freshRes.SessionHost)
	case !freshRes.SessionCreated:
		harness.Fact("new-session-after-restart", "failed: "+orNone(freshRes.CreateError))
	default:
		harness.Fact("new-session-after-restart", "failed: "+orNone(freshRes.WaitErrorText))
	}
	return nil
}

// waitActivityRunning blocks until the named activity of the run is started on a worker
// and has heartbeated at least once, which together mean a worker is inside it now.
func waitActivityRunning(ctx context.Context, c client.Client, workflowID, runID, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		desc, err := c.DescribeWorkflowExecution(ctx, workflowID, runID)
		if err != nil {
			last = err.Error()
		} else {
			last = "no pending activity named " + name
			for _, pending := range desc.GetPendingActivities() {
				if pending.GetActivityType().GetName() != name {
					continue
				}
				last = fmt.Sprintf("%s is %s, heartbeated=%t",
					name, pending.GetState(), pending.GetLastHeartbeatTime() != nil)
				if pending.GetState() == enumspb.PENDING_ACTIVITY_STATE_STARTED && pending.GetLastHeartbeatTime() != nil {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("activity %s was not running after %s: %s", name, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// historyFacts is what a run's history says about the session activity.
type historyFacts struct {
	// events is every event of the run.
	events int
	// starts is the ActivityTaskStarted events the session activity has. The server
	// writes one only when an attempt ends, so an attempt whose worker was killed
	// mid-run leaves none: a start here is an attempt that ran somewhere to an end.
	starts int
	// startsAfterKill is how many of those starts happened after the kill, which is
	// how the experiment sees the work resume on a worker other than the dead one.
	startsAfterKill int
	// final is the last history event the session activity reached.
	final string
}

// inspectHistory reads a run's whole history and reports what it says about the session
// activity: how often it started, how often it started after killedAt, and where it
// ended up.
func inspectHistory(ctx context.Context, c client.Client, workflowID, runID string, killedAt time.Time) (historyFacts, error) {
	facts := historyFacts{final: "none"}
	scheduled := map[int64]bool{}
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return historyFacts{}, fmt.Errorf("read history of %s: %w", workflowID, err)
		}
		facts.events++
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			if attrs.GetActivityType().GetName() == SessionActivityName {
				scheduled[event.GetEventId()] = true
				facts.final = eventName(event)
			}
			continue
		}
		id, ok := activityEventScheduledID(event)
		if !ok || !scheduled[id] {
			continue
		}
		facts.final = eventName(event)
		if event.GetEventType() == enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED {
			facts.starts++
			if !event.GetEventTime().AsTime().Before(killedAt) {
				facts.startsAfterKill++
			}
		}
	}
	return facts, nil
}

// activityEventScheduledID is the scheduled event an activity event belongs to, for the
// activity events this experiment follows.
func activityEventScheduledID(event *historypb.HistoryEvent) (int64, bool) {
	switch attrs := event.GetAttributes().(type) {
	case *historypb.HistoryEvent_ActivityTaskStartedEventAttributes:
		return attrs.ActivityTaskStartedEventAttributes.GetScheduledEventId(), true
	case *historypb.HistoryEvent_ActivityTaskCompletedEventAttributes:
		return attrs.ActivityTaskCompletedEventAttributes.GetScheduledEventId(), true
	case *historypb.HistoryEvent_ActivityTaskFailedEventAttributes:
		return attrs.ActivityTaskFailedEventAttributes.GetScheduledEventId(), true
	case *historypb.HistoryEvent_ActivityTaskTimedOutEventAttributes:
		return attrs.ActivityTaskTimedOutEventAttributes.GetScheduledEventId(), true
	case *historypb.HistoryEvent_ActivityTaskCancelRequestedEventAttributes:
		return attrs.ActivityTaskCancelRequestedEventAttributes.GetScheduledEventId(), true
	case *historypb.HistoryEvent_ActivityTaskCanceledEventAttributes:
		return attrs.ActivityTaskCanceledEventAttributes.GetScheduledEventId(), true
	default:
		return 0, false
	}
}

// eventName is a history event's type without the protobuf enum's prefix.
func eventName(event *historypb.HistoryEvent) string {
	return strings.TrimPrefix(event.GetEventType().String(), "EVENT_TYPE_")
}

// sessionStateName names the SessionState the workflow reported.
func sessionStateName(state int) string {
	switch workflow.SessionState(state) {
	case workflow.SessionStateOpen:
		return "open"
	case workflow.SessionStateFailed:
		return "failed"
	case workflow.SessionStateClosed:
		return "closed"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
