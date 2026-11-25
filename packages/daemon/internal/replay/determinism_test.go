package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// orderedTables is the tables of the daemon's record whose rows a read the replay runs can find
// more than one of in one statement: the issues, their phase rows, pull requests, claims and
// their pending tasks, the slots, the outbox, and the runs of steps, which the end of a step reads
// by issue, kind and place, its key less the task.
var orderedTables = []string{"issues", "phases", "pull_requests", "claims", "pending_task_deliveries", "slots", "outbox", "step_starts"}

// rowsInOrder is a replay's look after each fact (RunConfig.afterFact) that rewrites each of
// orderedTables in the order of its primary key, ascending or, with descending, descending
// (CLUSTER over an index of its own on the key's columns, made at the first fact). A read that
// takes the first of several rows, or the rows in the order Postgres returns them, with no
// statement that fixes that order, answers by where the rows stand in the table's files and by the
// plan, which differ from run to run as the server vacuums, analyzes and reuses space: rewritten in
// the two orders, two replays answer such a read the two ways.
func rowsInOrder(descending bool) func(context.Context, *store.Pool, *shadow) error {
	direction := ""
	if descending {
		direction = " desc"
	}
	made := false
	return func(ctx context.Context, pool *store.Pool, _ *shadow) error {
		for _, table := range orderedTables {
			index := "replay_test_order_" + table
			if !made {
				var columns string
				if err := pool.QueryRow(ctx, `select string_agg(quote_ident(a.attname) || $2, ', ' order by k.ord)
					from pg_index i cross join unnest(i.indkey) with ordinality as k(attnum, ord)
					join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
					where i.indrelid = $1::regclass and i.indisprimary`, table, direction).Scan(&columns); err != nil {
					return fmt.Errorf("read the primary key of %s: %w", table, err)
				}
				if _, err := pool.Exec(ctx, "create index "+index+" on "+table+" ("+columns+")"); err != nil {
					return fmt.Errorf("index %s by its key: %w", table, err)
				}
			}
			if _, err := pool.Exec(ctx, "cluster "+table+" using "+index); err != nil {
				return fmt.Errorf("rewrite %s in the order of its key: %w", table, err)
			}
		}
		made = true
		return nil
	}
}

// hostFields is what run.json measures of the run itself, which no two runs share: its start on the
// wall clock, its wall time, its mean pass time, its peak memory, and the name of its database,
// drawn so two runs on one server never meet.
var hostFields = []string{"startedAt", "wallSeconds", "meanPassMillis", "peakMemoryBytes", "database"}

// A replay is a function of its window, its rule, its branch and its seed, so a rule's difference
// from another is never noise of the run: two replays of one window under one rule, branch and seed
// write the same log, the same two scorecards and the same run.json but for what it measures of
// the run itself (hostFields), byte for byte. The test replays each window twice at once, each on a
// database of its own as Run makes one, with orderedTables kept in the order of their keys in the
// first replay's database and in the reverse order in the second's (rowsInOrder), so a read whose
// answer rests on the order Postgres returns rows in answers the two ways. It replays the largest
// fixture window, forty-two-more-in-flight; the fixture window conflict-from-fitted-rate, whose
// conflict is drawn; and manyPairsWindow, whose issues draw conflicts, give a start back to wait
// on its backoff, and number their pull requests alike where a recorded line gives one issue's
// pull request the number of another's key. It fails for each output in which the two differ,
// naming its first differing line.
func TestAReplayIsTheSameAtEveryRun(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	for _, c := range []struct {
		name, rule string
		window     Window
	}{
		{name: "forty-two-more-in-flight", rule: record.MainRule.Name, window: fixtureWindow(t, "forty-two-more-in-flight")},
		{name: "conflict-from-fitted-rate", rule: "legion-v20.42.5", window: fixtureWindow(t, "conflict-from-fitted-rate")},
		{name: "many-pairs", rule: "legion-v20.42.5", window: manyPairsWindow(t)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rule := replayedRule(t, c.rule)
			var outs [2]string
			var results [2]RunResult
			var errs [2]error
			var runs sync.WaitGroup
			for i := range outs {
				outs[i] = t.TempDir()
				runs.Go(func() {
					results[i], errs[i] = Run(context.Background(), RunConfig{Window: c.window, Rule: rule, Branch: MainBranch, Seed: 1,
						Slowdown: 1, PGDSN: dsn, Out: outs[i], afterFact: rowsInOrder(i == 1)})
				})
			}
			runs.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("window %s, replay %d: %v", c.name, i+1, err)
				}
			}
			var logged []byte
			for _, name := range []string{logFile, scorecardFile, lineScorecardFile, runFile} {
				first, second := runOutput(t, outs[0], name), runOutput(t, outs[1], name)
				if line, a, b, differ := firstDifference(first, second); differ {
					t.Errorf("window %s: the two replays' %s differ first at line %d:\n  %s\n  %s", c.name, name, line, a, b)
				}
				if name == logFile {
					logged = first
				}
			}
			if c.name != "many-pairs" {
				return
			}
			if results[0].Divergence.Retried == 0 {
				t.Error("window many-pairs: the stub gave no row back to wait on its backoff, want each revised design's news-only start to wait behind its task")
			}
			if !drewConflict(replayLines(t, logged)) {
				t.Error("window many-pairs: no stretch drew a conflict that moved its issue back")
			}
		})
	}
}

