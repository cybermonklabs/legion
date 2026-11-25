package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/session"
)

const (
	// gateReplaced is the session a claim names as the one it replaces, gateClaimant the session
	// that claims gateRole naming it.
	gateReplaced  = "ses_gate_replaced"
	gateClaimant  = "ses_gate_claimant"
	gateRole      = "gate-role"
	gatePerson    = "ada@example.com"
	gateOther     = "grace@example.com"
	gateService   = "example-fleet"
	gateNoneToken = ""
)

// gateRig is a copiesRig whose /v1 routes are also served behind apiAuth with broker tokens
// taken (front), as newBrokerRig mounts them: the credential is real but the broker, a stub that
// answers what each row says each token is, and the routes behind it are the copies rig's own, over
// the same bus, relays and stores. A request sent past front, through the copies rig's own mux,
// presents no credential, as one on an open /v1 does.
type gateRig struct {
	*copiesRig
	front  http.Handler
	broker *ciphertokentest.StubBroker
	caller *ciphertokentest.Caller
}

func newGateRig(t *testing.T) *gateRig {
	t.Helper()
	rig := newCopiesRig(t)
	broker := ciphertokentest.NewStubBroker(t)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the listener's own account token: %v", err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        broker.Server.URL,
		AccountTokenFile: accountFile,
		Origin:           listenerBrokerOrigin,
	})
	if err := tokens.open(t.Context(), rig.harness.client.Conn, 1); err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	return &gateRig{
		copiesRig: rig,
		front:     apiAuth(listenerAPIAuth{brokers: tokens, logger: rig.harness.config.logger}, rig.mux),
		broker:    broker,
		caller:    ciphertokentest.NewCaller(t),
	}
}

// personToken is the access token of sessionID acting for person, which the stub broker answers.
func (rig *gateRig) personToken(sessionID, person string) string {
	token := "access-token-of-" + sessionID
	rig.broker.Says(token, sessionAnswer(sessionID, person, rig.caller.JKT()))
	return token
}

// serviceToken is the access token of sessionID naming service and no person.
func (rig *gateRig) serviceToken(sessionID, service string) string {
	token := "access-token-of-" + sessionID
	rig.broker.Says(token, serviceAnswer(sessionID, service, rig.caller.JKT()))
	return token
}

// postOn sends one POST of body to path presenting token, through front, or past it with no
// credential when token is empty.
func (rig *gateRig) postOn(t *testing.T, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	if token == gateNoneToken {
		rig.mux.ServeHTTP(recorder, postJSON(path, body))
		return recorder
	}
	rig.front.ServeHTTP(recorder, brokerTokenRequest(t, rig.caller, http.MethodPost, path, token, body))
	return recorder
}

// registerOn registers sessionID in consumer mode, as the copies rig's register does, presenting
// token, and fails the row unless the registration is taken.
func (rig *gateRig) registerOn(t *testing.T, token, sessionID string) {
	t.Helper()
	body := map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"title":           "a checked session",
		"topics":          []string{},
		"delivery":        map[string]any{"consumer": true, "epoch": "epoch-" + sessionID},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if recorder := rig.postOn(t, token, "/v1/interests/subscribe", string(encoded)); recorder.Code != http.StatusOK {
		t.Fatalf("register %s: status %d %s; want 200", sessionID, recorder.Code, recorder.Body.String())
	}
}

// claimOn has gateClaimant claim gateRole outright presenting token, naming gateReplaced as the
// session it replaces, and fails the row unless the claim is granted. It returns the claim's
// warnings.
func (rig *gateRig) claimOn(t *testing.T, token string) []string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": gateClaimant, "role": gateRole, "previous_session_id": gateReplaced})
	if err != nil {
		t.Fatal(err)
	}
	recorder := rig.postOn(t, token, "/v1/roles/set", string(body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s naming %s: status %d %s; want 200", gateClaimant, gateRole, gateReplaced, recorder.Code, recorder.Body.String())
	}
	var answer roleSetResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the claim's answer %s: %v", recorder.Body.String(), err)
	}
	return answer.Warnings
}

