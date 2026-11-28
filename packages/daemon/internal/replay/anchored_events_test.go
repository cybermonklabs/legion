package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// passedLoopWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00, in which a root's recorded conflict loop holds acts
// of two of its workers. ACME-2, a root promoted at 07:10, moves to testing at 08:40, and a
// conflict moves it back to implementing at 09:20, its stretch holding no commit of the base
// branch. In that loop its architect, whose session Dispatch names under the title `Legion
// architect · ACME-2`, files ACME-4 under it in triage at 09:35, and its implementer posts a
// comment at 09:40. ACME-2 is back in testing at 09:50 and its tester passes it at 10:20. At 10:30
// the architect releases ACME-4 (the daemon's session writes its todo), and ACME-4 takes a slot and
// starts. ACME-3, a root promoted at 09:00, works 15 minutes in each phase, and a conflict moves it
// back from reviewing at 10:45, its stretch holding the base branch's one commit, at 10:40: its two
// stretches are the window's only ones in the fit, and they separate, so the chance fitted at M =
// 0 is 0, and ACME-2's stretch draws no conflict under any seed.
func passedLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.capped(3)
	b.boot(clock(7, 0))
	rootThrough(b, "ACME-2", "a2", clock(7, 10), clock(7, 25), moveAt(7, 40, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 40, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 20, phase.Implementing, workflow.TriggerConflict))
	architect := recordedSession("ACME-2", claim.RoleArchitect, "00000000-0000-4000-8000-0000000000c2")
	b.filedBy(clock(9, 35), "ACME-4", filing{Title: "Ship the ACME-4 change", Parent: "ACME-2", Tree: "ACME-2", Status: "triage", Rank: "a4"},
		architect)
	b.commented(clock(9, 40), "ACME-2", recordedSession("ACME-2", claim.RoleImplementer, "00000000-0000-4000-8000-0000000000c3"))
	b.segmentEnds(clock(9, 50), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 20), "ACME-2", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.daemonWrites(clock(10, 30), "ACME-4", "todo")
	b.childStarts(clock(10, 30), "ACME-4")
	reviewConflictLoop(b, "ACME-3", "a3")
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: "7c4d2e9f1a3b5067b1c2d3e4f5a6b7c8d9e0f1a2", At: clock(10, 40), Branch: "main"},
	}}
	return window
}

// commented is a worker's session posting a comment on key at at that answers no ask
// (comment.created), which the daemon reads as no fact.
func (b *windowBuilder) commented(at time.Time, key string, worker eventActor) {
	b.t.Helper()
	b.event(at, key, "comment.created", worker, map[string]any{
		"id": fmt.Sprintf("00000000-0000-4000-8000-%012x", 0xd00+len(b.events)), "body": "The base branch is merged in again.",
		"author": worker, "ask_id": nil,
	})
}

