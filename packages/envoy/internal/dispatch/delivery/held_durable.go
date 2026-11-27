package delivery

import (
	"cmp"
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
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// durableBindInterval is how often an intake that does not deliver tries for its durable's hold,
// and for the durable under it. It was the listener's own bind interval until
// cybermonklabs/legion#2404's delivery lease replaced that bind: a standby polls every two seconds,
// so it starts to take the durable over within one interval of the holder releasing its hold. A
// release wakes no standby here (the intake listens on no duty.HoldReleasedChannel), and the
// take-over's own statements and JetStream calls come after that start. When the
// holder dies it takes the durable over at the hold's lapse (intakeHoldTTL) rather than at the
// poll after it, which comes up to one interval later: a poll that finds the hold live arms a
// timer to the lapse it read (heldDurable.run).
//
// Dispatch's standby polls for the life of the process: there is no deadline and no exit. The
// listener's standby does the same now: it waits for the machine's delivery lease for as long as
// it runs, reading the lease record every half second, and takes the lease over within seconds of
// its holder's stop (cmd/listener/lease.go). A durable another envoy-dispatch process holds is not
// a failed start but the ordinary state of every process but one, which has every other thing a
// Dispatch server does to get on with meanwhile.
const durableBindInterval = 2 * time.Second

// standbyLogEvery bounds how often one intake says it is standing by for its durable. The first
// refusal is logged as it happens, so an operator sees at once which process stood by and why;
// the hold is then tried every durableBindInterval, and an unthrottled line would be 1,800 an hour
// per durable, each saying exactly what the first said.
const standbyLogEvery = 30 * time.Second

// The hold a process delivers from a durable under (intakeHoldName, duty.Hold): it lasts
// intakeHoldTTL on the database's clock after each renewal, is renewed every intakeHoldRenew, and
// reports itself lost intakeHoldMargin before it lapses, so the holder stops delivering before any
// standby can take it. A holder that dies was replaced within intakeHoldTTL plus one
// durableBindInterval in the runs below, a JetStream reply lost with the standby's connection
// during the take-over included. That bound is measured, and no code holds it: the take-over's read
// of the durable may take up to intakeHoldRenew (lookupDurable), each of its finishing steps up to
// takeOverCallTimeout, and the whole finish up to takeOverFinishWithin (finishTakeOver). Its hold
// lapses intakeHoldTTL after the last renewal that landed -- the whole of intakeHoldTTL after the
// death when the death comes the instant after one -- and a standby tries for it again at the lapse
// its last poll read (heldDurable.run), so the take-over starts at the lapse and has
// durableBindInterval to finish in. At that worst phase, with the standby's poll just before the
// lapse, twenty runs replaced the holder 15.021 s to 15.041 s after its death, a margin of 1.959 s
// at least (TestADeadHolderIsReplacedWithinTheStatedBoundAtTheWorstPhase). The bound holds while
// Postgres answers: two of the take-over's statements commit on its way to the durable, the hold's
// take (takeHold) and the record of the durable's start before its delete (takeOver), and they must
// answer within about durableBindInterval together. A Postgres stall delays the take-over by its
// length: in a builds Job whose Postgres waited on a shared disk to commit, one take of the hold
// took 2.1 s and the row passed the bound at 17.78 s (dispatch://INFRA-557, round 181). A Cloud SQL
// failover, about a minute, delays it by that, and the holder cannot renew its hold meanwhile
// either, so no process delivers until Postgres answers. A standby that only polls does not keep
// the bound at the worst phase: its next poll comes a whole interval after the one before the
// lapse, so its take-over starts just short of the bound and passes it only by the take-over's own
// statements and JetStream calls, by milliseconds and not in every run. With no lapse timer
// (lapse.Reset(left) removed), the row takes the durable over about 20.9 s after the death (20.918
// s under the row's hooks; ten runs under its earlier timing took 21.011 s to 21.019 s), a known
// time past the bound (worstPhasePoll says why). A reply lost with the standby's connection costs
// the take-over the reconnect, nats.go's reconnect wait of about 1 s, which the interval holds too
// (TestATakeOverThatLosesAReplyWithItsConnectionReplacesADeadHolderWithinTheStatedBound): each of
// the take-over's JetStream calls ends at that connection's loss and is sent again on the
// connection that came back at once (onConnection), rather than at its deadline. The holder checks
// that the durable still delivers to it on the same interval it renews (heldDurable.check).
const (
	intakeHoldTTL    = 15 * time.Second
	intakeHoldRenew  = 5 * time.Second
	intakeHoldMargin = 2 * time.Second
)

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
//   - A standby tries for the hold every durableBindInterval, and again the moment a hold it found
//     live lapses, and takes it when no holder holds it live: released, lapsed, or never taken.
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
// The run loop owns hold, recorded, subject and resumeAt; mu guards delivering, which
// StopDelivering reads from any goroutine.
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
	// resumeAt is the stream sequence the durable this intake bound is created again at if it is
	// lost outside a take-over (check): the start it was created at, and then the sequence after
	// the ack floor each check reads of it (bus.AckFloor). 0 is no start known, while this intake
	// has bound nothing or created the durable with none.
	resumeAt uint64
	// boundHook, when not nil, runs once a take-over has bound the durable, before the take-over
	// returns. It is nil outside tests: a test sets it to hold a take-over that has bound the
	// durable until past Run's ctx's end.
	boundHook func()
	// bindEvery, when not zero, is how often run polls in place of durableBindInterval. It is zero
	// outside tests, where its only cost is one comparison when run starts; worstPhasePoll
	// (intake_takeover_lost_test.go) says why a row sets it.
	bindEvery time.Duration
	// attemptHook, when not nil, runs before each attempt run makes while this intake delivers
	// nothing, whatever started it: the first, a poll, or the lapse timer. It is nil outside tests,
	// where its only cost is one comparison per attempt; runNotingAttempts
	// (intake_lapse_timer_test.go) sets it to note when each attempt is made.
	attemptHook func()

	mu sync.Mutex
	// delivering is the latest subscription subscribe made, which StopDelivering stops.
	delivering *intakeDelivery
	// finalizers keep a stopped hold renewing while a hand-back still can reach NATS. The caller
	// that closes d.nats waits for them after that close, while its database pool is still open
	// (waitFinalizers).
	finalizers sync.WaitGroup
}

