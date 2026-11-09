package workflow

import (
	"os"
	"path/filepath"
	"slices"
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
// The texts of packages/daemon are read as files; every text outside it is read out of
// waiting_line_words_extract.txt, the copy of their sentences committed beside this package,
// and never out of the file itself: a test that reads a shipped text puts that text in this
// job's filter — a declared read applies to every compiled source file of a run — and an edit
// of AGENTS.md or a skill page would then start the whole daemon Go lane. A section of that
// copy is held to what the file it names was held to, so a constant renamed in the daemon
// fails here whatever those texts say, which is the half of this check that had to stay with
// the product. What keeps the copy the texts' own, and carries the ban over them, is
// internal/textcheck's TestTheWaitingLineWordsExtractIsWhatTheShippedTextsSay and
// TestNoShippedTextClaimsAPositionInTheWaitingLine (INFRA-1413).
//
// The naming list and the copy's `naming` sections are everything a root's architect and an
// operator read about a root whose wait a person answered while every slot was taken, with
// gate.go among them because the daemon's own comment is where the next reader of this path
// looks. That loop reads presence and nothing else: that each names the notice and the field,
// so no shipped text forgot the new words. Whether the sentence around a name is true is a
// reader's to judge, never this loop's: green here is coverage, never correctness. The band
// list and the `band` sections carry the texts that state where the line puts an issue whose
// work came back — admit.go carries two of them — and the loop over them reads the fragment's
// presence, again nothing more. That loop and the ban below read a file through one helper
// that puts its prose on a single line, so a text wrapped across a line break reads as one
// written out on a single line; a `band` section is quoted in that same one-line form, since
// that is the form it answers for. (INFRA-1100.)
//
// The rule those texts keep, and this test with them: a text states the placement,
// record.WaitingLineBand, with a tree root's own first phase placed at the moment its gate
// opened, and never the position that placement gives an issue, which depends on what else waits
// (record.Waiting, record/project.go). The third loop is the ban that rule needed, over every
// file of packages/daemon either list names; the texts outside it are the text-check half's,
// because a ban is a whole-file negative and no excerpt can prove one. Presence alone let a
// position claim through: AGENTS.md carried
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
		"packages/daemon/internal/workflow/gate.go",
	}
	band := []string{
		"packages/daemon/internal/admit/admit.go",
		"packages/daemon/cmd/legion/main.go",
		"packages/daemon/internal/admit/awaiting_person_test.go",
		"packages/daemon/internal/admit/hold_test.go",
		"packages/daemon/internal/api/state.go",
		"packages/daemon/internal/daemon/awaiting_catchup.go",
		"packages/daemon/internal/phase/phase.go",
		"packages/daemon/internal/workflow/awaiting_person.go",
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
	// The texts outside packages/daemon, as the copy beside this package quotes them: a file's
	// sections are held to what the file itself was held to, so a quotation that no longer
	// carries the constant fails here, which is what catches a renamed constant. The copy is
	// the text-check half's to keep the texts' own.
	for _, quoted := range waitingLineWordsQuotations(t) {
		switch quoted.list {
		case namingList:
			if !strings.Contains(quoted.body, string(WaitingForSlotNoticeKind)) {
				t.Errorf("%s, as %s quotes it, does not name the notice a root waiting for a slot gets (%q)",
					quoted.path, waitingLineWordsExtract, WaitingForSlotNoticeKind)
			}
			if !strings.Contains(quoted.body, api.WaitingLineField) {
				t.Errorf("%s, as %s quotes it, does not name the %q the root's record carries while it waits",
					quoted.path, waitingLineWordsExtract, api.WaitingLineField)
			}
		case bandList:
			if !strings.Contains(quoted.body, record.WaitingLineBand) {
				t.Errorf("%s, as %s quotes it, states where the waiting line puts an answered root "+
					"without saying it stands %q", quoted.path, waitingLineWordsExtract, record.WaitingLineBand)
			}
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

// The copy of the shipped texts outside packages/daemon, committed beside this package, and the
// two lists its sections answer for, named as the test above names them.
const (
	waitingLineWordsExtract = "waiting_line_words_extract.txt"
	namingList              = "naming"
	bandList                = "band"
)

// wordsSection is one file's quotations in that copy for one of the two lists: the path they
// were taken from, the list they answer for, the form they are quoted in — the file's own bytes
// (`raw`) or its prose on one line (`collapsed`) — and the bodies, joined in the copy's order. A
// file is quoted more than once where the two names it must carry stand a paragraph apart, and
// the join is what lets the loop above read a file's sections as it reads a whole file.
type wordsSection struct{ path, list, form, body string }

// waitingLineWordsQuotations reads that copy. A line `# <path> <list> <form>` opens a quotation
// and every line under it, to the next such line, is that quotation's own bytes; the lines above
// the first are the file's header, and a `# ` line inside a quotation that opens nothing is the
// quoted text's own. A marker whose form does not answer for its list is refused rather than
// read: the form says whether a body is the file's bytes or its one-line form, and the wrong one
// would hold a group to a text in a shape nobody ships.
func waitingLineWordsQuotations(t *testing.T) []wordsSection {
	t.Helper()
	text, err := os.ReadFile(waitingLineWordsExtract)
	if err != nil {
		t.Fatalf("read %s: %v", waitingLineWordsExtract, err)
	}
	var quoted []wordsSection
	at := -1
	for i, line := range strings.Split(string(text), "\n") {
		marker, hashed := strings.CutPrefix(line, "# ")
		parts := strings.Fields(marker)
		if hashed && len(parts) == 3 && (parts[1] == namingList || parts[1] == bandList) {
			if (parts[1] == namingList) != (parts[2] == "raw") {
				t.Fatalf("%s:%d: %s is quoted as `%s %s`: a %q section is the file's own bytes and a "+
					"%q section its one-line form, which is the form each list is read in",
					waitingLineWordsExtract, i+1, parts[0], parts[1], parts[2], namingList, bandList)
			}
			if at = slices.IndexFunc(quoted, func(s wordsSection) bool {
				return s.path == parts[0] && s.list == parts[1]
			}); at < 0 {
				quoted = append(quoted, wordsSection{path: parts[0], list: parts[1], form: parts[2]})
				at = len(quoted) - 1
			}
			continue
		}
		if at < 0 {
			continue
		}
		quoted[at].body += line + "\n"
	}
	// An empty copy is this check failing open: the file is still there, the parse matched
	// nothing, and every text outside packages/daemon would be held to nothing at all.
	if len(quoted) == 0 {
		t.Fatalf("%s quotes no text: the shipped texts outside packages/daemon are being held to "+
			"nothing, and internal/textcheck holds this copy to them", waitingLineWordsExtract)
	}
	return quoted
}
