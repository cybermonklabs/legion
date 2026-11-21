package pmdoc

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/maphash"
	"unicode/utf8"

	"github.com/yuin/goldmark/util"
)

// A run's spelling (runSpelling) is chosen for the whole run, and a run can hold two places that
// call for spellings no one spelling holds both of: a tilde beside a delimiter run that reads back
// only where it is written `\~`, and a letter that must be written as a reference for a delimiter
// run beside it to open or close. Such a run is cut into segments at spaces, and each segment is
// written in a spelling of its own (spellBySegment).
//
// A cut is an ASCII space in a text no mark is written over, with a character on either side of
// it, in that text or in the text node beside it, that is no whitespace (runCutsOf). The writer
// writes such a space as it is: it escapes a space only at the start or the end of a line, of the
// run or of a mark's text. A segment, the markdown between two cuts, read alone reads as it reads
// in the run:
//   - Every reader decides whether a delimiter run (`*`, `_`, `~`) can open or close by the
//     character on each side of it, and counts the start and the end of the markdown as
//     whitespace, as the space is: a delimiter run beside a cut flanks alone as it flanks in the
//     run. A bare URL, a www URL and an email end at whitespace, and an entity, a backslash escape
//     and a hard break hold none.
//   - Where the run reads back as written, the cut's space is text under no mark, so no emphasis,
//     code span, link, autolink, raw HTML, footnote reference or address the readers read spans
//     it: each lies inside one segment and is read from that segment's bytes alone. A delimiter
//     run is paired with the nearest one before it that it can pair with, and every delimiter run
//     a segment pairs lies inside it, so a segment pairs its runs alone as it pairs them in the
//     run. So a segment that does not read back alone under a reader means the run does not read
//     back under it either (runSegments.read).
//   - The other way, a run of segments that each read back alone reads back whole where no
//     delimiter run left unpaired in one segment pairs with one left unpaired in another, which
//     crossingEscapes keeps from happening by escaping each delimiter character the run's text
//     could leave unpaired in two places. What else could pair across a cut (a `[` with a `](` in
//     a later segment) is written alike in every spelling, and the run is read back whole before
//     it is kept.

// segmentGroupBytes is how much text a group of a run's segments holds at least (runSegments): a
// search reads a writing back a group at a time, so a run of short words is not read in thousands
// of parses, each with a parse's own cost, while one group's change is read again alone.
const segmentGroupBytes = 4096

// runCut is a space a run can be cut at: the byte offset of an ASCII space in the text of
// nodes[node].
type runCut struct {
	node, offset int
}

// runCutsOf is every space nodes can be cut at: an ASCII space in a text no mark is written over
// (visibleMarks), with a character on either side of it that is no whitespace to the parser, in
// that text or, where the space begins or ends it, in the text node beside it, whatever its marks.
// The writer writes such a space as it is, since it begins no line and ends no line, run or mark's
// text, and the segments it bounds begin and end in no whitespace the parser would trim: a text
// beside it under a mark is written with the mark's delimiter, bracket or backtick next to the
// space, or, a bare URL, with its own character there.
func runCutsOf(nodes []*Node) []runCut {
	var cuts []runCut
	for index, node := range nodes {
		if node.Type != "text" || len(visibleMarks(node.Marks)) > 0 {
			continue
		}
		for offset := range len(node.Text) {
			if node.Text[offset] != ' ' {
				continue
			}
			before, beside := runeBefore(nodes, index, offset)
			after, follows := runeAfter(nodes, index, offset+1)
			if beside && follows && !util.IsSpaceRune(before) && !util.IsSpaceRune(after) {
				cuts = append(cuts, runCut{node: index, offset: offset})
			}
		}
	}
	return cuts
}

// runeBefore is the character before offset in the text nodes[index], or, at its start, the last
// of the text node before it, and whether there is one.
func runeBefore(nodes []*Node, index, offset int) (rune, bool) {
	if offset > 0 {
		char, _ := utf8.DecodeLastRuneInString(nodes[index].Text[:offset])
		return char, true
	}
	if index > 0 && nodes[index-1].Type == "text" && nodes[index-1].Text != "" {
		char, _ := utf8.DecodeLastRuneInString(nodes[index-1].Text)
		return char, true
	}
	return 0, false
}

// runeAfter is the character at offset in the text nodes[index], or, at its end, the first of the
// text node after it, and whether there is one.
func runeAfter(nodes []*Node, index, offset int) (rune, bool) {
	if offset < len(nodes[index].Text) {
		char, _ := utf8.DecodeRuneInString(nodes[index].Text[offset:])
		return char, true
	}
	if index+1 < len(nodes) && nodes[index+1].Type == "text" && nodes[index+1].Text != "" {
		char, _ := utf8.DecodeRuneInString(nodes[index+1].Text)
		return char, true
	}
	return 0, false
}

