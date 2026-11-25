package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/session"
)

const (
	// gateReplaced is the session a claim names as the one it replaces, gateClaimant the session
	// that claims gateRole naming it. gateHeldRole is a role gateReplaced held, which the claim
	// does not take: its message reaches that role's kept lane only through the handover.
	gateReplaced  = "ses_gate_replaced"
	gateClaimant  = "ses_gate_claimant"
	gateRole      = "gate-role"
	gateHeldRole  = "gate-held-role"
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
// Each session and person has a token of its own, since the listener keeps what the broker
// answered for a token, so one token never acts for two principals.
func (rig *gateRig) personToken(sessionID, person string) string {
	token := "access-token-of-" + sessionID + "-for-" + person
	rig.broker.Says(token, sessionAnswer(sessionID, person, rig.caller.JKT()))
	return token
}

// serviceToken is the access token of sessionID naming service and no person.
func (rig *gateRig) serviceToken(sessionID, service string) string {
	token := "access-token-of-" + sessionID + "-for-" + service
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

// replacedUnread starts gateReplaced's process, registers it presenting token and has it claim
// gateHeldRole, stores a message to it that it reads, then one it does not read and one role
// message of gateHeldRole it does not read, and stops its process as a crash does, past its live
// window: its consumer stands, holding the unread messages unacknowledged, and it is not live.
// It returns the event ids of the read message, the unread direct message and the unread role
// message, and their sequences.
func (rig *gateRig) replacedUnread(t *testing.T, token string) (string, string, string, map[string]uint64) {
	t.Helper()
	standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
	rig.registerOn(t, token, gateReplaced)
	grantRoleClaim(t, rig.mux, gateReplaced, gateHeldRole, "", false)
	read := rig.sendDirect(t, gateReplaced, "dispatch", copiesReadMark+"a message to "+gateReplaced+" it read")
	unread := rig.sendDirect(t, gateReplaced, "dispatch", "an unread message to "+gateReplaced)
	role := rig.publish(t, gateHeldRole, "", "an unread role message of "+gateHeldRole, gateReplaced)
	seqs := copiesSeqs(t, standIn.collect(t, 3, copiesWatchBound), read, unread, role)
	awaitAckState(t, rig.consumers, gateReplaced, 2, seqs[read])
	rig.crash(t, gateReplaced, standIn)
	rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
	if _, state := rig.harness.sessions.Lookup(gateReplaced); state != session.Unresponsive {
		t.Fatalf("%s reads %s past its live window; want %s", gateReplaced, state, session.Unresponsive)
	}
	return read, unread, role, seqs
}

// claimantReceives starts gateClaimant's process, registers it presenting token, has it claim
// gateRole naming gateReplaced on that token, or on the one claimsAs gives where it is set, and
// counts what it receives by event id: want, through the claim's handover, then once more through
// a pass of the kept lane's sweep, which runs the passes of every record not finished. It returns
// the claim's warnings too.
func (rig *gateRig) claimantReceives(t *testing.T, token string, claimsAs func() string, want ...string) ([]string, copiesReceived) {
	t.Helper()
	taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
	rig.registerOn(t, token, gateClaimant)
	if claimsAs != nil {
		token = claimsAs()
	}
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

// A claim that names the session it replaces hands that session's unread messages to the claimant
// only when the credential the claim presents, the claimant's last registration and the replaced
// session's last registration all name one principal (handoverRefusal): a broker token's person,
// else its service, else an identity token's principal, else none. A session's own broker token
// proves only that the claimant is itself, so without this rule any session could name a stopped
// session - a person's among them - and read its messages. Each refused row fails where the rule
// is not kept: the claimant receives the unread direct message, the role message of the role the
// replaced session held reaches that role's kept lane, and the role holds a handover record.
//
// The row where neither session's credential proves a principal is the existing handover tests'
// (TestASessionThatReplacesAnotherReceivesItsUnreadMessages,
// TestTheHandoverHoldsForEachRoleAndForTheMessagesAPersonSent, TestAClaimNamingTheSessionItReplacesWritesOneHandoverRecord),
// whose sessions all register and claim on no credential, so each record names no principal and
// every claim's credential acts for none.
func TestTheHandoverGateHandsOverOnlyBetweenSessionsOfOnePrincipal(t *testing.T) {
	for _, row := range []struct {
		name string
		// replaced and claimant are the credentials each session registers on, and the
		// claimant claims on, by the token each mints for its own session. claimsAs, where set,
		// is another token of the claimant's own session, which it claims on once registered.
		replaced, claimant func(rig *gateRig, sessionID string) string
		claimsAs           func(rig *gateRig, sessionID string) string
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
			name:     "d refused: the claimant registered as one person and claims as the replaced session's",
			replaced: func(rig *gateRig, s string) string { return rig.personToken(s, gateOther) },
			claimant: func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimsAs: func(rig *gateRig, s string) string { return rig.personToken(s, gateOther) },
		},
		{
			name:     "e refused: a claim on a credential that proves no principal names a person's session",
			replaced: func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant: func(*gateRig, string) string { return gateNoneToken },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			read, unread, role, seqs := rig.replacedUnread(t, row.replaced(rig, gateReplaced))
			before := rig.harness.logs.String()
			var claimsAs func() string
			if row.claimsAs != nil {
				claimsAs = func() string { return row.claimsAs(rig, gateClaimant) }
			}
			warnings, received := rig.claimantReceives(t, row.claimant(rig, gateClaimant), claimsAs, wantIf(row.handedOver, unread)...)
			logged := strings.TrimPrefix(rig.harness.logs.String(), before)
			warning, warned := gateWarning(warnings)

			if row.handedOver {
				t.Run("case: the claimant receives the replaced session's unread direct message", func(t *testing.T) {
					copiesRequireEach(t, received, gateClaimant, unread)
				})
				t.Run("case: the unread message of the role the replaced session held waits on that role's kept lane", func(t *testing.T) {
					if n := rig.keptCopiesOf(t, gateHeldRole, role); n != 1 {
						t.Errorf("role %s's kept lane holds %d copies of %s; want exactly one", gateHeldRole, n, role)
					}
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
			t.Run("case: the unread message of the role the replaced session held is copied nowhere", func(t *testing.T) {
				if n := rig.keptCopiesOf(t, gateHeldRole, role); n != 0 {
					t.Errorf("role %s's kept lane holds %d copies of %s; want none", gateHeldRole, n, role)
				}
			})
			t.Run("case: the replaced session's consumer still holds its unread messages unacknowledged", func(t *testing.T) {
				rig.requireUnacknowledged(t, gateReplaced, seqs, unread, role)
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

// A record decides the rule for good only where it says which principal its session last
// registered as. The replaced session with no record in an open store, one whose record a
// listener of an earlier build wrote last (no principal recorded), and a listener with no store
// at all are refused, and the refusal names why. What only a later claim can read is
// errHandoverUnjudged, which the claim route answers 503: a store whose bucket is not open, and a
// claimant whose own registration is not recorded with its principal yet. The control is the same
// claim where both sessions registered on no credential, as the claim's own does, which stands.
func TestTheHandoverGateRefusesWhatNoRegistrationAttests(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	request := httptest.NewRequest(http.MethodPost, "/v1/roles/set", nil)
	if principal := requestPrincipal(request); principal != "" {
		t.Fatalf("a request presenting no credential acts for %q; want no principal", principal)
	}
	register := func(t *testing.T, sessionIDs ...string) {
		t.Helper()
		for _, sessionID := range sessionIDs {
			if err := known.RecordRegistrationWithin(t.Context(), session.KnownSession{SessionID: sessionID}); err != nil {
				t.Fatalf("record %s's registration: %v", sessionID, err)
			}
		}
	}
	writtenByAnEarlierBuild := func(t *testing.T, sessionID string) {
		t.Helper()
		kv, err := client.JS().KeyValue(bus.KnownSessionBucket)
		if err != nil {
			t.Fatalf("open the known-session bucket: %v", err)
		}
		value, err := json.Marshal(map[string]any{"session_id": sessionID, "topics": []string{}, "last_seen": time.Now().UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Put(sessionID, value); err != nil {
			t.Fatalf("write %s's record as an earlier build does: %v", sessionID, err)
		}
	}
	refused := func(t *testing.T, known *session.KnownSessions, replaced, claimant, reason string) {
		t.Helper()
		why, err := handoverRefusal(t.Context(), request, known, replaced, claimant)
		if err != nil || why == "" || !strings.Contains(why, reason) {
			t.Errorf("handoverRefusal(%s replaced by %s) = %q, %v; want a refusal saying %q", replaced, claimant, why, err, reason)
		}
	}
	unjudged := func(t *testing.T, known *session.KnownSessions, replaced, claimant string) {
		t.Helper()
		why, err := handoverRefusal(t.Context(), request, known, replaced, claimant)
		if why != "" || !errors.Is(err, errHandoverUnjudged) {
			t.Errorf("handoverRefusal(%s replaced by %s) = %q, %v; want %v, for the claim to be made again", replaced, claimant, why, err, errHandoverUnjudged)
		}
	}

	t.Run("case: an open store holds no record of the replaced session", func(t *testing.T) {
		register(t, "ses_gate_unit_claimant_a")
		refused(t, known, "ses_gate_unit_never", "ses_gate_unit_claimant_a", "holds no record of the principal ses_gate_unit_never")
	})
	t.Run("case: a listener of an earlier build wrote the replaced session's record last", func(t *testing.T) {
		register(t, "ses_gate_unit_claimant_b")
		writtenByAnEarlierBuild(t, "ses_gate_unit_earlier")
		refused(t, known, "ses_gate_unit_earlier", "ses_gate_unit_claimant_b", "earlier build")
	})
	t.Run("case: the listener keeps no known-session store", func(t *testing.T) {
		refused(t, nil, "ses_gate_unit_any", "ses_gate_unit_claimant_c", "keeps no record")
	})
	t.Run("case: the claimant's own registration is not recorded with its principal yet", func(t *testing.T) {
		register(t, "ses_gate_unit_replaced_d")
		unjudged(t, known, "ses_gate_unit_replaced_d", "ses_gate_unit_unregistered")
		writtenByAnEarlierBuild(t, "ses_gate_unit_claimant_earlier")
		unjudged(t, known, "ses_gate_unit_replaced_d", "ses_gate_unit_claimant_earlier")
	})
	t.Run("case: a store whose bucket is not open", func(t *testing.T) {
		unjudged(t, &session.KnownSessions{}, "ses_gate_unit_replaced_d", "ses_gate_unit_claimant_e")
	})
	t.Run("control: both sessions registered on no credential, and the claim presents none", func(t *testing.T) {
		register(t, "ses_gate_unit_replaced_f", "ses_gate_unit_claimant_f")
		if why, err := handoverRefusal(t.Context(), request, known, "ses_gate_unit_replaced_f", "ses_gate_unit_claimant_f"); why != "" || err != nil {
			t.Errorf("handoverRefusal = %q, %v; want the handover to stand", why, err)
		}
	})
}

// The rule binds the two sessions after the claim as well: each pass of the handover record, and
// the successor read Dispatch's reply makes, read both sessions' last registrations again
// (handoverStands). A claim of one person's session names that person's other session while it is
// still live, so the claim's own pass copies nothing and the record stands; then the replaced
// session registers again, as another person in the case row and as the same person in the
// control, and stops. In the case the sweep's pass copies nothing and the successor read names no
// one; in the control the claimant receives the unread message and is named. The case fails where
// a record, once written, hands over whatever the sessions register as later.
func TestAHandoverEndsWhenTheReplacedSessionRegistersAsAnotherPrincipal(t *testing.T) {
	for _, row := range []struct {
		name       string
		registered string
		handedOver bool
	}{
		{name: "case: it registers again as another person", registered: gateOther},
		{name: "control: it registers again as the same person", registered: gatePerson, handedOver: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
			rig.registerOn(t, rig.personToken(gateReplaced, gatePerson), gateReplaced)
			read := rig.sendDirect(t, gateReplaced, "dispatch", copiesReadMark+"a message to "+gateReplaced+" it read")
			unread := rig.sendDirect(t, gateReplaced, "dispatch", "an unread message to "+gateReplaced)
			seqs := copiesSeqs(t, standIn.collect(t, 2, copiesWatchBound), read, unread)
			awaitAckState(t, rig.consumers, gateReplaced, 1, seqs[read])

			taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
			token := rig.personToken(gateClaimant, gatePerson)
			rig.registerOn(t, token, gateClaimant)
			if warning, warned := gateWarning(rig.claimOn(t, token)); warned {
				t.Fatalf("the claim of one person's session naming that person's other session warned %q", warning)
			}
			if n := rig.handoverRecords(t, gateRole, gateReplaced, gateClaimant); n != 1 {
				t.Fatalf("role %s holds %d handover records from %s to %s; want exactly one", gateRole, n, gateReplaced, gateClaimant)
			}

			rig.registerOn(t, rig.personToken(gateReplaced, row.registered), gateReplaced)
			rig.crash(t, gateReplaced, standIn)
			rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
			rig.sweepPass()
			received := rig.watch(t, taker, gateClaimant, gateRole, wantIf(row.handedOver, unread)...)
			successors, recorder := readSuccessors(t, rig.mux, gateReplaced)
			requireStatus(t, recorder, http.StatusOK, "the successors of "+gateReplaced)

			if row.handedOver {
				copiesRequireEach(t, received, gateClaimant, unread)
				if !slices.Equal(successors.Successors, []string{gateClaimant}) {
					t.Errorf("the successors of %s = %v; want [%s]", gateReplaced, successors.Successors, gateClaimant)
				}
				return
			}
			copiesRequireNone(t, received, gateClaimant, unread)
			if n := rig.handedOverCopies(t, gateClaimant, unread); n != 0 {
				t.Errorf("%s's own subject holds %d copies of %s; want none", gateClaimant, n, unread)
			}
			rig.requireUnacknowledged(t, gateReplaced, seqs, unread)
			if len(successors.Successors) != 0 {
				t.Errorf("the successors of %s = %v; want none: it last registered as another person than %s", gateReplaced, successors.Successors, gateClaimant)
			}
		})
	}
}

// A pass holds each copy to the rule as well, not only its start: a registration as another
// principal that lands while the pass runs ends it before the next copy (handoverStillStands, read
// from the bucket's leader as the last step before each copy). The replaced session has two unread
// messages and stops; the sweep's pass begins, and just before its first copy the claimant
// registers again, as another person in the case row and as the same person in the control. In
// the case no message is copied after that registration, and both stay unread on the replaced
// session; in the control both reach the claimant. The case fails where the rule is read only at
// the pass's start, or before a step that can still let a registration land ahead of the copy.
func TestAHandoverPassEndsWhenASessionRegistersAsAnotherPrincipalWhileItRuns(t *testing.T) {
	for _, row := range []struct {
		name       string
		registered string
		both       bool
	}{
		{name: "case: the claimant registers again as another person just before the pass's first copy", registered: gateOther},
		{name: "control: the claimant registers again as the same person just before the pass's first copy", registered: gatePerson, both: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
			rig.registerOn(t, rig.personToken(gateReplaced, gatePerson), gateReplaced)
			// The message it reads keeps it live until it stops, so the claim's own pass copies
			// nothing and the sweep's pass after the stop is the one the fault runs in.
			read := rig.sendDirect(t, gateReplaced, "dispatch", copiesReadMark+"a message to "+gateReplaced+" it read")
			first := rig.sendDirect(t, gateReplaced, "dispatch", "the first unread message to "+gateReplaced)
			seqs := copiesSeqs(t, standIn.collect(t, 2, copiesWatchBound), read, first)
			awaitAckState(t, rig.consumers, gateReplaced, 1, seqs[read])

			taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
			token := rig.personToken(gateClaimant, gatePerson)
			rig.registerOn(t, token, gateClaimant)
			if warning, warned := gateWarning(rig.claimOn(t, token)); warned {
				t.Fatalf("the claim of one person's session naming that person's other session warned %q", warning)
			}
			if n := rig.handedOverCopies(t, gateClaimant, first); n != 0 {
				t.Fatalf("the claim's own pass copied %s while %s was live: %d copies", first, gateReplaced, n)
			}
			second := rig.sendDirect(t, gateReplaced, "dispatch", "the second unread message to "+gateReplaced)
			for eventID, seq := range copiesSeqs(t, standIn.collect(t, 1, copiesWatchBound), second) {
				seqs[eventID] = seq
			}
			registered := false
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{beforeCopy: func(_, previous string, _ uint64) error {
				if previous == gateReplaced && !registered {
					registered = true
					rig.registerOn(t, rig.personToken(gateClaimant, row.registered), gateClaimant)
				}
				return nil
			}})
			rig.crash(t, gateReplaced, standIn)
			rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
			rig.sweepPass()
			if !registered {
				t.Fatalf("the sweep's pass reached no copy of %s's messages", gateReplaced)
			}
			want := wantIf(row.both, first, second)
			received := rig.watch(t, taker, gateClaimant, gateRole, want...)

			if row.both {
				copiesRequireEach(t, received, gateClaimant, first, second)
				return
			}
			copiesRequireNone(t, received, gateClaimant, first, second)
			for _, eventID := range []string{first, second} {
				if n := rig.handedOverCopies(t, gateClaimant, eventID); n != 0 {
					t.Errorf("%s's own subject holds %d copies of %s after it registered as another person; want none", gateClaimant, n, eventID)
				}
			}
			rig.requireUnacknowledged(t, gateReplaced, seqs, first, second)
		})
	}
}

// A registration whose known-session write failed leaves the record naming the principal the
// session registered as before, which the handover gate would read as the one it last registered
// as. So the registration route answers such a registration 503 with Retry-After: 1 rather than
// 200 (session.ErrPrincipalUnrecorded), and the client registers again until the record names its
// principal. The session first registers as a service; then the bucket takes no write (its
// stream's largest message is one byte) and it registers again, with a new title so the record
// is written rather than skipped as unchanged. The case is a registration as a person, which fails
// where a failed write is answered 200 whatever principal it leaves recorded. The controls are a
// registration as the same service, whose failed write leaves its own principal recorded and is
// answered 200, and a registration as a person through a bucket that takes the write.
func TestARegistrationThatLeavesAnotherPrincipalRecordedIsAnswered503(t *testing.T) {
	const sessionID = "ses_gate_registration_unrecorded"
	for _, row := range []struct {
		name      string
		refused   bool
		registers func(rig *gateRig) string
		status    int
		stored    string
	}{
		{
			name:      "case: the session registers again as a person and the bucket takes no write",
			refused:   true,
			registers: func(rig *gateRig) string { return rig.personToken(sessionID, gatePerson) },
			status:    http.StatusServiceUnavailable,
			stored:    gateService,
		},
		{
			name:      "control: the session registers again as the same service and the bucket takes no write",
			refused:   true,
			registers: func(rig *gateRig) string { return rig.serviceToken(sessionID, gateService) },
			status:    http.StatusOK,
			stored:    gateService,
		},
		{
			name:      "control: the session registers again as a person and the bucket takes the write",
			registers: func(rig *gateRig) string { return rig.personToken(sessionID, gatePerson) },
			status:    http.StatusOK,
			stored:    gatePerson,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			rig.registerOn(t, rig.serviceToken(sessionID, gateService), sessionID)
			if row.refused {
				js := rig.harness.client.JS()
				info, err := js.StreamInfo("KV_" + bus.KnownSessionBucket)
				if err != nil {
					t.Fatalf("read the known-session stream: %v", err)
				}
				info.Config.MaxMsgSize = 1
				if _, err := js.UpdateStream(&info.Config); err != nil {
					t.Fatalf("make the known-session stream take no write: %v", err)
				}
			}
			body, err := json.Marshal(map[string]any{
				"session_id":      sessionID,
				"self_subscribed": true,
				"title":           "a session registered again",
				"topics":          []string{},
				"delivery":        map[string]any{"consumer": true, "epoch": "epoch-" + sessionID},
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := rig.postOn(t, row.registers(rig), "/v1/interests/subscribe", string(body))
			if recorder.Code != row.status {
				t.Errorf("the registration answered %d %s; want %d", recorder.Code, recorder.Body.String(), row.status)
			}
			if row.status == http.StatusServiceUnavailable && recorder.Header().Get("Retry-After") != "1" {
				t.Errorf("the 503 carries Retry-After %q; want 1", recorder.Header().Get("Retry-After"))
			}
			record, found, err := rig.deps.known.Read(t.Context(), sessionID)
			if err != nil || !found {
				t.Fatalf("read %s's record from the bucket's leader: found %t, %v", sessionID, found, err)
			}
			if record.Principal != row.stored {
				t.Errorf("the bucket's record names %q; want %q", record.Principal, row.stored)
			}
		})
	}
}