// newHeldDurable builds the delivery from config's durable on natsClient, taking its hold and
// consumer lease on leases, routing each message by key and handing it to handle, timed by tuning.
func newHeldDurable(tuning *intakeTuning, natsClient *bus.Client, leases intakeLeases, config func(*intakeTuning) natsgo.ConsumerConfig,
	key func(*natsgo.Msg) (string, *contracts.Envelope, bool), handle func(*intakeDelivery, intakeMessage),
) heldDurable {
	return heldDurable{
		nats: natsClient, leases: leases, holder: uuid.New(), throttle: &bindThrottle{},
		config: config, name: config(tuning).Durable, tuning: tuning, key: key, handle: handle,
	}
}

// intakeHoldLost is the line of an intake whose durable's hold is no longer its own, so it stops
// delivering (run).
const intakeHoldLost = "dispatch delivery: the intake's hold is lost; it stops delivering"

// run delivers from the durable while this intake holds its hold, and stands by for it otherwise,
// until ctx ends; then it stops delivering and releases the hold (stop), under the one stop
// deadline armed at ctx's end (stopContext), so a take-over under way at that moment spends that
// budget rather than starting one of its own. At once and each durableBindInterval while it
// delivers nothing, it tries for the hold and the durable (attempt), and again the moment a hold
// an attempt found another process holding lapses, by the time that attempt's read left it; each
// intakeHoldRenew while it delivers, it checks the durable still delivers to it (check); and the
// moment its hold is lost it stops delivering.
func (d *heldDurable) run(ctx context.Context) {
	// Armed before the first attempt, so a take-over under way as ctx ends -- a standby's poll,
	// whose JetStream calls take their own time -- spends the stop's budget rather than starting
	// it late.
	stopping, release := stopContext(ctx, d.tuning.stopBudget)
	defer release()
	defer d.stop(stopping)
	binds := time.NewTicker(cmp.Or(d.bindEvery, durableBindInterval))
	defer binds.Stop()
	checks := time.NewTicker(intakeHoldRenew)
	defer checks.Stop()
	// lapse fires as the hold the last attempt found another process holding live lapses: the
	// time that attempt's read left it on the database's clock, counted from the read's answer,
	// so it fires after the lapse and never before. A standby whose holder died tries again then
	// rather than at its next poll, which can come up to durableBindInterval after the lapse. The
	// attempt it makes is an ordinary one, which reads the hold again and takes it only if no
	// holder holds it live, so a holder whose renewal landed after the read the timer was armed
	// from keeps its hold (TestAHolderThatRenewsBetweenAStandbysReadAndItsLapseTimerKeepsTheHold).
	// An attempt that leaves this intake holding the hold stops the timer, so a lapse read before
	// a poll took the hold fires no attempt later (TestAStandbyWhosePollTakesTheHoldLeavesNoLapseTimerToFire).
	// Any other attempt that finds no hold held live leaves the timer as it was: one whose read
	// failed keeps the lapse the read before it found, and a timer that fires once another process
	// has taken the hold costs one attempt, which reads that process's hold and arms to its lapse.
	lapse := time.NewTimer(time.Hour)
	lapse.Stop()
	defer lapse.Stop()
	attempt := func() {
		if d.subject == "" {
			if d.attemptHook != nil {
				d.attemptHook()
			}
			switch left := d.attempt(ctx); {
			case left > 0:
				lapse.Reset(left)
			case d.hold != nil:
				lapse.Stop()
			}
			if hook := d.tuning.attempted.Load(); hook != nil {
				(*hook)()
			}
		}
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
		case <-binds.C:
			attempt()
		case <-lapse.C:
			attempt()
		case <-checks.C:
			d.check(ctx)
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
// waits for a process of the build before holds to let go takes no lease on every poll. It
// answers how long the hold another process holds live has left, as its read found it
// (takeHold), and zero when this intake holds the hold, or found it free, or could not read it.
func (d *heldDurable) attempt(ctx context.Context) (left time.Duration) {
	if d.hold == nil {
		if left = d.takeHold(ctx); d.hold == nil {
			return left
		}
	}
	info, err := d.lookupDurable(ctx)
	if err == nil && d.boundElsewhere(info) {
		return 0
	}
	// The take-over asked for mutual exclusion and nothing else, and no tick is due (the zero time).
	// The lease's lines are the intake's, so they go to its logger.
	duty.Exclusive(ctx, d.tuning.log, d.leases, consumerLease(d.name), time.Time{}, 0, d.takeOver)
	return 0
}

// lookupDurable reads the durable's state from the server, bounded by intakeHoldRenew and by the
// connection it goes out on (onConnection).
func (d *heldDurable) lookupDurable(ctx context.Context) (*natsgo.ConsumerInfo, error) {
	lookup, cancel := context.WithTimeout(ctx, intakeHoldRenew)
	defer cancel()
	return onConnection(lookup, d.nats, d.consumerInfo)
}

// heldElsewhere is the standby line of an intake whose durable's hold another process holds live.
const heldElsewhere = "dispatch delivery: NATS consumer held by another process; standing by"

// takeHold takes the durable's hold when no holder holds it live, keeping the token its row
// records, and sets d.hold once this intake holds it. While another process holds it live, it
// answers how long that hold has left on the database's clock, as the read found it, and zero
// otherwise. duty.TryHold records its own token as it takes a lapsed hold, so the lapsed hold's
// token is read first (duty.HoldToken), in the read that says how long the hold has left.
func (d *heldDurable) takeHold(ctx context.Context) (left time.Duration) {
	holdName := intakeHoldName(d.name)
	bounded, cancel := context.WithTimeout(ctx, intakeHoldRenew)
	defer cancel()
	token, left, err := duty.HoldToken(bounded, d.leases, holdName)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			d.tuning.log.Error("dispatch delivery: read the intake's hold", "name", d.name, "hold", holdName, "error", err)
		}
		return 0
	case left > 0:
		d.throttle.reportStandby(d.tuning.log, d.name, heldElsewhere)
		return left
	}
	hold, err := duty.TryHold(bounded, d.leases, holdName, d.holder, duty.HoldOptions{
		TTL: intakeHoldTTL, Renew: intakeHoldRenew, Margin: intakeHoldMargin, Token: token, Logger: d.tuning.log,
	})
	if err != nil {
		if ctx.Err() == nil {
			d.tuning.log.Error("dispatch delivery: take the intake's hold", "name", d.name, "hold", holdName, "error", err)
		}
		return 0
	}
	if hold == nil {
		d.throttle.reportStandby(d.tuning.log, d.name, heldElsewhere)
		return 0
	}
	d.hold, d.settled, d.recorded = hold, new(atomic.Bool), parseIntakeHoldToken(token)
	d.tuning.log.Info("dispatch delivery: took the intake's hold", "name", d.name, "hold", holdName)
	return 0
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
	return d.hold != nil && time.Now().Before(d.hold.Deadline())
}

