package specform

import (
	"fmt"
	"strings"
	"testing"
)

// stopNoSpace is the detail of the rule stop-no-space for word.
func stopNoSpace(word string) string {
	return fmt.Sprintf("a stop in %q has no space after it, so Dispatch cannot find the end of the sentence", word)
}

// checkStatement is the findings of Check for one statement of body.
func checkStatement(body string) (Text, []Finding) {
	text := statement(body)
	return text, check(planted(), Input{Texts: []Text{text}})
}

// TestAClosingQuoteOrBracketAfterAStopBelongsToItsSentence holds the rules of the form to a
// sentence that ends in a closing quote or bracket: the run of closing marks after the stop
// belongs to the sentence, and the white space after that run ends it. Each row is three
// sentences, which a statement may not have. Control: a bracket with no stop before it ends no
// sentence.
func TestAClosingQuoteOrBracketAfterAStopBelongsToItsSentence(t *testing.T) {
	control, got := checkStatement("Dispatch keeps each comment (for a year) and indexes it.")
	if len(got) != 0 {
		t.Fatalf("control: Check(%q) = %#v, want none", control.Body, got)
	}
	for _, body := range []string{
		`Dispatch prints "Start." Dispatch prints "Run." Dispatch prints "Stop."`,
		`Dispatch keeps each comment (for a year.) Dispatch indexes each comment (for search.) Dispatch shows each comment (in the margin.)`,
		"Dispatch prints \u201cStart.\u201d Dispatch prints \u2018Run.\u2019 Dispatch prints [Stop.]",
		`Dispatch prints 'Start.' Dispatch prints "(Run!)" Dispatch prints "Stop?"`,
	} {
		text, got := checkStatement(body)
		if want := []Finding{finding(text, "sentences", "it has 3 sentences, and a statement has one or two")}; !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", body, got, want)
		}
	}
}

