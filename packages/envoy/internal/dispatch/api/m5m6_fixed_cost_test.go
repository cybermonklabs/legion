package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fixedCostPair is one read of two targets that differ only in whether what they answer names an
// object the reader may not see: hidden names one, none names nothing. warm is a third target of
// the same shape read first, so neither counted read is the first of its kind; each counted read
// is the first of its own target, since a comment list keeps the answer it built.
type fixedCostPair struct {
	read               string
	caller             privateCaller
	warm, hidden, none string
}

// fixedCostSeed is what the pairs read, on seedHiddenHolders' seed: per pair a target naming
// session-priv, bob or a hidden claim, a target naming nothing, and a warming target.
type fixedCostSeed struct {
	hiddenHolderSeed
	pairs []fixedCostPair
	// faultIssue is the one issue of project COST, in progress under no priority with no assignee,
	// that the priorities' first fault lists; claimFault has session-priv claim it.
	faultIssue string
}

func seedFixedCost(t *testing.T, options testServerOptions) fixedCostSeed {
	t.Helper()
	seed := fixedCostSeed{hiddenHolderSeed: seedHiddenHolders(t, options)}
	mintListingToken(t, seed.privateSeed, callerAlicePubToken, seedAlice, []string{seedPublic})
	issue := func(title string, assignee any) string {
		t.Helper()
		key := decodeBody[struct {
			Key string `json:"key"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": seedPublic, "title": title, "force": true,
		}, http.StatusCreated)).Key
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"assignee": assignee}, http.StatusOK)
		return key
	}
	ask := func(key string) string {
		t.Helper()
		return createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/asks", map[string]any{"question": "Which way?"}))
	}
	comment := func(key string, mentions []map[string]string) string {
		t.Helper()
		body := map[string]any{"body": "A word on the work."}
		if mentions != nil {
			body["mentions"] = mentions
		}
		return createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/comments", body))
	}
	message := func(key, target string) {
		t.Helper()
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/messages",
			map[string]any{"body": "A note.", "target": target, "delivery": "btw"}, http.StatusCreated)
	}

	// Followers: an ask session-priv follows, and asks nobody follows.
	followed, unfollowed, warmFollowers := ask(issue("Followed", nil)), ask(issue("Unfollowed", nil)), ask(issue("Unfollowed too", nil))
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/asks/"+followed+"/followers/"+sessionOfPrivate, map[string]any{}, http.StatusNoContent)
	// Ask persons: an ask waiting on bob, whom a token that lists PUB does not see, and asks of
	// unassigned issues, which wait on the project's default assignee.
	waitingOnBob, waitingOnDefault, warmWaiting := ask(issue("Waits on bob", seedBob)), ask(issue("Unassigned", nil)), ask(issue("Unassigned too", nil))
	// Comment sessions: a comment mentioning session-priv, and plain comments.
	mentionsHidden, mentionsNone, mentionsWarm := issue("Mentions session-priv", nil), issue("Mentions nobody", nil), issue("Mentions nobody too", nil)
	comment(mentionsHidden, []map[string]string{{"target": "session:" + sessionOfPrivate}})
	comment(mentionsNone, nil)
	comment(mentionsWarm, nil)
	// Message sessions: a message to session-priv, and messages to a role no session holds.
	messagesHidden, messagesNone, messagesWarm := issue("Messages session-priv", nil), issue("Messages nobody", nil), issue("Messages nobody too", nil)
	message(messagesHidden, "session:"+sessionOfPrivate)
	message(messagesNone, "role:"+unheldRole)
	message(messagesWarm, "role:"+unheldRole)
	// Event payloads: carol routes an issue to session-priv, a route every commit's payload filter
	// reads, and retitles it and issues routed nowhere, so each retitle's issue.updated differs only
	// in its route.
	payloadRouted, payloadNone, payloadWarm := issue("Payload routed", nil), issue("Payload of nobody", nil), issue("Payload of nobody too", nil)
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+payloadRouted, map[string]any{"route": "session:" + sessionOfPrivate}, http.StatusOK)
	for _, key := range []string{payloadRouted, payloadNone, payloadWarm} {
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"title": "Retitled " + key}, http.StatusOK)
	}
	awaitBackgroundWrites(t, seed.server)

	// logOf is the log read of key's last issue.updated alone. It fails unless that payload names
	// exactly what the pair needs, read from the stored payload itself rather than from the filter
	// under test: under every key a payload may name an object by, nothing but route
	// session:session-priv for the routed issue, and nothing at all for the others.
	logOf := func(key string, routed bool) string {
		t.Helper()
		var id int64
		var raw []byte
		if err := seed.database.Pool.QueryRow(t.Context(),
			`select id, payload from events where issue_key = $1 and type = 'issue.updated' order by id desc limit 1`, key).Scan(&id, &raw); err != nil {
			t.Fatalf("read the last issue.updated of %s: %v", key, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode the issue.updated of %s: %v", key, err)
		}
		for _, field := range []string{"blocked_by", "references_changed", "assignee", "asked_of", "thread_logins", "route", "target",
			"thread_target", "suppressed_route", "suppressed_route_session_id", "claim", "previous_claim", "mentions", "deliveries"} {
			value, want := payload[field], any(nil)
			if field == "route" && routed {
				want = "session:" + sessionOfPrivate
			}
			if list, ok := value.([]any); ok && len(list) == 0 {
				value = nil
			}
			if value != want {
				t.Fatalf("the issue.updated of %s carries %s %v, want %v: %s", key, field, value, want, raw)
			}
		}
		return "/api/v1/events/log?" + url.Values{"after": {strconv.FormatInt(id-1, 10)}, "limit": {"1"}}.Encode()
	}

	// Priority faults: COST's one issue in flight under no priority, with no assignee.
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": "COST", "name": "Project COST"}, http.StatusCreated)
	seed.faultIssue = decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "COST", "title": "Work in flight", "force": true,
	}, http.StatusCreated)).Key
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+seed.faultIssue,
		map[string]any{"assignee": nil, "status": "in_progress"}, http.StatusOK)

	seed.pairs = []fixedCostPair{
		{"followers", callerAlice, "/api/v1/asks/" + warmFollowers + "/followers", "/api/v1/asks/" + followed + "/followers", "/api/v1/asks/" + unfollowed + "/followers"},
		{"ask persons", callerAlicePubToken, "/api/v1/asks/" + warmWaiting, "/api/v1/asks/" + waitingOnBob, "/api/v1/asks/" + waitingOnDefault},
		{"comment sessions", callerAlice, "/api/v1/issues/" + mentionsWarm + "/comments", "/api/v1/issues/" + mentionsHidden + "/comments", "/api/v1/issues/" + mentionsNone + "/comments"},
		{"message sessions", callerAlice, "/api/v1/issues/" + messagesWarm + "/messages", "/api/v1/issues/" + messagesHidden + "/messages", "/api/v1/issues/" + messagesNone + "/messages"},
		{"event payloads", callerAlice, logOf(payloadWarm, false), logOf(payloadRouted, true), logOf(payloadNone, false)},
	}
	return seed
}

// claimFault has session-priv claim COST's issue under a credential that proves it in COST.
func (s fixedCostSeed) claimFault(t *testing.T) {
	t.Helper()
	s.asSession(t, sessionOfPrivate, "COST", http.MethodPost, "/api/v1/issues/"+s.faultIssue+"/claim", map[string]any{}, http.StatusOK)
}

// readCost is the statements one request of caller's GET target sends, and how many of them are
// the rule of references' statement (requestStatements).
func readCost(t *testing.T, seed privateSeed, counter *requestStatements, caller privateCaller, target string) (int64, int64) {
	t.Helper()
	ctx := context.WithValue(t.Context(), requestStatementsKey{}, true)
	before, seenBefore := counter.sent.Load(), counter.visibility.Load()
	if response := seed.requestIn(t, ctx, caller, http.MethodGet, target, nil); response.Code != http.StatusOK {
		t.Fatalf("%s: GET %s = %d %s", caller, target, response.Code, response.Body.String())
	}
	return counter.sent.Load() - before, counter.visibility.Load() - seenBefore
}

// fixedCosts reads every pair, and the priorities' faults of COST before and after session-priv
// claims its issue, as each pair's caller (alice for the priorities, a deployment read), and
// answers each read's cost as "hidden N, none M", with the rule of references' statements.
func fixedCosts(t *testing.T, seed fixedCostSeed, counter *requestStatements) map[string][2][2]int64 {
	t.Helper()
	costs := map[string][2][2]int64{}
	for _, pair := range seed.pairs {
		readCost(t, seed.privateSeed, counter, pair.caller, pair.warm)
		hidden, hiddenSeen := readCost(t, seed.privateSeed, counter, pair.caller, pair.hidden)
		none, noneSeen := readCost(t, seed.privateSeed, counter, pair.caller, pair.none)
		costs[pair.read] = [2][2]int64{{hidden, hiddenSeen}, {none, noneSeen}}
	}
	faults := "/api/v1/priorities?project=COST"
	readCost(t, seed.privateSeed, counter, callerAlice, faults)
	none, noneSeen := readCost(t, seed.privateSeed, counter, callerAlice, faults)
	seed.claimFault(t)
	hidden, hiddenSeen := readCost(t, seed.privateSeed, counter, callerAlice, faults)
	costs["priority faults"] = [2][2]int64{{hidden, hiddenSeen}, {none, noneSeen}}
	for read, cost := range costs {
		t.Logf("%s: hidden %d (the rule of references' statement %d), none %d (%d)", read, cost[0][0], cost[0][1], cost[1][0], cost[1][1])
	}
	return costs
}

// TestM5M6EveryVisibilityReadCostsWhatNamingNothingCosts: every read that leaves out what its
// reader does not see makes its one visibility statement (seenReferences) whatever its rows name,
// so an answer naming a hidden object costs what one naming nothing costs: an ask's followers
// (session-priv following, to alice), an issue's comments (a mention of session-priv, to alice),
// its messages (to session-priv, to alice), an event's payload read from the log (an issue.updated
// carrying route session:session-priv, against one naming nothing at all: logOf checks both), and the
// priorities' faults (a row session-priv claims). alice, an open member, does not see
// session-priv, which acted in PRIV.
//
// An ask's persons (waiting on bob, to alice's token that lists PUB, which does not see bob, a
// restricted member of PRIV alone) are counted too, but that pair cannot tell the two reads apart:
// every ask waits on a person (askedOfPersonExpression ends at the project's default assignee,
// which every project names), so its none side names a person as well, and its read was queued at
// the base too. The pair holds that the batched read stays one statement either way.
//
// Control: seenReferences skipping a set that names nothing (its read made only when named holds
// something); every pair but the ask's persons then costs one statement more for its hidden
// target, and the row fails naming each.
func TestM5M6EveryVisibilityReadCostsWhatNamingNothingCosts(t *testing.T) {
	t.Parallel()
	counter := &requestStatements{}
	seed := seedFixedCost(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	for read, cost := range fixedCosts(t, seed, counter) {
		if cost[0][0] != cost[1][0] {
			t.Errorf("%s: the read naming a hidden object costs %d statements and the one naming nothing %d: want the same", read, cost[0][0], cost[1][0])
		}
		if cost[0][1] == 0 {
			t.Errorf("%s: the read naming a hidden object made no rule-of-references statement, so the count measures nothing", read)
		}
	}
}

// TestM5M6WithNoPrivateProjectEveryVisibilityReadCostsAsBefore (R9): with PRIV public alice, a
// full member, sees every object, so she reads every pair, and the faults, making no rule of
// references' statement at all, each pair at one cost, as the base read them. Every count is
// logged for the report's comparison of the two commits.
//
// Control: the comments' visibility read made for a reader that sees every object. Two guards stand
// in its way (leaveOutHiddenCommentSessions' first test and seenReferences'), so the control drops
// both; alice's comment reads then make the statement and the row fails naming them.
func TestM5M6WithNoPrivateProjectEveryVisibilityReadCostsAsBefore(t *testing.T) {
	t.Parallel()
	counter := &requestStatements{}
	seed := seedFixedCost(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), counter)})
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for index := range seed.pairs {
		seed.pairs[index].caller = callerAlice
	}
	for read, cost := range fixedCosts(t, seed, counter) {
		if cost[0][1] != 0 || cost[1][1] != 0 {
			t.Errorf("PRIV public: alice's %s reads make the rule of references' statement %d and %d times, want none: she sees every object", read, cost[0][1], cost[1][1])
		}
		if cost[0][0] != cost[1][0] {
			t.Errorf("PRIV public: alice's %s reads cost %d and %d statements, want the same, as before", read, cost[0][0], cost[1][0])
		}
	}
}
