package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
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

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: an answer inside the client's 5 s attempt, from a route whose budget is 3.5 s.
//
// An unsubscribe the listener answers while its NATS connection is reconnecting fails by name, and
// inside the client's own deadline (R14 of dispatch://INFRA-3589), as a registration does
// (TestARegistrationThroughANATSReconnectFailsByName). An unsubscribe of a session with a consumer
// removes topics from the session's interest row and then sets the consumer's filter to what is
// left (sessionDelivery.refollow), and the client waits for both. Its client is the plugin, whose
// envoy-client gives one attempt contracts.ListenerCallTimeout: an answer that comes later
// reaches nobody. So the answer is a 503 whose reason names the reconnect, inside that attempt,
// and no listener line names nats.go's buffer refusal.
func TestAnUnsubscribeThroughANATSReconnectFailsByName(t *testing.T) {
	const (
		machineID = "reconnect-unsubscribe"
		sessionID = "ses_reconnect_unsubscribe"
		topic     = "notifications.team.reconnect-unsubscribe"
	)
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID)
	listener.waitHealthy(t)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1", topic))

	disconnected := func() int {
		return strings.Count(listener.Output.String(), "envoy nats disconnected name=envoy ")
	}
	before := disconnected()
	server.Stop(t)
	waitFor(t, "the listener to see the NATS server go", func() bool { return disconnected() > before })

	started := time.Now()
	answer := reconnectWritesCall(listener.port, "/v1/interests/unsubscribe", unsubscribeBody(t, sessionID, topic), 15*time.Second)
	took := time.Since(started)
	t.Logf("the unsubscribe during the outage answered %s after %s", answer, took.Round(time.Millisecond))
	if answer.err != nil || answer.status != http.StatusServiceUnavailable || !strings.Contains(answer.body, reconnectingReason) {
		t.Errorf("the unsubscribe during the outage answered %s, want 503 naming %q", answer, reconnectingReason)
	}
	if took >= contracts.ListenerCallTimeout {
		t.Errorf("the unsubscribe during the outage was answered after %s, want inside the client's %s attempt (contracts.ListenerCallTimeout), or the plugin never reads the answer",
			took.Round(time.Millisecond), contracts.ListenerCallTimeout)
	}
	if strings.Contains(listener.Output.String(), reconnectBufferRefusal) {
		t.Errorf("the listener logged %q during the outage, want the reconnect named instead", reconnectBufferRefusal)
	}
}

// consumerCallCutRig is a listener rig whose session-consumer connection alone goes through a
// proxy the test can cut, so the bucket keeps answering while that connection reconnects.
type consumerCallCutRig struct {
	*sessionConsumerRig
	proxy *testnats.Proxy
}

func newConsumerCallCutRig(t *testing.T) *consumerCallCutRig {
	t.Helper()
	_, uri := testnats.Start(t)
	proxy := testnats.StartProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := &consumerCallCutRig{sessionConsumerRig: newSessionConsumerRigOn(t, client, proxy.URL()), proxy: proxy}
	t.Cleanup(proxy.Restore)
	return rig
}

// consumerCall is one call over the session-consumer connection: the info of sessionID's
// consumer, bounded by 200 ms.
func (rig *consumerCallCutRig) consumerCall(sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := rig.deps.consumers.consumers.Info(ctx, sessionID)
	return err
}

// cutAtNextFilterWrite makes the next filter write cut the proxy after it reads the stored row and
// before it sends the call. The write then waits until the connection is reconnecting, which the
// test reads through sessionID's consumer. It cuts once.
func (rig *consumerCallCutRig) cutAtNextFilterWrite(t *testing.T, sessionID string) {
	var cut sync.Once
	rig.deps.consumers.afterStoredRowRead = func() {
		cut.Do(func() {
			rig.proxy.Cut()
			waitFor(t, "the listener's session-consumer connection to start reconnecting", func() bool {
				return errors.Is(rig.consumerCall(sessionID), bus.ErrReconnecting)
			})
		})
	}
}

// restore brings the proxy back and waits until sessionID's consumer answers over the
// session-consumer connection again.
func (rig *consumerCallCutRig) restore(t *testing.T, sessionID string) {
	t.Helper()
	rig.proxy.Restore()
	waitFor(t, "the listener's session-consumer connection to reconnect", func() bool {
		return rig.consumerCall(sessionID) == nil
	})
}

