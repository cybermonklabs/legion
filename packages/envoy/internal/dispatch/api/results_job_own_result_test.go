// results_job_own_result_test.go holds GET /api/v1/issues/{key}/results to R12 of The Spec: the
// result of a whole job decides no check. A check run's conclusion is its whole job's, so at a head
// where a check's CI job recorded a result of its own for the check, the run derives the check no
// result there, and the check's ci names the case job_recorded. The tests read the route's answer
// on the wire and name the cases as strings, so they build at a commit before the rule and fail
// there on their own assertions.
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAJobsFailOnAnotherCheckLeavesThePassItRecorded: c1 and c2 are both proved by the go-checks
// job. At the head of ACME-1's open pull request #70 the job recorded a pass of c1 and nothing of
// c2, and the run failed, as it does when c2's test fails. c1 reads yes, from the job's own pass,
// and c2 reads no, from the run's fail.
func TestAJobsFailOnAnotherCheckLeavesThePassItRecorded(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	twoProved := ciJobVerification + "\n" + ciCheckBlock("c2", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`)
	if written := uploadVerification(t, rig.handler, rig.issue, twoProved); written.Code != http.StatusCreated {
		t.Fatalf("write c1, k1 and c2: status=%d body=%s", written.Code, written.Body.String())
	}
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: ciJobHead})
	jobRecorded(t, "the job's pass of c1 at the pull request's head", rig.post(t, rig.issue,
		jobTokenOn(rig, ciJobHead, "refs/heads/legion/work", 99), jobResultAt(ciJobHead, "pass")))
	rig.github.listings.set(ciJobHead, completedRun(11, ciJobName, "failure"))

	c1 := passStandsOnWire(t, rig.handler, rig.issue, "c1")
	if subject := subjectAt(t, c1, ciJobHead); c1.Answer != model.PassStandsYes || subject.Answer != model.PassStandsYes {
		t.Fatalf("c1, which the job passed at %s, reads %+v, its subject %+v; want yes at both", ciJobHead, c1, subject)
	}
	c2 := passStandsOnWire(t, rig.handler, rig.issue, "c2")
	if subject := subjectAt(t, c2, ciJobHead); c2.Answer != model.PassStandsNo || subject.Answer != model.PassStandsNo || subject.Reason != "fail" {
		t.Fatalf("c2, which the job recorded nothing of at %s where its run failed, reads %+v, its subject %+v; want no, and no (fail) at the subject",
			ciJobHead, c2, subject)
	}
	if ci := checkOf(t, readResults(t, rig.handler, rig.issue), "c1").CI; ci == nil || ci.State != model.CIStateNoResult || ci.Case != "job_recorded" || ci.Result != "" {
		t.Fatalf("c1's ci is %+v, want no_result for job_recorded, naming no derived result", ci)
	}
}

// TestTheJobsOwnResultAtAHeadTakesThePlaceOfItsRunsConclusion: ACME-1's open pull request #70 is
// at ciJobHead. Where the job recorded its own result of c1 at that head, the job's result decides
// the head whatever the go-checks run there concluded, and c1's ci names the case job_recorded.
// The controls: with no result of the job, the run's conclusion decides; with the job's result
// alone and no run, the job's result decides; and once the job's result is retracted, the run's
// conclusion decides again.
func TestTheJobsOwnResultAtAHeadTakesThePlaceOfItsRunsConclusion(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, job, derived string
		retract            bool
		answer, reason     string
		ciCase             string
	}{
		{"the job's fail beside a passing run", "fail", ciConclusionSuccess, false, model.PassStandsNo, "fail", "job_recorded"},
		{"the job's pass beside a failing run", "pass", "failure", false, model.PassStandsYes, "", "job_recorded"},
		{"the job's pass beside a passing run", "pass", ciConclusionSuccess, false, model.PassStandsYes, "", "job_recorded"},
		{"the control: the job's fail alone", "fail", "", false, model.PassStandsNo, "fail", "absent"},
		{"the control: a failing run and no result of the job", "", "failure", false, model.PassStandsNo, "fail", "failure"},
		{"the control: the job's pass retracted beside a failing run", "pass", "failure", true, model.PassStandsNo, "fail", "failure"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCIJobRig(t)
			writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
			rig.github.setBranch("legion/work", ciJobBranchHead{sha: ciJobHead})
			if row.job != "" {
				recorded := jobRecorded(t, "the job's "+row.job+" at the pull request's head", rig.post(t, rig.issue,
					jobTokenOn(rig, ciJobHead, "refs/heads/legion/work", 99), jobResultAt(ciJobHead, row.job)))
				if row.retract {
					if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/results/"+recorded.ID+"/retract",
						map[string]any{"reason": "Recorded against the wrong build."}, "alice"); response.Code != http.StatusCreated {
						t.Fatalf("retract the job's %s: status=%d body=%s", row.job, response.Code, response.Body.String())
					}
				}
			}
			if row.derived != "" {
				rig.github.listings.set(ciJobHead, completedRun(11, ciJobName, row.derived))
			}
			stands := passStandsOnWire(t, rig.handler, rig.issue, "c1")
			subject := subjectAt(t, stands, ciJobHead)
			if stands.Answer != row.answer || subject.Answer != row.answer || subject.Reason != row.reason {
				t.Fatalf("c1 reads %+v, its subject at %s %+v; want %s, and %s (reason %q) at the subject",
					stands, ciJobHead, subject, row.answer, row.answer, row.reason)
			}
			if ci := checkOf(t, readResults(t, rig.handler, rig.issue), "c1").CI; ci == nil || ci.Case != row.ciCase {
				t.Fatalf("c1's ci is %+v, want the case %s", ci, row.ciCase)
			}
		})
	}
}

// TestAJobsResultWhoseRepositoryDidNotResolveIsAFailedRead: the job recorded a pass of c1 at the
// head of ACME-1's open pull request #70, a result that names no repository and so is of the
// issue's own, and the run there failed. Whether that result takes the run's place turns on the
// repository the issue resolves to, and the read of it does not answer: ACME-1's parent chain
// loops, which no API write can make, and refs.IssueRepository refuses to resolve a chain that
// loops. A read that did not answer gives no state: c1 is unread at the head (read_failed), and
// the run derives it no fail, so the head reads neither the pass and the fail of a database
// error nor anything a reviewer acts on (R15). The control is the row the job's pass beside a
// failing run of TestTheJobsOwnResultAtAHeadTakesThePlaceOfItsRunsConclusion, whose resolve answers.
func TestAJobsResultWhoseRepositoryDidNotResolveIsAFailedRead(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: ciJobHead})
	jobRecorded(t, "the job's pass of c1 at the pull request's head", rig.post(t, rig.issue,
		jobTokenOn(rig, ciJobHead, "refs/heads/legion/work", 99), jobResultAt(ciJobHead, "pass")))
	rig.github.listings.set(ciJobHead, completedRun(11, ciJobName, "failure"))
	top := anotherIssue(t, rig)
	for _, link := range [][2]string{{rig.issue, top}, {top, rig.issue}} {
		if _, err := rig.database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, link[0], link[1]); err != nil {
			t.Fatalf("make %s the parent of %s: %v", link[0], link[1], err)
		}
	}

	c1 := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	if ci := c1.CI; ci == nil || ci.State != model.CIStateUnread || ci.Case != model.ReasonReadFailed || ci.Commit != ciJobHead ||
		!strings.Contains(ci.Detail, rig.issue) {
		t.Fatalf("c1's ci at %s is %+v, want unread for read_failed, its detail naming %s", ciJobHead, ci, rig.issue)
	}
	for _, result := range c1.Results {
		if result.ID == "ci-run:11" {
			t.Fatalf("c1 holds the fail the failing run derived at %s, %+v: a read that did not answer must derive no state", ciJobHead, result)
		}
	}
	if c1.PassStands.Answer == model.PassStandsYes || c1.PassStands.Reason == model.PassStandsReasonPassAndFail {
		t.Fatalf("c1 reads %+v, want no yes and no pass and fail where the read did not answer", c1.PassStands)
	}
	for _, subject := range c1.PassStands.Subjects {
		if subject.Basis.Commit == ciJobHead && subject.Reason == model.PassStandsReasonPassAndFail {
			t.Fatalf("c1's head %s reads %+v, a pass and a fail, where the read did not answer", ciJobHead, subject)
		}
	}
}

// storeJobResult stores a pass of c1 as a row of ci_check_results the way the record route stores
// a CI job's, with run, commit, check digest and repository given, so each condition under which
// a job's result takes the run's place at a head is put to the read on its own.
func storeJobResult(t *testing.T, rig *ciJobRig, run, commit, digest string, repo *string) {
	t.Helper()
	check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	if digest == "" {
		digest = check.SHA256
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into ci_check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256, run, commit_sha, branch, verdict, attestation, session, actor, commit_repo)
		values ($1, $1, $2::uuid, 'c1', $3, $4, $5, $6, 'legion/work', 'pass', '{}', $7, '{"kind":"session","id":"ci"}', $8)
	`, rig.issue, check.ArtifactID, check.Version, digest, run, commit, ciJobSession, repo); err != nil {
		t.Fatalf("store the job's pass of c1: %v", err)
	}
}

