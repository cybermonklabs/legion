package specgraph

import "fmt"

// A member of an input type of the derivation, a type of a file of a scenario (ReadScenario) or of
// an Input (Derive, deriveChecks), a FixtureHistory or a Unit (WaitCycle), is read as its type marks
// it with the tag input, since a member that its writer left out is its zero value, and a rule that
// read that value as a real one would turn a state toward true or hide a failure. The reader of a
// file and the entry of the library read a member by one rule, in the functions that both call
// (Result.fault, JobRun.fault, subjectFault, approvedFault, FixtureHistory.check,
// CommitTies.fault, unitsFault, uncitedFault), or the entry's alone for a member that only an Input
// holds (Input.check, historyFault), or the reader's alone for one that only a file holds
// (approvedFileNode.unsetFault, readResults, jobRunFile.unsetFault). Each member has one of four
// marks, and a test fails for a member with none (TestEachMemberOfAnInputHasAMark):
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
//     nil list, which the validation of its type refuses (decodeJSON refuses a null, so nil is only
//     a member left out); set, its zero, such as false for removed, "" for leg or [] for commits,
//     is a value of the format, as the reason says. A nil list is refused from a file that leaves
//     it out and from a caller that does not set it. A pointer is a member of a type of a file
//     alone (approvedFileNode, scenarioResult, jobRunFile), whose reader refuses a member that the
//     file leaves out and gives the value to a type of the library, which holds it as a plain value
//     marked zero: a caller that does not set it gives that value, as the reason of its mark says.
//     So no mark of the library reads the Go zero value of a member that the caller did not set as
//     a value that it is not, and no reader of a value of the library follows a nil pointer;
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
// value, left out, moves a state toward true has a wrong mark. Seq is marked nonzero for that
// reason: a result with no seq, or seq 0, would sort before each record of its time, so a failure
// would hide behind the pass that it cancels, and the reader and the entry refuse it (Result.fault;
// TestTheReaderRefusesAResultThatTheOrderOfRecordCannotPlace).

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
