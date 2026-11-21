package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A resend that reaches a three-copy stream while its earlier attempt is still being stored is
// refused by the stream as a publish whose message id is still in process (nats-server's 10158,
// bus.ErrDuplicateInProcess): the leader holds the id's dedupe entry with no sequence yet. That is
// how a hub failover leaves the stream for a sender whose first attempt went unanswered. The route
// publishes again until the earlier attempt is stored and its answer names that copy
// (publishPastInProcess), so both attempts are answered 200, and the stream holds one copy. It
// is never answered 503 bus_unavailable, "the message bus did not answer": the bus answered,
// and the sender would be told its message was not taken when its first attempt was.
//
// The rows are each path of the listener that publishes under a message id the sender's
// idempotency key names: the publish route to a team topic and to a topic the stream captures
// for a source, and the send route to a session. The rig is a real cluster: the listener is
// connected to the stream's leader alone, and the members' routes are frozen (FreezeRoutes), so
// the leader takes both attempts and can store neither until the thaw. The thaw comes once the
// leader has been handed both attempts, read off a subscriber on the leader itself.
func TestAResendWhoseEarlierAttemptIsStillInProcessIsAnsweredOnceThatAttemptIsStored(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	owner, err := bus.ConnectOwningStream(cluster.URLs(), bus.WithReplicas(3))
	if err != nil {
		t.Fatalf("connect the listener that creates the three-copy stream: %v", err)
	}
	t.Cleanup(owner.Close)
	js := cluster.JetStream(t)
	leaderName := testnats.AwaitStreamCopies(t, js, bus.Stream, 3).Cluster.Leader
	leader := cluster.Member(leaderName)
	if leader < 0 {
		t.Fatalf("the stream's leader %q is no member of the cluster", leaderName)
	}
	client, err := bus.ConnectOwningStream([]string{cluster.Servers[leader].URL()}, bus.WithReplicas(3))
	if err != nil {
		t.Fatalf("connect the listener to the stream's leader %s: %v", leaderName, err)
	}
	t.Cleanup(client.Close)
	registry, sessions := setupSessionsTest(t, nil, nil)
	d := &listenerDeps{client: client, registry: registry, sessions: sessions}
	watch := testnats.Connect(t, cluster.Servers[leader].URL())
	t.Cleanup(watch.Close)

	const target = "ses_in_process_target"
	if err := sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	for _, row := range []struct {
		name    string
		route   string
		subject string
		body    func(key string) map[string]any
	}{
		{"publish to a team topic", "/v1/messages/publish", "notifications.team.in-process",
			func(key string) map[string]any {
				return map[string]any{"topic": "notifications.team.in-process", "source": "agent", "message": "team, resent while in process", "idempotency_key": key}
			}},
		{"publish to a captured topic", "/v1/messages/publish", "notifications.github.legion-test.in_process.pr.1",
			func(key string) map[string]any {
				return map[string]any{"topic": "notifications.github.legion-test.in_process.pr.1", "source": "agent", "message": "captured, resent while in process", "idempotency_key": key}
			}},
		{"send to a session", "/v1/messages/send", contracts.LegacyAgentSubject(target),
			func(key string) map[string]any {
				return map[string]any{"target_session": target, "source": "agent", "message": "send, resent while in process", "idempotency_key": key}
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if now := testnats.AwaitStreamCopies(t, js, bus.Stream, 3).Cluster.Leader; now != leaderName {
				t.Fatalf("the stream's leader moved from %s to %s, and the rig's listener is connected to %s alone", leaderName, now, leaderName)
			}
			heard, err := watch.SubscribeSync(row.subject)
			if err != nil {
				t.Fatalf("subscribe to %s on the leader: %v", row.subject, err)
			}
			t.Cleanup(func() { _ = heard.Unsubscribe() })
			if err := watch.Flush(); err != nil {
				t.Fatalf("flush the subscription: %v", err)
			}
			encoded, err := json.Marshal(row.body(id.New()))
			if err != nil {
				t.Fatal(err)
			}
			thaw := cluster.FreezeRoutes(t)
			t.Cleanup(thaw)
			answers := make(chan *httptest.ResponseRecorder, 2)
			for range 2 {
				go func() { answers <- serveInProcessAttempt(d, row.route, string(encoded)) }()
			}
			for n := range 2 {
				if _, err := heard.NextMsg(10 * time.Second); err != nil {
					t.Fatalf("the leader was handed %d of the two attempts: %v", n, err)
				}
			}
			thaw()
			for range 2 {
				answer := <-answers
				if answer.Code != http.StatusOK {
					t.Errorf("an attempt was answered %d %s; want 200: its earlier attempt was still in process, and is stored", answer.Code, strings.TrimSpace(answer.Body.String()))
				}
			}
			info, err := js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: row.subject})
			if err != nil {
				t.Fatalf("read the stream's copies on %s: %v", row.subject, err)
			}
			if got := info.State.Subjects[row.subject]; got != 1 {
				t.Errorf("the stream holds %d copies on %s, want 1: both attempts carry one message id", got, row.subject)
			}
		})
	}
}

// serveInProcessAttempt sends body to route, the publish route or the send route, and returns its
// answer.
func serveInProcessAttempt(d *listenerDeps, route, body string) *httptest.ResponseRecorder {
	handler := publishHandler(d, logging.New("test"))
	if route == "/v1/messages/send" {
		handler = sendHandler(d, logging.New("test"))
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}
