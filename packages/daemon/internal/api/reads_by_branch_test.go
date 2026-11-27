package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// branchGitHub is GitHub's REST API for acme/widgets as an implementer's completion reads it: the
// list of the open pull requests on the branch the read asks for, each listed in the order given,
// which is GitHub's own when it is newest first. It answers no other path.
func branchGitHub(t *testing.T, branch string, listed ...openPull) *handoffGitHub {
	t.Helper()
	pulls := make([]string, len(listed))
	for i, pull := range listed {
		pulls[i] = fmt.Sprintf(`{"number":%d,"state":"open","html_url":"https://github.com/acme/widgets/pull/%d","head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":%q},"updated_at":"2026-11-16T21:54:48Z"}`,
			pull.number, pull.number, branch, headSHA, pull.base)
	}
	url := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" {
			githubtest.Unexpected(t, w, r)
			return
		}
		if query := r.URL.Query(); query.Get("head") != "acme:"+branch || query.Get("state") != "open" {
			t.Errorf("listed pull requests by %q, want the open ones on acme:%s", r.URL.RawQuery, branch)
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(pulls, ",")+"]")
	}})
	return &handoffGitHub{url: url}
}

// Every read that has only a branch takes each open pull request on it
// (dispatch://INFRA-5910/spec#b-d1: "A push, which carries a branch and no number, applies to each
// open pull request on that branch, and so does each read that has only a branch"). This is the
// API's part of the inventory of those reads, row b10 of the plan's decision 20; the workflow's
// (b1 to b5), the daemon's (b6 to b9) and ghbranch's (b11) hold a test of this name in their own
// packages. The implementer's completion reads the open pull requests on its task's branch
// (implementationPullRequest) and takes the task's own pull request among them by the rule of
// decision 5: of those open, the first opened whose base is the task's base. The issue's one task,
// a, works on legion/<KEY> onto main, and its branch holds two open pull requests, as INFRA-1276's
// branch in the deployment repository held #3654 onto main and the draft #3955, opened after it,
// onto legion/INFRA-1276-ci-base; GitHub lists them newest first.
//
//   - b10: the record holds the draft alone, as an earlier daemon wrote it in place of #3654. The
//     completion is taken on #3654: the opened its webhook would have carried is applied, an opened
//     and no reopen, then the completion of task a, and nothing names the draft, which keeps its
//     own record.
//   - The issue links the task's own pull request, and a pull request of another repository on a
//     branch of the same name (decision 5: a task's own pull request stays its own when the issue
//     links it, and a pull request of another repository is no task's). The record holds both
//     open, so the completion is taken on #3654 as the record holds it and applies nothing else.
//
// The fault it catches is the read that takes the first open pull request GitHub lists on the
// branch whatever its base (the first of githubrest.Client.OpenPullRequestsOn's): it takes the
// draft, against a base the plan does not give task a, and refuses each completion 409
// HANDOFF_PULL_REQUEST_BASE.
func TestEachReadByBranchTakesEveryOpenPullRequestOnIt(t *testing.T) {
	const branch, ciBase = "legion/LEGION-276", "legion/LEGION-276-ci-base"
	listed := []openPull{{number: 3955, base: ciBase}, {number: 3654, base: "main"}}
	draft := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-276", Repo: "acme/widgets", Number: 3955,
		Branch: branch, Base: ciBase, HeadSHA: headSHA, Failing: []string{}}
	own := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-276", Repo: "acme/widgets", Number: 3654,
		Branch: branch, Base: "main", HeadSHA: headSHA, Failing: []string{}}
	other := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-276", Repo: "acme/gadgets", Number: 12,
		Branch: branch, Base: "main", HeadSHA: headSHA, Failing: []string{}}
	for _, tc := range []struct {
		name     string
		recorded []record.PullRequest
		links    []record.PullRequestLink
		// opened is the number of the pull request whose opened the completion applies before it,
		// 0 for none.
		opened int
	}{
		{name: "b10 the task's own pull request among two open on its branch", recorded: []record.PullRequest{draft}, opened: 3654},
		{name: "the task's own pull request the issue links", recorded: []record.PullRequest{own, other},
			links: []record.PullRequestLink{{Issue: "LEGION-276", Repo: "acme/widgets", Number: 3654}, {Issue: "LEGION-276", Repo: "acme/gadgets", Number: 12}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, branchGitHub(t, branch, listed...), nil, nil)
			const key = "LEGION-276"
			seedIssueAt(t, h, key, phase.Implementing)
			seedTaskPlan(t, h, key, taskStanding{task: "a", state: record.TaskRunning, place: 1})
			if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
				records := record.NewStore()
				for _, pr := range tc.recorded {
					if err := records.PutPullRequest(ctx, tx, pr); err != nil {
						return err
					}
				}
				if len(tc.links) == 0 {
					return nil
				}
				return records.ReplacePullRequestLinks(ctx, tx, key, tc.links)
			}); err != nil {
				t.Fatalf("record the pull requests of %s: %v", key, err)
			}

			implementer := newLiveClaim(t, h, key, claim.RoleImplementer)
			written := implementer.writeHandoff("implement", 1)
			completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA, HandoffID: written, HandoffVersion: 1}))

			applied := facts.recorded()
			var opened []intake.PullRequestOpened
			for _, fact := range applied {
				if fact, ok := fact.(intake.PullRequestOpened); ok {
					opened = append(opened, fact)
				}
			}
			switch {
			case tc.opened == 0 && len(opened) != 0:
				t.Errorf("the completion applied %+v, want no opened: the record holds the task's own pull request open", opened)
			case tc.opened != 0 && (len(opened) != 1 || opened[0].Number != tc.opened || opened[0].Base != "main" ||
				opened[0].Branch != branch || opened[0].Reopened):
				t.Errorf("the completion applied the opened %+v, want one opened of #%d onto main on %s, and no reopen", opened, tc.opened, branch)
			}
			if fact := lastCompletion(t, facts); fact.Task != "a" || fact.Role != claim.RoleImplementer {
				t.Errorf("the completion applied %+v, want the implementer's completion of task a", fact)
			}
			if len(applied) != len(opened)+1 {
				t.Errorf("the completion applied %#v, want the opened of the task's own pull request where the record lacks it, then the completion, and nothing else", applied)
			}
		})
	}
}
