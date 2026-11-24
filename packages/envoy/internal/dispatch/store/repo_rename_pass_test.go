package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// renamedRepositoriesSeed writes a row under the old name sjawhar/legion in every field of the
// pass, in mixed case where a field takes it, and beside each a row of sjawhar/legion-smoke or
// sjawhar/dotfiles, which only begin like the old name or share its owner. updated_at is a fixed
// time on every table that has one, so a test can tell the pass left it alone. The rename pair is
// a repo_projects row, as production holds it, and one stored event names the old name.
const renamedRepositoriesSeed = `
	insert into projects (key, name, default_assignee, default_repo) values
		('INFRA', 'Infra', 'alice', 'sjawhar/legion'),
		('SMOKE', 'Smoke', 'alice', 'sjawhar/legion-smoke');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('SJawhar/Legion', 'INFRA', '{"kind":"user","id":"alice"}', 'CybermonkLabs/Legion'),
		('sjawhar/legion-smoke', 'SMOKE', '{"kind":"user","id":"alice"}', null);
	insert into issues (key, project_key, number, title, created_by, rank, repo, updated_at) values
		('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U1', 'SJawhar/Legion', '2026-01-01T00:00:00Z'),
		('INFRA-2', 'INFRA', 2, 'Two', '{"kind":"user","id":"alice"}', 'U2', 'sjawhar/legion', '2026-01-01T00:00:00Z'),
		('INFRA-3', 'INFRA', 3, 'Three', '{"kind":"user","id":"alice"}', 'U3', 'sjawhar/legion-smoke', '2026-01-01T00:00:00Z'),
		('INFRA-4', 'INFRA', 4, 'Four', '{"kind":"user","id":"alice"}', 'U4', 'sjawhar/dotfiles', '2026-01-01T00:00:00Z');
	insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by) values
		('7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'INFRA-1', 'INFRA', 'verification', 'verification.md', 'doc', '{"kind":"user","id":"alice"}');
	insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, actor, commit_repo) values
		('INFRA-1', 'INFRA-1', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'c1', 1, 'digest', 'tester', repeat('a', 40), 'pass',
			'{"kind":"user","id":"alice"}', 'SJawhar/Legion'),
		('INFRA-1', 'INFRA-1', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'c2', 1, 'digest', 'tester', repeat('b', 40), 'pass',
			'{"kind":"user","id":"alice"}', 'sjawhar/legion-smoke'),
		('INFRA-1', 'INFRA-1', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'c3', 1, 'digest', 'tester', repeat('c', 40), 'pass',
			'{"kind":"user","id":"alice"}', null);
	insert into ci_check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, run, commit_sha, branch, verdict, attestation, session, actor, commit_repo) values
		('INFRA-1', 'INFRA-1', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'c4', 1, 'digest', 'go-checks', repeat('d', 40),
			'legion/INFRA-1', 'pass', '{}', 'ci-run', '{"kind":"session","id":"ci-run"}', 'SJawhar/Legion'),
		('INFRA-1', 'INFRA-1', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'c5', 1, 'digest', 'go-checks', repeat('e', 40),
			'legion/INFRA-1', 'pass', '{}', 'ci-run', '{"kind":"session","id":"ci-run"}', null);
	insert into issue_external_links (issue_key, url, kind) values
		('INFRA-1', 'https://github.com/SJawhar/Legion/pull/7', 'url'),
		('INFRA-2', 'https://github.com/sjawhar/legion/issues/1955', 'github_issue'),
		('INFRA-3', 'https://github.com/sjawhar/legion-smoke/issues/599', 'github_issue'),
		('INFRA-4', 'https://github.com/sjawhar/dotfiles/pull/3', 'url');
	insert into pull_request_heads (repo, number, head_sha, state, url, attribution_cited_issues) values
		('sjawhar/legion', 7, 'a7', 'merged', 'https://github.com/sjawhar/legion/pull/7',
			'{https://github.com/SJawhar/Legion/issues/5,https://github.com/sjawhar/legion-smoke/issues/6,https://github.com/cybermonklabs/legion/issues/5}'),
		('sjawhar/legion-smoke', 8, 'a8', 'merged', 'https://github.com/sjawhar/legion-smoke/pull/8', null);
	insert into branch_commits (repo, branch, sha, on_branch) values
		('SJawhar/Legion', 'main', 'c1', true),
		('sjawhar/dotfiles', 'main', 'c1', true);
	insert into delivery_pull_requests (repo, number, title, url, author, updated_at,
			attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys) values
		('sjawhar/legion', 11, 'Eleven', 'https://github.com/sjawhar/legion/pull/11', 'octocat', '2026-01-01T00:00:00Z',
			'{}', '{https://github.com/SJawhar/Legion/issues/5,https://github.com/sjawhar/legion-smoke/issues/6,https://github.com/cybermonklabs/legion/issues/5}', '{}', '{}'),
		('sjawhar/legion-smoke', 12, 'Twelve', 'https://github.com/sjawhar/legion-smoke/pull/12', 'octocat', '2026-01-01T00:00:00Z',
			null, null, null, null);
	insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, completed_at,
			conclusion, url, head_branch, event, jobs_complete, created_at) values
		('sjawhar/legion', 100, 'deploy', 11, 'h100', '2026-01-01T00:00:00Z', '2026-01-01T00:01:00.123456Z', '2026-01-01T00:09:00Z',
			'success', 'https://github.com/sjawhar/legion/actions/runs/100', 'main', 'push', true, '2026-01-01T00:00:30Z'),
		('sjawhar/legion-smoke', 101, 'deploy', null, 'h101', '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z', null,
			null, 'https://github.com/sjawhar/legion-smoke/actions/runs/101', 'main', 'push', false, null);
	insert into delivery_run_jobs (repo, run_id, name, conclusion) values
		('sjawhar/legion', 100, 'build', 'success'),
		('sjawhar/legion', 100, 'deploy', 'success'),
		('sjawhar/legion-smoke', 101, 'build', null);
	insert into delivery_run_warnings (repo, run_id, job_names, apply_step) values
		('sjawhar/legion', 100, '{deploy}', 'apply'),
		('sjawhar/legion-smoke', 101, '{build}', 'apply');
	insert into delivery_changes (kind, run_kind, repo, ref) values
		('run', 'deploy', 'SJawhar/Legion', '100'),
		('run', 'deploy', 'sjawhar/legion-smoke', '101');
	insert into delivery_settings (deploy_repo, deploy_workflow_path, pr_checks_workflow_path, population_authors,
			excluded_repos, updated_by, updated_at, production_job_names) values
		('sjawhar/legion', '.github/workflows/deploy.yml', '.github/workflows/pr.yml', '{octocat}',
			'{SJawhar/Legion,sjawhar/legion-smoke}', '{"kind":"user","id":"alice"}', '2026-01-01T00:00:00Z', '{release}');
	insert into delivery_reconcile_progress (step, scope, through, updated_at) values
		('search', 'authors=octocat excluded=SJawhar/Legion,sjawhar/legion-smoke deploy=sjawhar/legion', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
		('runs:deploy', 'sjawhar/legion .github/workflows/deploy.yml', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
		('runs:smoke', 'sjawhar/legion-smoke .github/workflows/deploy.yml', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
	insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
		('INFRA', 'sjawhar/legion', 'main', 1, '{"kind":"user","id":"alice"}'),
		('SMOKE', 'sjawhar/legion-smoke', 'main', 1, '{"kind":"user","id":"alice"}');
	insert into ci_reconcile_passes (instance, cursor_repository) values
		(gen_random_uuid(), 'SJawhar/Legion'),
		(gen_random_uuid(), 'sjawhar/dotfiles');
	insert into events (project_key, seq, type, actor, payload, notify) values
		('INFRA', 1, 'settings.repo_project.updated', '{"kind":"user","id":"alice"}',
			'{"repo":"SJawhar/Legion","project":"INFRA","renamed_to":"CybermonkLabs/Legion"}', false);
`

