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
// What the answers mean is the REST read's: the rows written, which pull requests get one, the
// settles, and what a commit no pull request heads leaves open (ReadThroughCommitPulls's doc
// comment says it in full). The query reads the first CommitPullsPerCommit pull requests GitHub
// names for a commit, oldest first, and their count; a commit GitHub names more for is read over
// REST, as an answer too large for one query is.

// CommitPullsBatch is the most commits one query asks about. GitHub prices a query by its
// connections and refuses an answer above the client's limit, which a batch of this size has
// room under: each commit answers at most CommitPullsPerCommit pull requests, each with the
// fields of its head row and its texts (commitPullsFields) and no commit message. A read of more
// commits sends a query for each batch of this many.
const CommitPullsBatch = 20

// CommitPullsPerCommit is the most pull requests the query reads of one commit. GitHub names
// every open and merged pull request whose head or history holds the commit; a result's commit
// is behind a handful at most. GitHub lists them oldest first, so a commit it names more for is
// read over REST rather than read short of its newest pull requests (commitPulls.more).
const CommitPullsPerCommit = 10

// CommitPullsRead is what one commit of a batched read-through learned: how many rows were
// written for it, and whether a pull request GitHub named for it has it as its head, which the
// caller settles the pair on (CommitPullReads.Settle). It is the pair ReadThroughCommitPulls
// answers for one commit.
type CommitPullsRead struct {
	Written int
	AtHead  bool
}

// CommitsPullsAnswer is what one batched read-through answers. Reads is what each commit learned,
// by commit, and holds only a commit whose answer was stored in full. Asked is each commit whose
// claim an answer or a failure of GitHub's stands behind: each commit of a query GitHub failed,
// each commit of an answered query up to and with the one the read stopped at, and a commit read
// over REST. A query whose answer was too large asks about none until its halves are asked. A
// commit Asked does not hold has no answer of GitHub's behind a claim of it: no question named it,
// or GitHub answered it and the read stopped before storing that answer.
//
// Failed is each commit Asked holds whose read GitHub failed: each commit of a query GitHub
// failed or answered with what could not be read, a commit whose alias GitHub's answer names an
// error for or answers with no list of its pull requests, a commit whose REST read GitHub failed,
// and a commit whose answer was cut short by GitHub's rate limit on a pull request's commit messages.
// Its claim stands, and the read is no answer about the commit: the caller reads a result there
// as a read that failed, never as what the stored rows alone say. A commit whose answer Dispatch's
// own database could not store is in neither Reads nor Failed, and the error says so
// (ErrReadThroughNotStored).
type CommitsPullsAnswer struct {
	Reads  map[string]CommitPullsRead
	Asked  map[string]bool
	Failed map[string]bool
}

// ReadThroughCommitsPulls teaches Dispatch the pull requests GitHub names for each of shas, in
// repoFull, and stores each one's facts, as ReadThroughCommitPulls does for one commit: it
// answers what each commit learned, by commit (CommitsPullsAnswer.Reads). Only a commit whose
// answer was stored in full is in Reads. A commit the read did not reach is absent, and so is the
// commit whose answer it could not store in full, whatever rows it wrote for it first, as
// ReadThroughCommitPulls answers that commit with its error. For a commit absent from Reads that
// Asked holds, the error says why, and only ErrReadThroughNotStored gives its claim back: a failure
// of GitHub's, its rate limit on a pull request's commit messages among them, keeps it, as it keeps
// the claim of the read of one commit GitHub failed, and names the commit in Failed. A commit
// Asked does not hold gives its claim back whatever the error, since no answer of GitHub's about it
// is stored or failed.
//
// The commits are asked in batches of CommitPullsBatch, each one query. A batch GitHub's answer
// is too large for is split in two and asked again, and one commit whose answer alone is too
// large is read over REST (readThroughCommitPulls), as the facts query falls back to a REST read
// of one pull request. The first failure ends the read, with what the commits before it learned:
// a failure of GitHub's is most often one the next batch meets too (a rate limit, an outage), and
// the commits after it are given back to the next read rather than asked into the same failure.
// The read starts no batch once ctx is done — its deadline passed or its caller hung up — and
// ends with the commits from there on unasked, so a caller bounds the whole read by the deadline
// it gives: a batch already started runs to its own bound.
//
// A writer's read calls this. A read-only caller's read of an issue's results stores nothing and
// calls AnswerCommitsPulls, which is this read with its writes kept for the request.
func (a *Attributor) ReadThroughCommitsPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client,
	repoFull string, shas []string, now time.Time,
) (CommitsPullsAnswer, error) {
	return a.readThroughCommitsPulls(ctx, facts{pool: pool}, client, repoFull, shas, now)
}

