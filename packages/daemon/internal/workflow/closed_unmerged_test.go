package workflow

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownClosed is GitHub's close of LEGION-208's sjawhar/legion#42 with no merge.
var ownClosed = intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-a", UpdatedAt: linkedAt.Add(2 * time.Minute)}

// todaysUnmerged is the pr-closed-unmerged reason the daemon gives today for #42 of LEGION-208,
// closed with no merge while the issue stood in at.
func todaysUnmerged(at phase.Phase) string {
	return "pull request sjawhar/legion#42 closed without merging while LEGION-208 was in " + string(at)
}

// foundOpen is sjawhar/legion#43 as the daemon's read of LEGION-208's branch finds it on GitHub:
// open on legion/LEGION-208 onto main, its head a branch of the repository itself, as its opened
// event would carry it.
var foundOpen = intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-208", Base: "main", HeadSHA: "head-43",
	HeadRepo: "sjawhar/legion", URL: "https://github.com/sjawhar/legion/pull/43", UpdatedAt: linkedAt.Add(3 * time.Minute)}

// branchReadsOf is each read of the branches of key's tasks that its outbox holds
// (record.PullRequestBranchRead), in the order they were queued.
func branchReadsOf(t *testing.T, pool *pgxpool.Pool, key string) []record.PullRequestBranchRead {
	t.Helper()
	var reads []record.PullRequestBranchRead
	for _, row := range issueOutboxOf(t, pool, key, string(record.OutboxKindPullRequestBranchRead)) {
		payload, err := record.DecodeOutboxPayload(record.OutboxRow{ID: row.id, Kind: record.OutboxKindPullRequestBranchRead, Issue: key, Payload: row.payload})
		if err != nil {
			t.Fatalf("decode the branch read row %d of %s: %v", row.id, key, err)
		}
		reads = append(reads, payload.(record.PullRequestBranchRead))
	}
	return reads
}

// readBranches is the daemon's read of the branches of key's tasks ending, as its executor ends it
// (the daemon's pullRequestBranchRead): it fails unless key's outbox holds exactly one such read,
// ends that row, as the outbox ends a row that ran, and applies the read's fact
// (intake.PullRequestBranchReadDone) with open, each pull request GitHub shows open on those
// branches.
func readBranches(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result, key string, open ...intake.PullRequestOpened) {
	t.Helper()
	rows := issueOutboxOf(t, pool, key, string(record.OutboxKindPullRequestBranchRead))
	reads := branchReadsOf(t, pool, key)
	if len(reads) != 1 {
		t.Fatalf("the outbox holds %d reads of the branches of %s (%+v), want the one the close of its last open pull request queued", len(reads), key, reads)
	}
	if _, err := pool.Exec(t.Context(), "delete from outbox where id = $1", rows[0].id); err != nil {
		t.Fatalf("end the branch read row %d of %s: %v", rows[0].id, key, err)
	}
	read := reads[0]
	mustSpend(t, "the read of the branches of "+key, apply(fmt.Sprintf("branch-read-%s-%d", key, rows[0].id),
		intake.PullRequestBranchReadDone{Issue: key, Generation: read.Generation, Repo: read.Repo, Number: read.Number, Open: open}))
}

// undecided fails unless the close named after has told key's architect nothing yet, neither
// pr-closed-unmerged nor pr-closed-others-open: the close of the last open pull request of a set
// decides nothing before the daemon has read the branches of its tasks again.
func undecided(t *testing.T, pool *pgxpool.Pool, key, after string) {
	t.Helper()
	for _, kind := range []record.NoticeKind{"pr-closed-unmerged", "pr-closed-others-open"} {
		if told := noticesOf(t, pool, key, kind); len(told) != 0 {
			t.Fatalf("after %s %s's architect holds %s %+v, want none before the read of its branches", after, key, kind, told)
		}
	}
}

// readOf is the read of the branches of LEGION-208's one branch, legion/LEGION-208, that the close
// of number of repo queues at generation 1.
func readOf(repo string, number int) record.PullRequestBranchRead {
	return record.PullRequestBranchRead{Generation: 1, Repo: repo, Number: number, Branches: []string{"legion/LEGION-208"}}
}

