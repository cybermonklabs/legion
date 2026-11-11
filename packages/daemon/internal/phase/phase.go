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
	AwaitingMerge  Phase = "awaiting_merge"
	Done           Phase = "done"
	Held           Phase = "held"
)

// All is every phase, in the transition table's order.
var All = []Phase{
	Admitted, AwaitingPerson, Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging,
	AwaitingMerge, Done, Held,
}

// Order is the line an issue walks forward, and the only phases a move back is read against. It
// differs from All: All is every phase the daemon writes, admitted, awaiting_person, done and
// held among them, while Order holds the eight working phases alone, in the order the workflow
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
var Order = []Phase{Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging, AwaitingMerge}

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
// two: MovesBack is true for pairs such as merging-awaiting_merge that no backward row and no
// count know. Phase words hold "_" and never "-", so the one "-" of a pair in the set splits it.
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

// backwardPairs is built once, in the order the workflow's backward rows are generated: each of
// the first seven phases of Order paired with every phase before it, 21 pairs, and then the one
// move back AwaitingMerge makes, which the workflow table carries as a row of its own.
var backwardPairs = buildBackwardPairs()

// backwardPairSet is backwardPairs keyed for the lookup ParsePair makes.
var backwardPairSet = buildBackwardPairSet()

func buildBackwardPairs() []string {
	// AwaitingMerge is last in Order and moves back to Implementing alone: an issue whose pull
	// request waits on the merge queue goes back to the work, not to a phase it already passed.
	forward := Order[:len(Order)-1]
	pairs := make([]string, 0, len(forward)*(len(forward)-1)/2+1)
	for fromIndex, from := range forward {
		for _, to := range forward[:fromIndex] {
			pairs = append(pairs, Pair(from, to))
		}
	}
	return append(pairs, Pair(AwaitingMerge, Implementing))
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
// role the workflow starts on the phase (workflow.RoleFor, whose tests tie the two). Retro and the
// merger's READY write none, and report the commit they stand on; accepting is the architect's
// sign-off, no handoff at all.
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
