package docs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// leakCheckChildEnv selects TestLeakCheckChild's case in a child of this test binary.
const leakCheckChildEnv = "DOCS_LEAK_CHECK_CHILD"

// leakCheckHeld keeps the never-shut-down child's service alive until its binary exits.
var leakCheckHeld *Service

// formerLeakBound is the bound the end-of-run check once gave every service to be collected
// (INFRA-2963). The shut-down child holds its service past it.
const formerLeakBound = 30 * time.Second

// leakCheckHoldEnded is the line the shut-down child writes to stderr as its hold ends, while its
// service is still reachable: a count of no service alive after it is one the check made after
// the hold.
const leakCheckHoldEnded = "docs: the leak check's child lets its shut-down service go"

// The end-of-run leak check (reportLeakedServices) fails a run at once for a service no Shutdown
// of which ever started, and waits for a service its test shut down until the collector has freed
// it, through the shared wait (testwait.InMain), with no bound of its own: under the race detector
// on a loaded runner a shut-down service has stayed reachable past the 30 s the check once gave
// every service (formerLeakBound), with no goroutine, finalizer or cleanup holding it, and the run
// failed for a test that had shut its service down. Each case runs this binary again with one test
// (TestLeakCheckChild), once, under -timeout 0 (testwait.RunChild with no hang guard), where the
// shared wait has no end, and reads the check's exit code and its lines, each after the one before
// it:
//   - one service shut down and then held past formerLeakBound, counted from the check's own
//     start (leakCheckStarted) and not from the test's end, so the database drops storetest.Main
//     waits for in between cannot shorten it: the child writes leakCheckHoldEnded as it lets the
//     service go, and the check counts no service alive after that line, so it waited the hold
//     out and passed, however long a loaded machine took over the child's run;
//   - one service never shut down: the check fails it by the name of the test that built it, from
//     its state, without waiting, so the child exits on its own.
//
// A child whose check never ends is killed where this test's own wait gives up (testwait.RunChild),
// which fails the case by name: the parent's binary keeps the one hang guard, and no case holds a
// number of its own. Under a -timeout of 3 min, the shut-down child's check gave up 2 min 30 s after
// the child's start, so a slow child run and a long hold could pass it.
func TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown(t *testing.T) {
	// Not parallel: it runs two more copies of this binary at once, each holding a service until its
	// end-of-run check, one of them past formerLeakBound; beside the package's parallel tests those
	// children would share the runner's 4 vCPU and its memory with them.
	if !storetest.Available() {
		t.Skip("DISPATCH_TEST_DATABASE_URL is unset, so the child builds no service and nothing here is tested")
	}
	for _, test := range []struct {
		child    string
		wantExit int
		// want is the lines the child writes to stderr, each after the one before it.
		want []string
	}{
		{child: "shut-down-held", wantExit: 0, want: []string{
			leakCheckHoldEnded,
			"docs: 1 services built, 0 still alive after",
		}},
		{child: "never-shut-down", wantExit: 1, want: []string{
			"docs: 1 services built, 1 still alive after",
			"docs: a service built by docs.TestLeakCheckChild is still alive: its Shutdown never started",
			"docs: a service still alive fails this run",
		}},
	} {
		t.Run(test.child, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, err := testwait.RunChild(t, 0, "^TestLeakCheckChild$", leakCheckChildEnv+"="+test.child)
			exit := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run the child: %v\n%s\nstderr:\n%s", err, stdout, stderr)
				}
				exit = exitErr.ExitCode()
			}
			if exit != test.wantExit {
				t.Fatalf("the child exited %d, want %d:\n%s\nstderr:\n%s", exit, test.wantExit, stdout, stderr)
			}
			rest := stderr
			for _, line := range test.want {
				at := strings.Index(rest, line)
				if at < 0 {
					t.Fatalf("the child's stderr lacks %q after the lines of %q before it:\n%s\nstdout:\n%s", line, test.want, stderr, stdout)
				}
				rest = rest[at+len(line):]
			}
		})
	}
}

// TestLeakCheckChild is the child of TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown:
// it builds one service and leaves it to the end-of-run check as its case says.
func TestLeakCheckChild(t *testing.T) {
	child := os.Getenv(leakCheckChildEnv)
	if child == "" {
		t.Skip(leakCheckChildEnv + " is not set: this test is the child of TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown")
	}
	database := storetest.Open(t)
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    stagedAhead,
	})
	switch child {
	case "shut-down-held":
		if err := docstest.Shutdown(t, context.Background(), service); err != nil {
			t.Fatalf("shutdown document service: %v", err)
		}
		holdFromTheCheck(service, formerLeakBound+5*time.Second)
	case "never-shut-down":
		leakCheckHeld = service
	default:
		t.Fatalf("unknown %s case %q", leakCheckChildEnv, child)
	}
}

// holdFromTheCheck keeps service reachable until hold has passed since the end-of-run check began,
// then writes leakCheckHoldEnded to stderr and lets it go.
func holdFromTheCheck(service *Service, hold time.Duration) {
	go func() {
		<-leakCheckStarted
		time.Sleep(hold)
		fmt.Fprintln(os.Stderr, leakCheckHoldEnded)
		runtime.KeepAlive(service)
	}()
}
