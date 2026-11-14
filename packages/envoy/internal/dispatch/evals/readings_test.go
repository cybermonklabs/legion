package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	page, err := r.registry.Current(t.Context(), r.project, "", 10, 0)
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

// TestAReadingUnderAComputedSlugIsStoredAndFoldedIntoNoDayRow: a reading logged under a computed
// eval's slug is stored and answered by the items read, its series registered unlisted, and no
// day row of the computed eval moves.
func TestAReadingUnderAComputedSlugIsStoredAndFoldedIntoNoDayRow(t *testing.T) {
	r := newRig(t)
	if _, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "deploy-frequency", Caller: RegistryActor,
		Kind: Set(KindDelivery), Source: Set("computed:delivery/measures.Compute"), Unit: Set("deploys/day"),
		Series: []SeriesChange{{Series: "deploys", Agg: Set(AggSum)}},
	}); err != nil {
		t.Fatalf("register the computed eval: %v", err)
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
// was computed is stored and folded into no day row; once a register write makes the eval logged,
// a reading of the same value is folded into that day's row, and a correction of the first,
// unfolded reading adds its own value and takes nothing away, so the day holds the second
// reading's value and the correction's.
func TestACorrectionOfAReadingNeverFoldedTakesNothingFromItsDayRow(t *testing.T) {
	r := newRig(t)
	register := func(source string) {
		t.Helper()
		if _, err := r.registry.Register(t.Context(), Registration{
			Project: r.project, Slug: "review-load", Caller: alice, Kind: Set(KindExperiment), Source: Set(source),
			Series: []SeriesChange{{Series: "minutes", Agg: Set(AggQuantile)}},
		}); err != nil {
			t.Fatalf("register review-load as %s: %v", source, err)
		}
	}
	register("computed:evals.ComputedForTheTest")
	unfolded := r.write(t, "review-load", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 7, "measured_at": at(5, 9, 0)})).Readings[0]
	register(SourceLogged)
	r.write(t, "review-load", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "b", "value": 7, "measured_at": at(5, 10, 0)}))
	evalID := EvalID(r.project, "review-load")
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
