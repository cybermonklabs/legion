package delivery

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// durableBindInterval is how often an intake that does not deliver tries for its durable's hold,
// and for the durable under it, on the hold's clock (heldDurable.holdClock). It is
// cmd/listener/durable.go's interval: a standby polls every two seconds, so it takes the durable
// over within one interval of the holder releasing its hold, and within one interval of the hold's
// lapse when the holder dies (intakeHoldTTL).
//
// Unlike the listener, which gives up at its durableBindDeadline and exits so the runtime starts
// it again, Dispatch's standby polls for the life of the process: there is no deadline and no
// exit. A durable another envoy-dispatch process holds is not a failed start but the ordinary
// state of every process but one, which has every other thing a Dispatch server does to get on
// with meanwhile.
const durableBindInterval = 2 * time.Second

// standbyLogEvery bounds how often one intake says it is standing by for its durable. The first
// refusal is logged as it happens, so an operator sees at once which process stood by and why;
// the hold is then tried every durableBindInterval, and an unthrottled line would be 1,800 an hour
// per durable, each saying exactly what the first said.
const standbyLogEvery = 30 * time.Second

// The hold a process delivers from a durable under (intakeHoldName, duty.Hold): it lasts
// intakeHoldTTL on the database's clock after each renewal, is renewed every intakeHoldRenew, and
// reports itself lost intakeHoldMargin before it lapses, so the holder stops delivering before any
// standby can take it. A holder that dies is replaced within intakeHoldTTL plus one
// durableBindInterval. The holder checks that the durable still delivers to it on the same
// interval it renews (heldDurable.check).
const (
	intakeHoldTTL    = 15 * time.Second
	intakeHoldRenew  = 5 * time.Second
	intakeHoldMargin = 2 * time.Second
)

// IntakeHoldTTLFloor is the TTL an intake's hold must exceed (IntakeBounds.HoldTTL): its renewal
// interval and its margin together, the least duty.TryHold takes with them, since a TTL no longer
// than that leaves no renewal time to be confirmed before the hold reports itself lost.
const IntakeHoldTTLFloor = intakeHoldRenew + intakeHoldMargin

// intakeHoldName names the hold a process delivers from durable under. Its token records what the
// next holder needs (intakeHoldToken).
func intakeHoldName(durable string) string { return "delivery-intake-" + durable }

// intakeHoldToken is what an intake's hold records as its token:
//
//   - subject, the deliver subject the last holder made the durable deliver to, which tells the
//     next holder whether a binding it finds is a holder's (heldDurable.boundElsewhere);
//   - start, the stream sequence a take-over that deleted the durable creates it again at, or 0
//     for none. A take-over records it before its delete and clears it once its create succeeds.
//     What a take-over that never creates the durable again leaves in the row, and who reads
//     that start next, is on heldDurable.finishTakeOver. A holder that stops with the start set
//     lapses its hold rather than deleting the row (heldDurable.stop), so the next holder still
//     reads it.
//
// The token is the subject alone, or the subject followed by intakeHoldTokenStart and the
// sequence in decimal. A deliver subject holds no space, so the two never mix. A token of any
// other shape reads as a subject alone.
type intakeHoldToken struct {
	subject string
	start   uint64
}

// intakeHoldTokenStart separates an intake hold token's subject from its start.
const intakeHoldTokenStart = " start="

func (t intakeHoldToken) String() string {
	if t.start == 0 {
		return t.subject
	}
	return t.subject + intakeHoldTokenStart + strconv.FormatUint(t.start, 10)
}

// parseIntakeHoldToken reads the token a hold's row records (intakeHoldToken).
func parseIntakeHoldToken(token string) intakeHoldToken {
	subject, start, found := strings.Cut(token, intakeHoldTokenStart)
	if !found {
		return intakeHoldToken{subject: token}
	}
	sequence, err := strconv.ParseUint(start, 10, 64)
	if err != nil || sequence == 0 {
		return intakeHoldToken{subject: token}
	}
	return intakeHoldToken{subject: subject, start: sequence}
}

// intakeLeases is the pool an intake takes its durable's hold and consumer lease on and renews the
// hold through: store.Pool.Leases, which StartIntakes hands each intake. A *pgxpool.Pool is both.
type intakeLeases interface {
	duty.Leases
	duty.Holds
}

// consumerLease names the lease every change to one durable runs under: this build's take-over
// (heldDurable.takeOver), and the configuration reconciliation of the build before holds, whose
// processes still run during a rolling release and take the same lease. So the two never delete,
// create or update one durable at once.
//
// The name carries the durable because that, and only that, is what the exclusion is about: two
// processes over one durable. One name over both delivery durables coupled two independent
// reconciliations, and INFRA-66 measured what that cost against a real nats-server and the real
// envoy-dispatch binary, with the Dispatch user denied exactly
// $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.> (envoy-infra's live row "without
// the delivery consumer create Dispatch leaves only that intake unbound"). A denied AddConsumer
// drew no reply at all then, so the create blocked for the client's whole JetStream MaxWait -- 10 s
// (internal/bus/nats.go, nats.MaxWait) -- and the lease was held for every second of it; it now
// ends as the server's refusal arrives (internal/bus/refusal_reads.go), and a take-over step the
// grant refuses is not tried again (retryHeld), so the lease is released within milliseconds,
// while a change that is stuck some other way still holds its lease for as long as it takes. The
// events intake took the lease again on its next durableBindInterval poll and held it for another
// 10 s, round after round:
//
//	17:57:16.232 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:16.232 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//	17:57:26.235 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:26.236 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//
// Under that one name the job intake, whose own durable is created by a pass under the same lease,
// could only get in during the milliseconds between those holds: in CI's run of that row it never
// did, and the row failed with "consumer delivery-jobs was not push-bound within 20s". A lease per
// durable keeps a stuck change to the durable it is stuck on.
func consumerLease(durable string) string { return "delivery-consumer-" + durable }

