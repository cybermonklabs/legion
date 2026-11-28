package storetest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// pastEndChildEnv selects TestATestsAdminStatementPastTheConnectEndStillRunsChild in a child of
// this test binary.
const pastEndChildEnv = "STORETEST_PAST_END_CHILD"

// pastEndChildTimeout is the child's -timeout: its connect end (testdb.ConnectEnd) is 30 s into
// the run, the time its first statement has before that end. That statement is a race binary's
// start, a login and a create database, whose file copy forces two checkpoints that every package
// on the server queues for (the package comment); docs' leak-check child, under -timeout 40s, was
// still making its template 5 s past an end 10 s into its run.
const pastEndChildTimeout = 60 * time.Second

// TestATestsAdminStatementPastTheConnectEndStillRuns: a test may make its admin statements after
// its connect end, on a server that answers, and they run. The child, under -timeout 60s, makes
// one statement first (Empty), begun 30 s before its connect end, so the admin pool holds an idle
// connection and the statements after the end need no new connect; it waits until a second past
// that end, then asks for an empty database (Empty) and a store cloned from a template it makes
// (OpenFrom: the template's create, its alter and the clone). docs' leak-check child does this
// under load, its template still being made 5 s past its connect end. A deadline on the statement
// itself, rather than on its wait for one of the admin pool's connections, fails each of them on
// an expired context, at once. The clone's pool, which its cleanup closes once the child's test
// ends, past the connect end, under the bound adminExec's wait takes there
// (closeThroughTheSharedWait), must then be closed, which the child reads in a cleanup that runs
// after the clone's, whatever that close's own wait reported.
//
// The row says nothing on a machine too slow for its windows, and skips there, by name: where a
// statement fails on a wait the connect end bounds that ran out, judged by the wait itself and not
// by how long its step took (pastEndWait): before the end, the first statement's wait for the
// admin pool, whose bound is the seconds left until the end; past it, one of the two waits that
// past the end have a second each. The child skips, and this row skips on the child's skip.
func TestATestsAdminStatementPastTheConnectEndStillRuns(t *testing.T) {
	if os.Getenv(pastEndChildEnv) != "" {
		t.Skip("this is the child's binary")
	}
	// It only waits on its child, so it runs beside this package's other such rows.
	t.Parallel()
	ready(t)
	ctx := testwait.Context(t)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestATestsAdminStatementPastTheConnectEndStillRunsChild$", "-test.timeout="+pastEndChildTimeout.String(), "-test.v")
	child.Env = append(os.Environ(), pastEndChildEnv+"=1")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Run(); err != nil {
		t.Fatalf("the child exited %v: an admin statement made past its connect end failed:\n%s", err, &output)
	}
	if strings.Contains(output.String(), "--- SKIP: TestATestsAdminStatementPastTheConnectEndStillRunsChild (") {
		t.Skipf("the child skipped, so this machine was too slow for the row to say anything:\n%s", &output)
	}
	if !strings.Contains(output.String(), "--- PASS: TestATestsAdminStatementPastTheConnectEndStillRunsChild (") {
		t.Fatalf("the child's row did not pass, so it made no statement past its connect end:\n%s", &output)
	}
}

// TestATestsAdminStatementPastTheConnectEndStillRunsChild is the child of
// TestATestsAdminStatementPastTheConnectEndStillRuns. It runs only when the parent re-executes
// this binary.
func TestATestsAdminStatementPastTheConnectEndStillRunsChild(t *testing.T) {
	if os.Getenv(pastEndChildEnv) == "" {
		t.Skip(pastEndChildEnv + " is not set: this test is the child of TestATestsAdminStatementPastTheConnectEndStillRuns")
	}
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Fatal("the child runs with no -test.timeout, so it has no connect end to be past")
	}
	// The clone's store, whose pool its cleanup closes once this test ends, past the connect end.
	// Registered before the steps, this runs after their cleanups: that close must have finished,
	// every connection of the pool destroyed, whatever its own wait reported.
	var cloned *store.Store
	testcleanup.Add(t, func() {
		if cloned == nil {
			return
		}
		if open := cloned.Pool.Stat().TotalConns(); open != 0 {
			t.Fatalf("the clone's pool still had %d connections once its cleanup had run, past the connect end: its close did not finish", open)
		}
		t.Log("the clone's pool closed in its cleanup, past the connect end")
	})
	// One statement first, so the admin pool holds an idle connection. Whenever it ends, the
	// statements after it run past the connect end: a failure of it skips only as pastEndWait
	// judges the failure itself, never on the moment it ended.
	stepRan(t, "the child's first statement (Empty)", fatalOf(t, func(t testing.TB) { Empty(t) }))
	// The row's subject is a statement made past the connect end, which is the moment this
	// binary's shared waits give up (testwait.GiveUpAt), so no shared wait can wait past it.
	time.Sleep(time.Until(end.Add(time.Second)))
	for _, step := range []struct {
		name string
		call func(t testing.TB)
	}{
		{"Empty", func(t testing.TB) { Empty(t) }},
		{"OpenFrom", func(t testing.TB) { cloned = OpenFrom(t, "past-the-connect-end", func(string) error { return nil }) }},
	} {
		began := time.Now()
		failure := fatalOf(t, step.call)
		took := time.Since(began)
		stepRan(t, fmt.Sprintf("%s, begun %s past the connect end and ended %s later,", step.name, began.Sub(end).Round(time.Millisecond), took.Round(time.Millisecond)), failure)
	}
	t.Logf("Empty and OpenFrom ran %s past the connect end", time.Since(end).Round(time.Millisecond))
}

