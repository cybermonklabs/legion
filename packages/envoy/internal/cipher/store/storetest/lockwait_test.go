package storetest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lockWaitChildURL is the environment variable that makes
// TestABuilderBehindAHeldTemplateLockGivesUpNamingIt the child it runs: the URL of the database the
// child builds its template from, on the parent's own server.
const lockWaitChildURL = "STORETEST_LOCK_WAIT_CHILD_URL"

// lockWaitChildGuard is the child's -timeout (testwait.RunChild): a hang guard only, which a child
// whose wait never gives up runs into, and never what decides the row. The child's whole run, its
// start, its connect and its give-up (RunChild's span), measured 0.11 to 0.25 s in five runs in an
// issue pod of 2 CPUs; two minutes is over four hundred times the slowest, and a setup a loaded
// machine holds up to 90 s, where the child's waits would give up, still reaches its step.
const lockWaitChildGuard = 2 * time.Minute

// A builder that finds no template of its login and the template lock held by another process that
// never gives it back waits for the lock through the wait every test shares, so it gives up as
// every other wait of a test does, in the shared wait's own words naming the template lock, and
// its test fails by name (INFRA-3149 spec v9, R2: "When a wait ends, its failure names the fact it
// waited for"), where a wait with no end would run into go test's alarm.
//
// The test runs on a Postgres server of its own (ownServer), holds the template lock there on the
// server's postgres database, and runs its own test binary again as a child (testwait.RunChild)
// that builds a template as a login of its own, which has no template on that server. The child
// connects once on the wall clock, its setup, and then takes the give-up as a step of its own
// (testwait.SetUpDone), so what it reports follows from what its wait did and not from how long a
// loaded machine took over the setup; its -timeout is a hang guard only. The row passes only on the
// shared wait's own words for this give-up ("testwait: storetest: gave up waiting for " and
// templateLockWait) and on the child's test failing by name with no panic. Control: a builder that
// takes no lock builds its template, writes no give-up and fails this row, and so does one whose
// wait has no end, which the guard ends with go test's alarm.
func TestABuilderBehindAHeldTemplateLockGivesUpNamingIt(t *testing.T) {
	t.Parallel()
	const test = "TestABuilderBehindAHeldTemplateLockGivesUpNamingIt"
	if childURL := os.Getenv(lockWaitChildURL); childURL != "" {
		conn, err := pgx.Connect(context.Background(), childURL)
		if err != nil {
			t.Fatalf("the child connects to its database on the parent's server: %v", err)
		}
		conn.Close(context.Background())
		testwait.SetUpDone(t)
		if _, err := makeTemplate(childURL); err != nil {
			t.Fatalf("broker test template database: %v", err)
		}
		t.Fatal("the child built a template while another process held the template lock")
	}
	ctx := context.Background()
	server := ownServer(t)
	login := newLogin(t, server, "broker_lock_wait_test")
	builder := testdb.DatabaseOn(t, login.String(), "", "broker_lock_wait_test")
	holder, err := pgx.Connect(ctx, testdb.DatabaseURL(server, lockDatabase))
	if err != nil {
		t.Fatalf("connect the holder to the test's own server: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	if _, err := holder.Exec(ctx, "select pg_advisory_lock(hashtext($1))", templateLockKey); err != nil {
		t.Fatalf("take the template lock: %v", err)
	}

	stdout, stderr, err := testwait.RunChild(t, os.Args[0], lockWaitChildGuard, "^"+test+"$", lockWaitChildURL+"="+builder)
	// The child's backend may still wait on the lock after the child is gone; end it, so the
	// login's cleanup drops what the login owns with nothing of it left on the server.
	if _, err := holder.Exec(ctx, "select pg_terminate_backend(pid) from pg_stat_activity where usename = $1", login.User.Username()); err != nil {
		t.Errorf("end the child's backends: %v", err)
	}
	text := stdout + stderr
	if err == nil {
		t.Fatalf("the child builder, behind a template lock that was never given back, passed its row; want it to fail, its wait giving up:\n%s", text)
	}
	if strings.Contains(text, "panic:") {
		t.Fatalf("the child builder panicked rather than failing its row by name:\n%s", text)
	}
	for _, want := range []string{"testwait: storetest: gave up waiting for " + templateLockWait + " ", "--- FAIL: " + test + " ("} {
		if !strings.Contains(text, want) {
			t.Errorf("the child builder, behind a template lock that was never given back, does not say %q; want its wait for the lock to give up through the shared wait. It wrote:\n%s", want, text)
		}
	}
}
