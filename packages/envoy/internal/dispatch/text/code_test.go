package text

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// A body that writes the same party inside code and outside it names it: the occurrence outside
// carries no mark, which is what keeps it a delivering party (api.commentPartiesOf).
func TestExtractMarksEachOccurrenceOnItsOwn(t *testing.T) {
	body := "Run `dispatch://role/reviewer` and ask dispatch://role/reviewer about it."
	located := ExtractAt(body, "https://dispatch.test")
	if len(located) != 2 {
		t.Fatalf("read %#v, want both occurrences", located)
	}
	if !located[0].InCode || located[1].InCode {
		t.Errorf("read InCode=%t then %t, want true then false", located[0].InCode, located[1].InCode)
	}
}

// A bare key and a `#id` carry the mark as an address does, the one reading of the body's code
// contexts serving every reader of it.
func TestMentionsMarksBareCandidatesInCode(t *testing.T) {
	resolver := mapResolver{
		issues:    map[string]bool{"CORE-1": true},
		questions: map[string]Ref{"q2": {Kind: "ask", IssueKey: "CORE-1", ID: "a2"}},
	}
	located, err := Mentions("`CORE-1` and #q2 and\n\n    CORE-1\n", "https://dispatch.test", resolver)
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	want := []bool{true, false, true}
	got := make([]bool, len(located))
	for index, item := range located {
		got[index] = item.InCode
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Mentions() read %#v with InCode %v, want %v", located, got, want)
	}
}

// Code ranges are read once per body, however many references stand in them, and they run in
// order so a reference's offset can be looked up in them.
func TestCodeRangesAreOrderedAndDisjoint(t *testing.T) {
	body := "`a` and `b`\n\n```\nfenced `x`\n```\n\n    indented `y`\n\nlast `z`"
	ranges := codeRanges(body)
	if len(ranges) != 5 {
		t.Fatalf("codeRanges(%q) = %#v, want five", body, ranges)
	}
	for index, candidate := range ranges {
		if candidate.start >= candidate.end {
			t.Errorf("range %d is empty: %#v", index, candidate)
		}
		if index > 0 && ranges[index-1].end > candidate.start {
			t.Errorf("range %d starts inside range %d: %#v", index, index-1, ranges)
		}
	}
	// The fenced block's own backticks open no span: the block took them.
	if got := body[ranges[2].start:ranges[2].end]; got != "```\nfenced `x`\n```\n" {
		t.Errorf("the fenced range is %q", got)
	}
	if got := body[ranges[3].start:ranges[3].end]; !strings.HasPrefix(got, "    indented") {
		t.Errorf("the indented range is %q", got)
	}
}

// CodeRanges answers the ranges codeRanges reads, each a block exactly where codeBlocks read one:
// the fenced and the indented block, and none of the three spans around them.
func TestCodeRangesTellABlockFromASpan(t *testing.T) {
	body := "`a` and `b`\n\n```\nfenced `x`\n```\n\n    indented `y`\n\nlast `z`"
	blocks, _ := codeBlocks(body)
	ranges := codeRanges(body)
	exported := CodeRanges(body)
	if len(exported) != len(ranges) || len(ranges) != 5 {
		t.Fatalf("CodeRanges(%q) = %#v, want the five codeRanges reads", body, exported)
	}
	for index, candidate := range ranges {
		block := slices.ContainsFunc(blocks, func(block codeRange) bool {
			return block.start == candidate.start && block.end == candidate.end
		})
		if want := (CodeRange{Start: candidate.start, End: candidate.end, Block: block}); exported[index] != want {
			t.Errorf("CodeRanges(%q)[%d] = %#v, want %#v", body, index, exported[index], want)
		}
	}
	if got := []bool{exported[0].Block, exported[1].Block, exported[2].Block, exported[3].Block, exported[4].Block}; !reflect.DeepEqual(got, []bool{false, false, true, true, false}) {
		t.Errorf("CodeRanges(%q) tells blocks %v, want the fenced and the indented block alone", body, got)
	}
}

// onePassSlack is how many steps past four times its steps on a text a reader may take on a text
// four times as long. A reader of one pass takes a·n + b steps on a text of n, which at 4n is
// within 4·(a·n + b) wherever b is not below zero, and past it by -3b, a few steps whatever n is,
// where a loop skips a few bytes it was handed. A reader that rereads its text takes about 16
// times its steps on four times the text, far past this at the lengths read here. It is the
// dashboard's ONE_PASS_SLACK (`packages/dispatch/web/src/__tests__/one-pass.ts`).
const onePassSlack = 16