// renamedRepositoriesAfter reads, after a pass over renamedRepositoriesSeed, every value the seed
// wrote in a field of the pass, each with what it must read.
var renamedRepositoriesAfter = map[string]string{
	`select string_agg(key || '=' || coalesce(repo, ''), ' ' order by key) from issues`:                                            "INFRA-1=cybermonklabs/legion INFRA-2=cybermonklabs/legion INFRA-3=sjawhar/legion-smoke INFRA-4=sjawhar/dotfiles",
	`select string_agg(key || '=' || default_repo, ' ' order by key) from projects`:                                                "INFRA=cybermonklabs/legion SMOKE=sjawhar/legion-smoke",
	`select string_agg(check_block_id || '=' || coalesce(commit_repo, 'null'), ' ' order by check_block_id) from check_results`:    "c1=cybermonklabs/legion c2=sjawhar/legion-smoke c3=null",
	`select string_agg(check_block_id || '=' || coalesce(commit_repo, 'null'), ' ' order by check_block_id) from ci_check_results`: "c4=cybermonklabs/legion c5=null",
	`select string_agg(url, ' ' order by issue_key) from issue_external_links`: "https://github.com/cybermonklabs/legion/pull/7 https://github.com/cybermonklabs/legion/issues/1955 " +
		"https://github.com/sjawhar/legion-smoke/issues/599 https://github.com/sjawhar/dotfiles/pull/3",
	`select string_agg(repo || '#' || number || ' ' || url || ' ' || coalesce(attribution_cited_issues::text, 'null'), ' ' order by number) from pull_request_heads`: "cybermonklabs/legion#7 https://github.com/cybermonklabs/legion/pull/7 " +
		"{https://github.com/cybermonklabs/legion/issues/5,https://github.com/sjawhar/legion-smoke/issues/6} " +
		"sjawhar/legion-smoke#8 https://github.com/sjawhar/legion-smoke/pull/8 null",
	`select string_agg(repo, ' ' order by repo) from branch_commits`: "cybermonklabs/legion sjawhar/dotfiles",
	`select string_agg(repo || '#' || number || ' ' || url || ' ' || coalesce(attribution_cited_issues::text, 'null'), ' ' order by number) from delivery_pull_requests`: "cybermonklabs/legion#11 https://github.com/cybermonklabs/legion/pull/11 " +
		"{https://github.com/cybermonklabs/legion/issues/5,https://github.com/sjawhar/legion-smoke/issues/6} " +
		"sjawhar/legion-smoke#12 https://github.com/sjawhar/legion-smoke/pull/12 null",
	`select string_agg(repo || '#' || run_id || ' ' || url, ' ' order by run_id) from delivery_runs`: "cybermonklabs/legion#100 https://github.com/cybermonklabs/legion/actions/runs/100 " +
		"sjawhar/legion-smoke#101 https://github.com/sjawhar/legion-smoke/actions/runs/101",
	`select string_agg(repo || '#' || run_id || ' ' || name, ' ' order by run_id, name) from delivery_run_jobs`: "cybermonklabs/legion#100 build cybermonklabs/legion#100 deploy sjawhar/legion-smoke#101 build",
	`select string_agg(repo || '#' || run_id, ' ' order by run_id) from delivery_run_warnings`:                  "cybermonklabs/legion#100 sjawhar/legion-smoke#101",
	`select count(*)::text from delivery_changes where lower(repo) = 'sjawhar/legion'`:                          "0",
	`select deploy_repo || ' ' || excluded_repos::text from delivery_settings`:                                  "cybermonklabs/legion {cybermonklabs/legion,sjawhar/legion-smoke}",
	`select string_agg(step || '=' || scope, ' | ' order by step) from delivery_reconcile_progress`: "runs:deploy=cybermonklabs/legion .github/workflows/deploy.yml | " +
		"runs:smoke=sjawhar/legion-smoke .github/workflows/deploy.yml | " +
		"search=authors=octocat excluded=cybermonklabs/legion,sjawhar/legion-smoke deploy=cybermonklabs/legion",
	`select string_agg(project_key || '=' || repo, ' ' order by project_key) from architecture_sources`: "INFRA=cybermonklabs/legion SMOKE=sjawhar/legion-smoke",
	`select string_agg(cursor_repository, ' ' order by cursor_repository) from ci_reconcile_passes`:     "cybermonklabs/legion sjawhar/dotfiles",
}

