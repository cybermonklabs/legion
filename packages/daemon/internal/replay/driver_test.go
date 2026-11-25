package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// replayDSN is the Postgres server a test replays on, or the test's skip where none is set.
func replayDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(testdb.SkipMessage)
	}
	return dsn
}

// mainRule is the rule main, which every recorded fixture window ran.
func mainRule(t *testing.T) record.SlotRule {
	t.Helper()
	return replayedRule(t, record.MainRule.Name)
}

// fixtureWindow reads the fixture window name.
func fixtureWindow(t *testing.T, name string) Window {
	t.Helper()
	window, err := ReadWindow(filepath.Join(fixtureWindowsDir, name))
	if err != nil {
		t.Fatalf("read the window %s: %v", name, err)
	}
	return window
}

// replayLine is one line of a replay's log, by the attributes the tests read of it.
type replayLine struct {
	Time    time.Time            `json:"time"`
	Msg     string               `json:"msg"`
	Issue   string               `json:"issue"`
	Claim   string               `json:"claim"`
	Role    string               `json:"role"`
	From    string               `json:"from"`
	To      string               `json:"to"`
	Trigger workflow.TriggerKind `json:"trigger"`
}

// replayLog is every line of the replay log in out, in order.
func replayLog(t *testing.T, out string) []replayLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, logFile))
	if err != nil {
		t.Fatalf("read the replay's log: %v", err)
	}
	return replayLines(t, data)
}

