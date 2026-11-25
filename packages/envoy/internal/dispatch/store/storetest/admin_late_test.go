package storetest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// adminHoldEnv runs TestALaterAdminConnectionEndsByTheConnectEnd: a Go duration, how long after
// the admin pool opens the row asks it for a new connection.
const adminHoldEnv = "STORETEST_ADMIN_HOLD"

// TestALaterAdminConnectionEndsByTheConnectEnd is Tester5032's row (forcing FA on
// cybermonklabs/legion#5189): the admin pool, opened by the first test that asks, makes a
// connection long after, while the server holds that connection's login. The connect must end on
// pgx's own error by the binary's deadline less testwait's margin (testdb.ConnectEnd), never
// outlast it into go test's alarm, and the row fails by name where it has not ended by half the
// margin past that end. A pool whose new connections keep the first test's connect timeout, or
// one computed when the pool opened, fails it.
//
// It holds a login for 900 s and runs for most of the binary's -timeout, so it runs only where
// STORETEST_ADMIN_HOLD names the wait before the late connection (60s under -timeout 4m), against
// a server the run holds; the hold reaches only this binary's own admin logins (their
// application_name, adminApplicationName) and is dropped when the row ends.
func TestALaterAdminConnectionEndsByTheConnectEnd(t *testing.T) {
	wait, err := time.ParseDuration(os.Getenv(adminHoldEnv))
	if err != nil {
		t.Skip(adminHoldEnv + " is not set: this row holds a login for most of the binary's -timeout")
	}
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Skip("the row needs a -timeout: without one no connect has an end")
	}
	ctx := context.Background()
	if _, err := shared.admin.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("the first admin statement: %v", err)
	}
	// The hold, made and dropped on a connection of the row's own, whose application_name the
	// trigger passes over.
	control, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, "postgres"))+"&application_name=storetest-admin-hold")
	if err != nil {
		t.Fatalf("connect for the hold: %v", err)
	}
	testcleanup.Add(t, func() {
		_, _ = control.Exec(context.Background(), "drop event trigger if exists storetest_admin_hold")
		_, _ = control.Exec(context.Background(), "drop function if exists storetest_admin_hold()")
		_ = control.Close(context.Background())
	})
	name := strings.ReplaceAll(adminApplicationName(), "'", "")
	for _, statement := range []string{
		"create or replace function storetest_admin_hold() returns event_trigger language plpgsql as $f$ begin if current_setting('application_name') = '" + name + "' then perform pg_sleep(900); end if; end $f$",
		"create event trigger storetest_admin_hold on login execute function storetest_admin_hold()",
	} {
		if _, err := control.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Logf("every login of this binary's admin pool held 900 s from %s", time.Now().UTC().Format(time.RFC3339))
	time.Sleep(wait)
	shared.admin.Reset()
	began := time.Now()
	answered := make(chan error, 1)
	go func() {
		_, err := shared.admin.Exec(ctx, "select 1")
		answered <- err
	}()
	giveUp := time.NewTimer(time.Until(end.Add(testwait.Margin / 2)))
	defer giveUp.Stop()
	select {
	case err := <-answered:
		left := time.Until(end)
		t.Logf("the late admin connection answered %v after %s, %s before the connect end", err, time.Since(began).Round(time.Second), left.Round(time.Second))
		if err == nil {
			t.Fatal("a connect whose login was held 900 s answered no error")
		}
		if !pgconn.Timeout(err) {
			t.Fatalf("the late admin connection ended on %v; want pgx's connect timeout", err)
		}
		// The bound is whole seconds, rounded down (testdb.TimeoutUntil), so the timeout comes up
		// to a second before the end; a timeout well before it is a bound that ended for another
		// reason, as a shorter connect_timeout would.
		if left > 5*time.Second {
			t.Fatalf("the late admin connection timed out %s before the connect end; want it to time out at that end", left.Round(time.Second))
		}
	case <-giveUp.C:
		t.Fatalf("the late admin connection had not ended %s after it began, %s past the connect end (the binary's deadline less %s): its connect timeout outlasts that end", time.Since(began).Round(time.Second), time.Since(end).Round(time.Second), testwait.Margin)
	}
}
