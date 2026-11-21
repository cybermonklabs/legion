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

// guardFixedNaming is the fixed mode's guard against running today's names over a selection
// bucket the cutover converted: it reads the marker before the start (awaitFixedNaming), and once
// the start goes on has every later connection read it again (WatchConversion), calling converted
// when a reconnect finds the store converted. A host's listener (ENVOY_LISTENER_ROLE=host) skips
// both and reads nothing: it runs fixed names for good. Its NATS user, the one its host's sessions
// share (natsgrant.Host), holds no read of the bucket, so each read would only have the server log
// a permissions violation at every start and connect; natsgrant.Host is retiring (plan E's t-e-59
// deletes it), no production connection holds it, and the cutover's window has the precondition
// that no host listener runs at or after it, so a host listener never meets a converted bucket and
// its durable is never converted.
func guardFixedNaming(ctx context.Context, host bool, consumers *bus.SessionConsumers, logger *logging.Logger, interval time.Duration, converted func(error)) error {
	if host {
		logger.Info("a host's listener runs fixed consumer names for good and reads no cutover marker",
			slog.String("role", bus.ListenerRoleHost), slog.String("bucket", consumernames.SessionBucket))
		return nil
	}
	if err := awaitFixedNaming(ctx, consumers, logger, interval); err != nil {
		return err
	}
	consumers.WatchConversion(converted)
	return nil
}

// awaitFixedNaming reads, in the fixed mode, the selection bucket's cutover marker before the
// listener runs today's names (bus.SessionConsumers.RefuseConvertedStore), and answers:
//   - nil when there is no marker: no bucket, a sentinel whose marker is not done, or a read the
//     connection's grant refuses, which is logged as a warning (the listener beside Dispatch and a
//     listener whose hub never gave it the window's grants read no bucket at all, and neither can
//     hold a converted one);
//   - bus.ErrConvertedStore when the marker says the cutover converted every session consumer, on
//     which the start ends, naming ENVOY_CONSUMER_NAMES;
//   - ctx's error when ctx ends first.
//
// Any other failed read tells nothing, so it is logged and read again every interval: a start that
// cannot read the marker waits rather than run today's names unread. That includes no connection
// and a timeout, and a bucket that exists with no sentinel or with a value under the sentinel's key
// that is not the sentinel, which is never no marker (bus.SessionConsumers.RefuseConvertedStore).
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
