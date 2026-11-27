//go:build natslive

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// republishSessionRecordListingPRIV stores, at sessionID's own session key, a body a RePublish of
// another stream gave: it reads as sessionID's record naming its key, a machine, a time and PRIV as
// its projects, with the stream's own headers (a server-written message). The session cache takes
// it as a PRIV session, so every edge decision that asks the cache alone would grant the session to
// a PRIV token. It waits for the cache to hold it.
func republishSessionRecordListingPRIV(t *testing.T, rig *edgeRig, sessionID string, extra map[string]any) {
	t.Helper()
	fields := map[string]any{
		"session_id": sessionID, "machine_id": "shared-listener", "updated_at": time.Now().UnixMilli(),
		"projects": []string{"PRIV"}, "self_subscribed": true,
	}
	for k, v := range extra {
		fields[k] = v
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode the republished record of %s: %v", sessionID, err)
	}
	testnats.RepublishesKey(t, rig.harness.client.Conn, bus.SessionBucket, sessionID, body)
	testwait.Eventually(t, "the session cache to take the republished record of "+sessionID, func() bool {
		entry, state := rig.deps.sessions.Lookup(sessionID)
		return state != session.Gone && len(entry.Projects) == 1 && entry.Projects[0] == "PRIV"
	})
}

// The edge shows a Legion agent only when its session is a session of the issue's project confirmed
// by the session's stored record (edgeScope.showsAgentConfirmed, through its leader read), not by
// the cache alone. An in-house PUB session with a chosen Legion agent entry naming it under PRIV-8's
// three keys is hidden to an outside PRIV token (the control). A RePublish of a session record
// listing PRIV at the in-house key makes the cache read it as a PRIV session, yet its stored record
// is the server's: the edge still hides it from the listing, and its session read answers 404. This
// is census M4 reopened by a RePublished record, which be248e69cc closed for a chosen entry alone.
func TestAnEdgeConfirmsALegionAgentByItsStoredRecord(t *testing.T) {
	t.Parallel()
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, inHouse, issue, roleToken = "ses_la_conf_outside", "ses_la_conf_in_house", "PRIV-8", "legion-priv-priv-8-tester"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, inHouse, "PUB")
	raw, err := rig.harness.client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the legion agent bucket: %v", err)
	}
	chosen := legionagents.Entry{SessionID: inHouse, LegionProject: "priv", Issue: issue, Role: "tester", RoleToken: roleToken,
		Title: "a title its writer chose", Harness: "omp", StartedAt: "2026-11-20T01:00:00Z", RecordedAt: "2026-11-20T01:00:00Z"}
	value, err := json.Marshal(chosen)
	if err != nil {
		t.Fatalf("encode the chosen legion agent entry: %v", err)
	}
	for _, key := range []string{"issue." + issue + "." + inHouse, "role." + roleToken + "." + inHouse, "session." + inHouse} {
		if _, err := raw.Put(key, value); err != nil {
			t.Fatalf("store the chosen entry at %s: %v", key, err)
		}
	}
	listed := func(when string) []string {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, "/v1/legion/agents?issue="+issue, "")
		requireStatus(t, recorder, http.StatusOK, "GET /v1/legion/agents?issue="+issue)
		var answer listenerwire.LegionAgentList
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		ids := make([]string, 0, len(answer.Entries))
		for _, entry := range answer.Entries {
			ids = append(ids, entry.SessionID)
		}
		t.Logf("%s: GET /v1/legion/agents?issue=%s answers %v", when, issue, ids)
		return ids
	}
	// Control: the chosen entry names a PUB session, which the edge does not show to a PRIV token.
	if ids := listed("control, the chosen entry alone"); len(ids) != 0 {
		t.Fatalf("control: the in-house session is listed to an outside PRIV token with no RePublish: %v", ids)
	}
	republishSessionRecordListingPRIV(t, rig, inHouse, map[string]any{"title": "a title its writer chose"})
	if ids := listed("after the RePublish"); len(ids) != 0 {
		t.Errorf("the edge lists the in-house session %v under %s after a RePublish of its record: the Legion agent read took the cached entry unconfirmed", ids, issue)
	}
	if recorder := rig.call(t, http.MethodGet, token, "/v1/legion/agents/sessions/"+inHouse, ""); recorder.Code != http.StatusNotFound {
		t.Errorf("GET /v1/legion/agents/sessions/%s after the RePublish: status %d (%s), want 404", inHouse, recorder.Code, recorder.Body.String())
	}
}
