package exp4

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

const (
	dialTimeout     = 30 * time.Second
	workflowTimeout = 90 * time.Second
	waitTimeout     = 60 * time.Second
	attemptWindow   = 10 * time.Second
	// maxAttempts is the poison delivery's bound. Three is enough to show the bound
	// binding and short enough to keep the run quick.
	maxAttempts = 3
)

// Run is experiment 4: an idempotent delivery that Temporal retries after it crashed
// past its own write, and a delivery that can never succeed and must end somewhere.
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
	queue := fmt.Sprintf("exp4-%d", stamp)
	w := worker.New(c, queue, worker.Options{})
	Register(w)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	params := Params{
		Key:           fmt.Sprintf("exp4-pr-comment-%d", stamp),
		PoisonKey:     fmt.Sprintf("exp4-poison-%d", stamp),
		Ledger:        filepath.Join(workDir, fmt.Sprintf("exp4-ledger-%d", stamp)),
		Calls:         filepath.Join(workDir, fmt.Sprintf("exp4-calls-%d", stamp)),
		MaxAttempts:   maxAttempts,
		AttemptWindow: attemptWindow,
	}
	harness.Fact("idempotency-key", params.Key)
	harness.Fact("poison-key", params.PoisonKey)
	harness.Fact("outside-system-ledger", params.Ledger)
	harness.Fact("activity-call-log", params.Calls)

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp4-delivery-%d", stamp),
		TaskQueue:                queue,
		WorkflowExecutionTimeout: workflowTimeout,
	}, WorkflowName, params)
	if err != nil {
		return fmt.Errorf("start workflow: %w", err)
	}
	harness.Fact("workflow-id", run.GetID())

	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	var res Result
	if err := run.Get(waitCtx, &res); err != nil {
		return fmt.Errorf("workflow %s did not return: %w", run.GetID(), err)
	}

	calls, err := CountCalls(params.Calls, params.Key)
	if err != nil {
		return err
	}
	records, err := CountRecords(params.Ledger, params.Key)
	if err != nil {
		return err
	}
	poisonCalls, err := CountCalls(params.Calls, params.PoisonKey)
	if err != nil {
		return err
	}
	poisonRecords, err := CountRecords(params.Ledger, params.PoisonKey)
	if err != nil {
		return err
	}

	harness.Fact("idempotent-delivery-activity-calls", calls)
	harness.Fact("idempotent-delivery-records-written", records)
	harness.Fact("idempotent-delivery-outcome", res.DeliveryOutcome)
	harness.Fact("idempotent-delivery-succeeded-on-attempt", res.DeliveryAttempt)

	harness.Fact("poison-delivery-max-attempts-allowed", maxAttempts)
	harness.Fact("poison-delivery-activity-calls", poisonCalls)
	harness.Fact("poison-delivery-records-written", poisonRecords)
	harness.Fact("poison-delivery-error-type", res.TerminalErrorType)
	harness.Fact("poison-delivery-error-is-application-error", res.IsApplicationError)
	harness.Fact("poison-delivery-error-is-timeout", res.IsTimeout)
	harness.Fact("poison-delivery-application-error-type", res.ApplicationErrorType)
	harness.Fact("poison-delivery-application-error-message", res.ApplicationErrorMessage)
	harness.Fact("poison-delivery-retry-state", res.TerminalRetryState)
	harness.Fact("poison-delivery-error", res.TerminalErrorText)
	harness.Fact("workflow-continued-to-handler", res.Routed)
	harness.Fact("dead-letter-record", res.DeadLetter)

	events, err := harness.History(ctx, c, run.GetID(), run.GetRunID())
	if err != nil {
		return err
	}
	var attempts []int32
	scheduled, completed, failed := 0, 0, 0
	for _, event := range events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			scheduled++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
			attempts = append(attempts, event.GetActivityTaskStartedEventAttributes().GetAttempt())
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			completed++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			failed++
		}
	}
	desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		return fmt.Errorf("describe %s: %w", run.GetID(), err)
	}
	status := desc.GetWorkflowExecutionInfo().GetStatus()
	harness.Fact("history-event-count", len(events))
	harness.Fact("history-activities-scheduled", scheduled)
	harness.Fact("history-activities-completed", completed)
	harness.Fact("history-activities-failed", failed)
	harness.Fact("history-attempt-of-each-activity-start-in-order", attempts)
	harness.Fact("workflow-status", status.String())
	harness.Fact("limit", "one process, one file standing in for GitHub and Dispatch, three attempts: it shows that Temporal's retry plus an idempotency key writes once and that a bounded policy hands the workflow a routable error, not that Legion's real deliveries are idempotent or that its dead letters would be read")

	switch {
	case calls < 2:
		return fmt.Errorf("the idempotent delivery was called %d time(s), so no retry was exercised", calls)
	case records != 1:
		return fmt.Errorf("the ledger holds %d records for %s, not exactly one", records, params.Key)
	case res.DeliveryOutcome != "already-delivered":
		return fmt.Errorf("the retry reported %q, so it did not find its own earlier write", res.DeliveryOutcome)
	case poisonCalls != maxAttempts:
		return fmt.Errorf("the poison delivery was called %d time(s), not the %d its policy allowed", poisonCalls, maxAttempts)
	case res.TerminalRetryState != enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED.String():
		return fmt.Errorf("the poison delivery ended in retry state %q, not maximum attempts reached", res.TerminalRetryState)
	case !res.IsApplicationError || res.ApplicationErrorType != "DeliveryRefused":
		return fmt.Errorf("the workflow got %s, not the application error the activity raised", res.TerminalErrorType)
	case !res.Routed || res.DeadLetter == "":
		return fmt.Errorf("the workflow did not reach its handler after the terminal error")
	case status != enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return fmt.Errorf("the workflow ended %s, so it did not carry on past the failed delivery", status)
	}
	return nil
}
