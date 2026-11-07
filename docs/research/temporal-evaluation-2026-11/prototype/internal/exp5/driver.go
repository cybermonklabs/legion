package exp5

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

const (
	dialTimeout     = 30 * time.Second
	workflowTimeout = 2 * time.Minute
	waitTimeout     = 90 * time.Second

	// admissionCap is the admission limit under test, waveSize how many issues ask for
	// it at once, and waves how many times that happens. Two waves let the gatekeeper
	// fall idle in the middle of the run, which is when it continues as new.
	admissionCap = 4
	waveSize     = 6
	waves        = 2
	// hold is how long an admitted issue occupies its slot. It is long against the
	// rate at which this server starts workflows: with a short hold the control run's
	// early issues finish before its late ones begin, and the control then measures
	// how fast issues start rather than the absence of a cap.
	hold = 1500 * time.Millisecond
	// grantTimeout is how long an issue waits for its grant before asking again.
	grantTimeout = 3 * time.Second
	// rollAfter is how many signals one gatekeeper run handles before it continues as
	// new, which it does only once nothing is in flight.
	rollAfter = 8

	// workerActivitySlots and workerTaskSlots are set far above the cap on purpose: if
	// they bound anything, the experiment would be measuring the worker rather than
	// the gatekeeper.
	workerActivitySlots = 64
	workerTaskSlots     = 64

	cappedLabel   = "capped"
	uncappedLabel = "uncapped"
)

