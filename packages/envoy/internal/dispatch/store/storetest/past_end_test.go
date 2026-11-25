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

// pastEndChildTimeout is the child's -timeout, docs' leak-check child's: its connect end
// (testdb.ConnectEnd) is 10 s into the run, time for one statement before it on a loaded machine.
const pastEndChildTimeout = 40 * time.Second

// TestATestsAdminStatementPastTheConnectEndStillRuns: a test may make its admin statements after
// its connect end, on a server that answers, and they run. The child, under -timeout 40s, makes
// one statement before its connect end (Empty), so the admin pool holds an idle connection and
// the statements after the end need no new connect; it waits until a second past that end, then
// asks for an empty database (Empty) and a store cloned from a template it makes (OpenFrom: the
// template's create, its alter and the clone). docs' leak-check child does this under load, its
// template still being made 5 s past its connect end. A deadline on the statement itself, rather
// than on its wait for one of the admin pool's connections, fails each of them on an expired
// context.
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
	Empty(t)
	if left := time.Until(end); left < 0 {
		t.Fatalf("the child's first statement ended %s past its connect end, so this machine is too slow for the row to say anything", -left)
	}
	time.Sleep(time.Until(end.Add(time.Second)))
	Empty(t)
	OpenFrom(t, "past-the-connect-end", func(string) error { return nil })
	t.Logf("Empty and OpenFrom ran %s past the connect end", time.Since(end).Round(time.Millisecond))
}
