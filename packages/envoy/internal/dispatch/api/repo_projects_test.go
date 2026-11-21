package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type repoProjectResponse struct {
	Repo    string `json:"repo"`
	Project string `json:"project"`
	Default bool   `json:"default"`
}

// projectEvents reads the payloads of the project's events of one type, oldest first.
func projectEvents(t *testing.T, database *store.Store, project, eventType string) []map[string]any {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select payload from events where project_key = $1 and type = $2 order by id
	`, project, eventType)
	if err != nil {
		t.Fatalf("read %s events of %s: %v", eventType, project, err)
	}
	defer rows.Close()
	var payloads []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan %s event: %v", eventType, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s event: %v", eventType, err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s events of %s: %v", eventType, project, err)
	}
	return payloads
}

// A project's default is set with the mapping and moves with one write: setting a second mapping
// default demotes the first in the same transaction, and both rows' events say so. A mapping moved
// to another project emits settings.repo_project.updated on the old project and the new one.
func TestRepoProjectDefaultMovesAndEventsReachBothProjects(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	for _, project := range []map[string]string{{"key": "CORE", "name": "Core"}, {"key": "OTHER", "name": "Other"}} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	first := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	if first.Code != http.StatusOK || !decodeBody[repoProjectResponse](t, first).Default {
		t.Fatalf("map acme/widgets as default: status=%d body=%s", first.Code, first.Body.String())
	}
	second := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE", "default": true}, "alice")
	if second.Code != http.StatusOK {
		t.Fatalf("map acme/gadgets as default: status=%d body=%s", second.Code, second.Body.String())
	}
	listed := decodeBody[[]repoProjectResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice"))
	defaults := map[string]bool{}
	for _, mapping := range listed {
		defaults[mapping.Repo] = mapping.Default
	}
	if defaults["acme/widgets"] || !defaults["acme/gadgets"] {
		t.Fatalf("defaults after the move = %v, want gadgets alone", defaults)
	}
	events := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if len(events) != 3 { // widgets set, widgets demoted, gadgets set
		t.Fatalf("CORE saw %d mapping events, want 3: %v", len(events), events)
	}
	if demoted := events[1]["mapping"].(map[string]any); demoted["repo"] != "acme/widgets" || demoted["default"] != false || events[1]["deleted"] != false {
		t.Fatalf("second CORE event = %v, want acme/widgets demoted", events[1])
	}
	// A write that leaves default out keeps the mapping's default while its project is unchanged.
	if kept := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE"}, "alice"); kept.Code != http.StatusOK || !decodeBody[repoProjectResponse](t, kept).Default {
		t.Fatalf("re-map acme/gadgets without default: status=%d body=%s, want it still the default", kept.Code, kept.Body.String())
	}
	moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "OTHER"}, "alice")
	if moved.Code != http.StatusOK || decodeBody[repoProjectResponse](t, moved).Default {
		t.Fatalf("move acme/gadgets to OTHER: status=%d body=%s, want it moved and not OTHER's default", moved.Code, moved.Body.String())
	}
	if got := len(projectEvents(t, database, "OTHER", "settings.repo_project.updated")); got != 1 {
		t.Fatalf("OTHER saw %d mapping events after the move, want 1", got)
	}
	core := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if len(core) != 5 { // the three above, the re-map, and the leaving mapping
		t.Fatalf("CORE saw %d mapping events after the move, want 5: %v", len(core), core)
	}
	if leaving := core[4]["mapping"].(map[string]any); leaving["repo"] != "acme/gadgets" || leaving["project"] != "CORE" || core[4]["deleted"] != true {
		t.Fatalf("CORE's last event = %v, want acme/gadgets leaving CORE, deleted:true", core[4])
	}
}

// A mapping an issue names holds nothing in place: it moves to another project and it is deleted,
// and the issue keeps naming the repository through both, since an issue's own repository needs
// no mapping (INFRA-2543 R3).
func TestRepoProjectAnIssueNamesIsMovedAndDeleted(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []map[string]string{{"key": "CORE", "name": "Core"}, {"key": "OTHER", "name": "Other"}} {
		dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice")
	}
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "One", "repo": "acme/widgets"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[model.Issue](t, created).Key
	moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "OTHER"}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("move a mapping %s names: status=%d body=%s, want 200", key, moved.Code, moved.Body.String())
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")); issue.Repo == nil || *issue.Repo != "acme/widgets" {
		t.Fatalf("%s's repo after the move = %v, want the acme/widgets it names, now OTHER's mapping", key, issue.Repo)
	}
	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/acme/widgets", nil, "alice")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete a mapping %s names: status=%d body=%s, want 204", key, deleted.Code, deleted.Body.String())
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	if issue.Repo == nil || *issue.Repo != "acme/widgets" {
		t.Fatalf("%s's repo after the delete = %v, want the acme/widgets it names, now mapped by no project", key, issue.Repo)
	}
	if issue.ResolvedRepo == nil || *issue.ResolvedRepo != "acme/widgets" {
		t.Fatalf("%s's resolved repo after the delete = %v, want its own acme/widgets", key, issue.ResolvedRepo)
	}
}

func TestRepoProjectSettingsUpsertListAndDelete(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice")
	if mapped.Code != http.StatusOK {
		t.Fatalf("upsert repository project: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	if got := decodeBody[repoProjectResponse](t, mapped); got != (repoProjectResponse{Repo: "owner/repo", Project: "CORE"}) {
		t.Fatalf("upserted mapping: got %#v", got)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list repository projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if got := decodeBody[[]repoProjectResponse](t, listed); len(got) != 1 || got[0] != (repoProjectResponse{Repo: "owner/repo", Project: "CORE"}) {
		t.Fatalf("listed mappings: got %#v", got)
	}

	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/owner/repo", nil, "alice")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete repository project: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	listed = dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
	if got := decodeBody[[]repoProjectResponse](t, listed); len(got) != 0 {
		t.Fatalf("mappings after delete: got %#v", got)
	}

	missingProject := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/missing", map[string]string{
		"project": "MISSING",
	}, "alice")
	if missingProject.Code != http.StatusNotFound {
		t.Fatalf("upsert missing project: status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}
}

func TestAgentListsRepoProjectSettings(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("seed repository project mapping: status=%d body=%s", response.Code, response.Body.String())
	}

	response := agentRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("agent list repository projects: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := decodeBody[[]repoProjectResponse](t, response); len(got) != 1 || got[0] != (repoProjectResponse{Repo: "owner/repo", Project: "CORE"}) {
		t.Fatalf("agent listed mappings: got %#v", got)
	}
}

func TestAgentPutsRepoProjectSettingsWithItsActor(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	response := agentRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]any{
		"project": "CORE",
		"actor":   map[string]string{"kind": "session", "id": "agent-repo-project-put"},
	}, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("agent upsert repository project: status=%d body=%s", response.Code, response.Body.String())
	}
	var actorID string
	if err := database.Pool.QueryRow(t.Context(), `
		select created_by->>'id' from repo_projects where repo = 'owner/repo'
	`).Scan(&actorID); err != nil {
		t.Fatalf("read repository project actor: %v", err)
	}
	if actorID != "agent-repo-project-put" {
		t.Fatalf("repository project actor = %q, want agent-repo-project-put", actorID)
	}
}

func TestAgentDeletesRepoProjectSettingsWithItsActor(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("seed repository project mapping: status=%d body=%s", response.Code, response.Body.String())
	}

	response := agentRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/owner/repo", map[string]any{
		"actor": map[string]string{"kind": "session", "id": "agent-repo-project-delete"},
	}, "agent-token")
	if response.Code != http.StatusNoContent {
		t.Fatalf("agent delete repository project: status=%d body=%s", response.Code, response.Body.String())
	}
	var actorID string
	if err := database.Pool.QueryRow(t.Context(), `
		select actor->>'id'
		from events
		where type = 'settings.repo_project.updated' and payload->>'deleted' = 'true'
		order by id desc
		limit 1
	`).Scan(&actorID); err != nil {
		t.Fatalf("read repository project deletion actor: %v", err)
	}
	if actorID != "agent-repo-project-delete" {
		t.Fatalf("repository project deletion actor = %q, want agent-repo-project-delete", actorID)
	}
}

func TestRepoProjectMappingCanonicalizesRepositoryVariants(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithDefaultProject(t)
	for _, project := range []map[string]string{
		{"key": "CORE", "name": "Core"},
		{"key": "DEFAULT", "name": "Default"},
	} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %q: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}
	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/Owner/Repo.git", map[string]string{"project": "CORE"}, "alice")
	if mapped.Code != http.StatusOK || decodeBody[repoProjectResponse](t, mapped).Repo != "owner/repo" {
		t.Fatalf("map canonical repository: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	for number, ref := range []string{"Owner/Repo#1", "owner/repo.git#2"} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"external": ref}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create external issue %q: status=%d body=%s", ref, response.Code, response.Body.String())
		}
		if issue := decodeBody[struct {
			Project string `json:"project"`
		}](t, response); issue.Project != "CORE" {
			t.Fatalf("external issue %d project: got %q, want CORE", number, issue.Project)
		}
	}
	// A DELETE names only the mapping stored under its path's own spelling
	// (TestRepoProjectDeleteRemovesOnlyTheMappingItsPathSpells), so a variant the PUT and the
	// references above canonicalise removes nothing, and the canonical name removes the mapping.
	for _, removal := range []struct {
		path string
		left int
	}{{"OWNER/REPO.git", 1}, {"owner/repo", 0}} {
		deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/"+removal.path, nil, "alice")
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s: status=%d body=%s, want 204", removal.path, deleted.Code, deleted.Body.String())
		}
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
		if got := decodeBody[[]repoProjectResponse](t, listed); len(got) != removal.left {
			t.Fatalf("after DELETE %s the mappings are %#v, want %d", removal.path, got, removal.left)
		}
	}
	if got := refs.ExternalRepo(" Owner ", " Repo.git "); got != "owner/repo" {
		t.Fatalf("canonical repository whitespace: got %q", got)
	}
}

// A DELETE removes only the mapping stored under its path's own spelling, compared byte for byte
// on the table's primary key. A path the canonical reading (lower-cased, one trailing `.git`
// trimmed) reads as itself, `acme/x`, is the canonical row's own spelling; any other path names
// the mapping stored under its exact spelling alone, never the row that reading names. A PUT of
// `acme/x.git.git` stores `acme/x.git`, here beside `acme/x`: the DELETE of `acme/x.git.git`, a
// spelling no mapping is stored under, removes nothing; that of `acme/x.git` removes
// `acme/x.git`; that of `Acme/X.git`, which the reading reads as `acme/x`, removes nothing; and
// that of `acme/x` removes `acme/x`. Names only direct SQL stores are removed by their own
// spelling too: `Acme/X`, whose DELETE sent again leaves `acme/x` beside it, and `acme/a b`,
// which the reading refuses. A path that names no mapping is 204 where the reading takes it and
// 400 where it refuses it.
//
// The negative control is the DELETE reaching, where its spelling names no mapping, the row the
// canonical reading names, as it did: its DELETE of `acme/x.git.git` removes `acme/x.git`.
func TestRepoProjectDeleteRemovesOnlyTheMappingItsPathSpells(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	mapPaths := func(paths ...string) {
		t.Helper()
		for _, path := range paths {
			if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+path,
				map[string]string{"project": "CORE"}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("map %s: status=%d body=%s", path, response.Code, response.Body.String())
			}
		}
	}
	// stored is every mapping's name the listing answers, in byte order.
	stored := func() []string {
		t.Helper()
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list the mappings: status=%d body=%s", listed.Code, listed.Body.String())
		}
		var repos []string
		for _, mapping := range decodeBody[[]repoProjectResponse](t, listed) {
			repos = append(repos, mapping.Repo)
		}
		slices.Sort(repos)
		return repos
	}
	deleteLeaving := func(path string, status int, want ...string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/"+path, nil, "alice")
		if response.Code != status {
			t.Fatalf("DELETE %s: status=%d body=%s, want %d", path, response.Code, response.Body.String(), status)
		}
		if got := stored(); !slices.Equal(got, want) {
			t.Fatalf("after DELETE %s the mappings are %q, want %q", path, got, want)
		}
	}

	mapPaths("acme/x.git.git", "acme/x")
	if got := stored(); !slices.Equal(got, []string{"acme/x", "acme/x.git"}) {
		t.Fatalf("the PUTs stored %q, want acme/x and acme/x.git", got)
	}
	deleteLeaving("acme/x.git.git", http.StatusNoContent, "acme/x", "acme/x.git")
	deleteLeaving("acme/x.git", http.StatusNoContent, "acme/x")
	events := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if removed := events[len(events)-1]; removed["deleted"] != true || removed["mapping"].(map[string]any)["repo"] != "acme/x.git" {
		t.Fatalf("the DELETE's event = %v, want acme/x.git removed, deleted:true", removed)
	}
	deleteLeaving("Acme/X.git", http.StatusNoContent, "acme/x")
	deleteLeaving("acme/x", http.StatusNoContent)

	mapPaths("acme/x")
	for _, name := range []string{"Acme/X", "acme/a b"} {
		if _, err := database.Pool.Exec(t.Context(), `
			insert into repo_projects (repo, project, created_by) values ($1, 'CORE', '{"kind":"system","id":"test"}'::jsonb)
		`, name); err != nil {
			t.Fatalf("store the mapping %q by SQL: %v", name, err)
		}
	}
	deleteLeaving("Acme/X", http.StatusNoContent, "acme/a b", "acme/x")
	deleteLeaving("Acme/X", http.StatusNoContent, "acme/a b", "acme/x")
	deleteLeaving("acme/a%20b", http.StatusNoContent, "acme/x")
	deleteLeaving("acme/nothing", http.StatusNoContent, "acme/x")
	deleteLeaving("acme/no%20thing", http.StatusBadRequest, "acme/x")
	// Space before the owner is trimmed and space after the name is not, as the route read a path
	// when it read it as the reference `<path>#1`.
	deleteLeaving("%20acme/nothing", http.StatusNoContent, "acme/x")
	deleteLeaving("acme/nothing%20", http.StatusBadRequest, "acme/x")
}

