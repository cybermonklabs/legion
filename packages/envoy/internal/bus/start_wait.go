// The waits of a start on a fact of the cluster it cannot hurry, and the hang guard that bounds
// them: a stream's leader, a raised stream's copies, a request the server left unanswered sent
// again, and a lost read of a stream's state read again after its read limit. Each ends on the
// fact, on the server's answer or error, or on the guard, never on a clock that decides the result.

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

// streamStep is the step of a start a wait on a stream follows (awaitStream). It names the step
// in the wait's error, `create KV_legion_agents: the stream had no leader that takes writes when
// the wait ended: ...`, and says what a read answered "stream not found" means: after a create, a
// member that has not applied it yet, which is read again (awaitsApply); after any other step, a
// stream that is gone, which ends the wait.
type streamStep int

const (
	// afterCreate follows a create of the stream, this start's or a concurrent start's.
	afterCreate streamStep = iota + 1
	// afterRaise follows a raise of the stream's copies that this start made.
	afterRaise
	// afterOpen follows a read of a stream another program created or raised a moment ago.
	afterOpen
)

// String is the step's name in a wait's error.
func (s streamStep) String() string {
	switch s {
	case afterCreate:
		return "create"
	case afterRaise:
		return "raise"
	case afterOpen:
		return "open"
	}
	return "streamStep(" + strconv.Itoa(int(s)) + ")"
}

// awaitsApply reports whether a read answered "stream not found" after s is a member that has not
// applied a create yet (bucketAppliedInterval), which only a create leaves.
func (s streamStep) awaitsApply() bool {
	return s == afterCreate
}

// startHangGuard bounds one step of a start that waits on a fact of the cluster the start cannot
// hurry: an answer the server gives only once a stream's group has elected a leader (the create of
// a bucket or of the stream with more than one copy, a lookup at a member while the stream's leader
// is gone), a member applying a create (awaitBucketApplied), a stream's leader after a create or a
// raise (awaitStreamLeader), or the copies of a raise becoming current (awaitReplicasCurrent).
// Each of those waits ends on the fact, on the server's own error, or when the caller's context
// ends, whichever comes first; the guard ends the wait only on a cluster that never settles, so
// that start fails with an error naming what it waited for (errHangGuard) rather than hanging with
// its port answering "starting". No shorter bound decides the result: how long an election takes
// depends on the machine and on the election's own timers.
//
// The guard is three times the slowest such wait measured under load, the sizing docs/testing.md
// gives a bound of this kind. On 2026-11-17, on a dev box with 288 CPUs at a one-minute load of
// 1,375 to 2,125, fresh three-member clusters of nats-server v2.15.1 were raised and created 64 at a
// time. In 400 raises of a one-copy bucket to three copies, the stream had a leader and every copy
// was current at most 5.1 s after the raise was answered, and a create of a three-copy bucket was
// answered, at its first election, within 3.3 s. In 160 more, whose preferred member did not
// campaign at once, the group elected on its election timers alone (4 s to 9 s,
// minElectionTimeoutDefault and maxElectionTimeoutDefault in server/raft.go), which is the election
// a group holds when the preferred member's first campaign is lost: a leader and every copy current
// came at most 11.9 s after the raise was answered (one in ten took more than 10 s), and a create
// was answered within 8.7 s. Three times 11.9 s is 36 s. A step the guard ends fails the start at
// once: the open that hit the guard returns its own error, naming what it waited for, and the
// program exits with it. No probe ends such a start first, since the start-up probe reads /livez,
// which answers before the NATS connect.
const startHangGuard = 36 * time.Second

// errHangGuard is what a start's wait ends with, wrapped, when its hang guard passes before the
// fact it waits on holds (hangGuard).
var errHangGuard = errors.New("the start's hang guard passed")

