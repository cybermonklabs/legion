package api

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// TestV2Refusals is c-refusals of dispatch://INFRA-4071 as far as the api
// package reaches it: a missing issue's comments are 404 ISSUE_NOT_FOUND; the route tour's
// missing-objects subtest covers every route whose path names an object Dispatch stores; a route
// whose path names a session, and the app-installation read, answer as /api/v1 does; and a parent
// loop and a dependency loop are declared with one code and answer it, each sent to PATCH
// /api/v2/issues/{key}. The document's code list equalling the declared codes is
// TestTheV2DocumentListsEveryCodeItCanAnswer (openapi_test.go), which leaves out API_V1_ENDED
// because only /api/v1 answers it; one condition with one code is TestOneConditionOneCode; the
// unknown path, /auth/callback and the origin guard are TestAuthAndOriginRefusalsCarryACode in the
// routes package.
func TestV2Refusals(t *testing.T) {
	tour := newRouteTour(t)

	t.Run("a missing issue's comments", func(t *testing.T) {
		response := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v2/issues/NOPE-9/comments", nil, "alice")
		if code, _ := refusalOf(t, response); response.Code != http.StatusNotFound || code != "ISSUE_NOT_FOUND" {
			t.Fatalf("GET /api/v2/issues/NOPE-9/comments answered %d %s, want 404 ISSUE_NOT_FOUND", response.Code, response.Body.String())
		}
	})

	t.Run("every path object a route names is a missing-objects step", func(t *testing.T) {
		covered := map[string]map[string]bool{}
		for _, step := range tourSteps() {
			for _, missing := range step.Missing {
				if covered[step.route()] == nil {
					covered[step.route()] = map[string]bool{}
				}
				covered[step.route()][missing.Param] = true
			}
		}
		for _, name := range slices.Sorted(maps.Keys(tour.routes)) {
			route := tour.routes[name]
			for _, wildcard := range patternWildcards(route.Pattern) {
				if !namesAStoredObject(route, wildcard) {
					continue
				}
				if !covered[name][wildcard] {
					t.Errorf("%s names {%s}, an object Dispatch stores, and no step of the tour sends it missing", name, wildcard)
				}
			}
		}
	})

	t.Run("a session's route and the app-installation read answer as /api/v1 does", func(t *testing.T) {
		const session = "session-nobody-ever-ran"
		v1 := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v1/agents/"+session+"/messages", nil, "alice")
		v2 := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v2/agents/"+session+"/messages", nil, "alice")
		if listed := decodeBody[page[agentMessageV2]](t, v2); v1.Code != http.StatusOK || v1.Body.String() != "[]\n" ||
			v2.Code != http.StatusOK || listed.Total != 0 || len(listed.Items) != 0 {
			t.Fatalf("a session Dispatch has no row of: /api/v1 %d %s, /api/v2 %d %s, want both 200 and empty",
				v1.Code, v1.Body.String(), v2.Code, v2.Body.String())
		}
		v1 = dispatchRequest(t, tour.handler, http.MethodGet, "/api/v1/github/app-installation/nobody/nothing", nil, "alice")
		v2 = dispatchRequest(t, tour.handler, http.MethodGet, "/api/v2/repositories/nobody/nothing/app-installation", nil, "alice")
		if v1.Code != http.StatusOK || v2.Code != v1.Code || !jsonEqual(t, v2.Body.Bytes(), v1.Body.String()) {
			t.Fatalf("a repository GitHub does not show the App: /api/v1 %d %s, /api/v2 %d %s, want one answer",
				v1.Code, v1.Body.String(), v2.Code, v2.Body.String())
		}
	})

	t.Run("a parent loop and a dependency loop answer one code", func(t *testing.T) {
		declared := map[apicode.Condition]apicode.Code{}
		for _, route := range tour.routes {
			for _, refusal := range route.Refusals {
				if refusal.Condition == apicode.ParentLoop || refusal.Condition == apicode.DependencyLoop {
					declared[refusal.Condition] = refusal.Code
				}
			}
		}
		parent, dependency := declared[apicode.ParentLoop], declared[apicode.DependencyLoop]
		if parent.Name() != "ISSUE_CYCLE" || dependency.Name() != "ISSUE_CYCLE" || parent.Status() != http.StatusConflict || dependency.Status() != http.StatusConflict {
			t.Fatalf("the parent loop is declared %s %d and the dependency loop %s %d, want both 409 ISSUE_CYCLE",
				parent.Name(), parent.Status(), dependency.Name(), dependency.Status())
		}

		// The declaration is the row's and the answer is the handler's, so both loops are sent to
		// PATCH /api/v2/issues/{key} (its parent and blocked_by members): each answers 409
		// ISSUE_CYCLE and leaves the issue it names as it stood, with no parent and no blocker.
		project := tour.project(t)
		create := func(title string, fields map[string]any) string {
			t.Helper()
			body := map[string]any{"project": project, "title": title, "force": true}
			maps.Copy(body, fields)
			created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues", body, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create %q: %d %s", title, created.Code, created.Body.String())
			}
			return decodeBody[tourIssue](t, created).Key
		}
		refusesTheLoop := func(loop, key string, patch map[string]any) {
			t.Helper()
			refused := dispatchRequest(t, tour.handler, http.MethodPatch, "/api/v2/issues/"+key, patch, "alice")
			if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "ISSUE_CYCLE" {
				t.Errorf("%s: PATCH /api/v2/issues/%s answered %d %s, want 409 ISSUE_CYCLE", loop, key, refused.Code, refused.Body.String())
			}
			read := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v2/issues/"+key, nil, "alice")
			if read.Code != http.StatusOK {
				t.Fatalf("%s: GET /api/v2/issues/%s answered %d %s", loop, key, read.Code, read.Body.String())
			}
			if issue := decodeBody[struct {
				Parent    *string  `json:"parent"`
				BlockedBy []string `json:"blocked_by"`
			}](t, read); issue.Parent != nil || len(issue.BlockedBy) != 0 {
				t.Errorf("%s: %s reads %s after the refusal, want it as it stood, with no parent and no blocked_by", loop, key, read.Body.String())
			}
		}
		root := create("A loop's root", nil)
		child := create("A loop's child", map[string]any{"parent": root})
		refusesTheLoop("a parent loop", root, map[string]any{"parent": child})
		blocker := create("A loop's blocker", nil)
		waiter := create("A loop's waiter", map[string]any{"blocked_by": []string{blocker}})
		refusesTheLoop("a dependency loop", blocker, map[string]any{"blocked_by": []string{waiter}})
	})
}

