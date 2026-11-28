package cistore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

const (
	// The verdict App's id, another App's, and GitHub Actions' App's, as GitHub's JSON carries them.
	verdictApp = 5341240
	otherApp   = 5341243
	actionsApp = 15368

	verdictSecret = "s"
	verdictHead   = "abcdef1234567890abcdef1234567890abcdef12"

	// verdictSubject carries acme/widgets#42's settlements, which each listener subscribes to.
	verdictSubject = "notifications.github.acme.widgets.pr.42.checks"
	// verdictMarker is what settledByDebounce publishes on verdictSubject once the passes it waited
	// for have ended.
	verdictMarker = "marker: published once the summary passes ended"
)

// verdictDeployment is a NATS server of the test's own, whose client owns the stream, the
// listeners on it, and the clock their CI stores and summary loops run on: a fake that stands until
// the test moves it. Each listener's loop runs at cmd/listener's debounce and tick (loopDebounce,
// loopTick) on that fake. Two listeners on one deployment are one deployment before and after the
// setting changed, on one clock.
type verdictDeployment struct {
	t         *testing.T
	client    *bus.Client
	fake      *clocktest.Fake
	listeners []*verdictListener
}

// newVerdictDeployment is a deployment with no listener yet. Its clock stands at a whole second,
// so the millisecond a record's last event is stamped at and the instants of a loop's grid are
// moments of the fake.
func newVerdictDeployment(t *testing.T) *verdictDeployment {
	t.Helper()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(client.Close)
	return &verdictDeployment{t: t, client: client, fake: clocktest.New(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC))}
}

// verdictListener is the listener's real webhook handler over its real CI store and summary loop,
// configured with a verdict App id (ENVOY_VERDICT_APP_ID) or none, with a subscription to
// acme/widgets#42's settlements and the standard logger's lines. Its store, and so its loop, run on
// its deployment's fake clock.
type verdictListener struct {
	t          *testing.T
	deployment *verdictDeployment
	handler    func(event, body string)
	store      *cistore.Store
	// passes counts the passes its summary loop ended.
	passes *summaryPasses
	// messages is what the subscription receives.
	messages chan *nats.Msg
	logged   *logtest.Buffer
}

// newVerdictListener is a listener configured with the verdict App, on a deployment of its own.
func newVerdictListener(t *testing.T) *verdictListener {
	t.Helper()
	return verdictListenerOn(newVerdictDeployment(t), fmt.Sprint(verdictApp))
}

// summaryPasses counts the passes a summary loop ended (cistore.SummaryObserver), so a test that
// moved the loop's clock waits for the passes the move fired to end.
type summaryPasses struct {
	mu    sync.Mutex
	ended int
}

func (p *summaryPasses) SummaryScheduled(string, cistore.State, string) {}

func (p *summaryPasses) SummaryPassEnded() {
	p.mu.Lock()
	p.ended++
	p.mu.Unlock()
}

func (p *summaryPasses) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended
}