// A DELETE is the same however often it is sent: the dashboard sends a DELETE again, with no key,
// when an earlier attempt's answer did not reach it and that attempt may have landed
// (packages/dispatch/web/src/features/writes/keyed-write.ts), and two tabs can send the same
// removal at once. So the DELETE of `acme/x.git` that removed the mapping stored under that
// spelling removes nothing more when it is sent again, and the mapping of `acme/x` beside it,
// which nobody asked to remove, stays.
func TestRepoProjectDeleteSentAgainRemovesNoOtherMapping(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, path := range []string{"acme/x.git.git", "acme/x"} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+path,
			map[string]string{"project": "CORE"}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	for attempt := 1; attempt <= 2; attempt++ {
		response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/acme/x.git", nil, "alice")
		if response.Code != http.StatusNoContent {
			t.Fatalf("DELETE acme/x.git, attempt %d: status=%d body=%s, want 204", attempt, response.Code, response.Body.String())
		}
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
		var repos []string
		for _, mapping := range decodeBody[[]repoProjectResponse](t, listed) {
			repos = append(repos, mapping.Repo)
		}
		if !slices.Equal(repos, []string{"acme/x"}) {
			t.Fatalf("after DELETE acme/x.git, attempt %d, the mappings are %q, want acme/x alone", attempt, repos)
		}
	}
}

