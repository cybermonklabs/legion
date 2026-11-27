package exchange_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// TestAWorkflowsRepositoryIsHeldToGitHubsOwnShape: the shape of a workflow entry's repository is
// githubname's one rule for GitHub's own words, so a name no repository in a path can hold — "."
// and "..", which name the directory and its parent — is refused. Every other refusal the entry
// already gave stands (TestNewSetRefusesEntriesWithoutPrintingThem), and a repository GitHub can
// hold is taken.
func TestAWorkflowsRepositoryIsHeldToGitHubsOwnShape(t *testing.T) {
	entry := func(repo string) []exchange.WorkflowEntry {
		return []exchange.WorkflowEntry{{Repository: repo, Workflow: workflow, Ref: ref, Project: project, Person: person}}
	}
	for _, refused := range []string{"ada/.", "ada/.."} {
		_, err := exchange.NewSet(nil, nil, entry(refused), nil, signInDomain)
		if err == nil || !strings.Contains(err.Error(), "a repository is <owner>/<name>") {
			t.Errorf("NewSet(a workflow of %q) = %v; want it refused: no repository in a path holds that name", refused, err)
		}
	}
	for _, taken := range []string{"ada/example-repo", "ada/example.repo", "mona-cat_octo/example-repo"} {
		if _, err := exchange.NewSet(nil, nil, entry(taken), nil, signInDomain); err != nil {
			t.Errorf("NewSet(a workflow of %q) = %v; want it taken: GitHub holds such a repository", taken, err)
		}
	}
}
