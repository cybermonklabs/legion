package exp21

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp19"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// Run is experiment 21: the handout to a process that holds a working copy and a
// credential, measured twice over on one server. Case A reaches that process out of
// band, with nothing but its task token in a file. Case B reaches it through a task
// queue the process named for itself, in the shape experiment 3 saw Temporal's own
// session framework use.
//
// Every value below is printed as the run observed it, including a value that
// contradicts what the mechanism is described to do: work that reached the holder by
// a route this experiment did not predict, or an outcome that did not come back, is
// the finding rather than a failure of the run.
func Run(ctx context.Context, cfg harness.Config) error {
	if role := os.Getenv(roleEnv); role != "" {
		// This process is the holder the driver started, not a run of the
		// experiment: it serves and exits on its own, so nothing it does reaches
		// the experiment's verdict line.
		os.Exit(holder(ctx, cfg, role))
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
	workflowQueue := fmt.Sprintf("exp21-workflow-%d", stamp)

	// The workflow runs inside this process and the handout activity never does:
	// the body must run in the worker process of case A and in the holder of case
	// B, or neither case would measure where the work went.
	wfWorker := worker.New(c, workflowQueue, worker.Options{})
	exp19.RegisterWorkflow(wfWorker)
	if err := wfWorker.Start(); err != nil {
		return fmt.Errorf("start workflow worker: %w", err)
	}
	defer wfWorker.Stop()

	harness.Fact("mechanism", "Temporal Asynchronous Activity Completion: an activity body that returns activity.ErrResultPending, completed later by whoever holds the task token")
	harness.Fact("sdk-completion-call", "client.CompleteActivity(ctx, taskToken, result, nil)")
	harness.Fact("question", "can a server that holds none of a session's state hand work to a process that holds all of it -- a working copy and a credential -- and take the outcome back?")
	harness.Fact("holder", "a process standing for a Legion pod: it makes a jj repository of its own under its work directory, writes a placeholder credential file, and does work only it could do -- a file written in that repository and committed with jj, whose commit id exists in no other repository")
	harness.Fact("holder-holds-of-the-engines-state", "none: no history, no task queue of the engine's, no workflow and no workflow state; what it holds is its working copy and a file standing in for a credential")
	harness.Fact("credential", credentialNote)
	harness.Fact("workflow-and-activity", "experiment 19's, imported rather than repeated: the same handout workflow, the same activity whose body returns activity.ErrResultPending, and -- in case A -- the same worker process to run that body")
	harness.Fact("handout-start-to-close-timeout", handoutWindow)
	harness.Fact("handout-heartbeat-timeout", "none: the activity is scheduled without one, so no heartbeat of either handout can expire")
	harness.Fact("handout-maximum-attempts", 1)
	harness.Fact("workflow-execution-timeout", workflowTimeout)
	harness.Fact("workflow-task-queue", workflowQueue)
	harness.Fact("driver-process-registered-the-handout-activity", false)

	state := &run{cfg: cfg, client: c, workDir: workDir, workflowQueue: workflowQueue, stamp: stamp}
	if err := state.caseA(ctx); err != nil {
		return err
	}
	if err := state.caseB(ctx); err != nil {
		return err
	}
	state.reasoning()
	return state.verdict()
}

// run is one execution of the experiment: the two cases share the server, the
// workflow task queue and the scratch directory, and each records what it measured
// for the reasoning and the verdict.
type run struct {
	cfg           harness.Config
	client        client.Client
	workDir       string
	workflowQueue string
	stamp         int64

	a reading
	b reading
}

// reading is what one case measured: enough to say whether the work reached the
// holder, whether its outcome came back, and whether the case took its readings at
// all.
type reading struct {
	// queue is the task queue the handout was dispatched to, and holderDir the
	// directory the holder held its working copy and its credential in.
	queue     string
	holderDir string
	// holderPID is the process the work was meant to reach; bodyPID is the process
	// the activity's body says it ran in.
	holderPID int
	bodyPID   int
	// report is what the holder did, in its own words.
	report Report
	// gotReport says the holder answered at all, which is the reading the case
	// cannot be taken without.
	gotReport bool
	// bodies is how many times the activity's body ran, counted outside Temporal.
	bodies int
	// openFor is how long the handout was open: from the instant the body's own
	// line says it ran to the instant the holder's answer reached the driver.
	openFor time.Duration
	// returned, received and errText are what the workflow ended with.
	returned bool
	received string
	errText  string
	// pollers is what the server said was polling the handout's task queue.
	pollers queuePollers
	// finalEvent is the last activity event of the run's history.
	finalEvent string
}

// caseA reaches the holder out of band. The activity's body runs on experiment 19's
// worker process, which holds nothing of the holder's; the holder runs no Temporal
// worker and registers no activity, and the only thing that crosses to it is its
// task token, in a file in its own work directory.
func (r *run) caseA(ctx context.Context) error {
	harness.Fact("case-a", "out of band: the activity's body hands its task token out from a worker process that is not the holder, the token reaches the holder as a file in the holder's own work directory, and the holder -- which runs no Temporal worker and registers no activity -- does its work and calls client.CompleteActivity with the commit id")

	dir := filepath.Join(r.workDir, fmt.Sprintf("exp21-holder-a-%d", r.stamp))
	queue := fmt.Sprintf("exp21-a-activity-%d", r.stamp)
	handedToken := filepath.Join(dir, "handed-task-token")
	r.a.queue = queue
	r.a.holderDir = dir

	harness.Fact("a-activity-task-queue", queue)
	harness.Fact("a-holder-work-directory", dir)
	harness.Fact("a-holder-repository", repositoryPath(dir))
	harness.Fact("a-holder-credential-file", credentialPath(dir))
	harness.Fact("a-holder-token-file", handedToken)

	// 1. The holder, which holds its working copy and its credential before
	// anything at all is scheduled, and which polls nothing.
	held, err := startHolder(ctx, r.cfg, "holder-a", r.workDir, dir, outOfBandMode, handedToken, r.file("a-holder-report.json"), holderStartWait)
	if err != nil {
		return err
	}
	defer held.Stop()
	r.a.holderPID = held.PID
	harness.Fact("a-holder-pid", held.PID)
	harness.Fact("a-holder-host", held.Host)
	harness.Fact("a-holder-ran-a-temporal-worker", false)
	harness.Fact("a-holder-registered-the-handout-activity", false)
	harness.Fact("a-holder-task-queue", orNone(held.Queue))
	harness.Fact("a-holder-held-its-working-copy-before-the-workflow-started", true)

	// 2. The process that runs the activity's body: experiment 19's worker, which
	// is not the holder and holds nothing the holder holds.
	dispatcher, err := exp19.StartWorkerChild(ctx, r.cfg, "dispatch-worker", r.workDir, queue, workerStartWait)
	if err != nil {
		return err
	}
	defer dispatcher.Stop()
	harness.Fact("a-dispatch-worker-pid", dispatcher.PID)
	harness.Fact("a-dispatch-worker-is-the-holder", dispatcher.PID == held.PID)

	// 3. The workflow, whose one activity is dispatched to that worker's queue.
	params := exp19.Params{
		TaskQueue:    queue,
		EffectFile:   r.file("a-handout-effects"),
		TokenFile:    r.file("a-handout-tokens"),
		StartToClose: handoutWindow,
		MaxAttempts:  1,
	}
	harness.Fact("a-handout-side-effect-file", params.EffectFile)
	handout, err := r.start(ctx, "a-handout", params)
	if err != nil {
		return err
	}
	harness.Fact("a-handout-workflow-id", handout.GetID())
	harness.Fact("a-handout-run-id", handout.GetRunID())

	lines, err := exp19.WaitForLines(ctx, params.EffectFile, 1, bodyWait)
	if err != nil {
		return fmt.Errorf("the handout activity's body never ran: %w", err)
	}
	ranAt, err := bodyRanAt(lines[0])
	if err != nil {
		return err
	}
	r.a.bodies = len(lines)
	r.a.bodyPID, err = bodyPID(lines[0])
	if err != nil {
		return err
	}
	harness.Fact("a-handout-first-body-line", lines[0])
	harness.Fact("a-handout-body-ran-in-pid", r.a.bodyPID)
	harness.Fact("a-handout-body-ran-in-the-holder", r.a.bodyPID == held.PID)

	before, status, err := exp19.WaitActivityStarted(ctx, r.client, handout, bodyWait)
	if err != nil {
		return err
	}
	identity, err := lastWorkerIdentity(ctx, r.client, handout)
	if err != nil {
		return err
	}
	harness.Fact("a-handout-body-executions-before-the-handover", r.a.bodies)
	harness.Fact("a-handout-activity-state-before-the-handover", before.State)
	harness.Fact("a-handout-activity-attempt-before-the-handover", before.Attempt)
	harness.Fact("a-handout-last-worker-identity-reported-by-the-server", orNone(identity))
	harness.Fact("a-handout-workflow-status-before-the-handover", status)

	// 4. The worker that ran the body goes away before the token is handed over, so
	// nothing of Temporal's is alive to take the work while the holder does it.
	dispatcher.Stop()
	stoppedAt := time.Now()
	harness.Fact("a-dispatch-worker-stopped-before-the-handover", true)
	harness.Fact("a-dispatch-worker-alive-after-it-was-stopped", dispatcher.Alive())

	// 5. The handover: one file, in the holder's own work directory.
	token, err := exp19.TokenForAttempt(params.TokenFile, 1)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("attempt=1 token=%s\n", base64.StdEncoding.EncodeToString(token))
	if err := os.WriteFile(handedToken, []byte(line), 0o644); err != nil {
		return fmt.Errorf("hand the task token to the holder by writing %s: %w", handedToken, err)
	}
	harness.Fact("a-token-bytes", len(token))
	harness.Fact("a-token-handed-to-the-holder-by", fmt.Sprintf("a file written into the holder's own work directory, %s: no task queue, no worker and no Temporal connection of the holder's carried it", handedToken))

	pollers, err := describePollers(ctx, r.client, queue, stoppedAt)
	if err != nil {
		return err
	}
	r.a.pollers = pollers
	pollers.report("a-activity-task-queue", "after the dispatch worker was stopped")

	// 6. What the holder did with it.
	report, err := held.Wait(ctx, holderWorkWait)
	if err != nil {
		return err
	}
	r.a.report = report
	r.a.gotReport = true
	r.a.openFor = time.Since(ranAt)
	reportFacts("a", report)
	harness.Fact("a-completion-caller-is-the-holder", report.PID == held.PID)
	harness.Fact("timed: a-handout-stayed-open-from-the-body-running-until-the-holder-had-answered", r.a.openFor.Round(time.Millisecond))

	// 7. What the workflow received.
	return r.readWorkflow(ctx, "a", handout, params, &r.a)
}

// caseB reaches the holder through a task queue the holder named for itself. The
// same handout activity is scheduled on that queue, its body runs inside the holder,
// and the holder completes it by token from its own process.
func (r *run) caseB(ctx context.Context) error {
	harness.Fact("case-b", "a queue named for the holder: the holder registers a worker on a task queue it names for itself, in the <resourceID>@<hostname> shape experiment 3 saw Temporal's session framework use, the workflow schedules the same handout activity on that queue, the body runs inside the holder, and the holder completes it by token from its own process")

	dir := filepath.Join(r.workDir, fmt.Sprintf("exp21-holder-b-%d", r.stamp))
	// The body runs inside the holder, so the file it writes its task token to is
	// inside the holder's own work directory: the token this case completes with
	// never leaves the process that was handed the work.
	tokenFile := filepath.Join(dir, "own-task-token")
	r.b.holderDir = dir

	harness.Fact("b-holder-work-directory", dir)
	harness.Fact("b-holder-repository", repositoryPath(dir))
	harness.Fact("b-holder-credential-file", credentialPath(dir))
	harness.Fact("b-holder-token-file", tokenFile)

	// 1. The holder, with a worker of its own on a queue it names for itself. The
	// driver learns that name here, from the holder, and from nowhere else.
	held, err := startHolder(ctx, r.cfg, "holder-b", r.workDir, dir, queueMode, tokenFile, r.file("b-holder-report.json"), holderStartWait)
	if err != nil {
		return err
	}
	defer held.Stop()
	r.b.holderPID = held.PID
	r.b.queue = held.Queue
	harness.Fact("b-holder-pid", held.PID)
	harness.Fact("b-holder-host", held.Host)
	harness.Fact("b-holder-ran-a-temporal-worker", true)
	harness.Fact("b-holder-registered-the-handout-activity", true)
	harness.Fact("b-task-queue", held.Queue)
	harness.Fact("b-task-queue-named-by", "the holder process itself, which reported it to the driver before any workflow was started; nothing else in this run could have known it")
	harness.Fact("b-task-queue-shape", fmt.Sprintf("<resourceID>@<hostname>, the shape experiment 3 read out of the history for the queue a Temporal session's own worker process made: here the resource is the holder, and the hostname is %s", held.Host))
	harness.Fact("b-holder-worker-identity", held.Identity)

	// 2. The workflow, whose one activity is dispatched to the holder's own queue.
	params := exp19.Params{
		TaskQueue:    held.Queue,
		EffectFile:   r.file("b-handout-effects"),
		TokenFile:    tokenFile,
		StartToClose: handoutWindow,
		MaxAttempts:  1,
	}
	harness.Fact("b-handout-side-effect-file", params.EffectFile)
	scheduledAt := time.Now()
	handout, err := r.start(ctx, "b-handout", params)
	if err != nil {
		return err
	}
	harness.Fact("b-handout-workflow-id", handout.GetID())
	harness.Fact("b-handout-run-id", handout.GetRunID())

	lines, err := exp19.WaitForLines(ctx, params.EffectFile, 1, bodyWait)
	if err != nil {
		return fmt.Errorf("the handout activity's body never ran, so the work never reached the holder's queue: %w", err)
	}
	ranAt, err := bodyRanAt(lines[0])
	if err != nil {
		return err
	}
	r.b.bodies = len(lines)
	r.b.bodyPID, err = bodyPID(lines[0])
	if err != nil {
		return err
	}
	harness.Fact("b-handout-first-body-line", lines[0])
	harness.Fact("b-handout-body-ran-in-pid", r.b.bodyPID)
	harness.Fact("b-handout-body-ran-in-the-holder", r.b.bodyPID == held.PID)

	before, status, err := exp19.WaitActivityStarted(ctx, r.client, handout, bodyWait)
	if err != nil {
		return err
	}
	identity, err := lastWorkerIdentity(ctx, r.client, handout)
	if err != nil {
		return err
	}
	scheduledQueue, err := scheduledTaskQueue(ctx, r.client, handout)
	if err != nil {
		return err
	}
	harness.Fact("b-handout-body-executions-before-the-completion", r.b.bodies)
	harness.Fact("b-handout-activity-state-while-the-holder-works", before.State)
	harness.Fact("b-handout-activity-attempt", before.Attempt)
	harness.Fact("b-handout-task-queue-in-the-servers-own-history", orNone(scheduledQueue))
	harness.Fact("b-handout-task-queue-in-the-history-is-the-holders", scheduledQueue == held.Queue)
	harness.Fact("b-handout-last-worker-identity-reported-by-the-server", orNone(identity))
	harness.Fact("b-handout-last-worker-identity-is-the-holders", identity == held.Identity)
	harness.Fact("b-handout-workflow-status-while-the-holder-works", status)

	// 3. Who else could have taken it: what the server says is polling that queue.
	pollers, err := describePollers(ctx, r.client, held.Queue, scheduledAt)
	if err != nil {
		return err
	}
	r.b.pollers = pollers
	pollers.report("b-task-queue", "after the workflow was started")
	harness.Fact("b-task-queue-pollers-other-than-the-holder", pollers.otherThan(held.Identity))
	harness.Fact("b-processes-this-run-started-that-registered-the-handout-activity-on-this-queue",
		fmt.Sprintf("1, the holder in pid %d: the driver registered the workflow and no activity, and case A's dispatch worker registered the activity on %s, another queue, and was stopped before this case began", held.PID, r.a.queue))

	// 4. What the holder did, in the same process the body ran in.
	report, err := held.Wait(ctx, holderWorkWait)
	if err != nil {
		return err
	}
	r.b.report = report
	r.b.gotReport = true
	r.b.openFor = time.Since(ranAt)
	reportFacts("b", report)
	harness.Fact("b-completion-caller-is-the-process-the-body-ran-in", report.PID == r.b.bodyPID)
	harness.Fact("timed: b-handout-stayed-open-from-the-body-running-until-the-holder-had-answered", r.b.openFor.Round(time.Millisecond))

	// 5. What the workflow received.
	return r.readWorkflow(ctx, "b", handout, params, &r.b)
}

// readWorkflow reads what one case's workflow ended with and what the server's own
// history says about its activity, and records it for the reasoning and the verdict.
func (r *run) readWorkflow(ctx context.Context, prefix string, handout client.WorkflowRun, params exp19.Params, into *reading) error {
	res, returned, waitErr := r.wait(ctx, handout, workflowWait)
	into.returned = returned
	harness.Fact(prefix+"-handout-workflow-returned", returned)
	if !returned {
		harness.Fact(prefix+"-handout-workflow-wait-error", waitErr)
	} else {
		into.received = res.Completion
		into.errText = res.ErrorText
		harness.Fact(prefix+"-handout-workflow-received-result", orNone(res.Completion))
		harness.Fact(prefix+"-handout-workflow-received-the-holders-commit-id", res.Completion != "" && res.Completion == into.report.CommitID)
		harness.Fact(prefix+"-handout-workflow-activity-error-type", orNone(res.ErrorType))
		harness.Fact(prefix+"-handout-workflow-activity-error-is-timeout", res.IsTimeout)
		harness.Fact(prefix+"-handout-workflow-activity-error", orNone(res.ErrorText))
	}

	bodies, err := exp19.Lines(params.EffectFile)
	if err != nil {
		return err
	}
	into.bodies = bodies
	hist, err := exp19.ReadHistory(ctx, r.client, handout)
	if err != nil {
		return err
	}
	into.finalEvent = hist.Final
	_, status, err := exp19.DescribeActivity(ctx, r.client, handout)
	if err != nil {
		return err
	}
	harness.Fact(prefix+"-handout-body-executions-total", bodies)
	harness.Fact(prefix+"-handout-activity-task-scheduled-events", hist.Scheduled)
	harness.Fact(prefix+"-handout-activity-task-started-events", hist.Starts)
	harness.Fact(prefix+"-handout-activity-task-completed-events", hist.Completed)
	harness.Fact(prefix+"-handout-activity-task-failed-events", hist.Failed)
	harness.Fact(prefix+"-handout-activity-task-timed-out-events", hist.TimedOut)
	harness.Fact(prefix+"-handout-last-activity-history-event", hist.Final)
	harness.Fact(prefix+"-handout-history-event-count", hist.Events)
	harness.Fact(prefix+"-handout-workflow-status", status)
	return nil
}

// reasoning says, for each case, what the readings above amount to: how the work
// reached the holder, whether the outcome came back, what the engine had to be told
// for the work to reach that process at all, and what the four hours above are and
// are not.
func (r *run) reasoning() {
	harness.Fact("reasoned: a-did-the-work-reach-the-holder",
		fmt.Sprintf("%s, carried by a file and nothing else: the body ran once in pid %d, a worker process that is not the holder, and the driver copied the %d-byte task token into the holder's own work directory; the holder, pid %d, ran no Temporal worker, registered no activity and polled no task queue, and it already held the working copy %s and the placeholder credential %s before the workflow was started",
			reached(r.a), r.a.bodyPID, r.a.report.TokenBytes, r.a.holderPID, r.a.report.RepoPath, r.a.report.CredentialPath))
	harness.Fact("reasoned: a-did-the-outcome-come-back",
		outcomeSentence(r.a, "client.CompleteActivity from a process that runs no worker"))
	harness.Fact("reasoned: a-what-the-engine-had-to-be-told",
		fmt.Sprintf("nothing whatever about the holder: the server was told an activity type, the task queue %s that its own dispatch worker polls, a %s start-to-close timeout and one attempt, and it was never told the holder's host, its repository, its credential, its queue or that it existed. The whole of the integration was %d bytes of task token carried out of band and one client.CompleteActivity call carrying the result back, so the price of addressing this process was a byte string and a connection the holder opens itself",
			r.a.queue, handoutWindow, r.a.report.TokenBytes))
	harness.Fact("reasoned: a-the-bound", boundSentence(r.a))

	harness.Fact("reasoned: b-did-the-work-reach-the-holder",
		fmt.Sprintf("%s, carried by the task queue the holder named for itself: the server's own history records the activity as scheduled on %s, the server reported %d poller(s) on that queue (%s), and the body ran in pid %d, which is the holder's own pid; the holder held the working copy %s and the placeholder credential %s before the workflow was started",
			reached(r.b), r.b.queue, r.b.pollers.count, r.b.pollers.joined(), r.b.bodyPID, r.b.report.RepoPath, r.b.report.CredentialPath))
	harness.Fact("reasoned: b-did-the-outcome-come-back",
		outcomeSentence(r.b, "client.CompleteActivity from the same process the body ran in"))
	harness.Fact("reasoned: b-what-the-engine-had-to-be-told",
		fmt.Sprintf("one string and one worker: the workflow's ActivityOptions.TaskQueue had to carry the exact name %s, which the holder made for itself and which the driver could only learn from the holder before it started the workflow, and the holder had to be a Temporal worker on that name -- an SDK client, a long poll against the server and the activity type registered in the holder's own process. That is the integration cost this case prices, and it is more than case A's, which was the task token alone",
			r.b.queue))
	harness.Fact("reasoned: b-could-anything-else-have-taken-it",
		fmt.Sprintf("the server reported %d poller(s) on %s and %d of them other than the holder itself (%s); the driver registered no handout activity, and the only other worker this run started, case A's dispatch worker in pid %d, polled %s and was stopped before this case began",
			r.b.pollers.count, r.b.queue, r.b.pollers.otherThan(r.b.report.Identity), r.b.pollers.joined(), r.a.bodyPID, r.a.queue))
	harness.Fact("reasoned: b-the-bound", boundSentence(r.b))

	harness.Fact("reasoned: what-the-two-routes-cost-differently",
		fmt.Sprintf("both routes reached a process holding a working copy and a credential and both brought a commit id back (%s out of band, %s through the holder's own queue, two commits that exist in two different repositories). What separates them is what the engine was told: out of band it was told nothing about the holder and the token was carried by whatever the caller likes, while the queue route made the holder a worker and made its name part of what the workflow schedules",
			shortID(r.a.report.CommitID), shortID(r.b.report.CommitID)))

	harness.Fact("limit",
		fmt.Sprintf("one dev server on one pod, one holder per case, a scratch jj repository each holder made for itself and a credential that is a placeholder: it shows that a handout reached a process holding a working copy, that the commit that process made came back to the workflow as the activity's result, and what each route had to be told. It does not show what handling a real credential costs, because none was used; not what a pod-sized working copy or a session of hours costs; not what carrying a task token to a pod on another machine costs, because this holder read it from a file on the same filesystem; not what a cluster or a restarted server would hold; and not that Legion's pods would map onto either route. The %s start-to-close timeout above is a bound the server was given, and what this run observed is %s for case A and %s for case B",
			handoutWindow, r.a.openFor.Round(time.Millisecond), r.b.openFor.Round(time.Millisecond)))
}

// verdict fails the run that could not take one of its readings, and passes every
// run that took them all -- whatever they say. What the server and the holder did is
// the measurement, so no reading here is required to come out one way: a refused
// completion, an outcome that did not come back or work that reached the holder by a
// route this experiment did not predict is a finding this run prints and passes with.
func (r *run) verdict() error {
	for _, c := range []struct {
		name string
		got  reading
	}{{"a", r.a}, {"b", r.b}} {
		switch {
		case !c.got.gotReport:
			return fmt.Errorf("case %s: the holder wrote no report, so this run has no reading of what the process holding the working copy did with the work", c.name)
		case c.got.report.CommitID == "":
			return fmt.Errorf("case %s: the holder made no commit, so nothing only that process could do was done and this run measured nothing about an outcome coming back", c.name)
		case c.got.report.TokenBytes == 0:
			return fmt.Errorf("case %s: no task token reached the holder, so this run measured nothing about work reaching it", c.name)
		case c.got.bodies != 1:
			return fmt.Errorf("case %s: the activity's body ran %d time(s), not once, so what the holder answered for was not a single handout", c.name, c.got.bodies)
		case !c.got.returned:
			return fmt.Errorf("case %s: the handout's workflow did not return within %s, so this run has no reading of what the workflow received", c.name, workflowWait)
		}
	}
	return nil
}

// reportFacts prints what one holder did, under its own case's prefix.
func reportFacts(prefix string, report Report) {
	harness.Fact(prefix+"-holder-jj-version", report.JJVersion)
	harness.Fact(prefix+"-holder-repository-made-by-the-holder", report.RepoPath)
	harness.Fact(prefix+"-holder-credential-written-by-the-holder", report.CredentialPath)
	harness.Fact(prefix+"-holder-credential-bytes", report.CredentialBytes)
	harness.Fact(prefix+"-holder-credential-is-a-placeholder", true)
	harness.Fact(prefix+"-holder-credential-first-line", report.CredentialFirstLine)
	harness.Fact(prefix+"-holder-read-the-token-from", report.TokenReadFrom)
	harness.Fact(prefix+"-holder-token-bytes", report.TokenBytes)
	harness.Fact(prefix+"-holder-work", report.Work)
	harness.Fact(prefix+"-holder-commit-id", report.CommitID)
	harness.Fact(prefix+"-holder-commit-read-back-from-its-own-repository", report.CommitReadBack)
	harness.Fact("timed: "+prefix+"-holder-work-took", report.WorkTook.Round(time.Millisecond))
	harness.Fact(prefix+"-completion-called-by-pid", report.PID)
	harness.Fact(prefix+"-completion-accepted-by-the-server", report.Accepted)
	harness.Fact(prefix+"-completion-error-type", orNone(report.ErrorType))
	harness.Fact(prefix+"-completion-error-message", orNone(report.ErrorMessage))
}

// reached says whether the work reached the holder at all, in the word the reasoned
// line opens with.
func reached(c reading) string {
	if c.report.TokenBytes > 0 && c.report.CommitID != "" {
		return "yes"
	}
	return "no"
}

// outcomeSentence says whether the outcome of the work done inside the holder came
// back to the workflow, by value and in the words the readings support.
func outcomeSentence(c reading, by string) string {
	accepted := "refused"
	if c.report.Accepted {
		accepted = "accepted"
	}
	if c.returned && c.received != "" && c.received == c.report.CommitID {
		return fmt.Sprintf("yes, by value: the holder committed %s in its own repository, answered with %s, the server %s the call, and the workflow received %q -- the same string, which existed nowhere before the holder made it",
			c.report.CommitID, by, accepted, c.received)
	}
	if !c.returned {
		return fmt.Sprintf("no: the holder committed %s and answered with %s, which the server %s, but the workflow did not return within %s, so nothing was observed arriving",
			c.report.CommitID, by, accepted, workflowWait)
	}
	return fmt.Sprintf("no: the holder committed %s and answered with %s, which the server %s, and the workflow returned with result %q and error %q instead",
		c.report.CommitID, by, accepted, orNone(c.received), orNone(c.errText))
}

// boundSentence is what the four hours above are and are not. A run of minutes must
// never read as a claim about hours, so the sentence says which number the server
// was given and which number this run observed, and names the longest hold anything
// in this directory has watched.
func boundSentence(c reading) string {
	return fmt.Sprintf("the %s start-to-close timeout is a bound this run gave the server, not a duration it observed: what was observed is %s, the time from the body running to the holder's answer. The longest anything in this directory has watched a handout stay open is experiment 20's five-minute hold with no worker alive; neither that run nor this one observed hours, so nothing here supports a claim about a handout of hours beyond saying that the bound the server enforces is the one it is given",
		handoutWindow, c.openFor.Round(time.Millisecond))
}

// file is one of the run's scratch files, named so that two runs never share one.
func (r *run) file(name string) string {
	return filepath.Join(r.workDir, fmt.Sprintf("exp21-%s-%d", name, r.stamp))
}

// start runs one handout workflow, which is experiment 19's.
func (r *run) start(ctx context.Context, name string, p exp19.Params) (client.WorkflowRun, error) {
	run, err := r.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp21-%s-%d", name, r.stamp),
		TaskQueue:                r.workflowQueue,
		WorkflowExecutionTimeout: workflowTimeout,
		WorkflowTaskTimeout:      10 * time.Second,
	}, exp19.WorkflowName, p)
	if err != nil {
		return nil, fmt.Errorf("start the %s workflow: %w", name, err)
	}
	return run, nil
}

