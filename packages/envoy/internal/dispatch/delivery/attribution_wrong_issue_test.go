package delivery

// A merged pull request, #2808 of acme/tools, whose head branch names ACME-2250 and whose body
// names ACME-3044 first. A build whose rule ranked the title and the body above the branch
// credited it to ACME-3044; the rule of this build credits it to ACME-2250, from the branch.
// Two issues of one repository are made, as the older build held them: ACME-2250 (done, the
// pull request's own work) and ACME-3044 (in progress, made before the merge), both children of
// ACME-455, both with the repository acme/tools as their own.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	pr2808Repo   = "acme/tools"
	pr2808Number = 2808
	pr2808URL    = "https://github.com/acme/tools/pull/2808"
	pr2808Branch = "legion/ACME-2250"
	pr2808Head   = "7eb591d9b576102a47e1d80618c7955aa4077602"
	// pr2808Earlier is a commit of #2808's branch before its head, where ACME-2250 recorded
	// results; GitHub names #2808 for it (`GET /repos/.../commits/2a010bc1.../pulls`), so a
	// results read of ACME-2250 reads #2808 through for it.
	pr2808Earlier = "2a010bc19afcbfda8187db93d48fb73bb8abfff0"
	pr2808Merge   = "e761bc74f5d4d03ee9f42299ed749e5cb3b251c3"
	pr2808Title   = "ci-review: the model's job moves to a file of its own"
	// pr2808Right is the issue #2808's branch names; pr2808Wrong the one its body names first.
	pr2808Right = "ACME-2250"
	pr2808Wrong = "ACME-3044"
	// pr2808Judged is the read time the wrong row carried, as the older build stored it.
	pr2808Judged = "2026-11-13T09:35:42.863384Z"
)

// pr2808Body has the keys of #2808's body in the order they first appear there (ACME-3044,
// ACME-2250, ACME-455, ACME-519, ACME-2199, ACME-1522, ACME-49) and its four bare references
// (#3091, #2493, #2287, #3018). The title names no key. The body's first 2048 runes, which a
// `pull_request` envelope carries (contracts' capBody), name ACME-3044 alone.
var pr2808Body = "## What this changes\n\nThe model's job runs in a file of its own. " +
	"The ref credential ACME-3044 names stays a risk until that issue lands.\n\n" +
	strings.Repeat("A paragraph of the description with no key in it. ", 50) + "\n\n" +
	"Dispatch: ACME-2250, a child of ACME-455. It follows ACME-519 and ACME-2199, and the " +
	"attribution of ACME-1522; the reviewer failure is ACME-49. See #3091, #2493, #2287 and #3018.\n"

