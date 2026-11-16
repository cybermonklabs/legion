package exp19

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings. They are short on purpose: three cases, two worker
// processes and a timeout sat through have to finish well inside the runner's bound.
const (
	dialTimeout = 30 * time.Second
	// handoutWindow is the StartToCloseTimeout of the handouts that are meant to be
	// completed from outside: long enough that the kill, the replacement worker and
	// the completion all happen inside it, so nothing those cases see is the timeout.
	handoutWindow = 60 * time.Second
	// boundWindow is the StartToCloseTimeout of the handout nobody completes. It is
	// short so that this run can sit through the bound rather than reason about it.
	boundWindow = 10 * time.Second

	workflowTimeout = 150 * time.Second
	workerStartWait = 40 * time.Second
	bodyWait        = 30 * time.Second
	// settleAfterKill is how long the server is left alone with a dead worker before
	// the activity is looked at again.
	settleAfterKill = 2 * time.Second
	// replacementObserve is how long the replacement worker polls the same task queue
	// before the run reads whether anything was handed to it.
	replacementObserve = 8 * time.Second
	completerWait      = 30 * time.Second
	workflowWait       = 60 * time.Second
	boundWait          = 90 * time.Second
	describePoll       = 200 * time.Millisecond
)

// Run is experiment 19: Temporal's Asynchronous Activity Completion, measured in three
// cases on one server. Case A hands an activity out and kills the worker that took it.
// Case B completes it from a process that holds only its task token. Case C takes two
// readings of the edges: what the server answers to the token of an attempt it has
// moved on from, and what happens to a handout nobody completes.
//
// Every value below is printed as the run observed it, including a value that
// contradicts what the mechanism is described to do.
func Run(ctx context.Context, cfg harness.Config) error {
	if role := os.Getenv(roleEnv); role != "" {
		// This process is one of the helper processes the driver started, not a run
		// of the experiment: it serves or completes and exits on its own, so nothing
		// a helper does reaches the experiment's verdict line.
		os.Exit(helper(ctx, cfg, role))
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

	// One set of queues per run, so a rerun never meets the last run's leftovers.
	stamp := time.Now().UnixNano()
	activityQueue := fmt.Sprintf("exp19-activity-%d", stamp)
	workflowQueue := fmt.Sprintf("exp19-workflow-%d", stamp)

	// The workflow runs inside this process, the activity only on the worker
	// processes this driver starts, so the kill cannot take the workflow with it.
	wfWorker := worker.New(c, workflowQueue, worker.Options{})
	RegisterWorkflow(wfWorker)
	if err := wfWorker.Start(); err != nil {
		return fmt.Errorf("start workflow worker: %w", err)
	}
	defer wfWorker.Stop()

	harness.Fact("mechanism", "Temporal Asynchronous Activity Completion: an activity body that returns activity.ErrResultPending, completed later by whoever holds the task token")
	harness.Fact("sdk-completion-call", "client.CompleteActivity(ctx, taskToken, result, nil)")
	harness.Fact("activity-task-queue", activityQueue)
	harness.Fact("workflow-task-queue", workflowQueue)
	harness.Fact("driver-process-registered-the-handout-activity", false)

	state := &run{cfg: cfg, client: c, workDir: workDir, activityQueue: activityQueue, workflowQueue: workflowQueue, stamp: stamp}
	defer state.stopWorkers()
	if err := state.caseA(ctx); err != nil {
		return err
	}
	if err := state.caseB(ctx); err != nil {
		return err
	}
	if err := state.caseCFence(ctx); err != nil {
		return err
	}
	if err := state.caseCBound(ctx); err != nil {
		return err
	}

	harness.Fact("reasoned: what-the-replacement-window-covers",
		fmt.Sprintf("the replacement worker polled the same task queue for %s inside a %s start-to-close timeout, so the re-dispatch counted above is what the server did in that window, not what it would do once the bound expires",
			replacementObserve, handoutWindow))
	harness.Fact("reasoned: what-a-pod-cannot-do",
		fmt.Sprintf("this run handed work out for seconds and sat through a %s bound; nothing here observed a handout left open for the hours a prompt into a live session can take, and a handout of hours would have to carry a start-to-close timeout of hours for the server to leave it alone that long", boundWindow))
	harness.Fact("limit", "one dev server, one handout per case, two worker processes and seconds rather than hours: it shows what this server did with an activity whose body returned ErrResultPending when its worker died, when another process completed it, when a superseded attempt's token was used and when nobody completed it, not how a cluster behaves, not what a completion storm costs, and not that Legion's sessions would map onto this mechanism")

	return state.verdict()
}

// run is one execution of the experiment: the three cases share the server, the
// queues and the scratch directory, and each records what it measured for the verdict.
type run struct {
	cfg           harness.Config
	client        client.Client
	workDir       string
	activityQueue string
	workflowQueue string
	stamp         int64

	// Case A's handout, which case B completes from another process.
	handoutParams  Params
	handout        client.WorkflowRun
	handoutStarted time.Time

	// What the verdict looks at: whether each case could take its reading at all.
	killSent        bool
	bodyBeforeKill  int
	stateBeforeKill string
	completionTried bool
	fenceBodies     int
	staleTokenTried bool
	boundReturned   bool

	// The replacement worker of case A, which stays for the cases after it.
	worker2 *Child
}

// caseA hands an activity out, kills the worker process that took it, and starts a
// replacement worker on the same task queue: the question is whether the server hands
// the same work to the replacement, and how often the body runs.
func (r *run) caseA(ctx context.Context) error {
	harness.Fact("case-a", "the handout that is not re-run: the worker that took the activity is killed with SIGKILL and replaced while the activity is still started")

	params := Params{
		TaskQueue:    r.activityQueue,
		EffectFile:   r.file("handout-effects"),
		TokenFile:    r.file("handout-tokens"),
		StartToClose: handoutWindow,
		MaxAttempts:  1,
	}
	r.handoutParams = params
	harness.Fact("handout-side-effect-file", params.EffectFile)
	harness.Fact("handout-token-file", params.TokenFile)
	harness.Fact("handout-start-to-close-timeout", params.StartToClose)
	harness.Fact("handout-maximum-attempts", params.MaxAttempts)

	// 1. The worker that will be killed.
	first, err := StartWorkerChild(ctx, r.cfg, "worker1", r.workDir, r.activityQueue, workerStartWait)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("worker1-pid", first.PID)

	// 2. The workflow, whose one activity is dispatched to that worker's queue.
	handout, err := r.start(ctx, "handout", params, workflowTimeout)
	if err != nil {
		return err
	}
	r.handout = handout
	harness.Fact("handout-workflow-id", handout.GetID())
	harness.Fact("handout-run-id", handout.GetRunID())

	// 3. Wait until the body has run and handed its token out.
	lines, err := WaitForLines(ctx, params.EffectFile, 1, bodyWait)
	if err != nil {
		return fmt.Errorf("the handout activity's body never ran: %w", err)
	}
	r.handoutStarted = time.Now()
	harness.Fact("handout-first-body-line", lines[0])
	tokens, err := Tokens(params.TokenFile)
	if err != nil {
		return err
	}
	harness.Fact("handout-tokens-written-before-kill", len(tokens))

	before, status, err := WaitActivityStarted(ctx, r.client, handout, bodyWait)
	if err != nil {
		return err
	}
	r.bodyBeforeKill = len(lines)
	r.stateBeforeKill = before.State
	harness.Fact("handout-body-executions-before-kill", r.bodyBeforeKill)
	harness.Fact("handout-activity-pending-before-kill", before.Present)
	harness.Fact("handout-activity-state-before-kill", before.State)
	harness.Fact("handout-activity-attempt-before-kill", before.Attempt)
	harness.Fact("handout-workflow-status-before-kill", status)

	// 4. The kill itself, by the exact pid the driver recorded.
	if !first.Alive() {
		harness.Fact("kill-sent", false)
		harness.Fact("worker1-already-gone", true)
		return fmt.Errorf("worker1 was gone before the kill, so this run measured nothing about a worker dying on a handed-out activity")
	}
	if err := first.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	r.killSent = true
	harness.Fact("kill-sent", true)
	harness.Fact("killed-pid", first.PID)
	harness.Fact("kill-signal", "SIGKILL")
	harness.Fact("worker1-alive-after-kill", first.Alive())

	if err := sleep(ctx, settleAfterKill); err != nil {
		return err
	}
	afterKill, status, err := DescribeActivity(ctx, r.client, handout)
	if err != nil {
		return err
	}
	bodyAfterKill, err := Lines(params.EffectFile)
	if err != nil {
		return err
	}
	harness.Fact("timed: handout-watched-after-the-kill-for", settleAfterKill)
	harness.Fact("handout-activity-pending-after-kill", afterKill.Present)
	harness.Fact("handout-activity-state-after-kill", afterKill.State)
	harness.Fact("handout-activity-attempt-after-kill", afterKill.Attempt)
	harness.Fact("handout-body-executions-after-kill", bodyAfterKill)
	harness.Fact("handout-workflow-status-after-kill", status)

	// 5. The replacement worker: a worker that could run the body again, if the
	// server offered it the activity.
	second, err := StartWorkerChild(ctx, r.cfg, "worker2", r.workDir, r.activityQueue, workerStartWait)
	if err != nil {
		return err
	}
	r.worker2 = second
	harness.Fact("worker2-pid", second.PID)
	if err := sleep(ctx, replacementObserve); err != nil {
		return err
	}

	afterSecond, status, err := DescribeActivity(ctx, r.client, handout)
	if err != nil {
		return err
	}
	hist, err := ReadHistory(ctx, r.client, handout)
	if err != nil {
		return err
	}
	bodyAfterSecond, err := Lines(params.EffectFile)
	if err != nil {
		return err
	}
	tokens, err = Tokens(params.TokenFile)
	if err != nil {
		return err
	}
	harness.Fact("timed: handout-replacement-worker-polled-the-same-queue-for", replacementObserve)
	harness.Fact("handout-activity-pending-after-replacement-worker", afterSecond.Present)
	harness.Fact("handout-activity-state-after-replacement-worker", afterSecond.State)
	harness.Fact("handout-activity-attempt-after-replacement-worker", afterSecond.Attempt)
	harness.Fact("handout-activity-task-started-events-after-replacement-worker", hist.Starts)
	harness.Fact("handout-activity-task-scheduled-events-after-replacement-worker", hist.Scheduled)
	harness.Fact("handout-body-executions-after-replacement-worker", bodyAfterSecond)
	harness.Fact("handout-tokens-written-after-replacement-worker", len(tokens))
	harness.Fact("handout-last-activity-history-event-after-replacement-worker", hist.Final)
	harness.Fact("handout-history-event-count-after-replacement-worker", hist.Events)
	harness.Fact("handout-workflow-status-after-replacement-worker", status)
	return nil
}

// caseB completes that same handed-out activity from a process holding only its task
// token, and reads what the workflow then received.
func (r *run) caseB(ctx context.Context) error {
	harness.Fact("case-b", "the completion from another process: a process that runs no worker and registers no activity calls client.CompleteActivity with the task token")

	token, err := TokenForAttempt(r.handoutParams.TokenFile, 1)
	if err != nil {
		return err
	}
	result := fmt.Sprintf("exp19-outside-answer-%d", r.stamp)
	harness.Fact("completion-token-bytes", len(token))
	harness.Fact("completion-result-sent", result)

	answer, err := CompleteFromAnotherProcess(ctx, r.cfg, r.workDir, r.file("handout-answer.json"), token, result, completerWait)
	if err != nil {
		return err
	}
	r.completionTried = true
	harness.Fact("timed: handout-stayed-handed-out-until-the-completion-call-for", time.Since(r.handoutStarted).Round(time.Millisecond))
	harness.Fact("completion-called-by-pid", answer.PID)
	harness.Fact("completion-caller-ran-a-worker", false)
	harness.Fact("completion-caller-registered-the-activity", false)
	harness.Fact("completion-accepted-by-the-server", answer.Accepted)
	harness.Fact("completion-error-type", orNone(answer.ErrorType))
	harness.Fact("completion-error-message", orNone(answer.ErrorMessage))

	res, returned, waitErr := r.wait(ctx, r.handout, workflowWait)
	harness.Fact("handout-workflow-returned", returned)
	if !returned {
		harness.Fact("handout-workflow-wait-error", waitErr)
	} else {
		harness.Fact("handout-workflow-received-result", orNone(res.Completion))
		harness.Fact("handout-workflow-activity-error-type", orNone(res.ErrorType))
		harness.Fact("handout-workflow-activity-error", orNone(res.ErrorText))
	}

	bodies, err := Lines(r.handoutParams.EffectFile)
	if err != nil {
		return err
	}
	hist, err := ReadHistory(ctx, r.client, r.handout)
	if err != nil {
		return err
	}
	_, status, err := DescribeActivity(ctx, r.client, r.handout)
	if err != nil {
		return err
	}
	harness.Fact("handout-body-executions-total", bodies)
	harness.Fact("handout-activity-task-started-events-total", hist.Starts)
	harness.Fact("handout-activity-task-completed-events", hist.Completed)
	harness.Fact("handout-activity-task-failed-events", hist.Failed)
	harness.Fact("handout-activity-task-timed-out-events", hist.TimedOut)
	harness.Fact("handout-last-activity-history-event", hist.Final)
	harness.Fact("handout-history-event-count", hist.Events)
	harness.Fact("handout-workflow-status", status)
	return nil
}

// caseCFence runs a handout whose first attempt records its token and then fails, so a
// second attempt runs, and tries the completion with the first attempt's token: the
// question is what the server answers to a token it has moved on from. The current
// attempt's token is then used too, as the control that says the refusal was about the
// attempt and not about the token being unusable at all.
func (r *run) caseCFence(ctx context.Context) error {
	harness.Fact("case-c-fence", "the token of a superseded attempt: attempt 1 records its token and fails, attempt 2 runs, and the completion is tried with attempt 1's token")

	params := Params{
		TaskQueue:        r.activityQueue,
		EffectFile:       r.file("fence-effects"),
		TokenFile:        r.file("fence-tokens"),
		StartToClose:     handoutWindow,
		MaxAttempts:      2,
		FailFirstAttempt: true,
	}
	harness.Fact("fence-side-effect-file", params.EffectFile)
	harness.Fact("fence-maximum-attempts", params.MaxAttempts)

	fence, err := r.start(ctx, "fence", params, workflowTimeout)
	if err != nil {
		return err
	}
	harness.Fact("fence-workflow-id", fence.GetID())

	// Both attempts have run once the body has written two lines: attempt 1 before it
	// failed, attempt 2 before it handed its token out.
	lines, err := WaitForLines(ctx, params.EffectFile, 2, bodyWait)
	if err != nil {
		return fmt.Errorf("the fence's second attempt never ran: %w", err)
	}
	r.fenceBodies = len(lines)
	for i, line := range lines {
		harness.Fact(fmt.Sprintf("fence-body-line-%d", i+1), line)
	}
	tokens, err := Tokens(params.TokenFile)
	if err != nil {
		return err
	}
	attempts := make([]int32, 0, len(tokens))
	for _, token := range tokens {
		attempts = append(attempts, token.Attempt)
	}
	harness.Fact("fence-body-executions", r.fenceBodies)
	harness.Fact("fence-attempts-that-recorded-a-token", attempts)

	pendingNow, _, err := WaitActivityStarted(ctx, r.client, fence, bodyWait)
	if err != nil {
		return err
	}
	harness.Fact("fence-activity-state-while-attempt-2-is-pending", pendingNow.State)
	harness.Fact("fence-activity-attempt-reported-by-the-server", pendingNow.Attempt)

	stale, err := TokenForAttempt(params.TokenFile, 1)
	if err != nil {
		return err
	}
	staleAnswer, err := CompleteFromAnotherProcess(ctx, r.cfg, r.workDir, r.file("fence-stale-answer.json"), stale,
		fmt.Sprintf("exp19-stale-attempt-answer-%d", r.stamp), completerWait)
	if err != nil {
		return err
	}
	r.staleTokenTried = true
	harness.Fact("fence-stale-token-completion-accepted", staleAnswer.Accepted)
	harness.Fact("fence-stale-token-completion-error-type", orNone(staleAnswer.ErrorType))
	harness.Fact("fence-stale-token-completion-error-message", orNone(staleAnswer.ErrorMessage))

	current, err := TokenForAttempt(params.TokenFile, 2)
	if err != nil {
		return err
	}
	currentResult := fmt.Sprintf("exp19-current-attempt-answer-%d", r.stamp)
	currentAnswer, err := CompleteFromAnotherProcess(ctx, r.cfg, r.workDir, r.file("fence-current-answer.json"), current, currentResult, completerWait)
	if err != nil {
		return err
	}
	harness.Fact("fence-current-token-completion-accepted", currentAnswer.Accepted)
	harness.Fact("fence-current-token-completion-error-type", orNone(currentAnswer.ErrorType))
	harness.Fact("fence-current-token-completion-error-message", orNone(currentAnswer.ErrorMessage))

	res, returned, waitErr := r.wait(ctx, fence, workflowWait)
	harness.Fact("fence-workflow-returned", returned)
	if !returned {
		harness.Fact("fence-workflow-wait-error", waitErr)
	} else {
		harness.Fact("fence-workflow-received-result", orNone(res.Completion))
		harness.Fact("fence-workflow-activity-error", orNone(res.ErrorText))
	}
	bodies, err := Lines(params.EffectFile)
	if err != nil {
		return err
	}
	hist, err := ReadHistory(ctx, r.client, fence)
	if err != nil {
		return err
	}
	_, status, err := DescribeActivity(ctx, r.client, fence)
	if err != nil {
		return err
	}
	harness.Fact("fence-body-executions-total", bodies)
	harness.Fact("fence-activity-task-started-events", hist.Starts)
	harness.Fact("fence-activity-task-failed-events", hist.Failed)
	harness.Fact("fence-activity-task-completed-events", hist.Completed)
	harness.Fact("fence-last-activity-history-event", hist.Final)
	harness.Fact("fence-workflow-status", status)
	return nil
}

// caseCBound runs the same handout with a short start-to-close timeout and completes
// it from nowhere: the question is what the workflow is told when the bound expires,
// and whether the body runs again.
func (r *run) caseCBound(ctx context.Context) error {
	harness.Fact("case-c-bound", "the bound: the same handout with a short start-to-close timeout and nobody completing it")

	params := Params{
		TaskQueue:    r.activityQueue,
		EffectFile:   r.file("bound-effects"),
		TokenFile:    r.file("bound-tokens"),
		StartToClose: boundWindow,
		MaxAttempts:  1,
	}
	harness.Fact("bound-side-effect-file", params.EffectFile)
	harness.Fact("bound-start-to-close-timeout", params.StartToClose)
	harness.Fact("bound-maximum-attempts", params.MaxAttempts)

	bound, err := r.start(ctx, "bound", params, workflowTimeout)
	if err != nil {
		return err
	}
	harness.Fact("bound-workflow-id", bound.GetID())

	lines, err := WaitForLines(ctx, params.EffectFile, 1, bodyWait)
	if err != nil {
		return fmt.Errorf("the bounded handout's body never ran: %w", err)
	}
	handedOut := time.Now()
	harness.Fact("bound-first-body-line", lines[0])

	res, returned, waitErr := r.wait(ctx, bound, boundWait)
	r.boundReturned = returned
	harness.Fact("timed: bound-workflow-waited-for", time.Since(handedOut).Round(time.Millisecond))
	harness.Fact("bound-workflow-returned", returned)
	if !returned {
		harness.Fact("bound-workflow-wait-error", waitErr)
	} else {
		harness.Fact("bound-workflow-received-result", orNone(res.Completion))
		harness.Fact("bound-workflow-activity-error-type", orNone(res.ErrorType))
		harness.Fact("bound-workflow-activity-error-is-timeout", res.IsTimeout)
		harness.Fact("bound-workflow-activity-timeout-type", orNone(res.TimeoutType))
		harness.Fact("bound-workflow-activity-retry-state", orNone(res.RetryState))
		harness.Fact("bound-workflow-activity-error", orNone(res.ErrorText))
	}

	bodies, err := Lines(params.EffectFile)
	if err != nil {
		return err
	}
	tokens, err := Tokens(params.TokenFile)
	if err != nil {
		return err
	}
	hist, err := ReadHistory(ctx, r.client, bound)
	if err != nil {
		return err
	}
	_, status, err := DescribeActivity(ctx, r.client, bound)
	if err != nil {
		return err
	}
	harness.Fact("bound-body-executions-total", bodies)
	harness.Fact("bound-tokens-written-total", len(tokens))
	harness.Fact("bound-activity-task-started-events", hist.Starts)
	harness.Fact("bound-activity-task-scheduled-events", hist.Scheduled)
	harness.Fact("bound-activity-task-timed-out-events", hist.TimedOut)
	harness.Fact("bound-last-activity-history-event", hist.Final)
	harness.Fact("bound-workflow-status", status)
	return nil
}

// stopWorkers takes down the worker process the cases after case A share. The worker
// that was killed is reaped by the case that killed it.
func (r *run) stopWorkers() {
	if r.worker2 != nil {
		r.worker2.Stop()
	}
}

// verdict fails the run that could not take one of its readings, and passes every run
// that took them all -- whatever they say. What the server did is the measurement, so
// no reading here is required to come out one way.
func (r *run) verdict() error {
	switch {
	case !r.killSent:
		return fmt.Errorf("no worker was killed, so this run measured nothing about a handed-out activity whose worker died")
	case r.bodyBeforeKill != 1:
		return fmt.Errorf("the handout's body ran %d time(s) before the kill, not once, so the kill did not land on a single handed-out activity", r.bodyBeforeKill)
	case r.stateBeforeKill != enumspb.PENDING_ACTIVITY_STATE_STARTED.String():
		return fmt.Errorf("the handed-out activity was %s rather than %s when the kill was sent, so the kill did not land on a started activity",
			r.stateBeforeKill, enumspb.PENDING_ACTIVITY_STATE_STARTED)
	case !r.completionTried:
		return fmt.Errorf("no process completed the activity from its token, so this run measured nothing about completion from outside")
	case r.fenceBodies < 2:
		return fmt.Errorf("the fence's second attempt did not run (%d body execution(s)), so the token the stale-token completion used was not stale", r.fenceBodies)
	case !r.staleTokenTried:
		return fmt.Errorf("the completion with the superseded attempt's token was never tried, so this run measured nothing about it")
	case !r.boundReturned:
		return fmt.Errorf("the bounded handout's workflow did not return within %s, so this run measured nothing about what the bound does", boundWait)
	}
	return nil
}

// file is one of the run's scratch files, named so that two runs never share one.
func (r *run) file(name string) string {
	return filepath.Join(r.workDir, fmt.Sprintf("exp19-%s-%d", name, r.stamp))
}

// start runs one handout workflow.
func (r *run) start(ctx context.Context, name string, p Params, timeout time.Duration) (client.WorkflowRun, error) {
	run, err := r.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp19-%s-%d", name, r.stamp),
		TaskQueue:                r.workflowQueue,
		WorkflowExecutionTimeout: timeout,
		WorkflowTaskTimeout:      10 * time.Second,
	}, WorkflowName, p)
	if err != nil {
		return nil, fmt.Errorf("start the %s workflow: %w", name, err)
	}
	return run, nil
}

