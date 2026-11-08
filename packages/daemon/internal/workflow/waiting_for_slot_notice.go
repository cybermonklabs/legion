package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// WaitingForSlotNoticeKind is the kind of the notice below, and the one kind that says a root's
// wait on a person is over and its own work waits for an admission slot.
const WaitingForSlotNoticeKind = record.NoticeKind("waiting-for-slot")

// WaitingForSlotNotice is what a root's architect is told when an admission pass finds the root's
// wait answered, the root needing an admission slot and the cap holding none free: what answered
// the wait (by and detail, as record.Awaiting holds them), where the root stands in the admission
// line then (place of length), and that the daemon starts the architect again by itself at the
// next free slot. The architect acts on none of it. It is told once per wait, by the mark the
// pass writes beside the answer (record.Awaiting's SlotNoticeAt).
//
// The whole of it is the reason: record.validateOutboxPayload refuses an actor and a ref on a
// kind that is not a spec decision, so the notice carries no other field to say any of this in.
// The place is a snapshot, which the reason says, since the line moves with every pass; the
// issue's own record carries the live place in `waitingLine`.
func WaitingForSlotNotice(by record.Answer, detail string, place, length int) record.Notice {
	reason := fmt.Sprintf("%s Your issue needs an admission slot, and the admission cap has no free slot now. "+
		"Your issue stands at place %d of %d in the admission line. "+
		"The daemon starts you again by itself at the next free slot. Do nothing until it starts you. "+
		"The place above is the place at the moment of this notice. "+
		"The issue's own record carries the live place in its `waitingLine` field.",
		waitAnswered(by, detail), place, length)
	return record.Notice{Kind: WaitingForSlotNoticeKind, Role: claim.RoleArchitect, Reason: reason}
}

// waitAnswered is the one sentence that says what answered the root's wait: the act (by) and that
// act's own detail (detail), as record.Awaiting records the pair. An act this release does not
// know, and one whose detail the record does not carry, get the last sentence, which says only
// that the wait is over: a sentence with a hole where its subject belongs tells a reader less
// than a short one does.
func waitAnswered(by record.Answer, detail string) string {
	const unnamed = "A person's act answered the wait of your issue."
	if by == record.AnsweredByRetraction {
		return "Dispatch retracted the last ask of your wait, so no person will answer it."
	}
	if detail == "" {
		return unnamed
	}
	switch by {
	case record.AnsweredByAsk:
		return fmt.Sprintf("A person answered your ask %s.", detail)
	case record.AnsweredByReply:
		return fmt.Sprintf("A person answered another ask of your issue, %s.", detail)
	case record.AnsweredByTurn:
		return fmt.Sprintf("A session's reply handed your question %s to the agent's turn, so no person is asked anything there.", detail)
	case record.AnsweredByClosed:
		return fmt.Sprintf("%s closed the last ask of your wait, so no person will answer it.", detail)
	case record.AnsweredByApproval:
		// A wait the gates.design: off rule answered carries that suffix on its detail
		// (gateOffAnswer), and no person approved the document then, which the resume task says
		// too: the sentence says it rather than reading the suffix out as part of the version.
		if version, off := strings.CutSuffix(detail, gateOffAnswer); off {
			return fmt.Sprintf("The design gate was off when your wait was answered at %s, so no person approved your document then.", version)
		}
		return fmt.Sprintf("Your design gate answered the wait at %s.", detail)
	case record.AnsweredByChanges:
		return fmt.Sprintf("A person requested changes to your document at %s.", detail)
	case record.AnsweredByVersion:
		return fmt.Sprintf("A newer version of your document, %s, handed the approval request back to you.", detail)
	}
	return unnamed
}
