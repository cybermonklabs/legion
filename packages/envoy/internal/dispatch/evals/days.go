package evals

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// A logged eval's day rows are the readings write's (eval_days.writer `readings`), kept in its
// transaction, so no read of a window reads a reading.
//
// A reading counts on its day, the UTC date of its measured_at, in the project's row (repo '')
// and, where its attributes carry a repository (attributes.repo), in that repository's row too.
// A reading with no value (only an absence reason) is no part of any day row. A row holds, for
// the readings it folds:
//
//   - samples, every value, whatever the series' fold, so a later register write that changes the
//     fold (agg) still folds every day exactly;
//   - n, how many values; sum, their sum; max, the largest; all three read off samples;
//   - num and den, the weighted parts a weighted_mean folds: each value weighted by the reading's
//     own n, or by 1 where the reading carries none;
//   - last_value and last_at, the value measured last and when, which the `last` fold reads: a
//     reading measured at or after last_at takes its place, the one written later winning a tie,
//     and within one batch the later row.
//
// The readings a row folds are the standing readings of its day (superseded_at is null) that have
// a value and were written while the eval was logged (writtenWhileLogged): a reading written while
// it was computed was stored and never folded, and stays out of the row whatever its source is
// now.
//
// A row the write takes no replaced reading away from is kept by appending: the write adds each of
// its readings' parts to the row as it stands, which reads no reading. A row the write takes a
// replaced reading away from is folded again from what stands: the standing readings of its day
// stored before the write (standingParts, read under the row locks the write holds), then the
// write's own, in the batch's order. Taking the replaced reading's part away by subtraction would
// leave num and den the running difference of every value the day ever held, which keeps of the
// readings that stand only the precision the largest value it held left them: readings of 1e16
// and 1, the first corrected to 2, would leave a numerator of 2 where the fold of 2 and 1 is 3.
// Appending stays exact to that rule: an append is one more float addition to a row that is the
// fold of the readings it holds, so the row it leaves is the fold of those readings and the new
// ones, in the order they were written.
//
// A row whose readings are all taken away stays, as the zero row its day then is: n 0, sum, num and
// den 0, no maximum, no sample and no last value.
//
// A write that would leave a value of a row past what a float holds (a sum, numerator or
// denominator that overflows to an infinity) is refused, naming the row of the batch whose part
// made it so (CodeReadingDayOverflow), and stores nothing: no read can answer such a figure.

// dayKey is one day row of an eval: its series, its repository (empty for the project's own row)
// and its day.
type dayKey struct {
	series string
	repo   string
	day    time.Time
}

// compareDayKeys orders day keys as the rows' primary key does: by series, repository and day.
func compareDayKeys(a, b dayKey) int {
	return cmp.Or(cmp.Compare(a.series, b.series), cmp.Compare(a.repo, b.repo), a.day.Compare(b.day))
}

// dayPart is one reading's part of a day row: its value, its weight, when it was measured, and the
// index in the write's batch of the reading it is the part of, -1 for a reading stored before the
// write.
type dayPart struct {
	value  float64
	weight float64
	at     time.Time
	row    int
}

// dayKeysOf is the day rows a reading counts in, and its part of each as the batch's row row, or
// none for a reading with no value.
func dayKeysOf(reading Reading, row int) ([]dayKey, dayPart, bool) {
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
	return keys, dayPart{value: *reading.Value, weight: weight, at: reading.MeasuredAt, row: row}, true
}

