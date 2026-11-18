package enroll

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// servingInterval is the rig clocks' Interval, and so the bound on each of their beats. The
// deadline a beat arms is on the rig's fake clock (servingStart), which no row moves unless it
// means a beat to fail there. The lock_timeout, statement_timeout and
// idle_in_transaction_session_timeout a beat sets are PostgreSQL's, on its own clock, which no
// fake reaches: ten minutes is far past anything a beat waits on unless a row holds it up, and
// such a row ends the beat at its deadline on the fake.
const servingInterval = 10 * time.Minute

// notServingAfter is the threshold every clock in this file records unless a test says otherwise,
// three intervals as cmd/broker records three sweeps: a gap after the latest beat longer than this
// is time no broker was serving. The gap it is compared with is PostgreSQL's own (now() less
// last_serving_at, migration 0024's rule), so no clock of Envoy's can stand in for it, and no row
// lets real time make one or close one: a row that needs a gap past it makes one in SQL
// (outageGap), and a gap a row means to stay within it is a few seconds, half an hour short of it.
const notServingAfter = 3 * servingInterval

// outageGap is the gap a row makes in SQL to stand for time no broker was serving: a minute past
// notServingAfter, so the real time a row takes only widens it, and half an hour short of both
// servingPrune and the enrollment lease, so a beat that gives it back prunes no row it was
// measured from and cuts no lease it gives it back to at one lease past the beat.
const outageGap = notServingAfter + time.Minute

// servingStart is where the rig's fake clocks stand until a row moves them.
var servingStart = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// servingRig is a Service on a fresh schema, the serving clock beating on its store on a fake
// clock no row moves, and one person's launcher credential to enroll sessions under.
type servingRig struct {
	svc   *Service
	clock *ServingClock
	cred  Credential
}

func newServingRig(t *testing.T) servingRig {
	t.Helper()
	svc := newService(t)
	return servingRig{
		svc:   svc,
		clock: &ServingClock{Enrollments: svc, Interval: servingInterval, NotServingAfter: notServingAfter, Clock: clocktest.New(servingStart)},
		cred:  mintCredential(t, svc, str("ada@example.com"), nil, "devbox"),
	}
}

// enroll creates a live session enrollment under the rig's credential.
func (r servingRig) enroll(t *testing.T, runtimeID string) string {
	t.Helper()
	e, err := r.svc.Create(context.Background(), r.cred, Enrollment{Kind: "session", RuntimeID: runtimeID, Operator: str("ada@example.com"), Thumbprint: "tp-" + runtimeID})
	if err != nil {
		t.Fatalf("Create(%s): %v", runtimeID, err)
	}
	return e.ID.String()
}

// exec runs one statement on the rig's store, failing t when it fails.
func (r servingRig) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := r.svc.Store.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// setLease sets enrollment id's lease to expr, an SQL expression whose parameters, from $2 on, are
// args.
func (r servingRig) setLease(t *testing.T, id, expr string, args ...any) {
	t.Helper()
	r.exec(t, `update enrollments set lease_expires_at = `+expr+` where id=$1`, append([]any{id}, args...)...)
}

func (r servingRig) lease(t *testing.T, id string) time.Time {
	t.Helper()
	var lease time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id=$1`, id).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

// servingOf reads clock's broker_serving row: when its process last served and the threshold it
// recorded.
func (r servingRig) servingOf(t *testing.T, clock *ServingClock) (time.Time, time.Duration) {
	t.Helper()
	var last time.Time
	var threshold time.Duration
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select last_serving_at, not_serving_after from broker_serving where process=$1`,
		clock.processID()).Scan(&last, &threshold); err != nil {
		t.Fatalf("read the broker_serving row of process %s: %v", clock.processID(), err)
	}
	return last, threshold
}

// serving reads the rig clock's broker_serving row.
func (r servingRig) serving(t *testing.T) (time.Time, time.Duration) {
	t.Helper()
	return r.servingOf(t, r.clock)
}