// replayLines is every line of the replay log data, in order.
func replayLines(t *testing.T, data []byte) []replayLine {
	t.Helper()
	var lines []replayLine
	for i, text := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if text == "" {
			continue
		}
		var line replayLine
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("read line %d of the replay's log: %v", i+1, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// replayedMoves is each issue's sequence of moves in a replay's log, in the order the log holds its
// `workflow: phase changed` lines.
func replayedMoves(log []replayLine) map[string][]fixtureMove {
	moves := make(map[string][]fixtureMove)
	for _, line := range log {
		if line.Msg == phaseChangedLine {
			moves[line.Issue] = append(moves[line.Issue], fixtureMove{From: phase.Phase(line.From), To: phase.Phase(line.To), Trigger: line.Trigger})
		}
	}
	return moves
}

// stopsPhaseRule is main with a wait that stops the phase under way (record.SlotRule.WaitStopsPhase),
// the one choice of #4592's rule the test needs, built here while the repository keeps no rule
// with it.
func stopsPhaseRule() record.SlotRule {
	rule := record.MainRule
	rule.Name, rule.WaitStopsPhase = "main-wait-stops-phase", true
	rule.KeepsSlotIn = slices.Clone(record.MainRule.KeepsSlotIn)
	return rule
}

// linkDuringImplementingWindow is a window in which a wait begins while a phase is under way.
// ACME-1, a root, is promoted at 09:05, plans from 09:20 and implements from 09:50, its one turn
// running to the move to testing at 11:00. ACME-3, an issue Legion is not given, stands in backlog
// from 09:06. At 10:00 a person links ACME-1 as blocked by ACME-3, and admission gives ACME-1's slot
// back for the wait; at 10:30 the person closes ACME-3, and ACME-1 takes its slot again. Under
// main the implementer works on through the wait, so the move comes at 11:00 as recorded.
func linkDuringImplementingWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 6), "ACME-3", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a3"})
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.edit(clock(10, 0), "ACME-1", func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-3"} })
	b.slotReleased(clock(10, 0), "ACME-1")
	b.edit(clock(10, 30), "ACME-3", setStatus(record.DoneStatus))
	b.waitEnded(clock(10, 30), "ACME-1", phase.Implementing)
	b.segmentEnds(clock(11, 0), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// A rule whose wait stops the phase under way stops it in a replay (spec R2): the daemon's own code
// suspends the worker of the phase when the pass gives the slot back for the wait
// (workflow.Engine.StopForWait), and the replay's worker stub pauses the segment that worker works,
// with the rest of its length, ends its turn, and resumes it when the issue takes its slot again
// and the pass sends its worker the entry again. The test replays linkDuringImplementingWindow
// under main and under main with a wait that stops the phase. Under main the implementer is never
// suspended and ACME-1 moves to testing at 11:00, as recorded; under the other rule its implementer
// is suspended at 10:00 and starts no turn until the wait ends at 10:30, and the move comes at
// 11:30, half an hour of the segment's length done after the wait.
func TestARuleThatStopsThePhaseStopsIt(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := linkDuringImplementingWindow(t)
	implementer := string(builtToken(t, "ACME-1", claim.RoleImplementer))
	for _, c := range []struct {
		rule      record.SlotRule
		moved     time.Time
		suspended bool
	}{
		{rule: mainRule(t), moved: clock(11, 0)},
		{rule: stopsPhaseRule(), moved: clock(11, 30), suspended: true},
	} {
		t.Run(c.rule.Name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			if _, err := Run(context.Background(), RunConfig{Window: window, Rule: c.rule, Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: out}); err != nil {
				t.Fatalf("replay the window under %s: %v", c.rule.Name, err)
			}
			var moved []time.Time
			var suspendedAt []time.Time
			var turnsInWait []time.Time
			for _, line := range replayLog(t, out) {
				switch {
				case line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.From == string(phase.Implementing):
					moved = append(moved, line.Time)
				case line.Msg == "supervise: suspended" && line.Claim == implementer:
					suspendedAt = append(suspendedAt, line.Time)
				case line.Msg == "supervise: a turn started" && line.Claim == implementer && !line.Time.Before(clock(10, 0)) &&
					line.Time.Before(clock(10, 30)):
					turnsInWait = append(turnsInWait, line.Time)
				}
			}
			if len(moved) != 1 || !moved[0].Equal(c.moved) {
				t.Errorf("ACME-1 moved out of implementing at %v, want once at %s", moved, c.moved.Format(time.RFC3339))
			}
			switch {
			case c.suspended && (len(suspendedAt) != 1 || !suspendedAt[0].Equal(clock(10, 0))):
				t.Errorf("ACME-1's implementer was suspended at %v, want once at 10:00, when its slot went back for the wait", suspendedAt)
			case !c.suspended && len(suspendedAt) > 0:
				t.Errorf("ACME-1's implementer was suspended at %v, and under %s a wait stops no phase", suspendedAt, c.rule.Name)
			}
			if len(turnsInWait) > 0 {
				t.Errorf("ACME-1's implementer started turns at %v, inside the wait from 10:00 to 10:30", turnsInWait)
			}
		})
	}
}

// recordedStarts is each issue's recorded starts of its pieces of work from the window's start on,
// the moments a replay under the recorded rule must start them: each visit's work segment, cut at
// each hold of its role's claim, as the replay's script reads them.
func recordedStarts(t *testing.T, window Window) map[string][]time.Time {
	t.Helper()
	in, err := newWindowInputs(window)
	if err != nil {
		t.Fatalf("build the window's trace: %v", err)
	}
	s, err := buildScript(in.trace, in.events, in.lines)
	if err != nil {
		t.Fatalf("read the window's script: %v", err)
	}
	starts := map[string][]time.Time{}
	for key, pieces := range s.pieces {
		for _, p := range pieces {
			if !p.start.Before(window.From) {
				starts[key] = append(starts[key], p.start)
			}
		}
	}
	return starts
}

// checkRecordedStarts fails for each issue of window whose work segments in result, a replay of it
// under the rule it recorded with every segment traced, start otherwise than the recorded starts of
// its pieces of work (recordedStarts).
func checkRecordedStarts(t *testing.T, window Window, result RunResult) {
	t.Helper()
	replayed := map[string][]time.Time{}
	for _, segment := range result.Segments {
		replayed[segment.Issue] = append(replayed[segment.Issue], segment.Start)
	}
	recorded := recordedStarts(t, window)
	for _, issue := range slices.Sorted(maps.Keys(unionKeys(replayed, recorded))) {
		got, want := replayed[issue], recorded[issue]
		slices.SortFunc(got, time.Time.Compare)
		slices.SortFunc(want, time.Time.Compare)
		if !slices.EqualFunc(got, want, time.Time.Equal) {
			t.Errorf("%s's segments start at %s in the replay, and its work starts at %s in the record", issue, describeTimes(got), describeTimes(want))
		}
	}
}

// Every hold applies in a replay at the times the window records it (spec R2, assumption 7), so
// under the recorded rule every piece of every issue's work starts where the record started it. The
// test replays the fixture gate-holds-child-first-phase, whose design gate holds a child's first
// phase until a person's approval, under main with every segment traced, and fails for each issue
// whose segment starts differ from the recorded starts of its pieces of work (checkRecordedStarts).
// The windows of four more holds (holds_test.go) are held to the same check on
// TestEveryHoldHasASourceInTheReplay's replays of them (holdCase.pieces): an ancestor's live tree
// holds an orphan, an accepted ancestor holds a child filed under it, a closed tree's linger holds
// its member until it hands it over, and a held phase holds its worker until the architect retries
// it.
func TestHoldsApplyAtTheirRecordedTimes(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	const name = "gate-holds-child-first-phase"
	window := fixtureWindow(t, name)
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: t.TempDir(), TraceSegments: true})
	if err != nil {
		t.Fatalf("replay the window %s under main: %v", name, err)
	}
	checkRecordedStarts(t, window, result)
}

