package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// sendBody is one send request's JSON, written the way a client writes it, so each test below
// states only the fields it is about.
func sendBody(fields map[string]any) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// postJSON is an inbound request to one of the two message routes, as the listener's own mux
// hands it over: the handlers are called directly, so the context is the request's own and the
// send budget (sendRequestBudget) is measured from the call.
func postJSON(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func routeAnswer(t *testing.T, handler http.HandlerFunc, req *http.Request) <-chan *httptest.ResponseRecorder {
	t.Helper()
	req = req.WithContext(testwait.Context(t))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		answered <- recorder
	}()
	return answered
}

// finishUnansweredPublish expires the sender stamp and waits for the bus client
// to arm its acknowledgement bound. The route's earlier deadline must end that call.
func finishUnansweredPublish(t *testing.T, fake *clocktest.Fake, started time.Time) {
	t.Helper()
	budgetEnd := started.Add(sendRequestBudget)
	stampEnd := started.Add(senderStampReadTimeout)
	fake.AwaitArmedAt(t, "the sender stamp read window", stampEnd)
	if !stampEnd.Before(budgetEnd) {
		t.Fatalf("the stamp ends at %s, want it to leave time to publish before %s", stampEnd, budgetEnd)
	}
	fake.Set(stampEnd)
	armed := fake.AwaitArmed(t, "the route budget and publish acknowledgement bound", 2)
	if len(armed) != 2 || !armed[0].Equal(budgetEnd) || !armed[1].After(budgetEnd) {
		t.Fatalf("armed deadlines = %v, want route budget %s followed by the publish acknowledgement bound", armed, budgetEnd)
	}
	fake.Set(budgetEnd)
}

func budgetRouteDeps(t *testing.T, client *bus.Client, fake *clocktest.Fake, live ...string) *listenerDeps {
	t.Helper()
	rig := openStampRig(t, client)
	for _, id := range live {
		rig.register(t, id, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + id})
	}
	return &listenerDeps{client: client, registry: rig.registry, sessions: rig.sessions, clock: fake}
}

// serveRecorded serves one request through a handler built over a logger of its own and returns
// the answer with the single record line that handler wrote. Each call gets a fresh buffer, so
// "exactly one line" is asserted per request rather than over a whole test: one line per request
// is the whole point of the record, and a buffer shared across requests could only ever show that
// at least one line was written.
func serveRecorded(t *testing.T, build func(*logging.Logger) http.HandlerFunc, request *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	logs := &cmdtest.LockedBuffer{}
	recorder := httptest.NewRecorder()
	build(logging.NewWithWriter("test", logs)).ServeHTTP(recorder, request)
	return recorder, soleRecord(t, logs.String())
}

// soleRecord is the one log line a request wrote, parsed, failing t unless exactly one was
// written: serveRecorded's reading, and that of a test that serves its request on a goroutine.
func soleRecord(t *testing.T, logs string) map[string]any {
	t.Helper()
	written := strings.TrimSpace(logs)
	lines := strings.Split(written, "\n")
	if written == "" || len(lines) != 1 {
		t.Fatalf("one request wrote %d record lines, want exactly one:\n%s", len(lines), logs)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("the record line is not JSON: %v\n%s", err, lines[0])
	}
	return record
}

// assertRecordFields compares the named fields of one record line, field by field, as the
// starting gate's own record test does. A JSON number unmarshals into float64, so a status is
// named as float64(200) here.
func assertRecordFields(t *testing.T, record map[string]any, want map[string]any) {
	t.Helper()
	for field, expected := range want {
		if record[field] != expected {
			line, _ := json.Marshal(record)
			t.Fatalf("the record's %s = %#v, want %#v: %s", field, record[field], expected, line)
		}
	}
}

// assertRecordErrorCarries fails unless the error field of one record line carries want: a line's
// error wraps the cause a test planted in what the reader that met it adds.
func assertRecordErrorCarries(t *testing.T, record map[string]any, want string) {
	t.Helper()
	if message, _ := record["error"].(string); !strings.Contains(message, want) {
		line, _ := json.Marshal(record)
		t.Fatalf("the record's error %q does not carry %q: %s", message, want, line)
	}
}

