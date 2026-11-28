package storetest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// teardownHoldEnv runs TestTheDropsTakeABoundOfTheirOwnNotTheTests: a Go duration, how long the
// server holds each login of the child's admin pool once the child's test has returned.
const teardownHoldEnv = "STORETEST_TEARDOWN_HOLD"

// teardownChildEnv names, in the child, the event trigger that holds its admin logins, which the
// parent drops once the child has exited.
const teardownChildEnv = "STORETEST_TEARDOWN_CHILD"

// teardownChildTimeout is the child's -timeout: its connect end (testdb.ConnectEnd) is 5 s into
// the run, so a drop whose connect took the tests' bound would have at most the rest of those 5 s,
// or the bound's one-second floor past the end, less than the hold.
const teardownChildTimeout = 35 * time.Second

// TestTheDropsTakeABoundOfTheirOwnNotTheTests is the reviewer's probe of 0f07b5cf36 on
// cybermonklabs/legion#5189: the child opens a template and a clone, has the server hold every
// login of its admin pool, resets the pool and returns, under -timeout 35s. The clone's drop,
// which its cleanup starts, and the template's, which Run makes after the tests, then log in while
// the hold lasts: the clone's drop well before the child's connect end (about half a second into
// the run, measured on 2026-11-28), the template's once the tests have ended. A drop whose connect
// takes the tests' bound gets at most what is left until that end, or the bound's one-second floor
// past it, less than the hold, and fails, and Run then exits 1 with the clone and the template left
// on the server; a drop that waits the hold out lets the child exit 0. The row fails where the child exits other
// than 0, where Run reports a drop it could not make, or where the child exits before any login of
// its admin pool was held, which would mean nothing held its drops' logins. That a login was held
// is a fact the server shows while the child runs, a login of the child's admin pool waiting in
// the hold's pg_sleep (pg_stat_activity), which this row waits for, not how long the child took.
//
// It holds logins and runs a child binary, so it runs only where STORETEST_TEARDOWN_HOLD names
// the hold (8s), longer than the child's 5 s to its connect end, against a server the run holds
// (an event trigger on login needs a superuser); the hold reaches only the child's own admin
// logins (their application_name, adminApplicationName).
func TestTheDropsTakeABoundOfTheirOwnNotTheTests(t *testing.T) {
	hold, err := time.ParseDuration(os.Getenv(teardownHoldEnv))
	if err != nil {
		t.Skip(teardownHoldEnv + " is not set: this row holds a child's logins for its whole teardown")
	}
	if os.Getenv(teardownChildEnv) != "" {
		t.Skip("this is the child's binary")
	}
	if toEnd := teardownChildTimeout - testwait.Margin; hold <= toEnd {
		t.Fatalf("%s=%s does not outlast the child's %s to its connect end, so its drops would not log in after that end", teardownHoldEnv, hold, toEnd)
	}
	ready(t)
	ctx := testwait.Context(t)
	trigger := fmt.Sprintf("storetest_teardown_hold_%d", os.Getpid())
	control, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, "postgres"))+"&application_name=storetest-teardown-hold")
	if err != nil {
		t.Fatalf("connect to drop the child's hold: %v", err)
	}
	testcleanup.Add(t, func() {
		_, _ = control.Exec(context.Background(), "drop event trigger if exists "+trigger)
		_, _ = control.Exec(context.Background(), "drop function if exists "+trigger+"()")
		_ = control.Close(context.Background())
	})
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTheDropsTakeABoundOfTheirOwnChild$", "-test.timeout="+teardownChildTimeout.String(), "-test.v")
	child.Env = append(os.Environ(), teardownChildEnv+"="+trigger)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	// The child's adminApplicationName: only a drop logs in under it once the child's hold is made.
	admin := adminApplicationNameOf(child.Process.Pid)
	held := testwait.Until(t, "a login of the child's admin pool held by its hold (waiting in pg_sleep, in pg_stat_activity)", func() (int, bool) {
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("the child exited %v: its teardown failed (a drop's connect under the tests' bound, which a held login outlasts):\n%s", err, &output)
			}
			t.Fatalf("the child exited 0 before any login of its admin pool was held: nothing held its drops' logins, so this run proves nothing:\n%s", &output)
		default:
		}
		var count int
		if err := control.QueryRow(ctx, "select count(*) from pg_stat_activity where application_name = $1 and wait_event = 'PgSleep'", admin).Scan(&count); err != nil {
			t.Fatalf("read the logins of the child's admin pool its hold holds: %v", err)
		}
		return count, count > 0
	})
	if err := testwait.Receive(t, "the child to exit", exited); err != nil {
		t.Fatalf("the child exited %v: its teardown failed (a drop's connect under the tests' bound, which a held login outlasts):\n%s", err, &output)
	}
	if strings.Contains(output.String(), "storetest: drop") {
		t.Fatalf("the child exited 0 yet Run reported a drop it could not make:\n%s", &output)
	}
	if !strings.Contains(output.String(), "--- PASS: TestTheDropsTakeABoundOfTheirOwnChild (") {
		t.Fatalf("the child's row did not pass, so it made no template, clone or hold to drop:\n%s", &output)
	}
	t.Logf("the child exited 0, its drops' logins held %s each (%d seen held at once)", hold, held)
}

// TestTheDropsTakeABoundOfTheirOwnChild is the child of TestTheDropsTakeABoundOfTheirOwnNotTheTests.
// It runs only when the parent re-executes this binary.
func TestTheDropsTakeABoundOfTheirOwnChild(t *testing.T) {
	trigger := os.Getenv(teardownChildEnv)
	if trigger == "" {
		t.Skip(teardownChildEnv + " is not set: this test is the child of TestTheDropsTakeABoundOfTheirOwnNotTheTests")
	}
	hold, err := time.ParseDuration(os.Getenv(teardownHoldEnv))
	if err != nil {
		t.Fatalf("%s: %v", teardownHoldEnv, err)
	}
	// A template and a clone of it: the clone's drop starts in this test's cleanup, and the
	// template's in Run, after the tests.
	OpenFrom(t, "teardown-hold", func(string) error { return nil })
	ctx := context.Background()
	control, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, "postgres"))+"&application_name=storetest-teardown-hold")
	if err != nil {
		t.Fatalf("connect for the hold: %v", err)
	}
	defer control.Close(ctx)
	name := strings.ReplaceAll(adminApplicationName(), "'", "")
	for _, statement := range []string{
		fmt.Sprintf("create or replace function %s() returns event_trigger language plpgsql as $f$ begin if current_setting('application_name') = '%s' then perform pg_sleep(%f); end if; end $f$", trigger, name, hold.Seconds()),
		"create event trigger " + trigger + " on login execute function " + trigger + "()",
	} {
		if _, err := control.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	shared.admin.Reset()
	t.Logf("every new login of this binary's admin pool held %s from %s", hold, time.Now().UTC().Format(time.RFC3339))
}
