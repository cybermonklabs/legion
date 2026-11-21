package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
)

// edgeWithheldSeries is envoy_edge_exceptions_withheld_total's series for one topic prefix and
// reason, as /metrics renders it.
func edgeWithheldSeries(topicPrefix, reason string) string {
	return fmt.Sprintf(`envoy_edge_exceptions_withheld_total{topic_prefix=%q,reason=%q}`, topicPrefix, reason)
}

// registryBody is met's /metrics as an operator reads it.
func registryBody(t *testing.T, met *metrics.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return recorder.Body.String()
}

// testEdgeGrant is the grant of an edge listener of test-machine serving PRIV, counting on met.
func testEdgeGrant(t *testing.T, met *metrics.Registry) *edgeGrant {
	t.Helper()
	grant, err := newEdgeGrant("test-machine", []string{"PRIV"}, met)
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	return grant
}

// The start's grant refuses a project list natsgrant.Edge refuses, naming edge mode, and counts
// nothing on it.
func TestAnEdgeListenerGrantRefusesAListThatIsNoListOfProjectKeys(t *testing.T) {
	grant, err := newEdgeGrant("test-machine", []string{"*"}, metrics.New())
	if err == nil || grant != nil || !strings.Contains(err.Error(), "edge mode") {
		t.Fatalf("newEdgeGrant(test-machine, [*]) = %v, %v; want no grant and a refusal naming edge mode", grant, err)
	}
}

// An edge listener's grant publishes no kept role lane, so the edge keeps no role message and
// hands no kept lane over: a handover of a lane that holds a copy for a live holder forwards
// nothing and writes no cursor, the sweep ends before it starts, and a keep stores nothing. The
// same listener without the edge's grant hands the same copy over to the same holder and keeps the
// same message, which shows the rig sees a handover and a keep when one is made.
func TestAnEdgeListenerKeepsNoRoleMessageAndHandsNoKeptLaneOver(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "edge-kept-handover"
		other  = "edge-kept-keep"
		holder = "ses_edge_kept_handover"
	)
	shared := harness.config
	forwards := make(chan string, 16)
	shared.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ string, _ time.Duration) error {
		forwards <- item.DedupeKey
		return nil
	})
	edge := shared
	edge.edge = testEdgeGrant(t, harness.metrics)

	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "edge-kept-handover-"+id.New())
	if _, err := keepRoleMessage(shared, role, item); err != nil {
		t.Fatalf("keep the copy a shared listener keeps; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := drainKeptRole(ctx, edge, role, edge.now); err != nil {
		t.Fatalf("the edge's handover of role %s: %v", role, err)
	}
	select {
	case key := <-forwards:
		t.Fatalf("the edge handed over %s from role %s's kept lane, which its grant may not write a cursor of", key, role)
	default:
	}
	if cursor, err := harness.client.ReadKeptRoleCursor(ctx, role); err != nil || cursor.Sequence != 0 {
		t.Fatalf("role %s's drain cursor after the edge's handover is %+v (%v), want none written", role, cursor, err)
	}
	select {
	case <-startKeptRoleSweep(ctx, edge, time.Hour):
	default:
		t.Fatal("the edge started a sweep of the kept role lanes")
	}
	otherItem := listenerTestEnvelope(contracts.RoleTopicPrefix+other, "edge-kept-keep-"+id.New())
	if _, err := keepForNextHolder(edge, other, otherItem); !errors.Is(err, errEdgeKeepsNothing) {
		t.Fatalf("the edge's keep for role %s's next holder = %v, want errEdgeKeepsNothing", other, err)
	}
	assertNothingKept(t, harness.client, other)

	if err := drainKeptRole(ctx, shared, role, shared.now); err != nil {
		t.Fatalf("the shared listener's handover of role %s: %v", role, err)
	}
	select {
	case key := <-forwards:
		if key != roleForwardDedupePrefix+item.DedupeKey {
			t.Fatalf("the shared listener handed over %s, want the kept copy %s", key, roleForwardDedupePrefix+item.DedupeKey)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the shared listener's handover forwarded nothing, so the rig cannot see a handover:\n%s", harness.logs.String())
	}
	if cursor, err := harness.client.ReadKeptRoleCursor(ctx, role); err != nil || cursor.Sequence == 0 {
		t.Fatalf("role %s's drain cursor after the shared listener's handover is %+v (%v), want one written", role, cursor, err)
	}
	if _, err := keepForNextHolder(shared, other, otherItem); err != nil {
		t.Fatalf("the shared listener kept nothing for role %s (%v), so the rig cannot see a keep:\n%s", other, err, harness.logs.String())
	}
}

// At the edge a publish to a role no session holds is refused 404 and kept nowhere, since the
// edge's grant publishes no kept role lane. Its refusal's exception is published where the grant
// holds the lane, a role of the edge's own project, and withheld and counted where it does not, a
// role in the form that names no project.
func TestAnEdgePublishToAnUnheldRoleIsRefusedAndKeptNowhere(t *testing.T) {
	d, client := keptRoleDeps(t)
	met := metrics.New()
	d.delivery.edge = testEdgeGrant(t, met)
	const (
		project = "PRIV.edge-unheld"
		legacy  = "edge-unheld-legacy"
	)
	exceptions, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exception lanes: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}

	for _, role := range []string{legacy, project} {
		recorder, _ := publishThrough(d, `{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"nobody holds this"}`)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("the edge's publish to the unheld role %s: status %d, body %s; want 404, nothing kept", role, recorder.Code, recorder.Body.String())
		}
	}
	for _, role := range []string{legacy, "edge-unheld", project} {
		assertNothingKept(t, client, role)
	}
	if got := counterValue(t, registryBody(t, met), edgeWithheldSeries("notifications.role", "unclaimed")); got != 1 {
		t.Fatalf("the edge counted %d withheld role refusals, want 1: the refusal of %s", got, legacy)
	}
	message, err := exceptions.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("the refusal of %s raised no exception on its lane: %v", project, err)
	}
	var exception contracts.Envelope
	if err := json.Unmarshal(message.Data, &exception); err != nil {
		t.Fatalf("decode the refusal exception: %v", err)
	}
	if want := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + project; message.Subject != want {
		t.Fatalf("the first refusal exception was on %s, want %s: the edge published one on a lane its grant leaves out", message.Subject, want)
	}
}

