package docs

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The leak registry answers one question this package's tests cannot otherwise ask: once the
// tests have run, which of the services they built are still alive? A test that builds a service
// and never shuts it down keeps that service - and every room it loaded, with that room's
// document in memory - for the rest of the binary's run, which is the memory this check exists to
// catch, and so does a service that was shut down but that something still holds.
//
// A service is alive in one of three ways. No Shutdown of it ever started (shutdownStartedHook):
// the package's test constructor (newTestServiceWith, service_test.go) shuts each service it
// builds down through a cleanup of the test's own (testcleanup.Add), before the test ends and so
// before the check runs, so a service of the constructor is never alive this way, and one a test
// built with New and left running is. Once the tests have run no test is left to shut such a
// service down, so the check fails it at once, from that state. Or its Shutdown started and has not finished
// (shutdownServiceHook): a test started it and did not wait for it. Or its Shutdown has finished
// and a weak pointer to it still resolves after a collection: something still holds it. A
// Shutdown still running ends by itself, and something can hold a shut-down service for a while -
// a goroutine its Shutdown did not wait for, a timer that fires late - so the check waits for
// those two, with no bound of its own: three runs failed on a service still reachable some 45 s
// after its test's cleanup had shut it down without error, under the 30 s bound this check once
// had (INFRA-2963). The registry holds each service through a weak pointer, so it pins nothing
// itself.

// leakCheckStarted is closed when the end-of-run check begins (reportLeakedServices), so a test
// of the check can time what it holds from the check's start rather than from its own end: the
// database drops storetest.Main waits for come between the two.
var leakCheckStarted = make(chan struct{})

// builtService is one service New built, named by the test that built it, whether a Shutdown of it
// has started, and whether it has ended: whether its own Shutdown has reported that it finished.
type builtService struct {
	service  weak.Pointer[Service]
	test     string
	shutDown atomic.Bool
	ended    atomic.Bool
}

var (
	builtServicesMu sync.Mutex
	builtServices   []*builtService
)

// New and Shutdown run from tests that run in parallel, so the hooks work under the mutex.
func init() {
	newServiceHook = recordBuiltService
	shutdownStartedHook = recordShutDownService
	shutdownServiceHook = recordEndedService
}

func recordBuiltService(service *Service) {
	entry := &builtService{service: weak.Make(service), test: buildingTest()}
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	builtServices = append(builtServices, entry)
}

// recordShutDownService marks the service whose Shutdown has just started as shut down.
func recordShutDownService(service *Service) {
	if entry := builtEntry(service); entry != nil {
		entry.shutDown.Store(true)
	}
}

// recordEndedService marks the service whose Shutdown has just finished as ended.
func recordEndedService(service *Service) {
	if entry := builtEntry(service); entry != nil {
		entry.ended.Store(true)
	}
}

// builtEntry is the registry's entry for service, nil for a service New did not record.
func builtEntry(service *Service) *builtService {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		if entry.service.Value() == service {
			return entry
		}
	}
	return nil
}

// serviceEnded reports whether service has ended: whether its own Shutdown has finished.
func serviceEnded(service *Service) bool {
	entry := builtEntry(service)
	return entry != nil && entry.ended.Load()
}

// buildingTest names the nearest frame of the call to New that belongs to a test of this package:
// the test to fix when the service it built is still alive at the end of the run. Helpers of this
// package that build a service for a test are skipped, so the name is the test's own.
func buildingTest() string {
	var pcs [64]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if name := frame.Function; strings.Contains(name, "docs.Test") {
			if cut := strings.LastIndex(name, "/"); cut >= 0 {
				name = name[cut+1:]
			}
			return name
		}
		if !more {
			return "an unnamed caller"
		}
	}
}

// liveServices reports how many services the tests built, and names the tests that built the
// ones still alive, in the order they were built: those no Shutdown of which ever started
// (unshut), those whose Shutdown started and has not finished (ending), and those whose Shutdown
// finished and which a weak pointer still resolves (held).
func liveServices() (built int, unshut, ending, held []string) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		switch {
		case !entry.shutDown.Load():
			unshut = append(unshut, entry.test)
		case !entry.ended.Load():
			ending = append(ending, entry.test)
		case entry.service.Value() != nil:
			held = append(held, entry.test)
		}
	}
	return len(builtServices), unshut, ending, held
}

// collect forces two collections: the first can leave a service reachable from a finalizer it
// queued.
func collect() {
	runtime.GC()
	runtime.GC()
}

