package textcheck

//ci:reads packages/daemon/internal/workflow/design_approved_wake_extract.txt TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake reads the phrase from it, in place of importing internal/workflow
//ci:reads skills/legion-architect/references/wake-routing.md TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake reads it under ../../../..
//ci:reads packages/daemon/internal/prompts/roles/architect-root.md TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake reads it under ../../../..

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A root waiting on a person gets no design-approved notice, so the architect's instructions must
// take the resume task that says the gate is open as that wake: the wake routing table and the
// root architect's prompt each name the resume's phrase. The claude-envoy plugin's copy under
// packages/claude-envoy/legion/ is generated from the skill file, and nothing commits that tree,
// so a pull request has no copy to read here.
//
// The phrase is read from internal/workflow's design_approved_wake_extract.txt, which that
// package's TestTheDesignApprovedWakeExtractIsTheConstant holds to texts.DesignApprovedWake,
// rather than imported: an import of internal/workflow compiles the daemon's closure, the store
// included, into this package, and this job is built with no database and reads no product code.
//
// Each file is held in both its readings (phraseOfThePage): as written, which an agent reads, and
// as its rendered page shows it, which a person reads, so that a phrase only an HTML comment holds
// fails rather than passing as the instruction.
func TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake(t *testing.T) {
	const extract = "../workflow/design_approved_wake_extract.txt"
	_, phrase, ok := strings.Cut(readExtract(t, extract), "\n\n")
	phrase = strings.TrimSuffix(phrase, "\n")
	if !ok || phrase == "" {
		t.Fatalf("%s holds no phrase under its `#` header", extract)
	}
	for _, file := range []string{
		"skills/legion-architect/references/wake-routing.md",
		"packages/daemon/internal/prompts/roles/architect-root.md",
	} {
		if err := phraseOfThePage(shippedText(t, file), phrase); err != nil {
			t.Errorf("%s does not name the resume's design-approved wake: %v", file, err)
		}
	}
}

// A phrase a reader of the rendered page cannot see is none: the page shows nothing of a phrase
// inside an HTML comment, around it or around the fenced block holding it, or after a comment
// opened on a line of its own and never closed, and a comment that splits the phrase shows it whole
// on the page alone. So the phrase is held in both readings of the page, and each of those fails for
// the comment. A comment that holds nothing of the phrase, a "<!--" inside a fenced block before it,
// and a "<!--" inside a line that its paragraph never closes, which the page shows as text, hide
// nothing of it.
func TestTheDesignApprovedWakeIsHeldWhereAReaderSeesIt(t *testing.T) {
	const phrase = "this resume is your design-approved wake"
	line := "A resume task saying \"" + phrase + "\" is that wake.\n"
	for _, page := range []struct {
		shape, text string
		err         error
	}{
		{"the phrase a reader sees", "## Release\n\n" + line, nil},
		{"the phrase missing", "## Release\n\nA resume task is that wake.\n", errNoPhrase},
		{"the phrase inside a comment", "## Release\n\n<!--\n" + line + "-->\n", errCommented},
		{"a comment splitting the phrase",
			"## Release\n\nA resume task saying \"this resume is your <!-- whole --> design-approved wake\".\n",
			errNoPhrase},
		{"a comment splitting the phrase, the page showing it whole",
			"## Release\n\nA resume task saying \"this resume is your <!-- whole -->design-approved wake\".\n",
			errCommented},
		{"a comment never closed before the phrase", "## Release\n\nRead this.\n<!-- a note\n\n" + line, errCommented},
		{"a \"<!--\" never closed inside a line before the phrase", "## Release\n\nRead this. <!-- a note\n\n" + line, nil},
		{"a comment around the fenced block holding the phrase",
			"## Release\n\n<!--\n```text\n" + phrase + "\n```\n-->\n", errCommented},
		{"a comment holding nothing of the phrase", "## Release\n\n<!-- a note -->\n" + line, nil},
		{"a comment opened inside a fenced block before the phrase",
			"## Release\n\n```html\n<!--\n```\n\n" + line, nil},
	} {
		err := phraseOfThePage(page.text, phrase)
		if !errors.Is(err, page.err) {
			t.Errorf("%s: holding the phrase answers the error %v, want %v: %q", page.shape, err, page.err, page.text)
		}
	}
}

// errNoPhrase is why phraseIn refuses a text: the phrase does not stand in it.
var errNoPhrase = errors.New("the phrase does not stand in it")

// phraseOfThePage holds phrase to page, a Markdown page, in both its readings (bothReadings): as
// written, and as it renders (visibleMarkdown).
func phraseOfThePage(page, phrase string) error {
	return bothReadings(phraseIn(page, phrase), phraseIn(visibleMarkdown(page), phrase))
}

// phraseIn holds phrase to text, one reading of a page: the phrase stands in it (errNoPhrase).
func phraseIn(text, phrase string) error {
	if strings.Contains(text, phrase) {
		return nil
	}
	return fmt.Errorf("%q: %w", phrase, errNoPhrase)
}
