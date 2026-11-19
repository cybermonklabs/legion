package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// visibleRepositories is which of repositories the reader whose visibility argument is viewer
// sees (access.RepositoryClause). The empty name is a repository no project maps. Its one caller is
// getAppInstallation's gate, which GH-1 removes; this file goes with that gate.
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
