package evals

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// Entry is one eval as the registry read answers it: its newest definition and the newest
// version of each of its series, the primary series first, then by name.
type Entry struct {
	Definition Definition `json:"definition"`
	Series     []Series   `json:"series"`
}

// Page is one page of a read: its items, and how many the read's filter holds in all.
type Page[T any] struct {
	Items []T
	Total int
}

// Range is a span of time, From inclusive and To exclusive.
type Range struct {
	From time.Time
	To   time.Time
}

// Projects is ProjectsIn on the pool.
func (s *Store) Projects(ctx context.Context) ([]string, error) {
	return ProjectsIn(ctx, s.database.Pool)
}

// ProjectsIn is every project's key, in key order, read in q: the projects the refresher walks and
// seeds on its first pass, and the ones a cross-project read reads where its caller names none, in
// that read's one snapshot.
func ProjectsIn(ctx context.Context, q delivery.Querier) ([]string, error) {
	rows, err := q.Query(ctx, `select key from projects order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	return keys, nil
}

// Current is CurrentIn in a snapshot of its own (BeginSnapshot).
func (s *Store) Current(ctx context.Context, projects []string, kind string, limit, offset int) (Page[Entry], error) {
	tx, err := s.BeginSnapshot(ctx)
	if err != nil {
		return Page[Entry]{}, fmt.Errorf("begin the read of the evals of %v: %w", projects, err)
	}
	defer tx.Rollback(ctx)
	return CurrentIn(ctx, tx, projects, kind, limit, offset)
}

// CurrentIn reads in tx one page of the evals of projects through evals_current, ordered by
// project in projects' order and then by slug byte by byte (`collate "C"`), so the order and each
// page of it are the same whatever collation the database was made with (en_US or ICU puts
// production-deploy before pr-open-to-merge), with each eval's current series: every eval of those
// projects, or those of one kind where kind is not "". The page runs across the projects: Total is
// every project's count, and offset counts from the first project's first eval. The count,
// page and series all read tx's one snapshot (BeginSnapshot). A concurrent registration could
// otherwise return a new eval under the total counted before it.
func CurrentIn(ctx context.Context, tx pgx.Tx, projects []string, kind string, limit, offset int) (Page[Entry], error) {
	if err := checkPage(limit, offset); err != nil {
		return Page[Entry]{}, err
	}
	filter, args := `project_key = any($1::text[])`, []any{projects}
	if kind != "" {
		filter, args = filter+` and kind = $2`, append(args, kind)
	}
	var total int
	if err := tx.QueryRow(ctx, `select count(*) from evals_current where `+filter, args...).Scan(&total); err != nil {
		return Page[Entry]{}, fmt.Errorf("count the evals of %v: %w", projects, err)
	}
	limitAt, offsetAt := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := tx.Query(ctx, `
		select `+definitionColumns+` from evals_current where `+filter+`
		order by array_position($1::text[], project_key), slug collate "C" limit $`+limitAt+` offset $`+offsetAt, append(args, limit, offset)...)
	if err != nil {
		return Page[Entry]{}, fmt.Errorf("read the evals of %v: %w", projects, err)
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
	if err != nil {
		return Page[Entry]{}, fmt.Errorf("read the evals of %v: %w", projects, err)
	}
	entries, err := entriesOf(ctx, tx, definitions, fmt.Sprint(projects))
	if err != nil {
		return Page[Entry]{}, err
	}
	return Page[Entry]{Items: entries, Total: total}, nil
}

// entriesOf is definitions, each as an entry with the newest version of each of its series, the
// primary series first and then by name, read in q (eval_series_current): the part of a read of
// evals that Current and readEntries share. what names the evals in an error.
// The series read sends eval IDs in chunks at the statement byte bound.
func entriesOf(ctx context.Context, q delivery.Querier, definitions []Definition, what string) ([]Entry, error) {
	entries := make([]Entry, len(definitions))
	ids := make([]string, len(definitions))
	position := make(map[string]int, len(definitions))
	for index, definition := range definitions {
		entries[index] = Entry{Definition: definition, Series: []Series{}}
		ids[index] = definition.EvalID
		position[definition.EvalID] = index
	}
	for chunk := range statementChunks(ids, paramBytes(1), textBytes) {
		rows, err := q.Query(ctx, `
			select `+seriesColumns+` from eval_series_current where eval_id = any($1)
			order by eval_id, is_primary desc, series
		`, chunk)
		if err != nil {
			return nil, fmt.Errorf("read the series of the evals of %s: %w", what, err)
		}
		series, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Series, error) { return scanSeries(row) })
		if err != nil {
			return nil, fmt.Errorf("read the series of the evals of %s: %w", what, err)
		}
		for _, one := range series {
			entry := &entries[position[one.EvalID]]
			entry.Series = append(entry.Series, one)
		}
	}
	return entries, nil
}

// EvalVersionsIn reads in tx one eval as EvalIn does and one page of the versions of its definition,
// newest first, with how many versions it holds, all in tx's one snapshot (BeginSnapshot), as
// CurrentIn reads its statements: a register write committing between the reads would otherwise
// answer a newest version that is not the definition read, which the eval's read states the
// window's values for. The count and the page are one round trip, and the page reads the versions
// it answers alone. found is false where no version holds evalID.
func EvalVersionsIn(ctx context.Context, tx pgx.Tx, evalID string, limit, offset int) (Entry, Page[Definition], bool, error) {
	if err := checkPage(limit, offset); err != nil {
		return Entry{}, Page[Definition]{}, false, err
	}
	entry, found, err := EvalIn(ctx, tx, evalID)
	if err != nil || !found {
		return Entry{}, Page[Definition]{}, found, err
	}
	versions := Page[Definition]{Items: []Definition{}}
	batch := &pgx.Batch{}
	batch.Queue(`select count(*) from eval_versions where eval_id = $1`, evalID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&versions.Total)
	})
	batch.Queue(`
		select `+definitionColumns+` from eval_versions where eval_id = $1 order by version desc limit $2 offset $3
	`, evalID, limit, offset).Query(func(rows pgx.Rows) error {
		page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
		versions.Items = append(versions.Items, page...)
		return err
	})
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return Entry{}, Page[Definition]{}, false, fmt.Errorf("read the versions of %s: %w", evalID, err)
	}
	return entry, versions, true, nil
}

// BeginSnapshot begins a read-only repeatable-read transaction, whose statements read one
// snapshot: the one database moment of an answer whose every read runs in it (ProjectsIn,
// CurrentIn, EvalVersionsIn, EvalIn, FirstDaysIn, ReadStoredFiguresIn, DayWindowIn, EvalDaysIn,
// CountedIn, ItemsIn, RefresherAgeIn, DeployRepoIn, BuiltSettingsIn, and the delivery and Legion
// reads an items answer makes in it). Its caller commits it once those reads are done.
func (s *Store) BeginSnapshot(ctx context.Context) (pgx.Tx, error) {
	return s.database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
}

// EvalIn reads in tx one eval as CurrentIn reads each: its newest definition, through
// evals_current, and the newest version of each of its series, the primary series first, then by
// name, the two in tx's one snapshot (BeginSnapshot), as an answer reads its eval in the answer's
// one snapshot. found is false where no version holds evalID.
func EvalIn(ctx context.Context, tx pgx.Tx, evalID string) (Entry, bool, error) {
	definition, err := scanDefinition(tx.QueryRow(ctx, `
		select `+definitionColumns+` from evals_current where eval_id = $1
	`, evalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("read eval %s: %w", evalID, err)
	}
	rows, err := tx.Query(ctx, `
		select `+seriesColumns+` from eval_series_current where eval_id = $1 order by is_primary desc, series
	`, evalID)
	if err != nil {
		return Entry{}, false, fmt.Errorf("read the series of %s: %w", evalID, err)
	}
	series, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Series, error) { return scanSeries(row) })
	if err != nil {
		return Entry{}, false, fmt.Errorf("read the series of %s: %w", evalID, err)
	}
	return Entry{Definition: definition, Series: nonNil(series)}, true, nil
}

// FirstDaysIn reads in q, for each series of entries' evals that holds a day row of all its
// project's repositories (repo the empty string), the first day it holds one, by eval id and then
// by series: the day its input begins. From that day on a series has a row of every day the
// refresher has written, an explicit zero row on a day with no input, so a range that starts
// before it reads "no data" and names it (R12). A project's rows are its own and those of the
// owner keys it reads under the published defaults (eval_project_owners, owners.go), whose
// first day is the first of any of them: each owner writes its zero rows from its own first day,
// so the project holds a row of every day from the first on. Each series is one probe of
// eval_days' primary key for each owner, its first day, under its own eval's slug and among its
// own writer's rows (Definition.writer) of the deploy repository its read reads where the rows are
// recorded under deployRepo ("" where none is set; DeployRepoIn, DeployRepoOf), so the evals of
// many projects are one read, a row the other writer made moves no eval's first day, and an eval
// that reads the deploy repository begins on that repository's own first day.
// A repository filter still names the project's first day. A repository day without a row
// reads no data, not zero.
func FirstDaysIn(ctx context.Context, q delivery.Querier, entries []Entry, deployRepo string) (map[string]map[string]time.Time, error) {
	firsts := map[string]map[string]time.Time{}
	if len(entries) == 0 {
		return firsts, nil
	}
	projects, evalIDs, writers := make([]string, len(entries)), make([]string, len(entries)), make([]string, len(entries))
	deployRepos := make([]string, len(entries))
	for index, entry := range entries {
		projects[index], evalIDs[index], writers[index] = entry.Definition.ProjectKey, entry.Definition.EvalID, entry.Definition.writer()
		deployRepos[index] = DeployRepoOf(entry.Definition, deployRepo)
	}
	rows, err := q.Query(ctx, `
		select given.eval_id, s.series, min(first.day)
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as given(project_key, eval_id, writer, deploy_repo)
		cross join lateral (select series from eval_series_current s where s.eval_id = given.eval_id) s
		join eval_project_owners o on o.project_key = given.project_key
		cross join lateral (
			select d.day from eval_days d
			where d.project_key = o.owner and d.eval_id = `+evalIDAs("o.owner", "given.eval_id", "given.project_key")+`
			  and d.writer = given.writer and d.deploy_repo = given.deploy_repo and d.series = s.series and d.repo = ''
			order by d.day
			limit 1
		) first
		group by given.eval_id, s.series
	`, projects, evalIDs, writers, deployRepos)
	if err != nil {
		return nil, fmt.Errorf("read the first days of %d evals: %w", len(evalIDs), err)
	}
	defer rows.Close()
	for rows.Next() {
		var evalID, series string
		var day time.Time
		if err := rows.Scan(&evalID, &series, &day); err != nil {
			return nil, fmt.Errorf("read the first days of %d evals: %w", len(evalIDs), err)
		}
		if firsts[evalID] == nil {
			firsts[evalID] = map[string]time.Time{}
		}
		firsts[evalID][series] = day.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the first days of %d evals: %w", len(evalIDs), err)
	}
	return firsts, nil
}

// Built is how far the stored rows of each family of computed evals are whole through, as one
// answer reads them (BuiltIn), and that answer's UTC day, today: the rule each read of a computed
// eval's window and compare range states its values by (Withhold), until a day row is built its
// windows reading "no data" (R12). A walk builds its family's days in day order, each day of every
// key a project reads in one commit that moves the family's serving record (eval_days_serving)
// past it (refresh.walkWrite), and a rebuild's flip moves the record in the commit that makes its
// rows visible, never back, so a family a rebuild rebuilds keeps serving. A day on or after the
// record is one the stored rows are not whole through: a family's first walk has not reached it,
// or a walk a stopped server left behind has not caught up to it, and a window that holds it would
// fold the days the walk has built as if they were all of the window's. A family no walk has
// built a day of and no rebuild has flipped is whole through no day.
type Built struct {
	today time.Time
	next  map[walkKey]time.Time
}

// builtWalk is the walk whose serving record says how far definition's day rows are whole through:
// the delivery walk's for a computed delivery or CI eval (deliveryDaySlugs), its project's Legion
// walk's for a computed Legion eval (legionDaySlugs), and false for every other eval. No walk
// builds the rows of those: a logged eval's readings write writes its rows, and the zero rows they
// leave owed through today, in its own transaction (writeLoggedZeros), and the daily fill writes
// each new day's zero row in the pass the UTC midnight starts; unowned-p0's rows are written for
// that day alone (WriteUnownedP0Day); change-fail-rate and failed-deploy-recovery get
// no rows at all. So no day of theirs waits for a later write to build it from input it holds.
func builtWalk(definition Definition) (walkKey, bool) {
	switch {
	case !definition.Computed():
		return walkKey{}, false
	case slices.Contains(deliveryDaySlugs, definition.Slug):
		return walkKey{family: walkDelivery}, true
	case slices.Contains(legionDaySlugs, definition.Slug):
		return walkKey{family: walkLegion, project: definition.ProjectKey}, true
	}
	return walkKey{}, false
}

// BuiltIn reads in q the serving records (eval_days_serving) of the families whose rows entries'
// computed evals fold (builtWalk), as of now's UTC day: the delivery rows' record and each Legion
// project's, in one statement by the table's key, which holds a row for each family a walk or a
// rebuild has built; and none where no entry is such an eval. It runs in the snapshot the
// answer's rows are read in (BeginSnapshot), so the records and the rows are one moment.
func BuiltIn(ctx context.Context, q delivery.Querier, entries []Entry, now time.Time) (Built, error) {
	built := Built{today: measures.Midnight(now), next: map[walkKey]time.Time{}}
	walked := false
	var legion []string
	for _, entry := range entries {
		key, held := builtWalk(entry.Definition)
		walked = walked || held
		if held && key.family == walkLegion && !slices.Contains(legion, key.project) {
			legion = append(legion, key.project)
		}
	}
	if !walked {
		return built, nil
	}
	rows, err := q.Query(ctx, `
		select family, project_key, next_day from eval_days_serving
		where (family = $1 and project_key = '') or (family = $2 and project_key = any($3::text[]))
	`, walkDelivery, walkLegion, legion)
	if err != nil {
		return Built{}, fmt.Errorf("read how far the day rows of %d evals are built: %w", len(entries), err)
	}
	var key walkKey
	var next time.Time
	if _, err := pgx.ForEachRow(rows, []any{&key.family, &key.project, &next}, func() error {
		built.next[key] = measures.Midnight(next)
		return nil
	}); err != nil {
		return Built{}, fmt.Errorf("read how far the day rows of %d evals are built: %w", len(entries), err)
	}
	return built, nil
}

// unbuilt reports whether of holds a day before b's today that the rows of key's family are not
// whole through: a UTC date d, From's date <= d < To's date (DayWindowIn's days), before today and
// on or after the family's serving record, any day where it has none.
func (b Built) unbuilt(key walkKey, of Range) bool {
	from, to := measures.Midnight(of.From), measures.Midnight(of.To)
	if next := b.next[key]; next.After(from) {
		from = next
	}
	if b.today.Before(to) {
		to = b.today
	}
	return from.Before(to)
}

// StoredRepos resolves each requested repository, without case, to the distinct spellings in
// listed day rows of the projects' owners or among the projects' published defaults. A name with
// no match stays as given. It returns a sorted list, without duplicates. Call it once in the read's
// snapshot before selecting the cut: the figure, day and item reads can then compare keys exactly.
// With no repository filter there is nothing to resolve.
func StoredRepos(ctx context.Context, q delivery.Querier, projects, repos []string) ([]string, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	base, widest := paramBytes(2), 0
	for i := range repos {
		widest = max(widest, textBytes(&repos[i]))
	}
	matched, stored := make(map[string]bool, len(repos)), make(map[string]bool, len(repos))
	for keys := range statementChunks(projects, base+widest, textBytes) {
		bytes := base
		for i := range keys {
			bytes += textBytes(&keys[i])
		}
		for named := range statementChunks(repos, bytes, textBytes) {
			rows, err := q.Query(ctx, storedReposQuery, keys, named)
			if err != nil {
				return nil, fmt.Errorf("resolve repository spellings: %w", err)
			}
			var request, spelling string
			if _, err := pgx.ForEachRow(rows, []any{&request, &spelling}, func() error {
				matched[request], stored[spelling] = true, true
				return nil
			}); err != nil {
				return nil, fmt.Errorf("resolve repository spellings: %w", err)
			}
		}
	}
	for _, repo := range repos {
		if !matched[repo] {
			stored[repo] = true
		}
	}
	return slices.Sorted(maps.Keys(stored)), nil
}

// storedReposQuery walks the distinct repository keys of each owner through eval_days_window.
// Each probe stops at the first entry for the next key; no probe enumerates a repository's days.
// Comparing case only after that walk preserves the index bounds on every later read of the cut.
const storedReposQuery = `
	with owners as (
		select distinct owner from eval_project_owners where project_key = any($1::text[])
	), spellings as (
		select held.repo from owners o
		cross join lateral (
			with recursive held(repo) as (
				(select d.repo from eval_days d where d.project_key = o.owner and d.listed and d.repo > '' order by d.repo limit 1)
				union all
				select (select d.repo from eval_days d where d.project_key = o.owner and d.listed and d.repo > held.repo order by d.repo limit 1)
				from held where held.repo is not null
			)
			select held.repo from held where held.repo is not null
		) held
		union
		select repo from eval_project_repos where project_key = any($1)
	)
	select named.repo, spellings.repo
	from unnest($2::text[]) named(repo) join spellings on lower(spellings.repo) = lower(named.repo)
`

// EvalDaysIn reads in q every day row of definition's eval on the days of window, listed or not
// (pr-ci's per-job series are not), among its own writer's rows (Definition.writer) of the deploy
// repository its read reads where the rows are recorded under deployRepo ("" where none is set;
// DeployRepoIn, DeployRepoOf): the rows of all its project's repositories, or those of repos
// where repos names any, and of one series where series is not ""; newest day first, then by
// series and repository. A project's rows are its own and those of the owner keys it reads under
// the published defaults (eval_project_owners, owners.go), each read under the project's key and
// its eval id of the row's slug. Rows of one series, day and cut are added into one (addOwnedRows),
// including a Legion row read through the published default. Only the computed pr-ci job rows
// send samples, which the items route folds into job timings; day items do not display samples.
// Each owner's read is a range of eval_days' primary key, bounded by the owner keys (ownedKeys).
func EvalDaysIn(ctx context.Context, q delivery.Querier, definition Definition, deployRepo, series string, repos []string, window Range) ([]DayRow, error) {
	cut := `d.repo = any($5)`
	repo, samples := `d.repo`, `null::double precision[]`
	if len(repos) == 0 {
		repos = []string{""}
	} else {
		cut = repoCut("d", "$5", "$1")
		repo = cutOf("d", "$1")
	}
	if definition.Computed() && definition.Slug == SlugPRCI {
		samples = `case when starts_with(d.series, '` + measures.JobSeriesPrefix + `') then d.samples end`
	}
	filter := `o.project_key = $1 and ` + ownedKeys("d", "= $1") + ` and d.eval_id = o.owner || substr($2, length($1) + 1)
		and d.writer = $3 and d.deploy_repo = $4 and ` + cut + ` and d.day >= $6::date and d.day < $7::date`
	args := []any{definition.ProjectKey, definition.EvalID, definition.writer(), DeployRepoOf(definition, deployRepo), repos, window.From.UTC(), window.To.UTC()}
	if series != "" {
		args = append(args, series)
		filter += ` and d.series = $8`
	}
	rows, err := q.Query(ctx, `
		select `+ownedDayColumns(repo, samples)+` from eval_project_owners o join eval_days d on d.project_key = o.owner
		where `+filter+` order by d.day desc, d.series, cut_repo
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %s: %w", definition.EvalID, err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %s: %w", definition.EvalID, err)
	}
	return addOwnedRows(days), nil
}

