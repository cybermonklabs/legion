package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// waitNow is the moment every wait below was declared around. The rule reads no clock, so the
// times in it are the fixture's own.
var waitNow = time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)

// waitReading is everything WaitStands reads of one claim's wait, held together so a row can
// change exactly one term of the standing case and leave the rest as it stands.
type waitReading struct {
	issue record.Issue
	root  record.Issue
	row   record.PhaseRow
	run   record.RoleRun
	gate  record.TreeGate
}

func (w waitReading) stands() (record.ClaimWait, bool) {
	return WaitStands(w.issue, w.root, w.row, w.run, w.gate)
}

func (w waitReading) cause() string {
	return WaitEndCause(w.issue, w.root, w.row, w.run, w.gate)
}

// wait is the wait row the reading's claim holds.
func (w *waitReading) wait() *record.ClaimWait { return w.run.Claim.Wait }

// aStandingWait is the rule's true case: the implementer of an issue in implementing, of a tree
// that runs, declared a wait in this session, at this generation, phase and phases row entry, and
// its row records no completion. The tree's design gate is closed, which holds no implementer.
func aStandingWait() waitReading {
	issue := record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Implementing,
		Generation: 3, Status: "in_progress"}
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Admitted,
		Generation: 3, Status: "in_progress"}
	token := claim.Token("legion-legion-legion-209-implementer")
	wait := &record.ClaimWait{ID: "wait-1", Project: "legion", Token: token, Issue: issue.Key, Role: claim.RoleImplementer,
		Generation: 3, Phase: phase.Implementing, PhaseEntry: 4, TakenEntry: 4, Session: "ses_implementer",
		WaitingOn: "the review of the root's pull request", WakeAt: waitNow.Add(2 * time.Hour), DeclaredAt: waitNow}
	return waitReading{
		issue: issue,
		root:  root,
		row:   record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: token, Entry: 4},
		run: record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateIdle, Tree: root.Key,
			Session: "ses_implementer", Wait: wait}},
		gate: closedSpecGate(root.Key),
	}
}

// aMergersStandingWait is the true case of a merger: the merger of an issue in merging declared a
// wait, and the design gate stands open.
func aMergersStandingWait() waitReading {
	w := aStandingWait()
	w.issue.Phase = phase.Merging
	w.wait().Role, w.wait().Phase = claim.RoleMerger, phase.Merging
	w.row.Role = claim.RoleMerger
	w.gate = openSpecGate(w.root.Key)
	return w
}

// closedSpecGate is a tree gate that gates the spec, whose row holds version 2 of the document
// with no approval of it.
func closedSpecGate(root string) record.TreeGate {
	return record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}, Rows: map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec: {Issue: root, Step: record.DesignStepSpec, ArtifactID: "spec-artifact", LatestVersion: 2},
	}}
}

// openSpecGate is a tree gate that gates the spec, whose row's latest version a person approved.
func openSpecGate(root string) record.TreeGate {
	approved := 2
	return record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}, Rows: map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec: {Issue: root, Step: record.DesignStepSpec, ArtifactID: "spec-artifact", LatestVersion: 2,
			ApprovedVersion: &approved, EverOpened: true},
	}}
}

