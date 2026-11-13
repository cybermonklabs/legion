package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// silenceBound is the bound these ticks read a worker's silence against, as a deployment's
// `worker_silence_minutes` sets it (config.Config.WorkerSilence, registered with
// UseWorkerSilence). It is the shipped default, and both steps below are placed against it: the
// silent worker's is hours past it and the healthy one's well inside it.
const silenceBound = 30 * time.Minute

// backgroundBound is the longer bound these ticks read a worker's own report of background work
// against, as a deployment's `worker_background_hours` sets it (config.Config.WorkerBackground,
// registered with UseWorkerSilence beside silenceBound). It is the shipped default.
const backgroundBound = 4 * time.Hour

// reportedBackground stamps the claim of key's worker with its own session's report of
// background work running, made at at, in the columns the record reads (record.RoleClaim
// ActivityAt and ActivityBackground). It is a write of its own for the reason steppedAt is: the
// daemon writes these columns by a call of their own (store's PutClaimActivity).
func reportedBackground(t *testing.T, pool *pgxpool.Pool, key string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set activity_at = $2, activity_background = true,
		activity_kinds = 'async-job' where issue = $1`, key, at); err != nil {
		t.Fatalf("record the background report of %s's worker: %v", key, err)
	}
}

// steppedAt stamps token's claim with the moment its agent last stepped, in the two columns the
// record reads (record.RoleClaim.Step): the step itself, and the start of the turn it was taken
// in, ten minutes before it. The turn start is the earlier of the two on purpose, so the moment
// the list names is the step's own (supervise.StepRecord.LastStep) and a read of the wrong
// column would show.
//
// It is a write of its own because the claim fixtures of this package write the claim row alone
// and these columns are written by a call of their own in the daemon too (store's PutClaimStep,
// which no write of a whole claim carries).
func steppedAt(t *testing.T, pool *pgxpool.Pool, token claim.Token, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set step_at = $2, turn_started_at = $3 where token = $1`,
		string(token), at, at.Add(-10*time.Minute)); err != nil {
		t.Fatalf("record the last step of the claim %s: %v", token, err)
	}
}

// seedPhaseWorker records a running root in the phase its own worker works, holding one of the
// cap's slots, with that worker's claim up and between turns (supervise.StateReady) holding no
// task, and the moment its agent last stepped. That is every term of the rule but the one each
// case varies, which is how old the step is (workflow.SilentPhaseWorker).
func seedPhaseWorker(t *testing.T, pool *pgxpool.Pool, key, rank string, stands phase.Phase, role claim.Role, lastStep time.Time) {
	t.Helper()
	seedSlotted(t, pool, key, rank)
	working := issue(t, pool, key)
	working.Phase = stands
	putIssue(t, pool, working)
	token := putWorkerClaim(t, pool, key, key, role, working.Generation, supervise.StateReady, "")
	steppedAt(t, pool, token, lastStep)
}

// tickNotice is the tick wake this pass queued for the controller, and fails the test where the
// pass queued anything else: these cases are about what one tick carries, so a second controller
// notice in the outbox would make the assertions below read the wrong row.
func tickNotice(t *testing.T, pool *pgxpool.Pool) record.Notice {
	t.Helper()
	var queued []record.ControllerNotice
	for _, row := range effects(t, pool) {
		if row.kind != record.OutboxKindControllerNotice {
			continue
		}
		notice, ok := row.payload.(record.ControllerNotice)
		if !ok {
			t.Fatalf("a controller notice row carries %#v, want a controller notice", row.payload)
		}
		queued = append(queued, notice)
	}
	if len(queued) != 1 || queued[0].Kind != record.TickNotice {
		t.Fatalf("the controller notices = %#v, want the one tick this pass queued", queued)
	}
	return record.Notice(queued[0])
}