func TestExternalIssueUsesPersistedRepoProject(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithDefaultProject(t)
	for _, project := range []map[string]string{
		{"key": "CORE", "name": "Core"},
		{"key": "DEFAULT", "name": "Default"},
	} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %q: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}

	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/mapped/repository", map[string]string{
		"project": "CORE",
	}, "alice")
	if mapped.Code != http.StatusOK {
		t.Fatalf("upsert repository project: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	mismatch := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "mapped/repository#1",
		"project":  "DEFAULT",
	}, "alice")
	if mismatch.Code != http.StatusBadRequest || !strings.Contains(mismatch.Body.String(), `"code":"EXTERNAL_PROJECT_MISMATCH"`) {
		t.Fatalf("reject mismatched external project: status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}

	linked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Linked pull request",
	}, "alice")
	if linked.Code != http.StatusCreated {
		t.Fatalf("create linked issue: status=%d body=%s", linked.Code, linked.Body.String())
	}
	linkedKey := decodeBody[struct {
		Key string `json:"key"`
	}](t, linked).Key
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+linkedKey, map[string]any{
		"external_links": []map[string]string{{
			"url": "https://github.com/mapped/repository/pull/2",
		}},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("link pull request: status=%d body=%s", response.Code, response.Body.String())
	}
	mismatchedLinked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "mapped/repository#2",
		"project":  "DEFAULT",
	}, "alice")
	if mismatchedLinked.Code != http.StatusBadRequest ||
		!strings.Contains(mismatchedLinked.Body.String(), `"code":"EXTERNAL_PROJECT_MISMATCH"`) {
		t.Fatalf(
			"reject mismatched linked external project: status=%d body=%s",
			mismatchedLinked.Code,
			mismatchedLinked.Body.String(),
		)
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "mapped/repository#1",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create mapped external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key    string   `json:"key"`
		Labels []string `json:"labels"`
	}](t, created)
	if issue.Key != "CORE-2" || len(issue.Labels) != 0 {
		t.Fatalf("persisted mapping external issue: got %#v", issue)
	}
}

