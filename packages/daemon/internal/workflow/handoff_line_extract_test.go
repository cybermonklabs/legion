package workflow

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// handoffLineExtract is the copy of task's Handoff: sentence that the skill-scenario rig and the
// plan-pointer drive write their task lines from.
const handoffLineExtract = "handoff_line_extract.txt"

// The product half of the handoff line's copy: handoff_line_extract.txt, with its three
// placeholders filled, is the sentence task writes for a predecessor's handoff record. There is
// no text half: the two scripts that hand an agent a task line read the copy itself, so this test
// alone keeps what they write the daemon's. The daemon's own tests of the tasks it writes keep no
// words of the line: they assert what the line names (assertNamesTheHandoff), or read the line
// from this copy (handoffPartOf, its second reader, through handoffSentence as this test does).
func TestTheHandoffLineExtractIsTheSentenceTheTaskWrites(t *testing.T) {
	t.Parallel()
	sentence := handoffSentence(t)
	for _, placeholder := range []string{"{word}", "{version}", "{issue}"} {
		if !strings.Contains(sentence, placeholder) {
			t.Errorf("%s names no %s, which both scripts fill in", handoffLineExtract, placeholder)
		}
	}
	issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Testing}
	handoff := record.PhaseRow{Role: claim.RoleImplementer, HandoffRecord: "implement-7", HandoffVersion: 7}
	filled := strings.NewReplacer("{word}", handoffWord(handoff.Role), "{version}", strconv.Itoa(handoff.HandoffVersion), "{issue}", issue.Key).Replace(sentence)
	at := WorkingCopy(issue, nil)
	if got, want := task(issue, at, handoff, ""), taskOpening(issue, at)+" "+filled; got != want {
		t.Errorf("task writes\n%q\nand %s, filled, is\n%q\nchange the two in one commit", got, handoffLineExtract, want)
	}
}

// handoffSentence is the sentence handoff_line_extract.txt holds, its placeholders unfilled: the
// one line after the blank line that ends the copy's `#` header. Both readers of the copy in this
// package take it from here (TestTheHandoffLineExtractIsTheSentenceTheTaskWrites, handoffPartOf),
// so the test that holds the copy equal to what task writes reads it by the same rule as the
// tests that fill it in.
func handoffSentence(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(handoffLineExtract)
	if err != nil {
		t.Fatalf("read %s: %v", handoffLineExtract, err)
	}
	_, sentence, ok := strings.Cut(string(body), "\n\n")
	if !ok {
		t.Fatalf("%s has no blank line between its `#` header and the sentence", handoffLineExtract)
	}
	sentence = strings.TrimSuffix(sentence, "\n")
	if strings.Contains(sentence, "\n") {
		t.Fatalf("%s holds more than one line after its header, and its readers take the first alone:\n%s", handoffLineExtract, sentence)
	}
	return sentence
}

// assertNamesTheHandoff fails the test unless task names the handoff record of word at version
// the way an agent reads it: the command that reads that version, the same command with no
// --version, which reads the record's newest version, and that a newer version is a correction.
// The sentence's words are held once, by TestTheHandoffLineExtractIsTheSentenceTheTaskWrites, so
// a test of what a task carries reads what it carries and not how the line words it.
func assertNamesTheHandoff(t *testing.T, task, issue, word string, version int) {
	t.Helper()
	read := fmt.Sprintf("dispatch handoff-read --issue %s --phase %s", issue, word)
	if pinned := fmt.Sprintf("%s --version %d", read, version); !strings.Contains(task, pinned) {
		t.Errorf("task %q does not name %q, the command that reads the version the daemon accepted", task, pinned)
	}
	if !slices.ContainsFunc(strings.Split(task, read)[1:], func(rest string) bool { return !strings.HasPrefix(rest, " --version") }) {
		t.Errorf("task %q does not name %q with no --version, the command that reads the record's newest version", task, read)
	}
	if !strings.Contains(task, "correction") {
		t.Errorf("task %q does not say that a version above %d is a correction its writer recorded", task, version)
	}
}
