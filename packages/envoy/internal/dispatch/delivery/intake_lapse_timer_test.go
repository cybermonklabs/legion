package delivery

// The standby's lapse timer (heldDurable.run): a standby that finds the durable's hold held live
// arms a timer to the lapse its read found, and tries for the hold again when it fires. The rows
// here hold the two things asked of that timer: it never displaces a holder that holds the hold
// live, and it fires nothing once the standby has stopped or has taken the hold another way.

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// armedLeft is how long the hold has left as the standby of
// TestAHolderThatRenewsBetweenAStandbysReadAndItsLapseTimerKeepsTheHold reads it, in the read its
// lapse timer is armed from. Every read of a hold its holder still holds finds more than
// intakeHoldMargin left, since the holder reports its hold lost that long before the hold lapses,
// and intakeHoldMargin is not less than durableBindInterval: the standby's next poll therefore
// comes before the timer fires, and the row fails that poll's read. armedLeft is far enough short
// of two intervals that the timer fires before the poll after that one.
const armedLeft = 3500 * time.Millisecond

// lapseSlack is how far the rows of this file hold each moment they force from the ones beside it:
// the reads that fail end that long before the lapse timer is due, and a read the row counts as a
// poll's, or as the timer's, is that close to the moment it is due at most.
const lapseSlack = 250 * time.Millisecond

