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

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp19"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings. They are short on purpose: the whole run -- the kill, the
// replacement worker, the window it polls in and the fresh session after it -- has to
// finish in well under two minutes.
const (
	dialTimeout = 30 * time.Second
	// sessionHold is how long the session activity occupies its worker. It must be far
	// longer than the time the driver needs to see the activity start and send the kill.
	sessionHold = 30 * time.Second
	// creationTimeout bounds creating a session, sessionExecution bounds its whole life.
	creationTimeout  = 15 * time.Second
	sessionExecution = 55 * time.Second
	// sessionHeartbeat is how long the server waits for the session's heartbeat before
	// declaring the host gone. It sets how fast a killed worker becomes a failed
	// session, and with it how long the server holds the killed attempt before it ends
	// it: long enough here that the replacement worker is polling while the server
	// still holds the activity for the process that died.
	sessionHeartbeat = 15 * time.Second

	// holdOpenBound is the workflow's own bound on the wait for the release signal: a
	// driver that died before sending it leaves no run sitting to the end of its
	// execution timeout. It is far longer than the watching below takes.
	holdOpenBound = 90 * time.Second

	workflowTimeout = 180 * time.Second
	workerStartWait = 30 * time.Second
	activityRunWait = 20 * time.Second
	bodyWait        = 20 * time.Second
	workflowWait    = 60 * time.Second
	// settleAfterKill is how long the server is left alone with a dead worker, with no
	// replacement anywhere, before the activity is looked at again.
	settleAfterKill = 2 * time.Second
	// replacementObserve is how long the replacement worker polls before the reading
	// that catches the killed attempt still pending: the server is holding it for a
	// process that no longer exists while another worker asks that same base queue for
	// work.
	replacementObserve = 6 * time.Second
	// attemptEndWait bounds the wait for the killed attempt to end. The control run
	// kills nothing, so there the attempt ends only when the hold does.
	attemptEndWait = 60 * time.Second
	// afterEndObserve is how long the replacement worker keeps polling after the
	// attempt has ended, which is the window the server could re-dispatch the work in.
	afterEndObserve  = 5 * time.Second
	freshSessionHold = 1 * time.Second
	// describePoll is how often the driver asks the server what it holds, when it is
	// waiting for the server to reach a state rather than watching for a while.
	describePoll = 200 * time.Millisecond
)

