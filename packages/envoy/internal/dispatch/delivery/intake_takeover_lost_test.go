package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lostReplyPoll is how often these rows read the hold, the standby's connection and the durable:
// fine enough that what they measure is the take-over and not their own polling.
const lostReplyPoll = 5 * time.Millisecond

// A holder that dies is replaced within the bound held_durable.go states, intakeHoldTTL plus one
// durableBindInterval, when the standby's take-over loses a JetStream reply with its connection.
// The standby's link runs through a relay that drops the take-over's read of the durable under the
// consumer lease, and the row then cuts that link: nats.go fails no request its connection drops,
// so without the connection watch the read waited out the JetStream context's 10 s, the attempt
// ended on it, and the next poll took the durable over 20 s or more after the holder died.
//
// The holder dies halfway through a renewal period, as a kill lands anywhere in one, so its hold
// lapses 10.5 to 12.5 s after the death and the standby tries again at the lapse
// (heldDurable.run). The row's own cut, which keeps the standby off NATS for nats.go's reconnect
// wait (about 1 s), counts against the bound like every other moment from the death to the
// durable bound. The worst phase, a death the instant after a renewal, whose hold lasts the whole
// intakeHoldTTL from the death, is TestADeadHolderIsReplacedWithinTheStatedBoundAtTheWorstPhase's.
func TestATakeOverThatLosesAReplyWithItsConnectionReplacesADeadHolderWithinTheStatedBound(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holder := startIntakeProcess(t, pool, natsURL, nil)
	awaitBoundDurable(t, publisher)
	held, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable the holder bound: %v", err)
	}
	relay := testnats.StartUnansweredRelayOn(t, natsURL, "$JS.API.CONSUMER.INFO."+bus.Stream+"."+bus.DispatchDeliveryDurable)
	standby := startIntakeProcess(t, pool, relay.URL(), nil)
	// While the holder lives the standby stands by at the hold and reads nothing of the durable.
	// Once it holds the hold, its first read is the lookup outside the consumer lease, which the
	// relay passes, and its second the take-over's read under the lease, which the relay drops.
	relay.PassThenDrop(1, 1)

	renewal := awaitHalfwayThroughARenewal(t, ctx, pool)
	abandoned := time.Now()
	holder.leases.cut.Store(true)
	holder.client.Close()

	waitFor(t, intakeHoldTTL+2*durableBindInterval, func() bool { return relay.Sent() >= 2 }, func() string {
		return "the standby did not send the take-over's read of the durable: " + consumerState(publisher)
	})
	conn := standby.client.Connection()
	before := conn.Stats().Reconnects
	cut := time.Now()
	relay.Drop()
	for conn.Stats().Reconnects == before {
		if time.Since(cut) > 10*time.Second {
			t.Fatal("the standby's connection did not reconnect through the relay within 10s")
		}
		time.Sleep(lostReplyPoll)
	}

	took := awaitTakenOver(t, publisher, held, abandoned, renewal.lapse).Sub(abandoned)
	if bound := intakeHoldTTL + durableBindInterval; took > bound {
		t.Fatalf("the standby took the durable over %s after the holder died, with one reply of its take-over lost with its connection; want within intakeHoldTTL plus one durableBindInterval (%s)",
			took.Round(time.Millisecond), bound)
	}
	t.Logf("the standby took the durable over %s after the holder died", took.Round(time.Millisecond))
}

