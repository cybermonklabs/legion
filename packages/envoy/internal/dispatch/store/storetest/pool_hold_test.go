package storetest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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

// fatalOf runs call with t's Fatal and Fatalf recorded (stopsOnFatal), in a goroutine of its own,
// waits for it to end, and answers what it recorded: "" where call returned. The wait has no bound
// of its own: a call that never ends is bounded by go test's alarm.
func fatalOf(t *testing.T, call func(t testing.TB)) string {
	failed := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(stopsOnFatal{T: t, failed: failed})
	}()
	<-done
	select {
	case message := <-failed:
		return message
	default:
		return ""
	}
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
// under a -timeout short enough that the hold is under two minutes, against a server the run holds
// (an event trigger on login needs a superuser); the hold reaches only this binary's admin logins
// (their application_name, adminApplicationName). Run it alone, so no later row of the binary runs
// past the connect end while the held drops fill the pool, until 20 s past that end, and fails:
// STORETEST_POOL_HOLD=1 go test -timeout 90s -run '^TestATestsAdminStatementEndsByTheConnectEndWhileDropsHoldThePool$'.
// TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeld is the row CI runs for
// the same bound.
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

// poolHeldChildEnv selects TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeldChild
// in a child of this test binary.
const poolHeldChildEnv = "STORETEST_POOL_HELD_CHILD"

// poolHeldChildTimeout is the child's -timeout: its connect end (testdb.ConnectEnd) is 5 s into
// the run, and go test's alarm testwait.Margin past that end.
const poolHeldChildTimeout = 35 * time.Second

// TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeld is the row CI runs
// for the bound on a test's wait for an admin connection (adminExec), with no superuser and no
// variable to set. The child, under -timeout 35s, holds every connection of its admin pool itself
// (shared.admin.Acquire, MaxConns times) and asks for an empty database (Empty). Empty must end on
// its wait's deadline, the connect end, before half the margin past that end. A wait on
// context.Background() has no bound: it waits for a held connection, and the child fails by name
// half the margin past the end, then lets its connections go so the wait ends. The child skips,
// and this row on its skip, where a held connection's connect times out: the seconds left before
// the connect end bound each of them, and a login slower than that says nothing of the wait.
func TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeld(t *testing.T) {
	if os.Getenv(poolHeldChildEnv) != "" {
		t.Skip("this is the child's binary")
	}
	// It only waits on its child, so it runs beside this package's other such rows.
	t.Parallel()
	ready(t)
	ctx := testwait.Context(t)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeldChild$", "-test.timeout="+poolHeldChildTimeout.String(), "-test.v")
	child.Env = append(os.Environ(), poolHeldChildEnv+"=1")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Run(); err != nil {
		t.Fatalf("the child exited %v: a test's admin statement did not end by its connect end while every admin connection was held:\n%s", err, &output)
	}
	if strings.Contains(output.String(), "--- SKIP: TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeldChild (") {
		t.Skipf("the child skipped, so this machine was too slow for the row to say anything:\n%s", &output)
	}
	if !strings.Contains(output.String(), "--- PASS: TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeldChild (") {
		t.Fatalf("the child's row did not pass, so it held no connection and made no statement:\n%s", &output)
	}
}

// TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeldChild is the child of
// TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeld. It runs only when
// the parent re-executes this binary.
func TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeldChild(t *testing.T) {
	if os.Getenv(poolHeldChildEnv) == "" {
		t.Skip(poolHeldChildEnv + " is not set: this test is the child of TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeld")
	}
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Fatal("the child runs with no -test.timeout, so it has no connect end")
	}
	// Every connection the pool may make, each made at once and held. Each acquire makes a new
	// connection, whose connect is bounded by the seconds left before the connect end (openAdmin),
	// so none of these waits outlasts that end by more than a second.
	slots := int(shared.admin.Config().MaxConns)
	type acquired struct {
		connection *pgxpool.Conn
		err        error
	}
	answers := make(chan acquired, slots)
	for range slots {
		go func() {
			connection, err := shared.admin.Acquire(context.Background())
			answers <- acquired{connection, err}
		}()
	}
	var held []*pgxpool.Conn
	letGo := func() {
		for _, connection := range held {
			connection.Release()
		}
	}
	testcleanup.Add(t, letGo)
	var timedOut, failed []string
	for range slots {
		answer := <-answers
		switch {
		case answer.err == nil:
			held = append(held, answer.connection)
		case pgconn.Timeout(answer.err):
			timedOut = append(timedOut, answer.err.Error())
		default:
			failed = append(failed, answer.err.Error())
		}
	}
	if len(failed) > 0 {
		t.Fatalf("hold the admin pool's %d connections: %s", slots, strings.Join(failed, "; "))
	}
	if len(timedOut) > 0 {
		t.Skipf("%d of the admin pool's %d connections timed out on their connect, bounded by the connect end %s from now: this machine is too slow for the row to say anything: %s", len(timedOut), slots, time.Until(end).Round(time.Millisecond), strings.Join(timedOut, "; "))
	}

	failure := make(chan string, 1)
	done := make(chan struct{})
	began := time.Now()
	go func() {
		defer close(done)
		Empty(stopsOnFatal{T: t, failed: failure})
	}()
	giveUp := time.NewTimer(time.Until(end.Add(testwait.Margin / 2)))
	defer giveUp.Stop()
	select {
	case <-done:
		select {
		case message := <-failure:
			if !adminWait.MatchString(message) || !strings.Contains(message, "context deadline exceeded") {
				t.Fatalf("Empty ended %s after it began on %q; want its wait for an admin connection to end on that wait's deadline", time.Since(began).Round(time.Millisecond), message)
			}
			t.Logf("Empty ended %s after it began, %s before the connect end, on its wait's deadline: %s", time.Since(began).Round(time.Millisecond), time.Until(end).Round(time.Millisecond), message)
		default:
			t.Fatal("Empty made its database while every connection of the admin pool was held: the pool was not full, so this run proves nothing")
		}
	case <-giveUp.C:
		t.Errorf("Empty had not ended %s after it began, %s past the connect end: its wait for one of the admin pool's connections outlasts that end", time.Since(began).Round(time.Second), time.Since(end).Round(time.Second))
		// The verdict is given. The held connections are let go so the wait ends, and the row waits
		// for Empty so it does not register a cleanup on a finished test. A hang guard only.
		letGo()
		guard := time.NewTimer(time.Until(end.Add(testwait.Margin - 5*time.Second)))
		defer guard.Stop()
		select {
		case <-done:
		case <-guard.C:
		}
	}
}
