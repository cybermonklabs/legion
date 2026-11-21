package specform

import (
	"fmt"
	"strings"

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
// the text, in the same way (wholeText.passes, wholeNote).
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
// judge reads whole, or nothing.
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
// out changes one sentence, so the judge reads that sentence alone, by each rule that judges a text
// of its kind: the form rules, with the other sentences of the text and of its footnotes in the
// count of a statement, and the reference rules. The sentence is one of the text's words as the
// form rules read them, code font and all, also for a finding of the words that the page shows
// (sentencePlaces). In that count each stop with no space after it but the finding's own ends a
// sentence, as the space that its own finding asks for makes it end one, so the ways out of one
// finding do not change with the white space after the stop of another. One finding stands for
// each place of its words, and the writer changes the words at each place, so the judge offers a
// way out only where it passes in the sentence of each place, with the way out written at each
// place of the finding's words in that sentence: it first takes each place (survey), then judges.
// A rule that reads more than one sentence, such as synonym-rotation, which reads the text whole,
// the judge reads in the whole text that the way out writes at each place (wholeText). A nil judge
// offers each way out unjudged: the judge's own reading of a sentence is one.
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
	places    map[wayKey][]unitKey
	rank      map[place]int
	ranked    map[unitKey]int
	units     map[unitKey]*judgedUnit
	given     map[wayKey]offer
	// whole is the text as the judge reads it for the rules that read more than one sentence
	// (told), and elsewhere, for the judge of a footnote, what wholeText.elsewhere says
	// (footnoteJudge.rotationCanChange).
	whole     *wholeText
	elsewhere func(before, after ste.Verbs) bool
}

// unitKey is a sentence that the judge reads, or the address of a link or an image (address),
// which the reference rules read as an address and no form rule reads.
type unitKey struct {
	text    string
	address bool
}

// place is a finding, by the words that its ways out change, at one unit that holds it.
type place struct {
	way  wayKey
	unit unitKey
}

// judgedUnit is a unit as the judge reads it alone: how many sentences of the text stand outside
// it (others), each stop with no space after it there ending one, how many such stops it holds
// (joined), and each finding by its rule and detail, with how many times it stands, read with the
// stops of the finding whose ways out the judge reads left out of the count (findings, by how many
// those are).
type judgedUnit struct {
	others, joined int
	findings       map[int]map[findingKey]int
}

// findingKey is a finding as the judge compares two readings of a unit: its rule and its detail,
// the rule's words at its place, word for word. The ways out that the finding gives are no part of
// it (broken.waysSaid).
type findingKey struct {
	rule   Rule
	detail string
}

// wayKey is a finding as the judge keeps the ways out that it offered: its rule and the words that
// its ways out change.
type wayKey struct {
	rule Rule
	from string
}

// newWayJudge is the judge of the ways out of the findings of a text of kind, whose words the
// reference rules read as the page shows them where shown (Held.Shown).
func newWayJudge(c Context, people [][]string, kind Kind, shown bool) *wayJudge {
	return &wayJudge{c: c, people: people, kind: kind, shown: shown}
}

