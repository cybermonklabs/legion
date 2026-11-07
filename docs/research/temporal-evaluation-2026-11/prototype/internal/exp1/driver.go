package exp1

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The experiment's timings. The whole run is signals and one trivial activity per phase,
// so seconds are enough.
const (
	dialTimeout     = 30 * time.Second
	workflowTimeout = 90 * time.Second
	waitTimeout     = 60 * time.Second
)

// The files of Legion's own engine this experiment quotes the size of, relative to the
// repository root.
var legionSources = []string{
	"packages/daemon/internal/workflow/table.go",
	"packages/daemon/internal/workflow/engine.go",
}

// Run is experiment 1: it runs Legion's lifecycle as one workflow, drives it with the
// twelve triggers of Script -- which include a rejected review, so the issue goes
// backwards from reviewing to implementing and through testing a second time -- and
// checks the run's own history for the evidence.
func Run(ctx context.Context, cfg harness.Config) error {
	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	stamp := time.Now().UnixNano()
	queue := fmt.Sprintf("exp1-%d", stamp)
	w := worker.New(c, queue, worker.Options{})
	Register(w)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	issueID := fmt.Sprintf("EXP1-%d", stamp)
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       fmt.Sprintf("exp1-issue-%d", stamp),
		TaskQueue:                queue,
		WorkflowExecutionTimeout: workflowTimeout,
	}, WorkflowName, Params{IssueID: issueID})
	if err != nil {
		return fmt.Errorf("start workflow: %w", err)
	}
	harness.Fact("issue-id", issueID)
	harness.Fact("workflow-id", run.GetID())
	harness.Fact("run-id", run.GetRunID())

	sent, err := Drive(ctx, c, run.GetID())
	harness.Fact("signals-sent", sent)
	if err != nil {
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	var res Result
	if err := run.Get(waitCtx, &res); err != nil {
		return fmt.Errorf("workflow %s did not return: %w", run.GetID(), err)
	}
	harness.Fact("final-phase", res.Phase)
	harness.Fact("phase-path", strings.Join(res.Path, " -> "))
	harness.Fact("triggers-consumed", res.Triggers)
	harness.Fact("triggers-no-row-accepted", res.Ignored)
	harness.Fact("role-prompts", res.Prompts)

	backward := HasStep(res.Path, PhaseReviewing, PhaseImplementing)
	testingEntries := Entries(res.Path, PhaseTesting)
	harness.Fact("backward-move-reviewing-to-implementing", backward)
	harness.Fact("testing-entries", testingEntries)

	events, err := harness.History(ctx, c, run.GetID(), run.GetRunID())
	if err != nil {
		return err
	}
	signalled, phaseEvents := 0, 0
	for _, event := range events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
			signalled++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			phaseEvents++
		}
	}
	harness.Fact("history-event-count", len(events))
	harness.Fact("history-signal-events", signalled)
	harness.Fact("history-activity-scheduled-events", phaseEvents)

	if err := reportLines(cfg); err != nil {
		return err
	}
	harness.Fact("limit", "one issue, one happy path with one rejection, on one dev server: it shows the lifecycle fits in a workflow, not that Legion's scale of issues, guards, effects and statuses does")

	switch {
	case res.Phase != PhaseDone:
		return fmt.Errorf("the workflow ended in %s, not %s", res.Phase, PhaseDone)
	case !backward:
		return fmt.Errorf("no backward move from %s to %s in the path %v", PhaseReviewing, PhaseImplementing, res.Path)
	case testingEntries < 2:
		return fmt.Errorf("the issue entered %s %d time(s), not twice, in the path %v", PhaseTesting, testingEntries, res.Path)
	case signalled != len(Script):
		return fmt.Errorf("history holds %d signal events, not the %d sent", signalled, len(Script))
	}
	return nil
}

// Drive sends the Script's triggers, in order, to a running workflow. The server keeps
// a workflow's signals in the order they arrive, so sending them without waiting for the
// workflow to catch up still drives the phases in order.
func Drive(ctx context.Context, c client.Client, workflowID string) (int, error) {
	for i, trigger := range Script {
		if err := c.SignalWorkflow(ctx, workflowID, "", TriggerSignal, trigger); err != nil {
			return i, fmt.Errorf("signal %s with %s: %w", workflowID, trigger, err)
		}
	}
	return len(Script), nil
}

// HasStep reports whether the path ever went straight from one phase to another.
func HasStep(path []string, from, to string) bool {
	for i := 0; i+1 < len(path); i++ {
		if path[i] == from && path[i+1] == to {
			return true
		}
	}
	return false
}

// Entries is how often the path entered a phase.
func Entries(path []string, phase string) int {
	entries := 0
	for _, stood := range path {
		if stood == phase {
			entries++
		}
	}
	return entries
}

// reportLines prints the size of the workflow against the size of what Legion runs
// today: the transition table and the engine that interprets it, read from the
// repository at run time so the numbers cannot go stale.
func reportLines(cfg harness.Config) error {
	file, lines, err := harness.FuncLines(PhaseWorkflow)
	if err != nil {
		return err
	}
	harness.Fact("workflow-source-file", file)
	harness.Fact("workflow-lines-of-go", lines)

	root, err := harness.RepoRoot(cfg.RepoRoot)
	if err != nil {
		return err
	}
	harness.Fact("legion-repo-root", root)
	total := 0
	for _, rel := range legionSources {
		path := filepath.Join(root, rel)
		count, err := harness.FileLines(path)
		if err != nil {
			return err
		}
		name := strings.ReplaceAll(filepath.Base(rel), ".", "-")
		harness.Fact("legion-"+name+"-path", path)
		harness.Fact("legion-"+name+"-lines", count)
		total += count
	}
	harness.Fact("legion-phase-machine-lines-total", total)
	return nil
}
