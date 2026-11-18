package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// webhookSecret signs every delivery these tests send.
	webhookSecret = "frozen-bus-secret"
	// slackWithin is the window Slack waits for an answer before it takes an event as failed and
	// retries it: the Slack route's own answer budget must fall inside it.
	slackWithin = 3 * time.Second
)

// frozenWebhookBus is a NATS server of its own, which the test can freeze, and the listener's bus
// client on it.
type frozenWebhookBus struct {
	server *testnats.Server
	client *bus.Client
}

func newFrozenWebhookBus(t *testing.T, opts ...bus.ConnectOption) *frozenWebhookBus {
	t.Helper()
	server, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, append([]bus.ConnectOption{bus.WithReplicas(1)}, opts...)...)
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	return &frozenWebhookBus{server: server, client: client}
}

// freeze stops the server's traffic in both directions while every connection stays up, until the
// test ends.
func (b *frozenWebhookBus) freeze(t *testing.T) {
	t.Helper()
	t.Cleanup(b.server.Freeze(t))
}

// serveDelivery serves req on the test's own goroutine and gives the handler's answer. It is the
// control path, where the bus answers and no route spends a budget.
func serveDelivery(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// serveDeliveryOnGoroutine serves req on a goroutine, so a test may drive a fake clock while the
// handler is in flight, and returns the recorder with a channel closed once the handler answered.
func serveDeliveryOnGoroutine(handler http.Handler, req *http.Request) (*httptest.ResponseRecorder, <-chan struct{}) {
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, req)
	}()
	return recorder, done
}

