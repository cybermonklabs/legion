package bus

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The keeper's period runs on the clock WithKeepClock gives it: on a clocktest.Fake the keeper
// arms its first period at its interval, defaultKeepInterval or WithKeepInterval's, runs no pass
// while the fake stands before it, and runs one pass once the fake is moved there. The keeper arms
// each period after its first only once the pass of the period before has been counted (keep), so
// the count read as a period is armed is every pass the keeper ran before it. A keeper whose
// period runs on the wall clock arms nothing on the fake, and the wait for its first period fails
// naming it; one whose first tick comes at its start, before its first period, has counted that
// tick's pass when it arms the period, and the count read there fails the row.
func TestTheKeeperRunsAPassOnlyOnceItsClockReachesItsPeriod(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		interval time.Duration
		options  []ConnectOption
	}{
		{name: "the default period", interval: defaultKeepInterval},
		{name: "the period WithKeepInterval sets", interval: time.Minute, options: []ConnectOption{WithKeepInterval(time.Minute)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			keeps := clocktest.New(start)
			client, err := ConnectOwningStream([]string{testnats.URL(t)}, append([]ConnectOption{WithKeepClock(keeps)}, row.options...)...)
			if err != nil {
				t.Fatalf("start owning the stream: %v", err)
			}
			t.Cleanup(func() { closeClient(t, client) })

			keeps.AwaitArmedAt(t, "the keeper's first period", start.Add(row.interval))
			if n := client.keepPasses.Load(); n != 0 {
				t.Fatalf("the keeper ran %d passes before its clock reached its first period, want none", n)
			}

			keeps.Advance(row.interval)
			testwait.Until(t, "the keeper's pass at its first period", func() (uint64, bool) {
				n := client.keepPasses.Load()
				return n, n >= 1
			})
			keeps.AwaitArmedAt(t, "the keeper's second period, armed once the first period's pass was counted", start.Add(2*row.interval))
			if n := client.keepPasses.Load(); n != 1 {
				t.Fatalf("the keeper ran %d passes by its second period, want the first period's one", n)
			}
		})
	}
}
