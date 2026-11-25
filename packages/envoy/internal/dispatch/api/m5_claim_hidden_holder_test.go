package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// hiddenHolderSessions is what the stand-in listener lists for the claim, route and role rows below:
// session-priv, which acts in PRIV (seedHiddenHolders), so no caller that does not see PRIV may read
// it (access.SessionClause), holds role:reviewer; session-pub, which acts in PUB alone, holds
// role:legion-priv-architect, a role of PRIV by its name (contracts.RoleProject).
const hiddenHolderSessions = `[
	{"session_id":"session-priv","title":"Private work","capabilities":["btw","steer"],"roles":["reviewer"]},
	{"session_id":"session-pub","title":"Public work","capabilities":["btw","steer"],"roles":["legion-priv-architect"]}
]`

// The roles the rows route to: one of PRIV by its name that session-pub holds, one session-priv
// holds, and one no session holds.
const (
	privateProjectRole = "legion-priv-architect"
	hiddenHolderRole   = "reviewer"
	unheldRole         = "idle"
)

// answerHeldRoles answers the listener's GET /v1/roles/{role} from sessionsJSON's roles: the
// session holding the role, or the listener's 404 for a role no session holds.
func answerHeldRoles(t *testing.T, sessionsJSON string) func(http.ResponseWriter, *http.Request) bool {
	t.Helper()
	var sessions []struct {
		SessionID    string   `json:"session_id"`
		Title        string   `json:"title"`
		Capabilities []string `json:"capabilities"`
		Roles        []string `json:"roles"`
	}
	if err := json.Unmarshal([]byte(sessionsJSON), &sessions); err != nil {
		t.Fatalf("decode the stand-in listener's sessions: %v", err)
	}
	return func(w http.ResponseWriter, r *http.Request) bool {
		role, asked := strings.CutPrefix(r.URL.Path, "/v1/roles/")
		if r.Method != http.MethodGet || !asked {
			return false
		}
		for _, session := range sessions {
			if slices.Contains(session.Roles, role) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"role": role, "holder": session.SessionID, "title": session.Title, "capabilities": session.Capabilities,
				})
				return true
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no holder for role ` + role + `"}`))
		return true
	}
}

// hiddenHolderSeed is verification v16's seed (private_seed_test.go) with the objects the M5, M6
// and M10 rows read, each written through its route by the caller who would write it.
type hiddenHolderSeed struct {
	privateSeed
	// claimed is PUB's issue, which session-priv claims and carol moves into in_progress, so it
	// stands in the priorities' first fault; child is an issue under it that session-priv claims
	// too; unclaimed is a PUB issue nobody claims; parked advances the priority ship-pub and was
	// moved into backlog by session-priv, so it stands in the second fault.
	claimed, child, unclaimed, parked string
	// toPrivateRole, toHiddenHolder and toUnheldRole are PUB issues carol routes to
	// role:legion-priv-architect, role:reviewer and role:idle; asks is the id of carol's open ask on
	// each, by issue key.
	toPrivateRole, toHiddenHolder, toUnheldRole string
	asks                                        map[string]string
}