// A holder whose renewal lands between a standby's read of its hold and the moment the lapse timer
// that read armed fires keeps its hold. The timer fires and the standby attempts: the attempt reads
// the hold again, finds it live with the time the renewal gave it, stands by, and arms to that.
// One process holds the hold throughout, and it is the holder: no statement of the standby's
// takes the hold, the hold's row names the holder with its token unchanged, the holder's hold
// never reaches its loss deadline, the durable still delivers to the holder's subject, and the
// holder handles a message published afterwards.
//
// The ordering is forced. The holder's lease pool holds its renewals from one that landed
// (heldRenewals), so its hold runs down; the standby's run starts so that a poll of it reads the
// hold armedLeft before its lapse; and the row then lets the one renewal waiting go, which lands
// after that read's answer and before the lapse, and before the holder's own loss deadline. The
// holder's renewals after that one wait again until the standby's next poll has read, so the
// timer's read finds the time that one renewal gave the hold. The standby's next poll would read
// the renewed hold before the timer fires and arm to it (armedLeft), so the row fails that poll's
// read (holdReads), and the timer keeps the lapse the read before it found, as run says a failed
// read does. The row fails rather than prove nothing when any of that missed: the read found
// other than armedLeft, the renewal landed outside the window, no read failed between, or the
// first read after the failed one is the next poll's and not the timer's.
//
// What keeps the holder is the attempt's own read of the hold, and behind it the take's condition
// in Postgres (duty.TryHold, which takes a row only once it has lapsed): a standby whose timer
// path ignores what its read found still takes nothing, its take affecting no row, and one that
// also takes without that condition takes the hold from the holder here, which this row fails.
func TestAHolderThatRenewsBetweenAStandbysReadAndItsLapseTimerKeepsTheHold(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holder := newTakeOverProcess(t, pool, natsURL)
	renewals := &heldRenewals{DurableLeases: holder.intake.leases}
	holder.intake.leases = renewals
	startIntake(t, holder.intake)
	// Registered after the holder's run, so it runs before the run's stop: a stop whose release
	// waited on the gate would wait out the release's own deadline.
	t.Cleanup(renewals.release)
	awaitBoundDurable(t, publisher)
	held, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable the holder bound: %v", err)
	}
	var delivering *intakeDelivery
	waitFor(t, 10*time.Second, func() bool {
		delivering = subscribedDelivery(holder.intake)
		return delivering != nil
	}, func() string { return "the holder bound the durable with no delivery of its own" })
	// The take-over records the holder's subject as the hold's token as its last step, a moment
	// after the durable is bound (heldDurable.finishTakeOver).
	owner := holdOwner{holder: holder.intake.holder.String(), token: held.Config.DeliverSubject}
	waitFor(t, 10*time.Second, func() bool { return readHoldOwner(t, ctx, pool) == owner }, func() string {
		return fmt.Sprintf("the hold's row names %+v, want the holder's %+v", readHoldOwner(t, ctx, pool), owner)
	})

	standby := newTakeOverProcess(t, pool, natsURL)
	reads := &holdReads{DurableLeases: standby.intake.leases}
	standby.intake.leases = reads

	landed := len(landedExecs(renewals.noted()))
	renewed := awaitRenewal(t, ctx, pool, readHold(t, ctx, pool), lostReplyPoll)
	renewals.hold()
	// The renewal the row saw land is noted as its Exec returns, a moment after it commits.
	waitFor(t, 5*time.Second, func() bool { return len(landedExecs(renewals.noted())) > landed }, func() string {
		return fmt.Sprintf("the renewal the row saw land was not noted: %+v", renewals.noted())
	})
	confirmed := landedExecs(renewals.noted())
	deadline := confirmed[len(confirmed)-1].sent.Add(intakeHoldTTL - intakeHoldMargin)

	aim := renewed.lapse.Add(-armedLeft)
	start := aim
	for time.Until(start) > durableBindInterval {
		start = start.Add(-durableBindInterval)
	}
	time.Sleep(time.Until(start))
	startIntake(t, standby.intake)

	armed := reads.awaitRead(t, aim.Add(-durableBindInterval/2), 2*intakeHoldTTL, "poll aimed armedLeft before the lapse")
	lapses := armed.answered.Add(armed.left)
	reads.failWhile(func(sent time.Time) bool { return sent.After(armed.answered) && sent.Before(lapses.Add(-lapseSlack)) })
	renewals.releaseWaiting()
	var renewal leaseExec
	waitFor(t, intakeHoldRenew, func() bool {
		noted := renewals.noted()
		i := slices.IndexFunc(noted, func(e leaseExec) bool { return e.forwarded.After(armed.answered) })
		if i >= 0 {
			renewal = noted[i]
		}
		return i >= 0
	}, func() string {
		return fmt.Sprintf("the holder's renewal the row let go was not answered: %+v", renewals.noted())
	})
	renewedAgain := readHold(t, ctx, pool)

	fired := reads.awaitRead(t, lapses.Add(-lapseSlack), 2*durableBindInterval, "read after the poll the row failed")
	allReads, _ := reads.noted()
	var failed []time.Time
	for _, read := range allReads {
		if read.sent.After(armed.answered) && read.sent.Before(lapses.Add(-lapseSlack)) {
			if read.err != errCutOff {
				t.Fatalf("the standby's read at %s, between the read its timer was armed from and the timer, did not fail: %+v", read.sent, read)
			}
			failed = append(failed, read.sent)
		}
	}
	nextPoll := armed.sent.Add(2 * durableBindInterval)
	switch {
	case armed.err != nil || armed.left < armedLeft-lapseSlack || armed.left > armedLeft+lapseSlack:
		t.Fatalf("the row did not force the ordering: the standby's poll read the hold with %s left (error %v), want %s within %s",
			armed.left, armed.err, armedLeft, lapseSlack)
	case renewal.err != nil || renewal.rows != 1 || !renewal.answered.Before(lapses) || !renewal.answered.Before(deadline):
		t.Fatalf("the row did not force the ordering: the holder's renewal left for Postgres %s after the standby's read was answered and was answered %s before the lapse the read found and %s before the holder's loss deadline, affecting %d rows (error %v); want it answered before both, affecting the holder's row",
			renewal.forwarded.Sub(armed.answered), lapses.Sub(renewal.answered), deadline.Sub(renewal.answered), renewal.rows, renewal.err)
	case len(failed) == 0:
		t.Fatalf("the row did not force the ordering: no read of the standby's went out between the read its timer was armed from (answered %s) and the timer, so none failed", armed.answered)
	case fired.sent.Before(lapses) || !fired.sent.Before(nextPoll.Add(-lapseSlack)):
		t.Fatalf("the row did not force the ordering: the standby's first read after the failed poll went out %s after the lapse its timer was armed for and %s before its next poll; want its lapse timer's, after the lapse and before that poll",
			fired.sent.Sub(lapses), nextPoll.Sub(fired.sent))
	}

	// Past the standby's next poll, so whatever the timer's attempt went on to do has been done;
	// then the holder's renewals go on as they would have.
	time.Sleep(time.Until(nextPoll.Add(lapseSlack)))
	renewals.release()
	_, execs := reads.noted()
	for _, exec := range execs {
		if exec.rows > 0 {
			t.Fatalf("two holders: the standby's statement at %s, %s after its lapse timer fired, affected %d rows: it took the hold from the holder, which by its own clock held it %s longer (its renewal landed %s before the lapse); the hold's row now names %+v, the holder is %s",
				exec.sent, exec.sent.Sub(fired.sent), exec.rows, renewal.sent.Add(intakeHoldTTL-intakeHoldMargin).Sub(exec.answered),
				lapses.Sub(renewal.answered), readHoldOwner(t, ctx, pool), holder.intake.holder)
		}
	}
	if now := readHoldOwner(t, ctx, pool); now != owner {
		t.Fatalf("the hold was displaced: its row names %+v, want the holder's %+v", now, owner)
	}
	if want := renewedAgain.lapse.Sub(fired.answered); fired.err != nil || fired.left < want-lapseSlack || fired.left > want+lapseSlack {
		t.Fatalf("the standby's lapse timer's read found the hold with %s left (error %v), want the time the holder's renewal gave it, %s",
			fired.left, fired.err, want)
	}
	if subscribedDelivery(standby.intake) != nil {
		t.Fatal("the standby subscribed to the durable the holder holds")
	}
	if subscribedDelivery(holder.intake) != delivering || !delivering.holdCovers() || !delivering.hold.Deadline().After(deadline) {
		t.Fatalf("the holder no longer holds its hold: its delivery is the one it bound with %t, its loss deadline %s, %s past the one before its renewal",
			subscribedDelivery(holder.intake) == delivering, delivering.hold.Deadline(), delivering.hold.Deadline().Sub(deadline))
	}
	info := awaitPushBound(t, publisher, "", "the durable the holder holds is bound to nobody")
	if info.Config.DeliverSubject != held.Config.DeliverSubject {
		t.Fatalf("the durable delivers to %s, want the holder's subject %s", info.Config.DeliverSubject, held.Config.DeliverSubject)
	}
	publishMergedPullRequest(t, publisher, 41)
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.Config.DeliverSubject == held.Config.DeliverSubject && info.AckFloor.Stream > held.AckFloor.Stream && info.NumAckPending == 0
	}, func() string {
		return "the holder did not handle a message published once the standby's timer had fired: " + consumerState(publisher)
	})

	var sent []string
	for _, exec := range execs {
		sent = append(sent, fmt.Sprintf("%s after the timer's read, affecting %d rows", exec.sent.Sub(fired.sent), exec.rows))
	}
	t.Logf("the standby's poll read the hold %s before its lapse with %s left; the holder's renewal left for Postgres %s after that read's answer and was answered %s before the lapse and %s before the holder's loss deadline; the standby's poll at %s after the read failed its read; its lapse timer's read went out %s after the lapse and found %s left; the standby sent %d statements through Exec %v",
		lapses.Sub(armed.sent), armed.left, renewal.forwarded.Sub(armed.answered), lapses.Sub(renewal.answered), deadline.Sub(renewal.answered),
		durations(failed, armed.sent), fired.sent.Sub(lapses), fired.left, len(sent), sent)
}

