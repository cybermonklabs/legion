package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logging"
)

// commit_pulls_batch.go is the read-through of many commits of one repository in one question to
// GitHub (ReadThroughCommitsPulls): one GraphQL query naming each commit, where the REST read
// (ReadThroughCommitPulls) names one. A results read of an issue whose results stand at scores of
// commits no stored head names asks it once for all of them, so no bound on how many commits one
// read may learn is needed and no result is left answered from the stored rows alone.
//
// Measured against GitHub on 2026-11-27 over 18 commits of one repository, each the head of a
// pull request: 18 sequential REST reads took 19.6 s to 21.1 s over three rounds, and one query
// of the 18 took 2.0 s to 2.8 s, cost one point of the installation's GraphQL budget, and named
// the same pull requests. Each of those calls went through a command-line client whose own start
// costs about a second, so a server's saving is smaller than the ratio of those times; what the
// measurement shows is that one question answers what eighteen answered.
//
// Everything the answers mean is the REST read's: the rows written, which pull requests get one,
// the settles, and what a commit no pull request heads leaves open (ReadThroughCommitPulls's doc
// comment says it in full). Only the question changes.

// CommitPullsBatch is the most commits one query asks about. GitHub prices a query by its
// connections and refuses an answer above the client's limit, which a batch of this size has
// room under: each commit answers at most CommitPullsPerCommit pull requests, each with its first
// page of commit messages, as the facts query answers at most FactsBatchSize pull requests the
// same way. A read of more commits sends a query for each batch of this many.
const CommitPullsBatch = 20

// CommitPullsPerCommit is the most pull requests the query reads of one commit. GitHub names
// every open and merged pull request whose head or history holds the commit; a result's commit
// is behind a handful at most, and the rows this read writes are the heads a check compares
// with, which no reader reads scores of.
const CommitPullsPerCommit = 10

// CommitPullsRead is what one commit of a batched read-through learned: how many rows were
// written for it, and whether a pull request GitHub named for it has it as its head, which the
// caller settles the pair on (CommitPullReads.Settle). It is the pair ReadThroughCommitPulls
// answers for one commit.
type CommitPullsRead struct {
	Written int
	AtHead  bool
}

// ReadThroughCommitsPulls teaches Dispatch the pull requests GitHub names for each of shas, in
// repoFull, and stores each one's facts, as ReadThroughCommitPulls does for one commit: it
// answers what each commit learned, by commit. Only a commit whose answer was stored in full is
// in the answer. A commit the read did not reach is absent, and so is the commit whose answer it
// could not store in full, whatever rows it wrote for it first, as ReadThroughCommitPulls answers
// that commit with its error. The error says why, and only ErrReadThroughNotStored gives an absent
// commit's claim back: a failure of GitHub's, its rate limit on a pull request's commit messages
// among them, keeps it.
//
// The commits are asked in batches of CommitPullsBatch, each one query. A batch GitHub's answer
// is too large for is split in two and asked again, and one commit whose answer alone is too
// large is read over REST (readThroughCommitPulls), as the facts query falls back to a REST read
// of one pull request. The first failure ends the read, with what the commits before it learned.
//
// A writer's read calls this. A read-only caller's read of an issue's results stores nothing and
// calls AnswerCommitsPulls, which is this read with its writes kept for the request.
func (a *Attributor) ReadThroughCommitsPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	repoFull string, shas []string, now time.Time,
) (map[string]CommitPullsRead, error) {
	return a.readThroughCommitsPulls(ctx, facts{pool: pool}, client, repoFull, shas, now)
}

// AnswerCommitsPulls is ReadThroughCommitsPulls for a read-only caller's results read, which
// stores nothing and keeps nothing after its request (plan D32): each of its statements runs in a
// transaction of pool that first makes the writes answers holds and is rolled back, and each
// write it makes is kept in answers (facts, HeadAnswers), as AnswerCommitPulls does for one
// commit.
func (a *Attributor) AnswerCommitsPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, answers *HeadAnswers,
	repoFull string, shas []string, now time.Time,
) (map[string]CommitPullsRead, error) {
	return a.readThroughCommitsPulls(ctx, facts{pool: pool, answers: answers}, client, repoFull, shas, now)
}

