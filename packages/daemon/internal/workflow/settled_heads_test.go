package workflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// headSettled is #42's settlement of head: ci, run id, at the listener's generation, failing the
// checks named.
func headSettled(head string, id, generation int64, failing ...string) intake.PullRequestChecks {
	return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, CheckRuns: []record.AttemptRun{{Name: "ci", ID: id}},
		Generation: generation, Snapshot: fmt.Sprintf("%s-%d", head, generation), Failing: append([]string{}, failing...), Cancelled: []string{}}
}

// settledHeadKinds is how many rows of each kind pull_request_settled_heads holds.
func settledHeadKinds(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select kind, count(*) from pull_request_settled_heads group by kind`)
	if err != nil {
		t.Fatalf("count the settled heads: %v", err)
	}
	defer rows.Close()
	kinds := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatalf("count the settled heads: %v", err)
		}
		kinds[kind] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the settled heads: %v", err)
	}
	return kinds
}

// twoHeadsSettledAndARunOpen seeds LEGION-208 in testing with #42 open at head, and applies
// head's settlement, the branch's move to head-2 with its push, head-2's settlement, and a move to
// head-3 whose push is not known: the pull request then remembers the fences of head and head-2,
// and the head run the move opened remembers both as well.
func twoHeadsSettledAndARunOpen(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact)) {
	t.Helper()
	const dev = "legion-implementer[bot]"
	pool := migratedPool(t)
	seedLateIssue(t, pool, phase.Testing, "testing", &record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: "head", HeadUpdatedAt: lateApplied, Failing: []string{}})
	apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("head-settled", headSettled("head", 5, 2))
	apply("pushed-head-2", sentPush("head", "head-2", dev))
	apply("moved-to-head-2", namedSync("head", "head-2", dev, lateApplied.Add(time.Minute)))
	apply("head-2-settled", headSettled("head-2", 7, 1))
	apply("moved-to-head-3", namedSync("head-2", "head-3", dev, lateApplied.Add(2*time.Minute)))
	if kinds := settledHeadKinds(t, pool); kinds["pull_request"] != 2 || kinds["head_run"] != 2 || len(kinds) != 2 {
		t.Fatalf("the store holds the settled heads %v, want two of the pull request's and two of its open head run's", kinds)
	}
	return pool, apply
}

// A reopen keeps what the pull request remembers of its settled heads, its own and its open head
// run's (classify.Reopen; record.PullRequest.SettledHeads, record.HeadRun.Settled): the rows of
// both kinds the store holds read the same after it. Here head and head-2 settled, the branch moved
// on to head-3 with its push not known, which opened a head run, and the pull request was closed at
// head-3 and reopened there.
func TestAReopenKeepsBothKindsOfStoredFences(t *testing.T) {
	t.Parallel()
	pool, apply := twoHeadsSettledAndARunOpen(t)
	apply("closed", closedAt("head-3", lateApplied.Add(3*time.Minute)))
	before := tablesText(t, pool, "pull_request_settled_heads")
	apply("reopened", readOpened("head-3", lateApplied.Add(4*time.Minute), true))
	if pr := latePullRequest(t, pool); pr.State != record.PullRequestOpen || pr.HeadSHA != "head-3" || pr.Run == nil {
		t.Fatalf("after the reopen the pull request is %s at %s, a head run open %v; want it open at head-3 with its run", pr.State, pr.HeadSHA, pr.Run != nil)
	}
	if after := tablesText(t, pool, "pull_request_settled_heads"); after != before {
		t.Errorf("the reopen changed the stored settled heads:\nbefore %s\nafter  %s", before, after)
	}
}

// An opened of another pull request number on the branch is a pull request of its own, which
// replaces none (dispatch://INFRA-5910 D1): its row remembers no settled head and has no head run,
// and the earlier pull request's row keeps what it remembers, its settled heads and its open head
// run, with the store's rows of both kinds as they stood. Under the record keyed by the issue and
// the branch the opened replaced the earlier row, and the rows it remembered went with it.
func TestAnOpenedOfAnotherNumberKeepsTheEarlierRowAndItsFences(t *testing.T) {
	t.Parallel()
	pool, apply := twoHeadsSettledAndARunOpen(t)
	before := settledHeadKinds(t, pool)
	// The opened names its head's repository, as GitHub's does: one that names none is not recorded
	// (pullRequestOpened, as a fork's is).
	apply("opened-43", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-208", HeadSHA: "head-43", HeadRepo: "sjawhar/legion",
		URL: "https://github.com/sjawhar/legion/pull/43", UpdatedAt: lateApplied.Add(3 * time.Minute)})
	if pr := pullRequestOf(t, pool, "sjawhar/legion", 43); pr == nil || pr.HeadSHA != "head-43" || len(pr.SettledHeads) != 0 || pr.Run != nil {
		t.Fatalf("after #43 opened its row is %+v; want #43 at head-43 remembering nothing, no run", pr)
	}
	if pr := pullRequestOf(t, pool, "sjawhar/legion", 42); pr == nil || pr.HeadSHA != "head-3" || len(pr.SettledHeads) != 2 || pr.Run == nil {
		t.Fatalf("after #43 opened #42's row is %+v; want #42 at head-3 remembering its two settled heads, its run open", pr)
	}
	if after := settledHeadKinds(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("after #43 opened the store holds the settled heads %v, want %v, #42's as they stood", after, before)
	}
}
