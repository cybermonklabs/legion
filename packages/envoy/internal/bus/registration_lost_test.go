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
