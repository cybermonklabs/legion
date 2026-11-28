package replay

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// heldMergeWindow is a window from 11:10 to 12:30, its inputs reaching back to 08:40, recorded
// under rule (main or cover), in which a root's merge leaves it in merging, its close waiting for
// the child beneath it. ACME-1, a root, is promoted at 09:00 and works 15 minutes in each phase up
// to its sign-off at 10:30. In its retro its architect files ACME-2 under it, whose first phase
// starts at 10:35 on a slot of its own; ACME-2 then works 15 minutes in each phase from 10:45. At
// 11:00 ACME-1 moves to merging, and at 11:15 its merger's turn ends and its pull request merges:
// the record holds the close's wait on ACME-2 then, and no move. At 12:15 ACME-2's merger's turn
// ends and a person sets ACME-2 done: it leaves the workflow, and the daemon closes ACME-1 at once,
// merging → done by pull_request_merged, and its tree lingers. Under main each issue gives its slot
// back at the end of its review, as main keeps none past it; under cover each keeps its slot,
// ACME-1 to the merge, when ACME-2 takes it over as ACME-1's cover, and ACME-2 to its leave.
//
// The window starts once ACME-2 has started, so the start state places the two as the record left
// them: today's daemon holds a child filed beneath an accepted root at admitted until that root
// closes (record.HeldUnderAccepted), so a replay from before the filing would never start ACME-2.
// The person's done is the one Dispatch event in the window that is not the daemon's own write,
// and it applies only because the replay's copy of Dispatch starts at the sequence the record
// stands at (shadow.seedEvent), where the start state records ACME-2: the daemon's fence drops an
// event at or below that sequence.
func heldMergeWindow(t *testing.T, rule string) Window {
	b := newWindowBuilder(t, clock(11, 10), clock(12, 30))
	b.window.Parts[0].Rule = rule
	b.reaches(150 * time.Minute)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootThroughReview(clock(9, 0), "ACME-1")
	pastReview := b.segmentEnds
	if rule == "cover" {
		pastReview = b.pastReviewKept
	}
	pastReview(clock(10, 15), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	pastReview(clock(10, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.file(clock(10, 35), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.childStarts(clock(10, 35), "ACME-2")
	b.segmentEnds(clock(10, 45), "ACME-2", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	pastReview(clock(11, 0), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.segmentEnds(clock(11, 0), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.turnEnds(clock(11, 15), "ACME-1", claim.RoleMerger)
	b.info(clock(11, 15), mergedCloseWaitsLine, "issue", "ACME-1", "tree", "ACME-1", "pullRequest", 1, "waitingFor", "ACME-2")
	if rule == "cover" {
		b.slotReleased(clock(11, 15), "ACME-1")
	}
	b.segmentEnds(clock(11, 15), "ACME-2", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	pastReview(clock(11, 30), "ACME-2", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	pastReview(clock(11, 45), "ACME-2", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	pastReview(clock(12, 0), "ACME-2", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.turnEnds(clock(12, 15), "ACME-2", claim.RoleMerger)
	b.edit(clock(12, 15), "ACME-2", setStatus(record.DoneStatus))
	b.leaves(clock(12, 15), "ACME-2", record.DoneStatus)
	b.moved(clock(12, 15), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(12, 15), "ACME-1", record.DoneStatus)
	return b.build()
}

// The trace reads a merged issue's close waiting for the issues beneath it as the moment its pull
// request merged: the first `workflow: a merged issue's close waits for the issues beneath it` of
// the visit to merging is the visit's merge, the merger's segment ends there by a held merge, and
// the stretch ends there too, while the visit itself runs on to the close's move to done. The test
// builds the trace of heldMergeWindow under main: ACME-1's visit to merging, from 11:00, holds its
// merge at 11:15, by pull_request_merged, and its move out to done at 12:15; its segment runs from
// 11:00 to 11:15 and ends by held_merge; its one stretch, from its move into testing at 09:45, ends
// at 11:15 with no conflict; and the trace places every line.
func TestTheTraceReadsAHeldMergeAtItsOwnLine(t *testing.T) {
	t.Parallel()
	window := heldMergeWindow(t, record.MainRule.Name)
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("BuildTrace: %v", err)
	}
	visits := trace.Issues["ACME-1"].Visits
	if len(visits) == 0 {
		t.Fatal("the trace holds no visit of ACME-1")
	}
	merging := visits[len(visits)-1]
	switch {
	case merging.Phase != phase.Merging || !merging.Enter.Equal(clock(11, 0)):
		t.Errorf("ACME-1's last visit is %s from %s, want merging from 11:00", merging.Phase, stamp(merging.Enter))
	case merging.Merge == nil || !merging.Merge.At.Equal(clock(11, 15)) || merging.Merge.Trigger != workflow.TriggerPullRequestMerged ||
		merging.Merge.To != phase.Done:
		t.Errorf("ACME-1's visit to merging holds the merge %s, want one at 11:15 by pull_request_merged to done", describeReading(merging.Merge))
	case merging.Out == nil || !merging.Out.At.Equal(clock(12, 15)) || merging.Out.Trigger != workflow.TriggerPullRequestMerged:
		t.Errorf("ACME-1's visit to merging ends by %s, want the close's move at 12:15 by pull_request_merged", describeReading(merging.Out))
	case merging.Segment == nil || !merging.Segment.Start.Equal(clock(11, 0)) || !merging.Segment.End.Equal(clock(11, 15)) ||
		merging.Segment.EndedBy != SegmentEndHeldMerge:
		t.Errorf("ACME-1's merger's segment is %s, want 11:00 to 11:15 ended by %s", describeReading(merging.Segment), SegmentEndHeldMerge)
	}
	var stretches []Stretch
	for _, stretch := range recordedStretches(trace, newBaseMerges(window, trace)) {
		if stretch.Issue == "ACME-1" {
			stretches = append(stretches, stretch)
		}
	}
	if len(stretches) != 1 || !stretches[0].Start.Equal(clock(9, 45)) || !stretches[0].End.Equal(clock(11, 15)) || stretches[0].Conflict {
		t.Errorf("ACME-1's recorded stretches are %s, want one from 09:45 to its merge at 11:15, no conflict", describeReading(stretches))
	}
	if len(trace.Unplaced) != 0 {
		t.Errorf("the trace could not place %s", describeReading(trace.Unplaced))
	}
}

// heldMergeRun replays heldMergeWindow, recorded under rule, under that rule from 11:10, and
// answers its result, its log, and what each settle shows of ACME-1 and ACME-2 (coverState).
func heldMergeRun(t *testing.T, rule string) (RunResult, []replayLine, []coverState) {
	t.Helper()
	dsn := replayDSN(t)
	var states []coverState
	out := t.TempDir()
	cfg := RunConfig{Window: heldMergeWindow(t, rule), Rule: replayedRule(t, rule), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: out}
	cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
		state, err := readCoverState(ctx, st, at, "ACME-1", "ACME-2")
		states = append(states, state)
		return err
	}
	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("replay heldMergeWindow under %s: %v", rule, err)
	}
	return result, replayLog(t, out), states
}

// checkHeldMerge fails t unless the run applied ACME-1's merge at its recorded 11:15, where the
// engine wrote the close's wait, closed ACME-1 by pull_request_merged at ACME-2's leave at 12:15,
// counted one held merge, refused no move and had no Dispatch event dropped by a fence.
func checkHeldMerge(t *testing.T, result RunResult, log []replayLine) {
	t.Helper()
	waited := linesAt(log, func(line replayLine) bool { return line.Msg == mergedCloseWaitsLine && line.Issue == "ACME-1" })
	if want := []time.Time{clock(11, 15)}; !slices.EqualFunc(waited, want, time.Time.Equal) {
		t.Errorf("the engine wrote ACME-1's close waiting at %s, want at its recorded merge, %s", describeTimes(waited), describeTimes(want))
	}
	closed := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.To == string(phase.Done)
	})
	if want := []time.Time{clock(12, 15)}; !slices.EqualFunc(closed, want, time.Time.Equal) {
		t.Errorf("ACME-1 moved to done at %s, want once, at ACME-2's leave, %s", describeTimes(closed), describeTimes(want))
	}
	if done := result.Scorecard.DoneAt["ACME-1"]; !done.Equal(clock(12, 15)) {
		t.Errorf("ACME-1 is done at %s, want 12:15", stamp(done))
	}
	if result.Divergence.HeldMerges != 1 {
		t.Errorf("the replay counts %d held merges, want 1, ACME-1's", result.Divergence.HeldMerges)
	}
	if result.Divergence.Fenced != 0 {
		t.Errorf("a fence of the daemon's dropped %d of the replay's Dispatch events, want none: the person's done of ACME-2 among them",
			result.Divergence.Fenced)
	}
	for _, refused := range result.Divergence.Refused {
		t.Errorf("the replay refused %s's move %s→%s by %s at %s: %s", refused.Issue, refused.From, refused.To, refused.Trigger,
			stamp(refused.At), refused.Reason)
	}
}

// A root whose pull request merges while an issue beneath it is in the workflow stays in merging,
// its close waiting for that issue, and a replay applies the merge at the moment the record holds
// it, not at the close's later move to done. The test replays heldMergeWindow under main: ACME-1's
// merger's segment ends at 11:15, where the replay applies intake.PullRequestMerged and the engine
// writes the close's wait on ACME-2; every settle from then until 12:15 shows ACME-1 in merging;
// at 12:15 the person's done takes ACME-2 out of the workflow, and the engine closes ACME-1 by
// pull_request_merged, the replay entering no move of its own. The run counts one held merge and
// refuses no move.
func TestAHeldMergeIsAppliedAtItsTimeAndClosesAtTheLastLeave(t *testing.T) {
	t.Parallel()
	result, log, states := heldMergeRun(t, record.MainRule.Name)
	checkHeldMerge(t, result, log)
	held := 0
	for _, state := range states {
		switch {
		case state.at.Before(clock(11, 15)):
		case state.at.Before(clock(12, 15)):
			held++
			if state.parentPhase != phase.Merging {
				t.Errorf("at %s ACME-1 stands in %s, want merging, its close waiting for ACME-2", stamp(state.at), state.parentPhase)
			}
		case state.parentPhase != phase.Done:
			t.Errorf("at %s, after ACME-2's leave, ACME-1 stands in %s, want done", stamp(state.at), state.parentPhase)
		}
	}
	if held == 0 {
		t.Errorf("no settle from the merge at 11:15 to ACME-2's leave at 12:15 shows ACME-1: %+v", states)
	}
}

// Under the rule cover a merged root whose close waits for an issue beneath it waits on that issue
// (spec v4 R2; record.LineFacts.BeneathWaits), and that issue, started and holding a slot, holds
// the root's slot for it. The test replays heldMergeWindow, recorded under cover, under cover:
// every settle before the merge at 11:15 shows ACME-1 holding its own slot and no cover; every
// settle from the merge until 12:15 shows ACME-1 in merging holding no slot, covered by ACME-2,
// which holds one; and at 12:15, when the person's done takes ACME-2 out of the workflow, ACME-1 is
// done and no cover is left. The merge and the close come as under main, with one held merge and no
// refused move.
func TestAHeldMergeIsAWaitACoverHolds(t *testing.T) {
	t.Parallel()
	result, log, states := heldMergeRun(t, "cover")
	checkHeldMerge(t, result, log)
	covered := 0
	for _, state := range states {
		switch {
		case state.at.Before(clock(11, 15)):
			if !state.parentHolds || state.parentHolder != "" {
				t.Errorf("at %s, before the merge, ACME-1 holds a slot %t and is covered by %q, want its own slot and no cover", stamp(state.at),
					state.parentHolds, state.parentHolder)
			}
		case state.at.Before(clock(12, 15)):
			covered++
			if state.parentPhase != phase.Merging || state.parentHolds || state.parentHolder != "ACME-2" || !state.childHolds {
				t.Errorf("at %s ACME-1 stands in %s holding a slot %t, covered by %q, and ACME-2 holds a slot %t; want merging, no slot, covered by ACME-2, which holds one",
					stamp(state.at), state.parentPhase, state.parentHolds, state.parentHolder, state.childHolds)
			}
		case state.parentPhase != phase.Done || state.parentHolder != "":
			t.Errorf("at %s, after ACME-2's leave, ACME-1 stands in %s covered by %q, want done with no cover", stamp(state.at), state.parentPhase,
				state.parentHolder)
		}
	}
	if covered == 0 {
		t.Errorf("no settle from the merge at 11:15 to ACME-2's leave at 12:15 shows ACME-1: %+v", states)
	}
}
