package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/listenerwire"
)

// The edge shows a Legion agent registry entry only of a session of its issue's project
// (edgeScope.showsAgentConfirmed). A chosen entry naming an in-house session of PUB under a PRIV issue's three
// keys (issue, role token and session), as any writer of the registry's bucket can store it, is
// listed to the outside session by neither listing, its session read answers 404, and it is no
// replacement of the outside session's own entry, whose replaced_by stays null. A copy of the
// outside session's own entry stored under another session's key is passed over by the store's
// listing (legionagents.Store.list), so the outside session is listed once. The control in the same
// row is the outside session's own entry, which every read answers.
func TestAnEdgeShowsALegionAgentOnlyOfASessionOfItsIssuesProject(t *testing.T) {
	t.Parallel()
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const (
		outside   = "ses_agents_outside"
		inHouse   = "ses_agents_in_house"
		copyKey   = "ses_agents_copy"
		issue     = "PRIV-7"
		roleToken = "legion-priv-priv-7-tester"
	)
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, inHouse, "PUB")

	own := legionagents.Entry{SessionID: outside, LegionProject: "priv", Issue: issue, Role: "tester", RoleToken: roleToken,
		Title: "Legion tester · " + issue, Harness: "omp", StartedAt: "2026-11-20T00:00:00Z"}
	if _, err := rig.agents.Put(t.Context(), own); err != nil {
		t.Fatalf("record %s: %v", outside, err)
	}
	raw, err := rig.harness.client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the legion agent bucket: %v", err)
	}
	put := func(key string, entry legionagents.Entry) {
		t.Helper()
		value, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encode the entry for %s: %v", key, err)
		}
		if _, err := raw.Put(key, value); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}
	chosen := own
	chosen.SessionID, chosen.Title, chosen.StartedAt, chosen.RecordedAt = inHouse, "a title its writer chose", "2026-11-20T01:00:00Z", "2026-11-20T01:00:00Z"
	for _, key := range []string{"issue." + issue + "." + inHouse, "role." + roleToken + "." + inHouse, "session." + inHouse} {
		put(key, chosen)
	}
	stored, err := rig.agents.Get(t.Context(), outside)
	if err != nil {
		t.Fatalf("read %s's entry back: %v", outside, err)
	}
	put("issue."+issue+"."+copyKey, stored.LegionAgentEntry)

	list := func(path string) []listenerwire.LegionAgentAnswer {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, path, "")
		requireStatus(t, recorder, http.StatusOK, "GET "+path)
		var answer listenerwire.LegionAgentList
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		return answer.Entries
	}
	requireOnlyOwn := func(path string, entries []listenerwire.LegionAgentAnswer) {
		t.Helper()
		if len(entries) != 1 || entries[0].SessionID != outside {
			ids := make([]string, len(entries))
			for i, entry := range entries {
				ids[i] = entry.SessionID
			}
			t.Errorf("GET %s answers %v, want only %s", path, ids, outside)
			return
		}
		if entries[0].ReplacedBy != nil {
			t.Errorf("GET %s answers %s replaced by %s, want no replacement", path, outside, *entries[0].ReplacedBy)
		}
	}
	for _, path := range []string{"/v1/legion/agents?issue=" + issue, "/v1/legion/agents/roles/" + roleToken} {
		requireOnlyOwn(path, list(path))
	}
	if recorder := rig.call(t, http.MethodGet, token, "/v1/legion/agents/sessions/"+inHouse, ""); recorder.Code != http.StatusNotFound {
		t.Errorf("GET /v1/legion/agents/sessions/%s: status %d (%s), want 404", inHouse, recorder.Code, recorder.Body.String())
	}
	recorder := rig.call(t, http.MethodGet, token, "/v1/legion/agents/sessions/"+outside, "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/legion/agents/sessions/"+outside)
	var answer listenerwire.LegionAgentAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	requireOnlyOwn("/v1/legion/agents/sessions/"+outside, []listenerwire.LegionAgentAnswer{answer})
}

// The role read's `answering` says whether a project's daemon answers for its stopped agents, and
// the edge says it only of a role of one of its token's projects, by the issue the role token
// names, as it shows only such an issue's entries (edgeScope.showsIssue): a role of another
// project reads false however that project's answering record reads. The control is a role of the
// token's own project under a record that says it answers, which reads true.
func TestAnEdgeAnswersTheRoleReadsAnsweringFlagOnlyOfItsOwnProjects(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside = "ses_role_answering_outside"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	for _, project := range []string{"priv", "pub"} {
		if _, err := rig.agents.PutProject(t.Context(), project, legionagents.Project{Answering: true, UpdatedAt: "2026-11-20T00:00:00Z"}); err != nil {
			t.Fatalf("state that %s answers: %v", project, err)
		}
	}
	for _, tc := range []struct {
		role string
		want bool
	}{
		{"legion-priv-priv-7-tester", true},
		{"legion-pub-pub-7-tester", false},
	} {
		path := "/v1/legion/agents/roles/" + tc.role
		recorder := rig.call(t, http.MethodGet, token, path, "")
		requireStatus(t, recorder, http.StatusOK, "GET "+path)
		var answer struct {
			Entries   []json.RawMessage `json:"entries"`
			Answering *bool             `json:"answering"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		if len(answer.Entries) != 0 || answer.Answering == nil || *answer.Answering != tc.want {
			t.Errorf("GET %s through the edge = %s, want no entries and answering %t", path, recorder.Body.String(), tc.want)
		}
	}
}
