package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A merge forward's message the pause held for a role released before the pause wakes that role
// after the resume with the pause's held wake, which pairs with no release (decision 15 of
// dispatch://INFRA-3759, store.Store.TakePodWake). The pause did not stop the role, since it was
// already released, so no stop says the wake is the pause's (store.StoppingPauseTx answers 0) and
// the project is no longer paused when the row runs: the row's mark (record.OutboxRow.HeldByPause),
// which deliverHeld passes to wakeForDelivery, is the one thing that does. Without it the wake
// pairs with the release from before the pause, and the release records count the pause's wait as
// that release's. deliverHeld is the delivery a merge-queue removal shares, so this holds for both.
func TestAHeldMergeForwardMessageWakesARoleReleasedBeforeThePauseWithThatPause(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.Gates.Design = config.DesignGateOff.Steps() })
	issue := r.issue(1)
	head := "c0de000000000000000000000000000000000001"
	r.seed(r.root(issue, phase.Testing), r.running(issue, claim.RoleTester, supervise.StateIdle),
		r.released(issue, claim.RoleImplementer))
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutPullRequest(r.ctx, tx, record.PullRequest{Issue: issue, Repo: liveRepo, Number: 42,
			Branch: "legion/" + issue, Base: "main", HeadSHA: head, HeadUpdatedAt: r.clock.Now().Add(-time.Hour).UTC(),
			State: record.PullRequestOpen, MergeForward: &record.MergeForward{Head: head, Base: "main", Phase: phase.Testing,
				Why: record.ConflictCheckNotGreen, AskedAt: r.clock.Now().Add(-time.Hour).UTC()}})
	}); err != nil {
		t.Fatalf("record the pull request of %s with its merge forward: %v", issue, err)
	}
	// The idle release that took the implementer's pod before the pause, which a wake no pause held
	// would pair with.
	if _, err := r.pool.Exec(r.ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, $2, 1, 'implementer', $3)`, r.project, issue, r.clock.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("record a release of %s's pod: %v", issue, err)
	}
	r.boot()

	pause := r.pauseProject()
	r.awaitState(r.token(issue, claim.RoleTester), supervise.StateSuspended, "the pause stops the idle tester")
	if got := r.count(`select count(*) from project_pause_stops where claim = $1`, string(r.token(issue, claim.RoleImplementer))); got != 0 {
		t.Fatalf("the pause recorded %d stops of the implementer it found released, want none: this case proves nothing", got)
	}
	ask := r.enqueue(issue, record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: head,
		Base: "main", Phase: phase.Testing, PullRequest: 42, Why: record.ConflictCheckNotGreen})
	r.resumeProject()
	if got := r.heldByPause(ask); got != pause.ID {
		t.Fatalf("the resume marked the held ask with pause %d, want %d", got, pause.ID)
	}
	testwait.Eventually(t, "the held ask to wake the implementer", func() bool {
		return r.count(`select count(*) from pod_resumes where issue = $1 and role = 'implementer' and kind = $2`, issue, pods.WakeDelivery) == 1
	})
	var held *int64
	var release int64
	if err := r.pool.QueryRow(r.ctx, `select pause_id, release_id from pod_resumes where issue = $1 and role = 'implementer' and kind = $2`,
		issue, pods.WakeDelivery).Scan(&held, &release); err != nil {
		t.Fatalf("read the ask's wake: %v", err)
	}
	if held == nil || *held != pause.ID || release != 0 {
		t.Fatalf("the held ask's wake is marked with pause %v and release %d, want pause %d and no release", held, release, pause.ID)
	}
}
