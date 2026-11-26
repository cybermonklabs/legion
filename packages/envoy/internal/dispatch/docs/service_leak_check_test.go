package docs

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// leakCheckChildEnv selects TestLeakCheckChild's case in a child of this test binary.
const leakCheckChildEnv = "DOCS_LEAK_CHECK_CHILD"

// leakCheckDatabaseEnv names, in a child, the database its parent built for the child's service
// (buildLeakCheckDatabase): the child only connects to it, so the work under its -test.timeout is
// its own test and not a database build, whose migration costs what the machine gives it.
const leakCheckDatabaseEnv = "DOCS_LEAK_CHECK_DATABASE_URL"

// leakCheckHeld keeps the never-shut-down child's service alive until its binary exits.
var leakCheckHeld *Service

// leakCheckReported reads how long the check waited before it wrote its count.
var leakCheckReported = regexp.MustCompile(`still alive after (\S+)\n`)

// The end-of-run leak check (reportLeakedServices) fails a run for a service its test never shut
// down, and only for that. A service whose test shut it down is waited for until the collector
// has freed it, under the binary's own deadline, however long the loaded machine takes: under
// the race detector on a loaded runner a shut-down service has stayed reachable past the 30 s the
// check once gave every service, with no goroutine, finalizer or cleanup holding it, and the run
// failed for a test that had shut its service down. Each case runs this binary again with one
// test (TestLeakCheckChild) and reads the check's exit code, its lines and how long it waited:
//   - one service shut down and then held for longer than leakWaitBudget, counted from the
//     check's own start (leakCheckStarted) and not from the test's end, so the database drops
//     storetest.Run waits for in between cannot shorten it: the check says at leakWaitBudget what
//     it is waiting for, waits it out and passes;
//   - one service shut down and held 5 s, in a child whose tests end later than its -test.timeout
//     less leakCheckMargin: the check still gives it leakWaitBudget, since the binary's alarm has
//     stopped by then (m.Run stops it before TestMain's check runs), and passes. A child's
//     -test.timeout covers its test alone: its database is built before the child starts, so a
//     build slower than the child's timeout - as a migration under -race on a loaded runner is -
//     leaves the case as it is;
//   - one service never shut down: the check fails it by the name of the test that built it, and
//     no later than leakWaitBudget allows, far sooner than the child's deadline, at which a
//     shut-down service would be given up on.
func TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown(t *testing.T) {
	// Not parallel: it runs three more copies of this race binary at once, each holding a service
	// until its end-of-run check, and holds the never-shut-down child's check to leakWaitBudget
	// plus 30 s of wall clock; beside the package's parallel tests those children would share the
	// runner's 4 vCPU and its memory with them.
	if os.Getenv("DISPATCH_TEST_DATABASE_URL") == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL is unset, so the child builds no service and nothing here is tested")
	}
	for _, test := range []struct {
		child    string
		timeout  string
		wantExit int
		want     []string
		// reportedWithin, when set, bounds how long the check waited before it reported.
		reportedWithin time.Duration
		// buildDelay is added to the build of the child's database (buildLeakCheckDatabase).
		buildDelay time.Duration
	}{
		{child: "shut-down-held", timeout: "3m", wantExit: 0, want: []string{
			"docs: the service built by docs.TestLeakCheckChild was shut down and is still reachable after 30s; waiting for it to be collected until",
			"docs: 1 services built, 0 still alive after",
		}},
		{child: "shut-down-near-the-timeout", timeout: "40s", wantExit: 0, want: []string{"docs: 1 services built, 0 still alive after"}},
		// The same case on a database whose build takes longer than the child's -test.timeout, as
		// a build under -race on a loaded runner can: the child's 40 s alarm went off while its
		// database's migration was still compiling libpg_query (envoy-go-race-docs soak pod
		// legion-ci-docsfork-soakb1, 2026-11-25). Its timeout keeps the case's shape, 1 s past
		// leakCheckMargin, and the build takes 1 s longer than that timeout.
		{child: "slow-database-near-the-timeout", timeout: "31s", buildDelay: 32 * time.Second, wantExit: 0, want: []string{"docs: 1 services built, 0 still alive after"}},
		{child: "never-shut-down", timeout: "3m", wantExit: 1, want: []string{
			"docs: 1 services built, 1 still alive after",
			"docs: a service built by docs.TestLeakCheckChild is still alive and was never shut down",
			"docs: a service still alive fails this run",
		}, reportedWithin: leakWaitBudget + 30*time.Second},
	} {
		t.Run(test.child, func(t *testing.T) {
			t.Parallel()
			database := buildLeakCheckDatabase(t, test.buildDelay)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeakCheckChild$", "-test.timeout="+test.timeout)
			child.Env = append(os.Environ(), leakCheckChildEnv+"="+test.child, leakCheckDatabaseEnv+"="+database.Pool.Config().ConnString())
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
			if test.reportedWithin > 0 {
				waited := leakCheckWaited(t, output)
				if waited > test.reportedWithin {
					t.Fatalf("the check reported the service never shut down after %s, want within %s:\n%s", waited, test.reportedWithin, output)
				}
			}
		})
	}
}