// A send reads the target from the session bucket itself when this listener's cache has not seen
// it: the cache follows the bucket through a watcher and so trails it, and a sender told its
// destination is gone routes around the pane. This drives the send route with a session another
// listener registered a moment ago, through a listener whose cache was warm before that write and
// has followed nothing since. This is the same miss a cache still refilling after a reconnect has.
//
// The negative control is the Get below: if the cache held the target the 200 would prove nothing
// about reading through to the bucket, since every warm-cache send already answers 200. The
// delivery read back from notifications.agent.<id> is the second half: a 200 alone would pass
// even if the envelope never left the listener.
func TestSendReadsTheSessionStoreWhileTheCacheIsCold(t *testing.T) {
	t.Parallel()
	const target = "ses_cold_cache_target"
	client := setupPublishTestClient(t)
	fake := clocktest.New(time.Unix(0, 0))
	registrar, cold := budgetRouteDeps(t, client, fake), budgetRouteDeps(t, client, fake)
	cold.sessions.EndWatch(testwait.Context(t))
	if err := registrar.sessions.Put(target, session.SessionEntry{
		SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + target, Title: "cold target",
	}); err != nil {
		t.Fatalf("register %s through the other listener: %v", target, err)
	}
	received, err := cold.client.Conn.SubscribeSync(contracts.LegacyAgentSubject(target))
	if err != nil {
		t.Fatalf("subscribe to %s: %v", contracts.LegacyAgentSubject(target), err)
	}
	t.Cleanup(func() { _ = received.Unsubscribe() })
	ctx, cancel := clock.WithShownDeadline(testwait.Context(t))
	defer cancel()
	if err := cold.client.Conn.FlushWithContext(ctx); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	if entry, err := cold.sessions.Get(target); err == nil {
		t.Fatalf("the sending listener's cache already holds %s (%+v): the send below would prove nothing", target, entry)
	} else if !strings.Contains(err.Error(), nats.ErrKeyNotFound.Error()) {
		t.Fatalf("the sending listener's cache answers %v for %s, want %v", err, target, nats.ErrKeyNotFound)
	}

	recorder := testwait.Receive(t, "the send through the cold session cache to answer",
		routeAnswer(t, sendHandler(cold, logging.New("test")), postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_cold_cache_sender",
			"message":        "the bucket holds you even when my cache does not",
		}))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("send to a session only the bucket holds: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	var answered contracts.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}

	message, err := received.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("nothing reached %s: %v", contracts.LegacyAgentSubject(target), err)
	}
	var delivered contracts.Envelope
	if err := json.Unmarshal(message.Data, &delivered); err != nil {
		t.Fatalf("decode the delivered envelope: %v", err)
	}
	if delivered.EventID != answered.EventID || delivered.Topic != contracts.LegacyAgentSubject(target) {
		t.Fatalf("delivered %+v, want the answered envelope %+v on the target's subject", delivered, answered)
	}
}

// A send whose bus connection has gone away under the listener is answered 503 naming the bus,
// inside the five seconds the HTTP client gives one attempt (envoy-client's transport), rather
// than left to that caller's own timeout, which it would retry blind. This drives the real
// handler over a real connection cut at the TCP level, which is what the hub's virtual machine
// being recreated did on 2026-10-23.
//
// The target is registered first and its entry read back from the warm cache, so the session read
// cannot be what fails: the 503 can only come from the publish. The negative control is the
// identical request before the cut, which answers 200 — without it a rig that refused every send
// (a bad body, an unregistered target) would pass this test with a 503 of a different kind.
func TestSendAnswersServiceUnavailableWhileTheBusIsUnreachable(t *testing.T) {
	t.Parallel()
	const target = "ses_cut_bus_target"
	// The relay the reconnect test uses (reconnect_shutdown_test.go): cut below closes every live
	// link and refuses every connection after it, as the hub's virtual machine being recreated
	// (2026-10-23) did to the listener's bus connection.
	relay := startListenerRelay(t, testnats.URL(t))
	fake := clocktest.New(time.Unix(0, 0))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1), bus.WithPublishAcknowledgementClock(fake))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	state := budgetRouteDeps(t, client, fake, target)
	handler := sendHandler(state, logging.New("test"))
	request := func() *http.Request {
		return postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_cut_bus_sender",
			"message":        "is the bus there?",
		}))
	}

	if reachable := testwait.Receive(t, "the control send to answer", routeAnswer(t, handler, request())); reachable.Code != http.StatusOK {
		t.Fatalf("the same send with the bus reachable: status %d, body %s; want 200", reachable.Code, reachable.Body.String())
	}

	relay.cut()
	testwait.Until(t, "the NATS client to notice the severed link", func() (string, bool) {
		status := client.Conn.Status()
		return status.String(), status == nats.RECONNECTING
	})
	started := fake.Now()
	answered := routeAnswer(t, handler, request())
	fake.AwaitArmedAt(t, "the send request budget", started.Add(sendRequestBudget))
	finishUnansweredPublish(t, fake, started)
	recorder := testwait.Receive(t, "the send to refuse the cut bus at the request budget", answered)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("send with the bus unreachable: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
	}
	var refusal listenerwire.APIError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: the sender is being asked to try the same route again", got)
	}
}

