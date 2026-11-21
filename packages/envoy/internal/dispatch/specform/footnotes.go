package specform

import (
	"fmt"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// maxSentences is how many sentences a statement has at most (R49).
const maxSentences = 2

// holdsWords marks each footnote of notes that holds words, or refers to a footnote that does,
// directly or in turn (Footnote.words). Each footnote that holds words marks the footnotes that
// refer to it, and a marked one stops the walk, so each footnote is marked once.
func holdsWords(notes []*Footnote) {
	referrers := map[*Footnote][]*Footnote{}
	for _, note := range notes {
		for _, child := range note.Notes {
			referrers[child] = append(referrers[child], note)
		}
	}
	var marked []*Footnote
	for _, note := range notes {
		if note.Body != "" {
			marked = append(marked, note)
		}
	}
	for len(marked) > 0 {
		note := marked[len(marked)-1]
		marked = marked[:len(marked)-1]
		if note.words {
			continue
		}
		note.words = true
		marked = append(marked, referrers[note]...)
	}
}

// footnoteJudge judges the footnotes of the texts of one Check. It judges each footnote once
// (judgment, findings), and gives each of its findings once: the findings of the form rules to the
// first text that holds the footnote and whose kind those rules judge (prose), and the findings of
// the reference rules to the first text that holds it, so that the work and the findings of a
// judgment grow with the texts and their footnotes, not with their product. A text holds a
// footnote that it refers to, and each footnote that such a footnote refers to in turn. The words
// of each finding name the footnote and how many other texts refer to it (place). The sentences of
// each footnote that a statement holds count among the statement's (held), and so the ways out of
// its findings are judged in the largest statement that holds it (statements). Two verbs for one
// action are the one finding of the text or the footnote that holds them both with no one footnote
// that it refers to holding them all (held, rotations).
type footnoteJudge struct {
	c      Context
	people [][]string
	texts  []Text
	judged map[*Footnote]*footnoteJudgment
	// formGiven and referencesGiven are each footnote whose findings of the form rules, and of the
	// reference rules, a text already holds.
	formGiven, referencesGiven map[*Footnote]bool
	// referrers is how many of the texts refer to each footnote directly.
	referrers map[*Footnote]int
	// counted is, for each footnote that a text holds, the footnotes with sentences that it holds,
	// until their sentences pass maxSentences, and verbs the verbs of the synonym groups that it
	// and the footnotes that it holds use (carry). gives is the verbs of each synonym group that a
	// footnote rotates with the footnotes that it refers to (rotations), and rings and ring the
	// loops of the footnotes (footnoteRings).
	counted map[*Footnote]*sentenceCount
	verbs   map[*Footnote]ste.Verbs
	gives   map[*Footnote]ste.Verbs
	rings   []footnoteRing
	ring    map[*Footnote]int
	// inStatement is, for each footnote that a statement holds, the count of sentences of the
	// largest such statement (statements).
	inStatement map[*Footnote]int
	// The fields below are what rotationCanChange reads, each the first time that a way out asks:
	// the sets of a synonym group in each ring (ringCount) and below each text (textCount), the
	// verbs of each text's words, the texts of prose that refer to each ring, and the place of each
	// footnote that a text of prose holds (formPlace).
	ringCounts    map[ringGroup]ringCount
	textCounts    map[ringGroup]verbCount
	textVerbsRead map[int]ste.Verbs
	textsOfRing   [][]int
	formPlaces    map[*Footnote]string
}

// footnoteJudgment is one footnote judged: the findings of the form rules, the findings of the
// reference rules, the count of its sentences and the verbs of the synonym groups that it uses;
// the judge of the ways out of its findings, and whether it judged them (findings).
type footnoteJudgment struct {
	form, references []broken
	sentences        int
	verbs            ste.Verbs
	judge            *wayJudge
	judged           bool
}

// sentenceCount is the footnotes with sentences that one footnote holds, itself among them, and the
// sum of their sentences, which stops growing once it passes maxSentences: a statement that holds
// it then has more than maxSentences sentences, whatever the rest.
type sentenceCount struct {
	notes []*Footnote
	sum   int
}

func (count *sentenceCount) full() bool {
	return count.sum > maxSentences
}

// footnotesHeld is what the footnotes that a text holds add to it: for a statement, each footnote
// whose sentences count among its own, with how many it adds, their sum (count) and whether that
// sum is only the least that they hold (atLeast), because one footnote holds more than Dispatch
// counted; for a text of prose, the verbs of the synonym groups that each footnote that it refers
// to uses with the footnotes that it holds.
type footnotesHeld struct {
	adds    []sentencesAdded
	count   int
	atLeast bool
	verbs   []ste.Verbs
}

// sentencesAdded is a footnote, by its label, and how many sentences it adds to a statement.
type sentencesAdded struct {
	label     string
	sentences int
}

func newFootnoteJudge(c Context, people [][]string, texts []Text) *footnoteJudge {
	judge := &footnoteJudge{
		c: c, people: people, texts: texts,
		judged:          map[*Footnote]*footnoteJudgment{},
		formGiven:       map[*Footnote]bool{},
		referencesGiven: map[*Footnote]bool{},
		referrers:       map[*Footnote]int{},
	}
	for _, text := range texts {
		for _, note := range text.Notes {
			judge.referrers[note]++
		}
	}
	return judge
}

// judgment is note as the judge reads it, the first time that a text asks for it: the count of its
// sentences and the verbs that it uses, and each place of each finding, which the judge of its ways
// out takes first (survey), before it judges them (findings).
func (j *footnoteJudge) judgment(note *Footnote) *footnoteJudgment {
	if judged, ok := j.judged[note]; ok {
		return judged
	}
	judged := &footnoteJudgment{judge: newWayJudge(j.c, j.people, kindFootnote, note.Held.Shown != "")}
	judged.judge.elsewhere = func(before, after ste.Verbs) bool { return j.rotationCanChange(note, before, after) }
	judged.judge.survey(func() { j.find(note, judged) })
	j.judged[note] = judged
	return judged
}

// findings is note judged, with the ways out of its findings judged at each place of each, the
// first time that a text asks for them: in the largest statement that holds note (statements), with
// the statement's sentences in the count of the rule sentences, as the refusal counts them.
func (j *footnoteJudge) findings(note *Footnote) *footnoteJudgment {
	judged := j.judgment(note)
	if !judged.judged {
		if count, ok := j.statements()[note]; ok {
			judged.judge.heldIn(count)
		}
		j.find(note, judged)
		judged.judged = true
	}
	return judged
}

// find writes each finding of note into judged, of the form rules and of the reference rules, with
// the count of its sentences and its verbs, the ways out judged by judged.judge.
func (j *footnoteJudge) find(note *Footnote, judged *footnoteJudgment) {
	judged.form, judged.sentences, judged.verbs = formBreaks(kindFootnote, oneLine(note.Body), footnotesHeld{}, judged.judge)
	judged.references = heldBreaks(j.c, j.people, note.Body, note.Held, judged.judge)
}

// statements is, for each footnote that a statement holds, the count of sentences of the largest
// such statement, with those of its footnotes (held), up to maxSentences+1, past which a way out
// that adds a sentence makes a count that the statement does not give, as the one before it: one
// number for each footnote, which each statement that holds it raises, along each reference, at
// most maxSentences+2 times, so its work grows with the texts and the footnotes.
func (j *footnoteJudge) statements() map[*Footnote]int {
	if j.inStatement != nil {
		return j.inStatement
	}
	j.inStatement = map[*Footnote]int{}
	for _, text := range j.texts {
		if text.Kind != KindStatement || len(text.Notes) == 0 {
			continue
		}
		count := min(maxSentences+1, len(sentencesOf(oneLine(text.Body)))+j.held(text).count)
		walk := slices.Clone(text.Notes)
		for len(walk) > 0 {
			note := walk[len(walk)-1]
			walk = walk[:len(walk)-1]
			if held, ok := j.inStatement[note]; ok && held >= count {
				continue
			}
			j.inStatement[note] = count
			walk = append(walk, note.Notes...)
		}
	}
	return j.inStatement
}

// owned is the findings of each footnote that text is the first text to hold, for each of the two
// kinds of rule that judge text: the form rules for a text of prose, then the reference rules.
func (j *footnoteJudge) owned(text Text) []broken {
	var found []broken
	if prose(text.Kind) {
		j.walk(text, j.formGiven, func(via, note *Footnote) {
			found = append(found, j.placed(via, note, j.form(note))...)
		})
	}
	j.walk(text, j.referencesGiven, func(via, note *Footnote) {
		found = append(found, j.placed(via, note, j.findings(note).references)...)
	})
	return found
}

// form is the findings of the form rules of note: those of its words (findings), then each synonym
// group that it rotates with the footnotes that it refers to (rotations).
func (j *footnoteJudge) form(note *Footnote) []broken {
	if j.counted == nil {
		j.carry()
	}
	form := j.findings(note).form
	if gives := j.gives[note]; gives != 0 {
		return append(slices.Clip(form), rotationBreaks(gives, "the footnotes that it refers to")...)
	}
	return form
}

// walk calls visit for each footnote that text holds and given does not, in the order of the first
// reference, with the footnote that text refers to directly through which it holds it (via), and
// adds it to given. A footnote in given was walked whole by an earlier text, so the walk stops at
// it, and each footnote is walked once for each set.
func (j *footnoteJudge) walk(text Text, given map[*Footnote]bool, visit func(via, note *Footnote)) {
	type step struct{ via, note *Footnote }
	var queue []step
	for _, note := range text.Notes {
		queue = append(queue, step{note, note})
	}
	for index := 0; index < len(queue); index++ {
		each := queue[index]
		if given[each.note] {
			continue
		}
		given[each.note] = true
		visit(each.via, each.note)
		for _, child := range each.note.Notes {
			if !given[child] {
				queue = append(queue, step{each.via, child})
			}
		}
	}
}

// placed is found, the findings of note, with the place in front of each detail (placeWords).
func (j *footnoteJudge) placed(via, note *Footnote, found []broken) []broken {
	if len(found) == 0 {
		return nil
	}
	place := j.placeWords(via, note)
	placed := make([]broken, len(found))
	for index, each := range found {
		each.detail = place + ": " + each.detail
		placed[index] = each
	}
	return placed
}

// placeWords is the place of a finding of note: the footnote, the footnote that the text refers to
// when it holds note through it (via), and how many other texts refer to each.
func (j *footnoteJudge) placeWords(via, note *Footnote) string {
	place := fmt.Sprintf("in its footnote [^%s]%s", note.Label, others(j.referrers[note]-1, "it"))
	if via != note {
		place = fmt.Sprintf("in the footnote [^%s], which its footnote [^%s] refers to%s", note.Label, via.Label, others(j.referrers[via]-1, "[^"+via.Label+"]"))
		if direct := j.referrers[note]; direct > 0 {
			place += fmt.Sprintf(", and %s to [^%s]", textCount(direct), note.Label)
		}
	}
	return place
}

// others says how many other texts also refer to what, when any do.
func others(count int, what string) string {
	switch {
	case count <= 0:
		return ""
	case count == 1:
		return fmt.Sprintf(" (1 other text also refers to %s)", what)
	}
	return fmt.Sprintf(" (%d other texts also refer to %s)", count, what)
}

// textCount is count texts, in words.
func textCount(count int) string {
	if count == 1 {
		return "1 text refers"
	}
	return fmt.Sprintf("%d texts refer", count)
}

// held is what the footnotes that text holds add to it (footnotesHeld): for a statement, the
// footnotes with sentences of each footnote that it refers to (carry), each once; for a text of
// prose, the verbs that each footnote that it refers to holds (carry).
func (j *footnoteJudge) held(text Text) footnotesHeld {
	var noted footnotesHeld
	if !prose(text.Kind) || len(text.Notes) == 0 {
		return noted
	}
	if j.counted == nil {
		j.carry()
	}
	for _, note := range text.Notes {
		if verbs := j.verbs[note]; verbs != 0 {
			noted.verbs = append(noted.verbs, verbs)
		}
	}
	if text.Kind != KindStatement {
		return noted
	}
	var held []*Footnote
	for _, note := range text.Notes {
		count := j.counted[note]
		noted.atLeast = noted.atLeast || count.full()
		for _, each := range count.notes {
			if noted.count > maxSentences {
				noted.atLeast = true
				return noted
			}
			if !slices.Contains(held, each) {
				held = append(held, each)
				sentences := j.judgment(each).sentences
				noted.count += sentences
				noted.adds = append(noted.adds, sentencesAdded{label: each.Label, sentences: sentences})
			}
		}
	}
	return noted
}

// carry counts, for each footnote that a text holds, the footnotes with sentences that it holds
// (sentenceCount), and the verbs that it holds. Each footnote with sentences is added to itself and
// to each footnote that holds it, walking back along the references, and the walk stops at a
// footnote that has it already or whose count is full. A footnote whose count is full holds more
// than maxSentences sentences, and so does each footnote that refers to it. So each footnote takes
// at most maxSentences+1 footnotes. The verbs of each footnote are added to itself and to each
// footnote that holds it in the same way, and the walk stops at a footnote that holds them all
// already, so each footnote takes each verb once. So the work is in proportion to the footnotes
// and their references. Then it finds the rotations that each footnote gives (rotations).
func (j *footnoteJudge) carry() {
	j.counted, j.verbs = map[*Footnote]*sentenceCount{}, map[*Footnote]ste.Verbs{}
	var held, order []*Footnote
	for _, text := range j.texts {
		held = append(held, text.Notes...)
	}
	for index := 0; index < len(held); index++ {
		note := held[index]
		if j.counted[note] != nil {
			continue
		}
		j.counted[note] = &sentenceCount{}
		order = append(order, note)
		held = append(held, note.Notes...)
	}
	referrers := map[*Footnote][]*Footnote{}
	for _, note := range order {
		for _, child := range note.Notes {
			referrers[child] = append(referrers[child], note)
		}
	}
	for _, source := range order {
		judged := j.judgment(source)
		if verbs := judged.verbs; verbs != 0 {
			walk := []*Footnote{source}
			for len(walk) > 0 {
				note := walk[len(walk)-1]
				walk = walk[:len(walk)-1]
				if j.verbs[note]&verbs == verbs {
					continue
				}
				j.verbs[note] |= verbs
				walk = append(walk, referrers[note]...)
			}
		}
		sentences := judged.sentences
		if sentences == 0 {
			continue
		}
		walk := []*Footnote{source}
		for len(walk) > 0 {
			note := walk[len(walk)-1]
			walk = walk[:len(walk)-1]
			count := j.counted[note]
			if count.full() || slices.Contains(count.notes, source) {
				continue
			}
			count.notes, count.sum = append(count.notes, source), count.sum+sentences
			walk = append(walk, referrers[note]...)
		}
	}
	j.rotations(order)
}

// rotations finds, for each footnote that a text holds, the verbs of each synonym group that it
// breaks the rule synonym-rotation in with the footnotes that it refers to (gives), once carry has
// carried the verbs. It reads the footnotes as their rings (footnoteRings): footnotes that refer to
// each other in a loop hold each other, and so use the same verbs. A ring gives a group's finding
// where it and the footnotes that it holds use two verbs or more of the group together, and neither
// the words of one footnote of the ring, which are Lint's finding of that footnote, nor one
// footnote outside the ring that it refers to uses each of them (ste.Rotations). Such a footnote
// gives that finding itself, rotates them in its own words, or holds a ring that does, in turn. So
// one finding stands for each group however many footnotes hold its verbs, and the first footnote
// of the ring in the order of the texts gives it: for a footnote that is no loop, the footnote
// itself. A ring reads each reference to a footnote outside it once, so the work is in proportion
// to the footnotes and their references.
func (j *footnoteJudge) rotations(order []*Footnote) {
	j.rings, j.ring = footnoteRings(order)
	j.gives = map[*Footnote]ste.Verbs{}
	for _, ring := range j.rings {
		verbs := j.verbs[ring.notes[0]]
		rotated := verbs.Rotated()
		for _, note := range ring.notes {
			rotated &^= usesAll(verbs, j.judgment(note).verbs)
		}
		for _, below := range ring.below {
			rotated &^= usesAll(verbs, j.verbs[j.rings[below].notes[0]])
		}
		if rotated != 0 {
			j.gives[ring.notes[0]] = rotated
		}
	}
}
