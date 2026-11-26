package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// failWorkerClaim records the claim of role on key failed under cause as the store writes it: a
// claim held for OpenAI's cyber-policy refusal is stored as `provider_error` beside held_for
// `cyber_policy` (migration 20261125223008_claim_held_for), which record.RoleClaim reads back as
// the machine holds it (supervise.CauseCyberPolicy).
func failWorkerClaim(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role, heldFor string) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `update claims set state = 'failed', suspended_by = '', failure_cause = 'provider_error',
			failure = 'a provider error that cannot clear ended this claim''s turn', held_for = $2 where token = $1`, string(token), heldFor); err != nil {
			t.Fatalf("fail the %s of %s: %v", role, key, err)
		}
	})
}

// Admission's repair of a slot whose worker has no task (resumeSlottedWorkers) passes over a phase
// worker held for OpenAI's cyber-policy refusal: no start of the daemon's own launches its agent
// again (supervise.ErrHeldForCyberPolicy), so a start written here would be refused at the outbox
// on every pass. The control is the same fixture with the worker failed for another provider
// error that cannot clear, which the repair does start again: the fixture is one the repair
// reaches, and what the held run reads is the hold.
func TestTheRepairWritesNoStartForAWorkerHeldForCyberPolicy(t *testing.T) {
	const root, member = "LEGION-240", "LEGION-241"
	failed := func(t *testing.T, heldFor string) *pgxpool.Pool {
		t.Helper()
		pool := migratedPool(t)
		seedSlotRepairRow(t, pool, slotRepairRow{root: root, key: member, stands: phase.Testing, generation: 1,
			state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon, taskGeneration: 1, confirmed: true})
		failWorkerClaim(t, pool, member, claim.RoleTester, heldFor)
		return pool
	}
	// A todo of an issue nobody handed to Legion drives the pass and promotes nothing, applied
	// through the mirror as the daemon applies it, whose write of the issue's row is the record the
	// pass runs for (a fact that writes nothing runs none). So the repair is the one thing the pass
	// can write for the member.
	elsewhere := intake.DispatchIssue{Key: "LEGION-900", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated",
		Status: "todo", Title: "an issue of another team", Rank: "Z"}
	pass := func(pool *pgxpool.Pool) {
		admission := quiet(t, 1)
		applyMirrored(t, pool, admission, "dispatch-1", elsewhere, admission.engine)
	}

	held := failed(t, "cyber_policy")
	pass(held)
	if got := startsOf(t, held, member); len(got) != 0 {
		t.Fatalf("the tester of %s held for cyber policy has the start rows %+v, want none", member, got)
	}

	other := failed(t, "")
	pass(other)
	if got := startsOf(t, other, member); len(got) != 1 {
		t.Fatalf("the tester of %s failed for another provider error has the start rows %+v, want the repair's one: the fixture must be one the repair reaches", member, got)
	}
}

// The controller's restart of a stuck tree is its set_status of todo on the root, in Legion's work,
// whose architect claim failed (R20 of dispatch://INFRA-3320, in place of the release before's
// backlog and then todo on the root, which re-admitted it): it writes no status and opens no new
// run, and starts that claim again as `legion claims retry` does (workflow's
// retryFailedArchitect). That start is someone's decision (record.SuperviseRequest.Decided), so it
// is the start that launches again, in a new session, an architect held for OpenAI's cyber-policy
// refusal: the controller restarts the tree as for any stuck tree and needs to know nothing of the
// hold. The root is the Dispatch chain's, an issue the copy shows under no parent, and its
// architect the claim of its own key.
func TestTheRestartOfATreeStartsItsRootArchitectAsADecidedStart(t *testing.T) {
	const root = "LEGION-250"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedSlottedInPhase(t, pool, testProject, root, "A", phase.Implementing, nil)
	putWorkerClaim(t, pool, root, root, claim.RoleArchitect, 1, supervise.StateFailed, "")
	failWorkerClaim(t, pool, root, claim.RoleArchitect, "cyber_policy")
	if starts := roleStarts(t, pool, root, claim.RoleArchitect); len(starts) != 0 {
		t.Fatalf("the architect starts of %s before the controller's todo = %+v, want none: the todo's is the one this case reads", root, starts)
	}

	applyAsTheDaemon(t, pool, admission, "api", "set-status", intake.StatusCall{Op: intake.SetStatusCall,
		Issues: []intake.StatusCallIssue{{Key: root, Status: "todo"}}}, admission.engine)

	starts := roleStarts(t, pool, root, claim.RoleArchitect)
	if len(starts) != 1 || !starts[0].Decided || starts[0].Generation != 1 || starts[0].Phase != "" {
		t.Fatalf("the architect starts of %s after the controller's todo = %+v, want one of generation 1 and no phase, marked decided", root, starts)
	}
	if got := issue(t, pool, root); got.Generation != 1 || got.Phase != phase.Implementing {
		t.Fatalf("%s after the controller's todo stands in %s at generation %d, want implementing at 1: the todo restarts its architect and opens no run", root, got.Phase, got.Generation)
	}
}
