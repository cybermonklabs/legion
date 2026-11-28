package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// relayedRegistry dials a client through a relay that drops the sends of subject once armed and
// opens the registry on that client's connection. It returns the relay, the client's connection
// and the registry.
func relayedRegistry(t *testing.T, subject string) (*testnats.UnansweredRelay, *natsgo.Conn, *Registry) {
	t.Helper()
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), subject)
	client, err := bus.Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	return relay, client.Connection(), openRegistry(t, client.Conn)
}

// requireLostBeforeHalfTheBound arms relay to drop the next send of its subject, makes call within
// leaderReadLostBound on a goroutine of its own, and cuts every link of the relay once that send
// is out: the order of a request whose bytes reached the socket and died with it. It requires conn
// to reconnect within a quarter of the bound from call's start, and call to answer
// bus.ErrRequestLostWithConnection before half of it: a call that fails that soon failed at the
// reconnect, and one that waited for its bound fails the row.
func requireLostBeforeHalfTheBound(t *testing.T, relay *testnats.UnansweredRelay, conn *natsgo.Conn, what string, call func(context.Context) error) {
	t.Helper()
	relay.Arm(1)
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	type answer struct {
		err error
		at  time.Time
	}
	answers := make(chan answer, 1)
	go func() {
		err := call(ctx)
		answers <- answer{err: err, at: time.Now()}
	}()
	testwait.WithinPolling(t, leaderReadLostBound, testwait.Poll, "the relay to count a send of "+relay.Subject(), func() bool { return relay.Sent() >= 1 })
	before := conn.Stats().Reconnects
	relay.Drop()
	testwait.WithinPolling(t, leaderReadLostBound, testwait.Poll, "the connection to reconnect through the relay", func() bool { return conn.Stats().Reconnects > before })
	reconnected := time.Now()
	if took := reconnected.Sub(began); took > leaderReadLostBound/4 {
		t.Fatalf("the connection reconnected %s after %s began, past a quarter of its %s bound: the row cannot tell a call that failed at the reconnect from one that failed at its bound",
			took.Round(time.Millisecond), what, leaderReadLostBound)
	}
	var got answer
	select {
	case got = <-answers:
	case <-time.After(leaderReadLostBound + time.Second):
		t.Fatalf("%s whose connection was cut has not answered %s after it began", what, leaderReadLostBound+time.Second)
	}
	if took := got.at.Sub(began); !errors.Is(got.err, bus.ErrRequestLostWithConnection) || took >= leaderReadLostBound/2 {
		t.Fatalf("%s whose connection was cut under it answered %v, %s after it began and %s after the reconnect; want %v before half its %s bound",
			what, got.err, took.Round(time.Millisecond), got.at.Sub(reconnected).Round(time.Millisecond), bus.ErrRequestLostWithConnection, leaderReadLostBound)
	}
}

// A deregistration's delete of a session's interest record (Registry.Remove with no topics, whose
// delete is deleteInterest) whose leader read reached the socket and died with it fails with
// bus.ErrRequestLostWithConnection at the reconnect, rather than waiting out its bound for an
// answer nothing will send: nats.go fails no request its connection drops. The read is
// bus.KeyValue.DeleteAtRead's, a stream message get, `$JS.API.STREAM.MSG.GET.KV_<interest
// bucket>`, on the registry's handle, which is opened on the connection of the client that
// dialled through the relay; the relay drops that request, and the cut comes once it is out.
// Remove reads the record by a direct get and releases the session's role claim before that
// delete, so the claim found free after the cut says the request lost was the delete's, and not
// the record's read, which a relay armed on the direct get would lose. The reconnect takes at most
// a quarter of the bound, and the deregistration fails before half of it; the deregistration made
// again on the connection that came back reads the record and deletes it. Control: with
// deleteInterest's read made without bus.BoundedOn, it waits out its bound and the row fails.
func TestAnInterestDeleteLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		holder = "ses_interest_delete_leader_lost"
		role   = "reviewer"
	)
	relay, conn, registry := relayedRegistry(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.InterestBucket)
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("%s claims %s: %v", holder, role, err)
	}
	deregister := func(ctx context.Context) error {
		_, err := registry.Remove(ctx, holder, nil, RoleReleaseAuthority{Force: true})
		return err
	}

	requireLostBeforeHalfTheBound(t, relay, conn, "the delete of "+holder+"'s interest record", deregister)

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if claim, err := registry.RoleClaim(again, role); err != nil || claim.Revision != 0 {
		t.Fatalf("%s's claim after the deregistration whose delete was lost = %+v, %v; want it released, so the request lost was the record's delete", role, claim, err)
	}
	if err := deregister(again); err != nil {
		t.Fatalf("the deregistration of %s again after the reconnect: %v", holder, err)
	}
	if record, _, err := registry.RefreshAt(again, holder); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("%s's interest record after the deregistration made again = %+v, %v; want %v", holder, record, err, natsgo.ErrKeyNotFound)
	}
}