// namesAStoredObject reports whether the wildcard of route's path names a row Dispatch stores,
// whose absence the route answers with that object's 404 (decision 11): every wildcard but a
// {session_id} (ruling A), the {owner}/{repo} of the app-installation read (ruling A) and of a PUT
// that creates the repository row, the {id} of a plan's PUT, which creates the plan whatever id it
// names (a plan write names its whole identity, so no plan is missing to it), a mark's {kind}, and
// the {id} of a deploy run, which is GitHub's run id of the deployment's deploy repository:
// decision 11 names no deploy run among the objects, and the read keeps /api/v1's 404 NOT_FOUND
// for a run Dispatch has not mirrored.
func namesAStoredObject(route apiRoute, wildcard string) bool {
	switch {
	case wildcard == "session_id":
		return false
	case (wildcard == "owner" || wildcard == "repo") && (strings.HasSuffix(route.Pattern, "/app-installation") || route.Method == http.MethodPut):
		return false
	case wildcard == "id" && route.Method == http.MethodPut && strings.HasSuffix(route.Pattern, "/plans/{id}"):
		return false
	case wildcard == "kind" && strings.Contains(route.Pattern, "/marks/{kind}/{id}"):
		return false
	case wildcard == "id" && strings.HasSuffix(route.Pattern, "/deploy-runs/{id}"):
		return false
	}
	return true
}