// untouchedByThePass reads what the pass must leave as it found it: the mapping, the event log,
// and updated_at on every table the seed wrote it on.
const untouchedByThePass = `
	select jsonb_build_object(
		'repo_projects', (select jsonb_agg(to_jsonb(r) order by r.repo) from repo_projects r),
		'events', (select jsonb_agg(to_jsonb(e) order by e.id) from events e),
		'updated_at', jsonb_build_array(
			(select array_agg(updated_at order by key) from issues),
			(select array_agg(updated_at order by number) from delivery_pull_requests),
			(select array_agg(updated_at) from delivery_settings),
			(select array_agg(updated_at order by step) from delivery_reconcile_progress)))::text`

// passLog is one record the pass or its test logged, decoded from slog's JSON handler.
type passLog map[string]any

// capturePassLogs sends slog's default logger to a JSON buffer until t ends, and answers a reader
// of the records logged so far.
func capturePassLogs(t *testing.T) func() []passLog {
	t.Helper()
	var buffer bytes.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&buffer, nil))
	return func() []passLog {
		var records []passLog
		for line := range strings.SplitSeq(strings.TrimSpace(buffer.String()), "\n") {
			if line == "" {
				continue
			}
			var record passLog
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode the log line %q: %v", line, err)
			}
			records = append(records, record)
		}
		buffer.Reset()
		return records
	}
}

