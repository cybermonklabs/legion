package api

// The tests of INFRA-4799's R8 that walk Dispatch's whole route table. The cases of
// private_project_writes_test.go name the write checks one by one. These walk every mutating route
// whose declaration states the class scopeProject (routeDecl.WriteClass), whichever object its
// path names (an issue, a project, a document, an ask, a comment, a message or a check result),
// and send each as a broker token that names one project:
//
//   - at an object of another project that is public: the answer is never 403 PROJECT_SCOPE, which
//     is what a credential that names no project gets at such an object too (R8's first sentence,
//     for every route);
//   - at an object of a private project that the token does not name: the answer is the answer to
//     the same write at an object that does not exist, in status and code, never a 2xx, and nothing
//     is written; and the same write by a token that names the private project is never 403
//     PROJECT_SCOPE.
//
// Each route is sent objects of its own, of the one kind its path names, so a write that deletes
// one leaves every other route its own, and the routes run beside each other: in the walk at a
// public project, every route on the one server; in the walk at a private project, whose check
// that nothing is written counts whole tables, each kind of object on a server of its own, its
// routes one after another. A route that needs the person scope answers a broker token
// OAUTH_SCOPE_REQUIRED before the project check reads anything, for a missing object as for a held
// one. The walk at a private project counts those routes and holds them to one answer all the
// same; the walk at a public project sends each route at a missing object first, and files nothing
// for a route that answers so.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// sweepKinds are the kinds of object a route's path can name first, each as the path's first
// segment after /api/v1/ spells it.
var sweepKinds = []string{"issues", "projects", "artifacts", "asks", "comments", "messages", "results"}

// sweepKind is the kind of object pattern names first, or "" for a kind the walk does not know.
func sweepKind(pattern string) string {
	kind, _, _ := strings.Cut(strings.TrimPrefix(pattern, "/api/v1/"), "/")
	if !slices.Contains(sweepKinds, kind) {
		return ""
	}
	return kind
}

var sweepCounter atomic.Int64

