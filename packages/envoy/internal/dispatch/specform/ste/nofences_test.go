package ste

import (
	"reflect"
	"testing"
)

// NoFences writes each fence marker of a text as white space, byte for byte: the run of backticks
// or tildes at the start of a line, the word right after it, such as a language, and the next run
// of as many on the line, which closes it. So Lint reads each line of what NoFences writes, and the
// words after a marker are words. A run inside a line, and two backticks, are no fence marker, and
// NoFences leaves them. The control: Lint reads the same text with its markers as fences, and finds
// nothing.
func TestNoFencesWritesEachFenceMarkerAsWhiteSpace(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{"```sh Run it. ```", "      Run it.    "},
		{"~~~ a; b", "    a; b"},
		{"  ~~~python a; b ~~~ c", "            a; b     c"},
		{"``` ``` a", "        a"},
		{"``` ```` a", "         a"},
		{"One.\n```sh\nTwo.\n```", "One.\n     \nTwo.\n   "},
		{"One.\u2028\x1c~~~\u2028Two.", "One.\u2028\x1c   \u2028Two."},
		{"The `x` stays, and so does a ``` in a line.", "The `x` stays, and so does a ``` in a line."},
		{"`` two backticks open no fence", "`` two backticks open no fence"},
	} {
		got := NoFences(test.text)
		if got != test.want {
			t.Errorf("NoFences(%q) = %q, want %q", test.text, got, test.want)
		}
		if len(got) != len(test.text) {
			t.Errorf("NoFences(%q) has %d bytes, want %d", test.text, len(got), len(test.text))
		}
	}
	const fenced = "```sh\nx; y\n```"
	if got := Lint(fenced); len(got) != 0 {
		t.Errorf("the control: Lint(%q) = %v, want none, as ste-lint reads a fence", fenced, got)
	}
	if got := Lint(NoFences(fenced)); !sameFindings(got, []Finding{{"semicolon", ";"}}) {
		t.Errorf("Lint(NoFences(%q)) = %v, want the semicolon", fenced, got)
	}
	var words [][]string
	for _, piece := range Words(NoFences("```\nOne. Two.\n```")) {
		var each []string
		for _, word := range piece {
			each = append(each, word.Text)
		}
		words = append(words, each)
	}
	if want := [][]string{{"One.", "Two."}}; !reflect.DeepEqual(words, want) {
		t.Errorf("Words of a fence of lines that NoFences writes = %q, want %q", words, want)
	}
}
