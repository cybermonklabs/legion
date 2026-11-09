package exp20

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp19"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// Run is experiment 20: one handout, the worker that took it killed, five minutes of
// real time with nobody at all, and then the completion from a process holding only
// the task token.
//
// Every value below is printed as the run observed it, including a value that
// contradicts what the mechanism is described to do: a poll that found the activity
// reaped, re-dispatched or run a second time is the finding, not a failure.
func Run(ctx context.Context, cfg harness.Config) error {
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
	activityQueue := fmt.Sprintf("exp20-activity-%d", stamp)
	workflowQueue := fmt.Sprintf("exp20-workflow-%d", stamp)

	// The workflow runs inside this process and the activity only in the worker
	// process this driver starts, so the kill cannot take the workflow with it.
	wfWorker := worker.New(c, workflowQueue, worker.Options{})
	exp19.RegisterWorkflow(wfWorker)
	if err := wfWorker.Start(); err != nil {
		return fmt.Errorf("start workflow worker: %w", err)
	}
	defer wfWorker.Stop()

	harness.Fact("mechanism", "Temporal Asynchronous Activity Completion: an activity body that returns activity.ErrResultPending, completed later by whoever holds the task token")
	harness.Fact("sdk-completion-call", "client.CompleteActivity(ctx, taskToken, result, nil)")
	harness.Fact("case", fmt.Sprintf("the handout held open for minutes: the worker that took the activity is killed with SIGKILL, the activity is then left for %s of real time with no worker alive and nothing touching it, and only then does a process holding its task token complete it", holdWindow))
	harness.Fact("workflow-and-activity", "experiment 19's, imported rather than repeated: the same handout workflow, the same activity, the same worker and completer processes")
	harness.Fact("activity-task-queue", activityQueue)
	harness.Fact("workflow-task-queue", workflowQueue)
	harness.Fact("driver-process-registered-the-handout-activity", false)

	state := &run{cfg: cfg, client: c, workDir: workDir, activityQueue: activityQueue, workflowQueue: workflowQueue, stamp: stamp}
	if err := state.handOut(ctx); err != nil {
		return err
	}
	if err := state.hold(ctx); err != nil {
		return err
	}
	if err := state.complete(ctx); err != nil {
		return err
	}

	harness.Fact("reasoned: what-a-handout-of-hours-would-need",
		fmt.Sprintf("%s, and the start-to-close timeout is the only bound the server was told to enforce on it, so what a handout of hours needs is a start-to-close timeout of hours rather than anything else: nothing here kept the activity alive and nothing here would have had to be asked for more time",
			state.heldSentence()))
	harness.Fact("limit",
		fmt.Sprintf("one dev server on one pod, one handout, %s of real time and a %s bound this run never reached: it shows what this server did with a started activity whose worker was dead for five minutes, not what it does over the hours a prompt into a live session can take, not what happens when a start-to-close timeout of hours expires, not what a restarted or clustered server would hold, and not that Legion's sessions would map onto this mechanism",
			holdWindow, handoutWindow))

	return state.verdict()
}

// run is one execution of the experiment: the handout, the hold and the completion
// share the server, the queues and the scratch directory, and each records what it
// measured for the verdict.
type run struct {
	cfg           harness.Config
	client        client.Client
	workDir       string
	activityQueue string
	workflowQueue string
	stamp         int64

	params  exp19.Params
	handout client.WorkflowRun

	// bodyRanAt is the instant the activity's own line says its body ran, which is
	// when the handout began.
	bodyRanAt time.Time
	// heldFrom is the instant the worker that took the activity was killed, from
	// which nothing at all held the handout.
	heldFrom time.Time

	// What the verdict looks at: whether each reading could be taken at all.
	bodyBeforeKill  int
	stateBeforeKill string
	killSent        bool
	polls           []poll
	heldFor         time.Duration
	completionTried bool
	openFor         time.Duration
}

