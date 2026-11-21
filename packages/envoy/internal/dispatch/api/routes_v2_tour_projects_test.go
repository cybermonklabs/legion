package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func init() { addTourSteps("projects", projectsTourSteps) }

// projectsTourSteps are the route tour's steps of the project, architecture, repository,
// near-copy check, priority, settings and related-search routes: each route, the project list with
// its default assignees, the project patch of its gated steps alone by the deployment's daemon, the
// sync of one named source and of every source, the related search with the other projects'
// questions, and the priorities' count alone at limit 0.
func projectsTourSteps() []tourStep {
	alice := tourPerson("alice")
	projectMissing := []tourMissing{{Param: "key", Value: "NOPE", Code: apicode.ProjectNotFound}}
	priorityMissing := []tourMissing{{Param: "id", Value: "no-such-priority", Code: apicode.PriorityNotFound}}
	onProject := func(query url.Values, body any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{"key": tour.project(t)}, Query: query, Body: body, As: alice}
		}
	}
	onSource := func(body func(repo string) any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			project, repo := tour.architectureSource(t)
			owner, name, _ := strings.Cut(repo, "/")
			request := tourRequest{Path: map[string]string{"key": project, "owner": owner, "repo": name}, As: alice}
			if body != nil {
				request.Body = body(repo)
			}
			return request
		}
	}
	onPriority := func(body any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{"id": tour.priority(t)}, Body: body, As: alice}
		}
	}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/projects", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.project(t)
				return tourRequest{As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/projects", Name: "with their default assignees", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.project(t)
				return tourRequest{Query: url.Values{"include": {"default_assignee"}, "limit": {"5"}}, As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/projects", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"key": fmt.Sprintf("NEW%d", tour.keys.Add(1)), "name": "A project the tour creates"}, As: alice}
			},
		},
		{
			Method: http.MethodPatch, Pattern: "/projects/{key}", Status: http.StatusOK,
			Setup:   onProject(nil, map[string]any{"default_assignee": "alice", "gated_steps": []string{"spec"}}),
			Missing: projectMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/projects/{key}", Name: "its gated steps, by the deployment's daemon",
			Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"key": tour.project(t)},
					Body: map[string]any{
						"gated_steps": []string{"decisions", "spec"},
						"actor":       map[string]any{"kind": "session", "id": tourSession},
					},
					As: tourBearer("agent-token"),
				}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/architecture", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project, _ := tour.architectureSource(t)
				return tourRequest{Path: map[string]string{"key": project}, As: alice}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/architecture-sources", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project, _ := tour.architectureSource(t)
				return tourRequest{Path: map[string]string{"key": project}, Query: url.Values{"limit": {"1"}}, As: alice}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodDelete, Pattern: "/projects/{key}/architecture-sources/{owner}/{repo}", Status: http.StatusNoContent,
			Setup: onSource(nil),
			Missing: []tourMissing{
				projectMissing[0],
				{Param: "owner", Value: "no-such-owner", Code: apicode.SourceNotFound},
				{Param: "repo", Value: "no-such-source", Code: apicode.SourceNotFound},
			},
		},
		{
			Method: http.MethodPut, Pattern: "/projects/{key}/architecture-sources/{owner}/{repo}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				owner, name, _ := strings.Cut(tour.architectureRepo(t), "/")
				return tourRequest{
					Path: map[string]string{"key": tour.project(t), "owner": owner, "repo": name},
					Body: map[string]any{"branch": "main"}, As: alice,
				}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/architecture-syncs", Status: http.StatusOK,
			Setup:   onSource(func(repo string) any { return map[string]any{"repos": []string{repo}} }),
			Missing: projectMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/architecture-syncs", Name: "every source", Status: http.StatusOK,
			Setup:   onSource(func(string) any { return map[string]any{} }),
			Missing: projectMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/related-searches", Status: http.StatusOK,
			Setup:   onProject(nil, map[string]any{"title": "Astrolabe calibration", "text": "The tour searches for its words."}),
			Missing: projectMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/related-searches", Name: "with the other projects' questions",
			Status:  http.StatusOK,
			Setup:   onProject(url.Values{"include": {"other_projects"}}, map[string]any{"title": "Astrolabe calibration", "keywords_only": true}),
			Missing: projectMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/near-copy-checks", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project, _ := tour.nearCopyCheck(t)
				return tourRequest{
					Path:  map[string]string{"key": project},
					Query: url.Values{"from": {"2026-01-01T00:00:00Z"}, "to": {"2099-01-01T00:00:00Z"}, "limit": {"10"}}, As: alice,
				}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/near-copy-checks/{id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				_, id := tour.nearCopyCheck(t)
				return tourRequest{Path: map[string]string{"id": id}, As: alice}
			},
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.NearCopyCheckNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/repositories", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project, _ := tour.repository(t)
				return tourRequest{Query: url.Values{"project": {project, tour.project(t)}}, As: alice}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/repositories/{owner}/{repo}", Status: http.StatusNoContent,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				_, repo := tour.repository(t)
				owner, name, _ := strings.Cut(repo, "/")
				return tourRequest{Path: map[string]string{"owner": owner, "repo": name}, As: alice}
			},
			Missing: []tourMissing{
				{Param: "owner", Value: "no-such-owner", Code: apicode.RepositoryNotFound},
				{Param: "repo", Value: "no-such-repository", Code: apicode.RepositoryNotFound},
			},
		},
		{
			Method: http.MethodPut, Pattern: "/repositories/{owner}/{repo}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"owner": "tour", "repo": "mapped-" + uuid.NewString()[:8]},
					Body: map[string]any{"project": tour.project(t), "default": true}, As: alice,
				}
			},
		},
		{
			// GitHub answers 404 for a repository it does not show the App, which is {installed:
			// false} and no 404 of Dispatch's (ruling A), so the step names no missing object.
			Method: http.MethodGet, Pattern: "/repositories/{owner}/{repo}/app-installation", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"owner": "tour", "repo": "not-installed"}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/priorities", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.priority(t)
				return tourRequest{Query: url.Values{"include": {"fault_issues"}, "project": {tour.project(t)}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/priorities", Name: "the counts alone", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"limit": {"0"}}, As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/priorities", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"title": fmt.Sprintf("A priority the tour writes %d", tour.keys.Add(1))}, As: alice}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/priorities/{id}", Status: http.StatusNoContent,
			Setup: onPriority(nil), Missing: priorityMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/priorities/{id}", Status: http.StatusOK,
			Setup:   onPriority(map[string]any{"title": "The tour renamed this priority", "owner": "alice"}),
			Missing: priorityMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/priorities/{id}/acts", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"id": tour.priority(t)}, Query: url.Values{"sort": {"at"}}, As: alice}
			},
			Missing: priorityMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/settings/new-issue", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodGet, Pattern: "/settings/credential-broker", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodGet, Pattern: "/settings/text-model", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodGet, Pattern: "/related-check-stats", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{
					"project": {tour.project(t), tour.project(t)}, "source_kind": {"issue"},
					"from": {"2026-01-01T00:00:00Z"}, "to": {"2099-01-01T00:00:00Z"},
				}, As: alice}
			},
		},
	}
}