// awaitHalfwayThroughARenewal waits until the delivery durable's hold was last renewed at least
// half a renewal period ago (intakeHoldRenew) and its next renewal is still half a second off, as
// the hold's row on the database's clock says, and answers the row it read then. It reads the row
// every 100 ms: that window is two seconds wide, so the read that finds the hold inside it still
// leaves the death the row deals next mid-renewal.
func awaitHalfwayThroughARenewal(t *testing.T, ctx context.Context, pool *store.Pool) holdRow {
	t.Helper()
	deadline := time.Now().Add(2 * intakeHoldTTL)
	for {
		row := readHold(t, ctx, pool)
		if row.left <= intakeHoldTTL-intakeHoldRenew/2 && row.left >= intakeHoldTTL-intakeHoldRenew+500*time.Millisecond {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder's hold had %s left at the last read; it never came halfway through a renewal", row.left)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// takenOverSlack is how long before the dead holder's hold lapses awaitTakenOver starts reading
// the durable: far more than a read of the hold's row errs by, so no take-over finishes before its
// first read.
const takenOverSlack = 250 * time.Millisecond

// awaitTakenOver waits for the durable to be push-bound to a deliver subject other than held's,
// and answers when it first read it so: the take-over's completion, which a holder that died at
// abandoned has not seen within 40 s fails. No take-over finishes before lapse, when the dead
// holder's hold lapses, so it reads nothing until takenOverSlack before then, and reads the
// durable every lostReplyPoll from there.
func awaitTakenOver(t *testing.T, publisher *bus.Client, held *natsgo.ConsumerInfo, abandoned, lapse time.Time) time.Time {
	t.Helper()
	time.Sleep(time.Until(lapse.Add(-takenOverSlack)))
	for {
		if time.Since(abandoned) > 40*time.Second {
			t.Fatalf("the standby has not taken the durable over 40s after the holder died: %s", consumerState(publisher))
		}
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err == nil && info.PushBound && info.Config.DeliverSubject != held.Config.DeliverSubject {
			return time.Now()
		}
		time.Sleep(lostReplyPoll)
	}
}

// worstPhaseLead is how long before the hold's lapse the worst-phase row lets the standby's poll
// go out (holdReads.holdUntil), and half the window it checks that poll went out in. A poll let go
// at the lapse itself could reach Postgres after it -- the row reads the lapse off the hold's row,
// to within half that read's round trip, and the poll's statement takes its own time to arrive --
// and so take the hold at once, which is the best phase and not the worst. Aimed this far ahead,
// it went out 5 to 13 ms before the lapse in the runs held_durable.go records, the last poll to
// find the hold live.
const worstPhaseLead = 10 * time.Millisecond

// worstPhaseEarly is how long before the moment the worst-phase row lets the standby's poll go
// out (worstPhaseLead ahead of the lapse) it puts that poll's tick, by when it starts the
// standby's run. It is far more than a loaded machine delays a tick -- about 12 ms, in one run of
// sixty -- so the poll comes before that moment and waits there, rather than going out past the
// lapse and taking the hold at once; and far less than the standby's read of the hold may wait for
// its answer (intakeHoldRenew, heldDurable.takeHold), so the wait costs that read nothing.
const worstPhaseEarly = 100 * time.Millisecond

// worstPhasePoll is how often the worst-phase row's standby polls (heldDurable.bindEvery): three
// durableBindIntervals. In production the standby polls every durableBindInterval, so without the
// lapse timer its next try would start about 16.99 s after the death, 10 ms inside the 17 s bound,
// and the take-over's own work then carried it past the bound by milliseconds and not in every run:
// the timer is what gives the bound its margin, and this poll is there so that a missing timer
// shows. While the lapse timer works it changes nothing the row measures: the poll before the lapse
// still finds the hold live, the timer's attempt at the lapse takes the durable over, and every
// later poll finds the standby delivering and does nothing. With no lapse timer the standby's next
// try is its next poll, worstPhasePoll after the tick of the one before the lapse, which comes
// worstPhaseEarly ahead of the moment the row lets that one go: 15 s - 10 ms - 100 ms + 6 s =
// 20.89 s after the death, 3.89 s past the 17 s bound before the take-over has sent anything. The
// row's start then falls 1.89 s after the renewal it reads (20 s - 110 ms less three polls), before
// the renewal the holder dies after.
const worstPhasePoll = 3 * durableBindInterval

// A holder that dies is replaced within the bound held_durable.go states at the worst phase there
// is, with no reply lost: the holder dies the instant after a renewal landed, so its hold lasts the
// whole intakeHoldTTL from the death, and the standby's poll goes out just before the lapse, so it
// is the last poll to find the hold live. The standby tries again at the lapse that poll read
// (heldDurable.run). held_durable.go records what twenty runs of this row measured, and what ten
// measured with no lapse timer, when the standby's next try was its next poll, worstPhasePoll
// after this one.
//
// Two hooks in the two processes' lease pools place part of the phase, and timing margins the
// rest. Once the standby runs, the row arms the holder's pool to cut itself and close the holder's
// NATS client inside the Exec of the hold's next renewal, the instant Postgres has answered it
// (cuttableLeases.afterNextRenewal): the hold's row, read then, says when that renewal landed and
// when the hold lapses. The row then has the standby's pool hold each read of the hold that goes
// out within worstPhasePoll before the moment worstPhaseLead ahead of that lapse until that moment,
// and let it go then (holdReads.holdUntil). So the hooks fix which renewal the holder dies after,
// the instant its answer returns, and when a held read goes to Postgres: at the moment.
//
// They do not fix whether the standby's poll is the read held. The standby polls every
// worstPhasePoll from when its run starts, so the row reads a renewal off the hold's row and spins
// until a whole number of polls before worstPhaseEarly ahead of the moment the next renewal sets,
// and starts that run then (newTakeOverProcess builds it unstarted). One of its ticks then comes
// worstPhaseEarly (100 ms) before the moment, inside holdUntil's window, and the tick before it
// worstPhaseEarly before the window opens: a tick more than 100 ms late, or a next renewal that
// lands more than 100 ms sooner than intakeHoldRenew after the one the row read, misses the phase.
// Nor do they fix what that read finds: it finds the hold live only when it reaches Postgres within
// worstPhaseLead (10 ms) of the moment, before the lapse, and the holder's death is the instant
// after its renewal only when that renewal's answer returned within worstPhaseLead of its landing.
// The standby's next poll comes worstPhasePoll after the held tick.
//
// The row checks that the phase was met: the holder died within worstPhaseLead of its renewal, the
// standby's last read before the lapse went out within twice worstPhaseLead of it and found the
// hold live, and the next read found the hold free and took it, which makes it the standby's last,
// since the standby holds the hold from then on. A miss of either, of what the hooks place or of
// what rests on timing, is a miss of the row's own setup, never a pass and never a breach of the
// bound: the try ends there, and the row sets the phase up again from the start, on a fresh
// holder, standby, database and NATS account (worstPhaseTry, one subtest each), up to
// worstPhaseTries times. It fails as UNARRANGED, with what each try observed, only when every try
// missed. The bound is asserted on the first try that met the phase, and a take-over past it fails
// the row at once, with no further try. Each run logs the try that met the phase.
func TestADeadHolderIsReplacedWithinTheStatedBoundAtTheWorstPhase(t *testing.T) {
	var misses []string
	for try := 1; try <= worstPhaseTries; try++ {
		met, missed := false, false
		if !t.Run(fmt.Sprintf("try %d", try), func(t *testing.T) {
			miss, arranged := worstPhaseTry(t)
			if !arranged {
				missed = true
				misses = append(misses, fmt.Sprintf("try %d: %s", try, miss))
				t.Skipf("the try missed the worst phase: %s", miss)
			}
			met = true
		}) {
			return
		}
		if met {
			t.Logf("the worst phase was met on try %d of %d", try, worstPhaseTries)
			return
		}
		if !missed {
			// The try skipped itself, as its database does with no DISPATCH_TEST_DATABASE_URL.
			t.Skip("the try skipped itself before it set the phase up")
		}
	}
	t.Fatalf("UNARRANGED: every one of the %d tries missed the worst phase, so the row never reached its precondition:\n%s", worstPhaseTries, strings.Join(misses, "\n"))
}

// worstPhaseTries is how many times the worst-phase row sets its phase up before it fails as
// UNARRANGED. A try misses when a tick, a renewal or a read lands outside the row's margins
// (worstPhaseLead, worstPhaseEarly), which a loaded run makes likelier, the race detector's above
// all; a miss says nothing of the take-over, so another try costs the row nothing but its time,
// about intakeHoldTTL each.
const worstPhaseTries = 5

// worstPhaseTry is one try of the worst-phase row, on a fresh holder, standby, database and NATS
// account: it reports whether the phase was met and, when it was not, what the try observed. A try
// that met the phase asserts the bound, and fails t when the take-over came past it.
func worstPhaseTry(t *testing.T) (miss string, arranged bool) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holder := startIntakeProcess(t, pool, natsURL, nil)
	awaitBoundDurable(t, publisher)
	held, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable the holder bound: %v", err)
	}
	standby := newTakeOverProcess(t, pool, natsURL)
	reads := &holdReads{intakeLeases: standby.intake.leases}
	standby.intake.leases = reads
	standby.intake.bindEvery = worstPhasePoll

	renewed := awaitRenewal(t, ctx, pool, readHold(t, ctx, pool), lostReplyPoll)
	start := renewed.lapse.Add(intakeHoldRenew - worstPhaseLead - worstPhaseEarly)
	for time.Until(start) > worstPhasePoll {
		start = start.Add(-worstPhasePoll)
	}
	spinUntil(start)
	startIntake(t, standby.intake)

	death := make(chan time.Time, 1)
	holder.leases.afterNextRenewal(func() {
		at := time.Now()
		holder.leases.cut.Store(true)
		holder.client.Close()
		death <- at
	})
	var abandoned time.Time
	select {
	case abandoned = <-death:
	case <-time.After(2 * intakeHoldRenew):
		return fmt.Sprintf("no renewal of the holder's hold landed within %s of the standby's start, so the holder never died the instant after one", 2*intakeHoldRenew), false
	}
	last := readHold(t, ctx, pool)
	reads.holdUntil(last.lapse.Add(-worstPhaseLead), worstPhasePoll)

	took := awaitTakenOver(t, publisher, held, abandoned, last.lapse).Sub(abandoned)
	died := abandoned.Sub(last.landed())
	noted, _ := reads.noted()
	live := -1
	for i, read := range noted {
		if read.forwarded.Before(last.lapse) {
			live = i
		}
	}
	if died > worstPhaseLead || live < 0 || live != len(noted)-2 ||
		noted[live].err != nil || noted[live].left <= 0 || last.lapse.Sub(noted[live].forwarded) > 2*worstPhaseLead ||
		noted[live+1].err != nil || noted[live+1].left > 0 {
		return fmt.Sprintf("the holder died %s after its last renewal landed, and the standby's reads of the hold, from the lapse: %s; want the holder dead within %s of its last renewal, the standby's last read before the lapse going out within %s of it and finding the hold live, and the next read, its last, finding the hold free; the standby took the durable over %s after the holder died",
			died, readsFrom(noted, last.lapse), worstPhaseLead, 2*worstPhaseLead, took), false
	}
	poll, next := noted[live], noted[live+1]
	t.Logf("worst phase forced: the holder died %s after its last renewal landed; the standby's poll, sent %s before the lapse, went out %s before it and found the hold live with %s left, and its next read, %s after the lapse, found the hold free and took it; the standby took the durable over %s after the holder died",
		died, last.lapse.Sub(poll.sent), last.lapse.Sub(poll.forwarded), poll.left, next.forwarded.Sub(last.lapse), took)
	bound := intakeHoldTTL + durableBindInterval
	if took > bound {
		t.Fatalf("the standby took the durable over %s after the holder died the instant after a renewal, its poll just before the lapse; want within intakeHoldTTL plus one durableBindInterval (%s)",
			took.Round(time.Millisecond), bound)
	}
	return "", true
}

// readsFrom is each of reads as a failure reports it, from at: when it went out, when the standby
// sent it if the row held it, and how long it found the hold had left, or its error.
func readsFrom(reads []holdRead, at time.Time) string {
	described := make([]string, len(reads))
	for i, read := range reads {
		described[i] = fmt.Sprintf("went out %s", read.forwarded.Sub(at))
		if read.forwarded.After(read.sent) {
			described[i] += fmt.Sprintf(" (sent %s)", read.sent.Sub(at))
		}
		if read.err != nil {
			described[i] += fmt.Sprintf(" and failed: %v", read.err)
		} else {
			described[i] += fmt.Sprintf(" and found %s left", read.left)
		}
	}
	return strings.Join(described, "; ")
}

// holdReads is a standby's lease pool that notes each of its reads of its durable's hold, the one
// statement it sends through QueryRow (duty.HoldToken): when the standby sent it, when it went to
// Postgres, when its answer was scanned, how long it found the hold had left, and its error. A
// read that goes out while fail reports true fails as a cut-off process's read does (cutOffRow). A
// read sent inside the window holdUntil sets waits until that window ends, and goes to Postgres
// then. It notes each statement it sends through Exec too, which while it holds no hold is its
// take (duty.TryHold).
type holdReads struct {
	intakeLeases

	mu    sync.Mutex
	reads []holdRead
	execs []leaseExec
	fail  func(sent time.Time) bool
	// heldFrom and heldUntil are the window holdUntil set, both zero until it does.
	heldFrom, heldUntil time.Time
}

// holdRead is one read of the hold as holdReads noted it: forwarded is when it went to Postgres,
// which is sent unless holdUntil held it, left is how long it found the hold had left, on the
// database's clock, and answered is zero until its answer is scanned.
type holdRead struct {
	sent, forwarded, answered time.Time
	left                      time.Duration
	err                       error
}

// leaseExec is one statement a process sent through its lease pool's Exec: when the process sent
// it, when it left for Postgres, when Postgres answered it, the rows it affected, and its error.
type leaseExec struct {
	sent, forwarded, answered time.Time
	rows                      int64
	err                       error
}

func (r *holdReads) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	sent := time.Now()
	r.mu.Lock()
	index := len(r.reads)
	r.reads = append(r.reads, holdRead{sent: sent, forwarded: sent})
	cut := r.fail != nil && r.fail(sent)
	until := r.heldUntil
	held := !cut && sent.Before(until) && !sent.Before(r.heldFrom)
	r.mu.Unlock()
	if cut {
		r.answer(index, 0, errCutOff)
		return cutOffRow{}
	}
	if held {
		spinUntil(until)
		r.mu.Lock()
		r.reads[index].forwarded = time.Now()
		r.mu.Unlock()
	}
	return notedHoldRead{row: r.intakeLeases.QueryRow(ctx, sql, args...), reads: r, index: index}
}

