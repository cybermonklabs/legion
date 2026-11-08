package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The notice tells the root's architect three things it can read nowhere else in the message:
// what answered its wait, where its issue stands in the admission line, and how long that line
// is. It goes to the architect's role, since the root's architect is the one the daemon starts
// again at the next free slot.
func TestTheWaitingForSlotNoticeNamesTheAnswerAndThePlace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		by     record.Answer
		detail string
		says   string
	}{
		{"a person's answer on the awaited ask", record.AnsweredByAsk, "ask-q", "ask-q"},
		{"a person's act on another ask", record.AnsweredByReply, "ask-other", "ask-other"},
		{"an approval of the document", record.AnsweredByApproval, "version 4", "version 4"},
		// The gates.design: off rule answers with that suffix on the detail, and the sentence
		// says the gate was off rather than reading the suffix out as part of the version.
		{"an answer while the design gate was off", record.AnsweredByApproval, "version 4" + gateOffAnswer,
			"The design gate was off when your wait was answered at version 4, so no person approved your document then."},
		{"a changes request", record.AnsweredByChanges, "version 4, say more", "version 4, say more"},
		{"a newer version", record.AnsweredByVersion, "version 5", "version 5"},
		{"a close by a session", record.AnsweredByClosed, "a session of your tree", "a session of your tree"},
		{"a reply handing the question to the turn", record.AnsweredByTurn, "ask-q", "ask-q"},
		// Dispatch's settlement names no detail, so the sentence names the act alone.
		{"Dispatch's settlement", record.AnsweredByRetraction, "", "retracted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := WaitingForSlotNotice(tc.by, tc.detail, 2, 5)
			if notice.Kind != WaitingForSlotNoticeKind || notice.Role != claim.RoleArchitect {
				t.Fatalf("the notice is %s to the %s, want %s to the architect", notice.Kind, notice.Role, WaitingForSlotNoticeKind)
			}
			if !strings.Contains(notice.Reason, tc.says) {
				t.Errorf("the reason %q does not name what answered the wait, %q", notice.Reason, tc.says)
			}
			if !strings.Contains(notice.Reason, "place 2 of 5") {
				t.Errorf("the reason %q does not name the place and the length of the line", notice.Reason)
			}
			if !strings.Contains(notice.Reason, "waitingLine") {
				t.Errorf("the reason %q does not send the reader to the live place on the record", notice.Reason)
			}
		})
	}
}

// An answer this release does not know leaves the architect a sentence that stands on its own,
// never one with a hole where its subject belongs.
func TestTheWaitingForSlotNoticeStandsWithoutTheAnswersDetail(t *testing.T) {
	reason := WaitingForSlotNotice(record.Answer("whenever"), "", 1, 1).Reason
	if !strings.Contains(reason, "A person's act answered the wait of your issue.") {
		t.Errorf("the reason of an answer with no detail = %q, want the sentence that names no subject it has not got", reason)
	}
	if !strings.Contains(reason, "place 1 of 1") {
		t.Errorf("the reason %q does not name the place and the length of the line", reason)
	}
}

// The outbox refuses a kind it does not know, and the admission pass writes this notice inside
// the transaction that marks the wait told: a kind missing from record.validNoticeKind would
// fail that whole transaction rather than lose one notice.
func TestTheOutboxTakesAWaitingForSlotNotice(t *testing.T) {
	row, err := record.NewOutboxRow("LEGION-208", WaitingForSlotNotice(record.AnsweredByAsk, "ask-q", 1, 3),
		time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("encode a waiting-for-slot notice for the outbox: %v", err)
	}
	if row.Issue != "LEGION-208" {
		t.Fatalf("the row is of %s, want the root the notice is about", row.Issue)
	}
}
