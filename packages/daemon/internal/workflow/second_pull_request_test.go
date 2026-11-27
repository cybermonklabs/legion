package workflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The replay of INFRA-1276, in a neutral repository: LEGION-1276 is a root of the deployment
// repository acme/deploy with no plan rows, so its own branch is its work's. GitHub held #3654 from
// that branch onto main, and then the draft #3955 from the same branch onto a scratch base.
const (
	replayIssue  = "LEGION-1276"
	replayRepo   = "acme/deploy"
	replayBranch = "legion/LEGION-1276"
	replayCIBase = "legion/LEGION-1276-ci-base"
)

// replayAt is GitHub's clock of the replay's first event.
var replayAt = time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)

// replayRoot seeds LEGION-1276 standing at at, generation 1, with its tree open and no plan rows.
func replayRoot(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: replayIssue, Repo: replayRepo, DefaultBranch: "main", GitHubRepo: replayRepo, Tree: replayIssue,
		Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: PhaseStatus(at), Rank: "U", LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, replayIssue)
}

// replayOpened is GitHub's open of pull request number from LEGION-1276's branch onto base, at
// minute minutes past replayAt, at the head both pull requests of the replay stood at.
func replayOpened(number int, base string, minute int) intake.PullRequestOpened {
	return intake.PullRequestOpened{Repo: replayRepo, Number: number, Branch: replayBranch, Base: base, HeadSHA: "a95b58b7", HeadRepo: replayRepo,
		UpdatedAt: replayAt.Add(time.Duration(minute) * time.Minute)}
}

// replayLinks is Dispatch's event of LEGION-1276 at seq, at status, that names the issue's links
// whole: a link to each of numbers in acme/deploy.
func replayLinks(seq int64, status string, numbers ...int) intake.DispatchIssue {
	links := []intake.PullRequestLink{}
	for _, number := range numbers {
		links = append(links, intake.PullRequestLink{Repo: replayRepo, Number: number})
	}
	return intake.DispatchIssue{Key: replayIssue, ResolvedRepo: replayRepo, Seq: seq, Type: "issue.updated", Status: status, Title: "root", Rank: "U",
		Links: links, LinksKnown: true}
}

// pullRequestSetOf is each pull request of key's set as the record reads it
// (record.Store.PullRequestSetsOf), as owner/name#number, in its order.
func pullRequestSetOf(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	var sets map[string][]record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		sets, err = record.NewStore().PullRequestSetsOf(t.Context(), tx, []string{key})
		return err
	})
	names := []string{}
	for _, pr := range sets[key] {
		names = append(names, fmt.Sprintf("%s#%d", pr.Repo, pr.Number))
	}
	return names
}

// pullRequestOf is pull request number of repo as the record reads it by its repository and number,
// nil where it holds no row of it.
func pullRequestOf(t *testing.T, pool *pgxpool.Pool, repo string, number int) *record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		pr, err = record.NewStore().PullRequestByNumber(t.Context(), tx, repo, number)
		return err
	})
	return pr
}

// mergeOf is the pull request of key's merge (record.Store.MergePullRequest), and mergesOf the
// number of each of its merges (MergePullRequests).
func mergeOf(t *testing.T, pool *pgxpool.Pool, key string) (*record.PullRequest, []int) {
	t.Helper()
	var merge *record.PullRequest
	var merges []int
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		var err error
		if merge, err = store.MergePullRequest(t.Context(), tx, key); err != nil {
			return err
		}
		prs, err := store.MergePullRequests(t.Context(), tx, key)
		for _, pr := range prs {
			merges = append(merges, pr.Number)
		}
		return err
	})
	return merge, merges
}

