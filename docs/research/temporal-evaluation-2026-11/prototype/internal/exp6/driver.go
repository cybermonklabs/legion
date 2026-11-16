package exp6

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/temporalproto"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp1"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

const (
	dialTimeout     = 30 * time.Second
	workflowTimeout = 2 * time.Minute
	waitTimeout     = 90 * time.Second
	// issues is how many issue workflows the run drives. More than one, so the counts
	// per issue, per phase and per prompt are divisions rather than restatements.
	issues = 3
	// visibilityTimeout bounds the wait for the server's index to show the run's
	// workflows, which it fills in asynchronously.
	visibilityTimeout = 30 * time.Second
	// metricInterval and metricTimeout bound the wait for the action metric to hold
	// still over two reads.
	metricInterval = 2 * time.Second
	metricTimeout  = 40 * time.Second
)

// Run is experiment 6: record a real run's history, replay it against a changed
// workflow and against the same change behind GetVersion, then count the run's Actions
// from the server's metric and from the history, and show the difference.
func Run(ctx context.Context, cfg harness.Config) error {
	if cfg.MetricsURL == "" {
		return fmt.Errorf("-metrics-url is required by experiment 6: without the server's own action metric there is only one count, and the experiment is a comparison of two")
	}
	workDir, err := harness.WorkDir(cfg)
	if err != nil {
		return err
	}

	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	// 1. The run: the phase workflow of experiment 1, driven by the same script.
	stamp := time.Now().UnixNano()
	queue := fmt.Sprintf("exp6-%d", stamp)
	w := worker.New(c, queue, worker.Options{})
	exp1.Register(w)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	runs := make([]client.WorkflowRun, 0, issues)
	for i := 1; i <= issues; i++ {
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:                       fmt.Sprintf("exp6-issue-%d-%03d", stamp, i),
			TaskQueue:                queue,
			WorkflowExecutionTimeout: workflowTimeout,
		}, exp1.WorkflowName, exp1.Params{IssueID: fmt.Sprintf("EXP6-%d-%03d", stamp, i)})
		if err != nil {
			return fmt.Errorf("start issue %d: %w", i, err)
		}
		runs = append(runs, run)
	}
	runPhases, runPrompts, runSignals := 0, 0, 0
	for _, run := range runs {
		sent, err := exp1.Drive(ctx, c, run.GetID())
		runSignals += sent
		if err != nil {
			return err
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for _, run := range runs {
		var res exp1.Result
		if err := run.Get(waitCtx, &res); err != nil {
			return fmt.Errorf("issue %s did not return: %w", run.GetID(), err)
		}
		if res.Phase != exp1.PhaseDone {
			return fmt.Errorf("issue %s ended in %s, not done", run.GetID(), res.Phase)
		}
		runPhases += len(res.Path) - 1
		runPrompts += res.Prompts
	}
	harness.Fact("run-issues", len(runs))
	harness.Fact("run-phases-entered", runPhases)
	harness.Fact("run-role-prompts", runPrompts)
	harness.Fact("run-signals-sent", runSignals)

	// 2. The history, written where the replay can read it back.
	recorded := runs[0]
	events, err := harness.History(ctx, c, recorded.GetID(), recorded.GetRunID())
	if err != nil {
		return err
	}
	historyFile := filepath.Join(workDir, fmt.Sprintf("exp6-history-%s.json", recorded.GetID()))
	if err := writeHistory(historyFile, events); err != nil {
		return err
	}
	info, err := os.Stat(historyFile)
	if err != nil {
		return fmt.Errorf("stat %s: %w", historyFile, err)
	}
	harness.Fact("recorded-workflow-id", recorded.GetID())
	harness.Fact("recorded-history-file", historyFile)
	harness.Fact("recorded-history-bytes", info.Size())
	harness.Fact("recorded-history-events", len(events))

	replayed, err := readHistory(historyFile)
	if err != nil {
		return err
	}
	harness.Fact("history-events-read-back", len(replayed.GetEvents()))

	// 3. The two replays.
	changedErr := replay(ChangedPhaseWorkflow, replayed)
	harness.Fact("replay-of-the-changed-workflow-failed", changedErr != nil)
	harness.Fact("replay-of-the-changed-workflow-error", errorText(changedErr))
	nonDeterminism := changedErr != nil && strings.Contains(changedErr.Error(), "nondeterministic workflow")
	harness.Fact("replay-of-the-changed-workflow-is-non-determinism", nonDeterminism)

	versionedErr := replay(VersionedPhaseWorkflow, replayed)
	harness.Fact("replay-of-the-versioned-workflow-failed", versionedErr != nil)
	harness.Fact("replay-of-the-versioned-workflow-error", errorText(versionedErr))
	harness.Fact("change-made-safe-by", "workflow.GetVersion("+VersionChangeID+")")

	// 4. The first count: the mapping of the namespace's histories onto the published
	// list of billable Actions.
	executions, err := listExecutions(ctx, c, cfg.Namespace, runs, visibilityTimeout)
	if err != nil {
		return err
	}
	mapped := &Mapped{}
	nsIssues, nsPhases, nsPrompts := 0, 0, 0
	for _, execution := range executions {
		history, err := harness.History(ctx, c, execution.GetExecution().GetWorkflowId(), execution.GetExecution().GetRunId())
		if err != nil {
			return err
		}
		mapped.MapActions(history)
		if execution.GetType().GetName() != exp1.WorkflowName {
			continue
		}
		nsIssues++
		phases, prompts, err := contents(history)
		if err != nil {
			return err
		}
		nsPhases += phases
		nsPrompts += prompts
	}
	harness.Fact("namespace-executions-counted", len(executions))
	harness.Fact("namespace-history-events-counted", mapped.Events)
	harness.Fact("namespace-issues", nsIssues)
	harness.Fact("namespace-phases-entered", nsPhases)
	harness.Fact("namespace-role-prompts", nsPrompts)
	harness.Fact("namespace-holds-only-this-run", nsIssues == len(runs))
	for _, line := range mapped.Breakdown() {
		harness.Fact("action-mapping", line)
	}
	harness.Fact("actions-by-mapping-the-history", mapped.Actions)

	// 5. The second count: the server's own action metric.
	rows, metric, reads, err := AwaitStableActionFamily(ctx, cfg.MetricsURL, cfg.Namespace, metricInterval, metricTimeout)
	if err != nil {
		for _, row := range rows {
			harness.Fact("action-metric-row", row.Line)
		}
		return err
	}
	for _, row := range rows {
		harness.Fact("action-metric-row", row.Line)
	}
	harness.Fact("action-metric-rows", len(rows))
	harness.Fact("action-metric-reads-until-stable", reads)
	harness.Fact("action-metric-namespace-counted", cfg.Namespace)
	harness.Fact("action-metric-namespace-excluded", "temporal_system")
	harness.Fact("actions-by-the-server-metric", metric)

	// 6. The two counts side by side.
	gap := metric - float64(mapped.Actions)
	harness.Fact("action-count-difference-metric-minus-mapping", gap)
	harness.Fact("action-count-ratio-metric-over-mapping", ratio(metric, float64(mapped.Actions)))
	harness.Fact("action-count-gap-means", gapMeans(gap, metric))
	for _, per := range []struct {
		name  string
		count int
	}{{"issue", nsIssues}, {"phase", nsPhases}, {"role-prompt", nsPrompts}} {
		harness.Fact("actions-by-the-server-metric-per-"+per.name, ratio(metric, float64(per.count)))
		harness.Fact("actions-by-mapping-per-"+per.name, ratio(float64(mapped.Actions), float64(per.count)))
	}
	harness.Fact("limit", "three issues of one shape on one dev server: it shows that an unversioned change breaks replay and a versioned one does not, and what this namespace's Actions were counted as two ways, not that the mapping holds for workflows with children, timers, local activities or retries, nor that a dev server charges as Temporal Cloud bills")

	switch {
	case changedErr == nil:
		return fmt.Errorf("replaying the recorded history against the changed workflow succeeded, so this run shows no cost to an unversioned change")
	case !nonDeterminism:
		return fmt.Errorf("the changed workflow's replay failed with something other than a non-determinism error: %v", changedErr)
	case versionedErr != nil:
		return fmt.Errorf("the change behind GetVersion did not replay cleanly: %w", versionedErr)
	case mapped.Actions == 0:
		return fmt.Errorf("the mapping found no Actions in %d events, so it measured nothing", mapped.Events)
	case metric == 0:
		return fmt.Errorf("the server's action metric holds nothing for namespace %s, so there is only one count", cfg.Namespace)
	}
	return nil
}

// replay runs one recorded history against one version of the workflow, registered
// under the name the history was written by.
func replay(fn any, history *historypb.History) error {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: exp1.WorkflowName})
	return replayer.ReplayWorkflowHistory(harness.Logger(), history)
}

