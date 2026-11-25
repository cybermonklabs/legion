package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// releaseBuild is the build a test's release runs as: another build than testServerBuild, whose
// refresher wrote the rows the release finds.
const releaseBuild = "api-release-build"

// TestARestartThatMovesValuesSendsTheEvalsPageOneDeltaAtItsFlip (dispatch://INFRA-2916, the
// architect's 07:59Z ruling): a release whose rebuild moves values lands them in one commit, its
// flip, whose eval_days changes carry the rebuild's mark, so the Delivery page's live hub
// recomputes an evals view of them at once and sends it one delta. Over the refreshed history, three
// days of the deploy-frequency rows the main project reads in the page's window are moved away from
// what their input builds, as a build whose rules differ leaves them (their updated_at with them, so
// the stored figures' read of the change log folds them again): the rows of every key the project
// reads (eval_project_owners, each eval id mapped to the key's as the reads map it). Every deploy
// of this history is stored under an owner key of its repository (evals.OwnerKey) and none under
// the project's own key: its issues own no repository, so a pull request is held through the
// project's default repository, and a deploy that ships none counts for no issue. The running
// build's passes fold their figures, and the page takes the deltas of that write. Then a new
// build's refresher runs its passes until it has nothing left to do, on a hub whose clock stands
// still: the page gets one delta, which lands on the route's body, from one recompute of cause
// rebuild, and nothing follows it, however far the clock moves past every quiet. Before, the new
// build's walk rewrote the days one commit each, each change waiting for its quiet.
func TestARestartThatMovesValuesSendsTheEvalsPageOneDeltaAtItsFlip(t *testing.T) {
	page := newRefreshedEvalsPage(t, nil, nil)
	project := page.fixture.Project
	today := page.now.Truncate(24 * time.Hour)
	if _, err := page.rig.database.Pool.Exec(t.Context(), `
		update eval_days d set n = d.n + 2, sum = coalesce(d.sum, 0) + 2, updated_at = clock_timestamp()
		from eval_project_owners o
		where o.project_key = $1 and d.project_key = o.owner and d.eval_id = o.owner || substr($2, length($1) + 1)
			and d.writer = 'computation' and d.repo = '' and d.day in (
				select distinct r.day from eval_project_owners ro
				join eval_days r on r.project_key = ro.owner and r.eval_id = ro.owner || substr($2, length($1) + 1)
				where ro.project_key = $1 and r.n > 0 and r.day > $3 order by r.day desc limit 3)
	`, project, evals.EvalID(project, evals.SlugDeployFrequency), today.AddDate(0, 0, -27)); err != nil {
		t.Fatalf("move three days of deploy-frequency: %v", err)
	}
	page.refresh(t)
	page.rig.hub.catchUpNow()
	page.rig.clock.Advance(viewMaxWait)
	moved := page.take(t, 2*time.Second)
	page.idle(t)
	if moved == 0 {
		t.Fatal("the page was sent no delta of the moved rows, and the check needs a release that moves them back")
	}
	page.requireAtRoute(t, "the moved rows")
	before := page.route(t)

	mark := page.rig.mark()
	deps := evals.RefresherDeps{Horizon: events.NewHorizon(page.rig.database.Pool), Build: releaseBuild}
	if err := page.registry.RefreshThrough(t.Context(), deps, page.now); err != nil {
		t.Fatalf("the release's passes: %v", err)
	}
	page.rig.hub.catchUpNow()
	delta := page.conn.delta(t, "e")
	page.held = applyLiveDelta(t, evalsView, page.held, delta)
	page.requireAtRoute(t, "the release's one delta")
	if reflect.DeepEqual(liveComparable(t, mustEncodeLive(t, evalsView, before)), liveComparable(t, mustEncodeLive(t, evalsView, page.held))) {
		t.Fatal("the route answers after the release what it answered before, and the check needs a release that moves it")
	}
	page.rig.clock.Advance(viewMaxWait)
	page.conn.none(t, time.Second, "the release's one delta, past every quiet")
	begun, rebuilds := page.rig.count(mark, recomputeBegin("")), page.rig.count(mark, recomputeBegin(deliveryCauseRebuild))
	if begun != 1 || rebuilds != 1 {
		t.Fatalf("the release began %d recomputes of the page's view, %d of them %s, want the one", begun, rebuilds, deliveryCauseRebuild)
	}
	t.Logf("the write that moved three days sent %d deltas; the release's flip was recomputed once (%s) on a clock standing still, and its one delta upserts %v and lands on the route's body",
		moved, deliveryCauseRebuild, liveUpsertIDs(t, delta, "items"))
}