// named answers the records whose message is msg.
func named(records []passLog, msg string) []passLog {
	var found []passLog
	for _, record := range records {
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	return found
}

const (
	rewroteLine = "dispatch: rewrote a renamed repository's old name"
	deletedLine = "dispatch: deleted a row whose key the renamed repository's new name already holds"
	failedLine  = "dispatch: rewrite a renamed repository's old name"
)

// noneLeft is a count of the rows left that ran and found none.
var noneLeft = RepoRenamesLeft{Counted: true}

// openRenamedRepositoriesStore is a migrated store holding seed, and the pairs the boot reads
// from it.
func openRenamedRepositoriesStore(t *testing.T, seed string) (*Store, []RepoRename) {
	t.Helper()
	ctx := context.Background()
	store := migratedEmptyStore(t)
	if _, err := store.Pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pairs, err := ReadRepoRenames(ctx, store.Pool)
	if err != nil {
		t.Fatalf("read the pairs: %v", err)
	}
	return store, pairs
}

// readText answers query's one text value.
func readText(t *testing.T, store *Store, query string) string {
	t.Helper()
	var value *string
	if err := store.Pool.QueryRow(context.Background(), query).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if value == nil {
		return "null"
	}
	return *value
}

// The pass writes the new name in every field that holds the old name, in any case, and leaves
// sjawhar/legion-smoke and sjawhar/dotfiles as they are. It logs each field it wrote, and a second
// run writes nothing. It leaves the mapping, the event log and updated_at as they were, and
// appends no event. A run it moves keeps every other column.
func TestTheRenamePassWritesEveryFieldAndASecondRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedRepositoriesSeed)
	untouched := readText(t, store, untouchedByThePass)
	const runColumns = `select (to_jsonb(r) - 'repo' - 'url')::text from delivery_runs r where run_id = 100`
	run := readText(t, store, runColumns)
	logs := capturePassLogs(t)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Pairs != 1 || pass.Deleted != 0 || pass.Failures != 0 || pass.Left != noneLeft || pass.Err() != nil {
		t.Fatalf("the pass = %+v (%v), want one pair, nothing deleted, no failure and nothing left", pass, pass.Err())
	}
	for query, want := range renamedRepositoriesAfter {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}
	if got := readText(t, store, untouchedByThePass); got != untouched {
		t.Errorf("the mapping, the events or an updated_at changed:\nbefore %s\nafter  %s", untouched, got)
	}
	if got := readText(t, store, runColumns); got != run {
		t.Errorf("the moved run's other columns:\nbefore %s\nafter  %s", run, got)
	}
	written := map[string]float64{}
	var rows int64
	for _, record := range named(logs(), rewroteLine) {
		if record["level"] != "INFO" || record["from"] != "sjawhar/legion" || record["to"] != "cybermonklabs/legion" {
			t.Errorf("a field's line = %v, want INFO from sjawhar/legion to cybermonklabs/legion", record)
		}
		written[record["field"].(string)] += record["rows"].(float64)
		rows += int64(record["rows"].(float64))
	}
	for field, want := range map[string]float64{
		"issues.repo": 2, "projects.default_repo": 1, "check_results.commit_repo": 1, "ci_check_results.commit_repo": 1,
		"issue_external_links.url": 2,
		"pull_request_heads.repo":  1, "pull_request_heads.url": 1, "pull_request_heads.attribution_cited_issues": 1, "branch_commits.repo": 1,
		"delivery_pull_requests.repo": 1, "delivery_pull_requests.url": 1, "delivery_pull_requests.attribution_cited_issues": 1,
		"delivery_runs.repo": 1, "delivery_run_jobs.repo": 2, "delivery_runs.url": 1, "delivery_run_warnings.repo": 1,
		"delivery_settings.deploy_repo": 1, "delivery_settings.excluded_repos": 1, "delivery_reconcile_progress.scope": 2,
		"architecture_sources.repo": 1, "ci_reconcile_passes.cursor_repository": 1,
	} {
		if written[field] != want {
			t.Errorf("logged %v rows of %s, want %v (every line: %v)", written[field], field, want, written)
		}
	}
	// The delivery tables' triggers log each change of theirs to delivery_changes under the name
	// the row had, the seed's inserts and the pass's own deletes of old rows included, so the
	// line written after them finds those too.
	if written["delivery_changes.repo"] < 1 {
		t.Errorf("logged no rows of delivery_changes.repo (every line: %v)", written)
	}
	if rows != pass.Rows {
		t.Errorf("the field lines sum to %d rows, the pass counted %d", rows, pass.Rows)
	}

	changes := readText(t, store, `select count(*)::text from delivery_changes`)
	again := RewriteRenamedRepositories(ctx, store.Pool, pairs)
	if again != (RepoRenamePass{Pairs: 1, Left: noneLeft}) {
		t.Errorf("a second pass = %+v, want one pair and nothing written, deleted, failed or left", again)
	}
	if lines := named(logs(), rewroteLine); len(lines) != 0 {
		t.Errorf("a second pass logged writes: %v", lines)
	}
	if got := readText(t, store, `select count(*)::text from delivery_changes`); got != changes {
		t.Errorf("a second pass logged %s delivery changes, want the %s the first left", got, changes)
	}
	if got := readText(t, store, untouchedByThePass); got != untouched {
		t.Errorf("a second pass changed the mapping, the events or an updated_at:\nbefore %s\nafter  %s", untouched, got)
	}
}

