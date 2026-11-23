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
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// republishSessionRecord stores, by a RePublish of another stream, a session record at sessionID's
// key that names the key, a machine, a time and projects, with that stream's headers, and waits
// until the registry's cache has taken it as a record listing projects[0]. A direct get and the
// cache read it as a registration of sessionID; only the stream's leader's headers tell it from a
// writer's record (session.SessionRegistry.StoredByWriter).
func republishSessionRecord(t *testing.T, rig *edgeRig, sessionID string, projects ...string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "machine_id": "shared-listener", "updated_at": time.Now().UnixMilli(),
		"projects": projects, "self_subscribed": true, "title": "a title its writer chose"})
	if err != nil {
		t.Fatalf("encode the republished record: %v", err)
	}
	testnats.RepublishesKey(t, rig.harness.client.Conn, bus.SessionBucket, sessionID, body)
	testwait.Eventually(t, "the session cache to take the republished record of "+sessionID, func() bool {
		entry, _ := rig.deps.sessions.Lookup(sessionID)
		return len(projects) > 0 && slices.Contains(entry.Projects, projects[0])
	})
}

// R1: the edge's role-holder answer confirms the holder's record by the leader read, so a server-
// written record that names an in-house session of another project reads as no holder. The role is
// held by an in-house PUB session; the outside reader's token lists PRIV. Control: with no RePublish
// the role reads unclaimed, since the holder is a session of a project the reader's token does not
// list. After a RePublish of the holder's record listing PRIV, the answer must still name no holder:
// before the confirmation it named the in-house holder and its process.
func TestAnEdgeRoleHolderOverARepublishedRecordNamesNoHolder(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const reader, foreign, role = "ses_rh_reader", "ses_rh_foreign", "legion-priv-priv-6-tester"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	rig.placeElsewhere(t, foreign, "PUB")
	if _, err := rig.deps.registry.SetRoleWithPrevious(context.Background(), store.RoleClaimRequest{
		SessionID: foreign, MachineID: "shared-listener", ClientInstanceID: "instance-foreign", Role: role}); err != nil {
		t.Fatalf("claim %s for %s: %v", role, foreign, err)
	}
	read := func(when string) *httptest.ResponseRecorder {
		t.Helper()
		return rig.call(t, http.MethodGet, token, "/v1/roles/"+role, "")
	}
	if rec := read("control"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), foreign) {
		t.Fatalf("control: GET /v1/roles/%s = %d %s, want 404 naming no foreign holder", role, rec.Code, rec.Body.String())
	}
	republishSessionRecord(t, rig, foreign, "PRIV")
	if rec := read("after the RePublish"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), foreign) {
		t.Errorf("after a RePublish of %s's record listing PRIV, GET /v1/roles/%s = %d %s, names the foreign holder: the role answer took the cached entry unconfirmed", foreign, role, rec.Code, rec.Body.String())
	}
}

// R2: a role claim's refresh of the claimant's registration (roleSetHandler), made through the
// claimant's own listener in no edge mode, must not write a RePublished record's projects back as a
// writer's record. An in-house PUB session's key holds a RePublished record listing PRIV, then that
// session claims a role through the shared listener. Control in the row: after the RePublish and
// before the claim, the edge hides the session. After the claim the edge must still hide it: the
// refresh confirms the stored record by the leader read on every listener, and writes the claim's
// token's projects (none, for an in-house claim) when it is no writer's.
func TestAnEdgeHidesASessionAClaimRefreshedFromARepublishedRecord(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const reader, inHouse = "ses_cr_reader", "ses_cr_in_house"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	rig.placeElsewhere(t, inHouse, "PUB")
	listed := func() []string {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, "/v1/sessions", "")
		requireStatus(t, recorder, http.StatusOK, "GET /v1/sessions")
		var rows []listenerwire.SessionInfo
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		ids := []string{}
		for _, row := range rows {
			ids = append(ids, row.SessionID)
		}
		return ids
	}
	republishSessionRecord(t, rig, inHouse, "PRIV")
	if ids := listed(); slices.Contains(ids, inHouse) {
		t.Fatalf("control: the edge lists %s over the RePublished record before any claim: %v", inHouse, ids)
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(fmt.Sprintf(`{"session_id":%q,"role":"cr-free-role","soft":true}`, inHouse))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the in-house session's claim through a listener in no edge mode: status %d %s", recorder.Code, recorder.Body.String())
	}
	if ids := listed(); slices.Contains(ids, inHouse) {
		t.Errorf("after the in-house session's role claim, the edge lists %s (%v): the claim's refresh wrote the RePublished record's PRIV back as a writer's record", inHouse, ids)
	}
	send := rig.call(t, http.MethodPost, token, "/v1/messages/send",
		fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, inHouse, reader))
	if send.Code != http.StatusNotFound {
		t.Errorf("after the claim, the outside session's send to %s answers %d %s, want 404", inHouse, send.Code, strings.TrimSpace(send.Body.String()))
	}
}

