package cistore_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// loopDebounce and loopTick are the summary loop's debounce and tick as cmd/listener runs it:
	// ENVOY_CI_DEBOUNCE's default and the loop's one-second tick.
	loopDebounce = 5 * time.Second
	loopTick     = time.Second
)

// loopClock is the fake clock a row's CI store, the writers beside it and its summary loop run on
// (cistore.WithClock), and the passes that loop ended (summaryPasses, for
// cistore.WithSummaryObserver). It stands at start, a whole second, until the row moves it, so each
// record written before the row moves it is stamped at start, where the loop's grid starts too.
type loopClock struct {
	fake   *clocktest.Fake
	start  time.Time
	passes *summaryPasses
}

func newLoopClock() *loopClock {
	start := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	return &loopClock{fake: clocktest.New(start), start: start, passes: &summaryPasses{}}
}

// settle moves the clock as production's grid runs over a head whose records were all written at
// start. Once store's cache holds key's record as the CI bucket does (awaitCachedAsStored), so the
// loop's change feed has named the record's last write, it moves to the grid's first instant,
// whose pass finds the head inside its debounce, and then to the instant the debounce runs out,
// which that pass arms; the pass there publishes the head's settlement. Each move waits until the
// loop has armed the instant it moves to, so a loop that never wakes for the debounce fails naming
// that instant.
func (c *loopClock) settle(t *testing.T, store *cistore.Store, conn *natsgo.Conn, key string) {
	t.Helper()
	awaitCachedAsStored(t, store, conn, key)
	first := c.start.Add(loopTick)
	c.fake.AwaitArmedAt(t, "the summary loop's first pass", first)
	c.fake.Set(first)
	due := c.start.Add(loopDebounce)
	c.fake.AwaitArmedAt(t, "the summary loop's pass at the head's debounce, which its first pass arms", due)
	c.fake.Set(due)
}

// runTo moves the clock to each instant the summary loop arms, earliest first, up to until, and
// then to until, and after each move that reached an armed instant waits for the pass it fired to
// end (summaryPasses): each pass the moves ran has ended, and published what it publishes, when it
// returns. While no record is being written the loop's timer is the one armed on the clock.
func (c *loopClock) runTo(t *testing.T, until time.Time) {
	t.Helper()
	for {
		ended := c.passes.count()
		var at time.Time
		c.fake.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			if len(armed) == 0 || armed[0].After(until) {
				return until, until.After(now)
			}
			at = armed[0]
			return at, true
		})
		if at.IsZero() {
			return
		}
		testwait.Until(t, "the summary pass the move to "+at.Format(time.RFC3339Nano)+" fired to end", func() (int, bool) {
			got := c.passes.count() - ended
			return got, got >= 1
		})
	}
}

// ciBucket is the CI record bucket on conn through the jetstream package, whose calls take the
// test's deadline as their context (testwait.Context): nats.go's older KeyValue, Get and Keys take
// none, and end at a timeout of their own.
func ciBucket(t *testing.T, conn *natsgo.Conn) jetstream.KeyValue {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(testwait.Context(t), bus.CIStateBucket)
	if err != nil {
		t.Fatalf("bind the CI bucket: %v", err)
	}
	return kv
}

// awaitCachedAsStored waits until store's cache holds key's record as the CI bucket holds it, so
// the summary loop's change feed has named the record's last write before the row moves the loop's
// clock: a pass that ran before the cache had that write would judge the record it replaced.
func awaitCachedAsStored(t *testing.T, store *cistore.Store, conn *natsgo.Conn, key string) {
	t.Helper()
	entry, err := ciBucket(t, conn).Get(testwait.Context(t), key)
	if err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	var stored cistore.State
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("decode the record of %s: %v", key, err)
	}
	testwait.Until(t, fmt.Sprintf("the cache to hold %s at generation %d, snapshot %s, last event %d", key, stored.Generation, stored.Hash(), stored.LastEventAt), func() (string, bool) {
		for _, st := range store.List() {
			if cistore.Key(st.Owner, st.Repo, st.Number, st.SHA) == key {
				return fmt.Sprintf("generation %d, snapshot %s, last event %d", st.Generation, st.Hash(), st.LastEventAt),
					st.Generation == stored.Generation && st.Hash() == stored.Hash() && st.LastEventAt == stored.LastEventAt
			}
		}
		return "no record of " + key, false
	})
}

