package api

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// sharedIssueExamples is where the shared issue event examples are: the envelopes the outbox's
// TestTheBuiltIssueEnvelopesAreTheSharedFixtures builds, which the daemon's intake and
// packages/contracts read.
const sharedIssueExamples = "../../../../daemon/internal/intake/testdata/dispatch/"

// The claim and release handlers write what the shared issue event examples say they write. The
// outbox's test builds each example's event from a hand-made model.Event, so a handler that started
// writing another reason, claim or actor would change no example and fail none of the tests that
// read them. Here each write the examples show runs through its real handler, under the credential
// the example's write was made with - a first claim and the holder's release under a token the
// secrets broker issued a session acting for its person, a forced takeover under a person's
// personal token, a takeover of a session that ended under a broker token an operator's entry
// scoped to the issue's project, a claim under the shared agent token every Dispatch takes, whose
// actor is the session its body names, and a person's close at a browser - and the event it records must
// carry its example's type, reason, claim and previous claim, as null or not, and notify mark, and
// an actor of the example's kind and proven mark with the example's keys at every depth. So a
// handler's reason renamed, or an actor written with a key more or less than its example's, fails
// here by name.
func TestTheClaimHandlersWriteWhatTheSharedIssueExamplesHold(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one", "session-two")
	rig := newBrokerTokenRigOn(t, testServerOptions{envoyURL: claimListener(t, live, nil).URL})
	handler := rig.handler
	key := claimIssueKey(t, handler, "todo")
	path := "/api/v1/issues/" + key
	// Each token the broker says it issued a session: one acting for its person, which names no
	// project (issue-claimed-first.json's and issue-released-null-claim.json's actor), and one an
	// operator's entry scoped to the issue's project (issue-claimed-every-field.json's).
	caller := newBrokerCaller(t)
	sessionToken := func(token, session, scope string, projects ...string) string {
		rig.broker.Says(token, brokertoken.Introspection{
			Active:       true,
			Session:      session,
			Person:       brokerTestPerson,
			ProjectScope: scope,
			Projects:     projects,
			Scope:        "dispatch.write",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
			ClientID:     brokertoken.ExchangeClientID,
			Thumbprint:   caller.jkt,
		})
		return token
	}
	firstHolder := sessionToken("first-holder-acting-for-its-person", "session-one", brokertoken.ProjectScopePerson)
	secondHolder := sessionToken("second-holder-scoped-to-the-project", "session-two", brokertoken.ProjectScopeNamed, "TEST")
	releasing := sessionToken("second-holder-acting-for-its-person", "session-two", brokertoken.ProjectScopePerson)
	brokerWrite := func(write, method, token string, body any) {
		t.Helper()
		if response := rig.serve(caller.presenting(t, method, path+"/claim", token, body)); response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", write, response.Code, response.Body.String())
		}
	}
	recorded := 0
	// requireLike reads the claim or release event the write before it recorded, the one more
	// than the issue held before, and holds it to the example file.
	requireLike := func(write, file string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path+"/events", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read the issue's events after %s: status=%d body=%s", write, response.Code, response.Body.String())
		}
		var claims []map[string]json.RawMessage
		for _, event := range decodeBody[[]map[string]json.RawMessage](t, response) {
			if kind := string(event["type"]); kind == `"issue.claimed"` || kind == `"issue.released"` {
				claims = append(claims, event)
			}
		}
		if len(claims) != recorded+1 {
			t.Fatalf("%s recorded %d claim and release events, want one more than the %d before it", write, len(claims), recorded)
		}
		recorded++
		got, want := claims[len(claims)-1], sharedIssueExample(t, file)
		var gotPayload, wantPayload map[string]json.RawMessage
		for _, part := range []struct {
			raw  json.RawMessage
			into *map[string]json.RawMessage
		}{{got["payload"], &gotPayload}, {want["payload"], &wantPayload}} {
			if err := json.Unmarshal(part.raw, part.into); err != nil {
				t.Fatalf("decode a payload of %s: %v", write, err)
			}
		}
		for _, field := range []struct {
			name      string
			got, want json.RawMessage
		}{
			{"type", got["type"], want["type"]},
			{"notify", got["notify"], want["notify"]},
			{"reason", gotPayload["reason"], wantPayload["reason"]},
		} {
			if string(field.got) != string(field.want) {
				t.Errorf("%s recorded %s %s, and %s holds %s", write, field.name, field.got, file, field.want)
			}
		}
		for _, claim := range []string{"claim", "previous_claim"} {
			gotHeld, wantHeld := heldClaim(gotPayload[claim]), heldClaim(wantPayload[claim])
			if gotHeld != wantHeld {
				t.Errorf("%s recorded %s %s, and %s holds %s", write, claim, gotHeld, file, wantHeld)
			}
		}
		var gotActor, wantActor map[string]json.RawMessage
		if err := json.Unmarshal(got["actor"], &gotActor); err != nil {
			t.Fatalf("decode the actor %s recorded: %v", write, err)
		}
		if err := json.Unmarshal(want["actor"], &wantActor); err != nil {
			t.Fatalf("decode %s's actor: %v", file, err)
		}
		gotKeys, wantKeys := heldKeys(t, got["actor"]), heldKeys(t, want["actor"])
		if !slices.Equal(gotKeys, wantKeys) || string(gotActor["kind"]) != string(wantActor["kind"]) ||
			string(gotActor["proven"]) != string(wantActor["proven"]) {
			t.Errorf("%s recorded the actor %s, and %s holds %s: want its keys %v, kind and proven mark",
				write, got["actor"], file, want["actor"], wantKeys)
		}
	}

	brokerWrite("first claim", http.MethodPost, firstHolder, map[string]any{})
	requireLike("a first claim", "issue-claimed-first.json")

	// A person's personal token takes the live session's claim with force, for a session outside
	// tmux its body names: the actor is that session, with the token's person as its owner.
	token := mintPersonalToken(t, handler, "alice", "takeover", nil)
	if response := agentRequest(t, handler, http.MethodPost, path+"/claim", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "session-forced", "origin": map[string]any{
			"host": "worker-9", "machine": "worker-9", "cwd": "/work/widgets",
		}},
		"force": true,
	}, token.Token); response.Code != http.StatusOK {
		t.Fatalf("forced takeover: status=%d body=%s", response.Code, response.Body.String())
	}
	requireLike("a forced takeover under a person's token", "issue-claimed-forced.json")

	// The forced holder's session is not among the live ones, so another takes it without force,
	// from a tmux pane its body names.
	brokerWrite("takeover of a session that ended", http.MethodPost, secondHolder, map[string]any{
		"actor": map[string]any{"kind": "session", "id": "session-two", "origin": map[string]any{
			"host": "worker-7", "machine": "worker-7", "cwd": "/work/widgets", "tmux": "legion", "pane": "%4",
			"session_title": "Second agent",
		}},
	})
	requireLike("a takeover of a session that ended", "issue-claimed-every-field.json")

	brokerWrite("the holder's release", http.MethodDelete, releasing, map[string]any{})
	requireLike("the holder's release", "issue-released-null-claim.json")

	// The shared agent token claims for the session its body names, from where its body says the
	// session runs: the token proves no person, service, project or session, so the actor carries
	// none of them (bearerSessionActor).
	if response := bearerRequest(t, handler, http.MethodPost, path+"/claim", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "session-one", "origin": map[string]any{
			"host": "worker-7", "machine": "worker-7", "cwd": "/work/widgets", "tmux": "legion", "pane": "%5",
			"session_title": "CAPTURE-7 implementer",
		}},
	}); response.Code != http.StatusOK {
		t.Fatalf("a claim under the shared agent token: status=%d body=%s", response.Code, response.Body.String())
	}
	requireLike("a claim under the shared agent token", "issue-claimed-agent-token.json")

	// A person at a browser closes the issue, which releases the claim.
	if response := dispatchRequest(t, handler, http.MethodPatch, path, map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("a person's close: status=%d body=%s", response.Code, response.Body.String())
	}
	requireLike("a person's close", "issue-released-closed.json")
}

// heldKeys lists the keys a JSON object holds at every depth, each by its path (`origin.host`),
// sorted, so two actors with the same keys in nested objects compare equal.
func heldKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var keys []string
	for key, value := range object {
		keys = append(keys, key)
		if len(value) > 0 && value[0] == '{' {
			for _, inner := range heldKeys(t, value) {
				keys = append(keys, key+"."+inner)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// sharedIssueExample is the event a shared issue event example's envelope carries.
func sharedIssueExample(t *testing.T, file string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(sharedIssueExamples + file)
	if err != nil {
		t.Fatalf("read the shared example: %v", err)
	}
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode the shared example %s: %v", file, err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(envelope.Payload), &event); err != nil {
		t.Fatalf("decode the event of the shared example %s: %v", file, err)
	}
	return event
}

// heldClaim says how a payload holds a claim: absent, null, or a claim.
func heldClaim(raw json.RawMessage) string {
	switch {
	case raw == nil:
		return "absent"
	case string(raw) == "null":
		return "null"
	}
	return "a claim"
}
