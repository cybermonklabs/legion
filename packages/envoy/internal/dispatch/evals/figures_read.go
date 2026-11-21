package evals

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The read of the stored figures (figures.go): the eval list's read of an offered range answers
// each eval from its stored rows, by lookup, where the day rows' read (DayWindow) would fold every
// row of the window and the compare range again. k8 measured the first paint of 84 days with a
// compare range on the farm's slowest node: 95.6 ms of database time over the day rows, 12.2 ms
// over the stored rows, with the answers equal byte for byte.

// FiguresOffered reports whether the stored figures can answer a read of window and compare (nil
// for none) of repos: window and compare each whole UTC days of one of FigureLengths, and at most
// one repository, whose rows a stored row of that repository folds (none: the project's own). A
// read of another length, or of two repositories, whose fold merges several repositories' rows
// that no stored row holds, folds the day rows.
func FiguresOffered(repos []string, window Range, compare *Range) bool {
	if len(repos) > 1 || !figureRange(window) {
		return false
	}
	return compare == nil || figureRange(*compare)
}

// figureRange reports whether r is whole UTC days of one of FigureLengths.
func figureRange(r Range) bool {
	if !r.From.Equal(measures.Midnight(r.From)) || !r.To.Equal(measures.Midnight(r.To)) {
		return false
	}
	return slices.Contains(FigureLengths, rangeDays(r))
}

// rangeDays is how many whole days r spans.
func rangeDays(r Range) int {
	return int(r.To.Sub(r.From) / (24 * time.Hour))
}

// storedRange is a stored range row as the read reads it: its fold key, its figures and the oldest
// updated_at of the rows it folds.
type storedRange struct {
	key     int64
	figures []byte
	oldest  time.Time
}

// storedDay is a stored day row as the read reads it: its day, its fold key and its figures.
type storedDay struct {
	day     time.Time
	key     int64
	figures []byte
}

// StoredFigures is what a read of the stored figures read for one window, compare range and
// repository (ReadStoredFigures): how far each project's figures are built, each eval's stored row
// of the window and of the compare range, and each eval's stored day rows of the window.
type StoredFigures struct {
	built           map[string]time.Time
	window, compare map[string]storedRange
	days            map[string][]storedDay
}

// ReadStoredFigures reads the stored figures a read of window and compare (nil for none) of repo
// (the empty string for the projects' own rows) folds for entries, evals of projects, where the
// delivery settings name deployRepo ("" where none are set): of an eval keyed by the deploy
// repository the fold of that repository's rows, and of every other the fold of the rows of none
// (DeployRepoOf). It reads side by side: the range rows of the window and of the compare range,
// the `union all` of two ranges of eval_range_figures' primary key; and how far each project's
// figures are built (FiguresThrough) and, one after the other in the same read, the day rows of
// the window of the evals with a headline series, one range of eval_day_figures' primary key for
// each project. The read is FiguresOffered's to make.
func (s *Store) ReadStoredFigures(ctx context.Context, projects []string, entries []Entry, deployRepo, repo string, window Range, compare *Range) (StoredFigures, error) {
	read := StoredFigures{window: map[string]storedRange{}, compare: map[string]storedRange{}, days: map[string][]storedDay{}}
	evalIDs := make([]string, len(entries))
	var headlined []string
	for index, entry := range entries {
		evalIDs[index] = entry.Definition.EvalID
		if _, headline := listedAndHeadline(entry); len(headline) > 0 {
			headlined = append(headlined, entry.Definition.EvalID)
		}
	}
	keyed := keyedEvals(entries)
	ranged := `select days, end_day, eval_id, fold_key, figures, oldest_updated_at from eval_range_figures
		where project_key = any($1::text[]) and repo = $2 and eval_id = any($3::text[]) and days = $%d and end_day = $%d::date
			and ` + deployRepoCase("deploy_repo", "$4", "$5")
	query := fmt.Sprintf(ranged, 6, 7)
	args := []any{projects, repo, evalIDs, keyed, deployRepo, rangeDays(window), window.To}
	if compare != nil {
		query += "\n\t\tunion all\n\t\t" + fmt.Sprintf(ranged, 8, 9)
		args = append(args, rangeDays(*compare), compare.To)
	}
	err := store.ReadSideBySide(ctx,
		func(ctx context.Context) error {
			rows, err := s.database.Pool.Query(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("read the stored range figures of %v: %w", projects, err)
			}
			defer rows.Close()
			for rows.Next() {
				var days int32
				var end time.Time
				var evalID string
				var row storedRange
				if err := rows.Scan(&days, &end, &evalID, &row.key, &row.figures, &row.oldest); err != nil {
					return fmt.Errorf("read the stored range figures of %v: %w", projects, err)
				}
				end, row.oldest = end.UTC(), row.oldest.UTC()
				if int(days) == rangeDays(window) && end.Equal(window.To) {
					read.window[evalID] = row
				}
				if compare != nil && int(days) == rangeDays(*compare) && end.Equal(compare.To) {
					read.compare[evalID] = row
				}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read the stored range figures of %v: %w", projects, err)
			}
			return nil
		},
		func(ctx context.Context) error {
			built, err := s.FiguresThrough(ctx, projects)
			if err != nil {
				return err
			}
			read.built = built
			if len(headlined) == 0 {
				return nil
			}
			rows, err := s.database.Pool.Query(ctx, `
				select eval_id, day, fold_key, figures from eval_day_figures
				where project_key = any($1::text[]) and repo = $2 and day >= $3::date and day < $4::date and eval_id = any($5::text[])
					and `+deployRepoCase("deploy_repo", "$6", "$7")+`
			`, projects, repo, window.From, window.To, headlined, keyed, deployRepo)
			if err != nil {
				return fmt.Errorf("read the stored day figures of %v: %w", projects, err)
			}
			defer rows.Close()
			for rows.Next() {
				var evalID string
				var row storedDay
				if err := rows.Scan(&evalID, &row.day, &row.key, &row.figures); err != nil {
					return fmt.Errorf("read the stored day figures of %v: %w", projects, err)
				}
				row.day = row.day.UTC()
				read.days[evalID] = append(read.days[evalID], row)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read the stored day figures of %v: %w", projects, err)
			}
			return nil
		})
	if err != nil {
		return StoredFigures{}, err
	}
	return read, nil
}