// R6: the edge shows no gone Legion agent of the token's own project, as the README and the
// changelog say. Two agents under one PRIV issue: a live PRIV session and a gone one. The listing
// answers the live agent alone. Before the gone check the gone agent's record is a writer's too, so
// it was shown; the control is the live agent, which the listing must keep.
func TestAnEdgeShowsNoGoneLegionAgentOfTheTokensProject(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, live, gone, issue, roleToken = "ses_ga_outside", "ses_ga_live", "ses_ga_gone", "PRIV-9", "legion-priv-priv-9-tester"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, live, "PRIV")
	sessions, err := rig.harness.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	record, err := json.Marshal(map[string]any{"session_id": gone, "machine_id": "shared-listener", "projects": []string{"PRIV"},
		"updated_at": time.Now().Add(-3 * time.Hour).UnixMilli(), "self_subscribed": true})
	if err != nil {
		t.Fatalf("encode the gone record: %v", err)
	}
	if _, err := sessions.Put(gone, record); err != nil {
		t.Fatalf("store %s's record: %v", gone, err)
	}
	testwait.Eventually(t, "the session cache to hold "+gone, func() bool {
		entry, state := rig.deps.sessions.Lookup(gone)
		return entry.SessionID == gone && state == session.Gone
	})
	raw, err := rig.harness.client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.LegionAgentBucket, err)
	}
	for _, agentSession := range []string{live, gone} {
		entry := legionagents.Entry{SessionID: agentSession, LegionProject: "priv", Issue: issue, Role: "tester", RoleToken: roleToken,
			Title: "Legion tester " + agentSession, Harness: "omp", StartedAt: "2026-11-20T00:00:00Z", RecordedAt: "2026-11-20T00:00:00Z"}
		value, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encode %s's agent entry: %v", agentSession, err)
		}
		for _, key := range []string{"issue." + issue + "." + agentSession, "role." + roleToken + "." + agentSession, "session." + agentSession} {
			if _, err := raw.Put(key, value); err != nil {
				t.Fatalf("store %s: %v", key, err)
			}
		}
	}
	recorder := rig.call(t, http.MethodGet, token, "/v1/legion/agents?issue="+issue, "")
	requireStatus(t, recorder, http.StatusOK, "the issue listing")
	var answer listenerwire.LegionAgentList
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	shown := []string{}
	for _, entry := range answer.Entries {
		shown = append(shown, entry.SessionID)
	}
	if slices.Contains(shown, gone) {
		t.Errorf("the edge shows the agent of %s, whose session is gone, against the README's 'a gone agent of the token's own project is not shown': %v", gone, shown)
	}
	if !slices.Contains(shown, live) {
		t.Errorf("the edge hides the agent of the live session %s: %v (the control cannot fail if nothing is shown)", live, shown)
	}
}

// R9: the registry routes confirm each session they answer by the leader read, so a RePublished
// record is answered 404, as the full view and the send route are. A live in-house PRIV session is
// the control, which both routes must answer 200. Covers the registration view (?view=registration)
// and the full view, whose confirmations the reviewer's mutation left with no failing test.
func TestAnEdgeHidesARepublishedRecordOnTheRegistryRoutes(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, forged, live = "ses_reg_outside", "ses_reg_forged", "ses_reg_live"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, live, "PRIV")
	rig.placeElsewhere(t, forged, "PUB")
	republishSessionRecord(t, rig, forged, "PRIV")
	for _, route := range []struct{ name, path string }{
		{"the full view", "/v1/registry/"},
		{"the registration view", "/v1/registry/"},
	} {
		suffix := ""
		if route.name == "the registration view" {
			suffix = "?view=registration"
		}
		if rec := rig.call(t, http.MethodGet, token, route.path+live+suffix, ""); rec.Code != http.StatusOK {
			t.Fatalf("control, %s of the live session %s: status %d (%s), want 200", route.name, live, rec.Code, rec.Body.String())
		}
		if rec := rig.call(t, http.MethodGet, token, route.path+forged+suffix, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s of the RePublished record %s: status %d (%s), want 404: the route did not confirm the record by the leader read", route.name, forged, rec.Code, rec.Body.String())
		}
	}
}

