// Package githubread declares the read set: the permissions a Legion GitHub App token that reads a
// repository holds, each at read. Two programs read it. The Legion daemon's read lease
// (packages/daemon/internal/appauth) asks for it from an App key: it is what a role holds for each
// repository of its project but its own issue's, what a controller holds for each one, and the
// token a pod clones and fetches its repository with. The secrets broker's read-only narrowing
// (packages/envoy/internal/broker/secrets) asks for it for a github-app secret released read-only.
// Each of the two cuts the set to what the App's installation holds, so an installation that
// holds less gets a token that holds less.
//
// The set is the most such a token holds. A read lease reaches every repository its installation
// holds, and a broker token can, so what keeps it from writing is that each permission of the set
// is a read, and that the set names nothing beside them. Each permission is here for a reason of
// its own:
//
// contents:read is a clone, a fetch and a read of a file: a pod's repository is cloned and fetched
// with it, and a role reads the code of another repository of its project through it. It stays a
// read: contents:read cannot push a commit, a branch or a tag.
//
// metadata:read is every read of the repository itself, such as its name, its default branch and
// its collaborators, and GitHub requires it of every token. It stays a read: metadata:read
// changes nothing of the repository.
//
// pull_requests:read is a pull request, its reviews and its comments: a role reads the work of
// another repository of its project there, and a controller reads the work of each one. It stays
// a read: pull_requests:read cannot open, review, comment on or merge a pull request.
//
// issues:read is here because an issue is where a work's context is often written, and a role or
// a controller reads the issues of every repository its project serves. Measured on 2026-11-10,
// and again on 2026-11-15, from roles of both Apps on a private repository: with no issues
// permission in the request, reading an issue answered 403 "Resource not accessible by
// integration" while the same token read that repository's contents and pull requests. It stays
// a read: issues:read cannot open, edit, close or comment on an issue.
//
// actions:read is here because reading a repository includes reading what its merges did: a
// deployment check of a merge reads the push run that carries it, that run's jobs and a job's
// log, and a role that cannot read them cannot tell a deploy that applied from one that was
// skipped. Measured on 2026-11-04 against a private repository whose installation holds
// actions:write: unnarrowed, actions/runs and actions/runs/{id}/jobs answered 200 and
// actions/jobs/{id}/logs answered 302 to a signed URL that answered 200; narrowed to contents,
// pull requests and metadata, all three answered 403 "Resource not accessible by integration".
// The narrowing was the whole refusal, and no App or installation setting was missing. It stays a
// read: actions:read cannot cancel, re-run or dispatch a workflow, and reaches no Actions secret
// or variable, which are permissions of their own.
//
// checks:read is here because a role and a controller read what CI said of a head: the check runs
// of a commit, and the check runs in a pull request's check rollup. Measured on 2026-11-16 on a
// private repository: through a read lease, commits/<sha>/check-runs answered 403 and the pull
// request's statusCheckRollup answered FORBIDDEN, while the implement App's token without the
// narrowing read the same commit. It stays a read: checks:read cannot create or change a check
// run.
//
// statuses:read is here because a commit status is the other half of what CI says of a head: the
// commit statuses of a commit, and the statuses in a pull request's check rollup. A base branch
// can require a commit status before a merge. Measured on 2026-11-16: through a read lease on a
// private repository, commits/<sha>/status answered 403. It stays a read: statuses:read cannot
// create a commit status.
//
// attestations:read is here because a role reads the attestations of an artifact, to check that
// an image was built from the default branch. Measured on 2026-11-17: through a read lease on a
// private repository, a read of its attestations answered 403 "Resource not accessible by
// integration". It stays a read: attestations:read cannot create or delete an attestation.
package githubread

// Permissions is the read set as a GitHub token request's permissions object names it: each
// permission the package doc gives, mapped to "read". It returns a new map at each call, so a
// caller that changes its copy changes no other caller's set.
func Permissions() map[string]string {
	return map[string]string{
		"contents":      "read",
		"metadata":      "read",
		"pull_requests": "read",
		"issues":        "read",
		"actions":       "read",
		"checks":        "read",
		"statuses":      "read",
		"attestations":  "read",
	}
}