// TestOnlyAJobsResultThatCountsAtTheHeadTakesThePlaceOfItsRun holds each condition of
// jobResultHeads: ACME-1's open pull request #70 of the job's repository is at ciJobHead, where
// go-checks failed, and the job's pass of c1 stands in one way each row. Only the job's own
// result, under the check's words now, at the head's commit and of the head's repository, takes
// the run's place (job_recorded); in every other row the run decides the head, and c1 holds the
// fail it derived there. The row of another commit is a job's pass at an old commit, which must
// never hide a failing run at the new head.
func TestOnlyAJobsResultThatCountsAtTheHeadTakesThePlaceOfItsRun(t *testing.T) {
	t.Parallel()
	other := "acme/another-repository"
	for _, row := range []struct {
		name, run, commit, digest string
		repo                      *string
		ciCase                    string
	}{
		{"the control: the job's pass at the head", ciJobName, ciJobHead, "", nil, "job_recorded"},
		{"a pass at another commit than the head's", ciJobName, ciJobOtherCommit, "", nil, "failure"},
		{"a pass under the check's old words", ciJobName, ciJobHead, strings.Repeat("0", 64), nil, "failure"},
		{"a pass of another repository at the head's commit", ciJobName, ciJobHead, "", &other, "failure"},
		{"another job's pass at the head", otherCIJob, ciJobHead, "", nil, "failure"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCIJobRig(t)
			writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
			rig.github.setBranch("legion/work", ciJobBranchHead{sha: ciJobHead})
			rig.github.listings.set(ciJobHead, completedRun(11, ciJobName, "failure"))
			storeJobResult(t, rig, row.run, row.commit, row.digest, row.repo)

			c1 := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
			if ci := c1.CI; ci == nil || ci.Commit != ciJobHead || ci.Case != row.ciCase {
				t.Fatalf("c1's ci at %s is %+v, want the case %s", ciJobHead, ci, row.ciCase)
			}
			derived := false
			for _, result := range c1.Results {
				derived = derived || (result.ID == "ci-run:11" && result.Verdict == "fail")
			}
			if want := row.ciCase == "failure"; derived != want {
				t.Fatalf("c1 holds the fail run 11 derived at %s: %v, want %v; its results %+v", ciJobHead, derived, want, c1.Results)
			}
		})
	}
}
