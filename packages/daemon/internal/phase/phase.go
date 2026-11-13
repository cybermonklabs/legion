// Package phase owns the issue workflow phase vocabulary.
package phase

import (
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Phase is the issue's position in the daemon's transition table — the state it sits in, not the
// role working it (`workers` is keyed by role). The daemon advances it from facts it observes; no
// agent chooses it. The order is the transition table's own.
type Phase string

const (
	Admitted Phase = "admitted"
	// AwaitingPerson is a root admitted tree that waits only on a person: its architect asked a
	// question and handed it to the person. It keeps the admission slot it took when the daemon
	// started it, and its architect stays live in its own session, reading and answering the
	// person's comments; the person's answer puts the root in the waiting line as well, among the
	// issues whose work came back, oldest first, and the pass that reads it delivers the
	// architect its task, taking no further slot.
	AwaitingPerson Phase = "awaiting_person"
	Planning       Phase = "planning"
	Implementing   Phase = "implementing"
	Testing        Phase = "testing"
	Reviewing      Phase = "reviewing"
	Accepting      Phase = "accepting"
	Retro          Phase = "retro"
	Merging        Phase = "merging"
	Done           Phase = "done"
	Held           Phase = "held"
)

// All is every phase, in the transition table's order.
var All = []Phase{
	Admitted, AwaitingPerson, Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging,
	Done, Held,
}

// Order is the line an issue walks forward, and the only phases a move back is read against. It
// differs from All: All is every phase the daemon writes, admitted, awaiting_person, done and
// held among them, while Order holds the seven working phases alone, in the order the workflow
// advances them. A phase outside Order has no place in that line, so no move into it and no move
// out of it is a move back: an issue put on held, or closed as done, went nowhere backward, and
// MovesBack answers false for it.
//
// Both internal/config and internal/workflow read this set: the loader refuses a hook written for
// a move pair the daemon never counts, and the engine counts the rounds. It lives here, not in
// internal/workflow, because internal/config cannot import internal/workflow. Measured with
// `go list`: every test file of package workflow is `package workflow`, and 42 of its 57 test
// files import internal/config (`go list -f '{{.TestImports}}' ./internal/workflow/` names
// internal/config), so an edge from internal/config to internal/workflow would close a cycle in
// the workflow test binary. The edge this package needs is already there and stays small:
// `go list -deps ./internal/config/` names internal/phase, and `go list -deps ./internal/phase/`
// names one package of this repository, internal/claim.
var Order = []Phase{Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging}

// MovesBack says whether to is a workflow phase before from. Both must stand in Order: a phase
// outside it is no part of the forward line, so nothing moves back into or out of it.
func MovesBack(from, to Phase) bool {
	toIndex := slices.Index(Order, to)
	return toIndex >= 0 && toIndex < slices.Index(Order, from)
}

// BackwardPairs is every move back the daemon counts, each written "<from>-<to>". It is the whole
// vocabulary the two readers named on Order share: the engine counts a round for a pair in this
// list, and the config loader refuses a hook naming a pair that is not in it. The answer is a
// fresh slice, so a caller that sorts or trims it leaves the next caller's answer whole.
func BackwardPairs() []string {
	return slices.Clone(backwardPairs)
}

// ParsePair reads one "<from>-<to>" pair and answers ok only for a pair BackwardPairs holds. The
// check is against that set, not against MovesBack alone, because the set is the smaller of the
// two: MovesBack answers for the whole triangle over Order, where a row and a count know only the
// pairs the workflow carries. Phase words hold "_" and never "-", so the one "-" of a pair in the
// set splits it.
func ParsePair(text string) (from, to Phase, ok bool) {
	if !backwardPairSet[text] {
		return "", "", false
	}
	fromText, toText, _ := strings.Cut(text, "-")
	return Phase(fromText), Phase(toText), true
}

// Pair spells one move back: "<from>-<to>". It is the one spelling the counts of
// record.PhaseRow.BackwardMoves stand under, the words BackwardPairs answers, and the word a
// hook's `moves` names a move by (config's readHookMoves). The three are one vocabulary, so the
// spelling is written here and nowhere else.
func Pair(from, to Phase) string {
	return string(from) + "-" + string(to)
}

// backwardPairs is built once, in the order the workflow's backward rows are generated: each
// phase of Order paired with every phase before it, 21 pairs. There is no pair beyond them,
// since the merger merges the head it read and the daemon closes the issue from the merged pull
// request: no phase of this workflow waits for a merge somebody else makes
// (dispatch://INFRA-714).
var backwardPairs = buildBackwardPairs()

// backwardPairSet is backwardPairs keyed for the lookup ParsePair makes.
var backwardPairSet = buildBackwardPairSet()

func buildBackwardPairs() []string {
	pairs := make([]string, 0, len(Order)*(len(Order)-1)/2)
	for fromIndex, from := range Order {
		for _, to := range Order[:fromIndex] {
			pairs = append(pairs, Pair(from, to))
		}
	}
	return pairs
}

func buildBackwardPairSet() map[string]bool {
	set := make(map[string]bool, len(backwardPairs))
	for _, text := range backwardPairs {
		set[text] = true
	}
	return set
}

// Handoff is the word of the Dispatch handoff record a phase ends with, the role that writes it,
// and whether the phase ends with one: the planner's, the implementer's implementing rounds, the
// tester's, and the reviewer's, each under the word its role's prompt spells
// (internal/prompts/roles/*.md), the phase of the record whose newest version the `legion` tool's
// `handoff_complete` reads and names (packages/pi-shared/src/handoff-record.ts). The writer is the
// role the workflow starts on the phase (RoleFor, whose tests tie the two). Retro and merging
// write none, and report the commit they stand on; accepting is the architect's sign-off, no
// handoff at all.
func Handoff(p Phase) (word string, writer claim.Role, ok bool) {
	switch p {
	case Planning:
		return "plan", claim.RolePlanner, true
	case Implementing:
		return "implement", claim.RoleImplementer, true
	case Testing:
		return "test", claim.RoleTester, true
	case Reviewing:
		return "review", claim.RoleReviewer, true
	default:
		return "", "", false
	}
}

// RoleFor is the role that works a phase: the one a transition starts, and the one whose handoff
// record the `legion` tool's `handoff_complete` reads for a phase with a handoff (Handoff, whose
// writer a test holds to this; packages/pi-shared/src/handoff-record.ts). A phase no role works —
// awaiting_person, done, held — has none. Accepting is the tree architect's, a
// claim per tree that no row starts: every site that starts, resumes or reports a phase's worker
// skips it (workflow's start, admit's startMidPhaseMembers, workflow's booted and Stranded).
//
// It stands in this package, with the phase vocabulary it reads, rather than in internal/workflow,
// which starts the roles: the state projection names the role each issue's entry numbers are of
// (projection.Project), admission starts the mid-phase children of a tree it re-admits on each
// child's own phase's role, and `legion state` reads the same role back for its line
// (cmd/legion's entriesBehind). internal/workflow's own tests import internal/projection, so the
// projection cannot reach a workflow function at all.
func RoleFor(p Phase) claim.Role {
	switch p {
	case Planning:
		return claim.RolePlanner
	case Implementing, Retro:
		return claim.RoleImplementer
	case Testing:
		return claim.RoleTester
	case Reviewing:
		return claim.RoleReviewer
	case Accepting:
		return claim.RoleArchitect
	case Merging:
		return claim.RoleMerger
	default:
		return ""
	}
}

// LeftBy is the phase role has left when the issue, standing at at, no longer stands in a phase
// that role works: RoleFor read the other way round, the one phase each role works, and, for the
// implementer, which works two, implementing while the issue stands before retro and retro once
// it stands past it. A completion that arrives after the issue moved on without its role is of
// this phase, which the workflow records it on (workflow's leftItsPhase) and whose handoff record
// the completion route reads it against (api's handoffComplete). The architect works accepting
// with its sign-off and completes nothing, so it leaves no phase here.
func LeftBy(role claim.Role, at Phase) Phase {
	switch role {
	case claim.RolePlanner:
		return Planning
	case claim.RoleTester:
		return Testing
	case claim.RoleReviewer:
		return Reviewing
	case claim.RoleMerger:
		return Merging
	case claim.RoleImplementer:
		if MovesBack(at, Retro) {
			return Retro
		}
		return Implementing
	}
	return ""
}
