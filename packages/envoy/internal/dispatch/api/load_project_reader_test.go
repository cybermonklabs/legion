package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// loadProject reads a project as its reader reads it (access.ProjectClause): a reader that sees
// the project, the server's own viewer among them, reads the row, and one that does not answers
// 404 PROJECT_NOT_FOUND, as a key no project holds answers. Control: a key no project holds
// answers the same 404 to the server's viewer, so the refusal below is the clause and not a
// missing row.
func TestLoadProjectAnswersAProjectItsReaderDoesNotSeeAsMissing(t *testing.T) {
	t.Parallel()
	handler, _, deps := newTestServer(t, testServerOptions{})
	s := directServer(deps)
	ctx := context.Background()
	createTestIssue(t, handler, "CORE", "A project to read")
	pool := deps.Store.Pool

	seer := model.Actor{Kind: "user", ID: "alice", Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"CORE"}}}
	stranger := model.Actor{Kind: "user", ID: "bob", Viewer: &model.Viewer{Person: "bob", Sees: []string{"OTHER"}}}

	for name, reader := range map[string]model.Actor{"the server's viewer": everyReader, "a reader that sees CORE": seer} {
		project, err := s.loadProject(ctx, pool, reader, "CORE")
		if err != nil || project.Key != "CORE" {
			t.Errorf("loadProject of CORE for %s = %q, %v; want CORE", name, project.Key, err)
		}
	}
	requireProjectNotFound := func(name string, err error) {
		t.Helper()
		var refused *apiError
		if !errors.As(err, &refused) || refused.status != http.StatusNotFound || refused.code != "PROJECT_NOT_FOUND" {
			t.Errorf("%s = %v, want 404 PROJECT_NOT_FOUND", name, err)
		}
	}
	_, err := s.loadProject(ctx, pool, stranger, "CORE")
	requireProjectNotFound("loadProject of CORE for a reader that does not see it", err)
	_, err = s.loadProject(ctx, pool, model.Actor{Kind: "user", ID: "carol"}, "CORE")
	requireProjectNotFound("loadProject of CORE for a reader with no viewer (the closed one)", err)
	_, err = s.loadProject(ctx, pool, everyReader, "NOPE")
	requireProjectNotFound("loadProject of a key no project holds", err)
}
