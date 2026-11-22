package docs

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// leakCheckChildEnv selects TestLeakCheckChild's case in a child of this test binary.
const leakCheckChildEnv = "DOCS_LEAK_CHECK_CHILD"

// leakCheckHeld keeps the never-shut-down child's service alive until its binary exits.
var leakCheckHeld *Service

// The end-of-run leak check (reportLeakedServices) fails a run for a service its test never shut
// down, and only for that. A service whose test shut it down is waited for until the collector
// has freed it, under the binary's own deadline, however long the loaded machine takes: under
// the race detector on a loaded runner a shut-down service has stayed reachable past the 30 s the
// check once gave every service, with no goroutine, finalizer or cleanup holding it, and the run
// failed for a test that had shut its service down. Each case runs this binary again with one
// test (TestLeakCheckChild) and reads the check's exit code and lines: one service shut down and
// then held for longer than leakWaitBudget, which the check waits out and passes; and one service
// never shut down, which it still fails, by the name of the test that built it.
func TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown(t *testing.T) {
	if os.Getenv("DISPATCH_TEST_DATABASE_URL") == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL is unset, so the child builds no service and nothing here is tested")
	}
	for _, test := range []struct {
		child    string
		wantExit int
		want     []string
	}{
		{child: "shut-down-held", wantExit: 0, want: []string{"docs: 1 services built, 0 still alive after"}},
		{child: "never-shut-down", wantExit: 1, want: []string{
			"docs: 1 services built, 1 still alive after",
			"docs: a service built by docs.TestLeakCheckChild is still alive and was never shut down",
			"docs: a service still alive fails this run",
		}},
	} {
		t.Run(test.child, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeakCheckChild$", "-test.timeout=3m")
			child.Env = append(os.Environ(), leakCheckChildEnv+"="+test.child)
			output, err := child.CombinedOutput()
			exit := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run the child: %v\n%s", err, output)
				}
				exit = exitErr.ExitCode()
			}
			if exit != test.wantExit {
				t.Fatalf("the child exited %d, want %d:\n%s", exit, test.wantExit, output)
			}
			for _, line := range test.want {
				if !strings.Contains(string(output), line) {
					t.Fatalf("the child's output lacks %q:\n%s", line, output)
				}
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
		Settle:    time.Hour,
	})
	switch child {
	case "shut-down-held":
		if err := service.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown document service: %v", err)
		}
		// Reachable past the budget the check once gave every service, then let go.
		go func() {
			time.Sleep(leakWaitBudget + 5*time.Second)
			runtime.KeepAlive(service)
		}()
	case "never-shut-down":
		leakCheckHeld = service
	default:
		t.Fatalf("unknown %s case %q", leakCheckChildEnv, child)
	}
}
