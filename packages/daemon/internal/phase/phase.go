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
	// person's comments; the person's answer puts the root in the waiting line as well, within its
	// priority, among the issues whose work came back, oldest first, and the pass that reads it delivers the
	// architect its task, taking no further slot.
	AwaitingPerson Phase = "awaiting_person"
	Planning       Phase = "planning"
	Implementing   Phase = "implementing"
	Testing        Phase = "testing"
	Reviewing      Phase = "reviewing"
	// Step is a step of a role the deployment or the issue's plan defines: none of Legion's own roles works it, and
	// the role that does is the one the issue's plan names for the step it stands at, so RoleFor
	// names none.
	Step      Phase = "step"
	Accepting Phase = "accepting"
	Retro     Phase = "retro"
	Merging   Phase = "merging"
	Done      Phase = "done"
	Held      Phase = "held"
)

// All is every phase, in the transition table's order.
var All = []Phase{
	Admitted, AwaitingPerson, Planning, Implementing, Testing, Reviewing, Step, Accepting, Retro,
	Merging, Done, Held,
}

// Order is the line an issue of the default steps walks forward, and the phases a move back is
// read against for an issue with no plan rows; a task's own line reads its own moves back
// (workflow's Walk.MovesBack). It differs from All: All is every phase the daemon writes, admitted,
// awaiting_person, done and held among them, while Order holds the seven working phases alone, in
// the order of the default steps. A phase outside Order has no place in that line, so no move into it and no move
// out of it is a move back: an issue put on held, or closed as done, went nowhere backward, and
// MovesBack answers false for it.
//
// Both internal/config and internal/workflow read this set. The loader takes a hook's move as a
// pair of MovePairs (all 42 ordered pairs of the seven, and the 15 pairs of step) that some task's
// line makes as a move back (plan.MovesBackPairs, 41 of them), since it reads the file with no
// plan, and refuses the other 16, which would never fire. The engine counts the pair of each
// move back the task's own line reads (Walk.MovesBack), and a pass. It
// lives here, not in internal/workflow, because internal/config cannot import internal/workflow.
// Measured with
// `go list`: every test file of package workflow is `package workflow`, and 42 of its 57 test
// files import internal/config (`go list -f '{{.TestImports}}' ./internal/workflow/` names
// internal/config), so an edge from internal/config to internal/workflow would close a cycle in
// the workflow test binary. The edge this package needs is already there and stays small:
// `go list -deps ./internal/config/` names internal/phase, and `go list -deps ./internal/phase/`
// names one package of this repository, internal/claim.
var Order = []Phase{Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging}

// MovesBack says whether to is a phase of today's line, Order, before from. Both must stand in
// Order. A phase outside it, step among them, is no part of the forward line, so this answers no
// move into or out of it. A task in another order moves back by its own (workflow's
// Walk.MovesBack).
func MovesBack(from, to Phase) bool {
	toIndex := slices.Index(Order, to)
	return toIndex >= 0 && toIndex < slices.Index(Order, from)
}

// BackwardPairs is every move back of today's line, Order, each written "<from>-<to>": each
// phase of Order paired with every phase before it, 21 pairs. They are the first part of
// MovePairs, in this order, and the backward rows of a line in today's order (workflow's
// backwardRows). The answer is a fresh slice, so a caller that sorts or trims it leaves the next
// caller's answer whole.
func BackwardPairs() []string {
	return slices.Clone(backwardPairs)
}

// MovePairs is every move the daemon counts, each written "<from>-<to>": BackwardPairs first, in
// their order, then the 21 forward pairs, each phase of Order paired with every phase after it,
// then the pairs of step: step to each phase of Order, each phase of Order to step, and step-step.
// A task in another order than today's moves back by its own order (workflow's Walk.MovesBack),
// so the move back of one task, testing to reviewing, is a forward pair of Order. A step of a role
// the deployment defines runs as step wherever it stands in its task, so a move back may leave or
// reach step, and one from such a step to an earlier one is step-step, the one pair of a phase
// with itself. It is the whole vocabulary the readers of a move share: the engine counts a move
// back under its pair, the config loader refuses a hook naming a pair outside it, and the outbox
// refuses a hook prompt of a move outside it (record.HookPrompt). The answer is a fresh slice.
func MovePairs() []string {
	return slices.Clone(movePairs)
}

// IsMovePair says whether from-to is a pair MovePairs holds: two different phases of Order and
// step, or step-step.
func IsMovePair(from, to Phase) bool {
	return movePairSet[Pair(from, to)]
}