// hangGuard is ctx bounded by guard as well. When guard passes first, ctx's cause is an error that
// wraps both errHangGuard, naming guard, and context.DeadlineExceeded, so a caller that reads a
// deadline off a start's error reads one whichever of the two ended the wait, and one that reads
// errHangGuard is told it was the guard.
func hangGuard(ctx context.Context, guard time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, guard, fmt.Errorf("%w after %s: %w", errHangGuard, guard, context.DeadlineExceeded))
}

// A create a clustered server answers is committed to the JetStream metadata group, not applied
// by every member yet: a member that has not applied the new stream assignment answers a read of
// the bucket's stream "stream not found", the member the create was sent through included. A probe
// over 25 fresh three-member clusters asked every member for a bucket's stream the instant
// CreateKeyValue answered, and 67 of the 75 member reads still said so, the slowest for 206 ms. So
// a read of a bucket this start has just created, or that a concurrent start created while this
// one looked, is taken again every bucketAppliedInterval until the member answers for it.
//
// bucketAppliedInterval is short against that 206 ms, so the wait ends within a small fraction of
// the catch-up it waits out, and each miss is one STREAM.INFO a member answers from memory. It is
// also the pause between two sends of a request the server left unanswered (sentAgain) and between
// two reads of a raised stream's copies (awaitReplicasCurrent).
const bucketAppliedInterval = 25 * time.Millisecond

// pause waits d out and reports nil, or reports ctx's cause when ctx ends first, which is what a
// wait between two reads of a fact ends with: the caller's context, or the start's hang guard.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// leaderless reports whether info describes a stream of more than one copy that has no leader
// that takes a write yet (leaderNotServing): one whose group has not elected one yet after a
// create or a raise, or whose new leader has not yet applied its first entry as leader. A stream
// of one copy is its one member's, and needs no election.
func leaderless(info *nats.StreamInfo) bool {
	return info.Config.Replicas > 1 && leaderNotServing(info) != ""
}

// leaderNotServing says why the stream info describes has no leader that takes a write yet, and
// is empty when it has one. A read names a group's new leader before that leader takes a write:
// nats-server reports the Raft node's leader (Cluster.Leader) from the moment the node wins its
// election (switchToLeader), and the stream takes a write only once that leader has applied its
// first entry as leader, when the node sets its leader state and the time it took the lead, which
// a read reports as Cluster.LeaderSince (v2.15.1 server/raft.go:6307 and :1603-1613,
// server/jetstream_cluster.go:14075-14076). A publish that reaches the leader in between is
// dropped with no answer (processClusteredInboundMsg, "not leader", jetstream_cluster.go:13120),
// and nats.go sends it once, so its caller waits out its whole bound for an acknowledgement that
// never comes and the stream holds nothing: the first publish after a raise to three copies failed
// so (envoy-go-race job 139083337221). The four releases read for this, v2.11.12, v2.12.4,
// v2.14.0 and v2.15.1 (the first of each line natsversion.FixedReleases admits, and the hub's),
// set LeaderSince at that one point and nowhere else; v2.11.8's Raft node keeps no such time. The
// wait assumes a server that sets it: a program checks its server against that floor only after
// its bus start has run, so on a server that never reports LeaderSince a wait on a stream of more
// than one copy would end at the start's hang guard, where the floor would have refused the server
// anyway. A read that a member other than the leader answers names the leader with no
// LeaderSince, the member's own, and reads as not serving yet, so the wait reads again. A stream
// of one copy has no Raft node, takes a write from its member at once and reports no LeaderSince
// (jetstream_cluster.go:14013-14019), so its named member is its serving leader: a start waits on
// such a stream when another start created a bucket at one copy (ensureKeyValue).
func leaderNotServing(info *nats.StreamInfo) string {
	switch {
	case info.Cluster == nil || info.Cluster.Leader == "":
		return "the stream has no leader yet"
	case info.Config.Replicas > 1 && info.Cluster.LeaderSince == nil:
		return info.Cluster.Leader + " leads the stream and takes no write yet"
	}
	return ""
}

