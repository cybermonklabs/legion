package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// ScanIdleTimeout is how long a scan of a bucket's existing keys waits for its next entry before it
// gives up as timed out: a cache's read (Watcher), and ScanExistingKeys for the callers that pass
// it. It is the JetStream MaxWait the listener's handles carry, the idle bound nats.go's legacy
// watcher held a scan to before kvwatch moved to the jetstream watcher, which has none.
const ScanIdleTimeout = 10 * time.Second

// KeyScan counts what one pass over a bucket's existing keys delivered, as disjoint fields: Puts is
// the live keys handed to visitLive (entries whose operation is a PUT, from the scan's watch or read
// from the stream's leader for a listed key the watch left out), Markers the delete markers the
// watch streamed past to find them. A listed key the leader answers as holding no value counts as
// neither. They must not be reported as one total of "keys": a few claims beside many subjects would
// read as claims left unrestored (LEGION-360).
type KeyScan struct {
	Puts    int
	Markers int
}

// count adds entry to the scan and reports whether it is a live key. A delete, a purge and a
// record the server removed for its TTL (a purge, Entry) are markers.
func (s *KeyScan) count(entry Entry) bool {
	if entry.Op == OpPut {
		s.Puts++
		return true
	}
	s.Markers++
	return false
}

// ScanOption sets how a key scan's watch reads the bucket (ScanMetaOnly). The scan takes no
// jetstream.WatchOpt. Its watch must deliver each key's delete and purge markers, since the scan
// reads from the stream's leader each listed key its watch did not deliver, so a caller's
// IgnoreDeletes would cost one leader read per deleted key; and jetstream.WatchOpt's one method is
// unexported (nats.go v1.55.0 jetstream/kv.go), so the scan could not tell which options it was
// given. IncludeHistory, UpdatesOnly and ResumeFromRevision would each make the watch's first
// delivery something other than the newest message of each key, so none of them can be passed
// either.
type ScanOption func(*scanOptions)

// scanOptions is what a scan's ScanOptions set.
type scanOptions struct {
	// metaOnly has the watch deliver no values (ScanMetaOnly).
	metaOnly bool
}

// ScanMetaOnly has the scan's watch deliver each entry without its value (jetstream.MetaOnly), the
// watch nats.go's Keys runs, for a caller that needs only keys and revisions. Unlike Keys, the scan
// keeps each entry's revision, so a caller reads a revision without a round trip per key. A listed
// key the watch left out is still read whole from the stream's leader.
func ScanMetaOnly() ScanOption {
	return func(opts *scanOptions) { opts.metaOnly = true }
}

// ScanExistingKeys hands visitLive each live key kv holds under the key pattern keys
// (jetstream.AllKeys for every key), with its value and revision, and returns what it read. The
// keys come from the bucket's stream: its state (bus.KeyValue.ReadStreamState), then the list of
// every key the stream holds a message on through that state's last sequence
// (bus.KeyValue.StoredKeys), which names a key another client writes again while the scan runs. The
// values come from one watch of the same pattern, opened after the list, that delivers the markers
// too. The watch's nil entry ends only its first delivery, and that delivery can end before a key
// another client writes again while it runs, whose new message then comes after the nil entry
// (dispatch://INFRA-6356 found 1,438 such first deliveries in 14,231). So the scan ends only once it
// has handed over each listed key the bucket still holds: a listed key the first delivery left out
// is read from the stream's leader (bus.KeyValue.GetFromLeader), handed over where it holds a value,
// and skipped where the leader answers that it holds none. A key the watch delivers that the list
// does not name, one written after the list, is handed over too, and a key it delivers twice is
// handed over twice. visitLive never sees a marker, and an error it returns ends the scan with that
// error.
//
// Beside the watch, a scan sends one STREAM.INFO for the state, one for each page of the list, and
// one stream message get for each listed key the watch left out: none for a key whose newest message
// is a marker, however many the bucket holds. A key this build cannot read (bus.ErrRefused: too
// long, or outside nats.go's key alphabet) is delivered by the watch like any other, since a watch
// of a pattern names no key; one the watch left out cannot be read from the leader, and its refusal
// fails the scan.
//
// A snapshot short of the bucket is not a reading of it, so a scan that did not hand over every
// listed key fails the caller instead of handing it what happened to arrive. The jetstream watcher
// (nats.go v1.55.0 jetstream/kv.go) ends its first delivery two ways: with the nil entry, and by
// closing its updates with no nil at all when its subscription ends first, an ordered consumer it
// could not recreate or a closed connection. It has no idle timer, so the scan holds its own: a
// watch that delivers no entry for idle gives up as timed out. Only the nil entry is a whole first
// delivery. A failed read of the stream's state, of the list or of a left-out key fails the scan
// too. what names the caller at the head of each of those errors; a watch that cannot be made is
// the error nats.go gave.
//
// ctx is the caller's: the whole scan ends with it, the consumer the watch creates included, so a
// read answering a request waits no longer than that request's own bound. A caller that answers
// none, and holds the watch to its idle bound alone, passes context.Background(), and each read of
// the stream is then bounded by bus.RequestTimeout. The watch lives in a context derived from ctx
// and cancelled on return, so the goroutine nats.go parks on it ends with the scan. A scan that is
// done with its watcher while it is still subscribed -- its first delivery whole, timed out, ended
// by ctx, or ended by visitLive -- stops it and has it read until it closes: nats.go's delivery
// callback blocks holding the watcher's lock on a full 256-entry buffer, so a watcher stopped and
// left unread would park its subscription's goroutine for good. A stop that fails is logged through
// log at WARN, except after ctx's end, which unsubscribes the watch through nats.go itself.
func ScanExistingKeys(ctx context.Context, kv bus.KeyValue, keys, what string, idle time.Duration, log *slog.Logger, visitLive func(Entry) error, opts ...ScanOption) (KeyScan, error) {
	return scanKeys(ctx, kv, []string{keys}, false, what, idle, log, visitLive, opts)
}

