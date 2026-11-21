package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// The census cells M3' and M6 (the nats-security census, census-cells.md, "M3' (X3') and the role
// part of M6"), read from its setup (harness census_test.go, rowM3p): O is a PRIV session whose
// token lists PRIV, S_pub an in-house session whose token lists PUB (an earlier broker's token, so
// no restricted claim), and a PRIV peer whose token lists PRIV sends on PRIV's role lanes. The
// roles are Legion roles of PRIV (legion-priv-priv-<k>-…), whose claim key is the role itself.
// Each row runs on a listener in the required mode, the head, and in the all-public mode, which
// answers as #4691's base does (TestWithNoPrivateProjectEveryListenerAnswerIsAsBefore): the
// control, under which each row reproduces what the census measured.

// censusParty is a session of the census rows: its token, and the stand-in its frames reach.
type censusParty struct {
	id, token string
	standIn   *sessionStandIn
}

// censusSession registers id under a token the broker answers as an earlier broker's (no
// restricted claim) listing projects, and returns the party.
func censusSession(t *testing.T, rig *privacyRig, id string, projects ...string) censusParty {
	t.Helper()
	party := censusParty{id: id, token: rig.personToken(id, projects...), standIn: startSessionStandIn(t, rig.url, id, answerDone)}
	requireStatus(t, rig.subscribe(t, party.token, id), http.StatusOK, "the registration of "+id)
	return party
}

// heartbeat registers party again, as its client's heartbeat does.
func (p censusParty) heartbeat(t *testing.T, rig *privacyRig) {
	t.Helper()
	requireStatus(t, rig.subscribe(t, p.token, p.id), http.StatusOK, "the heartbeat of "+p.id)
}

// sharedCall sends a request with no broker token, as the census's in-house calls through the
// shared listener did with its shared bearer: the listener's mux behind the credential, which
// answers such a caller as no member list holds it (scopeOf).
func sharedCall(t *testing.T, rig *privacyRig, method, path, body string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rig.mux.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// sendOnRole publishes one message marked marker on PRIV's lane of role as the peer, keep false.
func sendOnRole(t *testing.T, rig *privacyRig, peer censusParty, role, marker string) int {
	t.Helper()
	body := fmt.Sprintf(`{"topic":%q,"message":%q,"source":"agent","source_session":%q,"keep":false}`, contracts.RoleSubject("PRIV", role), marker, peer.id)
	return rig.call(t, http.MethodPost, peer.token, "/v1/messages/publish", body).Code
}

// markersWithin is every marker of markers whose message reached standIn within window.
func markersWithin(standIn *sessionStandIn, window time.Duration, markers ...string) map[string]bool {
	got := map[string]bool{}
	deadline := time.After(window)
	for {
		select {
		case frame := <-standIn.frames:
			for _, marker := range markers {
				if strings.Contains(string(frame.msg.Data), marker) {
					got[marker] = true
				}
			}
		case <-deadline:
			return got
		}
	}
}

// claimHolder is the session the role bucket's record of role names, "" for none.
func claimHolder(t *testing.T, rig *privacyRig, role string) string {
	t.Helper()
	claim, err := rig.deps.registry.RoleClaim(t.Context(), role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim.HolderSessionID
}

// putClaimRecord writes data as role's record in the role bucket, as the census's writer did: a
// plain publish of a record no claim route wrote.
func putClaimRecord(t *testing.T, rig *privacyRig, role string, data []byte) {
	t.Helper()
	kv, err := rig.harness.client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	if _, err := kv.Put(role, data); err != nil {
		t.Fatalf("write %s's record: %v", role, err)
	}
}

// TestCensusCellM3pAnInHousePUBSessionGetsNoPRIVRole: census cell M3'. In the required mode an
// in-house session whose token lists PUB holds no role of private PRIV: its own claim of one
// through the shared listener is refused 403 project_not_listed and writes nothing (the untouched
// form), and a claim record written for it at an unheld PRIV role - a plain-text record naming it
// (CHOSEN), or a copy of its own claim of a PUB role (COPY) - makes it no holder, so a PRIV peer's
// message on each of those lanes reaches it not at all. Control, the all-public mode (#4691's
// base): the census's measurement, the claim 200 and S_pub receiving the message of each lane.
func TestCensusCellM3pAnInHousePUBSessionGetsNoPRIVRole(t *testing.T) {
	for _, required := range []bool{true, false} {
		t.Run(fmt.Sprintf("required %v", required), func(t *testing.T) {
			rig := newPrivacyRig(t, required)
			rig.source.answer(privacyListingPRIV)
			k := "31"
			if !required {
				k = "32"
			}
			pub := censusSession(t, rig, "ses_pub_"+k, "PUB")
			peer := censusSession(t, rig, "ses_peer_"+k, "PRIV")
			roleA := "legion-priv-priv-" + k + "-reviewer"
			roleB := "legion-priv-priv-" + k + "-architect"
			roleC := "legion-priv-priv-" + k + "-planner"
			pubRole := "legion-pub-pub-" + k + "-tester"

			status, body := sharedCall(t, rig, http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, pub.id, pubRole))
			if status != http.StatusOK {
				t.Fatalf("S_pub's claim of its own project's %s: %d %s", pubRole, status, body)
			}
			status, body = sharedCall(t, rig, http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, pub.id, roleA))
			if required && (status != http.StatusForbidden || !strings.Contains(body, projectNotListed) || claimHolder(t, rig, roleA) != "") {
				t.Errorf("required: S_pub's claim of PRIV's %s answered %d %s with holder %q; want 403 %s and no claim", roleA, status, body, claimHolder(t, rig, roleA), projectNotListed)
			}
			if !required && status != http.StatusOK {
				t.Fatalf("control: S_pub's claim of PRIV's %s answered %d %s; the census measured 200", roleA, status, body)
			}
			putClaimRecord(t, rig, roleB, []byte(pub.id))
			copied, err := rig.harness.client.JS().KeyValue(bus.RoleClaimBucket)
			if err != nil {
				t.Fatalf("open the role bucket: %v", err)
			}
			source, err := copied.Get(pubRole)
			if err != nil {
				t.Fatalf("read S_pub's claim of %s to copy: %v", pubRole, err)
			}
			putClaimRecord(t, rig, roleC, source.Value())

			markers := map[string]string{roleA: "m3p-" + k + "-untouched", roleB: "m3p-" + k + "-chosen", roleC: "m3p-" + k + "-copy"}
			for role, marker := range markers {
				sendOnRole(t, rig, peer, role, marker)
			}
			got := markersWithin(pub.standIn, 7*time.Second, markers[roleA], markers[roleB], markers[roleC])
			for role, marker := range markers {
				if required && got[marker] {
					t.Errorf("required: S_pub, whose token lists PUB, received the message of PRIV's %s", role)
				}
				if !required && !got[marker] {
					t.Errorf("control: S_pub did not receive the message of PRIV's %s; the census measured it CROSSED", role)
				}
			}
		})
	}
}

