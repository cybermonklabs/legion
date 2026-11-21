package evals

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestATargetChangeWritesANewVersionAndCopiesEveryOtherFieldForward: a register write that sets
// the target alone writes version 2, which holds that target and every field version 1 held; a
// write that changes nothing writes no version; and the versions read newest first.
func TestATargetChangeWritesANewVersionAndCopiesEveryOtherFieldForward(t *testing.T) {
	r := newRig(t)
	first, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "review-rounds", Caller: alice,
		Name: Set("Review rounds"), Question: Set("How many rounds does a pull request take?"), Unit: Set("rounds"),
		Direction: Set(DirectionLowerBetter), TargetJudges: Set([]string{"rounds.median"}),
		Links:  Set([]Link{{Rel: "describedby", Href: "https://wiki.example/review-rounds", Label: "the rule"}}),
		Series: []SeriesChange{{Series: "rounds", Agg: Set(AggQuantile)}},
	})
	if err != nil {
		t.Fatalf("register the eval: %v", err)
	}
	target := 2.0
	second, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-rounds", Caller: implementer, Target: Set(&target)})
	if err != nil {
		t.Fatalf("set the target: %v", err)
	}
	unchanged, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-rounds", Caller: alice, Target: Set(&target)})
	if err != nil {
		t.Fatalf("set the same target again: %v", err)
	}
	versions := r.versionsOf(t, first.Definition.EvalID)
	t.Logf("%d versions; v1 %+v; v2 %+v", len(versions), first.Definition, second.Definition)
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("the eval holds versions %v, want 2 then 1", versionNumbers(versions))
	}
	if !second.Wrote || unchanged.Wrote || unchanged.Definition.Version != 2 {
		t.Errorf("the target write wrote %v, the repeat wrote %v at version %d; want a version for the change and none for the repeat",
			second.Wrote, unchanged.Wrote, unchanged.Definition.Version)
	}
	v1, v2 := versions[1], versions[0]
	if v1.Target != nil || v2.Target == nil || *v2.Target != 2 {
		t.Errorf("the target is %v at version 1 and %v at version 2, want off then 2", v1.Target, v2.Target)
	}
	copied := v1
	copied.Version, copied.Target, copied.WrittenBy, copied.CreatedAt = v2.Version, v2.Target, v2.WrittenBy, v2.CreatedAt
	copied.SetByPerson = v2.SetByPerson
	if !sameDefinition(copied, v2) || v2.WrittenVia != ViaRegister || v2.WrittenBy.ID != implementer.ID {
		t.Errorf("version 2 is %+v, want version 1's fields with the target set, written by the implementer through the register write", v2)
	}
	// Each version records the fields a person's write named, whether or not it moved their
	// value, and carries the record before it forward (Definition.SetByPerson): version 1 named
	// the judges and the links, and version 2 the target beside them.
	if want := []string{"links", "target", "target_judges"}; !slices.Equal(v1.SetByPerson, []string{"links", "target_judges"}) || !slices.Equal(v2.SetByPerson, want) {
		t.Errorf("the versions record %v then %v as set by a person, want %v then %v", v1.SetByPerson, v2.SetByPerson, []string{"links", "target_judges"}, want)
	}
	if v2.Name != "Review rounds" || v2.Unit != "rounds" || v2.Direction != DirectionLowerBetter || len(v2.Links) != 1 ||
		!v2.Links[0].RecordedAt.Equal(v1.Links[0].RecordedAt) || v2.DocBlock != v1.DocBlock || v2.DocBlock == "" {
		t.Errorf("version 2 did not carry version 1's name, unit, direction, link and doc block forward: %+v", v2)
	}
}

