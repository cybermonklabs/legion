package evals

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// defaults.go publishes the projects' default repositories (eval_days_defaults, migration
// 20261117100006), the selection every read of a project's day rows makes among the owner keys'
// rows (owners.go): a person's write of a project's default repository
// (PUT /api/v1/projects/{key}/default-repository, projects.default_repo) moves no read and no view
// until the refresher's page that reads it publishes it, and that page's commit makes visible, with
// the new defaults, the stored figures of the projects' own rows it moves folded under them, and
// the marks of the stored cuts of repositories it moves, each naming the evals whose rows it moved
// there (figures.go). So a read in one snapshot answers the old defaults whole, their figures among
// them, or the new ones whole: a read that names no repository from figures folded under them, and
// a read that names a repository whose cut the move marked from the day rows of the evals the mark
// names until they are folded again (refoldStaleCuts), and from the stored figures of every other
// eval of the cut, whose rows the move did not move. A default moves more than its own project's
// reads: a pull request whose issue is held through a project's default repository (a condition of
// its attribution list, delivery_days.go) counts for that project while its default is the pull
// request's repository and for the repository's other readers otherwise, so the move moves what
// each of them reads. The move rewrites no day row and no member: every owner key's rows are
// written whatever the defaults are, its zero rows from its own first input day on, and the
// published defaults decide who reads them (eval_project_owners).

// defaultsMove is what a page publishes of the projects' default repositories (syncDefaults):
// every project's default repository as it stands (live, by project), the projects whose published
// default it moves (moved, in key order), and the project whose published default is the deploy
// repository (judging, the delivery views' judge, DeployRepoEvals) before and after it, "" for none.
type defaultsMove struct {
	live              map[string]string
	moved             []string
	deployRepo        string
	judgingWas, judge string
}

