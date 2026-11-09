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
// here. The thirteen files below carry the fourteen texts that state it — admit.go carries two
// of them — and the first loop reads the fragment's presence and nothing else: whether the
// sentence around it is true is a reader's to judge, never this loop's, so green here is
// coverage and not correctness. Both loops read a file through one helper that puts its prose on
// a single line, so a text wrapped across a line break reads as one written out on a single
// line. (INFRA-1100.)
//
// The rule those texts keep, and this test with them: a text states the placement,
// record.WaitingLineBand, with a tree root's own first phase placed at the moment its gate
// opened, and never the position that placement gives an issue, which depends on what else waits
// (record.Waiting, record/project.go). The second loop is the ban that rule needed. It runs over
// the thirteen files of the one list this test holds, which is every file held to the fragment
// at all: the list of files that named the waiting-line notice went with that notice when
// INFRA-1909 deleted it, since an issue now keeps its admission slot until it is done, leaves
// the workflow or waits only for its merge, and no answered root waits for a slot to resume.
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
// Whether a sentence claims a position or merely defines the ordinal 1 is a reading, and the
// program that tried it here was a sentence parser with two escape hatches — about 170 lines of
// guesswork — which this repository does not keep: a programmatic gate checks what a program
// checks easily and leaves the fuzzy reading to the agent who writes the text. The ban needs no
// escape hatch because its phrases are narrow enough to leave running-legion.md's honest
// definition of the ordinal, `1 being first in the line`, standing.
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
	for _, file := range band {
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
