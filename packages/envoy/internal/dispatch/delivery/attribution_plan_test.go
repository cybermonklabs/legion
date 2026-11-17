package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// planTexts are 200 texts of the size real pull requests have, made from a fixed seed: bodies of
// about 8,900 characters and messages of about 2,900, each naming its own issue by its branch
// (two in three), its title and its messages, another project's issue in its body, and a GitHub
// issue by a full URL and by a bare #n.
func planTexts() []readingText {
	random := rand.New(rand.NewPCG(4141, 8))
	vocabulary := strings.Fields("the read of a pull request stores facts only and the statement gives its issue from " +
		"stored keys under one reading with no environment and one renaming rule")
	words := func(characters int) string {
		var out []string
		for size := 0; size < characters; {
			word := vocabulary[random.IntN(len(vocabulary))]
			out = append(out, word)
			size += len(word) + 1
		}
		return strings.Join(out, " ")
	}
	texts := make([]readingText, 0, 200)
	for i := range 200 {
		repo, own := "acme/widgets", fmt.Sprintf("ACME-%d", 1000+i)
		if i%2 == 1 {
			repo, own = "acme/legion", fmt.Sprintf("LEGN-%d", 1000+i)
		}
		branch := "legion/" + strings.ToLower(own)
		if i%3 == 0 {
			branch = "work/" + strings.ReplaceAll(words(12), " ", "-")
		}
		texts = append(texts, readingText{
			Repo:    repo,
			HeadRef: branch,
			Title:   own + ": " + words(60),
			Body: fmt.Sprintf("Follows OTHR-%d. %s See https://github.com/%s/issues/%d and #%d. %s",
				100+i%37, words(4400), repo, 50+i%9, 12+i%5, words(4400)),
			Messages: []string{words(900) + "\n\nDispatch: " + own, words(1000), "fix: " + words(1000)},
		})
	}
	return texts
}

// seedPlanFixture stores 5,000 pull requests with texts through the trigger, one in a hundred of
// acme/widgets under the name acme/old it was renamed from, a timeline row for half of them and
// 500 pull requests with a timeline row only, an issue for each key the texts name, 20,000 more
// issues and 5,000 more links, and then analyzes each table, as a deployment's planner sees
// them. Three issues link a URL the texts cite, and 300 link a pull request.
func seedPlanFixture(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	for project, repo := range map[string]string{"ACME": "acme/widgets", "LEGN": "acme/legion", "OTHR": "", "FILL": ""} {
		exec("project", `insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict do nothing`, project)
		if repo != "" {
			exec("mapping", `insert into repo_projects (repo, project, created_by, is_default) values ($1, $2, '{"kind":"system","id":"test"}', true)`, repo, project)
		}
	}
	exec("old name", `insert into repo_projects (repo, project, created_by, renamed_to) values ('acme/old', 'ACME', '{"kind":"system","id":"test"}', 'acme/widgets')`)
	texts := planTexts()
	const rows = 5000
	payload := make([]map[string]any, 0, rows)
	for number := 1; number <= rows; number++ {
		text := texts[number%len(texts)]
		repo := text.Repo
		if repo == "acme/widgets" && number%100 == 0 {
			repo = "acme/old"
		}
		payload = append(payload, map[string]any{"repo": repo, "number": number, "title": text.Title, "body": text.Body, "head_ref": text.HeadRef, "messages": text.Messages})
	}
	for start := 0; start < len(payload); start += 1000 {
		chunk, err := json.Marshal(payload[start:min(start+1000, len(payload))])
		if err != nil {
			t.Fatal(err)
		}
		exec("results rows", `insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key,
				attribution_source, title, url, event_at, checked_at, github_read_at)
			select u.repo, u.number, lpad(to_hex(u.number), 40, '0'), u.head_ref, 'main', 'merged', null, 'none', u.title,
				'https://github.com/' || u.repo || '/pull/' || u.number, timestamptz '2026-11-01' + make_interval(secs => u.number), now(), now()
			from jsonb_to_recordset($1::jsonb) as u(repo text, number int, title text, head_ref text)`, string(chunk))
		exec("texts", `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
			select u.repo, u.number, lpad(to_hex(u.number), 40, '0'), u.head_ref, u.repo, 'main', u.title, u.body, u.messages,
				timestamptz '2026-11-01' + make_interval(secs => u.number)
			from jsonb_to_recordset($1::jsonb) as u(repo text, number int, title text, head_ref text, body text, messages text[])`, string(chunk))
	}
	exec("issues the texts name", `insert into issues (key, project_key, number, title, status, created_by, rank)
		select k, split_part(k, '-', 1), split_part(k, '-', 2)::int, k, 'todo', '{"kind":"system","id":"test"}', k
		from (select distinct unnest(branch_keys || title_keys || commit_keys) k from pull_request_texts) keys
		where split_part(k, '-', 1) in ('ACME', 'LEGN', 'OTHR')`)
	exec("filler issues", `insert into issues (key, project_key, number, title, status, created_by, rank)
		select 'FILL-' || g, 'FILL', g, 'filler', 'todo', '{"kind":"system","id":"test"}', 'FILL-' || g from generate_series(1, 20000) g`)
	exec("filler links", `insert into issue_external_links (issue_key, url, kind)
		select 'FILL-' || g, 'https://github.com/acme/other/pull/' || g, 'other' from generate_series(1, 5000) g`)
	exec("timeline rows", `insert into delivery_pull_requests (repo, number, title, url, author, issue_key)
		select h.repo, h.number, h.title, h.url, 'octocat', null from pull_request_heads h where h.number % 2 = 0`)
	exec("timeline rows only", `insert into delivery_pull_requests (repo, number, title, url, author, issue_key)
		select 'acme/widgets', g, 't', 'https://github.com/acme/widgets/pull/' || g, 'octocat', 'OTHR-100'
		from generate_series($1::int + 1, $1::int + 500) g`, rows)
	exec("links of pull requests", `insert into issue_external_links (issue_key, url, kind)
		select k.key, 'https://github.com/acme/widgets/pull/' || row_number() over (order by k.key), 'github'
		from (select key from issues where project_key in ('ACME', 'LEGN') order by key limit 300) k`)
	exec("issues that link a cited issue", `insert into issues (key, project_key, number, title, status, created_by, rank)
		select 'ACME-' || g, 'ACME', g, 'cited', 'todo', '{"kind":"system","id":"test"}', 'ACME-' || g from generate_series(1, 3) g`)
	exec("links of cited issues", `insert into issue_external_links (issue_key, url, kind)
		select 'ACME-' || g, 'https://github.com/Acme/Widgets/issues/' || (49 + g), 'github' from generate_series(1, 3) g`)
	for _, table := range []string{"pull_request_heads", "pull_request_texts", "issues", "issue_external_links", "delivery_pull_requests", "repo_projects"} {
		exec("analyze", `vacuum analyze `+table)
	}
}