// stepRan ends the child where what, a step of it, failed (failure, its fatal, is not ""): a skip
// where pastEndWait judges the failure the machine's, a fatal otherwise. A step that returned must
// leave every connection of the admin pool let go: adminExec releases its connection before it
// returns, the child makes its statements one at a time, and no drop is in flight, so a connection
// still held is one a statement never let go.
func stepRan(t *testing.T, what, failure string) {
	t.Helper()
	if failure == "" {
		if held := shared.admin.Stat().AcquiredConns(); held != 0 {
			t.Fatalf("%s returned with %d of the admin pool's connections still held: a statement never let its connection go", what, held)
		}
		return
	}
	why, skip := pastEndWait(failure)
	if skip {
		t.Skipf("%s failed because %s: this machine is too slow for the row to say anything: %s", what, why, failure)
	}
	if why != "" {
		t.Fatalf("%s failed, and %s: %s", what, why, failure)
	}
	t.Fatalf("%s failed: %s", what, failure)
}

// pastEndWait judges failure, a fatal of Empty or OpenFrom. A failure is the machine's, and skips,
// only where a wait the connect end bounds ran out, as the wait itself shows it, whatever the
// step's other statements took before it. Before the end that is adminExec's wait for one of the
// admin pool's connections, whose bound is the seconds left until the end. Past the end two waits
// have a second each: adminExec's wait (its semaphore, pgxpool's ping of a connection idle more
// than a second, or a new connection's connect), and the connect of the store a clone opens, whose
// connect_timeout is then a second (testdb.WithConnectDeadline):
//   - the store's: a connect that timed out ("failed to connect to" with pgconn's "timeout: "). A
//     deadline on store.Open's context, which past the end has passed, fails its ping at once on
//     the context's bare error, which says neither.
//   - the acquire's: a wait that ran its whole bound and then failed with no admin connection held
//     but the one a ping that ran out holds. adminExec's error says how long it waited and its
//     bound ("waited 1.001s of a 1s bound"); an acquire that failed at once, as one whose deadline
//     is the connect end itself does past that end, says it waited less, and fails the row, as
//     does an error that does not say. The child makes its statements one at a time with no drop
//     in flight, and each step that returned let every connection go (stepRan), so a connection
//     held as the step failed is one of its statements, or the ping its wait abandoned: pgxpool's
//     Acquire lets that ping end under the same deadline, then destroys its connection
//     (pool.go:660-698 at v5.13.0). So more than one held is a connection never let go, read once:
//     past the connect end no shared wait can wait for the ping's.
//
// It answers skip and the reason, or no skip and why the failure is the row's ("" where nothing
// more is to be said).
func pastEndWait(failure string) (why string, skip bool) {
	switch {
	case strings.HasPrefix(failure, "open isolated database: "):
		if strings.Contains(failure, "failed to connect to ") && strings.Contains(failure, "timeout: ") {
			return "the connect of the clone's store timed out on its second", true
		}
		return "it is no connect that timed out, so the store's connect was not what ran out", false
	case strings.Contains(failure, "wait for an admin connection"):
		match := adminWait.FindStringSubmatch(failure)
		if match == nil {
			return "its wait for an admin connection does not say how long it waited", false
		}
		waited, waitedErr := time.ParseDuration(match[1])
		bound, boundErr := time.ParseDuration(match[2])
		if waitedErr != nil || boundErr != nil {
			return fmt.Sprintf("its wait for an admin connection says it waited %q of a %q bound, which do not read as durations", match[1], match[2]), false
		}
		if waited < bound {
			return fmt.Sprintf("its wait for an admin connection failed after %s of its %s bound, so it did not wait for one: the wait's deadline had passed before it began", waited, bound), false
		}
		if held := shared.admin.Stat().AcquiredConns(); held > 1 {
			return fmt.Sprintf("%d of the admin pool's connections were held as it failed, more than the one a ping that ran out holds: a connection was never let go", held), false
		}
		return fmt.Sprintf("its wait for an admin connection ran its %s bound with no connection held but, at most, the one its abandoned ping holds", bound), true
	}
	return "", false
}

// adminWait reads how long a failed wait of adminExec's waited, and its bound.
var adminWait = regexp.MustCompile(`wait for an admin connection \(waited (\S+) of a (\S+) bound\): `)
