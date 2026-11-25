package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestNoWriteReachesAHiddenObject walks every write row whose pattern names an object ({key}, {id}
// or {session_id}), whatever its write class, on verification v16's seed
// (seedPrivateProject). Each row is called twice as one caller: once naming PRIV's object of each
// kind its Probe names, and once naming an object nobody holds. The two answers must be the same
// status and the same bytes (the second's ids written as the first's), and neither call may change
// a row of any table: the event log, the graph's tuples and the object itself among them. The call
// naming nothing must be answered as the row answers a missing object, so a body the row refuses
// before its read cannot pass the row. The two calls must also do the same work before they answer
// (callPair): the same statements, each reading, locking or writing as many rows, and the same
// calls of the Envoy listener, so no row lock, read or listener call is made of a hidden object
// before the row knows its caller may not see it. PRIV's issue is claimed by s1, a session the
// listener holds live, so a claim that asked the listener about its holder first would ask it here.
//
// Seven callers walk it. alice, at a browser, is an open viewer who sees PUB and no object of PRIV,
// and every session but one that has acted in PRIV (privateSession). alice's token that lists PRIV
// and her token that lists PUB are full members' credentials, so each is an open viewer that sees
// PUB alone (alice does not see PRIV), and each list holds its writes from the deployment's rows,
// which are refused it whatever they name. alice's broker token scoped to PUB is an open viewer
// that sees PUB whose writes are held to PUB (namedProjectOf), so it walks the rows of a project's
// object, which its scope refuses before the handler: the scope's read of the object's project
// must answer PRIV's object as one nobody holds. bob, a restricted member, and bob's token that
// lists PRIV each see PRIV alone, so a session that has acted in PUB alone is hidden from them too:
// they walk the rows whose one hidden object is a session. Every row is walked by at least one of
// them.
//
// A row whose path names a session beside another object - a subscriber of an issue or a document,
// a follower of an ask - is walked once more by each caller that sees a project, with that other
// object one of the project it sees and the session alone hidden from it (compareSessionBeside): a
// row that reads the other object first answers a hidden one before it ever reads the session.
//
// Control: projectResolverFor's reads without their project clause: PRIV's objects resolve to
// PRIV for alice's broker token, its refusal names PRIV where an object nobody holds is refused
// naming no project, and the walk fails naming each row of a project's object.
func TestNoWriteReachesAHiddenObject(t *testing.T) {
	walk := seedHiddenWrites(t)
	rows := objectWriteRows()
	if problems := writeProbeProblems(rows); len(problems) > 0 {
		t.Fatalf("writeProbes does not describe the write rows that name an object:\n%s", strings.Join(problems, "\n"))
	}
	walked := map[string]bool{}
	besides := 0
	// A named token holds no OAuth scope, so the gate refuses it every row that needs one before
	// any read, and the test hooks' rows are no production write: the scope's own read is walked on
	// the rest. bob's token holds none either, a restricted member's token (R16), so it walks no
	// row that needs one, nor a follower row, whose handler asks for the person scope before it
	// reads a session other than the caller's own; bob at his browser walks those.
	scoped := map[string]bool{}
	for _, route := range walk.server.routes() {
		scoped[route.Method+" "+route.Pattern] = len(route.OAuthScopes) > 0
	}
	for _, caller := range []struct {
		who          privateCaller
		sight        walkSight
		sessionsOnly bool
		// sees is the project whose objects the caller sees, "" for none walked beside a session.
		sees string
		// scopeless is a credential that holds no scope, other than the named token.
		scopeless bool
	}{
		{callerAlice, walkSight{open: true}, false, seedPublic, false},
		{callerAlicePrivToken, walkSight{open: true, held: true}, false, seedPublic, false},
		{callerAlicePubToken, walkSight{open: true, held: true}, false, seedPublic, false},
		{callerAliceNamedToken, walkSight{open: true, held: true, named: true}, false, "", false},
		{callerBob, walkSight{}, true, seedPrivate, false},
		{callerBobPrivToken, walkSight{held: true}, true, seedPrivate, true},
	} {
		for _, name := range rows {
			probe := writeProbes[name]
			if caller.sight.named && (writeClasses[name] != scopeProject || scoped[name] || strings.Contains(name, "/_test/")) {
				continue
			}
			if caller.scopeless && (scoped[name] || strings.Contains(name, "/followers/")) {
				continue
			}
			if caller.sees != "" && probe.namesASessionBeside() {
				walked[name] = true
				besides++
				walk.compareSessionBeside(t, caller.who, caller.sight, caller.sees, name)
			}
			if !probe.hides(caller.sight) || caller.sessionsOnly && !probe.hidesOnlySessions(caller.sight) {
				continue
			}
			walked[name] = true
			walk.compare(t, caller.who, caller.sight, name)
		}
	}
	for _, name := range rows {
		if !walked[name] {
			t.Errorf("%s: no caller of the walk has an object of it hidden, so nothing called it", name)
		}
	}
	if besides == 0 {
		t.Errorf("no row names a session beside another object, so no session was walked beside an object its caller sees")
	}
	t.Logf("walked %d of the %d write rows that name an object, %d calls of them with a session alone hidden", len(walked), len(rows), besides)

	// What makes the subscriber and follower rows able to fail: the walk's listener holds s1, s-pub
	// and privateSession subscribed to both projects' issue and document, s-pub follows PRIV's ask
	// and privateSession PUB's, so carol, who sees both projects and every session, removes each. A
	// listener that held no such interest, or an ask no hidden session followed, would answer every
	// removal "not subscribed" or "does not follow", a hidden session's and a missing one's alike.
	for _, path := range []string{
		"/api/v1/issues/" + walk.fills[probeIssue].hidden + "/subscribers/s1",
		"/api/v1/artifacts/" + walk.fills[probeDocument].hidden + "/subscribers/s1",
		"/api/v1/issues/" + walk.fills[probeIssue].hidden + "/subscribers/s-pub",
		"/api/v1/artifacts/" + walk.fills[probeDocument].hidden + "/subscribers/s-pub",
		"/api/v1/asks/" + walk.fills[probeAsk].hidden + "/followers/s-pub",
		"/api/v1/issues/" + walk.fills[probeIssue].public + "/subscribers/" + privateSession,
		"/api/v1/artifacts/" + walk.fills[probeDocument].public + "/subscribers/" + privateSession,
		"/api/v1/asks/" + walk.fills[probeAsk].public + "/followers/" + privateSession,
	} {
		if removed := walk.request(t, callerCarol, http.MethodDelete, path, nil); removed.Code != http.StatusNoContent {
			t.Errorf("carol: DELETE %s = %d %s, want 204: the walk's listener or its asks hold no such subscriber or follower, so its rows that name it prove nothing",
				path, removed.Code, strings.TrimSpace(removed.Body.String()))
		}
	}
}

// bodyReferences is every field of a write's body that names an object of a project, with a call
// alice may make that names it: the row, the path of an object she sees (PUB's issue, project or
// conversation), and the body with %s where the named object goes. Every row refuses an object
// nobody holds, so it refuses PRIV's alike: the design gates refuse a document their writer may
// not see as one naming no document (refuseUnseenGateDocuments). A broadcast's session names
// privateSession, which has acted in PRIV, to alice, and s-pub, which has acted in no project, to
// her token that lists PUB (probeFill.hiddenValue). An import's external reference names no object
// of a project: since #4457 an import names its project in its body and reads no repository
// mapping, so a repository a mapping of PRIV holds is a name like any other there.
var bodyReferences = []struct {
	row, field, path string
	object           probeObject
	body             string
}{
	{"POST /api/v1/issues", "parent", "/api/v1/issues", probeIssue, `{"project":"PUB","title":"A walked child","parent":"%s"}`},
	{"POST /api/v1/issues", "blocked_by", "/api/v1/issues", probeIssue, `{"project":"PUB","title":"A walked wait","blocked_by":["%s"]}`},
	{"PATCH /api/v1/issues/{key}", "parent", "/api/v1/issues/PUB-1", probeIssue, `{"parent":"%s"}`},
	{"PATCH /api/v1/issues/{key}", "blocked_by", "/api/v1/issues/PUB-1", probeIssue, `{"blocked_by":["%s"]}`},
	{"PATCH /api/v1/issues/{key}", "rank.after", "/api/v1/issues/PUB-1", probeIssue, `{"rank":{"after":"%s"}}`},
	{"POST /api/v1/issues/{key}/comments", "reply_to", "/api/v1/issues/PUB-1/comments", probeComment, `{"body":"A walked reply.","reply_to":"%s"}`},
	{"POST /api/v1/issues/{key}/comments", "ask_id", "/api/v1/issues/PUB-1/comments", probeAsk, `{"body":"A walked answer.","ask_id":"%s"}`},
	{"POST /api/v1/issues/{key}/messages", "in_reply_to", "/api/v1/issues/PUB-1/messages", probeMessage, `{"body":"A walked reply.","in_reply_to":"%s"}`},
	{"POST /api/v1/agents/{session_id}/messages", "in_reply_to", "/api/v1/agents/s1/messages", probeMessage, `{"body":"A walked reply.","delivery":"btw","in_reply_to":"%s"}`},
	{"PUT /api/v1/issues/{key}/design-gates", "gates[].artifact_id", "/api/v1/issues/PUB-1/design-gates", probeDocument, `{"gates":[{"step":"spec","artifact_id":"%s"}]}`},
	{"POST /api/v1/projects/{key}/related", "exclude_issue", "/api/v1/projects/PUB/related", probeIssue, `{"text":"walked","exclude_issue":"%s"}`},
	{"POST /api/v1/broadcasts", "session_ids", "/api/v1/broadcasts", probeSession, `{"body":"To walked sessions.","delivery":"btw","session_ids":["%s"]}`},
}

// TestNoBodyReachesAHiddenObject: every field of a write's body that names an object
// (bodyReferences) answers PRIV's object exactly as an object nobody holds - the same status and
// bytes, the same work, and no table written - while the path names an object the caller sees, to
// alice and to her token that lists PUB, an open viewer that sees PUB alone and names its session
// (probeActor). An object nobody holds is refused there, and PRIV's alike.
func TestNoBodyReachesAHiddenObject(t *testing.T) {
	walk := seedHiddenWrites(t)
	if walk.issue[seedPublic] != "PUB-1" {
		t.Fatalf("PUB's issue is %s; the walk's paths name PUB-1", walk.issue[seedPublic])
	}
	for _, caller := range []struct {
		who  privateCaller
		open bool
	}{{callerAlice, true}, {callerAlicePubToken, true}} {
		for _, reference := range bodyReferences {
			name := reference.row + " (" + reference.field + ")"
			fill := walk.fills[reference.object]
			hidden := fill.hiddenValue(caller.open)
			method, _, _ := strings.Cut(reference.row, " ")
			body := reference.body
			if caller.who.login() == "" {
				body = withProbeActor(body)
			}
			missing := walk.callPair(t, caller.who, name, method, reference.path, reference.path,
				json.RawMessage(fmt.Sprintf(body, hidden)), json.RawMessage(fmt.Sprintf(body, fill.missing)),
				map[string]string{fill.missing: hidden})
			if missing.Code < http.StatusBadRequest {
				t.Errorf("%s as %s: an object nobody holds answered %d %s; want a refusal", name, caller.who, missing.Code,
					strings.TrimSpace(missing.Body.String()))
			}
		}
	}
	t.Logf("walked %d body fields that name an object", len(bodyReferences))
}

// TestARefusalOfAHiddenObjectNamesNone: the writes a hidden object refuses where an object nobody
// holds would be taken, because the two cannot both stand, are refused naming nothing of it, ask
// the listener nothing and write nothing. A URL links one issue, so a link of the URL PRIV's issue
// holds is 409 EXTERNAL_LINK_TAKEN in the words a link no reader may see is refused in, and an
// import of it is refused naming no issue (a URL nobody holds is a 201: the residual these rows
// keep), to alice and to her token that lists PUB. A wait may not close a ring, so a wait that
// closes one through PRIV's issue is refused naming only the issue written, and a document is
// removed only while nothing links to it, so a removal of one only PRIV's comment cites is refused
// naming nothing that cites it.
func TestARefusalOfAHiddenObjectNamesNone(t *testing.T) {
	walk := seedHiddenWrites(t)
	both := []privateCaller{callerAlice, callerAlicePubToken}
	newPublicIssue := func(title string) string {
		t.Helper()
		return decodeBody[struct {
			Key string `json:"key"`
		}](t, walk.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
			map[string]string{"project": seedPublic, "title": title}, http.StatusCreated)).Key
	}
	// A ring through PRIV's issue that a wait of closing on waiting would close: waiting waits
	// on PRIV's issue, which waits on closing.
	waiting, closing := newPublicIssue("Waits on PRIV"), newPublicIssue("Would close the ring")
	walk.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+waiting,
		map[string]any{"blocked_by": []string{walk.issue[seedPrivate]}}, http.StatusOK)
	walk.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+walk.issue[seedPrivate],
		map[string]any{"blocked_by": []string{closing}}, http.StatusOK)
	// A document of PUB's issue, empty as typing its title makes it, that only a comment of
	// PRIV's cites.
	cited := decodeBody[uploadedArtifact](t, walk.mustRequest(t, callerCarol, http.MethodPost,
		"/api/v1/issues/"+walk.issue[seedPublic]+"/artifacts", map[string]string{"name": "cited.md", "content": ""}, http.StatusCreated)).Artifact
	walk.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPrivate]+"/comments",
		map[string]any{"body": "See dispatch://" + walk.issue[seedPublic] + "/artifact/" + cited.Slug + " for the rest."}, http.StatusCreated)
	for _, refusal := range []struct {
		name, method, path string
		body               map[string]any
		callers            []privateCaller
		hides              []string
		status             int
		code, words        string
	}{
		{"a link of the URL PRIV's issue holds", http.MethodPatch, "/api/v1/issues/" + walk.issue[seedPublic],
			map[string]any{"external_links": []map[string]string{{"url": privateHeldURL}}}, both, nil,
			http.StatusConflict, "EXTERNAL_LINK_TAKEN", privateHeldURL + " is already linked from another issue"},
		{"an import of the URL PRIV's issue holds", http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPublic, "title": "A walked import", "external": "acme/w_pub#9"}, both, nil, 0, "", ""},
		{"a wait that closes a ring through PRIV's issue", http.MethodPatch, "/api/v1/issues/" + closing,
			map[string]any{"blocked_by": []string{waiting}}, both, nil,
			http.StatusConflict, "DEPENDENCY_CYCLE", closing + " would wait on itself through blocked_by and parent links"},
		{"a removal of a document only PRIV's comment cites", http.MethodDelete, "/api/v1/artifacts/" + cited.ID,
			map[string]any{}, both, nil, http.StatusConflict, "ARTIFACT_NOT_UNTOUCHED",
			"artifact " + cited.Slug + " (cited.md) is not untouched: something in Dispatch links to it; a document is removed only while it holds nothing"},
	} {
		for _, caller := range refusal.callers {
			before := walk.tableDigests(t)
			sent := refusal.body
			if caller.login() == "" {
				sent = map[string]any{"actor": map[string]string{"kind": "session", "id": "s-probe"}}
				for field, value := range refusal.body {
					sent[field] = value
				}
			}
			answer, _, calls := walk.recorded(t, caller, refusal.method, refusal.path, sent)
			t.Logf("%s as %s: %d %s", refusal.name, caller, answer.Code, strings.TrimSpace(answer.Body.String()))
			if changed := changedTables(before, walk.tableDigests(t)); len(changed) > 0 {
				t.Errorf("%s as %s: answered %d %s and changed %v", refusal.name, caller, answer.Code, strings.TrimSpace(answer.Body.String()), changed)
			}
			if len(calls) > 0 {
				t.Errorf("%s as %s: asked the listener %v", refusal.name, caller, calls)
			}
			body := answer.Body.String()
			if answer.Code < http.StatusBadRequest {
				t.Errorf("%s as %s: answered %d %s, want a refusal", refusal.name, caller, answer.Code, strings.TrimSpace(body))
			}
			for _, hidden := range append([]string{walk.issue[seedPrivate], `"` + seedPrivate + `"`, " " + seedPrivate}, refusal.hides...) {
				if strings.Contains(body, hidden) {
					t.Errorf("%s as %s: answered %d %s, naming %s", refusal.name, caller, answer.Code, strings.TrimSpace(body), strings.TrimSpace(hidden))
				}
			}
			var got refusalAnswer
			_ = json.Unmarshal(answer.Body.Bytes(), &got)
			if refusal.status != 0 && (answer.Code != refusal.status || got.Code != refusal.code || got.Error != refusal.words) {
				t.Errorf("%s as %s: answered %d %s, want %d %s %q", refusal.name, caller, answer.Code, strings.TrimSpace(body),
					refusal.status, refusal.code, refusal.words)
			}
		}
	}
}

