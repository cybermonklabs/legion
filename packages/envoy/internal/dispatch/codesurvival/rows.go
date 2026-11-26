package codesurvival

// rows.go builds the day rows of the two code evals of a project from the stored results, the
// one builder the run's results transaction, the marks' transaction and the regroup share (plan,
// "The day rows and their series"): which changes count for which project (R11), each project's
// day states, the cuts its rows are stored under, and the write of them (evals.WriteCodeDaysTx)
// beside the project's group series (evals.WriteCodeGroupSeriesTx) and its groups digest
// (code_projects).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// codeEvalIDs is project's eval ids of the two code evals.
func codeEvalIDs(project string) []string {
	return []string{evals.EvalID(project, evals.SlugCodeSurvival), evals.EvalID(project, evals.SlugPostMergeRework)}
}

// project is one project as the builders read it: its key, its default repository in lower case
// ("" for none), and its code groups in order, none where it saved none.
type project struct {
	Key         string
	DefaultRepo string
	Groups      []delivery.CodeGroup
}

// readProjects is every project of keys, or every project when keys is nil, in key order, with
// its default repository and its code groups (project_delivery_settings.code_groups, none for a
// project with no delivery settings).
func readProjects(ctx context.Context, db Querier, keys []string) ([]project, error) {
	rows, err := db.Query(ctx, `
		select p.key, lower(coalesce(p.default_repo, '')), coalesce(s.code_groups, '[]'::jsonb)
		from projects p left join project_delivery_settings s on s.project_key = p.key
		where $1::text[] is null or p.key = any($1::text[])
		order by p.key`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the projects' code groups: %w", err)
	}
	projects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (project, error) {
		var (
			read   project
			groups []byte
		)
		if err := row.Scan(&read.Key, &read.DefaultRepo, &groups); err != nil {
			return project{}, err
		}
		if err := json.Unmarshal(groups, &read.Groups); err != nil {
			return project{}, fmt.Errorf("decode the code groups of %s: %w", read.Key, err)
		}
		return read, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the projects' code groups: %w", err)
	}
	return projects, nil
}

// groupsDigest is the sha256 of groups' canonical JSON, the list as the settings hold it with
// every list of prefixes and logins written, empty or not: what code_projects.groups_digest holds
// of the groups a project's day rows stand built under.
func groupsDigest(groups []delivery.CodeGroup) [32]byte {
	canonical := make([]delivery.CodeGroup, len(groups))
	for index, group := range groups {
		canonical[index] = delivery.CodeGroup{Name: group.Name, BranchPrefixes: nonNil(group.BranchPrefixes), AuthorLogins: nonNil(group.AuthorLogins)}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// A list of strings always encodes.
		panic(fmt.Sprintf("codesurvival: encode code groups: %v", err))
	}
	return sha256.Sum256(encoded)
}

// nonNil is values, or an empty list for none.
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// groupNames is the names of groups, in order.
func groupNames(groups []delivery.CodeGroup) []string {
	names := make([]string, len(groups))
	for index, group := range groups {
		names[index] = group.Name
	}
	return names
}

// projectsOfRepo is the keys of every project repo's results on days count for (R11), sorted:
// each project whose default repository repo is, and each project of an issue of a change repo
// holds on days. A caller that replaces the days' changes reads it before the replace and adds the
// projects of the new changes' issues (projectsOfIssues), so a project that loses its last change
// of those days still has its rows of them rebuilt.
func projectsOfRepo(ctx context.Context, db Querier, repo string, days []time.Time) ([]string, error) {
	rows, err := db.Query(ctx, `
		select key from projects where lower(default_repo) = $1
		union
		select i.project_key from code_changes c join issues i on i.key = c.issue_key
		where c.repo = $1 and c.day = any($2::date[])
		order by 1`, repo, days)
	if err != nil {
		return nil, fmt.Errorf("read the projects of %s: %w", repo, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects of %s: %w", repo, err)
	}
	return keys, nil
}

// projectsOfIssues is the keys of the projects of the issues of changes, sorted.
func projectsOfIssues(ctx context.Context, db Querier, changes []survival.Change) ([]string, error) {
	var issues []string
	for _, change := range changes {
		if change.IssueKey != "" {
			issues = append(issues, change.IssueKey)
		}
	}
	if len(issues) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, `select distinct project_key from issues where key = any($1::text[]) order by 1`, issues)
	if err != nil {
		return nil, fmt.Errorf("read the projects of the changes' issues: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects of the changes' issues: %w", err)
	}
	return keys, nil
}