// bindThrottle is one intake's own standby-log state for its own durable: when it last said it
// was standing by for it, and why. Per intake rather than a package-global map keyed by consumer
// name, because an intake is exactly what the window is about -- one process's repeated polls of
// one durable -- and a global outlives the durable it throttles: a second intake of the same name
// would stand by silently for a durable it has said nothing about.
type bindThrottle struct {
	mu sync.Mutex
	// standby is when the last standby line was logged and standbyReason the message it carried,
	// so a change of state -- the hold going from another process's to a binding no hold recorded
	// -- is said at once rather than waiting out the window of the line before it.
	standby       time.Time
	standbyReason string
}

// reportStandby logs message at INFO to logger, the intake's -- this durable is not this
// process's to deliver from yet, for the reason message names -- unless this intake logged that
// same message within standbyLogEvery.
func (t *bindThrottle) reportStandby(logger *slog.Logger, name, message string) {
	t.mu.Lock()
	if t.standbyReason == message && time.Since(t.standby) < standbyLogEvery {
		t.mu.Unlock()
		return
	}
	t.standby, t.standbyReason = time.Now(), message
	t.mu.Unlock()
	logger.Info(message, "name", name)
}

// heldDurable is one intake's delivery from its durable, which it does only while it holds the
// durable's hold (intakeHoldName), so that of every envoy-dispatch process -- of this build and of
// the build before it, during a rolling release -- one delivers from the durable at a time:
//
//   - A standby tries for the hold every durableBindInterval and takes it when no holder holds it
//     live: released, lapsed, or never taken.
//   - Under the hold, and under the durable's consumer lease, it takes the durable over
//     (takeOver): it records the stream sequence after the durable's ack floor in the hold's
//     token, deletes the durable, subscribes to a fresh deliver subject, creates the durable again
//     at that sequence on that subject, and records the subject alone as the hold's token
//     (intakeHoldToken). The delete and create, rather than an update of the deliver subject,
//     redeliver a dead holder's unacked messages at once and fence the holder off: a
//     subscription its client sends again after a reconnect is on a subject the durable no
//     longer delivers to.
//     nats-server refuses that update anyway while the old subject still has interest, which a
//     dead connection keeps for minutes; a delete has no such check. Subscribing before the
//     create leaves the durable push-bound from the moment it exists. A durable bound to a
//     subject no hold recorded is a process of the build before holds, which binds by lookup and
//     holds nothing: it is never taken from it, and the take-over waits until it lets go.
//   - The holder stops delivering at once when its hold is lost, and when its check finds the
//     durable delivering to another subject (check); it does not handle a message it is handed
//     once its hold's loss deadline has passed, and leaves it with the durable rather than
//     answering for it (intakeDelivery.receive, intakeDelivery.handBack). A hand-back it began
//     that may still reach the server when the hold is lost has its connection severed, before
//     any standby can recreate the durable, by a watcher of the hold of its own (watchLoss).
//   - When its process stops, it stops delivering and hands back every message it was delivered
//     and did not handle. Once no hand-back can begin any more (intakeDelivery.seal) and every one
//     that began reached the server, it releases the hold (stop), so a standby takes over at once
//     and is redelivered those messages within seconds rather than after the ack wait. A stop with
//     a hand-back still pending, or whose flush cannot confirm the hand-backs reached the server,
//     keeps the hold renewing until they have, or until its connection has closed, and releases it
//     only then (finalizeHandBacks); a hold lost meanwhile severs that connection instead
//     (severHandBacks).
//
// The run loop owns hold, recorded and subject; mu guards delivering, which StopDelivering reads
// from any goroutine.
type heldDurable struct {
	nats   *bus.Client
	leases intakeLeases
	// holder names this intake in its hold's row: each intake draws one as it is built.
	holder uuid.UUID
	// throttle is this intake's own standby-log state (bindThrottle).
	throttle *bindThrottle
	// config is the durable's config in this build, timed by tuning; each call draws a fresh
	// deliver subject.
	config func(*intakeTuning) natsgo.ConsumerConfig
	// name is the durable's name: config's Durable, which every call returns alike.
	name string
	// tuning is what this intake times its work by and the steps a test hooks (intakeTuning),
	// shared with every delivery it makes.
	tuning *intakeTuning
	// key reads whether this intake wants a message at all and the key whose goroutine handles it
	// (intakeKey for the delivery intake, jobKey for the job intake).
	key func(*natsgo.Msg) (string, *contracts.Envelope, bool)
	// handle handles one message on its key's goroutine and answers it there, under the delivery
	// it arrived on: that is what carries the handler's context and what says whether an answer
	// of the handler's own is still the hold's to make (intakeDelivery.mayAnswer).
	handle func(delivery *intakeDelivery, message intakeMessage)

	// hold is the durable's hold while this intake holds it.
	hold *duty.Hold
	// settled is hold's settlement, made with it (takeHold) and shared by every delivery made under
	// it (intakeDelivery.markSettled).
	settled *atomic.Bool
	// recorded is the token the hold's row records (intakeHoldToken): the deliver subject the last
	// holder made the durable deliver to, which may be this intake's own, and the start a take-over
	// that deleted the durable and did not create it again recorded.
	recorded intakeHoldToken
	// subject is the deliver subject this intake delivers at, and empty while it delivers nothing.
	subject string
	// boundHook, when not nil, runs once a take-over has bound the durable, before the take-over
	// returns. It is nil outside tests: a test sets it to hold a take-over that has bound the
	// durable until past Run's ctx's end.
	boundHook func()
	// clock is the time this intake's stop runs on: its deadline (stopContext) and the bounds of
	// the flush and the release that follow it (flushHandBacks, releaseHold); the delivery
	// intake's handler deadline too (Intake.deliver), and the job intake's bursts and reads
	// (JobIntake.schedule, JobIntake.fire). clock.Real, which newHeldDurable gives it, and a test's
	// clocktest.Fake, set before Run, as boundHook is.
	clock intakeClock
	// messageDeadline is the end a test gives one message's handling and one run's read in place
	// of the tuning's messageTimeout on clock after each began (Intake.deliver, JobIntake.fire,
	// IntakeBounds.MessageDeadline); its zero value, production's, keeps messageTimeout. The ack
	// wait still derives from messageTimeout (intakeTuning.ackWait).
	messageDeadline clock.Deadline
	// holdTTL is how long this intake's hold lasts on the database's clock after each take and
	// renewal (duty.HoldOptions.TTL): intakeHoldTTL, which newHeldDurable gives it, and a test's
	// own (IntakeBounds.HoldTTL), set before Run. holdCallDeadline is the end a test gives each
	// try's read and take of the hold and each read of the durable (takeHold, lookupDurable) in
	// place of intakeHoldRenew on holdClock (IntakeBounds.HoldCallDeadline); its zero value,
	// production's, keeps intakeHoldRenew.
	holdTTL          time.Duration
	holdCallDeadline clock.Deadline
	// stopDeadline is the end a test gives this intake's stop in place of intakeStopBudget after
	// Run's ctx ends (stopContext, IntakeBounds.StopDeadline); its zero value, production's, keeps
	// intakeStopBudget.
	stopDeadline clock.Deadline
	// holdClock is the time the durable's hold runs on: this intake's tries for it every
	// durableBindInterval and its checks of the durable every intakeHoldRenew (run), the bound on
	// each try's read and take of the hold and on each read of the durable (takeHold,
	// lookupDurable), its renewals and its loss deadline (duty.HoldOptions.Clock), every reading of
	// whether this intake still holds it (holds, intakeDelivery.holdCovers, retryHeld), the
	// take-over's bound and its waits between tries under it (finishTakeOver, retryHeld), and the
	// loss watcher's wait for a flush's answer once the hold is lost (lostWith). clock.Real, which
	// newHeldDurable gives it, and a test's clocktest.Fake, set before Run, on which a try or a
	// check comes, a try's or a check's read gives up, a take-over tries a step again or gives it
	// up, a loss deadline passes and a lost hold's wait for a flush ends only where the test moves
	// it. Whether another process may take the hold is the database's clock either way.
	holdClock clock.Clock
	// checkPasses counts this intake's checks of its durable (check, DurableChecked's pass). Only
	// the run loop's goroutine reads and writes it.
	checkPasses uint64

	mu sync.Mutex
	// delivering is the latest subscription subscribe made, which StopDelivering stops.
	delivering *intakeDelivery
	// finalizers keep a stopped hold renewing while a hand-back still can reach NATS. The caller
	// that closes d.nats waits for them after that close, while its database pool is still open
	// (waitFinalizers).
	finalizers sync.WaitGroup
	// lossWatchers counts this intake's loss watchers still running (watchLoss): subscribe raises it
	// as it starts one, before the watcher's goroutine runs, and the watcher lowers it as it returns.
	// Nothing in production reads it. It is a test seam, read only through the test-only accessor
	// lossWatchers, by the rows that require a watcher to end with its delivery's hand-backs and not
	// with the hold: TestTakingTheDurableAgainUnderOneHoldLeavesNoLossWatcherPerAttempt and
	// TestAReleaseAtTheStopSeversNothingForAnEarlierDeliveryStillConfirmingItsHandBack.
	lossWatchers atomic.Int32
}