// describeTimes is times as a failure prints them.
func describeTimes(times []time.Time) string {
	parts := make([]string, len(times))
	for i, at := range times {
		parts[i] = at.UTC().Format("15:04:05")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// recordedMoves is each issue's recorded moves from the window's start on, as the trace reads them:
// the move that ended each visit, in order, the last one's move to done among them.
func recordedMoves(t *testing.T, window Window) map[string][]fixtureMove {
	t.Helper()
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("build the window's trace: %v", err)
	}
	moves := map[string][]fixtureMove{}
	for key, iss := range trace.Issues {
		for _, visit := range iss.Visits {
			if out := visit.Out; out != nil && !out.At.Before(window.From) {
				moves[key] = append(moves[key], fixtureMove{From: out.RecordedFrom, To: out.RecordedTo, Trigger: out.Trigger})
			}
		}
	}
	return moves
}

// A replay under the rule a window recorded makes the moves the record made (design 10.11, the
// review's first early test), and asks nothing outside its own database (spec R1): no Dispatch, no
// GitHub and no NATS. The fixture every-trigger takes a root and two children through every trigger
// — a failed test, a review asking for changes, an architect's send-back from accepting, a sign-off
// held behind a child, the merges and the linger's expiry — so its run starts and stops workers,
// writes statuses, signs off, merges and closes a linger. The test points every address a daemon's
// clients read — DISPATCH_URL, the GitHub API's and the proxies every HTTP client of Go asks first —
// at a stub that records each request it is sent, and replays the window under main. It fails for
// each request the stub saw; for each issue whose sequence of (from, to, trigger) in the replay's
// own log differs from the trace's; for a move the replay refused; and for a start that re-sent an
// entry its claim had been given already.
func TestTheEveryTriggerWindowReplaysItsRecordedMovesAndAsksNothingOutside(t *testing.T) {
	// Serial: the test points the process's environment at its stub (t.Setenv), which every other
	// test of the process reads, so it runs before the parallel tests start.
	dsn := replayDSN(t)
	var mu sync.Mutex
	var seen []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.String())
		mu.Unlock()
		http.Error(w, "the replay asked something outside", http.StatusTeapot)
	}))
	defer stub.Close()
	for _, name := range []string{"DISPATCH_URL", "GITHUB_API_URL", "GH_HOST", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, stub.URL)
	}
	t.Setenv("NO_PROXY", "")
	window := fixtureWindow(t, "every-trigger")
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: out})
	if err != nil {
		t.Fatalf("replay the window every-trigger: %v", err)
	}
	mu.Lock()
	if len(seen) > 0 {
		t.Errorf("the replay sent %d requests outside its database: %s", len(seen), strings.Join(seen, "; "))
	}
	mu.Unlock()
	moved := replayedMoves(replayLog(t, out))
	recorded := recordedMoves(t, window)
	for _, issue := range slices.Sorted(maps.Keys(unionKeys(moved, recorded))) {
		if got, want := moved[issue], recorded[issue]; !slices.Equal(got, want) {
			t.Errorf("%s moved %s in the replay, and the record moved it %s", issue, describeMoves(got), describeMoves(want))
		}
	}
	for _, refused := range result.Divergence.Refused {
		t.Errorf("the replay refused %s's move %s→%s by %s at %s: %s", refused.Issue, refused.From, refused.To, refused.Trigger,
			refused.At.Format(time.RFC3339), refused.Reason)
	}
	if result.Divergence.Reassertions != 0 {
		t.Errorf("the replay made %d reassertion starts, want 0: every start under the recorded rule hands its role new work", result.Divergence.Reassertions)
	}
}

// mirrorRow is an issue's Dispatch values as the daemon's copy of Dispatch or the replay's shadow
// holds them.
type mirrorRow struct {
	Status   string
	Blockers []string
	Parent   string
	Labels   []string
	Rank     string
}