func githubDelivery(event, delivery, body string) *http.Request {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func slackDelivery(body string) *http.Request {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte("v0:" + timestamp + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/slack", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func ghostWisprDelivery(delivery, body string) *http.Request {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/ghostwispr", strings.NewReader(body))
	req.Header.Set("X-GhostWispr-Delivery", delivery)
	req.Header.Set("X-GhostWispr-Event", "summary_ready")
	req.Header.Set("X-GhostWispr-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

// wantDeliveryServed asserts the route's 200 "ok".
func wantDeliveryServed(t *testing.T, route string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("%s with the bus answering: status %d, body %q; want 200 ok", route, recorder.Code, recorder.Body.String())
	}
}

// wantDeliveryFailed asserts the answer a delivery the listener could not record or publish has
// always had (webhook's deliveryFailed): 503 "service unavailable".
func wantDeliveryFailed(t *testing.T, route string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != "service unavailable\n" {
		t.Fatalf("%s with the bus silent: status %d, body %q; want 503 service unavailable", route, recorder.Code, recorder.Body.String())
	}
	t.Logf("%s with the bus silent: 503 once its budget was spent", route)
}

// A check_suite or check_run delivery writes its commit's CI record, and nats.go's KV reads and
// writes take no context and wait out the JetStream MaxWait (10 s), as long as GitHub's own wait
// for an answer. On a bus that went silent without closing the connection the delivery must still
// get the answer a record that failed always got, 503, once the CI store's record wait is spent.
// That wait runs on a fake clock here, so the delivery ends when this test moves the clock past
// cistore.RecordWait and not when the machine's clock reaches it. The negative control is a
// delivery recorded on the same store before the freeze.
func TestAGitHubCIDeliveryAnswersAFrozenBusInsideTheRecordWait(t *testing.T) {
	frozen := newFrozenWebhookBus(t)
	// The CI store's record wait runs on a fake clock this test moves (cistore.WithClock).
	recordClock := clocktest.New(time.Now())
	store, err := cistore.Open(frozen.client.Conn, logging.New("test"), cistore.WithClock(recordClock), cistore.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the CI store: %v", err)
	}
	t.Cleanup(store.StopWatch)
	if err := store.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for the CI cache: %v", err)
	}
	handler := webhook.GitHubHandler(webhookSecret, "@legion", "", frozen.client, store)
	checkSuite := func(number string) string {
		return `{"action":"completed","check_suite":{"id":900,"status":"completed","conclusion":"success",` +
			`"updated_at":"2026-10-26T03:01:00Z","head_sha":"abcdef1234567890abcdef1234567890abcdef12","app":{"id":77},` +
			`"pull_requests":[{"number":` + number + `}]},"repository":{"name":"example-repo","owner":{"login":"example-org"}}}`
	}
	checkRun := `{"action":"completed","check_run":{"id":987654321,"name":"unit-tests","status":"completed","conclusion":"failure",` +
		`"completed_at":"2026-10-26T03:00:00Z","head_sha":"abcdef1234567890abcdef1234567890abcdef12","check_suite":{"id":900},` +
		`"pull_requests":[{"number":43}]},"repository":{"name":"example-repo","owner":{"login":"example-org"}}}`

	recorder := serveDelivery(handler, githubDelivery("check_suite", "delivery-control", checkSuite("41")))
	wantDeliveryServed(t, "a check_suite delivery", recorder)

	frozen.freeze(t)
	for _, delivery := range []struct{ event, body string }{
		{"check_suite", checkSuite("42")},
		{"check_run", checkRun},
	} {
		recorder, done := serveDeliveryOnGoroutine(handler, githubDelivery(delivery.event, "delivery-frozen-"+delivery.event, delivery.body))
		awaitArmedBeforeReturn(t, recordClock, "a "+delivery.event+" delivery's CI record wait on the frozen bus", timersArmed(1), closeReturned(done, recorderAnswer(recorder)))
		recordClock.Advance(cistore.RecordWait)
		testwait.Receive(t, "a "+delivery.event+" delivery's answer once the record wait was spent", done)
		wantDeliveryFailed(t, "a "+delivery.event+" delivery", recorder)
	}
}

// A webhook's publish waits for the bus within the request: Slack's within the 3 s Slack waits for
// an answer, Ghost Wispr's and a GitHub event's until their caller drops the request. Each used
// the bus's own 5 s bound alone, so a bus that went silent was answered after Slack had given up,
// and after the others' callers had gone. Each must still get the answer a failed publish always
// got, 503. The negative control is each route served on the same bus before the freeze.
func TestTheWebhookPublishesAnswerAFrozenBusInsideTheirCallersWait(t *testing.T) {
	// The bus client's own publish bound runs on a fake clock this test never moves, so a route
	// that ignored its caller's dropped request and waited the bus's bound would hang on a timer
	// that never fires rather than answer late. The bound is armed on the client's clock for a core
	// publish and on its acknowledgement clock for a JetStream one (bus.PublishBound), so busClock
	// is both. The Slack route's own answer budget runs on a fake of its own, which the test fires.
	busClock := clocktest.New(time.Now())
	frozen := newFrozenWebhookBus(t, bus.WithClock(busClock), bus.WithPublishAcknowledgementClock(busClock))
	// publishBounds reads the deadlines armed on busClock and how many of them stand at the moment
	// a publish arms its bound for. The client arms its keeper's period and its refresh of the
	// stream's subjects on busClock from its connect, at other moments, so only a publish's bound
	// stands there.
	publishBounds := func() ([]time.Time, int) {
		armed, bound, at := busClock.Armed(), busClock.Now().Add(bus.PublishBound), 0
		for _, deadline := range armed {
			if deadline.Equal(bound) {
				at++
			}
		}
		return armed, at
	}
	slackClock := clocktest.New(time.Now())
	slack := webhook.SlackHandler(webhookSecret, frozen.client, webhook.WithClock(slackClock))
	ghostWispr := webhook.GhostWisprHandler(webhookSecret, frozen.client)
	github := webhook.GitHubHandler(webhookSecret, "@legion", "", frozen.client, noCI{})
	slackEvent := func(eventID string) string {
		return `{"type":"event_callback","event_id":"` + eventID + `","team_id":"T01234567",` +
			`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"hello world","ts":"1700000000.000100"}}`
	}
	summary := `{"event_type":"summary_ready","payload":{"session_id":"20261026041629","type":"summary_ready"}}`
	comment := func(id string) string {
		return `{"action":"created","repository":{"name":"widgets","owner":{"login":"acme"},"full_name":"acme/widgets"},` +
			`"issue":{"number":7,"title":"t","html_url":"https://github.com/acme/widgets/issues/7"},` +
			`"comment":{"id":` + id + `,"body":"no mention here","html_url":"https://github.com/acme/widgets/issues/7#issuecomment-` + id + `","user":{"login":"someone","type":"User"}},` +
			`"sender":{"login":"someone","type":"User"}}`
	}

	wantDeliveryServed(t, "a Slack event", serveDelivery(slack, slackDelivery(slackEvent("Ev_control"))))
	wantDeliveryServed(t, "a Ghost Wispr event", serveDelivery(ghostWispr, ghostWisprDelivery("gw-control", summary)))
	wantDeliveryServed(t, "a GitHub issue_comment", serveDelivery(github, githubDelivery("issue_comment", "delivery-comment-control", comment("101"))))

	frozen.freeze(t)
	t.Run("slack", func(t *testing.T) {
		recorder, done := serveDeliveryOnGoroutine(slack, slackDelivery(slackEvent("Ev_frozen")))
		armed := awaitArmedBeforeReturn(t, slackClock, "the Slack route's own answer budget on the frozen bus", timersArmed(1), closeReturned(done, recorderAnswer(recorder)))
		if d := armed[0].Sub(slackClock.Now()); d > slackWithin {
			t.Fatalf("the Slack route's answer budget is %s out, past the %s Slack waits before it takes an event as failed", d, slackWithin)
		}
		slackClock.Set(armed[0])
		testwait.Receive(t, "the Slack event's answer once its budget was spent", done)
		wantDeliveryFailed(t, "a Slack event", recorder)
	})
	for _, route := range []struct {
		name    string
		handler http.Handler
		req     *http.Request
	}{
		{"a Ghost Wispr event", ghostWispr, ghostWisprDelivery("gw-frozen", summary)},
		{"a GitHub issue_comment", github, githubDelivery("issue_comment", "delivery-comment-frozen", comment("102"))},
	} {
		t.Run(route.name, func(t *testing.T) {
			// The caller drops its request at the point this test chooses: once the route's publish
			// has armed its own bound on busClock, which this test never moves. The route must answer
			// then, not wait that bound.
			_, before := publishBounds()
			ctx, cancel := context.WithCancel(context.Background())
			recorder, done := serveDeliveryOnGoroutine(route.handler, route.req.WithContext(ctx))
			awaitArmedBeforeReturn(t, busClock, route.name+"'s publish to arm its bound on the frozen bus, "+bus.PublishBound.String()+" out", func([]time.Time) bool {
				_, at := publishBounds()
				return at > before
			}, closeReturned(done, recorderAnswer(recorder)))
			cancel()
			testwait.Receive(t, route.name+"'s answer once its caller gave up", done)
			wantDeliveryFailed(t, route.name, recorder)
		})
	}
}