// TestTwoRegisterWritesAtOnceLandAsTheNextTwoVersions: two register writes of one eval, on two
// connections at once, both succeed, as versions 2 and 3, and version 3 holds both writes'
// fields. The test holds the eval's lock until both writes wait for it.
func TestTwoRegisterWritesAtOnceLandAsTheNextTwoVersions(t *testing.T) {
	r := newRig(t)
	first, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "queue-depth", Caller: alice, Name: Set("Queue depth")})
	if err != nil {
		t.Fatalf("register the eval: %v", err)
	}
	holder, err := r.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(t.Context())
	if err := lockEval(t.Context(), holder, first.Definition.EvalID); err != nil {
		t.Fatalf("hold the eval's lock: %v", err)
	}
	target := 10.0
	changes := []Registration{
		{Project: r.project, Slug: "queue-depth", Caller: alice, Target: Set(&target)},
		{Project: r.project, Slug: "queue-depth", Caller: alice, Unit: Set("issues")},
	}
	failures := make([]error, len(changes))
	var writers sync.WaitGroup
	for index, change := range changes {
		writers.Go(func() { _, failures[index] = r.registry.Register(t.Context(), change) })
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, time.Minute); waiting < 2 {
		t.Fatalf("%d register writes wait for the eval's lock, want both", waiting)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("release the eval's lock: %v", err)
	}
	writers.Wait()
	for index, err := range failures {
		if err != nil {
			t.Errorf("register write %d: %v", index, err)
		}
	}
	versions := r.versionsOf(t, first.Definition.EvalID)
	if len(versions) != 3 || versions[0].Version != 3 || versions[1].Version != 2 || versions[0].Target == nil {
		t.Fatalf("the eval holds versions %v, want 3, 2 and 1, the newest with a target", versionNumbers(versions))
	}
	t.Logf("versions %v; newest target %v, unit %q", versionNumbers(versions), *versions[0].Target, versions[0].Unit)
	if newest := versions[0]; newest.Target == nil || *newest.Target != 10 || newest.Unit != "issues" || newest.Name != "Queue depth" {
		t.Errorf("version 3 is %+v, want both writes' target and unit and version 1's name", newest)
	}
}

// TestDocTextReplacesTheSectionsMethodAndNamesTheNewBlock: a register write's doc_text becomes
// the method block of the eval's section of evals-md, and the version it writes names that
// block; a second doc_text replaces the block, whose id the next version names, and the first
// block and its text are gone, under the one heading of the eval.
func TestDocTextReplacesTheSectionsMethodAndNamesTheNewBlock(t *testing.T) {
	r := newRig(t)
	firstText := "Counts each pull request's review rounds.\n\n- A round is one review that requests changes."
	first, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "rework", Caller: alice, DocText: Set(firstText)})
	if err != nil {
		t.Fatalf("register the eval with its method: %v", err)
	}
	text, blocks := r.document(t)
	if !hasBlock(blocks, first.Definition.DocBlock) || !strings.Contains(text, "A round is one review that requests changes.") {
		t.Fatalf("doc_block %q names no block of evals-md holding the method, which reads:\n%s", first.Definition.DocBlock, text)
	}
	secondText := "Counts the rounds after the first review.\n\n:::callout{title=\"Why\"}\nThe first review always happens.\n:::"
	second, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "rework", Caller: alice, DocText: Set(secondText)})
	if err != nil {
		t.Fatalf("replace the method: %v", err)
	}
	text, blocks = r.document(t)
	t.Logf("doc_block %s then %s; evals-md reads:\n%s", first.Definition.DocBlock, second.Definition.DocBlock, text)
	if second.Definition.Version != 2 || second.Definition.DocBlock == first.Definition.DocBlock {
		t.Errorf("the second method wrote version %d naming block %s, want version 2 naming a new block", second.Definition.Version, second.Definition.DocBlock)
	}
	if !hasBlock(blocks, second.Definition.DocBlock) || hasBlock(blocks, first.Definition.DocBlock) {
		t.Errorf("evals-md holds the new block %v and the old one %v, want the new alone",
			hasBlock(blocks, second.Definition.DocBlock), hasBlock(blocks, first.Definition.DocBlock))
	}
	if strings.Contains(text, "requests changes") || !strings.Contains(text, "rounds after the first review") || !strings.Contains(text, "always happens") {
		t.Errorf("evals-md does not hold the second method in place of the first:\n%s", text)
	}
	if n := headings(text, "rework"); n != 1 {
		t.Errorf("evals-md holds %d headings of rework, want 1", n)
	}
}

