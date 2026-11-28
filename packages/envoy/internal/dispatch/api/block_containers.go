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

// containerIndex is the placement index of outer, the stored text of a block's outermost
// container, the block at the document's top level that holds it (refs.OutermostContainers,
// refs.OutermostContainerAt), in which each text of a block that the container holds is placed
// (place). ends is where each line of outer ends, in order: the place of each newline of outer,
// and then outer's end. hashes is the hash (placementHashStep) of outer's bytes before each of
// those places, in the same order, the last of them the whole of outer's, so that the hash of
// outer's bytes from one newline to a later one, or to outer's end, is two of them and one product
// apart. Beside outer, which it shares and does not copy, the index holds 12 bytes for each line of
// outer, 4 for where it ends and 8 for the hash before that. A place fits an int32, since each
// stored text is a Postgres text value, which holds 1 GB at most.
type containerIndex struct {
	outer  string
	ends   []int32
	hashes []uint64
}

// newContainerIndex is the placement index of outer (containerIndex), made in time that grows
// with len(outer): one count of outer's newlines, which sizes ends and hashes, and one pass over
// its bytes, which takes each into the hash of the bytes before it and notes each newline's place
// with the hash of the bytes before it, and then outer's end with the hash of the whole.
func newContainerIndex(outer string) *containerIndex {
	lines := strings.Count(outer, "\n") + 1
	index := &containerIndex{
		outer:  outer,
		ends:   make([]int32, 0, lines),
		hashes: make([]uint64, 0, lines),
	}
	var hash uint64
	for at := range len(outer) {
		if outer[at] == '\n' {
			index.ends = append(index.ends, int32(at))
			index.hashes = append(index.hashes, hash)
		}
		hash = placementHashStep(hash, outer[at])
	}
	index.ends = append(index.ends, int32(len(outer)))
	index.hashes = append(index.hashes, hash)
	return index
}

