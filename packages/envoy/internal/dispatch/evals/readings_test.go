package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
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
	if days := r.count(t, `select count(*) from eval_days where day = '2026-11-03'`); days != 0 {
		t.Errorf("the refused batches left %d day rows on their day, want none", days)
	}
}

// TestABatchThatWouldOverflowADayRowIsRefusedNamingItsRow: a batch whose readings would take a sum
// of a logged eval's day row past what a float holds is refused whole, naming the row whose value
// does it: the second of two readings of 1e308 on one day (the day's sum), a reading of 1e300
// measured over an n of 1e10 (its weighted sum, num), and a reading of 1e308 on a day an earlier
// write left holding one. Each stores no reading, registers no eval and moves no day row; the same
// values spread over days whose sums stay within a float are stored.
func TestABatchThatWouldOverflowADayRowIsRefusedNamingItsRow(t *testing.T) {
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

// TestTwoFirstWritesOfOneSlugRegisterItOnce: two writes of a slug no eval holds, on two
// connections at once, both store their readings, and the eval is registered once: one version,
// written by a reading, the caller its owner, and one section in the project's evals-md, which
// neither write found and one of them created. The test holds the eval's lock until both writes
// wait for it, so each has read that no version exists before either writes one.
func TestTwoFirstWritesOfOneSlugRegisterItOnce(t *testing.T) {
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
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, time.Minute); waiting < 2 {
		t.Fatalf("%d writes wait for the eval's lock, want both", waiting)
	}
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
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, time.Minute); waiting < 1 {
		t.Fatalf("the correction never waited for the eval's lock")
	}
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
	held := New(r.database, pause, r.registry.events)
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

// sameValue reports whether two optional values are both absent or both the same value.
func sameValue(got, want *float64) bool {
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}

// TestAReadingUnderAComputedSlugIsStoredAndFoldedIntoNoDayRow: a reading logged under a computed
// eval's slug, the seed's deploy-frequency, is stored and answered by the items read, its series
// registered unlisted, and no day row of the computed eval moves.
func TestAReadingUnderAComputedSlugIsStoredAndFoldedIntoNoDayRow(t *testing.T) {
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
// repositories, its two rows folded in the order DayWindow answered them and reversed, answers
// the later write's value, which the project's row of that day holds as its last.
func TestALastWindowOverRepositoriesTakesTheLaterWriteOfATie(t *testing.T) {
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
		rows, err := r.registry.DayWindow(t.Context(), []string{r.project}, []Entry{entry}, "", []string{"acme/a", "acme/b"}, window, nil)
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