// seedHiddenHolders writes the seed. alice claims PRIV's issue and moves it into backlog while PRIV
// is still public, as a person did before the project went private, so bob, who sees PRIV and not
// alice (access.PersonClause), reads an issue a person he may not see claimed and moved.
func seedHiddenHolders(t *testing.T, options testServerOptions) hiddenHolderSeed {
	t.Helper()
	listener, _ := newBroadcastListener(t, hiddenHolderSessions, answerHeldRoles(t, hiddenHolderSessions))
	options.envoyURL = listener.URL
	seed := hiddenHolderSeed{privateSeed: seedPrivateProjectWith(t, options), asks: map[string]string{}}
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/claim", map[string]any{}, http.StatusOK)
	seed.mustRequest(t, callerAlice, http.MethodPatch, "/api/v1/issues/"+seed.issue[seedPrivate], map[string]any{"status": "backlog"}, http.StatusOK)
	seed.setVisibility(t, seedPrivate, model.ProjectPrivate)

	seed.asSession(t, sessionOfPrivate, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "Done on PRIV."}, http.StatusCreated)
	seed.asSession(t, sessionOfPublic, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	create := func(title string, fields map[string]any) string {
		t.Helper()
		body := map[string]any{"project": seedPublic, "title": title, "force": true}
		for key, value := range fields {
			body[key] = value
		}
		return decodeBody[struct {
			Key string `json:"key"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", body, http.StatusCreated)).Key
	}
	seed.claimed = seed.issue[seedPublic]
	seed.child = create("The claimed issue's child", map[string]any{"parent": seed.claimed})
	seed.unclaimed = create("Work nobody claims", nil)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/priorities", map[string]any{"id": "ship-pub", "title": "Ship PUB"}, http.StatusCreated)
	seed.parked = create("Parked work", map[string]any{"advances": []string{"ship-pub"}})
	for _, key := range []string{seed.claimed, seed.child} {
		seed.asSession(t, sessionOfPrivate, seedPublic, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{}, http.StatusOK)
	}
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+seed.claimed, map[string]any{"status": "in_progress"}, http.StatusOK)
	seed.asSession(t, sessionOfPrivate, seedPublic, http.MethodPatch, "/api/v1/issues/"+seed.parked, map[string]any{"status": "backlog"}, http.StatusOK)

	// The titles and questions name no role, so a role the answer leaves out is named nowhere in it.
	for index, routed := range []struct {
		key  *string
		role string
	}{{&seed.toPrivateRole, privateProjectRole}, {&seed.toHiddenHolder, hiddenHolderRole}, {&seed.toUnheldRole, unheldRole}} {
		*routed.key = create(fmt.Sprintf("Routed work %d", index), nil)
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+*routed.key, map[string]any{"route": "role:" + routed.role}, http.StatusOK)
		seed.asks[*routed.key] = decodeBody[struct {
			ID string `json:"id"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+*routed.key+"/asks",
			map[string]any{"question": "Who works this issue's route?"}, http.StatusCreated)).ID
	}
	return seed
}

// readJSON is the answer to caller's GET target, decoded into fields, failing unless it is 200.
func (s hiddenHolderSeed) readJSON(t *testing.T, caller privateCaller, target string, into any) string {
	t.Helper()
	response := s.mustRequest(t, caller, http.MethodGet, target, nil, http.StatusOK)
	if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
		t.Fatalf("%s: decode GET %s: %v", caller, target, err)
	}
	return response.Body.String()
}