// place places text, the stored text of a document's block that the index's container holds, in
// the container's text outer, and spends from left, what the request has left of
// placementWorkMax. Both texts are slices of one markdown of the document. A block at the top
// level starts its line, so outer's first line starts at column 0 with the container's own
// syntax, and each of its later lines starts at column 0 too. So the line of outer that text starts
// on is the document's whole line, and its bytes before text's first byte, the block's lead, are
// what every container of the block writes on that line, the outermost and each one inside it:
// containerPrefix of that lead is what they write before each of the block's lines after the
// first. The texts of the containers inside the outermost add nothing to it, so they are not read.
//
// The block index records which blocks hold the block and not where in them it stands, so text is
// found in outer: at a place past outer's first byte, where the outermost container's own syntax
// stands, whose lead containerPrefix accepts and from which outer's bytes are text's up to where a
// line of outer ends, as the block's own bytes end, since the renderer ends each block where a line
// ends. prefix is containerPrefix of the first such place's lead, and ambiguous is whether a later
// such place's lead writes another prefix. They are "" and false where text holds one line, which
// has no later line, and where no place's bytes are text's, which texts of one markdown do not
// give. unplaced is whether the placement passes left: a test that passes the hashes is charged to
// it, and where left holds less than one such test's charge, place stops there and answers unplaced
// with no prefix, and left keeps what it held.
//
// text holds a newline, so its bytes stand at a place of outer only where its first line ends the
// line of outer the place is on, and its first newline is that line's: each line of outer that ends
// in a newline has one place text can start at, that newline less the length of text's first line,
// and none where that place is before the line's start or at outer's first byte. Each such place is
// tested, line by line. Its end, where text from it ends, must be a line's end: the newline that
// stands as many newlines past the line's own as text holds, or outer's end where outer holds
// exactly that many newlines from the line's own on, which one compare of two places tells, and
// which also says that outer's bytes from the line's newline to the end hold as many lines as
// text's later lines. Then the hash of those bytes must be the hash of text's bytes from its first
// newline on (hashes), which one product tells, and which two different strings of that length
// share only by chance (placementHashBase). A place that passes both is charged 1, and the bytes of
// text's first line and of the place's lead, which the test then reads: it compares outer's bytes
// before the line's newline with text's first line, and where they agree containerPrefix reads the
// lead, up to its first byte that no container writes. Up to the first place whose bytes are
// text's, where containerPrefix accepts the lead, the bytes of text's later lines are compared too,
// and prefix is the lead's of the first where they agree. Past it the hashes stand for those bytes:
// place stops at the first later place whose lead containerPrefix accepts and writes another
// prefix, where ambiguous is true, and where no later place does, ambiguous is false.
//
// So placing text costs the hash of its later lines; at each line of outer, a test of index
// arithmetic and one compare, and one product where that compare agrees; and the work charged to
// left, beside one compare of text's later lines at the first place whose bytes are text's, and at
// another only by chance. The time grows with len(text), the lines of outer and the work charged,
// which left bounds, and not with outer's other bytes. A request reads each distinct outermost
// container's bytes once, into one index, and places each distinct text of its blocks in that
// container once (containerPlacements).
//
// The first such place is the block's own unless the outermost container holds the same bytes
// before it, in another block or in the code of a code block. Each line of a block's text after
// the first carries the prefix its lead writes, so such bytes after a lead that writes another
// prefix differ from the block's in every later line but where those lines start with what reads
// as the other prefix, which only code whose lines start with spaces or `>` can do, and such bytes
// after a lead that writes the same prefix, as where each item of a list holds the same code
// block, give the block its own prefix wherever they stand. outer alone cannot tell which place is
// the block's, so the block is ambiguous where two places write different prefixes, before its own
// or after it: its read takes its prefix from its document's parse, which names the bytes of each
// nested block in the markdown, where it can (calledFootnotes, rangedPrefix), and the first
// place's prefix where it cannot (documentBlockPrefix). The block's own place is one of them, so
// where every place writes one prefix, that prefix is the block's, the only one the parse could
// answer: the block takes it and reads no document, where a parse would spend one of the request's
// footnoteDocumentsMax places and could leave a document's footnote definitions unread for nothing.
// A later place whose hashes agree with text's later lines and whose bytes there are not text's,
// which comes only by chance, can only make ambiguous a block that is not, whose read then spends
// one parse that answers that same prefix: testing the places past the first without comparing the
// bytes of text's later lines changes no tree.
func (index *containerIndex) place(text string, left *int) (prefix string, ambiguous, unplaced bool) {
	outer := index.outer
	first := strings.IndexByte(text, '\n')
	if first < 0 || len(text) >= len(outer) {
		return "", false, false
	}
	// later is the length of text from its first newline on, lines how many newlines it holds,
	// want its hash and weight placementHashBase to the power later.
	later, lines := len(text)-first, strings.Count(text[first:], "\n")
	var want uint64
	weight := uint64(1)
	for at := first; at < len(text); at++ {
		want = placementHashStep(want, text[at])
		weight = placementTimes(weight, placementHashBase)
	}
	// Each place's line ends at a newline of starts, and text from it would end at the line end
	// as many lines on, at the same index of ends: text holds at least one newline, so starts
	// holds no end of outer's last line, which no newline ends, and where outer holds fewer
	// newlines than text, no place holds text.
	if lines >= len(index.ends) {
		return "", false, false
	}
	starts, ends, hashes := index.ends[:len(index.ends)-lines], index.ends[lines:], index.hashes
	ends = ends[:len(starts)]
	// found is whether the first place whose bytes are text's is found, whose lead's prefix is
	// prefix.
	found := false
	for at, newline := range starts {
		end := int(ends[at])
		if end-int(newline) != later {
			continue
		}
		start, lead := int(newline)-first, 0
		if at > 0 {
			lead = int(starts[at-1]) + 1
		}
		if start < lead || start == 0 {
			continue
		}
		sum := placementTimes(hashes[at], weight) + want
		if sum >= placementHashModulus {
			sum -= placementHashModulus
		}
		if hashes[at+lines] != sum {
			continue
		}
		charge := 1 + first + start - lead
		if *left < charge {
			return "", false, true
		}
		*left -= charge
		if outer[start:newline] != text[:first] {
			continue
		}
		other, accepted := containerPrefix(outer[lead:start])
		if !accepted {
			continue
		}
		if !found {
			if found = outer[newline:end] == text[first:]; found {
				prefix = other
			}
			continue
		}
		if other != prefix {
			return prefix, true, false
		}
	}
	return prefix, false, false
}

