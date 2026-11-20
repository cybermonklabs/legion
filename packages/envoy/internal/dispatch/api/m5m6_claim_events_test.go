package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// claimEventsWrite is the writes the event rows read back, on PUB's issue nobody claimed
// (seedHiddenHolders' unclaimed): session-priv claims it under a credential that proves it in PUB;
// carol moves it to todo and routes it to role:legion-priv-architect, a role of PRIV, so the
// issue.updated payload carries both the claim and the route; carol, a person, releases the claim,
// so issue.released names session-priv as the claim it cleared; carol messages the issue to that
// role (message.created's target); and carol comments mentioning it (comment.created's mentions),
// the marker that ends a live read.
func (s hiddenHolderSeed) claimEventsWrite(t *testing.T) (key, marker string) {
	t.Helper()
	key = s.unclaimed
	role := "role:" + privateProjectRole
	s.asSession(t, sessionOfPrivate, seedPublic, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{}, http.StatusOK)
	s.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+key,
		map[string]any{"status": "todo", "route": role}, http.StatusOK)
	s.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, http.StatusOK)
	s.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/messages",
		map[string]any{"body": "A note for the architect.", "target": role, "delivery": "btw"}, http.StatusCreated)
	marker = createdID(t, s.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/comments",
		map[string]any{"body": "The marker of the claim events.", "mentions": []map[string]string{{"target": role}}}))
	// The mention's delivery settles on a sender of its own; every read below waits for it, so each
	// reads the same events.
	awaitBackgroundWrites(t, s.server)
	return key, marker
}

// issueEvent is what the event rows read of one event, with its raw JSON.
type issueEvent struct {
	ID       int64           `json:"id"`
	IssueKey *string         `json:"issue_key"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	raw      string
}

// decodeIssueEvents decodes a JSON array of events, keeping each one's raw JSON.
func decodeIssueEvents(t *testing.T, body []byte) []issueEvent {
	t.Helper()
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		t.Fatalf("decode the events %s: %v", body, err)
	}
	events := make([]issueEvent, 0, len(raws))
	for _, raw := range raws {
		var event issueEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("decode the event %s: %v", raw, err)
		}
		event.raw = string(raw)
		events = append(events, event)
	}
	return events
}

// claimEventsOf names what events say of a claim, a route and session-priv: each claim write's
// type, and every event that names session-priv or role:legion-priv-architect anywhere.
func claimEventsOf(events []issueEvent) (types []string, naming []string) {
	for _, event := range events {
		if event.Type == "issue.claimed" || event.Type == "issue.released" {
			types = append(types, event.Type)
		}
		if strings.Contains(event.raw, `"`+sessionOfPrivate+`"`) || strings.Contains(event.raw, privateProjectRole) {
			naming = append(naming, event.Type+" "+event.raw)
		}
	}
	return types, naming
}

// TestM5M6TheEventsNameNoHiddenClaimHolderOrRole: an issue's events, the event log and the live
// socket name no claim holder and no role their reader may not see (claimEventsWrite). To carol's
// token that lists no project, which sees PUB and not session-priv, a session that acted in PRIV:
//
//   - the issue.claimed and issue.released events are absent (eventReadableSQL's claim rule), on
//     the issue's events, the log and the live socket alike;
//   - the issue.updated payload carries the claim null and the route null (payloadReferenceKeys);
//   - no event of the issue names session-priv or role:legion-priv-architect.
//
// carol at her browser, who sees PRIV, reads both claim events and the role. bob, who sees PRIV and
// not alice, reads no issue.claimed of the claim alice took on PRIV's issue, and its later events
// carry the claim null.
//
// Controls, one line each: the claim rule passing every claim event (the claim events reach carol's
// token); the payload filter leaving claim alone (the issue.updated payload names session-priv).
func TestM5M6TheEventsNameNoHiddenClaimHolderOrRole(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	socketURL := liveURLOf(t, seed.handler)
	live := map[privateCaller]*socketClient{}
	for _, caller := range []privateCaller{callerCarolToken, callerCarol} {
		live[caller] = seed.dialLive(t, socketURL, seed.liveHeader(t, caller))
		subscribeEvents(t, live[caller], "live", nil)
	}
	key, marker := seed.claimEventsWrite(t)

	ofKey := func(events []issueEvent) []issueEvent {
		return slices.DeleteFunc(events, func(event issueEvent) bool { return event.IssueKey == nil || *event.IssueKey != key })
	}
	reads := func(caller privateCaller) map[string][]issueEvent {
		t.Helper()
		answers := map[string][]issueEvent{}
		answers["GET /api/v1/issues/"+key+"/events"] = decodeIssueEvents(t,
			seed.mustRequest(t, caller, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, http.StatusOK).Body.Bytes())
		safeHorizon(t, seed.server)
		answers["GET /api/v1/events/log"] = ofKey(decodeIssueEvents(t, seed.mustRequest(t, caller, http.MethodGet,
			"/api/v1/events/log?"+url.Values{"limit": {"1000"}}.Encode(), nil, http.StatusOK).Body.Bytes()))
		var sent []issueEvent
		for _, event := range liveEventsThrough(t, live[caller], "live", commentCreated(marker)) {
			raw, _ := json.Marshal(event)
			sent = append(sent, issueEvent{ID: event.ID, IssueKey: &key, Type: event.Type, Payload: event.Payload, raw: string(raw)})
		}
		answers["the live socket"] = sent
		return answers
	}

	for read, events := range reads(callerCarolToken) {
		types, naming := claimEventsOf(events)
		if len(types) > 0 {
			t.Errorf("carol's PUB token: %s holds %v of %s, want no claim event: session-priv held the claim", read, types, key)
		}
		for _, named := range naming {
			t.Errorf("carol's PUB token: %s names session-priv or role:%s: %s", read, privateProjectRole, named)
		}
		updated := slices.IndexFunc(events, func(event issueEvent) bool {
			return event.Type == "issue.updated" && strings.Contains(event.raw, `"route"`) && strings.Contains(event.raw, `"todo"`)
		})
		if updated < 0 {
			t.Errorf("carol's PUB token: %s holds no issue.updated of the move to todo, so nothing here read the claim and the route", read)
			continue
		}
		var payload struct {
			Claim json.RawMessage `json:"claim"`
			Route json.RawMessage `json:"route"`
		}
		if err := json.Unmarshal(events[updated].Payload, &payload); err != nil {
			t.Fatalf("decode %s's issue.updated payload: %v", read, err)
		}
		if string(payload.Claim) != "null" || string(payload.Route) != "null" {
			t.Errorf("carol's PUB token: %s's issue.updated carries claim %s and route %s, want both null", read, payload.Claim, payload.Route)
		}
	}
	for read, events := range reads(callerCarol) {
		if types, naming := claimEventsOf(events); !slices.Equal(types, []string{"issue.claimed", "issue.released"}) || len(naming) == 0 {
			t.Errorf("carol: %s holds claim events %v naming session-priv or the role %d times, want issue.claimed and issue.released, named", read, types, len(naming))
		}
	}

	bobs := decodeIssueEvents(t, seed.mustRequest(t, callerBob, http.MethodGet,
		"/api/v1/issues/"+seed.issue[seedPrivate]+"/events", nil, http.StatusOK).Body.Bytes())
	if types, _ := claimEventsOf(bobs); len(types) > 0 {
		t.Errorf("bob: %s's events hold %v, want no claim event: alice, whom he may not see, took the claim", seed.issue[seedPrivate], types)
	}
	for _, event := range bobs {
		var payload struct {
			Claim json.RawMessage `json:"claim"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && len(payload.Claim) > 0 && string(payload.Claim) != "null" {
			t.Errorf("bob: %s's %s carries claim %s, want null", seed.issue[seedPrivate], event.Type, payload.Claim)
		}
	}

	// The writer of a visible row is named whatever its reader may see of it (the residual this
	// round leaves): session-priv moved PUB's parked issue into backlog, and carol's PUB token reads
	// that event with session-priv as its actor.
	for _, event := range decodeIssueEvents(t, seed.mustRequest(t, callerCarolToken, http.MethodGet,
		"/api/v1/issues/"+seed.parked+"/events", nil, http.StatusOK).Body.Bytes()) {
		if strings.Contains(event.raw, `"actor":{"kind":"session","id":"`+sessionOfPrivate+`"`) {
			t.Logf("residual: carol's PUB token reads %s's %s %d with actor %s", seed.parked, event.Type, event.ID, sessionOfPrivate)
		}
	}
}

