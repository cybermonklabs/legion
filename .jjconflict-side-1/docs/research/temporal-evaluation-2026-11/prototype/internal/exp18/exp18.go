// Package exp18 is experiment 18 of the evaluation: what durabletask-go does to a
// run that was recorded before the workflow's code changed.
//
// Legion's workflows change the way every program's do: a step is inserted in the
// middle of a phase. The question is what becomes of the runs that are already under
// way -- the ones that recorded the old shape and have not finished.
//
// durabletask-go has two documented answers, and this experiment measures both
// beside the unguarded change: ctx.IsPatched, which runs the new step only for a run
// that had not passed that point, and a versioned registry, which pins a run to the
// version of the code it started under. It has no queues and no routing of any kind,
// so side-by-side deployment -- go-workflows' answer in experiment 15 -- is not
// available here: any worker polls any instance.
//
// This package holds the same workflow three times, under one canonical name:
//
//   - RecordedWorkflow: plan, implement, wait for a person, deliver.
//   - ChangedWorkflow: plan, review, implement, wait, deliver -- the ordinary change.
//   - PatchedWorkflow: the same change behind ctx.IsPatched.
package exp18

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dapr/durabletask-go/task"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dtx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The names this experiment's pieces are registered and recorded under.
const (
	WorkflowName = "exp18PhaseWorkflow"

	StepPlan      = "plan"
	StepReview    = "review"
	StepImplement = "implement"
	StepDeliver   = "deliver"

	// ReleaseEvent is the person's answer that wakes a recorded run, which is when
	// changed code first gets to replay that run's history.
	ReleaseEvent = "release"

	// PatchName is the name of the patch that guards the inserted step.
	PatchName = "review-before-implement"

	// VersionOld and VersionNew are the two versions of the versioned registry.
	VersionOld = "v1"
	VersionNew = "v2"
)

// The timings this experiment runs the library with. The work-item lock is short so
// that a work item the changed code cannot execute is seen being retried rather than
// merely assumed to be.
const (
	WorkflowLockTimeout = 6 * time.Second
	ActivityLockTimeout = 6 * time.Second
	// Wait is the gate's timeout: negative is the library's unbounded wait.
	Wait = -1 * time.Second
)

// SideEffectFile is where every activity of every run writes a line, so the driver
// can say which steps really ran, in which run, under which code.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp18-steps.log")
}

// Params is one run of the phase workflow. Every body takes it, so a run recorded by
// one can be replayed by another.
type Params struct {
	SideEffectFile string `json:"sideEffectFile"`
	Instance       string `json:"instance"`
}

// RecordedWorkflow is the code as it stood before the change.
func RecordedWorkflow(ctx *task.WorkflowContext) (any, error) {
	p, err := input(ctx)
	if err != nil {
		return nil, err
	}
	if err := call(ctx, p, StepPlan); err != nil {
		return nil, err
	}
	if err := call(ctx, p, StepImplement); err != nil {
		return nil, err
	}
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return nil, call(ctx, p, StepDeliver)
}

// ChangedWorkflow is the ordinary change: one new activity inserted in the middle,
// with nothing said to the library about it.
func ChangedWorkflow(ctx *task.WorkflowContext) (any, error) {
	p, err := input(ctx)
	if err != nil {
		return nil, err
	}
	if err := call(ctx, p, StepPlan); err != nil {
		return nil, err
	}
	if err := call(ctx, p, StepReview); err != nil {
		return nil, err
	}
	if err := call(ctx, p, StepImplement); err != nil {
		return nil, err
	}
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return nil, call(ctx, p, StepDeliver)
}

// PatchedWorkflow is the same change made the way the library supports it: the new
// activity runs only for a run that had not passed this point when the patch was
// deployed.
func PatchedWorkflow(ctx *task.WorkflowContext) (any, error) {
	p, err := input(ctx)
	if err != nil {
		return nil, err
	}
	if err := call(ctx, p, StepPlan); err != nil {
		return nil, err
	}
	if ctx.IsPatched(PatchName) {
		if err := call(ctx, p, StepReview); err != nil {
			return nil, err
		}
	}
	if err := call(ctx, p, StepImplement); err != nil {
		return nil, err
	}
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return nil, call(ctx, p, StepDeliver)
}

