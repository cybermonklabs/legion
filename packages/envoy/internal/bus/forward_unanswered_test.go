package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A forward that failed is ErrUnacknowledged exactly when it went out and nothing answered it, so
// that the stream may hold it: no acknowledgement within its window, a wait its connection closed
// under, a publish the client could not confirm and nothing then answered, and a publish nats.go
// took and could not flush. The listener reads it so (cmd/listener's mayHaveStored): after such a
// forward, a role send's later refusal for want of a subject says the message may be stored. A
// forward the server answered stored nothing and is not ErrUnacknowledged: no stream took it (no
// responders), or the stream refused it with its error. Nor is a forward refused before anything
// was sent, or one the server denied. Every forward that failed after it went out is
// ErrForwardNotStored. The control is a forward the stream stored.
func TestAForwardMayHaveStoredOnlyWhenItWentOutAndNothingAnsweredIt(t *testing.T) {
	envelope := contracts.Envelope{
		EventID:   "evt_forward_unanswered",
		Source:    "agent",
		Topic:     "notifications.role.forward-unanswered",
		DedupeKey: "publish.forward-unanswered",
		IssuedAt:  contracts.NowMillis(),
	}
	// forward forwards envelope to subject on client and waits for its outcome.
	forward := func(client *Client, subject string, window time.Duration) error {
		pending, err := client.StartRequestStoredTo(subject, envelope, ForwardMsgID(envelope.DedupeKey, subject), window)
		if err != nil {
			return err
		}
		return pending.Wait()
	}
	connect := func(t *testing.T, uri string) *Client {
		t.Helper()
		client, err := ConnectOwningStream([]string{uri}, WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		return client
	}
	// silent subscribes subject on a connection of its own and answers nothing, so a forward to it
	// meets a responder and no acknowledgement.
	silent := func(t *testing.T, uri, subject string) {
		t.Helper()
		conn := testnats.Connect(t, uri)
		t.Cleanup(conn.Close)
		if _, err := conn.Subscribe(subject, func(*nats.Msg) {}); err != nil {
			t.Fatalf("subscribe %s: %v", subject, err)
		}
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush the subscription: %v", err)
		}
	}
	for _, row := range []struct {
		name           string
		run            func(t *testing.T) error
		notStored      bool  // ErrForwardNotStored
		unacknowledged bool  // ErrUnacknowledged
		matches        error // what else the error must match, when not nil
		streamAnswer   bool  // the error carries the stream's answer, a *nats.APIError
	}{
		{"control: the stream stored the forward", func(t *testing.T) error {
			return forward(connect(t, testnats.URL(t)), contracts.LegacyAgentSubject("ses_forward_stored"), 2*time.Second)
		}, false, false, nil, false},
		{"no stream took the forward (no responders)", func(t *testing.T) error {
			return forward(connect(t, testnats.URL(t)), "test.forward-unanswered.none", 2*time.Second)
		}, true, false, nats.ErrNoResponders, false},
		{"another stream refused the forward with its error", func(t *testing.T) error {
			uri := testnats.URL(t)
			client := connect(t, uri)
			if _, err := rawJetStream(t, uri).AddStream(&nats.StreamConfig{Name: anotherProgramsStream, Subjects: []string{"test.forward-unanswered.other.>"}, Storage: nats.FileStorage}); err != nil {
				t.Fatalf("another program creates %s: %v", anotherProgramsStream, err)
			}
			return forward(client, "test.forward-unanswered.other.x", 2*time.Second)
		}, true, false, nil, true},
		{"nothing answered the forward within its window", func(t *testing.T) error {
			uri := testnats.URL(t)
			client := connect(t, uri)
			silent(t, uri, "test.forward-unanswered.silent")
			return forward(client, "test.forward-unanswered.silent", 300*time.Millisecond)
		}, true, true, nil, false},
		{"the forward's connection closed under its wait", func(t *testing.T) error {
			uri := testnats.URL(t)
			client := connect(t, uri)
			const subject = "test.forward-unanswered.closed"
			silent(t, uri, subject)
			pending, err := client.StartRequestStoredTo(subject, envelope, ForwardMsgID(envelope.DedupeKey, subject), 10*time.Second)
			if err != nil {
				return err
			}
			closing := client.Connection()
			time.AfterFunc(100*time.Millisecond, closing.Close)
			return pending.Wait()
		}, true, true, nats.ErrConnectionClosed, false},
		{"the client could not confirm the publish and nothing answered it", func(t *testing.T) error {
			conn := answeringServer(t, "notifications.other", false)
			return forward(&Client{Conn: conn}, "notifications.agent.holder", 300*time.Millisecond)
		}, true, true, ErrPublishUnconfirmed, false},
		{"the client could not flush the publish nats.go took", func(t *testing.T) error {
			server, uri := testnats.Start(t)
			client := connect(t, uri)
			thaw := server.Freeze(t)
			t.Cleanup(thaw)
			return forward(client, contracts.LegacyAgentSubject("ses_forward_frozen"), 500*time.Millisecond)
		}, false, true, nats.ErrTimeout, false},
		{"control: the server denied the publish", func(t *testing.T) error {
			const subject = "notifications.agent.denied"
			conn := answeringServer(t, subject, false)
			return forward(&Client{Conn: conn}, subject, 300*time.Millisecond)
		}, false, false, ErrPublishDenied, false},
		{"control: the forward was refused before anything was sent", func(t *testing.T) error {
			client := connect(t, testnats.URL(t))
			_, err := client.StartRequestStoredTo(contracts.LegacyAgentSubject("ses_forward_unnamed"), envelope, "", 300*time.Millisecond)
			return err
		}, false, false, ErrNoForwardMsgID, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := row.run(t)
			t.Logf("forward: %v", err)
			if !row.notStored && !row.unacknowledged && row.matches == nil && !row.streamAnswer {
				if err != nil {
					t.Fatalf("control: the forward the stream stored = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the forward returned no error")
			}
			if got := errors.Is(err, ErrForwardNotStored); got != row.notStored {
				t.Errorf("the forward's error %v is ErrForwardNotStored %t, want %t", err, got, row.notStored)
			}
			if got := errors.Is(err, ErrUnacknowledged); got != row.unacknowledged {
				t.Errorf("the forward's error %v is ErrUnacknowledged %t, want %t", err, got, row.unacknowledged)
			}
			if apiErr := (*nats.APIError)(nil); row.streamAnswer && !errors.As(err, &apiErr) {
				t.Errorf("the forward's error %v carries no *nats.APIError, want the stream's answer", err)
			}
			if row.matches != nil && !errors.Is(err, row.matches) {
				t.Errorf("the forward's error %v does not match %v", err, row.matches)
			}
		})
	}
}