// A wait stands only while every term of WaitStands holds, and each term is load-bearing: each
// row below turns one term of a standing wait false, and the wait no longer stands, with the
// cause of that term (WaitEndCause). A claim holding no wait row with no end has no wait at all,
// and nothing to end.
func TestAWaitStandsUntilOneOfItsTermsFailsAndItsCauseNamesThatTerm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		start  func() waitReading
		change func(*waitReading)
		cause  string
	}{
		{name: "an implementer's wait, all its terms holding", start: aStandingWait},
		{name: "a merger's wait under an open design gate", start: aMergersStandingWait},
		{
			name:   "this daemon holds no claim of the role",
			change: func(w *waitReading) { w.run.Claim = nil },
		},
		{
			name:   "the claim holds no wait",
			change: func(w *waitReading) { w.run.Claim.Wait = nil },
		},
		{
			name:   "the claim's wait row has ended",
			change: func(w *waitReading) { w.wait().EndedAt, w.wait().EndCause = waitNow, record.WaitReplaced },
		},
		{
			name:   "the role's phases row records its completion",
			change: func(w *waitReading) { w.row.CompletedAt = waitNow.Add(time.Minute) },
			cause:  WaitEndCompleted,
		},
		{
			// Retro is the implementer's too, so the phase is the one term that moved.
			name:   "the issue moved from the wait's phase to another",
			change: func(w *waitReading) { w.issue.Phase = phase.Retro },
			cause:  WaitEndPhaseMoved(phase.Retro),
		},
		{
			name:   "the role's phases row stands on a new entry of the same phase",
			change: func(w *waitReading) { w.row.Entry = 5 },
			cause:  WaitEndEntryMoved(5),
		},
		{
			name: "the phase the wait was declared in is another role's",
			change: func(w *waitReading) {
				w.wait().Role = claim.RoleTester
				w.row.Role = claim.RoleTester
			},
			cause: WaitEndAnotherRolesPhase,
		},
		{
			name:   "the issue left the workflow by its Dispatch status",
			change: func(w *waitReading) { w.issue.Status = "backlog" },
			cause:  WaitEndLeftWorkflow,
		},
		{
			name:   "the tree's root lingers after its close",
			change: func(w *waitReading) { w.root.LingerUntil = &waitNow },
			cause:  WaitEndTreeLingers,
		},
		{
			name:   "the issue runs a new generation",
			change: func(w *waitReading) { w.issue.Generation = 4 },
			cause:  WaitEndNewGeneration,
		},
		{
			name:   "the claim runs a fresh session",
			change: func(w *waitReading) { w.run.Claim.Session = "ses_fresh" },
			cause:  WaitEndFreshSession,
		},
		{
			name:   "the design gate closed over a merger's wait",
			start:  aMergersStandingWait,
			change: func(w *waitReading) { w.gate = closedSpecGate(w.root.Key) },
			cause:  WaitEndDesignGateClosed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := tc.start
			if start == nil {
				start = aStandingWait
			}
			w := start()
			var declared record.ClaimWait
			if w.run.Claim != nil && w.run.Claim.Wait != nil {
				declared = *w.run.Claim.Wait
			}
			if tc.change != nil {
				tc.change(&w)
			}
			standing := tc.change == nil
			got, stands := w.stands()
			if stands != standing {
				t.Fatalf("WaitStands = %t, want %t", stands, standing)
			}
			if standing && got != declared {
				t.Errorf("WaitStands answered the wait %+v, want the claim's own %+v", got, declared)
			}
			if !standing && got != (record.ClaimWait{}) {
				t.Errorf("WaitStands answered the wait %+v for a wait that does not stand, want the zero wait", got)
			}
			if cause := w.cause(); cause != tc.cause {
				t.Errorf("WaitEndCause = %q, want %q", cause, tc.cause)
			}
		})
	}
}

// The cause is the first term that fails, in one fixed order, whatever else failed beside it:
// completed, the phase moved, another role's phase, left the workflow, the tree lingers, a new
// generation, a new entry of the same phase, a fresh session, and the design gate holding a
// merger. A reading where every term fails at once is repaired one term at a time, in that order,
// and at each step the cause names the next term still failing, until the wait stands.
func TestAWaitsEndCauseIsItsFirstFailingTermInOneFixedOrder(t *testing.T) {
	w := aMergersStandingWait()
	merging, declared := w.issue, *w.wait()
	w.row.CompletedAt = waitNow
	w.issue.Phase = phase.Done
	w.row.Entry = declared.PhaseEntry + 1
	w.wait().Role = claim.RoleTester
	w.issue.Status = "done"
	w.root.LingerUntil = &waitNow
	w.issue.Generation = declared.Generation + 1
	w.run.Claim.Session = "ses_fresh"
	w.gate = closedSpecGate(w.root.Key)

	for _, step := range []struct {
		cause  string
		repair func(*waitReading)
	}{
		{cause: WaitEndCompleted, repair: func(w *waitReading) { w.row.CompletedAt = time.Time{} }},
		{cause: WaitEndPhaseMoved(phase.Done), repair: func(w *waitReading) { w.issue.Phase = merging.Phase }},
		{cause: WaitEndAnotherRolesPhase, repair: func(w *waitReading) { w.wait().Role = declared.Role }},
		{cause: WaitEndLeftWorkflow, repair: func(w *waitReading) { w.issue.Status = merging.Status }},
		{cause: WaitEndTreeLingers, repair: func(w *waitReading) { w.root.LingerUntil = nil }},
		{cause: WaitEndNewGeneration, repair: func(w *waitReading) { w.issue.Generation = merging.Generation }},
		{cause: WaitEndEntryMoved(declared.PhaseEntry + 1), repair: func(w *waitReading) { w.row.Entry = declared.PhaseEntry }},
		{cause: WaitEndFreshSession, repair: func(w *waitReading) { w.run.Claim.Session = declared.Session }},
		{cause: WaitEndDesignGateClosed, repair: func(w *waitReading) { w.gate = openSpecGate(w.root.Key) }},
	} {
		if got := w.cause(); got != step.cause {
			t.Fatalf("WaitEndCause = %q, want %q: the first failing term in the fixed order", got, step.cause)
		}
		if _, stands := w.stands(); stands {
			t.Fatalf("WaitStands = true while %q still fails", step.cause)
		}
		step.repair(&w)
	}
	if got := w.cause(); got != "" {
		t.Fatalf("WaitEndCause = %q once every term is repaired, want none", got)
	}
	if _, stands := w.stands(); !stands {
		t.Fatal("WaitStands = false once every term is repaired, want the wait standing")
	}
}

