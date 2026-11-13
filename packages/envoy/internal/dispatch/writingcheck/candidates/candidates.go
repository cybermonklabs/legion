// Package candidates is the code stage of the writing check's detector: it picks, by grammar
// alone, the prose sentences that may hold a vacuous verb phrase, so the model reader reads only
// those.
//
// Each sentence is split into words by github.com/jdkato/prose/v3/tokenize and tagged with Penn
// Treebank tags by github.com/jdkato/prose/v3/tag. A sentence is a candidate when any one of its
// clauses has this shape; what the rest of the sentence holds does not matter:
//
//  1. a subject: a noun, a pronoun, a determiner or a code token somewhere before the verb in
//     the sentence (the clause's own subject, a relative pronoun, or the subject of an earlier
//     clause whose verb phrase this one continues);
//  2. a verb group: a finite verb (VBZ, VBD, VBP), a modal (MD), or a base form (VB) straight
//     after its subject or after "to", followed by any adverbs and further verbs (VB, VBN, VBG),
//     so "will name", "has shown", "did not pay", "is naming" and "is named" are each one group
//     whose main verb is its last verb. A past form the tagger reads as a participle (VBN) is the
//     finite past when it follows its subject's noun and an object follows it ("The plan named
//     the step"), or, in a sentence with no other finite verb, a preposition ("This branch paid
//     for itself"). A word that states a value is never a verb;
//  3. a main verb that is not a form of be, have or do (a copula, a possession or a stand-in
//     verb) and not a modal standing alone; an auxiliary or a modal before the main verb never
//     excludes it;
//  4. a complement with no value in it: a light object noun phrase with the prepositional
//     phrases that hang off it ("names the owner of the queue", reason svo-empty), a particle
//     with an optional object, or a stranded preposition ("pays off", "what it waits on",
//     sv-particle), a preposition and its noun phrase ("pays for itself", sv-prep), a
//     that-clause or a wh-clause ("shows that it works", "shows what the fix does", sv-clause),
//     or nothing: the verb ends a relative clause whose object is the noun before it ("the head
//     this issue names", sv-gap), the verb is a passive participle ("The owners are named.",
//     sv-passive), or the clause is its subject and its verb ("That pays.", "until the fix
//     lands,", sv-bare).
//
// A value is a number (CD) or a word holding a digit, a currency or number sign, the code or
// address token of package prose, a word inside double quotes, a proper noun (NNP, NNPS) that
// heads its noun phrase, or a colon after the object followed by a list or a value. A number
// spelled in letters with no noun after it stands for a noun said elsewhere ("pays one") and is
// no value. A value inside the complement, or inside a that-clause or wh-clause up to the next
// semicolon, rules that clause out; a value anywhere else in the sentence does not.
//
// The tagger reads some verbs as nouns or adjectives ("The record names ..." tags names NNS,
// "Owners name ..." tags name NN, "the head this issue names" tags names NNS). Such a word is
// read as the verb when it follows its subject's head (a noun, a pronoun, a value word, a
// determiner that opens its clause, an adjective after a determiner) and an object, a particle
// or (in a sentence with no finite verb) a preposition, an adjective or a plural noun follows
// it; when it ends in -s, follows a conjunction or a comma and an object follows it ("..., and
// names the limit"); and when it ends in -s or -ed and closes a noun run that opens with a
// determiner, a pronoun or a value word straight after another noun (sv-gap).
//
// The only word lists are exclusions: the forms of be, have and do, and the modals the tagger
// may miss, each excluding a main verb. Options.NoLists drops both, which shows that what the
// grammar finds does not rest on them: without them a sentence can only gain candidates.
//
// Find answers the candidates strongest first, so a reader that reads only the first few reads
// the likeliest: a candidate ranks by its complement's shape, by a complement of at most three
// words, and by a clause that is the whole sentence; candidates of equal strength keep document
// order.
package candidates

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/jdkato/prose/v3/tag"
	"github.com/jdkato/prose/v3/tokenize"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// The reason codes a candidate carries: which complement of step 4 its clause has.
const (
	ReasonObject   = "svo-empty"
	ReasonParticle = "sv-particle"
	ReasonPrep     = "sv-prep"
	ReasonBare     = "sv-bare"
	ReasonClause   = "sv-clause"
	ReasonGap      = "sv-gap"
	ReasonPassive  = "sv-passive"
)

