package evals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// A logged eval's day rows are kept by the readings write, in its transaction, by increment: the
// write adds each new reading's part to the rows of its day and takes each replaced reading's
// part away from the rows of that reading's day, so no read of a window reads a reading and the
// write reads none but the readings it replaces.
//
// A reading counts on its day, the UTC date of its measured_at, in the project's row (repo '')
// and, where its attributes carry a repository (attributes.repo), in that repository's row too.
// A reading with no value (only an absence reason) is no part of any day row. A row holds, for
// the readings of its day that have a value:
//
//   - samples, every value, whatever the series' fold, so a later register write that changes the
//     fold (agg) still folds every day exactly;
//   - n, how many values; sum, their sum; max, the largest; all three read off samples;
//   - num and den, the weighted parts a weighted_mean folds: each value weighted by the reading's
//     own n, or by 1 where the reading carries none, kept by adding and subtracting;
//   - last_value and last_at, the value measured last and when, which the `last` fold reads: a
//     reading measured at or after last_at takes its place, the batch's later rows winning a tie.
//
// A replaced reading that was its day's last (its measured_at is the row's last_at) leaves the
// row with no last value unless a reading the write adds was measured at or after it. Only then
// does the write read a reading other than the ones it replaces: the newest reading of that
// series and day that stands and has a value (newestStanding), one row of that eval's standing
// readings, which the partial row index (eval_readings_row) holds under the eval's id.
//
// A row whose values are all taken away stays, as the zero row its day then is: n 0, sum, num and
// den 0, no maximum, no sample and no last value.

// dayKey is one day row of an eval: its series, its repository (empty for the project's own row)
// and its day.
type dayKey struct {
	series string
	repo   string
	day    time.Time
}

// dayPart is one reading's part of a day row: its value, its weight and when it was measured.
type dayPart struct {
	value  float64
	weight float64
	at     time.Time
}

// dayChange is what one write adds to one day row and takes from it.
type dayChange struct {
	added   []dayPart
	removed []dayPart
}

// dayKeysOf is the day rows a reading counts in, and its part of each, or none for a reading with
// no value.
func dayKeysOf(reading Reading) ([]dayKey, dayPart, bool) {
	if reading.Value == nil {
		return nil, dayPart{}, false
	}
	day := measures.Midnight(reading.MeasuredAt)
	keys := []dayKey{{series: reading.Series, day: day}}
	if repo, ok := reading.Attributes["repo"].(string); ok && repo != "" {
		keys = append(keys, dayKey{series: reading.Series, repo: repo, day: day})
	}
	weight := 1.0
	if reading.N != nil {
		weight = float64(*reading.N)
	}
	return keys, dayPart{value: *reading.Value, weight: weight, at: reading.MeasuredAt}, true
}

