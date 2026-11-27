//go:build natslive

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
)

// leaderReadUnanswered is the error StoredByWriter returns for a leader read that did not answer,
// in the shape the registry wraps nats.go's "no responders" in (session.SessionRegistry's
// StoredByWriter). A test installs it through the registry's leader-read seam
// (SetStoredByWriterSeamForTest) so one leader read fails while the cache and every other read
// still answer, which a cancelled request context cannot do: the Legion listing's own bucket read
// takes the same context, so a cancel fails that read first and the confirmation is never reached.
func leaderReadUnanswered(sessionID string) error {
	return fmt.Errorf("read session %s from the bucket's stream leader: %w", sessionID, errors.New("nats: no responders available for request"))
}

// requireLegionBusUnavailable fails the test unless recorder is the bus-unavailable 503 the Legion
// agent routes give a registry read that did not answer (writeLegionAgentError): 503 with
// Retry-After: 1.
func requireLegionBusUnavailable(t *testing.T, recorder *httptest.ResponseRecorder, path string) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("GET %s with the leader read failing: status %d (%s), want 503", path, recorder.Code, recorder.Body.String())
		return
	}
	if retry := recorder.Header().Get("Retry-After"); retry != "1" {
		t.Errorf("GET %s answered 503 with Retry-After %q, want 1", path, retry)
	}
}

// R9(d): the Legion agent listing and its replacement confirm each session by the record the
// stream's leader holds (sessionInProject, through session.SessionRegistry.StoredByWriter), reached
// by showsAgentConfirmed and agentsConfirmed. When that leader read does not answer, every one of
// these routes must answer the bus-unavailable 503 it answers for any read that did not answer, not
// drop the agent silently from a 200 listing. The fault is one session's leader read refused through
// the registry's seam; the Legion bucket read (ByIssue, ByRoleToken, Get) still answers, so only the
// confirmation does not, which a cancelled request context cannot isolate (that read takes the same
// context and fails first). The control in each row is the same route with no seam, which answers
// 200 and shows the agent, so a 503 is the confirmation read's and not an agent the edge never shows.
// The revert-control the slice owner named - sessionInProject dropping its error (return false, nil)
// - turns each 503 into a silent 200 that this test then catches.
func TestAnEdgeLegionAgentLeaderReadFaultAnswersBusUnavailable(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, agentSession, issue, roleToken = "ses_lr_outside", "ses_lr_agent", "PRIV-9", "legion-priv-priv-9-tester"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	rig.placeElsewhere(t, agentSession, "PRIV")
	putEdgeLegionAgent(t, rig, issue, roleToken, agentSession, "2026-11-20T00:00:00Z")

	sessionRoute := "/v1/legion/agents/sessions/" + agentSession
	for _, path := range []string{"/v1/legion/agents?issue=" + issue, "/v1/legion/agents/roles/" + roleToken, sessionRoute} {
		t.Run(path, func(t *testing.T) {
			// The instrument's control: with a working leader read the route answers 200 and shows
			// the agent, so a 503 under the fault is the confirmation read's.
			ok := rig.call(t, http.MethodGet, token, path, "")
			requireStatus(t, ok, http.StatusOK, "the control, "+path)
			if path == sessionRoute {
				var answer listenerwire.LegionAgentAnswer
				if err := json.Unmarshal(ok.Body.Bytes(), &answer); err != nil {
					t.Fatalf("decode %s: %v", ok.Body.String(), err)
				}
				if answer.SessionID != agentSession {
					t.Fatalf("the control answered session %q, want %s (the control cannot fail if the agent is not shown)", answer.SessionID, agentSession)
				}
			} else if shown := legionSessionIDs(t, ok); !slices.Contains(shown, agentSession) {
				t.Fatalf("the control lists %v, want %s among them (the control cannot fail if the agent is not shown)", shown, agentSession)
			}

			restore := rig.deps.sessions.SetStoredByWriterSeamForTest(agentSession, session.SessionEntry{}, false, leaderReadUnanswered(agentSession))
			defer restore()
			requireLegionBusUnavailable(t, rig.call(t, http.MethodGet, token, path, ""), path)
		})
	}

	// The replacement path: showsAgentConfirmed confirms an agent's replacement
	// (agent.ReplacedBy) by its own leader read (sessionInProject). An older agent of the same role
	// token is replaced by a newer; the session route of the older reaches the older's own
	// confirmation (which answers) and then the newer's, the replacement's. With only the newer's
	// leader read refused, the older's session route answers the bus-unavailable 503, and the
	// control, with no seam, answers 200 naming the newer as the replacement.
	t.Run("the replacement", func(t *testing.T) {
		const older, newer, replIssue, replRole = "ses_lr_older", "ses_lr_newer", "PRIV-10", "legion-priv-priv-10-tester"
		rig.placeElsewhere(t, older, "PRIV")
		rig.placeElsewhere(t, newer, "PRIV")
		putEdgeLegionAgent(t, rig, replIssue, replRole, older, "2026-11-20T00:00:00Z")
		putEdgeLegionAgent(t, rig, replIssue, replRole, newer, "2026-11-20T01:00:00Z")
		olderRoute := "/v1/legion/agents/sessions/" + older

		ok := rig.call(t, http.MethodGet, token, olderRoute, "")
		requireStatus(t, ok, http.StatusOK, "the control, "+olderRoute)
		var answer listenerwire.LegionAgentAnswer
		if err := json.Unmarshal(ok.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode %s: %v", ok.Body.String(), err)
		}
		if answer.ReplacedBy == nil || *answer.ReplacedBy != newer {
			t.Fatalf("the control answers %s replaced_by %v, want %s (the control cannot fail if the replacement is not confirmed)", older, answer.ReplacedBy, newer)
		}

		restore := rig.deps.sessions.SetStoredByWriterSeamForTest(newer, session.SessionEntry{}, false, leaderReadUnanswered(newer))
		defer restore()
		requireLegionBusUnavailable(t, rig.call(t, http.MethodGet, token, olderRoute, ""), olderRoute)
	})
}