// ScanFilteredKeys is ScanExistingKeys over several key patterns at once, for a caller reading a
// window of a bucket keyed by time (the webhook redelivery index): the list is read for each pattern
// in turn, each key taken once, and one watcher carries every pattern, so the window costs one
// consumer rather than one per pattern, and one STREAM.INFO for each pattern and page of the list.
// The scan is whole only once it has handed over each listed key of them all that the bucket still
// holds. Its one caller answers no request, so this scan holds its watch to its idle bound and each
// read of the stream to bus.RequestTimeout, as a scan under context.Background() does.
func ScanFilteredKeys(kv bus.KeyValue, keys []string, what string, idle time.Duration, log *slog.Logger, visitLive func(Entry) error, opts ...ScanOption) (KeyScan, error) {
	return scanKeys(context.Background(), kv, keys, true, what, idle, log, visitLive, opts)
}

// scanKeys is both scans (ScanExistingKeys, ScanFilteredKeys) over patterns: the stream's state;
// the list of each pattern's keys through its last sequence; one watch of them all
// (WatchFilteredKeys where filtered, else WatchKeys of the one pattern), opened even when the list
// names no key, so a scan makes exactly one; its first delivery up to the nil entry, the one place
// the watcher's two endings, ctx's end and the scan's own idle bound are told apart; and then each
// listed key for which that delivery carried neither a put nor a marker, read from the leader in
// the list's order.
func scanKeys(ctx context.Context, kv bus.KeyValue, patterns []string, filtered bool, what string, idle time.Duration, log *slog.Logger, visitLive func(Entry) error, opts []ScanOption) (KeyScan, error) {
	state, err := kv.ReadStreamState(ctx)
	if err != nil {
		return KeyScan{}, fmt.Errorf("%s: read the bucket's stream state: %w", what, err)
	}
	var listed []string
	delivered := map[string]bool{}
	for _, pattern := range patterns {
		keys, err := kv.StoredKeys(ctx, pattern, state.LastSeq)
		if err != nil {
			return KeyScan{}, fmt.Errorf("%s: %w", what, err)
		}
		for _, key := range keys {
			if _, alreadyListed := delivered[key]; !alreadyListed {
				listed = append(listed, key)
				delivered[key] = false
			}
		}
	}

	var options scanOptions
	for _, opt := range opts {
		opt(&options)
	}
	var watchOpts []jetstream.WatchOpt
	if options.metaOnly {
		watchOpts = append(watchOpts, jetstream.MetaOnly())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var watcher jetstream.KeyWatcher
	if filtered {
		watcher, err = kv.WatchFilteredKeys(ctx, patterns, watchOpts...)
	} else {
		watcher, err = kv.WatchKeys(ctx, patterns[0], watchOpts...)
	}
	if err != nil {
		return KeyScan{}, err
	}

	done := func() {
		// nats.go unsubscribes a watch when its context ends, the context the subscription carries,
		// so a stop that ran after that one is no fault of the scan's and says nothing worth a line.
		if err := watcher.Stop(); err != nil && ctx.Err() == nil {
			log.Warn("could not stop the "+what+" watch of the bucket", slog.String("error", err.Error()))
		}
		go bus.DrainWatcher(watcher)
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	var scan KeyScan
	updates := watcher.Updates()
firstDelivery:
	for {
		select {
		case entry, open := <-updates:
			if !open {
				return KeyScan{}, fmt.Errorf("%s: the bucket's watch ended after %d entries, before it had delivered them all", what, scan.Puts+scan.Markers)
			}
			if entry == nil {
				timer.Stop()
				done()
				break firstDelivery
			}
			timer.Reset(idle)
			live := JetStreamEntry(entry)
			delivered[live.Key] = true
			if !scan.count(live) {
				continue
			}
			if err := visitLive(live); err != nil {
				done()
				return KeyScan{}, err
			}
		case <-timer.C:
			done()
			return KeyScan{}, fmt.Errorf("%s: the bucket's watch delivered nothing for %s after %d entries: %w", what, idle, scan.Puts+scan.Markers, ErrScanTimedOut)
		case <-ctx.Done():
			done()
			return KeyScan{}, fmt.Errorf("%s: the read's context ended after %d entries, before the bucket's watch had delivered them all: %w", what, scan.Puts+scan.Markers, ctx.Err())
		}
	}

	for _, key := range listed {
		if delivered[key] {
			continue
		}
		entry, err := kv.GetFromLeader(ctx, key)
		if errors.Is(err, nats.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return KeyScan{}, fmt.Errorf("%s: read key %q from the bucket's leader: %w", what, key, err)
		}
		scan.Puts++
		if err := visitLive(LegacyEntry(entry)); err != nil {
			return KeyScan{}, err
		}
	}
	return scan, nil
}

// ErrScanTimedOut is the error a scan that delivered no entry within its idle bound carries
// (ScanExistingKeys).
var ErrScanTimedOut = errors.New("the scan timed out")
