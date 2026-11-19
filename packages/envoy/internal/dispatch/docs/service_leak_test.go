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

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The leak registry answers one question this package's tests cannot otherwise ask: once the
// tests have run, which of the services they built are still alive? A test that builds a service
// and never shuts it down keeps that service - and every room it loaded, with that room's
// document in memory - for the rest of the binary's run, which is the memory this check exists to
// catch, and so does a service that was shut down but that something still holds.
//
// A service is alive in one of two ways. Its own Shutdown has not finished (shutdownServiceHook):
// the package's test constructor (newTestServiceWith, service_test.go) shuts each service it
// builds down through the test's own t.Cleanup, before the test ends and so before the check
// runs, so a service of the constructor is never alive this way, and one a test built with New
// and left running is. Or its Shutdown has finished and a weak pointer to it still resolves after
// a collection: something still holds it. Something can hold a shut-down service for a while - a
// goroutine its Shutdown did not wait for, a timer that fires late - so the check waits for it to
// be let go, with no bound of its own: three runs failed on a service still reachable some 45 s
// after its test's cleanup had shut it down without error, under the 30 s bound this check once
// had (INFRA-2963). The registry holds each service through a weak pointer, so it pins nothing
// itself.

// builtService is one service New built, named by the test that built it, and whether it has
// ended: whether its own Shutdown has reported that it finished.
type builtService struct {
	service weak.Pointer[Service]
	test    string
	ended   atomic.Bool
}

var (
	builtServicesMu sync.Mutex
	builtServices   []*builtService
)

// New and Shutdown run from tests that run in parallel, so the hooks work under the mutex.
func init() {
	newServiceHook = recordBuiltService
	shutdownServiceHook = recordEndedService
}

func recordBuiltService(service *Service) {
	entry := &builtService{service: weak.Make(service), test: buildingTest()}
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	builtServices = append(builtServices, entry)
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
// ones still alive, in the order they were built: those whose Shutdown has not finished
// (running), and those whose Shutdown finished and which a weak pointer still resolves (held).
func liveServices() (built int, running, held []string) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		switch {
		case !entry.ended.Load():
			running = append(running, entry.test)
		case entry.service.Value() != nil:
			held = append(held, entry.test)
		}
	}
	return len(builtServices), running, held
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
// true here into a non-zero exit code when the tests themselves passed. A service whose Shutdown
// a test started and did not wait for can still be ending, and one that something holds after
// its Shutdown can still be let go, so the report waits for every service to have ended and gone
// through the shared wait (testwait.InMain), forcing collections in each ask only to read the
// second, and reports the ones still alive if that wait gives up at the test binary's deadline.
// The fix for a service whose Shutdown never finished is a t.Cleanup on the test named that
// shuts it down, or building it with newTestServiceWith, which does that; one held after its
// Shutdown is held by something that outlives it, which the fix lets go.
func reportLeakedServices() bool {
	start := time.Now()
	collect()
	built, running, held := liveServices()
	if len(running)+len(held) > 0 {
		if err := testwait.InMain("every service the tests built to end and be let go", func() bool {
			collect()
			built, running, held = liveServices()
			return len(running)+len(held) == 0
		}); err != nil {
			fmt.Fprintln(os.Stderr, "docs:", err)
		}
	}
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, len(running)+len(held), time.Since(start).Round(time.Millisecond))
	for _, test := range running {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: its Shutdown never finished\n", test)
	}
	for _, test := range held {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: its Shutdown finished and something still holds it\n", test)
	}
	if len(running)+len(held) == 0 {
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
// to the leak check, as one whose Shutdown never finished, under the name of the test that built
// it, so the run fails naming that test. This test shuts its service down itself once it has read
// that, so its own run's check finds nothing of it.
func TestTheLeakCheckNamesAServiceItsTestLeftRunning(t *testing.T) {
	database := storetest.Open(t)
	service := New(testServiceDeps(database))
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, running, _ := liveServices(); !slices.Contains(running, "docs.TestTheLeakCheckNamesAServiceItsTestLeftRunning") {
		t.Fatalf("the leak check reads the services whose Shutdown never finished as %q, which does not name this test, whose service runs", running)
	}
}

// A service whose Shutdown has finished and which something still holds past a collection is still
// alive to the leak check, as a held one, under the name of the test that built it. Here the test
// itself is what holds it; once the test has returned, nothing does, and its own run's check finds
// nothing of it.
func TestTheLeakCheckNamesAServiceSomethingHoldsAfterItsShutdown(t *testing.T) {
	database := storetest.Open(t)
	service := New(testServiceDeps(database))
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown document service: %v", err)
	}
	collect()
	_, running, held := liveServices()
	runtime.KeepAlive(service)
	const name = "docs.TestTheLeakCheckNamesAServiceSomethingHoldsAfterItsShutdown"
	if slices.Contains(running, name) || !slices.Contains(held, name) {
		t.Fatalf("the leak check reads the services whose Shutdown never finished as %q and the held ones as %q, want this test's service among the held ones alone", running, held)
	}
}
