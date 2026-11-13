// Package candidates is the code stage of the writing check's detector: it picks, by grammar
// alone, the prose sentences that may hold a vacuous verb phrase, so the model reader reads only
// those.
//
// Each sentence is split into words by github.com/jdkato/prose/v3/tokenize and tagged with Penn
// Treebank tags by github.com/jdkato/prose/v3/tag. A sentence is a candidate when it is one
// clause of this shape and holds nothing else:
//
//  1. an optional opener: adverbs or conjunctions, or an introductory phrase that starts with a
//     preposition or an adverb and ends at a comma before any finite verb ("In short, ...");
//  2. a subject noun phrase (determiners, pronouns, adjectives, common nouns, possessives), with
//     the prepositional phrases that hang off it ("the owner of the queue");
//  3. a finite main verb (VBZ, VBD, VBP, or VB straight after the subject) that is not a modal
//     (tag MD) and not a form of be, have or do;
//  4. one of: an object noun phrase ("names the owner", reason svo-empty), a particle with an
//     optional object ("pays off", reason sv-particle), a preposition and its noun phrase
//     ("pays for itself", reason sv-prep), or nothing ("that pays", reason sv-bare);
//  5. after that, only more noun phrases, prepositional phrases, adverbs and conjunctions
//     joining noun phrases.
//
// A sentence holds a value, and so is never a candidate, when any word of it is a number (CD),
// holds a digit, is a proper noun (NNP, NNPS), is a currency or number sign, or is the code or
// address token of package prose. A second finite verb or a modal anywhere after the main verb
// is a second clause (a subordinate or relative clause, or a coordinated verb phrase) and ends
// the candidate, as does a wh-word (which, who, how, ...; it opens a clause even where the
// tagger misses that clause's verb), a non-finite verb that does not stand as an adjective
// before a noun, and any tag the shape above does not name (a colon, a bracket, a symbol, a
// foreign word).
//
// The grammar is tuned for recall. A prepositional phrase whose words are all common words
// states no value the grammar can see, so it keeps the sentence and the reader decides. The
// tagger reads some verbs as nouns ("The record names ..." tags names NNS): where a sentence
// holds no finite verb at all, the first noun of the opening noun run that follows a subject and
// is followed by a determiner, a pronoun, a particle or a preposition is read as the verb, and
// the sentence then needs that complement.
//
// The only word lists are exclusions: the forms of be, have and do, and the modals the tagger
// may miss. Options.NoLists drops both, which shows that what the grammar finds does not rest
// on them.
package candidates

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/jdkato/prose/v3/tag"
	"github.com/jdkato/prose/v3/tokenize"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// The reason codes a candidate carries: which shape of step 4 the sentence has.
