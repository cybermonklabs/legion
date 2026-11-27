package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The receive loop's timing.
//
// A term that must settle (leaseStatus.Settle: the lease was taken over an expired record nobody
// released, its holder killed or its node lost, or this pod resumed after its renewals lapsed, or
// from a record that says a pod of this build may still hold messages or a push binding) subscribes
// at once, beside any push binding, and holds every message it receives, keeping them alive with
// InProgress, until every message the durable has outstanding is in its hands: it reads the
// durable every settlePoll and ends the hold once NumAckPending is no more than the messages it
// holds, or at its cap, counted from the subscribe. Then it hands the held batch to the serial
// handler in stream order before it goes on live. The cap is what the previous holder can still
// have outstanding then: consumerAckWait and settleMargin, 7 s, since the messages a dead holder
// had received come back one ack wait after its last InProgress, and the ones the server pushed
// to a lost node's connection alone before this pod subscribed one ack wait after that push, so
// all are back one ack wait after the subscribe, in the held batch beside the newer ones, and the
// sort puts them back before them: a session keeps the order of its events across a hard
// take-over. A term taken from a clean hand-back ends its hold sooner, at handBackDelay and
// settleMargin, 3 s: the stopping holder NAKed everything it held with handBackDelay before its
// release, and NAKed what reached it with the same delay until it read this pod receiving, so
// nothing of it went to a lost node's binding, or to no subscriber at all, alone. That is not
// read from the lease record or from any clock: the stopping pod writes a mark for the record it
// released once its hand-over went through, and the hold reads that mark every settlePoll and
// ends at the later of the mark and 3 s after the subscribe (lease.HandBackMarked). A mark that
// is not there, or that cannot be read, holds the term to the 7 s cap, which is what a message
// that reached no subscriber of this machine's needs. With nothing outstanding the hold ends at
// its first read. A message a handler NAKed with a longer delay (a retry) stays outstanding until
// its delay ends, so the hold runs to its cap, or to its mark, and the retry arrives after it, as
// it would have on the previous holder.
//
// A pod that takes the lease from an absent or released record with neither stale_binding nor
// unsettled and finds the durable push-bound finds a listener build from before the lease bound to
// it with nats.Bind, during the first roll onto this build: a released holder has already
// unsubscribed, and an absent record means no holder of this build. It polls the durable every
// durableBindPoll, renewing its lease meanwhile, and subscribes once the binding is gone. Waiting
// past durableTakeoverAlarmAfter, it logs durableTakeoverAlarmLine at ERROR, which the deployed
// alarm matches, and again every durableTakeoverAlarmEvery; it never exits for this, since a
// restart would wait the same.
//
// A holder that cannot read, create, correct or subscribe to the durable retries every
// durableBindPoll and times its failures from the start of the first since the term began or an
// attempt last succeeded (durableReceiver.failed). Once they have lasted durableTakeoverAlarmAfter
// it logs durableTakeoverAlarmLine at ERROR with the failure as its reason and gives the lease up
// (lease.GiveUp), so a standby takes it at its next poll and tries in its place; this pod goes back
// to standby and takes the lease again, after two standby polls, only when no other pod has. A
// listener without a lease has no one to give it to: it keeps retrying and logs the line again
// every durableTakeoverAlarmEvery.
//
// A read, create or correction of the durable that the listener's NATS grant refuses
// (bus.ErrPublishDenied, answered as the server's refusal arrives) is retried the same way and
// never ends the process: each retry logs durableGrantRefusedLine at ERROR, naming the refused
// subject and the server's own text, and the retries back off from durableBindPoll, doubling, to
// durableRefusedRetryCap. The holder keeps the lease through it and its alarm line goes on every
// durableTakeoverAlarmEvery, since every pod of the machine connects as the same NATS user and
// would be refused the same way, and /healthz answers 503 for as long as the refusal lasts
// (receiveReport.Refused). An exit would send each pod in turn to the lease, to the refusal and to
// a crash loop whose backoff delays the recovery by minutes once the grant is fixed; a pod that
// stays up recovers at its next retry.
//
// A holder whose receive connection goes down keeps none of that: the lease reads the signal
// this loop gives it (durableReceiver.receiveUp) and, once the connection has been down for
// leaseReceiveLost, releases the record, so a standby takes it while this pod's own connection
// to the lease bucket is still renewing (lease.releaseReceiveLost). The term then ends as any
// lost lease's does - the subscription on the dead connection is dropped and what is queued is
// left to come back at its ack wait (durableReceiver.endLost) - since no hand-back can be NAKed
// over a connection that is down. This pod takes no lease back while the connection stays down,
// and stands by as any other pod once it is up.
//
// A listener whose NATS user lacked the grant for the listener state bucket at its start receives
// without a lease, the machine's one deliverer, and tries the bucket again until a grant lets it
// open it (joinDeliveryLease). Its lease's first rounds then take the record or read another pod's
// (lease.Contend), and the receive loop hands the term it receives on to that lease
// (durableReceiver.joinTerm). A lease that holds takes the term as it stands - its subscription,
// its queue and the message in hand - so no message waits for the switch (durableReceiver.adopt).
// A lease another pod holds, one that took it while this pod delivered without it and waits out
// this pod's binding as a pre-lease build's (awaitUnbound), ends the term: from that instant
// nothing it takes is forwarded or acked, its relays stop and get relayFormerSettle to settle the
// frames they forwarded, and then its subscription goes, which ends that holder's wait at its next
// poll. Either way the pod goes on under the lease, as the holder or a standby.
//
// The holder sends InProgress every inProgressEvery for every message it has received and not
// settled, queued or in hand, so none waits out the ack wait while this pod is alive. On a
// shutdown the message in hand gets inflightGrace to finish before its handling context is
// cancelled, which aborts a delivery in flight; inflightCancelWait bounds the wait for the
// handler to return after that.
//
// A holder's graceful stop hands its messages back (durableReceiver.handBack) and keeps its
// subscription until the next holder has one, since a push consumer's last subscriber going while
// a first delivery of a message is still in the server's send queue costs that message: the
// consumer counts the delivery undone below zero, and on nats-server before v2.15.0 the message
// then leaves the pending list at its ack wait and is never redelivered, lost for good, while on
// v2.15.0 or later, which this listener requires, it costs a whole ack wait, since the server
// redelivers it only then. It NAKs what it holds with handBackDelay, so the durable
// redelivers it once the next holder has subscribed, releases the lease unsettled, so the next
// holder settles, and then lingers: it NAKs, with the same delay, each message that reaches it, a
// repeat of one it already handed back included (handBackNaksMax NAKs of one message at most), and
// reads the lease record every lingerPoll until it names another holder that is receiving, or
// until lingerMax after the batch was NAKed. A record no other pod has taken ends the linger too:
// a running standby takes a released record at its next poll, so there is none to wait for. No end
// but a receiving holder comes before lingerUntaken and a poll have passed since the release, so
// a standby that is still taking the record has the deliver subject held for it. Then it drains
// its subscription, waiting handBackDrainWait at most for the server's word that it sends no
// more, and writes the lease's clean-hand-back mark for the record it released when all of this
// went through: its own release wrote that record, the term had no lost node's binding open, the
// message in hand's handler returned, the linger ended with another pod receiving, and the server
// confirmed the drain. The next holder's settle hold reads that mark and ends at 3 s rather than
// 7 s (lease.MarkHandBack, durableReceiver.handBack); a stop that falls short of any of it writes
// nothing and leaves the next holder the 7 s
// hold. handBackReleaseWait bounds the wait for the lease's loop to stop before the release, and
// listenerStateRequestWait the mark's write.
// The 3 s grace, its cancel's 1 s, lingerMax (the flush and the release inside it), the 2 s drain
// and the mark's 2 s write come to 11 s at most besides a lease call that hangs; a flush and a
// release that both run to their bounds (1 s and 2 s) leave the linger holding its floor past
// that cap, 1.25 s after the release, and 12.25 s in all. The receive stop does not wait for
// this pod's session relays: the term's end only cancels them (sessionRelays.setOwned), and the
// shutdown waits for their stopped frames to settle afterwards, up to relaySettleGrace (0.75 s),
// in its own step (shutdownListener). The 10 s the shutdown gives the receive stop, the relays
// and the HTTP stop together bounds none of that: it is how long the shutdown waits for the
// receive stop. Past it the shutdown goes on to the relays and the HTTP stop with no time left,
// then to the NATS drains and os.Exit(1), while the hand-back may still be running; client.Drain
// ends the subscription of a hand-back still lingering or draining, and os.Exit(1) ends only a
// mark write still in flight, and the next holder then settles at its 7 s cap, as it does after
// any stop that wrote no mark. The record a stop releases
// carries the ack wait its term still owes (lease.ackWaitOwed): a term that must settle owes one
// until its settle hold ends, so a stop before that leaves the next holder's hold to wait an ack
// wait. A release the lease's loop outlasts records that debt before it gives up (lease.Release),
// and the stop writes no mark: the linger before it read an unreleased record, ended "not taken"
// and let go of the deliver subject, so nothing vouches for the release that lands after it.
const (
	durableTakeoverAlarmLine  = "lease holder cannot deliver from the durable"
	durableTakeoverAlarmAfter = 135 * time.Second
	durableTakeoverAlarmEvery = 5 * time.Minute
	durableBindPoll           = time.Second
	inProgressEvery           = 2 * time.Second
	inflightGrace             = 3 * time.Second
	inflightCancelWait        = time.Second
	// durableGrantRefusedLine is the ERROR a term logs at each retry of a durable request the
	// NATS grant refused (durableReceiver.prepare), with the refused subject and the server's text.
	durableGrantRefusedLine = "durable consumer refused by the NATS grant; retrying"
	// durableRefusedRetryCap is the longest wait between two tries of a request the NATS grant
	// refused, a durable's or a start step's (waitOutGrantRefusal), of a start's request the
	// server answered from its cluster's state (waitOutClusterState), and of a start's connection
	// the server refused (openStartConnection): the waits start at bindPoll and double up to it
	// (nextRefusedWait). 30 s writes at most two of those ERROR lines a minute once the cap is
	// reached, and a grant fixed or a member back meanwhile is used within 30 s.
	durableRefusedRetryCap = 30 * time.Second
	// holderLoopsStopWait bounds how long the end of a term waits for a pass of the holder-only
	// loops in flight, measured from when they were told to stop.
	holderLoopsStopWait = 5 * time.Second
	// settlePoll is how often a settle hold reads the durable's outstanding messages.
	settlePoll = 250 * time.Millisecond
	// settleMargin is how much longer a settle hold lasts at most than the wait for the messages
	// the previous holder can still have outstanding (one ack wait after a hard stop, the
	// hand-back delay after a graceful one): it covers a redelivery the server sends a moment
	// after that wait runs out, and the stopping pod's lingerPoll and a round trip after a
	// graceful one.
	settleMargin = time.Second
	// handBackDelay is the NAK delay of every message a graceful stop hands back: long enough for
	// a standby to take the released lease and subscribe, so the redelivery reaches it.
	handBackDelay = 2 * time.Second
	// handBackNaksMax is how many times a stopping pod NAKs one message with handBackDelay: the
	// hand-back and two repeats, which reach it alone when the next holder has not subscribed yet.
	// A repeat past it is dropped and comes back at its ack wait. Each NAK is one more delivery
	// against the durable's MaxDeliver of 20.
	handBackNaksMax = 3
	// lingerPoll is how often a pod that stopped gracefully reads the lease record for the next
	// holder's receiving, and lingerMax the longest it keeps its subscription after it NAKed what
	// it held. lingerUntaken, two standby polls, is how long after its release it waits for any
	// other pod to take the lease at all, and so, with a poll on top, the floor of any linger that
	// no receiving holder ends: a standby that is taking the record then still finds the deliver
	// subject held for it.
	lingerPoll    = 250 * time.Millisecond
	lingerMax     = 3 * time.Second
	lingerUntaken = 2 * leaseStandbyPoll
	// handBackDrainWait bounds the drain of a stopping pod's subscription: past a slow link's
	// latency, since what the server pushed before the drain must still reach the pod.
	// handBackReleaseWait bounds its wait for the lease's loop to stop before the release.
	handBackDrainWait   = 2 * time.Second
	handBackReleaseWait = 2 * time.Second
	// receiveQueueCapacity is the local queue's: the durable sends no more than MaxAckPending
	// messages the holder has not settled, so the queue never fills; twice that leaves room for a
	// redelivery that lands as a NAK is being processed.
	receiveQueueCapacity = 2 * consumerMaxAckPending
)