// sweepMake files, with the person's own credential, the objects of project that a route whose
// path names kind reads, and answers the ids of those of kind, each an object the route is sent
// at. A route reads only the objects its path names, so each kind files what those rest on and
// nothing more: an issue route an issue; a project route nothing, since the project's key is the
// object; a document route an issue document and a project document; an ask route and a comment
// route one on an issue and one anchored in a project document; a message route one on an issue;
// and a results route two results, with the issues and the Verification documents they are
// recorded on and against.
func (rig *privateProjectRig) sweepMake(t *testing.T, project, kind string) []string {
	t.Helper()
	created := func(what string, response *httptest.ResponseRecorder) string {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, response).ID
	}
	record := func(what, issue string) string {
		t.Helper()
		response := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+issue+"/results", resultBody("k1", "pass", newHeadCommit, nil))
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return decodeBody[recordedResult](t, response).Result.ID
	}
	fileIssue := func() string {
		t.Helper()
		return rig.issue(t, project, "A sweep issue", map[string]any{"spec": "# Spec"})
	}
	// Each project document takes a name of its own, so its slug is design-notes-<n>: none is
	// design-notes, the slug every {slug} of the walk names.
	projectDocument := func() model.Artifact {
		t.Helper()
		return createProjectDocument(t, rig.handler, project, fmt.Sprintf("Design notes %d", sweepCounter.Add(1)), "# Design notes\n")
	}
	switch kind {
	case "issues":
		return []string{fileIssue()}
	case "projects":
		return []string{project}
	case "artifacts":
		issue := fileIssue()
		issueDoc := uploadDocument(t, rig.handler, "/api/v1/issues/"+issue, "notes.md", "# Notes\n\nOne.\n").Artifact
		return []string{projectDocument().ID, issueDoc.ID}
	case "asks":
		issue, projectDoc := fileIssue(), projectDocument()
		anchor := map[string]string{"artifact": projectDoc.Slug, "quote": "Design"}
		return []string{
			created("ask on the issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{"question": "Which?"})),
			created("ask on the document", rig.asPerson(t, http.MethodPost, "/api/v1/artifacts/"+projectDoc.ID+"/asks", map[string]any{"question": "Ready?", "anchor": anchor})),
		}
	case "comments":
		issue, projectDoc := fileIssue(), projectDocument()
		anchor := map[string]string{"artifact": projectDoc.Slug, "quote": "Design"}
		return []string{
			created("comment on the issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A remark."})),
			created("comment on the document", rig.asPerson(t, http.MethodPost, "/api/v1/artifacts/"+projectDoc.ID+"/comments", map[string]any{"body": "Use Plan.", "anchor": anchor})),
		}
	case "messages":
		issue := fileIssue()
		return []string{created("message on the issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{"body": "A note."}))}
	case "results":
		// A result recorded on the project's own issue, and a result recorded on an issue of the
		// project against the check of an issue of the token's project above it, whose project is
		// not the project of the issue that the result was recorded on.
		issue := fileIssue()
		uploadDocument(t, rig.handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
		holder := rig.issue(t, brokerTestProject, "A holder of a check", map[string]any{"spec": "# Spec"})
		uploadDocument(t, rig.handler, "/api/v1/issues/"+holder, "verification.md", twoChecks)
		below := rig.issue(t, project, "An issue that records on the holder's check", map[string]any{"parent": holder})
		return []string{record("result on the issue", issue), record("result against a check of another project", below)}
	}
	t.Fatalf("the walk files no object of kind %q", kind)
	return nil
}

var sweepWildcard = regexp.MustCompile(`\{([a-z_]+)\}`)

// sweepPath is pattern at the object id of the kind its path names. Its first wildcard is that
// object. Each other wildcard takes a value of its own, which names no object the walk files but
// for {number}, the first version every document has: no document's slug is design-notes
// (projectDocument in sweepMake), and the documents an artifact route is sent at hold no block k1,
// which only a Verification document of a results route holds.
func sweepPath(pattern, id string) string {
	first := true
	return sweepWildcard.ReplaceAllStringFunc(pattern, func(wildcard string) string {
		if first {
			first = false
			return id
		}
		switch wildcard {
		case "{slug}":
			return "design-notes"
		case "{block_id}":
			return "k1"
		case "{session_id}", "{session}":
			return "a-session"
		case "{phase}":
			return "implement"
		case "{owner}":
			return "owner"
		case "{repo}":
			return "repo"
		}
		return "1"
	})
}

// sweepMissing is the path of pattern at an object of kind, the kind its path names, that does not
// exist. It needs no object filed, so a route can be sent there before the walk files its objects.
func sweepMissing(pattern, kind string) string {
	switch kind {
	case "issues":
		return sweepPath(pattern, missingIssue)
	case "projects":
		return sweepPath(pattern, missingProject)
	}
	return sweepPath(pattern, uuid.NewString())
}

// sweepHeld is the path of pattern at each object of ids.
func sweepHeld(pattern string, ids []string) []string {
	held := make([]string, 0, len(ids))
	for _, id := range ids {
		held = append(held, sweepPath(pattern, id))
	}
	return held
}

// sweepAnswer is the status and the code of one answer.
type sweepAnswer struct {
	status int
	code   string
}

// sweepSigning holds the walks to one signature at a time. The routes of the walk at a public
// project run beside each other through one caller, and ciphertokentest's Caller.Signed answers
// the last request its transport signed, so two proofs signed at once could each come back as the
// other's.
var sweepSigning sync.Mutex

// sweepSend is the answer to the write of method to path under token. A route that Dispatch does
// not mount (a test hook of a build that mounts none) answers its router's plain 404.
func (rig *privateProjectRig) sweepSend(t *testing.T, token, method, path string) sweepAnswer {
	t.Helper()
	signed := func() *http.Request {
		sweepSigning.Lock()
		defer sweepSigning.Unlock()
		return rig.caller.presenting(t, method, path, token, map[string]any{})
	}()
	response := rig.serve(signed)
	code := ""
	if response.Code >= http.StatusBadRequest {
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			code = "NOT_JSON"
		} else {
			code = body.Code
		}
	}
	return sweepAnswer{response.Code, code}
}

// projectWriteRoutes is every mutating route that writes an object belonging to one project, by "<method>
// <pattern>".
func projectWriteRoutes() []string {
	var routes []string
	for _, route := range routeRegistry {
		if route.WriteClass == scopeProject {
			routes = append(routes, route.Method+" "+route.Pattern)
		}
	}
	sort.Strings(routes)
	return routes
}

