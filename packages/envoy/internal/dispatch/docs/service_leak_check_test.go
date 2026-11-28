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

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// leakCheckChildEnv selects TestLeakCheckChild's case in a child of this test binary.
const leakCheckChildEnv = "DOCS_LEAK_CHECK_CHILD"

// leakCheckDatabaseEnv names the database the parent built for this child alone.
const leakCheckDatabaseEnv = "DOCS_LEAK_CHECK_DATABASE_URL"

// leakCheckHeld keeps the never-shut-down child's service alive until its binary exits.
var leakCheckHeld *Service

const (
	formerLeakBound = 30 * time.Second
	leakCheckHold   = 35 * time.Second

	leakCheckHoldAdvanced = "docs: the leak check's child advanced past the former 30s bound"
	leakCheckHoldEnded    = "docs: the leak check's child lets its shut-down service go"
)

// A child with a shut-down service holds it for 35 s on its fake clock, starting when the
// end-of-run check begins. The report must see it still held past the old cutoff, then wait
// for its collection. A child with a never-shut-down service must fail before entering the
// collection wait, even when it also holds a shut-down service. Both children run without
// their own deadline. The parent's shared wait kills a child that does not exit.
func TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown(t *testing.T) {
	// Not parallel: race_memory.py measures only the largest process, not the sum of the
	// parent and its children. Run one child at a time and do not overlap other docs tests.
	for _, test := range []struct {
		child    string
		wantExit int
		want     []string // Ordered lines in the child's stderr.
	}{
		{child: "shut-down-held", wantExit: 0, want: []string{
			leakCheckHoldAdvanced,
			leakCheckHoldEnded,
			"docs: 1 services built, 0 still alive after 35s",
		}},
		{child: "never-shut-down", wantExit: 1, want: []string{
			"docs: 2 services built, 2 still alive after",
			"docs: a service built by docs.TestLeakCheckChild is still alive: its Shutdown never started",
			"docs: a service built by docs.TestLeakCheckChild is still alive: its Shutdown returned and something still holds it",
			"docs: a service still alive fails this run",
		}},
	} {
		t.Run(test.child, func(t *testing.T) {
			database := storetest.Open(t)
			stdout, stderr, err := testwait.RunChild(t, os.Args[0], 0, "^TestLeakCheckChild$",
				leakCheckChildEnv+"="+test.child,
				leakCheckDatabaseEnv+"="+database.Pool.Config().ConnString(),
			)
			exit := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run the child: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
				}
				exit = exitErr.ExitCode()
			}
			if exit != test.wantExit {
				t.Fatalf("the child exited %d, want %d:\nstdout:\n%s\nstderr:\n%s", exit, test.wantExit, stdout, stderr)
			}
			if test.wantExit != 0 && strings.Contains(stderr, leakCheckHoldAdvanced) {
				t.Fatalf("the child waited for collection before failing the service never shut down:\n%s", stderr)
			}
			rest := stderr
			for _, line := range test.want {
				_, after, found := strings.Cut(rest, line)
				if !found {
					t.Fatalf("the child's stderr lacks %q in order:\n%s\nstdout:\n%s", line, stderr, stdout)
				}
				rest = after
			}
			t.Logf("child exit %d:\n%s", exit, stderr)
		})
	}
}

// TestLeakCheckChild makes the services its case needs and hands the verdict to TestMain.
func TestLeakCheckChild(t *testing.T) {
	// Not parallel: the child installs a clock and a callback for its process's leak report.
	child := os.Getenv(leakCheckChildEnv)
	if child == "" {
		t.Skip(leakCheckChildEnv + " is not set: this test is the child of TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown")
	}
	database, err := store.Open(context.Background(), os.Getenv(leakCheckDatabaseEnv))
	if err != nil {
		t.Fatalf("open the database the parent built, %s: %v", leakCheckDatabaseEnv, err)
	}
	testcleanup.Add(t, database.Pool.Close)
	deps := Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	}
	switch child {
	case "never-shut-down":
		leakCheckHeld = New(deps)
		deps.Events = events.NewBroker()
		// Give the report another service it would have to wait for if it did not fail at once.
	case "shut-down-held":
	default:
		t.Fatalf("unknown %s case %q", leakCheckChildEnv, child)
	}
	service := New(deps)
	shutdownLeakCheckService(t, service)
	holdFromTheCheck(service)
}

// holdFromTheCheck drives the report across the former cutoff without releasing the service.
// The report calls back with the same clock's elapsed time after each pass that still sees a
// live service. Only the next pass, which still sees it beyond 30 s, can release it.
func holdFromTheCheck(service *Service) {
	fake := clocktest.New(time.Unix(0, 0))
	leakReportClock = fake
	leakReportAfterGC = func(elapsed time.Duration) {
		if elapsed <= formerLeakBound {
			fake.Advance(formerLeakBound + time.Second - elapsed)
			fmt.Fprintln(os.Stderr, leakCheckHoldAdvanced)
			return
		}
		fake.Advance(leakCheckHold - elapsed)
		fmt.Fprintln(os.Stderr, leakCheckHoldEnded)
		runtime.KeepAlive(service)
		service = nil
		leakReportAfterGC = nil
	}
}
