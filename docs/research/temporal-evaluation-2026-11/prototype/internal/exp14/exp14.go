// Package exp14 is experiment 14 of the evaluation: what a go-workflows instance
// costs while it waits for a person.
//
// Legion waits for people: an architect approves a spec, a reviewer answers a
// question, and the issue stands still meanwhile -- for hours or for days. The
// question here is what such a wait holds: a goroutine, a database connection, a row,
// a lock, and what the recorded state of a waiting instance is. Then the answer
// arrives and the workflow has to go on from where it waited, including when the
// process that was waiting died in the meantime.
//
// The workflow lives here because both cmd/gwworker, which hosts it, and the driver,
// which reads what it left in PostgreSQL, need its names.
package exp14

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/registry"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/gwx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// The names this experiment's pieces are registered and recorded under.
const (
	WorkflowName  = "exp14HumanGateWorkflow"
	AskActivity   = "ask-the-person"
	ApplyActivity = "apply-the-answer"
	// AnswerSignal is the signal channel the person's answer arrives on.
	AnswerSignal = "approval"
	// FieldPrefix is what cmd/gwworker puts in front of every line its driver reads.
	FieldPrefix = "gwworker"
)

// The timings this experiment runs the library with.
const (
	// StickyTimeout and CacheTTL are what keeps a waiting instance in a process:
	// after the last workflow task the instance stays bound to that worker for
	// StickyTimeout and its executor stays in memory for CacheTTL. Both are set
	// explicitly so the experiment can measure what the process holds before and
	// after they lapse.
	StickyTimeout = 10 * time.Second
	CacheTTL      = 10 * time.Second
	// WorkflowLockTimeout and the heartbeat bound a workflow task, not the wait.
	WorkflowLockTimeout       = 30 * time.Second
	WorkflowHeartbeatInterval = 5 * time.Second
	ActivityLockTimeout       = 30 * time.Second
	ActivityHeartbeatInterval = 5 * time.Second
	// baselineSettle is how long a worker lets the library's own goroutines start
	// before it counts them, so the count the wait is measured against is the
	// process at rest.
	baselineSettle = 1500 * time.Millisecond
	// cacheSettle is how long after the wait began the process is counted again,
	// past both the stickiness and the executor cache's time to live.
	cacheSettle     = 14 * time.Second
	workerPollEvery = 250 * time.Millisecond
)

// QuestionFile is where the activity before the wait writes its line: one line per
// time that activity really ran. It stays at one line across a restart, which is how
// the experiment shows that work whose outcome was recorded is not run again.
func QuestionFile(workDir string) string {
	return filepath.Join(workDir, "exp14-question.log")
}

// DecisionFile is where the activity after the wait writes what the answer licensed.
func DecisionFile(workDir string) string {
	return filepath.Join(workDir, "exp14-decision.log")
}

// Params is one gate.
type Params struct {
	// IssueID is the issue whose question is waiting for a person.
	IssueID string `json:"issueID"`
	// QuestionFile and DecisionFile are where the two activities write their lines.
	QuestionFile string `json:"questionFile"`
	DecisionFile string `json:"decisionFile"`
}

// Result is what the gate did.
type Result struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Decision string `json:"decision"`
}

// HumanGateWorkflow asks a person and waits for the answer. The wait has no deadline:
// go-workflows' signal channel is received from without a timeout, so an open-ended
// wait is the library's ordinary case rather than a long timer.
func HumanGateWorkflow(ctx workflow.Context, p Params) (Result, error) {
	question, err := workflow.ExecuteActivity[string](ctx, workflow.ActivityOptions{
		RetryOptions: workflow.RetryOptions{MaxAttempts: 1},
	}, AskActivity, p).Get(ctx)
	if err != nil {
		return Result{}, err
	}

	answer, _ := workflow.NewSignalChannel[string](ctx, AnswerSignal).Receive(ctx)

	decision, err := workflow.ExecuteActivity[string](ctx, workflow.ActivityOptions{
		RetryOptions: workflow.RetryOptions{MaxAttempts: 1},
	}, ApplyActivity, p, answer).Get(ctx)
	if err != nil {
		return Result{}, err
	}
	return Result{Question: question, Answer: answer, Decision: decision}, nil
}

// Ask is the activity before the wait: it writes the question down, as Legion would
// write an ask into Dispatch.
func Ask(ctx context.Context, p Params) (string, error) {
	line := fmt.Sprintf("issue=%s question=approve-the-spec asked-by-pid=%d", p.IssueID, os.Getpid())
	if err := libx.AppendLine(p.QuestionFile, line); err != nil {
		return "", err
	}
	return line, nil
}

// Apply is the activity after the wait: it records what the answer licensed.
func Apply(ctx context.Context, p Params, answer string) (string, error) {
	line := fmt.Sprintf("issue=%s answer=%s applied-by-pid=%d", p.IssueID, answer, os.Getpid())
	if err := libx.AppendLine(p.DecisionFile, line); err != nil {
		return "", err
	}
	return line, nil
}