// listenerLeaseUnavailable is what /healthz says about a listener whose NATS user may not use the
// listener state bucket. It then delivers without a lease, as the machine's one deliverer,
// receiving only while no other subscriber holds the durable, and tries the bucket again with the
// start rule's wait for a refusal until a grant lets it open it (joinDeliveryLease): from then on
// it is in the lease protocol, as the holder or a standby (durableReceiver.joinTerm), and /healthz
// says this no more. It says this of an edge listener for as long as that listener runs: the
// edge's grant never holds the bucket (natsgrant.Edge), so it opens no lease, never tries the
// bucket and never joins one.
const listenerLeaseUnavailable = "unavailable: missing NATS grant for KV bucket " + bus.ListenerStateBucket

// receiveConfig carries the receive loop's timing, so a test of this package shortens it;
// productionReceiveConfig is the only value the listener runs with.
type receiveConfig struct {
	// settlePoll is how often a settle hold reads the durable, and the hand-back's mark with it;
	// settleCap is the longest any hold lasts, and handBackSettleCap the soonest one ends on that
	// mark (lease.HandBackMarked).
	settlePoll        time.Duration
	settleCap         time.Duration
	handBackSettleCap time.Duration
	bindPoll          time.Duration
	alarmAfter        time.Duration
	alarmEvery        time.Duration
	inProgressEvery   time.Duration
	inflightGrace     time.Duration
	// handBackDelay, lingerPoll, lingerMax and lingerUntaken time a graceful stop's hand-back.
	handBackDelay time.Duration
	lingerPoll    time.Duration
	lingerMax     time.Duration
	lingerUntaken time.Duration
}

