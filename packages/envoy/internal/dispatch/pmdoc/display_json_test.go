package pmdoc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
)

// The display tree a response carries is ReadForDisplay's own tree as the wire encoding writes
// it, encoded once and kept by the text it was read from, so two readers of one comment are
// served the same bytes. The read mints no block id, so a second read of one text is the same
// bytes without a generator being held still for it.
func TestDisplayJSONIsTheDisplayReadsTreeAndIsKept(t *testing.T) {
	const markdown = "A **bold** line nothing else in this package reads.\n"
	tree := DisplayJSON(markdown, DisplayLatest)
	want, err := json.Marshal(ReadForDisplay(markdown, DisplayLatest).toDisplayJSON())
	if err != nil {
		t.Fatalf("encode the display read: %v", err)
	}
	if !bytes.Equal(tree, want) {
		t.Errorf("DisplayJSON =\n\t%s\nwant ReadForDisplay's\n\t%s", tree, want)
	}
	if again := DisplayJSON(markdown, DisplayLatest); &again[0] != &tree[0] {
		t.Errorf("a second read of the same text built the tree again")
	}
	if DisplayJSON("", DisplayLatest) != nil {
		t.Errorf("an empty text was read as a tree")
	}
}

// The cache the display trees are kept in holds at most what it is given room for, counted as
// the bytes of the JSON it holds, and the least recently read entry is the one that leaves. The
// cache itself is shared (bytelru), which has its own tests; this one holds the trees to the
// bound DisplayCacheBytes promises, under the key this package builds.
func TestTheDisplayCacheHoldsItsBound(t *testing.T) {
	const limit int64 = 64
	cache := bytelru.New[displayCacheKey, json.RawMessage](limit)
	first := displayCacheKey{sum: [32]byte{1}}
	second := displayCacheKey{sum: [32]byte{2}}
	third := displayCacheKey{sum: [32]byte{3}}
	cache.Put(first, make(json.RawMessage, 32))
	cache.Put(second, make(json.RawMessage, 32))
	if _, held := cache.Get(first); !held {
		t.Fatalf("the first tree left while the cache still had room")
	}
	// Reading the first put the second at the back, so the third evicts it.
	cache.Put(third, make(json.RawMessage, 32))
	if _, held := cache.Get(second); held {
		t.Errorf("the least recently read tree stayed")
	}
	if _, held := cache.Get(first); !held {
		t.Errorf("the most recently read tree left")
	}
	if cache.Held() > limit {
		t.Errorf("the cache holds %d bytes, over its %d bound", cache.Held(), limit)
	}
	cache.Put(displayCacheKey{sum: [32]byte{4}}, make(json.RawMessage, 65))
	if _, held := cache.Get(displayCacheKey{sum: [32]byte{4}}); held {
		t.Errorf("a tree larger than the whole cache was kept")
	}
}