// R1-T: a claim's 409 names the holder only when the leader read confirms its record
// (namesHolderConfirmed). An in-house PRIV session holds a role; the outside claimant's soft claim
// loses to it. Over a RePublished holder record the 409 names no holder; control: a confirmed holder
// is named.
func TestAnEdgeClaim409NamesNoRePublishedHolder(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const claimant, holder, role = "ses_409_claimant", "ses_409_holder", "legion-priv-priv-7-tester"
	token := rig.personToken(claimant, "PRIV")
	requireStatus(t, rig.subscribe(t, token, claimant), http.StatusOK, "the claimant's registration")
	rig.placeElsewhere(t, holder, "PRIV")
	if _, err := rig.deps.registry.SetRole(holder, "shared-listener", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	named := func(when string) any {
		t.Helper()
		rec := rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, claimant, role))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: the soft claim of %s: status %d (%s), want 409", when, role, rec.Code, rec.Body.String())
		}
		var answer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: decode %s: %v", when, rec.Body.String(), err)
		}
		return answer["holder"]
	}
	if got := named("control"); got != holder {
		t.Fatalf("control: the 409 names holder %v, want %s", got, holder)
	}
	republishSessionRecord(t, rig, holder, "PRIV")
	if got := named("after the RePublish"); got != nil {
		t.Errorf("after a RePublish of %s's record, the 409 names holder %v: namesHolderConfirmed took the cached entry unconfirmed", holder, got)
	}
}

// R1-T: a role send's answer names the holder only when the leader read confirms its record
// (roleSend.shownHolder). The send still forwards to the holder; over a RePublished holder record
// the answer names no holder. Control: a confirmed holder is named.
func TestAnEdgeRoleSendNamesNoRePublishedHolder(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const sender, holder, role = "ses_send_sender", "ses_send_holder", "legion-priv-priv-8-tester"
	token := rig.personToken(sender, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sender), http.StatusOK, "the sender's registration")
	holderToken := rig.personToken(holder, "PRIV")
	startSessionStandIn(t, rig.url, holder, answerDone)
	requireStatus(t, rig.subscribe(t, holderToken, holder), http.StatusOK, "the holder's registration")
	requireStatus(t, rig.call(t, http.MethodPost, holderToken, "/v1/roles/set",
		fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, holder, role)), http.StatusOK, "the holder's claim")
	topic := contracts.RoleSubject("PRIV", role)
	named := func(when string) string {
		t.Helper()
		rec := rig.call(t, http.MethodPost, token, "/v1/messages/publish",
			fmt.Sprintf(`{"topic":%q,"message":"for the holder","source":"agent","source_session":%q}`, topic, sender))
		requireStatus(t, rec, http.StatusOK, when+": a publish to "+topic)
		var answer listenerwire.PublishResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: decode %s: %v", when, rec.Body.String(), err)
		}
		return answer.Holder
	}
	if got := named("control"); got != holder {
		t.Fatalf("control: the role send names holder %q, want %s", got, holder)
	}
	republishSessionRecord(t, rig, holder, "PRIV")
	if got := named("after the RePublish"); got != "" {
		t.Errorf("after a RePublish of %s's record, the role send names holder %q: shownHolder took the cached entry unconfirmed", holder, got)
	}
}

// R9: the Legion agent replacement path confirms the replacement's session by the leader read. An
// older agent is replaced by a newer of the same role token; the listing names the newer as the
// older's replaced_by only when the newer's session is confirmed. Over a RePublished record for the
// newer, the older's replaced_by is null; control: a confirmed newer is named.
func TestAnEdgeShowsNoReplacementOverARepublishedRecord(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, older, newer, issue, roleToken = "ses_repl_outside", "ses_repl_older", "ses_repl_newer", "PRIV-9", "legion-priv-priv-10-tester"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, older, "PRIV")
	rig.placeElsewhere(t, newer, "PRIV")
	raw, err := rig.harness.client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.LegionAgentBucket, err)
	}
	put := func(sessionID, startedAt string) {
		entry := legionagents.Entry{SessionID: sessionID, LegionProject: "priv", Issue: issue, Role: "tester", RoleToken: roleToken,
			Title: "t " + sessionID, Harness: "omp", StartedAt: startedAt, RecordedAt: startedAt}
		value, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encode %s: %v", sessionID, err)
		}
		for _, key := range []string{"issue." + issue + "." + sessionID, "role." + roleToken + "." + sessionID, "session." + sessionID} {
			if _, err := raw.Put(key, value); err != nil {
				t.Fatalf("store %s: %v", key, err)
			}
		}
	}
	put(older, "2026-11-20T00:00:00Z")
	put(newer, "2026-11-20T01:00:00Z")
	replacedBy := func(when string) *string {
		t.Helper()
		rec := rig.call(t, http.MethodGet, token, "/v1/legion/agents?issue="+issue, "")
		requireStatus(t, rec, http.StatusOK, when+": the issue listing")
		var answer listenerwire.LegionAgentList
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: decode %s: %v", when, rec.Body.String(), err)
		}
		for _, e := range answer.Entries {
			if e.SessionID == older {
				return e.ReplacedBy
			}
		}
		t.Fatalf("%s: the listing does not show %s: %s", when, older, rec.Body.String())
		return nil
	}
	if by := replacedBy("control"); by == nil || *by != newer {
		t.Fatalf("control: %s's replaced_by is %v, want %s", older, by, newer)
	}
	republishSessionRecord(t, rig, newer, "PRIV")
	if by := replacedBy("after the RePublish"); by != nil {
		t.Errorf("after a RePublish of %s's record, %s's replaced_by is %v: the replacement took the cached entry unconfirmed", newer, older, *by)
	}
}