func productionReceiveConfig() receiveConfig {
	return receiveConfig{
		settlePoll:        settlePoll,
		settleCap:         consumerAckWait + settleMargin,
		handBackSettleCap: handBackDelay + settleMargin,
		bindPoll:          durableBindPoll,
		alarmAfter:        durableTakeoverAlarmAfter,
		alarmEvery:        durableTakeoverAlarmEvery,
		inProgressEvery:   inProgressEvery,
		inflightGrace:     inflightGrace,
		handBackDelay:     handBackDelay,
		lingerPoll:        lingerPoll,
		lingerMax:         lingerMax,
		lingerUntaken:     lingerUntaken,
	}
}

// receiveState is where the receive loop stands.
type receiveState int

const (
	// receiveStandby: this pod does not hold the lease (or, without one, has not started).
	receiveStandby receiveState = iota
	// receivePreparing: it holds the lease and is reading, creating or correcting the durable.
	receivePreparing
	// receiveAwaitingBinding: the durable is push-bound by a subscriber without the lease.
	receiveAwaitingBinding
	// receiveActive: it is subscribed to the durable's deliver subject and delivering.
	receiveActive
)

func (s receiveState) String() string {
	switch s {
	case receivePreparing:
		return "preparing the durable"
	case receiveAwaitingBinding:
		return "waiting for another subscriber's push binding to end"
	case receiveActive:
		return "receiving"
	default:
		return "standby"
	}
}

// receiveReport is the receive loop at one instant, as /healthz reads it.
type receiveReport struct {
	State receiveState
	// Subject is the deliver subject the active subscription is on, and SubscriptionValid whether
	// that subscription is valid on the client's current connection; both are zero unless State
	// is receiveActive.
	Subject           string
	SubscriptionValid bool
	// NoLease is set when the listener runs without a lease (listenerLeaseUnavailable), and Lease
	// is the lease's status otherwise.
	NoLease bool
	Lease   leaseStatus
	// Deliverer is whether this pod is the machine's deliverer as far as it knows, from the one
	// snapshot of the lease the report takes (durableReceiver.report): it holds the lease or runs
	// without one, the lease record names this pod's own instance though it is not holding (its
	// renewals lapsed and no other pod has taken the record yet), or the record names no holder
	// at all as this pod last read it. With State other than receiveActive it says that nothing
	// of the durable reaches a session and this pod knows of no other pod that would deliver it,
	// which no answer of /healthz may call healthy.
	Deliverer bool
	// Failing is how long the term has failed to read, create, correct or subscribe to the
	// durable (durableReceiver.failed), zero while it is not failing; FailingReason is the latest
	// failure, Stuck says the failures have lasted the alarm bound (alarmAfter), and Refused says
	// the latest failure is the NATS grant's refusal (bus.ErrPublishDenied), which /healthz answers
	// 503 for while it lasts.
	Failing       time.Duration
	FailingReason string
	Stuck         bool
	Refused       bool
}

// durableReceiverOptions is what a durableReceiver is made of.
type durableReceiverOptions struct {
	client   *bus.Client
	consumer string
	// lease is this pod's delivery lease when its start opened the listener state bucket; nil runs
	// without one (listenerLeaseUnavailable) until a lease joins (durableReceiver.join).
	lease *lease
	// handle delivers one message and settles it.
	handle durableHandler
	// loops starts the loops only the deliverer runs, under ctx, and returns a channel per loop
	// that closes once it has returned; nil starts none.
	loops func(ctx context.Context) []<-chan struct{}
	// relays are this listener's session-consumer relays, which the term elects: a term that
	// begins has this pod relay its machine's sessions, and its end has it stop
	// (sessionRelays.setOwned). Among the pods of one machine id only the one receiving relays,
	// so this is the one writer of that flag. Nil for a listener that relays nothing, and in a
	// rig that has no relays.
	relays *sessionRelays
	// refused is called once the durable is refused (errListenerDurableRefused); the receive loop
	// ends after it.
	refused func(error)
	logger  *logging.Logger
	timing  receiveConfig
}

// durableReceiver receives the machine's durable while this pod holds the delivery lease: it
// subscribes to the durable's deliver subject with a core subscription (never nats.Bind, never
// through the bus client's JetStream subscription slot), hands the messages one at a time to the
// delivery handler, keeps every unsettled one alive with InProgress, and unsubscribes when the
// lease is lost or released. Without a lease it receives whenever no other subscriber holds the
// durable, until a lease joins (join).
type durableReceiver struct {
	durableReceiverOptions

	// lease is this pod's delivery lease, nil while it runs without one (listenerLeaseUnavailable):
	// the option's at the start, and set once more, by the receive loop alone, when a lease joins a
	// pod that started without one (joinTerm, run), from which instant holding is the lease's. It
	// is an atomic because the handler, /healthz, the self-health monitor and the shutdown read it
	// on goroutines of their own.
	lease atomic.Pointer[lease]
	// joined is the lease a pod that started without one was handed once its grant let it open the
	// listener state bucket (join), and joinCtx ends when it is handed: the term this pod serves
	// without a lease stops preparing then (serve), or, once it receives, is handed to that lease
	// (receive, joinTerm).
	joined     atomic.Pointer[lease]
	joinCtx    context.Context
	joinCancel context.CancelFunc
	// stopJoining ends the try to join the machine's lease that startJoin runs, and joinTried is
	// closed once that try has returned; both are nil, under mu, for a pod that runs no such try
	// (one that started with a lease, an edge listener). Stop ends the try and waits for it before
	// it stops the receive loop, so a lease the try took as the pod stopped is handed over or
	// released by that stop, never left renewing past it.
	stopJoining context.CancelFunc
	joinTried   chan struct{}
	// adopted is the term of the joined lease that took over the subscription this pod received on
	// without a lease (joinTerm), the zero status while none did. Only the receive loop reads or
	// writes it.
	adopted leaseStatus
	// reconnects signals that the client's connection came back or was replaced (reconnected).
	reconnects chan struct{}
	// clientConn is the main client's connection as the receive loop last heard of it: the one
	// the constructor was handed, and the one each reconnect hook reports (reconnected). It is
	// what the loop subscribes a term on (begin, resubscribe) and judges the term's subscription
	// stale against (repair, report). It is an atomic because the reconnect hook writes it
	// without r.mu: the hook must wait on no lock of the receive loop's, which begin holds
	// across the subscribe that waits for nats.go's connection lock, which a reconnect dial a
	// route dropping this pod's packets leaves hanging holds for the connect timeout. The
	// lease's signal reads the bus client's own connection instead, so a hook that never runs
	// cannot leave this copy and the term agreeing on a connection the client has replaced
	// (receiveUp).
	clientConn atomic.Pointer[nats.Conn]
	// subscribed is the term being served as the signal reads it, nil while no term is receiving
	// (termSubscription). The receive loop stores it whole: a term's successful subscribe
	// (begin, resubscribe), a resubscribe that left the term without one, and the term's end.
	subscribed atomic.Pointer[termSubscription]
	// awaitedBinding is set, by the receive loop alone, while the term it serves has waited out a
	// push binding and not yet subscribed, and cleared at the start of every term that is not this
	// pod's own resumed one (serve), so it says only this: the term this one resumes was waiting
	// out a pre-lease build's binding. A term the lease reports resumed (leaseStatus.Resumed) is
	// this pod's own record after its renewals lapsed, which it counts as taken over an expired
	// record; with the flag set, the binding it was waiting out is still the pre-lease build's,
	// not a dead holder's, so it is waited out again rather than received beside, and the term
	// does not settle. Any other acquisition is another pod's record, dead or handed back, and
	// settles as that record says.
	awaitedBinding bool

	mu    sync.Mutex
	state receiveState
	term  *receiveTerm
	stop  context.CancelFunc
	done  chan struct{}
	// failingSince is when the first of the term's failed attempts on the durable began, zero
	// while there is none since the term began or an attempt last succeeded; failingReason is the
	// latest failure, failingRefused whether that failure is the NATS grant's refusal, and
	// failAlarm paces their alarm (durableReceiver.failed).
	failingSince   time.Time
	failingReason  string
	failingRefused bool
	failAlarm      alarmThrottle
}