// replacedUnread starts gateReplaced's process and registers it presenting token, stores a message
// to it that it reads and then one it does not read, and stops its process as a crash does, past
// its live window: its consumer stands, holding the unread message unacknowledged, and it is not
// live. It returns the event ids of the read message and the unread one, and their sequences.
func (rig *gateRig) replacedUnread(t *testing.T, token string) (string, string, map[string]uint64) {
	t.Helper()
	standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
	rig.registerOn(t, token, gateReplaced)
	read := rig.sendDirect(t, gateReplaced, "dispatch", copiesReadMark+"a message to "+gateReplaced+" it read")
	unread := rig.sendDirect(t, gateReplaced, "dispatch", "an unread message to "+gateReplaced)
	seqs := copiesSeqs(t, standIn.collect(t, 2, copiesWatchBound), read, unread)
	awaitAckState(t, rig.consumers, gateReplaced, 1, seqs[read])
	rig.crash(t, gateReplaced, standIn)
	rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
	if _, state := rig.harness.sessions.Lookup(gateReplaced); state != session.Unresponsive {
		t.Fatalf("%s reads %s past its live window; want %s", gateReplaced, state, session.Unresponsive)
	}
	return read, unread, seqs
}

// claimantReceives starts gateClaimant's process, registers it presenting token, has it claim
// gateRole naming gateReplaced on the same credential, and counts what it receives by event id:
// want, through the claim's handover, then once more through a pass of the kept lane's sweep,
// which runs the passes of every record not finished. It returns the claim's warnings too.
func (rig *gateRig) claimantReceives(t *testing.T, token string, want ...string) ([]string, copiesReceived) {
	t.Helper()
	taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
	rig.registerOn(t, token, gateClaimant)
	warnings := rig.claimOn(t, token)
	received := rig.watch(t, taker, gateClaimant, gateRole, want...)
	rig.sweepPass()
	for eventID, n := range rig.watch(t, taker, gateClaimant, gateRole) {
		received[eventID] += n
	}
	return warnings, received
}

// gateWarning is the claim's warning naming the replaced session's messages as not handed over,
// and whether it has one.
func gateWarning(warnings []string) (string, bool) {
	for _, warning := range warnings {
		if strings.Contains(warning, gateReplaced) && strings.Contains(warning, "no message of") {
			return warning, true
		}
	}
	return "", false
}

