// Package codesurvival holds what Dispatch keeps of the code survival and post-merge rework
// measurement of dispatch://INFRA-3206: the five tables of migration 20261126103753_code_survival,
// and the reads and writes of them that the daily run and the regroup take.
//
// What the tables hold is the result of a run and nothing a run read: a measured change's counts,
// its day, its head branch, its author's login and its issue; the pairs of a change and a later one
// that removed its lines; each replayed day's fingerprint, the count and the sha256 of its
// first-parent commits' SHAs, and the state of the day; the repositories the run reads; and the
// groups each project's day rows stand built under. A patch is never an argument of any function
// here, and no column holds one (TestTheResultsTablesHoldNoTextOfAPatch).
//
// Every function takes the caller's connection, a transaction where the caller's writes must
// stand or fall together (ReplaceDays, MarkDays, LockProject), so a run decides where its
// transactions begin and end. The shapes the functions take and answer are the survival package's
// (survival.MeasureOutput, survival.Fingerprint, survival.DayStatus, survival.Change and
// survival.Pair), so the run hands what Measure answered to the store and the store's reads to
// SelectDays, ChangedDays and the regroup with no conversion between.
package codesurvival

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// Lease is the name of the lease the daily run takes (duty.RunLeased), so one process runs each
// tick, and the name the lease pool reserves the run's connection by (cmd/dispatch/main.go).
const Lease = "code-survival"

// Querier is what the reads and the plain writes need of their connection: the pool, or a
// transaction. A caller that wants its statements to stand or fall together passes a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RepoState is the state of a repository in code_repos.
type RepoState string

// The states of a repository: one whose branch the App can read, and one it cannot.
const (
	RepoReadable   RepoState = "readable"
	RepoUnreadable RepoState = "unreadable"
)

// Repo is one row of code_repos: a repository the run reads. A zero time is a column that holds
// null, and so is an empty Branch. Reason is the text of an unreadable repository's state, which
// the table holds exactly where State is RepoUnreadable.
type Repo struct {
	// Repo is the repository's name in lower case, as projects.default_repo holds it.
	Repo   string
	Branch string
	// StartDay is the first day the run measures: no day before it is (R4).
	StartDay time.Time
	State    RepoState
	Reason   string
	// CheckedOn is the UTC day of the last daily pass that finished reading the repository, and
	// LastMeasuredOn the UTC day a run last stored a result for it.
	CheckedOn      time.Time
	LastMeasuredOn time.Time
	// RetryAt is when a run that stopped at the budget may start again.
	RetryAt time.Time
}

// repoColumns are the columns of code_repos in the order scanRepo reads them.
const repoColumns = `repo, coalesce(branch, ''), start_day, state, coalesce(state_reason, ''), checked_on, last_measured_on, retry_at`

// GetRepo is repo's row, and whether the table holds one.
func GetRepo(ctx context.Context, db Querier, repo string) (Repo, bool, error) {
	var (
		found                                 Repo
		start, checked, lastMeasured, retryAt *time.Time
		state                                 string
	)
	err := db.QueryRow(ctx, `select `+repoColumns+` from code_repos where repo = $1`, repo).
		Scan(&found.Repo, &found.Branch, &start, &state, &found.Reason, &checked, &lastMeasured, &retryAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, false, nil
	}
	if err != nil {
		return Repo{}, false, fmt.Errorf("read the code repository %s: %w", repo, err)
	}
	found.State = RepoState(state)
	found.StartDay, found.CheckedOn, found.LastMeasuredOn, found.RetryAt = utc(start), utc(checked), utc(lastMeasured), utc(retryAt)
	return found, true, nil
}