// serve answers one request through the listener's /v1 routes.
func (rig *consumerCallCutRig) serve(method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder
}

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: the cut connection's close inside 3 s, and an answer inside the client's 5 s attempt.
//
// An unsubscribe whose store work answers and whose consumer call meets a NATS reconnect fails by
// name, and inside the client's own deadline, as a registration's consumer call does. The filter
// update after the row is written (sessionDelivery.refollow) is the route's one consumer call, and
// its client waits for it: it runs within sendRequestBudget of the request's arrival, as the
// route's store work does, and a call that failed because NATS is reconnecting is the
// bus-unavailable 503 that names the reconnect. The session-consumer connection alone goes
// through a proxy, which the test cuts after the update read the row and before it sends the
// call, so the bucket keeps answering.
//
// An update the route's budget did not bound would wait out sessionConsumerCallTimeout (5 s) from
// its own start and only be logged, and the route would answer 200 after the client's attempt.
func TestAnUnsubscribeWhoseConsumerCallMeetsANATSReconnectFailsByName(t *testing.T) {
	const (
		sessionID = "ses_unsubscribe_consumer_call"
		topic     = "notifications.team.unsubscribe-consumer-call"
	)
	rig := newConsumerCallCutRig(t)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", topic))
	rig.cutAtNextFilterWrite(t, sessionID)

	started := time.Now()
	recorder := rig.serve(http.MethodPost, "/v1/interests/unsubscribe", unsubscribeBody(t, sessionID, topic))
	took := time.Since(started)
	t.Logf("the unsubscribe whose consumer call met the reconnect answered %d %s after %s",
		recorder.Code, strings.TrimSpace(recorder.Body.String()), took.Round(time.Millisecond))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), reconnectingReason) {
		t.Errorf("the unsubscribe answered %d %s, want 503 naming %q", recorder.Code, recorder.Body.String(), reconnectingReason)
	}
	if took >= contracts.ListenerCallTimeout {
		t.Errorf("the unsubscribe was answered after %s, want inside the client's %s attempt (contracts.ListenerCallTimeout)",
			took.Round(time.Millisecond), contracts.ListenerCallTimeout)
	}
	if strings.Contains(rig.harness.logs.String(), reconnectBufferRefusal) {
		t.Errorf("the listener logged %q, want the reconnect named instead", reconnectBufferRefusal)
	}
}

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: the cut connection's close inside 3 s, within the route's 3.5 s budget.
//
// The retry of an unsubscribe that took every topic of the session's record answers 200. The
// first attempt removes the topics, and the removal of the last one deletes the record
// (store.Registry.Remove). Its filter update then meets a NATS reconnect, so the route answers
// the bus-outage 503 after the record is gone. envoy-client retries a 5xx once with the same
// request. The retry finds no record. A removal from a session the bucket holds no record for
// removes nothing and is no error, so the route answers 200. Its removed list is empty, since no
// topic left a record in that request.
//
// A store that read an absent record as an error would answer the retry 500 `nats: key not found`
// for a removal that did happen.
func TestTheRetryOfAnUnsubscribeThatEmptiedTheRecordAnswers200(t *testing.T) {
	const (
		sessionID = "ses_unsubscribe_emptied_record"
		topic     = "notifications.team.unsubscribe-emptied-record"
	)
	rig := newConsumerCallCutRig(t)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", topic))
	read := rig.serve(http.MethodGet, "/v1/interests/"+sessionID, "")
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

	rig.cutAtNextFilterWrite(t, sessionID)
	first := rig.serve(http.MethodPost, "/v1/interests/unsubscribe", body)
	t.Logf("the unsubscribe of every topic whose consumer call met the reconnect answered %d %s",
		first.Code, strings.TrimSpace(first.Body.String()))
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), reconnectingReason) ||
		first.Header().Get("Retry-After") != "1" {
		t.Fatalf("the first attempt answered %d %s (Retry-After %q), want 503 naming %q with Retry-After 1",
			first.Code, first.Body.String(), first.Header().Get("Retry-After"), reconnectingReason)
	}
	if gone := rig.serve(http.MethodGet, "/v1/interests/"+sessionID, ""); gone.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/interests/%s after the first attempt answered %d %s, want 404: that attempt deleted the record",
			sessionID, gone.Code, gone.Body.String())
	}
	rig.restore(t, sessionID)

	retry := rig.serve(http.MethodPost, "/v1/interests/unsubscribe", body)
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
	if gone := rig.serve(http.MethodGet, "/v1/interests/"+sessionID, ""); gone.Code != http.StatusNotFound {
		t.Errorf("GET /v1/interests/%s after the retry answered %d %s, want 404", sessionID, gone.Code, gone.Body.String())
	}
}