// TestAGoingRunOfAHiddenProjectAnswersAsOneOfNoProject: a going run asked of a project its asker
// cannot see holds nothing of that project, so it answers its asker exactly as a run asked of a
// key no project holds - its create and its stream alike - and asks the listener nothing. A run
// asked of a project its asker saw is held to what the asker sees afterwards
// (TestADocumentOfAPrivateProjectAnswersAsMissing).
func TestAGoingRunOfAHiddenProjectAnswersAsOneOfNoProject(t *testing.T) {
	walk := seedHiddenWrites(t)
	run := func(project string) (string, *httptest.ResponseRecorder, *httptest.ResponseRecorder, []string) {
		t.Helper()
		created, _, calls := walk.recorded(t, callerAlice, http.MethodPost, "/api/v1/going/requests",
			map[string]any{"words": "What do the runbook steps say?", "project": project})
		var request struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(created.Body.Bytes(), &request)
		if created.Code != http.StatusCreated || request.ID == "" {
			return "", created, nil, calls
		}
		awaitBackgroundWrites(t, walk.server)
		stream, _, streamed := walk.recorded(t, callerAlice, http.MethodGet, "/api/v1/going/requests/"+request.ID+"/stream", nil)
		return request.ID, created, stream, append(calls, streamed...)
	}
	hiddenID, hiddenCreate, hiddenStream, hiddenCalls := run(seedPrivate)
	missingID, missingCreate, missingStream, missingCalls := run("NOPE")
	if hiddenCreate.Code != missingCreate.Code {
		t.Fatalf("alice asks a run of PRIV: %d %s; of NOPE: %d %s", hiddenCreate.Code, hiddenCreate.Body.String(),
			missingCreate.Code, missingCreate.Body.String())
	}
	if missingStream == nil {
		t.Fatalf("alice's run of NOPE was not created: %d %s", missingCreate.Code, missingCreate.Body.String())
	}
	written := map[string]string{missingID: hiddenID, `"NOPE"`: `"` + seedPrivate + `"`}
	if got, want := maskTimes(hiddenCreate.Body.String()), maskTimes(rewritten([]string{missingCreate.Body.String()}, written)[0]); got != want {
		t.Errorf("alice's run of PRIV is created as %s, and her run of NOPE as %s", got, want)
	}
	want := maskTimes(rewritten([]string{missingStream.Body.String()}, written)[0])
	if got := maskTimes(hiddenStream.Body.String()); hiddenStream.Code != missingStream.Code || got != want {
		t.Errorf("the stream of alice's run of PRIV answers %d %s, and of her run of NOPE %d %s",
			hiddenStream.Code, got, missingStream.Code, want)
	}
	if len(hiddenCalls) != 0 || len(missingCalls) != 0 {
		t.Errorf("alice's run of PRIV asked the listener %v, and her run of NOPE %v, want nothing of either", hiddenCalls, missingCalls)
	}
	// The recorder sees a call the listener is asked: alice's message to s-pub is delivered through it.
	if _, _, delivered := walk.recorded(t, callerAlice, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/messages",
		json.RawMessage(`{"body":"An open viewer's message.","target":"session:s-pub","delivery":"btw"}`)); len(delivered) == 0 {
		t.Errorf("alice's message to s-pub recorded no listener call, so the record above sees nothing")
	}
}