// runOutput is the output file name of the run in out, run.json without hostFields.
func runOutput(t *testing.T, out, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, name))
	if err != nil {
		t.Fatalf("read the run's %s: %v", name, err)
	}
	if name != runFile {
		return data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("read the run's %s: %v", name, err)
	}
	for _, field := range hostFields {
		if _, ok := fields[field]; !ok {
			t.Fatalf("the run's %s holds no %s", name, field)
		}
		delete(fields, field)
	}
	data, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// firstDifference is the first line, by its number from 1, at which a and b differ, with the two
// lines as a failure prints them; false where they are the same bytes.
func firstDifference(a, b []byte) (int, string, string, bool) {
	if bytes.Equal(a, b) {
		return 0, "", "", false
	}
	x, y := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	clip := func(lines []string, i int) string {
		if i >= len(lines) {
			return "(no line)"
		}
		if line := lines[i]; len(line) > 400 {
			return line[:400] + "…"
		}
		return lines[i]
	}
	for i := range max(len(x), len(y)) {
		if i >= len(x) || i >= len(y) || x[i] != y[i] {
			return i + 1, clip(x, i), clip(y, i), true
		}
	}
	return 0, "", "", false
}

// drewConflict says whether the replay log moves an issue back by a conflict, which a replay
// under a rule whose conflicting head moves back writes only for a drawn conflict.
func drewConflict(log []replayLine) bool {
	return slices.ContainsFunc(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Trigger == workflow.TriggerConflict
	})
}

// manyPairsPairs is how many pairs of issues manyPairsWindow holds.
const manyPairsPairs = 10