// PutRepo writes the whole row of r.Repo: a row that exists is replaced in every column, so a
// caller that changes one reads the row first (GetRepo). The table refuses a name that is not in
// lower case, a state that is neither, and an unreadable repository with no reason.
func PutRepo(ctx context.Context, db Querier, r Repo) error {
	if _, err := db.Exec(ctx, `
		insert into code_repos (repo, branch, start_day, state, state_reason, checked_on, last_measured_on, retry_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (repo) do update set
			branch = excluded.branch, start_day = excluded.start_day, state = excluded.state,
			state_reason = excluded.state_reason, checked_on = excluded.checked_on,
			last_measured_on = excluded.last_measured_on, retry_at = excluded.retry_at`,
		r.Repo, nullString(r.Branch), nullTime(r.StartDay), string(r.State), nullString(r.Reason),
		nullTime(r.CheckedOn), nullTime(r.LastMeasuredOn), nullTime(r.RetryAt)); err != nil {
		return fmt.Errorf("write the code repository %s: %w", r.Repo, err)
	}
	return nil
}

// ReplaceDays stores one run's results in the caller's transaction: the rows of days in
// code_changes, code_pairs and code_days are replaced whole, and the fingerprints of the other days
// the run replayed are written beside them. days are the days the run selected, UTC midnights, and
// out is what survival.Measure answered for them.
//
// For each selected day it deletes the day's rows (a change's pairs go with it) and inserts out's:
// the changes and pairs, and one code_days row, measured or blocked as the day's result says, with
// the fingerprint out holds for it, the time of the replace as measured_at (of a measured day) and
// as tried_at, and, for a blocked day, the commit that blocked it and the count of its files. A
// day's old row carries nothing over, so a replace clears the mark of a day it measures and the
// blocking commit of a day it no longer blocks. Every other day out fingerprints is written as
// replayed, a fingerprint and nothing else, and only where the table holds no row of the day or a
// replayed one: a measured, marked or blocked day is never overwritten by a fingerprint.
//
// It refuses before it writes anything an output that does not match days: a day that is no UTC
// midnight, a selected day with no result or no fingerprint, a result of a day that was not
// selected or that is neither measured nor blocked, a change of a day that holds no measurement,
// and a pair whose adder is no change of out. A row the table refuses, a change whose lines outrun
// its added lines, say, fails the call after the statements before it ran, so the caller rolls the
// transaction back and the store is as it was: the run's results go in whole or not at all.
func ReplaceDays(ctx context.Context, tx pgx.Tx, repo string, days []time.Time, out survival.MeasureOutput) error {
	plan, err := planReplace(days, out)
	if err != nil {
		return fmt.Errorf("replace the days of %s: %w", repo, err)
	}
	if _, err := tx.Exec(ctx, `delete from code_changes where repo = $1 and day = any($2::date[])`, repo, plan.days); err != nil {
		return fmt.Errorf("delete the changes of the selected days of %s: %w", repo, err)
	}
	if _, err := tx.Exec(ctx, `delete from code_days where repo = $1 and day = any($2::date[])`, repo, plan.days); err != nil {
		return fmt.Errorf("delete the days of %s: %w", repo, err)
	}

	changes := make([][]any, len(out.Changes))
	for i, c := range out.Changes {
		changes[i] = []any{repo, c.SHA, c.Day, c.CommittedAt, c.PR, c.HeadRef, c.Author, nullString(c.IssueKey), c.Added, c.AliveAtMark, c.RemovedByNonPR}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"code_changes"},
		[]string{"repo", "sha", "day", "committed_at", "pr_number", "head_ref", "author", "issue_key", "added", "alive", "removed_by_non_pr"},
		pgx.CopyFromRows(changes)); err != nil {
		return fmt.Errorf("store the changes of %s: %w", repo, err)
	}
	pairs := make([][]any, len(out.Pairs))
	for i, p := range out.Pairs {
		pairs[i] = []any{repo, p.AdderSHA, p.RemoverSHA, p.Remover.PR, p.Remover.HeadRef, p.Remover.Author, nullString(p.Remover.IssueKey), p.Lines, p.DaysBetween}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"code_pairs"},
		[]string{"repo", "adder_sha", "remover_sha", "remover_pr", "remover_head_ref", "remover_author", "remover_issue_key", "lines", "days_between"},
		pgx.CopyFromRows(pairs)); err != nil {
		return fmt.Errorf("store the pairs of %s: %w", repo, err)
	}

	var (
		selectedDays = make([]time.Time, len(plan.days))
		commits      = make([]int, len(plan.days))
		digests      = make([][]byte, len(plan.days))
		states       = make([]string, len(plan.days))
		blockedSHAs  = make([]*string, len(plan.days))
		blockedFiles = make([]*int, len(plan.days))
	)
	for i, day := range plan.days {
		result, fp := plan.results[dayKey(day)], plan.prints[dayKey(day)]
		selectedDays[i], commits[i], digests[i], states[i] = day, fp.Commits, fp.Digest[:], string(result.State)
		if result.State == survival.DayBlocked {
			blockedSHAs[i], blockedFiles[i] = &result.BlockedSHA, &result.BlockedFiles
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into code_days (repo, day, commits, digest, state, measured_at, blocked_sha, blocked_files, tried_at)
		select $1::text, d.day, d.commits, d.digest, d.state, case when d.state = 'measured' then now() end,
			d.blocked_sha, d.blocked_files, now()
		from unnest($2::date[], $3::int[], $4::bytea[], $5::text[], $6::text[], $7::int[])
			as d(day, commits, digest, state, blocked_sha, blocked_files)`,
		repo, selectedDays, commits, digests, states, blockedSHAs, blockedFiles); err != nil {
		return fmt.Errorf("store the selected days of %s: %w", repo, err)
	}

	var replayed []survival.Fingerprint
	for _, fp := range out.Fingerprints {
		if !plan.selected[dayKey(fp.Day)] {
			replayed = append(replayed, fp)
		}
	}
	if len(replayed) == 0 {
		return nil
	}
	var (
		replayedDays    = make([]time.Time, len(replayed))
		replayedCommits = make([]int, len(replayed))
		replayedDigests = make([][]byte, len(replayed))
	)
	for i, fp := range replayed {
		replayedDays[i], replayedCommits[i], replayedDigests[i] = fp.Day, fp.Commits, fp.Digest[:]
	}
	if _, err := tx.Exec(ctx, `
		insert into code_days (repo, day, commits, digest, state)
		select $1::text, d.day, d.commits, d.digest, 'replayed'
		from unnest($2::date[], $3::int[], $4::bytea[]) as d(day, commits, digest)
		on conflict (repo, day) do update set commits = excluded.commits, digest = excluded.digest
		where code_days.state = 'replayed'`,
		repo, replayedDays, replayedCommits, replayedDigests); err != nil {
		return fmt.Errorf("store the fingerprints of the replayed days of %s: %w", repo, err)
	}
	return nil
}

// replacement is the output of one run, checked against the days it was run for.
type replacement struct {
	// selected is the selected days' numbers, and days the days themselves, sorted, each once.
	selected map[int64]bool
	days     []time.Time
	// results and prints are each selected day's result and fingerprint, by day number.
	results map[int64]survival.DayResult
	prints  map[int64]survival.Fingerprint
}

// planReplace checks out against days, as ReplaceDays says, and answers what it writes.
func planReplace(days []time.Time, out survival.MeasureOutput) (replacement, error) {
	if len(days) == 0 {
		return replacement{}, errors.New("no day to replace")
	}
	plan := replacement{selected: map[int64]bool{}, results: map[int64]survival.DayResult{}, prints: map[int64]survival.Fingerprint{}}
	for _, day := range days {
		if err := checkMidnight(day); err != nil {
			return replacement{}, err
		}
		if !plan.selected[dayKey(day)] {
			plan.selected[dayKey(day)] = true
			plan.days = append(plan.days, day.UTC())
		}
	}
	slices.SortFunc(plan.days, time.Time.Compare)

	for _, result := range out.Days {
		if err := checkMidnight(result.Day); err != nil {
			return replacement{}, err
		}
		key := dayKey(result.Day)
		switch {
		case !plan.selected[key]:
			return replacement{}, fmt.Errorf("the result of %s is of a day that was not selected", dayName(result.Day))
		case result.State != survival.DayMeasured && result.State != survival.DayBlocked:
			return replacement{}, fmt.Errorf("the result of %s is %q, neither measured nor blocked", dayName(result.Day), result.State)
		}
		if _, twice := plan.results[key]; twice {
			return replacement{}, fmt.Errorf("two results of %s", dayName(result.Day))
		}
		plan.results[key] = result
	}
	for _, print := range out.Fingerprints {
		if err := checkMidnight(print.Day); err != nil {
			return replacement{}, err
		}
		if _, twice := plan.prints[dayKey(print.Day)]; twice {
			return replacement{}, fmt.Errorf("two fingerprints of %s", dayName(print.Day))
		}
		plan.prints[dayKey(print.Day)] = print
	}
	for _, day := range plan.days {
		if _, found := plan.results[dayKey(day)]; !found {
			return replacement{}, fmt.Errorf("the selected day %s has no result", dayName(day))
		}
		if _, found := plan.prints[dayKey(day)]; !found {
			return replacement{}, fmt.Errorf("the selected day %s has no fingerprint", dayName(day))
		}
	}

	stored := make(map[string]bool, len(out.Changes))
	for _, change := range out.Changes {
		if result, found := plan.results[dayKey(change.Day)]; !found || result.State != survival.DayMeasured {
			return replacement{}, fmt.Errorf("the change %s is of %s, which holds no measurement", change.SHA, dayName(change.Day))
		}
		stored[change.SHA] = true
	}
	for _, pair := range out.Pairs {
		if !stored[pair.AdderSHA] {
			return replacement{}, fmt.Errorf("the pair of adder %s and remover %s names an adder that is no change of the result", pair.AdderSHA, pair.RemoverSHA)
		}
	}
	return plan, nil
}

// MarkDays marks, in the caller's transaction, each measured day of days of repo: its state
// becomes marked, with at as the time of the mark (R8). It marks measured days alone: a day that
// holds a fingerprint only, a blocked day, a day with no row, and a day marked already, which
// keeps the time of its first mark, are left as they are. It answers the days it marked, in order.
// It writes nothing outside code_days, so a caller that must also move other rows with the marks
// does so in its own statements, in the same transaction, and the marks alone commit before a run
// goes on to read anything else.
func MarkDays(ctx context.Context, tx pgx.Tx, repo string, days []time.Time, at time.Time) ([]time.Time, error) {
	for _, day := range days {
		if err := checkMidnight(day); err != nil {
			return nil, fmt.Errorf("mark the days of %s: %w", repo, err)
		}
	}
	rows, err := tx.Query(ctx, `
		update code_days set state = 'marked', marked_at = $3
		where repo = $1 and day = any($2::date[]) and state = 'measured'
		returning day`, repo, days, at)
	if err != nil {
		return nil, fmt.Errorf("mark the days of %s: %w", repo, err)
	}
	marked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Time, error) {
		var day time.Time
		err := row.Scan(&day)
		return day.UTC(), err
	})
	if err != nil {
		return nil, fmt.Errorf("mark the days of %s: %w", repo, err)
	}
	slices.SortFunc(marked, time.Time.Compare)
	return marked, nil
}

// FingerprintsFrom is the fingerprint of every day of repo that holds one, whatever the day's
// state, from day on, in day order: what a run compares with the chain it read (survival.ChangedDays).
func FingerprintsFrom(ctx context.Context, db Querier, repo string, from time.Time) ([]survival.Fingerprint, error) {
	rows, err := db.Query(ctx, `select day, commits, digest from code_days where repo = $1 and day >= $2 order by day`, repo, from)
	if err != nil {
		return nil, fmt.Errorf("read the fingerprints of %s: %w", repo, err)
	}
	prints, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (survival.Fingerprint, error) {
		var (
			print  survival.Fingerprint
			digest []byte
		)
		if err := row.Scan(&print.Day, &print.Commits, &digest); err != nil {
			return survival.Fingerprint{}, err
		}
		if len(digest) != len(print.Digest) {
			return survival.Fingerprint{}, fmt.Errorf("the digest of %s holds %d bytes, want %d", dayName(print.Day), len(digest), len(print.Digest))
		}
		print.Day = print.Day.UTC()
		copy(print.Digest[:], digest)
		return print, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the fingerprints of %s: %w", repo, err)
	}
	return prints, nil
}

// DayStatuses is the state of every day of repo from from to to, both included, in day order, as
// survival.SelectDays reads it: each day's state, and for a day a run tried, when. A day with no
// row is not listed, and a day that holds a fingerprint alone is listed as replayed.
func DayStatuses(ctx context.Context, db Querier, repo string, from, to time.Time) ([]survival.DayStatus, error) {
	rows, err := db.Query(ctx, `
		select day, state, tried_at from code_days
		where repo = $1 and day >= $2 and day <= $3 order by day`, repo, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the days of %s: %w", repo, err)
	}
	statuses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (survival.DayStatus, error) {
		var (
			status survival.DayStatus
			state  string
			tried  *time.Time
		)
		if err := row.Scan(&status.Day, &state, &tried); err != nil {
			return survival.DayStatus{}, err
		}
		status.Day, status.State, status.TriedAt = status.Day.UTC(), survival.DayState(state), utc(tried)
		return status, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the days of %s: %w", repo, err)
	}
	return statuses, nil
}

// Changes is every change of repo on days, in the order the commits were made: what a run
// measured, as survival.Measure answered it.
func Changes(ctx context.Context, db Querier, repo string, days []time.Time) ([]survival.Change, error) {
	if len(days) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		select sha, day, committed_at, pr_number, head_ref, author, coalesce(issue_key, ''), added, alive, removed_by_non_pr
		from code_changes where repo = $1 and day = any($2::date[]) order by committed_at, sha`, repo, days)
	if err != nil {
		return nil, fmt.Errorf("read the changes of %s: %w", repo, err)
	}
	changes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (survival.Change, error) {
		var change survival.Change
		err := row.Scan(&change.SHA, &change.Day, &change.CommittedAt, &change.PR, &change.HeadRef, &change.Author, &change.IssueKey,
			&change.Added, &change.AliveAtMark, &change.RemovedByNonPR)
		change.Day, change.CommittedAt = change.Day.UTC(), change.CommittedAt.UTC()
		return change, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the changes of %s: %w", repo, err)
	}
	return changes, nil
}

