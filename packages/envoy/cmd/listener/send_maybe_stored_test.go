package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// A send whose publish went out and was never acknowledged may be stored: the send route answers
// 503 with the code that says so (listenerwire.CodeMaybeStored), and Dispatch's client reads it as
// dispatchenvoy.ErrStoreUnconfirmed, a send that may have landed, and not as one that stored
// nothing. The relay sees the publish leave the listener's NATS connection but withholds it,
// so no acknowledgement can arrive. The control disconnects that connection before the send:
// the client waits to reconnect and never publishes. The send budget ends both waits.
func TestASendWhosePublishWasNeverAcknowledgedIsReadAsMaybeStored(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T, url string) (*listenerDeps, *listenerRelay, *clocktest.Fake) {
		t.Helper()
		relay := startListenerRelay(t, url)
		fake := clocktest.New(time.Unix(0, 0))
		client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1), bus.WithPublishAcknowledgementClock(fake))
		if err != nil {
			t.Fatalf("connect the listener through the relay: %v", err)
		}
		t.Cleanup(client.Close)
		return budgetRouteDeps(t, client, fake), relay, fake
	}
	t.Run("the publish went out and was never acknowledged: 503 maybe_stored, ErrStoreUnconfirmed", func(t *testing.T) {
		t.Parallel()
		uri := testnats.URL(t)
		state, relay, fake := open(t, uri)
		const target = "ses_maybe_stored_target"
		if err := state.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Delivery: session.DeliveryConsumer}); err != nil {
			t.Fatalf("register %s: %v", target, err)
		}
		plugin := testnats.Connect(t, uri)
		t.Cleanup(plugin.Close)
		probed := make(chan error, 1)
		if _, err := plugin.Subscribe(bus.DeliverSubject(target), func(msg *natsgo.Msg) {
			if msg.Header.Get(bus.ConnectionProbeHeader) != "" {
				probed <- msg.Respond([]byte("done"))
			}
		}); err != nil {
			t.Fatalf("subscribe %s's relay subject: %v", target, err)
		}
		ctx, cancel := clock.WithShownDeadline(testwait.Context(t))
		defer cancel()
		if err := plugin.FlushWithContext(ctx); err != nil {
			t.Fatalf("flush the relay subscription: %v", err)
		}
		relay.arm(contracts.LegacyAgentSubject(target)+" ", 1)
		deadline := fake.Now().Add(sendRequestBudget)
		status, body, err := sendThroughDispatch(t, sendHandler(state, logging.New("test")), target, func() {
			fake.AwaitArmedAt(t, "the send request budget", deadline)
			if err := testwait.Receive(t, "the target's plug-in to answer the connection probe", probed); err != nil {
				t.Fatalf("answer %s's connection probe: %v", target, err)
			}
			testwait.Receive(t, "the relay to see and withhold the send's stored publish", relay.swallowed)
			fake.Set(deadline)
		})
		var refusal listenerwire.APIError
		if status != http.StatusServiceUnavailable || json.Unmarshal([]byte(body), &refusal) != nil || refusal.Code != listenerwire.CodeMaybeStored {
			t.Fatalf("the send whose publish was never acknowledged was answered %d %s; want 503 with code %q", status, body, listenerwire.CodeMaybeStored)
		}
		if !errors.Is(err, dispatchenvoy.ErrStoreUnconfirmed) || errors.Is(err, dispatchenvoy.ErrReceiptTimeout) || !errors.Is(err, dispatchenvoy.ErrUnavailable) {
			t.Fatalf("Dispatch's client read it as %v; want ErrStoreUnconfirmed (and ErrUnavailable), not a receipt timeout", err)
		}
	})

	t.Run("control: a 503 that sent nothing carries no code and is no ErrStoreUnconfirmed", func(t *testing.T) {
		t.Parallel()
		state, relay, fake := open(t, testnats.URL(t))
		const target = "ses_maybe_stored_control"
		if err := state.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
			t.Fatalf("register %s: %v", target, err)
		}
		relay.cut()
		testwait.Until(t, "the listener's connection to wait for a reconnect after the link is cut", func() (natsgo.Status, bool) {
			status := state.client.Connection().Status()
			return status, status == natsgo.RECONNECTING
		})
		started := fake.Now()
		status, body, err := sendThroughDispatch(t, sendHandler(state, logging.New("test")), target, func() {
			deadline := started.Add(sendRequestBudget)
			fake.AwaitArmedAt(t, "the send request budget", deadline)
			armed := fake.AwaitArmed(t, "the route budget and publish acknowledgement bound during reconnect", 2)
			if len(armed) != 2 || !armed[0].Equal(deadline) || !armed[1].After(deadline) {
				t.Fatalf("armed deadlines = %v, want the route deadline %v followed by the publish acknowledgement bound", armed, deadline)
			}
			fake.Set(deadline)
		})
		var refusal listenerwire.APIError
		if status != http.StatusServiceUnavailable || json.Unmarshal([]byte(body), &refusal) != nil || refusal.Code != "" {
			t.Fatalf("the send that could not reconnect was answered %d %s; want a 503 with no code", status, body)
		}
		if errors.Is(err, dispatchenvoy.ErrStoreUnconfirmed) || !errors.Is(err, dispatchenvoy.ErrUnavailable) {
			t.Fatalf("Dispatch's client read it as %v; want ErrUnavailable and not ErrStoreUnconfirmed", err)
		}
	})
}

