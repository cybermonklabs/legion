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

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// subjectsWithout is every subject this build carries except those that capture subject: the
// stream a newer build leaves that dropped it.
func subjectsWithout(t *testing.T, subject string) []string {
	t.Helper()
	var without []string
	for _, carried := range bus.StreamCarriedSubjects() {
		if !bus.SubjectContains(carried, subject) {
			without = append(without, carried)
		}
	}
	if len(without) == len(bus.StreamCarriedSubjects()) {
		t.Fatalf("no subject this build carries captures %s", subject)
	}
	return without
}

// Each route of the listener whose answer a stored publish decides answers a publish the
// notifications stream did not take because, as the listener read it, the deployed stream
// captures no subject of the message's (bus.ErrStreamLacksSubject) at once: the 503 with
// Retry-After: 1 whose body opens with streamLacksSubjectPrefix and names the subject and the
// stream's generation, recorded stream_lacks_subject, with nothing stored, before the route's own
// window for that publish ends. One row per route, each with that window:
//
//   - the send route, over a stream that dropped the agent subject, inside the send's budget
//     (sendRequestBudget);
//   - the publish route's role send to a live holder, whose forward goes to the holder's agent
//     subject: one forward, not the forty a send makes over 4 s for a forward the stream did not
//     acknowledge, since every attempt would meet the same refusal, inside the role send's window
//     (publishDeliveryWindow);
//   - the publish route's role send to a role nobody claims, whose keep goes to the role's kept
//     lane, over a stream that dropped that lane: not the 404 of a role nothing was kept for,
//     since the keep failed for want of the subject and the sender learns that nothing stored the
//     message and why, inside the keep's window (roleKeepPublishWindow);
//   - the send route's send marked with a role whose claim names another session than the
//     target, or none (answerRoleSend), which keeps the message on the role's kept lane over a
//     stream that dropped that lane, for the same reason and inside the same window;
//   - the role claim route's claim that takes a role from another session (roleSetHandler), which
//     writes a takeover record on the role's takeover lane before the claim
//     (takeoverTries.beforeTry), over a stream that dropped that lane: the route's own 503 with
//     Retry-After: 1 for a record that did not store (errTakeoverUnrecorded), whose words carry the
//     bus's ErrStreamLacksSubject with the subject and the generation, with no claim written and no
//     record stored, inside the claim's budget (sendRequestBudget);
//   - the publish route's topic publish, whose topic the stream leaves out: refused 400 before
//     anything is published (ReachesNoSession), as it was, since no session's consumer would hold
//     it.
//
// The webhook routes' rows are internal/webhook's
// (TestAWebhookDeliveryTheStreamLacksTheSubjectOfIsAnswered503NamingIt). The routes whose lanes
// keep core NATS by design publish nothing stored, so no row of theirs is here: their answers are
// TestEveryCoreLaneDeliversWhateverTheDeployedStreamCaptures's.
func TestEveryRouteThatEndsInAStoredPublishAnswersAStreamThatLacksTheSubjectAtOnce(t *testing.T) {
	type answer struct {
		status     int
		retryAfter string
		body       string
		record     map[string]any
		elapsed    time.Duration
	}
	serve := func(t *testing.T, build func(*logging.Logger) http.HandlerFunc, request *http.Request) answer {
		t.Helper()
		started := time.Now()
		recorder, record := serveRecorded(t, build, request)
		var refusal listenerwire.APIError
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		a := answer{status: recorder.Code, retryAfter: recorder.Header().Get("Retry-After"), body: refusal.Error, record: record, elapsed: time.Since(started)}
		line, _ := json.Marshal(record)
		t.Logf("status %d, Retry-After %q, after %s, body %s, record %s", a.status, a.retryAfter, a.elapsed, strings.TrimSpace(recorder.Body.String()), line)
		return a
	}
	// lacks asserts the answer of a stored publish the stream lacks subject of, made inside
	// window, the route's own bound for that publish: an answer that comes only once window has
	// passed is one the route made after it waited, or tried again, for the whole of it.
	lacks := func(t *testing.T, a answer, subject string, window time.Duration) {
		t.Helper()
		if a.status != http.StatusServiceUnavailable || a.retryAfter != "1" {
			t.Errorf("status %d, Retry-After %q; want 503 and 1", a.status, a.retryAfter)
		}
		if !strings.HasPrefix(a.body, streamLacksSubjectPrefix+": ") || !strings.Contains(a.body, fmt.Sprintf("%q", subject)) || !strings.Contains(a.body, "generation 1000000") {
			t.Errorf("the refusal reads %q, want it to open with %q and name %s and generation 1000000", a.body, streamLacksSubjectPrefix, subject)
		}
		if a.record["outcome"] != string(outcomeStreamLacksSubject) || a.record["status"] != float64(http.StatusServiceUnavailable) {
			t.Errorf("the record's outcome is %v with status %v, want %s and 503", a.record["outcome"], a.record["status"], outcomeStreamLacksSubject)
		}
		if a.elapsed >= window {
			t.Errorf("the refusal was answered after %s, the route's whole window of %s for it", a.elapsed, window)
		}
	}
	// connect deploys a newer build's stream of subjects on a server of the row's own and
	// connects the listener's client to it, which leaves that stream as it is.
	connect := func(t *testing.T, subjects []string) (*bus.Client, string) {
		t.Helper()
		url := testnats.URL(t)
		deployNewerBuildsStreamOf(t, url, subjects)
		client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect the listener: %v", err)
		}
		t.Cleanup(client.Close)
		return client, url
	}

	t.Run("the send route", func(t *testing.T) {
		const target = "ses_lacks_route_send"
		subject := contracts.LegacyAgentSubject(target)
		client, url := connect(t, subjectsWithout(t, subject))
		rig := newKnownSessionRigOn(t, client, url)
		if err := rig.deps.sessions.Put(target, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Delivery: session.DeliveryConsumer}); err != nil {
			t.Fatalf("write the session entry of %s: %v", target, err)
		}
		a := serve(t, func(logger *logging.Logger) http.HandlerFunc { return sendHandler(rig.deps, logger) },
			postJSON("/v1/messages/send", sendBody(map[string]any{"target_session": target, "source": "agent", "source_session": "ses_lacks_route_sender", "message": "are you there?"})))
		lacks(t, a, subject, sendRequestBudget)
		if stored := storedOnStream(t, client, bus.Stream, subject); stored != 0 {
			t.Errorf("the stream holds %d copies on %s, want none", stored, subject)
		}
	})

	// publishRig is a publish route over client whose role forwards are counted.
	publishRig := func(t *testing.T, client *bus.Client) (*listenerDeps, listenerDeliveryHarness, *atomic.Int32) {
		t.Helper()
		harness := newListenerDeliveryHarnessOn(t, client)
		forwards := &atomic.Int32{}
		config := harness.config
		forward := config.forwardRole
		config.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
			forwards.Add(1)
			return forward(subject, item, msgID, timeout)
		}
		deps := harness.publishDeps(t)
		deps.delivery = &config
		return deps, harness, forwards
	}
	rolePublish := func(role string) *http.Request {
		return postJSON("/v1/messages/publish", fmt.Sprintf(`{"topic":%q,"message":"role event","source":"agent"}`, contracts.RoleTopicPrefix+role))
	}

	t.Run("the publish route's role send to a live holder", func(t *testing.T) {
		const (
			role   = "lacks-route-forward"
			holder = "ses_lacks_route_holder"
		)
		subject := contracts.LegacyAgentSubject(holder)
		client, _ := connect(t, subjectsWithout(t, subject))
		deps, harness, forwards := publishRig(t, client)
		if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
			t.Fatalf("register the holder: %v", err)
		}
		claim := httptest.NewRecorder()
		roleSetHandler(&listenerDeps{registry: harness.registry, sessions: harness.sessions, delivery: &harness.config}, "test-machine", logging.New("test")).ServeHTTP(claim,
			postJSON("/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)))
		if claim.Code != http.StatusOK {
			t.Fatalf("claim %s for %s: status %d, body %s", role, holder, claim.Code, claim.Body.String())
		}
		a := serve(t, func(logger *logging.Logger) http.HandlerFunc { return publishHandler(deps, logger) }, rolePublish(role))
		lacks(t, a, subject, publishDeliveryWindow)
		if got := forwards.Load(); got != 1 {
			t.Errorf("the send made %d forwards, want 1: a forward to a subject the stream does not capture is refused the same way every time", got)
		}
		if stored := storedOnStream(t, client, bus.Stream, subject); stored != 0 {
			t.Errorf("the stream holds %d copies on %s, want none", stored, subject)
		}
	})

	t.Run("the publish route's role send kept for a role nobody claims", func(t *testing.T) {
		const role = "lacks-route-keep"
		subject := bus.KeptRoleSubject(role)
		client, _ := connect(t, subjectsWithout(t, subject))
		deps, _, forwards := publishRig(t, client)
		a := serve(t, func(logger *logging.Logger) http.HandlerFunc { return publishHandler(deps, logger) }, rolePublish(role))
		lacks(t, a, subject, roleKeepPublishWindow)
		if got := forwards.Load(); got != 0 {
			t.Errorf("the send made %d forwards to a role nobody claims, want none", got)
		}
		if stored := storedOnStream(t, client, bus.Stream, subject); stored != 0 {
			t.Errorf("the stream holds %d copies on %s, want none", stored, subject)
		}
	})

	t.Run("the send route's role-marked send kept for a role nobody claims", func(t *testing.T) {
		const (
			role      = "lacks-route-marked-keep"
			target    = "ses_lacks_route_marked_target"
			principal = "dispatch@lacks-route.example"
		)
		subject := bus.KeptRoleSubject(role)
		client, _ := connect(t, subjectsWithout(t, subject))
		deps, _, forwards := publishRig(t, client)
		deps.dispatchPrincipal = principal
		request := postJSON("/v1/messages/send", fmt.Sprintf(`{"target_session":%q,"source":"dispatch","message":"a role event","role":%q}`, target, role))
		request = request.WithContext(withVerifiedPrincipal(request.Context(), principal))
		a := serve(t, func(logger *logging.Logger) http.HandlerFunc { return sendHandler(deps, logger) }, request)
		lacks(t, a, subject, roleKeepPublishWindow)
		if got := forwards.Load(); got != 0 {
			t.Errorf("the send made %d forwards for a role nobody claims, want none", got)
		}
		if stored := storedOnStream(t, client, bus.Stream, subject); stored != 0 {
			t.Errorf("the stream holds %d copies on %s, want none", stored, subject)
		}
	})

	t.Run("the role claim route's claim that takes a role from another session", func(t *testing.T) {
		const (
			role     = "lacks-route-takeover"
			previous = "ses_lacks_route_previous"
			claimant = "ses_lacks_route_claimant"
		)
		lane := bus.TakeoverSubject(role, previous, "")
		client, url := connect(t, subjectsWithout(t, bus.TakeoverSubject(role, previous, "attempt")))
		rig := newSessionConsumerRigOn(t, client, url)
		rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
		rig.subscribe(t, consumerRegistration(claimant, "epoch-1"))
		grantRoleClaim(t, rig.mux, previous, role, "", false)
		started := time.Now()
		recorder := postRoleClaim(rig.mux, claimant, role, "", false)
		elapsed := time.Since(started)
		var refusal listenerwire.APIError
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		retryAfter := recorder.Header().Get("Retry-After")
		t.Logf("status %d, Retry-After %q, after %s, body %s", recorder.Code, retryAfter, elapsed, strings.TrimSpace(recorder.Body.String()))
		if recorder.Code != http.StatusServiceUnavailable || retryAfter != "1" {
			t.Errorf("status %d, Retry-After %q; want 503 and 1", recorder.Code, retryAfter)
		}
		if !strings.HasPrefix(refusal.Error, errTakeoverUnrecorded.Error()) || !strings.Contains(refusal.Error, bus.ErrStreamLacksSubject.Error()) ||
			!strings.Contains(refusal.Error, `"`+lane) || !strings.Contains(refusal.Error, "generation 1000000") {
			t.Errorf("the refusal reads %q, want the claim's words for a takeover record that did not store, naming a subject on %s and generation 1000000 through %q", refusal.Error, lane, bus.ErrStreamLacksSubject)
		}
		if elapsed >= sendRequestBudget {
			t.Errorf("the refusal was answered after %s, the route's whole window of %s for it", elapsed, sendRequestBudget)
		}
		if holder, err := rig.harness.registry.RoleHolder(role); err != nil || holder != previous {
			t.Errorf("role %s is held by %q (%v), want %s still: the refused claim writes nothing", role, holder, err, previous)
		}
		if records, err := client.TakeoverRecords(context.Background(), role); err != nil || len(records) != 0 {
			t.Errorf("the stream holds %d takeover records of %s (%v), want none", len(records), role, err)
		}
	})

	t.Run("the publish route's topic publish, refused before the publish", func(t *testing.T) {
		const topic = "notifications.github.acme.widgets.push.branch.main"
		client, _ := connect(t, subjectsWithout(t, topic))
		deps, _, _ := publishRig(t, client)
		a := serve(t, func(logger *logging.Logger) http.HandlerFunc { return publishHandler(deps, logger) },
			postJSON("/v1/messages/publish", fmt.Sprintf(`{"topic":%q,"message":"a push","source":"agent"}`, topic)))
		if a.status != http.StatusBadRequest || !strings.Contains(a.body, "no stream subject captures "+topic) || a.record["outcome"] != string(outcomeInvalidRequest) {
			t.Errorf("status %d, body %q, outcome %v; want the 400 that names the topic no stream subject captures, recorded invalid_request", a.status, a.body, a.record["outcome"])
		}
		if stored := storedOnStream(t, client, bus.Stream, topic); stored != 0 {
			t.Errorf("the stream holds %d copies on %s, want none", stored, topic)
		}
	})
}