// latest reads the latest broker_serving row's last_serving_at, when any broker last served.
func (r servingRig) latest(t *testing.T) time.Time {
	t.Helper()
	var last time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select max(last_serving_at) from broker_serving`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	return last
}

// processes answers the process of every broker_serving row, in order.
func (r servingRig) processes(t *testing.T) []string {
	t.Helper()
	var processes []string
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select coalesce(array_agg(process order by process), '{}') from broker_serving`).Scan(&processes); err != nil {
		t.Fatal(err)
	}
	return processes
}

// now is Postgres's clock_timestamp(), the clock a beat reads.
func (r servingRig) now(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// beat runs one Beat of the rig clock and answers the database's clock just before and just after
// it, the bounds of the time it read.
func (r servingRig) beat(t *testing.T) (before, after time.Time) {
	t.Helper()
	before = r.now(t)
	if err := r.clock.Beat(context.Background()); err != nil {
		t.Fatalf("Beat: %v", err)
	}
	return before, r.now(t)
}

func (r servingRig) live(t *testing.T, id string) bool {
	t.Helper()
	_, live, err := r.svc.Lookup(context.Background(), id)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", id, err)
	}
	return live
}

// endLapsed runs EndLapsed and answers the ids it ended.
func (r servingRig) endLapsed(t *testing.T) []string {
	t.Helper()
	ended, err := r.svc.EndLapsed(context.Background())
	if err != nil {
		t.Fatalf("EndLapsed: %v", err)
	}
	ids := []string{}
	for _, e := range ended {
		ids = append(ids, e.ID)
	}
	return ids
}

// within fails t unless got lies in [low, high].
func within(t *testing.T, what string, got, low, high time.Time) {
	t.Helper()
	if got.Before(low) || got.After(high) {
		t.Fatalf("%s = %s, want it between %s and %s", what, got, low, high)
	}
}

// logBuffer is a log destination safe to write from several goroutines and read from the test.
type logBuffer = logtest.Buffer

// captureSlog sends slog's default logger to a buffer until t ends, and answers the buffer.
func captureSlog(t *testing.T) *logBuffer {
	t.Helper()
	logged := &logBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	return logged
}

