package specgraph

import (
	"fmt"
	"reflect"
	"strings"
)

// A member of an input type of the derivation, a type of a file of a scenario (ReadScenario) or of
// an Input (Derive, CheckStates), a FixtureHistory or a Unit (WaitCycle), is read as its type marks
// it with the tag input, since a member that its writer left out is its zero value, and a rule that
// read that value as a real one would turn a state toward true or hide a failure. The reader of a
// file and the entry of the library read a member by one rule, in the functions that both call
// (Result.fault, JobRun.fault, subjectFault, ApprovedNode.check, FixtureHistory.check,
// CommitTies.fault, unitsFault, uncitedFault), or the entry's alone for a member that only an Input
// holds (Input.check, historyFault), or the reader's alone for one that only a file holds
// (ApprovedFile.approved, readResults). Each member has one of four marks, and a test fails for a
// member with none (TestEachMemberOfAnInputHasAMark):
//
//   - input:"nonzero": its zero value is no value of the format, so the validation of its type
//     refuses it, and a file that leaves it out is refused too, as its zero value. A nonzero
//     string has at least one character, and a nonzero interface is set, as Input.History is. A
//     nonzero list is set, not nil, and an empty list is a value, as Input.JobRuns [] says that no
//     job ran. A nonzero map is set, not nil, and each of its values is nonzero by the same rule,
//     at each depth of a map of maps; an empty map is a value, as Input.Uncited {} says that CI
//     refused no check, and a rule "" in it is refused. A value of a struct type is read by the
//     marks of its own members, as a Tie that is not known is a value (R18). A type's own rule can
//     ask more, as FixtureCommit.Files holds at least one path;
//   - input:"present: <reason>": the member's type has a value for "not set", a nil pointer or a
//     nil list, which the validation of its type refuses, from a file that leaves it out (decodeJSON
//     refuses a null, so nil is only a member left out) and from a caller that does not set it;
//     set, its zero, such as false for removed, "" for leg or [] for commits, is a value of the
//     format, as the reason says. So no mark of the library reads the Go zero value of a member that
//     the caller did not set as a value;
//   - input:"zero: <reason>": its zero value is a value of the format, as the reason says, and a
//     file that leaves it out sets that value, unless a rule of its type refuses that value where
//     the derivation reads it, as Result.fault refuses a run of a check of the code with no commit;
//   - input:"one of <member>: <reason>": of it and the other member, exactly one is set, and the
//     validation refuses neither and both, as the reason says.
//
// A member that names something, a commit, a check, a job, a leg, a deploy, a transaction of the
// graph, a branch or a unit, as a value or as the key of a map, names nothing when it holds no
// letter and no digit, and is refused by one rule (nameFault, hasWord); a path of the repository,
// in a tie and in a tree of the history, by the rule of NewTie (isRepoPath).
//
// No mark reads a JSON null: decodeJSON refuses a null at each place of each file. The reason of a
// mark zero is a decision that a review checks, since the reflection tests read only that a member
// left out reads as its zero value, and that the library takes its zero value; the sweep of the
// scenarios is its guard (TestNoInputThatSaysNothingMovesAStateTowardTrue): a member whose zero
// value, left out, moves a state toward true has a wrong mark, as leg had. The hand-written rows of
// seq (TestAcceptAFailureWithNoSeqIsNeverHidden, TestAMissedFailingExampleWithNoSeqIsNeverHidden)
// pin the one that was wrong before.
//
// Of the 17 members marked zero, the ten that a file can leave out, graph_tx of a subject, parent
// of a commit of the history, and commit, deploy, graph_tx, job, attempt, fault_author,
// test_authors and test_commit of a result, move no state toward true (the sweep). The other seven
// are the library's: an Input with no Files, Approved or Results and an Approved with no Nodes
// read as none, a Tie with no Why gives no reason, a Tie with no Known is a tie that is not known,
// and a known Tie with no Paths is refused, as is a Tie that is not known and holds a path.
type memberMark struct {
	kind  string // "nonzero", "present", "zero" or "one of"
	other string // the other member of a "one of" mark, by its JSON name
}

// markOf is the mark of the member f, or an error that names what the tag lacks: a kind, or a
// reason after a mark that needs one.
func markOf(f reflect.StructField) (memberMark, error) {
	tag := f.Tag.Get("input")
	if tag == "nonzero" {
		return memberMark{kind: tag}, nil
	}
	head, reason, _ := strings.Cut(tag, ": ")
	if !hasWord(reason) {
		return memberMark{}, fmt.Errorf("member %s has the mark %q, not nonzero or a mark with its reason", f.Name, tag)
	}
	switch {
	case head == "present" || head == "zero":
		return memberMark{kind: head}, nil
	case strings.HasPrefix(head, "one of ") && hasWord(strings.TrimPrefix(head, "one of ")):
		return memberMark{kind: "one of", other: strings.TrimPrefix(head, "one of ")}, nil
	}
	return memberMark{}, fmt.Errorf("member %s has the mark %q, not nonzero, present, zero or one of", f.Name, tag)
}

// inputMember is one member of an input type, as its JSON text names it: its name, its field and
// the path of the field's index from the type.
type inputMember struct {
	name  string
	field reflect.StructField
	index []int
}

// inputMembers is each member of the struct type t, in the order of its fields, a member of an
// embedded struct with no JSON name of its own in its place, as encoding/json/v2 reads it inline.
// A member that JSON does not hold (json:"-") is a member too, with no name.
func inputMembers(t reflect.Type) []inputMember {
	var members []inputMember
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			continue // its own members follow it in VisibleFields
		}
		if name == "" {
			name = f.Name
		}
		if name == "-" {
			name = ""
		}
		members = append(members, inputMember{name: name, field: f, index: f.Index})
	}
	return members
}

// namedMember is one member of an input that names something: the member's name, what it names,
// and its value.
type namedMember struct{ member, what, value string }

// nameFault is the refusal of the first of members whose value is set and names nothing, and ""
// when each is "" or a name: a name has a letter or a digit (hasWord), since one of only blanks or
// format characters names nothing, as Validate reads a missing name (R15). It is the one rule for a
// name of a commit, a check, a job, a leg, a deploy, a transaction of the graph, a branch or a
// unit, in a file and in the library alike. Whether "" is a value is the member's mark: the fault
// of each type refuses "" for a member marked nonzero.
func nameFault(members ...namedMember) string {
	for _, m := range members {
		if m.value != "" && !hasWord(m.value) {
			return fmt.Sprintf("it sets a %s of no letter and no digit, which names no %s", m.member, m.what)
		}
	}
	return ""
}

// pointerToken writes a name as a token of a JSON pointer (RFC 6901 §3).
var pointerToken = strings.NewReplacer("~", "~0", "/", "~1")
