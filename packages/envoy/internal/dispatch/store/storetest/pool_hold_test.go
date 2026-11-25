package storetest

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// poolHoldEnv runs TestATestsAdminStatementEndsByTheConnectEndWhileDropsHoldThePool when set.
const poolHoldEnv = "STORETEST_POOL_HOLD"

// stopsOnFatal is t with its Fatal and Fatalf recorded rather than failing t, for a call the row
// runs in a goroutine of its own and whose failure it reads: each sends its message and ends that
// goroutine. Every other method is t's, Deadline among them, so testdb.ConnectEnd reads t's.
type stopsOnFatal struct {
	*testing.T
	failed chan<- string
}

func (s stopsOnFatal) Fatalf(format string, args ...any) {
	s.failed <- fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (s stopsOnFatal) Fatal(args ...any) {
	s.failed <- fmt.Sprint(args...)
	runtime.Goexit()
}

// TestATestsAdminStatementEndsByTheConnectEndWhileDropsHoldThePool is Tester5486's probe of
// cddf290615 on cybermonklabs/legion#5189, made a row. The server holds every login of this
// binary's admin pool until 20 s past the connect end, the pool is emptied, and as many drops as
// the pool has connections fill it, each waiting on its held login with testdb's two minutes. A
// test's admin statement, Empty's create database, must then end on its own error at the connect
// end, not wait for a drop's connection past it. A statement on context.Background() has no bound
// on that wait: it waits until a login is let go, and the row fails by name half the margin past
// the end. The drops wait the hold out, so none of their databases is left.
//
// It holds logins for most of the binary's run, so it runs only where STORETEST_POOL_HOLD is set,
// under a -timeout short enough that the hold is under two minutes (-timeout 90s), against a
// server the run holds (an event trigger on login needs a superuser); the hold reaches only this
// binary's admin logins (their application_name, adminApplicationName).
func TestATestsAdminStatementEndsByTheConnectEndWhileDropsHoldThePool(t *testing.T) {
	if os.Getenv(poolHoldEnv) == "" {
		t.Skip(poolHoldEnv + " is not set: this row holds the admin pool's logins for most of the binary's run")
	}
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Skip("the row needs a -timeout: without one no connect has an end")
	}
	ctx := context.Background()
	slots := int(shared.admin.Config().MaxConns)
	names := make([]string, slots)
	dropped := false
	for i := range names {
		names[i] = randomName(t, "dispatch_test_")
		if _, err := shared.admin.Exec(ctx, "create database "+names[i]+" strategy file_copy"); err != nil {
			t.Fatalf("create a database to drop: %v", err)
		}
	}
	testcleanup.Add(t, func() {
		if !dropped {
			for _, name := range names {
				dropDatabase(name)
			}
		}
	})
	trigger := fmt.Sprintf("storetest_pool_hold_%d", os.Getpid())
	control, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, "postgres"))+"&application_name=storetest-pool-hold")
	if err != nil {
		t.Fatalf("connect for the hold: %v", err)
	}
	testcleanup.Add(t, func() {
		_, _ = control.Exec(context.Background(), "drop event trigger if exists "+trigger)
		_, _ = control.Exec(context.Background(), "drop function if exists "+trigger+"()")
		_ = control.Close(context.Background())
	})
	// Held until 20 s past the connect end: past the row's give-up, half the margin past it, and
	// before go test's alarm, the margin past it, so a statement that waits for a drop's connection
	// is still waiting when the row gives up, and returns before the alarm.
	hold := time.Until(end) + 20*time.Second
	if hold >= testdb.NoDeadlineConnectTimeout {
		t.Fatalf("the hold would be %s, not under the drops' %s: run this row under a shorter -timeout (90s)", hold.Round(time.Second), testdb.NoDeadlineConnectTimeout)
	}
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
	dropped = true
	for _, name := range names {
		dropDatabase(name)
	}
	testwait.Until(t, "the drops to fill the admin pool", func() (string, bool) {
		stat := shared.admin.Stat()
		return fmt.Sprintf("%d of %d connections being made", stat.ConstructingConns(), slots), int(stat.ConstructingConns()) >= slots
	})
	t.Logf("%d drops wait on logins held %s from %s; the connect end is in %s", slots, hold.Round(time.Second), time.Now().UTC().Format(time.RFC3339), time.Until(end).Round(time.Second))

	failed := make(chan string, 1)
	done := make(chan struct{})
	began := time.Now()
	go func() {
		defer close(done)
		Empty(stopsOnFatal{T: t, failed: failed})
	}()
	giveUp := time.NewTimer(time.Until(end.Add(testwait.Margin / 2)))
	defer giveUp.Stop()
	select {
	case <-done:
		left := time.Until(end)
		select {
		case message := <-failed:
			t.Logf("Empty's statement ended %s after it began, %s before the connect end: %s", time.Since(began).Round(time.Second), left.Round(time.Second), message)
			if left > 5*time.Second {
				t.Fatalf("Empty's statement ended %s before the connect end; want it to end at that end, its bound", left.Round(time.Second))
			}
		default:
			t.Fatal("Empty made its database while the drops held every connection of the admin pool: the pool was not full, so this run proves nothing")
		}
	case <-giveUp.C:
		t.Errorf("Empty's statement had not ended %s after it began, %s past the connect end: its wait for one of the admin pool's connections outlasts that end", time.Since(began).Round(time.Second), time.Since(end).Round(time.Second))
		// The verdict is given; the statement returns once a held login is let go, which this
		// waits for so it does not register a cleanup on a finished test. A hang guard only.
		guard := time.NewTimer(time.Until(end.Add(testwait.Margin - 5*time.Second)))
		defer guard.Stop()
		select {
		case <-done:
		case <-guard.C:
		}
	}
}
