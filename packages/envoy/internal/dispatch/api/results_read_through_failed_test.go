package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A read of a commit's pull requests that GitHub fails is no answer about the commit, so a result
// there that no basis names is not known (model.ReasonReadFailed), never another commit. On
// 2026-11-27 the REST list of 64 commits of one repository passed the GitHub client's cap on a
// response, 18 reads each in under two hours, and each read answered every result at those
// commits as `other_commit`: a check whose issue held no pull request Dispatch knew of answered
// pass_stands `no` for a pass that GitHub would have placed at the issue's pull request.
//
// The failed read keeps the claim it took for delivery.CommitPullReadStaleAfter, so the answer
// holds for that span too: on the read GitHub failed, on a second read of the same process inside
// the span, which asks GitHub nothing, and on a read of a second process, which the claim every
// process shares refuses. Once the span has passed, a read GitHub answers places the result as
// the answer says, and so does a read inside the span that read took.
//
// Each case records a pass at headCommit while GitHub names no pull request for it, so the
// record's own read-through stores nothing and the check compares with nothing, and then makes
// GitHub fail the read of headCommit: the batched query answered 502, and a pull request at the
// commit whose answer alone passes the cap, so no page of the REST list it falls back to can be
// read (delivery.ErrCommitPullsIncomplete), which is the production failure. The control case
// lets GitHub answer, naming nothing: the result stands at another commit on every read, as it
// did before this row.
func TestAResultAtACommitGitHubFailsIsNotKnownForTheClaimsSpan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// fail makes GitHub fail the read of sha's pull requests for issueKey, and answers what
		// lifts the failure, after which GitHub names no pull request for sha.
		fail       func(fake *commitPullsFake, sha, issueKey string) (lift func())
		stands     string
		reason     string
		passStands string
	}{
		{
			name: "GitHub answers the query 502",
			fail: func(fake *commitPullsFake, sha, _ string) func() {
				fake.refusesCommitPulls(sha, http.StatusBadGateway)
				return func() { fake.answersCommitPullsAgain(sha) }
			},
			stands: model.CommitUnknown, reason: model.ReasonReadFailed, passStands: model.PassStandsUnknown,
		},
		{
			name: "one pull request at the commit passes the response cap",
			fail: func(fake *commitPullsFake, _, issueKey string) func() {
				fake.answers(pullPastTheCap(95, issueKey))
				return func() { fake.answers(func(string, string) string { return `[]` }) }
			},
			stands: model.CommitUnknown, reason: model.ReasonReadFailed, passStands: model.PassStandsUnknown,
		},
		{
			name:   "control: GitHub names no pull request",
			fail:   func(*commitPullsFake, string, string) func() { return func() {} },
			stands: model.CommitOtherCommit, passStands: model.PassStandsNo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			otherHandler, other := secondReadThroughServer(t, database, fake)
			issue := issueInRepository(t, handler, "RESRF", "acme/widgets", "A read GitHub fails")
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
			pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
			lift := tc.fail(fake, headCommit, issue.Key)

			read := func(handler http.Handler, what, stands, reason, passStands string, asks func(int) bool) {
				t.Helper()
				spent := fake.asks("acme/widgets")
				results := readResults(t, handler, issue.Key)
				asked := fake.asks("acme/widgets") - spent
				check, got := checkOf(t, results, "k1"), resultRead(t, results, "k1", pass.Result.ID).CommitStands
				t.Logf("%s: GitHub asked %d times; the pass %+v; pass_stands %q; compared nothing %q",
					what, asked, got, check.PassStands.Answer, check.ComparedNothing)
				if !asks(asked) {
					t.Fatalf("%s asked GitHub %d times", what, asked)
				}
				if got == nil || got.Answer != stands || got.Reason != reason {
					t.Fatalf("%s places the pass %+v, want %s with the reason %q", what, got, stands, reason)
				}
				if check.PassStands.Answer != passStands {
					t.Fatalf("%s answers pass_stands %q, want %q", what, check.PassStands.Answer, passStands)
				}
			}
			asked := func(asked int) bool { return asked > 0 }
			none := func(asked int) bool { return asked == 0 }

			read(handler, "the read that asks GitHub", tc.stands, tc.reason, tc.passStands, asked)
			read(handler, "a second read of the process inside the span", tc.stands, tc.reason, tc.passStands, none)
			read(otherHandler, "a read of a second process inside the span", tc.stands, tc.reason, tc.passStands, none)

			// GitHub answers again, naming no pull request, and the span passes for both processes.
			// The read granted after it reads the commit through, and the claim it takes is no failed
			// read: a read the claim refuses places the result as the answer did.
			lift()
			endCommitPullReadSpans(t, served, other)
			read(handler, "the read after the span", model.CommitOtherCommit, "", model.PassStandsNo, asked)
			read(otherHandler, "a read of the second process inside the new span", model.CommitOtherCommit, "", model.PassStandsNo, none)
		})
	}
}

// pullPastTheCap is GitHub's answer for a commit that heads one open pull request titled after
// issueKey, whose body alone passes the GitHub client's cap on a response (1 MiB): neither the
// batched query nor any page of the REST list, however small, can carry it.
func pullPastTheCap(number int, issueKey string) func(repo, sha string) string {
	body := strings.Repeat("x", 1<<20)
	return func(repo, sha string) string {
		return fmt.Sprintf(`[{"number":%d,"title":%q,"body":%q,"html_url":"https://github.com/%s/pull/%d",
			"state":"open","merged":false,"updated_at":"2026-11-27T19:30:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			number, issueKey+" the work", body, repo, number, sha, repo)
	}
}

// endCommitPullReadSpans is what the clock does to every claim once
// delivery.CommitPullReadStaleAfter has passed, without the test waiting for it: each server's
// memory of its claims is empty, and each claim every process shares holds a moment that has just
// passed. Unlike rewindCommitPullReads, the rows stay, so the claim granted next is granted over
// the row a failed read marked.
func endCommitPullReadSpans(t *testing.T, served ...*server) {
	t.Helper()
	for _, server := range served {
		server.commitPulls = delivery.CommitPullReads{}
	}
	if _, err := served[0].deps.Store.Pool.Exec(context.Background(),
		`update commit_pull_reads set not_before = $1`, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("end the span of the claims of the reads of commits: %v", err)
	}
}