// personAndSessionReferences is every field of a write's body that names a person or a session,
// with a call a caller that sees one project alone may make that names it, on that project's
// objects: the row, the path (hiddenWriteSeed.on), the body with %s where the person or session
// goes. owner is a call only PRIV's owner makes; the owner is a full member, so each credential of
// hers is an open viewer, which sees every person, and no caller of the walk has a person hidden
// there. writes is a call that writes either way - a message or a mention, each recorded and its
// delivery failed - and is compared by its deliveries (deliveryPart); every other call is refused
// either way, writing nothing, and compared whole. open is a call alice makes too, an open viewer
// who sees PUB, on PUB's objects, naming privateSession, which has acted in PRIV; her broadcast to
// it is walked with the body references (bodyReferences), which hold the listener calls of a pair
// too. A delivery's `target` (POST /api/v1/comments/{id}/deliveries) names no session of its own:
// it picks one of the comment's mentions, and its send resolves through the same
// resolveDeliveryTarget.
var personAndSessionReferences = []struct {
	row, field, path string
	object           probeObject
	body             string
	owner            bool
	writes           bool
	open             bool
}{
	{"POST /api/v1/issues", "assignee", "/api/v1/issues", probePerson,
		`{` + probeActor + `,"project":"PRIV","title":"A walked issue","assignee":"%s"}`, false, false, false},
	{"PATCH /api/v1/issues/{key}", "assignee", "/api/v1/issues/{issue}", probePerson,
		`{` + probeActor + `,"assignee":"%s"}`, false, false, false},
	{"PUT /api/v1/asks/{id}/asked-of", "asked_of", "/api/v1/asks/{ask}/asked-of", probePerson,
		`{"asked_of":"%s"}`, false, false, false},
	{"PUT /api/v1/projects/{key}/default-assignee", "assignee", "/api/v1/projects/{project}/default-assignee", probePerson,
		`{"assignee":"%s"}`, false, false, false},
	{"PATCH /api/v1/projects/{key}/access", "owner", "/api/v1/projects/{project}/access", probePerson,
		`{"owner":"%s"}`, true, false, false},
	{"POST /api/v1/priorities", "owner", "/api/v1/priorities", probePerson,
		`{` + probeActor + `,"title":"A walked priority","owner":"%s"}`, false, false, false},
	{"PUT /api/v1/access-groups/{name}", "members", "/api/v1/access-groups/walked-group", probePerson,
		`{"members":["%s"]}`, false, false, false},
	{"POST /api/v1/issues/{key}/messages", "target", "/api/v1/issues/{issue}/messages", probeSession,
		`{` + probeActor + `,"body":"A walked message.","target":"session:%s","delivery":"btw"}`, false, true, true},
	{"POST /api/v1/issues/{key}/comments", "mentions[].target", "/api/v1/issues/{issue}/comments", probeSession,
		`{` + probeActor + `,"body":"A walked mention.","mentions":[{"target":"session:%s"}]}`, false, true, true},
	{"POST /api/v1/issues/{key}/messages", "target (a role)", "/api/v1/issues/{issue}/messages", probeRole,
		`{` + probeActor + `,"body":"A walked message.","target":"role:%s","delivery":"btw"}`, false, true, false},
	{"POST /api/v1/issues/{key}/comments", "mentions[].target (a role)", "/api/v1/issues/{issue}/comments", probeRole,
		`{` + probeActor + `,"body":"A walked mention.","mentions":[{"target":"role:%s"}]}`, false, true, false},
	{"POST /api/v1/broadcasts", "session_ids", "/api/v1/broadcasts", probeSession,
		`{"body":"To walked sessions.","delivery":"btw","session_ids":["%s"]}`, false, false, false},
	{"POST /api/v1/delivery/session-groups", "sessions", "/api/v1/delivery/session-groups", probeSession,
		`{` + probeActor + `,"name":"walked","sessions":["%s"]}`, false, false, true},
}