func newDurableReceiver(options durableReceiverOptions) *durableReceiver {
	r := &durableReceiver{
		durableReceiverOptions: options,
		reconnects:             make(chan struct{}, 1),
	}
	r.lease.Store(options.lease)
	// The receiver's own field is the lease from here on; the option's copy would only go stale.
	r.durableReceiverOptions.lease = nil
	r.joinCtx, r.joinCancel = context.WithCancel(context.Background())
	r.clientConn.Store(options.client.Conn)
	return r
}

// join hands this pod, which started without a lease, the lease it joined once its NATS grant let
// it open the listener state bucket (joinDeliveryLease, main.go): the lease's first rounds have
// taken the record or read another pod's (lease.Contend) and its loop runs. The receive loop
// takes it from there (run, joinTerm). It is called once, by the try startJoin runs.
func (r *durableReceiver) join(l *lease) {
	r.joined.Store(l)
	r.joinCancel()
}

// startJoin runs try, the join of its machine's lease that a pod started without one makes
// (joinDeliveryLease and the steps main.go takes after it), on a goroutine of its own and under a
// context of its own, which Stop ends, and hands the receive loop the lease try returns (join). try
// returns nil when it ends with no lease. It returns once its context has ended, from a wait
// between two of its tries too (joinDeliveryLease waits each through waitUnlessDone), and after
// at most one round of the lease it made from then on: lease.Contend reads its context only
// between two rounds, so the round in flight as the context ends, or the first round when the
// context ended before any began, runs to its end and may take the record. Stop then releases a
// lease it took, and waits for the try within the shutdown's budget. It is called once, after
// Start.
func (r *durableReceiver) startJoin(try func(ctx context.Context) *lease) {
	ctx, stop := context.WithCancel(context.Background())
	tried := make(chan struct{})
	r.mu.Lock()
	r.stopJoining, r.joinTried = stop, tried
	r.mu.Unlock()
	go func() {
		defer close(tried)
		defer stop()
		if l := try(ctx); l != nil {
			r.join(l)
		}
	}()
}

// Start runs the receive loop until Stop. Start is called once; the lease, when there is one, is
// started by the caller.
func (r *durableReceiver) Start() {
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	r.stop, r.done = stop, done
	r.mu.Unlock()
	go func() {
		defer close(done)
		r.run(ctx)
	}()
}

// Stop ends receiving in the shutdown's order: the holder-only loops are told to stop, the
// handler takes no more messages, the message in hand gets inflightGrace before its handling is
// cancelled, and a holder hands every message it has not handled back to the next one, releases
// the lease, so a standby takes it at its next poll, and keeps its subscription until the next
// holder receives (durableReceiver.handBack). It returns once that is done or ctx ends; a lease
// left unreleased at ctx's end is taken as an expired one. A loop that has already returned is read
// again at ctx's end (awaitDone, closedWithin), which a hand-back that ran to its bounds can have
// ended first, so a stop that did everything it owed does not report itself unfinished. The release
// here is the one that writes the record when the hand-back's own release outlasted its bound, and
// it writes the ack-wait debt that release recorded (lease.Release).
//
// A pod still trying to join its machine's lease (startJoin) has that try ended first, and Stop
// waits for it to return, within ctx, before it stops the receive loop: a join that completes
// after the stop began would otherwise take the record and renew it until the process exits,
// which leaves a standby to wait it out as an expired one. The try returns once its context ends,
// from a wait between two of its tries too, so no such wait spends ctx, and after at most the
// round of the lease it made that is in flight then, or that lease's first round when none had
// begun (startJoin, lease.Contend). A lease the try handed over before it returned is
// taken up by the receive loop, whose term then ends as any term of the lease does, or, when the
// loop did not take it up (join), is released here all the same.
func (r *durableReceiver) Stop(ctx context.Context) {
	r.mu.Lock()
	stop, done, stopJoining, joinTried := r.stop, r.done, r.stopJoining, r.joinTried
	r.mu.Unlock()
	if stopJoining != nil {
		stopJoining()
		if awaitDone(ctx, joinTried) != nil {
			r.logger.Warn("listener lease join still running at shutdown; a lease it takes after this is left to expire")
		}
	}
	if stop != nil {
		stop()
		if awaitDone(ctx, done) != nil {
			r.logger.Warn("durable receive loop still settling at shutdown")
		}
	}
	l := r.lease.Load()
	if l == nil {
		l = r.joined.Load()
	}
	if l == nil {
		return
	}
	if err := l.Release(ctx); err != nil {
		r.logger.Warn("listener lease not released; a standby takes it once it expires", slog.String("error", err.Error()))
	}
}

// reconnected is the client's reconnect hook: conn is the client's connection after a reconnect
// in place or a replacement, which the receive loop moves the subscription onto when it is no
// longer valid there. It never fails, so it never holds up the client's recovery, and it takes
// no lock of the receive loop's, so a hook that lands while a subscribe is in flight waits for
// nothing (clientConn). It makes no request, so the run's context it is handed bounds nothing.
func (r *durableReceiver) reconnected(_ context.Context, conn *nats.Conn) error {
	r.clientConn.Store(conn)
	select {
	case r.reconnects <- struct{}{}:
	default:
	}
	return nil
}

// holding reports whether this pod may deliver now: it holds the lease, or runs without one. A
// lease that joined and found another pod holding the record makes it false from the instant the
// receive loop stores it (joinTerm), so nothing the handler takes from then on is delivered.
func (r *durableReceiver) holding() bool {
	l := r.lease.Load()
	return l == nil || l.Holding()
}

// termSubscription is the term being served as the lease's signal reads it: conn is the
// connection its subscription was made on, and nil when the term holds none at all, which a
// resubscribe that failed leaves. It is stored whole, under one atomic, so a reader takes no
// lock for it (durableReceiver.subscribed).
type termSubscription struct {
	conn *nats.Conn
}