// runSegments is a run cut into segments at its cuts (runCutsOf): segment k runs from cut k-1, or
// the run's start, to cut k, or its end, the cuts' spaces in none. A search over the run's spellings
// (inlineWithEscapes) records where each writing put each cut, reads the writing back a group of
// segments at a time, and keeps, for each writing, what each group read back as; where no writing
// reads back whole, the run is written in a spelling for each segment chosen from those readings
// (spellBySegment).
type runSegments struct {
	nodes  []*Node
	prefix string
	cuts   []runCut
	// spellings, while the run is written by segments, is the spelling each segment is written in;
	// nil while every segment is written in the run's one spelling.
	spellings []runSpelling
	// from is where the writing being made begins in the renderer's markdown, at is where it wrote
	// each cut's space there, and fused is whether it fused two delimiter runs in each segment.
	from  int
	at    []int
	fused []bool
	// broken is set by a writing that did not put each cut's space where the run's cuts say: no
	// writing of the run is then written by segments.
	broken bool
	// groups is the first segment of each group: segments from the run's start, or the one after
	// a group, to the first cut at least segmentGroupBytes of text past where the group begins.
	// groupLabels and groupSignatures are each group's referencedLabels and inlineSignature.
	groups                       []int
	groupLabels, groupSignatures [][]string
	// readings is each group markdown read so far, and readingOf its index there by the group and
	// the digest of what it wrote.
	readings  []segmentReading
	readingOf map[segmentKey]int
	// segmentReads is how far each segment markdown read alone reads back, by the segment and the
	// digest of its bytes, and segmentNodes the nodes of each segment read so (spellBySegment).
	segmentReads map[segmentKey]int
	segmentNodes map[int][]*Node
	// rows is what each writing of the search read back as, in the order it was written.
	rows []segmentRow
}

// segmentKey names a group's or a segment's markdown by its index and a 128-bit digest of its
// bytes (segmentSeeds).
type segmentKey struct {
	index int
	sum   [2]uint64
}

// segmentSeeds are the seeds of the two 64-bit digests a segmentKey holds. A search digests every
// group of every writing it makes, and a run no spelling reads back is written in dozens: a
// cryptographic digest of each costs more than reading the changed groups back, and nothing here
// is adversarial, so two independent seeded hashes stand for one 128-bit digest.
var segmentSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

// segmentReading is a group's markdown read back: how many readers, from the first, it reads back
// under (readsBackUnder), and, where that is not every reader, its bytes and where each cut inside
// it is written in them, for its segments to be read back one at a time (spellBySegment).
type segmentReading struct {
	depth   int
	written []byte
	cuts    []int
}

// segmentRow is what one writing of a run read back as: its spelling, the index in readings of
// what each group read back as, and, for a spelling that orders marks otherwise than as written
// and is not one of finalResorts' (flankingFused), each segment it fused two delimiter runs in,
// where it is never kept.
type segmentRow struct {
	spelling runSpelling
	readings []int
	fused    []int
}

// newRunSegments is nodes, a run whose lines after the first are written behind prefix, cut at
// each of its cuts, or nil where it holds none.
func newRunSegments(nodes []*Node, prefix string) *runSegments {
	cuts := runCutsOf(nodes)
	if len(cuts) == 0 {
		return nil
	}
	s := &runSegments{
		nodes:        nodes,
		prefix:       prefix,
		cuts:         cuts,
		fused:        make([]bool, len(cuts)+1),
		groups:       []int{0},
		readingOf:    map[segmentKey]int{},
		segmentReads: map[segmentKey]int{},
		segmentNodes: map[int][]*Node{},
	}
	// textBefore is how much text the nodes before each node hold.
	textBefore := make([]int, len(nodes))
	for index := 1; index < len(nodes); index++ {
		textBefore[index] = textBefore[index-1] + len(nodes[index-1].Text)
	}
	groupFrom := 0
	for index, cut := range cuts {
		if at := textBefore[cut.node] + cut.offset; at-groupFrom >= segmentGroupBytes {
			s.groups = append(s.groups, index+1)
			groupFrom = at + 1
		}
	}
	for group := range s.groups {
		first, last := s.groupSegments(group)
		nodes := s.nodesOf(first, last)
		s.groupLabels = append(s.groupLabels, referencedLabels(nodes))
		s.groupSignatures = append(s.groupSignatures, inlineSignature(nodes))
	}
	return s
}