// union is the keys of every list, sorted, each once.
func union(lists ...[]string) []string {
	var all []string
	for _, list := range lists {
		all = append(all, list...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// builder is one transaction's build of the day rows of projects: the projects in key order, which
// it locks as every builder of them does (prepare), what each one's row of code_projects held when
// it was locked, and the build and moment the rows are written as.
type builder struct {
	tx       pgx.Tx
	build    string
	at       time.Time
	projects []project
	stood    map[string]Project
}

// prepare starts a build of the rows of keys in tx, the first statements of tx that take a lock,
// in the one order every builder (the run's results, WriteMarks, a rebuild and the regroup) takes
// them in:
//
//  1. each project's row of code_projects (LockProject, `select … for update`), in key order;
//  2. then each project's group series (evals.WriteCodeGroupSeriesTx, which takes the two code
//     evals' locks of the project), in key order;
//  3. then, in write, the day rows.
//
// So two builders of overlapping projects wait for each other at the first row they share and
// never in a cycle: a builder takes a project's eval locks only while it holds every row it needs,
// and no other builder can hold one of those. The row locks give tx a transaction id before it
// waits for an eval lock, which holds back the event log's readers for that wait
// (WriteCodeGroupSeriesTx); the wait is for another writer of the code evals' series, which holds
// them for its own short transaction.
//
// afterRows, when set, runs between steps 1 and 2: a test's hold of the two phases. A project whose
// code evals the seed has not written yet is left out: the eval day refresher seeds every project,
// and a regroup builds the rows of one that has none built (Regroup). It answers the keys it left
// out.
func prepare(ctx context.Context, tx pgx.Tx, keys []string, build string, at time.Time, afterRows func()) (*builder, []string, error) {
	b := &builder{tx: tx, build: build, at: at, stood: map[string]Project{}}
	if len(keys) == 0 {
		return b, nil, nil
	}
	projects, err := readProjects(ctx, tx, keys)
	if err != nil {
		return nil, nil, err
	}
	var unseeded []string
	for _, p := range projects {
		seeded := true
		for _, evalID := range codeEvalIDs(p.Key) {
			if _, found, err := evals.EvalIn(ctx, tx, evalID); err != nil {
				return nil, nil, fmt.Errorf("read the code evals of %s: %w", p.Key, err)
			} else if !found {
				seeded = false
			}
		}
		if !seeded {
			unseeded = append(unseeded, p.Key)
			continue
		}
		b.projects = append(b.projects, p)
	}
	for _, p := range b.projects {
		stood, err := LockProject(ctx, tx, p.Key)
		if err != nil {
			return nil, nil, err
		}
		b.stood[p.Key] = stood
	}
	if afterRows != nil {
		afterRows()
	}
	for _, p := range b.projects {
		if _, err := evals.WriteCodeGroupSeriesTx(ctx, tx, p.Key, groupNames(p.Groups)); err != nil {
			return nil, nil, fmt.Errorf("write the code group series of %s: %w", p.Key, err)
		}
	}
	return b, unseeded, nil
}

// write rebuilds, from the stored results tx reads, every prepared project's day rows of the code
// evals on days, and stores the digest of the groups they stand built under. A project whose rows
// were never built, or stand built under other groups than it holds now (a save the regroup has
// not reached yet), has every day rebuilt (decidedDays), as does every project where days is nil:
// so the digest it stores is that of the groups every row of the project stands built under.
func (b *builder) write(ctx context.Context, days []time.Time) error {
	for _, p := range b.projects {
		digest, written := groupsDigest(p.Groups), days
		if stood := b.stood[p.Key]; written == nil || !stood.Built() || stood.GroupsDigest != digest {
			var err error
			if written, err = decidedDays(ctx, b.tx, p); err != nil {
				return err
			}
		}
		if len(written) > 0 {
			rows, err := projectRows(ctx, b.tx, p, written)
			if err != nil {
				return err
			}
			if err := evals.WriteCodeDaysTx(ctx, b.tx, p.Key, codeEvalIDs(p.Key), written, rows, b.build); err != nil {
				return fmt.Errorf("write the code day rows of %s: %w", p.Key, err)
			}
		}
		if err := StoreProject(ctx, b.tx, p.Key, digest, b.at); err != nil {
			return err
		}
	}
	return nil
}

// decidedDays is every day p's code evals can hold a row of: each day its default repository
// measured, marked or blocked, each day of a change that counts for it, and each day it holds a
// row of either eval, so a rebuild deletes a row its input no longer builds.
func decidedDays(ctx context.Context, db Querier, p project) ([]time.Time, error) {
	rows, err := db.Query(ctx, `
		select day from code_days where repo = $2 and state <> 'replayed'
		union
		select c.day from code_changes c join issues i on i.key = c.issue_key where i.project_key = $1
		union
		select day from eval_days where project_key = $1 and eval_id = any($3::text[])
		order by 1`, p.Key, p.DefaultRepo, codeEvalIDs(p.Key))
	if err != nil {
		return nil, fmt.Errorf("read the days of %s: %w", p.Key, err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Time, error) {
		var day time.Time
		err := row.Scan(&day)
		return day.UTC(), err
	})
	if err != nil {
		return nil, fmt.Errorf("read the days of %s: %w", p.Key, err)
	}
	return days, nil
}

// projectRows is p's day rows of the code evals on days, built from the stored results
// (fold.BuildDays), in each cut it holds: its own (repository ""), its default repository's, and
// each other repository that holds a change that counts for it.
//
// A change counts for p as R11 says (fold.CountsFor): a change of an issue of p's, in any
// repository, and a change with no issue in p's default repository. p's own day D is as its
// default repository's day D is, measured, marked or blocked (a day of no state there has no row,
// whatever another repository holds); a project with no default repository, or one the App cannot
// read, has a row of D only where a repository holds a change that counts for it on D. A day
// whose changes include one of a repository's marked day is marked, so a window that holds it
// reads "no data" naming it (R8). A repository's cut holds that repository's changes and its own
// state of each day: every decided day of the default repository, and each day of another
// repository that holds a change that counts for p. A project whose default repository the App
// cannot read has repo_unreadable on every row (R11).
func projectRows(ctx context.Context, db Querier, p project, days []time.Time) ([]evals.DayRow, error) {
	changes, err := changesFor(ctx, db, p, days)
	if err != nil {
		return nil, err
	}
	pairs, err := pairsOf(ctx, db, changes)
	if err != nil {
		return nil, err
	}
	repos := map[string]bool{}
	if p.DefaultRepo != "" {
		repos[p.DefaultRepo] = true
	}
	for _, change := range changes {
		repos[change.Repo] = true
	}
	states, err := dayStates(ctx, db, slices.Sorted(maps.Keys(repos)), days)
	if err != nil {
		return nil, err
	}
	unreadable := false
	if p.DefaultRepo != "" {
		repo, found, err := GetRepo(ctx, db, p.DefaultRepo)
		if err != nil {
			return nil, err
		}
		unreadable = found && repo.State == RepoUnreadable
	}
	// held is each repository's days that hold a change that counts for p.
	held := map[string]map[int64]bool{}
	for _, change := range changes {
		if held[change.Repo] == nil {
			held[change.Repo] = map[int64]bool{}
		}
		held[change.Repo][dayKey(change.Day)] = true
	}
	groups := delivery.SurvivalGroups(p.Groups)
	cut := func(repo string, decided []fold.Day, cutChanges []fold.Change, cutPairs []fold.Pair) []evals.DayRow {
		survivalParts, reworkParts := fold.BuildDays(fold.BuildInput{
			Project: fold.Project{Key: p.Key, DefaultRepo: p.DefaultRepo}, Groups: groups, RepoUnreadable: unreadable,
			Days: decided, Changes: cutChanges, Pairs: cutPairs,
		})
		return evals.CodeDayRows(p.Key, repo, survivalParts, reworkParts)
	}

	var own []fold.Day
	for _, day := range days {
		key := dayKey(day)
		var seen []fold.Day
		if p.DefaultRepo != "" {
			if state, found := states[p.DefaultRepo][key]; found {
				seen = append(seen, state)
			} else if !unreadable {
				continue
			}
		}
		for repo := range held {
			if repo != p.DefaultRepo && held[repo][key] {
				if state, found := states[repo][key]; found {
					seen = append(seen, state)
				}
			}
		}
		if len(seen) == 0 {
			continue
		}
		decided := fold.Day{Day: day, State: survival.DayMeasured}
		for _, state := range seen {
			switch {
			case state.State == survival.DayMarked:
				decided = fold.Day{Day: day, State: survival.DayMarked}
			case state.State == survival.DayBlocked && decided.State == survival.DayMeasured:
				decided = state
			}
		}
		own = append(own, decided)
	}
	rows := cut("", own, changes, pairs)
	for _, repo := range slices.Sorted(maps.Keys(repos)) {
		var decided []fold.Day
		for _, day := range days {
			state, found := states[repo][dayKey(day)]
			if found && (repo == p.DefaultRepo || held[repo][dayKey(day)]) {
				decided = append(decided, state)
			}
		}
		if len(decided) == 0 {
			continue
		}
		var repoChanges []fold.Change
		for _, change := range changes {
			if change.Repo == repo {
				repoChanges = append(repoChanges, change)
			}
		}
		var repoPairs []fold.Pair
		for _, pair := range pairs {
			if pair.Repo == repo {
				repoPairs = append(repoPairs, pair)
			}
		}
		rows = append(rows, cut(repo, decided, repoChanges, repoPairs)...)
	}
	return rows, nil
}

// changesFor is every stored change on days that counts for p (R11): each change of an issue of
// p's, in any repository, and each change with no issue in p's default repository, each with the
// project of its issue, in the order the commits were made.
func changesFor(ctx context.Context, db Querier, p project, days []time.Time) ([]fold.Change, error) {
	rows, err := db.Query(ctx, `
		select c.repo, coalesce(i.project_key, ''), c.sha, c.day, c.committed_at, c.pr_number, c.head_ref, c.author,
			coalesce(c.issue_key, ''), c.added, c.alive, c.removed_by_non_pr
		from code_changes c left join issues i on i.key = c.issue_key
		where c.day = any($3::date[])
		  and ((c.issue_key is not null and i.project_key = $1) or (c.issue_key is null and c.repo = $2))
		order by c.committed_at, c.repo, c.sha`, p.Key, p.DefaultRepo, days)
	if err != nil {
		return nil, fmt.Errorf("read the changes of %s: %w", p.Key, err)
	}
	changes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fold.Change, error) {
		var change fold.Change
		err := row.Scan(&change.Repo, &change.IssueProject, &change.SHA, &change.Day, &change.CommittedAt, &change.PR,
			&change.HeadRef, &change.Author, &change.IssueKey, &change.Added, &change.AliveAtMark, &change.RemovedByNonPR)
		change.Day, change.CommittedAt = change.Day.UTC(), change.CommittedAt.UTC()
		return change, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the changes of %s: %w", p.Key, err)
	}
	return changes, nil
}

// pairsOf is every stored pair whose adder is one of changes.
func pairsOf(ctx context.Context, db Querier, changes []fold.Change) ([]fold.Pair, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	repos, shas := make([]string, len(changes)), make([]string, len(changes))
	for index, change := range changes {
		repos[index], shas[index] = change.Repo, change.SHA
	}
	rows, err := db.Query(ctx, `
		select p.repo, p.adder_sha, p.remover_sha, p.remover_pr, p.remover_head_ref, p.remover_author,
			coalesce(p.remover_issue_key, ''), p.lines, p.days_between
		from code_pairs p join unnest($1::text[], $2::text[]) as a(repo, sha) on p.repo = a.repo and p.adder_sha = a.sha
		order by p.repo, p.adder_sha, p.remover_sha`, repos, shas)
	if err != nil {
		return nil, fmt.Errorf("read the pairs of the changes: %w", err)
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fold.Pair, error) {
		var pair fold.Pair
		err := row.Scan(&pair.Repo, &pair.AdderSHA, &pair.RemoverSHA, &pair.Remover.PR, &pair.Remover.HeadRef, &pair.Remover.Author,
			&pair.Remover.IssueKey, &pair.Lines, &pair.DaysBetween)
		return pair, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the pairs of the changes: %w", err)
	}
	return pairs, nil
}

// dayStates is the state of each decided day (measured, marked or blocked) of repos on days, by
// repository and day.
func dayStates(ctx context.Context, db Querier, repos []string, days []time.Time) (map[string]map[int64]fold.Day, error) {
	states := map[string]map[int64]fold.Day{}
	if len(repos) == 0 {
		return states, nil
	}
	rows, err := db.Query(ctx, `
		select repo, day, state, coalesce(blocked_files, 0) from code_days
		where repo = any($1::text[]) and day = any($2::date[]) and state <> 'replayed'`, repos, days)
	if err != nil {
		return nil, fmt.Errorf("read the day states of %v: %w", repos, err)
	}
	var (
		repo  string
		day   fold.Day
		state string
	)
	if _, err := pgx.ForEachRow(rows, []any{&repo, &day.Day, &state, &day.BlockedFiles}, func() error {
		if states[repo] == nil {
			states[repo] = map[int64]fold.Day{}
		}
		day.Day, day.State = day.Day.UTC(), survival.DayState(state)
		states[repo][dayKey(day.Day)] = day
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the day states of %v: %w", repos, err)
	}
	return states, nil
}

// WriteMarks marks days of repo, each a UTC midnight, at at, in one transaction of its own (R8):
// MarkDays marks each of them that is measured, and the day rows of every project repo's results
// on the days it marked count for are rebuilt, under build, in the way a marked day has them: the
// status row alone, with marked 1 and the day's first_marked_day, the headline and group rows of
// the day deleted, and the stored figures of those days folded again (evals.WriteCodeDaysTx). Once
// the transaction commits, its eval_days changes are moved into the change log
// (delivery.DrainAfterCommit), so both read paths, the list read's stored figures and the items
// route, read "no data" naming the day as soon as the mark stands, whatever stops the run after
// it. It reads no patch and rebuilds nothing else, and answers the days it marked.
func WriteMarks(ctx context.Context, pool *store.Pool, repo string, days []time.Time, at time.Time, build string) ([]time.Time, error) {
	repo = strings.ToLower(repo)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the marks of %s: %w", repo, err)
	}
	defer tx.Rollback(ctx)
	keys, err := projectsOfRepo(ctx, tx, repo, days)
	if err != nil {
		return nil, err
	}
	b, _, err := prepare(ctx, tx, keys, build, at, nil)
	if err != nil {
		return nil, err
	}
	marked, err := MarkDays(ctx, tx, repo, days, at)
	if err != nil {
		return nil, err
	}
	if len(marked) > 0 {
		if err := b.write(ctx, marked); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the marks of %s: %w", repo, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "codesurvival: the marks' day rows could not move their changes into the delivery change log; the next drain moves them")
	return marked, nil
}
