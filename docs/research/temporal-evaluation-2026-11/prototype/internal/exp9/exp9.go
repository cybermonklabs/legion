// Package exp9 is experiment 9 of the evaluation: what DBOS does to a run recorded
// before the workflow's code changed.
//
// Legion's workflows change the way every program's do: a step is inserted in the
// middle of a phase. The question is what becomes of the runs that are already under
// way -- the ones that recorded the old shape and have not finished. Does the new code
// refuse them, re-run them, skip past the change, or is there a version gate that keeps
// them apart; and what is that version keyed on.
//
// This package holds the same workflow three times, registered under one name:
//
//   - RecordedWorkflow: plan, implement, deliver -- the code the earlier run recorded.
//   - ChangedWorkflow: plan, review, implement, deliver -- an ordinary change, one new
//     step in the middle.
//   - PatchedWorkflow: the same change made the way DBOS supports, behind dbos.Patch.
package exp9

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
)

// The names this experiment's pieces are registered and recorded under. All three
// bodies register under one workflow name, because to DBOS they are one workflow whose
// code moved on.
const (
	WorkflowName = "exp9PhaseWorkflow"
	AppName      = "legion-exp9"
	ExecutorID   = "exp9-phase-host"

	// RecordedVersion is the application version the earlier run was recorded under,
	// ChangedVersion the one the changed code runs under, and PatchedVersion the one
	// the patched code runs under. They are pinned rather than computed: DBOS derives
	// an application version from the running binary's hash, so an ordinary
	// deployment changes it, which is exactly the situation these three names stand
	// in for.
	RecordedVersion = "exp9-code-v1"
	ChangedVersion  = "exp9-code-v2"
	PatchedVersion  = "exp9-code-v3-patched"

	// PatchName is the name of the patch that guards the inserted step.
	PatchName = "review-before-implement"

	StepPlan      = "plan"
	StepReview    = "review"
	StepImplement = "implement"
	StepDeliver   = "deliver"
)

// SideEffectFile is where every step of every run writes a line, so the driver can say
// which steps really ran, in which run, under which version.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp9-steps.log")
}

// ReleaseFile is what lets the deliver step finish. The first run is killed while that
// step waits for it; the driver creates it before any later run.
func ReleaseFile(workDir string) string {
	return filepath.Join(workDir, "exp9-release")
}

// Params is one run of the phase workflow. All three bodies take it, so a run recorded
// by one can be replayed by another.
type Params struct {
	SideEffectFile string        `json:"sideEffectFile"`
	ReleaseFile    string        `json:"releaseFile"`
	Hold           time.Duration `json:"hold"`
	Version        string        `json:"version"`
}

// RecordedWorkflow is the code as it stood before the change.
func RecordedWorkflow(ctx dbos.Context, p Params) (string, error) {
	if _, err := runStep(ctx, p, StepPlan); err != nil {
		return "", err
	}
	if _, err := runStep(ctx, p, StepImplement); err != nil {
		return "", err
	}
	return deliver(ctx, p)
}

// ChangedWorkflow is the ordinary change: one new step inserted in the middle, with
// nothing said to DBOS about it.
func ChangedWorkflow(ctx dbos.Context, p Params) (string, error) {
	if _, err := runStep(ctx, p, StepPlan); err != nil {
		return "", err
	}
	if _, err := runStep(ctx, p, StepReview); err != nil {
		return "", err
	}
	if _, err := runStep(ctx, p, StepImplement); err != nil {
		return "", err
	}
	return deliver(ctx, p)
}

// PatchedWorkflow is the same change made the way DBOS supports it: the new step runs
// only for a workflow that had not passed this point when the patch was deployed.
func PatchedWorkflow(ctx dbos.Context, p Params) (string, error) {
	if _, err := runStep(ctx, p, StepPlan); err != nil {
		return "", err
	}
	patched, err := dbos.Patch(ctx, PatchName)
	if err != nil {
		return "", fmt.Errorf("patch %s: %w", PatchName, err)
	}
	if patched {
		if _, err := runStep(ctx, p, StepReview); err != nil {
			return "", err
		}
	}
	if _, err := runStep(ctx, p, StepImplement); err != nil {
		return "", err
	}
	return deliver(ctx, p)
}

// runStep is one phase step: it writes its line and returns what it wrote.
func runStep(ctx dbos.Context, p Params, name string) (string, error) {
	workflowID, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return "", err
	}
	return dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		line := fmt.Sprintf("step=%s workflow=%s version=%s pid=%d at=%s",
			name, workflowID, p.Version, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		if err := dbosx.AppendLine(p.SideEffectFile, line); err != nil {
			return "", err
		}
		return line, nil
	}, dbos.WithStepName(name))
}

// deliver is the last step: it writes its line and then holds, so the first run can be
// killed inside it and leave the phase half finished.
func deliver(ctx dbos.Context, p Params) (string, error) {
	workflowID, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return "", err
	}
	return dbos.RunAsStep(ctx, func(stepCtx context.Context) (string, error) {
		line := fmt.Sprintf("step=%s workflow=%s version=%s pid=%d at=%s",
			StepDeliver, workflowID, p.Version, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
		if err := dbosx.AppendLine(p.SideEffectFile, line); err != nil {
			return "", err
		}
		if !dbosx.WaitForFile(stepCtx, p.ReleaseFile, p.Hold) {
			return "", fmt.Errorf("the deliver step was never released: %s did not appear within %s", p.ReleaseFile, p.Hold)
		}
		return line, nil
	}, dbos.WithStepName(StepDeliver))
}

// NewParams is the input every run of this experiment's workflow is given.
func NewParams(workDir, version string) Params {
	return Params{
		SideEffectFile: SideEffectFile(workDir),
		ReleaseFile:    ReleaseFile(workDir),
		Hold:           Hold,
		Version:        version,
	}
}

// Worker is cmd/dbosworker's experiment 9 mode: a process running the code as it stood
// before the change, which the driver kills while its last step holds.
func Worker(ctx context.Context, cfg dbosx.WorkerConfig, shutdown time.Duration) error {
	dbosCtx, err := dbosx.Open(ctx, cfg.Options())
	if err != nil {
		return err
	}
	defer dbos.Shutdown(dbosCtx, shutdown) //nolint:errcheck // a killed process never reaches this

	dbos.RegisterWorkflow(dbosCtx, RecordedWorkflow, dbos.WithWorkflowName(WorkflowName))
	if err := dbos.Launch(dbosCtx); err != nil {
		return err
	}
	dbosx.Field("pid", os.Getpid())
	if cfg.Start {
		params := NewParams(cfg.WorkDir, cfg.AppVersion)
		if _, err := dbos.RunWorkflow(dbosCtx, RecordedWorkflow, params, dbos.WithWorkflowID(cfg.WorkflowID)); err != nil {
			return fmt.Errorf("start %s: %w", cfg.WorkflowID, err)
		}
		dbosx.Field("workflow-started", cfg.WorkflowID)
	} else {
		dbosx.Field("recovery-launched", true)
	}
	<-ctx.Done()
	return nil
}