// verdictListenerOn is a listener on d whose ENVOY_VERDICT_APP_ID is verdictAppID, "" for unset, as
// cmd/listener builds one: the setting goes to the webhook handler.
func verdictListenerOn(d *verdictDeployment, verdictAppID string) *verdictListener {
	t := d.t
	t.Helper()
	// The standard logger is written by goroutines this test does not end, an earlier test's
	// watcher reporting a repair among them, while the test reads its lines: the buffer locks.
	logged := &logtest.Buffer{}
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })
	passes := &summaryPasses{}
	store := openListener(t, d.client.Conn, cistore.WithClock(d.fake), cistore.WithSummaryObserver(passes))
	loop, endLoop := context.WithCancel(t.Context())
	t.Cleanup(endLoop)
	cistore.StartSummaryLoop(loop, store, d.client, loopDebounce, loopTick, metrics.New().NewGauge("held", ""), logging.New("e2e"))
	messages := make(chan *nats.Msg, 64)
	sub, err := d.client.Conn.ChanSubscribe(verdictSubject, messages)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	handler := webhook.GitHubHandler(verdictSecret, "@legion", "", verdictAppID, d.client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	deliveries := 0
	l := &verdictListener{t: t, deployment: d, store: store, passes: passes, messages: messages, logged: logged, handler: func(event, body string) {
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
	d.listeners = append(d.listeners, l)
	return l
}

// passesEnded is how many passes the summary loops of d's listeners have ended.
func (d *verdictDeployment) passesEnded() int {
	ended := 0
	for _, l := range d.listeners {
		ended += l.passes.count()
	}
	return ended
}

// pastDebounce is the head's debounce and a tick past the fake's present, where the head's last
// write was stamped, since the fake stands until the test moves it: a loop settles a ready head at
// the first instant of its grid past the debounce, which is no later than that.
func (d *verdictDeployment) pastDebounce() time.Time {
	return d.fake.Now().Add(loopDebounce + loopTick)
}

// cached waits until the cache of each of d's listeners holds the head's record as the bucket
// holds it (awaitCachedAsStored), so each loop's change feed names the head's last write before the
// fake moves: a pass that ran before its cache had the write would judge the record the write
// replaced.
func (d *verdictDeployment) cached() {
	d.t.Helper()
	key := cistore.Key("acme", "widgets", "42", verdictHead)
	for _, l := range d.listeners {
		awaitCachedAsStored(d.t, l.store, d.client.Conn, key)
	}
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

// suiteWithoutApp delivers a check suite of the head in action and status that names no App, as
// a record written before the record carried a suite's App holds one.
func (l *verdictListener) suiteWithoutApp(id int, action, status, conclusion string) {
	l.t.Helper()
	l.handler("check_suite", fmt.Sprintf(`{
		"action": %q,
		"check_suite": {"id": %d, "status": %q, "conclusion": %q, "updated_at": "2026-11-20T03:00:00Z",
			"head_sha": %q, "latest_check_runs_count": 0, "pull_requests": [{"number": 42}]},
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
	}`, action, id, status, conclusion, verdictHead))
}

// recorded writes a check suite of the head in status, created by app ("" for none), to the CI
// store, as a listener before the webhook route stopped taking a requested check_suite delivery
// (contracts.GithubCIObservations) recorded one. No delivery records a suite before it completes
// now, so a queued suite is in a record only that way.
func (l *verdictListener) recorded(id int, status, app string) {
	l.t.Helper()
	if err := l.store.RecordSuite(l.t.Context(), contracts.CIObservation{
		Owner: "acme", Repo: "widgets", Number: "42", SHA: verdictHead,
		AppID: app, SuiteID: fmt.Sprint(id), Status: status, ObservedAt: "2026-11-20T02:00:00Z",
	}); err != nil {
		l.t.Fatalf("record suite %d: %v", id, err)
	}
}

// A queued suite whose record names no App is no suite of the verdict App's: with
// ENVOY_VERDICT_APP_ID set it still holds the head, as cistore's suiteHolds holds any queued suite
// of no App, and the head settles once the suite's completed delivery, which the listener does
// not leave out, arrives. Such a suite is in a record a listener wrote before the record carried
// a suite's App (recorded). The test reads the settlement it then requires, which shows the
// subscription would have seen one.
func TestASuiteWithNoAppIDStillHoldsTheHead(t *testing.T) {
	l := newVerdictListener(t)
	l.recorded(820, "queued", "")
	l.suite(900, "completed", "completed", "success", actionsApp)
	l.run(1, "build", "success", actionsApp)
	if settled, ok := l.settledByDebounce(); ok {
		t.Fatalf("the head settled as %+v while a suite naming no App was queued, want it held", settled)
	}
	if strings.Contains(l.logged.String(), "ci: left out the verdict App's own check suite") {
		t.Fatalf("the listener left a suite naming no App out:\n%s", l.logged.String())
	}
	l.suiteWithoutApp(820, "completed", "completed", "success")
	l.settlement("a settlement once the suite naming no App completed")
}

// settledByDebounce reports the settlement a summary loop of the deployment published on what its
// listeners have recorded, and false when none did. It moves the fake to each moment a loop armed,
// up to the head's debounce and a tick past its last write (pastDebounce), and then there, once
// every cache holds that write (cached); after each move it waits for every pass the move fired to
// end. While the head is held, each loop's one timer, at the next instant of its grid, is all
// there is armed; a ready head would be attempted at the first instant past its debounce. Then it
// publishes verdictMarker on the subscription's subject over the connection the loops publish on,
// and reads the subscription's first message: NATS hands a subscriber one connection's messages in
// the order they were published, so a settlement any of those passes published comes before it.
func (l *verdictListener) settledByDebounce() (cistore.Summary, bool) {
	t, d := l.t, l.deployment
	t.Helper()
	d.cached()
	until := d.pastDebounce()
	for there := false; !there && len(l.messages) == 0; {
		ended := d.passesEnded()
		var at time.Time
		fired := 0
		d.fake.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			if len(armed) == 0 || armed[0].After(until) {
				there = true
				return until, until.After(now)
			}
			at = armed[0]
			for _, deadline := range armed {
				if deadline.Equal(at) {
					fired++
				}
			}
			return at, true
		})
		if fired == 0 {
			continue
		}
		testwait.Until(t, fmt.Sprintf("the %d summary passes the move to %s fired to end", fired, at.Format(time.RFC3339Nano)), func() (string, bool) {
			if len(l.messages) > 0 {
				// A pass published: the read below reports what.
				return "a message reached the subscription", true
			}
			got := d.passesEnded() - ended
			return fmt.Sprintf("%d of them ended", got), got >= fired
		})
	}
	if err := d.client.Conn.Publish(verdictSubject, []byte(verdictMarker)); err != nil {
		t.Fatalf("publish the marker: %v", err)
	}
	msg := testwait.Receive(t, "the marker published once the summary passes ended", l.messages)
	if string(msg.Data) == verdictMarker {
		return cistore.Summary{}, false
	}
	return l.decode(msg), true
}

// settlement waits for the head's next settlement through the shared wait (testwait.Until), whose
// give-up names what, and returns it. Between two reads it moves the fake to the earliest moment a
// loop armed, up to the head's debounce and a tick past its last write (pastDebounce): a loop
// settles a ready head at the first instant of its grid past the debounce, so one that has not by
// then does not on that clock.
func (l *verdictListener) settlement(what string) cistore.Summary {
	t, d := l.t, l.deployment
	t.Helper()
	until := d.pastDebounce()
	var msg *nats.Msg
	testwait.Until(t, what, func() (string, bool) {
		select {
		case msg = <-l.messages:
			return "a settlement", true
		default:
		}
		armed, _ := d.fake.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			if len(armed) == 0 || armed[0].After(until) {
				return now, false
			}
			return armed[0], true
		})
		return fmt.Sprintf("no settlement with the fake at %s, timers armed at %v, moved up to %s",
			d.fake.Now().Format(time.RFC3339Nano), armed, until.Format(time.RFC3339Nano)), false
	})
	return l.decode(msg)
}

