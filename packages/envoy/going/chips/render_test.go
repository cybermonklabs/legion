package chips

import (
	"reflect"
	"testing"
)

// TestRenderAnswerWritesEachLinkAsTheBarDrawsIt: the judge reads an answer as the bar draws it:
// the first link to an issue is the issue's chip, and a link to another item with words of its
// own, such as [its spec](dispatch://OPS-4/spec), is those words, where RenderAnswer wrote the
// item's words, the issue's title, which the bar does not draw. A link to another item whose words
// are its address is what the bar draws for the item, and a later link to an issue that has its
// chip, a time link whose words are its key among them, is its words, the time link's with the
// date its hover shows, as every time link's words are.
func TestRenderAnswerWritesEachLinkAsTheBarDrawsIt(t *testing.T) {
	for _, c := range []struct{ name, answer, want string }{
		{
			"a time link's words with the date its hover shows",
			"Moved [5 days ago](dispatch-time:2026-11-18T11:35:00Z).",
			"Moved 5 days ago (18 Nov 2026, 11:35 UTC).",
		},
		{
			"a link to an issue's spec with words of its own",
			"Read [its spec](dispatch://OPS-4/spec).",
			"Read its spec.",
		},
		{
			"a link to a document with words of its own, and one whose words are its address",
			"See [the handbook](dispatch://OPS/artifact/oncall-handbook) or <dispatch://OPS/artifact/oncall-handbook>.",
			"See the handbook or [On-call handbook].",
		},
		{
			"later links to an issue that has its chip",
			"[OPS-4](dispatch://OPS-4) moved [OPS-4](dispatch-time:2026-10-10T09:00:00Z), and [the rotation](/issues/OPS-4) waits.",
			"[OPS-4 · In progress · Rotate the signing keys] moved OPS-4 (10 Oct 2026, 09:00 UTC), and the rotation waits.",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderAnswer(c.answer, keptItems); got != c.want {
				t.Errorf("RenderAnswer(%q) = %q, want %q", c.answer, got, c.want)
			}
		})
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
