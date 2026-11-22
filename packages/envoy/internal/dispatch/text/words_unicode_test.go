package text

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"go/format"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// updateWordBreakTable is this package's one -update flag: it writes words_table.go from the
// Unicode data in testdata, as TestTheWordBreakTableIsTheUnicodeData reads it.
var updateWordBreakTable = flag.Bool("update", false, "write words_table.go from the Unicode data in testdata")

// wordBreakTableFile is the table -update writes and words.go reads.
const wordBreakTableFile = "words_table.go"

// unicodeDataFiles are the files of Unicode 17.0.0's character database that the word rules are
// built and tested from, each as https://www.unicode.org/Public/17.0.0/ucd/<source> publishes it,
// with its SHA-256, so a file edited by hand fails the test that reads it.
var unicodeDataFiles = map[string]struct{ source, sha256 string }{
	"testdata/WordBreakProperty-17.0.0.txt": {"auxiliary/WordBreakProperty.txt", "72274cac1e6b919507db35655c3e175aa27274668a1ece95c28d2069f2ad9852"},
	"testdata/emoji-data-17.0.0.txt":        {"emoji/emoji-data.txt", "2cb2bb9455cda83e8481541ecf5b6dfda66a3bb89efa3fa7c5297eccf607b72b"},
	"testdata/WordBreakTest-17.0.0.txt":     {"auxiliary/WordBreakTest.txt", "1de23a75f37904abc7d206239ee8d34f8fdf0fb4ab32a7174dfbabbde25419b2"},
}

// wordBreakValues are the Word_Break values the rules read, by the name the data gives each; a
// code point the data names no value for is Other.
var wordBreakValues = map[string]wordBreak{
	"CR": wbCR, "LF": wbLF, "Newline": wbNewline, "Extend": wbExtend, "ZWJ": wbZWJ,
	"Regional_Indicator": wbRegionalIndicator, "Format": wbFormat, "Katakana": wbKatakana,
	"Hebrew_Letter": wbHebrewLetter, "ALetter": wbALetter, "Single_Quote": wbSingleQuote,
	"Double_Quote": wbDoubleQuote, "MidNumLet": wbMidNumLet, "MidLetter": wbMidLetter,
	"MidNum": wbMidNum, "Numeric": wbNumeric, "ExtendNumLet": wbExtendNumLet, "WSegSpace": wbWSegSpace,
}

// readUnicodeData is the content of one of unicodeDataFiles, after its SHA-256 is checked.
func readUnicodeData(t *testing.T, file string) string {
	t.Helper()
	want, known := unicodeDataFiles[file]
	if !known {
		t.Fatalf("%s is not one of the Unicode data files", file)
	}
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != want.sha256 {
		t.Fatalf("%s has SHA-256 %x, not %s, that of https://www.unicode.org/Public/17.0.0/ucd/%s", file, sum, want.sha256, want.source)
	}
	return string(content)
}

// unicodeDataLines calls each with the code points and the first property field of every data
// line of a Unicode property file, "first..last ; property # comment" or "point ; property".
func unicodeDataLines(t *testing.T, file string, each func(first, last rune, property string)) {
	t.Helper()
	lines := 0
	for number, line := range strings.Split(readUnicodeData(t, file), "\n") {
		data, _, _ := strings.Cut(line, "#")
		if strings.TrimSpace(data) == "" {
			continue
		}
		points, property, found := strings.Cut(data, ";")
		if !found {
			t.Fatalf("%s:%d: no ; in %q", file, number+1, line)
		}
		from, to, isRange := strings.Cut(strings.TrimSpace(points), "..")
		if !isRange {
			to = from
		}
		first, errFirst := strconv.ParseUint(from, 16, 32)
		last, errLast := strconv.ParseUint(to, 16, 32)
		if errFirst != nil || errLast != nil || first > last || last > unicode.MaxRune {
			t.Fatalf("%s:%d: no code point range in %q", file, number+1, line)
		}
		each(rune(first), rune(last), strings.TrimSpace(property))
		lines++
	}
	if lines == 0 {
		t.Fatalf("%s holds no data line", file)
	}
}