// handOut starts the handout and kills the worker process that took it. It is
// experiment 19's case A up to the kill, and it is here only to put the server in the
// state the hold then measures.
func (r *run) handOut(ctx context.Context) error {
	r.params = exp19.Params{
		TaskQueue:    r.activityQueue,
		EffectFile:   r.file("handout-effects"),
		TokenFile:    r.file("handout-tokens"),
		StartToClose: handoutWindow,
		MaxAttempts:  1,
	}
	harness.Fact("handout-side-effect-file", r.params.EffectFile)
	harness.Fact("handout-token-file", r.params.TokenFile)
	harness.Fact("handout-start-to-close-timeout", r.params.StartToClose)
	harness.Fact("handout-heartbeat-timeout", "none: the activity is scheduled without one, so no heartbeat of this handout can expire")
	harness.Fact("handout-maximum-attempts", r.params.MaxAttempts)

	// 1. The worker that will be killed, in its own process.
	first, err := exp19.StartWorkerChild(ctx, r.cfg, "worker1", r.workDir, r.activityQueue, workerStartWait)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("worker1-pid", first.PID)

	// 2. The workflow, whose one activity is dispatched to that worker's queue.
	handout, err := r.start(ctx)
	if err != nil {
		return err
	}
	r.handout = handout
	harness.Fact("handout-workflow-id", handout.GetID())
	harness.Fact("handout-run-id", handout.GetRunID())

	// 3. Wait until the body has run and handed its token out.
	lines, err := exp19.WaitForLines(ctx, r.params.EffectFile, 1, bodyWait)
	if err != nil {
		return fmt.Errorf("the handout activity's body never ran: %w", err)
	}
	ranAt, err := bodyRanAt(lines[0])
	if err != nil {
		return err
	}
	r.bodyRanAt = ranAt
	r.bodyBeforeKill = len(lines)
	harness.Fact("handout-first-body-line", lines[0])
	tokens, err := exp19.Tokens(r.params.TokenFile)
	if err != nil {
		return err
	}
	harness.Fact("handout-tokens-written-before-kill", len(tokens))

	before, status, err := exp19.WaitActivityStarted(ctx, r.client, handout, bodyWait)
	if err != nil {
		return err
	}
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
		return fmt.Errorf("worker1 was gone before the kill, so this run measured nothing about a handout nobody holds")
	}
	if err := first.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	r.heldFrom = time.Now()
	r.killSent = true
	harness.Fact("kill-sent", true)
	harness.Fact("killed-pid", first.PID)
	harness.Fact("kill-signal", "SIGKILL")
	harness.Fact("worker1-alive-after-kill", first.Alive())
	return nil
}

// hold leaves the handed-out activity alone for the whole hold window and asks the
// server what it holds every pollInterval. Nothing in this process touches the
// activity while it runs: no worker polls its task queue, nothing heartbeats it and
// nothing completes it, so whatever the series shows is the server's own doing.
func (r *run) hold(ctx context.Context) error {
	harness.Fact("hold-window", holdWindow)
	harness.Fact("hold-poll-interval", pollInterval)
	harness.Fact("workers-polling-the-activity-task-queue-during-the-hold", 0)
	harness.Fact("processes-touching-the-handout-during-the-hold", "none: it is neither heartbeated, nor cancelled, nor completed, and no worker is alive to be offered it")

	// Poll 00 is the state the kill left behind, taken in the same shape as the rest
	// of the series so the series can be read against it.
	atKill, err := r.readPoll(ctx, 0)
	if err != nil {
		return err
	}
	r.polls = append(r.polls, atKill)
	harness.Fact(fmt.Sprintf("handout-poll-%02d", atKill.n), atKill.line())

	for n := 1; n <= pollsPerHold; n++ {
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
		p, err := r.readPoll(ctx, n)
		if err != nil {
			return err
		}
		r.polls = append(r.polls, p)
		harness.Fact(fmt.Sprintf("handout-poll-%02d", p.n), p.line())
	}
	r.heldFor = time.Since(r.heldFrom)

	differing, what := changedAcrossTheHold(r.polls)
	harness.Fact("timed: handout-held-with-no-worker-alive-for", r.heldFor.Round(time.Second))
	harness.Fact("handout-polls-taken", len(r.polls))
	harness.Fact("handout-polls-that-differ-from-poll-00", differing)
	harness.Fact("handout-what-changed-across-the-hold", what)
	harness.Fact("handout-what-reaped-or-re-dispatched-it-during-the-hold", disturbed(r.polls))
	return nil
}