// sendThroughDispatch sends one message to target with Dispatch's own client through mux, the
// listener's routes, and returns the route's status and body as the client met them, and the
// client's error. drive advances the listener while the client's own clock stays still.
func sendThroughDispatch(t *testing.T, mux http.Handler, target string, drive func()) (int, string, error) {
	t.Helper()
	type answer struct {
		status int
		body   string
	}
	answered := make(chan answer, 1)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, r)
		answered <- answer{recorder.Code, recorder.Body.String()}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(func() {
		testwait.Returns(t, "the HTTP listener to close after the send request ends", listener.Close)
	})
	clientDone := make(chan error, 1)
	go func() {
		_, err := dispatchenvoy.New(listener.URL, dispatchenvoy.WithClock(clocktest.Held())).Send(t.Context(), dispatchenvoy.SendInput{
			TargetSession: target, Message: "may be stored", Payload: json.RawMessage(`{}`), IdempotencyKey: "maybe-stored-" + target,
		})
		clientDone <- err
	}()
	drive()
	err := testwait.Receive(t, "Dispatch's send to "+target+" to return the listener's answer", clientDone)
	got := testwait.Receive(t, "the HTTP listener to record the send's answer", answered)
	return got.status, got.body, err
}

// A publish whose first send went unanswered and whose resend the full stream refused is both
// bus.ErrUnacknowledged and bus.ErrStreamFull (bus.sendStored), and a kept role-route send whose
// keep's publish went unacknowledged reaches the 503 wrapped (keepRoleSend): either may have stored
// the message, so its 503 carries listenerwire.CodeMaybeStored. So does a send or a topic publish
// the stream refused as in process under an earlier publish until its request ended, which
// publishHeld publishes again past each refusal (publishPastInProcess), since the stream may store
// that earlier publish yet, and one whose resend the stream answered a duplicate
// (bus.StoredAck.Resent) whose copy could not be read back: that copy may be this request's own,
// stored by the first send. The controls are a full stream that sent nothing, a keep that stored
// nothing, and a first send's duplicate whose copy could not be read, which carry no code.
func TestEveryBusUnavailableAnswerThatMayHaveStoredTheMessageSaysSo(t *testing.T) {
	t.Parallel()
	unacknowledged := fmt.Errorf("%w: %w", bus.ErrUnacknowledged, errors.New("nats: connection closed"))
	inProcess := fmt.Errorf("%w: %w", bus.ErrDuplicateInProcess,
		&natsgo.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"})
	// inProcessUntilEnded is the error publishHeld answers a publish the stream refuses as in
	// process on every send until the request's context ends.
	inProcessUntilEnded := func() error {
		item := contracts.MessageEnvelope(contracts.Message{Text: "in process"}, "notifications.team.in-process", bus.PublishDedupePrefix+uuid.NewString())
		var err error
		testwait.Bubble(t, func(t testing.TB) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sends := 0
			_, err = publishHeld(ctx, nil, func(context.Context, contracts.Envelope) (bus.StoredAck, error) {
				sends++
				if sends == 2 {
					cancel()
				}
				return bus.StoredAck{}, inProcess
			}, &requestRecord{}, requestSender{}, &item)
		})
		return err
	}
	// unread is the error publishHeld answers a duplicate verdict at sequence 7, a resend's where
	// resent is set, whose read finds the subject's next message at 8 and so no copy at 7.
	unread := func(resent bool) error {
		item := contracts.MessageEnvelope(contracts.Message{Text: "resent"}, "notifications.team.resent-unread", bus.PublishDedupePrefix+uuid.NewString())
		reader := &standInReader{stored: &bus.StoredMessage{Sequence: 8, Subject: item.Topic}}
		_, err := publishHeld(context.Background(), reader, func(context.Context, contracts.Envelope) (bus.StoredAck, error) {
			return bus.StoredAck{Sequence: 7, Duplicate: true, Resent: resent}, nil
		}, &requestRecord{}, requestSender{}, &item)
		return err
	}
	for _, row := range []struct {
		name  string
		err   error
		maybe bool
	}{
		{"an unacknowledged send whose resend met a full stream", fmt.Errorf("%w: %w", bus.ErrUnacknowledged, fmt.Errorf("%w: maximum bytes exceeded", bus.ErrStreamFull)), true},
		{"a kept role-route send whose keep went unacknowledged", fmt.Errorf("the kept lane of role r did not store the message: %w", fmt.Errorf("keep the role r message on its kept lane under k: %w", unacknowledged)), true},
		{"a publish refused in process until its request ended", inProcessUntilEnded(), true},
		{"a resend the stream answered a duplicate whose copy could not be read", unread(true), true},
		{"control: a full stream that sent nothing", fmt.Errorf("%w: maximum bytes exceeded", bus.ErrStreamFull), false},
		{"control: a keep that stored nothing", fmt.Errorf("the kept lane of role r did not store the message: %w", errors.New("nats: no responders")), false},
		{"control: a first send's duplicate whose copy could not be read", unread(false), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			status := writeBusUnavailable(recorder, row.err)
			var body listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode the answer %q: %v", recorder.Body.String(), err)
			}
			want := ""
			if row.maybe {
				want = listenerwire.CodeMaybeStored
			}
			if status != http.StatusServiceUnavailable || body.Code != want || recorder.Header().Get("Retry-After") != "1" {
				t.Fatalf("answered %d, code %q, Retry-After %q (%s); want 503, code %q, Retry-After 1",
					status, body.Code, recorder.Header().Get("Retry-After"), body.Error, want)
			}
		})
	}
}
