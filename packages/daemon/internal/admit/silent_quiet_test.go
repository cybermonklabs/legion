package admit

// The controller's tick list of silent workers and the stalled-tree measure read the one rule the
// silence sweep reads (workflow.SilentPhaseWorker), so a phase worker the record gives a reason of
// the daemon's own for its quiet is named by neither (dispatch://INFRA-2040 R1 and R3): a merger
// the closed design gate holds, and a worker whose wait stands.

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// waitEntry is the phases row entry every waiting worker below stands on and declared its wait at:
// above 0, so a wait judged against a row that was never read reads as a moved entry and stands
// not.
const waitEntry = 2

// workerToken is the claim token of key's role under testProject.
func workerToken(t *testing.T, key string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatalf("claim token of the %s of %s: %v", role, key, err)
	}
	return token
}

// declaresWait records key's role on its phases row at waitEntry, its claim given that entry's
// work already (so it is owed nothing and no repair of a role behind its entry writes a start for
// it, which would leave it unnamed for a reason of its own), and a wait of its claim, declared in
// session at the issue's generation and phase on that entry, as the declaration records one
// (record.Store.DeclareWait). session is the claim's own (putWorkerClaim's ses_worker) for a wait
// that stands, another for one a fresh session does not know. It answers the wait.
func declaresWait(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase, role claim.Role, session string) record.ClaimWait {
	t.Helper()
	token := workerToken(t, key, role)
	wait := record.ClaimWait{ID: "wait-" + key, Project: "legion", Token: token, Issue: key, Role: role, Generation: 1,
		Phase: stands, PhaseEntry: waitEntry, TakenEntry: waitEntry, Session: session, WaitingOn: "the run on the branch's head",
		WakeAt: fixedNow.Add(time.Hour), DeclaredAt: fixedNow.Add(-3 * time.Hour)}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: role, Claim: token, Entry: waitEntry}); err != nil {
			t.Fatalf("record the %s's phases row of %s: %v", role, key, err)
		}
		if _, err := tx.Exec(context.Background(), `update claims set given_entry = $2 where token = $1`, string(token), waitEntry); err != nil {
			t.Fatalf("give the %s of %s its entry's work: %v", role, key, err)
		}
		if _, err := records.DeclareWait(context.Background(), tx, wait); err != nil {
			t.Fatalf("record the wait of %s's %s: %v", key, role, err)
		}
	})
	return wait
}

// gatesSpec records the spec's gate row of root at version 2, approved at version approved (0 for
// none, a closed gate).
func gatesSpec(t *testing.T, pool *pgxpool.Pool, root string, approved int) {
	t.Helper()
	gate := record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: "spec-" + root, LatestVersion: 2}
	if approved > 0 {
		gate.ApprovedVersion, gate.EverOpened = &approved, true
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, gate); err != nil {
			t.Fatalf("record the spec gate of %s: %v", root, err)
		}
	})
}

// silentKeys is the issue of every entry of the one tick notice's silent list, in its order.
func silentKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	for _, entry := range tickNotice(t, pool).Silent {
		keys = append(keys, entry.Issue)
	}
	return keys
}

// clearOutbox drops every outbox row, so the next tick's notice is the one row tickNotice reads.
func clearOutbox(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `delete from outbox`); err != nil {
		t.Fatalf("clear the outbox: %v", err)
	}
}

