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
// Each route is sent objects of its own, so a write that deletes one leaves the next route its own.
// A route that needs the person scope answers a broker token OAUTH_SCOPE_REQUIRED before the project
// check reads anything, for a missing object as for a held one, so the walk counts those routes and
// holds them to one answer all the same.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// sweepObjects are the ids of the objects one project holds, as a route's path names them.
type sweepObjects struct {
	project  string
	issue    string
	document []string // a project document and an issue document
	ask      []string // an issue ask and a document ask
	comment  []string // an issue comment and a document comment
	message  []string
	// result holds a result recorded on the project's own issue and a result recorded on an issue of
	// the project against a check of an issue of the token's project above it, whose project is not
	// the project of the issue that the result was recorded on.
	result []string
}

var sweepCounter atomic.Int64

// sweepMake files the objects of project with the person's own credential.
func (rig *privateProjectRig) sweepMake(t *testing.T, project string) sweepObjects {
	t.Helper()
	n := sweepCounter.Add(1)
	o := sweepObjects{project: project}
	o.issue = rig.issue(t, project, "A sweep issue", map[string]any{"spec": "# Spec"})
	uploadDocument(t, rig.handler, "/api/v1/issues/"+o.issue, "verification.md", twoChecks)
	issueDoc := uploadDocument(t, rig.handler, "/api/v1/issues/"+o.issue, "notes.md", "# Notes\n\nOne.\n").Artifact
	projectDoc := createProjectDocument(t, rig.handler, project, fmt.Sprintf("Design notes %d", n), "# Design notes\n")
	o.document = []string{projectDoc.ID, issueDoc.ID}
	created := func(what string, response *httptest.ResponseRecorder) string {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, response).ID
	}
	anchor := map[string]string{"artifact": projectDoc.Slug, "quote": "Design"}
	o.ask = []string{
		created("ask on the issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+o.issue+"/asks", map[string]any{"question": "Which?"})),
		created("ask on the document", rig.asPerson(t, http.MethodPost, "/api/v1/artifacts/"+projectDoc.ID+"/asks", map[string]any{"question": "Ready?", "anchor": anchor})),
	}
	o.comment = []string{
		created("comment on the issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+o.issue+"/comments", map[string]any{"body": "A remark."})),
		created("comment on the document", rig.asPerson(t, http.MethodPost, "/api/v1/artifacts/"+projectDoc.ID+"/comments", map[string]any{"body": "Use Plan.", "anchor": anchor})),
	}
	o.message = []string{created("message on the issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+o.issue+"/messages", map[string]any{"body": "A note."}))}
	record := func(what, issue string) string {
		t.Helper()
		response := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+issue+"/results", resultBody("k1", "pass", newHeadCommit, nil))
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return decodeBody[recordedResult](t, response).Result.ID
	}
	// A result on an issue of project against the check of the token's project's issue above it.
	holder := rig.issue(t, brokerTestProject, "A holder of a check", map[string]any{"spec": "# Spec"})
	uploadDocument(t, rig.handler, "/api/v1/issues/"+holder, "verification.md", twoChecks)
	below := rig.issue(t, project, "An issue that records on the holder's check", map[string]any{"parent": holder})
	o.result = []string{record("result on the issue", o.issue), record("result against a check of another project", below)}
	return o
}

var sweepWildcard = regexp.MustCompile(`\{([a-z_]+)\}`)

// targets are the paths of one route at each object of the kind its path names, and at an object
// of that kind that does not exist.
func (o sweepObjects) targets(pattern string) (held []string, missing string) {
	rest := strings.TrimPrefix(pattern, "/api/v1/")
	var ids []string
	var absent string
	switch strings.SplitN(rest, "/", 2)[0] {
	case "issues":
		ids, absent = []string{o.issue}, missingIssue
	case "projects":
		ids, absent = []string{o.project}, missingProject
	case "artifacts":
		ids, absent = o.document, uuid.NewString()
	case "asks":
		ids, absent = o.ask, uuid.NewString()
	case "comments":
		ids, absent = o.comment, uuid.NewString()
	case "messages":
		ids, absent = o.message, uuid.NewString()
	case "results":
		ids, absent = o.result, uuid.NewString()
	default:
		return nil, ""
	}
	fill := func(id string) string {
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
	for _, id := range ids {
		held = append(held, fill(id))
	}
	return held, fill(absent)
}

// sweepAnswer is the status and the code of one answer.
type sweepAnswer struct {
	status int
	code   string
}

// sweepSend is the answer to the write of method to path under token. A route that Dispatch does
// not mount (a test hook of a build that mounts none) answers its router's plain 404.
func (rig *privateProjectRig) sweepSend(t *testing.T, token, method, path string) sweepAnswer {
	t.Helper()
	response := rig.send(t, token, method, path, map[string]any{})
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
	reached := 0
	for _, route := range projectWriteRoutes() {
		method, pattern, _ := strings.Cut(route, " ")
		held, missing := rig.sweepMake(t, "PUB").targets(pattern)
		if len(held) == 0 {
			t.Errorf("%s names no kind of object that the walk knows", route)
			continue
		}
		absent := rig.sweepSend(t, scopedToken, method, missing)
		if absent.code == "OAUTH_SCOPE_REQUIRED" || absent.code == "NOT_JSON" {
			continue
		}
		if absent.code != codeProjectScope {
			t.Errorf("%s at an object that does not exist: %d %s, want 403 %s", route, absent.status, absent.code, codeProjectScope)
		}
		for _, path := range held {
			if got := rig.sweepSend(t, scopedToken, method, path); got.code == codeProjectScope {
				t.Errorf("%s at %s: %d %s, want the handler's answer, as a credential that names no project gets", route, path, got.status, got.code)
			}
			reached++
		}
	}
	if reached == 0 {
		t.Fatal("the walk reached no route's project check")
	}
}

// TestEveryProjectRouteAnswersAPrivateProjectsObjectAsAMissingOne: for every route of class
// scopeProject, a token that names BRK is answered at an object of the private project PRIV as it
// is at an object that does not exist, with no 2xx and nothing written; a token that names PRIV is
// not refused PROJECT_SCOPE.
func TestEveryProjectRouteAnswersAPrivateProjectsObjectAsAMissingOne(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	type objects struct {
		route, method string
		held          []string
		missing       string
	}
	var filed []objects
	// The objects are filed while PRIV is public, so that each write below meets objects that
	// exist, then PRIV is made private.
	for _, route := range projectWriteRoutes() {
		method, pattern, _ := strings.Cut(route, " ")
		held, missing := rig.sweepMake(t, privateTestProject).targets(pattern)
		if len(held) == 0 {
			t.Errorf("%s names no kind of object that the walk knows", route)
			continue
		}
		filed = append(filed, objects{route, method, held, missing})
	}
	rig.makePrivate(t)
	refused := 0
	for _, set := range filed {
		before := rig.written(t)
		absent := rig.sweepSend(t, scopedToken, set.method, set.missing)
		for _, path := range set.held {
			got := rig.sweepSend(t, scopedToken, set.method, path)
			if got.status < http.StatusBadRequest {
				t.Errorf("%s at %s by a token that does not name the private project: %d, want a refusal", set.route, path, got.status)
			}
			if got != absent {
				t.Errorf("%s at %s: %d %s, and at an object that does not exist %d %s, want one answer", set.route, path, got.status, got.code, absent.status, absent.code)
			}
			if got.code == codeProjectScope {
				refused++
			}
		}
		if after := rig.written(t); after != before {
			t.Errorf("%s changed %s to %s for a token that does not name the private project", set.route, before, after)
		}
	}
	if refused == 0 {
		t.Fatal("the walk reached no route's project check")
	}
	for _, route := range projectWriteRoutes() {
		method, pattern, _ := strings.Cut(route, " ")
		held, _ := rig.sweepMake(t, privateTestProject).targets(pattern)
		for _, path := range held {
			if got := rig.sweepSend(t, namingToken, method, path); got.code == codeProjectScope {
				t.Errorf("%s at %s by the token that names the private project: %d %s, want the handler's answer", route, path, got.status, got.code)
			}
		}
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