// ContextSentences is how many sentences of context a candidate carries on each side.
const ContextSentences = 2

// Options changes how Find reads.
type Options struct {
	// NoLists drops every word list (the auxiliaries and the modals), so only tags decide.
	NoLists bool
}

// Candidate is a sentence the reader is to read. ID is unique among the candidates of one Find
// call, Reason is the shape's reason code, and Before and After are up to ContextSentences
// sentences of the same field on each side, in document order.
type Candidate struct {
	ID       string
	Sentence prose.Sentence
	Reason   string
	Before   []string
	After    []string
}

// Find is the candidates among sentences, strongest first; candidates of equal strength keep
// the sentences' order. A sentence's context is drawn from the sentences next to it in the slice
// that carry the same Field.
func Find(sentences []prose.Sentence, opts Options) []Candidate {
	t := tagger()
	type found struct {
		index    int
		reason   string
		strength int
	}
	var all []found
	for i, sentence := range sentences {
		if m, ok := shape(t, sentence.Text, opts); ok {
			all = append(all, found{i, m.reason, m.strength})
		}
	}
	slices.SortStableFunc(all, func(a, b found) int { return cmp.Compare(b.strength, a.strength) })
	out := make([]Candidate, len(all))
	for k, f := range all {
		out[k] = Candidate{
			ID:       fmt.Sprintf("c%d", k+1),
			Sentence: sentences[f.index],
			Reason:   f.reason,
			Before:   around(sentences, f.index, -1),
			After:    around(sentences, f.index, +1),
		}
	}
	return out
}

// around is up to ContextSentences texts next to sentences[i] in direction step, of the same
// field, in document order.
func around(sentences []prose.Sentence, i, step int) []string {
	var texts []string
	for j := i + step; j >= 0 && j < len(sentences) && len(texts) < ContextSentences; j += step {
		if sentences[j].Field != sentences[i].Field {
			break
		}
		texts = append(texts, sentences[j].Text)
	}
	if step < 0 {
		slices.Reverse(texts)
	}
	return texts
}

// tagger is the Penn tagger every Find shares; it is safe for concurrent use.
var tagger = sync.OnceValue(func() *tag.Tagger {
	t, err := tag.New()
	if err != nil {
		// The model is compiled into the binary; failing to decode it is a broken build.
		panic(fmt.Sprintf("candidates: the built-in tagger model: %v", err))
	}
	return t
})

// auxiliaries are the forms of be, have and do, which are never the main verb of a candidate.
var auxiliaries = set("am", "is", "are", "was", "were", "be", "been", "being", "'s", "'re", "'m",
	"has", "have", "had", "having", "'ve", "'d", "does", "do", "did", "done", "doing")

// modals are the modal verbs, for where the tagger does not tag one MD.
var modals = set("can", "could", "may", "might", "must", "shall", "should", "will", "would", "'ll", "ought")

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// word is one tagged word of a sentence. Value is whether the word alone states a value: a
// number, a digit, a sign, a code or address token, a proper noun, or a word inside quotes.
type word struct {
	text  string
	tag   string
	value bool
}

// match is the strongest clause of a sentence.
type match struct {
	reason   string
	strength int
}

// shape is the strongest candidate clause of the sentence, when it has one.
func shape(t *tag.Tagger, text string, opts Options) (match, bool) {
	tokens := tokenize.ForLanguage("en").Tokenize(text)
	t.TagTokens(tokens)
	words := make([]word, 0, len(tokens))
	quoted := false
	for _, tok := range tokens {
		switch tok.Text {
		case `"`, "“", "”", "``", "''":
			// A double quote mark carries no word; the words between two of them are quoted.
			quoted = !quoted
			continue
		}
		if tok.Tag == "``" || tok.Tag == "''" {
			// A single quote mark, often a plural possessive ("the tools' commit").
			continue
		}
		value := quoted || tok.Text == prose.CodeToken || tok.Text == prose.URLToken ||
			strings.ContainsFunc(tok.Text, unicode.IsDigit)
		switch tok.Tag {
		case "CD", "NNP", "NNPS", "$", "#":
			value = true
		}
		words = append(words, word{text: tok.Text, tag: tok.Tag, value: value})
	}
	for len(words) > 0 && words[len(words)-1].tag == "." {
		words = words[:len(words)-1]
	}
	p := parser{words: words, opts: opts, finite: -1}
	return p.best()
}