// A claim that names the session it replaces hands that session's unread direct messages to the
// claimant only when the credential the claim presents acts for the principal that session last
// registered as (handoverRefusal): a broker token's person, else its service, else an identity
// token's principal, else none. A session's own broker token proves only that the claimant is
// itself, so without this rule any session could name a stopped session - a person's among them -
// and read its messages. Each refused row fails where the rule is not kept: the claimant receives
// the unread message, and the role holds a handover record.
//
// The row where neither session's credential proves a principal is the existing handover tests'
// (TestASessionThatReplacesAnotherReceivesItsUnreadMessages,
// TestTheHandoverHoldsForEachRoleAndForAPersonsSession, TestAClaimNamingTheSessionItReplacesWritesOneHandoverRecord),
// whose sessions all register and claim on no credential, so each record names no principal and
// every claim's credential acts for none.
func TestTheHandoverGateHandsOverOnlyBetweenSessionsOfOnePrincipal(t *testing.T) {
	for _, row := range []struct {
		name string
		// replaced and claimant are the credentials each session registers on, and the
		// claimant claims on, by the token each mints for its own session.
		replaced, claimant func(rig *gateRig, sessionID string) string
		handedOver         bool
	}{
		{
			name:       "a allowed: both sessions act for one person",
			replaced:   func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant:   func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			handedOver: true,
		},
		{
			name:       "a allowed: both sessions act for one service",
			replaced:   func(rig *gateRig, s string) string { return rig.serviceToken(s, gateService) },
			claimant:   func(rig *gateRig, s string) string { return rig.serviceToken(s, gateService) },
			handedOver: true,
		},
		{
			name:     "b refused: a service's session names a person's session",
			replaced: func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant: func(rig *gateRig, s string) string { return rig.serviceToken(s, gateService) },
		},
		{
			name:     "b refused: one person's session names another person's session",
			replaced: func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant: func(rig *gateRig, s string) string { return rig.personToken(s, gateOther) },
		},
		{
			name:     "c refused: the replaced session registered on a credential that proves no principal",
			replaced: func(*gateRig, string) string { return gateNoneToken },
			claimant: func(rig *gateRig, s string) string { return rig.serviceToken(s, gateService) },
		},
		{
			name:     "e refused: a claim on a credential that proves no principal names a person's session",
			replaced: func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant: func(*gateRig, string) string { return gateNoneToken },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			read, unread, seqs := rig.replacedUnread(t, row.replaced(rig, gateReplaced))
			before := rig.harness.logs.String()
			warnings, received := rig.claimantReceives(t, row.claimant(rig, gateClaimant), wantIf(row.handedOver, unread)...)
			logged := strings.TrimPrefix(rig.harness.logs.String(), before)
			warning, warned := gateWarning(warnings)

			if row.handedOver {
				t.Run("case: the claimant receives the replaced session's unread direct message", func(t *testing.T) {
					copiesRequireEach(t, received, gateClaimant, unread)
				})
				t.Run("control: the claimant receives none of what the replaced session read, and the claim warns of nothing", func(t *testing.T) {
					copiesRequireNone(t, received, gateClaimant, read)
					if warned {
						t.Errorf("the claim warned %q; want no warning about %s's messages", warning, gateReplaced)
					}
				})
				t.Run("control: the role holds the handover record", func(t *testing.T) {
					if n := rig.handoverRecords(t, gateRole, gateReplaced, gateClaimant); n != 1 {
						t.Errorf("role %s holds %d handover records from %s to %s; want exactly one", gateRole, n, gateReplaced, gateClaimant)
					}
				})
				return
			}
			t.Run("case: the claim stands and warns that no message of the replaced session is handed over", func(t *testing.T) {
				if !warned {
					t.Errorf("the claim's warnings are %q; want one naming %s and saying no message of it is handed over", warnings, gateReplaced)
				}
			})
			t.Run("case: the role holds no handover record", func(t *testing.T) {
				if n := rig.handoverRecords(t, gateRole, gateReplaced, gateClaimant); n != 0 {
					t.Errorf("role %s holds %d handover records from %s to %s; want none", gateRole, n, gateReplaced, gateClaimant)
				}
			})
			t.Run("case: the claimant receives none of the replaced session's direct messages", func(t *testing.T) {
				copiesRequireNone(t, received, gateClaimant, read, unread)
				if n := rig.handedOverCopies(t, gateClaimant, unread); n != 0 {
					t.Errorf("%s's own subject holds %d copies of %s; want none", gateClaimant, n, unread)
				}
			})
			t.Run("case: the replaced session's consumer still holds its unread message unacknowledged", func(t *testing.T) {
				rig.requireUnacknowledged(t, gateReplaced, seqs, unread)
			})
			t.Run("case: the listener logs the refusal once at INFO, naming the role and both sessions", func(t *testing.T) {
				lines := 0
				for _, line := range strings.Split(logged, "\n") {
					if !strings.Contains(line, "hands over no message of the session it replaces") {
						continue
					}
					lines++
					for _, want := range []string{`"level":"INFO"`, `"role":"` + gateRole + `"`, `"previous_session_id":"` + gateReplaced + `"`, `"session_id":"` + gateClaimant + `"`} {
						if !strings.Contains(line, want) {
							t.Errorf("the refusal line %s lacks %s", line, want)
						}
					}
				}
				if lines != 1 {
					t.Errorf("the listener logged %d refusal lines; want one: %s", lines, logged)
				}
			})
		})
	}
}

// wantIf is eventIDs when want holds, and none otherwise: a watch for a message the row expects
// no copy of returns once the claim's handover has ended instead of waiting out its bound.
func wantIf(want bool, eventIDs ...string) []string {
	if !want {
		return nil
	}
	return eventIDs
}

// The rule refuses where nothing tells the principal the replaced session registered as: a session
// the store holds no record of, and a listener with no store open, which reads every session so.
// A request with no credential acts for no principal, and a record naming none is the only one
// it matches.
func TestTheHandoverGateRefusesASessionWithNoRecord(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/roles/set", nil)
	if principal := requestPrincipal(request); principal != "" {
		t.Fatalf("a request presenting no credential acts for %q; want no principal", principal)
	}
	if refusal := handoverRefusal(request, nil, gateReplaced); !strings.Contains(refusal, gateReplaced) {
		t.Errorf("the refusal of a claim naming %s on a listener with no known-session store is %q; want one naming it", gateReplaced, refusal)
	}
}
