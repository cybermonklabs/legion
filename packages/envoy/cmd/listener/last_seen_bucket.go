package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The last-seen bucket (bus.LastSeenBucket) keeps one value per key, every session's `seen.`
// record, and drops a key nobody has written for its MaxAge. That record is the one answer every
// pod shares for a lapsed session's last_seen (session.SessionRegistry.LastSeen), which the
// registry reports for the session TTL and its last-seen retention past the record's write
// (session.SessionRegistry.LastSeenWindow), so the bucket keeps every key that long and the
// interest reaper's allowance more (store.InterestReapAllowance), and the window, not the bucket,
// ends the answer on every pod at once. At the default session settings that is the 2 hour TTL,
// its 2 hour retention and 15 minutes, 4 h 15 min (lastSeenMaxAge); a longer session TTL makes it
// longer (lastSeenAge). The records are in a bucket of their own so that this age keeps no lease
// or ledger record (bus.ListenerStateBucket, whose keys live 20 minutes, listenerStateMaxAge).
// TestTheLastSeenBucketOutlivesTheLastSeenWindow holds lastSeenMaxAge to no less than a default
// registry's window and that allowance.
var lastSeenMaxAge = lastSeenAge(session.DefaultLastSeenWindow)

// lastSeenAge is the MaxAge every open of the last-seen bucket creates it at or raises it to
// (openLastSeen) under a session registry whose last-seen window is lastSeenWindow
// (session.SessionRegistry.LastSeenWindow): that window and the interest reaper's allowance, and
// never less than at the default session settings (lastSeenMaxAge). A longer session TTL,
// ENVOY_SESSION_MAX_AGE or a session bucket that already keeps its records longer, raises it. Each
// try of the open reads the registry's window at the try (startLastSeenBucket), so a try follows
// the TTL the registry runs with then; a TTL that moves while the pods run reaches the bucket at
// the next pod's open, since every pod of the fleet opens the one bucket.
func lastSeenAge(lastSeenWindow time.Duration) time.Duration {
	return max(lastSeenWindow, session.DefaultLastSeenWindow) + store.InterestReapAllowance(store.InterestReapGrace)
}

// lastSeenOpenBound bounds one try of the last-seen bucket's open (openLastSeen):
// listenerStateOpenSends of listenerStateRequestWait, as one open of the listener state bucket
// is bounded. A healthy start waits for the first try to end before /v1 opens
// (startLastSeenBucket), so this is the most the bucket can hold that start back. The bound
// decides no result: a try it ends is a failure the next try follows, and the try after a slow
// election finds the bucket and waits for its leader again.
const lastSeenOpenBound = listenerStateOpenSends * listenerStateRequestWait

// openLastSeen opens the last-seen bucket on conn, creating it at replicas with file storage, age
// as its MaxAge and one value per key when it does not exist, with listenerStateRequestWait as
// the JetStream wait of every request on the handle it returns, the bound a request's read of a
// lapsed session's last-seen time takes beside its own budget (resolveCoreRoleHolder). A bucket
// that keeps its keys for a shorter age, one opened under a shorter session TTL, is raised to age
// (bus.RaiseMaxAge), and one that keeps them longer, or for good, is left as it is: how long a
// `seen.` record stands past its write must be at least how long past that write every pod
// answers a lapsed session's last_seen, and a create that finds the bucket there changes nothing
// of it. The open and the raise wait on the cluster until it answers, ctx ends, the start's hang
// guard passes, or lastSeenOpenBound has passed, whichever comes first.
//
// conn is the bus client's connection (bus.Client.Connection), not the lease's: the client
// records the server's refusal of each request, so a request the grant refuses ends at once with
// bus.ErrPublishDenied, as the spent-proof bucket's does (openSpentProofBucket), where the lease's
// connection, which no client owns, keeps one last error for the lease join's own refused tries
// too, and two refused opens there would hide each other (listenerStateRefusal).
func openLastSeen(ctx context.Context, conn *nats.Conn, replicas int, age time.Duration) (bus.KeyValue, error) {
	ctx, cancel := context.WithTimeout(ctx, lastSeenOpenBound)
	defer cancel()
	js, err := conn.JetStream(nats.MaxWait(listenerStateRequestWait))
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.LastSeenBucket, err)
	}
	kv, err := ensureAgedBucket(ctx, conn, js, bus.LastSeenBucket, replicas, age)
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.LastSeenBucket, err)
	}
	return kv, nil
}