// parser reads one sentence's tagged words.
type parser struct {
	words []word
	opts  Options
	// finite is 1 when the sentence holds a finite verb or a modal, 0 when not, -1 unread.
	finite int
}

func (p *parser) at(i int) string {
	if i < 0 || i >= len(p.words) {
		return ""
	}
	return p.words[i].tag
}

func (p *parser) lower(i int) string { return strings.ToLower(p.words[i].text) }

// best is the strongest clause of the sentence that has a candidate's shape.
func (p *parser) best() (match, bool) {
	var (
		top   match
		found bool
	)
	for i := 0; i < len(p.words); {
		main, next, aux := p.verbGroup(i)
		if main >= 0 {
			if m, ok := p.complement(i, main, aux); ok && (!found || m.strength > top.strength) {
				top, found = m, true
			}
		}
		i = next
	}
	return top, found
}

// verbGroup reads the verb group that starts at word i: its main verb (-1 when none starts
// there, or the main verb is excluded), the index after the group, and whether an auxiliary or
// a modal comes before the main verb.
func (p *parser) verbGroup(i int) (main, next int, aux bool) {
	tag := p.at(i)
	if p.words[i].value {
		// A code token, a number or a proper noun the tagger reads as a verb is a value.
		return -1, i + 1, false
	}
	switch {
	case tag == "MD", tag == "VBZ", tag == "VBD", tag == "VBP":
	case tag == "VB" && (p.subjectBefore(i) || p.at(p.prevNonAdverb(i)) == "TO"):
	case tag == "VBN" && p.pastAfterSubject(i):
	case (isNoun(tag) || isAdjective(tag)) && p.misreadVerb(i):
	default:
		return -1, i + 1, false
	}
	if !p.anySubject(i) {
		return -1, i + 1, false
	}
	last := i
	for j := i + 1; j < len(p.words) && isVerbish(tag); j++ {
		switch t := p.at(j); {
		case isAdverb(t):
		case (t == "VB" || t == "VBN" || t == "VBG") && !p.words[j].value:
			last = j
		default:
			j = len(p.words)
		}
	}
	next = last + 1
	if p.excluded(last) {
		return -1, next, false
	}
	return last, next, last > i
}

// excluded is whether word i, read as the main verb, is a modal or a form of be, have or do.
func (p *parser) excluded(i int) bool {
	if p.at(i) == "MD" {
		return true
	}
	if p.opts.NoLists {
		return false
	}
	w := p.lower(i)
	return auxiliaries[w] || modals[w]
}

// subjectBefore is whether word i (a base form) follows its subject's head, as the present
// tense of a plural subject does ("Owners name ...").
func (p *parser) subjectBefore(i int) bool {
	prev := p.prevNonAdverb(i)
	return prev >= 0 && p.isHead(prev)
}

// pastAfterSubject is whether word i, tagged VBN, is the finite past of a clause: it follows
// its subject's head and an object, a particle or a clause follows it ("The plan named the
// step"). A participle after a noun with no object ("the gates named above") is not.
func (p *parser) pastAfterSubject(i int) bool {
	prev := p.prevNonAdverb(i)
	if prev < 0 || !p.isHead(prev) {
		return false
	}
	switch p.at(i + 1) {
	case "DT", "PDT", "PRP", "PRP$", "RP", "WP", "WDT", "WRB", "NN", "NNS", "JJ":
		return true
	case "IN", "TO", "":
		// With no finite verb in the sentence the participle is its verb ("This branch paid
		// for itself").
		if !p.hasFinite() {
			return true
		}
	}
	return p.gap(i)
}

