package specform

import (
	"sort"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/specform/charge"
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
// A way out writes the sentences that hold its finding (wayJudge.places) and the sentences where
// only its words stand (wayJudge.alsoAt), so the judge reads only those again, beside what it read
// once of each other sentence: the work of a way out grows with those sentences and the pieces of
// ste-lint that run through them, not with the text. A text that ste-lint reads as more than one
// line, as it ends a line at a line break such as \v or U+2028 that the one line of a text keeps
// (ste.Lines), the judge reads whole for each way out instead (lines), since ste-lint ends a list
// item and a piece at such a break. Each such reading reads the whole text, so the judge reads such
// a text for the ways out of its first maxJudgedInSentence findings only, as it reads a sentence
// for those of the first maxJudgedInSentence findings of the sentence (the work table's row of many
// lines of one text). A finding after them is still refused and named, and its detail says that
// Dispatch offers no way out of it (wholeNote).
//
// wholeText keeps no byte of a sentence and nothing that it read of one, but the words of each
// finding whose ways out it read and of those ways out (readFor and passed, by wayKey and wayOut),
// which for a finding of the rule instructions are its whole sentence, hashed a fixed number of
// times for that finding; the detail of each finding of the text, read once (before, by
// findingKey), which for a finding of ste100-dangling-conjunction quotes the last line of its list
// item; and the words of the first place of each member of a synonym group (firsts): it names each
// sentence by its unit and its place in the line, and what it keeps of each place (sentenceAt) is
// in the judgment's texts, which give it only through the sentence's charge (charge.Texts.Read). So
// each reading of a sentence, its words, its Spread or a count of its pieces is charged, as every
// reading of the judge is, and the work of a judgment stays in proportion to its text however many
// ways out read one sentence; a way out that would read a sentence past its charge is refused
// unread (elsewhereNote). The first place of each member of a synonym group (firsts) is kept outside
// the charge: one for each member of rules.json at most, so no walk of it grows with the text.
type wholeText struct {
	texts *charge.Texts[sentenceAt]
	// units is the unit of each sentence of the line, in order (sentencesOf), and nth the index of
	// its place among the places of its unit (charge.Texts.Keep); at is the sentences of each
	// unit.
	units []charge.Unit
	nth   []int
	at    map[charge.Unit][]int
	// bare is each word of the line, by its place, under its bare form (bareWord), read the first
	// time that a finding asks where its words stand (holding, indexed).
	bare    charge.Index[wordAt]
	indexed bool
	// held is the verbs of each footnote that the text holds (footnotesHeld.verbs), read once
	// (ste.HeldOf), so that the reading of a way out reads the sets of verbs of each synonym group
	// that the footnotes use, not each footnote. elsewhere, for the text of a footnote, reports
	// whether a way out that changes the verbs of its words from one set to another gives a finding
	// of the rule synonym-rotation of the footnotes and the texts that hold the footnote that the
	// judgment does not give (footnoteJudge.rotationCanChange). lines is whether the judge reads the
	// text whole for each way out, and readFor each finding whose ways out it read so.
	held      ste.Held
	elsewhere func(before, after ste.Verbs) bool
	lines     bool
	readFor   map[wayKey]bool
	// The fields below are read once (load): the first place of each member of a synonym group in
	// the text (firsts), and each sentence that it stands in, in order, by the index of its Hit in
	// the sentence's Spread (stands); the findings of the rules that read more than one sentence,
	// the text's verbs; whether its first sentence starts a list item (listItem), whether its last
	// sentence ends in "and" or "or" (endsInConjunction), and so whether it is a list item that ends
	// so (dangles). refused is whether a charge refused one of the judgment's readings of each
	// sentence for no finding (fixedReads), which the bounds that ownReads is derived from never
	// let happen: then each way out is refused (elsewhereNote).
	read, refused     bool
	firsts            map[[2]int]sentenceHit
	stands            map[[2]int][]standAt
	before            map[findingKey]int
	verbs             ste.Verbs
	listItem          bool
	endsInConjunction bool
	dangles           bool
	passed            map[wholeWay]bool
}

// sentenceAt is what a judgment keeps of one sentence of the line, one place of its unit
// (charge.Texts.Keep), which only a reading of the unit through its charge gives
// (charge.Texts.Read): its place in the line (index); its bytes, from the end of the sentence before
// it, so that the sentences write the line whole (span, sentencesOf); and whether a backtick that
// nothing closes stands before it, an odd count of backticks in the line before it (opened):
// ste-lint cuts code font from a backtick to the next one, so a backtick that a way out writes in
// that sentence would close it, and code font would take the words of each sentence between. The
// judgment's one reading of the sentence for its Spread (load) adds its Spread; the words of the
// piece of ste-lint that runs on into it from the sentences before it (left); and the words of the
// piece that runs from the start of the next sentence to the end of its piece, none after the last
// sentence (after).
type sentenceAt struct {
	index       int
	span        string
	opened      bool
	spread      ste.Spread
	left, after int
}

// wordAt is a word of a line, as strings.Fields cuts it, by the sentence that it stands in and
// where it stands in the sentence's span (sentenceAt.span[start:end]).
type wordAt struct {
	sentence, start, end int
}

// sentenceHit is the first place of a member of a synonym group in one sentence of a text.
type sentenceHit struct {
	sentence int
	hit      ste.Hit
}

// standAt is a sentence of a text where a member of a synonym group stands, and the index of its
// first place there in the Hits of the sentence's Spread.
type standAt struct {
	sentence, hit int
}

// writtenSentence is a sentence of a text as a way out writes it: its bytes and their Spread, and
// what the judgment keeps of the sentence (was), which the way out's charged reading gave.
type writtenSentence struct {
	text   string
	spread ste.Spread
	was    *sentenceAt
}

// wholeWay is a way out of a finding as the whole text reads it once: the finding by its rule and
// its words, and the way out.
type wholeWay struct {
	finding wayKey
	way     wayOut
}

// newWholeText is line as the judge reads it whole, with held, the verbs of its footnotes, and
// elsewhere for the text of a footnote (nil for a text): each sentence of line named in texts, the
// judgment's texts, with what the judgment keeps of its place (charge.Texts.Name, Keep). Its split
// of the line reads the line, which the rules hand it, and no sentence through the charge.
func newWholeText(line string, held []ste.Verbs, elsewhere func(before, after ste.Verbs) bool, texts *charge.Texts[sentenceAt]) *wholeText {
	w := &wholeText{texts: texts, held: ste.HeldOf(held), elsewhere: elsewhere, at: map[charge.Unit][]int{}, lines: ste.Lines(line) > 1}
	ticks := 0
	for index, each := range sentencesOf(line) {
		span, unit := line[each.start:each.end], texts.Name(each.in(line), false, false)
		w.units = append(w.units, unit)
		w.nth = append(w.nth, texts.Keep(unit, sentenceAt{index: index, span: span, opened: ticks%2 == 1}))
		w.at[unit] = append(w.at[unit], index)
		ticks += strings.Count(span, "`")
	}
	return w
}

// readAll is the judgment's one reading of each sentence of the line for no finding (fixedReads),
// each unit once through its charge, and is what the judgment keeps of each sentence, in the order
// of the line; false where a charge refuses the reading, which the bounds that ownReads is derived
// from never let happen.
func (w *wholeText) readAll() ([]*sentenceAt, charge.Answer) {
	records := make([]*sentenceAt, len(w.units))
	for index, unit := range w.units {
		if records[index] != nil {
			continue
		}
		_, kept, charged := w.texts.Read(unit, true)
		if !charged {
			return nil, false
		}
		for place := range kept {
			records[kept[place].index] = &kept[place]
		}
	}
	return records, true
}

// other is what the judgment keeps of the sentence index, for a way out of a finding that does not
// stand in it: read, the readings of one judgment of a way out, gives each unit's places once, so
// each sentence is read once for each such judgment through its charge; false where the charge
// refuses the reading.
func (w *wholeText) other(index int, read map[charge.Unit][]sentenceAt) (*sentenceAt, charge.Answer) {
	unit := w.units[index]
	kept, ok := read[unit]
	if !ok {
		var charged charge.Answer
		if _, kept, charged = w.texts.Read(unit, false); !charged {
			return nil, false
		}
		read[unit] = kept
	}
	return &kept[w.nth[index]], true
}

// bareWord is word without the characters at its two ends that are no letter or digit: its marks,
// stops, colons and code font.
func bareWord(word string) string {
	return strings.TrimFunc(word, func(letter rune) bool { return !isLetterOrDigit(letter) })
}

// fieldsOf calls each with where each word of text, as strings.Fields cuts it, starts and ends.
func fieldsOf(text string, each func(start, end int)) {
	start := -1
	for index, letter := range text {
		switch space := unicode.IsSpace(letter); {
		case space && start >= 0:
			each(start, index)
			start = -1
		case !space && start < 0:
			start = index
		}
	}
	if start >= 0 {
		each(start, len(text))
	}
}

// holding is each sentence of the line, by its unit, where a writer who changes words at each place
// where they stand (strings.ReplaceAll) changes them and no place of places stands, each once; and
// whether the charge of each sentence that it read let it read it (true), or refused a reading
// (false). words is a word with no white space, the words of a stop with no space after it or of
// an email address, which stands in a word of the line whose bare form (bareWord) is its own:
// "Web.Config" stands in "Web.Config.", "`Web.Config`" and "(Web.Config)", and "ada@example.com"
// in "ada@example.com:.". The words of the line are read once, by their places under their bare
// forms (bare), so each finding's look reads the words that have its bare form, not the line, and
// tests each such word, not its sentence. A test reads a word of a sentence where the finding does
// not stand, so it reads the sentence through its charge, once for each sentence of the look
// (other). Words with white space in them, the sentence of a finding of instructions or a person's
// name, stand in no word, and so in no sentence that holding names; a sentence that holds the same
// text is one of places already (at).
func (w *wholeText) holding(words string, places []charge.Unit) ([]charge.Unit, charge.Answer) {
	if words == "" || strings.ContainsFunc(words, unicode.IsSpace) {
		return nil, true
	}
	if !w.indexed {
		w.indexed = true
		records, charged := w.readAll()
		w.refused = w.refused || !bool(charged)
		for _, at := range records {
			fieldsOf(at.span, func(start, end int) {
				w.bare.Put(bareWord(at.span[start:end]), wordAt{at.index, start, end})
			})
		}
	}
	if w.refused {
		return nil, false
	}
	read := map[int]bool{}
	for _, place := range places {
		if !place.Address() && !place.Title() {
			for _, index := range w.at[place] {
				read[index] = true
			}
		}
	}
	look := map[charge.Unit][]sentenceAt{}
	var units []charge.Unit
	for _, at := range w.bare.Get(bareWord(words)) {
		if read[at.sentence] {
			continue
		}
		sentence, charged := w.other(at.sentence, look)
		if !charged {
			return nil, false
		}
		if !strings.Contains(sentence.span[at.start:at.end], words) {
			continue
		}
		unit := w.units[at.sentence]
		for _, index := range w.at[unit] {
			read[index] = true
		}
		units = append(units, unit)
	}
	return units, true
}

// load reads each sentence of the text once, the first time that a way out asks: its Spread, the
// first place of each member of a synonym group, the words of the pieces that run across its two
// ends, and the findings of the whole text; false where the judgment refused that reading
// (refused).
func (w *wholeText) load() charge.Answer {
	if w.read {
		return charge.Answer(!w.refused)
	}
	w.read = true
	records, charged := w.readAll()
	if w.refused = w.refused || !bool(charged); w.refused {
		return false
	}
	if w.lines {
		var line strings.Builder
		for _, at := range records {
			line.WriteString(at.span)
		}
		w.before, w.verbs = lintedWhole(line.String(), w.held)
		return true
	}
	count := len(records)
	w.firsts, w.stands = map[[2]int]sentenceHit{}, map[[2]int][]standAt{}
	for index, at := range records {
		at.spread = ste.SpreadOf(at.span)
		for place, hit := range at.spread.Hits {
			member := [2]int{hit.Group, hit.Member}
			if _, ok := w.firsts[member]; !ok {
				w.firsts[member] = sentenceHit{index, hit}
			}
			w.stands[member] = append(w.stands[member], standAt{index, place})
		}
	}
	for index := 1; index < count; index++ {
		before := records[index-1].spread
		switch last := len(before.Pieces) - 1; {
		case !before.Open:
		case last == 0:
			records[index].left = records[index-1].left + before.Pieces[0]
		default:
			records[index].left = before.Pieces[last]
		}
	}
	runs := 0
	for index := count - 1; index >= 0; index-- {
		at := records[index]
		at.after = runs
		if runs = at.spread.Pieces[0]; len(at.spread.Pieces) == 1 && at.spread.Open {
			runs += at.after
		}
	}
	// The text as it stands changes no sentence, so its findings of synonym-rotation are those of its
	// own first places, which this read, and no sentence is read again through a charge.
	w.before, w.verbs = w.rotationsOf(w.firsts)
	if count > 0 {
		spread := func(index int) ste.Spread { return records[index].spread }
		pieces(0, count-1, spread, records[0].left, records[count-1].after, false, func(words int) { addLong(w.before, words, 1) })
		w.listItem, w.endsInConjunction = ste.ListItem(records[0].span), ste.EndsInConjunction(records[count-1].span)
	}
	w.dangles = w.listItem && w.endsInConjunction
	return true
}

// passes reports whether way, a way out of finding written at each place of its words in the
// sentences of the text that places and also name, writes a text that gives no finding of the rules
// that read more than one sentence that the text does not give as many times (wholeText); and what
// the finding says where the judge did not read it (note): wholeNote where the judge reads the text
// whole, which it does for the ways out of the first maxJudgedInSentence findings that ask and no
// more, and where a reading of a sentence was refused (charge.Texts.Read), elsewhereNote for its
// charge and documentNote for the budget of its document (refusedNote).
// The sentences of places hold the finding, and those of also only its words (wayJudge.alsoAt). A
// place that is no sentence of the text, an address or a sentence of a title, changes none of its
// words. A place of the text that is no sentence of the line, which this reading cannot write,
// refuses the way out, and so does a way out that writes a backtick in a sentence after a backtick
// that nothing closes (opened), which would take the sentences between into code font: neither
// passes unread. No way out that a rule writes holds a line break that ste-lint reads (ste.Lines):
// its words are a word of ste-lint, an address or a sentence without the white space at its ends,
// in a text that ste-lint reads as one line or that this reading reads whole (lines).
func (w *wholeText) passes(finding wayKey, way wayOut, places, also []charge.Unit) (passed bool, note string) {
	key := wholeWay{finding, way}
	if passed, ok := w.passed[key]; ok {
		return passed, ""
	}
	ticks := strings.Count(way.to, "`") != strings.Count(way.from, "`")
	changed := map[int]writtenSentence{}
	passed = true
	// write writes the way out at each place of units, the finding's own (own) or not, where the
	// charge of its sentence lets it, until one refuses the way out.
	write := func(units []charge.Unit, own bool) charge.Answer {
		for _, place := range units {
			if !passed {
				return true
			}
			if place.Address() || place.Title() {
				continue
			}
			if _, ok := w.at[place]; !ok {
				passed = false
				return true
			}
			_, kept, charged := w.texts.Read(place, own)
			if !charged {
				return false
			}
			for index := range kept {
				at := &kept[index]
				passed = passed && !(ticks && at.opened)
				text := strings.ReplaceAll(at.span, way.from, way.to)
				changed[at.index] = writtenSentence{text, ste.SpreadOf(text), at}
			}
		}
		return true
	}
	if !write(places, true) || !write(also, false) {
		return false, refusedNote(w.texts)
	}
	if passed && len(changed) > 0 {
		if w.lines && !w.readFor[finding] {
			if len(w.readFor) >= maxJudgedInSentence {
				return false, wholeNote
			}
			if w.readFor == nil {
				w.readFor = map[wayKey]bool{}
			}
			w.readFor[finding] = true
		}
		if !w.load() {
			return false, refusedNote(w.texts)
		}
		var charged charge.Answer
		if passed, charged = w.writesNoNewFinding(changed); !charged {
			return false, refusedNote(w.texts)
		}
	}
	if w.passed == nil {
		w.passed = map[wholeWay]bool{}
	}
	w.passed[key] = passed
	return passed, ""
}

// writesNoNewFinding reports whether the text with the sentences of changed in place of its own
// gives no finding of the rules that read more than one sentence that the text does not give as
// many times (passed), and whether the charge of each sentence that it reads beside those of
// changed let it read it (charged): each other sentence of a text that it reads whole, each
// sentence between two of changed that a piece of ste-lint runs through (changedRuns), and the
// sentence where a member of a synonym group first stands after the sentences of changed
// (rotations). Those are sentences where the finding does not stand, as changed holds each place of
// the finding, and each is read once for this way out (read).
func (w *wholeText) writesNoNewFinding(changed map[int]writtenSentence) (passed bool, charged charge.Answer) {
	read := map[charge.Unit][]sentenceAt{}
	var after map[findingKey]int
	var verbs ste.Verbs
	if w.lines {
		var written strings.Builder
		for index := range w.units {
			if each, ok := changed[index]; ok {
				written.WriteString(each.text)
				continue
			}
			at, charged := w.other(index, read)
			if !charged {
				return false, false
			}
			written.WriteString(at.span)
		}
		after, verbs = lintedWhole(written.String(), w.held)
	} else if after, verbs, charged = w.rotations(changed, read); !charged {
		return false, false
	}
	if w.elsewhere != nil && verbs != w.verbs && w.elsewhere(w.verbs, verbs) {
		return false, true
	}
	if w.lines {
		return noneNew(after, w.before), true
	}
	// The way out changes the count of the findings of long-sentence only in the pieces that its
	// runs take part in: long is each change of a count, the pieces of the text taken away and the
	// pieces of the way out added, so the reading reads those pieces, not each finding of the text.
	long := map[findingKey]int{}
	runs, charged := w.changedRuns(changed, read)
	if !charged {
		return false, false
	}
	// spreadOf is the Spread of the sentence index of a run, as the way out writes it (written) or
	// as the text holds it: a sentence of changed, or one between two of them that changedRuns read.
	spreadOf := func(written bool) func(index int) ste.Spread {
		return func(index int) ste.Spread {
			if each, ok := changed[index]; ok {
				if written {
					return each.spread
				}
				return each.was.spread
			}
			return read[w.units[index]][w.nth[index]].spread
		}
	}
	for _, run := range runs {
		// The piece that starts the sentence after the run is part of what both readings count
		// where either runs on into it (next), so that the count after the way out holds it as a
		// piece of its own where the way out ends the piece that ran on into it, and the count
		// before it where the way out makes a piece run on into it.
		first, last := changed[run[0]], changed[run[1]]
		next := last.was.spread.Open || last.spread.Open
		pieces(run[0], run[1], spreadOf(false), first.was.left, last.was.after, next, func(words int) { addLong(long, words, -1) })
		pieces(run[0], run[1], spreadOf(true), first.was.left, last.was.after, next, func(words int) { addLong(long, words, 1) })
	}
	if !noneNew(after, w.before) {
		return false, true
	}
	for _, count := range long {
		if count > 0 {
			return false, true
		}
	}
	if !w.dangling(changed) {
		return true, true
	}
	// The finding of dangling-conjunction quotes the item whole, so it stays the text's own only
	// where each changed sentence reads to the rule as it did.
	if !w.dangles {
		return false, true
	}
	for _, written := range changed {
		if ste.Neutral(written.text) != ste.Neutral(written.was.span) {
			return false, true
		}
	}
	return true, true
}

// lintedWhole is the findings of the rules that read more than one sentence (wholeText) in line,
// read whole by ste-lint, with the verbs of its footnotes, held, and its verbs: the reading of a
// text that ste-lint reads as more than one line, and the one that the sentences of any other text
// add up to.
func lintedWhole(line string, held ste.Held) (map[findingKey]int, ste.Verbs) {
	lint, verbs := ste.LintVerbs(line)
	found := map[findingKey]int{}
	for _, finding := range lint {
		switch finding.Rule {
		case "synonym-rotation", "long-sentence", "dangling-conjunction":
			found[findingKeyOf(steBreak(finding))]++
		}
	}
	for _, each := range rotationBreaks(held.Rotations(verbs), "its footnotes") {
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
// in place of its own, and its verbs (rotationsOf), and whether the charge of each sentence that it
// read let it read it. A member whose first place is in a sentence of changed first stands, beside
// changed, in the next sentence of stands that changed does not hold, which it reads through its
// charge (read).
func (w *wholeText) rotations(changed map[int]writtenSentence, read map[charge.Unit][]sentenceAt) (map[findingKey]int, ste.Verbs, charge.Answer) {
	firsts := map[[2]int]sentenceHit{}
	for member, first := range w.firsts {
		if _, ok := changed[first.sentence]; !ok {
			firsts[member] = first
			continue
		}
		for _, stand := range w.stands[member][1:] {
			if _, ok := changed[stand.sentence]; ok {
				continue
			}
			at, charged := w.other(stand.sentence, read)
			if !charged {
				return nil, 0, false
			}
			firsts[member] = sentenceHit{stand.sentence, at.spread.Hits[stand.hit]}
			break
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
	found, verbs := w.rotationsOf(firsts)
	return found, verbs, true
}

// rotationsOf is the findings of the rule synonym-rotation of a text whose members of the synonym
// groups first stand at firsts, and its verbs: of its own words, each member of a group after the
// first one to stand in it, at its first place, in the order of ste.LintVerbs; and of its words with
// those of its footnotes (ste.Held.Rotations, held). It reads no sentence.
func (w *wholeText) rotationsOf(firsts map[[2]int]sentenceHit) (map[findingKey]int, ste.Verbs) {
	groups := map[int][]sentenceHit{}
	var verbs ste.Verbs
	for _, first := range firsts {
		groups[first.hit.Group] = append(groups[first.hit.Group], first)
		verbs |= first.hit.Verb()
	}
	found := map[findingKey]int{}
	for _, present := range groups {
		// The sentences in order, and the places of one sentence in ste-lint's order (Hit.Less).
		sort.Slice(present, func(a, b int) bool {
			x, y := present[a], present[b]
			if x.sentence != y.sentence {
				return x.sentence < y.sentence
			}
			return x.hit.Less(y.hit)
		})
		for _, each := range present[1:] {
			found[findingKeyOf(steBreak(each.hit.Rotation()))]++
		}
	}
	for _, each := range rotationBreaks(w.held.Rotations(verbs), "its footnotes") {
		found[findingKeyOf(each)]++
	}
	return found, verbs
}

// changedRuns is the sentences of changed as runs, each from the first to the last of the sentences
// of changed that the pieces of ste-lint join: a piece runs on, in the text or as changed writes
// it, from each sentence of a run to the next. So no piece runs from a run into a sentence of
// changed outside it, before the way out or after it. It reads the Spread of each sentence between
// two of changed until a piece stops there, through the sentence's charge (read), which the pieces
// of the runs then read from read; it reports false where a charge refuses one.
func (w *wholeText) changedRuns(changed map[int]writtenSentence, read map[charge.Unit][]sentenceAt) ([][2]int, charge.Answer) {
	indexes := make([]int, 0, len(changed))
	for index := range changed {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	open := func(index int) (bool, charge.Answer) {
		if each, ok := changed[index]; ok {
			return each.was.spread.Open || each.spread.Open, true
		}
		at, charged := w.other(index, read)
		if !charged {
			return false, false
		}
		return at.spread.Open, true
	}
	var runs [][2]int
	for _, index := range indexes {
		if count := len(runs); count > 0 {
			joined, charged := true, charge.Answer(true)
			for at := runs[count-1][1]; at < index && joined; at++ {
				if joined, charged = open(at); !charged {
					return nil, false
				}
			}
			if joined {
				runs[count-1][1] = index
				continue
			}
		}
		runs = append(runs, [2]int{index, index})
	}
	return runs, true
}

// pieces calls emit with the words of each piece of ste-lint that the sentences from to through
// take part in, each sentence's Spread as spread gives it, with the words of the piece that runs on
// into from from the sentences before it (left), and from through into the sentences after it
// (after). Where next asks for the piece that starts the sentence after through and through does
// not run on into it, that piece is emitted alone, as ste-lint reads it then.
func pieces(from, through int, spread func(index int) ste.Spread, left, after int, next bool, emit func(int)) {
	open := left
	for index := from; index <= through; index++ {
		each := spread(index)
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
	switch {
	case spread(through).Open:
		emit(open + after)
	case next:
		emit(after)
	}
}

// dangling reports whether the text with the sentences of changed in place of its own is a list
// item that ends in "and" or "or", as the rule dangling-conjunction reads it: its mark stands in
// its first sentence, and its last word in its last. It reads only the sentences of changed, and
// for the others what load read of them once (listItem, endsInConjunction).
func (w *wholeText) dangling(changed map[int]writtenSentence) bool {
	count := len(w.units)
	if count == 0 {
		return false
	}
	first, last := w.listItem, w.endsInConjunction
	if each, ok := changed[0]; ok {
		first = ste.ListItem(each.text)
	}
	if each, ok := changed[count-1]; ok && first {
		last = ste.EndsInConjunction(each.text)
	}
	return first && last
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
