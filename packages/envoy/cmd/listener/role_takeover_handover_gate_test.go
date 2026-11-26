package main

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
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
	gateProject   = "ACME"
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
	// The successor read a row makes is Dispatch's (readSuccessors), which the route names the
	// successors' principal to alone.
	rig.deps.dispatchPrincipal = testDispatchPrincipal
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

// namedToken is the access token of sessionID that an operator's entry gave person for project
// (ciphertoken.ProjectScopeNamed): a worker pod's, which acts for person on that one project and
// is an agent's, not the person's own.
func (rig *gateRig) namedToken(sessionID, project, person string) string {
	token := "access-token-of-" + sessionID + "-for-" + person + "-on-" + project
	answer := sessionAnswer(sessionID, person, rig.caller.JKT())
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopeNamed, []string{project}
	rig.broker.Says(token, answer)
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

// gateClaimDrainHeld has the claim route start no handover of gateRole (skipClaimHandover), for a
// row whose fault must run in the sweep's pass: the claim's own handover runs on a goroutine of its
// own, and one that starts late runs its pass after the row has stopped the replaced session and
// moved the clock, outside the seam the row sets, and copies everything first.
func gateClaimDrainHeld(role string) bool { return role == gateRole }

// A claim that names the session it replaces hands that session's unread messages to the claimant
// only when the credential the claim presents, the claimant's last registration and the replaced
// session's last registration all name one principal (handoverRefusal), in the form that names the
// credential's kind and scope (ciphertoken.Caller.Principal): a person's own token, a token an
// operator's entry gave a person for a project, a service, an identity token, or none. A session's
// own broker token proves only that the claimant is itself, so without this rule any session could
// name a stopped session - a person's among them - and read its messages, and a worker agent on a
// project entry that names a person could read that person's own session's. Each refused row
// fails where the rule is not kept: the claimant receives the unread direct message, the role
// message of the role the replaced session held reaches that role's kept lane, the role holds a
// handover record, and the successor read names the claimant, which lets Dispatch take its reply
// to the replaced session's deliveries.
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
		// principal is the principal a row that hands over names both sessions for, as the
		// successor read answers it.
		principal string
	}{
		{
			name:       "a allowed: both sessions act for one person",
			replaced:   func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant:   func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			handedOver: true,
			principal:  "person:" + gatePerson,
		},
		{
			name:       "a allowed: both sessions act for one service",
			replaced:   func(rig *gateRig, s string) string { return rig.serviceToken(s, gateService) },
			claimant:   func(rig *gateRig, s string) string { return rig.serviceToken(s, gateService) },
			handedOver: true,
			principal:  "service:" + gateService,
		},
		{
			name:       "a allowed: both sessions are one project entry's, acting for one person",
			replaced:   func(rig *gateRig, s string) string { return rig.namedToken(s, gateProject, gatePerson) },
			claimant:   func(rig *gateRig, s string) string { return rig.namedToken(s, gateProject, gatePerson) },
			handedOver: true,
			principal:  "named:" + gateProject + ":" + gatePerson,
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
			name:     "b refused: a project entry's session acting for a person names that person's own session",
			replaced: func(rig *gateRig, s string) string { return rig.personToken(s, gatePerson) },
			claimant: func(rig *gateRig, s string) string { return rig.namedToken(s, gateProject, gatePerson) },
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
				t.Run("control: the successor read names the claimant, for the principal both registered as", func(t *testing.T) {
					successors, recorder := readSuccessors(t, rig.mux, gateReplaced)
					requireStatus(t, recorder, http.StatusOK, "the successors of "+gateReplaced)
					if !slices.Equal(successors.Successors, []string{gateClaimant}) || successors.Principal != row.principal {
						t.Errorf("the successors of %s = %v for %q; want [%s] for %q", gateReplaced, successors.Successors, successors.Principal, gateClaimant, row.principal)
					}
				})
				return
			}
			t.Run("case: the successor read names no session, so Dispatch refuses the claimant's reply to the replaced session's deliveries", func(t *testing.T) {
				successors, recorder := readSuccessors(t, rig.mux, gateReplaced)
				requireStatus(t, recorder, http.StatusOK, "the successors of "+gateReplaced)
				if len(successors.Successors) != 0 {
					t.Errorf("the successors of %s = %v for %q; want none", gateReplaced, successors.Successors, successors.Principal)
				}
			})
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

// A claim is refused where no record says which principal the replaced session last registered
// as (recordedPrincipal). The replaced session with no record in an open store, one whose record a
// listener of an earlier build wrote last (no principal recorded), one whose record a registration
// as another principal cleared and has not written its own over yet, and a store that cannot
// answer - one whose bucket is not open, which reads no record and records no principal until an
// attempt of its own opens the bucket, and a listener with no store at all, which reads as one -
// are refused, and the refusal names why, the two records with no principal recorded by one
// warning that lists no cause, since other writes leave a record so too (an interest change that
// creates it, in TestARegistrationThroughAListenerWhoseKnownSessionBucketIsNotOpenIsAdmittedWithAWarning).
// What only a later claim can read is errHandoverUnjudged, which the claim route answers 503: a
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
		registerAs(t, known, "", sessionIDs...)
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
	unrecorded := func(sessionID string) string {
		return "no registration through a listener of this build has recorded a principal for " + sessionID
	}

	t.Run("case: an open store holds no record of the replaced session", func(t *testing.T) {
		register(t, "ses_gate_unit_claimant_a")
		refused(t, known, "ses_gate_unit_never", "ses_gate_unit_claimant_a", "holds no record of the principal ses_gate_unit_never")
	})
	t.Run("case: a listener of an earlier build wrote the replaced session's record last", func(t *testing.T) {
		register(t, "ses_gate_unit_claimant_b")
		writtenByAnEarlierBuild(t, "ses_gate_unit_earlier")
		refused(t, known, "ses_gate_unit_earlier", "ses_gate_unit_claimant_b", unrecorded("ses_gate_unit_earlier"))
	})
	t.Run("case: a registration as another principal cleared the replaced session's record", func(t *testing.T) {
		register(t, "ses_gate_unit_claimant_g")
		registerAs(t, known, "service:example-fleet", "ses_gate_unit_cleared")
		if _, err := known.AdmitRegistration(t.Context(), "ses_gate_unit_cleared", "service:another-fleet"); err != nil {
			t.Fatalf("admit ses_gate_unit_cleared's registration as another principal: %v", err)
		}
		refused(t, known, "ses_gate_unit_cleared", "ses_gate_unit_claimant_g", unrecorded("ses_gate_unit_cleared"))
	})
	t.Run("case: the listener keeps no known-session store", func(t *testing.T) {
		refused(t, nil, "ses_gate_unit_any", "ses_gate_unit_claimant_c", knownBucketNotOpenRefusal)
	})
	t.Run("case: the claimant's own registration is not recorded with its principal yet", func(t *testing.T) {
		register(t, "ses_gate_unit_replaced_d")
		unjudged(t, known, "ses_gate_unit_replaced_d", "ses_gate_unit_unregistered")
		writtenByAnEarlierBuild(t, "ses_gate_unit_claimant_earlier")
		unjudged(t, known, "ses_gate_unit_replaced_d", "ses_gate_unit_claimant_earlier")
	})
	t.Run("case: a store whose bucket is not open", func(t *testing.T) {
		refused(t, &session.KnownSessions{}, "ses_gate_unit_replaced_d", "ses_gate_unit_claimant_e", "known-session bucket (envoy_known_sessions) is not open")
	})
	t.Run("control: both sessions registered on no credential, and the claim presents none", func(t *testing.T) {
		register(t, "ses_gate_unit_replaced_f", "ses_gate_unit_claimant_f")
		if why, err := handoverRefusal(t.Context(), request, known, "ses_gate_unit_replaced_f", "ses_gate_unit_claimant_f"); why != "" || err != nil {
			t.Errorf("handoverRefusal = %q, %v; want the handover to stand", why, err)
		}
	})
}