// writeDays keeps the day rows of definition, a logged eval, for the readings a write added and
// the readings it replaced, in the write's transaction: it makes sure each touched row exists,
// locks the rows in key order (so two writes of overlapping days wait on each other rather than
// deadlock), and writes each row's new parts, recording builtBy as the build that wrote them. It
// runs after the write's readings are inserted and its replaced readings superseded, so a read
// of the newest standing reading sees both.
func writeDays(ctx context.Context, w *writeTx, definition Definition, series map[string]Series, added, removed []Reading, builtBy string) error {
	changes := map[dayKey]*dayChange{}
	change := func(key dayKey) *dayChange {
		if changes[key] == nil {
			changes[key] = &dayChange{}
		}
		return changes[key]
	}
	for _, reading := range added {
		if keys, part, ok := dayKeysOf(reading); ok {
			for _, key := range keys {
				change(key).added = append(change(key).added, part)
			}
		}
	}
	for _, reading := range removed {
		if keys, part, ok := dayKeysOf(reading); ok {
			for _, key := range keys {
				change(key).removed = append(change(key).removed, part)
			}
		}
	}
	if len(changes) == 0 {
		return nil
	}
	keys := make([]dayKey, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.series != b.series {
			return a.series < b.series
		}
		if a.repo != b.repo {
			return a.repo < b.repo
		}
		return a.day.Before(b.day)
	})
	seriesNames, repos, days, listed := make([]string, len(keys)), make([]string, len(keys)), make([]time.Time, len(keys)), make([]bool, len(keys))
	for index, key := range keys {
		seriesNames[index], repos[index], days[index], listed[index] = key.series, key.repo, key.day, series[key.series].Listed
	}
	// A row two writes would both create is created once: the second insert waits for the
	// first's and then does nothing, and both lock the row below.
	if _, err := w.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, built_by)
		select $1, $2, k.series, k.repo, k.day, k.listed, $3
		from unnest($4::text[], $5::text[], $6::date[], $7::bool[]) as k(series, repo, day, listed)
		on conflict (project_key, eval_id, series, repo, day) do nothing
	`, definition.ProjectKey, definition.EvalID, builtBy, seriesNames, repos, days, listed); err != nil {
		return fmt.Errorf("make the day rows of %s: %w", definition.EvalID, err)
	}
	locked, err := w.Query(ctx, `
		select `+dayColumns+` from eval_days
		where project_key = $1 and eval_id = $2
		  and (series, repo, day) in (select * from unnest($3::text[], $4::text[], $5::date[]))
		order by series, repo, day
		for update
	`, definition.ProjectKey, definition.EvalID, seriesNames, repos, days)
	if err != nil {
		return fmt.Errorf("lock the day rows of %s: %w", definition.EvalID, err)
	}
	rows, err := pgx.CollectRows(locked, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
	if err != nil {
		return fmt.Errorf("lock the day rows of %s: %w", definition.EvalID, err)
	}
	if len(rows) != len(keys) {
		return fmt.Errorf("the write touches %d day rows of %s and locked %d", len(keys), definition.EvalID, len(rows))
	}
	batch := &pgx.Batch{}
	for _, row := range rows {
		key := dayKey{series: row.Series, repo: row.Repo, day: row.Day}
		next, restore := applyParts(row, *changes[key])
		if restore {
			if next.LastValue, next.LastAt, err = newestStanding(ctx, w.Tx, definition.EvalID, key); err != nil {
				return err
			}
		}
		batch.Queue(`
			update eval_days set listed = $6, n = $7, sum = $8, num = $9, den = $10, max = $11, samples = $12,
				last_value = $13, last_at = $14, built_by = $15, updated_at = now()
			where project_key = $1 and eval_id = $2 and series = $3 and repo = $4 and day = $5
		`, row.ProjectKey, row.EvalID, row.Series, row.Repo, row.Day, series[row.Series].Listed, next.N, next.Sum,
			next.Num, next.Den, next.Max, next.Samples, next.LastValue, next.LastAt, builtBy)
	}
	if err := w.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("write the day rows of %s: %w", definition.EvalID, err)
	}
	return nil
}

// applyParts is row with change applied, and whether its last value must be read again: the
// replaced reading was the row's last and no reading the write adds was measured at or after it.
func applyParts(row DayRow, change dayChange) (DayRow, bool) {
	samples := slices.Clone(row.Samples)
	num, den := measures.ValueOrZero(row.Num), measures.ValueOrZero(row.Den)
	lastValue, lastAt := row.LastValue, row.LastAt
	var replacedLast *time.Time
	for _, part := range change.removed {
		// change.removed holds only readings the row was given (foldedReadings), so its value is
		// among the samples; equal values are one value to a sample list, so any one is its own.
		index := slices.Index(samples, part.value)
		if index < 0 {
			continue
		}
		samples = slices.Delete(samples, index, index+1)
		num -= part.value * part.weight
		den -= part.weight
		if lastAt != nil && part.at.Equal(*lastAt) {
			replacedLast = &part.at
			lastValue, lastAt = nil, nil
		}
	}
	for _, part := range change.added {
		samples = append(samples, part.value)
		num += part.value * part.weight
		den += part.weight
		if lastAt == nil || !part.at.Before(*lastAt) {
			value, at := part.value, part.at
			lastValue, lastAt = &value, &at
		}
	}
	next := row
	next.N = len(samples)
	next.Samples = samples
	if next.N == 0 {
		zero := 0.0
		next.Sum, next.Num, next.Den, next.Max, next.LastValue, next.LastAt = &zero, &zero, &zero, nil, nil, nil
		next.Samples = []float64{}
		return next, false
	}
	sum, largest := 0.0, samples[0]
	for _, value := range samples {
		sum += value
		largest = max(largest, value)
	}
	next.Sum, next.Num, next.Den, next.Max = &sum, &num, &den, &largest
	next.LastValue, next.LastAt = lastValue, lastAt
	restore := replacedLast != nil && (lastAt == nil || lastAt.Before(*replacedLast))
	return next, restore
}

// newestStanding is the value and time of the newest reading of one day row that stands and has
// a value, nil where none does: the one read of a reading a readings write makes beyond the
// readings it replaces (writeDays).
func newestStanding(ctx context.Context, tx pgx.Tx, evalID string, key dayKey) (*float64, *time.Time, error) {
	var value float64
	var at time.Time
	err := tx.QueryRow(ctx, `
		select value, measured_at from eval_readings
		where eval_id = $1 and superseded_at is null and series = $2 and value is not null
		  and measured_at >= $3::timestamptz and measured_at < $3::timestamptz + interval '1 day'
		  and ($4 = '' or attributes ->> 'repo' = $4)
		order by measured_at desc, recorded_at desc
		limit 1
	`, evalID, key.series, key.day, key.repo).Scan(&value, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the newest standing reading of %s on %s: %w", evalID, key.day.Format(time.DateOnly), err)
	}
	at = at.UTC()
	return &value, &at, nil
}
