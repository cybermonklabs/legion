package prose

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// words is the sentence every scope case places, in prose or out of it.
const words = "The cache stores the key."

// holding is the sentences of body whose text holds "cache", the word only words carries.
func holding(body string) []Sentence {
	var found []Sentence
	for _, sentence := range ProseSentences("body", body) {
		if strings.Contains(sentence.Text, "cache") {
			found = append(found, sentence)
		}
	}
	return found
}

// TestProseScopeConstructs reads each prose construct both ways: written as the construct, words
// is a sentence of the construct's kind; written one step outside prose, it is no sentence.
func TestProseScopeConstructs(t *testing.T) {
	cases := []struct {
		name     string
		prose    string
		kind     string
		notProse string
	}{
		{"paragraph", words, KindParagraph, "```\n" + words + "\n```"},
		{"heading", "## " + words, KindHeading, "> ## " + words},
		{"bullet list item", "- " + words, KindListItem, "> - " + words},
		{"ordered list item", "1. " + words, KindListItem, "    1. " + words},
		{"table cell", "| Label | Detail |\n| --- | --- |\n| Clear | " + words + " |",
			KindTableCell, "```\n| Label | Detail |\n| --- | --- |\n| Clear | " + words + " |\n```"},
		{"table header cell", "| " + words + " | Detail |\n| --- | --- |\n| a | b |",
			KindTableCell, "> | " + words + " | Detail |\n> | --- | --- |\n> | a | b |"},
		{"directive paragraph", ":::callout{kind=\"note\"}\n" + words + "\n:::",
			KindDirective, ":::callout{kind=\"note\" title=\"" + words + "\"}\nThe disk holds the log.\n:::"},
		{"list item inside a directive", ":::callout{kind=\"note\"}\n- " + words + "\n:::",
			KindListItem, ":::callout{kind=\"note\"}\n```\n- " + words + "\n```\n:::"},
		{"footnote paragraph", "The disk holds the log.[^1]\n\n[^1]: " + words,
			KindParagraph, "The disk holds the log.[^1]\n\n[^1]: `" + words + "`"},
		{"link text", "Read [" + words + "](https://example.com/guide)",
			KindParagraph, "Read [the guide](https://example.com/the/cache/stores/the/key)"},
		{"text inside inline HTML", "The <b>cache</b> stores the key.",
			KindParagraph, "![" + words + "](cache.png)"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pmdoc.Parse(test.prose); err != nil {
				t.Fatalf("pmdoc.Parse(%q) = %v; the case must reach the tree, not the fallback", test.prose, err)
			}
			found := holding(test.prose)
			if len(found) != 1 || found[0].Kind != test.kind {
				t.Fatalf("ProseSentences(%q) = %+v, want one %s sentence holding the words", test.prose, found, test.kind)
			}
			if found := holding(test.notProse); len(found) != 0 {
				t.Fatalf("ProseSentences(%q) = %+v, want no sentence holding the words", test.notProse, found)
			}
		})
	}
}

// TestProseScopeMovingWordsIntoAParagraph places words inside each construct that is not prose,
// then moves the same words out into a paragraph after it, leaving the construct in place: only the
// moved words are a sentence.
func TestProseScopeMovingWordsIntoAParagraph(t *testing.T) {
	cases := []struct {
		name   string
		inside string
		moved  string
	}{
		{"fenced code", "```\n" + words + "\n```", "```\nx = 1\n```\n\n" + words},
		{"tilde fenced code", "~~~text\n" + words + "\n~~~", "~~~text\nx = 1\n~~~\n\n" + words},
		{"indented code", "    " + words, "    x = 1\n\n" + words},
		{"inline code", "`" + words + "`", "Set `x = 1` first. " + words},
		{"block quote", "> " + words, "> The disk holds the log.\n\n" + words},
		{"nested block quote", "- > " + words, "- > The disk holds the log.\n\n" + words},
		{"link target", "Read [the guide](https://example.com/the/cache/stores/the/key).",
			"Read [the guide](https://example.com/guide). " + words},
		{"bare URL", "https://example.com/the/cache/stores/the/key",
			"Read https://example.com/guide first. " + words},
		{"autolink", "<https://example.com/the/cache/stores/the/key>",
			"Read <https://example.com/guide> first. " + words},
		{"bare email address", "the.cache.stores.the.key@example.com", "Mail ops@example.com first. " + words},
		{"image alt text", "![" + words + "](cache.png)", "![The disk.](disk.png) " + words},
		{"HTML block", "<div>\n" + words + "\n</div>", "<div>\nThe disk holds the log.\n</div>\n\n" + words},
		{"directive attribute", ":::callout{kind=\"note\" title=\"" + words + "\"}\nThe disk holds the log.\n:::",
			":::callout{kind=\"note\" title=\"Read this\"}\nThe disk holds the log.\n:::\n\n" + words},
		{"front matter", "---\ntitle: " + words + "\n---\n\nThe disk holds the log.",
			"---\ntitle: Notes\n---\n\nThe disk holds the log. " + words},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if found := holding(test.inside); len(found) != 0 {
				t.Fatalf("ProseSentences(%q) = %+v, want no sentence holding the words", test.inside, found)
			}
			found := holding(test.moved)
			if len(found) != 1 || found[0].Text != words {
				t.Fatalf("ProseSentences(%q) = %+v, want the sentence %q", test.moved, found, words)
			}
		})
	}
}