// receiveUp is the signal the delivery lease judges this pod's receiving by
// (lease.SetReceiveSignal): the lease holder releases the record once it has been down for
// leaseReceiveLost, and no pod takes the record while it is down. It is the state /healthz
// reports as an inactive subscription on the active pod, read here rather than there: the
// client has no connected connection, or the term is receiving and its subscription is not on
// that connection, which a connection the client replaced leaves until the resubscribe (report,
// repair). A holder that is still preparing - reading the durable, waiting out a pre-lease
// build's binding, settling before it has subscribed - is receiving as far as this goes, as
// /healthz counts it a standby rather than an inactive subscription: its connection is up and
// its term is on its way.
//
// The term's connection is compared with the bus client's own installed, connected one
// (bus.Client.ConnectedConn), never with what the receive loop last heard: a run of the
// reconnect hooks stops at the first hook that fails (bus.Client.AddReconnectHook) and main
// registers the KV rewatch hook ahead of this loop's (durableReceiver.reconnected), so a rewatch
// that keeps failing leaves this loop's own copy of the connection naming the one the client
// replaced - the same closed connection the term is subscribed on, which compared with each
// other read as a pod that receives and delivered nothing. A reconnect in place keeps the
// connection, and nats.go re-sends its subscriptions, so it reads up throughout, hooks and all.
//
// The receive loop's own copy of the connection (clientConn) must name that installed one too,
// with or without a term: the loop subscribes on its copy (begin), so a pod whose copy still
// names a connection the client replaced cannot receive whatever term it starts, and a standby in
// that state must not take the lease (lease.poll), only to fail every subscribe until it gives
// the lease up again.
//
// It loads atomics and nothing else. The lease reads it from its own loop, the one that renews
// the record and releases it when this pod stops receiving, so it may wait on no lock a call
// into nats.go can hold: nats.go holds its connection's lock across every reconnect dial and the
// connect init after it, which a route that drops this pod's packets leaves hanging for the
// connect timeout, and durableReceiver.begin holds the receive loop's lock across the subscribe
// that waits for that same lock. The receive loop keeps its atomics current from what it already
// learns: each reconnect (reconnected) and each term's subscribe and end (begin, resubscribe, end).
func (r *durableReceiver) receiveUp() bool {
	connected := r.client.ConnectedConn()
	if connected == nil || r.clientConn.Load() != connected {
		return false
	}
	subscribed := r.subscribed.Load()
	return subscribed == nil || subscribed.conn == connected
}

func (r *durableReceiver) setState(state receiveState) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

// standbyUnlessReceiving sets the loop's state back to standby when a serve returns, unless a term
// still receives: one a lease that joined took over as it stood (joinTerm), which its lease's term
// goes on serving (adopt). Every other serve returns with no term, having never begun one or
// having ended it (end).
func (r *durableReceiver) standbyUnlessReceiving() {
	r.mu.Lock()
	if r.term == nil {
		r.state = receiveStandby
	}
	r.mu.Unlock()
}

// report reads the receive loop now.
func (r *durableReceiver) report() receiveReport {
	l := r.lease.Load()
	r.mu.Lock()
	report := receiveReport{State: r.state, NoLease: l == nil}
	if t := r.term; t != nil {
		report.Subject = t.subject
		report.SubscriptionValid = t.sub != nil && t.sub.IsValid() && t.conn == r.clientConn.Load()
	}
	if !r.failingSince.IsZero() {
		report.Failing = time.Since(r.failingSince)
		report.FailingReason = r.failingReason
		report.Stuck = report.Failing >= r.timing.alarmAfter
		report.Refused = r.failingRefused
	}
	r.mu.Unlock()
	// One snapshot answers both questions the report asks of the lease, so the hold and the
	// record it names are the lease at one instant rather than two reads a change falls between.
	if l == nil {
		report.Deliverer = true
		return report
	}
	status := l.Status()
	report.Lease = status
	report.Deliverer = status.Holding ||
		status.ActiveInstance == "" || status.ActiveInstance == l.instance
	return report
}