// newHeldDurable builds the delivery from config's durable on natsClient, taking its hold and
// consumer lease on leases, routing each message by key and handing it to handle, timed by tuning.
func newHeldDurable(tuning *intakeTuning, natsClient *bus.Client, leases intakeLeases, config func(*intakeTuning) natsgo.ConsumerConfig,
	key func(*natsgo.Msg) (string, *contracts.Envelope, bool), handle func(*intakeDelivery, intakeMessage),
) heldDurable {
	return heldDurable{
		nats: natsClient, leases: leases, holder: uuid.New(), throttle: &bindThrottle{},
		config: config, name: config(tuning).Durable, tuning: tuning, key: key, handle: handle,
		clock: clock.Real(), holdClock: clock.Real(), holdTTL: intakeHoldTTL,
	}
}

// intakeClock is the time an intake's stop and the job intake's schedule run on
// (heldDurable.clock), a narrowing of clock.Clock: clock.Real in production and a test's
// clocktest.Fake. A read is armed for a moment as the wait from the clock's present at the arm,
// so a clock that moved between the read and the arm fires the timer at that moment all the same
// (api's hubClock has the same shape). A handler's deadline and a read's are the clock's too
// (Intake.deliver, JobIntake.fire), and so are the bounds of the stop's flush and release
// (flushHandBacks, releaseHold), so a test moves time past them rather than waiting them out.
type intakeClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) clock.Timer
	WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
}

