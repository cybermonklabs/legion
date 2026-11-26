package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// good is a reading every refusal test batches beside the row it refuses, so each refusal is of
// one row of a batch that also holds a row the registry would store.
func good(t *testing.T) json.RawMessage {
	return row(t, map[string]any{"series": "score", "row_kind": "run", "row_key": "run-1", "value": 0.8, "measured_at": at(3, 9, 0)})
}

// TestEachBadRowIsRefusedByItsIndexAndCode: a batch whose second row breaks one rule of a reading
// is refused whole, and the refusal names that row's index and the rule's code, for every code
// the readings write refuses with.
func TestEachBadRowIsRefusedByItsIndexAndCode(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// A reading another correction replaced already, for READING_ALREADY_REPLACED.
	first := r.write(t, "accuracy", alice, good(t))
	replaced := first.Readings[0].ID
	r.write(t, "accuracy", alice, row(t, map[string]any{
		"series": "score", "row_kind": "run", "row_key": "run-1", "value": 0.7, "measured_at": at(3, 9, 0), "replaces": replaced,
	}))
	other := r.write(t, "other-eval", alice, good(t)).Readings[0].ID
	standing := r.write(t, "accuracy", alice, good(t)).Readings[0].ID

	for _, test := range []struct {
		code  string
		field string
		rows  []json.RawMessage
		index int
	}{
		{CodeValueOrReason, "value", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "measured_at": at(3, 10, 0),
		})}, 1},
		{CodeFieldServerSet, "recorded_by", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "recorded_by": map[string]any{"kind": "user", "id": "mallory"},
		})}, 1},
		{CodeFieldServerSet, "recorded_at", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "recorded_at": at(1, 0, 0),
		})}, 1},
		{CodeFieldServerSet, "links[0].recorded_at", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
			"links": []any{map[string]any{"rel": "via", "href": "https://ci.example/run/1", "recorded_at": at(1, 0, 0)}},
		})}, 1},
		{CodeReplacesUnknown, "replaces", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "replaces": uuid.NewString(),
		})}, 1},
		{CodeReplacesUnknown, "replaces", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "replaces": other,
		})}, 1},
		{CodeAlreadyReplaced, "replaces", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "replaces": replaced,
		})}, 1},
		{CodeAlreadyReplaced, "replaces", []json.RawMessage{
			row(t, map[string]any{"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "replaces": standing}),
			row(t, map[string]any{"series": "score", "row_kind": "run", "value": 2, "measured_at": at(3, 10, 0), "replaces": standing}),
		}, 1},
		{CodeLinkHrefNotAbsolute, "links[0].href", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
			"links": []any{map[string]any{"rel": "via", "href": "/runs/1"}},
		})}, 1},
		{CodeLinkHrefNotAbsolute, "links[0].href", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
			"links": []any{map[string]any{"rel": "via", "label": "no address"}},
		})}, 1},
		{CodeLinkRelMissing, "links[0].rel", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
			"links": []any{map[string]any{"href": "https://ci.example/run/1"}},
		})}, 1},
		{CodeReadingSeriesInvalid, "series", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "two words", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
		})}, 1},
		{CodeReadingSeriesInvalid, "series", []json.RawMessage{good(t), row(t, map[string]any{
			"row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
		})}, 1},
		{CodeReadingInvalid, "measured_at", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1,
		})}, 1},
		{CodeReadingInvalid, "colour", []json.RawMessage{good(t), row(t, map[string]any{
			"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0), "colour": "red",
		})}, 1},
	} {
		t.Run(test.code+"/"+test.field, func(t *testing.T) {
			before := r.count(t, `select count(*) from eval_readings`)
			_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: "accuracy", Rows: test.rows, Caller: alice, BuiltBy: testBuild})
			var refused *Error
			if !errors.As(err, &refused) {
				t.Fatalf("the batch was answered %v, want a refusal naming row %d %s", err, test.index, test.code)
			}
			if refused.Code != test.code {
				t.Errorf("the refusal's code is %s, want %s, the code of its first refused row", refused.Code, test.code)
			}
			named := slices.ContainsFunc(refused.Rows, func(row RowError) bool {
				return row.Index == test.index && row.Code == test.code && row.Field == test.field
			})
			if !named {
				t.Errorf("the refusal names %+v, want row %d refused %s on %s", refused.Rows, test.index, test.code, test.field)
			}
			if after := r.count(t, `select count(*) from eval_readings`); after != before {
				t.Errorf("the refused batch moved eval_readings from %d rows to %d", before, after)
			}
			t.Logf("refused %s: %s; rows %+v", refused.Code, refused.Message, refused.Rows)
		})
	}
}

// TestAReadingsNIsHeldWholeToTheLargestInt64: n is the one whole number a readings write takes,
// and eval_readings holds it as a bigint, as wide as the int64 a row's n is decoded into. So the
// largest int64 is stored whole, and an n past it is refused READING_INVALID naming n, and stores
// nothing, rather than reaching the store.
func TestAReadingsNIsHeldWholeToTheLargestInt64(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	written := r.write(t, "sizes", alice, row(t, map[string]any{
		"series": "score", "row_kind": "run", "value": 1, "n": int64(math.MaxInt64), "measured_at": at(3, 9, 0),
	}))
	var stored int64
	if err := r.database.Pool.QueryRow(t.Context(), `select n from eval_readings where id = $1`, written.Readings[0].ID).Scan(&stored); err != nil {
		t.Fatalf("read the stored n: %v", err)
	}
	if stored != math.MaxInt64 {
		t.Errorf("n %d was stored as %d", int64(math.MaxInt64), stored)
	}
	before := r.count(t, `select count(*) from eval_readings`)
	_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: "sizes", Caller: alice, BuiltBy: testBuild, Rows: []json.RawMessage{
		row(t, map[string]any{"series": "score", "row_kind": "run", "value": 1, "n": json.Number("9223372036854775808"), "measured_at": at(3, 10, 0)}),
	}})
	var refused *Error
	if !errors.As(err, &refused) || !slices.ContainsFunc(refused.Rows, func(row RowError) bool { return row.Code == CodeReadingInvalid && row.Field == "n" }) {
		t.Fatalf("n 9223372036854775808 was answered %v, want row 0 refused %s on n", err, CodeReadingInvalid)
	}
	if after := r.count(t, `select count(*) from eval_readings`); after != before {
		t.Errorf("the refused n moved eval_readings from %d rows to %d", before, after)
	}
}

// TestAReadingMeasuredOutsideItsRangeIsRefusedNamingItsRow: a reading's measured_at lies from
// 2025-01-01T00:00:00Z (MeasuredAtFloor) to 24 hours after the server's time (R18). A row measured
// at 0001-01-01, as the reading was that made one refresher pass write 739,934 zero rows, or a
// second before the floor, or an hour past the ceiling, or in the year 10000, is refused
// MEASURED_AT_OUT_OF_RANGE naming its row, and its batch stores nothing. A row at the floor itself
// and one an hour inside the ceiling are stored.
func TestAReadingMeasuredOutsideItsRangeIsRefusedNamingItsRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	now := time.Now().UTC()
	measuredAt := func(measured any) json.RawMessage {
		return row(t, map[string]any{"series": "score", "row_kind": "run", "value": 1, "measured_at": measured})
	}
	for _, measured := range []any{"0001-01-01T00:00:00Z", "2024-12-31T23:59:59Z", now.Add(MeasuredAtLead + time.Hour), "9999-12-31T23:59:59-14:00"} {
		before := r.count(t, `select count(*) from eval_readings`)
		_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: "accuracy", Caller: alice, BuiltBy: testBuild,
			Rows: []json.RawMessage{good(t), measuredAt(measured)}})
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != CodeMeasuredAtOutOfRange || len(refused.Rows) != 1 ||
			refused.Rows[0].Index != 1 || refused.Rows[0].Field != "measured_at" {
			t.Errorf("a reading measured at %v was answered %v (%+v), want %s naming row 1's measured_at", measured, err, refused, CodeMeasuredAtOutOfRange)
			continue
		}
		if after := r.count(t, `select count(*) from eval_readings`); after != before {
			t.Errorf("the refused batch measured at %v moved eval_readings from %d rows to %d", measured, before, after)
		}
		t.Logf("refused %v: %s", measured, refused.Rows[0].Message)
	}
	written := r.write(t, "accuracy", alice, measuredAt(MeasuredAtFloor), measuredAt(now.Add(MeasuredAtLead-time.Hour)))
	if len(written.Readings) != 2 {
		t.Errorf("the readings at the floor and inside the ceiling stored %d readings, want both", len(written.Readings))
	}
}

// TestAKeyAtItsBoundIsStoredAndOnePastItRefused: a key holds at most maxKeyBytes bytes, which keeps
// every index entry of the registry inside the 2,704 bytes a B-tree entry holds. A reading whose
// eval's slug, series, row_kind, row_key and attributes.repo each hold maxKeyBytes bytes that do
// not compress is stored with its day row, the widest entries the registry makes. One byte more
// in one of the reading's keys is refused EVAL_KEY_TOO_LONG naming its row and field, and in a
// slug or a register write's series naming the field; none of them stores anything.
func TestAKeyAtItsBoundIsStoredAndOnePastItRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// Hex of a multiplicative hash, which repeats nothing a compressor could use.
	var digits strings.Builder
	for index := 0; digits.Len() <= maxKeyBytes; index++ {
		fmt.Fprintf(&digits, "%08x", uint32(index)*2654435761)
	}
	key, long := digits.String()[:maxKeyBytes], digits.String()[:maxKeyBytes+1]
	reading := func(series, rowKind, rowKey, repo string) json.RawMessage {
		return row(t, map[string]any{"series": series, "row_kind": rowKind, "row_key": rowKey, "value": 1, "measured_at": at(3, 9, 0),
			"attributes": map[string]any{"repo": repo}})
	}
	written := r.write(t, key, alice, reading(key, key, key, key))
	if days := r.count(t, `select count(*) from eval_days where eval_id = $1 and series = $2 and repo = $2`, EvalID(r.project, key), key); len(written.Readings) != 1 || days != 1 {
		t.Fatalf("the reading whose keys are each %d bytes stored %d readings and %d day rows, want one of each", maxKeyBytes, len(written.Readings), days)
	}
	readings, versions := r.count(t, `select count(*) from eval_readings`), r.count(t, `select count(*) from eval_versions`)
	for _, test := range []struct {
		field string
		row   json.RawMessage
	}{
		{"series", reading(long, key, key, key)},
		{"row_kind", reading(key, long, key, key)},
		{"row_key", reading(key, key, long, key)},
		{"attributes.repo", reading(key, key, key, long)},
	} {
		_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: key, Caller: alice, BuiltBy: testBuild, Rows: []json.RawMessage{good(t), test.row}})
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != CodeKeyTooLong || len(refused.Rows) != 1 || refused.Rows[0].Index != 1 || refused.Rows[0].Field != test.field {
			t.Errorf("a reading whose %s holds %d bytes was answered %v (%+v), want %s naming row 1's %s", test.field, len(long), err, refused, CodeKeyTooLong, test.field)
		}
	}
	_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: long, Caller: alice, BuiltBy: testBuild, Rows: []json.RawMessage{good(t)}})
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != CodeKeyTooLong || refused.Field != "slug" {
		t.Errorf("a readings write under a slug of %d bytes was answered %v (%+v), want %s naming the slug", len(long), err, refused, CodeKeyTooLong)
	}
	for _, write := range []struct {
		field  string
		change Registration
	}{
		{"slug", Registration{Slug: long}},
		{"series[0].series", Registration{Slug: key, Series: []SeriesChange{{Series: long}}}},
	} {
		change := write.change
		change.Project, change.Caller = r.project, alice
		_, err := r.registry.Register(t.Context(), change)
		if !errors.As(err, &refused) || refused.Code != CodeKeyTooLong || refused.Field != write.field {
			t.Errorf("a register write whose %s holds %d bytes was answered %v (%+v), want %s naming it", write.field, len(long), err, refused, CodeKeyTooLong)
		}
	}
	if afterReadings, afterVersions := r.count(t, `select count(*) from eval_readings`), r.count(t, `select count(*) from eval_versions`); afterReadings != readings || afterVersions != versions {
		t.Errorf("the refused writes moved the readings from %d to %d and the versions from %d to %d", readings, afterReadings, versions, afterVersions)
	}
}