// run is the receive loop. With a lease it serves each term of the lease this pod acquires, from
// the acquisition until the term ends. Without one it serves until ctx ends or a lease joins
// (join), and then goes on as a pod that started with that lease: the term it received on becomes
// the lease's term when the lease holds the record, with its subscription and its queue kept
// (adopt), and ends when another pod holds it (joinTerm).
func (r *durableReceiver) run(ctx context.Context) {
	var served uint64
	if r.lease.Load() == nil {
		refused := r.serve(ctx, ctx, leaseStatus{How: leaseTakenAbsent})
		// A term the joined lease took over (joinTerm) is served even once a shutdown has come:
		// the shutdown ends it at once, with the hand-back a holder's stop owes (end), where
		// returning here would leave its subscription open under a lease the stop then releases.
		if refused || (ctx.Err() != nil && !r.adopted.Holding) {
			return
		}
		// A lease joined. A term that ended before it received anything, still preparing or
		// waiting out a binding, had nothing to hand over, and the lease is stored here.
		r.lease.CompareAndSwap(nil, r.joined.Load())
		if r.adopted.Holding {
			served = r.adopted.Term
			if r.serveTerm(ctx, r.adopted, true) {
				return
			}
		}
	}
	l := r.lease.Load()
	for {
		if status := l.Status(); status.Holding && status.Term != served {
			served = status.Term
			if r.serveTerm(ctx, status, false) {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-l.Changes():
		case <-r.reconnects:
		}
	}
}

// serveTerm serves one term of the lease, under a context that ends when the term does (the lease
// is lost, or taken again as a new term) or ctx does: the term of a subscription it starts
// (serve), or, with adopt, of the subscription this pod already receives on, which a lease that
// joined took over (adopt). It reports whether the durable was refused.
func (r *durableReceiver) serveTerm(ctx context.Context, status leaseStatus, adopt bool) bool {
	l := r.lease.Load()
	termCtx, cancel := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-termCtx.Done():
				return
			case <-l.Changes():
				if now := l.Status(); !now.Holding || now.Term != status.Term {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-watched
	}()
	if adopt {
		return r.adopt(ctx, termCtx, status)
	}
	return r.serve(ctx, termCtx, status)
}

// serve receives the durable for one term, which termCtx bounds and status describes: it reads the
// durable, waits out a pre-lease build's binding when the term does not settle, corrects the
// durable's policy, subscribes, and delivers until termCtx ends; a term that settles subscribes at
// once, beside any binding, and holds what it receives until every outstanding message is in hand
// (receiveTerm.settle), for settleCap at most, or until the clean-hand-back mark of the stop it
// took the record from is there and handBackSettleCap has passed since the subscribe, then
// delivers that in stream order.
// ctx is the receive loop's own, whose end is a shutdown. It reports whether the durable was
// refused. A term whose reads, writes or subscribes of the durable fail for alarmAfter gives the
// lease up (abandon).
//
// Nothing about the hand-back is judged here: the hold is the ack wait's from the start, and the
// mark the stopping pod writes once its hand-over went through is what shortens it, read inside
// the hold (lease.HandBackMarked). So a term that is slow to subscribe - a long prepare, a
// durable that refuses a subscribe and is retried - or whose SUB the server never read holds for
// the 7 s cap by doing nothing: no mark reaches a term whose stopping pod never saw it receiving.
//
// Before it subscribes it records stale_binding in the lease: set when it subscribes beside a
// binding it did not wait out, cleared when it finds the durable not push-bound. A term taken
// from a hand-back candidate records it beside a binding as any other settling term does, and the
// mark is what clears it (receiveTerm.settle): a binding beside such a term is the stopping pod's
// own linger, since a binding the stopping pod had found beside its own would have left
// stale_binding on the record it released and made this term no candidate - but only the mark
// says that linger is what it was.
//
// A term the lease reports resumed (leaseStatus.Resumed) while this pod was waiting out a
// pre-lease build's binding (awaitedBinding) is this pod's own term again, with that binding still
// the pre-lease build's: it waits it out again rather than settling beside it. The flag is cleared
// at the start of every other term, so a term that ended any other way - the lease lost, the lease
// given up, a shutdown - leaves nothing behind for a later take-over over a dead holder's record.
//
// The policy is written only once nothing but this pod will receive: a pre-lease build that still
// holds the durable binds it again after a reconnect with nats.Bind, which refuses a durable whose
// filter is not its own ("subject does not match consumer").
func (r *durableReceiver) serve(ctx, termCtx context.Context, status leaseStatus) bool {
	defer r.standbyUnlessReceiving()
	defer r.succeeded()
	// The lease is read once: a lease that joins a pod serving without one is stored by this
	// goroutine alone, once this term has ended or handed itself over (joinTerm, run).
	l := r.lease.Load()
	// A term served without a lease stops preparing once a lease joins (join): its reads and
	// waits run under prepCtx, which ends with that. Its subscription and its relays, once it has
	// them, follow termCtx alone, so they go on until the join decides them (receive, joinTerm).
	prepCtx := termCtx
	if l == nil {
		var cancel context.CancelFunc
		prepCtx, cancel = context.WithCancel(termCtx)
		defer cancel()
		defer context.AfterFunc(r.joinCtx, cancel)()
	}
	start := time.Now()
	if !status.Resumed {
		r.awaitedBinding = false
	}
	// The flag stands only for a term this pod resumed, the line above having cleared it for
	// every other: a term that waited a pre-lease build's binding out and resumed waits it out
	// again rather than settling beside it.
	settle := status.Settle && !r.awaitedBinding
	r.setState(receivePreparing)
	info, err := r.prepare(prepCtx, false)
	if err != nil {
		return r.abandon(err, status)
	}
	r.succeeded()
	var hold time.Duration
	if settle {
		hold = r.timing.settleCap
		r.logger.Info("listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand",
			slog.String("consumer", r.consumer), slog.String("lease", status.How.String()), slog.String("cap", hold.String()),
			slog.Bool("hand_back", status.HandBack))
	} else {
		if status.Settle {
			l.Settled(status.Term)
		}
		if info.PushBound {
			if err = r.awaitUnbound(prepCtx); err != nil {
				return r.abandon(err, status)
			}
		}
	}
	r.setState(receivePreparing)
	beside := false
	for {
		attempt := time.Now()
		if info, err = r.prepare(prepCtx, true); err != nil {
			return r.abandon(err, status)
		}
		switch {
		case l == nil:
		case !info.PushBound:
			l.SetStaleBinding(status.Term, false)
		case settle:
			l.SetStaleBinding(status.Term, true)
			if !beside {
				beside = true
				r.logger.Info("durable still push-bound by an earlier lease holder; receiving beside it", slog.String("consumer", r.consumer))
			}
		}
		err := r.begin(termCtx, info.Config.DeliverSubject, hold, status.Term)
		if err == nil {
			r.awaitedBinding = false
			r.succeeded()
			break
		}
		r.logger.Warn("durable subscribe failed; retrying", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		if r.failed(attempt, fmt.Errorf("subscribe to the durable's deliver subject: %w", err)) {
			return r.abandon(errDurableStuck, status)
		}
		if waitUnlessDone(prepCtx, r.timing.bindPoll) != nil {
			return false
		}
	}
	taken := "unavailable"
	if l != nil {
		taken = status.How.String()
	}
	r.logger.Info("durable receiving",
		slog.String("consumer", r.consumer),
		slog.String("deliver_subject", info.Config.DeliverSubject),
		slog.String("lease", taken),
		slog.Int64("waited_ms", time.Since(start).Milliseconds()),
		slog.Bool("settle", settle),
	)
	return r.receive(ctx, termCtx)
}

// receive is the rest of a term once it receives: it runs the holder-only loops under the term,
// moves its subscription onto the client's connection after a reconnect (repair), and once termCtx
// ends stops the loops and ends the term (end), waiting holderLoopsStopWait at most for a pass in
// flight. A term this pod serves without a lease is handed instead to a lease that joins (join),
// the loops stopped the same way (joinTerm), unless termCtx has ended by then. It reports false: a
// refusal of the durable comes before it.
func (r *durableReceiver) receive(ctx, termCtx context.Context) bool {
	loopsCtx, stopLoops := context.WithCancel(context.Background())
	var loops []<-chan struct{}
	if r.loops != nil {
		loops = r.loops(loopsCtx)
	}
	// joined stays nil, and so never ready, for a term of the lease.
	var joined <-chan struct{}
	if r.lease.Load() == nil {
		joined = r.joinCtx.Done()
	}
	// finish stops the loops, ends the term by end, and waits for a pass of the loops in flight.
	finish := func(end func(context.Context)) bool {
		stopped := time.Now()
		stopLoops()
		end(ctx)
		waitLoops(r.logger, loops, stopped.Add(holderLoopsStopWait))
		return false
	}
	for {
		select {
		case <-termCtx.Done():
			return finish(r.end)
		case <-joined:
			// A stop that comes as the lease joins leaves both cases ready, and select picks
			// either. The stop has begun, so the term ends here, as the stop of a pod without a
			// lease ends it: it drains its subscription and NAKs what it holds (end). Stop then
			// releases the lease that joined.
			if termCtx.Err() != nil {
				return finish(r.end)
			}
			return finish(r.joinTerm)
		case <-r.reconnects:
			r.repair(termCtx)
		}
	}
}

// joinTerm hands the term this pod received on without a lease to the lease that joined (join),
// whose first rounds took the record or read another pod's (lease.Contend). It reads the lease
// once, and that read decides; the lease is stored as this pod's at once, and from that instant
// the handler delivers only while the lease holds (holding).
//
// A lease that holds the record takes the term over as it stands: the term's subscription, its
// queue and the message in hand stay, so nothing waits for the switch, and the term is the
// lease's from here (receiveTerm.leaseTerm), which the receive loop serves (run, adopt). The
// holder-only loops the term ran without a lease have been told to stop, and start again under
// the lease's term.
//
// A lease another pod holds - the pod that took the lease while this one delivered without it and
// waits for this pod's binding to end (awaitUnbound) - ends the term, as a lost lease's end does:
// from the store on, nothing the handler takes is forwarded or acked, so each such message comes
// back to the holder at its ack wait and is delivered there once, while the message in hand
// settles as normal, its ack included (endLost). The relays stop first and are given
// relayFormerSettle, the settle a former holder's successor waits out (lease.FormerSettleWait),
// before the durable's subscription goes: that subscription's end is what lets the holder start,
// and its relays restart each session's consumer at its ack floor (sessionRelays.restart), so the
// frames this pod's relays forwarded settle, their acks included, before that floor is read and
// none comes back as a repeat. This pod then stands by under the lease, as any pod that never
// held it.
func (r *durableReceiver) joinTerm(ctx context.Context) {
	l := r.joined.Load()
	status := l.Status()
	r.mu.Lock()
	if status.Holding {
		r.term.leaseTerm = status.Term
	}
	r.lease.Store(l)
	r.mu.Unlock()
	if status.Holding {
		r.adopted = status
		r.logger.Info(leaseJoinedHolderLine,
			slog.String("consumer", r.consumer), slog.String("lease", status.How.String()), slog.Uint64("term", status.Term))
		return
	}
	r.logger.Info(leaseJoinedStandbyLine, slog.String("consumer", r.consumer), slog.String("active_instance", status.ActiveInstance))
	r.relays.setOwned(nil)
	_ = waitUnlessDone(ctx, relayFormerSettle)
	r.end(ctx)
}

// The lines a pod that received without a lease logs as the lease it joined decides its term
// (joinTerm): it took the lease and keeps its subscription, or another pod holds the lease and it
// stands by.
const (
	leaseJoinedHolderLine  = "listener lease joined: this pod took it and goes on receiving the durable on its subscription"
	leaseJoinedStandbyLine = "listener lease joined: another pod holds it, so this pod stops delivering and stands by"
)

// adopt serves status's term on the subscription this pod received on without a lease, which the
// lease that joined took over (joinTerm): it subscribes nothing and drops nothing. It records in
// the lease what begin records for a term it subscribes, the deliver subject and the term
// receiving, and hands the relays this term, bounded by the lease's loss deadline
// (lease.UntilLoss), in place of the one without a lease they followed, so each relay goes on
// pulling with no restart. The holder-only loops start under the term (receive), as they do for
// any term. The term settles nothing: what the durable has outstanding was pushed to this very
// subscription, and a record that said its term must settle (unsettled, stale_binding) keeps
// saying so, so the pod after this one settles in its turn.
func (r *durableReceiver) adopt(ctx, termCtx context.Context, status leaseStatus) bool {
	l := r.lease.Load()
	r.mu.Lock()
	subject := r.term.subject
	r.mu.Unlock()
	l.SetDeliverSubject(subject)
	l.SetReceiving(status.Term)
	r.relays.setOwned(&relayTerm{ctx: termCtx, untilLoss: func() time.Duration { return l.UntilLoss(status.Term) }})
	return r.receive(ctx, termCtx)
}

// prepare reads the durable, creating it when it is missing and, when correct is set, correcting
// its policy when it has drifted (correctListenerDurable), retrying every bindPoll until it
// answers, and records its deliver subject in the lease. Each failure counts toward the term's
// alarm bound (failed). A failure that is the NATS grant's refusal (bus.ErrPublishDenied) is
// logged at ERROR at every retry, durableGrantRefusedLine with the refused subject and the
// server's text, and its retries wait bindPoll, then twice as long each time, up to
// durableRefusedRetryCap; any other failure is logged once, at WARN, and retried every bindPoll.
// It returns a refusal (errListenerDurableRefused), errDurableStuck once a holder's failures
// other than the grant's refusal have lasted that bound, or ctx's error.
func (r *durableReceiver) prepare(ctx context.Context, correct bool) (*nats.ConsumerInfo, error) {
	failing := false
	refusedWait := r.timing.bindPoll
	for {
		attempt := time.Now()
		step := "read"
		info, err := listenerDurable(ctx, r.client, r.consumer)
		if err == nil && (correct || info == nil) {
			step = "correct the policy of"
			if info == nil {
				step = "create"
			}
			info, err = correctListenerDurable(ctx, r.client, r.consumer, info)
		}
		if err == nil {
			if failing {
				r.logger.Info("durable consumer read recovered", slog.String("consumer", r.consumer))
			}
			if l := r.lease.Load(); l != nil {
				l.SetDeliverSubject(info.Config.DeliverSubject)
			}
			return info, nil
		}
		if errors.Is(err, errListenerDurableRefused) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		err = fmt.Errorf("%s the durable consumer: %w", step, err)
		wait := r.timing.bindPoll
		switch {
		case errors.Is(err, bus.ErrPublishDenied):
			wait, refusedWait = refusedWait, nextRefusedWait(refusedWait)
			r.logger.Error(durableGrantRefusedLine,
				slog.String("consumer", r.consumer),
				slog.String("subject", refusedSubject(err)),
				slog.String("error", err.Error()),
				slog.String("retry_in", wait.String()))
		case !failing:
			r.logger.Warn("durable consumer read failed; retrying", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		}
		failing = true
		if r.failed(attempt, err) {
			return nil, errDurableStuck
		}
		if err := waitUnlessDone(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// refusedSubject is the subject err, a request or a subscription the NATS grant refused
// (bus.ErrPublishDenied, nats.ErrPermissionViolation), was refused on, read from the server's
// violation it carries (grantViolation), and empty when it carries none.
func refusedSubject(err error) string {
	if match := grantViolation.FindStringSubmatch(err.Error()); match != nil {
		return match[1]
	}
	return ""
}

// nextRefusedWait is the wait before the next try of a request the NATS grant refused, or a start
// request the server answered from its cluster's state or refused, after a wait of previous: twice
// it, up to durableRefusedRetryCap. The durable's requests (prepare, from bindPoll) and the start's
// steps (waitOutGrantRefusal, waitOutClusterState and openStartConnection, from durableBindPoll)
// back off by it.
func nextRefusedWait(previous time.Duration) time.Duration {
	return min(2*previous, durableRefusedRetryCap)
}

// awaitUnbound waits, polling the durable every bindPoll without writing it, until no subscriber
// holds its push binding. Its wait is alarmed on by the same throttle as a term's failures
// (alarmThrottle, durableReceiver.failed), and it never gives up for the binding: only ctx's
// end, a refusal or reads that fail for alarmAfter (prepare) end it.
func (r *durableReceiver) awaitUnbound(ctx context.Context) error {
	r.setState(receiveAwaitingBinding)
	r.awaitedBinding = true
	r.logger.Info("durable is push-bound by a subscriber without the lease; waiting for its binding to end",
		slog.String("consumer", r.consumer))
	start := time.Now()
	var alarm alarmThrottle
	for {
		if err := waitUnlessDone(ctx, r.timing.bindPoll); err != nil {
			return err
		}
		info, err := r.prepare(ctx, false)
		if err != nil {
			return err
		}
		r.succeeded()
		waited := time.Since(start)
		if !info.PushBound {
			r.logger.Info("durable push binding ended", slog.String("consumer", r.consumer), slog.Int64("waited_ms", waited.Milliseconds()))
			return nil
		}
		if alarm.due(r.timing, time.Now(), waited) {
			r.alarm("another subscriber holds the durable's push binding", waited)
		}
	}
}

// begin starts a term's receiving: a core subscription on subject on the client's current
// connection, the serial handler and the InProgress ticker. A hold above zero has the handler
// first hold what arrives until the durable's outstanding messages are all in hand, or for that
// long at most, then deliver it in stream order (receiveTerm.settle); term is the lease term
// whose unsettled the hold's end clears. Once the server has the subscription, the lease records
// the term receiving, which a pod that stopped gracefully waits for before it lets go of the
// deliver subject - and, after that, before it writes the mark that shortens this term's hold. A
// flush that fails leaves the SUB to nats.go's reconnect, records no term receiving and so earns
// no mark: the hold runs to its 7 s cap, which is what the messages that reached no subscriber
// meanwhile need.
//
// The term elects this pod the relayer of its machine's session consumers (sessionRelays.setOwned,
// cleared by durableReceiver.end): gaining the durable starts the relay of every such session the
// cache holds for this machine, each restarted at its ack floor. termCtx is the term's context,
// and the relays take the term only while it is live, so a term that ended after its prepare and
// before this begin starts no relay. Each restart of a session's consumer is held to termCtx and
// to the lease's loss deadline for term (lease.UntilLoss, sessionRelays.termContext).
func (r *durableReceiver) begin(termCtx context.Context, subject string, hold time.Duration, term uint64) error {
	t := &receiveTerm{
		subject:   subject,
		queue:     make(chan *nats.Msg, receiveQueueCapacity),
		unsettled: map[*nats.Msg]struct{}{},
		closing:   make(chan struct{}),
		handled:   make(chan struct{}),
		stopped:   make(chan struct{}),
		logger:    r.logger,
		hold:      hold,
		leaseTerm: term,
		holding:   hold > 0,
	}
	handling, cancel := context.WithCancel(context.Background())
	t.handling, t.cancel = handling, cancel
	r.mu.Lock()
	conn := r.clientConn.Load()
	sub, err := conn.Subscribe(subject, t.intake)
	if err != nil {
		r.mu.Unlock()
		t.cancel()
		return err
	}
	t.sub, t.conn = sub, conn
	r.term, r.state = t, receiveActive
	r.subscribed.Store(&termSubscription{conn: conn})
	r.mu.Unlock()
	// The server counts the subscription once it has read it; a flush that fails, while the
	// connection is down, leaves it to nats.go to send again on the reconnect, and the term is
	// recorded receiving once a resubscribe on the new connection has flushed.
	flushed := conn.FlushTimeout(time.Second) == nil
	l := r.lease.Load()
	if flushed && l != nil {
		l.SetReceiving(term)
	}
	relaying := &relayTerm{ctx: termCtx}
	if l != nil {
		relaying.untilLoss = func() time.Duration { return l.UntilLoss(term) }
	}
	r.relays.setOwned(relaying)
	go t.handle(r)
	go t.keepAlive(r.timing.inProgressEvery)
	return nil
}

// repair moves the term's subscription onto the client's current connection when it is no longer
// valid there: the client replaced its connection, or the subscription ended.
func (r *durableReceiver) repair(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	stale := t != nil && (t.sub == nil || !t.sub.IsValid() || t.conn != r.clientConn.Load())
	r.mu.Unlock()
	if !stale {
		return
	}
	if err := r.resubscribe(ctx); err != nil {
		r.logger.Warn("durable resubscribe failed; the self-health monitor retries it", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
	}
}

// resubscribe moves the term's subscription onto the durable's deliver subject on the client's
// current connection, creating the durable through ensureListenerDurable when it is missing. A
// pod that is not receiving has nothing to move.
func (r *durableReceiver) resubscribe(ctx context.Context) error {
	r.mu.Lock()
	receiving := r.term != nil
	r.mu.Unlock()
	if !receiving {
		return nil
	}
	info, err := ensureListenerDurable(ctx, r.client, r.consumer)
	if err != nil {
		return fmt.Errorf("read the durable consumer: %w", err)
	}
	subject := info.Config.DeliverSubject
	r.mu.Lock()
	t := r.term
	if t == nil {
		r.mu.Unlock()
		return nil
	}
	if t.sub != nil {
		_ = t.sub.Unsubscribe()
	}
	conn := r.clientConn.Load()
	sub, err := conn.Subscribe(subject, t.intake)
	if err != nil {
		t.sub = nil
		r.subscribed.Store(&termSubscription{})
		r.mu.Unlock()
		return fmt.Errorf("subscribe to the durable's deliver subject: %w", err)
	}
	t.sub, t.conn, t.subject = sub, conn, subject
	r.subscribed.Store(&termSubscription{conn: conn})
	// The lease and the term it serves are read together, under the lock a lease that joins this
	// term's receiving takes to set both (joinTerm).
	l, term := r.lease.Load(), t.leaseTerm
	r.mu.Unlock()
	if l != nil {
		l.SetDeliverSubject(subject)
	}
	if conn.FlushTimeout(time.Second) == nil && l != nil {
		l.SetReceiving(term)
	}
	r.logger.Info("durable resubscribed", slog.String("consumer", r.consumer), slog.String("deliver_subject", subject))
	return nil
}

// probe is the self-health monitor's durable check, which only the receiving pod makes: a standby
// has no subscription to check, and probes healthy. On the receiving pod a consumer that is gone
// is nats.ErrConsumerNotFound, and one whose deliver subject is not the subscription's, or whose
// subscription is no longer valid on the client's connection, errDurableUnbound; both are
// terminal, so the monitor's rebuild resubscribes. A healthy durable whose policy has drifted is
// corrected, and one the policy refuses (listenerDurableRefusal) is reported as its refusal, which
// the monitor logs and does not rebuild. Any other read error is reported as success, deliberately:
// a JetStream deadline or a reconnect gap must not count as a terminal failure, since the monitor
// splits its counts on it. Each read runs under ctx, so the shutdown cuts one the server has not
// answered.
//
// A mismatch is confirmed by a second read, durableProbeConfirm later, since a read can land in
// the receive loop's own resubscribe.
func (r *durableReceiver) probe(ctx context.Context) error {
	for read := range 2 {
		if read > 0 && waitUnlessDone(ctx, durableProbeConfirm) != nil {
			return nil
		}
		if report := r.report(); report.State != receiveActive {
			return nil
		}
		info, err := listenerDurable(ctx, r.client, r.consumer)
		switch {
		case errors.Is(err, errListenerDurableRefused):
			return err
		case err != nil:
			return nil
		case info == nil:
			return nats.ErrConsumerNotFound
		}
		report := r.report()
		if report.State != receiveActive {
			return nil
		}
		if report.SubscriptionValid && info.Config.DeliverSubject == report.Subject {
			if _, err := correctListenerDurable(ctx, r.client, r.consumer, info); err != nil {
				return fmt.Errorf("correct the durable consumer's policy: %w", err)
			}
			return nil
		}
	}
	return errDurableUnbound
}

// waitLoops waits for each loop to return, until deadline. A loop is read again once the deadline
// has passed (awaitDone, through closedWithin), so a pass that ended just as it ran out is not
// reported as still running.
func waitLoops(logger *logging.Logger, loops []<-chan struct{}, deadline time.Time) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, done := range loops {
		if awaitDone(ctx, done) != nil {
			logger.Warn("a holder-only loop was still running when receiving ended")
			return
		}
	}
}
