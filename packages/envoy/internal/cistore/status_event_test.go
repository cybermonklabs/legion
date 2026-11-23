package cistore_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

// The listener drops each commit status webhook: a signed `status` delivery through the real
// webhook handler, wired to the real CI store and summary loop, is answered 200, records nothing in
// the CI record bucket and publishes nothing on NATS. A status event carries a commit and no pull
// request, so the listener cannot tell which head's record it belongs to; Dispatch's CI reconcile
// reads each open head's statuses from GitHub and records them instead (cistore.Writer.RecordStatus).
// A pull_request delivery after it is published, so the subscription that saw nothing of the
// status was listening.
func TestTheListenerDropsAStatusEvent(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store := openListener(t, client.Conn)
	const secret = "s"
	handler := webhook.GitHubHandler(secret, "@legion", "", client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 50*time.Millisecond, 10*time.Millisecond,
		metrics.New().NewGauge("held", ""), logging.New("loop"))
	sub, err := client.Conn.SubscribeSync("notifications.>")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	post := func(event, delivery, body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s delivery answered %d, want 200; body=%s", event, rr.Code, rr.Body.String())
		}
	}

	const sha = "0123456789abcdef0123456789abcdef01234567"
	// Shaped from GitHub's documented status payload.
	post("status", "d-status", `{
		"id": 4021,
		"sha": "`+sha+`",
		"name": "acme/widgets",
		"target_url": "https://ci.example.com/acme/widgets/4021",
		"context": "ci/preview",
		"description": "The preview build failed",
		"state": "failure",
		"commit": {"sha": "`+sha+`"},
		"branches": [{"name": "feature", "commit": {"sha": "`+sha+`"}}],
		"created_at": "2026-11-17T05:00:00Z",
		"updated_at": "2026-11-17T05:00:00Z",
		"sender": {"login": "preview-bot[bot]", "type": "Bot"},
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
	}`)
	// Several of the summary loop's ticks and its debounce pass with nothing to settle.
	time.Sleep(300 * time.Millisecond)
	post("pull_request", "d-pull-request", `{
		"action": "opened", "number": 7,
		"pull_request": {"head": {"sha": "`+sha+`"}, "updated_at": "2026-11-17T05:01:00Z"},
		"repository": {"name": "widgets", "owner": {"login": "acme"}}
	}`)

	seen := 0
	for {
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if errors.Is(err, natsgo.ErrTimeout) {
			break
		}
		if err != nil {
			t.Fatalf("read the bus: %v", err)
		}
		var env contracts.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			t.Fatalf("a message on %s is no envelope: %v", msg.Subject, err)
		}
		if env.SourceEventID != "d-pull-request" {
			t.Fatalf("the bus carried %s from delivery %q, want nothing but the pull request's envelopes", msg.Subject, env.SourceEventID)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("the bus carried nothing of the pull_request delivery either, so the subscription saw nothing")
	}

	bucket, err := client.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	if keys, err := bucket.Keys(); !errors.Is(err, natsgo.ErrNoKeysFound) {
		t.Fatalf("the CI bucket holds %v (%v) after a status delivery, want no record", keys, err)
	}
}
