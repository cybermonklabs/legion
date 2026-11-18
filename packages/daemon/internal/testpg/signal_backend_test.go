package testpg_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// Every test of this module that makes a database of its own drops it with `with (force)`, which
// ends every backend still attached to that database. The clause is there for a connection a test
// left open, which is why the helpers write it: a plain `drop database` makes the server end the
// database's autovacuum worker itself and needs no right for it, while `with (force)` ends that
// worker and every other backend as the dropping login, and ending a backend of another role needs
// pg_signal_backend or the superuser (PostgreSQL 18.6's TerminateOtherDBBackends, in
// src/backend/storage/ipc/procarray.c). A server whose login holds neither fails those drops with
// `permission denied to terminate process` (SQLSTATE 42501), at a different handful of tests each
// run, and only where that server is: CI's login is a superuser, so CI stays green while every
// Legion issue pod goes red. This holds the right the drop needs on whatever server
// LEGION_TEST_PG_DSN names, which is the one the tests will use. It reads the right in a database
// of its own on that server (testdb.Database), whose URL keeps the DSN's login, so the login it
// reads is the one every test's drop runs as and no read reaches the DSN's own database.
func TestTheDSNRoleMayTerminateAnotherRolesBackend(t *testing.T) {
	conn, err := pgx.Connect(t.Context(), testdb.Database(t, "legion_signal_backend_test"))
	if err != nil {
		t.Fatalf("connect to a database of the test's own on the test server: %v", err)
	}
	defer conn.Close(t.Context())

	// pg_has_role's 'usage' is has_privs_of_role, which answers true for a superuser before it
	// looks at any membership, so this one read covers both ways the right can be held.
	var mayTerminate bool
	if err := conn.QueryRow(t.Context(),
		`select pg_has_role(current_user, 'pg_signal_backend', 'usage')`,
	).Scan(&mayTerminate); err != nil {
		t.Fatalf("read whether the login may terminate a backend: %v", err)
	}
	if !mayTerminate {
		var login string
		if err := conn.QueryRow(t.Context(), "select current_user").Scan(&login); err != nil {
			login = "the DSN's login"
		}
		t.Fatalf("%s is no superuser and does not hold pg_signal_backend, so `drop database … with (force)` fails for it with SQLSTATE 42501. "+
			"packages/daemon/docker/legion-testpg grants the right on every start, so a pod that fails here runs a worker image built before that grant: "+
			"the fix is a newer worker image, built from a commit that carries the grant; a person moves that pin, and no edit in this tree does", login)
	}
}
