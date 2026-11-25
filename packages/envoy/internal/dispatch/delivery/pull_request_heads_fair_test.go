package delivery

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// TestAHealthyRowBehindAFullBatchOfFailingRowsIsReadWithinTwoPasses: a full batch of open pull
// requests (PullRequestHeadBatch) whose rows no read has reached sit ahead of #70 in the
// reconcile's order, and GitHub answers each of their reads 502 at every pass. #70 is a row of
// the release before this one: read by that release an hour ago, with no base revision stored,
// so the check states settler compares none of its candidates until a read stores one. GitHub
// answers #70's read. The reconcile tries each due row within ceil(n/PullRequestHeadBatch) passes
// that try their whole batch, each stamp acknowledged, as every stamp here is, of n due rows, the
// least lately tried first, whatever earlier tries' reads answered: so the second pass, with no
// rate limit, reads #70 and stores its base revision,
// and the failed reads leave each failing row as it was, unread and with no base revision, since
// a failed read is evidence of nothing. The third pass tries the one failing row the second left,
// before any row the second pass tried.
func TestAHealthyRowBehindAFullBatchOfFailingRowsIsReadWithinTwoPasses(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 11)
	const (
		healthy  = 70
		first    = 1000
		head     = "7070707070707070707070707070707070707070"
		baseSHA  = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
		failures = PullRequestHeadBatch
	)
	var mu sync.Mutex
	pass := 0
	triedIn := map[int]map[int]bool{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("a read of pull request %q", r.PathValue("number"))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		if triedIn[number] == nil {
			triedIn[number] = map[int]bool{}
		}
		triedIn[number][pass] = true
		mu.Unlock()
		if number != healthy {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		mustEncode(t, w, map[string]any{
			"number": healthy, "title": issue + " the work", "body": "",
			"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", healthy),
			"state":    "open", "merged": false, "updated_at": "2026-02-01T00:00:00Z",
			"head": map[string]any{"ref": "legion/" + issue, "sha": head, "repo": map[string]any{"full_name": "acme/widgets"}},
			"base": map[string]any{"ref": "main", "sha": baseSHA},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{})
	})
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, state)
		select 'acme/widgets', n, lpad(to_hex(n), 40, '0'), 'open' from generate_series($1::integer, $2::integer) n`,
		first, first+failures-1); err != nil {
		t.Fatalf("store the pull requests whose reads fail: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, checked_at)
		values ('acme/widgets', $1, $2, 'legion/' || $3, 'main', 'open', $3, 'title', now() - interval '1 hour')`,
		healthy, head, issue); err != nil {
		t.Fatalf("store #%d as the release before stored it: %v", healthy, err)
	}
	stale, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	for _, row := range stale {
		if row.Number == healthy {
			t.Fatalf("the first batch already holds #%d: this test holds no row behind a full batch", healthy)
		}
	}

	reconcile := NewReconcile(pool, fake.newTestClient(), nil, unrenamed())
	baseOf := func() string {
		t.Helper()
		var stored *string
		if err := pool.QueryRow(ctx, `select base_sha from pull_request_heads where repo = 'acme/widgets' and number = $1`, healthy).Scan(&stored); err != nil {
			t.Fatalf("read #%d's base revision: %v", healthy, err)
		}
		if stored == nil {
			return ""
		}
		return *stored
	}
	for _, at := range []int{1, 2} {
		mu.Lock()
		pass = at
		mu.Unlock()
		if err := reconcile.reconcilePullRequestHeads(ctx, time.Now()); err != nil {
			t.Fatalf("reconcile pass %d: %v", at, err)
		}
	}
	if got := baseOf(); got != baseSHA {
		t.Fatalf("#%d's row stores the base revision %q after two passes, want %s: of %d due rows, each is tried within two passes of %d, whatever the reads of the others answer (tried in passes %v)",
			healthy, got, baseSHA, failures+1, PullRequestHeadBatch, triedIn[healthy])
	}
	var untried []int
	for number := first; number < first+failures; number++ {
		if len(triedIn[number]) == 0 {
			untried = append(untried, number)
		}
	}
	if len(untried) > 0 {
		t.Fatalf("after two passes %d failing rows were never tried (%v), want each tried", len(untried), untried)
	}
	var unchanged int
	if err := pool.QueryRow(ctx, `
		select count(*) from pull_request_heads
		where repo = 'acme/widgets' and number between $1 and $2 and checked_at is null and base_sha is null and head_sha = lpad(to_hex(number), 40, '0')`,
		first, first+failures-1).Scan(&unchanged); err != nil {
		t.Fatalf("read the failing rows: %v", err)
	}
	if unchanged != failures {
		t.Fatalf("%d of the %d rows whose reads failed are as they were, want all: a failed read is no read of the pull request", unchanged, failures)
	}

	var left []int
	for number := first; number < first+failures; number++ {
		if !triedIn[number][2] {
			left = append(left, number)
		}
	}
	if len(left) != 1 {
		t.Fatalf("the second pass left %v of the failing rows untried, want exactly one: it tries #%d and %d of them", left, healthy, PullRequestHeadBatch-1)
	}
	mu.Lock()
	pass = 3
	mu.Unlock()
	stale, err = ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads before the third pass: %v", err)
	}
	if len(stale) == 0 || stale[0].Number != left[0] {
		t.Fatalf("the third pass's batch begins %v, want #%d, the one row the second pass left untried", stale[:min(len(stale), 3)], left[0])
	}
}

