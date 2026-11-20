package text

import (
	"fmt"
	"strings"
	"testing"
)

// headWordsRow is one value HeadWords cuts, and the answer it must give.
type headWordsRow struct {
	name, value string
	limit       int
	want        string
	cut         bool
}

func checkHeadWords(t *testing.T, rows []headWordsRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, cut := HeadWords(row.value, row.limit)
			if got != row.want || cut != row.cut {
				t.Fatalf("HeadWords(%q, %d) = %q, %v; want %q, %v", row.value, row.limit, got, cut, row.want, row.cut)
			}
		})
	}
}

// White space parts two words and is no word itself: the cut falls at the last word end at or
// before the limit, never after white space, a word is never cut in two, and the answer is a cut
// where it leaves out a character that is no white space, a mark alone among them. Both callers
// in the api package pass words joined by single spaces, so the rows with other white space are
// the only ones that hold what HeadWords does with it.
func TestHeadWordsEndsAWordAtWhiteSpace(t *testing.T) {
	rows := []headWordsRow{
		{name: "the cut falls at the last space inside the limit", value: "one two three four", limit: 10, want: "one two", cut: true},
		{name: "a word that ends at the limit is kept", value: "one two three four", limit: 13, want: "one two three", cut: true},
		{name: "a word that ends one past the limit is left out", value: "one two three four", limit: 12, want: "one two", cut: true},
		{name: "a tab and a newline end a word as a space does", value: "one\ttwo\nthree", limit: 9, want: "one\ttwo", cut: true},
		{name: "a tab alone ends a word", value: "one\ttwo three", limit: 5, want: "one", cut: true},
		{name: "white space at the start is kept and ends no word", value: "  one two", limit: 6, want: "  one", cut: true},
		{name: "white space at the start ends no word, so the first word after it is kept whole", value: "  abcdefgh ij", limit: 4, want: "  abcdefgh", cut: true},
		{name: "white space alone after the last word kept is no cut", value: "one two     ", limit: 9, want: "one two", cut: false},
		{name: "marks alone after the last word kept are no word, but leaving them out is a cut", value: "abc " + strings.Repeat("\u0301", 100), limit: 80, want: "abc", cut: true},
		{name: "word joiners alone after the last word kept are no word, but leaving them out is a cut", value: strings.Repeat("x", 79) + " " + strings.Repeat("\u2060", 5), limit: 80, want: strings.Repeat("x", 79), cut: true},
		{name: "a first word past the limit is kept whole and is a cut where a word follows it", value: "abcdefghij klm", limit: 4, want: "abcdefghij", cut: true},
		{name: "a first word past the limit with white space alone after it is no cut", value: "abcdefghij   ", limit: 4, want: "abcdefghij", cut: false},
		{name: "a first word past the limit that is the whole value is returned whole", value: "abcdefghij", limit: 4, want: "abcdefghij", cut: false},
		{name: "a mark at the start is no word and ends none", value: "\u0301abcdefgh ij", limit: 4, want: "\u0301abcdefgh", cut: true},
		{name: "a mark alone after a line break is no word, but leaving it out is a cut", value: "abcdefghij\n\u0301", limit: 4, want: "abcdefghij", cut: true},
		{name: "a mark after a space is no word, so the cut does not end with it", value: "abc \u0301 defghijkl", limit: 6, want: "abc", cut: true},
		{name: "a joiner after a line break is no word, so the cut does not end with it", value: "abc\n\u2060 defghijkl", limit: 6, want: "abc", cut: true},
		{name: "a value at or under the limit is returned unchanged", value: " one  two ", limit: 10, want: " one  two ", cut: false},
		{name: "a value of white space alone past the limit holds no word and is returned unchanged", value: " \t\n  ", limit: 2, want: " \t\n  ", cut: false},
	}
	for _, length := range []int{81, 110, 161, 1000} {
		word := strings.Repeat("w", length)
		rows = append(rows,
			headWordsRow{name: fmt.Sprintf("a first word of %d characters is kept whole before more words", length), value: word + " and more", limit: 80, want: word, cut: true},
			headWordsRow{name: fmt.Sprintf("a first word of %d characters that is the whole value is returned whole", length), value: word, limit: 80, want: word, cut: false})
	}
	checkHeadWords(t, rows)
}