// renamedRepoProject reads the pair a mapping names beside its repository and project;
// repoProjectResponse above leaves it out, so the comparisons there stay as they are.
type renamedRepoProject struct {
	Repo      string  `json:"repo"`
	Project   string  `json:"project"`
	RenamedTo *string `json:"renamed_to"`
}

// A repository renamed to another: the pair is written through the mapping route,
// canonicalised as the route's own path is, answered by the listing, kept by a write that does
// not name it — the page sends {project, default} and nothing else — and cleared by "". Every
// pair that would put a canonical name two steps away, or point a repository at itself, is
// refused, which is what lets refs.CanonicalRepo read one row and stop.
func TestRepoProjectRenamedToIsSetKeptClearedAndHeldToOneStep(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	put := func(repo string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, body, "alice")
	}
	renamedTo := func(repo string) *string {
		t.Helper()
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list the mappings: status=%d body=%s", listed.Code, listed.Body.String())
		}
		for _, mapping := range decodeBody[[]renamedRepoProject](t, listed) {
			if mapping.Repo == repo {
				return mapping.RenamedTo
			}
		}
		t.Fatalf("the listing holds no mapping of %s", repo)
		return nil
	}

	if response := put("acme/gadgets", map[string]any{"project": "CORE", "default": true}); response.Code != http.StatusOK {
		t.Fatalf("map acme/gadgets: status=%d body=%s", response.Code, response.Body.String())
	}
	// The pair is canonicalised the way this route canonicalises its own path.
	response := put("oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": " Acme/Gadgets.git "})
	if response.Code != http.StatusOK {
		t.Fatalf("rename oldco/gadgets: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := decodeBody[renamedRepoProject](t, response).RenamedTo; got == nil || *got != "acme/gadgets" {
		t.Fatalf("the write answered renamed_to %v, want acme/gadgets", got)
	}
	if got := renamedTo("oldco/gadgets"); got == nil || *got != "acme/gadgets" {
		t.Fatalf("the listing answers renamed_to %v for oldco/gadgets, want acme/gadgets", got)
	}
	if got := renamedTo("acme/gadgets"); got != nil {
		t.Fatalf("the listing answers renamed_to %q for acme/gadgets, want null: it was never renamed", *got)
	}

	// A write that does not name the pair leaves it alone, as it leaves the default alone.
	if response := put("oldco/gadgets", map[string]any{"project": "CORE"}); response.Code != http.StatusOK {
		t.Fatalf("re-map oldco/gadgets: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := renamedTo("oldco/gadgets"); got == nil || *got != "acme/gadgets" {
		t.Fatalf("renamed_to after a write that did not name it = %v, want acme/gadgets kept", got)
	}

	for _, refused := range []struct {
		what string
		repo string
		body map[string]any
	}{
		{"a repository renamed to itself", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "oldco/gadgets"}},
		{"a pair that is not owner/name", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "gadgets"}},
		{"a target that is itself renamed", "ancient/gadgets", map[string]any{"project": "CORE", "renamed_to": "oldco/gadgets"}},
		{"a rename of what another mapping points at", "acme/gadgets", map[string]any{"project": "CORE", "renamed_to": "newer/gadgets"}},
		// The pair is the name GitHub is asked about, its halves put into GitHub's paths as they
		// are, so it is held to the names GitHub allows, as a source repository is.
		{"a name of ..", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "acme/.."}},
		{"a name of .", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "acme/."}},
		{"a name that is .. once .git is trimmed", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "acme/...git"}},
		// The pair is held to the rule as it is stored: canonicalising trims one `.git`, and
		// every later read trims another.
		{"a name that is empty once .git is trimmed twice", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "acme/.git.git"}},
		{"a name that is .. once .git is trimmed twice", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "acme/...git.git"}},
		{"an owner of ..", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "../gadgets"}},
		{"a name holding a character GitHub does not allow", "oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": "acme/gad?gets"}},
	} {
		response := put(refused.repo, refused.body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", refused.what, response.Code, response.Body.String())
		}
		if code, _ := refusalOf(t, response); code != "INVALID_REPO_PROJECT" {
			t.Fatalf("%s: code %s, want INVALID_REPO_PROJECT (body %s)", refused.what, code, response.Body.String())
		}
	}
	if got := renamedTo("oldco/gadgets"); got == nil || *got != "acme/gadgets" {
		t.Fatalf("renamed_to after the refusals = %v, want acme/gadgets untouched", got)
	}

	// "" clears the pair, and the repository is its own canonical name again.
	if response := put("oldco/gadgets", map[string]any{"project": "CORE", "renamed_to": ""}); response.Code != http.StatusOK {
		t.Fatalf("clear the pair: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := renamedTo("oldco/gadgets"); got != nil {
		t.Fatalf("renamed_to after clearing = %q, want null", *got)
	}
}

