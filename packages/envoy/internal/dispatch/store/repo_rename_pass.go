package store

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The rename pass writes the new name of each rename pair (RepoRename) in place of its old name,
// in every stored repository field of Dispatch. The boot runs it once before it serves
// (cmd/dispatch/main.go). From this release on, each write of these fields stores the new name
// (refs.Renames.Canonical and CanonicalURL), so a later pass finds nothing to write. The exception
// is a row that a build before this one writes during a roll: the next boot's pass writes it.
//
// The fields are the lines of repoRenameLines, read from the schema, and the pass writes them in
// that order. Each line is one transaction, so a line that fails leaves the lines before it
// written and does not stop the lines after it. A field matches by one of three rules:
//
//   - a name matches when its lower case is the pair's old name, and the pass writes the new
//     name;
//   - a URL matches by RepoURLRule, and the pass writes the new owner/name in place of the old
//     one and keeps every other character;
//   - a token of delivery_reconcile_progress.scope matches the old name, in any case, after the
//     start, a space, `=` or `,` and before the end, a space or `,`. That is how delivery's
//     searchProgressScope and runsProgressScope write a repository into a scope.
//
// The pass writes no other column, updated_at included, and appends no event. It never writes
// repo_projects, the event log, the text of documents, comments, asks and messages, the reference
// tables read from that text (refs, block_refs, graph_edges), architecture_snapshots.files, a
// daemon's own readings (legion_current, legion_observations) or github_rate_limits, whose scope
// names no repository.
//
// A line whose table has a key with the repository in it can meet a collision: the new name gives
// a row the key of another row. Two keys are equal when their repository is equal in lower case.
// The row under the new name stays, and the pass deletes the row under the old name and logs its
// key. Where two rows under the old name meet, the row whose repository sorts first stays and is
// then written.

// repoRenameLockNamespace is the first key of the transaction advisory lock each line of the pass
// takes ("RNAM"), with 0 as its second key. Two processes that boot at once take turns on each
// line, and the second finds nothing left to write. It is the two-key form, which Postgres keeps
// apart from the single-key locks. The other first keys of the two-key space are the copy locks'
// (docs.copiedAsksLockNamespace), the loop leases' (duty.leaseNamespace), the test hooks' (the api
// package's testHookLockNamespace), a conditional edit's admission's
// (docs.conditionalEditQueueLockNamespace), the issue dependencies' (api's
// lockProjectIssueDependencies) and the deployment's own issue dependencies' (api's
// lockDeploymentIssueDependencies), none of them this one.
const repoRenameLockNamespace int32 = 0x524E414D

// repoRenameLockTimeout is how long a statement of the pass waits for a lock: the advisory lock
// of a line that another process is writing, or a row that a request holds. A line that waits
// longer fails and is logged, and the next boot writes it.
const repoRenameLockTimeout = `set local lock_timeout = '5s'`

// repoRenamesLeftTimeout bounds the count of the rows left on an old name. The count has a bound
// of its own, which the end of the pass's bound does not end, so a pass whose lines ran out of
// time still says how much they left. The count is one statement over the pass's tables.
const repoRenamesLeftTimeout = 10 * time.Second

// RepoRenamePass is what one run of RewriteRenamedRepositories did.
type RepoRenamePass struct {
	// Pairs is the number of rename pairs the pass was given.
	Pairs int
	// Rows is the number of field values the pass wrote: the sum of the rows of each field's
	// INFO line. Deleted is the number of rows it deleted because the new name already had a
	// row with their key.
	Rows, Deleted int64
	// Failures is the number of lines that failed, plus one when the count of the rows left
	// failed. The boot adds one more when its read of the pairs failed.
	Failures int
	// Left is the number of rows that name an old name in a field of the pass after its writes,
	// or unknown where nothing counted them.
	Left RepoRenamesLeft
}

// RepoRenamesLeft is a count of the rows left on an old name, known only where the count ran and
// answered. Its zero value is unknown, so a count that did not run can never read as 0.
type RepoRenamesLeft struct {
	Rows    int64
	Counted bool
}

// LogValue is the count, or `unknown` where it is not known.
func (l RepoRenamesLeft) LogValue() slog.Value {
	if !l.Counted {
		return slog.StringValue("unknown")
	}
	return slog.Int64Value(l.Rows)
}