// Opening punctuation that leads into a word goes with that word. The first word is the first
// word that holds a letter or a number, with all that comes before it: white space, any
// punctuation, a symbol or an emoji, at any length. Where it runs past the limit, the answer keeps
// it whole with what leads into it, as a test's name in backticks or a path such as `./TestName`
// is written, and never the lead-in alone, even where the lead-in itself runs past the limit, as a
// rule of hyphens before a sentence does. A value with no word of letters or numbers is cut at the
// last end of a word inside the limit, or after its first word of any kind where no word ends
// inside it, and a value with no word at all is returned whole. A later word that runs past the
// limit is left out with the opening marks before it, so the answer never ends with a backtick or
// a bracket that opens a word it left out. After a word of letters or numbers, a separator, such
// as a slash, stays a word boundary.
func TestHeadWordsKeepsAnOpeningMarkWithTheWordItLeadsInto(t *testing.T) {
	name := "TestA" + strings.Repeat("LongName", 12) // 101 characters, a test's name
	var rows []headWordsRow
	for _, lead := range []struct{ open, close string }{
		{"`", "`"}, {"[", "]"}, {"(", ")"}, {"{", "}"}, {`"`, `"`}, {"'", "'"}, {"“", "”"}, {"‘", "’"}, {"(`", "`)"},
	} {
		rows = append(rows, headWordsRow{
			name:  fmt.Sprintf("a first word of 101 characters after %q is kept whole with it", lead.open),
			value: lead.open + name + lead.close + " passed 20 of 20 runs.", limit: 80, want: lead.open + name, cut: true,
		})
		rows = append(rows, headWordsRow{
			name:  fmt.Sprintf("a later word of 101 characters after %q is left out with it", lead.open),
			value: "Run " + lead.open + name + lead.close + " now.", limit: 80, want: "Run", cut: true,
		})
	}
	// Punctuation that opens no word, a symbol and an emoji lead into the first word as an
	// opening mark does.
	for _, lead := range []string{"/", "./", "...", "-", "--", ".", ":", ")", "- ", "§", "🛑"} {
		rows = append(rows, headWordsRow{
			name:  fmt.Sprintf("a first word of 101 characters after %q is kept whole with it", lead),
			value: lead + name + " passed 20 of 20 runs.", limit: 80, want: lead + name, cut: true,
		})
	}
	checkHeadWords(t, append(rows,
		headWordsRow{name: "a first word after a backtick that is the whole value is returned whole", value: "`" + name, limit: 80, want: "`" + name, cut: false},
		headWordsRow{name: "a first word after 79 slashes is kept whole with them", value: strings.Repeat("/", 79) + "ab tail words", limit: 80, want: strings.Repeat("/", 79) + "ab", cut: true},
		headWordsRow{name: "a first word after 81 slashes is kept whole with them", value: strings.Repeat("/", 81) + "ab tail words", limit: 80, want: strings.Repeat("/", 81) + "ab", cut: true},
		headWordsRow{name: "a rule of 100 hyphens leads into the first word after it", value: strings.Repeat("-", 100) + " Then the run passed.", limit: 80, want: strings.Repeat("-", 100) + " Then", cut: true},
		headWordsRow{name: "41 spaced hyphens lead into the first word after them", value: strings.Repeat("- ", 41) + name + " passed.", limit: 80, want: strings.Repeat("- ", 41) + name, cut: true},
		headWordsRow{name: "one word of 100 underscores leads into the first word after it", value: strings.Repeat("_", 100) + " Then the run passed.", limit: 80, want: strings.Repeat("_", 100) + " Then", cut: true},
		headWordsRow{name: "100 slashes with no word of letters or numbers after them are cut inside the limit", value: strings.Repeat("/", 100), limit: 80, want: strings.Repeat("/", 80), cut: true},
		headWordsRow{name: "spaced hyphens with no word of letters or numbers after them are cut inside the limit", value: strings.Repeat("- ", 60), limit: 80, want: strings.Repeat("- ", 39) + "-", cut: true},
		headWordsRow{name: "a first word after a backtick that ends inside the limit is cut at a word boundary as before", value: "`short` passed 20 of 20 runs", limit: 20, want: "`short` passed 20 of", cut: true},
		headWordsRow{name: "a long word after a first word that ends inside the limit is left out", value: "Run " + name + " now", limit: 80, want: "Run", cut: true},
		headWordsRow{name: "a long word after a number that ends inside the limit is left out", value: "1 " + name, limit: 80, want: "1", cut: true},
		headWordsRow{name: "a long word after a numbered item is left out with its backtick", value: "1. `" + name + "` passed.", limit: 80, want: "1.", cut: true},
		headWordsRow{name: "a mark that ends a word inside the limit stays with that word", value: "Run `x` " + name, limit: 80, want: "Run `x`", cut: true},
		headWordsRow{name: "a slash after a first word is a word boundary, not a lead-in", value: "Run /" + name + " now", limit: 80, want: "Run /", cut: true},
	))
}

