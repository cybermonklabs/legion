package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// One event of the project's merge holds document tells each role that a hold can stop, and no
// other role (#4065). With one issue in accepting and one in merging,
// it writes two deliveries: a merge-holds-changed notice on the accepting issue, which reaches its
// tree's architect, and a merge-holds row on the merging issue, which reaches its merger. Each
// carries the one text that names the document and says to read it again. It writes nothing for
// an issue in another phase, for one out of the workflow, or for one whose tree lingers. The same
// event delivered again writes nothing more, since ApplyFact records the event's id, and an event
// of another project writes nothing.
func TestAMergeHoldsChangeTellsTheArchitectInAcceptingAndTheMergerInMerging(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seed := func(key, tree string, at phase.Phase, status string, lingers bool) {
		issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: tree,
			Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: status, Rank: "U"}
		if key != tree {
			issue.Parent = &tree
		}
		if lingers {
			until := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
			issue.LingerUntil = &until
		}
		seedIssue(t, pool, issue)
	}
	seed("LEGION-208", "LEGION-208", phase.Accepting, "needs_review", false)
	seed("LEGION-209", "LEGION-209", phase.Merging, "retro", false)
	seed("LEGION-210", "LEGION-210", phase.Implementing, "in_progress", false)
	seed("LEGION-211", "LEGION-211", phase.Merging, "backlog", false)
	seed("LEGION-212", "LEGION-212", phase.Done, "done", true)
	seed("LEGION-213", "LEGION-212", phase.Merging, "retro", false)

	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := func(id string, fact intake.Fact) {
		t.Helper()
		if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, engine, admissionStub{}); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}
	want := []string{"notice LEGION-208", "merge_holds_changed LEGION-209"}

	apply("dispatch-71", intake.MergeHoldsChanged{Project: "LEGION", Event: "dispatch-71"})
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("the merge holds change wrote %v, want %v: the accepting issue's notice and the merging issue's row, and nothing else", got, want)
	}
	reason := record.MergeHoldsChangedReason("LEGION")
	if told := noticesOfKind(t, pool, "LEGION-208", "merge-holds-changed"); len(told) != 1 || told[0].Reason != reason ||
		told[0].Role != claim.RoleArchitect || told[0].Phase != phase.Accepting {
		t.Fatalf("the accepting issue's notices = %+v, want one merge-holds-changed for its architect with the reason %q", told, reason)
	}
	if row := mergeHoldsRow(t, pool, "LEGION-209"); row != (record.MergeHoldsChanged{Project: "LEGION"}) {
		t.Fatalf("the merging issue's row = %+v, want the project's merge holds change", row)
	}

	apply("dispatch-71", intake.MergeHoldsChanged{Project: "LEGION", Event: "dispatch-71"})
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("the same event delivered again left %v, want %v: it writes nothing twice", got, want)
	}

	apply("dispatch-72", intake.MergeHoldsChanged{Project: "OTHER", Event: "dispatch-72"})
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("another project's merge holds change left %v, want %v: it writes nothing", got, want)
	}
}

// A root whose pull request merged stands in merging until every issue under it leaves the
// workflow (closeMerged), and a merge hold stops nothing there (finishedBy, over the pull request
// of the one task of a run with no plan rows), so a merge holds change writes its merger no row,
// and MergingNow, which the row's executor reads before each attempt, says the issue left
// merging. The control is a root in merging whose pull request is still open: it gets its row,
// and MergingNow says it works merging.
func TestAMergeHoldsChangeTellsNoMergerWhosePullRequestMerged(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	for i, state := range []record.PullRequestState{record.PullRequestMerged, record.PullRequestOpen} {
		key := fmt.Sprintf("LEGION-%d", 208+i)
		seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key,
			Project: "LEGION", Title: key, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
		seedPR(t, pool, record.PullRequest{State: state, Issue: key, Repo: "sjawhar/legion", Number: 42 + i,
			Branch: "legion/" + key, HeadSHA: "head", Failing: []string{}})
	}

	engine := testEngine(config.DesignGateRootIssues, nil)
	if _, err := intake.ApplyFact(t.Context(), pool, "dispatch", "dispatch-71", intake.MergeHoldsChanged{Project: "LEGION", Event: "dispatch-71"},
		engine, admissionStub{}); err != nil {
		t.Fatalf("apply the merge holds change: %v", err)
	}
	if got, want := mergeHoldsDeliveries(t, pool), []string{"merge_holds_changed LEGION-209"}; !slices.Equal(got, want) {
		t.Fatalf("the merge holds change wrote %v, want %v: a row for the open pull request's merger alone", got, want)
	}

	for key, want := range map[string]string{"LEGION-208": "the pull request is merged", "LEGION-209": ""} {
		if left := mergingNowOf(t, pool, key); left != want {
			t.Fatalf("MergingNow(%s) says %q, want %q", key, left, want)
		}
	}
}

