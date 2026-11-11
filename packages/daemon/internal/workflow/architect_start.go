package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// architectStartTail ends every task ArchitectStartTask writes, and nothing else writes it. It is
// how a reader of a task the daemon holds tells one that carries that instruction from one that
// carries only what its tree is owed (CarriesArchitectStart), which is what keeps the instruction
// through the narrowing a task's decisions go through when one of them leaves the tree
// (NarrowRootTask).
const architectStartTail = " Nothing else starts this tree, and no message is coming."

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
// Admission composes this for the start of a tree nobody has ever architected — no architect
// claim, no design-gate row of its own, and no hand-over of a root that arrived from another tree
// — and for no other start. An architect that has already run is resumed rather than born, and
// may still hold a task of its own that a second one would be refused behind; a root a hand-over
// carried in stands at a phase of its own with the gates it brought, and is told to take that up
// rather than to decompose a tree again (internal/prompts/roles/architect-root.md). Each of those
// is told by its catch-up, as before.
//
// decided is what the same start names of the decisions its tree is owed (ParkedDecisionTask),
// empty where there are none; it leads the task rather than replacing it, since a person's
// decision is read first and a tree that owes one still has to be taken up.
//
// steps are the design-gate steps the deployment gates, in its own order. The task names each
// step's own document (record.DesignStep.Document), because a deployment that gates the decisions
// and the criteria asks for those two registrations and not the spec's
// (skills/legion-architect/references/design-gate.md, "The steps"); with no step gated, one
// registration of the spec opens the gate and no approval is asked for.
func ArchitectStartTask(root record.Issue, steps []record.DesignStep, decided string) string {
	var task strings.Builder
	if decided != "" {
		task.WriteString(decided)
		task.WriteString("\n\n")
	}
	fmt.Fprintf(&task, "%s is yours and its tree is open at generation %d, holding an admission slot while nothing in it runs. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and every child of your decomposition filed, then",
		root.Key, root.Generation)
	if len(steps) == 1 {
		fmt.Fprintf(&task, " request approval of %s and register that step of its design gate.", gatedDocuments(steps))
	} else if len(steps) > 1 {
		fmt.Fprintf(&task, " request approval of each document this deployment gates and register each of their steps of its design gate: %s.",
			gatedDocuments(steps))
	} else {
		task.WriteString(" register that spec at its current version, which opens the design gate with no approval.")
	}
	task.WriteString(architectStartTail)
	return task.String()
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

// CarriesArchitectStart says whether task is one ArchitectStartTask wrote. Its one reader is the
// narrowing a root architect's held task goes through when a decision it named leaves the tree
// (NarrowRootTask): a task that carries the instruction keeps it, and one that carries only what
// its tree was owed is rewritten from what it still names, as it always was.
func CarriesArchitectStart(task string) bool {
	return strings.HasSuffix(task, architectStartTail)
}

// NarrowRootTask is a root architect's held task rewritten from what it still names of its tree's
// kept decisions (the daemon's rootTask, which drops a decision whose issue has left this tree).
// The decisions' own text is written again from them, as it always was (KeptDecisionsTask), and
// where the task it rewrites carried the first-task instruction that instruction is written again
// with it: a decision leaving the tree says nothing about whether the tree has been taken up, and
// a rewrite that dropped the instruction would hand a fresh architect the empty task the
// instruction exists to replace. root is the tree's root as the record holds it now, and steps the
// deployment's gated steps, the two ArchitectStartTask reads. The empty string is a task that
// names nothing, which its caller retires rather than sends.
func NarrowRootTask(root record.Issue, steps []record.DesignStep, held string, stays record.KeptDecisions) string {
	decided := KeptDecisionsTask(root.Tree, stays)
	if !CarriesArchitectStart(held) {
		return decided
	}
	return ArchitectStartTask(root, steps, decided)
}