// wait reads what a workflow returned, and reports a workflow that did not return
// instead of failing the run: an outcome that never arrived is a reading too.
func (r *run) wait(ctx context.Context, handout client.WorkflowRun, timeout time.Duration) (exp19.Result, bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var res exp19.Result
	if err := handout.Get(waitCtx, &res); err != nil {
		return exp19.Result{}, false, err
	}
	return res, true, nil
}

// lastWorkerIdentity is the identity the server has recorded for the worker that
// took the handout activity: the server's own answer to which process is holding it.
func lastWorkerIdentity(ctx context.Context, c client.Client, handout client.WorkflowRun) (string, error) {
	desc, err := c.DescribeWorkflowExecution(ctx, handout.GetID(), handout.GetRunID())
	if err != nil {
		return "", fmt.Errorf("describe %s: %w", handout.GetID(), err)
	}
	for _, info := range desc.GetPendingActivities() {
		if info.GetActivityType().GetName() != exp19.HandoutActivityName {
			continue
		}
		return info.GetLastWorkerIdentity(), nil
	}
	return "", nil
}

// scheduledTaskQueue is the task queue the server's own history says the handout
// activity was dispatched to, read from the ActivityTaskScheduled event rather than
// from what this process believes it asked for.
func scheduledTaskQueue(ctx context.Context, c client.Client, handout client.WorkflowRun) (string, error) {
	events, err := harness.History(ctx, c, handout.GetID(), handout.GetRunID())
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			return attrs.GetTaskQueue().GetName(), nil
		}
	}
	return "", nil
}

