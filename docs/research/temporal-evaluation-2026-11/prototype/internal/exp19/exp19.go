// Package exp19 is experiment 19 of the Temporal evaluation: Asynchronous Activity
// Completion, the state an activity is in once its body has handed the work out.
//
// Legion's unit of work is a prompt into a live session: something outside the engine
// takes it, nobody knows yet whether it will succeed, and running it a second time is
// not the same as running it once. Temporal's mechanism for that shape is an activity
// whose body returns activity.ErrResultPending: the worker is free again, the server
// keeps the Activity Execution started, and whoever holds the task token completes it
// later from any process.
//
// The experiment measures four things about that state, which no other experiment of
// this evaluation touches: what the server does with such an activity when the worker
// that took it is killed and replaced, what a process holding only the token can do
// with it, what the server answers to a token from an attempt that has been
// superseded, and what happens when nobody ever completes it.
//
// The workflow and the activity live together because both the driver process (which
// hosts the workflow) and the worker processes the driver starts (which host the
// activity, and one of which is killed) need them.
//
// Experiment 20 is this experiment's case A held open for minutes rather than
// seconds, so it imports this package instead of repeating it: the workflow, the
// activity, the two helper roles, the worker processes and the readings taken of the
// server are all this package's, and exp20 holds only what it measures differently.
package exp19

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The registered names of this experiment's pieces. They are strings so the workflow
// can name an activity that only the worker processes have registered.
const (
	WorkflowName        = "exp19HandoutWorkflow"
	HandoutActivityName = "exp19Handout"
)

// Params is one handout: where the activity runs, where it writes what it did, and the
// two bounds Temporal puts on it.
type Params struct {
	// TaskQueue is the queue the handout activity is dispatched to, which the worker
	// processes poll and the driver's own worker does not.
	TaskQueue string
	// EffectFile gets one line per execution of the activity's body. Its line count is
	// how many times the body ran, which is the reading that separates "handed out and
	// left alone" from "handed out and run again somewhere else".
	EffectFile string
	// TokenFile gets one line per execution too, holding that execution's task token:
	// the handle a process outside the worker completes the activity with.
	TokenFile string
	// StartToClose is how long the server lets the Activity Execution stay started
	// before it declares it timed out. For a handed-out activity it is the bound on
	// how long the outside world has to answer.
	StartToClose time.Duration
	// MaxAttempts is the retry policy's bound.
	MaxAttempts int32
	// FailFirstAttempt makes the first attempt record its token and then fail, so a
	// second attempt runs and the first attempt's token becomes a stale one.
	FailFirstAttempt bool
}

// Result is what the workflow saw, in the words it saw it in. A failed activity is a
// measurement here rather than an error of the run, so the workflow records the error
// instead of returning it.
type Result struct {
	// Completion is what the activity's eventual completion delivered to the workflow.
	Completion string
	// Failed reports that the wait on the activity ended in an error.
	Failed bool
	// ErrorType and ErrorText are the Go type and the message of that error.
	ErrorType string
	ErrorText string
	// IsTimeout and TimeoutType say whether that error was a timeout and which one.
	IsTimeout   bool
	TimeoutType string
	// RetryState is what the server said about the retries when the activity ended.
	RetryState string
}

// HandoutWorkflow schedules the handout activity and records whatever the wait on it
// ended with: a result somebody else delivered, or the error the server raised.
func HandoutWorkflow(ctx workflow.Context, p Params) (*Result, error) {
	res := &Result{}
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           p.TaskQueue,
		StartToCloseTimeout: p.StartToClose,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    200 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumAttempts:    p.MaxAttempts,
		},
	})

	var completion string
	err := workflow.ExecuteActivity(actx, HandoutActivityName, p).Get(actx, &completion)
	if err == nil {
		res.Completion = completion
		return res, nil
	}
	res.Failed = true
	res.ErrorType = fmt.Sprintf("%T", err)
	res.ErrorText = err.Error()
	var activityErr *temporal.ActivityError
	if errors.As(err, &activityErr) {
		res.RetryState = activityErr.RetryState().String()
	}
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		res.IsTimeout = true
		res.TimeoutType = timeoutErr.TimeoutType().String()
	}
	return res, nil
}

