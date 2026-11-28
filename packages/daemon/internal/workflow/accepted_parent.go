package workflow

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ChildUnderAcceptedParentNotice is what the architect above the child is told when a person
// moves an issue to todo beneath an ancestor whose head the daemon has already accepted
// (record.AcceptedAncestor): the head the last step of the ancestor's task passed, which is the
// reviewed head only where that task has a reviewer step. The issue is recorded, and left
// standing at admitted while that ancestor stands in retro or merging, because work entering the
// workflow beneath it now would let that ancestor merge a head which does not carry the work. The
// two doors an agent uses refuse such an issue outright (the API's CHILD_UNDER_ACCEPTED_PARENT),
// so what reaches this is a person's own move, which the daemon records rather than undoes.
//
// It is the child-status kind every other word about a child entering or leaving its tree is
// told under, so it reaches the architect above the child by the route those already take.
func ChildUnderAcceptedParentNotice(child, ancestor string) record.Notice {
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"%[1]s is beneath %[2]s, whose head the last step of its task passed and the daemon has already accepted, so %[1]s is recorded at admitted and nothing starts it while %[2]s stands in retro or merging.",
		child, ancestor)}
}

// ChildUnderAcceptedParent is what release_children and rerun_child answer for an issue whose
// ancestor the daemon has already accepted (CHILD_UNDER_ACCEPTED_PARENT), naming the ancestor and
// what undoes the refusal. Both calls say one sentence, because they refuse for one reason: a
// parent is accepted only once no issue beneath it is still in the workflow, so nothing enters the
// workflow beneath an issue already accepted. The head is the one the last step of the ancestor's
// task passed, which is what record.Accepted means for every plan (a plan with no reviewer step has
// no reviewed head). operation is what the caller would do again once the ancestor has closed. The
// rule exempts no ancestor, the chain's gate root included (record.AcceptedAncestor walks every
// issue of Chains.Above), so the sentence names none.
func ChildUnderAcceptedParent(child, ancestor, operation string) string {
	return fmt.Sprintf("%[1]s is beneath %[2]s, whose head the last step of its task passed and the daemon has already accepted, and work entering the workflow beneath %[2]s now would let it merge a head that does not carry that work; %[3]s once %[2]s has closed",
		child, ancestor, operation)
}