// TestANameWithAStopInsideEndsNoSentence holds the rules of the form to a stop that the text
// does not show as an end: an initialism of single letters such as "U.S." is one word, and a stop
// that an upper-case letter follows with no space ends no sentence and is a finding of its own,
// which names the place. Control: three sentences with a space after each stop are refused for
// their count.
func TestANameWithAStopInsideEndsNoSentence(t *testing.T) {
	control, got := checkStatement("Dispatch keeps each comment. Dispatch indexes each comment. Dispatch shows each comment.")
	if want := []Finding{finding(control, "sentences", "it has 3 sentences, and a statement has one or two")}; !sameFindings(got, want) {
		t.Fatalf("control: Check = %#v, want %#v", got, want)
	}
	for _, test := range []struct {
		body  string
		words []string
	}{
		{"Dispatch keeps each record in the U.S. region.", nil},
		{"Dispatch keeps each record in the U.S. region. Dispatch copies each record each night.", nil},
		{"Dispatch reads the file Web.Config at boot. Legion writes it.", []string{"Web.Config"}},
		{"Dispatch reads the file Web.Config at boot.", []string{"Web.Config"}},
		{"Dispatch ships the release v1.2.Next to each server. Legion waits for it.", []string{"v1.2.Next"}},
		{"Dispatch keeps each comment.Dispatch indexes each comment.Dispatch shows each comment.", []string{"comment.Dispatch"}},
		{"Dispatch stops!Dispatch starts?Dispatch waits.", []string{"stops!Dispatch", "starts?Dispatch"}},
		{"Dispatch runs each job on .NET and on Go.", nil},
		{`Dispatch prints "Start."Dispatch stops.`, []string{`"Start."Dispatch`}},
		{"Dispatch reads the file `Web.Config` at boot. Legion writes it.", nil},
	} {
		text, got := checkStatement(test.body)
		var want []Finding
		for _, word := range test.words {
			want = append(want, finding(text, "stop-no-space", stopNoSpace(word)))
		}
		if !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}

// TestTheWordRuleReadsEachSentenceAfterTheWriterPutsTheSpaceIn holds the 25-word rule to two
// sentences that a stop joins. With no space after the stop, the text shows one sentence, and
// the finding names the stop. With the space, the rule reads each sentence: 25 and 26 words give
// the finding of 26, and 25 and 25 give none. ste-lint counts the same sentences, so each long
// one is also its hard finding long-sentence.
func TestTheWordRuleReadsEachSentenceAfterTheWriterPutsTheSpaceIn(t *testing.T) {
	const w25 = "Dispatch shows the person each question of each issue that the person follows, with the age of the question and the key of its project."
	const w26 = "Dispatch shows the person each open approval request that waits for the person, with the document, its version number and the summary that its author wrote."
	const other25 = "Dispatch shows the person each approval request that waits for the person, with the document, its version number and the summary that its author wrote."
	// Each text is longer than an excerpt, which holds its first 160 characters.
	cut := func(findings ...Finding) []Finding {
		for index := range findings {
			findings[index].Excerpt = excerpt(findings[index].Excerpt)
		}
		return findings
	}
	for _, test := range []struct {
		name, second string
	}{
		{"joined", w26},
		{"joined 25 and 25", other25},
	} {
		// The stop joins the last word of w25 and the first of the second sentence into one word.
		words := 24 + len(strings.Fields(test.second))
		joined := statement(w25 + test.second)
		if got, want := check(planted(), Input{Texts: []Text{joined}}), cut(
			finding(joined, "stop-no-space", stopNoSpace("project.Dispatch")),
			finding(joined, "sentence-words", fmt.Sprintf(`a sentence has %d words, and a sentence has at most 25: "Dispatch shows the person each question of each"`, words)),
			finding(joined, "ste100-long-sentence", fmt.Sprintf(`ASD-STE100 does not permit a sentence of more than 25 words: "%d words"`, words)),
		); !sameFindings(got, want) {
			t.Errorf("%s: Check = %#v, want %#v", test.name, got, want)
		}
	}
	spaced := statement(w25 + " " + w26)
	if got, want := check(planted(), Input{Texts: []Text{spaced}}), cut(
		finding(spaced, "sentence-words", `a sentence has 26 words, and a sentence has at most 25: "Dispatch shows the person each open approval request"`),
		finding(spaced, "ste100-long-sentence", `ASD-STE100 does not permit a sentence of more than 25 words: "26 words"`),
	); !sameFindings(got, want) {
		t.Errorf("25 and 26 words: Check = %#v, want %#v", got, want)
	}
	if _, got := checkStatement(w25 + " " + other25); len(got) != 0 {
		t.Errorf("25 and 25 words: Check = %#v, want none", got)
	}
}

// TestTheSentenceRuleCountsTwentySentencesAsAReaderDoes holds sentencesOf to twenty texts of the
// cases where a stop does not end a sentence, or a mark after it hides the end. On each row the
// rule counts what a reader counts, or the row has the finding stop-no-space, where the text
// shows no end. One row is the decided exception: no stop of an initialism ends a sentence, also
// the last one before a space, so "in the U.S. Dispatch keeps it" is one sentence where a reader
// counts two, and the 25-word rule reads the whole of it.
func TestTheSentenceRuleCountsTwentySentencesAsAReaderDoes(t *testing.T) {
	for _, test := range []struct {
		body        string
		reader      int
		rule        int
		stopNoSpace bool
	}{
		{"Dispatch keeps each record in the U.S. region.", 1, 1, false},
		{"The record is in the U.S. Dispatch keeps it.", 2, 1, false},
		{"Dispatch refuses a long statement, e.g. one of three sentences.", 1, 1, false},
		{"Dispatch keeps each record in one region (e.g. the U.S.) for a year.", 1, 1, false},
		{"Dispatch keeps each record in the U.S. region. Dispatch copies each record each night.", 2, 2, false},
		{"Dispatch keeps each log for 1.5 days.", 1, 1, false},
		{"Legion reads terms.md before each run.", 1, 1, false},
		{"Legion reads the file terms.md. Dispatch writes it.", 2, 2, false},
		{"Dispatch ships the release v1.2.3 to each server.", 1, 1, false},
		{"Dispatch ships the release v1.2.Next to each server.", 1, 0, true},
		{"Dispatch reads the file Web.Config at boot.", 1, 0, true},
		{"Dispatch reads the file `Web.Config` at boot.", 1, 1, false},
		{"The check runs `make test. make lint` in one step.", 1, 1, false},
		{"Dispatch waits for the lease... Legion then starts the run.", 2, 2, false},
		{`The page asks the person "Is the run done?"`, 1, 1, false},
		{`The page asks "Is the run done?" Dispatch waits for the answer.`, 2, 2, false},
		{"Dispatch keeps each comment (for one year.) Dispatch indexes each comment.", 2, 2, false},
		{"Dispatch keeps each comment.   ", 1, 1, false},
		{"Dispatch stops! Legion waits? Envoy sends.", 3, 3, false},
		{"Dispatch keeps each comment.Dispatch shows it.", 2, 0, true},
	} {
		line := oneLine(test.body)
		var stops []string
		for _, each := range sentencesOf(line) {
			stops = append(stops, each.stopsWithNoSpace()...)
		}
		if test.stopNoSpace {
			if len(stops) != 1 {
				t.Errorf("%q: the words with a stop and no space are %q, want one", test.body, stops)
			}
			continue
		}
		if len(stops) != 0 {
			t.Errorf("%q: the words with a stop and no space are %q, want none", test.body, stops)
		}
		if got := len(sentencesOf(line)); got != test.rule {
			t.Errorf("%q: the rule counts %d sentences, want %d, and a reader counts %d", test.body, got, test.rule, test.reader)
		}
	}
}

// TestNoStopOfAnInitialismEndsASentence holds the rules of the form to an initialism of single
// letters: none of its stops ends a sentence, also the last one before a space, and the end of
// the text still ends it. So "in the U.S. Dispatch keeps it" is one sentence, and the 25-word
// rule reads the whole of it: 16 and 10 words that "U.S." joins are one sentence of 26 words.
func TestNoStopOfAnInitialismEndsASentence(t *testing.T) {
	for _, body := range []string{
		"Dispatch keeps each record in the U.S. region.",
		"The record is in the U.S. Dispatch keeps it.",
		"Dispatch keeps each record in the U.S.",
	} {
		if _, got := checkStatement(body); len(got) != 0 {
			t.Errorf("Check(%q) = %#v, want none", body, got)
		}
	}
	text, got := checkStatement("Dispatch keeps each record of each issue for one year in one region of the U.S. Dispatch copies each record to a second region each night.")
	if want := []Finding{finding(text, "sentence-words", `a sentence has 26 words, and a sentence has at most 25: "Dispatch keeps each record of each issue for"`)}; !sameFindings(got, want) {
		t.Errorf("Check(%q) = %#v, want %#v", text.Body, got, want)
	}
}
