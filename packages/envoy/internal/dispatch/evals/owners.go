package evals

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// owners.go is where what a repository attributes is kept (dispatch://INFRA-2916, R15 and plan
// D1): what counts for each project whose default
// repository is its repository, and what counts for a project only as long as that project's
// default repository is its repository, are selections made at read time. A pull request whose
// attribution list (delivery_days.go's attributionList) is not constant, a CI run of such a pull
// request, of a pull request no issue attributes or of none, and a deploy run where it ships such
// a pull request or ships none, with each job of it a deploy card follows, have their day rows,
// members and owed days written once, whatever the defaults are, under an owner key of their
// repository (OwnerKey) in eval_days, eval_day_members and eval_days_owed, where a project's rows
// stand under its key (migration 20261117100006): the refresher's delivery family treats an owner
// key as one more project, its walks, pages, spans, settings rewrites and rebuilds alike, and every
// read of a project folds its own rows with those of the owner keys it reads under the published
// defaults (eval_project_owners, which eval_owner_readers decides for a key that carries lists), so
// a move of a default repository moves what each project reads and rewrites no day row: the commit
// that publishes it marks stale every cut of the stored figures it moves rows into or out of, the
// project's own cut ('') as much as a repository's, and folds only each marked cut's first paint
// (foldMarkedFirstPaints); the refresher folds the rest of each marked cut again (publishDefaults,
// refoldStaleCuts, figures.go).
//
// An owner key carries what decides its readers. Its sharers: a deploy run that ships pull
// requests that count for projects whatever the defaults are (a constant list's terminal) counts
// for those projects in their own rows, and its repository's row of it names them as its sharers,
// which skip it, so such a project counts the run once, through its issue (plan D1 (iii)). And its
// lists: each attribution list of a pull request it counts that is not constant, as its rows count
// for whichever project the list picks under the published defaults (its first condition whose
// default is the key's repository, else its terminal), and, where some list picks none, or the key
// carries none (a plain key), for each project whose default is the key's repository. One owner key
// stands for each distinct set of sharers and lists, so both are part of the rows' key: two deploys
// of one day with different sharers, or pull requests of one day with different lists, are rows
// of two keys.

// ownerPrefix begins every owner key, and no project key (projects_key_check,
// ^[A-Z][A-Z0-9]{1,9}$, migration 0001): an owner key never names a project.
const ownerPrefix = ":"

// OwnerKey is the owner key of the rows repo attributes, the projects of sharers count in their own
// rows already and lists decide the readers of: ':' + repo + '|' + the sorted, distinct keys of
// sharers joined by ',', and, where some list holds a condition or a terminal, '|' + the sorted,
// distinct lists (attributionList.String) joined by ';'. A key whose every list is none (a pull
// request no issue attributes, a deploy that ships only such pull requests or ships none) is the
// plain key ':' + repo + '|' + sharers, which every project whose default is repo reads. repo is the
// repository's canonical name, as every delivery table keeps it. It is the one place the key is
// made (and parseOwner the one place it is read), beside the change trigger's reading of it
// (eval_days_change_log, migration 20261117100014). No repository name holds '|', and no project
// key ',', ';' or '>', so the key reads back as it was made.
func OwnerKey(repo string, sharers []string, lists []attributionList) string {
	key := ownerPrefix + repo + "|" + strings.Join(slices.Compact(slices.Sorted(slices.Values(sharers))), ",")
	encoded := make([]string, 0, len(lists))
	carried := false
	for _, list := range lists {
		encoded = append(encoded, list.String())
		carried = carried || !list.none()
	}
	if !carried {
		return key
	}
	slices.Sort(encoded)
	return key + "|" + strings.Join(slices.Compact(encoded), ";")
}

// isOwner reports whether key is an owner key (OwnerKey) rather than a project's.
func isOwner(key string) bool {
	return strings.HasPrefix(key, ownerPrefix)
}

// parseOwner is the repository, the sharers and the lists (each as attributionList.String writes
// it, as eval_day_owners.lists keeps it) of the owner key key, false where key is none.
func parseOwner(key string) (string, []string, []string, bool) {
	rest, ok := strings.CutPrefix(key, ownerPrefix)
	repo, rest, found := strings.Cut(rest, "|")
	if !ok || !found || repo == "" {
		return "", nil, nil, false
	}
	joined, carried, _ := strings.Cut(rest, "|")
	var sharers, lists []string
	if joined != "" {
		sharers = strings.Split(joined, ",")
	}
	if carried != "" {
		lists = strings.Split(carried, ";")
	}
	return repo, sharers, lists, true
}

