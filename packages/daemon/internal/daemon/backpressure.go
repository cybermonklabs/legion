package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/backpressure"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// pollBackpressure starts the poll of `backpressure_url` into hold on a goroutine of its own, and
// returns the stop that ends it and waits for it to end. It polls at once, so a hold that stands
// when the daemon starts holds the boot's own wakes and admission; a first poll that fails holds
// nothing, as every failed poll does.
func pollBackpressure(ctx context.Context, cfg config.Config, hold *backpressure.Hold, now func() time.Time, interval func() time.Duration) func() {
	polling, cancel := context.WithCancel(ctx)
	poller := &backpressure.Poller{URL: cfg.BackpressureURL, TokenFile: cfg.BackpressureTokenFile, Hold: hold, Now: now, Interval: interval}
	var done sync.WaitGroup
	done.Go(func() { poller.Run(polling) })
	return func() {
		cancel()
		done.Wait()
	}
}

// backpressureView is the hold that stands at now as the state shows it, nil while none stands
// and for a daemon with no `backpressure_url`.
func backpressureView(hold *backpressure.Hold, now time.Time) *api.BackpressureView {
	if hold == nil {
		return nil
	}
	view, ok := hold.View(now)
	if !ok {
		return nil
	}
	return &api.BackpressureView{
		Team: view.Team, Reason: view.Reason, HoldUntil: view.Until.UTC(), Since: view.Since.UTC(),
		HeldWakes: view.Wakes, HeldAdmissions: view.Admissions,
	}
}
