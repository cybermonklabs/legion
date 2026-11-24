//go:build natslive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// A leader read that confirms a session's stored record (edgeScope.showsConfirmed,
// sessionInProject) and does not answer is the bus-unavailable 503 at every edge route that reads
// one, and that 503 names no session: the read's own error names the session it read, which the
// read did not confirm and the request did not name (edgeScope.confirmUnanswered). The session is
// an in-house session of PUB whose record the server re-published listing PRIV, so the cache shows
// it to a PRIV token while the stream's leader holds no writer's record of it. It holds a PRIV role
// and is the replacement of a PRIV Legion agent. Each route is asked twice by an outside session of
// PRIV, the foreign session's leader read driven through the registry's seam:
//   - answering, as the server-written record it is: the route reads the confirmation and hides the
//     session (the control: a 503 under the fault is that read's, and the route shows nothing of the
//     session when the read answers);
//   - not answering: the route's 503, with Retry-After: 1, whose whole body names no foreign
//     session.
//
// The routes are the role read, both 409 branches of a claim (a soft claim of a held role, and a
// claim naming a revision the claim is not at), the session list, the Legion issue listing, the
// Legion role-token listing, and a Legion agent's session read through its replacement. The role
// send, which forwards to the holder whatever it names, answers either way naming no holder
// (roleSend.shownHolder reads a failed confirmation as a role no claim holds). Control: with the
// confirmation's error passed to the answer as it is (edgeScope.showsConfirmed and
// showsAgentConfirmed returning err), every 503 names the foreign session and the row fails.
func TestAnEdgeConfirmationThatDoesNotAnswerNamesNoSession(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const reader, foreign, older = "ses_unanswered_reader", "ses_unanswered_foreign_pub", "ses_unanswered_older_priv"
	const role, issue, roleToken = "legion-priv-priv-11-tester", "PRIV-12", "legion-priv-priv-12-tester"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	// The foreign session's cached record lists PRIV, as a record the server re-published there
	// does; the seam below makes its leader read the server-written record it is.
	rig.placeElsewhere(t, foreign, "PRIV")
	rig.placeElsewhere(t, older, "PRIV")
	if _, err := rig.deps.registry.SetRoleWithPrevious(context.Background(), store.RoleClaimRequest{SessionID: foreign, MachineID: "shared-listener", Role: role}); err != nil {
		t.Fatalf("claim %s for %s: %v", role, foreign, err)
	}
	putEdgeLegionAgent(t, rig, issue, roleToken, older, "2026-11-20T00:00:00Z")
	putEdgeLegionAgent(t, rig, issue, roleToken, foreign, "2026-11-20T01:00:00Z")

	answering := func() func() {
		return rig.deps.sessions.SetStoredByWriterSeamForTest(foreign, session.SessionEntry{}, false, nil)
	}
	unanswered := func() func() {
		return rig.deps.sessions.SetStoredByWriterSeamForTest(foreign, session.SessionEntry{}, false, leaderReadUnanswered(foreign))
	}
	sent := fmt.Sprintf(`{"topic":%q,"source":"agent","source_session":%q,"message":"to the role"}`, contracts.RoleTopicPrefix+"PRIV."+role, reader)

	for _, route := range []struct {
		name, method, path, body string
		// control checks the answer with the foreign session's leader read answering.
		control func(t *testing.T, recorder *httptest.ResponseRecorder)
		// prefix is the 503's own wording under the fault, "" for a route that answers it none.
		prefix string
	}{
		{"the role read", http.MethodGet, "/v1/roles/" + role, "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusNotFound, "the role read of a role a server-written record holds")
		}, busUnansweredPrefix},
		{"a soft claim's 409", http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, reader, role), func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusConflict, "the soft claim of a held role")
		}, busUnansweredPrefix},
		{"a revision claim's 409", http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, reader, role), func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusConflict, "the claim at revision 0 of a held role")
		}, busUnansweredPrefix},
		{"the session list", http.MethodGet, "/v1/sessions", "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusOK, "the session list")
			if !strings.Contains(rec.Body.String(), reader) {
				t.Fatalf("the list %s leaves out the reader itself: the control cannot tell a hidden session from an empty list", rec.Body.String())
			}
		}, busUnansweredPrefix},
		{"the Legion issue listing", http.MethodGet, "/v1/legion/agents?issue=" + issue, "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusOK, "the Legion issue listing")
			if shown := legionSessionIDs(t, rec); !slices.Equal(shown, []string{older}) {
				t.Fatalf("the issue listing shows %v, want %s alone", shown, older)
			}
		}, "envoy: the legion agent registry did not answer"},
		{"the Legion role-token listing", http.MethodGet, "/v1/legion/agents/roles/" + roleToken, "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusOK, "the Legion role-token listing")
			if shown := legionSessionIDs(t, rec); !slices.Equal(shown, []string{older}) {
				t.Fatalf("the role-token listing shows %v, want %s alone", shown, older)
			}
		}, "envoy: the legion agent registry did not answer"},
		{"a Legion agent's replacement", http.MethodGet, "/v1/legion/agents/sessions/" + older, "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			requireStatus(t, rec, http.StatusOK, "the Legion session read of "+older)
			var answer listenerwire.LegionAgentAnswer
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if answer.SessionID != older || answer.ReplacedBy != nil {
				t.Fatalf("the session read answers %s replaced_by %v, want %s with no replacement shown", answer.SessionID, answer.ReplacedBy, older)
			}
		}, "envoy: the legion agent registry did not answer"},
		{"the role send", http.MethodPost, "/v1/messages/publish", sent, nil, ""},
	} {
		t.Run(route.name, func(t *testing.T) {
			restore := answering()
			ok := rig.call(t, route.method, token, route.path, route.body)
			restore()
			t.Logf("%s with the leader read answering: status %d %s", route.name, ok.Code, strings.TrimSpace(ok.Body.String()))
			requireBodyNamesNone(t, route.name+" with the leader read answering", ok.Body.String(), foreign)
			if route.control != nil {
				route.control(t, ok)
			}

			restore = unanswered()
			fault := rig.call(t, route.method, token, route.path, route.body)
			restore()
			body := strings.TrimSpace(fault.Body.String())
			t.Logf("%s with the leader read unanswered: status %d %s", route.name, fault.Code, body)
			requireBodyNamesNone(t, route.name+" with the leader read unanswered", body, foreign)
			if route.prefix == "" {
				return
			}
			var refusal listenerwire.APIError
			_ = json.Unmarshal(fault.Body.Bytes(), &refusal)
			if fault.Code != http.StatusServiceUnavailable || fault.Header().Get("Retry-After") != "1" || !strings.HasPrefix(refusal.Error, route.prefix) {
				t.Errorf("%s with the leader read unanswered: status %d, Retry-After %q, %s; want the 503 %q with Retry-After: 1",
					route.name, fault.Code, fault.Header().Get("Retry-After"), body, route.prefix)
			}
		})
	}
}

// requireBodyNamesNone fails the test unless body, the whole answer to asked, names none of names:
// requireNamesNone for an object, which also reads its holder fields, and the text alone for any
// other answer, a list among them.
func requireBodyNamesNone(t *testing.T, asked, body string, names ...string) {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		requireNamesNone(t, asked, body, names...)
		return
	}
	for _, name := range names {
		if strings.Contains(body, name) {
			t.Errorf("%s answers %s, which names %s", asked, body, name)
		}
	}
}
