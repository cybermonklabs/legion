package admit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A confirmed conflict is merged forward in the phase the issue stands in, with no phase moved and
// no start written (dispatch://INFRA-4374 R1), so it changes nothing of what the cap counts: an
// issue past its review, in accepting, retro or merging, takes no slot for the merge, and an issue
// in testing keeps the slot it already held (record.KeepsSlot). The conflict is read through the
// workflow engine as the daemon reads it, two CONFLICTING reads of the head a minute apart against
// a base that requires a check the head has not passed, so the merge forward is asked in every
// phase. The cap has room for every issue here, so a merge forward that owed a worker a start
// would put the issue in the waiting line and the pass would grant it a slot; none does. The
// active set the state shows (projection.Project's Admission.Active) agrees with the slot rows.
func TestAConflictKeepsPhaseAndTakesNoSlot(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	cases := []struct {
		row    rowRuleCase
		number int
	}{
		{rowRuleCase{key: "LEGION-450", stands: phase.Accepting, status: "needs_review",
			why: "the architect reads a reviewed head here, and the merge forward starts nobody"}, 450},
		{rowRuleCase{key: "LEGION-451", stands: phase.Retro, status: "retro",
			why: "the retro writes no more of the change, and the merge forward starts nobody"}, 451},
		{rowRuleCase{key: "LEGION-452", stands: phase.Merging, status: "retro",
			why: "the merger merges work already reviewed, and the merge forward starts nobody"}, 452},
		{rowRuleCase{key: "LEGION-453", stands: phase.Testing, status: "testing", seeded: true, holds: true,
			why: "a worker is testing the change, and the merge forward leaves it the slot it held"}, 453},
	}
	for index, tc := range cases {
		rowRuleRoot(t, pool, tc.row, string(rune('a'+index)))
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: tc.row.key,
				Repo: "acme/widgets", Number: tc.number, Branch: "legion/" + tc.row.key, HeadSHA: "head", Base: "main", Failing: []string{}}); err != nil {
				t.Fatalf("seed the pull request of %s: %v", tc.row.key, err)
			}
		})
	}
	admission := quiet(t, len(cases)+2)

	for _, tc := range cases {
		for _, after := range []time.Duration{0, time.Minute} {
			read := intake.PullRequestMergeability{Repo: "acme/widgets", Number: tc.number, Base: "main", HeadSHA: "head",
				Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, ReadAt: fixedNow.Add(after),
				Rules: &record.BaseRules{Required: []string{"ci"}}}
			if result, err := intake.ApplyFact(ctx, pool, "github", "conflict-"+tc.row.key+"-"+after.String(), read, engine, admission); err != nil || result.Refusal != nil {
				t.Fatalf("apply the conflicting read of %s %s after the first = %+v, %v", tc.row.key, after, result.Refusal, err)
			}
		}
	}
	tick(t, pool, admission, "the pass after the merge forwards")

	var active []string
	inTx(t, pool, func(tx pgx.Tx) {
		state, err := projection.Project(ctx, tx, record.NewStore(), testProject, nil, nil, nil, nil, nil, fixedNow)
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		active = state.Admission.Active
	})
	for _, tc := range cases {
		key := tc.row.key
		if got := issue(t, pool, key).Phase; got != tc.row.stands {
			t.Fatalf("%s is in %s after the confirmed conflict, want it still in %s", key, got, tc.row.stands)
		}
		var pr *record.PullRequest
		inTx(t, pool, func(tx pgx.Tx) {
			var err error
			if pr, err = record.NewStore().PullRequest(ctx, tx, key); err != nil {
				t.Fatalf("read the pull request of %s: %v", key, err)
			}
		})
		if asked := pr.MergeForward; asked == nil || asked.Head != "head" || asked.Phase != tc.row.stands {
			t.Fatalf("the merge forward of %s = %+v, want one asked of head in %s", key, asked, tc.row.stands)
		}
		var starts int
		if err := pool.QueryRow(ctx, "select count(*) from outbox where kind = 'supervise' and issue = $1", key).Scan(&starts); err != nil {
			t.Fatalf("count the starts of %s: %v", key, err)
		}
		if starts != 0 {
			t.Fatalf("%s holds %d supervise rows after the merge forward, want none: the merge forward starts no phase", key, starts)
		}
		if held := holdsSlot(t, pool, key); held != tc.row.holds {
			t.Fatalf("after the pass %s in %s holds a slot = %t, want %t: %s", key, tc.row.stands, held, tc.row.holds, tc.row.why)
		}
		if shown := slices.Contains(active, key); shown != tc.row.holds {
			t.Fatalf("the state's active set %v holds %s = %t, want %t: %s", active, key, shown, tc.row.holds, tc.row.why)
		}
	}
}