// eventsApplied is each moment the replay applied an event of kind on key to its copy of Dispatch
// (shadow), the copy the run handed its atStart.
func eventsApplied(t *testing.T, copied *shadow, key, kind string) []time.Time {
	t.Helper()
	if copied == nil {
		t.Fatal("the run never handed the test its copy of Dispatch at the window's start")
	}
	iss := copied.issues[key]
	if iss == nil {
		return nil
	}
	var applied []time.Time
	for i, raw := range iss.log[iss.seeded:] {
		var event struct {
			Type      string    `json:"type"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("read event %d of %s in the copy of Dispatch: %v", i+1, key, err)
		}
		if event.Type == kind {
			applied = append(applied, event.CreatedAt)
		}
	}
	return applied
}

// An event enters a replay where no work segment of the replay's own runs it, and enters once: an
// architect's works between its root's phases, where the replay runs no work segment of its own,
// so an event its session writes there is no act of a segment and enters as any other event enters
// (placeEvents); and a worker's event inside a work segment of its role is an act of that segment,
// which enters where the replay runs it, and where the replay passes over the segment, enters as
// any other event enters, once (placeUnrun). The test replays passedLoopWindow under
// legion-v20.42.5, where ACME-2's stretch draws no conflict and goes on past its recorded loop: the
// replay passes over the loop's segment of the implementer at 09:20. The architect's filing of
// ACME-4 at 09:35, in that loop, enters at 09:35, and the release at 10:30 starts ACME-4 then; and
// the implementer's comment at 09:40, in the segment, enters the replay's copy of Dispatch once, at
// 09:40. Placed in the implementer's segment, the filing never enters: the replay never creates
// ACME-4, and the release starts nothing; and left with the segment, the comment never enters.
func TestTheEventsOfAPassedOverLoopAreAppliedOnce(t *testing.T) {
	t.Parallel()
	var copied *shadow
	out := t.TempDir()
	cfg := RunConfig{Window: passedLoopWindow(t), Rule: replayedRule(t, "legion-v20.42.5"), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay passedLoopWindow under legion-v20.42.5: %v", err)
	}
	started := linesAt(replayLog(t, out), func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-4" && line.From == string(phase.Admitted) && line.To == string(phase.Planning)
	})
	if want := []time.Time{clock(10, 30)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-4 moved into planning at %s, want once at %s: its architect files it at 09:35 and releases it at 10:30",
			describeTimes(started), describeTimes(want))
	}
	if applied, want := eventsApplied(t, copied, "ACME-2", "comment.created"), []time.Time{clock(9, 40)}; !slices.EqualFunc(applied, want, time.Time.Equal) {
		t.Errorf("the copy of Dispatch holds the implementer's comment on ACME-2 applied at %s, want once, at %s",
			describeTimes(applied), describeTimes(want))
	}
}

// givenLoopWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, whose two
// recorded conflict loops left testing. ACME-2, a root promoted at 09:00, works 15 minutes in each
// phase: its tester fails it at 10:00, a conflict moves it back from reviewing at 10:45, its loop
// is 15 minutes in implementing and 15 in testing, and it is back in reviewing at 11:15 to the
// window's end. ACME-4, a root promoted at 09:30, works 15 minutes in each phase to testing at
// 10:15, a conflict moves it back at 10:30, its loop is 10 minutes in implementing, and it is back
// in testing at 10:40 to the window's end. ACME-3, a root promoted at 11:00, works 15 minutes in
// each phase to testing at 11:45, a conflict moves it back at 12:00, and its loop is 25 minutes in
// implementing, in which its implementer posts a comment at 12:20; it is back in testing at 12:25
// to the window's end. The base branch took a commit inside each stretch that ends in a conflict,
// at 10:25, 10:40 and 11:50, and none inside ACME-2's stretch that its tester ended: the stretches
// separate, so the chance fitted at M = 0 is 0 and at M = 1 is 1, and each stretch that ended in a
// conflict draws one under any seed.
func givenLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(4)
	b.boot(clock(9, 0))
	reviewConflictLoop(b, "ACME-2", "a2")
	rootThrough(b, "ACME-4", "a4", clock(9, 30), clock(9, 45), moveAt(10, 0, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 15, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 30, phase.Implementing, workflow.TriggerConflict),
		moveAt(10, 40, phase.Testing, workflow.TriggerImplementationReady))
	rootThrough(b, "ACME-3", "a3", clock(11, 0), clock(11, 15), moveAt(11, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(11, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 0, phase.Implementing, workflow.TriggerConflict))
	b.commented(clock(12, 20), "ACME-3", recordedSession("ACME-3", claim.RoleImplementer, "00000000-0000-4000-8000-0000000000c5"))
	b.segmentEnds(clock(12, 25), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: "5b8e1d3f7a2c4069b1c2d3e4f5a6b7c8d9e0f1a2", At: clock(10, 25), Branch: "main"},
		{SHA: "7c4d2e9f1a3b5067b1c2d3e4f5a6b7c8d9e0f1a2", At: clock(10, 40), Branch: "main"},
		{SHA: "9f2b6d4a8c1e3057c2d3e4f5a6b7c8d9e0f1a2b3", At: clock(11, 50), Branch: "main"},
	}}
	return window
}

// Where a drawn conflict loop takes the place of an issue's own recorded loop, the events of the
// issue's own loop go with the drawn work that stands in for theirs, the segment of the same role
// and phase, at their offsets, or at its end where it is shorter (giveEvents), and enter once. The
// test replays givenLoopWindow under legion-v20.42.5 at the seeds 1 to 8. ACME-3's stretch draws
// its conflict at 12:00 and is given, by the seed, its own loop or ACME-4's. Given its own, it is
// back in testing at 12:25 and its implementer's comment enters at 12:20, as recorded; given
// ACME-4's, its implementer works that loop's 10 minutes, it is back in testing at 12:10, and the
// comment, 20 minutes into ACME-3's own loop, enters at 12:10, the end of the shorter loop's
// segment of the implementer, before the move it ends with. The test fails where no seed gives
// ACME-3 ACME-4's loop.
func TestAnEventOfALoopADrawnLoopReplacesGoesWithIt(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := givenLoopWindow(t)
	given := 0
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var copied *shadow
			out := t.TempDir()
			cfg := RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: drawnConflictBranch, Seed: seed, Slowdown: 1, PGDSN: dsn,
				Out: out}
			cfg.atStart = func(s *shadow) { copied = s }
			if _, err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("replay givenLoopWindow under legion-v20.42.5 at seed %d: %v", seed, err)
			}
			back := linesAt(replayLog(t, out), func(line replayLine) bool {
				return line.Msg == phaseChangedLine && line.Issue == "ACME-3" && line.From == string(phase.Implementing) &&
					line.To == string(phase.Testing) && line.Time.After(clock(12, 0))
			})
			want := clock(12, 20)
			switch {
			case slices.EqualFunc(back, []time.Time{clock(12, 10)}, time.Time.Equal):
				given++
				want = clock(12, 10)
			case !slices.EqualFunc(back, []time.Time{clock(12, 25)}, time.Time.Equal):
				t.Fatalf("at seed %d ACME-3 moved back to testing at %s, want at 12:10 after ACME-4's loop or at 12:25 after its own",
					seed, describeTimes(back))
			}
			if applied := eventsApplied(t, copied, "ACME-3", "comment.created"); !slices.EqualFunc(applied, []time.Time{want}, time.Time.Equal) {
				t.Errorf("at seed %d the copy of Dispatch holds the implementer's comment on ACME-3 applied at %s, want once, at %s",
					seed, describeTimes(applied), describeTimes([]time.Time{want}))
			}
		})
	}
	if given == 0 {
		t.Error("no seed from 1 to 8 gave ACME-3 ACME-4's loop")
	}
}

// cancelledQuestionWindow is a window from 09:00 to 13:00 recorded under main, whose cap is 1 and
// whose deployment gates the spec, in which a root's later start in a replay under keep-past-review
// runs its architect's question past the person's close of the root. ACME-1, a root, holds the slot
// from 09:00 through its review, gives it back at its accepting at 10:15 and merges at 11:00; ACME-2
// is promoted at 10:15, and its architect, whose session Dispatch names under the title `Legion
// architect · ACME-2`, writes version 1 of its spec 10 minutes into its turn (10:25) and registers
// it, unapproved, at 10:30. A person approves the version 30 minutes after it (10:55), which opens
// the gate and starts ACME-2's planning, and closes ACME-2 at 11:05.
func cancelledQuestionWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	recordCancelledQuestion(b, nil)
	return b.build()
}

// recordCancelledQuestion writes cancelledQuestionWindow's record into b, and runs before, where a
// window built on it adds acts, just before the person's close of ACME-2 at 11:05.
func recordCancelledQuestion(b *windowBuilder, before func()) {
	b.t.Helper()
	b.capped(1)
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 0), "ACME-1")
	b.file(clock(9, 2), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.specWritten(clock(9, 5), "ACME-1")
	b.approves(clock(9, 10), "ACME-1", "")
	b.rootDesigned(clock(9, 15), "ACME-1")
	b.segmentEnds(clock(9, 30), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(9, 45), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 0), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(10, 15), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.segmentEnds(clock(10, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.segmentEnds(clock(10, 45), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.turnEnds(clock(11, 0), "ACME-1", claim.RoleMerger)
	b.moved(clock(11, 0), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(11, 0), "ACME-1", record.DoneStatus)
	b.rootPromoted(clock(10, 15), "ACME-2")
	b.specWrittenBy(clock(10, 25), "ACME-2", recordedSession("ACME-2", claim.RoleArchitect, "00000000-0000-4000-8000-0000000000c6"))
	b.turnEnds(clock(10, 30), "ACME-2", claim.RoleArchitect)
	b.registered(clock(10, 30), "ACME-2", false)
	b.approves(clock(10, 55), "ACME-2", "")
	b.gateOpened(clock(10, 55), "ACME-2", intake.DispatchArtifactApproved, 1)
	b.rootFirstPhase(clock(10, 55), "ACME-2")
	if before != nil {
		before()
	}
	b.edit(clock(11, 5), "ACME-2", setStatus(record.DoneStatus))
	b.leaves(clock(11, 5), "ACME-2", record.DoneStatus)
}

// Under answer offset a person's answer to a worker's question comes at its recorded offset from
// the replay's moment of the question, wherever the replay places the question (placeAnswers), so
// no answer comes before its question. The test replays cancelledQuestionWindow under
// keep-past-review, where ACME-1 keeps its slot past its review to its merge at 11:00, so ACME-2 is
// promoted then, 45 minutes later than in the record, and its architect's version falls at 11:10.
// The person's close of ACME-2 at 11:05 cancels the architect's segment before it: the version
// enters by its anchor, at once (11:05), and the approval 30 minutes after it, at 11:35. Timed from
// the version's place in the cancelled segment, the approval would enter at 11:40; and the version,
// dropped with the segment, never entered at all.
func TestAnAnswerComesFromItsQuestionsNewPlace(t *testing.T) {
	t.Parallel()
	var copied *shadow
	out := t.TempDir()
	cfg := RunConfig{Window: cancelledQuestionWindow(t), Rule: replayedRule(t, "keep-past-review"), Branch: MainBranch, Seed: 1,
		Slowdown: 1, PGDSN: replayDSN(t), Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay cancelledQuestionWindow under keep-past-review: %v", err)
	}
	taken := linesAt(replayLog(t, out), func(line replayLine) bool { return line.Msg == "admission: slot taken" && line.Issue == "ACME-2" })
	if want := []time.Time{clock(11, 0)}; !slices.EqualFunc(taken, want, time.Time.Equal) {
		t.Fatalf("ACME-2 took a slot at %s, want at %s, as ACME-1 merges", describeTimes(taken), describeTimes(want))
	}
	for _, want := range []struct {
		kind string
		at   []time.Time
	}{{"artifact.version", []time.Time{clock(11, 5)}}, {"artifact.approved", []time.Time{clock(11, 35)}}} {
		if applied := eventsApplied(t, copied, "ACME-2", want.kind); !slices.EqualFunc(applied, want.at, time.Time.Equal) {
			t.Errorf("the copy of Dispatch holds the %s of ACME-2 applied at %s, want once, at %s", want.kind, describeTimes(applied),
				describeTimes(want.at))
		}
	}
}

// lateChildFilingWindow is a window from 09:00 to 14:00 recorded under main with a cap of 2, its
// inputs reaching back to the daemon's boot at 06:00. ACME-1, a root promoted at 06:05, implements
// from 07:00 to the window's end, holding a slot. ACME-2, a root promoted at 06:30, holds the other
// slot to its review's approval at 10:00, and merges at 12:00. ACME-3, which a person files todo
// under ACME-1 at 09:30, takes that slot at 10:00, its first start. At 10:20 ACME-3's architect,
// whose session Dispatch names under the title `Legion architect · ACME-3`, files ACME-4 under it in
// triage, while only ACME-3's planner has work: no work segment of ACME-3's places the filing. Under
// keep-past-review ACME-2 keeps its slot past its review to its merge at noon, so ACME-3 can take
// its first start only then, two hours late.
func lateChildFilingWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.reaches(3 * time.Hour)
	b.capped(2)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(7, 0, phase.Implementing, workflow.TriggerPlannerCompleted))
	rootThrough(b, "ACME-2", "a2", clock(6, 30), clock(6, 45), moveAt(7, 15, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 15, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 0, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 40, phase.Merging, workflow.TriggerRetroCompleted), moveAt(12, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(12, 0), "ACME-2", record.DoneStatus)
	b.file(clock(9, 30), "ACME-3", filing{Title: "Make the shelf map", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a3"})
	b.childStarts(clock(10, 0), "ACME-3")
	b.filedBy(clock(10, 20), "ACME-4", filing{Title: "Label the shelves", Parent: "ACME-3", Tree: "ACME-1", Status: "triage", Rank: "a4"},
		recordedSession("ACME-3", claim.RoleArchitect, "00000000-0000-4000-8000-0000000000c3"))
	return b.build()
}

// An architect's filing of a child under its own issue that no work segment of the issue places,
// made after the issue's first start in the record, enters at its recorded offset from the replay's
// first start of the issue, or at its recorded time where that is later (spec R2: a fact that
// depends on a start moves with that start). The child's open status holds the issue's first start
// (an issue waits for the Dispatch children under it), so a filing applied before the replay starts
// the issue would hold a start the record made before the filing. The test replays
// lateChildFilingWindow under keep-past-review with no wait that stops the phase
// (mechanismNoChildrenWait): ACME-3 takes its first start at noon, and ACME-4 is
// filed 20 minutes after it, at 12:20. It fails where the filing keeps its recorded time, for which
// ACME-4 is filed at 10:20, while ACME-3 waits for its slot, and holds ACME-3's start to the window's
// end.
func TestAnArchitectsFilingUnderItsIssueMovesWithTheIssuesStart(t *testing.T) {
	t.Parallel()
	log, copied := replayCopiedUnder(t, lateChildFilingWindow(t), mechanismNoChildrenWait(t, "keep-past-review"))
	if started, want := startedAt(log, "ACME-3"), []time.Time{clock(12, 0)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-3 started at %s, want at %s, as ACME-2 merges", describeTimes(started), describeTimes(want))
	}
	if filed, want := eventsApplied(t, copied, "ACME-4", "issue.created"), []time.Time{clock(12, 20)}; !slices.EqualFunc(filed, want, time.Time.Equal) {
		t.Errorf("ACME-4 was filed at %s, want at %s, 20 minutes after ACME-3's start", describeTimes(filed), describeTimes(want))
	}
}

// lateChildRecord is a window from 09:00 to 16:00 recorded under main with a cap of 2, its inputs
// reaching back to the daemon's boot at 06:00, in which a child's first start comes two hours late
// in a replay under keep-past-review. ACME-1, a root promoted at 06:05, implements from 07:00 to the
// window's end, holding a slot. ACME-2, a root promoted at 06:30, holds the other slot to its
// review's approval at 10:00, and merges at 12:00. ACME-3, which a person files todo under ACME-1 at
// 09:30, takes that slot at 10:00, its first start, and its planner works from then on. Under
// keep-past-review ACME-2 keeps its slot past its review to its merge, so ACME-3 can take its first
// start only at noon. tail writes what the record shows next.
func lateChildRecord(t *testing.T, tail func(b *windowBuilder)) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(16, 0))
	b.reaches(3 * time.Hour)
	b.capped(2)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(7, 0, phase.Implementing, workflow.TriggerPlannerCompleted))
	rootThrough(b, "ACME-2", "a2", clock(6, 30), clock(6, 45), moveAt(7, 15, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 15, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 0, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 40, phase.Merging, workflow.TriggerRetroCompleted), moveAt(12, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(12, 0), "ACME-2", record.DoneStatus)
	b.file(clock(9, 30), "ACME-3", filing{Title: "Make the shelf map", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a3"})
	b.childStarts(clock(10, 0), "ACME-3")
	tail(b)
	return b.build()
}

// architectOf is the session that runs key's architect, as Dispatch records it: by an id of its
// own, under the title `Legion architect · <KEY>`.
func architectOf(key string) eventActor {
	ids := map[string]string{"ACME-1": "00000000-0000-4000-8000-0000000000c1", "ACME-2": "00000000-0000-4000-8000-0000000000c2",
		"ACME-3": "00000000-0000-4000-8000-0000000000c3"}
	return recordedSession(key, claim.RoleArchitect, ids[key])
}

// shelfLabels is the filing of ACME-4 under ACME-3 in triage at at, by the actor by.
func shelfLabels(b *windowBuilder, at time.Time, by eventActor) {
	b.t.Helper()
	b.filedBy(at, "ACME-4", filing{Title: "Label the shelves", Parent: "ACME-3", Tree: "ACME-1", Status: "triage", Rank: "a4"}, by)
}

// retitled is the actor by editing key's title at at.
func retitled(b *windowBuilder, at time.Time, key string, by eventActor) {
	b.t.Helper()
	b.changed(at, key, by, func(issue *builtIssue) { issue.Title += ", by aisle" })
}

// appliedBy is each moment the replay applied an event of kind on key that actor made to its copy of
// Dispatch (shadow), the copy the run handed its atStart.
func appliedBy(t *testing.T, copied *shadow, key, kind string, actor eventActor) []time.Time {
	t.Helper()
	if copied == nil {
		t.Fatal("the run never handed the test its copy of Dispatch at the window's start")
	}
	iss := copied.issues[key]
	if iss == nil {
		return nil
	}
	var applied []time.Time
	for i, raw := range iss.log[iss.seeded:] {
		var event struct {
			Type      string     `json:"type"`
			CreatedAt time.Time  `json:"created_at"`
			Actor     eventActor `json:"actor"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("read event %d of %s in the copy of Dispatch: %v", i+1, key, err)
		}
		if event.Type == kind && event.Actor.ID == actor.ID {
			applied = append(applied, event.CreatedAt)
		}
	}
	return applied
}

// replayRecording replays window under keep-past-review with no wait that stops the phase
// (mechanismNoChildrenWait) on the main branch at seed 1 and f 1, and answers the replay's log, its
// copy of Dispatch, and the first moment its daemon recorded key, zero where it never did.
func replayRecording(t *testing.T, window Window, key string) ([]replayLine, *shadow, time.Time) {
	t.Helper()
	var copied *shadow
	var entered time.Time
	out := t.TempDir()
	cfg := RunConfig{Window: window, Rule: mechanismNoChildrenWait(t, "keep-past-review"), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
		if !entered.IsZero() {
			return nil
		}
		var recorded int
		if err := st.Pool().QueryRow(ctx, `select count(*) from issues where key = $1`, key).Scan(&recorded); err != nil {
			return err
		}
		if recorded > 0 {
			entered = at
		}
		return nil
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay the window under keep-past-review: %v", err)
	}
	return replayLog(t, out), copied, entered
}

// An architect's act on a child it filed follows that filing, at its recorded offset from it, and
// never before its recorded time (spec R2: the child's filing moves with the start it depends on,
// and the child's own course follows the filing). The test replays lateChildRecord under
// keep-past-review, in which ACME-3's architect files ACME-4 under ACME-3 in triage at 10:20, the
// architect's release writes ACME-4 todo under the daemon's session at 10:21, and the architect edits
// ACME-4's title. ACME-3 takes its first start at noon, so the filing enters at 12:20 and the release
// at 12:21, where the replay's daemon first records ACME-4. In the first case the edit, made at
// 10:30, enters at 12:30; in the second, made at 12:40, 2 hours 20 minutes after the filing, it
// enters at 14:40. The shape is a validation round's: a parent's architect edited a child it had
// filed while the replay had not yet started the parent, and the edit, at its recorded time, came
// before the filing. The first case fails where the edit keeps its recorded time: it gives the copy
// of Dispatch ACME-4 todo under ACME-3 at 10:30, the daemon records the child then, ACME-3 waits for
// it and never starts, and the filing that waits for ACME-3's start never enters. The second fails
// where the edit enters at its recorded time, 12:40, or where it only waits for the filing.
func TestAnArchitectsActOnAChildItFiledFollowsTheFiling(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name           string
		edited, enters time.Time
	}{
		{name: "the filing moves past the act", edited: clock(10, 30), enters: clock(12, 30)},
		{name: "the act keeps its offset from the filing", edited: clock(12, 40), enters: clock(14, 40)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			window := lateChildRecord(t, func(b *windowBuilder) {
				shelfLabels(b, clock(10, 20), architectOf("ACME-3"))
				b.daemonWrites(clock(10, 21), "ACME-4", "todo")
				retitled(b, c.edited, "ACME-4", architectOf("ACME-3"))
			})
			log, copied, entered := replayRecording(t, window, "ACME-4")
			if started, want := startedAt(log, "ACME-3"), []time.Time{clock(12, 0)}; !slices.EqualFunc(started, want, time.Time.Equal) {
				t.Errorf("ACME-3 started at %s, want at %s, as ACME-2 merges", describeTimes(started), describeTimes(want))
			}
			if filed, want := eventsApplied(t, copied, "ACME-4", "issue.created"), []time.Time{clock(12, 20)}; !slices.EqualFunc(filed, want, time.Time.Equal) {
				t.Errorf("ACME-4 was filed at %s, want at %s, 20 minutes after ACME-3's start", describeTimes(filed), describeTimes(want))
			}
			edited := appliedBy(t, copied, "ACME-4", "issue.updated", architectOf("ACME-3"))
			if want := []time.Time{c.enters}; !slices.EqualFunc(edited, want, time.Time.Equal) {
				t.Errorf("the architect's edit of ACME-4 entered at %s, want at %s, at its recorded offset from the filing", describeTimes(edited),
					describeTimes(want))
			}
			if !entered.Equal(clock(12, 21)) {
				t.Errorf("the replay's daemon first recorded ACME-4 at %s, want at 12:21, its release after its filing", stamp(entered))
			}
		})
	}
}

// An act of an issue's own architect on the issue, made after its first start in the record where no
// work segment of the architect's places it, enters at its recorded offset from the replay's first
// start of the issue, or at its recorded time where that is later (spec R2: a fact that depends on a
// start moves with that start). The test replays lateChildRecord under keep-past-review, in which a
// person files ACME-5 in backlog at 09:15 and ACME-3's architect sets ACME-3 blocked by ACME-5 at
// 10:20, twenty minutes after ACME-3's first start, while only ACME-3's planner has work. It first
// reads the window's script and finds no piece of the architect's work at 10:20 (script.workPiece):
// the act falls outside every piece of its role. ACME-3 then takes its first start at noon, and the
// link enters at 12:20. The shape is a validation round's: an issue's own architect set a blocker on
// it at its recorded time, before the replay started the issue, and the issue got no slot in the
// window. It fails where the link keeps its recorded time, for which ACME-3 stands blocked by an
// unfinished issue from 10:20 and never starts.
func TestAnArchitectsActOnItsOwnUnstartedIssueMovesWithItsStart(t *testing.T) {
	t.Parallel()
	window := lateChildRecord(t, func(b *windowBuilder) {
		b.file(clock(9, 15), "ACME-5", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "c5"})
		b.changed(clock(10, 20), "ACME-3", architectOf("ACME-3"), func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-5"} })
	})
	in, err := newWindowInputs(window)
	if err != nil {
		t.Fatalf("build the window's trace: %v", err)
	}
	s, err := buildScript(in.trace, in.events, in.lines)
	if err != nil {
		t.Fatalf("read the window's script: %v", err)
	}
	if p := s.workPiece(SessionClaim{Issue: "ACME-3", Role: claim.RoleArchitect}, clock(10, 20)); p != nil {
		t.Errorf("ACME-3's architect acts at 10:20 inside its piece %d in %s, want outside every piece of its role", p.index, p.phase)
	}
	log, copied := replayCopiedUnder(t, window, mechanismNoChildrenWait(t, "keep-past-review"))
	if started, want := startedAt(log, "ACME-3"), []time.Time{clock(12, 0)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-3 started at %s, want at %s, as ACME-2 merges", describeTimes(started), describeTimes(want))
	}
	linked := appliedBy(t, copied, "ACME-3", "issue.updated", architectOf("ACME-3"))
	if want := []time.Time{clock(12, 20)}; !slices.EqualFunc(linked, want, time.Time.Equal) {
		t.Errorf("the architect's link on ACME-3 entered at %s, want at %s, 20 minutes after ACME-3's start", describeTimes(linked),
			describeTimes(want))
	}
}

// A child's filing under an issue that no work segment places, made after the issue's first start in
// the record, enters at its recorded offset from the replay's first start of the issue, or at its
// recorded time where that is later, whoever filed it (spec R2: a child's filing enters at its
// recorded offset from the start of the issue it is filed under). The test replays lateChildRecord
// under keep-past-review, in which ACME-4 is filed in triage under ACME-3, a member of ACME-1's tree,
// at 10:20: by the tree root's architect, and by a person. ACME-3 takes its first start at noon, and
// ACME-4 is filed at 12:20. The shape is a validation round's: a tree root's architect filed a child
// under a member of its tree that the replay had not yet started, and the member waited for its
// children from that moment and started more than a day late. It fails where the filing keeps its
// recorded time, for which ACME-4 is filed at 10:20, while ACME-3 waits for its slot, and holds
// ACME-3's start to the window's end.
func TestAFilingUnderAnIssueMovesWithItsStartWhoeverFiledIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		by   eventActor
	}{
		{name: "the tree root's architect", by: architectOf("ACME-1")},
		{name: "a person", by: builtPerson},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			log, copied := replayCopiedUnder(t, lateChildRecord(t, func(b *windowBuilder) { shelfLabels(b, clock(10, 20), c.by) }),
				mechanismNoChildrenWait(t, "keep-past-review"))
			if started, want := startedAt(log, "ACME-3"), []time.Time{clock(12, 0)}; !slices.EqualFunc(started, want, time.Time.Equal) {
				t.Errorf("ACME-3 started at %s, want at %s, as ACME-2 merges", describeTimes(started), describeTimes(want))
			}
			if filed, want := eventsApplied(t, copied, "ACME-4", "issue.created"), []time.Time{clock(12, 20)}; !slices.EqualFunc(filed, want, time.Time.Equal) {
				t.Errorf("ACME-4 was filed at %s, want at %s, 20 minutes after ACME-3's start", describeTimes(filed), describeTimes(want))
			}
		})
	}
}

// No recorded event of an issue enters a replay before the issue's filing, whoever made it and
// however it is placed: an event that would enter first waits for the filing, and enters at its
// recorded offset from it, or at its recorded time where that is later (driver.awaitsFiling). The
// test replays lateChildRecord under keep-past-review, in which ACME-3's architect files ACME-4 under
// ACME-3 in triage at 10:20, the architect's release writes ACME-4 todo under the daemon's session
// at 10:21, and at 10:30 the architect of another issue, ACME-2, edits ACME-4's title. ACME-3 takes
// its first start at noon, so the filing enters at 12:20 and the release at 12:21, where the
// replay's daemon first records ACME-4, and the edit, held back for the filing, at 12:30. The shape
// is a validation round's: a root's architect filed children in the root's accepting work, which
// the replay ran hours late, and each child's later edit, at its recorded time, came before its
// filing; the copy of Dispatch took the child whole from the edit, at a status of its later work, the
// filing and the release changed none of it, and the replay never filed the child into its workflow.
// It fails where the edit keeps its recorded time: it gives the copy of Dispatch ACME-4 todo under
// ACME-3 at 10:30, the daemon records the child then, ACME-3 waits for it and never starts, and the
// filing that waits for ACME-3's start never enters.
func TestNoEventOfAnIssueEntersBeforeItsFiling(t *testing.T) {
	t.Parallel()
	window := lateChildRecord(t, func(b *windowBuilder) {
		shelfLabels(b, clock(10, 20), architectOf("ACME-3"))
		b.daemonWrites(clock(10, 21), "ACME-4", "todo")
		retitled(b, clock(10, 30), "ACME-4", architectOf("ACME-2"))
	})
	log, copied, entered := replayRecording(t, window, "ACME-4")
	if started, want := startedAt(log, "ACME-3"), []time.Time{clock(12, 0)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-3 started at %s, want at %s, as ACME-2 merges", describeTimes(started), describeTimes(want))
	}
	if filed, want := eventsApplied(t, copied, "ACME-4", "issue.created"), []time.Time{clock(12, 20)}; !slices.EqualFunc(filed, want, time.Time.Equal) {
		t.Errorf("ACME-4 was filed at %s, want at %s, 20 minutes after ACME-3's start", describeTimes(filed), describeTimes(want))
	}
	edited := appliedBy(t, copied, "ACME-4", "issue.updated", architectOf("ACME-2"))
	if want := []time.Time{clock(12, 30)}; !slices.EqualFunc(edited, want, time.Time.Equal) {
		t.Errorf("ACME-2's architect's edit of ACME-4 entered at %s, want at %s, 10 minutes after the filing", describeTimes(edited),
			describeTimes(want))
	}
	if !entered.Equal(clock(12, 21)) {
		t.Errorf("the replay's daemon first recorded ACME-4 at %s, want at 12:21, its release after its filing", stamp(entered))
	}
}

// skippedLoopChildWindow is a window from 09:00 to 14:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 06:00. ACME-1, a root promoted at 06:05, moves into testing
// at 08:00 and into accepting at 08:20, and a conflict moves it back to implementing at 09:10; it is
// back in accepting at 10:20, and merges at 11:20. Inside that loop, at 10:15, its architect files
// ACME-2 under it in triage, and at 10:16 the daemon's session writes ACME-2 todo (the architect's
// release), and ACME-2 starts. A person closes ACME-2 at 10:35. ACME-1's stretch from 10:00 is the
// window's one stretch in the fit, and it ends in no conflict, so the fitted chance is 0.
func skippedLoopChildWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.reaches(3 * time.Hour)
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(7, 0, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 10, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(8, 20, phase.Accepting, workflow.TriggerReviewApproved), moveAt(9, 10, phase.Implementing, workflow.TriggerConflict),
		moveAt(10, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 10, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 20, phase.Accepting, workflow.TriggerReviewApproved), moveAt(11, 0, phase.Retro, workflow.TriggerSignOff),
		moveAt(11, 10, phase.Merging, workflow.TriggerRetroCompleted), moveAt(11, 20, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(11, 20), "ACME-1", record.DoneStatus)
	b.filedBy(clock(10, 15), "ACME-2", filing{Title: "Add shelf labels", Parent: "ACME-1", Tree: "ACME-1", Status: "triage", Rank: "b2"},
		b.session("ACME-1", claim.RoleArchitect))
	b.daemonWrites(clock(10, 16), "ACME-2", "todo")
	b.childStarts(clock(10, 16), "ACME-2")
	b.edit(clock(10, 35), "ACME-2", setStatus(record.DoneStatus))
	b.leaves(clock(10, 35), "ACME-2", record.DoneStatus)
	return b.build()
}

// A recorded conflict loop the replay passes over takes none of its issue's acts on other issues
// with it (assumption 3): a filing of a child, its release, a link or an edit of another issue keeps
// its recorded time, while the loop's own work drops out, and the issue does not leave the workflow
// before the entry of a child that the record filed under it before its own leave, as the daemon
// holds a merged issue's close while an issue beneath it is in the workflow. The test replays
// skippedLoopChildWindow under legion-v20.42.5: ACME-1 draws no conflict at 09:10 and runs on past
// its loop to merging by 10:10, but does not close there; ACME-2 is filed at 10:15, as recorded,
// the replay's daemon records it at 10:16, at its release, and ACME-1 moves to done only after
// that. ACME-2 then waits beneath its parent, which the daemon has accepted (its own hold of such a
// child), until the person closes it. It fails where ACME-1 closes before the filing, for which
// ACME-2 is filed under a tree no longer running and the replay never records it.
func TestAChildFiledInASkippedLoopEntersAtItsRecordedTime(t *testing.T) {
	t.Parallel()
	var copied *shadow
	var entered time.Time
	out := t.TempDir()
	cfg := RunConfig{Window: skippedLoopChildWindow(t), Rule: replayedRule(t, "legion-v20.42.5"), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
		if !entered.IsZero() {
			return nil
		}
		var recorded int
		if err := st.Pool().QueryRow(ctx, `select count(*) from issues where key = $1`, "ACME-2").Scan(&recorded); err != nil {
			return err
		}
		if recorded > 0 {
			entered = at
		}
		return nil
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay skippedLoopChildWindow under legion-v20.42.5: %v", err)
	}
	if filed, want := eventsApplied(t, copied, "ACME-2", "issue.created"), []time.Time{clock(10, 15)}; !slices.EqualFunc(filed, want, time.Time.Equal) {
		t.Errorf("ACME-2 was filed at %s, want at %s, its recorded time", describeTimes(filed), describeTimes(want))
	}
	if !entered.Equal(clock(10, 16)) {
		t.Errorf("the replay's daemon first records ACME-2 at %s, want at 10:16, its release", stamp(entered))
	}
	done := linesAt(replayLog(t, out), func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.To == string(phase.Done)
	})
	if len(done) != 1 || done[0].Before(clock(10, 16)) {
		t.Errorf("ACME-1 moved to done at %s, want once, no sooner than its child's entry at 10:16", describeTimes(done))
	}
}