// The tick leaves out a merger the closed design gate holds (R1), and names it once the gate
// opens. The deployment gates the spec and the decisions. Three roots stand in merging, each with
// its merger up between turns and quiet for two hours, past the bound: LEGION-780's spec row is
// unapproved, which holds its merger; LEGION-790's spec is approved and its decisions have no row,
// a gated step with no row, which holds no merger although the tree's gate reads closed; and
// LEGION-785's merger has reported background work standing past its own bound under the same
// unapproved spec. The tick names LEGION-790 alone; once LEGION-780's and LEGION-785's specs are
// approved, the next tick names all three.
func TestTheControllerTickLeavesOutAMergerTheDesignGateHolds(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmissionWithSteps(t, 3, []record.DesignStep{record.DesignStepSpec, record.DesignStepDecisions},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission.UseWorkerSilence(silenceBound, backgroundBound)
	registerController(t, pool)
	silentStep := fixedNow.Add(-6 * time.Hour)
	for i, key := range []string{"LEGION-780", "LEGION-785", "LEGION-790"} {
		seedPhaseWorker(t, pool, key, string(rune('A'+i)), phase.Merging, claim.RoleMerger, silentStep)
	}
	reportedBackground(t, pool, "LEGION-785", fixedNow.Add(-5*time.Hour))
	gatesSpec(t, pool, "LEGION-780", 0)
	gatesSpec(t, pool, "LEGION-785", 0)
	gatesSpec(t, pool, "LEGION-790", 2)

	tick(t, pool, admission, "tick-held")

	if got, want := silentKeys(t, pool), []string{"LEGION-790"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tick names %v as silent, want %v: a merger the unapproved spec holds is quiet by design, and a gated step with no row holds none", got, want)
	}

	clearOutbox(t, pool)
	gatesSpec(t, pool, "LEGION-780", 2)
	gatesSpec(t, pool, "LEGION-785", 2)
	tick(t, pool, admission, "tick-opened")

	if got, want := silentKeys(t, pool), []string{"LEGION-780", "LEGION-785", "LEGION-790"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tick names %v as silent once the specs were approved, want %v: an open gate holds no merger", got, want)
	}
}

// The tick leaves out a worker whose wait stands (R3), and names it once the wait ends. Three
// roots' workers are up between turns and quiet for six hours, past the bound: LEGION-800's
// implementer declared a wait in the session it runs, LEGION-805's tester declared one too and its
// background report has stood past its own bound, and LEGION-810's implementer declared one in a
// session it no longer runs, a wait a fresh session does not know, which stands not although no
// fact has ended it. The tick names LEGION-810 alone; once the two waits end, the next tick names
// all three.
func TestTheControllerTickLeavesOutAWorkerWhoseWaitStands(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 3)
	admission.UseWorkerSilence(silenceBound, backgroundBound)
	registerController(t, pool)
	silentStep := fixedNow.Add(-6 * time.Hour)
	seedPhaseWorker(t, pool, "LEGION-800", "A", phase.Implementing, claim.RoleImplementer, silentStep)
	seedPhaseWorker(t, pool, "LEGION-805", "B", phase.Testing, claim.RoleTester, silentStep)
	seedPhaseWorker(t, pool, "LEGION-810", "C", phase.Implementing, claim.RoleImplementer, silentStep)
	reportedBackground(t, pool, "LEGION-805", fixedNow.Add(-5*time.Hour))
	waits := []record.ClaimWait{
		declaresWait(t, pool, "LEGION-800", phase.Implementing, claim.RoleImplementer, "ses_worker"),
		declaresWait(t, pool, "LEGION-805", phase.Testing, claim.RoleTester, "ses_worker"),
	}
	declaresWait(t, pool, "LEGION-810", phase.Implementing, claim.RoleImplementer, "ses_before")

	tick(t, pool, admission, "tick-waiting")

	if got, want := silentKeys(t, pool), []string{"LEGION-810"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tick names %v as silent, want %v: a worker whose wait stands is quiet by design, and a wait of another session stands not", got, want)
	}

	clearOutbox(t, pool)
	inTx(t, pool, func(tx pgx.Tx) {
		for _, wait := range waits {
			if ended, err := record.NewStore().EndWait(context.Background(), tx, wait.Project, wait.Token, wait.ID,
				workflow.WaitEndTimeCame, "test", fixedNow); err != nil || !ended {
				t.Fatalf("end wait %s = %t, %v; want it ended", wait.ID, ended, err)
			}
		}
	})
	tick(t, pool, admission, "tick-ended")

	if got, want := silentKeys(t, pool), []string{"LEGION-800", "LEGION-805", "LEGION-810"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tick names %v as silent once the waits ended, want %v", got, want)
	}
}

// The stalled-tree measure reads a claim at work as R6 does, save a phase worker the daemon names
// silent (workingTrees), and a worker the record gives a reason of its own for its quiet is none:
// its claim in ready is work under way. The deployment's silence bound is longer than its quiet
// bound, as the measure's own control runs (TestAWorkingTreeIsNeverReportedStalled), and every
// tree is quiet for seven hours, its one claim a worker in ready whose last step is nine hours old,
// past the bound. LEGION-630's implementer is silent, and its tree is named; LEGION-650's
// implementer has a wait that stands, and LEGION-660's merger in merging is held by its root's
// unapproved spec, and neither tree is named.
func TestAWaitOrAHoldKeepsAQuietWorkersTreeOutOfTheStallReport(t *testing.T) {
	pool := migratedPool(t)
	cfg := deployment(t, "worker_silence_minutes: 480\nworker_background_hours: 12\n")
	admission := quiet(t, 3)
	admission.UseStalledTreeNotice(cfg.StalledTreeNotice)
	admission.UseWorkerSilence(cfg.WorkerSilence, cfg.WorkerBackground)
	registerController(t, pool)

	const silentTree, waitingTree, heldTree = "LEGION-630", "LEGION-650", "LEGION-660"
	tokens := map[string]claim.Token{}
	for key, stands := range map[string]phase.Phase{silentTree: phase.Implementing, waitingTree: phase.Implementing, heldTree: phase.Merging} {
		root := seedQuietRoot(t, pool, key, key, stands)
		tokens[key] = putWorkerClaim(t, pool, key, key, workflow.OpenPhaseRole(stands), root.Generation, supervise.StateReady, "")
	}
	declaresWait(t, pool, waitingTree, phase.Implementing, claim.RoleImplementer, "ses_worker")
	gatesSpec(t, pool, heldTree, 0)

	settled := settleMovements(t, pool, silentTree, waitingTree, heldTree)
	for _, token := range tokens {
		steppedAt(t, pool, token, settled.Add(-2*time.Hour))
	}
	admission.now = func() time.Time { return settled.Add(7 * time.Hour) }

	tick(t, pool, admission, "stalled")

	if got, want := stalledRoots(t, pool), []string{silentTree}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v: a worker whose wait stands and a merger the design gate holds are work under way, as the silence rule reads them",
			got, want)
	}
}
