// Package exp17 is experiment 17 of the evaluation: what a durabletask-go instance
// costs while it waits for a person.
//
// Legion waits for people: an architect approves a spec, a reviewer answers a
// question, and the issue stands still meanwhile -- for hours or for days. The
// question here is what such a wait holds: a goroutine, a database connection, a row,
// a lock, and what the recorded state of a waiting instance is. Then the answer
// arrives and the workflow has to go on from where it waited, including when the
// process that was waiting died in the meantime.
//
// The workflow lives here because both cmd/dtworker, which hosts it, and the driver,
// which reads what it left in PostgreSQL, need its names.
package exp17

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/dapr/durabletask-go/api"
	"github.com/dapr/durabletask-go/task"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dtx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The names this experiment's pieces are registered and recorded under.
const (
	WorkflowName  = "exp17HumanGateWorkflow"
	AskActivity   = "ask-the-person"
	ApplyActivity = "apply-the-answer"
	// AnswerEvent is the external event the person's answer arrives as.
	AnswerEvent = "approval"
	// FieldPrefix is what cmd/dtworker puts in front of every line its driver reads.
	FieldPrefix = "dtworker"
)

// The timings this experiment runs the library with.
const (
	WorkflowLockTimeout = 30 * time.Second
	ActivityLockTimeout = 30 * time.Second
	// Wait is what the gate's WaitForSingleEvent is given. A negative timeout is
	// the library's unbounded wait: it still creates a durable timer, with a fire
	// time of the year 9999, rather than no timer at all.
	Wait = -1 * time.Second
	// baselineSettle is how long a worker lets the engine's own goroutines start
	// before it counts them, so the count the wait is measured against is the
	// process at rest.
	baselineSettle = 1500 * time.Millisecond
	// idleSettle is how long after the wait began the process is counted again, to
	// show whether anything the wait holds is let go of on its own.
	idleSettle      = 8 * time.Second
	workerPollEvery = 250 * time.Millisecond
)

// QuestionFile is where the activity before the wait writes its line: one line per
// time that activity really ran. It stays at one line across a restart, which is how
// the experiment shows that work whose outcome was recorded is not run again.
func QuestionFile(workDir string) string {
	return filepath.Join(workDir, "exp17-question.log")
}

// DecisionFile is where the activity after the wait writes what the answer licensed.
func DecisionFile(workDir string) string {
	return filepath.Join(workDir, "exp17-decision.log")
}

// Params is one gate.
type Params struct {
	IssueID      string `json:"issueID"`
	QuestionFile string `json:"questionFile"`
	DecisionFile string `json:"decisionFile"`
}

// HumanGateWorkflow asks a person and waits for the answer.
func HumanGateWorkflow(ctx *task.WorkflowContext) (any, error) {
	var p Params
	if err := ctx.GetInput(&p); err != nil {
		return nil, err
	}
	var question string
	if err := ctx.CallActivity(AskActivity, task.WithActivityInput(p)).Await(&question); err != nil {
		return nil, err
	}

	var answer string
	if err := ctx.WaitForSingleEvent(AnswerEvent, Wait).Await(&answer); err != nil {
		return nil, fmt.Errorf("waiting for the person's answer: %w", err)
	}

	var decision string
	if err := ctx.CallActivity(ApplyActivity,
		task.WithActivityInput(applyInput{Params: p, Answer: answer})).Await(&decision); err != nil {
		return nil, err
	}
	return decision, nil
}

// applyInput is what the activity after the wait is given: the gate and the answer.
type applyInput struct {
	Params Params `json:"params"`
	Answer string `json:"answer"`
}

// Ask is the activity before the wait: it writes the question down, as Legion would
// write an ask into Dispatch.
func Ask(ctx task.ActivityContext) (any, error) {
	var p Params
	if err := ctx.GetInput(&p); err != nil {
		return nil, err
	}
	line := fmt.Sprintf("issue=%s question=approve-the-spec asked-by-pid=%d", p.IssueID, os.Getpid())
	if err := libx.AppendLine(p.QuestionFile, line); err != nil {
		return nil, err
	}
	return line, nil
}