// TestNoBodyNamesAHiddenPersonOrSession: every field of a write's body that names a person or a
// session (personAndSessionReferences) answers one the caller may not see exactly as one nobody
// holds, to the callers that see PRIV alone and are not open - bob, a restricted member, and bob's
// token that lists PRIV - and a role whose holder it may not see as a role nobody holds; and a
// session to alice, an open viewer, who does not see privateSession, which has acted in PRIV.
// A message to a session the caller may not see, and that message's Retry, queue nothing on the
// session's own subject (queueForUnknownSession), which would hand the message to the session
// once it registers again: the recorder the walk's server publishes through holds no envelope
// there. A session nobody holds is refused alike, so the two answers stay one.
// An open viewer is answered as before: alice's message to s-pub and to the role it holds are
// delivered, which also shows the walk's listener holds both, and she assigns dana.
func TestNoBodyNamesAHiddenPersonOrSession(t *testing.T) {
	walk := seedHiddenWrites(t)
	type walker struct {
		who     privateCaller
		open    bool
		project string
	}
	for _, reference := range personAndSessionReferences {
		callers := []walker{{callerBob, false, seedPrivate}, {callerBobPrivToken, false, seedPrivate}}
		if reference.owner {
			callers = nil
		}
		if reference.open {
			callers = append(callers, walker{callerAlice, true, seedPublic})
		}
		fill := walk.fills[reference.object]
		method, _, _ := strings.Cut(reference.row, " ")
		for _, caller := range callers {
			name := reference.row + " (" + reference.field + ")"
			hiddenID := fill.hiddenValue(caller.open)
			path := walk.on(reference.path, caller.project)
			// A call refused either way writes nothing: the tables are digested around it, once the
			// work an earlier call left running (a mention's delivery) has settled (tableDigests).
			var before map[string]string
			if !reference.writes {
				before = walk.tableDigests(t)
			}
			hidden := walk.request(t, caller.who, method, path, json.RawMessage(fmt.Sprintf(reference.body, hiddenID)))
			missing := walk.request(t, caller.who, method, path, json.RawMessage(fmt.Sprintf(reference.body, fill.missing)))
			if !reference.writes {
				if changed := changedTables(before, walk.tableDigests(t)); len(changed) > 0 {
					t.Errorf("%s as %s: a call refused either way changed %v", name, caller.who, changed)
				}
			}
			got, want := hidden.Body.String(), strings.ReplaceAll(missing.Body.String(), fill.missing, hiddenID)
			if reference.writes {
				got, want = deliveryPart(t, hidden.Body.Bytes()), strings.ReplaceAll(deliveryPart(t, missing.Body.Bytes()), fill.missing, hiddenID)
			}
			if hidden.Code != missing.Code || got != want {
				t.Errorf("%s as %s: %s answers %d %s, and one nobody holds %d %s", name, caller.who, hiddenID,
					hidden.Code, strings.TrimSpace(hidden.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
			}
			if succeeded := missing.Code < http.StatusBadRequest; succeeded != reference.writes {
				t.Errorf("%s as %s: one nobody holds answered %d %s; want a write: %t", name, caller.who,
					missing.Code, strings.TrimSpace(missing.Body.String()), reference.writes)
			}
			if reference.writes && reference.object == probeSession && strings.HasSuffix(reference.row, "/messages") {
				walk.assertNothingQueuedOn(t, name+" as "+string(caller.who), hiddenID)
				// The Retry of each (POST /api/v1/messages/{id}/deliveries) reads the target as its
				// caller reads it, and answers the two as one too.
				retry := func(message *httptest.ResponseRecorder) *httptest.ResponseRecorder {
					return walk.request(t, caller.who, http.MethodPost, "/api/v1/messages/"+createdID(t, message)+"/deliveries",
						json.RawMessage(`{`+probeActor+`,"delivery":"btw"}`))
				}
				hiddenRetry, missingRetry := retry(hidden), retry(missing)
				got, want := deliveryPart(t, hiddenRetry.Body.Bytes()), strings.ReplaceAll(deliveryPart(t, missingRetry.Body.Bytes()), fill.missing, hiddenID)
				if hiddenRetry.Code != missingRetry.Code || got != want {
					t.Errorf("%s as %s: the Retry naming %s answers %d %s, and one nobody holds %d %s", name, caller.who, hiddenID,
						hiddenRetry.Code, strings.TrimSpace(hiddenRetry.Body.String()), missingRetry.Code, strings.TrimSpace(missingRetry.Body.String()))
				}
				walk.assertNothingQueuedOn(t, name+" retried as "+string(caller.who), hiddenID)
			}
		}
	}
	for _, target := range []string{"session:s-pub", "role:" + walkedRole} {
		sent := walk.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/messages",
			json.RawMessage(`{"body":"An open viewer's message.","target":"`+target+`","delivery":"btw"}`))
		if part := deliveryPart(t, sent.Body.Bytes()); sent.Code != http.StatusCreated || strings.Contains(part, "no live session") ||
			strings.Contains(part, "no holder") || !strings.Contains(part, `"s-pub"`) {
			t.Errorf("alice, an open viewer, messaging %s: %d %s, want it delivered to s-pub", target, sent.Code, strings.TrimSpace(sent.Body.String()))
		}
	}
	if assigned := walk.request(t, callerAlice, http.MethodPatch, "/api/v1/issues/"+walk.issue[seedPublic],
		json.RawMessage(`{"assignee":"`+hiddenPerson+`"}`)); assigned.Code != http.StatusOK {
		t.Errorf("alice, an open viewer, assigning %s: %d %s, want 200", hiddenPerson, assigned.Code, strings.TrimSpace(assigned.Body.String()))
	}
	t.Logf("walked %d body fields that name a person or a session", len(personAndSessionReferences))
}

// TestWithNoPrivateProjectASessionWriteAnswersAsToday (R9): with PRIV made public no project is
// private, so alice, an open viewer, sees every session, and each write of hers below that names a
// session answers as it did before a project could be private and reads nothing of whether she sees
// that session: she unfollows privateSession from PUB's ask and follows it again, removes it from
// PUB's issue and document, which the walk's listener holds it subscribed to, and broadcasts to it,
// reaching it. None of them issues either statement that reads whether the caller sees sessions a
// request names (sessionVisibilityRead); the reads of the ask and the document keep their own rule,
// as before. What makes that check able to fail: while PRIV is private, her write to a session's
// own row (withinSessionScope) issues the first, and her read of PUB's subscribers the second.
func TestWithNoPrivateProjectASessionWriteAnswersAsToday(t *testing.T) {
	walk := seedHiddenWrites(t)
	reads := func(statements []string) bool {
		return slices.ContainsFunc(statements, sessionVisibilityRead.MatchString)
	}
	for _, control := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/api/v1/me/agents/s1/state", json.RawMessage(`{"pinned":true}`)},
		{http.MethodGet, "/api/v1/issues/" + walk.fills[probeIssue].public + "/subscribers", nil},
	} {
		if _, statements, _ := walk.recorded(t, callerAlice, control.method, control.path, control.body); !reads(statements) {
			t.Fatalf("while PRIV is private, alice's %s %s read no session's visibility: %v", control.method, control.path, statements)
		}
	}
	walk.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, call := range []struct {
		method, path string
		body         any
		status       int
	}{
		{http.MethodDelete, "/api/v1/asks/" + walk.fills[probeAsk].public + "/followers/" + privateSession, nil, http.StatusNoContent},
		{http.MethodPut, "/api/v1/asks/" + walk.fills[probeAsk].public + "/followers/" + privateSession, nil, http.StatusNoContent},
		{http.MethodDelete, "/api/v1/issues/" + walk.fills[probeIssue].public + "/subscribers/" + privateSession, nil, http.StatusNoContent},
		{http.MethodDelete, "/api/v1/artifacts/" + walk.fills[probeDocument].public + "/subscribers/" + privateSession, nil, http.StatusNoContent},
		{http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "To a walked session.", "delivery": "btw", "session_ids": []string{privateSession},
		}, http.StatusCreated},
	} {
		answer, statements, _ := walk.recorded(t, callerAlice, call.method, call.path, call.body)
		if answer.Code != call.status {
			t.Errorf("PRIV public: alice %s %s = %d %s, want %d", call.method, call.path, answer.Code, strings.TrimSpace(answer.Body.String()), call.status)
		}
		if reads(statements) {
			t.Errorf("PRIV public: alice %s %s read a session's visibility, which every session passes for her: %v", call.method, call.path, statements)
		}
		if call.status == http.StatusCreated {
			if sent := decodeBody[broadcastResponse](t, answer); !slices.Equal(sendTargets(sent), []string{privateSession}) {
				t.Errorf("PRIV public: alice's broadcast to %s reaches %v and leaves out %+v, want it reached", privateSession, sendTargets(sent), sent.Excluded)
			}
		}
		awaitBackgroundWrites(t, walk.server)
	}
}

