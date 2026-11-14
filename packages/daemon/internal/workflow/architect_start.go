package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// architectStartTail ends every task ArchitectStartTask writes, and nothing else writes it. It is
// how a reader of a task the daemon holds tells one that carries that instruction from one that
// carries only what its tree is owed (CarriesArchitectStart), which is what keeps the instruction
// through the narrowing a task's decisions go through when one of them leaves the tree
// (NarrowRootTask). Every shape ends with it, so that reading answers for all four.
const architectStartTail = " Nothing else starts this tree, and no message is coming."

// Each shape of the start task carries one label of its own, as testerVerdictLabel ties a task's
// text to its reader: a test reads the shape a root was given by the label its text carries. No
// label is part of another, so a task carries the label of its own shape and of no other.
const (
	// ArchitectFreshTreeLabel is in the task of a tree nobody has architected.
	ArchitectFreshTreeLabel = "its tree is open at generation"
	// ArchitectCarriedPhaseLabel is in the task of a root a hand-over carried in.
	ArchitectCarriedPhaseLabel = "the phase a hand-over carried it in at"
	// ArchitectGateRowsLabel is in the task of a root that already holds design gate rows.
	ArchitectGateRowsLabel = "its design gate already stands part way"
	// ArchitectReadmittedLabel is in the task of a tree that ran at an earlier generation.
	ArchitectReadmittedLabel = "ran at an earlier generation"
	// ArchitectDecompositionLabel is the sentence that asks for a decomposition. Only a tree
	// nobody has architected is asked for one, so this label stands in that shape alone.
	ArchitectDecompositionLabel = "every child of your decomposition filed"
)

// ArchitectStart is everything the first task of a root architect is derived from, which is the
// record and nothing else. The caller reads these five values and the derivation chooses the
// shape, so admission and the narrowing of a held task cannot disagree about what a root is told.
type ArchitectStart struct {
	// Root is the tree's root as the record holds it: its key, its tree, the generation the tree
	// is open at, the phase it stands in and whether its run has stood in a phase of its own
	// (record.Issue.TakenBack).
	Root record.Issue
	// Carried says a hand-over carried the root in at the phase it stands in. Admission reads it
	// from the mark on the issue (record.Issue.HandedOverFrom) before the write that opens the
	// tree clears it, and every reader after that write reads the hand-over row of the root's
	// generation instead (ArchitectCarried): the sweep's start and the narrowing of a held task.
	Carried bool
	// Gate is the steps the deployment gates and the design gate rows the root holds now
	// (record.Store.GatesOf). The zero value is a root that has registered nothing.
	Gate record.TreeGate
	// Launch is the generation the tree's architect claim stands at
	// (record.Store.ArchitectLaunches), zero where that tree has no architect claim.
	Launch uint64
	// Decided is what a person decided on the tree while nobody in it could hear them, as
	// ParkedDecisionTask composes it, empty where there is nothing.
	Decided string
}