// prefixRunes is the first n runes of s.
func prefixRunes(s string, n int) string {
	return string([]rune(s)[:n])
}

// In a script written without spaces between its words, Unicode's default word rules (UAX #29)
// put a word boundary beside each ideograph, each Hiragana character and each letter of Thai and
// the other Complex_Context scripts, whose Word_Break value is Other (WB999), and keep a mark, a
// joiner or a format character with the character before it (WB4). Katakana is one word for as
// long as it runs (WB13), and a number is one word (WB8).
func TestHeadWordsCutsAScriptWithoutSpacesAtItsWordBoundaries(t *testing.T) {
	// Each value of the second group is 100 to 122 characters and is cut at 80, the budget of a
	// pull-request body line: one row for each script written without spaces that is neither
	// Chinese, Japanese nor Thai, two for a spacing vowel sign at the 81st character, and one for
	// a number across the 80th.
	khmerVowels := "ខ" + strings.Repeat("កា", 60)   // KHA, then KA with the spacing vowel AA (U+17B6), 121 runes
	myanmarVowels := "ခ" + strings.Repeat("ကာ", 60) // KHA, then KA with the spacing vowel AA (U+102C), 121 runes
	thaiNumber := "กก" + strings.Repeat("ก๑๒๓", 30) // KO KAI, then KO KAI before a three-digit number, 122 runes
	checkHeadWords(t, []headWordsRow{
		{name: "Chinese: a boundary falls beside each ideograph", value: "每个测试都通过了", limit: 3, want: "每个测", cut: true},
		{name: "Japanese: a boundary falls beside each Hiragana character", value: "すべてのしけん", limit: 3, want: "すべて", cut: true},
		{name: "Japanese: a run of Katakana is one word, kept whole past the limit", value: "テストテストは通った", limit: 4, want: "テストテスト", cut: true},
		{name: "Thai: the cut inside the limit falls before a letter, never before its marks", value: "ที่นี่", limit: 4, want: "ที่", cut: true},
		{name: "Thai: with no boundary inside the limit the first letter keeps its marks", value: "ที่นี่", limit: 2, want: "ที่", cut: true},
		{name: "a variation selector stays with its ideograph", value: "漢\ufe00字", limit: 1, want: "漢\ufe00", cut: true},
		{name: "a combining sound mark stays with its Hiragana character", value: "か\u3099き", limit: 1, want: "か\u3099", cut: true},
		{name: "a boundary after an ideograph falls after the marks that follow it", value: "中\ufe00defgh", limit: 4, want: "中\ufe00", cut: true},
		{name: "a first word in Latin letters ends at the ideograph after it", value: "abcdefgh中文", limit: 3, want: "abcdefgh", cut: true},
		{name: "words and ideographs mixed: the cut falls at the last boundary of either kind", value: "ab 中文字", limit: 4, want: "ab 中", cut: true},
		{name: "the white space before an ideograph is not kept at the end of the cut", value: "ab 中文字", limit: 3, want: "ab", cut: true},
		{name: "a cut may fall right after a zero-width joiner that no pictograph follows", value: "中\u200d文字", limit: 2, want: "中\u200d", cut: true},
		{name: "a value at or under the limit is returned unchanged", value: "每个测试", limit: 4, want: "每个测试", cut: false},

		{name: "Lao: a boundary falls beside each letter", value: strings.Repeat("ກ", 100), limit: 80, want: strings.Repeat("ກ", 80), cut: true},
		{name: "Khmer: a boundary falls beside each letter", value: strings.Repeat("ក", 100), limit: 80, want: strings.Repeat("ក", 80), cut: true},
		{name: "Myanmar: a boundary falls beside each letter", value: strings.Repeat("က", 100), limit: 80, want: strings.Repeat("က", 80), cut: true},
		{name: "Tai Le: a boundary falls beside each letter", value: strings.Repeat("ᥐ", 100), limit: 80, want: strings.Repeat("ᥐ", 80), cut: true},
		{name: "New Tai Lue: a boundary falls beside each letter", value: strings.Repeat("ᦀ", 100), limit: 80, want: strings.Repeat("ᦀ", 80), cut: true},
		{name: "Tai Tham: a boundary falls beside each letter", value: strings.Repeat("ᨠ", 100), limit: 80, want: strings.Repeat("ᨠ", 80), cut: true},
		{name: "Tai Viet: a boundary falls beside each letter", value: strings.Repeat("ꪀ", 100), limit: 80, want: strings.Repeat("ꪀ", 80), cut: true},
		{name: "Ahom: a boundary falls beside each letter", value: strings.Repeat("𑜀", 100), limit: 80, want: strings.Repeat("𑜀", 80), cut: true},
		// The 80th character is a KA whose vowel sign is the 81st, so the cut falls before the KA.
		{name: "Khmer: a spacing vowel sign stays with its letter", value: khmerVowels, limit: 80, want: prefixRunes(khmerVowels, 79), cut: true},
		{name: "Myanmar: a spacing vowel sign stays with its letter", value: myanmarVowels, limit: 80, want: prefixRunes(myanmarVowels, 79), cut: true},
		// The 80th and 81st characters are the first two digits of a number, so the cut falls
		// before the number.
		{name: "Thai: a number in Thai digits is one word", value: thaiNumber, limit: 80, want: prefixRunes(thaiNumber, 79), cut: true},
	})
}