// A pass holds a store that cannot answer to be an error, not a refusal (handoverStands): a
// refusal sets the record back (runTakeoverPass's setBack), which a listener that can read no
// principal has no ground for, while an error ends the pass and leaves the record for a later
// pass to judge. The cases are a store whose bucket is not open and a listener with no store at
// all; each fails where handoverStands answers either as a handover that does not stand. The
// control is an open store where both sessions registered as one principal, which stands, and
// one where the successor registered as another, which does not, with no error.
func TestAHandoverPassReadsAStoreThatCannotAnswerAsAnError(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	const previous, successor, stranger = "ses_gate_stands_previous", "ses_gate_stands_successor", "ses_gate_stands_stranger"
	registerAs(t, known, "service:example-fleet", previous, successor)
	registerAs(t, known, "service:another-fleet", stranger)

	for _, row := range []struct {
		name  string
		known *session.KnownSessions
	}{
		{name: "case: a store whose bucket is not open", known: &session.KnownSessions{}},
		{name: "case: the listener keeps no known-session store", known: nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			principal, stands, err := handoverStands(t.Context(), row.known, previous, successor)
			if !errors.Is(err, session.ErrKnownSessionsUnavailable) || stands || principal != "" {
				t.Errorf("handoverStands = %q, %t, %v; want no principal, not standing, and %v", principal, stands, err, session.ErrKnownSessionsUnavailable)
			}
		})
	}
	t.Run("control: both sessions registered as one principal through an open store", func(t *testing.T) {
		principal, stands, err := handoverStands(t.Context(), known, previous, successor)
		if err != nil || !stands || principal != "service:example-fleet" {
			t.Errorf("handoverStands = %q, %t, %v; want service:example-fleet, standing, no error", principal, stands, err)
		}
	})
	t.Run("control: the successor registered as another principal through an open store", func(t *testing.T) {
		if _, stands, err := handoverStands(t.Context(), known, previous, stranger); err != nil || stands {
			t.Errorf("handoverStands = %t, %v; want not standing, no error", stands, err)
		}
	})
}