// A pass that finds no row on an old name before its lines runs none of them. A second run on a
// written database meets a trigger that refuses every statement on each table of the pass, even
// one that meets no row, and is not refused: it reports its pair, no row, no failure and none
// left.
func TestASecondRenamePassOnAWrittenDatabaseRunsNoLine(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedRepositoriesSeed)
	if first := RewriteRenamedRepositories(ctx, store.Pool, pairs); first.Failures != 0 || first.Left != noneLeft {
		t.Fatalf("the first pass = %+v, want no failure and nothing left", first)
	}
	if _, err := store.Pool.Exec(ctx, `
		create function refuse_rename_pass() returns trigger language plpgsql as $$
		begin raise exception '% refuses statements', tg_table_name; end $$`); err != nil {
		t.Fatalf("create the refusing trigger function: %v", err)
	}
	tables := map[string]bool{}
	r := repoRename{RepoRename: pairs[0], url: NewRepoURLRule(pairs[0].From, pairs[0].To)}
	for _, line := range repoRenameLines {
		for _, step := range line.steps(r) {
			tables[step.table] = true
		}
	}
	for table := range tables {
		if _, err := store.Pool.Exec(ctx, `create trigger refuse_rename_pass before insert or update or delete on `+table+`
			for each statement execute function refuse_rename_pass()`); err != nil {
			t.Fatalf("make %s refuse statements: %v", table, err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `update issues set repo = repo where false`); err == nil {
		t.Fatal("a statement that meets no row went past the trigger, so the test cannot tell a line ran")
	}
	logs := capturePassLogs(t)

	again := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if again != (RepoRenamePass{Pairs: 1, Left: noneLeft}) || again.Err() != nil {
		t.Fatalf("a second pass = %+v (%v), want one pair and nothing written, deleted, failed or left", again, again.Err())
	}
	if failed := named(logs(), failedLine); len(failed) != 0 {
		t.Fatalf("a second pass ran a line: %v", failed)
	}
}

// renameCollisionsSeed holds, for each table whose key holds a repository, a row under the old
// name and a twin under the new name with the same key, and beside some a row under the old name
// with no twin. The twin of the delivery pull request is in mixed case. Two pull request heads
// and two links are each under the old name in two spellings.
const renameCollisionsSeed = `
	insert into projects (key, name, default_assignee) values ('INFRA', 'Infra', 'alice');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('sjawhar/legion', 'INFRA', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
	insert into issues (key, project_key, number, title, created_by, rank) values
		('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U1'),
		('INFRA-2', 'INFRA', 2, 'Two', '{"kind":"user","id":"alice"}', 'U2');
	insert into issue_external_links (issue_key, url, kind) values
		('INFRA-1', 'https://github.com/sjawhar/legion/issues/5', 'github_issue'),
		('INFRA-2', 'https://github.com/cybermonklabs/legion/issues/5', 'github_issue'),
		('INFRA-1', 'https://github.com/SJawhar/Legion/pull/9', 'url'),
		('INFRA-2', 'https://github.com/sjawhar/legion/pull/9', 'url');
	insert into pull_request_heads (repo, number, head_sha, state, url) values
		('sjawhar/legion', 7, 'old', 'merged', 'https://github.com/sjawhar/legion/pull/7'),
		('cybermonklabs/legion', 7, 'new', 'merged', 'https://github.com/cybermonklabs/legion/pull/7'),
		('SJawhar/Legion', 9, 'one', 'open', ''),
		('sjawhar/legion', 9, 'two', 'open', '');
	insert into branch_commits (repo, branch, sha, on_branch) values
		('sjawhar/legion', 'main', 'c1', false),
		('cybermonklabs/legion', 'main', 'c1', true);
	insert into delivery_pull_requests (repo, number, title, url, author) values
		('sjawhar/legion', 11, 'Old', 'https://github.com/sjawhar/legion/pull/11', 'octocat'),
		('CybermonkLabs/Legion', 11, 'New', 'https://github.com/CybermonkLabs/Legion/pull/11', 'octocat');
	insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, url) values
		('sjawhar/legion', 100, 'deploy', 'old', now(), now(), 'https://github.com/sjawhar/legion/actions/runs/100'),
		('cybermonklabs/legion', 100, 'deploy', 'new', now(), now(), 'https://github.com/cybermonklabs/legion/actions/runs/100'),
		('sjawhar/legion', 101, 'deploy', 'moved', now(), now(), 'https://github.com/sjawhar/legion/actions/runs/101');
	insert into delivery_run_jobs (repo, run_id, name) values
		('sjawhar/legion', 100, 'old-build'),
		('sjawhar/legion', 100, 'old-deploy'),
		('cybermonklabs/legion', 100, 'twin-build'),
		('sjawhar/legion', 101, 'moved-build');
	insert into delivery_run_warnings (repo, run_id, job_names, apply_step) values
		('sjawhar/legion', 100, '{deploy}', 'apply'),
		('cybermonklabs/legion', 100, '{deploy}', 'apply'),
		('sjawhar/legion', 100, '{other}', 'apply');
	insert into architecture_snapshots (project_key, commit, files) values ('INFRA', 'c1', '{}');
	insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
		('INFRA', 'sjawhar/legion', 'old', 1, '{"kind":"user","id":"alice"}'),
		('INFRA', 'cybermonklabs/legion', 'new', 1, '{"kind":"user","id":"alice"}');
	insert into components (project_key, id, title, prose, snapshot_id, source_id)
		select 'INFRA', c.id, c.id, '', (select id from architecture_snapshots), s.id
		from architecture_sources s
		join (values ('sjawhar/legion', 'old-api'), ('sjawhar/legion', 'old-store'), ('cybermonklabs/legion', 'new-api')) c(repo, id)
			on c.repo = s.repo;
`

// Where the new name gives a row the key of another row, the row under the new name stays, in
// whatever case it is, and the row under the old name is deleted and logged with its table and
// key. A run with a twin goes with its jobs while the twin keeps its own, and a run with no twin
// is moved with its jobs. A source with a twin goes with its components. Of two rows under the
// old name with one key, one stays and takes the new name.
func TestTheRenamePassKeepsTheNewNameRowOfEachCollision(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renameCollisionsSeed)
	logs := capturePassLogs(t)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Deleted != 9 || pass.Failures != 0 || pass.Left != noneLeft {
		t.Fatalf("the pass = %+v, want 9 rows deleted, no failure and nothing left", pass)
	}
	for query, want := range map[string]string{
		`select string_agg(issue_key || ' ' || url, ' | ' order by url) from issue_external_links where url like '%/issues/5'`: "INFRA-2 https://github.com/cybermonklabs/legion/issues/5",
		`select string_agg(url, ' | ') from issue_external_links where url ilike '%/pull/9'`:                                   "https://github.com/cybermonklabs/legion/pull/9",
		`select string_agg(repo || ' ' || head_sha, ' | ' order by number) from pull_request_heads where number = 7`:           "cybermonklabs/legion new",
		`select string_agg(repo, ' | ') from pull_request_heads where number = 9`:                                              "cybermonklabs/legion",
		`select string_agg(repo || ' ' || on_branch, ' | ') from branch_commits`:                                               "cybermonklabs/legion true",
		`select string_agg(repo || ' ' || title, ' | ') from delivery_pull_requests`:                                           "CybermonkLabs/Legion New",
		`select string_agg(repo || '#' || run_id || ' ' || head_sha, ' | ' order by run_id) from delivery_runs`:                "cybermonklabs/legion#100 new | cybermonklabs/legion#101 moved",
		`select string_agg(repo || '#' || run_id || ' ' || name, ' | ' order by run_id, name) from delivery_run_jobs`:          "cybermonklabs/legion#100 twin-build | cybermonklabs/legion#101 moved-build",
		`select string_agg(repo || ' ' || job_names::text, ' | ' order by job_names) from delivery_run_warnings`:               "cybermonklabs/legion {deploy} | cybermonklabs/legion {other}",
		`select string_agg(repo || ' ' || branch, ' | ') from architecture_sources`:                                            "cybermonklabs/legion new",
		`select string_agg(id, ' | ' order by id) from components`:                                                             "new-api",
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}
	deleted := map[string]int{}
	for _, record := range named(logs(), deletedLine) {
		if record["level"] != "WARN" || record["from"] != "sjawhar/legion" || record["to"] != "cybermonklabs/legion" {
			t.Errorf("a deletion's line = %v, want WARN from sjawhar/legion to cybermonklabs/legion", record)
		}
		key, _ := record["key"].(string)
		if !strings.Contains(strings.ToLower(key), "sjawhar/legion") {
			t.Errorf("a deletion's key = %q, want the deleted row's key under the old name", key)
		}
		deleted[record["table"].(string)]++
	}
	want := map[string]int{
		"issue_external_links": 2, "pull_request_heads": 2, "branch_commits": 1, "delivery_pull_requests": 1,
		"delivery_runs": 1, "delivery_run_warnings": 1, "architecture_sources": 1,
	}
	if len(deleted) != len(want) {
		t.Errorf("logged deletions %v, want %v", deleted, want)
	}
	for table, count := range want {
		if deleted[table] != count {
			t.Errorf("logged %d deletions of %s, want %d (every table: %v)", deleted[table], table, count, deleted)
		}
	}
}

