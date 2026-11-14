package pmdoc

import (
	"slices"
	"strings"
	"testing"
)

// The shapes below were the browser balancer's own, one Go test per shape its suite covered,
// when both readings of a cut snippet existed and had to agree. The browser's is gone with the
// rest of its markdown parsing, so these are now the whole record of what a cut snippet must
// come back as. They come from ts_headline with search.go's options (MaxWords=24, MinWords=12,
// MaxFragments=1) run on a real Postgres.

func balanced(t *testing.T, fragment, want string) {
	t.Helper()
	if got := BalanceCutMarkdown(fragment); got != want {
		t.Errorf("BalanceCutMarkdown(%q) = %q, want %q", fragment, got, want)
	}
}

// ts_headline on `… twenty **anchor** after the mark …` with a 24-word window starts the window
// after the opener and ends it after the closer, so only the `**` after `anchor` survives, and
// the balancer drops exactly that.
func TestASnippetCutAfterABoldSpanOpenedLosesOnlyTheDelimiterTheCutLeftOpen(t *testing.T) {
	balanced(t, "twenty anchor** after the mark twentyone", "twenty anchor after the mark twentyone")
	balanced(t,
		"stop the deploy before the cut and report back** eleven",
		"stop the deploy before the cut and report back eleven",
	)
	// The same for single emphasis, which CommonMark also leaves literal when unmatched.
	balanced(t, "check this thing* before you report back", "check this thing before you report back")
	balanced(t, "Please check *this thing before you report", "Please check this thing before you report")
}

// A delimiter the author typed on purpose stays: one CommonMark would read as literal. `** ` is
// not at either edge of a word and cannot open emphasis, so it is text, not a cut.
func TestADelimiterCommonMarkReadsAsLiteralStays(t *testing.T) {
	balanced(t, "Use ** for bold", "Use ** for bold")
	balanced(t, "press the ` key then", "press the ` key then")
	balanced(t, "2 * 3 is 6", "2 * 3 is 6")
	balanced(t, "snake_case and __init__", "snake_case and __init__")
}

// A tilde run pairs only with a closer its own length, as GFM's strikethrough does: remark-gfm
// reads `~~a~` as plain text, and leaving `~a~` would strike out text the author never struck.
func TestATildeRunPairsOnlyWithACloserItsOwnLength(t *testing.T) {
	balanced(t, "keep ~~a~ here", "keep a here")
	// A run of three or more is literal text to GFM (the third marker ends the sequence), so the
	// `~~` closer is the only delimiter here, and the cut is what took its opener.
	balanced(t, "x ~~~a~~ y", "x ~~~a y")
	// Runs that do pair are the author's, at either length.
	balanced(t, "a ~~struck~~ and ~one~ here", "a ~~struck~~ and ~one~ here")
	// A cut that took the closer leaves the opener to drop, as it does for `**`.
	balanced(t, "report ~~struck text the cut", "report struck text the cut")
}

// Delimiters inside a code span are code, never counted.
func TestDelimitersInsideACodeSpanAreNeverCounted(t *testing.T) {
	balanced(t, "`code with ** inside` and **bold**", "`code with ** inside` and **bold**")
	balanced(t, "call `f(**kwargs)` with", "call `f(**kwargs)` with")
	balanced(t, "``a ` tick`` stays", "``a ` tick`` stays")
}

// A snippet cut inside a link keeps the link's words.
func TestASnippetCutInsideALinkKeepsTheLinksWords(t *testing.T) {
	balanced(t, "read it and [link", "read it and link")
	balanced(t, "read [the docs](https://example.com/do", "read the docs")
	balanced(t, "docs](https://example.com/docs) and then", "docs and then")
}

// A fragment whose marks are balanced is left exactly as it is.
func TestABalancedFragmentIsLeftExactlyAsItIs(t *testing.T) {
	whole := "**Bold** then `code` and [a link](https://example.com) and ~~old~~ __u__ *i* _j_"
	balanced(t, whole, whole)
	balanced(t, "", "")
}

// A fragment cut inside a code span drops the open backtick.
func TestAFragmentCutInsideACodeSpanDropsTheOpenBacktick(t *testing.T) {
	balanced(t, "Before the cut `dispatch-deploy.y", "Before the cut dispatch-deploy.y")
}