// TestARegisterWriteOfTheMethodTheSectionHoldsWritesNothing: a register write whose doc_text is the
// method the eval's section of evals-md holds, as it was sent or with white space around it,
// writes no version of the eval, keeps its method block and writes no version of evals-md; a
// doc_text that differs writes the next version, naming a new block.
func TestARegisterWriteOfTheMethodTheSectionHoldsWritesNothing(t *testing.T) {
	r := newRig(t)
	method := "Counts the rounds after the first review.\n\n:::callout{title=\"Why\"}\nThe first review always happens.\n:::"
	register := func(text string) Registered {
		t.Helper()
		registered, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "rework", Caller: alice, DocText: Set(text)})
		if err != nil {
			t.Fatalf("register the method %q: %v", text, err)
		}
		return registered
	}
	first := register(method)
	documentVersions := func() int {
		return r.count(t, `select count(*) from artifact_versions where artifact_id = $1::uuid`, r.documentID(t, r.project))
	}
	before := documentVersions()
	for _, again := range []string{method, "\n" + method + "\n\n"} {
		repeat := register(again)
		if repeat.Wrote || repeat.Definition.Version != first.Definition.Version || repeat.Definition.DocBlock != first.Definition.DocBlock {
			t.Errorf("the method sent again (%q) wrote %t, standing at version %d naming %s; want nothing written, version %d naming %s",
				again, repeat.Wrote, repeat.Definition.Version, repeat.Definition.DocBlock, first.Definition.Version, first.Definition.DocBlock)
		}
	}
	if after := documentVersions(); after != before {
		t.Errorf("evals-md went from %d versions to %d on the method sent again, want none written", before, after)
	}
	changed := register("Counts every review round.")
	if !changed.Wrote || changed.Definition.Version != first.Definition.Version+1 || changed.Definition.DocBlock == first.Definition.DocBlock {
		t.Errorf("a new method wrote %t at version %d naming %s, want version %d naming a new block",
			changed.Wrote, changed.Definition.Version, changed.Definition.DocBlock, first.Definition.Version+1)
	}
	t.Logf("method sent three times: versions %d, %d; evals-md versions %d before the repeats, %d after the change",
		first.Definition.Version, changed.Definition.Version, before, documentVersions())
}

// TestARegisterWriteThatChangesASeriesAloneWritesAVersion: a register write that changes one
// series of an eval and no field of its definition stores the series' next version, answers that
// it wrote, and logs one eval_versions change of the eval, so a live view of the eval wakes; the
// same write again stores nothing, answers so, and logs nothing.
func TestARegisterWriteThatChangesASeriesAloneWritesAVersion(t *testing.T) {
	r := newRig(t)
	minutes := []SeriesChange{{Series: "minutes", Agg: Set(AggQuantile)}}
	if _, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-time", Caller: alice, Name: Set("Review time"), Series: minutes}); err != nil {
		t.Fatalf("register the eval: %v", err)
	}
	evalID := EvalID(r.project, "review-time")
	head := func() int64 {
		t.Helper()
		cursor, err := delivery.ChangesHead(t.Context(), r.database.Pool)
		if err != nil {
			t.Fatal(err)
		}
		return cursor.Seq
	}
	versionChanges := func(after int64) int {
		return r.count(t, `select count(*) from delivery_changes where seq > $1 and kind = 'eval_versions' and repo = $2 and ref = $3`, after, r.project, evalID)
	}
	for _, write := range []struct {
		what    string
		wrote   bool
		changes int
	}{{"unit set", true, 1}, {"unit set again", false, 0}} {
		seq := head()
		registered, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-time", Caller: alice,
			Series: []SeriesChange{{Series: "minutes", Unit: Set("minutes")}}})
		if err != nil {
			t.Fatalf("%s: %v", write.what, err)
		}
		logged := versionChanges(seq)
		if registered.Wrote != write.wrote || logged != write.changes || registered.Definition.Version != 1 {
			t.Errorf("%s answered wrote %t and logged %d eval_versions changes, the definition at version %d; want wrote %t, %d changes, version 1",
				write.what, registered.Wrote, logged, registered.Definition.Version, write.wrote, write.changes)
		}
		t.Logf("%s: wrote %t, the series at version %d, %d eval_versions changes", write.what, registered.Wrote, registered.Series[0].Version, logged)
	}
}

