package evals

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The two code evals of dispatch://INFRA-3206: code survival at 30 days and post-merge rework at
// 30 days, which the daily run of codesurvival reads from GitHub's patches and keeps as counts.
// No refresher family builds their day rows. The run writes them in its own transaction, and so
// does the regroup that a save of a project's code groups wakes (WriteCodeDaysTx), as
// WriteUnownedP0Day writes unowned-p0's rows outside the families. So the refresher writes no
// zero row of them, no walk serves them (builtWalk), no read-time path builds them (atReadFamilies),
// and no rebuild stages them (rebuild.go stages the families' evals alone); a day with no row is a
// day that waits (R7).
//
// Each eval holds the catalogue's status series (fold.SeriesStatus) and a series of each of the
// project's code groups, `<base>:<group>` (catalogueEntry.groups): rows of eval_series that the run
// and the regroup write (WriteCodeGroupSeriesTx), which the seed keeps (ensure). The rows of a
// project are stored under its own key, the project's own cut (repo "") and the cut of each
// repository it holds changes of, and a project reads its own rows' via_repository as stored
// (viaAsRead): the changes with no issue that reached it through its default repository.

// The slugs of the two code evals.
const (
	SlugCodeSurvival    = "code-survival-30d"
	SlugPostMergeRework = "post-merge-rework-30d"
)

// CodeSlugs is the two code evals, whose day rows WriteCodeDaysTx writes and no refresher family
// builds: the list every reader of both evals (codesurvival's builder, the eval bench's k17) names
// them by.
var CodeSlugs = []string{SlugCodeSurvival, SlugPostMergeRework}

// CheckSeriesName refuses name where the registry refuses it as a series name (validSeriesName):
// empty, or holding whitespace, a control character or a `.`. A project's code group names the
// code evals' series `<base>:<group>`, so a group whose series this refuses is one the registry
// cannot hold.
func CheckSeriesName(name string) error {
	return validSeriesName(name)
}

// CheckCodeGroupName refuses group, the name of a project's code group, where a code eval could
// not hold a series it names: the first series `<base>:<group>` of the code evals' group bases
// (catalogueEntry.groupSeries), in CodeSlugs' order, that the registry refuses (refusedGroupSeries),
// which WriteCodeGroupSeriesTx refuses too. It answers that series and why, and "" and nil where
// every series it names is one the registry holds.
func CheckCodeGroupName(group string) (string, error) {
	entries, err := codeEntries()
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if series, err := refusedGroupSeries(entry, []string{group}); err != nil {
			return series, err
		}
	}
	return "", nil
}

// codeEntries is the catalogue's entry of each code eval, in CodeSlugs' order.
func codeEntries() ([]catalogueEntry, error) {
	entries := make([]catalogueEntry, len(CodeSlugs))
	for index, slug := range CodeSlugs {
		at := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug })
		if at < 0 {
			return nil, fmt.Errorf("evals: the catalogue holds no code eval %s", slug)
		}
		entries[index] = catalogue[at]
	}
	return entries, nil
}

// refusedGroupSeries is the first series of entry's group series of groups that the registry
// refuses, as a series name (validSeriesName) or as a key too long (keyTooLong), and why; "" and
// nil where it refuses none.
func refusedGroupSeries(entry catalogueEntry, groups []string) (string, error) {
	for _, series := range entry.groupSeries(groups) {
		if err := validSeriesName(series.Series); err != nil {
			return series.Series, err
		}
		if reason := keyTooLong("series", series.Series); reason != "" {
			return series.Series, errors.New(reason)
		}
	}
	return "", nil
}

// CodeDayRows is the day rows of survival and rework, the parts fold.BuildDays builds of the code
// survival eval and of the post-merge rework eval, each under project's eval id of its eval, in
// the cut repo: "" for the project's own rows, which a read that names no repository folds, or a
// repository, whose rows a read of that repository folds. Every row is listed and of no deploy
// repository. A cut is folded alone, so a cut holds the status rows of its days beside its group
// rows: the folds read a window's state from them.
func CodeDayRows(project, repo string, survival, rework []measures.DayPart) []DayRow {
	rows := make([]DayRow, 0, len(survival)+len(rework))
	for _, eval := range []struct {
		slug  string
		parts []measures.DayPart
	}{{SlugCodeSurvival, survival}, {SlugPostMergeRework, rework}} {
		evalID := EvalID(project, eval.slug)
		for _, part := range eval.parts {
			rows = append(rows, DayRow{ProjectKey: project, EvalID: evalID, Repo: repo, Listed: true, DayPart: part})
		}
	}
	return rows
}

