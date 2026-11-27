// check_states_ci_population_test.go holds the settler to the evidence each state is computed from
// (deriveCIResults, eachSubjectStands): the state it publishes at an open head is the state GET
// /api/v1/issues/{key}/results answers there, from the same results, whatever the run of a check CI
// proves says at a former head, and the result a former head's own run derives counts at that
// former head alone. A run's conclusion is its whole job's at a former head as at an open one, so
// where the check's CI job recorded its own result of the check at a former head, the run derives
// none there. The rig is check_states_test.go's, on resultsGitHubServed's server or newCIJobRig's.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// checkStatesRigOn is a checkStatesRig of issue on a server a test built itself (served, whose
// handler and database they are), with a settler of its own.
func checkStatesRigOn(t *testing.T, handler http.Handler, database *store.Store, served *server, issue string) *checkStatesRig {
	t.Helper()
	callers, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	r := &checkStatesRig{
		ciJobRig: &ciJobRig{brokerTokenRig: &brokerTokenRig{handler: handler, database: database, served: served, deps: served.deps}, issue: issue},
		callers:  callers,
	}
	r.settler = r.newSettler(t, "check states")
	served.checkStatesSchedule = checkStatesTestSchedule
	return r
}

// routedHead is one open head of a test: the repository of its pull request and its commit.
type routedHead struct{ repo, head string }

