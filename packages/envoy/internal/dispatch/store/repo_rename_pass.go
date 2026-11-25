package store

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
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
// Beside those fields the pass writes only the stale marks of the eval figures' cuts a rename
// moves (markRenamedCuts). It writes no other column, updated_at included, and appends no event.
// It never writes repo_projects, the event log, the text of documents, comments, asks and
// messages, the reference tables read from that text (refs, block_refs, graph_edges),
// architecture_snapshots.files, a daemon's own readings (legion_current, legion_observations) or
// github_rate_limits, whose scope names no repository.
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
// (docs.copiedAsksLockNamespace), the owner locks' (OwnerLockNamespace), the owner lock gate's
// (OwnerLockGateNamespace), the loop leases' (duty.leaseNamespace), the test hooks' (the api
// package's testHookLockNamespace), a conditional edit's admission's
// (docs.conditionalEditQueueLockNamespace), the issue dependencies' (api's
// lockProjectIssueDependencies) and the deployment's own issue dependencies' (api's
// lockDeploymentIssueDependencies), none of them this one.
const repoRenameLockNamespace int32 = 0x524E414D

// repoRenameLockTimeout is how long a statement of the pass waits for a lock: the advisory lock
// of a line that another process is writing, or a row that a request holds. A line that waits
// longer fails and is logged, and the next boot writes it.
const repoRenameLockTimeout = `set local lock_timeout = '5s'`

// repoRenamesLeftTimeout bounds each count of the rows left on an old name, the one before the
// lines and the one after them. The count has a bound of its own, which the end of the pass's
// bound does not end, so a pass whose lines ran out of time still says how much they left. The
// count is one statement over the pass's tables.
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
//
// It first counts those rows too, under repoRenamesLeftTimeout. Where that count finds none, it
// runs no line and answers none left, as a pass after a pass that wrote everything would. Where
// that count fails, it runs every line, and the failure is not counted.
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
	if left, err := countRepoRenamesLeftBounded(ctx, pool, renames); err == nil && left == 0 {
		pass.Left = RepoRenamesLeft{Counted: true}
		return pass
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
	left, err := countRepoRenamesLeftBounded(ctx, pool, renames)
	if err != nil {
		pass.Failures++
		slog.Error("dispatch: count the rows left on a renamed repository's old name", "error", err)
		return pass
	}
	pass.Left = RepoRenamesLeft{Rows: left, Counted: true}
	return pass
}

