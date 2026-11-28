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

// TestARevertWhoseDeployFinishedBeforeTheBlamedDeployIsRefusedOnIt (dispatch://INFRA-978, R8 of the
// review at 20c604ef): a revert whose restoring deploy finished before the deploy the revert blames
// cannot restore a failure of that deploy, so the revert signal writes no row for it, and refuses
// the revert on that deploy (revert_refusals), logged once: a later pass writes nothing and logs
// nothing again. A revert shipped by the same deploy whose blamed deploy finished before it still
// writes its row, and is refused on nothing.
func TestARevertWhoseDeployFinishedBeforeTheBlamedDeployIsRefusedOnIt(t *testing.T) {
	r := newRig(t)
	refusals := r.logRefusals()
	later := r.postDeploy(alpha, "ci:after", at(200), nil, []string{"acme/other#5"})
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
	refusedOn := func() map[int]string {
		t.Helper()
		rows, err := r.pool.Query(r.ctx, `select number, deploy_id::text from revert_refusals where repo = $1`, widgets)
		if err != nil {
			t.Fatalf("read the revert refusals: %v", err)
		}
		defer rows.Close()
		refused := map[int]string{}
		for rows.Next() {
			var number int
			var deploy string
			if err := rows.Scan(&number, &deploy); err != nil {
				t.Fatalf("read the revert refusals: %v", err)
			}
			refused[number] = deploy
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the revert refusals: %v", err)
		}
		return refused
	}
	for _, step := range []string{"the pass that ships the reverts", "a later pass"} {
		r.passDeploys()
		restoring := r.deployOf(420)
		rows := r.failures()
		if len(rows) != 1 || rows[0].SourceRef != RevertSourceRef(widgets, 10) || *rows[0].DeployID != earlier.ID ||
			rows[0].RestoredByDeploy == nil || *rows[0].RestoredByDeploy != restoring.ID {
			t.Fatalf("after %s, failure rows = %+v, want only revert #10 blaming %s restored by %s", step, rows, earlier.ID, restoring.ID)
		}
		if refused := refusedOn(); len(refused) != 1 || refused[9] != later.ID {
			t.Errorf("after %s, the reverts are refused on %v; want #9 on ci:after's deploy %s alone", step, refused, later.ID)
		}
		if logged, want := refusals(), []string{prodevents.SignalRevert + " acme/widgets#9 " + later.ID}; !slices.Equal(logged, want) {
			t.Errorf("after %s, the refusals logged = %q; want %q, once", step, logged, want)
		}
	}
}

// TestARevertIsReadWhileItCanMakeItsFailure (dispatch://INFRA-978, R7): the reverts read
// (delivery.ListRevertShips) holds a revert that a deploy run ships, that no revert row of any
// project holds yet, and whose run completed on or after the earliest day a delivery-on project
// counts reverts from. While ALPHA, the one such project, counts them from the day after the
// revert's deploy, the read does not hold the revert and the pass makes no row; once ALPHA counts
// them from the day before, the read holds it and the next pass makes its row, after which the
// read holds it no more.
func TestARevertIsReadWhileItCanMakeItsFailure(t *testing.T) {
	r := newRig(t)
	r.pullRequest(5, at(30), sha("5"), "")
	r.deployRun(430, at(60), sha("a"), prodJob, success, success)
	if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
		Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
		MergedAt: at(90), MergeCommitSHA: new(sha("9")),
	}); err != nil {
		t.Fatalf("record the revert: %v", err)
	}
	r.deployRun(431, at(120), sha("b"), prodJob, success, success)
	read := func() []delivery.RevertShip {
		t.Helper()
		ships, err := delivery.ListRevertShips(r.ctx, r.pool, rule(prodJob), prodevents.SourceReverts, prodevents.RevertSourceRefPrefix)
		if err != nil {
			t.Fatalf("read the reverts: %v", err)
		}
		return ships
	}

	r.saveSource(alpha, nil, base.AddDate(0, 0, 1), prodJob)
	r.passDeploys()
	if ships, rows := read(), r.failures(); len(ships) != 0 || len(rows) != 0 {
		t.Fatalf("with reverts counted from the day after the revert's deploy, the read holds %+v and the failure rows are %+v; want neither",
			ships, rows)
	}

	r.saveRule(prodJob)
	if ships := read(); len(ships) != 1 || ships[0].Number != 9 || ships[0].ShipRunID != 431 {
		t.Fatalf("with reverts counted from the day before, the read holds %+v; want the revert #9 run 431 ships", ships)
	}
	r.passDeploys()
	if rows := r.failures(); len(rows) != 1 || rows[0].SourceRef != RevertSourceRef(widgets, 9) {
		t.Fatalf("after the pass, failure rows = %+v; want the revert's", rows)
	}
	if ships := read(); len(ships) != 0 {
		t.Errorf("once its row is written, the read holds %+v; want the revert no more", ships)
	}
}

