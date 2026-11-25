package specform

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specform/charge"
	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// wholeTestLines is the rows of the tests of the whole text, then each line of ste-lint's corpus.
// The rows hold what joins or splits a piece of ste-lint across two sentences, and what moves the
// first place of a verb: a stop before a closing quote or bracket, an initialism, code font that
// holds a stop and a space, a verb of a group in code font and in a name, and list items. The rows
// after them hold each half of what the judge reads of a piece that crosses a changed sentence
// (wholeText.load, changedRuns, writesNoNewFinding), each where a count one word off, or the other
// half alone, changes the verdict: the two ways a piece runs on into a sentence from the one before
// it, from an open sentence of one piece and of two (left: the initialism ends a piece of ste-lint
// and no sentence), with a closed sentence as the control, and a way out that brings the last piece
// of a sentence of two pieces one word past the bound of 25, and to the bound where a piece runs on
// into that sentence too; a closed sentence that a way out opens before an open sentence of two
// pieces, whose piece runs on into that sentence's first piece and ends there, short of the bound,
// and past it if it ran on into the long sentence after (after: the initialism ends the piece and
// no sentence); one sentence written twice with an open sentence between, whose closing quote a way
// out takes away at both places, and a closed one that a way out opens at both places (a changed
// sentence's run, as the text holds it and as the way out writes it); and a sentence of one word
// that a way out writes in code font, open with no word in its last piece, before a long piece,
// which it then runs on into (the piece after a run, as the way out writes it).
func wholeTestLines(t *testing.T) []string {
	t.Helper()
	lines := []string{
		`The guide says "Run it." The job copies the report to deploy@build.example.com: each night after the tests of the merge pass on main.`,
		`The guide says (run it.) Then the job copies each report of the night to the host of the build and keeps a copy of it for a week or two.`,
		`Dispatch keeps it in the U.S. Then the job copies each report of the night to the host of the build and keeps a copy for a week or two at most.`,
		"Dispatch reads `one. two` and Web.Config at boot. It logs each read of the file and each write of the file for the team of the night build today.",
		"Dispatch checks Foo.Validate at boot. It validated the file.",
		"Dispatch checks `Foo.Validate` at boot. It validated the file. It confirms each record and verifies it.",
		"- Dispatch reads Web.Config. It keeps the record and",
		"1. Dispatch keeps the record. It checks it or",
		`He said "Stop." "Go." "Wait." Then each of the twenty words of this long piece runs on and on past every closing quote of the line today.`,
		`He said "Stop." "Go." Then the job copies each report of the night build to ada@example.com and keeps a copy for a week.`,
		`The job copies each report of the night build to ada@example.com and says "Done." "Ok." Then it stops for the rest of the night.`,
		`He said "Send it to ada@example.com." Then the job copies the log to ada@example.com each night after the build ends.`,
		`It runs in the U.S. and it said "wa wb wc wd we wf wg wh wi wj wk wl wm wn wo wp." It keeps it. It ends.`,
		`He said "wa wb wc wd we wf wg wh wi wj wk wl wm wn wo wp wq wr." It keeps it. It ends.`,
		`He said wa wb wc wd we wf wg wh wi wj wk wl wm wn wo wp wq wr. It keeps it. It ends.`,
		`It runs in the U.S. and it said "wa wb wc wd we wf wg wh wi wj wk wl wm wn." It keeps it. It ends.`,
		`He said "Go." It runs in the U.S. and it said "wa wb wc wd we wf wg wh wi wj wk wl wm." It keeps it. It ends.`,
		`He said "sa sb sc sd se sf sg sh si last." He said "ma mb mc md me mf." He said "sa sb sc sd se sf sg sh si last." It ends.`,
		`It logs. He said "wa wb wc wd we wf wg wh wi wj wk wl wm wn wo wp wq wr." It logs. It ends.`,
		`Go. Then each of the twenty-six words of this long sentence runs on and on past the bound that the rule sets for each sentence of a text today.`,
		`It logs. It runs in the U.S. and it said "wa wb." Then xa xb xc xd xe xf xg xh xi xj xk xl xm xn xo xp xq now.`,
	}
	corpus, err := os.Open("ste/testdata/corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	scanner := bufio.NewScanner(corpus)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line string
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// The judge reads the rules that read more than one sentence from what it reads once of each
// sentence (wholeText), and that reading is ste-lint's reading of the whole line (lintedWhole): for
// each line of ste-lint's corpus and of the rows (wholeTestLines) that ste-lint reads as one line,
// the findings of synonym-rotation and long-sentence that wholeText reads, its verbs and whether it
// is a list item that ends in "and" or "or" are ste-lint's. A line that ste-lint reads as more than
// one, as the corpus's lines with \v or U+2028 are, the judge reads whole (wholeText.lines).
func TestTheWholeTextReadsAsSteLintReadsTheLine(t *testing.T) {
	lines := wholeTestLines(t)
	read, joined, several := 0, 0, 0
	for _, text := range lines {
		line := oneLine(text)
		whole := newWholeText(line, nil, nil, newTexts(nil))
		if len(whole.units) == 0 {
			continue
		}
		if whole.lines {
			several++
			continue
		}
		if !whole.load() {
			t.Fatalf("%q: the charge refuses the judgment's one reading of each sentence", line)
		}
		read++
		want, verbs := lintedWhole(line, ste.Held{})
		dangles := false
		for key := range want {
			if key.rule == "ste100-dangling-conjunction" {
				dangles = true
				delete(want, key)
			}
		}
		if !maps.Equal(whole.before, want) || whole.verbs != verbs || whole.dangles != dangles {
			t.Errorf("%q: wholeText reads %v, verbs %b and dangles %v; ste-lint reads %v, verbs %b and dangles %v",
				line, whole.before, whole.verbs, whole.dangles, want, verbs, dangles)
		}
		sentences := sentencesOf(line)
		for _, each := range sentences[:len(sentences)-1] {
			if ste.SpreadOf(line[each.start:each.end]).Open {
				joined++
			}
		}
	}
	t.Logf("read %d lines by their sentences, with %d sentences that a piece runs on from, and %d whole", read, joined, several)
	if read < 700 || joined == 0 || several == 0 {
		t.Errorf("read %d lines by their sentences, %d sentences that a piece runs on from and %d lines whole: the test reads too little", read, joined, several)
	}
}

// The judge reads a change of one sentence in the whole text that it writes as ste-lint reads that
// text (wholeText.passes against lintedWhole of the written line), both ways: no change passes
// that ste-lint gives a new finding of synonym-rotation, long-sentence or dangling-conjunction, and
// none is refused that ste-lint gives none. The changes are of one word of a sentence, each at
// every place of the word in that sentence: the word in code font, six words after it, another
// verb of a synonym group, and, at a stop, a closing quote after it, the stop taken away or the
// closing marks taken away. They end a piece of ste-lint where it ran on into the next sentence,
// make one run on where it ended, and move a verb's first place, so the count of each piece after
// the change, from the sentences before it (left) and into those after it (runs), is read for each
// way a piece can cross the changed sentence, each where a count one word off, or one half of what
// the judge reads alone, changes the verdict (counted): after an open sentence of two pieces, whose
// last piece runs on into it, where the way out brings that piece one word past the bound of 25,
// and to the bound where a piece runs on into the sentence of two pieces too; before an open
// sentence of two pieces, where the way out leaves the sentence open, so that its last piece runs
// on into that sentence's first piece and ends there, short of the bound, and would pass it if it
// ran on into the sentence after that too; in a sentence written twice that a run of open sentences
// joins, where the way out closes it at both places, and in one that the way out opens at both
// places, so that only the text it writes joins them, into a piece that ste-lint refuses and
// neither place alone gives; and in a closed sentence that the way out leaves open with no word in
// its last piece, before a long piece that it then runs on into. Each line is one that the judge
// reads by its sentences, with no backtick that nothing closes before a sentence (opened), which
// the judge refuses a backtick after on purpose. Each change is read by a reading of its own, as a
// judgment reads one finding's ways out, so the charge of a sentence (charge.Texts.Read) holds
// each. The text that ste-lint reads is written from the line's own split (sentencesOf), not from
// what the judge keeps.
func TestTheWholeTextReadsAChangeOfASentenceAsSteLintReadsTheTextItWrites(t *testing.T) {
	verbs := []string{"checks", "verifies", "validated", "confirm", "deletes", "removed", "starts", "begin", "halted", "display"}
	texts, ways, opened, closed, refused, afterTwo, joinedTwice := 0, 0, 0, 0, 0, 0, 0
	atBound, pastBound, leftAfterLeft, openedTwice, emptyOpened, intoTwo := 0, 0, 0, 0, 0, 0
	long := func(words int) bool {
		_, breaks := ste.LongSentence(words)
		return breaks
	}
	for _, text := range wholeTestLines(t) {
		line := oneLine(text)
		sentences := sentencesOf(line)
		if len(sentences) == 0 || ste.Lines(line) > 1 || strings.Count(line, "`")%2 == 1 {
			continue
		}
		texts++
		before, _ := lintedWhole(line, ste.Held{})
		spreads, standsAt := make([]ste.Spread, len(sentences)), map[string][]int{}
		for index, each := range sentences {
			span := line[each.start:each.end]
			spreads[index] = ste.SpreadOf(span)
			standsAt[ste.Strip(span)] = append(standsAt[ste.Strip(span)], index)
		}
		for index, each := range sentences {
			// leftOfTwo is whether the sentence before this one is open and of two pieces or more;
			// joined whether this sentence stands at two places that a run of open sentences joins.
			leftOfTwo := index > 0 && spreads[index-1].Open && len(spreads[index-1].Pieces) > 1
			joined, places := false, standsAt[ste.Strip(line[each.start:each.end])]
			for at := 1; at < len(places); at++ {
				run := true
				for between := places[at-1]; between < places[at]; between++ {
					run = run && spreads[between].Open
				}
				joined = joined || run
			}
			place := each.in(line)
			pieces := ste.Words(place)
			if len(pieces) == 0 {
				continue
			}
			seen := map[wayOut]bool{}
			words := pieces[0]
			for at, word := range words {
				var candidates []wayOut
				if at < 5 || at == len(words)-1 {
					candidates = append(candidates, wayOut{word.Text, "`" + word.Text + "`"}, wayOut{word.Text, word.Text + " x x x x x x"},
						wayOut{word.Text, verbs[(len(word.Text)+at)%len(verbs)]})
				}
				if trimmed := strings.TrimRight(word.Text, closingMarks); trimmed != word.Text {
					candidates = append(candidates, wayOut{word.Text, trimmed})
				} else if strings.HasSuffix(word.Text, ".") {
					candidates = append(candidates, wayOut{word.Text, word.Text + `"`}, wayOut{word.Text, strings.TrimSuffix(word.Text, ".")})
				}
				for _, way := range candidates {
					if seen[way] || strings.Contains(way.from, "`") {
						continue
					}
					seen[way] = true
					// The judge keeps its reading of a way out by its finding and the way out, so the
					// finding names the sentence: the same change in two sentences is two readings.
					finding := wayKey{rule: Rule(fmt.Sprintf("probe of sentence %d", index)), from: way.from}
					judgment := newTexts(nil)
					judged := newWholeText(line, nil, nil, judgment)
					passed, note := judged.passes(finding, way, []charge.Unit{judgment.Name(place, false, false)}, nil)
					if note != "" {
						t.Fatalf("%q: the judge did not read %q to %q: %s", line, way.from, way.to, note)
					}
					// writtenAt is the line with the way out at each place of the sentence, or at the
					// place only (a sentence index) where only is not -1.
					writtenAt := func(only int) string {
						var written strings.Builder
						for which, other := range sentences {
							span := line[other.start:other.end]
							if ste.Strip(span) == place && (only < 0 || which == only) {
								span = strings.ReplaceAll(span, way.from, way.to)
							}
							written.WriteString(span)
						}
						return written.String()
					}
					after, _ := lintedWhole(writtenAt(-1), ste.Held{})
					full := noneNew(after, before)
					if passed != full {
						t.Errorf("%q: %q to %q in sentence %d: the judge says %v, and ste-lint over the written text says %v (%v, was %v)",
							line, way.from, way.to, index, passed, full, after, before)
					}
					ways++
					if !passed {
						refused++
					}
					if leftOfTwo {
						afterTwo++
					}
					if joined {
						joinedTwice++
					}
					span := line[each.start:each.end]
					now := ste.SpreadOf(strings.ReplaceAll(span, way.from, way.to))
					switch was := spreads[index].Open; {
					case was && !now.Open:
						closed++
					case !was && now.Open:
						opened++
						// The way out joins two places of the sentence that the text does not, each
						// sentence between them open, and ste-lint gives a new finding of the text it
						// writes at both, and none of the text it writes at either alone.
						for at := 1; at < len(places) && !full; at++ {
							run := true
							for between := places[at-1] + 1; between < places[at]; between++ {
								run = run && spreads[between].Open
							}
							first, _ := lintedWhole(writtenAt(places[at-1]), ste.Held{})
							second, _ := lintedWhole(writtenAt(places[at]), ste.Held{})
							if run && noneNew(first, before) && noneNew(second, before) {
								openedTwice++
								break
							}
						}
						// The way out leaves no word in the sentence's last piece, where no piece runs on
						// into the sentence, and the piece of the next sentence is long.
						if now.Pieces[len(now.Pieces)-1] == 0 && (index == 0 || !spreads[index-1].Open) && index+1 < len(sentences) && long(spreads[index+1].Pieces[0]) {
							emptyOpened++
						}
					}
					// The piece that runs on into the sentence from an open sentence of two pieces ends in
					// the sentence as the way out writes it, at the bound or one word past it.
					if leftOfTwo && (len(now.Pieces) > 1 || !now.Open) {
						piece := spreads[index-1].Pieces[len(spreads[index-1].Pieces)-1] + now.Pieces[0]
						switch {
						case !long(piece) && long(piece+1):
							atBound++
							if index > 1 && spreads[index-2].Open {
								leftAfterLeft++
							}
						case long(piece) && !long(piece-1):
							pastBound++
						}
					}
					// The way out leaves the sentence open before an open sentence of two pieces, so its
					// last piece runs on into that sentence's first piece and ends there: short of the
					// bound, and past it with the first piece of the sentence after that too.
					if now.Open && index+2 < len(sentences) && spreads[index+1].Open && len(spreads[index+1].Pieces) > 1 {
						piece := now.Pieces[len(now.Pieces)-1] + spreads[index+1].Pieces[0]
						if !long(piece) && long(piece+spreads[index+2].Pieces[0]) {
							intoTwo++
						}
					}
				}
			}
		}
	}
	t.Logf("%d lines, %d changes, %d refused, %d that end a piece that ran on, %d that make one run on, %d after an open sentence of two pieces (%d that end its piece at the bound, %d of them after a piece that runs on into it, and %d one past it), %d that run on into an open sentence of two pieces and end in it, %d in a sentence that a run joins at two places, %d that join two places into a finding that neither gives alone, %d that leave no word in an open last piece before a long one",
		texts, ways, refused, closed, opened, afterTwo, atBound, leftAfterLeft, pastBound, intoTwo, joinedTwice, openedTwice, emptyOpened)
	if texts < 700 || refused == 0 || refused == ways || closed == 0 || opened == 0 || afterTwo == 0 || joinedTwice == 0 ||
		atBound == 0 || leftAfterLeft == 0 || pastBound == 0 || intoTwo == 0 || openedTwice == 0 || emptyOpened == 0 {
		t.Errorf("%d lines, %d changes, %d refused, %d and %d that end and make a piece that runs on, %d after an open sentence of two pieces (%d, %d and %d at the bound, after a piece into it and past it), %d that run on into one and end in it, %d in a sentence that a run joins at two places, %d that join two, and %d that leave no word in an open last piece: the test reads too little",
			texts, ways, refused, closed, opened, afterTwo, atBound, leftAfterLeft, pastBound, intoTwo, joinedTwice, openedTwice, emptyOpened)
	}
}
