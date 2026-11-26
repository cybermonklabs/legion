package processes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A live process refuses a newcomer whose key set does not share its own both ways, each way on
// its own, and the refusal records nothing and says what to do. Key sets that overlap with the
// current keys swapped, which is the middle of a rotation, are accepted, and so is a key set no
// live process shares when the only other row is a process gone. A peer that refuses stays live
// throughout, so its refusal stands through Register's wait for it to lapse.
func TestRegisterRefusesAKeySetALivePeerDoesNotShare(t *testing.T) {
	for _, c := range []struct {
		name, peer, mine string
		stale            bool
		refused          bool
		says             string
	}{
		{name: "the peer's current key is not one of mine", peer: "K0 K1", mine: "K1", refused: true,
			says: "signs its cookies with a key this process does not hold"},
		{name: "my current key is not one of the peer's", peer: "K0", mine: "K1 K0", refused: true,
			says: "does not hold the key this process would sign its cookies with"},
		{name: "keys of their own", peer: "K0", mine: "K9", refused: true,
			says: "signs its cookies with a key this process does not hold"},
		{name: "the rotation's middle: K1 K0 beside K0 K1", peer: "K1 K0", mine: "K0 K1"},
		{name: "one key set", peer: "K0", mine: "K0"},
		{name: "another key, but the peer is gone", peer: "K0", mine: "K9", stale: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			database := storetest.Open(t)
			// The peer heartbeats on a clock of its own, which nothing moves, so no beat of its
			// stamps the row the test ages or holds live; the newcomer waits on the other.
			held := clocktest.New(time.Now())
			fake := clocktest.New(time.Now())
			peer := mustRegisterOn(t, database, held, "c1", c.peer)
			switch {
			case c.stale:
				// A gone process does not heartbeat back into Live: its clock never moves here.
				ageHeartbeat(t, database.Pool, peer.ID, LiveWindow+time.Second)
			case c.refused:
				// A refusing peer is live at every try of the newcomer's, however long the
				// machine takes before them (holdLive).
				holdLive(t, database, peer.ID)
			}
			answered := make(chan registerAnswer, 1)
			go func() {
				registration, err := registerOn(t, database, fake, "c2", c.mine)
				answered <- registerAnswer{registration: registration, err: err}
			}()
			if c.refused {
				// The wait for the peer's row to lapse runs on this clock, and the row is live
				// on the database's clock throughout, so the wait ends in the refusal where the
				// test moves it past LiveWindow and one HeartbeatInterval.
				fake.AwaitArmed(t, "Register's retry gap", 1)
				fake.Advance(LiveWindow + HeartbeatInterval)
			}
			got := testwait.Receive(t, "the registration's answer beside the peer", answered)
			registration, err := got.registration, got.err
			if !c.refused {
				if err != nil {
					t.Fatalf("a process holding %q beside a peer holding %q was refused: %v", c.mine, c.peer, err)
				}
				if !rowExists(t, database, registration.ID) {
					t.Errorf("the accepted process has no row")
				}
				return
			}
			if !errors.Is(err, ErrKeySetMismatch) {
				t.Fatalf("a process holding %q beside a live peer holding %q registered with %v, want ErrKeySetMismatch", c.mine, c.peer, err)
			}
			for _, part := range []string{peer.ID, "commit c1", c.says, "DISPATCH_SIGNING_KEY", "add the new key second"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the refusal %q does not say %q", err, part)
				}
			}
			var rows int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from server_processes`).Scan(&rows); err != nil {
				t.Fatalf("count the registry's rows: %v", err)
			}
			if rows != 1 {
				t.Errorf("the registry holds %d rows after the refusal, want the peer's alone", rows)
			}
		})
	}
}

// Two processes with keys of their own booting at once are serialised: the second reads the
// first's row and is refused, so exactly one of them registers, and the refusal stands through the
// second's wait for that row to lapse. Both are held at their first read of the registry by a lock
// this test takes on the table, and released together, which is the moment two reads without the
// registry's own lock would both find the registry empty.
//
// Booting at once is the two transactions beginning within LiveWindow of each other: each
// registration judges the rows it reads by its own transaction's now(), its start (admit, Live),
// and the first one's row is dated with that one's. So the row reads both starts from the
// database while the two are held (registerTwoAtOnce), and an attempt whose transactions the
// machine began LiveWindow or more apart, in which the second may lawfully count the first as
// gone, is made again on a database of its own.
func TestTwoRegistrationsAtOnceWithKeysOfTheirOwnAreSerialised(t *testing.T) {
	for attempt := 1; ; attempt++ {
		pair := registerTwoAtOnce(t)
		if pair.begunApart >= LiveWindow {
			t.Logf("attempt %d: the two registrations' transactions began %s apart, LiveWindow (%s) or more, so the second may lawfully have counted the first as gone; trying again on a new database",
				attempt, pair.begunApart, LiveWindow)
			continue
		}
		if !errors.Is(pair.second.err, ErrKeySetMismatch) {
			t.Fatalf("the second registration with keys of their own answered %v once its wait was over, want ErrKeySetMismatch: exactly one of the two is refused", pair.second.err)
		}
		if got := liveIDs(t, pair.database); !slices.Equal(got, []string{pair.first.registration.ID}) {
			t.Errorf("live = %v, want the one registration accepted, %s", got, pair.first.registration.ID)
		}
		return
	}
}

// twoAtOnce is one attempt of TestTwoRegistrationsAtOnceWithKeysOfTheirOwnAreSerialised: its
// database, how far apart the two registrations' transactions began, and what each answered, the
// first to answer first.
type twoAtOnce struct {
	database      *store.Store
	begunApart    time.Duration
	first, second registerAnswer
}

// registerTwoAtOnce runs two registrations with keys of their own, K0 and K1, at once on a
// database of its own, held at their first read of the registry until both have queued there.
// Each waits and heartbeats on a clock of its own. The first to answer must be the one admitted,
// since the other answers only once the test has moved its clock: the admitted one's row is put
// a day ahead (holdLive) and its clock never moved, so it is live through the other's whole
// wait however long the machine takes, and the test then moves the other's clock past
// LiveWindow and one HeartbeatInterval, which ends that wait.
func registerTwoAtOnce(t *testing.T) twoAtOnce {
	t.Helper()
	database := storetest.Open(t)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("open the transaction holding the registry: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(context.Background(), `lock table server_processes in access exclusive mode`); err != nil {
		t.Fatalf("hold the registry: %v", err)
	}

	clocks := []*clocktest.Fake{clocktest.New(time.Now()), clocktest.New(time.Now())}
	type answer struct {
		index int
		registerAnswer
	}
	answers := make(chan answer, 2)
	for i, setting := range []string{"K0", "K1"} {
		go func() {
			registration, err := registerOn(t, database, clocks[i], "c1", setting)
			answers <- answer{index: i, registerAnswer: registerAnswer{registration: registration, err: err}}
		}()
	}
	storetest.WaitForLockWaiters(t, gate, 2)
	// Both transactions have begun by now: one holds the registry's lock and waits on the table,
	// the other waits on that lock. pg_stat_activity is read once, through the gate's connection.
	var queued int
	var apart float64
	if err := gate.QueryRow(context.Background(), `
		select count(*), coalesce(extract(epoch from max(xact_start) - min(xact_start)), 0)
		from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock'`).Scan(&queued, &apart); err != nil {
		t.Fatalf("read when the two registrations' transactions began: %v", err)
	}
	if queued != 2 {
		t.Fatalf("%d backends wait on a lock while the registry is held, want the two registrations", queued)
	}
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the registry: %v", err)
	}

	first := testwait.Receive(t, "the first of the two registrations to answer", answers)
	if first.err != nil {
		t.Fatalf("the first registration to answer answered %v, want acceptance: the other answers only once the test moves its clock", first.err)
	}
	holdLive(t, database, first.registration.ID)
	waiting := clocks[1-first.index]
	waiting.AwaitArmed(t, "the other registration's retry gap", 1)
	waiting.Advance(LiveWindow + HeartbeatInterval)
	second := testwait.Receive(t, "the other registration's answer once its wait was over", answers)
	return twoAtOnce{
		database:   database,
		begunApart: time.Duration(apart * float64(time.Second)),
		first:      first.registerAnswer,
		second:     second.registerAnswer,
	}
}

