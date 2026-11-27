package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
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
	insert into commit_holds (repo, commit_sha, merge_sha, holds, checked_at) values
		('sjawhar/legion', 'c2', 'm1', true, '2026-01-01T00:00:00Z'),
		('sjawhar/dotfiles', 'c2', 'm1', true, '2026-01-01T00:00:00Z');
	insert into deploys (repo, program, commit_sha, deployed_at, source, recorded_by) values
		('sjawhar/legion', 'dispatch', repeat('d', 40), '2026-01-01T00:00:00Z', 'route', '{"kind":"user","id":"alice"}'),
		('sjawhar/legion-smoke', 'dispatch', repeat('e', 40), '2026-01-01T00:00:00Z', 'route', '{"kind":"user","id":"alice"}');
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
	`select string_agg(repo, ' ' order by repo) from commit_holds`:   "cybermonklabs/legion sjawhar/dotfiles",
	`select string_agg(repo || ' ' || commit_sha, ' ' order by repo) from deploys`: "cybermonklabs/legion " + strings.Repeat("d", 40) +
		" sjawhar/legion-smoke " + strings.Repeat("e", 40),
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
		"pull_request_heads.repo": 1, "pull_request_heads.url": 1, "branch_commits.repo": 1, "commit_holds.repo": 1, "deploys.repo": 1,
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
	insert into commit_holds (repo, commit_sha, merge_sha, holds, checked_at) values
		('sjawhar/legion', 'c1', 'm1', false, now()),
		('cybermonklabs/legion', 'c1', 'm1', true, now());
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

	if pass.Deleted != 10 || pass.Failures != 0 || pass.Left != noneLeft {
		t.Fatalf("the pass = %+v, want 10 rows deleted, no failure and nothing left", pass)
	}
	for query, want := range map[string]string{
		`select string_agg(issue_key || ' ' || url, ' | ' order by url) from issue_external_links where url like '%/issues/5'`: "INFRA-2 https://github.com/cybermonklabs/legion/issues/5",
		`select string_agg(url, ' | ') from issue_external_links where url ilike '%/pull/9'`:                                   "https://github.com/cybermonklabs/legion/pull/9",
		`select string_agg(repo || ' ' || head_sha, ' | ' order by number) from pull_request_heads where number = 7`:           "cybermonklabs/legion new",
		`select string_agg(repo, ' | ') from pull_request_heads where number = 9`:                                              "cybermonklabs/legion",
		`select string_agg(repo || ' ' || on_branch, ' | ') from branch_commits`:                                               "cybermonklabs/legion true",
		`select string_agg(repo || ' ' || holds, ' | ') from commit_holds`:                                                     "cybermonklabs/legion true",
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
		"issue_external_links": 2, "pull_request_heads": 2, "branch_commits": 1, "commit_holds": 1, "delivery_pull_requests": 1,
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

// evalLineHoldKey is the advisory lock a held statement of the pass waits for (holdTheEvalLine).
const evalLineHoldKey = int64(0x524E414D0001)

// holdTheEvalLine puts a trigger of the test's database alone before each statement that runs op
// on table. The trigger lifts the lock timeout of the statement's transaction for the rest of it
// (repoRenameLockTimeout), so a test that holds a lock the pass's line of the eval tables waits for
// is no race against its 5 s. Given hold, it then makes the statement wait, before it reads a row,
// for an advisory lock the answered transaction holds, which the test commits to let the line on.
// Given no hold it answers nil.
func holdTheEvalLine(t *testing.T, store *Store, op, table string, hold bool) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`create function rename_pass_test_hold() returns trigger language plpgsql as $$
		begin
			perform set_config('lock_timeout', '0', true);
			if tg_argv[0]::boolean then
				perform pg_advisory_xact_lock(tg_argv[1]::bigint);
			end if;
			return null;
		end $$`,
		fmt.Sprintf(`create trigger rename_pass_test_hold before %s on %s for each statement
			execute function rename_pass_test_hold('%t', '%d')`, op, table, hold, evalLineHoldKey),
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the pass at its %s of %s: %v", op, table, err)
		}
	}
	if !hold {
		return nil
	}
	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of the pass: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, evalLineHoldKey); err != nil {
		t.Fatalf("take the hold's lock: %v", err)
	}
	return holder
}

