package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The owner lock beside an earlier build (ownerlock_fleet_test.go): the bound on an earlier
// build's registration's wait for the owner lock gate.

// ───── an earlier build's registration and its bound on the gate ───────────

// registrationGateBound bounds an earlier build's registration's wait for the owner lock gate:
// owner_lock_legacy_registration's lock_timeout, spelled again here, read back from the function's
// own settings by every run (registrationLockTimeout), and the timing lane's reading of the
// refusal.
const registrationGateBound = 5 * time.Second

// registrationLockTimeout is the setting owner_lock_legacy_registration runs under (pg_proc's
// proconfig), as Postgres holds it: what bounds the registration's wait, and the one place the
// migration (20261115200014_owner_lock_legacy_gate) sets it.
func registrationLockTimeout(t *testing.T, s *Store) []string {
	t.Helper()
	var config []string
	if err := s.Pool.QueryRow(context.Background(), `
		select coalesce(proconfig, '{}') from pg_proc where oid = 'public.owner_lock_legacy_registration()'::regprocedure
	`).Scan(&config); err != nil {
		t.Fatalf("read owner_lock_legacy_registration's settings: %v", err)
	}
	return config
}

// An earlier build's registration waits for the owner lock gate no longer than its lock_timeout. It
// holds the registry's lock while it waits, and every process's heartbeat takes that lock too, so
// behind a transaction of this build that holds the gate shared for longer, the registration
// fails with 55P03 lock_not_available, Postgres's answer at that bound, stamps no mark, and lets
// go of the registry's lock: a heartbeat of this build queued on it goes through. A dashboard
// publish keeps waiting past the bound, since the earlier build bounds its publish itself, and
// goes through once that transaction commits. Without the bound the registration waits for that
// transaction as long as the publish does, and the heartbeat with it, which the shared wait's end
// fails by name. The bound itself is the function's lock_timeout of 5 seconds, read off pg_proc,
// so a migration that raised it fails here; how long the refusal takes on the clock is the timing
// lane's (TestAnEarlierBuildsRegistrationIsRefusedWithinItsBoundOnTheClock). That the publish
// waits past the bound is read off that bound itself, and the row reads no clock: the publish
// waits for the gate first, then two registrations wait behind it one after the other, each until
// its lock_timeout refuses it, and the publish still waits. Each registration began to wait after
// the publish did, so by the second refusal the publish has waited longer than two of those
// lock_timeouts: a bound of the publish's own (in its session, the gate's trigger function, or a
// function that one calls) at the registration's 5 seconds, or up to about twice that, has ended
// its wait by then and fails the row by name.
//
// No clock decides a refusal either. Each must be 55P03 raised inside
// owner_lock_legacy_registration while the transaction it waits for still holds the gate: that
// refusal, and not a sight of the registration waiting, is what shows it waited for the gate and
// gave up at its own bound. A row slower than the bound between starting a registration and
// reading pg_locks finds it already refused, and judges that refusal the same way
// (awaitGateWaitOrEnd). A registration's own transaction stops a lock wait no sooner than the
// row's waits give up (setFleetLockTimeout), so no bound but the function's can refuse it while
// the row waits.
func TestAnEarlierBuildsRegistrationWaitsForTheGateNoLongerThanItsBound(t *testing.T) {
	t.Parallel()
	s, owners := openFleetStore(t)
	ctx := context.Background()
	if config, bound := registrationLockTimeout(t, s), fmt.Sprintf("lock_timeout=%ds", int(registrationGateBound/time.Second)); !slices.Contains(config, bound) {
		t.Fatalf("owner_lock_legacy_registration runs under %q, want %s, the bound its registration waits for the gate", config, bound)
	}
	mine := registerFleetProcess(t, s, OwnerLockProtocol, 0)
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if err := LockOwner(ctx, holder, owners.issueA); err != nil {
		t.Fatal(err)
	}
	if gate, granted := gateLock(t, s, holder.Conn().PgConn().PID()); gate != "ShareLock" || !granted {
		t.Fatalf("the transaction in flight holds the gate as %q (granted %t), want it held shared", gate, granted)
	}

	registration, publish := earlierBuildArrivals[1], earlierBuildArrivals[0]
	publishConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publishConn.Release()
	// No bound of the publish's own ends its wait at the registration's lock_timeout, read first
	// as settings where such a bound would stand: its session runs under no lock_timeout or
	// statement_timeout, and the gate's trigger function it waits in (owner_lock_legacy_arrival)
	// carries neither in its settings, as owner_lock_legacy_registration's settings carry its own
	// (registrationLockTimeout). The lock_timeout arriveOn sets in the publish's transaction is the
	// row's, the moment the shared wait gives up (setFleetLockTimeout), not the earlier build's. A
	// bound set anywhere else on the publish's path, in a function's body or a callee's settings,
	// is caught by what the publish does: it still waits for the gate once the registrations below
	// are refused.
	var sessionLockTimeout, sessionStatementTimeout string
	if err := publishConn.QueryRow(ctx, `select current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&sessionLockTimeout, &sessionStatementTimeout); err != nil {
		t.Fatalf("read %s's session timeouts: %v", publish.name, err)
	}
	if sessionLockTimeout != "0" || sessionStatementTimeout != "0" {
		t.Fatalf("%s's session runs under lock_timeout %s and statement_timeout %s, want neither: either ends its wait for the gate", publish.name, sessionLockTimeout, sessionStatementTimeout)
	}
	var arrivalConfig []string
	if err := s.Pool.QueryRow(ctx, `
		select coalesce(proconfig, '{}') from pg_proc where proname = 'owner_lock_legacy_arrival'
	`).Scan(&arrivalConfig); err != nil {
		t.Fatalf("read owner_lock_legacy_arrival's settings: %v", err)
	}
	if len(arrivalConfig) != 0 {
		t.Fatalf("owner_lock_legacy_arrival, the gate %s waits in, runs under settings %v, want no bound of its own", publish.name, arrivalConfig)
	}
	published := make(chan error, 1)
	go func() { published <- arriveOn(ctx, t, publishConn, publish, uuid.NewString()) }()
	publishPID := publishConn.Conn().PgConn().PID()
	awaitGateWait(t, s, publishPID, published, publish.name)
	// Two registrations, one after the other on a connection of their own, wait for the gate
	// behind the publish, and each is refused at its lock_timeout. Each began to wait after the
	// publish did, so the publish has waited for the gate longer than one lock_timeout at the first
	// refusal and longer than two at the second.
	registrationConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer registrationConn.Release()
	for _, nth := range []string{"first", "second"} {
		what := fmt.Sprintf("%s (the %s)", registration.name, nth)
		registered := make(chan error, 1)
		go func() { registered <- arriveOn(ctx, t, registrationConn, registration, uuid.NewString()) }()
		ended, err := awaitGateWaitOrEnd(t, s, registrationConn.Conn().PgConn().PID(), registered, what)
		beat := make(chan error, 1)
		go func() { beat <- heartbeatAsThisBuild(ctx, t, s, mine) }()
		if !ended {
			err = testwait.Receive(t, what+" behind a transaction holding the gate to be refused at its lock_timeout", registered)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || !strings.Contains(pgErr.Where, "owner_lock_legacy_registration") {
			t.Fatalf("%s behind a transaction holding the gate past the bound ended with %v, want 55P03 lock_not_available inside owner_lock_legacy_registration", what, err)
		}
		t.Logf("%s was refused: %s (%s)", what, pgErr.Message, pgErr.Where)
		if gate, granted := gateLock(t, s, holder.Conn().PgConn().PID()); gate != "ShareLock" || !granted {
			t.Fatalf("when %s was refused the transaction it waited for holds the gate as %q (granted %t), want it still held shared: only the function's bound can have refused it", what, gate, granted)
		}
		if err := testwait.Receive(t, "this build's heartbeat to go through once "+what+" was refused", beat); err != nil {
			t.Fatalf("this build's heartbeat after %s was refused: %v", what, err)
		}
		if hasLegacyMark(t, s) {
			t.Errorf("%s was refused and the earlier build's mark is stamped, want none", what)
		}
	}
	// The publish still waits for the gate after the second refusal, read once: its backend waits
	// on a Lock, its gate request is not granted, and it has not ended. Its end is read after its
	// backend, so a publish that a bound of its own cut off fails the row naming its error.
	var waitEventType string
	var gateWaiting bool
	if err := s.Pool.QueryRow(ctx, `
		select coalesce(a.wait_event_type, ''),
		  exists (select 1 from pg_locks l
		    where l.pid = a.pid and l.locktype = 'advisory' and not l.granted and l.mode = 'ExclusiveLock'
		      and l.database = (select oid from pg_database where datname = current_database())
		      and l.classid::bigint = $2 and l.objid::bigint = $3 and l.objsubid = 2)
		from pg_stat_activity a where a.pid = $1`,
		publishPID, OwnerLockGateNamespace, OwnerLockGateKey).Scan(&waitEventType, &gateWaiting); err != nil {
		t.Fatalf("read %s's wait on the database: %v", publish.name, err)
	}
	select {
	case err := <-published:
		t.Fatalf("%s ended (%v) while a transaction held the gate shared, by the time %s was refused twice at its lock_timeout; want it to keep waiting past the registration's bound",
			publish.name, err, registration.name)
	default:
	}
	if waitEventType != "Lock" || !gateWaiting {
		t.Fatalf("%s's backend has wait event type %q and waits for the gate %t once %s was refused twice at its lock_timeout, while a transaction holds the gate shared; want it still waiting for the gate",
			publish.name, waitEventType, gateWaiting, registration.name)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, publish.name+" to end once the transaction it waited for committed", published); err != nil {
		t.Fatalf("%s after the transaction it waited for committed: %v", publish.name, err)
	}
	if !hasLegacyMark(t, s) {
		t.Errorf("%s went through and the earlier build's mark is not stamped", publish.name)
	}
}

// The registration's refusal on the clock, main's reading of the bound: behind a transaction that
// holds the gate, the refusal comes within registrationGateBound and three seconds more of the
// registration's arrival. Its time is the machine's as much as the product's, so it is the timing
// lane's (benchtime.TimingEnv), which CI never runs; every run reads the bound off the function
// itself (TestAnEarlierBuildsRegistrationWaitsForTheGateNoLongerThanItsBound).
func TestAnEarlierBuildsRegistrationIsRefusedWithinItsBoundOnTheClock(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	s, owners := openFleetStore(t)
	ctx := context.Background()
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if err := LockOwner(ctx, holder, owners.issueA); err != nil {
		t.Fatal(err)
	}
	registration := earlierBuildArrivals[1]
	registrationConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer registrationConn.Release()
	registered := make(chan error, 1)
	began := time.Now()
	go func() { registered <- arriveOn(ctx, t, registrationConn, registration, uuid.NewString()) }()
	err = testwait.Receive(t, registration.name+" behind a transaction holding the gate to be refused at its lock_timeout", registered)
	took := time.Since(began)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("%s behind a transaction holding the gate ended with %v after %s, want 55P03 lock_not_available", registration.name, err, took)
	}
	if took > registrationGateBound+3*time.Second {
		t.Errorf("%s was refused %s after it started, past its bound of %s and three seconds more", registration.name, took.Round(time.Millisecond), registrationGateBound)
	}
	t.Logf("%s was refused after %s: %s", registration.name, took.Round(time.Millisecond), pgErr.Message)
}
