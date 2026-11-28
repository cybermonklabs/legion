package api

import (
	"math/bits"
	"math/rand/v2"
	"strings"
)

// footnoteIndent is what a footnote definition writes before each line of its content after the
// line its label opens, the renderer's definitionIndent (pmdoc's renderer.block): four columns,
// whether its first block stands on the label's line or on the line after it.
const footnoteIndent = "    "

// containerPrefix is what the containers of a document's block write before each of the block's
// lines after the first, given lead, the bytes before the block's first byte on the line it
// starts on. On that line the containers opened on an earlier line write what they write before
// each of their later lines, and those opened on the line itself write their opening syntax, which
// the renderer writes before each later line as (pmdoc's renderer.block and renderer.list): a
// quote's `> ` as itself, or `>` where its line holds nothing after it; a list item's marker,
// `- `, `* `, `N. ` or `N) `, as as many spaces, a task item's `[ ] ` or `[x] ` after the marker as
// nothing; a footnote definition's label, `[^label]: `, as the definition's four columns
// (footnoteIndent); and a typed block's opening line as nothing, since a block it holds starts on
// a later line. So lead is read segment by segment, each as containerSegment reads it and written
// as containerSegment says: each space of lead is kept, each `>` is kept with the one space after
// it where it has one, a marker (containerMarker, not `>`) followed by one space is written as
// that many spaces and one more, and a label, each `]` in which the renderer escapes with `\`, is
// written as four spaces. ok is false for a lead that holds any other byte, which no container
// writes before a block.
//
// Lead is read once to judge it and once more to write its prefix, so a lead refused allocates
// nothing, and a lead of spaces and `>` alone is its own prefix, answered with nothing allocated.
func containerPrefix(lead string) (prefix string, ok bool) {
	size, itself := 0, true
	for at := 0; at < len(lead); {
		length, _, spaces := containerSegment(lead[at:])
		if length == 0 {
			return "", false
		}
		if spaces == 0 {
			size += length
		} else {
			size, itself = size+spaces, false
		}
		at += length
	}
	if itself {
		return lead, true
	}
	var written strings.Builder
	written.Grow(size)
	for at := 0; at < len(lead); {
		length, _, spaces := containerSegment(lead[at:])
		if spaces == 0 {
			written.WriteString(lead[at : at+length])
		} else {
			writeSpaces(&written, spaces)
		}
		at += length
	}
	return written.String(), true
}

// containerSegment reads the one segment of container syntax that rest opens with, where rest is
// what is left of a lead (containerPrefix) or of a line (placement.on) from where the segment
// starts: a space; a quote's `>`, with the one space after it where rest has one; a list item's
// marker (containerMarker, not `>`) and the one space after it, with a task item's `[ ] ` or
// `[x] ` after them where rest has one; or a footnote definition's label (footnoteLabel) and the
// one space after it. length is how many bytes of rest the segment spans, and 0 where rest opens
// with no segment. optional is how many of those bytes, at the segment's end, are the space after
// a `>` or the checkbox after a marker's space: a lead that ends before them holds the segment
// without them, so the place where they start ends a lead the renderer writes too. spaces is how
// many spaces the renderer writes for the segment before each later line, the marker's width and
// one more for a list item, whose checkbox it writes as nothing, and four for a label
// (footnoteIndent), and 0 for a space and a `>`, which it writes as they stand.
//
// It reads no byte of rest past the segment's end but the optional end it looks for, and where
// rest opens with no segment, no rest cut shorter from it opens with one. So a lead and the line
// it is cut from read the same segments up to the lead's end, but that the line's last one holds
// an optional end the lead's goes without where the lead ends before it: containerPrefix and
// placement.on cannot disagree on where a lead the renderer writes ends.
func containerSegment(rest string) (length, optional, spaces int) {
	if rest == "" {
		return 0, 0, 0
	}
	switch rest[0] {
	case ' ':
		return 1, 0, 0
	case '>':
		if strings.HasPrefix(rest[1:], " ") {
			return 2, 1, 0
		}
		return 1, 0, 0
	case '[':
		label := footnoteLabel(rest)
		if label == 0 || !strings.HasPrefix(rest[label:], " ") {
			return 0, 0, 0
		}
		return label + 1, 0, len(footnoteIndent)
	}
	marker := containerMarker(rest)
	if marker == 0 || !strings.HasPrefix(rest[marker:], " ") {
		return 0, 0, 0
	}
	if task := rest[marker+1:]; strings.HasPrefix(task, "[ ] ") || strings.HasPrefix(task, "[x] ") {
		return marker + 1 + len("[ ] "), len("[ ] "), marker + 1
	}
	return marker + 1, 0, marker + 1
}

