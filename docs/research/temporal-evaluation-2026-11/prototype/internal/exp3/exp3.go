// Package exp3 is experiment 3 of the Temporal evaluation: what a workflow sees when the
// worker process holding its session is killed.
//
// A Legion step is a prompt into a session that already holds state, so the question is
// whether Temporal tells the workflow that the session's host died, or quietly runs the
// work again somewhere else. The workflow here creates a Temporal session, runs one
// long activity on it, and records verbatim whatever the wait on that activity ended
// with; the driver in driver.go kills the worker while that activity runs.
//
// The workflow and the activities live together because both cmd/experiments (which
// hosts the workflow) and cmd/sessionworker (which hosts the activities) need them.
package exp3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The registered names of this experiment's pieces. They are strings so the workflow can
// name an activity that only the session worker's process has the code for.
const (
	WorkflowName        = "exp3SessionWorkflow"
	SessionActivityName = "exp3HoldSession"
	ProbeActivityName   = "exp3Probe"
)

// Params configures one run of SessionWorkflow.
type Params struct {
	// TaskQueue is the base task queue the session workers poll. The session machinery
	// derives its own two queues from it.
	TaskQueue string
	// Hold is how long the session activity occupies its worker. It must outlast the
	// kill the driver sends, so that the kill lands inside the activity.
	Hold time.Duration
	// CreationTimeout bounds how long creating the session may take.
	CreationTimeout time.Duration
	// ExecutionTimeout bounds the session's whole life.
	ExecutionTimeout time.Duration
	// HeartbeatTimeout is how long the server waits for a heartbeat before it declares
	// the session's host gone. It is what turns a killed process into a failed session.
	HeartbeatTimeout time.Duration
}

// Result is what the workflow saw, in the words the workflow saw it in. The driver
// prints these, and the write-up quotes them: the error text is the evidence.
type Result struct {
	// SessionCreated reports whether CreateSession returned a session at all.
	SessionCreated bool
	// CreateError is CreateSession's error, when it returned one.
	CreateError string
	// SessionID and SessionHost identify the session and the worker that took it.
	SessionID   string
	SessionHost string

	// ActivityResult is what the session activity returned when it ran to the end,
	// which only happens when nothing killed its worker.
	ActivityResult string
	// WaitErrorType and WaitErrorText are the Go type and the message of the error the
	// workflow's wait on the session activity ended with.
	WaitErrorType string
	WaitErrorText string
	// WaitIsSessionFailed reports whether that error is workflow.ErrSessionFailed.
	WaitIsSessionFailed bool
	// WaitIsCanceled reports whether that error is a cancellation, which is what the
	// SDK turns a failed session into: it cancels the session's context, and with it
	// every activity running on the session.
	WaitIsCanceled bool

	// SessionStateAfter is the session's state once the wait ended:
	// 0 open, 1 failed, 2 closed.
	SessionStateAfter int
	// SessionCtxError is the session context's own error once the wait ended, which
	// says whether the cancellation came from the session rather than from elsewhere.
	SessionCtxError string

	// ProbeScheduled reports whether the probe activity actually ran. A failed session
	// answers the probe without scheduling anything, so a probe that ran means the
	// session was still alive.
	ProbeScheduled bool
	// ProbeErrorType, ProbeErrorText and ProbeIsSessionFailed describe what asking the
	// same session for one more activity returned.
	ProbeErrorType       string
	ProbeErrorText       string
	ProbeIsSessionFailed bool
}

// SessionWorkflow creates a session on the worker task queue, runs one long activity on
// it, then asks the same session for one more activity. It returns what it saw and never
// fails: a failed session is the measurement, not an error of the run.
func SessionWorkflow(ctx workflow.Context, p Params) (*Result, error) {
	res := &Result{}

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              p.TaskQueue,
		ScheduleToStartTimeout: p.CreationTimeout,
		StartToCloseTimeout:    p.ExecutionTimeout,
		HeartbeatTimeout:       p.HeartbeatTimeout,
	})

	sessionCtx, err := workflow.CreateSession(ctx, &workflow.SessionOptions{
		CreationTimeout:  p.CreationTimeout,
		ExecutionTimeout: p.ExecutionTimeout,
		HeartbeatTimeout: p.HeartbeatTimeout,
	})
	if err != nil {
		res.CreateError = err.Error()
		return res, nil
	}
	defer workflow.CompleteSession(sessionCtx)

	res.SessionCreated = true
	if info := workflow.GetSessionInfo(sessionCtx); info != nil {
		res.SessionID, res.SessionHost = info.SessionID, info.HostName
	}

	var held string
	err = workflow.ExecuteActivity(sessionCtx, SessionActivityName, p.Hold).Get(sessionCtx, &held)
	if err != nil {
		var canceled *temporal.CanceledError
		res.WaitErrorType = fmt.Sprintf("%T", err)
		res.WaitErrorText = err.Error()
		res.WaitIsSessionFailed = errors.Is(err, workflow.ErrSessionFailed)
		res.WaitIsCanceled = errors.As(err, &canceled)
	} else {
		res.ActivityResult = held
	}
	if info := workflow.GetSessionInfo(sessionCtx); info != nil {
		res.SessionStateAfter = int(info.SessionState)
	}
	if ctxErr := sessionCtx.Err(); ctxErr != nil {
		res.SessionCtxError = ctxErr.Error()
	}

	// Ask the same session for one more activity. The SDK answers ErrSessionFailed
	// without scheduling anything once the session is failed, so this separates "the
	// session is gone" from "this one activity happened to fail".
	var probed string
	probeErr := workflow.ExecuteActivity(sessionCtx, ProbeActivityName).Get(sessionCtx, &probed)
	res.ProbeScheduled = probed != ""
	if probeErr != nil {
		res.ProbeErrorType = fmt.Sprintf("%T", probeErr)
		res.ProbeErrorText = probeErr.Error()
		res.ProbeIsSessionFailed = errors.Is(probeErr, workflow.ErrSessionFailed)
	}
	return res, nil
}

// HoldSession is the session-bound activity: it occupies its worker for hold,
// heartbeating all the while, so that killing the worker interrupts a running activity
// rather than an idle one. It returns the process that ran it.
func HoldSession(ctx context.Context, hold time.Duration) (string, error) {
	stamp := Stamp()
	deadline := time.Now().Add(hold)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			activity.RecordHeartbeat(ctx, stamp)
			if !time.Now().Before(deadline) {
				return stamp, nil
			}
		}
	}
}

// Probe is the short activity the workflow runs to ask whether its session still takes
// work. It returns the process that ran it.
func Probe(_ context.Context) (string, error) {
	return Stamp(), nil
}

// Stamp names the process running an activity, so the output says which worker did what.
func Stamp() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("pid=%d host=%s", os.Getpid(), host)
}
