package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// webhookSecret signs every delivery these tests send.
	webhookSecret = "frozen-bus-secret"
	// githubRecordWithin leaves time to return the response inside GitHub's 10 s window.
	githubRecordWithin = 5 * time.Second
	// Slack retries an event it has no answer to within 3 s.
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
	return &frozenWebhookBus{server: server, client: connectWebhookBus(t, uri, opts...)}
}

func connectWebhookBus(t *testing.T, uri string, opts ...bus.ConnectOption) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{uri}, append([]bus.ConnectOption{bus.WithReplicas(1)}, opts...)...)
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// freeze stops the server's traffic in both directions while every connection stays up, until the
// test ends.
func (b *frozenWebhookBus) freeze(t *testing.T) {
	t.Helper()
	t.Cleanup(b.server.Freeze(t))
}

func startWebhookDelivery(handler http.Handler, req *http.Request) <-chan *httptest.ResponseRecorder {
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		answered <- recorder
	}()
	return answered
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

// slackEvent builds a message with text already escaped for a JSON string.
func slackEvent(eventID, text string) string {
	return `{"type":"event_callback","event_id":"` + eventID + `","team_id":"T01234567",` +
		`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"` + text + `","ts":"1700000000.000100"}}`
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
	t.Logf("%s with the bus silent: 503 service unavailable", route)
}

// A check_suite or check_run delivery writes its commit's CI record, and nats.go's KV reads and
// writes take no context and wait out the JetStream MaxWait (10 s), as long as GitHub's own wait for
// an answer. On a bus that went silent without closing the connection the delivery must still get
// the answer a record that failed always got, 503, inside the CI store's 4 s record wait. The
// negative control is a delivery recorded on the same store before the freeze.
func TestAGitHubCIDeliveryAnswersAFrozenBusInsideTheRecordWait(t *testing.T) {
	t.Parallel()
	if wait := cistore.RecordWait; wait <= 0 || wait >= githubRecordWithin {
		t.Fatalf("CI store RecordWait = %s, want positive and below %s for a GitHub CI delivery", wait, githubRecordWithin)
	}
	clk := clocktest.New(time.Unix(0, 0))
	frozen := newFrozenWebhookBus(t)
	store, err := cistore.Open(testwait.Context(t), frozen.client.Conn, logging.New("test"), cistore.WithReplicas(1), cistore.WithClock(clk))
	if err != nil {
		t.Fatalf("open the CI store: %v", err)
	}
	t.Cleanup(store.StopWatch)
	if err := store.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for the CI cache: %v", err)
	}
	handler := webhook.GitHubHandler(webhookSecret, "@legion", "", "", frozen.client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	checkSuite := func(number string) string {
		return `{"action":"completed","check_suite":{"id":900,"status":"completed","conclusion":"success",` +
			`"updated_at":"2026-10-26T03:01:00Z","head_sha":"abcdef1234567890abcdef1234567890abcdef12","app":{"id":77},` +
			`"pull_requests":[{"number":` + number + `}]},"repository":{"name":"example-repo","owner":{"login":"example-org"}}}`
	}
	checkRun := `{"action":"completed","check_run":{"id":987654321,"name":"unit-tests","status":"completed","conclusion":"failure",` +
		`"completed_at":"2026-10-26T03:00:00Z","head_sha":"abcdef1234567890abcdef1234567890abcdef12","check_suite":{"id":900},` +
		`"pull_requests":[{"number":43}]},"repository":{"name":"example-repo","owner":{"login":"example-org"}}}`

	control := startWebhookDelivery(handler, githubDelivery("check_suite", "delivery-control", checkSuite("41")).WithContext(t.Context()))
	recorder := testwait.Receive(t, "the check_suite control delivery to be served", control)
	wantDeliveryServed(t, "a check_suite delivery", recorder)

	frozen.freeze(t)
	// The deliveries write different records (PRs 42 and 43). Hold the clock at their
	// common start until both have armed their own wait and sent their KV request.
	sentBefore := frozen.client.Conn.Stats().OutMsgs
	due := clk.Now().Add(cistore.RecordWait)
	deliveries := []struct {
		event    string
		answered <-chan *httptest.ResponseRecorder
	}{
		{"check_suite", startWebhookDelivery(handler, githubDelivery("check_suite", "delivery-frozen-check_suite", checkSuite("42")).WithContext(t.Context()))},
		{"check_run", startWebhookDelivery(handler, githubDelivery("check_run", "delivery-frozen-check_run", checkRun).WithContext(t.Context()))},
	}
	testwait.Until(t, "check_suite and check_run to arm RecordWait at "+due.String()+" and send their KV requests", func() (string, bool) {
		for _, delivery := range deliveries {
			select {
			case answer := <-delivery.answered:
				t.Fatalf("%s answered %d %q before its CI record wait expired", delivery.event, answer.Code, answer.Body.String())
			default:
			}
		}
		armed := clk.Armed()
		var recordWaits int
		for _, at := range armed {
			if at.Equal(due) {
				recordWaits++
			}
		}
		sent := frozen.client.Conn.Stats().OutMsgs - sentBefore
		return fmt.Sprintf("deadlines %v, NATS requests sent %d", armed, sent), recordWaits == len(deliveries) && sent >= uint64(len(deliveries))
	})
	clk.Set(due.Add(time.Nanosecond))
	for _, delivery := range deliveries {
		answer := testwait.Receive(t, delivery.event+" to answer after RecordWait", delivery.answered)
		wantDeliveryFailed(t, "a "+delivery.event+" delivery", answer)
	}
}

// observedWebhookPublish reports the context supplied by the handler and the result of the actual
// bus call. The 503 alone would not tell which context ended the publish.
type observedWebhookPublish struct {
	client  *bus.Client
	started chan context.Context
	result  chan error
}

