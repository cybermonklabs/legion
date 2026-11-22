package api

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// testRepositories serves every repository the API tests record issues on, each at main. A
// repository it does not name is one no preparation has read GitHub for, which the daemon answers
// as unread (its lookup): only GitHub's answer for a repository refuses it.
var testRepositories = servedRepositories{"acme/widgets": "main", "sjawhar/legion": "main"}

type servedRepositories map[string]string

func (r servedRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if branch, ok := r[resolved]; ok {
		return workflow.Resolution{Repo: resolved, DefaultBranch: branch, GitHubRepo: resolved}, nil
	}
	return workflow.Resolution{Refusal: "GitHub has not been read for " + resolved + " yet", Unread: true}, nil
}

// prepare is the daemon's preparation of a Dispatch issue fact (its prepareRepository) as far as a
// recorded issue's correction reads it, over these repositories: a fact resolved to one of them
// carries GitHub's read of it, both role Apps installed (intake.DispatchIssue.RepositoryRead),
// which the correction is written from (workflow.Engine.ApplyRepositoryChange), and one resolved
// to any other carries none, as a read GitHub did not answer.
func (r servedRepositories) prepare(_ context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	if branch, ok := r[issue.ResolvedRepo]; ok {
		issue.RepositoryRead = &ghbranch.Repository{Name: ghrepo.MustParse(issue.ResolvedRepo), DefaultBranch: branch}
	}
	return issue, nil
}