// none is true where the count ran and found no row.
func (l RepoRenamesLeft) none() bool {
	return l.Counted && l.Rows == 0
}

// LogArgs are the counts as slog key-value pairs. left is `unknown` where it is not known.
func (p RepoRenamePass) LogArgs() []any {
	return []any{"pairs", p.Pairs, "rows", p.Rows, "deleted", p.Deleted, "failures", p.Failures, "left", p.Left}
}

// Err is nil when the pass failed nowhere and is known to have left no old name, else an error
// that says how much is left for the next boot.
func (p RepoRenamePass) Err() error {
	if p.Failures == 0 && p.Left.none() {
		return nil
	}
	return fmt.Errorf("the pass failed %d times and left %v rows on an old name; the next boot tries again",
		p.Failures, p.Left.LogValue())
}

// RewriteRenamedRepositories writes the new name of each pair in place of its old name in every
// field of repoRenameLines, one transaction for each line, and then counts the rows that still
// name an old name. It logs each field it wrote at INFO, each row it deleted at WARN, and each
// line or count that failed at ERROR, and goes on after a failure. A failure caused by ctx, such as
// the end of the boot's bound, fails the line in flight and each line after it; the count runs
// all the same, under repoRenamesLeftTimeout. With no pairs it reads and writes nothing, and no
// row can name an old name.
func RewriteRenamedRepositories(ctx context.Context, pool *Pool, pairs []RepoRename) RepoRenamePass {
	pass := RepoRenamePass{Pairs: len(pairs)}
	if len(pairs) == 0 {
		pass.Left = RepoRenamesLeft{Counted: true}
		return pass
	}
	renames := make([]repoRename, 0, len(pairs))
	for _, pair := range pairs {
		renames = append(renames, repoRename{RepoRename: pair, url: NewRepoURLRule(pair.From, pair.To)})
	}
	for _, line := range repoRenameLines {
		rows, deleted, err := rewriteRepoRenameLine(ctx, pool, line, renames)
		if err != nil {
			pass.Failures++
			slog.Error("dispatch: rewrite a renamed repository's old name", "field", line.fields, "error", err)
			continue
		}
		pass.Rows += rows
		pass.Deleted += deleted
	}
	countCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repoRenamesLeftTimeout)
	defer cancel()
	left, err := countRepoRenamesLeft(countCtx, pool, renames)
	if err != nil {
		pass.Failures++
		slog.Error("dispatch: count the rows left on a renamed repository's old name", "error", err)
		return pass
	}
	pass.Left = RepoRenamesLeft{Rows: left, Counted: true}
	return pass
}