// footnoteLabel is the length of the footnote definition's label that rest starts with, `[^`, the
// label, each `]` in which the renderer escapes with `\`, and `]:`, or 0 where rest starts with
// none.
func footnoteLabel(rest string) int {
	label, definition := strings.CutPrefix(rest, "[^")
	for at := 0; definition && at < len(label); at++ {
		switch label[at] {
		case '\\':
			at++
		case ']':
			if !strings.HasPrefix(label[at+1:], ":") {
				return 0
			}
			return len("[^") + at + len("]:")
		}
	}
	return 0
}

// placedPrefix is what the containers of a document's block write before each of its lines after
// the first, where outer is the stored text of the block's outermost container, the block at the
// document's top level that holds it (refs.OutermostContainers, refs.OutermostContainerAt), and
// text is the block's own stored text. Both texts are slices of one markdown of the document. A
// block at the top level starts its line, so outer's first line starts at column 0 with the
// container's own syntax, and each of its later lines starts at column 0 too. So the line of outer
// that text starts on is the document's whole line, and its bytes before text's first byte, the
// block's lead, are what every container of the block writes on that line, the outermost and each
// one inside it: containerPrefix of that lead is the whole prefix. The texts of the containers
// inside the outermost add nothing to it, so they are not read.
//
// The block index records which blocks hold the block and not where in them it stands, so text is
// found in outer: at the first place past outer's first byte, where the outermost container's own
// syntax stands, whose lead containerPrefix accepts and from which outer's bytes are text's. A
// block starts only where its lead is container syntax, so each line of outer is walked once from
// its start, segment by segment as containerPrefix reads a lead (containerSegment), the line's
// start and each place a segment ends being a place text may start, and the walk of a line stops
// at the first byte that opens no segment, since no lead the renderer writes holds it. At each
// such place a window of len(text) bytes of outer, slid along it one byte at a time, has its hash
// compared with text's (placement), and the bytes are compared only where the two hashes agree,
// which two different strings of that length do only by chance (placementHashBase). So the time
// grows with len(outer) + len(text), whatever bytes their author writes, and the first place
// whose bytes are text's answers containerPrefix of its lead. It is "" where text holds one line,
// which has no later line, and where no place's bytes are text's, which texts of one markdown do
// not give: the block then reads as its text stands, as a block at the top level does.
//
// The first such place is the block's own unless the outermost container holds the same bytes
// before it, in another block or in the code of a code block, after a lead that writes another
// prefix. Each line of a block's text after the first carries the prefix its lead writes, so such
// bytes differ from the block's in every later line but where those lines start with what reads
// as the other prefix, which only code whose lines start with spaces or `>` can do. Such a block
// is placed at the first of them.
func placedPrefix(outer, text string) string {
	if !strings.Contains(text, "\n") || len(text) >= len(outer) {
		return ""
	}
	search := newPlacement(outer, text)
	for line := 0; line <= search.last; {
		end := len(outer)
		if newline := strings.IndexByte(outer[line:], '\n'); newline >= 0 {
			end = line + newline
		}
		if at := search.on(line, end); at >= 0 {
			// The walk ends a lead only where containerPrefix accepts it (containerSegment).
			prefix, _ := containerPrefix(outer[line:at])
			return prefix
		}
		line = end + 1
	}
	return ""
}

// placement is placedPrefix's search for text in outer: a window of len(text) bytes of outer,
// which starts at start and only slides forward, with hash, the hash of the bytes it covers
// (placementHashStep), beside want, text's hash. weight is placementHashBase to the power
// len(text), the factor a byte that leaves the window carries in the hash once the window has
// taken in the byte after its end, and last is the last place in outer the window can start at.
type placement struct {
	outer, text        string
	start, last        int
	hash, want, weight uint64
}