// The lines the last-seen bucket's open logs (startLastSeenBucket), beside clusterStateLine,
// which it logs for an answer of the cluster's state with the bucket.
const (
	// lastSeenRefusedLine is the ERROR at each try the NATS grant refuses, with the bucket, the
	// grant it lacks, the refused subject, the server's text and the wait before the next try
	// (logGrantRefusal).
	lastSeenRefusedLine = "last-seen bucket refused by the NATS grant; keeping last-seen in process and retrying"
	// lastSeenAccountStateLine is the ERROR at each try the account's JetStream limits or the
	// server's own failure refuse, with the bucket, the code, the server's text and the wait before
	// the next try (logAccountState).
	lastSeenAccountStateLine = "last-seen bucket refused by the NATS account's limits or the server's state; keeping last-seen in process and retrying"
	// lastSeenOpenFailedLine is the WARN at each try that failed for any other reason, one no
	// answer came to within lastSeenOpenBound included, with the bucket, the wait before the next
	// try (startRetry) and the error.
	lastSeenOpenFailedLine = "last-seen bucket open failed; retrying"
	// lastSeenOpenedLine is the INFO of the try that opened the bucket, with the bucket.
	lastSeenOpenedLine = "last-seen bucket opened: recording each session's last-seen time in it"
	// lastSeenNotOpenedLine is the INFO an edge listener logs at its start in place of any try:
	// its grant holds no call of the bucket (natsgrant.Edge).
	lastSeenNotOpenedLine = "last-seen bucket not opened: an edge listener's grant holds no last-seen bucket; keeping last-seen in process"
)

// startLastSeenBucket opens the last-seen bucket on client's connection at replicas copies, at the
// age sessions' last-seen window asks for at each try (lastSeenAge), and once a try opens it gives
// it to sessions (session.SessionRegistry.SetLastSeenBucket) and has the client's keeper keep it
// (bus.Client.KeepBucket), logging lastSeenOpenedLine. It returns at once, with a channel closed
// when its first try has ended, whatever that try came to; the start waits on that before /v1
// opens, so a healthy start has the bucket before the first registration. Until a try opens the
// bucket the registry keeps last-seen times in its own process, as a listener without the bucket
// always did, and nothing else waits on it: neither the lease's open nor its join.
//
// It never ends the process. A try the NATS grant refuses (bus.ErrPublishDenied, which
// openLastSeen returns as the refusal arrives) is tried again after the wait logGrantRefusal
// returns, which logs lastSeenRefusedLine with grant, listenerGrant, the grant of every listener
// that opens the bucket (an edge listener opens none); a try answered from the cluster's state
// (bus.ClusterStateAnswer) after the wait logClusterState returns; a try the account's JetStream
// limits or the server's own failure refuse (bus.AccountStateAnswer, where the bucket's byte
// limit is the listener's code and no setting) after the wait logAccountState returns, which logs
// lastSeenAccountStateLine; and any other failure, a try lastSeenOpenBound ended included, after
// startRetry with one WARN, lastSeenOpenFailedLine. Each of the three refusals' waits starts at
// durableBindPoll and doubles up to durableRefusedRetryCap, so a grant, a cluster or an account
// that takes the bucket after the pod started is picked up within one wait and one try, with no
// restart.
//
// Every try and every wait runs under ctx, which the shutdown ends (listenerParts.stopLastSeen):
// the loop reads ctx as soon as a try returns, before it reads the try's result, so a try the stop
// cut logs nothing and gives the registry nothing, and a wait the stop cut ends the loop at once.
func startLastSeenBucket(ctx context.Context, client *bus.Client, replicas int, sessions *session.SessionRegistry, logger *logging.Logger) <-chan struct{} {
	firstTry := make(chan struct{})
	tried := sync.OnceFunc(func() { close(firstTry) })
	go func() {
		defer tried()
		refusedWait, stateWait := durableBindPoll, durableBindPoll
		clusterWait := clusterStateWait{next: durableBindPoll}
		var wait time.Duration
		for waitUnlessDone(ctx, wait) == nil {
			kv, err := openLastSeen(ctx, client.Connection(), replicas, lastSeenAge(sessions.LastSeenWindow()))
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				sessions.SetLastSeenBucket(kv)
				client.KeepBucket(bus.LastSeenBucket)
				logger.Info(lastSeenOpenedLine, slog.String("bucket", bus.LastSeenBucket))
				return
			}
			bucket := slog.String("bucket", bus.LastSeenBucket)
			if code, ok := bus.ClusterStateAnswer(err); ok {
				wait = logClusterState(logger, &clusterWait, bucket, replicas, code, err)
			} else if errors.Is(err, bus.ErrPublishDenied) {
				wait = logGrantRefusal(logger, &refusedWait, lastSeenRefusedLine, err, bucket, slog.String("grant", listenerGrant))
			} else if code, ok := bus.AccountStateAnswer(err, true); ok {
				wait = logAccountState(logger, &stateWait, lastSeenAccountStateLine, code, err, bucket)
			} else {
				logger.Warn(lastSeenOpenFailedLine, bucket, slog.String("retry_in", startRetry.String()), slog.String("error", err.Error()))
				wait = startRetry
			}
			tried()
		}
	}()
	return firstTry
}