// A bus that goes silent without closing the connection - a partition, or a hub host that stops
// answering without resetting its sockets - is unreachable too, and a send and a publish answer it
// with the same named 503 inside the caller's attempt. nats.go cannot tell such a link is dead
// until its pings go unanswered, minutes later, so every bus call made on it waits: the target's
// read and the publish are bounded by sendRequestBudget, and so must every other bus call the
// route makes before it answers, the sender stamp's read of the source session's interest record
// (senderStamp) among them. That read is the cache's miss read through to the bucket, as it is for
// any sender whose interest record the cache does not hold: one that subscribed to nothing, or any
// sender while the cache refills after a reconnect, which is the case this issue began with.
//
// The relay freezes the link by discarding everything the client writes from the moment it is
// armed, keeping the connection open, so the client still reports itself connected. The target is
// in the warm session cache, so its read is not what is under test. The negative control is the
// identical request before the freeze, answered 200 on the same rig.
func TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget(t *testing.T) {
	t.Parallel()
	const (
		target = "ses_frozen_bus_target"
		sender = "ses_frozen_bus_sender_with_no_interest_record"
	)
	for _, route := range []struct {
		name    string
		handler func(*listenerDeps) http.HandlerFunc
		path    string
		body    map[string]any
	}{
		{
			name:    "send",
			handler: func(d *listenerDeps) http.HandlerFunc { return sendHandler(d, logging.New("test")) },
			path:    "/v1/messages/send",
			body:    map[string]any{"target_session": target, "source": "agent", "source_session": sender, "message": "is the bus there?"},
		},
		{
			name:    "publish",
			handler: func(d *listenerDeps) http.HandlerFunc { return publishHandler(d, logging.New("test")) },
			path:    "/v1/messages/publish",
			body:    map[string]any{"topic": "notifications.team.example-team", "source": "agent", "source_session": sender, "message": "is the bus there?"},
		},
	} {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			relay := startListenerRelay(t, testnats.URL(t))
			fake := clocktest.New(time.Unix(0, 0))
			client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1), bus.WithPublishAcknowledgementClock(fake))
			if err != nil {
				t.Fatalf("connect the listener's bus through the relay: %v", err)
			}
			t.Cleanup(client.Close)
			state := budgetRouteDeps(t, client, fake)
			entry := session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + target, Title: "frozen target"}
			if err := state.sessions.Put(target, entry); err != nil {
				t.Fatalf("register %s: %v", target, err)
			}
			if _, err := state.sessions.Get(target); err != nil {
				t.Fatalf("the listener's cache does not hold %s (%v): the target's read could be what fails", target, err)
			}
			handler := route.handler(state)
			request := func() *http.Request { return postJSON(route.path, sendBody(route.body)) }
			if reachable := testwait.Receive(t, "the control "+route.name+" to answer", routeAnswer(t, handler, request())); reachable.Code != http.StatusOK {
				t.Fatalf("the same %s with the bus answering: status %d, body %s; want 200", route.name, reachable.Code, reachable.Body.String())
			}

			// The next protocol line the client writes arms the swallow; every byte after it is
			// discarded and the connection stays open.
			relay.arm("\r\n", 1)
			started := fake.Now()
			answered := routeAnswer(t, handler, request())
			fake.AwaitArmedAt(t, "the "+route.name+" request budget", started.Add(sendRequestBudget))
			testwait.Receive(t, "the relay to swallow the "+route.name+"'s bus traffic", relay.swallowed)
			finishUnansweredPublish(t, fake, started)
			recorder := testwait.Receive(t, "the "+route.name+" to refuse the silent bus at the request budget", answered)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s with the bus silent: status %d, body %s; want 503", route.name, recorder.Code, recorder.Body.String())
			}
			var refusal listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
				t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
			}
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
		})
	}
}