// A process killed without deregistering leaves a row that counts as live until LiveWindow after
// its last heartbeat, and a newcomer whose key set it does not share - a dev sign-in server
// restarted after a kill, whose key is minted per process - is refused beside it at first.
// Register waits for the row to lapse instead of refusing at once, and admits the newcomer then.
func TestARefusalBesideARowThatStopsHeartbeatingIsAdmittedOnceTheRowLapses(t *testing.T) {
	database := storetest.Open(t)
	fake := clocktest.New(time.Now())
	var dead string
	// The dead row is written a day ahead of the database's clock (livePeer), so it counts as
	// live until the test ages it below, however much time passes before then; and the newcomer's
	// row, once admitted, is held live the same way for the read that ends the row (holdLive), so
	// no verdict of this row depends on LiveWindow not passing.
	underRegistryLock(t, database, func(tx pgx.Tx) { dead = livePeer(t, tx, "K9") })

	// The newcomer is refused while the dead row still counts as live, and waits: its retry gap
	// is armed on this clock. The row then lapses on the database's clock, which this test
	// ages it against, and the retry the test brings admits the newcomer.
	answered := make(chan registerAnswer, 1)
	go func() {
		registration, err := registerOn(t, database, fake, "c2", "K0")
		answered <- registerAnswer{registration: registration, err: err}
	}()
	fake.AwaitArmed(t, "Register's retry gap", 1)
	// A newcomer admitted at once would arm its heartbeat's gap (Registration.heartbeat) here
	// instead, for the same deadline, so the timer alone does not tell the two apart. Admission
	// commits the newcomer's row (tryRegister) before RegisterOn starts that heartbeat, so the
	// dead row alone is live while the newcomer is refused and waits.
	live := liveIDs(t, database)
	if !slices.Contains(live, dead) {
		t.Fatalf("live = %v once a timer was armed on the newcomer's clock: the dead row %s lapsed before the test aged it, so the refusal this row reads was not tested", live, dead)
	}
	if !slices.Equal(live, []string{dead}) {
		t.Fatalf("live = %v once a timer was armed on the newcomer's clock, want the dead row %s alone: the newcomer was admitted beside it at once rather than refused and left waiting for it to lapse", live, dead)
	}
	ageHeartbeat(t, database.Pool, dead, LiveWindow+time.Second)
	fake.Advance(HeartbeatInterval)
	got := testwait.Receive(t, "the newcomer's answer once the dead row lapsed", answered)
	registration, err := got.registration, got.err
	if err != nil {
		t.Fatalf("a process beside a row that stopped heartbeating was refused: %v", err)
	}
	holdLive(t, database, registration.ID)
	if got := liveIDs(t, database); !slices.Equal(got, []string{registration.ID}) {
		t.Errorf("live = %v, want the admitted process %s alone, the dead row %s lapsed", got, registration.ID, dead)
	}
}

