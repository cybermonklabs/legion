package signals

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

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

// TestARevertWhoseDeployFinishedBeforeTheBlamedDeployWaitsForItsDeployToMove (dispatch://INFRA-978,
// R8 of the review at 20c604ef, and the architect's ruling
// dispatch://INFRA-978/comment/91d5e75b-57eb-4b76-ba55-5a101fec2c32): a revert whose own deploy
// finished before the deploy the revert blames restores no failure of that deploy, so the revert
// writes no row while that holds. It is no refusal: it is held, logged once for that judgement, and
// a later pass that finds the same deploys and finishes writes nothing and logs nothing. Once its
// own deploy finishes after the blamed one, as a GitHub re-run that moves that deploy's finish in
// place or a rule change that makes a later run its ship run makes it, the next pass writes its row,
// restored by that deploy, and the revert holds nothing. A revert shipped by the same deploy whose
// blamed deploy finished before it writes its row on the first pass.
func TestARevertWhoseDeployFinishedBeforeTheBlamedDeployWaitsForItsDeployToMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		// move makes the revert's own deploy finish after ci:after, the deploy it blames: the deploy
		// of run ship.
		move func(r *rig)
		ship int64
	}{
		{"a re-run moves the finish of the revert's own deploy", func(r *rig) { r.rerun(420, at(120), sha("c"), canaryJob, at(240)) }, 420},
		{"a rule change makes a later run its ship run", func(r *rig) { r.saveRule(prodJob) }, 421},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			lines := r.logLines()
			held := func() []string { return lines("", "key", "deploy", "restoring", "hold") }
			refusals := func() []string { return lines(refusedMessage, "signal", "key", "deploy") }
			r.saveRule(prodJob, canaryJob)
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
			// Run 420, a canary run, runs from at(120) to at(130): before ci:after (at(200)) and after
			// ci:before (at(50)). Run 421, a production run, runs from at(250) to at(260).
			r.deployRun(420, at(120), sha("c"), canaryJob, success, success)
			r.deployRun(421, at(250), sha("d"), prodJob, success, success)
			for _, step := range []string{"the pass that ships the reverts", "a later pass"} {
				r.passDeploys()
				first := r.deployOf(420)
				rows := r.failures()
				if len(rows) != 1 || rows[0].SourceRef != RevertSourceRef(widgets, 10) || *rows[0].DeployID != earlier.ID ||
					rows[0].RestoredByDeploy == nil || *rows[0].RestoredByDeploy != first.ID {
					t.Fatalf("after %s, failure rows = %+v, want only revert #10 blaming %s restored by %s", step, rows, earlier.ID, first.ID)
				}
				if logged, want := held(), []string{"acme/widgets#9 " + later.ID + " " + first.ID + " too_early"}; !slices.Equal(logged, want) {
					t.Errorf("after %s, the holds logged = %q; want %q, once", step, logged, want)
				}
				if logged := refusals(); len(logged) != 0 {
					t.Errorf("after %s, the refusals logged = %q; want none", step, logged)
				}
			}

			restoring := r.deployOf(tc.ship)
			tc.move(r)
			r.passDeploys()
			moved := r.deployOf(tc.ship)
			if moved.ID != restoring.ID || !moved.FinishedAt.After(*later.FinishedAt) {
				t.Fatalf("after the move, run %d's row is %s finished at %v; want %s, finished after ci:after at %v",
					tc.ship, moved.ID, moved.FinishedAt, restoring.ID, later.FinishedAt)
			}
			var revert *prodevents.Event
			rows := r.failures()
			for i := range rows {
				if rows[i].SourceRef == RevertSourceRef(widgets, 9) {
					revert = &rows[i]
				}
			}
			if len(rows) != 2 || revert == nil || *revert.DeployID != later.ID || revert.RestoredByDeploy == nil ||
				*revert.RestoredByDeploy != moved.ID || revert.RestoredAt == nil || !revert.RestoredAt.Equal(*moved.FinishedAt) {
				t.Fatalf("after the move, failure rows = %+v; want revert #10's and revert #9's, blaming %s, restored by %s at its finish",
					rows, later.ID, moved.ID)
			}
			if logged := held(); len(logged) != 1 {
				t.Errorf("after the move, the holds logged = %q; want the one logged before it", logged)
			}
			if logged := refusals(); len(logged) != 0 {
				t.Errorf("after the move, the refusals logged = %q; want none", logged)
			}
			var kept int
			if err := r.pool.QueryRow(r.ctx, `select count(*) from revert_refusals`).Scan(&kept); err != nil || kept != 0 {
				t.Errorf("after the move, revert_refusals holds %d rows (%v); want none, the hold dropped with the revert's row", kept, err)
			}
		})
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