// wait reads what a workflow returned, and reports a workflow that did not return
// instead of failing the run: a handout nobody could complete is a reading too.
func (r *run) wait(ctx context.Context, run client.WorkflowRun, timeout time.Duration) (Result, bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var res Result
	if err := run.Get(waitCtx, &res); err != nil {
		return Result{}, false, err
	}
	return res, true, nil
}

// PendingActivity is what DescribeWorkflowExecution says about the handout activity.
type PendingActivity struct {
	Present bool
	State   string
	Attempt int32
}

// DescribeActivity asks the server what it holds for the run's handout activity now,
// and what the workflow's own status is.
func DescribeActivity(ctx context.Context, c client.Client, run client.WorkflowRun) (PendingActivity, enumspb.WorkflowExecutionStatus, error) {
	desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		return PendingActivity{}, enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, fmt.Errorf("describe %s: %w", run.GetID(), err)
	}
	found := PendingActivity{State: "none"}
	for _, info := range desc.GetPendingActivities() {
		if info.GetActivityType().GetName() != HandoutActivityName {
			continue
		}
		found.Present = true
		found.State = info.GetState().String()
		found.Attempt = info.GetAttempt()
	}
	return found, desc.GetWorkflowExecutionInfo().GetStatus(), nil
}

// WaitActivityStarted blocks until the server holds the handout activity as started,
// which is the state a handed-out activity sits in while the outside world has it.
func WaitActivityStarted(ctx context.Context, c client.Client, run client.WorkflowRun, timeout time.Duration) (PendingActivity, enumspb.WorkflowExecutionStatus, error) {
	deadline := time.Now().Add(timeout)
	for {
		found, status, err := DescribeActivity(ctx, c, run)
		if err != nil {
			return found, status, err
		}
		if found.Present && found.State == enumspb.PENDING_ACTIVITY_STATE_STARTED.String() {
			return found, status, nil
		}
		if !time.Now().Before(deadline) {
			return found, status, nil
		}
		if err := sleep(ctx, describePoll); err != nil {
			return found, status, err
		}
	}
}

// ActivityFacts is what a run's whole history says about its one activity. Each of
// this experiment's workflows schedules exactly one, so every ACTIVITY_TASK_ event in
// the history belongs to it, and so does every one of experiment 20's.
type ActivityFacts struct {
	Events    int
	Scheduled int
	Starts    int
	Completed int
	Failed    int
	TimedOut  int
	Final     string
}

// ReadHistory reads a run's whole history from the server and counts what it says.
func ReadHistory(ctx context.Context, c client.Client, run client.WorkflowRun) (ActivityFacts, error) {
	events, err := harness.History(ctx, c, run.GetID(), run.GetRunID())
	if err != nil {
		return ActivityFacts{}, err
	}
	facts := ActivityFacts{Final: "none"}
	for _, event := range events {
		facts.Events++
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			facts.Scheduled++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
			facts.Starts++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			facts.Completed++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			facts.Failed++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
			facts.TimedOut++
		default:
			continue
		}
		facts.Final = harness.EventName(event)
	}
	return facts, nil
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