// Until a broker has beaten, broker_serving is empty, so a lease is live exactly while it has not
// run out: a lapsed lease is refused and swept.
func TestBeforeAnyBeatALapsedLeaseIsLapsed(t *testing.T) {
	r := newServingRig(t)
	id := r.enroll(t, "box-unbeaten")
	if processes := r.processes(t); len(processes) != 0 {
		t.Fatalf("broker_serving before any beat holds %v, want no row", processes)
	}
	r.setLease(t, id, `now() - interval '1 minute'`)

	if r.live(t, id) {
		t.Fatal("Lookup answered live a lease that ran out before any broker beat")
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != id {
		t.Fatalf("EndLapsed ended %v, want just %s", ended, id)
	}
}

// A gap after the latest beat no longer than the threshold is a broker still serving: a lease that
// ran out inside it is lapsed, and the next beat moves only the clock.
func TestABeatAfterAGapWithinTheThresholdMovesNoLease(t *testing.T) {
	r := newServingRig(t)
	r.beat(t)
	id := r.enroll(t, "box-short-gap")
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '5 seconds'`)
	r.setLease(t, id, `now() - interval '1 second'`)
	last, _ := r.serving(t)
	lease := r.lease(t, id)

	if r.live(t, id) {
		t.Fatal("Lookup answered live a lease that ran out while a broker was serving")
	}
	before, after := r.beat(t)
	if got := r.lease(t, id); !got.Equal(lease) {
		t.Fatalf("lease after a beat within the threshold = %s, want it left at %s", got, lease)
	}
	moved, threshold := r.serving(t)
	within(t, "last_serving_at after the beat", moved, before, after)
	if !moved.After(last) || threshold != notServingAfter {
		t.Fatalf("broker_serving after the beat = %s, %s; want it moved past %s with threshold %s", moved, threshold, last, notServingAfter)
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != id {
		t.Fatalf("EndLapsed ended %v, want just %s", ended, id)
	}
}

// After a gap past the latest beat longer than the threshold, a lease that ran out after that beat
// is live to Lookup and Renew and EndLapsed leaves it; one that ran out before the beat, and a
// revoked one, are not. The next beat moves each unended lease still running at the latest beat on
// by the gap, no further than one Lease past the beat, leaves the others, and turns the guard off:
// the restored lease is live on its own value.
func TestTheTimeNoBrokerServedIsGivenBackToTheLeasesItRanOut(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	r.beat(t)
	restorable := r.enroll(t, "box-restorable")
	renewer := r.enroll(t, "box-renewer")
	before := r.enroll(t, "box-ran-out-before")
	revoked := r.enroll(t, "box-revoked")
	capped := r.enroll(t, "box-capped")
	if err := r.svc.Revoke(ctx, r.cred, revoked, "ada@example.com"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	r.exec(t, `update broker_serving set last_serving_at = now() - $1::interval`, outageGap)
	r.setLease(t, restorable, `now() - interval '1 minute'`)
	r.setLease(t, renewer, `now() - interval '1 minute'`)
	r.setLease(t, revoked, `now() - interval '1 minute'`)
	r.setLease(t, before, `now() - $2::interval`, outageGap+10*time.Minute)
	r.setLease(t, capped, `now() + interval '55 minutes'`)

	for id, want := range map[string]bool{restorable: true, renewer: true, before: false, revoked: false, capped: true} {
		if got := r.live(t, id); got != want {
			t.Fatalf("Lookup(%s) live = %v, want %v", id, got, want)
		}
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != before {
		t.Fatalf("EndLapsed ended %v, want just the lease that ran out before the latest beat, %s", ended, before)
	}
	if _, err := r.svc.Renew(ctx, renewer, ""); err != nil {
		t.Fatalf("Renew of a lease that ran out while no broker served: %v", err)
	}
	if _, err := r.svc.Renew(ctx, before, ""); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Renew of the ended enrollment = %v, want ErrNotLive", err)
	}

	last := r.latest(t)
	old := map[string]time.Time{}
	for _, id := range []string{restorable, before, revoked, capped} {
		old[id] = r.lease(t, id)
	}
	beatBefore, beatAfter := r.beat(t)

	within(t, "the restored lease", r.lease(t, restorable), old[restorable].Add(beatBefore.Sub(last)), old[restorable].Add(beatAfter.Sub(last)))
	within(t, "the capped lease", r.lease(t, capped), beatBefore.Add(r.svc.Lease), beatAfter.Add(r.svc.Lease))
	within(t, "the renewed lease", r.lease(t, renewer), beatBefore, beatAfter.Add(r.svc.Lease))
	for _, id := range []string{before, revoked} {
		if got := r.lease(t, id); !got.Equal(old[id]) {
			t.Fatalf("lease of ended enrollment %s = %s after the beat, want it left at %s", id, got, old[id])
		}
	}
	moved, _ := r.serving(t)
	within(t, "last_serving_at after the beat", moved, beatBefore, beatAfter)

	var guard bool
	if err := r.svc.Store.Pool.QueryRow(ctx, `select enrollment_lease_live(now() - interval '1 second')`).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if guard {
		t.Fatal("enrollment_lease_live answered a lease that just ran out live after the beat gave the time back")
	}
	if !r.lease(t, restorable).After(r.now(t)) || !r.live(t, restorable) {
		t.Fatalf("the restored lease %s is not live on its own value", r.lease(t, restorable))
	}
	if ended := r.endLapsed(t); len(ended) != 0 {
		t.Fatalf("EndLapsed after the beat ended %v, want nothing", ended)
	}
}

// Two brokers beating at once after a gap give the time back once: both find the gap, the second
// waits on the lock the first holds while it gives the gap back, then reads the row the first wrote
// and finds no gap.
func TestTwoBeatsAtOnceGiveTheTimeBackOnce(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	other := &ServingClock{Enrollments: r.svc, Interval: servingInterval, NotServingAfter: notServingAfter, Clock: r.clock.Clock}
	r.beat(t)
	if err := other.Beat(ctx); err != nil {
		t.Fatalf("Beat of the other broker: %v", err)
	}
	id := r.enroll(t, "box-concurrent")
	r.exec(t, `update broker_serving set last_serving_at = now() - $1::interval`, outageGap)
	r.setLease(t, id, `now() - interval '1 minute'`)
	last := r.latest(t)
	old := r.lease(t, id)

	holder, err := r.svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, servingLock); err != nil {
		t.Fatal(err)
	}
	start := r.now(t)
	done := make(chan error, 2)
	for _, clock := range []*ServingClock{r.clock, other} {
		go func() { done <- clock.Beat(ctx) }()
	}
	storetest.AwaitLockWaiters(t, r.svc.Store.Pool, holder, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("Beat: %v", err)
		}
	}
	end := r.now(t)

	within(t, "the lease after two beats", r.lease(t, id), old.Add(start.Sub(last)), old.Add(end.Sub(last)))
	for _, clock := range []*ServingClock{r.clock, other} {
		moved, _ := r.servingOf(t, clock)
		within(t, "last_serving_at after the beats", moved, start, end)
	}
}

// The clock never moves backward: a beat that finds its row's last_serving_at ahead of its own
// time leaves it there, and records its own threshold.
func TestABeatNeverMovesTheClockBackward(t *testing.T) {
	r := newServingRig(t)
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values ($1, now() + interval '1 hour', interval '1 second')`, r.clock.processID())
	ahead, _ := r.serving(t)
	r.beat(t)
	if got, threshold := r.serving(t); !got.Equal(ahead) || threshold != notServingAfter {
		t.Fatalf("broker_serving after the beat = %s, %s; want it left at %s with threshold %s", got, threshold, ahead, notServingAfter)
	}
}

