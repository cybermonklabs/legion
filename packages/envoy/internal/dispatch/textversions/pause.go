package textversions

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
)

// pauseAskEvery is how often a paused fill asks again whether the answers reads are slow.
const pauseAskEvery = time.Second

// The lines a pause logs: one as it begins, one as it ends. Each names the threshold it holds to
// by its attributes, slow_read (sessionanswers.SlowRead) and slow_within
// (sessionanswers.SlowWithin), so its text stays the same when they change.
const (
	pausedMessage    = "dispatch: the keyword fill paused: an answers read took longer than slow_read within the last slow_within, or has run longer than that"
	continuesMessage = "dispatch: the keyword fill continues: no answers read has taken or run longer than slow_read for slow_within"
)

// pauser is the fill's pause (filler.pause), on the statistic of the answers reads this process
// serves (sessionanswers.Latency), which the answers route records and the fill shares. Before
// each batch the fill asks it whether a read that took longer than sessionanswers.SlowRead ended
// within the last sessionanswers.SlowWithin, or one has run longer than that and not ended
// (Latency.Slow); while either holds the fill writes nothing and asks again each pauseAskEvery,
// and it continues once neither has held for the last SlowWithin. It is asked between batches
// alone, so a batch that runs as a read turns slow is not interrupted: it commits, and the pause
// holds the next.
type pauser struct {
	latency *sessionanswers.Latency
	// now is the clock the pause asks Slow at, and wait waits between its asks, or until ctx ends
	// with its error.
	now    func() time.Time
	wait   func(ctx context.Context, d time.Duration) error
	logger *slog.Logger
}

// newPauser is the pause on latency's reads, logging to logger, on the process's clock.
func newPauser(latency *sessionanswers.Latency, logger *slog.Logger) pauser {
	return pauser{latency: latency, now: time.Now, wait: sleep, logger: logger}
}

// waitUntilFast returns at once while the answers reads are fast. Otherwise it logs that the fill
// paused, asks again each pauseAskEvery until they are fast, and logs that it continues, with how
// long it paused; ctx ending ends the pause with its error.
func (p pauser) waitUntilFast(ctx context.Context) error {
	paused := p.now()
	if !p.latency.Slow(paused) {
		return nil
	}
	p.logger.Info(pausedMessage, "slow_read", sessionanswers.SlowRead, "slow_within", sessionanswers.SlowWithin)
	for {
		if err := p.wait(ctx, pauseAskEvery); err != nil {
			return err
		}
		if now := p.now(); !p.latency.Slow(now) {
			p.logger.Info(continuesMessage, "paused_ms", now.Sub(paused).Milliseconds(),
				"slow_read", sessionanswers.SlowRead, "slow_within", sessionanswers.SlowWithin)
			return nil
		}
	}
}