// readThroughCommitsPulls is the two above through f (facts): one read of the repository's token
// and of the reads owed in it, then a query for each batch of commits.
//
// The read runs on a context its caller's end cannot cancel, bounded by ReadThroughTimeout for
// each batch, as the REST read-through is bounded for its one commit: a client that hangs up must
// not cut a write short and leave a claim standing with nothing stored.
func (a *Attributor) readThroughCommitsPulls(
	ctx context.Context, f facts, client *githubapp.Client, repoFull string, shas []string, now time.Time,
) (map[string]CommitPullsRead, error) {
	reads := map[string]CommitPullsRead{}
	if len(shas) == 0 {
		return reads, nil
	}
	canonical := a.canonical(repoFull)
	owner, repo, token, err := repositoryRead(ctx, client, canonical)
	if err != nil {
		return reads, err
	}
	var owed map[int][]owedRead
	if err := f.read(ctx, func(q refs.Pool) error {
		owed = a.owedReadsIn(ctx, q, canonical)
		return nil
	}); err != nil {
		return reads, readThroughNotStored(err)
	}
	batch := commitPullsRead{
		attributor: a, client: client, token: token,
		canonical: canonical, storedUnder: repoFull, owner: owner, repo: repo,
		owed: owed, now: now, reads: reads,
	}
	for _, chunk := range slices.Collect(slices.Chunk(shas, CommitPullsBatch)) {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReadThroughTimeout)
		err := batch.query(bounded, f, chunk)
		cancel()
		if err != nil {
			return reads, err
		}
	}
	return reads, nil
}

// commitPullsRead is one call of readThroughCommitsPulls: what each of its queries needs, and the
// answers it collects. The request's own writes are no part of it: each read and write is made
// through the facts its caller hands down, which no type of this package holds past its request
// (plan D32, read_only_fold_test.go's inventory).
type commitPullsRead struct {
	attributor  *Attributor
	client      *githubapp.Client
	token       string
	canonical   string
	storedUnder string
	owner, repo string
	owed        map[int][]owedRead
	now         time.Time
	reads       map[string]CommitPullsRead
}

// query asks GitHub about shas in one request and stores what it answers through f, splitting the
// batch where the answer is above the client's limit.
func (c *commitPullsRead) query(ctx context.Context, f facts, shas []string) error {
	if len(shas) == 0 {
		return nil
	}
	text, variables := commitPullsQuery(c.owner, c.repo, shas)
	began, err := f.startRead(ctx)
	if err != nil {
		return readThroughNotStored(err)
	}
	body, status, _, err := c.client.GraphQL(ctx, c.token, text, variables)
	var tooLarge *githubapp.ResponseTooLargeError
	switch {
	case errors.As(err, &tooLarge) && len(shas) == 1:
		return c.overREST(ctx, f, shas[0])
	case errors.As(err, &tooLarge):
		half := len(shas) / 2
		if err := c.query(ctx, f, shas[:half]); err != nil {
			return err
		}
		return c.query(ctx, f, shas[half:])
	case err != nil:
		return fmt.Errorf("query the pull requests of %d commits of %s: %w", len(shas), c.canonical, err)
	case status != http.StatusOK:
		return fmt.Errorf("query the pull requests of %d commits of %s: status %d: %s",
			len(shas), c.canonical, status, githubapp.ShownBody(body))
	}
	answered, err := decodeCommitPulls(body, shas)
	if err != nil {
		return err
	}
	for _, sha := range shas {
		pulls, found := answered[sha]
		if !found {
			// GitHub answered the repository null, or named this commit nothing at all: no
			// answer about it, so the commit keeps whatever the stored rows say.
			continue
		}
		read, err := c.store(ctx, f, sha, pulls, began)
		if err != nil {
			// sha's answer is not stored in full, so sha is no commit this read learned: it stays
			// out of the answer, and its caller reads the error for it.
			return err
		}
		c.reads[sha] = read
	}
	return nil
}

// overREST reads one commit whose answer alone is above the client's limit through the REST
// read-through, which asks for that commit's pull requests alone and stores them the same way.
func (c *commitPullsRead) overREST(ctx context.Context, f facts, sha string) error {
	written, atHead, err := c.attributor.readThroughCommitPulls(ctx, f, c.client, c.storedUnder, sha, c.now)
	if err != nil {
		return fmt.Errorf("read the pull requests of %s@%s, whose answer is above the limit of one query: %w", c.canonical, sha, err)
	}
	c.reads[sha] = CommitPullsRead{Written: written, AtHead: atHead}
	return nil
}