// TestARefusedRevertStaysRefusedThroughItsDeploysMoves (the architect's rulings,
// dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706 and
// dispatch://INFRA-978/comment/91d5e75b-57eb-4b76-ba55-5a101fec2c32): acme/widgets#5, BETA-1's pull
// request, is the one pull request run 410 ships, so 410's deploy counts for BETA alone, and ALPHA's
// revert of #5, which finished no later than its own deploy, is refused on that deploy, logged
// once. It stays refused through the moves that judge a held revert again: a GitHub re-run that
// moves its own deploy's finish in place, and a rule change that makes a later run its ship run.
// Each pass after either writes no row, holds nothing and logs nothing.
func TestARefusedRevertStaysRefusedThroughItsDeploysMoves(t *testing.T) {
	for _, tc := range []struct {
		name string
		// move makes the deploy of run ship the revert's own deploy, finished later than before it.
		move func(r *rig)
		ship int64
	}{
		{"a re-run moves the finish of the revert's own deploy", func(r *rig) { r.rerun(411, at(100), sha("b"), canaryJob, at(240)) }, 411},
		{"a rule change makes a later run its ship run", func(r *rig) { r.saveRule(prodJob) }, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			lines := r.logLines()
			r.saveRule(prodJob, canaryJob)
			r.project("BETA", "acme/beta")
			r.pullRequest(5, at(30), sha("5"), r.issueOf("BETA", 1))
			r.deployRun(410, at(60), sha("a"), prodJob, success, success)
			if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
				Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
				MergedAt: at(90), MergeCommitSHA: new(sha("9")),
			}); err != nil {
				t.Fatalf("record the revert: %v", err)
			}
			// Run 411, a canary run, is the revert's ship run under the first rule, from at(100) to
			// at(110); run 412, a production run, from at(150) to at(160), is the first production run
			// after the revert.
			r.deployRun(411, at(100), sha("b"), canaryJob, success, success)
			r.deployRun(412, at(150), sha("c"), prodJob, success, success)
			r.passDeploys()
			blamed := r.deployOf(410)
			before := r.deployOf(tc.ship)
			requireRefused := func(step string) {
				t.Helper()
				var refused []string
				rows, err := r.pool.Query(r.ctx, `select deploy_id::text from revert_refusals where repo = $1 and number = 9`, widgets)
				if err == nil {
					refused, err = pgx.CollectRows(rows, pgx.RowTo[string])
				}
				if err != nil || !slices.Equal(refused, []string{blamed.ID}) {
					t.Errorf("%s, the revert holds refusals and holds of %v (%v); want its refusal of run 410's deploy %s alone", step, refused, err, blamed.ID)
				}
				var failures int
				if err := r.pool.QueryRow(r.ctx, `select count(*) from production_events where kind = 'failure'`).Scan(&failures); err != nil || failures != 0 {
					t.Errorf("%s, the record holds %d failure rows across every project (%v); want none", step, failures, err)
				}
				if logged, want := lines(refusedMessage, "signal", "key", "deploy"), []string{prodevents.SignalRevert + " acme/widgets#9 " + blamed.ID}; !slices.Equal(logged, want) {
					t.Errorf("%s, the refusals logged = %q; want %q, once", step, logged, want)
				}
				if logged := lines("", "key", "restoring"); len(logged) != 0 {
					t.Errorf("%s, the holds logged = %q; want none", step, logged)
				}
			}

			requireRefused("under the first rule")
			tc.move(r)
			r.passDeploys()
			if moved := r.deployOf(tc.ship); moved.ID != before.ID || !moved.FinishedAt.After(*blamed.FinishedAt) {
				t.Fatalf("after the move, run %d's row is %s finished at %v; want %s, finished after run 410's deploy", tc.ship, moved.ID, moved.FinishedAt, before.ID)
			}
			requireRefused("after the move")
			r.passDeploys()
			requireRefused("a pass after the move")
		})
	}
}