// A standby that stops leaves no lapse timer to fire: it found the hold held live, armed its timer
// to the lapse, and stopped before that lapse. Its run returns, every goroutine it ran returns, and
// nothing of it reads the hold or makes an attempt at the lapse or after it.
func TestAStandbyThatStopsLeavesNoLapseTimerToFire(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	holdElsewhere(t, ctx, pool, 6*time.Second)
	standby := newTakeOverProcess(t, pool, natsURL)
	reads := &holdReads{DurableLeases: standby.intake.leases}
	standby.intake.leases = reads

	before := runtime.NumGoroutine()
	run := runNotingAttempts(t, standby.intake)
	first := reads.awaitRead(t, time.Time{}, 10*time.Second, "first read")
	// The timer the standby stops with is one its poll armed, not only its first attempt.
	armed := reads.awaitRead(t, first.sent.Add(durableBindInterval/2), 2*durableBindInterval, "first poll")
	lapses := armed.answered.Add(armed.left)
	run.stop()
	stopped := time.Now()
	if armed.err != nil || lapses.Before(stopped.Add(time.Second)) {
		t.Fatalf("the row did not force the ordering: the standby's poll found the hold with %s left (error %v), and the standby stopped %s before that lapse; want the hold live and a second at least",
			armed.left, armed.err, lapses.Sub(stopped))
	}
	time.Sleep(time.Until(lapses.Add(time.Second)))

	allReads, _ := reads.noted()
	for _, read := range allReads {
		if read.sent.After(stopped) {
			t.Fatalf("the standby read the hold %s after it stopped, %s from the lapse its timer was armed for", read.sent.Sub(stopped), read.sent.Sub(lapses))
		}
	}
	for _, at := range run.attempts() {
		if at.After(stopped) {
			t.Fatalf("the standby made an attempt %s after it stopped, %s from the lapse its timer was armed for", at.Sub(stopped), at.Sub(lapses))
		}
	}
	after := awaitGoroutinesBackTo(t, before)
	t.Logf("the standby armed its lapse timer for %s after it stopped; %s past that lapse it had read nothing and attempted nothing since its stop, and the process ran %d goroutines, %d before the standby's run",
		lapses.Sub(stopped), time.Since(lapses), after, before)
}

