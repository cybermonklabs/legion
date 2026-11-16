// Package exp15 is experiment 15 of the evaluation: what go-workflows does to a run
// that was recorded before the workflow's code changed.
//
// Legion's workflows change the way every program's do: a step is inserted in the
// middle of a phase. The question is what becomes of the runs that are already under
// way -- the ones that recorded the old shape and have not finished.
//
// go-workflows has no workflow.Version and no patch: its documented answer, in the
// library's own FAQ, is side-by-side deployment, with queues to route instances to
// the workers that can still run them. So this experiment measures both halves: what
// an unguarded change really does to a recorded run, and whether the documented
// alternative carries the same run to the end.
//
// This package holds the same workflow twice, registered under one name:
//
//   - RecordedWorkflow: plan, implement, wait for a person, deliver -- the code the
//     earlier run recorded.
//   - ChangedWorkflow: plan, review, implement, wait, deliver -- an ordinary change,
//     one new activity in the middle, with nothing said to the library about it.
package exp15

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cschleiden/go-workflows/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/gwx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The names this experiment's pieces are registered and recorded under. Both bodies
// register under one workflow name, because to the library they are one workflow
// whose code moved on.
const (
	WorkflowName = "exp15PhaseWorkflow"

	StepPlan      = "plan"
	StepReview    = "review"
	StepImplement = "implement"
	StepDeliver   = "deliver"

	// ReleaseSignal is the person's answer that wakes a recorded run, which is when
	// the changed code first gets to replay that run's history.
	ReleaseSignal = "release"

	// The three queues. A queue is go-workflows' only routing, and the documented
	// way to keep an old deployment serving the runs it recorded.
	QueueChanged    = "exp15-unguarded-change"
	QueueSideBySide = "exp15-side-by-side"
	QueueNew        = "exp15-new-code"

	// The versions the two bodies write into their lines, so a side effect says
	// which code performed it.
	VersionRecorded = "v1-recorded"
	VersionChanged  = "v2-changed"
)

// The timings this experiment runs the library with. The workflow task lock is short
// so that a task the changed code cannot execute is seen being retried rather than
// merely assumed to be.
const (
	WorkflowLockTimeout       = 6 * time.Second
	WorkflowHeartbeatInterval = 2 * time.Second
	ActivityLockTimeout       = 20 * time.Second
	ActivityHeartbeatInterval = 5 * time.Second
	StickyTimeout             = 2 * time.Second
)

// SideEffectFile is where every activity of every run writes a line, so the driver
// can say which steps really ran, in which run, under which code.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp15-steps.log")
}

// Params is one run of the phase workflow. Both bodies take it, so a run recorded by
// one can be replayed by the other.
type Params struct {
	SideEffectFile string `json:"sideEffectFile"`
	Instance       string `json:"instance"`
}

// RecordedWorkflow is the code as it stood before the change.
func RecordedWorkflow(ctx workflow.Context, p Params) (string, error) {
	if _, err := runStep(ctx, p, StepPlan); err != nil {
		return "", err
	}
	if _, err := runStep(ctx, p, StepImplement); err != nil {
		return "", err
	}
	workflow.NewSignalChannel[string](ctx, ReleaseSignal).Receive(ctx)
	return runStep(ctx, p, StepDeliver)
}

// ChangedWorkflow is the ordinary change: one new activity inserted in the middle,
// with nothing said to the library about it.
func ChangedWorkflow(ctx workflow.Context, p Params) (string, error) {
	if _, err := runStep(ctx, p, StepPlan); err != nil {
		return "", err
	}
	if _, err := runStep(ctx, p, StepReview); err != nil {
		return "", err
	}
	if _, err := runStep(ctx, p, StepImplement); err != nil {
		return "", err
	}
	workflow.NewSignalChannel[string](ctx, ReleaseSignal).Receive(ctx)
	return runStep(ctx, p, StepDeliver)
}

// runStep calls one phase step, each of which is its own activity: inserting a step
// is therefore inserting an activity, which is the change a workflow really makes.
func runStep(ctx workflow.Context, p Params, name string) (string, error) {
	return workflow.ExecuteActivity[string](ctx, workflow.ActivityOptions{
		RetryOptions: workflow.RetryOptions{MaxAttempts: 1},
	}, name, p, name).Get(ctx)
}

// Step is the activity body every phase step shares: it writes its line and returns
// what it wrote. Each step is registered under its own name, so the four steps are
// four activities to the library.
func Step(ctx context.Context, p Params, name string) (string, error) {
	line := fmt.Sprintf("step=%s instance=%s pid=%d at=%s",
		name, p.Instance, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := libx.AppendLine(p.SideEffectFile, line); err != nil {
		return "", err
	}
	return line, nil
}

// Options is how every worker of this experiment runs the library, so a lock a fact
// names means the same thing everywhere.
func Options(workerName string) gwx.Options {
	return gwx.Options{
		WorkerName:          workerName,
		ActivityLockTimeout: ActivityLockTimeout,
		WorkflowLockTimeout: WorkflowLockTimeout,
		StickyTimeout:       StickyTimeout,
	}
}