// TestProseScopeOperandsStandAsOneWord shows a code span and a bare address kept in their
// sentence as one token each, so the sentence keeps its words and its grammar.
func TestProseScopeOperandsStandAsOneWord(t *testing.T) {
	cases := map[string]string{
		"Combine `left` and `right`.":                            "Combine CODE and CODE.",
		"Use `and` as a label.":                                  "Use CODE as a label.",
		"Send the log to https://example.com/logs now.":          "Send the log to URL now.",
		"Mail ops@example.com or www.example.com today.":         "Mail URL or URL today.",
		"Read [https://example.com](https://example.com) first.": "Read URL first.",
	}
	for body, want := range cases {
		got := ProseSentences("body", body)
		if len(got) != 1 || got[0].Text != want {
			t.Errorf("ProseSentences(%q) = %+v, want one sentence %q", body, got, want)
		}
	}
	if got := ProseSentences("body", "Run this.\n\n`make test`\n\n<https://example.com>"); len(got) != 1 {
		t.Errorf("a paragraph of only a token yields %+v, want only the sentence of the first paragraph", got)
	}
}

// TestProseScopeSentencesAndOffsets checks the fields of each sentence: the field it carries, the
// segment it stands in, its offsets in the segment's text and whether it ends the segment.
func TestProseScopeSentencesAndOffsets(t *testing.T) {
	body := "# The disk\n\nThe cache stores the key. The disk holds the log.\n\n" +
		"- Start the task and\n\n  record the result.\n  - Stop the task or\n"
	want := []Sentence{
		{Field: "description", Segment: 0, Kind: KindHeading, Text: "The disk", Start: 0, End: 8, Last: true},
		{Field: "description", Segment: 1, Kind: KindParagraph, Text: words, Start: 0, End: 25},
		{Field: "description", Segment: 1, Kind: KindParagraph, Text: "The disk holds the log.", Start: 26, End: 49, Last: true},
		// An item's paragraphs are one segment, joined by a blank line in its text.
		{Field: "description", Segment: 2, Kind: KindListItem, Text: "Start the task and", Start: 0, End: 18},
		{Field: "description", Segment: 2, Kind: KindListItem, Text: "record the result.", Start: 20, End: 38, Last: true},
		// A nested list's item is its own segment.
		{Field: "description", Segment: 3, Kind: KindListItem, Text: "Stop the task or", Start: 0, End: 16, Last: true},
	}
	if got := ProseSentences("description", body); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProseSentences = %+v\nwant %+v", got, want)
	}
}

// TestProseScopeSegmenterKnowsAbbreviations shows the sentences come from the Punkt model and not
// from a split at each full stop.
func TestProseScopeSegmenterKnowsAbbreviations(t *testing.T) {
	got := ProseSentences("body", "Use a short name, e.g. the key. The disk holds 3.5 GB.")
	if len(got) != 2 || got[0].Text != "Use a short name, e.g. the key." || got[1].Text != "The disk holds 3.5 GB." {
		t.Fatalf("ProseSentences = %+v, want two sentences", got)
	}
}

// TestProseScopeRefusedBodyFallsBackToLines reads a body the parser refuses by the line scan: its
// paragraphs are prose, and its fenced code, HTML block, quoted lines and directive fences are not.
func TestProseScopeRefusedBodyFallsBackToLines(t *testing.T) {
	body := "<div>\nThe cache is in the HTML.\n</div>\n\n" +
		"```\nThe cache is in the code.\n```\n\n" +
		"> The cache is in the quote.\n\n" +
		":::unknown{title=\"The cache is in the fence.\"}\n" +
		"The disk holds the log. Run `the cache` now.\n" +
		":::\n"
	if _, err := pmdoc.Parse(body); err == nil {
		t.Fatal("pmdoc.Parse accepts the body; the test must reach the fallback")
	}
	want := []Sentence{
		{Field: "body", Segment: 0, Kind: KindParagraph, Text: "The disk holds the log.", Start: 0, End: 23},
		{Field: "body", Segment: 0, Kind: KindParagraph, Text: "Run CODE now.", Start: 24, End: 37, Last: true},
	}
	if got := ProseSentences("body", body); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProseSentences = %+v\nwant %+v", got, want)
	}
}

func TestProseScopeEmptyBody(t *testing.T) {
	for _, body := range []string{"", " \n\n\t", "```\n```", "![](x.png)"} {
		if got := ProseSentences("body", body); len(got) != 0 {
			t.Errorf("ProseSentences(%q) = %+v, want none", body, got)
		}
	}
}
