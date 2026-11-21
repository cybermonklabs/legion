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
	"github.com/jackc/pgx/v5/pgxpool"
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

// worstPhaseLead is how long before the hold's lapse the worst-phase row aims the standby's poll,
// and half the window it holds that poll to. A poll aimed at the lapse itself would go out after
// it -- the run's ticker fires about a millisecond late, and the holder's renewal lands up to a
// few milliseconds either side of its own tick -- and so take the hold at once, which is the best
// phase and not the worst. Aimed this far ahead, it went out 5 to 13 ms before the lapse in the
// runs held_durable.go records, the last poll to find the hold live.
const worstPhaseLead = 10 * time.Millisecond

// worstPhaseAttempts is how many deaths the worst-phase row places before it fails for having
// forced the worst phase on none of them. A loaded machine now and then delays the standby's poll,
// or the row's own read of the holder's renewal, past the windows the row holds them to: in one
// run of sixty the poll fired about 12 ms late, so its read went out 1.8 ms after the lapse and
// took the hold at once, which is the best phase.
const worstPhaseAttempts = 3

// A holder that dies is replaced within the bound held_durable.go states at the worst phase there
// is, with no reply lost: the holder dies the instant after a renewal landed, so its hold lasts the
// whole intakeHoldTTL from the death, and the standby's poll goes out just before the lapse, so it
// is the last poll to find the hold live. The standby tries again at the lapse that poll read
// (heldDurable.run). held_durable.go records what twenty runs of this row measured, and what they
// measured before it did, when its next try was its next poll, a whole durableBindInterval after
// this one, and every run passed the bound.
//
// The phase is forced rather than waited for. The holder renews every intakeHoldRenew from when it
// took the hold, and the standby polls every durableBindInterval from when its run starts, so the
// row reads one renewal off the hold's row, starts the standby's run (newTakeOverProcess builds it
// unstarted) a whole number of intervals before the moment worstPhaseLead ahead of the lapse the
// next renewal sets, waits for that renewal, and kills the holder as soon as the row shows it. The
// standby's lease pool notes when each read of the hold went out (holdReads). An attempt misses
// the worst phase when the holder died more than worstPhaseLead after the renewal, or when no read
// went out within twice worstPhaseLead before the lapse with a later read taking the hold: a read
// that finds the hold free is the standby's last, since it holds the hold from then on.
//
// A miss is a miss of the row's own setup, never a pass. The row places the death again rather
// than fail on one, since a loaded machine now and then delays a poll or a read past those windows
// (worstPhaseAttempts): the standby that took the durable over holds the hold and renews it as the
// first holder did, so it is the next attempt's holder, and a fresh standby is aimed at its next
// renewal. The row passes only on an attempt that forced the worst phase and met the bound there,
// and fails when all worstPhaseAttempts attempts missed. A missed attempt's take-over must meet
// the bound too, as every death's must, though meeting it proves nothing.
func TestADeadHolderIsReplacedWithinTheStatedBoundAtTheWorstPhase(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	first := startIntakeProcess(t, pool, natsURL, nil)
	awaitBoundDurable(t, publisher)
	holderLeases, holderClient := first.leases, first.client
	bound := intakeHoldTTL + durableBindInterval
	var misses []string
	for attempt := 1; attempt <= worstPhaseAttempts; attempt++ {
		held, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err != nil {
			t.Fatalf("read the durable the holder bound: %v", err)
		}
		standby := newTakeOverProcess(t, pool, natsURL)
		// Cuttable, as the first holder's lease pool is, since a standby whose attempt missed is
		// the next attempt's holder.
		standbyLeases := &cuttableLeases{pool: standby.intake.leases.(*pgxpool.Pool)}
		reads := &holdReads{intakeLeases: standbyLeases}
		standby.intake.leases = reads

		renewed := awaitRenewal(t, ctx, pool, readHold(t, ctx, pool), lostReplyPoll)
		start := renewed.lapse.Add(intakeHoldRenew - worstPhaseLead)
		for time.Until(start) > durableBindInterval {
			start = start.Add(-durableBindInterval)
		}
		// Spun up to rather than slept to: a sleep wakes up to milliseconds late.
		time.Sleep(time.Until(start) - 10*time.Millisecond)
		for time.Now().Before(start) {
		}
		startIntake(t, standby.intake)

		time.Sleep(time.Until(renewed.landed().Add(intakeHoldRenew - 20*time.Millisecond)))
		last := awaitRenewal(t, ctx, pool, renewed, 0)
		abandoned := time.Now()
		holderLeases.cut.Store(true)
		holderClient.Close()

		took := awaitTakenOver(t, publisher, held, abandoned, last.lapse).Sub(abandoned)
		died := abandoned.Sub(last.landed())
		sent := reads.times()
		fromLapse := durations(sent, last.lapse)
		live := -1
		for i, at := range sent {
			if at.Before(last.lapse) {
				live = i
			}
		}
		if died > worstPhaseLead || live < 0 || live == len(fromLapse)-1 || -fromLapse[live] > 2*worstPhaseLead {
			misses = append(misses, fmt.Sprintf("attempt %d: the holder died %s after its last renewal landed and the standby read the hold at %v from the lapse",
				attempt, died, fromLapse))
			t.Logf("%s, which missed the worst phase; the standby took the durable over %s after the holder died", misses[len(misses)-1], took)
			if took > bound {
				t.Fatalf("the standby took the durable over %s after the holder died, on attempt %d, which missed the worst phase; want within intakeHoldTTL plus one durableBindInterval (%s) at every phase",
					took.Round(time.Millisecond), attempt, bound)
			}
			holderLeases, holderClient = standbyLeases, standby.client
			continue
		}
		t.Logf("worst phase forced on attempt %d after %d misses: the holder died %s after its last renewal landed; the standby's poll went out %s before the lapse and found the hold live, and its next read, %s after the lapse, took it; the standby took the durable over %s after the holder died",
			attempt, len(misses), died, -fromLapse[live], fromLapse[live+1], took)
		if took > bound {
			t.Fatalf("the standby took the durable over %s after the holder died the instant after a renewal, its poll just before the lapse; want within intakeHoldTTL plus one durableBindInterval (%s)",
				took.Round(time.Millisecond), bound)
		}
		return
	}
	t.Fatalf("the row did not force the worst phase in %d attempts: %s; want the holder dead within %s of its last renewal, and the standby's last read before the lapse within %s of it and a later one taking the hold",
		worstPhaseAttempts, strings.Join(misses, "; "), worstPhaseLead, 2*worstPhaseLead)
}

// holdReads is a standby's lease pool that notes each of its reads of its durable's hold, the one
// statement it sends through QueryRow (duty.HoldToken): when it went out, when its answer was
// scanned, how long it found the hold had left, and its error. A read that goes out while fail
// reports true fails as a cut-off process's read does (cutOffRow). It notes each statement it
// sends through Exec too, which while it holds no hold is its take (duty.TryHold).
type holdReads struct {
	intakeLeases

	mu    sync.Mutex
	reads []holdRead
	execs []leaseExec
	fail  func(sent time.Time) bool
}

// holdRead is one read of the hold as holdReads noted it: left is how long it found the hold had
// left, on the database's clock, and answered is zero until its answer is scanned.
type holdRead struct {
	sent, answered time.Time
	left           time.Duration
	err            error
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
	r.reads = append(r.reads, holdRead{sent: sent})
	cut := r.fail != nil && r.fail(sent)
	r.mu.Unlock()
	if cut {
		r.answer(index, 0, errCutOff)
		return cutOffRow{}
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

// times is when each read went out, in order.
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
