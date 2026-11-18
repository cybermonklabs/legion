package api

import "github.com/sjawhar/legion/daemon/internal/workflow"

// testRepositories serves every repository the API tests record issues on, each at main. A
// repository it does not name is one no preparation has read GitHub for, which the daemon answers
// as unread (its lookup): no mapping entry refuses a repository, GitHub's answer for it does.
var testRepositories = servedRepositories{"acme/widgets": "main", "sjawhar/legion": "main"}

type servedRepositories map[string]string

func (r servedRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if branch, ok := r[resolved]; ok {
		return workflow.Resolution{Repo: resolved, DefaultBranch: branch, GitHubRepo: resolved}, nil
	}
	return workflow.Resolution{Refusal: "GitHub has not been read for " + resolved + " yet", Unread: true}, nil
}