// TestARegisterWriteOfAComputedEvalNamingItsComputationIsRefused: a computed eval's definition is
// its code (R5). A register write of deploy frequency that names a field of its computation, each
// in a write of its own beside a target, is refused EVAL_FIELD_COMPUTED naming that field as the
// body spells it, and stores nothing: no version and no series version. One write of every field
// a person sets on it is stored: its target, inclusiveness and judges, the owner role, person,
// issue, links and state, and its method; and the min_n of pr-ci's job series, the one series min_n
// is taken on (TestAMinNIsTakenOnlyOnTheSeriesAReadRanksBy). A logged eval takes every field.
func TestARegisterWriteOfAComputedEvalNamingItsComputationIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	evalID := EvalID(r.project, SlugDeployFrequency)
	target, minN := 9.0, 4
	deploys := measures.SeriesDeploys
	link := []Link{{Rel: "via", Href: "https://ci.example/deploys"}}
	stored := func() (int, int) {
		return r.count(t, `select count(*) from eval_versions where eval_id = $1`, evalID),
			r.count(t, `select count(*) from eval_series where eval_id = $1`, evalID)
	}
	for _, test := range []struct {
		field  string
		change Registration
	}{
		{"name", Registration{Name: Set("Deploys")}},
		{"question", Registration{Question: Set("How many deploys a day?")}},
		{"number_rule", Registration{NumberRule: Set("Count the deploys.")}},
		{"decision", Registration{Decision: Set("Whether to deploy more often.")}},
		{"failure_modes", Registration{FailureModes: Set([]FailureMode{{Mode: "A mode.", Guard: "A guard."}})}},
		{"unit", Registration{Unit: Set("deploys a day")}},
		{"direction", Registration{Direction: Set(DirectionLowerBetter)}},
		{"kind", Registration{Kind: Set(KindExperiment)}},
		{"source", Registration{Source: Set(SourceLogged)}},
		{"series[0].unit", Registration{Series: []SeriesChange{{Series: deploys, Unit: Set("runs")}}}},
		{"series[0].role", Registration{Series: []SeriesChange{{Series: deploys, Role: Set(RoleControl)}}}},
		{"series[0].is_primary", Registration{Series: []SeriesChange{{Series: deploys, IsPrimary: Set(false)}}}},
		{"series[0].listed", Registration{Series: []SeriesChange{{Series: deploys, Listed: Set(false)}}}},
		{"series[0].agg", Registration{Series: []SeriesChange{{Series: deploys, Agg: Set(AggMax)}}}},
		{"series[0].links", Registration{Series: []SeriesChange{{Series: deploys, Links: Set(link)}}}},
		{"series[1].series", Registration{Series: []SeriesChange{{Series: deploys, MinN: Set(&minN)}, {Series: "invented", MinN: Set(&minN)}}}},
	} {
		t.Run(test.field, func(t *testing.T) {
			change := test.change
			change.Project, change.Slug, change.Caller, change.Target = r.project, SlugDeployFrequency, alice, Set(&target)
			versions, series := stored()
			_, err := r.registry.Register(ctx, change)
			var refused *Error
			if !errors.As(err, &refused) || refused.Code != CodeFieldComputed || refused.Field != test.field {
				t.Fatalf("the write naming %s was answered %v (%+v), want %s naming %s", test.field, err, refused, CodeFieldComputed, test.field)
			}
			if afterVersions, afterSeries := stored(); afterVersions != versions || afterSeries != series {
				t.Errorf("the refused write moved %s from %d versions and %d series versions to %d and %d", evalID, versions, series, afterVersions, afterSeries)
			}
			t.Logf("refused %s: %s", refused.Field, refused.Message)
		})
	}

	person, err := r.registry.Register(ctx, Registration{
		Project: r.project, Slug: SlugDeployFrequency, Caller: alice,
		Target: Set(&target), TargetInclusive: Set(true), TargetJudges: Set([]string{deploys + ".per_day"}),
		OwnerRole: Set("implementer"), Person: Set("alice"), IssueKey: Set("EVAL-1"), Links: Set(link),
		State: Set(StateActive), DocText: Set("Alice's method: count the deploys."),
	})
	if err != nil {
		t.Fatalf("a write of the fields a person sets on %s: %v", evalID, err)
	}
	if definition := person.Definition; !person.Wrote || definition.Target == nil || *definition.Target != target || !definition.TargetInclusive ||
		definition.Person != "alice" || definition.IssueKey != "EVAL-1" || len(definition.Links) != 1 {
		t.Errorf("the write of a person's fields left %+v, want alice's target, owner, issue and link", definition)
	}
	ranked, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugPRCI, Caller: alice,
		Series: []SeriesChange{{Series: SeriesJobs, MinN: Set(&minN)}}})
	if err != nil {
		t.Fatalf("a write of the min_n of %s's job series: %v", SlugPRCI, err)
	}
	if jobs := ranked.Series[slices.IndexFunc(ranked.Series, func(one Series) bool { return one.Series == SeriesJobs })]; jobs.MinN == nil || *jobs.MinN != minN {
		t.Errorf("the write of %s's job series' min_n left min_n %s, want %d", SlugPRCI, showMinN(jobs.MinN), minN)
	}

	if _, err := r.registry.Register(ctx, Registration{
		Project: r.project, Slug: "review-load", Caller: alice, Name: Set("Review load"), Question: Set("How many reviews wait?"),
		NumberRule: Set("Count the open reviews."), Decision: Set("Whether to add a reviewer."), Unit: Set("reviews"),
		Direction: Set(DirectionLowerBetter), Kind: Set(KindExperiment), Source: Set(SourceLogged),
		Series: []SeriesChange{{Series: "reviews", Unit: Set("reviews"), Agg: Set(AggMax), Listed: Set(true), Links: Set(link)}},
	}); err != nil {
		t.Errorf("a logged eval's register write of every field: %v", err)
	}
}