// TestABatchWithABadLastRowStoresNothing: a batch of three readings whose last row carries the
// recorder is refused whole: eval_readings holds as many rows after it as before, and the slug
// it named, which no eval held, is not registered either.
func TestABatchWithABadLastRowStoresNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "latency", alice, row(t, map[string]any{"series": "ms", "row_kind": "request", "value": 12, "measured_at": at(2, 8, 0)}))

	for _, slug := range []string{"latency", "unregistered"} {
		before := r.count(t, `select count(*) from eval_readings`)
		versions := r.count(t, `select count(*) from eval_versions`)
		_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild, Rows: []json.RawMessage{
			row(t, map[string]any{"series": "ms", "row_kind": "request", "value": 10, "measured_at": at(3, 8, 0)}),
			row(t, map[string]any{"series": "ms", "row_kind": "request", "value": 11, "measured_at": at(3, 9, 0)}),
			row(t, map[string]any{"series": "ms", "row_kind": "request", "value": 13, "measured_at": at(3, 10, 0), "recorded_by": alice}),
		}})
		after := r.count(t, `select count(*) from eval_readings`)
		t.Logf("%s: eval_readings held %d rows before the batch and %d after it; the write answered %v", slug, before, after, err)
		var refused *Error
		if !errors.As(err, &refused) || len(refused.Rows) != 1 || refused.Rows[0].Index != 2 {
			t.Errorf("%s: the batch was answered %v, want a refusal of row 2 alone", slug, err)
		}
		if after != before {
			t.Errorf("%s: the refused batch moved eval_readings from %d rows to %d", slug, before, after)
		}
		if moved := r.count(t, `select count(*) from eval_versions`); moved != versions {
			t.Errorf("%s: the refused batch moved eval_versions from %d rows to %d", slug, versions, moved)
		}
	}
	// The first write left the zero rows of the days after its reading's (writeLoggedZeros), this
	// day's among them, which a stored batch would have added its readings to.
	if days := r.count(t, `select count(*) from eval_days where day = '2026-11-03' and n > 0`); days != 0 {
		t.Errorf("the refused batches left %d day rows holding readings on their day, want none", days)
	}
}

// TestABatchThatWouldOverflowADayRowIsRefusedNamingItsRow: a batch whose readings would take a sum
// of a logged eval's day row past what a float holds is refused whole, naming the row whose value
// does it: the second of two readings of 1e308 on one day (the day's sum), a reading of 1e300
// measured over an n of 1e10 (its weighted sum, num), and a reading of 1e308 on a day an earlier
// write left holding one. Each stores no reading, registers no eval and moves no day row; the same
// values spread over days whose sums stay within a float are stored.
func TestABatchThatWouldOverflowADayRowIsRefusedNamingItsRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	huge := func(key string, value float64, n int64, measured time.Time) json.RawMessage {
		return row(t, map[string]any{"series": "bytes", "row_kind": "run", "row_key": key, "value": value, "n": n, "measured_at": measured})
	}
	held := r.write(t, "held", alice, huge("held", 1e308, 1, at(4, 8, 0))).Definition.EvalID
	for _, test := range []struct {
		what, slug string
		rows       []json.RawMessage
		index      int
	}{
		{"two readings of 1e308 on one day", "fresh", []json.RawMessage{
			good(t), huge("a", 1e308, 1, at(5, 8, 0)), huge("b", 1e308, 1, at(5, 9, 0)),
		}, 2},
		{"a reading of 1e300 over an n of 1e10", "fresh", []json.RawMessage{good(t), huge("c", 1e300, 1e10, at(5, 8, 0))}, 1},
		{"a reading of 1e308 on a day that holds one", "held", []json.RawMessage{huge("d", 1e308, 1, at(4, 9, 0))}, 0},
	} {
		readings, versions := r.count(t, `select count(*) from eval_readings`), r.count(t, `select count(*) from eval_versions`)
		_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: test.slug, Rows: test.rows, Caller: alice, BuiltBy: testBuild})
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != CodeReadingDayOverflow || len(refused.Rows) != 1 ||
			refused.Rows[0].Index != test.index || refused.Rows[0].Code != CodeReadingDayOverflow || refused.Rows[0].Field != "value" {
			t.Errorf("%s: the batch was answered %v, want a refusal of row %d alone, %s on its value", test.what, err, test.index, CodeReadingDayOverflow)
		} else {
			t.Logf("%s: refused %s: %s; rows %+v", test.what, refused.Code, refused.Message, refused.Rows)
		}
		if after := r.count(t, `select count(*) from eval_readings`); after != readings {
			t.Errorf("%s: the refused batch moved eval_readings from %d rows to %d", test.what, readings, after)
		}
		if after := r.count(t, `select count(*) from eval_versions`); after != versions {
			t.Errorf("%s: the refused batch moved eval_versions from %d rows to %d", test.what, versions, after)
		}
	}
	if day := r.day(t, held, "bytes", "", at(4, 0, 0)); day.N != 1 || *day.Sum != 1e308 {
		t.Errorf("the day an earlier write left holding 1e308 holds n %d, sum %v after the refused batch; want 1 and 1e308", day.N, *day.Sum)
	}
	spread := r.write(t, "fresh", alice, huge("a", 1e308, 1, at(5, 8, 0)), huge("b", 1e308, 1, at(6, 8, 0)))
	if len(spread.Readings) != 2 {
		t.Errorf("the readings spread over two days stored %d readings, want 2", len(spread.Readings))
	}
}

// TestAReadingsWriteThatWouldPassTheSeriesBoundIsRefusedNamingItsRow: a logged eval holds four
// series fewer than MaxEvalSeries. A batch naming six series it does not hold, two of them twice,
// beside series it holds, is refused (CodeSeriesLimit) naming its one row whose series first passes
// the bound, the bound, and how many series the eval would hold, and stores nothing: no reading and
// no series. The batch less the two series past the bound is taken, and the eval then holds
// MaxEvalSeries series; a batch of series it holds is taken, and one naming one more series is
// refused at that row.
func TestAReadingsWriteThatWouldPassTheSeriesBoundIsRefusedNamingItsRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	const slug = "series-bound"
	evalID := EvalID(r.project, slug)
	change := Registration{Project: r.project, Slug: slug, Caller: alice}
	for index := range MaxEvalSeries - 4 {
		change.Series = append(change.Series, SeriesChange{Series: fmt.Sprintf("held-%03d", index)})
	}
	if _, err := r.registry.Register(t.Context(), change); err != nil {
		t.Fatalf("register %d series of %s: %v", len(change.Series), slug, err)
	}
	reading := func(series string, index int) json.RawMessage {
		return row(t, map[string]any{"series": series, "row_kind": "run", "row_key": fmt.Sprint(index), "value": float64(index), "measured_at": at(3, 9, index)})
	}
	batch := func(names ...string) []json.RawMessage {
		rows := make([]json.RawMessage, len(names))
		for index, name := range names {
			rows[index] = reading(name, index)
		}
		return rows
	}
	countSeries := `select count(distinct series) from eval_series where eval_id = $1`
	series := func() int { return r.count(t, countSeries, evalID) }
	for _, test := range []struct {
		what    string
		names   []string
		refused int
		holds   int
		would   int
	}{
		{"six new series, the fifth and sixth past the bound", []string{"held-000", "new-a", "new-b", "new-a", "new-c", "new-d", "new-e", "held-001", "new-f", "new-e"},
			6, MaxEvalSeries - 4, MaxEvalSeries + 2},
		{"the four new series the bound takes", []string{"held-000", "new-a", "new-b", "new-a", "new-c", "new-d", "held-001"}, -1, MaxEvalSeries, 0},
		{"series the eval holds", []string{"held-002", "new-d", "new-a"}, -1, MaxEvalSeries, 0},
		{"one series more", []string{"held-002", "new-g", "new-a"}, 1, MaxEvalSeries, MaxEvalSeries + 1},
	} {
		readings := r.count(t, `select count(*) from eval_readings where eval_id = $1`, evalID)
		_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: slug, Rows: batch(test.names...), Caller: alice, BuiltBy: testBuild})
		stored := r.count(t, `select count(*) from eval_readings where eval_id = $1`, evalID) - readings
		if test.refused < 0 {
			if err != nil || stored != len(test.names) || series() != test.holds {
				t.Errorf("%s: answered %v, %d readings stored and the eval holding %d series, want %d stored and %d series", test.what, err, stored, series(), len(test.names), test.holds)
			}
			continue
		}
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != CodeSeriesLimit || len(refused.Rows) != 1 {
			t.Fatalf("%s: answered %v, want %s naming one row", test.what, err, CodeSeriesLimit)
		}
		one := refused.Rows[0]
		if one.Index != test.refused || one.Code != CodeSeriesLimit || one.Field != "series" ||
			!strings.Contains(one.Message, fmt.Sprintf("at most %d series", MaxEvalSeries)) || !strings.Contains(one.Message, fmt.Sprintf("holding %d", test.would)) {
			t.Errorf("%s: row %d is refused %s on %q: %s; want row %d, %s on series, naming the bound %d and the %d series the eval would hold",
				test.what, one.Index, one.Code, one.Field, one.Message, test.refused, CodeSeriesLimit, MaxEvalSeries, test.would)
		}
		if stored != 0 || series() != test.holds {
			t.Errorf("%s: %d readings stored and the eval holding %d series after the refusal, want none stored and %d series", test.what, stored, series(), test.holds)
		}
		t.Logf("%s: refused row %d: %s", test.what, one.Index, one.Message)
	}
}

// dayKeyOrder answers the positions, from 0, of the day keys its arrays hold (series, repository
// and day, each array a column of the keys), in the byte order of the keys, every text `collate
// "C"`: the order every statement that inserts or locks one eval's day rows takes them in
// (compareDayKeys).
const dayKeyOrder = `
	select (k.sent - 1)::int from unnest($1::text[], $2::text[], $3::date[]) with ordinality as k(series, repo, day, sent)
	order by k.series collate "C", k.repo collate "C", k.day`