// With every row gone a lease is live exactly while it has not run out, and the next beat writes
// its process's row as of its own time.
func TestABeatRecreatesADeletedRow(t *testing.T) {
	r := newServingRig(t)
	r.beat(t)
	id := r.enroll(t, "box-no-row")
	r.exec(t, `delete from broker_serving`)
	r.setLease(t, id, `now() - interval '1 minute'`)
	if r.live(t, id) {
		t.Fatal("Lookup answered live a lapsed lease with no broker_serving row")
	}
	before, after := r.beat(t)
	last, threshold := r.serving(t)
	within(t, "the recreated last_serving_at", last, before, after)
	if threshold != notServingAfter {
		t.Fatalf("the recreated threshold = %s, want %s", threshold, notServingAfter)
	}
}

// A beat deletes the rows of processes more than an hour behind the latest row and keeps the rest;
// the latest row is never deleted, even when it is not the beating process's.
func TestABeatPrunesRowsAnHourBehindTheLatest(t *testing.T) {
	r := newServingRig(t)
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values
		('gone', now() - interval '2 hours', interval '15 seconds'),
		('recent', now() - interval '30 minutes', interval '15 seconds')`)
	r.beat(t)
	if got, want := sortedCopy(r.processes(t)), sortedCopy([]string{r.clock.processID(), "recent"}); !slices.Equal(got, want) {
		t.Fatalf("broker_serving after the beat holds %v, want %v", got, want)
	}

	r.exec(t, `delete from broker_serving`)
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values ('ahead', now() + interval '2 hours', interval '15 seconds')`)
	r.beat(t)
	if got := r.processes(t); !slices.Contains(got, "ahead") {
		t.Fatalf("broker_serving after the beat holds %v, want it to keep the latest row, ahead", got)
	}
}