// told gives the judge what the form rules read of the whole text: how many sentences count for
// the rule sentences, with those of its footnotes, how many stops with no space after them it
// holds (joined), and the text itself as one line with the verbs of its footnotes (held), for the
// rules that read more than one sentence (wholeText). The count of the judge of a footnote that a
// statement holds stays the statement's (heldIn).
func (j *wayJudge) told(count, joined int, line string, held []ste.Verbs) {
	if j == nil {
		return
	}
	if !j.inStatement {
		j.count = count
	}
	j.joined = joined
	if j.whole == nil || j.whole.line != line {
		j.whole = newWholeText(line, held, j.elsewhere)
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
// unit, the sentence that holds it, or the address that holds it (address), with a zero wayOut in
// place of each that passes no reading (passes) at some place of the finding (survey), or that the
// whole text that it writes at each place refuses (wholeText.passes). A finding
// past maxJudgedInSentence of the findings of a unit that holds it gives none, and its offer says
// so (pastNote), as does one past maxJudgedInSentence of the findings of a text that the judge
// reads whole (wholeNote); so does one whose sentence is not known at some place (unit ""), with no
// word of it, and one that survey did not see. A nil judge offers ways as they are, and so does
// each judge a finding that gives none.
func (j *wayJudge) offered(unit string, address bool, rule Rule, from string, ways [maxWays]wayOut) offer {
	if j == nil || ways == ([maxWays]wayOut{}) {
		return offer{ways: ways}
	}
	key := wayKey{rule, from}
	if j.surveying {
		j.take(place{key, unitKey{unit, address}})
		return offer{}
	}
	if given, ok := j.given[key]; ok {
		return given
	}
	given := offer{ways: ways}
	places := j.places[key]
	for _, at := range places {
		if j.rank[place{key, at}] >= maxJudgedInSentence {
			given.note = pastNote
		}
		if at.text == "" || given.note != "" {
			given.ways = [maxWays]wayOut{}
		}
	}
	if len(places) == 0 {
		given.ways = [maxWays]wayOut{}
	}
	for _, at := range places {
		if given.ways == ([maxWays]wayOut{}) {
			break
		}
		state := j.unit(at)
		for index, way := range given.ways {
			if way != (wayOut{}) && !j.passes(state, at, rule, way) {
				given.ways[index] = wayOut{}
			}
		}
	}
	if j.whole != nil {
		for index, way := range given.ways {
			if way == (wayOut{}) {
				continue
			}
			passed, read := j.whole.passes(key, way, places)
			if !read {
				given.ways, given.note = [maxWays]wayOut{}, wholeNote
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
		j.places, j.rank, j.ranked = map[wayKey][]unitKey{}, map[place]int{}, map[unitKey]int{}
	}
	j.rank[at] = j.ranked[at.unit]
	j.ranked[at.unit]++
	j.places[at.way] = append(j.places[at.way], at.unit)
}

// unit is the judge's reading of unit, read once: how many sentences of the text stand outside it
// and how many stops with no space after them it holds.
func (j *wayJudge) unit(unit unitKey) *judgedUnit {
	if state, ok := j.units[unit]; ok {
		return state
	}
	state := &judgedUnit{findings: map[int]map[findingKey]int{}}
	if prose(j.kind) && !unit.address {
		sentences := sentencesOf(unit.text)
		state.joined = joinedIn(sentences)
		state.others = max(0, j.count+j.joined-len(sentences)-state.joined)
	}
	if j.units == nil {
		j.units = map[unitKey]*judgedUnit{}
	}
	j.units[unit] = state
	return state
}

// before is each finding of the unit that state reads, with how many times it stands, where each
// stop with no space after it ends a sentence but the stops of the finding whose ways out the judge
// reads, stops of them, read once for each count of such stops.
func (j *wayJudge) before(state *judgedUnit, unit unitKey, stops int) map[findingKey]int {
	if found, ok := state.findings[stops]; ok {
		return found
	}
	found := j.read(unit, state.others+state.joined-stops)
	state.findings[stops] = found
	return found
}

// read is each finding of unit, with how many times it stands: for a sentence of prose, the form
// rules, as a sentence of a text of the judge's kind that outside sentences of the text stand
// beside; then the reference rules, for an address as an address, and for a sentence of a text
// that the page shows with no code font as the page shows it (withoutCodeFont). It judges no way
// out.
func (j *wayJudge) read(unit unitKey, outside int) map[findingKey]int {
	findings := map[findingKey]int{}
	if prose(j.kind) && !unit.address {
		form, _, _ := formBreaks(j.kind, unit.text, footnotesHeld{count: outside}, nil)
		for _, each := range form {
			findings[findingKey{each.rule, each.detail}]++
		}
	}
	words := unit.text
	if j.shown && !unit.address {
		words = withoutCodeFont(words)
	}
	for _, each := range referenceBreaks(j.c, j.people, words, unit.address, nil, nil) {
		findings[findingKey{each.rule, each.detail}]++
	}
	return findings
}

// passes reports whether way, a way out of a finding of rule in the unit that state reads, written
// at each place of its words in the unit, as the writer who follows the finding writes it, writes a
// unit that gives no finding that the unit does not give as many times, and fewer findings of rule.
// Each stop with no space after it of another finding ends a sentence in the count of both
// readings: in the unit before the way out, all but the finding's own, which are the stops that the
// way out takes away at each place; after it, each that is left.
func (j *wayJudge) passes(state *judgedUnit, unit unitKey, rule Rule, way wayOut) bool {
	changed := unitKey{strings.ReplaceAll(unit.text, way.from, way.to), unit.address}
	if changed.text == unit.text {
		return false
	}
	stops, outside := 0, state.others
	if prose(j.kind) && !unit.address {
		left := joinedIn(sentencesOf(changed.text))
		outside += left
		if rule == ruleStopNoSpace {
			stops = state.joined - left
		}
	}
	before, after := j.before(state, unit, stops), j.read(changed, outside)
	own, ownAfter := 0, 0
	for key, count := range after {
		if count > before[key] {
			return false
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
	return ownAfter < own
}

// sentencePlaces finds the sentence that holds a place of a finding, for the judge of the ways out
// (wayJudge.offered): a sentence of words, the words of a text as the rules of the form read them,
// code font and all (sentencesOf), which take each byte of words once. A place is an offset of
// shown, the words that the reference rules read: the words as the page shows them (Held.Shown),
// which are words with the marks of code font taken out, so each byte of shown stands at the next
// byte of words that equals it; or words itself, for a text with no Shown and for a title. One
// cursor walks shown and words once, and one the sentences, for places asked in the order of shown.
type sentencePlaces struct {
	words, shown     string
	sentences        []sentence
	read             bool
	at               int
	shownAt, wordsAt int
}

// of is the sentence of words that holds the place of shown at offset, at or after the last place
// asked for, where name stands; or "" where words writes name at no such place, as for a name that
// the marks of code font cut, or holds no sentence there.
func (p *sentencePlaces) of(offset int, name string) string {
	if !p.read {
		p.sentences, p.read = sentencesOf(p.words), true
	}
	for p.shownAt < offset && p.wordsAt < len(p.words) {
		if p.words[p.wordsAt] == p.shown[p.shownAt] {
			p.shownAt++
		}
		p.wordsAt++
	}
	for p.shownAt == offset && p.wordsAt < len(p.words) && p.words[p.wordsAt] != p.shown[offset] {
		p.wordsAt++
	}
	if p.shownAt < offset || !strings.HasPrefix(p.words[p.wordsAt:], name) {
		return ""
	}
	for p.at < len(p.sentences) && p.sentences[p.at].end <= p.wordsAt {
		p.at++
	}
	if p.at == len(p.sentences) {
		return ""
	}
	return p.sentences[p.at].in(p.words)
}