// nextMsg waits for sub's next message until the test binary's deadline less testwait's margin
// (testwait.Context), and fails the test naming what, the message it waited for, when none comes.
func nextMsg(t *testing.T, sub *natsgo.Subscription, what string) *natsgo.Msg {
	t.Helper()
	msg, err := sub.NextMsgWithContext(testwait.Context(t))
	if err != nil {
		t.Fatalf("gave up waiting for %s: %v", what, err)
	}
	return msg
}

// TestEndToEndCheckRunToChecks drives the listener's real webhook, CI store,
// and NATS loop. CI webhooks publish nothing raw; one settled checks envelope
// is emitted for the PR after the quiet period. The store and its loop run at
// cmd/listener's debounce and tick on a fake clock the test moves (loopClock).
func TestEndToEndCheckRunToChecks(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	c := newLoopClock()
	store, err := cistore.Open(t.Context(), client.Conn, logging.New("test"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour), cistore.WithClock(c.fake))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	if err := store.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("cache ready: %v", err)
	}

	const (
		secret = "s"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	handler := webhook.GitHubHandler(secret, "@legion", "", "", client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, loopDebounce, loopTick, metrics.New().NewGauge("held", ""), logging.New("e2e"))

	sub, err := client.Conn.SubscribeSync("notifications.github.example-org.example-repo.pr.42.checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	postEvent := func(event, body, delivery string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}
	postEvent("pull_request", fmt.Sprintf(`{
		"action": "opened", "number": 42,
		"pull_request": {"head": {"sha": %q}, "updated_at": "2026-09-07T03:00:00Z"},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, sha), "d0")

	post := func(name, status, conclusion, delivery string) {
		t.Helper()
		body := fmt.Sprintf(`{
			"action": "completed",
			"check_run": {"id": 1, "name": %q, "status": %q, "conclusion": %q, "completed_at": "2026-09-07T03:01:00Z", "head_sha": %q,
				"pull_requests": [{"number": 42}]},
			"sender": {"login": "ci", "type": "Bot"},
			"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
		}`, name, status, conclusion, sha)
		postEvent("check_run", body, delivery)
	}

	post("build", "completed", "success", "d1")
	post("lint", "completed", "failure", "d2")
	c.settle(t, store, client.Conn, cistore.Key("example-org", "example-repo", "42", sha))
	msg := nextMsg(t, sub, "the settled checks envelope of pull request 42")
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("checks envelope not JSON: %v", err)
	}
	if env.Topic != "notifications.github.example-org.example-repo.pr.42.checks" {
		t.Fatalf("topic = %q", env.Topic)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
	}
	if sum.Kind != "checks" || sum.Passed.Count != 1 || sum.Failed.Count != 1 {
		t.Fatalf("unexpected checks summary: %+v", sum)
	}
}

// A check run GitHub names no pull request for still belongs to one: its check suite's head
// branch is the ref that pull request's head is fetched by. The listener records it under that
// number, and the settlement it publishes for the number names the failed check among its
// failing checks. The store and its loop run at cmd/listener's debounce and tick on a fake clock
// the test moves (loopClock).
func TestEndToEndACheckRunWithNoPullRequestIsAttributedByItsSuiteHeadBranch(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	c := newLoopClock()
	store, err := cistore.Open(t.Context(), client.Conn, logging.New("test"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour), cistore.WithClock(c.fake))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	if err := store.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("cache ready: %v", err)
	}

	const (
		secret = "s"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	handler := webhook.GitHubHandler(secret, "@legion", "", "", client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, loopDebounce, loopTick, metrics.New().NewGauge("held", ""), logging.New("e2e"))

	sub, err := client.Conn.SubscribeSync("notifications.github.example-org.example-repo.pr.42.checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	postEvent := func(event, body, delivery string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}
	postEvent("pull_request", fmt.Sprintf(`{
		"action": "opened", "number": 42,
		"pull_request": {"head": {"sha": %q}, "updated_at": "2026-09-07T03:00:00Z"},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, sha), "d0")
	postEvent("check_run", fmt.Sprintf(`{
		"action": "completed",
		"check_run": {"id": 126934155799, "name": "ci-ok", "status": "completed", "conclusion": "failure",
			"completed_at": "2026-09-07T03:01:00Z", "head_sha": %q,
			"check_suite": {"id": 900, "head_branch": "refs/pull/42/head"},
			"pull_requests": []},
		"sender": {"login": "ci", "type": "Bot"},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}, "default_branch": "main"}
	}`, sha), "d1")

	c.settle(t, store, client.Conn, cistore.Key("example-org", "example-repo", "42", sha))
	msg := nextMsg(t, sub, "the settlement of the pull request the head branch names")
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("checks envelope not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
	}
	if sum.Number != "42" || sum.SHA != sha || sum.Failed.Count != 1 ||
		len(sum.Failed.Checks) != 1 || sum.Failed.Checks[0] != "ci-ok" ||
		len(sum.FailingChecks) != 1 || sum.FailingChecks[0].Name != "ci-ok" {
		t.Fatalf("settlement = %+v, want pull request 42 at %s failing ci-ok", sum, sha)
	}

	// The settlement is published from the record, so by now the record of 42 at that head holds
	// the check the webhook named no pull request for.
	var state cistore.State
	for _, held := range store.List() {
		if held.Number == "42" && held.SHA == sha {
			state = held
		}
	}
	if check, ok := state.Checks["ci-ok"]; !ok || check.Conclusion != "failure" || check.Status != "completed" {
		t.Fatalf("the record of 42 at %s holds checks %+v, want ci-ok failed", sha, state.Checks)
	}
}

// A handoff push can carry GitHub's skip-checks trailer, so the pull request's new head runs no CI,
// and the code head it replaced settles afterwards. That settlement is published, carrying the
// code head's SHA, though the code head is no longer the pull request's head. The store and its
// loop run at cmd/listener's debounce and tick on a fake clock the test moves (loopClock).
func TestEndToEndACodeHeadSettlesAfterASkippedHandoffHead(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	c := newLoopClock()
	store, err := cistore.Open(t.Context(), client.Conn, logging.New("test"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour), cistore.WithClock(c.fake))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	if err := store.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("cache ready: %v", err)
	}
	const (
		secret  = "s"
		code    = "c0de000000000000000000000000000000000000"
		handoff = "4a4d0ff000000000000000000000000000000000"
	)
	handler := webhook.GitHubHandler(secret, "@legion", "", "", client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, loopDebounce, loopTick, metrics.New().NewGauge("held", ""), logging.New("e2e"))
	sub, err := client.Conn.SubscribeSync("notifications.github.example-org.example-repo.pr.42.checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	postEvent := func(event, body, delivery string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}
	pullRequest := func(action, head, updatedAt, delivery string) {
		t.Helper()
		postEvent("pull_request", fmt.Sprintf(`{
			"action": %q, "number": 42,
			"pull_request": {"head": {"sha": %q}, "updated_at": %q},
			"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
		}`, action, head, updatedAt), delivery)
	}
	pullRequest("opened", code, "2026-09-07T03:00:00Z", "d0")
	postEvent("check_run", fmt.Sprintf(`{
		"action": "created",
		"check_run": {"id": 1, "name": "build", "status": "in_progress", "head_sha": %q, "pull_requests": [{"number": 42}]},
		"sender": {"login": "ci", "type": "Bot"},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, code), "d1")
	// The handoff head replaces the code head while its run is still going, and starts none.
	pullRequest("synchronize", handoff, "2026-09-07T03:00:30Z", "d2")
	postEvent("check_run", fmt.Sprintf(`{
		"action": "completed",
		"check_run": {"id": 1, "name": "build", "status": "completed", "conclusion": "success", "completed_at": "2026-09-07T03:01:00Z",
			"head_sha": %q, "pull_requests": [{"number": 42}]},
		"sender": {"login": "ci", "type": "Bot"},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, code), "d3")
	c.settle(t, store, client.Conn, cistore.Key("example-org", "example-repo", "42", code))
	msg := nextMsg(t, sub, "the code head's settlement")
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("checks envelope not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
	}
	if sum.SHA != code || sum.Passed.Count != 1 || sum.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want the code head %s passing", sum, code)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
