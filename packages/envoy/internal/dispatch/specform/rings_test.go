package specform

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// ringCase is one judgment of the test of the verbs of footnotes: footnotes by their labels, each
// with its words and the labels of the footnotes that it refers to, and texts, each with its kind,
// its words and the labels of the footnotes that it refers to.
type ringCase struct {
	notes []ringNote
	texts []ringText
}

type ringNote struct {
	body string
	refs []int
}

type ringText struct {
	kind Kind
	body string
	refs []int
}

// build is the texts of the case, with a footnote of its own for each label, and the words of the
// footnote at changed written as to in place of its own (changed is -1 for none).
func (test ringCase) build(changed int, to string) ([]Text, []*Footnote) {
	notes := make([]*Footnote, len(test.notes))
	for index, note := range test.notes {
		notes[index] = &Footnote{Label: fmt.Sprint(index + 1), Body: note.body}
		if index == changed {
			notes[index].Body = to
		}
	}
	for index, note := range test.notes {
		for _, ref := range note.refs {
			notes[index].Notes = append(notes[index].Notes, notes[ref])
		}
	}
	texts := make([]Text, len(test.texts))
	for index, text := range test.texts {
		texts[index] = Text{Node: fmt.Sprintf("t%d", index), Kind: text.kind, Body: text.body, Where: where}
		for _, ref := range text.refs {
			texts[index].Notes = append(texts[index].Notes, notes[ref])
		}
	}
	return texts, notes
}

// String is the case for a message.
func (test ringCase) String() string {
	var said []string
	for index, text := range test.texts {
		said = append(said, fmt.Sprintf("text %d (%s) %q refers to %v", index, text.kind, text.body, text.refs))
	}
	for index, note := range test.notes {
		said = append(said, fmt.Sprintf("[^%d] %q refers to %v", index+1, note.body, note.refs))
	}
	return strings.Join(said, "; ")
}

// randomRingCase is a case of one to four footnotes and one to three texts, each of whose words use
// verbs of the group of "check" at random, and one footnote of which, at least, holds the name
// Foo.Validate, a stop with no space after it whose code font takes the verb "validate" away. Each
// footnote refers to each footnote, itself among them, at random, so footnotes refer to each other
// in loops; each text refers to each footnote at random, and the first to one at least.
func randomRingCase(random *rand.Rand) ringCase {
	phrases := []string{"checks the size", "verifies the type", "confirms the log", "removes the copy"}
	words := func(subject string, name bool) string {
		var used []string
		for _, phrase := range phrases {
			if random.IntN(3) == 0 {
				used = append(used, phrase)
			}
		}
		if name {
			used = append(used, "reads Foo.Validate")
		}
		if len(used) == 0 {
			used = []string{"keeps the note"}
		}
		return subject + " " + strings.Join(used, " and ") + " at boot."
	}
	var test ringCase
	count := 1 + random.IntN(4)
	for index := range count {
		note := ringNote{body: words("It", index == 0 || random.IntN(2) == 0)}
		for ref := range count {
			if random.IntN(3) == 0 {
				note.refs = append(note.refs, ref)
			}
		}
		test.notes = append(test.notes, note)
	}
	kinds := []Kind{KindClaim, KindStatement, KindProcedure, KindName}
	for index := range 1 + random.IntN(3) {
		text := ringText{kind: kinds[random.IntN(len(kinds))], body: words("Dispatch", false)}
		for ref := range count {
			if random.IntN(2) == 0 || index == 0 && ref == count-1 && len(text.refs) == 0 {
				text.refs = append(text.refs, ref)
			}
		}
		test.texts = append(test.texts, text)
	}
	return test
}