// seedPR2808Issues makes project ACME, maps acme/tools to it as its default, and makes every
// issue #2808's body names, as the older build held them.
func seedPR2808Issues(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	seedProjectRepository(t, ctx, pool, "ACME", "Acme", pr2808Repo)
	type issue struct {
		key, status, parent, repo string
		closed                    bool
	}
	for _, seeded := range []issue{
		{key: "ACME-455", status: "in_progress"},
		{key: "ACME-2250", status: "done", parent: "ACME-455", repo: pr2808Repo, closed: true},
		{key: "ACME-3044", status: "in_progress", parent: "ACME-455", repo: pr2808Repo},
		{key: "ACME-519", status: "todo"},
		{key: "ACME-2199", status: "todo"},
		{key: "ACME-1522", status: "todo"},
		{key: "ACME-49", status: "todo"},
	} {
		var number int
		if _, err := fmt.Sscanf(seeded.key, "ACME-%d", &number); err != nil {
			t.Fatalf("issue key %q: %v", seeded.key, err)
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, created_by, rank, parent_key, repo, closed_at)
			values ($1, 'ACME', $2, $1, $3, '{"kind":"system","id":"test"}'::jsonb, $1,
				nullif($4, ''), nullif($5, ''), case when $6 then '2026-11-11T23:10:28Z'::timestamptz end)
			on conflict do nothing
		`, seeded.key, number, seeded.status, seeded.parent, seeded.repo, seeded.closed); err != nil {
			t.Fatalf("seed issue %s: %v", seeded.key, err)
		}
	}
}

// pr2808Payload is #2808 as GitHub answers it: the single form of `GET .../pulls/2808`, or the
// list form of `GET .../commits/{sha}/pulls`, which carries merged_at and no merged.
func pr2808Payload(listForm bool) map[string]any {
	payload := map[string]any{
		"number": pr2808Number, "title": pr2808Title, "body": pr2808Body, "html_url": pr2808URL,
		"state": "closed", "merged_at": "2026-11-11T23:10:25Z", "updated_at": "2026-11-12T00:49:50Z",
		"merge_commit_sha": pr2808Merge, "user": map[string]any{"login": "legion-implementer[bot]"},
		"head": map[string]any{
			"ref": pr2808Branch, "sha": pr2808Head,
			"repo": map[string]any{"full_name": pr2808Repo},
		},
		"base": map[string]any{"ref": "main"},
	}
	if !listForm {
		payload["merged"] = true
	}
	return payload
}

// pr2808Reads counts the REST reads of #2808's fake (restRead): the reads of a pull request and
// the pages of its commit messages. The messages are the fifth source, and two of #2808's 32
// commit messages hold ACME-3044. The fake counts a batched query (readFactsBatch) apart.
type pr2808Reads struct{ pulls, commitPages atomic.Int32 }

// pr2808GitHub answers #2808's three reads the writers make, and counts its REST reads.
func pr2808GitHub(t *testing.T) (*fakeGitHub, *pr2808Reads) {
	t.Helper()
	fake := newFakeGitHub(t)
	fake.installationRepos = []string{pr2808Repo}
	reads := &pr2808Reads{}
	fake.handle("GET /repos/acme/tools/pulls/2808", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			reads.pulls.Add(1)
		}
		mustEncode(t, w, pr2808Payload(false))
	})
	fake.handle("GET /repos/acme/tools/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{pr2808Payload(true)})
	})
	fake.handle("GET /repos/acme/tools/pulls/2808/commits", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			reads.commitPages.Add(1)
		}
		mustEncode(t, w, []any{
			map[string]any{"sha": pr2808Head, "commit": map[string]any{"message": "ci-review: the split\n\nDispatch: ACME-3044", "author": map[string]any{"date": "2026-11-09T12:00:00Z"}}},
		})
	})
	return fake, reads
}

// pr2808Envelope is the `pull_request` envelope of #2808's merge, as contracts' githubPayload
// makes it from GitHub's webhook.
func pr2808Envelope() map[string]string {
	return map[string]string{
		"kind": "pr", "action": "closed", "repo": pr2808Repo, "number": "2808",
		"title": pr2808Title, "author": "legion-implementer[bot]", "url": pr2808URL,
		"head_sha": pr2808Head, "head_ref": pr2808Branch, "head_repo": pr2808Repo, "base_ref": "main",
		"merged": "true", "merge_commit_sha": pr2808Merge, "merged_by": "legion-merger[bot]",
		"updated_at": "2026-11-11T23:10:25Z", "body": pr2808Body,
	}
}

// storePR2808AsAnOlderRuleJudgedIt writes #2808's row as the older build left it: credited to
// ACME-3044 from the title and the body, read at pr2808Judged. The row has the texts of GitHub's
// answer, as a writer of this build stores them, and the older build's verdict is stored with one
// update, as that build stored it.
func storePR2808AsAnOlderRuleJudgedIt(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	updated := time.Date(2026, 11, 12, 0, 49, 50, 0, time.UTC)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: pr2808Repo, Number: pr2808Number, HeadSHA: pr2808Head, HeadRef: pr2808Branch,
		BaseRef: "main", State: "merged", Title: pr2808Title, URL: pr2808URL, EventAt: &updated,
		Texts: &PullRequestTexts{HeadRepo: pr2808Repo, BaseRef: "main", Title: pr2808Title, Body: pr2808Body,
			CommitMessages: []string{"ci-review: the split\n\nDispatch: ACME-3044"}, MessagesRead: true},
	}); err != nil {
		t.Fatalf("store #2808 as the older rule judged it: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update pull_request_heads set issue_key = $4, attribution_source = $5, checked_at = $3::timestamptz
		where repo = $1 and number = $2
	`, pr2808Repo, pr2808Number, pr2808Judged, pr2808Wrong, string(sourceTitleBody)); err != nil {
		t.Fatalf("store the older rule's verdict of #2808: %v", err)
	}
}