// The tick is the turn that posts the day's report, so it names every issue whose phase worker
// is one nothing will prompt: the issue, the role whose claim is silent, the moment that claim
// last stepped and how long it has been quiet (record.SilentIssue). The issue's own architect
// was told the same fact by a `worker-silent` notice, and this is the second reader, because
// that architect may be failed or gone, so its notice reaches nobody.
//
// A worker that stepped inside the bound is in no sense silent, and the tick says nothing about
// it: the two fixtures differ in the age of that one step and in nothing else. A tick over a
// record with nothing silent carries no list at all, as a tick with nothing blocked carries
// none.
func TestControllerTickNamesTheSilentIssues(t *testing.T) {
	silentStep := fixedNow.Add(-2 * time.Hour)
	healthyStep := fixedNow.Add(-5 * time.Minute)
	t.Run("a silent worker beside a healthy one", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		admission.UseWorkerSilence(silenceBound, backgroundBound)
		registerController(t, pool)
		seedPhaseWorker(t, pool, "LEGION-700", "A", phase.Implementing, claim.RoleImplementer, silentStep)
		seedPhaseWorker(t, pool, "LEGION-710", "B", phase.Testing, claim.RoleTester, healthyStep)

		tick(t, pool, admission, "tick-1")

		silent := tickNotice(t, pool).Silent
		if len(silent) != 1 {
			t.Fatalf("the tick names %#v as silent, want the one worker whose last step passed the bound: the other stepped %s ago",
				silent, fixedNow.Sub(healthyStep))
		}
		named := silent[0]
		if named.Issue != "LEGION-700" || named.Role != claim.RoleImplementer {
			t.Fatalf("the tick names %s's %s as silent, want LEGION-700's implementer: the role is the one that works the issue's phase",
				named.Issue, named.Role)
		}
		if !named.LastStep.Equal(silentStep) {
			t.Errorf("the tick says LEGION-700's implementer last stepped at %s, want the %s its claim records", named.LastStep, silentStep)
		}
		if want := (2 * time.Hour).String(); named.Silent != want {
			t.Errorf("the tick says the silence is %q, want %q: the length a reader of the day's report needs at a glance", named.Silent, want)
		}
	})
	// A worker whose own session reports background work running is one the daemon never
	// prompts, and once that report has stood past the background bound the tick names it, with
	// the moment of the report, as the `worker-background-stalled` notice names it to its
	// architect (workflow.BackgroundStalledWorker). Both workers here last stepped hours ago, so
	// both are past the silence bound, and both reports explain that quiet to the silence rule;
	// they differ in the age of the report alone, and the one inside its bound is not named.
	t.Run("a background report past its bound beside one inside it", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		admission.UseWorkerSilence(silenceBound, backgroundBound)
		registerController(t, pool)
		oldStep := fixedNow.Add(-6 * time.Hour)
		stalledReport := fixedNow.Add(-5 * time.Hour)
		runningReport := fixedNow.Add(-1 * time.Hour)
		seedPhaseWorker(t, pool, "LEGION-760", "A", phase.Implementing, claim.RoleImplementer, oldStep)
		reportedBackground(t, pool, "LEGION-760", stalledReport)
		seedPhaseWorker(t, pool, "LEGION-770", "B", phase.Testing, claim.RoleTester, oldStep)
		reportedBackground(t, pool, "LEGION-770", runningReport)

		tick(t, pool, admission, "tick-1")

		silent := tickNotice(t, pool).Silent
		if len(silent) != 1 || silent[0].Issue != "LEGION-760" || silent[0].Role != claim.RoleImplementer {
			t.Fatalf("the tick names %#v as silent, want LEGION-760's implementer alone: its background report has stood %s, past the %s bound, and LEGION-770's %s, inside it",
				silent, fixedNow.Sub(stalledReport), backgroundBound, fixedNow.Sub(runningReport))
		}
		named := silent[0]
		if !named.BackgroundSince.Equal(stalledReport) {
			t.Errorf("the tick says LEGION-760's background report was made at %s, want the %s its claim records", named.BackgroundSince, stalledReport)
		}
		if !named.LastStep.Equal(oldStep) {
			t.Errorf("the tick says LEGION-760's implementer last stepped at %s, want the %s its claim records", named.LastStep, oldStep)
		}
		if want := (6 * time.Hour).String(); named.Silent != want {
			t.Errorf("the tick says the silence is %q, want %q: the time since the last step, as for any entry", named.Silent, want)
		}
	})
	t.Run("nothing silent", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseWorkerSilence(silenceBound, backgroundBound)
		registerController(t, pool)
		seedPhaseWorker(t, pool, "LEGION-710", "B", phase.Testing, claim.RoleTester, healthyStep)

		tick(t, pool, admission, "tick-1")

		if silent := tickNotice(t, pool).Silent; len(silent) != 0 {
			t.Fatalf("the tick names %#v as silent, want no list at all: every worker of this record stepped inside the bound", silent)
		}
	})
	// A merged root stands in merging until the rest of its tree leaves the workflow, and its
	// merger, its phase over by the merge (workflow.PhaseFinished), is owed no turn however long
	// it is quiet. Both roots here stand in merging with a merger silent as long; they differ in
	// the state of their pull request alone, and the tick names the one whose merge is not done.
	t.Run("a merged root's merger beside an unmerged one", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		admission.UseWorkerSilence(silenceBound, backgroundBound)
		registerController(t, pool)
		seedPhaseWorker(t, pool, "LEGION-720", "A", phase.Merging, claim.RoleMerger, silentStep)
		seedPhaseWorker(t, pool, "LEGION-730", "B", phase.Merging, claim.RoleMerger, silentStep)
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			for _, pr := range []record.PullRequest{
				{State: record.PullRequestMerged, Issue: "LEGION-720", Number: 720},
				{State: record.PullRequestOpen, Issue: "LEGION-730", Number: 730},
			} {
				pr.Repo, pr.Branch, pr.HeadSHA, pr.Failing = "acme/widgets", "legion/"+pr.Issue, "head-"+pr.Issue, []string{}
				if err := records.PutPullRequest(context.Background(), tx, pr); err != nil {
					t.Fatalf("record the pull request of %s: %v", pr.Issue, err)
				}
			}
		})

		tick(t, pool, admission, "tick-1")

		silent := tickNotice(t, pool).Silent
		if len(silent) != 1 || silent[0].Issue != "LEGION-730" || silent[0].Role != claim.RoleMerger {
			t.Fatalf("the tick names %#v as silent, want LEGION-730's merger alone: LEGION-720's pull request is recorded merged, which ends its merger's phase",
				silent)
		}
	})
	// A role whose completion of this phase entry the workflow accepted is owed no turn
	// (workflow.PhaseWorkerOwedTurn), and the silence handler leaves it alone, so the tick does
	// too. Both roots stand in implementing with an implementer silent as long; they differ in
	// the completion moment of their implementer's phase row alone.
	t.Run("a role whose completion was accepted beside one still working", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		admission.UseWorkerSilence(silenceBound, backgroundBound)
		registerController(t, pool)
		seedPhaseWorker(t, pool, "LEGION-740", "A", phase.Implementing, claim.RoleImplementer, silentStep)
		seedPhaseWorker(t, pool, "LEGION-750", "B", phase.Implementing, claim.RoleImplementer, silentStep)
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			for _, row := range []record.PhaseRow{
				{Issue: "LEGION-740", Role: claim.RoleImplementer, Claim: "implementer-740", CompletedAt: silentStep},
				{Issue: "LEGION-750", Role: claim.RoleImplementer, Claim: "implementer-750"},
			} {
				if err := records.PutPhase(context.Background(), tx, row); err != nil {
					t.Fatalf("record the implementer's row of %s: %v", row.Issue, err)
				}
			}
		})

		tick(t, pool, admission, "tick-1")

		silent := tickNotice(t, pool).Silent
		if len(silent) != 1 || silent[0].Issue != "LEGION-750" || silent[0].Role != claim.RoleImplementer {
			t.Fatalf("the tick names %#v as silent, want LEGION-750's implementer alone: LEGION-740's implementer's completion of this entry was accepted",
				silent)
		}
	})
	t.Run("no bound registered", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		registerController(t, pool)
		seedPhaseWorker(t, pool, "LEGION-700", "A", phase.Implementing, claim.RoleImplementer, silentStep)

		tick(t, pool, admission, "tick-1")

		if silent := tickNotice(t, pool).Silent; len(silent) != 0 {
			t.Fatalf("the tick names %#v as silent under no bound, want none: a bound of zero switches the rule off, here and everywhere else it is read", silent)
		}
	})
}