// The sentinels ts_headline writes around what a search matched (search.go's markStart and
// markEnd), and CORE-1 as it marks a search for CORE-1: word by word (webMarkedCore1).
const (
	snippetStart = "\uE000"
	snippetStop  = "\uE001"
	snippetCore1 = snippetStart + "CORE" + snippetStop + snippetStart + "-1" + snippetStop
)

// snippetText is one text node of a snippet's display read: its text without the sentinels, the
// types of its marks, and the type of the node holding it.
type snippetText struct {
	text   string
	marks  []string
	parent string
}

// snippetTexts is every text node of markdown read for display, in document order.
func snippetTexts(markdown string) []snippetText {
	var texts []snippetText
	var walk func(node *Node)
	walk = func(node *Node) {
		for _, child := range node.Children {
			if child.Type != "text" {
				walk(child)
				continue
			}
			text := snippetText{
				text:   strings.NewReplacer(snippetStart, "", snippetStop, "").Replace(child.Text),
				parent: node.Type,
			}
			for _, mark := range child.Marks {
				text.marks = append(text.marks, mark.Type)
			}
			texts = append(texts, text)
		}
	}
	walk(ReadForDisplay(markdown, DisplayLatest))
	return texts
}

// snippetTextHolding is the text node of markdown's display read that holds word, and whether
// there is one.
func snippetTextHolding(markdown, word string) (snippetText, bool) {
	for _, text := range snippetTexts(markdown) {
		if strings.Contains(text.text, word) {
			return text, true
		}
	}
	return snippetText{}, false
}