// writeHistory saves a history where the replay can read it back. The file goes under
// -work-dir: the prototype's own tree holds Go and nothing else.
func writeHistory(path string, events []*historypb.HistoryEvent) error {
	encoded, err := temporalproto.CustomJSONMarshalOptions{Indent: "  "}.Marshal(&historypb.History{Events: events})
	if err != nil {
		return fmt.Errorf("encode the history: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readHistory loads a history the run recorded.
func readHistory(path string) (*historypb.History, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	history, err := client.HistoryFromJSON(file, client.HistoryJSONOptions{})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return history, nil
}

// listExecutions is every workflow execution the namespace holds, waited for until the
// server's index shows at least the ones this run started. Both Action counts have to
// cover the same work, and the metric counts the whole namespace.
func listExecutions(ctx context.Context, c client.Client, namespace string, runs []client.WorkflowRun, timeout time.Duration) ([]*workflowpb.WorkflowExecutionInfo, error) {
	wanted := map[string]bool{}
	for _, run := range runs {
		wanted[run.GetID()] = false
	}
	deadline := time.Now().Add(timeout)
	for {
		var found []*workflowpb.WorkflowExecutionInfo
		seen := map[string]bool{}
		var token []byte
		for {
			resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
				Namespace:     namespace,
				PageSize:      100,
				NextPageToken: token,
			})
			if err != nil {
				return nil, fmt.Errorf("list the executions of %s: %w", namespace, err)
			}
			found = append(found, resp.GetExecutions()...)
			for _, execution := range resp.GetExecutions() {
				seen[execution.GetExecution().GetWorkflowId()] = true
			}
			token = resp.GetNextPageToken()
			if len(token) == 0 {
				break
			}
		}
		missing := 0
		for id := range wanted {
			if !seen[id] {
				missing++
			}
		}
		if missing == 0 {
			return found, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("the server's index still misses %d of this run's %d workflows after %s", missing, len(wanted), timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// contents is what one issue's history says the run contained: the phases the issue
// entered and the role prompts it ran, taken from the result the workflow returned.
func contents(events []*historypb.HistoryEvent) (int, int, error) {
	for _, event := range events {
		attrs := event.GetWorkflowExecutionCompletedEventAttributes()
		if attrs == nil {
			continue
		}
		var res exp1.Result
		if err := converter.GetDefaultDataConverter().FromPayloads(attrs.GetResult(), &res); err != nil {
			return 0, 0, fmt.Errorf("decode a finished issue's result: %w", err)
		}
		return len(res.Path) - 1, res.Prompts, nil
	}
	return 0, 0, nil
}

// errorText is an error's message, or "none".
func errorText(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

// ratio is a/b to two places, or "n/a" when b is zero.
func ratio(a, b float64) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", a/b)
}

// gapMeans says plainly what the difference between the two counts is worth. The
// mapping is the rule every Action estimate rests on; the metric is what the server
// itself counted. They are only worth comparing if the reader is told which way the
// comparison came out.
func gapMeans(gap, metric float64) string {
	if gap == 0 {
		return "none: the published mapping rule, applied to this run's histories, gave exactly the count the server's own action metric gave, so a cost estimate made from a history of this shape is right about this shape of run"
	}
	share := "n/a"
	if metric != 0 {
		share = fmt.Sprintf("%.1f%% of the server's count", 100*gap/metric)
	}
	return fmt.Sprintf("the two counts differ by %.0f Actions (%s): the mapping is the rule every cost estimate uses and the metric is what the server counted, so that difference is the error in any Action estimate made from a history by that rule", gap, share)
}
