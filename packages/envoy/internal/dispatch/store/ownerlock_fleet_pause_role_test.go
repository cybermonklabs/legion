package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// TestTheFleetReadPauseRoleLeavesNoRoleWhenItsGrantsCannotBeDroppedFirst holds the claim of
// pauseFleetReadsAt that a role spans the cluster and its cleanup drops it whatever failed first.
// The test body leaves the role owning a table that a view of the test's login depends on, so the
// cleanup's `drop owned by` of the role fails (2BP01) and a `drop role` run while the test database
// still exists finds the role's grants and its table there and fails too. Once the test database
// is gone nothing depends on the role, so a drop made then takes it. The role must not outlive
// the test: the check is registered before the store is opened, so it runs after the test
// database's own drop, and it drops any role it finds, so this test leaves nothing either way. A
// role spans the server, so the check looks for the role and drops it in a database the test made
// for it (testdb.DatabaseOn), never in the server's postgres database: that database is made
// before the check is registered, so its drop runs after the check, and it is there when the
// check connects.
func TestTheFleetReadPauseRoleLeavesNoRoleWhenItsGrantsCannotBeDroppedFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := adminDatabaseURL(t)
	checkDatabase := testdb.DatabaseOn(t, admin.String(), "", "dispatch_pause_role_check")
	var role string
	t.Cleanup(func() {
		if role == "" {
			return
		}
		conn, err := pgx.Connect(context.Background(), checkDatabase)
		if err != nil {
			t.Errorf("connect to the test server to look for the role %s: %v", role, err)
			return
		}
		defer conn.Close(context.Background())
		var left bool
		if err := conn.QueryRow(context.Background(),
			`select exists (select from pg_roles where rolname = $1)`, role).Scan(&left); err != nil {
			t.Errorf("look for the role %s: %v", role, err)
			return
		}
		if left {
			t.Errorf("the fleet read's pause role %s outlived its test once its first cleanup statement failed; "+
				"drop the role after the test database is gone, when nothing depends on it", role)
			if _, err := conn.Exec(context.Background(), "drop role "+role); err != nil {
				t.Errorf("drop the role %s this test left: %v", role, err)
			}
		}
	})

	s, _ := openFleetStore(t)
	paused := registerFleetProcess(t, s, OwnerLockProtocol, 0)
	role, _ = pauseFleetReadsAt(t, s, paused)
	// The role owns a table, and a view of the login depends on it: `drop owned by` the role,
	// which drops that table, is refused while the view stands.
	if _, err := s.Pool.Exec(ctx, fmt.Sprintf(`
		grant create on schema public to %[1]s;
		set role %[1]s;
		create table public.pause_role_owned (id int);
		reset role;
		create view public.pause_role_dependent as select id from public.pause_role_owned;`, role)); err != nil {
		t.Fatalf("leave the role a table a view depends on: %v", err)
	}
}