// A mapping's own repository is held to the rule its renamed_to is: the path is a person's text,
// and the stored name becomes the repository of every issue of the project that names none, whose
// halves the linked-repository reads and the App's installation read put into GitHub paths as
// they are. `...git` and `.git` are no dot segments, so the mux does not clean them, and the
// trailing `.git` that refs.ExternalRepo trims leaves `..` or an empty name behind.
func TestRepoProjectMappingPathIsHeldToTheNamesGitHubAllows(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, refused := range []struct {
		what string
		path string
	}{
		{"a name that is .. once .git is trimmed", "acme/...git"},
		{"a name that is empty once .git is trimmed", "acme/.git"},
	} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+refused.path,
			map[string]any{"project": "CORE", "default": true}, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", refused.what, response.Code, response.Body.String())
		}
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the mappings: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, mapping := range decodeBody[[]repoProjectResponse](t, listed) {
		if mapping.Repo == "acme/.." || mapping.Repo == "acme/" {
			t.Fatalf("a refused write stored a mapping of %q", mapping.Repo)
		}
	}
}

// A mapping's path is held to the rule in the form it is stored: canonicalising trims one `.git`,
// and every later read of the stored name trims another, so `acme/.git.git` would be stored as
// `acme/.git`, a name no later read accepts.
func TestRepoProjectMappingPathIsHeldToTheNameAsItIsStored(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, refused := range []struct {
		what string
		path string
	}{
		{"a name that is empty once .git is trimmed twice", "acme/.git.git"},
		{"a name that is .. once .git is trimmed twice", "acme/...git.git"},
	} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+refused.path,
			map[string]any{"project": "CORE", "default": true}, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", refused.what, response.Code, response.Body.String())
		}
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the mappings: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, mapping := range decodeBody[[]repoProjectResponse](t, listed) {
		if mapping.Repo == "acme/.git" || mapping.Repo == "acme/...git" {
			t.Fatalf("a refused write stored a mapping of %q", mapping.Repo)
		}
	}
}

