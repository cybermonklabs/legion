package ghbranch

// RefusalKind is why a write of an issue's branch failed in a way the same write meets again
// (Refusal): what GitHub answered, or that the deployment's rules refuse the branch's name.
type RefusalKind string

const (
	// RefusedName is a branch name the deployment's private-name rules match (leakguard.Refusal):
	// the name is the issue's own, so every retry sends the same one.
	RefusedName RefusalKind = "name-refused"
	// RefusedForbidden is a REST call GitHub answered 403 that is no rate limit: the App lacks a
	// permission the call needs, which only a person grants.
	RefusedForbidden RefusalKind = "forbidden"
	// RefusedNotFound is a REST call GitHub answered 404: a repository the App cannot see, a base
	// branch GitHub does not have, or a commit a compare does not find. A read of the branch's own
	// ref that GitHub answers 404 is no branch, and never this.
	RefusedNotFound RefusalKind = "not-found"
	// RefusedConflict is a REST call GitHub answered 409: a repository with no commit, which holds
	// nothing to start a branch at.
	RefusedConflict RefusalKind = "conflict"
	// RefusedUnprocessable is a create GitHub answered 422 with anything but "Reference already
	// exists", which Create reads as the branch it was asked for.
	RefusedUnprocessable RefusalKind = "unprocessable"
	// RefusedNoBase is a move whose base and fallback are both branches GitHub does not have.
	RefusedNoBase RefusalKind = "no-base"
	// RefusedNoCommit is a read of a branch's ref that GitHub answered with no commit.
	RefusedNoCommit RefusalKind = "no-commit"
	// RefusedNoNodeID is a read of the repository that named no node id, which the move's
	// mutation names the repository by.
	RefusedNoNodeID RefusalKind = "no-node-id"
	// RefusedCompareStatus is a compare GitHub answered with a status this package does not read.
	RefusedCompareStatus RefusalKind = "compare-status"
	// RefusedGraphQLForbidden is the move's mutation refused as forbidden: GitHub's GraphQL
	// endpoint answered 403 that is no rate limit, or updateRefs answered an error of type
	// FORBIDDEN or INSUFFICIENT_SCOPES. It is what an App that cannot use the GraphQL API meets.
	RefusedGraphQLForbidden RefusalKind = "graphql-forbidden"
	// RefusedGraphQLNotFound is the move's mutation refused as not found: the GraphQL endpoint
	// answered 404, as a GitHub with no GraphQL API there does, or updateRefs answered an error of
	// type NOT_FOUND.
	RefusedGraphQLNotFound RefusalKind = "graphql-not-found"
)

// Refusal is a failure of Create or Reset that the same call meets again, with its kind: GitHub's
// verdict on the App, the repository or the request, or the deployment's rules on the branch's
// name. Err is the failure as the call met it, and its text is the refusal's.
type Refusal struct {
	Kind RefusalKind
	Err  error
}

func (r *Refusal) Error() string { return r.Err.Error() }

func (r *Refusal) Unwrap() error { return r.Err }

// PermanentRefusal is the kind of refusal err carries, and whether it carries one: a failure of
// Create or Reset that a retry meets again (Refusal).
func PermanentRefusal(err error) (RefusalKind, bool) {
	return "", false
}