// planOf is the plan of sql as it ran, one line a node, and the tables a node read with a
// sequential scan that ran: a node marked never executed did not run.
func planOf(t *testing.T, ctx context.Context, pool *store.Pool, sql string, args ...any) ([]string, []string) {
	t.Helper()
	rows, err := pool.Query(ctx, `explain (analyze, timing off, buffers off) `+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan, scanned []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
		if _, after, found := strings.Cut(line, "Seq Scan on "); found && !strings.Contains(line, "never executed") {
			table, _, _ := strings.Cut(after, " ")
			scanned = append(scanned, table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return plan, scanned
}

// indexesRead is each index a node of plan reads.
func indexesRead(plan []string) []string {
	var indexes []string
	for _, line := range plan {
		for _, marker := range []string{"Index Scan using ", "Index Only Scan using ", "Bitmap Index Scan on "} {
			if _, after, found := strings.Cut(line, marker); found {
				index, _, _ := strings.Cut(after, " ")
				if !slices.Contains(indexes, index) {
					indexes = append(indexes, index)
				}
			}
		}
	}
	slices.Sort(indexes)
	return indexes
}

// The tables that grow with pull requests and links, which no read of an issue may scan.
var growingTables = []string{"pull_request_heads", "pull_request_texts", "delivery_pull_requests", "issue_external_links"}

// Each read of a pull request's issue reads an index of each table that grows with pull requests
// and links, at 5,000 pull requests with texts of the size real pull requests have: a read by an
// issue that a branch names, that a link names, that a citation names and that the timeline
// names, and a read of 1 and of 100 keys. The arm by key that compares a cited URL with a link
// reads issue_external_links_url_lower. Scans of issues and repo_projects are printed and fail
// nothing: the repository rule (refs.IssueRepoTerms) and the hold join issues by a hash where the
// planner prices that lower, and repo_projects holds one row for each mapped repository. Control
// in the same test: with pull_request_heads_url_lower dropped, the read of the issue a link names
// scans pull_request_heads.
func TestEveryReadByIssueUsesAnIndex(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedPlanFixture(t, ctx, pool)
	statements := attributionStatementsFor(readingSchema(t, ctx, pool))
	var branchIssue, linkIssue string
	if err := pool.QueryRow(ctx, `select k from pull_request_texts, unnest(branch_keys) k join issues i on i.key = k order by k limit 1`).Scan(&branchIssue); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select issue_key from issue_external_links where url like '%/pull/%' and issue_key not like 'FILL-%' order by issue_key limit 1`).Scan(&linkIssue); err != nil {
		t.Fatal(err)
	}
	reads := []struct {
		what, sql string
		args      []any
	}{
		{"HeadsAttributedTo of " + branchIssue + ", which a branch names", statements.headsAttributedTo, []any{branchIssue}},
		{"HeadsAttributedTo of " + linkIssue + ", which a link names", statements.headsAttributedTo, []any{linkIssue}},
		{"HeadsAttributedTo of ACME-1, whose link a body cites", statements.headsAttributedTo, []any{"ACME-1"}},
		{"HeadsAttributedTo of OTHR-100, which the timeline names", statements.headsAttributedTo, []any{"OTHR-100"}},
		{"PullRequestIssues of 1 key", statements.pullRequestIssues, []any{[]string{"acme/widgets"}, []int{2}}},
	}
	var repos []string
	var numbers []int
	for number := 2; number <= 200; number += 2 {
		repos, numbers = append(repos, "acme/widgets"), append(numbers, number)
	}
	reads = append(reads, struct {
		what, sql string
		args      []any
	}{"PullRequestIssues of 100 keys", statements.pullRequestIssues, []any{repos, numbers}})
	for _, read := range reads {
		plan, scanned := planOf(t, ctx, pool, read.sql, read.args...)
		t.Logf("%s: sequential scans of %v, indexes %v", read.what, scanned, indexesRead(plan))
		for _, table := range scanned {
			if slices.Contains(growingTables, table) {
				t.Errorf("%s scans %s:\n%s", read.what, table, strings.Join(plan, "\n"))
			}
		}
	}

	// The arm by key of a cited URL reads issue_external_links_url_lower by its first term, and
	// takes the fold of the reading as a filter.
	byKey, _ := planOf(t, ctx, pool, statements.pullRequestIssues, []string{"acme/widgets"}, []int{50})
	citedArm := false
	for i, line := range byKey {
		if strings.Contains(line, "Index Scan using issue_external_links_url_lower") && i+1 < len(byKey) &&
			strings.Contains(byKey[i+1], "lower(u.url)") {
			citedArm = true
			t.Logf("the arm by key of a cited URL: %s", strings.TrimSpace(strings.Join(byKey[i:min(i+3, len(byKey))], " | ")))
		}
	}
	if !citedArm {
		t.Errorf("the arm by key of a cited URL reads no issue_external_links_url_lower:\n%s", strings.Join(byKey, "\n"))
	}
	// The arm by issue of a cited URL reads the cited lists by their index, with the links of the
	// issue read by its key, in the fold of the reading as in the plan's text, which folds the link
	// with lower() alone. Its overlap is the index condition of pull_request_texts_keys: an overlap
	// of the list with an array under another collation than the list's reads no index.
	fold := "array(select " + linkURLAsCited + " from issue_external_links"
	planText := strings.Replace(statements.headsAttributedTo, fold, "array(select lower(l.url) from issue_external_links", 1)
	if planText == statements.headsAttributedTo {
		t.Fatalf("the statement by issue holds no %q", fold)
	}
	for _, text := range []struct{ what, sql string }{{"the fold of the reading", statements.headsAttributedTo}, {"the plan's text", planText}} {
		plan, scanned := planOf(t, ctx, pool, text.sql, "ACME-1")
		var citedArm []string
		for i, line := range plan {
			if strings.Contains(line, "Index Cond: (cited_issues && ") && i > 0 {
				citedArm = append(citedArm, strings.TrimSpace(plan[i-1]), strings.TrimSpace(line))
			}
		}
		t.Logf("the arm by issue of a cited URL, in %s: %v; indexes %v, sequential scans of %v", text.what, citedArm, indexesRead(plan), scanned)
		if len(citedArm) != 2 || !strings.Contains(citedArm[0], "Bitmap Index Scan on pull_request_texts_keys") {
			t.Errorf("the arm by issue of a cited URL, in %s, reads no pull_request_texts_keys by its overlap:\n%s", text.what, strings.Join(plan, "\n"))
		}
		if !slices.Contains(indexesRead(plan), "issue_external_links_pkey") {
			t.Errorf("the read by issue of ACME-1, in %s, reads the links of ACME-1 by no issue_external_links_pkey:\n%s", text.what, strings.Join(plan, "\n"))
		}
	}
	if listed := listedFor(t, ctx, pool, "ACME-1"); len(listed) == 0 {
		t.Error("ACME-1 lists no pull request: the read by issue of a cited URL read nothing to plan")
	}

	if _, err := pool.Exec(ctx, `drop index pull_request_heads_url_lower`); err != nil {
		t.Fatal(err)
	}
	plan, scanned := planOf(t, ctx, pool, statements.headsAttributedTo, linkIssue)
	t.Logf("the control, without pull_request_heads_url_lower: HeadsAttributedTo of %s scans %v", linkIssue, scanned)
	if !slices.Contains(scanned, "pull_request_heads") {
		t.Errorf("the control: without pull_request_heads_url_lower, HeadsAttributedTo of %s scans no pull_request_heads:\n%s", linkIssue, strings.Join(plan, "\n"))
	}
}
