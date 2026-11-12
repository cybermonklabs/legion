// Package exp10 is experiment 10 of the evaluation: what Restate does when the process
// holding a handler is killed while that handler is running.
//
// It is the counterpart of experiment 3, which asked Temporal, and experiment 7, which
// asked DBOS. A Legion step is a prompt into a session that already holds state: it
// writes somewhere the moment it starts, and it must not be run twice. The handler here
// stands for such a prompt -- it appends a line to a file, naming its attempt and its
// process, and then blocks -- and the driver kills the process that is inside it.
//
// The number the experiment exists to produce is how many times that line was written.
//
// Restate's shape makes the question sharper than for the other two. The handler does
// not run in the server and does not run in the driver: it runs in a process of its own
// behind an HTTP endpoint, and the server holds the journal. An invocation is pinned to
// a deployment, and a deployment is that endpoint's URL -- so "the process the work is
// bound to" is, for Restate, a URL rather than a process, and killing the process
// behind it is the experiment.
//
// The handler lives here because both cmd/restateservice, which hosts it, and the
// driver, which reads what it left in the server, need its names.
package exp10

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

// The names this experiment's pieces are registered and recorded under. The service
// and handler names are explicit so the driver and the service process, two different
// binaries, agree on them.
const (
	ServiceName = "Exp10Prompt"
	HandlerName = "Prompt"
	// StepName is the name the side-effecting durable step is journaled under, which
	// is how the driver finds it among the journal's entries.
	StepName = "prompt-the-session"
	// Hold bounds the block inside the step. It must outlast the kill, the
	// replacement process's start and the driver's release, and still end a run whose
	// driver died -- and it must stay under the server's one-minute inactivity
	// timeout, past which the server asks the handler to suspend instead.
	Hold = 45 * time.Second
)

// SideEffectFile is where the step writes its side effect: one line per time the step
// body really ran. Its line count is the experiment's central number.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp10-side-effects.log")
}

// ReleaseFile is what the driver creates to let a blocked step finish, standing for the
// session answering at last.
func ReleaseFile(workDir string) string {
	return filepath.Join(workDir, "exp10-release")
}

// Params is one invocation of the prompt handler. Restate journals it when the
// invocation is created, so every attempt -- in whichever process -- is handed exactly
// what the first one was.
type Params struct {
	SideEffectFile string `json:"sideEffectFile"`
	ReleaseFile    string `json:"releaseFile"`
	// HoldMillis bounds the block inside the step.
	HoldMillis int64 `json:"holdMillis"`
}

// Result is what the attempt that finished the step reported.
type Result struct {
	Line string `json:"line"`
	PID  int    `json:"pid"`
}

// Service is the service definition cmd/restateservice binds. One handler, one durable
// step inside it.
func Service() restate.ServiceDefinition {
	return restate.NewService(ServiceName).
		Handler(HandlerName, restate.NewServiceHandler(prompt))
}

// prompt is the Legion step under test: one durable step that writes a side effect and
// then waits, as a prompt into a live session does. The order is the whole point: the
// effect is in the world before anything about this attempt is in the journal, which is
// the state a kill has to find.
func prompt(ctx restate.Context, p Params) (Result, error) {
	defer restatex.EnterHandler()()
	// The invocation id is read from the handler's context, never from inside the Run
	// closure, which may only use the RunContext it is given.
	invocation := ctx.Request().ID
	line, err := restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		// The attempt number counts the lines already there, so it is the number of
		// times this side effect has been performed across every process, not a
		// counter this process could reset by dying.
		attempt := restatex.CountLines(p.SideEffectFile) + 1
		line := fmt.Sprintf("attempt=%d pid=%d invocation=%s at=%s",
			attempt, os.Getpid(), invocation, time.Now().UTC().Format(time.RFC3339Nano))
		if err := restatex.AppendLine(p.SideEffectFile, line); err != nil {
			return "", err
		}
		hold := time.Duration(p.HoldMillis) * time.Millisecond
		if !restatex.WaitForFile(runCtx, p.ReleaseFile, hold) {
			return "", fmt.Errorf("the step was never released: %s did not appear within %s", p.ReleaseFile, hold)
		}
		return line, nil
	}, restate.WithName(StepName))
	if err != nil {
		return Result{}, err
	}
	return Result{Line: line, PID: os.Getpid()}, nil
}

// NewParams is the input every invocation of this experiment's handler is given.
func NewParams(workDir string) Params {
	return Params{
		SideEffectFile: SideEffectFile(workDir),
		ReleaseFile:    ReleaseFile(workDir),
		HoldMillis:     Hold.Milliseconds(),
	}
}