// intakeHoldLost is the line of an intake whose durable's hold is no longer its own, so it stops
// delivering (run).
const intakeHoldLost = "dispatch delivery: the intake's hold is lost; it stops delivering"

// run delivers from the durable while this intake holds its hold, and stands by for it otherwise,
// until ctx ends; then it stops delivering and releases the hold (stop), under the one stop
// deadline armed at ctx's end (stopContext), so a take-over under way at that moment spends that
// budget rather than starting one of its own. Each durableBindInterval on holdClock while it
// delivers nothing, and ready reports true, it tries for the hold and the durable (attempt); each
// intakeHoldRenew while it delivers, it checks the durable still delivers to it (check); and the
// moment its hold is lost it stops delivering. wake, when not nil, answers a channel the loop
// waits on beside its timers and takes again each time it fires -- the delivery intake's settings
// cache storing settings, which is what makes its first attempt ready (Intake.Run).
func (d *heldDurable) run(ctx context.Context, wake func() <-chan struct{}, ready func() bool) {
	// Armed before the first attempt, so a take-over under way as ctx ends -- a standby's poll,
	// whose JetStream calls take their own time -- spends the stop's budget rather than starting
	// it late.
	stopping, release := stopContext(ctx, d.clock, d.tuning.stopBudget, d.stopDeadline)
	defer release()
	defer d.stop(stopping)
	// The tries tick on a clock.Ticker durableBindInterval apart from the run's start, and the
	// checks on one intakeHoldRenew apart, both on the hold's clock: a try or a check that overran
	// its interval is followed by one at once, and the grid moments it overran are dropped.
	binds := clock.NewTicker(d.holdClock, durableBindInterval, false)
	defer binds.Stop()
	checks := clock.NewTicker(d.holdClock, intakeHoldRenew, false)
	defer checks.Stop()
	attempt := func() {
		if d.subject == "" && ready() {
			d.attempt(ctx)
		}
	}
	// Taken before each attempt, which reads what the cache holds, so settings stored after that
	// read wake the loop.
	var changed <-chan struct{}
	if wake != nil {
		changed = wake()
	}
	attempt()
	for {
		// lost stays nil, and so never ready, while this intake holds no hold.
		var lost <-chan struct{}
		if d.hold != nil {
			lost = d.hold.Lost()
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			changed = wake()
			attempt()
		case <-binds.C():
			binds.Took()
			attempt()
			binds.Next()
		case <-checks.C():
			checks.Took()
			d.check(ctx)
			checks.Next()
		case <-lost:
			d.tuning.log.Warn(intakeHoldLost, "name", d.name)
			// The hold lasts intakeHoldMargin longer on the database's clock, so a handler that
			// finishes within it finished while the hold still held; the rest are given up,
			// since another process may take the hold from that moment. The delivery's loss
			// watcher seals it and settles its hand-backs meanwhile (watchLoss), on a goroutine
			// of its own, so nothing this loop waits for can delay that.
			d.stopDeliveringWithin(intakeHoldMargin)
			d.hold, d.settled, d.subject = nil, nil, ""
		}
	}
}

// stopDeliveringWithin stops this intake's subscription, giving the handlers still running budget
// to finish and be acknowledged before it gives them up (StopDelivering). It runs on a context of
// its own, so a stop of the process does not cut the hand-back short.
func (d *heldDurable) stopDeliveringWithin(budget time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	d.StopDelivering(ctx)
}

// attempt tries to make this intake the one delivering from the durable: it takes the hold when
// no other process holds it live, then takes the durable over under it (takeOver). A durable
// bound to a subject no hold recorded is read first outside the consumer lease, so a holder that
// waits for a process of the build before holds to let go takes no lease on every poll.
func (d *heldDurable) attempt(ctx context.Context) {
	if d.hold == nil && !d.takeHold(ctx) {
		return
	}
	info, err := d.lookupDurable(ctx)
	if err == nil && d.boundElsewhere(info) {
		return
	}
	// The take-over asked for mutual exclusion and nothing else, and no tick is due (the zero time).
	duty.Exclusive(ctx, d.leases, consumerLease(d.name), time.Time{}, 0, d.takeOver)
}