// The rule binds the two sessions after the claim as well: each pass of the handover record reads
// both sessions' last registrations again (handoverFence, handoverStands), and the successor read
// Dispatch's reply makes follows a record only to a session of the replaced session's principal
// (handoverSuccessors). A claim of one person's session names that person's other session while it is
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
// principal that lands while the pass runs ends it before the next copy (handoverFence.judge, read
// from the bucket's leader as the last step before each copy). The replaced session has two unread
// messages and stops; the sweep's pass begins, and just before its first copy the claimant
// registers again, as another person in the case row and as the same person in the control. In
// the case no message is copied after that registration, and both stay unread on the replaced
// session; in the control both reach the claimant. The case fails where the rule is read only at
// the pass's start, or before a step that can still let a registration land ahead of the copy.
// The claim's own handover is held (gateClaimDrainHeld), so the sweep's pass is the record's first.
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
			// The claim's own handover is held, so the sweep's pass after the stop is the one the
			// fault runs in.
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld})
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
			second := rig.sendDirect(t, gateReplaced, "dispatch", "the second unread message to "+gateReplaced)
			for eventID, seq := range copiesSeqs(t, standIn.collect(t, 1, copiesWatchBound), second) {
				seqs[eventID] = seq
			}
			registered := false
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld, beforeCopy: func(_, previous string, _ uint64) error {
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

// A pass copies nothing while the replaced session is live, and it reads that before each copy as
// well as at its start, after every step but the principal reads and the copy itself: a
// registration of the replaced session that lands while the pass runs ends it before the next
// copy. The replaced session has two unread messages and stops; the sweep's pass begins, and just
// before its first copy (the beforeCopy seam) the replaced session registers again, as the same
// person, in the case row, and nothing registers in the control. In the case neither message is
// copied to the claimant, and both stay unread on the replaced session, which answers again; in
// the control both reach the claimant. The case fails where the pass reads the replaced session's
// state only at its start, or before a step a registration can still land behind. The claim's own
// handover is held (gateClaimDrainHeld), so the sweep's pass is the record's first.
func TestAHandoverPassEndsWhenTheReplacedSessionRegistersAgainWhileItRuns(t *testing.T) {
	for _, row := range []struct {
		name    string
		returns bool
	}{
		{name: "case: the replaced session registers again just before the pass's first copy", returns: true},
		{name: "control: nothing registers while the pass runs"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			replacedToken := rig.personToken(gateReplaced, gatePerson)
			standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
			rig.registerOn(t, replacedToken, gateReplaced)
			// The claim's own handover is held, so the sweep's pass after the stop is the one the
			// seam runs in.
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld})
			read := rig.sendDirect(t, gateReplaced, "dispatch", copiesReadMark+"a message to "+gateReplaced+" it read")
			first := rig.sendDirect(t, gateReplaced, "dispatch", "the first unread message to "+gateReplaced)
			second := rig.sendDirect(t, gateReplaced, "dispatch", "the second unread message to "+gateReplaced)
			seqs := copiesSeqs(t, standIn.collect(t, 3, copiesWatchBound), read, first, second)
			awaitAckState(t, rig.consumers, gateReplaced, 2, seqs[read])

			taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
			token := rig.personToken(gateClaimant, gatePerson)
			rig.registerOn(t, token, gateClaimant)
			if warning, warned := gateWarning(rig.claimOn(t, token)); warned {
				t.Fatalf("the claim of one person's session naming that person's other session warned %q", warning)
			}
			reached := false
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld, beforeCopy: func(_, previous string, _ uint64) error {
				if previous == gateReplaced && !reached {
					reached = true
					if row.returns {
						rig.registerOn(t, replacedToken, gateReplaced)
					}
				}
				return nil
			}})
			rig.crash(t, gateReplaced, standIn)
			rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
			rig.sweepPass()
			if !reached {
				t.Fatalf("the sweep's pass reached no copy of %s's messages", gateReplaced)
			}
			received := rig.watch(t, taker, gateClaimant, gateRole, wantIf(!row.returns, first, second)...)

			if !row.returns {
				copiesRequireEach(t, received, gateClaimant, first, second)
				return
			}
			if _, state := rig.harness.sessions.Lookup(gateReplaced); state != session.Live {
				t.Fatalf("%s reads %s after it registered again; the case needs it live", gateReplaced, state)
			}
			copiesRequireNone(t, received, gateClaimant, first, second)
			for _, eventID := range []string{first, second} {
				if n := rig.handedOverCopies(t, gateClaimant, eventID); n != 0 {
					t.Errorf("%s's own subject holds %d copies of %s after %s registered again; want none", gateClaimant, n, eventID, gateReplaced)
				}
			}
			rig.requireUnacknowledged(t, gateReplaced, seqs, first, second)
		})
	}
}

