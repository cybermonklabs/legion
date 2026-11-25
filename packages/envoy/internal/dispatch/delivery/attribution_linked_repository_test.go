// attribution_linked_repository_test.go holds the attribution to R19 of the spec of several open
// pull requests: a pull request that an issue links by its URL is that issue's pull request in any
// repository, and a pull request of another repository that only names the issue is not. A pull
// request whose head is in a fork has no issue, linked or not. Each pull request reaches Dispatch
// through a writer of the intake or of the reconcile, never through a row written straight into
// pull_request_heads, and each case reads the issue's heads as the results read reads them
// (HeadsAttributedTo). The file builds at a commit before the rule and fails there on its own
// assertion.
package delivery

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The issue of this test, ACME-1, the default repository of its project, and the repository of each
// pull request of the test, which is another one.
const (
	linkedIssue     = "ACME-1"
	linkedIssueRepo = "acme/app"
	linkedPullRepo  = "acme/infra"
)

// ruleBeforeR19 is AttributionRuleVersion before R19, the value at 621d1378273e: the rule that
// held the issue's own link to a pull request's URL to the pull request's repository, as it held
// each other source.
const ruleBeforeR19 = 2

// linkedPullURL is the URL of acme/infra#number, which an external link of ACME-1 names.
func linkedPullURL(number int) string {
	return "https://github.com/" + linkedPullRepo + "/pull/" + strconv.Itoa(number)
}

// seedLinkedIssue writes project ACME, maps acme/app to it as its default, so ACME-1 resolves to
// acme/app, writes ACME-1, and writes each of links as an external link of ACME-1.
// seedAttributionIssues writes only the ATTR-<n> issues of the project that maps acme/widgets, so
// the issue of another repository is written here.
func seedLinkedIssue(t *testing.T, ctx context.Context, pool *store.Pool, links ...string) {
	t.Helper()
	seedProjectRepository(t, ctx, pool, "ACME", "Acme", linkedIssueRepo)
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, 'ACME', 1, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1)
		on conflict do nothing
	`, linkedIssue); err != nil {
		t.Fatalf("seed issue %s: %v", linkedIssue, err)
	}
	for _, url := range links {
		if _, err := pool.Exec(ctx, `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')
			on conflict do nothing`, linkedIssue, url); err != nil {
			t.Fatalf("seed link %s -> %s: %v", url, linkedIssue, err)
		}
	}
}

// stampAttributionRule sets a stored row's attribution_rule to rule, as stampLegacyAttributionRule
// (pull_request_heads_test.go) sets 0: no writer stamps a rule older than this build's, so a test
// that needs a row an older rule judged writes it with this one update. It is in this file so that
// the file, copied alone into a tree before R19, builds there.
func stampAttributionRule(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number, rule int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		update pull_request_heads set attribution_rule = $3 where repo = $1 and number = $2
	`, repo, number, rule); err != nil {
		t.Fatalf("make %s#%d a row of rule %d: %v", repo, number, rule, err)
	}
}

// linkedPullEnvelope is the `pull_request` envelope of acme/infra#number, opened from the branch
// headRef of headRepo at sha, as recordPullRequestHead reads it.
func linkedPullEnvelope(number int, title, headRef, headRepo, sha string) map[string]string {
	return map[string]string{
		"kind": "pr", "action": "opened", "repo": linkedPullRepo, "number": strconv.Itoa(number),
		"title": title, "body": "", "head_ref": headRef, "head_repo": headRepo, "head_sha": sha,
		"base_ref": "main", "merged": "false", "url": linkedPullURL(number),
		"updated_at": "2026-11-17T00:00:00Z",
	}
}

// linkedHeads is the heads of ACME-1, as the results read reads them, by pull request number.
func linkedHeads(t *testing.T, ctx context.Context, pool *store.Pool) map[int]AttributedHead {
	t.Helper()
	heads, err := unrenamed().HeadsAttributedTo(ctx, pool, linkedIssue)
	if err != nil {
		t.Fatalf("read the heads of %s: %v", linkedIssue, err)
	}
	byNumber := map[int]AttributedHead{}
	for _, head := range heads {
		byNumber[head.Number] = head
	}
	return byNumber
}

