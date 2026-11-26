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

// mechanismNoChildrenWait is the kept rule named base with no wait that stops the phase under way
// (record.SlotRule.WaitStopsPhase off), under the name mechanism-no-children-wait, a rule no part
// of the tool offers (record.SlotRules) and no fixture's result names. A test of a mechanism of the
// replay, such as an event that moves with an issue's start or a hold that keeps a tree's rank,
// replays under it a window whose fact is that mechanism: since cybermonklabs/legion#4592 the
// daemon's own rule moves a started parent with an open child into awaiting_children, which would
// free the parent's slot and change the very starts those tests hold, so they hold the mechanism on
// the course their windows were written for, and the path of the wait itself is held by tests of
// its own (TestARuleThatStopsThePhaseStopsIt, TestAWaitingParentsEventsMoveWithItsReturn).
func mechanismNoChildrenWait(t *testing.T, base string) record.SlotRule {
	t.Helper()
	rule := replayedRule(t, base)
	rule.Name, rule.WaitStopsPhase = "mechanism-no-children-wait", false
	rule.KeepsSlotIn = slices.Clone(rule.KeepsSlotIn)
	return rule
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

// childDuringImplementingWindow is a window in which a child is filed under a parent while the
// parent's phase is under way. ACME-1, a root, is promoted at 09:05, plans from 09:20 and
// implements from 09:50, its one turn running to the move to testing at 11:00. At 10:00 its
// architect files ACME-2 under it in the backlog, a child it never releases, which is open
// (record.ChildOpen); at 10:30 a person moves ACME-2 to icebox, which closes it as a child.
func childDuringImplementingWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.filedBy(clock(10, 0), "ACME-2", filing{Title: "Name the export's columns", Parent: "ACME-1", Status: "backlog", Rank: "a1"},
		b.session("ACME-1", claim.RoleArchitect))
	b.edit(clock(10, 30), "ACME-2", setStatus("icebox"))
	b.segmentEnds(clock(11, 0), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// linkDuringImplementingWindow is a window in which a wait begins while a phase is under way.
// ACME-1, a root, is promoted at 09:05, plans from 09:20 and implements from 09:50, its one turn
// running to the move to testing at 11:00. ACME-3, an issue Legion is not given, stands in backlog
// from 09:06. At 10:00 a person links ACME-1 as blocked by ACME-3, and admission gives ACME-1's slot
// back for the wait; at 10:30 the person closes ACME-3, and ACME-1 takes its slot again. The
// implementer works on through the wait under every rule, since a blocked_by link stops no phase,
// so the move comes at 11:00 as recorded.
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

// A wait that stops the phase under way is cybermonklabs/legion#4592's, and a replay plays it
// through the daemon's own engine (spec R2): under main the admission pass that reads ACME-2 filed
// under ACME-1 moves ACME-1 into awaiting_children at 10:00, retiring its implementer's task, and
// the replay's worker stub pauses the segment that worker works, with the rest of its length, and
// ends its turn; the pass that reads ACME-2 in icebox at 10:30 returns ACME-1 to implementing,
// whose start resumes the segment, so the move to testing comes at 11:30, half an hour of the
// segment's length done after the wait. Under legion-v22.14.1, the rule of a release before #4592,
// the child stops nothing: ACME-1 never enters awaiting_children and moves to testing at 11:00, as
// recorded. Under neither does the implementer start a turn inside the wait.
func TestARuleThatStopsThePhaseStopsIt(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := childDuringImplementingWindow(t)
	implementer := string(builtToken(t, "ACME-1", claim.RoleImplementer))
	recorded, kept := record.SlotRuleNamed("legion-v22.14.1")
	if !kept {
		t.Fatal("the repository keeps no rule legion-v22.14.1")
	}
	for _, c := range []struct {
		rule  record.SlotRule
		moves []string
	}{
		{rule: mainRule(t), moves: []string{"implementing -> awaiting_children at 10:00", "awaiting_children -> implementing at 10:30",
			"implementing -> testing at 11:30"}},
		{rule: recorded, moves: []string{"implementing -> testing at 11:00"}},
	} {
		t.Run(c.rule.Name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			if _, err := Run(context.Background(), RunConfig{Window: window, Rule: c.rule, Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: out}); err != nil {
				t.Fatalf("replay the window under %s: %v", c.rule.Name, err)
			}
			var moves []string
			var turnsInWait []time.Time
			for _, line := range replayLog(t, out) {
				switch {
				case line.Msg == "supervise: a turn started" && line.Claim == implementer && !line.Time.Before(clock(10, 0)) &&
					line.Time.Before(clock(10, 30)):
					turnsInWait = append(turnsInWait, line.Time)
				case line.Issue != "ACME-1" || line.Time.Before(clock(10, 0)):
				case line.Msg == phaseChangedLine:
					moves = append(moves, line.From+" -> "+line.To+" at "+line.Time.Format("15:04"))
				case line.Msg == workflow.ParentWaitsLine:
					moves = append(moves, line.From+" -> "+string(phase.AwaitingChildren)+" at "+line.Time.Format("15:04"))
				case line.Msg == workflow.ParentReturnsLine:
					moves = append(moves, string(phase.AwaitingChildren)+" -> "+line.To+" at "+line.Time.Format("15:04"))
				}
			}
			if !slices.Equal(moves, c.moves) {
				t.Errorf("ACME-1's moves from 10:00 on under %s = %v, want %v", c.rule.Name, moves, c.moves)
			}
			if len(turnsInWait) > 0 {
				t.Errorf("ACME-1's implementer started turns at %v, inside the wait from 10:00 to 10:30", turnsInWait)
			}
		})
	}
}

// childDoneDuringPlanningWindow is a window from 09:00 to 13:00 under a cap of two in which a
// parent's architect files a child under it while its planner plans, and the child is done while
// the parent still plans. ACME-1, a root, is promoted at 09:05 and plans from 09:20 to 09:50. At
// 09:30 its architect files ACME-2 under it, todo, which starts at once on the second slot and
// works 10 minutes in each phase to its merge at 10:40, giving its slot back at accepting. At 09:40
// ACME-1's architect retitles ACME-1, an act that no piece of the architect's work places, so it
// is placed from ACME-1's start. ACME-1 implements from 09:50 to 10:30 and moves to testing.
func childDoneDuringPlanningWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.capped(2)
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a0")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.filedBy(clock(9, 30), "ACME-2", filing{Title: "Name the export's columns", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"},
		b.session("ACME-1", claim.RoleArchitect))
	b.childStarts(clock(9, 30), "ACME-2")
	b.through("ACME-2", phase.Planning, moveAt(9, 40, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(9, 50, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 0, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 10, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 30, phase.Merging, workflow.TriggerRetroCompleted))
	b.turnEnds(clock(10, 40), "ACME-2", claim.RoleMerger)
	b.moved(clock(10, 40), "ACME-2", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(10, 40), "ACME-2", record.DoneStatus)
	retitled(b, clock(9, 40), "ACME-1", architectOf("ACME-1"))
	b.through("ACME-1", phase.Planning, moveAt(9, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 30, phase.Testing, workflow.TriggerImplementationReady))
	return b.build()
}