// readWhole reports whether the run's one group is the whole run, so that reading it back is
// reading the run back; a broken segmentation reads nothing for the run.
func (s *runSegments) readWhole() bool {
	return s != nil && !s.broken && len(s.groups) == 1
}

// groupSegments is the first segment of group and the one after its last.
func (s *runSegments) groupSegments(group int) (first, last int) {
	if group+1 < len(s.groups) {
		return s.groups[group], s.groups[group+1]
	}
	return s.groups[group], len(s.cuts) + 1
}

// nodesOf is the nodes segments first to last-1 hold, with the cuts between them: a text a cut
// bounds them inside is a copy holding its part, and one a cut begins or ends them at the edge of
// is left out.
func (s *runSegments) nodesOf(first, last int) []*Node {
	startNode, startOffset := 0, 0
	if first > 0 {
		startNode, startOffset = s.cuts[first-1].node, s.cuts[first-1].offset+1
	}
	endNode, endOffset := len(s.nodes)-1, -1
	if last <= len(s.cuts) {
		endNode, endOffset = s.cuts[last-1].node, s.cuts[last-1].offset
	}
	nodes := make([]*Node, 0, endNode-startNode+1)
	for index := startNode; index <= endNode; index++ {
		node := s.nodes[index]
		from, to := 0, len(node.Text)
		if index == startNode {
			from = startOffset
		}
		if index == endNode && endOffset >= 0 {
			to = endOffset
		}
		if from == 0 && to == len(node.Text) {
			nodes = append(nodes, node)
			continue
		}
		if from == to {
			continue
		}
		part := *node
		part.Text = node.Text[from:to]
		nodes = append(nodes, &part)
	}
	return nodes
}

// span is where segments first to last-1 begin and end in written, a writing of the run that is
// length bytes long, by where it wrote the cuts.
func (s *runSegments) span(first, last, length int) (start, end int) {
	end = length
	if first > 0 {
		start = s.at[first-1] - s.from + 1
	}
	if last <= len(s.cuts) {
		end = s.at[last-1] - s.from
	}
	return start, end
}

// reset readies s, where the run's segments are recorded, for a writing that begins at from.
func (s *runSegments) reset(from int) {
	if s == nil {
		return
	}
	s.from = from
	s.at = s.at[:0]
	clear(s.fused)
}

// cutsIn is the cuts in nodes[node], the next of which is cut index, or none where the run's
// segments are not recorded.
func (s *runSegments) cutsIn(node, index int) []runCut {
	if s == nil {
		return nil
	}
	end := index
	for end < len(s.cuts) && s.cuts[end].node == node {
		end++
	}
	return s.cuts[index:end]
}

// spellingOf is the spelling segment is written in: its own where the run is written by segments,
// and spelling, the run's one, where it is not.
func (s *runSegments) spellingOf(segment int, spelling runSpelling) runSpelling {
	if s == nil || s.spellings == nil {
		return spelling
	}
	return s.spellings[segment]
}

// fuse records that the writing fused two delimiter runs in segment.
func (s *runSegments) fuse(segment int) {
	if s != nil {
		s.fused[segment] = true
	}
}

// cutAt records that the writing wrote a cut's space at at, and spells context, the text it stands
// in, in the spelling of segment, the segment after it, where the run is written by segments.
func (s *runSegments) cutAt(at, segment int, context *escapeContext) {
	s.at = append(s.at, at)
	if s.spellings != nil {
		context.spell(s.spellings[segment], false, false)
	}
}

// shift moves each cut written after at by by bytes, where the writer wrote an escape before them.
func (s *runSegments) shift(at, by int) {
	if s == nil {
		return
	}
	for index := len(s.at) - 1; index >= 0 && s.at[index] > at; index-- {
		s.at[index] += by
	}
}

// digest names written, the markdown of group or segment index, with where each of cuts, the cuts
// inside it, stands in it.
func digest(index int, cuts []int, written []byte) segmentKey {
	key := segmentKey{index: index}
	var number [8]byte
	for which, seed := range segmentSeeds {
		var sum maphash.Hash
		sum.SetSeed(seed)
		for _, cut := range cuts {
			binary.LittleEndian.PutUint64(number[:], uint64(cut))
			sum.Write(number[:])
		}
		sum.Write(written)
		key.sum[which] = sum.Sum64()
	}
	return key
}