// A second pull request from an issue's branch replaces none (dispatch://INFRA-5910 D1), as the
// record of INFRA-1276 showed it did on 2026-11-16: read_record held the closed draft #3955 at the
// head of the deployment repository's #3654, which was open, and the close of the draft told the
// architect the issue's pull request closed unmerged. Here LEGION-1276 of acme/deploy, with no plan
// rows, has #3654 opened from legion/LEGION-1276 onto main and then the draft #3955 from the same
// branch onto legion/LEGION-1276-ci-base. Each row is one replay, on a record of its own:
//   - both rows stand, and #3654, the first opened, stays the issue's pull request (decision 5);
//   - the close of #3955 tells the architect that #3654 is still open (pr-closed-others-open) and
//     sends no pr-closed-unmerged, whose words say the issue's pull request closed (D5);
//   - a link of #3955 takes it into the set as linked, still another pull request of the issue;
//   - at #3654's merge in merging the linked #3955 holds the close: the issue stays in merging, the
//     stack's close does not close #3955, and one pr-holds-close names it with the way out, its
//     link's removal (decisions 9, 10 and 11);
//   - once its link is removed, #3654 is still the issue's merge, #3955 stays in the set as another
//     pull request of the issue, the issue closes, and the stack's close closes #3955 with the
//     comment that the issue is done (decisions 9, 11 and 18).
//
// The fault it catches is the record keyed by the issue and the branch, or by the issue, the
// repository and the branch, the two keys dispatch://INFRA-5910/comment/37e6f13e-cb9d-4c00-9ee1-bf090dd747e3
// offers: the open of #3955 takes the place of #3654, as read_record showed at 21:54:48Z.
func TestASecondPullRequestFromTheBranchReplacesNone(t *testing.T) {
	t.Parallel()
	draft := fmt.Sprintf("%s#%d", replayRepo, 3955)
	opened := func(t *testing.T, at phase.Phase) (*pgxpool.Pool, func(string, intake.Fact) intake.Result) {
		t.Helper()
		pool := migratedPool(t)
		replayRoot(t, pool, at)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		mustSpend(t, "the open of #3654", apply("opened-3654", replayOpened(3654, "main", 0)))
		mustSpend(t, "the open of #3955", apply("opened-3955", replayOpened(3955, replayCIBase, 5)))
		return pool, apply
	}

	t.Run("both stand, and #3654 stays the issue's pull request", func(t *testing.T) {
		pool, _ := opened(t, phase.Reviewing)
		for _, want := range []struct {
			number int
			base   string
		}{{3654, "main"}, {3955, replayCIBase}} {
			pr := pullRequestOf(t, pool, replayRepo, want.number)
			if pr == nil || pr.State != record.PullRequestOpen || pr.Base != want.base || pr.Issue != replayIssue {
				t.Errorf("#%d is %+v, want open onto %s for %s: a second pull request from %s replaces none", want.number, pr, want.base, replayIssue, replayBranch)
			}
		}
		if merge, _ := mergeOf(t, pool, replayIssue); merge == nil || merge.Number != 3654 {
			t.Errorf("the issue's pull request is %+v, want #3654, the first opened onto the task's base", merge)
		}
		if got, want := pullRequestSetOf(t, pool, replayIssue), []string{replayRepo + "#3654", draft}; !slices.Equal(got, want) {
			t.Errorf("the set of %s is %v, want %v", replayIssue, got, want)
		}
	})

	t.Run("the close of #3955 tells the architect #3654 is still open", func(t *testing.T) {
		pool, apply := opened(t, phase.Reviewing)
		mustSpend(t, "the close of #3955", apply("closed-3955", intake.PullRequestClosed{Repo: replayRepo, Number: 3955, HeadSHA: "a95b58b7",
			UpdatedAt: replayAt.Add(10 * time.Minute)}))
		if got := noticesOf(t, pool, replayIssue, "pr-closed-unmerged"); len(got) != 0 {
			t.Errorf("the close of #3955 sent pr-closed-unmerged %+v, want none: #3654, the issue's pull request, is open", got)
		}
		others := noticesOf(t, pool, replayIssue, "pr-closed-others-open")
		if len(others) != 1 || !strings.Contains(others[0].Reason, replayRepo+"#3654") {
			t.Errorf("the close of #3955 sent pr-closed-others-open %+v, want one that names %s#3654 as still open", others, replayRepo)
		}
		if got := storedIssue(t, pool, replayIssue); got.Phase != phase.Reviewing {
			t.Errorf("after the close of #3955 %s stands in %s, want reviewing: the close moves nothing", replayIssue, got.Phase)
		}
	})

	t.Run("a link of #3955 takes it into the set as linked", func(t *testing.T) {
		pool, apply := opened(t, phase.Reviewing)
		mustSpend(t, "the link of #3955", apply("linked-3955", replayLinks(2, PhaseStatus(phase.Reviewing), 3955)))
		if pr := pullRequestOf(t, pool, replayRepo, 3955); pr == nil || !pr.Linked {
			t.Errorf("#3955 is %+v, want it linked", pr)
		}
		if merge, _ := mergeOf(t, pool, replayIssue); merge == nil || merge.Number != 3654 {
			t.Errorf("the issue's pull request is %+v, want #3654: a link makes no pull request the task's own", merge)
		}
		if got, want := pullRequestSetOf(t, pool, replayIssue), []string{replayRepo + "#3654", draft}; !slices.Equal(got, want) {
			t.Errorf("the set of %s is %v, want %v", replayIssue, got, want)
		}
	})

	t.Run("at #3654's merge the linked #3955 holds the close", func(t *testing.T) {
		pool, apply := opened(t, phase.Merging)
		mustSpend(t, "the link of #3955", apply("linked-3955", replayLinks(2, PhaseStatus(phase.Merging), 3955)))
		mustSpend(t, "the merge of #3654", apply("merged-3654", intake.PullRequestMerged{Repo: replayRepo, Number: 3654, MergeSHA: "merge-3654",
			HeadSHA: "a95b58b7"}))
		if got := storedIssue(t, pool, replayIssue); got.Phase != phase.Merging || got.Status == "done" {
			t.Errorf("after #3654's merge %s stands in %s at %s, want merging: the linked #3955 is open, and holds the close", replayIssue, got.Phase, got.Status)
		}
		for _, close := range closesOf(t, pool, replayIssue) {
			if slices.Contains(close.Numbers, 3955) {
				t.Errorf("the stack's close %+v closes #3955, want none: a linked pull request is the issue's to merge or to unlink", close)
			}
		}
		holds := noticesOf(t, pool, replayIssue, "pr-holds-close")
		if len(holds) != 1 || !strings.Contains(holds[0].Reason, draft) || !strings.Contains(holds[0].Reason, "link") {
			t.Errorf("the hold sent pr-holds-close %+v, want one that names %s and the removal of its link", holds, draft)
		}
	})

	t.Run("with its link removed #3955 stays in the set and the issue closes", func(t *testing.T) {
		pool, apply := opened(t, phase.Merging)
		mustSpend(t, "the link of #3955", apply("linked-3955", replayLinks(2, PhaseStatus(phase.Merging), 3955)))
		if pr := pullRequestOf(t, pool, replayRepo, 3955); pr == nil || !pr.Linked {
			t.Fatalf("after its link #3955 is %+v, want it linked", pr)
		}
		mustSpend(t, "the merge of #3654", apply("merged-3654", intake.PullRequestMerged{Repo: replayRepo, Number: 3654, MergeSHA: "merge-3654",
			HeadSHA: "a95b58b7"}))
		mustSpend(t, "the removal of #3955's link", apply("unlinked-3955", replayLinks(3, storedIssue(t, pool, replayIssue).Status)))
		if _, merges := mergeOf(t, pool, replayIssue); !slices.Equal(merges, []int{3654}) {
			t.Errorf("the merges of %s are %v, want #3654 alone: a merged pull request stays the task's own", replayIssue, merges)
		}
		if pr := pullRequestOf(t, pool, replayRepo, 3955); pr == nil || pr.Linked || !slices.Contains(pullRequestSetOf(t, pool, replayIssue), draft) {
			t.Errorf("#3955 is %+v in the set %v, want it unlinked and still in the set: it is on the issue's own branch", pr, pullRequestSetOf(t, pool, replayIssue))
		}
		if got := storedIssue(t, pool, replayIssue); got.Phase != phase.Done {
			t.Errorf("after #3955's link went %s stands in %s, want done: nothing of its set holds the close", replayIssue, got.Phase)
		}
		done := fmt.Sprintf("Issue %s is done; its last merge is #3654.", replayIssue)
		if !slices.ContainsFunc(closesOf(t, pool, replayIssue), func(close record.TaskPullRequestsClose) bool {
			return slices.Contains(close.Numbers, 3955) && close.Comment == done
		}) {
			t.Errorf("the stack's closes %+v close no #3955 with %q, want one: the release of the held close closes each open task pull request the issue does not link",
				closesOf(t, pool, replayIssue), done)
		}
	})
}