// rewriteRepoRenameLine runs line's statements for each pair in one transaction and commits it,
// and answers the rows it wrote and the rows it deleted. It logs its writes and its deletions only
// once they are committed, so a line that fails logs nothing but its error.
func rewriteRepoRenameLine(ctx context.Context, pool *Pool, line repoRenameLine, renames []repoRename) (int64, int64, error) {
	// A line is a transaction like any other, so it is marked like any other: nothing it runs may
	// take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, repoRenameLockTimeout); err != nil {
		return 0, 0, fmt.Errorf("set the lock timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1, 0)`, repoRenameLockNamespace); err != nil {
		return 0, 0, fmt.Errorf("take the pass's lock: %w", err)
	}
	type written struct {
		field string
		rows  int64
		repoRename
	}
	type deletion struct {
		table, key string
		repoRename
	}
	var writes []written
	var deletions []deletion
	var rows int64
	for _, r := range renames {
		for _, step := range line.steps(r) {
			if step.deletes != "" {
				keys, err := deletedKeys(ctx, tx, step.sql)
				if err != nil {
					return 0, 0, fmt.Errorf("delete the rows of %s whose key the new name %s holds: %w", step.deletes, r.To, err)
				}
				for _, key := range keys {
					deletions = append(deletions, deletion{step.deletes, key, r})
				}
				continue
			}
			tag, err := tx.Exec(ctx, step.sql)
			if err != nil {
				return 0, 0, fmt.Errorf("write %s in place of %s: %w", r.To, r.From, err)
			}
			if step.write != "" && tag.RowsAffected() > 0 {
				writes = append(writes, written{step.write, tag.RowsAffected(), r})
				rows += tag.RowsAffected()
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	for _, d := range deletions {
		slog.Warn("dispatch: deleted a row whose key the renamed repository's new name already holds",
			"table", d.table, "key", d.key, "from", d.From, "to", d.To)
	}
	for _, w := range writes {
		slog.Info("dispatch: rewrote a renamed repository's old name", "field", w.field, "from", w.From, "to", w.To, "rows", w.rows)
	}
	return rows, int64(len(deletions)), nil
}

// deletedKeys runs a delete that returns the key of each row it deleted, as text, and answers
// those keys.
func deletedKeys(ctx context.Context, tx pgx.Tx, sql string) ([]string, error) {
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// countRepoRenamesLeft counts the rows that name an old name in a field of a line, in one
// statement over every line and pair.
func countRepoRenamesLeft(ctx context.Context, pool *Pool, renames []repoRename) (int64, error) {
	var terms []string
	for _, line := range repoRenameLines {
		for _, r := range renames {
			for _, rows := range line.left(r) {
				terms = append(terms, `(select count(*) from `+rows.table+` where `+rows.where+`)`)
			}
		}
	}
	var left int64
	if err := pool.QueryRow(ctx, `select `+strings.Join(terms, ` + `)).Scan(&left); err != nil {
		return 0, err
	}
	return left, nil
}

// repoRename is one pair, as the statements of the pass are built from it. The names go into the
// statements as literals (QuoteLiteral), as RewriteSQL puts the URL rule there, so a statement
// takes no parameters.
type repoRename struct {
	RepoRename
	url RepoURLRule
}

// from and to are the pair's old and new name as SQL literals.
func (r repoRename) from() string { return QuoteLiteral(r.From) }
func (r repoRename) to() string   { return QuoteLiteral(r.To) }

// named is true where column, a repository name, is the old name in any case.
func (r repoRename) named(column string) string {
	return `lower(` + column + `) = ` + r.from()
}

// urlNamed is true where column, a URL, names the old name by the URL rule. The rule changes such
// a URL, as the new name differs from the old one, and no other.
func (r repoRename) urlNamed(column string) string {
	return r.url.RewriteSQL(column) + ` <> ` + column
}

// token is the pattern of the old name as a token of a reconcile progress scope, as a SQL literal.
// Its lookahead leaves the separator after the name for the next match, so `,old,old` matches
// twice.
func (r repoRename) token() string {
	return QuoteLiteral(`(^|[ =,])` + regexp.QuoteMeta(r.From) + `(?=$|[ ,])`)
}

// tokenReplacement puts the new name in place of a match of token, after the separator the match
// began with.
func (r repoRename) tokenReplacement() string {
	return QuoteLiteral(`\1` + strings.ReplaceAll(r.To, `\`, `\\`))
}

// repoRenameLine is one line of the pass: the fields it writes in one transaction.
type repoRenameLine struct {
	// fields names the line's fields, as the ERROR line of a line that failed names them.
	fields string
	// steps are the line's statements for one pair, in the order they run.
	steps func(r repoRename) []repoRenameStep
	// left is, for each table of the line, the condition a row of it meets while a field of it
	// still names r's old name.
	left func(r repoRename) []repoRenameRows
}

// repoRenameStep is one statement of a line. write names the field that the statement writes: its
// rows are counted and logged at INFO. deletes names the table that the statement deletes the
// colliding rows of: it returns the key of each row it deleted, as text, and each is counted and
// logged at WARN. A step that names neither is a statement whose rows a write step of the line
// counts already.
type repoRenameStep struct {
	write, deletes string
	sql            string
}

// repoRenameRows is the rows of table that meet where.
type repoRenameRows struct {
	table, where string
}

// nameWrite writes the new name in column of table wherever the column holds the old name.
func nameWrite(r repoRename, table, column string) repoRenameStep {
	return repoRenameStep{
		write: table + "." + column,
		sql:   `update ` + table + ` set ` + column + ` = ` + r.to() + ` where ` + r.named(column),
	}
}

// urlWrite rewrites column of table, a URL, wherever it names the old name.
func urlWrite(r repoRename, table, column string) repoRenameStep {
	return repoRenameStep{
		write: table + "." + column,
		sql:   `update ` + table + ` set ` + column + ` = ` + r.url.RewriteSQL(column) + ` where ` + r.urlNamed(column),
	}
}

// collisions deletes each row of table under the old name whose key, with the new name for its
// repository, is the key of another row: a row under the new name in any case, or a row under the
// old name whose repository sorts first. The key is the table's repo column and keys. The
// subquery reads the table as the statement began, so of two rows under the old name the one that
// sorts first stays for the write after this.
func collisions(r repoRename, table string, keys ...string) repoRenameStep {
	same := ""
	key := `'repo', d.repo`
	for _, column := range keys {
		same += ` and t.` + column + ` = d.` + column
		key += `, '` + column + `', d.` + column
	}
	return repoRenameStep{
		deletes: table,
		sql: `delete from ` + table + ` d
			where ` + r.named("d.repo") + `
			  and exists (
				select 1 from ` + table + ` t
				where (lower(t.repo) = ` + r.to() + ` or (` + r.named("t.repo") + ` and t.repo < d.repo))` + same + `)
			returning jsonb_build_object(` + key + `)::text`,
	}
}

// nameLine is the line of one name column of a table with no key that holds it.
func nameLine(table, column string) repoRenameLine {
	return repoRenameLine{
		fields: table + "." + column,
		steps:  func(r repoRename) []repoRenameStep { return []repoRenameStep{nameWrite(r, table, column)} },
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{table, r.named(column)}}
		},
	}
}

// citedNamed is true where a delivery pull request cites an issue URL that names the old name.
func citedNamed(r repoRename) string {
	return `exists (select 1 from unnest(attribution_cited_issues) e(url) where ` + r.urlNamed("e.url") + `)`
}

// excludedNamed is true where the delivery settings exclude the old name.
func excludedNamed(r repoRename) string {
	return `exists (select 1 from unnest(excluded_repos) e(repo) where ` + r.named("e.repo") + `)`
}

// repoRenameLines are the stored repository fields of Dispatch, in the order the pass writes them,
// one transaction for each line. They are each column of the schema that holds a repository name
// or a URL that names one, but for those the package comment says the pass never writes.
var repoRenameLines = []repoRenameLine{
	nameLine("issues", "repo"),
	nameLine("projects", "default_repo"),
	{
		// A URL links one issue (issue_external_links_url), and readers compare a link in lower
		// case (issue_external_links_url_lower). So an old URL collides with every link whose
		// lower case is its rewrite in lower case. `old` reads the old URLs once, with their
		// rewrites, so the collision costs one scan of the table and a probe of the index for each
		// old URL.
		fields: "issue_external_links.url",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				{deletes: "issue_external_links", sql: `
					with old as materialized (
						select url, lower(` + r.url.RewriteSQL("url") + `) as rewritten
						from issue_external_links where ` + r.urlNamed("url") + `
					), twinned as (
						select o.url from old o
						where exists (select 1 from issue_external_links n where lower(n.url) = o.rewritten)
						   or exists (select 1 from old p where p.rewritten = o.rewritten and p.url < o.url)
					)
					delete from issue_external_links l using twinned t where l.url = t.url
					returning jsonb_build_object('issue_key', l.issue_key, 'url', l.url)::text`},
				urlWrite(r, "issue_external_links", "url"),
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"issue_external_links", r.urlNamed("url")}}
		},
	},
	{
		fields: "pull_request_heads.repo, .url",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "pull_request_heads", "number"),
				nameWrite(r, "pull_request_heads", "repo"),
				urlWrite(r, "pull_request_heads", "url"),
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"pull_request_heads", r.named("repo") + ` or ` + r.urlNamed("url")}}
		},
	},
	{
		fields: "branch_commits.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "branch_commits", "branch", "sha"),
				nameWrite(r, "branch_commits", "repo"),
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"branch_commits", r.named("repo")}}
		},
	},
	{
		// Each cited URL is rewritten in its place. A URL the array then holds twice is kept once,
		// at its first place, as delivery's citedIssues keeps each URL once.
		fields: "delivery_pull_requests.repo, .url, .attribution_cited_issues",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "delivery_pull_requests", "number"),
				nameWrite(r, "delivery_pull_requests", "repo"),
				urlWrite(r, "delivery_pull_requests", "url"),
				{write: "delivery_pull_requests.attribution_cited_issues", sql: `
					update delivery_pull_requests set attribution_cited_issues = array(
						select cited.url from (
							select ` + r.url.RewriteSQL("e.url") + ` as url, min(e.n) as n
							from unnest(attribution_cited_issues) with ordinality e(url, n)
							group by 1
						) cited order by cited.n)
					where ` + citedNamed(r)},
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"delivery_pull_requests",
				r.named("repo") + ` or ` + r.urlNamed("url") + ` or ` + citedNamed(r)}}
		},
	},
	{
		// A run's jobs name it by (repo, run_id), and that foreign key has no ON UPDATE
		// (0081_delivery_runs), so a run cannot be renamed in place. A run with a twin under the
		// new name is deleted, and its jobs with it (ON DELETE CASCADE); the twin keeps its own
		// jobs. Each other run is copied under the new name, its jobs are pointed at the copy, and
		// the old run, which no job names any more, is deleted. The copy goes through the row's
		// jsonb, so it carries every column of the table, a column a later migration adds
		// included.
		fields: "delivery_runs.repo, .url with delivery_run_jobs.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "delivery_runs", "run_id"),
				{write: "delivery_runs.repo", sql: `
					insert into delivery_runs
					select moved.* from delivery_runs stored
					cross join lateral jsonb_populate_record(null::delivery_runs,
						to_jsonb(stored) || jsonb_build_object('repo', ` + r.to() + `::text)) moved
					where ` + r.named("stored.repo")},
				nameWrite(r, "delivery_run_jobs", "repo"),
				{sql: `delete from delivery_runs where ` + r.named("repo")},
				urlWrite(r, "delivery_runs", "url"),
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{
				{"delivery_runs", r.named("repo") + ` or ` + r.urlNamed("url")},
				{"delivery_run_jobs", r.named("repo")},
			}
		},
	},
	{
		fields: "delivery_run_warnings.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "delivery_run_warnings", "run_id", "job_names", "apply_step"),
				nameWrite(r, "delivery_run_warnings", "repo"),
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"delivery_run_warnings", r.named("repo")}}
		},
	},
	nameLine("delivery_changes", "repo"),
	{
		// Each excluded repository is rewritten in its place, and none is dropped, so the scope
		// searchProgressScope writes from the settings stays the scope the line after this one
		// writes into the stored progress.
		fields: "delivery_settings.deploy_repo, .excluded_repos",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				nameWrite(r, "delivery_settings", "deploy_repo"),
				{write: "delivery_settings.excluded_repos", sql: `
					update delivery_settings set excluded_repos = array(
						select case when ` + r.named("e.repo") + ` then ` + r.to() + ` else e.repo end
						from unnest(excluded_repos) with ordinality e(repo, n) order by e.n)
					where ` + excludedNamed(r)},
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"delivery_settings", r.named("deploy_repo") + ` or ` + excludedNamed(r)}}
		},
	},
	{
		// The scope says which settings a step's stored progress was measured against, and the
		// reconcile backfills a step afresh when its scope differs from the one it writes now. So
		// the scope gets the new name wherever the settings got it, and the progress stays.
		fields: "delivery_reconcile_progress.scope",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{{write: "delivery_reconcile_progress.scope", sql: `
				update delivery_reconcile_progress
				set scope = regexp_replace(scope, ` + r.token() + `, ` + r.tokenReplacement() + `, 'gi')
				where scope ~* ` + r.token()}}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"delivery_reconcile_progress", `scope ~* ` + r.token()}}
		},
	},
	{
		// A source deleted for its twin takes its components with it (components_source_fkey,
		// ON DELETE CASCADE). The facts recorded for those components stay until the project's
		// next import, which deletes the facts of every component no source projects.
		fields: "architecture_sources.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "architecture_sources", "project_key"),
				nameWrite(r, "architecture_sources", "repo"),
			}
		},
		left: func(r repoRename) []repoRenameRows {
			return []repoRenameRows{{"architecture_sources", r.named("repo")}}
		},
	},
	nameLine("ci_reconcile_passes", "cursor_repository"),
}
