package duty_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// periodGate is Exclusive's shape over the record duty_passes holds, kept in memory: a tick runs
// its pass unless the period it was due in is the one the last pass served, which is
// periodServed's rule, and the period a tick falls in is the product's own boundary
// (duty.PeriodBoundary of its due time). Every tick of a RunLeased loop carries that loop's
// interval as its period, so the scale a record was made on never differs from the scale it is
// read on here, and the comparison is the boundary alone. The lease is never held elsewhere -
// one loop, one process - and the pass is recorded once it has returned, as Exclusive records it.
type periodGate struct {
	mu     sync.Mutex
	served []time.Time
}

func (g *periodGate) exclusive(ctx context.Context, _ duty.Leases, _ string, due time.Time, period time.Duration, pass func(context.Context)) bool {
	boundary := duty.PeriodBoundary(due, period)
	g.mu.Lock()
	refused := len(g.served) > 0 && g.served[len(g.served)-1].Equal(boundary)
	g.mu.Unlock()
	if refused {
		return false
	}
	pass(ctx)
	g.mu.Lock()
	g.served = append(g.served, boundary)
	g.mu.Unlock()
	return true
}

// periods is the period every pass so far served, in order.
func (g *periodGate) periods() []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Time(nil), g.served...)
}

// A lone loop of a fixed cadence - the compaction, issue-ask repair and prune loops, each on
// duty.RunLeased's ticker - must serve every period its run spans: each pass records the period
// after the one the pass before it recorded. Here every pass takes 40% of the interval, as a
// compaction sweep of many owed documents takes a noticeable share of its own. A loop whose next
// tick is armed only once the pass returns is due one interval plus that pass's time after the
// tick before, so its due times drift across the period boundaries and, about every two and a
// half passes here, jump over a whole period that no pass serves: the period a lone process
// loses. A loop that ticks on a ticker, its due times one interval apart while each pass takes
// less than the interval, serves every one.
//
// The subject is a span - the interval between two ticks, and the period each tick is due in - so
// the row runs on a fake clock (testing/synctest), where a pass takes exactly its 40% of the
// interval and the ticker's ticks are exactly an interval apart. Measured on the wall clock
// instead, what the row read was the runner: a pass descheduled past its next tick overruns the
// interval, the ticker drops that tick (Go's runtime, time.go) and the period it would have
// served gets no pass - which is what RunLeased promises for a pass that takes longer than the
// interval, and is the machine's doing rather than the loop's. CI run 42968328144 read 600 ms
// between two served periods of a 300 ms interval for exactly that reason (dispatch://INFRA-1075).
//
// The gate is a periodGate rather than Exclusive against Postgres, because no bubbled goroutine
// may block on a socket (ExclusiveFunc): it judges each tick by the same boundary the product
// does, and what remains in the bubble is the loop, its ticker and the clock. Exclusive's own
// half - that a tick is bucketed by the moment it was due and by nothing measured after it - is
// read against a real database by TestExclusiveCountsALateTickInThePeriodItWasDueIn and
// TestExclusiveCountsATickStalledAfterItsMeasureInThePeriodItWasDueIn.
//
// The interval is this test's own argument to RunLeased, which takes it in the call, and the pass
// time is scaled with it: what this reads is that a pass taking most of an interval still serves
// every period, a property of the ratio and not of the second. cmd/dispatch passes each loop's
// own interval.
func TestRunLeasedServesEveryPeriodOfALoneLoopWhosePassesTakeTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const name = "document-compaction"
		const interval, passTime = 300 * time.Millisecond, 120 * time.Millisecond
		const periods = 7

		gate := &periodGate{}
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		booted := time.Now()
		go func() {
			defer close(done)
			duty.RunLeasedUnderGate(ctx, name, interval, duty.TickAtStart, gate.exclusive, func(context.Context) {
				time.Sleep(passTime)
			})
		}()
		// The tick at start serves the first period and the ticker's ticks the rest, so the loop
		// has run through every one of them half an interval past the last tick - a moment at
		// which no tick is due, so the cancel races nothing.
		time.Sleep((periods-1)*interval + interval/2)
		stop()
		<-done

		served := gate.periods()
		first := duty.PeriodBoundary(booted, interval)
		for i, boundary := range served {
			want := first.Add(time.Duration(i) * interval)
			if boundary.Equal(want) {
				continue
			}
			if i == 0 {
				t.Errorf("the first pass served the period %v past the one the loop booted in, want that one: the tick at start is due at the moment the loop starts",
					boundary.Sub(first))
				continue
			}
			t.Errorf("pass %d served the period %v after the boot period and the pass before it the period %v after it, %v apart: %d period(s) between them got no pass (each pass takes %v of the %v interval)",
				i+1, boundary.Sub(first), served[i-1].Sub(first), boundary.Sub(served[i-1]), int(boundary.Sub(served[i-1])/interval)-1, passTime, interval)
		}
		if len(served) != periods {
			t.Errorf("the loop served %d periods in %v with an interval of %v (each pass takes %v of it); want %d, one per period it ran through",
				len(served), time.Since(booted), interval, passTime, periods)
		}
	})
}
