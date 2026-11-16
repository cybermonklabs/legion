package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// listCIReconcilePasses answers the CI reconcile passes of the last day, newest first.
//
// The pass logs one INFO line and the container log rotates - the production pod's reached back
// 45 minutes - so the counts also live in ci_reconcile_passes, and this is how anyone reads them
// without a shell on the pod. It reads rows and nothing else: a row with no end time is read as a
// pass still running only while its process is live, the lease is held and it is the newest row
// (cireconcile.RecentPasses), and this route decides none of that itself.
//
// A pass answers only to a caller that sees the repository it left its cursor in
// (access.RepositoryClause): one mapped to a project the caller sees, or, for an open viewer, one
// no project maps. A pass with no cursor names no repository, so it answers an open viewer alone.
// The rows are cut after RecentPasses has read them all, because which row is the newest is
// decided over every pass, not over the ones a caller sees.
func (s *server) listCIReconcilePasses(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	passes, err := cireconcile.RecentPasses(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repositories := make([]string, len(passes))
	for index, pass := range passes {
		if pass.Cursor != nil {
			repositories[index] = pass.Cursor.Repository
		}
	}
	seen, err := s.visibleRepositories(r.Context(), refs.VisibilityArg(reader), repositories)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	visible := passes[:0]
	for index, pass := range passes {
		if seen[repositories[index]] {
			visible = append(visible, pass)
		}
	}
	WriteJSON(w, http.StatusOK, visible)
}

// visibleRepositories is which of repositories the reader whose visibility argument is viewer
// sees (access.RepositoryClause). The empty name is a repository no project maps.
func (s *server) visibleRepositories(ctx context.Context, viewer any, repositories []string) (map[string]bool, error) {
	seen := map[string]bool{}
	if len(repositories) == 0 {
		return seen, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select candidate.repo from unnest($1::text[]) candidate(repo)
		where `+access.RepositoryClause("candidate.repo", "$2"), slices.Compact(slices.Sorted(slices.Values(repositories))), viewer)
	if err != nil {
		return nil, fmt.Errorf("read which repositories the caller sees: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, fmt.Errorf("scan a repository the caller sees: %w", err)
		}
		seen[repository] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read which repositories the caller sees: %w", err)
	}
	return seen, nil
}
