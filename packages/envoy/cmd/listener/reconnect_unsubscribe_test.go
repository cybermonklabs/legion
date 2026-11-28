package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// unsubscribeBody is the body of an unsubscribe of topics from sessionID, as the plugin sends it.
func unsubscribeBody(t *testing.T, sessionID string, topics ...string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "topics": topics})
	if err != nil {
		t.Fatalf("marshal the unsubscribe: %v", err)
	}
	return string(body)
}

// An unsubscribe the listener answers while its NATS connection is reconnecting fails by name.
// TestReconnectRouteBudgetsFitTheClientAttempt checks the production budget against the caller's.
func TestAnUnsubscribeThroughANATSReconnectFailsByName(t *testing.T) {
	t.Parallel()
	const (
		machineID = "reconnect-unsubscribe"
		sessionID = "ses_reconnect_unsubscribe"
		topic     = "notifications.team.reconnect-unsubscribe"
	)
	server, listener := startShortBudgetReconnectFixture(t, machineID, consumerRegistration(sessionID, "epoch-1", topic))
	stopReconnectServer(t, server, listener)

	from := len(listener.Output.String())
	answer := callReconnectRoute(t, listener, "/v1/interests/unsubscribe", unsubscribeBody(t, sessionID, topic))
	t.Logf("the unsubscribe during the outage answered %s", answer)
	if answer.err != nil || answer.status != http.StatusServiceUnavailable ||
		!strings.Contains(answer.body, "write the interest record "+sessionID) ||
		!strings.Contains(answer.body, reconnectingReason+": context deadline exceeded") {
		t.Fatalf("the unsubscribe during the outage answered %s, want 503 naming the interest write and the reconnect deadline", answer)
	}
	t.Log(waitChildRecord(t, listener, from, "listener unsubscribe", "session_id", sessionID))
	if strings.Contains(listener.Output.String(), reconnectBufferRefusal) {
		t.Errorf("the listener logged %q during the outage, want the reconnect named instead", reconnectBufferRefusal)
	}
}

// TestReconnectRouteBudgetsFitTheClientAttempt checks production constants, not elapsed time.
// Registration gives its final known-session record write a separate window. The session delete's
// consumer cleanup happens after its response, so the client does not wait for it.
func TestReconnectRouteBudgetsFitTheClientAttempt(t *testing.T) {
	t.Parallel()
	for _, bound := range []struct {
		name string
		end  time.Duration
	}{
		{"reconnect refusals", sendRequestBudget},
		{"registration including the final known-session record", sendRequestBudget + knownRecordWriteTimeout},
	} {
		if bound.end >= contracts.ListenerCallTimeout {
			t.Errorf("%s can take %s, want less than the client's %s attempt", bound.name, bound.end, contracts.ListenerCallTimeout)
		}
	}
}

// newPromptSessionConsumerRig opens both listener connections with prompt reconnect.
// It waits for the registries' first scans through the shared test deadline.
func newPromptSessionConsumerRig(t *testing.T, streamURL, consumerURL string) *sessionConsumerRig {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{streamURL}, bus.WithReplicas(1), bus.WithPromptReconnect())
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	ctx := testwait.Context(t)
	registry, err := store.Open(ctx, client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm interest and role claim caches: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(ctx, client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm session cache: %v", err)
	}
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test-machine", logs)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("listener log:\n%s", logs.String())
		}
	})
	cfg := listenerDeliveryHandlerConfig{client: client, registry: registry, sessions: sessions, logger: logger, machineID: "test-machine", now: time.Now}
	delivery, err := connectSessionDelivery([]string{consumerURL},
		inactiveThresholdFor(sessions, defaultSessionReapInterval, defaultSessionConsumerSweepInterval),
		client, logger, bus.WithPromptReconnect(), bus.WithLogger(logger.Slog()))
	if err != nil {
		t.Fatalf("connect the listener's session consumers: %v", err)
	}
	t.Cleanup(delivery.close)
	delivery.relay = &recordingRelay{}
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions, consumers: delivery, delivery: &cfg}
	mux := http.NewServeMux()
	registerV1Routes(mux, deps, cfg.machineID, logger)
	return &sessionConsumerRig{
		harness: listenerDeliveryHarness{client: client, registry: registry, sessions: sessions, logs: logs, config: cfg},
		deps:    deps,
		mux:     mux,
	}
}

