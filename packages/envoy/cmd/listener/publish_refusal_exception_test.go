package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// refusedPublish is an envelope of the shape the publish route builds for a role-lane publish,
// carrying payload as its machine payload.
func refusedPublish(payload string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        "evt-refused",
		Source:         "agent",
		SourceEventID:  "src-refused",
		Topic:          contracts.RoleTopicPrefix + "unheld",
		DedupeKey:      "publish.refused",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "please review",
		Payload:        payload,
		TraceID:        "trace-refused",
	}
}

// exceptionPayloadOf decodes the exception's own payload, the shape the Legion daemon's consumer
// reads (packages/daemon/internal/daemon/notice_exceptions.go, roleLaneException).
func exceptionPayloadOf(t *testing.T, exception contracts.Envelope) deliveryExceptionPayload {
	t.Helper()
	var payload deliveryExceptionPayload
	if err := json.Unmarshal([]byte(exception.Payload), &payload); err != nil {
		t.Fatalf("decode the exception payload: %v", err)
	}
	return payload
}

// The exception carries the refused publish's machine payload, since that is what a consumer
// re-reads the message from, but only while it fits refusalExceptionBound. A larger payload is
// left out instead: carrying it would make an envelope past the server's max payload, which NATS
// refuses whole on every attempt, so a refusal nothing was wrong with would log
// exceptionPublishFailedLine, which the deployed publish-failure alarm matches. Everything a
// consumer of a refusal needs beside the payload -- the topic, the reason, the dedupe key and the
// one-line summary -- is carried whatever the payload's size. The delivery path passes no bound
// and carries the payload whole, as it always has.
func TestARefusalExceptionCarriesThePayloadOnlyWhileItFitsItsBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload string
		bound   int
		carried string
	}{
		{"a payload inside the bound", `{"kind":"pr-blocked"}`, refusalExceptionBound, `{"kind":"pr-blocked"}`},
		{"a payload exactly at the bound", strings.Repeat("p", refusalExceptionBound), refusalExceptionBound, strings.Repeat("p", refusalExceptionBound)},
		{"a payload one byte past the bound", strings.Repeat("p", refusalExceptionBound+1), refusalExceptionBound, ""},
		{"no bound carries it whole", strings.Repeat("p", refusalExceptionBound+1), 0, strings.Repeat("p", refusalExceptionBound+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := refusedPublish(tc.payload)
			exception, err := deliveryExceptionEnvelope(item, string(roles.Unclaimed), "", tc.bound)
			if err != nil {
				t.Fatalf("build the exception: %v", err)
			}
			payload := exceptionPayloadOf(t, exception)
			if payload.Payload != tc.carried {
				t.Fatalf("the exception carries %d bytes of payload, want %d", len(payload.Payload), len(tc.carried))
			}
			if payload.OriginalTopic != item.Topic || payload.EventID != item.EventID ||
				payload.Reason != string(roles.Unclaimed) || payload.DedupeKey != item.DedupeKey ||
				payload.PayloadSummary != item.PayloadSummary || payload.Source != item.Source {
				t.Fatalf("the exception payload = %+v, want the refused publish's own fields whatever the payload's size", payload)
			}
			if exception.Topic != bus.ExceptionTopicPrefix+item.Topic || exception.Source != "envoy" || exception.SourceEventID != item.EventID {
				t.Fatalf("the exception envelope = %+v, want it keyed to the refused topic and sourced envoy", exception)
			}
		})
	}
}

// A refusal's exception is best effort: the 404 is already written when it is published, so a
// publish that fails is logged and nothing else -- no retry, and nothing of the answer changes. A
// closed client is the failure every other cause reaches this branch the same way.
func TestARefusalExceptionThatCannotBePublishedIsOnlyLogged(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t)
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test", logs)
	client.Close()

	publishRefusalException(client, logger, nil, refusedPublish(""), roles.Resolution{State: roles.Unclaimed})

	testwait.Until(t, "the failed refusal exception to be logged", func() (string, bool) {
		lines := logs.String()
		return lines, strings.Contains(lines, exceptionPublishFailedLine)
	})
}

