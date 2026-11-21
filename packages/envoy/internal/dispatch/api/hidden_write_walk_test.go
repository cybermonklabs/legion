package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The fixture and the comparisons the hidden-object walks of hidden_write_test.go share: the seed
// they call (seedHiddenWrites) and its Envoy listener (walkListener), the value of every probe kind
// (probeFill), the record of a call's statements and listener calls (recorded), the table digests
// (tableDigests), and the comparison of a call naming a hidden object with one naming an object
// nobody holds (compare, compareSessionBeside, callPair, answerAsHidden, answersMissing).

// timeStamp matches an RFC 3339 time, which two runs asked a moment apart each stamp their own.
var timeStamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// maskTimes is body with every RFC 3339 time written as one placeholder.
func maskTimes(body string) string { return timeStamp.ReplaceAllString(body, "<time>") }

// ageSeconds is an answer's `"age_seconds":N` member, a count of whole seconds since a row was made.
var ageSeconds = regexp.MustCompile(`"age_seconds":[0-9]+`)

// maskAges is body with every age in seconds written as one placeholder.
func maskAges(body string) string { return ageSeconds.ReplaceAllString(body, `"age_seconds":<age>`) }

// hiddenPerson is a full member no list of PRIV names, so a caller that sees PRIV alone does not
// see them (access.PersonClause), and an open viewer does.
const hiddenPerson = "dana@example.com"

// walkedRole is a role the walk's listener says s-pub holds; every other role it answers unclaimed.
const walkedRole = "walked-role"

// probeRole fills a role a delivery's target names: walkedRole, or one no session claims.
const probeRole probeObject = "role"

// on is path with {project}, {issue} and {ask} written as project's own: its key, its issue and
// its ask.
func (w hiddenWriteSeed) on(path, project string) string {
	return strings.NewReplacer("{project}", project, "{issue}", w.issue[project], "{ask}", w.fills[probeAsk].ofProject(project)).Replace(path)
}