// A search hit's snippet is the headline set back into the source ts_headline read and widened
// to the source's own constructs. Each headline below is exactly what ts_headline answered for
// its source with search.go's headlineOptions, measured on PostgreSQL 18.6 (INFRA-1702); the
// check beside each is what the snippet must read as.
func TestASearchSnippetReadsAsTheSourceItQuotes(t *testing.T) {
	for _, test := range []struct {
		name, source, headline, want string
		check                        func(t *testing.T, snippet string)
	}{
		{
			// ts_headline drops `Run` and the opening backtick, so CORE-1 alone read as prose.
			name:     "a code span in a short body",
			source:   "Run `CORE-1` locally please",
			headline: snippetCore1 + "` locally please",
			want:     "Run `" + snippetCore1 + "` locally please",
			check:    inCodeSpan("CORE-1"),
		},
		{
			// The window starts inside the span; the balancer alone would drop its closing tick.
			name: "a code span the window starts inside",
			source: "Before you ship the change to the staging cluster for the platform team please run " +
				"`make check --verbose --race --count one two three four five six seven eight nine ten CORE-1 locally` on a clean tree",
			headline: "count one two three four five six seven eight nine ten " + snippetCore1 + " locally` on a clean tree",
			want: "`make check --verbose --race --count one two three four five six seven eight nine ten " +
				snippetCore1 + " locally` on a clean tree",
			check: inCodeSpan("CORE-1"),
		},
		{
			name:     "a worded link cut at its text in a short body",
			source:   "See [the gyroscope](dispatch://CORE-1) now",
			headline: snippetStart + "gyroscope" + snippetStop + "](dispatch://CORE",
			want:     "See [the " + snippetStart + "gyroscope" + snippetStop + "](dispatch://CORE-1) now",
			check:    inLink("the gyroscope"),
		},
		{
			name: "a worded link cut at its text in a long body",
			source: "We spent the whole morning going over the plan for next quarter with everyone. See [the old and " +
				"very much broken spare part of the ship gyroscope alignment rig](dispatch://CORE-1) now and then " +
				"report back to the platform team before the end of the week please.",
			headline: "very much broken spare part of the ship " + snippetStart + "gyroscope" + snippetStop +
				" alignment rig](dispatch://CORE-1) now and then report back to the platform team before",
			want: "[the old and very much broken spare part of the ship " + snippetStart + "gyroscope" + snippetStop +
				" alignment rig](dispatch://CORE-1) now and then report back to the platform team before",
			check: inLink("the old and very much broken spare part of the ship gyroscope alignment rig"),
		},
		{
			name:     "a bare address cut inside the address in a short body",
			source:   "Ask dispatch://person/alice first",
			headline: "person/alice " + snippetStart + "first" + snippetStop,
			want:     "Ask dispatch://person/alice " + snippetStart + "first" + snippetStop,
			check:    readsText("dispatch://person/alice"),
		},
		{
			name: "a bare address cut inside the address in a long body",
			source: "Ask dispatch://person/alice first and then the platform team about the deploy checklist the " +
				"release notes the migration plan and the rollback steps before the window closes tonight please thanks again everyone",
			headline: "person/alice " + snippetStart + "first" + snippetStop + " and then the platform team about the " +
				"deploy checklist the release notes the migration plan and the rollback steps before the window",
			want: "dispatch://person/alice " + snippetStart + "first" + snippetStop + " and then the platform team about the " +
				"deploy checklist the release notes the migration plan and the rollback steps before the window",
			check: readsText("dispatch://person/alice"),
		},
		{
			// A short body's headline less its first word reads as the whole body again.
			name:     "the first word dropped from a short body",
			source:   "The release is blocked on CORE-99 until it lands",
			headline: "release is blocked on " + snippetStart + "CORE" + snippetStop + snippetStart + "-99" + snippetStop + " until it lands",
			want: "The release is blocked on " + snippetStart + "CORE" + snippetStop + snippetStart + "-99" + snippetStop +
				" until it lands",
			check: readsText("The release is blocked on CORE-99 until it lands"),
		},
		{
			// The window starts on an indented line: the snippet keeps the lines' indentation.
			name: "an indented code block",
			source: "Run these on the build host when the deploy finishes.\n\n    step one alpha bravo charlie\n" +
				"    step two delta echo foxtrot\n    step three golf hotel india\n    run gyroscope calibrate now\n" +
				"    step four juliet kilo lima\n    step five mike november oscar\n\nThen report back.",
			headline: "step two delta echo foxtrot\n    step three golf hotel india\n    run " + snippetStart + "gyroscope" +
				snippetStop + " calibrate now\n    step four juliet kilo lima\n    step five mike november oscar",
			want: "    step two delta echo foxtrot\n    step three golf hotel india\n    run " + snippetStart + "gyroscope" +
				snippetStop + " calibrate now\n    step four juliet kilo lima\n    step five mike november oscar",
			check: inCodeBlock("gyroscope"),
		},
		{
			// The window starts past the fence: the snippet puts the fence line back in front.
			name: "a fenced code block",
			source: "Run these on the build host when the deploy finishes.\n\n```sh\nstep one alpha bravo charlie\n" +
				"step two delta echo foxtrot\nstep three golf hotel india\nrun gyroscope calibrate now\n" +
				"step four juliet kilo lima\nstep five mike november oscar\n```\n\nThen report back.",
			headline: "step two delta echo foxtrot\nstep three golf hotel india\nrun " + snippetStart + "gyroscope" +
				snippetStop + " calibrate now\nstep four juliet kilo lima\nstep five mike november oscar",
			want: "```sh\nstep two delta echo foxtrot\nstep three golf hotel india\nrun " + snippetStart + "gyroscope" +
				snippetStop + " calibrate now\nstep four juliet kilo lima\nstep five mike november oscar",
			check: inCodeBlock("gyroscope"),
		},
		{
			// The window starts inside a bold run: the widened cut is still a cut, and the
			// balancer drops the closer whose opener it left out.
			name: "a long body cut in the middle of prose",
			source: "Some words open the comment before the bold part. **one two three four five six seven eight nine " +
				"ten eleven twelve thirteen fourteen fifteen sixteen the new anchor build today** and then more words " +
				"after it for the rest of the week until friday comes around again",
			headline: "eight nine ten eleven twelve thirteen fourteen fifteen sixteen the new " + snippetStart + "anchor" +
				snippetStop + " build today** and then more words after it for the rest",
			want: "eight nine ten eleven twelve thirteen fourteen fifteen sixteen the new " + snippetStart + "anchor" +
				snippetStop + " build today and then more words after it for the rest",
			check: readsNoBold,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := SearchSnippet(test.headline, test.source, snippetStart, snippetStop)
			if got != test.want {
				t.Fatalf("SearchSnippet(%q, %q) = %q, want %q", test.headline, test.source, got, test.want)
			}
			test.check(t, got)
		})
	}
}