func (r *holdReads) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	sent := time.Now()
	tag, err := r.intakeLeases.Exec(ctx, sql, arguments...)
	r.mu.Lock()
	r.execs = append(r.execs, leaseExec{sent: sent, forwarded: sent, answered: time.Now(), rows: tag.RowsAffected(), err: err})
	r.mu.Unlock()
	return tag, err
}

// failWhile makes every read that goes out while fail reports true for its send time fail.
func (r *holdReads) failWhile(fail func(sent time.Time) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

// holdUntil holds each read the standby sends within window before until, and lets it go to
// Postgres at until. A read sent at or after until, or before the window, goes at once.
func (r *holdReads) holdUntil(until time.Time, window time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heldFrom, r.heldUntil = until.Add(-window), until
}

// spinUntil returns at at: it sleeps to a few milliseconds before at and spins through those,
// since a sleep wakes up to milliseconds late.
func spinUntil(at time.Time) {
	time.Sleep(time.Until(at) - 10*time.Millisecond)
	for time.Now().Before(at) {
	}
}

// answer notes the answer of the read at index.
func (r *holdReads) answer(index int, left time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads[index].answered, r.reads[index].left, r.reads[index].err = time.Now(), left, err
}

// notedHoldRead is the row one read of the hold answers, which notes that answer as it is
// scanned: duty.HoldToken scans the token and how long the hold has left, in microseconds.
type notedHoldRead struct {
	row   pgx.Row
	reads *holdReads
	index int
}

func (n notedHoldRead) Scan(dest ...any) error {
	err := n.row.Scan(dest...)
	var left time.Duration
	if micros, ok := dest[len(dest)-1].(*int64); ok && err == nil {
		left = time.Duration(*micros) * time.Microsecond
	}
	n.reads.answer(n.index, left, err)
	return err
}

// times is when the standby sent each read, in order.
func (r *holdReads) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	times := make([]time.Time, len(r.reads))
	for i, read := range r.reads {
		times[i] = read.sent
	}
	return times
}

