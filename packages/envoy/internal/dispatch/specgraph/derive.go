package specgraph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// State is the state of each check and each statement at one subject (R10: "A page says which
// subject it shows"): Checks as deriveChecks gives them, and Statements in the order of Derive.
type State struct {
	Subject    Point            `json:"subject"`
	Checks     []CheckState     `json:"checks"`
	Statements []StatementState `json:"statements"`
}

// Derive is the state at in.Subject of each check that deriveChecks judges, and of each statement
// that the approved spec holds a current text of, in preorder: the top statements by id, then the
// statements under each one by id, depth first.
//
// The state of a statement S follows its own checks that read the subject, the checks that
// deriveChecks judges (R10: "Only the checks that read the subject count"), the state of each
// statement under S, and the state of each statement that supports S, the statements whose serves
// names S (R1: "A statement is true when the statements under it, the statements that support it,
// and its own checks are true"). So it follows them at each level: a failing check of a supporter
// of a statement under S, or of a supporter of a supporter, makes S not met. They are taken in the
// search order: S itself, S's own checks by id, each statement under S by id, then S's supporters
// by id. The first rule that holds decides, and Detail names the first statement, check or
// supporter in the search order that the rule reads, a statement under S giving its own reason and
// Detail (statementJudge.byChecks):
//
//  1. The files do not hold the current approved text of S or of a statement under it: not_built,
//     not_built, Detail the first such statement in preorder, S first (R10: "Not built: the files
//     at that commit do not hold the approved text of the statement, or of a statement under it").
//     A supporter that is not built does not make S not built; it makes S unknown (rule 3).
//  2. An own check is false, or a statement under S or a supporter is not met: not_met,
//     check_false or supporter_not_met (R10: "Not met: an own check reads false, or a statement
//     under it or that supports it reads not met"), also when the tree rules refuse S, since a
//     refusal never hides a failure.
//  3. An own check is unknown, S has no check where the tree rules ask for one (a leaf, or a
//     statement that other statements serve), the tree rules refuse S, or a statement under S or a
//     supporter is unknown or not built: unknown, check_unknown, no_check, refused,
//     statement_refused, supporter_unknown or supporter_not_built (R10: "Unknown: every other
//     case"; R6: "Each leaf statement has one or more checks"; R5: "A statement that other
//     statements support still has checks of its own"; R7: "silence is not a pass"; and no clean
//     state for a tree that the rules refuse).
//  4. Else met, checks_true, also when no check counts (R10: "Met: its own checks read true, and
//     statements under it or that support it read met"), and
//     PartlyChecked when the approved spec marks S (R10: "The page then says partly checked beside
//     met"). R37 needs that reading of no check: a statement whose only checks read production or
//     the graph is met at the head of main before its issue is done and its production checks run.
//     A leaf whose checks all read production or the graph is such a statement, and not a leaf with
//     no check.
//
// Own is the state of S over its own checks that read the subject, which a wait on S reads (R29:
// "its own checks read true there. The statements under it do not count for that wait"; R10:
// "Only the checks that read the subject count"), in this order (statementJudge.ownState):
// not_built when the files do not hold S's current text; else not_met when one of them is false,
// also when the tree rules refuse S, since a refusal never hides a failure; else unknown when S has
// no check where the tree rules ask for one, the tree rules refuse S, or one of them is unknown, so
// a wait on it holds (R6, R5, and R10: "Met means that its checks pass"); else met, also when S
// has statements under it, no supporter and no own check, or none of its checks reads the subject.
// So at the head of main a wait on S never waits for a check of production.
//
// Derive gives an error, and no state, for an Input that is not whole (Input.check), and for a
// History that could not answer, as deriveChecks does.
func Derive(in Input) (State, error) {
	checks, states, err := deriveChecks(in)
	if err != nil {
		return State{}, err
	}
	s := State{Subject: in.Subject, Checks: states}
	if s.Checks == nil {
		s.Checks = []CheckState{}
	}
	j := newStatementJudge(checks.current, in.Approved, checks.holds, checks.refused, s.Checks)
	s.Statements = j.derive(checks.current)
	return s, nil
}