// Apply is the activity after the wait: it records what the answer licensed.
func Apply(ctx task.ActivityContext) (any, error) {
	var in applyInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	line := fmt.Sprintf("issue=%s answer=%s applied-by-pid=%d", in.Params.IssueID, in.Answer, os.Getpid())
	if err := libx.AppendLine(in.Params.DecisionFile, line); err != nil {
		return nil, err
	}
	return line, nil
}

// Registry is the workflow and its two activities, registered under the names the
// driver reads back out of the database.
func Registry() (*task.TaskRegistry, error) {
	r := task.NewTaskRegistry()
	if err := r.AddWorkflowN(WorkflowName, HumanGateWorkflow); err != nil {
		return nil, fmt.Errorf("register the workflow: %w", err)
	}
	if err := r.AddActivityN(AskActivity, Ask); err != nil {
		return nil, fmt.Errorf("register the ask activity: %w", err)
	}
	if err := r.AddActivityN(ApplyActivity, Apply); err != nil {
		return nil, fmt.Errorf("register the apply activity: %w", err)
	}
	return r, nil
}

// Options is how every process of this experiment runs the library, driver included.
func Options(dsn, schema string) dtx.Options {
	return dtx.Options{
		DSN: dsn, Schema: schema,
		WorkflowLockTimeout: WorkflowLockTimeout,
		ActivityLockTimeout: ActivityLockTimeout,
	}
}

// Worker is cmd/dtworker's experiment 17 mode. It hosts the gate, reports how many
// goroutines this process holds before the workflow exists, once the workflow is
// really waiting, and again some seconds later, and then waits to be stopped.
func Worker(ctx context.Context, cfg libx.WorkerConfig) error {
	db, err := libx.Open(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
		return err
	}
	defer db.Close()

	r, err := Registry()
	if err != nil {
		return err
	}
	engine, err := dtx.Start(ctx, r, Options(cfg.DSN, cfg.Schema))
	if err != nil {
		return err
	}
	defer engine.Shutdown(context.WithoutCancel(ctx))
	libx.Field(FieldPrefix, "pid", os.Getpid())

	if err := libx.Sleep(ctx, baselineSettle); err != nil {
		return err
	}
	libx.Field(FieldPrefix, "goroutines-before-the-workflow", runtime.NumGoroutine())

	if cfg.Start {
		params := Params{
			IssueID:      "INFRA-1546",
			QuestionFile: QuestionFile(cfg.WorkDir),
			DecisionFile: DecisionFile(cfg.WorkDir),
		}
		if _, err := engine.Client.ScheduleNewWorkflow(ctx, WorkflowName,
			api.WithInstanceID(api.InstanceID(cfg.InstanceID)), api.WithInput(params)); err != nil {
			return fmt.Errorf("create instance %s: %w", cfg.InstanceID, err)
		}
		libx.Field(FieldPrefix, "workflow-started", cfg.InstanceID)
	} else {
		libx.Field(FieldPrefix, "replacement-polling", true)
	}

	if err := waitUntilWaiting(ctx, db, cfg.InstanceID); err != nil {
		return err
	}
	libx.Field(FieldPrefix, "goroutines-while-waiting", runtime.NumGoroutine())
	if err := libx.Sleep(ctx, idleSettle); err != nil {
		return err
	}
	libx.Field(FieldPrefix, "goroutines-while-still-waiting", runtime.NumGoroutine())

	<-ctx.Done()
	return nil
}

// waitUntilWaiting blocks until the instance is blocked on its external event: the
// ask activity is in history, no activity is outstanding, and the instance is not
// locked by anyone.
func waitUntilWaiting(ctx context.Context, db *sql.DB, instanceID string) error {
	for {
		instance, err := dtx.ReadInstance(ctx, db, instanceID)
		if err != nil {
			return err
		}
		history, err := dtx.ReadHistory(ctx, db, instanceID)
		if err != nil {
			return err
		}
		tasks, err := dtx.ReadNewTasks(ctx, db, instanceID)
		if err != nil {
			return err
		}
		waiting := instance.Found && !instance.Completed() &&
			dtx.CountEvents(history, "TaskCompleted") >= 1 &&
			dtx.CountEvents(history, "TimerCreated") >= 1 &&
			len(tasks) == 0 && instance.LockExpiration == nil
		if waiting {
			return nil
		}
		if err := libx.Sleep(ctx, workerPollEvery); err != nil {
			return err
		}
	}
}