// wantOpenHeadsAsRouted fails the test unless, at each of heads, the state of c1 the settler
// published is the subject GET /api/v1/issues/{key}/results answers there: its answer, its reason
// and its CI case. The route names a subject's repository on its CI read (model.CheckCI.Repo).
func wantOpenHeadsAsRouted(t *testing.T, what string, rig *checkStatesRig, heads ...routedHead) {
	t.Helper()
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s: read the results of %s: status=%d body=%s", what, rig.issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID    string                `json:"block_id"`
			PassStands model.CheckPassStands `json:"pass_stands"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("%s: decode the results of %s: %v", what, rig.issue, err)
	}
	var subjects []model.PassStandsSubject
	for _, check := range read.Checks {
		if check.BlockID == "c1" {
			subjects = check.PassStands.Subjects
		}
	}
	for _, head := range heads {
		var routed *model.PassStandsSubject
		for index := range subjects {
			if subject := &subjects[index]; subject.Basis.Commit == head.head && subject.CI != nil && subject.CI.Repo == head.repo {
				routed = subject
			}
		}
		if routed == nil {
			t.Fatalf("%s: the route answers no subject of c1 at %s of %s: %+v", what, head.head[:6], head.repo, subjects)
		}
		row, held := publishedRow(t, rig.database, rig.issue, head.repo, head.head, "c1")
		if !held {
			t.Fatalf("%s: no state of c1 at %s of %s is published, and the route answers %s/%s", what, head.head[:6], head.repo, routed.Answer, routed.Reason)
		}
		if row.state != routed.Answer || row.reason != routed.Reason || row.ciCase != routed.CI.Case {
			t.Fatalf("%s: the settler published c1 at %s of %s as %s/%s (CI case %q), and the route answers %s/%s (CI case %q)",
				what, head.head[:6], head.repo, row.state, row.reason, row.ciCase, routed.Answer, routed.Reason, routed.CI.Case)
		}
	}
}

// TestAFormerHeadsRunNeverChangesTheStateOfAnOpenHead: c1 is a check CI proves. acme/widgets#70 and
// acme/infra#71 are open at one commit, H1, where #70's run goes on and #71's run failed. #70 moves
// to H2, and then its run at H1 passes. At every step the state the settler publishes at each open
// head is the one the route answers there, so #71's state at H1 stays no for its fail; the former
// head of #70 at H1, whose own run passed at a commit where #71's failed, reads unknown for
// pass_and_fail.
func TestAFormerHeadsRunNeverChangesTheStateOfAnOpenHead(t *testing.T) {
	const widgets, infra = "acme/widgets", "acme/infra"
	github := newCIEachHeadGitHub()
	handler, database, served := resultsGitHubServed(t, map[string]bool{widgets: true, infra: true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, github.serve)
	issue := issueInRepository(t, handler, "POPUL", widgets, "A former head's run and an open head's state")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciEachHeadVerification)
	rig := checkStatesRigOn(t, handler, database, served, issue.Key)

	github.set(widgets, eachHeadH1, pendingRun(501, "go-checks", "in_progress"))
	github.set(widgets, eachHeadH2, completedRun(502, "go-checks", "success"))
	github.set(infra, eachHeadH1, completedRun(503, "go-checks", "failure"))
	writeHead(t, database, widgets, 70, eachHeadH1, "open", issue.Key)
	writeHead(t, database, infra, 71, eachHeadH1, "open", issue.Key)
	settleOwed(t, rig)
	wantOpenHeadsAsRouted(t, "#70 and #71 at H1", rig, routedHead{widgets, eachHeadH1}, routedHead{infra, eachHeadH1})

	writeHead(t, database, widgets, 70, eachHeadH2, "open", issue.Key)
	settleOwed(t, rig)
	wantOpenHeadsAsRouted(t, "#70 moved to H2", rig, routedHead{widgets, eachHeadH2}, routedHead{infra, eachHeadH1})

	github.set(widgets, eachHeadH1, completedRun(501, "go-checks", "success"))
	if _, err := database.Pool.Exec(context.Background(), `select check_states_owe($1)`, issue.Key); err != nil {
		t.Fatalf("mark %s owed as the settlement of a pull request's checks does: %v", issue.Key, err)
	}
	settleOwed(t, rig)
	wantOpenHeadsAsRouted(t, "#70's run at H1 passed after the move", rig, routedHead{widgets, eachHeadH2}, routedHead{infra, eachHeadH1})
	former, held := publishedRow(t, database, issue.Key, widgets, eachHeadH1, "c1")
	if !held || former.state != model.PassStandsUnknown || former.reason != model.PassStandsReasonPassAndFail {
		t.Fatalf("c1 at H1, a former head of #70 whose own run passed beside #71's fail, is published as %+v, want unknown/pass_and_fail", former)
	}
}

// wantPublishedAt fails the test unless the settler publishes check at head of the rig's
// repository as state, for reason, with the CI case ciCase.
func wantPublishedAt(t *testing.T, what string, rig *checkStatesRig, head, check, state, reason, ciCase string) {
	t.Helper()
	row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, head, check)
	published := "no state"
	if held {
		published = row.state
	}
	if published != state || row.reason != reason || row.ciCase != ciCase {
		t.Fatalf("%s: %s at %s is published as %s/%s with the CI case %q (row held %t), want %s/%s with the CI case %q",
			what, check, head[:6], published, row.reason, row.ciCase, held, state, reason, ciCase)
	}
}

// TestAJobsOwnResultAtAFormerHeadTakesThePlaceOfItsRunsConclusion: c1 and c2 are both proved by
// the go-checks job. At H1, the head of #70, the job recorded a pass of c1 and nothing of c2, and
// its run there failed, as it does when c2's test fails: c1 reads yes at H1 from the job's own
// pass, with the CI case job_recorded, and c2 no from the run's fail. #70 moves to H2, where the
// job's run goes on, and H1 is a former head of #70 whose run the settler still reads. There too
// the run's conclusion is the whole job's, so c1 at H1 still follows the job's pass: a resolution
// that took the job's own results at the open heads alone would publish c1 at H1 unknown for a
// pass and a fail, from the job's pass and the run's fail. c2 at H1 stays no, from the run.
func TestAJobsOwnResultAtAFormerHeadTakesThePlaceOfItsRunsConclusion(t *testing.T) {
	twoProved := ciJobVerification + "\n" + ciCheckBlock("c2", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`)
	rig := newCheckStatesRig(t, twoProved)
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: eachHeadH1})
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	jobRecorded(t, "the job's pass of c1 at H1", rig.post(t, rig.issue,
		jobTokenOn(rig.ciJobRig, eachHeadH1, "refs/heads/legion/work", 99), jobResultAt(eachHeadH1, "pass")))
	rig.github.listings.set(eachHeadH1, completedRun(11, ciJobName, "failure"))
	settleOwed(t, rig)
	wantPublishedAt(t, "#70 at H1", rig, eachHeadH1, "c1", model.PassStandsYes, "", ciCaseJobRecorded)
	wantPublishedAt(t, "#70 at H1", rig, eachHeadH1, "c2", model.PassStandsNo, model.PassSubjectReasonFail, "failure")

	rig.github.listings.set(eachHeadH2, pendingRun(12, ciJobName, "in_progress"))
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: eachHeadH2})
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	settleOwed(t, rig)
	wantPublishedAt(t, "#70 moved to H2", rig, eachHeadH2, "c1", model.PassStandsNo, model.PassSubjectReasonNoResult, ciCaseNotCompleted)
	wantPublishedAt(t, "#70 moved to H2", rig, eachHeadH1, "c1", model.PassStandsYes, "", ciCaseJobRecorded)
	wantPublishedAt(t, "#70 moved to H2", rig, eachHeadH1, "c2", model.PassStandsNo, model.PassSubjectReasonFail, "failure")

	if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
		t.Fatalf("mark %s owed as the settlement of a pull request's checks does: %v", rig.issue, err)
	}
	settleOwed(t, rig)
	wantPublishedAt(t, "the run at H1 read again after the move", rig, eachHeadH1, "c1", model.PassStandsYes, "", ciCaseJobRecorded)
	wantPublishedAt(t, "the run at H1 read again after the move", rig, eachHeadH1, "c2", model.PassStandsNo, model.PassSubjectReasonFail, "failure")
}

