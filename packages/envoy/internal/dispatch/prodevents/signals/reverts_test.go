package signals

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// TestARevertsOwnDeployRestoresTheFailureItMade: a merged revert of acme/widgets#5, recorded
// whoever authored it, makes no row while no deploy ships it; once the deploy that ships it has its
// row, it is one counted revert row of the repository's owner, blaming the deploy that shipped #5,
// restored at the revert's deploy's finish by that deploy. A repeat writes no second row.
func TestARevertsOwnDeployRestoresTheFailureItMade(t *testing.T) {
	r := newRig(t)
	r.pullRequest(5, at(30), sha("5"), "")
	r.deployRun(400, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	shipped := r.deployOf(400)

	if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
		Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
		MergedAt: at(90), MergeCommitSHA: new(sha("9")),
	}); err != nil {
		t.Fatalf("record the revert: %v", err)
	}
	r.passDeploys()
	if rows := r.failures(); len(rows) != 0 {
		t.Fatalf("with no deploy shipping the revert, failure rows = %+v, want none", rows)
	}

	r.deployRun(401, at(120), sha("b"), prodJob, success, success)
	r.passDeploys()
	restoring := r.deployOf(401)
	rows := r.failures()
	if len(rows) != 1 {
		t.Fatalf("once the revert's deploy landed, failure rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.ProjectKey != alpha || row.Source != prodevents.SourceReverts || *row.Signal != prodevents.SignalRevert ||
		row.SourceRef != "revert:acme/widgets#9" || *row.DeployID != shipped.ID || row.State != prodevents.StateCounted ||
		!slices.Equal(row.PullRequests, []string{"acme/widgets#5"}) || !row.DetectedAt.Equal(at(90)) {
		t.Fatalf("revert row = %+v", row)
	}
	if row.RestoredAt == nil || !row.RestoredAt.Equal(*restoring.FinishedAt) || row.RestoredByDeploy == nil ||
		*row.RestoredByDeploy != restoring.ID || row.RestoredBy == nil || *row.RestoredBy != Actor {
		t.Fatalf("revert row restored at %v by deploy %v (%v), want %v by %s", row.RestoredAt, row.RestoredByDeploy, row.RestoredBy,
			restoring.FinishedAt, restoring.ID)
	}

	r.passDeploys()
	if rows := r.failures(); len(rows) != 1 {
		t.Fatalf("after a repeat, failure rows = %+v, want the one", rows)
	}
}