// complete completes the handout from a process holding only its task token, after
// the five minutes, and reads what the server answered and what the workflow then
// received.
func (r *run) complete(ctx context.Context) error {
	token, err := exp19.TokenForAttempt(r.params.TokenFile, 1)
	if err != nil {
		return err
	}
	result := fmt.Sprintf("exp20-outside-answer-%d", r.stamp)
	harness.Fact("completion-token-bytes", len(token))
	harness.Fact("completion-result-sent", result)

	// The call itself happens inside the process started here, so the handout was
	// certainly open for at least this long with nobody completing it.
	r.openFor = time.Since(r.bodyRanAt)
	harness.Fact("timed: handout-stayed-handed-out-until-the-completion-call-for", r.openFor.Round(time.Second))

	answer, err := exp19.CompleteFromAnotherProcess(ctx, r.cfg, r.workDir, r.file("handout-answer.json"), token, result, completerWait)
	if err != nil {
		return err
	}
	r.completionTried = true
	harness.Fact("completion-called-by-pid", answer.PID)
	harness.Fact("completion-caller-ran-a-worker", false)
	harness.Fact("completion-caller-registered-the-activity", false)
	harness.Fact("completion-accepted-by-the-server", answer.Accepted)
	harness.Fact("completion-error-type", orNone(answer.ErrorType))
	harness.Fact("completion-error-message", orNone(answer.ErrorMessage))

	res, returned, waitErr := r.wait(ctx, workflowWait)
	harness.Fact("handout-workflow-returned", returned)
	if !returned {
		harness.Fact("handout-workflow-wait-error", waitErr)
	} else {
		harness.Fact("handout-workflow-received-result", orNone(res.Completion))
		harness.Fact("handout-workflow-activity-error-type", orNone(res.ErrorType))
		harness.Fact("handout-workflow-activity-error-is-timeout", res.IsTimeout)
		harness.Fact("handout-workflow-activity-timeout-type", orNone(res.TimeoutType))
		harness.Fact("handout-workflow-activity-error", orNone(res.ErrorText))
	}

	bodies, err := exp19.Lines(r.params.EffectFile)
	if err != nil {
		return err
	}
	tokens, err := exp19.Tokens(r.params.TokenFile)
	if err != nil {
		return err
	}
	hist, err := exp19.ReadHistory(ctx, r.client, r.handout)
	if err != nil {
		return err
	}
	_, status, err := exp19.DescribeActivity(ctx, r.client, r.handout)
	if err != nil {
		return err
	}
	harness.Fact("handout-body-executions-total", bodies)
	harness.Fact("handout-tokens-written-total", len(tokens))
	harness.Fact("handout-activity-task-scheduled-events", hist.Scheduled)
	harness.Fact("handout-activity-task-started-events-while-it-was-open", startsAcrossTheHold(r.polls))
	harness.Fact("handout-activity-task-started-events-total", hist.Starts)
	harness.Fact("handout-activity-task-completed-events", hist.Completed)
	harness.Fact("handout-activity-task-failed-events", hist.Failed)
	harness.Fact("handout-activity-task-timed-out-events", hist.TimedOut)
	harness.Fact("handout-last-activity-history-event", hist.Final)
	harness.Fact("handout-history-event-count", hist.Events)
	harness.Fact("handout-workflow-status", status)
	return nil
}