// queuePollers is what the server says about one task queue's activity pollers: a
// queue only the holder polls is how case B keeps the work to one process, so the
// experiment reads the pollers rather than only the queue name.
type queuePollers struct {
	queue string
	// count is every poller the server has heard from lately, which includes a
	// worker that has since been stopped: the server keeps one it saw for minutes.
	count int
	// since is how many of them polled after the moment the case cares about.
	since int
	// identities is what each of them calls itself, with when it last polled.
	identities []string
}

// describePollers asks the server who is polling a task queue for activities, and
// when each of them last did.
func describePollers(ctx context.Context, c client.Client, queue string, since time.Time) (queuePollers, error) {
	found := queuePollers{queue: queue}
	desc, err := c.DescribeTaskQueue(ctx, queue, enumspb.TASK_QUEUE_TYPE_ACTIVITY)
	if err != nil {
		return found, fmt.Errorf("describe task queue %s: %w", queue, err)
	}
	for _, poller := range desc.GetPollers() {
		last := poller.GetLastAccessTime().AsTime()
		found.count++
		if last.After(since) {
			found.since++
		}
		found.identities = append(found.identities,
			fmt.Sprintf("%s last-polled-at=%s", poller.GetIdentity(), last.UTC().Format(time.RFC3339Nano)))
	}
	return found, nil
}