func waitConsumerConnectionEvent(t *testing.T, rig *sessionConsumerRig, from int, event string) {
	t.Helper()
	testwait.Until(t, "the session-consumer connection to log "+event, func() (string, bool) {
		logs := rig.harness.logs.String()[from:]
		return logs, len(linesIn(logs, event, map[string]any{"name": "envoy-session-consumers"})) > 0
	})
}

func serveReconnectRequest(ctx context.Context, rig *sessionConsumerRig, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body)))
	return recorder
}

func serveReconnectRoute(t *testing.T, rig *sessionConsumerRig, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := testwait.Context(t)
	var answer *httptest.ResponseRecorder
	testwait.Returns(t, method+" "+path+" to answer", func() {
		answer = serveReconnectRequest(ctx, rig, method, path, body)
	})
	return answer
}

func registerReconnectSession(t *testing.T, rig *sessionConsumerRig, body string) {
	t.Helper()
	if answer := serveReconnectRoute(t, rig, http.MethodPost, "/v1/interests/subscribe", body); answer.Code != http.StatusOK {
		t.Fatalf("register the session: %d %s", answer.Code, answer.Body.String())
	}
}

// consumerCallCutRig cuts the session-consumer connection alone, so the bucket keeps answering.
type consumerCallCutRig struct {
	*sessionConsumerRig
	proxy *testnats.Proxy
}

func newConsumerCallCutRig(t *testing.T) *consumerCallCutRig {
	t.Helper()
	_, uri := testnats.Start(t)
	proxy := testnats.StartProxy(t, uri)
	rig := &consumerCallCutRig{sessionConsumerRig: newPromptSessionConsumerRig(t, uri, proxy.URL()), proxy: proxy}
	t.Cleanup(proxy.Restore)
	return rig
}

// unsubscribeInOutage cuts only the consumer connection after the route's store work.
// The consumer's Info hook shows that the filter update reached its NATS call.
func (rig *consumerCallCutRig) unsubscribeInOutage(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	fake := clocktest.New(time.Unix(0, 0))
	rig.deps.clock = fake
	rig.deps.consumers.clock = fake
	arrived := fake.Now()
	ctx := testwait.Context(t)
	disconnected := make(chan struct{})
	rig.deps.consumers.afterStoredRowRead = sync.OnceFunc(func() {
		rig.proxy.Cut()
		select {
		case <-disconnected:
		case <-ctx.Done():
		}
	})
	info := make(chan struct{})
	sawInfo := sync.OnceFunc(func() { close(info) })
	rig.deps.consumers.consumers.SetInfoFault(func(string) error {
		sawInfo()
		return nil
	})
	defer rig.deps.consumers.consumers.SetInfoFault(nil)
	from := len(rig.harness.logs.String())
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- serveReconnectRequest(ctx, rig.sessionConsumerRig, http.MethodPost, "/v1/interests/unsubscribe", body)
	}()
	waitConsumerConnectionEvent(t, rig.sessionConsumerRig, from, "envoy nats disconnected")
	close(disconnected)
	testwait.Receive(t, "the filter write to enter the consumer Info call", info)
	deadline := arrived.Add(sendRequestBudget)
	fake.AwaitArmedAt(t, "the unsubscribe route budget", deadline)
	fake.Set(deadline.Add(time.Nanosecond))
	answer := testwait.Receive(t, "the unsubscribe 503 after its budget ended", answered)
	rig.deps.consumers.afterStoredRowRead = nil
	return answer
}

// restore waits for a new call over the restored session-consumer connection.
func (rig *consumerCallCutRig) restore(t *testing.T, sessionID string) {
	t.Helper()
	from := len(rig.harness.logs.String())
	rig.proxy.Restore()
	waitConsumerConnectionEvent(t, rig.sessionConsumerRig, from, "envoy nats reconnected")
	if _, err := rig.deps.consumers.consumers.Info(testwait.Context(t), sessionID); err != nil {
		t.Fatalf("read the session consumer after reconnect: %v", err)
	}
}

