// Package exp11 is experiment 11 of the evaluation: what a wait for a person costs in
// Restate, and what its `paused` state is like to use.
//
// It has two halves because Restate has the most interesting answer in the class to
// both questions.
//
// The wait. Legion waits for people: an architect approves a spec, a reviewer answers a
// question, and the issue stands still for hours or days. In Restate a handler creates
// an awakeable, publishes its id and blocks on it; the server suspends the invocation
// and the service process goes back to holding nothing at all. The answer arrives as an
// HTTP request from anywhere, and the handler goes on from where it stopped.
//
// `paused`. Restate 1.8 can stop an invocation that has failed too many times in a
// first-class state a person can list, inspect and resume, rather than killing it or
// retrying it for ever. That is the mechanism Legion most lacks, so this half drives a
// handler that always fails under a small max-attempts, asserts it reaches `paused`,
// reads it as an operator would, fixes the thing it was failing on, and resumes it.
//
// The handlers live here because both cmd/restateservice, which hosts them, and the
// driver, which reads what they left in the server, need their names.
package exp11

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

// The names this experiment's pieces are registered and recorded under.
const (
	// WaitServiceName holds the handler that waits for a person, PauseServiceName the
	// handler that always fails until a person fixes what it fails on, and
	// SelfPauseServiceName the handler that pauses itself from inside its own body.
	WaitServiceName      = "Exp11Wait"
	PauseServiceName     = "Exp11Pause"
	SelfPauseServiceName = "Exp11SelfPause"

	AskHandlerName       = "Ask"
	FixHandlerName       = "Fix"
	SelfPauseHandlerName = "PauseMyself"

	AskStepName   = "ask-the-person"
	ApplyStepName = "apply-the-answer"
	SelfStepName  = "pause-myself"

	// PauseMaxAttempts is the small retry budget the failing handler is registered
	// with, so the experiment reaches `paused` in seconds rather than in the hours
	// the server-wide default of seventy attempts would take.
	PauseMaxAttempts = 3
	// pauseInitialInterval, pauseFactor and pauseMaxInterval keep those three
	// attempts close together.
	pauseInitialInterval = 300 * time.Millisecond
	pauseFactor          = 1.5
	pauseMaxInterval     = time.Second

	// SelfPauseHold bounds the wait inside the self-pausing handler, and stays under
	// the server's one-minute inactivity timeout.
	SelfPauseHold = 30 * time.Second

	// WaitInactivityTimeout is how long the server lets the waiting handler hold its
	// connection open without making progress before it asks the handler to suspend.
	// It is set on this service because the Go SDK's default protocol mode is
	// bidirectional: the SDK keeps the attempt's HTTP/2 stream open while the handler
	// is blocked, and only gives it up when the server asks -- so with the server's
	// own default of a minute, a wait reads as `running` with an attempt in flight
	// for a whole minute before it suspends. Three seconds makes the measurement
	// quick; the experiment prints the server's default beside it.
	WaitInactivityTimeout = 3 * time.Second
)

// AskFile is where the waiting handler publishes its awakeable id: one line per time
// that step ran. DecisionFile is where it writes what the answer licensed, so the
// driver can see the handler really went on from where it suspended.
func AskFile(workDir string) string {
	return filepath.Join(workDir, "exp11-ask.log")
}

// DecisionFile is written by the step after the wait.
func DecisionFile(workDir string) string {
	return filepath.Join(workDir, "exp11-decision.log")
}

// AttemptFile is where the always-failing handler writes one line per attempt, which
// is how the experiment counts the attempts the server really made before pausing.
func AttemptFile(workDir string) string {
	return filepath.Join(workDir, "exp11-attempts.log")
}

// FixFile is the thing a person has to put right: while it is missing the handler
// fails, and once the driver creates it -- standing for the person having looked -- a
// resumed invocation succeeds.
func FixFile(workDir string) string {
	return filepath.Join(workDir, "exp11-fix")
}

// SelfPauseFile is where the self-pausing handler records what it did and what the
// admin API answered it.
func SelfPauseFile(workDir string) string {
	return filepath.Join(workDir, "exp11-self-pause.log")
}

// SelfPauseReleaseFile lets the resumed self-pausing handler finish, so a run that
// paused itself can still be driven to an end.
func SelfPauseReleaseFile(workDir string) string {
	return filepath.Join(workDir, "exp11-self-pause-release")
}

// WaitParams is one wait for a person.
type WaitParams struct {
	IssueID      string `json:"issueID"`
	AskFile      string `json:"askFile"`
	DecisionFile string `json:"decisionFile"`
}

// askRecord is what the step before the wait durably recorded: the line it published
// and the process that published it.
type askRecord struct {
	Line string `json:"line"`
	PID  int    `json:"pid"`
}

// WaitResult is what the gate did, and in which process each half of it ran.
type WaitResult struct {
	AwakeableID  string `json:"awakeableID"`
	Answer       string `json:"answer"`
	AskedByPID   int    `json:"askedByPID"`
	AppliedByPID int    `json:"appliedByPID"`
}

// FixParams is one invocation of the always-failing handler.
type FixParams struct {
	AttemptFile string `json:"attemptFile"`
	FixFile     string `json:"fixFile"`
}

// SelfPauseParams is one invocation of the handler that pauses itself. It is given the
// admin address because the pause is an ordinary admin API call the handler makes
// about itself, from inside its own body.
type SelfPauseParams struct {
	AdminAddress string `json:"adminAddress"`
	LogFile      string `json:"logFile"`
	ReleaseFile  string `json:"releaseFile"`
}