// sortedCopy answers a sorted copy of s.
func sortedCopy(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// A beat counts the brokers beating at its own instant and hands that count to OnReplicas: its
// own row and every other row whose gap since its last beat is within that row's own threshold.
// A row that stopped beating is not one of them, however recently it is still in the table, so a
// fleet that loses a pod stops counting it a threshold later rather than at the next prune, an
// hour on.
func TestABeatCountsTheBrokersStillBeating(t *testing.T) {
	r := newServingRig(t)
	var counted []int
	r.clock.OnReplicas = func(replicas int) { counted = append(counted, replicas) }
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values
		('peer-1', now(), $1::interval),
		('peer-2', now(), $1::interval),
		('peer-3', now(), $1::interval),
		('peer-4', now(), $1::interval)`, notServingAfter)
	r.beat(t)
	if want := []int{5}; !slices.Equal(counted, want) {
		t.Fatalf("a beat beside four beating peers counted %v, want %v: the four and this process's own row", counted, want)
	}
	r.exec(t, `update broker_serving set last_serving_at = now() - $1::interval where process like 'peer-%'`, outageGap)
	r.beat(t)
	if want := []int{5, 1}; !slices.Equal(counted, want) {
		t.Fatalf("a beat beside four peers a minute past their own %s threshold counted %v, want %v", notServingAfter, counted, want)
	}
}

// A broker beating alone counts one, itself, and says so: holding a lone pod to the rate two
// pods get is api.MinReplicas's work and not the count's, so the count a process hands over is
// the fleet it really sees.
func TestALoneBrokersBeatCountsItself(t *testing.T) {
	r := newServingRig(t)
	var counted []int
	r.clock.OnReplicas = func(replicas int) { counted = append(counted, replicas) }
	r.beat(t)
	r.beat(t)
	if want := []int{1, 1}; !slices.Equal(counted, want) {
		t.Fatalf("a lone broker's two beats counted %v, want %v", counted, want)
	}
}

// A beat held up on its own row (another transaction holds it) fails at its own deadline, and Run
// warns once, that it timed out, for a run of such beats, while another broker beats on through
// the hold, at once and at its next tick: that broker's row stays the latest, so the held row,
// fallen past its threshold, turns no guard on, and a lease that ran out after the held row's
// last beat is lapsed, not held live as if no broker were serving.
func TestAHeldServingLockFailsTheBeatAndAnotherBrokerKeepsTheGuardOff(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	// Each broker runs on a fake clock of its own, which only this test moves: the test alone
	// brings a beat to its deadline and Run to its next tick, so which beat fails and which beats
	// on is the code's, and a loaded machine can neither time a beat out nor leave a gap between
	// two. This row once ran each beat under a real bound (200 ms, then a second after INFRA-985),
	// and a machine with more runnable threads than cores timed the serving broker's beats out,
	// which turned the guard on.
	stuckTimers, servingTimers := clocktest.New(servingStart), clocktest.New(servingStart)
	stuck := &ServingClock{Enrollments: r.svc, Interval: servingInterval, NotServingAfter: notServingAfter, Clock: stuckTimers}
	serving := &ServingClock{Enrollments: r.svc, Interval: servingInterval, NotServingAfter: notServingAfter, Clock: servingTimers}
	for _, clock := range []*ServingClock{stuck, serving} {
		if err := clock.Beat(ctx); err != nil {
			t.Fatalf("Beat: %v", err)
		}
	}
	id := r.enroll(t, "box-held")
	logged := captureSlog(t)
	// The held row last beat past its threshold, and the lease ran out after that beat, so a guard
	// that read the held row rather than the latest would hold the lease live. The gap is made in
	// SQL, since PostgreSQL measures it on its own clock (notServingAfter), and before the hold,
	// which leaves the row as it is until the test ends.
	r.exec(t, `update broker_serving set last_serving_at = now() - $2::interval where process = $1`, stuck.processID(), outageGap)
	r.setLease(t, id, `now() - interval '1 minute'`)

	holder, err := r.svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select from broker_serving where process=$1 for update`, stuck.processID()); err != nil {
		t.Fatal(err)
	}
	stuckLast, _ := r.servingOf(t, stuck)
	servingLast, _ := r.servingOf(t, serving)

	// A beat on the held row waits on it until the deadline it armed, Interval from its start,
	// ends it with its own timeout. What that failure is, and not how long it took, is the fact.
	held := make(chan error, 1)
	go func() { held <- stuck.Beat(ctx) }()
	storetest.AwaitLockWaiters(t, r.svc.Store.Pool, holder, 1)
	deadline := stuckTimers.AwaitArmed(t, "the held beat's deadline", 1)
	if want := servingStart.Add(servingInterval); len(deadline) != 1 || !deadline[0].Equal(want) {
		t.Fatalf("the held beat armed %v, want one deadline at %s, Interval from its start", deadline, want)
	}
	stuckTimers.Set(deadline[0])
	if err := testwait.Receive(t, "the held beat to end at its deadline", held); err == nil || !beatTimedOut(err) {
		t.Fatalf("Beat on a held row = %v, want its own timeout", err)
	}

	runs, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, clock := range []*ServingClock{stuck, serving} {
		wg.Go(func() { clock.Run(runs) })
	}
	// The serving broker beats at once, and again at the tick the test moves its clock to, while
	// the other row is held. Its first beat has returned once its deadline is no longer armed, so
	// the move brings Run's tick alone and ends no beat in flight.
	first := r.beatsPast(t, serving, servingLast)
	tick := testwait.Until(t, "the serving broker's Run to wait on its tick alone", func() ([]time.Time, bool) {
		armed := servingTimers.Armed()
		return armed, len(armed) == 1
	})
	servingTimers.Set(tick[0])
	r.beatsPast(t, serving, first)
	// The stuck broker's Run arms its tick and its first beat's deadline at the same moment, and
	// the move there ends that beat, on the held row, and brings the tick: Run warns, then beats
	// again. The next move ends the second beat the same way. Run has decided what to log of it
	// once it has armed its tick and a third beat's deadline.
	stuckBeat := func(what string) {
		armed := stuckTimers.AwaitArmed(t, what, 2)
		stuckTimers.Set(armed[1])
	}
	stuckBeat("the stuck broker's tick and its first beat's deadline")
	testwait.Next(t, "Run's warning that the stuck broker could not beat", logged.Stream(), func(line string) bool {
		return strings.Contains(line, "level=WARN") && strings.Contains(line, "process="+stuck.processID())
	})
	stuckBeat("the stuck broker's next tick and its second beat's deadline")
	stuckTimers.AwaitArmed(t, "the stuck broker's tick and its third beat's deadline", 2)
	live := r.live(t, id)
	ended := r.endLapsed(t)
	cancel()
	wg.Wait()

	if live {
		t.Fatal("Lookup answered live a lease that ran out while another broker was serving")
	}
	if !slices.Contains(ended, id) {
		t.Fatalf("EndLapsed ended %v, want it to end %s", ended, id)
	}
	if last, _ := r.servingOf(t, serving); !last.After(first) {
		t.Fatalf("the serving broker's last_serving_at = %s, want it past %s, its first beat under Run: it beat again at its tick while the other row was held", last, first)
	}
	if last, _ := r.servingOf(t, stuck); !last.Equal(stuckLast) {
		t.Fatalf("the stuck broker's last_serving_at = %s, want it left at %s while its row is held", last, stuckLast)
	}
	// The stuck broker's two beats under Run both timed out, and Run warns once for a run of
	// failures: one warning, saying the serving clock could not beat, with timed_out=true. The
	// count is the test's, which moved the clock to two deadlines.
	warnings := []string{}
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "process="+stuck.processID()) {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "the serving clock could not beat") || !strings.Contains(warnings[0], "timed_out=true") {
		t.Fatalf("Run logged %q for the stuck broker over two beats that timed out, want one warning that the serving clock could not beat, with timed_out=true", warnings)
	}
}

