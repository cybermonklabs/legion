// Package exp16 is experiment 16 of the evaluation: what durabletask-go does when
// the process holding an activity is killed while that activity is running.
//
// It is the counterpart of experiments 3, 7, 10 and 13, which asked Temporal, DBOS,
// Restate and go-workflows the same question. A Legion step is a prompt into a
// session that already holds state: it writes somewhere the moment it starts, and it
// must not be run twice. The workflow here has one activity that stands for such a
// prompt -- it appends a line to a file, naming its attempt and its process, and then
// blocks -- and the driver kills the process that is inside it.
//
// The number the experiment exists to produce is how many times that line was
// written.
//
// The workflow lives here because both cmd/dtworker, which hosts it, and the driver,
// which reads what it left in PostgreSQL, need its names.
package exp16

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dapr/durabletask-go/api"
	"github.com/dapr/durabletask-go/task"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dtx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The names this experiment's pieces are registered and recorded under. They are
// explicit so the driver and the worker, two different binaries, agree on them.
const (
	WorkflowName = "exp16PromptWorkflow"
	ActivityName = "prompt-into-session"
	// FieldPrefix is what cmd/dtworker puts in front of every line its driver reads.
	FieldPrefix = "dtworker"
)

// The timings this experiment runs the library with.
const (
	// WorkflowLockTimeout replaces the library's documented default of two minutes.
	// The PostgreSQL backend uses this one setting for both kinds of work item: its
	// getActivityWorkItem computes the activity lock's expiration from it too. A pod
	// experiment cannot sit out two minutes of lapse, so the run shortens the lock
	// and says so rather than waiting.
	WorkflowLockTimeout = 10 * time.Second
	// ActivityLockTimeout is what the process asks for. The PostgreSQL backend of
	// v0.16.0 never reads it, which is itself a measured fact of this experiment.
	ActivityLockTimeout = 10 * time.Second
	// Hold bounds the block inside the activity. It must outlast the kill, the
	// replacement process's start, the lock's lapse and the driver's release, and
	// still end a run whose driver died.
	Hold = 70 * time.Second
)

// SideEffectFile is where the activity writes its side effect: one line per time the
// activity body really ran. Its line count is the experiment's central number.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp16-side-effects.log")
}

// ReleaseFile is what the driver creates to let a blocked activity finish, standing
// for the session answering at last.
func ReleaseFile(workDir string) string {
	return filepath.Join(workDir, "exp16-release")
}

// Params is one run of the workflow. It is serialized into the library's own tables
// when the instance is created, so a process that takes the work over receives
// exactly what the process that started it was given.
type Params struct {
	// SideEffectFile is the file the activity appends its line to.
	SideEffectFile string `json:"sideEffectFile"`
	// ReleaseFile is the file whose appearance ends the activity's block.
	ReleaseFile string `json:"releaseFile"`
	// HoldSeconds bounds that block, so a run whose driver died never hangs a
	// process.
	HoldSeconds int `json:"holdSeconds"`
}

// PromptWorkflow is the Legion step under test: one activity that writes a side
// effect and then waits, as a prompt into a live session does.
func PromptWorkflow(ctx *task.WorkflowContext) (any, error) {
	var p Params
	if err := ctx.GetInput(&p); err != nil {
		return nil, err
	}
	var result dtx.Result
	if err := ctx.CallActivity(ActivityName, task.WithActivityInput(p)).Await(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// Prompt performs the side effect and then blocks. The order is the whole point: the
// effect is in the world before anything about this attempt is in the database, which
// is the state a kill has to find.
func Prompt(ctx task.ActivityContext) (any, error) {
	var p Params
	if err := ctx.GetInput(&p); err != nil {
		return nil, err
	}
	lines, err := libx.ReadLines(p.SideEffectFile)
	if err != nil {
		return nil, err
	}
	attempt := len(lines) + 1
	line := fmt.Sprintf("attempt=%d pid=%d at=%s", attempt, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := libx.AppendLine(p.SideEffectFile, line); err != nil {
		return nil, err
	}
	hold := time.Duration(p.HoldSeconds) * time.Second
	if !libx.WaitForFile(ctx.Context(), p.ReleaseFile, hold) {
		return nil, fmt.Errorf("the activity was never released: %s did not appear within %s", p.ReleaseFile, hold)
	}
	return dtx.Result{Attempt: attempt, PID: os.Getpid()}, nil
}

// Registry is the workflow and the activity, registered under the names the driver
// reads back out of the database.
func Registry() (*task.TaskRegistry, error) {
	r := task.NewTaskRegistry()
	if err := r.AddWorkflowN(WorkflowName, PromptWorkflow); err != nil {
		return nil, fmt.Errorf("register the workflow: %w", err)
	}
	if err := r.AddActivityN(ActivityName, Prompt); err != nil {
		return nil, fmt.Errorf("register the activity: %w", err)
	}
	return r, nil
}

// Worker is cmd/dtworker's experiment 16 mode: a process that runs the engine and
// waits to be killed. With Start it also creates the instance; without it, it is the
// replacement, and the only thing it does is poll -- which is where recovery happens,
// if it happens at all.
func Worker(ctx context.Context, cfg libx.WorkerConfig) error {
	r, err := Registry()
	if err != nil {
		return err
	}
	engine, err := dtx.Start(ctx, r, dtx.Options{
		DSN: cfg.DSN, Schema: cfg.Schema,
		WorkflowLockTimeout: WorkflowLockTimeout,
		ActivityLockTimeout: ActivityLockTimeout,
	})
	if err != nil {
		return err
	}
	defer engine.Shutdown(context.WithoutCancel(ctx))
	libx.Field(FieldPrefix, "pid", os.Getpid())

	if cfg.Start {
		params := Params{
			SideEffectFile: SideEffectFile(cfg.WorkDir),
			ReleaseFile:    ReleaseFile(cfg.WorkDir),
			HoldSeconds:    int(Hold / time.Second),
		}
		id, err := engine.Client.ScheduleNewWorkflow(ctx, WorkflowName,
			api.WithInstanceID(api.InstanceID(cfg.InstanceID)), api.WithInput(params))
		if err != nil {
			return fmt.Errorf("create instance %s: %w", cfg.InstanceID, err)
		}
		libx.Field(FieldPrefix, "workflow-started", string(id))
	} else {
		libx.Field(FieldPrefix, "replacement-polling", true)
	}

	<-ctx.Done()
	return nil
}