// unicodeWordBreakProperties are the Word_Break value and the Extended_Pictographic property of
// every code point, as the two property files give them.
func unicodeWordBreakProperties(t *testing.T) ([]wordBreak, []bool) {
	t.Helper()
	values := make([]wordBreak, unicode.MaxRune+1)
	named := make([]bool, unicode.MaxRune+1)
	unicodeDataLines(t, "testdata/WordBreakProperty-17.0.0.txt", func(first, last rune, property string) {
		value, known := wordBreakValues[property]
		if !known {
			t.Fatalf("WordBreakProperty-17.0.0.txt names the Word_Break value %q, which the rules do not read", property)
		}
		for r := first; r <= last; r++ {
			if named[r] {
				t.Fatalf("WordBreakProperty-17.0.0.txt names U+%04X twice", r)
			}
			named[r], values[r] = true, value
		}
	})
	pictographic := make([]bool, unicode.MaxRune+1)
	pictographs := 0
	unicodeDataLines(t, "testdata/emoji-data-17.0.0.txt", func(first, last rune, property string) {
		if property != "Extended_Pictographic" {
			return
		}
		for r := first; r <= last; r++ {
			pictographic[r] = true
			pictographs++
		}
	})
	if pictographs == 0 {
		t.Fatal("emoji-data-17.0.0.txt names no Extended_Pictographic code point")
	}
	return values, pictographic
}

// wordBreakTableSource is words_table.go as -update writes it: every run of code points that
// share a Word_Break value and the Extended_Pictographic property, but for those that are Other
// and no pictograph, in order.
func wordBreakTableSource(t *testing.T, values []wordBreak, pictographic []bool) []byte {
	t.Helper()
	names := make(map[wordBreak]string, len(wordBreakValues))
	for name, value := range wordBreakValues {
		names[value] = "wb" + strings.ReplaceAll(name, "_", "")
	}
	var source bytes.Buffer
	source.WriteString(`// Code generated by "go test ./internal/dispatch/text -run TestTheWordBreakTableIsTheUnicodeData -update"; DO NOT EDIT.

package text

// wordBreakRanges are the code points whose Word_Break value is not Other or that are
// Extended_Pictographic, in order, from testdata/WordBreakProperty-17.0.0.txt and
// testdata/emoji-data-17.0.0.txt, files of the Unicode Character Database 17.0.0
// (https://www.unicode.org/Public/17.0.0/ucd/), © 2025 Unicode®, Inc.; for terms of use and
// license, see https://www.unicode.org/terms_of_use.html.
var wordBreakRanges = [...]wordBreakRange{
`)
	for first := rune(0); first <= unicode.MaxRune; {
		last := first
		for last < unicode.MaxRune && values[last+1] == values[first] && pictographic[last+1] == pictographic[first] {
			last++
		}
		if values[first] != wbOther || pictographic[first] {
			name := "wbOther"
			if values[first] != wbOther {
				name = names[values[first]]
			}
			fmt.Fprintf(&source, "\t{0x%04X, 0x%04X, %s, %t},\n", first, last, name, pictographic[first])
		}
		first = last + 1
	}
	source.WriteString("}\n")
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		t.Fatalf("format the table: %v", err)
	}
	return formatted
}

// words_table.go is what -update writes from Unicode's data, so it holds nothing the data does
// not, and wordBreakOf answers, for each of the 1,114,112 code points, the Word_Break value and
// the Extended_Pictographic property the data gives it.
func TestTheWordBreakTableIsTheUnicodeData(t *testing.T) {
	values, pictographic := unicodeWordBreakProperties(t)
	source := wordBreakTableSource(t, values, pictographic)
	if *updateWordBreakTable {
		if err := os.WriteFile(wordBreakTableFile, source, 0o644); err != nil {
			t.Fatalf("write %s: %v", wordBreakTableFile, err)
		}
	}
	committed, err := os.ReadFile(wordBreakTableFile)
	if err != nil {
		t.Fatalf("read %s (run with -update to write it): %v", wordBreakTableFile, err)
	}
	if !bytes.Equal(committed, source) {
		t.Errorf("%s is not what -update writes from the Unicode data (run with -update to write it)", wordBreakTableFile)
	}
	read, mismatches := 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		value, isPictograph := wordBreakOf(r)
		read++
		if value != values[r] || isPictograph != pictographic[r] {
			mismatches++
			if mismatches <= 10 {
				t.Errorf("wordBreakOf(U+%04X) = %d, %t; the data gives %d, %t", r, value, isPictograph, values[r], pictographic[r])
			}
		}
	}
	if read != unicode.MaxRune+1 {
		t.Fatalf("read %d code points, want %d", read, unicode.MaxRune+1)
	}
	if mismatches > 0 {
		t.Errorf("wordBreakOf disagrees with the data at %d of %d code points", mismatches, read)
	}
	t.Logf("read %d code points against the %d ranges of %s", read, len(wordBreakRanges), wordBreakTableFile)
}