// beatsPast waits for clock's broker_serving row to pass want, and answers where it stands then.
func (r servingRig) beatsPast(t *testing.T, clock *ServingClock, want time.Time) time.Time {
	t.Helper()
	return testwait.Until(t, "the serving broker's last_serving_at to pass "+want.String(), func() (time.Time, bool) {
		last, _ := r.servingOf(t, clock)
		return last, last.After(want)
	})
}

// Run beats as soon as it starts and returns once its context ends.
func TestRunBeatsAtOnceAndReturnsWhenItsContextEnds(t *testing.T) {
	r := newServingRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.clock.Run(ctx)
		close(done)
	}()
	testwait.Eventually(t, "Run to beat", func() bool {
		return slices.Contains(r.processes(t), r.clock.processID())
	})
	cancel()
	testwait.Receive(t, "Run to return once its context ended", done)
}

// beforeTheSharedRule is the gap condition ServingClock.Beat held inline, in SQL of its own,
// before migration 0024 gave the rule one definition: $1 is the beat's instant and latest is the
// latest broker_serving row, left joined so that an empty table is no outage.
const beforeTheSharedRule = `coalesce($1::timestamptz - latest.last_serving_at > latest.not_serving_after, false)`

// beforeTheSharedRuleLiveness is enrollment_lease_live's body as migration 0019 wrote it, with
// now() written as $1 and the lease as $2, so that this instant is a parameter here too.
const beforeTheSharedRuleLiveness = `($2::timestamptz > $1::timestamptz or exists (
		select from (
			select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
		) old
		where $1::timestamptz - old.last_serving_at > old.not_serving_after
			and $2::timestamptz > old.last_serving_at))`

