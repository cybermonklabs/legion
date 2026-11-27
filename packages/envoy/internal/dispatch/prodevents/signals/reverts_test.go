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
		*row.RestoredByDeploy != restoring.ID || row.RestoredBy == nil || *row.RestoredBy != loopActor {
		t.Fatalf("revert row restored at %v by deploy %v (%v), want %v by %s", row.RestoredAt, row.RestoredByDeploy, row.RestoredBy,
			restoring.FinishedAt, restoring.ID)
	}

	r.passDeploys()
	if rows := r.failures(); len(rows) != 1 {
		t.Fatalf("after a repeat, failure rows = %+v, want the one", rows)
	}
}

// TestARevertBlamesNoDeployAnotherProjectPosted: a deploy posted to BETA that names acme/widgets#5
// and finished first is never blamed by ALPHA's revert of #5, which blames ALPHA's GitHub deploy
// of #5 instead.
func TestARevertBlamesNoDeployAnotherProjectPosted(t *testing.T) {
	r := newRig(t)
	r.project("BETA", "acme/beta")
	r.pullRequest(5, at(30), sha("5"), "")
	r.deployRun(410, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	shipped := r.deployOf(410)
	r.postDeploy("BETA", "ci:beta-5", at(40), []string{sha("5")}, []string{"acme/widgets#5"})

	if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
		Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
		MergedAt: at(90), MergeCommitSHA: new(sha("9")),
	}); err != nil {
		t.Fatalf("record the revert: %v", err)
	}
	r.deployRun(411, at(120), sha("b"), prodJob, success, success)
	r.passDeploys()
	rows := r.failures()
	if len(rows) != 1 || *rows[0].DeployID != shipped.ID {
		t.Fatalf("failure rows = %+v, want one blaming ALPHA's deploy %s rather than BETA's earlier post", rows, shipped.ID)
	}
}

// TestARevertWhoseDeployFinishedBeforeTheBlamedDeployMakesNoRow: a revert whose restoring deploy
// finished before the deploy the revert blames cannot restore a failure of that deploy, so the
// revert signal writes no row for it; a revert shipped by the same deploy whose blamed deploy
// finished before it still writes its row.
func TestARevertWhoseDeployFinishedBeforeTheBlamedDeployMakesNoRow(t *testing.T) {
	r := newRig(t)
	r.postDeploy(alpha, "ci:after", at(200), nil, []string{"acme/other#5"})
	earlier := r.postDeploy(alpha, "ci:before", at(50), nil, []string{"acme/other#6"})
	for _, revert := range []delivery.Revert{
		{Repo: widgets, Number: 9, RevertsRepo: "acme/other", RevertsNumber: 5, MergedAt: at(90)},
		{Repo: widgets, Number: 10, RevertsRepo: "acme/other", RevertsNumber: 6, MergedAt: at(95)},
	} {
		if err := delivery.RecordRevert(r.ctx, r.pool, revert); err != nil {
			t.Fatalf("record revert #%d: %v", revert.Number, err)
		}
	}
	// Run 420 starts at(120) and finishes at(130): before ci:after (at(200)) and after ci:before (at(50)).
	r.deployRun(420, at(120), sha("c"), prodJob, success, success)
	r.passDeploys()
	restoring := r.deployOf(420)
	rows := r.failures()
	if len(rows) != 1 || rows[0].SourceRef != RevertSourceRef(widgets, 10) || *rows[0].DeployID != earlier.ID ||
		rows[0].RestoredByDeploy == nil || *rows[0].RestoredByDeploy != restoring.ID {
		t.Fatalf("failure rows = %+v, want only revert #10 blaming %s restored by %s", rows, earlier.ID, restoring.ID)
	}
}