// A merge holds change tells the merger of a task at its merge while another task of the issue
// implements: of a, at its first step, and b, at its merge with #7 open (seedBAtItsMerge), the
// issue stands in implementing, a's phase, and a line of the merge holds can hold b's merge. The
// change writes b's merger its row, and MergingNow, which the row's executor reads before each
// attempt, says the workflow works that merge, so the executor delivers the row. Once #7 has
// merged, a second change writes no second row, and MergingNow says why, task by task: b is judged
// by its own pull request, not by the issue's merge pull request (record.Store.PullRequestsOf,
// a's #42, which is still open).
//
// The faults it catches are the merge holds judged by the issue's own phase, which is its least
// finished task's (its control: mergeHoldsChanged and MergingNow of the revision before, which
// write no row and answer "the issue stands in implementing, not merging"), and a task's merge
// judged merged by the issue's merge pull request (its control: finishedBy given PullRequestsOf's
// #42, which writes b's merger a second row after #7 merged).
func TestAMergeHoldsChangeTellsTheMergerOfATaskAtItsMergeWhileAnotherImplements(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	seedBAtItsMerge(t, pool, root)
	apply := mergeHoldsChangeOf(t, pool)
	want := []string{"merge_holds_changed LEGION-208"}

	apply("dispatch-71")
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("the merge holds change wrote %v, want %v: the row of b's merger, while a implements", got, want)
	}
	if row := mergeHoldsRow(t, pool, root.Key); row != (record.MergeHoldsChanged{Project: "LEGION"}) {
		t.Fatalf("the merger's row of %s = %+v, want the project's merge holds change", root.Key, row)
	}
	if left := mergingNowOf(t, pool, root.Key); left != "" {
		t.Fatalf("MergingNow(%s) says %q, want \"\": the workflow works b's merge, so the executor delivers the row", root.Key, left)
	}

	seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: root.Key, Repo: "sjawhar/legion", Number: 7,
		Branch: "legion/" + root.Key + ".b", HeadSHA: "h7", MergeSHA: "merge-7", Failing: []string{}})
	apply("dispatch-72")
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("the merge holds change after #7 merged left %v, want %v: no second row, since b's own pull request merged", got, want)
	}
	wantLeft := "for task a, the issue stands in implementing, not merging; for task b, the pull request is merged"
	if left := mergingNowOf(t, pool, root.Key); left != wantLeft {
		t.Fatalf("MergingNow(%s) after #7 merged says %q, want %q", root.Key, left, wantLeft)
	}
}