// Pairs is every pair of repo whose adder is a change on days, in the order of the adders' commits
// and then of the removers' SHAs: what a run measured, as survival.Measure answered it.
func Pairs(ctx context.Context, db Querier, repo string, days []time.Time) ([]survival.Pair, error) {
	if len(days) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		select p.adder_sha, p.remover_sha, p.remover_pr, p.remover_head_ref, p.remover_author,
			coalesce(p.remover_issue_key, ''), p.lines, p.days_between
		from code_pairs p
		join code_changes c on c.repo = p.repo and c.sha = p.adder_sha
		where c.repo = $1 and c.day = any($2::date[])
		order by c.committed_at, c.sha, p.remover_sha`, repo, days)
	if err != nil {
		return nil, fmt.Errorf("read the pairs of %s: %w", repo, err)
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (survival.Pair, error) {
		var pair survival.Pair
		err := row.Scan(&pair.AdderSHA, &pair.RemoverSHA, &pair.Remover.PR, &pair.Remover.HeadRef, &pair.Remover.Author,
			&pair.Remover.IssueKey, &pair.Lines, &pair.DaysBetween)
		return pair, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the pairs of %s: %w", repo, err)
	}
	return pairs, nil
}

// Project is one row of code_projects: the sha256 of the code groups a project's day rows stand
// built under, and when they were built. A project whose rows were never built has the zero
// BuiltAt, and its GroupsDigest means nothing.
type Project struct {
	Key          string
	GroupsDigest [32]byte
	BuiltAt      time.Time
}

// Built reports whether the project's day rows were built, under GroupsDigest.
func (p Project) Built() bool { return !p.BuiltAt.IsZero() }

// scanProject reads a code_projects row of the columns `project_key, groups_digest, built_at`.
func scanProject(row pgx.Row) (Project, error) {
	var (
		project Project
		digest  []byte
		built   *time.Time
	)
	if err := row.Scan(&project.Key, &digest, &built); err != nil {
		return Project{}, err
	}
	if built != nil {
		if len(digest) != len(project.GroupsDigest) {
			return Project{}, fmt.Errorf("the groups digest of %s holds %d bytes, want %d", project.Key, len(digest), len(project.GroupsDigest))
		}
		copy(project.GroupsDigest[:], digest)
		project.BuiltAt = built.UTC()
	}
	return project, nil
}

// ProjectDigest is the groups digest of key's day rows, and when they were built. A project with
// no row, or a row that holds no digest yet, is not built.
func ProjectDigest(ctx context.Context, db Querier, key string) (Project, error) {
	project, err := scanProject(db.QueryRow(ctx, `select project_key, groups_digest, built_at from code_projects where project_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{Key: key}, nil
	}
	if err != nil {
		return Project{}, fmt.Errorf("read the groups digest of %s: %w", key, err)
	}
	return project, nil
}

