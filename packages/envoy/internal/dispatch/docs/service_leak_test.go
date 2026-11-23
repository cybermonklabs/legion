package docs

import (
	"fmt"
	"os"
	"runtime"
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
// for the services the tests released to be collected: the time it then takes to print what it
// found and exit. A service is unreachable the moment its test returns, but it is only reported as
// collected once a GC has cleared its weak pointer, and its shutdown's goroutines may still be
// unwinding when the last test returns, for as long as a loaded machine takes to run them.
const leakReportSpare = 30 * time.Second

// leakWaitStep is how long the report sleeps between the pairs of collections it forces.
const leakWaitStep = 250 * time.Millisecond

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

// liveServices reports how many services the tests built and names the ones a weak pointer still
// resolves, in the order they were built.
func liveServices() (built int, live []string) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		if entry.service.Value() != nil {
			live = append(live, entry.test)
		}
	}
	return len(builtServices), live
}

// reportLeakedServices forces collections until no service the tests built is still reachable, or
// until leakReportSpare before the test binary's deadline (testwait.BinaryRemaining, since it runs
// in TestMain after the tests, with no *testing.T), prints what it found to stderr, and reports
// whether a service is still alive. The wait has no span of its own, so a service a loaded machine
// is slow to release is not reported; under -timeout 0, which sets no deadline, it has no end, as
// testwait.Until has none. It fails the run for a live service: TestMain (persistence_test.go)
// turns a true here into a non-zero exit code when the tests themselves passed. A non-zero count of
// live services means a test built a service and never shut it down, so that service, its rooms
// and their documents stayed in memory for the rest of the binary's life; the lines after the count
// name the tests to fix, each by a t.Cleanup that shuts the service it built down.
func reportLeakedServices() bool {
	start := time.Now()
	left, bounded := testwait.BinaryRemaining(leakReportSpare)
	deadline := start.Add(left)
	var built int
	var live []string
	for {
		// Twice: the first collection can leave a service reachable from a finalizer it queued.
		runtime.GC()
		runtime.GC()
		built, live = liveServices()
		if len(live) == 0 || bounded && !time.Now().Before(deadline) {
			break
		}
		time.Sleep(leakWaitStep)
	}
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, len(live), time.Since(start).Round(time.Millisecond))
	for _, test := range live {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive\n", test)
	}
	if len(live) == 0 {
		return false
	}
	fmt.Fprintln(os.Stderr, "docs: a service still alive fails this run")
	return true
}