// TestAReadingsWritesDayRowsGoInBoundedStatementsInKeyOrder: a readings write of 190 readings on
// one day, each of its own series and its own repository (attributes.repo), each name the most
// bytes a key holds, touches 380 day rows, whose keys one insert would send in more than
// statementBytes of parameters. writeDays' insert, lock and update each go in at least two
// statements, and no statement of the write takes more than the bound as pgx encodes its
// parameters as the types the server describes (the update's as dayUpdateOIDs, and within its
// estimate); the inserts, the locks and the updates each take every key once, in the byte order of
// the keys across their statements, the order Postgres's `collate "C"` gives them (dayKeyOrder);
// every row holds its one reading; and the write logs one eval_days change of the eval, spanning
// the day and the zero rows it writes of every day after it through today, as one statement does
// (migration 20261117100014).
func TestAReadingsWritesDayRowsGoInBoundedStatementsInKeyOrder(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	statements := &sentStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	const slug, count = "wide-keys", 190
	evalID := EvalID(r.project, slug)
	day := at(3, 0, 0)
	// Of names that start with "a", "A", "b" and "B", a collation that folds case orders some the
	// other way from their bytes.
	name := func(kind string, index int) string {
		prefix := fmt.Sprintf("%c%s%03d-", "aAbB"[index%4], kind, index)
		return prefix + strings.Repeat("x", maxKeyBytes-len(prefix))
	}
	rows := make([]json.RawMessage, count)
	var want []dayKey
	values := map[dayKey]float64{}
	for index := range count {
		series, repo, value := name("s", index), name("r", index), float64(index)+0.25
		rows[index] = row(t, map[string]any{"series": series, "row_kind": "run", "row_key": fmt.Sprint(index), "value": value,
			"measured_at": day.Add(time.Duration(index) * time.Second), "attributes": map[string]any{"repo": repo}})
		for _, key := range []dayKey{{series: series, day: day}, {series: series, repo: repo, day: day}} {
			want, values[key] = append(want, key), value
		}
	}
	slices.SortFunc(want, compareDayKeys)
	whole := paramBytes(4, r.project, evalID, writerReadings, testBuild)
	for index := range want {
		whole += want[index].insertBytes()
	}
	if whole <= statementBytes {
		t.Fatalf("one insert of the %d keys would take an estimated %d bytes of parameters, and the check needs more than %d", len(want), whole, statementBytes)
	}
	heads := r.changeHeads(t)
	r.write(t, slug, alice, rows...)
	sent := r.measured(t, statements.take())
	for _, statement := range sent {
		if statement.bytes > statementBytes {
			t.Errorf("the write sent %.80q with %d bytes of parameters as pgx encodes them, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
	}
	for _, kind := range []struct {
		name string
		// marks is what the statement's SQL holds, each of them.
		marks []string
		// at is the index of the statement's series, after which come its repositories and days.
		at int
	}{
		{"insert", []string{"insert into eval_days", "k(series, repo, day, listed)"}, 4},
		{"lock", []string{"from eval_days", "for update"}, 3},
		{"update", []string{"update eval_days"}, 4},
	} {
		var series, repos []string
		var days []time.Time
		var sizes []int
		for _, statement := range sent {
			if slices.ContainsFunc(kind.marks, func(mark string) bool { return !strings.Contains(statement.sql, mark) }) {
				continue
			}
			series, repos = append(series, statement.args[kind.at].([]string)...), append(repos, statement.args[kind.at+1].([]string)...)
			days, sizes = append(days, statement.args[kind.at+2].([]time.Time)...), append(sizes, statement.bytes)
			if kind.name == "update" {
				if !slices.Equal(statement.oids, dayUpdateOIDs) {
					t.Errorf("the server describes the parameters of the update of day rows as %v, where the test encodes them as %v", statement.oids, dayUpdateOIDs)
				}
				if estimated := paramEstimate(t, statement.args); estimated > statementBytes || statement.bytes > estimated {
					t.Errorf("an update of day rows takes %d bytes of parameters as pgx encodes them and an estimated %d, where the bound is %d", statement.bytes, estimated, statementBytes)
				}
			}
		}
		got := make([]dayKey, len(series))
		for index := range got {
			got[index] = dayKey{series: series[index], repo: repos[index], day: days[index].UTC()}
		}
		if len(sizes) < 2 || !slices.Equal(got, want) {
			t.Errorf("the %ss of day rows went in %d statements of %v bytes taking %d keys, want at least two taking the %d keys in their byte order", kind.name, len(sizes), sizes, len(got), len(want))
			continue
		}
		if at := firstOutOfOrder(keyOrderOf(t, r.database.Pool, len(got), dayKeyOrder, series, repos, days)); at >= 0 {
			t.Errorf("the %ss of day rows take the key of series %.12q... and repository %.12q... at %d, out of the byte order Postgres's collate \"C\" gives them", kind.name, series[at], repos[at], at)
		}
		t.Logf("the %ss of day rows: %d statements of %v bytes of parameters, of %d", kind.name, len(sizes), sizes, statementBytes)
	}
	for _, key := range want {
		if stored := r.day(t, evalID, key.series, key.repo, day); stored.N != 1 || stored.Sum == nil || *stored.Sum != values[key] || !slices.Equal(stored.Samples, []float64{values[key]}) {
			t.Errorf("the day row of series %.12q... and repository %.12q... holds n %d, sum %v and samples %v, want its one reading %v", key.series, key.repo, stored.N, showValue(stored.Sum), stored.Samples, values[key])
		}
	}
	var clock time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select now()`).Scan(&clock); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	changes := r.loggedChanges(t, "eval_days", heads)
	if today := measures.Midnight(clock); len(changes) != 1 || changes[0].evalID != evalID || !changes[0].first.Equal(day) || !changes[0].last.Equal(today) {
		t.Errorf("the write logged the eval_days changes %v, want one of %s from %s to %s", changes, evalID, day.Format(time.DateOnly), today.Format(time.DateOnly))
	}
	largest, what := largestStatement(sent)
	t.Logf("%d statements, the largest %d bytes of parameters (%s), of %d; changes logged %v", len(sent), largest, what, statementBytes, changes)
}

// TestTheMostZeroRowsOneReadingsWriteOwesGoInBoundedStatements: the most zero rows one readings
// write owes, a first reading of each of the MaxEvalSeries series an eval holds, each name the most
// bytes a key holds, measured on MeasuredAtFloor, the earliest day a write takes, so that every
// series owes the zero row of every day after the floor through today. The stored figures are
// folded through today first, so the write folds the figures of the days it fills. The write sends
// no statement of more than statementBytes of parameters as pgx encodes them, leaves each series a
// row on every day from the floor through today, logs one eval_days change of the eval spanning
// them, and leaves the stored figures the fold of the rows that stand. It logs how long it took and
// how many day rows it wrote.
func TestTheMostZeroRowsOneReadingsWriteOwesGoInBoundedStatements(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	var clock time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select now()`).Scan(&clock); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	r.now = clock
	r.settle(t)
	statements := &sentStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	const slug = "floor-fill"
	evalID := EvalID(r.project, slug)
	name := func(index int) string {
		prefix := fmt.Sprintf("s%03d-", index)
		return prefix + strings.Repeat("x", maxKeyBytes-len(prefix))
	}
	rows := make([]json.RawMessage, MaxEvalSeries)
	for index := range rows {
		rows[index] = row(t, map[string]any{"series": name(index), "row_kind": "run", "row_key": fmt.Sprint(index), "value": 1.5,
			"measured_at": MeasuredAtFloor.Add(time.Duration(index) * time.Second)})
	}
	heads := r.changeHeads(t)
	began := time.Now()
	r.write(t, slug, alice, rows...)
	took := time.Since(began)
	sent := r.measured(t, statements.take())
	for _, statement := range sent {
		if statement.bytes > statementBytes {
			t.Errorf("the write sent %.80q with %d bytes of parameters as pgx encodes them, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
	}
	today := measures.Midnight(clock)
	days := int(today.Sub(MeasuredAtFloor).Hours()/24) + 1
	written := r.count(t, `select count(*) from eval_days where eval_id = $1`, evalID)
	zeros := r.count(t, `select count(*) from eval_days where eval_id = $1 and n = 0 and day > $2 and day <= $3`, evalID, MeasuredAtFloor, today)
	if want := MaxEvalSeries * days; written != want || zeros != want-MaxEvalSeries {
		t.Errorf("the write left %d day rows of %s, %d of them zero rows after the floor, want a row of each of its %d series on each of the %d days from %s through %s, all but the floor's zero rows",
			written, evalID, zeros, MaxEvalSeries, days, MeasuredAtFloor.Format(time.DateOnly), today.Format(time.DateOnly))
	}
	changes := r.loggedChanges(t, "eval_days", heads)
	if len(changes) != 1 || changes[0].evalID != evalID || !changes[0].first.Equal(MeasuredAtFloor) || !changes[0].last.Equal(today) {
		t.Errorf("the write logged the eval_days changes %v, want one of %s from %s to %s", changes, evalID, MeasuredAtFloor.Format(time.DateOnly), today.Format(time.DateOnly))
	}
	expectTruth(t, "after the write of the most zero rows", r.storedFigures(t), r.truthFigures(t))
	largest, what := largestStatement(sent)
	t.Logf("a first reading of each of %d series on %s wrote %d day rows through %s in %s: %d statements, the largest %d bytes of parameters (%s), of %d",
		MaxEvalSeries, MeasuredAtFloor.Format(time.DateOnly), written, today.Format(time.DateOnly), took, len(sent), largest, what, statementBytes)
}

// changeHeads is the newest pending and the newest drained change, before a write whose changes
// loggedChanges reads.
func (r rig) changeHeads(t *testing.T) [2]int {
	t.Helper()
	return [2]int{r.count(t, `select coalesce(max(id), 0) from delivery_changes_pending`), r.count(t, `select coalesce(max(seq), 0) from delivery_changes`)}
}

// loggedChanges is every change of kind logged after heads (changeHeads), pending or drained, as
// dayChange holds one: its project, its eval and its span.
func (r rig) loggedChanges(t *testing.T, kind string, heads [2]int) []dayChange {
	t.Helper()
	changed, err := r.database.Pool.Query(t.Context(), `
		select repo, ref, t_old, t_new from delivery_changes_pending where kind = $1 and id > $2
		union all
		select repo, ref, t_old, t_new from delivery_changes where kind = $1 and seq > $3`, kind, heads[0], heads[1])
	if err != nil {
		t.Fatalf("read the %s changes logged: %v", kind, err)
	}
	changes, err := pgx.CollectRows(changed, func(row pgx.CollectableRow) (dayChange, error) {
		var change dayChange
		err := row.Scan(&change.project, &change.evalID, &change.first, &change.last)
		return change, err
	})
	if err != nil {
		t.Fatalf("read the %s changes logged: %v", kind, err)
	}
	return changes
}

// TestTheLargestReadingsBodyGoesInBoundedStatements: a readings write of one body of nearly a
// megabyte, the most the route takes, of readings whose row kinds, row keys and attributes.repo
// are each the largest a key holds, is stored in statements that each take at most
// statementBytes of parameters as pgx encodes them, its insert of readings in several, its
// readings answered in the batch's order, and logs one eval_readings change, spanning their
// measured_at, as one statement does (migration 20261117100012). A reading whose own row would
// pass the bound alone, a note of 300,000 bytes, is refused READING_TOO_LARGE naming its note, and
// the batch stores nothing; one with a note of 200,000 bytes is stored, in statements within the
// bound.
func TestTheLargestReadingsBodyGoesInBoundedStatements(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	statements := &sentStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	const slug, body = "wide-body", 1 << 20
	evalID := EvalID(r.project, slug)
	day := at(3, 0, 0)
	key := func(prefix string, index int) string {
		head := fmt.Sprintf("%s%06d-", prefix, index)
		return head + strings.Repeat("k", maxKeyBytes-len(head))
	}
	reading := func(index int, note string) json.RawMessage {
		return row(t, map[string]any{"series": "minutes", "row_kind": key("kind", index%7), "row_key": key("row", index), "value": float64(index%97) + 0.5,
			"measured_at": day.Add(time.Duration(index) * time.Second), "attributes": map[string]any{"repo": key("repo", 0)}, "note": note})
	}
	// The batch's readings, as many as a body under the route's megabyte holds, with room for the
	// rest of the body (the readings' brackets and commas, its actor).
	var rows []json.RawMessage
	for size := 0; ; {
		next := reading(len(rows), "a note")
		if size+len(next)+1 > body-512 {
			break
		}
		rows, size = append(rows, next), size+len(next)+1
	}
	heads := r.changeHeads(t)
	written := r.write(t, slug, alice, rows...)
	sent := r.measured(t, statements.take())
	inserts := 0
	for _, statement := range sent {
		if statement.bytes > statementBytes {
			t.Errorf("the write sent %.80q with %d bytes of parameters as pgx encodes them, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
		if strings.Contains(statement.sql, "insert into eval_readings") {
			inserts++
		}
	}
	if inserts < 2 {
		t.Errorf("the %d readings of a body of nearly %d bytes were inserted in %d statements, and the check needs them cut", len(rows), body, inserts)
	}
	ordered := len(written.Readings) == len(rows)
	for index, stored := range written.Readings {
		ordered = ordered && stored.RowKey == key("row", index)
	}
	if !ordered || r.count(t, `select count(*) from eval_readings where eval_id = $1`, evalID) != len(rows) {
		t.Errorf("the write answered %d readings, in the batch's order %t, of the %d it was given", len(written.Readings), ordered, len(rows))
	}
	changes := r.loggedChanges(t, "eval_readings", heads)
	if last := day.Add(time.Duration(len(rows)-1) * time.Second); len(changes) != 1 || changes[0].evalID != evalID || !changes[0].first.Equal(day) || !changes[0].last.Equal(last) {
		t.Errorf("the write logged the eval_readings changes %v, want one of %s from %s to %s", changes, evalID, day.Format(time.RFC3339), last.Format(time.RFC3339))
	}
	largest, what := largestStatement(sent)
	t.Logf("%d readings in %d insert statements; %d statements, the largest %d bytes of parameters (%s), of %d; changes %v",
		len(rows), inserts, len(sent), largest, what, statementBytes, changes)

	readings := r.count(t, `select count(*) from eval_readings`)
	_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild,
		Rows: []json.RawMessage{reading(1000000, "a note"), reading(1000001, strings.Repeat("n", 300000))}})
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != CodeReadingTooLarge || len(refused.Rows) != 1 || refused.Rows[0].Index != 1 || refused.Rows[0].Field != "note" {
		t.Errorf("a reading with a note of 300,000 bytes was answered %v (%+v), want %s naming row 1's note", err, refused, CodeReadingTooLarge)
	} else {
		t.Logf("refused: %s", refused.Rows[0].Message)
	}
	if after := r.count(t, `select count(*) from eval_readings`); after != readings {
		t.Errorf("the refused batch moved eval_readings from %d rows to %d", readings, after)
	}
	statements.take()
	r.write(t, slug, alice, reading(1000002, strings.Repeat("n", 200000)))
	for _, statement := range r.measured(t, statements.take()) {
		if statement.bytes > statementBytes {
			t.Errorf("the reading with a note of 200,000 bytes sent %.80q with %d bytes of parameters, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
	}
}

// TestTheMostCorrectionsABodyHoldsGoInBoundedStatements: of each of two evals, readings and then
// one body under a megabyte that corrects every one of them; every statement of the correction
// takes at most statementBytes of parameters as pgx encodes them, and it logs one eval_readings
// and one eval_days change of its eval:
//   - 7,000 readings of one series on one day: the ids the correction locks, supersedes and reads
//     as folded, and the write's own readings, which the read of the day's standing readings once
//     sent, grow with the batch; the day row holds the 7,000 corrected values;
//   - 1,200 readings of one series on one day, each of its own repository of the largest name a
//     key holds: the keys of the 1,201 day rows the correction folds again grow with it; each
//     repository's row holds its corrected value.
func TestTheMostCorrectionsABodyHoldsGoInBoundedStatements(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	statements := &sentStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	day := at(3, 0, 0)
	for _, shape := range []struct {
		slug  string
		count int
		repo  func(index int) string
	}{
		{"many-corrections", 7000, func(int) string { return "" }},
		{"wide-corrections", 1200, func(index int) string {
			head := fmt.Sprintf("acme-%06d-", index)
			return head + strings.Repeat("r", maxKeyBytes-len(head))
		}},
	} {
		evalID := EvalID(r.project, shape.slug)
		reading := func(index int, value float64, replaces string) json.RawMessage {
			fields := map[string]any{"series": "v", "row_kind": "r", "row_key": fmt.Sprint(index), "value": value, "measured_at": day.Add(time.Duration(index) * time.Second)}
			if repo := shape.repo(index); repo != "" {
				fields["attributes"] = map[string]any{"repo": repo}
			}
			if replaces != "" {
				fields["replaces"] = replaces
			}
			return row(t, fields)
		}
		originals := make([]json.RawMessage, shape.count)
		for index := range originals {
			originals[index] = reading(index, float64(index%13)+1, "")
		}
		stored := r.write(t, shape.slug, alice, originals...)
		corrections, size := make([]json.RawMessage, shape.count), 0
		for index, original := range stored.Readings {
			corrections[index] = reading(index, 2.5, original.ID)
			size += len(corrections[index]) + 1
		}
		if size > 1<<20-512 {
			t.Fatalf("%s: the correction body holds %d bytes of readings, more than the route takes", shape.slug, size)
		}
		statements.take()
		heads := r.changeHeads(t)
		r.write(t, shape.slug, alice, corrections...)
		sent := r.measured(t, statements.take())
		for _, statement := range sent {
			if statement.bytes > statementBytes {
				t.Errorf("%s: the correction sent %.80q with %d bytes of parameters as pgx encodes them, past %d", shape.slug,
					strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
			}
		}
		if readings, changes := r.loggedChanges(t, "eval_readings", heads), r.loggedChanges(t, "eval_days", heads); len(readings) != 1 || len(changes) != 1 {
			t.Errorf("%s: the correction logged the eval_readings changes %v and the eval_days changes %v, want one of each", shape.slug, readings, changes)
		}
		if superseded := r.count(t, `select count(*) from eval_readings where eval_id = $1 and superseded_at is not null`, evalID); superseded != shape.count {
			t.Errorf("%s: the correction left %d readings superseded, want %d", shape.slug, superseded, shape.count)
		}
		if all := r.day(t, evalID, "v", "", day); all.N != shape.count || all.Sum == nil || *all.Sum != 2.5*float64(shape.count) {
			t.Errorf("%s: the project's day row holds n %d and sum %v, want %d corrected values of 2.5", shape.slug, all.N, showValue(all.Sum), shape.count)
		}
		if shape.repo(0) != "" {
			if corrected := r.count(t, `select count(*) from eval_days where eval_id = $1 and repo <> '' and n = 1 and sum = 2.5`, evalID); corrected != shape.count {
				t.Errorf("%s: %d repositories' day rows hold the one corrected value, want %d", shape.slug, corrected, shape.count)
			}
		}
		largest, what := largestStatement(sent)
		t.Logf("%s: a correction of %d readings in a body of %d bytes: %d statements, the largest %d bytes of parameters (%s), of %d",
			shape.slug, shape.count, size, len(sent), largest, what, statementBytes)
	}
}

// TestAWeeksCorrectionOfSmallReadingsIsInsertedInOneStatement: a week of readings as the bench
// writes them, 1,408 over 8 series and 7 days with short series names and row keys, each with a
// value and with n and the confidence fields null, and then one body correcting every one of them,
// which adds the id each replaces: about 180 bytes a reading as the insert sends it, some 250,000
// in all. The correction's readings are inserted in exactly one statement, within statementBytes
// as pgx encodes its parameters and within what readingBytes estimates of them beside the
// statement's own (paramBytes), which statementChunks cuts by: an estimate that counted the 8
// bytes of a number for each null would pass the bound and cut them in two.
func TestAWeeksCorrectionOfSmallReadingsIsInsertedInOneStatement(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	statements := &sentStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	const slug, count = "week-of-corrections", 1408
	evalID := EvalID(r.project, slug)
	reading := func(index int, value float64, replaces string) json.RawMessage {
		fields := map[string]any{"series": fmt.Sprintf("s%d", index/7%8), "row_kind": "run", "row_key": fmt.Sprintf("k%d", index), "value": value,
			"measured_at": at(6+index%7, 10, 0)}
		if replaces != "" {
			fields["replaces"] = replaces
		}
		return row(t, fields)
	}
	originals := make([]json.RawMessage, count)
	for index := range originals {
		originals[index] = reading(index, float64(1+index%7), "")
	}
	stored := r.write(t, slug, alice, originals...)
	corrections := make([]json.RawMessage, count)
	for index, original := range stored.Readings {
		corrections[index] = reading(index, *original.Value+1, original.ID)
	}
	statements.take()
	written := r.write(t, slug, alice, corrections...)
	var inserts []sentStatement
	for _, statement := range r.measured(t, statements.take()) {
		if strings.Contains(statement.sql, "insert into eval_readings") {
			inserts = append(inserts, statement)
		}
	}
	recordedBy, err := json.Marshal(alice.Recorded())
	if err != nil {
		t.Fatalf("encode the recorder: %v", err)
	}
	estimate, nulls := paramBytes(readingArrays, evalID)+4+len(recordedBy), 0
	for index := range written.Readings {
		one := &written.Readings[index]
		attributes, err := json.Marshal(one.Attributes)
		if err != nil {
			t.Fatalf("encode the attributes of reading %d: %v", index, err)
		}
		links, err := json.Marshal(nonNil(one.Links))
		if err != nil {
			t.Fatalf("encode the links of reading %d: %v", index, err)
		}
		estimate += readingBytes(one, string(attributes), string(links))
		for _, null := range []bool{one.Value == nil, one.N == nil, one.CILow == nil, one.CIHigh == nil, one.CILevel == nil} {
			if null {
				nulls++
			}
		}
	}
	if estimate+8*nulls <= statementBytes {
		t.Fatalf("the correction's readings take an estimated %d bytes, %d with 8 for each of their %d null numbers, and the check needs the second past %d",
			estimate, estimate+8*nulls, nulls, statementBytes)
	}
	sizes := make([]int, len(inserts))
	for index, insert := range inserts {
		sizes[index] = insert.bytes
	}
	if len(inserts) != 1 || inserts[0].bytes > statementBytes || inserts[0].bytes > estimate {
		t.Errorf("the correction of %d readings was inserted in %d statements of %v bytes of parameters as pgx encodes them, want one within %d and within the estimate %d",
			count, len(inserts), sizes, statementBytes, estimate)
	}
	if superseded := r.count(t, `select count(*) from eval_readings where eval_id = $1 and superseded_at is not null`, evalID); superseded != count || len(written.Readings) != count {
		t.Errorf("the correction stored %d readings and left %d superseded, want %d of each", len(written.Readings), superseded, count)
	}
	t.Logf("a correction of %d readings: %d insert statement(s) of %v bytes of parameters, estimated %d (%d counting 8 for each of %d null numbers), of %d",
		count, len(inserts), sizes, estimate, estimate+8*nulls, nulls, statementBytes)
}

// TestARefusalListsAtMostItsBoundOfRowsAndCountsTheRest: a refusal of 20,000 refused rows, given
// out of order, each with a field and an error past their bounds and full of characters JSON
// escapes, lists the first MaxRefusedRows by index, each text within its bound as JSON encodes it,
// and counts the rest (rows_omitted), its JSON, as the route's encoder writes it, within
// MaxAnswerBytes, what the stored answer's statement leaves it within statementBytes.
// MaxRefusedRows row errors at their largest (every text at its bound, a code of codeBytes, an
// index and a count of 20 digits) encode within that; one more passes it. Every code of the
// registry takes at most codeBytes.
func TestARefusalListsAtMostItsBoundOfRowsAndCountsTheRest(t *testing.T) {
	t.Parallel()
	encoded := func(value any) []byte {
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(value); err != nil {
			t.Fatalf("encode a refusal: %v", err)
		}
		return buffer.Bytes()
	}
	const count = 20000
	refused := make([]RowError, count)
	for index := range refused {
		refused[index] = RowError{Index: count - 1 - index, Code: CodeReadingInvalid, Field: strings.Repeat("<é", 500), Message: strings.Repeat("\"&\u2028x", 700)}
	}
	refusal := rowsRefusal(refused)
	var answer struct {
		Rows []struct {
			Index int             `json:"index"`
			Field json.RawMessage `json:"field"`
			Error json.RawMessage `json:"error"`
		} `json:"rows"`
		RowsOmitted int `json:"rows_omitted"`
	}
	body := encoded(refusal)
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if len(answer.Rows) != MaxRefusedRows || answer.RowsOmitted != count-MaxRefusedRows || len(answer.Rows)+answer.RowsOmitted != count {
		t.Errorf("the refusal of %d rows lists %d and counts %d more, want %d listed and the rest counted", count, len(answer.Rows), answer.RowsOmitted, MaxRefusedRows)
	}
	for at, one := range answer.Rows {
		if one.Index != at || len(one.Field) > fieldTextBytes+2 || len(one.Error) > errorTextBytes+2 {
			t.Errorf("listed row %d is row %d, its field %d bytes and its error %d as JSON, want row %d within %d and %d", at, one.Index, len(one.Field), len(one.Error), at,
				fieldTextBytes+2, errorTextBytes+2)
			break
		}
	}
	if room := MaxAnswerBytes; len(body) > room {
		t.Errorf("the refusal of %d rows encodes in %d bytes, past the %d the stored answer's statement leaves it", count, len(body), room)
	}
	t.Logf("a refusal of %d rows: %d listed, %d counted, %d bytes as JSON; %s", count, len(answer.Rows), answer.RowsOmitted, len(body), refusal.Message)

	largest := func(rows int) int {
		listed := make([]RowError, rows)
		for index := range listed {
			listed[index] = RowError{Index: math.MinInt64, Code: strings.Repeat("C", codeBytes), Field: strings.Repeat("f", fieldTextBytes), Message: strings.Repeat("m", errorTextBytes)}
		}
		return len(encoded(&Error{Code: strings.Repeat("C", codeBytes), Message: strings.Repeat("m", errorTextBytes), Rows: listed, RowsOmitted: math.MinInt64}))
	}
	room := MaxAnswerBytes
	if most, past := largest(MaxRefusedRows), largest(MaxRefusedRows+1); most > room || past <= room {
		t.Errorf("%d row errors at their largest encode in %d bytes and %d in %d, want the first within the %d the stored answer's statement leaves and the second past it",
			MaxRefusedRows, most, MaxRefusedRows+1, past, room)
	} else {
		t.Logf("%d row errors at their largest: %d bytes; %d: %d bytes; room %d", MaxRefusedRows, most, MaxRefusedRows+1, past, room)
	}
	for _, code := range []string{CodeSlugInvalid, CodeProjectNotFound, CodeFieldInvalid, CodeSeriesInvalid, CodeDocTextEmpty, CodeDocumentNotDocument,
		CodeReadingsEmpty, CodeReadingInvalid, CodeValueOrReason, CodeFieldServerSet, CodeReplacesUnknown, CodeAlreadyReplaced, CodeLinkHrefNotAbsolute,
		CodeLinkHrefNotHTTP, CodeLinkRelMissing, CodeReadingSeriesInvalid, CodeReadingDayOverflow, CodeMeasuredAtOutOfRange, CodeKeyTooLong,
		CodeFieldComputed, CodeSeriesLimit, CodeReadingDaySamplesLimit, CodeReadingTooLarge, CodeVersionTooLarge} {
		if len(code) > codeBytes {
			t.Errorf("the code %s takes %d bytes, past codeBytes, %d", code, len(code), codeBytes)
		}
	}
}

// TestADayRowPastItsSampleBoundIsRefusedNamingItsRow: batches of 1,000 readings of one series on
// one day fill its day row until a batch would leave it holding more than MaxDaySamples values:
// that batch is refused (CodeReadingDaySamplesLimit), naming its first row past the bound on its
// measured_at, and the bound, and stores nothing. A batch that fills the row to the bound is taken,
// no statement of it taking more than statementBytes of parameters as pgx encodes them, and one
// value more is then refused at its row 0; a correction of a reading of the full day, which adds no
// value, is taken, and so is a batch on another day.
func TestADayRowPastItsSampleBoundIsRefusedNamingItsRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	const slug, size, most = "many-samples", 1000, 25000
	evalID := EvalID(r.project, slug)
	day, other := at(3, 0, 0), at(4, 0, 0)
	batch := func(first, n int, on time.Time) Batch {
		rows := make([]json.RawMessage, n)
		for index := range n {
			rows[index] = row(t, map[string]any{"series": "v", "row_kind": "run", "row_key": fmt.Sprint(first + index), "value": float64((first+index)%997) + 0.5,
				"measured_at": on.Add(time.Duration(first+index) * time.Second)})
		}
		return Batch{Project: r.project, Slug: slug, Rows: rows, Caller: alice, BuiltBy: testBuild}
	}
	readings := func() int { return r.count(t, `select count(*) from eval_readings where eval_id = $1`, evalID) }
	held := 0
	var last Written
	var refused *Error
	for {
		if held >= most {
			t.Fatalf("one day row took %d values and refused none", held)
		}
		written, err := r.registry.WriteReadings(t.Context(), batch(held, size, day))
		if err != nil {
			if !errors.As(err, &refused) {
				t.Fatalf("a batch onto a day row of %d values: %v", held, err)
			}
			break
		}
		last, held = written, held+size
	}
	if refused.Code != CodeReadingDaySamplesLimit || len(refused.Rows) != 1 || refused.Rows[0].Code != CodeReadingDaySamplesLimit || refused.Rows[0].Field != "measured_at" {
		t.Fatalf("a batch onto a day row of %d values was refused %s, rows %+v, want %s naming one row's measured_at", held, refused.Code, refused.Rows, CodeReadingDaySamplesLimit)
	}
	limit := held + refused.Rows[0].Index
	if limit != MaxDaySamples || !strings.Contains(refused.Rows[0].Message, strconv.Itoa(limit)) {
		t.Errorf("the batch onto %d values was refused at its row %d, a bound of %d, naming: %s; want the bound %d named", held, refused.Rows[0].Index, limit, refused.Rows[0].Message, MaxDaySamples)
	}
	if n, stored := r.day(t, evalID, "v", "", day).N, readings(); n != held || stored != held {
		t.Errorf("the refused batch left the day row holding %d values and %d readings stored, want %d of each", n, stored, held)
	}
	t.Logf("a batch onto %d values: %s at row %d: %s", held, refused.Code, refused.Rows[0].Index, refused.Rows[0].Message)

	statements := &sentStatements{}
	traced := New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	if _, err := traced.WriteReadings(t.Context(), batch(held, MaxDaySamples-held, day)); err != nil {
		t.Fatalf("a batch filling the day row to %d values: %v", MaxDaySamples, err)
	}
	sent := r.measured(t, statements.take())
	for _, statement := range sent {
		if statement.bytes > statementBytes {
			t.Errorf("the batch filling the day row sent %.80q with %d bytes of parameters as pgx encodes them, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
	}
	largest, what := largestStatement(sent)
	t.Logf("the batch filling the day row to %d values: %d statements, the largest %d bytes of parameters (%s), of %d", MaxDaySamples, len(sent), largest, what, statementBytes)
	if n := r.day(t, evalID, "v", "", day).N; n != MaxDaySamples {
		t.Errorf("the day row holds %d values, want %d", n, MaxDaySamples)
	}
	_, err := r.registry.WriteReadings(t.Context(), batch(MaxDaySamples, 1, day))
	if !errors.As(err, &refused) || refused.Code != CodeReadingDaySamplesLimit || len(refused.Rows) != 1 || refused.Rows[0].Index != 0 ||
		!strings.Contains(refused.Rows[0].Message, strconv.Itoa(MaxDaySamples)) {
		t.Errorf("one value more onto the full day row was answered %v, want %s at row 0 naming %d", err, CodeReadingDaySamplesLimit, MaxDaySamples)
	}
	replaced := last.Readings[0]
	r.write(t, slug, alice, row(t, map[string]any{"series": "v", "row_kind": "run", "row_key": replaced.RowKey, "value": 7.5,
		"measured_at": replaced.MeasuredAt, "replaces": replaced.ID}))
	if n, stored := r.day(t, evalID, "v", "", day).N, readings(); n != MaxDaySamples || stored != MaxDaySamples+1 {
		t.Errorf("a correction on the full day row left it holding %d values and %d readings stored, want %d and %d", n, stored, MaxDaySamples, MaxDaySamples+1)
	}
	if _, err := r.registry.WriteReadings(t.Context(), batch(0, size, other)); err != nil {
		t.Errorf("a batch on another day: %v", err)
	} else if n := r.day(t, evalID, "v", "", other).N; n != size {
		t.Errorf("the batch on another day left its day row holding %d values, want %d", n, size)
	}
}

// TestTwoFirstWritesOfOneSlugRegisterItOnce: two writes of a slug no eval holds, on two
// connections at once, both store their readings, and the eval is registered once: one version,
// written by a reading, the caller its owner, and one section in the project's evals-md, which
// neither write found and one of them created. The test holds the eval's lock until both writes
// wait for it, so each has read that no version exists before either writes one.
func TestTwoFirstWritesOfOneSlugRegisterItOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	slug := "first-try-rate"
	evalID := EvalID(r.project, slug)
	holder, err := r.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(t.Context())
	if err := lockEval(t.Context(), holder, evalID); err != nil {
		t.Fatalf("hold the eval's lock: %v", err)
	}

	results := make([]Written, 2)
	failures := make([]error, 2)
	var writers sync.WaitGroup
	for index := range 2 {
		writers.Go(func() {
			results[index], failures[index] = r.registry.WriteReadings(t.Context(), Batch{
				Project: r.project, Slug: slug, Caller: implementer, BuiltBy: testBuild,
				Rows: []json.RawMessage{row(t, map[string]any{
					"series": "rate", "row_kind": "issue", "row_key": fmt.Sprintf("EVAL-%d", index+1), "value": float64(index), "measured_at": at(4, 12, index),
				})},
			})
		})
	}
	storetest.WaitForLockWaiters(t, holder, 2)
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("release the eval's lock: %v", err)
	}
	writers.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("write %d: %v", index, err)
		}
	}

	registered := 0
	for _, result := range results {
		if result.Registered {
			registered++
		}
	}
	versions := r.count(t, `select count(*) from eval_versions where eval_id = $1`, evalID)
	readings := r.count(t, `select count(*) from eval_readings where eval_id = $1`, evalID)
	text, blocks := r.document(t)
	sections := headings(text, slug)
	t.Logf("%d versions, %d readings, %d writes registered the eval, %d sections in evals-md:\n%s", versions, readings, registered, sections, text)
	if versions != 1 || registered != 1 {
		t.Errorf("the eval has %d versions and %d writes say they registered it, want 1 and 1", versions, registered)
	}
	if readings != 2 {
		t.Errorf("the eval holds %d readings, want both writes' 2", readings)
	}
	if sections != 1 {
		t.Errorf("evals-md holds %d sections for %s, want 1", sections, slug)
	}
	page, err := r.registry.Current(t.Context(), []string{r.project}, "", 10, 0)
	if err != nil {
		t.Fatalf("read the registry: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("the registry holds %d evals, want the one registered", page.Total)
	}
	definition := page.Items[0].Definition
	if definition.WrittenVia != ViaReading || definition.Kind != KindExperiment || definition.Source != SourceLogged ||
		definition.State != StateActive || definition.Target != nil {
		t.Errorf("the registered eval is %+v, want a logged experiment, active, with no target, written by a reading", definition)
	}
	if definition.OwnerRole != "implementer" || definition.Person != "bob" || definition.WrittenBy.ID != implementer.ID {
		t.Errorf("the eval's owner is role %q, person %q, written by %q, want the implementer session acting for bob",
			definition.OwnerRole, definition.Person, definition.WrittenBy.ID)
	}
	if !hasBlock(blocks, definition.DocBlock) {
		t.Errorf("doc_block %q names no block of evals-md", definition.DocBlock)
	}
	if !strings.Contains(text, "registered by a reading that session `session-implementer` for `bob` logged at") {
		t.Errorf("the eval's section does not say who registered it by a reading:\n%s", text)
	}
	if series := page.Items[0].Series; len(series) != 1 || series[0].Role != RoleHeadline || !series[0].IsPrimary || series[0].Agg != AggLast || !series[0].Listed {
		t.Errorf("the eval's series are %+v, want rate alone, the listed headline primary series folded by its last value", series)
	}
}

// TestACorrectionWaitsForTheEvalsLockHoldingNoTransactionID: a correction of a reading of an
// eval whose lock another transaction holds waits for that lock before it locks the reading it
// replaces, so it waits holding no transaction id (pg_stat_activity's backend_xid is null) and
// holds back no reader of the event log, whose horizon waits for every running transaction id
// (events.Horizon). Let go, it lands. Before, it locked the reading first (lockReplaced, `for
// update`), which gave it an id it held for the whole wait.
func TestACorrectionWaitsForTheEvalsLockHoldingNoTransactionID(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	slug := "correction-wait"
	stored := r.write(t, slug, alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 4, "measured_at": at(4, 9, 0)}))
	evalID, replaced := stored.Definition.EvalID, stored.Readings[0].ID
	holder, err := r.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(context.Background())
	if err := lockEval(t.Context(), holder, evalID); err != nil {
		t.Fatalf("hold the eval's lock: %v", err)
	}

	answered := make(chan error, 1)
	go func() {
		_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild,
			Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 6, "measured_at": at(4, 9, 0), "replaces": replaced})}})
		answered <- err
	}()
	storetest.WaitForLockWaiters(t, holder, 1)
	// backend_xid is null for a backend holding no transaction id, read here as "".
	rows, err := r.database.Pool.Query(t.Context(), `
		select pid, coalesce(wait_event, ''), coalesce(backend_xid::text, '') from pg_stat_activity
		where datname = current_database() and $1::integer = any(pg_blocking_pids(pid))
	`, int32(holder.Conn().PgConn().PID()))
	if err != nil {
		t.Fatalf("read the backends waiting on the eval's lock: %v", err)
	}
	type waiter struct {
		pid       int32
		waitEvent string
		xid       string
	}
	waiters, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (waiter, error) {
		var found waiter
		err := row.Scan(&found.pid, &found.waitEvent, &found.xid)
		return found, err
	})
	if err != nil {
		t.Fatalf("read the backends waiting on the eval's lock: %v", err)
	}
	if len(waiters) != 1 || waiters[0].waitEvent != "advisory" {
		t.Fatalf("the backends waiting on the holder are %+v, want the correction alone, on the eval's advisory lock", waiters)
	}
	if xid := waiters[0].xid; xid != "" {
		t.Errorf("the correction waits for the eval's lock holding transaction id %s, want none", xid)
	}
	t.Logf("backend %d waits on the eval's %s lock holding transaction id %q", waiters[0].pid, waiters[0].waitEvent, waiters[0].xid)

	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("release the eval's lock: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("the correction, let go: %v", err)
	}
	if superseded := r.count(t, `select count(*) from eval_readings where id = $1 and superseded_at is not null`, replaced); superseded != 1 {
		t.Errorf("the reading the correction replaces is not superseded")
	}
}

