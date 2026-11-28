package charts

import (
	"testing"
	"unicode"
)

// afterUnicode151 are characters that Unicode 16.0 and 17.0 assign, which Go (Unicode 17.0) reads as
// printable and Python 3.13 (Unicode 15.1) does not.
var afterUnicode151 = []rune{0x0897, 0x088F, 0x0C5C, 0x10D40, 0x16D40, 0x1CC00, 0x1E5D0, 0x1FAE9, 0xA7CE}

// ofUnicode151 are characters that Unicode 15.0 and 15.1 assign, which Python 3.13 reads as printable.
var ofUnicode151 = []rune{0x2EBF0, 0x2FFC, 0x1F6DC, 0x1FAE8, 0x31350, 0x11B00}

// TestTheTablesAreThoseOfPythonsUnicode151 checks the printable and white space tables on the
// characters where the Unicode versions differ, and on the characters where Go's own tables and
// Python's differ in kind: Python's white space holds the four information separators, and it
// counts neither U+180E nor any control character other than white space as printable.
func TestTheTablesAreThoseOfPythonsUnicode151(t *testing.T) {
	for _, char := range afterUnicode151 {
		if !unicode.IsPrint(char) {
			t.Errorf("U+%04X is not printable to Go, so it shows nothing of the tables' difference", char)
		}
		if printable(string(char)) {
			t.Errorf("U+%04X, assigned after Unicode 15.1, is printable", char)
		}
	}
	for _, char := range ofUnicode151 {
		if !printable(string(char)) {
			t.Errorf("U+%04X, assigned by Unicode 15.1, is not printable", char)
		}
	}
	for _, char := range []rune{'a', ' ', '~', 0xA1, 0xE9, 0x4E2D, 0x1F600, 0x10000, 0xFFFD} {
		if !printable(string(char)) {
			t.Errorf("U+%04X is not printable", char)
		}
	}
	for _, char := range []rune{0, '\t', '\n', 0x7F, 0x85, 0xA0, 0xAD, 0x180E, 0x2003, 0x200B, 0x2028, 0x3000, 0xE000, 0xFFFF, 0x10FFFF} {
		if printable(string(char)) {
			t.Errorf("U+%04X is printable", char)
		}
	}
	for _, char := range []rune{'\t', '\n', '\v', '\f', '\r', 0x1C, 0x1D, 0x1E, 0x1F, ' ', 0x85, 0xA0, 0x1680, 0x2000, 0x2003, 0x200A,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000} {
		if !pySpace(char) {
			t.Errorf("U+%04X is no white space", char)
		}
	}
	for _, char := range []rune{'a', 0, 0x1B, 0x180E, 0x200B, 0x2010, 0xFEFF} {
		if pySpace(char) {
			t.Errorf("U+%04X is white space", char)
		}
	}
	if got := words("a\u001fb\u2003c\u180ed  e"); got != 4 {
		t.Errorf("words of a text with U+001F, an em space and U+180E holds %d, want 4", got)
	}
	if !blank("\u3000\u0085\u001f") || blank("\u3000\u180e") {
		t.Error("blank does not strip Python's white space only")
	}
}

// TestALoneSurrogateIsOneCharacterThatIsNotPrintable checks the WTF-8 spelling of a lone surrogate:
// one character to chars, none printable, and the character it is when written back as JSON.
func TestALoneSurrogateIsOneCharacterThatIsNotPrintable(t *testing.T) {
	for _, char := range []rune{0xD800, 0xDBFF, 0xDC00, 0xDFFF} {
		text := "a" + surrogate(char) + "b"
		if got := chars(text); got != 3 {
			t.Errorf("U+%04X: chars holds %d, want 3", char, got)
		}
		if printable(text) || !printable("ab") {
			t.Errorf("U+%04X: a text of a lone surrogate is printable", char)
		}
		var read []rune
		for point := range codePoints(text) {
			read = append(read, point)
		}
		if len(read) != 3 || read[1] != char {
			t.Errorf("U+%04X: code points read %U", char, read)
		}
		if got, want := dumps(text), `"a\u`+map[rune]string{0xD800: "d800", 0xDBFF: "dbff", 0xDC00: "dc00", 0xDFFF: "dfff"}[char]+`b"`; got != want {
			t.Errorf("U+%04X: dumps writes %s, want %s", char, got, want)
		}
	}
	if got := chars("\U0001F600é\x00"); got != 3 {
		t.Errorf("chars of an emoji, a letter and a NUL holds %d, want 3", got)
	}
	var seen int
	for range codePoints("ab\U0001F600") {
		seen++
		break
	}
	if seen != 1 {
		t.Error("codePoints does not stop when its reader does")
	}
}