// ArchitectStartTask is what a root architect is given with the start its admission queues: the
// one thing that tells a tree's architect, in the turn its own launch starts, that the tree is its
// to take up and what the first act is.
//
// A phase worker's start carries its phase's task, so a worker the daemon launches has work the
// moment it is ready. A root architect's start carried a task only where a person had decided
// something on its tree while nobody in that tree could hear it (ParkedDecisionTask), which is
// rare: the common fresh admission launched an architect with nothing at all, and the first thing
// the daemon ever gave it was the catch-up its ready writes. That catch-up states the tree and
// ends "start or resume it as your role says", which leaves the first act to the agent to infer
// from a context it has just been born into — and an architect that infers nothing holds its tree's
// admission slot while it sits idle, with the root unclaimed and its design gate unregistered,
// until a person messages the session. So the start says it instead, and the catch-up goes on
// stating the tree.
//
// The derivation serves two roots: one standing at phase.Admitted, which is where every tree the
// workflow opens stands, and one a hand-over carried in at the phase that came with it. It gives
// every other root nothing. Which of four shapes a root it serves gets is read here rather than
// by each caller. The first rule that holds wins:
//
//  1. The hand-over mark stands and the root is past phase.Admitted. A hand-over carried it in
//     with that phase and the design gate it brought, so it is told to take that phase up and
//     that it decomposes nothing (internal/prompts/roles/architect-root.md, "decompose nothing
//     and release no child"). It is asked for no spec.
//  2. The root is past phase.Admitted under its own power. It stands in a working phase, waits on
//     a person, or stands in accepting, and its work is told by that phase's own role, by the
//     resume the daemon composes when a person answers, or by the notice that moved it. A
//     first-act statement would be false of it, so this writes none: the answer is Decided alone,
//     which is the empty string where a person has decided nothing, and which carries no tail, so
//     CarriesArchitectStart answers false for it.
//  3. The root holds design gate rows. Its gate stands part way, so the task reports it step by
//     step and the architect carries it on from there.
//  4. The tree's architect claim stands below the root's generation, or the root's run has stood
//     in a phase of its own (record.Issue.TakenBack). The tree ran at an earlier generation, and
//     re-admission cleared that generation and its design gate with it, so the spec is registered
//     again and nothing is decomposed anew.
//  5. Otherwise nobody has architected this tree, and the task asks for the whole first act.
//
// A root a hand-over carried in that stands at phase.Admitted falls past rule 1 to rules 3, 4 and
// 5. That is right: a child handed over before it ever started has no phase of its own to take up,
// and it writes its own spec like any tree nobody has architected.
//
// One reading of rule 4 is worth stating, because it is the one that is easy to get wrong. A tree
// whose earlier generation ended with no phase of its own leaves no TakenBack mark, and its
// architect claim can already stand at the root's own generation, which the launch of this very
// admission puts it at. Such a tree falls through to rule 5, and that is right: the first act is
// the act that tree still owes, since nothing of it has been claimed, decomposed or registered.
//
// Decided leads every shape rather than replacing it, since a person's decision is read first and
// a tree that owes one still has to be taken up.
func ArchitectStartTask(start ArchitectStart) string {
	if start.Root.Phase != phase.Admitted && !start.Carried {
		return start.Decided
	}
	var task strings.Builder
	if start.Decided != "" {
		task.WriteString(start.Decided)
		task.WriteString("\n\n")
	}
	switch {
	case start.Root.Phase != phase.Admitted:
		writeCarriedPhase(&task, start)
	case len(start.Gate.Rows) > 0:
		writeGateStanding(&task, start)
	case start.Launch > 0 && start.Launch < start.Root.Generation, start.Root.TakenBack:
		writeEarlierGeneration(&task, start)
	default:
		writeFreshTree(&task, start)
	}
	task.WriteString(architectStartTail)
	return task.String()
}

// writeFreshTree writes the task of a tree nobody has architected: the whole first act, since
// nothing of this tree has been claimed, decomposed or registered.
func writeFreshTree(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and %s %d, holding an admission slot while nothing in it runs. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and %s, then",
		start.Root.Key, ArchitectFreshTreeLabel, start.Root.Generation, ArchitectDecompositionLabel)
	writeGateAsk(task, start.Gate.Steps, "")
}

// writeCarriedPhase writes the task of a root a hand-over carried in. It stands in the phase that
// came with it, and its workers already run from the phase records the carry brought, so it is
// asked for no spec and no decomposition (internal/prompts/roles/architect-root.md).
func writeCarriedPhase(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and stands in %s, %s, with the design gate it brought. "+
		"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. "+
		"Decompose nothing and release no child.",
		start.Root.Key, start.Root.Phase, ArchitectCarriedPhaseLabel)
}

// writeGateStanding writes the task of a root that already holds design gate rows: where its gate
// stands, step by step, and then the tree taken up from there. A spec is registered under those
// rows already, so the task asks for no decomposition.
func writeGateStanding(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and %s: %s. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands "+
		"and take the tree up from there.",
		start.Root.Key, ArchitectGateRowsLabel, gateStanding(start.Gate))
}

// writeEarlierGeneration writes the task of a tree that ran at an earlier generation. Re-admission
// cleared that generation and its design gate with it, so the spec is registered again; the
// children of the old decomposition are not filed again.
func writeEarlierGeneration(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and its tree %s and is open again at generation %d, holding an admission slot "+
		"while nothing in it runs. Re-admission cleared the old generation and its design gate with it. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, then",
		start.Root.Key, ArchitectReadmittedLabel, start.Root.Generation)
	writeGateAsk(task, start.Gate.Steps, " again")
	task.WriteString(" Decompose nothing anew.")
}