// Run is experiment 5: a cap of four held by a gatekeeper workflow, measured by what the
// work actually did, and the same work again with the cap taken away.
func Run(ctx context.Context, cfg harness.Config) error {
	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	stamp := time.Now().UnixNano()
	queue := fmt.Sprintf("exp5-%d", stamp)
	w := worker.New(c, queue, worker.Options{
		MaxConcurrentActivityExecutionSize:     workerActivitySlots,
		MaxConcurrentWorkflowTaskExecutionSize: workerTaskSlots,
	})
	Register(w)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	harness.Fact("admission-cap", admissionCap)
	harness.Fact("issues-per-wave", waveSize)
	harness.Fact("waves", waves)
	harness.Fact("issues-total", waveSize*waves)
	harness.Fact("hold-per-issue", hold)
	harness.Fact("worker-max-concurrent-activities", workerActivitySlots)
	harness.Fact("worker-max-concurrent-workflow-tasks", workerTaskSlots)
	harness.Fact("why-not-worker-concurrency", "worker-side concurrency is explicitly not the mechanism: it caps one worker process rather than the deployment, so it would measure nothing about a limit across workflows")

	// 1. The capped run.
	gatekeeperID := fmt.Sprintf("exp5-gatekeeper-%d", stamp)
	gatekeeper, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                  gatekeeperID,
		TaskQueue:           queue,
		WorkflowTaskTimeout: 10 * time.Second,
	}, GatekeeperWorkflowName, GateState{Cap: admissionCap, RollAfter: rollAfter})
	if err != nil {
		return fmt.Errorf("start the gatekeeper: %w", err)
	}
	firstGeneration := gatekeeper.GetRunID()
	harness.Fact("gatekeeper-workflow-id", gatekeeperID)
	harness.Fact("gatekeeper-first-run-id", firstGeneration)

	queries := 0
	issues := 0
	for wave := 1; wave <= waves; wave++ {
		runs := make([]client.WorkflowRun, 0, waveSize)
		for range waveSize {
			issues++
			issueID := fmt.Sprintf("EXP5-%d-%03d", stamp, issues)
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:                       fmt.Sprintf("exp5-issue-%d-%03d", stamp, issues),
				TaskQueue:                queue,
				WorkflowExecutionTimeout: workflowTimeout,
			}, IssueWorkflowName, IssueParams{
				IssueID:      issueID,
				Gatekeeper:   gatekeeperID,
				Label:        cappedLabel,
				Hold:         hold,
				GrantTimeout: grantTimeout,
			})
			if err != nil {
				return fmt.Errorf("start issue %s: %w", issueID, err)
			}
			runs = append(runs, run)
		}
		state, err := queryGate(ctx, c, gatekeeperID, &queries)
		if err != nil {
			return err
		}
		harness.Fact(fmt.Sprintf("wave-%d-gatekeeper-holders-when-the-wave-started", wave), len(state.Holders))
		if err := awaitAll(ctx, runs); err != nil {
			return err
		}
		harness.Fact(fmt.Sprintf("wave-%d-issues-finished", wave), len(runs))
	}

	// The gatekeeper rolls its history over when it falls idle; give it the moment
	// that takes before asking what it ended up as.
	settled, err := awaitGate(ctx, c, gatekeeperID, &queries, func(s GateState) bool {
		return s.Admitted == waveSize*waves && len(s.Holders) == 0 && s.Generation > 0
	}, 20*time.Second)
	if err != nil {
		return fmt.Errorf("the gatekeeper never settled: %w", err)
	}
	harness.Fact("gatekeeper-admitted-total", settled.Admitted)
	harness.Fact("gatekeeper-released-total", settled.Released)
	harness.Fact("gatekeeper-generations", settled.Generation)
	harness.Fact("gatekeeper-holders-at-the-end", len(settled.Holders))
	harness.Fact("gatekeeper-queue-at-the-end", len(settled.Queue))

	generations, signalsToGatekeeper, continuedAsNew, err := walkGatekeeper(ctx, c, gatekeeperID, firstGeneration)
	if err != nil {
		return err
	}
	harness.Fact("gatekeeper-runs-in-history", generations)
	harness.Fact("gatekeeper-continued-as-new", continuedAsNew)
	harness.Fact("gatekeeper-signals-received", signalsToGatekeeper)

	grants, timers, err := walkIssues(ctx, c, stamp, waveSize*waves)
	if err != nil {
		return err
	}
	harness.Fact("grants-signalled-to-issues", grants)
	harness.Fact("retry-timers-started-by-issues", timers)
	harness.Fact("queries-sent-to-the-gatekeeper", queries)

	cappedHolds, cappedPeak, cappedOverlap, _ := Observed(cappedLabel)
	harness.Fact("capped-holds-recorded", cappedHolds)
	harness.Fact("capped-max-concurrency-counted-live", cappedPeak)
	harness.Fact("capped-max-concurrency-by-timestamps", cappedOverlap)

	// The gatekeeper is long-lived by design, so the run has to end it.
	if err := c.TerminateWorkflow(ctx, gatekeeperID, "", "experiment 5 finished"); err != nil {
		return fmt.Errorf("terminate the gatekeeper: %w", err)
	}
	harness.Fact("gatekeeper-terminated", true)

	// 2. The negative control: the same work, no cap in front of it.
	control := make([]client.WorkflowRun, 0, waveSize*waves)
	for i := 1; i <= waveSize*waves; i++ {
		issueID := fmt.Sprintf("EXP5-CONTROL-%d-%03d", stamp, i)
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:                       fmt.Sprintf("exp5-control-%d-%03d", stamp, i),
			TaskQueue:                queue,
			WorkflowExecutionTimeout: workflowTimeout,
		}, IssueWorkflowName, IssueParams{IssueID: issueID, Label: uncappedLabel, Hold: hold})
		if err != nil {
			return fmt.Errorf("start control issue %s: %w", issueID, err)
		}
		control = append(control, run)
	}
	if err := awaitAll(ctx, control); err != nil {
		return err
	}
	uncappedHolds, uncappedPeak, uncappedOverlap, _ := Observed(uncappedLabel)
	harness.Fact("control-issues", len(control))
	harness.Fact("control-holds-recorded", uncappedHolds)
	harness.Fact("control-max-concurrency-counted-live", uncappedPeak)
	harness.Fact("control-max-concurrency-by-timestamps", uncappedOverlap)

	// 3. What the mechanism cost.
	file, lines, err := harness.FuncLines(Gatekeeper)
	if err != nil {
		return err
	}
	harness.Fact("gatekeeper-source-file", file)
	harness.Fact("gatekeeper-lines-of-go", lines)
	admissions := settled.Admitted
	actions := signalsToGatekeeper + grants + timers + queries
	harness.Fact("actions-rule-applied", "one Action per signal, per query and per timer started (Temporal Cloud's published rule); the issue workflows' own starts and activities are the work, not the admission")
	harness.Fact("actions-of-admission-total", actions)
	harness.Fact("actions-per-admission", fmt.Sprintf("%.2f", float64(actions)/float64(admissions)))
	harness.Fact("actions-per-admission-without-the-driver-queries", fmt.Sprintf("%.2f", float64(actions-queries)/float64(admissions)))
	harness.Fact("limit", "twelve issues, one worker process and a cap of four on one dev server: it shows a gatekeeper workflow bounds concurrency across workflows and that the uncapped control does not, not that the gatekeeper holds at Legion's issue rate or survives a busy deployment's signal volume")

	switch {
	case cappedHolds != waveSize*waves:
		return fmt.Errorf("the capped run recorded %d holds, not the %d issues it started", cappedHolds, waveSize*waves)
	case cappedOverlap > admissionCap || cappedPeak > admissionCap:
		return fmt.Errorf("the capped run reached %d concurrent holds (live count %d), above the cap of %d", cappedOverlap, cappedPeak, admissionCap)
	case cappedOverlap < admissionCap:
		return fmt.Errorf("the capped run never reached the cap: %d concurrent holds against a cap of %d, so this run does not show the cap binding", cappedOverlap, admissionCap)
	case !continuedAsNew || settled.Generation < 1:
		return fmt.Errorf("the gatekeeper never continued as new, so this run says nothing about a long-lived counter")
	case uncappedOverlap <= admissionCap:
		return fmt.Errorf("the control reached only %d concurrent holds with no cap at all, so the capped measurement proves nothing", uncappedOverlap)
	}
	return nil
}