// misreadVerb is whether word i, tagged as a noun or an adjective, is a verb the tagger
// misread. It must follow its subject's head and either the start of an object or a particle
// follows it, or (in a sentence with no finite verb) a preposition, an adjective or a plural
// noun follows it, or it ends a relative clause (gap). A word ending in -s straight after a
// conjunction or a comma continues the verb phrase of the clause before it when an object
// follows it ("..., and names the limit").
func (p *parser) misreadVerb(i int) bool {
	w := p.lower(i)
	if i == 0 || isAdjective(p.at(i)) && !strings.HasSuffix(w, "ed") {
		return false
	}
	if isAdverb(p.at(i - 1)) {
		// "the line this change no longer carries"
		return p.gap(i)
	}
	switch prev := p.at(i - 1); {
	case prev == "CD":
		// A number before a plural noun counts it ("500 requests"); before a base form it
		// can be the subject ("most name no project").
		if strings.HasSuffix(w, "s") {
			return false
		}
	case p.isHead(i - 1):
	case prev == "DT" && (i == 1 || isBoundary(p.at(i-2))), isAdjective(prev) && i > 1 && isDeterminer(p.at(i-2)):
		// A determiner that opens its clause stands as a pronoun ("; neither shows that ...");
		// an adjective after a determiner stands as the noun the tagger missed ("whose
		// directory names no user"). An object or a clause must follow.
		if !isNoun(p.at(i)) || !strings.HasSuffix(w, "s") {
			return false
		}
		switch p.at(i + 1) {
		case "DT", "PDT", "PRP", "PRP$", "WDT", "WP", "WRB":
			return true
		}
		return false
	case prev == "CC" || prev == ",":
		if !isNoun(p.at(i)) || !strings.HasSuffix(w, "s") || !p.anySubject(i-1) {
			return false
		}
		switch p.at(i + 1) {
		case "DT", "PDT", "PRP", "PRP$":
			return true
		}
		return false
	default:
		return false
	}
	switch p.at(i + 1) {
	case "DT", "PDT", "PRP", "PRP$", "RP":
		return isNoun(p.at(i))
	case "IN", "TO", "JJ", "NNS":
		if isNoun(p.at(i)) && !p.hasFinite() {
			return true
		}
	}
	return p.gap(i)
}

// gap is whether word i ends a relative clause whose object is the noun before that clause:
// a word ending in -s or -ed closes a noun run that opens with a determiner, a pronoun or a
// code token straight after another noun ("the head this issue names", "the lines it names",
// "the module CODE names"), and the clause ends there or a preposition, an adverb, a
// conjunction or the main clause's verb follows.
func (p *parser) gap(i int) bool {
	w := p.lower(i)
	if !strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ed") {
		return false
	}
	switch t := p.at(i + 1); {
	case i+1 >= len(p.words), isBoundary(t), t == "IN", t == "TO", t == "CC", isAdverb(t), isVerbish(t):
	default:
		return false
	}
	return p.relativeSubject(i)
}

// relativeSubject is whether the words before word i (skipping adverbs) are the subject of a
// relative clause that follows the noun it modifies: a noun phrase with no number in it that
// opens with a determiner, a pronoun or a value word straight after a noun or a relative
// pronoun.
func (p *parser) relativeSubject(i int) bool {
	for s := p.prevNonAdverb(i); s > 0 && (isNominal(p.at(s)) || p.words[s].value); s-- {
		switch t := p.at(s); {
		case t == "CD":
			// "every 30 seconds" is a time, not a clause.
			return false
		case t == "DT", t == "PDT", t == "PRP", t == "PRP$", p.words[s].value:
			if p.isHead(s-1) || p.at(s-1) == "WDT" || p.at(s-1) == "WP" {
				return true
			}
		}
	}
	return false
}

// clauseOpen is whether the words before the verb group at start are the subject noun phrase
// of a clause that opens there: before the noun phrase comes the start of the sentence, a
// comma, a colon, a bracket, a conjunction, a preposition (a subordinator), a wh-word or an
// adverb.
func (p *parser) clauseOpen(start int) bool {
	k := p.prevNonAdverb(start)
	headed := false
	for ; k >= 0; k-- {
		t := p.at(k)
		if !isNominal(t) && !p.words[k].value && t != "CC" {
			break
		}
		headed = headed || p.isHead(k) || t == "DT"
	}
	if !headed {
		return false
	}
	switch t := p.at(k); {
	case k < 0, isBoundary(t), isAdverb(t), t == "IN", t == "CC", t == "WRB", t == "WDT", t == "WP", t == "UH", t == "-LRB-":
		return true
	}
	return false
}