// The sender stamp spends one read window on a bus that stopped answering, not one window for
// each half of the stamp: its interest read and its title read share the window
// senderStampReadTimeout bounds (senderStamp), so a send whose two reads both go unanswered still
// leaves its publish the 1.5 s that answers the named 503
// (TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget). It is measured on the stamp
// itself rather than through a route, because a route spends whatever its reads leave of
// sendRequestBudget on the publish and so answers at about the same moment whatever the stamp did.
//
// Each subtest has its own relay and caches. The cached sender needs no bus read.
// With no cached title the stamp waits for a session read. With neither record
// cached, the unanswered interest and session reads still share the same window.
func TestTheSenderStampSpendsOneReadWindowOnAFrozenBus(t *testing.T) {
	t.Parallel()
	const (
		sender = "ses_frozen_stamp_sender"
		title  = "the cached sender's title"
	)
	for _, row := range []struct {
		name              string
		interest, session bool
	}{
		{name: "both cached", interest: true, session: true},
		{name: "title missing", interest: true},
		{name: "neither cached"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			relay := startListenerRelay(t, testnats.URL(t))
			client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener's bus through the relay: %v", err)
			}
			t.Cleanup(client.Close)
			rig := openStampRig(t, client)
			if row.interest {
				if _, err := rig.registry.Upsert(store.Interest{
					SessionID: sender, MachineID: "test-machine", Dir: "/test/" + sender,
				}, []string{contracts.LegacyAgentSubject(sender)}); err != nil {
					t.Fatalf("write the sender's interest record: %v", err)
				}
			}
			if _, held := rig.registry.Cached(sender); held != row.interest {
				t.Fatalf("interest cached = %t, want %t", held, row.interest)
			}
			wantTitle := ""
			if row.session {
				rig.register(t, sender, session.SessionEntry{
					SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + sender, Title: title,
				})
				wantTitle = title
			}

			relay.arm(sender, 1)
			if row.session {
				// Freeze the relay before the cache-hit stamp starts. No stamp traffic is needed
				// to freeze it, and no fake deadline advances while this stamp runs.
				if err := client.Conn.Publish("test.freeze."+sender, nil); err != nil {
					t.Fatalf("send freeze marker: %v", err)
				}
				testwait.Receive(t, "the relay to swallow the freeze marker", relay.swallowed)
			}
			fake := &stampWindowCounter{Fake: clocktest.New(time.Unix(0, 0))}
			deadline := fake.Now().Add(senderStampReadTimeout)
			answered := rig.startStamp(t, fake, sender)
			if !row.session {
				expireStampRead(t, fake.Fake, deadline, relay.swallowed)
			}
			var stamp *contracts.EnvelopeSender
			testwait.Until(t, "the sender stamp to return without another read window", func() (string, bool) {
				if fake.windows.Load() > 1 {
					return "the stamp armed another read window", true
				}
				select {
				case stamp = <-answered:
					return "the stamp returned", true
				default:
					return "the stamp has not returned", false
				}
			})
			if got := fake.windows.Load(); got > 1 || (!row.session && got != 1) {
				t.Fatalf("the stamp armed %d read windows, want one for a read-through and at most one for a cache hit", got)
			}
			if stamp == nil || stamp.SessionID != sender {
				t.Fatalf("stamp of %s = %+v, want the sender itself", sender, stamp)
			}
			if stamp.Title != wantTitle {
				t.Fatalf("stamp of %s carries the title %q, want %q", sender, stamp.Title, wantTitle)
			}
		})
	}
}

// Count every arm, including one stopped before it could appear in Fake.Armed.
type stampWindowCounter struct {
	*clocktest.Fake
	windows atomic.Int32
}

func (c *stampWindowCounter) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	c.windows.Add(1)
	return c.Fake.WithTimeout(ctx, d)
}