// outageRow is one broker_serving row of the matrix below, placed behind the instant the case is
// measured at.
type outageRow struct {
	process   string
	behind    time.Duration // how far behind that instant this process last served
	threshold time.Duration // its not_serving_after
}

// The one rule migration 0024 states, broker_outage_since, answers what the beat's own inline
// condition answered and what enrollment_lease_live answers, over a matrix of rows, thresholds,
// leases and instants: a gap below the threshold, a gap exactly on it, a gap one microsecond past
// it, no row at all, and a lease before, on and after the latest beat. The case's instant is a
// parameter and the rows are placed against it, so the boundary is exact rather than whatever a
// wall clock left between two statements. The whole case runs in one transaction that never
// commits, so now() - the instant enrollment_lease_live measures from - is that same parameter,
// and the rows it inserts are gone when it ends.
//
// This one test does three jobs:
//
//  1. it is the test that fails if the two statements of the rule can disagree (the beat's old
//     condition against the shared function), and it places a gap exactly on the threshold;
//  2. it is the proof that the build an image pin replaces still reads and beats against the
//     schema this migration leaves: enrollment_lease_live at its old one-argument signature and
//     the old inline SQL, both run here over the post-0024 schema, answer what they answered
//     before it. A roll runs pods of both builds, and 0024 touches no table, so nothing a pod of
//     either build writes is at stake;
//  3. it is the target of this change's named revert: >= in place of > inside
//     broker_outage_since, which turns the row whose gap lies exactly on the threshold red.
func TestTheOutageRuleAgreesWithTheBeatsOldCondition(t *testing.T) {
	r := newServingRig(t)
	// threshold is the not_serving_after of the matrix's rows. Each case places its rows against
	// its own instant inside one transaction, so no real time passes between them and any threshold
	// serves; this one is short enough that a row a minute behind is past it.
	const threshold = 15 * time.Second
	for _, c := range []struct {
		name       string
		rows       []outageRow
		lease      time.Duration // the lease under test, as an offset from the case's instant
		wantOutage bool
		wantLive   bool
	}{
		{name: "no row at all and a lease that has run out", lease: -time.Second},
		{name: "no row at all and a lease still running", lease: time.Second, wantLive: true},
		{
			name:  "a gap below the threshold",
			rows:  []outageRow{{process: "p1", behind: 10 * time.Second, threshold: threshold}},
			lease: -time.Second,
		},
		{
			name:  "a gap exactly on the threshold",
			rows:  []outageRow{{process: "p1", behind: threshold, threshold: threshold}},
			lease: -time.Second,
		},
		{
			name:       "a gap one microsecond past the threshold",
			rows:       []outageRow{{process: "p1", behind: threshold + time.Microsecond, threshold: threshold}},
			lease:      -time.Second,
			wantOutage: true,
			wantLive:   true,
		},
		{
			name:       "a lease that ran out before the latest beat",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: threshold}},
			lease:      -90 * time.Second,
			wantOutage: true,
		},
		{
			name:       "a lease that ran out exactly at the latest beat",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: threshold}},
			lease:      -time.Minute,
			wantOutage: true,
		},
		{
			name:       "a lease that ran out after the latest beat",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: threshold}},
			lease:      -30 * time.Second,
			wantOutage: true,
			wantLive:   true,
		},
		{
			name:       "a lease still running while no broker serves",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: threshold}},
			lease:      time.Second,
			wantOutage: true,
			wantLive:   true,
		},
		{
			name: "the latest row's own threshold decides, not an older row's",
			rows: []outageRow{
				{process: "older", behind: time.Minute, threshold: threshold},
				{process: "latest", behind: 20 * time.Second, threshold: 30 * time.Second},
			},
			lease: -time.Second,
		},
		{
			name: "an older row within its own generous threshold is not read",
			rows: []outageRow{
				{process: "older", behind: 5 * time.Minute, threshold: time.Hour},
				{process: "latest", behind: 20 * time.Second, threshold: threshold},
			},
			lease:      -10 * time.Second,
			wantOutage: true,
			wantLive:   true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := r.svc.Store.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var at time.Time
			if err := tx.QueryRow(ctx, `select now()`).Scan(&at); err != nil {
				t.Fatal(err)
			}
			for _, row := range c.rows {
				if _, err := tx.Exec(ctx, `insert into broker_serving (process, last_serving_at, not_serving_after)
					values ($1, $2::timestamptz - $3::interval, $4::interval)`, row.process, at, row.behind, row.threshold); err != nil {
					t.Fatalf("insert the broker_serving row of %s: %v", row.process, err)
				}
			}
			lease := at.Add(c.lease)
			var outageSince, latest *time.Time
			var oldOutage, live, oldLive, atIsNow bool
			if err := tx.QueryRow(ctx, `select broker_outage_since($1::timestamptz), `+beforeTheSharedRule+`,
					enrollment_lease_live($2::timestamptz), `+beforeTheSharedRuleLiveness+`,
					latest.last_serving_at, now() = $1::timestamptz
				from (select 1) beat
				left join lateral (
					select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
				) latest on true`, at, lease).Scan(&outageSince, &oldOutage, &live, &oldLive, &latest, &atIsNow); err != nil {
				t.Fatal(err)
			}
			if !atIsNow {
				t.Fatalf("the transaction's now() is not %s, the instant its rows are placed against: enrollment_lease_live would measure from another instant", at)
			}
			outage := outageSince != nil
			if outage != oldOutage {
				t.Fatalf("broker_outage_since(%s) is not null = %t, while the beat's own old condition at that instant = %t: the shared rule and the condition it replaced disagree",
					at, outage, oldOutage)
			}
			if outage != c.wantOutage {
				t.Fatalf("broker_outage_since(%s) is not null = %t, want %t", at, outage, c.wantOutage)
			}
			if outage && (latest == nil || !outageSince.Equal(*latest)) {
				t.Fatalf("broker_outage_since(%s) = %s, want the latest row's last_serving_at %v: that is the instant the beat gives the gap back from",
					at, *outageSince, latest)
			}
			if live != oldLive {
				t.Fatalf("enrollment_lease_live(%s) = %t, while enrollment_lease_live as migration 0019 wrote it = %t: a pod of the build before 0024 would read this lease differently",
					lease, live, oldLive)
			}
			if live != c.wantLive {
				t.Fatalf("enrollment_lease_live(%s) = %t, want %t", lease, live, c.wantLive)
			}
		})
	}
}