// Run is experiment 3: it starts a session worker, runs a workflow that puts a long
// activity on a Temporal session that worker holds, kills that worker with SIGKILL while
// the activity runs, starts a replacement worker on the same base task queue, and reads
// -- while the workflow is still running and that replacement is polling -- what the
// server did with the killed activity and how often its body ran.
//
// suppressKill is the run's control: with it the driver sends no signal, so nothing
// should fail, and the experiment must report that it measured nothing.
//
// Run prints its measured facts as they are established and returns nil only when the
// workflow observed the session's failure after a kill that was really sent, with the
// run still running while the replacement worker polled.
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
	workDir, err := harness.WorkDir(cfg)
	if err != nil {
		return err
	}

	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	// One pair of queues per run, so a rerun never meets the last run's leftovers.
	stamp := time.Now().UnixNano()
	sessionQueue := fmt.Sprintf("exp3-session-%d", stamp)
	workflowQueue := fmt.Sprintf("exp3-workflow-%d", stamp)
	creationQueue := sessionQueue + CreationQueueSuffix

	params := Params{
		TaskQueue:        sessionQueue,
		Hold:             sessionHold,
		EffectFile:       filepath.Join(workDir, fmt.Sprintf("exp3-session-effects-%d", stamp)),
		HoldOpen:         holdOpenBound,
		CreationTimeout:  creationTimeout,
		ExecutionTimeout: sessionExecution,
		HeartbeatTimeout: sessionHeartbeat,
	}

	harness.Fact("mechanism", "Temporal sessions: workflow.CreateSession pins every activity of the session to the one worker process that took it")
	harness.Fact("session-task-queue", sessionQueue)
	harness.Fact("session-creation-task-queue", creationQueue)
	harness.Fact("workflow-task-queue", workflowQueue)
	harness.Fact("session-activity-side-effect-file", params.EffectFile)
	harness.Fact("workflow-held-open-by", fmt.Sprintf("the %s signal: after probing the session the workflow waits for it, and the driver sends it only once the replacement worker has polled, so every reading below is taken from a run that is still going", ReleaseSignalName))
	harness.Fact("workflow-hold-open-fallback", holdOpenBound)

	// The workflow runs inside this process. Only the activities run on the worker that
	// gets killed, so the kill cannot take the driver's own workflow worker with it.
	wfWorker := worker.New(c, workflowQueue, worker.Options{})
	wfWorker.RegisterWorkflowWithOptions(SessionWorkflow, workflow.RegisterOptions{Name: WorkflowName})
	if err := wfWorker.Start(); err != nil {
		return fmt.Errorf("start workflow worker: %w", err)
	}
	defer wfWorker.Stop()

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
	} else {
		lines, waitErr := exp19.WaitForLines(ctx, params.EffectFile, 1, bodyWait)
		if waitErr != nil {
			return fmt.Errorf("the session activity's body wrote no line: %w", waitErr)
		}
		harness.Fact("session-activity-first-body-line", lines[0])
	}

	killedAt := time.Now()
	before, err := observe(ctx, c, run, params.EffectFile, killedAt)
	if err != nil {
		return err
	}
	before.report("before-kill")

	// 4. The kill itself, by the exact pid the driver recorded.
	killSent := false
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

	// 5. The server, left alone with a dead worker.
	if err := sleep(ctx, settleAfterKill); err != nil {
		return err
	}
	harness.Fact("timed: watched-after-the-kill-for", settleAfterKill)
	afterKill, err := observe(ctx, c, run, params.EffectFile, killedAt)
	if err != nil {
		return err
	}
	afterKill.report("after-kill")

	// 6. The replacement worker: a worker that could run the session's activity again,
	// if the server ever offered it. It polls the same base task queue the session was
	// created on, and the workflow is still running the whole time it does.
	second, err := startChild(ctx, "worker2", cfg.SessionWorker, cfg.Address, cfg.Namespace, sessionQueue, workDir, workerStartWait)
	if err != nil {
		return err
	}
	defer second.stop()
	harness.Fact("second-worker-started", true)
	harness.Fact("worker2-started-pid", second.pid)
	harness.Fact("worker2-polls-task-queue", sessionQueue)
	if err := sleep(ctx, replacementObserve); err != nil {
		return err
	}
	harness.Fact("timed: replacement-worker-polled-for", replacementObserve)

	// 7. The first of the two readings the claim rests on: the server is still holding
	// the attempt for the process it was killed on, and another worker is asking the
	// base queue for work.
	during, err := observe(ctx, c, run, params.EffectFile, killedAt)
	if err != nil {
		return err
	}
	during.report("while-replacement-polls")
	harness.Fact("session-activity-task-started-events-after-the-kill-while-replacement-polls", during.hist.startsAfterKill)

	// 8. The second: the attempt has ended, so the server is free to put the work
	// somewhere else, the workflow is still running and the replacement is still
	// polling. This is the window a re-dispatch would happen in.
	ended, err := waitAttemptEnded(ctx, c, run, attemptEndWait)
	if err != nil {
		return err
	}
	harness.Fact("killed-attempt-ended", ended)
	harness.Fact("timed: killed-attempt-stayed-pending-after-the-kill-for", time.Since(killedAt).Round(time.Millisecond))
	if err := sleep(ctx, afterEndObserve); err != nil {
		return err
	}
	harness.Fact("timed: replacement-worker-kept-polling-after-the-attempt-ended-for", afterEndObserve)
	afterEnd, err := observe(ctx, c, run, params.EffectFile, killedAt)
	if err != nil {
		return err
	}
	afterEnd.report("after-the-attempt-ended")
	harness.Fact("session-activity-task-started-events-after-the-kill-once-the-attempt-had-ended", afterEnd.hist.startsAfterKill)
	harness.Fact("session-activity-retry-state-when-the-attempt-ended", afterEnd.hist.retryState)

	// The queues: the one the session's activity was dispatched to, as the server
	// recorded it, against the one the replacement worker polls. This is the mechanism
	// the write-up names as the reason no other worker can take the work, so the run
	// prints both names and what the server says is polling each of them, read at the
	// moment above: the attempt over, the run going, the replacement polling.
	harness.Fact("session-resource-specific-task-queue", afterEnd.hist.activityQueue)
	harness.Fact("session-creation-activity-task-queue", afterEnd.hist.creationQueue)
	harness.Fact("session-resource-specific-task-queue-is-the-base-task-queue", afterEnd.hist.activityQueue == sessionQueue)
	sessionPollers, err := describePollers(ctx, c, afterEnd.hist.activityQueue, killedAt)
	if err != nil {
		return err
	}
	sessionPollers.report("session-resource-specific-task-queue")
	basePollers, err := describePollers(ctx, c, sessionQueue, killedAt)
	if err != nil {
		return err
	}
	basePollers.report("session-task-queue")
	creationPollers, err := describePollers(ctx, c, creationQueue, killedAt)
	if err != nil {
		return err
	}
	creationPollers.report("session-creation-task-queue")

	// 9. Let the workflow end, and read what it saw.
	note := fmt.Sprintf("the driver finished watching the killed session at %s, after the replacement worker had polled for %s and kept polling for %s past the end of the killed attempt",
		time.Now().UTC().Format(time.RFC3339Nano), replacementObserve, afterEndObserve)
	if err := c.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), ReleaseSignalName, note); err != nil {
		return fmt.Errorf("send %s to %s: %w", ReleaseSignalName, run.GetID(), err)
	}
	harness.Fact("release-signal-sent", true)
	harness.Fact("release-signal-name", ReleaseSignalName)

	waitCtx, cancelWait := context.WithTimeout(ctx, workflowWait)
	defer cancelWait()
	var res Result
	if err := run.Get(waitCtx, &res); err != nil {
		return fmt.Errorf("workflow %s did not return: %w", run.GetID(), err)
	}
	reportResult(res)

	// 10. The end state, once the run this experiment is about is over.
	final, err := observe(ctx, c, run, params.EffectFile, killedAt)
	if err != nil {
		return err
	}
	harness.Fact("session-body-executions-total", final.bodies)
	harness.Fact("history-event-count", final.hist.events)
	harness.Fact("session-activity-schedule-count", final.hist.scheduled)
	harness.Fact("session-activity-start-count", final.hist.starts)
	harness.Fact("session-activity-starts-after-kill", final.hist.startsAfterKill)
	harness.Fact("session-activity-final-event", final.hist.final)
	harness.Fact("killed-run-status-after-release", final.status)

	observed := res.SessionCreated &&
		res.ActivityResult == "" &&
		res.WaitErrorText != "" &&
		res.SessionStateAfter == int(workflow.SessionStateFailed) &&
		res.ProbeIsSessionFailed
	harness.Fact("workflow-observed-session-failure", observed)

	// 11. A brand new session on the same queue: does the queue still work at all, now
	// that the only worker on it is the replacement?
	if err := measureFreshSession(ctx, c, workDir, workflowQueue, params, stamp); err != nil {
		return err
	}

	harness.Fact("reasoned: what-the-activity-task-started-count-means",
		"the server writes ActivityTaskStarted only when an attempt ends, and dates it from when that attempt began, so a count of zero does not say the body never ran and a count of one is not a second run; the side-effect file's line count is the reading that says how often the body ran")
	harness.Fact("reasoned: whether-the-session-queue-is-one-a-replacement-could-poll",
		fmt.Sprintf("the server dispatched the session's activity to %s, the queue the session's own worker process made for itself, while the replacement worker polls %s, %s and a queue of its own; %d poller(s) had touched %s since the kill when the readings above were taken, against %d on %s",
			afterEnd.hist.activityQueue, sessionQueue, creationQueue,
			sessionPollers.sinceKill, afterEnd.hist.activityQueue, basePollers.sinceKill, sessionQueue))
	harness.Fact("reasoned: what-the-server-put-anywhere-else",
		fmt.Sprintf("the server ended the killed attempt as %s with retry state %s, and the history holds %d ActivityTaskScheduled event(s) for the session activity, the first of them the original dispatch; any further attempt would have gone to the queue those events name, %s, while the queue the replacement worker polls is %s",
			afterEnd.hist.final, afterEnd.hist.retryState, final.hist.scheduled, afterEnd.hist.activityQueue, sessionQueue))
	harness.Fact("reasoned: what-held-the-workflow-open",
		fmt.Sprintf("the workflow waited for the %s signal after probing the session, and the driver sent it only after the replacement worker had polled for %s and then for %s more past the end of the killed attempt, so the run was %s at both readings and %s only once it was released",
			ReleaseSignalName, replacementObserve, afterEndObserve, during.status, final.status))
	harness.Fact("limit",
		fmt.Sprintf("one dev server, one session, one kill and one replacement worker, watched for %s before the killed attempt ended and %s after it: it says what this server did with this session's activity in that window, not what it would do once the session's %s execution timeout expires, and not that Legion's sessions would map onto Temporal's",
			replacementObserve, afterEndObserve, sessionExecution))

	// 12. The verdict. A run that killed nothing, or whose workflow was not running
	// while the replacement polled, measured nothing, whatever else it saw.
	if !killSent {
		return fmt.Errorf("no worker was killed, so this run measured nothing about session failure")
	}
	if before.bodies != 1 {
		return fmt.Errorf("the session activity's body ran %d time(s) before the kill, not once, so the kill did not land on a single running body", before.bodies)
	}
	for _, reading := range []struct {
		when   string
		bodies int
	}{
		{"after the kill", afterKill.bodies},
		{"while the replacement worker polled", during.bodies},
		{"once the killed attempt had ended", afterEnd.bodies},
		{"in total, once the run was over", final.bodies},
	} {
		if reading.bodies == 1 {
			continue
		}
		why := "so the work was run again somewhere rather than left with the worker that died, which is the thing this experiment exists to detect"
		if reading.bodies < 1 {
			why = "so the side-effect file no longer holds the one body run the kill landed on, and the reading this experiment rests on is gone"
		}
		return fmt.Errorf("the session activity's body ran %d time(s) %s, not once, %s", reading.bodies, reading.when, why)
	}
	if !ended {
		return fmt.Errorf("the killed attempt had not ended after %s, so this run never reached the state the claim is about: an activity the server is free to put somewhere else", attemptEndWait)
	}
	for _, reading := range []struct {
		when   string
		status enumspb.WorkflowExecutionStatus
	}{
		{"while the replacement worker polled", during.status},
		{"once the killed attempt had ended", afterEnd.status},
	} {
		if reading.status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			return fmt.Errorf("the workflow was %s rather than %s %s, so this run measured its own ending rather than what Temporal did with the killed activity",
				reading.status, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, reading.when)
		}
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
	harness.Fact("workflow-released-by", orNone(res.ReleasedBy))
	harness.Fact("workflow-release-note", orNone(res.ReleaseNote))
	harness.Fact("timed: workflow-stayed-running-after-the-probe-for", res.HeldOpenFor.Round(time.Millisecond))
}

