package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/record"
)

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
// stating the tree. Admission composes this for the start that creates its tree's architect and
// no other: a start that resumes an architect that has already run carries what its tree is owed
// and nothing else, since that session is not born empty and may hold a task of its own.
//
// decided is what the same start names of the decisions its tree is owed (ParkedDecisionTask),
// empty where there are none; it leads the task rather than replacing it, since a person's
// decision is read first and a tree that owes one still has to be taken up.
// steps are the design-gate steps the deployment gates: with none, a registration opens the gate
// and no approval is asked for (skills/legion-architect/references/design-gate.md).
func ArchitectStartTask(root record.Issue, steps []record.DesignStep, decided string) string {
	var task strings.Builder
	if decided != "" {
		task.WriteString(decided)
		task.WriteString("\n\n")
	}
	fmt.Fprintf(&task, "%s is yours and its tree is open at generation %d, holding an admission slot while nothing in it runs. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and every child of your decomposition filed, then",
		root.Key, root.Generation)
	if len(steps) > 0 {
		task.WriteString(" request approval of that spec and register its design gate.")
	} else {
		task.WriteString(" register that spec at its current version, which opens the design gate with no approval.")
	}
	task.WriteString(" Nothing else starts this tree, and no message is coming.")
	return task.String()
}