// groupKey names group's markdown in written, a writing of the run, by the group, where the writing
// put its inner cuts and its bytes, and answers where the group begins and ends in written and
// where its inner cuts stand in it.
func (s *runSegments) groupKey(group int, written []byte) (key segmentKey, start, end int, cuts []int) {
	first, last := s.groupSegments(group)
	start, end = s.span(first, last, len(written))
	cuts = make([]int, 0, last-first-1)
	for cut := first; cut < last-1; cut++ {
		cuts = append(cuts, s.at[cut]-s.from-start)
	}
	return digest(group, cuts, written[start:end]), start, end, cuts
}

// read reads written, the writing of the run in spelling just made, back a group at a time,
// records what each group read back as in a row, and answers how many readers, from the first,
// every group reads back under: the run reads back whole under no more of them (runSegments). A
// group whose bytes an earlier writing wrote is not read again, and neither is a run's one group
// whose bytes whole, how far each writing read whole reads back by the digest of its bytes, holds.
// A writing that did not put each cut's space where the cuts say answers every reader, so that the
// run is read whole, and leaves the run never written by segments.
func (s *runSegments) read(written []byte, spelling runSpelling, whole map[[sha256.Size]byte]int) int {
	if s.broken || len(s.at) != len(s.cuts) {
		s.broken = true
		return readBackReaderCount
	}
	for _, at := range s.at {
		if written[at-s.from] != ' ' {
			s.broken = true
			return readBackReaderCount
		}
	}
	row := segmentRow{spelling: spelling, readings: make([]int, len(s.groups))}
	if spelling.order != marksAsWritten && !spelling.flankingFused {
		for segment, fused := range s.fused {
			if fused {
				row.fused = append(row.fused, segment)
			}
		}
	}
	least := readBackReaderCount
	for group := range s.groups {
		key, start, end, cuts := s.groupKey(group, written)
		index, seen := s.readingOf[key]
		if !seen {
			depth, read := 0, false
			if s.readWhole() {
				depth, read = whole[sha256.Sum256(written)]
			}
			if !read {
				depth = readsBackUnder(written[start:end], s.prefix, s.groupLabels[group], s.groupSignatures[group])
			}
			reading := segmentReading{depth: depth}
			if depth < readBackReaderCount {
				reading.written = append([]byte(nil), written[start:end]...)
				reading.cuts = cuts
			}
			index = len(s.readings)
			s.readings = append(s.readings, reading)
			s.readingOf[key] = index
		}
		row.readings[group] = index
		least = min(least, s.readings[index].depth)
	}
	s.rows = append(s.rows, row)
	return least
}

// spellBySegment writes the run s cuts, which begins at from and which no writing of the search
// reads back whole under inlineMarkdown (inlineWithEscapes), in a spelling for each segment, and
// reports whether that reads back whole under inlineMarkdown as the run's nodes, whose footnote
// labels are labels and whose inlineSignature is signature; where it does not, the caller writes
// the run's first writing again. whole is how far each writing the search read whole reads back,
// by the digest of its bytes, so that no markdown is read twice.
//
// Each segment is written in the spelling the search would keep for it alone: the earliest writing
// of the greatest depth, the first writing counting first, among the writings the search made,
// those that escape fewer delimiters than crossingEscapes asks left out. A group whose earliest
// deepest writing reads back under every reader is written in it whole, since each of its segments
// then reads back under every reader alone; each segment of any other group is chosen alone, read
// back from the bytes each writing wrote it in.
func (r *renderer) spellBySegment(s *runSegments, from int, context inlineContext, labels, signature []string, whole map[[sha256.Size]byte]int) bool {
	floor := crossingEscapes(s.nodes)
	rows := s.rows
	if floor == delimitersAsRuled && !s.broken {
		r.b.Truncate(from)
		r.segments = s
		r.writeInlineRun(s.nodes, s.prefix, context, runSpelling{})
		r.segments = nil
		if r.err != nil {
			return false
		}
		// The first writing counts first, as it does in the search.
		if s.read(r.b.Bytes()[from:], runSpelling{}, whole); !s.broken {
			rows = append([]segmentRow{s.rows[len(s.rows)-1]}, s.rows[:len(s.rows)-1]...)
		}
	}
	if s.broken {
		return false
	}
	spellings := make([]runSpelling, len(s.cuts)+1)
	for group := range s.groups {
		first, last := s.groupSegments(group)
		best, depth := earliestDeepest(rows, floor, first, last, func(row segmentRow) int {
			return s.readings[row.readings[group]].depth
		})
		if best < 0 {
			return false
		}
		if depth == readBackReaderCount {
			for segment := first; segment < last; segment++ {
				spellings[segment] = rows[best].spelling
			}
			continue
		}
		for segment := first; segment < last; segment++ {
			best, _ := earliestDeepest(rows, floor, segment, segment+1, func(row segmentRow) int {
				return s.segmentDepth(s.readings[row.readings[group]], first, last, segment)
			})
			if best < 0 {
				return false
			}
			spellings[segment] = rows[best].spelling
		}
	}
	r.b.Truncate(from)
	r.segments = s
	s.spellings = spellings
	r.writeInlineRun(s.nodes, s.prefix, context, spellings[0])
	s.spellings = nil
	r.segments = nil
	if r.err != nil {
		return false
	}
	// A writing read before, whole or as the run's one group, is not read again.
	written := r.b.Bytes()[from:]
	depth, read := whole[sha256.Sum256(written)]
	if !read && s.readWhole() && len(s.at) == len(s.cuts) {
		key, _, _, _ := s.groupKey(0, written)
		var index int
		if index, read = s.readingOf[key]; read {
			depth = s.readings[index].depth
		}
	}
	if !read {
		depth = readsBackUnder(written, s.prefix, labels, signature)
	}
	return depth > 0
}

