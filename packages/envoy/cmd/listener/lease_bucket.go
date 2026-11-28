package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// The listener state bucket (bus.ListenerStateBucket) keeps one value per key and drops a key
// nobody has written for listenerStateMaxAge, so a lease record nobody renews leaves the bucket
// after 20 minutes, and so does a delivery ledger record or a clean-hand-back mark nobody writes
// again. The bucket holds the lease, the hand-back marks and the ledger alone: the sessions'
// last-seen records, which must outlive the session's last-seen window, are in a bucket of their
// own (bus.LastSeenBucket, last_seen_bucket.go), so no record of this bucket stands longer than
// its 20 minutes. A pod that starts between an earlier build's 10 minutes and 20 minutes after a
// killed holder's last renewal now finds that holder's record rather than none: it waits the
// lease's ttl and takes the lease as expired, with a settle hold and the ack wait that term owes
// its successor, where it took an absent record at once and settled nothing.
//
// listenerStateRequestWait is the JetStream wait of every request on the bucket's handle: a
// renewal that hangs must give up well inside the holder's validity (leaseTTL - leaseMargin) so
// its retry still lands in it. A bucket create the wait cuts short is a connection error the
// caller retries, and the retry finds the bucket the server made.
const (
	listenerStateMaxAge      = 20 * time.Minute
	listenerStateRequestWait = 2 * time.Second
)

// listenerStateOpenSends is how many of its requests' waits one open of the bucket may take
// (openListenerState). The bucket is opened on the lease's own connection (bus.Dial), which no bus
// client owns, so the server's refusal of a request reaches the open only as the connection's last
// error, read once the open has ended (listenerStateRefusal); bounded by the start's hang guard
// alone, a refused request would be sent again for 36 s before that read. Three waits is what an
// open took to give up on a request nothing answered when each was sent three times, so a grant
// without the bucket is named as soon as it was. The bound decides no result: an open it ends is a
// connection error, which the caller tries again (openDeliveryLease), and the try after a slow
// election finds the bucket and waits for its leader again.
const listenerStateOpenSends = 3

// errListenerStateRefused marks a listener whose NATS user may not use the listener state
// bucket: no retry opens it until the grant changes. It wraps the server's own violation.
var errListenerStateRefused = errors.New("NATS refused KV bucket " + bus.ListenerStateBucket +
	": the listener's NATS user lacks the grant for KV bucket " + bus.ListenerStateBucket +
	" (" + listenerGrant + ")")

// openListenerState opens the listener state bucket, creating it at replicas with file storage,
// listenerStateMaxAge and one value per key when it does not exist, with wait as the JetStream
// wait of every request on the handle it returns (listenerStateRequestWait in production). A
// bucket an earlier build created keeps its keys for a shorter age, so the open raises it to
// listenerStateMaxAge (bus.RaiseMaxAge): how long a killed holder's record stands past its last
// renewal is how long a pod that starts after it finds that record and counts a push binding as
// the dead holder's, and a create that finds the bucket there changes nothing of it. A bucket
// that keeps its keys longer, or for good, is left as it is. The open and the raise each wait on
// the cluster until it answers, ctx ends, the start's hang guard passes (bus.EnsureKeyValue,
// bus.RaiseMaxAge), or listenerStateOpenSends of wait have passed, whichever comes first; an open
// that ends for want of an answer is a connection error its caller retries.
//
// A refusal under the connection's grant is errListenerStateRefused, which errors.Is tells apart
// from a connection error. nats-server drops a publish the grant refuses and answers it only with
// `-ERR 'Permissions Violation for Publish to "<subject>"'`, which nats.go records as the
// connection's last error and reports to no request, so the refused request itself runs out at
// wait. So a failed open is a refusal when its own error is such a violation, or when the
// connection's last error changed across the open to a violation of a subject of this bucket
// (its stream KV_envoy_listener, or its keys $KV.envoy_listener.>). nats.go keeps one last error
// per connection, so a violation another goroutine causes in between hides this one, and the
// open reads as a connection error its caller retries.
func openListenerState(ctx context.Context, conn *nats.Conn, replicas int, wait time.Duration) (bus.KeyValue, error) {
	ctx, cancel := context.WithTimeout(ctx, listenerStateOpenSends*wait)
	defer cancel()
	before := conn.LastError()
	js, err := conn.JetStream(nats.MaxWait(wait))
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
	}
	kv, err := ensureAgedBucket(ctx, conn, js, bus.ListenerStateBucket, replicas, listenerStateMaxAge)
	if err == nil {
		return kv, nil
	}
	if violation := listenerStateRefusal(err, before, conn.LastError()); violation != nil {
		return bus.KeyValue{}, fmt.Errorf("%w: %w", errListenerStateRefused, violation)
	}
	return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
}

// ensureAgedBucket is the create and the raise of one bucket's open (openListenerState,
// openLastSeen): it creates bucket at replicas with file storage, age as its MaxAge and one value
// per key when it does not exist, and raises a found bucket that keeps its keys for a shorter age
// to age (bus.RaiseMaxAge), under ctx and over js, the caller's. It returns the create's or the
// raise's error as it came, which the caller wraps or reads for a refusal.
func ensureAgedBucket(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, replicas int, age time.Duration) (bus.KeyValue, error) {
	kv, err := bus.EnsureKeyValue(ctx, conn, js, &nats.KeyValueConfig{
		Bucket:   bucket,
		History:  1,
		TTL:      age,
		Storage:  nats.FileStorage,
		Replicas: replicas,
	})
	if err == nil {
		err = bus.RaiseMaxAge(ctx, conn, js, bucket, age)
	}
	return kv, err
}

// publishViolation captures the subject of the server's refusal of a publish.
var publishViolation = regexp.MustCompile(`Publish to "([^"]*)"`)

// grantViolation captures the subject of the server's refusal of a publish or of a subscription.
var grantViolation = regexp.MustCompile(`(?:Publish|Subscription) to "([^"]*)"`)

// listenerStateRefusal returns the permissions violation that refused a failed open of the
// listener state bucket, or nil when the open failed for another reason (bucketRefusal over that
// one bucket).
func listenerStateRefusal(err, before, after error) error {
	_, violation := bucketRefusal([]string{bus.ListenerStateBucket}, err, before, after)
	return violation
}

// sameLastError reports whether a and b are the same error value. nats.go records each
// violation as a new error, so a new refusal never equals the one it replaced. A value that
// cannot be compared counts as different rather than panicking.
func sameLastError(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}