// The replay's copy of Dispatch (its shadow) is what the daemon's own copy (dispatch_issues) is
// fed from, every event the shadow makes taking its issue's next sequence and carrying its whole
// payload, so after every fact the two agree and no fence drops an event (design 10.4, the review's
// second early test). The test replays every fixture window under main with a look after each fact,
// and fails for an issue of the project whose dispatch_issues row differs from the shadow in its
// status, blockers, parent, labels or rank, for a row the shadow holds no issue of, and for an
// issue whose record's Dispatch sequence (record.Issue.LastDispatchSeq) falls.
func TestTheShadowMatchesTheMirrorAfterEveryFact(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	for _, name := range fixtureWindowNames(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			window := fixtureWindow(t, name)
			records := record.NewStore()
			highest := map[string]int64{}
			facts := 0
			var failures []string
			look := func(ctx context.Context, pool *store.Pool, s *shadow) error {
				facts++
				if len(failures) >= 10 {
					return nil
				}
				var rows []record.DispatchIssueRow
				var issues []record.Issue
				if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
					var err error
					if rows, err = records.DispatchIssues(ctx, tx, window.Project); err != nil {
						return err
					}
					issues, err = records.Issues(ctx, tx)
					return err
				}); err != nil {
					return err
				}
				for _, row := range rows {
					iss, held := s.issues[row.Key]
					if !held || iss.payload == nil {
						failures = append(failures, fmt.Sprintf("after fact %d, dispatch_issues holds %s, which the shadow holds no payload of", facts, row.Key))
						continue
					}
					mirrored := mirrorRow{Status: row.Status, Blockers: slices.Sorted(slices.Values(row.Blockers)), Parent: row.Parent,
						Labels: slices.Sorted(slices.Values(row.Labels)), Rank: row.Rank}
					shadowed := mirrorRow{Status: iss.status(), Blockers: slices.Sorted(slices.Values(iss.blockers())), Parent: iss.parent(),
						Labels: slices.Sorted(slices.Values(iss.labels())), Rank: s.rank(iss)}
					if fmt.Sprint(mirrored) != fmt.Sprint(shadowed) {
						failures = append(failures, fmt.Sprintf("after fact %d, %s's dispatch_issues row is %+v and the shadow shows %+v", facts, row.Key, mirrored, shadowed))
					}
				}
				for _, issue := range issues {
					if issue.LastDispatchSeq < highest[issue.Key] {
						failures = append(failures, fmt.Sprintf("after fact %d, %s's record stands at Dispatch sequence %d, below the %d it reached", facts,
							issue.Key, issue.LastDispatchSeq, highest[issue.Key]))
					}
					highest[issue.Key] = max(highest[issue.Key], issue.LastDispatchSeq)
				}
				return nil
			}
			out := t.TempDir()
			result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: out, afterFact: look})
			if err != nil {
				t.Fatalf("replay the window %s under main: %v", name, err)
			}
			if facts == 0 {
				t.Fatalf("the replay of %s applied no fact, so nothing was compared", name)
			}
			for _, failure := range failures {
				t.Error(failure)
			}
			logRun(t, name, out)
			if result.Divergence.Fenced != 0 {
				t.Errorf("a fence of the daemon's dropped %d of the replay's Dispatch events", result.Divergence.Fenced)
			}
		})
	}
}

// logRun logs what the run in out counted (its run.json): its divergence counters, the facts it
// applied and its wall time, one line for the window name.
func logRun(t *testing.T, name, out string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, runFile))
	if err != nil {
		t.Errorf("read the run's %s: %v", runFile, err)
		return
	}
	var ran runRecord
	if err := json.Unmarshal(data, &ran); err != nil {
		t.Errorf("read the run's %s: %v", runFile, err)
		return
	}
	counted, _ := json.Marshal(ran.Divergence)
	t.Logf("window %s: %d facts, %d passes, mean pass %.2f ms, wall %.2f s; divergence %s", name, ran.Facts, ran.Passes,
		ran.MeanPassMillis, ran.WallSeconds, counted)
}

// startState is what the test of a warm start reads of one issue at a moment: its phase, whether
// it holds a slot, the part of the waiting line it stands in ("" for none), whether its tree's
// design gate stands open, whether it waits on a person, and the phase it is held from ("" for
// none).
type startState struct {
	Phase    phase.Phase
	Slot     bool
	Part     string
	GateOpen bool
	Awaiting bool
	Held     phase.Phase
}