// AnswerCommitsPulls is ReadThroughCommitsPulls for a read-only caller's results read, which
// stores nothing and keeps nothing after its request (plan D32): each of its statements runs in a
// transaction of pool that first makes the writes answers holds and is rolled back, and each
// write it makes is kept in answers (facts, HeadAnswers), as readThroughCommitPulls keeps a
// read-only caller's writes for one commit.
func (a *Attributor) AnswerCommitsPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, answers *HeadAnswers,
	repoFull string, shas []string, now time.Time,
) (CommitsPullsAnswer, error) {
	return a.readThroughCommitsPulls(ctx, facts{pool: pool, answers: answers}, client, repoFull, shas, now)
}

// readThroughCommitsPulls is the two above through f (facts): one read of the repository's token
// and of the reads owed in it, then a query for each batch of commits.
//
// The reads run on a context its caller's end cannot cancel, as the REST read-through of one
// commit does: a client that hangs up must not cut a read or a write short and leave its claims
// standing with nothing asked or stored. The read of the token and of the owed reads, and each
// batch with the REST reads of its commits, are each bounded by ReadThroughTimeout instead. ctx
// itself decides only whether another batch starts.
func (a *Attributor) readThroughCommitsPulls(
	ctx context.Context, f facts, client *githubapp.Client, repoFull string, shas []string, now time.Time,
) (CommitsPullsAnswer, error) {
	answer := CommitsPullsAnswer{Reads: map[string]CommitPullsRead{}, Asked: map[string]bool{}, Failed: map[string]bool{}}
	if len(shas) == 0 {
		return answer, nil
	}
	whole := ctx
	ctx = context.WithoutCancel(ctx)
	canonical := a.canonical(repoFull)
	if whole.Err() != nil {
		return answer, fmt.Errorf("asked nothing about %d commits of %s: %w", len(shas), canonical, context.Cause(whole))
	}
	bounded, cancel := context.WithTimeout(ctx, ReadThroughTimeout)
	owner, repo, token, err := repositoryRead(bounded, client, canonical)
	if err != nil {
		cancel()
		return answer, err
	}
	var owed map[int][]owedRead
	err = f.read(bounded, func(q refs.Pool) error {
		owed = a.owedReadsIn(bounded, q, canonical)
		return nil
	})
	cancel()
	if err != nil {
		return answer, readThroughNotStored(err)
	}
	batch := commitPullsRead{
		attributor: a, client: client, token: token,
		canonical: canonical, storedUnder: repoFull, owner: owner, repo: repo,
		owed: owed, now: now, answer: answer,
	}
	for at, chunk := range slices.Collect(slices.Chunk(shas, CommitPullsBatch)) {
		if whole.Err() != nil {
			return answer, fmt.Errorf("asked nothing about the last %d of %d commits of %s: %w",
				len(shas)-at*CommitPullsBatch, len(shas), canonical, context.Cause(whole))
		}
		bounded, cancel := context.WithTimeout(ctx, ReadThroughTimeout)
		err := batch.query(bounded, f, chunk)
		cancel()
		if err != nil {
			return answer, err
		}
	}
	return answer, nil
}

// commitPullsRead is one call of readThroughCommitsPulls: what each of its queries needs, and the
// answer it collects. The request's own writes are no part of it: each read and write is made
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
	answer      CommitsPullsAnswer
}

