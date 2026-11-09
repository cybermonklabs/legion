// The stranded-claim half of this package's state: the claims nothing will wake, who suspended
// them, and the move that brings each back. It moved out of state.go when that file crossed a
// thousand lines (INFRA-1077, review round 14); it reads none of the rest of the state and
// nothing of it reads this, so the two are a file each.

package api

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// StrandedClaim is one claim nothing will wake — a worker of its issue's phase, or its tree's
// architect, in a tree that runs — named by its issue, its role and its state; the issue's own
// entry under `issues` carries the rest. Its state, its role and who suspended it say the move
// that brings it back (Move), and that suspender is no field of it: every plugin parses this
// body with the schema of the contract it was built at, and a session whose plugin predates
// INFRA-1048 refuses the whole state over a key it does not know — a daemon that relaunches no
// stale session (spec R10) would break every such pod the moment a claim is stranded. A plugin
// built after it strips the key instead, which loses the field rather than the state, and the
// drift gate in packages/contracts names it at the next golden write. It is served on the
// operator claim routes, which no plugin calls (OperatorClaim.SuspendedBy), so a reader hands
// Move the suspender it holds: the daemon's own boot the claim's (SuspendersOf), `legion state`
// the operator claim list's.
type StrandedClaim struct {
	Issue string     `json:"issue"`
	Role  claim.Role `json:"role"`
	State string     `json:"state"`
}

// Suspenders is who suspended each claim, by the key its issue and role make: the fact a
// stranded claim's move turns on, which the state body does not carry.
type Suspenders map[string]string

// SuspendersOf is who suspended each suspended claim of stranded, as the daemon's own boot reads
// it from the claims themselves. A failed claim carries no suspension.
func SuspendersOf(stranded []supervise.Claim) Suspenders {
	by := make(Suspenders, len(stranded))
	for _, c := range stranded {
		if c.State == supervise.StateSuspended {
			by[strandedKey(c.Issue, c.Role)] = string(c.SuspendedBy)
		}
	}
	return by
}

// SuspendersOfOperatorClaims is who suspended each claim of the operator claim list, the one
// surface that serves it (OperatorClaim.SuspendedBy): how a reader off the daemon, `legion
// state`, names the move of a stranded claim.
func SuspendersOfOperatorClaims(claims []OperatorClaim) Suspenders {
	by := make(Suspenders, len(claims))
	for _, c := range claims {
		by[strandedKey(c.Issue, c.Role)] = c.SuspendedBy
	}
	return by
}

// Of is who suspended c, "" when nobody did or when this lookup does not name it.
func (s Suspenders) Of(c StrandedClaim) string {
	return s[strandedKey(c.Issue, c.Role)]
}

// strandedKey names one claim of a tree in both lookups: an issue runs one claim of a role.
func strandedKey(issue string, role claim.Role) string {
	return issue + " " + string(role)
}

// StrandedMove is the move that brings a stranded claim back (StrandedClaim.Move). Its value is the
// key the boot's `claims nothing will wake` log line lists the claim under.
type StrandedMove string

const (
	StrandedResume           StrandedMove = "resume"
	StrandedSuspendedWorkers StrandedMove = "suspendedWorkers"
	StrandedReleasedNoResume StrandedMove = "releasedNoResume"
	StrandedRetry            StrandedMove = "retry"
	StrandedWorkflowStart    StrandedMove = "needsWorkflowStart"
)

// StrandedMoves is every move, in the order the boot's log line and `legion state` list them.
func StrandedMoves() []StrandedMove {
	return []StrandedMove{StrandedResume, StrandedSuspendedWorkers, StrandedReleasedNoResume, StrandedRetry, StrandedWorkflowStart}
}

// Move is the move that brings c back, given who suspended it (Suspenders.Of, "" for a claim
// nobody suspended or whose suspender the caller cannot read): the one statement of it that the
// boot's log line and `legion state` both group by:
//
//   - StrandedReleasedNoResume, a claim whose idle pod the daemon released and whose wake never
//     brought it back (workflow.Stranded's ten-minute rule): the pod and its disk are gone, so
//     the move is the operator's reading of why the replacement pod never took a turn, not a
//     resume of the claim — a released claim resumes on its next delivery, and the wake that
//     should have resumed it has already been taken.
//   - StrandedResume, a suspended architect: the operator's resume (`legion claims resume`), which
//     a catch-up follows.
//   - StrandedSuspendedWorkers, a suspended phase worker: the move its claim's suspendedBy calls
//     for (the operator claim view): the boot's start once it is marked the daemon's when it was
//     suspended before 0038, the operator's resume when an operator suspended it and it kept its
//     task or its phase start waits, otherwise the operator's delivery of its phase's resume task.
//   - StrandedRetry, a failed architect: the operator's retry (`legion claims retry`), which
//     relaunches its session with fresh budgets, and a catch-up follows.
//   - StrandedWorkflowStart, a failed phase worker: the retry of its held phase — its architect's
//     retry_or_escalate, or the operator's retry (`legion claims retry`, which applies the same
//     retry) — since a failed claim ignores resume and deliver.
//   - StrandedWorkflowStart, a retired claim, whatever its role: only a workflow start relaunches
//     one, since the operator's retry refuses a claim that has not failed and a retired claim
//     ignores resume and deliver. A start of its phase sends it the retry the daemon's own start
//     executor makes of a retired claim, so the move is the act that queues such a start: the
//     architect's retry_or_escalate of a held phase, a transition into the phase, or the tree's
//     close and re-admission for an architect, whose tree's admission is the only start of it.
//
// The retired case is read before every other, because a claim released and then retired keeps
// `suspendedBy: released` on its row and SuspendersOfOperatorClaims copies that value whatever the
// state: read as released, it would name a pod coming back from a wake, and nothing is coming
// back. For the same reason the released case asks for the suspended state itself rather than for
// anything that has not failed.
func (c StrandedClaim) Move(suspendedBy string) StrandedMove {
	state := supervise.ClaimState(c.State)
	switch {
	case state == supervise.StateRetired:
		return StrandedWorkflowStart
	case state == supervise.StateSuspended && suspendedBy == string(supervise.SuspendedByReleased):
		return StrandedReleasedNoResume
	case state == supervise.StateFailed && c.Role == claim.RoleArchitect:
		return StrandedRetry
	case state == supervise.StateFailed:
		return StrandedWorkflowStart
	case c.Role == claim.RoleArchitect:
		return StrandedResume
	default:
		return StrandedSuspendedWorkers
	}
}

// StrandedClaimsOf is stranded as the state shows it, claim for claim.
func StrandedClaimsOf(stranded []supervise.Claim) []StrandedClaim {
	claims := make([]StrandedClaim, len(stranded))
	for i, c := range stranded {
		claims[i] = StrandedClaim{Issue: c.Issue, Role: c.Role, State: string(c.State)}
	}
	return claims
}

// StrandedByMove is each stranded claim named `<issue> <role>`, grouped by its Move under
// suspended, each group in stranded's order; every move has a group, empty when no claim takes
// it.
func StrandedByMove(stranded []StrandedClaim, suspended Suspenders) map[StrandedMove][]string {
	grouped := make(map[StrandedMove][]string, len(StrandedMoves()))
	for _, move := range StrandedMoves() {
		grouped[move] = []string{}
	}
	for _, c := range stranded {
		move := c.Move(suspended.Of(c))
		grouped[move] = append(grouped[move], strandedKey(c.Issue, c.Role))
	}
	return grouped
}