// TestACorrectionSupersedesTheReadingItReplacesAndMovesItsDayRow: a correction leaves the reading
// it replaces stored, with superseded_at set, and takes that reading's part out of its day rows
// while adding its own: the project's row and its repository's row both move n, sum, samples,
// max and the last value. A second correction measured before the day's other reading makes that
// reading the day's last again.
func TestACorrectionSupersedesTheReadingItReplacesAndMovesItsDayRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	written := r.write(t, "build-minutes", alice,
		row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 10, "n": 2, "measured_at": at(5, 10, 0)}),
		row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "b", "value": 20, "measured_at": at(5, 11, 0), "attributes": map[string]any{"repo": "acme/api"}}),
	)
	evalID := written.Definition.EvalID
	replaced := written.Readings[1]
	day := at(5, 0, 0)
	before := r.day(t, evalID, "minutes", "", day)
	t.Logf("before: n=%d sum=%s samples=%v max=%s last=%s at %v num=%s den=%s", before.N, showValue(before.Sum), before.Samples,
		showValue(before.Max), showValue(before.LastValue), before.LastAt, showValue(before.Num), showValue(before.Den))

	corrected := r.write(t, "build-minutes", alice, row(t, map[string]any{
		"series": "minutes", "row_kind": "run", "row_key": "b", "value": 5, "measured_at": at(5, 11, 0),
		"attributes": map[string]any{"repo": "acme/api"}, "replaces": replaced.ID,
	})).Readings[0]

	var supersededAt *time.Time
	var value float64
	if err := r.database.Pool.QueryRow(t.Context(), `select superseded_at, value from eval_readings where id = $1`, replaced.ID).Scan(&supersededAt, &value); err != nil {
		t.Fatalf("read the replaced reading: %v", err)
	}
	if supersededAt == nil || value != 20 {
		t.Errorf("the replaced reading holds value %v and superseded_at %v, want 20, kept, and superseded", value, supersededAt)
	}
	if corrected.Replaces != replaced.ID || corrected.SupersededAt != nil {
		t.Errorf("the correction replaces %q and was superseded at %v, want %s and standing", corrected.Replaces, corrected.SupersededAt, replaced.ID)
	}
	all := r.day(t, evalID, "minutes", "", day)
	repo := r.day(t, evalID, "minutes", "acme/api", day)
	t.Logf("after:  n=%d sum=%v samples=%v max=%v last=%v at %v num=%v den=%v; acme/api n=%d samples=%v last=%v",
		all.N, *all.Sum, all.Samples, *all.Max, *all.LastValue, *all.LastAt, *all.Num, *all.Den, repo.N, repo.Samples, *repo.LastValue)
	if all.N != 2 || *all.Sum != 15 || !slices.Equal(all.Samples, []float64{10, 5}) || *all.Max != 10 ||
		*all.LastValue != 5 || !all.LastAt.Equal(at(5, 11, 0)) || *all.Num != 25 || *all.Den != 3 {
		t.Errorf("the project's day row is n=%d sum=%v samples=%v max=%v last=%v, want n=2 sum=15 samples=[10 5] max=10 last=5 at 11:00 num=25 den=3",
			all.N, *all.Sum, all.Samples, *all.Max, *all.LastValue)
	}
	if repo.N != 1 || !slices.Equal(repo.Samples, []float64{5}) || *repo.LastValue != 5 || repo.BuiltBy != testBuild {
		t.Errorf("acme/api's day row is n=%d samples=%v last=%v built by %q, want the correction alone, built by %s", repo.N, repo.Samples, *repo.LastValue, repo.BuiltBy, testBuild)
	}

	// The correction was the day's last; its own correction is measured before the other
	// reading, so the day's last is that reading again, which the write reads back.
	r.write(t, "build-minutes", alice, row(t, map[string]any{
		"series": "minutes", "row_kind": "run", "row_key": "b", "value": 7, "measured_at": at(5, 9, 0),
		"attributes": map[string]any{"repo": "acme/api"}, "replaces": corrected.ID,
	}))
	again := r.day(t, evalID, "minutes", "", day)
	t.Logf("again:  n=%d sum=%v samples=%v last=%v at %v", again.N, *again.Sum, again.Samples, *again.LastValue, *again.LastAt)
	if again.N != 2 || *again.Sum != 17 || *again.LastValue != 10 || !again.LastAt.Equal(at(5, 10, 0)) {
		t.Errorf("after the second correction the day row is n=%d sum=%v last=%v at %v, want n=2 sum=17 and the 10:00 reading's 10 last",
			again.N, *again.Sum, *again.LastValue, *again.LastAt)
	}
	if standing := r.count(t, `select count(*) from eval_readings where eval_id = $1 and superseded_at is null`, evalID); standing != 2 {
		t.Errorf("%d readings of the eval stand, want 2 of its 4", standing)
	}
}