// TestEveryProjectRouteWritesAnotherPublicProjectsObject: for every route of class scopeProject, a
// token that names BRK gets no 403 PROJECT_SCOPE at an object that exists in the public project
// PUB, while the same write at an object that does not exist is 403 PROJECT_SCOPE, or is refused
// OAUTH_SCOPE_REQUIRED first where the route needs the person scope.
func TestEveryProjectRouteWritesAnotherPublicProjectsObject(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PUB", "name": "PUB"}); created.Code != http.StatusCreated {
		t.Fatalf("create project PUB: status=%d body=%s", created.Code, created.Body.String())
	}
	// The routes run beside each other on the one server, each at objects of its own, so the
	// counts are read once the group has returned, when every route has.
	var reached, routes atomic.Int64
	t.Run("each route", func(t *testing.T) {
		for _, route := range projectWriteRoutes() {
			t.Run(route, func(t *testing.T) {
				t.Parallel()
				method, pattern, _ := strings.Cut(route, " ")
				kind := sweepKind(pattern)
				if kind == "" {
					t.Errorf("%s names no kind of object that the walk knows", route)
					return
				}
				// The object that does not exist needs nothing filed, so the walk sends it first
				// and files no objects for a route that it leaves there.
				absent := rig.sweepSend(t, scopedToken, method, sweepMissing(pattern, kind))
				if absent.code == "OAUTH_SCOPE_REQUIRED" || absent.code == "NOT_JSON" {
					return
				}
				if absent.code != codeProjectScope {
					t.Errorf("%s at an object that does not exist: %d %s, want 403 %s", route, absent.status, absent.code, codeProjectScope)
				}
				for _, path := range sweepHeld(pattern, rig.sweepMake(t, "PUB", kind)) {
					if got := rig.sweepSend(t, scopedToken, method, path); got.code == codeProjectScope {
						t.Errorf("%s at %s: %d %s, want the handler's answer, as a credential that names no project gets", route, path, got.status, got.code)
					}
					reached.Add(1)
				}
				routes.Add(1)
			})
		}
	})
	t.Logf("the walk reached the project check of %d routes, at %d objects", routes.Load(), reached.Load())
	if reached.Load() == 0 {
		t.Fatal("the walk reached no route's project check")
	}
}

// TestEveryProjectRouteAnswersAPrivateProjectsObjectAsAMissingOne: for every route of class
// scopeProject, a token that names BRK is answered at an object of the private project PRIV as it
// is at an object that does not exist, with no 2xx and nothing written; a token that names PRIV is
// not refused PROJECT_SCOPE.
func TestEveryProjectRouteAnswersAPrivateProjectsObjectAsAMissingOne(t *testing.T) {
	t.Parallel()
	byKind := map[string][]string{}
	for _, route := range projectWriteRoutes() {
		_, pattern, _ := strings.Cut(route, " ")
		kind := sweepKind(pattern)
		if kind == "" {
			t.Errorf("%s names no kind of object that the walk knows", route)
			continue
		}
		byKind[kind] = append(byKind[kind], route)
	}
	// written counts whole tables, so no two routes of one server may write at once: each kind of
	// object has a server of its own, whose routes run one after another, and the kinds run beside
	// each other. The counts are read once the group has returned, when every kind has.
	var refused, routes atomic.Int64
	t.Run("each kind", func(t *testing.T) {
		for _, kind := range sweepKinds {
			t.Run(kind, func(t *testing.T) {
				t.Parallel()
				rig := newPrivateProjectRig(t, testServerOptions{})
				type objects struct {
					route, method string
					held          []string
					missing       string
				}
				var filed []objects
				// The objects are filed while PRIV is public, so that each write below meets
				// objects that exist, then PRIV is made private.
				for _, route := range byKind[kind] {
					method, pattern, _ := strings.Cut(route, " ")
					held := sweepHeld(pattern, rig.sweepMake(t, privateTestProject, kind))
					filed = append(filed, objects{route, method, held, sweepMissing(pattern, kind)})
				}
				rig.makePrivate(t)
				for _, set := range filed {
					before := rig.written(t)
					absent := rig.sweepSend(t, scopedToken, set.method, set.missing)
					reached := false
					for _, path := range set.held {
						got := rig.sweepSend(t, scopedToken, set.method, path)
						if got.status < http.StatusBadRequest {
							t.Errorf("%s at %s by a token that does not name the private project: %d, want a refusal", set.route, path, got.status)
						}
						if got != absent {
							t.Errorf("%s at %s: %d %s, and at an object that does not exist %d %s, want one answer", set.route, path, got.status, got.code, absent.status, absent.code)
						}
						if got.code == codeProjectScope {
							refused.Add(1)
							reached = true
						}
					}
					if reached {
						routes.Add(1)
					}
					if after := rig.written(t); after != before {
						t.Errorf("%s changed %s to %s for a token that does not name the private project", set.route, before, after)
					}
				}
				// The token that names PRIV is sent at objects filed once PRIV is private, and
				// sweepMake fails on any answer but 201, so this also holds that a person files
				// each kind of object in a private project.
				for _, route := range byKind[kind] {
					method, pattern, _ := strings.Cut(route, " ")
					for _, path := range sweepHeld(pattern, rig.sweepMake(t, privateTestProject, kind)) {
						if got := rig.sweepSend(t, namingToken, method, path); got.code == codeProjectScope {
							t.Errorf("%s at %s by the token that names the private project: %d %s, want the handler's answer", route, path, got.status, got.code)
						}
					}
				}
			})
		}
	})
	t.Logf("the walk reached the project check of %d routes: %d objects were refused %s", routes.Load(), refused.Load(), codeProjectScope)
	if refused.Load() == 0 {
		t.Fatal("the walk reached no route's project check")
	}
}

