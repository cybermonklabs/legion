package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestNoWriteReachesAHiddenObject walks every write row whose pattern names an object ({key}, {id},
// {session_id}, or a mapping's {repo}), whatever its write class, on verification v16's seed
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
// Five callers walk it. alice, at a browser, is an open viewer who sees PUB and no object of PRIV,
// and every session. alice's token that lists PRIV sees no project at all, since alice does not see
// PRIV, and is not open: the deployment's rows are refused it whatever they name, and a session is
// hidden from it unless it is its own or has acted in a project it sees (access.SessionClause).
// alice's token that lists PUB sees PUB alone and is not open, so it is held as her other token is.
// bob, a restricted member, and carol's token that lists PRIV each see PRIV alone, so a session that
// has acted in PUB alone is hidden from them too: they walk the rows whose one hidden object is a
// session. Every row is walked by at least one of them.
func TestNoWriteReachesAHiddenObject(t *testing.T) {
	walk := seedHiddenWrites(t)
	rows := objectWriteRows()
	if problems := writeProbeProblems(rows); len(problems) > 0 {
		t.Fatalf("writeProbes does not describe the write rows that name an object:\n%s", strings.Join(problems, "\n"))
	}
	walked := map[string]bool{}
	for _, caller := range []struct {
		who          privateCaller
		open         bool
		sessionsOnly bool
	}{
		{callerAlice, true, false},
		{callerAlicePrivToken, false, false},
		{callerAlicePubToken, false, false},
		{callerBob, false, true},
		{callerCarolPrivToken, false, true},
	} {
		for _, name := range rows {
			probe := writeProbes[name]
			if !probe.hides(caller.open) || caller.sessionsOnly && !probe.hidesOnlySessions(caller.open) {
				continue
			}
			walked[name] = true
			walk.compare(t, caller.who, caller.open, name)
		}
	}
	for _, name := range rows {
		if !walked[name] {
			t.Errorf("%s: no caller of the walk has an object of it hidden, so nothing called it", name)
		}
	}
	t.Logf("walked %d of the %d write rows that name an object", len(walked), len(rows))

	// What makes the subscriber rows able to fail: the walk's listener holds s1 subscribed to
	// PRIV's issue and document, so carol, who sees PRIV, removes it. A listener that held no such
	// interest would answer every removal "not subscribed", PRIV's and a missing one's alike.
	for _, path := range []string{
		"/api/v1/issues/" + walk.fills[probeIssue].hidden + "/subscribers/s1",
		"/api/v1/artifacts/" + walk.fills[probeDocument].hidden + "/subscribers/s1",
	} {
		if removed := walk.request(t, callerCarol, http.MethodDelete, path, nil); removed.Code != http.StatusNoContent {
			t.Errorf("carol: DELETE %s = %d %s, want 204: the walk's listener does not hold s1 subscribed to PRIV, so its subscriber rows prove nothing",
				path, removed.Code, strings.TrimSpace(removed.Body.String()))
		}
	}

	// The {owner}/{repo} rows, as alice, with PRIV mapping acme/p_x: a mapping of a project she
	// cannot see is neither removed, moved nor named, and a target project she cannot see is
	// answered as a key no project holds. Each pair answers alike and writes nothing.
	const mappings = "/api/v1/settings/repo-projects/"
	for _, pair := range []struct {
		name, method, hiddenPath, missingPath string
		hiddenBody, missingBody               any
	}{
		{"DELETE a mapping of PRIV's", http.MethodDelete, mappings + "acme/p_x", mappings + "acme/nobody", nil, nil},
		{"PUT over a mapping of PRIV's", http.MethodPut, mappings + "acme/p_x", mappings + "acme/p.x",
			map[string]string{"project": seedPublic}, map[string]string{"project": seedPublic}},
		{"PUT into PRIV", http.MethodPut, mappings + "acme/new", mappings + "acme/new",
			map[string]string{"project": seedPrivate}, map[string]string{"project": "NOPE"}},
	} {
		walk.callPair(t, callerAlice, pair.name, pair.method, pair.hiddenPath, pair.missingPath, pair.hiddenBody, pair.missingBody, nil, false)
	}
}

// bodyReferences is every field of a write's body that names an object of a project, with a call
// alice may make that names it: the row, the path of an object she sees (PUB's issue, project or
// conversation), and the body with %s where the named object goes. accepted is a row that takes an
// object nobody holds rather than refusing it: the design gates store a document's id they do not
// read, so both calls write, and must write the same tables. An external reference names a
// repository PRIV maps, acme/p_x, beside acme/p.x on its lane, which no mapping holds: a mapping
// of a project the caller cannot see answers as no mapping.
var bodyReferences = []struct {
	row, field, path string
	object           probeObject
	body             string
	accepted         bool
}{
	{"POST /api/v1/issues", "parent", "/api/v1/issues", probeIssue, `{"project":"PUB","title":"A walked child","parent":"%s"}`, false},
	{"POST /api/v1/issues", "blocked_by", "/api/v1/issues", probeIssue, `{"project":"PUB","title":"A walked wait","blocked_by":["%s"]}`, false},
	{"PATCH /api/v1/issues/{key}", "parent", "/api/v1/issues/PUB-1", probeIssue, `{"parent":"%s"}`, false},
	{"PATCH /api/v1/issues/{key}", "blocked_by", "/api/v1/issues/PUB-1", probeIssue, `{"blocked_by":["%s"]}`, false},
	{"PATCH /api/v1/issues/{key}", "rank.after", "/api/v1/issues/PUB-1", probeIssue, `{"rank":{"after":"%s"}}`, false},
	{"POST /api/v1/issues/{key}/comments", "reply_to", "/api/v1/issues/PUB-1/comments", probeComment, `{"body":"A walked reply.","reply_to":"%s"}`, false},
	{"POST /api/v1/issues/{key}/comments", "ask_id", "/api/v1/issues/PUB-1/comments", probeAsk, `{"body":"A walked answer.","ask_id":"%s"}`, false},
	{"POST /api/v1/issues/{key}/messages", "in_reply_to", "/api/v1/issues/PUB-1/messages", probeMessage, `{"body":"A walked reply.","in_reply_to":"%s"}`, false},
	{"POST /api/v1/agents/{session_id}/messages", "in_reply_to", "/api/v1/agents/s1/messages", probeMessage, `{"body":"A walked reply.","delivery":"btw","in_reply_to":"%s"}`, false},
	{"PUT /api/v1/issues/{key}/design-gates", "gates[].artifact_id", "/api/v1/issues/PUB-1/design-gates", probeDocument, `{"gates":[{"step":"spec","artifact_id":"%s"}]}`, true},
	{"POST /api/v1/projects/{key}/related", "exclude_issue", "/api/v1/projects/PUB/related", probeIssue, `{"text":"walked","exclude_issue":"%s"}`, false},
	{"POST /api/v1/issues", "external", "/api/v1/issues", probeMappedRepository, `{"project":"PUB","title":"A walked import","external":"acme/%s#7"}`, false},
	{"POST /api/v1/issues", "external (no project)", "/api/v1/issues", probeMappedRepository, `{"title":"A walked import","external":"acme/%s#7"}`, false},
}

// TestNoBodyReachesAHiddenObject: every field of a write's body that names an object
// (bodyReferences) answers PRIV's object exactly as an object nobody holds - the same status and
// bytes, the same work, and no table written - while the path names an object the caller sees, to
// alice and to her token that lists PUB, which sees PUB alone and names its session (probeActor). An
// object nobody holds is refused there, except where the row takes one (accepted).
func TestNoBodyReachesAHiddenObject(t *testing.T) {
	walk := seedHiddenWrites(t)
	if walk.issue[seedPublic] != "PUB-1" {
		t.Fatalf("PUB's issue is %s; the walk's paths name PUB-1", walk.issue[seedPublic])
	}
	for _, caller := range []privateCaller{callerAlice, callerAlicePubToken} {
		for _, reference := range bodyReferences {
			name := reference.row + " (" + reference.field + ")"
			fill := walk.fills[reference.object]
			method, _, _ := strings.Cut(reference.row, " ")
			body := reference.body
			if caller.login() == "" {
				body = withProbeActor(body)
			}
			missing := walk.callPair(t, caller, name, method, reference.path, reference.path,
				json.RawMessage(fmt.Sprintf(body, fill.hidden)), json.RawMessage(fmt.Sprintf(body, fill.missing)),
				map[string]string{fill.missing: fill.hidden}, reference.accepted)
			if refused := missing.Code >= http.StatusBadRequest; refused == reference.accepted {
				t.Errorf("%s as %s: an object nobody holds answered %d %s; want a refusal: %t", name, caller, missing.Code,
					strings.TrimSpace(missing.Body.String()), !reference.accepted)
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
// naming nothing that cites it. A repository is renamed one step at most, so alice's rename
// through PRIV's rename is refused as a mapping on a lane a project she cannot see holds
// (hiddenLaneError), naming neither of PRIV's repositories; the mappings are the deployment's,
// which her token that lists PUB may not write at all.
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
		{"a rename of what PRIV's repository was renamed to", http.MethodPut, "/api/v1/settings/repo-projects/acme/p_y",
			map[string]any{"project": seedPublic, "renamed_to": "acme/w_z"}, []privateCaller{callerAlice}, []string{"p_x"},
			http.StatusConflict, codeRepositoryLaneShared, hiddenLaneError(repoLanes("acme/p_y", nil)[0]).Error()},
		{"a rename to PRIV's renamed repository", http.MethodPut, "/api/v1/settings/repo-projects/acme/w_y",
			map[string]any{"project": seedPublic, "renamed_to": "acme/p_x"}, []privateCaller{callerAlice}, []string{"p_y"},
			http.StatusConflict, codeRepositoryLaneShared, hiddenLaneError(repoLanes("acme/p_x", nil)[0]).Error()},
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

// timeStamp matches an RFC 3339 time, which two runs asked a moment apart each stamp their own.
var timeStamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// maskTimes is body with every RFC 3339 time written as one placeholder.
func maskTimes(body string) string { return timeStamp.ReplaceAllString(body, "<time>") }

// hiddenPerson is a full member no list of PRIV names, so a caller that sees PRIV alone does not
// see them (access.PersonClause), and an open viewer does.
const hiddenPerson = "dana@example.com"

// walkedRole is a role the walk's listener says s-pub holds; every other role it answers unclaimed.
const walkedRole = "walked-role"

// probeRole fills a role a delivery's target names: walkedRole, or one no session claims.
const probeRole probeObject = "role"

// personAndSessionReferences is every field of a write's body that names a person or a session,
// with a call a caller that sees PRIV alone may make that names it, on PRIV's objects: the row,
// the path, the body with %s where the person or session goes. owner is a call only PRIV's owner
// makes, walked as carol's token alone. writes is a call that writes either way - a message or a
// mention, each recorded and its delivery failed - and is compared by its deliveries
// (deliveryPart); every other call is refused either way, writing nothing, and compared whole. A
// delivery's `target` (POST /api/v1/comments/{id}/deliveries) names no session of its own: it picks
// one of the comment's mentions, and its send resolves through the same resolveDeliveryTarget.
var personAndSessionReferences = []struct {
	row, field string
	path       func(w hiddenWriteSeed) string
	object     probeObject
	body       string
	owner      bool
	writes     bool
}{
	{"POST /api/v1/issues", "assignee", func(hiddenWriteSeed) string { return "/api/v1/issues" }, probePerson,
		`{` + probeActor + `,"project":"PRIV","title":"A walked issue","assignee":"%s"}`, false, false},
	{"PATCH /api/v1/issues/{key}", "assignee", func(w hiddenWriteSeed) string { return "/api/v1/issues/" + w.issue[seedPrivate] }, probePerson,
		`{` + probeActor + `,"assignee":"%s"}`, false, false},
	{"PUT /api/v1/asks/{id}/asked-of", "asked_of", func(w hiddenWriteSeed) string { return "/api/v1/asks/" + w.fills[probeAsk].hidden + "/asked-of" }, probePerson,
		`{"asked_of":"%s"}`, false, false},
	{"PUT /api/v1/projects/{key}/default-assignee", "assignee", func(hiddenWriteSeed) string { return "/api/v1/projects/PRIV/default-assignee" }, probePerson,
		`{"assignee":"%s"}`, false, false},
	{"PATCH /api/v1/projects/{key}/access", "owner", func(hiddenWriteSeed) string { return "/api/v1/projects/PRIV/access" }, probePerson,
		`{"owner":"%s"}`, true, false},
	{"POST /api/v1/priorities", "owner", func(hiddenWriteSeed) string { return "/api/v1/priorities" }, probePerson,
		`{` + probeActor + `,"title":"A walked priority","owner":"%s"}`, false, false},
	{"PUT /api/v1/access-groups/{name}", "members", func(hiddenWriteSeed) string { return "/api/v1/access-groups/walked-group" }, probePerson,
		`{"members":["%s"]}`, false, false},
	{"POST /api/v1/issues/{key}/messages", "target", func(w hiddenWriteSeed) string { return "/api/v1/issues/" + w.issue[seedPrivate] + "/messages" }, probeSession,
		`{` + probeActor + `,"body":"A walked message.","target":"session:%s","delivery":"btw"}`, false, true},
	{"POST /api/v1/issues/{key}/comments", "mentions[].target", func(w hiddenWriteSeed) string { return "/api/v1/issues/" + w.issue[seedPrivate] + "/comments" }, probeSession,
		`{` + probeActor + `,"body":"A walked mention.","mentions":[{"target":"session:%s"}]}`, false, true},
	{"POST /api/v1/issues/{key}/messages", "target (a role)", func(w hiddenWriteSeed) string { return "/api/v1/issues/" + w.issue[seedPrivate] + "/messages" }, probeRole,
		`{` + probeActor + `,"body":"A walked message.","target":"role:%s","delivery":"btw"}`, false, true},
	{"POST /api/v1/issues/{key}/comments", "mentions[].target (a role)", func(w hiddenWriteSeed) string { return "/api/v1/issues/" + w.issue[seedPrivate] + "/comments" }, probeRole,
		`{` + probeActor + `,"body":"A walked mention.","mentions":[{"target":"role:%s"}]}`, false, true},
	{"POST /api/v1/broadcasts", "session_ids", func(hiddenWriteSeed) string { return "/api/v1/broadcasts" }, probeSession,
		`{"body":"To walked sessions.","delivery":"btw","session_ids":["%s"]}`, false, false},
	{"POST /api/v1/delivery/session-groups", "sessions", func(hiddenWriteSeed) string { return "/api/v1/delivery/session-groups" }, probeSession,
		`{` + probeActor + `,"name":"walked","sessions":["%s"]}`, false, false},
}

// TestNoBodyNamesAHiddenPersonOrSession: every field of a write's body that names a person or a
// session (personAndSessionReferences) answers one the caller may not see exactly as one nobody
// holds, to the callers that see PRIV alone and are not open - bob, a restricted member, and
// carol's token that lists PRIV - and a role whose holder it may not see as a role nobody holds.
// An open viewer is answered as before: alice's message to s-pub and to the role it holds are
// delivered, which also shows the walk's listener holds both, and she assigns dana.
func TestNoBodyNamesAHiddenPersonOrSession(t *testing.T) {
	walk := seedHiddenWrites(t)
	for _, reference := range personAndSessionReferences {
		callers := []privateCaller{callerBob, callerCarolPrivToken}
		if reference.owner {
			callers = []privateCaller{callerCarolPrivToken}
		}
		fill := walk.fills[reference.object]
		method, _, _ := strings.Cut(reference.row, " ")
		for _, caller := range callers {
			name := reference.row + " (" + reference.field + ")"
			// A call refused either way writes nothing: the tables are digested around it, once the
			// work an earlier call left running (a mention's delivery) has settled (tableDigests).
			var before map[string]string
			if !reference.writes {
				before = walk.tableDigests(t)
			}
			hidden := walk.request(t, caller, method, reference.path(walk), json.RawMessage(fmt.Sprintf(reference.body, fill.hidden)))
			missing := walk.request(t, caller, method, reference.path(walk), json.RawMessage(fmt.Sprintf(reference.body, fill.missing)))
			if !reference.writes {
				if changed := changedTables(before, walk.tableDigests(t)); len(changed) > 0 {
					t.Errorf("%s as %s: a call refused either way changed %v", name, caller, changed)
				}
			}
			got, want := hidden.Body.String(), strings.ReplaceAll(missing.Body.String(), fill.missing, fill.hidden)
			if reference.writes {
				got, want = deliveryPart(t, hidden.Body.Bytes()), strings.ReplaceAll(deliveryPart(t, missing.Body.Bytes()), fill.missing, fill.hidden)
			}
			if hidden.Code != missing.Code || got != want {
				t.Errorf("%s as %s: %s answers %d %s, and one nobody holds %d %s", name, caller, fill.hidden,
					hidden.Code, strings.TrimSpace(hidden.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
			}
			if succeeded := missing.Code < http.StatusBadRequest; succeeded != reference.writes {
				t.Errorf("%s as %s: one nobody holds answered %d %s; want a write: %t", name, caller,
					missing.Code, strings.TrimSpace(missing.Body.String()), reference.writes)
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

// deliveryPart is what a write that records a delivery answers about it: each delivery's state,
// session and failure, each mention's session, or the one attempt's, as JSON.
func deliveryPart(t *testing.T, body []byte) string {
	t.Helper()
	type attempt struct {
		State     string  `json:"state"`
		SessionID *string `json:"session_id"`
		Error     *string `json:"error"`
	}
	var answer struct {
		attempt
		Deliveries []attempt `json:"deliveries"`
		Mentions   []struct {
			SessionID *string `json:"session_id"`
		} `json:"mentions"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode a delivery's answer %s: %v", body, err)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode a delivery's answer: %v", err)
	}
	return string(encoded)
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

// takesAUUID reports whether an idRoutes reason says the route's id is a uuid of a kind the prefix
// layer does not resolve.
func takesAUUID(reason string) bool {
	for _, kind := range []string{"a handoff id", "a check result id", "a personal agent token id", "a thread id",
		"a credential grant id", "a credential request id", "a machine login id", "a Delivery session group id",
		"a broadcast id", "a going request's run id"} {
		if strings.HasPrefix(reason, kind) {
			return true
		}
	}
	return false
}

// missingAnswers is what a row answers for an object nobody holds where that is no 404, and why.
var missingAnswers = map[string]struct {
	status int
	code   string
	why    string
}{
	"DELETE /api/v1/me/asks/{id}/snooze": {http.StatusNoContent, "",
		"un-snoozing an ask the caller never snoozed leaves the state asked for, whatever the id names (idRoutes: notFoundNone)"},
	"PUT /api/v1/me/threads/{id}/read": {http.StatusNoContent, "",
		"the caller's own read mark, kept under any thread id and reading no message (idRoutes)"},
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}": {http.StatusConflict, codeRepositoryLaneShared,
		"a PUT over a mapping of a project the caller cannot see answers as a PUT of a neighbour on its GitHub subject lane, the lane refusal's hidden form (refuseHiddenMapping)"},
	"DELETE /api/v1/settings/repo-projects/{owner}/{repo}": {http.StatusNoContent, "",
		"removing a mapping of a project the caller cannot see answers as removing one no repository has, writing nothing (refuseHiddenMapping)"},
	"POST /api/v1/issues/{key}/results": {http.StatusBadRequest, "UNKNOWN_CHECK",
		"a result names a check of its issue's Verification document, and an issue the caller cannot read holds none"},
	"PUT /api/v1/issues/{key}/role-holders": {http.StatusForbidden, "ROLE_HOLDER_FORBIDDEN",
		"only the session of the issue's own project's Legion daemon writes its role holders, and every other caller is refused before any read, naming only the key it sent (putIssueRoleHolders)"},
}

// refusedAtABrowser is the refusal a row makes a person at a browser before it reads anything: the
// row is a session's own, so only a bearer reaches its read.
var refusedAtABrowser = map[string]string{
	"POST /api/v1/comments/{id}/reply":                       "REPLY_FORBIDDEN",
	"POST /api/v1/messages/{id}/reply":                       "REPLY_FORBIDDEN",
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": "ACCEPT_FORBIDDEN",
	"POST /api/v1/issues/{key}/handoffs/{phase}":             "HANDOFF_FORBIDDEN",
}

// objectWriteRows is every write row of writeClasses whose pattern names an object, sorted.
func objectWriteRows() []string {
	var rows []string
	for name := range writeClasses {
		if strings.Contains(name, "{key}") || strings.Contains(name, "{id}") || strings.Contains(name, "{session_id}") ||
			strings.Contains(name, "{repo}") {
			rows = append(rows, name)
		}
	}
	slices.Sort(rows)
	return rows
}

// writeProbeProblems names every row writeProbes leaves out, every entry that is no such row, and
// every Probe that does not fill exactly its pattern's wildcards with known objects or whose body
// is not JSON; and every entry of noProjectWrites that is no write row of class scopeDeployment,
// that writeProbes also holds, that says no reason, or whose body is not JSON.
func writeProbeProblems(rows []string) []string {
	var problems []string
	for _, name := range rows {
		if _, ok := writeProbes[name]; !ok {
			problems = append(problems, name+": no entry in writeProbes")
		}
	}
	for name, probe := range writeProbes {
		if !slices.Contains(rows, name) {
			problems = append(problems, name+": an entry of writeProbes for no write row that names an object")
			continue
		}
		_, pattern, _ := strings.Cut(name, " ")
		wildcards := map[string]bool{}
		for _, match := range patternWildcard.FindAllStringSubmatch(pattern, -1) {
			wildcards[match[1]] = true
			if _, filled := probe.Probe[match[1]]; !filled {
				problems = append(problems, fmt.Sprintf("%s: a Probe that does not fill {%s}", name, match[1]))
			}
		}
		for wildcard, object := range probe.Probe {
			if !wildcards[wildcard] {
				problems = append(problems, fmt.Sprintf("%s: a Probe that fills {%s}, which the pattern does not hold", name, wildcard))
			}
			if !slices.Contains(probeObjects, object) {
				problems = append(problems, fmt.Sprintf("%s: a Probe that fills {%s} with %q, none of probeObjects", name, wildcard, object))
			}
		}
		if probe.Body != "" && !json.Valid([]byte(probe.Body)) {
			problems = append(problems, name+": a body that is not JSON")
		}
	}
	for name, write := range noProjectWrites {
		if writeClasses[name] != scopeDeployment {
			problems = append(problems, fmt.Sprintf("%s: an entry of noProjectWrites for a row of class %q, not %q", name, writeClasses[name], scopeDeployment))
		}
		if _, ok := writeProbes[name]; ok {
			problems = append(problems, name+": in both writeProbes and noProjectWrites")
		}
		if write.Reason == "" {
			problems = append(problems, name+": an entry of noProjectWrites that says no reason")
		}
		if write.Body != "" && !json.Valid([]byte(write.Body)) {
			problems = append(problems, name+": a noProjectWrites body that is not JSON")
		}
	}
	slices.Sort(problems)
	return problems
}

// hiddenFrom reports whether the walk names a hidden object of kind in place of one nobody holds,
// for an open caller or not: an object of a project, a repository's mapping among them, is hidden
// from every caller of the walk, a personal token is someone else's, and a row of the deployment
// and a session that acted in PUB alone are hidden from a caller that is not open. Every other
// kind is part of an object the path already names - a document's slug, a version, a block, an
// attempt, a phase, a person or group on a list, a project's repository, a pause point - and is
// PRIV's own in both calls.
func hiddenFrom(object probeObject, open bool) bool {
	switch object {
	case probeProject, probeIssue, probeDocument, probeAsk, probeComment, probeMessage, probeThread,
		probeResult, probeAgentToken, probeMappedRepository:
		return true
	case probeSession, probePriority, probeSessionGroup, probeCredentialRequest, probeCredentialGrant,
		probeMachineLogin, probeSignin:
		return !open
	}
	return false
}

// hides reports whether the probe names an object hidden from the caller.
func (p writeProbe) hides(open bool) bool {
	for _, object := range p.Probe {
		if hiddenFrom(object, open) {
			return true
		}
	}
	return false
}

// hidesOnlySessions reports whether every object the probe hides from the caller is a session.
func (p writeProbe) hidesOnlySessions(open bool) bool {
	for _, object := range p.Probe {
		if hiddenFrom(object, open) && object != probeSession {
			return false
		}
	}
	return true
}

// probeFill is one kind's values: the hidden object, one of that kind nobody holds, and the one a
// call names beside another kind's hidden object ("" for the hidden one): a session subscribed to
// PRIV's issue is the one a subscriber row names beside that issue, and a session of PUB alone is
// the one hidden from a caller that sees PRIV alone.
type probeFill struct{ hidden, missing, beside string }

// besideValue is the value a call names for a kind that is not the object it hides.
func (f probeFill) besideValue() string {
	if f.beside != "" {
		return f.beside
	}
	return f.hidden
}

// hiddenWriteSeed is the seed the walk calls, with the value of every probe kind, the trace of its
// store and the record of its listener's calls. digest is the one statement that digests every
// table (tableDigests), built once from the tables the seed lists, which no call changes.
type hiddenWriteSeed struct {
	privateSeed
	fills  map[probeObject]probeFill
	digest string
	trace  *existenceTrace
	calls  *listenerCalls
}

// callerAlicePubToken is alice's token that lists PUB: it sees PUB alone and is not open, and a
// token of carol's is someone else's to it.
const callerAlicePubToken privateCaller = "alice's token that lists PUB"

// withProbeActor is a JSON object body with probeActor first among its fields: what a bearer that
// proves no session names as itself.
func withProbeActor(body string) string {
	if body == "{}" {
		return "{" + probeActor + "}"
	}
	return "{" + probeActor + "," + strings.TrimPrefix(body, "{")
}

// privateHeldURL is the external link PRIV's issue holds: issue 9 of acme/w_pub, a repository
// PUB maps.
const privateHeldURL = "https://github.com/acme/w_pub/issues/9"

// mintListingToken mints login's personal token named caller, writes projects as its list, and
// keeps it among the seed's tokens, so seed.request presents it.
func mintListingToken(t *testing.T, seed privateSeed, caller privateCaller, login string, projects []string) {
	t.Helper()
	minted := decodeBody[agentTokenResponse](t, dispatchRequest(t, seed.handler, http.MethodPost, "/api/v1/me/agent-tokens",
		map[string]string{"name": string(caller)}, login))
	if minted.Token == "" {
		t.Fatalf("mint %s: no token in the answer", caller)
	}
	if _, err := seed.database.Pool.Exec(context.Background(), `update agent_tokens set projects = $2 where id = $1::uuid`,
		minted.ID, projects); err != nil {
		t.Fatalf("write the list of %s: %v", caller, err)
	}
	seed.tokens[caller] = minted.Token
}

// recorded makes one request as caller on a context the seed's trace records, and answers it with
// the statements it issued, less volatileStatement's, and the listener calls made while it ran.
func (w hiddenWriteSeed) recorded(t *testing.T, caller privateCaller, method, path string, body any) (*httptest.ResponseRecorder, []string, []string) {
	t.Helper()
	w.trace.take()
	w.calls.take()
	response := w.requestIn(t, context.WithValue(context.Background(), recordingKey{}, true), caller, method, path, body)
	return response, slices.DeleteFunc(w.trace.take(), volatileStatement), w.calls.take()
}

// volatileColumns are the columns a call stamps whatever it names, left out of a table's digest: a
// personal token's use stamps its own row.
var volatileColumns = map[string]string{"public.agent_tokens": "last_used_at"}

// volatileStatement reports whether a statement is the stamp of volatileColumns' column, which a
// token's first use in a minute writes and its next does not, whatever either call names.
func volatileStatement(statement string) bool {
	return strings.HasPrefix(statement, "update agent_tokens set last_used_at = now()")
}

// otherWork names each call whose hidden object is answered as an object nobody holds by a path
// that must read the hidden row to answer at all, with why: callPair holds it to the same answer
// and no write, and not to the same statements.
var otherWork = map[string]string{
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}": "a mapping is keyed by its repository, so a PUT over a mapping of a " +
		"project the caller cannot see must find that row to refuse it rather than take the repository as unmapped and " +
		"overwrite it; its missing side is a neighbour on its lane, refused by the lane check after the write (missingAnswers)",
	"PUT over a mapping of PRIV's": "the walk's own call of the PUT row above (otherWork's first entry)",
}

// walkListener is a fake Envoy listener holding session s1 live and subscribed to PRIV's issue and
// project document, so a write that removes a subscriber reaches the removal. It records every
// request it is sent in calls.
func walkListener(t *testing.T, topics *[]string, calls *listenerCalls) *httptest.Server {
	t.Helper()
	const s1 = `{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}`
	// s-pub is live too, so a delivery a caller may not make to it would reach it: the person and
	// session walk holds a delivery to it to the failure one to a session nobody holds records.
	const sPub = `{"session_id":"s-pub","title":"pub worker","dir":"/w/pub","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(s1))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s-pub":
			_, _ = w.Write([]byte(sPub))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/"+walkedRole:
			_, _ = w.Write([]byte(`{"role":"` + walkedRole + `","holder":"s-pub","holder_state":"live",` + sPub[1:]))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role ` + role + `","reason":"unclaimed","claim_revision":0}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[` + s1 + `,` + sPub + `]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interests/s1":
			encoded, _ := json.Marshal(map[string]any{"session_id": "s1", "topics": *topics, "updated_at": 1})
			_, _ = w.Write(encoded)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/interests/unsubscribe":
			var request struct {
				Topics []string `json:"topics"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			encoded, _ := json.Marshal(map[string]any{"removed": request.Topics})
			_, _ = w.Write(encoded)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
}

// seedHiddenWrites stands up verification v16's seed and writes, as carol, PRIV's object of every
// kind a write row names, through the routes. The seed's store is traced and its listener records
// its calls, so a pair of calls can be held to the same work (callPair). PRIV's issue is claimed by
// s1 and links privateHeldURL; PRIV maps acme/p_x, which it renamed to acme/p_y, and PUB maps
// acme/w_pub, whose issue 9 privateHeldURL is.
func seedHiddenWrites(t *testing.T) hiddenWriteSeed {
	t.Helper()
	topics := []string{}
	calls := &listenerCalls{}
	trace := &existenceTrace{}
	listener := walkListener(t, &topics, calls)
	t.Cleanup(listener.Close)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, store: storetest.TracedStore(t, storetest.Open(t), trace)})
	issue := seed.issue[seedPrivate]
	asCarol := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		response := seed.request(t, callerCarol, method, path, body)
		if response.Code < 200 || response.Code > 299 {
			t.Fatalf("carol: %s %s = %d %s", method, path, response.Code, response.Body.String())
		}
		return response
	}

	asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "verification.md", "content": twoChecks})
	issueDocument := decodeBody[uploadedArtifact](t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts",
		map[string]string{"name": "walked-notes.md", "content": "# Notes\n\nA passage of PRIV.\n"}))
	projectDocument := decodeBody[uploadedArtifact](t, asCarol(http.MethodPost, "/api/v1/projects/"+seedPrivate+"/artifacts",
		map[string]string{"name": "walked-notes.md", "content": "# Notes\n\nA passage of PRIV.\n"}))
	if issueDocument.Artifact.Slug != projectDocument.Artifact.Slug {
		t.Fatalf("the issue's document is %q and the project's %q: one slug fills both owners' rows", issueDocument.Artifact.Slug, projectDocument.Artifact.Slug)
	}
	topics = []string{issueSubscriberBase(issue) + ".>", documentSubscriberBase(seedPrivate, projectDocument.Artifact.Slug) + ".>"}
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, asCarol(http.MethodGet, "/api/v1/artifacts/"+projectDocument.Artifact.ID+"/blocks", nil))
	if len(blocks) == 0 {
		t.Fatalf("PRIV's project document holds no block")
	}
	ask := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{"question": "Which way for PRIV?"}))
	comment := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A remark on PRIV."}))
	message := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": "A note on PRIV.", "target": "session:s1", "delivery": "btw",
	}))
	result := decodeBody[recordedResult](t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/results",
		resultBody("k1", "pass", headCommit, nil))).Result.ID
	const group = "priv-readers"
	asCarol(http.MethodPut, "/api/v1/access-groups/"+group, map[string]any{"members": []string{seedCarol}})
	asCarol(http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/groups/"+group, nil)
	priority := createdID(t, asCarol(http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority PRIV's people steer"}))
	sessionGroup := createdID(t, asCarol(http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "PRIV's sessions", "sessions": []string{"s1"},
	}))
	// A repository PRIV maps, which a mapping write's {repo} names, renamed to acme/p_y; and one
	// PUB maps, an issue of which PRIV's issue links (privateHeldURL).
	asCarol(http.MethodPut, "/api/v1/settings/repo-projects/acme/p_x", map[string]any{"project": seedPrivate, "renamed_to": "acme/p_y"})
	asCarol(http.MethodPut, "/api/v1/settings/repo-projects/acme/w_pub", map[string]any{"project": seedPublic})
	asCarol(http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"external_links": []map[string]string{{"url": privateHeldURL}}})
	// s1, live at the walk's listener, holds PRIV's issue.
	seed.mustRequest(t, callerCarolPrivToken, http.MethodPost, "/api/v1/issues/"+issue+"/claim",
		map[string]any{"actor": map[string]string{"kind": "session", "id": "s1"}}, http.StatusOK)
	mintListingToken(t, seed, callerAlicePubToken, seedAlice, []string{seedPublic})
	// s-pub acts in PUB alone, through the shared token, which sees PUB: a session hidden from
	// every caller that sees PRIV alone.
	createdID(t, seed.request(t, callerSharedToken, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "s-pub works on PUB.", "actor": map[string]any{"kind": "session", "id": "s-pub"}}))
	// dana is a full member no list of PRIV names: hidden from every caller that sees PRIV alone.
	seedPeople(t, seed.database, hiddenPerson)
	// An architecture source of PRIV from acme/walked, the repository the source rows' {owner}/{repo}
	// name. Written to the store: the route's PUT asks GitHub, which the test server has not got.
	if _, err := seed.database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ($1, 'acme/walked', 'main', 1, '{"kind":"user","id":"carol@example.com"}')`, seedPrivate); err != nil {
		t.Fatalf("give PRIV an architecture source: %v", err)
	}
	var carolsToken string
	if err := seed.database.Pool.QueryRow(context.Background(), `select id::text from agent_tokens where name = $1`,
		string(callerCarolPrivToken)).Scan(&carolsToken); err != nil {
		t.Fatalf("read the id of %s: %v", callerCarolPrivToken, err)
	}

	same := func(value string) probeFill { return probeFill{hidden: value, missing: value} }
	nobody := func(value string) probeFill { return probeFill{hidden: value, missing: uuid.NewString()} }
	walk := hiddenWriteSeed{privateSeed: seed, trace: trace, calls: calls, fills: map[probeObject]probeFill{
		probeProject:         {hidden: seedPrivate, missing: "NOPE"},
		probeIssue:           {hidden: issue, missing: "NOPE-1"},
		probeDocument:        nobody(projectDocument.Artifact.ID),
		probeDocumentSlug:    same(projectDocument.Artifact.Slug),
		probeDocumentVersion: same("1"),
		probeBlock:           same(blocks[0].ID),
		probeAsk:             nobody(ask),
		probeComment:         nobody(comment),
		probeMessage:         nobody(message),
		probeThread:          nobody(message),
		probeResult:          nobody(result),
		probeAttempt:         same("1"),
		probeHandoffPhase:    same("build"),
		probeSession:         {hidden: "s-pub", missing: "s-nobody", beside: "s1"},
		probeEmail:           same(seedBob),
		probePerson:          {hidden: hiddenPerson, missing: "nobody@example.com"},
		probeRole:            {hidden: walkedRole, missing: "nobody-role"},
		probeGroup:           same(group),
		probeRepositoryOwner: same("acme"),
		probeRepositoryName:  same("walked"),
		probeLivePause:       same("walked"),
		probeAgentToken:      nobody(carolsToken),
		probePriority:        {hidden: priority, missing: "no-such-priority"},
		probeSessionGroup:    nobody(sessionGroup),
		// The secrets broker holds these, and the test server has none: both fills name nothing, and
		// the walk holds the deployment's refusal of a caller that is not open.
		probeCredentialRequest: nobody(uuid.NewString()),
		probeCredentialGrant:   nobody(uuid.NewString()),
		probeMachineLogin:      nobody(uuid.NewString()),
		probeSignin:            nobody(uuid.NewString()),
		// A run no record holds: the step write reads its issue first, which the key decides.
		probeStep: same(uuid.NewString()),
		// acme/p.x is on acme/p_x's GitHub subject lane, and no mapping holds it.
		probeMappedRepository: {hidden: "p_x", missing: "p.x"},
	}}
	rows, err := seed.database.Pool.Query(context.Background(), `
		select format('%I.%I', schemaname, tablename) from pg_tables
		where schemaname not in ('pg_catalog', 'information_schema') order by 1`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) == 0 {
		t.Fatalf("list the tables: %v (%d)", err, len(tables))
	}
	parts := make([]string, len(tables))
	for index, table := range tables {
		row := "to_jsonb(t)"
		if column, volatile := volatileColumns[table]; volatile {
			row += " - " + sqlString(column)
		}
		parts[index] = fmt.Sprintf(`select %s, md5(coalesce(string_agg((%s)::text, '|' order by (%s)::text), '')) from %s t`,
			sqlString(table), row, row, table)
	}
	walk.digest = strings.Join(parts, " union all ")
	return walk
}

// fill is pattern with each wildcard filled for the caller: the hidden object of each kind hidden
// from it, or one nobody holds of that kind when hidden is false, and each other kind's value beside
// it. It answers too what the second fill wrote in place of the first, so an answer that names what
// the caller sent can be compared.
func (w hiddenWriteSeed) fill(t *testing.T, pattern string, probe writeProbe, open, hidden bool) (string, map[string]string) {
	t.Helper()
	written := map[string]string{}
	path := patternWildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe.Probe[patternWildcard.FindStringSubmatch(match)[1]]
		value, ok := w.fills[object]
		if !ok {
			t.Fatalf("%s: the walk holds no value of %q", pattern, object)
		}
		switch {
		case !hiddenFrom(object, open):
			return value.besideValue()
		case hidden:
			return value.hidden
		}
		written[value.missing] = value.hidden
		return value.missing
	})
	return path, written
}

// tableDigests is a digest of every row of every table, by table, less volatileColumns, taken
// once the writes still owed have landed (settle).
func (w hiddenWriteSeed) tableDigests(t *testing.T) map[string]string {
	t.Helper()
	w.settle(t)
	rows, err := w.database.Pool.Query(context.Background(), w.digest)
	if err != nil {
		t.Fatalf("digest the tables: %v", err)
	}
	digests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		return pair, row.Scan(&pair[0], &pair[1])
	})
	if err != nil {
		t.Fatalf("digest the tables: %v", err)
	}
	byTable := make(map[string]string, len(digests))
	for _, pair := range digests {
		byTable[pair[0]] = pair[1]
	}
	return byTable
}

// settle waits for the writes that the seed or an earlier call left running past its answer: each
// document's settlement (awaitSettled, over every document the store holds), then the server's
// own background writes (awaitBackgroundWrites: the term-statistics refresh, detached runs and the
// mention sender's sends). A digest taken while one of them runs counts its write as the next
// call's: under load, the seed's settlements and term refresh landed inside a refused call's
// digests and named it as writing doc_settlements_pending and the search-term tables. Taken after
// them, each write is the call's that caused it.
func (w hiddenWriteSeed) settle(t *testing.T) {
	t.Helper()
	rows, err := w.database.Pool.Query(context.Background(), `select id::text from artifacts`)
	if err != nil {
		t.Fatalf("list the documents: %v", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the documents: %v", err)
	}
	awaitSettled(t, w.database, documents...)
	awaitBackgroundWrites(t, w.server)
}

// sqlString is value as a SQL string literal.
func sqlString(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// changedTables names every table whose rows differ between two digests.
func changedTables(before, after map[string]string) []string {
	var changed []string
	for table, digest := range after {
		if before[table] != digest {
			changed = append(changed, table)
		}
	}
	sort.Strings(changed)
	return changed
}

// compare calls one row as caller on PRIV's objects and on objects nobody holds, and holds the two
// to one answer and no write.
func (w hiddenWriteSeed) compare(t *testing.T, caller privateCaller, open bool, name string) {
	t.Helper()
	probe := writeProbes[name]
	method, pattern, _ := strings.Cut(name, " ")
	var body any
	if probe.Body != "" {
		body = json.RawMessage(probe.Body)
	}
	hiddenPath, _ := w.fill(t, pattern, probe, open, true)
	missingPath, written := w.fill(t, pattern, probe, open, false)
	missing := w.callPair(t, caller, name, method, hiddenPath, missingPath, body, body, written, false)
	if !answersMissing(name, open, missing) {
		t.Errorf("%s as %s: the call naming nothing (%s) answered %d %s, which is no answer for a missing object: the row's probe never reaches its read of the object",
			name, caller, missingPath, missing.Code, strings.TrimSpace(missing.Body.String()))
	}
}

// callPair makes one call naming a hidden object and one naming an object nobody holds, as caller,
// and holds them to one answer - the second's values written as the first's (written) - and to no
// write of any table, or, for a row that writes either way (writes), to writes of the same tables.
// A pair that writes nothing is held to the same work as well: the same listener calls, and the
// same statements, each ending on the same command tag, so a row the hidden call read, locked or
// wrote before its check, or a listener it asked, is named. It answers the second call's answer.
func (w hiddenWriteSeed) callPair(t *testing.T, caller privateCaller, name, method, hiddenPath, missingPath string,
	hiddenBody, missingBody any, written map[string]string, writes bool) *httptest.ResponseRecorder {
	t.Helper()
	before := w.tableDigests(t)
	hidden, hiddenStatements, hiddenCalls := w.recorded(t, caller, method, hiddenPath, hiddenBody)
	afterHidden := w.tableDigests(t)
	hiddenChanged := changedTables(before, afterHidden)
	missing, missingStatements, missingCalls := w.recorded(t, caller, method, missingPath, missingBody)
	missingChanged := changedTables(afterHidden, w.tableDigests(t))
	switch {
	case writes && !slices.Equal(hiddenChanged, missingChanged):
		t.Errorf("%s as %s: the call naming the hidden object changed %v, and the call naming nothing %v",
			name, caller, hiddenChanged, missingChanged)
	case !writes && len(hiddenChanged) > 0:
		t.Errorf("%s as %s: the call naming the hidden object (%s) answered %d %s and changed %v",
			name, caller, hiddenPath, hidden.Code, strings.TrimSpace(hidden.Body.String()), hiddenChanged)
	case !writes && len(missingChanged) > 0:
		t.Errorf("%s as %s: the call naming nothing (%s) answered %d %s and changed %v",
			name, caller, missingPath, missing.Code, strings.TrimSpace(missing.Body.String()), missingChanged)
	}
	if calls := rewritten(missingCalls, written); !slices.Equal(hiddenCalls, calls) {
		t.Errorf("%s as %s: the call naming the hidden object asked the listener %v, and the call naming nothing %v",
			name, caller, hiddenCalls, calls)
	}
	if statements := rewritten(missingStatements, written); !writes && otherWork[name] == "" && !slices.Equal(hiddenStatements, statements) {
		t.Errorf("%s as %s: the call naming the hidden object did other work than the call naming nothing, %s",
			name, caller, firstDifference(hiddenStatements, statements))
	}
	want := rewritten([]string{missing.Body.String()}, written)[0]
	if hidden.Code != missing.Code || hidden.Body.String() != want {
		t.Errorf("%s as %s: the hidden object answers %d %s, and an object nobody holds %d %s",
			name, caller, hidden.Code, strings.TrimSpace(hidden.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
	}
	return missing
}

// answersMissing reports whether a call naming no object was answered as the row answers a missing
// one: the deployment's refusal of a caller that is not open, which comes before the row's
// handler; else 404, the row's own answer (missingAnswers), or the refusal of a person at a
// browser that comes before any read (refusedAtABrowser).
func answersMissing(name string, open bool, answer *httptest.ResponseRecorder) bool {
	var refusal refusalAnswer
	_ = json.Unmarshal(answer.Body.Bytes(), &refusal)
	if !open && writeClasses[name] == scopeDeployment {
		return answer.Code == http.StatusForbidden && refusal.Code == codeProjectScopeDeployment
	}
	if declared, ok := missingAnswers[name]; ok {
		return answer.Code == declared.status && refusal.Code == declared.code
	}
	switch {
	case answer.Code == http.StatusNotFound:
		return true
	case answer.Code == http.StatusForbidden && open:
		return refusedAtABrowser[name] != "" && refusal.Code == refusedAtABrowser[name]
	}
	return false
}