// readStartState reads each issue of project in the database url names, as the daemon's own pass
// would read it at at: the record, the slots, the line the pass judges (blockers.JudgeLine under
// rule and steps) and the tree gates.
func readStartState(t *testing.T, url, project string, steps []record.DesignStep, rule record.SlotRule, at time.Time) map[string]startState {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to %s: %v", url, err)
	}
	defer conn.Close(ctx)
	records := record.NewStore()
	states := map[string]startState{}
	if err := pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		issues, err := records.LiveIssues(ctx, tx, project)
		if err != nil {
			return err
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		slotted := map[string]bool{}
		for _, slot := range slots {
			slotted[slot.Issue] = true
		}
		facts, err := record.ReadLineFactsUnder(ctx, tx, records, project, issues, steps, rule)
		if err != nil {
			return err
		}
		_, _, line, _, err := blockers.JudgeLine(ctx, tx, records, project, issues, slots, facts, nil, at)
		if err != nil {
			return err
		}
		parts := map[string]string{}
		for _, entry := range line {
			parts[entry.Issue.Key] = fmt.Sprint(entry.Part)
		}
		gates, err := record.ReadTreeGates(ctx, tx, records, issues, steps)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			state := startState{Phase: issue.Phase, Slot: slotted[issue.Key], Part: parts[issue.Key], Awaiting: issue.Awaiting != nil}
			if gate, ok := gates[issue.Tree]; ok {
				state.GateOpen = gate.Open()
			}
			if issue.Hold != nil {
				state.Held = issue.Hold.From
			}
			states[issue.Key] = state
		}
		return nil
	}); err != nil {
		t.Fatalf("read the state of %s: %v", url, err)
	}
	return states
}

// A replay that runs through a moment leaves the state its start state would have built from the
// record's lines at that moment (assumption 6, the review's third early test): the fixture
// every-trigger is replayed from its start to 12:25, with ACME-1 accepting without a slot, ACME-3
// implementing again on its slot after its review asked for changes, and ACME-2 done, and its
// database kept; the start state (Seed) is built at 12:25 from the window's trace in a database of
// its own. The test fails for each issue whose phase, slot, part of the line, gate, wait on a
// person or hold differ between the two.
func TestAWarmStartMatchesTheSeed(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	ctx := context.Background()
	window := fixtureWindow(t, "every-trigger")
	at := clock(12, 25)
	rule := mainRule(t)

	warm := window
	warm.To = at
	result, err := Run(ctx, RunConfig{Window: warm, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn, Out: t.TempDir(),
		KeepDB: true})
	if err != nil {
		t.Fatalf("replay every-trigger to %s: %v", at.Format(time.RFC3339), err)
	}
	dropKeptDatabase(t, dsn, result.Database)
	warmURL, err := testdb.DatabaseURL(dsn, result.Database)
	if err != nil {
		t.Fatal(err)
	}

	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("build every-trigger's trace: %v", err)
	}
	cold, st := replayStore(t, dsn)
	simulated := newSimClock(at)
	st.UseClock(simulated.Now)
	err = store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStoreWithClock(simulated.Now)
		engine := workflow.New(records, workflow.Config{Project: trace.Project, Clock: simulated.Now}, nil)
		unplaced, err := Seed(ctx, tx, records, st, engine, withRanks(trace, firstStartRanks(trace).ranks()), at)
		if err == nil && len(unplaced) > 0 {
			err = fmt.Errorf("the start state could not place %v", unplaced)
		}
		return err
	})
	st.Close()
	if err != nil {
		t.Fatalf("build the start state at %s: %v", at.Format(time.RFC3339), err)
	}

	replayed := readStartState(t, warmURL, window.Project, window.GateSteps, rule, at)
	seeded := readStartState(t, cold.url, window.Project, window.GateSteps, rule, at)
	if len(seeded) == 0 {
		t.Fatalf("the start state at %s holds no issue in the workflow, so nothing was compared", at.Format(time.RFC3339))
	}
	for _, issue := range slices.Sorted(maps.Keys(unionKeys(replayed, seeded))) {
		if got, want := replayed[issue], seeded[issue]; got != want {
			t.Errorf("at %s %s stands %+v in the replay and %+v in the start state", at.Format(time.RFC3339), issue, got, want)
		}
	}
}
