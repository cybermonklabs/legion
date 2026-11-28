package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The owner lock beside an earlier build (ownerlock_fleet_test.go): the bound on an earlier
// build's registration's wait for the owner lock gate.

// ───── an earlier build's registration and its bound on the gate ───────────

// registrationGateBound bounds an earlier build's registration's wait for the owner lock gate:
// owner_lock_legacy_registration's lock_timeout, spelled again here and read back from the
// function's own settings below.
const registrationGateBound = 5 * time.Second

// An earlier build's registration waits for the owner lock gate at most registrationGateBound. It
// holds the registry's lock while it waits, and every process's heartbeat takes that lock too, so
// behind a transaction of this build that holds the gate shared for longer, the registration
// fails with 55P03 lock_not_available once the bound has passed, stamps no mark, and lets go of the
// registry's lock: a heartbeat of this build queued on it goes through. A dashboard publish keeps
// waiting past the bound, since the earlier build bounds its publish itself, and goes through once
// that transaction commits. Without the bound the registration waits for that transaction as
// long as the publish does, and the heartbeat with it.
//
// No clock decides the bound. The row reads it from the function's settings (pg_proc.proconfig),
// and requires the registration's refusal to be 55P03 raised inside that function while the
// transaction it waits for still holds the gate: that refusal, and not a sight of the registration
// waiting, is what shows it waited for the gate and gave up at its own bound. A row slower than
// the bound between starting the registration and reading pg_locks finds it already refused, and
// judges that refusal the same way (awaitGateWaitOrEnd). The registration's own transaction stops
// a lock wait no sooner than the row's waits give up (setFleetLockTimeout), so no bound but the
// function's can refuse it while the row waits.
//
// No clock decides that the publish waits past the bound either: the bound itself does. The
// publish waits for the gate first, then two registrations wait behind it one after the other,
// each until its bound refuses it, and the publish is read still waiting. Each registration began
// to wait after the publish did, so by the second refusal the publish has waited longer than two
// of those bounds: a bound of the publish's own up to twice the registration's has ended its wait
// by then and fails the row by name. Before it starts, the row reads that no setting where such a
// bound would stand holds one: the publish's session runs under no lock_timeout or
// statement_timeout, and the gate's trigger function it waits in (owner_lock_legacy_arrival)
// carries neither in its settings, as owner_lock_legacy_registration's settings carry its own.
func TestAnEarlierBuildsRegistrationWaitsForTheGateNoLongerThanItsBound(t *testing.T) {
	t.Parallel()
	s, owners := openFleetStore(t)
	ctx := context.Background()
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
	var settings []string
	if err := s.Pool.QueryRow(ctx, `select coalesce(proconfig, '{}') from pg_proc where oid = 'public.owner_lock_legacy_registration()'::regprocedure`).Scan(&settings); err != nil {
		t.Fatalf("read owner_lock_legacy_registration's settings: %v", err)
	}
	if bound := fmt.Sprintf("lock_timeout=%ds", int(registrationGateBound/time.Second)); !slices.Contains(settings, bound) {
		t.Fatalf("owner_lock_legacy_registration runs with the settings %v, want %s: nothing bounds %s's wait for the gate", settings, bound, registration.name)
	}
	publishConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publishConn.Release()
	// The lock_timeout arriveOn sets in the publish's transaction is the row's, the moment the
	// shared wait gives up (setFleetLockTimeout), not the earlier build's.
	var sessionLockTimeout, sessionStatementTimeout string
	if err := publishConn.QueryRow(ctx, `select current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&sessionLockTimeout, &sessionStatementTimeout); err != nil {
		t.Fatalf("read %s's session timeouts: %v", publish.name, err)
	}
	if sessionLockTimeout != "0" || sessionStatementTimeout != "0" {
		t.Fatalf("%s's session runs under lock_timeout %s and statement_timeout %s, want neither: either ends its wait for the gate", publish.name, sessionLockTimeout, sessionStatementTimeout)
	}
	var arrivalSettings []string
	if err := s.Pool.QueryRow(ctx, `select coalesce(proconfig, '{}') from pg_proc where oid = 'public.owner_lock_legacy_arrival()'::regprocedure`).Scan(&arrivalSettings); err != nil {
		t.Fatalf("read owner_lock_legacy_arrival's settings: %v", err)
	}
	if len(arrivalSettings) != 0 {
		t.Fatalf("owner_lock_legacy_arrival, the gate %s waits in, runs with the settings %v, want no bound of its own", publish.name, arrivalSettings)
	}
	published := make(chan error, 1)
	go func() { published <- arriveOn(ctx, t, publishConn, publish, uuid.NewString()) }()
	publishPID := publishConn.Conn().PgConn().PID()
	awaitGateWait(t, s, publishPID, published, publish.name)

	// Two registrations, one after the other on a connection of their own, wait for the gate
	// behind the publish, and each is refused at its bound.
	registrationConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer registrationConn.Release()
	for _, nth := range []string{"first", "second"} {
		which := fmt.Sprintf("%s (the %s)", registration.name, nth)
		registered := make(chan error, 1)
		go func() { registered <- arriveOn(ctx, t, registrationConn, registration, uuid.NewString()) }()
		ended, err := awaitGateWaitOrEnd(t, s, registrationConn.Conn().PgConn().PID(), registered, which)
		beat := make(chan error, 1)
		go func() { beat <- heartbeatAsThisBuild(ctx, t, s, mine) }()
		if !ended {
			err = testwait.Receive(t, which+" to be refused at its bound on the gate", registered)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || !strings.Contains(pgErr.Where, "owner_lock_legacy_registration") {
			t.Fatalf("%s behind a transaction holding the gate past the bound ended with %v, want 55P03 lock_not_available inside owner_lock_legacy_registration", registration.name, err)
		}
		t.Logf("%s was refused: %s (%s)", which, pgErr.Message, pgErr.Where)
		if gate, granted := gateLock(t, s, holder.Conn().PgConn().PID()); gate != "ShareLock" || !granted {
			t.Fatalf("when %s was refused the transaction it waited for holds the gate as %q (granted %t), want it still held shared: only the function's bound can have refused it", registration.name, gate, granted)
		}
		if err := testwait.Receive(t, "this build's heartbeat to go through once "+which+" was refused", beat); err != nil {
			t.Fatalf("this build's heartbeat after %s was refused: %v", registration.name, err)
		}
		if hasLegacyMark(t, s) {
			t.Errorf("%s was refused and the earlier build's mark is stamped, want none", registration.name)
		}
	}

	// The publish still waits for the gate after the second refusal, read once: its gate request
	// is not granted, and it has not ended. Its end is read after its gate, so a publish that a
	// bound of its own cut off between the two reads fails the row naming its error.
	gate, granted := gateLock(t, s, publishPID)
	select {
	case err := <-published:
		t.Fatalf("%s ended (%v) while the transaction it waits for was still under way, by the time %s was refused twice at its bound; want it to wait as long as the earlier build's publish lets it",
			publish.name, err, registration.name)
	default:
	}
	if gate != "ExclusiveLock" || granted {
		t.Fatalf("%s holds or waits for the gate as %q (granted %t) once %s was refused twice at its bound, while a transaction holds the gate shared; want it still waiting for the gate exclusively",
			publish.name, gate, granted, registration.name)
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
