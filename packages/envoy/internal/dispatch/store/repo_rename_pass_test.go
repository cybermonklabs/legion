package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// renamedRepositoriesSeed writes a row under the old name sjawhar/legion in every field of the
// pass, in mixed case where a field takes it, and beside each a row of sjawhar/legion-smoke or
// sjawhar/dotfiles, which only begin like the old name or share its owner. updated_at is a fixed
// time on every table that has one, so a test can tell the pass left it alone. The rename pair is
// a repo_projects row, as production holds it, and one stored event names the old name. INFRA
// publishes the old name as its default, SMOKE holds stored eval figures of a cut of the old name
// in two spellings, and two cuts hold a stale mark already, one of them a cut the pass marks.
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
	insert into issue_facts (project_key, issue_key, repo) values
		('INFRA', 'INFRA-1', 'SJawhar/Legion'),
		('INFRA', 'INFRA-3', 'sjawhar/legion-smoke'),
		('INFRA', 'INFRA-4', null);
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
	insert into issue_external_links (issue_key, url, kind) values
		('INFRA-1', 'https://github.com/SJawhar/Legion/pull/7', 'url'),
		('INFRA-2', 'https://github.com/sjawhar/legion/issues/1955', 'github_issue'),
		('INFRA-3', 'https://github.com/sjawhar/legion-smoke/issues/599', 'github_issue'),
		('INFRA-4', 'https://github.com/sjawhar/dotfiles/pull/3', 'url');
	insert into pull_request_heads (repo, number, head_sha, state, url) values
		('sjawhar/legion', 7, 'a7', 'merged', 'https://github.com/sjawhar/legion/pull/7'),
		('sjawhar/legion-smoke', 8, 'a8', 'merged', 'https://github.com/sjawhar/legion-smoke/pull/8');
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
	insert into eval_days_defaults (project_key, repo) values ('INFRA', 'sjawhar/legion'), ('SMOKE', 'sjawhar/legion-smoke');
	insert into eval_range_figures (project_key, repo, days, end_day, eval_id, fold_key, figures, oldest_updated_at, built_by) values
		('SMOKE', 'SJawhar/Legion', 7, '2026-01-02', 'SMOKE/a', 1, '\x00', '2026-01-01T00:00:00Z', 'test'),
		('SMOKE', 'sjawhar/legion-smoke', 7, '2026-01-02', 'SMOKE/a', 1, '\x00', '2026-01-01T00:00:00Z', 'test');
	insert into eval_day_figures (project_key, repo, day, eval_id, fold_key, figures, rows_stamp, built_by) values
		('SMOKE', 'sjawhar/legion', '2026-01-01', 'SMOKE/a', 1, '\x00', 1, 'test');
	insert into eval_figure_cuts_stale (project_key, repo, generation, ready_through, next_day, evals) values
		('INFRA', 'cybermonklabs/legion', 2, '2026-01-03', '2026-01-01', '{INFRA/a}'),
		('SMOKE', 'sjawhar/legion-smoke', 1, '2026-01-03', '2026-01-01', '{SMOKE/b}');
	insert into events (project_key, seq, type, actor, payload, notify) values
		('INFRA', 1, 'settings.repo_project.updated', '{"kind":"user","id":"alice"}',
			'{"repo":"SJawhar/Legion","project":"INFRA","renamed_to":"CybermonkLabs/Legion"}', false);