// TestTheRowsARateLimitedPassDidNotTryAreTheFirstTheNextPassTries: PullRequestHeadBatch + 10 open
// pull requests are due, none read yet, each tried before at a time of its own, so the reconcile
// takes them in the order of those tries, which runs against their numbers. GitHub answers the
// first pass's reads with a rate limit, which stops that pass's batch: the rows it tried go behind
// every due row, and the rows of its batch it did not try keep their places, so the next pass
// takes them first, before the ten rows the stopped pass did not take and before every row it
// tried, and tries each of them. A try is no read: no row stores a read time or a base revision.
func TestTheRowsARateLimitedPassDidNotTryAreTheFirstTheNextPassTries(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const due = PullRequestHeadBatch + 10
	var mu sync.Mutex
	limited := true
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"Server Error"}`)
	})
	// Row n was tried n minutes before a day ago, so the row of the highest number goes first.
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, state, attempted_at)
		select 'acme/widgets', n, lpad(to_hex(n), 40, '0'), 'open', now() - interval '1 day' - make_interval(mins => n)
		from generate_series(1, $1::integer) n`, due); err != nil {
		t.Fatalf("store the due pull requests: %v", err)
	}
	numbers := func(heads []PullRequestHead) []int {
		var got []int
		for _, head := range heads {
			got = append(got, head.Number)
		}
		return got
	}
	first, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads before the first pass: %v", err)
	}
	if len(first) != PullRequestHeadBatch || first[0].Number != due || first[len(first)-1].Number != due-PullRequestHeadBatch+1 {
		t.Fatalf("the first batch is %d rows from #%v, want %d from #%d to #%d: this test holds no order of tries", len(first), numbers(first[:min(len(first), 3)]), PullRequestHeadBatch, due, due-PullRequestHeadBatch+1)
	}
	triedAt := func(at time.Time) map[int]bool {
		t.Helper()
		rows, err := pool.Query(ctx, `select number from pull_request_heads where attempted_at = $1`, at)
		if err != nil {
			t.Fatalf("read the rows tried at %s: %v", at, err)
		}
		defer rows.Close()
		tried := map[int]bool{}
		for rows.Next() {
			var number int
			if err := rows.Scan(&number); err != nil {
				t.Fatalf("read the rows tried at %s: %v", at, err)
			}
			tried[number] = true
		}
		return tried
	}

	reconcile := NewReconcile(pool, fake.newTestClient(), nil, unrenamed())
	stopped := time.Now().Truncate(time.Microsecond)
	err = reconcile.reconcilePullRequestHeads(ctx, stopped)
	if _, ok := githubapp.AsRateLimit(err); !ok {
		t.Fatalf("the first pass answers %v, want GitHub's rate limit: this test holds no stopped pass", err)
	}
	tried := triedAt(stopped)
	var untried []int
	for _, head := range first {
		if !tried[head.Number] {
			untried = append(untried, head.Number)
		}
	}
	if len(tried) == 0 || len(untried) == 0 {
		t.Fatalf("the stopped pass tried %d rows of its batch of %d and left %d untried, want some of each: a rate limit stops a batch part of the way", len(tried), len(first), len(untried))
	}
	t.Logf("the stopped pass tried %d rows of its batch of %d and left %d untried", len(tried), len(first), len(untried))

	next, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads before the second pass: %v", err)
	}
	if got := numbers(next); len(got) < len(untried) || !slices.Equal(got[:len(untried)], untried) {
		t.Fatalf("the second batch begins %v, want the %d rows the stopped pass did not try, in their places, %v: a row a stopped pass did not try keeps its place", got[:min(len(got), len(untried))], len(untried), untried)
	}
	for index, head := range next {
		if tried[head.Number] && index < len(untried)+due-PullRequestHeadBatch {
			t.Fatalf("the second batch takes #%d, which the stopped pass tried, at %d, before a row that pass did not try or take: a try moves a row behind every due row tried less lately", head.Number, index)
		}
	}

	mu.Lock()
	limited = false
	mu.Unlock()
	after := stopped.Add(time.Second)
	if err := reconcile.reconcilePullRequestHeads(ctx, after); err != nil {
		t.Fatalf("the second pass: %v", err)
	}
	triedNext := triedAt(after)
	for _, number := range untried {
		if !triedNext[number] {
			t.Fatalf("the second pass did not try #%d, which the stopped pass left untried", number)
		}
	}
	var answered int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where checked_at is not null or base_sha is not null`).Scan(&answered); err != nil {
		t.Fatalf("read the rows a read stamped: %v", err)
	}
	if answered != 0 {
		t.Fatalf("%d rows store a read time or a base revision after reads GitHub refused, want none: a try is no read of the pull request", answered)
	}
}
