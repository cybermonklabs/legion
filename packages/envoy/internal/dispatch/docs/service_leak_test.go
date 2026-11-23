package docs

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The leak registry answers one question this package's tests cannot otherwise ask: when the test
// binary ends, how many of the services its tests built are still alive? A test that builds a
// service and never shuts it down keeps that service - and every room it loaded, with that room's
// document in memory - for the rest of the binary's run, which is the memory this check exists to
// catch. The registry holds each service through a weak pointer, so it pins nothing itself: what
// stays alive stays alive because a test left it so.

// leakReportSpare is what the report leaves of the test binary's deadline when it stops waiting
// for the services the tests shut down to be collected: the time it then takes to print what it
// found and exit. A service is unreachable once its Shutdown has returned and its test has
// returned, but it is only reported as collected once a GC has cleared its weak pointer, and what
// its shutdown let go of may still hold it until a loaded machine runs it: a goroutine still
// unwinding, or a stopped timer the runtime has not yet cleared (docs/testing.md).
const leakReportSpare = 30 * time.Second

// leakWaitStep is how long the report sleeps between the pairs of collections it forces.
const leakWaitStep = 250 * time.Millisecond

// builtService is one service New built, named by the test that built it, with how many of its
// Shutdown calls have begun and how many have returned (shutdownHook).
type builtService struct {
	service   weak.Pointer[Service]
	test      string
	shutdowns int
	returned  int
}

var (
	builtServicesMu sync.Mutex
	builtServices   []*builtService
	// builtByService finds a service's entry as its Shutdown begins. Two weak pointers made from
	// one service are equal, so the key finds the entry and pins nothing.
	builtByService = make(map[weak.Pointer[Service]]*builtService)
)

// New and Shutdown run from tests that run in parallel, so the hooks write under the mutex.
func init() {
	newServiceHook = recordBuiltService
	shutdownHook = recordShutdown
}

func recordBuiltService(service *Service) {
	entry := &builtService{service: weak.Make(service), test: buildingTest()}
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	builtServices = append(builtServices, entry)
	builtByService[entry.service] = entry
}

// recordShutdown counts a Shutdown of service as it begins and returns what counts it as it
// returns. A service New did not build, such as a test's own &Service{}, has no entry, and its
// Shutdown is not counted.
func recordShutdown(service *Service) (returned func()) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	entry := builtByService[weak.Make(service)]
	if entry == nil {
		return func() {}
	}
	entry.shutdowns++
	return func() {
		builtServicesMu.Lock()
		defer builtServicesMu.Unlock()
		entry.returned++
	}
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

// liveService is a service the tests built that a weak pointer still resolves, with the counts of
// its Shutdown calls as the report read them.
type liveService struct {
	test      string
	shutdowns int
	returned  int
}

// released says a test shut the service down: one of its Shutdown calls has begun. The report
// waits for such a service to be collected, which a correct run brings once that Shutdown has
// returned and the goroutines and timers it stopped have let go of the service.
func (s liveService) released() bool { return s.shutdowns > 0 }

// why says what the report knows of why the service is still alive.
func (s liveService) why() string {
	switch {
	case !s.released():
		return "its test never shut it down"
	case s.returned < s.shutdowns:
		return "its Shutdown had not returned"
	default:
		return "it was shut down, and something still holds it"
	}
}

// liveServices reports how many services the tests built and the ones a weak pointer still
// resolves, in the order they were built.
func liveServices() (built int, live []liveService) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		if entry.service.Value() != nil {
			live = append(live, liveService{test: entry.test, shutdowns: entry.shutdowns, returned: entry.returned})
		}
	}
	return len(builtServices), live
}

// reportLeakedServices forces collections until no service a test shut down is still reachable,
// prints what it found to stderr, and reports whether a service is still alive. It waits on the
// services the tests shut down alone: each one's Shutdown returning and its collection, which a
// correct run brings however slowly a loaded machine runs it. It does not wait for a service no
// test shut down, the leak it exists to catch, since that service's own goroutines keep it alive
// for ever: such a service is reported as soon as every one shut down has gone. The wait has no
// bound of its own: it ends leakReportSpare before the test binary's deadline
// (testwait.BinaryRemaining, since it runs in TestMain after the tests, with no *testing.T), and
// under -timeout 0, which sets no deadline, only on its fact, as testwait.Until does. It fails the
// run for a live service: TestMain (persistence_test.go) turns a true here into a non-zero exit
// code when the tests themselves passed. A non-zero count of live services means a test built a
// service and never shut it down, so that service, its rooms and their documents stayed in memory
// for the rest of the binary's life, or that something still holds a service its test shut down;
// the lines after the count name each one's test and which of those it is.
func reportLeakedServices() bool {
	start := time.Now()
	left, bounded := testwait.BinaryRemaining(leakReportSpare)
	deadline := start.Add(left)
	var built int
	var live []liveService
	for {
		// Twice: the first collection can leave a service reachable from a finalizer it queued.
		runtime.GC()
		runtime.GC()
		built, live = liveServices()
		if !slices.ContainsFunc(live, liveService.released) || bounded && !time.Now().Before(deadline) {
			break
		}
		time.Sleep(leakWaitStep)
	}
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, len(live), time.Since(start).Round(time.Millisecond))
	for _, service := range live {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: %s\n", service.test, service.why())
	}
	if len(live) == 0 {
		return false
	}
	fmt.Fprintln(os.Stderr, "docs: a service still alive fails this run")
	return true
}