// measureFreshSession runs a second workflow on the same base task queue, with a short
// hold and a side-effect file of its own, and reports whether a session created afresh
// there succeeds now that the only worker on the queue is the replacement.
func measureFreshSession(
	ctx context.Context,
	c client.Client,
	workDir, workflowQueue string,
	params Params,
	stamp int64,
) error {
	fresh := params
	fresh.Hold = freshSessionHold
	fresh.EffectFile = filepath.Join(workDir, fmt.Sprintf("exp3-fresh-effects-%d", stamp))
	harness.Fact("new-session-side-effect-file", fresh.EffectFile)

	freshRun, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp3-fresh-%d", time.Now().UnixNano()),
		TaskQueue:                workflowQueue,
		WorkflowExecutionTimeout: workflowTimeout,
		WorkflowTaskTimeout:      10 * time.Second,
	}, WorkflowName, fresh)
	if err != nil {
		return fmt.Errorf("start fresh-session workflow: %w", err)
	}
	// This workflow holds itself open too, and there is nothing to watch on it: the
	// signal is sent at once and waits in its history until it reads it.
	if err := c.SignalWorkflow(ctx, freshRun.GetID(), freshRun.GetRunID(), ReleaseSignalName,
		"the fresh session is not watched: nothing is killed under it"); err != nil {
		return fmt.Errorf("send %s to %s: %w", ReleaseSignalName, freshRun.GetID(), err)
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
	bodies, err := exp19.Lines(fresh.EffectFile)
	if err != nil {
		return err
	}
	harness.Fact("new-session-body-executions", bodies)
	return nil
}