// durableDeliversElsewhere is the line of an intake whose durable no longer delivers to its
// deliver subject, so it stops delivering (check).
const durableDeliversElsewhere = "dispatch delivery: the NATS consumer no longer delivers to this intake; it stops delivering"

// durableLost is the line of an intake whose bound durable is gone, which the next take-over
// creates again at start_seq (check), moved back to the first message the stream holds when it lies
// past the stream's end (createStart): the sequence after the last ack floor the intake read, or
// the start it was created at when no check had read one since. check runs only while this intake
// delivers from a durable it created, and every create names a start of at least 1
// (finishTakeOver sets resumeAt to it), so start_seq is never 0.
const durableLost = "dispatch delivery: the NATS consumer this intake had bound is gone; the next take-over creates it again at start_seq"

// check stops delivering when the durable no longer delivers to this intake's deliver subject:
// something deleted it, or replaced it with one on another subject. The intake keeps its hold, and
// its next attempt takes the durable over again, or waits for a process that binds without a
// hold. A lookup that fails says nothing, and changes nothing.
//
// A durable that still delivers here gives check its ack floor (bus.AckFloor, takeOverStart's
// floor rule), and the sequence after it is where the durable is created again if it is lost
// (resumeAt). A durable that has been delivered nothing (deliveredNothing) teaches nothing beyond
// the start it was created at, which resumeAt already holds. A durable that is gone was lost
// outside a take-over, by a delete or a stream's loss: check says so at ERROR, in the one line it
// writes for the loss, and records resumeAt as the start in the hold's token (recordLoss), so the
// next take-over, this intake's or the next holder's, creates it there (takeOver, createStart),
// moved back to the stream's first message if the stream's end now lies below it. Created at the
// stream's next message instead, as a durable with no start recorded is, it would skip every
// message unacknowledged at the loss and every one published before the new create, and no
// reconcile pass writes the head rows those messages carry (intake.go's package comment).
func (d *heldDurable) check(ctx context.Context) {
	if d.subject == "" {
		return
	}
	info, err := d.lookupDurable(ctx)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		d.recordLoss(ctx)
	case err != nil:
		return
	case info.Config.DeliverSubject == d.subject:
		if !deliveredNothing(info) {
			d.resumeAt = bus.AckFloor(consumerPosition(info)) + 1
		}
		return
	default:
		d.tuning.log.Warn(durableDeliversElsewhere, "name", d.name)
	}
	// The durable's messages are the next holder's from here, so a handler still running after
	// the renewal interval is given up rather than left to write what that holder writes too.
	d.stopDeliveringWithin(intakeHoldRenew)
	d.subject = ""
}