// TestALinkWhoseHrefIsNoHTTPURLIsRefused: a link's href is an absolute http or https URL (R9),
// since a page draws the link for a person to click. Any other scheme, javascript:, data:,
// mailto:, urn:, and javascript: and ftp: with a host, is refused LINK_HREF_NOT_HTTP, and an href
// with no scheme or no host LINK_HREF_NOT_ABSOLUTE, on every link a write takes: a reading's,
// naming its row and its link, and an eval's and a series', naming the field. A refused write
// stores nothing; an https URL written in capitals is one, and is stored.
func TestALinkWhoseHrefIsNoHTTPURLIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	stored := func() (int, int) {
		return r.count(t, `select count(*) from eval_readings`), r.count(t, `select count(*) from eval_versions`)
	}
	for index, test := range []struct{ href, code string }{
		{"javascript:alert(1)", CodeLinkHrefNotHTTP},
		{"javascript://ci.example/%0Aalert(1)", CodeLinkHrefNotHTTP},
		{"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==", CodeLinkHrefNotHTTP},
		{"mailto:alice@example.com", CodeLinkHrefNotHTTP},
		{"urn:isbn:0451450523", CodeLinkHrefNotHTTP},
		{"ftp://files.example/report.csv", CodeLinkHrefNotHTTP},
		{"/runs/1", CodeLinkHrefNotAbsolute},
		{"//ci.example/runs/1", CodeLinkHrefNotAbsolute},
		{"https:///runs/1", CodeLinkHrefNotAbsolute},
	} {
		readings, versions := stored()
		_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "accuracy", Caller: alice, BuiltBy: testBuild, Rows: []json.RawMessage{
			good(t), row(t, map[string]any{"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
				"links": []any{map[string]any{"rel": "via", "href": test.href}}}),
		}})
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != test.code || len(refused.Rows) != 1 ||
			refused.Rows[0] != (RowError{Index: 1, Code: test.code, Field: "links[0].href", Message: refused.Rows[0].Message}) {
			t.Errorf("%d: a reading linking %q was answered %v (%+v), want %s naming row 1's links[0].href", index, test.href, err, refused, test.code)
		}
		links := []Link{{Rel: "via", Href: test.href}}
		for _, write := range []struct {
			field  string
			change Registration
		}{
			{"links[0].href", Registration{Links: Set(links)}},
			{"series[0].links[0].href", Registration{Series: []SeriesChange{{Series: "score", Links: Set(links)}}}},
		} {
			change := write.change
			change.Project, change.Slug, change.Caller = r.project, "accuracy", alice
			_, err := r.registry.Register(ctx, change)
			if !errors.As(err, &refused) || refused.Code != test.code || refused.Field != write.field {
				t.Errorf("%d: a register write linking %q in %s was answered %v (%+v), want %s naming %s", index, test.href, write.field, err, refused, test.code, write.field)
			}
		}
		if afterReadings, afterVersions := stored(); afterReadings != readings || afterVersions != versions {
			t.Errorf("%d: the refused writes linking %q moved the readings from %d to %d and the versions from %d to %d",
				index, test.href, readings, afterReadings, versions, afterVersions)
		}
	}
	written := r.write(t, "accuracy", alice, row(t, map[string]any{"series": "score", "row_kind": "run", "value": 1, "measured_at": at(3, 10, 0),
		"links": []any{map[string]any{"rel": "via", "href": "HTTPS://CI.EXAMPLE/runs/1"}}}))
	if links := written.Readings[0].Links; len(links) != 1 || links[0].Href != "HTTPS://CI.EXAMPLE/runs/1" {
		t.Errorf("a reading linking an https URL in capitals stored the links %+v, want it as written", links)
	}
}

