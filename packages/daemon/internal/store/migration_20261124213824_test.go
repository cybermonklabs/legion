package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roleChangeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const roleChangeVersion = 20261124213824

// The record that a claim took another role is kept on the claim's own row
// (supervise.Claim.RoleChange), because the first prompt in the new role, which tells the agent
// that the tokens of its earlier role are void, is a relaunch and can be a daemon restart away. The
// round trip is the column through putClaim and scanClaim; the migration gives every row the table
// already holds null; and the way back its header carries drops the column and its version row, so
// the roll-forward adds the column again.
func TestARoleChangeIsRecordedOnTheClaimRow(t *testing.T) {
	t.Run("the record round-trips with the claim, and its absence is null", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-209-implementer")
		want.Role = claim.RoleTester
		want.RoleChange = supervise.RoleChange{From: claim.RoleImplementer}
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with its record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// A record a turn took the prompt of says so, and names that prompt's delivery.
		want.RoleChange.Told, want.RoleChange.ToldBy = true, "dlv-7"
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with its told record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// A claim that records none writes null, not an empty record.
		want.RoleChange = supervise.RoleChange{}
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with no record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
		var null bool
		if err := store.Pool().QueryRow(ctx, "select role_change is null from claims where token = $1",
			string(want.Token)).Scan(&null); err != nil {
			t.Fatalf("read the column: %v", err)
		}
		if !null {
			t.Error("a claim that never took another role wrote a record, want null")
		}
	})

	t.Run("the migration gives every stored claim no record", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, roleChangeVersion-1)

		const token = "legion-LEGION-211-implementer"
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', 'idle', '', 0, 0, 0, 0)`, token); err != nil {
			t.Fatalf("record %s before the migration: %v", token, err)
		}
		_, err := store.Pool().Exec(ctx, `update claims set role_change = '{}'::jsonb`)
		if err == nil || !strings.Contains(err.Error(), "role_change") {
			t.Fatalf("writing role_change before the migration = %v, want the column to be unknown", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the role change record: %v", err)
		}
		var null bool
		if err := store.Pool().QueryRow(ctx, "select role_change is null from claims where token = $1", token).Scan(&null); err != nil {
			t.Fatalf("read the column after the migration: %v", err)
		}
		if !null {
			t.Error("the stored claim holds a record after the migration, want null")
		}
		if read := onlyClaim(t, store); read.RoleChange != (supervise.RoleChange{}) {
			t.Errorf("the stored claim reads the record %+v after the migration, want none", read.RoleChange)
		}
	})

	t.Run("the way back drops the column and its version, and the roll-forward adds it again", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		c := tmuxClaim("legion-LEGION-212-implementer")
		c.Role, c.RoleChange = claim.RoleTester, supervise.RoleChange{From: claim.RoleImplementer}
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put the claim with its record: %v", err)
		}

		if _, err := store.Pool().Exec(ctx, wayBackOf(t, roleChangeVersion, "the claim's role change record")); err != nil {
			t.Fatalf("run the way back: %v", err)
		}
		var column, version bool
		if err := store.Pool().QueryRow(ctx, `select
			exists (select 1 from information_schema.columns
				where table_schema = current_schema() and table_name = 'claims' and column_name = 'role_change'),
			exists (select 1 from schema_version where version = $1)`, roleChangeVersion).Scan(&column, &version); err != nil {
			t.Fatalf("read the schema after the way back: %v", err)
		}
		if column || version {
			t.Fatalf("after the way back the column stands = %t and the version row stands = %t, want neither", column, version)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("roll forward: %v", err)
		}
		read := onlyClaim(t, store)
		if read.RoleChange != (supervise.RoleChange{}) || read.Role != claim.RoleTester {
			t.Errorf("after the roll-forward the claim reads role %s and record %+v, want the tester's role and no record",
				read.Role, read.RoleChange)
		}
	})
}