// recordLoss says at ERROR that the durable this intake had bound is gone, and records resumeAt as
// the start in the hold's token, where the next take-over reads it (check). A token write that
// fails is logged and leaves the start in this intake's own record, so its own next take-over
// creates the durable there all the same; only another holder's would not.
func (d *heldDurable) recordLoss(ctx context.Context) {
	d.tuning.log.Error(durableLost, "name", d.name, "start_seq", d.resumeAt)
	pending := intakeHoldToken{subject: d.recorded.subject, start: d.resumeAt}
	if err := d.hold.SetToken(ctx, pending.String()); err != nil {
		d.tuning.log.Error("dispatch delivery: record the lost NATS consumer's start", "name", d.name, "start_seq", d.resumeAt, "error", err)
	}
	d.recorded = pending
}

// stop is the end of run: it stops delivering under ctx, the run's stop deadline, and releases the
// hold once every hand-back reached the server (handBacksReached), so a standby takes the durable
// over at once and is redelivered whatever this intake handed back. A hold whose token still records
// a take-over's start (intakeHoldToken) is lapsed instead, which frees it as Release does and keeps
// the row, so the next holder reads that start. The release runs on a context of its own: it is one
// statement, and it must reach the database even when the stop's budget has run out.
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

// intakeHoldFinalizing is the line of a stop whose hand-back had not reached NATS by its deadline,
// so its hold stays renewing until it does (stop).
const intakeHoldFinalizing = "dispatch delivery: a hand-back has not reached NATS by the stop's deadline, so the intake's hold stays renewing until it does"