// tourInstallation is the App installation the tour's GitHub answers every architecture
// repository with (architectureRepo).
const tourInstallation = 77

// architectureRepo tells the tour's GitHub to answer a repository of its own as one the App reads
// with Contents: read and whose main branch exists, which is what a save of an architecture source
// proves (checkArchitectureSource), answering it as owner/name. Its commits and its tree GitHub
// does not answer, so a sync of it fails, and records the failure on its row.
func (tour *routeTour) architectureRepo(t *testing.T) string {
	t.Helper()
	repo := "tour/arch-" + uuid.NewString()[:8]
	answerJSON := func(status int, body any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Errorf("encode GitHub's answer: %v", err)
			}
		}
	}
	tour.github.answer(http.MethodGet, "/repos/"+repo+"/installation", answerJSON(http.StatusOK, map[string]any{
		"id": tourInstallation, "app_slug": "dispatch-tour", "permissions": map[string]string{"contents": "read"},
	}))
	tour.github.answer(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", tourInstallation),
		answerJSON(http.StatusCreated, map[string]any{"token": "ghs_tour", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}))
	tour.github.answer(http.MethodGet, "/repos/"+repo, answerJSON(http.StatusOK, map[string]any{}))
	tour.github.answer(http.MethodGet, "/repos/"+repo+"/git/ref/heads/main", answerJSON(http.StatusOK, map[string]any{
		"ref": "refs/heads/main", "object": map[string]string{"sha": shaOne, "type": "commit"},
	}))
	return repo
}