// placementWorkMax is the work that the placements of one request's document blocks in their
// outermost containers may spend on the tests that pass the hashes (containerIndex.place): each
// such test is charged 1, and the bytes of the text's first line and of the place's lead that it
// then reads. A block whose placement would pass what its request has left is unplaced
// (blockPlacement): its read takes its range from its document's parse, within the request's
// footnoteDocumentsMax documents (calledFootnotes), and past those reads as a block whose
// containers the block index does not record (documentBlockPrefix).
//
// Its size is set from a measurement on this service's pod, gVisor on 2 CPUs, of one request
// placing 256 blocks in one container of about a mebibyte, once with 3,000,000 left and once with
// none: the work the bound admits took what the medians of five runs of each differ by. It took
// 4.7 ms where each test was charged 8, the copies of 256 paragraphs whose later lines all agree
// of check c-placed-crafted-cost's distinct-copies; 7.9 ms where each was charged 4,304, mostly a
// lead of 4,002 spaces that containerPrefix reads up to the `x` after them, in a code block whose
// lines are those spaces and 300 `x` beside paragraphs of 1 to 256 `x`; and 0.16 ms where each was
// charged 2,004, mostly a first line of 2,001 bytes compared, in copies of 256 paragraphs whose
// first lines differ in their last three bytes alone. Reading the container's bytes into its index
// took 4.0 to 4.8 ms beside it. An ordinary request spends far less: the 256 paragraphs of check
// c-placed-request-cost, each with a first line of 2,037 bytes and a backslash, spend 522,496, and
// the list of check c-placed-cost and each row of check c-open-fence at most 51.
const placementWorkMax = 3_000_000

// placementHashModulus is the Mersenne prime 2^61 − 1, modulo which a container's placement index
// takes hashes (containerIndex).
const placementHashModulus = 1<<61 - 1

// placementHashBase is the base of a container's placement hashes (placementHashStep), drawn
// once for the process at random from 2 to placementHashModulus − 1, as pmdoc's footnoteHashBase
// is, so that no document can be written whose container holds, at a place a block may start,
// bytes as long as the block's lines after the first that share their hash and not their bytes but
// by chance: two strings of n bytes that differ are two polynomials that differ, which share a hash
// at n−1 of the bases at most, a chance below n in 2^61 at each place a placement tests
// (containerIndex.place).
var placementHashBase = 2 + rand.Uint64N(placementHashModulus-2)

// placementHashStep is the hash of a string with b after it, where hash is the string's. The hash
// of a string is the polynomial whose coefficients are its bytes, the last byte the constant,
// taken at placementHashBase modulo placementHashModulus, and the empty string's is 0, so a
// string's hash is its bytes taken in by this step from the first. The hash of a string is the
// hash of its first m bytes times the base to the power of its length less m, plus the hash of its
// bytes from m on, so the hash of the bytes between two places of a text is the hashes of the
// text's prefixes that end at them and one product apart (containerIndex.place).
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
// (nestedPrefix), and calledFootnotes reads so the containers of each block whose placement in its
// outermost container's text leaves them to its document's parse (blockPlacement): an ambiguous
// block, whose bytes stand at two or more starts that container syntax allows in that text, where
// two of those starts write different prefixes, and an unplaced one, whose placement passed the
// request's placementWorkMax (containerIndex.place).
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

// blockPlacement is what the placement of a document's block in its outermost container answers
// (documentBlockPrefix): prefix, what the block's containers write before each of its lines after
// the first; ambiguous, whether the block's bytes stand at two or more starts that container
// syntax allows in the container's text, where two of those starts write different prefixes,
// which that text alone cannot tell apart, prefix then being the first start's; and unplaced,
// whether the block's placement passed what its request had left of placementWorkMax, prefix then
// being the one the block takes where the block index records no container of it. The read of an
// ambiguous or an unplaced block takes its prefix from its document's parse instead, where that
// parse gives the block a range (calledFootnotes, rangedPrefix), and keeps this one where it gives
// none or the request reads no more documents (footnoteDocumentsMax). A block is never both.
type blockPlacement struct {
	prefix              string
	ambiguous, unplaced bool
}

