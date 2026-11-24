package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A takeover of the publisher hold waits for an expiry record its holder has open, so the new
// holder reads every write that record makes - a note it adds to a window among them - before it
// can post that window. With the hold read and not locked, the takeover goes through while the
// record is still open, and the old holder writes after the new one holds the hold.
func TestATakeoverOfThePublisherHoldWaitsForAnOpenExpiryRecord(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-record-open"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	locked, release := make(chan struct{}), make(chan struct{})
	deps := rig.deps()
	deps.holdLocked = func() {
		close(locked)
		<-release
	}
	outbox := holding(t, deps)
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	expire(t, rig.client, role, lane[0])
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		outbox.passKeptExpiries(ctx, unfenced{})
	}()
	select {
	case <-locked:
	case <-time.After(30 * time.Second):
		t.Fatal("the expiry record did not reach its hold check within 30s")
	}
	tookOver := make(chan error, 1)
	go func() {
		_, err := rig.database.Pool.Exec(ctx, `
			insert into duty_holds (name, holder, expires_at) values ($1, $2, now() + interval '1 hour')
			on conflict (name) do update set holder = excluded.holder, expires_at = excluded.expires_at
		`, publisherHold, uuid.New())
		tookOver <- err
	}()
	select {
	case err := <-tookOver:
		close(release)
		<-passed
		t.Fatalf("another process took the publisher hold (%v) while the holder's expiry record was open", err)
	case <-time.After(time.Second):
	}
	close(release)
	select {
	case <-passed:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's pass did not return within 30s of its release")
	}
	select {
	case err := <-tookOver:
		if err != nil {
			t.Fatalf("the takeover failed once the record committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the takeover did not go through within 30s of the record's commit")
	}
	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 {
		t.Fatalf("the holder's record wrote %+v, want its one expiry, committed before the takeover", rows)
	}
}
