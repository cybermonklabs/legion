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
// (cmd/dispatch/main.go). Writers that use refs.Renames store the new name from then on.
// Delivery intake and reconcile can still use the old name until the pass rewrites their
// settings. A previous release can also write an old name during a roll.
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
// daemon's own readings (legion_current, legion_observations), the results of spec checks
// (spec_check_results) or github_rate_limits, whose scope names no repository. A row of
// spec_check_results is the record of one CI run, with its repository as the run's results token
// named it, which the row's in-toto statement holds too: nothing updates or deletes such a row,
// and no read selects or compares by its repository column.
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

// rewriteRepoRenameLine runs line's statements in one transaction and commits it: its row locks
// and its copies, each over every pair at once, and then its steps for each pair. It answers the
// rows it wrote and the rows it deleted. It logs its writes and its deletions only once they are
// committed, so a line that fails logs nothing but its error.
//
// A line that holds its rows (hold) gives each statement after hold the keys of the rows it holds
// as $1 (heldRow): the rows hold locked, and each row a statement of the line has written since,
// under the key the write left it with.
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
	var held []string
	holding := map[string]bool{}
	keep := func(keys ...string) {
		for _, key := range keys {
			if !holding[key] {
				holding[key] = true
				held = append(held, key)
			}
		}
	}
	args := func() []any {
		if line.hold == nil {
			return nil
		}
		return []any{held}
	}
	if line.hold != nil {
		keys, err := returnedKeys(ctx, tx, line.hold(renames))
		if err != nil {
			return 0, 0, fmt.Errorf("lock the delivery rows before rewriting them: %w", err)
		}
		keep(keys...)
	}
	// Lock every pair's rows before the first write. A trigger for an earlier pair must not
	// take the change log's lock ahead of a later pair's rows.
	if line.lockRows != nil {
		for _, statement := range line.lockRows(renames) {
			if _, err := tx.Exec(ctx, statement, args()...); err != nil {
				return 0, 0, fmt.Errorf("lock the delivery rows before rewriting them: %w", err)
			}
		}
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
	// The copies run before every step, so their statement is the line's first that fires a
	// change trigger, and a statement fires its rows' triggers at its end: the insert waits for a
	// row another writer holds under a new name while the line does not hold the change log's
	// lock. The line holds each row they insert, and the steps write it.
	if line.copies != nil {
		copies := line.copies(renames)
		copied, err := tx.Query(ctx, copies.sql, args()...)
		if err != nil {
			return 0, 0, fmt.Errorf("copy the rows of %s under the new names: %w", copies.table, err)
		}
		var inserted, key *string
		var pair *int
		if _, err := pgx.ForEachRow(copied, []any{&inserted, &pair, &key}, func() error {
			if inserted != nil {
				keep(*inserted)
			} else {
				deletions = append(deletions, deletion{copies.table, *key, renames[*pair]})
			}
			return nil
		}); err != nil {
			return 0, 0, fmt.Errorf("copy the rows of %s under the new names: %w", copies.table, err)
		}
	}
	for _, r := range renames {
		for _, step := range line.steps(r) {
			if step.deletes {
				keys, err := returnedKeys(ctx, tx, step.sql, args()...)
				if err != nil {
					return 0, 0, fmt.Errorf("delete the rows of %s whose key the new name %s holds: %w", step.table, r.To, err)
				}
				for _, key := range keys {
					deletions = append(deletions, deletion{step.table, key, r})
				}
				continue
			}
			var count int64
			if line.hold != nil {
				// The write returns the key of each row it wrote, as the write left it.
				keys, err := returnedKeys(ctx, tx, step.sql, args()...)
				if err != nil {
					return 0, 0, fmt.Errorf("write %s in place of %s: %w", r.To, r.From, err)
				}
				keep(keys...)
				count = int64(len(keys))
			} else {
				tag, err := tx.Exec(ctx, step.sql)
				if err != nil {
					return 0, 0, fmt.Errorf("write %s in place of %s: %w", r.To, r.From, err)
				}
				count = tag.RowsAffected()
			}
			if step.write != "" && count > 0 {
				writes = append(writes, written{step.table + "." + step.write, count, r})
				rows += count
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

// returnedKeys runs a statement that returns one key for each row it locked, wrote or deleted, as
// text, and answers those keys.
func returnedKeys(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
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
	// hold, where a line has it, is the line's first lock: it locks the rows the line writes,
	// before lockRows, and returns the key of each, as text (runKey and its like). Each
	// statement of the line after it writes only the rows the line holds (heldRow, heldOnly):
	// those, and each row a statement of the line has written since, under the key the write
	// left it with, so each write step of such a line is heldOnly's. A row another writer stores
	// under an old name after the line's locks stays there for the next pass, and no statement of
	// the line waits on it.
	hold func([]repoRename) string
	// lockRows names the rows a delivery line writes, in the order intake takes them, after hold.
	lockRows func([]repoRename) []string
	// copies is the statement of a line whose rows cannot take the new name in place, over
	// every pair at once, that runs after lockRows and before the steps.
	copies func([]repoRename) repoRenameCopies
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
// the line counts already.
type repoRenameStep struct {
	table, where string
	write        string
	deletes      bool
	sql          string
}

// repoRenameCopies is a statement that inserts a copy of rows of table under their pair's new
// name and deletes, as collisions, the rows under an old name it does not copy. It returns three
// columns for each row it inserted or deleted: a row it inserted has its key (runKey), which its
// line then holds, and a null pair and deletion; a row it deleted has a null key, the index of its
// pair among the pass's pairs and its key as the deletion logs it, as text, and each is counted
// and logged at WARN as a collisions step's are.
type repoRenameCopies struct {
	table, sql string
}

// The key of a row of each table whose line holds its rows (hold), as text over the table's own
// columns: a run by its repository and id, which name it in its jobs too, a pull request by its
// repository and number, and the delivery settings by their one row.
const (
	runKey         = `jsonb_build_array(repo, run_id)::text`
	pullRequestKey = `jsonb_build_array(repo, number)::text`
	settingsKey    = `singleton::text`
)

// heldRow is true where key, a row's key as runKey and its like spell it, is the key of a row its
// line holds: one of the line's argument $1.
func heldRow(key string) string {
	return key + ` = any($1::text[])`
}

// heldOnly limits step, a write whose statement ends in its where, to the rows its line holds,
// whose key is key, and returns the key of each row it writes, as the write leaves it, which the
// line then holds.
func heldOnly(key string, step repoRenameStep) repoRenameStep {
	step.sql += ` and ` + heldRow(key) + ` returning ` + key
	return step
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
// sorts first stays for the write after this. also, where it is not empty, is one more condition
// the deleted row meets, on its own columns with no alias, as heldRow is.
func collisions(r repoRename, table, also string, keys ...string) repoRenameStep {
	same := ""
	key := `'repo', d.repo`
	for _, column := range keys {
		same += ` and t.` + column + ` = d.` + column
		key += `, '` + column + `', d.` + column
	}
	if also != "" {
		also = ` and ` + also
	}
	return repoRenameStep{
		table:   table,
		where:   r.named("repo"),
		deletes: true,
		sql: `delete from ` + table + ` d
			where ` + r.named("d.repo") + also + `
			  and exists (
				select 1 from ` + table + ` t
				where (lower(t.repo) = ` + r.to() + ` or (` + r.named("t.repo") + ` and t.repo < d.repo))` + same + `)
			returning jsonb_build_object(` + key + `)::text`,
	}
}

// copyRuns copies each run its line holds under an old name to its pair's new name, for every
// pair in one statement, and deletes each such run that it does not copy, as a collision: a run
// under the new name, in any case, holds its key, or another run under an old name comes first
// for the same new name and id, in the order of the pairs and then of the repositories, as the
// lines that take a pair at a time meet them. A run that another writer stores under the new name
// while the statement runs collides too: the insert waits for that writer's row, finds the key
// taken and copies nothing, and the run under the old name is deleted. A run another writer
// stores under an old name after the line's locks is not one the line holds, and the statement
// neither copies nor deletes it. The delete reads which runs the insert copied from its returned
// keys, since the statement's parts all read the table as it began. Postgres fires the
// statement's change triggers at its end, so the insert waits for another writer's row before
// the line takes the change log's lock. The copy goes through the row's jsonb, so it carries
// every column of the table, a column a later migration adds included.
func copyRuns(renames []repoRename) repoRenameCopies {
	pairs := make([]string, 0, len(renames))
	for i, r := range renames {
		pairs = append(pairs, fmt.Sprintf(`(%d, %s, %s)`, i, r.from(), r.to()))
	}
	return repoRenameCopies{table: "delivery_runs", sql: `
		with stored as materialized (
			select s.repo, s.run_id, p.pair, p.new, to_jsonb(s) || jsonb_build_object('repo', p.new) as copy,
				row_number() over (partition by p.new, s.run_id order by p.pair, s.repo) = 1
				and not exists (select 1 from delivery_runs t where lower(t.repo) = p.new and t.run_id = s.run_id) as copies
			from delivery_runs s
			join (values ` + strings.Join(pairs, `, `) + `) p (pair, old, new) on lower(s.repo) = p.old
			where ` + heldRow(runKey) + `
		), copied as (
			insert into delivery_runs
			select moved.* from stored
			cross join lateral jsonb_populate_record(null::delivery_runs, stored.copy) moved
			where stored.copies
			on conflict (repo, run_id) do nothing
			returning repo, run_id, ` + runKey + ` as key
		), deleted as (
			delete from delivery_runs d using stored s
			where d.repo = s.repo and d.run_id = s.run_id
			  and not (s.copies and exists (select 1 from copied c where c.repo = s.new and c.run_id = s.run_id))
			returning s.pair, jsonb_build_object('repo', d.repo, 'run_id', d.run_id)::text as logged
		)
		select key, null::integer, null::text from copied
		union all
		select null::text, pair, logged from deleted`}
}

// nameLine is the line of one name column of a table with no key that holds it.
func nameLine(table, column string) repoRenameLine {
	return repoRenameLine{
		fields: table + "." + column,
		steps:  func(r repoRename) []repoRenameStep { return []repoRenameStep{nameWrite(r, table, column)} },
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

// anyRepoRename joins the row predicates for a line that takes its locks across all pairs.
func anyRepoRename(renames []repoRename, predicate func(repoRename) string) string {
	terms := make([]string, 0, len(renames))
	for _, r := range renames {
		terms = append(terms, predicate(r))
	}
	return strings.Join(terms, " or ")
}

// repoRenameLines are the stored repository fields of Dispatch, in the order the pass writes them,
// one transaction for each line. They are each column of the schema that holds a repository name
// or a URL that names one, but for those the package comment says the pass never writes.
var repoRenameLines = []repoRenameLine{
	nameLine("issues", "repo"),
	nameLine("projects", "default_repo"),
	// The repository each issue's stored Legion facts name (legionfacts.ReadRepos), resolved
	// from the two lines above when the row was computed. Its trigger logs each row it writes
	// as an issue_facts change (20261117100002), which wakes the Legion board of its project.
	nameLine("issue_facts", "repo"),
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
				collisions(r, "pull_request_heads", "", "number"),
				nameWrite(r, "pull_request_heads", "repo"),
				urlWrite(r, "pull_request_heads", "url"),
			}
		},
	},
	{
		fields: "branch_commits.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "branch_commits", "", "branch", "sha"),
				nameWrite(r, "branch_commits", "repo"),
			}
		},
	},
	{
		fields: "commit_holds.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "commit_holds", "", "commit_sha", "merge_sha"),
				nameWrite(r, "commit_holds", "repo"),
			}
		},
	},
	// A program's current deploy is its newest row under the repository's name
	// (delivery.CurrentDeploys), so the rows recorded under the old name join the new name's
	// history rather than leave the program with no deploy until its next one. No key of the
	// table holds the repository, so nothing collides.
	nameLine("deploys", "repo"),
	{
		// Each cited URL is rewritten in its place. A URL the array then holds twice is kept once,
		// at its first place, as delivery's citedIssues keeps each URL once. The line writes only
		// the pull requests it holds (hold): a pull request another writer stores naming an old
		// name after the line's locks stays as it is for the next pass.
		fields: "delivery_pull_requests.repo, .url, .attribution_cited_issues",
		hold: func(renames []repoRename) string {
			where := anyRepoRename(renames, func(r repoRename) string {
				return r.named("repo") + " or " + r.urlNamed("url") + " or " + citedNamed(r)
			})
			return `select ` + pullRequestKey + ` from delivery_pull_requests where ` + where + ` order by repo, number for update`
		},
		steps: func(r repoRename) []repoRenameStep {
			where := citedNamed(r)
			return []repoRenameStep{
				collisions(r, "delivery_pull_requests", heldRow(pullRequestKey), "number"),
				heldOnly(pullRequestKey, nameWrite(r, "delivery_pull_requests", "repo")),
				heldOnly(pullRequestKey, urlWrite(r, "delivery_pull_requests", "url")),
				heldOnly(pullRequestKey, repoRenameStep{table: "delivery_pull_requests", where: where, write: "attribution_cited_issues", sql: `
					update delivery_pull_requests set attribution_cited_issues = array(
						select cited.url from (
							select ` + r.url.RewriteSQL("e.url") + ` as url, min(e.n) as n
							from unnest(attribution_cited_issues) with ordinality e(url, n)
							group by 1
						) cited order by cited.n)
					where ` + where}),
			}
		},
	},
	{
		// A run's jobs name it by (repo, run_id), and that foreign key has no ON UPDATE
		// (0081_delivery_runs), so a run cannot be renamed in place. Each run is copied under the
		// new name (copyRuns), or, where it collides, deleted with its jobs (ON DELETE CASCADE),
		// and the row under the new name keeps its own jobs. Then the jobs of each copied run are
		// pointed at the copy, and the old run, which no job names any more, is deleted. The line
		// writes only the runs it holds (hold), the runs it locked first and its copies of them. A
		// run another writer stores under an old name after those locks stays there with its
		// jobs, counted among the rows left, for the next pass, and the line, which holds the
		// change log's lock after its copies, never waits on such a row.
		fields: "delivery_runs.repo, .url with delivery_run_jobs.repo",
		// Copying a run logs a change before its jobs move. Lock runs before jobs as intake
		// does, with the stronger lock needed to delete runs and move job keys.
		hold: func(renames []repoRename) string {
			runs := anyRepoRename(renames, func(r repoRename) string {
				return r.named("repo") + " or " + r.urlNamed("url")
			})
			return `select ` + runKey + ` from delivery_runs where ` + runs + ` order by repo, run_id for update`
		},
		lockRows: func(renames []repoRename) []string {
			jobs := anyRepoRename(renames, func(r repoRename) string { return r.named("repo") })
			return []string{
				`select 1 from delivery_run_jobs where (` + jobs + `) and ` + heldRow(runKey) +
					` order by repo, run_id, name collate "C" for update`,
			}
		},
		copies: copyRuns,
		steps: func(r repoRename) []repoRenameStep {
			where := r.named("repo")
			return []repoRenameStep{
				heldOnly(runKey, nameWrite(r, "delivery_run_jobs", "repo")),
				// The held runs left under the old name are the ones copyRuns copied, so this
				// delete ends each move, and its rows are the runs the line moved.
				heldOnly(runKey, repoRenameStep{table: "delivery_runs", where: where, write: "repo", sql: `delete from delivery_runs where ` + where}),
				heldOnly(runKey, urlWrite(r, "delivery_runs", "url")),
			}
		},
	},
	{
		fields: "delivery_run_warnings.repo",
		steps: func(r repoRename) []repoRenameStep {
			return []repoRenameStep{
				collisions(r, "delivery_run_warnings", "", "run_id", "job_names", "apply_step"),
				nameWrite(r, "delivery_run_warnings", "repo"),
			}
		},
	},
	nameLine("delivery_changes", "repo"),
	{
		// Each excluded repository is rewritten in its place, and none is dropped, so the scope
		// searchProgressScope writes from the settings stays the scope the line after this one
		// writes into the stored progress. The line writes the settings only where it holds them
		// (hold): settings another writer first stores after the line's lock stay for the next
		// pass.
		fields: "delivery_settings.deploy_repo, .excluded_repos",
		hold: func(renames []repoRename) string {
			where := anyRepoRename(renames, func(r repoRename) string {
				return r.named("deploy_repo") + " or " + excludedNamed(r)
			})
			return `select ` + settingsKey + ` from delivery_settings where ` + where + ` for no key update`
		},
		steps: func(r repoRename) []repoRenameStep {
			where := excludedNamed(r)
			return []repoRenameStep{
				heldOnly(settingsKey, nameWrite(r, "delivery_settings", "deploy_repo")),
				heldOnly(settingsKey, repoRenameStep{table: "delivery_settings", where: where, write: "excluded_repos", sql: `
					update delivery_settings set excluded_repos = array(
						select case when ` + r.named("e.repo") + ` then ` + r.to() + ` else e.repo end
						from unnest(excluded_repos) with ordinality e(repo, n) order by e.n)
					where ` + where}),
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
				collisions(r, "architecture_sources", "", "project_key"),
				nameWrite(r, "architecture_sources", "repo"),
			}
		},
	},
	nameLine("ci_reconcile_passes", "cursor_repository"),
}