// A bus that is reachable and does not accept the publish is the same 503, carrying the server's
// own reason: here the notification stream is deleted under the running listener, so the
// JetStream publish to notifications.agent.<id> is left without a stream to acknowledge it
// (nats.ErrNoStreamResponse). The record is asserted with it, because a 503 nothing records is
// the failure this route's line exists to end.
//
// The two answers that must not be swept into that 503 are asserted in the same test, against the
// same live bus: an envelope past the server's max payload is still the caller's own 413, and a
// subject NATS does not accept is still a 400, neither carrying Retry-After, since retrying
// either sends the same bytes to the same refusal. The invalid subject is reached through the
// publish route, not the send route: a send's subject is built from target_session, and a target
// that would make an invalid agent subject is first read from the session bucket, where nats.go
// refuses the key and the route answers 404 before the bus ever sees the subject. A publish's
// topic goes to the bus as written, so `notifications..x` clears the handler's prefix checks and
// is refused by the bus's own subject check (bus.ErrInvalidSubject).
func TestSendAnswersServiceUnavailableWhenTheBusRefusesThePublish(t *testing.T) {
	t.Parallel()
	const target = "ses_refused_publish_target"
	client := setupPublishTestClient(t)
	state := withPublishRoute(t, budgetRouteDeps(t, client, clocktest.New(time.Unix(0, 0)), target))
	// The stream goes after every store is open: opening one reconciles the stream back.
	if err := client.JS().DeleteStream(bus.Stream); err != nil {
		t.Fatalf("delete the notification stream: %v", err)
	}

	recorder, record := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) },
		postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_refused_publish_sender",
			"message":        "the stream is gone",
		})).WithContext(testwait.Context(t)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("send the bus refuses: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
	}
	var refusal listenerwire.APIError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	reason, found := strings.CutPrefix(refusal.Error, busUnansweredPrefix+": ")
	if !found || strings.TrimSpace(reason) == "" {
		t.Fatalf("the refusal reads %q, want %q followed by the bus's own reason", refusal.Error, busUnansweredPrefix)
	}
	if !strings.Contains(reason, nats.ErrNoStreamResponse.Error()) {
		t.Fatalf("the refusal carries %q, want the bus's own answer to a publish no stream takes (%v)", reason, nats.ErrNoStreamResponse)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	assertRecordFields(t, record, map[string]any{
		"msg":            "listener send",
		"level":          "WARN",
		"outcome":        "bus_unavailable",
		"status":         float64(http.StatusServiceUnavailable),
		"target_session": target,
	})
	t.Logf("bus refused the publish: %s", refusal.Error)

	// An envelope past the server's max payload: nats.go refuses it before sending, which the bus
	// reports as bus.ErrTooLarge, and that is the caller's own 413, not a bus that did not answer.
	oversized, oversizedRecord := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) },
		postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_refused_publish_sender",
			"message":        strings.Repeat("a", 2<<20),
		})).WithContext(testwait.Context(t)))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized send: status %d, body %s; want 413", oversized.Code, oversized.Body.String())
	}
	if got := oversized.Header().Get("Retry-After"); got != "" {
		t.Fatalf("an oversized send carries Retry-After %q: the same bytes are refused however often they are sent", got)
	}
	assertRecordFields(t, oversizedRecord, map[string]any{
		"msg": "listener send", "level": "WARN", "outcome": "too_large",
		"status": float64(http.StatusRequestEntityTooLarge),
	})

	// A topic with an empty token: no stream subject matches it and NATS does not accept it, so
	// the bus refuses it as bus.ErrInvalidSubject and the route answers 400.
	invalid, invalidRecord := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
		postJSON("/v1/messages/publish", sendBody(map[string]any{
			"topic":          "notifications..x",
			"source":         "agent",
			"source_session": "ses_refused_publish_sender",
			"message":        "a subject NATS will not take",
		})).WithContext(testwait.Context(t)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("a publish to notifications..x: status %d, body %s; want 400", invalid.Code, invalid.Body.String())
	}
	if got := invalid.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a publish NATS refuses carries Retry-After %q: the same subject is refused however often it is sent", got)
	}
	assertRecordFields(t, invalidRecord, map[string]any{
		"msg": "listener publish", "level": "WARN", "outcome": "invalid_request",
		"status": float64(http.StatusBadRequest), "topic": "notifications..x",
	})
}

