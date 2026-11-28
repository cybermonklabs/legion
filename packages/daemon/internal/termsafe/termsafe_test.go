package termsafe

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Escaped writes each character that does not print as a Go escape and each byte that is not
// UTF-8 as \xNN, keeps every printable character and a backslash as they are, and with max 0 cuts
// nothing.
func TestEscapedWritesEveryCharacterThatDoesNotPrintAsAnEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain café.md", "plain café.md"},
		{"a\nb\tc\rd", `a\nb\tc\rd`},
		{"esc\x1b[2K del\x7f", `esc\x1b[2K del\x7f`},
		{"c1\u009b2K", `c1\u009b2K`},
		{"bidi\u202edm", `bidi\u202edm`},
		{"tag\U000e0041", `tag\U000e0041`},
		{"bad\xffz", `bad\xffz`},
		{`back\x1b`, `back\x1b`},
	} {
		if got := Escaped(tc.in, 0); got != tc.want {
			t.Errorf("Escaped(%q, 0) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("\x01", 10_000)
	if got := Escaped(long, 0); got != strings.Repeat(`\x01`, 10_000) {
		t.Errorf("Escaped with max 0 cut a text of %d bytes to %d", len(long), len(got))
	}
}

// With max above 0 the text is at most max bytes, cut after the last character or escape that
// leaves room for the ellipsis, so no escape and no character is cut in two.
func TestEscapedCutsBetweenEscapesAndCharacters(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{strings.Repeat("é", 200), 255, strings.Repeat("é", 126) + ellipsis},
		{strings.Repeat("\x01", 100), 255, strings.Repeat(`\x01`, 63) + ellipsis},
		{"short", 255, "short"},
	} {
		got := Escaped(tc.in, tc.max)
		if got != tc.want || len(got) > tc.max || !utf8.ValidString(got) {
			t.Errorf("Escaped(%q…, %d) = %q (%d bytes), want %q", tc.in[:min(len(tc.in), 8)], tc.max, got, len(got), tc.want)
		}
	}
}

// Every code point comes out printable: no character a terminal acts on passes raw.
func TestEscapedLeavesNothingThatDoesNotPrint(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		for _, c := range Escaped(string(r), 0) {
			if !unicode.IsPrint(c) {
				t.Fatalf("Escaped(%U) holds %U, which does not print", r, c)
			}
		}
	}
}
