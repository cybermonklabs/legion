package storetest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lockWaitChildURL is the environment variable that makes
// TestABuilderBehindAHeldTemplateLockGivesUpNamingIt the child it starts: the URL of the database
// the child builds its template from, on the parent's own server.
const lockWaitChildURL = "STORETEST_LOCK_WAIT_CHILD_URL"

// lockWaitChildTimeout is the child's -timeout: the wait every test shares gives up its margin
// (testwait.Margin, 30 s) before the binary's deadline, so a builder that waits through it gives up
// about a second after it begins to wait, and one that waits on no bound of its own runs into go
// test's alarm at 31 s.
const lockWaitChildTimeout = "31s"

// A builder that finds no template of its login and the template lock held by another process that
// never gives it back waits for the lock through the wait every test shares, so its failure names
// the template lock before the test binary's deadline, as every other wait of a test does
// (INFRA-3149 spec v9, R2: "When a wait ends, its failure names the fact it waited for"). A
// builder that waits on the lock with no bound of its own runs into go test's alarm instead
// ("panic: test timed out"), whose text names no lock, and fails this test.
//
// The test runs on a Postgres server of its own (ownServer), holds the template lock there on the
// server's postgres database, and starts its own binary again as a child that builds a template
// as a login of its own, which has no template on that server, with a -timeout of
// lockWaitChildTimeout. It reads what the child wrote once the child has exited.
func TestABuilderBehindAHeldTemplateLockGivesUpNamingIt(t *testing.T) {
	t.Parallel()
	if childURL := os.Getenv(lockWaitChildURL); childURL != "" {
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

	child := exec.Command(os.Args[0], "-test.run=^TestABuilderBehindAHeldTemplateLockGivesUpNamingIt$", "-test.timeout="+lockWaitChildTimeout, "-test.v")
	child.Env = append(os.Environ(), lockWaitChildURL+"="+builder)
	var out []byte
	testwait.Returns(t, "the child builder, behind the held template lock, to exit", func() {
		out, _ = child.CombinedOutput()
	})
	// The child's backend may still wait on the lock after the child is gone; end it, so the
	// login's cleanup drops what the login owns with nothing of it left on the server.
	if _, err := holder.Exec(ctx, "select pg_terminate_backend(pid) from pg_stat_activity where usename = $1", login.User.Username()); err != nil {
		t.Errorf("end the child's backends: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "panic: test timed out") || !strings.Contains(text, "template lock") {
		t.Errorf("the child builder, behind a template lock that was never given back, ended on go test's alarm or with a failure that does not name the template lock; want its wait to give up through the shared wait, naming the template lock, before the binary's deadline. It wrote:\n%s", text)
	}
}