// lookupDurable reads the durable's state from the server, bounded by intakeHoldRenew on the
// hold's clock (heldDurable.holdClock), the clock the try or the check that reads it runs on, or by
// the end a test gave it (holdCallDeadline).
func (d *heldDurable) lookupDurable(ctx context.Context) (*natsgo.ConsumerInfo, error) {
	bounded, cancelBound := d.holdCallDeadline.WithTimeout(d.holdClock, ctx, intakeHoldRenew)
	defer cancelBound()
	// nats.go is shown a deadline it can read: a fake's is its own, and no end shows none
	// (clock.WithShownDeadline).
	lookup, cancel := clock.WithShownDeadline(bounded)
	defer cancel()
	return d.nats.JS().ConsumerInfo(bus.Stream, d.name, natsgo.Context(lookup))
}

// heldElsewhere is the standby line of an intake whose durable's hold another process holds live.
const heldElsewhere = "dispatch delivery: NATS consumer held by another process; standing by"

// takeHold takes the durable's hold when no holder holds it live, keeping the token its row
// records, and reports whether this intake holds it now. duty.TryHold records its own token as it
// takes a lapsed hold, so the lapsed hold's token is read first (duty.HoldToken). The read and the
// take are bounded together by intakeHoldRenew on the hold's clock (heldDurable.holdClock), the
// clock the try runs on, or by the end a test gave them (holdCallDeadline), and the hold takes
// holdTTL (intakeHoldTTL in production).
func (d *heldDurable) takeHold(ctx context.Context) bool {
	holdName := intakeHoldName(d.name)
	bounded, cancel := d.holdCallDeadline.WithTimeout(d.holdClock, ctx, intakeHoldRenew)
	defer cancel()
	token, live, err := duty.HoldToken(bounded, d.leases, holdName)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			d.tuning.log.Error("dispatch delivery: read the intake's hold", "name", d.name, "hold", holdName, "error", err)
		}
		return false
	case live:
		d.throttle.reportStandby(d.tuning.log, d.name, heldElsewhere)
		return false
	}
	hold, err := duty.TryHold(bounded, d.leases, holdName, d.holder, duty.HoldOptions{
		TTL: d.holdTTL, Renew: intakeHoldRenew, Margin: intakeHoldMargin, Token: token, Clock: d.holdClock,
	})
	if err != nil {
		if ctx.Err() == nil {
			d.tuning.log.Error("dispatch delivery: take the intake's hold", "name", d.name, "hold", holdName, "error", err)
		}
		return false
	}
	if hold == nil {
		d.throttle.reportStandby(d.tuning.log, d.name, heldElsewhere)
		return false
	}
	d.hold, d.settled, d.recorded = hold, new(atomic.Bool), parseIntakeHoldToken(token)
	d.tuning.log.Info("dispatch delivery: took the intake's hold", "name", d.name, "hold", holdName)
	return true
}

// boundElsewhere reports whether info is a durable bound to a subject the hold does not record: a
// process of the build before holds, which this intake never takes the durable from. It says so
// at INFO, throttled.
func (d *heldDurable) boundElsewhere(info *natsgo.ConsumerInfo) bool {
	if !info.PushBound || info.Config.DeliverSubject == d.recorded.subject {
		return false
	}
	d.throttle.reportStandby(d.tuning.log, info.Config.Durable, "dispatch delivery: NATS consumer bound to a process that holds no hold; standing by until it lets go")
	return true
}

// holds reports whether this intake may still act under its hold: the hold's loss deadline has
// not passed (duty.Hold.Deadline).
func (d *heldDurable) holds() bool {
	return d.hold != nil && d.holdClock.Now().Before(d.hold.Deadline())
}

// takeOver makes the durable deliver to this intake, under the hold and the durable's consumer
// lease. A durable that does not exist is created with this build's config, at the start a
// take-over before this one recorded when it deleted the durable and did not create it again
// (intakeHoldToken). A durable bound to a subject no hold recorded is left as it is
// (boundElsewhere). Any other -- unbound, or bound to the subject the hold records, which is a
// holder that died or was cut off -- is deleted and created again at its ack floor on a fresh
// deliver subject (takeOverConfig), and that start is recorded as the hold's token before the
// delete. The durable's state is read again here, under the lease, since a process of the build
// before holds can bind it between attempt's read and this one.
//
// This intake subscribes to the fresh deliver subject before it creates the durable, so the
// durable is push-bound from the moment it exists. A process of the build before holds binds by
// looking the durable up and binding it when the lookup reports it unbound; a durable created
// before its subscriber would leave a moment in which that lookup finds it unbound, and both
// processes would then be handed every message on one subject. The subscribe sends its SUB on
// the client's connection before the create's request goes out on the same connection, and the
// server handles one connection's protocol in order, so the subject has interest before the
// create is even read (TestATakeOverCreatesTheDurableAlreadyBound). Then this intake records the
// subject alone as the hold's token, still under the lease.
//
// From the delete on -- or from the lookup, when it finds no durable -- the take-over runs to its
// end inside this one lease (finishTakeOver, which says what a lease given up with the durable
// absent leaves behind).
func (d *heldDurable) takeOver(ctx context.Context) {
	js := d.nats.JS()
	wanted := d.config(d.tuning)
	name := wanted.Durable
	create := wanted
	deleteFirst := false
	info, err := js.ConsumerInfo(bus.Stream, name, natsgo.Context(ctx))
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if d.recorded.start != 0 {
			create = startingAt(wanted, d.recorded.start)
		}
	case err != nil:
		d.tuning.log.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return
	case d.boundElsewhere(info):
		return
	default:
		create = takeOverConfig(info, wanted)
		if !d.holds() {
			return
		}
		pending := intakeHoldToken{subject: d.recorded.subject, start: create.OptStartSeq}
		if err := d.hold.SetToken(ctx, pending.String()); err != nil {
			d.tuning.log.Error("dispatch delivery: record the take-over's start before deleting NATS consumer", "name", name, "start_seq", create.OptStartSeq, "error", err)
			return
		}
		d.recorded = pending
		deleteFirst = true
	}
	d.finishTakeOver(create, deleteFirst)
}

