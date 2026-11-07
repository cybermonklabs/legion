// Package exp4 is experiment 4 of the Temporal evaluation: delivery to an outside
// system.
//
// Legion's outbox delivers to GitHub and Dispatch. Two things decide whether a durable
// engine helps there: whether a retry can repeat a call without repeating its effect,
// and what happens to a delivery that will never succeed. The first is the activity's
// own job -- Temporal retries, the idempotency key decides -- and the second is a path
// Legion's outbox has none of: a delivery that exhausts its attempts and is routed
// somewhere a person can see it, rather than retried for ever.
package exp4

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The registered names of this experiment's pieces.
const (
	WorkflowName           = "exp4DeliveryWorkflow"
	DeliverActivityName    = "exp4Deliver"
	DeadLetterActivityName = "exp4DeadLetter"
)

// Params is one run: two deliveries, one that crashes after its write and one that can
// never succeed.
type Params struct {
	// Key and PoisonKey are the idempotency keys of the two deliveries.
	Key       string
	PoisonKey string
	// Ledger is the file standing for the outside system's record: one line per
	// delivery it accepted, keyed by idempotency key.
	Ledger string
	// Calls is the file the activity appends to on every call, whatever it then does,
	// so the run can tell how often Temporal called it.
	Calls string
	// MaxAttempts bounds the poison delivery's retries.
	MaxAttempts int32
	// AttemptWindow bounds one attempt.
	AttemptWindow time.Duration
}

// Delivery is one call into the outside system.
type Delivery struct {
	Key    string
	Ledger string
	Calls  string
	// CrashAfterWrite makes the activity write the record and then fail, which is the
	// case an idempotency key exists for: the effect landed, the report of it did not.
	CrashAfterWrite bool
	// AlwaysRefuse makes the outside system refuse, however often it is asked.
	AlwaysRefuse bool
}

// Receipt is what a delivery that got through reports.
type Receipt struct {
	// Outcome is "delivered" when this call wrote the record, "already-delivered" when
	// it found the key already there.
	Outcome string
	// Attempt is the attempt number Temporal was on when the call succeeded.
	Attempt int32
}

// Result is what the workflow saw.
type Result struct {
	// The idempotent delivery.
	DeliveryOutcome string
	DeliveryAttempt int32

	// The delivery that can never succeed.
	TerminalErrorType       string
	TerminalErrorText       string
	TerminalRetryState      string
	ApplicationErrorType    string
	ApplicationErrorMessage string
	IsApplicationError      bool
	IsTimeout               bool

	// Routed reports that the workflow carried on after the terminal error and ran its
	// handler, rather than standing still.
	Routed     bool
	DeadLetter string
}

// DeliveryWorkflow runs both deliveries: the one whose retry must not write twice, and
// the one whose failure must end somewhere a person can act on.
func DeliveryWorkflow(ctx workflow.Context, p Params) (*Result, error) {
	res := &Result{}

	idempotent := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: p.AttemptWindow,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    200 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumAttempts:    5,
		},
	})
	var receipt Receipt
	if err := workflow.ExecuteActivity(idempotent, DeliverActivityName, Delivery{
		Key:             p.Key,
		Ledger:          p.Ledger,
		Calls:           p.Calls,
		CrashAfterWrite: true,
	}).Get(idempotent, &receipt); err != nil {
		return res, fmt.Errorf("the idempotent delivery never got through: %w", err)
	}
	res.DeliveryOutcome, res.DeliveryAttempt = receipt.Outcome, receipt.Attempt

	bounded := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: p.AttemptWindow,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    100 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumAttempts:    p.MaxAttempts,
		},
	})
	var refused Receipt
	err := workflow.ExecuteActivity(bounded, DeliverActivityName, Delivery{
		Key:          p.PoisonKey,
		Ledger:       p.Ledger,
		Calls:        p.Calls,
		AlwaysRefuse: true,
	}).Get(bounded, &refused)
	if err == nil {
		return res, fmt.Errorf("the delivery that can never succeed returned %+v, so this run saw no terminal failure", refused)
	}
	res.TerminalErrorType = fmt.Sprintf("%T", err)
	res.TerminalErrorText = err.Error()
	var activityErr *temporal.ActivityError
	if errors.As(err, &activityErr) {
		res.TerminalRetryState = activityErr.RetryState().String()
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		res.IsApplicationError = true
		res.ApplicationErrorType, res.ApplicationErrorMessage = appErr.Type(), appErr.Message()
	}
	var timeoutErr *temporal.TimeoutError
	res.IsTimeout = errors.As(err, &timeoutErr)

	// The dead-letter path: the workflow routes the failure and keeps going. Its own
	// retries are bounded too, so the handler cannot become the thing that is stuck.
	handler := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: p.AttemptWindow,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	if err := workflow.ExecuteActivity(handler, DeadLetterActivityName, p.PoisonKey, res.TerminalErrorText).Get(handler, &res.DeadLetter); err != nil {
		return res, fmt.Errorf("the dead-letter handler failed: %w", err)
	}
	res.Routed = true
	return res, nil
}