// GitHub allows a repository name of up to 100 characters, and a mapping path may carry the
// trailing `.git` a clone URL does, which refs.ExternalRepo trims before the name is stored. So the
// bound is the stored name's: a name of exactly 100 characters written with `.git` is that
// repository, mapped under its 100 characters, and one character more is refused however it is
// written.
func TestRepoProjectMappingPathBoundsTheNameAsItIsStored(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	longest := strings.Repeat("a", 100)
	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/"+longest+".git",
		map[string]any{"project": "CORE"}, "alice")
	if mapped.Code != http.StatusOK {
		t.Fatalf("a 100-character name written with .git: status=%d body=%s, want 200", mapped.Code, mapped.Body.String())
	}
	if got := decodeBody[repoProjectResponse](t, mapped).Repo; got != "acme/"+longest {
		t.Fatalf("a 100-character name written with .git is mapped as %q, want acme/%s", got, longest)
	}
	for _, path := range []string{"acme/" + longest + "a", "acme/" + longest + "a.git"} {
		refused := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+path,
			map[string]any{"project": "CORE"}, "alice")
		if refused.Code != http.StatusBadRequest {
			t.Fatalf("a 101-character name (%d characters written): status=%d body=%s, want 400",
				len(path)-len("acme/"), refused.Code, refused.Body.String())
		}
	}
}