// takeOverConfig is the config a take-over recreates the durable deployed describes with: the
// deployed config when a newer build stamped it with a higher generation, this build's (wanted)
// otherwise, on a fresh deliver subject, starting at the stream sequence after the durable's ack
// floor. Starting there, the recreated durable redelivers at once every message the durable had
// not acknowledged -- what a dead holder had been handed, what a stopped one handed back --
// replays nothing it had, and skips nothing (TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse,
// and TestATakeOverWhoseCreateFailsCreatesTheDurableAtItsFloorOnTheNextAttempt for a create retried).
// The take-over records that start in its hold's token before it deletes the durable, and
// finishTakeOver says what a create that fails after the delete leaves behind.
// The deployed metadata keeps its own keys and drops the ones nats-server sets itself (the
// `_nats.` prefix), which the server sets again on create.
func takeOverConfig(deployed *natsgo.ConsumerInfo, wanted natsgo.ConsumerConfig) natsgo.ConsumerConfig {
	config := wanted
	if bus.MetadataGeneration(deployed.Config.Metadata, consumerGenerationKey) > consumerConfigGeneration {
		config = deployed.Config
		config.DeliverSubject = natsgo.NewInbox()
		config.OptStartTime = nil
		config.Metadata = bus.OwnMetadata(deployed.Config.Metadata, 0)
	}
	return startingAt(config, deployed.AckFloor.Stream+1)
}

// startingAt is config delivering from stream sequence start on.
func startingAt(config natsgo.ConsumerConfig, start uint64) natsgo.ConsumerConfig {
	config.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
	config.OptStartSeq = start
	return config
}

// The bounds on finishTakeOver, the part of a take-over that runs after it has recorded the
// durable's start, and that must not give up the consumer lease with the durable deleted.
//
// takeOverFinishWithin bounds the whole of it, from its first step to its last try, on the hold's
// clock (heldDurable.holdClock). It is under intakeHoldTTL - intakeHoldMargin (13 s), how long a
// hold lasts from its taking or its last renewal. A hold takeHold just took has at most 13 s left
// when attempt reaches these steps, less the lookup of the durable (lookupDurable, up to 5 s), the
// consumer lease (duty.Exclusive) and takeOver's read of the durable before them: with every
// renewal after the taking failing, the hold covers most of the 10 s of the steps, not always all
// of them. A take-over that runs again after a check found the durable delivering elsewhere
// (check) begins at any point in the renewal cycle of a hold this intake already held, so up to
// intakeHoldRenew (5 s) after its last renewal: with the renewals from there on failing, about 8 s
// of the hold are left against the 10 s bound. In both cases retryHeld stops at the hold's
// deadline. That exit is the safe one -- no step acts once another process can take the hold, and
// the next attempt takes the durable over again. takeOverFinishWithin is also inside
// intakeStopBudget, and so inside stopGrace (cmd/dispatch), since a stop that comes meanwhile
// waits for it to end.
//
// takeOverCallTimeout bounds each try of each step, so that one call that draws no answer -- a
// JetStream request during a leader change -- leaves room for the tries after it. A JetStream
// leader election ordinarily ends within it.
//
// A failed try waits takeOverFirstRetry on the hold's clock before the next one, doubling after
// each try up to takeOverLastRetry: with calls that fail at once, that is eight tries within
// takeOverFinishWithin. A try the connection's grant refuses (bus.ErrPublishDenied) is not tried
// again: the server refuses it again however often it is sent, so the step gives up at once and
// the consumer lease is released, rather than held for the rest of takeOverFinishWithin.
const (
	takeOverFinishWithin = 10 * time.Second
	takeOverCallTimeout  = 3 * time.Second
	takeOverFirstRetry   = 250 * time.Millisecond
	takeOverLastRetry    = 2 * time.Second
)

