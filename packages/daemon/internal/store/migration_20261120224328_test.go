package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimSourceHoldVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimSourceHoldVersion = 20261120224328

// claimSourceHoldRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const claimSourceHoldRelease = "the release that stops a tree's work under the work hold"

// 20261120224328 makes 'hold' a source the claims table takes: a database the release before left
// refuses a claim suspended by the work hold, and one this migration took stores it and reads it
// back. The way back, the last paragraph of the migration's header (migrations.WayBack), hands each
// claim the hold stopped to the operator — the one source of the release before that keeps the
// claim's session and pending task and that no rule resumes — leaves every other claim's source as
// it was, writes the check as the release before wrote it, which refuses 'hold' again, and deletes
// the migration's own version row, so the roll-forward runs it again.
func TestTheWorkHoldSourceMigrationTakesHoldAndItsWayBackHandsTheClaimToTheOperator(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimSourceHoldVersion-1)
	suspended := func(token claim.Token, role claim.Role, by supervise.SuspendSource) supervise.Claim {
		return supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: role,
			Generation: 2, State: supervise.StateSuspended, Session: "ses_" + string(role), SuspendedBy: by}
	}
	held := suspended("legion-LEGION-209-implementer", claim.RoleImplementer, supervise.SuspendedByHold)
	closed := suspended("legion-LEGION-209-tester", claim.RoleTester, supervise.SuspendedByDaemon)
	refusedByTheCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "claims_suspended_by_check"
	}
	if err := store.PutClaim(ctx, held); !refusedByTheCheck(err) {
		t.Fatalf("the database the release before left stored a claim suspended by the hold = %v, want its source check to refuse it (23514)", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the hold's source: %v", err)
	}
	for _, c := range []supervise.Claim{held, closed} {
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put the claim suspended by %q: %v", c.SuspendedBy, err)
		}
	}
	const sources = `select row(role, suspended_by)::text from claims order by role`
	if got, want := rowsAsText(t, store, sources), []string{"(implementer,hold)", "(tester,daemon)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the sources after the migration = %v, want %v", got, want)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, claimSourceHoldVersion, claimSourceHoldRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}

	if got, want := rowsAsText(t, store, sources), []string{"(implementer,operator)", "(tester,daemon)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the sources after the way back = %v, want %v: the held claim the operator's, the daemon's as it was", got, want)
	}
	if err := store.PutClaim(ctx, held); !refusedByTheCheck(err) {
		t.Fatalf("after the way back a claim suspended by the hold = %v, want the release before's source check to refuse it (23514)", err)
	}
	var recorded bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`,
		claimSourceHoldVersion).Scan(&recorded); err != nil || recorded {
		t.Fatalf("the way back left the version row recorded = %v (%v), want it deleted", recorded, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate forward again: %v", err)
	}
	if err := store.PutClaim(ctx, held); err != nil {
		t.Fatalf("after the roll-forward put the claim suspended by the hold: %v", err)
	}
}
