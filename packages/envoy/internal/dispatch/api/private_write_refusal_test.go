package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// callerCarolPubToken is carol's personal token that lists PUB. carol is a full member who sees
// PRIV, and a full member's list decides only which private projects her token sees
// (access.CredentialCovers): this one sees PUB and every other public project, and no private one,
// PRIV included.
const callerCarolPubToken privateCaller = "carol's token that lists PUB"

// seedSwitchedPrivate is verification v16's seed with PRIV made private the way an owner makes a
// project private in a deployment that names it on DISPATCH_PRIVATE_BEFORE_FULL_SWITCH: the marker
// rows of both backfills the switch waits for are written, as backfill-session-projects and
// scrub-access-events write them, and then carol, PRIV's owner, sends
// PATCH /api/v1/projects/PRIV/access {visibility: private} from her browser, which must answer 200.
// No hub record is asked for: a project private before its full switch needs none (ensurePrivate).
func seedSwitchedPrivate(t *testing.T, options testServerOptions) privateSeed {
	t.Helper()
	options.privateBeforeFullSwitch = []string{seedPrivate}
	seed := seedPublicProjectsWith(t, options)
	if _, err := seed.database.Pool.Exec(t.Context(), `
		insert into session_projects_filled default values;
		insert into access_events_scrubbed default values`); err != nil {
		t.Fatalf("write the markers of backfill-session-projects and scrub-access-events: %v", err)
	}
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/projects/"+seedPrivate+"/access",
		map[string]string{"visibility": model.ProjectPrivate}, http.StatusOK)
	if visibility := readVisibility(t.Context(), seed.database.Pool, seedPrivate); visibility != model.ProjectPrivate {
		t.Fatalf("PRIV is %s after its owner's switch, want private", visibility)
	}
	return seed
}

// projectObjects are the probe kinds that belong to a project: a write that names one of PRIV's is
// a write of PRIV, which a token that does not name PRIV may not see.
var projectObjects = []probeObject{probeProject, probeIssue, probeDocument, probeAsk, probeComment, probeMessage, probeThread, probeResult}

// bodyProjectWrites are the rows whose project the request's body names (scopeProjectInBody), each
// with a body that names its project at %s.
var bodyProjectWrites = map[string]string{
	"POST /api/v1/issues":         `{"project":"%s","title":"A walked issue"}`,
	"POST /api/v1/going/requests": `{"project":"%s","words":"What is walked?"}`,
}

// TestAPrivateProjectRefusesEveryWriteOfATokenThatDoesNotNameIt (R3; the gate's
// c-private-write-refusal, in its mode): once carol has made PRIV private, in a deployment that
// names it among the projects private before their full switch (seedSwitchedPrivate), every write
// row of the route table (RouteProbes) whose path names an object, and every row whose body names
// a project (bodyProjectWrites):
//
//  1. answers carol's token that lists PUB, for each object PRIV holds that the path names (an
//     object of PRIV, or a session that acts in PRIV alone: namesAPrivateObject), exactly as it
//     answers objects nobody holds - the same status, code and message, the same work - and writes
//     nothing (callPair): a private project is no project to a token that does not name it;
//  2. answers carol's token that lists PRIV, for PUB's object, other than an object nobody holds,
//     and never 403 PROJECT_SCOPE: a full member's list does not hold her token off a public
//     project it does not name;
//  3. answers that token, for PRIV's object, other than an object nobody holds, and never 403
//     PROJECT_SCOPE: the token that names the private project still writes it.
//
// Arms 2 and 3 call the rows whose path names an object of a project, of a project's or a
// session's write class: a write of the whole deployment is refused every credential that lists a
// project while any project is private, whatever it names (refuseOutsideCallerProjects, t-a-19).
// Arm 1 runs first, since it writes nothing; arms 2 and 3 write, so every DELETE runs after the
// other rows, and one that deletes the object its path names last of all.
//
// Control: answer one write site's privacy as public (private=false where it decides whether the
// caller sees the object's project): arm 1 fails, naming that row.
func TestAPrivateProjectRefusesEveryWriteOfATokenThatDoesNotNameIt(t *testing.T) {
	walk := seedHiddenWritesOn(t, seedSwitchedPrivate)
	mintListingToken(t, walk.privateSeed, callerCarolPubToken, seedCarol, []string{seedPublic})
	walk.fillPublicObjects(t)

	var objectRows, bodyRows []string
	for _, probe := range RouteProbes() {
		name := probe.Method + " " + probe.Pattern
		switch {
		case probe.Method == http.MethodGet || probe.TestHook:
			continue
		case writeClasses[name] == scopeProjectInBody:
			if _, ok := bodyProjectWrites[name]; !ok {
				t.Errorf("%s: its body names its project, and bodyProjectWrites gives it no body", name)
				continue
			}
			bodyRows = append(bodyRows, name)
		case probe.Probe == nil && (strings.Contains(probe.Pattern, "{key}") || strings.Contains(probe.Pattern, "{id}") || strings.Contains(probe.Pattern, "{session_id}")):
			t.Errorf("%s: a write whose path names an object, and writeProbes gives it no probe, so no arm calls it", name)
		case len(probe.Probe) > 0:
			objectRows = append(objectRows, name)
		}
	}
	if len(objectRows) == 0 || len(bodyRows) == 0 {
		t.Fatalf("UNKNOWN: the route table gave %d rows whose path names an object and %d rows whose body names a project; the walk read nothing",
			len(objectRows), len(bodyRows))
	}
	slices.SortStableFunc(objectRows, func(a, b string) int { return deleteRank(a) - deleteRank(b) })

	hiddenSight := walkSight{open: true, held: true}
	hidden, reached := 0, 0
	for _, name := range objectRows {
		if namesAPrivateObject(writeProbes[name]) {
			hidden++
			walk.compare(t, callerCarolPubToken, hiddenSight, name)
		}
	}
	for _, name := range bodyRows {
		walk.compareBodyProject(t, callerCarolPubToken, name, seedPrivate)
	}
	for _, name := range bodyRows {
		walk.reachesBodyProject(t, callerCarolPrivToken, name, seedPublic)
		walk.reachesBodyProject(t, callerCarolPrivToken, name, seedPrivate)
	}
	for _, name := range objectRows {
		if !namesAProjectObject(writeProbes[name]) || writeClasses[name] == scopeDeployment {
			continue
		}
		reached++
		walk.reaches(t, callerCarolPrivToken, name, seedPublic)
		walk.reaches(t, callerCarolPrivToken, name, seedPrivate)
	}
	if hidden == 0 || reached == 0 {
		t.Fatalf("UNKNOWN: of %d rows whose path names an object, arm 1 called %d and arms 2 and 3 called %d", len(objectRows), hidden, reached)
	}
	t.Logf("%d rows whose path names an object: arm 1 called %d, arms 2 and 3 called %d; %d rows whose body names a project, three arms each",
		len(objectRows), hidden, reached, len(bodyRows))
}

