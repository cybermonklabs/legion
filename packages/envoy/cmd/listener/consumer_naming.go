package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// fixedNamingRetry is how long a fixed-mode start waits before it reads the selection bucket's
// marker again after a read that answered nothing (awaitFixedNaming).
const fixedNamingRetry = 2 * time.Second

// awaitFixedNaming reads, in the fixed mode, the selection bucket's cutover marker before the
// listener runs today's names (bus.SessionConsumers.RefuseConvertedStore), and answers:
//   - nil when there is no marker: no bucket, no sentinel, a marker that is not done, or a read the
//     connection's grant refuses, which is logged as a warning (the listener beside Dispatch and a
//     listener whose hub never gave it the window's grants read no bucket at all, and neither can
//     hold a converted one);
//   - bus.ErrConvertedStore when the marker says the cutover converted every session consumer, on
//     which the start ends, naming ENVOY_CONSUMER_NAMES;
//   - ctx's error when ctx ends first.
//
// Any other failed read (no connection, a timeout) tells nothing, so it is logged and read again
// every interval: a start that cannot read the marker waits rather than run today's names unread.
func awaitFixedNaming(ctx context.Context, consumers *bus.SessionConsumers, logger *logging.Logger, interval time.Duration) error {
	for logged := false; ; {
		readCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
		err := consumers.RefuseConvertedStore(readCtx)
		cancel()
		switch {
		case err == nil:
			return nil
		case consumerGrantRefused(err):
			logger.Warn("the NATS user may not read the session consumers' selection bucket; the fixed naming mode runs, as on a hub that holds no converted bucket",
				slog.String("bucket", consumernames.SessionBucket), slog.String("error", err.Error()))
			return nil
		case errors.Is(err, bus.ErrConvertedStore):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
		if !logged {
			logged = true
			logger.Warn("the fixed naming mode waits to read the session consumers' selection bucket before it runs today's names",
				slog.String("bucket", consumernames.SessionBucket), slog.String("error", err.Error()), slog.String("retry", interval.String()))
		}
		if err := waitUnlessDone(ctx, interval); err != nil {
			return err
		}
	}
}
