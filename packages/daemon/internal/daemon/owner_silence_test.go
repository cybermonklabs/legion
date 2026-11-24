package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The sweep mints an owner's silence for an architect idle past the bound over a run of its issue
// that started and has not ended, past the bound too, keyed on that run: LEGION-1's architect,
// idle two hours over a run that started an hour ago, is one fact naming the architect; LEGION-2's,
// whose issue holds no open run, is none; LEGION-3's, over a run that started ten minutes ago, is
// none, since the run's own notice has had less than the bound; LEGION-4's, whose own session
// says it waits on background work, is none. A later run of LEGION-1's place, with the owner
// still silent, is a fact under another id. A phase worker's silence is minted as before.
//
// The control: the sweep before the owner's silence skipped every architect's claim and minted
// the tester's fact alone.
func TestTheSweepMintsAnOwnersSilenceOverAnOpenRunPastTheBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	bound := 30 * time.Minute
	idle := func(issue string, role claim.Role) supervise.Claim {
		return supervise.Claim{Token: mustClaimToken(t, issue, role), Issue: issue, Role: role, Generation: 4, State: supervise.StateIdle,
			Step: supervise.StepRecord{At: now.Add(-2 * time.Hour)}}
	}
	background := idle("LEGION-4", claim.RoleArchitect)
	background.ActivityBackground = true
	claims := []supervise.Claim{idle("LEGION-1", claim.RoleArchitect), idle("LEGION-2", claim.RoleArchitect),
		idle("LEGION-3", claim.RoleArchitect), background, idle("LEGION-5", claim.RoleTester)}
	runs := map[string]record.IssueStepRun{
		"LEGION-1": {Issue: "LEGION-1", Place: record.StepPlace{Task: "a", Kind: "step", Place: 1},
			StepStart: record.StepStart{Starts: 1, StartedAt: now.Add(-time.Hour), Run: record.RunOpen}},
		"LEGION-3": {Issue: "LEGION-3", Place: record.StepPlace{Task: "a", Kind: "step", Place: 1},
			StepStart: record.StepStart{Starts: 1, StartedAt: now.Add(-10 * time.Minute), Run: record.RunOpen}},
		"LEGION-4": {Issue: "LEGION-4", Place: record.StepPlace{Task: "a", Kind: "step", Place: 1},
			StepStart: record.StepStart{Starts: 1, StartedAt: now.Add(-time.Hour), Run: record.RunOpen}},
	}
	var minted []sweptFact
	var asked []string
	s := &supervision{cfg: config.Config{WorkerSilence: bound, WorkerBackground: 4 * time.Hour}, log: quietLogger(),
		plan: plan{clock: &heldClock{at: now}},
		applySweepFacts: func(_ context.Context, facts []sweptFact) error {
			minted = append(minted, facts...)
			return nil
		},
		openRuns: func(_ context.Context, issues []string) (map[string]record.IssueStepRun, error) {
			asked = append(asked, issues...)
			open := map[string]record.IssueStepRun{}
			for _, issue := range issues {
				if run, ok := runs[issue]; ok {
					open[issue] = run
				}
			}
			return open, nil
		},
	}

	s.tellSilentPhaseWorkers(context.Background(), claims)
	if want := []string{"LEGION-1", "LEGION-2", "LEGION-3"}; !slices.Equal(asked, want) {
		t.Fatalf("the sweep read the open runs of %v, want %v: the idle architects whose quiet nothing explains", asked, want)
	}
	owner := claims[0]
	wantOwner := sweptFact{ID: ownerSilentEventID(owner.Token, owner.Generation, owner.Step.At, runs["LEGION-1"]),
		Fact: intake.WorkerSilent{Issue: "LEGION-1", Role: claim.RoleArchitect, Generation: owner.ServingRun(), LastStep: owner.Step.At, Bound: bound}}
	tester := claims[4]
	// The tester's silence names the claim the sweep read it off, as every worker's silence does on
	// the base (intake.WorkerSilent.Claim); the owner's names none, since ownerSilent reads no claim.
	wantTester := sweptFact{ID: workerSilentEventID(tester.Token, tester.Generation, tester.Step.At),
		Fact: intake.WorkerSilent{Issue: "LEGION-5", Role: claim.RoleTester, Claim: tester.Token, Generation: tester.ServingRun(), LastStep: tester.Step.At, Bound: bound}}
	if len(minted) != 2 || minted[0] != wantTester || minted[1] != wantOwner {
		t.Fatalf("the sweep minted %+v, want the tester's silence %+v and LEGION-1's owner's %+v", minted, wantTester, wantOwner)
	}

	later := runs["LEGION-1"]
	later.Starts, later.StartedAt = 2, now.Add(-45*time.Minute)
	runs["LEGION-1"] = later
	minted = nil
	s.tellSilentPhaseWorkers(context.Background(), claims)
	if len(minted) != 2 || minted[1].ID == wantOwner.ID {
		t.Fatalf("the sweep over a later run of LEGION-1 minted %+v, want an owner's silence under another id than %s", minted, wantOwner.ID)
	}
}