// writeDays keeps the day rows of definition, a logged eval, for the readings a write added (in
// the batch's order) and the readings it replaced that a row folded, in the write's transaction:
// it makes sure each touched row exists, locks the rows in key order (so two writes of
// overlapping days wait on each other rather than deadlock), folds each row again or appends to
// it, and writes every row's new parts in one statement, recording builtBy as the build that wrote
// them. It runs after the write's readings are inserted and its replaced readings superseded, so a
// read of the standing readings sees both. A row it would leave holding a value no float holds
// refuses the write (CodeReadingDayOverflow).
func writeDays(ctx context.Context, w *writeTx, definition Definition, series map[string]Series, added, removed []Reading, builtBy string) error {
	// parts is each touched row's parts of the write's own readings, in the batch's order; refold
	// holds each row a replaced reading counted in; first is the first row of the batch that
	// touches each row, which answers for a value of that row the readings stored before the
	// write make too large.
	parts, refold, first := map[dayKey][]dayPart{}, map[dayKey]bool{}, map[dayKey]int{}
	touch := func(key dayKey, row int) {
		if earlier, ok := first[key]; !ok || row < earlier {
			first[key] = row
		}
	}
	written, correctedBy := make([]string, len(added)), map[string]int{}
	for index, reading := range added {
		written[index] = reading.ID
		if reading.Replaces != "" {
			correctedBy[reading.Replaces] = index
		}
		if keys, part, ok := dayKeysOf(reading, index); ok {
			for _, key := range keys {
				parts[key] = append(parts[key], part)
				touch(key, index)
			}
		}
	}
	for _, reading := range removed {
		if keys, _, ok := dayKeysOf(reading, -1); ok {
			for _, key := range keys {
				refold[key] = true
				touch(key, correctedBy[reading.ID])
			}
		}
	}
	if len(first) == 0 {
		return nil
	}
	keys := slices.SortedFunc(maps.Keys(first), compareDayKeys)
	seriesNames, repos, days, listed := make([]string, len(keys)), make([]string, len(keys)), make([]time.Time, len(keys)), make([]bool, len(keys))
	var refolded []dayKey
	for index, key := range keys {
		seriesNames[index], repos[index], days[index], listed[index] = key.series, key.repo, key.day, series[key.series].Listed
		if refold[key] {
			refolded = append(refolded, key)
		}
	}
	// A row two writes would both create is created once: the second insert waits for the
	// first's and then does nothing, and both lock the row below.
	if _, err := w.Exec(ctx, `
		insert into eval_days (project_key, eval_id, writer, series, repo, day, listed, built_by)
		select $1, $2, $3, k.series, k.repo, k.day, k.listed, $4
		from unnest($5::text[], $6::text[], $7::date[], $8::bool[]) as k(series, repo, day, listed)
		on conflict (project_key, eval_id, writer, series, repo, day) do nothing
	`, definition.ProjectKey, definition.EvalID, writerReadings, builtBy, seriesNames, repos, days, listed); err != nil {
		return fmt.Errorf("make the day rows of %s: %w", definition.EvalID, err)
	}
	locked, err := w.Query(ctx, `
		select `+dayColumns+` from eval_days
		where project_key = $1 and eval_id = $2 and writer = $3
		  and (series, repo, day) in (select * from unnest($4::text[], $5::text[], $6::date[]))
		order by series, repo, day
		for update
	`, definition.ProjectKey, definition.EvalID, writerReadings, seriesNames, repos, days)
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
	standing, err := standingParts(ctx, w.Tx, definition.EvalID, refolded, written)
	if err != nil {
		return err
	}
	// One statement writes every row, so the statement-level trigger of eval_days logs this eval's
	// change once (20261113082202) rather than rewriting its change row once a row. Each row's key
	// and values go at its index among the locked rows, the key's arrays above filled again in that
	// order. An array parameter holds no arrays of differing lengths, so the rows' samples go as one
	// flat list, each row's the n of them from its first's position on.
	counts, firsts := make([]int, len(rows)), make([]int, len(rows))
	sums, nums, dens, maxes := make([]*float64, len(rows)), make([]*float64, len(rows)), make([]*float64, len(rows)), make([]*float64, len(rows))
	lastValues, lastAts := make([]*float64, len(rows)), make([]*time.Time, len(rows))
	// Empty rather than nil, which pgx would send as null, and a slice of null is null where a row
	// with no value holds the empty list.
	samples := []float64{}
	var refused []RowError
	for index, row := range rows {
		seriesNames[index], repos[index], days[index] = row.Series, row.Repo, row.Day
		key := dayKey{series: row.Series, repo: row.Repo, day: row.Day}
		base, given := row, parts[key]
		if refold[key] {
			base.Samples, base.Num, base.Den, base.LastValue, base.LastAt = nil, nil, nil, nil, nil
			given = append(standing[key], given...)
		}
		next, overflow := addParts(base, given, first[key])
		if overflow >= 0 {
			if !slices.ContainsFunc(refused, func(refusal RowError) bool { return refusal.Index == overflow }) {
				refused = append(refused, RowError{Index: overflow, Code: CodeReadingDayOverflow, Field: "value", Message: fmt.Sprintf(
					"with this value the sums of series %q on %s (its values, and each weighted by its n) pass the largest number a float holds, which no read could answer",
					row.Series, row.Day.Format(time.DateOnly))})
			}
			continue
		}
		listed[index], counts[index], firsts[index] = series[row.Series].Listed, next.N, len(samples)+1
		sums[index], nums[index], dens[index], maxes[index] = next.Sum, next.Num, next.Den, next.Max
		lastValues[index], lastAts[index] = next.LastValue, next.LastAt
		samples = append(samples, next.Samples...)
	}
	if len(refused) > 0 {
		return rowsRefusal(refused)
	}
	if _, err := w.Exec(ctx, `
		update eval_days d set listed = given.listed, n = given.n, sum = given.sum, num = given.num, den = given.den,
			max = given.max, samples = ($15::float8[])[given.first : given.first + given.n - 1],
			last_value = given.last_value, last_at = given.last_at, built_by = $4, updated_at = now()
		from unnest($5::text[], $6::text[], $7::date[], $8::bool[], $9::int[], $10::float8[], $11::float8[],
			$12::float8[], $13::float8[], $14::float8[], $16::timestamptz[], $17::int[])
			as given(series, repo, day, listed, n, sum, num, den, max, last_value, last_at, first)
		where d.project_key = $1 and d.eval_id = $2 and d.writer = $3
		  and d.series = given.series and d.repo = given.repo and d.day = given.day
	`, definition.ProjectKey, definition.EvalID, writerReadings, builtBy, seriesNames, repos, days, listed, counts, sums, nums,
		dens, maxes, lastValues, samples, lastAts, firsts); err != nil {
		return fmt.Errorf("write the day rows of %s: %w", definition.EvalID, err)
	}
	return nil
}

