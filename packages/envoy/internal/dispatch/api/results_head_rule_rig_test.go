// results_head_rule_rig_test.go is the Dispatch the tests of the head rule of a `before_merge`
// check build (newHeadRuleRig): an issue of a repository whose Verification document holds one
// check, k1, read through a GitHub that holds every commit. The tests hold the route's answer as
// cybermonklabs/legion#4230 writes it, and the rig is made of helpers its earlier rounds hold too,
// so each test builds at a commit before the rule and fails there on its own assertion.
package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rig's repository and the two commits of it that a test writes a pull request's head at are
// ciJobRepository, ciJobCommit and ciJobHead, which ci_job_results_test.go declares for the CI
// job's rig: one issue's repository and commits serve both rigs, and a test of either reads the
// same three names.

// headRuleVerification is the Verification document of the rig's issue: k1, a `before_merge`
// check that no CI job proves.
var headRuleVerification = ciCheckBlock("k1", `negative_control="A build whose server does not build."`)

// headRuleRig is a Dispatch, its database and the key of its one issue, ACME-1, which resolves
// to ciJobRepository and holds no pull request until a test writes one.
type headRuleRig struct {
	handler  http.Handler
	database *store.Store
	issue    string
}

// newHeadRuleRig is a headRuleRig of its own for t. Its GitHub holds every commit, so each result
// is recorded, and names `main` as the default branch.
func newHeadRuleRig(t *testing.T) headRuleRig {
	t.Helper()
	handler, database := resultsGitHubAnswering(t, map[string]bool{ciJobRepository: true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" })
	issue := issueInRepository(t, handler, "ACME", ciJobRepository, "A change in several pull requests")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", headRuleVerification)
	return headRuleRig{handler: handler, database: database, issue: issue.Key}
}