// Emoji, symbols and punctuation are words of their own, apart from the letters beside them, as
// Unicode's default word rules (UAX #29) part them: a boundary falls beside each (WB999), apart
// from a zero-width joiner and the pictograph after it (WB3c), a pair of regional indicators, which
// is one flag (WB15, WB16), and the punctuation the rules keep inside a word: a colon or a full
// stop between two letters (WB6, WB7) and a full stop or a comma between two digits (WB11, WB12).
// So a long run of them with no space is cut inside the limit, and a path is cut at a slash. A
// chain of pictographs joined by zero-width joiners is one word: before the first word of letters
// or numbers it leads into that word as any word does, an opening mark before it too; where the
// value holds no such word, a chain that is the first word is kept whole; and after a word that
// ends inside the limit, it is left out.
func TestHeadWordsCutsEmojiSymbolsAndPunctuationAtTheirWordBoundaries(t *testing.T) {
	chain := strings.Repeat("🛑\u200d", 99) + "🛑" // 100 pictographs joined, 199 runes
	checkHeadWords(t, []headWordsRow{
		{name: "81 emoji with no space are cut at the 80th", value: strings.Repeat("🛑", 81), limit: 80, want: strings.Repeat("🛑", 80), cut: true},
		{name: "a run of CJK radicals with no space is cut at the 80th", value: strings.Repeat("⺀", 100), limit: 80, want: strings.Repeat("⺀", 80), cut: true},
		{name: "Katakana words joined by a middle dot are cut at a dot", value: strings.Repeat("データベース・", 15), limit: 80, want: strings.Repeat("データベース・", 11), cut: true},
		{name: "no cut falls between a zero-width joiner and the pictograph after it", value: "🛑\u200d🛑🛑", limit: 2, want: "🛑\u200d🛑", cut: true},
		{name: "a pair of regional indicators is one flag", value: "🇫🇷🇩🇪🇯🇵", limit: 3, want: "🇫🇷", cut: true},
		{name: "a skin-tone modifier stays with its emoji", value: "👍🏽👍🏽", limit: 1, want: "👍🏽", cut: true},
		{name: "a full stop after a word is a word of its own", value: "one two.", limit: 7, want: "one two", cut: true},
		{name: "a colon between two letters is inside a word", value: "user:pass and more", limit: 6, want: "user:pass", cut: true},
		{name: "a full stop between two digits is inside a number", value: "3.14159 is pi", limit: 4, want: "3.14159", cut: true},
		{name: "a path is cut at the last slash inside the limit", value: "/srv/data/" + strings.Repeat("segment/", 12), limit: 80, want: "/srv/data/" + strings.Repeat("segment/", 8), cut: true},
		{name: "a chain of 100 joined pictographs that is the whole value is returned whole", value: chain, limit: 80, want: chain, cut: false},
		{name: "a chain of 100 joined pictographs leads into the first word of letters after it", value: chain + " and more", limit: 80, want: chain + " and", cut: true},
		{name: "a chain of 100 joined pictographs with no word of letters after it is kept whole", value: chain + " 🛑 🛑", limit: 80, want: chain, cut: true},
		{name: "a chain of 100 joined pictographs after a first word is left out", value: "Log " + chain, limit: 80, want: "Log", cut: true},
		{name: "an opening parenthesis and a chain of 100 joined pictographs lead into the first word of letters after them", value: "(" + chain + " and more", limit: 80, want: "(" + chain + " and", cut: true},
	})
}

