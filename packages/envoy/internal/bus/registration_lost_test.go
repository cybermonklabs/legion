package bus_test

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A session registration's compare-and-set write (session.SessionRegistry.PutWithin, the write
// POST /v1/interests/subscribe makes of the session's entry) whose request reached the socket and
// died with it fails with bus.ErrRequestLostWithConnection at the reconnect, rather than waiting
// out the registration's budget for an answer nothing will send. The relay drops the write's
// JetStream publish to the session's key, so nothing stores it, and then cuts the link: nats.go
// fails no request its connection drops, so without the connection watch the write waited out its
// whole context and the registration answered 503 after it.
// It must not run in parallel: the write must fail within 500 ms of the reconnect it times.
func TestARegistrationWriteLostWithItsConnectionFailsAtTheReconnect(t *testing.T) {
	const sessionID = "ses_registration_write_lost"
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), "$KV."+bus.SessionBucket+"."+sessionID)
	client, err := bus.Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)

	entry := session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + sessionID, Title: sessionID}
	bus.RequireLostWithTheConnection(t, relay, client.Connection(), "a registration's write", func(ctx context.Context) error {
		return sessions.PutWithin(ctx, sessionID, entry)
	})

	// The registration made again on the connection that came back is stored.
	if err := sessions.PutWithin(context.Background(), sessionID, entry); err != nil {
		t.Fatalf("the registration again after the reconnect: %v", err)
	}
}

// A registration write that lost to another write reads the entry again from the bucket stream's
// leader (session's readLatest, a stream message get), and that read, whose request reached the
// socket and died with it, fails with bus.ErrRequestLostWithConnection at the reconnect, as the
// direct get it replaced on that path does, rather than holding the registration for its whole
// budget for an answer nothing will send. The registry's copy is held at its own write by stopping
// its watch, and another write of the entry lands behind it, so the registration's first write
// loses and its next read is the leader's. The relay drops that read's request and then cuts the
// link. Control: with the leader read made without bus.BoundedOn (readLatest before this row), it
// waits out the call's context and the row fails.
// It must not run in parallel: the leader read must fail within 500 ms of the reconnect it times.
func TestARegistrationWritesLeaderReadLostWithItsConnectionFailsAtTheReconnect(t *testing.T) {
	const sessionID = "ses_registration_leader_read_lost"
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), "$JS.API.STREAM.MSG.GET.KV_"+bus.SessionBucket)
	client, err := bus.Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	entry := session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + sessionID, Title: sessionID}
	if err := sessions.PutWithin(t.Context(), sessionID, entry); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	sessions.StopWatch()
	bucket, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := bucket.Put(sessionID, []byte(`{"machine_id":"another-listener"}`)); err != nil {
		t.Fatalf("write %s's entry from another listener: %v", sessionID, err)
	}

	bus.RequireLostWithTheConnection(t, relay, client.Connection(), "a registration's leader read after a lost write", func(ctx context.Context) error {
		return sessions.PutWithin(ctx, sessionID, entry)
	})

	// The registration made again on the connection that came back is stored.
	if err := sessions.PutWithin(context.Background(), sessionID, entry); err != nil {
		t.Fatalf("the registration again after the reconnect: %v", err)
	}
}

// A refresh of a session's record from the bucket stream's leader
// (session.SessionRegistry.RefreshFromLeader, the read cmd/listener's delete of a session's
// consumer makes before it deletes) whose request reached the socket and died with it fails with
// bus.ErrRequestLostWithConnection at the reconnect, as the registration's leader read above does,
// rather than holding its caller for its whole budget for an answer nothing will send. The relay
// drops that read's stream message get and then cuts the link. Control: with the leader read made
// without bus.BoundedOn (RefreshFromLeader before this row), it waits out the call's context and
// the row fails.
// It must not run in parallel: the refresh must fail within 500 ms of the reconnect it times.
func TestASessionRefreshFromTheLeaderLostWithItsConnectionFailsAtTheReconnect(t *testing.T) {
	const sessionID = "ses_refresh_leader_read_lost"
	relay := testnats.StartUnansweredRelayOn(t, testnats.URL(t), "$JS.API.STREAM.MSG.GET.KV_"+bus.SessionBucket)
	client, err := bus.Connect([]string{relay.URL()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	entry := session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + sessionID, Title: sessionID}
	if err := sessions.PutWithin(t.Context(), sessionID, entry); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}

	bus.RequireLostWithTheConnection(t, relay, client.Connection(), "a refresh of a session's record from the leader", func(ctx context.Context) error {
		_, err := sessions.RefreshFromLeader(ctx, sessionID)
		return err
	})

	// The refresh made again on the connection that came back reads the record.
	if got, err := sessions.RefreshFromLeader(context.Background(), sessionID); err != nil || got.Title != sessionID {
		t.Fatalf("the refresh again after the reconnect = %+v, %v; want %s's record", got, err, sessionID)
	}
}