// pr2808Credit is the issue and the source that every reader answers for #2808
// (PullRequestIssues), "none" for nobody.
func pr2808Credit(t *testing.T, ctx context.Context, pool *store.Pool) (string, string) {
	t.Helper()
	head := readHead(t, ctx, pool, pr2808Repo, pr2808Number)
	if head.IssueKey == nil {
		return "none", head.AttributionSource
	}
	return *head.IssueKey, head.AttributionSource
}

func dropPR2808(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `delete from pull_request_heads where repo = $1 and number = $2`, pr2808Repo, pr2808Number); err != nil {
		t.Fatalf("drop #2808's row: %v", err)
	}
}

// Every writer of pull_request_heads, given #2808's true facts, credits ACME-2250 from the head
// branch (rank 3), above ACME-3044, the first key of the body (rank 4), and reads no commit
// message over REST, since rank 3 already holds a candidate. A writer that ranks the body above
// the branch, or one that pages the messages, makes this test fail.
func TestPR2808EveryWriterGivesTheBranchsIssue(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedPR2808Issues(t, ctx, pool)
	fake, reads := pr2808GitHub(t)
	client := fake.newTestClient()
	broker := events.NewBroker()

	for _, writer := range []struct {
		name  string
		write func(t *testing.T)
	}{
		{"the pull_request envelope of the merge", func(t *testing.T) {
			intake := newIntake(nil, pool, nil, client, broker, nil)
			if _, err := intake.recordPullRequestHead(ctx, pr2808Envelope(), pr2808Number); err != nil {
				t.Fatalf("record the envelope's head: %v", err)
			}
		}},
		{"the read-through of the head commit, which a recorded result makes", func(t *testing.T) {
			if _, _, err := ReadThroughCommitPulls(ctx, pool, client, pr2808Repo, pr2808Head, time.Now()); err != nil {
				t.Fatalf("read the head commit through: %v", err)
			}
		}},
		{"the read-through of an earlier commit of the branch, which a results read makes", func(t *testing.T) {
			if _, _, err := ReadThroughCommitPulls(ctx, pool, client, pr2808Repo, pr2808Earlier, time.Now()); err != nil {
				t.Fatalf("read the earlier commit through: %v", err)
			}
		}},
		{"the reconcile, judging again the row an older rule credited", func(t *testing.T) {
			storePR2808AsAnOlderRuleJudgedIt(t, ctx, pool)
			if err := NewReconcile(pool, client, broker).reconcilePullRequestHeads(ctx, time.Now()); err != nil {
				t.Fatalf("reconcile the pull request heads: %v", err)
			}
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			dropPR2808(t, ctx, pool)
			reads.commitPages.Store(0)
			writer.write(t)
			issue, source := pr2808Credit(t, ctx, pool)
			t.Logf("#2808 credits %s from %s", issue, source)
			if issue != pr2808Right || source != string(sourceBranch) {
				t.Errorf("#2808 credits %s from %q, want %s from the branch %s", issue, source, pr2808Right, pr2808Branch)
			}
			if pages := reads.commitPages.Load(); pages != 0 {
				t.Errorf("the write read %d pages of commit messages over REST, want none: the branch already names a held issue", pages)
			}
		})
	}
}