// architectureSource makes a project of its own with one architecture source, saved through
// /api/v1 as alice on a repository of architectureRepo's, answering the project and the source's
// owner/name.
func (tour *routeTour) architectureSource(t *testing.T) (project, repo string) {
	t.Helper()
	project, repo = tour.project(t), tour.architectureRepo(t)
	saved := dispatchRequest(t, tour.handler, http.MethodPut, "/api/v1/projects/"+project+"/architecture-sources/"+repo,
		map[string]any{"branch": "main"}, "alice")
	if saved.Code != http.StatusOK {
		t.Fatalf("save the architecture source %s of %s: %d %s", repo, project, saved.Code, saved.Body.String())
	}
	return project, repo
}

// priority writes a priority of its own through /api/v1 as alice, answering its id.
func (tour *routeTour) priority(t *testing.T) string {
	t.Helper()
	id := fmt.Sprintf("tour-priority-%d", tour.keys.Add(1))
	if created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"id": id, "title": "A toured priority " + id,
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("write priority %s: %d %s", id, created.Code, created.Body.String())
	}
	return id
}

// nearCopyCheck makes an issue in a project of its own, whose create the near-copy gate answers
// and records, answering the project and the id of that record.
func (tour *routeTour) nearCopyCheck(t *testing.T) (project, id string) {
	t.Helper()
	project = tour.projectOf(t, tour.issue(t))
	if err := tour.database.Pool.QueryRow(t.Context(), `
		select id::text from near_copy_checks where project_key = $1 order by created_at, id limit 1
	`, project).Scan(&id); err != nil {
		t.Fatalf("read the near-copy check of %s's create: %v", project, err)
	}
	return project, id
}

// repository maps a repository of its own to a project of its own through /api/v1 as alice,
// answering the project and the repository's owner/name.
func (tour *routeTour) repository(t *testing.T) (project, repo string) {
	t.Helper()
	project, repo = tour.project(t), "tour/mapped-"+uuid.NewString()[:8]
	if mapped := dispatchRequest(t, tour.handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo,
		map[string]any{"project": project}, "alice"); mapped.Code != http.StatusOK {
		t.Fatalf("map %s to %s: %d %s", repo, project, mapped.Code, mapped.Body.String())
	}
	return project, repo
}