// repositoryOfHead is the repository AttributedHead names, and false where the type has no field
// Repo. The field is read by its name, and not as head.Repo, so that this file builds at
// c9065ed8f452, whose AttributedHead has no such field, and fails there on its own assertion.
func repositoryOfHead(head AttributedHead) (string, bool) {
	field := reflect.ValueOf(head).FieldByName("Repo")
	if !field.IsValid() || field.Kind() != reflect.String {
		return "", false
	}
	return field.String(), true
}

// linkedPullGitHub is GitHub for acme/infra: the commit messages of each pull request, where only
// those of #10 name ACME-1, and #12, merged, whose reads it counts.
func linkedPullGitHub(t *testing.T, merged string, mergedReads *atomic.Int32) *fakeGitHub {
	t.Helper()
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/infra/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		messages := []any{}
		if r.PathValue("number") == "10" {
			messages = append(messages, map[string]any{"commit": map[string]any{"message": linkedIssue + ": move the deploy job"}})
		}
		mustEncode(t, w, messages)
	})
	fake.handle("GET /repos/acme/infra/pulls/12", func(w http.ResponseWriter, _ *http.Request) {
		mergedReads.Add(1)
		mustEncode(t, w, map[string]any{
			"number": 12, "title": "Move the deploy job", "body": "",
			"html_url": linkedPullURL(12),
			"state":    "closed", "merged": true, "merged_at": "2026-11-16T00:00:00Z",
			"updated_at": "2026-11-16T00:00:00Z",
			"head": map[string]any{
				"ref": "work/deploy-job", "sha": merged,
				"repo": map[string]any{"full_name": linkedPullRepo},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	return fake
}

// TestALinkedPullRequestOfAnotherRepositoryIsTheIssues: R19. ACME-1's repository is acme/app. A
// pull request of acme/infra that ACME-1 links by its URL is ACME-1's, from the intake's writer and
// from the reconcile's read of a row an older rule judged; one that only names ACME-1 is nobody's,
// and so is one whose head is in a fork, linked or not.
func TestALinkedPullRequestOfAnotherRepositoryIsTheIssues(t *testing.T) {
	const (
		linkedHead = "9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"
		namedHead  = "10a10a10a10a10a10a10a10a10a10a10a10a10a1"
		forkHead   = "11b11b11b11b11b11b11b11b11b11b11b11b11b1"
		mergedHead = "12c12c12c12c12c12c12c12c12c12c12c12c12c1"
	)

	t.Run("linked", func(t *testing.T) {
		fastPageRetries(t)
		pool, ctx := deliveryTestPool(t)
		seedLinkedIssue(t, ctx, pool, linkedPullURL(9))
		var reads atomic.Int32
		intake := newIntake(nil, pool, nil, linkedPullGitHub(t, mergedHead, &reads).newTestClient(), nil, nil, unrenamed())
		if _, err := intake.recordPullRequestHead(ctx,
			linkedPullEnvelope(9, "Move the deploy job", "work/deploy-job", linkedPullRepo, linkedHead), 9); err != nil {
			t.Fatalf("record the head of acme/infra#9: %v", err)
		}
		heads := linkedHeads(t, ctx, pool)
		head, held := heads[9]
		if !held {
			t.Fatalf("the heads of %s = %+v, want acme/infra#9: %s links it by its URL", linkedIssue, heads, linkedIssue)
		}
		if repo, named := repositoryOfHead(head); !named || repo != linkedPullRepo {
			t.Fatalf("acme/infra#9 is held under the repository %q (a field Repo: %v), want %s", repo, named, linkedPullRepo)
		}
		if head.HeadSHA != linkedHead || !head.Open {
			t.Fatalf("acme/infra#9 = %+v, want open at %s", head, linkedHead)
		}
	})

	t.Run("named only", func(t *testing.T) {
		fastPageRetries(t)
		pool, ctx := deliveryTestPool(t)
		seedLinkedIssue(t, ctx, pool)
		var reads atomic.Int32
		intake := newIntake(nil, pool, nil, linkedPullGitHub(t, mergedHead, &reads).newTestClient(), nil, nil, unrenamed())
		envelope := linkedPullEnvelope(10, linkedIssue+" move the deploy job", "legion/"+linkedIssue, linkedPullRepo, namedHead)
		envelope["body"] = "This moves the deploy job of " + linkedIssue + "."
		if _, err := intake.recordPullRequestHead(ctx, envelope, 10); err != nil {
			t.Fatalf("record the head of acme/infra#10: %v", err)
		}
		if heads := linkedHeads(t, ctx, pool); len(heads) != 0 {
			t.Fatalf("the heads of %s = %+v, want none: acme/infra#10 only names %s, and no issue links it", linkedIssue, heads, linkedIssue)
		}
		if row := readHead(t, ctx, pool, linkedPullRepo, 10); row.IssueKey != nil || row.AttributionSource != string(sourceNone) {
			t.Fatalf("acme/infra#10 = %+v, want a row with no issue", row)
		}
	})

	t.Run("fork", func(t *testing.T) {
		fastPageRetries(t)
		pool, ctx := deliveryTestPool(t)
		seedLinkedIssue(t, ctx, pool, linkedPullURL(11))
		var reads atomic.Int32
		intake := newIntake(nil, pool, nil, linkedPullGitHub(t, mergedHead, &reads).newTestClient(), nil, nil, unrenamed())
		if _, err := intake.recordPullRequestHead(ctx,
			linkedPullEnvelope(11, "Move the deploy job", "work/deploy-job", "contributor/infra", forkHead), 11); err != nil {
			t.Fatalf("record the head of acme/infra#11: %v", err)
		}
		if heads := linkedHeads(t, ctx, pool); len(heads) != 0 {
			t.Fatalf("the heads of %s = %+v, want none: the head of acme/infra#11 is in the fork contributor/infra", linkedIssue, heads)
		}
		if row := readHead(t, ctx, pool, linkedPullRepo, 11); row.IssueKey != nil {
			t.Fatalf("acme/infra#11 = %+v, want a row with no issue: its head is a fork's", row)
		}
	})

	t.Run("merged before the rule", func(t *testing.T) {
		fastPageRetries(t)
		pool, ctx := deliveryTestPool(t)
		seedLinkedIssue(t, ctx, pool, linkedPullURL(12))
		var reads atomic.Int32
		client := linkedPullGitHub(t, mergedHead, &reads).newTestClient()

		// #12 as a rule before R19 left it: merged, with no issue, and read an hour ago.
		eventAt := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
		read := time.Now().Add(-time.Hour)
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
			Repo: linkedPullRepo, Number: 12, HeadSHA: mergedHead, HeadRef: "work/deploy-job",
			BaseRef: "main", State: "merged", AttributionSource: string(sourceNone),
			Title: "Move the deploy job", URL: linkedPullURL(12), EventAt: &eventAt, CheckedAt: &read,
		}); err != nil {
			t.Fatalf("store the merged head of acme/infra#12: %v", err)
		}
		stampAttributionRule(t, ctx, pool, linkedPullRepo, 12, ruleBeforeR19)
		if heads := linkedHeads(t, ctx, pool); len(heads) != 0 {
			t.Fatalf("the heads of %s before the pass = %+v, want none", linkedIssue, heads)
		}

		if err := NewReconcile(pool, client, nil, unrenamed()).reconcilePullRequestHeads(ctx, time.Now()); err != nil {
			t.Fatalf("reconcile the pull request heads: %v", err)
		}
		heads := linkedHeads(t, ctx, pool)
		head, held := heads[12]
		if !held {
			t.Fatalf("the heads of %s after one pass = %+v (GitHub read #12 %d times), want acme/infra#12: %s links it, and the pass judges a row of rule %d again",
				linkedIssue, heads, reads.Load(), linkedIssue, ruleBeforeR19)
		}
		if head.HeadSHA != mergedHead || head.Open {
			t.Fatalf("acme/infra#12 = %+v, want merged at %s", head, mergedHead)
		}
	})
}
