package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// GET /api/v1/settings/repo-projects answers the stored rows of repo_projects, by repository, to
// a person and to an agent. No route writes the rows now, so the test stores them by SQL, as the
// last release left them: a project's default, a renamed repository and a plain row.
func TestRepoProjectsReadAnswersTheStoredRows(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := database.Pool.Exec(t.Context(), `
		insert into repo_projects (repo, project, is_default, renamed_to, created_by, created_at) values
			('acme/widgets', 'CORE', true, null, '{"kind":"user","id":"alice"}', '2026-11-01T00:00:00Z'),
			('oldco/widgets', 'CORE', false, 'acme/widgets', '{"kind":"session","id":"agent-1"}', '2026-11-02T00:00:00Z'),
			('acme/gadgets', 'CORE', false, null, '{"kind":"user","id":"bob"}', '2026-11-03T00:00:00Z')
	`); err != nil {
		t.Fatalf("store the rows: %v", err)
	}
	renamedTo := "acme/widgets"
	want := []model.RepoProject{
		{Repo: "acme/gadgets", Project: "CORE", CreatedBy: model.Actor{Kind: "user", ID: "bob"}, CreatedAt: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		{Repo: "acme/widgets", Project: "CORE", Default: true, CreatedBy: model.Actor{Kind: "user", ID: "alice"}, CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{Repo: "oldco/widgets", Project: "CORE", RenamedTo: &renamedTo, CreatedBy: model.Actor{Kind: "session", ID: "agent-1"}, CreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
	}

	for _, read := range []struct {
		who      string
		response *httptest.ResponseRecorder
	}{
		{"a person", dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")},
		{"an agent", agentRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "agent-token")},
	} {
		if read.response.Code != http.StatusOK {
			t.Fatalf("%s reads the rows: status=%d body=%s", read.who, read.response.Code, read.response.Body.String())
		}
		got := decodeBody[[]model.RepoProject](t, read.response)
		if len(got) != len(want) {
			t.Fatalf("%s reads %d rows, want %d: %s", read.who, len(got), len(want), read.response.Body.String())
		}
		for index, row := range got {
			expected := want[index]
			if row.Repo != expected.Repo || row.Project != expected.Project || row.Default != expected.Default ||
				(row.RenamedTo == nil) != (expected.RenamedTo == nil) ||
				(row.RenamedTo != nil && *row.RenamedTo != *expected.RenamedTo) ||
				row.CreatedBy.Kind != expected.CreatedBy.Kind || row.CreatedBy.ID != expected.CreatedBy.ID ||
				!row.CreatedAt.Equal(expected.CreatedAt) {
				t.Errorf("%s reads row %d as %+v, want %+v", read.who, index, row, expected)
			}
		}
	}
}