// TestARevertWhoseBlamedDeployCountsForNoProjectIsHeldUntilItCounts (the architect's ruling,
// dispatch://INFRA-978/comment/527e19cd-344b-4a20-9a4b-b7d515da91b0, for the revert signal): ALPHA
// reads acme/widgets as a named source and its default repository is another, so #5, which no issue
// attributes, and run 440's deploy that ships it count for no project. ALPHA's revert of #5 then
// writes no row and is not refused: it is held, logged once over two passes with the deploy it
// blames and its own. Once #5 is attributed to ALPHA-1, so that deploy counts for ALPHA, the next
// pass writes the revert's row, and the revert holds nothing.
func TestARevertWhoseBlamedDeployCountsForNoProjectIsHeldUntilItCounts(t *testing.T) {
	r := newRig(t)
	lines := r.logLines()
	r.exec("move ALPHA's default repository", `update projects set default_repo = 'acme/alpha' where key = $1`, alpha)
	r.saveSource(alpha, new(widgets), revertsSince, prodJob)
	r.pullRequest(5, at(30), sha("5"), "")
	r.deployRun(440, at(60), sha("a"), prodJob, success, success)
	if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
		Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
		MergedAt: at(90), MergeCommitSHA: new(sha("9")),
	}); err != nil {
		t.Fatalf("record the revert: %v", err)
	}
	r.deployRun(441, at(120), sha("b"), prodJob, success, success)
	for _, step := range []string{"the pass that ships the revert", "a later pass"} {
		r.passDeploys()
		blamed, restoring := r.deployOf(440), r.deployOf(441)
		facts, err := storedDeployFacts(r.ctx, r.pool, blamed)
		if err != nil {
			t.Fatalf("after %s, read the facts of run 440's deploy: %v", step, err)
		}
		if projects := prodevents.DeployProjects(blamed, facts); len(projects) != 0 {
			t.Fatalf("after %s, run 440's deploy counts for %v; want no project", step, projects)
		}
		if rows := r.failures(); len(rows) != 0 {
			t.Fatalf("after %s, failure rows = %+v; want none", step, rows)
		}
		if logged, want := lines("", "key", "deploy", "restoring", "hold"), []string{"acme/widgets#9 " + blamed.ID + " " + restoring.ID + " no_project"}; !slices.Equal(logged, want) {
			t.Errorf("after %s, the holds logged = %q; want %q, once", step, logged, want)
		}
		if logged := lines(refusedMessage, "key"); len(logged) != 0 {
			t.Errorf("after %s, the refusals logged = %q; want none", step, logged)
		}
	}

	r.pullRequest(5, at(30), sha("5"), r.issue(1))
	r.passDeploys()
	blamed, restoring := r.deployOf(440), r.deployOf(441)
	if rows := r.failures(); len(rows) != 1 || rows[0].SourceRef != RevertSourceRef(widgets, 9) || *rows[0].DeployID != blamed.ID ||
		rows[0].RestoredByDeploy == nil || *rows[0].RestoredByDeploy != restoring.ID {
		t.Fatalf("once #5 is ALPHA-1's, failure rows = %+v; want the revert's, blaming %s, restored by %s", rows, blamed.ID, restoring.ID)
	}
	var kept int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from revert_refusals`).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("once the revert's row is written, revert_refusals holds %d rows (%v); want none", kept, err)
	}
}