// Deliver is the call into the outside system. It records every call it is given, then
// behaves as its Delivery asks: refuse for ever, or write the record once under its
// idempotency key and crash before reporting that it did.
func Deliver(ctx context.Context, d Delivery) (Receipt, error) {
	attempt := activity.GetInfo(ctx).Attempt
	if err := appendLine(d.Calls, fmt.Sprintf("key=%s attempt=%d at=%s", d.Key, attempt, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return Receipt{}, err
	}
	if d.AlwaysRefuse {
		return Receipt{}, temporal.NewApplicationError(
			fmt.Sprintf("the outside system refused %s on attempt %d and will refuse every attempt", d.Key, attempt),
			"DeliveryRefused")
	}
	already, err := HasRecord(d.Ledger, d.Key)
	if err != nil {
		return Receipt{}, err
	}
	if already {
		return Receipt{Outcome: "already-delivered", Attempt: attempt}, nil
	}
	if err := appendLine(d.Ledger, fmt.Sprintf("%s delivered at=%s", d.Key, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return Receipt{}, err
	}
	if d.CrashAfterWrite {
		return Receipt{}, temporal.NewApplicationError(
			fmt.Sprintf("%s was written on attempt %d, then the delivery crashed before it could report success", d.Key, attempt),
			"CrashAfterWrite")
	}
	return Receipt{Outcome: "delivered", Attempt: attempt}, nil
}

// DeadLetter is where a delivery that can never succeed ends: a record a person can
// read, written once the workflow has given up on the delivery itself.
func DeadLetter(ctx context.Context, key, reason string) (string, error) {
	return fmt.Sprintf("dead-lettered key=%s attempt=%d reason=%s", key, activity.GetInfo(ctx).Attempt, reason), nil
}

// Register puts the workflow and its activities on a worker.
func Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(DeliveryWorkflow, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterActivityWithOptions(Deliver, activity.RegisterOptions{Name: DeliverActivityName})
	w.RegisterActivityWithOptions(DeadLetter, activity.RegisterOptions{Name: DeadLetterActivityName})
}

// HasRecord reports whether the ledger already holds a line for the key.
func HasRecord(path, key string) (bool, error) {
	count, err := CountRecords(path, key)
	return count > 0, err
}

// CountRecords is how many lines of the ledger belong to the key. One is what the
// experiment asserts after a retried delivery; two would be the duplicate an
// idempotency key exists to prevent.
func CountRecords(path, key string) (int, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), key+" ") {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return count, nil
}

// CountCalls is how many calls the activity recorded for a key.
func CountCalls(path, key string) (int, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "key="+key+" ") {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return count, nil
}

// appendLine adds one line to a file, creating it if it is not there yet.
func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, line); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
