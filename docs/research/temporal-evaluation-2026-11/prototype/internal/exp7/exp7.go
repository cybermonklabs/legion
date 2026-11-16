// Package exp7 is experiment 7 of the evaluation: what DBOS does when the process
// holding a step is killed while that step is running.
//
// It is the counterpart of experiment 3, which asked Temporal the same question. A
// Legion step is a prompt into a session that already holds state: it writes somewhere
// the moment it starts, and it must not be run twice. The workflow here has one step
// that stands for such a prompt -- it appends a line to a file, naming its attempt and
// its process, and then blocks -- and the driver kills the process that is inside it.
//
// The number the experiment exists to produce is how many times that line was written.
//
// The workflow lives here because both cmd/dbosworker, which hosts it, and the driver,
// which reads what it left in PostgreSQL, need its names.
package exp7

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
)

// The names this experiment's pieces are registered and recorded under. The workflow
// name is explicit so the driver and the worker, two different binaries, agree on it.
const (
	WorkflowName = "exp7PromptWorkflow"
	StepName     = "prompt-into-session"
	AppName      = "legion-exp7"
	// AppVersion is pinned. DBOS otherwise derives the application version from the
	// running binary's hash, and recovery only picks up a workflow whose recorded
	// version matches the recovering process's: the driver's binary and the worker's
	// are different files, so a computed version would strand every workflow.
	AppVersion = "exp7-code-1"
	// ExecutorID is the identity both the killed process and its replacement open DBOS
	// under. A pending workflow is recovered at launch by a process carrying the
	// executor id recorded on it, and by no other.
	ExecutorID = "exp7-session-host"
)

// SideEffectFile is where the step writes its side effect: one line per time the step
// body really ran. Its line count is the experiment's central number.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp7-side-effects.log")
}

// ReleaseFile is what the driver creates to let a blocked step finish, standing for the
// session answering at last.
func ReleaseFile(workDir string) string {
	return filepath.Join(workDir, "exp7-release")
}

// Params is one run of PromptWorkflow. It is serialized into DBOS's own tables when the
// workflow starts, so the process that recovers the workflow receives exactly what the
// process that started it was given.
type Params struct {
	// SideEffectFile is the file the step appends its line to.
	SideEffectFile string `json:"sideEffectFile"`
	// ReleaseFile is the file whose appearance ends the step's block.
	ReleaseFile string `json:"releaseFile"`
	// Hold bounds that block, so a run whose driver died never hangs a process.
	Hold time.Duration `json:"hold"`
}

// Result is what the attempt that finished the step reported.
type Result struct {
	// Attempt is which performance of the side effect this was: 1 for the first.
	Attempt int `json:"attempt"`
	// PID is the process that performed it.
	PID int `json:"pid"`
}

// PromptWorkflow is the Legion step under test: one durable step that writes a side
// effect and then waits, as a prompt into a live session does.
func PromptWorkflow(ctx dbos.Context, p Params) (Result, error) {
	return dbos.RunAsStep(ctx, func(stepCtx context.Context) (Result, error) {
		return prompt(stepCtx, p)
	}, dbos.WithStepName(StepName))
}

// prompt performs the side effect and then blocks. The order is the whole point: the
// effect is in the world before anything about this attempt is in the database, which
// is the state a kill has to find.
func prompt(ctx context.Context, p Params) (Result, error) {
	lines, err := dbosx.ReadLines(p.SideEffectFile)
	if err != nil {
		return Result{}, err
	}
	attempt := len(lines) + 1
	line := fmt.Sprintf("attempt=%d pid=%d at=%s", attempt, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := dbosx.AppendLine(p.SideEffectFile, line); err != nil {
		return Result{}, err
	}
	if !dbosx.WaitForFile(ctx, p.ReleaseFile, p.Hold) {
		return Result{}, fmt.Errorf("the step was never released: %s did not appear within %s", p.ReleaseFile, p.Hold)
	}
	return Result{Attempt: attempt, PID: os.Getpid()}, nil
}

// Worker is cmd/dbosworker's experiment 7 mode: a process that hosts the workflow and
// waits to be killed. With Start it also starts the workflow; without it, it is the
// replacement, and the only thing it does is launch DBOS -- which is where recovery
// happens, if it happens at all.
func Worker(ctx context.Context, cfg dbosx.WorkerConfig, shutdown time.Duration) error {
	dbosCtx, err := dbosx.Open(ctx, cfg.Options())
	if err != nil {
		return err
	}
	defer dbos.Shutdown(dbosCtx, shutdown) //nolint:errcheck // a killed process never reaches this

	dbos.RegisterWorkflow(dbosCtx, PromptWorkflow, dbos.WithWorkflowName(WorkflowName))
	launchStart := time.Now()
	if err := dbos.Launch(dbosCtx); err != nil {
		return err
	}
	dbosx.Field("pid", os.Getpid())
	dbosx.Field("launch-seconds", fmt.Sprintf("%.3f", time.Since(launchStart).Seconds()))

	if cfg.Start {
		params := Params{
			SideEffectFile: SideEffectFile(cfg.WorkDir),
			ReleaseFile:    ReleaseFile(cfg.WorkDir),
			Hold:           Hold,
		}
		handle, err := dbos.RunWorkflow(dbosCtx, PromptWorkflow, params, dbos.WithWorkflowID(cfg.WorkflowID))
		if err != nil {
			return fmt.Errorf("start %s: %w", cfg.WorkflowID, err)
		}
		dbosx.Field("workflow-started", handle.GetWorkflowID())
	} else {
		dbosx.Field("recovery-launched", true)
	}

	<-ctx.Done()
	return nil
}