// rotationsWithFootnotes is each finding of the rule synonym-rotation that the verbs of footnotes
// give, of a footnote with those that it refers to or of a text with its footnotes, by its rule and
// its detail, with how many times Check gives it.
func rotationsWithFootnotes(texts []Text) map[findingKey]int {
	found := map[findingKey]int{}
	for _, each := range Check(planted(), Input{Texts: texts}) {
		if each.Rule == "ste100-"+steSynonymRotation && strings.Contains(each.Detail, "with the words of") {
			found[findingKey{each.Rule, each.Detail}]++
		}
	}
	return found
}

// The judge of the ways out of a footnote refuses a way out that changes the verbs of its words
// exactly where Check finds, after the way out, a finding of the rule synonym-rotation of the
// verbs of footnotes that it does not find as many times before (rotationCanChange): of a footnote
// with the footnotes that it refers to, or of a text with the footnotes that it refers to, word for
// word. For each of 1,000 cases of footnotes that refer to each other at random, loops among them,
// and texts of each kind (250 under the race detector, whose instrumentation makes each about 15
// times the work), the code font of Foo.Validate in each footnote that holds it is the way out.
// The cases must hold each kind of answer: ways out that give a finding, ways out that give none
// though two verbs of the group stand in other texts or footnotes after them, and footnotes in a
// loop.
func TestTheJudgeRefusesAWayOutOfAFootnoteExactlyWhereItGivesARotation(t *testing.T) {
	random := rand.New(rand.NewPCG(18, 4179))
	cases := 1_000
	if raceEnabled {
		cases = 250
	}
	gives, quiet, elsewhere, loops := 0, 0, 0, 0
	for range cases {
		test := randomRingCase(random)
		texts, notes := test.build(-1, "")
		before := rotationsWithFootnotes(texts)
		judge := newFootnoteJudge(planted(), peopleNames(planted(), Input{Texts: texts}), texts)
		judge.carry()
		for index, note := range notes {
			if _, held := judge.ring[note]; !held || !strings.Contains(note.Body, "Foo.Validate") {
				continue
			}
			to := strings.ReplaceAll(note.Body, "Foo.Validate", "`Foo.Validate`")
			changed, _ := test.build(index, to)
			want := false
			for key, times := range rotationsWithFootnotes(changed) {
				if times > before[key] {
					want = true
				}
			}
			was := judge.judgment(note).verbs
			_, now := ste.LintVerbs(oneLine(to))
			got := was != now && judge.rotationCanChange(note, was, now)
			if got != want {
				t.Errorf("%s: the code font of Foo.Validate in [^%d]: the judge refuses it: %v, and Check finds a new finding: %v", test, index+1, got, want)
			}
			if len(judge.rings[judge.ring[note]].notes) > 1 || slices.Contains(note.Notes, note) {
				loops++
			}
			switch {
			case want:
				gives++
			case twoVerbsElsewhere(judge, note, now):
				elsewhere++
			default:
				quiet++
			}
		}
	}
	t.Logf("ways out that give a finding %d, that give none beside two verbs elsewhere %d, others %d; in a loop %d", gives, elsewhere, quiet, loops)
	if gives == 0 || elsewhere == 0 || quiet == 0 || loops == 0 {
		t.Errorf("the cases hold %d ways out that give a finding, %d that give none beside two verbs of the group elsewhere, %d others and %d in a loop: the test reads too little",
			gives, elsewhere, quiet, loops)
	}
}

// twoVerbsElsewhere reports whether, after a way out that leaves note's words the verbs now, two
// verbs or more of a group that the way out changes stand in note's words and in the words of the
// other footnotes and texts of prose of the judgment together, where a count of the judgment, not
// of each text, would refuse the way out.
func twoVerbsElsewhere(judge *footnoteJudge, note *Footnote, now ste.Verbs) bool {
	all := now
	for other := range judge.ring {
		if other != note {
			all |= judge.judgment(other).verbs
		}
	}
	for index, text := range judge.texts {
		if prose(text.Kind) {
			all |= judge.textVerbs(index)
		}
	}
	for _, group := range (judge.judgment(note).verbs ^ now).GroupsEach() {
		if (all & group).Count() > 1 {
			return true
		}
	}
	return false
}