// verdict fails the run that could not take one of its readings, and passes every run
// that took them all -- whatever they say. What the server did with the handout is the
// measurement, so no reading here is required to come out one way; a refused
// completion or a reaped activity is a finding this run reports and passes with.
func (r *run) verdict() error {
	switch {
	case !r.killSent:
		return fmt.Errorf("no worker was killed, so this run measured nothing about a handout nobody holds")
	case r.bodyBeforeKill != 1:
		return fmt.Errorf("the handout's body ran %d time(s) before the kill, not once, so the kill did not land on a single handed-out activity", r.bodyBeforeKill)
	case r.stateBeforeKill != enumspb.PENDING_ACTIVITY_STATE_STARTED.String():
		return fmt.Errorf("the handed-out activity was %s rather than %s when the kill was sent, so the kill did not land on a started activity",
			r.stateBeforeKill, enumspb.PENDING_ACTIVITY_STATE_STARTED)
	case len(r.polls) != pollsPerHold+1:
		return fmt.Errorf("the hold took %d reading(s), not the %d it is made of, so the series printed above is not the five minutes it claims", len(r.polls), pollsPerHold+1)
	case r.heldFor < holdWindow:
		return fmt.Errorf("the handout was held with no worker alive for %s, less than the %s this experiment exists to measure, so this run says nothing a run of seconds did not",
			r.heldFor.Round(time.Second), holdWindow)
	case !r.completionTried:
		return fmt.Errorf("no process completed the activity from its token after the hold, so this run measured nothing about what the handout was still worth")
	}
	return nil
}

// heldSentence is what this run observed, in one clause, for the reasoned line to
// stand on: how long the handout was left with nobody at all, and what -- if anything
// -- the server did to it in that time.
func (r *run) heldSentence() string {
	what := disturbed(r.polls)
	if what == nothingHappened {
		return fmt.Sprintf("this run left one handed-out activity for %s with no worker alive and nothing touching it, and across %d readings the server neither reaped it, nor re-dispatched it, nor ran its body again, inside a %s start-to-close timeout and with no heartbeat timeout set",
			r.heldFor.Round(time.Second), len(r.polls), handoutWindow)
	}
	return fmt.Sprintf("this run left one handed-out activity for %s with no worker alive and nothing touching it, and the server did not leave it alone: %s",
		r.heldFor.Round(time.Second), what)
}

// readPoll takes one reading of the handout: what the server holds for the activity,
// what its history has recorded, and how many times its body has run.
func (r *run) readPoll(ctx context.Context, n int) (poll, error) {
	found, _, err := exp19.DescribeActivity(ctx, r.client, r.handout)
	if err != nil {
		return poll{}, err
	}
	hist, err := exp19.ReadHistory(ctx, r.client, r.handout)
	if err != nil {
		return poll{}, err
	}
	lines, err := exp19.Lines(r.params.EffectFile)
	if err != nil {
		return poll{}, err
	}
	return poll{
		n:       n,
		elapsed: time.Since(r.heldFrom),
		pending: found.Present,
		state:   found.State,
		attempt: found.Attempt,
		starts:  hist.Starts,
		lines:   lines,
	}, nil
}

// file is one of the run's scratch files, named so that two runs never share one.
func (r *run) file(name string) string {
	return filepath.Join(r.workDir, fmt.Sprintf("exp20-%s-%d", name, r.stamp))
}

// start runs the handout workflow, which is experiment 19's.
func (r *run) start(ctx context.Context) (client.WorkflowRun, error) {
	run, err := r.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp20-handout-%d", r.stamp),
		TaskQueue:                r.workflowQueue,
		WorkflowExecutionTimeout: workflowTimeout,
		WorkflowTaskTimeout:      10 * time.Second,
	}, exp19.WorkflowName, r.params)
	if err != nil {
		return nil, fmt.Errorf("start the handout workflow: %w", err)
	}
	return run, nil
}

// wait reads what the workflow returned, and reports a workflow that did not return
// instead of failing the run: a handout the server refused to accept a completion for
// is a reading too.
func (r *run) wait(ctx context.Context, timeout time.Duration) (exp19.Result, bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var res exp19.Result
	if err := r.handout.Get(waitCtx, &res); err != nil {
		return exp19.Result{}, false, err
	}
	return res, true, nil
}

