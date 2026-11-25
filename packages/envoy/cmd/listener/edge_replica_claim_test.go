package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// edgeProcessCall sends a /v1 request to the listener process edge under token, signed by caller
// as an agent's client signs one, and returns the status and body it answered.
func edgeProcessCall(t *testing.T, caller *ciphertokentest.Caller, edge *listenerProcess, token, method, path, body string) (int, string) {
	t.Helper()
	origin := "http://127.0.0.1:" + strconv.Itoa(edge.port)
	signed := caller.Signed(t, method, origin+path, token)
	request, err := http.NewRequest(method, origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(answer)
}

// Two edge replicas, A and B, serve PRIV beside a shared listener, which takes the role lane. An
// edge session registers through A, and A relays its agent messages. The session then claims a
// role of PRIV through B: the claim renews the session's record with B's machine, which moves the
// session's relay from A to B, and B relays the session on the projects of the claim's own token,
// so its next agent message and a message to its role, which the shared listener forwards to its
// agent subject, both reach it before it registers again. The controls: the session's agent
// message reaches it through A before the claim, and a claim through B of a role of another
// project is refused and moves nothing.
func TestARoleClaimOnAnotherEdgeReplicaKeepsDelivery(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	shared := startListenerProcess(t, binary, uri, "replica-claim-shared")
	shared.waitHealthy(t)
	shared.WaitForOutput(t, `"msg":"role lane subscribed"`)
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	a := startEdgeListenerProcess(t, binary, uri, "replica-claim-edge-a", edgeProject, broker.Server.URL, edgeAccountToken)
	b := startEdgeListenerProcess(t, binary, uri, "replica-claim-edge-b", edgeProject, broker.Server.URL, edgeAccountToken)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the shared listener's output:\n%s", shared.Output.String())
			t.Logf("replica A's output:\n%s", a.Output.String())
			t.Logf("replica B's output:\n%s", b.Output.String())
		}
	})
	a.waitHealthy(t)
	b.waitHealthy(t)

	const sessionID, role = "ses_replica_claim", "PRIV.replica-reviewer"
	token := "access-token-of-" + sessionID
	answer := sessionAnswer(sessionID, "outside@example.com", caller.JKT())
	answer.Projects = []string{edgeProject}
	broker.Says(token, answer)
	standIn := startSessionStandIn(t, uri, sessionID, answerDone)
	publisher := connectPublisher(t, uri)
	agentSubject := contracts.LegacyAgentSubject(sessionID)

	if status, body := edgeProcessCall(t, caller, a, token, http.MethodPost, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1")); status != http.StatusOK {
		t.Fatalf("the session's registration through A: status %d %s", status, body)
	}
	before := newEnvelope("agent", agentSubject, "before the claim")
	publish(t, publisher, before)
	requireFrames(t, standIn.collect(t, 1, 20*time.Second), before)

	refused := fmt.Sprintf(`{"session_id":%q,"role":"INFRA.replica-reviewer"}`, sessionID)
	if status, body := edgeProcessCall(t, caller, b, token, http.MethodPost, "/v1/roles/set", refused); status != http.StatusForbidden || !strings.Contains(body, edgeRoleNotInProject) {
		t.Fatalf("the session's claim of an INFRA role through B: status %d %s, want 403 %s", status, body, edgeRoleNotInProject)
	}
	claim := fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role)
	if status, body := edgeProcessCall(t, caller, b, token, http.MethodPost, "/v1/roles/set", claim); status != http.StatusOK {
		t.Fatalf("the session's claim of %s through B: status %d %s", role, status, body)
	}
	// A gives the session up once its copy of the record names B, so a message stored from here
	// on is B's to relay or nobody's.
	a.WaitForOutput(t, `"reason":"session_not_ours"`)

	after := newEnvelope("agent", agentSubject, "after the claim")
	publish(t, publisher, after)
	// The claim moved the relay from A to B. When A drops the session (session_not_ours) it gives
	// the server back the frames it still held unacked, so B may deliver the pre-claim "before"
	// frame once more: an at-least-once redelivery across the relay handoff, which the client
	// dedupes, not a lost or cross-project frame. The meaning here is that the "after" frame reaches
	// the session; a redelivered "before" is allowed, nothing else is.
	requireEventuallyDelivered(t, standIn, after, 20*time.Second, before)

	const summary = "to the role claimed through B"
	roleMessage := newEnvelope("agent", contracts.RoleTopicPrefix+role, summary)
	publish(t, publisher, roleMessage)
	forwarded := requireEventuallyDelivered(t, standIn, roleMessage, 20*time.Second, before, after).item
	if forwarded.PayloadSummary != summary || forwarded.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the session was relayed %q on %s, want %q on %s", forwarded.PayloadSummary, forwarded.Topic, summary, contracts.RoleTopicPrefix+role)
	}
}