// countReaderSteps is how many steps (readerSteps) read takes. readerSteps belongs to the
// package, and every reading of a body in this test binary adds to it while it is set, so only a
// test that does not call t.Parallel counts: go test runs such a test while none of the
// package's parallel tests runs, since those wait until every test that is not parallel has
// returned. No test of the package calls t.Parallel today; one that does cannot move a count.
func countReaderSteps(read func()) int {
	steps := 0
	readerSteps = &steps
	defer func() { readerSteps = nil }()
	read()
	return steps
}

// stepsAtNAndFourN is how many steps read takes on a text of n units and on one of 4n, a unit
// being the row's own: a run of backticks and the space before it, or a line the text repeats.
func stepsAtNAndFourN(read func(n int), n int) (atN, atFourN int) {
	return countReaderSteps(func() { read(n) }), countReaderSteps(func() { read(4 * n) })
}

// expectOnePass fails t unless read reads its text in one pass: more steps on a text of 4n than
// on one of n, and no more than four times as many and onePassSlack.
func expectOnePass(t *testing.T, read func(n int), n int) {
	t.Helper()
	atN, atFourN := stepsAtNAndFourN(read, n)
	t.Logf("%d steps at n = %d and %d at 4n, bound %d", atN, n, atFourN, 4*atN+onePassSlack)
	if atFourN <= atN || atFourN > 4*atN+onePassSlack {
		t.Errorf("%d steps at n = %d and %d at 4n, want more at 4n and at most %d", atN, n, atFourN, 4*atN+onePassSlack)
	}
}

// expectAboveOnePass fails t unless read takes more than four times its steps and onePassSlack
// on a text of 4n: a control, showing the bound fails the reading its row guards against.
func expectAboveOnePass(t *testing.T, read func(n int), n int) {
	t.Helper()
	atN, atFourN := stepsAtNAndFourN(read, n)
	t.Logf("%d steps at n = %d and %d at 4n, bound %d", atN, n, atFourN, 4*atN+onePassSlack)
	if atFourN <= 4*atN+onePassSlack {
		t.Errorf("%d steps at n = %d and %d at 4n, want more than %d", atN, n, atFourN, 4*atN+onePassSlack)
	}
}

// unclosedRuns is one paragraph of n bytes: runs of 1, 2, 3 and more backticks, each after a
// space, as many as fit, then spaces to the end. No two runs are as wide, so none closes another,
// and a text four times as long holds about twice as many runs.
func unclosedRuns(n int) string {
	var text strings.Builder
	for width := 1; text.Len()+1+width <= n; width++ {
		text.WriteByte(' ')
		text.WriteString(strings.Repeat("`", width))
	}
	return text.String() + strings.Repeat(" ", n-text.Len())
}

// One paragraph of runs of backticks of distinct widths, none closed: each run's closer is looked
// for among the body's runs of its own width, read once (closingRun), where a search of the rest
// of the paragraph from each opener took about 8 times the time on 4 times the text.
func TestCodeRangesReadsUnclosedRunsInOnePass(t *testing.T) {
	var ranges []codeRange
	expectOnePass(t, func(n int) { ranges = codeRanges(unclosedRuns(n)) }, 1<<12)
	if len(ranges) != 0 {
		t.Errorf("codeRanges read %#v in runs nothing closes, want nothing", ranges)
	}
}

// The control of the row above: the same bound fails the old reading (oldCodeRanges), whose
// search for a closing run read the rest of the paragraph again from each opener, on the same
// text.
func TestOnePassBoundFailsTheOldCloserSearch(t *testing.T) {
	if ranges := oldCodeRanges(unclosedRuns(1 << 12)); len(ranges) != 0 {
		t.Errorf("oldCodeRanges read %#v in runs nothing closes, want nothing", ranges)
	}
	expectAboveOnePass(t, func(n int) { oldCodeRanges(unclosedRuns(n)) }, 1<<12)
}

// spanThenBlockUnit is a line holding a code span, parted by a blank line from the fenced block
// after it.
const spanThenBlockUnit = "`a` b\n\n```\nc\n```\n"

