// results_subjects_test.go holds GET /api/v1/issues/{key}/results to which heads a `before_merge`
// check's pass must stand at, its subjects: the head of each open pull request of the issue, and
// where it has none, the final head of the newest merged one and the head of each of its branches
// that a CI job's result names. A pass stands only when one stands at each subject, and the answer
// says the state of each. It also holds R7 for a CI job's results: a pass and a fail of one check at
// one commit make that commit's state unknown, in either order.
package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// wirePassSubject is one subject of a check's pass_stands, as the wire carries it.
type wirePassSubject struct {
	Basis struct {
		Kind        string `json:"kind"`
		Commit      string `json:"commit"`
		Branch      string `json:"branch"`
		PullRequest *int   `json:"pull_request"`
	} `json:"basis"`
	Answer string `json:"answer"`
	Reason string `json:"reason"`
}

// wirePassStands is a check's pass_stands as the wire carries it, with the members this rule adds.
type wirePassStands struct {
	Answer   string            `json:"answer"`
	Reason   string            `json:"reason"`
	Subjects []wirePassSubject `json:"subjects"`
}

// passStandsOnWire is check blockID's pass_stands as GET /api/v1/issues/{issue}/results answers it.
func passStandsOnWire(t *testing.T, handler http.Handler, issue, blockID string) wirePassStands {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID    string          `json:"block_id"`
			PassStands json.RawMessage `json:"pass_stands"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	for _, check := range read.Checks {
		if check.BlockID == blockID {
			var stands wirePassStands
			if err := json.Unmarshal(check.PassStands, &stands); err != nil {
				t.Fatal(err)
			}
			return stands
		}
	}
	t.Fatalf("the read of %s holds no check %s", issue, blockID)
	return wirePassStands{}
}

// subjectAt is the subject of stands at commit, failing the test where none is.
func subjectAt(t *testing.T, stands wirePassStands, commit string) wirePassSubject {
	t.Helper()
	for _, subject := range stands.Subjects {
		if subject.Basis.Commit == commit {
			return subject
		}
	}
	t.Fatalf("pass_stands %+v names no subject at %s", stands, commit)
	return wirePassSubject{}
}

// TestAJobsFailAtThePullRequestHeadReadsNoPass: ACME-1's open pull request #70 is on legion/work
// at ciJobHead. The job passed c1 at ciJobCommit on a push of legion/ACME-1, then failed it at
// ciJobHead. The open pull request's head is the one subject: c1 compares with it alone, and reads
// no pass, naming that head and its fail.
func TestAJobsFailAtThePullRequestHeadReadsNoPass(t *testing.T) {
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: ciJobHead})
	jobRecorded(t, "the job's pass on legion/ACME-1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	jobRecorded(t, "the job's fail at the pull request's head", rig.post(t, rig.issue,
		jobTokenOn(rig, ciJobHead, "refs/heads/legion/work", 99), jobResultAt(ciJobHead, "fail")))

	check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	if len(check.ComparedWith) != 1 || check.ComparedWith[0].Commit != ciJobHead {
		t.Errorf("c1 compares with %+v, want the open pull request's head %s alone", check.ComparedWith, ciJobHead)
	}
	if check.PassStands.Answer != model.PassStandsNo {
		t.Fatalf("c1 reads %+v while the job failed at the pull request's head, want no", check.PassStands)
	}
	if subject := subjectAt(t, passStandsOnWire(t, rig.handler, rig.issue, "c1"), ciJobHead); subject.Answer != model.PassStandsNo || subject.Reason != "fail" {
		t.Fatalf("the subject at %s reads %+v, want no for its fail", ciJobHead, subject)
	}
}

// TestAPassAtABranchHeadStandsForNothingWhileAPullRequestIsOpen: the job's pass at the head of
// legion/ACME-1 counts only where the issue has no open pull request. With #70 open at ciJobHead,
// where nothing ran, c1 reads no pass; the control is the same pass read before #70 is opened.
func TestAPassAtABranchHeadStandsForNothingWhileAPullRequestIsOpen(t *testing.T) {
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	jobRecorded(t, "the job's pass on legion/ACME-1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	if check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1"); check.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the control: with no pull request the pass at the branch head reads %+v, want yes", check.PassStands)
	}

	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	if check.PassStands.Answer != model.PassStandsNo {
		t.Fatalf("with pull request #70 open at %s, c1 reads %+v, want no", ciJobHead, check.PassStands)
	}
	if subject := subjectAt(t, passStandsOnWire(t, rig.handler, rig.issue, "c1"), ciJobHead); subject.Reason != "no_result" {
		t.Fatalf("the subject at %s reads %+v, want no_result", ciJobHead, subject)
	}
}

// TestAFailAtOneBranchHeadHoldsBackThePassAtAnother: ACME-1 has no pull request and two branches its
// job pushed, legion/ACME-1 (a pass at its head) and legion/ACME-1-retry (a fail at its head). Each
// head is a subject, so c1 reads no pass, and the answer says each head's state. The control is the
// read before the fail.
func TestAFailAtOneBranchHeadHoldsBackThePassAtAnother(t *testing.T) {
	rig := newCIJobRig(t)
	const retry = "legion/ACME-1-retry"
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	rig.github.setBranch(retry, ciJobBranchHead{sha: ciJobOtherCommit})
	jobRecorded(t, "the job's pass on legion/ACME-1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	if check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1"); check.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the control: one branch with a pass reads %+v, want yes", check.PassStands)
	}

	jobRecorded(t, "the job's fail on "+retry, rig.post(t, rig.issue,
		jobTokenOn(rig, ciJobOtherCommit, "refs/heads/"+retry, 99), jobResultAt(ciJobOtherCommit, "fail")))
	if check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1"); check.PassStands.Answer != model.PassStandsNo {
		t.Fatalf("a fail at the head of %s reads %+v, want no", retry, check.PassStands)
	}
	stands := passStandsOnWire(t, rig.handler, rig.issue, "c1")
	if passed, failed := subjectAt(t, stands, ciJobCommit), subjectAt(t, stands, ciJobOtherCommit); passed.Answer != model.PassStandsYes || failed.Answer != model.PassStandsNo || failed.Reason != "fail" {
		t.Fatalf("the subjects read %+v and %+v, want yes at %s and no (fail) at %s", passed, failed, ciJobCommit, ciJobOtherCommit)
	}
}

// TestAPassAndAFailOfTheJobAtOneCommitReadUnknown: R7. Two attempts of the job at one commit, a
// fail and a pass, in each order, make that commit's state unknown: no pass stands, and the answer
// says why (pass_and_fail). The control: two passes at one commit read yes.
func TestAPassAndAFailOfTheJobAtOneCommitReadUnknown(t *testing.T) {
	for _, order := range [][2]string{{"fail", "pass"}, {"pass", "fail"}, {"pass", "pass"}} {
		t.Run(order[0]+" then "+order[1], func(t *testing.T) {
			rig := newCIJobRig(t)
			rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
			jobRecorded(t, "the first attempt", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", order[0])))
			jobRecorded(t, "the second attempt", rig.post(t, rig.issue, jobTokenOn(rig, ciJobCommit, ciJobRef, 98), jobResultOf("c1", order[1])))
			stands := passStandsOnWire(t, rig.handler, rig.issue, "c1")
			if order[0] == order[1] {
				if stands.Answer != model.PassStandsYes {
					t.Fatalf("the control: two passes at %s read %+v, want yes", ciJobCommit, stands)
				}
				return
			}
			if stands.Answer != model.PassStandsUnknown || stands.Reason != "pass_and_fail" {
				t.Fatalf("a %s and a %s of c1 at %s read %+v, want unknown for pass_and_fail", order[0], order[1], ciJobCommit, stands)
			}
		})
	}
}

// TestCIsResultsThatDisagreeAtOneHeadReadUnknown: R7. ACME-1's open pull request #70 is at
// ciJobHead. The job's own result of c1 at that head and the result Dispatch derives from the
// go-checks run there are both CI's evidence of c1 at one commit, so a fail and a pass of the two,
// in each order, make that head unknown (pass_and_fail). The controls: the job's fail alone reads
// no, and the job's pass beside a derived pass reads yes.
func TestCIsResultsThatDisagreeAtOneHeadReadUnknown(t *testing.T) {
	for _, row := range []struct {
		name, job, derived string
		answer, reason     string
	}{
		{"the job's fail beside a derived pass", "fail", ciConclusionSuccess, model.PassStandsUnknown, model.PassStandsReasonPassAndFail},
		{"the job's pass beside a derived fail", "pass", "failure", model.PassStandsUnknown, model.PassStandsReasonPassAndFail},
		{"the control: the job's fail alone", "fail", "", model.PassStandsNo, "fail"},
		{"the control: the job's pass beside a derived pass", "pass", ciConclusionSuccess, model.PassStandsYes, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCIJobRig(t)
			writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
			rig.github.setBranch("legion/work", ciJobBranchHead{sha: ciJobHead})
			jobRecorded(t, "the job's "+row.job+" at the pull request's head", rig.post(t, rig.issue,
				jobTokenOn(rig, ciJobHead, "refs/heads/legion/work", 99), jobResultAt(ciJobHead, row.job)))
			if row.derived != "" {
				rig.github.listings.set(ciJobHead, completedRun(11, ciJobName, row.derived))
			}
			stands := passStandsOnWire(t, rig.handler, rig.issue, "c1")
			subject := subjectAt(t, stands, ciJobHead)
			if stands.Answer != row.answer || subject.Answer != row.answer || subject.Reason != row.reason {
				t.Fatalf("c1 reads %+v, its subject at %s %+v; want %s, and %s at the subject", stands, ciJobHead, subject, row.answer, row.reason)
			}
		})
	}
}

// TestAMergedHeadStaysABasisWhileAPullRequestIsOpen: ACME-1's pull request #69 merged at
// ciJobCommit, and #70 is open at ciJobHead. k1 compares with both heads: the merged one stays a
// basis and is no subject. A tester's pass at the merged head stands at that basis, and no pass of
// k1 stands, since the open head, the one subject, holds no result.
func TestAMergedHeadStaysABasisWhileAPullRequestIsOpen(t *testing.T) {
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 69, ciJobCommit, "merged", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	if recorded := rig.post(t, rig.issue, rig.sessionToken(), resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})); recorded.Code != http.StatusCreated {
		t.Fatalf("the tester's pass of k1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	check := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
	heads := map[int]string{}
	for _, basis := range check.ComparedWith {
		if basis.PullRequest != nil {
			heads[*basis.PullRequest] = basis.Commit
		}
	}
	if len(check.ComparedWith) != 2 || heads[69] != ciJobCommit || heads[70] != ciJobHead {
		t.Fatalf("k1 compares with %+v, want the heads of #69 (%s) and #70 (%s)", check.ComparedWith, ciJobCommit, ciJobHead)
	}
	if len(check.Results) != 1 || check.Results[0].CommitStands == nil || check.Results[0].CommitStands.Answer != model.CommitAtBasis {
		t.Fatalf("the tester's pass at the merged head reads %+v, want at_basis", check.Results)
	}
	stands := passStandsOnWire(t, rig.handler, rig.issue, "k1")
	if stands.Answer != model.PassStandsNo || len(stands.Subjects) != 1 || stands.Subjects[0].Basis.Commit != ciJobHead {
		t.Fatalf("k1 reads %+v, want no, with the open head %s its one subject", stands, ciJobHead)
	}
}

// TestTheNewestMergedHeadIsASubjectWhereNoPullRequestIsOpen: ACME-1 has no open pull request and
// two merged ones, #69 at ciJobCommit and #70, the newest, at ciJobHead. k1 compares with both
// heads, and #70's final head is its one subject: a tester's pass at #69's head stands at that
// basis, and no pass of k1 stands, since #70's head holds no result. The control: a tester's pass at
// #70's head makes that subject yes, and k1 reads a pass from the tester.
func TestTheNewestMergedHeadIsASubjectWhereNoPullRequestIsOpen(t *testing.T) {
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 69, ciJobCommit, "merged", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "merged", rig.issue)
	older := jobRecorded(t, "the tester's pass of k1 at #69's head", rig.post(t, rig.issue, rig.sessionToken(),
		resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})))
	read := readResults(t, rig.handler, rig.issue)
	check := checkOf(t, read, "k1")
	heads := map[int]string{}
	for _, basis := range check.ComparedWith {
		if basis.PullRequest != nil {
			heads[*basis.PullRequest] = basis.Commit
		}
	}
	if len(check.ComparedWith) != 2 || heads[69] != ciJobCommit || heads[70] != ciJobHead {
		t.Fatalf("k1 compares with %+v, want the heads of #69 (%s) and #70 (%s)", check.ComparedWith, ciJobCommit, ciJobHead)
	}
	standsAt(t, read, "k1", older.ID, "the tester's pass at the older merged head", model.CommitAtBasis, false)
	stands := passStandsOnWire(t, rig.handler, rig.issue, "k1")
	if subject := subjectAt(t, stands, ciJobHead); stands.Answer != model.PassStandsNo || len(stands.Subjects) != 1 ||
		subject.Basis.PullRequest == nil || *subject.Basis.PullRequest != 70 || subject.Answer != model.PassStandsNo || subject.Reason != "no_result" {
		t.Fatalf("k1 reads %+v, want no, with #70's head %s its one subject and no result there", stands, ciJobHead)
	}

	jobRecorded(t, "the control: the tester's pass of k1 at #70's head", rig.post(t, rig.issue, rig.sessionToken(),
		resultBody("k1", "pass", ciJobHead, map[string]any{"role": "tester"})))
	stands = passStandsOnWire(t, rig.handler, rig.issue, "k1")
	if subject := subjectAt(t, stands, ciJobHead); stands.Answer != model.PassStandsYes || len(stands.Subjects) != 1 || subject.Answer != model.PassStandsYes {
		t.Fatalf("the control: with a pass at #70's head k1 reads %+v, want yes at its one subject", stands)
	}
	passStandsFrom(t, checkOf(t, readResults(t, rig.handler, rig.issue), "k1"), "the control", model.PassStandsYes, "tester")
}

// TestABranchHeadAnOlderMergedHeadNamesIsASubject: ACME-1 has no open pull request and two merged
// ones, #69 at ciJobCommit and #70, the newest, at ciJobHead. The job's pass of c1 names
// legion/ACME-1, whose head is still ciJobCommit. That head adds no basis, since #69's head names
// the commit already, but it is a subject, under #69's basis, beside #70's final head: a tester's
// pass of k1 at #70's head alone reads no pass, and the answer names #69's head with no result.
// The control: a tester's pass at ciJobCommit too makes both subjects yes, and k1 reads a pass.
func TestABranchHeadAnOlderMergedHeadNamesIsASubject(t *testing.T) {
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 69, ciJobCommit, "merged", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "merged", rig.issue)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	jobRecorded(t, "the job's pass of c1 on "+ciJobBranch, rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	jobRecorded(t, "the tester's pass of k1 at #70's head", rig.post(t, rig.issue, rig.sessionToken(),
		resultBody("k1", "pass", ciJobHead, map[string]any{"role": "tester"})))

	check := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
	heads := map[int]string{}
	for _, basis := range check.ComparedWith {
		if basis.PullRequest != nil {
			heads[*basis.PullRequest] = basis.Commit
		}
	}
	if len(check.ComparedWith) != 2 || heads[69] != ciJobCommit || heads[70] != ciJobHead || len(check.BranchHeads) != 0 {
		t.Fatalf("k1 compares with %+v and branch heads %+v, want the heads of #69 (%s) and #70 (%s) and no branch head",
			check.ComparedWith, check.BranchHeads, ciJobCommit, ciJobHead)
	}
	stands := passStandsOnWire(t, rig.handler, rig.issue, "k1")
	newest, branch := subjectAt(t, stands, ciJobHead), subjectAt(t, stands, ciJobCommit)
	if stands.Answer != model.PassStandsNo || len(stands.Subjects) != 2 || newest.Answer != model.PassStandsYes ||
		branch.Basis.PullRequest == nil || *branch.Basis.PullRequest != 69 || branch.Answer != model.PassStandsNo || branch.Reason != "no_result" {
		t.Fatalf("k1 reads %+v, want no, with #70's head yes and the head of %s, #69's at %s, no for no_result", stands, ciJobBranch, ciJobCommit)
	}

	jobRecorded(t, "the control: the tester's pass of k1 at the head of "+ciJobBranch, rig.post(t, rig.issue, rig.sessionToken(),
		resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})))
	stands = passStandsOnWire(t, rig.handler, rig.issue, "k1")
	if stands.Answer != model.PassStandsYes || len(stands.Subjects) != 2 ||
		subjectAt(t, stands, ciJobHead).Answer != model.PassStandsYes || subjectAt(t, stands, ciJobCommit).Answer != model.PassStandsYes {
		t.Fatalf("the control: with a pass at both heads k1 reads %+v, want yes at both subjects", stands)
	}
}