// WriteCodeDaysTx writes, in tx, the caller's transaction, by build, project's day rows of the code
// evals evalIDs names (each the eval id of one of CodeSlugs in project) on days, each a UTC
// midnight: rows, every row of those evals on those days, of every cut (CodeDayRows). Each row is
// upserted where a value moved (upsertDays), so a row whose values stand is left as it is and logs
// no change. Each stored row of those evals on those days that rows does not hold is deleted
// (deleteStaleDays): the series of a group the project no longer names, the group rows of a day
// that is marked or blocked now, and every row of a day that rows holds none of. The stored
// figures of the days from the first of days to the last are then folded again where their rows
// moved (foldFiguresInTx).
//
// The eval_days trigger records each change in the change log's outbox, so tx takes no change log
// lock. The caller moves the changes into the log once tx has committed (delivery.DrainAfterCommit),
// as every writer of day rows does. A row of another project, eval, deploy repository or day, and
// an eval id that is no code eval of project, are refused before anything is written. The caller
// writes the evals' group series first, in tx (WriteCodeGroupSeriesTx), so the fold reads the
// series the rows are of.
func WriteCodeDaysTx(ctx context.Context, tx pgx.Tx, project string, evalIDs []string, days []time.Time, rows []DayRow, build string) error {
	slugs := make([]string, 0, len(evalIDs))
	for _, evalID := range evalIDs {
		slug := slugOf(project, evalID)
		if evalID != EvalID(project, slug) || !slices.Contains(CodeSlugs, slug) {
			return fmt.Errorf("evals: %s is no code eval of %s, whose code evals are %v", evalID, project, CodeSlugs)
		}
		if !slices.Contains(slugs, slug) {
			slugs = append(slugs, slug)
		}
	}
	written := map[time.Time]*ProjectDay{}
	var span daySpan
	for _, day := range days {
		date := measures.Midnight(day)
		if _, held := written[date]; !held {
			written[date], span = &ProjectDay{Project: project, Day: date}, span.widen(date)
		}
	}
	for _, row := range rows {
		day, held := written[measures.Midnight(row.Day)]
		if !held {
			return fmt.Errorf("evals: a code day row of %s is of %s on %s, which is none of the days written", project, row.EvalID, row.Day.Format(time.DateOnly))
		}
		day.Rows = append(day.Rows, row)
	}
	ordered := make([]ProjectDay, 0, len(written))
	for _, date := range slices.SortedFunc(maps.Keys(written), time.Time.Compare) {
		if err := checkProjectDay("code", *written[date], slugs, nil); err != nil {
			return err
		}
		ordered = append(ordered, *written[date])
	}
	if len(ordered) == 0 || len(slugs) == 0 {
		return nil
	}
	if err := writeDayRowsIn(ctx, tx, ordered, slugs, build); err != nil {
		return err
	}
	return foldFiguresInTx(ctx, tx, build, map[string]map[string]daySpan{project: slugSpans(project, slugs, span)})
}

