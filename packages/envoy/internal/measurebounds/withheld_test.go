package measurebounds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Each of the four requests the request helper does not bind ends when its reply never comes: a
// hold withholds the reply (testnats.ReplyHold), and the row times how long the request waits
// before it answers. Each must answer, and within its bound and a margin; none may wait on. The
// first three end at a bound of their own; the replay ends with the context its caller gives it,
// and the relay's own bound for it, agentStreamReplayTimeout, is internal/dispatch/api's row
// (TestAReplayWhoseAnswerNeverComesEndsAtTheReplayTimeout).
func TestEachUnboundRequestEndsAtItsOwnBoundWhenItsReplyIsWithheld(t *testing.T) {
	const margin = 3 * time.Second
	// ended times call, which makes the request whose reply held withholds, and requires three
	// things of its answer: the hold withheld that reply before the answer came, so the request
	// waited on it and not on something else; the answer is not one the row names a wrong answer
	// (errWrongAnswer: the request got through, which a withheld reply forbids); and it came at
	// the bound, within margin. The margin is the row's tolerance for a loaded box, so a bound
	// lengthened by less than it passes: the row shows each request ends near its bound, not the
	// bound to the millisecond.
	ended := func(t *testing.T, what string, bound time.Duration, held *testnats.HeldReplies, call func() error) {
		t.Helper()
		done := make(chan error, 1)
		started := time.Now()
		go func() { done <- call() }()
		select {
		case err := <-done:
			took := time.Since(started)
			t.Logf("BOUND %s: answered after %s with %v (bound %s)", what, took.Round(time.Millisecond), err, bound)
			select {
			case <-held.Withheld():
			default:
				t.Errorf("%s answered %v after %s with no reply withheld, want it to wait on the withheld reply", what, err, took)
			}
			if errors.Is(err, errWrongAnswer) {
				t.Errorf("%s answered %v, want it to end at its bound with no answer", what, err)
			}
			if took < bound-time.Second || took > bound+margin {
				t.Errorf("%s answered after %s, want it at its bound %s", what, took, bound)
			}
		case <-time.After(bound + margin):
			t.Fatalf("%s was still waiting %s after it began, past its bound %s", what, bound+margin, bound)
		}
	}

	t.Run("a KV watch's create", func(t *testing.T) {
		hold := testnats.StartReplyHold(t, testnats.URL(t))
		client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		kv, err := bus.EnsureKeyValue(t.Context(), client.Connection(), client.JS(), &nats.KeyValueConfig{Bucket: "withheld_watch", Storage: nats.MemoryStorage})
		if err != nil {
			t.Fatalf("ensure the bucket: %v", err)
		}
		held := hold.Hold("$JS.API.CONSUMER.CREATE")
		ended(t, "a KV watch's create", 5*time.Second, held, func() error {
			w, err := kv.WatchKeys(context.Background(), ">")
			if err == nil {
				_ = w.Stop()
				return fmt.Errorf("%w: the create answered no error", errWrongAnswer)
			}
			return err
		})
	})

	t.Run("the durable consumer's bind", func(t *testing.T) {
		hold := testnats.StartReplyHold(t, testnats.URL(t))
		client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		const name = "withheld-bind"
		if _, err := client.JS().AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: name, DeliverSubject: nats.NewInbox(), AckPolicy: nats.AckExplicitPolicy}); err != nil {
			t.Fatalf("add the durable: %v", err)
		}
		held := hold.Hold("$JS.API.CONSUMER.INFO")
		ended(t, "the durable consumer's bind", 10*time.Second, held, func() error {
			sub, err := client.Subscribe("", func(*nats.Msg) {}, nats.Bind(bus.Stream, name), nats.ManualAck())
			if err == nil {
				_ = sub.Unsubscribe()
				return fmt.Errorf("%w: the bind answered no error", errWrongAnswer)
			}
			return err
		})
	})

	t.Run("the consumer-name lister", func(t *testing.T) {
		hold := testnats.StartReplyHold(t, testnats.URL(t))
		client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		held := hold.Hold("$JS.API.CONSUMER.NAMES")
		ended(t, "the consumer-name lister", 10*time.Second, held, func() error {
			for range client.JS().ConsumerNames(bus.Stream) {
				return fmt.Errorf("%w: the lister named a consumer through a withheld reply", errWrongAnswer)
			}
			return nil
		})
	})

	t.Run("Dispatch's agent-stream replay", func(t *testing.T) {
		const sessionID = "ses_withheld_replay"
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		responder, err := conn.Subscribe(contracts.AgentStreamControlSubject(sessionID), func(m *nats.Msg) { _ = m.Respond([]byte("frame")) })
		if err != nil {
			t.Fatalf("subscribe the session: %v", err)
		}
		t.Cleanup(func() { _ = responder.Unsubscribe() })
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		hold := testnats.StartReplyHold(t, testnats.URL(t))
		relay, err := nats.Connect(hold.URL())
		if err != nil {
			t.Fatalf("connect the relay through the hold: %v", err)
		}
		t.Cleanup(relay.Close)
		held := hold.Hold(contracts.AgentStreamControlSubject(sessionID))
		// Replay ends with the context its caller gives it, here 2 s; the relay's own bound,
		// agentStreamReplayTimeout, is the internal/dispatch/api row's.
		ended(t, "Dispatch's agent-stream replay", 2*time.Second, held, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			frame, err := agentstream.NewNATS(relay).Replay(ctx, sessionID)
			if err == nil && frame != nil {
				return fmt.Errorf("%w: the replay answered %q through a withheld reply", errWrongAnswer, frame)
			}
			return err
		})
	})

	// The control: the unbound form of the replay row, a hand-rolled request on the same withheld
	// subject that waits for its reply with a context that has no deadline. It runs only with
	// MEASUREBOUNDS_CONTROL set, and it must fail: it is still waiting when the row's wait ends.
	t.Run("the unbound form, the control", func(t *testing.T) {
		if os.Getenv("MEASUREBOUNDS_CONTROL") == "" {
			t.Skip("the control runs only with MEASUREBOUNDS_CONTROL set")
		}
		const sessionID = "ses_withheld_unbound"
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		responder, err := conn.Subscribe(contracts.AgentStreamControlSubject(sessionID), func(m *nats.Msg) { _ = m.Respond([]byte("frame")) })
		if err != nil {
			t.Fatalf("subscribe the session: %v", err)
		}
		t.Cleanup(func() { _ = responder.Unsubscribe() })
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		hold := testnats.StartReplyHold(t, testnats.URL(t))
		relay, err := nats.Connect(hold.URL())
		if err != nil {
			t.Fatalf("connect the relay through the hold: %v", err)
		}
		t.Cleanup(relay.Close)
		held := hold.Hold(contracts.AgentStreamControlSubject(sessionID))
		ended(t, "a request with no bound", 2*time.Second, held, func() error {
			inbox := nats.NewInbox()
			replies, err := relay.SubscribeSync(inbox)
			if err != nil {
				return err
			}
			if err := relay.PublishRequest(contracts.AgentStreamControlSubject(sessionID), inbox, []byte("replay")); err != nil {
				return err
			}
			_, err = replies.NextMsgWithContext(context.Background())
			return err
		})
	})
}

// errWrongAnswer marks a row's answer that the withheld reply forbids: the request got through.
var errWrongAnswer = errors.New("a withheld request was answered")