// Every inbound send and publish leaves exactly one line, so a send that was never attempted, one
// attempted and lost and one attempted and refused can be told apart from the listener's own log
// afterwards. This drives both routes against a real bus and reads each line back field by field,
// as the starting gate's record test reads its own.
//
// Each answer is asserted to be exactly one line, not at least one: a route that logged its
// request twice, or that logged a second line from elsewhere in the same request, would read as
// two requests to anyone counting the record. The refusals are the half no other system holds:
// nothing was published, so no arrival line and no stream entry will ever name them, which is why
// the line's own fields (the summary among them) are checked on a refusal that has an envelope
// and on one refused before any envelope exists.
func TestListenerRecordsEverySendAndPublishRequest(t *testing.T) {
	t.Parallel()
	const target = "ses_recorded_target"
	const sender = "ses_recorded_sender"
	client := setupPublishTestClient(t)
	state := withPublishRoute(t, budgetRouteDeps(t, client, clocktest.New(time.Unix(0, 0)), target, sender))
	registry := state.registry
	send := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		return serveRecorded(t,
			func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) },
			postJSON("/v1/messages/send", body).WithContext(testwait.Context(t)))
	}
	publish := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		return serveRecorded(t,
			func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
			postJSON("/v1/messages/publish", body).WithContext(testwait.Context(t)))
	}

	accepted, acceptedRecord := send(sendBody(map[string]any{
		"target_session": target,
		"source":         "dispatch",
		"source_session": sender,
		"message":        "ship it",
	}))
	if accepted.Code != http.StatusOK {
		t.Fatalf("an accepted send: status %d, body %s", accepted.Code, accepted.Body.String())
	}
	var answered contracts.Envelope
	if err := json.Unmarshal(accepted.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode the answer %s: %v", accepted.Body.String(), err)
	}
	if answered.EventID == "" {
		t.Fatalf("the accepted send answered no event id: %s", accepted.Body.String())
	}
	assertRecordFields(t, acceptedRecord, map[string]any{
		"msg":             "listener send",
		"level":           "INFO",
		"outcome":         "accepted",
		"status":          float64(http.StatusOK),
		"target_session":  target,
		"event_id":        answered.EventID,
		"source":          "dispatch",
		"source_session":  sender,
		"payload_summary": "ship it",
	})

	gone, goneRecord := send(sendBody(map[string]any{
		"target_session": "ses_recorded_gone",
		"source":         "agent",
		"source_session": sender,
		"message":        "anyone there?",
	}))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("a send to a session the store does not hold: status %d, body %s; want 404", gone.Code, gone.Body.String())
	}
	assertRecordFields(t, goneRecord, map[string]any{
		"msg":             "listener send",
		"level":           "WARN",
		"outcome":         "no_live_session",
		"status":          float64(http.StatusNotFound),
		"target_session":  "ses_recorded_gone",
		"source":          "agent",
		"source_session":  sender,
		"payload_summary": "anyone there?",
	})

	// Refused for its body: no envelope was ever built, so the line carries the destination and
	// the empty envelope fields rather than being skipped.
	empty, emptyRecord := send(sendBody(map[string]any{
		"target_session": target,
		"source":         "agent",
		"source_session": sender,
		"message":        "   ",
	}))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("a send with an empty message: status %d, body %s; want 400", empty.Code, empty.Body.String())
	}
	assertRecordFields(t, emptyRecord, map[string]any{
		"msg":             "listener send",
		"level":           "WARN",
		"outcome":         "invalid_request",
		"status":          float64(http.StatusBadRequest),
		"target_session":  target,
		"event_id":        "",
		"payload_summary": "",
	})

	// Refused for the target: it is live, but the frame asks for a delivery mode it does not
	// advertise (the store holds it with no capabilities at all), so the route never publishes.
	// The envelope was built, so the line carries its fields.
	unadvertised, unadvertisedRecord := send(sendBody(map[string]any{
		"target_session": target,
		"source":         "dispatch",
		"source_session": sender,
		"message":        "steer this",
		"payload":        `{"delivery":{"mode":"steer"}}`,
	}))
	if unadvertised.Code != http.StatusForbidden {
		t.Fatalf("a send asking for a mode the target does not advertise: status %d, body %s; want 403", unadvertised.Code, unadvertised.Body.String())
	}
	assertRecordFields(t, unadvertisedRecord, map[string]any{
		"msg":             "listener send",
		"level":           "WARN",
		"outcome":         "capability_refused",
		"status":          float64(http.StatusForbidden),
		"target_session":  target,
		"source":          "dispatch",
		"source_session":  sender,
		"payload_summary": "steer this",
	})
	if eventID, _ := unadvertisedRecord["event_id"].(string); eventID == "" {
		t.Fatalf("the capability refusal's line carries no event id, though its envelope was built: %v", unadvertisedRecord)
	}

	const topic = "notifications.team.recorded-squad"
	published, publishedRecord := publish(sendBody(map[string]any{
		"topic":          topic,
		"source":         "agent",
		"source_session": sender,
		"message":        "deploy complete",
	}))
	if published.Code != http.StatusOK {
		t.Fatalf("an accepted publish: status %d, body %s", published.Code, published.Body.String())
	}
	var publishedEnvelope contracts.Envelope
	if err := json.Unmarshal(published.Body.Bytes(), &publishedEnvelope); err != nil {
		t.Fatalf("decode the answer %s: %v", published.Body.String(), err)
	}
	assertRecordFields(t, publishedRecord, map[string]any{
		"msg":             "listener publish",
		"level":           "INFO",
		"outcome":         "accepted",
		"status":          float64(http.StatusOK),
		"topic":           topic,
		"event_id":        publishedEnvelope.EventID,
		"source":          "agent",
		"source_session":  sender,
		"payload_summary": "deploy complete",
	})

	// A role nobody holds: the listener keeps the message for the role's next holder and answers
	// 202, so the line is an accepted one. There is still no holder to name.
	unclaimed := contracts.RoleTopicPrefix + "recorded-unclaimed"
	keptUnclaimed, keptUnclaimedRecord := publish(sendBody(map[string]any{
		"topic":          unclaimed,
		"source":         "agent",
		"source_session": sender,
		"message":        "nobody holds this",
	}))
	if keptUnclaimed.Code != http.StatusAccepted {
		t.Fatalf("a publish to a role nobody holds: status %d, body %s; want 202 for a kept message", keptUnclaimed.Code, keptUnclaimed.Body.String())
	}
	assertRecordFields(t, keptUnclaimedRecord, map[string]any{
		"msg":            "listener publish",
		"level":          "INFO",
		"outcome":        "accepted",
		"status":         float64(http.StatusAccepted),
		"topic":          unclaimed,
		"source_session": sender,
	})
	if holder, ok := keptUnclaimedRecord["holder"]; ok {
		t.Fatalf("the unclaimed role's line names holder %v: there is no holder to name", fmt.Sprint(holder))
	}

	// The role is claimed, but by a session the store does not hold: the claim has lapsed and
	// stands, and the listener keeps the message for the role's next live holder as it does for a
	// role nobody holds. The answer is 202, so the line is an accepted one, and it names the
	// holder the answer's own body names, and the copy the answer names.
	const lapsedHolder = "ses_recorded_lapsed_holder"
	lapsedRole := "recorded-lapsed"
	if _, err := registry.SetRole(lapsedHolder, "test-machine", lapsedRole, false); err != nil {
		t.Fatalf("claim role %s for a session the store does not hold: %v", lapsedRole, err)
	}
	lapsedTopic := contracts.RoleTopicPrefix + lapsedRole
	lapsed, lapsedRecord := publish(sendBody(map[string]any{
		"topic":          lapsedTopic,
		"source":         "agent",
		"source_session": sender,
		"message":        "is the holder still there?",
	}))
	if lapsed.Code != http.StatusAccepted {
		t.Fatalf("a publish to a role whose holder lapsed: status %d, body %s; want 202 for a kept message", lapsed.Code, lapsed.Body.String())
	}
	var lapsedAnswer struct {
		EventID string `json:"event_id"`
		Holder  string `json:"holder"`
	}
	if err := json.Unmarshal(lapsed.Body.Bytes(), &lapsedAnswer); err != nil {
		t.Fatalf("decode the answer %s: %v", lapsed.Body.String(), err)
	}
	if lapsedAnswer.Holder != lapsedHolder {
		t.Fatalf("the kept answer names holder %q, want %s", lapsedAnswer.Holder, lapsedHolder)
	}
	assertRecordFields(t, lapsedRecord, map[string]any{
		"msg":             "listener publish",
		"level":           "INFO",
		"outcome":         "accepted",
		"status":          float64(http.StatusAccepted),
		"topic":           lapsedTopic,
		"event_id":        lapsedAnswer.EventID,
		"source":          "agent",
		"source_session":  sender,
		"payload_summary": "is the holder still there?",
		"holder":          lapsedHolder,
	})
}