// noneOpenAfter is the end of the daemon's read of the branches of key's tasks that finds no pull
// request open on them (intake.PullRequestBranchReadDone), as its executor applies it after the
// close of number of repo, the last open pull request of key's set, at generation 1: what a test of
// a close that is not about the read applies after that close, as the daemon's outbox would.
func noneOpenAfter(key, repo string, number int) intake.PullRequestBranchReadDone {
	return intake.PullRequestBranchReadDone{Issue: key, Generation: 1, Repo: repo, Number: number}
}

// A pull request closed with no merge while another of its issue's set is open tells the architect
// in a notice of its own kind and moves nothing (dispatch://INFRA-5910 D5). LEGION-208's task a,
// at its review, has its own sjawhar/legion#42 on legion/LEGION-208, and the issue links
// acme/deploy#6990, which stands on the same branch name in a second repository. #6990 closes with
// no merge: one pr-closed-others-open names #42 as still open, no pr-closed-unmerged is sent, no
// read of the branches is queued, since the close is not the last open pull request's, a stays at
// its review, and #42 is still in the set. #42 is then reviewed, a passes its retro, and #42
// merges: the issue closes, with #6990 still closed, which holds nothing.
//
// The faults it catches: the close handler that sends pr-closed-unmerged for any close, under which
// this close finds that notice; and the record keyed by issue and branch, under which the closed
// pull request's row replaces the open one's of the same branch name.
func TestAPullRequestClosedUnmergedWhileAnotherIsOpenKeepsTheOtherInTheSet(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := noCheckEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasksAt(t, pool, root.Key, root.Generation,
		taskAt{id: "a", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	linkDeploy(t, pool, apply, root.Key, "", 2, "legion/LEGION-208")
	mustSpend(t, "the close of "+linkedPR, apply("closed-6990", deployClosed))
	if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 0 {
		t.Fatalf("the close of %s sent pr-closed-unmerged %+v, want none: #42 is still open", linkedPR, told)
	}
	if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 1 || !strings.Contains(told[0].Reason, "sjawhar/legion#42") ||
		!strings.Contains(told[0].Reason, linkedPR) {
		t.Fatalf("the close of %s sent pr-closed-others-open %+v, want one that names it closed and sjawhar/legion#42 still open", linkedPR, told)
	}
	if reads := branchReadsOf(t, pool, root.Key); len(reads) != 0 {
		t.Fatalf("the close of %s queued the branch reads %+v, want none: #42 is still open", linkedPR, reads)
	}
	stillOpen(t, pool, root.Key, phase.Reviewing, "the close of "+linkedPR)
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
	if set := pullRequestSetOf(t, pool, root.Key); !slices.Contains(set, "sjawhar/legion#42") {
		t.Fatalf("after the close of %s the set of %s is %v, want sjawhar/legion#42 in it", linkedPR, root.Key, set)
	}

	approvesReviewOf(t, pool, engine, root.Key)
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atRetro)
	taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
	stillOpen(t, pool, root.Key, phase.Merging, "a's retro")
	mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
	closedDone(t, pool, root.Key, "the merge of #42")
	if pr := pullRequestOf(t, pool, deployLink.Repo, deployLink.Number); pr == nil || pr.State != record.PullRequestClosed {
		t.Fatalf("after the close of %s it is %+v, want it still closed", root.Key, pr)
	}
}

// The last open pull request of an issue's set closed with no merge, after another merged, makes
// the daemon judge the close again (dispatch://INFRA-5910 D5, decisions 9 and 12), once it has read
// from GitHub the open pull requests of the branch of each task. LEGION-208 is in merging with its
// task a at its merge, a's own sjawhar/legion#42, and the link of acme/deploy#6990. Each close of
// the last open pull request queues the read of a's branch, legion/LEGION-208, and decides nothing
// until the read's fact:
//   - #42 merges, which the open #6990 holds the close of; #6990 then closes with no merge, and the
//     read finds no pull request open on the branch: the issue closes at the read's fact, with no
//     pr-closed-unmerged;
//   - #6990 merges first, and a's own #42 then closes with no merge, and the read finds none open:
//     a's merge is not met, so the issue stays in merging and the architect gets the
//     pr-closed-unmerged of today, whose words are then true;
//   - #42 merges, #6990 closes with no merge, and the read finds sjawhar/legion#43 open on a's
//     branch, whose opened the daemon never got: #43 is recorded as its opened would record it, the
//     architect gets one pr-closed-others-open that names it, no pr-closed-unmerged, and the issue
//     closes nothing and stays in merging.
//
// The faults it catches: the close handler left as it was, which never judges the close, under
// which the issue stays in merging after #6990 closes; a close that ends the issue at any last
// close, a verdict of its own, under which the second row closes it; a close that decides before
// the read, under which each row finds its decision at the close; and a read that does not record
// what it finds, under which the third row closes the issue.
func TestTheLastOpenPullRequestClosedUnmergedAfterAnotherMergedJudgesTheCloseAgain(t *testing.T) {
	t.Parallel()
	merging := func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact) intake.Result, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Merging)
		seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
		apply := applyFacts(t, pool, noCheckEngine())
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		return pool, apply, root
	}
	// lastClosed is the close of the last open pull request, closed, named what, which decides
	// nothing yet and queues the read of a's branch for it.
	lastClosed := func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result, root record.Issue, what string, closed intake.PullRequestClosed) {
		t.Helper()
		mustSpend(t, what, apply(fmt.Sprintf("closed-%d", closed.Number), closed))
		stillOpen(t, pool, root.Key, phase.Merging, what)
		undecided(t, pool, root.Key, what)
		if reads, want := branchReadsOf(t, pool, root.Key), readOf(closed.Repo, closed.Number); len(reads) != 1 || !reflect.DeepEqual(reads[0], want) {
			t.Fatalf("%s queued the branch reads %+v, want one: %+v", what, reads, want)
		}
	}

	t.Run("a's #42 merged, then the linked one closed", func(t *testing.T) {
		pool, apply, root := merging(t)
		mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of #42")
		lastClosed(t, pool, apply, root, "the close of "+linkedPR, deployClosed)
		readBranches(t, pool, apply, root.Key)
		closedDone(t, pool, root.Key, "the read of a's branch")
		for _, kind := range []record.NoticeKind{"pr-closed-unmerged", "pr-closed-others-open"} {
			if told := noticesOf(t, pool, root.Key, kind); len(told) != 0 {
				t.Fatalf("the close of %s sent %s %+v, want none: it closed the issue", linkedPR, kind, told)
			}
		}
	})

	t.Run("the linked one merged, then a's own #42 closed", func(t *testing.T) {
		pool, apply, root := merging(t)
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		lastClosed(t, pool, apply, root, "the close of #42", ownClosed)
		readBranches(t, pool, apply, root.Key)
		stillOpen(t, pool, root.Key, phase.Merging, "the read of a's branch")
		if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 1 || told[0].Reason != todaysUnmerged(phase.Merging) {
			t.Fatalf("the close of #42 sent pr-closed-unmerged %+v, want one: %q", told, todaysUnmerged(phase.Merging))
		}
		if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 0 {
			t.Fatalf("the close of #42 sent pr-closed-others-open %+v, want none: no pull request of the set is open", told)
		}
	})

	t.Run("a's #42 merged, the linked one closed, and the read finds #43 open on a's branch", func(t *testing.T) {
		pool, apply, root := merging(t)
		mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
		lastClosed(t, pool, apply, root, "the close of "+linkedPR, deployClosed)
		readBranches(t, pool, apply, root.Key, foundOpen)
		stillOpen(t, pool, root.Key, phase.Merging, "the read of a's branch")
		if pr := pullRequestOf(t, pool, foundOpen.Repo, foundOpen.Number); pr == nil || pr.State != record.PullRequestOpen || pr.Issue != root.Key ||
			pr.Branch != foundOpen.Branch || pr.HeadSHA != foundOpen.HeadSHA {
			t.Fatalf("after the read sjawhar/legion#43 is %+v, want it recorded open for %s on %s at %s", pr, root.Key, foundOpen.Branch, foundOpen.HeadSHA)
		}
		if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 1 || !strings.Contains(told[0].Reason, "sjawhar/legion#43") ||
			!strings.Contains(told[0].Reason, linkedPR) {
			t.Fatalf("the read of a's branch sent pr-closed-others-open %+v, want one that names %s closed and sjawhar/legion#43 still open", told, linkedPR)
		}
		if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 0 {
			t.Fatalf("the read of a's branch sent pr-closed-unmerged %+v, want none: sjawhar/legion#43 is open", told)
		}
	})
}