// manyPairsWindow is a window from 09:00 to 16:30 recorded under legion-v20.42.5, in a deployment
// that gates the spec, at a cap of 16, of manyPairsPairs pairs of roots, the pair i (from 0)
// beginning 20i minutes after 09:00. Its first, ACME-(10+i), is promoted at once, its spec written
// and approved and its design registered by 15 minutes; it plans to 30, and while it implements,
// to 45, its architect writes a second version of its spec at 35, which a person approves at 40,
// so the news of it waits behind the implementer's task; it works 15 minutes in each phase to
// merging at 105, where a conflict moves it back at 120; its loop is 30 minutes in implementing
// and 15 in each phase after, back in merging at 210, and it merges at 225. Its second,
// ACME-(20+i), is promoted 5 minutes in and works through each phase to merging at 100, where its
// pull request leaves the merge queue at 102 under the number ACME-(10+i)'s key gives (a line
// names it, so the record gives it that number), and it merges at 105, its merge landing a commit
// on main. A root's linger closes two hours after its merge, inside the window or not at all.
// Each first's stretch to its conflict holds the merges of its pair and the two before, and the
// model fitted on the window draws conflicts with some chance at every number of merges.
func manyPairsWindow(t *testing.T) Window {
	t.Helper()
	b := newWindowBuilder(t, clock(9, 0), clock(16, 30))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(16)
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	var merges []Merge
	for i := range manyPairsPairs {
		base := clock(9, 0).Add(time.Duration(i) * 20 * time.Minute)
		at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
		first, second := fmt.Sprintf("ACME-%d", 10+i), fmt.Sprintf("ACME-%d", 20+i)
		designed := func(key, rank string, filed int) {
			promotedRoot(b, at(filed), key, rank)
			b.specWritten(at(filed+5), key)
			b.approves(at(filed+10), key, "")
			b.rootDesigned(at(filed+15), key)
		}
		done := func(key string, merged int) {
			b.leaves(at(merged), key, record.DoneStatus)
			if closes := at(merged + 120); closes.Before(b.window.To) {
				b.lingerCloses(closes, key, 1)
			}
		}

		designed(first, fmt.Sprintf("a%02d", 2*i), 0)
		b.through(first, phase.Planning, builtMove{at(30), phase.Implementing, workflow.TriggerPlannerCompleted})
		b.specRevised(at(35), first, 2)
		b.gateClosed(at(35), first, intake.DispatchArtifactVersion, 2)
		b.approvesVersion(at(40), first, "", 2)
		b.gateOpened(at(40), first, intake.DispatchArtifactApproved, 2)
		b.through(first, phase.Implementing,
			builtMove{at(45), phase.Testing, workflow.TriggerImplementationReady},
			builtMove{at(60), phase.Reviewing, workflow.TriggerTesterPassed},
			builtMove{at(75), phase.Accepting, workflow.TriggerReviewApproved},
			builtMove{at(90), phase.Retro, workflow.TriggerSignOff},
			builtMove{at(105), phase.Merging, workflow.TriggerRetroCompleted},
			builtMove{at(120), phase.Implementing, workflow.TriggerConflict},
			builtMove{at(150), phase.Testing, workflow.TriggerImplementationReady},
			builtMove{at(165), phase.Reviewing, workflow.TriggerTesterPassed},
			builtMove{at(180), phase.Accepting, workflow.TriggerReviewApproved},
			builtMove{at(195), phase.Retro, workflow.TriggerSignOff},
			builtMove{at(210), phase.Merging, workflow.TriggerRetroCompleted},
			builtMove{at(225), phase.Done, workflow.TriggerPullRequestMerged})
		done(first, 225)

		designed(second, fmt.Sprintf("a%02d", 2*i+1), 5)
		b.through(second, phase.Planning,
			builtMove{at(35), phase.Implementing, workflow.TriggerPlannerCompleted},
			builtMove{at(50), phase.Testing, workflow.TriggerImplementationReady},
			builtMove{at(65), phase.Reviewing, workflow.TriggerTesterPassed},
			builtMove{at(80), phase.Accepting, workflow.TriggerReviewApproved},
			builtMove{at(90), phase.Retro, workflow.TriggerSignOff},
			builtMove{at(100), phase.Merging, workflow.TriggerRetroCompleted})
		b.info(at(102), "workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once", "issue", second,
			"pullRequest", 10+i, "head", fmt.Sprintf("%040x", 0xc0de00+i))
		b.through(second, phase.Merging, builtMove{at(105), phase.Done, workflow.TriggerPullRequestMerged})
		done(second, 105)
		merges = append(merges, Merge{SHA: fmt.Sprintf("%040x", 0x5a0000+i), At: at(105), Branch: "main"})
	}
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: merges}
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("build the trace of the window of many pairs: %v", err)
	}
	for i := range manyPairsPairs {
		second := fmt.Sprintf("ACME-%d", 20+i)
		if got := trace.Issues[second].PullRequestNumber; got != 10+i {
			t.Fatalf("the trace numbers %s's pull request %d, want %d, the number of ACME-%d's key", second, got, 10+i, 10+i)
		}
	}
	return window
}
