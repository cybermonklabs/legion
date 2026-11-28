package docs

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The registry holds each service through a weak pointer. Only the test or the service's own
// work can keep it alive. Shutdown counts tell the report whether a test ever tried to shut it
// down and whether every call has returned, without reading the service's internal state.

var (
	// leakReportClock measures the report's elapsed time.
	leakReportClock = clock.Real()
	// leakReportAfterGC is a hook a child sets to run after a collection pass that still sees
	// a live service and will keep waiting.
	leakReportAfterGC func(elapsed time.Duration)
)

// builtService records the test that built a service and the number of Shutdown calls that began
// and returned.
type builtService struct {
	service   weak.Pointer[Service]
	test      string
	shutdowns int
	returned  int
}

func (entry *builtService) shutdownReturned() bool {
	return entry.shutdowns > 0 && entry.returned == entry.shutdowns
}

var (
	builtServicesMu sync.Mutex
	builtServices   []*builtService
	builtByService  = make(map[weak.Pointer[Service]]*builtService)
)

// New and Shutdown can run in parallel. The hooks write under builtServicesMu.
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
	builtByService[entry.service] = entry
}

// recordShutDownService counts each Shutdown at entry.
func recordShutDownService(service *Service) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	if entry := builtByService[weak.Make(service)]; entry != nil {
		entry.shutdowns++
	}
}

// recordEndedService counts each Shutdown at return.
func recordEndedService(service *Service) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	if entry := builtByService[weak.Make(service)]; entry != nil {
		entry.returned++
	}
}

// serviceEnded reports whether a service has had a Shutdown and all its calls returned.
func serviceEnded(service *Service) bool {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	entry := builtByService[weak.Make(service)]
	return entry != nil && entry.shutdownReturned()
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

// liveService is a snapshot of the shutdown state of a service the report has not cleared.
type liveService struct {
	test      string
	shutdowns int
	returned  int
}

func (s liveService) shutdownStarted() bool { return s.shutdowns > 0 }

func (s liveService) why() string {
	switch {
	case !s.shutdownStarted():
		return "its Shutdown never started"
	case s.returned < s.shutdowns:
		return "its Shutdown started and has not returned"
	default:
		return "its Shutdown returned and something still holds it"
	}
}

// liveServices reads the registry in build order. A missing or in-flight Shutdown counts as
// live from the call counts alone. After all calls return, only a resolving weak pointer counts.
func liveServices() (built int, live []liveService) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		if !entry.shutdownReturned() || entry.service.Value() != nil {
			live = append(live, liveService{test: entry.test, shutdowns: entry.shutdowns, returned: entry.returned})
		}
	}
	return len(builtServices), live
}

// collect runs twice because the first collection may queue a finalizer that holds a service.
func collect() {
	runtime.GC()
	runtime.GC()
}

// reportLeakedServices fails immediately if a test never started a service's Shutdown.
// Otherwise, it waits for each Shutdown to return and each service to become unreachable.
// The only bound is the binary's deadline less testwait's margin. With -timeout 0,
// there is no deadline. TestMain turns a true result into a nonzero exit code.
func reportLeakedServices() bool {
	start := leakReportClock.Now()
	collect()
	var built int
	var live []liveService
	neverShutDown := func(s liveService) bool { return !s.shutdownStarted() }
	done := func() bool {
		built, live = liveServices()
		return len(live) == 0 || slices.ContainsFunc(live, neverShutDown)
	}
	asks := 0
	// InMain already prints its error to stderr if the shared wait gives up.
	_ = testwait.InMain("the document services to finish Shutdown and be collected", func() bool {
		asks++
		if done() {
			return true
		}
		for _, service := range live {
			if service.returned < service.shutdowns {
				return false
			}
		}
		// At testwait's 10 ms pace, 25 asks give one collection pair about every 250 ms.
		if asks%25 != 0 {
			return false
		}
		collect()
		if done() {
			return true
		}
		if leakReportAfterGC != nil {
			leakReportAfterGC(leakReportClock.Now().Sub(start))
		}
		return false
	})
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, len(live), leakReportClock.Now().Sub(start).Round(time.Millisecond))
	for _, service := range live {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive: %s\n", service.test, service.why())
	}
	if len(live) == 0 {
		return false
	}
	fmt.Fprintln(os.Stderr, "docs: a service still alive fails this run")
	return true
}

// Hold the service across the end of the subtest so the test reads the constructor's cleanup,
// not the collector. The constructor must not shut it down until the subtest ends.
func TestAServiceTheTestConstructorBuildsHasEndedOnceItsTestEnds(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	var service *Service
	t.Run("building a service", func(t *testing.T) {
		service = newTestServiceOn(t, database)
		if serviceEnded(service) {
			t.Fatal("Shutdown returned before the subtest ended")
		}
	})
	if !serviceEnded(service) {
		t.Fatal("Shutdown had not returned when the subtest ended")
	}
}

func TestTheLeakCheckNamesAServiceItsTestLeftRunning(t *testing.T) {
	t.Parallel()
	service := New(testServiceDeps(storetest.Open(t)))
	testcleanup.Add(t, func() { shutdownLeakCheckService(t, service) })
	_, live := liveServices()
	const name = "docs.TestTheLeakCheckNamesAServiceItsTestLeftRunning"
	if !slices.Contains(live, liveService{test: name}) {
		t.Fatalf("live services: %+v, want %s with no Shutdown calls", live, name)
	}
}

func TestTheLeakCheckNamesAServiceSomethingHoldsAfterItsShutdown(t *testing.T) {
	// Not parallel: it forces garbage collections for the whole process.
	service := New(testServiceDeps(storetest.Open(t)))
	shutdownLeakCheckService(t, service)
	collect()
	_, live := liveServices()
	runtime.KeepAlive(service)
	const name = "docs.TestTheLeakCheckNamesAServiceSomethingHoldsAfterItsShutdown"
	if !slices.Contains(live, liveService{test: name, shutdowns: 1, returned: 1}) {
		t.Fatalf("live services: %+v, want %s with one Shutdown that returned", live, name)
	}
}

func shutdownLeakCheckService(t *testing.T, service *Service) {
	t.Helper()
	ctx := testwait.Context(t)
	testwait.Returns(t, "document service Shutdown", func() {
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
}
