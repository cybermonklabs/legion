package cistore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// The verdict App's id, another App's, and GitHub Actions' App's, as GitHub's JSON carries them.
	verdictApp = 5341240
	otherApp   = 5341243
	actionsApp = 15368

	verdictSecret = "s"
	verdictHead   = "abcdef1234567890abcdef1234567890abcdef12"
)

// verdictListener is the listener's real webhook handler over its real CI store and summary loop,
// configured with a verdict App id (ENVOY_VERDICT_APP_ID) or none, with a subscription to
// acme/widgets#42's settlements and the standard logger's lines.
type verdictListener struct {
	t       *testing.T
	handler func(event, body string)
	sub     *nats.Subscription
	logged  *bytes.Buffer
}

// newVerdictListener is a listener configured with the verdict App, on a NATS server of its own.
func newVerdictListener(t *testing.T) *verdictListener {
	t.Helper()
	return verdictListenerOn(t, verdictBus(t), fmt.Sprint(verdictApp))
}

// verdictBus is a bus client of a NATS server of the test's own, which owns the stream.
func verdictBus(t *testing.T) *bus.Client {
	t.Helper()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// verdictListenerOn is a listener on client whose ENVOY_VERDICT_APP_ID is verdictAppID, "" for
// unset, as cmd/listener builds one: the setting goes to the webhook handler and to the CI store.
// Two of them on one client are one deployment before and after the setting changed.
func verdictListenerOn(t *testing.T, client *bus.Client, verdictAppID string) *verdictListener {
	t.Helper()
	var logged bytes.Buffer
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })
	store, err := cistore.Open(t.Context(), client.Conn, logging.New("test"),
		cistore.WithReplicas(1), cistore.WithTTL(time.Hour), cistore.WithVerdictApp(verdictAppID))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	ready, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(ready); err != nil {
		t.Fatalf("cache ready: %v", err)
	}
	loop, endLoop := context.WithCancel(t.Context())
	t.Cleanup(endLoop)
	cistore.StartSummaryLoop(loop, store, client, 100*time.Millisecond, 20*time.Millisecond, metrics.New().NewGauge("held", ""), logging.New("e2e"))
	sub, err := client.Conn.SubscribeSync("notifications.github.acme.widgets.pr.42.checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	handler := webhook.GitHubHandler(verdictSecret, "@legion", "", verdictAppID, client, store)
	deliveries := 0
	return &verdictListener{t: t, sub: sub, logged: &logged, handler: func(event, body string) {
		t.Helper()
		deliveries++
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("d-verdict-%s-%d", verdictAppID, deliveries))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(verdictSecret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}}
}

// run delivers a completed check run of name on the head, created by app.
func (l *verdictListener) run(id int, name, conclusion string, app int) {
	l.t.Helper()
	l.handler("check_run", fmt.Sprintf(`{
		"action": "completed",
		"check_run": {"id": %d, "name": %q, "status": "completed", "conclusion": %q, "completed_at": "2026-11-20T03:00:00Z",
			"head_sha": %q, "app": {"id": %d}, "check_suite": {"id": 900}, "pull_requests": [{"number": 42}]},
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
	}`, id, name, conclusion, verdictHead, app))
}

// suite delivers a check suite of the head in action and status, created by app.
func (l *verdictListener) suite(id int, action, status, conclusion string, app int) {
	l.t.Helper()
	l.handler("check_suite", fmt.Sprintf(`{
		"action": %q,
		"check_suite": {"id": %d, "status": %q, "conclusion": %q, "updated_at": "2026-11-20T03:00:00Z",
			"head_sha": %q, "app": {"id": %d}, "latest_check_runs_count": 0, "pull_requests": [{"number": 42}]},
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
	}`, action, id, status, conclusion, verdictHead, app))
}

// settlement is the head's next settlement within wait, and false when none arrives.
func (l *verdictListener) settlement(wait time.Duration) (cistore.Summary, bool) {
	l.t.Helper()
	msg, err := l.sub.NextMsg(wait)
	if err != nil {
		return cistore.Summary{}, false
	}
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		l.t.Fatalf("checks envelope not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		l.t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
	}
	return sum, true
}