// addParts is row with parts added in their order, and the index in the write's batch of the first
// part after which a value of the row is past what a float holds, or -1 where every value stays
// within it. A part of a reading stored before the write (row -1) that makes it so is answered
// for by blame, the first row of the batch that touches the row. The row's sum and maximum are
// read off its samples, in their order, as every write of the row reads them.
func addParts(row DayRow, parts []dayPart, blame int) (DayRow, int) {
	samples := slices.Clone(row.Samples)
	num, den := measures.ValueOrZero(row.Num), measures.ValueOrZero(row.Den)
	sum, largest := 0.0, math.Inf(-1)
	for _, value := range samples {
		sum += value
		largest = max(largest, value)
	}
	lastValue, lastAt := row.LastValue, row.LastAt
	overflow := -1
	for _, part := range parts {
		samples = append(samples, part.value)
		sum += part.value
		largest = max(largest, part.value)
		num += part.value * part.weight
		den += part.weight
		if lastAt == nil || !part.at.Before(*lastAt) {
			value, at := part.value, part.at
			lastValue, lastAt = &value, &at
		}
		if overflow < 0 && !finite(part.value, sum, num, den) {
			overflow = part.row
			if overflow < 0 {
				overflow = blame
			}
		}
	}
	next := row
	next.N = len(samples)
	next.Samples = samples
	if next.N == 0 {
		zero := 0.0
		next.Sum, next.Num, next.Den, next.Max, next.LastValue, next.LastAt = &zero, &zero, &zero, nil, nil, nil
		next.Samples = []float64{}
		return next, overflow
	}
	next.Sum, next.Num, next.Den, next.Max = &sum, &num, &den, &largest
	next.LastValue, next.LastAt = lastValue, lastAt
	return next, overflow
}

// finite reports whether every one of values is a number a float holds, neither infinite nor NaN.
func finite(values ...float64) bool {
	for _, value := range values {
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return false
		}
	}
	return true
}

// writtenWhileLogged is the test that the reading r was written while its eval was logged: the
// version in force at its recorded_at, which is the write's own transaction time as a version's
// created_at is, has source logged. Such a reading is the one a logged eval's day rows fold.
const writtenWhileLogged = `(select v.source from eval_versions v
	where v.eval_id = r.eval_id and v.created_at <= r.recorded_at
	order by v.version desc limit 1) = '` + SourceLogged + `'`

// standingParts is, for each of keys, the parts of the readings its row folds that were stored
// before the write (whose own readings written names), in the order they were written: the
// standing readings of its day and series (of its repository, for a repository's row) that have a
// value and were written while the eval was logged (writtenWhileLogged). One statement reads
// every key's, each the eval's standing readings of one day through eval_readings_row.
func standingParts(ctx context.Context, tx pgx.Tx, evalID string, keys []dayKey, written []string) (map[dayKey][]dayPart, error) {
	standing := map[dayKey][]dayPart{}
	if len(keys) == 0 {
		return standing, nil
	}
	series, repos, days := make([]string, len(keys)), make([]string, len(keys)), make([]time.Time, len(keys))
	for index, key := range keys {
		series[index], repos[index], days[index] = key.series, key.repo, key.day
	}
	rows, err := tx.Query(ctx, `
		select k.at, r.value, coalesce(r.n, 1)::float8, r.measured_at
		from unnest($2::text[], $3::text[], $4::timestamptz[]) with ordinality as k(series, repo, day, at)
		join eval_readings r on r.eval_id = $1 and r.superseded_at is null and r.value is not null
			and r.series = k.series and r.measured_at >= k.day and r.measured_at < k.day + interval '24 hours'
			and (k.repo = '' or r.attributes ->> 'repo' = k.repo)
			and r.id <> all($5::text[]::uuid[])
			and `+writtenWhileLogged+`
		order by k.at, r.recorded_at, r.measured_at, r.id
	`, evalID, series, repos, days, written)
	if err != nil {
		return nil, fmt.Errorf("read the standing readings of %d day rows of %s: %w", len(keys), evalID, err)
	}
	var at int
	var value, weight float64
	var measured time.Time
	if _, err := pgx.ForEachRow(rows, []any{&at, &value, &weight, &measured}, func() error {
		key := keys[at-1]
		standing[key] = append(standing[key], dayPart{value: value, weight: weight, at: measured.UTC(), row: -1})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the standing readings of %d day rows of %s: %w", len(keys), evalID, err)
	}
	return standing, nil
}