// readOwners reads every owner key eval_days has held a row of (eval_day_owners), in key order:
// the owners the delivery walk writes the days of beside the projects.
func readOwners(ctx context.Context, q delivery.Querier) ([]string, error) {
	rows, err := q.Query(ctx, `select owner from eval_day_owners order by owner collate "C"`)
	if err != nil {
		return nil, fmt.Errorf("read the owner keys of the day rows: %w", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the owner keys of the day rows: %w", err)
	}
	return owners, nil
}

// recordOwners records in eval_day_owners, in tx, each owner key of keys it does not hold yet, with
// its repository, its sharers and its lists, as the eval_days change trigger records a key the
// first time a statement writes a row of it (migration 20261117100014): a rebuild stages its rows
// aside first (stageRows), and its fold of the stored figures reads the staged rows through
// eval_project_owners before the flip writes them, so a key whose first rows it stages is read by
// the projects its readers are. A key whose staged rows the rebuild abandons stands with no row,
// as one whose rows all went.
func recordOwners(ctx context.Context, tx pgx.Tx, keys []string) error {
	type owner struct{ key, repo, sharers, lists string }
	var owners []owner
	for _, key := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		if repo, shared, carried, ok := parseOwner(key); ok {
			owners = append(owners, owner{key: key, repo: repo, sharers: strings.Join(shared, ","), lists: strings.Join(carried, ";")})
		}
	}
	// The keys grow with the history a rebuild stages, so they go in statements of at most
	// statementBytes, in key order, the order each inserts them in.
	size := func(one *owner) int { return 4*4 + len(one.key) + len(one.repo) + len(one.sharers) + len(one.lists) }
	for chunk := range statementChunks(owners, paramBytes(4), size) {
		owned, repos, sharers, lists := make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
		for i, one := range chunk {
			owned[i], repos[i], sharers[i], lists[i] = one.key, one.repo, one.sharers, one.lists
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_day_owners (owner, repo, sharers, lists)
			select given.owner, given.repo, coalesce(string_to_array(nullif(given.sharers, ''), ','), '{}'),
				coalesce(string_to_array(nullif(given.lists, ''), ';'), '{}')
			from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as given(owner, repo, sharers, lists)
			order by given.owner collate "C"
			on conflict do nothing
		`, owned, repos, sharers, lists); err != nil {
			return fmt.Errorf("record %d owner keys a rebuild stages rows of: %w", len(owners), err)
		}
	}
	return nil
}

// readersOf reads in q the projects whose reads fold the rows of each of owners
// (eval_project_owners), by owner, each owner's in key order: for a plain key, the projects whose
// published default is its repository, less its sharers; for a key that carries lists, the
// projects eval_owner_readers picks. One read for every owner, in statements of at most
// statementBytes; an owner no project reads has none.
func readersOf(ctx context.Context, q delivery.Querier, owners []string) (map[string][]string, error) {
	readers := map[string][]string{}
	for chunk := range statementChunks(owners, paramBytes(1), textBytes) {
		rows, err := q.Query(ctx, `
			select owner, project_key from eval_project_owners where owner = any($1) order by project_key collate "C"
		`, chunk)
		if err != nil {
			return nil, fmt.Errorf("read the projects that read %d owner keys: %w", len(owners), err)
		}
		var owner, project string
		if _, err := pgx.ForEachRow(rows, []any{&owner, &project}, func() error {
			readers[owner] = append(readers[owner], project)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the projects that read %d owner keys: %w", len(owners), err)
		}
	}
	return readers, nil
}

// projectOwner is one key whose rows a project's reads fold (eval_project_owners): the key, and
// whether the project reads its rows through the repository (via, viaAsRead).
type projectOwner struct {
	key string
	via bool
}

// ownersIn reads in q the keys whose rows each of projects' reads fold (eval_project_owners): the
// project's own, the plain owner keys of the repository it publishes as its default, less those it
// shares, and the owner keys carrying lists whose readers hold it (eval_owner_readers), each with
// its via, each project's in key order. The read-time path builds each of them (ComputeAtRead).
func ownersIn(ctx context.Context, q delivery.Querier, projects []string) (map[string][]projectOwner, error) {
	rows, err := q.Query(ctx, `
		select project_key, owner, via from eval_project_owners where project_key = any($1::text[])
		order by project_key, owner collate "C"`, projects)
	if err != nil {
		return nil, fmt.Errorf("read the owners of the day rows of %v: %w", projects, err)
	}
	owners := map[string][]projectOwner{}
	var project string
	var owner projectOwner
	if _, err := pgx.ForEachRow(rows, []any{&project, &owner.key, &owner.via}, func() error {
		owners[project] = append(owners[project], owner)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the owners of the day rows of %v: %w", projects, err)
	}
	return owners, nil
}

// PublishedDefaultIn is the repository project publishes as its default, read in q
// (eval_project_repos, the one seam a read takes its repositories from), "" where it publishes
// none yet. A Legion row stored under legionfacts.DefaultRepo is a row of that repository, which
// an items read shows and filters by.
func PublishedDefaultIn(ctx context.Context, q delivery.Querier, project string) (string, error) {
	var repo string
	if err := q.QueryRow(ctx, `select coalesce(max(repo), '') from eval_project_repos where project_key = $1`, project).Scan(&repo); err != nil {
		return "", fmt.Errorf("read the published default repository of %s: %w", project, err)
	}
	return repo, nil
}

// asReader is the eval id evalID, of owner's rows, as project's read of them names it: the same
// slug under project.
func asReader(owner, project, evalID string) string {
	return EvalID(project, slugOf(owner, evalID))
}

// evalIDAs is asReader's rule in SQL: the eval id evalID, of the key of, under key, the same slug
// (key || substr(evalID, length(of) + 1)), each argument an expression of the statement: an owner
// key's eval id as a project reads it, or a project's eval id as an owner key stores it.
func evalIDAs(key, evalID, of string) string {
	return key + ` || substr(` + evalID + `, length(` + of + `) + 1)`
}

// viaAsRead is via, the via_repository column of a day row of the owner o of eval_project_owners
// names, as the project o names reads it, evalID being the row's eval id (as stored, or as the
// project reads it): the rows a project reads through an issue of its own count none through a
// repository (o.via false: its own rows, and an owner key's whose list picks it), and an owner
// key's rows it reads through the repository count every row (o.via true), as their
// via_repository does. The code evals' own rows (CodeSlugs) are the one exception: their
// writer counts in via_repository the changes with no issue that reached the project through its
// default repository (dispatch://INFRA-3206/comment/81b94de5-418c-4aa5-8e61-c8b83e7ac984), so a
// project reads its own rows of those evals as stored. No owner key holds a row of them. Every read
// of a project's day rows through eval_project_owners reads via_repository through it.
func viaAsRead(via, evalID string) string {
	return `case when o.via then ` + via + ` when o.owner = o.project_key and substr(` + evalID + `, length(o.project_key) + 2) in (` +
		ownViaSlugs + `) then ` + via + ` else 0 end`
}

// ownViaSlugs is CodeSlugs as a list of SQL text literals, the slugs whose own rows viaAsRead
// reads as stored. A slug holds no quote (slugPattern).
var ownViaSlugs = "'" + strings.Join(CodeSlugs, "', '") + "'"

// ownedKeys is the condition that a row of eval_days or eval_day_members (alias) stands under one of
// the keys the projects read (eval_project_owners), where projects is the condition on
// eval_project_owners.project_key that names them (`= $1`, `= any($1::text[])`). A read joins
// eval_project_owners o on alias.project_key = o.owner, and the planner joins the view by a hash,
// which carries that equality into no index condition of the table: without this condition a read
// of a repository's rows takes every project's rows of that repository and of the days through its
// index on (project_key, repo, day), and the items read of one eval merges every project's rows of
// it. The array is one InitPlan, so each index range of the read is bounded by the keys. Measured at
// INFRA's size on the build farm, the eval list's 84-day read of one repository with a compare
// range took 93 to 132 ms (median 125, 8 fresh connections) of execution without it and 92 to
// 131 ms (median 99) with it and its values in the plan (DayWindowIn); on the pod, at a tenth of
// that size, it reads 8,985 + 9,272 heap rows for the 8,985 + 9,272 it keeps where it read
// 13,417 + 9,372, and the items read of one eval takes 3.4 ms where it took 455 ms.
func ownedKeys(alias, projects string) string {
	return alias + `.project_key = any(array(select owner from eval_project_owners where project_key ` + projects + `))`
}

// repoCut is the condition that a row (alias) read through eval_project_owners o is one of the
// repositories repos (a text[] expression) names: a row of one of them, or a row of the project's
// own stored under legionfacts.DefaultRepo (a Legion row of an issue whose own chain names no
// repository) where project (an expression of its key) publishes one of them as its default
// (eval_project_repos), since such a row is one of that repository: a move of the default moves the
// cut and rewrites no row (dispatch://INFRA-2916, plan D1 (vi)), and marks the stored figures of
// the two defaults' cuts stale until they are folded again (publishDefaults). The stored figures
// keep the fold of each cut a read of one repository reads by cutOf, the same rule read the other
// way. A read that names no repository adds none of it.
func repoCut(alias, repos, project string) string {
	return `(` + alias + `.repo = any(` + repos + `) or (` + alias + `.repo = '` + legionfacts.DefaultRepo + `' and ` + alias + `.project_key = o.project_key and exists (
		select 1 from eval_project_repos p where p.project_key = ` + project + ` and p.repo = any(` + repos + `))))`
}

// ownedDayColumns selects the day as the project reads it, with repo its cut and samples the list
// the items read needs, or SQL null when it needs none.
func ownedDayColumns(repo, samples string) string {
	return `o.project_key, ` + evalIDAs("o.project_key", "d.eval_id", "o.owner") + `, d.deploy_repo, d.series, ` + repo + ` as cut_repo,
	d.day, d.listed, d.n, d.sum, d.num, d.den, d.max, ` + samples + `, d.last_value, d.last_at, d.last_recorded_at,
	coalesce(d.last_reading::text, ''), d.counts, ` + viaAsRead("d.via_repository", "d.eval_id") + `, d.built_by, d.updated_at`
}

// addOwnedRows is days, ordered by day, then series and repository, of one eval and deploy
// repository, with the rows of one series, repository and day that several owners hold (a
// project's own and its default repository's, read under the project) added into one, as a fold
// of the day adds their parts (addDayRow): a project's day is one row of each series and cut.
func addOwnedRows(days []DayRow) []DayRow {
	merged := days[:0]
	for _, day := range days {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if last.DeployRepo == day.DeployRepo && last.Series == day.Series && last.Repo == day.Repo && last.Day.Equal(day.Day) {
				addDayRow(last, day)
				continue
			}
		}
		merged = append(merged, day)
	}
	return merged
}

// addDayRow adds row's part into into's, as the folds add two parts of one day: counts, sums,
// numerators, denominators and the via-repository count summed, the larger maximum, the samples of
// both, the newer last value, and the older moment of a write (the freshness a read answers).
func addDayRow(into *DayRow, row DayRow) {
	add := func(a, b *float64) *float64 {
		switch {
		case a == nil:
			return b
		case b == nil:
			return a
		}
		return new(*a + *b)
	}
	into.N += row.N
	into.Sum, into.Num, into.Den = add(into.Sum, row.Sum), add(into.Num, row.Num), add(into.Den, row.Den)
	if row.Max != nil && (into.Max == nil || *row.Max > *into.Max) {
		into.Max = row.Max
	}
	into.Samples = append(slices.Clone(into.Samples), row.Samples...)
	if len(row.Counts) > 0 {
		counts := make(map[string]float64, len(into.Counts)+len(row.Counts))
		for name, value := range into.Counts {
			counts[name] += value
		}
		for name, value := range row.Counts {
			counts[name] += value
		}
		into.Counts = counts
	}
	into.ViaRepository += row.ViaRepository
	if row.LastAt != nil && (into.LastAt == nil || row.LastAt.After(*into.LastAt)) {
		into.LastValue, into.LastAt, into.LastRecordedAt, into.LastReading = row.LastValue, row.LastAt, row.LastRecordedAt, row.LastReading
	}
	if row.UpdatedAt.Before(into.UpdatedAt) {
		into.UpdatedAt = row.UpdatedAt
	}
}
