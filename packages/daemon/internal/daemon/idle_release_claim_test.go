package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// twoClaimIdleRig is newIdleRig's quiet pod with a second live claim beside the implementer's: key
// a's agent works the tester's step in the claim it started as implementer, so the tester's
// phases row names that claim, and the fresh tester an earlier version of the plan started still
// holds its own claim in the pod, idle. It answers the second claim's machine.
func twoClaimIdleRig(t *testing.T) (*idleRig, *supervise.Machine) {
	t.Helper()
	ctx := context.Background()
	rig := newIdleRig(t)
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range []record.PhaseRow{
			{Issue: rig.issue.Key, Role: claim.RoleImplementer, Claim: rig.token, StepTask: "t", StepPlace: 1, Entry: 1},
			{Issue: rig.issue.Key, Role: claim.RoleTester, Claim: rig.token, StepTask: "t", StepPlace: 2, Entry: 1},
		} {
			if err := rig.issues.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write the phases rows of key a's two steps: %v", err)
	}
	fresh := mustClaimToken(t, rig.issue.Key, claim.RoleTester)
	locator, err := rig.rt.Spawn(ctx, runtime.SpawnSpec{Claim: fresh, Role: claim.RoleTester})
	if err != nil {
		t.Fatalf("mint a locator: %v", err)
	}
	stored := supervise.Claim{Token: fresh, Project: "legion", Tree: rig.issue.Tree, Issue: rig.issue.Key,
		Role: claim.RoleTester, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, Locator: &locator, Session: "ses-tester",
		SessionFile: "/tmp/tester.jsonl", PluginContract: api.DaemonAPIVersion()}
	if err := rig.st.PutClaim(ctx, stored); err != nil {
		t.Fatalf("seed the earlier version's tester: %v", err)
	}
	if _, err := rig.sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the earlier version's tester: %v", err)
	}
	machine, ok := rig.sup.Machine(fresh)
	if !ok {
		t.Fatalf("no machine for %s", fresh)
	}
	if _, err := rig.pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes',
		activity_at = now() - interval '25 minutes', activity_background = false,
		activity_kinds = '', activity_next_reminder_at = null where issue = $1`, rig.issue.Key); err != nil {
		t.Fatalf("backdate the pod: %v", err)
	}
	return rig, machine
}

// claimsOfRows is the claim each queued outbox row of kind and op names, oldest first, "" for a
// row that names none.
func claimsOfRows(t *testing.T, rig *idleRig, kind, op string) []claim.Token {
	t.Helper()
	found, err := rig.pool.Query(context.Background(), `select kind, coalesce(payload->>'op', ''),
		coalesce(payload->>'claim', '') from outbox where issue = $1 order by id`, rig.issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer found.Close()
	var claims []claim.Token
	for found.Next() {
		var rowKind, rowOp, token string
		if err := found.Scan(&rowKind, &rowOp, &token); err != nil {
			t.Fatal(err)
		}
		if rowKind == kind && rowOp == op {
			claims = append(claims, claim.Token(token))
		}
	}
	if err := found.Err(); err != nil {
		t.Fatal(err)
	}
	return claims
}

// releaseTwoClaims runs the idle rule over the rig's pod and requires the release it queues: the
// workspace check, a stop for each of the two live claims and the teardown. The publication of
// the role holders that the claims' own writes queue is no row of the release, and is left out. A
// pod the rule held is no case of either test below, so the hold it recorded is named.
func releaseTwoClaims(t *testing.T, rig *idleRig) {
	t.Helper()
	rig.w.releaseIdlePod(context.Background(), rig.issue.Key)
	want := []string{string(record.OutboxKindIssueBundle), string(record.OutboxKindSupervise) + ":release",
		string(record.OutboxKindSupervise) + ":release", string(record.OutboxKindIssueRelease)}
	got := slices.DeleteFunc(rig.rows(t), func(kind string) bool { return kind == string(record.OutboxKindRoleHoldersPublish) })
	if !slices.Equal(got, want) {
		t.Fatalf("the release queued %v, want %v; the holds recorded: %+v", got, want, rig.records(t).Holds)
	}
}

// A pod's idle release names each live claim it suspends: the workspace check asks the shim of the
// live claim on an open step, else the first live claim, and each stop names its own claim. Key
// a's agent on the tester's step and the fresh tester beside it are then each suspended as
// released, and the teardown, which waits while any claim of the issue may still hold a process,
// takes the pod. Rows that named the role alone reached the claim of the worker on the tester's
// step for both stops, so the fresh tester was never stopped and the teardown waited on it.
func TestTheReleaseRowsNameEachLiveClaimAndSuspendIt(t *testing.T) {
	t.Parallel()
	rig, fresh := twoClaimIdleRig(t)
	releaseTwoClaims(t, rig)
	if got, want := claimsOfRows(t, rig, string(record.OutboxKindIssueBundle), ""), []claim.Token{rig.token}; !slices.Equal(got, want) {
		t.Fatalf("the workspace check names %q, want %q: the shim of the live claim on the open step", got, want)
	}
	if got, want := claimsOfRows(t, rig, string(record.OutboxKindSupervise), "release"), []claim.Token{rig.token, fresh.Claim().Token}; !slices.Equal(got, want) {
		t.Fatalf("the release's stops name %q, want %q: one for each live claim", got, want)
	}

	rig.drain(t)

	for _, machine := range []*supervise.Machine{rig.machine, fresh} {
		if got := machine.Claim(); !got.Released() {
			t.Errorf("claim %s after the release = %s by %s, want it released", got.Token, got.State, got.SuspendedBy)
		}
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the one the release asked for once every claim stopped", suspended)
	}
}

// A cancelled release resumes each claim it suspended by its token: the resume of key a's agent
// and the resume of the fresh tester are two rows, each naming its claim, and both claims run
// again in the pod. Resumes that named the role alone reached the claim of the worker on the
// tester's step twice and left the fresh tester released in a pod that stands, where a later wake
// could take it.
func TestACancelledReleaseResumesEachClaimByItsToken(t *testing.T) {
	t.Parallel()
	rig, fresh := twoClaimIdleRig(t)
	ctx := context.Background()
	releaseTwoClaims(t, rig)
	// Under the pod lock, before the store is asked: a re-admission moved the issue's run on, so
	// the teardown is refused and the release is cancelled. The teardown runs after the release's
	// stops in the issue's lane, so each claim is read there as those stops left it.
	var atTeardown []bool
	rig.rt.beforeAuthorize = func() {
		atTeardown = []bool{rig.machine.Claim().Released(), fresh.Claim().Released()}
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if !slices.Equal(atTeardown, []bool{true, true}) {
		t.Fatalf("key a's claim and the fresh tester released when the teardown asked = %v, want both: a release suspends each live claim before the pod goes", atTeardown)
	}
	for _, machine := range []*supervise.Machine{rig.machine, fresh} {
		if got := machine.Claim(); got.State == supervise.StateSuspended {
			t.Errorf("claim %s after the cancel = %s by %s, want it resumed", got.Token, got.State, got.SuspendedBy)
		}
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
}