// A `pull_request` envelope that names no head branch but does name the head repository was a
// complete read to the envelope writer of the builds that wrote what the envelope says: the
// branch's candidate is gone, so the body's first key wins at rank 4 and the write may replace
// the stored issue. GitHub's own webhook always carries head_ref (contracts' githubPayload), so
// only another publisher of `notifications.github.` subjects sends this; any Envoy /v1 bearer
// may. This build writes nothing the envelope says and reads the pull request from GitHub.
func TestPR2808AnEnvelopeWithoutItsBranchGivesTheBodysFirstKey(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedPR2808Issues(t, ctx, pool)
	fake, _ := pr2808GitHub(t)
	intake := newIntake(nil, pool, nil, fake.newTestClient(), events.NewBroker(), nil)

	envelope := pr2808Envelope()
	delete(envelope, "head_ref")
	if _, err := intake.recordPullRequestHead(ctx, envelope, pr2808Number); err != nil {
		t.Fatalf("record the envelope's head: %v", err)
	}
	issue, source := pr2808Credit(t, ctx, pool)
	t.Logf("#2808 credits %s from %s", issue, source)
	if issue != pr2808Right {
		t.Errorf("an envelope naming no branch credits #2808 to %s from %q, want %s, which GitHub's own answer names in its branch", issue, source, pr2808Right)
	}
}

// The class. A pull request's stored issue is a verdict of the rule of the build that wrote it.
// A later build reads no stored verdict of a pull request with current texts: the statement gives
// its issue from the texts at each read. One pass reads every row with no facts, and here every
// row has facts. So the pass reads nothing of GitHub, and #2808 credits its branch's issue. The
// test stores 50 merged rows before #2808, each with an older rule's stored issue. A reader that
// answers a stored verdict, or a pass that reads a row whose texts are current, makes it fail.
func TestPR2808AStoredVerdictOfAnOlderRuleStandsNoLongerThanOnePass(t *testing.T) {
	pass := judgePR2808BehindOlderRows(t, 50)
	if pass.issue != pr2808Right {
		t.Errorf("after one pass, #2808 still credits %s from %q, the verdict of an older rule; want %s from its branch",
			pass.issue, pass.source, pr2808Right)
	}
	if pass.comparedWithWrong {
		t.Errorf("the results read compares %s's before_merge checks with #2808 at %s", pr2808Wrong, pr2808Head)
	}
	if pass.restReads != 0 || pass.factsQueries != 0 {
		t.Errorf("the pass read GitHub %d times over REST and asked %d batched queries, want none: every row has current texts and read messages",
			pass.restReads, pass.factsQueries)
	}
}

// pr2808Pass is what one reconcile pass over #2808 and the rows before it leaves. It holds the
// issue and the source that #2808 credits, and whether the results read compares ACME-3044 with
// it. It also holds the reads of GitHub that the pass made, over REST and by batched queries.
type pr2808Pass struct {
	issue, source           string
	comparedWithWrong       bool
	restReads, factsQueries int32
}