// earliestDeepest is the index of the earliest of rows of the greatest depth, and that depth, among
// those that escape at least floor and fused no delimiter runs in segments first to last-1, or -1
// where none does.
func earliestDeepest(rows []segmentRow, floor delimiterEscapes, first, last int, depth func(segmentRow) int) (best, deepest int) {
	best, deepest = -1, -1
	for index, row := range rows {
		if row.spelling.escapes < floor || fusedIn(row.fused, first, last) {
			continue
		}
		if reads := depth(row); reads > deepest {
			best, deepest = index, reads
		}
	}
	return best, deepest
}

// fusedIn reports whether one of fused is a segment from first to last-1.
func fusedIn(fused []int, first, last int) bool {
	for _, segment := range fused {
		if segment >= first && segment < last {
			return true
		}
	}
	return false
}

// segmentDepth is how many readers, from the first, segment, of the group of segments first to
// last-1 a writing wrote as reading, reads back under alone. Each segment of a group that reads back
// under every reader reads back under every reader alone (runSegments), and a group of one segment
// is that segment; any other is read back from the bytes the writing wrote it in, once for each
// segment and bytes.
func (s *runSegments) segmentDepth(reading segmentReading, first, last, segment int) int {
	if reading.depth == readBackReaderCount || last-first == 1 {
		return reading.depth
	}
	start, end := 0, len(reading.written)
	if segment > first {
		start = reading.cuts[segment-first-1] + 1
	}
	if segment < last-1 {
		end = reading.cuts[segment-first]
	}
	written := reading.written[start:end]
	key := digest(segment, nil, written)
	if depth, read := s.segmentReads[key]; read {
		return depth
	}
	nodes, made := s.segmentNodes[segment]
	if !made {
		nodes = s.nodesOf(segment, segment+1)
		s.segmentNodes[segment] = nodes
	}
	depth := readsBackUnder(written, s.prefix, referencedLabels(nodes), inlineSignature(nodes))
	s.segmentReads[key] = depth
	return depth
}

// crossingEscapes is the fewest delimiter escapes a run written by segments writes each segment
// in (spellBySegment), so that no delimiter run its text leaves unescaped in one segment pairs
// with one left unescaped in another: every delimiter run a segment that reads back alone pairs is
// inside it, so only two runs of text could pair across a cut. delimitersAll where the run's text
// holds two free runs of asterisks or of underscores (freeDelimiterRuns, the runs the escape rules
// leave as they are and that could open or close), delimitersTildes where it holds two runs of
// tildes, which the rules never escape, and delimitersAsRuled where it holds neither, so each
// delimiter character is left unescaped in one place at most.
func crossingEscapes(nodes []*Node) delimiterEscapes {
	stars, underscores, tildes := 0, 0, 0
	for _, node := range nodes {
		if node.Type != "text" || nodeHasMark(node, "inlineCode") {
			continue
		}
		stars += freeDelimiterRuns(node.Text, '*')
		underscores += freeDelimiterRuns(node.Text, '_')
		for offset := range len(node.Text) {
			if node.Text[offset] == '~' && (offset == 0 || node.Text[offset-1] != '~') {
				tildes++
			}
		}
	}
	switch {
	case stars >= 2 || underscores >= 2:
		return delimitersAll
	case tildes >= 2:
		return delimitersTildes
	}
	return delimitersAsRuled
}
