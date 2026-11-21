package metrics

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// mergeTimesRepo is the repository of the merged pull requests of the merge-time fixture, the
// default repository of project ACME, so an issue of ACME resolves to it.
const mergeTimesRepo = "acme/widgets"

// mergedPull is one merged pull request of the fixture. stored is the issue the last reconcile
// pass wrote to its rows (delivery.SyncAttributions), nil for none. A pull request with texts
// has a results row and the texts of a read of GitHub that describe its head; messages nil says
// that read did not read the commit messages. One with no texts has a timeline row alone.
type mergedPull struct {
	number      int
	stored      *string
	texts       bool
	title       string
	messages    []string
	mergedAt    time.Time
	unfetchable bool
}

// mergeTimesDatabase is a database with the statement of a pull request's issue built, as main
// builds it after the migrations, project ACME mapped to mergeTimesRepo, and issues ACME-1 to
// ACME-4, each closed in the window the test reads. ACME-5 closed a day before that window, so
// the read asks for no pull request of it.
func mergeTimesDatabase(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	pool, ctx := storetest.Open(t).Pool, t.Context()
	if err := delivery.PrepareAttribution(ctx, pool); err != nil {
		t.Fatalf("prepare the statement of a pull request's issue: %v", err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	exec(`insert into projects (key, name, default_assignee) values ('ACME', 'ACME', 'alice')`)
	exec(`insert into repo_projects (repo, project, created_by, is_default)
		values ($1, 'ACME', '{"kind":"system","id":"metrics-test"}'::jsonb, true)`, mergeTimesRepo)
	for number := 1; number <= 5; number++ {
		created, closed := after(0), after(10)
		if number == 5 {
			created, closed = after(-48), after(-24)
		}
		exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at, closed_at)
			values ($1, 'ACME', $2, $1, 'done', $3, '{"kind":"user","id":"alice"}'::jsonb, $4, $5)`,
			fmt.Sprintf("ACME-%d", number), number, fmt.Sprintf("r%d", number), created, closed)
	}
	return pool, ctx
}

// storeMergedPull writes pull's rows as the reconcile leaves them, with no pass since.
func storeMergedPull(t *testing.T, ctx context.Context, pool *store.Pool, pull mergedPull) {
	t.Helper()
	url := fmt.Sprintf("https://github.com/%s/pull/%d", mergeTimesRepo, pull.number)
	head := fmt.Sprintf("%040x", pull.number)
	branch := fmt.Sprintf("work/change-%d", pull.number)
	if pull.texts {
		if _, err := pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at, github_read_at)
			values ($1, $2, $3, $4, 'main', 'merged', $5, 'none', $6, $7, $8, now(), now())`,
			mergeTimesRepo, pull.number, head, branch, pull.stored, pull.title, url, pull.mergedAt); err != nil {
			t.Fatalf("store the results row of #%d: %v", pull.number, err)
		}
		if _, err := pool.Exec(ctx, `
			insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
			values ($1, $2, $3, $4, $1, 'main', $5, '', $6, $7)`,
			mergeTimesRepo, pull.number, head, branch, pull.title, pull.messages, pull.mergedAt); err != nil {
			t.Fatalf("store the texts of #%d: %v", pull.number, err)
		}
	}
	var unfetchable *time.Time
	if pull.unfetchable {
		unfetchable = &pull.mergedAt
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author, issue_key, merged_at, unfetchable_at)
		values ($1, $2, $3, $4, 'octocat', $5, $6, $7)`,
		mergeTimesRepo, pull.number, pull.title, url, pull.stored, pull.mergedAt, unfetchable); err != nil {
		t.Fatalf("store the timeline row of #%d: %v", pull.number, err)
	}
}

// TestTheMergeTimesReadEachPullRequestsIssueFromTheStatement: the merge time of an issue is the
// earliest merge of a pull request that the one statement of a pull request's issue gives to it,
// as every reader of the timeline reads it (delivery.ListPullRequestsOfIssues), and never the
// issue a reconcile pass last stored. Each case is a state between two passes: a link made since
// the last pass, facts read since that name another issue, and facts that decide no issue yet.
// Control in the same test: a timeline row with no results row keeps its stored issue, which is
// the statement's answer for it, and a row GitHub proved absent counts for nothing.
func TestTheMergeTimesReadEachPullRequestsIssueFromTheStatement(t *testing.T) {
	pool, ctx := mergeTimesDatabase(t)
	// #11: ACME-1 links #11's own URL. The last pass ran before the link, so both rows of #11
	// store no issue.
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ('ACME-1', $1, 'github')`,
		fmt.Sprintf("https://github.com/%s/pull/11", mergeTimesRepo)); err != nil {
		t.Fatalf("link #11 on ACME-1: %v", err)
	}
	storeMergedPull(t, ctx, pool, mergedPull{number: 11, texts: true, title: "A change", messages: []string{}, mergedAt: after(5)})
	// #12: its rows store ACME-5, which the last pass wrote and which the read does not ask for, so
	// only the keys of #12's texts can give it to an issue the read asks for. The read of GitHub
	// since that pass stored a title that names ACME-2, and its commit messages.
	storeMergedPull(t, ctx, pool, mergedPull{number: 12, stored: new("ACME-5"), texts: true, title: "ACME-2: a change", messages: []string{"a change"}, mergedAt: after(6)})
	// #13: its rows store ACME-3. Its texts name no issue and its commit messages are not
	// read, so its answer is not known: it belongs to no issue yet.
	storeMergedPull(t, ctx, pool, mergedPull{number: 13, stored: new("ACME-3"), texts: true, title: "Another change", mergedAt: after(4)})
	// The control: #14 has a timeline row alone, so its stored issue is its answer, and #15,
	// merged earlier, is a row GitHub proved absent.
	storeMergedPull(t, ctx, pool, mergedPull{number: 14, stored: new("ACME-4"), title: "A fourth change", mergedAt: after(7)})
	storeMergedPull(t, ctx, pool, mergedPull{number: 15, stored: new("ACME-4"), title: "A gone change", mergedAt: after(3), unfetchable: true})

	population, err := Read(ctx, pool, nil, "ACME", after(-2), after(24))
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	for _, want := range []struct {
		issue string
		at    *time.Time
		why   string
	}{
		{"ACME-1", new(after(5)), "ACME-1 links #11, so the statement gives #11 to ACME-1, though the last pass stored no issue on its rows"},
		{"ACME-2", new(after(6)), "#12's title names ACME-2 in the facts read since the last pass, which stored ACME-5, an issue the read does not ask for"},
		{"ACME-3", nil, "#13's answer is not known since its commit messages are not read, whatever the last pass stored"},
		{"ACME-4", new(after(7)), "#14 has no results row, so its stored issue is its answer, and #15 is unfetchable"},
	} {
		got := population.Issues[want.issue].MergedAt
		if (got == nil) != (want.at == nil) || (got != nil && !got.Equal(*want.at)) {
			t.Errorf("%s's merge time is %s, want %s: %s", want.issue, timeText(got), timeText(want.at), want.why)
		}
	}
}

func timeText(at *time.Time) string {
	if at == nil {
		return "none"
	}
	return at.Format(time.RFC3339)
}
