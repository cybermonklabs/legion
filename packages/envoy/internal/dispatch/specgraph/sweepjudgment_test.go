package specgraph

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The states of a wait (R29: "That wait ends when main holds the statement's approved text and its
// own checks read true there"): it ends, or it holds.
const (
	waitEnds  = "ends"
	waitHolds = "holds"
)

// scenarioStates is what the sweeps judge of the scenario fsys, read and derived in the test process
// (ReadScenario, Derive): the state of each check, of each statement and its own state, and of each
// wait of each unit, at each subject, by its place: "subject <n> check <id>", "subject <n> statement
// <id>", "subject <n> own state of <id>" and "subject <n> wait of <unit> on <id>"; or the refusal of
// the reader or of the derivation. A subject is named by its place in subjects.json, not its value,
// since an input that leaves out a member of a subject, such as graph_tx, derives that subject too. A
// wait ends where WaitEnds says so, and holds where it does not.
func scenarioStates(fsys fs.FS) (map[string]string, error) {
	s, err := ReadScenario(fsys)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for i := range s.Subjects {
		state, err := Derive(s.Input(i))
		if err != nil {
			return nil, err
		}
		at := fmt.Sprintf("subject %d", i+1)
		for _, c := range state.Checks {
			states[at+" check "+c.ID] = c.State
		}
		for _, st := range state.Statements {
			states[at+" statement "+st.ID] = st.State
			states[at+" own state of "+st.ID] = st.Own
		}
		for _, u := range s.Units {
			for _, statement := range u.WaitsOn {
				wait := waitHolds
				if state.WaitEnds(statement) {
					wait = waitEnds
				}
				states[at+" wait of "+u.ID+" on "+statement] = wait
			}
		}
	}
	return states, nil
}

// stateKind is one kind of state that the sweeps judge: the words of its place after the subject, its
// states, and top, the one of them that a move toward true reaches.
type stateKind struct {
	words  string
	states []string
	top    string
}

// stateKinds is each kind of state that scenarioStates gives, by the package's own constants: a check
// true, false or unknown; a statement, and its own state, not_built, met, not_met or unknown; and a
// wait that ends or holds.
func stateKinds() []stateKind {
	statement := []string{statementNotBuilt, statementMet, statementNotMet, statementUnknown}
	return []stateKind{
		{words: "check ", states: []string{CheckTrue, CheckFalse, checkUnknown}, top: CheckTrue},
		{words: "statement ", states: statement, top: statementMet},
		{words: "own state of ", states: statement, top: statementMet},
		{words: "wait of ", states: []string{waitEnds, waitHolds}, top: waitEnds},
	}
}

// kindAt is the kind of the state at the place at, by the words of the place (scenarioStates).
func kindAt(at string) (stateKind, error) {
	words := strings.SplitN(at, " ", 3)
	if len(words) == 3 && words[0] == "subject" {
		if _, err := strconv.Atoi(words[1]); err == nil {
			for _, k := range stateKinds() {
				if strings.HasPrefix(words[2], k.words) {
					return k, nil
				}
			}
		}
	}
	return stateKind{}, fmt.Errorf("the place %q is of no kind of state that the judgment knows (stateKinds)", at)
}

// judgeAnswer is the one judgment of the sweeps (sweepAgainst): of input, the states of an input of a
// sweep, against base, the states of its base (scenarioStates), the line of each place that moved
// toward true (toward) and, of those, of each that the input's right does not allow (beyond), in the
// order of the places. A place moved toward true where the input reads the top state of its kind
// (stateKinds) and the base reads another state there, or none. An input with no right (hasRight
// false) is allowed no move; a right with allowed nil covers each place; and a right with allowed
// allows, at each place that it names, the one state that it names there, and nothing at a place that
// it leaves out. A place of no kind, or a state that is no state of the place's kind at the input or at
// the base, is an error.
func judgeAnswer(base, input map[string]string, hasRight bool, allowed map[string]string) (toward, beyond []string, err error) {
	for _, at := range slices.Sorted(maps.Keys(input)) {
		k, err := kindAt(at)
		if err != nil {
			return nil, nil, err
		}
		was, held := base[at]
		switch {
		case !slices.Contains(k.states, input[at]):
			return nil, nil, fmt.Errorf("%s: the input reads %q, which is no state of its kind", at, input[at])
		case held && !slices.Contains(k.states, was):
			return nil, nil, fmt.Errorf("%s: the base reads %q, which is no state of its kind", at, was)
		case input[at] != k.top || was == k.top:
			continue
		}
		line := fmt.Sprintf("%s: %q, and %q in the whole scenario", at, input[at], was)
		toward = append(toward, line)
		if state, named := allowed[at]; !hasRight || allowed != nil && (!named || state != input[at]) {
			beyond = append(beyond, line)
		}
	}
	return toward, beyond, nil
}

// The judgment of the sweeps (judgeAnswer) reads each kind of state as the approved words give its
// states, and a move toward true as a move to the state that they name true. R7: a check's state is
// "true, false, or unknown with a reason", and "silence is not a pass". R10: "Each statement shows one
// of four states, for one subject at a time: not built, met, not met, or unknown"; "Met: its own
// checks read true, and statements under it or that support it read met". R29: a wait on a statement
// "ends when main holds the statement's approved text and its own checks read true there. The
// statements under it do not count for that wait", so a statement's own state is met where its own
// checks read true, and the wait holds until it ends. Each row is one place: the state that the base
// reads there ("" for none), the state that the input reads, and the input's right; and the judgment
// refuses a place of no kind and a state of another kind.
func TestTheSweepsJudgmentReadsAsTheSpec(t *testing.T) {
	t.Parallel()
	const check, statement, own, wait = "subject 1 check c", "subject 1 statement s", "subject 1 own state of s", "subject 1 wait of u on s"
	// right is the right of an input: none, one that covers each place (allowed nil), or one that
	// names places, each with the one state that it allows there.
	type right struct {
		has     bool
		allowed map[string]string
	}
	none, each := right{}, right{has: true}
	naming := func(at, state string) right { return right{has: true, allowed: map[string]string{at: state}} }
	for _, r := range []struct {
		words           string
		at, base, input string
		right           right
		toward, beyond  bool
	}{
		{`R7: "true, false, or unknown with a reason"; unknown to true`, check, checkUnknown, CheckTrue, none, true, true},
		{`R7: false to true`, check, CheckFalse, CheckTrue, none, true, true},
		{`R7: "silence is not a pass"; true where the base reads no state`, check, "", CheckTrue, none, true, true},
		{`R7: unknown where the base reads no state`, check, "", checkUnknown, none, false, false},
		{`R7: false to unknown is no move to true`, check, CheckFalse, checkUnknown, none, false, false},
		{`R7: true to false`, check, CheckTrue, CheckFalse, none, false, false},
		{`R10: "not built, met, not met, or unknown"; not built to met`, statement, statementNotBuilt, statementMet, none, true, true},
		{`R10: not met to met`, statement, statementNotMet, statementMet, none, true, true},
		{`R10: not met to unknown is no move to met`, statement, statementNotMet, statementUnknown, none, false, false},
		{`R10: met to unknown`, statement, statementMet, statementUnknown, none, false, false},
		{`R29: "its own checks read true there"; an own state, unknown to met`, own, statementUnknown, statementMet, none, true, true},
		{`R29: "That wait ends when main holds the statement's approved text and its own checks read true there"; holds to ends`, wait, waitHolds, waitEnds, none, true, true},
		{`R29: a wait that ends in the base`, wait, waitEnds, waitEnds, none, false, false},
		{`R29: ends to holds`, wait, waitEnds, waitHolds, none, false, false},
		{`a right that covers each place allows unknown to true`, check, checkUnknown, CheckTrue, each, true, false},
		{`a right that names the place with true allows false to true`, check, CheckFalse, CheckTrue, naming(check, CheckTrue), true, false},
		{`a right that names the place with unknown allows no true`, check, CheckFalse, CheckTrue, naming(check, checkUnknown), true, true},
		{`a right that names another place allows nothing here`, wait, waitHolds, waitEnds, naming(check, CheckTrue), true, true},
		{`a right that names places and leaves this one out allows nothing here`, statement, statementNotMet, statementMet, right{has: true, allowed: map[string]string{}}, true, true},
	} {
		base := map[string]string{}
		if r.base != "" {
			base[r.at] = r.base
		}
		toward, beyond, err := judgeAnswer(base, map[string]string{r.at: r.input}, r.right.has, r.right.allowed)
		if err != nil || len(toward) > 1 || len(beyond) > 1 || (len(toward) == 1) != r.toward || (len(beyond) == 1) != r.beyond {
			t.Errorf("%s: %s at %q in the base and %q at the input: the judgment gives %q toward true and %q beyond the right (%v), want toward %v and beyond %v",
				r.words, r.at, r.base, r.input, toward, beyond, err, r.toward, r.beyond)
		}
	}
	for _, r := range []struct {
		what        string
		base, input map[string]string
	}{
		{"a place of no kind", nil, map[string]string{"subject 1 note of c": CheckTrue}},
		{"a check that reads a state of a statement", nil, map[string]string{check: statementMet}},
		{"a wait that reads a state of a check", nil, map[string]string{wait: CheckTrue}},
		{"a base whose statement reads a state of a wait", map[string]string{statement: waitEnds}, map[string]string{statement: statementMet}},
	} {
		if _, _, err := judgeAnswer(r.base, r.input, false, nil); err == nil {
			t.Errorf("%s: the judgment reads it", r.what)
		}
	}
}