// A registration that cannot record its principal first leaves the record naming the principal
// the session registered as before, which the handover gate would read as the one it last
// registered as. So the registration route answers such a registration 503 with Retry-After: 1
// rather than 200 (session.ErrPrincipalUnrecorded), before anything of the session changes
// (session.KnownSessions.AdmitRegistration): its entry keeps the title of the registration before,
// and the client registers again until the record names its principal. The session first
// registers as a service; then the bucket takes no write (its stream's largest message is one
// byte, so the record naming the service can be neither cleared nor written over) and it registers
// again, with a new title. The case is a registration as a person, which fails where it is
// answered 200, or admitted before the 503, whatever principal it leaves recorded. The controls
// are a registration as the same service, whose failed write leaves its own principal recorded and
// is answered 200, and a registration as a person through a bucket that takes the write, each
// admitted with its new title. A listener whose bucket is not open is
// TestARegistrationThroughAListenerWhoseKnownSessionBucketIsNotOpenIsAdmittedWithAWarning's.
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
			stored:    "service:" + gateService,
		},
		{
			name:      "control: the session registers again as the same service and the bucket takes no write",
			refused:   true,
			registers: func(rig *gateRig) string { return rig.serviceToken(sessionID, gateService) },
			status:    http.StatusOK,
			stored:    "service:" + gateService,
		},
		{
			name:      "control: the session registers again as a person and the bucket takes the write",
			registers: func(rig *gateRig) string { return rig.personToken(sessionID, gatePerson) },
			status:    http.StatusOK,
			stored:    "person:" + gatePerson,
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
			recorder := rig.registerTitled(t, row.registers(rig), sessionID, "a session registered again")
			if recorder.Code != row.status {
				t.Errorf("the registration answered %d %s; want %d", recorder.Code, recorder.Body.String(), row.status)
			}
			if row.status == http.StatusServiceUnavailable && recorder.Header().Get("Retry-After") != "1" {
				t.Errorf("the 503 carries Retry-After %q; want 1", recorder.Header().Get("Retry-After"))
			}
			wantTitle := "a session registered again"
			if row.status == http.StatusServiceUnavailable {
				wantTitle = "a checked session"
			}
			if entry, err := rig.harness.sessions.Get(sessionID); err != nil || entry.Title != wantTitle {
				t.Errorf("the session's entry after the registration = %+v, %v; want the title %q", entry, err, wantTitle)
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

// registerTitled registers sessionID in consumer mode with title, presenting token, and answers
// what the route answered, as it came.
func (rig *gateRig) registerTitled(t *testing.T, token, sessionID, title string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"title":           title,
		"topics":          []string{},
		"delivery":        map[string]any{"consumer": true, "epoch": "epoch-" + sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rig.postOn(t, token, "/v1/interests/subscribe", string(body))
}

// A listener whose known-session bucket is not open - its NATS grant does not carry the bucket -
// can read and write no session's record until an attempt of the store's own opens the bucket
// (session.KnownSessions says how often it attempts), which is not before the grant carries it.
// So it admits each registration, heartbeats included, with no principal recorded, and answers it
// 200 with a warning that says so (knownBucketNotOpenWarning), logged at most once a minute for
// the whole listener with the count it held back; and it refuses every claim's handover: a claim
// through it that names the session it replaces stands, is answered 200 with a warning naming
// why, and writes no handover record. Both sessions first register as one person through the
// listener whose bucket is open, so the bucket holds a record of each that a handover would be
// judged by; then the routes are served as a listener whose store has no bucket open serves them.
// The cases fail where such a registration is answered 503 - which refuses every session's
// registration and heartbeat on a listener whose grant lacks the bucket - or written, or answered
// without the warning; where each registration logs its own line, or the line after a minute does
// not count the one held back; and where the claim is answered 503 or writes a handover record.
// The control is the next registration through the store whose bucket is open, answered 200 with
// no such warning and its principal recorded. Last, a session that registered while the bucket was
// not open, and so has no record, changes its interests once the bucket is open: that write is no
// registration (recordFollowedTopics), and creates the record with no principal. A claim naming
// that session stands, is answered 200 with the warning that no registration of this build
// recorded its principal, and writes no handover record; the case fails where the warning names
// another cause than the record has, or the claim hands anything over.
func TestARegistrationThroughAListenerWhoseKnownSessionBucketIsNotOpenIsAdmittedWithAWarning(t *testing.T) {
	const line = "listener registered a session with no principal recorded"
	rig := newGateRig(t)
	token := rig.personToken(gateClaimant, gatePerson)
	rig.registerOn(t, rig.personToken(gateReplaced, gatePerson), gateReplaced)
	rig.registerOn(t, token, gateClaimant)
	open := rig.deps.known
	// The routes read the deps at each request, so what follows is served as a listener whose
	// store has no bucket open serves it; the records are read from the open store, over the same
	// bucket.
	rig.deps.known = &session.KnownSessions{}
	t.Cleanup(func() { rig.deps.known = open })
	// No other test runs beside this one (it is not parallel), so the listener-wide minute starts
	// here.
	knownBucketNotOpenWarnings.mu.Lock()
	knownBucketNotOpenWarnings.last, knownBucketNotOpenWarnings.held = time.Time{}, 0
	knownBucketNotOpenWarnings.mu.Unlock()
	before, found, err := open.Read(t.Context(), gateClaimant)
	if err != nil || !found || before.Principal != "person:"+gatePerson {
		t.Fatalf("%s's record before the registrations = %+v, found %t, %v; the case needs it naming person:%s", gateClaimant, before, found, err, gatePerson)
	}
	logged := func() []string {
		var lines []string
		for _, entry := range strings.Split(rig.harness.logs.String(), "\n") {
			if strings.Contains(entry, line) {
				lines = append(lines, entry)
			}
		}
		return lines
	}
	registerWarned := func(t *testing.T, title string) {
		t.Helper()
		recorder := rig.registerTitled(t, token, gateClaimant, title)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the registration answered %d %s; want 200", recorder.Code, recorder.Body.String())
		}
		var answer listenerwire.SubscribeResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the registration's answer %s: %v", recorder.Body.String(), err)
		}
		if !slices.Contains(answer.Warnings, knownBucketNotOpenWarning) {
			t.Errorf("the registration's warnings are %q; want %q", answer.Warnings, knownBucketNotOpenWarning)
		}
		if entry, err := rig.harness.sessions.Get(gateClaimant); err != nil || entry.Title != title {
			t.Errorf("the session's entry after the registration = %+v, %v; want the title %q", entry, err, title)
		}
	}

	t.Run("case: each registration is admitted with the warning, and writes no record", func(t *testing.T) {
		registerWarned(t, "registered with the bucket shut")
		registerWarned(t, "a heartbeat with the bucket shut")
		after, found, err := open.Read(t.Context(), gateClaimant)
		if err != nil || !found || after.Principal != before.Principal || after.LastSeen != before.LastSeen || after.Title != before.Title {
			t.Errorf("%s's record after the registrations = %+v, found %t, %v; want it as it was, %+v", gateClaimant, after, found, err, before)
		}
	})
	t.Run("case: the listener logs the registrations once a minute, counting what it held back", func(t *testing.T) {
		if lines := logged(); len(lines) != 1 || !strings.Contains(lines[0], `"level":"WARN"`) || !strings.Contains(lines[0], `"held_back":0`) {
			t.Fatalf("the listener logged %q for two registrations within a minute; want one WARN line holding back none", lines)
		}
		knownBucketNotOpenWarnings.mu.Lock()
		knownBucketNotOpenWarnings.last = time.Now().Add(-2 * time.Minute)
		knownBucketNotOpenWarnings.mu.Unlock()
		registerWarned(t, "registered a minute later")
		if lines := logged(); len(lines) != 2 || !strings.Contains(lines[1], `"held_back":1`) {
			t.Errorf("the listener logged %q once a minute had passed; want a second line holding back one", lines)
		}
	})
	t.Run("case: a claim naming the session it replaces stands, warns why, and hands nothing over", func(t *testing.T) {
		warning, warned := gateWarning(rig.claimOn(t, token))
		if !warned || !strings.Contains(warning, knownBucketNotOpenRefusal) {
			t.Errorf("the claim's warning is %q; want one naming %s and saying %q", warning, gateReplaced, knownBucketNotOpenRefusal)
		}
		if n := rig.handoverRecords(t, gateRole, gateReplaced, gateClaimant); n != 0 {
			t.Errorf("role %s holds %d handover records from %s to %s; want none", gateRole, n, gateReplaced, gateClaimant)
		}
	})
	t.Run("control: a registration through the store whose bucket is open records its principal, with no such warning", func(t *testing.T) {
		rig.deps.known = open
		recorder := rig.registerTitled(t, token, gateClaimant, "registered with the bucket open")
		if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "is not recorded, since this listener's known-session bucket") {
			t.Fatalf("the registration answered %d %s; want 200 with no warning of a shut bucket", recorder.Code, recorder.Body.String())
		}
		after, found, err := open.Read(t.Context(), gateClaimant)
		if err != nil || !found || after.Title != "registered with the bucket open" || after.Principal != "person:"+gatePerson || !after.PrincipalRecorded {
			t.Errorf("%s's record after the registration = %+v, found %t, %v; want it written, naming person:%s", gateClaimant, after, found, err, gatePerson)
		}
	})
	t.Run("case: a session whose record an interest change created after it registered with the bucket shut is refused", func(t *testing.T) {
		const shut, role, topic = "ses_gate_registered_shut", "gate-shut-role", "notifications.team.gate-shut"
		shutToken := rig.personToken(shut, gatePerson)
		rig.deps.known = &session.KnownSessions{}
		registration, err := json.Marshal(map[string]any{
			"session_id":      shut,
			"self_subscribed": true,
			"title":           "registered with the bucket shut",
			"topics":          []string{topic},
			"delivery":        map[string]any{"consumer": true, "epoch": "epoch-" + shut},
		})
		if err != nil {
			t.Fatal(err)
		}
		recorder := rig.postOn(t, shutToken, "/v1/interests/subscribe", string(registration))
		var registered listenerwire.SubscribeResponse
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &registered) != nil || !slices.Contains(registered.Warnings, knownBucketNotOpenWarning) {
			t.Fatalf("%s's registration with the bucket shut answered %d %s; want 200 with the warning %q", shut, recorder.Code, recorder.Body.String(), knownBucketNotOpenWarning)
		}
		if record, found, err := open.Read(t.Context(), shut); err != nil || found {
			t.Fatalf("%s's record after its registration with the bucket shut = %+v, found %t, %v; the case needs none", shut, record, found, err)
		}

		rig.deps.known = open
		change, err := json.Marshal(map[string]any{"session_id": shut, "topics": []string{topic}})
		if err != nil {
			t.Fatal(err)
		}
		if recorder := rig.postOn(t, shutToken, "/v1/interests/unsubscribe", string(change)); recorder.Code != http.StatusOK {
			t.Fatalf("%s's interest change answered %d %s; want 200", shut, recorder.Code, recorder.Body.String())
		}
		record, found, err := open.Read(t.Context(), shut)
		if err != nil || !found || record.Principal != "" || record.PrincipalRecorded {
			t.Fatalf("%s's record after its interest change = %+v, found %t, %v; the case needs one the change created, naming no principal", shut, record, found, err)
		}

		claim, err := json.Marshal(map[string]any{"session_id": gateClaimant, "role": role, "previous_session_id": shut})
		if err != nil {
			t.Fatal(err)
		}
		recorder = rig.postOn(t, token, "/v1/roles/set", string(claim))
		var answer roleSetResponse
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil {
			t.Fatalf("%s claims %s naming %s: status %d %s; want 200", gateClaimant, role, shut, recorder.Code, recorder.Body.String())
		}
		want := "no message of " + shut + " is handed over to this session: no registration through a listener of this build has recorded a principal for " + shut
		if !slices.Contains(answer.Warnings, want) {
			t.Errorf("the claim's warnings are %q; want %q", answer.Warnings, want)
		}
		if n := rig.handoverRecords(t, role, shut, gateClaimant); n != 0 {
			t.Errorf("role %s holds %d handover records from %s to %s; want none", role, n, shut, gateClaimant)
		}
	})
}

