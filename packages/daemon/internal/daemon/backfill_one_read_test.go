package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The backfill reads the pull requests of every record it fills in one statement, whatever the
// number of records: the rows of every task of every issue, by issue
// (record.Store.TaskPullRequestRowsOf), where a read for each issue costs a boot over many records
// one statement for each. Three records, each with a pull request on the repository Dispatch
// resolves, are each filled, and the pool the backfill reads through sends one statement that
// reads the rows of pull_requests by issue. That the read still takes the pull request of each
// task, not of the merge alone, TestTheBackfillLeavesARecordWhoseNonMergingTasksPullRequestIsOnAnotherRepository
// holds.
func TestTheBackfillReadsThePullRequestsOfEveryRecordInOneStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, records := isolatedOutboxPool(t), record.NewStore()
	keys := []string{"LEGION-7", "LEGION-9", "LEGION-12"}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for i, key := range keys {
			if _, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
				values ($1, $1, 'LEGION', $1, 'implementing', 1, 'in_progress', 'A', 1, '', '')`, key); err != nil {
				return err
			}
			if err := records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "acme/widgets", Number: 70 + i,
				Branch: "legion/" + key, HeadSHA: "head", State: record.PullRequestOpen}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sent := &sentStatements{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = sent
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the recording pool: %v", err)
	}
	t.Cleanup(traced.Close)
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{
		{Key: "LEGION-7", ResolvedRepo: "acme/widgets"}, {Key: "LEGION-9", ResolvedRepo: "acme/widgets"}, {Key: "LEGION-12", ResolvedRepo: "acme/widgets"},
	}}
	left, err := fillRepositories(ctx, store.NewPool(traced), records, client, "LEGION", t.TempDir(),
		newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), ghrepo.Renames{}, productionRepositoryReads(), quietLogger())
	if err != nil || left {
		t.Fatalf("backfill = left %t, %v; want every record filled and no error", left, err)
	}
	statements := sent.statements()
	var reads []string
	for _, sql := range statements {
		if strings.Contains(sql, "from pull_requests where issue") {
			reads = append(reads, sql)
		}
	}
	if len(reads) == 0 {
		t.Fatalf("UNKNOWN: the backfill sent no statement that reads pull_requests by issue, so nothing says how it read them; sent %q", statements)
	}
	if len(reads) != 1 {
		t.Errorf("the backfill of %d records read pull_requests by issue in %d statements, want 1: %q", len(keys), len(reads), reads)
	}
	w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
	for _, key := range keys {
		if got := recordedIssue(t, w, key); got == nil || got.Repo != "acme/widgets" {
			t.Errorf("%s's record = %+v after the backfill, want it filled with acme/widgets", key, got)
		}
	}
}
