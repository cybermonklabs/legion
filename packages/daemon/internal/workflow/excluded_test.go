package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The listener counted ci-ok's run 20 for the head, which failed, then learned that GitHub does not
// count its workflow run for the pull request and settles again with no run of ci-ok at all,
// naming run 20 excluded. The settlement reaches the record through the engine and the store: the
// fence keeps no run of ci-ok, nothing is failing, the record keeps run 20 as excluded, and the
// roles are told the head's CI now reports unit alone. The settlement from before the exclusion,
// delivered after it, changes nothing and tells nobody: the store kept the exclusion, so the
// excluded run does not come back.
func TestAnExcludedRunLeavesTheRecordThroughTheEngine(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	// The issue is the one seedSettlingRound seeds, read off the pull request #42 it seeds.
	var seeded *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		seeded, err = record.NewStore().PullRequestByNumber(t.Context(), tx, "sjawhar/legion", 42)
		return err
	})
	if seeded == nil {
		t.Fatal("seedSettlingRound recorded no pull request #42")
	}
	issue := seeded.Issue
	engine := mergingEngine()
	before := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", Generation: 2, Snapshot: "head-2",
		CheckRuns: []record.AttemptRun{{Name: "ci-ok", ID: 20}, {Name: "unit", ID: 15}}, Failing: []string{"ci-ok"}, Cancelled: []string{}}
	correction := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", Generation: 3, Snapshot: "head-3",
		CheckRuns: []record.AttemptRun{{Name: "unit", ID: 15}}, Failing: []string{}, Cancelled: []string{},
		Excluded: []record.AttemptRun{{Name: "ci-ok", ID: 20}}}

	applyRefusingNothing(t, pool, engine, before)
	messages := tell(t, []record.RoleMessage{}, `CI settled at head: "ci-ok" failed, "unit" succeeded`)
	assertRoleMessages(t, pool, issue, messages)

	applyRefusingNothing(t, pool, engine, correction)
	corrected := recordedPR(t, pool, issue)
	if want := []record.AttemptRun{{Name: "unit", ID: 15, Head: "head", Conclusion: record.RunSucceeded}}; !reflect.DeepEqual(corrected.CheckRuns, want) ||
		len(corrected.Failing) != 0 || corrected.Generation != 3 {
		t.Fatalf("after the correction the fence is %+v, failing %v at generation %d; want %+v, nothing failing, at 3",
			corrected.CheckRuns, corrected.Failing, corrected.Generation, want)
	}
	if want := []record.AttemptRun{{Name: "ci-ok", ID: 20}}; !reflect.DeepEqual(corrected.ExcludedRuns, want) {
		t.Fatalf("the record keeps excluded runs %+v, want %+v", corrected.ExcludedRuns, want)
	}
	messages = tell(t, messages, `CI settled at head: "unit" succeeded`)
	assertRoleMessages(t, pool, issue, messages)

	applyRefusingNothing(t, pool, engine, before)
	if got := recordedPR(t, pool, issue); !reflect.DeepEqual(got, corrected) {
		t.Fatalf("after the settlement from before the exclusion the record is\n  %+v\nwant it as the correction left it:\n  %+v", got, corrected)
	}
	assertRoleMessages(t, pool, issue, messages)
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("the issue is in %s, want testing", got)
	}
}
