package exp2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

const (
	dialTimeout     = 30 * time.Second
	workflowTimeout = 90 * time.Second
	waitTimeout     = 60 * time.Second
	// idleObserve is how long the gate is watched doing nothing before the first
	// approval, which is the evidence that an open-ended wait holds no worker.
	idleObserve = 3 * time.Second
	// confirmTimeout bounds the confirmation step inside the workflow.
	confirmTimeout = 30 * time.Second
	// pollInterval is how often the driver asks the server what the gate is doing.
	pollInterval = 150 * time.Millisecond
	// settle is how long the driver gives the gate to take a signal in before reading
	// the state that signal produced.
	settle = 2 * time.Second

	// activeStoragePricePerGBHour is Temporal Cloud's published Active Storage price.
	// It is a published number, not something this run measured, so every line derived
	// from it is printed as reasoned rather than timed.
	activeStoragePricePerGBHour = 0.042
)

// Run is experiment 2: a gate that waits for a person, is closed again by a new document
// version, and opens only on the approval of the version that stands.
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

	stamp := time.Now().UnixNano()
	queue := fmt.Sprintf("exp2-%d", stamp)
	w := worker.New(c, queue, worker.Options{})
	Register(w)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	release := filepath.Join(workDir, fmt.Sprintf("exp2-release-%d", stamp))
	defer os.Remove(release)
	issueID := fmt.Sprintf("EXP2-%d", stamp)
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp2-gate-%d", stamp),
		TaskQueue:                queue,
		WorkflowExecutionTimeout: workflowTimeout,
	}, WorkflowName, Params{IssueID: issueID, ReleaseFile: release, ConfirmTimeout: confirmTimeout})
	if err != nil {
		return fmt.Errorf("start workflow: %w", err)
	}
	harness.Fact("issue-id", issueID)
	harness.Fact("workflow-id", run.GetID())
	harness.Fact("release-file", release)

	queries := 0
	signals := 0

	// 1. The wait itself: the gate is open and holds nothing.
	idle, err := observeIdle(ctx, c, run, idleObserve)
	if err != nil {
		return err
	}
	harness.Fact("gate-status-while-waiting", idle.status)
	harness.Fact("gate-pending-activities-while-waiting", idle.pendingActivities)
	harness.Fact("gate-pending-children-while-waiting", idle.pendingChildren)
	harness.Fact("gate-pending-workflow-task-while-waiting", idle.pendingWorkflowTask)
	harness.Fact("gate-last-history-event-while-waiting", idle.lastEvent)
	harness.Fact("gate-history-events-while-waiting", idle.events)
	harness.Fact("gate-observed-idle-for", idleObserve)
	before, err := queryState(ctx, c, run, &queries)
	if err != nil {
		return err
	}
	harness.Fact("gate-phase-before-any-approval", before.Phase)

	// 2. The first approval, of version 1, and the version that lands inside the
	// confirmation step it starts.
	if err := c.SignalWorkflow(ctx, run.GetID(), "", ApprovalSignal, 1); err != nil {
		return fmt.Errorf("send the first approval: %w", err)
	}
	signals++
	if err := waitPendingActivity(ctx, c, run, ConfirmActivityName, waitTimeout); err != nil {
		return err
	}
	if err := c.SignalWorkflow(ctx, run.GetID(), "", VersionSignal, 2); err != nil {
		return fmt.Errorf("send the new document version: %w", err)
	}
	signals++
	if err := os.WriteFile(release, []byte("released\n"), 0o644); err != nil {
		return fmt.Errorf("write the release file: %w", err)
	}

	// 3. What the first approval bought: nothing. The new version withdrew it.
	withdrawn, err := awaitState(ctx, c, run, &queries, func(s State) bool { return s.Withdrawals > 0 }, waitTimeout)
	if err != nil {
		return fmt.Errorf("the gate never recorded the withdrawal: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	after, err := queryState(ctx, c, run, &queries)
	if err != nil {
		return err
	}
	harness.Fact("approvals-after-the-first-approval", after.Approvals)
	harness.Fact("withdrawals-after-the-first-approval", after.Withdrawals)
	harness.Fact("confirmation-steps-run", after.Confirms)
	harness.Fact("gate-phase-after-the-first-approval", after.Phase)
	harness.Fact("next-phase-started-after-the-first-approval", after.NextPhaseStarted != "")
	stillIdle, err := observeIdle(ctx, c, run, 0)
	if err != nil {
		return err
	}
	harness.Fact("gate-pending-activities-after-the-first-approval", stillIdle.pendingActivities)

	// 4. The second approval, of the version that stands.
	if err := c.SignalWorkflow(ctx, run.GetID(), "", ApprovalSignal, 2); err != nil {
		return fmt.Errorf("send the second approval: %w", err)
	}
	signals++

	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	var final State
	if err := run.Get(waitCtx, &final); err != nil {
		return fmt.Errorf("gate %s did not return: %w", run.GetID(), err)
	}
	harness.Fact("gate-phase-final", final.Phase)
	harness.Fact("approvals-total", final.Approvals)
	harness.Fact("versions-total", final.Versions)
	harness.Fact("withdrawals-total", final.Withdrawals)
	harness.Fact("confirmation-steps-total", final.Confirms)
	harness.Fact("advanced-after-approval-number", final.AdvancedAfterApproval)
	harness.Fact("next-phase-prompt", final.NextPhaseStarted)

	events, err := harness.History(ctx, c, run.GetID(), run.GetRunID())
	if err != nil {
		return err
	}
	lastSignal, nextPhaseScheduled := int64(0), int64(0)
	for _, event := range events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
			lastSignal = event.GetEventId()
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			if event.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName() == NextPhaseActivityName && nextPhaseScheduled == 0 {
				nextPhaseScheduled = event.GetEventId()
			}
		}
	}
	harness.Fact("last-signal-event-id", lastSignal)
	harness.Fact("next-phase-scheduled-event-id", nextPhaseScheduled)

	if err := reportCost(ctx, c, cfg, run, len(events), signals, queries); err != nil {
		return err
	}
	harness.Fact("limit", "one gate on one dev server, waiting seconds rather than days: it shows a blocked workflow holds no worker and that a new version withdraws an approval, while the cost of a real multi-day wait is arithmetic on this run's bytes, not a wait this pod sat through")

	switch {
	case idle.status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String():
		return fmt.Errorf("the gate was %s, not running, while it waited", idle.status)
	case idle.pendingActivities != 0 || idle.pendingChildren != 0:
		return fmt.Errorf("the waiting gate held %d activities and %d children, not nothing", idle.pendingActivities, idle.pendingChildren)
	case withdrawn.Withdrawals != 1 || after.Withdrawals != 1:
		return fmt.Errorf("the new version withdrew %d approvals, not exactly one", after.Withdrawals)
	case after.Phase != PhaseAwaitingApproval || after.NextPhaseStarted != "":
		return fmt.Errorf("the first approval moved the issue to %s already", after.Phase)
	case final.Phase != PhaseNext:
		return fmt.Errorf("the gate ended in %s, not %s", final.Phase, PhaseNext)
	case final.AdvancedAfterApproval != 2:
		return fmt.Errorf("the gate opened on approval %d, not on the second", final.AdvancedAfterApproval)
	case nextPhaseScheduled == 0 || nextPhaseScheduled < lastSignal:
		return fmt.Errorf("the next phase was scheduled at event %d, not after the last signal at event %d", nextPhaseScheduled, lastSignal)
	}
	return nil
}