// WriteCodeGroupSeriesTx writes, in tx, the caller's transaction, the series of groups, project's
// code groups by name and in order, into each code eval of project, by the seed's own version
// write (seriesToWrite) over the eval's group series alone: version 1 of each group series of each
// group the eval holds none of (catalogueEntry.groupSeries), the next version of one that differs
// from the form the catalogue states (a group named again after it was removed is listed again),
// and the next version, unlisted, of each listed group series of a group groups no longer names.
// It answers how many versions it wrote, none where the evals hold the groups' series already.
// eval_series' trigger records each version in the change log's outbox, so a view of the evals
// wakes to it, and the refresher's pass marks the cuts whose stored figures the evals' new fold key
// moved (markMovedKeys) once the caller has moved the changes into the log after tx commits.
//
// It takes the two evals' locks (lockEvals), as every writer of an eval's series does, and reads
// their series under them, so the versions it numbers are free. A transaction that waits for a
// lock while it holds a transaction id holds back every reader of the event log (inWrite), so the
// caller calls it before tx writes anything. A group name whose series the registry refuses
// (refusedGroupSeries), a group named twice, and a project whose code evals the seed has not
// written, are refused before anything is written.
func WriteCodeGroupSeriesTx(ctx context.Context, tx pgx.Tx, project string, groups []string) (int, error) {
	entries, err := codeEntries()
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(entries))
	for index, entry := range entries {
		ids[index] = EvalID(project, entry.definition.Slug)
		if series, err := refusedGroupSeries(entry, groups); err != nil {
			return 0, fmt.Errorf("evals: a code group of %s names the series %q of %s: %w", project, series, ids[index], err)
		}
	}
	for index, group := range groups {
		if slices.Contains(groups[:index], group) {
			return 0, fmt.Errorf("evals: %s names the code group %q twice", project, group)
		}
	}
	if err := lockEvals(ctx, tx, ids); err != nil {
		return 0, err
	}
	written := 0
	for index, entry := range entries {
		if _, found, err := newestDefinition(ctx, tx, ids[index]); err != nil {
			return 0, err
		} else if !found {
			return 0, fmt.Errorf("evals: %s holds no version of %s: the seed writes the catalogue into a project before its code groups' series", project, ids[index])
		}
		current, err := currentSeries(ctx, tx, ids[index])
		if err != nil {
			return 0, err
		}
		held := entry.groupSeriesIn(current, true)
		for _, series := range seriesToWrite(ids[index], entry.groupSeries(groups), held, held) {
			if _, inserted, err := insertSeries(ctx, tx, series); err != nil {
				return 0, err
			} else if !inserted {
				return 0, fmt.Errorf("version %d of series %s of %s was written by a write that holds no lock of the eval", series.Version, series.Series, series.EvalID)
			}
			written++
		}
	}
	return written, nil
}

// CheckCodeGroupSeriesRoom refuses, in tx, a save of groups, project's code groups by name in
// order, that would let a code eval hold more than MaxEvalSeries series, listed or not, so that no
// regroup is refused later for the bound (WriteCodeGroupSeriesTx writes a computed eval's series
// with no such check). It answers nil where both evals have room, and otherwise a CodeSeriesLimit
// refusal of the field code_groups naming the eval, the count and the bound. For each code eval it
// counts as a register write counts against the bound (seriesPastLimit): every series the eval
// holds now, listed or not, or the catalogue's series where the seed has not written the eval yet,
// and each group series it does not hold of groups and of the groups the project's settings hold
// now. WriteCodeGroupSeriesTx unlists the series of a group the project no longer names and deletes
// none, so every group name ever written stays counted. The stored groups count because a regroup
// may not have written their series yet and the next one may. The caller holds the project's
// settings lock (delivery.LockProject), so no other save replaces the stored groups before its own
// commits.
func CheckCodeGroupSeriesRoom(ctx context.Context, tx pgx.Tx, project string, groups []string) error {
	stored, err := delivery.GetProjectSettings(ctx, tx, project)
	if err != nil && !errors.Is(err, delivery.ErrNoSettings) {
		return err
	}
	names := slices.Concat(groups, delivery.CodeGroupNames(stored.CodeGroups))
	entries, err := codeEntries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		evalID := EvalID(project, entry.definition.Slug)
		held := map[string]Series{}
		if _, found, err := newestDefinition(ctx, tx, evalID); err != nil {
			return err
		} else if found {
			if held, err = currentSeries(ctx, tx, evalID); err != nil {
				return err
			}
		} else {
			for _, series := range entry.series {
				held[series.Series] = series
			}
		}
		series := entry.groupSeries(names)
		if index, _, total := seriesPastLimit(held, len(series), func(index int) string { return series[index].Series }); index >= 0 {
			return fieldRefusal(CodeSeriesLimit, "code_groups",
				"these code groups would leave %s holding %d series, listed or not, where an eval holds at most %d: each group's name adds %d series to it, and the series of a name no longer used stay counted, so name fewer new groups, or reuse names saved before",
				evalID, total, MaxEvalSeries, len(entry.groups))
		}
	}
	return nil
}