// store writes the facts of each pull request GitHub named for sha, as readThroughCommitPulls
// writes them for its one commit: a closed pull request Dispatch holds no row of gets none, each
// other is stored through the one writer (storePullRequestFacts), and a stored pull request
// settles the read it was owed when this read began.
func (c *commitPullsRead) store(ctx context.Context, f facts, sha string, pulls []pullRequestPayload, began readStart) (CommitPullsRead, error) {
	var read CommitPullsRead
	for _, pull := range pulls {
		if pull.Head.SHA == sha {
			read.AtHead = true
		}
		if PullRequestHeadState(pull.State, pull.Merged || pull.MergedAt != nil) == "closed" {
			var held bool
			if err := f.read(ctx, func(q refs.Pool) error {
				var err error
				held, err = c.attributor.pullRequestHeld(ctx, q, c.canonical, pull.Number)
				return err
			}); err != nil {
				return read, readThroughNotStored(err)
			}
			if !held {
				continue
			}
		}
		if _, err := c.attributor.storePullRequestFacts(
			ctx, f, c.client, c.token, c.canonical, c.storedUnder, pull, &c.now, began, false, false,
		); err != nil {
			if limited, ok := githubapp.AsRateLimit(err); ok {
				return read, limited
			}
			return read, readThroughNotStored(err)
		}
		read.Written++
		for _, owed := range c.owed[pull.Number] {
			if _, err := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
				return false, settleOwedRead(ctx, db, owed.Repo, owed.Number, owed.Revision)
			}); err != nil {
				logging.FromContext(ctx).Warn("dispatch delivery: settle the read owed before this read-through; the reconcile reads the pull request once more",
					"repo", owed.Repo, "number", owed.Number, "error", err)
			}
		}
	}
	return read, nil
}

// commitPullsQuery is the text and the variables of the query of shas in one repository: one
// alias c<i> for each commit, in the order given, each answering the pull requests GitHub names
// for it with the fields every reader of a pull request reads (pullRequestFacts). The owner and
// the name go in variables; each commit goes in the text, since an alias cannot take one.
//
// A commit the repository does not hold answers null for its alias, which is no answer about it,
// as the REST read's own empty answer for such a commit is none.
func commitPullsQuery(owner, repo string, shas []string) (string, map[string]any) {
	var text strings.Builder
	text.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for index, sha := range shas {
		fmt.Fprintf(&text, "    c%d: object(oid: %q) { ... on Commit { associatedPullRequests(first: %d) { nodes { ...pullRequestFacts } } } }\n",
			index, sha, CommitPullsPerCommit)
	}
	text.WriteString("  }\n}\n")
	text.WriteString(factsFields)
	return text.String(), map[string]any{"owner": owner, "name": repo}
}

// decodeCommitPulls reads GitHub's answer to one query of shas: the pull requests of each commit
// the answer names, by commit, in the shape every writer of a head row takes
// (pullRequestPayload). A commit whose alias is null, and one whose alias is no commit, is absent
// from the answer: GitHub said nothing about it, which is not the same as naming no pull request
// for it.
func decodeCommitPulls(body []byte, shas []string) (map[string][]pullRequestPayload, error) {
	var answer struct {
		Data struct {
			Repository map[string]json.RawMessage `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("decode the answer to the query of %d commits' pull requests: %w", len(shas), err)
	}
	if answer.Data.Repository == nil {
		return nil, fmt.Errorf("GitHub answered the query of %d commits' pull requests with no repository: %s",
			len(shas), githubapp.ShownBody(body))
	}
	answered := make(map[string][]pullRequestPayload, len(shas))
	for index, sha := range shas {
		raw := answer.Data.Repository["c"+strconv.Itoa(index)]
		if isJSONNull(raw) {
			continue
		}
		var commit struct {
			AssociatedPullRequests struct {
				Nodes []factsNode `json:"nodes"`
			} `json:"associatedPullRequests"`
		}
		if err := json.Unmarshal(raw, &commit); err != nil {
			return nil, fmt.Errorf("decode the pull requests of %s: %w", sha, err)
		}
		pulls := make([]pullRequestPayload, 0, len(commit.AssociatedPullRequests.Nodes))
		for _, node := range commit.AssociatedPullRequests.Nodes {
			pulls = append(pulls, node.payload())
		}
		answered[sha] = pulls
	}
	return answered, nil
}
