package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// queryTimeoutNear is how long a GraphQL query that failed has run when it ran into the time
// GitHub gives one query, 10 s: GitHub answers such a query 502, and on 2026-11-24 it did so 10.9 s
// after the query was sent.
const queryTimeoutNear = 9 * time.Second

// queryTimedOut says whether err, a query's failure after took, is the query running out of
// GitHub's time rather than GitHub failing it: it ran for queryTimeoutNear or longer, its own bound
// ended it, or GitHub's answer says it could not respond in time. A rate limit is none.
func queryTimedOut(err error, took time.Duration) bool {
	if err == nil {
		return false
	}
	if after, _ := githubrest.RetryAfter(err); after > 0 {
		return false
	}
	return took >= queryTimeoutNear || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(err.Error()), "couldn't respond to your request in time")
}

// baseReadChunk is how many pull requests one GraphQL query of a base's batch asks for, each by
// number (chunkQuery). GitHub ends a query that runs 10 s, and its merge state costs GitHub time
// for each pull request asked: on 2026-11-24, against cybermonklabs/legion's 408 open pull
// requests, a query of every open pull request on main, a hundred to a page with its merge state,
// answered 502 after about 11 s three times out of three; its twenty oldest asked for by number
// answered in 7.5 and 8.1 s and once 502 after 10.9 s; ten in 3.6 to 4.9 s; and five in 1.4 to
// 4.9 s over twelve queries. A query that fails all the same is asked again one pull request at a
// time (readBase).
const baseReadChunk = 5

// chunkSize is baseReadChunk, or the size a test sets (workflowRuntime.baseChunk).
func (w *workflowRuntime) chunkSize() int {
	if w.baseChunk > 0 {
		return w.baseChunk
	}
	return baseReadChunk
}

// pullRequestFields is what a read of a base's batch asks GitHub of each pull request: its
// number, whether it is open, its head, the base it sits on now, and GitHub's lazily computed
// mergeability and merge state for that head, which a GET /pulls/{number} answers as `state`,
// `head`, `base`, `mergeable` and `mergeable_state` (graphQLPull.githubPullRequest).
const pullRequestFields = "number state headRefOid headRefName baseRefName mergeable mergeStateStatus"

// chunkQuery is the GraphQL query of numbers, pull requests of the repository $owner/$name: each
// asked for by its number under the alias p<number>, so one that GitHub cannot answer leaves the
// others answered.
func chunkQuery(numbers []int) string {
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, number := range numbers {
		fmt.Fprintf(&query, "    p%d: pullRequest(number: %d) { %s }\n", number, number, pullRequestFields)
	}
	query.WriteString("  }\n}")
	return query.String()
}

// graphQLPull is one pull request as chunkQuery answers it.
type graphQLPull struct {
	Number           int    `json:"number"`
	State            string `json:"state"`
	HeadRefOid       string `json:"headRefOid"`
	HeadRefName      string `json:"headRefName"`
	BaseRefName      string `json:"baseRefName"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
}

// githubPullRequest is p as GET /pulls/{number} answers the same pull request, the shape
// applyMergeability reads: GitHub's GraphQL `state` OPEN is the REST `open`, and CLOSED and MERGED
// are its `closed`; its `mergeable` MERGEABLE, CONFLICTING and UNKNOWN are the REST `mergeable`
// true, false and null; and its `mergeStateStatus` is the REST `mergeable_state` in capitals
// (CLEAN, BLOCKED, BEHIND, DIRTY, DRAFT, HAS_HOOKS, UNSTABLE, UNKNOWN). A state outside those reads
// as unknown, as a REST one does (githubPullRequest.mergeState).
func (p graphQLPull) githubPullRequest() githubPullRequest {
	pull := githubPullRequest{Number: p.Number, State: "closed", MergeableState: strings.ToLower(p.MergeStateStatus), Merged: p.State == "MERGED"}
	if p.State == "OPEN" {
		pull.State = "open"
	}
	pull.Base.Ref, pull.Head.Ref, pull.Head.SHA = p.BaseRefName, p.HeadRefName, p.HeadRefOid
	switch p.Mergeable {
	case "MERGEABLE":
		mergeable := true
		pull.Mergeable = &mergeable
	case "CONFLICTING":
		mergeable := false
		pull.Mergeable = &mergeable
	}
	return pull
}

// queryChunk asks GitHub's GraphQL API for numbers, pull requests of repo, with the implement App's
// installation token github carries, in one query (chunkQuery) bounded by issueRead, as a REST read
// of one pull request is, made through that installation's gate (queryGate.run: its rate-limit
// hold, its pace and one query at a time). It answers each pull request GitHub answered by number,
// GitHub's word on each it did not (an error of the answer's for that pull request alone, which
// leaves the others answered), the moment the answer was in hand, and whether a query that failed
// ran out of GitHub's time (queryTimedOut). A query GitHub refused or answered with no repository
// at all is an error.
func (w *workflowRuntime) queryChunk(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, numbers []int) (map[int]graphQLPull, map[int]string, time.Time, bool, error) {
	query := githubrest.Client{Token: github.Token, API: githubrest.GraphQLEndpoint(w.githubAPI)}
	var answer struct {
		Repository map[string]*graphQLPull `json:"repository"`
	}
	var readAt time.Time
	took, err := w.queryGate(github.Scope).run(ctx, func() error {
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		defer cancel()
		err := query.GraphQL(bounded, chunkQuery(numbers), map[string]any{"owner": repo.Owner(), "name": repo.Name()}, &answer)
		readAt = time.Now().UTC()
		return err
	})
	var partial githubrest.GraphQLErrors
	if err != nil && !(errors.As(err, &partial) && answer.Repository != nil) {
		return nil, nil, readAt, queryTimedOut(err, took), fmt.Errorf("read pull requests %v of %s: %w", numbers, repo, err)
	}
	if answer.Repository == nil {
		return nil, nil, readAt, false, fmt.Errorf("read pull requests %v of %s: GitHub answered no repository", numbers, repo)
	}
	answers, unanswered := map[int]graphQLPull{}, map[int]string{}
	for _, number := range numbers {
		if node := answer.Repository[fmt.Sprintf("p%d", number)]; node != nil {
			answers[number] = *node
		} else {
			unanswered[number] = "no answer"
		}
	}
	for _, entry := range partial {
		if len(entry.Path) >= 2 {
			var number int
			if alias, ok := entry.Path[1].(string); ok {
				if _, err := fmt.Sscanf(alias, "p%d", &number); err == nil {
					if _, left := unanswered[number]; left {
						unanswered[number] = entry.Message
					}
				}
			}
		}
	}
	return answers, unanswered, readAt, false, nil
}
