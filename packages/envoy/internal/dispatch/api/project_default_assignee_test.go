package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type projectDefaultAssigneeRow struct {
	Project  string `json:"project"`
	Assignee string `json:"assignee"`
}

func createDefaultAssigneeProject(t *testing.T, handler http.Handler, key, name string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": name,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func putDefaultAssignee(t *testing.T, handler http.Handler, project string, assignee any) projectDefaultAssigneeRow {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-assignee",
		map[string]any{"assignee": assignee}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("put default assignee %v: status=%d body=%s", assignee, response.Code, response.Body.String())
	}
	return decodeBody[projectDefaultAssigneeRow](t, response)
}

// The PUT sets and changes a project's default, answering {project, assignee} each time, and
// refuses to clear it: a project always names the person its unowned work lands on.
func TestPutProjectDefaultAssigneeSetsChangesAndRefusesClearing(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	set := putDefaultAssignee(t, handler, "CORE", " Bob ")
	if set.Project != "CORE" || set.Assignee != "bob" {
		t.Fatalf("set = %#v, want CORE/bob", set)
	}
	changed := putDefaultAssignee(t, handler, "CORE", "Alice")
	if changed.Assignee != "alice" {
		t.Fatalf("changed = %#v, want alice", changed)
	}
	cleared := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{"assignee": nil}, "alice")
	if cleared.Code != http.StatusBadRequest || !strings.Contains(cleared.Body.String(), `"code":"INVALID_PROJECT"`) {
		t.Fatalf("clearing the default: status=%d body=%s, want 400 INVALID_PROJECT", cleared.Code, cleared.Body.String())
	}
	var stored string
	if err := database.Pool.QueryRow(context.Background(),
		`select default_assignee from projects where key = 'CORE'`).Scan(&stored); err != nil {
		t.Fatalf("read stored default: %v", err)
	}
	if stored != "alice" {
		t.Fatalf("stored default = %q, want alice: a refused clear stores nothing", stored)
	}
}

// An unknown project is a 404, and a login nobody has signed in as is refused before anything is
// stored.
func TestPutProjectDefaultAssigneeRefusesUnknownProjectAndLogin(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	missing := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/NOPE/default-assignee",
		map[string]any{"assignee": "bob"}, "alice")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown project: status=%d body=%s", missing.Code, missing.Body.String())
	}
	unlisted := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{"assignee": "mallory"}, "alice")
	if unlisted.Code != http.StatusBadRequest || !strings.Contains(unlisted.Body.String(), `"code":"ASSIGNEE_NOT_ALLOWED"`) {
		t.Fatalf("unlisted login: status=%d body=%s", unlisted.Code, unlisted.Body.String())
	}
	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	defaults := decodeBody[[]projectDefaultAssigneeRow](t, list)
	if len(defaults) != 1 || defaults[0].Project != "CORE" || defaults[0].Assignee != "alice" {
		t.Fatalf("a refused put stored something: %#v, want CORE still defaulting to the person who created it", defaults)
	}
}

// A body that names no assignee at all is refused and stores nothing, as a body whose assignee
// is null is: the contract's ProjectDefaultAssigneeInput requires the field, and a project
// always names the person its unowned work lands on, so neither a PUT of `{}` nor one of
// `{"assignee": null}` may wipe it.
func TestPutProjectDefaultAssigneeRefusesABodyWithoutAssignee(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	putDefaultAssignee(t, handler, "CORE", "bob")

	absent := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{}, "alice")
	if absent.Code != http.StatusBadRequest {
		t.Fatalf("a body without assignee: status=%d body=%s, want 400", absent.Code, absent.Body.String())
	}
	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	defaults := decodeBody[[]projectDefaultAssigneeRow](t, list)
	if len(defaults) != 1 || defaults[0].Project != "CORE" || defaults[0].Assignee != "bob" {
		t.Fatalf("a refused put changed the stored default: %#v, want CORE/bob", defaults)
	}
}

// The list route answers every project with the person it defaults to, by key — every project
// names one, the person who created it until somebody moves it — and an empty list, never null,
// where there is no project at all.
func TestListProjectDefaultAssigneesOrdersByProjectKey(t *testing.T) {
	handler := newTestHandler(t)

	empty := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	if empty.Code != http.StatusOK || strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatalf("list with no project: status=%d body=%s", empty.Code, empty.Body.String())
	}

	createDefaultAssigneeProject(t, handler, "OPS", "Ops")
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	createDefaultAssigneeProject(t, handler, "WEB", "Web")
	putDefaultAssignee(t, handler, "OPS", "bob")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list defaults: status=%d body=%s", response.Code, response.Body.String())
	}
	defaults := decodeBody[[]projectDefaultAssigneeRow](t, response)
	want := []projectDefaultAssigneeRow{{"CORE", "alice"}, {"OPS", "bob"}, {"WEB", "alice"}}
	if !slices.Equal(defaults, want) {
		t.Fatalf("defaults = %#v, want %#v", defaults, want)
	}
}

// Both routes need the person scope: a project's default names a person, so a credential the
// deployment configured rather than a person is turned away, and a person's own token sets and
// reads the default as their browser does.
func TestProjectDefaultAssigneeRoutesNeedThePersonScope(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	listed := agentRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "agent-token")
	if listed.Code != http.StatusForbidden || !strings.Contains(listed.Body.String(), `"code":"`+codeScopeRequired+`"`) {
		t.Fatalf("the shared token lists defaults: status=%d body=%s, want 403 %s", listed.Code, listed.Body.String(), codeScopeRequired)
	}
	put := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{"assignee": "bob", "actor": sessionActor()}, "agent-token")
	if put.Code != http.StatusForbidden || !strings.Contains(put.Body.String(), `"code":"`+codeScopeRequired+`"`) {
		t.Fatalf("the shared token sets a default: status=%d body=%s, want 403 %s", put.Code, put.Body.String(), codeScopeRequired)
	}

	token := personalTokenHolding(t, database, model.ScopePerson)
	if own := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{"assignee": "bob"}, token); own.Code != http.StatusOK {
		t.Fatalf("a person's own token sets a default: status=%d body=%s", own.Code, own.Body.String())
	}
	read := agentRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, token)
	if read.Code != http.StatusOK {
		t.Fatalf("a person's own token lists defaults: status=%d body=%s", read.Code, read.Body.String())
	}
	if rows := decodeBody[[]projectDefaultAssigneeRow](t, read); len(rows) != 1 || rows[0].Project != "CORE" ||
		rows[0].Assignee != "bob" {
		t.Fatalf("a person's own token read %#v, want CORE defaulting to bob", rows)
	}
}

// The PUT appends one quiet event on the project: settings, retained and streamed, waking nobody.
func TestPutProjectDefaultAssigneeAppendsAQuietEvent(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	putDefaultAssignee(t, handler, "CORE", "bob")

	var project, assignee string
	var notify bool
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(project_key, ''), payload->>'assignee', notify from events
		where type = 'settings.project_default_assignee.updated' order by id desc limit 1
	`).Scan(&project, &assignee, &notify); err != nil {
		t.Fatalf("read default assignee event: %v", err)
	}
	if project != "CORE" || assignee != "bob" || notify {
		t.Fatalf("event = project %q assignee %q notify %t, want CORE/bob and quiet", project, assignee, notify)
	}
}