// TestAMinNIsTakenOnlyOnTheSeriesAReadRanksBy: min_n is the fewest timed results a job needs
// before it is ranked among pr-ci's slowest jobs (R5), and no other read ranks a series by it. A
// register write of a min_n on any other series, of a computed eval or of a logged one, is refused
// EVAL_FIELD_INVALID naming that series' min_n, and stores nothing; the one of pr-ci's job series
// is stored, and the readers of it read it.
func TestAMinNIsTakenOnlyOnTheSeriesAReadRanksBy(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	if _, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: "review-time", Caller: alice, Name: Set("Review time"),
		Series: []SeriesChange{{Series: "minutes"}}}); err != nil {
		t.Fatalf("register review-time: %v", err)
	}
	minN := 4
	for _, test := range []struct {
		slug   string
		series []SeriesChange
		field  string
	}{
		{SlugDeployFrequency, []SeriesChange{{Series: measures.SeriesDeploys, MinN: Set(&minN)}}, "series[0].min_n"},
		{SlugMergeToProduction, []SeriesChange{{Series: measures.SeriesMinutes, MinN: Set[*int](nil)}}, "series[0].min_n"},
		{SlugPRCI, []SeriesChange{{Series: SeriesJobs, MinN: Set(&minN)}, {Series: measures.SeriesMinutes, MinN: Set(&minN)}}, "series[1].min_n"},
		{"review-time", []SeriesChange{{Series: "minutes", MinN: Set(&minN)}}, "series[0].min_n"},
		{"review-time", []SeriesChange{{Series: "minutes", MinN: Set[*int](nil)}}, "series[0].min_n"},
	} {
		evalID := EvalID(r.project, test.slug)
		before := r.count(t, `select count(*) from eval_series where eval_id = $1`, evalID)
		_, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: test.slug, Caller: alice, Series: test.series})
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != CodeFieldInvalid || refused.Field != test.field {
			t.Errorf("a min_n on %s's %s was answered %v (%+v), want %s naming %s", test.slug, test.field, err, refused, CodeFieldInvalid, test.field)
			continue
		}
		if after := r.count(t, `select count(*) from eval_series where eval_id = $1`, evalID); after != before {
			t.Errorf("the refused min_n on %s moved its series versions from %d to %d", test.slug, before, after)
		}
		t.Logf("refused %s on %s: %s", refused.Field, test.slug, refused.Message)
	}
	ranked, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugPRCI, Caller: alice,
		Series: []SeriesChange{{Series: SeriesJobs, MinN: Set(&minN)}}})
	if err != nil {
		t.Fatalf("a min_n on pr-ci's job series: %v", err)
	}
	jobs := ranked.Series[slices.IndexFunc(ranked.Series, func(one Series) bool { return one.Series == SeriesJobs })]
	if !ranked.Wrote || jobs.MinN == nil || *jobs.MinN != minN {
		t.Errorf("a min_n of %d on pr-ci's job series wrote %t and left min_n %s, want it stored", minN, ranked.Wrote, showMinN(jobs.MinN))
	}
}