// query asks GitHub about shas in one request and stores what it answers through f, splitting the
// batch where the answer is above the client's limit. A commit is asked (CommitsPullsAnswer.Asked)
// once an answer or a failure of GitHub's stands behind it: each commit of a query GitHub failed,
// and each commit of an answered query that the store loop reached, in order. A query whose answer
// was too large asks about its commits only through its halves, and the commits after the one the
// loop stopped at are not asked: GitHub answered them, but nothing of that answer was stored, so
// no answer of GitHub's is behind their claims and the caller gives them back.
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
	if errors.As(err, &tooLarge) {
		if len(shas) == 1 {
			return c.overREST(ctx, f, shas[0])
		}
		half := len(shas) / 2
		if err := c.query(ctx, f, shas[:half]); err != nil {
			return err
		}
		return c.query(ctx, f, shas[half:])
	}
	failed := func() {
		for _, sha := range shas {
			c.answer.Asked[sha] = true
			c.answer.Failed[sha] = true
		}
	}
	switch {
	case err != nil:
		failed()
		return fmt.Errorf("query the pull requests of %d commits of %s: %w", len(shas), c.canonical, err)
	case status != http.StatusOK:
		failed()
		return fmt.Errorf("query the pull requests of %d commits of %s: status %d: %s",
			len(shas), c.canonical, status, githubapp.ShownBody(body))
	}
	answered, err := decodeCommitPulls(body, shas)
	if err != nil {
		// GitHub's answer could not be read: a failure of GitHub's behind every commit it named.
		failed()
		return err
	}
	for _, sha := range shas {
		// GitHub answered sha: from here its claim stands on that answer, stored below or read
		// over REST, or on GitHub's failure of that REST read. An exit below leaves every commit
		// after sha unasked.
		c.answer.Asked[sha] = true
		pulls := answered[sha]
		switch {
		case pulls.failed:
			// GitHub's answer names an error at this commit's alias, or holds no list of its pull
			// requests: a failure of GitHub's behind this commit alone, and the rest of the
			// answer stands.
			c.answer.Failed[sha] = true
			continue
		case pulls.more:
			// GitHub names more pull requests for the commit than the query reads of it, oldest
			// first, so the ones it left out are the newest: the commit is read over REST, which
			// reads them as the read of one commit does.
			if err := c.overREST(ctx, f, sha); err != nil {
				return err
			}
			continue
		}
		read, err := c.store(ctx, f, sha, pulls.pulls, began)
		if err != nil {
			// sha's answer is not stored in full, so sha is no commit this read learned: it stays
			// out of the answer, and its caller reads the error for it. GitHub's rate limit on a
			// pull request's commit messages, which cut the answer short, is a failure of GitHub's.
			if !errors.Is(err, ErrReadThroughNotStored) {
				c.answer.Failed[sha] = true
			}
			return err
		}
		c.answer.Reads[sha] = read
	}
	return nil
}

