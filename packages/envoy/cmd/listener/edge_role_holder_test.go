package main

// The edge names a role's holder only when that holder is a session of the reader's projects
// (sessionOfProjects): a role of the reader's project can be claimed by any session, and its
// claim record can be written by any writer of the role bucket, so a holder outside the reader's
// projects is no holder the edge names, in a role read or in the answers of a role send.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// requireNamesNone fails the test unless body, an answer to asked, names none of names and has no
// field naming a holder, its process, its state or its last registration, nor a kept reason that
// says a holder lapsed.
func requireNamesNone(t *testing.T, asked, body string, names ...string) {
	t.Helper()
	for _, name := range names {
		if strings.Contains(body, name) {
			t.Errorf("%s answers %s, which names %s", asked, body, name)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("%s: decode %s: %v", asked, body, err)
	}
	for _, field := range []string{"holder", "holder_state", "client_instance_id", "last_seen", "handed_back"} {
		if value, ok := fields[field]; ok && string(value) != `""` {
			t.Errorf("%s answers %s, whose %s is %s: a holder outside the reader's projects is dated or named", asked, body, field, value)
		}
	}
	if reason, ok := fields["kept_reason"]; ok && string(reason) != `"`+string(roles.Unclaimed)+`"` {
		t.Errorf("%s answers %s, whose kept_reason is %s: a holder outside the reader's projects is said to stand", asked, body, reason)
	}
}

// A PRIV role's claim names S_pub, a live in-house session of PUB, under a distinctive process,
// and another PRIV role's claim names S_gone, a PUB session with no record left. To S_priv, an
// outside session of PRIV, each role reads as one no claim holds: GET /v1/roles/<role> is the
// 404 of an unclaimed role, with no holder, process, state, last registration or claim revision,
// and its text names neither session; a role send to either role names no holder in its answer;
// and the 409 of a claim of S_pub's role, a soft one or one naming the revision 0 the read gave,
// names no holder either. The positive control: a third PRIV role held by S_peer, a session of
// PRIV, is answered whole to the same token, a send to it names S_peer, and so does the 409 of a
// soft claim of it.
func TestAnEdgeRoleAnswerNamesNoForeignHolder(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, peer, foreign, gone = "ses_edge_answer_reader", "ses_edge_answer_peer", "ses_edge_answer_pub", "ses_edge_answer_pub_gone"
	const foreignInstance, goneInstance = "instance-of-the-pub-process", "instance-of-the-gone-process"
	const foreignRole, goneRole, peerRole = "legion-priv-priv-6-tester", "legion-priv-priv-7-tester", "legion-priv-priv-8-tester"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	rig.placeElsewhere(t, foreign, "PUB")
	for _, claim := range []store.RoleClaimRequest{
		{SessionID: foreign, MachineID: "shared-listener", ClientInstanceID: foreignInstance, Role: foreignRole},
		{SessionID: gone, MachineID: "shared-listener", ClientInstanceID: goneInstance, Role: goneRole},
		{SessionID: peer, MachineID: "test-machine", Role: peerRole},
	} {
		if _, err := rig.deps.registry.SetRoleWithPrevious(context.Background(), claim); err != nil {
			t.Fatalf("claim %s for %s: %v", claim.Role, claim.SessionID, err)
		}
	}
	names := []string{foreign, gone, foreignInstance, goneInstance}

	for _, role := range []string{foreignRole, goneRole} {
		asked := "GET /v1/roles/" + role
		recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+role, "")
		requireNamesNone(t, asked, recorder.Body.String(), names...)
		var refusal listenerwire.RoleRefusal
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		if recorder.Code != http.StatusNotFound || refusal.Reason != roles.Unclaimed || refusal.ClaimRevision != 0 || refusal.ClaimReleased != nil {
			t.Errorf("%s: status %d %s, want the 404 of a role no claim holds: reason unclaimed, claim revision 0",
				asked, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		}
	}

	recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+peerRole, "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/roles/"+peerRole)
	var whole listenerwire.RoleHolder
	if err := json.Unmarshal(recorder.Body.Bytes(), &whole); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if whole.Holder != peer || whole.HolderState != session.Live || whole.MachineID == "" || whole.Host == "" || whole.LastSeen == nil || whole.ClaimRevision == 0 {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s whole: live, with its machine, host, last registration and claim revision", peerRole, whole, peer)
	}

	send := func(role, keep string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"topic":%q,"source":"agent","source_session":%q,"message":"to the role"%s}`,
			contracts.RoleTopicPrefix+"PRIV."+role, reader, keep)
		recorder := rig.call(t, http.MethodPost, token, "/v1/messages/publish", body)
		t.Logf("the send to %s%s: status %d %s", role, keep, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		return recorder.Code, recorder.Body.String()
	}
	for _, sent := range []struct{ role, keep string }{
		{foreignRole, ""},
		{goneRole, ""},
		{goneRole, `,"keep":false`},
	} {
		status, body := send(sent.role, sent.keep)
		if status/100 != 2 && status != http.StatusNotFound {
			t.Errorf("the send to %s%s: status %d %s, want it delivered, kept or refused as a role no claim holds", sent.role, sent.keep, status, body)
		}
		requireNamesNone(t, "the send to "+sent.role+sent.keep, body, names...)
	}
	status, body := send(peerRole, "")
	var delivered listenerwire.PublishResponse
	_ = json.Unmarshal([]byte(body), &delivered)
	if status != http.StatusOK || delivered.Holder != peer || delivered.HolderState != session.Live {
		t.Errorf("the send to %s: status %d %s, want it delivered to %s, named live", peerRole, status, body, peer)
	}

	claim := func(body string) (int, string) {
		t.Helper()
		recorder := rig.call(t, http.MethodPost, token, "/v1/roles/set", body)
		t.Logf("POST /v1/roles/set %s: status %d %s", body, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		return recorder.Code, recorder.Body.String()
	}
	for _, body := range []string{
		fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, reader, foreignRole),
		fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, reader, foreignRole),
	} {
		status, answer := claim(body)
		if status != http.StatusConflict {
			t.Errorf("POST /v1/roles/set %s: status %d %s, want 409: the role is held", body, status, answer)
		}
		requireNamesNone(t, "POST /v1/roles/set "+body, answer, names...)
	}
	if status, answer := claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, reader, peerRole)); status != http.StatusConflict || !strings.Contains(answer, `"holder":"`+peer+`"`) {
		t.Errorf("a soft claim of %s: status %d %s, want the 409 naming %s, a session of PRIV", peerRole, status, answer, peer)
	}
}
