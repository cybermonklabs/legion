// Package githubpath holds the GitHub API paths Dispatch's GitHub proxy reads: the proxy refuses
// every other path by them (githubapi.ProxyREST), and a record of a request to the proxy writes a
// path by them (heldpath.Of), so the proxy and its records cannot disagree on what the proxy reads.
package githubpath

import "regexp"

// Prefix is the route the web app reads GitHub under: /api/github/rest/<GitHub API path>.
const Prefix = "/api/github/rest/"

// readable are the GitHub API paths the web app reads, and the only ones the proxy forwards: a
// pull request, an issue (which also answers for a pull request), the check runs of a commit, and
// the workflow runs (`actions/runs`, group 3), whose query the proxy holds to one commit. The App
// can read more of a repository than that, and none of the rest is shown anywhere.
var readable = regexp.MustCompile(`^repos/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(?:pulls/[0-9]+|issues/[0-9]+|commits/[0-9A-Fa-f]{7,64}/check-runs|(actions/runs))$`)

// Read is one path the proxy reads.
type Read struct {
	// Owner and Repo name the repository the path reads.
	Owner, Repo string
	// WorkflowRuns is true for a commit's workflow runs, whose query the proxy holds to one
	// commit and its page.
	WorkflowRuns bool
}

// Match reads suffix, a request path after Prefix, as one of the paths the proxy reads: a pull
// request, an issue, a commit's check runs or its workflow runs, each under
// repos/<owner>/<name>/, where neither name is . or ... It answers false for every other path.
func Match(suffix string) (Read, bool) {
	match := readable.FindStringSubmatch(suffix)
	if match == nil || match[1] == "." || match[1] == ".." || match[2] == "." || match[2] == ".." {
		return Read{}, false
	}
	return Read{Owner: match[1], Repo: match[2], WorkflowRuns: match[3] != ""}, true
}