// A line whose table refuses its writes fails alone: the pass logs it at ERROR with its field,
// counts it a failure and the rows it left, and writes every other line. The pass after the
// table takes writes again writes that line, and nothing is left.
func TestARenamePassLineThatFailsLeavesTheOthersWritten(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedRepositoriesSeed)
	if _, err := store.Pool.Exec(ctx, `
		create function refuse_branch_commits() returns trigger language plpgsql as $$
		begin raise exception 'branch_commits refuses writes'; end $$;
		create trigger refuse_branch_commits before update or delete on branch_commits
			for each row execute function refuse_branch_commits();
	`); err != nil {
		t.Fatalf("make branch_commits refuse writes: %v", err)
	}
	logs := capturePassLogs(t)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Failures != 1 || pass.Left != (RepoRenamesLeft{Rows: 1, Counted: true}) || pass.Err() == nil {
		t.Fatalf("the pass = %+v (%v), want one failure and the one branch commit left", pass, pass.Err())
	}
	failed := named(logs(), failedLine)
	if len(failed) != 1 || failed[0]["level"] != "ERROR" || failed[0]["field"] != "branch_commits.repo" ||
		!strings.Contains(failed[0]["error"].(string), "branch_commits refuses writes") {
		t.Fatalf("the failure's lines = %v, want one ERROR naming branch_commits.repo and the trigger's error", failed)
	}
	if got := readText(t, store, `select string_agg(repo, ' ' order by repo collate "C") from branch_commits`); got != "SJawhar/Legion sjawhar/dotfiles" {
		t.Errorf("branch_commits = %s, want it as the seed wrote it", got)
	}
	for query, want := range renamedRepositoriesAfter {
		if strings.Contains(query, "branch_commits") {
			continue
		}
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}

	if _, err := store.Pool.Exec(ctx, `drop trigger refuse_branch_commits on branch_commits`); err != nil {
		t.Fatalf("let branch_commits take writes: %v", err)
	}
	again := RewriteRenamedRepositories(ctx, store.Pool, pairs)
	if again != (RepoRenamePass{Pairs: 1, Rows: 1, Left: noneLeft}) {
		t.Fatalf("the pass after the trigger went = %+v, want the one branch commit written and nothing left", again)
	}
	if got := readText(t, store, `select string_agg(repo, ' ' order by repo collate "C") from branch_commits`); got != "cybermonklabs/legion sjawhar/dotfiles" {
		t.Errorf("branch_commits after the trigger went = %s, want the old name written", got)
	}
}