// finalizeHandBacks keeps hold renewing while a hand-back of the stopped delivery is pending, and
// releases it once that hand-back has landed and a flush on the connection it was written to
// answers, or once the intake client's connection has closed (handBacksSettled). A connection that
// carries a hand-back no flush on it has confirmed resets its sockets as it closes
// (bus.Client.MarkUnconfirmed): at Close, which StartIntakes' stop makes, nats.go's final flush of
// its outbound buffer and whatever the kernel still holds of the hand-back are discarded rather than
// sent, and when nats.go gives a socket up as stale and reconnects, a flush on the new socket
// confirms nothing the old one could still send; only then is it safe to release the hold. It runs
// on a goroutine of its own that nothing in Run waits for, so it waits for nats.go's lock with no
// bound and makes one flush at a time. StartIntakes waits for it after it closes that client and
// before it returns to a caller that can close the database pool. A lost hold ends it, and severs
// the connection that carries the hand-back (severHandBacks): another holder may act once the row
// lapses, as after any loss of the hold, and the hand-back must not reach the durable that holder
// recreates.
func (d *heldDurable) finalizeHandBacks(delivery *intakeDelivery, hold *duty.Hold, lapse bool) {
	select {
	case <-delivery.handBacksLanded():
	case <-hold.Lost():
		// The delivery's loss watcher severs what may still reach NATS (watchLoss).
		return
	}
	retry := time.NewTicker(intakeFlushWait)
	defer retry.Stop()
	for {
		if d.handBacksSettled() {
			if before := d.tuning.beforeRelease.Load(); before != nil {
				(*before)()
			}
			select {
			case <-hold.Lost():
			default:
				delivery.markSettled()
				d.releaseHold(hold, lapse)
			}
			return
		}
		select {
		case <-hold.Lost():
			return
		case <-retry.C:
		}
	}
}

// intakeHandBacksSevered is the line of an intake whose hold was lost while a hand-back it began
// could still reach NATS, so it severs the connection that carries it (severHandBacks).
const intakeHandBacksSevered = "dispatch delivery: the intake's hold is lost with a hand-back NATS may not have yet, so its connection is severed and the hand-back never reaches the durable the next holder recreates"

// severHandBacks makes sure that no hand-back of this intake's stopped delivery reaches NATS, for a
// caller whose hold is lost while one may still reach the server: a publish NATS has not taken
// (intakeDelivery.handBacksLanded), or one nats.go took whose arrival no flush has confirmed. The
// delivery is sealed already (intakeDelivery.seal), so no hand-back begins any more. It severs the
// intake client's connection (bus.Client.Sever): every hand-back is a publish on that connection, or
// on one the client already closed, which sends nothing, since the client replaces only a closed
// connection. Sever resets the connection's sockets without waiting for nats.go's lock, which a write
// to a stalled socket holds until its write deadline, so a publish waiting behind that write never
// goes out, nor what the kernel had not sent yet; the client then dials a connection afresh.
//
// The hold reports itself lost intakeHoldMargin before its row lapses (duty.Hold), and no standby
// takes the durable over before the row lapses, so a sever made within that margin comes before any
// standby can recreate the durable. Keeping the hold renewing is no answer here: a lost hold is one
// this process could not renew, or one the database already names another holder in. The messages
// the hand-backs named stay outstanding, and the next holder's take-over recreates the durable at
// its ack floor and is handed them once (takeOverConfig).
func (d *heldDurable) severHandBacks() {
	d.tuning.log.Warn(intakeHandBacksSevered, "name", d.name)
	d.nats.Sever()
}

