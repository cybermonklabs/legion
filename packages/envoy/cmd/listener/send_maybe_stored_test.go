package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A send whose publish went out and was never acknowledged may be stored: the send route answers
// 503 with the code that says so (listenerwire.CodeMaybeStored), and Dispatch's client reads it as
// dispatchenvoy.ErrStoreUnconfirmed, a send that may have landed, and not as one that stored
// nothing. The server is frozen as the send's connection probe reaches the target's stand-in, so the
// publish goes out into the frozen connection, as sender_limit_test.go's row of the same name does.
// The control is a 503 that sent nothing: the listener's server stopped and its connection waiting
// to reconnect, so the publish waits out the route's deadline before any send (bus.ErrUnacknowledged
// does not wrap it), as sender_limit_test.go's reconnect row shows; it carries no code and is no
// ErrStoreUnconfirmed.
func TestASendWhosePublishWasNeverAcknowledgedIsReadAsMaybeStored(t *testing.T) {
	t.Run("the publish went out and was never acknowledged: 503 maybe_stored, ErrStoreUnconfirmed", func(t *testing.T) {
		rig, server := newSenderLimitRigOnServer(t)
		const target = "ses_maybe_stored_target"
		rig.register(t, target, nil)
		thaws := make(chan func(), 1)
		t.Cleanup(func() {
			select {
			case thaw := <-thaws:
				thaw()
			default:
			}
		})
		var freeze sync.Once
		plugin := testnats.Connect(t, rig.url)
		t.Cleanup(plugin.Close)
		if _, err := plugin.Subscribe(bus.DeliverSubject(target), func(msg *natsgo.Msg) {
			if msg.Header.Get(bus.ConnectionProbeHeader) != "" {
				freeze.Do(func() { thaws <- server.Freeze(t) })
			}
		}); err != nil {
			t.Fatalf("subscribe %s's relay subject: %v", target, err)
		}
		if err := plugin.Flush(); err != nil {
			t.Fatalf("flush the relay subscription: %v", err)
		}
		status, body, err := sendThroughDispatch(t, rig.mux, target)
		select {
		case thaw := <-thaws:
			thaw()
		default:
			t.Fatalf("the send probed nothing, so its publish met no frozen server; answer %d %s", status, body)
		}
		var refusal listenerwire.APIError
		if status != http.StatusServiceUnavailable || json.Unmarshal([]byte(body), &refusal) != nil || refusal.Code != listenerwire.CodeMaybeStored {
			t.Fatalf("the send whose publish was never acknowledged was answered %d %s; want 503 with code %q", status, body, listenerwire.CodeMaybeStored)
		}
		if !errors.Is(err, dispatchenvoy.ErrStoreUnconfirmed) || errors.Is(err, dispatchenvoy.ErrReceiptTimeout) || !errors.Is(err, dispatchenvoy.ErrUnavailable) {
			t.Fatalf("Dispatch's client read it as %v; want ErrStoreUnconfirmed (and ErrUnavailable), not a receipt timeout", err)
		}
	})

	t.Run("control: a 503 that sent nothing carries no code and is no ErrStoreUnconfirmed", func(t *testing.T) {
		rig, server := newSenderLimitRigOnServer(t)
		const target = "ses_maybe_stored_control"
		rig.register(t, target, nil)
		client := rig.harness.client
		server.Stop(t)
		t.Cleanup(func() { server.Start(t) })
		waitFor(t, 10*time.Second, "the listener's connection to reconnect after its server stopped", func() bool {
			return !client.Connected() && client.Connection().IsReconnecting()
		})
		status, body, err := sendThroughDispatch(t, rig.mux, target)
		var refusal listenerwire.APIError
		if status != http.StatusServiceUnavailable || json.Unmarshal([]byte(body), &refusal) != nil || refusal.Code != "" {
			t.Fatalf("the send that met a frozen server before its read was answered %d %s; want a 503 with no code", status, body)
		}
		if errors.Is(err, dispatchenvoy.ErrStoreUnconfirmed) || !errors.Is(err, dispatchenvoy.ErrUnavailable) {
			t.Fatalf("Dispatch's client read it as %v; want ErrUnavailable and not ErrStoreUnconfirmed", err)
		}
	})
}

// sendThroughDispatch sends one message to target with Dispatch's own client through mux, the
// listener's routes, and returns the route's status and body as the client met them, and the
// client's error.
func sendThroughDispatch(t *testing.T, mux http.Handler, target string) (int, string, error) {
	t.Helper()
	var (
		mu     sync.Mutex
		status int
		body   string
	)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, r)
		mu.Lock()
		status, body = recorder.Code, recorder.Body.String()
		mu.Unlock()
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(listener.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := dispatchenvoy.New(listener.URL, dispatchenvoy.WithTimeout(20*time.Second)).Send(ctx, dispatchenvoy.SendInput{
		TargetSession: target, Message: "may be stored", Payload: json.RawMessage(`{}`), IdempotencyKey: "maybe-stored-" + target,
	})
	mu.Lock()
	defer mu.Unlock()
	return status, body, err
}

// A publish whose first send went unanswered and whose resend the full stream refused is both
// bus.ErrUnacknowledged and bus.ErrStreamFull (bus.sendStored), and a kept role-route send whose
// keep's publish went unacknowledged reaches the 503 wrapped (keepRoleSend): either may have stored
// the message, so its 503 carries listenerwire.CodeMaybeStored. The controls are a full stream that
// sent nothing and a keep that stored nothing, which carry no code.
func TestEveryBusUnavailableAnswerThatMayHaveStoredTheMessageSaysSo(t *testing.T) {
	unacknowledged := fmt.Errorf("%w: %w", bus.ErrUnacknowledged, errors.New("nats: connection closed"))
	for _, row := range []struct {
		name  string
		err   error
		maybe bool
	}{
		{"an unacknowledged send whose resend met a full stream", fmt.Errorf("%w: %w", bus.ErrUnacknowledged, fmt.Errorf("%w: maximum bytes exceeded", bus.ErrStreamFull)), true},
		{"a kept role-route send whose keep went unacknowledged", fmt.Errorf("the kept lane of role r did not store the message: %w", fmt.Errorf("keep the role r message on its kept lane under k: %w", unacknowledged)), true},
		{"control: a full stream that sent nothing", fmt.Errorf("%w: maximum bytes exceeded", bus.ErrStreamFull), false},
		{"control: a keep that stored nothing", fmt.Errorf("the kept lane of role r did not store the message: %w", errors.New("nats: no responders")), false},
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
