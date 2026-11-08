// Package exp12 is experiment 12 of the evaluation: what Restate does to a run whose
// journal was recorded before the handler's code changed.
//
// Legion's phases change the way every program's do: a step is inserted in the middle.
// The question is what becomes of the invocations already under way -- the ones that
// recorded the old shape and have not finished. Does the server detect the divergence,
// what is the error, and is the invocation killed, paused or retried for ever; and what
// is the supported way to make the change safely.
//
// Restate's answer to the last question is a new deployment: an invocation is pinned to
// the deployment it started on, so a change deployed at a new endpoint leaves in-flight
// invocations running the code they started with, and only new invocations reach the
// new code. This package holds the handler twice, under one service name, so a driver
// can run both halves:
//
//   - the recorded variant: plan, implement, deliver -- the code the earlier run recorded;
//   - the changed variant: plan, review, implement, deliver -- an ordinary change, one
//     new durable step in the middle.
//
// The handlers live here because both cmd/restateservice, which hosts them, and the
// driver, which reads what they left in the server, need their names.
package exp12

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

// The names this experiment's pieces are registered and recorded under. Both variants
// register under one service name, because to Restate they are one service whose code
// moved on.
const (
	ServiceName = "Exp12Phase"
	HandlerName = "Phase"

	StepPlan      = "plan"
	StepReview    = "review"
	StepImplement = "implement"
	StepDeliver   = "deliver"

	// VariantRecorded is the code as it stood before the change, and VariantChanged
	// the code after it.
	VariantRecorded = "recorded"
	VariantChanged  = "changed"

	// Hold bounds the block inside the last step, and stays under the server's
	// one-minute inactivity timeout.
	Hold = 45 * time.Second
)

// variants are the durable steps each version of the code runs before it delivers.
// The change is the ordinary one: a review inserted between planning and implementing.
var variants = map[string][]string{
	VariantRecorded: {StepPlan, StepImplement},
	VariantChanged:  {StepPlan, StepReview, StepImplement},
}

// StepFile is where every step of every run writes a line, so the driver can say which
// steps really ran, in which run, under which variant.
func StepFile(workDir string) string {
	return filepath.Join(workDir, "exp12-steps.log")
}

// ReleaseFile is what lets one run's last step finish. Each run has its own, so one
// invocation can be held in the middle of its journal while another runs to the end.
func ReleaseFile(workDir, run string) string {
	return filepath.Join(workDir, "exp12-release-"+run)
}

// Params is one run of the phase handler. Both variants take it, so a run recorded by
// one can be replayed by the other.
type Params struct {
	// Run labels the invocation, so the driver can pick this run's lines out of the
	// one file every run writes to.
	Run         string `json:"run"`
	StepFile    string `json:"stepFile"`
	ReleaseFile string `json:"releaseFile"`
	HoldMillis  int64  `json:"holdMillis"`
}

// Result is what the attempt that finished reported: the steps it believes it ran, and
// the variant and process that ran them.
type Result struct {
	Run     string   `json:"run"`
	Variant string   `json:"variant"`
	Steps   []string `json:"steps"`
	PID     int      `json:"pid"`
}

// Service is the service definition cmd/restateservice binds for one variant of the
// code. The variant is chosen when the process starts, which is exactly how an
// ordinary deployment chooses it: by which binary is running.
func Service(variant string) (restate.ServiceDefinition, error) {
	steps, ok := variants[variant]
	if !ok {
		return nil, fmt.Errorf("-variant must be %q or %q, got %q", VariantRecorded, VariantChanged, variant)
	}
	return restate.NewService(ServiceName).
		Handler(HandlerName, restate.NewServiceHandler(
			func(ctx restate.Context, p Params) (Result, error) {
				return phase(ctx, p, variant, steps)
			})), nil
}

// phase runs the variant's steps and then delivers. Every step is a durable one, so a
// replay takes its result from the journal rather than running it again -- which is
// what makes a step inserted in the middle a question at all.
func phase(ctx restate.Context, p Params, variant string, steps []string) (Result, error) {
	defer restatex.EnterHandler()()
	invocation := ctx.Request().ID
	ran := make([]string, 0, len(steps)+1)
	for _, step := range steps {
		line, err := runStep(ctx, p, variant, invocation, step)
		if err != nil {
			return Result{}, err
		}
		ran = append(ran, line)
	}
	line, err := deliver(ctx, p, variant, invocation)
	if err != nil {
		return Result{}, err
	}
	return Result{Run: p.Run, Variant: variant, Steps: append(ran, line), PID: os.Getpid()}, nil
}

// runStep is one durable step: it writes its line and returns what it wrote.
func runStep(ctx restate.Context, p Params, variant, invocation, name string) (string, error) {
	return restate.Run(ctx, func(restate.RunContext) (string, error) {
		line := fmt.Sprintf("run=%s step=%s variant=%s invocation=%s pid=%d at=%s",
			p.Run, name, variant, invocation, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		return line, restatex.AppendLine(p.StepFile, line)
	}, restate.WithName(name))
}

// deliver is the last step: it writes its line and then holds, so a run can be caught
// with its journal half recorded and its code changed underneath it.
func deliver(ctx restate.Context, p Params, variant, invocation string) (string, error) {
	return restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
		line := fmt.Sprintf("run=%s step=%s variant=%s invocation=%s pid=%d at=%s",
			p.Run, StepDeliver, variant, invocation, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		if err := restatex.AppendLine(p.StepFile, line); err != nil {
			return "", err
		}
		hold := time.Duration(p.HoldMillis) * time.Millisecond
		if !restatex.WaitForFile(runCtx, p.ReleaseFile, hold) {
			return "", fmt.Errorf("the deliver step was never released: %s did not appear within %s", p.ReleaseFile, hold)
		}
		return line, nil
	}, restate.WithName(StepDeliver))
}

// NewParams is the input one run of this experiment's handler is given.
func NewParams(workDir, run string) Params {
	return Params{
		Run:         run,
		StepFile:    StepFile(workDir),
		ReleaseFile: ReleaseFile(workDir, run),
		HoldMillis:  Hold.Milliseconds(),
	}
}
