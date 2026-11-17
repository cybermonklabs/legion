package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Where the waiting line puts an issue whose work came back is shipped text, so the fragment is
// read from the source it is declared in (record.WaitingLineBand) rather than written out again
// here. Sixteen files carry the eighteen texts that state it — AGENTS.md and admit.go carry two
// each — and the eight of them that are files of packages/daemon are read here, one loop running
// both checks over each: the fragment's presence, and the ban below. Presence reads the fragment
// and nothing else: whether the sentence around it is true is a reader's to judge, never this
// loop's, so green here is coverage and not correctness. The file is read through one helper
// that puts its prose on a single line, so a text wrapped across a line break reads as one
// written out on a single line. (INFRA-1100.)
//
// The eight texts outside packages/daemon are read out of waiting_line_words_extract.txt, the
// copy of their sentences committed beside this package, and never out of the files themselves:
// a test that reads a shipped text puts that text in this job's filter — a declared read applies
// to every compiled source file of a run — and an edit of AGENTS.md or a skill page would then
// start the whole daemon Go lane. Each quotation is held to the constant its file was held to,
// so a constant renamed in the daemon fails here whatever those texts say, which is the half of
// this check that had to stay with the product. What keeps the copy the texts' own, holds it to
// cover every file of that list, and carries the ban over them, is internal/textcheck's
// TestTheWaitingLineWordsExtractIsWhatTheShippedTextsSay and
// TestNoShippedTextClaimsAPositionInTheWaitingLine (INFRA-1413).
//
// The rule those texts keep, and this test with them: a text states the placement,
// record.WaitingLineBand, with a tree root's own first phase placed at the moment its gate
// opened, and never the position that placement gives an issue, which depends on what else waits
// (record.Waiting, record/project.go). The ban is what that rule needed. It runs over the files
// of packages/daemon this test reads, which are its own job's inputs either way; over the texts
// outside it, a ban is a whole-file negative that no excerpt can prove, and internal/textcheck
// is where it runs.
//
// Presence alone let a position claim through: AGENTS.md carried record.WaitingLineBand word for
// word in a sentence about a root's own first phase, two lines below a sentence that claimed a
// position for an issue whose work came back, and the presence check passed on the neighbouring
// sentence's words. The ban reads that same one-line text, lower-cased.
//
// What the ban proves: no file of the list carries one of the four wordings that claim a
// position. What it does not prove: whether what a file says instead is true, which stays a
// reader's to judge; and a wrong wording nobody has written yet, since the list holds the
// phrases these texts used and no more.
//
// It bans rather than judges because the judgement is fuzzy and a plain substring is not.
// Whether a sentence claims a position or merely defines the ordinal 1 is a reading, and a
// program that reads sentences to decide it is guesswork this repository does not keep: a
// programmatic gate checks what a program checks easily and leaves the fuzzy reading to the
// agent who writes the text. The ban needs no escape hatch because its phrases are narrow
// enough to leave running-legion.md's honest definition of the ordinal, `1 being first in the
// line`, standing.
func TestTheShippedWordsNameTheWaitingLineBand(t *testing.T) {
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
	// Lower case, since the text is lower-cased before the comparison: a sentence that opens on
	// one of these is banned as the one that carries it mid-sentence is.
	banned := []string{
		"front of the waiting line",
		"front of the line",
		"front of the whole line",
		"never first in",
	}
	// One read a file: the collapsed text serves the presence check and the ban alike, so the
	// failures of one file are reported together rather than the whole list twice.
	for _, file := range band {
		text := collapsed(file)
		if !strings.Contains(text, record.WaitingLineBand) {
			t.Errorf("%s states where the waiting line puts an answered root without saying it stands %q",
				file, record.WaitingLineBand)
		}
		lowered := strings.ToLower(text)
		for _, phrase := range banned {
			if !strings.Contains(lowered, phrase) {
				continue
			}
			t.Errorf("%s says %q, which no file this test lists may say: such a text states where "+
				"the line puts an issue — %q, a tree root's own first phase placed at the moment its "+
				"gate opened — and never the position that gives it, which depends on what else "+
				"waits (record.Waiting, record/project.go).",
				file, phrase, record.WaitingLineBand)
		}
	}
	// The texts outside packages/daemon, as the copy beside this package quotes them: every
	// quotation is held to what the file it was taken from was held to, so a quotation that no
	// longer carries the constant fails here, which is what catches a renamed constant.
	for _, quoted := range waitingLineBandQuotations(t) {
		if !strings.Contains(quoted.body, record.WaitingLineBand) {
			t.Errorf("%s, as %s quotes it, states where the waiting line puts an answered root "+
				"without saying it stands %q", quoted.path, waitingLineWordsExtract, record.WaitingLineBand)
		}
	}
}

// The copy of the shipped texts outside packages/daemon, committed beside this package, and the
// one form word its format knows.
const (
	waitingLineWordsExtract = "waiting_line_words_extract.txt"
	collapsedForm           = "collapsed"
)

// bandQuotation is one quotation of that copy: the path it was taken from and its body, the
// bytes under the section's own `#` line. A file opens more than one section where it states the
// placement more than once, and each quotation answers for itself.
type bandQuotation struct{ path, body string }

// waitingLineBandQuotations reads that copy. A line `# <path> collapsed` opens a quotation and
// every line under it, to the next such line, is that quotation's own bytes; the lines above the
// first are the file's header, and a `# ` line inside a quotation that opens nothing is the
// quoted text's own, since a collapsed body may itself begin with a hash. A quotation is the
// file's prose on one line because the fragment this loop looks for is wrapped across a line
// break in every one of these texts, so it is a substring of that one-line form and of no raw
// text; a marker in any other form is one no reader of this copy knows, and
// internal/textcheck's TestEveryWaitingLineWordsExtractSectionIsCollapsed is what refuses it.
func waitingLineBandQuotations(t *testing.T) []bandQuotation {
	t.Helper()
	text, err := os.ReadFile(waitingLineWordsExtract)
	if err != nil {
		t.Fatalf("read %s: %v", waitingLineWordsExtract, err)
	}
	var quoted []bandQuotation
	at := -1
	for _, line := range strings.Split(string(text), "\n") {
		marker, hashed := strings.CutPrefix(line, "# ")
		parts := strings.Fields(marker)
		if hashed && len(parts) == 2 && parts[1] == collapsedForm {
			quoted = append(quoted, bandQuotation{path: parts[0]})
			at = len(quoted) - 1
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
