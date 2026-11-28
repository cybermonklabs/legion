package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// leaderReadLostBound is the bound the rows below hold each delete to, internal/bus's
// lostReadBound: well above the reconnect each row cuts the connection into (nats.go dials the same
// server again after its ReconnectWait, 1 s), which the row requires to take at most a quarter of
// it, so a delete that fails before half of it failed at the reconnect and not at its bound.
const leaderReadLostBound = 8 * time.Second

// relayedDeletes dials a client through a relay that drops the sends of subject once armed, opens
// the registry on that client's connection, and registers sessionID with entry, the record each
// row's read reads before the cut and again after it. It returns the relay, the client's
// connection and the registry.
func relayedDeletes(t *testing.T, subject, sessionID string, entry SessionEntry) (*testnats.UnansweredRelay, *natsgo.Conn, *SessionRegistry) {
	t.Helper()
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), subject)
	client, err := bus.Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	registry := openBindingRegistry(t, client)
	if err := registry.Put(sessionID, entry); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	return relay, client.Connection(), registry
}

// requireDeleteLostBeforeHalfItsBound arms relay to drop the next send of its subject, makes call
// within leaderReadLostBound on a goroutine of its own, and cuts every link of the relay once that
// send is out: the order of a request whose bytes reached the socket and died with it. It
// requires conn to reconnect within a quarter of the bound from call's start, and call to answer
// bus.ErrRequestLostWithConnection before half of it: a call that fails that soon failed at the
// reconnect, and one that waited for its bound fails the row.
func requireDeleteLostBeforeHalfItsBound(t *testing.T, relay *testnats.UnansweredRelay, conn *natsgo.Conn, what string, call func(context.Context) error) {
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

// requireDeleted requires the leader of the session bucket's stream to hold a delete marker as
// sessionID's newest message: the delete made again read the record registered before the cut and
// deleted it at its revision, where a read that answered the key absent deletes nothing.
func requireDeleted(t *testing.T, registry *SessionRegistry, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if leader, err := registry.watcher.KV().ReadFromLeader(ctx, sessionID); err != nil || leader.Kind != bus.LeaderMarker {
		t.Fatalf("the leader's newest message for %s after the delete made again = kind %d, %v; want the delete marker (kind %d)", sessionID, leader.Kind, err, bus.LeaderMarker)
	}
}

// A delete of a session's entry (SessionRegistry.Delete, DELETE /v1/sessions/<session_id>) whose
// leader read reached the socket and died with it fails with bus.ErrRequestLostWithConnection at
// the reconnect, rather than waiting out its bound for an answer nothing will send: nats.go fails
// no request its connection drops. The read is bus.KeyValue.DeleteAtRead's, a stream message get,
// `$JS.API.STREAM.MSG.GET.KV_<session bucket>`, on the registry's handle, which is opened on the
// connection of the client that dialled through the relay; the relay drops that request, and the
// cut comes once it is out. The reconnect takes at most a quarter of the bound, and the delete
// fails before half of it; the delete made again on the connection that came back reads the entry
// and deletes it. Control: with Delete's read made without bus.BoundedOn, it waits out its bound
// and the row fails.
func TestASessionDeleteLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const sessionID = "ses_delete_leader_lost"
	relay, conn, registry := relayedDeletes(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.SessionBucket, sessionID,
		SessionEntry{Port: 13381, MachineID: "test-machine"})

	requireDeleteLostBeforeHalfItsBound(t, relay, conn, "the delete of "+sessionID, func(ctx context.Context) error {
		return registry.Delete(ctx, sessionID)
	})

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if err := registry.Delete(again, sessionID); err != nil {
		t.Fatalf("the delete of %s again after the reconnect: %v", sessionID, err)
	}
	requireDeleted(t, registry, sessionID)
}

// A conditional delete of a session's entry (SessionRegistry.DeleteIf, the stop that names a
// client instance or a moment) whose first read of the entry (readForStop) reached the socket and
// died with it fails with bus.ErrRequestLostWithConnection at the reconnect, rather than waiting
// out its bound. The entry the stop reads is there and is the stopping instance's, so that read is
// bus.KeyValue.ReadStored's direct get alone,
// `$JS.API.DIRECT.GET.KV_<session bucket>.$KV.<session bucket>.<session id>`, which the relay
// drops before the cut. The reconnect takes at most a quarter of the bound, and the stop fails
// before half of it; the stop made again on the connection that came back reads the entry and
// deletes it. Control: with DeleteIf's read made without bus.BoundedOn, it waits out its bound and
// the row fails.
func TestAConditionalDeleteLostWithItsConnectionAtItsFirstReadFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		sessionID = "ses_delete_if_first_read_lost"
		instance  = "instance-stopping"
	)
	relay, conn, registry := relayedDeletes(t, "$JS.API.DIRECT.GET.KV_"+bus.SessionBucket+".$KV."+bus.SessionBucket+"."+sessionID, sessionID,
		SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: instance})
	stop := DeleteConditions{ClientInstanceID: instance}

	requireDeleteLostBeforeHalfItsBound(t, relay, conn, "the conditional delete of "+sessionID, func(ctx context.Context) error {
		_, err := registry.DeleteIf(ctx, sessionID, stop)
		return err
	})

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if outcome, err := registry.DeleteIf(again, sessionID, stop); err != nil || outcome != EntryDeleted {
		t.Fatalf("the conditional delete of %s again after the reconnect = %s, %v; want %s", sessionID, outcome, err, EntryDeleted)
	}
	requireDeleted(t, registry, sessionID)
}