// A standby whose poll takes the hold before the lapse timer it armed fires leaves no timer to
// fire: once the hold is its own, no attempt of its comes at the lapse its earlier read found.
// Another process holds the hold live, the standby's first read finds it so and arms the timer, and
// the other process then lets the hold go, so the standby's next poll takes it. The durable is
// bound by a process of the build before holds, which the standby, holding the hold, waits for
// (heldDurable.boundElsewhere): it delivers nothing, so it makes an attempt, a lookup of the
// durable, at each of its polls, and would make one at the lapse too if the timer still fired. A
// standby that delivered would show nothing either way, since run attempts nothing while the
// intake delivers.
//
// The other process's hold lapses an odd number of seconds after the standby's first read, so the
// lapse falls halfway between two of the standby's polls after the take. The row fails rather than
// prove nothing when the take is not its next poll's, when the lapse is not between two polls, or
// when no poll came after the lapse.
func TestAStandbyWhosePollTakesTheHoldLeavesNoLapseTimerToFire(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	bindAsPreviousBuild(t, intakeTestClient(t))

	standby := newTakeOverProcess(t, pool, natsURL)
	reads := &holdReads{DurableLeases: standby.intake.leases}
	standby.intake.leases = reads
	letGo := holdElsewhere(t, ctx, pool, 5*time.Second)
	run := runNotingAttempts(t, standby.intake)

	armed := reads.awaitRead(t, time.Time{}, 10*time.Second, "first read")
	lapses := armed.answered.Add(armed.left)
	letGo()
	waitFor(t, 2*durableBindInterval, func() bool {
		_, execs := reads.noted()
		return slices.ContainsFunc(execs, func(e leaseExec) bool { return e.rows == 1 })
	}, func() string { return "the standby's poll did not take the hold the other process let go" })
	// Past the lapse and the poll after it.
	time.Sleep(time.Until(lapses.Add(durableBindInterval/2 + lapseSlack)))

	attempts := run.attempts()
	_, execs := reads.noted()
	took := execs[slices.IndexFunc(execs, func(e leaseExec) bool { return e.rows == 1 })]
	poll := func(at time.Time) time.Duration {
		off := at.Sub(attempts[0]) % durableBindInterval
		return min(off, durableBindInterval-off)
	}
	switch {
	case armed.err != nil || armed.left <= 0:
		t.Fatalf("the row did not force the ordering: the standby's first read found the hold with %s left (error %v), want it live", armed.left, armed.err)
	case len(attempts) < 2 || poll(attempts[1]) > lapseSlack || attempts[1].Sub(attempts[0]) > durableBindInterval+lapseSlack ||
		took.sent.Before(attempts[1]) || took.sent.Sub(attempts[1]) > lapseSlack:
		t.Fatalf("the row did not force the ordering: the standby's take went out %s after its first attempt, want at its next poll; its attempts came at %v from the first",
			took.sent.Sub(attempts[0]), durations(attempts, attempts[0]))
	case !lapses.After(took.sent.Add(time.Second)) || poll(lapses) < durableBindInterval/2-lapseSlack:
		t.Fatalf("the row did not force the ordering: the lapse the standby's timer was armed for came %s after its take and %s from its nearest poll, want a second after the take at least and halfway between two polls",
			lapses.Sub(took.sent), poll(lapses))
	case !slices.ContainsFunc(attempts, func(at time.Time) bool { return at.After(lapses) }):
		t.Fatalf("the row did not force the ordering: the standby made no attempt after the lapse its timer was armed for; its attempts came at %v from the first", durations(attempts, attempts[0]))
	}
	for _, at := range attempts {
		if at.After(took.sent) && poll(at) > lapseSlack {
			t.Fatalf("the standby's lapse timer fired after its poll took the hold: it made an attempt %s from the lapse its first read found, %s off its polls, %s after the take; its attempts came at %v from the first",
				at.Sub(lapses), poll(at), at.Sub(took.sent), durations(attempts, attempts[0]))
		}
	}
	if owner := readHoldOwner(t, ctx, pool); owner.holder != standby.intake.holder.String() {
		t.Fatalf("the hold's row names %+v, want the standby %s that took it", owner, standby.intake.holder)
	}
	if subscribedDelivery(standby.intake) != nil {
		t.Fatal("the standby subscribed to the durable a process of the build before holds is bound to, so its attempts after the take say nothing of the timer")
	}
	t.Logf("the standby's poll took the hold %s after its first attempt; the lapse its timer was armed for came %s after the take, %s from its nearest poll, and its attempts came at %v from the first",
		took.sent.Sub(attempts[0]), lapses.Sub(took.sent), poll(lapses), durations(attempts, attempts[0]))
}