// newPlacement is the search for text in outer with its window at outer's start, for a text no
// longer than outer.
func newPlacement(outer, text string) placement {
	search := placement{outer: outer, text: text, last: len(outer) - len(text), weight: 1}
	for index := range len(text) {
		search.hash = placementHashStep(search.hash, outer[index])
		search.want = placementHashStep(search.want, text[index])
		search.weight = placementTimes(search.weight, placementHashBase)
	}
	return search
}

// on is the first place on outer's line from line to end, its newline or outer's end, at which
// text may start and from which outer's bytes are text's, or -1 where none is. A place text may
// start at is past outer's first byte, at most last, and ends a lead that containerPrefix
// accepts: the line's start, and the end of each segment (containerSegment) of the run of them the
// line opens with, and of each such segment without its optional end. Every other place on the
// line ends a lead that containerPrefix refuses, so the walk stops at the first byte that opens no
// segment.
func (s *placement) on(line, end int) int {
	for at := line; at <= s.last; {
		if at > 0 && s.holds(at) {
			return at
		}
		length, optional, _ := containerSegment(s.outer[at:end])
		if length == 0 {
			return -1
		}
		if bare := at + length - optional; optional > 0 && bare <= s.last && s.holds(bare) {
			return bare
		}
		at += length
	}
	return -1
}

// holds is whether outer's bytes from start are text's, for a start no earlier than the window's
// and at most last. The window slides to start one byte at a time, each step taking the byte
// that enters it into the hash and the byte that leaves it out of it, and the bytes are compared
// only where the hashes agree.
func (s *placement) holds(start int) bool {
	for ; s.start < start; s.start++ {
		leaving := placementTimes(uint64(s.outer[s.start]), s.weight)
		s.hash = placementHashStep(s.hash, s.outer[s.start+len(s.text)])
		if s.hash < leaving {
			s.hash += placementHashModulus
		}
		s.hash -= leaving
	}
	return s.hash == s.want && s.outer[start:start+len(s.text)] == s.text
}

// placementHashModulus is the Mersenne prime 2^61 − 1, modulo which placement takes hashes.
const placementHashModulus = 1<<61 - 1

// placementHashBase is the base of placement's hashes (placementHashStep), drawn once for the
// process at random from 2 to placementHashModulus − 1, as pmdoc's footnoteHashBase is, so that no
// document can be written whose container holds, at a place a block may start, bytes of the
// block's length that share the block's hash and not its bytes but by chance: two strings of n
// bytes that differ are two polynomials that differ, which share a hash at n−1 of the bases at
// most, a chance below n in 2^61 at each place placedPrefix compares.
var placementHashBase = 2 + rand.Uint64N(placementHashModulus-2)

// placementHashStep is the hash of a string with b after it, where hash is the string's. The hash
// of a string is the polynomial whose coefficients are its bytes, the last byte the constant,
// taken at placementHashBase modulo placementHashModulus, and the empty string's is 0, so a
// string's hash is its bytes taken in by this step from the first. The hash of a string without
// its first byte b is the string's hash minus b times the base to the power of the string's
// length less one, so a window slid one byte along a text has its next hash in two products
// (placement.holds).
func placementHashStep(hash uint64, b byte) uint64 {
	hash = placementTimes(hash, placementHashBase) + uint64(b)
	if hash >= placementHashModulus {
		hash -= placementHashModulus
	}
	return hash
}

// placementTimes is a times b modulo placementHashModulus, for a and b below it. The product of
// two numbers under 2^61 is under 2^122, and 2^61 is 1 modulo the modulus, so the product's bits
// from the 61st up, which are under the modulus, added to the 61 bits under them, are the product
// modulo the modulus but for at most the modulus once more.
func placementTimes(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	product := (hi<<3 | lo>>61) + lo&placementHashModulus
	if product >= placementHashModulus {
		product -= placementHashModulus
	}
	return product
}

// lineStart is where the line of text that holds the byte at starts.
func lineStart(text string, at int) int {
	return strings.LastIndexByte(text[:at], '\n') + 1
}