// writeGateAsk writes what the architect does about the design gate once the spec is written: a
// request for approval of each document the deployment gates and a registration of each of their
// steps, or, where it gates none, the one registration that opens the gate with no approval
// (skills/legion-architect/references/design-gate.md, "The steps"). again is " again" for a tree
// whose earlier generation's gate re-admission cleared, and empty for a tree registering a first
// time.
func writeGateAsk(task *strings.Builder, steps []record.DesignStep, again string) {
	switch {
	case len(steps) == 1:
		fmt.Fprintf(task, " request approval of %s and register that step of its design gate%s.", gatedDocuments(steps), again)
	case len(steps) > 1:
		fmt.Fprintf(task, " request approval of each document this deployment gates and register each of their steps of its design gate%s: %s.",
			again, gatedDocuments(steps))
	default:
		fmt.Fprintf(task, " register that spec%s at its current version, which opens the design gate with no approval.", again)
	}
}

// gateStanding says where a root's design gate stands, one clause per step, in the deployment's
// order: whether the root has registered the step, at which document version, and what a person
// has said of that version. It reads record.RequiredSteps rather than the gated list, so a
// deployment that gates no step still reports the one registration that opens its gate; under
// that deployment no person approves anything, so no clause of it names one. A changes request
// below the registered version is an answer to a document the architect has moved past, which
// leaves the current version unanswered.
func gateStanding(gate record.TreeGate) string {
	steps := record.RequiredSteps(gate.Steps)
	said := make([]string, 0, len(steps))
	for _, step := range steps {
		row, registered := gate.Rows[step]
		switch {
		case !registered:
			said = append(said, fmt.Sprintf("its %s is not registered", step.Document()))
		case len(gate.Steps) == 0:
			said = append(said, fmt.Sprintf("its %s is registered at version %d, which opened the gate with no approval",
				step.Document(), row.LatestVersion))
		case record.GateOpen(row):
			said = append(said, fmt.Sprintf("its %s is registered at version %d and a person has approved that version",
				step.Document(), row.LatestVersion))
		case row.ToldChangesVersion != nil && *row.ToldChangesVersion == row.LatestVersion:
			said = append(said, fmt.Sprintf("its %s is registered at version %d and a person has asked for changes to it",
				step.Document(), row.LatestVersion))
		default:
			said = append(said, fmt.Sprintf("its %s is registered at version %d and no person has answered it",
				step.Document(), row.LatestVersion))
		}
	}
	return strings.Join(said, "; ")
}

// gatedDocuments names every gated step's own document, in the deployment's order, as a sentence
// lists them: "its decisions document and its Verification document".
func gatedDocuments(steps []record.DesignStep) string {
	named := make([]string, len(steps))
	for i, step := range steps {
		named[i] = "its " + step.Document()
	}
	if len(named) == 1 {
		return named[0]
	}
	return strings.Join(named[:len(named)-1], ", ") + " and " + named[len(named)-1]
}

// CarriesArchitectStart says whether task is one ArchitectStartTask wrote, of any of its four
// shapes. Two readers ask it. The narrowing a root architect's held task goes through when a
// decision it named leaves the tree (NarrowRootTask): a task that carries the instruction keeps
// it, and one that carries only what its tree was owed is rewritten from what it still names. And
// the sweep's send of an entry a start already raised (SendArchitectStart): a task the
// derivation wrote is derived again, and one the resume of a person's answer wrote is sent as
// written.
func CarriesArchitectStart(task string) bool {
	return strings.HasSuffix(task, architectStartTail)
}

// NarrowRootTask is a root architect's held task rewritten from what it still names of its tree's
// kept decisions (the daemon's rootTask, which drops a decision whose issue has left this tree).
// The decisions' own text is written again from them, as it always was (KeptDecisionsTask), and
// where the task it rewrites carried the first-task instruction that instruction is written again
// with it: a decision leaving the tree says nothing about whether the tree has been taken up, and
// a rewrite that dropped the instruction would hand a fresh architect the empty task the
// instruction exists to replace. start is the record as it stands at the narrowing, whose Decided
// this writes from stays; the shape is derived again from it, so a root that moved between the
// send and the narrowing is rewritten into the shape it stands in now, and a root that moved past
// admitted under its own power, which the derivation serves no task, is rewritten into the kept
// decisions' own text alone. The empty string is a task that names nothing, which its caller
// retires rather than sends.
func NarrowRootTask(start ArchitectStart, held string, stays record.KeptDecisions) string {
	start.Decided = KeptDecisionsTask(start.Root.Tree, stays)
	if !CarriesArchitectStart(held) {
		return start.Decided
	}
	return ArchitectStartTask(start)
}