// TestTheDaysLastSurvivesARefoldWhenTheLaterStartedOfTwoTiedWritesCommitsFirst: two writes of one
// series and day, each a reading measured at 12:00, the write that began later committing first,
// and then a correction of the day's 08:00 reading, which folds the day's row again. The day's
// last value is the reading of the write that began later, whose recorded_at (the time its
// transaction began) breaks the tie, once both writes land and after the refold alike. The write
// that began first is held right after its transaction began, before it takes the eval's lock,
// the first lock it takes (WriteReadings), while the other writes and commits: its registry's
// document service holds its Join, the step after the begin (joinPause). Before, it appended its
// reading to the row the other had written and so made it the day's last, as the later part at
// an equal moment, and the refold, which orders the readings by when their writes began, put the
// other's back: the day's last flipped from 10 to 20.
func TestTheDaysLastSurvivesARefoldWhenTheLaterStartedOfTwoTiedWritesCommitsFirst(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	slug := "tie-break"
	evalID, day, moment := EvalID(r.project, slug), at(4, 0, 0), at(4, 12, 0)
	minutes := func(key string, value float64, measured time.Time, replaces string) json.RawMessage {
		fields := map[string]any{"series": "minutes", "row_kind": "run", "row_key": key, "value": value, "measured_at": measured}
		if replaces != "" {
			fields["replaces"] = replaces
		}
		return row(t, fields)
	}
	early := r.write(t, slug, alice, minutes("early", 1, at(4, 8, 0), "")).Readings[0].ID
	pause := joinPause{API: r.documents, reached: make(chan struct{}), release: make(chan struct{})}
	let := sync.OnceFunc(func() { close(pause.release) })
	t.Cleanup(let)
	held := New(r.database, pause, r.registry.events, r.registry.attributor)
	firstDone := make(chan error, 1)
	go func() {
		_, err := held.WriteReadings(context.WithValue(ctx, joinPauseKey{}, true), Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild,
			Rows: []json.RawMessage{minutes("first", 10, moment, "")}})
		firstDone <- err
	}()
	select {
	case <-pause.reached:
	case err := <-firstDone:
		t.Fatalf("the write that began first answered %v before it was held", err)
	case <-time.After(lockOrderBound):
		t.Fatalf("the write that began first never reached its Join")
	}
	_, laterErr := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild,
		Rows: []json.RawMessage{minutes("later", 20, moment, "")}})
	let()
	if firstErr := <-firstDone; firstErr != nil || laterErr != nil {
		t.Fatalf("the write that began first answered %v and the one that began later %v", firstErr, laterErr)
	}
	var laterBegan bool
	if err := r.database.Pool.QueryRow(ctx, `
		select (select recorded_at from eval_readings where eval_id = $1 and row_key = 'first')
		     < (select recorded_at from eval_readings where eval_id = $1 and row_key = 'later')
	`, evalID).Scan(&laterBegan); err != nil || !laterBegan {
		t.Fatalf("the reading of the write held first was not recorded before the other (%v): the race did not happen", err)
	}
	landed := r.day(t, evalID, "minutes", "", day)
	r.write(t, slug, alice, minutes("early", 3, at(4, 8, 0), early))
	refolded := r.day(t, evalID, "minutes", "", day)
	t.Logf("the 4th's last once both writes landed: %s at %v; after the refold: %s at %v",
		showValue(landed.LastValue), landed.LastAt, showValue(refolded.LastValue), refolded.LastAt)
	for _, check := range []struct {
		when string
		row  DayRow
	}{{"once both writes landed", landed}, {"after the refold", refolded}} {
		if check.row.LastValue == nil || *check.row.LastValue != 20 || check.row.LastAt == nil || !check.row.LastAt.Equal(moment) {
			t.Errorf("the 4th's last value %s is %s, want 20 at 12:00, the reading of the write that began later", check.when, showValue(check.row.LastValue))
		}
	}
}