// An unsubscribe whose store work succeeds but whose filter update meets a reconnect
// answers a 503 naming that reconnect when the route budget ends.
func TestAnUnsubscribeWhoseConsumerCallMeetsANATSReconnectFailsByName(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "ses_unsubscribe_consumer_call"
		topic     = "notifications.team.unsubscribe-consumer-call"
	)
	rig := newConsumerCallCutRig(t)
	registerReconnectSession(t, rig.sessionConsumerRig, consumerRegistration(sessionID, "epoch-1", topic))
	recorder := rig.unsubscribeInOutage(t, unsubscribeBody(t, sessionID, topic))
	t.Logf("the unsubscribe whose consumer call met the reconnect answered %d %s", recorder.Code, strings.TrimSpace(recorder.Body.String()))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), reconnectingReason+": context deadline exceeded") {
		t.Errorf("the unsubscribe answered %d %s, want 503 naming the reconnect at the route deadline", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(rig.harness.logs.String(), reconnectBufferRefusal) {
		t.Errorf("the listener logged %q, want the reconnect named instead", reconnectBufferRefusal)
	}
}

// The first unsubscribe removes every topic from the record before the filter call meets a
// reconnect. Its retry finds no record and answers 200 with an empty removed list, not a 500.
func TestTheRetryOfAnUnsubscribeThatEmptiedTheRecordAnswers200(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "ses_unsubscribe_emptied_record"
		topic     = "notifications.team.unsubscribe-emptied-record"
	)
	rig := newConsumerCallCutRig(t)
	registerReconnectSession(t, rig.sessionConsumerRig, consumerRegistration(sessionID, "epoch-1", topic))
	get := func() *httptest.ResponseRecorder {
		return serveReconnectRoute(t, rig.sessionConsumerRig, http.MethodGet, "/v1/interests/"+sessionID, "")
	}
	read := get()
	if read.Code != http.StatusOK {
		t.Fatalf("GET /v1/interests/%s answered %d %s, want 200", sessionID, read.Code, read.Body.String())
	}
	var record struct {
		Topics []string `json:"topics"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &record); err != nil {
		t.Fatalf("decode the record %s: %v", read.Body.String(), err)
	}
	// The record holds the session's own agent subject from every registration, so a removal
	// that empties it names that subject too.
	agentSubject := contracts.LegacyAgentSubject(sessionID)
	if !slices.Contains(record.Topics, agentSubject) || !slices.Contains(record.Topics, topic) {
		t.Fatalf("the record holds %v, want %s and %s among its topics", record.Topics, agentSubject, topic)
	}
	body := unsubscribeBody(t, sessionID, record.Topics...)

	first := rig.unsubscribeInOutage(t, body)
	t.Logf("the unsubscribe of every topic whose consumer call met the reconnect answered %d %s", first.Code, strings.TrimSpace(first.Body.String()))
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), reconnectingReason) ||
		first.Header().Get("Retry-After") != "1" {
		t.Fatalf("the first attempt answered %d %s (Retry-After %q), want 503 naming %q with Retry-After 1",
			first.Code, first.Body.String(), first.Header().Get("Retry-After"), reconnectingReason)
	}
	if gone := get(); gone.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/interests/%s after the first attempt answered %d %s, want 404: that attempt deleted the record",
			sessionID, gone.Code, gone.Body.String())
	}
	rig.restore(t, sessionID)

	retry := serveReconnectRoute(t, rig.sessionConsumerRig, http.MethodPost, "/v1/interests/unsubscribe", body)
	t.Logf("the retry once NATS was back answered %d %s", retry.Code, strings.TrimSpace(retry.Body.String()))
	if retry.Code != http.StatusOK {
		t.Fatalf("the retry answered %d %s, want 200: the first attempt removed every topic", retry.Code, retry.Body.String())
	}
	var answer struct {
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the retry's answer %s: %v", retry.Body.String(), err)
	}
	if answer.Removed == nil || len(answer.Removed) != 0 {
		t.Errorf("the retry answered removed %v (%s), want the empty list: no topic left a record in that request",
			answer.Removed, retry.Body.String())
	}
	if gone := get(); gone.Code != http.StatusNotFound {
		t.Errorf("GET /v1/interests/%s after the retry answered %d %s, want 404", sessionID, gone.Code, gone.Body.String())
	}
}
