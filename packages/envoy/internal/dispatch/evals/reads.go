package evals

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
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

// Current reads one page of a project's evals through evals_current, ordered by slug, with each
// eval's current series: every eval of the project, or those of one kind where kind is not "".
func (s *Store) Current(ctx context.Context, project, kind string, limit, offset int) (Page[Entry], error) {
	if err := checkPage(limit, offset); err != nil {
		return Page[Entry]{}, err
	}
	filter, args := `project_key = $1`, []any{project}
	if kind != "" {
		filter, args = filter+` and kind = $2`, append(args, kind)
	}
	var total int
	if err := s.database.Pool.QueryRow(ctx, `select count(*) from evals_current where `+filter, args...).Scan(&total); err != nil {
		return Page[Entry]{}, fmt.Errorf("count the evals of %s: %w", project, err)
	}
	limitAt, offsetAt := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := s.database.Pool.Query(ctx, `
		select `+definitionColumns+` from evals_current where `+filter+`
		order by slug limit $`+limitAt+` offset $`+offsetAt, append(args, limit, offset)...)
	if err != nil {
		return Page[Entry]{}, fmt.Errorf("read the evals of %s: %w", project, err)
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
	if err != nil {
		return Page[Entry]{}, fmt.Errorf("read the evals of %s: %w", project, err)
	}
	entries := make([]Entry, len(definitions))
	ids := make([]string, len(definitions))
	position := make(map[string]int, len(definitions))
	for index, definition := range definitions {
		entries[index] = Entry{Definition: definition, Series: []Series{}}
		ids[index] = definition.EvalID
		position[definition.EvalID] = index
	}
	if len(ids) > 0 {
		rows, err := s.database.Pool.Query(ctx, `
			select `+seriesColumns+` from eval_series_current where eval_id = any($1)
			order by eval_id, is_primary desc, series
		`, ids)
		if err != nil {
			return Page[Entry]{}, fmt.Errorf("read the series of the evals of %s: %w", project, err)
		}
		series, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Series, error) { return scanSeries(row) })
		if err != nil {
			return Page[Entry]{}, fmt.Errorf("read the series of the evals of %s: %w", project, err)
		}
		for _, one := range series {
			entry := &entries[position[one.EvalID]]
			entry.Series = append(entry.Series, one)
		}
	}
	return Page[Entry]{Items: entries, Total: total}, nil
}

// Versions reads every version of one eval's definition, newest first; none for an eval no
// version holds.
func (s *Store) Versions(ctx context.Context, evalID string) ([]Definition, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select `+definitionColumns+` from eval_versions where eval_id = $1 order by version desc
	`, evalID)
	if err != nil {
		return nil, fmt.Errorf("read the versions of %s: %w", evalID, err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
	if err != nil {
		return nil, fmt.Errorf("read the versions of %s: %w", evalID, err)
	}
	return versions, nil
}

// DayWindow reads a project's listed day rows for a window of days and, where compare is not
// nil, for a compare range too, as one statement: the `union all` of one read of each range,
// each an index range of eval_days_window, so the compare range is a second windowed read of the
// same rows and never one read with two windows (R2, R12). Each row says which range it answers
// for (RangeWindow, RangeCompare). A range's days are the UTC dates d with From's date <= d <
// To's date.
//
// repos empty reads the project's own rows (repo is the empty string); repos named reads those
// repositories' rows, a repository filter's fold. evalIDs empty reads every eval of the project;
// named, only those evals' rows.
func (s *Store) DayWindow(ctx context.Context, project string, evalIDs, repos []string, window Range, compare *Range) ([]DayRow, error) {
	if len(repos) == 0 {
		repos = []string{""}
	}
	var evals []string
	if len(evalIDs) > 0 {
		evals = evalIDs
	}
	ranged := func(name string, from, to int) string {
		return `select '` + name + `', ` + dayColumns + ` from eval_days
			where project_key = $1 and repo = any($2) and listed
			  and day >= $` + strconv.Itoa(from) + `::date and day < $` + strconv.Itoa(to) + `::date
			  and ($3::text[] is null or eval_id = any($3))`
	}
	query := ranged(RangeWindow, 4, 5)
	args := []any{project, repos, evals, window.From.UTC(), window.To.UTC()}
	if compare != nil {
		query += "\n\t\tunion all\n\t\t" + ranged(RangeCompare, 6, 7)
		args = append(args, compare.From.UTC(), compare.To.UTC())
	}
	rows, err := s.database.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %s: %w", project, err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayRow, error) {
		var name string
		day, err := scanDay(row, &name)
		day.Range = name
		return day, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the day rows of %s: %w", project, err)
	}
	return days, nil
}

// Items reads one page of a logged eval's standing readings (superseded_at is null) measured in
// window, newest first, through eval_readings_row: those of one series, row kind and row where
// each is not "". A filter left "" is left out of the statement rather than written as a test
// that the parameter is empty, so the plan Postgres caches for the statement keeps the row
// index's columns that the read names.
func (s *Store) Items(ctx context.Context, evalID, series, rowKind, row string, window Range, limit, offset int) (Page[Reading], error) {
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
	var total int
	if err := s.database.Pool.QueryRow(ctx, `select count(*) from eval_readings where `+filter, args...).Scan(&total); err != nil {
		return Page[Reading]{}, fmt.Errorf("count the readings of %s: %w", evalID, err)
	}
	limitAt, offsetAt := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := s.database.Pool.Query(ctx, `
		select `+readingColumns+` from eval_readings where `+filter+`
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
