package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// proxyShows answers whether the GitHub proxy shows login repository: its own read
// (routes' repositoryVisibleQuery, refs.RepositoryClause) for the viewer it resolves a cookie to
// (access.Resolve of model.BrowserActor).
func proxyShows(t *testing.T, database *store.Store, login, repository string) bool {
	t.Helper()
	viewer, err := access.Resolve(context.Background(), database.Pool, model.BrowserActor(login))
	if err != nil {
		t.Fatalf("resolve what %s sees: %v", login, err)
	}
	var shown bool
	if err := database.Pool.QueryRow(context.Background(), `select `+refs.RepositoryClause("$2", "$1"),
		refs.VisibilityArg(model.Actor{Viewer: &viewer}), repository).Scan(&shown); err != nil {
		t.Fatalf("read whether %s sees %s: %v", login, repository, err)
	}
	return shown
}