// A handover record waits for its replaced session for as long as that session stays live: a pass
// that finds it live sets the record back and moves the record's life on (HolderSeenAt), and never
// finishes it, so what the session leaves unread when it stops at last is handed over then. A
// claim of one person's session names that person's other session while it is live, and the
// claim's own handover is held (gateClaimDrainHeld); the sweep's pass runs past the record's life
// (handoverRecordLife) while the replaced session stays live; then it stops with an unread direct
// message, and the sweep's pass runs again. In the case the claimant receives that message; it
// fails where a pass that finds the replaced session live past the record's life finishes the
// record. The control is the same with no pass past the life, which hands the message over either
// way.
func TestAHandoverWaitsForAReplacedSessionThatStaysLivePastTheRecordsLife(t *testing.T) {
	for _, row := range []struct {
		name        string
		pastTheLife bool
	}{
		{name: "case: the replaced session stays live past the record's life, then stops", pastTheLife: true},
		{name: "control: the replaced session stops inside the record's life"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld})
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
			claimed := time.Now()
			t.Cleanup(rig.listenerClock.unpin)
			if row.pastTheLife {
				rig.listenerClock.pin(claimed.Add(handoverRecordLife + time.Minute))
				rig.sweepPass()
				if _, state := rig.harness.sessions.Lookup(gateReplaced); state != session.Live {
					t.Fatalf("%s reads %s at the pass past the record's life; the case needs it live", gateReplaced, state)
				}
			}
			rig.crash(t, gateReplaced, standIn)
			rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
			if row.pastTheLife {
				rig.listenerClock.pin(claimed.Add(handoverRecordLife + 2*time.Minute))
			}
			rig.sweepPass()
			received := rig.watch(t, taker, gateClaimant, gateRole, unread)
			copiesRequireEach(t, received, gateClaimant, unread)
			copiesRequireNone(t, received, gateClaimant, read)
		})
	}
}