// judgePR2808BehindOlderRows stores ahead merged rows, each with current texts and the issue an
// older rule stored, each read before #2808. Then it stores #2808 as the older build left it, and
// runs one reconcile pass. It counts the reads of GitHub that the pass made.
func judgePR2808BehindOlderRows(t *testing.T, ahead int) pr2808Pass {
	t.Helper()
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedPR2808Issues(t, ctx, pool)
	fake, reads := pr2808GitHub(t)
	// The rows ahead of #2808 are other merged pull requests of the repository that name no issue.
	fake.handle("GET /repos/acme/tools/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			reads.pulls.Add(1)
		}
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("pull request number %q: %v", r.PathValue("number"), err)
			return
		}
		mustEncode(t, w, map[string]any{
			"number": number, "title": "chore: tidy", "body": "", "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", pr2808Repo, number),
			"state": "closed", "merged": true, "merged_at": "2026-11-10T00:00:00Z", "updated_at": "2026-11-10T00:00:00Z",
			"head": map[string]any{"ref": fmt.Sprintf("chore/tidy-%d", number), "sha": fmt.Sprintf("%040d", number), "repo": map[string]any{"full_name": pr2808Repo}},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/tools/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			reads.commitPages.Add(1)
		}
		mustEncode(t, w, []any{})
	})
	earlier := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for number := 1; number <= ahead; number++ {
		if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
			Repo: pr2808Repo, Number: number, HeadSHA: fmt.Sprintf("%040d", number), HeadRef: fmt.Sprintf("chore/tidy-%d", number),
			BaseRef: "main", State: "merged",
			Texts: &PullRequestTexts{HeadRepo: pr2808Repo, BaseRef: "main", CommitMessages: []string{}, MessagesRead: true},
		}); err != nil {
			t.Fatalf("store merged row #%d: %v", number, err)
		}
		if _, err := pool.Exec(ctx, `
			update pull_request_heads set issue_key = $3, attribution_source = $4, checked_at = $5 where repo = $1 and number = $2
		`, pr2808Repo, number, pr2808Wrong, string(sourceTitleBody), earlier.Add(time.Duration(number)*time.Second)); err != nil {
			t.Fatalf("store the older rule's verdict of #%d: %v", number, err)
		}
	}
	storePR2808AsAnOlderRuleJudgedIt(t, ctx, pool)

	if err := NewReconcile(pool, fake.newTestClient(), events.NewBroker()).reconcilePullRequestHeads(ctx, time.Now()); err != nil {
		t.Fatalf("reconcile the pull request heads: %v", err)
	}
	var pass pr2808Pass
	pass.issue, pass.source = pr2808Credit(t, ctx, pool)
	pass.restReads = reads.pulls.Load() + reads.commitPages.Load()
	pass.factsQueries = fake.factsQueries.Load()
	t.Logf("with %d older-rule rows read before it, after one pass #2808 credits %s from %s; the pass made %d REST reads and %d batched queries",
		ahead, pass.issue, pass.source, pass.restReads, pass.factsQueries)
	wrongHeads, err := HeadsAttributedTo(ctx, pool, pr2808Wrong)
	if err != nil {
		t.Fatalf("read the heads of %s: %v", pr2808Wrong, err)
	}
	for _, head := range wrongHeads {
		pass.comparedWithWrong = pass.comparedWithWrong || head.Number == pr2808Number
	}
	return pass
}

// Dispatch stores two attributions of one pull request: delivery_pull_requests, for the
// delivery timeline, and pull_request_heads, which the results read compares a check with. The
// first stores its inputs and every reconcile pass judges a batch of them again from the
// database alone (AttributePullRequests), so a change of the ranking reaches it without a
// GitHub read. The second stores no inputs and is judged again only by a read of GitHub. After
// that pass the two must name one issue.
func TestPR2808TheTwoStoredAttributionsOfOnePullRequestAgree(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedPR2808Issues(t, ctx, pool)
	merged := time.Date(2026, 11, 11, 23, 10, 25, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, partial,
			issue_key, attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys)
		values ($1, $2, $3, $4, 'legion-implementer[bot]', $5, $5, false, $6, $7, $8, $9, '{}')
	`, pr2808Repo, pr2808Number, pr2808Title, pr2808URL, merged, pr2808Wrong,
		uniqueKeys(pr2808Title+"\n"+pr2808Body), citedIssues(pr2808Repo, pr2808Body), uniqueKeys(strings.ToUpper(pr2808Branch))); err != nil {
		t.Fatalf("store #2808's delivery row as the older rule judged it: %v", err)
	}
	storePR2808AsAnOlderRuleJudgedIt(t, ctx, pool)

	if _, err := AttributePullRequests(ctx, pool, AttributionResolveBatch); err != nil {
		t.Fatalf("attribute the stored pull requests: %v", err)
	}
	var timeline *string
	if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where repo = $1 and number = $2`,
		pr2808Repo, pr2808Number).Scan(&timeline); err != nil {
		t.Fatalf("read #2808's delivery row: %v", err)
	}
	results, source := pr2808Credit(t, ctx, pool)
	t.Logf("delivery_pull_requests credits %v; pull_request_heads credits %s from %s", pr2808KeyOf(timeline), results, source)
	if pr2808KeyOf(timeline) != results {
		t.Errorf("one pull request, two issues: the delivery timeline credits #2808 to %s, the results read to %s (from %q)",
			pr2808KeyOf(timeline), results, source)
	}
}

func pr2808KeyOf(key *string) string {
	if key == nil {
		return "none"
	}
	return *key
}