// complement reads the complement of the main verb of the group that starts at start, and
// answers the clause's match when it is a candidate's.
func (p *parser) complement(start, main int, aux bool) (match, bool) {
	j := p.nextNonAdverb(main)
	t := p.at(j)
	var (
		reason string
		end    int
	)
	switch {
	case j >= len(p.words) || isBoundary(t) || t == "CC" || isVerbish(t):
		switch {
		case p.relativeSubject(start):
			// The object is the noun the relative clause modifies; a conjunction or the
			// main clause's verb may follow it ("the surfaces this check names failed").
			reason = ReasonGap
		case j < len(p.words) && !isBoundary(t):
			return match{}, false
		case aux && p.at(main) == "VBN":
			reason = ReasonPassive
		case p.wholeClause(start) && j >= len(p.words), isVerbish(p.at(main)) && p.clauseOpen(start):
			// The clause is its subject and its verb: "That pays.", "until the fix lands,".
			reason = ReasonBare
		default:
			return match{}, false
		}
		end = j
	case t == "RP":
		reason = ReasonParticle
		end = p.object(j + 1)
	case t == "WDT" || t == "WP" || t == "WRB" || p.subordinate(j):
		reason = ReasonClause
		end = j + 1
		for end < len(p.words) && p.at(end) != ":" && p.words[end].text != ";" {
			end++
		}
	case t == "IN" || t == "TO":
		end = p.object(j + 1)
		switch next := p.at(j + 1); {
		case end > j+1:
			reason = ReasonPrep
		case j+1 >= len(p.words) || isBoundary(next) || next == "IN":
			// A stranded preposition: "the queue it waits on", "what they pay for with".
			reason = ReasonParticle
		case aux && p.at(main) == "VBN":
			// A passive with an infinitive after it: "we are not shown to be".
			reason, end = ReasonPassive, j
		default:
			// An infinitive or a gerund: its own verb group is read on its own.
			return match{}, false
		}
	case isNominal(t) || p.words[j].value:
		reason = ReasonObject
		end = p.object(j)
		if p.at(end) == ":" && p.listAfter(end) {
			// A list after the object states its values.
			return match{}, false
		}
	default:
		return match{}, false
	}
	if p.hasValue(j, end) {
		return match{}, false
	}
	return match{reason: reason, strength: p.strength(reason, start, j, end)}, true
}

// object is the end of the noun phrase that starts at i with the prepositional phrases that
// hang off it ("the owner of the queue"), or i when none starts there.
func (p *parser) object(i int) int {
	end := p.nounPhrase(i)
	if end == i {
		return i
	}
	for p.at(end) == "IN" || p.at(end) == "TO" {
		next := p.nounPhrase(end + 1)
		if next == end+1 {
			break
		}
		end = next
	}
	return end
}

// nounPhrase is the end of the noun phrase that starts at i, or i when none does. A gerund or
// participle stands in one only as an adjective before a noun or an adjective ("the merged
// change"); a value word stands in one as any other word does.
func (p *parser) nounPhrase(i int) int {
	for ; i < len(p.words); i++ {
		tag := p.at(i)
		switch {
		case isNominal(tag), p.words[i].value && !isVerbish(tag) && tag != "IN":
		case (tag == "VBG" || tag == "VBN") && (isNoun(p.at(i+1)) || isAdjective(p.at(i+1))):
		default:
			return i
		}
	}
	return i
}

// subordinate is whether word i, a preposition, opens a clause: a noun phrase and a finite
// verb or a modal follow it ("shows that the design works").
func (p *parser) subordinate(i int) bool {
	if p.at(i) != "IN" {
		return false
	}
	end := p.nounPhrase(i + 1)
	if end == i+1 {
		return false
	}
	switch p.at(p.nextNonAdverb(end - 1)) {
	case "VBZ", "VBD", "VBP", "MD":
		return true
	}
	return false
}

// hasValue is whether words [from, to) state a value. A proper noun states one only as the
// head of its noun phrase; a proper noun before a common noun modifies it ("the Envoy skill").
// A number spelled in letters that stands alone, with no noun after it, stands for a noun said
// elsewhere ("an image that still pays one") and counts nothing.
func (p *parser) hasValue(from, to int) bool {
	for i := from; i < to; i++ {
		if !p.words[i].value {
			continue
		}
		next := p.at(i + 1)
		if i+1 >= to {
			next = ""
		}
		switch p.at(i) {
		case "NNP", "NNPS":
			if isNoun(next) || next == "NNP" || next == "NNPS" {
				continue
			}
		case "CD":
			if !isNoun(next) && !isAdjective(next) && !strings.ContainsFunc(p.words[i].text, unicode.IsDigit) {
				continue
			}
		}
		return true
	}
	return false
}