// TestV2NearCopyChecksPageAProjectsRowsWithinTheWindow holds the near-copy
// list (the plan's departure 2): GET /api/v2/projects/{key}/near-copy-checks answers the envelope
// of the project's rows alone, oldest first, from inclusive and to exclusive on created_at, paged
// by limit and offset with the total the window holds, 1,000 the default limit and the largest; a
// window whose from is not before its to is refused, and /api/v1's after cursor is no key of it.
func TestV2NearCopyChecksPageAProjectsRowsWithinTheWindow(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodGet, "/projects/{key}/near-copy-checks")
	project := tour.project(t)
	tour.nearCopyCheck(t) // another project's row, which no page of this project's holds
	start := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	var ids []string
	for index, title := range []string{
		"Astrolabe calibration drifts after a reseat",
		"Ledger reconciliation stalls on overnight batches",
		"Webhook retries flood the delivery queue",
	} {
		created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": title,
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create %q: %d %s", title, created.Code, created.Body.String())
		}
		var id string
		if err := tour.database.Pool.QueryRow(t.Context(), `
			update near_copy_checks set created_at = $3
			 where project_key = $1 and issue_key = $2
			returning id::text`, project, decodeBody[tourIssue](t, created).Key, start.Add(time.Duration(index)*time.Hour),
		).Scan(&id); err != nil {
			t.Fatalf("place the near-copy check of %q in time: %v", title, err)
		}
		ids = append(ids, id)
	}
	at := func(hours int) string { return start.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339) }
	read := func(query url.Values) page[model.NearCopyCheck] {
		t.Helper()
		response := tour.send(t, route, tourRequest{Path: map[string]string{"key": project}, Query: query, As: tourPerson("alice")}, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET with %v: %d %s", query, response.Code, response.Body.String())
		}
		return decodeBody[page[model.NearCopyCheck]](t, response)
	}
	for _, want := range []struct {
		name         string
		query        url.Values
		rows         []string
		total, limit int
	}{
		{"no window", nil, ids, 3, nearCopyChecksPageMax},
		{"from the second row's instant", url.Values{"from": {at(1)}}, ids[1:], 2, nearCopyChecksPageMax},
		{"to the third row's instant", url.Values{"to": {at(2)}}, ids[:2], 2, nearCopyChecksPageMax},
		{"between the two", url.Values{"from": {at(1)}, "to": {at(2)}}, ids[1:2], 1, nearCopyChecksPageMax},
		{"one row from the second", url.Values{"limit": {"1"}, "offset": {"1"}}, ids[1:2], 3, 1},
		{"past the last row", url.Values{"offset": {"3"}}, []string{}, 3, nearCopyChecksPageMax},
	} {
		answer := read(want.query)
		got := make([]string, 0, len(answer.Items))
		for _, row := range answer.Items {
			got = append(got, row.ID)
			if row.Project != project || row.Spec != nil {
				t.Errorf("%s: row %+v is another project's or carries its spec", want.name, row)
			}
		}
		if !slices.Equal(got, want.rows) || answer.Total != want.total || answer.Limit != want.limit {
			t.Errorf("%s answered rows %v, total %d, limit %d; want %v, %d, %d",
				want.name, got, answer.Total, answer.Limit, want.rows, want.total, want.limit)
		}
	}
	for query, code := range map[string]apicode.Code{
		"limit=1001":                            apicode.InvalidLimit,
		"from=" + at(2) + "&to=" + at(1):        apicode.InvalidQuery,
		"from=" + at(1) + "&to=" + at(1):        apicode.InvalidQuery,
		"after=" + ids[0]:                       apicode.QueryKeyUnknown,
		"project=" + project + "&limit=1":       apicode.QueryKeyUnknown,
		"from=" + url.QueryEscape("2026-03-01"): apicode.InvalidQuery,
	} {
		values, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		response := tour.send(t, route, tourRequest{Path: map[string]string{"key": project}, Query: values, As: tourPerson("alice")}, "")
		if refused, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || refused != code.Name() {
			t.Errorf("GET with %s: %d %s, want 400 %s", query, response.Code, response.Body.String(), code.Name())
		}
	}
}

// TestV2ArchitectureSyncAnswersAnAccessFailureOnTheSourcesRow holds the route map's J15: POST
// /api/v2/projects/{key}/architecture-syncs naming one source whose branch GitHub does
// not answer imports it and answers 200 with the failure on that row's last_error, where /api/v1's
// sync of that one source answers 409 SOURCE_ACCESS; a repos list naming a source the project does
// not have is 404 SOURCE_NOT_FOUND, and the named source beside it is not imported.
func TestV2ArchitectureSyncAnswersAnAccessFailureOnTheSourcesRow(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodPost, "/projects/{key}/architecture-syncs")
	project, repo := tour.architectureSource(t)
	alice := tourPerson("alice")
	if v1 := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/projects/"+project+"/architecture-sources/"+repo+"/sync", nil, "alice"); v1.Code != http.StatusConflict {
		t.Fatalf("/api/v1's sync of the one source answered %d %s, want its 409 SOURCE_ACCESS", v1.Code, v1.Body.String())
	}
	var lastSync *time.Time
	if err := tour.database.Pool.QueryRow(t.Context(), `select last_sync_at from architecture_sources where project_key = $1`, project).Scan(&lastSync); err != nil {
		t.Fatalf("read the source's last sync: %v", err)
	}

	refused := tour.send(t, route, tourRequest{
		Path: map[string]string{"key": project}, Body: map[string]any{"repos": []string{repo, "tour/no-such-source"}}, As: alice,
	}, "")
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusNotFound || code != apicode.SourceNotFound.Name() {
		t.Fatalf("a sync naming a source the project lacks answered %d %s, want 404 SOURCE_NOT_FOUND", refused.Code, refused.Body.String())
	}
	var after *time.Time
	if err := tour.database.Pool.QueryRow(t.Context(), `select last_sync_at from architecture_sources where project_key = $1`, project).Scan(&after); err != nil {
		t.Fatalf("read the source's last sync: %v", err)
	}
	if (lastSync == nil) != (after == nil) || (lastSync != nil && !lastSync.Equal(*after)) {
		t.Fatalf("the refused sync imported the source beside the missing one: last_sync_at %v, then %v", lastSync, after)
	}

	synced := tour.send(t, route, tourRequest{Path: map[string]string{"key": project}, Body: map[string]any{"repos": []string{repo}}, As: alice}, "")
	if synced.Code != http.StatusOK {
		t.Fatalf("the sync of the one source answered %d %s, want 200", synced.Code, synced.Body.String())
	}
	answer := decodeBody[page[model.ArchitectureSource]](t, synced)
	if answer.Total != 1 || len(answer.Items) != 1 || answer.Items[0].Repo != repo || answer.Items[0].LastError == nil ||
		!strings.Contains(*answer.Items[0].LastError, "main") {
		t.Fatalf("the sync of the one source answered %s, want its one row with the branch's failure in last_error", synced.Body.String())
	}
}

