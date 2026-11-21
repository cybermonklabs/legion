package specform

import (
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// wholeText is a text as the judge of the ways out reads it for the rules that read more than one
// of its sentences, so that a way out passes only where the whole text that it writes gives no
// finding of those rules that the text does not give, word for word (passes). The rules that read
// more than one sentence are:
//   - ste100-synonym-rotation, in three places: each member of a synonym group after the first one
//     to stand in the text, at its first place (ste.LintVerbs, read from the Hits of each
//     sentence); the verbs of a text with those of its footnotes (rotationBreaks in formBreaks,
//     with held); and the verbs of a footnote beside those of the footnotes that it refers to and
//     of each text that holds it (footnoteJudge.rotations and held, with elsewhere);
//   - ste100-long-sentence, whose piece runs on past a stop that a closing quote or bracket
//     follows, where a sentence of the form ends (ste.Spread.Open), so that one piece can take the
//     words of several sentences;
//   - ste100-dangling-conjunction, which reads a list item from its mark to its last word, and
//     quotes it whole;
//   - sentences, the count of a statement, which the judge reads in the sentence of the way out
//     with the other sentences of the text (wayJudge.told) and of the statement that holds a
//     footnote (wayJudge.heldIn), so it is no part of wholeText.
//
// Each other rule reads one sentence or one word: stop-no-space, sentence-words and instructions
// read a sentence (sentencesOf); the reference rules read a word, or a name whose parts only white
// space parts (personNames), and no form of theirs holds the stop that ends a sentence; and the
// rules semicolon, phrasal-verb, marketing-adjective and nominalization of ste-lint match words
// with no stop between them. The rules term-line and unreadable read a line under Terms and a
// version, which no way out of a text writes.
//
// A way out writes the sentences that hold its finding (wayJudge.places), so the judge reads only
// those again, beside what it read once of each other sentence: the work of a way out grows with
// the sentences of its finding and the pieces of ste-lint that run through them, not with the
// text. A text that ste-lint reads as more than one line, as it ends a line at a line break such as
// \v or U+2028 that the one line of a text keeps (ste.Lines), the judge reads whole for each way
// out instead (lines), since ste-lint ends a list item and a piece at such a break. Each such
// reading reads the whole text, so the judge reads such a text for the ways out of its
// first maxJudgedInSentence findings only, as it reads a sentence for those of the first
// maxJudgedInSentence findings of the sentence, and the work of a judgment stays in proportion to
// its text (the work table's row of many lines of one text). A finding after them is still refused
// and named, and its detail says that Dispatch offers no way out of it (wholeNote).
type wholeText struct {
	line string
	// spans is the bytes of each sentence of the line, from the end of the one before it, so that
	// they write the line whole (sentencesOf); at is the sentences of each sentence's text
	// (sentence.in), as the judge names a place (unitKey).
	spans []string
	at    map[string][]int
	// held is the verbs of each footnote that the text holds (footnotesHeld.verbs). elsewhere, for
	// the text of a footnote, reports whether a way out that changes the verbs of its words from
	// one set to another gives a finding of the rule synonym-rotation of the footnotes and the
	// texts that hold the footnote that the judgment does not give (footnoteJudge.rotationCanChange).
	// lines is whether the judge reads the text whole for each way out, and readFor each finding
	// whose ways out it read so.
	held      []ste.Verbs
	elsewhere func(before, after ste.Verbs) bool
	lines     bool
	readFor   map[wayKey]bool
	// The fields below are read once (load): the Spread of each sentence; the sentences that each
	// member of a synonym group stands in, in order, with its first place in each; the words of the
	// piece of ste-lint that runs on into each sentence from the ones before it (left), and that
	// runs from the start of each sentence to the end of its piece (runs, one more than the
	// sentences); the findings of the rules that read more than one sentence, the text's verbs, and
	// whether it is a list item that ends in "and" or "or" (dangles).
	read    bool
	spreads []ste.Spread
	stands  map[[2]int][]sentenceHit
	left    []int
	runs    []int
	before  map[findingKey]int
	verbs   ste.Verbs
	dangles bool
	passed  map[wholeWay]bool
}

// sentenceHit is the first place of a member of a synonym group in one sentence of a text.
type sentenceHit struct {
	sentence int
	hit      ste.Hit
}

// writtenSentence is a sentence of a text as a way out writes it: its bytes, and their Spread.
type writtenSentence struct {
	text   string
	spread ste.Spread
}

// wholeWay is a way out of a finding as the whole text reads it once: the finding by its rule and
// its words, and the way out.
type wholeWay struct {
	finding wayKey
	way     wayOut
}

// newWholeText is line as the judge reads it whole, with held, the verbs of its footnotes, and
// elsewhere for the text of a footnote (nil for a text).
func newWholeText(line string, held []ste.Verbs, elsewhere func(before, after ste.Verbs) bool) *wholeText {
	w := &wholeText{line: line, held: held, elsewhere: elsewhere, at: map[string][]int{}, lines: ste.Lines(line) > 1}
	for index, each := range sentencesOf(line) {
		w.spans = append(w.spans, line[each.start:each.end])
		w.at[each.in(line)] = append(w.at[each.in(line)], index)
	}
	return w
}

// load reads each sentence of the text once, the first time that a way out asks: its Spread, the
// first place of each member of a synonym group, the words of the pieces that run across its two
// ends, and the findings of the whole text.
func (w *wholeText) load() {
	if w.read {
		return
	}
	w.read = true
	if w.lines {
		w.before, w.verbs = lintedWhole(w.line, w.held)
		return
	}
	count := len(w.spans)
	w.spreads = make([]ste.Spread, count)
	w.stands = map[[2]int][]sentenceHit{}
	for index, span := range w.spans {
		w.spreads[index] = ste.SpreadOf(span)
		for _, hit := range w.spreads[index].Hits {
			member := [2]int{hit.Group, hit.Member}
			w.stands[member] = append(w.stands[member], sentenceHit{index, hit})
		}
	}
	w.left = make([]int, count)
	for index := 1; index < count; index++ {
		before := w.spreads[index-1]
		switch last := len(before.Pieces) - 1; {
		case !before.Open:
		case last == 0:
			w.left[index] = w.left[index-1] + before.Pieces[0]
		default:
			w.left[index] = before.Pieces[last]
		}
	}
	w.runs = make([]int, count+1)
	for index := count - 1; index >= 0; index-- {
		spread := w.spreads[index]
		w.runs[index] = spread.Pieces[0]
		if len(spread.Pieces) == 1 && spread.Open {
			w.runs[index] += w.runs[index+1]
		}
	}
	w.before, w.verbs = w.rotations(nil)
	if count > 0 {
		w.pieces(0, count-1, nil, func(words int) { addLong(w.before, words, 1) })
	}
	w.dangles = w.dangling(nil)
}

// passes reports whether way, a way out of finding written at each place of its words in the
// sentences of the text that places names, writes a text that gives no finding of the rules that
// read more than one sentence that the text does not give as many times (wholeText), and whether
// the judge read it (read): a text that the judge reads whole it reads for the ways out of the
// first maxJudgedInSentence findings that ask, and no more. A place that is no sentence of the
// text, such as an address or a title, changes none of its words.
func (w *wholeText) passes(finding wayKey, way wayOut, places []unitKey) (passed, read bool) {
	key := wholeWay{finding, way}
	if passed, ok := w.passed[key]; ok {
		return passed, true
	}
	changed := map[int]writtenSentence{}
	for _, place := range places {
		if place.address {
			continue
		}
		for _, index := range w.at[place.text] {
			if _, ok := changed[index]; !ok {
				text := strings.ReplaceAll(w.spans[index], way.from, way.to)
				changed[index] = writtenSentence{text, ste.SpreadOf(text)}
			}
		}
	}
	passed = true
	if len(changed) > 0 {
		if w.lines && !w.readFor[finding] {
			if len(w.readFor) >= maxJudgedInSentence {
				return false, false
			}
			if w.readFor == nil {
				w.readFor = map[wayKey]bool{}
			}
			w.readFor[finding] = true
		}
		w.load()
		passed = w.writesNoNewFinding(changed)
	}
	if w.passed == nil {
		w.passed = map[wholeWay]bool{}
	}
	w.passed[key] = passed
	return passed, true
}

// writesNoNewFinding reports whether the text with the sentences of changed in place of its own
// gives no finding of the rules that read more than one sentence that the text does not give as
// many times.
func (w *wholeText) writesNoNewFinding(changed map[int]writtenSentence) bool {
	var after map[findingKey]int
	var verbs ste.Verbs
	if w.lines {
		var written strings.Builder
		for index, span := range w.spans {
			if each, ok := changed[index]; ok {
				span = each.text
			}
			written.WriteString(span)
		}
		after, verbs = lintedWhole(written.String(), w.held)
	} else {
		after, verbs = w.rotations(changed)
	}
	if w.elsewhere != nil && verbs != w.verbs && w.elsewhere(w.verbs, verbs) {
		return false
	}
	if w.lines {
		return noneNew(after, w.before)
	}
	for key, count := range w.before {
		if key.rule == "ste100-long-sentence" {
			after[key] += count
		}
	}
	for _, run := range w.changedRuns(changed) {
		w.pieces(run[0], run[1], nil, func(words int) { addLong(after, words, -1) })
		w.pieces(run[0], run[1], changed, func(words int) { addLong(after, words, 1) })
	}
	if !noneNew(after, w.before) {
		return false
	}
	if !w.dangling(changed) {
		return true
	}
	// The finding of dangling-conjunction quotes the item whole, so it stays the text's own only
	// where each changed sentence reads to the rule as it did.
	if !w.dangles {
		return false
	}
	for index, written := range changed {
		if ste.Neutral(written.text) != ste.Neutral(w.spans[index]) {
			return false
		}
	}
	return true
}

// lintedWhole is the findings of the rules that read more than one sentence (wholeText) in line,
// read whole by ste-lint, with the verbs of its footnotes, held, and its verbs: the reading of a
// text that ste-lint reads as more than one line, and the one that the sentences of any other text
// add up to.
func lintedWhole(line string, held []ste.Verbs) (map[findingKey]int, ste.Verbs) {
	lint, verbs := ste.LintVerbs(line)
	found := map[findingKey]int{}
	for _, finding := range lint {
		switch finding.Rule {
		case "synonym-rotation", "long-sentence", "dangling-conjunction":
			found[findingKeyOf(steBreak(finding))]++
		}
	}
	for _, each := range rotationBreaks(ste.Rotations(verbs, held), "its footnotes") {
		found[findingKeyOf(each)]++
	}
	return found, verbs
}

// noneNew reports whether after holds no finding more times than before.
func noneNew(after, before map[findingKey]int) bool {
	for key, count := range after {
		if count > before[key] {
			return false
		}
	}
	return true
}

// rotations is the findings of the rule synonym-rotation of the text with the sentences of changed
// in place of its own, and its verbs: of its own words, each member of a group after the first one
// to stand in it, at its first place, in the order of ste.LintVerbs; and of its words with those of
// its footnotes (ste.Rotations, held).
func (w *wholeText) rotations(changed map[int]writtenSentence) (map[findingKey]int, ste.Verbs) {
	firsts := map[[2]int]sentenceHit{}
	for member, stands := range w.stands {
		for _, stand := range stands {
			if _, ok := changed[stand.sentence]; !ok {
				firsts[member] = stand
				break
			}
		}
	}
	for index, written := range changed {
		for _, hit := range written.spread.Hits {
			member := [2]int{hit.Group, hit.Member}
			if first, ok := firsts[member]; !ok || index < first.sentence {
				firsts[member] = sentenceHit{index, hit}
			}
		}
	}
	groups := map[int][]sentenceHit{}
	var verbs ste.Verbs
	for _, first := range firsts {
		groups[first.hit.Group] = append(groups[first.hit.Group], first)
		verbs |= first.hit.Verb()
	}
	found := map[findingKey]int{}
	for _, present := range groups {
		sort.Slice(present, func(a, b int) bool {
			x, y := present[a], present[b]
			if x.sentence != y.sentence {
				return x.sentence < y.sentence
			}
			if x.hit.Line != y.hit.Line {
				return x.hit.Line < y.hit.Line
			}
			if x.hit.Column != y.hit.Column {
				return x.hit.Column < y.hit.Column
			}
			return x.hit.Match < y.hit.Match
		})
		for _, each := range present[1:] {
			found[findingKeyOf(steBreak(each.hit.Rotation()))]++
		}
	}
	for _, each := range rotationBreaks(ste.Rotations(verbs, w.held), "its footnotes") {
		found[findingKeyOf(each)]++
	}
	return found, verbs
}

// spread is the Spread of the sentence index of the text, or of the sentence that changed writes in
// its place.
func (w *wholeText) spread(changed map[int]writtenSentence, index int) ste.Spread {
	if written, ok := changed[index]; ok {
		return written.spread
	}
	return w.spreads[index]
}

// changedRuns is the sentences of changed as runs, each from the first to the last of the sentences
// of changed that the pieces of ste-lint join: a piece runs on, in the text or as changed writes
// it, from each sentence of a run to the next. So no piece runs from a run into a sentence of
// changed outside it, before the way out or after it.
func (w *wholeText) changedRuns(changed map[int]writtenSentence) [][2]int {
	indexes := make([]int, 0, len(changed))
	for index := range changed {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	open := func(index int) bool {
		return w.spreads[index].Open || w.spread(changed, index).Open
	}
	var runs [][2]int
	for _, index := range indexes {
		if count := len(runs); count > 0 {
			joined := true
			for at := runs[count-1][1]; at < index && joined; at++ {
				joined = open(at)
			}
			if joined {
				runs[count-1][1] = index
				continue
			}
		}
		runs = append(runs, [2]int{index, index})
	}
	return runs
}

// pieces calls emit with the words of each piece of ste-lint that the sentences from to through
// take part in, each sentence as changed writes it, with the words of the piece that runs on into
// from from the sentences before it (left), and from through into the sentences after it (runs).
func (w *wholeText) pieces(from, through int, changed map[int]writtenSentence, emit func(int)) {
	open := w.left[from]
	for index := from; index <= through; index++ {
		each := w.spread(changed, index)
		last := len(each.Pieces) - 1
		if last == 0 {
			open += each.Pieces[0]
		} else {
			emit(open + each.Pieces[0])
			for _, words := range each.Pieces[1:last] {
				emit(words)
			}
			open = each.Pieces[last]
		}
		if !each.Open {
			emit(open)
			open = 0
		}
	}
	if w.spread(changed, through).Open {
		emit(open + w.runs[through+1])
	}
}

// dangling reports whether the text with the sentences of changed in place of its own is a list
// item that ends in "and" or "or", as the rule dangling-conjunction reads it: its mark stands in
// its first sentence, and its last word in its last.
func (w *wholeText) dangling(changed map[int]writtenSentence) bool {
	count := len(w.spans)
	if count == 0 {
		return false
	}
	written := func(index int) string {
		if each, ok := changed[index]; ok {
			return each.text
		}
		return w.spans[index]
	}
	return ste.ListItem(written(0)) && ste.EndsInConjunction(written(count-1))
}

// addLong adds by to the count in found of the finding of the rule long-sentence of a piece of
// words, where a piece of so many words gives one.
func addLong(found map[findingKey]int, words, by int) {
	if finding, long := ste.LongSentence(words); long {
		found[findingKeyOf(steBreak(finding))] += by
	}
}

// findingKeyOf is a finding as the judge compares two readings: its rule and its detail.
func findingKeyOf(each broken) findingKey {
	return findingKey{each.rule, each.detail}
}