// Services are the three service definitions cmd/restateservice binds for this
// experiment. The failing one carries the retry policy that is the half's subject:
// three attempts, and then `paused` rather than death or an endless retry.
func Services() []restate.ServiceDefinition {
	return []restate.ServiceDefinition{
		restate.NewService(WaitServiceName,
			restate.WithInactivityTimeout(WaitInactivityTimeout)).
			Handler(AskHandlerName, restate.NewServiceHandler(ask)),
		restate.NewService(PauseServiceName,
			restate.WithInvocationRetryPolicy(
				restate.WithMaxRetryAttempts(PauseMaxAttempts),
				restate.WithInitialRetryInterval(pauseInitialInterval),
				restate.WithRetryIntervalFactor(pauseFactor),
				restate.WithMaxRetryInterval(pauseMaxInterval),
				restate.PauseOnMaxAttempts(),
			)).
			Handler(FixHandlerName, restate.NewServiceHandler(fix)),
		restate.NewService(SelfPauseServiceName).
			Handler(SelfPauseHandlerName, restate.NewServiceHandler(pauseMyself)),
	}
}

// ask is the wait for a person: it creates an awakeable, publishes its id, and blocks
// on it. Blocking on an awakeable is what makes the server suspend the invocation and
// close the connection, which is why this process holds nothing while it waits.
func ask(ctx restate.Context, p WaitParams) (WaitResult, error) {
	defer restatex.EnterHandler()()
	awakeable := restate.Awakeable[string](ctx)
	id := awakeable.Id()

	// The step's result carries the process that ran it, so the pid in the handler's
	// own result is the one that asked rather than the one that happens to be alive
	// when the answer comes: a replay takes it from the journal.
	asked, err := restate.Run(ctx, func(restate.RunContext) (askRecord, error) {
		line := fmt.Sprintf("issue=%s awakeable=%s asked-by-pid=%d at=%s",
			p.IssueID, id, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		return askRecord{Line: line, PID: os.Getpid()}, restatex.AppendLine(p.AskFile, line)
	}, restate.WithName(AskStepName))
	if err != nil {
		return WaitResult{}, err
	}

	answer, awakeErr := awakeable.Result()
	if awakeErr != nil {
		return WaitResult{}, awakeErr
	}

	if _, err := restate.Run(ctx, func(restate.RunContext) (string, error) {
		line := fmt.Sprintf("issue=%s answer=%s applied-by-pid=%d at=%s",
			p.IssueID, answer, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		return line, restatex.AppendLine(p.DecisionFile, line)
	}, restate.WithName(ApplyStepName)); err != nil {
		return WaitResult{}, err
	}
	return WaitResult{AwakeableID: id, Answer: answer, AskedByPID: asked.PID, AppliedByPID: os.Getpid()}, nil
}

// fix always fails until the thing it fails on is put right. The attempt is recorded
// outside any durable step on purpose: a Run would be replayed from the journal rather
// than executed again, and what this half counts is attempts the server really made.
//
// The error returned is an ordinary one rather than a terminal one, so Restate retries
// it -- and retrying it until the budget runs out is what reaches `paused`.
func fix(_ restate.Context, p FixParams) (string, error) {
	defer restatex.EnterHandler()()
	attempt := restatex.CountLines(p.AttemptFile) + 1
	line := fmt.Sprintf("attempt=%d pid=%d at=%s", attempt, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := restatex.AppendLine(p.AttemptFile, line); err != nil {
		return "", err
	}
	if !restatex.Exists(p.FixFile) {
		return "", fmt.Errorf("the session's credentials are missing: a person has to look (attempt %d)", attempt)
	}
	return fmt.Sprintf("succeeded on attempt %d in pid %d", attempt, os.Getpid()), nil
}

// pauseMyself asks the admin API to pause the invocation it is itself running, which
// is the documented PATCH /invocations/{id}/pause against an invocation that is
// running. It does it on its first attempt only: a handler that paused itself every
// time could never be resumed into finishing.
func pauseMyself(ctx restate.Context, p SelfPauseParams) (string, error) {
	defer restatex.EnterHandler()()
	invocation := ctx.Request().ID
	attempt := attemptsIn(p.LogFile) + 1
	line := fmt.Sprintf("attempt=%d pid=%d invocation=%s", attempt, os.Getpid(), invocation)
	if err := restatex.AppendLine(p.LogFile, line); err != nil {
		return "", err
	}
	if attempt > 1 {
		return fmt.Sprintf("went on after being resumed, on attempt %d in pid %d", attempt, os.Getpid()), nil
	}

	status, err := patchPause(p.AdminAddress, invocation)
	if err != nil {
		return "", err
	}
	if err := restatex.AppendLine(p.LogFile, "self-pause-http-status="+status); err != nil {
		return "", err
	}
	// Hold, so the server has an attempt in flight to pause. Whether this returns at
	// all is one of the things the experiment is measuring.
	if !restatex.WaitForFile(ctx, p.ReleaseFile, SelfPauseHold) {
		return "", fmt.Errorf("the handler was never released and was never stopped either: %s did not appear within %s", p.ReleaseFile, SelfPauseHold)
	}
	return fmt.Sprintf("ran to the end on attempt %d without being paused", attempt), nil
}

// patchPause is the admin API call a handler makes about itself.
func patchPause(adminAddress, invocation string) (string, error) {
	url := fmt.Sprintf("http://%s/invocations/%s/pause", adminAddress, invocation)
	req, err := http.NewRequest(http.MethodPatch, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("pause myself: %w", err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if body.Len() == 0 {
		return resp.Status, nil
	}
	return resp.Status + " " + body.String(), nil
}

// attemptsIn counts the attempts a handler has already recorded in its log, ignoring
// the other lines it writes beside them.
func attemptsIn(path string) int {
	lines, err := restatex.ReadLines(path)
	if err != nil {
		return 0
	}
	attempts := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "attempt=") {
			attempts++
		}
	}
	return attempts
}