// stampSessionTitle gives session title the way Dispatch learns one, an event the session wrote on
// issue carrying its origin's session_title, and names the session in a delivery pull request, as
// the title rule reads the sessions population pull requests name (delivery.ListPopulationSessionIDs).
// The event is written to the store, so it records no session_projects pair.
func stampSessionTitle(t *testing.T, walk hiddenWriteSeed, issue, session, title string) {
	t.Helper()
	ctx := t.Context()
	if _, err := walk.database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select $1::text, coalesce(max(seq), 0) + 1, 'message.created',
			jsonb_build_object('kind', 'session', 'id', $2::text, 'origin', jsonb_build_object('session_title', $3::text)),
			'{}', false, now()
		from events where issue_key = $1::text`, issue, session, title); err != nil {
		t.Fatalf("stamp %s's title on %s: %v", session, issue, err)
	}
	var number int
	if err := walk.database.Pool.QueryRow(ctx, `select coalesce(max(number), 0) + 1 from delivery_pull_requests where repo = 'acme/titles'`).Scan(&number); err != nil {
		t.Fatalf("the next titles pull request: %v", err)
	}
	merged := time.Now()
	if err := delivery.UpsertPullRequest(ctx, walk.database.Pool, delivery.DeliveryPullRequest{
		Repo: "acme/titles", Number: number, Title: "work by " + session, URL: fmt.Sprintf("https://github.com/acme/titles/pull/%d", number),
		Author: "octocat", MergedAt: &merged, CreatedAt: &merged, Sessions: []string{session},
	}); err != nil {
		t.Fatalf("a pull request naming %s: %v", session, err)
	}
}

// sessionVisibilityRead matches the statements that read whether the caller sees sessions a request
// names, as the trace writes them: one session's (sessionHiddenFrom) and a list's (seenSessions).
var sessionVisibilityRead = regexp.MustCompile(`^select (` + regexp.QuoteMeta(access.Marker+"SessionClause */") +
	`|\w+\.id from unnest\(\$1::text\[\]\) as \w+\(id\) where ` + regexp.QuoteMeta(access.Marker+"SessionClause */") + `)`)

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
	"POST /api/v1/issues/{key}/results": {http.StatusBadRequest, "UNKNOWN_CHECK",
		"a result names a check of its issue's Verification document, and an issue the caller cannot read holds none"},
	"POST /api/v1/issues/{key}/results/dry-run": {http.StatusBadRequest, "UNKNOWN_CHECK",
		"the record's dry run makes the record's every refusal before its transaction (readResultRequest), so it answers the record's UNKNOWN_CHECK"},
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
		if strings.Contains(name, "{key}") || strings.Contains(name, "{id}") || strings.Contains(name, "{session_id}") {
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

// walkSight is what one caller of the walk reads and writes. open is a caller that reads the rows of
// no project and the sessions an open viewer reads; held one whose writes a list holds, so a write of
// the deployment is refused it before its handler (writesHeldTo), though it may read the
// deployment's rows; and named a full member's broker token scoped to one project, whose write of
// any other project's object its scope refuses before the handler (namedProjectOf).
type walkSight struct{ open, held, named bool }

// hiddenFrom reports whether the walk names a hidden object of kind in place of one nobody holds,
// for a caller of sight: an object of a project is hidden from every caller of the walk, a
// personal token is someone else's, and a row of the deployment is hidden from a caller that is not
// open or whose writes a list holds. A session is hidden from every caller: privateSession, which
// has a session_projects pair of PRIV, a private project, from an open caller, which does not see
// PRIV, and s-pub, which has acted in no project, from a caller that is not open
// (access.SessionClause; probeFill.hiddenValue). Every other kind is part of an object the path
// already names - a document's slug, a version, a block, an attempt, a phase, a person or group on a
// list, a repository, a pause point - and is PRIV's own in both calls.
func hiddenFrom(object probeObject, sight walkSight) bool {
	switch object {
	case probeProject, probeIssue, probeDocument, probeAsk, probeComment, probeMessage, probeThread,
		probeResult, probeAgentToken, probeSession:
		return true
	case probePriority, probeSessionGroup, probeCredentialRequest, probeCredentialGrant,
		probeMachineLogin, probeSignin:
		return !sight.open || sight.held
	}
	return false
}

// hides reports whether the probe names an object hidden from the caller.
func (p writeProbe) hides(sight walkSight) bool {
	for _, object := range p.Probe {
		if hiddenFrom(object, sight) {
			return true
		}
	}
	return false
}

// hidesOnlySessions reports whether every object the probe hides from the caller is a session.
func (p writeProbe) hidesOnlySessions(sight walkSight) bool {
	for _, object := range p.Probe {
		if hiddenFrom(object, sight) && object != probeSession {
			return false
		}
	}
	return true
}

// namesASessionBeside reports whether the probe names a session beside another object: a subscriber
// of an issue or a document, or a follower of an ask.
func (p writeProbe) namesASessionBeside() bool {
	for _, object := range p.Probe {
		if object == probeSession {
			return len(p.Probe) > 1
		}
	}
	return false
}

// probeFill is one kind's values: the hidden object, one of that kind nobody holds, the one a call
// names beside another kind's hidden object ("" for the hidden one), the one hidden from an open
// caller where that is another ("" for the hidden one), and PUB's own ("" for a kind with none): a
// session subscribed to PRIV's issue is the one a subscriber row names beside that issue, a session
// of PUB alone is the one hidden from a caller that sees PRIV alone, and a session of PRIV the one
// hidden from an open caller, which does not see PRIV.
type probeFill struct{ hidden, missing, beside, hiddenFromOpen, public string }

// besideValue is the value a call names for a kind that is not the object it hides.
func (f probeFill) besideValue() string {
	if f.beside != "" {
		return f.beside
	}
	return f.hidden
}

// hiddenValue is the object of the kind hidden from an open caller or one that is not.
func (f probeFill) hiddenValue(open bool) string {
	if open && f.hiddenFromOpen != "" {
		return f.hiddenFromOpen
	}
	return f.hidden
}

// ofProject is the object of the kind that project holds: PRIV's is the hidden one, which a caller
// that sees PRIV sees, and PUB's its own.
func (f probeFill) ofProject(project string) string {
	if project == seedPublic {
		return f.public
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

// callerAlicePubToken is alice's token that lists PUB: a full member's, so it sees PUB and is open,
// and its list holds its writes from the deployment's rows; a token of carol's is someone else's
// to it.
const callerAlicePubToken privateCaller = "alice's token that lists PUB"

// callerAliceNamedToken is a broker token an operator's entry mapped to PUB, alice's: a full
// member's, so it reads as an open viewer that sees PUB, and its writes are held to PUB alone
// (namedProjectOf), so the scope refuses its write of any other project's object before the
// handler, an object it does not see answered as one nobody holds.
const callerAliceNamedToken privateCaller = "alice's broker token scoped to PUB"

// sayBrokered has the seed's broker answer for a live token of caller's, alice's and scoped to
// project by an operator's entry, proving session, which requestIn presents for caller.
func sayBrokered(seed privateSeed, caller privateCaller, session, project string) {
	token := "broker-token-of-" + string(caller)
	seed.broker.Says(token, ciphertoken.Introspection{
		Active: true, Session: session, Person: seedAlice,
		ProjectScope: ciphertoken.ProjectScopeNamed, Projects: []string{project}, Scope: "dispatch.write",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: seed.agent.jkt,
	})
	seed.brokered[caller] = token
}

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

// volatileTables are the tables a call writes whatever it names, left out of the digests: a broker
// token's presentation records its DPoP proof, so a replay of that proof is refused.
var volatileTables = map[string]bool{"public.dpop_proofs": true}

// volatileStatement reports whether a statement is the stamp of volatileColumns' column, which a
// token's first use in a minute writes and its next does not, whatever either call names.
func volatileStatement(statement string) bool {
	return strings.HasPrefix(statement, "update agent_tokens set last_used_at = now()")
}

// privateSession is a session that has acted in PRIV under the one credential that proves it
// (asSession), so it has a session_projects pair of PRIV, and an open viewer that does not see
// PRIV does not see it (access.SessionClause).
const privateSession = "s-priv"

// walkListener is a fake Envoy listener holding sessions s1, s-pub and privateSession live and
// each subscribed to every topic topics holds (both projects' issue and project document), so a
// write that removes a subscriber reaches the removal, whichever of them it names. It records every
// request it is sent in calls.
func walkListener(t *testing.T, topics *[]string, calls *listenerCalls) *httptest.Server {
	t.Helper()
	const s1 = `{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"projects":[],"last_seen":42}`
	// s-pub and privateSession are live too, so a delivery a caller may not make to one would
	// reach it: the person and session walk holds such a delivery to the failure one to a session
	// nobody holds records, and a broadcast to one to the refusal of a send to one nobody holds.
	const sPub = `{"session_id":"s-pub","title":"pub worker","dir":"/w/pub","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"projects":[],"last_seen":42}`
	const sPriv = `{"session_id":"` + privateSession + `","title":"priv worker","dir":"/w/priv","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"projects":["PRIV"],"last_seen":42}`
	records := map[string]string{"s1": s1, "s-pub": sPub, privateSession: sPriv}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(r)
		registered, held := records[strings.TrimPrefix(r.URL.Path, "/v1/registry/")]
		interested := strings.TrimPrefix(r.URL.Path, "/v1/interests/")
		_, subscribed := records[interested]
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/") && held:
			_, _ = w.Write([]byte(registered))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/"+walkedRole:
			_, _ = w.Write([]byte(`{"role":"` + walkedRole + `","holder":"s-pub","holder_state":"live",` + sPub[1:]))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role ` + role + `","reason":"unclaimed","claim_revision":0}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[` + s1 + `,` + sPub + `,` + sPriv + `]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/interests/") && subscribed:
			encoded, _ := json.Marshal(map[string]any{"session_id": interested, "topics": *topics, "updated_at": 1})
			_, _ = w.Write(encoded)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interests/":
			rows := []map[string]any{}
			for _, session := range slices.Sorted(maps.Keys(records)) {
				rows = append(rows, map[string]any{"session_id": session, "topics": *topics, "updated_at": 1})
			}
			encoded, _ := json.Marshal(rows)
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
// s1 and links privateHeldURL; PRIV maps acme/p_x, and PUB maps acme/w_pub, whose issue 9
// privateHeldURL is. PUB holds a project document and an ask of its own too, the objects a session
// hidden from a caller that sees PUB is named beside (compareSessionBeside): privateSession acts in
// PRIV and follows PUB's ask, and s-pub follows PRIV's.
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
	publicDocument := decodeBody[uploadedArtifact](t, asCarol(http.MethodPost, "/api/v1/projects/"+seedPublic+"/artifacts",
		map[string]string{"name": "walked-notes.md", "content": "# Notes\n\nA passage of PUB.\n"}))
	topics = []string{
		issueSubscriberBase(issue) + ".>", documentSubscriberBase(seedPrivate, projectDocument.Artifact.Slug) + ".>",
		issueSubscriberBase(seed.issue[seedPublic]) + ".>", documentSubscriberBase(seedPublic, publicDocument.Artifact.Slug) + ".>",
	}
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, asCarol(http.MethodGet, "/api/v1/artifacts/"+projectDocument.Artifact.ID+"/blocks", nil))
	if len(blocks) == 0 {
		t.Fatalf("PRIV's project document holds no block")
	}
	ask := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{"question": "Which way for PRIV?"}))
	publicAsk := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/asks", map[string]any{"question": "Which way for PUB?"}))
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
	// A repository PRIV maps, which an issue's external reference names; and one PUB maps, an
	// issue of which PRIV's issue links (privateHeldURL).
	asCarol(http.MethodPut, "/api/v1/settings/repo-projects/acme/p_x", map[string]any{"project": seedPrivate})
	asCarol(http.MethodPut, "/api/v1/settings/repo-projects/acme/w_pub", map[string]any{"project": seedPublic})
	asCarol(http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"external_links": []map[string]string{{"url": privateHeldURL}}})
	// s1, live at the walk's listener, holds PRIV's issue.
	seed.mustRequest(t, callerCarolPrivToken, http.MethodPost, "/api/v1/issues/"+issue+"/claim",
		map[string]any{"actor": map[string]string{"kind": "session", "id": "s1"}}, http.StatusOK)
	mintListingToken(t, seed, callerAlicePubToken, seedAlice, []string{seedPublic})
	sayBrokered(seed, callerAliceNamedToken, "s-alice-named", seedPublic)
	// s-pub acts in PUB alone, through the shared token, which sees PUB: a session hidden from
	// every caller that sees PRIV alone.
	createdID(t, seed.request(t, callerSharedToken, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "s-pub works on PUB.", "actor": map[string]any{"kind": "session", "id": "s-pub"}}))
	// privateSession acts in PRIV under the credential that proves it, which gives it a
	// session_projects pair of PRIV: a session hidden from an open caller that does not see PRIV.
	seed.asSession(t, privateSession, seedPrivate, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": privateSession + " works on PRIV."}, http.StatusCreated)
	// Dispatch holds the listener's record of s1 and s-pub, each naming no project, and of
	// privateSession, naming PRIV, as it does once any list of the sessions has read them (the
	// route_roster row): while a project is private, an open viewer reaches a session that has acted
	// in no project only through that record (access.SessionClause). No call of the walk reads the
	// list again.
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	// A Delivery session group of s1, which carol may name now that Dispatch holds s1's record: a
	// group write reads its sessions as its writer sees them (refuseHiddenSessions).
	sessionGroup := createdID(t, asCarol(http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "PRIV's sessions", "sessions": []string{"s1"},
	}))
	// Each hidden session follows an ask of a project its callers see, so an unfollow that reached
	// it would remove it.
	asCarol(http.MethodPut, "/api/v1/asks/"+ask+"/followers/s-pub", nil)
	asCarol(http.MethodPut, "/api/v1/asks/"+publicAsk+"/followers/"+privateSession, nil)
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
		probeIssue:           {hidden: issue, missing: "NOPE-1", public: seed.issue[seedPublic]},
		probeDocument:        {hidden: projectDocument.Artifact.ID, missing: uuid.NewString(), public: publicDocument.Artifact.ID},
		probeDocumentSlug:    same(projectDocument.Artifact.Slug),
		probeDocumentVersion: same("1"),
		probeBlock:           same(blocks[0].ID),
		probeAsk:             {hidden: ask, missing: uuid.NewString(), public: publicAsk},
		probeComment:         nobody(comment),
		probeMessage:         nobody(message),
		probeThread:          nobody(message),
		probeResult:          nobody(result),
		probeAttempt:         same("1"),
		probeHandoffPhase:    same("build"),
		probeSession:         {hidden: "s-pub", missing: "s-nobody", beside: "s1", hiddenFromOpen: privateSession},
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
		// No mapping holds acme/p.x.
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
	tables = slices.DeleteFunc(tables, func(table string) bool { return volatileTables[table] })
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
func (w hiddenWriteSeed) fill(t *testing.T, pattern string, probe writeProbe, sight walkSight, hidden bool) (string, map[string]string) {
	t.Helper()
	written := map[string]string{}
	path := patternWildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe.Probe[patternWildcard.FindStringSubmatch(match)[1]]
		value, ok := w.fills[object]
		if !ok {
			t.Fatalf("%s: the walk holds no value of %q", pattern, object)
		}
		switch {
		case !hiddenFrom(object, sight):
			return value.besideValue()
		case hidden:
			return value.hiddenValue(sight.open)
		}
		written[value.missing] = value.hiddenValue(sight.open)
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
func (w hiddenWriteSeed) compare(t *testing.T, caller privateCaller, sight walkSight, name string) {
	t.Helper()
	probe := writeProbes[name]
	method, pattern, _ := strings.Cut(name, " ")
	var body any
	if probe.Body != "" {
		body = json.RawMessage(probe.Body)
	}
	hiddenPath, _ := w.fill(t, pattern, probe, sight, true)
	missingPath, written := w.fill(t, pattern, probe, sight, false)
	missing := w.callPair(t, caller, name, method, hiddenPath, missingPath, body, body, written, false)
	if !answersMissing(name, sight, missing) {
		t.Errorf("%s as %s: the call naming nothing (%s) answered %d %s, which is no answer for a missing object: the row's probe never reaches its read of the object",
			name, caller, missingPath, missing.Code, strings.TrimSpace(missing.Body.String()))
	}
}

// compareSessionBeside calls one row whose path names a session beside another object as caller,
// with that object the one of its kind project holds, which the caller sees, and the session one
// hidden from it, then with the same object and a session nobody holds, and holds the two to one
// answer and no write (callPair), the call naming nothing to a missing object's answer.
func (w hiddenWriteSeed) compareSessionBeside(t *testing.T, caller privateCaller, sight walkSight, project, name string) {
	t.Helper()
	probe := writeProbes[name]
	method, pattern, _ := strings.Cut(name, " ")
	var body any
	if probe.Body != "" {
		body = json.RawMessage(probe.Body)
	}
	session := w.fills[probeSession]
	hidden := session.hiddenValue(sight.open)
	path := func(sessionID string) string {
		return patternWildcard.ReplaceAllStringFunc(pattern, func(match string) string {
			object := probe.Probe[patternWildcard.FindStringSubmatch(match)[1]]
			if object == probeSession {
				return sessionID
			}
			value := w.fills[object].ofProject(project)
			if value == "" {
				t.Fatalf("%s: the walk holds no %s of %s", name, object, project)
			}
			return value
		})
	}
	label := fmt.Sprintf("%s (%s beside an object of %s)", name, hidden, project)
	missing := w.callPair(t, caller, label, method, path(hidden), path(session.missing), body, body,
		map[string]string{session.missing: hidden}, false)
	if !answersMissing(name, sight, missing) {
		t.Errorf("%s as %s: the call naming a session nobody holds (%s) answered %d %s, which is no answer for a missing session",
			label, caller, path(session.missing), missing.Code, strings.TrimSpace(missing.Body.String()))
	}
}

// callPair makes one call naming a hidden object and one naming an object nobody holds, as caller,
// and holds them to one answer - the second's values written as the first's (written,
// answerAsHidden) - and to no write of any table, or, for a row that writes either way (writes), to
// writes of the same tables. A pair that writes nothing is held to the same work as well: the same
// listener calls, and the same statements, each ending on the same command tag, so a row the hidden
// call read, locked or wrote before its check, or a listener it asked, is named. It answers the
// second call's answer.
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
	if statements := rewritten(missingStatements, written); !writes && !slices.Equal(hiddenStatements, statements) {
		t.Errorf("%s as %s: the call naming the hidden object did other work than the call naming nothing, %s",
			name, caller, firstDifference(hiddenStatements, statements))
	}
	want := answerAsHidden(missing.Body.String(), written)
	if hidden.Code != missing.Code || hidden.Body.String() != want {
		t.Errorf("%s as %s: the hidden object answers %d %s, and an object nobody holds %d %s",
			name, caller, hidden.Code, strings.TrimSpace(hidden.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
	}
	return missing
}

// answerWord is one run of a key's characters in an answer: a project key, an issue key, a session.
var answerWord = regexp.MustCompile(`[A-Za-z0-9_-]+`)

// answerAsHidden is answer, the call naming nothing's, as the call naming the hidden object would
// read had it been answered as missing: each value written replaced (rewritten), and the project of
// each issue key written replaced by the hidden key's where it stands as a word of its own ("project
// NOPE", "legion-daemon:NOPE"). A key's project is the key up to its first -, which the caller sent
// itself, so an answer that names it says nothing the key did not; any other text of the hidden
// key's, its title say, is no word of the missing answer's, and still differs.
func answerAsHidden(answer string, written map[string]string) string {
	answer = rewritten([]string{answer}, written)[0]
	for missing, hidden := range written {
		missingProject, isKey := contracts.IssueProject(missing)
		hiddenProject, alsoKey := contracts.IssueProject(hidden)
		if !isKey || !alsoKey {
			continue
		}
		answer = answerWord.ReplaceAllStringFunc(answer, func(word string) string {
			if word == missingProject {
				return hiddenProject
			}
			return word
		})
	}
	return answer
}

// answersMissing reports whether a call naming no object was answered as the row answers a missing
// one: the deployment's refusal of a caller that is not open or whose writes a list holds, which
// comes before the row's handler; a named token's refusal of a project's row (403 PROJECT_SCOPE),
// which comes before the handler too; else 404, the row's own answer (missingAnswers), or the
// refusal of a person at a browser that comes before any read (refusedAtABrowser).
func answersMissing(name string, sight walkSight, answer *httptest.ResponseRecorder) bool {
	var refusal refusalAnswer
	_ = json.Unmarshal(answer.Body.Bytes(), &refusal)
	if (!sight.open || sight.held) && writeClasses[name] == scopeDeployment {
		return answer.Code == http.StatusForbidden && refusal.Code == codeProjectScopeDeployment
	}
	if sight.named && writeClasses[name] == scopeProject {
		return answer.Code == http.StatusForbidden && refusal.Code == codeProjectScope
	}
	if declared, ok := missingAnswers[name]; ok {
		return answer.Code == declared.status && refusal.Code == declared.code
	}
	switch {
	case answer.Code == http.StatusNotFound:
		return true
	case answer.Code == http.StatusForbidden && sight.open:
		return refusedAtABrowser[name] != "" && refusal.Code == refusedAtABrowser[name]
	}
	return false
}
