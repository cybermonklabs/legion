package wordrecords

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// A person's own lines are those that do not start with `>`; a quoted line loses its `>` and the
// space after it, and a text that quotes nothing is its own words as written.
func TestSplitQuoteKeepsThePersonsOwnLines(t *testing.T) {
	cases := []struct {
		name, text, own, quoted string
	}{
		{"no quote", "  What is a store?\n", "  What is a store?\n", ""},
		{"a quote, then a question", "> holds rows that a delete already removed\nWhat does that mean?",
			"What does that mean?", "holds rows that a delete already removed"},
		{"Quote reply's blocks, then typed words", "> The R3 audit decides it.\n>\n> **Ship it.**\n\nWhy?\n",
			"Why?", "The R3 audit decides it.\n\n**Ship it.**"},
		{"typed words, then a quote", "Flip. One thing I can't read here:\n\n> go with B per the train?\n\n",
			"Flip. One thing I can't read here:", "go with B per the train?"},
		{"a quote after three spaces, and code after four", "   > quoted\n    > typed", "    > typed", "quoted"},
		{"a nested quote keeps its inner mark", ">> said before\n> > said too\nOK", "OK", "> said before\n> said too"},
		{"a `>` inside a line", "5 > 3, so yes", "5 > 3, so yes", ""},
		{"a quote alone", "> What is a store?\n\n", "", "What is a store?"},
	}
	for _, c := range cases {
		own, quoted := SplitQuote(c.text)
		if own != c.own || quoted != c.quoted {
			t.Errorf("%s: SplitQuote(%q) = %q, %q; want %q, %q", c.name, c.text, own, quoted, c.own, c.quoted)
		}
	}
}

// The user message gives the person's own lines as their text, and the lines they quote as the
// part their text points to, after the part of a document an anchor quotes.
func TestTheInputGivesQuotedLinesAsThePartThePersonPointsTo(t *testing.T) {
	agent := Text{Kind: "comment", Text: "The mirror holds rows that a delete already removed."}
	got := Input(agent, "", Text{Kind: "comment", Text: "> holds rows that a delete already removed\nWhat does that mean?"})
	want := "<agent_text kind=\"comment\">\nThe mirror holds rows that a delete already removed.\n</agent_text>\n\n" +
		"<quoted_part>\nholds rows that a delete already removed\n</quoted_part>\n\n" +
		"<person_text kind=\"comment\">\nWhat does that mean?\n</person_text>\n"
	if got != want {
		t.Errorf("input = %q\nwant %q", got, want)
	}
	anchored := Input(agent, "the mirror", Text{Kind: "comment", Text: "> a delete\n\nWhich delete?"})
	if !strings.Contains(anchored, "<quoted_part>\nthe mirror\n\na delete\n</quoted_part>") ||
		!strings.Contains(anchored, "<person_text kind=\"comment\">\nWhich delete?\n</person_text>") {
		t.Errorf("an anchored comment that quotes: %q", anchored)
	}
}

// The two rows of INFRA-3231's check 5fb14fc7, through the job's classifier and its stand-in
// model, which reads a question wherever the person's part of the message holds one, quoted or
// not, and the words it asks about from the part the person points to: a quoted clause, then
// "What does that mean?", is a question about the quoted words; "> What is a store?", then
// "Done.", is no question, though the same words typed are one.
func TestTheJobReadsAQuestionOnlyOutsideQuotedLines(t *testing.T) {
	const clause = "holds rows that a delete already removed"
	// The person's part of a request: what stands between its tags.
	personPart := func(r modeltest.Request) string {
		_, rest, _ := strings.Cut(r.Text, "<person_text kind=\"comment\">\n")
		part, _, _ := strings.Cut(rest, "\n</person_text>")
		return part
	}
	stand := modeltest.New(t,
		modeltest.Rule{
			Match: func(r modeltest.Request) bool {
				return strings.Contains(personPart(r), "What does that mean?") &&
					strings.Contains(r.Text, "<quoted_part>\n"+clause+"\n</quoted_part>")
			},
			Reply: modeltest.Reply{Text: `{"verdict":"asks_word_meaning","words":["` + clause + `"]}`},
		},
		modeltest.Rule{
			Match: func(r modeltest.Request) bool { return strings.Contains(personPart(r), "What is a store?") },
			Reply: modeltest.Reply{Text: `{"verdict":"asks_word_meaning","words":["store"]}`},
		},
		modeltest.Rule{Reply: modeltest.Reply{Text: `{"verdict":"none","words":[]}`}},
	)
	classifier := Classifier{Model: agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: agentloop.AnthropicMessages, BaseURL: stand.URL,
			Token: func(context.Context) (string, error) { return "stand-in", nil }},
		ID: "stand-in-records", Effort: "off",
	}}
	agent := Text{Kind: "ask", Text: "The mirror " + clause + ". What is a store, and which one holds the cache?"}
	cases := []struct {
		name, text, verdict string
		words               []string
	}{
		{"a question about a quoted clause", "> " + clause + "\nWhat does that mean?", WordMeaning, []string{clause}},
		{"a quoted question, then no question", "> What is a store?\nDone.", None, []string{}},
		{"the same question typed", "What is a store?", WordMeaning, []string{"store"}},
	}
	for _, c := range cases {
		verdict, err := classifier.Classify(t.Context(), Input(agent, "", Text{Kind: "comment", Text: c.text}))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if verdict.Verdict != c.verdict || !slices.Equal(verdict.Words, c.words) {
			t.Errorf("%s: %s %q, want %s %q", c.name, verdict.Verdict, verdict.Words, c.verdict, c.words)
		}
	}
}