// A run with no white space that holds a URL is one word, so the answer keeps a URL whole or ends
// before it, and never ends inside it: GitHub's markdown makes a link of a URL up to the white
// space after it, so a cut inside one leaves a link that leads nowhere. A path with no URL in its
// run is still cut at a slash (TestHeadWordsCutsEmojiSymbolsAndPunctuationAtTheirWordBoundaries).
func TestHeadWordsKeepsAURLWhole(t *testing.T) {
	url := "https://example.com/" + strings.Repeat("segment/", 12) // 116 runes; the 80th is inside it
	short := "https://example.com/" + strings.Repeat("x", 50)      // 70 runes
	www := "www.example.com/" + strings.Repeat("segment/", 12)     // 112 runes
	rows := []headWordsRow{
		{name: "a URL across the limit is left out, and the cut falls before it", value: "Log: " + url + " shows the step.", limit: 80, want: "Log:", cut: true},
		{name: "a URL that ends inside the limit is kept whole", value: "Log: " + short + " shows the step.", limit: 80, want: "Log: " + short, cut: true},
		{name: "a first word that is a URL past the limit is kept whole", value: url + " shows the step.", limit: 80, want: url, cut: true},
		{name: "a URL past the limit that is the whole value is returned whole", value: url, limit: 80, want: url, cut: false},
		{name: "an address that starts with www and no scheme is one word", value: "See " + www, limit: 80, want: "See", cut: true},
		{name: "an address that starts with www in capitals is one word", value: "See " + strings.ToUpper(www), limit: 80, want: "See", cut: true},
		{name: "a www inside a word starts no link, so its run is cut at a slash", value: "See a" + www, limit: 80, want: "See awww.example.com/" + strings.Repeat("segment/", 7), cut: true},
		{name: "a markdown link's address is not cut inside", value: "See [the log](" + url + ") now", limit: 80, want: "See [the", cut: true},
	}
	// cmark-gfm, GitHub's markdown, links a `www.` address with no scheme at the start of a run or
	// after one of `*`, `_`, `~` and `(`, and in lower case only (extensions/autolink.c of
	// github/cmark-gfm at commit 46829933c3, www_match, lines 178-183). isURL is wider on purpose:
	// it reads such an address after `[` and `]` too, and in any case, so no text GitHub links is
	// cut; a run it reads as a URL that GitHub does not link is only kept whole or left out.
	for _, lead := range []string{"(", "*", "_", "[", "]", "~"} {
		rows = append(rows, headWordsRow{
			name:  fmt.Sprintf("an address that starts with www after %q is one word", lead),
			value: "Log " + lead + www + " shows the step.", limit: 80, want: "Log", cut: true,
		})
	}
	checkHeadWords(t, rows)
}

