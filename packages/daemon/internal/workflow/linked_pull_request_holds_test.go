package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// newestOutboxID is the highest id the outbox holds, 0 for none.
func newestOutboxID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var newest int64
	if err := pool.QueryRow(t.Context(), `select coalesce(max(id), 0) from outbox`).Scan(&newest); err != nil {
		t.Fatalf("read the newest outbox id: %v", err)
	}
	return newest
}

// linkReadsQueuedSince is how many reads of repo#number for key the outbox queued in rows after the
// row after.
func linkReadsQueuedSince(t *testing.T, pool *pgxpool.Pool, key, repo string, number int, after int64) int {
	t.Helper()
	var queued int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'pull_request_link_read' and issue = $1
		and lower(payload->>'repo') = lower($2) and (payload->>'number')::int = $3 and id > $4`, key, repo, number, after).Scan(&queued); err != nil {
		t.Fatalf("count the queued reads of %s#%d: %v", repo, number, err)
	}
	return queued
}

// noHoldTold fails if key's architect holds a pr-holds-close.
func noHoldTold(t *testing.T, pool *pgxpool.Pool, key, after, why string) {
	t.Helper()
	if holds := noticesOf(t, pool, key, "pr-holds-close"); len(holds) > 0 {
		t.Errorf("%s told the architect %+v, want no pull request holding the close: %s", after, holds, why)
	}
}

// A link whose pull request merged in an earlier run of the issue holds nothing of the next run's
// close (dispatch://INFRA-5910 D4; plan decision 23: a new generation deletes no link, and "the new
// run reads each linked pull request again"). LEGION-208, in merging, links acme/deploy#6990,
// which merges while its task's #42 is still open. A person then sets the issue back to todo: the
// re-admission clears the old run's record (Engine.ClearGeneration), which deletes the merged
// row of #6990 and keeps its link, and the new run stands at its merge with #42 again. Where the
// clear queued a read of #6990, the read answers it merged, as GitHub does. The merge of #42 then
// closes the issue.
//
// The fault it catches: a clear that keeps the link, deletes its merged row and reads nothing
// again, under which the link holds the close as a read still owed (setHolds), the architect is told
// that Legion has not read #6990 yet, and the issue waits in merging until the next boot reads it.
func TestALinkWhosePullRequestMergedInAnEarlierRunHoldsNoCloseOfTheNext(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := noCheckEngine()
	root := stepRoot(t, pool, phase.Merging)
	seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
	mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
	stillOpen(t, pool, root.Key, phase.Merging, "the merge of "+linkedPR+" with #42 open")

	cleared := storedIssue(t, pool, root.Key)
	before := newestOutboxID(t, pool)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return engine.ClearGeneration(t.Context(), tx, cleared, time.Now().UTC())
	})
	next := cleared
	next.Generation++
	seedIssue(t, pool, next)
	seedTasksAt(t, pool, root.Key, next.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
	if linkReadsQueuedSince(t, pool, root.Key, deployLink.Repo, deployLink.Number, before) > 0 {
		mustSpend(t, "the new run's read of "+linkedPR, apply("reread-6990", linkRead(root.Key, deployLink.Repo, deployLink.Number,
			record.PullRequestMerged, "fix/deploy")))
	}

	mustSpend(t, "the new run's merge of #42", apply("merged-42", mergedFact(42, "a")))
	noHoldTold(t, pool, root.Key, "the new run's merge of #42", linkedPR+" merged in the earlier run")
	closedDone(t, pool, root.Key, "the new run's merge of #42, with "+linkedPR+" merged in the earlier run")
}

// A link that names a pull request of the issue under the other name of a configured rename is the
// same pull request, and holds the close only while that pull request is open (dispatch://INFRA-5910
// D4, decision 4's rename canonical form). LEGION-208 was recorded on sjawhar/legion before the
// repository moved to cybermonklabs/legion, the pair a configured rename makes one repository
// (renamedLegion), so its task's #42 is recorded under sjawhar/legion, while GitHub and Dispatch
// name the repository cybermonklabs/legion now. A person links #42 on the issue by the URL GitHub
// shows, https://github.com/cybermonklabs/legion/pull/42. GitHub then merges #42, its event naming
// cybermonklabs/legion: the merge is the issue's last, and it closes the issue.
//
// The fault it catches: a link compared with the record's row under the link's own name alone
// (setHolds), under which the link of a pull request the record holds under the other name reads
// as a read still owed — the link's add found the row under either name and queued no read — and
// holds the close for ever: the architect is told that Legion has not read #42 yet, and the issue
// waits in merging until its link is removed.
func TestALinkUnderTheOtherNameOfARenameHoldsNoCloseOnceItsPullRequestMerged(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := renamedEngine(renamedLegion)
	engine.cfg.ProductionCheckRole = ""
	root := stepRoot(t, pool, phase.Merging)
	seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the link of cybermonklabs/legion#42", apply("linked-42", linkEvent(t, pool, root.Key, "", 2,
		intake.PullRequestLink{Repo: "cybermonklabs/legion", Number: 42})))

	mustSpend(t, "the merge of #42", apply("merged-42", intake.PullRequestMerged{Repo: "cybermonklabs/legion", Number: 42, MergeSHA: "merge-a"}))
	noHoldTold(t, pool, root.Key, "the merge of #42", "the link names #42, which merged")
	closedDone(t, pool, root.Key, "the merge of #42, linked under the repository's new name")
}

// A merge of a linked pull request that GitHub delivers after the daemon's read of it asked GitHub,
// and before that read's answer lands, is not lost (dispatch://INFRA-5910 D4: the merge of a pull
// request is recorded). LEGION-208, in merging with its task's #42 open, links acme/deploy#6990:
// the link queues the read of #6990, and the record holds no row of it yet. GitHub answers the
// read open, then merges #6990, and the merge's event lands before the read's answer, which then
// records #6990 open. Where the merge queued another read of #6990, that read answers it merged,
// as GitHub does. The merge of #42 then closes the issue.
//
// The fault it catches: a merge of a linked pull request the record holds no row of yet that
// records nothing and asks nothing, under which the read's stale answer records #6990 open, no
// later event of it comes, and #6990 holds the close until the next boot reads it again.
func TestAMergeOfALinkedPullRequestBeforeItsFirstReadLandsIsNotLost(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := noCheckEngine()
	root := stepRoot(t, pool, phase.Merging)
	seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the link of "+linkedPR, apply("linked", linkEvent(t, pool, root.Key, "", 2, deployLink)))
	before := newestOutboxID(t, pool)
	apply("merged-6990", deployMerged)
	mustSpend(t, "the read's answer, asked before the merge", apply("read-open", linkRead(root.Key, deployLink.Repo, deployLink.Number,
		record.PullRequestOpen, "fix/deploy")))
	if linkReadsQueuedSince(t, pool, root.Key, deployLink.Repo, deployLink.Number, before) > 0 {
		mustSpend(t, "the read the merge asked for", apply("reread-6990", linkRead(root.Key, deployLink.Repo, deployLink.Number,
			record.PullRequestMerged, "fix/deploy")))
	}

	mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
	noHoldTold(t, pool, root.Key, "the merge of #42", linkedPR+" merged")
	closedDone(t, pool, root.Key, "the merge of #42, with "+linkedPR+" merged before its read landed")
}

// A pull request on another issue's branch stays that issue's when a link of this issue is read
// before the other issue's own open of it lands (dispatch://INFRA-5910 D2: the pull request of each
// plan task is in its issue's set; decision 8: a link to another issue's pull request is refused,
// and a pull request belongs to one issue). LEGION-209, in testing with no plan rows, opens
// sjawhar/legion#50 on its branch legion/LEGION-209, and LEGION-208 links #50. The read of the link
// lands first, then GitHub's event of #50's open: #50 is LEGION-209's, and LEGION-208's link of it
// is refused as LEGION-209's.
//
// The fault it catches: a read that records a pull request the record holds no row of as the
// linking issue's, whatever branch it stands on, under which #50 is recorded for LEGION-208, the
// open of #50 records nothing ("a pull request the record holds for another issue stays with it"),
// and LEGION-209 never has its own pull request, so its implementer's completion is never taken.
func TestALinkReadBeforeAnotherIssuesOpenLeavesItsPullRequestToThatIssue(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := tasksRoot(t, pool)
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-209",
		Project: "LEGION", Title: "another root", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, "LEGION-209")
	apply := applyFacts(t, pool, stepEngine(nil))
	theirs := intake.PullRequestLink{Repo: "sjawhar/legion", Number: 50}
	mustSpend(t, "the link of LEGION-209's #50", apply("linked-50", linksOf(root.Key, 2, "in_progress", theirs)))
	mustSpend(t, "the read of #50", apply("read-50", linkRead(root.Key, "sjawhar/legion", 50, record.PullRequestOpen, "legion/LEGION-209")))
	mustSpend(t, "the open of #50 on legion/LEGION-209", apply("opened-50", openedOn(50, "legion/LEGION-209", linkedAt.Add(time.Minute), false)))

	if pr := pullRequestOf(t, pool, "sjawhar/legion", 50); pr == nil || pr.Issue != "LEGION-209" {
		t.Errorf("#50 is %+v, want it LEGION-209's: it stands on LEGION-209's branch", pr)
	}
	if links := storedLinks(t, pool, root.Key); len(links) != 1 || links[0].Refused != record.LinkRefusedOtherIssue {
		t.Errorf("the links of %s are %+v, want #50 refused as another issue's", root.Key, links)
	}
}