// awaitStreamLeader waits until stream has a leader that takes a write (leaderNotServing), for as
// long as ctx lasts, after step: a create of more than one copy that this start made or that a
// concurrent start made (afterCreate), a raise of the notification stream's copies (afterRaise), or
// a read of a stream another program created or raised a moment ago (afterOpen). A create sent
// again while the stream's first election runs is answered before that election ends, and a raise
// of a one-copy stream at once (EnsureKeyValue, ensureStreamWithConfig); a stream whose leader is
// not serving yet answers no write, and a publish sent then draws no answer even once the leader
// serves. It asks for the leader alone and not for every copy current: a new stream holds no
// message a copy could be behind on, a create with one member of three stopped lands with that
// member offline and serves on the other two
// (TestOneServerStoppedACreateAtThreeCopiesLandsAndTheStoppedMemberCatchesUp), and the
// notification stream is read through consumers, which its leader serves.
func awaitStreamLeader(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, step streamStep, stream string) error {
	return awaitStream(ctx, logger, js, step, stream, "the stream had no leader that takes writes", "envoy nats stream has no leader yet, waiting", leaderNotServing)
}

// awaitStream reads stream every bucketAppliedInterval until short, given what a read answered,
// reports nothing short, or ctx ends: the wait after a raise (awaitReplicasCurrent) and after a
// create or a read of a stream with no leader (awaitStreamLeader), step naming which. Its first
// read that is short logs waiting to logger, the WARN that says a start waited and for which
// stream. The end of ctx is an error naming step, the stream, want (what was still short), the
// last thing short reported and ctx's cause.
//
// A read the server answers with an error is returned as it is, since the stream is there: this
// runs only after a raise or a create of it answered, or a read found it. The one exception is a
// member that has not yet applied a create, which answers "stream not found" for it
// (bucketAppliedInterval): after a create (streamStep.awaitsApply) that answer is read again. A
// read the server leaves unanswered (unanswered) says nothing of the stream, so it is read again,
// as sentAgain sends a request again. Each read is waited for up to the read limit of ctx's run of
// reads (limitedRead), so one lost read costs the start a fraction of a second rather than the
// caller's whole ten second wait after the fact it waits for is true; a read in flight when ctx
// ends is abandoned too.
func awaitStream(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, step streamStep, stream, want, waiting string, short func(*nats.StreamInfo) string) error {
	behind := "no read of the stream answered yet"
	limits := readLimitOf(ctx)
	for attempt := 1; ; attempt++ {
		info, wait, answered, err := limitedRead(ctx, limits, func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
		switch {
		case !answered && ctx.Err() != nil:
			return fmt.Errorf("%s %s: %s when the wait ended, a read in flight: %s: %w", step, stream, want, behind, context.Cause(ctx))
		case !answered:
			behind = fmt.Sprintf("the stream's read drew no answer within %s", wait)
		case unanswered(err):
			behind = "the stream's read drew no answer: " + err.Error()
		case step.awaitsApply() && bucketMissing(err):
			behind = "the member has not applied the create yet: " + err.Error()
		case err != nil:
			return fmt.Errorf("%s %s: read the stream: %w", step, stream, err)
		default:
			behind = short(info)
		}
		if behind == "" {
			return nil
		}
		if attempt == 1 {
			logger.Warn(waiting,
				slog.String("stream", stream),
				slog.String("behind", behind),
				slog.String("retry_in", bucketAppliedInterval.String()))
		}
		if ended := pause(ctx, bucketAppliedInterval); ended != nil {
			return fmt.Errorf("%s %s: %s when the wait ended: %s: %w", step, stream, want, behind, ended)
		}
	}
}

// sentAgain is what send answers, sent again while the server leaves it unanswered (unanswered)
// and ctx lasts, with request naming it in the line each further send logs to logger: the stream
// read and the create of ensureStreamWithConfig, the handle open EnsureKeyValue makes, and
// EnsureKeyValue's lookup of a KV bucket and its create (kv_ensure.go). A wait that runs out is
// either a request the server will never answer, on one of the paths listed below, or one it is
// still working on, and either is safe to send again: a read changes nothing, and a second identical
// create is deduplicated and carries the same id as the one that may have landed, so no second
// send can contradict the send before it (an update is read again rather than sent again,
// updateStream). The create of a stream of more than one copy is one the server is still working
// on until that stream's group elects its first leader, and the server answers it then: how many
// sends that takes depends on the election, so no count of sends, and no clock of its own, ends
// this wait. ctx does, which every caller bounds by the start's hang guard (startHangGuard), so a
// bus that is not serving JetStream at all ends the start at that guard, with an error that says
// how many sends drew no answer (unansweredUntil). Each send carries the wait of each request it
// makes, and a send in flight when ctx ends is abandoned and its answer dropped (Bounded): a create
// that lands after that leaves a stream the next start finds. Between two sends it pauses
// bucketAppliedInterval, so a request that a context of the send's own ends at once
// (requestsUnder) is not sent in a loop.
// Any answer, the server's refusal included, is returned at once, so a refused create is sent
// once. A refusal the server reports on the connection rather than in the request's answer
// arrives here as an answer too, because every request of a start goes through a JetStream
// context that reads the connection's record of it as it arrives (readsRefusals, refusedBy):
// EnsureKeyValue's lookup, create, raise and handle open, and ensureStreamWithConfig's read and
// create. So on a connection a client owns, a request the grant does not cover is sent once.
//
// Two programs starting at once each reconcile the one ENVOY_NOTIFICATIONS stream, and the start
// that loses the answer to its own stream read is the listener that dies ten seconds after it
// binds its port, serving nothing: cmd/listener's role-lane row
// (TestRoleLaneRefusedSharedQueueFallsBackToMachineGroupsAndStoresOneCopy), whose fatal line was
// the bare "context deadline exceeded" of the JetStream context's own ten-second wait.
// Nor is it the in-process server's own fault: the production server leaves a JetStream request
// unanswered too while its meta leader changes.
//
// A request that draws no answer never draws one later, because neither side sends it again:
// nats.go sends each JetStream management request once and waits (nats.go@v1.55.0
// js.go:3711-3729), and nats-server v2.15.1 returns from several paths these requests take with
// no response to the requester:
//   - the account $JS.API.INFO nats.go sends before a KV create (nats.go@v1.55.0 kv.go:422-434)
//     and a STREAM.CREATE, each at a member that is not the metadata leader
//     (server/jetstream_api.go:1461-1474 and :1550-1563), and a STREAM.INFO of a stream this
//     member holds no assignment for (a bucket not yet created, or one whose create this member
//     has not yet applied) at such a member (:2087-2093). Each of those answers
//     JSClusterNotAvailError instead once js.isLeaderless() is true, which it is only once the
//     metadata group is older than lostQuorumInterval and the member holds no leader
//     (jetstream_cluster.go:1481-1497); so the silence is a young metadata group, or a member
//     that has not yet seen its leader go. That error is an answer and is not sent again. The
//     STREAM.INFO handler sends its own 500 ms late (sendDelayedAPIErrResponse, errRespDelay at
//     jetstream_api.go:1211), to let the leader answer first;
//   - a STREAM.INFO of an existing bucket at a member that is not its stream's leader, while
//     that member does not yet see the stream's group as leaderless (:2100-2133): only the
//     stream's leader answers it;
//   - a STREAM.CREATE whose metadata proposal is refused because the term moved
//     (jetstream_cluster.go:11110-11112, with raft.go:1046-1060);
//   - every request pending in the JetStream API queue (a STREAM.CREATE) or the info queue (a
//     STREAM.INFO, the account $JS.API.INFO) when a push reaches that queue's limit, 10,000 by
//     default: the server drains that queue, answers none of them, logs a rate-limited warning
//     and publishes JSAdvisoryAPILimitReached to the system account
//     (jetstream_api.go:1029-1045), which is no answer to the caller.
//
// So a start that sent a request once through a member in one of those states waited that send's
// bound out, and ended the program over a send that was never going to be answered.
func sentAgain[T any](ctx context.Context, logger *slog.Logger, request string, send func() (T, error)) (T, error) {
	var last T
	var lastErr error
	for sends := 1; ; sends++ {
		answer, answered, err := Bounded(ctx, send)
		if !answered {
			return last, unansweredUntil(sends-1, lastErr, context.Cause(ctx))
		}
		if !unanswered(err) {
			return answer, err
		}
		last, lastErr = answer, err
		logger.Warn("envoy nats stream request unanswered; sending it again",
			slog.String("request", request), slog.Int("sent", sends), slog.String("error", err.Error()))
		if ended := pause(ctx, bucketAppliedInterval); ended != nil {
			return last, unansweredUntil(sends, lastErr, ended)
		}
	}
}

// unansweredUntil is sentAgain's error once ended, its context's cause, stopped it: the sends it
// made with no answer and the last of those sends' errors, nats.ErrTimeout or
// context.DeadlineExceeded, which unanswered() still reads through it, beside ended. The caller
// names the request (namedRequest), as it names the server's answers to it.
func unansweredUntil(sends int, last, ended error) error {
	if last == nil {
		return fmt.Errorf("no send answered when the wait ended: %w", ended)
	}
	return fmt.Errorf("%d sends drew no answer when the wait ended: %w: %w", sends, last, ended)
}

// firstReadLimit is how long a start waits for the answer to one read of a stream's state (a
// STREAM.INFO) before it takes that read as lost and reads again (readAgain, awaitStream). A read
// a member answers takes milliseconds, and a read it loses never gets an answer at all, so the
// caller's own JetStream wait, ten seconds, held the start that long after the fact it waited for
// was already true. It is three times the slowest answer measured, the sizing docs/testing.md gives
// a bound of this kind: on 2026-11-17, on a dev box with 288 CPUs at a one-minute load of 566 to
// 685, 448 fresh three-member clusters of nats-server v2.15.1, 64 and then 128 at a time, each had
// another start's three-copy create land with its first leader election held 11 s, and a start on
// another member read the stream every 25 ms until it had had a leader for 2 s. Of 167,230 reads,
// the slowest answered took 209 ms, and 153 drew no answer within ten seconds, in 150 of the 448
// rounds. Three times 209 ms is 627 ms.
//
// The limit decides no result. Each read the server leaves unanswered doubles the next one's
// limit, up to RequestTimeout (readLimit.lost), so a server that answers slower than the
// measurement is waited for at the caller's own wait after a few reads, and only the start's hang
// guard ends the wait. Each read the server answers sets the next one's limit to three times that
// read's own time, the same sizing, and never under this one (readLimit.answered): lost reads early
// in a run of reads do not make a lost read later in it cost more, and a server that answers every
// read late keeps a limit above its delay, so no read of it is cut and read again.
const firstReadLimit = 630 * time.Millisecond

// firstReadLimitKey is the context key a test that counts a start's requests sets its first read
// limit under (countedReads, in this package's tests), at the caller's whole wait, so a read
// a loaded machine answers past firstReadLimit is not read again and the count is the requests'
// own. Nothing outside the tests sets it.
type firstReadLimitKey struct{}

// firstReadLimitOf is the first read limit of the reads made under ctx: firstReadLimit, unless a
// test set another (firstReadLimitKey).
func firstReadLimitOf(ctx context.Context) time.Duration {
	if first, ok := ctx.Value(firstReadLimitKey{}).(time.Duration); ok {
		return first
	}
	return firstReadLimit
}

// readLimit is the limit of the next read of a stream's state in a run of reads: every read of one
// bucket's open (EnsureKeyValue), of one ensure of the notification stream (ensureStreamWithConfig)
// or of one setting of the session bucket's age (SetKeyValueMaxAge), each of which carries its run
// in its context (withReadLimit), so that a delay one step's reads measured holds for the next
// step's first read and a slow server is not learnt again at every step. The reads of a run are made
// one after the other, never two at once. A read made under a context that carries no run, such as
// a keeper pass's, starts a run of its own (readLimitOf).
type readLimit struct {
	first, limit time.Duration
}

// readLimitKey is the context key of the run of reads withReadLimit puts in a context.
type readLimitKey struct{}

// withReadLimit is ctx carrying a new run of reads, starting at ctx's first read limit
// (firstReadLimitOf). Each call makes a new run, so two calls under one parent context never share.
func withReadLimit(ctx context.Context) context.Context {
	first := firstReadLimitOf(ctx)
	return context.WithValue(ctx, readLimitKey{}, &readLimit{first: first, limit: first})
}

// readLimitOf is the run of reads ctx carries (withReadLimit), or a new one when it carries none.
func readLimitOf(ctx context.Context) *readLimit {
	if run, ok := ctx.Value(readLimitKey{}).(*readLimit); ok {
		return run
	}
	first := firstReadLimitOf(ctx)
	return &readLimit{first: first, limit: first}
}

// lost records a read the server left unanswered within the limit: the next one waits twice as
// long, up to RequestTimeout.
func (r *readLimit) lost() {
	r.limit = min(2*r.limit, RequestTimeout)
}

// answered records a read the server answered in took: the next one waits three times that, never
// less than the first limit and never more than RequestTimeout.
func (r *readLimit) answered(took time.Duration) {
	r.limit = min(max(r.first, 3*took), RequestTimeout)
}

// limitedRead is one send of read, a read of a stream's state, waited for up to limit's limit and
// then abandoned, its answer dropped (Bounded), and it sets limit for the next read of the run
// from what this one came to: a read the server left unanswered (lost) or one it answered
// (answered), an error of the server's included. answered is false when the limit or ctx passed
// first, and wait is the limit the read had, so a caller's error names it. Its callers are the
// reads a start makes of one request, sent again while unanswered (readAgain), and the reads of a
// wait on a stream (awaitStream).
func limitedRead[T any](ctx context.Context, limit *readLimit, read func() (T, error)) (value T, wait time.Duration, answered bool, err error) {
	wait = limit.limit
	readCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	sent := time.Now()
	value, answered, err = Bounded(readCtx, read)
	if !answered || unanswered(err) {
		limit.lost()
	} else {
		limit.answered(time.Since(sent))
	}
	return value, wait, answered, err
}

// readAgain is sentAgain for read, a read of a stream's state, each send of which is waited for
// up to its read limit (limitedRead) rather than up to the caller's whole wait: a read past its
// limit is read again, as an unanswered one is. A create keeps the caller's wait (sentAgain): its
// first send is answered only at the stream's first leader election, seconds after it is sent.
// Its callers are the reads a start makes: EnsureKeyValue's lookup, its read of a found bucket's
// stream and its handle open (wrapKeyValue), the stream ensure's read (ensureStreamWithConfig),
// SetKeyValueMaxAge's read and updateStream's read after an update that drew no answer; and the
// keeper's first read of the stream (keepStream) and of each bucket it raises (raiseReplicas).
func readAgain[T any](ctx context.Context, logger *slog.Logger, request string, limit *readLimit, read func() (T, error)) (T, error) {
	return sentAgain(ctx, logger, request, func() (T, error) {
		value, wait, answered, err := limitedRead(ctx, limit, read)
		if !answered {
			err = fmt.Errorf("no answer within %s: %w", wait, context.DeadlineExceeded)
		}
		return value, err
	})
}