// ParsePair reads one "<from>-<to>" pair and answers ok only for a pair MovePairs holds: two
// different phases of Order and step, or step-step. Phase words hold "_" and never "-", so the
// one "-" of a pair in the set splits it.
func ParsePair(text string) (from, to Phase, ok bool) {
	if !movePairSet[text] {
		return "", "", false
	}
	fromText, toText, _ := strings.Cut(text, "-")
	return Phase(fromText), Phase(toText), true
}

// Pair spells one move: "<from>-<to>". It is the one spelling the counts of
// record.PhaseRow.BackwardMoves stand under, the words MovePairs answers, and the word a hook's
// `moves` names a move by (config's readHookMoves). The three are one vocabulary, so the spelling
// is written here and nowhere else.
func Pair(from, to Phase) string {
	return string(from) + "-" + string(to)
}

// backwardPairs is built once, in the order the backward rows of today's line are generated: each
// phase of Order paired with every phase before it, 21 pairs. There is no pair beyond the pairs
// of Order, since the merger merges the head it read and the daemon closes the issue from the
// merged pull request: no phase of this workflow waits for a merge somebody else makes
// (dispatch://INFRA-714).
var backwardPairs = buildBackwardPairs()

// movePairs is built once: backwardPairs, then each phase of Order paired with every phase after
// it, then step to each phase of Order, each phase of Order to step, and step-step, 57 pairs.
var movePairs = buildMovePairs()

// movePairSet is movePairs keyed for the lookups ParsePair and IsMovePair make.
var movePairSet = buildMovePairSet()

func buildBackwardPairs() []string {
	pairs := make([]string, 0, len(Order)*(len(Order)-1)/2)
	for fromIndex, from := range Order {
		for _, to := range Order[:fromIndex] {
			pairs = append(pairs, Pair(from, to))
		}
	}
	return pairs
}

func buildMovePairs() []string {
	pairs := make([]string, 0, len(Order)*(len(Order)-1)+2*len(Order)+1)
	pairs = append(pairs, backwardPairs...)
	for fromIndex, from := range Order {
		for _, to := range Order[fromIndex+1:] {
			pairs = append(pairs, Pair(from, to))
		}
	}
	for _, to := range Order {
		pairs = append(pairs, Pair(Step, to))
	}
	for _, from := range Order {
		pairs = append(pairs, Pair(from, Step))
	}
	return append(pairs, Pair(Step, Step))
}

func buildMovePairSet() map[string]bool {
	set := make(map[string]bool, len(movePairs))
	for _, text := range movePairs {
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
// handoff at all; and step, whose role the deployment or the issue's plan defines, writes none either.
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
// awaiting_person, done, held — has none, and neither has step, whose role is the one the issue's
// plan names rather than one of the phase's. Admitted and accepting are the architect's, and
// neither phase starts a worker. On a root both are its own architect's. On a child both are the
// architect that owns it: its own while that claim holds its role, else the one above. At
// admitted that is the one above, since the child's own architect has not started yet or a leave
// suspended or retired it, unless a person set the child back to todo while it ran or was held:
// that re-entry starts and stops nothing, so a child's own architect that held its role still
// holds it (workflow's ReenterChild). Every site that starts, resumes or reports a phase's worker
// skips the architect, so it skips both phases (workflow's start, admit's startMidPhaseMembers,
// workflow's booted and Stranded).
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
	case Admitted, Accepting:
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
// it stands past it. A role the deployment or the issue's plan defines works step alone, so step is the phase it has
// left wherever the issue stands. A completion that arrives after the issue moved on without its
// role is of this phase (Of). The architect works admitted and accepting, signs accepting off and
// completes nothing, so it leaves no phase here, and neither does a name that is no role.
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
	if claim.IsDeploymentRole(role) {
		return Step
	}
	return ""
}

// Of is the phase role's completion is of while the issue stands at at: at when role works it
// (RoleFor), and otherwise the phase role has left (LeftBy). The workflow records a completion on
// this phase (workflow's leftItsPhase) and the completion route reads its handoff record against
// it (api's handoffComplete), so the two read one answer. It is "" for the architect anywhere but
// admitted and accepting, the two phases it works, since the architect leaves no phase. At
// admitted that answer is the issue's phase, the one the route read for the architect's
// completion while RoleFor named no role there, and the workflow refuses an architect's
// completion before it reads this at all.
func Of(role claim.Role, at Phase) Phase {
	if RoleFor(at) == role {
		return at
	}
	return LeftBy(role, at)
}