// A search snippet's tree is the snippet read for display with the match marked: the sentinels
// are gone from the text and the words between them carry DisplayHighlightMark.
func TestDisplaySnippetJSONMarksWhatTheQueryMatched(t *testing.T) {
	const start, stop = "\uE000", "\uE001"
	tree := DisplaySnippetJSON("Keep the "+start+"astrolabe"+stop+" here", start, stop)
	var root struct {
		Content []struct {
			Content []struct {
				Text  string `json:"text"`
				Marks []struct {
					Type string `json:"type"`
				} `json:"marks"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	if len(root.Content) != 1 {
		t.Fatalf("the snippet read as %d blocks, want one paragraph: %s", len(root.Content), tree)
	}
	var text, highlighted strings.Builder
	for _, piece := range root.Content[0].Content {
		text.WriteString(piece.Text)
		for _, mark := range piece.Marks {
			if mark.Type == DisplayHighlightMark {
				highlighted.WriteString(piece.Text)
			}
		}
	}
	if text.String() != "Keep the astrolabe here" {
		t.Errorf("the snippet's text is %q, want it without the sentinels", text.String())
	}
	if highlighted.String() != "astrolabe" {
		t.Errorf("the snippet highlights %q, want the matched word", highlighted.String())
	}
}

// A headline the window cut is balanced by SearchSnippet before DisplaySnippetJSON reads it, so
// the palette is shown no delimiter the parser would have paired across the cut. The headline is
// ts_headline's for this source (fragment_test.go, "a long body cut in the middle of prose").
func TestDisplaySnippetJSONReadsTheBalancedSnippet(t *testing.T) {
	const start, stop = "\uE000", "\uE001"
	source := "Some words open the comment before the bold part. **one two three four five six seven eight nine " +
		"ten eleven twelve thirteen fourteen fifteen sixteen the new anchor build today** and then more words " +
		"after it for the rest of the week until friday comes around again"
	headline := "eight nine ten eleven twelve thirteen fourteen fifteen sixteen the new " + start + "anchor" + stop +
		" build today** and then more words after it for the rest"
	tree := DisplaySnippetJSON(SearchSnippet(headline, source, start, stop), start, stop)
	if bytes.Contains(tree, []byte("**")) {
		t.Errorf("the cut's unmatched delimiter reached the tree: %s", tree)
	}
	if bytes.Contains(tree, []byte("strong")) {
		t.Errorf("the cut's unmatched delimiter opened a mark: %s", tree)
	}
}

// snippetRun is one text node of a snippet tree's first paragraph: its text, the href of its link
// mark where it has one, and whether it carries DisplayHighlightMark.
type snippetRun struct {
	text        string
	href        string
	highlighted bool
}

// snippetRuns decodes a one-paragraph snippet tree into its runs.
func snippetRuns(t *testing.T, tree []byte) []snippetRun {
	t.Helper()
	var root struct {
		Content []struct {
			Content []struct {
				Text  string `json:"text"`
				Marks []struct {
					Type  string `json:"type"`
					Attrs struct {
						Href string `json:"href"`
					} `json:"attrs"`
				} `json:"marks"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	if len(root.Content) != 1 {
		t.Fatalf("the snippet read as %d blocks, want one paragraph: %s", len(root.Content), tree)
	}
	var runs []snippetRun
	for _, piece := range root.Content[0].Content {
		run := snippetRun{text: piece.Text}
		for _, mark := range piece.Marks {
			switch mark.Type {
			case "link":
				run.href = mark.Attrs.Href
			case DisplayHighlightMark:
				run.highlighted = true
			}
		}
		runs = append(runs, run)
	}
	return runs
}

// A search that matched a link's address marks that address in the link's href as well as in its
// words: ts_headline reads `[dispatch://CORE-1](dispatch://CORE-1)`, which is how a document
// stores the autolink `<dispatch://CORE-1>`, as words like any other. The headlines below are
// PostgreSQL 18's own for these sources searched for CORE with headlineOptions (measured
// 2026-11-14, INFRA-1702). Only the words carry the highlight: the href is the address itself, so
// the browser still reads it as a reference, and the sentinels in it start and stop nothing, so
// the words after the link stay plain.
func TestASnippetMarksALinksWordsAndNeverItsAddress(t *testing.T) {
	const start, stop = "\uE000", "\uE001"
	marked := start + "CORE" + stop
	for _, test := range []struct {
		name, source, headline, text, highlighted string
	}{
		{
			name:        "an autolink",
			source:      "See [dispatch://CORE-1](dispatch://CORE-1) since Monday.",
			headline:    marked + "-1](dispatch://" + marked + "-1) since Monday",
			text:        "See dispatch://CORE-1 since Monday.",
			highlighted: "CORE",
		},
		{
			name:     "a worded link",
			source:   "See [the design](dispatch://CORE-1) since Monday.",
			headline: "design](dispatch://" + marked + "-1) since Monday",
			text:     "See the design since Monday.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snippet := SearchSnippet(test.headline, test.source, start, stop)
			tree := DisplaySnippetJSON(snippet, start, stop)
			var text, highlighted strings.Builder
			linked := false
			for _, run := range snippetRuns(t, tree) {
				text.WriteString(run.text)
				if run.highlighted {
					highlighted.WriteString(run.text)
				}
				if run.href == "" {
					continue
				}
				linked = true
				if run.href != "dispatch://CORE-1" {
					t.Errorf("the link over %q points at %q, want dispatch://CORE-1", run.text, run.href)
				}
			}
			if !linked {
				t.Errorf("the snippet lost its link: %s", tree)
			}
			if text.String() != test.text {
				t.Errorf("the snippet reads %q, want %q", text.String(), test.text)
			}
			if highlighted.String() != test.highlighted {
				t.Errorf("the snippet highlights %q, want %q: %s", highlighted.String(), test.highlighted, tree)
			}
		})
	}
}

// No sentinel survives into a snippet's tree, wherever in the markdown the query matched: every
// attribute a reading fills from the text - an address, a caption, a title, a code block's
// language, inline HTML, a footnote's label - is shown the reader as something other than marked
// words, so the highlight is taken out of it rather than drawn.
func TestNoSentinelReachesASnippetsAttributes(t *testing.T) {
	const start, stop = "\uE000", "\uE001"
	marked := start + "CORE" + stop
	for _, markdown := range []string{
		"See [dispatch://" + marked + "-1](dispatch://" + marked + "-1 \"" + marked + "\") now.",
		"See <https://example.com/" + marked + "> now.",
		"See https://example.com/" + marked + " now.",
		"![shot " + marked + "](dispatch://" + marked + "-1/artifact/shot-png@v1 \"a " + marked + "\")",
		"```" + start + "go" + stop + "\nx := 1\n```\n",
		"A <span title=\"" + marked + "\">x</span> here",
		"A note[^" + marked + "].\n\n[^" + marked + "]: The " + marked + " note.\n",
	} {
		tree := DisplaySnippetJSON(markdown, start, stop)
		if bytes.Contains(tree, []byte(start)) || bytes.Contains(tree, []byte(stop)) {
			t.Errorf("a sentinel of %q reached the tree: %s", markdown, tree)
		}
	}
}