// Handout is the activity that hands its work out. It records that its body ran and
// the task token this execution was given, then returns without completing the
// Activity Execution, which is what activity.ErrResultPending means: the worker has
// nothing more to do with it and the server waits for somebody holding that token.
//
// With FailFirstAttempt it fails the first attempt instead, so that the token that
// attempt recorded belongs to an attempt the server has moved on from.
func Handout(ctx context.Context, p Params) (string, error) {
	info := activity.GetInfo(ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := appendLine(p.EffectFile, fmt.Sprintf("attempt=%d pid=%d at=%s", info.Attempt, os.Getpid(), now)); err != nil {
		return "", err
	}
	if err := appendLine(p.TokenFile, fmt.Sprintf("attempt=%d token=%s", info.Attempt, base64.StdEncoding.EncodeToString(info.TaskToken))); err != nil {
		return "", err
	}
	if p.FailFirstAttempt && info.Attempt == 1 {
		return "", temporal.NewApplicationError(
			fmt.Sprintf("attempt %d recorded its task token and then failed, so the server starts another attempt", info.Attempt),
			"HandoutAttemptFailed")
	}
	return "", activity.ErrResultPending
}

// RegisterWorkflow puts the workflow on the driver's own worker. The activity is left
// off it on purpose: the body must run only in the worker processes the driver starts
// and kills, or killing one of them would measure nothing.
func RegisterWorkflow(w worker.Worker) {
	w.RegisterWorkflowWithOptions(HandoutWorkflow, workflow.RegisterOptions{Name: WorkflowName})
}

// RegisterActivity puts the handout activity on a worker process.
func RegisterActivity(w worker.Worker) {
	w.RegisterActivityWithOptions(Handout, activity.RegisterOptions{Name: HandoutActivityName})
}

// Token is one line of a token file: the task token one execution of the activity was
// given, as that execution wrote it down.
type Token struct {
	Attempt int32
	Token   []byte
}

// Tokens is every token the activity has written to path so far, oldest first. A file
// that is not there yet holds none, which is not an error: it does not exist before the
// first execution writes to it.
func Tokens(path string) ([]Token, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	tokens := make([]Token, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "attempt=") || !strings.HasPrefix(fields[1], "token=") {
			return nil, fmt.Errorf("%s holds the line %q, which is not `attempt=N token=BASE64`", path, line)
		}
		attempt, err := strconv.Atoi(strings.TrimPrefix(fields[0], "attempt="))
		if err != nil {
			return nil, fmt.Errorf("%s holds the line %q, whose attempt is not a number: %w", path, line, err)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fields[1], "token="))
		if err != nil {
			return nil, fmt.Errorf("%s holds the line %q, whose token is not base64: %w", path, line, err)
		}
		tokens = append(tokens, Token{Attempt: int32(attempt), Token: raw})
	}
	return tokens, nil
}

// TokenForAttempt is the token one numbered attempt recorded, and an error when that
// attempt wrote none.
func TokenForAttempt(path string, attempt int32) ([]byte, error) {
	tokens, err := Tokens(path)
	if err != nil {
		return nil, err
	}
	for _, token := range tokens {
		if token.Attempt == attempt {
			return token.Token, nil
		}
	}
	return nil, fmt.Errorf("%s holds no token for attempt %d", path, attempt)
}

// Lines is how many lines a file holds, and none for a file that is not there yet. The
// side-effect file's count is how many times the activity's body ran.
func Lines(path string) (int, error) {
	lines, err := readLines(path)
	return len(lines), err
}

// WaitForLines blocks until a file holds at least want lines, and returns the lines it
// holds. It fails when timeout passes first, naming how many lines it did see.
func WaitForLines(ctx context.Context, path string, want int, timeout time.Duration) ([]string, error) {
	deadline := time.Now().Add(timeout)
	for {
		lines, err := readLines(path)
		if err != nil {
			return nil, err
		}
		if len(lines) >= want {
			return lines, nil
		}
		if !time.Now().Before(deadline) {
			return lines, fmt.Errorf("%s held %d line(s), not %d, after %s", path, len(lines), want, timeout)
		}
		select {
		case <-ctx.Done():
			return lines, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// readLines is every non-empty line of a file, and no lines at all when the file does
// not exist.
func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return lines, nil
}

// appendLine adds one line to a file, creating it if it is not there yet, and pushes it
// out of this process: the driver is another process and reads these files while the
// run is going on.
func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := fmt.Fprintln(file, line); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return file.Close()
}