// A direct message to the replaced session has no fence, as a role forward has: a sender - a
// person who answers a Dispatch ask the session made, a peer that knows its id - can write to it
// hours after the claim. So a handover record lives the stream's retention (handoverRecordLife),
// not the keep window, and a message sent after a pass past the claim's hour still reaches the
// claimant. The replaced session stops and is claimed, the claim's own handover held
// (gateClaimDrainHeld), so the sweep's passes are the record's; a pass runs at the row's time,
// then the message is sent, then a pass runs again. In the case the pass runs past the claim's
// hour, the record stays open and the claimant receives the message; it fails where a handover
// record ends with the keep window, as a takeover record does, since that pass would finish it and
// none would read the message. The control is the same past the record's life: that pass finishes
// the record, and the message stays on the replaced session's own subject, unread - the end the
// life states, and the proof that the case's watch sees a record that finished.
func TestADirectMessageSentAfterTheClaimsHourIsHandedOver(t *testing.T) {
	for _, row := range []struct {
		name   string
		after  time.Duration
		handed bool
	}{
		{name: "case: the message is sent after a pass past the claim's hour", after: takeoverRecordLife + 10*time.Minute, handed: true},
		{name: "control: the message is sent after a pass past the record's life", after: handoverRecordLife + 10*time.Minute},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld})
			standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
			rig.registerOn(t, rig.personToken(gateReplaced, gatePerson), gateReplaced)
			read := rig.sendDirect(t, gateReplaced, "dispatch", copiesReadMark+"a message to "+gateReplaced+" it read")
			seqs := copiesSeqs(t, standIn.collect(t, 1, copiesWatchBound), read)
			awaitAckState(t, rig.consumers, gateReplaced, 0, seqs[read])
			rig.crash(t, gateReplaced, standIn)
			rig.sessionClock.advance(session.LiveWindow + 30*time.Second)

			taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
			token := rig.personToken(gateClaimant, gatePerson)
			rig.registerOn(t, token, gateClaimant)
			if warning, warned := gateWarning(rig.claimOn(t, token)); warned {
				t.Fatalf("the claim of one person's session naming that person's other session warned %q", warning)
			}
			claimed := time.Now()
			t.Cleanup(rig.listenerClock.unpin)
			rig.listenerClock.pin(claimed.Add(row.after))
			rig.sweepPass()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			records, err := rig.harness.client.TakeoverRecords(ctx, gateRole)
			if err != nil {
				t.Fatalf("read role %s's takeover records: %v", gateRole, err)
			}
			subject := bus.HandoverSubject(gateRole, gateReplaced, gateClaimant)
			i := slices.IndexFunc(records, func(stored bus.StoredTakeoverRecord) bool { return stored.Subject == subject })
			if i < 0 || records[i].Record.Finished == row.handed {
				t.Fatalf("the handover record %s after the pass %s past the claim = %+v; want it finished %t", subject, row.after, records, !row.handed)
			}

			unread := rig.sendDirect(t, gateReplaced, "dispatch", "a message to "+gateReplaced+" sent after the pass")
			rig.listenerClock.pin(claimed.Add(row.after + 5*time.Minute))
			rig.sweepPass()
			received := rig.watch(t, taker, gateClaimant, gateRole, wantIf(row.handed, unread)...)
			copiesRequireNone(t, received, gateClaimant, read)
			if row.handed {
				copiesRequireEach(t, received, gateClaimant, unread)
				return
			}
			copiesRequireNone(t, received, gateClaimant, unread)
			if n := rig.handedOverCopies(t, gateClaimant, unread); n != 0 {
				t.Errorf("%s's own subject holds %d copies of %s; want none, as the record finished before it was sent", gateClaimant, n, unread)
			}
		})
	}
}