// reportLeakedServices prints to stderr how many services the tests built and how many are still
// alive once the tests have run, names the test that built each one still alive and how, and
// reports whether there is one. It fails the run for one: TestMain (persistence_test.go) turns a
// true here into a non-zero exit code when the tests themselves passed. A service no Shutdown of
// which ever started is reported as the check finds it, with no wait: once the tests have run no
// test is left to shut it down. A service whose Shutdown a test started and did not wait for can
// still be ending, and one that something holds after its Shutdown can still be let go, so for
// those the report waits through the shared wait (testwait.InMain) for every service a Shutdown
// started on to have ended and been let go, forcing collections in each ask only to read the
// second, and reports the ones still alive if that wait gives up at the test binary's deadline.
// The fix for a service no Shutdown of which started, or whose Shutdown never finished, is a
// cleanup on the test named (testcleanup.Add) that shuts it down and waits for that, or building
// it with newTestServiceWith, which does that; one held after its Shutdown is held by something
// that outlives it, which the fix lets go. It closes leakCheckStarted as it begins.
func reportLeakedServices() bool {
	close(leakCheckStarted)
	start := time.Now()
	collect()
	built, unshut, ending, held := liveServices()
	if len(ending)+len(held) > 0 {
		if err := testwait.InMain("every service a Shutdown started on to end and be let go", func() bool {
			collect()
			built, unshut, ending, held = liveServices()
			return len(ending)+len(held) == 0
		}); err != nil {
			fmt.Fprintln(os.Stderr, "docs:", err)
		}
	}
	alive := len(unshut) + len(ending) + len(held)
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, alive, time.Since(start).Round(time.Millisecond))
	for _, test := range unshut {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: its Shutdown never started\n", test)
	}
	for _, test := range ending {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: its Shutdown started and never finished\n", test)
	}
	for _, test := range held {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: its Shutdown finished and something still holds it\n", test)
	}
	if alive == 0 {
		return false
	}
	fmt.Fprintln(os.Stderr, "docs: a service still alive fails this run")
	return true
}

// The package's test constructor ends the service it builds through the test's own cleanup, so
// the service has ended by the time the test that built it ends, before the leak check runs, and
// the check has nothing of it to wait for. The test holds the service itself, and what it reads is
// the service's own report that its Shutdown finished: no garbage collection decides it.
func TestAServiceTheTestConstructorBuildsHasEndedOnceItsTestEnds(t *testing.T) {
	var built *Service
	t.Run("building a service", func(t *testing.T) {
		built, _ = newTestService(t)
		if serviceEnded(built) {
			t.Fatal("the service has ended while the test that built it still runs")
		}
	})
	if built == nil {
		t.Skip("the subtest built no service")
	}
	if !serviceEnded(built) {
		t.Fatal("the service the test constructor built has not ended once the test that built it ended")
	}
}

// A service a test built outside the package's test constructor and left running is still alive
// to the leak check, as one no Shutdown of which started, under the name of the test that built
// it, so the run fails naming that test, at once. This test shuts its service down itself once it
// has read that, so its own run's check finds nothing of it.
func TestTheLeakCheckNamesAServiceItsTestLeftRunning(t *testing.T) {
	database := storetest.Open(t)
	service := New(testServiceDeps(database))
	testcleanup.Add(t, func() {
		if err := docstest.Shutdown(t, context.Background(), service); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, unshut, _, _ := liveServices(); !slices.Contains(unshut, "docs.TestTheLeakCheckNamesAServiceItsTestLeftRunning") {
		t.Fatalf("the leak check reads the services no Shutdown of which started as %q, which does not name this test, whose service runs", unshut)
	}
}

// A service whose Shutdown has finished and which something still holds past a collection is still
// alive to the leak check, as a held one, under the name of the test that built it: not as one no
// Shutdown of which started, which the check would fail at once, nor as one still ending. Here the
// test itself is what holds it; once the test has returned, nothing does, and its own run's check
// finds nothing of it.
func TestTheLeakCheckNamesAServiceSomethingHoldsAfterItsShutdown(t *testing.T) {
	database := storetest.Open(t)
	service := New(testServiceDeps(database))
	if err := docstest.Shutdown(t, context.Background(), service); err != nil {
		t.Fatalf("shutdown document service: %v", err)
	}
	collect()
	_, unshut, ending, held := liveServices()
	runtime.KeepAlive(service)
	const name = "docs.TestTheLeakCheckNamesAServiceSomethingHoldsAfterItsShutdown"
	if slices.Contains(unshut, name) || slices.Contains(ending, name) || !slices.Contains(held, name) {
		t.Fatalf("the leak check reads the services no Shutdown of which started as %q, the ones still ending as %q and the held ones as %q, want this test's service among the held ones alone", unshut, ending, held)
	}
}