// nothingHappened is what disturbed says when every reading of the series found the
// handout exactly as the kill left it.
const nothingHappened = "nothing"

// disturbed names what the series shows happening to the handout while nobody held
// it, measured against the reading the kill left behind: an activity the server has
// stopped holding, moved to another state or another attempt, dispatched again or
// run again is what this experiment is looking for, so it is named poll by poll.
func disturbed(polls []poll) string {
	if len(polls) == 0 {
		return "nothing was read"
	}
	atKill := polls[0]
	var found []string
	switch {
	case !atKill.pending:
		found = append(found, "poll-00: the kill left the server holding no pending activity at all")
	case atKill.state != enumspb.PENDING_ACTIVITY_STATE_STARTED.String():
		found = append(found, fmt.Sprintf("poll-00: the kill left the activity %s rather than %s", atKill.state, enumspb.PENDING_ACTIVITY_STATE_STARTED))
	}
	for _, p := range polls[1:] {
		var why []string
		switch {
		case !p.pending:
			why = append(why, "the server no longer holds it as a pending activity")
		case p.state != atKill.state:
			why = append(why, fmt.Sprintf("its state is %s rather than %s", p.state, atKill.state))
		}
		if p.attempt != atKill.attempt {
			why = append(why, fmt.Sprintf("it is on attempt %d rather than %d", p.attempt, atKill.attempt))
		}
		if p.starts != atKill.starts {
			why = append(why, fmt.Sprintf("its history holds %d ActivityTaskStarted event(s) rather than %d", p.starts, atKill.starts))
		}
		if p.lines != atKill.lines {
			why = append(why, fmt.Sprintf("its body has run %d time(s) rather than %d", p.lines, atKill.lines))
		}
		if len(why) > 0 {
			found = append(found, fmt.Sprintf("poll-%02d: %s", p.n, strings.Join(why, ", ")))
		}
	}
	if len(found) == 0 {
		return nothingHappened
	}
	return strings.Join(found, "; ")
}

// startsAcrossTheHold is what the ActivityTaskStarted count did over the series: one
// number when every reading agreed, and all of them in order when they did not.
func startsAcrossTheHold(polls []poll) string {
	if len(polls) == 0 {
		return "nothing was read"
	}
	counts := make([]string, 0, len(polls))
	same := true
	for _, p := range polls {
		counts = append(counts, fmt.Sprint(p.starts))
		same = same && p.starts == polls[0].starts
	}
	if same {
		return fmt.Sprintf("%d at every one of the %d readings", polls[0].starts, len(polls))
	}
	return strings.Join(counts, " ")
}

// changedAcrossTheHold is how many readings of the series say something other than
// the first one does, and which they are: the series is printed so a reader can see
// that nothing changed, and this counts it rather than leaving it to the eye.
func changedAcrossTheHold(polls []poll) (int, string) {
	if len(polls) == 0 {
		return 0, "nothing was read"
	}
	first := polls[0]
	var differing []string
	for _, p := range polls[1:] {
		if !p.same(first) {
			differing = append(differing, fmt.Sprintf("poll-%02d", p.n))
		}
	}
	if len(differing) == 0 {
		return 0, fmt.Sprintf("nothing: all %d readings report the same pending flag, state, attempt, ActivityTaskStarted count and body-execution count as poll-00", len(polls))
	}
	return len(differing), strings.Join(differing, " ")
}

// bodyRanAt is the instant the activity's body recorded for itself, read from the
// line it wrote: the handout was open from then, and from no moment the driver could
// name more exactly.
func bodyRanAt(line string) (time.Time, error) {
	for _, field := range strings.Fields(line) {
		value, ok := strings.CutPrefix(field, "at=")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("the body's line %q holds the time %q, which is not RFC 3339: %w", line, value, err)
		}
		return at, nil
	}
	return time.Time{}, fmt.Errorf("the body's line %q holds no at= time, so this run cannot say when the handout began", line)
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