// A headline SearchSnippet cannot find in its source is read alone, as the snippet was read
// before the source was: whole when it is shorter than the window, balanced when it is the window
// wide. ts_headline returns a short body whole (measured: `Use ** for bold and anchor here` ->
// `bold and <<anchor>> here`, 4 words), so the author's `**` is theirs; a window of 24 words
// around the match with the mark's opener left outside it loses the closer.
func TestASearchSnippetNotInItsSourceIsReadAlone(t *testing.T) {
	const elsewhere = "a source the headline was not cut from"
	short := "bold and ** " + snippetStart + "anchor" + snippetStop + " here"
	if got := SearchSnippet(short, elsewhere, snippetStart, snippetStop); got != short {
		t.Errorf("a four-word headline was balanced: %q", got)
	}
	window := strings.Repeat("word ", 21) + "anchor** after the"
	if words := len(strings.Split(window, " ")); words != 24 {
		t.Fatalf("the window holds %d words, want 24", words)
	}
	if got, want := SearchSnippet(window, elsewhere, snippetStart, snippetStop), strings.Replace(window, "**", "", 1); got != want {
		t.Errorf("SearchSnippet(window) = %q, want %q", got, want)
	}
	if got := SearchSnippet("", "", snippetStart, snippetStop); got != "" {
		t.Errorf("no headline became the snippet %q", got)
	}
}

// inCodeSpan checks that word reads as code text in an inline code mark, and is no link.
func inCodeSpan(word string) func(*testing.T, string) {
	return func(t *testing.T, snippet string) {
		t.Helper()
		text, ok := snippetTextHolding(snippet, word)
		if !ok {
			t.Fatalf("%q reads no text holding %q: %+v", snippet, word, snippetTexts(snippet))
		}
		if !slices.Contains(text.marks, "inlineCode") || slices.Contains(text.marks, "link") {
			t.Errorf("%q reads %q with the marks %v, want code and no link", snippet, word, text.marks)
		}
	}
}

// inLink checks that words read whole as the text of a link.
func inLink(words string) func(*testing.T, string) {
	return func(t *testing.T, snippet string) {
		t.Helper()
		var linked strings.Builder
		for _, text := range snippetTexts(snippet) {
			if slices.Contains(text.marks, "link") {
				linked.WriteString(text.text)
			}
		}
		if linked.String() != words {
			t.Errorf("%q reads the link text %q, want %q", snippet, linked.String(), words)
		}
	}
}

// readsText checks that the snippet's text, read for display, holds words whole.
func readsText(words string) func(*testing.T, string) {
	return func(t *testing.T, snippet string) {
		t.Helper()
		var read strings.Builder
		for _, text := range snippetTexts(snippet) {
			read.WriteString(text.text)
		}
		if !strings.Contains(read.String(), words) {
			t.Errorf("%q reads as %q, which does not hold %q", snippet, read.String(), words)
		}
	}
}

// inCodeBlock checks that word reads inside a code block.
func inCodeBlock(word string) func(*testing.T, string) {
	return func(t *testing.T, snippet string) {
		t.Helper()
		text, ok := snippetTextHolding(snippet, word)
		if !ok {
			t.Fatalf("%q reads no text holding %q: %+v", snippet, word, snippetTexts(snippet))
		}
		if text.parent != "code_block" {
			t.Errorf("%q reads %q in a %s, want a code block", snippet, word, text.parent)
		}
	}
}

// readsNoBold checks that no text of the snippet is bold and none holds a `**` left over.
func readsNoBold(t *testing.T, snippet string) {
	t.Helper()
	for _, text := range snippetTexts(snippet) {
		if slices.Contains(text.marks, "strong") || strings.Contains(text.text, "**") {
			t.Errorf("%q reads %q with the marks %v, want no bold", snippet, text.text, text.marks)
		}
	}
}