// A pass whose bound has already run out fails every line, and still counts the rows its lines
// left, under a bound of its own: the count is the true count, never a 0 nothing counted.
func TestARenamePassOutOfTimeStillCountsWhatItLeft(t *testing.T) {
	store, pairs := openRenamedRepositoriesStore(t, `
		insert into projects (key, name, default_assignee, default_repo) values ('INFRA', 'Infra', 'alice', 'sjawhar/legion');
		insert into repo_projects (repo, project, created_by, renamed_to)
			values ('sjawhar/legion', 'INFRA', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
		insert into issues (key, project_key, number, title, created_by, rank, repo) values
			('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U1', 'SJawhar/Legion'),
			('INFRA-2', 'INFRA', 2, 'Two', '{"kind":"user","id":"alice"}', 'U2', 'sjawhar/legion');
		insert into issue_external_links (issue_key, url, kind)
			values ('INFRA-1', 'https://github.com/sjawhar/legion/issues/5', 'github_issue');
	`)
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	pass := RewriteRenamedRepositories(ended, store.Pool, pairs)

	want := RepoRenamePass{Pairs: 1, Failures: len(repoRenameLines), Left: RepoRenamesLeft{Rows: 4, Counted: true}}
	if pass != want || pass.Err() == nil {
		t.Fatalf("the pass out of time = %+v (%v), want every line failed and the 4 rows left counted: %+v", pass, pass.Err(), want)
	}
}

