package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// refusedPrefix is an HTML block the parser refuses, so a body that opens with it is read by the
// line scan, which keeps list markers, emphasis and a link's syntax in the sentence.
const refusedPrefix = "<div>\nA block.\n</div>\n\n"

func TestKeyPointer(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		pattern string // "" where the body gives no finding
		text    string // the sentence found, where it is not the body
		rewrite string // the finding's rewrite, where the case checks it
	}{
		// The shapes the owner reported, from real texts.
		{name: "a list line whose only label is the key and a link", body: "- BET-34: <https://mlflow.example/runs/34>",
			pattern: keyPointerLabel, text: "BET-34: URL",
			rewrite: "Say in words what BET-34 delivers. Put the key or the link after those words."},
		{name: "a key subject before a colon", body: "INFRA-805 continues in parallel: root cause found.", pattern: keyPointerSubject},
		{name: "an address subject", body: "dispatch://BET-661 reads them as the build snapshot.", pattern: keyPointerSubject,
			rewrite: "Say in words what dispatch://BET-661 delivers. Put the key or the link after those words."},
		{name: "a document address subject", body: "dispatch://BET-559/artifact/removal-units-md names 15 shared groups.", pattern: keyPointerSubject},
		{name: "two keys and a relative clause", body: "BET-9 and BET-10, which XV6's owner session is working, pause their writes.",
			pattern: keyPointerSubject, rewrite: "Say in words what BET-9 and BET-10 deliver. Put the keys or the links after those words."},
		{name: "a passive", body: "INFRA-1513 is closed as its duplicate.", pattern: keyPointerSubject},
		{name: "a parenthetical of one word and a link (the BET-1323 sentence)",
			body: "dispatch://BET-915 (approved: dispatch://BET-915/ask/561eb61a) moves that lane's code to lean-tasks.", pattern: keyPointerSubject},
		{name: "a possessive key that opens the sentence", body: "BET-1323's open spec approval covers them.", pattern: keyPointerPossessive,
			rewrite: "Say in words what BET-1323 delivers. Put the key or the link after those words."},
		{name: "a possessive key inside the sentence", body: "The plan stands, as BET-915's architect wrote.", pattern: keyPointerPossessive},

		// No finding, from real texts.
		{name: "a key after the words that name it", body: "The conformance check (BET-823) passes."},
		{name: "an address in a parenthetical after words", body: "The grading roadmap already plans to rename them (dispatch://BET-59 end-state)."},
		{name: "a key as a place", body: "Read the three drafts on LINUX-1234."},
		{name: "a run id", body: "The failure is in run 43057728552."},
		{name: "a file and a line number", body: "The check stops at isolation.py:1958."},
		{name: "a key in a link whose text is words", body: "[The move of the export code](dispatch://BET-915) lands today."},
		{name: "a parenthetical that names the key before a verb", body: "BET-894 (the comparator, spec v12, which you approved) has 8 of its 12 children done."},
		{name: "a parenthetical that names the key before a colon", body: "- BET-85 (scaffold against plain): <https://mlflow.example/runs/85>"},
		{name: "a parenthetical of three words", body: "INFRA-118 (Envoy hub deploy) moves through INFRA-127, as you said."},

		// The other pointers and openings.
		{name: "a link subject", body: "<https://github.com/o/r/pull/3> adds review-change.md.", pattern: keyPointerSubject, text: "URL adds review-change.md.",
			rewrite: "Say in words what the link delivers. Put the key or the link after those words."},
		{name: "a pull request of a named repository", body: "acme/infra#3315 put the gateway below the daemons.", pattern: keyPointerSubject},
		{name: "a pull request number", body: "#3710 merges first.", pattern: keyPointerSubject},
		{name: "a possessive pull request number", body: "The fix follows #3981's review.", pattern: keyPointerPossessive,
			rewrite: "Say in words what #3981 delivers. Put the key or the link after those words."},
		{name: "a possessive pull request of a named repository", body: "Then acme/infra#3445's checks pass.", pattern: keyPointerPossessive},
		{name: "three keys joined by commas and and", body: "INFRA-4, INFRA-47 and INFRA-48 wait on the review.", pattern: keyPointerSubject},
		{name: "an opening word", body: "So INFRA-481 waits on the review.", pattern: keyPointerSubject},
		{name: "an opening word and a comma", body: "Also, #3840 conflicts with the lane.", pattern: keyPointerSubject},
		{name: "an adverb before the verb", body: "dispatch://INFRA-910 then gives an agent a token.", pattern: keyPointerSubject},
		{name: "a floating quantifier before the verb", body: "OSSV-343 and OSSV-432 each came back.", pattern: keyPointerSubject},
		{name: "a modal", body: "OSSV-474 may build only on the farm.", pattern: keyPointerSubject},
		{name: "a prepositional phrase between commas", body: "dispatch://BET-927, in lean-tasks, moves the four cutters.", pattern: keyPointerSubject},
		{name: "emphasis around the key", body: "- **BET-34**: <https://mlflow.example/runs/34>", pattern: keyPointerLabel, text: "BET-34: URL"},
		{name: "a heading labelled by its key", body: "## INFRA-3231: the writing check", pattern: keyPointerLabel, text: "INFRA-3231: the writing check"},
		{name: "the line scan keeps the list marker, the emphasis and the link", body: refusedPrefix + "- **BET-34**: [graph](https://mlflow.example/runs/34)",
			pattern: keyPointerLabel, text: "- **BET-34**: [graph](https://mlflow.example/runs/34)"},
		{name: "the line scan keeps a link whose text is words", body: refusedPrefix + "[The move of the export code](dispatch://BET-915) lands today."},

		// The edges of a pointer subject.
		{name: "a key in inline code", body: "`BET-34` adds a cache."},
		{name: "a possessive key in inline code", body: "`BET-34`'s spec covers it."},
		{name: "a sentence of only a key and a link", body: "BET-34 <https://mlflow.example/runs/34>"},
		{name: "a sentence of only a key", body: "- OSSV-349."},
		{name: "a key as the object of a clause", body: "The lane waits until INFRA-688 re-lands."},
		{name: "a key that runs into a version", body: "GPT-6.1 approved this version."},
		{name: "a noun after the key", body: "INFRA-430 architect: thank you."},
		{name: "an appositive after the key", body: "INFRA-1242, the child that builds it, merged tonight."},
		{name: "a quoted title after the key", body: "- dispatch://INFRA-190 “A linked image loses its link when a document is saved”."},
		{name: "a preposition after the keys", body: "| Issue |\n| --- |\n| dispatch://BET-467 and dispatch://BET-580 for the namespace model |"},
		{name: "a link in a parenthetical after words", body: "The removal units (dispatch://BET-559/artifact/removal-units-md) name 15 groups."},
		{name: "a parenthetical of one word and a version", body: "INFRA-2397 (approved v14) carries the same deletion.", pattern: keyPointerSubject},
		{name: "a parenthetical of one word and a date", body: "sjawhar/legion#2228 (merged 2026-10-27) lets a session replay what it missed.", pattern: keyPointerSubject},
		{name: "a parenthetical that names the key leaves a possessive key", body: "BET-894 (the comparator) replaces BET-12's cache.", pattern: keyPointerPossessive,
			rewrite: "Say in words what BET-12 delivers. Put the key or the link after those words."},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if strings.HasPrefix(test.body, refusedPrefix) {
				if _, err := pmdoc.Parse(test.body); err == nil {
					t.Fatalf("pmdoc.Parse accepts %q; the case must reach the line scan", test.body)
				}
			}
			got := of(check("body", test.body), "key-pointer")
			if test.pattern == "" {
				if len(got) != 0 {
					t.Fatalf("check(%q) = %+v, want no key-pointer finding", test.body, got)
				}
				return
			}
			text := test.text
			if text == "" {
				text = test.body
			}
			if len(got) != 1 || got[0].Text != text {
				t.Fatalf("check(%q) = %+v, want one key-pointer finding of %q", test.body, got, text)
			}
			if pattern, _ := keyPointerShape(got[0].Text); pattern != test.pattern {
				t.Errorf("keyPointerShape(%q) = %q, want %q", got[0].Text, pattern, test.pattern)
			}
			if test.rewrite != "" && got[0].Rewrite != test.rewrite {
				t.Errorf("rewrite = %q, want %q", got[0].Rewrite, test.rewrite)
			}
		})
	}
}

// A sentence that both opens with a pointer subject and holds a possessive key gives one finding,
// which names the subject.
func TestKeyPointerGivesOneFindingASentence(t *testing.T) {
	sentence := prose.Sentence{Field: "body", Text: "BET-12 replaces BET-11's cache."}
	got := keyPointer(sentence)
	want := []Finding{{Field: "body", Rule: "key-pointer", Text: sentence.Text,
		Rewrite: "Say in words what BET-12 delivers. Put the key or the link after those words."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyPointer = %+v, want %+v", got, want)
	}
}
