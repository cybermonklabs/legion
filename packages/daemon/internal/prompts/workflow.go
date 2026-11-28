package prompts

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// WorkflowLabel starts the line of a started worker's task that names its issue's workflow
// (WorkflowLine). The part every phase worker and the merger share (go/worker-common.md) tells them
// to read the line by this label, and a test holds the two spellings to each other.
const WorkflowLabel = "Workflow:"

// WorkflowLine is the line a started worker's task carries for an issue on a workflow other than
// the default (dispatch://INFRA-3570): the workflow's name, then each of its phases in order with
// the role that works it. It is the one place a worker is told its own issue's line.
//
// It goes in the task and not in a role part because no role part is one issue's: New snapshots
// the parts once per role at boot, Compose picks them by role and root alone, and a session keeps
// the prompt of its launch while a reopened child, back at admitted, may be given another workflow
// before its next run starts (record.Issue.Workflow). The task is written at each phase start and
// each resume of the issue (internal/workflow's task and ResumePhaseTask), beside the `Handoff:` and
// `Reason:` lines the parts already read, so it always names the line the issue walks now.
//
// The default sequence gets no line, and the answer is "": the parts state the default's phases in
// its order already and say that a task with no line is on the default, so the task of an issue on
// the default reads word for word as it did before the library existed, and nothing a running tree
// is told changes until a project or an architect chooses another workflow.
func WorkflowLine(sequence phase.Sequence) string {
	if sequence.Name == phase.DefaultSequenceName {
		return ""
	}
	steps := make([]string, len(sequence.Phases))
	for i, p := range sequence.Phases {
		steps[i] = fmt.Sprintf("%s (%s)", p, phase.RoleFor(p))
	}
	return fmt.Sprintf("%s %s, in order: %s.", WorkflowLabel, sequence.Name, strings.Join(steps, ", "))
}