// awaitTheHeldLine waits for a statement of the pass to wait at its hold (holdTheEvalLine), read
// through holder's own connection, and answers the pid of the pass's backend. It fails the test
// where the pass answers on passed first.
func awaitTheHeldLine(t *testing.T, holder pgx.Tx, passed <-chan RepoRenamePass) uint32 {
	t.Helper()
	return testwait.Until(t, "the pass to wait at its hold", func() (uint32, bool) {
		select {
		case pass := <-passed:
			t.Fatalf("the pass answered %+v before it reached its hold", pass)
		default:
		}
		var pid uint32
		if err := holder.QueryRow(context.Background(), `
			select coalesce((select pid from pg_locks where locktype = 'advisory' and not granted and objsubid = 1
				and classid = (($1::bigint >> 32) & 4294967295)::oid and objid = ($1::bigint & 4294967295)::oid limit 1), 0)
		`, evalLineHoldKey).Scan(&pid); err != nil {
			t.Fatalf("read the waits at the hold: %v", err)
		}
		return pid, pid != 0
	})
}

// requireNoDeadlock fails the test where err is not nil, saying where it is Postgres's deadlock.
func requireNoDeadlock(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "40P01":
		t.Errorf("%s deadlocked (SQLSTATE 40P01): %v", what, err)
	case err != nil:
		t.Errorf("%s: %v", what, err)
	}
}