// A session read that did not answer is no evidence the session is gone, so the send that made it
// is refused 503 naming the bus rather than 404 naming the session. The 404 is the one answer a
// sender routes around the pane on, so it must rest on the bucket's own word
// (nats.ErrKeyNotFound) and on nothing else — reporting an absence this listener never checked is
// the incident INFRA-141 is about. It is the one row of the send route's status map that only an
// unanswered read reaches: the cold-cache test's read answers, the bus-unreachable and
// bus-refusal tests fail at the publish with the target already in the cache, and the absent
// target's 404 is the bucket answering. Collapsing the two errors back into one 404 leaves every
// other test in this package green.
//
// The negative control is the first send: the same route, the same listener, the same
// unregistered target, with the bucket answering. It is the 404, and the second send differs from
// it in one thing only — the bucket no longer answers — so a rig that refused every send could
// not produce the pair.
func TestSendRefusesTheBusRatherThanTheSessionWhenTheStoreReadDoesNotAnswer(t *testing.T) {
	t.Parallel()
	const target = "ses_unanswered_store_read"
	const sender = "ses_unanswered_store_sender"
	fake := clocktest.New(time.Unix(0, 0))
	observer := budgetRouteDeps(t, setupPublishTestClient(t), fake)
	log := &cmdtest.LockedBuffer{}
	handler := sendHandler(observer, logging.NewWithWriter("test", log))
	request := func() *http.Request {
		return postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": sender,
			"message":        "is the pane there?",
		}))
	}

	answered := testwait.Receive(t, "the control send to answer", routeAnswer(t, handler, request()))
	answeredRecord := soleRecord(t, log.String())
	if answered.Code != http.StatusNotFound {
		t.Fatalf("a send whose bucket read answered that the key is gone: status %d, body %s; want 404", answered.Code, answered.Body.String())
	}
	// The error keeps its prefix - Dispatch classifies a delivery failure on the "no live
	// session " one - the state the refusal was decided on stands beside it, and, this listener
	// having no registration to name for a target that never registered, the error says so. The
	// body is spelt out rather than built from contracts.NoLiveSession, so a change to the
	// constant, which changes the wire, fails here.
	if got, want := strings.TrimSpace(answered.Body.String()), fmt.Sprintf(`{"error":"no live session %s; this listener does not know when it last registered","state":"gone"}`, target); got != want {
		t.Fatalf("the 404 body is %s, want %s", got, want)
	}
	assertRecordFields(t, answeredRecord, map[string]any{
		"msg":            "listener send",
		"level":          "WARN",
		"outcome":        "no_live_session",
		"status":         float64(http.StatusNotFound),
		"target_session": target,
		"source_session": sender,
	})

	observer.sessions.EndWatch(testwait.Context(t))
	readStarted := stallObservedSessionRead(t, observer.sessions)
	started := fake.Now()
	budgetEnd := started.Add(sendRequestBudget)
	readEnd := started.Add(sendSessionReadTimeout)
	if !readEnd.Before(budgetEnd) {
		t.Fatalf("the session read ends at %s, want it to end before the request budget at %s", readEnd, budgetEnd)
	}
	logged := len(log.String())
	pending := routeAnswer(t, handler, request())
	fake.AwaitArmedAt(t, "the send request budget", budgetEnd)
	fake.AwaitArmedAt(t, "the target session read deadline", readEnd)
	if key := testwait.Receive(t, "the target's bucket read to stall", readStarted); key != target {
		t.Fatalf("the bucket read %q, want the target %q", key, target)
	}
	fake.Set(readEnd)
	unanswered := testwait.Receive(t, "the send to refuse the stalled read before advancing the request budget", pending)
	unansweredRecord := soleRecord(t, log.String()[logged:])
	if unanswered.Code != http.StatusServiceUnavailable {
		t.Fatalf("a send whose bucket read never answered: status %d, body %s; want 503, since nothing was learned about %s", unanswered.Code, unanswered.Body.String(), target)
	}
	var refusal listenerwire.APIError
	if err := json.Unmarshal(unanswered.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", unanswered.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
	}
	if strings.Contains(refusal.Error, contracts.NoLiveSession) {
		t.Fatalf("the refusal reads %q: an unanswered read must not be reported as the session being gone", refusal.Error)
	}
	if got := unanswered.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: the sender is being asked to try the same route again", got)
	}
	if !strings.Contains(refusal.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("the refusal reads %q, want the target read's deadline error", refusal.Error)
	}
	assertRecordFields(t, unansweredRecord, map[string]any{
		"msg":            "listener send",
		"level":          "WARN",
		"outcome":        "bus_unavailable",
		"status":         float64(http.StatusServiceUnavailable),
		"target_session": target,
		"source_session": sender,
	})
}