// TestAMessageToASessionNobodyHoldsIsQueuedOnlyWithNoPrivateProject: Dispatch queues a message to a
// session no store of the listener's holds (queueForUnknownSession) only for a writer that may see
// that session (mayQueueForUnknownSession). While PRIV is private, alice, an open viewer, may not
// see s-nobody - a session with neither a project nor a listener record passes no viewer but its
// own then (access.SessionClause) - so her message to it fails "no live session", as one to a
// session hidden from her does, and nothing is queued on its subject. With no private project (R9)
// she sees every session, and the same message is queued on s-nobody's own subject, as before.
func TestAMessageToASessionNobodyHoldsIsQueuedOnlyWithNoPrivateProject(t *testing.T) {
	walk := seedHiddenWrites(t)
	const nobody = "s-nobody"
	send := func() string {
		t.Helper()
		sent := walk.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/messages",
			json.RawMessage(`{"body":"To a session nobody holds.","target":"session:`+nobody+`","delivery":"btw"}`))
		if sent.Code != http.StatusCreated {
			t.Fatalf("alice's message to %s: %d %s", nobody, sent.Code, strings.TrimSpace(sent.Body.String()))
		}
		return deliveryPart(t, sent.Body.Bytes())
	}
	queuedOn := func() int {
		count := 0
		for _, envelope := range publishedBy(t, walk.deps) {
			if envelope.Topic == contracts.LegacyAgentSubject(nobody) {
				count++
			}
		}
		return count
	}
	if part := send(); !strings.Contains(part, `"state":"failed"`) || !strings.Contains(part, "no live session "+nobody) || queuedOn() != 0 {
		t.Errorf("PRIV private: alice's message to %s answers %s with %d envelopes queued on its subject, want it failed and none queued",
			nobody, part, queuedOn())
	}
	walk.setVisibility(t, seedPrivate, model.ProjectPublic)
	if part := send(); !strings.Contains(part, `"state":"queued"`) || queuedOn() != 1 {
		t.Errorf("PRIV public: alice's message to %s answers %s with %d envelopes queued on its subject, want it queued once",
			nobody, part, queuedOn())
	}
}