// R8-T: every edge decision that shows or sends to a session re-judges its scope on the record the
// stream's leader holds for the session's key, never on the cached entry (edgeScope.showsConfirmed,
// through session.SessionRegistry.StoredByWriter). When the cache trails the bucket - a RePublished
// record listing a project the token holds is cached, then the session's own writer record listing
// another project is stored - the edge must follow the leader's record and hide the session, not the
// cache's. The divergence is forced through the registry's leader-read seam: the cached entry lists
// PRIV (placeElsewhere, which the PRIV token would show), while the leader read returns a writer's
// record listing PUB alone. The list, the full registry view and the send route must all hide the
// session. The control in each row is the same route before the seam, when the cache and the leader
// agree on PRIV, which every route shows. The revert-control the slice owner named - showsConfirmed
// judging the cached entry (s.shows(entry)) rather than the leader's record (s.shows(stored)) - shows
// the session on every route, which this test then catches.
func TestAnEdgeFollowsTheLeaderRecordNotTheCachedEntry(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, peer = "ses_leader_outside", "ses_leader_peer"
	token := rig.personToken(outside, "PRIV")
	requireStatus(t, rig.subscribe(t, token, outside), http.StatusOK, "the outside session's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")

	// The control: with the cache and the leader agreeing on PRIV, every route shows the peer, so a
	// hidden peer under the seam is the leader record's doing and not a peer the edge never shows.
	if listed := rig.listedSessions(t, token); !slices.Contains(listed, peer) {
		t.Fatalf("control: GET /v1/sessions lists %v, want %s among them", listed, peer)
	}
	if rec := rig.call(t, http.MethodGet, token, "/v1/registry/"+peer, ""); rec.Code != http.StatusOK {
		t.Fatalf("control: the full view of %s: status %d (%s), want 200", peer, rec.Code, rec.Body.String())
	}
	if rec := rig.call(t, http.MethodPost, token, "/v1/messages/send",
		fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, peer, outside)); rec.Code != http.StatusOK {
		t.Fatalf("control: the send to %s: status %d (%s), want 200", peer, rec.Code, rec.Body.String())
	}

	// The leader read returns a writer's record of the peer's key listing PUB alone, a project the
	// PRIV token does not hold, while the cache still lists PRIV.
	restore := rig.deps.sessions.SetStoredByWriterSeamForTest(peer,
		session.SessionEntry{SessionID: peer, MachineID: "shared-listener", Projects: []string{"PUB"}}, true, nil)
	defer restore()

	if listed := rig.listedSessions(t, token); slices.Contains(listed, peer) {
		t.Errorf("GET /v1/sessions lists %s: the list showed the cached PRIV entry, not the leader's PUB record", peer)
	}
	if rec := rig.call(t, http.MethodGet, token, "/v1/registry/"+peer, ""); rec.Code != http.StatusNotFound {
		t.Errorf("the full view of %s: status %d (%s), want 404: the view showed the cached PRIV entry, not the leader's PUB record", peer, rec.Code, rec.Body.String())
	}
	rig.requireMissingSend(t, token, outside, peer, "with the leader read returning a PUB record for the PRIV-cached peer")
}

// putEdgeLegionAgent stores one Legion agent registry entry for sessionID under its issue, role
// token and session keys, as the daemon's write lands them, so a listing and a session read both
// find it.
func putEdgeLegionAgent(t *testing.T, rig *edgeRig, issue, roleToken, sessionID, startedAt string) {
	t.Helper()
	raw, err := rig.harness.client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.LegionAgentBucket, err)
	}
	entry := legionagents.Entry{SessionID: sessionID, LegionProject: "priv", Issue: issue, Role: "tester", RoleToken: roleToken,
		Title: "Legion tester " + sessionID, Harness: "omp", StartedAt: startedAt, RecordedAt: startedAt}
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode %s's agent entry: %v", sessionID, err)
	}
	for _, key := range []string{"issue." + issue + "." + sessionID, "role." + roleToken + "." + sessionID, "session." + sessionID} {
		if _, err := raw.Put(key, value); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}
}

// legionSessionIDs is the session ids a Legion agent listing answers.
func legionSessionIDs(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	var answer listenerwire.LegionAgentList
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	ids := make([]string, len(answer.Entries))
	for i, entry := range answer.Entries {
		ids[i] = entry.SessionID
	}
	return ids
}