// SeriesValues is SeriesValuesOf over the stored figures: entry's listed series with their values
// over window and compare (nil for none), read against firsts, each series' first day, the answer
// SeriesValuesOf gives over the day rows, bit for bit, each statistic a lookup; and the oldest
// updated_at of the rows the stored rows it read fold, nil where it read none. A range of which the
// eval has no stored row holds none of its rows, and reads the fold of no parts, as the day rows'
// read folds none. ok is false where the stored figures cannot answer the eval, which the caller
// answers from its day rows: its project's figures are not built through the end of both ranges
// (FiguresThrough), a stored row it would read was folded under another fold key than the eval's
// definition now has (a register write the refresher has not folded yet), or one does not decode.
func (f StoredFigures) SeriesValues(entry Entry, firsts map[string]time.Time, window Range, compare *Range) ([]SeriesValues, *time.Time, bool) {
	definition := entry.Definition
	through, built := f.built[definition.ProjectKey]
	if !built || through.Before(window.To) || compare != nil && through.Before(compare.To) {
		return nil, nil, false
	}
	listed, _ := listedAndHeadline(entry)
	key := foldKeyOf(entry)
	var oldest *time.Time
	fold := func(rows map[string]storedRange, r Range) (map[string]Statistics, map[string]int, bool) {
		row, held := rows[definition.EvalID]
		if !held {
			return FoldStatistics(definition, listed, nil, r), nil, true
		}
		if row.key != key {
			return nil, nil, false
		}
		folded, via, err := decodeFigures(row.figures)
		if err != nil {
			return nil, nil, false
		}
		if oldest == nil || row.oldest.Before(*oldest) {
			oldest = &row.oldest
		}
		return folded, via, true
	}
	windowed, via, ok := fold(f.window, window)
	if !ok {
		return nil, nil, false
	}
	var compared map[string]Statistics
	if compare != nil {
		if compared, _, ok = fold(f.compare, *compare); !ok {
			return nil, nil, false
		}
	}
	stored := slices.SortedFunc(slices.Values(f.days[definition.EvalID]), func(a, b storedDay) int { return a.day.Compare(b.day) })
	daily := make([]dayFold, len(stored))
	for index, day := range stored {
		if day.key != key {
			return nil, nil, false
		}
		folded, _, err := decodeFigures(day.figures)
		if err != nil {
			return nil, nil, false
		}
		daily[index] = dayFold{day: day.day, folded: folded}
	}
	return valuesOf(listed, windowed, via, compared, daily, firsts, window, compare), oldest, true
}