// The last open pull request of an issue's set closed with no merge, while none merged, tells the
// architect as the daemon does today (dispatch://INFRA-5910 D5), once the daemon's read of the
// issue's branch on GitHub finds no other pull request open there: LEGION-208, in reviewing with no
// plan rows, has its #42 on legion/LEGION-208. Each row is a record of its own:
//   - #42 alone closes, and the read finds none open: today's one pr-closed-unmerged, and nothing
//     moves;
//   - acme/deploy#6990, which the issue links, closes first (pr-closed-others-open, naming #42, and
//     no read, since #42 is open), and then #42 closes, and the read finds none open: the same;
//   - #42 alone closes, and the read finds sjawhar/legion#43 open on the branch, whose opened the
//     daemon never got: #43 is recorded open, one pr-closed-others-open names it, no
//     pr-closed-unmerged is sent, and nothing moves.
//
// Each close of #42 queues the read of legion/LEGION-208 and tells nothing before its fact.
//
// The faults it catches are a close that judges the issue closed, or tells nothing, where nothing
// merged; a close that tells before the read; and a read that does not record what it finds, under
// which the third row sends pr-closed-unmerged.
func TestTheLastOpenPullRequestClosedUnmergedWithNoneMergedTellsTheArchitectAsToday(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		linked bool
		found  []intake.PullRequestOpened
	}{
		{name: "#42 alone"},
		{name: "after the linked pull request closed", linked: true},
		{name: "#42 alone, and the read finds #43 open on the branch", found: []intake.PullRequestOpened{foundOpen}},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := stepRoot(t, pool, phase.Reviewing)
			apply := applyFacts(t, pool, noCheckEngine())
			if row.linked {
				linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
				mustSpend(t, "the close of "+linkedPR, apply("closed-6990", deployClosed))
				if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 1 || !strings.Contains(told[0].Reason, "sjawhar/legion#42") {
					t.Fatalf("the close of %s sent pr-closed-others-open %+v, want one that names sjawhar/legion#42", linkedPR, told)
				}
				if reads := branchReadsOf(t, pool, root.Key); len(reads) != 0 {
					t.Fatalf("the close of %s queued the branch reads %+v, want none: #42 is still open", linkedPR, reads)
				}
			}
			mustSpend(t, "the close of #42", apply("closed-42", ownClosed))
			if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 0 {
				t.Fatalf("the close of #42 sent pr-closed-unmerged %+v, want none before the read of the branch", told)
			}
			if reads := branchReadsOf(t, pool, root.Key); len(reads) != 1 || !reflect.DeepEqual(reads[0], readOf(ownClosed.Repo, ownClosed.Number)) {
				t.Fatalf("the close of #42 queued the branch reads %+v, want one: %+v", reads, readOf(ownClosed.Repo, ownClosed.Number))
			}
			readBranches(t, pool, apply, root.Key, row.found...)
			stillOpen(t, pool, root.Key, phase.Reviewing, "the read of the branch")
			if len(row.found) == 0 {
				if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 1 || told[0].Reason != todaysUnmerged(phase.Reviewing) {
					t.Fatalf("the close of #42 sent pr-closed-unmerged %+v, want one: %q", told, todaysUnmerged(phase.Reviewing))
				}
				return
			}
			if pr := pullRequestOf(t, pool, foundOpen.Repo, foundOpen.Number); pr == nil || pr.State != record.PullRequestOpen || pr.Issue != root.Key {
				t.Fatalf("after the read sjawhar/legion#43 is %+v, want it recorded open for %s", pr, root.Key)
			}
			if told := noticesOf(t, pool, root.Key, "pr-closed-others-open"); len(told) != 1 || !strings.Contains(told[0].Reason, "sjawhar/legion#43") ||
				!strings.Contains(told[0].Reason, "sjawhar/legion#42") {
				t.Fatalf("the read of the branch sent pr-closed-others-open %+v, want one that names #42 closed and sjawhar/legion#43 still open", told)
			}
			if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 0 {
				t.Fatalf("the read of the branch sent pr-closed-unmerged %+v, want none: sjawhar/legion#43 is open", told)
			}
		})
	}
}