// reportCost prints what a wait of days costs. Every line says which half it belongs to:
// timed: is a number this run measured; reasoned: is arithmetic on a published price
// over a span of days no pod can sit through.
func reportCost(ctx context.Context, c client.Client, cfg harness.Config, run client.WorkflowRun, events, signals, queries int) error {
	desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		return fmt.Errorf("describe %s: %w", run.GetID(), err)
	}
	historyBytes := desc.GetWorkflowExecutionInfo().GetHistorySizeBytes()
	ns, err := c.WorkflowService().DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{Namespace: cfg.Namespace})
	if err != nil {
		return fmt.Errorf("describe namespace %s: %w", cfg.Namespace, err)
	}
	retention := ns.GetConfig().GetWorkflowExecutionRetentionTtl().AsDuration()

	harness.Fact("timed: gate-history-event-count", events)
	harness.Fact("timed: gate-history-size-bytes", historyBytes)
	harness.Fact("timed: gate-history-size-bytes-source", "DescribeWorkflowExecution.workflowExecutionInfo.historySizeBytes")
	harness.Fact("timed: signals-sent", signals)
	harness.Fact("timed: actions-from-signals", signals)
	harness.Fact("timed: actions-rule-applied", "one Action per signal (Temporal Cloud's published rule)")
	harness.Fact("timed: queries-sent-by-this-driver", queries)
	harness.Fact("timed: namespace-retention-hours", retention.Hours())
	harness.Fact("timed: wall-clock-the-gate-actually-waited", idleObserve.String())

	gb := float64(historyBytes) / 1e9
	for _, days := range []int{1, 7, 30} {
		hours := float64(days) * 24
		harness.Fact(fmt.Sprintf("reasoned: active-storage-usd-for-one-gate-open-%d-days", days),
			fmt.Sprintf("%.9f (%d bytes x %.0f h x $%.3f per GB-hour)", gb*hours*activeStoragePricePerGBHour, historyBytes, hours, activeStoragePricePerGBHour))
	}
	harness.Fact("reasoned: active-storage-usd-for-1000-gates-open-30-days",
		fmt.Sprintf("%.6f (1000 x %d bytes x 720 h x $%.3f per GB-hour)", 1000*gb*720*activeStoragePricePerGBHour, historyBytes, activeStoragePricePerGBHour))
	harness.Fact("reasoned: active-storage-usd-per-gate-for-the-namespace-retention-after-it-closes",
		fmt.Sprintf("%.9f (%d bytes x %.0f h x $%.3f per GB-hour)", gb*retention.Hours()*activeStoragePricePerGBHour, historyBytes, retention.Hours(), activeStoragePricePerGBHour))
	harness.Fact("reasoned: actions-while-waiting",
		"zero: a workflow blocked on a signal with no timer running runs no workflow task and schedules nothing, so the days it waits cost Actions only through the signals that end the wait")
	harness.Fact("reasoned: what-a-pod-cannot-do",
		"this run waited seconds; the lines above extend its measured bytes over days by arithmetic, and nothing here observed a workflow that really stood for days")
	return nil
}