`

// renamedRepositoriesAfter reads, after a pass over renamedRepositoriesSeed, every value the seed
// wrote in a field of the pass, each with what it must read.
var renamedRepositoriesAfter = map[string]string{
	`select string_agg(key || '=' || coalesce(repo, ''), ' ' order by key) from issues`:                                         "INFRA-1=cybermonklabs/legion INFRA-2=cybermonklabs/legion INFRA-3=sjawhar/legion-smoke INFRA-4=sjawhar/dotfiles",
	`select string_agg(key || '=' || default_repo, ' ' order by key) from projects`:                                             "INFRA=cybermonklabs/legion SMOKE=sjawhar/legion-smoke",
	`select string_agg(issue_key || '=' || coalesce(repo, 'null'), ' ' order by issue_key) from issue_facts`:                    "INFRA-1=cybermonklabs/legion INFRA-3=sjawhar/legion-smoke INFRA-4=null",
	`select string_agg(check_block_id || '=' || coalesce(commit_repo, 'null'), ' ' order by check_block_id) from check_results`: "c1=cybermonklabs/legion c2=sjawhar/legion-smoke c3=null",
	`select string_agg(url, ' ' order by issue_key) from issue_external_links`: "https://github.com/cybermonklabs/legion/pull/7 https://github.com/cybermonklabs/legion/issues/1955 " +
		"https://github.com/sjawhar/legion-smoke/issues/599 https://github.com/sjawhar/dotfiles/pull/3",
	`select string_agg(repo || '#' || number || ' ' || url, ' ' order by number) from pull_request_heads`: "cybermonklabs/legion#7 https://github.com/cybermonklabs/legion/pull/7 " +
		"sjawhar/legion-smoke#8 https://github.com/sjawhar/legion-smoke/pull/8",
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
	`select string_agg(project_key || '=' || repo, ' ' order by project_key) from eval_days_defaults`:   "INFRA=cybermonklabs/legion SMOKE=sjawhar/legion-smoke",
	// The cut of the old name, in each spelling a row holds it in, and of the new name, of each
	// project whose published default the pass moves from the old name, or that holds stored
	// figures of the old name: at generation 1, or at the next where a mark stands, for every eval,
	// its first paint and its refold to begin. The pass writes no stored figure, which the
	// refresher folds again.
	`select string_agg(project_key || ' ' || repo || ' ' || generation || ' ' || coalesce(ready_through::text, '-') || ' ' || coalesce(next_day::text, '-') || ' ' ||
		coalesce(evals::text, 'every'), ' | ' order by project_key, repo collate "C") from eval_figure_cuts_stale`: "INFRA cybermonklabs/legion 3 - - every | " +
		"INFRA sjawhar/legion 1 - - every | SMOKE SJawhar/Legion 1 - - every | SMOKE cybermonklabs/legion 1 - - every | SMOKE sjawhar/legion 1 - - every | " +
		"SMOKE sjawhar/legion-smoke 1 2026-01-03 2026-01-01 {SMOKE/b}",
	`select string_agg(repo, ' ' order by repo collate "C") from (select repo from eval_range_figures union all select repo from eval_day_figures) f`: "SJawhar/Legion sjawhar/legion sjawhar/legion-smoke",
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
// sjawhar/legion-smoke and sjawhar/dotfiles as they are. It marks stale the eval figures' cuts of
// the old and the new name of each project that publishes the old name or holds stored figures of
// it, for every eval, and no other cut. It logs each field it wrote, and a second run writes
// nothing. It leaves the mapping, the event log and updated_at as they were, and appends no event.
// A run it moves keeps every other column.
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
		"issues.repo": 2, "projects.default_repo": 1, "issue_facts.repo": 1, "check_results.commit_repo": 1, "issue_external_links.url": 2,
		"pull_request_heads.repo": 1, "pull_request_heads.url": 1, "branch_commits.repo": 1,
		"delivery_pull_requests.repo": 1, "delivery_pull_requests.url": 1, "delivery_pull_requests.attribution_cited_issues": 1,
		"delivery_runs.repo": 1, "delivery_run_jobs.repo": 2, "delivery_runs.url": 1, "delivery_run_warnings.repo": 1,
		"delivery_settings.deploy_repo": 1, "delivery_settings.excluded_repos": 1, "delivery_reconcile_progress.scope": 2,
		"architecture_sources.repo": 1, "ci_reconcile_passes.cursor_repository": 1, "eval_days_defaults.repo": 1,
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

// renamedDefaultSeed holds the rename pair sjawhar/legion to cybermonklabs/legion, INFRA, which
// publishes the old name as its default, and CYBER, which published the new name before the
// rename. Neither holds a stored eval figure or a stale mark.
const renamedDefaultSeed = `
	insert into projects (key, name, default_assignee, default_repo) values
		('INFRA', 'Infra', 'alice', 'sjawhar/legion'),
		('CYBER', 'Cyber', 'alice', 'cybermonklabs/legion');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('sjawhar/legion', 'INFRA', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
	insert into eval_days_defaults (project_key, repo) values ('INFRA', 'sjawhar/legion'), ('CYBER', 'cybermonklabs/legion');
`

// A run of the pass after one that committed, which runs its lines because a row a build before
// this one wrote during a roll names the old name, marks no cut through a default the run before
// it moved: every mark stands as that run and the refold since left it, the old name's cut, whose
// refold is done, unmarked, and the new name's at its generation, its first paint folded and its
// refold begun. Neither run marks a cut of CYBER, which published the new name before the rename
// and holds no stored figure of the old name. Red where the line marks the cuts of every project
// that publishes the new name once it has written the defaults: the first run then marks CYBER's
// two cuts, and the second marks both cuts of each project again, at the next generation, with
// their first paints owed and their refolds to begin again.
func TestARenamePassAfterOneThatCommittedLeavesTheMarksItMade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedDefaultSeed)
	const marks = `select coalesce(string_agg(project_key || ' ' || repo || ' ' || generation || ' ' || coalesce(ready_through::text, '-') || ' ' ||
		coalesce(next_day::text, '-') || ' ' || coalesce(evals::text, 'every'), ' | ' order by project_key, repo collate "C"), 'none') from eval_figure_cuts_stale`
	if first := RewriteRenamedRepositories(ctx, store.Pool, pairs); first.Failures != 0 || first.Left != noneLeft {
		t.Fatalf("the first pass = %+v, want no failure and nothing left", first)
	}
	if got, want := readText(t, store, marks), "INFRA cybermonklabs/legion 1 - - every | INFRA sjawhar/legion 1 - - every"; got != want {
		t.Errorf("the first pass left the marks %s, want %s", got, want)
	}
	if _, err := store.Pool.Exec(ctx, `
		delete from eval_figure_cuts_stale where project_key = 'INFRA' and repo = 'sjawhar/legion';
		update eval_figure_cuts_stale set ready_through = '2026-01-03', next_day = '2026-01-01' where project_key = 'INFRA' and repo = 'cybermonklabs/legion';
		insert into issues (key, project_key, number, title, created_by, rank, repo, updated_at) values
			('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U1', 'sjawhar/legion', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("fold the marked cuts and write an issue under the old name: %v", err)
	}
	folded := readText(t, store, marks)

	again := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if again != (RepoRenamePass{Pairs: 1, Rows: 1, Left: noneLeft}) {
		t.Fatalf("the second pass = %+v, want the one issue written and nothing left", again)
	}
	if got := readText(t, store, marks); got != folded {
		t.Errorf("the second pass left the marks %s, want them as the first pass and the refold left them: %s", got, folded)
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

// renameOwnersSeed holds the rename pair sjawhar/legion to cybermonklabs/legion and no row of the
// new name. Under owner keys of the old name it holds a day row of one eval, series and day under
// the plain key and under a key that carries a list, and a day row of another series under each of
// two spellings of the old name in a key with a sharer. Each of the four keys holds a member of
// the same pull request and an owed day, and the eval_days change trigger records each in
// eval_day_owners. Two spellings of the old name in a key with another sharer hold an owner row
// and no day row, as a key whose rows are all gone keeps its row.
const renameOwnersSeed = `
	insert into projects (key, name, default_assignee) values ('INFRA', 'Infra', 'alice');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('sjawhar/legion', 'INFRA', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
	insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		select owner, owner || '/deploy-frequency', series, '', '2026-11-03', true, n, n, 'test'
		from (values (':sjawhar/legion|', 'deploys', 1), (':sjawhar/legion||INFRA>', 'deploys', 2),
			(':SJawhar/Legion|INFRA', 'shared', 3), (':sjawhar/legion|INFRA', 'shared', 3)) as k(owner, series, n);
	insert into eval_day_members (project_key, family, row_key, day)
		select project_key, 'delivery', 'pull_request:sjawhar/legion#7', day from eval_days;
	insert into eval_days_owed (project_key, family, day)
		select project_key, 'delivery', day from eval_days;
	insert into eval_day_owners (owner, repo, sharers) values
		(':SJawhar/Legion|SMOKE', 'SJawhar/Legion', '{SMOKE}'),
		(':sjawhar/legion|SMOKE', 'sjawhar/legion', '{SMOKE}');
`

// The rename writes the new name in the repository of an owner key and keeps its sharers and its
// lists, so the plain key of the old name and a key that carries a list rename to two keys: the
// day rows of one eval, series and day under each both stay, each under its renamed key and eval
// id, and the pass deletes and logs neither, nor their members, owed days or owner rows. Two keys
// that hold the old name in two spellings of case rename to one key, and of each of their rows one
// stays, an owner row with no day row as much as a day row. The pass counts the rows it logs as
// deleted. Red where the pass deletes a row of the old name wherever another row of the old name
// with the same slug and other key columns has an owner key that sorts first, whatever the two
// keys rename to: on a database that held no row of the new name, it deleted the conditional
// key's rows as collisions of the plain key's. Red where the line writes the owner rows after the
// day rows, whose change trigger records each renamed key first: the line deleted and logged the
// owner row of every key it moved. And red where it deletes an owner row of the old name only for
// a row of the new name: the two spellings of the key with no day row renamed to one owner, and
// the line failed.
func TestTheRenamePassKeepsTheRowsOfOwnerKeysThatRenameApart(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renameOwnersSeed)
	logs := capturePassLogs(t)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Failures != 0 || pass.Left != noneLeft {
		t.Fatalf("the pass = %+v, want no failure and nothing left", pass)
	}
	const keys = ":cybermonklabs/legion|, :cybermonklabs/legion|INFRA, :cybermonklabs/legion||INFRA>"
	for query, want := range map[string]string{
		`select string_agg(project_key || ' ' || eval_id || ' ' || series || ' ' || n, ', ' order by project_key collate "C") from eval_days`: ":cybermonklabs/legion| :cybermonklabs/legion|/deploy-frequency deploys 1, " +
			":cybermonklabs/legion|INFRA :cybermonklabs/legion|INFRA/deploy-frequency shared 3, " +
			":cybermonklabs/legion||INFRA> :cybermonklabs/legion||INFRA>/deploy-frequency deploys 2",
		`select string_agg(project_key, ', ' order by project_key collate "C") from eval_day_members`: keys,
		`select string_agg(project_key, ', ' order by project_key collate "C") from eval_days_owed`:   keys,
		`select string_agg(owner || ' ' || repo, ', ' order by owner collate "C") from eval_day_owners`: ":cybermonklabs/legion| cybermonklabs/legion, " +
			":cybermonklabs/legion|INFRA cybermonklabs/legion, :cybermonklabs/legion|SMOKE cybermonklabs/legion, " +
			":cybermonklabs/legion||INFRA> cybermonklabs/legion",
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}
	deletions := named(logs(), deletedLine)
	if pass.Deleted != int64(len(deletions)) {
		t.Errorf("the pass counted %d rows deleted and logged %d: %v", pass.Deleted, len(deletions), deletions)
	}
	deleted := map[string][]string{}
	for _, record := range deletions {
		var key struct {
			ProjectKey string `json:"project_key"`
			Owner      string `json:"owner"`
		}
		if text, _ := record["key"].(string); json.Unmarshal([]byte(text), &key) != nil {
			t.Errorf("a deletion's line = %v, want the deleted row's key", record)
		}
		table, _ := record["table"].(string)
		deleted[table] = append(deleted[table], strings.ToLower(key.ProjectKey+key.Owner))
	}
	for _, found := range deleted {
		slices.Sort(found)
	}
	// Of each key held in two spellings, the row of the spelling that sorts second, in lower case.
	want := map[string][]string{
		"eval_days": {":sjawhar/legion|infra"}, "eval_day_members": {":sjawhar/legion|infra"}, "eval_days_owed": {":sjawhar/legion|infra"},
		"eval_day_owners": {":sjawhar/legion|infra", ":sjawhar/legion|smoke"},
	}
	if !maps.EqualFunc(deleted, want, slices.Equal[[]string]) {
		t.Errorf("logged deletions %v, want %v", deleted, want)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// projectDeliveryRenameSeed writes, under the old name sjawhar/legion, each project's delivery
// settings the pass writes (dispatch://INFRA-978): an excluded repository of TOOLS among others, a
// deploy source of TOOLS that names it, a source of SMOKE that names it beside one that names the
// new name already, and the reconcile progress of its run steps (delivery's repoStep names), one
// of them under a step whose new name a row holds already, beside the search's progress and a run
// step of sjawhar/legion-smoke, which only begins like the old name.
const projectDeliveryRenameSeed = `
	insert into projects (key, name, default_assignee, default_repo) values
		('INFRA', 'Infra', 'alice', 'sjawhar/legion'),
		('SMOKE', 'Smoke', 'alice', 'sjawhar/legion-smoke'),
		('TOOLS', 'Tools', 'alice', 'acme/tools');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('SJawhar/Legion', 'INFRA', '{"kind":"user","id":"alice"}', 'CybermonkLabs/Legion');
	insert into project_delivery_settings (project_key, delivery, population_authors, excluded_repos, updated_by, updated_at) values
		('INFRA', true, '{octocat}', '{sjawhar/dotfiles}', '{"kind":"user","id":"alice"}', '2026-01-01T00:00:00Z'),
		('SMOKE', true, '{octocat}', '{}', '{"kind":"user","id":"alice"}', '2026-01-01T00:00:00Z'),
		('TOOLS', true, '{hubot}', '{acme/sandbox,SJawhar/Legion,sjawhar/legion-smoke}', '{"kind":"user","id":"alice"}', '2026-01-01T00:00:00Z');
	insert into project_deploy_sources (project_key, position, kind, repo, deploy_workflow_path, production_job_names, pr_checks_workflow_path) values
		('INFRA', 0, 'github_workflow', null, 'deploy.yml', '{release}', 'pr.yml'),
		('TOOLS', 0, 'github_workflow', 'acme/tools', 'deploy.yml', '{release}', 'pr.yml'),
		('TOOLS', 1, 'github_workflow', 'sjawhar/legion', 'ship.yml', '{ship}', 'ci.yml'),
		('SMOKE', 0, 'github_workflow', 'sjawhar/legion', 'deploy.yml', '{release}', 'pr.yml'),
		('SMOKE', 1, 'github_workflow', 'cybermonklabs/legion', 'deploy.yml', '{release}', 'pr.yml'),
		('SMOKE', 2, 'github_workflow', null, 'deploy.yml', '{release}', 'pr.yml');
	insert into delivery_reconcile_progress (step, scope, through, updated_at) values
		('runs/deploy/sjawhar/legion', 'sjawhar/legion deploy.yml', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
		('backfill/runs/pr_checks/sjawhar/legion', 'sjawhar/legion pr.yml', '2026-01-02T00:00:00Z', '2026-01-01T00:00:00Z'),
		('backfill/created_at/deploy/sjawhar/legion', 'sjawhar/legion deploy.yml', '2026-01-03T00:00:00Z', '2026-01-01T00:00:00Z'),
		('backfill/created_at/deploy/cybermonklabs/legion', 'cybermonklabs/legion deploy.yml', '2026-01-04T00:00:00Z', '2026-01-01T00:00:00Z'),
		('runs/deploy/sjawhar/legion-smoke', 'sjawhar/legion-smoke deploy.yml', '2026-01-05T00:00:00Z', '2026-01-01T00:00:00Z'),
		('merged_pull_requests/installation/1', 'authors=octocat,hubot excluded=sjawhar/dotfiles,acme/sandbox,SJawhar/Legion,sjawhar/legion-smoke deploy=sjawhar/legion,acme/tools,sjawhar/legion-smoke',
			'2026-01-06T00:00:00Z', '2026-01-01T00:00:00Z');
`

// The pass writes the new name into each project's delivery settings and the reconcile progress
// of the repository's run steps (dispatch://INFRA-978, D2): an excluded repository in its place
// and every other one as it was, a source that names the old name, and the name and the scope of
// each run step of the old name with its progress kept, so the reconcile resumes the renamed
// repository's steps and imports nothing again. A source whose project names the new name already
// is deleted, and so is a run step whose new name a row holds already: the rows under the new name
// stay. A step that only begins like the old name, and the steps' times, are left as they were. A
// second pass writes nothing.
func TestARenameMovesEachProjectsExcludedRepositories(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, projectDeliveryRenameSeed)
	logs := capturePassLogs(t)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Deleted != 2 || pass.Failures != 0 || pass.Left != noneLeft || pass.Err() != nil {
		t.Fatalf("the pass = %+v (%v), want 2 rows deleted, no failure and nothing left", pass, pass.Err())
	}
	for query, want := range map[string]string{
		`select string_agg(project_key || '=' || excluded_repos::text, ' ' order by project_key) from project_delivery_settings`:                                "INFRA={sjawhar/dotfiles} SMOKE={} TOOLS={acme/sandbox,cybermonklabs/legion,sjawhar/legion-smoke}",
		`select string_agg(project_key || '#' || position || '=' || coalesce(repo, 'default'), ' ' order by project_key, position) from project_deploy_sources`: "INFRA#0=default SMOKE#1=cybermonklabs/legion SMOKE#2=default TOOLS#0=acme/tools TOOLS#1=cybermonklabs/legion",
		`select string_agg(key || '=' || default_repo, ' ' order by key) from projects`:                                                                         "INFRA=cybermonklabs/legion SMOKE=sjawhar/legion-smoke TOOLS=acme/tools",
		`select string_agg(step || '=' || scope || '@' || to_char(through at time zone 'UTC', 'MM-DD'), ' | ' order by step) from delivery_reconcile_progress`: "" +
			"backfill/created_at/deploy/cybermonklabs/legion=cybermonklabs/legion deploy.yml@01-04 | " +
			"backfill/runs/pr_checks/cybermonklabs/legion=cybermonklabs/legion pr.yml@01-02 | " +
			"merged_pull_requests/installation/1=authors=octocat,hubot excluded=sjawhar/dotfiles,acme/sandbox,cybermonklabs/legion,sjawhar/legion-smoke deploy=cybermonklabs/legion,acme/tools,sjawhar/legion-smoke@01-06 | " +
			"runs/deploy/cybermonklabs/legion=cybermonklabs/legion deploy.yml@01-01 | " +
			"runs/deploy/sjawhar/legion-smoke=sjawhar/legion-smoke deploy.yml@01-05",
		`select string_agg(to_char(updated_at at time zone 'UTC', 'YYYY-MM-DD'), ' ' order by project_key) from project_delivery_settings`: "2026-01-01 2026-01-01 2026-01-01",
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}
	written, deleted := map[string]float64{}, map[string]int{}
	records := logs()
	for _, record := range named(records, rewroteLine) {
		written[record["field"].(string)] += record["rows"].(float64)
	}
	for _, record := range named(records, deletedLine) {
		deleted[record["table"].(string)]++
	}
	for field, want := range map[string]float64{
		"project_delivery_settings.excluded_repos": 1, "project_deploy_sources.repo": 1,
		"delivery_reconcile_progress.step": 2, "delivery_reconcile_progress.scope": 3,
	} {
		if written[field] != want {
			t.Errorf("logged %v rows of %s, want %v (every line: %v)", written[field], field, want, written)
		}
	}
	if want := map[string]int{"project_deploy_sources": 1, "delivery_reconcile_progress": 1}; !maps.Equal(deleted, want) {
		t.Errorf("logged deletions %v, want %v", deleted, want)
	}
	if again := RewriteRenamedRepositories(ctx, store.Pool, pairs); again != (RepoRenamePass{Pairs: 1, Left: noneLeft}) {
		t.Errorf("a second pass = %+v, want one pair and nothing written, deleted, failed or left", again)
	}
}

// productionEventsRenameSeed writes, under the old name sjawhar/legion, the fields of the record
// of production events and of the merged reverts (dispatch://INFRA-978, R6 and R7): a revert of
// the old name, one whose twin the new name holds, and one of sjawhar/legion-smoke that reverts a
// pull request of the old name; a GitHub deploy row whose pull requests name the old name in mixed
// case beside one of sjawhar/legion-smoke, and whose run's page names the old name in mixed case, a
// posted deploy that names none, its page sjawhar/legion-smoke's, and two revert rows of the old
// name, each with the revert's page, one of them with a twin under the new name; the rule the old
// name's deploy rows stand derived under; and the progress of its reverts' search.
const productionEventsRenameSeed = `
	insert into projects (key, name, default_assignee, default_repo) values
		('INFRA', 'Infra', 'alice', 'sjawhar/legion');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('SJawhar/Legion', 'INFRA', '{"kind":"user","id":"alice"}', 'CybermonkLabs/Legion');
	insert into delivery_reverts (repo, number, reverts_repo, reverts_number, merged_at) values
		('sjawhar/legion', 9, 'sjawhar/legion', 5, '2026-01-02T00:00:00Z'),
		('cybermonklabs/legion', 9, 'cybermonklabs/legion', 5, '2026-01-02T00:00:00Z'),
		('sjawhar/legion', 10, 'sjawhar/legion-smoke', 3, '2026-01-03T00:00:00Z'),
		('sjawhar/legion-smoke', 4, 'SJawhar/Legion', 2, '2026-01-04T00:00:00Z');
	insert into production_events (id, project_key, kind, source, source_ref, finished_at, repo, pull_requests, url, actor) values
		('00000000-0000-7000-8000-000000000001', 'INFRA', 'deploy', 'github', 'github:100', '2026-01-01T01:00:00Z',
			'sjawhar/legion', '{sjawhar/legion#5,SJawhar/Legion#6,sjawhar/legion-smoke#7}',
			'https://github.com/SJawhar/Legion/actions/runs/100', '{"kind":"system","id":"production-signals"}'),
		('00000000-0000-7000-8000-000000000002', 'INFRA', 'deploy', 'api', 'posted-1', '2026-01-01T02:00:00Z',
			'', '{sjawhar/legion-smoke#8}', 'https://github.com/sjawhar/legion-smoke/pull/8', '{"kind":"session","id":"poster"}');
	insert into production_events (id, project_key, kind, source, source_ref, repo, pull_requests, url, actor, deploy_id, signal, detected_at) values
		('00000000-0000-7000-8000-000000000003', 'INFRA', 'failure', 'reverts', 'revert:sjawhar/legion#10', 'sjawhar/legion',
			'{sjawhar/legion-smoke#3}', 'https://github.com/sjawhar/legion/pull/10', '{"kind":"system","id":"production-signals"}',
			'00000000-0000-7000-8000-000000000001', 'revert', '2026-01-03T00:00:00Z'),
		('00000000-0000-7000-8000-000000000004', 'INFRA', 'failure', 'reverts', 'revert:sjawhar/legion#9', 'sjawhar/legion',
			'{sjawhar/legion#5}', 'https://github.com/sjawhar/legion/pull/9', '{"kind":"system","id":"production-signals"}',
			'00000000-0000-7000-8000-000000000001', 'revert', '2026-01-02T00:00:00Z'),
		('00000000-0000-7000-8000-000000000005', 'INFRA', 'failure', 'reverts', 'revert:cybermonklabs/legion#9', 'cybermonklabs/legion',
			'{cybermonklabs/legion#5}', 'https://github.com/cybermonklabs/legion/pull/9', '{"kind":"system","id":"production-signals"}',
			'00000000-0000-7000-8000-000000000001', 'revert', '2026-01-02T00:00:00Z');
	insert into production_deploy_rules (repo, project_key, production_job_names) values
		('sjawhar/legion', 'INFRA', '{release}');
	insert into delivery_reconcile_progress (step, scope, through, updated_at) values
		('reverts/sjawhar/legion', 'sjawhar/legion', '2026-01-05T00:00:00Z', '2026-01-01T00:00:00Z');
`

// The pass writes the new name into the merged reverts, the record of production events and the
// rule its deploy rows stand derived under (dispatch://INFRA-978): each revert's repository and the
// repository of the pull request it reverts, each row's repository and the page its url names,
// each pull request a row names in its place with its number, and each revert row's source_ref, so
// the revert's signal reads its row back under the new name and writes no second one. A revert and
// a revert row whose twin the new name holds are deleted, and the twins stay. The reverts' search
// step keeps its progress under the new name. Names that only begin like the old name are left. A
// second pass writes nothing.
func TestARenameMovesTheRecordOfProductionEventsAndTheReverts(t *testing.T) {
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, productionEventsRenameSeed)
	logs := capturePassLogs(t)

	pass := RewriteRenamedRepositories(ctx, store.Pool, pairs)

	if pass.Deleted != 2 || pass.Failures != 0 || pass.Left != noneLeft || pass.Err() != nil {
		t.Fatalf("the pass = %+v (%v), want 2 rows deleted, no failure and nothing left", pass, pass.Err())
	}
	for query, want := range map[string]string{
		`select string_agg(repo || '#' || number || '>' || reverts_repo || '#' || reverts_number, ' ' order by repo collate "C", number) from delivery_reverts`: "" +
			"cybermonklabs/legion#9>cybermonklabs/legion#5 cybermonklabs/legion#10>sjawhar/legion-smoke#3 sjawhar/legion-smoke#4>cybermonklabs/legion#2",
		`select string_agg(source_ref || '=' || repo || ' ' || pull_requests::text || ' ' || url, ' | ' order by id) from production_events`: "" +
			"github:100=cybermonklabs/legion {cybermonklabs/legion#5,cybermonklabs/legion#6,sjawhar/legion-smoke#7} https://github.com/cybermonklabs/legion/actions/runs/100 | " +
			"posted-1= {sjawhar/legion-smoke#8} https://github.com/sjawhar/legion-smoke/pull/8 | " +
			"revert:cybermonklabs/legion#10=cybermonklabs/legion {sjawhar/legion-smoke#3} https://github.com/cybermonklabs/legion/pull/10 | " +
			"revert:cybermonklabs/legion#9=cybermonklabs/legion {cybermonklabs/legion#5} https://github.com/cybermonklabs/legion/pull/9",
		`select string_agg(repo || '=' || project_key, ' ') from production_deploy_rules`:                                                        "cybermonklabs/legion=INFRA",
		`select string_agg(step || '=' || scope || '@' || to_char(through at time zone 'UTC', 'MM-DD'), ' | ') from delivery_reconcile_progress`: "reverts/cybermonklabs/legion=cybermonklabs/legion@01-05",
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
		}
	}
	written, deleted := map[string]float64{}, map[string]int{}
	records := logs()
	for _, record := range named(records, rewroteLine) {
		written[record["field"].(string)] += record["rows"].(float64)
	}
	for _, record := range named(records, deletedLine) {
		deleted[record["table"].(string)]++
	}
	for field, want := range map[string]float64{
		"delivery_reverts.repo": 1, "delivery_reverts.reverts_repo": 1,
		"production_events.source_ref": 1, "production_events.repo": 2, "production_events.url": 2, "production_events.pull_requests": 1,
		"production_deploy_rules.repo": 1, "delivery_reconcile_progress.step": 1, "delivery_reconcile_progress.scope": 1,
	} {
		if written[field] != want {
			t.Errorf("logged %v rows of %s, want %v (every line: %v)", written[field], field, want, written)
		}
	}
	if want := map[string]int{"delivery_reverts": 1, "production_events": 1}; !maps.Equal(deleted, want) {
		t.Errorf("logged deletions %v, want %v", deleted, want)
	}
	if again := RewriteRenamedRepositories(ctx, store.Pool, pairs); again != (RepoRenamePass{Pairs: 1, Left: noneLeft}) {
		t.Errorf("a second pass = %+v, want one pair and nothing written, deleted, failed or left", again)
	}
}