func (p observedWebhookPublish) PublishEachWithin(ctx context.Context, item contracts.Envelope, published func(string, bool, bool)) error {
	p.started <- ctx
	err := p.client.PublishEachWithin(ctx, item, published)
	p.result <- err
	return err
}

// A webhook's publish waits for the bus within the request: Slack's within the 3 s Slack waits for
// an answer, Ghost Wispr's and a GitHub event's until their caller drops the request. Each used
// the bus's own 5 s bound alone, so a bus that went silent was answered after Slack had given up,
// and after the others' callers had gone. Each must still get the answer a failed publish always
// got, 503. The negative control is each route served on the same bus before the freeze.
func TestTheWebhookPublishesAnswerAFrozenBusInsideTheirCallersWait(t *testing.T) {
	t.Parallel()
	if slackRoom, slackAnswer := webhook.SlackRoomWait, webhook.SlackAnswerBudget; slackRoom <= 0 || slackRoom >= slackAnswer || slackAnswer >= slackWithin {
		t.Fatalf("Slack defaults: room wait %s, answer budget %s; want 0 < room wait < answer budget < Slack's %s window",
			slackRoom, slackAnswer, slackWithin)
	}

	// Do not advance the bus's fallback clock. The route's own budget or its request
	// cancellation must end the wait.
	ackClock := bus.WithPublishAcknowledgementClock(clocktest.New(time.Unix(0, 0)))
	frozen := newFrozenWebhookBus(t, ackClock)
	summary := `{"event_type":"summary_ready","payload":{"session_id":"20261026041629","type":"summary_ready"}}`
	comment := func(id string) string {
		return `{"action":"created","repository":{"name":"widgets","owner":{"login":"acme"},"full_name":"acme/widgets"},` +
			`"issue":{"number":7,"title":"t","html_url":"https://github.com/acme/widgets/issues/7"},` +
			`"comment":{"id":` + id + `,"body":"no mention here","html_url":"https://github.com/acme/widgets/issues/7#issuecomment-` + id + `","user":{"login":"someone","type":"User"}},` +
			`"sender":{"login":"someone","type":"User"}}`
	}

	type route struct {
		name, delivery  string
		client          *bus.Client
		handler         func(webhook.Publisher, ...webhook.HandlerOption) http.Handler
		control, frozen *http.Request
	}
	routes := []route{
		{
			name: "slack", delivery: "a Slack event", client: frozen.client,
			handler: func(pub webhook.Publisher, opts ...webhook.HandlerOption) http.Handler {
				return webhook.SlackHandler(webhookSecret, pub, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil), opts...)
			},
			control: slackDelivery(slackEvent("Ev_control", "hello world")),
			frozen:  slackDelivery(slackEvent("Ev_frozen", "hello world")),
		},
		{
			name: "a Ghost Wispr event", delivery: "a Ghost Wispr event", client: connectWebhookBus(t, frozen.server.URL(), ackClock),
			handler: func(pub webhook.Publisher, opts ...webhook.HandlerOption) http.Handler {
				return webhook.GhostWisprHandler(webhookSecret, pub, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil), opts...)
			},
			control: ghostWisprDelivery("gw-control", summary),
			frozen:  ghostWisprDelivery("gw-frozen", summary),
		},
		{
			name: "a GitHub issue_comment", delivery: "a GitHub issue_comment", client: connectWebhookBus(t, frozen.server.URL(), ackClock),
			handler: func(pub webhook.Publisher, _ ...webhook.HandlerOption) http.Handler {
				return webhook.GitHubHandler(webhookSecret, "@legion", "", "", pub, noCI{}, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
			},
			control: githubDelivery("issue_comment", "delivery-comment-control", comment("101")),
			frozen:  githubDelivery("issue_comment", "delivery-comment-frozen", comment("102")),
		},
	}
	for _, route := range routes {
		control := startWebhookDelivery(route.handler(route.client), route.control.WithContext(t.Context()))
		wantDeliveryServed(t, route.delivery, testwait.Receive(t, route.delivery+" control to be served", control))
	}

	frozen.freeze(t)
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pub := observedWebhookPublish{route.client, make(chan context.Context, 1), make(chan error, 1)}
			var opts []webhook.HandlerOption
			if route.name == "slack" {
				opts = append(opts, webhook.WithSlackAnswerBudget(25*time.Millisecond))
			}
			sentBefore := route.client.Conn.Stats().OutMsgs
			answered := startWebhookDelivery(route.handler(pub, opts...), route.frozen.WithContext(ctx))

			publishCtx := testwait.Receive(t, route.delivery+" to reach the frozen publisher", pub.started)
			wantErr := context.DeadlineExceeded
			if route.name == "slack" {
				if _, ok := publishCtx.Deadline(); !ok {
					t.Fatal("Slack publish context has no answer-budget deadline")
				}
			} else {
				// Each row owns a connection, so only this delivery can send a publish on it.
				testwait.Until(t, route.delivery+" to send its publish on the frozen connection", func() (uint64, bool) {
					sent := route.client.Conn.Stats().OutMsgs
					return sent, sent > sentBefore
				})
				cancel()
				wantErr = context.Canceled
				if err := publishCtx.Err(); err != wantErr {
					t.Fatalf("%s publish context after caller cancellation = %v, want %v", route.delivery, err, wantErr)
				}
			}
			if err := testwait.Receive(t, route.delivery+" publish to end", pub.result); !errors.Is(err, wantErr) {
				t.Fatalf("%s publish error = %v, want %v", route.delivery, err, wantErr)
			}
			wantDeliveryFailed(t, route.delivery, testwait.Receive(t, route.delivery+" to answer 503", answered))
		})
	}
}
