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
	"github.com/sjawhar/envoy/internal/testwait"
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

// M4: a role claim through a replica whose relay run for the session had ended for want of an
// admission. The claim records the admission and renews the entry, but the entry's change is a
// relayCheck, which starts no ended run (sessionRelays.apply), so without a resume the session's
// messages wait until its next registration. The run is made to end as a replica's restart ends
// it: the admission is forgotten and the relays lose and regain the durable binding, so the run
// started again finds no admission. The refresh then sends the resume a registration sends
// (api.go, the roleSetHandler write), which starts the ended run. Controls: the relay delivers the
// session's first message before the admission is lost, and, when the resume is missing, a
// registration after the probe's window delivers the message the probe waited for.
func TestAnEdgeRoleClaimResumesARelayRunThatEndedForWantOfAnAdmission(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const sessionID, role = "ses_claim_resume", "legion-priv-priv-31-tester"
	const held = "edge relay holds the session's messages in its consumer until the session registers or claims a role through this listener"
	const stopped = "session relay stopped"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the session's registration")
	first, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), first)

	count := func(level, prefix string) int {
		n := 0
		for _, line := range edgeLogLines(rig.harness.logs.String(), level, prefix) {
			if line["session_id"] == sessionID {
				n++
			}
		}
		return n
	}

	rig.deps.consumers.admissions.prune(time.Now().Add(time.Hour), func(string) bool { return true })
	rig.relays.setOwned(false)
	testwait.Eventually(t, "the session's relay to stop", func() bool { return count("INFO", stopped)+count("WARN", stopped) > 0 })
	rig.relays.setOwned(true)
	testwait.Eventually(t, "the session's relay to end for want of an admission", func() bool { return count("INFO", held) > 0 })

	claim := rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role))
	requireStatus(t, claim, http.StatusOK, "the claim of "+role)
	if _, admitted := rig.deps.consumers.admissions.of(sessionID); !admitted {
		t.Fatalf("UNKNOWN: the claim recorded no admission, so the test does not reach the ended run")
	}
	second, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	if relayFrameArrives(standIn, second, 30*time.Second) {
		t.Logf("after the claim: the session's message %s reached it", second.EventID)
		return
	}
	t.Errorf("after a role claim through the replica, the session's message %s reached it in no 30 s: the ended run was not resumed", second.EventID)
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the control registration")
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), second)
	t.Logf("CONTROL: the registration after the window delivered %s", second.EventID)
}