// overREST reads one commit over the REST read-through, which asks for that commit's pull requests
// alone and stores them the same way: a commit whose answer alone is above the client's limit, and
// one GitHub names more pull requests for than the query reads (CommitPullsPerCommit).
func (c *commitPullsRead) overREST(ctx context.Context, f facts, sha string) error {
	c.answer.Asked[sha] = true
	written, atHead, err := c.attributor.readThroughCommitPulls(ctx, f, c.client, c.storedUnder, sha, c.now)
	if err != nil {
		if !errors.Is(err, ErrReadThroughNotStored) {
			c.answer.Failed[sha] = true
		}
		return fmt.Errorf("read the pull requests of %s@%s over REST: %w", c.canonical, sha, err)
	}
	c.answer.Reads[sha] = CommitPullsRead{Written: written, AtHead: atHead}
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
// alias c<i> for each commit, in the order given, each answering how many pull requests GitHub
// names for it and the first CommitPullsPerCommit of them with the fields of commitPullsFields.
// The owner and the name go in variables; each commit goes in the text, since an alias cannot take
// one.
//
// A commit the repository does not hold answers null for its alias, with no error naming it, as
// GitHub answered such a commit on 2026-11-28: that is GitHub's answer that no pull request names
// the commit, as the REST read's 422 for it answers an empty list (fetchCommitPullPayloads).
func commitPullsQuery(owner, repo string, shas []string) (string, map[string]any) {
	var text strings.Builder
	text.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for index, sha := range shas {
		fmt.Fprintf(&text, "    c%d: object(oid: %q) { ... on Commit { associatedPullRequests(first: %d) { totalCount nodes { ...pullRequestFacts } } } }\n",
			index, sha, CommitPullsPerCommit)
	}
	text.WriteString("  }\n}\n")
	text.WriteString(commitPullsFields)
	return text.String(), map[string]any{"owner": owner, "name": repo}
}

// commitPulls is one commit of decodeCommitPulls' answer: the pull requests GitHub named for it,
// and more where it names more of them than the query read (totalCount above the nodes), or failed
// where GitHub's answer is no answer about it.
type commitPulls struct {
	pulls  []pullRequestPayload
	more   bool
	failed bool
}

// decodeCommitPulls reads GitHub's answer to one query of shas: the pull requests of each commit,
// by commit, in the shape every writer of a head row takes (pullRequestPayload). It reads `errors`
// beside `data`, as the facts batch does (factsRead.decode):
//   - an error whose path names a commit's alias (`repository`, `c<i>`, and anything below it)
//     fails that commit alone, whatever its alias holds;
//   - an error whose path names no commit's alias fails the whole answer, since no commit of it
//     can be told apart from the one GitHub failed;
//   - a commit whose alias is null with no error is one the repository does not hold, and an alias
//     `{}` names an object that is no commit, as GitHub answers a tree's id: GitHub names no pull
//     request for either, an answer, as the REST read answers both;
//   - a commit whose associatedPullRequests is null, or whose alias the answer leaves out, has no
//     answer about its pull requests, which fails it.
func decodeCommitPulls(body []byte, shas []string) (map[string]commitPulls, error) {
	var answer struct {
		Data struct {
			Repository map[string]json.RawMessage `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Path    []any  `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("decode the answer to the query of %d commits' pull requests: %w", len(shas), err)
	}
	if answer.Data.Repository == nil {
		return nil, fmt.Errorf("GitHub answered the query of %d commits' pull requests with no repository: %s",
			len(shas), githubapp.ShownBody(body))
	}
	refused := map[string]bool{}
	for _, refusal := range answer.Errors {
		var alias string
		if len(refusal.Path) >= 2 && refusal.Path[0] == "repository" {
			alias, _ = refusal.Path[1].(string)
		}
		if alias == "" {
			return nil, fmt.Errorf("GitHub answered the query of %d commits' pull requests with an error that names no commit (path %v): %s",
				len(shas), refusal.Path, refusal.Message)
		}
		refused[alias] = true
	}
	answered := make(map[string]commitPulls, len(shas))
	for index, sha := range shas {
		alias := "c" + strconv.Itoa(index)
		raw, named := answer.Data.Repository[alias]
		switch {
		case refused[alias] || !named:
			// An error names the alias, or the answer leaves it out: no answer about the commit.
			answered[sha] = commitPulls{failed: true}
			continue
		case isJSONNull(raw):
			answered[sha] = commitPulls{}
			continue
		}
		var commit map[string]json.RawMessage
		if err := json.Unmarshal(raw, &commit); err != nil {
			return nil, fmt.Errorf("decode the pull requests of %s: %w", sha, err)
		}
		connection, isCommit := commit["associatedPullRequests"]
		switch {
		case !isCommit:
			// `... on Commit` selected nothing: the object is no commit, which no pull request heads.
			answered[sha] = commitPulls{}
			continue
		case isJSONNull(connection):
			answered[sha] = commitPulls{failed: true}
			continue
		}
		var listed struct {
			TotalCount int         `json:"totalCount"`
			Nodes      []factsNode `json:"nodes"`
		}
		if err := json.Unmarshal(connection, &listed); err != nil {
			return nil, fmt.Errorf("decode the pull requests of %s: %w", sha, err)
		}
		pulls := make([]pullRequestPayload, 0, len(listed.Nodes))
		for _, node := range listed.Nodes {
			pulls = append(pulls, node.payload())
		}
		answered[sha] = commitPulls{pulls: pulls, more: listed.TotalCount > len(listed.Nodes)}
	}
	return answered, nil
}