// wordBreakCase is one case of WordBreakTest-17.0.0.txt: its text, and the byte offsets inside it
// and at its end where the case puts a word boundary.
type wordBreakCase struct {
	line       int
	marks      string
	value      string
	boundaries []int
}

// wordBreakCases are the cases of WordBreakTest-17.0.0.txt. A case is a line of code points in
// hex, each between two marks: ÷ where UAX #29 puts a boundary, × where it puts none.
func wordBreakCases(t *testing.T) []wordBreakCase {
	t.Helper()
	var cases []wordBreakCase
	declared := -1
	for number, line := range strings.Split(readUnicodeData(t, "testdata/WordBreakTest-17.0.0.txt"), "\n") {
		if count, found := strings.CutPrefix(line, "# Lines: "); found {
			declared, _ = strconv.Atoi(strings.TrimSpace(count))
		}
		data, _, _ := strings.Cut(line, "#")
		fields := strings.Fields(data)
		if len(fields) == 0 {
			continue
		}
		if len(fields)%2 == 0 || fields[0] != "÷" || fields[len(fields)-1] != "÷" {
			t.Fatalf("WordBreakTest-17.0.0.txt:%d: not a case: %q", number+1, line)
		}
		c := wordBreakCase{line: number + 1, marks: strings.TrimSpace(data)}
		var value strings.Builder
		for i := 1; i < len(fields); i += 2 {
			point, err := strconv.ParseUint(fields[i], 16, 32)
			if err != nil || !utf8.ValidRune(rune(point)) {
				t.Fatalf("WordBreakTest-17.0.0.txt:%d: %q is no code point", number+1, fields[i])
			}
			value.WriteRune(rune(point))
			switch fields[i+1] {
			case "÷":
				c.boundaries = append(c.boundaries, value.Len())
			case "×":
			default:
				t.Fatalf("WordBreakTest-17.0.0.txt:%d: %q is neither ÷ nor ×", number+1, fields[i+1])
			}
		}
		c.value = value.String()
		cases = append(cases, c)
	}
	if len(cases) == 0 || len(cases) != declared {
		t.Fatalf("read %d cases of WordBreakTest-17.0.0.txt, which declares %d", len(cases), declared)
	}
	return cases
}

// Each case of Unicode's word-break test data parts into exactly the segments its marks give:
// wordSegments ends a segment at each ÷ and at no ×. And HeadWords, at each limit from 1 to the
// case's length, answers a prefix that ends at one of those boundaries.
func TestWordSegmentsBreakWhereUnicodesTestDataDoes(t *testing.T) {
	cases := wordBreakCases(t)
	failures, boundaries, heads := 0, 0, 0
	fail := func(format string, args ...any) {
		failures++
		if failures <= 20 {
			t.Errorf(format, args...)
		}
	}
	for _, c := range cases {
		segments := wordSegments{s: c.value}
		var ends []int
		for {
			end, runes, _ := segments.segment()
			if runes == 0 {
				break
			}
			ends = append(ends, end)
		}
		boundaries += len(c.boundaries)
		if !slices.Equal(ends, c.boundaries) {
			fail("WordBreakTest-17.0.0.txt:%d: %s: the segments end at bytes %v, the case at %v", c.line, c.marks, ends, c.boundaries)
			continue
		}
		for limit := 1; limit <= utf8.RuneCountInString(c.value); limit++ {
			heads++
			if head, _ := HeadWords(c.value, limit); !slices.Contains(c.boundaries, len(head)) {
				fail("WordBreakTest-17.0.0.txt:%d: %s: HeadWords at %d ends at byte %d, at no boundary of %v", c.line, c.marks, limit, len(head), c.boundaries)
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d answers disagree with the test data", failures)
	}
	t.Logf("read %d cases with %d boundaries, and %d answers of HeadWords", len(cases), boundaries, heads)
}