// A body of code spans and fenced blocks parted by blank lines: the paragraph breaks are walked
// once for all the stretches the blocks leave, each going on from where the last one stopped
// (appendCodeSpans), where a search of them by halves once a block added a factor of about
// log n.
func TestCodeRangesWalksTheBreaksOnce(t *testing.T) {
	const quarter = 1 << 10
	var ranges []codeRange
	expectOnePass(t, func(n int) { ranges = codeRanges(strings.Repeat(spanThenBlockUnit, n)) }, quarter)
	body := strings.Repeat(spanThenBlockUnit, 4*quarter)
	if len(ranges) != 2*4*quarter {
		t.Fatalf("codeRanges read %d ranges, want a span and a block for each of %d units", len(ranges), 4*quarter)
	}
	for index, candidate := range ranges {
		want := codeRange{start: len(spanThenBlockUnit) * (index / 2), end: len(spanThenBlockUnit)*(index/2) + len("`a`")}
		if index%2 == 1 {
			want = codeRange{start: want.start + len("`a` b\n\n"), end: want.start + len(spanThenBlockUnit), block: true}
		}
		if candidate != want {
			t.Fatalf("range %d is %#v (%q), want %#v", index, candidate, body[candidate.start:candidate.end], want)
		}
	}
}

// The control of the row above: the same bound fails the old reading (oldCodeRanges), which
// searched the breaks by halves once a block for the first one past it, on the same text.
func TestOnePassBoundFailsASearchOfTheBreaksByHalves(t *testing.T) {
	expectAboveOnePass(t, func(n int) { oldCodeRanges(strings.Repeat(spanThenBlockUnit, n)) }, 1<<10)
}

// spanThenReferenceUnit is a key and an address each written in a code span and then outside
// one, which spanThenReferenceResolver names.
const spanThenReferenceUnit = "`CORE-1` CORE-2 `dispatch://CORE-3` dispatch://CORE-4 "

var spanThenReferenceResolver = mapResolver{issues: map[string]bool{"CORE-1": true, "CORE-2": true}, questions: map[string]Ref{}}

// A body of code spans each followed by a reference: the reference spans, then the bare keys
// and `#id`s, are read in order, so each list walks the code ranges once (codeCursor), where a
// search of them by halves once a reference added a factor of about log n.
func TestMentionsMarksCodeInOnePass(t *testing.T) {
	const quarter = 1 << 10
	var located []Located
	expectOnePass(t, func(n int) {
		var err error
		if located, err = Mentions(strings.Repeat(spanThenReferenceUnit, n), "https://dispatch.test", spanThenReferenceResolver); err != nil {
			t.Fatalf("Mentions() error: %v", err)
		}
	}, quarter)
	written := []string{"CORE-1", "CORE-2", "dispatch://CORE-3", "dispatch://CORE-4"}
	if len(located) != len(written)*4*quarter {
		t.Fatalf("Mentions() read %d references, want %d for each of %d units", len(located), len(written), 4*quarter)
	}
	for index, item := range located {
		unit, which := index/len(written), index%len(written)
		offset := unit*len(spanThenReferenceUnit) + strings.Index(spanThenReferenceUnit, written[which])
		if item.Offset != offset || item.InCode != (which%2 == 0) {
			t.Fatalf("reference %d is %#v, want %s at %d, in code %t", index, item, written[which], offset, which%2 == 0)
		}
	}
}

// inCodeSearchedByHalvesOnceAReference is the marking the row above guards against: Mentions'
// own reading, and then, once a reference, the old search of the body's code contexts by halves
// for its first byte (oldInCode).
func inCodeSearchedByHalvesOnceAReference(body string) []bool {
	located, _ := Mentions(body, "https://dispatch.test", spanThenReferenceResolver)
	ranges := codeRanges(body)
	marks := make([]bool, len(located))
	for index, item := range located {
		readerStep()
		marks[index] = oldInCode(ranges, item.Offset)
	}
	return marks
}

// The control of the row above: the same bound fails a marking that searches the code by halves
// once a reference, on the same text.
func TestOnePassBoundFailsAMarkingThatSearchesTheCodeByHalves(t *testing.T) {
	expectAboveOnePass(t, func(n int) { inCodeSearchedByHalvesOnceAReference(strings.Repeat(spanThenReferenceUnit, n)) }, 1<<10)
}

// oldCodeRanges is codeRanges as main's #3125 wrote it, before its search for a closing run read
// each run of the body once and its paragraph breaks were walked once: the answers the reader is
// held to (TestCodeRangesAnswersAsTheOldReading), and the reading the one-pass rows' controls
// fail the bound with. Its loops count steps as the reader's own do, which changes no answer of
// it; so do the two loops a library call made for it, each written out: the search by halves
// slices.BinarySearch made (searchByHalves), and the bytes strings.IndexByte read
// (indexByteSteps).
func oldCodeRanges(body string) []codeRange {
	blocks, breaks := codeBlocks(body)
	if len(blocks) == 0 {
		return oldAppendCodeSpans(nil, body, 0, len(body), breaks)
	}
	ranges := make([]codeRange, 0, 2*len(blocks)+1)
	at := 0
	for _, block := range blocks {
		readerStep()
		ranges = oldAppendCodeSpans(ranges, body, at, block.start, breaks)
		block.block = true
		ranges = append(ranges, block)
		at = block.end
	}
	return oldAppendCodeSpans(ranges, body, at, len(body), breaks)
}

