package specgraph

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// Outcome is what one result of a check found: its test or query passed, failed, or could not
// run.
type Outcome string

// The three outcomes of a result.
const (
	OutcomePass        Outcome = "pass"
	OutcomeFail        Outcome = "fail"
	OutcomeCouldNotRun Outcome = "could_not_run"
)

// ResultKind is what a result reports: a run of the check on its subject, or a run on its failing
// example (R9), where OutcomeFail means that the check failed on the fault, as it must.
type ResultKind string

// The two kinds of result.
const (
	ResultRun            ResultKind = "run"
	ResultFailingExample ResultKind = "failing_example"
)

// Result is one result of a check as Dispatch stores it (R7, R12): appended and never edited. Its
// JSON names are the form of results.json, which names the approved version of the check that ran
// in the place of CheckDigest (scenarioResult), and its tag input marks how each member reads when
// its writer does not set it (memberMark).
type Result struct {
	// Check is the id of the check, and CheckDigest the Digest of the check's text that ran: the
	// procedure of the result (R7).
	Check       string     `json:"check" input:"nonzero"`
	CheckDigest string     `json:"-" input:"nonzero"`
	Kind        ResultKind `json:"kind" input:"nonzero"`
	Outcome     Outcome    `json:"outcome" input:"nonzero"`
	// Commit, Deploy and GraphTx are the subject that the result read: a commit of the code, a
	// deploy of production, or a transaction of the graph; at most one of the three is set. A result
	// counts only at the subject of its kind that the derivation is given (R7: "A result counts only
	// for the procedure that ran it and for the subject that it read").
	Commit  string `json:"commit" input:"zero: it read no commit; fault refuses that for a run or a failing example of a check of the code"`
	Deploy  string `json:"deploy" input:"zero: it read no deploy; fault refuses that for a run of a check of production"`
	GraphTx string `json:"graph_tx" input:"zero: it read no transaction; fault refuses that for a run of a check of the graph"`
	// Job and Leg are the job that reported the result and its matrix values, "" for none (R12), and
	// Attempt the attempt of the job's run. Leg is nil when it is not set, since "" is a leg too.
	Job     string  `json:"job" input:"zero: no job reported it; fault refuses that for a run of a program check that names a job"`
	Leg     *string `json:"leg" input:"present: \"\" is the leg of no matrix, which the results of a job from before its matrix give"`
	Attempt int     `json:"attempt" input:"zero: no rule reads it"`
	// At is Dispatch's record time and Seq its order of record, from 1: (At, Seq) orders results.
	At  time.Time `json:"at" input:"nonzero"`
	Seq int64     `json:"seq" input:"nonzero"`
	// FaultAuthor wrote the fault of a failing example, and TestAuthors the test that ran on it
	// (R9: "An agent that did not write the test writes the fault"). The failing example of a
	// program check counts only when both are named and FaultAuthor is none of TestAuthors.
	FaultAuthor string   `json:"fault_author" input:"zero: it names no author, so a failing example of a program check reads fault_by_test_author"`
	TestAuthors []string `json:"test_authors" input:"zero: it names no author, so a failing example of a program check reads fault_by_test_author"`
	// TestCommit is the commit of the test that ran, for a run or a failing example of a program
	// check whose subject is production or the graph, and "" for a result of any other check. Such a
	// result counts only while no file of the check's tie differs between TestCommit and the
	// subject's commit (R7: the procedure is "the code that runs it" too; R9: "its test, as it is
	// now").
	TestCommit string `json:"test_commit" input:"zero: no test of its own ran; fault refuses that for a program check of production or the graph"`
}