// TestARefusedRevertStaysRefusedThroughASourceMoveAndBack (the architect's ruling,
// dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706): acme/widgets#5, BETA-1's pull
// request, is the one pull request run 410 ships, so 410's deploy counts for BETA alone. The revert
// of #5, which run 411 ships, blames 410's deploy, and while acme/widgets's one source is ALPHA's
// its row would be ALPHA's: it is refused on that deploy, logged once, and it stays refused. When
// the source moves to BETA, so the revert's row would be BETA's, for whom 410's deploy counts, the
// pass reads the revert again and writes no row; when the source moves back, the record holds no
// failure row of any project.
func TestARefusedRevertStaysRefusedThroughASourceMoveAndBack(t *testing.T) {
	r := newRig(t)
	refusals := r.logRefusals()
	r.project("BETA", "acme/beta")
	r.pullRequest(5, at(30), sha("5"), r.issueOf("BETA", 1))
	r.deployRun(410, at(60), sha("a"), prodJob, success, success)
	if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
		Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
		MergedAt: at(90), MergeCommitSHA: new(sha("9")),
	}); err != nil {
		t.Fatalf("record the revert: %v", err)
	}
	r.deployRun(411, at(120), sha("b"), prodJob, success, success)
	r.passDeploys()
	blamed := r.deployOf(410)
	requireRefused := func(step, owner string) {
		t.Helper()
		for _, run := range []int64{410, 411} {
			if row := r.deployOf(run); row.ProjectKey != owner {
				t.Fatalf("%s, run %d's row is %s's; want %s's", step, run, row.ProjectKey, owner)
			}
		}
		moved := r.deployOf(410)
		facts, err := storedDeployFacts(r.ctx, r.pool, moved)
		if err != nil {
			t.Fatalf("%s, read the facts of run 410's deploy: %v", step, err)
		}
		if projects := prodevents.DeployProjects(moved, facts); moved.ID != blamed.ID || !slices.Equal(projects, []string{"BETA"}) {
			t.Fatalf("%s, run 410's row is %s, counting for %v; want %s, counting for BETA alone", step, moved.ID, projects, blamed.ID)
		}
		var refused bool
		if err := r.pool.QueryRow(r.ctx, `select exists (select from revert_refusals where repo = $1 and number = 9 and deploy_id = $2::uuid)`,
			widgets, blamed.ID).Scan(&refused); err != nil || !refused {
			t.Errorf("%s, the revert holds a refusal of run 410's deploy %t (%v); want it held", step, refused, err)
		}
		var rows int
		if err := r.pool.QueryRow(r.ctx, `select count(*) from production_events where kind = 'failure'`).Scan(&rows); err != nil || rows != 0 {
			t.Errorf("%s, the record holds %d failure rows across every project (%v); want none", step, rows, err)
		}
	}

	requireRefused("under ALPHA's source", alpha)
	r.deliveryOff(alpha)
	r.saveSource("BETA", new(widgets), revertsSince, prodJob)
	r.passDeploys()
	requireRefused("once the source moved to BETA", "BETA")
	r.deliveryOff("BETA")
	r.saveRule(prodJob)
	r.passDeploys()
	requireRefused("once the source moved back to ALPHA", alpha)
	if logged, want := refusals(), []string{prodevents.SignalRevert + " acme/widgets#9 " + blamed.ID}; !slices.Equal(logged, want) {
		t.Errorf("the refusals logged = %q; want %q, once", logged, want)
	}
}