// nestedPrefix is what the containers of a document's block write before each of its lines after
// the first, where text is the block's stored text at a version, markdown that version's markdown
// and nested the byte ranges in it of the blocks that stand in a container and hold more than one
// line (footnoteParse.Nested): containerPrefix of the lead of the first range whose bytes are
// text, and "" where none is, so the block reads as its text stands, as a block at the
// document's top level does.
func nestedPrefix(markdown string, nested [][2]int, text string) string {
	for _, span := range nested {
		if markdown[span[0]:span[1]] == text {
			prefix, _ := containerPrefix(markdown[lineStart(markdown, span[0]):span[0]])
			return prefix
		}
	}
	return ""
}

// fencePrefix is what the containers of a typed block write before each of its lines after the
// first, read from whole, the block's stored text, for the coverage and the results reads
// (typedBlocksIn): the run of spaces and `>` that the last line of whole starts with, where the
// rest of that line is the block's closing fence, colons alone. The renderer writes that fence
// after the prefix of the block's containers (`"\n" + prefix + fence`, pmdoc's renderer.block), so
// the run is that prefix exactly. It is "" for a text of one line and for one whose last line is
// no such fence, which then reads as it stands.
func fencePrefix(whole string) string {
	end := strings.LastIndexByte(whole, '\n')
	if end < 0 {
		return ""
	}
	last := whole[end+1:]
	fence := strings.TrimLeft(last, " >")
	if fence == "" || strings.Trim(fence, ":") != "" {
		return ""
	}
	return last[:len(last)-len(fence)]
}

// withoutPrefix is whole, the stored text of a document's block, with prefix, what the block's
// containers write before each of its lines after the first (placedPrefix, nestedPrefix,
// fencePrefix), taken off each of those lines, from the outermost container in. The text is the
// document's markdown from the block's first byte to its last, so its first line starts past the
// containers' syntax and is kept as it stands. Read alone, the prefix changes the block: a fenced
// code block's or a typed block's closing fence four columns in, or after a `>`, closes nothing, a
// paragraph's line in a quote that starts with `>` opens a quote of its own, and a list item's
// line two columns in opens a list inside the item before it.
//
// Each line after the first has the segments of prefix that it carries taken off, in order
// (carriedSegments): a `>`, with the one space after it where it has one, and a run of spaces.
// From the first segment it does not carry, the rest of the line is kept as it stands: a
// paragraph's lazy continuation line carries none, and a blank line can carry fewer than all,
// since the renderer writes a blank line between blocks without a list item's indentation and a
// quote's as `>` alone, and a blank line of code in a footnote definition without the
// definition's indentation. So a blank line of code in a footnote definition that holds fewer
// spaces than the definition's indentation keeps them, as its document reads them, and one that
// holds as many or more has the definition's columns taken off, which the text cannot tell from
// the code's. A prefix of "" answers whole, with nothing allocated.
func withoutPrefix(whole, prefix string) string {
	if prefix == "" {
		return whole
	}
	first, rest, found := strings.Cut(whole, "\n")
	if !found {
		return whole
	}
	var written strings.Builder
	written.Grow(len(whole))
	written.WriteString(whole[:len(first)+1])
	for line := range strings.Lines(rest) {
		_, on := carriedSegments(prefix, line)
		written.WriteString(line[on:])
	}
	return written.String()
}

// carriedSegments walks prefix, a run of spaces and `>`, and line from their starts, segment by
// segment as withoutPrefix takes them off, while line carries each segment of prefix: a `>` where
// prefix has one, taken with the one space after it in each where it has one, and at least as
// many spaces as a run of prefix holds, of which that many are taken. at is how many bytes of
// prefix line carries, and on how many bytes of line those segments span.
func carriedSegments(prefix, line string) (at, on int) {
	for at < len(prefix) {
		if prefix[at] == '>' {
			if on == len(line) || line[on] != '>' {
				return at, on
			}
			at, on = at+1, on+1
			if at < len(prefix) && prefix[at] == ' ' {
				at++
			}
			if on < len(line) && line[on] == ' ' {
				on++
			}
			continue
		}
		spaces := spacesAt(prefix, at)
		if spacesAt(line, on) < spaces {
			return at, on
		}
		at, on = at+spaces, on+spaces
	}
	return at, on
}

// spacesAt is how many spaces text holds from at on.
func spacesAt(text string, at int) int {
	return len(text[at:]) - len(strings.TrimLeft(text[at:], " "))
}