// Beside a peer that keeps heartbeating, Register's wait ends in the refusal: the peer's row stays
// live past LiveWindow and one HeartbeatInterval after the first refusal, and nothing is recorded.
//
// The wait runs on a frozen clock of its own, which this test moves by Set, so what the wait is
// worth is read off that clock rather than off the machine's: it answers its refusal once that
// clock has passed LiveWindow and one HeartbeatInterval, and nothing sooner. A clock whose Now
// drifted with the wall clock would re-arm the retry gap just past the moment Set moves to, and
// the gap would never fire. The peer heartbeats once on a second clock the test beats by hand,
// and its row is then put a day ahead of the database's clock (holdLive), so it is live
// throughout however long the machine takes over the wait.
func TestARefusalBesideAPeerThatKeepsHeartbeatingStandsAfterTheWait(t *testing.T) {
	database := storetest.Open(t)
	beating := clocktest.New(time.Now())
	peer := mustRegisterOn(t, database, beating, "c1", "K9")
	beating.AwaitArmed(t, "the peer's heartbeat", 1)
	since := testdb.Now(t, database.Pool)
	beating.Advance(HeartbeatInterval)
	waitForStamp(t, database, peer.ID, since, "the peer's heartbeat to stamp its row")
	holdLive(t, database, peer.ID)

	waiting := clocktest.New(time.Date(2026, time.November, 5, 3, 0, 0, 0, time.UTC))
	began := waiting.Now()
	refused := make(chan error, 1)
	go func() {
		_, err := registerOn(t, database, waiting, "c2", "K0")
		refused <- err
	}()
	// Each retry arms the gap to the next, and the registration's clock holds that gap alone:
	// a live window in, the wait has answered nothing.
	waiting.AwaitArmed(t, "Register's first retry gap", 1)
	waiting.Set(began.Add(LiveWindow))
	waiting.AwaitArmed(t, "Register's retry gap a live window in", 1)
	select {
	case err := <-refused:
		t.Fatalf("Register answered %v a live window in, want it waiting until LiveWindow and one HeartbeatInterval have passed", err)
	default:
	}

	waiting.Set(began.Add(LiveWindow + HeartbeatInterval))
	err := testwait.Receive(t, "Register's refusal once its wait for the peer to lapse is over", refused)
	if !errors.Is(err, ErrKeySetMismatch) || !strings.Contains(err.Error(), peer.ID) {
		t.Fatalf("a process beside a heartbeating peer with another key registered with %v, want ErrKeySetMismatch naming %s", err, peer.ID)
	}
	if got := liveIDs(t, database); !slices.Equal(got, []string{peer.ID}) {
		t.Errorf("live = %v after the refusal, want the peer %s alone", got, peer.ID)
	}
}

