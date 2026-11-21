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
//     reading measured after last_at takes its place, and of readings measured at one moment the
//     one written last holds it: the one of the later write (recorded_at, the time its
//     transaction began) and within one write the later row (id), whichever path writes the row;
//   - last_recorded_at and last_reading, that reading's recorded_at and id, so an append orders a
//     reading against the row's last as a refold does, and a read that folds several
//     repositories' rows of one day breaks a tie of their last_at by the same order.
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
// A row whose last a reading of the write ties, one measured at the row's last_at, is appended to
// as any other: the row holds which reading holds its last value (last_recorded_at,
// last_reading), so the append orders the two by when they were written, as a refold of the
// standing readings does. A write that began later can take the row's lock and commit first, and
// its reading then stays the row's last when this write appends its own.
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

// compareDayKeys orders day keys by series, repository and day, each string by its bytes: the
// order every statement that inserts or locks one eval's day rows takes them in, whatever the
// database's collation (ICU en-US on this deployment's server orders a series `latency` before a
// series `Queue`, and bytes the other way), so two writers of overlapping rows wait on each other
// rather than deadlock. Each names that order as `collate "C"`, save writeDayRows', which takes
// its rows in the order it is sent them, this one (compareDayRows).
func compareDayKeys(a, b dayKey) int {
	return cmp.Or(cmp.Compare(a.series, b.series), cmp.Compare(a.repo, b.repo), a.day.Compare(b.day))
}

// dayPart is one reading's part of a day row: its value, its weight, when it was measured, when
// its write began and its id, which order it among the readings written (writtenBefore), and the
// index in the write's batch of the reading it is the part of, -1 for a reading stored before the
// write.
type dayPart struct {
	value      float64
	weight     float64
	at         time.Time
	recordedAt time.Time
	id         string
	row        int
}

// writtenBefore reports whether p's reading was written before q's: by an earlier write (its
// recorded_at, the time its transaction began) or, within one write, earlier in the batch (its
// id, a UUIDv7 parseRows mints in the batch's order, whose text sorts as the uuid does). It is the
// order standingParts reads the readings in, and it breaks a tie of moments for a last value on
// every path that writes a row (addParts) and in the `last` fold of a window (aggStatistics).
func (p dayPart) writtenBefore(q dayPart) bool {
	return cmp.Or(p.recordedAt.Compare(q.recordedAt), cmp.Compare(p.id, q.id)) < 0
}

// lastBefore reports whether p's value comes before q's as a last value: measured earlier, or at
// one moment written earlier (writtenBefore).
func (p dayPart) lastBefore(q dayPart) bool {
	return p.at.Before(q.at) || p.at.Equal(q.at) && p.writtenBefore(q)
}

// lastOf is part's last value as the part of the reading that holds it: the value, its moment
// and the write that stored it (LastRecordedAt, LastReading; the zero time and "" where the part
// records no write, which every write orders after), and false where part holds no last value.
func lastOf(part measures.DayPart) (dayPart, bool) {
	if part.LastValue == nil || part.LastAt == nil {
		return dayPart{}, false
	}
	last := dayPart{value: *part.LastValue, at: *part.LastAt, id: part.LastReading, row: -1}
	if part.LastRecordedAt != nil {
		last.recordedAt = *part.LastRecordedAt
	}
	return last, true
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
	return keys, dayPart{value: *reading.Value, weight: weight, at: reading.MeasuredAt, recordedAt: reading.RecordedAt, id: reading.ID, row: row}, true
}