// countRepoRenamesLeftBounded is countRepoRenamesLeft under repoRenamesLeftTimeout, a bound of
// its own that the end of ctx does not end.
func countRepoRenamesLeftBounded(ctx context.Context, pool *Pool, renames []repoRename) (int64, error) {
	countCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repoRenamesLeftTimeout)
	defer cancel()
	return countRepoRenamesLeft(countCtx, pool, renames)
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
			if step.deletes {
				keys, err := deletedKeys(ctx, tx, step.sql)
				if err != nil {
					return 0, 0, fmt.Errorf("delete the rows of %s whose key the new name %s holds: %w", step.table, r.To, err)
				}
				for _, key := range keys {
					deletions = append(deletions, deletion{step.table, key, r})
				}
				continue
			}
			tag, err := tx.Exec(ctx, step.sql)
			if err != nil {
				return 0, 0, fmt.Errorf("write %s in place of %s: %w", r.To, r.From, err)
			}
			if step.write != "" && tag.RowsAffected() > 0 {
				writes = append(writes, written{step.table + "." + step.write, tag.RowsAffected(), r})
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
// statement over every line and pair. It reads them from the line's steps for the pair: a row of a
// step's table is left where it meets the step's where. A row counts once for its line and pair,
// however many of that line's steps it meets.
func countRepoRenamesLeft(ctx context.Context, pool *Pool, renames []repoRename) (int64, error) {
	// tableRows is one table of a line, with the where of each of the line's steps on it.
	type tableRows struct {
		table  string
		wheres []string
	}
	var terms []string
	for _, line := range repoRenameLines {
		for _, r := range renames {
			var tables []tableRows
			for _, step := range line.steps(r) {
				i := slices.IndexFunc(tables, func(t tableRows) bool { return t.table == step.table })
				if i < 0 {
					i = len(tables)
					tables = append(tables, tableRows{table: step.table})
				}
				if !slices.Contains(tables[i].wheres, step.where) {
					tables[i].wheres = append(tables[i].wheres, step.where)
				}
			}
			for _, t := range tables {
				terms = append(terms, `(select count(*) from `+t.table+` where (`+strings.Join(t.wheres, `) or (`)+`))`)
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

// urlNamed is true where column, a URL, names the old name by the URL rule: the pattern RewriteSQL
// replaces. The rewrite changes each URL it matches, as the new name differs from the old one in
// lower case.
func (r repoRename) urlNamed(column string) string {
	return column + ` ~ ` + QuoteLiteral(r.url.pattern)
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
}

// repoRenameStep is one statement of a line. table is the table that the statement writes or
// deletes from. where is the condition, on that table's columns with no alias, that a row of it
// meets while the statement still has that row to write or delete: a field of the row names the
// old name. The count of the rows left (countRepoRenamesLeft) reads only table and where, and a
// pass whose first count finds no row runs no line, so a field that no step's where reads is a
// field the pass never comes back to.
//
// write names the column of table that the statement writes: its rows are counted and logged at
// INFO, under the field table.write. deletes is true where the statement deletes the colliding
// rows of table: it returns the key of each row it deleted, as text, and each is counted and
// logged at WARN. A step that neither writes nor deletes is a statement whose rows a write step of
// the line counts already, or one that writes no field of the pass (markRenamedCuts).
type repoRenameStep struct {
	table, where string
	write        string
	deletes      bool
	sql          string
}

// nameWrite writes the new name in column of table wherever the column holds the old name.
func nameWrite(r repoRename, table, column string) repoRenameStep {
	where := r.named(column)
	return repoRenameStep{
		table: table,
		where: where,
		write: column,
		sql:   `update ` + table + ` set ` + column + ` = ` + r.to() + ` where ` + where,
	}
}

// urlWrite rewrites column of table, a URL, wherever it names the old name.
func urlWrite(r repoRename, table, column string) repoRenameStep {
	where := r.urlNamed(column)
	return repoRenameStep{
		table: table,
		where: where,
		write: column,
		sql:   `update ` + table + ` set ` + column + ` = ` + r.url.RewriteSQL(column) + ` where ` + where,
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
		table:   table,
		where:   r.named("repo"),
		deletes: true,
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
	}
}

// ownerNamed is true where column, an owner key of the eval day rows (evals.OwnerKey), holds the
// old name as its repository, in any case: the key is ':' + repository + '|' + its sharers, and
// '|' + its lists where it carries any, which name projects alone.
func ownerNamed(r repoRename, column string) string {
	return `lower(split_part(` + column + `, '|', 1)) = ` + QuoteLiteral(":"+strings.ToLower(r.From))
}

// ownerRenamed is column, an owner key, with the new name for its repository.
func ownerRenamed(r repoRename, column string) string {
	return QuoteLiteral(":"+r.To) + ` || substr(` + column + `, length(split_part(` + column + `, '|', 1)) + 1)`
}

// ownerWrite writes the new name in the owner key of table's project_key, and, where the table
// holds eval ids under that key (eval_days), in the eval id's project too.
func ownerWrite(r repoRename, table string, evalIDs bool) repoRenameStep {
	where := ownerNamed(r, "project_key")
	set := `project_key = ` + ownerRenamed(r, "project_key")
	if evalIDs {
		set += `, eval_id = ` + ownerRenamed(r, "project_key") + ` || substr(eval_id, length(project_key) + 1)`
	}
	return repoRenameStep{
		table: table,
		where: where,
		write: "project_key",
		sql:   `update ` + table + ` set ` + set + ` where ` + where,
	}
}

// ownerCollisions deletes each row of table under an owner key of the old name whose key, with the
// new name for its repository, is the key of another row, as collisions does for a repo column.
// The eval id of a row holds its owner key too (ownerWrite rewrites both), so the keys compare
// the slug that follows it rather than the id.
func ownerCollisions(r repoRename, table string, keys ...string) repoRenameStep {
	same := ""
	key := `'project_key', d.project_key`
	for _, column := range keys {
		if column == "eval_id" {
			same += ` and substr(t.eval_id, length(t.project_key) + 1) = substr(d.eval_id, length(d.project_key) + 1)`
		} else {
			same += ` and t.` + column + ` = d.` + column
		}
		key += `, '` + column + `', d.` + column + `::text`
	}
	return repoRenameStep{
		table:   table,
		where:   ownerNamed(r, "project_key"),
		deletes: true,
		sql: `delete from ` + table + ` d
			where ` + ownerNamed(r, "d.project_key") + `
			  and exists (
				select 1 from ` + table + ` t
				where (t.project_key = ` + ownerRenamed(r, "d.project_key") + `
					or (` + ownerNamed(r, "t.project_key") + ` and t.project_key < d.project_key))` + same + `)
			returning jsonb_build_object(` + key + `)::text`,
	}
}

// markRenamedCuts marks stale (eval_figure_cuts_stale) the stored eval figures' cut of the old name
// and the cut of the new name of each project that publishes the new name as its default
// (eval_days_defaults), which the line's write before it gave every project that published the old
// name, or that holds stored figures of a cut of the old name (eval_range_figures,
// eval_day_figures), each name matched in any case: the old name's cut under each spelling the
// stored figures hold it in, and, of a published default, under the old name.
// It marks them as evals.markStaleCuts marks the cuts a move of a default moves, but for every eval
// (evals null): a cut no mark holds at generation 1, and one a mark holds at its next generation,
// its refold to begin again from the cut's first end day (next_day null), for every eval whatever
// the standing mark named. It takes the cuts in the byte order of their keys, the order every
// writer of the marks takes them in.
//
// It runs after the line writes eval_days_defaults, as evals.publishDefaults writes the defaults
// and then the marks in one transaction, so a refresher of another process that publishes a move
// beside the pass takes the rows in the same order and waits for the line rather than deadlocking
// with it. A project that published the new name before the rename has its two cuts marked too,
// which costs their refold and leaves each of their figures the fold of the rows that stand.
//
// Its where is false: a mark holds no field the pass writes, and the mark of the old name's cut
// names the old name until the refresher's refold clears it, so the count of the rows left reads no
// mark. The line's write steps keep the line in that count until it commits, and the marks with it.
func markRenamedCuts(r repoRename) repoRenameStep {
	named := r.named("repo")
	return repoRenameStep{
		table: "eval_figure_cuts_stale",
		where: "false",
		sql: `with old as (
				select project_key, ` + r.from() + ` as repo from eval_days_defaults where lower(repo) = ` + r.to() + `
				union select project_key, repo from eval_range_figures where ` + named + `
				union select project_key, repo from eval_day_figures where ` + named + `
			)
			insert into eval_figure_cuts_stale (project_key, repo, generation, next_day, evals)
			select project_key, repo, 1, null, null
			from (select project_key, repo from old union select project_key, ` + r.to() + ` from old) cut
			order by project_key collate "C", repo collate "C"
			on conflict (project_key, repo) do update set generation = eval_figure_cuts_stale.generation + 1, next_day = null, evals = null`,
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
	// The repository each issue's stored Legion facts name (legionfacts.ReadRepos), resolved
	// from the two lines above when the row was computed. Its trigger logs each row it writes
	// as an issue_facts change (20261117100002), so the day-row refresher writes that issue's
	// Legion day rows again, under the new name.
	nameLine("issue_facts", "repo"),
	// What a repository attributes is kept under its owner key in the eval day rows, their
	// members and their owed days (evals.OwnerKey, migration 20261117100006): the key holds the
	// repository's name, and so does the eval id of each row, so the pass rewrites both. A row
	// under the new name already (a repository renamed to one that holds rows) is left, and the
	// old name's row of the same key goes, as every other line's collision does. The day-row
	// refresher writes the new name's rows again from their input, and the eval_days change each
	// write logs wakes the views that read them.
	//
	// The published defaults (eval_days_defaults) take the new name in the same transaction, so
	// the refresher sees no move of a default and marks no cut stale itself. But a project's own
	// Legion rows of an issue whose chain names no repository are stored under
	// legionfacts.DefaultRepo (':default'), never under a name, and a read folds them into the
	// cut of the default the project publishes: no row of them moves, and no change the pass's
	// writes log names them, so their stored figures stay in the old name's cut and the new
	// name's holds none of them. So the line, once it has written the defaults, marks both cuts
	// of each such project stale, every eval of them (markRenamedCuts), in the order
	// evals.publishDefaults takes them (defaults, then marks), so a refresher of another process
	// publishing beside it waits rather than deadlocks. The refresher's next pass folds every
	// marked cut again (evals.refoldStaleCuts, which every pass runs), and a read of a marked cut
	// folds the day rows until then.
	{
		fields: "eval_days.project_key, eval_day_members.project_key, eval_days_owed.project_key, eval_day_owners.owner, eval_days_defaults.repo",
		steps: func(r repoRename) []repoRenameStep {
			var steps []repoRenameStep
			for _, one := range []struct {
				table string
				keys  []string
			}{
				{"eval_days", []string{"eval_id", "writer", "deploy_repo", "series", "repo", "day"}},
				{"eval_day_members", []string{"family", "deploy_repo", "row_key"}},
				{"eval_days_owed", []string{"family", "day"}},
			} {
				steps = append(steps, ownerCollisions(r, one.table, one.keys...), ownerWrite(r, one.table, one.table == "eval_days"))
			}
			return append(steps,
				repoRenameStep{
					table: "eval_day_owners", where: ownerNamed(r, "owner"), deletes: true,
					sql: `delete from eval_day_owners d where ` + ownerNamed(r, "d.owner") + `
						and exists (select 1 from eval_day_owners t where t.owner = ` + ownerRenamed(r, "d.owner") + `)
						returning jsonb_build_object('owner', d.owner)::text`,
				},
				repoRenameStep{
					table: "eval_day_owners", where: ownerNamed(r, "owner"), write: "owner",
					sql: `update eval_day_owners set owner = ` + ownerRenamed(r, "owner") + `, repo = ` + r.to() +
						` where ` + ownerNamed(r, "owner"),
				},
				nameWrite(r, "eval_days_defaults", "repo"),
				markRenamedCuts(r))
		},
	},
	nameLine("check_results", "commit_repo"),
	{
		// A URL links one issue (issue_external_links_url), and readers compare a link in lower
		// case (issue_external_links_url_lower). So an old URL collides with every link whose
		// lower case is its rewrite in lower case. `old` reads the old URLs once, with their
		// rewrites, so the collision costs one scan of the table and a probe of the index for each
		// old URL.
		fields: "issue_external_links.url",
		steps: func(r repoRename) []repoRenameStep {
			where := r.urlNamed("url")
			return []repoRenameStep{
				{table: "issue_external_links", where: where, deletes: true, sql: `
					with old as materialized (
						select url, lower(` + r.url.RewriteSQL("url") + `) as rewritten
						from issue_external_links where ` + where + `
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
	},
	{
		fields: "branch_commits.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "branch_commits", "branch", "sha"),
				nameWrite(r, "branch_commits", "repo"),
			}
		},
	},
	{
		// Each cited URL is rewritten in its place. A URL the array then holds twice is kept once,
		// at its first place, as delivery's citedIssues keeps each URL once.
		fields: "delivery_pull_requests.repo, .url, .attribution_cited_issues",
		steps: func(r repoRename) []repoRenameStep {
			where := citedNamed(r)
			return []repoRenameStep{
				collisions(r, "delivery_pull_requests", "number"),
				nameWrite(r, "delivery_pull_requests", "repo"),
				urlWrite(r, "delivery_pull_requests", "url"),
				{table: "delivery_pull_requests", where: where, write: "attribution_cited_issues", sql: `
					update delivery_pull_requests set attribution_cited_issues = array(
						select cited.url from (
							select ` + r.url.RewriteSQL("e.url") + ` as url, min(e.n) as n
							from unnest(attribution_cited_issues) with ordinality e(url, n)
							group by 1
						) cited order by cited.n)
					where ` + where},
			}
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
			where := r.named("repo")
			return []repoRenameStep{
				collisions(r, "delivery_runs", "run_id"),
				{table: "delivery_runs", where: where, write: "repo", sql: `
					insert into delivery_runs
					select moved.* from delivery_runs stored
					cross join lateral jsonb_populate_record(null::delivery_runs,
						to_jsonb(stored) || jsonb_build_object('repo', ` + r.to() + `::text)) moved
					where ` + r.named("stored.repo")},
				nameWrite(r, "delivery_run_jobs", "repo"),
				{table: "delivery_runs", where: where, sql: `delete from delivery_runs where ` + where},
				urlWrite(r, "delivery_runs", "url"),
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
	},
	nameLine("delivery_changes", "repo"),
	{
		// Each excluded repository is rewritten in its place, and none is dropped, so the scope
		// searchProgressScope writes from the settings stays the scope the line after this one
		// writes into the stored progress.
		fields: "delivery_settings.deploy_repo, .excluded_repos",
		steps: func(r repoRename) []repoRenameStep {
			where := excludedNamed(r)
			return []repoRenameStep{
				nameWrite(r, "delivery_settings", "deploy_repo"),
				{table: "delivery_settings", where: where, write: "excluded_repos", sql: `
					update delivery_settings set excluded_repos = array(
						select case when ` + r.named("e.repo") + ` then ` + r.to() + ` else e.repo end
						from unnest(excluded_repos) with ordinality e(repo, n) order by e.n)
					where ` + where},
			}
		},
	},
	{
		// The scope says which settings a step's stored progress was measured against, and the
		// reconcile backfills a step afresh when its scope differs from the one it writes now. So
		// the scope gets the new name wherever the settings got it, and the progress stays.
		fields: "delivery_reconcile_progress.scope",
		steps: func(r repoRename) []repoRenameStep {
			where := `scope ~* ` + r.token()
			return []repoRenameStep{{table: "delivery_reconcile_progress", where: where, write: "scope", sql: `
				update delivery_reconcile_progress
				set scope = regexp_replace(scope, ` + r.token() + `, ` + r.tokenReplacement() + `, 'gi')
				where ` + where}}
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
	},
	nameLine("ci_reconcile_passes", "cursor_repository"),
}
