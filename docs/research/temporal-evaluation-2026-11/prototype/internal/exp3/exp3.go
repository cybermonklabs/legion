// Package exp3 is experiment 3 of the Temporal evaluation: what Temporal does with a
// session-bound activity when the worker process holding that session is killed, while
// the workflow is still running and a replacement worker is polling.
//
// A Legion step is a prompt into a session that already holds state, so the question is
// whether Temporal tells the workflow that the session's host died, or quietly runs the
// work again somewhere else. The second half of that question is answerable only under
// three conditions, and the experiment is built around them:
//
//   - The workflow has to be still running while another worker could take the work.
//     This one creates a session, runs one long activity on it, probes the session once
//     the wait on that activity has ended, and then waits for the driver's release
//     signal, which the driver sends only after the replacement worker has polled. A
//     workflow that returned as soon as it had probed would measure its own ending.
//   - How often the activity's body ran has to be counted outside Temporal. The body
//     appends one line to a side-effect file every time it runs, as experiment 19's
//     does, and that file's line count is the reading. The history's
//     ActivityTaskStarted events are not: the server writes one only when an attempt
//     ends, so a killed attempt leaves none however long its body ran.
//   - The task queue the session's activities are dispatched to has to be visible,
//     because a queue named for the one worker process is the mechanism the write-up
//     gives as the reason nothing else can take the work. The driver reads that queue
//     from the history the server kept and compares it with the queue the replacement
//     worker polls.
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

	// ReleaseSignalName is the signal that lets the workflow end. It is what holds the
	// run open across the kill and the replacement worker: the workflow waits for it
	// after it has probed the failed session, and the driver sends it only once it has
	// finished watching.
	ReleaseSignalName = "exp3Release"

	// SessionCreationActivityName is the SDK's own activity, the one that takes a
	// session on a worker and heartbeats for as long as that worker lives. It is named
	// here because the driver reads the task queue it was scheduled on out of the
	// history, and because it is the activity whose missed heartbeat fails the session.
	SessionCreationActivityName = "internalSessionCreationActivity"

	// CreationQueueSuffix is what the SDK appends to the base task queue to get the
	// queue it dispatches the session creation activity to.
	CreationQueueSuffix = "__internal_session_creation"
)

// Params configures one run of SessionWorkflow.
type Params struct {
	// TaskQueue is the base task queue the session workers poll. The session machinery
	// derives its own two queues from it.
	TaskQueue string
	// Hold is how long the session activity occupies its worker. It must outlast the
	// kill the driver sends, so that the kill lands inside the activity.
	Hold time.Duration
	// EffectFile gets one line per execution of the session activity's body. Its line
	// count is how many times the body ran, which is the reading that separates "the
	// work was lost" from "the work was run again somewhere else".
	EffectFile string
	// HoldOpen bounds the wait for the release signal, so that a driver which dies
	// before sending it leaves no workflow running to the end of its execution
	// timeout. Reaching this bound means no signal came, which the workflow reports.
	HoldOpen time.Duration
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

	// ReleasedBy names what ended the wait that held the workflow open after the
	// probe: the driver's signal, or the workflow's own fallback timer.
	ReleasedBy string
	// ReleaseNote is what the driver's signal carried, which says what the driver had
	// finished watching when it let the workflow end.
	ReleaseNote string
	// HeldOpenFor is how long the workflow stayed running after it had probed the
	// session, which is the window every reading of the replacement worker was taken
	// in.
	HeldOpenFor time.Duration
}

// SessionWorkflow creates a session on the worker task queue, runs one long activity on
// it, asks the same session for one more activity, and then stays running until the
// driver releases it. It returns what it saw and never fails: a failed session is the
// measurement, not an error of the run.
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
	err = workflow.ExecuteActivity(sessionCtx, SessionActivityName, p).Get(sessionCtx, &held)
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

	// Stay running. Everything the experiment is cited for -- that no other worker
	// re-runs the killed session's activity -- is a statement about a live execution
	// with a replacement worker polling, so the workflow must outlast both. The wait
	// is on the workflow's own context, not the session's, which the failed session
	// has already cancelled.
	res.ReleasedBy, res.ReleaseNote, res.HeldOpenFor = holdOpen(ctx, p.HoldOpen)
	return res, nil
}

// holdOpen blocks until the driver's release signal arrives or bound passes, and
// reports which of the two it was, what the signal carried and how long the wait took.
func holdOpen(ctx workflow.Context, bound time.Duration) (string, string, time.Duration) {
	started := workflow.Now(ctx)
	releasedBy := fmt.Sprintf("the %s signal", ReleaseSignalName)
	var note string

	selector := workflow.NewSelector(ctx)
	selector.AddReceive(workflow.GetSignalChannel(ctx, ReleaseSignalName), func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, &note)
	})
	selector.AddFuture(workflow.NewTimer(ctx, bound), func(workflow.Future) {
		releasedBy = fmt.Sprintf("the workflow's own %s fallback timer: no %s signal arrived", bound, ReleaseSignalName)
	})
	selector.Select(ctx)

	return releasedBy, note, workflow.Now(ctx).Sub(started)
}

// HoldSession is the session-bound activity: it records that its body ran, then
// occupies its worker for hold, heartbeating all the while, so that killing the worker
// interrupts a running activity rather than an idle one. It returns the process that
// ran it.
//
// The line it appends is written before anything else it does, so a body that was
// killed a moment after it started still left its line: the file counts bodies that
// began, which is what "did the work run again" asks.
func HoldSession(ctx context.Context, p Params) (string, error) {
	stamp := Stamp()
	info := activity.GetInfo(ctx)
	line := fmt.Sprintf("attempt=%d %s at=%s", info.Attempt, stamp, time.Now().UTC().Format(time.RFC3339Nano))
	if err := appendLine(p.EffectFile, line); err != nil {
		return "", err
	}

	deadline := time.Now().Add(p.Hold)
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

// appendLine adds one line to a file, creating it if it is not there yet, and pushes it
// out of this process: the driver is another process and reads this file while the run
// is going on. The driver reads it back with experiment 19's readers, which this
// package shares; the writer is here because the activity runs in cmd/sessionworker.
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
