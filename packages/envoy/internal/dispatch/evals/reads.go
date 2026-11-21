package evals

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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

// Projects is every project's key, in key order: the projects the refresher walks and seeds on
// its first pass, and the ones a cross-project read reads where its caller names none.
func (s *Store) Projects(ctx context.Context) ([]string, error) {
	rows, err := s.database.Pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	return keys, nil
}

// Current reads one page of the evals of projects through evals_current, ordered by project in
// projects' order and then by slug byte by byte (`collate "C"`), so the order and each page of it
// are the same whatever collation the database was made with (en_US or ICU puts production-deploy
// before pr-open-to-merge), with each eval's current series: every eval of those projects,
// or those of one kind where kind is not "". The page runs across the projects: Total is every
// project's count, and offset counts from the first project's first eval. Its three statements
// (the count, the page and the page's series) read one snapshot, in a read-only repeatable-read
// transaction: a write that commits between two of them, such as a reading that registers its
// eval, would otherwise answer a page holding the new eval under the total counted before it,
// which a live view's delta would send as it stands. Each statement reads every project at once,
// so a read of many projects costs what a read of one does.
func (s *Store) Current(ctx context.Context, projects []string, kind string, limit, offset int) (Page[Entry], error) {
	if err := checkPage(limit, offset); err != nil {
		return Page[Entry]{}, err
	}
	tx, err := s.beginSnapshot(ctx)
	if err != nil {
		return Page[Entry]{}, fmt.Errorf("begin the read of the evals of %v: %w", projects, err)
	}
	defer tx.Rollback(ctx)
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
func entriesOf(ctx context.Context, q delivery.Querier, definitions []Definition, what string) ([]Entry, error) {
	entries := make([]Entry, len(definitions))
	ids := make([]string, len(definitions))
	position := make(map[string]int, len(definitions))
	for index, definition := range definitions {
		entries[index] = Entry{Definition: definition, Series: []Series{}}
		ids[index] = definition.EvalID
		position[definition.EvalID] = index
	}
	if len(ids) == 0 {
		return entries, nil
	}
	rows, err := q.Query(ctx, `
		select `+seriesColumns+` from eval_series_current where eval_id = any($1)
		order by eval_id, is_primary desc, series
	`, ids)
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
	return entries, nil
}

// EvalVersions reads one eval as Eval does and one page of the versions of its definition, newest
// first, with how many versions it holds, in one read-only repeatable-read transaction, as
// Current reads its statements: a register write committing between the reads would otherwise
// answer a newest version that is not the definition read, which the eval's read states the
// window's values for. The count and the page are one round trip, and the page reads the
// versions it answers alone. found is false where no version holds evalID.
func (s *Store) EvalVersions(ctx context.Context, evalID string, limit, offset int) (Entry, Page[Definition], bool, error) {
	if err := checkPage(limit, offset); err != nil {
		return Entry{}, Page[Definition]{}, false, err
	}
	tx, err := s.beginSnapshot(ctx)
	if err != nil {
		return Entry{}, Page[Definition]{}, false, fmt.Errorf("begin the read of eval %s: %w", evalID, err)
	}
	defer tx.Rollback(ctx)
	entry, found, err := readEval(ctx, tx, evalID)
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

// Eval reads one eval as Current reads each: its newest definition, through evals_current, and
// the newest version of each of its series, the primary series first, then by name, the two in
// one snapshot. found is false where no version holds evalID.
func (s *Store) Eval(ctx context.Context, evalID string) (Entry, bool, error) {
	tx, err := s.beginSnapshot(ctx)
	if err != nil {
		return Entry{}, false, fmt.Errorf("begin the read of eval %s: %w", evalID, err)
	}
	defer tx.Rollback(ctx)
	return readEval(ctx, tx, evalID)
}

// beginSnapshot begins a read-only repeatable-read transaction, whose statements read one
// snapshot.
func (s *Store) beginSnapshot(ctx context.Context) (pgx.Tx, error) {
	return s.database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
}

// readEval is Eval's two reads, in tx.
func readEval(ctx context.Context, tx pgx.Tx, evalID string) (Entry, bool, error) {
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

// FirstDays reads, for each series of entries' evals that holds a day row of all its project's
// repositories (repo the empty string), the first day it holds one, by eval id and then by
// series: the day its input begins. From that day on a series has a row of every day the
// refresher has written, an explicit zero row on a day with no input, so a range that starts
// before it reads "no data" and names it (R12). Each series is one probe of eval_days' primary
// key, its first day, under its own eval's project and among its own writer's rows
// (Definition.writer) of the deploy repository its read reads where the delivery settings name
// deployRepo ("" where none are set; DeployRepoOf), so the evals of many projects are one read, a
// row the other writer made moves no eval's first day, and an eval that reads the deploy
// repository begins on that repository's own first day.
func (s *Store) FirstDays(ctx context.Context, entries []Entry, deployRepo string) (map[string]map[string]time.Time, error) {
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
	rows, err := s.database.Pool.Query(ctx, `
		select given.eval_id, s.series, first.day
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as given(project_key, eval_id, writer, deploy_repo)
		cross join lateral (select series from eval_series_current s where s.eval_id = given.eval_id) s
		cross join lateral (
			select d.day from eval_days d
			where d.project_key = given.project_key and d.eval_id = given.eval_id and d.writer = given.writer
			  and d.deploy_repo = given.deploy_repo and d.series = s.series and d.repo = ''
			order by d.day
			limit 1
		) first
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

// EvalDays reads every day row of definition's eval on the days of window, listed or not (pr-ci's
// per-job series are not), among its own writer's rows (Definition.writer) of the deploy
// repository its read reads where the delivery settings name deployRepo ("" where none are set;
// DeployRepoOf): the rows of all its project's repositories, or those of repos where repos names
// any, and of one series where series is not ""; newest day first, then by series and repository.
// It is the items route's read of an eval's day rows, one range of eval_days' primary key.
func (s *Store) EvalDays(ctx context.Context, definition Definition, deployRepo, series string, repos []string, window Range) ([]DayRow, error) {
	if len(repos) == 0 {
		repos = []string{""}
	}
	filter := `project_key = $1 and eval_id = $2 and writer = $3 and deploy_repo = $4 and repo = any($5) and day >= $6::date and day < $7::date`
	args := []any{definition.ProjectKey, definition.EvalID, definition.writer(), DeployRepoOf(definition, deployRepo), repos, window.From.UTC(), window.To.UTC()}
	if series != "" {
		args = append(args, series)
		filter += ` and series = $8`
	}
	rows, err := s.database.Pool.Query(ctx, `
		select `+dayColumns+` from eval_days where `+filter+` order by day desc, series, repo
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %s: %w", definition.EvalID, err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %s: %w", definition.EvalID, err)
	}
	return days, nil
}

// Counted is one item a family of a project's day rows counted (Member), with the day
// eval_day_members says it was last counted on.
type Counted struct {
	Member
	Day time.Time
}

// CountedIn reads the items family's day rows of project counted on the days of window, under
// deployRepo ("" for the items counted whatever the deploy repository is, a repository's name for
// those the evals that read it counted with it as the deploy repository; DeployRepoOf), those of
// repos where repos names any, by day and then by row key, through eval_day_members_day: the pull
// requests and deploy runs the delivery evals' rows of those days count, or the runs the CI evals'
// rows do. An items read answers them, so it lists the items the window's numbers fold.
func (s *Store) CountedIn(ctx context.Context, project, family, deployRepo string, repos []string, window Range) ([]Counted, error) {
	filter := `project_key = $1 and family = $2 and deploy_repo = $3 and day >= $4::date and day < $5::date`
	args := []any{project, family, deployRepo, window.From.UTC(), window.To.UTC()}
	if len(repos) > 0 {
		args = append(args, repos)
		filter += ` and repo = any($6)`
	}
	rows, err := s.database.Pool.Query(ctx, `
		select row_key, repo, shipped_by, day from eval_day_members where `+filter+` order by day, row_key
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

// DayWindow reads the listed day rows of entries, evals of projects, for a window of days and,
// where compare is not nil, for a compare range too, as one statement: the `union all` of one read
// of each range, each an index range of eval_days_window for each project, so the compare range is
// a second windowed read of the same rows and never one read with two windows (R2, R12), and the
// evals of many projects are one statement. Each row says which range it answers for
// (RangeWindow, RangeCompare). A range's days are the UTC dates d with From's date <= d < To's
// date. repos empty reads the projects' own rows (repo is the empty string); repos named reads
// those repositories' rows, a repository filter's fold. Each eval's rows are its own writer's
// (Definition.writer, R3): a computed eval's the computation's, a logged eval's the readings
// write's, so an eval whose source changed folds none of the rows the other writer made. And they
// are those of the deploy repository its read reads, where the delivery settings name deployRepo
// ("" where none are set; DeployRepoOf): of an eval keyed by the deploy repository that
// repository's rows, and of every other the rows of none, so a move of the deploy repository is
// this selection (deploy_repos.go).
//
// A row answers what the folds and the answer read and nothing more (WindowRow), since what the
// read costs is the bytes it sends: no project, repository, listing or build, and a sample list
// only for a series whose fold reads one (readsSamples), so a logged series folded by anything but
// a quantile, whose rows keep every value of their day, sends none. The write that stored a row's
// last value (last_recorded_at, last_reading), which the `last` fold breaks a tie of two rows'
// last_at by (aggStatistics), comes only with a row of a `last` series in a read that names more
// than one repository: only that read folds two rows of one series and day, which can tie, and
// it sets no order. Every other row answers no write: null and the empty string.
func (s *Store) DayWindow(ctx context.Context, projects []string, entries []Entry, deployRepo string, repos []string, window Range, compare *Range) ([]WindowRow, error) {
	return dayWindow(ctx, s.database.Pool, projects, entries, deployRepo, repos, window, compare)
}

// dayWindow is DayWindow's read in q: the pool, or a transaction that writes the rows it reads,
// as the stored figures' writes fold them (figures.go), so a fold of the rows a write left reads
// them as a paint does.
func dayWindow(ctx context.Context, q delivery.Querier, projects []string, entries []Entry, deployRepo string, repos []string, window Range, compare *Range) ([]WindowRow, error) {
	if len(repos) == 0 {
		repos = []string{""}
	}
	evalIDs := make([]string, len(entries))
	var computedEvals, sampledEvals, sampledSeries, lastEvals, lastSeries []string
	for index, entry := range entries {
		evalIDs[index] = entry.Definition.EvalID
		if entry.Definition.Computed() {
			computedEvals = append(computedEvals, entry.Definition.EvalID)
		}
		for _, series := range entry.Series {
			if readsSamples(entry.Definition, series) {
				sampledEvals, sampledSeries = append(sampledEvals, entry.Definition.EvalID), append(sampledSeries, series.Series)
			}
			if len(repos) > 1 && series.Agg == AggLast {
				lastEvals, lastSeries = append(lastEvals, entry.Definition.EvalID), append(lastSeries, series.Series)
			}
		}
	}
	ranged := func(inWindow string, from, to int) string {
		return `select ` + inWindow + `, eval_id, series, day, n, sum, num, den, max,
				case when (eval_id, series) in (select * from unnest($4::text[], $5::text[])) then samples end,
				last_value, last_at,
				case when (eval_id, series) in (select * from unnest($7::text[], $8::text[])) then last_recorded_at end,
				case when (eval_id, series) in (select * from unnest($7::text[], $8::text[])) then coalesce(last_reading::text, '') else '' end,
				nullif(counts, '{}'), via_repository, updated_at
			from eval_days
			where project_key = any($1::text[]) and repo = any($2) and listed and eval_id = any($3)
			  and writer = case when eval_id = any($6::text[]) then '` + writerComputation + `' else '` + writerReadings + `' end
			  and ` + deployRepoCase("deploy_repo", "$9", "$10") + `
			  and day >= $` + strconv.Itoa(from) + `::date and day < $` + strconv.Itoa(to) + `::date`
	}
	query := ranged("true", 11, 12)
	args := []any{projects, repos, evalIDs, sampledEvals, sampledSeries, computedEvals, lastEvals, lastSeries,
		keyedEvals(entries), deployRepo, window.From.UTC(), window.To.UTC()}
	if compare != nil {
		query += "\n\t\tunion all\n\t\t" + ranged("false", 13, 14)
		args = append(args, compare.From.UTC(), compare.To.UTC())
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %v: %w", projects, err)
	}
	days, err := pgx.CollectRows(rows, scanWindowRow)
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %v: %w", projects, err)
	}
	return days, nil
}

// Items reads one page of a logged eval's standing readings (superseded_at is null) measured in
// window, newest first, through eval_readings_row: those of one series, row kind and row where
// each is not "", and those whose attributes.repo is one of repos where repos names any. A filter
// left "" is left out of the statement rather than written as a test that the parameter is
// empty, so the plan Postgres caches for the statement keeps the row index's columns that the
// read names.
func (s *Store) Items(ctx context.Context, evalID, series, rowKind, row string, repos []string, window Range, limit, offset int) (Page[Reading], error) {
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
	if err := s.database.Pool.QueryRow(ctx, `select count(*) from eval_readings where `+filter, args...).Scan(&total); err != nil {
		return Page[Reading]{}, fmt.Errorf("count the readings of %s: %w", evalID, err)
	}
	limitAt, offsetAt := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := s.database.Pool.Query(ctx, `
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