// syncDefaults reads the projects' default repositories and the published ones, and where they
// differ records in w what the page's commit publishes (publishDefaults). One statement, which
// reads every project's row: a few dozen at most.
func (r *refresh) syncDefaults(ctx context.Context, w *pageWork) error {
	pool := r.store.database.Pool
	live, published := map[string]string{}, map[string]string{}
	rows, err := pool.Query(ctx, `
		select 'live', key, default_repo from projects where default_repo is not null
		union all
		select 'published', project_key, repo from eval_days_defaults
	`)
	if err != nil {
		return fmt.Errorf("read the projects' default repositories: %w", err)
	}
	var side, project, repo string
	if _, err := pgx.ForEachRow(rows, []any{&side, &project, &repo}, func() error {
		if side == "live" {
			live[project] = repo
		} else {
			published[project] = repo
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read the projects' default repositories: %w", err)
	}
	var moved []string
	for _, project := range slices.Sorted(maps.Keys(live)) {
		if was, held := published[project]; !held || was != live[project] {
			moved = append(moved, project)
		}
	}
	for _, project := range slices.Sorted(maps.Keys(published)) {
		if _, held := live[project]; !held {
			moved = append(moved, project)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	slices.Sort(moved)
	deployRepo, err := DeployRepoIn(ctx, pool)
	if err != nil {
		return err
	}
	w.defaults = &defaultsMove{
		live: live, moved: moved, deployRepo: deployRepo,
		judgingWas: judgingProject(published, deployRepo), judge: judgingProject(live, deployRepo),
	}
	return nil
}

// judgingProject is the one project of defaults (default repository by project) whose default is
// deployRepo, compared without case as DeployRepoEvals compares it, "" where none or more than one
// is, or no deploy repository is set.
func judgingProject(defaults map[string]string, deployRepo string) string {
	var judges []string
	for project, repo := range defaults {
		if deployRepo != "" && strings.EqualFold(repo, deployRepo) {
			judges = append(judges, project)
		}
	}
	if len(judges) != 1 {
		return ""
	}
	return judges[0]
}

// publishDefaults publishes, in tx, the page's commit, the default repositories move holds: every
// project's published default replaced by the one it stands at; then, for each project the move
// moves, the stored figures of its own rows folded again over every day of its delivery evals
// (refoldProject, figureScope.own), so they fold the rows it reads under the new defaults, and one
// eval_days change of no eval (delivery.EvalDaysWrittenByDefaults), which marks its evals views;
// each cut of a repository the move moves rows into or out of marked stale, naming the evals whose
// rows it moved (staleMarksOf, markStaleCuts), their refold left to the refresher
// (refoldStaleCuts); and, where the project judging the delivery views moved, one more change
// (delivery.JudgingMoved), which marks those views. The projects it moves are those whose own
// default it moves, and every project whose owner keys (eval_project_owners, with whether it reads
// each through the repository) the new defaults move, read in tx before and after the write: a
// default move moves the readers of every key a list of which holds a condition of the moved
// project, whatever their own defaults. A day whose rows stand and whose readers' via moved folds
// again too, since its stamp holds each row's via_repository as the project reads it (rowsStamp).
// The cuts it moves of a project are those of its old and its new published default, whose cut
// holds its own rows stored under legionfacts.DefaultRepo (cutOf), where its own default moves,
// and that of the repository of each owner key it gains, loses or reads otherwise through the
// repository (noteMovedCuts); it folds none of them, since at INFRA's size the two defaults' cuts
// hold about 290,000 range rows, past what a commit folds inside R14's 2 s. Of those two cuts the
// marks name the evals that hold its rows under legionfacts.DefaultRepo, 6 of INFRA's 40, and those
// of the owner keys of each repository it gains or loses, and every other eval's stored figures of
// them stand, read as they are meanwhile. Every write of the commit lands together, so a read
// answers the old defaults and figures or the new ones. The refresher's pass whose page commits
// the move folds the marked evals of the stale cuts again once its pages are done, batch after
// batch, each in a short transaction of its own (refoldStaleCuts).
func publishDefaults(ctx context.Context, tx pgx.Tx, build string, move *defaultsMove) error {
	projects := slices.AppendSeq(make([]string, 0, len(move.live)), maps.Keys(move.live))
	slices.Sort(projects)
	repos := make([]string, len(projects))
	for i, project := range projects {
		repos[i] = move.live[project]
	}
	was, err := readPublishedDefaults(ctx, tx)
	if err != nil {
		return err
	}
	before, err := readOwnerPairs(ctx, tx)
	if err != nil {
		return err
	}
	// projects is never nil, so a move that clears the last default deletes every published one:
	// a nil slice is sent as null, `project_key = any(null)` is null, and the delete took none.
	if _, err := tx.Exec(ctx, `delete from eval_days_defaults where not (project_key = any($1))`, projects); err != nil {
		return fmt.Errorf("publish the default repositories: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into eval_days_defaults (project_key, repo)
		select given.project_key, given.repo from unnest($1::text[], $2::text[]) as given(project_key, repo)
		join projects p on p.key = given.project_key
		order by given.project_key collate "C"
		on conflict (project_key) do update set repo = excluded.repo where eval_days_defaults.repo is distinct from excluded.repo
	`, projects, repos); err != nil {
		return fmt.Errorf("publish the default repositories: %w", err)
	}
	after, err := readOwnerPairs(ctx, tx)
	if err != nil {
		return err
	}
	moved := slices.Clone(move.moved)
	for project := range maps.Keys(before) {
		if !maps.Equal(before[project], after[project]) {
			moved = append(moved, project)
		}
	}
	for project := range maps.Keys(after) {
		if _, held := before[project]; !held {
			moved = append(moved, project)
		}
	}
	slices.Sort(moved)
	cuts := map[staleCut]*movedCut{}
	for _, project := range slices.Compact(moved) {
		if _, _, err := refoldProject(ctx, tx, build, project, true, slugSpans(project, deliveryDaySlugs, everyDay)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select delivery_change_log_pending($1, $2, $3, null, null, null)`,
			delivery.ChangeKindEvalDays, delivery.EvalDaysWrittenByDefaults, project); err != nil {
			return fmt.Errorf("log the default repository %s publishes: %w", project, err)
		}
		noteMovedCuts(cuts, project, was[project], move.live[project], before[project], after[project])
	}
	marks, err := staleMarksOf(ctx, tx, cuts)
	if err != nil {
		return err
	}
	if err := markStaleCuts(ctx, tx, marks); err != nil {
		return err
	}
	if move.judgingWas != move.judge {
		if _, err := tx.Exec(ctx, `select delivery_change_log_pending($1, $2, $3, $4, null, null)`,
			delivery.ChangeKindEvalDays, delivery.EvalDaysWrittenByDefaults, move.deployRepo, delivery.DefaultsJudgingRef); err != nil {
			return fmt.Errorf("log the project judging the delivery views: %w", err)
		}
	}
	return nil
}

// movedCut is what a move of the default repositories moves of one cut of a project's stored
// figures (noteMovedCuts): the project's own rows stored under legionfacts.DefaultRepo, where its
// published default moves to or from the cut's repository (ownDefault), and the rows of each owner
// key whose reading by the project it moves (owners).
type movedCut struct {
	ownDefault bool
	owners     map[string]bool
}

// noteMovedCuts records in cuts the cuts of project's stored figures a move of the default
// repositories moves (publishDefaults), and what it moves of each, its published default moving
// from was to now ("" for none) and the owner keys it reads, with whether through the repository,
// from before to after: the cuts of was and of now where they differ, which hold its own rows
// stored under legionfacts.DefaultRepo (cutOf), and the cut of the repository of each owner key it
// gains, loses or reads otherwise, whose rows of that repository the cut folds (an owner key's rows
// are of its own repository, OwnerKey). No cut of the project's own rows is among them: the move's
// commit folds those itself.
func noteMovedCuts(cuts map[staleCut]*movedCut, project, was, now string, before, after map[string]bool) {
	cut := func(repo string) *movedCut {
		key := staleCut{project: project, repo: repo}
		if cuts[key] == nil {
			cuts[key] = &movedCut{owners: map[string]bool{}}
		}
		return cuts[key]
	}
	if was != now {
		for _, repo := range []string{was, now} {
			if repo != "" {
				cut(repo).ownDefault = true
			}
		}
	}
	for _, pairs := range []map[string]bool{before, after} {
		for owner := range pairs {
			viaBefore, readBefore := before[owner]
			viaAfter, readAfter := after[owner]
			if readBefore && readAfter && viaBefore == viaAfter {
				continue
			}
			if repo, _, _, ok := parseOwner(owner); ok {
				cut(repo).owners[owner] = true
			}
		}
	}
}

// staleMarksOf reads in tx, the commit of the move, the evals whose rows the move moves in each
// of cuts (noteMovedCuts), from the rows stored as it commits, so an eval whatever its name or
// kind is named once it holds such a row, a new eval among them: of a cut the project's published
// default moves to or from, each of the project's evals that holds a listed row under
// legionfacts.DefaultRepo (DefaultRowEvalsQuery); and of a cut whose owner keys it moves, each of
// the project's evals of a slug a listed row of one of those keys holds (ownerEvalsQuery, asReader).
// It answers each cut's ids in byte order, and leaves out a cut that holds no row the move moved,
// whose stored figures stand. The reads are bounded by eval_days' indexes, inside R14's 2 s: the
// first one range of eval_days_window for each project whose default moves, every listed row it
// holds under legionfacts.DefaultRepo (at INFRA's size, 21,993 rows of 6 of its 40 evals); the
// second a walk of each owner key's eval ids through eval_days_stamps, one probe for each eval of
// it and one past the last, in statements of at most statementBytes.
func staleMarksOf(ctx context.Context, tx pgx.Tx, cuts map[staleCut]*movedCut) (map[staleCut][]string, error) {
	var projects, owners []string
	for cut, moved := range cuts {
		if moved.ownDefault && !slices.Contains(projects, cut.project) {
			projects = append(projects, cut.project)
		}
		for owner := range moved.owners {
			if !slices.Contains(owners, owner) {
				owners = append(owners, owner)
			}
		}
	}
	defaultRows := map[string][]string{}
	if len(projects) > 0 {
		rows, err := tx.Query(ctx, DefaultRowEvalsQuery, projects)
		if err != nil {
			return nil, fmt.Errorf("read the evals of %v that hold rows under %s: %w", projects, legionfacts.DefaultRepo, err)
		}
		var project, evalID string
		if _, err := pgx.ForEachRow(rows, []any{&project, &evalID}, func() error {
			defaultRows[project] = append(defaultRows[project], evalID)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the evals of %v that hold rows under %s: %w", projects, legionfacts.DefaultRepo, err)
		}
	}
	ownerRows := map[string][]string{}
	slices.Sort(owners)
	for chunk := range statementChunks(owners, paramBytes(1), textBytes) {
		rows, err := tx.Query(ctx, ownerEvalsQuery, chunk)
		if err != nil {
			return nil, fmt.Errorf("read the evals of %d owner keys a move of the default repositories moves: %w", len(chunk), err)
		}
		var owner, evalID string
		if _, err := pgx.ForEachRow(rows, []any{&owner, &evalID}, func() error {
			ownerRows[owner] = append(ownerRows[owner], evalID)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the evals of %d owner keys a move of the default repositories moves: %w", len(chunk), err)
		}
	}
	marks := map[staleCut][]string{}
	for cut, moved := range cuts {
		evals := map[string]bool{}
		if moved.ownDefault {
			for _, evalID := range defaultRows[cut.project] {
				evals[evalID] = true
			}
		}
		for owner := range moved.owners {
			for _, evalID := range ownerRows[owner] {
				evals[asReader(owner, cut.project, evalID)] = true
			}
		}
		if len(evals) > 0 {
			marks[cut] = slices.Sorted(maps.Keys(evals))
		}
	}
	return marks, nil
}

// DefaultRowEvalsQuery reads the evals of each project $1 names that hold a listed row stored
// under legionfacts.DefaultRepo, each row a project and one of its evals: the evals a move of the
// project's published default names in the cuts of the two defaults (staleMarksOf), and the bench's
// k16 fallback marks its default's cut with, as a move of that default marks it. One range of
// eval_days_window (project_key, repo, day) for each project, bounded by its key and that
// repository, every row of which it reads.
const DefaultRowEvalsQuery = `
	select distinct d.project_key, d.eval_id from eval_days d
	where d.project_key = any($1::text[]) and d.repo = '` + legionfacts.DefaultRepo + `' and d.listed
`

// ownerEvalsQuery reads the evals of each owner key $1 names that hold a listed row (staleMarksOf):
// for each key, a walk of its eval ids through eval_days_stamps (project_key, eval_id, writer, day),
// each probe the first listed row of the key after the eval before, so one probe for each eval of
// the key and one past its last, rather than every row of it.
const ownerEvalsQuery = `
	select o.owner, held.eval_id
	from unnest($1::text[]) as o(owner)
	cross join lateral (
		with recursive held(eval_id) as (
			(select e.eval_id from eval_days e where e.project_key = o.owner and e.listed order by e.eval_id limit 1)
			union all
			select (select e.eval_id from eval_days e where e.project_key = o.owner and e.listed and e.eval_id > held.eval_id order by e.eval_id limit 1)
			from held where held.eval_id is not null
		)
		select eval_id from held where eval_id is not null
	) held
`

// readPublishedDefaults reads in q every project's published default repository, by project: a
// project that publishes none is in none.
func readPublishedDefaults(ctx context.Context, q delivery.Querier) (map[string]string, error) {
	rows, err := q.Query(ctx, `select project_key, repo from eval_days_defaults`)
	if err != nil {
		return nil, fmt.Errorf("read the published default repositories: %w", err)
	}
	published := map[string]string{}
	var project, repo string
	if _, err := pgx.ForEachRow(rows, []any{&project, &repo}, func() error {
		published[project] = repo
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the published default repositories: %w", err)
	}
	return published, nil
}

// readOwnerPairs reads in q the owner keys each project reads under the published defaults, with
// whether it reads each through the repository (eval_project_owners, less each project's own key),
// by project: a project that reads none is in none.
func readOwnerPairs(ctx context.Context, q delivery.Querier) (map[string]map[string]bool, error) {
	rows, err := q.Query(ctx, `select project_key, owner, via from eval_project_owners where owner <> project_key`)
	if err != nil {
		return nil, fmt.Errorf("read the owner keys each project reads: %w", err)
	}
	pairs := map[string]map[string]bool{}
	var project, owner string
	var via bool
	if _, err := pgx.ForEachRow(rows, []any{&project, &owner, &via}, func() error {
		if pairs[project] == nil {
			pairs[project] = map[string]bool{}
		}
		pairs[project][owner] = via
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the owner keys each project reads: %w", err)
	}
	return pairs, nil
}
