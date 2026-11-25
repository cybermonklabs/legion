package daemon

import (
	"strings"
	"testing"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's refusedPreview (daemon/outbox.go at d46ec4e0) quoted "the first line of a body a notice
// quotes, cut to messageRefusedPreview", and its notice said the reason "begins %q". The change
// cuts the asker's words at 200 UTF-16 units and states its own rule for that cut in the record
// (texts.MessageRefusedRecord): "Begins holds the first line's head and nothing else, cut where it
// parts no extended grapheme cluster of Unicode's UAX #29 (a letter and the combining marks after
// it, ...)", since, as its own test says, a parted letter "quotes a character the asker did not
// write". The head's wholeGraphemes reads the clusters from github.com/rivo/uniseg v0.2.0, whose
// tables are Unicode 12's (March 2019): a combining mark that Unicode added since is no mark to it,
// so the cut parts it from its letter. The round's previous head backed off from every mark that
// Go's own tables know (unicode.IsMark), these among them.
//
// INFRA-3686's R1: a text that carries a worker's words carries "the first part, the count of the
// rest", in "a block of their own ... which names the author". Here a tester's backward move gives
// words whose 200th unit is a letter followed by a nonspacing mark that Go's unicode tables
// (unicode.Version) hold: the Telugu nukta U+0C3C and the Arabic small high word al-juz U+0898,
// each of Unicode 14, and the combining left parenthesis above left U+1AC1. The block under the
// tester's name ends with the letter and leaves out the mark the tester put on it.
func TestTheBeginsBlockKeepsEveryCombiningMarkWithItsLetter(t *testing.T) {
	for _, tc := range []struct {
		name         string
		letter, mark rune
	}{
		{name: "a Telugu letter and its nukta", letter: '\u0C15', mark: '\u0C3C'},
		{name: "an Arabic letter and a small high word al-juz", letter: '\u0628', mark: '\u0898'},
		{name: "a Latin letter and a combining parenthesis above", letter: 'e', mark: '\u1AC1'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !unicode.IsMark(tc.mark) {
				t.Fatalf("U+%04X is no mark to Go's unicode tables %s; this case proves nothing", tc.mark, unicode.Version)
			}
			words := strings.Repeat("a", 199) + string(tc.letter) + string(tc.mark) + " hangs on input"
			write := record.OrdinaryStatusWrite("in_progress", "testing", moveSentence(phase.Testing, phase.Implementing)+": "+words)
			got := refused(t, statusWriteRefused("LEGION-2", write, cause, statusWrites{status: true}))
			if got.Words["begins"].By != "tester" || got.RefusedMove == "" {
				t.Fatalf("begins by %q with refused_move %q, want the tester's words of a refused move; this case proves nothing", got.Words["begins"].By, got.RefusedMove)
			}
			want := strings.Repeat("a", 199)
			if got.Begins != want || got.Words["begins"].Rest != texts.Units(words)-texts.Units(want) {
				runes := []rune(got.Begins)
				t.Errorf("the tester's block holds %d units ending %q (rest %d): it ends with U+%04X and leaves out the mark U+%04X the tester put on it, "+
					"want the cut backed off to %d units (rest %d)",
					texts.Units(got.Begins), string(runes[max(0, len(runes)-3):]), got.Words["begins"].Rest, tc.letter, tc.mark,
					texts.Units(want), texts.Units(words)-texts.Units(want))
			}
		})
	}
}
