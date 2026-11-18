package delivery

// What ends a refusal the reconcile reads, and what a pass spends on refusals: the one answer of
// GitHub that settles a refusal (pullRequestAbsent), and the pass's lookups of each repository's
// installation (passInstallations). One answer of GitHub can show that a repository is readable
// and that it holds no such pull request; that answer is a read that succeeded, and Dispatch keeps
// nothing for that number.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// pullRequestRefusal is GitHub's refusal of one read of a pull request (RefreshPullRequestHead):
// why, and the read GitHub refused, the repository's canonical owner and name, the number, and the
// installation token the read was made with, "" where no token was minted because no installation
// covers the repository.
type pullRequestRefusal struct {
	err         error
	owner, repo string
	number      int
	token       string
}

// absenceQuery asks GitHub's GraphQL API about one pull request of one repository in one request,
// so that one answer shows three facts at once: whether the credential reads the repository,
// whether it reads the repository's pull requests (pullRequests' totalCount, which only a
// credential with the pull-requests permission is given), and whether the repository holds a pull
// request of that number. GraphQL's Int is 32 bits, which every number pullRequestNamed lets
// through fits.
const absenceQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequests { totalCount } pullRequest(number: $number) { number } }
}`

// pullRequestAbsent answers whether GitHub's refusal of a read of a pull request is final: whether
// one answer of GitHub, to absenceQuery asked with the token of the read GitHub refused, shows the
// repository readable, its pull requests readable, and the pull request absent. Then the
// repository holds no such pull request, which R4 counts as a read that succeeded, and Dispatch
// keeps nothing for that number. That answer is HTTP 200 with a repository object whose
// pullRequests holds a totalCount and whose pullRequest is null, beside errors that are all of
// type NOT_FOUND on the path repository.pullRequest. GitHub answers a number its repository does
// not have, and an issue's number, so (read with `gh api graphql`, 2026-11-17):
//
//	{"data":{"repository":{"pullRequests":{"totalCount":3856},"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 2000000000."}]}
//
// The count is the proof that the token can read pull requests of the repository at all. Without
// it, a token that reads the repository and holds no pull-requests permission could be answered
// a null pull request with NOT_FOUND for one that exists, and that would end a real pull request's
// read and lose its head (hardening note H1 of the review of cybermonklabs/legion#4209 at
// ef5112db43). GitHub's guide to forming GraphQL calls says a token that lacks the permissions a
// resource needs gets an error that states them, so such a token gets no count; an answer with no
// count, or a null one, is no evidence.
//
// Every other answer is no evidence, and the read stays owed: a refusal that is not GitHub's 404
// or 410 of the pull request; one the App had no token for, since no installation covers the
// repository; a null repository, which GitHub answers alike for a repository the token does not
// read and for one that does not exist (NOT_FOUND on the path repository); an answer with no count
// of the pull requests; an error of another type, FORBIDDEN for one, or on another path; a pull
// request GitHub now answers; a status other than 200; an answer that does not decode; and a
// request that failed, which is answered as the error, so the pass keeps a rate limit
// (passRateLimit) and logs any other failure.
//
// The token of the refused read is what makes the facts one answer about that read: an
// installation that lost the repository between the two reads answers a null repository, and one
// that has it again answers the pull request. No memory of a pass or of the process ends a record:
// each refusal is asked about on its own, at its own read. The call is githubapp.Client.GraphQL,
// the client the CI reconcile's batched read queries GitHub through (cireconcile.Reconciler.query),
// held by the token's installation scope and by that installation's GraphQL budget
// (githubapp.GraphQLInstallationScope). It costs one request, one point of that budget (the count
// included: `rateLimit { cost }` answers 1), for each read of a 404 or a 410 a pass makes: at
// most RefusedReadsPerPass from the refusals, and from the failures each that GitHub now refuses
// so, up to PullRequestHeadBatch more.
func pullRequestAbsent(ctx context.Context, client *githubapp.Client, refused *pullRequestRefusal) (bool, error) {
	if refused == nil || refused.token == "" || !errors.Is(refused.err, ErrPullRequestNotFound) {
		return false, nil
	}
	body, status, _, err := client.GraphQL(ctx, refused.token, absenceQuery,
		map[string]any{"owner": refused.owner, "name": refused.repo, "number": refused.number})
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, nil
	}
	var answer struct {
		Data struct {
			Repository *struct {
				PullRequests *struct {
					TotalCount *int `json:"totalCount"`
				} `json:"pullRequests"`
				PullRequest *struct {
					Number int `json:"number"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type string `json:"type"`
			Path []any  `json:"path"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Data.Repository == nil || answer.Data.Repository.PullRequest != nil || len(answer.Errors) == 0 {
		return false, nil
	}
	if counted := answer.Data.Repository.PullRequests; counted == nil || counted.TotalCount == nil {
		return false, nil
	}
	for _, refusal := range answer.Errors {
		if refusal.Type != "NOT_FOUND" || len(refusal.Path) != 2 || refusal.Path[0] != "repository" || refusal.Path[1] != "pullRequest" {
			return false, nil
		}
	}
	return true, nil
}

// passInstallations is one reconcile head pass's lookups of the installation that covers each
// repository it reads, by canonical name (reconcilePullRequestHeads). githubapp.Client keeps the
// installation a repository resolved to, but no refusal: its answer that the App is not installed
// on a repository is asked of GitHub again at every read, each one signed by the App JWT under the
// App's own budget, which every token mint shares (githubapp.AppScope). A pass reads up to
// RefusedReadsPerPass owed rows, and any bearer of Envoy's /v1 publish route can owe rows of a
// repository the App is not installed on. So the first read of a
// repository in a pass looks its installation up, the reads of it beside that one wait for that
// answer, and a repository the App is not installed on costs the pass no further call: each of its
// rows is read as the refusal it is, and stays owed. It is a memory of work and nothing more: it
// keeps only GitHub's answer that no installation covers the repository, it ends no record, and
// the next pass asks again, so an installation that comes back is found at the next pass.
type passInstallations struct{ known sync.Map }

// installationLookup is one repository's lookup of a pass, made once.
type installationLookup struct {
	once sync.Once
	// missing is GitHub's answer that the App is not installed on the repository, nil after any
	// other answer.
	missing error
}

// repositoryToken is client.RepositoryToken for owner/repo, with the installation looked up once a
// pass for a repository the App is not installed on. A nil passInstallations, a caller with no
// pass, asks every time.
func (p *passInstallations) repositoryToken(ctx context.Context, client *githubapp.Client, owner, repo string) (string, error) {
	if p == nil {
		return client.RepositoryToken(ctx, owner, repo)
	}
	entry, _ := p.known.LoadOrStore(owner+"/"+repo, &installationLookup{})
	lookup := entry.(*installationLookup)
	var token string
	var err error
	asked := false
	lookup.once.Do(func() {
		asked = true
		token, err = client.RepositoryToken(ctx, owner, repo)
		if errors.Is(err, githubapp.ErrNoInstallation) {
			lookup.missing = err
		}
	})
	switch {
	case asked:
		return token, err
	case lookup.missing != nil:
		return "", lookup.missing
	default:
		// The installation, and its token, are the client's own to keep: githubapp.Client
		// answers them from its caches and asks GitHub again only once they lapse.
		return client.RepositoryToken(ctx, owner, repo)
	}
}
