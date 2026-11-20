package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The owner lock beside an earlier build (ownerlock_fleet_test.go): the bound on an earlier
// build's registration's wait for the owner lock gate.

// ───── an earlier build's registration and its bound on the gate ───────────

// registrationGateBound bounds an earlier build's registration's wait for the owner lock gate:
// owner_lock_legacy_registration's lock_timeout, spelled again here.
const registrationGateBound = 5 * time.Second

// An earlier build's registration waits for the owner lock gate at most registrationGateBound. It
// holds the registry's lock while it waits, and every process's heartbeat takes that lock too, so
// behind a transaction of this build that holds the gate shared for longer, the registration
// fails with 55P03 lock_not_available once the bound has passed, stamps no mark, and lets go of the
// registry's lock: a heartbeat of this build queued on it goes through. A dashboard publish keeps
// waiting past the bound, since the earlier build bounds its publish itself, and goes through once
// that transaction commits. Without the bound the registration waits for that transaction as
// long as the publish does, and the heartbeat with it.
func TestAnEarlierBuildsRegistrationWaitsForTheGateNoLongerThanItsBound(t *testing.T) {
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
	registrationConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer registrationConn.Release()
	registered := make(chan error, 1)
	began := time.Now()
	go func() { registered <- arriveOn(ctx, t, registrationConn, registration, uuid.NewString()) }()
	awaitGateWait(t, s, registrationConn.Conn().PgConn().PID(), registered, registration.name)
	beat := make(chan error, 1)
	go func() { beat <- heartbeatAsThisBuild(ctx, t, s, mine) }()
	select {
	case err := <-registered:
		took := time.Since(began)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("%s behind a transaction holding the gate past the bound ended with %v after %s, want 55P03 lock_not_available", registration.name, err, took)
		}
		t.Logf("%s was refused after %s: %s", registration.name, took.Round(time.Millisecond), pgErr.Message)
	case <-time.After(registrationGateBound + 3*time.Second):
		t.Errorf("%s still waited for the gate %s after it started, past its bound of %s", registration.name, time.Since(began).Round(time.Millisecond), registrationGateBound)
		_ = holder.Rollback(ctx)
		<-registered
		<-beat
		t.FailNow()
	}
	select {
	case err := <-beat:
		if err != nil {
			t.Fatalf("this build's heartbeat after %s was refused: %v", registration.name, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("this build's heartbeat did not go through within 5s of %s's refusal", registration.name)
	}
	if hasLegacyMark(t, s) {
		t.Errorf("%s was refused and the earlier build's mark is stamped, want none", registration.name)
	}

	publishConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publishConn.Release()
	published := make(chan error, 1)
	go func() { published <- arriveOn(ctx, t, publishConn, publish, uuid.NewString()) }()
	awaitGateWait(t, s, publishConn.Conn().PgConn().PID(), published, publish.name)
	select {
	case err := <-published:
		t.Fatalf("%s ended (%v) within %s while the transaction it waits for was still under way; want it to wait as long as the earlier build's publish lets it",
			publish.name, err, registrationGateBound+time.Second)
	case <-time.After(registrationGateBound + time.Second):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("%s after the transaction it waited for committed: %v", publish.name, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not end within 10s of the commit of the transaction it waited for", publish.name)
	}
	if !hasLegacyMark(t, s) {
		t.Errorf("%s went through and the earlier build's mark is not stamped", publish.name)
	}
}