// The edge publishes a delivery exception only on a lane its grant holds. A fan-out refusal on a
// followed GitHub topic or Dispatch event - a delivery mode no one can read, or one the session
// does not advertise - is withheld and counted, and the agent subject's delivery failure is
// published on its exception lane. The same GitHub refusal on a listener without the edge's grant
// is published, which shows the probe sees the lane: the probe reads the exceptions in the order
// the one connection published them, so the GitHub lane's would come first had the edge published
// it.
func TestAnEdgeListenerPublishesNoDeliveryExceptionItsGrantDoesNotHold(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	edge := harness.config
	edge.edge = testEdgeGrant(t, harness.metrics)
	exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exception lanes: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	const recipient = "ses_edge_refused"

	github := listenerTestEnvelope("notifications.github.acme.pub.pr.1.opened", "edge-github-"+id.New())
	applyDeliveryOutcome(edge, github, fanoutRefusalOutcome(recipient, github, followedTopicModeUnreadable, "", false))
	dispatch := listenerTestEnvelope("notifications.dispatch.issue.PRIV.PRIV-1.comment.created", "edge-dispatch-"+id.New())
	applyDeliveryOutcome(edge, dispatch, fanoutRefusalOutcome(recipient, dispatch, followedTopicModeNotAdvertised, "bogus", false))
	agent := listenerTestEnvelope(contracts.LegacyAgentSubject(recipient), "edge-agent-"+id.New())
	applyDeliveryOutcome(edge, agent, deliveryOutcome{sessionID: recipient, exceptionReason: "delivery_failed"})
	shared := listenerTestEnvelope("notifications.github.acme.pub.pr.2.opened", "shared-github-"+id.New())
	applyDeliveryOutcome(harness.config, shared, fanoutRefusalOutcome(recipient, shared, followedTopicModeUnreadable, "", false))

	for _, want := range []string{bus.ExceptionTopicPrefix + agent.Topic, bus.ExceptionTopicPrefix + shared.Topic} {
		message, err := exceptions.NextMsg(10 * time.Second)
		if err != nil {
			t.Fatalf("no exception on %s within 10s: %v\n%s", want, err, harness.logs.String())
		}
		if message.Subject != want {
			t.Fatalf("an exception on %s, want %s next: the edge published one on a lane its grant leaves out", message.Subject, want)
		}
	}
	body := metricsBody(t, harness)
	for _, series := range []string{edgeWithheldSeries("notifications.github", "delivery_failed"), edgeWithheldSeries("notifications.dispatch", "delivery_failed")} {
		if got := counterValue(t, body, series); got != 1 {
			t.Fatalf("%s = %d, want 1", series, got)
		}
	}
	if got := counterValue(t, body, edgeWithheldSeries("notifications.agent", "delivery_failed")); got != -1 {
		t.Fatalf("the edge counted the agent subject's exception withheld (%d), which its grant publishes", got)
	}
}