// TestARegisterWriteMakesNoEvalComputed: a computed eval is the catalogue's, which the seed
// writes; a register write that names a computed source, the catalogue's own or any other, of an
// eval a reading registered or of one it registers, is refused EVAL_FIELD_INVALID naming source,
// and stores nothing, so no logged eval is made computed and left reading "no data" for good. A
// write of source logged is taken.
func TestARegisterWriteMakesNoEvalComputed(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.write(t, "build-minutes", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "value": 4, "measured_at": at(4, 9, 0)}))
	for _, test := range []struct{ slug, source string }{
		{"build-minutes", SourceFoldDeployFrequency},
		{"build-minutes", "computed:evals.Invented"},
		{"new-computed", SourceFoldTiming},
	} {
		evalID := EvalID(r.project, test.slug)
		before := r.count(t, `select count(*) from eval_versions where eval_id = $1`, evalID)
		_, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: test.slug, Caller: alice, Source: Set(test.source)})
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != CodeFieldInvalid || refused.Field != "source" {
			t.Errorf("source %s on %s was answered %v (%+v), want %s naming source", test.source, test.slug, err, refused, CodeFieldInvalid)
			continue
		}
		if after := r.count(t, `select count(*) from eval_versions where eval_id = $1`, evalID); after != before {
			t.Errorf("the refused source on %s moved its versions from %d to %d", test.slug, before, after)
		}
		t.Logf("refused source %s on %s: %s", test.source, test.slug, refused.Message)
	}
	if _, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: "build-minutes", Caller: alice, Source: Set(SourceLogged)}); err != nil {
		t.Errorf("source logged on a logged eval: %v", err)
	}
	if definition, _ := r.currentOf(t, "build-minutes"); definition.Computed() {
		t.Errorf("build-minutes reads source %s, want it logged", definition.Source)
	}
}