const (
	ReasonObject   = "svo-empty"
	ReasonParticle = "sv-particle"
	ReasonPrep     = "sv-prep"
	ReasonBare     = "sv-bare"
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

// Find is the candidates among sentences, in their order. A sentence's context is drawn from
// the sentences next to it in the slice that carry the same Field.
func Find(sentences []prose.Sentence, opts Options) []Candidate {
	t := tagger()
	var found []Candidate
	for i, sentence := range sentences {
		reason, ok := shape(t, sentence.Text, opts)
		if !ok {
			continue
		}
		found = append(found, Candidate{
			ID:       fmt.Sprintf("c%d", len(found)+1),
			Sentence: sentence,
			Reason:   reason,
			Before:   around(sentences, i, -1),
			After:    around(sentences, i, +1),
		})
	}
	return found
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

// shape is the sentence's reason code when it has a candidate's shape. The values a word's text
// alone shows are looked for before the sentence is tagged, which is the stage's main cost.
func shape(t *tag.Tagger, text string, opts Options) (string, bool) {
	tokens := tokenize.ForLanguage("en").Tokenize(text)
	for _, tok := range tokens {
		if tok.Text == prose.CodeToken || tok.Text == prose.URLToken || strings.ContainsFunc(tok.Text, unicode.IsDigit) {
			return "", false
		}
	}
	t.TagTokens(tokens)
	words := tokens[:0]
	for _, tok := range tokens {
		switch tok.Tag {
		case "CD", "NNP", "NNPS", "$", "#":
			return "", false
		case "``", "''", "\"":
			// Quote marks carry no word of their own.
			continue
		}
		words = append(words, tok)
	}
	for len(words) > 0 && words[len(words)-1].Tag == "." {
		words = words[:len(words)-1]
	}
	p := parser{words: words, opts: opts}
	return p.parse()
}

// parser walks one sentence's tagged words left to right.
type parser struct {
	words []tag.Token
	pos   int
	opts  Options
}

func (p *parser) at(i int) string {
	if i < 0 || i >= len(p.words) {
		return ""
	}
	return p.words[i].Tag
}

func (p *parser) done() bool { return p.pos >= len(p.words) }

func (p *parser) parse() (string, bool) {
	p.opener()
	start := p.pos
	end := p.subject(start)
	if end == start || !headed(p.words[start:end]) {
		return p.misreadVerb(start)
	}
	verb := end
	for isAdverb(p.at(verb)) {
		verb++
	}
	if !p.finiteVerb(verb, verb == end) {
		return p.misreadVerb(start)
	}
	if p.excluded(verb) {
		return "", false
	}
	p.pos = verb + 1
	return p.rest(true)
}

// subject is the end of the subject noun phrase that starts at i, with the prepositional
// phrases that hang off it ("the owner of the queue"), or i when none starts there.
func (p *parser) subject(i int) int {
	end := p.nounPhrase(i)
	if end == i {
		return i
	}
	for p.at(end) == "IN" {
		next := p.nounPhrase(end + 1)
		if next == end+1 {
			break
		}
		end = next
	}
	return end
}

// misreadVerb reads a sentence that holds no finite verb at all as one whose verb the tagger
// took for a noun ("The record names ..." tags names NNS, "Owners name ..." tags name NN): the
// first noun of the opening noun run that follows a headed subject and is itself followed by
// the start of an object, a particle or a preposition. The sentence then needs that
// complement, since without one it is only a noun phrase.
func (p *parser) misreadVerb(start int) (string, bool) {
	if p.anyFiniteVerb() {
		return "", false
	}
	end := p.nounPhrase(start)
	for k := start + 1; k < end; k++ {
		if !isNoun(p.at(k)) || !headed(p.words[start:k]) {
			continue
		}
		switch p.at(k + 1) {
		case "DT", "PDT", "PRP", "PRP$", "IN", "TO", "RP":
			if p.excluded(k) {
				return "", false
			}
			p.pos = k + 1
			return p.rest(false)
		}
	}
	return "", false
}

// opener skips the words before the subject: adverbs, conjunctions and interjections, and an
// introductory phrase that starts with a preposition or an adverb and ends at a comma before
// any finite verb.
func (p *parser) opener() {
	if tag := p.at(p.pos); tag == "IN" || isAdverb(tag) {
		for i := p.pos; i < len(p.words); i++ {
			if p.at(i) == "," {
				p.pos = i + 1
				break
			}
			if p.finiteVerb(i, false) || p.at(i) == "MD" {
				break
			}
		}
	}
	for tag := p.at(p.pos); isAdverb(tag) || tag == "CC" || tag == "UH" || tag == ","; tag = p.at(p.pos) {
		p.pos++
	}
}

// nounPhrase is the end of the noun phrase that starts at i, or i when none does. A gerund or
// participle stands in one only as an adjective before a noun or an adjective ("the merged
// change").
func (p *parser) nounPhrase(i int) int {
	for ; i < len(p.words); i++ {
		tag := p.at(i)
		switch {
		case isNominal(tag):
		case (tag == "VBG" || tag == "VBN") && (isNoun(p.at(i+1)) || isAdjective(p.at(i+1))):
		default:
			return i
		}
	}
	return i
}

// rest reads the verb's complement (step 4) and what follows it (step 5). A verb the tagger
// read as a noun must have a complement: without one, the sentence is a noun phrase.
func (p *parser) rest(bareAllowed bool) (string, bool) {
	for isAdverb(p.at(p.pos)) {
		p.pos++
	}
	var reason string
	switch tag := p.at(p.pos); {
	case p.done():
		if !bareAllowed {
			return "", false
		}
		return ReasonBare, true
	case tag == "RP":
		reason = ReasonParticle
		p.pos++
		p.pos = p.nounPhrase(p.pos)
	case isNominal(tag):
		reason = ReasonObject
		p.pos = p.nounPhrase(p.pos)
	case tag == "IN" || tag == "TO":
		end := p.nounPhrase(p.pos + 1)
		if end == p.pos+1 {
			return "", false
		}
		reason = ReasonPrep
		p.pos = end
	default:
		return "", false
	}
	for ; !p.done(); p.pos++ {
		tag := p.at(p.pos)
		switch {
		case isNominal(tag), isAdverb(tag), tag == "IN", tag == "TO", tag == "RP", tag == "CC", tag == ",":
		case (tag == "VBG" || tag == "VBN") && (isNoun(p.at(p.pos+1)) || isAdjective(p.at(p.pos+1))):
		default:
			// A second verb, a modal, or a tag the shape does not name.
			return "", false
		}
	}
	return reason, true
}

// finiteVerb is whether word i is a finite verb. A base form counts only straight after the
// subject (afterSubject), where the tagger gives it to a plural subject's present tense.
func (p *parser) finiteVerb(i int, afterSubject bool) bool {
	switch p.at(i) {
	case "VBZ", "VBD", "VBP":
		return true
	case "VB":
		return afterSubject
	}
	return false
}

// anyFiniteVerb is whether the sentence holds a finite verb or a modal anywhere.
func (p *parser) anyFiniteVerb() bool {
	for i := range p.words {
		if p.finiteVerb(i, false) || p.at(i) == "MD" {
			return true
		}
	}
	return false
}

// excluded is whether word i, read as the main verb, is a modal or an auxiliary.
func (p *parser) excluded(i int) bool {
	if p.at(i) == "MD" {
		return true
	}
	if p.opts.NoLists {
		return false
	}
	w := strings.ToLower(p.words[i].Text)
	return auxiliaries[w] || modals[w]
}

// headed is whether words hold a head a subject can rest on: a noun, a pronoun or a determiner.
func headed(words []tag.Token) bool {
	for _, w := range words {
		if isNoun(w.Tag) || w.Tag == "PRP" || w.Tag == "DT" {
			return true
		}
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
