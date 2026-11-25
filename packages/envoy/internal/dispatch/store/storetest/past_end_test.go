package storetest

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
// one statement before its connect end (Empty), so the admin pool holds an idle connection and
// the statements after the end need no new connect; it waits until a second past that end, then
// asks for an empty database (Empty) and a store cloned from a template it makes (OpenFrom: the
// template's create, its alter and the clone). docs' leak-check child does this under load, its
// template still being made 5 s past its connect end. A deadline on the statement itself, rather
// than on its wait for one of the admin pool's connections, fails each of them on an expired
// context, at once.
//
// The row says nothing on a machine too slow for its windows, and skips there, by name: where the
// child's first statement ends past its connect end, or where a statement past the end fails on
// the second adminExec's wait then has (a busy pool, pgxpool's ping of a connection idle more than
// a second, or a new connection's connect, the clone's store's among them) after that second has
// gone by. The child skips, and this row skips on the child's skip.
func TestATestsAdminStatementPastTheConnectEndStillRuns(t *testing.T) {
	if os.Getenv(pastEndChildEnv) != "" {
		t.Skip("this is the child's binary")
	}
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
	began := time.Now()
	failure := fatalOf(t, func(t testing.TB) { Empty(t) })
	if past := time.Since(end); past >= 0 {
		t.Skipf("the child's first statement (Empty) ended %s past its connect end, %s after it began (it answered %q): this machine is too slow for the row to say anything", past.Round(time.Millisecond), time.Since(began).Round(time.Millisecond), failure)
	}
	if failure != "" {
		t.Fatalf("the child's first statement (Empty), before its connect end, failed: %s", failure)
	}
	time.Sleep(time.Until(end.Add(time.Second)))
	for _, step := range []struct {
		name string
		call func(t testing.TB)
	}{
		{"Empty", func(t testing.TB) { Empty(t) }},
		{"OpenFrom", func(t testing.TB) { OpenFrom(t, "past-the-connect-end", func(string) error { return nil }) }},
	} {
		began := time.Now()
		failure := fatalOf(t, step.call)
		took := time.Since(began)
		if failure == "" {
			continue
		}
		if took >= time.Second && onThePastEndSecond(failure) {
			t.Skipf("%s, begun %s past the connect end, timed out after %s on the second its wait for an admin connection, or a new connection's connect, has past that end: this machine is too slow for the row to say anything: %s", step.name, began.Sub(end).Round(time.Millisecond), took.Round(time.Millisecond), failure)
		}
		t.Fatalf("%s, begun %s past the connect end, failed after %s: %s", step.name, began.Sub(end).Round(time.Millisecond), took.Round(time.Millisecond), failure)
	}
	t.Logf("Empty and OpenFrom ran %s past the connect end", time.Since(end).Round(time.Millisecond))
}

// onThePastEndSecond reports whether failure, a fatal of Empty or OpenFrom, is a timeout of the
// second a test's admin connection has past its connect end: adminExec's wait for one ("wait for
// an admin connection"), or the connect of the store a clone opens ("open isolated database"),
// whose connect_timeout is then a second too (testdb.WithConnectDeadline). A statement that fails
// on its own deadline says neither.
func onThePastEndSecond(failure string) bool {
	timedOut := strings.Contains(failure, "deadline exceeded") || strings.Contains(failure, "timeout")
	return timedOut && (strings.Contains(failure, "wait for an admin connection: ") || strings.HasPrefix(failure, "open isolated database: "))
}
