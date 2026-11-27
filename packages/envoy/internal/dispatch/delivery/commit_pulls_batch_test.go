package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// commitPullsAlias reads the aliases of the query of many commits' pull requests
// (commitPullsQuery): each is `c<i>: object(oid: "<sha>")`.
var commitPullsAlias = regexp.MustCompile(`(c\d+): object\(oid: "([0-9a-f]+)"\)`)

// serveCommitPullsQuery makes the fake answer the batched query of commits' pull requests
// (commitPullsQuery) for repo, from the same state the REST route answers from
// (serveCommitPulls): each pull request g holds whose head is the commit, in GitHub's GraphQL
// shapes. It counts each query in queries, and each commit it answers in commits. A commit in
// absent answers null for its alias, as GitHub answers a commit the repository does not hold.
//
// The fake's own GraphQL handler answers the batched query of facts, which carries the same
// fragment, so this one is set as the test's facts handler and passes any query that names no
// `object(oid:` alias back to it (fakeGitHub.answerFactsQuery).
func serveCommitPullsQuery(t *testing.T, fake *fakeGitHub, g *githubPulls, repo string, absent map[string]bool) (queries, commits *int) {
	t.Helper()
	asked, answered := 0, 0
	fake.facts = func(w http.ResponseWriter, r *http.Request) {
		query, _ := decodeGraphQLRequest(t, r)
		aliases := commitPullsAlias.FindAllStringSubmatch(query, -1)
		if len(aliases) == 0 {
			var body []byte
			body, _ = json.Marshal(map[string]any{"query": query})
			fake.answerFactsQuery(w, r, body)
			return
		}
		asked++
		pulls := map[string]any{}
		for _, alias := range aliases {
			if absent[alias[2]] {
				pulls[alias[1]] = nil
				continue
			}
			answered++
			nodes := []any{}
			g.mu.Lock()
			for _, number := range slices.Sorted(maps.Keys(g.pulls)) {
				pull := g.pulls[number]
				head, _ := pull["head"].(map[string]any)
				if head["sha"] != alias[2] {
					continue
				}
				node, err := fakeFactsNode(number, maps.Clone(pull), g.messages[number], false)
				if err != nil {
					t.Errorf("the facts of pull request %d: %v", number, err)
				}
				nodes = append(nodes, node)
			}
			g.mu.Unlock()
			pulls[alias[1]] = map[string]any{"associatedPullRequests": map[string]any{"nodes": nodes}}
		}
		mustEncode(t, w, map[string]any{"data": map[string]any{"repository": pulls}})
	}
	return &asked, &answered
}

// seedCommitPullsFixture is the fixture both reads of the tests below run against: issue, a
// repository GitHub holds pulls pull requests of, each at a head of its own, and the commits of
// those heads in the order a read asks about them. Each pull request's title names the issue, so
// the statement credits each to it, and each head commit is that pull request's alone, so a wrong
// pairing of an alias with a commit writes the wrong head for it.
func seedCommitPullsFixture(t *testing.T, ctx context.Context, pool *store.Pool, number int, pulls int) (issue string, fake *fakeGitHub, github *githubPulls, shas []string) {
	t.Helper()
	issue = seedHeadIssue(t, ctx, pool, number)
	fake = newFakeGitHub(t)
	github = serveGitHubPulls(t, fake, "acme/widgets")
	serveCommitPulls(t, fake, github, "acme/widgets")
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for index := range pulls {
		sha := fmt.Sprintf("%040x", index+1)
		number := 500 + index
		github.set("acme/widgets", number, "open", sha, "legion/"+issue, "", at)
		github.edit(number, func(pull map[string]any) { pull["title"] = issue + " the work" })
		shas = append(shas, sha)
	}
	return issue, fake, github, shas
}

// storedHeadsOf is each pull request of repo Dispatch holds a results row of, by number, with its
// head commit: what a read-through wrote, which is what the two reads below are compared by.
func storedHeadsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string) map[int]string {
	t.Helper()
	rows, err := pool.Query(ctx, `select number, head_sha from pull_request_heads where repo = $1 order by number`, repo)
	if err != nil {
		t.Fatalf("read the stored heads of %s: %v", repo, err)
	}
	defer rows.Close()
	heads := map[int]string{}
	for rows.Next() {
		var number int
		var head string
		if err := rows.Scan(&number, &head); err != nil {
			t.Fatalf("read the stored heads of %s: %v", repo, err)
		}
		heads[number] = head
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored heads of %s: %v", repo, err)
	}
	return heads
}

