package docs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
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
	leakCheckHeldPast = 30 * time.Second
	leakCheckHold     = 35 * time.Second

	leakCheckHoldEnded = "docs: the leak check's child lets its shut-down service go"
	leakCheckWaited    = "docs: the leak check's child entered the collection wait"
)

var leakCheckHoldAdvanced = fmt.Sprintf("docs: the leak check's child held its shut-down service past %s", leakCheckHeldPast)

// A child with a shut-down service holds it for 35 s on its fake clock, starting when the
// end-of-run check begins. The report must see it still held past 30 s, then wait
// for its collection. With a never-shut-down service the report must fail without waiting
// and must not name a shut-down service that nothing holds. A third child starts the report
// where the shared wait gives up. It must still collect its shut-down service.
func TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown(t *testing.T) {
	// Not parallel: race_memory.py measures only the largest process, not the sum of the
	// parent and its children. Run one child at a time and do not overlap other docs tests.
	for _, test := range []struct {
		child    string
		guard    time.Duration
		wantExit int
		want     []string // Ordered lines in the child's stderr.
		notWant  []string
	}{
		{child: "shut-down-held", wantExit: 0, want: []string{
			leakCheckHoldAdvanced,
			leakCheckHoldEnded,
			fmt.Sprintf("docs: 1 services built, 0 still alive after %s", leakCheckHold),
		}},
		{child: "never-shut-down", wantExit: 1, want: []string{
			"docs: 2 services built, 1 still alive after",
			"docs: a service built by docs.TestLeakCheckChild is still alive: its Shutdown never started",
			"docs: a service still alive fails this run",
		}, notWant: []string{leakCheckWaited, "its Shutdown returned and something still holds it"}},
		// Keep 30 s for setup, which takes well under that even with -race, plus testwait's
		// 30 s margin. The child steps to give-up, so the timeout only guards against hangs.
		{child: "shut-down-near-the-timeout", guard: time.Minute, want: []string{
			"docs: 1 services built, 0 still alive after",
		}},
	} {
		t.Run(test.child, func(t *testing.T) {
			database := storetest.Open(t)
			stdout, stderr, err := testwait.RunChild(t, os.Args[0], test.guard, "^TestLeakCheckChild$",
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
			for _, line := range test.notWant {
				if strings.Contains(stderr, line) {
					t.Fatalf("the child's stderr contains unexpected %q:\n%s", line, stderr)
				}
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
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	// In this child only, leave collection to the leak check so a background GC
	// cannot clear a weak pointer before the report reads it.
	debug.SetGCPercent(-1)
	switch child {
	case "never-shut-down":
		leakCheckHeld = New(deps)
		deps.Events = events.NewBroker()
		leakReportAfterGC = func(time.Duration) { fmt.Fprintln(os.Stderr, leakCheckWaited) }
	case "shut-down-held", "shut-down-near-the-timeout":
	default:
		t.Fatalf("unknown %s case %q", leakCheckChildEnv, child)
	}
	service := New(deps)
	shutdownLeakCheckService(t, service)
	switch child {
	case "shut-down-held":
		holdFromTheCheck(service)
	case "shut-down-near-the-timeout":
		testwait.SetUpDone(t)
		// Run the end-of-run check while the stepped clock still applies. TestMain
		// runs it again after the child test returns and restores the wall clock.
		if reportLeakedServices() {
			t.Fatal("the end-of-run check reported a leak at the shared wait's give-up")
		}
	}
}

// holdFromTheCheck drives the report past 30 s without releasing the service.
// The report calls back with the same clock's elapsed time after each pass that still sees a
// live service. Only the next pass, which still sees it beyond 30 s, can release it.
func holdFromTheCheck(service *Service) {
	fake := clocktest.New(time.Unix(0, 0))
	leakReportClock = fake
	leakReportAfterGC = func(elapsed time.Duration) {
		if elapsed <= leakCheckHeldPast {
			fake.Advance(leakCheckHeldPast + time.Second - elapsed)
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