// The refusal call returns before a reconnect can finish. The bubble runs the bus client's
// retry and publish timers without reading the wall clock or opening a real socket.
func TestARefusalExceptionDoesNotWaitForTheBus(t *testing.T) {
	t.Parallel()
	testwait.Bubble(t, func(t testing.TB) {
		logs := &cmdtest.LockedBuffer{}
		conn, err := natsgo.Connect(natsgo.DefaultURL,
			natsgo.InProcessServer(unavailableRefusalBus{}),
			natsgo.RetryOnFailedConnect(true))
		if err != nil {
			t.Fatalf("connect the listener's bus: %v", err)
		}
		defer conn.Close()
		if !conn.IsReconnecting() {
			t.Fatalf("the client must be reconnecting, got %s", conn.Status())
		}

		client := &bus.Client{Conn: conn}
		returned := make(chan struct{})
		go func() {
			publishRefusalException(client, logging.NewWithWriter("test", logs), nil, refusedPublish(""), roles.Resolution{State: roles.Unclaimed})
			close(returned)
		}()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Error("publishRefusalException is still waiting for the bus while reconnect is held: the 404 waits for it")
		}
		testwait.Until(t, "the detached exception to log the failed reconnect", func() (string, bool) {
			lines := logs.String()
			return lines, strings.Contains(lines, exceptionPublishFailedLine) && strings.Contains(lines, "reconnecting")
		})
	})
}

// unavailableRefusalBus starts no sockets. Every dial fails, so the NATS client stays reconnecting.
type unavailableRefusalBus struct{}

func (unavailableRefusalBus) InProcessConn() (net.Conn, error) {
	return nil, natsgo.ErrNoServers
}

func subscribeRefusalFrames(t *testing.T, conn *natsgo.Conn, subject string) (*natsgo.Subscription, <-chan *natsgo.Msg) {
	t.Helper()
	frames := make(chan *natsgo.Msg, 8)
	probe, err := conn.ChanSubscribe(subject, frames)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	// SUB and this publish travel down the same connection, in that order.
	if err := conn.Publish(subject, nil); err != nil {
		t.Fatalf("publish the subscription marker on %s: %v", subject, err)
	}
	testwait.Receive(t, "the subscription marker on "+subject, frames)
	return probe, frames
}

// requireNoRouteException keeps the existing observation span. A flush or a later request can
// overtake a detached publisher that has not run yet. The route exposes no completion state for
// that goroutine, and the live NATS transport cannot run under synctest.
func requireNoRouteException(t *testing.T, conn *natsgo.Conn, probe *natsgo.Subscription, frames <-chan *natsgo.Msg, what string) {
	t.Helper()
	testwait.Quiet(t, what, frames, 250*time.Millisecond,
		"the route starts an untracked publisher a later action could overtake, and the NATS client uses a timer pool shared across synctest bubbles")
	if !conn.IsConnected() || !probe.IsValid() {
		t.Fatalf("the connection or subscription to %s closed while checking for %s", probe.Subject, what)
	}
}

// A non-live state outside roleholder is not a refusal reason the Legion daemon knows, so it raises
// no exception. This keeps a third listener state from becoming a malformed role-lane exception.
func TestARefusalExceptionIsRaisedOnlyForASharedNoHolderReason(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t)
	client.Close()
	testwait.Bubble(t, func(t testing.TB) {
		logs := &cmdtest.LockedBuffer{}
		logger := logging.NewWithWriter("test", logs)
		item := refusedPublish("{}")
		item.Topic = contracts.RoleTopicPrefix + "unknown-refusal"

		// The closed client does no network I/O. Any detached publish attempt fails and logs
		// before the bubble settles. A shared reason exercises the same path as a control.
		publishRefusalException(client, logger, nil, item, roles.Resolution{State: roles.State("not_live")})
		synctest.Wait()
		if lines := logs.String(); lines != "" {
			t.Fatalf("an unlisted refusal state attempted to publish an exception: %s", lines)
		}
		publishRefusalException(client, logger, nil, item, roles.Resolution{State: roles.Unclaimed})
		synctest.Wait()
		if lines := logs.String(); !strings.Contains(lines, exceptionPublishFailedLine) {
			t.Fatalf("the shared refusal reason did not attempt a publish: %q", lines)
		}
	})
}