// watchLoss is a delivery's loss watcher, which subscribe starts for every delivery made under a
// hold. It waits for hold.Lost, which a release closes too, or for the delivery's seal at the end of
// its stop, whichever comes first:
//
//   - On the loss, it returns at once when the hold was released with every hand-back the connection
//     carries confirmed, which marks every delivery made under the hold (intakeDelivery.markSettled).
//     Otherwise it seals delivery, so no hand-back begins any more (intakeDelivery.seal), and severs
//     (severHandBacks) when a hand-back delivery began may still reach the server -- one NATS has not
//     taken (handBacksLanded), or one whose arrival a flush does not confirm within intakeFlushWait,
//     half the hold's margin (handBacksSettled).
//   - Once sealed, it returns when delivery handed nothing back, since nothing of it can reach the
//     server any more; otherwise it flushes until a flush confirms every hand-back, one flush at a
//     time, and returns then. A loss before that is handled as above.
//
// So a watcher ends with its delivery's hand-backs, and not with the hold: the intake subscribes
// again under one live hold after a create or a token write that fails, and after a check that finds
// the durable elsewhere, and a watcher that waited for the hold's end kept one goroutine and one
// stopped delivery per take-over for as long as the hold lasted, which can be days. A stopped
// delivery whose hand-back is still pending keeps its watcher, and is severed for on the loss.
//
// Each wait for a flush's answer is bounded on this goroutine: the flush itself takes nats.go's lock,
// which a write to a stalled socket can hold for a minute, and runs on a goroutine of its own, which
// the sever's reset of that socket ends. Nothing else the intake does -- the run loop's stop, a check
// of the durable, the finalizer's own flush -- comes between the loss and the sever.
//
// A delivery that handed nothing back, or whose hand-backs the flush confirms, keeps its
// connection, so the handlers the run loop's stop waits for still acknowledge their messages
// through it.
func (d *heldDurable) watchLoss(delivery *intakeDelivery, hold *duty.Hold) {
	select {
	case <-hold.Lost():
		d.lostWith(delivery, nil)
		return
	case <-delivery.sealedSignal:
	}
	if !delivery.handedBack() {
		return
	}
	retry := time.NewTicker(intakeFlushWait)
	defer retry.Stop()
	for {
		confirming := d.confirmHandBacks(delivery)
		select {
		case <-hold.Lost():
			d.lostWith(delivery, confirming)
			return
		case settled := <-confirming:
			if settled {
				select {
				case <-hold.Lost():
					d.lostWith(delivery, nil)
				default:
				}
				return
			}
		}
		select {
		case <-hold.Lost():
			d.lostWith(delivery, nil)
			return
		case <-retry.C:
		}
	}
}

// lostWith is the loss of the hold delivery was made under (watchLoss): unless the hold was
// released with every hand-back confirmed, it seals delivery and severs unless confirming -- a
// confirmation already under way, or a new one when nil -- answers within intakeFlushWait that every
// hand-back delivery made has reached the server.
func (d *heldDurable) lostWith(delivery *intakeDelivery, confirming <-chan bool) {
	if delivery.settledBeforeRelease() {
		return
	}
	delivery.seal()
	if !delivery.handedBack() {
		return
	}
	if confirming == nil {
		confirming = d.confirmHandBacks(delivery)
	}
	wait := time.NewTimer(intakeFlushWait)
	defer wait.Stop()
	select {
	case settled := <-confirming:
		if settled {
			return
		}
	case <-wait.C:
	}
	d.severHandBacks()
}