// Counted is one item a family of a project's day rows counted (Member), with the day
// eval_day_members says it was last counted on.
type Counted struct {
	Member
	Day time.Time
}

// CountedIn reads in q the items family's day rows of project counted on the days of window,
// under deployRepo ("" for the items counted whatever the deploy repository is, a repository's
// name for those the evals that read it counted with it as the deploy repository; DeployRepoOf),
// those of repos where repos names any, by day and then by row key, through eval_day_members_day:
// the pull requests and deploy runs the delivery evals' rows of those days count, or the runs the
// CI evals' rows do, the project's own and those of the owner keys it reads under the published
// defaults (eval_project_owners, owners.go). An items read answers them, so it lists
// the items the window's numbers fold.
func CountedIn(ctx context.Context, q delivery.Querier, project, family, deployRepo string, repos []string, window Range) ([]Counted, error) {
	filter := ownedKeys("m", "= $1") + ` and m.family = $2 and m.deploy_repo = $3 and m.day >= $4::date and m.day < $5::date`
	args := []any{project, family, deployRepo, window.From.UTC(), window.To.UTC()}
	if len(repos) > 0 {
		args = append(args, repos)
		filter += ` and ` + repoCut("m", "$6", "$1")
	}
	rows, err := q.Query(ctx, `
		select m.row_key, m.repo, m.shipped_by, m.day
		from eval_project_owners o join eval_day_members m on m.project_key = o.owner
		where o.project_key = $1 and `+filter+` order by m.day, m.row_key
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the %s items of %s: %w", family, project, err)
	}
	defer rows.Close()
	var items []Counted
	for rows.Next() {
		item := Counted{Member: Member{Family: family, DeployRepo: deployRepo}}
		if err := rows.Scan(&item.RowKey, &item.Repo, &item.ShippedBy, &item.Day); err != nil {
			return nil, fmt.Errorf("read the %s items of %s: %w", family, project, err)
		}
		item.Day = item.Day.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the %s items of %s: %w", family, project, err)
	}
	return items, nil
}

// DayWindowIn reads in q the listed day rows of entries, evals of projects, for a window of days
// and, where compare is not nil, for a compare range too: the `union all` of one read of each range,
// each an index range of eval_days_window for each project, so the compare range is a second
// windowed read of the same rows and never one read with two windows (R2, R12), and the evals of
// many projects are one statement where its parameters fit in statementBytes (below, how it cuts
// them where they do not). Each row says which range it answers for (RangeWindow, RangeCompare). A
// range's days are the UTC dates d with From's date <= d < To's date. repos empty reads the
// projects' own rows (repo is the empty string); repos named reads those repositories' rows, a
// repository filter's fold. Each eval's rows are its own writer's (Definition.writer, R3): a
// computed eval's the computation's, a logged eval's the readings write's, so an eval whose source
// changed folds none of the rows the other writer made. And they are those of the deploy
// repository its read reads, where the rows are recorded under deployRepo ("" where none is set;
// DeployRepoIn, DeployRepoOf): of an eval keyed by the deploy repository that repository's rows,
// and of every other the rows of none, so a move of the deploy repository is this selection
// (deploy_repos.go). A project's rows are its own and those of the owner keys it reads under the
// published defaults, less those it shares (eval_project_owners, owners.go), each under the
// project's eval id of its slug and its via_repository as the project reads it (viaAsRead): a
// selection at read time too, which a move of a default repository rewrites no row for. A project
// and an owner key can each hold a row of one series and day, and the folds add the two as they
// add two days' (WindowRow is a part, not a day). Each index range is bounded by the keys the
// projects read (ownedKeys), and a read that names repositories reads, besides their rows, the
// projects' own rows stored under the Legion default where they publish one of them (repoCut).
//
// The statement is planned with its values each time it runs (pgx.QueryExecModeDescribeExec), as
// refs' read of several blocks is: a connection's prepared statement takes a generic plan from its
// sixth run where Postgres prices it no dearer, and that plan, which knows neither the projects nor
// the evals, read the 84-day repository read on the build farm in 183 to 227 ms where the plan
// with its values took 96 to 124 ms.
//
// The subquery that names each row's eval as the project reads it (owned) ends in `offset 0`, which
// keeps Postgres from pulling it up into the outer query: pulled up, the row's eval id, a string
// built per row, was built again for each filter and case that reads it. Measured on the pod at a
// tenth of INFRA's size (the 84-day repository read with a compare range, 18,257 rows, 9 runs
// each), the fence took the median execution from 77 to 78 ms to 64 to 67 ms, with the same plan.
//
// A row answers what the folds and the answer read and nothing more (WindowRow), since what the
// read costs is the bytes it sends: no project, listing or build, but the owner it is stored under
// and, in a read that names repositories, the repository it is stored under, which the folds order
// a day's rows by (sortWindowRows) and a rebuild's overlay of staged rows reads
// (readOverlayGroups); a read that names none reads the rows stored under "" alone, and sends no
// repository. And a sample list only for a series whose fold reads one (readsSamples), so a
// logged series folded by anything but a quantile, whose rows keep every value of their day, sends
// none. The write that stored a row's last value (last_recorded_at, last_reading), which the
// `last` fold breaks a tie of two rows' last_at by (aggStatistics), comes only with a row of a
// `last` series in a read that names more than one repository: only that read folds two rows of
// one series and day, which can tie, and the order the folds take its rows in (sortWindowRows)
// says nothing of which was written later. Every other row answers no write: null and the empty
// string.
//
// q is the pool; a transaction that writes the rows it reads, as the stored figures' writes fold
// them (figures.go), so a fold of the rows a write left reads them as a paint does; or the snapshot
// one answer of the eval list reads in (BeginSnapshot). The read names each eval and, for each
// series whose samples or last write it reads, the pair (eval, series), which at MaxEvalSeries
// series of the largest names is about 244 KB of one eval: so entries go in runs whose parameters
// aim for statementBytes (statementChunks, paramBytes and windowEntryBytes). An eval's rows all
// come from one run: when that eval alone plus the shared parameters exceeds the budget, it is
// sent whole in its own statement. For example, a maximal eval and 100 maximal repository names
// can take about 296 KB together. A read that fits sends one statement. Each
// statement reads q's snapshot: in the list read's snapshot, as in any transaction at repeatable
// read, every run reads the one moment the answer's other reads read, as one statement would; on
// the pool, or in a transaction at read committed, each reads the rows that stand when it runs.
func DayWindowIn(ctx context.Context, q delivery.Querier, projects []string, entries []Entry, deployRepo string, repos []string, window Range, compare *Range) ([]WindowRow, error) {
	cut, storedRepo, ownedRepo, scan := `d.repo = any($2)`, "", "", scanWindowRow
	if len(repos) == 0 {
		repos = []string{""}
	} else {
		cut, storedRepo, ownedRepo, scan = repoCut("d", "$2", "o.project_key"), ", repo", ", d.repo", scanRepoWindowRow
	}
	ranged := func(inWindow string, from, to int) string {
		return `select ` + inWindow + `, eval_id, series, day, n, sum, num, den, max,
				case when (eval_id, series) in (select * from unnest($4::text[], $5::text[])) then samples end,
				last_value, last_at,
				case when (eval_id, series) in (select * from unnest($7::text[], $8::text[])) then last_recorded_at end,
				case when (eval_id, series) in (select * from unnest($7::text[], $8::text[])) then coalesce(last_reading::text, '') else '' end,
				nullif(counts, '{}'), via_repository, updated_at, owner` + storedRepo + `
			from (
				select o.owner, o.project_key || substr(d.eval_id, length(o.owner) + 1) as eval_id, d.writer, d.deploy_repo, d.series, d.day,
					d.n, d.sum, d.num, d.den, d.max, d.samples, d.last_value, d.last_at, d.last_recorded_at, d.last_reading, d.counts,
					` + viaAsRead("d.via_repository") + ` as via_repository, d.updated_at` + ownedRepo + `
				from eval_project_owners o join eval_days d on d.project_key = o.owner
				where o.project_key = any($1::text[]) and ` + ownedKeys("d", "= any($1::text[])") + ` and d.listed
				  and ` + cut + `
				  and d.day >= $` + strconv.Itoa(from) + `::date and d.day < $` + strconv.Itoa(to) + `::date
				offset 0
			) owned
			where eval_id = any($3)
			  and writer = case when eval_id = any($6::text[]) then '` + writerComputation + `' else '` + writerReadings + `' end
			  and ` + deployRepoCase("deploy_repo", "$9", "$10")
	}
	query, times := ranged("true", 11, 12), 2
	if compare != nil {
		query, times = query+"\n\t\tunion all\n\t\t"+ranged("false", 13, 14), 4
	}
	// base is the statement's parameters with no eval: its nine arrays, the deploy repository, the
	// days of the ranges and the projects and repositories it reads.
	base := paramBytes(9, deployRepo) + (4+8)*times
	for _, names := range [][]string{projects, repos} {
		for _, name := range names {
			base += 4 + len(name)
		}
	}
	lastPairs := len(repos) > 1
	days := []WindowRow{}
	for run := range statementChunks(entries, base, func(entry *Entry) int { return windowEntryBytes(entry, lastPairs) }) {
		evalIDs := make([]string, len(run))
		var computedEvals, sampledEvals, sampledSeries, lastEvals, lastSeries []string
		for index, entry := range run {
			evalIDs[index] = entry.Definition.EvalID
			if entry.Definition.Computed() {
				computedEvals = append(computedEvals, entry.Definition.EvalID)
			}
			for _, series := range entry.Series {
				if readsSamples(entry.Definition, series) {
					sampledEvals, sampledSeries = append(sampledEvals, entry.Definition.EvalID), append(sampledSeries, series.Series)
				}
				if lastPairs && series.Agg == AggLast {
					lastEvals, lastSeries = append(lastEvals, entry.Definition.EvalID), append(lastSeries, series.Series)
				}
			}
		}
		args := []any{pgx.QueryExecModeDescribeExec, projects, repos, evalIDs, sampledEvals, sampledSeries, computedEvals, lastEvals, lastSeries,
			keyedEvals(run), deployRepo, window.From.UTC(), window.To.UTC()}
		if compare != nil {
			args = append(args, compare.From.UTC(), compare.To.UTC())
		}
		rows, err := q.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("read the day rows of %v: %w", projects, err)
		}
		if days, err = pgx.AppendRows(days, rows, scan); err != nil {
			return nil, fmt.Errorf("read the day rows of %v: %w", projects, err)
		}
	}
	return days, nil
}

// windowEntryBytes is the most bytes entry adds to the parameters of DayWindowIn's statement, as pgx
// encodes them in binary, 4 bytes of length before each element: its eval id in the evals' array,
// and in the computed and the keyed evals' where it is one of them, and its id and a series' name
// in the arrays of pairs for each series whose samples it reads and, where lastPairs (the read
// names more than one repository), for each `last` series, whose last write it reads.
func windowEntryBytes(entry *Entry, lastPairs bool) int {
	id := 4 + len(entry.Definition.EvalID)
	bytes := id
	if entry.Definition.Computed() {
		bytes += id
	}
	if KeyedByDeployRepo(entry.Definition) {
		bytes += id
	}
	for _, series := range entry.Series {
		pair := id + 4 + len(series.Series)
		if readsSamples(entry.Definition, series) {
			bytes += pair
		}
		if lastPairs && series.Agg == AggLast {
			bytes += pair
		}
	}
	return bytes
}

// Items is ItemsIn on the pool.
func (s *Store) Items(ctx context.Context, evalID, series, rowKind, row string, repos []string, window Range, limit, offset int) (Page[Reading], error) {
	return ItemsIn(ctx, s.database.Pool, evalID, series, rowKind, row, repos, window, limit, offset)
}

// ItemsIn reads in q one page of a logged eval's standing readings (superseded_at is null)
// measured in window, newest first, through eval_readings_row: those of one series, row kind and
// row where each is not "", and those whose attributes.repo is one of repos where repos names any.
// A filter left "" is left out of the statement rather than written as a test that the parameter
// is empty, so the plan Postgres caches for the statement keeps the row index's columns that the
// read names. Its count and its page are two statements, so an answer reads them in one snapshot.
func ItemsIn(ctx context.Context, q delivery.Querier, evalID, series, rowKind, row string, repos []string, window Range, limit, offset int) (Page[Reading], error) {
	if err := checkPage(limit, offset); err != nil {
		return Page[Reading]{}, err
	}
	filter := `eval_id = $1 and superseded_at is null and measured_at >= $2 and measured_at < $3`
	args := []any{evalID, window.From.UTC(), window.To.UTC()}
	for _, named := range []struct{ column, value string }{{"row_kind", rowKind}, {"row_key", row}, {"series", series}} {
		if named.value != "" {
			args = append(args, named.value)
			filter += ` and ` + named.column + ` = $` + strconv.Itoa(len(args))
		}
	}
	if len(repos) > 0 {
		args = append(args, repos)
		filter += ` and attributes ->> 'repo' = any($` + strconv.Itoa(len(args)) + `)`
	}
	var total int
	if err := q.QueryRow(ctx, `select count(*) from eval_readings where `+filter, args...).Scan(&total); err != nil {
		return Page[Reading]{}, fmt.Errorf("count the readings of %s: %w", evalID, err)
	}
	limitAt, offsetAt := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := q.Query(ctx, `
		select `+ReadingColumns+` from eval_readings where `+filter+`
		order by measured_at desc, id limit $`+limitAt+` offset $`+offsetAt, append(args, limit, offset)...)
	if err != nil {
		return Page[Reading]{}, fmt.Errorf("read the readings of %s: %w", evalID, err)
	}
	readings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Reading, error) { return scanReading(row) })
	if err != nil {
		return Page[Reading]{}, fmt.Errorf("read the readings of %s: %w", evalID, err)
	}
	return Page[Reading]{Items: readings, Total: total}, nil
}

func checkPage(limit, offset int) error {
	if limit < 1 || offset < 0 {
		return errors.New("evals: a page needs a limit of at least 1 and an offset of at least 0")
	}
	return nil
}