// TestAListingChangeOfALoggedSeriesMovesItsStoredDayRows: a logged series registered unlisted keeps
// its readings' day rows unlisted, and a register write that lists it lists every day row it
// holds, the project's and a repository's, in its own transaction, so a window that spans the
// change folds every day of it, not only those written after; unlisting it again unlists them all.
func TestAListingChangeOfALoggedSeriesMovesItsStoredDayRows(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	slug := "queue-depth"
	evalID := EvalID(r.project, slug)
	list := func(listed bool) {
		t.Helper()
		if _, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: slug, Caller: alice, Name: Set("Queue depth"),
			Series: []SeriesChange{{Series: "depth", Listed: Set(listed)}}}); err != nil {
			t.Fatalf("list depth %t: %v", listed, err)
		}
	}
	reading := func(key string, day int) json.RawMessage {
		return row(t, map[string]any{"series": "depth", "row_kind": "queue", "row_key": key, "value": float64(day), "measured_at": at(day, 9, 0),
			"attributes": map[string]any{"repo": "acme/web"}})
	}
	list(false)
	r.write(t, slug, alice, reading("a", 4), reading("b", 5))
	list(true)
	r.write(t, slug, alice, reading("c", 6))
	window := Range{From: at(4, 0, 0), To: at(7, 0, 0)}
	for _, repos := range [][]string{nil, {"acme/web"}} {
		days, err := r.registry.DayWindow(ctx, []string{r.project}, r.entries(t, evalID), "", repos, window, nil)
		if err != nil {
			t.Fatalf("read the window of %v: %v", repos, err)
		}
		t.Logf("the window of %v reads %v", repos, dayLabels(days))
		if len(days) != 3 {
			t.Errorf("the window of %v, which spans the listing, reads %v, want the rows of the 4th, 5th and 6th", repos, dayLabels(days))
		}
	}
	list(false)
	if listed := r.count(t, `select count(*) from eval_days where eval_id = $1 and listed`, evalID); listed != 0 {
		t.Errorf("%d day rows of depth are listed after it was unlisted, want none", listed)
	}
}

// TestEveryFieldAPersonSetsIsRecordedAsSetByOne: the seed fills a field of the catalogue only
// where no version records a person setting it, so a field a person may set that the write does
// not record would be a write the next boot could undo, which is the defect this record closes
// (R5). Every field of a register write that is a person's to set, each Field of Registration
// and of SeriesChange outside the computation, is named by personFields, and setting it alone
// names it alone. A field added to either and missed there fails here.
func TestEveryFieldAPersonSetsIsRecordedAsSetByOne(t *testing.T) {
	for _, change := range []struct {
		what     string
		value    reflect.Value
		recorded func(value reflect.Value) []string
	}{
		{"a register write", reflect.New(reflect.TypeFor[Registration]()).Elem(),
			func(value reflect.Value) []string { return value.Interface().(Registration).personFields() }},
		{"a series of one", reflect.New(reflect.TypeFor[SeriesChange]()).Elem(),
			func(value reflect.Value) []string { return value.Interface().(SeriesChange).personFields() }},
	} {
		counted := 0
		for index := range change.value.NumField() {
			field := change.value.Type().Field(index)
			// Every field a person sets is a Field[T]; the computation's are in the embedded
			// change of their own (ComputationChange, SeriesComputationChange), which the
			// catalogue states on a computed eval and no seed reads a record of.
			if field.Anonymous || field.Type.Kind() != reflect.Struct {
				continue
			}
			if _, isField := field.Type.FieldByName("Set"); !isField {
				continue
			}
			one := reflect.New(change.value.Type()).Elem()
			one.Field(index).FieldByName("Set").SetBool(true)
			recorded := change.recorded(one)
			if len(recorded) != 1 {
				t.Errorf("%s setting %s alone records %v as set by a person, want that one field", change.what, field.Name, recorded)
				continue
			}
			counted++
			t.Logf("%s setting %s alone records %q", change.what, field.Name, recorded[0])
		}
		if recorded := change.recorded(change.value); len(recorded) != 0 {
			t.Errorf("%s that sets nothing records %v as set by a person, want none", change.what, recorded)
		}
		if counted == 0 {
			t.Errorf("%s holds no field a person sets, so the comparison compares nothing", change.what)
		}
	}
}

func versionNumbers(versions []Definition) []int {
	numbers := make([]int, len(versions))
	for index, version := range versions {
		numbers[index] = version.Version
	}
	return numbers
}

// versionsOf is every version of evalID, newest first: one page of EvalVersions that holds them
// all, failing the test where the eval holds more than the page or none.
func (r rig) versionsOf(t *testing.T, evalID string) []Definition {
	t.Helper()
	_, versions, found, err := r.registry.EvalVersions(t.Context(), evalID, 200, 0)
	if err != nil || !found || versions.Total != len(versions.Items) {
		t.Fatalf("read the versions of %s: found %t, %d of %d, %v", evalID, found, len(versions.Items), versions.Total, err)
	}
	return versions.Items
}