// TestM5M6WithNoPrivateProjectTheEventsReadAsBefore (R9): with PRIV public alice, a full member,
// reads the claim events' issue as the base answered it: its events, and its events in the log,
// each logged with times and ids blanked beside its sha256 (the report compares the two commits),
// with both claim events present and the issue.updated naming session-priv and the role.
//
// Control: the claim rule hiding every claim event; alice then reads none and the row fails.
func TestM5M6WithNoPrivateProjectTheEventsReadAsBefore(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	key, _ := seed.claimEventsWrite(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	safeHorizon(t, seed.server)
	for _, target := range []string{"/api/v1/issues/" + key + "/events", "/api/v1/events/log?" + url.Values{"limit": {"1000"}}.Encode()} {
		events := decodeIssueEvents(t, seed.mustRequest(t, callerAlice, http.MethodGet, target, nil, http.StatusOK).Body.Bytes())
		events = slices.DeleteFunc(events, func(event issueEvent) bool { return event.IssueKey == nil || *event.IssueKey != key })
		raws := make([]string, len(events))
		for index, event := range events {
			raws[index] = timestampsAndIDs.ReplaceAllString(event.raw, "<varies>")
		}
		blanked := "[" + strings.Join(raws, ",") + "]"
		t.Logf("R9 events of %s at %s sha256 %x: %s", key, strings.SplitN(target, "?", 2)[0], sha256.Sum256([]byte(blanked)), blanked)
		if types, naming := claimEventsOf(events); !slices.Equal(types, []string{"issue.claimed", "issue.released"}) || len(naming) == 0 {
			t.Errorf("PRIV public: alice's %s holds claim events %v naming session-priv or the role %d times, want both, named", target, types, len(naming))
		}
	}
}