// oldAppendCodeSpans is the old appendCodeSpans, which searched the breaks by halves for the
// first one past from in every stretch.
func oldAppendCodeSpans(spans []codeRange, body string, from, to int, breaks []int) []codeRange {
	// A break on from itself cuts nothing, the paragraph starting there.
	index := searchByHalves(breaks, from+1)
	start := from
	for ; index < len(breaks) && breaks[index] < to; index++ {
		readerStep()
		spans = oldAppendParagraphSpans(spans, body, start, breaks[index])
		start = breaks[index]
	}
	return oldAppendParagraphSpans(spans, body, start, to)
}

// oldAppendParagraphSpans is the old appendParagraphSpans, which looked for each opener's closer
// with oldNextBacktickRun.
func oldAppendParagraphSpans(spans []codeRange, body string, from, to int) []codeRange {
	for at := from; at < to; {
		readerStep()
		if body[at] == '\\' {
			at += 2
			continue
		}
		if body[at] != '`' {
			at++
			continue
		}
		opener := backtickRunEnd(body, at, to)
		width := opener - at
		closer := oldNextBacktickRun(body, opener, to, width)
		if closer < 0 {
			at = opener
			continue
		}
		spans = append(spans, codeRange{start: at, end: closer + width})
		at = closer + width
	}
	return spans
}

// oldNextBacktickRun is the old nextBacktickRun: where the first run of exactly width backticks
// at or after from starts, within [from, to), or -1 when there is none, read from from to the
// paragraph's end for every opener.
func oldNextBacktickRun(text string, from, to, width int) int {
	for at := from; at < to; {
		readerStep()
		found := strings.IndexByte(text[at:to], '`')
		indexByteSteps(found, to-at)
		if found < 0 {
			return -1
		}
		start := at + found
		at = backtickRunEnd(text, start, to)
		if at-start == width {
			return start
		}
	}
	return -1
}

// indexByteSteps counts the bytes strings.IndexByte read to answer found over a stretch of
// length bytes - through the byte it found, or the whole stretch - as the steps of a loop reading
// them one at a time, which is how the reader's own loops count.
func indexByteSteps(found, length int) {
	if readerSteps == nil {
		return
	}
	if found < 0 {
		*readerSteps += length
	} else {
		*readerSteps += found + 1
	}
}