// check is the first fault of in that the derivation refuses, nil when in is whole, in the words
// that the scenario reader refuses the same fault with. A member that the caller did not set is its
// zero value, and the judge would read that value as a real one, which turns a state toward true or
// hides a failure; so the judge never reads one, and no state is given. A member marked present has
// a type whose zero value is "not set" (members.go), refused here as a file that leaves it out is;
// a member whose zero is a value is the plain value, as its type says what a caller that leaves it
// zero gives: Leg "" is the leg of no matrix, Ended false a run that has not ended, Removed false a
// node that the approved spec keeps, and PartlyChecked false a statement whose checks decide all of
// it. Each name and each path is read by the one rule of its kind (nameFault, isRepoPath):
//
//   - the subject: its commit, its now, exactly one of a branch and a deploy, graph_tx only with a
//     deploy, and no name of no letter and no digit (subjectFault); with no now no time limit would
//     end, and with neither, no check of production would be judged;
//   - the approved spec, as ReadApproved refuses it (approvedFault), and each node under its own
//     id, as WriteApproved refuses it: a node with no version, a document that is not its one
//     document, a relation that names no node of the approved spec of its kind, a check whose
//     document leaves out serves, or a key that is not its id would leave the node or the relation
//     out, a loop of parent and support relations would leave each statement of the loop out of the
//     tree of the statement above it (R5), and a version with no time would make no result suspect
//     (R7: "A change to the text of a statement makes every result under it suspect");
//   - each result (Result.fault), with the current text of its check when it carries that text's
//     digest, so it is judged by what the judge reads of it, and no two results of one check at one
//     At and one Seq (sameRecordFault);
//   - History set, FixtureHistory{Commits: []FixtureCommit{}} for none: a run of a check of the
//     code counts only at its own commit, and a failing example that missed its fault only at the
//     commit that it counts from, history or not, and with no history no caught failing example
//     counts at another commit, nor a run of a program check of production or of the graph whose
//     test is of another commit (carries), so a failure of such a check there counts nowhere, and a
//     caller that gives none is not one that states that it has none; a FixtureHistory as
//     ReadFixtureHistory refuses it (FixtureHistory.check), Commits set among
//     it, a GitHistory with no Dir, and a nil value of any type that implements History
//     (historyFault): a commit with no files, or a tree with no path, would be an empty tree, and
//     two of them one tree, a content label of no letter and no digit, such as "" or " ", would be
//     one content at each commit that has it, a path that is no repository path a change that no
//     tie covers, and a nil history would panic where a result carries;
//   - Ties set, CommitTies{} for none, as ReadCommitTies refuses it (CommitTies.fault), for the
//     same reason: each commit and check of it a name, the map of each commit that it names set (a
//     commit where no tie is known has {} or no key), each known tie as NewTie refuses it, since a
//     known tie with no path covers no change, and no tie that is not known and holds a path;
//   - JobRuns and Uncited set, empty for none: with no job run a leg that ended with no result
//     would be hidden (R9), and with no map of the checks that CI refused, a refused check would be
//     judged at its commit by its results, where R48 makes it unknown ("At that commit, such a
//     check reads unknown, and no earlier result of it counts"), and elsewhere by the results of
//     the refused commit that support true, which count nowhere (deriveChecks, step 2); and Uncited
//     as the reader of uncited.json refuses it (uncitedFault);
//   - each job run (JobRun.fault).
//
// A History that could not answer is refused later, where the judge first asks it (checkJudge).
// current is the tree of the current texts of in.Approved.
func (in Input) check(current Tree) error {
	if fault := subjectFault(in.Subject, in.Now); fault != "" {
		return fmt.Errorf("specgraph: subject: %s", fault)
	}
	nodes := make([]ApprovedNode, 0, len(in.Approved.Nodes))
	for _, id := range slices.Sorted(maps.Keys(in.Approved.Nodes)) {
		n := in.Approved.Nodes[id]
		if err := n.keyFault(id); err != nil {
			return err
		}
		nodes = append(nodes, n)
	}
	if err := approvedFault(nodes, nil); err != nil {
		return err
	}
	type text struct {
		check  *Check
		digest string
	}
	texts := make(map[string]text, len(current.Checks))
	for i := range current.Checks {
		c := &current.Checks[i]
		texts[c.ID] = text{c, c.Digest()}
	}
	procedures := make([]*Check, len(in.Results)) // the current text of the check that ran each result, or nil
	for i, r := range in.Results {
		if t, held := texts[r.Check]; held && t.digest == r.CheckDigest {
			procedures[i] = t.check
		}
		if fault := r.fault(procedures[i]); fault != "" {
			return fmt.Errorf("specgraph: result %d: %s", i+1, fault)
		}
	}
	if fault := sameRecordFault(in.Results); fault != "" {
		return errors.New("specgraph: " + fault)
	}
	if err := historyFault(in.History); err != nil {
		return err
	}
	if in.Ties == nil {
		return errors.New("specgraph: it sets no Ties; an Input that knows no tie sets CommitTies{}")
	}
	if err := in.Ties.fault(); err != nil {
		return fmt.Errorf("specgraph: ties: %w", err)
	}
	switch {
	case in.JobRuns == nil:
		return errors.New("specgraph: it sets no JobRuns; an Input at a commit where no job ran sets an empty list")
	case in.Uncited == nil:
		return errors.New("specgraph: it sets no Uncited; an Input in whose history CI refused no check sets an empty map")
	}
	if fault := uncitedFault(in.Uncited); fault != "" {
		return errors.New("specgraph: uncited: " + fault)
	}
	for i, r := range in.JobRuns {
		if fault := r.fault(); fault != "" {
			return fmt.Errorf("specgraph: job run %d: %s", i+1, fault)
		}
	}
	if fault, _ := legFault(in.JobRuns, in.Results, procedures); fault != "" {
		return errors.New("specgraph: " + fault)
	}
	return nil
}