// documentBlockPrefix is the placement (blockPlacement) of a document's block whose stored text is
// whole, where container, where not nil, is the placement index (containerIndex) of the stored
// text of its outermost container, as the block index records it (refs.OutermostContainers,
// refs.OutermostContainerAt), and left is what the request has left of placementWorkMax: whole
// placed in that index (containerIndex.place), where that answers a prefix that is not "", or
// that the block is ambiguous. A block whose starts all write one prefix takes that prefix, which
// is its own start's, and is not ambiguous.
//
// Otherwise the block takes the prefix the coverage and the results reads take (typedBlocksIn): a
// typed block the prefix its closing fence carries, and any other block none, so it reads as its
// text stands (fencePrefix). That is where the block index records no container of the block:
// container is nil, as for a block at the document's top level, a block whose outermost container
// cannot be read, and a block whose row the graph backfill named with no parent_block_id before a
// reindex set it, which the index cannot tell from a block at the top level (heldContainer); or
// the placement finds whole nowhere in the container's text. It is also where the placement passes
// left, and the block is then unplaced. A typed block at the top level has its closing fence at
// column 0, so the rule answers "" for it, as the placement answers "" where it places a block
// whose containers write nothing before its later lines, and for a text of one line.
func documentBlockPrefix(container *containerIndex, whole string, left *int) blockPlacement {
	if container != nil {
		prefix, ambiguous, unplaced := container.place(whole, left)
		if unplaced {
			return blockPlacement{prefix: fencePrefix(whole), unplaced: true}
		}
		if prefix != "" || ambiguous {
			return blockPlacement{prefix: prefix, ambiguous: ambiguous}
		}
	}
	return blockPlacement{prefix: fencePrefix(whole)}
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

// containerPlacements is the placement (documentBlockPrefix) of each of blocks, the document
// blocks of one request in the order the request names them, whose stored texts are texts, in the
// outermost container that holds it, as outermost answers them (refs.OutermostContainers), each
// spending from left, what the request has left of placementWorkMax. The blocks are taken
// container by container, in the order in which blocks first names a block of each, and in the
// order of blocks within each: the placement index (containerIndex) of a container's text is made
// once, each distinct text of its blocks is placed in it once, the answer standing for every block
// of that text, and the index is dropped once the container's blocks are placed. So a request
// reads each distinct container's bytes once and holds one container's index at a time, each
// distinct text costs one test in constant time at each line of its container beside the work
// charged to left, and the order of blocks alone decides at which block the placements pass left.
// A block outermost names no container of takes the prefix of a block whose containers the block
// index does not record.
func containerPlacements(outermost refs.Outermost, blocks []refs.DocumentBlock, texts []string, left *int) []blockPlacement {
	placed := make([]blockPlacement, len(blocks))
	// containers is each container that holds one of blocks, in the order in which blocks first
	// names one of its blocks, and held the places in blocks of each one's blocks, in order.
	var containers []refs.DocumentBlock
	held := map[refs.DocumentBlock][]int{}
	for at, block := range blocks {
		holder, ok := outermost.Holders[block]
		if !ok {
			placed[at] = documentBlockPrefix(nil, texts[at], left)
			continue
		}
		if _, seen := held[holder]; !seen {
			containers = append(containers, holder)
		}
		held[holder] = append(held[holder], at)
	}
	for _, holder := range containers {
		index := newContainerIndex(outermost.Texts[holder])
		byText := make(map[string]blockPlacement, len(held[holder]))
		for _, at := range held[holder] {
			placement, done := byText[texts[at]]
			if !done {
				placement = documentBlockPrefix(index, texts[at], left)
				byText[texts[at]] = placement
			}
			placed[at] = placement
		}
	}
	return placed
}

// withoutPrefix is whole, the stored text of a document's block, with prefix, what the block's
// containers write before each of its lines after the first (containerIndex.place, rangedPrefix,
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