// awaitAll waits for every run to finish.
func awaitAll(ctx context.Context, runs []client.WorkflowRun) error {
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for _, run := range runs {
		var stamp string
		if err := run.Get(waitCtx, &stamp); err != nil {
			return fmt.Errorf("issue %s did not finish: %w", run.GetID(), err)
		}
	}
	return nil
}

// queryGate asks the gatekeeper for its state, counting the query.
func queryGate(ctx context.Context, c client.Client, workflowID string, queries *int) (GateState, error) {
	*queries++
	encoded, err := c.QueryWorkflow(ctx, workflowID, "", StateQuery)
	if err != nil {
		return GateState{}, fmt.Errorf("query %s: %w", workflowID, err)
	}
	var s GateState
	if err := encoded.Get(&s); err != nil {
		return GateState{}, fmt.Errorf("decode the gatekeeper's state: %w", err)
	}
	return s, nil
}

// awaitGate polls the gatekeeper's state until it satisfies want.
func awaitGate(ctx context.Context, c client.Client, workflowID string, queries *int, want func(GateState) bool, timeout time.Duration) (GateState, error) {
	deadline := time.Now().Add(timeout)
	for {
		s, err := queryGate(ctx, c, workflowID, queries)
		if err != nil {
			return GateState{}, err
		}
		if want(s) {
			return s, nil
		}
		if !time.Now().Before(deadline) {
			return s, fmt.Errorf("the gatekeeper's state never met the condition within %s (state: %+v)", timeout, s)
		}
		select {
		case <-ctx.Done():
			return GateState{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// walkGatekeeper follows the gatekeeper from its first run through every run it
// continued as new into, counting the runs and the signals they received.
func walkGatekeeper(ctx context.Context, c client.Client, workflowID, firstRunID string) (int, int, bool, error) {
	runs, signals, continued := 0, 0, false
	for runID := firstRunID; runID != ""; {
		events, err := harness.History(ctx, c, workflowID, runID)
		if err != nil {
			return 0, 0, false, err
		}
		runs++
		runID = ""
		for _, event := range events {
			switch harness.EventName(event) {
			case "EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED":
				signals++
			case "EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW":
				continued = true
				runID = event.GetWorkflowExecutionContinuedAsNewEventAttributes().GetNewExecutionRunId()
			}
		}
	}
	return runs, signals, continued, nil
}

// walkIssues reads the capped run's issue workflows and counts what the admission cost
// them: the grants they were signalled, and the timers their waits started.
func walkIssues(ctx context.Context, c client.Client, stamp int64, count int) (int, int, error) {
	grants, timers := 0, 0
	for i := 1; i <= count; i++ {
		events, err := harness.History(ctx, c, fmt.Sprintf("exp5-issue-%d-%03d", stamp, i), "")
		if err != nil {
			return 0, 0, err
		}
		for _, event := range events {
			switch harness.EventName(event) {
			case "EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED":
				grants++
			case "EVENT_TYPE_TIMER_STARTED":
				timers++
			}
		}
	}
	return grants, timers, nil
}