// observation is one reading of the killed session's activity, taken at one moment:
// how often its body has run, what DescribeWorkflowExecution holds for it, whether the
// run is still going, and what the whole history says.
type observation struct {
	// bodies is how many lines the side-effect file holds, which is how many times the
	// activity's body ran. It is the reading that says whether the work ran again.
	bodies int
	// pending, state and attempt are what DescribeWorkflowExecution reports about the
	// session activity now.
	pending bool
	state   string
	attempt int32
	// status is the workflow execution's own status at that moment.
	status enumspb.WorkflowExecutionStatus
	// hist is what the run's whole history says about the session activity.
	hist historyFacts
}

// observe takes one reading of all four at once.
func observe(ctx context.Context, c client.Client, run client.WorkflowRun, effectFile string, killedAt time.Time) (observation, error) {
	o := observation{state: "none"}
	bodies, err := exp19.Lines(effectFile)
	if err != nil {
		return o, err
	}
	o.bodies = bodies

	desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		return o, fmt.Errorf("describe %s: %w", run.GetID(), err)
	}
	for _, pending := range desc.GetPendingActivities() {
		if pending.GetActivityType().GetName() != SessionActivityName {
			continue
		}
		o.pending = true
		o.state = pending.GetState().String()
		o.attempt = pending.GetAttempt()
	}
	o.status = desc.GetWorkflowExecutionInfo().GetStatus()

	hist, err := inspectHistory(ctx, c, run.GetID(), run.GetRunID(), killedAt)
	if err != nil {
		return o, err
	}
	o.hist = hist
	return o, nil
}

