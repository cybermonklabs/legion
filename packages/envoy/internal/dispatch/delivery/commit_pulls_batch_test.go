package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// commitPullsAlias reads the aliases of the query of many commits' pull requests
// (commitPullsQuery): each is `c<i>: object(oid: "<sha>")`.
var commitPullsAlias = regexp.MustCompile(`(c\d+): object\(oid: "([0-9a-f]+)"\)`)

// commitPullsFirst reads how many pull requests the query reads of each commit.
var commitPullsFirst = regexp.MustCompile(`associatedPullRequests\(first: (\d+)\)`)

// serveCommitPullsQuery makes the fake answer the batched query of commits' pull requests
// (commitPullsQuery) for repo, from the same state the REST route answers from
// (serveCommitPulls): each pull request g holds that holds the commit (holdsCommit), in GitHub's
// GraphQL shapes, oldest first, the first of them the query's `first` asks for, with their count
// as totalCount. It counts each query in queries, and each commit it answers in commits. A commit
// in absent answers null for its alias, as GitHub answers a commit the repository does not hold.
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
		first := -1
		if matched := commitPullsFirst.FindStringSubmatch(query); matched != nil {
			first, _ = strconv.Atoi(matched[1])
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
			total := 0
			g.mu.Lock()
			for _, number := range slices.Sorted(maps.Keys(g.pulls)) {
				pull := g.pulls[number]
				if !holdsCommit(pull, alias[2]) {
					continue
				}
				total++
				if first >= 0 && len(nodes) == first {
					continue
				}
				node, err := fakeFactsNode(number, maps.Clone(pull), g.messages[number], false)
				if err != nil {
					t.Errorf("the facts of pull request %d: %v", number, err)
				}
				nodes = append(nodes, node)
			}
			g.mu.Unlock()
			pulls[alias[1]] = map[string]any{"associatedPullRequests": map[string]any{"totalCount": total, "nodes": nodes}}
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

// enrichCommitPullsFixture adds to a fixture of seedCommitPullsFixture the three cases a read of a
// commit's pull requests decides apart from storing a head, and answers the fixture's commits with
// theirs after them, in the order a read asks them:
//
//   - a commit whose one pull request is closed and unmerged and Dispatch holds no row of, so a
//     read writes no row for it (Written 0);
//   - a commit a pull request holds in its history and not as its head, so a read writes that
//     pull request at its own head and answers AtHead false with Written 1;
//   - a read owed for the fixture's first pull request, which a read that stores it settles.
func enrichCommitPullsFixture(t *testing.T, ctx context.Context, pool *store.Pool, github *githubPulls, shas []string) []string {
	t.Helper()
	closed := fmt.Sprintf("%040x", 0xc105ed)
	github.set("acme/widgets", 600, "closed", closed, "legion/elsewhere", "", time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC))
	history := fmt.Sprintf("%040x", 0x415)
	github.edit(501, func(pull map[string]any) { pull["fake_history"] = []string{history} })
	if _, err := pool.Exec(ctx, `insert into pull_request_reads_owed (repo, number, reason) values ('acme/widgets', 500, 'failure')`); err != nil {
		t.Fatalf("owe a read of #500: %v", err)
	}
	return append(slices.Clone(shas), closed, history)
}