// joinPauseKey marks the context of a write whose Join a joinPause holds.
type joinPauseKey struct{}

// joinPause is a registry's document service whose Join, the step of every registry write right
// after its transaction begins (writeOnce), says so on reached and waits for release in a write
// whose context carries joinPauseKey: so that write's transaction has begun, and its now() is
// fixed, before it takes any lock. Every other call goes to the service it holds.
type joinPause struct {
	docs.API
	reached, release chan struct{}
}

func (p joinPause) Join(ctx context.Context, tx pgx.Tx) (context.Context, *docs.Ledger) {
	if ctx.Value(joinPauseKey{}) != nil {
		p.reached <- struct{}{}
		<-p.release
	}
	return p.API.Join(ctx, tx)
}

// TestAWriteThatEmptiesADayRowWritesItsZeroRowBesideTheRowsItFills: one write's day rows are
// written together, each with its own parts and samples. A correction that moves a reading to
// another day empties the day it left, which stands as the zero row (n 0, a zero sum, numerator
// and denominator, the empty sample list, no maximum and no last value) between a day before it
// and a day after it that the same write adds to, each of which holds its own samples alone.
func TestAWriteThatEmptiesADayRowWritesItsZeroRowBesideTheRowsItFills(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	minutes := func(key string, value float64, n int, measured time.Time, extra map[string]any) json.RawMessage {
		fields := map[string]any{"series": "minutes", "row_kind": "run", "row_key": key, "value": value, "n": n, "measured_at": measured}
		maps.Copy(fields, extra)
		return row(t, fields)
	}
	written := r.write(t, "build-minutes", alice,
		minutes("a", 4, 1, at(4, 10, 0), nil), minutes("b", 6, 3, at(5, 10, 0), nil), minutes("c", 8, 1, at(6, 10, 0), nil))
	evalID := written.Definition.EvalID
	r.write(t, "build-minutes", alice,
		minutes("d", 1, 1, at(4, 12, 0), nil),
		minutes("b", 9, 2, at(6, 9, 0), map[string]any{"replaces": written.Readings[1].ID}),
		minutes("e", 2, 1, at(4, 8, 0), nil))

	for _, want := range []struct {
		day            time.Time
		samples        []float64
		sum, num, den  float64
		max, lastValue *float64
		lastAt         *time.Time
	}{
		{day: at(4, 0, 0), samples: []float64{4, 1, 2}, sum: 7, num: 7, den: 3, max: new(4.0), lastValue: new(1.0), lastAt: new(at(4, 12, 0))},
		{day: at(5, 0, 0), samples: []float64{}, sum: 0, num: 0, den: 0},
		{day: at(6, 0, 0), samples: []float64{8, 9}, sum: 17, num: 26, den: 3, max: new(9.0), lastValue: new(8.0), lastAt: new(at(6, 10, 0))},
	} {
		got := r.day(t, evalID, "minutes", "", want.day)
		shown := fmt.Sprintf("n=%d samples=%v sum=%s num=%s den=%s max=%s last=%s at %v", got.N, got.Samples, showValue(got.Sum),
			showValue(got.Num), showValue(got.Den), showValue(got.Max), showValue(got.LastValue), got.LastAt)
		t.Logf("%s: %s", want.day.Format(time.DateOnly), shown)
		sameTime := (got.LastAt == nil) == (want.lastAt == nil) && (got.LastAt == nil || got.LastAt.Equal(*want.lastAt))
		if got.N != len(want.samples) || got.Samples == nil || !slices.Equal(got.Samples, want.samples) ||
			got.Sum == nil || *got.Sum != want.sum || got.Num == nil || *got.Num != want.num || got.Den == nil || *got.Den != want.den ||
			!sameValue(got.Max, want.max) || !sameValue(got.LastValue, want.lastValue) || !sameTime {
			t.Errorf("the day row of %s is %s, want samples %v, sum %v, num %v, den %v, max %s, last %s at %v", want.day.Format(time.DateOnly),
				shown, want.samples, want.sum, want.num, want.den, showValue(want.max), showValue(want.lastValue), want.lastAt)
		}
	}
}

