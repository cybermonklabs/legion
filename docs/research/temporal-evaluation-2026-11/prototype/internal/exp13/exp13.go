// Package exp13 is experiment 13 of the evaluation: what go-workflows does when the
// process holding an activity is killed while that activity is running.
//
// It is the counterpart of experiments 3, 7 and 10, which asked Temporal, DBOS and
// Restate the same question. A Legion step is a prompt into a session that already
// holds state: it writes somewhere the moment it starts, and it must not be run twice.
// The workflow here has one activity that stands for such a prompt -- it appends a
// line to a file, naming its attempt and its process, and then blocks -- and the
// driver kills the process that is inside it.
//
// The number the experiment exists to produce is how many times that line was written.
//
// The workflow lives here because both cmd/gwworker, which hosts it, and the driver,
// which reads what it left in PostgreSQL, need its names.
package exp13

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/registry"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/gwx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The names this experiment's pieces are registered and recorded under. They are
// explicit so the driver and the worker, two different binaries, agree on them.
const (
	WorkflowName = "exp13PromptWorkflow"
	ActivityName = "prompt-into-session"
	// FieldPrefix is what cmd/gwworker puts in front of every line its driver reads.
	FieldPrefix = "gwworker"
)

// The timings this experiment runs the library with. The activity lock is the one
// number the whole measurement turns on, so it is named here and printed as a fact.
const (
	// ActivityLockTimeout replaces the library's documented default of two minutes.
	// A locked activity is nobody else's until this lapses, and the holder extends it
	// every ActivityHeartbeatInterval; a pod experiment cannot sit out two minutes of
	// lapse, so the run shortens the lock and says so rather than waiting.
	ActivityLockTimeout = 10 * time.Second
	// ActivityHeartbeatInterval must be shorter than the lock, or a live worker would
	// lose the activity it is still running.
	ActivityHeartbeatInterval = 2 * time.Second
	// WorkflowLockTimeout and WorkflowHeartbeatInterval are the same two numbers for
	// a workflow task, which this experiment does not kill anything inside of.
	WorkflowLockTimeout       = 30 * time.Second
	WorkflowHeartbeatInterval = 5 * time.Second
	// StickyTimeout is how long an instance stays bound to the worker that last ran
	// a workflow task for it, shortened so a dead worker's stickiness cannot be
	// mistaken for the activity lock this experiment measures.
	StickyTimeout = 5 * time.Second
	// Hold bounds the block inside the activity. It must outlast the kill, the
	// replacement process's start, the lock's lapse and the driver's release, and
	// still end a run whose driver died.
	Hold = 70 * time.Second
)

// SideEffectFile is where the activity writes its side effect: one line per time the
// activity body really ran. Its line count is the experiment's central number.
func SideEffectFile(workDir string) string {
	return filepath.Join(workDir, "exp13-side-effects.log")
}

// ReleaseFile is what the driver creates to let a blocked activity finish, standing
// for the session answering at last.
func ReleaseFile(workDir string) string {
	return filepath.Join(workDir, "exp13-release")
}

// Params is one run of PromptWorkflow. It is serialized into the library's own tables
// when the instance is created, so a process that takes the work over receives exactly
// what the process that started it was given.
type Params struct {
	// SideEffectFile is the file the activity appends its line to.
	SideEffectFile string `json:"sideEffectFile"`
	// ReleaseFile is the file whose appearance ends the activity's block.
	ReleaseFile string `json:"releaseFile"`
	// Hold bounds that block, so a run whose driver died never hangs a process.
	Hold time.Duration `json:"hold"`
}

// PromptWorkflow is the Legion step under test: one activity that writes a side effect
// and then waits, as a prompt into a live session does. The activity is given one
// attempt, so nothing the workflow itself does can re-run it: a second performance can
// only come from the backend handing the activity out again.
func PromptWorkflow(ctx workflow.Context, p Params) (gwx.Result, error) {
	return workflow.ExecuteActivity[gwx.Result](ctx, workflow.ActivityOptions{
		RetryOptions: workflow.RetryOptions{MaxAttempts: 1},
	}, ActivityName, p).Get(ctx)
}

// Prompt performs the side effect and then blocks. The order is the whole point: the
// effect is in the world before anything about this attempt is in the database, which
// is the state a kill has to find.
func Prompt(ctx context.Context, p Params) (gwx.Result, error) {
	lines, err := libx.ReadLines(p.SideEffectFile)
	if err != nil {
		return gwx.Result{}, err
	}
	attempt := len(lines) + 1
	line := fmt.Sprintf("attempt=%d pid=%d at=%s", attempt, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := libx.AppendLine(p.SideEffectFile, line); err != nil {
		return gwx.Result{}, err
	}
	if !libx.WaitForFile(ctx, p.ReleaseFile, p.Hold) {
		return gwx.Result{}, fmt.Errorf("the activity was never released: %s did not appear within %s", p.ReleaseFile, p.Hold)
	}
	return gwx.Result{Attempt: attempt, PID: os.Getpid()}, nil
}

// Worker is cmd/gwworker's experiment 13 mode: a process that hosts the workflow and
// its activity and waits to be killed. With Start it also creates the instance;
// without it, it is the replacement, and the only thing it does is poll -- which is
// where recovery happens, if it happens at all.
func Worker(ctx context.Context, cfg libx.WorkerConfig) error {
	db, err := libx.Open(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
		return err
	}
	defer db.Close()

	be := gwx.Backend(db, gwx.Options{
		WorkerName:          cfg.WorkerName,
		ActivityLockTimeout: ActivityLockTimeout,
		WorkflowLockTimeout: WorkflowLockTimeout,
		StickyTimeout:       StickyTimeout,
	})
	defer be.Close() //nolint:errcheck // a killed process never reaches this

	w := worker.New(be, &worker.Options{
		WorkflowWorkerOptions: worker.WorkflowWorkerOptions{
			WorkflowPollers:           1,
			WorkflowHeartbeatInterval: WorkflowHeartbeatInterval,
		},
		ActivityWorkerOptions: worker.ActivityWorkerOptions{
			ActivityPollers:           1,
			ActivityHeartbeatInterval: ActivityHeartbeatInterval,
		},
	})
	if err := w.RegisterWorkflow(PromptWorkflow, registry.WithName(WorkflowName)); err != nil {
		return fmt.Errorf("register the workflow: %w", err)
	}
	if err := w.RegisterActivity(Prompt, registry.WithName(ActivityName)); err != nil {
		return fmt.Errorf("register the activity: %w", err)
	}
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("start the worker: %w", err)
	}
	libx.Field(FieldPrefix, "pid", os.Getpid())
	libx.Field(FieldPrefix, "worker-name", cfg.WorkerName)

	if cfg.Start {
		c := client.New(be)
		params := Params{
			SideEffectFile: SideEffectFile(cfg.WorkDir),
			ReleaseFile:    ReleaseFile(cfg.WorkDir),
			Hold:           Hold,
		}
		instance, err := c.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{
			InstanceID: cfg.InstanceID,
		}, WorkflowName, params)
		if err != nil {
			return fmt.Errorf("create instance %s: %w", cfg.InstanceID, err)
		}
		libx.Field(FieldPrefix, "workflow-started", instance.InstanceID)
	} else {
		libx.Field(FieldPrefix, "replacement-polling", true)
	}

	<-ctx.Done()
	return nil
}