// report prints one observation under the moment it was taken at.
func (o observation) report(when string) {
	harness.Fact("session-body-executions-"+when, o.bodies)
	harness.Fact("session-activity-pending-"+when, o.pending)
	harness.Fact("session-activity-pending-state-"+when, o.state)
	harness.Fact("session-activity-pending-attempt-"+when, o.attempt)
	harness.Fact("session-activity-task-scheduled-events-"+when, o.hist.scheduled)
	harness.Fact("session-activity-task-started-events-"+when, o.hist.starts)
	harness.Fact("session-activity-last-history-event-"+when, o.hist.final)
	harness.Fact("history-event-count-"+when, o.hist.events)
	harness.Fact("killed-run-status-"+when, o.status)
}

// queuePollers is what the server says about one task queue's activity pollers: a
// queue nothing polls is how a session keeps its activities to one process, so the
// experiment reads the pollers rather than only the queue names.
type queuePollers struct {
	// queue is the task queue these pollers were read from.
	queue string
	// count is every poller the server has heard from lately, which includes a worker
	// that has since been killed: the server keeps a poller it saw for some minutes.
	count int
	// sinceKill is how many of them polled after the kill, which is the number that
	// says whether anything alive is listening to this queue now.
	sinceKill int
	// identities is what each of those pollers calls itself, with when it last polled.
	identities []string
}

// describePollers asks the server who is polling a task queue for activities, and when
// each of them last did.
func describePollers(ctx context.Context, c client.Client, queue string, since time.Time) (queuePollers, error) {
	found := queuePollers{queue: queue}
	if queue == "" || queue == "none" {
		return found, nil
	}
	desc, err := c.DescribeTaskQueue(ctx, queue, enumspb.TASK_QUEUE_TYPE_ACTIVITY)
	if err != nil {
		return found, fmt.Errorf("describe task queue %s: %w", queue, err)
	}
	for _, poller := range desc.GetPollers() {
		last := poller.GetLastAccessTime().AsTime()
		found.count++
		if last.After(since) {
			found.sinceKill++
		}
		found.identities = append(found.identities,
			fmt.Sprintf("%s last-polled-at=%s", poller.GetIdentity(), last.UTC().Format(time.RFC3339Nano)))
	}
	return found, nil
}

// report prints one queue's pollers under the name the experiment calls that queue.
func (p queuePollers) report(label string) {
	harness.Fact(label+"-pollers-while-replacement-polls", p.count)
	harness.Fact(label+"-pollers-since-the-kill", p.sinceKill)
	identities := "none"
	if len(p.identities) > 0 {
		identities = strings.Join(p.identities, "; ")
	}
	harness.Fact(label+"-poller-identities", identities)
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
		if err := sleep(ctx, describePoll); err != nil {
			return err
		}
	}
}