// A read of the holders of the controller notice lanes a session's topics name
// (Registry.ReadLaneHolders, which decides that session's consumer filter) whose read of the
// controller role's claim from the role bucket stream's leader (roleClaimFromLeader) reached the
// socket and died with it fails with bus.ErrRequestLostWithConnection at the reconnect, rather
// than waiting out its bound. The topic is a controller notice lane, so the claim is read: a
// stream message get, `$JS.API.STREAM.MSG.GET.KV_<role bucket>`, which the relay drops before the
// cut. The reconnect takes at most a quarter of the bound, and the read fails before half of it;
// the read made again on the connection that came back names the claim's holder. Control: with
// roleClaimFromLeader's read made without bus.BoundedOn, it waits out its bound and the row fails.
func TestALaneHoldersReadLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const holder = "ses_lane_holders_leader_lost"
	role := contracts.ControllerRoleToken("lost")
	lane, ok := bus.ControllerNoticeLane(role)
	if !ok {
		t.Fatalf("%s names no controller notice lane", role)
	}
	relay, conn, registry := relayedRegistry(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.RoleClaimBucket)
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("%s claims %s: %v", holder, role, err)
	}

	requireLostBeforeHalfTheBound(t, relay, conn, "the read of "+lane+"'s holder", func(ctx context.Context) error {
		_, err := registry.ReadLaneHolders(ctx, []string{lane})
		return err
	})

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if holders, err := registry.ReadLaneHolders(again, []string{lane}); err != nil || holders[role] != holder {
		t.Fatalf("the read of %s's holder again after the reconnect = %v, %v; want %s holding %s", lane, holders, err, holder, role)
	}
}

// A plan made again after a refused registration write (Registry.PlanInterestAfterConflict) whose
// read of the session's interest record from the bucket stream's leader (readInterest, readLeader)
// reached the socket and died with it fails with bus.ErrRequestLostWithConnection at the
// reconnect, rather than waiting out its bound. The read is bus.KeyValue.ReadFromLeader's, a
// stream message get, `$JS.API.STREAM.MSG.GET.KV_<interest bucket>`, which the relay drops before
// the cut. The reconnect takes at most a quarter of the bound, and the plan fails before half of
// it; the plan made again on the connection that came back is made over the stored record, at its
// revision. Control: with readInterest's leader read made without bus.BoundedOn, it waits out its
// bound and the row fails.
func TestAPlanAfterAConflictLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		sessionID = "ses_plan_leader_read_lost"
		stored    = "notifications.team.stored"
		requested = "notifications.team.requested"
	)
	relay, conn, registry := relayedRegistry(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.InterestBucket)
	if _, err := registry.Upsert(Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{stored}); err != nil {
		t.Fatalf("register %s's interests: %v", sessionID, err)
	}
	_, revision, err := registry.RefreshAt(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("read %s's interest record: %v", sessionID, err)
	}

	requireLostBeforeHalfTheBound(t, relay, conn, "the plan of "+sessionID+"'s registration after a conflict", func(ctx context.Context) error {
		_, err := registry.PlanInterestAfterConflict(ctx, sessionID, []string{requested})
		return err
	})

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if plan, err := registry.PlanInterestAfterConflict(again, sessionID, []string{requested}); err != nil || plan.Revision != revision || !slices.Contains(plan.Topics, stored) {
		t.Fatalf("the plan of %s's registration again after the reconnect = %+v, %v; want one over the stored record at revision %d, holding %s", sessionID, plan, err, revision, stored)
	}
}

// A read of a session's interest record from the bucket rather than the cache (Registry.RefreshAt,
// the read a relay restart makes of the session's row) whose request reached the socket and died
// with it fails with bus.ErrRequestLostWithConnection at the reconnect, rather than waiting out its
// bound. It is readInterest's other read, bus.KeyValue.ReadStored (latestStored for a caller that
// plans a write), and the record is the session's own row, so it is the direct get alone,
// `$JS.API.DIRECT.GET.KV_<interest bucket>.$KV.<interest bucket>.<session id>`, which the relay
// drops before the cut. The reconnect takes at most a quarter of the bound, and the read fails
// before half of it; the read made again on the connection that came back answers the record.
// Control: with that read made without bus.BoundedOn, it waits out its bound and the row fails.
func TestAnInterestRefreshLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		sessionID = "ses_interest_refresh_lost"
		topic     = "notifications.team.stored"
	)
	relay, conn, registry := relayedRegistry(t, "$JS.API.DIRECT.GET.KV_"+bus.InterestBucket+".$KV."+bus.InterestBucket+"."+sessionID)
	if _, err := registry.Upsert(Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interests: %v", sessionID, err)
	}

	requireLostBeforeHalfTheBound(t, relay, conn, "the read of "+sessionID+"'s interest record", func(ctx context.Context) error {
		_, _, err := registry.RefreshAt(ctx, sessionID)
		return err
	})

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if record, revision, err := registry.RefreshAt(again, sessionID); err != nil || revision == 0 || !slices.Contains(record.Topics, topic) {
		t.Fatalf("the read of %s's interest record again after the reconnect = %+v at revision %d, %v; want the stored record holding %s", sessionID, record, revision, err, topic)
	}
}
