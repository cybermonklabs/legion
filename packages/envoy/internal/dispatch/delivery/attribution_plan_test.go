package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
// in the same test: with issue_external_links_url_lower dropped, the read of one key, whose link
// arms read the links of its pull request by that index, scans issue_external_links.
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

	if _, err := pool.Exec(ctx, `drop index issue_external_links_url_lower`); err != nil {
		t.Fatal(err)
	}
	plan, scanned := planOf(t, ctx, pool, statements.pullRequestIssues, []string{"acme/widgets"}, []int{2})
	t.Logf("the control, without issue_external_links_url_lower: PullRequestIssues of 1 key scans %v", scanned)
	if !slices.Contains(scanned, "issue_external_links") {
		t.Errorf("the control: without issue_external_links_url_lower, PullRequestIssues of 1 key scans no issue_external_links:\n%s", strings.Join(plan, "\n"))
	}
}

// recordedReads is a Querier that sends each statement to pool and keeps it with its arguments.
// A plan test explains the statements that a reader sent, and not a copy of their text.
type recordedReads struct {
	pool *store.Pool
	sent []recordedRead
}

// recordedRead is one statement that a reader sent, with its arguments.
type recordedRead struct {
	sql  string
	args []any
}

func (r *recordedReads) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sent = append(r.sent, recordedRead{sql: sql, args: args})
	return r.pool.Query(ctx, sql, args...)
}

func (r *recordedReads) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.sent = append(r.sent, recordedRead{sql: sql, args: args})
	return r.pool.QueryRow(ctx, sql, args...)
}

// The reader of the pull requests whose answer is not known (PullRequestsNotKnown) reads the rows
// of one repository whose messages are not read through pull_request_texts_messages_not_read. No
// node of its plan scans pull_request_heads or pull_request_texts. 5,000 merged pull requests of
// acme/widgets and 5,000 of acme/gadgets have current texts, and 20 of them have no messages. In
// acme/widgets, 8 of those name a held issue in their branch, so the rule decides them without the
// messages. #18 and #20 name nothing, so their answer is not known. The test explains each
// statement that the reader sent (recordedReads). Control in the same test: with the index
// dropped, the same read scans pull_request_texts. The probe of the plan
// (zz_r18_notknown_cost_test.go) read 1.14 ms with the index and 1.72 ms without it, the best of
// 20 reads.
func TestTheReaderOfPullRequestsWhoseAnswerIsNotKnownUsesItsIndex(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	held := seedHeadIssue(t, ctx, pool, 1)
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("store 5,000 merged rows of each of two repositories", `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, url, title, event_at, checked_at, github_read_at, issue_key, attribution_source)
		select case when n % 2 = 0 then 'acme/widgets' else 'acme/gadgets' end, n, lpad(to_hex(n), 40, 'a'),
			case when n between 1 and 17 then 'legion/' || $1::text else 'work/' || n end, 'main', 'merged',
			'https://github.com/acme/x/pull/' || n, 'Change ' || n, '2026-11-18T01:00:00Z', now(), now(), null, 'none'
		from generate_series(1, 10000) n`, held)
	exec("store their texts, with no messages for #1 to #20", `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		select h.repo, h.number, h.head_sha, h.head_ref, h.repo, 'main', h.title, 'A body of change ' || h.number,
			case when h.number <= 20 then null else array['Work.']::text[] end, h.event_at
		from pull_request_heads h`)
	for _, table := range []string{"pull_request_heads", "pull_request_texts"} {
		exec("analyze "+table, `analyze `+table)
	}

	// read reads PullRequestsNotKnown(acme/widgets) 20 times, wants #18 and #20 from each read,
	// and answers the plan of each statement that the last read sent, with the tables that a node
	// of that plan scanned.
	read := func(t *testing.T) (plan, scanned []string) {
		t.Helper()
		var best time.Duration
		var numbers []int
		for i := range 20 {
			recorder := &recordedReads{pool: pool}
			began := time.Now()
			pulls, err := PullRequestsNotKnown(ctx, recorder, "acme/widgets")
			took := time.Since(began)
			if err != nil {
				t.Fatalf("PullRequestsNotKnown(acme/widgets): %v", err)
			}
			if i == 0 || took < best {
				best = took
			}
			numbers = numbers[:0]
			for _, pull := range pulls {
				numbers = append(numbers, pull.Number)
			}
			if !slices.Equal(numbers, []int{18, 20}) {
				t.Fatalf("PullRequestsNotKnown(acme/widgets) = %v, want [18 20]", numbers)
			}
			if i < 19 {
				continue
			}
			if len(recorder.sent) == 0 {
				t.Fatal("PullRequestsNotKnown(acme/widgets) sent no statement through its Querier, so no plan of it can be read")
			}
			for _, sent := range recorder.sent {
				lines, tables := planOf(t, ctx, pool, sent.sql, sent.args...)
				plan, scanned = append(plan, lines...), append(scanned, tables...)
			}
		}
		t.Logf("not_known=%v best_of_20=%s sequential_scans=%v indexes=%v", numbers, best, scanned, indexesRead(plan))
		return plan, scanned
	}

	t.Run("with_the_index_of_the_migration", func(t *testing.T) {
		plan, scanned := read(t)
		for _, table := range []string{"pull_request_heads", "pull_request_texts"} {
			if slices.Contains(scanned, table) {
				t.Errorf("PullRequestsNotKnown(acme/widgets) scans %s:\n%s", table, strings.Join(plan, "\n"))
			}
		}
		if !slices.Contains(indexesRead(plan), "pull_request_texts_messages_not_read") {
			t.Errorf("PullRequestsNotKnown(acme/widgets) reads no pull_request_texts_messages_not_read:\n%s", strings.Join(plan, "\n"))
		}
	})
	t.Run("control_without_the_index", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `drop index pull_request_texts_messages_not_read`); err != nil {
			t.Fatalf("drop the index of the migration: %v", err)
		}
		plan, scanned := read(t)
		if !slices.Contains(scanned, "pull_request_texts") {
			t.Errorf("the control: without pull_request_texts_messages_not_read, PullRequestsNotKnown(acme/widgets) scans no pull_request_texts, so the check above cannot fail:\n%s", strings.Join(plan, "\n"))
		}
	})
}