// writeDays keeps the day rows of definition, a logged eval, for the readings a write added (in
// the batch's order) and the readings it replaced that a row folded, in the write's transaction,
// under the eval's lock (WriteReadings): it makes sure each touched row exists and locks the
// rows, both in the byte order of their keys (compareDayKeys), folds each row again or appends to
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
	for index, key := range keys {
		seriesNames[index], repos[index], days[index], listed[index] = key.series, key.repo, key.day, series[key.series].Listed
	}
	// A row two writers would both create is created once: the second insert waits for the
	// first's and then does nothing, and both lock the row below. Every writer of these rows holds
	// the eval's lock, so none runs beside this one; each still takes the rows in this byte order,
	// so two that did would wait on each other rather than deadlock.
	if _, err := w.Exec(ctx, `
		insert into eval_days (project_key, eval_id, writer, series, repo, day, listed, built_by)
		select $1, $2, $3, k.series, k.repo, k.day, k.listed, $4
		from unnest($5::text[], $6::text[], $7::date[], $8::bool[]) as k(series, repo, day, listed)
		order by k.series collate "C", k.repo collate "C", k.day
		on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do nothing
	`, definition.ProjectKey, definition.EvalID, writerReadings, builtBy, seriesNames, repos, days, listed); err != nil {
		return fmt.Errorf("make the day rows of %s: %w", definition.EvalID, err)
	}
	locked, err := w.Query(ctx, `
		select `+dayColumns+` from eval_days
		where project_key = $1 and eval_id = $2 and writer = $3 and deploy_repo = ''
		  and (series, repo, day) in (select * from unnest($4::text[], $5::text[], $6::date[]))
		order by series collate "C", repo collate "C", day
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
	var refolded []dayKey
	for _, row := range rows {
		if key := (dayKey{series: row.Series, repo: row.Repo, day: row.Day}); refold[key] {
			refolded = append(refolded, key)
		}
	}
	standing, err := standingParts(ctx, w.Tx, definition.EvalID, refolded, written)
	if err != nil {
		return err
	}
	// One statement writes every row, so the statement-level trigger of eval_days logs this eval's
	// change once (20261117100014) rather than rewriting its change row once a row. Each row's key
	// and values go at its index among the locked rows, the key's arrays above filled again in that
	// order. An array parameter holds no arrays of differing lengths, so the rows' samples go as one
	// flat list, each row's the n of them from its first's position on.
	counts, firsts := make([]int, len(rows)), make([]int, len(rows))
	sums, nums, dens, maxes := make([]*float64, len(rows)), make([]*float64, len(rows)), make([]*float64, len(rows)), make([]*float64, len(rows))
	lastValues, lastAts, lastRecordedAts := make([]*float64, len(rows)), make([]*time.Time, len(rows)), make([]*time.Time, len(rows))
	lastReadings := make([]string, len(rows))
	// Empty rather than nil, which pgx would send as null, and a slice of null is null where a row
	// with no value holds the empty list.
	samples := []float64{}
	var refused []RowError
	for index, row := range rows {
		seriesNames[index], repos[index], days[index] = row.Series, row.Repo, row.Day
		key := dayKey{series: row.Series, repo: row.Repo, day: row.Day}
		base, given := row, parts[key]
		if refold[key] {
			base.Samples, base.Num, base.Den = nil, nil, nil
			base.LastValue, base.LastAt, base.LastRecordedAt, base.LastReading = nil, nil, nil, ""
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
		lastValues[index], lastAts[index], lastRecordedAts[index], lastReadings[index] = next.LastValue, next.LastAt, next.LastRecordedAt, next.LastReading
		samples = append(samples, next.Samples...)
	}
	if len(refused) > 0 {
		return rowsRefusal(refused)
	}
	if _, err := w.Exec(ctx, `
		update eval_days d set listed = given.listed, n = given.n, sum = given.sum, num = given.num, den = given.den,
			max = given.max, samples = ($15::float8[])[given.first : given.first + given.n - 1],
			last_value = given.last_value, last_at = given.last_at, last_recorded_at = given.last_recorded_at,
			last_reading = nullif(given.last_reading, '')::uuid, built_by = $4, updated_at = now()
		from unnest($5::text[], $6::text[], $7::date[], $8::bool[], $9::int[], $10::float8[], $11::float8[],
			$12::float8[], $13::float8[], $14::float8[], $16::timestamptz[], $18::timestamptz[], $19::text[], $17::int[])
			as given(series, repo, day, listed, n, sum, num, den, max, last_value, last_at, last_recorded_at, last_reading, first)
		where d.project_key = $1 and d.eval_id = $2 and d.writer = $3 and d.deploy_repo = ''
		  and d.series = given.series and d.repo = given.repo and d.day = given.day
	`, definition.ProjectKey, definition.EvalID, writerReadings, builtBy, seriesNames, repos, days, listed, counts, sums, nums,
		dens, maxes, lastValues, samples, lastAts, firsts, lastRecordedAts, lastReadings); err != nil {
		return fmt.Errorf("write the day rows of %s: %w", definition.EvalID, err)
	}
	return nil
}

// addParts is row with parts added in their order, and the index in the write's batch of the first
// part after which a value of the row is past what a float holds, or -1 where every value stays
// within it. A part of a reading stored before the write (row -1) that makes it so is answered
// for by blame, the first row of the batch that touches the row. The row's sum and maximum are
// read off its samples, in their order, as every write of the row reads them. Its last value is
// the part measured last, and of parts measured at one moment the one written last (lastBefore),
// the row's own last value among them as the part of the reading the row records holding it
// (lastOf), in whatever order they come: a refold's standing parts come first and the write's own
// after them, though a write that began later may have stored some of them, and the row an append
// adds to may hold the reading of a write that began after this one and committed first.
func addParts(row DayRow, parts []dayPart, blame int) (DayRow, int) {
	samples := slices.Clone(row.Samples)
	num, den := measures.ValueOrZero(row.Num), measures.ValueOrZero(row.Den)
	sum, largest := 0.0, math.Inf(-1)
	for _, value := range samples {
		sum += value
		largest = max(largest, value)
	}
	lastValue, lastAt, lastRecordedAt, lastReading := row.LastValue, row.LastAt, row.LastRecordedAt, row.LastReading
	// holder is the part whose value is the last: the row's own, until a part comes after it.
	holder, held := lastOf(row.DayPart)
	overflow := -1
	for _, part := range parts {
		samples = append(samples, part.value)
		sum += part.value
		largest = max(largest, part.value)
		num += part.value * part.weight
		den += part.weight
		if !held || holder.lastBefore(part) {
			value, at, recordedAt := part.value, part.at, part.recordedAt
			lastValue, lastAt, lastRecordedAt, lastReading = &value, &at, &recordedAt, part.id
			holder, held = part, true
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
		next.Sum, next.Num, next.Den, next.Max = &zero, &zero, &zero, nil
		next.LastValue, next.LastAt, next.LastRecordedAt, next.LastReading = nil, nil, nil, ""
		next.Samples = []float64{}
		return next, overflow
	}
	next.Sum, next.Num, next.Den, next.Max = &sum, &num, &den, &largest
	next.LastValue, next.LastAt, next.LastRecordedAt, next.LastReading = lastValue, lastAt, lastRecordedAt, lastReading
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

// loggedSpans is the common table expression logged_spans(from_at, until_at), which a statement
// testing writtenWhileLogged heads: the spans of time [from_at, until_at) over which a version of
// the eval $1 names, one whose source is logged, was the version in force. The version in force
// at a moment is the newest version created at or before it, so a version is in force from its
// created_at until the earliest created_at of any newer version (for ever, for the newest); a
// span that ends where it starts is a version a newer one replaced at once, and holds no moment.
// It is materialized, so the statement reads and orders the eval's versions once, whatever number
// of readings it tests against them.
const loggedSpans = `logged_spans as materialized (
	select from_at, until_at from (
		select source, created_at as from_at, coalesce(min(created_at) over (
			order by version desc rows between unbounded preceding and 1 preceding), 'infinity') as until_at
		from eval_versions where eval_id = $1
	) spans
	where source = '` + SourceLogged + `' and until_at > from_at
)`

// writtenWhileLogged is the test that the reading r, of the eval loggedSpans reads, was written
// while its eval was logged: its recorded_at, which is the write's own transaction time as a
// version's created_at is, lies in a span of logged_spans. Such a reading is the one a logged
// eval's day rows fold.
const writtenWhileLogged = `exists (select from logged_spans s where r.recorded_at >= s.from_at and r.recorded_at < s.until_at)`

// standingParts is, for each of keys, the parts of the readings its row folds that were stored
// before the write (whose own readings written names), in the order they were written
// (writtenBefore): by the write that stored them (recorded_at, its transaction's time) and, within
// one write, by id, which is the batch's order (parseRows mints them so). That is the order the
// appends took them in wherever the writes committed in the order they began, so a refold leaves
// the day's samples and sums as the appends left them; its last value is the appends' whatever
// order the writes committed in, since every path breaks a tie of moments by this order. They are
// the standing readings of its day and series (of its repository, for a repository's row) that
// have a value and were written while the eval was logged (writtenWhileLogged). One statement
// reads every key's (standingPartsQuery): the eval's standing readings of each day the keys name,
// once a day, through eval_readings_row with that day's range in its index condition, less the
// write's own, each then read by its id and matched to the keys of its day. Joined to every key
// at once instead, the readings were planned, for a week's keys, as one scan of the eval that
// each key then filtered whole.
func standingParts(ctx context.Context, tx pgx.Tx, evalID string, keys []dayKey, written []string) (map[dayKey][]dayPart, error) {
	standing := map[dayKey][]dayPart{}
	if len(keys) == 0 {
		return standing, nil
	}
	series, repos, days := make([]string, len(keys)), make([]string, len(keys)), make([]time.Time, len(keys))
	for index, key := range keys {
		series[index], repos[index], days[index] = key.series, key.repo, key.day
	}
	rows, err := tx.Query(ctx, standingPartsQuery, evalID, series, repos, days, written)
	if err != nil {
		return nil, fmt.Errorf("read the standing readings of %d day rows of %s: %w", len(keys), evalID, err)
	}
	var at int
	var value, weight float64
	var measured, recorded time.Time
	var id string
	if _, err := pgx.ForEachRow(rows, []any{&at, &value, &weight, &measured, &recorded, &id}, func() error {
		key := keys[at-1]
		standing[key] = append(standing[key], dayPart{value: value, weight: weight, at: measured.UTC(), recordedAt: recorded, id: id, row: -1})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the standing readings of %d day rows of %s: %w", len(keys), evalID, err)
	}
	return standing, nil
}

// standingPartsQuery is standingParts' statement: $1 the eval, $2 to $4 the keys (series,
// repository, day) and $5 the ids of the write's own readings. standing is the ids of the
// standing readings of the keys' days except the write's, materialized, so the set operation
// runs once whatever plan the joins after it take, and reads each side once (day_rows.go says
// why a day's write finds what it leaves out so): as `r.id <> all($5)` in each day's read, every
// reading was compared with the list, which a generic plan, knowing no value of it, cannot hash;
// as a `not exists`, its generic plan was a nested-loop anti-join that runs the list again for
// each reading, and its plan with the values hashed the list again for each key. Each reading is
// then matched to its keys by series and by the day itself, the UTC midnight its measured_at falls
// in, which every key's day is (dayKeysOf's measures.Midnight, or a date of eval_days): a hash
// join keys on it, where a match on the range of each key's day tested each reading against every
// key of its series.
const standingPartsQuery = `
	with ` + loggedSpans + `, k as (
		select * from unnest($2::text[], $3::text[], $4::timestamptz[]) with ordinality as k(series, repo, day, at)
	), standing as materialized (
		select r.id
		from (select distinct day from k) d
		cross join lateral (
			select r.id from eval_readings r
			where r.eval_id = $1 and r.superseded_at is null and r.value is not null
				and r.measured_at >= d.day and r.measured_at < d.day + interval '24 hours'
				and ` + writtenWhileLogged + `
		) r
		except
		select * from unnest($5::text[]::uuid[]) as written(id)
	)
	select k.at, r.value, coalesce(r.n, 1)::float8, r.measured_at, r.recorded_at, r.id::text
	from standing
	join eval_readings r on r.id = standing.id
	join k on k.series = r.series and k.day = date_trunc('day', r.measured_at, 'UTC')
		and (k.repo = '' or r.attributes ->> 'repo' = k.repo)
	order by k.at, r.recorded_at, r.id
`
