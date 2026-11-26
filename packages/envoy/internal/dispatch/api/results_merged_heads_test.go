// results_merged_heads_test.go holds GET /api/v1/issues/{key}/results to which heads a
// `before_merge` check's pass must stand at where its issue holds a merged pull request: the head
// of each open pull request of the issue, and where it has none, the final head of the newest
// merged one. A merged head stays a basis beside an open one, and is no subject. A pass stands only
// when one stands at each subject, and the answer says the state of each. The tests, their types
// and their helpers are cybermonklabs/legion#4230's (results_subjects_test.go, ci_job_results_test.go
// and results_branch_head_test.go there), over newHeadRuleRig.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

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

// jobResult is one result as the wire carries it, read apart from the server's own type, so
// each test reads the names a client reads.
type jobResult struct {
	ID              string      `json:"id"`
	IssueKey        string      `json:"issue_key"`
	CheckIssueKey   string      `json:"check_issue_key"`
	CheckBlockID    string      `json:"check_block_id"`
	Role            string      `json:"role"`
	Run             string      `json:"run"`
	Surface         string      `json:"surface"`
	NegativeControl string      `json:"negative_control"`
	Branch          string      `json:"branch"`
	Session         string      `json:"session"`
	Actor           model.Actor `json:"actor"`
	CommitSHA       string      `json:"commit_sha"`
	Verdict         string      `json:"verdict"`
	Observed        string      `json:"observed"`
	CreatedAt       time.Time   `json:"created_at"`
}

// jobRecorded decodes a 201 answer of the record route, failing the test on any other.
func jobRecorded(t *testing.T, what string, response *httptest.ResponseRecorder) jobResult {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s, want 201", what, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Result jobResult `json:"result"`
	}](t, response).Result
}

// passStandsFrom fails the test unless the check's pass_stands is answer, from roles.
func passStandsFrom(t *testing.T, check model.CheckBlock, what, answer string, roles ...string) {
	t.Helper()
	if check.PassStands.Answer != answer || !slices.Equal(check.PassStands.Roles, roles) {
		t.Fatalf("%s: %s answers pass_stands %+v, want %s from %v", what, check.BlockID, check.PassStands, answer, roles)
	}
}

// TestAMergedHeadStaysABasisWhileAPullRequestIsOpen: ACME-1's pull request #69 merged at
// ciJobCommit, and #70 is open at ciJobHead. k1 compares with both heads: the merged one stays a
// basis and is no subject. A tester's pass at the merged head stands at that basis, and no pass of
// k1 stands, since the open head, the one subject, holds no result.
func TestAMergedHeadStaysABasisWhileAPullRequestIsOpen(t *testing.T) {
	t.Parallel()
	rig := newHeadRuleRig(t)
	writeHead(t, rig.database, ciJobRepository, 69, ciJobCommit, "merged", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	if recorded := recordResult(t, rig.handler, rig.issue, resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})); recorded.Code != http.StatusCreated {
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
	t.Parallel()
	rig := newHeadRuleRig(t)
	writeHead(t, rig.database, ciJobRepository, 69, ciJobCommit, "merged", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "merged", rig.issue)
	older := jobRecorded(t, "the tester's pass of k1 at #69's head", recordResult(t, rig.handler, rig.issue,
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

	jobRecorded(t, "the control: the tester's pass of k1 at #70's head", recordResult(t, rig.handler, rig.issue,
		resultBody("k1", "pass", ciJobHead, map[string]any{"role": "tester"})))
	stands = passStandsOnWire(t, rig.handler, rig.issue, "k1")
	if subject := subjectAt(t, stands, ciJobHead); stands.Answer != model.PassStandsYes || len(stands.Subjects) != 1 || subject.Answer != model.PassStandsYes {
		t.Fatalf("the control: with a pass at #70's head k1 reads %+v, want yes at its one subject", stands)
	}
	passStandsFrom(t, checkOf(t, readResults(t, rig.handler, rig.issue), "k1"), "the control", model.PassStandsYes, "tester")
}