// fault is the first member of r that the derivation reads and that r does not set, or does not
// set to a value of the form, as a refusal names it, and "" when r is whole. The one reader of
// results.json and the entry of the derivation (Derive and CheckStates) both refuse a result by it,
// since the zero value of such a member would read as a real one and hide a failure (R7: "A pass
// and a failure on one such subject and one procedure make the state unknown"; "a later failure
// cancels an earlier pass"): a result with no at would be older than every approval, one with no
// seq the first record of its at, one with no leg a result of no matrix, and one with no subject, no
// job or no test commit would count nowhere, as would one whose check, commit, deploy, graph_tx,
// job, leg or test_commit names nothing (nameFault). Every result sets its check, the digest of
// the text that ran, a kind and an outcome of the format, at most one subject, its leg, at, and seq
// from 1. procedure is the text of the check that ran, when the caller holds it, and nil when not;
// with it, r also sets what that text makes the judge read: a run, and a failing example of a check
// of the code, the subject of the check's kind (commit, deploy or graph_tx), also a run of a check
// of the graph that could not run, which names the transaction that it tried to read (R13: a query
// "reads the graph at one transaction"); a run of a program check that names a job, the job; and a
// result of a program check of production or of the graph, test_commit. A failing example of a
// query or an agent of production or of the graph names no subject, since each one counts
// (CheckStates, step 7). The reader of results.json and the entry of the derivation name the
// result that they refuse as "result <n>", from 1, in the order of the list, as for a result that
// names two of commit, deploy and graph_tx.
func (r Result) fault(procedure *Check) string {
	switch named := namedSubjects(r); {
	case r.Check == "":
		return "it sets no check"
	case r.CheckDigest == "":
		return "it carries no digest of the text of its check"
	case r.Kind != ResultRun && r.Kind != ResultFailingExample:
		return fmt.Sprintf("kind %s is not run or failing_example", quoted(string(r.Kind)))
	case r.Outcome != OutcomePass && r.Outcome != OutcomeFail && r.Outcome != OutcomeCouldNotRun:
		return fmt.Sprintf("outcome %s is not pass, fail or could_not_run", quoted(string(r.Outcome)))
	case len(named) > 1:
		return fmt.Sprintf("it names the subjects %s; a result reads one subject", strings.Join(named, " and "))
	case r.Leg == nil:
		return `it sets no leg; a result of no matrix sets ""`
	case r.At.IsZero():
		return "it sets no at"
	case r.Seq < 1:
		return "it sets no seq"
	}
	if fault := nameFault(namedMember{"check", "check", r.Check}, namedMember{"commit", "commit", r.Commit},
		namedMember{"deploy", "deploy", r.Deploy}, namedMember{"graph_tx", "transaction of the graph", r.GraphTx},
		namedMember{"job", "job", r.Job}, namedMember{"leg", "leg", *r.Leg}, namedMember{"test_commit", "commit", r.TestCommit}); fault != "" || procedure == nil {
		return fault
	}
	c, run := *procedure, r.Kind == ResultRun
	switch {
	case (run || c.Subject == SubjectCode) && readOf(c, r.Commit, r.Deploy, r.GraphTx) == "":
		return fmt.Sprintf("it sets no %s, the subject that a check of %s reads",
			readOf(c, "commit", "deploy", "graph_tx"), readOf(c, "the code", "production", "the graph"))
	case run && c.Procedure == ProcedureProgram && checkJob(c) != "" && r.Job == "":
		return "it sets no job, and only the reports of the job of its check count"
	case c.Procedure == ProcedureProgram && readsProduction(c) && r.TestCommit == "":
		return "it sets no test_commit, the commit of the test that ran"
	}
	return ""
}

// namedSubjects is the name of each subject that the result r names, of commit, deploy and
// graph_tx, in that order.
func namedSubjects(r Result) []string {
	var named []string
	for _, s := range []struct{ name, value string }{{"commit", r.Commit}, {"deploy", r.Deploy}, {"graph_tx", r.GraphTx}} {
		if s.value != "" {
			named = append(named, s.name)
		}
	}
	return named
}

// sameRecordFault is the refusal of the first pair of results of one check at one At and one Seq,
// which the order of record (byRecord) cannot order, and "" when there is none: of such a pass and
// such a failure the judge would read the one listed first as the later, so a failure could be
// hidden. The reader of results.json and the entry of the derivation refuse such a pair.
func sameRecordFault(results []Result) string {
	type record struct {
		check     string
		sec, nsec int64
		seq       int64
	}
	first := make(map[record]int, len(results))
	for j, r := range results {
		key := record{r.Check, r.At.Unix(), int64(r.At.Nanosecond()), r.Seq}
		if i, seen := first[key]; seen {
			return fmt.Sprintf("result %d: it has the at and the seq of result %d, so neither of the two is the later", j+1, i+1)
		}
		first[key] = j
	}
	return ""
}