// TestAReadingsWriteWritesTheZeroRowsItsReadingsLeaveOwed: a readings write writes, in its own
// transaction, the zero row of every day its readings leave owed, so a view of the eval is sent one
// change of its day rows, and the refresher's fill after it finds nothing to write. A first reading
// of a slug twelve days back leaves its series a row on every day from its own through today, and
// then a reading three days before that first day leaves one on each of the two days up to it.
// After each write every row it added carries its transaction (xmin, the transaction of its
// reading's row), the change log holds one eval_days change of the eval spanning the days it wrote,
// and the refresher's fill of the project's logged evals as of the same moment writes no row and
// logs no change. Before, the fill wrote the days after the reading in a transaction of its own,
// which a view was sent as a second change.
func TestAReadingsWriteWritesTheZeroRowsItsReadingsLeaveOwed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	var clock time.Time
	if err := r.database.Pool.QueryRow(ctx, `select now()`).Scan(&clock); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	today := measures.Midnight(clock)
	const slug = "first-fill"
	evalID := EvalID(r.project, slug)
	// held is each day the series holds its all-repositories row of, by the transaction that wrote
	// the row.
	held := func() map[time.Time]string {
		t.Helper()
		rows, err := r.database.Pool.Query(ctx, `select day, xmin::text from eval_days where eval_id = $1 and series = 'minutes' and repo = ''`, evalID)
		if err != nil {
			t.Fatalf("read the rows of %s: %v", evalID, err)
		}
		days := map[time.Time]string{}
		var day time.Time
		var xmin string
		if _, err := pgx.ForEachRow(rows, []any{&day, &xmin}, func() error {
			days[measures.Midnight(day)] = xmin
			return nil
		}); err != nil {
			t.Fatalf("read the rows of %s: %v", evalID, err)
		}
		return days
	}
	for index, step := range []struct {
		what string
		// day is the reading's day, and last the last day the write owes a row of.
		day, last time.Time
	}{
		{"a first reading twelve days back", today.AddDate(0, 0, -12), today},
		{"a reading three days before the series' first day", today.AddDate(0, 0, -15), today.AddDate(0, 0, -13)},
	} {
		before, heads, key := held(), r.changeHeads(t), fmt.Sprint("run-", index)
		r.write(t, slug, alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": key, "value": 3, "measured_at": step.day.Add(9 * time.Hour)}))
		var xmin string
		if err := r.database.Pool.QueryRow(ctx, `select xmin::text from eval_readings where eval_id = $1 and row_key = $2`, evalID, key).Scan(&xmin); err != nil {
			t.Fatalf("%s: read the transaction of the reading: %v", step.what, err)
		}
		after := held()
		var missing, others []string
		for day := step.day; !day.After(today); day = day.AddDate(0, 0, 1) {
			if _, ok := after[day]; !ok {
				missing = append(missing, day.Format(time.DateOnly))
			}
		}
		for day, by := range after {
			if _, was := before[day]; !was && by != xmin {
				others = append(others, day.Format(time.DateOnly)+" "+by)
			}
		}
		slices.Sort(others)
		if len(missing) > 0 {
			t.Errorf("%s: the series holds no row of %v, days from the reading's %s through today %s", step.what, missing, step.day.Format(time.DateOnly), today.Format(time.DateOnly))
		}
		if added, want := len(after)-len(before), int(step.last.Sub(step.day).Hours()/24)+1; len(others) > 0 || added != want {
			t.Errorf("%s: the write added %d rows, %v of another transaction than its own %s, want %d of its own", step.what, added, others, xmin, want)
		}
		if changes := r.loggedChanges(t, "eval_days", heads); len(changes) != 1 || changes[0].evalID != evalID ||
			!changes[0].first.Equal(step.day) || !changes[0].last.Equal(step.last) {
			t.Errorf("%s: the write logged the eval_days changes %v, want one of %s from %s to %s", step.what, changes, evalID,
				step.day.Format(time.DateOnly), step.last.Format(time.DateOnly))
		}
		heads = r.changeHeads(t)
		filled, err := r.registry.newRefresh(testBuild, clock, walkBatch).fillLoggedZeros(ctx, r.project)
		if err != nil {
			t.Fatalf("%s: the refresher's fill after the write: %v", step.what, err)
		}
		if changes := r.loggedChanges(t, "eval_days", heads); filled != 0 || len(changes) != 0 {
			t.Errorf("%s: the refresher's fill after the write wrote %d rows and logged the eval_days changes %v, want none", step.what, filled, changes)
		}
		t.Logf("%s: the write added %d rows, through %s, in its transaction %s", step.what, len(after)-len(before), step.last.Format(time.DateOnly), xmin)
	}
}

// sameValue reports whether two optional values are both absent or both the same value.
func sameValue(got, want *float64) bool {
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}

// TestAReadingUnderAComputedSlugIsStoredAndFoldedIntoNoDayRow: a reading logged under a computed
// eval's slug, the seed's deploy-frequency, is stored and answered by the items read, its series
// registered unlisted, and no day row of the computed eval moves.
func TestAReadingUnderAComputedSlugIsStoredAndFoldedIntoNoDayRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.registry.Ensure(t.Context(), r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	written := r.write(t, "deploy-frequency", alice, row(t, map[string]any{
		"series": "manual-count", "row_kind": "day", "row_key": "2026-11-06", "value": 3, "measured_at": at(6, 18, 0),
	}))
	evalID := written.Definition.EvalID
	days := r.count(t, `select count(*) from eval_days where eval_id = $1`, evalID)
	items, err := r.registry.Items(t.Context(), evalID, "", "day", "", nil, Range{From: at(6, 0, 0), To: at(7, 0, 0)}, 10, 0)
	if err != nil {
		t.Fatalf("read the items: %v", err)
	}
	t.Logf("%d day rows, %d items, registered series %+v", days, items.Total, written.Series)
	if days != 0 {
		t.Errorf("the computed eval has %d day rows after a logged reading, want none", days)
	}
	if items.Total != 1 || items.Items[0].ID != written.Readings[0].ID {
		t.Errorf("the items read answers %+v, want the logged reading", items.Items)
	}
	if len(written.Series) != 1 || written.Series[0].Listed || written.Series[0].Role != RoleControl {
		t.Errorf("the reading registered %+v, want manual-count as an unlisted control series", written.Series)
	}
}