// searchByHalves is slices.BinarySearch(sorted, target)'s index, by the search it makes, each
// turn a step.
func searchByHalves(sorted []int, target int) int {
	low, high := 0, len(sorted)
	for low < high {
		readerStep()
		middle := int(uint(low+high) >> 1)
		if sorted[middle] < target {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low
}

// oldInCode is the old inCode: whether the byte at offset is inside one of ranges, by a search of
// them by halves, which is the search slices.BinarySearchFunc made for it, each turn a step.
func oldInCode(ranges []codeRange, offset int) bool {
	low, high := 0, len(ranges)
	for low < high {
		readerStep()
		middle := int(uint(low+high) >> 1)
		if ranges[middle].end <= offset {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low < len(ranges) && ranges[low].start <= offset
}

// draws is a splitmix64 stream: the same numbers from a seed on every Go release, so the bodies
// generatedBody draws are the same wherever the tests run and whoever rewrites the shared table.
type draws struct{ state uint64 }

// intN is a number in [0, n).
func (d *draws) intN(n int) int {
	d.state += 0x9e3779b97f4a7c15
	z := d.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return int((z ^ (z >> 31)) % uint64(n))
}

// pick is one of choices.
func (d *draws) pick(choices []string) string {
	return choices[d.intN(len(choices))]
}

// What a generated line opens with: nothing, or the markers and indentation of the containers
// and the code blocks code.go reads.
var generatedPrefixes = []string{
	"", "", "", "", "", "", "> ", ">", "> > ", ">     ", "- ", "* ", "+ ", "1. ", "2) ", "-     ",
	"- > ", "> - ", " ", "  ", "   ", "    ", "      ", "\t", " \t", "[^n]: ", "  [^n]: ",
}

// A generated line that is a fence: backticks and tildes, of three and four, with and without
// an info string, one holding a backtick, and a run of two, which is no fence.
var generatedFences = []string{
	"```", "```", "````", "~~~", "~~~", "~~~~", "```go", "``` sh", "~~~ x", "~~~`x", "```a`b", "``", "`````",
}

// A generated line that ends a paragraph or is blank: headings, underlines, thematic breaks and
// blank lines, some of them blank past their spaces or a tab.
var generatedEnders = []string{"", "", "", " ", "\t", "# h", "###### h", "#no", "---", "--", "===", "***", "* * *", "___"}

// What a generated line of prose is written with: words, spaces, tabs, backslashes before
// backticks and before one another, references, and characters of two and of four bytes, one
// UTF-16 unit and two; a run of 1 to 8 backticks is drawn as often as all of these together.
var generatedWords = []string{
	"a", "code", "x y", " ", "  ", "\t", "\\", "\\`", "\\\\", "dispatch://CORE-1", "CORE-2", "#q1", "é", "😀",
	"[l](dispatch://role/r)",
}

// generatedBody is a body of up to twelve lines drawn from d: each line written past some
// containers' markers or indentation as a fence, a blank line, a heading, an underline or a
// thematic break, or prose of 1 to 10 pieces, half of them runs of 1 to 8 backticks, the narrow
// ones drawn most often so that runs close spans, and ended by LF, CR LF or now and then a lone
// CR, which is a line's own character, or by the body's end.
func generatedBody(d *draws) string {
	var body strings.Builder
	lines := 1 + d.intN(12)
	for line := range lines {
		body.WriteString(d.pick(generatedPrefixes))
		switch kind := d.intN(10); {
		case kind < 1:
			body.WriteString(d.pick(generatedFences))
		case kind < 3:
			body.WriteString(d.pick(generatedEnders))
		default:
			for range 1 + d.intN(10) {
				if d.intN(2) == 0 {
					body.WriteString(strings.Repeat("`", 1+d.intN(1+d.intN(8))))
				} else {
					body.WriteString(d.pick(generatedWords))
				}
			}
		}
		if line == lines-1 && d.intN(2) == 0 {
			break
		}
		switch ending := d.intN(20); {
		case ending == 0:
			body.WriteString("\r")
		case ending < 6:
			body.WriteString("\r\n")
		default:
			body.WriteString("\n")
		}
	}
	return body.String()
}

// differentialSeeds are the seeds TestCodeRangesAnswersAsTheOldReading draws its bodies from,
// generatedPerSeed bodies each.
var differentialSeeds = []uint64{1, 2}

const generatedPerSeed = 25_000

// testdataStrings is every string the tables of testdata hold, each once and in order: the bodies
// and texts of the package's shared tables, and their names, references and heads as well.
func testdataStrings(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("testdata/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no table in testdata: %v", err)
	}
	seen := map[string]bool{}
	var walk func(value any)
	walk = func(value any) {
		switch value := value.(type) {
		case string:
			seen[value] = true
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			for _, item := range value {
				walk(item)
			}
		}
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var table any
		if err := json.Unmarshal(raw, &table); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		walk(table)
	}
	found := make([]string, 0, len(seen))
	for value := range seen {
		found = append(found, value)
	}
	slices.Sort(found)
	return found
}

// codeRanges answers what the old reading answered (oldCodeRanges), and codeCursor marks every
// offset, asked in order, as the old search of the ranges by halves did (oldInCode): on every
// string of the tables of testdata and on generatedPerSeed bodies drawn from each of
// differentialSeeds, with backtick runs of 1 to 8, fences, indented code, blank lines, list
// markers, block quotes, references, escapes, tabs and CR LF.
func TestCodeRangesAnswersAsTheOldReading(t *testing.T) {
	differing, spans, blocks := 0, 0, 0
	check := func(body string) {
		ranges := codeRanges(body)
		if old := oldCodeRanges(body); !slices.Equal(ranges, old) {
			if differing++; differing <= 5 {
				t.Errorf("codeRanges(%q) = %v, the old reading %v", body, ranges, old)
			}
			return
		}
		cursor := codeCursor{ranges: ranges}
		for offset := range len(body) + 1 {
			if got, want := cursor.inCode(offset), oldInCode(ranges, offset); got != want {
				if differing++; differing <= 5 {
					t.Errorf("in %q, codeCursor marks offset %d in code %t, the old search %t", body, offset, got, want)
				}
				return
			}
		}
		if slices.ContainsFunc(ranges, func(candidate codeRange) bool { return !candidate.block }) {
			spans++
		}
		if slices.ContainsFunc(ranges, func(candidate codeRange) bool { return candidate.block }) {
			blocks++
		}
	}
	written := testdataStrings(t)
	for _, body := range written {
		check(body)
	}
	for _, seed := range differentialSeeds {
		d := draws{state: seed}
		for range generatedPerSeed {
			check(generatedBody(&d))
		}
	}
	t.Logf("%d strings of testdata and %d bodies drawn from each of seeds %v: %d differ; %d hold a code span and %d a block",
		len(written), generatedPerSeed, differentialSeeds, differing, spans, blocks)
	if differing > 0 {
		t.Errorf("%d bodies read otherwise than the old reading", differing)
	}
}

// updateCodeTable rewrites the shared code table rather than reading it.
var updateCodeTable = flag.Bool("update", false, "rewrite testdata/code-ranges.json from sharedCodeBodies, the bodies drawn from sharedCodeSeed, and CodeRanges")

// codeTableRow is one row of the shared code table (`testdata/code-ranges.json`): a body and the
// code contexts CodeRanges reads in it, in order, each range's offsets counted in UTF-16 units
// as the dashboard's codeRanges counts them, where CodeRanges counts bytes (utf16Offset).
type codeTableRow struct {
	Name   string           `json:"name"`
	Body   string           `json:"body"`
	Ranges []codeTableRange `json:"ranges"`
}

// codeTableRange is one code context of a codeTableRow, the half-open range [Start, End) of its
// body in UTF-16 units, and whether it is a block.
type codeTableRange struct {
	Start int  `json:"start"`
	End   int  `json:"end"`
	Block bool `json:"block"`
}

// namedBody is a body of the shared code table and its name.
type namedBody struct{ name, body string }

// sharedCodeSeed is the seed the shared code table's drawn bodies are drawn from,
// sharedCodeDrawn of them, after its named ones.
const (
	sharedCodeSeed  = 3
	sharedCodeDrawn = 300
)

// sharedCodeBodies is the named bodies of the shared code table: the edges of the shared text
// table's code rows (`packages/contracts/src/dispatch-text-references-code.ts`), the bodies of
// this package's code rows and of the dashboard's (`text-references.test.ts`), the three shapes
// the one-pass rows measure, at a small n, and the edges of the search for closing runs and of
// the offsets the table counts in UTF-16 units.
func sharedCodeBodies() []namedBody {
	const agent = "dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"
	const planner = "[Planner](" + agent + ")"
	return []namedBody{
		{"an indented block after a paragraph and a blank line", "Pasted from the handbook:\n\n    " + agent + "\n"},
		{"a fenced block with an info string after a linked paragraph", "Ask [the reviewer](dispatch://role/reviewer) to run:\n\n```sh\ndispatch send-to-agent " + agent + "\n```\n"},
		{"a code span holding a party", "Run `dispatch message --to dispatch://role/reviewer` before the deploy."},
		{"the four spaces of a nested bullet are its own text", "- step one\n    - ask dispatch://role/reviewer\n"},
		{"a numbered item's paragraph four columns in", "1. step one\n\n    more about dispatch://role/reviewer\n"},
		{"a marker and three spaces indent its item's text four columns", "-   See this:\n\n    dispatch://role/reviewer\n"},
		{"a marker and five spaces leave indented code inside the item", "-     dispatch://role/reviewer\n"},
		{"a lazy line holds an item open over a blank line", "1.  step one\nlazy line\n\n    more about dispatch://role/reviewer\n"},
		{"a fence inside a quote", "> ```\n> dispatch://role/reviewer\n> ```\n"},
		{"a fence inside a quote inside a quote", "> > ```\n> > dispatch://role/reviewer\n> > ```\n"},
		{"an indented block inside a quote", "> See this:\n>\n>     dispatch://role/reviewer\n"},
		{"a fence inside a list item", "- Run this:\n\n  ```\n  dispatch://role/reviewer\n  ```\n"},
		{"a fence inside a quote inside a list item", "- Ask:\n\n  > ```\n  > dispatch://role/reviewer\n  > ```\n"},
		{"a fence closed by the line that leaves its quote", "> ```\n> text\n\ndispatch://role/reviewer\n"},
		{"an indented block under a thematic break", "***\n    dispatch://role/reviewer\n"},
		{"a backtick fence", "```\ndispatch://CORE-1\n```"},
		{"a backtick fence with an info string", "```sh\ndispatch://CORE-1\n```"},
		{"a tilde fence", "~~~\ndispatch://CORE-1\n~~~"},
		{"a fence closed by a longer run", "```\ndispatch://CORE-1\n`````"},
		{"a run too short to close a fence", "````\n``\ndispatch://CORE-1\n````"},
		{"a fence nothing closes", "```\ndispatch://CORE-1"},
		{"fences indented three spaces", "   ```\ndispatch://CORE-1\n   ```"},
		{"prose after a closed fence", "```\nx\n```\ndispatch://CORE-1"},
		{"a tilde fence whose info string holds a backtick", "~~~`sh\ndispatch://CORE-1\n~~~"},
		{"a backtick fence whose info string holds a backtick is no fence", "```a`b\ndispatch://CORE-1\n```"},
		{"an indented block after a blank line", "See:\n\n    dispatch://CORE-1\n"},
		{"an indented block opening the body", "    dispatch://CORE-1\n"},
		{"a block indented by a tab", "See:\n\n\tdispatch://CORE-1\n"},
		{"an indented block holding a blank line", "See:\n\n    x\n\n    dispatch://CORE-1\n"},
		{"four columns under a paragraph are its own text", "See:\n    dispatch://CORE-1\n"},
		{"prose after an indented block", "See:\n\n    x\n\ndispatch://CORE-1"},
		{"a span of one backtick", "Run `dispatch://CORE-1` now."},
		{"a span of two backticks", "Run ``dispatch://CORE-1`` now."},
		{"a longer run inside a span closes nothing", "Run `` dispatch://CORE-1 ``` ``"},
		{"a run nothing closes is literal text", "Run `dispatch://CORE-1 now."},
		{"two spans with prose between them", "`a` dispatch://CORE-1 `b`"},
		{"a span across a line ending", "Run `one\ndispatch://CORE-1` now."},
		{"a backtick inside a fenced block opens nothing", "```\n`\n```\ndispatch://CORE-1"},
		{"prose alone", "See dispatch://CORE-1 now."},
		{"a blank line ends the search of a run nothing closes", "Rename the `foo field.\n\n" + planner + " please check `bar` too."},
		{"an item's marker ends the search of a run nothing closes", "- Rename `foo field\n- ask " + planner + " about `bar`"},
		{"a bare quote marker ends the search of a run nothing closes", "> Rename `foo\n>\n> " + planner + " `bar`"},
		{"a blank line ends a paragraph", "Rename `foo field\n\nask " + planner + " about `bar`"},
		{"an ATX heading ends a paragraph", "Rename `foo field\n# Next up\nask " + planner + " about `bar`"},
		{"a thematic break ends a paragraph", "Rename `foo field\n***\nask " + planner + " about `bar`"},
		{"a -- underline ends a paragraph", "Rename `foo field\n--\nask " + planner + " about `bar`"},
		{"an === underline ends a paragraph", "Rename `foo field\n===\nask " + planner + " about `bar`"},
		{"an indented block under an ATX heading", "# Where to ask\n    dispatch://role/reviewer\n"},
		{"an indented block under an === underline", "Where to ask\n============\n    dispatch://role/reviewer\n"},
		{"a footnote definition's four columns are its own text", "[^who]: who to ask\n\n    dispatch://role/reviewer\n"},
		{"an indented footnote definition's four columns are its own text", "  [^who]: who to ask\n\n    dispatch://role/reviewer\n"},
		{"item 2. ends item 1.'s paragraph", "1. Rename `foo field\n2. ask " + planner + " about `bar`"},
		{"item 2) ends item 1)'s paragraph", "1) Rename `foo field\n2) ask " + planner + " about `bar`"},
		{"an empty item ends the paragraph above it", "- Rename `foo\n-\n  ask " + planner + " about `bar`"},
		{"an indented block under a -- underline", "Where to ask\n--\n    dispatch://role/reviewer\n"},
		{"a lazy === line under a quote is no underline", "> Where to ask\n===\n    dispatch://role/reviewer\n"},
		{"a lazy === line under an item is no underline", "- Where to ask\n===\n    dispatch://role/reviewer\n"},
		{"an escaped backtick opens no span", "Type \\` for a backtick, ask dispatch://role/reviewer, then run `dispatch status`."},
		{"an escaped backtick closes a span as written", "Run `grep \\` here` then ask dispatch://role/reviewer."},
		{"a list marker past the nesting bound is text", strings.Repeat("- ", 51) + "    dispatch://role/reviewer\n"},
		{"four columns after the last list marker within the nesting bound are code", strings.Repeat("- ", 50) + "    dispatch://role/reviewer\n"},
		{"a quote marker past the nesting bound is text", strings.Repeat("> ", 100) + ">     dispatch://role/reviewer\n"},
		{"four columns after the last quote marker within the nesting bound are code", strings.Repeat("> ", 99) + ">     dispatch://role/reviewer\n"},
		{"spans, a fenced block holding backticks and an indented block", "`a` and `b`\n\n```\nfenced `x`\n```\n\n    indented `y`\n\nlast `z`"},
		{"a party in a span and outside it", "Run `dispatch://role/reviewer` and ask dispatch://role/reviewer about it."},
		{"a key in a span and in an indented block", "`CORE-1` and #q2 and\n\n    CORE-1\n"},
		{"a run of two closes no span of one", "`dispatch://CORE-2/artifact/diagram-png`` See dispatch://CORE-2/artifact/design-md. See dispatch://CORE-2/artifact/quoted-md\"."},
		{"lines holding a code span, each followed by a fenced block", strings.Repeat("`CORE-1` and\n```\ncode\n```\n", 4)},
		{"code spans and fenced blocks parted by blank lines", strings.Repeat(spanThenBlockUnit, 4)},
		{"code spans each followed by a reference", strings.Repeat("`CORE-1` CORE-2 ", 4)},
		{"keys and addresses each written in a span and outside one", strings.Repeat(spanThenReferenceUnit, 4)},
		{"unclosed backtick runs of distinct widths", unclosedRuns(64)},
		{"a run a wider opener's search read closes a later span", "`` a ` b ` c"},
		{"a run read inside a span is passed by a later opener", "a ``b` c`` d `e`"},
		{"a wider run inside a span closes nothing", "`a``b`"},
		{"runs read in one paragraph close nothing in the next", "`` a ` b\n\n` c `"},
		{"runs read before a fenced block close nothing after it", "x `` a ` b\n```\nc\n```\n` d `"},
		{"an escaped backtick leaves the rest of its run an opener", "\\``a`"},
		{"characters of two and four bytes before spans", "😀 `é` 😀 ``x`` é"},
		{"CR LF line endings", "a\r\n```\r\nb\r\n```\r\n`c`\r\n\r\n    d\r\n"},
		{"a lone CR is a line's own character", "a `b\rc` d"},
		{"tabs around spans", "a\t`b`\t``c``\t`"},
	}
}

// utf16Offset is the byte offset of body in UTF-16 units, as the dashboard counts its offsets: a
// character of four bytes is two units, any other one.
func utf16Offset(body string, offset int) int {
	units := 0
	for _, char := range body[:offset] {
		units += utf16.RuneLen(char)
	}
	return units
}

// sharedCodeTable is the shared code table as this package writes it: sharedCodeBodies, then
// sharedCodeDrawn bodies drawn from sharedCodeSeed, each with the code contexts CodeRanges reads
// in it, counted in UTF-16 units.
func sharedCodeTable() []codeTableRow {
	bodies := sharedCodeBodies()
	d := draws{state: sharedCodeSeed}
	for index := range sharedCodeDrawn {
		bodies = append(bodies, namedBody{fmt.Sprintf("body %d drawn from seed %d", index+1, sharedCodeSeed), generatedBody(&d)})
	}
	rows := make([]codeTableRow, len(bodies))
	for index, item := range bodies {
		rows[index] = codeTableRow{Name: item.name, Body: item.body, Ranges: tableRanges(item.body)}
	}
	return rows
}

// tableRanges is the code contexts CodeRanges reads in body, counted in UTF-16 units.
func tableRanges(body string) []codeTableRange {
	ranges := []codeTableRange{}
	for _, candidate := range CodeRanges(body) {
		ranges = append(ranges, codeTableRange{Start: utf16Offset(body, candidate.Start), End: utf16Offset(body, candidate.End), Block: candidate.Block})
	}
	return ranges
}

// TestCodeRangesMatchesTheSharedCodeTable holds CodeRanges to the table the dashboard's
// codeRanges is held to as well (`testdata/code-ranges.json`, which
// `packages/dispatch/web/src/features/refs/code-ranges.test.ts` reads), so the two readers find
// the same code contexts in every row's body, each range's offsets counted in UTF-16 units on
// both sides. The table is written from sharedCodeTable, and holds what it writes: run this row
// with -update to rewrite it, and the dashboard's row then says whether its reader agrees.
func TestCodeRangesMatchesTheSharedCodeTable(t *testing.T) {
	want := sharedCodeTable()
	if *updateCodeTable {
		var written bytes.Buffer
		encoder := json.NewEncoder(&written)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(want); err != nil {
			t.Fatalf("write table: %v", err)
		}
		if err := os.WriteFile("testdata/code-ranges.json", written.Bytes(), 0o644); err != nil {
			t.Fatalf("write table: %v", err)
		}
		return
	}
	raw, err := os.ReadFile("testdata/code-ranges.json")
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	var rows []codeTableRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the shared code table is empty")
	}
	for _, row := range rows {
		if got := tableRanges(row.Body); !reflect.DeepEqual(got, row.Ranges) {
			t.Errorf("%s: CodeRanges(%.80q) reads %v in UTF-16 units, the table %v", row.Name, row.Body, got, row.Ranges)
		}
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("testdata/code-ranges.json is not the table sharedCodeTable writes: rewrite it with -update")
	}
}