// TestAJobsResultAtAFormerHeadWhoseRepositoryDidNotResolveIsAFailedRead: at H1, the head of #70,
// the go-checks job recorded a pass of c1, a result that names no repository and so is of the
// issue's own, and its run there failed; #70 moves to H2, and H1 is a former head whose run the
// settler still reads. Then the read of the repository the issue resolves to stops answering
// (ACME-1's parent chain loops, which refs.IssueRepository refuses to resolve), so whether the
// job's pass is of H1's repository, and takes the run's place there, is not known. At a former
// head as at an open one that is a failed read: c1 at H1 is unread for read_failed and the run
// derives no fail there. A resolution that took this case at the open heads alone computes c1 at
// H1 unknown for a pass and a fail, from the job's pass and the run's fail, which an architect
// acts on at once.
func TestAJobsResultAtAFormerHeadWhoseRepositoryDidNotResolveIsAFailedRead(t *testing.T) {
	rig := newCheckStatesRig(t, ciJobVerification)
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: eachHeadH1})
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	jobRecorded(t, "the job's pass of c1 at H1", rig.post(t, rig.issue,
		jobTokenOn(rig.ciJobRig, eachHeadH1, "refs/heads/legion/work", 99), jobResultAt(eachHeadH1, "pass")))
	rig.github.listings.set(eachHeadH1, completedRun(11, ciJobName, "failure"))
	rig.github.listings.set(eachHeadH2, pendingRun(12, ciJobName, "in_progress"))
	rig.github.setBranch("legion/work", ciJobBranchHead{sha: eachHeadH2})
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	settleOwed(t, rig)
	wantPublishedAt(t, "#70 moved to H2", rig, eachHeadH1, "c1", model.PassStandsYes, "", ciCaseJobRecorded)

	top := anotherIssue(t, rig.ciJobRig)
	for _, link := range [][2]string{{rig.issue, top}, {top, rig.issue}} {
		if _, err := rig.database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, link[0], link[1]); err != nil {
			t.Fatalf("make %s the parent of %s: %v", link[0], link[1], err)
		}
	}
	subject := computedSubject(t, rig, "c1", eachHeadH1)
	if subject.Answer != model.PassStandsUnknown || subject.Reason != model.PassSubjectReasonReadFailed ||
		subject.CI == nil || subject.CI.State != model.CIStateUnread || subject.CI.Case != model.ReasonReadFailed {
		t.Fatalf("c1 at H1, a former head whose job result's repository did not resolve, computes as %s/%s with the CI read %+v, want unknown/read_failed, unread for read_failed",
			subject.Answer, subject.Reason, subject.CI)
	}
	for _, id := range subject.Results {
		if id == "ci-run:11" {
			t.Fatalf("c1 at H1 counts the fail the failing run derived there, %v: a read that did not answer derives no state", subject.Results)
		}
	}
}