// gateIdle is what the server says a waiting gate holds.
type gateIdle struct {
	status              string
	pendingActivities   int
	pendingChildren     int
	pendingWorkflowTask string
	lastEvent           string
	events              int
}

// observeIdle watches the gate for a span and reports what the server said about it at
// the end of that span, with one exception: the activities and children it reports are
// the most it held at any moment of the span, so a gate that briefly held something
// cannot look idle because the driver happened to look at a quiet instant.
func observeIdle(ctx context.Context, c client.Client, run client.WorkflowRun, span time.Duration) (gateIdle, error) {
	deadline := time.Now().Add(span)
	peakActivities, peakChildren := 0, 0
	for {
		desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
		if err != nil {
			return gateIdle{}, fmt.Errorf("describe %s: %w", run.GetID(), err)
		}
		seen := gateIdle{
			status:              desc.GetWorkflowExecutionInfo().GetStatus().String(),
			pendingWorkflowTask: "none",
		}
		if len(desc.GetPendingActivities()) > peakActivities {
			peakActivities = len(desc.GetPendingActivities())
		}
		if len(desc.GetPendingChildren()) > peakChildren {
			peakChildren = len(desc.GetPendingChildren())
		}
		seen.pendingActivities, seen.pendingChildren = peakActivities, peakChildren
		if task := desc.GetPendingWorkflowTask(); task != nil {
			seen.pendingWorkflowTask = task.GetState().String()
		}
		if !time.Now().Before(deadline) {
			// Only the pass that ends the span is reported, so the history is read
			// once, here, rather than on every poll.
			events, err := harness.History(ctx, c, run.GetID(), run.GetRunID())
			if err != nil {
				return gateIdle{}, err
			}
			seen.events = len(events)
			if len(events) > 0 {
				seen.lastEvent = harness.EventName(events[len(events)-1])
			}
			return seen, nil
		}
		select {
		case <-ctx.Done():
			return gateIdle{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// waitPendingActivity blocks until the named activity of the run is started on a worker.
func waitPendingActivity(ctx context.Context, c client.Client, run client.WorkflowRun, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
		if err != nil {
			return fmt.Errorf("describe %s: %w", run.GetID(), err)
		}
		for _, pending := range desc.GetPendingActivities() {
			if pending.GetActivityType().GetName() == name && pending.GetState() == enumspb.PENDING_ACTIVITY_STATE_STARTED {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("activity %s was not running after %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// queryState asks the gate what it has seen, counting the query: a query is an Action
// under Temporal Cloud's published rule, so the experiment may not make them silently.
func queryState(ctx context.Context, c client.Client, run client.WorkflowRun, queries *int) (State, error) {
	*queries++
	encoded, err := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), StateQuery)
	if err != nil {
		return State{}, fmt.Errorf("query %s: %w", run.GetID(), err)
	}
	var s State
	if err := encoded.Get(&s); err != nil {
		return State{}, fmt.Errorf("decode the gate's state: %w", err)
	}
	return s, nil
}

// awaitState polls the gate's state until it satisfies want, or the timeout passes.
func awaitState(ctx context.Context, c client.Client, run client.WorkflowRun, queries *int, want func(State) bool, timeout time.Duration) (State, error) {
	deadline := time.Now().Add(timeout)
	for {
		s, err := queryState(ctx, c, run, queries)
		if err != nil {
			return State{}, err
		}
		if want(s) {
			return s, nil
		}
		if !time.Now().Before(deadline) {
			return s, fmt.Errorf("the gate's state never met the condition within %s (state: %+v)", timeout, s)
		}
		select {
		case <-ctx.Done():
			return State{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