// decode is the settlement msg carries, failing the test when it carries none.
func (l *verdictListener) decode(msg *nats.Msg) cistore.Summary {
	l.t.Helper()
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		l.t.Fatalf("checks envelope not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		l.t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
	}
	return sum
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
			settled := l.settlement("a settlement with the verdict App's queued suite at the head")
			if settled.SHA != verdictHead || settled.Passed.Count != 1 || settled.Failed.Count != 0 {
				t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
			}
		})
	}
}

// GitHub Actions' queued suite is a CI result still to come, so it holds the head from settling
// with ENVOY_VERDICT_APP_ID set, as it did before the verdict App's suites were left out: the
// listener leaves out that App's suites alone. Another App's requested suite is no CI result and
// holds nothing (cistore's suiteHolds), and the listener does not take it for the verdict App's.
// The test then completes Actions' suite and requires the settlement, which shows the
// subscription would have seen one.
func TestAnotherAppsIncompleteSuiteHoldsSettlement(t *testing.T) {
	l := newVerdictListener(t)
	l.recorded(810, "queued", fmt.Sprint(actionsApp))
	l.suite(811, "requested", "queued", "", otherApp)
	l.suite(900, "completed", "completed", "success", actionsApp)
	l.run(1, "build", "success", actionsApp)
	if settled, ok := l.settledByDebounce(); ok {
		t.Fatalf("the head settled as %+v while GitHub Actions' suite was queued, want it held", settled)
	}
	if strings.Contains(l.logged.String(), "ci: left out the verdict App's own check suite") {
		t.Fatalf("the listener left another App's suite out:\n%s", l.logged.String())
	}
	l.suite(810, "completed", "completed", "success", actionsApp)
	l.settlement("a settlement once GitHub Actions' suite completed")
}

// A listener without ENVOY_VERDICT_APP_ID recorded the verdict App's queued suite like any other
// (recorded). Once the setting names that App, the suite's later deliveries are left out, so the
// record keeps it queued for good; it holds no head, since the CI store holds a head only on a
// queued suite of GitHub Actions or of no App (cistore's suiteHolds), and the head settles on its
// CI.
func TestAVerdictAppSuiteRecordedBeforeTheSettingDoesNotHoldTheHead(t *testing.T) {
	d := newVerdictDeployment(t)
	before := verdictListenerOn(d, "")
	before.recorded(800, "queued", fmt.Sprint(verdictApp))

	after := verdictListenerOn(d, fmt.Sprint(verdictApp))
	after.suite(800, "completed", "completed", "success", verdictApp)
	if !strings.Contains(after.logged.String(), "ci: left out the verdict App's own check suite suite=800") {
		t.Fatalf("the listener with the setting logged\n%s\nwant the verdict App's completed suite left out", after.logged.String())
	}
	after.suite(900, "completed", "completed", "success", actionsApp)
	after.run(1, "build", "success", actionsApp)
	settled := after.settlement("a settlement, which the verdict App's suite recorded before the setting must not hold")
	if settled.SHA != verdictHead || settled.Passed.Count != 1 || settled.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
}

// GitHub Actions' queued suite recorded before the setting still holds the head once the setting
// names the verdict App, and the head settles once that suite completes.
func TestAnotherAppsSuiteRecordedBeforeTheSettingStillHoldsTheHead(t *testing.T) {
	d := newVerdictDeployment(t)
	before := verdictListenerOn(d, "")
	before.recorded(810, "queued", fmt.Sprint(actionsApp))

	after := verdictListenerOn(d, fmt.Sprint(verdictApp))
	after.suite(900, "completed", "completed", "success", actionsApp)
	after.run(1, "build", "success", actionsApp)
	if settled, ok := after.settledByDebounce(); ok {
		t.Fatalf("the head settled as %+v while GitHub Actions' suite recorded before the setting was queued, want it held", settled)
	}
	after.suite(810, "completed", "completed", "success", actionsApp)
	after.settlement("a settlement once GitHub Actions' suite completed")
}
