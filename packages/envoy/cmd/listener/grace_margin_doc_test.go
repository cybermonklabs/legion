package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// graceMarginDocs are the documents that state the margin a listener pod grants above its
// pre-stop wait and stopBudget: read is the path from this package's directory, and path names
// the same file from the repository root, for a failure read in CI. packages/envoy/infra is a Go
// module of its own and AGENTS.md is prose, so none of the three can name listenerGraceMargin as
// a symbol; each writes the number out and cites the constant beside it, and this list is what
// brings them back to it.
var graceMarginDocs = []struct{ read, path string }{
	{filepath.Join("..", "..", "infra", "README.md"), "packages/envoy/infra/README.md"},
	{filepath.Join("..", "..", "infra", "docs", "nats-hub.md"), "packages/envoy/infra/docs/nats-hub.md"},
	{filepath.Join("..", "..", "AGENTS.md"), "packages/envoy/AGENTS.md"},
}

// graceMarginConst is the constant the sentences cite, written here as the documents write it.
const graceMarginConst = "listenerGraceMargin"

// graceMarginSentence matches the one shape the sentences are written in, "margin of <n> seconds
// (`listenerGraceMargin`)", and captures the number. The citation is part of the shape and not
// decoration around it: packages/envoy/AGENTS.md uses the word "margin" some twenty times for a
// document's margin, and a pattern that asked only for a margin and a number would read one of
// those and hold a paragraph about layout to this listener's stop. A sentence is matched after
// runs of whitespace are collapsed to single spaces, so one an editor wraps across two lines is
// still one sentence here.
var graceMarginSentence = regexp.MustCompile("margin of ([0-9]+) seconds \\(`" + graceMarginConst + "`\\)")

// graceMarginWhitespace is the run of whitespace collapsed to a single space before matching.
var graceMarginWhitespace = regexp.MustCompile(`\s+`)

// TestDocumentedGraceMarginIsTheListenerGraceMargin holds every sentence that states the
// listener pod's grace margin to listenerGraceMargin, which nothing else does. Each sentence
// cites the constant by name, and a citation is not a check: a reader who moves
// listenerGraceMargin and reads the documents afterwards finds four sentences that still name the
// constant they now contradict, each looking right on its own. Only something that reads both
// sides at once can tell them apart, and this is that.
//
// What is compared: the number each sentence states, in packages/envoy/infra/README.md (twice),
// packages/envoy/infra/docs/nats-hub.md and packages/envoy/AGENTS.md, against
// int(listenerGraceMargin.Seconds()). Nothing else of the sentence is read.
//
// What this cannot see: it does not read a sentence's meaning, so a sentence whose number is
// right while the prose around it is stale, wrong, or describes some other pod's stop passes
// here. And it reads one shape only, so a sentence that states the margin some other way - a
// spelled-out "seven seconds", a margin stated in a table or a diagram, a citation written at
// the other end of the sentence - is invisible to it and is held by nothing. That is why the
// counts below are checked: a document that loses its sentence, or has it rewritten into a shape
// this does not know, fails rather than quietly covering one sentence less.
//
// The subtests stay serial: files and sentences are written inside them and compared after the
// loop, so a t.Parallel() added to one would make the counts fail rather than pass wrongly.
func TestDocumentedGraceMarginIsTheListenerGraceMargin(t *testing.T) {
	margin := int(listenerGraceMargin.Seconds())
	files, sentences := 0, 0
	for _, doc := range graceMarginDocs {
		t.Run(doc.path, func(t *testing.T) {
			// A document that cannot be read is this check losing a side, which fails here: a
			// skip would be an absence that reads exactly like a pass.
			body, err := os.ReadFile(doc.read)
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			collapsed := graceMarginWhitespace.ReplaceAllString(string(body), " ")
			stated := graceMarginSentence.FindAllStringSubmatch(collapsed, -1)
			if len(stated) == 0 {
				t.Fatalf("%s states no margin in the shape %q, the shape every such sentence is written in: the sentence was deleted or rewritten, and the number it stated is now held by nothing. Whoever rewrote it writes it in that shape again, or moves this check to the shape it chose.",
					doc.path, "margin of <n> seconds (`"+graceMarginConst+"`)")
			}
			for _, match := range stated {
				seconds, err := strconv.Atoi(match[1])
				if err != nil {
					t.Fatalf("%s states %q, whose number this check cannot read: %v", doc.path, match[0], err)
				}
				if seconds != margin {
					t.Errorf("%s states %q, a margin of %d seconds, where %s is %d seconds (%s in packages/envoy/cmd/listener/main.go): the constant decides this number and the sentence cites it, so the sentence is the one that is wrong. Whoever moves one moves the other.",
						doc.path, match[0], seconds, graceMarginConst, margin, listenerGraceMargin)
				}
				sentences++
			}
			files++
			t.Logf("%s states the margin in %d sentence(s), each held to %s's %d seconds",
				doc.path, len(stated), graceMarginConst, margin)
		})
	}

	// The counts are held to literals, never to len(graceMarginDocs) or to the sentences this run
	// happened to find, which would compare the list with itself: an entry dropped from it would
	// take its own assertion away and the run would pass having covered one document less, and an
	// emptied list would pass having covered none.
	const (
		graceMarginFiles     = 3
		graceMarginSentences = 4
	)
	t.Logf("read %d documents stating the margin, %d sentences in all", files, sentences)
	if sentences == 0 {
		t.Fatalf("read %d documents and found no sentence stating the margin at all: nothing holds %s to anything, which is the state this check exists to stop", files, graceMarginConst)
	}
	if files != graceMarginFiles || sentences != graceMarginSentences {
		t.Fatalf("read %d of the %d documents this test covers and %d of the %d sentences they state the margin in; a run that reads a sentence less covers a sentence less, so it fails here rather than passing",
			files, graceMarginFiles, sentences, graceMarginSentences)
	}
}

// INFRA-1413 selection probe, never merged.
