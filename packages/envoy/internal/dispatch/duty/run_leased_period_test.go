package duty_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A lone loop of a fixed cadence - the compaction, issue-ask repair and prune loops, each on
// duty.RunLeased's ticker - must serve every period its run spans: each pass records the period
// after the one the pass before it recorded. Here every pass takes 40% of the interval, as a
// compaction sweep of many owed documents takes a noticeable share of its own. A loop whose next
// tick is armed only once the pass returns is due one interval plus that pass's time after the
// tick before, so its due times drift across the period boundaries and, about every two and a
// half passes here, jump over a whole period that no pass serves: the period a lone process
// loses. A loop that ticks on a ticker, its due times one interval apart while each pass takes
// less than the interval, serves every one.
func TestRunLeasedServesEveryPeriodOfALoneLoopWhosePassesTakeTime(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	const name = "document-compaction"
	const interval, passTime = time.Second, 400 * time.Millisecond
	const passes = 7

	// Each pass reads the period the pass before it recorded: Exclusive writes a pass's record once
	// the pass has returned, so the record a pass reads at its start is its predecessor's.
	recorded := make(chan time.Time, passes)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunLeased(ctx, leases, name, interval, duty.TickAtStart, func(ctx context.Context) {
			var boundary time.Time
			err := database.Pool.QueryRow(ctx, `select boundary from duty_passes where lease = $1`, name).Scan(&boundary)
			if err == nil {
				recorded <- boundary
			} else if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
				t.Errorf("read the loop's last recorded period: %v", err)
			}
			time.Sleep(passTime)
		})
	}()
	defer func() { stop(); <-done }()

	// Each recorded period is waited for until the test binary's deadline less testwait's margin,
	// so a loop that is merely slow on a loaded machine still passes.
	var got []time.Time
	for len(got) < passes-1 {
		got = append(got, testwait.Receive(t, fmt.Sprintf("recorded period %d of %d with an interval of %v", len(got)+1, passes-1, interval), recorded))
	}
	for i := 1; i < len(got); i++ {
		if step := got[i].Sub(got[i-1]); step != interval {
			t.Errorf("pass %d served the period at %s and the pass after it the period at %s, %v later: %d period(s) between them got no pass (each pass takes %v of the %v interval)",
				i+1, got[i-1].UTC().Format("15:04:05.000"), got[i].UTC().Format("15:04:05.000"), step, int(step/interval)-1, passTime, interval)
		}
	}
}