// TestASessionGroupWriteAnswersASessionItsWriterMayNotSeeAsOneNobodyHolds: a session group's write
// that names a session its writer may not see answers as the same write naming a session nobody
// holds - the same status and bytes, the same work, and no table written (callPair) - and so names
// no group that holds it. carol, who sees PRIV, groups privateSession; alice, an open viewer who does
// not see PRIV, may not see it. Her group of it, her edit adding it to a group of her own, and her
// edit removing it from carol's group each answer as the same write naming s-nobody does: a refusal
// of a session another group holds would name carol's group, and a removal would empty it.
func TestASessionGroupWriteAnswersASessionItsWriterMayNotSeeAsOneNobodyHolds(t *testing.T) {
	walk := seedHiddenWrites(t)
	carols := createdID(t, walk.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/delivery/session-groups",
		map[string]any{"name": "PRIV workers", "sessions": []string{privateSession}}, http.StatusCreated))
	alices := createdID(t, walk.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/delivery/session-groups",
		map[string]any{"name": "alice's sessions", "sessions": []string{"s-pub"}}, http.StatusCreated))
	for _, call := range []struct{ name, method, path, body string }{
		{"a group of the session", http.MethodPost, "/api/v1/delivery/session-groups", `{"name":"walked","sessions":["%s"]}`},
		{"an edit adding it", http.MethodPatch, "/api/v1/delivery/session-groups/" + alices, `{"add":["%s"]}`},
		{"an edit removing it", http.MethodPatch, "/api/v1/delivery/session-groups/" + carols, `{"remove":["%s"]}`},
	} {
		missing := walk.callPair(t, callerAlice, call.name, call.method, call.path, call.path,
			json.RawMessage(fmt.Sprintf(call.body, privateSession)), json.RawMessage(fmt.Sprintf(call.body, "s-nobody")),
			map[string]string{"s-nobody": privateSession})
		if missing.Code != http.StatusNotFound {
			t.Errorf("%s: alice's write naming s-nobody answered %d %s, want 404: a session nobody holds is one she does not see while PRIV is private",
				call.name, missing.Code, strings.TrimSpace(missing.Body.String()))
		}
	}
}

