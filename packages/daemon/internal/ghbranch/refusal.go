package ghbranch

import (
	"errors"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

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
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return "", false
	}
	return refusal.Kind, true
}

// The answers of GitHub that lack what a call needs. Each is the end of its call's error, so the
// error's text is as it was, and refusalKind reads its kind from it.
var (
	errNoBase        = errors.New("GitHub has no such branch")
	errNoCommit      = errors.New("GitHub named no commit")
	errNoNodeID      = errors.New("GitHub named none")
	errCompareStatus = errors.New("GitHub answered the status")
)

// mutationFailure is a failure of the updateRefs mutation itself (moveFrom), so refusalKind reads
// GitHub's refusal of it as the GraphQL API's. Its text is the failure's own.
type mutationFailure struct{ err error }

func (m mutationFailure) Error() string { return m.err.Error() }

func (m mutationFailure) Unwrap() error { return m.err }

// withRefusal is err as Create and Reset return it: a *Refusal of err's kind where a retry meets
// it again (refusalKind), and err as it is otherwise.
func withRefusal(err error) error {
	if err == nil {
		return nil
	}
	if kind, ok := refusalKind(err); ok {
		return &Refusal{Kind: kind, Err: err}
	}
	return err
}

// refusalKind is the kind of refusal err is, and whether it is one a retry meets again. These are
// GitHub's documented refusals of the calls Create and Reset make, and the deployment's rules:
//
//   - a REST answer that is no rate limit: 403 (the App lacks the permission), 404 (a repository
//     the App cannot see, a base GitHub does not have, a commit a compare does not find), 409 (a
//     repository with no commit) and 422 (a create GitHub will not take);
//   - the mutation's answer that is no rate limit: the GraphQL endpoint's 403 or 404, and an
//     updateRefs error of type FORBIDDEN, INSUFFICIENT_SCOPES or NOT_FOUND;
//   - an answer that lacks what the call needs: no commit, no node id, a compare status this
//     package does not read, and neither the base nor the fallback on GitHub;
//   - a branch name the deployment's rules match.
//
// Nothing else is. A rate limit of either API (a 429, the secondary limit's 403, GraphQL's
// RATE_LIMITED), a 5xx, a 401 for a token GitHub did not take, an updateRefs error of no type
// while the branch still points at the merged head, and a call that got no answer may each pass on
// a retry, as the merge-state reads read GitHub's answers (internal/daemon's queryGate.settle):
// an answer of GitHub's own is GitHub's verdict, and no answer is no verdict.
func refusalKind(err error) (RefusalKind, bool) {
	var named *leakguard.Refusal
	switch {
	case errors.As(err, &named):
		return RefusedName, true
	case errors.Is(err, errNoBase):
		return RefusedNoBase, true
	case errors.Is(err, errNoCommit):
		return RefusedNoCommit, true
	case errors.Is(err, errNoNodeID):
		return RefusedNoNodeID, true
	case errors.Is(err, errCompareStatus):
		return RefusedCompareStatus, true
	}
	var mutation mutationFailure
	ofMutation := errors.As(err, &mutation)
	var answer *githubrest.Answer
	if errors.As(err, &answer) {
		if answer.RateLimited {
			return "", false
		}
		switch {
		case ofMutation && answer.Status == http.StatusForbidden:
			return RefusedGraphQLForbidden, true
		case ofMutation && answer.Status == http.StatusNotFound:
			return RefusedGraphQLNotFound, true
		case ofMutation:
			return "", false
		case answer.Status == http.StatusForbidden:
			return RefusedForbidden, true
		case answer.Status == http.StatusNotFound:
			return RefusedNotFound, true
		case answer.Status == http.StatusConflict:
			return RefusedConflict, true
		case answer.Status == http.StatusUnprocessableEntity:
			return RefusedUnprocessable, true
		}
		return "", false
	}
	var answered githubrest.GraphQLErrors
	if ofMutation && errors.As(err, &answered) {
		for _, refusal := range answered {
			switch refusal.Type {
			case "FORBIDDEN", "INSUFFICIENT_SCOPES":
				return RefusedGraphQLForbidden, true
			case "NOT_FOUND":
				return RefusedGraphQLNotFound, true
			}
		}
	}
	return "", false
}
