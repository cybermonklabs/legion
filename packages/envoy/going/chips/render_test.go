package chips

import (
	"reflect"
	"testing"
)

// TestDrawnWordsCountsAnAnswerAsTheBarDrawsIt: an answer of the command bar's agent has fewer than
// 120 words as the bar draws them, and in a preview at 8a3e562b 6 of 11 answers drew 120 or more
// once their chips were counted, though the model wrote fewer. So the count is of the words the
// bar draws: a link to an issue counts its chip's key, status and title, and not the link's own
// words; a time link counts its words alone, the date on its hover being drawn in no text, or its
// issue's chip where its words are the issue's key; a link to an item the store does not hold
// counts its bare reference, and a web address the link's words. Marks that stand alone - a
// list's dashes, a heading's hashes, a chip's separators - count as no words, and a word with
// marks around it counts once.
func TestDrawnWordsCountsAnAnswerAsTheBarDrawsIt(t *testing.T) {
	for _, c := range []struct {
		name, answer string
		want         int
	}{
		{"plain words and marks", "**Done:** two words\n\n- one\n- two\n\n# Heading", 6},
		{"an issue's chip in place of the link's words", "[the key rotation work of last quarter](dispatch://OPS-4) waits.", 8},
		{"an issue's chip by its dashboard path", "See [OPS-4](/issues/OPS-4).", 8},
		{"a time link's words, not its hover's date", "Moved [5 days ago](dispatch-time:2026-11-18T11:35:00Z).", 4},
		{"a time link whose words are a key", "[OPS-4](dispatch-time:2026-10-10T09:00:00Z) moved.", 8},
		{"an issue the store does not hold", "[DEMO-404, closed last week](dispatch://DEMO-404) is gone.", 3},
		{"a web address", "[the runbook](https://example.com/runbook) says so.", 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := DrawnWords(c.answer, keptItems); got != c.want {
				t.Errorf("DrawnWords(%q) = %d, want %d: the bar draws %q", c.answer, got, c.want, render(c.answer, keptItems, false))
			}
		})
	}
	const timed = "Moved [5 days ago](dispatch-time:2026-11-18T11:35:00Z)."
	if got, want := RenderAnswer(timed, keptItems), "Moved 5 days ago (18 Nov 2026, 11:35 UTC)."; got != want {
		t.Errorf("RenderAnswer(%q) = %q, want %q: the judge reads the date its hover shows", timed, got, want)
	}
}

// TestDropCodeBlocksTakesOutTheBlocksItIsToldTo: a fenced code block that drop names is taken out
// with its fences, an unclosed one to the end of the answer, and drop is given each block's info
// string and its body. Every other block and all other text stay byte for byte, and an answer
// with no block to take out is answered as it came.
func TestDropCodeBlocksTakesOutTheBlocksItIsToldTo(t *testing.T) {
	type block struct{ info, body string }
	var given []block
	drawOnly := func(info, body string) bool {
		given = append(given, block{info, body})
		return info == "draw"
	}
	const answer = "DEMO's issues, grouped by status:\n\n```draw\n{\"kind\":\"chart\"}\n```\n\nThe query:\n\n  ~~~~ sql\nselect 1;\n~~~~\n"
	if got, want := DropCodeBlocks(answer, drawOnly), "DEMO's issues, grouped by status:\n\n\nThe query:\n\n  ~~~~ sql\nselect 1;\n~~~~\n"; got != want {
		t.Errorf("DropCodeBlocks(%q)\n= %q\nwant %q", answer, got, want)
	}
	if want := []block{{"draw", "{\"kind\":\"chart\"}\n"}, {"sql", "select 1;\n"}}; !reflect.DeepEqual(given, want) {
		t.Errorf("drop was given the blocks %q, want %q", given, want)
	}
	const unclosed = "Here it is:\n```draw\n{\"kind\":\"metric\"}"
	if got, want := DropCodeBlocks(unclosed, drawOnly), "Here it is:\n"; got != want {
		t.Errorf("DropCodeBlocks(%q) = %q, want %q", unclosed, got, want)
	}
	const kept = "Run it:\n\n```sh\nmake test\n```\nand `draw` it."
	if got := DropCodeBlocks(kept, drawOnly); got != kept {
		t.Errorf("DropCodeBlocks(%q) = %q, want it as it came", kept, got)
	}
}