// claimText is a claim as the rows compare it: its holder's kind and id, or "null".
func claimText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "null"
	}
	var claim struct {
		Actor model.Actor `json:"actor"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return "undecodable " + string(raw)
	}
	return claim.Actor.Kind + ":" + claim.Actor.ID
}

// actorText is an actor as the rows compare it: its kind and id, or "null".
func actorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "null"
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		return "undecodable " + string(raw)
	}
	return actor.Kind + ":" + actor.ID
}

// rawText is a JSON value as the rows compare it, "null" for an absent one.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

// routeOf is the route caller's GET target answers, an issue's or an ask's, with its reach, as
// "<route> <route_status> <route_holder> <route_status_reason>", each "null" when absent, and the
// answer's body.
func (s hiddenHolderSeed) routeOf(t *testing.T, caller privateCaller, target string) (string, string) {
	t.Helper()
	var routed struct {
		Route             *string `json:"route"`
		RouteStatus       *string `json:"route_status"`
		RouteHolder       *string `json:"route_holder"`
		RouteStatusReason *string `json:"route_status_reason"`
	}
	body := s.readJSON(t, caller, target, &routed)
	return reachText(routed.Route, routed.RouteStatus) + " " + reachText(routed.RouteHolder, routed.RouteStatusReason), body
}

// claimRow is the part of an issue answer that names its claim, and of its children their claims.
type claimRow struct {
	Key         string          `json:"key"`
	Claim       json.RawMessage `json:"claim"`
	HolderState json.RawMessage `json:"holder_state"`
	LastSeen    json.RawMessage `json:"last_seen"`
	TakeableAt  json.RawMessage `json:"takeable_at"`
	MovedBy     json.RawMessage `json:"moved_by"`
	Children    []claimRow      `json:"children"`
}

// rowOf is the row of key among rows.
func rowOf(t *testing.T, rows []claimRow, key, where string) claimRow {
	t.Helper()
	index := slices.IndexFunc(rows, func(row claimRow) bool { return row.Key == key })
	if index < 0 {
		t.Fatalf("%s holds no row of %s", where, key)
	}
	return rows[index]
}

// priorityFaults is the part of GET /api/v1/priorities the rows read.
type priorityFaults struct {
	WorkUnderNoPriority struct {
		Issues []claimRow `json:"issues"`
	} `json:"work_under_no_priority"`
	PriorityWorkParked struct {
		Issues []claimRow `json:"issues"`
	} `json:"priority_work_parked"`
}

// TestM5AClaimNamesNoHolderItsReaderMayNotSee: a claim whose holder the reader may not see -
// session-priv, which has acted in PRIV, to alice, who does not see PRIV; alice, a person, to bob,
// who sees PRIV alone - answers as an issue nobody claims, in the issue read (its claim and its
// holder's liveness), its children, the listing and the priorities' fault lists. A claim write that
// such a holder refuses answers 409 ISSUE_CLAIMED naming no holder and no title, and the contended
// answer names none either. The same goes for the actor who moved an issue into backlog, which the
// parked fault and the listing's ?moved=true answer. carol, who sees PRIV, reads each holder.
//
// Control: leaveOutHiddenIssueReferences keeping the claim it reads; the issue read, the children
// and the listing then name session-priv to alice, and those rows fail naming it.
func TestM5AClaimNamesNoHolderItsReaderMayNotSee(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	issueOf := func(caller privateCaller, key string) (claimRow, string) {
		t.Helper()
		var row claimRow
		body := seed.readJSON(t, caller, "/api/v1/issues/"+key, &row)
		return row, body
	}

	t.Run("the issue read", func(t *testing.T) {
		row, body := issueOf(callerAlice, seed.claimed)
		if got := claimText(row.Claim) + " " + rawText(row.HolderState) + " " + rawText(row.LastSeen) + " " + rawText(row.TakeableAt); got != "null null null null" {
			t.Errorf("alice reads %s's claim and its holder's liveness as %s, want null null null null: session-priv is a session she may not see", seed.claimed, got)
		}
		if strings.Contains(body, sessionOfPrivate) {
			t.Errorf("alice's read of %s names %s: %s", seed.claimed, sessionOfPrivate, body)
		}
		row, _ = issueOf(callerBob, seed.issue[seedPrivate])
		if got := claimText(row.Claim); got != "null" {
			t.Errorf("bob reads %s's claim as %s, want null: alice is a person he may not see", seed.issue[seedPrivate], got)
		}
		row, _ = issueOf(callerCarol, seed.claimed)
		if got := claimText(row.Claim) + " " + rawText(row.HolderState); got != "session:"+sessionOfPrivate+` "live"` {
			t.Errorf("carol reads %s's claim as %s, want session-priv's, live", seed.claimed, got)
		}
	})

	t.Run("the children", func(t *testing.T) {
		row, _ := issueOf(callerAlice, seed.claimed)
		if got := claimText(rowOf(t, row.Children, seed.child, "alice's children of "+seed.claimed).Claim); got != "null" {
			t.Errorf("alice reads child %s's claim as %s, want none", seed.child, got)
		}
		row, _ = issueOf(callerCarol, seed.claimed)
		if got := claimText(rowOf(t, row.Children, seed.child, "carol's children of "+seed.claimed).Claim); got != "session:"+sessionOfPrivate {
			t.Errorf("carol reads child %s's claim as %s, want session-priv's", seed.child, got)
		}
	})

	t.Run("the listing", func(t *testing.T) {
		var rows []claimRow
		body := seed.readJSON(t, callerAlice, "/api/v1/issues?project="+seedPublic, &rows)
		for _, key := range []string{seed.claimed, seed.child} {
			if got := claimText(rowOf(t, rows, key, "alice's listing").Claim); got != "null" {
				t.Errorf("alice's listing reads %s's claim as %s, want null", key, got)
			}
		}
		if strings.Contains(body, sessionOfPrivate) {
			t.Errorf("alice's listing names %s: %s", sessionOfPrivate, body)
		}
		seed.readJSON(t, callerBob, "/api/v1/issues?project="+seedPrivate, &rows)
		if got := claimText(rowOf(t, rows, seed.issue[seedPrivate], "bob's listing").Claim); got != "null" {
			t.Errorf("bob's listing reads %s's claim as %s, want null", seed.issue[seedPrivate], got)
		}
		seed.readJSON(t, callerCarol, "/api/v1/issues?project="+seedPublic, &rows)
		if got := claimText(rowOf(t, rows, seed.claimed, "carol's listing").Claim); got != "session:"+sessionOfPrivate {
			t.Errorf("carol's listing reads %s's claim as %s, want session-priv's", seed.claimed, got)
		}
	})

	t.Run("the priority faults' claims", func(t *testing.T) {
		var faults priorityFaults
		seed.readJSON(t, callerAlice, "/api/v1/priorities", &faults)
		if got := claimText(rowOf(t, faults.WorkUnderNoPriority.Issues, seed.claimed, "alice's first fault").Claim); got != "null" {
			t.Errorf("alice's first fault reads %s's claim as %s, want null", seed.claimed, got)
		}
		seed.readJSON(t, callerCarol, "/api/v1/priorities", &faults)
		if got := claimText(rowOf(t, faults.WorkUnderNoPriority.Issues, seed.claimed, "carol's first fault").Claim); got != "session:"+sessionOfPrivate {
			t.Errorf("carol's first fault reads %s's claim as %s, want session-priv's", seed.claimed, got)
		}
	})

	t.Run("the priority faults' mover", func(t *testing.T) {
		var faults priorityFaults
		body := seed.readJSON(t, callerAlice, "/api/v1/priorities", &faults)
		if got := actorText(rowOf(t, faults.PriorityWorkParked.Issues, seed.parked, "alice's parked fault").MovedBy); got != "null" {
			t.Errorf("alice's parked fault reads %s's mover as %s, want null: session-priv is a session she may not see", seed.parked, got)
		}
		if strings.Contains(body, sessionOfPrivate) {
			t.Errorf("alice's priorities name %s: %s", sessionOfPrivate, body)
		}
		seed.readJSON(t, callerCarol, "/api/v1/priorities", &faults)
		if got := actorText(rowOf(t, faults.PriorityWorkParked.Issues, seed.parked, "carol's parked fault").MovedBy); got != "session:"+sessionOfPrivate {
			t.Errorf("carol's parked fault reads %s's mover as %s, want session-priv", seed.parked, got)
		}
	})

	t.Run("the listing's mover", func(t *testing.T) {
		var rows []claimRow
		body := seed.readJSON(t, callerBob, "/api/v1/issues?project="+seedPrivate+"&moved=true", &rows)
		if got := actorText(rowOf(t, rows, seed.issue[seedPrivate], "bob's moved listing").MovedBy); got != "null" {
			t.Errorf("bob's ?moved=true listing reads %s's mover as %s, want null: alice is a person he may not see", seed.issue[seedPrivate], got)
		}
		if strings.Contains(body, `"alice"`) {
			t.Errorf("bob's ?moved=true listing names alice: %s", body)
		}
		seed.readJSON(t, callerAlice, "/api/v1/issues?project="+seedPublic+"&moved=true", &rows)
		if got := actorText(rowOf(t, rows, seed.parked, "alice's moved listing").MovedBy); got != "null" {
			t.Errorf("alice's ?moved=true listing reads %s's mover as %s, want null", seed.parked, got)
		}
		seed.readJSON(t, callerCarol, "/api/v1/issues?project="+seedPrivate+"&moved=true", &rows)
		if got := actorText(rowOf(t, rows, seed.issue[seedPrivate], "carol's moved listing").MovedBy); got != "user:alice" {
			t.Errorf("carol's ?moved=true listing reads %s's mover as %s, want user:alice", seed.issue[seedPrivate], got)
		}
	})

	t.Run("the 409 of a claim a hidden session holds", func(t *testing.T) {
		refused := seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/issues/"+seed.claimed+"/claim", map[string]any{}, http.StatusConflict)
		body := refused.Body.String()
		var answer issueClaimedError
		if err := json.Unmarshal(refused.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the 409: %v", err)
		}
		if answer.Code != "ISSUE_CLAIMED" || answer.Claim != nil || strings.Contains(body, sessionOfPrivate) || strings.Contains(body, "Private work") {
			t.Errorf("alice's refused claim of %s answers %s, want ISSUE_CLAIMED with a null claim naming neither session-priv nor its title", seed.claimed, body)
		}
		carol := seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.claimed+"/claim", map[string]any{}, http.StatusConflict)
		if !strings.Contains(carol.Body.String(), "session "+sessionOfPrivate+" (Private work)") {
			t.Errorf("carol's refused claim of %s answers %s, want it to name session-priv and its title", seed.claimed, carol.Body.String())
		}
	})

	t.Run("the 409 of a claim a hidden person holds", func(t *testing.T) {
		refused := seed.mustRequest(t, callerBob, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/claim", map[string]any{}, http.StatusConflict)
		var answer issueClaimedError
		if err := json.Unmarshal(refused.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the 409: %v", err)
		}
		if answer.Code != "ISSUE_CLAIMED" || answer.Claim != nil || strings.Contains(refused.Body.String(), "alice") {
			t.Errorf("bob's refused claim of %s answers %s, want ISSUE_CLAIMED with a null claim naming no alice", seed.issue[seedPrivate], refused.Body.String())
		}
	})

	t.Run("the contended answer", func(t *testing.T) {
		alice, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice))
		if err != nil {
			t.Fatalf("resolve what alice sees: %v", err)
		}
		contended := func(key string) string {
			t.Helper()
			response := httptest.NewRecorder()
			seed.server.answerContendedClaim(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/issues/"+key+"/claim", nil), alice, key)
			return strings.ReplaceAll(response.Body.String(), key, "<key>")
		}
		if held, nobodys := contended(seed.claimed), contended(seed.unclaimed); held != nobodys {
			t.Errorf("the contended answer to alice for %s, which session-priv holds, is %s; for %s, which nobody holds, %s: want one answer", seed.claimed, held, seed.unclaimed, nobodys)
		}
	})
}

// timestampsAndIDs matches what differs between two runs of one seed: an RFC 3339 time and a uuid.
var timestampsAndIDs = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// TestM5M6WithNoPrivateProjectTheReadsAnswerAsBefore (R9): once PRIV is public again the deployment
// holds no private project, and alice, a full member, reads every claim, mover, route and route
// holder of the seed as the base answered them: session-priv's claims and move, its 409's name and
// title, role:legion-priv-architect live with session-pub and role:reviewer live with session-priv.
// Each answer is logged with its times and ids blanked, beside the sha256 of what it logs, so a run
// at the base and a run at the head compare byte for byte (the report's diff of the two runs).
//
// Control: leaveOutHiddenIssueReferences hiding every claim whatever the reader; alice's issue read
// then names no claim and the row fails.
func TestM5M6WithNoPrivateProjectTheReadsAnswerAsBefore(t *testing.T) {
	t.Parallel()
	seed := seedHiddenHolders(t, testServerOptions{})
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	logged := func(name, body string) {
		blanked := timestampsAndIDs.ReplaceAllString(body, "<varies>")
		t.Logf("R9 %s sha256 %x: %s", timestampsAndIDs.ReplaceAllString(name, "<id>"), sha256.Sum256([]byte(blanked)), blanked)
	}

	var issue claimRow
	logged("issue read", seed.readJSON(t, callerAlice, "/api/v1/issues/"+seed.claimed, &issue))
	if got := claimText(issue.Claim) + " " + rawText(issue.HolderState) + " " + claimText(rowOf(t, issue.Children, seed.child, "the children").Claim); got != "session:"+sessionOfPrivate+` "live" session:`+sessionOfPrivate {
		t.Errorf("alice reads %s's claim, liveness and child's claim as %s, want session-priv's, live, and session-priv's", seed.claimed, got)
	}
	var rows []claimRow
	logged("listing", seed.readJSON(t, callerAlice, "/api/v1/issues?project="+seedPublic+"&moved=true", &rows))
	if got := claimText(rowOf(t, rows, seed.claimed, "the listing").Claim) + " " + actorText(rowOf(t, rows, seed.parked, "the listing").MovedBy); got != "session:"+sessionOfPrivate+" session:"+sessionOfPrivate {
		t.Errorf("alice's listing reads %s's claim and %s's mover as %s, want session-priv's both", seed.claimed, seed.parked, got)
	}
	var faults priorityFaults
	logged("priorities", seed.readJSON(t, callerAlice, "/api/v1/priorities", &faults))
	if got := claimText(rowOf(t, faults.WorkUnderNoPriority.Issues, seed.claimed, "the first fault").Claim) + " " +
		actorText(rowOf(t, faults.PriorityWorkParked.Issues, seed.parked, "the parked fault").MovedBy); got != "session:"+sessionOfPrivate+" session:"+sessionOfPrivate {
		t.Errorf("alice's priorities read the claim and the mover as %s, want session-priv's both", got)
	}
	refused := seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/issues/"+seed.claimed+"/claim", map[string]any{}, http.StatusConflict)
	logged("409", refused.Body.String())
	if !strings.Contains(refused.Body.String(), "session "+sessionOfPrivate+" (Private work)") {
		t.Errorf("alice's refused claim answers %s, want it to name session-priv and its title", refused.Body.String())
	}
	for key, want := range map[string]string{
		seed.toPrivateRole:  "role:" + privateProjectRole + " live " + sessionOfPublic + " null",
		seed.toHiddenHolder: "role:" + hiddenHolderRole + " live " + sessionOfPrivate + " null",
	} {
		for _, target := range []string{"/api/v1/issues/" + key, "/api/v1/asks/" + seed.asks[key]} {
			got, body := seed.routeOf(t, callerAlice, target)
			logged("route of "+target, body)
			if got != want {
				t.Errorf("alice's GET %s reads the route as %s, want %s", target, got, want)
			}
		}
	}
}