// TestV2PrioritiesAtLimitZeroAnswerTheirCountsAlone: GET /api/v2/priorities?limit=0 is /api/v1's
// summary=true, the navigation rail's read: no priority, the total of the live ones, and the two
// fault counts with no counts by project or priority and no rows, while include=fault_issues adds
// the rows at limit 0 too and a page of one answers one priority of the same total.
func TestV2PrioritiesAtLimitZeroAnswerTheirCountsAlone(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodGet, "/priorities")
	tour.priority(t)
	tour.priority(t)
	read := func(query url.Values) map[string]json.RawMessage {
		t.Helper()
		response := tour.send(t, route, tourRequest{Query: query, As: tourPerson("alice")}, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET with %v: %d %s", query, response.Code, response.Body.String())
		}
		return decodeBody[map[string]json.RawMessage](t, response)
	}
	members := func(raw json.RawMessage) []string {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatalf("read a fault %s: %v", raw, err)
		}
		names := make([]string, 0, len(object))
		for name := range object {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	for _, want := range []struct {
		name         string
		query        url.Values
		items, limit string
		unplaced     []string
		parked       []string
	}{
		{"the counts alone", url.Values{"limit": {"0"}}, "[]", "0", []string{"count"}, []string{"count"}},
		{"the counts and the rows", url.Values{"limit": {"0"}, "include": {"fault_issues"}}, "[]", "0",
			[]string{"by_project", "count", "issues"}, []string{"by_priority", "count", "issues"}},
		{"a page of one", url.Values{"limit": {"1"}}, "", "1", []string{"by_project", "count"}, []string{"by_priority", "count"}},
	} {
		answer := read(want.query)
		var items []model.PriorityRow
		if err := json.Unmarshal(answer["items"], &items); err != nil {
			t.Fatalf("%s: read the items %s: %v", want.name, answer["items"], err)
		}
		switch {
		case want.items == "[]" && string(answer["items"]) != "[]":
			t.Errorf("%s answered items %s, want none", want.name, answer["items"])
		case want.items == "" && len(items) != 1:
			t.Errorf("%s answered %d priorities, want one", want.name, len(items))
		}
		if string(answer["total"]) != "2" || string(answer["limit"]) != want.limit {
			t.Errorf("%s answered total %s and limit %s, want the 2 live priorities at limit %s",
				want.name, answer["total"], answer["limit"], want.limit)
		}
		if got := members(answer["work_under_no_priority"]); !slices.Equal(got, want.unplaced) {
			t.Errorf("%s: work_under_no_priority holds %v, want %v", want.name, got, want.unplaced)
		}
		if got := members(answer["priority_work_parked"]); !slices.Equal(got, want.parked) {
			t.Errorf("%s: priority_work_parked holds %v, want %v", want.name, got, want.parked)
		}
	}
}