// owedNumbersOf is the numbers of repo's pull requests owed a read.
func owedNumbersOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string) []int {
	t.Helper()
	rows, err := pool.Query(ctx, `select number from pull_request_reads_owed where repo = $1 order by number`, repo)
	if err != nil {
		t.Fatalf("read the owed reads of %s: %v", repo, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read the owed reads of %s: %v", repo, err)
	}
	return numbers
}

// The batched read of many commits' pull requests answers what the read of one commit at a time
// answers, for each commit, and writes the same rows: the same pull requests at the same heads,
// each credited to the same issue, and settles the same owed reads. Each of the fixture's first
// commits heads a pull request of its own, so a query whose aliases and commits are paired wrongly
// writes another pull request's head for a commit and fails this test; and the fixture holds the
// cases that decide what a read answers apart from a head (enrichCommitPullsFixture): a closed
// pull request Dispatch holds no row of, a commit a pull request holds behind its head, and a
// read owed before the read began.
//
// It asks GitHub once for every commit of a batch, where the sequential read asks once per commit.
func TestTheBatchedReadOfCommitsPullsAnswersWhatTheSequentialReadAnswers(t *testing.T) {
	t.Parallel()
	const pulls = 6
	now := time.Now()
	type outcome struct {
		heads map[int]string
		reads map[string]CommitPullsRead
		owed  []int
	}

	sequential := func(t *testing.T) outcome {
		pool, ctx := deliveryTestPool(t)
		_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 60, pulls)
		shas = enrichCommitPullsFixture(t, ctx, pool, github, shas)
		client := fake.newTestClient()
		reads := map[string]CommitPullsRead{}
		for _, sha := range shas {
			written, atHead, err := unrenamed().ReadThroughCommitPulls(ctx, pool, client, "acme/widgets", sha, now)
			if err != nil {
				t.Fatalf("the sequential read of %s: %v", sha, err)
			}
			reads[sha] = CommitPullsRead{Written: written, AtHead: atHead}
		}
		return outcome{storedHeadsOf(t, ctx, pool, "acme/widgets"), reads, owedNumbersOf(t, ctx, pool, "acme/widgets")}
	}

	batched := func(t *testing.T) (outcome, int, int) {
		pool, ctx := deliveryTestPool(t)
		_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 61, pulls)
		shas = enrichCommitPullsFixture(t, ctx, pool, github, shas)
		queries, answered := serveCommitPullsQuery(t, fake, github, "acme/widgets", nil)
		answer, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, now)
		if err != nil {
			t.Fatalf("the batched read: %v", err)
		}
		return outcome{storedHeadsOf(t, ctx, pool, "acme/widgets"), answer.Reads, owedNumbersOf(t, ctx, pool, "acme/widgets")}, *queries, *answered
	}

	want := sequential(t)
	got, queries, answered := batched(t)
	if len(want.heads) != pulls {
		t.Fatalf("the sequential read stored %d heads, want %d: the fixture proves nothing", len(want.heads), pulls)
	}
	closed, history := fmt.Sprintf("%040x", 0xc105ed), fmt.Sprintf("%040x", 0x415)
	if read := want.reads[closed]; read.Written != 0 || !read.AtHead {
		t.Fatalf("the sequential read of the closed pull request's commit = %+v, want no row written at its head: the fixture proves nothing", read)
	}
	if read := want.reads[history]; read.Written != 1 || read.AtHead {
		t.Fatalf("the sequential read of a commit behind #501's head = %+v, want one row written and not at its head: the fixture proves nothing", read)
	}
	if len(want.owed) != 0 {
		t.Fatalf("the sequential read left the reads of %v owed, want #500's settled: the fixture proves nothing", want.owed)
	}
	if fmt.Sprint(got.heads) != fmt.Sprint(want.heads) {
		t.Fatalf("the batched read stored %v, and the sequential read stored %v", got.heads, want.heads)
	}
	if fmt.Sprint(got.reads) != fmt.Sprint(want.reads) {
		t.Fatalf("the batched read answers %v, and the sequential read answers %v", got.reads, want.reads)
	}
	if fmt.Sprint(got.owed) != fmt.Sprint(want.owed) {
		t.Fatalf("the batched read left the reads of %v owed, and the sequential read %v", got.owed, want.owed)
	}
	if queries != 1 || answered != pulls+2 {
		t.Fatalf("the batched read sent %d queries for %d commits, and they answered %d commits; want one query answering all %d",
			queries, pulls+2, answered, pulls+2)
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
			answer, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, time.Now())
			if err != nil {
				t.Fatalf("the batched read of %d commits: %v", commits, err)
			}
			if len(answer.Reads) != commits || len(answer.Asked) != commits || *answered != commits {
				t.Fatalf("the read of %d commits answered %d of them, asked about %d, and GitHub answered %d; want every one",
					commits, len(answer.Reads), len(answer.Asked), *answered)
			}
			for _, sha := range shas {
				if read := answer.Reads[sha]; read.Written != 1 || !read.AtHead {
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

// A query that fails drops no commit of its batch silently: the error comes back, the commits the
// read did answer are in its answer, and the commits the failed query named are asked, so a caller
// can tell the commits it learned from those GitHub failed and from those it was never asked
// about. The read stops at the failure, so a batch after it is asked nothing: of 21 commits, GitHub
// answering 502 to the first batch's query, the 20 of that batch are asked and the 21st is not. The
// control is the same read with GitHub answering: every commit is asked, read and stored.
func TestABatchedReadThatFailsDropsNoCommitSilently(t *testing.T) {
	t.Parallel()
	const commits = CommitPullsBatch + 1
	run := func(t *testing.T, failing bool) (CommitsPullsAnswer, error, map[int]string, []string) {
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
		answer, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, time.Now())
		return answer, err, storedHeadsOf(t, ctx, pool, "acme/widgets"), shas
	}

	answer, err, stored, shas := run(t, true)
	if err == nil {
		t.Fatal("a query GitHub answered 502 ended with no error, so a caller would read its commits as answered")
	}
	if len(answer.Reads) != 0 || len(stored) != 0 {
		t.Fatalf("a query GitHub answered 502 answered %v and stored %v, want neither", answer.Reads, stored)
	}
	for index, sha := range shas {
		if asked := answer.Asked[sha]; asked != (index < CommitPullsBatch) {
			t.Fatalf("commit %d of %d is asked %t; want the %d of the failed batch asked and the one after it not, since the read stopped at the failure",
				index+1, commits, asked, CommitPullsBatch)
		}
	}
	control, err, controlStored, _ := run(t, false)
	if err != nil || len(control.Reads) != commits || len(control.Asked) != commits || len(controlStored) != commits {
		t.Fatalf("the control = %v asked %d, %v, %v; want every commit asked, read and stored",
			control.Reads, len(control.Asked), err, controlStored)
	}
}

// A query whose answer is above the client's limit is split in two and asked again, down to one
// commit, and one commit whose answer alone is too large is read over REST (overREST), so a large
// answer drops no commit. GitHub answers every query that names the fourth of six commits with a
// body past the client's limit: the read asks the six (too large), the first three (answered), the
// last three (too large), the fourth alone (too large) and then reads it over REST, then the last
// two (answered). Every commit is read and stored, and the fourth is read over REST once. The
// control is GitHub answering every query: one query, and no REST read.
func TestATooLargeAnswerIsSplitAndItsOneCommitReadOverREST(t *testing.T) {
	t.Parallel()
	const commits = 6
	run := func(t *testing.T, large bool) (CommitsPullsAnswer, error, map[int]string, []string, int, int) {
		pool, ctx := deliveryTestPool(t)
		_, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 72, commits)
		queries, _ := serveCommitPullsQuery(t, fake, github, "acme/widgets", nil)
		if large {
			answerer := fake.facts
			fake.facts = func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read the GraphQL request: %v", err)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				if bytes.Contains(body, []byte(shas[3])) {
					*queries++
					fmt.Fprintf(w, `{"data":{"repository":{}},"padding":%q}`, strings.Repeat("x", 2<<20))
					return
				}
				answerer(w, r)
			}
		}
		before := github.commitPullsReads.Load()
		answer, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, time.Now())
		return answer, err, storedHeadsOf(t, ctx, pool, "acme/widgets"), shas, *queries, int(github.commitPullsReads.Load() - before)
	}

	answer, err, stored, shas, queries, rest := run(t, true)
	if err != nil {
		t.Fatalf("the read of six commits, one of whose answers is too large, failed: %v", err)
	}
	for _, sha := range shas {
		if read := answer.Reads[sha]; read.Written != 1 || !read.AtHead || !answer.Asked[sha] {
			t.Fatalf("the read of %s = %+v (asked %t), want one row written at its head", sha, read, answer.Asked[sha])
		}
	}
	if len(stored) != commits || queries != 5 || rest != 1 {
		t.Fatalf("the read stored %d heads in %d queries and %d REST reads; want %d heads, 5 queries (6, 3, 3, 1, 2 commits) and the one too-large commit over REST",
			len(stored), queries, rest, commits)
	}
	control, err, _, _, queries, rest := run(t, false)
	if err != nil || len(control.Reads) != commits || queries != 1 || rest != 0 {
		t.Fatalf("the control read %d commits in %d queries and %d REST reads, %v; want all %d in one query",
			len(control.Reads), queries, rest, err, commits)
	}
}