// R9: the full registry view's not-live branch confirms the session by the leader read. A
// RePublished record whose updated_at is past the live window reads not-live; the view answers it as
// a session no listener has heard of (unknownSession, no last_seen), not as a stopped session of the
// token's project. Control: a live confirmed session answers 200.
func TestAnEdgeHidesAPastWindowRepublishedRecordAsUnknown(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, live, forged = "ses_pw_outside", "ses_pw_live", "ses_pw_forged"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, live, "PRIV")
	body, err := json.Marshal(map[string]any{"session_id": forged, "machine_id": "shared-listener",
		"updated_at": time.Now().Add(-time.Hour).UnixMilli(), "projects": []string{"PRIV"}, "self_subscribed": true})
	if err != nil {
		t.Fatalf("encode the past-window record: %v", err)
	}
	testnats.RepublishesKey(t, rig.harness.client.Conn, bus.SessionBucket, forged, body)
	testwait.Eventually(t, "the cache to take the past-window record of "+forged, func() bool {
		entry, state := rig.deps.sessions.Lookup(forged)
		return entry.SessionID == forged && state != session.Live
	})
	if rec := rig.call(t, http.MethodGet, token, "/v1/registry/"+live, ""); rec.Code != http.StatusOK {
		t.Fatalf("control, the live session %s: status %d (%s), want 200", live, rec.Code, rec.Body.String())
	}
	rec := rig.call(t, http.MethodGet, token, "/v1/registry/"+forged, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("the not-live RePublished record %s: status %d (%s), want 404", forged, rec.Code, rec.Body.String())
	}
	var refusal listenerwire.NoLiveSessionRefusal
	_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	if refusal.LastSeen != 0 {
		t.Errorf("the 404 for %s names last_seen %d: the not-live branch judged the cached entry and answered a stopped session of the token's project, not an unknown one", forged, refusal.LastSeen)
	}
}

// R10: pruneEdgeInterests judges a session's GitHub topics by the token this replica admitted it on,
// not by its stored record, and skips a session this replica admitted no registration of. A record
// and interest row at the edge's own machine for a session the edge never admitted, following a
// GitHub topic of a repository no served project's answer names: the prune leaves the topic, because
// nothing here registered the session. A revert to the stored record's projects would read PRIV's
// answer and remove the topic.
func TestPruneEdgeInterestsSkipsAnUnadmittedSession(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	rig.tick(t)
	const unadmitted = "ses_prune_unadmitted"
	const githubTopic = contracts.GithubTopicPrefix + "acme.zzz.>"
	if err := rig.deps.sessions.Put(unadmitted, session.SessionEntry{MachineID: "test-machine", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("store %s's record: %v", unadmitted, err)
	}
	if _, err := rig.deps.registry.Upsert(store.Interest{SessionID: unadmitted, MachineID: "test-machine"}, []string{githubTopic}); err != nil {
		t.Fatalf("store %s's interest row: %v", unadmitted, err)
	}
	pruneEdgeInterests(context.Background(), rig.deps, "test-machine")
	row, _, err := rig.deps.registry.RefreshAt(context.Background(), unadmitted)
	if err != nil {
		t.Fatalf("read %s's row after the prune: %v", unadmitted, err)
	}
	if !slices.Contains(row.Topics, githubTopic) {
		t.Errorf("the prune removed %s's GitHub topic %q, though this replica admitted no registration of it: it judged the stored record's projects, not the admitted token (%v)", unadmitted, githubTopic, row.Topics)
	}
}