// deleteRank orders a row by what it removes: 0 for a row that is no DELETE, 1 for a DELETE of
// something its object holds, and 2 for a DELETE of the object its path names.
func deleteRank(row string) int {
	method, pattern, _ := strings.Cut(row, " ")
	switch {
	case method != http.MethodDelete:
		return 0
	case strings.HasSuffix(pattern, "{id}") && strings.Count(pattern, "{") == 1:
		return 2
	}
	return 1
}

// namesAProjectObject reports whether probe fills a wildcard with an object of a project.
func namesAProjectObject(probe writeProbe) bool {
	for _, object := range probe.Probe {
		if slices.Contains(projectObjects, object) {
			return true
		}
	}
	return false
}

// namesAPrivateObject reports whether probe fills a wildcard with an object PRIV holds: an object
// of a project, or a session, which the walk fills with one that acts in PRIV alone
// (privateSession). A row that names neither names an object of no project - a personal token, a
// priority, a Delivery session group, an object the secrets broker holds - whose visibility no
// project's privacy decides.
func namesAPrivateObject(probe writeProbe) bool {
	for _, object := range probe.Probe {
		if object == probeSession || slices.Contains(projectObjects, object) {
			return true
		}
	}
	return false
}

// fillPublicObjects gives each project kind the walk names its object of PUB, where the walk's seed
// made none: a comment, a message, the thread it starts, a check result and a block of PUB's
// document, each written by carol from her browser, and an architecture source of PUB from
// acme/walked, written to the store as the walk writes PRIV's (the route's PUT asks GitHub).
func (w hiddenWriteSeed) fillPublicObjects(t *testing.T) {
	t.Helper()
	issue := w.issue[seedPublic]
	made := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		response := w.request(t, callerCarol, method, path, body)
		if response.Code < 200 || response.Code > 299 {
			t.Fatalf("carol: %s %s = %d %s", method, path, response.Code, response.Body.String())
		}
		return response
	}
	comment := createdID(t, made(http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A remark on PUB."}))
	message := createdID(t, made(http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": "A note on PUB.", "target": "session:s1", "delivery": "btw",
	}))
	made(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "verification.md", "content": twoChecks})
	result := decodeBody[recordedResult](t, made(http.MethodPost, "/api/v1/issues/"+issue+"/results",
		resultBody("k1", "pass", headCommit, nil))).Result.ID
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, made(http.MethodGet, "/api/v1/artifacts/"+w.fills[probeDocument].public+"/blocks", nil))
	if len(blocks) == 0 {
		t.Fatalf("PUB's project document holds no block")
	}
	if _, err := w.database.Pool.Exec(t.Context(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ($1, 'acme/walked', 'main', 1, '{"kind":"user","id":"carol@example.com"}')`, seedPublic); err != nil {
		t.Fatalf("give PUB an architecture source: %v", err)
	}
	for object, public := range map[probeObject]string{
		probeProject: seedPublic, probeComment: comment, probeMessage: message, probeThread: message,
		probeResult: result, probeBlock: blocks[0].ID,
	} {
		fill := w.fills[object]
		fill.public = public
		w.fills[object] = fill
	}
}

// fillOf is pattern with each wildcard that names a project's object filled with project's object of
// that kind (probeFill.ofProject), or with one nobody holds when missing is set, and each other
// wildcard with the value the walk names beside it. It answers too what the missing fill wrote in
// place of project's, so the two answers can be compared.
func (w hiddenWriteSeed) fillOf(t *testing.T, name, project string, missing bool) (string, map[string]string) {
	t.Helper()
	probe := writeProbes[name]
	_, pattern, _ := strings.Cut(name, " ")
	written := map[string]string{}
	path := patternWildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe.Probe[patternWildcard.FindStringSubmatch(match)[1]]
		fill, ok := w.fills[object]
		if !ok {
			t.Fatalf("%s: the walk holds no value of %q", name, object)
		}
		if object == probeBlock {
			// A block is of the document the path names: PUB's document's block, or PRIV's.
			return fill.ofProject(project)
		}
		if !slices.Contains(projectObjects, object) {
			return fill.besideValue()
		}
		own := fill.ofProject(project)
		if own == "" {
			t.Fatalf("%s: the walk holds no %s of %s", name, object, project)
		}
		if !missing {
			return own
		}
		written[fill.missing] = own
		return fill.missing
	})
	return path, written
}

// reaches calls one row as caller on project's objects and on objects nobody holds, the missing
// call first since it writes nothing, and fails unless the call on project's objects is answered
// other than the missing one and is no 403 PROJECT_SCOPE: the caller's list let it reach the row's
// object.
func (w hiddenWriteSeed) reaches(t *testing.T, caller privateCaller, name, project string) {
	t.Helper()
	method, _, _ := strings.Cut(name, " ")
	var body any
	if probe := writeProbes[name]; probe.Body != "" {
		body = json.RawMessage(probe.Body)
	}
	missingPath, written := w.fillOf(t, name, project, true)
	ownPath, _ := w.fillOf(t, name, project, false)
	missing := w.request(t, caller, method, missingPath, body)
	own := w.request(t, caller, method, ownPath, body)
	w.requireReached(t, caller, name, project, ownPath, own, missing, written)
}

// compareBodyProject calls one row whose body names its project as caller, once naming project and
// once a project nobody holds, and holds the two to one answer and no write (callPair).
func (w hiddenWriteSeed) compareBodyProject(t *testing.T, caller privateCaller, name, project string) {
	t.Helper()
	method, path, _ := strings.Cut(name, " ")
	body := withProbeActor(bodyProjectWrites[name])
	missing := w.fills[probeProject].missing
	w.callPair(t, caller, name, method, path, path,
		json.RawMessage(fmt.Sprintf(body, project)), json.RawMessage(fmt.Sprintf(body, missing)),
		map[string]string{missing: project})
}

// reachesBodyProject is reaches for a row whose body names its project.
func (w hiddenWriteSeed) reachesBodyProject(t *testing.T, caller privateCaller, name, project string) {
	t.Helper()
	method, path, _ := strings.Cut(name, " ")
	body := withProbeActor(bodyProjectWrites[name])
	missing := w.fills[probeProject].missing
	answeredMissing := w.request(t, caller, method, path, json.RawMessage(fmt.Sprintf(body, missing)))
	own := w.request(t, caller, method, path, json.RawMessage(fmt.Sprintf(body, project)))
	w.requireReached(t, caller, name, project, path, own, answeredMissing, map[string]string{missing: project})
}

// requireReached fails, naming the row, the caller and project, when own, the answer to a call on
// project's objects, is a 403 PROJECT_SCOPE or the answer missing gave a call on objects nobody
// holds (its values written as own's, answerAsHidden). A row whose answer for an object nobody holds
// is no 404 (missingAnswers) gives that answer before it reads the object, or gives it for any
// object, so the two calls answer alike: such a row is held only to its declared answer where that
// is a success, and to no PROJECT_SCOPE.
func (w hiddenWriteSeed) requireReached(t *testing.T, caller privateCaller, name, project, path string, own, missing *httptest.ResponseRecorder, written map[string]string) {
	t.Helper()
	var refusal refusalAnswer
	_ = json.Unmarshal(own.Body.Bytes(), &refusal)
	if own.Code == http.StatusForbidden && refusal.Code == codeProjectScope {
		t.Errorf("%s as %s on %s's object (%s): 403 %s; a list that %s holds no write of it",
			name, caller, project, path, strings.TrimSpace(own.Body.String()), map[bool]string{true: "names it", false: "does not name a public project"}[project == seedPrivate])
		return
	}
	if declared, ok := missingAnswers[name]; ok {
		if declared.status < http.StatusBadRequest && own.Code != declared.status {
			t.Errorf("%s as %s on %s's object (%s): answered %d %s, want %d",
				name, caller, project, path, own.Code, strings.TrimSpace(own.Body.String()), declared.status)
		}
		return
	}
	if own.Code == missing.Code && own.Body.String() == answerAsHidden(missing.Body.String(), written) {
		t.Errorf("%s as %s on %s's object (%s): answered %d %s, as an object nobody holds; the row never reached %s's object",
			name, caller, project, path, own.Code, strings.TrimSpace(own.Body.String()), project)
	}
}