// The bound applies where the refusal publishes, not only in the helper: a refused publish past
// refusalExceptionBound reaches the exception lane of a live bus without its payload, and with the
// fields a consumer of the refusal needs.
func TestARefusalExceptionLeavesAnOversizedPayloadOutOnTheBus(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t)
	item := refusedPublish(strings.Repeat("p", refusalExceptionBound+1))
	item.Topic = contracts.RoleTopicPrefix + "oversized-refusal"
	_, frames := subscribeRefusalFrames(t, client.Conn, bus.ExceptionTopicPrefix+item.Topic)

	publishRefusalException(client, logging.NewWithWriter("test", io.Discard), nil, item, roles.Resolution{State: roles.Unclaimed})

	message := testwait.Receive(t, "the oversized refusal's exception", frames)
	var exception contracts.Envelope
	if err := json.Unmarshal(message.Data, &exception); err != nil {
		t.Fatalf("decode the refusal exception: %v", err)
	}
	payload := exceptionPayloadOf(t, exception)
	if payload.Payload != "" {
		t.Fatalf("the exception carries %d bytes of a %d-byte payload, want none past the %d-byte bound",
			len(payload.Payload), len(item.Payload), refusalExceptionBound)
	}
	if payload.OriginalTopic != item.Topic || payload.Reason != string(roles.Unclaimed) ||
		payload.DedupeKey != item.DedupeKey || payload.PayloadSummary != item.PayloadSummary {
		t.Fatalf("the exception payload = %+v, want the refused publish's own fields", payload)
	}
}

// The route raises the refusal's exception where it writes the 404, and only there. A sender that
// holds a queue of its own (keep: false) to a role nobody claims is refused `unclaimed` and the
// exception follows it; a publish whose message the route kept instead is answered 202 and raises
// nothing, because nothing was refused.
func TestTheRouteRaisesOneExceptionWhereItRefusesARoleSend(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t)
	registry, sessions := openRouteTestRegistries(t, client, store.WithReplicas(1))
	d := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
	const refused, kept = "refusal-exception-opted-out", "refusal-exception-kept"
	prefix := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix
	refusedProbe, refusedFrames := subscribeRefusalFrames(t, client.Conn, prefix+refused)
	keptProbe, keptFrames := subscribeRefusalFrames(t, client.Conn, prefix+kept)
	handler := publishHandler(d, logging.NewWithWriter("test", io.Discard))
	publish := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, postJSON("/v1/messages/publish", body).WithContext(testwait.Context(t)))
		return recorder
	}

	recorder := publish(`{"topic":"` + contracts.RoleTopicPrefix + refused + `","source":"agent","message":"the sender queues this itself","keep":false}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false to an unheld role: status %d, body %s; want 404", recorder.Code, recorder.Body.String())
	}
	message := testwait.Receive(t, "the route's refusal exception", refusedFrames)
	var exception contracts.Envelope
	if err := json.Unmarshal(message.Data, &exception); err != nil {
		t.Fatalf("decode the exception envelope: %v", err)
	}
	payload := exceptionPayloadOf(t, exception)
	if payload.OriginalTopic != contracts.RoleTopicPrefix+refused || payload.Reason != string(roles.Unclaimed) ||
		payload.RecipientSession != "" {
		t.Fatalf("the exception payload = %+v, want the refused topic, reason unclaimed and no recipient", payload)
	}
	requireNoRouteException(t, client.Conn, refusedProbe, refusedFrames, "a second exception from one refusal")

	recorder = publish(`{"topic":"` + contracts.RoleTopicPrefix + kept + `","source":"agent","source_session":"ses_refusal_exception_sender","message":"this one waits for the next holder"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202, the message kept", recorder.Code, recorder.Body.String())
	}
	requireNoRouteException(t, client.Conn, keptProbe, keptFrames, "an exception from a kept message")
}