// Options is how every process of this experiment runs the library, driver included,
// so a lock a fact names means the same thing everywhere.
func Options(workerName string) gwx.Options {
	return gwx.Options{
		WorkerName:          workerName,
		ActivityLockTimeout: ActivityLockTimeout,
		WorkflowLockTimeout: WorkflowLockTimeout,
		StickyTimeout:       StickyTimeout,
	}
}

// Worker is cmd/gwworker's experiment 14 mode. It hosts the gate, reports how many
// goroutines this process holds before the workflow exists, once the workflow is
// really waiting, and again once the library's stickiness and executor cache have
// lapsed, and then waits to be stopped.
func Worker(ctx context.Context, cfg libx.WorkerConfig) error {
	db, err := libx.Open(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
		return err
	}
	defer db.Close()

	be := gwx.Backend(db, Options(cfg.WorkerName))
	defer be.Close() //nolint:errcheck // a killed process never reaches this

	w := worker.New(be, &worker.Options{
		WorkflowWorkerOptions: worker.WorkflowWorkerOptions{
			WorkflowPollers:           1,
			WorkflowHeartbeatInterval: WorkflowHeartbeatInterval,
			WorkflowExecutorCacheTTL:  CacheTTL,
		},
		ActivityWorkerOptions: worker.ActivityWorkerOptions{
			ActivityPollers:           1,
			ActivityHeartbeatInterval: ActivityHeartbeatInterval,
		},
	})
	if err := w.RegisterWorkflow(HumanGateWorkflow, registry.WithName(WorkflowName)); err != nil {
		return fmt.Errorf("register the workflow: %w", err)
	}
	if err := w.RegisterActivity(Ask, registry.WithName(AskActivity)); err != nil {
		return fmt.Errorf("register the ask activity: %w", err)
	}
	if err := w.RegisterActivity(Apply, registry.WithName(ApplyActivity)); err != nil {
		return fmt.Errorf("register the apply activity: %w", err)
	}
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("start the worker: %w", err)
	}
	libx.Field(FieldPrefix, "pid", os.Getpid())

	// The library starts its own goroutines -- the pollers, the cache eviction, the
	// heartbeats -- as Start returns, so a count taken at once would charge some of
	// them to the wait. The baseline is taken once they are up.
	if err := libx.Sleep(ctx, baselineSettle); err != nil {
		return err
	}
	libx.Field(FieldPrefix, "goroutines-before-the-workflow", runtime.NumGoroutine())

	if cfg.Start {
		c := client.New(be)
		params := Params{
			IssueID:      "INFRA-1546",
			QuestionFile: QuestionFile(cfg.WorkDir),
			DecisionFile: DecisionFile(cfg.WorkDir),
		}
		if _, err := c.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{
			InstanceID: cfg.InstanceID,
		}, WorkflowName, params); err != nil {
			return fmt.Errorf("create instance %s: %w", cfg.InstanceID, err)
		}
		libx.Field(FieldPrefix, "workflow-started", cfg.InstanceID)
	} else {
		libx.Field(FieldPrefix, "replacement-polling", true)
	}

	// Report what the process holds once the workflow is really inside the wait: the
	// instance is active, its workflow task is done and no activity is outstanding.
	if err := waitUntilWaiting(ctx, db, cfg.InstanceID); err != nil {
		return err
	}
	libx.Field(FieldPrefix, "goroutines-while-waiting", runtime.NumGoroutine())
	if err := libx.Sleep(ctx, cacheSettle); err != nil {
		return err
	}
	libx.Field(FieldPrefix, "goroutines-after-the-stickiness-and-cache-lapsed", runtime.NumGoroutine())

	<-ctx.Done()
	return nil
}

// waitUntilWaiting blocks until the instance is blocked on its signal: the ask
// activity has been recorded in history, nothing is pending, and no workflow task is
// held.
func waitUntilWaiting(ctx context.Context, db *sql.DB, instanceID string) error {
	for {
		instance, err := gwx.ReadInstance(ctx, db, instanceID)
		if err != nil {
			return err
		}
		events, err := gwx.ReadHistory(ctx, db, instanceID)
		if err != nil {
			return err
		}
		pending, err := gwx.ReadPendingEvents(ctx, db, instanceID)
		if err != nil {
			return err
		}
		activities, err := gwx.ReadActivities(ctx, db, instanceID)
		if err != nil {
			return err
		}
		waiting := instance.Found && !instance.Finished() &&
			gwx.CountEvents(events, "ActivityCompleted") >= 1 &&
			len(pending) == 0 && len(activities) == 0 && instance.LockedUntil == nil
		if waiting {
			return nil
		}
		if err := libx.Sleep(ctx, workerPollEvery); err != nil {
			return err
		}
	}
}