// leakCheckWaited is how long the child's check waited before it wrote its count, read from the
// count's line.
func leakCheckWaited(t *testing.T, output []byte) time.Duration {
	t.Helper()
	match := leakCheckReported.FindSubmatch(output)
	if match == nil {
		t.Fatalf("the child's output has no count line:\n%s", output)
	}
	waited, err := time.ParseDuration(string(match[1]))
	if err != nil {
		t.Fatalf("read how long the check waited from %q: %v", match[1], err)
	}
	return waited
}

// TestLeakCheckChild is the child of TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown:
// it builds one service and leaves it to the end-of-run check as its case says.
func TestLeakCheckChild(t *testing.T) {
	child := os.Getenv(leakCheckChildEnv)
	if child == "" {
		t.Skip(leakCheckChildEnv + " is not set: this test is the child of TestTheLeakCheckWaitsForAShutDownServiceAndFailsOneNeverShutDown")
	}
	database, err := store.Open(context.Background(), os.Getenv(leakCheckDatabaseEnv))
	if err != nil {
		t.Fatalf("open the database the parent built, %s: %v", leakCheckDatabaseEnv, err)
	}
	testcleanup.Add(t, database.Pool.Close)
	service := New(Deps{
		Store:    database,
		Events:   events.NewBroker(),
		Identity: headerIdentity(database),
		Origins:  text.NewOrigins("https://dispatch.example"),
		Settle:   time.Hour,
	})
	switch child {
	case "shut-down-held":
		if err := service.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown document service: %v", err)
		}
		holdFromTheCheck(service, leakWaitBudget+5*time.Second)
	case "shut-down-near-the-timeout", "slow-database-near-the-timeout":
		if err := service.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown document service: %v", err)
		}
		// End the test 2 s after the binary's -test.timeout less leakCheckMargin, so the check
		// starts past that instant, then hold the service 5 s into the check.
		limit, ok := testTimeout()
		if !ok {
			t.Fatalf("the child runs with no -test.timeout, so its check has no deadline to start past")
		}
		time.Sleep(time.Until(binaryStarted.Add(limit - leakCheckMargin + 2*time.Second)))
		holdFromTheCheck(service, 5*time.Second)
	case "never-shut-down":
		leakCheckHeld = service
	default:
		t.Fatalf("unknown %s case %q", leakCheckChildEnv, child)
	}
}

// buildLeakCheckDatabase builds the database a child's service runs on, in the parent, delay later
// than its build would take.
func buildLeakCheckDatabase(t *testing.T, delay time.Duration) *store.Store {
	t.Helper()
	time.Sleep(delay)
	return storetest.Open(t)
}

// holdFromTheCheck keeps service reachable until hold has passed since the end-of-run check began,
// then lets it go.
func holdFromTheCheck(service *Service, hold time.Duration) {
	go func() {
		<-leakCheckStarted
		time.Sleep(hold)
		runtime.KeepAlive(service)
	}()
}