// GitHub makes an empty check suite for the verdict App on each push, queued until the App writes
// a run in it. The listener leaves it out of the CI record in each action GitHub sends it in, and
// logs each one it leaves out, so a head whose other suites completed settles on its CI.
func TestVerdictAppSuiteLeavesTheHeadToSettle(t *testing.T) {
	for _, action := range []string{"requested", "rerequested", "completed"} {
		t.Run(action, func(t *testing.T) {
			l := newVerdictListener(t)
			l.suite(800, action, "queued", "", verdictApp)
			if want := fmt.Sprintf("ci: left out the verdict App's own check suite suite=800 app_id=%d action=%s repo=acme/widgets sha=%s pr=42", verdictApp, action, verdictHead); !strings.Contains(l.logged.String(), want) {
				t.Fatalf("the listener logged\n%s\nwant the line %q", l.logged.String(), want)
			}
			l.suite(900, "completed", "completed", "success", actionsApp)
			l.run(1, "build", "success", actionsApp)
			settled, ok := l.settlement(3 * time.Second)
			if !ok {
				t.Fatal("no settlement was published with the verdict App's queued suite at the head")
			}
			if settled.SHA != verdictHead || settled.Passed.Count != 1 || settled.Failed.Count != 0 {
				t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
			}
		})
	}
}

// Another App's queued suite is a CI result still to come, so it holds the head from settling, as
// it did before the verdict App's suites were left out. The test then completes that suite and
// requires the settlement, which shows the subscription would have seen one.
func TestAnotherAppsIncompleteSuiteHoldsSettlement(t *testing.T) {
	l := newVerdictListener(t)
	l.suite(810, "requested", "queued", "", otherApp)
	l.suite(900, "completed", "completed", "success", actionsApp)
	l.run(1, "build", "success", actionsApp)
	if settled, ok := l.settlement(2 * time.Second); ok {
		t.Fatalf("the head settled as %+v while App %d's suite was queued, want it held", settled, otherApp)
	}
	if strings.Contains(l.logged.String(), "ci: left out the verdict App's own check suite") {
		t.Fatalf("the listener left another App's suite out:\n%s", l.logged.String())
	}
	l.suite(810, "completed", "completed", "success", otherApp)
	if _, ok := l.settlement(3 * time.Second); !ok {
		t.Fatal("no settlement was published once App 5341243's suite completed")
	}
}

// A listener without ENVOY_VERDICT_APP_ID records the verdict App's queued suite like any other.
// Once the setting names that App, the suite's later deliveries are left out, so the record keeps
// it queued for good: the CI store disregards the verdict App's stored suites when it decides
// whether a head settles, and the head settles on its CI.
func TestAVerdictAppSuiteRecordedBeforeTheSettingDoesNotHoldTheHead(t *testing.T) {
	client := verdictBus(t)
	before := verdictListenerOn(t, client, "")
	before.suite(800, "requested", "queued", "", verdictApp)

	after := verdictListenerOn(t, client, fmt.Sprint(verdictApp))
	after.suite(800, "completed", "completed", "success", verdictApp)
	if !strings.Contains(after.logged.String(), "ci: left out the verdict App's own check suite suite=800") {
		t.Fatalf("the listener with the setting logged\n%s\nwant the verdict App's completed suite left out", after.logged.String())
	}
	after.suite(900, "completed", "completed", "success", actionsApp)
	after.run(1, "build", "success", actionsApp)
	settled, ok := after.settlement(3 * time.Second)
	if !ok {
		t.Fatal("no settlement was published: the verdict App's suite recorded before the setting holds the head")
	}
	if settled.SHA != verdictHead || settled.Passed.Count != 1 || settled.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
}

// Another App's queued suite recorded before the setting still holds the head once the setting
// names the verdict App, and the head settles once that suite completes.
func TestAnotherAppsSuiteRecordedBeforeTheSettingStillHoldsTheHead(t *testing.T) {
	client := verdictBus(t)
	before := verdictListenerOn(t, client, "")
	before.suite(810, "requested", "queued", "", otherApp)

	after := verdictListenerOn(t, client, fmt.Sprint(verdictApp))
	after.suite(900, "completed", "completed", "success", actionsApp)
	after.run(1, "build", "success", actionsApp)
	if settled, ok := after.settlement(2 * time.Second); ok {
		t.Fatalf("the head settled as %+v while App %d's suite recorded before the setting was queued, want it held", settled, otherApp)
	}
	after.suite(810, "completed", "completed", "success", otherApp)
	if _, ok := after.settlement(3 * time.Second); !ok {
		t.Fatal("no settlement was published once App 5341243's suite completed")
	}
}
