package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The notice tells the root's architect three things it can read nowhere else in the message:
// what answered its wait, where its issue stands in the admission line, and how long that line
// is. It goes to the architect's role, since the root's architect is the one the daemon starts
// again at the next free slot.
func TestTheWaitingForSlotNoticeNamesTheAnswerAndThePlace(t *testing.T) {
	// The moment the wait was answered rides on the row rather than in the text: the test reads
	// it back off the notice below, and no sentence of the reason names it.
	answered := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
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
			notice := WaitingForSlotNotice(tc.by, tc.detail, answered, 2, 5)
			if notice.Kind != WaitingForSlotNoticeKind || notice.Role != claim.RoleArchitect {
				t.Fatalf("the notice is %s to the %s, want %s to the architect", notice.Kind, notice.Role, WaitingForSlotNoticeKind)
			}
			if notice.WaitingForSlot == nil || !notice.WaitingForSlot.AnsweredAt.Equal(answered) {
				t.Fatalf("the notice names the wait %#v, want the wait answered at %s: a held row is published only while the record's wait carries that answer",
					notice.WaitingForSlot, answered)
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
	reason := WaitingForSlotNotice(record.Answer("whenever"), "", time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), 1, 1).Reason
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
	row, err := record.NewOutboxRow("LEGION-208",
		WaitingForSlotNotice(record.AnsweredByAsk, "ask-q", time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), 1, 3),
		time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("encode a waiting-for-slot notice for the outbox: %v", err)
	}
	if row.Issue != "LEGION-208" {
		t.Fatalf("the row is of %s, want the root the notice is about", row.Issue)
	}
}

