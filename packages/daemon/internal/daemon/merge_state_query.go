package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
// ended it, or GitHub's answer says it could not respond in time (githubrest.NamesTimeout). A rate
// limit is none.
func queryTimedOut(err error, took time.Duration) bool {
	if err == nil {
		return false
	}
	if after, _ := githubrest.RetryAfter(err); after > 0 {
		return false
	}
	return took >= queryTimeoutNear || errors.Is(err, context.DeadlineExceeded) || githubrest.NamesTimeout(err)
}

// baseReadChunk is how many pull requests one GraphQL query of a base's batch asks for, each by
// number (chunkQuery). GitHub ends a query that runs 10 s, and its merge state costs GitHub time
// for each pull request asked: on 2026-11-24, against cybermonklabs/legion's 408 open pull
// requests, a query of every open pull request on main, a hundred to a page with its merge state,
// answered 502 after about 11 s three times out of three; its twenty oldest asked for by number
// answered in 7.5 and 8.1 s and once 502 after 10.9 s; ten in 3.6 to 4.9 s; and five in 1.4 to
// 4.9 s over twelve queries. A query that runs out of GitHub's time all the same is asked again
// one pull request at a time (readBase).
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
// asked for by its number under its own alias (pullAlias), so one that GitHub cannot answer leaves
// the others answered.
func chunkQuery(numbers []int) string {
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, number := range numbers {
		fmt.Fprintf(&query, "    %s: pullRequest(number: %d) { %s }\n", pullAlias(number), number, pullRequestFields)
	}
	query.WriteString("  }\n}")
	return query.String()
}

// pullAlias is the alias chunkQuery asks for pull request number under, which GitHub answers it
// under and names in an error of its own about it (queryChunk).
func pullAlias(number int) string {
	return "p" + strconv.Itoa(number)
}

// graphQLPull is one pull request as chunkQuery answers it.
type graphQLPull struct {
	Number           int        `json:"number"`
	State            string     `json:"state"`
	HeadRefOid       string     `json:"headRefOid"`
	HeadRefName      string     `json:"headRefName"`
	BaseRefName      githubText `json:"baseRefName"`
	Mergeable        string     `json:"mergeable"`
	MergeStateStatus string     `json:"mergeStateStatus"`
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

// chunkAnswer is what one query of a base's batch read (queryChunk): github, the implement App's
// client of the repository whose token the query was made with, which a CONFLICTING answer's base
// rules are read with too (applyMergeability); answers, each pull request GitHub answered, by
// number; unanswered, GitHub's word on each it did not; readAt, the moment the answer was in hand;
// and, for a query that failed, whether it was never sent because the token could not be minted
// (unminted) or ran out of GitHub's time (timedOut, queryTimedOut).
type chunkAnswer struct {
	github     githubrest.Client
	answers    map[int]graphQLPull
	unanswered map[int]string
	readAt     time.Time
	unminted   bool
	timedOut   bool
}

// queryChunk asks GitHub's GraphQL API for numbers, pull requests of repo, in one query
// (chunkQuery) bounded by issueRead, as a REST read of one pull request is, made through the gate
// of the implement App's installation for repo's owner (queryGate.run: its rate-limit hold, its
// pace and one query at a time). The installation token is minted in the query's own turn, once
// the gate's hold and pace have passed: a hold can outlast what a lease is promised to have left
// (appauth's refreshWindow), and a token minted before it could expire while the query waits,
// which GitHub would answer 401, an answer of its own that ends the gate's backoff (settle). The
// token cache makes a mint for each query cheap, and the mint is bounded by issueRead as the query
// is, so a cold mint that hangs holds the installation's other queries no longer than one query
// can. It answers each pull request GitHub answered by number, GitHub's word on each it did not (an
// error of the answer's for that pull request alone, which names it by its alias and leaves the
// others answered), and the moment the answer was in hand. A token that could not be minted, a
// query GitHub refused and one it answered with no repository at all are errors.
func (w *workflowRuntime) queryChunk(ctx context.Context, repo ghrepo.Repository, numbers []int) (chunkAnswer, error) {
	var got chunkAnswer
	var minted error
	var took time.Duration
	var answer struct {
		Repository map[string]*graphQLPull `json:"repository"`
	}
	err := w.queryGate(implementScope(repo)).run(ctx, func() error {
		minting, cancelMint := context.WithTimeout(ctx, issueRead)
		got.github, minted = implementClient(minting, w.tokens, w.githubAPI, repo)
		cancelMint()
		if minted != nil {
			return minted
		}
		query := githubrest.Client{Token: got.github.Token, API: githubrest.GraphQLEndpoint(w.githubAPI)}
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		defer cancel()
		sent := time.Now()
		err := query.GraphQL(bounded, chunkQuery(numbers), map[string]any{"owner": repo.Owner(), "name": repo.Name()}, &answer)
		took = time.Since(sent)
		got.readAt = time.Now().UTC()
		return err
	})
	if minted != nil {
		got.unminted = true
		return got, minted
	}
	var partial githubrest.GraphQLErrors
	if err != nil && !(errors.As(err, &partial) && answer.Repository != nil) {
		got.timedOut = queryTimedOut(err, took)
		return got, fmt.Errorf("read pull requests %v of %s: %w", numbers, repo, err)
	}
	if answer.Repository == nil {
		return got, fmt.Errorf("read pull requests %v of %s: GitHub answered no repository", numbers, repo)
	}
	asked := make(map[string]int, len(numbers))
	for _, number := range numbers {
		asked[pullAlias(number)] = number
	}
	got.answers, got.unanswered = map[int]graphQLPull{}, map[int]string{}
	for alias, number := range asked {
		if node := answer.Repository[alias]; node != nil {
			got.answers[number] = *node
		} else {
			got.unanswered[number] = "no answer"
		}
	}
	for _, entry := range partial {
		if len(entry.Path) < 2 {
			continue
		}
		alias, _ := entry.Path[1].(string)
		if number, ok := asked[alias]; ok {
			if _, left := got.unanswered[number]; left {
				got.unanswered[number] = entry.Message
			}
		}
	}
	return got, nil
}
