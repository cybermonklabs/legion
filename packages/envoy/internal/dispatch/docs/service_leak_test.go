package docs

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"
)

// The leak registry answers one question this package's tests cannot otherwise ask: when the test
// binary ends, how many of the services its tests built are still alive? A test that builds a
// service and never shuts it down keeps that service - and every room it loaded, with that room's
// document in memory - for the rest of the binary's run, which is the memory this check exists to
// catch. The registry holds each service through a weak pointer, so it pins nothing itself: what
// stays alive stays alive because a test left it so.

// leakWaitBudget bounds how long the report waits for a service its test never shut down to be
// collected: one its test dropped is unreachable at once, but only reported collected once a GC
// has cleared its weak pointer. A service its test did shut down is waited for until it is
// collected, under the binary's own deadline (leakCheckDeadline), however long that takes: under
// the race detector on a loaded runner a shut-down service has stayed reachable for more than this
// budget with no goroutine, finalizer or cleanup holding it, and failing the run for it named a
// test that had done what the report asks for.
const leakWaitBudget = 30 * time.Second

// leakWaitStep is how long the report sleeps between the pairs of collections it forces.
const leakWaitStep = 250 * time.Millisecond

// leakCheckMargin is what the wait for shut-down services leaves of the binary's deadline, so the
// report is written before the binary's timeout panics.
const leakCheckMargin = 30 * time.Second

// binaryStarted is when this test binary began, from which its -test.timeout counts.
var binaryStarted = time.Now()

// builtService is one service New built, named by the test that built it.
type builtService struct {
	service weak.Pointer[Service]
	test    string
}

var (
	builtServicesMu sync.Mutex
	builtServices   []builtService
)

// New runs from tests that run in parallel, so the hook appends under the mutex.
func init() { newServiceHook = recordBuiltService }

func recordBuiltService(service *Service) {
	entry := builtService{service: weak.Make(service), test: buildingTest()}
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	builtServices = append(builtServices, entry)
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

// liveService is a service a weak pointer still resolves: the test that built it, and whether
// that service was shut down (Shutdown stops it accepting work first, on every path).
type liveService struct {
	test     string
	shutDown bool
}

// liveServices reports how many services the tests built and the ones a weak pointer still
// resolves, in the order they were built.
func liveServices() (built int, live []liveService) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		if service := entry.service.Value(); service != nil {
			live = append(live, liveService{test: entry.test, shutDown: service.stopping.Load()})
		}
	}
	return len(builtServices), live
}

// leakCheckDeadline is when the wait for shut-down services gives up: leakCheckMargin before the
// binary's -test.timeout runs out, and no end when there is no timeout, as testwait's waits have
// none then.
func leakCheckDeadline() (time.Time, bool) {
	timeout := flag.Lookup("test.timeout")
	if timeout == nil {
		return time.Time{}, false
	}
	limit, ok := timeout.Value.(flag.Getter).Get().(time.Duration)
	if !ok || limit <= 0 {
		return time.Time{}, false
	}
	return binaryStarted.Add(limit - leakCheckMargin), true
}

// reportLeakedServices forces collections until no service the tests built is still reachable,
// prints what it found to stderr, and reports whether a service is still alive. It fails the run
// for one: TestMain (persistence_test.go) turns a true here into a non-zero exit code when the
// tests themselves passed. A service never shut down is given leakWaitBudget to be collected: one
// still alive then means a test built a service and never shut it down, so that service, its rooms
// and their documents stayed in memory for the rest of the binary's life, and the lines after the
// count name the tests to fix, each by a t.Cleanup that shuts the service it built down. A service
// that was shut down is waited for until it is collected, or until the binary's deadline is near
// (leakCheckDeadline): one still reachable then is held by something after its shutdown.
func reportLeakedServices() bool {
	start := time.Now()
	deadline, bounded := leakCheckDeadline()
	var built int
	var live []liveService
	for {
		// Twice: the first collection can leave a service reachable from a finalizer it queued.
		runtime.GC()
		runtime.GC()
		built, live = liveServices()
		if len(live) == 0 {
			break
		}
		if neverShutDown(live) && time.Since(start) >= leakWaitBudget {
			break
		}
		if bounded && !time.Now().Before(deadline) {
			break
		}
		time.Sleep(leakWaitStep)
	}
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, len(live), time.Since(start).Round(time.Millisecond))
	for _, service := range live {
		if service.shutDown {
			fmt.Fprintf(os.Stderr, "docs: a service built by %s was shut down and is still reachable\n", service.test)
		} else {
			fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive and was never shut down\n", service.test)
		}
	}
	if len(live) == 0 {
		return false
	}
	fmt.Fprintln(os.Stderr, "docs: a service still alive fails this run")
	return true
}

// neverShutDown is whether any of live was never shut down.
func neverShutDown(live []liveService) bool {
	for _, service := range live {
		if !service.shutDown {
			return true
		}
	}
	return false
}