// The waiting line's two new words and the one fragment that states its order are shipped text,
// so each is read from the source it is declared in rather than written out again here: the
// notice's kind (WaitingForSlotNoticeKind), the name the place carries on the wire
// (api.WaitingLineField) and where the line puts an issue whose work came back
// (record.WaitingLineBand).
//
// The first nine files are everything a root's architect and an operator read about a root whose
// wait a person answered while every slot was taken, with gate.go among them because the daemon's
// own comment is where the next reader of this path looks. This loop reads presence and nothing
// else: that each file names the notice and the field, so no shipped text forgot the new words.
// Whether the sentence around a name is true is a reader's to judge, never this loop's: green
// here is coverage, never correctness. The thirteen below them carry the fourteen texts that
// state where the line puts an issue whose work came back — admit.go carries two of them — and
// the loop over them reads the fragment's presence, again nothing more. That loop and the ban
// below read a file through one helper that puts its prose on a single line, so a text wrapped
// across a line break reads as one written out on a single line. (INFRA-1100.)
//
// The rule those texts keep, and this test with them: a text states the placement,
// record.WaitingLineBand, with a tree root's own first phase placed at the moment its gate
// opened, and never the position that placement gives an issue, which depends on what else waits
// (record.Waiting, record/project.go). The third loop is the ban that rule needed, over every
// file either list names. Presence alone let a position claim through: AGENTS.md carried
// record.WaitingLineBand word for word in a sentence about a root's own first phase, two lines
// below a sentence that claimed a position for an issue whose work came back, and the presence
// check passed on the neighbouring sentence's words. The ban reads that same one-line text,
// lower-cased.
//
// What the ban proves: no file of either list carries one of the four wordings that claim a
// position. What it does not prove: whether what a file says instead is true, which stays a
// reader's to judge; and a wrong wording nobody has written yet, since the list holds the
// phrases these texts used and no more.
//
// It bans rather than judges because the judgement is fuzzy and a plain substring is not.
// Whether a sentence claims a position or merely defines the ordinal 1 is a reading, and the
// program that tried it here was a sentence parser with two escape hatches — about 170 lines of
// guesswork — which this repository does not keep: a programmatic gate checks what a program
// checks easily and leaves the fuzzy reading to the agent who writes the text. The ban needs no
// escape hatch because its phrases are narrow enough to leave running-legion.md's honest
// definition of the ordinal, `1 being first in the line`, standing.
func TestTheShippedWordsNameTheWaitingLineNotice(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	body := func(file string) string {
		t.Helper()
		text, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		return string(text)
	}
	// The same file with its prose on one line: every line loses a leading run of whitespace and
	// the comment markers these files carry, and the words are joined by single spaces. A
	// fragment wrapped inside a `//` or ` * ` comment is the same wording, and a read that left
	// the markers in would see `work came // back` and report a true text as false.
	collapsed := func(file string) string {
		t.Helper()
		var words []string
		for _, line := range strings.Split(body(file), "\n") {
			line = strings.TrimLeft(line, " \t")
			for _, marker := range []string{"//", "*", "#"} {
				if rest, found := strings.CutPrefix(line, marker); found {
					line = rest
					break
				}
			}
			words = append(words, strings.Fields(line)...)
		}
		return strings.Join(words, " ")
	}
	naming := []string{
		"packages/daemon/internal/prompts/roles/architect-root.md",
		"skills/legion-architect/SKILL.md",
		"skills/legion-architect/references/wake-routing.md",
		"skills/legion-architect/references/design-gate.md",
		"skills/legion-architect/references/acceptance.md",
		"docs/site/src/content/docs/legion/running-legion.md",
		"docs/site/src/content/docs/legion/troubleshooting.md",
		"docs/site/src/content/docs/legion/concepts.md",
		"packages/daemon/internal/workflow/gate.go",
	}
	band := []string{
		"AGENTS.md",
		"packages/daemon/internal/admit/admit.go",
		"docs/site/src/content/docs/legion/running-legion.md",
		"docs/site/src/content/docs/legion/troubleshooting.md",
		"packages/contracts/src/legion-api.ts",
		"packages/daemon/cmd/legion/main.go",
		"packages/daemon/internal/admit/awaiting_person_test.go",
		"packages/daemon/internal/admit/hold_test.go",
		"packages/daemon/internal/api/state.go",
		"packages/daemon/internal/daemon/awaiting_catchup.go",
		"packages/daemon/internal/phase/phase.go",
		"packages/daemon/internal/workflow/awaiting_person.go",
		"skills/legion-controller/references/admission.md",
	}
	for _, file := range naming {
		text := body(file)
		if !strings.Contains(text, string(WaitingForSlotNoticeKind)) {
			t.Errorf("%s does not name the notice a root waiting for a slot gets (%q)", file, WaitingForSlotNoticeKind)
		}
		if !strings.Contains(text, api.WaitingLineField) {
			t.Errorf("%s does not name the %q the root's record carries while it waits", file, api.WaitingLineField)
		}
	}
	for _, file := range band {
		if !strings.Contains(collapsed(file), record.WaitingLineBand) {
			t.Errorf("%s states where the waiting line puts an answered root without saying it stands %q",
				file, record.WaitingLineBand)
		}
	}
	// Lower case, since the text is lower-cased before the comparison: a sentence that opens on
	// one of these is banned as the one that carries it mid-sentence is.
	banned := []string{
		"front of the waiting line",
		"front of the line",
		"front of the whole line",
		"never first in",
	}
	read := make(map[string]bool, len(naming)+len(band))
	for _, list := range [][]string{naming, band} {
		for _, file := range list {
			if read[file] {
				continue
			}
			read[file] = true
			text := strings.ToLower(collapsed(file))
			for _, phrase := range banned {
				if !strings.Contains(text, phrase) {
					continue
				}
				t.Errorf("%s says %q, which no file this test lists may say: such a text states where "+
					"the line puts an issue — %q, a tree root's own first phase placed at the moment its "+
					"gate opened — and never the position that gives it, which depends on what else "+
					"waits (record.Waiting, record/project.go).",
					file, phrase, record.WaitingLineBand)
			}
		}
	}
}