// noted is every read noted so far and every statement sent through Exec, each in order.
func (r *holdReads) noted() ([]holdRead, []leaseExec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads), slices.Clone(r.execs)
}

// awaitRead waits up to within for the first read that went out at or after from to be answered,
// and answers it. what names that read in the failure.
func (r *holdReads) awaitRead(t *testing.T, from time.Time, within time.Duration, what string) holdRead {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		reads, _ := r.noted()
		if i := slices.IndexFunc(reads, func(read holdRead) bool { return !read.sent.Before(from) }); i >= 0 && !reads[i].answered.IsZero() {
			return reads[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the standby's %s was not answered within %s: its reads went out at %v", what, within, r.times())
		}
		time.Sleep(lostReplyPoll)
	}
}

// holdRow is the delivery durable's hold as one read of its row found it: the expiry the last
// renewal wrote, on the database's clock, how long the hold had left then, on the database's
// clock too, and that lapse on this process's clock, read as the time the hold had left from
// halfway through the read.
type holdRow struct {
	expires time.Time
	left    time.Duration
	lapse   time.Time
}

// landed is when the renewal that wrote the row landed, on this process's clock.
func (r holdRow) landed() time.Time { return r.lapse.Add(-intakeHoldTTL) }

// readHold reads the delivery durable's hold's row, and fails the test when there is none.
func readHold(t *testing.T, ctx context.Context, pool *store.Pool) holdRow {
	t.Helper()
	var row holdRow
	var seconds float64
	sent := time.Now()
	if err := pool.QueryRow(ctx, `select expires_at, extract(epoch from expires_at - now())::float8 from duty_holds where name = $1`,
		intakeHold).Scan(&row.expires, &seconds); err != nil {
		t.Fatalf("read the holder's hold: %v", err)
	}
	row.left = time.Duration(seconds * float64(time.Second))
	row.lapse = sent.Add(time.Since(sent) / 2).Add(row.left)
	return row
}

// awaitRenewal reads the hold's row every every until a renewal later than previous's has landed,
// and answers the row it wrote.
func awaitRenewal(t *testing.T, ctx context.Context, pool *store.Pool, previous holdRow, every time.Duration) holdRow {
	t.Helper()
	deadline := time.Now().Add(2 * intakeHoldRenew)
	for {
		row := readHold(t, ctx, pool)
		if row.expires.After(previous.expires) {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder's hold was not renewed within %s", 2*intakeHoldRenew)
		}
		time.Sleep(every)
	}
}