// A cut that ends with a URL GitHub links needs white space before anything written after it, or
// GitHub's link takes that into its address: EndsInURL reads the last run with no white space, as
// HeadWords reads a run for a URL (isURL), after white space of any width.
func TestEndsInURLReadsTheLastRunWithNoWhiteSpace(t *testing.T) {
	for _, row := range []struct {
		value string
		want  bool
	}{
		{"See www.example.com/runs/7777", true},
		{"www.example.com", true},
		{"See WWW.EXAMPLE.COM/RUNS", true},
		{"See (www.example.com/runs/7777)", true},
		{"See *www.example.com*", true},
		{"See _www.example.com_", true},
		{"See [www.example.com]", true},
		{"See ]www.example.com", true},
		{"See ~www.example.com~", true},
		{"Log: https://example.com/a/b", true},
		{"See\u3000www.example.com", true},
		{"See www.example.com and more", false},
		{"See awww.example.com/runs", false},
		{"See example.com/runs", false},
		{"", false},
	} {
		if got := EndsInURL(row.value); got != row.want {
			t.Errorf("EndsInURL(%q) = %v, want %v", row.value, got, row.want)
		}
	}
}

// HoldsLetter reads a letter or a number by the one rule of what a word is (classOf), so it
// agrees with HeadWords on which word can be the first word: a letter of any script, a digit or
// another number, alone or with the marks after it, and never a mark, a joiner or a format
// character alone. The halfwidth Katakana sound marks U+FF9E and U+FF9F are letters to
// unicode.IsLetter and Extend marks to UAX #29, so they part the two rules: a word of them alone
// holds no letter. Each row is also read by HeadWords after a lead-in of 81 slashes, which keeps
// the value as the first word, and leaves out the word "tail" after it, exactly where the row
// holds a letter, so the wants are HeadWords' own.
func TestHoldsLetterReadsALetterAsClassOfDoes(t *testing.T) {
	lead := strings.Repeat("/", 81)
	for _, row := range []struct {
		name, value string
		want        bool
	}{
		{"a Latin letter", "a", true},
		{"a Latin letter with a combining mark after it", "e\u0301", true},
		{"a digit", "1", true},
		{"a letter number, the Roman numeral twelve", "\u216b", true},
		{"another number, a superscript two", "\u00b2", true},
		{"an ideograph", "日", true},
		{"a Katakana letter", "テ", true},
		{"a halfwidth Katakana letter with its voiced sound mark", "\uff76\uff9e", true},
		{"a Thai letter", "ก", true},
		{"a halfwidth voiced sound mark, then a letter", "\uff9ea", true},
		{"the halfwidth voiced sound mark U+FF9E alone", "\uff9e", false},
		{"the halfwidth semi-voiced sound mark U+FF9F alone", "\uff9f", false},
		{"a run of joined halfwidth sound marks", "\uff9e\uff9f\uff9e", false},
		{"a combining mark alone", "\u0301", false},
		{"joined combining marks", "\u0301\u0302", false},
		{"a zero-width joiner", "\u200d", false},
		{"a soft hyphen, a format character", "\u00ad", false},
		{"a slash", "/", false},
		{"a rule of hyphens", "---", false},
		{"an underscore", "_", false},
		{"an emoji", "🛑", false},
		{"white space", " ", false},
		{"nothing", "", false},
	} {
		if got := HoldsLetter(row.value); got != row.want {
			t.Errorf("%s: HoldsLetter(%q) = %v, want %v", row.name, row.value, got, row.want)
		}
		if got, _ := HeadWords(lead+row.value+" tail", 80); strings.Contains(got, "tail") == row.want {
			t.Errorf("%s: HeadWords(81 slashes + %q + \" tail\", 80) = %q: the row's want is not HeadWords' rule", row.name, row.value, got)
		}
	}
}