// A conditional delete whose read of the entry from the bucket stream's leader (readForStop with
// fromLeader, GetFromLeader) reached the socket and died with it fails with
// bus.ErrRequestLostWithConnection at the reconnect, rather than waiting out its bound. The stored
// entry is another client instance's, so the stop's first read, a direct get, answers a keep, and
// the stop reads the entry again from the leader before it answers that keep: a stream message
// get, `$JS.API.STREAM.MSG.GET.KV_<session bucket>`, which the relay drops before the cut. That
// read is a bus.BoundedOn wait of its own, the second the stop makes: the keep runs DeleteIf's
// loop again (`continue`) with fromLeader set, and the loop's read is bounded anew, as the direct
// get's was. The reconnect takes at most a quarter of the bound, and the stop fails before half of
// it; the stop made again on the connection that came back reads the entry and keeps it for its
// instance. Control: with DeleteIf's read made without bus.BoundedOn, it waits out its bound and
// the row fails.
func TestAConditionalDeleteLostWithItsConnectionAtItsLeaderReadFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const sessionID = "ses_delete_if_leader_read_lost"
	relay, conn, registry := relayedDeletes(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.SessionBucket, sessionID,
		SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: "instance-live"})
	stop := DeleteConditions{ClientInstanceID: "instance-stopping"}

	requireDeleteLostBeforeHalfItsBound(t, relay, conn, "the conditional delete of "+sessionID, func(ctx context.Context) error {
		_, err := registry.DeleteIf(ctx, sessionID, stop)
		return err
	})

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if outcome, err := registry.DeleteIf(again, sessionID, stop); err != nil || outcome != EntryKeptForAnotherInstance {
		t.Fatalf("the conditional delete of %s again after the reconnect = %s, %v; want %s", sessionID, outcome, err, EntryKeptForAnotherInstance)
	}
}

// A conditional delete whose fenced delete lost its compare-and-swap, and whose read of the entry
// from the bucket stream's leader after that loss (bus.KeyValue.DeleteAt's ReadFromLeader) reached
// the socket and died with it, fails with bus.ErrRequestLostWithConnection at the reconnect,
// rather than waiting out its bound. That read is the fenced delete's, inside the bus.BoundedOn
// wait DeleteIf makes for kv.DeleteAt, the second of the stop after the one its read of the entry
// answered in. The entry is the stopping instance's, so the stop's first read, a direct get,
// admits it, and the stop deletes it fenced on that read's revision; a heartbeat of the same
// instance lands between the read and the delete (heartbeatBeforeDeletes, the loss
// TestSessionRegistryDeleteIf_RetriesOnceAfterALostCompareAndSwap arranges), so the bucket refuses
// the delete and DeleteAt reads the entry from the leader: a stream message get,
// `$JS.API.STREAM.MSG.GET.KV_<session bucket>`, the first the stop sends, which the relay drops
// before the cut. The reconnect takes at most a quarter of the bound, and the stop fails before
// half of it; the stop made again on the connection that came back reads the heartbeat and deletes
// it. Control: with DeleteIf's fenced delete made without bus.BoundedOn, it waits out its bound and
// the row fails.
func TestAConditionalDeleteLostWithItsConnectionAtItsFencedDeletesLeaderReadFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		sessionID = "ses_delete_if_fenced_read_lost"
		instance  = "instance-stopping"
	)
	entry := SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: instance}
	relay, conn, registry := relayedDeletes(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.SessionBucket, sessionID, entry)
	entry.UpdatedAt = time.Now().UnixMilli()
	heartbeat, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode the interleaved heartbeat: %v", err)
	}
	deletes := heartbeatBeforeDeletes(t, registry, sessionID, heartbeat, 1)
	stop := DeleteConditions{ClientInstanceID: instance}

	requireDeleteLostBeforeHalfItsBound(t, relay, conn, "the conditional delete of "+sessionID, func(ctx context.Context) error {
		_, err := registry.DeleteIf(ctx, sessionID, stop)
		return err
	})
	if got := deletes(); got != 1 {
		t.Fatalf("the stop cut under its fenced delete's leader read made %d fenced deletes, want the one the heartbeat beat (1)", got)
	}

	again, cancel := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancel()
	if outcome, err := registry.DeleteIf(again, sessionID, stop); err != nil || outcome != EntryDeleted {
		t.Fatalf("the conditional delete of %s again after the reconnect = %s, %v; want %s", sessionID, outcome, err, EntryDeleted)
	}
	requireDeleted(t, registry, sessionID)
}