// report prints one reading of a queue's pollers, under what moment the second count
// is taken from.
func (q queuePollers) report(prefix, sinceWhat string) {
	harness.Fact(prefix+"-pollers", q.count)
	harness.Fact(prefix+"-pollers-that-polled-"+strings.ReplaceAll(sinceWhat, " ", "-"), q.since)
	harness.Fact(prefix+"-poller-identities", q.joined())
}

// joined is every poller on one line, as the transcript prints them.
func (q queuePollers) joined() string {
	if len(q.identities) == 0 {
		return "none"
	}
	return strings.Join(q.identities, "; ")
}

// otherThan is how many pollers of this queue are not the one identity named, which
// is how case B answers whether anything else could have taken the work.
func (q queuePollers) otherThan(identity string) int {
	other := 0
	for _, poller := range q.identities {
		if !strings.HasPrefix(poller, identity+" ") {
			other++
		}
	}
	return other
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

// bodyPID is the process the activity's body says it ran in, which is the reading
// that says whether the work reached the holder's own process.
func bodyPID(line string) (int, error) {
	for _, field := range strings.Fields(line) {
		value, ok := strings.CutPrefix(field, "pid=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("the body's line %q holds the pid %q, which is not a number: %w", line, value, err)
		}
		return pid, nil
	}
	return 0, fmt.Errorf("the body's line %q holds no pid=, so this run cannot say which process ran it", line)
}

// shortID is a commit id as a sentence names it, rather than all forty characters of
// it twice over. The full id is printed as its own fact.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