// A count of the rows left that fails is one more failure, and the pass says the rows left are
// unknown: its summary reads left=unknown, never left=0.
func TestARenamePassWhoseCountFailsSaysTheRowsLeftAreUnknown(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedRepositoriesSeed+`drop table ci_reconcile_passes;`)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Failures != 2 || pass.Left != (RepoRenamesLeft{}) || pass.Err() == nil {
		t.Fatalf("the pass = %+v (%v), want the line of the dropped table and the count failed, and the rows left unknown", pass, pass.Err())
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("summary", pass.LogArgs()...)
	if !strings.Contains(logs.String(), "failures=2 left=unknown") {
		t.Errorf("the summary's counts = %s, want failures=2 left=unknown", logs.String())
	}
	if !strings.Contains(pass.Err().Error(), "left unknown rows") {
		t.Errorf("the pass's error = %v, want it to say the rows left are unknown", pass.Err())
	}
}

// dispatch://INFRA-4411 drops repo_projects. The boot then reads no pairs, and the pass reads and
// writes nothing: every field keeps the name it holds.
func TestTheRenamePassWritesNothingOnceRepoProjectsIsDropped(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedRepositoriesSeed+`drop table repo_projects;`)
	if len(pairs) != 0 {
		t.Fatalf("the pairs with repo_projects dropped = %+v, want none", pairs)
	}

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass != (RepoRenamePass{Left: noneLeft}) || pass.Err() != nil {
		t.Fatalf("the pass with no pairs = %+v (%v), want nothing done and no error", pass, pass.Err())
	}
	if got := readText(t, store, `select string_agg(coalesce(repo, ''), ' ' order by key) from issues`); got != "SJawhar/Legion sjawhar/legion sjawhar/legion-smoke sjawhar/dotfiles" {
		t.Errorf("issues.repo with no pairs = %s, want each as the seed wrote it", got)
	}
}
