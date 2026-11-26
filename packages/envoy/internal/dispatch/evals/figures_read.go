package evals

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The read of the stored figures (figures.go): the eval list's read of an offered range answers
// each eval from its stored rows, by lookup, where the day rows' read (DayWindowIn) would fold every
// row of the window and the compare range again. k8 measured the first paint of 84 days with a
// compare range on the farm's slowest node: 95.6 ms of database time over the day rows, 12.2 ms
// over the stored rows, with the answers equal byte for byte.

// FiguresOffered reports whether the stored figures can answer a read of window and compare (nil
// for none) of repos: window and compare each whole UTC days of one of FigureLengths, and at most
// one repository named, whose cut a stored row folds (none: the project's own rows). A read of
// another length, or of two or more repositories, whose fold merges several cuts' rows that no
// stored row holds, folds the day rows.
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

// StoredFigures is what a read of the stored figures read for one window, compare range and cut
// (ReadStoredFiguresIn): the evals the stored figures of the cut serve over both ranges, each
// eval's stored row of the window and of the compare range, and each eval's stored day rows of the
// window.
type StoredFigures struct {
	served          map[string]bool
	window, compare map[string]storedRange
	days            map[string][]storedDay
}

// ReadStoredFiguresIn reads in q the stored figures a read of window and compare (nil for none) of
// repos folds for entries, evals of projects, where the rows are recorded under deployRepo (""
// where none is set; DeployRepoIn): of an eval keyed by the deploy repository the fold of that
// repository's rows, and of every other the fold of the rows of none (DeployRepoOf). Its cut is the
// one repository repos names, or the projects' own rows ("") where it names none (figures.go's
// cuts): the read is FiguresOffered's to make, which offers none of two or more. It reads, one
// after the other: how far each project's figures are built, which of its evals a stale mark of
// the cut names and what the mark's generation has folded again of them (figuresThrough),
// answering at once where no eval is left whose project's figures are built through the end of
// both ranges and which the stored rows of the cut this read reads answer (builtCut.serves); then
// of those evals alone, which it serves, the range rows of the window and of the compare range, the
// `union all` of two ranges of eval_range_figures' primary key, and the day rows of the window of
// those with a headline series, one range of eval_day_figures' primary key for each project. Every
// other eval the caller folds from the day rows: those of a project not built yet, and those whose
// rows in the cut a stale mark names (a move of a default repository, a readings write past the
// days and series it folds itself, inlineFoldDaySeries, and a register write that moves an eval's
// fold key, each mark written in that write's own transaction), until the refresher or the boot
// has folded again the stored rows this read reads of them: a first paint's from its first-paint
// batch on, and those of an older window and compare range once the refold, newest end day first,
// has come down past them. A refresher pass rewrites the stored rows and the marks in one
// transaction, so in one answer's snapshot (BeginSnapshot), as the eval list reads them, an eval's
// window figures are the fold of the days read beside them, and an eval read as served is one no
// mark names since the figures it reads were folded; on the pool each statement reads the rows that
// stand when it runs.
func ReadStoredFiguresIn(ctx context.Context, q delivery.Querier, projects []string, entries []Entry, deployRepo string, repos []string, window Range, compare *Range) (StoredFigures, error) {
	repo := ""
	if len(repos) == 1 {
		repo = repos[0]
	}
	built, err := figuresThrough(ctx, q, projects, repo)
	if err != nil {
		return StoredFigures{}, err
	}
	read := StoredFigures{served: map[string]bool{}, window: map[string]storedRange{}, compare: map[string]storedRange{}, days: map[string][]storedDay{}}
	var served []Entry
	var servedProjects, evalIDs, headlined []string
	for _, entry := range entries {
		definition := entry.Definition
		cut, held := built[definition.ProjectKey]
		if !held || cut.through.Before(window.To) || compare != nil && cut.through.Before(compare.To) || !cut.serves(definition.EvalID, window, compare) {
			continue
		}
		read.served[definition.EvalID] = true
		served, evalIDs = append(served, entry), append(evalIDs, definition.EvalID)
		if !slices.Contains(servedProjects, definition.ProjectKey) {
			servedProjects = append(servedProjects, definition.ProjectKey)
		}
		if _, headline := listedAndHeadline(entry); len(headline) > 0 {
			headlined = append(headlined, definition.EvalID)
		}
	}
	if len(served) == 0 {
		return read, nil
	}
	keyed := keyedEvals(served)
	ranged := `select days, end_day, eval_id, fold_key, figures, oldest_updated_at from eval_range_figures
		where project_key = any($1::text[]) and repo = $2 and eval_id = any($3::text[]) and days = $%d and end_day = $%d::date
			and ` + deployRepoCase("deploy_repo", "$4", "$5")
	query := fmt.Sprintf(ranged, 6, 7)
	args := []any{servedProjects, repo, evalIDs, keyed, deployRepo, rangeDays(window), window.To}
	if compare != nil {
		query += "\n\t\tunion all\n\t\t" + fmt.Sprintf(ranged, 8, 9)
		args = append(args, rangeDays(*compare), compare.To)
	}
	ranges, err := q.Query(ctx, query, args...)
	if err != nil {
		return StoredFigures{}, fmt.Errorf("read the stored range figures of %v: %w", servedProjects, err)
	}
	defer ranges.Close()
	for ranges.Next() {
		var days int32
		var end time.Time
		var evalID string
		var row storedRange
		if err := ranges.Scan(&days, &end, &evalID, &row.key, &row.figures, &row.oldest); err != nil {
			return StoredFigures{}, fmt.Errorf("read the stored range figures of %v: %w", servedProjects, err)
		}
		end, row.oldest = end.UTC(), row.oldest.UTC()
		if int(days) == rangeDays(window) && end.Equal(window.To) {
			read.window[evalID] = row
		}
		if compare != nil && int(days) == rangeDays(*compare) && end.Equal(compare.To) {
			read.compare[evalID] = row
		}
	}
	if err := ranges.Err(); err != nil {
		return StoredFigures{}, fmt.Errorf("read the stored range figures of %v: %w", servedProjects, err)
	}
	if len(headlined) == 0 {
		return read, nil
	}
	days, err := q.Query(ctx, `
		select eval_id, day, fold_key, figures from eval_day_figures
		where project_key = any($1::text[]) and repo = $2 and day >= $3::date and day < $4::date and eval_id = any($5::text[])
			and `+deployRepoCase("deploy_repo", "$6", "$7")+`
	`, servedProjects, repo, window.From, window.To, headlined, keyed, deployRepo)
	if err != nil {
		return StoredFigures{}, fmt.Errorf("read the stored day figures of %v: %w", servedProjects, err)
	}
	defer days.Close()
	for days.Next() {
		var evalID string
		var row storedDay
		if err := days.Scan(&evalID, &row.day, &row.key, &row.figures); err != nil {
			return StoredFigures{}, fmt.Errorf("read the stored day figures of %v: %w", servedProjects, err)
		}
		row.day = row.day.UTC()
		read.days[evalID] = append(read.days[evalID], row)
	}
	if err := days.Err(); err != nil {
		return StoredFigures{}, fmt.Errorf("read the stored day figures of %v: %w", servedProjects, err)
	}
	return read, nil
}