// The batched read of many commits' pull requests answers what the read of one commit at a time
// answers, for each commit, and writes the same rows: the same pull requests at the same heads,
// each credited to the same issue. Each commit of the fixture heads a pull request of its own, so
// a query whose aliases and commits are paired wrongly writes another pull request's head for a
// commit and fails this test.
//
// It asks GitHub once for every commit of a batch, where the sequential read asks once per commit.
func TestTheBatchedReadOfCommitsPullsAnswersWhatTheSequentialReadAnswers(t *testing.T) {
	t.Parallel()
	const pulls = 6
	now := time.Now()

	sequential := func(t *testing.T) (map[int]string, map[string]CommitPullsRead, int) {
		pool, ctx := deliveryTestPool(t)
		_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 60, pulls)
		client := fake.newTestClient()
		reads := map[string]CommitPullsRead{}
		for _, sha := range shas {
			written, atHead, err := unrenamed().ReadThroughCommitPulls(ctx, pool, client, "acme/widgets", sha, now)
			if err != nil {
				t.Fatalf("the sequential read of %s: %v", sha, err)
			}
			reads[sha] = CommitPullsRead{Written: written, AtHead: atHead}
		}
		return storedHeadsOf(t, ctx, pool, "acme/widgets"), reads, int(github.reads.Load())
	}

	batched := func(t *testing.T) (map[int]string, map[string]CommitPullsRead, int, int) {
		pool, ctx := deliveryTestPool(t)
		_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 61, pulls)
		queries, answered := serveCommitPullsQuery(t, fake, github, "acme/widgets", nil)
		reads, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, now)
		if err != nil {
			t.Fatalf("the batched read: %v", err)
		}
		return storedHeadsOf(t, ctx, pool, "acme/widgets"), reads, *queries, *answered
	}

	wantHeads, wantReads, _ := sequential(t)
	gotHeads, gotReads, queries, answered := batched(t)
	if len(wantHeads) != pulls {
		t.Fatalf("the sequential read stored %d heads, want %d: the fixture proves nothing", len(wantHeads), pulls)
	}
	if fmt.Sprint(gotHeads) != fmt.Sprint(wantHeads) {
		t.Fatalf("the batched read stored %v, and the sequential read stored %v", gotHeads, wantHeads)
	}
	if fmt.Sprint(gotReads) != fmt.Sprint(wantReads) {
		t.Fatalf("the batched read answers %v, and the sequential read answers %v", gotReads, wantReads)
	}
	if queries != 1 || answered != pulls {
		t.Fatalf("the batched read sent %d queries for %d commits, and they answered %d commits; want one query answering all %d",
			queries, pulls, answered, pulls)
	}
}

// The batched read reads every commit it is given, however many: it sends a query for each batch
// of CommitPullsBatch and leaves no commit unread, so no reader of it needs a bound of its own.
// The edges are one commit, exactly one batch, and one commit more than a batch.
func TestTheBatchedReadAsksAboutEveryCommitInBatches(t *testing.T) {
	t.Parallel()
	for _, commits := range []int{1, CommitPullsBatch, CommitPullsBatch + 1} {
		t.Run(fmt.Sprintf("%d commits", commits), func(t *testing.T) {
			t.Parallel()
			pool, ctx := deliveryTestPool(t)
			_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 62+commits, commits)
			queries, answered := serveCommitPullsQuery(t, fake, github, "acme/widgets", nil)
			reads, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, time.Now())
			if err != nil {
				t.Fatalf("the batched read of %d commits: %v", commits, err)
			}
			if len(reads) != commits || *answered != commits {
				t.Fatalf("the read of %d commits answered %d of them and GitHub answered %d; want every one",
					commits, len(reads), *answered)
			}
			for _, sha := range shas {
				if read := reads[sha]; read.Written != 1 || !read.AtHead {
					t.Fatalf("the read of %s = %+v, want one row written at its head", sha, read)
				}
			}
			want := (commits + CommitPullsBatch - 1) / CommitPullsBatch
			if *queries != want {
				t.Fatalf("the read of %d commits sent %d queries, want %d of at most %d commits each",
					commits, *queries, want, CommitPullsBatch)
			}
			if stored := storedHeadsOf(t, ctx, pool, "acme/widgets"); len(stored) != commits {
				t.Fatalf("the read of %d commits stored %d heads, want one for each", commits, len(stored))
			}
		})
	}
}

// A query that fails drops no commit of its batch silently: the error comes back, and the commits
// the read did answer are in its answer, so a caller can tell the commits it learned from the
// commits it did not. The control is the same read with GitHub answering: every commit is read.
func TestABatchedReadThatFailsDropsNoCommitSilently(t *testing.T) {
	t.Parallel()
	const commits = 3
	run := func(t *testing.T, failing bool) (map[string]CommitPullsRead, error, map[int]string) {
		pool, ctx := deliveryTestPool(t)
		_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 70, commits)
		serveCommitPullsQuery(t, fake, github, "acme/widgets", nil)
		if failing {
			answerer := fake.facts
			fake.facts = func(w http.ResponseWriter, r *http.Request) {
				query, _ := decodeGraphQLRequest(t, r)
				if commitPullsAlias.MatchString(query) {
					w.WriteHeader(http.StatusBadGateway)
					fmt.Fprint(w, `{"message":"Server Error"}`)
					return
				}
				answerer(w, r)
			}
		}
		reads, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, time.Now())
		return reads, err, storedHeadsOf(t, ctx, pool, "acme/widgets")
	}

	reads, err, stored := run(t, true)
	if err == nil {
		t.Fatal("a query GitHub answered 502 ended with no error, so a caller would read its commits as answered")
	}
	if len(reads) != 0 || len(stored) != 0 {
		t.Fatalf("a query GitHub answered 502 answered %v and stored %v, want neither", reads, stored)
	}
	control, err, controlStored := run(t, false)
	if err != nil || len(control) != commits || len(controlStored) != commits {
		t.Fatalf("the control = %v, %v, %v; want every commit read and stored", control, err, controlStored)
	}
}

// A repository the App cannot read writes nothing and answers the App's own error, as the read of
// one commit does: the caller leaves each result where its stored rows leave it.
func TestABatchedReadOfAnUnreadableRepositoryWritesNothing(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	_, fake, _, shas := seedCommitPullsFixture(t, ctx, pool, 71, 2)
	fake.answerInstallation("acme/hidden", http.StatusNotFound)
	reads, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/hidden", shas, time.Now())
	if !RepositoryUnreadable(err) {
		t.Fatalf("the batched read of a repository the App cannot read = %v, want %v", err, githubapp.ErrRepositoryUnreadable)
	}
	if len(reads) != 0 {
		t.Fatalf("it answered %v, want nothing", reads)
	}
}
