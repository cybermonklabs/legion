// Package exp8 is experiment 8 of the evaluation: what a DBOS workflow costs while it
// waits for a person.
//
// Legion waits for people: an architect approves a spec, a reviewer answers a question,
// and the issue stands still meanwhile -- for hours or for days. The question here is
// what such a wait holds: a goroutine, a database connection, a row, a lock, and what
// the recorded state of a waiting workflow is. Then the answer arrives and the workflow
// has to go on from where it waited, including when the process that was waiting died
// in the meantime.
//
// The workflow lives here because both cmd/dbosworker, which hosts it, and the driver,
// which reads what it left in PostgreSQL, need its names.
package exp8

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
)

// The names this experiment's pieces are registered and recorded under.
const (
	WorkflowName    = "exp8HumanGateWorkflow"
	AskStepName     = "ask-the-person"
	ApplyStepName   = "apply-the-answer"
	AnswerTopic     = "approval"
	AppName         = "legion-exp8"
	AppVersion      = "exp8-code-1"
	ExecutorID      = "exp8-gate-host"
	sleepStepName   = "DBOS.sleep"
	recvStepName    = "DBOS.recv"
	workerPollEvery = 200 * time.Millisecond
	// baselineSettle is how long a worker lets DBOS's own goroutines start before it
	// counts them, so the count the wait is measured against is the process at rest.
	baselineSettle = 1500 * time.Millisecond
)

// QuestionFile is where the step before the wait writes its line: one line per time
// that step really ran. It stays at one line across a restart, which is how the
// experiment shows that a step whose outcome was recorded is not run again.
func QuestionFile(workDir string) string {
	return filepath.Join(workDir, "exp8-question.log")
}

// DecisionFile is where the step after the wait writes what the answer licensed.
func DecisionFile(workDir string) string {
	return filepath.Join(workDir, "exp8-decision.log")
}

// Params is one gate.
type Params struct {
	// IssueID is the issue whose question is waiting for a person.
	IssueID string `json:"issueID"`
	// QuestionFile and DecisionFile are where the two steps write their lines.
	QuestionFile string `json:"questionFile"`
	DecisionFile string `json:"decisionFile"`
	// Wait bounds the wait for the person. DBOS's Recv has no unbounded form: it
	// takes a timeout and records the deadline it computes as a durable step, so a
	// wait of days is a Recv whose timeout is days.
	Wait time.Duration `json:"wait"`
}

// Result is what the gate did.
type Result struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Decision string `json:"decision"`
	// WaitedSeconds is how long the Recv call itself took, across every process that
	// ran it: the last one reports only its own share.
	WaitedSeconds float64 `json:"waitedSeconds"`
}

// HumanGateWorkflow asks a person and waits for the answer. Everything before the wait
// is a durable step, so a restart replays it from its record rather than asking twice.
func HumanGateWorkflow(ctx dbos.Context, p Params) (Result, error) {
	question, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		text := fmt.Sprintf("issue=%s question=approve-the-spec asked-by-pid=%d", p.IssueID, os.Getpid())
		if err := dbosx.AppendLine(p.QuestionFile, text); err != nil {
			return "", err
		}
		return text, nil
	}, dbos.WithStepName(AskStepName))
	if err != nil {
		return Result{}, err
	}

	start := time.Now()
	answer, err := dbos.Recv[string](ctx, AnswerTopic, p.Wait)
	if err != nil {
		return Result{}, fmt.Errorf("waiting for the person's answer: %w", err)
	}
	waited := time.Since(start)

	decision, err := dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		text := fmt.Sprintf("issue=%s answer=%s applied-by-pid=%d", p.IssueID, answer, os.Getpid())
		if err := dbosx.AppendLine(p.DecisionFile, text); err != nil {
			return "", err
		}
		return text, nil
	}, dbos.WithStepName(ApplyStepName))
	if err != nil {
		return Result{}, err
	}
	return Result{Question: question, Answer: answer, Decision: decision, WaitedSeconds: waited.Seconds()}, nil
}

// Worker is cmd/dbosworker's experiment 8 mode. It hosts the gate, reports how many
// goroutines this process holds before the workflow exists and again once the workflow
// is really waiting, and then waits to be stopped.
func Worker(ctx context.Context, cfg dbosx.WorkerConfig, shutdown time.Duration) error {
	dbosCtx, err := dbosx.Open(ctx, cfg.Options())
	if err != nil {
		return err
	}
	defer dbos.Shutdown(dbosCtx, shutdown) //nolint:errcheck // a killed process never reaches this
	pool, err := dbosx.Pool(dbosCtx)
	if err != nil {
		return err
	}

	dbos.RegisterWorkflow(dbosCtx, HumanGateWorkflow, dbos.WithWorkflowName(WorkflowName))
	if err := dbos.Launch(dbosCtx); err != nil {
		return err
	}
	dbosx.Field("pid", os.Getpid())
	// DBOS starts its own goroutines -- the queue runner, the scheduler, the
	// notification listener -- as Launch returns, so a count taken at once would
	// charge some of them to the wait. The baseline is taken once they are up.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(baselineSettle):
	}
	dbosx.Field("goroutines-before-the-workflow", runtime.NumGoroutine())

	if cfg.Start {
		params := Params{
			IssueID:      "INFRA-1546",
			QuestionFile: QuestionFile(cfg.WorkDir),
			DecisionFile: DecisionFile(cfg.WorkDir),
			Wait:         Wait,
		}
		if _, err := dbos.RunWorkflow(dbosCtx, HumanGateWorkflow, params, dbos.WithWorkflowID(cfg.WorkflowID)); err != nil {
			return fmt.Errorf("start %s: %w", cfg.WorkflowID, err)
		}
		dbosx.Field("workflow-started", cfg.WorkflowID)
	} else {
		dbosx.Field("recovery-launched", true)
	}

	// Report what the process holds once the workflow is really inside the wait. DBOS
	// checkpoints the wait's deadline as a step before it blocks, so that row
	// appearing while the workflow is PENDING is the moment the wait began.
	if err := waitUntilWaiting(ctx, pool, cfg); err != nil {
		return err
	}
	dbosx.Field("goroutines-while-waiting", runtime.NumGoroutine())

	<-ctx.Done()
	return nil
}

// waitUntilWaiting blocks until this process's workflow is blocked in its Recv.
func waitUntilWaiting(ctx context.Context, pool dbos.Pool, cfg dbosx.WorkerConfig) error {
	for {
		workflow, err := dbosx.ReadWorkflow(ctx, pool, cfg.Schema, cfg.WorkflowID)
		if err != nil {
			return err
		}
		steps, err := dbosx.ReadSteps(ctx, pool, cfg.Schema, cfg.WorkflowID)
		if err != nil {
			return err
		}
		waiting := workflow.Found && workflow.Status == string(dbos.WorkflowStatusPending) &&
			workflow.ExecutorID == cfg.ExecutorID
		for _, step := range steps {
			if step.Name == recvStepName {
				waiting = false // the wait is over: the message was consumed
			}
		}
		if waiting && hasStep(steps, sleepStepName) {
			// A moment for the goroutine count to settle on the blocked wait.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(workerPollEvery):
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(workerPollEvery):
		}
	}
}

func hasStep(steps []dbosx.Step, name string) bool {
	for _, step := range steps {
		if step.Name == name {
			return true
		}
	}
	return false
}
