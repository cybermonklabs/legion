package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The listener state bucket (bus.ListenerStateBucket) keeps one value per key and drops a key
// nobody has written for its MaxAge, which the session's `seen.` records size: that record is the
// one answer every pod shares for a lapsed session's last_seen (session.SessionRegistry.LastSeen),
// which the registry reports for the session TTL and its last-seen retention past the record's
// write (session.SessionRegistry.LastSeenWindow), so the bucket keeps every key that long and the
// interest reaper's allowance more (store.InterestReapAllowance), and the window, not the bucket,
// ends the answer on every pod at once. At the default session settings that is the 2 hour TTL,
// its 2 hour retention and 15 minutes, 4 h 15 min (listenerStateMaxAge); a longer session TTL
// makes it longer (listenerStateAge).
// TestTheListenerStateBucketOutlivesTheLastSeenWindow holds listenerStateMaxAge to no less than a
// default registry's window and that allowance.
//
// The age keeps the other keys' bytes and none of their decisions. A lease record and a delivery
// ledger record decide nothing once bus.ListenerStateRecordLife, 20 minutes and the bucket's
// MaxAge before the `seen.` records sized it, has passed since their last write. A standby takes
// such a lease record as absent, at its revision, an unreleased one only once its revision has
// also stood the lease's ttl (lease.poll), a holder writes nothing over its own
// (lease.outlivedLocked), and the ledger takes such an attempt over (dedupe.KV). A
// clean-hand-back mark is read only by the term that replaced the revision it names
// (lease.HandBackMarked). A pod
// that starts between an earlier build's 10 minutes and 20 minutes after a killed holder's last
// renewal finds that holder's record within its record life: it waits the lease's ttl and takes
// the lease as expired, with a settle hold and the ack wait that term owes its successor, where
// that build found no record, took the lease at once and settled nothing.
var listenerStateMaxAge = listenerStateAge(session.DefaultLastSeenWindow)

// listenerStateAge is the MaxAge every open of the listener state bucket creates it at or raises it
// to (openListenerState) under a session registry whose last-seen window is lastSeenWindow
// (session.SessionRegistry.LastSeenWindow): that window and the interest reaper's allowance, and
// never less than at the default session settings (listenerStateMaxAge). A longer session TTL,
// ENVOY_SESSION_MAX_AGE or a session bucket that already keeps its records longer, raises it.
func listenerStateAge(lastSeenWindow time.Duration) time.Duration {
	return max(lastSeenWindow, session.DefaultLastSeenWindow) + store.InterestReapAllowance(store.InterestReapGrace)
}

// listenerStateAgeOf is the age each try of an open of the listener state bucket asks for under
// sessions, the start's (openDeliveryLease) and a late join's (joinDeliveryLease) alike: read at
// the try, so a try follows the TTL the registry runs with then (listenerStateAge). It is read at
// no other time: a TTL that moves while the pods run reaches the bucket at the next pod's start
// or join, since every pod of the machine opens the one bucket.
func listenerStateAgeOf(sessions *session.SessionRegistry) func() time.Duration {
	return func() time.Duration { return listenerStateAge(sessions.LastSeenWindow()) }
}

// listenerStateRequestWait is the JetStream wait of every request on the bucket's handle: a
// renewal that hangs must give up well inside the holder's validity (leaseTTL - leaseMargin) so
// its retry still lands in it. A bucket create the wait cuts short is a connection error the
// caller retries, and the retry finds the bucket the server made.
const listenerStateRequestWait = 2 * time.Second

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
// age as its MaxAge and one value per key when it does not exist, with wait as the JetStream wait
// of every request on the handle it returns (listenerStateRequestWait in production). age is
// listenerStateAge under the session registry's TTL in production (listenerStateAgeOf). A bucket
// that keeps its keys for a shorter age, one an earlier build created or one opened under a
// shorter session TTL, is raised to age (bus.RaiseMaxAge), and one that keeps them longer, or for
// good, is left as it is: how long a `seen.` record stands past its write must be at least how
// long past that write every pod answers a lapsed session's last_seen, and a create that finds
// the bucket there changes nothing of it. The open and the raise each wait on the cluster until
// it answers, ctx ends, the start's hang guard passes (bus.EnsureKeyValue, bus.RaiseMaxAge), or
// listenerStateOpenSends of wait have passed, whichever comes first; an open that ends for want of
// an answer is a connection error its caller retries.
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
func openListenerState(ctx context.Context, conn *nats.Conn, replicas int, age, wait time.Duration) (bus.KeyValue, error) {
	ctx, cancel := context.WithTimeout(ctx, listenerStateOpenSends*wait)
	defer cancel()
	before := conn.LastError()
	js, err := conn.JetStream(nats.MaxWait(wait))
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
	}
	kv, err := bus.EnsureKeyValue(ctx, conn, js, &nats.KeyValueConfig{
		Bucket:   bus.ListenerStateBucket,
		History:  1,
		TTL:      age,
		Storage:  nats.FileStorage,
		Replicas: replicas,
	})
	if err == nil {
		err = bus.RaiseMaxAge(ctx, conn, js, bus.ListenerStateBucket, age)
	}
	if err == nil {
		return kv, nil
	}
	if violation := listenerStateRefusal(err, before, conn.LastError()); violation != nil {
		return bus.KeyValue{}, fmt.Errorf("%w: %w", errListenerStateRefused, violation)
	}
	return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
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