// finishTakeOver makes the durable deliver to this intake at create's start, under the consumer
// lease its caller (takeOver) holds: it deletes the durable when deleteFirst is set, subscribes
// to create's deliver subject, creates the durable with create, and records the subject alone as
// the hold's token. Each step that fails is tried again (retryHeld) until it succeeds, this intake
// no longer holds its hold, the grant refuses it, or takeOverFinishWithin has passed. It runs on a
// context of its own rather than the run's, so a stop that comes while the durable is deleted does
// not leave it so.
// Every step is safe to try again: a delete of a durable already gone succeeds, and a create of
// a durable that a timed-out try did create, with this same config, returns that durable.
//
// A create that fails after the delete is therefore tried again inside the one consumer lease, so
// among builds that take holds such a create loses neither the durable nor its start. The lease is
// given up with the durable absent only when this process is killed between the delete and the
// create, when it loses its hold, when the grant refuses the create, or when every try fails
// within takeOverFinishWithin. The start then stays in the hold's token (intakeHoldToken), and the
// next take-over of this build -- by
// this intake, or by the process that takes the hold next -- creates the durable at that old ack
// floor rather than at the default start (deliveryConsumerConfig names none, so the stream's first
// message). A process of the build before holds reconciles the durable under the same lease and
// never reads the token: finding the durable absent, it creates it at its own default start, the
// stream's first message for the events durable and the next new message for the jobs durable
// (TestATakeOverWhoseCreateFailsCreatesTheDurableBeforeAPreviousBuildReconciles).
func (d *heldDurable) finishTakeOver(create natsgo.ConsumerConfig, deleteFirst bool) {
	js := d.nats.JS()
	// nats.go is shown a deadline it can read: a fake's is its own (clock.WithShownTimeout).
	finish, cancel := clock.WithShownTimeout(d.holdClock, context.Background(), takeOverFinishWithin)
	defer cancel()
	if deleteFirst {
		if d.retryHeld(finish, "delete NATS consumer to take it over", func(call context.Context) error {
			if err := js.DeleteConsumer(bus.Stream, d.name, natsgo.Context(call)); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
				return err
			}
			return nil
		}) != nil {
			return
		}
	}
	if d.retryHeld(finish, "bind NATS consumer", func(context.Context) error {
		_, err := d.subscribe(create.DeliverSubject)
		return err
	}, "subject", create.FilterSubject) != nil {
		return
	}
	if d.retryHeld(finish, "create NATS consumer", func(call context.Context) error {
		_, err := d.tuning.createDurable(js, &create, natsgo.Context(call))
		return err
	}, "subject", create.FilterSubject, "start_seq", create.OptStartSeq) != nil {
		d.stopDeliveringWithin(intakeHoldRenew)
		return
	}
	bound := intakeHoldToken{subject: create.DeliverSubject}
	if d.retryHeld(finish, "record the intake's deliver subject", func(call context.Context) error {
		return d.hold.SetToken(call, bound.String())
	}) != nil {
		// The durable delivers here, but the hold does not say so: the next holder would take
		// the binding for a process that holds nothing and wait for it. Stop, and take the
		// durable over again on the next attempt.
		d.stopDeliveringWithin(intakeHoldRenew)
		return
	}
	d.recorded, d.subject = bound, create.DeliverSubject
	d.tuning.log.Info("dispatch delivery: NATS consumer bound", "name", d.name, "subject", create.FilterSubject,
		"start_seq", create.OptStartSeq, "generation", bus.MetadataGeneration(create.Metadata, consumerGenerationKey))
	if d.boundHook != nil {
		d.boundHook()
	}
}

// errHoldNotHeld is retryHeld finding that this intake no longer holds its hold.
var errHoldNotHeld = errors.New("the intake no longer holds its hold")

// retryHeld calls step until it succeeds, while this intake holds its hold and finish has not
// ended, waiting takeOverFirstRetry on the hold's clock after the first failed try and twice as
// long after each one after it, up to takeOverLastRetry. Each try runs on a context that ends
// takeOverCallTimeout after it starts, or at the hold's loss deadline if that comes first, so no
// step acts once another process can hold the hold. A try the connection's grant refuses
// (bus.ErrPublishDenied) ends the step at once, since no later try gets past it. A failed try is
// logged at WARN as doing what names, and the failure it gives up on at ERROR by failedStep, so a
// caller writes no step's text a second time and keeps only what it does next. It returns nil once
// step succeeds, and otherwise the last try's error, or errHoldNotHeld. attributes are the
// caller's own on that ERROR line.
func (d *heldDurable) retryHeld(finish context.Context, doing string, step func(context.Context) error, attributes ...any) error {
	wait := takeOverFirstRetry
	for {
		if !d.holds() {
			return d.failedStep(doing, errHoldNotHeld, attributes)
		}
		deadline := d.holdClock.Now().Add(takeOverCallTimeout)
		if lost := d.hold.Deadline(); lost.Before(deadline) {
			deadline = lost
		}
		call, cancel := d.holdClock.WithDeadline(finish, deadline)
		err := step(call)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, bus.ErrPublishDenied) {
			return d.failedStep(doing, err, attributes)
		}
		d.tuning.log.Warn("dispatch delivery: take-over step failed; trying it again", "name", d.name, "step", doing, "retry_in", wait, "error", err)
		timer := d.holdClock.NewTimer(wait)
		select {
		case <-finish.Done():
			timer.Stop()
			return d.failedStep(doing, err, attributes)
		case <-timer.C():
		}
		wait = min(2*wait, takeOverLastRetry)
	}
}

// failedStep logs the take-over step retryHeld gave up on at ERROR, as "dispatch delivery: " and
// doing, with the durable's name, the caller's own attributes and err, and answers err.
func (d *heldDurable) failedStep(doing string, err error, attributes []any) error {
	d.tuning.log.Error("dispatch delivery: "+doing, append(append([]any{"name", d.name}, attributes...), "error", err)...)
	return err
}