// confirmHandBacks answers, on the channel it returns, whether every hand-back delivery made has
// reached the server: once each has landed (handBacksLanded), whether a flush confirms them
// (handBacksSettled). It runs on a goroutine of its own, which a sever ends.
func (d *heldDurable) confirmHandBacks(delivery *intakeDelivery) <-chan bool {
	confirmed := make(chan bool, 1)
	go func() {
		<-delivery.handBacksLanded()
		confirmed <- d.handBacksSettled()
	}()
	return confirmed
}

// waitFinalizers waits for hand-backs a stopped durable left running to settle after its NATS
// client closes. They can then no longer wait for that client, and release their holds through the
// still-open database pool before its caller closes it (StartIntakes).
func (d *heldDurable) waitFinalizers() { d.finalizers.Wait() }

// releaseHold releases hold, or lapses it when the next holder must read a take-over's token.
func (d *heldDurable) releaseHold(hold *duty.Hold, lapse bool) {
	release := hold.Release
	if lapse {
		release = hold.Lapse
	}
	releasing, cancel := context.WithTimeout(context.Background(), intakeHoldRenew)
	defer cancel()
	if err := release(releasing); err != nil {
		d.tuning.log.Error("dispatch delivery: release the intake's hold", "name", d.name, "error", err)
	}
}

// handBacksReached reports whether every hand-back delivery published has reached the server. A
// publish NATS had not taken when the stop went on without it (intakeDelivery.publishHandBack) is
// waited for first, until ctx ends or hold is lost: the stop can return with one still waiting, since
// its drain ends once nats.go has answered the drain's flush and run the messages it had buffered,
// which need not wait for the write that holds nats.go's lock. Then the connection is flushed, a round
// trip to the server, so the hand-backs nats.go had queued have reached it (flushHandBacks). The stop
// sealed delivery before this reads it (intakeDelivery.seal), so no hand-back it does not count can
// begin afterwards: a subscription callback the stop's unsubscribe went on without is either counted
// here or withheld.
//
// A delivery that handed nothing back reports true at once only while the connection carries no
// hand-back a flush has not confirmed (bus.Client.CarriesUnconfirmed). A take-over the intake made
// again under the same hold -- after a create or a token write that failed, or a check that found the
// durable elsewhere -- leaves an earlier, stopped delivery, whose hand-back can still sit in the
// socket's kernel send queue; the release waits for it as for the latest delivery's own, since the
// flush confirms only once every marked hand-back of every delivery has been taken and reached the
// server (bus.Client.ConfirmMarked).
func (d *heldDurable) handBacksReached(ctx context.Context, delivery *intakeDelivery, hold *duty.Hold) bool {
	if delivery == nil || !delivery.handedBack() {
		return !d.nats.CarriesUnconfirmed() || d.flushHandBacks()
	}
	landed := delivery.handBacksLanded()
	select {
	case <-landed:
	case <-hold.Lost():
	case <-ctx.Done():
	}
	select {
	case <-landed:
	default:
		return false
	}
	return d.flushHandBacks()
}

// flushHandBacks is handBacksSettled within intakeFlushWait, on a bound of its own as the release
// that follows it is: both are part of what follows the stop's deadline (intakeStopBudget), and a
// healthy link answers the flush in a round trip. A flush that has not answered by then is false.
func (d *heldDurable) flushHandBacks() bool {
	flushing, cancel := context.WithTimeout(context.Background(), intakeFlushWait)
	defer cancel()
	settled, _, _ := bus.Bounded(flushing, nil, func() (bool, error) { return d.handBacksSettled(), nil })
	return settled
}

// handBacksSettled reports whether no hand-back nats.go had taken can reach the server later
// (bus.Client.ConfirmMarked): a flush answered within intakeFlushWait on the socket each was written
// to, or on a newer one after the older was reset, so each has reached the server or was discarded;
// or the connection has closed, which reset every socket that carried an unconfirmed hand-back.
func (d *heldDurable) handBacksSettled() bool {
	return d.nats.ConfirmMarked(intakeFlushWait)
}

// intakeFlushWait bounds each flush that confirms a stop's hand-backs reached the server before its
// hold is released (handBacksSettled).
const intakeFlushWait = time.Second