// heldRenewals is a holder's lease pool whose statements through Exec wait, from hold until
// release, for the row to let them go, each until its own context ends at the latest, as a
// statement Postgres has not answered waits. Once the holder has taken its durable over, those
// statements are its hold's renewals alone (duty.Hold.renew). It notes each statement (leaseExec).
type heldRenewals struct {
	DurableLeases

	mu    sync.Mutex
	gate  chan struct{}
	execs []leaseExec
}

func (r *heldRenewals) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	noted := leaseExec{sent: time.Now()}
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			noted.answered, noted.err = time.Now(), ctx.Err()
			r.note(noted)
			return pgconn.CommandTag{}, ctx.Err()
		}
	}
	noted.forwarded = time.Now()
	tag, err := r.DurableLeases.Exec(ctx, sql, arguments...)
	noted.answered, noted.rows, noted.err = time.Now(), tag.RowsAffected(), err
	r.note(noted)
	return tag, err
}

func (r *heldRenewals) note(exec leaseExec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, exec)
}

// hold makes every statement sent from here on wait until release.
func (r *heldRenewals) hold() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate == nil {
		r.gate = make(chan struct{})
	}
}

// releaseWaiting lets every statement waiting go to Postgres, and holds every later one until the
// next release.
func (r *heldRenewals) releaseWaiting() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate != nil {
		close(r.gate)
		r.gate = make(chan struct{})
	}
}

// release lets every statement waiting go to Postgres, and every later one straight through.
func (r *heldRenewals) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
}

// noted is every statement answered so far, in the order of their answers.
func (r *heldRenewals) noted() []leaseExec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.execs)
}

// landedExecs is each of execs that affected a row, in order.
func landedExecs(execs []leaseExec) []leaseExec {
	return slices.DeleteFunc(execs, func(exec leaseExec) bool { return exec.err != nil || exec.rows == 0 })
}

// holdOwner is the holder and token the delivery durable's hold's row records.
type holdOwner struct {
	holder string
	token  string
}