// The design gate holds a merger in merging, and nobody else, by the outbox's own test on the
// merger's start (classify.StandingGate): a step the deployment gates whose row no approval has
// opened. The gate answered is that row. A gated step nothing has registered holds no merger,
// though it holds the tree's work and record.TreeGate.Open reads it closed, and neither does a
// deployment that gates no step; a merger under an open gate, a merger outside merging and any
// other role in any phase are never held.
func TestTheDesignGateHoldsOnlyAMergerInMergingWhereTheOutboxFenceHoldsItsStart(t *testing.T) {
	const root = "LEGION-208"
	approved := 1
	approvedSpec := record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: "spec-artifact",
		LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}
	unapprovedSpec := closedSpecGate(root).Rows[record.DesignStepSpec]
	merging := record.Issue{Key: "LEGION-209", Tree: root, Project: "LEGION", Phase: phase.Merging, Generation: 3, Status: "in_progress"}
	inPhase := func(p phase.Phase) record.Issue {
		issue := merging
		issue.Phase = p
		return issue
	}

	for _, tc := range []struct {
		name      string
		issue     record.Issue
		role      claim.Role
		gate      record.TreeGate
		held      bool
		treeOpens bool
	}{
		{
			name: "the merger of an issue in merging, the spec it gates unapproved", issue: merging, role: claim.RoleMerger,
			gate: closedSpecGate(root), held: true,
		},
		{
			name: "the merger of an issue in merging, under an open gate", issue: merging, role: claim.RoleMerger,
			gate: openSpecGate(root), treeOpens: true,
		},
		{
			name: "a gated step with no row, the only step not approved", issue: merging, role: claim.RoleMerger,
			gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec, record.DesignStepDecisions},
				Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: approvedSpec}},
		},
		{
			name: "an empty gates.design over an unapproved spec row", issue: merging, role: claim.RoleMerger,
			gate: record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: unapprovedSpec}},
		},
		{
			name: "the implementer of an issue in merging", issue: merging, role: claim.RoleImplementer,
			gate: closedSpecGate(root),
		},
		{
			name: "the reviewer of an issue in reviewing", issue: inPhase(phase.Reviewing), role: claim.RoleReviewer,
			gate: closedSpecGate(root),
		},
		{
			name: "the merger of an issue in reviewing", issue: inPhase(phase.Reviewing), role: claim.RoleMerger,
			gate: closedSpecGate(root),
		},
		{
			name: "the merger of an issue in retro", issue: inPhase(phase.Retro), role: claim.RoleMerger,
			gate: closedSpecGate(root),
		},
		{
			name: "the merger of an issue held", issue: inPhase(phase.Held), role: claim.RoleMerger,
			gate: closedSpecGate(root),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if opens := tc.gate.Open(); opens != tc.treeOpens {
				t.Fatalf("the fixture's record.TreeGate.Open() = %t, want %t", opens, tc.treeOpens)
			}
			gate, held := HeldByDesignGate(tc.issue, tc.role, tc.gate)
			if held != tc.held {
				t.Fatalf("HeldByDesignGate = %t, want %t", held, tc.held)
			}
			if !held && gate != (record.DesignGate{}) {
				t.Errorf("HeldByDesignGate answered the gate %+v for a role it does not hold, want none", gate)
			}
			if held && (gate.Step != record.DesignStepSpec || gate.ArtifactID != "spec-artifact" || gate.LatestVersion != 2) {
				t.Errorf("HeldByDesignGate answered the gate %+v, want the unapproved spec row: its step, its document and its version", gate)
			}
		})
	}
}
