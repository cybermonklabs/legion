package storetest

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A Measure needs its test to run alone in the process (requireMeasuredAlone). One in a parallel
// test, or in a serial subtest of one, stops before its operation runs and says why; the same
// Measure in a test that no parallel test is above runs its operation. Every measuring test of the
// api package was once made parallel, and a window then read a 9.857 ms sleep it could not place
// while other tests ran beside it (envoy-go job 137701916089). This test runs in both builds: the
// rule holds wherever Measure runs.
func TestAMeasureInAParallelTestStopsBeforeItMeasures(t *testing.T) {
	database := Open(t)
	tracer := NewXidTracer()
	traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
	measure := func(t testing.TB) (stopped string, ran bool) {
		stopping := &stoppingTB{TB: t}
		measured := make(chan struct{})
		go func() {
			defer close(measured)
			tracer.Measure(stopping, traced.Pool, func() { ran = true })
		}()
		<-measured
		return stopping.stop(), ran
	}
	for _, test := range []struct {
		name  string
		under func(t *testing.T, body func(t *testing.T))
	}{
		{"a parallel test", func(t *testing.T, body func(t *testing.T)) {
			t.Run("parallel", func(t *testing.T) {
				t.Parallel()
				body(t)
			})
		}},
		{"a serial subtest of a parallel test", func(t *testing.T, body func(t *testing.T)) {
			t.Run("parallel", func(t *testing.T) {
				t.Parallel()
				t.Run("serial", body)
			})
		}},
	} {
		// Each row is judged inside the subtest that measures, so a run that filters a row out
		// judges nothing of it.
		t.Run(test.name, func(t *testing.T) {
			test.under(t, func(t *testing.T) {
				stopped, ran := measure(t)
				if ran {
					t.Errorf("a Measure in %s ran its operation, want it stopped before it measures", test.name)
				}
				if !strings.Contains(stopped, "t.Parallel") {
					t.Errorf("a Measure in %s stopped with %q, want a stop that names t.Parallel", test.name, stopped)
				}
			})
		})
	}
	if stopped, ran := measure(t); stopped != "" || !ran {
		t.Errorf("a Measure in a test no parallel test is above stopped with %q and ran its operation: %v, want it run", stopped, ran)
	}
}

// stoppingTB is a testing.TB whose Fatal, Fatalf and FailNow record why and stop the goroutine that
// calls them (runtime.Goexit), as a test's own do, without failing the test it wraps.
type stoppingTB struct {
	testing.TB
	mu      sync.Mutex
	stopped string
}

func (s *stoppingTB) Fatalf(format string, args ...any) {
	s.mu.Lock()
	s.stopped = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	runtime.Goexit()
}

func (s *stoppingTB) Fatal(args ...any) { s.Fatalf("%s", fmt.Sprint(args...)) }

func (s *stoppingTB) FailNow() { s.Fatalf("FailNow") }

// stop is why the TB was stopped, empty when it was not.
func (s *stoppingTB) stop() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}
