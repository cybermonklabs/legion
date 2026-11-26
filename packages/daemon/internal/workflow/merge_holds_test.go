package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

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
// an issue in another phase or for one out of the workflow by its own status (no tree lingers:
// R20 of dispatch://INFRA-3320). The same event delivered again writes nothing more, since
// ApplyFact records the event's id, and an event of another project writes nothing.
func TestAMergeHoldsChangeTellsTheArchitectInAcceptingAndTheMergerInMerging(t *testing.T) {
	pool := migratedPool(t)
	seed := func(key string, at phase.Phase, status string) {
		seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key,
			Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: status, Rank: "U"})
	}
	seed("LEGION-208", phase.Accepting, "needs_review")
	seed("LEGION-209", phase.Merging, "retro")
	seed("LEGION-210", phase.Implementing, "in_progress")
	seed("LEGION-211", phase.Merging, "backlog")

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
// workflow (closeMerged), and a merge hold stops nothing there (PhaseFinished), so a merge holds
// change writes its merger no row, and MergingNow, which the row's executor reads before each
// attempt, says the issue left merging. The control is a root in merging whose pull request is
// still open: it gets its row, and MergingNow says it works merging.
func TestAMergeHoldsChangeTellsNoMergerWhosePullRequestMerged(t *testing.T) {
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
		var left string
		if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			var err error
			_, left, err = MergingNow(t.Context(), record.NewStore(), tx, key)
			return err
		}); err != nil {
			t.Fatalf("read the merging phase of %s: %v", key, err)
		}
		if left != want {
			t.Fatalf("MergingNow(%s) says %q, want %q", key, left, want)
		}
	}
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