// readHoldOwner reads who the delivery durable's hold's row names, and its token.
func readHoldOwner(t *testing.T, ctx context.Context, pool *store.Pool) holdOwner {
	t.Helper()
	var owner holdOwner
	if err := pool.QueryRow(ctx, `select holder::text, token from duty_holds where name = $1`, intakeHold).Scan(&owner.holder, &owner.token); err != nil {
		t.Fatalf("read who the hold's row names: %v", err)
	}
	return owner
}

// holdElsewhere writes the delivery durable's hold as another process's, live for left on the
// database's clock, and answers the deletion of that row, as that process's release deletes it.
func holdElsewhere(t *testing.T, ctx context.Context, pool *store.Pool, left time.Duration) (letGo func()) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into duty_holds (name, holder, token, expires_at) values ($1, $2, '', now() + make_interval(secs => $3))`,
		intakeHold, uuid.New(), left.Seconds()); err != nil {
		t.Fatalf("write the durable's hold as another process's: %v", err)
	}
	return func() {
		if _, err := pool.Exec(ctx, `delete from duty_holds where name = $1`, intakeHold); err != nil {
			t.Fatalf("let the other process's hold go: %v", err)
		}
	}
}

// bindAsPreviousBuild creates the delivery durable and binds client to it, as a process of the
// build before holds does: a binding no hold records, which an intake holding the hold waits for
// (heldDurable.boundElsewhere).
func bindAsPreviousBuild(t *testing.T, client *bus.Client) {
	t.Helper()
	config := deliveryConsumerConfig(tuningFor(t))
	if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the durable a process of the build before holds binds: %v", err)
	}
	bound, err := client.Subscribe(config.FilterSubject, func(*natsgo.Msg) {}, natsgo.Bind(bus.Stream, config.Durable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the durable as a process of the build before holds: %v", err)
	}
	t.Cleanup(func() { _ = bound.Unsubscribe() })
	awaitPushBound(t, client, "", "the durable is not bound to the process of the build before holds")
}

// notedRun is one intake's run, started by the row, with the moment of each attempt it makes while
// it delivers nothing noted: run calls the intake's attemptHook before every such attempt, whatever
// started it -- the first, a poll, the lapse timer (heldDurable.run) -- so a hook that notes the
// moment notes them all.
type notedRun struct {
	mu   sync.Mutex
	at   []time.Time
	stop func()
}

// runNotingAttempts runs intake by Intake.Run, noting its attempts, until the test ends or its stop
// is called; the stop returns once the run has. Its callers' intakes (newTakeOverProcess) route by a
// settings cache that has made its first read (loadedSettingsCache), so Run starts its run at once.
func runNotingAttempts(t *testing.T, intake *Intake) *notedRun {
	t.Helper()
	noted := &notedRun{}
	intake.attemptHook = func() {
		noted.mu.Lock()
		noted.at = append(noted.at, time.Now())
		noted.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(ctx)
	}()
	noted.stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(noted.stop)
	return noted
}

// attempts is when each attempt was made, in order.
func (r *notedRun) attempts() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.at)
}

// awaitGoroutinesBackTo waits up to five seconds for this process to run no more goroutines than
// before and none that runNotingAttempts started, and answers how many it runs; the row fails
// with every goroutine's stack otherwise.
func awaitGoroutinesBackTo(t *testing.T, before int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buffer := make([]byte, 1<<22)
	for {
		n := runtime.NumGoroutine()
		late := time.Now().After(deadline)
		if n <= before || late {
			stacks := buffer[:runtime.Stack(buffer, true)]
			running := strings.Contains(string(stacks), "runNotingAttempts")
			if n <= before && !running {
				return n
			}
			if late {
				t.Fatalf("five seconds on, this process still runs %d goroutines, %d before the standby's run, the standby's run among them %t:\n%s",
					runtime.NumGoroutine(), before, running, stacks)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// durations is each of times as the time since from.
func durations(times []time.Time, from time.Time) []time.Duration {
	since := make([]time.Duration, len(times))
	for i, at := range times {
		since[i] = at.Sub(from)
	}
	return since
}
