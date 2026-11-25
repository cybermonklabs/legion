package replay

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// approvedAgainWindow is a window recorded under main, in a deployment that gates the spec, by a
// release that records nothing of what a holder took of its tree's approved design. ACME-1, a
// root, is promoted at 09:00; its architect writes version 1 of its spec at 09:05, a person
// approves it at 09:10, and the architect registers it open at 09:15, so its planning starts then.
// At 10:00, while ACME-1 is implementing, its architect writes version 2: the gate closes, and the
// phase goes on. At 10:20 a person approves version 2, and the gate opens again. ACME-1 goes on
// through testing, its review, its acceptance and retro, and its merger merges from 11:30 to 11:45.
func approvedAgainWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 0), "ACME-1")
	b.specWritten(clock(9, 5), "ACME-1")
	b.approves(clock(9, 10), "ACME-1", "")
	b.rootDesigned(clock(9, 15), "ACME-1")
	b.segmentEnds(clock(9, 30), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.specRevised(clock(10, 0), "ACME-1", 2)
	b.gateClosed(clock(10, 0), "ACME-1", intake.DispatchArtifactVersion, 2)
	b.approvesVersion(clock(10, 20), "ACME-1", "", 2)
	b.gateOpened(clock(10, 20), "ACME-1", intake.DispatchArtifactApproved, 2)
	b.segmentEnds(clock(10, 30), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 45), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(11, 0), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.segmentEnds(clock(11, 15), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.segmentEnds(clock(11, 30), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.segmentEnds(clock(11, 45), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(11, 45), "ACME-1", record.DoneStatus)
	return b.build()
}

// The news of an approved design runs in a replay as the daemon runs it (designChangesRow), is
// told once, and holds no merger. The test replays approvedAgainWindow under main. ACME-1's
// implementer took version 1 at its planning's start (workflow's Engine.enter), so the approval of
// version 2 at 10:20 owes the holder of ACME-1's phase the news of that version: the reconcile
// records the change set and queues the implementer's news-only start, which waits behind the task
// the implementer holds until its segment ends at 10:30 and then finishes on the move it serves;
// the tester's start at 10:30 carries the news, which the stub credits (creditNews). No later move
// owes ACME-1's holder anything, so no reconcile queues another start: the merger's start at 11:30
// is owed nothing and waits for nothing, its turn starts at 11:30, and ACME-1 merges at 11:45, as in
// the record.
func TestTheNewsOfAnApprovedVersionIsToldOnceAndHoldsNoMerger(t *testing.T) {
	t.Parallel()
	window := approvedAgainWindow(t)
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out})
	if err != nil {
		t.Fatalf("replay the window under main: %v", err)
	}
	merger := string(builtToken(t, "ACME-1", claim.RoleMerger))
	queued := map[string][]time.Time{}
	var mergerTurns, done []time.Time
	for _, line := range replayLog(t, out) {
		switch {
		case line.Msg == "outbox design_changes queued a news-only start" && line.Issue == "ACME-1":
			queued[line.Role] = append(queued[line.Role], line.Time)
		case line.Msg == turnStartedLine && line.Claim == merger:
			mergerTurns = append(mergerTurns, line.Time)
		case line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.To == string(phase.Done):
			done = append(done, line.Time)
		}
	}
	if got, want := queued[string(claim.RoleImplementer)], []time.Time{clock(10, 20)}; !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Errorf("the reconcile queued the news-only start of ACME-1's implementer at %s, want at %s", describeTimes(got), describeTimes(want))
	}
	for role, at := range queued {
		for _, moment := range at {
			if !moment.Equal(clock(10, 20)) {
				t.Errorf("the reconcile queued a news-only start of ACME-1's %s at %s, after the news was told at 10:20", role, stamp(moment))
			}
		}
	}
	if want := []time.Time{clock(11, 30)}; !slices.EqualFunc(mergerTurns, want, time.Time.Equal) {
		t.Errorf("ACME-1's merger began its turns at %s, want at %s", describeTimes(mergerTurns), describeTimes(want))
	}
	if want := []time.Time{clock(11, 45)}; !slices.EqualFunc(done, want, time.Time.Equal) {
		t.Errorf("ACME-1 moved to done at %s, want at %s", describeTimes(done), describeTimes(want))
	}
	if result.Divergence.Retried == 0 {
		t.Error("the stub gave no row back to wait on its backoff, want the implementer's news-only start to wait behind its task")
	}
}

// The replay's holder of a member's phase is the daemon's (record.PhaseHolder, held by
// TestOnlyAPhaseWorkerHoldsAMembersPhase): a phase worker holds a member's phase, and only a phase
// worker does. Admitted, which phase.RoleFor answers the tree's architect for, has no holder, since
// a member there has not started its work and its first phase's start carries its news; a phase no
// role works names none; a member in reviewing names its reviewer's claim, one in implementing its
// implementer's and one in merging its merger's. Accepting, which the owning architect holds, is
// named through the owner walk and not here. A tree whose root names no project token holds no
// claim for any member.
func TestOnlyAPhaseWorkerHoldsAMembersPhaseInTheReplay(t *testing.T) {
	t.Parallel()
	root := record.Issue{Key: "ACME-1", Project: "ACME", Phase: phase.Implementing}
	claims := record.NewNewsClaims(root, &record.NewsFacts{Members: []record.Issue{root}})
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		phase phase.Phase
		role  claim.Role
	}{
		{phase.Admitted, ""},
		{phase.AwaitingPerson, ""},
		{phase.Implementing, claim.RoleImplementer},
		{phase.Reviewing, claim.RoleReviewer},
		{phase.Merging, claim.RoleMerger},
	} {
		member := record.Issue{Key: "ACME-2", Project: "ACME", Phase: tc.phase}
		holder, held, err := claims.HolderOf(context.Background(), nil, nil, member)
		if err != nil {
			t.Fatalf("the holder of a member in %s: %v", tc.phase, err)
		}
		if held != (tc.role != "") || held && holder.Role != tc.role {
			t.Fatalf("the holder of a member in %s = %+v (held %t), want role %q", tc.phase, holder, held, tc.role)
		}
		if held {
			if want, _ := claim.NewToken(project, member.Key, tc.role); holder.Token != want {
				t.Fatalf("the holder of a member in %s is %s, want %s", tc.phase, holder.Token, want)
			}
		}
	}
	unnamed := record.NewNewsClaims(record.Issue{Key: "ACME-1", Phase: phase.Implementing}, &record.NewsFacts{})
	if _, held, err := unnamed.HolderOf(context.Background(), nil, nil, record.Issue{Key: "ACME-2", Phase: phase.Reviewing}); err != nil || held {
		t.Fatalf("a tree whose root names no project token held a member's phase (held %t, err %v), want no holder", held, err)
	}
}
