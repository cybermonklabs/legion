package specform

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/specform/charge"
)

// fixedReads is how many times a judgment of the ways out (wayJudge) reads each sentence of its
// text for no finding: once for the index of its words (wholeText.holding), and once for its
// Spread, or for the findings of the text read whole (wholeText.load). The split of the text into
// its sentences names each sentence (charge.Texts.Name) and reads none through the charge.
const fixedReads = 2

// ownReads is how many times at most a judgment reads one sentence for the findings that stand in
// it (charge.Texts.Read with own): fixedReads; once for what the sentence holds (wayJudge.unit);
// once for its findings with no stop of a finding left out (wayJudge.before with no stops), which
// each finding of a rule but stop-no-space reads; and, for each of the sentence's first
// maxJudgedInSentence findings, 2*maxWays: a finding of stop-no-space gives maxWays-1 ways out and
// reads three times for each, the sentence with the stops that the way out leaves (before), the
// way out alone (wayJudge.passes) and the whole text that the way out writes (wholeText.passes); a
// finding of any other rule gives maxWays ways out at most and reads twice for each, alone and in
// the whole text. The bound of a sentence's findings (maxJudgedInSentence) holds those readings, and
// no other walk reads a sentence for the findings that stand in it: the tests make a reading past
// ownReads stop the program (loudReads).
const ownReads = fixedReads + 1 + 1 + maxJudgedInSentence*2*maxWays

// maxReadsOfSentence is how many times at most a judgment reads one sentence of its text, for any
// purpose: ownReads for the findings that stand in it, and as many again for the findings that do
// not, where the finding's words stand in it (wholeText.holding), or where the whole text that a
// way out writes runs through it (wholeText.changedRuns), takes the first place of a member of a
// synonym group from it (wholeText.rotations) or is read whole (wholeText.writesNoNewFinding).
// That is what the number guards: a judgment reads a sentence's text, and what it keeps of each
// place of the sentence in the text (sentenceAt), only through the charge (charge.Texts.Read), so
// the work of a judgment is at most maxReadsOfSentence readings of each sentence, however many
// findings read one sentence, as when one sentence holds in code font the names that many other
// sentences end with (the work table's rows of one sentence that holds the words of many findings).
// The half for the findings that stand elsewhere is the half for the sentence's own findings, so
// that those findings never lose a way out to the others: their readings are counted apart (own).
const maxReadsOfSentence = 2 * ownReads

// elsewhereNote is what the detail of a finding says whose way out would read a sentence past its
// charge (charge.Texts.Read), so the writer knows that Dispatch offers no way out of it, not that
// none exists.
var elsewhereNote = fmt.Sprintf("Dispatch reads one sentence at most %d times for the ways out of findings that stand elsewhere, and a way out of this one would read a sentence past that, so it offers none for this one, though one can exist",
	ownReads)

// maxWayBytes is how many bytes the judgments of the ways out of the findings of one Check read
// together (charge.Budget), each reading the bytes of its text and charge.ReadingCost more. The
// bounds of each sentence keep that work in proportion to the text, at up to maxReadsOfSentence
// readings of each sentence; this keeps it within a bound that no document passes. A document of
// 1 MB of 7,527 statements, each with eight findings with ways out, of stops that no space follows,
// read 20.7 MB in 218,283 readings for their ways out, 23 s of CPU, where the rest of its judgment
// took 2.2 s. Under this budget its judgment takes 3 s, and its first 1,818 findings, more than the
// 500 that the refusal and the read route list, give their ways out (the api package's
// TestTheReadRouteJudgesADocumentOfOneMebibyteInBoundedWork).
const maxWayBytes = 1 << 20

// documentNote is what the detail of a finding says whose way out would read past the budget of
// its document (maxWayBytes), so the writer knows that Dispatch offers no way out of it, not that
// none exists.
var documentNote = fmt.Sprintf("Dispatch reads at most %d MiB of a document for ways out, so it offers none for this one, though one can exist",
	maxWayBytes>>20)

// refusedNote is what the detail of a finding says whose way out read a sentence that texts
// refused: documentNote where the budget of the document is spent (charge.Texts.Spent), and
// elsewhereNote where the charge of the sentence refused it.
func refusedNote(texts *charge.Texts[sentenceAt]) string {
	if texts.Spent() {
		return documentNote
	}
	return elsewhereNote
}

// loudReads is whether the charge stops the program at a reading of a sentence, for the findings
// that stand in it, that would pass ownReads: the bound of a sentence's findings holds those
// readings, so such a reading is a walk that breaks it. The tests of this package set it
// (reads_test.go), so that such a walk fails the suite at once. Where it is not set, the charge
// refuses such a reading, as it refuses any other past its charge.
var loudReads = false

// refuseReading, where the tests set it, is asked before each reading of a judgment's texts that
// the charge lets, and refuses each that it says yes to (charge.Texts.Refuse): the tests refuse each
// reading of a judgment in turn, so that each refusal is seen to end in the refusal of the way out
// that asked it (charge.Answer). It is nil outside the tests.
var refuseReading func() bool

// newTexts is the texts of one judgment of the ways out, each sentence's readings charged within
// ownReads for the findings that stand in it and as many for the others (charge.Texts), with what
// the whole text keeps of each place of a sentence (sentenceAt), and each reading's bytes taken
// from budget, the budget of the document that the text is part of (nil for none).
func newTexts(budget *charge.Budget) *charge.Texts[sentenceAt] {
	texts := charge.New[sentenceAt](ownReads, loudReads)
	texts.Refuse(refuseReading)
	texts.Share(budget)
	return texts
}