// A merge holds change tells the architect of a task at its owner's acceptance while another task
// of the issue implements: of a, at its first step, and b, at step 2 of `implementer
// acceptance:owner`, the issue stands in implementing, a's phase, and a line of the merge holds
// can hold b's sign_off. The change writes the architect its merge-holds-changed notice, and no
// merger row, since no task stands at its merge. Once b is moved back to its first step, a second
// change writes nothing.
//
// The fault it catches is the accepting arm judged by the issue's own phase (its control:
// mergeHoldsChanged of the revision before, which writes no notice, since the issue stands in
// implementing).
func TestAMergeHoldsChangeTellsTheArchitectOfATaskAtItsAcceptanceWhileAnotherImplements(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer", "acceptance:owner"}, agents: []claim.Agent{"implementer-2", ""},
			state: record.TaskRunning, place: atStep(2)})
	apply := mergeHoldsChangeOf(t, pool)
	want := []string{"notice LEGION-208"}

	apply("dispatch-71")
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("the merge holds change wrote %v, want %v: the architect's notice of b's acceptance, while a implements, and no merger row", got, want)
	}
	reason := record.MergeHoldsChangedReason("LEGION")
	if told := noticesOfKind(t, pool, root.Key, "merge-holds-changed"); len(told) != 1 || told[0].Reason != reason ||
		told[0].Role != claim.RoleArchitect || told[0].Phase != phase.Accepting {
		t.Fatalf("the notices of %s = %+v, want one merge-holds-changed for its architect in accepting with the reason %q", root.Key, told, reason)
	}

	seedRecord(t, pool, func(tx pgx.Tx) error {
		return seedMove(t.Context(), tx, root.Key, root.Generation, "b", atStep(1), record.TaskRunning)
	})
	apply("dispatch-72")
	if got := mergeHoldsDeliveries(t, pool); !slices.Equal(got, want) {
		t.Fatalf("the merge holds change once b is back at its first step left %v, want %v: no task stands at its acceptance", got, want)
	}
}

// seedBAtItsMerge writes on root the plan of a (`implementer`), running at its first step on the
// issue's own branch with stepRoot's #42, and b (`implementer`, whose block says merges), running
// at its merge on legion/<key>.b with its pull request #7 open at head h7. No task needs the
// other, so each merges by itself (plan.Merging), and the issue's merge pull request is a's #42,
// the first task with a merge that is not done (record.Store.MergePullRequest).
func seedBAtItsMerge(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"impl-b"}, state: record.TaskRunning, place: atMerge})
	markMerges(t, pool, root.Key, "b")
	seedTaskBranch(t, pool, root.Key, "b", "legion/"+root.Key+".b")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 7,
		Branch: "legion/" + root.Key + ".b", HeadSHA: "h7", Failing: []string{}})
}

// mergeHoldsChangeOf is the apply of an event of the merge holds document of the project LEGION,
// under the event id it is called with, through one test engine, as the Dispatch consumer applies
// it.
func mergeHoldsChangeOf(t *testing.T, pool *pgxpool.Pool) func(id string) {
	engine := testEngine(config.DesignGateRootIssues, nil)
	return func(id string) {
		t.Helper()
		if _, err := intake.ApplyFact(t.Context(), pool, "dispatch", id, intake.MergeHoldsChanged{Project: "LEGION", Event: id},
			engine, admissionStub{}); err != nil {
			t.Fatalf("apply the merge holds change %s: %v", id, err)
		}
	}
}

// mergingNowOf is what MergingNow, read in one transaction, says of key: why the workflow no
// longer works its merging phase for a merge hold, "" while it does.
func mergingNowOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var left string
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		_, left, err = MergingNow(t.Context(), record.NewStore(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the merging phase of %s: %v", key, err)
	}
	return left
}

// mergeHoldsDeliveries is every row the outbox holds, as its kind and its issue, in the order
// they were written.
func mergeHoldsDeliveries(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select kind, issue from outbox order by id")
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var kind, issue string
		if err := rows.Scan(&kind, &issue); err != nil {
			t.Fatalf("scan an outbox row: %v", err)
		}
		got = append(got, kind+" "+issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the outbox: %v", err)
	}
	return got
}

// mergeHoldsRow is the payload of issue's one merge holds change row.
func mergeHoldsRow(t *testing.T, pool *pgxpool.Pool, issue string) record.MergeHoldsChanged {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(t.Context(), "select payload from outbox where kind = 'merge_holds_changed' and issue = $1", issue).Scan(&payload); err != nil {
		t.Fatalf("read the merge holds change row of %s: %v", issue, err)
	}
	var row record.MergeHoldsChanged
	if err := json.Unmarshal(payload, &row); err != nil {
		t.Fatalf("decode the merge holds change row of %s: %v", issue, err)
	}
	return row
}