// TestWithNoPrivateProjectASessionGroupWriteAnswersAsToday (R9): with PRIV made public alice sees
// every session, so a group write of hers answers as before and reads nothing of whether she sees a
// session it names (sessionVisibilityRead): her group of privateSession, which carol's group holds,
// is refused naming carol's group, and her group of a session no group holds is made.
func TestWithNoPrivateProjectASessionGroupWriteAnswersAsToday(t *testing.T) {
	walk := seedHiddenWrites(t)
	walk.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/delivery/session-groups",
		map[string]any{"name": "PRIV workers", "sessions": []string{privateSession}}, http.StatusCreated)
	walk.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, call := range []struct {
		session string
		status  int
		code    string
	}{
		{privateSession, http.StatusConflict, "SESSION_IN_GROUP"},
		{"s-nobody", http.StatusCreated, ""},
	} {
		answer, statements, _ := walk.recorded(t, callerAlice, http.MethodPost, "/api/v1/delivery/session-groups",
			map[string]any{"name": "alice's " + call.session, "sessions": []string{call.session}})
		var refusal refusalAnswer
		_ = json.Unmarshal(answer.Body.Bytes(), &refusal)
		if answer.Code != call.status || refusal.Code != call.code {
			t.Errorf("PRIV public: alice's group of %s answered %d %s, want %d %s", call.session, answer.Code,
				strings.TrimSpace(answer.Body.String()), call.status, call.code)
		}
		if call.code != "" && !strings.Contains(refusal.Error, `"PRIV workers"`) {
			t.Errorf("PRIV public: alice's group of %s is refused %q, want carol's group named, as before", call.session, refusal.Error)
		}
		if slices.ContainsFunc(statements, sessionVisibilityRead.MatchString) {
			t.Errorf("PRIV public: alice's group of %s read a session's visibility, which every session passes for her: %v", call.session, statements)
		}
	}
}

// TestAGroupNameIsHeldOnlyToTheSessionsAndTitlesItsWriterSees: the title rule holds a group's name
// only to the sessions its writer sees, each by the titles it reads, so no refusal names a session,
// or answers a title, the writer may not see. privateSession, which has acted in PRIV and so is
// hidden from alice, took the title "priv worker" on an event of PUB's issue; s-pub, which alice
// sees, took "pub codename" on an event of PRIV's issue, which she does not read. A pull request
// names each. carol, who sees both, is refused a rename of the seed's group of s1 to either title,
// naming the session that holds it: the titles stand. alice's rename of that group to each answers
// as her rename to a title no session holds: 200, the same bytes but for the name and the times,
// and the same statements.
func TestAGroupNameIsHeldOnlyToTheSessionsAndTitlesItsWriterSees(t *testing.T) {
	walk := seedHiddenWrites(t)
	stampSessionTitle(t, walk, walk.issue[seedPublic], privateSession, "priv worker")
	stampSessionTitle(t, walk, walk.issue[seedPrivate], "s-pub", "pub codename")
	path := "/api/v1/delivery/session-groups/" + walk.fills[probeSessionGroup].hidden
	for index, title := range []struct{ name, session string }{{"priv worker", privateSession}, {"pub codename", "s-pub"}} {
		refused := walk.request(t, callerCarol, http.MethodPatch, path, map[string]any{"name": title.name})
		var refusal refusalAnswer
		_ = json.Unmarshal(refused.Body.Bytes(), &refusal)
		if refused.Code != http.StatusConflict || refusal.Code != "SESSION_GROUP_NAME_IS_SESSION_TITLE" || !strings.Contains(refusal.Error, title.session) {
			t.Fatalf("carol renames the group %q: %d %s, want 409 SESSION_GROUP_NAME_IS_SESSION_TITLE naming %s: the row holds no title",
				title.name, refused.Code, strings.TrimSpace(refused.Body.String()), title.session)
		}
		untitled := fmt.Sprintf("a title no session holds %d", index)
		missing, missingStatements, _ := walk.recorded(t, callerAlice, http.MethodPatch, path, map[string]any{"name": untitled})
		hidden, hiddenStatements, _ := walk.recorded(t, callerAlice, http.MethodPatch, path, map[string]any{"name": title.name})
		want := maskTimes(strings.ReplaceAll(missing.Body.String(), untitled, title.name))
		if hidden.Code != missing.Code || maskTimes(hidden.Body.String()) != want {
			t.Errorf("alice renames the group %q, %s's title: %d %s, and to %q: %d %s", title.name, title.session,
				hidden.Code, strings.TrimSpace(hidden.Body.String()), untitled, missing.Code, strings.TrimSpace(missing.Body.String()))
		}
		if missing.Code != http.StatusOK {
			t.Errorf("alice renames the group %q: %d %s, want 200", untitled, missing.Code, strings.TrimSpace(missing.Body.String()))
		}
		if !slices.Equal(hiddenStatements, missingStatements) {
			t.Errorf("alice's rename to %q did other work than her rename to %q, %s", title.name, untitled,
				firstDifference(hiddenStatements, missingStatements))
		}
	}
}

