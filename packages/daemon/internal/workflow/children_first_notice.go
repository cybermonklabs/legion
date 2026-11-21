package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ChildrenFirstNoticeKind is the kind of the notice below, and the one kind that says a parent's
// own work waits for the work under it.
const ChildrenFirstNoticeKind = record.NoticeKind("children-first")

// ChildCycleReason is what the daemon says of a cycle the children-first judgement found
// (record.ChildWait's Cycle): an open child held by a Dispatch blocked_by link that names an
// issue above it on its own parent chain, so the child waits for an issue that is waiting for
// the child. It is one fixed sentence rather than one built per case, so that a person reading
// two notices reads one rule and a test can hold the daemon to it word for word.
//
// The daemon names it and acts no further. Taking the link off decides what the work depends on,
// which is a person's decision; dropping a Dispatch link of its own accord would lose a fact
// nobody asked it to lose.
func ChildCycleReason(child, ancestor string) string {
	return fmt.Sprintf("%s is blocked_by %s, which stands above it on its own Dispatch parent chain, so neither can start until a person takes that link off", child, ancestor)
}

// childCycleSentences is ChildCycleReason's sentence for each of open whose own blocked_by link
// names an issue above it (record.OpenChild.Cycle), in order, each led by a space and ended by a
// full stop, as the children-first and the awaiting_children notices append them; "" for none.
func childCycleSentences(open []record.OpenChild) string {
	var sentences strings.Builder
	for _, child := range open {
		if child.Cycle != "" {
			sentences.WriteString(" " + ChildCycleReason(child.Key, child.Cycle) + ".")
		}
	}
	return sentences.String()
}

// ChildrenFirstNotice is what the architect that owns a parent is told when the parent's own first
// phase begins to wait for its children: the parent, each open child with the Dispatch status that
// holds it, what ends the wait, and each cycle among them. It is told once per issue and run of
// its tree — the mark's key is the issue, its tree and that tree's root generation (admit's
// tellChildrenFirst) — so a child filed later during one wait tells nobody again, while a
// re-admitted tree, whose architect writes its spec anew, and a sub-parent handed over to a tree
// of its own are told anew. It names no move of a child under another issue of the tree: an open
// child holds every phase start of an issue whose phases started, so such a move would make that
// issue wait too (dispatch://INFRA-3320 R11).
func ChildrenFirstNotice(wait record.ChildWait) record.Notice {
	reason := fmt.Sprintf("%[1]s starts no phase of its own while Dispatch shows an open child under it: %[2]s. "+
		"Each one must be released and run until it leaves the workflow, closed, moved to icebox, or moved out from under %[1]s to an issue outside its tree or to no parent; %[1]s starts its own first phase when the last of them has. "+
		"Move a child to icebox "+parkRule+", and cite that reason in a comment on the child. "+
		"A released child, a held one included, stays under %[1]s, because this wait ends when that child leaves the workflow. "+
		"Move out from under %[1]s, to no parent, only a child you have not released and that should not run now, and never under another issue of this tree: an open child holds every phase start of the issue it stands under. "+
		"After the move, have the person on your tree's root asked when that child's work should run or stop: "+
		"the root's architect asks with dispatch ask, and a child's architect sends the question to the architect above it. "+
		"When they say to run it, move it back under %[1]s and release it; when they say to stop it, close it, or move it to icebox under the rule when they say its work should wait, citing their answer. "+
		"Until they answer, sign off neither %[1]s nor any issue above it that you accept.",
		wait.Parent, openChildNames(wait.Open))
	return record.Notice{Kind: ChildrenFirstNoticeKind, Role: claim.RoleArchitect, Reason: reason + childCycleSentences(wait.Open)}
}