// foreignKeys is the keys that an answer's issues list names.
func foreignKeys[T any](issues []T, key func(T) string) []string {
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		keys = append(keys, key(issue))
	}
	return keys
}

// TestAnAcceptanceListsOnlyThePublicChild: a PATCH to retro that carries refuse_open_foreign_children
// over open children of a private and of a public project is 409 FOREIGN_CHILD_OPEN listing the
// public child alone, and over a private child alone lists none; each refuses the write.
func TestAnAcceptanceListsOnlyThePublicChild(t *testing.T) {
	t.Parallel()
	rig := newCascadeRig(t)
	privateOnly := rig.issue(t, brokerTestProject, "Retro with a private child", nil)
	secret := rig.issue(t, privateTestProject, "Secret retro child", map[string]any{"parent": privateOnly})
	mixed := rig.issue(t, brokerTestProject, "Retro with mixed children", nil)
	publicChild := rig.issue(t, "PUB", "Public retro child", map[string]any{"parent": mixed})
	secretChild := rig.issue(t, privateTestProject, "Secret mixed child", map[string]any{"parent": mixed})
	rig.makePrivate(t)

	body := map[string]any{"status": "retro", "refuse_open_foreign_children": true}
	only := rig.send(t, scopedToken, http.MethodPatch, "/api/v1/issues/"+privateOnly, body)
	answer := decodeBody[foreignChildOpenError](t, only)
	if only.Code != http.StatusConflict || answer.Code != "FOREIGN_CHILD_OPEN" || len(answer.Issues) != 0 || strings.Contains(only.Body.String(), secret) {
		t.Errorf("over a private child alone: status=%d body=%s", only.Code, only.Body.String())
	}
	pair := rig.send(t, scopedToken, http.MethodPatch, "/api/v1/issues/"+mixed, body)
	answer = decodeBody[foreignChildOpenError](t, pair)
	keys := foreignKeys(answer.Issues, func(child foreignChildOpen) string { return child.Key })
	if pair.Code != http.StatusConflict || answer.Code != "FOREIGN_CHILD_OPEN" || !slices.Equal(keys, []string{publicChild}) || strings.Contains(pair.Body.String(), secretChild) {
		t.Errorf("over a public and a private child: status=%d keys=%v body=%s", pair.Code, keys, pair.Body.String())
	}
}

// TestAParentStatusConflictListsOnlyThePublicChild: with the rule on, a close over open children of
// a public and of a private project is 409 listing the public child alone; an include_parent move
// under a private parent in triage is 409 listing none.
func TestAParentStatusConflictListsOnlyThePublicChild(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{parentStatusRule: true})
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PUB", "name": "PUB"}); created.Code != http.StatusCreated {
		t.Fatalf("create project PUB: status=%d body=%s", created.Code, created.Body.String())
	}
	mixed := rig.issue(t, brokerTestProject, "Closed with mixed children", nil)
	publicChild := rig.issue(t, "PUB", "Public open child", map[string]any{"parent": mixed})
	secretChild := rig.issue(t, privateTestProject, "Secret open child", map[string]any{"parent": mixed})
	secretParent := rig.issue(t, privateTestProject, "Secret triage parent", nil)
	under := rig.issue(t, brokerTestProject, "Child under a secret parent", map[string]any{"parent": secretParent})
	rig.makePrivate(t)

	type issueKey struct {
		Key string `json:"key"`
	}
	type conflict struct {
		Code   string     `json:"code"`
		Issues []issueKey `json:"issues"`
	}
	listed := func(c conflict) []string { return foreignKeys(c.Issues, func(i issueKey) string { return i.Key }) }
	closed := rig.send(t, scopedToken, http.MethodPost, "/api/v1/issues/"+mixed+"/status-change", map[string]any{"status": "done"})
	answer := decodeBody[conflict](t, closed)
	if closed.Code != http.StatusConflict || answer.Code != "PARENT_STATUS_CONFLICT" || !slices.Equal(listed(answer), []string{publicChild}) || strings.Contains(closed.Body.String(), secretChild) {
		t.Errorf("a close over a public and a private child: status=%d keys=%v body=%s", closed.Code, listed(answer), closed.Body.String())
	}
	moved := rig.send(t, scopedToken, http.MethodPost, "/api/v1/issues/"+under+"/status-change", map[string]any{"status": "in_progress", "include_parent": true})
	answer = decodeBody[conflict](t, moved)
	if moved.Code != http.StatusConflict || answer.Code != "PARENT_STATUS_CONFLICT" || len(answer.Issues) != 0 || strings.Contains(moved.Body.String(), secretParent) {
		t.Errorf("include_parent under a private parent with the rule on: status=%d body=%s", moved.Code, moved.Body.String())
	}
}