// LockProject locks key's row of code_projects, `select … for update`, in the caller's
// transaction, until it ends, and answers what the row holds. A builder of the project's day rows,
// the run or the regroup, holds it from before it reads the digest until it has stored the new one,
// so two builders never build one project's rows together. A project that has no row gets one, not
// built, so there is a row to lock; the project must exist.
func LockProject(ctx context.Context, tx pgx.Tx, key string) (Project, error) {
	if _, err := tx.Exec(ctx, `insert into code_projects (project_key) values ($1) on conflict (project_key) do nothing`, key); err != nil {
		return Project{}, fmt.Errorf("make the row of %s in code_projects: %w", key, err)
	}
	project, err := scanProject(tx.QueryRow(ctx, `select project_key, groups_digest, built_at from code_projects where project_key = $1 for update`, key))
	if err != nil {
		return Project{}, fmt.Errorf("lock the row of %s in code_projects: %w", key, err)
	}
	return project, nil
}

// StoreProject writes the digest of the groups key's day rows stand built under, and when they
// were built. A builder holds the row (LockProject) in the transaction it calls this in.
func StoreProject(ctx context.Context, db Querier, key string, digest [32]byte, builtAt time.Time) error {
	if _, err := db.Exec(ctx, `
		insert into code_projects (project_key, groups_digest, built_at) values ($1, $2, $3)
		on conflict (project_key) do update set groups_digest = excluded.groups_digest, built_at = excluded.built_at`,
		key, digest[:], builtAt); err != nil {
		return fmt.Errorf("store the groups digest of %s: %w", key, err)
	}
	return nil
}

// dayKey is the number of the UTC day a UTC midnight starts, the key days are compared by.
func dayKey(day time.Time) int64 { return day.Unix() / 86400 }

// dayName is day as its date, for an error that names it.
func dayName(day time.Time) string { return day.UTC().Format(time.DateOnly) }

// checkMidnight refuses a day that is no UTC midnight: every day of the store is a date.
func checkMidnight(day time.Time) error {
	if !day.Equal(survival.DayOf(day)) {
		return fmt.Errorf("the day %s is no UTC midnight", day.UTC().Format(time.RFC3339))
	}
	return nil
}

// nullString is s as a column value: null for the empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTime is t as a column value: null for the zero time.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// utc is *t in UTC, and the zero time for a null column.
func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