// GitHub names a commit's pull requests oldest first and the query reads CommitPullsPerCommit of
// them, so a commit GitHub names more for would be read short of its newest pull requests, the
// ones a read of a result's commit exists to learn. The read asks their count, and reads such a
// commit over REST: here a commit eleven open pull requests are at, the newest #510, beside a
// commit of one. Every pull request is stored, the commit of eleven is read over REST once, and
// the other is not. The control is the same commit with ten: no REST read.
func TestACommitWithMorePullRequestsThanTheQueryReadsIsReadOverREST(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, at int) (CommitsPullsAnswer, error, map[int]string, string, int) {
		pool, ctx := deliveryTestPool(t)
		issue, fake, github, shas := seedCommitPullsFixture(t, ctx, pool, 73, 2)
		shared := shas[0]
		for number := 502; number < 500+at+1; number++ {
			github.set("acme/widgets", number, "open", shared, fmt.Sprintf("legion/%s-%d", issue, number), "",
				time.Date(2026, 3, 1, 10, number-500, 0, 0, time.UTC))
			github.edit(number, func(pull map[string]any) { pull["title"] = issue + " more work" })
		}
		serveCommitPullsQuery(t, fake, github, "acme/widgets", nil)
		before := github.commitPullsReads.Load()
		answer, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/widgets", shas, time.Now())
		return answer, err, storedHeadsOf(t, ctx, pool, "acme/widgets"), shared, int(github.commitPullsReads.Load() - before)
	}

	answer, err, stored, shared, rest := run(t, CommitPullsPerCommit+1)
	if err != nil {
		t.Fatalf("the read: %v", err)
	}
	if read := answer.Reads[shared]; read.Written != CommitPullsPerCommit+1 || !read.AtHead {
		t.Fatalf("the read of the commit %d pull requests are at = %+v, want all %d written", CommitPullsPerCommit+1, read, CommitPullsPerCommit+1)
	}
	if _, newest := stored[500+CommitPullsPerCommit]; !newest || len(stored) != CommitPullsPerCommit+2 || rest != 1 {
		t.Fatalf("the read stored %v with %d REST reads; want the newest #%d among %d heads, and one REST read",
			stored, rest, 500+CommitPullsPerCommit, CommitPullsPerCommit+2)
	}
	control, err, controlStored, shared, rest := run(t, CommitPullsPerCommit)
	if err != nil || control.Reads[shared].Written != CommitPullsPerCommit || len(controlStored) != CommitPullsPerCommit+1 || rest != 0 {
		t.Fatalf("the control = %+v, %v, %d heads, %d REST reads; want %d written from the query and no REST read",
			control.Reads[shared], err, len(controlStored), rest, CommitPullsPerCommit)
	}
}

// A repository the App cannot read writes nothing and answers the App's own error, as the read of
// one commit does: the caller leaves each result where its stored rows leave it, and gives each
// claim back, since no question named a commit.
func TestABatchedReadOfAnUnreadableRepositoryWritesNothing(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	_, fake, _, shas := seedCommitPullsFixture(t, ctx, pool, 71, 2)
	fake.answerInstallation("acme/hidden", http.StatusNotFound)
	answer, err := unrenamed().ReadThroughCommitsPulls(ctx, pool, fake.newTestClient(), "acme/hidden", shas, time.Now())
	if !RepositoryUnreadable(err) {
		t.Fatalf("the batched read of a repository the App cannot read = %v, want %v", err, githubapp.ErrRepositoryUnreadable)
	}
	if len(answer.Reads) != 0 || len(answer.Asked) != 0 {
		t.Fatalf("it answered %v and asked about %v, want nothing", answer.Reads, answer.Asked)
	}
}
