package evals

import (
	"cmp"
	"context"
	"fmt"
	"iter"
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
// made it so (CodeReadingDayOverflow), and stores nothing: no read can answer such a figure. So is
// a write that would leave a row holding more values than MaxDaySamples where it held fewer
// (CodeReadingDaySamplesLimit): the row's update sends every value it holds, and past that many it
// would pass the statement bound (statementBytes). A write that adds no value to such a row, a
// correction of one of its readings, is taken.

// dayKey is one day row of an eval: its series, its repository (empty for the project's own row)
// and its day.
type dayKey struct {
	series string
	repo   string
	day    time.Time
}

// insertBytes is the most bytes k adds to the parameters of writeDays' insert of its row, as pgx
// encodes them in binary: in each of the statement's four arrays 4 bytes of length and the element,
// the series and the repository their bytes, the day at most 8 and the listing 1. The lock after
// the insert, and standingParts' read of the readings that stand, send three of those arrays, and
// so take no more.
func (k *dayKey) insertBytes() int {
	return 4*4 + len(k.series) + len(k.repo) + 8 + 1
}

// zeroBytes is the most bytes k adds to the parameters of writeLoggedZeros' insert of the zero rows
// after it, as pgx encodes them in binary: in each of the statement's two arrays 4 bytes of length
// and the element, the series its bytes and the day at most 8.
func (k *dayKey) zeroBytes() int {
	return 2*4 + len(k.series) + 8
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
// it, and writes every row's new parts, recording builtBy as the build that wrote them. Each of
// the three is sent in statements of at most statementBytes of parameters (statementChunks), in
// the order of the keys: the insert and the lock in chunks of the keys (insertBytes), the update
// in chunks of the rows (dayUpdates). It runs after the write's readings are inserted and its
// replaced readings superseded, so a read of the standing readings sees both. A row it would leave
// holding a value no float holds refuses the write (CodeReadingDayOverflow), and so does a row it
// would leave holding more than MaxDaySamples values where it held fewer
// (CodeReadingDaySamplesLimit); nothing is written then. Last it writes the zero rows the rows it
// touched leave owed (writeLoggedZeros) and adds the days it wrote of them to spans.
func writeDays(ctx context.Context, w *writeTx, definition Definition, series map[string]Series, added, removed []Reading, builtBy string, spans map[string]daySpan) error {
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
	// ends is where each chunk of the keys ends that one insert, and then one lock, sends.
	var ends []int
	end := 0
	for chunk := range statementChunks(keys, paramBytes(4, definition.ProjectKey, definition.EvalID, writerReadings, builtBy), (*dayKey).insertBytes) {
		end += len(chunk)
		ends = append(ends, end)
	}
	// A row two writers would both create is created once: the second insert waits for the
	// first's and then does nothing, and both lock the row below. Every writer of these rows holds
	// the eval's lock, so none runs beside this one; each still takes the rows in this byte order,
	// so two that did would wait on each other rather than deadlock. The chunks go in that order,
	// the inserts of every chunk before the lock of any, so the statements take the rows in the
	// order one insert and then one lock did.
	from := 0
	for _, to := range ends {
		if _, err := w.Exec(ctx, `
			insert into eval_days (project_key, eval_id, writer, series, repo, day, listed, built_by)
			select $1, $2, $3, k.series, k.repo, k.day, k.listed, $4
			from unnest($5::text[], $6::text[], $7::date[], $8::bool[]) as k(series, repo, day, listed)
			order by k.series collate "C", k.repo collate "C", k.day
			on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do nothing
		`, definition.ProjectKey, definition.EvalID, writerReadings, builtBy, seriesNames[from:to], repos[from:to], days[from:to], listed[from:to]); err != nil {
			return fmt.Errorf("make the day rows of %s: %w", definition.EvalID, err)
		}
		from = to
	}
	rows := make([]DayRow, 0, len(keys))
	from = 0
	for _, to := range ends {
		locked, err := w.Query(ctx, `
			select `+dayColumns+` from eval_days
			where project_key = $1 and eval_id = $2 and writer = $3 and deploy_repo = ''
			  and (series, repo, day) in (select * from unnest($4::text[], $5::text[], $6::date[]))
			order by series collate "C", repo collate "C", day
			for update
		`, definition.ProjectKey, definition.EvalID, writerReadings, seriesNames[from:to], repos[from:to], days[from:to])
		if err != nil {
			return fmt.Errorf("lock the day rows of %s: %w", definition.EvalID, err)
		}
		held, err := pgx.AppendRows(rows, locked, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
		if err != nil {
			return fmt.Errorf("lock the day rows of %s: %w", definition.EvalID, err)
		}
		rows, from = held, to
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
	// Every row's next state, at its index among the locked rows, before any is written: a refused
	// row refuses the write, each row of the batch named once, whichever rows it makes so.
	next := make([]DayRow, len(rows))
	var refused []RowError
	refuse := func(refusal RowError) {
		if !slices.ContainsFunc(refused, func(earlier RowError) bool { return earlier.Index == refusal.Index }) {
			refused = append(refused, refusal)
		}
	}
	for index, row := range rows {
		key := dayKey{series: row.Series, repo: row.Repo, day: row.Day}
		base, given := row, parts[key]
		if refold[key] {
			base.Samples, base.Num, base.Den = nil, nil, nil
			base.LastValue, base.LastAt, base.LastRecordedAt, base.LastReading = nil, nil, nil, ""
			given = append(standing[key], given...)
		}
		folded, overflow := addParts(base, given, first[key])
		switch {
		case overflow >= 0:
			refuse(RowError{Index: overflow, Code: CodeReadingDayOverflow, Field: "value", Message: fmt.Sprintf(
				"with this value the sums of series %q on %s (its values, and each weighted by its n) pass the largest number a float holds, which no read could answer",
				row.Series, row.Day.Format(time.DateOnly))})
		case folded.N > MaxDaySamples && folded.N > row.N:
			refuse(RowError{Index: pastDaySamples(base, given, first[key]), Code: CodeReadingDaySamplesLimit, Field: "measured_at", Message: fmt.Sprintf(
				"with this reading the day row of %s would hold %d values, and a day row holds at most %d, so that its write is sent in one statement of at most %d bytes",
				dayRowName(row), folded.N, MaxDaySamples, statementBytes)})
		default:
			folded.Listed = series[row.Series].Listed
			next[index] = folded
		}
	}
	if len(refused) > 0 {
		return rowsRefusal(refused)
	}
	// Each statement of the update widens the one eval_days change the transaction logs of this
	// eval (the statement-level trigger of 20261117100014), so the statements log the eval's change
	// once, as one statement did.
	for args := range dayUpdates(definition.ProjectKey, definition.EvalID, builtBy, next) {
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
		`, args...); err != nil {
			return fmt.Errorf("write the day rows of %s: %w", definition.EvalID, err)
		}
	}
	return writeLoggedZeros(ctx, w, definition, keys, builtBy, spans)
}

// writeLoggedZeros writes, in the readings write's transaction, the zero rows the day rows it
// touched leave owed, exactly the rows the refresher's fill (fillLoggedZeros) would write after
// them: of each series keys touch (writeDays' keys, in their byte order), the zero row of every
// day after each of its stored all-repositories rows from the first day the write touched of it
// on, up to the series' next stored row, or through today after its newest (loggedZeros), and it
// adds the days it wrote to spans. So a first reading of a series leaves it a row on every day from
// the reading's through today, and a reading before a series' first day one on every day up to that
// day, in the transaction that stores the reading: a view of the eval is sent the one change of the
// write's day rows (each statement widens the transaction's one eval_days change, migration
// 20261117100014), and never the days after a reading without their rows, which no whole fill
// leaves. A gap before the first day the write touched of a series was owed before the write, and
// is left to the fill.
//
// today is the UTC date of the database's clock when the write fills, read after the write took
// the eval's lock and, where it registers the eval, the lock of the project's `evals-md`
// (lockDocument): fillLoggedZeros says why that leaves every day of the eval its one row at a
// midnight. The series go in statements of at most statementBytes of parameters (statementChunks),
// in the keys' order, each inserting its rows in the byte order of their keys, after every row
// writeDays inserted and locked: every writer of the eval's rows holds its lock, so none takes them
// beside this one.
func writeLoggedZeros(ctx context.Context, w *writeTx, definition Definition, keys []dayKey, builtBy string, spans map[string]daySpan) error {
	// firsts is each touched series' first all-repositories key, the earliest day the write touched
	// of it: keys are in their byte order, and every reading counts in its project's row
	// (dayKeysOf).
	var firsts []dayKey
	for _, key := range keys {
		if key.repo == "" && (len(firsts) == 0 || firsts[len(firsts)-1].series != key.series) {
			firsts = append(firsts, key)
		}
	}
	var clock time.Time
	if err := w.QueryRow(ctx, `select statement_timestamp()`).Scan(&clock); err != nil {
		return fmt.Errorf("read the database's clock for the zero rows of %s: %w", definition.EvalID, err)
	}
	today := measures.Midnight(clock)
	// Beside its two arrays the statement sends four texts and today, a date of at most 8 bytes after
	// its 4 of length.
	base := paramBytes(2, definition.ProjectKey, writerReadings, builtBy, definition.EvalID) + 4 + 8
	for chunk := range statementChunks(firsts, base, (*dayKey).zeroBytes) {
		series, days := make([]string, len(chunk)), make([]time.Time, len(chunk))
		for index, key := range chunk {
			series[index], days[index] = key.series, key.day
		}
		if _, err := writeZeroRows(ctx, w.Tx, `
			select d.project_key, d.eval_id, d.series, d.day,
			       lead(d.day) over series_days as next_day,
			       last_value(d.listed) over (series_days rows between unbounded preceding and unbounded following) as listed
			from unnest($6::text[], $7::date[]) as touched(series, first)
			join eval_days d on d.project_key = $1 and d.eval_id = $5 and d.writer = $3 and d.deploy_repo = ''
			  and d.series = touched.series and d.repo = '' and d.day >= touched.first
			window series_days as (partition by d.eval_id, d.series order by d.day)
		`, spans, definition.ProjectKey, today, writerReadings, builtBy, definition.EvalID, series, days); err != nil {
			return fmt.Errorf("write the zero rows of %s: %w", definition.EvalID, err)
		}
	}
	return nil
}

// loggedZeros is the statement that writes a logged eval's zero rows, which the readings write
// (writeLoggedZeros) and the refresher's fill (fillLoggedZeros) both send, so the two write one row
// of a day alike: after each stored row stored answers, the zero row of every day up to its series'
// next stored row, or through today ($2) after its newest, of the project $1, the writer $3 and
// the build $4. The zero row is the row the readings write leaves where every value is taken away:
// n 0, a zero sum, numerator and denominator, no sample, no maximum and no last value, so a later
// reading of that day adds to it as to any row; and it takes the listing of the series' newest row.
// A day that holds a row keeps it. stored answers, of each all-repositories row of writer $3 it
// reads, its project, eval, series and day, the next day the series holds a row of (next_day) and
// the listing of the series' newest row (listed). The rows go in the byte order of their keys
// (eval, series, day), the order every statement that inserts or locks one eval's day rows takes
// them in (compareDayKeys); left to the database's collation, a series `latency` and a series
// `Queue` would be taken in the other order. It answers, of each eval it wrote rows of, the first
// and the last day it wrote and how many rows, so the most rows one write owes come back as three
// numbers.
func loggedZeros(stored string) string {
	return `
		with written as (
			insert into eval_days (project_key, eval_id, writer, series, repo, day, listed, n, sum, num, den, samples, built_by)
			select stored.project_key, stored.eval_id, $3::text, stored.series, '', gap.day::date, stored.listed, 0, 0, 0, 0, '{}', $4
			from (` + stored + `) stored
			cross join generate_series((stored.day + 1)::timestamp,
				least(coalesce(stored.next_day - 1, $2::date), $2::date)::timestamp, interval '1 day') as gap(day)
			order by stored.eval_id collate "C", stored.series collate "C", gap.day
			on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do nothing
			returning eval_id, day
		)
		select eval_id, min(day), max(day), count(*) from written group by eval_id`
}

// writeZeroRows sends loggedZeros over stored with args in tx, adds to spans the days it wrote of
// each eval, and answers how many rows it wrote.
func writeZeroRows(ctx context.Context, tx pgx.Tx, stored string, spans map[string]daySpan, args ...any) (int64, error) {
	rows, err := tx.Query(ctx, loggedZeros(stored), args...)
	if err != nil {
		return 0, err
	}
	var written, count int64
	var evalID string
	var first, last time.Time
	if _, err := pgx.ForEachRow(rows, []any{&evalID, &first, &last, &count}, func() error {
		addSpan(spans, evalID, measures.Midnight(first), measures.Midnight(last))
		written += count
		return nil
	}); err != nil {
		return 0, err
	}
	return written, nil
}

// pastDaySamples is the row of the batch that makes a day row hold more than MaxDaySamples values,
// base being the row before the parts given are added to it (addParts): the first of the write's
// own parts at or after the position whose value would be one past the bound, or blame, the first
// row of the batch that touches the row, where none is. Of a refold the parts given are the
// standing readings' and then the write's own, so the write's own come last.
func pastDaySamples(base DayRow, given []dayPart, blame int) int {
	for _, part := range given[min(max(0, MaxDaySamples-len(base.Samples)), len(given)):] {
		if part.row >= 0 {
			return part.row
		}
	}
	return blame
}

// dayRowName is row as a refusal names it: its series, its repository where it is a repository's
// row, and its day.
func dayRowName(row DayRow) string {
	if row.Repo == "" {
		return fmt.Sprintf("series %q on %s", row.Series, row.Day.Format(time.DateOnly))
	}
	return fmt.Sprintf("series %q in repository %q on %s", row.Series, row.Repo, row.Day.Format(time.DateOnly))
}

// dayUpdateArrays is how many arrays writeDays' update takes (dayUpdates): one for each key column
// and each column it writes of a row, and one more, the rows' samples as one flat list, beside its
// four scalars (the project, the eval, the writer and the build).
const dayUpdateArrays = 15

// dayUpdateBytes is the most bytes row adds to the parameters of writeDays' update, as pgx encodes
// them in binary: in each array but the samples' 4 bytes of length and the element, its series,
// repository and last reading their bytes, its listing 1 and each of its ten numbers, dates and
// times (day, n, sum, num, den, max, last value, last_at, first and last_recorded_at) at most 8; and
// for each of its samples 4 bytes of length and 8 of value.
func dayUpdateBytes(row *DayRow) int {
	return (dayUpdateArrays-1)*4 + len(row.Series) + len(row.Repo) + 1 + len(row.LastReading) + 10*8 + (4+8)*len(row.Samples)
}

// MaxDaySamples is the most values one day row of a logged eval holds (CodeReadingDaySamplesLimit):
// the most whose row, with every key and the build at their largest, writeDays' update sends in one
// statement within statementBytes (dayUpdates). A row keeps every value of its day (samples), and
// its update sends them all, 12 bytes a value, so a readings write that would leave a row holding
// more, where it held fewer, is refused, and no statement that writes a day row passes the bound.
// It is the bound less the statement's own parameters, paramBytes(dayUpdateArrays, project, eval,
// writerReadings, build), 4 bytes of length before each of its 19 parameters, an array header for
// each of its 15 arrays, a project key (maxProjectKeyBytes), an eval id (maxEvalIDBytes), the
// writer and a build of at most maxKeyBytes (figureRowReserve says why), 1,429 in all; less the
// row's bytes but its samples, dayUpdateBytes with a series and a repository of maxKeyBytes each
// and a last reading's uuid of 36 bytes, 1,197; over the 12 bytes of a value: (262,144 - 1,429 -
// 1,197) / 12 = 21,626.
const MaxDaySamples = (statementBytes -
	(4*(dayUpdateArrays+4) + arrayHeaderBytes*dayUpdateArrays + maxProjectKeyBytes + maxEvalIDBytes + len(writerReadings) + maxKeyBytes) -
	((dayUpdateArrays-1)*4 + 2*maxKeyBytes + 1 + 36 + 10*8)) / (4 + 8)

// dayUpdates is the arguments of each statement writeDays' update sends of rows, each row's next
// state in the byte order of their keys, recording builtBy: rows cut into chunks whose parameters
// take at most statementBytes as statementChunks cuts them, the statement's own estimated by
// paramBytes and each row's by dayUpdateBytes, each statement's arrays one chunk. An array
// parameter holds no arrays of differing lengths, so a chunk's samples go as one flat list, each
// row's the n of them from its first's position on.
func dayUpdates(project, evalID, builtBy string, rows []DayRow) iter.Seq[[]any] {
	return func(yield func([]any) bool) {
		for chunk := range statementChunks(rows, paramBytes(dayUpdateArrays, project, evalID, writerReadings, builtBy), dayUpdateBytes) {
			count, held := len(chunk), 0
			for index := range chunk {
				held += len(chunk[index].Samples)
			}
			seriesNames, repos, days, listed := make([]string, count), make([]string, count), make([]time.Time, count), make([]bool, count)
			counts, firsts := make([]int, count), make([]int, count)
			sums, nums, dens, maxes := make([]*float64, count), make([]*float64, count), make([]*float64, count), make([]*float64, count)
			lastValues, lastAts, lastRecordedAts := make([]*float64, count), make([]*time.Time, count), make([]*time.Time, count)
			lastReadings := make([]string, count)
			// Empty rather than nil, which pgx would send as null, and a slice of null is null where a
			// row with no value holds the empty list.
			samples := make([]float64, 0, held)
			for index := range chunk {
				row := &chunk[index]
				seriesNames[index], repos[index], days[index], listed[index] = row.Series, row.Repo, row.Day, row.Listed
				counts[index], firsts[index] = row.N, len(samples)+1
				sums[index], nums[index], dens[index], maxes[index] = row.Sum, row.Num, row.Den, row.Max
				lastValues[index], lastAts[index], lastRecordedAts[index], lastReadings[index] = row.LastValue, row.LastAt, row.LastRecordedAt, row.LastReading
				samples = append(samples, row.Samples...)
			}
			if !yield([]any{project, evalID, writerReadings, builtBy, seriesNames, repos, days, listed, counts, sums, nums,
				dens, maxes, lastValues, samples, lastAts, firsts, lastRecordedAts, lastReadings}) {
				return
			}
		}
	}
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
// adds to may hold the reading of a write that began after this one and committed first. Each
// value's product with its weight is converted to float64, which rounds it before the sum it feeds
// (num): the Go spec lets a build fuse a product with the sum it feeds into one instruction that
// rounds once, and an arm64 build does, so without the conversion a row's num would differ by
// machine (0.1 over an n of 1 and then 0.3 over an n of 3 sum to 0.9999999999999999 rounded at each
// step, 1 fused).
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
		num += float64(part.value * part.weight)
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
// have a value and were written while the eval was logged (writtenWhileLogged). A statement reads
// the keys' (standingPartsQuery): the eval's standing readings of each day the keys name, once a
// day, through eval_readings_row with that day's range in its index condition, each then read by
// its id and matched to the keys of its day. Joined to every key at once instead, the readings
// were planned, for a week's keys, as one scan of the eval that each key then filtered whole. The
// keys grow with the batch, so they go in statements of at most statementBytes of parameters, cut
// as writeDays' insert cuts them (insertBytes, whose four arrays take more than this statement's
// three), in their order, each statement's rows in its keys' order. The write's own readings,
// stored before this runs, come back among them and are left out here: the ids written are as many
// as the batch's rows, and a statement sending them would grow with it.
func standingParts(ctx context.Context, tx pgx.Tx, evalID string, keys []dayKey, written []string) (map[dayKey][]dayPart, error) {
	standing := map[dayKey][]dayPart{}
	if len(keys) == 0 {
		return standing, nil
	}
	own := make(map[string]bool, len(written))
	for _, id := range written {
		own[id] = true
	}
	var at int
	var value, weight float64
	var measured, recorded time.Time
	var id string
	for chunk := range statementChunks(keys, paramBytes(3, evalID), (*dayKey).insertBytes) {
		series, repos, days := make([]string, len(chunk)), make([]string, len(chunk)), make([]time.Time, len(chunk))
		for index, key := range chunk {
			series[index], repos[index], days[index] = key.series, key.repo, key.day
		}
		rows, err := tx.Query(ctx, standingPartsQuery, evalID, series, repos, days)
		if err != nil {
			return nil, fmt.Errorf("read the standing readings of %d day rows of %s: %w", len(chunk), evalID, err)
		}
		if _, err := pgx.ForEachRow(rows, []any{&at, &value, &weight, &measured, &recorded, &id}, func() error {
			if !own[id] {
				key := chunk[at-1]
				standing[key] = append(standing[key], dayPart{value: value, weight: weight, at: measured.UTC(), recordedAt: recorded, id: id, row: -1})
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the standing readings of %d day rows of %s: %w", len(chunk), evalID, err)
		}
	}
	return standing, nil
}

// standingPartsQuery is standingParts' statement: $1 the eval and $2 to $4 the keys (series,
// repository, day). standing is the ids of the standing readings of the keys' days, materialized,
// so each day's readings are read once, through the index, whatever plan the joins after it take.
// Each reading is then matched to its keys by series and by the day itself, the UTC midnight its
// measured_at falls in, which every key's day is (dayKeysOf's measures.Midnight, or a date of
// eval_days): a hash join keys on it, where a match on the range of each key's day tested each
// reading against every key of its series.
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
	)
	select k.at, r.value, coalesce(r.n, 1)::float8, r.measured_at, r.recorded_at, r.id::text
	from standing
	join eval_readings r on r.id = standing.id
	join k on k.series = r.series and k.day = date_trunc('day', r.measured_at, 'UTC')
		and (k.repo = '' or r.attributes ->> 'repo' = k.repo)
	order by k.at, r.recorded_at, r.id
`