func input(ctx *task.WorkflowContext) (Params, error) {
	var p Params
	if err := ctx.GetInput(&p); err != nil {
		return Params{}, err
	}
	return p, nil
}

// call runs one phase step, each of which is its own activity: inserting a step is
// therefore inserting an activity, which is the change a workflow really makes.
func call(ctx *task.WorkflowContext, p Params, name string) error {
	var line string
	return ctx.CallActivity(name, task.WithActivityInput(stepInput{Params: p, Step: name})).Await(&line)
}

// wait is the gate every body stops at, so a change meets a run that is parked.
func wait(ctx *task.WorkflowContext) error {
	var answer string
	return ctx.WaitForSingleEvent(ReleaseEvent, Wait).Await(&answer)
}

// stepInput is what one step's activity is given.
type stepInput struct {
	Params Params `json:"params"`
	Step   string `json:"step"`
}

// Step is the activity body every phase step shares: it writes its line and returns
// what it wrote. Each step is registered under its own name, so the four steps are
// four activities to the library.
func Step(ctx task.ActivityContext) (any, error) {
	var in stepInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	line := fmt.Sprintf("step=%s instance=%s pid=%d at=%s",
		in.Step, in.Params.Instance, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := libx.AppendLine(in.Params.SideEffectFile, line); err != nil {
		return nil, err
	}
	return line, nil
}

// Registry is one deployment's registry: body registered under the canonical name,
// with the four step activities beside it.
func Registry(body task.Workflow) (*task.TaskRegistry, error) {
	r := task.NewTaskRegistry()
	if err := r.AddWorkflowN(WorkflowName, body); err != nil {
		return nil, fmt.Errorf("register the workflow: %w", err)
	}
	return withActivities(r)
}

// VersionedRegistry is a deployment that registers one or two versions of the
// workflow under the canonical name, the last of them the latest. A run started
// under such a deployment records the version it resolved, and a later deployment
// runs that run under that version rather than under its newest code.
func VersionedRegistry(bodies map[string]task.Workflow, latest string) (*task.TaskRegistry, error) {
	r := task.NewTaskRegistry()
	for version, body := range bodies {
		if err := r.AddVersionedWorkflowN(WorkflowName, version, version == latest, body); err != nil {
			return nil, fmt.Errorf("register version %s: %w", version, err)
		}
	}
	return withActivities(r)
}

func withActivities(r *task.TaskRegistry) (*task.TaskRegistry, error) {
	for _, name := range []string{StepPlan, StepReview, StepImplement, StepDeliver} {
		if err := r.AddActivityN(name, Step); err != nil {
			return nil, fmt.Errorf("register activity %s: %w", name, err)
		}
	}
	return r, nil
}

// Options is how every deployment of this experiment runs the library.
func Options(dsn, schema string) dtx.Options {
	return dtx.Options{
		DSN: dsn, Schema: schema,
		WorkflowLockTimeout: WorkflowLockTimeout,
		ActivityLockTimeout: ActivityLockTimeout,
	}
}

// Deployment is one worker of this experiment: the engine, and the context that
// stops it. durabletask-go has no routing, so exactly one deployment may be running
// at a time or they would take each other's work items.
type Deployment struct {
	Engine *dtx.Engine
	stop   context.CancelFunc
}

// Deploy starts one deployment with the given registry.
func Deploy(ctx context.Context, r *task.TaskRegistry, dsn, schema string) (*Deployment, error) {
	runCtx, stop := context.WithCancel(ctx)
	engine, err := dtx.Start(runCtx, r, Options(dsn, schema))
	if err != nil {
		stop()
		return nil, err
	}
	return &Deployment{Engine: engine, stop: stop}, nil
}

// Stop takes the deployment down, as a rollout replacing it would.
func (d *Deployment) Stop(ctx context.Context) {
	if d == nil {
		return
	}
	d.Engine.Shutdown(ctx)
	d.stop()
}
