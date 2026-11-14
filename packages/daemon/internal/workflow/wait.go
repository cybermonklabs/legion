package workflow

import (
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// MinWaitAhead and MaxWaitAhead are the bounds a declared wait's time must fall within, measured
// from its declaration: less than a minute ahead is too soon, and more than a day ahead too far
// (dispatch://INFRA-2040 R2, D10). They are the daemon's own, the same on every deployment, so a
// worker with a wrong wait, or a message whose wake is lost, goes unseen for a day at most.
const (
	MinWaitAhead = time.Minute
	MaxWaitAhead = 24 * time.Hour
)

// The causes WaitEndCause names, one for each term of WaitStands a wait with no end can fail, and
// the one each such end is written with (record.ClaimWait.EndCause). A phase that moved names
// where it moved to (WaitEndPhaseMoved, WaitEndEntryMoved).
const (
	// WaitEndCompleted: the role's phases row records its completion of the phase.
	WaitEndCompleted = "completed"
	// WaitEndAnotherRolesPhase: the phase the issue stands in is not the wait's role's to work.
	WaitEndAnotherRolesPhase = "another role's phase"
	// WaitEndLeftWorkflow: the issue is out of the workflow (record.InWorkflow).
	WaitEndLeftWorkflow = "left the workflow"
	// WaitEndTreeLingers: the issue's tree root lingers after its close.
	WaitEndTreeLingers = "tree lingers"
	// WaitEndNewGeneration: the issue runs a generation the wait was not declared in.
	WaitEndNewGeneration = "new generation"
	// WaitEndFreshSession: the claim runs a session the wait was not declared in.
	WaitEndFreshSession = "fresh session"
	// WaitEndDesignGateClosed: the wait is a merger's, and the design gate holds it
	// (HeldByDesignGate).
	WaitEndDesignGateClosed = "design gate closed"
)

// WaitEndPhaseMoved is the cause of a wait whose issue moved from the wait's phase to another,
// naming the phase it moved to: a completion that moved it, a backward move, a move to held.
func WaitEndPhaseMoved(to phase.Phase) string { return "phase moved to " + string(to) }

// WaitEndEntryMoved is the cause of a wait whose issue stands in the wait's phase on a new entry
// of the role's phases row (record.PhaseRow.Entry), naming that entry: the phase was started
// again for the role, which hands it work it had not taken when it declared.
func WaitEndEntryMoved(entry int64) string { return fmt.Sprintf("phase entry moved to %d", entry) }

// WaitStands is whether the claim's wait stands now, and that wait when it does (the zero wait
// when it does not). It is the one rule every reader of a wait asks, so that what a wait does
// never depends on its end being written: a wait whose phase moved, whose session was replaced or
// whose merger the design gate holds stops counting the moment the fact that did it commits, as
// a task stops being worked the moment its phase moves (supervise.Delivery.StillWorked). The end
// such a wait is given is written with the cause WaitEndCause names (record.Store.EndWait), and
// this rule does not wait for it.
//
// issue is the claim's issue as recorded now, root its tree root, row the wait's role's phases
// row (record.PhaseRowOf), run the claim's run, of which only the claim is read (its session and
// its wait with no end, record.RoleClaim's Session and Wait), and gate the tree's design gate
// with the steps the deployment gates. It is true only while every one of these holds, in the
// order WaitEndCause asks them:
//   - the claim has a wait row with no end;
//   - the role's phases row records no completion (record.PhaseRow.CompletedAt);
//   - the issue stands in the wait's phase;
//   - that phase is the wait's role's to work (OpenPhaseRole);
//   - the issue is in the workflow (record.InWorkflow), and its tree root does not linger;
//   - the issue runs the generation the wait was declared in, and the role's phases row is on
//     the entry it was on at the declaration;
//   - the claim runs the session the wait was declared in, since a fresh session knows nothing
//     of the wait;
//   - the wait is not a merger's the design gate holds (HeldByDesignGate).
func WaitStands(issue, root record.Issue, row record.PhaseRow, run record.RoleRun, gate record.TreeGate) (record.ClaimWait, bool) {
	wait := unendedWait(run)
	if wait == nil || WaitEndCause(issue, root, row, run, gate) != "" {
		return record.ClaimWait{}, false
	}
	return *wait, true
}

// WaitEndCause is the cause that ends the claim's wait now: the first term of WaitStands that
// fails, asked in one fixed order — completed, the phase moved (and to what), another role's
// phase, left the workflow, the tree lingers, a new generation, a new entry of the same phase, a
// fresh session, and last the design gate holding a merger — and "" while the wait stands. A claim
// with no wait row that has no end has no wait to end, and answers "" too: WaitStands is the
// question of whether a wait stands, and this one is asked of a wait known to have no end.
//
// The order names what the worker did before what was done around it, and the phase before the
// tree: a completion that also moved the issue is a completion, a move into done is a move of the
// phase, and a re-admission that also moved the phase is the phase's move. The cause is what such
// a wait's end is written with (record.Store.EndWait), so one fact ends one wait with one cause
// whichever of its terms it broke.
func WaitEndCause(issue, root record.Issue, row record.PhaseRow, run record.RoleRun, gate record.TreeGate) string {
	wait := unendedWait(run)
	if wait == nil {
		return ""
	}
	switch working := OpenPhaseRole(issue.Phase); {
	case !row.CompletedAt.IsZero():
		return WaitEndCompleted
	case issue.Phase != wait.Phase:
		return WaitEndPhaseMoved(issue.Phase)
	case working == "" || working != wait.Role:
		return WaitEndAnotherRolesPhase
	case !record.InWorkflow(issue):
		return WaitEndLeftWorkflow
	case root.Lingers():
		return WaitEndTreeLingers
	case issue.Generation != wait.Generation:
		return WaitEndNewGeneration
	case row.Entry != wait.PhaseEntry:
		return WaitEndEntryMoved(row.Entry)
	case run.Claim.Session != wait.Session:
		return WaitEndFreshSession
	}
	if _, held := HeldByDesignGate(issue, wait.Role, gate); held {
		return WaitEndDesignGateClosed
	}
	return ""
}

// unendedWait is the wait row of run's claim that has no end, nil for a run with no claim and a
// claim with none.
func unendedWait(run record.RoleRun) *record.ClaimWait {
	if run.Claim == nil || run.Claim.Wait == nil || !run.Claim.Wait.EndedAt.IsZero() {
		return nil
	}
	return run.Claim.Wait
}