// TestACorrectionOfAReadingNeverFoldedTakesNothingFromItsDayRow: a reading logged while its eval
// was computed is stored and folded into no day row; once a later version makes the eval logged,
// a reading of the same value is folded into that day's row, and a correction of the first,
// unfolded reading adds its own value and takes nothing away, so the day holds the second
// reading's value and the correction's.
func TestACorrectionOfAReadingNeverFoldedTakesNothingFromItsDayRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	evalID := EvalID(r.project, "review-load")
	if _, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "review-load", Caller: alice, Kind: Set(KindExperiment),
		Series: []SeriesChange{{Series: "minutes", Agg: Set(AggQuantile)}},
	}); err != nil {
		t.Fatalf("register review-load: %v", err)
	}
	// Only the seed makes an eval computed, and no register write turns a computed eval logged
	// (R5), so the two versions are written here as the store holds them.
	r.setSource(t, evalID, "computed:evals.ComputedForTheTest")
	unfolded := r.write(t, "review-load", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 7, "measured_at": at(5, 9, 0)})).Readings[0]
	r.setSource(t, evalID, SourceLogged)
	r.write(t, "review-load", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "b", "value": 7, "measured_at": at(5, 10, 0)}))
	before := r.day(t, evalID, "minutes", "", at(5, 0, 0))
	r.write(t, "review-load", alice, row(t, map[string]any{
		"series": "minutes", "row_kind": "run", "row_key": "a", "value": 2, "measured_at": at(5, 9, 0), "replaces": unfolded.ID,
	}))
	after := r.day(t, evalID, "minutes", "", at(5, 0, 0))
	samples := slices.Sorted(slices.Values(after.Samples))
	if after.N != 2 || !slices.Equal(samples, []float64{2, 7}) {
		t.Errorf("after the correction of the unfolded reading the day holds n %d, samples %v; want 2, [2 7]", after.N, samples)
	}
	t.Logf("the day held samples %v before the correction and %v after it", before.Samples, samples)
}

// setSource writes the next version of evalID, the newest with source in place of its own, as the
// seed writes a computed one over a slug a reading registered, and as a register write stored a
// logged one before R5's rule.
func (r rig) setSource(t *testing.T, evalID, source string) {
	t.Helper()
	if _, err := r.database.Pool.Exec(t.Context(), `
		insert into eval_versions (eval_id, version, project_key, slug, name, question, number_rule, decision, doc_block,
			failure_modes, unit, direction, target, target_inclusive, target_judges, owner_role, person, issue_key, kind,
			source, state, replaced_by, links, written_via, written_by)
		select eval_id, version + 1, project_key, slug, name, question, number_rule, decision, doc_block,
			failure_modes, unit, direction, target, target_inclusive, target_judges, owner_role, person, issue_key, kind,
			$2, state, replaced_by, links, written_via, written_by
		from evals_current where eval_id = $1
	`, evalID, source); err != nil {
		t.Fatalf("make %s %s: %v", evalID, source, err)
	}
}

// TestACorrectionBatchOverSeveralDaysFoldsEachDayFromTheReadingsThatStand: one batch correcting a
// reading on each of three days folds every row a corrected reading counted in again from the
// readings that stand, the project's row and each repository's, and leaves out a reading written
// while the eval was computed, between two logged versions, which no row ever folded; a row no
// corrected reading counted in is appended to.
func TestACorrectionBatchOverSeveralDaysFoldsEachDayFromTheReadingsThatStand(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	reading := func(series string, value float64, measured time.Time, repo, replaces string) json.RawMessage {
		fields := map[string]any{"series": series, "row_kind": "run", "value": value, "measured_at": measured}
		if repo != "" {
			fields["attributes"] = map[string]any{"repo": repo}
		}
		if replaces != "" {
			fields["replaces"] = replaces
		}
		return row(t, fields)
	}
	first := r.write(t, "build-minutes", alice,
		reading("minutes", 1, at(4, 9, 0), "acme/api", ""),
		reading("minutes", 2, at(4, 10, 0), "", ""),
		reading("minutes", 3, at(5, 9, 0), "acme/web", ""),
		reading("queue", 4, at(5, 10, 0), "acme/api", ""),
		reading("minutes", 5, at(6, 9, 0), "acme/api", ""),
	).Readings
	evalID := EvalID(r.project, "build-minutes")
	r.setSource(t, evalID, "computed:evals.ComputedForTheTest")
	r.write(t, "build-minutes", alice, reading("minutes", 100, at(5, 11, 0), "", ""))
	r.setSource(t, evalID, SourceLogged)
	r.write(t, "build-minutes", alice, reading("minutes", 6, at(6, 10, 0), "acme/web", ""))
	r.write(t, "build-minutes", alice,
		reading("minutes", 10, at(4, 9, 0), "acme/api", first[0].ID),
		reading("minutes", 30, at(5, 9, 0), "acme/web", first[2].ID),
		reading("minutes", 50, at(6, 9, 0), "acme/api", first[4].ID),
		reading("queue", 7, at(5, 12, 0), "", ""),
	)

	for _, want := range []struct {
		series, repo string
		day          time.Time
		samples      []float64
	}{
		{"minutes", "", at(4, 0, 0), []float64{2, 10}},
		{"minutes", "acme/api", at(4, 0, 0), []float64{10}},
		{"minutes", "", at(5, 0, 0), []float64{30}},
		{"minutes", "acme/web", at(5, 0, 0), []float64{30}},
		{"queue", "", at(5, 0, 0), []float64{4, 7}},
		{"queue", "acme/api", at(5, 0, 0), []float64{4}},
		{"minutes", "", at(6, 0, 0), []float64{6, 50}},
		{"minutes", "acme/api", at(6, 0, 0), []float64{50}},
		{"minutes", "acme/web", at(6, 0, 0), []float64{6}},
	} {
		got := r.day(t, evalID, want.series, want.repo, want.day)
		if got.N != len(want.samples) || !slices.Equal(got.Samples, want.samples) {
			t.Errorf("the %s row of %s, repo %q, holds n %d, samples %v; want %v", want.day.Format(time.DateOnly), want.series, want.repo,
				got.N, got.Samples, want.samples)
		}
	}
}

// TestATieOfOneMomentKeepsTheDaysLastAcrossACorrection: a batch whose two readings of a day are
// measured at one moment leaves the later of the two in the batch as the day's last, and the day's
// samples in the batch's order. A later correction of a third reading of the day folds the day
// again from the readings that stand, which keeps that last and that order: the fold reads the
// standing readings in the order they were written, a batch's in its own order, never in an
// order a tie leaves to chance. Each of 24 days holds such a tie, so an order that chance decides
// keeps every day's last with a chance of one in 2^24.
func TestATieOfOneMomentKeepsTheDaysLastAcrossACorrection(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	on := func(day, hour int) time.Time { return time.Date(2026, time.October, day, hour, 0, 0, 0, time.UTC) }
	reading := func(key string, value float64, measured time.Time, replaces string) json.RawMessage {
		fields := map[string]any{"series": "depth", "row_kind": "queue", "row_key": key, "value": value, "measured_at": measured}
		if replaces != "" {
			fields["replaces"] = replaces
		}
		return row(t, fields)
	}
	const days = 24
	var tied []json.RawMessage
	for day := 1; day <= days; day++ {
		tied = append(tied,
			reading(fmt.Sprintf("a-%d", day), 1, on(day, 9), ""),
			reading(fmt.Sprintf("b-%d", day), 2, on(day, 9), ""),
			reading(fmt.Sprintf("c-%d", day), 3, on(day, 8), ""))
	}
	written := r.write(t, "queue-depth", alice, tied...)
	evalID := written.Definition.EvalID
	check := func(when string, samples []float64) {
		t.Helper()
		flipped := 0
		for day := 1; day <= days; day++ {
			got := r.day(t, evalID, "depth", "", on(day, 0))
			if got.LastValue == nil || *got.LastValue != 2 || !got.LastAt.Equal(on(day, 9)) || !slices.Equal(got.Samples, samples) {
				flipped++
				t.Errorf("%s, the row of %s holds last %s at %v and samples %v; want the batch's later reading at 09:00, 2, and samples %v",
					when, on(day, 0).Format(time.DateOnly), showValue(got.LastValue), got.LastAt, got.Samples, samples)
			}
		}
		t.Logf("%s: %d of %d days hold another last or order", when, flipped, days)
	}
	check("after the batch", []float64{1, 2, 3})
	var corrections []json.RawMessage
	for day := 1; day <= days; day++ {
		corrections = append(corrections, reading(fmt.Sprintf("c-%d", day), 4, on(day, 8), written.Readings[3*(day-1)+2].ID))
	}
	r.write(t, "queue-depth", alice, corrections...)
	check("after the correction", []float64{1, 2, 4})
}

// TestALastWindowOverRepositoriesTakesTheLaterWriteOfATie: two repositories' day rows of one
// `last` series and day, each holding a reading measured at the same moment, are a tie the
// window fold breaks as the project's own row breaks it (writtenBefore): by the later write of the
// two values, the write that began later (recorded_at) and within one write the later row (id),
// whatever order the rows come in and whichever repository's key sorts first. On the 1st acme/b's
// reading is written first and acme/a's in a later write; on the 2nd acme/a's first and acme/b's
// later; on the 3rd one batch holds acme/b's and then acme/a's. Each day's window over both
// repositories, its two rows folded in the order DayWindowIn answered them and reversed, answers
// the later write's value, which the project's row of that day holds as its last.
func TestALastWindowOverRepositoriesTakesTheLaterWriteOfATie(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	reading := func(repo string, value float64, day int) json.RawMessage {
		return row(t, map[string]any{"series": "depth", "row_kind": "queue", "row_key": fmt.Sprintf("%s-%d", repo, day),
			"value": value, "measured_at": at(day, 12, 0), "attributes": map[string]any{"repo": repo}})
	}
	r.write(t, "tied-depth", alice, reading("acme/b", 2, 1))
	r.write(t, "tied-depth", alice, reading("acme/a", 1, 1))
	r.write(t, "tied-depth", alice, reading("acme/a", 1, 2))
	r.write(t, "tied-depth", alice, reading("acme/b", 2, 2))
	written := r.write(t, "tied-depth", alice, reading("acme/b", 2, 3), reading("acme/a", 1, 3))
	evalID := written.Definition.EvalID
	entry := r.entries(t, evalID)[0]
	for _, want := range []struct {
		day   int
		value float64
	}{{1, 1}, {2, 2}, {3, 1}} {
		window := Range{From: at(want.day, 0, 0), To: at(want.day+1, 0, 0)}
		if project := r.day(t, evalID, "depth", "", window.From); project.LastValue == nil || *project.LastValue != want.value {
			t.Fatalf("the project's row of the %d. holds the last value %s, want %g, the later write's", want.day, showValue(project.LastValue), want.value)
		}
		rows, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, []Entry{entry}, "", []string{"acme/a", "acme/b"}, window, nil)
		if err != nil {
			t.Fatalf("read the window of the %d.: %v", want.day, err)
		}
		if len(rows) != 2 {
			t.Fatalf("the window of the %d. over acme/a and acme/b answers %d rows, want one of each", want.day, len(rows))
		}
		for _, order := range []struct {
			name string
			rows []WindowRow
		}{{"as read", rows}, {"reversed", []WindowRow{rows[1], rows[0]}}} {
			parts := []measures.DayPart{order.rows[0].Part, order.rows[1].Part}
			got := FoldStatistics(entry.Definition, entry.Series, parts, window)["depth"]["value"]
			t.Logf("the %d., rows %s: value %s", want.day, order.name, showValue(got))
			if got == nil || *got != want.value {
				t.Errorf("the window of the %d. over acme/a and acme/b, its rows %s, answers %s; want %g, the later write's, as the project's row holds",
					want.day, order.name, showValue(got), want.value)
			}
		}
	}
}
