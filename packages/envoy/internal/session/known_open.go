package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// OpenKnownSessions opens the envoy_known_sessions bucket if it exists, and starts the watcher
// that keeps its records in memory and the sweep that gives back the memory of the records that
// aged out. It creates no bucket: the operator's `envoy-infra buckets` creates it, with a 30-day
// TTL (KnownSessionRetention) on file storage (bus.CopiedBucketConfig), and so does a test fixture
// on a server of its own (internal/bus/bustest). A bucket the server says it does not hold leaves
// the store answering no record and writing none until an attempt opens it (BucketMissing).
//
// A bucket that does not open - a listener whose NATS user may not open it, on a hub whose grant
// does not carry the bucket - is no error and stops no start: the store is returned, logs the one
// WARN below, answers no record and writes nothing, and goes on attempting the bucket in the
// background until one succeeds (repairUntilStopped), at once after each connection event
// (Rewatch). The error is the JetStream context's alone, which leaves no store to open anything
// with; the caller then runs on a nil *KnownSessions, which answers as this store does and never
// opens. ctx bounds the first attempt, the one the start waits on (openKnownBucket).
func OpenKnownSessions(ctx context.Context, conn *nats.Conn, options ...KnownSessionsOption) (*KnownSessions, error) {
	opts := knownSessionsOpts{
		replicas:   1,
		now:        time.Now,
		retryFirst: knownBucketRetryFirst,
		retryMax:   knownBucketRetryMax,
	}
	for _, o := range options {
		o(&opts)
	}
	// A connection that gives no JetStream context can open nothing, now or later. It is this
	// function's one error, read here so the caller is answered it rather than meeting it on
	// every attempt; each attempt builds a context of its own, on the connection it runs against.
	if _, err := knownJetStream(conn); err != nil {
		return nil, err
	}
	s := newKnownSessions(conn, opts)
	kv, err := s.openBucket(ctx, conn)
	if err != nil {
		s.logger().Warn("the known-session bucket is not open; this listener answers no session's 30-day record and writes none until an attempt of its own opens the bucket",
			slog.String("bucket", bus.KnownSessionBucket), slog.String("error", err.Error()))
		s.armRepair()
	} else {
		s.startWatch(kv)
	}
	go s.sweepUntilStopped()
	return s, nil
}

// openBucket makes one attempt to open the bucket on conn within ctx (openKnownBucket) and records
// whether it found the bucket missing (bucketMissing), which a write then reads: the start's
// attempt and the background repair's alike.
func (s *KnownSessions) openBucket(ctx context.Context, conn *nats.Conn) (bus.KeyValue, error) {
	kv, err := openKnownBucket(ctx, conn, s.replicas)
	s.bucketMissing.Store(errors.Is(err, bus.ErrBucketMissing))
	return kv, err
}

// openKnownBucket makes one attempt to open the bucket on conn. The bucket is one a listener
// copies, which bus.EnsureKeyValue creates, with the retention and the copies the deployment asks
// for, only through a test fixture's connection (bus.CreateCopiedBucketsOn), and otherwise answers
// bus.ErrBucketMissing; the first attempt and the background repair's alike.
//
// The attempt asks for the bucket with one request first, waiting knownBucketOpenWait, and only a
// bucket the server says it does not hold goes through bus.EnsureKeyValue's create. That function
// sends a lookup the server leaves unanswered again until ctx ends or the start's hang guard
// passes (its sentAgain), and reads a refusal only on a connection a bus.Client owns, so a user
// whose grant does not carry the bucket would cost an attempt that whole wait rather than one
// send; the one request costs a denied attempt what it costs, the wait, and a granted one an
// answer. A create the server answers only at the bucket's first leader election (a test
// fixture's) is waited for the same way, so an election slower than a few of these 2 s sends
// opens the bucket on this attempt rather than leaving the store to write nothing until a retry
// does.
//
// The handle it returns is opened on a context with the wait every other store of the listener
// gives its bucket calls, since the store's writes and its cache watcher go through that handle
// and must not give up on a cluster that is merely slow: a write is bounded by its caller's own
// context (RecordWithin, bus.Bounded), and the open's short wait is for the attempt a start waits
// on, not for the work that follows it.
func openKnownBucket(ctx context.Context, conn *nats.Conn, replicas int) (bus.KeyValue, error) {
	opening, err := knownJetStream(conn)
	if err != nil {
		return bus.KeyValue{}, err
	}
	if _, err := opening.KeyValue(bus.KnownSessionBucket); errors.Is(err, nats.ErrBucketNotFound) {
		if _, err := bus.EnsureKeyValue(ctx, conn, opening, &nats.KeyValueConfig{
			Bucket:   bus.KnownSessionBucket,
			TTL:      KnownSessionRetention,
			Replicas: replicas,
			Storage:  nats.FileStorage,
		}); err != nil {
			return bus.KeyValue{}, err
		}
	} else if err != nil {
		return bus.KeyValue{}, err
	}
	serving, err := conn.JetStream(nats.MaxWait(knownBucketServeWait))
	if err != nil {
		return bus.KeyValue{}, err
	}
	return bus.OpenKeyValue(ctx, conn, serving, bus.KnownSessionBucket)
}