// Where the replaced session's floor is unknown - its session consumer gone with no record of
// where it read to, as the server's removal of an inactive consumer leaves it - a handover's pass
// starts at the first message the stream holds on that session's subject, not an hour before the
// claim as a role takeover's does: a message to a session has no hour, and one stored more than an
// hour before the claim and never read would otherwise never reach the claimant. The claim's own
// handover is held (gateClaimDrainHeld), and the sweep's first pass finds no floor and leaves the
// read of the consumer to its next. In the case the claim's time is two hours after the unread
// message was stored, and the claimant receives it; it fails where the pass starts an hour before
// the claim. The control is the same claim made at once, which both starts reach.
func TestAHandoverWhoseFloorIsUnknownStartsAtTheFirstMessageTheStreamHolds(t *testing.T) {
	for _, row := range []struct {
		name  string
		later time.Duration
	}{
		{name: "case: the claim comes two hours after the unread message", later: 2 * time.Hour},
		{name: "control: the claim comes at once"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newGateRig(t)
			setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: gateClaimDrainHeld})
			standIn := startCopiesStandIn(t, rig.url, gateReplaced, copiesHolderAnswer)
			rig.registerOn(t, rig.personToken(gateReplaced, gatePerson), gateReplaced)
			unread := rig.sendDirect(t, gateReplaced, "dispatch", "an unread message to "+gateReplaced)
			copiesSeqs(t, standIn.collect(t, 1, copiesWatchBound), unread)
			rig.crash(t, gateReplaced, standIn)
			rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
			// The server's removal of an inactive consumer: a delete of the consumer itself, which
			// records no floor.
			name, _ := bus.SessionConsumerName(gateReplaced)
			if err := rig.harness.client.JS().DeleteConsumer(bus.Stream, name); err != nil {
				t.Fatalf("delete %s's consumer %s: %v", gateReplaced, name, err)
			}
			rig.awaitNoConsumer(t, gateReplaced)
			if record, recorded := rig.resumeRecord(t, gateReplaced); recorded {
				t.Fatalf("%s has a resume record %+v; the row needs none", gateReplaced, record)
			}

			taker := startCopiesStandIn(t, rig.url, gateClaimant, answerDone)
			token := rig.personToken(gateClaimant, gatePerson)
			rig.registerOn(t, token, gateClaimant)
			t.Cleanup(rig.listenerClock.unpin)
			if row.later > 0 {
				rig.listenerClock.pin(time.Now().Add(row.later))
			}
			if warning, warned := gateWarning(rig.claimOn(t, token)); warned {
				t.Fatalf("the claim of one person's session naming that person's other session warned %q", warning)
			}
			rig.sweepPass()
			rig.sweepPass()
			received := rig.watch(t, taker, gateClaimant, gateRole, unread)
			copiesRequireEach(t, received, gateClaimant, unread)
		})
	}
}