// listAfter is whether the words after the colon at i, up to a semicolon, are a list or a
// value: they hold a comma, a conjunction or a value word.
func (p *parser) listAfter(i int) bool {
	for k := i + 1; k < len(p.words) && p.words[k].text != ";"; k++ {
		if p.words[k].value || p.at(k) == "," || p.at(k) == "CC" {
			return true
		}
	}
	return false
}

// wholeClause is whether the words before the verb group at start are only a subject noun
// phrase, after adverbs and conjunctions: the sentence is the subject and the verb. A verb the
// tagger read as a noun lies inside the subject's noun run, so the run may reach past start.
func (p *parser) wholeClause(start int) bool {
	i := 0
	for t := p.at(i); isAdverb(t) || t == "CC" || t == "UH" || t == ","; t = p.at(i) {
		i++
	}
	return start > i && p.object(i) >= start
}

// strength ranks a clause: the complement's shape (an object, a particle or a clause first),
// then a complement of at most three words, which holds the least that could state the value,
// then a clause that is the whole sentence.
func (p *parser) strength(reason string, start, from, to int) int {
	s := 1
	switch reason {
	case ReasonParticle, ReasonObject, ReasonClause:
		s = 3
	case ReasonPrep, ReasonGap:
		s = 2
	}
	if to-from <= 3 {
		s += 2
	}
	if p.wholeClause(start) {
		s += 2
	}
	return s
}

// hasFinite is whether the sentence holds a finite verb or a modal anywhere.
func (p *parser) hasFinite() bool {
	if p.finite < 0 {
		p.finite = 0
		for _, w := range p.words {
			switch w.tag {
			case "VBZ", "VBD", "VBP", "MD":
				p.finite = 1
			}
		}
	}
	return p.finite == 1
}

// anySubject is whether a word before word i can be a subject: a noun, a pronoun, a
// determiner, a relative pronoun or a value word such as a code token.
func (p *parser) anySubject(i int) bool {
	for k := range i {
		if p.isHead(k) || p.at(k) == "DT" || p.at(k) == "WDT" || p.at(k) == "WP" {
			return true
		}
	}
	return false
}

// isHead is whether word i can head a subject: a noun, a pronoun, or a value word that is not
// a verb, a preposition or a sign.
func (p *parser) isHead(i int) bool {
	switch t := p.at(i); {
	case isNoun(t), t == "PRP", t == "NNP", t == "NNPS", t == "CD":
		return true
	case p.words[i].value:
		return !isVerbish(t) && t != "IN" && t != "$" && t != "#" && !isBoundary(t)
	}
	return false
}

func (p *parser) prevNonAdverb(i int) int {
	i--
	for i >= 0 && isAdverb(p.at(i)) {
		i--
	}
	return i
}

func (p *parser) nextNonAdverb(i int) int {
	i++
	for i < len(p.words) && isAdverb(p.at(i)) {
		i++
	}
	return i
}

// isBoundary is whether a tag ends a clause: a comma, a dash, a semicolon, a colon or a
// closing bracket.
func isBoundary(tag string) bool {
	switch tag {
	case ",", ":", ".", "-RRB-", ")":
		return true
	}
	return false
}

func isVerbish(tag string) bool {
	switch tag {
	case "VB", "VBZ", "VBD", "VBP", "VBN", "VBG", "MD":
		return true
	}
	return false
}

func isNominal(tag string) bool {
	switch tag {
	case "DT", "PDT", "PRP", "PRP$", "POS", "NN", "NNS":
		return true
	}
	return isAdjective(tag)
}

func isNoun(tag string) bool      { return tag == "NN" || tag == "NNS" }
func isAdjective(tag string) bool { return tag == "JJ" || tag == "JJR" || tag == "JJS" }
func isAdverb(tag string) bool    { return tag == "RB" || tag == "RBR" || tag == "RBS" }

func isDeterminer(tag string) bool { return tag == "DT" || tag == "PRP$" || tag == "WP$" }
