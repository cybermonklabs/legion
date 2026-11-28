package api

import (
	"math/bits"
	"math/rand/v2"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
		length, spaces := containerSegment(lead[at:])
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
		length, spaces := containerSegment(lead[at:])
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
// what is left of a lead (containerPrefix) from where the segment starts: a space; a quote's `>`,
// with the one space after it where rest has one; a list item's marker (containerMarker, not `>`)
// and the one space after it, with a task item's `[ ] ` or `[x] ` after them where rest has one;
// or a footnote definition's label (footnoteLabel) and the one space after it. length is how many
// bytes of rest the segment spans, and 0 where rest opens with no segment. spaces is how many
// spaces the renderer writes for the segment before each later line, the marker's width and one
// more for a list item, whose checkbox it writes as nothing, and four for a label
// (footnoteIndent), and 0 for a space and a `>`, which it writes as they stand.
func containerSegment(rest string) (length, spaces int) {
	if rest == "" {
		return 0, 0
	}
	switch rest[0] {
	case ' ':
		return 1, 0
	case '>':
		if strings.HasPrefix(rest[1:], " ") {
			return 2, 0
		}
		return 1, 0
	case '[':
		label := footnoteLabel(rest)
		if label == 0 || !strings.HasPrefix(rest[label:], " ") {
			return 0, 0
		}
		return label + 1, len(footnoteIndent)
	}
	marker := containerMarker(rest)
	if marker == 0 || !strings.HasPrefix(rest[marker:], " ") {
		return 0, 0
	}
	if task := rest[marker+1:]; strings.HasPrefix(task, "[ ] ") || strings.HasPrefix(task, "[x] ") {
		return marker + 1 + len("[ ] "), marker + 1
	}
	return marker + 1, marker + 1
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
// found in outer: at a place past outer's first byte, where the outermost container's own syntax
// stands, whose lead containerPrefix accepts and from which outer's bytes are text's. prefix is
// containerPrefix of the first such place's lead, and ambiguous is whether a later place whose
// lead containerPrefix accepts holds text's bytes too and its lead writes another prefix, which
// the placement index of outer finds (containerIndex.place). It is "" and not ambiguous where text
// holds one line, which has no later line, and where no place's bytes are text's, which texts of
// one markdown do not give: the block is then read as one whose containers the block index does
// not record (documentBlockPrefix). placedPrefix makes the index of outer for text alone
// (newContainerIndex), so it reads outer's bytes once and then costs what place costs; a request
// that places several blocks of one container makes the container's index once and places each of
// them in it (blockTrees).
//
// The first such place is the block's own unless the outermost container holds the same bytes
// before it, in another block or in the code of a code block. Each line of a block's text after
// the first carries the prefix its lead writes, so such bytes after a lead that writes another
// prefix differ from the block's in every later line but where those lines start with what reads
// as the other prefix, which only code whose lines start with spaces or `>` can do, and such bytes
// after a lead that writes the same prefix, as where each item of a list holds the same code
// block, give the block its own prefix wherever they stand. outer alone cannot tell which place is
// the block's, so the block is ambiguous where its bytes stand at two or more starts that
// container syntax allows in outer, where two of those starts write different prefixes, before
// its own or after it: its read takes its prefix from its document's parse, which names the bytes
// of each nested block in the markdown, where it can (calledFootnotes, rangedPrefix), and the
// first place's prefix where it cannot (documentBlockPrefix). The block's own start is one of the
// places, so where every place writes one prefix, that prefix is the block's, the only one the
// parse could answer: the block takes it and reads no document, where a parse would spend one of
// the request's footnoteDocumentsMax places and could leave a document's footnote definitions
// unread for nothing. A later place whose test agrees with text (containerIndex.agrees) and whose
// bytes are not text's, which comes only by chance, can only make ambiguous a block that is not,
// whose read then spends one parse that answers that same prefix: testing the places past the
// first without comparing their bytes changes no tree.
func placedPrefix(outer, text string) (prefix string, ambiguous bool) {
	if !strings.Contains(text, "\n") || len(text) >= len(outer) {
		return "", false
	}
	return newContainerIndex(outer).place(text)
}

// containerIndex is the placement index of outer, the stored text of a block's outermost
// container (placedPrefix), in which each text of a block that the container holds is placed in
// time that grows with outer's lines and not with its bytes (place). newlines is where each
// newline of outer stands, in order. sums is the hash (placementHashStep) of outer's first
// k·placementStride bytes for each k from 0 to len(outer)/placementStride, from which the hash of
// outer's first n bytes, for any n, is the one sums holds for the multiple of placementStride at
// or below n with the at most placementStride − 1 bytes from there to n taken in (prefixHash),
// and the hash of the bytes between any two places of outer is two such hashes and one product
// (agrees). Beside outer, which it shares and does not copy, the index holds 4 bytes for each line
// of outer and 8 for each placementStride of its bytes, an eighth of its length. A newline's place
// fits an int32, since each stored text is a Postgres text value, which holds 1 GB at most.
type containerIndex struct {
	outer    string
	newlines []int32
	sums     []uint64
}

// placementStride is how many bytes of a container's text lie between two places whose prefix
// hashes its placement index holds (containerIndex): 64, so that those hashes take an eighth of
// the text's length, and the hash of any other prefix takes in at most 63 bytes more.
const placementStride = 64

// newContainerIndex is the placement index of outer (containerIndex), made in time that grows
// with len(outer): one count of outer's newlines, which sizes newlines, and one pass over its
// bytes, which takes each into the hash of the prefix it ends and notes each newline's place.
func newContainerIndex(outer string) *containerIndex {
	index := &containerIndex{
		outer:    outer,
		newlines: make([]int32, 0, strings.Count(outer, "\n")),
		sums:     make([]uint64, 0, len(outer)/placementStride+1),
	}
	var hash uint64
	for at := range len(outer) {
		if at%placementStride == 0 {
			index.sums = append(index.sums, hash)
		}
		if outer[at] == '\n' {
			index.newlines = append(index.newlines, int32(at))
		}
		hash = placementHashStep(hash, outer[at])
	}
	if len(outer)%placementStride == 0 {
		index.sums = append(index.sums, hash)
	}
	return index
}

// place is placedPrefix of the index's outer and text: of the places past outer's first byte whose
// lead containerPrefix accepts and from which outer's bytes are text's, prefix is what the first
// one's lead writes, and ambiguous is whether a later one's lead writes another. text holds a
// newline, so its bytes stand at a place of outer only where its first line ends the line of outer
// the place is on, and its first newline is that line's: each line of outer that ends in a
// newline has one place text can start at, that newline less the length of text's first line, and
// none where that place is before the line's start or at outer's first byte, or where text from it
// runs past outer's end, as from every later line too. Each such place, line by line, is tested:
// the up to 8 bytes at each end of the window of len(text) bytes from it are compared with text's,
// and then the window's hash with text's (agrees), which two different strings of len(text) bytes
// share only by chance (placementHashBase). Up to the first place whose bytes are text's,
// containerPrefix reads the lead of each place that passes the test, and the window's bytes are
// compared with text's where it accepts the lead; prefix is containerPrefix of the lead of the
// first whose bytes are text's. Past it the test stands for the bytes: at each later place that
// passes it and whose lead containerPrefix accepts, the prefix that lead writes is compared with
// prefix, and place stops at the first that differs, where ambiguous is true; where it reaches
// outer's last line with none that differs, ambiguous is false.
//
// So placing text costs the hash of text; one test of each line of outer, which compares at most
// 16 bytes and takes at most 2·(placementStride − 1) bytes into hashes; containerPrefix of the
// lead of each line whose test passes, which holds text's bytes from its one place but by chance,
// and which containerPrefix reads up to its first byte that no container writes; and one compare
// of len(text) bytes, up to the first place whose bytes are text's. The time grows with len(text),
// the lines of outer and the leads of the lines that hold copies of text, wherever their author
// writes them, and not with outer's other bytes. A request makes one index of each distinct
// outermost container of its blocks and places each of the blocks it holds in it (blockTrees), so
// it reads each container's bytes once, and each block then costs its text's length and its
// container's lines.
func (index *containerIndex) place(text string) (prefix string, ambiguous bool) {
	outer := index.outer
	first := strings.IndexByte(text, '\n')
	if first < 0 || len(text) >= len(outer) {
		return "", false
	}
	var want uint64
	weight := uint64(1)
	for at := range len(text) {
		want = placementHashStep(want, text[at])
		weight = placementTimes(weight, placementHashBase)
	}
	probe := min(len(text), 8)
	// found is whether the first place whose bytes are text's is found, whose lead's prefix is
	// prefix. line is where the line that ends at the next newline starts.
	found, line := false, 0
	for _, newline := range index.newlines {
		start, lead := int(newline)-first, line
		line = int(newline) + 1
		end := start + len(text)
		if end > len(outer) {
			break
		}
		if start < lead || start == 0 || outer[start:start+probe] != text[:probe] ||
			outer[end-probe:end] != text[len(text)-probe:] || !index.agrees(start, end, want, weight) {
			continue
		}
		other, accepted := containerPrefix(outer[lead:start])
		if !accepted {
			continue
		}
		if !found {
			if found = outer[start:end] == text; found {
				prefix = other
			}
			continue
		}
		if other != prefix {
			return prefix, true
		}
	}
	return prefix, false
}

// agrees is whether the hash of outer's bytes from start to end is want, where weight is
// placementHashBase to the power end − start. The hash of outer's first end bytes is the hash of
// its first start bytes times that power plus the hash of the bytes from start to end
// (placementHashStep), so it compares the one with the other two.
func (index *containerIndex) agrees(start, end int, want, weight uint64) bool {
	sum := placementTimes(index.prefixHash(start), weight) + want
	if sum >= placementHashModulus {
		sum -= placementHashModulus
	}
	return index.prefixHash(end) == sum
}

// prefixHash is the hash of outer's first n bytes: the hash sums holds for the multiple of
// placementStride at or below n, with each of the at most placementStride − 1 bytes from there to
// n taken in by placementHashStep.
func (index *containerIndex) prefixHash(n int) uint64 {
	hash := index.sums[n/placementStride]
	for at := n - n%placementStride; at < n; at++ {
		hash = placementHashStep(hash, index.outer[at])
	}
	return hash
}

// placementHashModulus is the Mersenne prime 2^61 − 1, modulo which a container's placement index
// takes hashes (containerIndex).
const placementHashModulus = 1<<61 - 1

// placementHashBase is the base of a container's placement hashes (placementHashStep), drawn
// once for the process at random from 2 to placementHashModulus − 1, as pmdoc's footnoteHashBase
// is, so that no document can be written whose container holds, at a place a block may start,
// bytes of the block's length that share the block's hash and not its bytes but by chance: two
// strings of n bytes that differ are two polynomials that differ, which share a hash at n−1 of the
// bases at most, a chance below n in 2^61 at each place a placement tests (containerIndex.place).
var placementHashBase = 2 + rand.Uint64N(placementHashModulus-2)

// placementHashStep is the hash of a string with b after it, where hash is the string's. The hash
// of a string is the polynomial whose coefficients are its bytes, the last byte the constant,
// taken at placementHashBase modulo placementHashModulus, and the empty string's is 0, so a
// string's hash is its bytes taken in by this step from the first. The hash of a string is the
// hash of its first m bytes times the base to the power of its length less m, plus the hash of its
// bytes from m on, so the hash of the bytes between two places of a text is the hashes of the
// text's prefixes that end at them and one product apart (containerIndex.agrees).
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

// rangedPrefix is what the containers of a document's block write before each of its lines after
// the first, where text is the block's stored text at a version, markdown that version's markdown
// and nested the byte ranges in it of the blocks that stand in a container and hold more than one
// line (footnoteParse.Nested): containerPrefix of the lead of the first range whose bytes are
// text, and found, whether one is. No range's bytes are text for a block at the document's top
// level, nor for any block where nested holds none, as a parse that works out no ranges holds
// none, and one whose rendering is not markdown byte for byte (footnoteParseOf). The reference
// graph's read reads the containers of a pin so where the graph store states none at its version
// (nestedPrefix), and calledFootnotes reads so the containers of each ambiguous block, whose
// bytes stand at two or more starts that container syntax allows in its outermost container's
// text, where two of those starts write different prefixes (placedPrefix).
func rangedPrefix(markdown string, nested [][2]int, text string) (prefix string, found bool) {
	for _, span := range nested {
		if markdown[span[0]:span[1]] == text {
			prefix, _ = containerPrefix(markdown[lineStart(markdown, span[0]):span[0]])
			return prefix, true
		}
	}
	return "", false
}

// nestedPrefix is what the containers of a document's block write before each of its lines after
// the first, where text is the block's stored text at a version, markdown that version's markdown
// and nested the byte ranges in it of the blocks that stand in a container and hold more than one
// line (footnoteParse.Nested): the prefix of the first range whose bytes are text (rangedPrefix).
// Where no range's bytes are text, as for a block at the document's top level and for every block
// where nested holds none, the block is read as one whose containers the block index does not
// record (documentBlockPrefix): a typed block takes the prefix its closing fence carries
// (fencePrefix), and any other block reads as its text stands.
func nestedPrefix(markdown string, nested [][2]int, text string) string {
	if prefix, found := rangedPrefix(markdown, nested, text); found {
		return prefix
	}
	return fencePrefix(text)
}

// fencePrefix is what the containers of a typed block write before each of its lines after the
// first, read from whole, the block's stored text, alone: the run of spaces and `>` that the last
// line of whole starts with, where the first line of whole opens a typed block, read as
// blockTextContent reads a document's block's opening line (pmdoc.ParseTypedDirectiveOpen), and
// the rest of its last line is the block's closing fence, colons alone. The renderer writes that
// fence after the prefix of the block's containers (`"\n" + prefix + fence`, pmdoc's
// renderer.block), so the run is that prefix exactly. The coverage and the results reads take a
// typed block's prefix from it (typedBlocksIn), and so does every read of a document's block
// whose containers the block index does not record (documentBlockPrefix, nestedPrefix). It is ""
// for a text of one line, for one whose last line is no such fence, and for one whose first line
// opens no typed block, such as a list whose last item ends in a typed block, which then reads as
// it stands.
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
	opening, _, _ := strings.Cut(whole, "\n")
	if _, _, typed := pmdoc.ParseTypedDirectiveOpen(strings.TrimRight(opening, "\r")); !typed {
		return ""
	}
	return last[:len(last)-len(fence)]
}

// documentBlockPrefix is what the containers of a document's block write before each of its lines
// after the first, where whole is the block's stored text and container, where not nil, is the
// placement index (containerIndex) of the stored text of its outermost container, as the block
// index records it (refs.OutermostContainers, refs.OutermostContainerAt): whole placed in it
// (containerIndex.place, placedPrefix), where that is not "" or the block is ambiguous, and
// ambiguous, whether it is. An ambiguous block's bytes stand at two or more starts that container
// syntax allows in the container's text, where two of those starts write different prefixes,
// which that text alone cannot tell apart, and prefix is the first start's: its read takes the
// prefix from its document's parse instead, where that parse gives the block a range
// (calledFootnotes, rangedPrefix), and keeps this one where it gives none or the request reads no
// more documents (footnoteDocumentsMax). A block whose starts all write one prefix takes that
// prefix, which is its own start's, and is not ambiguous.
//
// Otherwise the block index records no container of the block: container is nil, as for a block
// at the document's top level, a block whose outermost container cannot be read, and a block whose
// row the graph backfill named with no parent_block_id before a reindex set it, which the index
// cannot tell from a block at the top level (heldContainer); or the placement finds whole nowhere
// in the container's text. The block then takes the prefix the coverage and the results reads take
// (typedBlocksIn): a typed block the prefix its closing fence carries, and any other block none, so
// it reads as its text stands (fencePrefix). A typed block at the top level has its closing fence
// at column 0, so the rule answers "" for it, as the placement answers "" where it places a block
// whose containers write nothing before its later lines, and for a text of one line. Such a block
// is not ambiguous.
func documentBlockPrefix(container *containerIndex, whole string) (prefix string, ambiguous bool) {
	if container != nil {
		if prefix, ambiguous = container.place(whole); prefix != "" || ambiguous {
			return prefix, ambiguous
		}
	}
	return fencePrefix(whole), false
}

// heldContainer is the placement index of outer (containerIndex) where held is true, outer then
// being the stored text of the outermost container the block index records for one block, whose
// texts are placed in it alone (documentBlockPrefix), and nil where held is false, where the
// index records none.
func heldContainer(outer string, held bool) *containerIndex {
	if !held {
		return nil
	}
	return newContainerIndex(outer)
}

// containerPlacements places the document blocks of one request in the outermost containers that
// hold them, as refs.OutermostContainers answers them (blockTrees): it makes the placement index
// (containerIndex) of a container's text when it first places one of the container's blocks, and
// places each later block of that container in the same index, so the request reads each
// distinct container's bytes once however many of its blocks it names.
type containerPlacements struct {
	outermost refs.Outermost
	indexes   map[refs.DocumentBlock]*containerIndex
}

// newContainerPlacements is the placement of the blocks outermost names in their containers, with
// no index made yet.
func newContainerPlacements(outermost refs.Outermost) containerPlacements {
	return containerPlacements{outermost: outermost, indexes: make(map[refs.DocumentBlock]*containerIndex, len(outermost.Texts))}
}

// prefix is documentBlockPrefix of whole, block's stored text, in the index of block's outermost
// container, and with no container where outermost names none for block.
func (p containerPlacements) prefix(block refs.DocumentBlock, whole string) (prefix string, ambiguous bool) {
	holder, held := p.outermost.Holders[block]
	if !held {
		return documentBlockPrefix(nil, whole)
	}
	index := p.indexes[holder]
	if index == nil {
		index = newContainerIndex(p.outermost.Texts[holder])
		p.indexes[holder] = index
	}
	return documentBlockPrefix(index, whole)
}

// withoutPrefix is whole, the stored text of a document's block, with prefix, what the block's
// containers write before each of its lines after the first (placedPrefix, rangedPrefix,
// nestedPrefix, fencePrefix), taken off each of those lines, from the outermost container in. The
// text is the document's markdown from the block's first byte to its last, so its first line starts
// past the containers' syntax and is kept as it stands. Read alone, the prefix changes the block: a
// fenced code block's or a typed block's closing fence four columns in, or after a `>`, closes
// nothing, a paragraph's line in a quote that starts with `>` opens a quote of its own, and a list
// item's line two columns in opens a list inside the item before it.
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
		written.WriteString(line[carriedSegments(prefix, line):])
	}
	return written.String()
}

// carriedSegments is how many bytes of line the segments of prefix that it carries span, where
// prefix is a run of spaces and `>`: prefix and line are walked from their starts, segment by
// segment as withoutPrefix takes them off, while line carries each segment of prefix, a `>` where
// prefix has one, taken with the one space after it in each where it has one, and at least as
// many spaces as a run of prefix holds, of which that many are taken.
func carriedSegments(prefix, line string) int {
	at, on := 0, 0
	for at < len(prefix) {
		if prefix[at] == '>' {
			if on == len(line) || line[on] != '>' {
				return on
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
			return on
		}
		at, on = at+spaces, on+spaces
	}
	return on
}

// spacesAt is how many spaces text holds from at on.
func spacesAt(text string, at int) int {
	return len(text[at:]) - len(strings.TrimLeft(text[at:], " "))
}