// uncitedFault is the refusal of the first fault of uncited, the checks that CI refused at each
// commit, by commit and then by check, and "" when it has none: a commit or a check of no letter
// and no digit (hasWord), which names none, so the refusal would count nowhere, as the reader
// refuses such a commit of a subject; a commit whose map of checks is nil; and a rule "", since a
// refusal of CI names the rule of R48 that refused the check, and each value of a map marked
// nonzero is nonzero (members.go). A map {} of a commit is a value: CI refused no check there. The
// reader of uncited.json and the entry of the derivation both refuse uncited by it, and the least
// place by commit and then by check is the one named, whatever the order of the map.
func uncitedFault(uncited map[string]map[string]string) string {
	for _, commit := range slices.Sorted(maps.Keys(uncited)) {
		checks := uncited[commit]
		switch {
		case !hasWord(commit):
			return fmt.Sprintf("commit %s: a commit of no letter and no digit names no commit", quoted(commit))
		case checks == nil:
			return fmt.Sprintf("commit %s: it sets no map of the checks that CI refused there, and a commit where CI refused no check has an empty map or no key", quoted(commit))
		}
		for _, check := range slices.Sorted(maps.Keys(checks)) {
			switch {
			case !hasWord(check):
				return fmt.Sprintf("commit %s: check %s: a check of no letter and no digit names no check", quoted(commit), quoted(check))
			case checks[check] == "":
				return fmt.Sprintf("commit %s: check %s: its rule is empty, and a refusal of CI names the rule that refused the check", quoted(commit), quoted(check))
			}
		}
	}
	return ""
}

// WaitEnds is whether a wait on the statement ends at the subject (R29: "That wait ends when main
// holds the statement's approved text and its own checks read true there"): the subject is the
// head of main and the statement's Own state is met. It is false for an id that is no current
// statement.
func (s State) WaitEnds(statement string) bool {
	if s.Subject.Branch != "main" {
		return false
	}
	i := slices.IndexFunc(s.Statements, func(st StatementState) bool { return st.ID == statement })
	return i >= 0 && s.Statements[i].Own == statementMet
}