// IndexLetter is where the first letter or number of a value stands, by the same rule as
// HoldsLetter, past every mark, joiner and halfwidth sound mark before it.
func TestIndexLetterFindsTheFirstLetterOrNumber(t *testing.T) {
	for _, row := range []struct {
		value string
		want  int
	}{
		{"abc", 0},
		{"--a", 2},
		{"- - テ", 4},
		{"\uff9e\uff9fa", 6},
		{"\u0301e", 2},
		{"/ \u216b", 2},
		{"\uff9e\uff9f", -1},
		{"- / ...", -1},
		{"", -1},
	} {
		if got := IndexLetter(row.value); got != row.want {
			t.Errorf("IndexLetter(%q) = %d, want %d", row.value, got, row.want)
		}
	}
}

// HoldsWord reads a word of any kind by the one rule of what a word is (classOf): a letter, a
// number, punctuation, a symbol or an emoji, with or without marks after it, and never white
// space, a mark, a joiner or a format character alone, nor a halfwidth sound mark, which UAX #29
// reads as a mark though unicode.IsLetter reads it as a letter.
func TestHoldsWordReadsAWordOfAnyKindAsClassOfDoes(t *testing.T) {
	for _, row := range []struct {
		name, value string
		want        bool
	}{
		{"a letter", "a", true},
		{"a digit", "1", true},
		{"a hyphen", "-", true},
		{"a slash with a halfwidth sound mark after it", "/\uff9e", true},
		{"an underscore", "_", true},
		{"an emoji", "🛑", true},
		{"the halfwidth voiced sound mark U+FF9E alone", "\uff9e", false},
		{"the halfwidth semi-voiced sound mark U+FF9F alone", "\uff9f", false},
		{"joined halfwidth sound marks", "\uff9e\uff9f", false},
		{"a combining mark alone", "\u0301", false},
		{"a zero-width joiner", "\u200d", false},
		{"a word joiner, a format character", "\u2060", false},
		{"white space", " \t", false},
		{"nothing", "", false},
	} {
		if got := HoldsWord(row.value); got != row.want {
			t.Errorf("%s: HoldsWord(%q) = %v, want %v", row.name, row.value, got, row.want)
		}
	}
}

// HeadRunes cuts a value to its first limit runes and reads no further, so a value of 1 MiB cut
// to 80 runes costs no allocation; a value that is no valid UTF-8 is cut as Go's conversion to
// runes reads it, with U+FFFD for each byte that starts no rune.
func TestHeadRunesReadsOnlyTheRunesItKeeps(t *testing.T) {
	for _, row := range []struct {
		value string
		limit int
		want  string
	}{
		{"abc", 3, "abc"},
		{"abc", 2, "ab"},
		{"abc", 0, ""},
		{"", 4, ""},
		{"日本語です", 3, "日本語"},
		{"a🛑b", 2, "a🛑"},
		{"\xffab", 2, "\ufffda"},
		{"\xff\xfeab", 3, "\ufffd\ufffda"},
		{"a\xff", 4, "a\xff"},
	} {
		if got := HeadRunes(row.value, row.limit); got != row.want {
			t.Errorf("HeadRunes(%q, %d) = %q, want %q", row.value, row.limit, got, row.want)
		}
	}
	long := strings.Repeat("x", 1<<20)
	if allocs := testing.AllocsPerRun(5, func() {
		if len(HeadRunes(long, 80)) != 80 {
			t.Fatal("HeadRunes of 1 MiB of x to 80 runes is not 80 bytes")
		}
	}); allocs != 0 {
		t.Fatalf("HeadRunes of 1 MiB to 80 runes made %v allocations, want 0", allocs)
	}
}