// JobRun is one run of a CI job at a commit, in one leg of its matrix ("" for none, the form of
// Result.Leg), and whether it ended (R9: "after a run of its job ends"). A run that has not ended is
// no silent leg and no pass: the check reads unknown until its leg reports. Leg and Ended are nil
// when they are not set, since "" and false are values too. Its JSON names are the form of
// jobs.json.
type JobRun struct {
	Job    string  `json:"job" input:"nonzero"`
	Leg    *string `json:"leg" input:"present: \"\" is the leg of no matrix, which the results of a job from before its matrix give"`
	Commit string  `json:"commit" input:"nonzero"`
	Ended  *bool   `json:"ended" input:"present: false is a run that has not ended"`
}

// fault is the first member of r that the derivation reads and that r does not set, as a refusal
// names it, and "" when r sets each: the job, the leg, the commit and whether it ended, since a run
// of no job or at no commit would hide a leg of the check's job that ended with no result (R9:
// "When a check has no result that counts after a run of its job ends, it reads unknown"), one with
// no leg would read as the leg of no matrix, and one with no ended as a run that has not ended;
// and a job, a leg or a commit that names nothing (nameFault), which would hide that leg too. Leg
// "" is the leg of no matrix. The reader of jobs.json and the entry of the derivation both refuse a
// job run by it.
func (r JobRun) fault() string {
	switch {
	case r.Job == "":
		return "it sets no job"
	case r.Leg == nil:
		return `it sets no leg; a run of no matrix sets ""`
	case r.Commit == "":
		return "it sets no commit"
	case r.Ended == nil:
		return "it sets no ended; a run that has not ended sets false"
	}
	return nameFault(namedMember{"job", "job", r.Job}, namedMember{"leg", "leg", *r.Leg}, namedMember{"commit", "commit", r.Commit})
}

// legFault is the refusal of the first record that says that one job at one commit has legs and
// has none, and "" when there is none, with whether that record is a job run. The records of a job
// at a commit are its runs there, in their order, and then its run results, in their order, at the
// commit where the job ran, the result's TestCommit when it sets one, else its Commit; the record
// refused is the first whose leg is "" where the first record of its job at its commit names a
// leg, or that names a leg where that record's is "". "" is the leg of no matrix, and one job at one
// commit has legs or it has none (R12: "Each leg of a matrix reports its own result, and no leg
// replaces the result of another"), so such records say two things that cannot both be true: a run
// result of the leg "" at a commit where the job had legs would answer a run of no matrix wherever
// the results of that commit count, and hide that the run ended with no result (R9: "When a check
// has no result that counts after a run of its job ends, it reads unknown"). A state reads no leg
// of a failing example, so its leg is no part of the rule. Each run and each result is
// whole (JobRun.fault, Result.fault). The reader of a scenario and the entry of the derivation both
// refuse by it. The refusal names the record, its job, its commit and the two legs, and the reader
// of a scenario names the file of the record, jobs.json for a run and results.json for a result.
func legFault(runs []JobRun, results []Result) (fault string, ofRun bool) {
	type at struct{ job, commit string }
	type record struct{ name, leg string }
	first := map[at]record{} // the first record of each job at each commit
	differs := func(name, job, commit, leg string) string {
		f, seen := first[at{job, commit}]
		switch {
		case !seen:
			first[at{job, commit}] = record{name, leg}
		case (leg == "") != (f.leg == ""):
			return fmt.Sprintf(`%s: it sets the leg %s, and %s of its job %s at the commit %s sets the leg %s; one job at one commit has legs or it has none, and "" is the leg of no matrix`,
				name, quoted(leg), f.name, quoted(job), quoted(commit), quoted(f.leg))
		}
		return ""
	}
	for i, r := range runs {
		if fault := differs(fmt.Sprintf("job run %d", i+1), r.Job, r.Commit, *r.Leg); fault != "" {
			return fault, true
		}
	}
	for i, r := range results {
		if commit := cmp.Or(r.TestCommit, r.Commit); r.Kind == ResultRun && r.Job != "" && commit != "" {
			if fault := differs(fmt.Sprintf("result %d", i+1), r.Job, commit, *r.Leg); fault != "" {
				return fault, false
			}
		}
	}
	return "", false
}