// The line of the eval tables writes owner rows before day rows (its comment says why), where the
// eval_days change trigger, the one other writer of both, writes a key's owner row after its day
// rows, in the statement that writes them. So the line takes eval_days' own lock first, before any
// owner row: the place the eval tables' lock order gives eval_days (evals/defaults.go). Over
// renameOwnersSeed, the line is held at its first statement on the day rows, its owner rows renamed
// by then, and a refresher of an earlier build, as a roll runs one, rewrites the day row of the
// plain key of the old name, whose trigger writes that key's owner row: the write queues on
// eval_days' lock behind the line before it locks any row, holding no transaction id, and, once the
// line is let on and has committed, finds no row of the old name left to write. The pass fails
// nowhere and leaves nothing on the old name. Red where the line takes no lock of eval_days first:
// the write then locks the day row beside the held line and waits for the owner row the line
// renamed, and the line, let on, waits for that day row, a cycle Postgres breaks by failing one of
// the two with `deadlock detected` (40P01).
func TestTheRenamePassLocksTheDayRowsBeforeTheOwnerRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renameOwnersSeed)
	holder := holdTheEvalLine(t, store, "delete", "eval_days", true)
	passed := make(chan RepoRenamePass, 1)
	go func() { passed <- RewriteRenamedRepositories(ctx, store.Pool, pairs) }()
	line := awaitTheHeldLine(t, holder, passed)

	conn, err := store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the refresher's connection: %v", err)
	}
	writer := conn.Conn().PgConn().PID()
	written := make(chan error, 1)
	go func() {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `update eval_days set n = n + 1 where project_key = ':sjawhar/legion|'`)
			return err
		})
		conn.Release()
		written <- err
	}()
	// queued is what the refresher's write waits for behind the line, by pg_locks, and whether it
	// holds a transaction id, a row it wrote, while it waits.
	type queued struct {
		lock string
		xid  bool
	}
	wait := testwait.Until(t, "the refresher's write of a day row to queue behind the held line", func() (queued, bool) {
		select {
		case err := <-written:
			t.Fatalf("the refresher's write answered (%v) beside the held line, want it to queue behind the line", err)
		default:
		}
		var q queued
		if err := holder.QueryRow(ctx, `
			select coalesce((select l.locktype || ' ' || coalesce(l.relation::regclass::text, '-') || ' ' || l.mode
					from pg_locks l where l.pid = $1 and not l.granted and $2 = any(pg_blocking_pids($1)) limit 1), ''),
				exists (select 1 from pg_locks where pid = $1 and locktype = 'transactionid' and granted)
		`, writer, line).Scan(&q.lock, &q.xid); err != nil {
			t.Fatalf("read the waits of the refresher's write: %v", err)
		}
		return q, q.lock != ""
	})
	if wait.lock != "relation eval_days RowExclusiveLock" || wait.xid {
		t.Errorf("the refresher's write waits behind the line for %q, holding a transaction id %t; want it to wait for eval_days' lock, holding none",
			wait.lock, wait.xid)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the line on: %v", err)
	}
	pass := testwait.Receive(t, "the rename pass", passed)
	requireNoDeadlock(t, "the refresher's write", testwait.Receive(t, "the refresher's write", written))
	if pass.Failures != 0 || pass.Left != noneLeft {
		t.Errorf("the pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
}

// renamedDefaultsSeed holds the rename pair sjawhar/legion to cybermonklabs/legion and two projects
// that publish the old name as their default, BRAVO's published row written before ALPHA's, so a
// read of the published defaults in the table's own order meets BRAVO's first.
const renamedDefaultsSeed = `
	insert into projects (key, name, default_assignee, default_repo) values
		('ALPHA', 'Alpha', 'alice', 'sjawhar/legion'),
		('BRAVO', 'Bravo', 'alice', 'sjawhar/legion');
	insert into repo_projects (repo, project, created_by, renamed_to) values
		('sjawhar/legion', 'ALPHA', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
	insert into eval_days_defaults (project_key, repo) values ('BRAVO', 'sjawhar/legion'), ('ALPHA', 'sjawhar/legion');
`

// The line of the eval tables locks the published defaults it moves in the byte order of their
// keys, the order the eval tables' lock order gives them (evals/defaults.go), which a refresher's
// commit of a move of the defaults keeps too (evals.publishDefaults). Over renamedDefaultsSeed, a
// transaction holds BRAVO's published default, as such a commit that reached it would, and the
// pass runs: its line waits for BRAVO's row holding ALPHA's, and, once let on, marks both projects'
// cuts of the old and the new name and publishes the new name for both. Red where the line locks
// the rows in the table's own order, which meets BRAVO's first: it then waits for BRAVO's holding no
// lock of ALPHA's, the other way round from the commit, and the two deadlock where the commit holds
// ALPHA's and waits for BRAVO's while the line holds BRAVO's and waits for ALPHA's.
func TestTheRenamePassLocksThePublishedDefaultsInTheOrderOfTheirKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, pairs := openRenamedRepositoriesStore(t, renamedDefaultsSeed)
	holdTheEvalLine(t, store, "delete", "eval_day_owners", false)
	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of BRAVO's published default: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select from eval_days_defaults where project_key = 'BRAVO' for update`); err != nil {
		t.Fatalf("hold BRAVO's published default: %v", err)
	}
	passed := make(chan RepoRenamePass, 1)
	go func() { passed <- RewriteRenamedRepositories(ctx, store.Pool, pairs) }()
	testwait.Until(t, "the pass to wait for BRAVO's published default", func() (bool, bool) {
		select {
		case pass := <-passed:
			t.Fatalf("the pass answered %+v while BRAVO's published default was held, want it to wait for it", pass)
		default:
		}
		var waiting bool
		if err := holder.QueryRow(ctx, `select exists (select 1 from pg_locks where not granted and pg_backend_pid() = any(pg_blocking_pids(pid)))`).Scan(&waiting); err != nil {
			t.Fatalf("read the waits behind the hold: %v", err)
		}
		return waiting, waiting
	})
	probe, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe of ALPHA's published default: %v", err)
	}
	_, err = probe.Exec(ctx, `select from eval_days_defaults where project_key = 'ALPHA' for update nowait`)
	_ = probe.Rollback(ctx)
	if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Errorf("while the line waited for BRAVO's published default, a lock of ALPHA's answered %v; want SQLSTATE 55P03, the line holding it", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the line on: %v", err)
	}
	if pass := testwait.Receive(t, "the rename pass", passed); pass.Failures != 0 || pass.Left != noneLeft {
		t.Errorf("the pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
	for query, want := range map[string]string{
		`select string_agg(project_key || '=' || repo, ' ' order by project_key) from eval_days_defaults`: "ALPHA=cybermonklabs/legion BRAVO=cybermonklabs/legion",
		`select string_agg(project_key || ' ' || repo, ' | ' order by project_key, repo collate "C") from eval_figure_cuts_stale`: "ALPHA cybermonklabs/legion | ALPHA sjawhar/legion | " +
			"BRAVO cybermonklabs/legion | BRAVO sjawhar/legion",
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("%s\n= %s\nwant %s", query, got, want)
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