// TestWithNoPrivateProjectAGroupNameIsHeldToEveryTitleAsToday (R9): with PRIV made public alice
// sees every session and reads every event, so the title rule holds a name of hers to every title
// as before and reads nothing of whether she sees a session (sessionVisibilityRead): her rename of
// the seed's group to each title of TestAGroupNameIsHeldOnlyToTheSessionsAndTitlesItsWriterSees is
// refused naming the session that holds it.
func TestWithNoPrivateProjectAGroupNameIsHeldToEveryTitleAsToday(t *testing.T) {
	walk := seedHiddenWrites(t)
	stampSessionTitle(t, walk, walk.issue[seedPublic], privateSession, "priv worker")
	stampSessionTitle(t, walk, walk.issue[seedPrivate], "s-pub", "pub codename")
	walk.setVisibility(t, seedPrivate, model.ProjectPublic)
	path := "/api/v1/delivery/session-groups/" + walk.fills[probeSessionGroup].hidden
	for _, title := range []struct{ name, session string }{{"priv worker", privateSession}, {"pub codename", "s-pub"}} {
		answer, statements, _ := walk.recorded(t, callerAlice, http.MethodPatch, path, map[string]any{"name": title.name})
		var refusal refusalAnswer
		_ = json.Unmarshal(answer.Body.Bytes(), &refusal)
		if answer.Code != http.StatusConflict || refusal.Code != "SESSION_GROUP_NAME_IS_SESSION_TITLE" || !strings.Contains(refusal.Error, title.session) {
			t.Errorf("PRIV public: alice renames the group %q: %d %s, want 409 SESSION_GROUP_NAME_IS_SESSION_TITLE naming %s, as before",
				title.name, answer.Code, strings.TrimSpace(answer.Body.String()), title.session)
		}
		if slices.ContainsFunc(statements, sessionVisibilityRead.MatchString) {
			t.Errorf("PRIV public: alice's rename to %q read a session's visibility, which every session passes for her: %v", title.name, statements)
		}
	}
}

// TestAnIDThatIsNoUUIDAnswersAsAnIDNobodyHolds walks every route whose {id} names an item by uuid -
// a document, an ask, a comment or a message, which the prefix layer resolves (idRoutes), and every
// other route idRoutes says takes a uuid - and calls it, as alice, with an id that is neither a uuid
// nor a prefix any item carries, and with a uuid nobody holds. The first is answered either as the
// second - the same status and the same bytes, the second's id written as the first's - or by the
// route's own refusal of an id that is no id (400 <KIND>_ID_INPUT, which reads nothing), and never
// by a 500: a value Postgres will not read as a uuid is never the database's error.
func TestAnIDThatIsNoUUIDAnswersAsAnIDNobodyHolds(t *testing.T) {
	walk := seedHiddenWrites(t)
	const notAUUID = "not-a-uuid"
	var walked int
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name := route.Method + " " + route.Pattern
		entry, named := idRoutes[name]
		if !named || !strings.Contains(route.Pattern, "{id}") || !(entry.Kind != "" || takesAUUID(entry.Reason)) {
			continue
		}
		var body any
		if probe, ok := writeProbes[name]; ok && probe.Body != "" {
			body = json.RawMessage(probe.Body)
		}
		nobodys := uuid.NewString()
		fill := func(id string) string {
			return patternWildcard.ReplaceAllStringFunc(route.Pattern, func(match string) string {
				wildcard := patternWildcard.FindStringSubmatch(match)[1]
				if wildcard == "id" {
					return id
				}
				object := writeProbes[name].Probe[wildcard]
				if object == "" {
					object = readClasses[name].Probe[wildcard]
				}
				return walk.fills[object].besideValue()
			})
		}
		malformed := walk.request(t, callerAlice, route.Method, fill(notAUUID), body)
		missing := walk.request(t, callerAlice, route.Method, fill(nobodys), body)
		walked++
		if malformed.Code >= http.StatusInternalServerError {
			t.Errorf("%s: an id that is no uuid answers %d %s: a value Postgres will not read as a uuid is never the database's error",
				name, malformed.Code, strings.TrimSpace(malformed.Body.String()))
			continue
		}
		var refusal refusalAnswer
		_ = json.Unmarshal(malformed.Body.Bytes(), &refusal)
		asMissing := malformed.Code == missing.Code &&
			malformed.Body.String() == strings.ReplaceAll(missing.Body.String(), nobodys, notAUUID)
		refusedAsInput := malformed.Code == http.StatusBadRequest && strings.HasSuffix(refusal.Code, "_ID_INPUT")
		if !asMissing && !refusedAsInput {
			t.Errorf("%s: an id that is no uuid answers %d %s, and a uuid nobody holds %d %s", name,
				malformed.Code, strings.TrimSpace(malformed.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
		}
	}
	if walked == 0 {
		t.Fatal("no route was walked: idRoutes names no route whose {id} is a uuid")
	}
	t.Logf("walked %d routes whose {id} is a uuid", walked)
}

// TestTheWalkExcusesAHiddenKeysOwnProjectAlone holds answerAsHidden, the reading callPair compares
// a hidden key's answer to, to excusing no more than the key's own project. The refusal of a role
// holder write is built from the key alone - its project, the key, the daemon's session - so PRIV-1's
// reads as NOPE-1's, and only once the project is read as the key's (rewritten alone leaves NOPE).
// A refusal that named PRIV-1's title as well, which the caller never sent, still differs from
// NOPE-1's: the walk fails it.
func TestTheWalkExcusesAHiddenKeysOwnProjectAlone(t *testing.T) {
	written := map[string]string{"NOPE-1": "PRIV-1"}
	refusal := func(key, more string) string {
		project, _, _ := strings.Cut(key, "-")
		return `{"code":"ROLE_HOLDER_FORBIDDEN","error":"only the Legion daemon of project ` + project +
			` names the sessions that hold the roles of ` + key + more + `. That daemon writes as session legion-daemon:` + project + `."}`
	}
	hidden, missing := refusal("PRIV-1", ""), refusal("NOPE-1", "")
	if got := answerAsHidden(missing, written); got != hidden {
		t.Errorf("NOPE-1's refusal reads as %s, and PRIV-1's is %s: a refusal built from the key alone must read alike", got, hidden)
	}
	if got := rewritten([]string{missing}, written)[0]; got == hidden {
		t.Errorf("NOPE-1's refusal with its key alone written reads as PRIV-1's, %s: the project's reading is tested by nothing", got)
	}
	if titled := refusal("PRIV-1", " (The issue of PRIV)"); answerAsHidden(missing, written) == titled {
		t.Errorf("a refusal naming PRIV-1's title, %s, reads as NOPE-1's: the walk would excuse it", titled)
	}
}