// durableDeliversElsewhere is the line of an intake whose durable no longer delivers to its
// deliver subject, so it stops delivering (check).
const durableDeliversElsewhere = "dispatch delivery: the NATS consumer no longer delivers to this intake; it stops delivering"

// DurableChecked is the line an intake writes at Debug at the end of each check of its durable
// (check), after the give-up the check logged and the stop it made, so a test can count a process's
// checks from its log and read, once it has seen a check's line, everything that check did;
// production's handler, at Info, leaves it out. Its fields are name, the durable; pass, the check's
// number in this intake's run, from 1; and found, what the check found: checkNotDelivering,
// checkDeliversHere, checkDeliversElsewhere, checkDeleted or checkUnread.
const DurableChecked = "dispatch delivery: the intake checked its NATS consumer"

// What a check of the durable found (DurableChecked's found).
const (
	// checkNotDelivering: the intake delivers nothing, so the check looks nothing up.
	checkNotDelivering = "not delivering"
	// checkDeliversHere: the durable delivers to this intake's deliver subject.
	checkDeliversHere = "delivers here"
	// checkDeliversElsewhere: the durable delivers to another subject, so the intake stops.
	checkDeliversElsewhere = "delivers elsewhere"
	// checkDeleted: the durable is gone, so the intake stops.
	checkDeleted = "deleted"
	// checkUnread: the lookup failed, which says nothing and changes nothing.
	checkUnread = "unread"
)

// check stops delivering when the durable no longer delivers to this intake's deliver subject:
// something deleted it, or replaced it with one on another subject. The intake keeps its hold, and
// its next attempt takes the durable over again, or waits for a process that binds without a
// hold. A lookup that fails says nothing, and changes nothing. Each check writes DurableChecked
// last. Whether it stops is decided in the arm that found it; found is the line's label alone.
func (d *heldDurable) check(ctx context.Context) {
	d.checkPasses++
	found, stop := checkNotDelivering, false
	if d.subject != "" {
		info, err := d.lookupDurable(ctx)
		switch {
		case errors.Is(err, natsgo.ErrConsumerNotFound):
			found, stop = checkDeleted, true
		case err != nil:
			found = checkUnread
		case info.Config.DeliverSubject == d.subject:
			found = checkDeliversHere
		default:
			found, stop = checkDeliversElsewhere, true
		}
	}
	if stop {
		d.tuning.log.Warn(durableDeliversElsewhere, "name", d.name)
		// The durable's messages are the next holder's from here, so a handler still running after
		// the renewal interval is given up rather than left to write what that holder writes too.
		d.stopDeliveringWithin(intakeHoldRenew)
		d.subject = ""
	}
	if d.tuning.log.Enabled(ctx, slog.LevelDebug) {
		d.tuning.log.Debug(DurableChecked, "name", d.name, "pass", d.checkPasses, "found", found)
	}
}

// stop is the end of run: it stops delivering under ctx, the run's stop deadline, and releases the
// hold once every hand-back reached the server (handBacksReached), so a standby takes the durable
// over at once and is redelivered whatever this intake handed back. A hold whose token still records
// a take-over's start (intakeHoldToken) is lapsed instead, which frees it as Release does and keeps
// the row, so the next holder reads that start. The flush and the release run on bounds of their
// own, on the stop's clock (flushHandBacks, releaseHold): the release is one statement, and it must
// reach the database even when the stop's budget has run out.
//
// A hand-back still pending when the stop's context ends does not keep Run past that context, and
// does not give the hold up either. A hand-back is a publish at the message's reply subject, which
// names the durable rather than the consumer: one that reached the server after a standby had taken
// the durable over, and recreated it under the same name, would move that message's pending entry on
// the standby's consumer and have it delivered a second time. So the hold keeps renewing on a
// goroutine of its own (finalizeHandBacks) until that hand-back lands and a flush on its own
// connection confirms it, or the intake client's connection closes. A socket that carries an
// unconfirmed hand-back is reset as it closes (bus.Client.MarkUnconfirmed), so at Close the
// hand-back is dropped with it, whether it was still in nats.go's buffer or in the kernel's send
// queue; only then is the hold released. The renewal ends with the process, together with the
// connection, so the row cannot lapse while the hand-back can still be sent.
// A hold lost before the hand-back is confirmed -- its renewals failing, as when the
// database is out of reach -- cannot be kept, so the intake client's connection is severed instead
// (severHandBacks), before the row lapses and a standby can take the durable over.
func (d *heldDurable) stop(ctx context.Context) {
	d.StopDelivering(ctx)
	if d.hold == nil {
		return
	}
	hold, lapse := d.hold, d.recorded.start != 0
	d.hold, d.settled, d.subject = nil, nil, ""
	d.mu.Lock()
	delivery := d.delivering
	d.mu.Unlock()
	if d.handBacksReached(ctx, delivery, hold) {
		delivery.markSettled()
		d.releaseHold(hold, lapse)
		return
	}
	select {
	case <-hold.Lost():
		// Another holder may act once the row lapses, and there is no hold left to keep. The
		// delivery's loss watcher severs what may still reach NATS (watchLoss).
	default:
		d.tuning.log.Warn(intakeHoldFinalizing, "name", d.name)
		d.finalizers.Add(1)
		go func() {
			defer d.finalizers.Done()
			d.finalizeHandBacks(delivery, hold, lapse)
		}()
	}
}