// A parent that waits in awaiting_children goes on after its children are done, and what it does
// after the wait began moves with its next start (spec R2: a fact that depends on a start moves
// with that start), while it holds no slot through the wait. Under main (cybermonklabs/legion#4592)
// the pass that reads ACME-2 filed at 09:30 moves ACME-1 into awaiting_children, its planner
// stopped with 20 of its 30 minutes left, and its slot goes back; ACME-2 starts on a slot and is
// done at 10:40, where ACME-1 comes back to planning and takes a slot again. Its planner works the
// rest of its segment to 11:00 and its implementer the 40 minutes of its own to 11:40. Its
// architect's retitle, recorded 10 minutes into the wait, enters 10 minutes after the return, at
// 10:50. Under legion-v22.14.1, the rule of a release before #4592, the child stops nothing, and
// each of them comes at its recorded time. The test fails where the parent's later moves or its
// architect's act keep their recorded times under main, or where ACME-1 holds a slot while it
// waits.
func TestAParentThatWaitsForItsChildrenGoesOnAtItsReturn(t *testing.T) {
	t.Parallel()
	recorded, kept := record.SlotRuleNamed("legion-v22.14.1")
	if !kept {
		t.Fatal("the repository keeps no rule legion-v22.14.1")
	}
	for _, c := range []struct {
		rule     record.SlotRule
		moves    []string
		slots    []string
		retitled time.Time
	}{
		{rule: mainRule(t), moves: []string{"planning -> awaiting_children at 09:30", "awaiting_children -> planning at 10:40",
			"planning -> implementing at 11:00", "implementing -> testing at 11:40"},
			slots: []string{"admission: slot released at 09:30", "admission: slot taken at 10:40"}, retitled: clock(10, 50)},
		{rule: recorded, moves: []string{"planning -> implementing at 09:50", "implementing -> testing at 10:30"}, retitled: clock(9, 40)},
	} {
		t.Run(c.rule.Name, func(t *testing.T) {
			t.Parallel()
			log, copied := replayCopiedUnder(t, childDoneDuringPlanningWindow(t), c.rule)
			var moves, slots []string
			for _, line := range log {
				if line.Issue != "ACME-1" || line.Time.Before(clock(9, 30)) {
					continue
				}
				at := line.Time.Format("15:04")
				switch line.Msg {
				case phaseChangedLine:
					moves = append(moves, line.From+" -> "+line.To+" at "+at)
				case workflow.ParentWaitsLine:
					moves = append(moves, line.From+" -> "+string(phase.AwaitingChildren)+" at "+at)
				case workflow.ParentReturnsLine:
					moves = append(moves, string(phase.AwaitingChildren)+" -> "+line.To+" at "+at)
				case "admission: slot released", "admission: slot taken":
					slots = append(slots, line.Msg+" at "+at)
				}
			}
			if !slices.Equal(moves, c.moves) {
				t.Errorf("ACME-1's moves from 09:30 on under %s = %v, want %v", c.rule.Name, moves, c.moves)
			}
			if !slices.Equal(slots, c.slots) {
				t.Errorf("ACME-1's slot lines from 09:30 on under %s = %v, want %v, no slot held while it waits", c.rule.Name, slots, c.slots)
			}
			retitles := appliedBy(t, copied, "ACME-1", "issue.updated", architectOf("ACME-1"))
			if want := []time.Time{c.retitled}; !slices.EqualFunc(retitles, want, time.Time.Equal) {
				t.Errorf("the architect's retitle of ACME-1 entered at %s under %s, want at %s", describeTimes(retitles), c.rule.Name,
					describeTimes(want))
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
	rule := mechanismNoChildrenWait(t, record.MainRule.Name)

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