// Register's wait ends with its context: a process stopped (SIGTERM) while it waits for a refusing
// peer to lapse stops then, registered nowhere, not once the wait is over.
func TestRegistersWaitEndsWithItsContext(t *testing.T) {
	database := storetest.Open(t)
	fake := clocktest.New(time.Now())
	peer := mustRegisterOn(t, database, fake, "c1", "K9")
	// The peer must be live at the newcomer's first try, which it refuses, and at the read below,
	// however long the machine takes before them (holdLive); nothing moves its clock.
	holdLive(t, database, peer.ID)

	ctx, stop := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() {
		_, err := RegisterOn(ctx, fake, database.Pool, Instance{Commit: "c2", SigningKeys: fingerprints(t, "K0")})
		answered <- err
	}()
	// The wait is in its gap between retries, far from the moment it would give up, when the
	// process is stopped: nothing but the cancellation can answer it. The peer heartbeats on the
	// same fake, so its heartbeat timer is one of the two armed, and Register's gap is the other.
	fake.AwaitArmed(t, "Register's retry gap, beside the peer's heartbeat", 2)
	stop()
	if err := testwait.Receive(t, "Register's answer to its context ending", answered); !errors.Is(err, context.Canceled) {
		t.Fatalf("Register beside a refusing peer, its context ended, returned %v; want the context's error", err)
	}
	if got := liveIDs(t, database); !slices.Equal(got, []string{peer.ID}) {
		t.Errorf("live = %v, want the peer %s alone", got, peer.ID)
	}
}