// SeriesValues is SeriesValuesOf over the stored figures: entry's listed series with their values
// over window and compare (nil for none), the ranges the read read, read against firsts, each
// series' first day, the answer SeriesValuesOf gives over the day rows, bit for bit, each
// statistic a lookup; and the oldest updated_at of the rows the stored rows it read fold, nil where
// it read none. A range of which the eval has no stored row reads the fold of no parts, as the day
// rows' read folds none where the range holds none of the eval's listed rows, which every write
// that folds the rows it moves keeps true; a register write that lists or unlists a series folds
// none of its rows (relistDays) and marks stale, in its own transaction, every cut holding a row of
// the eval, so this read does not serve it (below) and the caller folds the day rows until the
// refresher's refold has passed the cut. ok is
// false where the stored figures cannot answer the eval, which the caller answers from its day
// rows: the read did not serve it (its project's figures are not built through the end of both
// ranges, or a stale mark of its cut names it and its generation has not folded again the stored
// rows the read reads; builtCut.serves), a stored row it would read was folded under another fold
// key than the eval's definition now has (a register write the refresher has not folded yet), or
// one does not decode.
func (f StoredFigures) SeriesValues(entry Entry, firsts map[string]time.Time, window Range, compare *Range) ([]SeriesValues, *time.Time, bool) {
	definition := entry.Definition
	if !f.served[definition.EvalID] {
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