// waitAttemptEnded blocks until the attempt the kill landed on has ended: the server
// holds no pending session activity any more, or it holds one of a later attempt,
// which means it ended the first and started another. It reports whether that
// happened within timeout rather than failing on it, because an attempt the server
// never ends is itself a reading, and the verdict is where it is weighed.
func waitAttemptEnded(ctx context.Context, c client.Client, run client.WorkflowRun, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
		if err != nil {
			return false, fmt.Errorf("describe %s: %w", run.GetID(), err)
		}
		ended := true
		for _, pending := range desc.GetPendingActivities() {
			if pending.GetActivityType().GetName() != SessionActivityName {
				continue
			}
			ended = pending.GetAttempt() > 1
		}
		if ended {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		if err := sleep(ctx, describePoll); err != nil {
			return false, err
		}
	}
}

// historyFacts is what a run's history says about the session activity.
type historyFacts struct {
	// events is every event of the run.
	events int
	// scheduled is how many times the server put the session activity on a task
	// queue. One of them is the original dispatch, so a second would be the server
	// handing the work out again.
	scheduled int
	// starts is the ActivityTaskStarted events the session activity has. The server
	// writes one only when an attempt ends, so an attempt whose worker was killed
	// mid-run leaves none: a start here is an attempt that ran somewhere to an end,
	// and a zero here is no evidence that no body ran.
	starts int
	// startsAfterKill is how many of those starts happened after the kill, which is
	// how the experiment sees the work resume on a worker other than the dead one.
	startsAfterKill int
	// final is the last history event the session activity reached.
	final string
	// retryState is what the server said about the retries when it ended the
	// activity's attempt, which is why it did or did not start another one.
	retryState string
	// activityQueue is the task queue the server dispatched the session activity to:
	// the resource-specific queue the session's own worker process made for itself.
	activityQueue string
	// creationQueue is the task queue the SDK's session creation activity went to.
	creationQueue string
}

// inspectHistory reads a run's whole history and reports what it says about the session
// activity: how often it was dispatched and started, how often it started after
// killedAt, where it ended up and with what retry state, and which task queues the
// server dispatched it and its session's creation activity to.
func inspectHistory(ctx context.Context, c client.Client, workflowID, runID string, killedAt time.Time) (historyFacts, error) {
	facts := historyFacts{final: "none", retryState: "none", activityQueue: "none", creationQueue: "none"}
	scheduledIDs := map[int64]bool{}
	events, err := harness.History(ctx, c, workflowID, runID)
	if err != nil {
		return historyFacts{}, err
	}
	for _, event := range events {
		facts.events++
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			switch attrs.GetActivityType().GetName() {
			case SessionActivityName:
				scheduledIDs[event.GetEventId()] = true
				facts.scheduled++
				facts.activityQueue = attrs.GetTaskQueue().GetName()
				facts.final = eventName(event)
			case SessionCreationActivityName:
				facts.creationQueue = attrs.GetTaskQueue().GetName()
			}
			continue
		}
		id, ok := activityEventScheduledID(event)
		if !ok || !scheduledIDs[id] {
			continue
		}
		facts.final = eventName(event)
		switch attrs := event.GetAttributes().(type) {
		case *historypb.HistoryEvent_ActivityTaskStartedEventAttributes:
			facts.starts++
			if !event.GetEventTime().AsTime().Before(killedAt) {
				facts.startsAfterKill++
			}
		case *historypb.HistoryEvent_ActivityTaskTimedOutEventAttributes:
			facts.retryState = attrs.ActivityTaskTimedOutEventAttributes.GetRetryState().String()
		case *historypb.HistoryEvent_ActivityTaskFailedEventAttributes:
			facts.retryState = attrs.ActivityTaskFailedEventAttributes.GetRetryState().String()
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

// eventName is a history event's type under the shorthand name the protobuf enum's own
// String method gives it, which is the form this experiment's facts are written in.
func eventName(event *historypb.HistoryEvent) string {
	return event.GetEventType().String()
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

// sleep waits for d and reports that the context ended first.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
