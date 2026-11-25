package specform

import (
	"fmt"
	"iter"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specform/charge"
	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// maxJudgedInSentence is how many findings of one sentence the judge of the ways out reads the
// ways out of (wayJudge.offered). A stop with no space after it and the address of a person can
// stand many times in one sentence, and each reading of a way out reads the whole sentence, so the
// judge reads one sentence at most this many times for each way out a finding can give, and the
// work of a judgment stays in proportion to its text (the work table's rows of many stops and many
// addresses of a person in one sentence). A finding past this many of one sentence is still
// refused and named, and its detail says that Dispatch offers no way out of it (pastNote). A text
// that the judge reads whole for each way out it reads for the ways out of this many findings of
// the text, in the same way (wholeText.passes, wholeNote). The charge of each sentence counts the
// readings that this bound holds (ownReads, charge.Texts).
const maxJudgedInSentence = 8

// pastNote is what the detail of a finding past the first maxJudgedInSentence findings of its
// sentence says, so the writer knows that Dispatch offers no way out of it, not that none exists.
var pastNote = fmt.Sprintf("Dispatch offers ways out for the first %d findings of a sentence, so it offers none for this one, though one can exist",
	maxJudgedInSentence)

// wholeNote is what the detail of a finding says whose ways out the judge would read in a text
// that it reads whole, past the first maxJudgedInSentence findings of that text (wholeText.passes),
// so the writer knows that Dispatch offers no way out of it, not that none exists.
var wholeNote = fmt.Sprintf("Dispatch offers ways out for the first %d findings of a text that ste-lint reads as more than one line, so it offers none for this one, though one can exist",
	maxJudgedInSentence)

// offer is the ways out that a judge offers a finding (ways), and what the finding says of the ways
// out that the judge did not read (note): pastNote for a finding past the first
// maxJudgedInSentence findings of its sentence, wholeNote for one past those of a text that the
// judge reads whole, elsewhereNote for one whose way out would read a sentence past its charge
// (charge.Texts.Read), documentNote for one whose way out would read past the budget of its
// document (maxWayBytes), or nothing.
type offer struct {
	ways [maxWays]wayOut
	note string
}

// said is the words that give the ways out of the finding of the offer (broken.waysSaid): ways,
// what the finding says of the ways out that it gives, with the offer's note after them.
func (o offer) said(ways string) string {
	switch {
	case o.note == "":
		return ways
	case ways == "":
		return o.note
	}
	return ways + ". " + o.note
}

// wayJudge offers a way out of a finding only where the text that it writes gives no finding that
// the text does not give, word for word, and one finding fewer of the finding's own rule
// (offered). A finding is its rule and its words at its place (findingKey), not the ways out that
// it gives, so a way out that changes only what another finding offers gives no new finding. So no
// refusal suggests a form that another rule refuses: "that" before a clause can make an
// instruction of 21 words, and a space after a stop can make a statement of three sentences. A way
// out changes one sentence and keeps where it ends, so the judge reads that sentence alone, by each
// rule that judges a text of its kind: the form rules, with the other sentences of the text and of
// its footnotes in the count of a statement, and the reference rules. A way out whose sentence no
// longer ends at a stop of its own, and so runs on into the next one, or that ends where it did
// not, writes a text that this reading does not read, so it is no way out (passes). The sentence
// is one of the text's words as the form rules read them, code font and all, also for a finding of
// the words that the page shows (sentencePlaces), and each finding hands it to the judge as the
// text holds it, white space and all, never rebuilt from its words. In that count each stop with
// no space after it but the finding's own ends a sentence, as the space that its own finding asks
// for makes it end one, so the ways out of one finding do not change with the white space after
// the stop of another. One finding stands for each place of its words, and the writer changes the
// words at each place, so the judge offers a way out only where it passes in the sentence of each
// place, with the way out written at each place of the finding's words in that sentence: it first
// takes each place (survey), then judges.
// A rule that reads more than one sentence, such as synonym-rotation, which reads the text whole,
// the judge reads in the whole text that the way out writes at each place (wholeText). The judge
// names each sentence by its unit (charge.Unit), an index, in each map and each walk, and reads a
// sentence's text only through its charge (texts, charge.Texts.Read), so the judge reads one
// sentence a bounded number of times however many findings read it. A nil judge offers each way
// out unjudged: the judge's own reading of a sentence is one.
type wayJudge struct {
	c      Context
	people [][]string
	kind   Kind
	// shown is whether the reference rules read the text's words as the page shows them, with no
	// mark of code font (Held.Shown), so the judge reads them in a sentence so too (read).
	shown bool
	// count is how many sentences of the text and of its footnotes count for the rule sentences, and
	// joined how many stops with no space after them the text holds (told). For a footnote that a
	// statement holds, count is that statement's (heldIn), and stays.
	count, joined int
	inStatement   bool
	// surveying is whether the judge takes places (survey); places is each unit that holds a
	// finding, once and in the order of the text, rank where a finding stands among the
	// findings of each unit that holds it, and ranked how many findings each unit holds.
	surveying bool
	places    map[wayKey][]charge.Unit
	rank      map[place]int
	ranked    map[charge.Unit]int
	units     map[charge.Unit]*judgedUnit
	given     map[wayKey]offer
	// whole is the text as the judge reads it for the rules that read more than one sentence
	// (told), and elsewhere, for the judge of a footnote, what wholeText.elsewhere says
	// (footnoteJudge.rotationCanChange).
	whole     *wholeText
	elsewhere func(before, after ste.Verbs) bool
	// texts is each sentence of the text, of its titles and each address that the judge reads, by
	// its unit, and the charge of each reading of them (charge.Texts), which the whole text reads
	// through too.
	texts *charge.Texts[sentenceAt]
}

// place is a finding, by the words that its ways out change, at one unit that holds it: a
// sentence of the text, as the text holds it; the address of a link or an image
// (charge.Unit.Address), which the reference rules read as an address and no form rule reads; or a
// sentence of a title (charge.Unit.Title), which is no sentence of the text, so the whole text that
// a way out writes holds none of its words (wholeText.passes). A unit that names no text
// (charge.None) is a place that the judge did not find.
type place struct {
	way  wayKey
	unit charge.Unit
}

// judgedUnit is a unit as the judge reads it alone: how many sentences of the text stand outside
// it (others), each stop with no space after it there ending one, how many such stops it holds
// (joined), whether its last sentence ends at a stop of its own (ends, endsWithStop), and each
// finding by its rule and detail, with how many times it stands, read with the stops of the
// finding whose ways out the judge reads left out of the count (findings, by how many those are).
type judgedUnit struct {
	others, joined int
	ends           bool
	findings       map[int]map[findingKey]int
}

// findingKey is a finding as the judge compares two readings of a unit: its rule and its detail,
// the rule's words at its place, word for word, which for a finding of ste100-dangling-conjunction
// quote the last line of its list item. The ways out that the finding gives are no part of it
// (broken.waysSaid).
type findingKey struct {
	rule   Rule
	detail string
}

// wayKey is a finding as the judge keeps the ways out that it offered: its rule and the words that
// its ways out change, which for a finding of the rule instructions are its whole sentence, so a key
// of that rule holds the sentence's text (charge).
type wayKey struct {
	rule Rule
	from string
}

// newWayJudge is the judge of the ways out of the findings of a text of kind, whose words the
// reference rules read as the page shows them where shown (Held.Shown). Its readings take their
// bytes from the budget of the Check that c is of (Context.wayBytes).
func newWayJudge(c Context, people [][]string, kind Kind, shown bool) *wayJudge {
	return &wayJudge{c: c, people: people, kind: kind, shown: shown, texts: newTexts(c.wayBytes)}
}

// sentence is the unit of the sentence index of the text that the judge was told (told), as
// sentencesOf splits it; no unit for a nil judge, a judge told no text or an index past its
// sentences. The form rules name the sentence that holds a finding by it, so no finding names its
// sentence by the sentence's text.
func (j *wayJudge) sentence(index int) charge.Unit {
	if j == nil || j.whole == nil || index >= len(j.whole.units) {
		return charge.None(false)
	}
	return j.whole.units[index]
}

// named is the unit of text, a sentence that a rule reads in a split of its own (instructionBreaks),
// or of an address (address): the unit of the sentence of the text of the same words where there is
// one (charge.Texts.Name). A rule names each such text once in each run, not once for each finding.
// No unit for a nil judge.
func (j *wayJudge) named(text string, address bool) charge.Unit {
	if j == nil {
		return charge.None(false)
	}
	return j.texts.Name(text, address, false)
}

// told gives the judge what the form rules read of the whole text: how many sentences count for
// the rule sentences, with those of its footnotes, how many stops with no space after them it
// holds (joined), and the text itself as one line with the verbs of its footnotes (held), for the
// rules that read more than one sentence (wholeText). The count of the judge of a footnote that a
// statement holds stays the statement's (heldIn). A judge judges one text, which each run of the
// rules tells it the same, so it reads the line the first time that it is told.
func (j *wayJudge) told(count, joined int, line string, held []ste.Verbs) {
	if j == nil {
		return
	}
	if !j.inStatement {
		j.count = count
	}
	j.joined = joined
	if j.whole == nil {
		j.whole = newWholeText(line, held, j.elsewhere, j.texts)
	}
}

// heldIn tells the judge of a footnote that a statement holds it, a statement of count sentences
// with those of its footnotes at most (footnoteJudge.statements). The footnote's sentences count in
// the statement's, so the judge reads each sentence of the footnote as a sentence of that statement,
// by the rule sentences too: a way out that adds a sentence to a footnote is judged as the refusal
// counts the statement.
func (j *wayJudge) heldIn(count int) {
	j.kind, j.count, j.inStatement = KindStatement, count, true
}

// survey runs find with the judge taking each place of each finding that find asks an offer of,
// and offering no way out (offered), so that the judge offers a run of find after it each way out
// judged at every place of its finding. find asks the same offers each time it runs, and only the
// findings of the run after survey stand. A nil judge takes nothing.
func (j *wayJudge) survey(find func()) {
	if j == nil {
		return
	}
	j.surveying = true
	find()
	j.surveying = false
}

// offered is the offer of ways, the ways out of a finding of rule whose ways out change from in
// unit, the place that holds it, with a zero wayOut in
// place of each that passes no reading (passes) at some place of the finding (survey), or that the
// whole text that it writes at each place refuses (wholeText.passes). A finding
// past maxJudgedInSentence of the findings of a unit that holds it gives none, and its offer says
// so (pastNote), as does one past maxJudgedInSentence of the findings of a text that the judge
// reads whole (wholeNote), one whose way out would read a sentence past its charge
// (elsewhereNote), and one that asks once the budget of its document is spent (documentNote); so
// does one whose sentence is not known at some place (a unit that names no
// text), with no word of it, and one that survey did not see. A nil judge offers ways as they are,
// and so does each judge a finding that gives none.
func (j *wayJudge) offered(unit charge.Unit, rule Rule, from string, ways [maxWays]wayOut) offer {
	if j == nil || ways == ([maxWays]wayOut{}) {
		return offer{ways: ways}
	}
	key := wayKey{rule, from}
	if j.surveying {
		j.take(place{key, unit})
		return offer{}
	}
	if given, ok := j.given[key]; ok {
		return given
	}
	if j.texts.Spent() {
		return offer{note: documentNote}
	}
	given := offer{ways: ways}
	places := j.places[key]
	for _, at := range places {
		if j.rank[place{key, at}] >= maxJudgedInSentence {
			given.note = pastNote
		}
		if !at.Named() || given.note != "" {
			given.ways = [maxWays]wayOut{}
		}
	}
	if len(places) == 0 {
		given.ways = [maxWays]wayOut{}
	}
	// The writer changes the words at each place where they stand (strings.ReplaceAll), also where
	// the finding does not stand: in a name that ends a sentence there ("Web.Config." beside
	// "Web.Config"), which is a finding of its own, or in an address that a colon makes a remote
	// path there ("deploy@build.example.com:."), which is none. So the judge reads the way out in
	// those sentences too (also), where it must give no new finding, and the finding's own rule
	// need not give fewer. One sentence can hold the words of many findings, as one sentence can
	// hold in code font the names that many other sentences end with, so each such reading, as each
	// reading of a sentence for any purpose, asks the sentence's charge first (charge.Texts.Read): a
	// judgment reads one sentence at most maxReadsOfSentence times, whatever reads it and for
	// however many findings, so its work is at most that many readings of its text (the work
	// table's rows of one sentence that holds the words of many findings). A finding whose way out
	// would read a sentence past its charge gets no way out, and says so (elsewhereNote).
	refuse := func(note string) { given.ways, given.note = [maxWays]wayOut{}, note }
	var also []charge.Unit
	if given.ways != ([maxWays]wayOut{}) {
		var charged charge.Answer
		if also, charged = j.alsoAt(from, places); !charged {
			refuse(refusedNote(j.texts))
		}
	}
	for at, holds := range j.eachPlace(places, also) {
		if given.ways == ([maxWays]wayOut{}) {
			break
		}
		state, charged := j.unit(at, holds)
		for index, way := range given.ways {
			if !charged || way == (wayOut{}) {
				continue
			}
			var passed bool
			if passed, charged = j.passes(state, at, rule, way, holds); !passed {
				given.ways[index] = wayOut{}
			}
		}
		if !charged {
			refuse(refusedNote(j.texts))
		}
	}
	if j.whole != nil {
		for index, way := range given.ways {
			if way == (wayOut{}) {
				continue
			}
			passed, note := j.whole.passes(key, way, places, also)
			if note != "" {
				refuse(note)
				break
			}
			if !passed {
				given.ways[index] = wayOut{}
			}
		}
	}
	if j.given == nil {
		j.given = map[wayKey]offer{}
	}
	j.given[key] = given
	return given
}

// take keeps at, a place of a finding, once: the unit among the places of its finding, and the
// finding's rank among the findings of the unit.
func (j *wayJudge) take(at place) {
	if _, ok := j.rank[at]; ok {
		return
	}
	if j.rank == nil {
		j.places, j.rank, j.ranked = map[wayKey][]charge.Unit{}, map[place]int{}, map[charge.Unit]int{}
	}
	j.rank[at] = j.ranked[at.unit]
	j.ranked[at.unit]++
	j.places[at.way] = append(j.places[at.way], at.unit)
}

// unit is the judge's reading of unit, read once, for a finding that stands in it (own) or for one
// that does not: how many sentences of the text stand outside it, how many stops with no space
// after them it holds, and whether it ends at a stop of its own. It reports false, and reads
// nothing, where the unit's charge refuses the reading (charge.Texts.Read).
func (j *wayJudge) unit(unit charge.Unit, own bool) (*judgedUnit, charge.Answer) {
	if state, ok := j.units[unit]; ok {
		return state, true
	}
	state := &judgedUnit{findings: map[int]map[findingKey]int{}}
	if prose(j.kind) && !unit.Address() {
		text, _, charged := j.texts.Read(unit, own)
		if !charged {
			return nil, false
		}
		sentences := sentencesOf(text)
		state.joined = joinedIn(sentences)
		state.others = max(0, j.count+j.joined-len(sentences)-state.joined)
		state.ends = endsWithStop(text)
	}
	if j.units == nil {
		j.units = map[charge.Unit]*judgedUnit{}
	}
	j.units[unit] = state
	return state, true
}

// before is each finding of the unit that state reads, with how many times it stands, where each
// stop with no space after it ends a sentence but the stops of the finding whose ways out the judge
// reads, stops of them, read once for each count of such stops, for a finding that stands in the
// unit (own) or for one that does not. It reports false, and reads nothing, where the unit's charge
// refuses the reading.
func (j *wayJudge) before(state *judgedUnit, unit charge.Unit, stops int, own bool) (map[findingKey]int, charge.Answer) {
	if found, ok := state.findings[stops]; ok {
		return found, true
	}
	text, _, charged := j.texts.Read(unit, own)
	if !charged {
		return nil, false
	}
	found := j.read(text, unit.Address(), state.others+state.joined-stops)
	state.findings[stops] = found
	return found, true
}

// read is each finding of text, the text of a unit or what a way out writes in its place (an
// address of a link or an image where address), with how many times it stands: for a sentence of
// prose, the form rules, as a sentence of a text of the judge's kind that outside sentences of the
// text stand beside; then the reference rules, for an address as an address, and for a sentence of
// a text that the page shows with no code font as the page shows it (withoutCodeFont). It judges no
// way out. Its callers read the text through its charge (before, passes).
func (j *wayJudge) read(text string, address bool, outside int) map[findingKey]int {
	findings := map[findingKey]int{}
	if prose(j.kind) && !address {
		form, _, _ := formBreaks(j.kind, text, footnotesHeld{count: outside}, nil)
		for _, each := range form {
			findings[findingKey{each.rule, each.detail}]++
		}
	}
	words := text
	if j.shown && !address {
		words = withoutCodeFont(words)
	}
	for _, each := range referenceBreaks(j.c, j.people, words, address, nil, nil) {
		findings[findingKey{each.rule, each.detail}]++
	}
	return findings
}

// passes reports whether way, a way out of a finding of rule in the unit that state reads, written
// at each place of its words in the unit, as the writer who follows the finding writes it, writes a
// unit that keeps where its last sentence ends (endsWithStop), gives no finding that the unit does
// not give as many times, and, where the finding stands in the unit (holds), gives fewer findings
// of rule (passed); and whether the unit's charge let the judge read it (charged). Each stop with
// no space after it of another finding ends a sentence in the count of both readings: in the unit
// before the way out, all but the finding's own, which are the stops that the way out takes away at
// each place; after it, each that is left.
func (j *wayJudge) passes(state *judgedUnit, unit charge.Unit, rule Rule, way wayOut, holds bool) (passed bool, charged charge.Answer) {
	text, _, charged := j.texts.Read(unit, holds)
	if !charged {
		return false, false
	}
	changed := strings.ReplaceAll(text, way.from, way.to)
	if changed == text {
		return false, true
	}
	stops, outside := 0, state.others
	if prose(j.kind) && !unit.Address() {
		if endsWithStop(changed) != state.ends {
			return false, true
		}
		left := joinedIn(sentencesOf(changed))
		outside += left
		if rule == ruleStopNoSpace {
			stops = state.joined - left
		}
	}
	before, charged := j.before(state, unit, stops, holds)
	if !charged {
		return false, false
	}
	after := j.read(changed, unit.Address(), outside)
	own, ownAfter := 0, 0
	for key, count := range after {
		if count > before[key] {
			return false, true
		}
		if key.rule == rule {
			ownAfter += count
		}
	}
	for key, count := range before {
		if key.rule == rule {
			own += count
		}
	}
	return !holds || ownAfter < own, true
}

// alsoAt is each sentence of the text, by its unit, that holds from where no place of places
// stands: a place of the words of a finding where the finding does not stand (offered); and
// whether the charge of each sentence that it read let it read it (wholeText.holding). A judge told
// no text (told) knows no sentence beside places.
func (j *wayJudge) alsoAt(from string, places []charge.Unit) ([]charge.Unit, charge.Answer) {
	if j.whole == nil {
		return nil, true
	}
	return j.whole.holding(from, places)
}

// eachPlace is each unit of places, where the finding stands (true), then each of also, where only
// its words stand (false).
func (j *wayJudge) eachPlace(places, also []charge.Unit) iter.Seq2[charge.Unit, bool] {
	return func(yield func(charge.Unit, bool) bool) {
		for _, at := range places {
			if !yield(at, true) {
				return
			}
		}
		for _, at := range also {
			if !yield(at, false) {
				return
			}
		}
	}
}

// sentencePlaces finds the sentence that holds a place of a finding, for the judge of the ways out
// (wayJudge.offered): a sentence of words, the words of a text as the rules of the form read them,
// code font and all (sentencesOf), which take each byte of words once, named by its unit. A place
// is an offset of shown, the words that the reference rules read: the words as the page shows them
// (Held.Shown), which are words with the marks of code font taken out, so each byte of shown
// stands at the next byte of words that equals it (wordsAt); or words itself, for a text with no
// Shown and for a title, whose units say so (charge.Unit.Title). It keeps no byte of words or
// shown: only where each byte of shown stands in words, where each sentence ends, and its unit.
type sentencePlaces struct {
	title bool
	// wordsAt is the offset in words of each byte of shown, or -1 past the last byte of shown that
	// words holds; ends is the end of each sentence of words in words, and units its unit.
	wordsAt []int32
	ends    []int
	units   []charge.Unit
}

// placesIn is the sentencePlaces of words, as the page shows them (shown), a title's where title,
// each of its sentences named once (charge.Texts.Name). It is nil for a nil judge, which takes no
// place, and outside the survey, where the judge takes none (survey): the run after the survey
// asks the offers that the survey's places gave (offered).
func (j *wayJudge) placesIn(words, shown string, title bool) *sentencePlaces {
	if j == nil || !j.surveying {
		return nil
	}
	p := &sentencePlaces{title: title, wordsAt: make([]int32, len(shown))}
	for _, each := range sentencesOf(words) {
		p.ends = append(p.ends, each.end)
		p.units = append(p.units, j.texts.Name(each.in(words), false, title))
	}
	at := 0
	for index := 0; index < len(shown); index++ {
		for at < len(words) && words[at] != shown[index] {
			at++
		}
		if at == len(words) {
			for ; index < len(shown); index++ {
				p.wordsAt[index] = -1
			}
			break
		}
		p.wordsAt[index] = int32(at)
		at++
	}
	return p
}

// of is the unit of the sentence of words that holds the place of shown at offset, where name, the
// words of shown there, stands in words as one run of bytes; or a unit of no text where words
// writes name at no such place, as for a name that the marks of code font cut, or holds no
// sentence there. A nil sentencePlaces finds no place.
func (p *sentencePlaces) of(offset int, name string) charge.Unit {
	if p == nil {
		return charge.None(false)
	}
	last := offset + len(name) - 1
	if name == "" || offset < 0 || last >= len(p.wordsAt) {
		return charge.None(p.title)
	}
	start := p.wordsAt[offset]
	if start < 0 || p.wordsAt[last] != start+int32(len(name))-1 {
		return charge.None(p.title)
	}
	index := sort.Search(len(p.ends), func(index int) bool { return p.ends[index] > int(start) })
	if index == len(p.ends) {
		return charge.None(p.title)
	}
	return p.units[index]
}