// TestCensusCellM6APRIVSessionWinsItsRoleBackFromAForgedHolder: census cell M6. O holds PRIV's
// role; a plain-text record naming S_pub (token PUB) is written over O's claim; both sessions
// heartbeat, and the record still names S_pub (S_pub's own renewal renews it, as the census
// measured). In the required mode S_pub is no holder of the role, since its token does not list
// PRIV: a PRIV peer's message on the role reaches S_pub not at all, and O's soft claim takes the
// role back (200), after which the peer's next message reaches O. Control, the all-public mode
// (#4691's base): the census's measurement, the message reaching S_pub and O's claim refused, the
// plain-text holder kept through both heartbeats.
func TestCensusCellM6APRIVSessionWinsItsRoleBackFromAForgedHolder(t *testing.T) {
	for _, required := range []bool{true, false} {
		t.Run(fmt.Sprintf("required %v", required), func(t *testing.T) {
			rig := newPrivacyRig(t, required)
			rig.source.answer(privacyListingPRIV)
			k := "61"
			if !required {
				k = "62"
			}
			owner := censusSession(t, rig, "ses_priv_"+k, "PRIV")
			pub := censusSession(t, rig, "ses_pub_"+k, "PUB")
			peer := censusSession(t, rig, "ses_peer_"+k, "PRIV")
			role := "legion-priv-priv-" + k + "-tester"
			claim := fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, owner.id, role)
			requireStatus(t, rig.call(t, http.MethodPost, owner.token, "/v1/roles/set", claim), http.StatusOK, "O's claim of PRIV's role")
			sendOnRole(t, rig, peer, role, "m6-"+k+"-held")
			if got := markersWithin(owner.standIn, 7*time.Second, "m6-"+k+"-held"); !got["m6-"+k+"-held"] {
				t.Fatal("O, holding PRIV's role, did not receive the peer's message on it: the instrument cannot see a delivery")
			}

			putClaimRecord(t, rig, role, []byte(pub.id))
			if decoded, err := store.DecodeRoleClaim([]byte(pub.id)); err != nil || decoded.HolderSessionID != pub.id {
				t.Fatalf("the plain-text record decodes as %+v, %v; the census read it as a bare holder id", decoded, err)
			}
			owner.heartbeat(t, rig)
			pub.heartbeat(t, rig)
			if holder := claimHolder(t, rig, role); holder != pub.id {
				t.Fatalf("after both heartbeats the record names %q, want %s, as the census measured", holder, pub.id)
			}

			sendOnRole(t, rig, peer, role, "m6-"+k+"-forged")
			got := markersWithin(pub.standIn, 7*time.Second, "m6-"+k+"-forged")
			back := rig.call(t, http.MethodPost, owner.token, "/v1/roles/set", claim)
			if !required {
				if !got["m6-"+k+"-forged"] {
					t.Error("control: S_pub did not receive the message of the role written over O's claim; the census measured it CROSSED")
				}
				if back.Code == http.StatusOK || claimHolder(t, rig, role) != pub.id {
					t.Errorf("control: O's soft claim answered %d with holder %q; the census measured O not winning its role back", back.Code, claimHolder(t, rig, role))
				}
				return
			}
			if got["m6-"+k+"-forged"] {
				t.Error("required: S_pub, whose token lists PUB, received the message of PRIV's role written over O's claim")
			}
			if back.Code != http.StatusOK || claimHolder(t, rig, role) != owner.id {
				t.Fatalf("required: O's soft claim answered %d %s with holder %q; want 200 and O holding its role again", back.Code, back.Body.String(), claimHolder(t, rig, role))
			}
			sendOnRole(t, rig, peer, role, "m6-"+k+"-back")
			if got := markersWithin(owner.standIn, 7*time.Second, "m6-"+k+"-back"); !got["m6-"+k+"-back"] {
				t.Error("required: O, holding its role again, did not receive the peer's next message on it")
			}
		})
	}
}
