package evals

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// documentID is the id of project's evals-md, failing the test when it has none.
func (r rig) documentID(t *testing.T, project string) string {
	t.Helper()
	var id string
	if err := r.database.Pool.QueryRow(t.Context(), `
		select id::text from artifacts where project_key = $1 and issue_key is null and slug = $2
	`, project, DocumentSlug).Scan(&id); err != nil {
		t.Fatalf("find the %s document of %s: %v", DocumentSlug, project, err)
	}
	return id
}

// seededState is what a project holds of the catalogue, as the seed tests compare it: the
// versions of each eval, the versions of each series, the document's versions and text, and its
// blocks as Docs.Blocks answers them.
type seededState struct {
	versions, series, documentVersions int
	text                               string
	blocks                             []model.ArtifactBlock
}

func (r rig) seededState(t *testing.T, project string) seededState {
	t.Helper()
	id := r.documentID(t, project)
	text, _, err := r.documents.TextWithBlocks(t.Context(), id)
	if err != nil {
		t.Fatalf("read the %s document of %s: %v", DocumentSlug, project, err)
	}
	blocks, err := r.documents.Blocks(t.Context(), id)
	if err != nil {
		t.Fatalf("read the blocks of the %s document of %s: %v", DocumentSlug, project, err)
	}
	return seededState{
		versions:         r.count(t, `select count(*) from eval_versions where project_key = $1`, project),
		series:           r.count(t, `select count(*) from eval_series where starts_with(eval_id, $1 || '/')`, project),
		documentVersions: r.count(t, `select count(*) from artifact_versions where artifact_id = $1::uuid`, id),
		text:             text, blocks: blocks,
	}
}

// requireSeeded fails the test unless project holds the catalogue: one version of each eval,
// written by the seed, computed and with no number rule, whose doc_block Docs.Blocks of the
// project's evals-md holds; one section of each eval, its heading the slug and its method the
// catalogue's; and each of its series. It answers the project's state.
func (r rig) requireSeeded(t *testing.T, project string) seededState {
	t.Helper()
	state := r.seededState(t, project)
	page := r.current(t, project)
	if page.Total != len(catalogue) || len(page.Items) != len(catalogue) {
		t.Fatalf("%s holds %d evals, want the catalogue's %d", project, page.Total, len(catalogue))
	}
	bySlug := map[string]Entry{}
	for _, entry := range page.Items {
		bySlug[entry.Definition.Slug] = entry
	}
	series := 0
	for _, want := range catalogue {
		slug := want.definition.Slug
		got, found := bySlug[slug]
		if !found {
			t.Errorf("%s holds no eval %s", project, slug)
			continue
		}
		definition := got.Definition
		if definition.Version != 1 || definition.WrittenVia != ViaSeed || definition.WrittenBy.ID != RegistryActor.ID {
			t.Errorf("%s: version %d written via %s by %+v, want version 1 by the seed", definition.EvalID, definition.Version, definition.WrittenVia, definition.WrittenBy)
		}
		if !definition.Computed() || definition.NumberRule != "" || definition.Source != want.definition.Source || definition.Name != want.definition.Name ||
			!sameTarget(definition.Target, want.definition.Target) || !slices.Equal(definition.TargetJudges, nonNil(want.definition.TargetJudges)) ||
			!slices.Equal(definition.FailureModes, want.definition.FailureModes) {
			t.Errorf("%s holds %+v, want the catalogue's %+v with no number rule", definition.EvalID, definition, want.definition)
		}
		if !hasBlock(state.blocks, definition.DocBlock) {
			t.Errorf("%s: doc_block %q is no block of the %s document", definition.EvalID, definition.DocBlock, DocumentSlug)
		}
		if n := headings(state.text, slug); n != 1 {
			t.Errorf("%s's %s holds %d sections of %s, want one", project, DocumentSlug, n, slug)
		}
		if !strings.Contains(state.text, want.definition.Question) {
			t.Errorf("%s's %s does not hold the question of %s", project, DocumentSlug, slug)
		}
		if got, wanted := len(got.Series), len(want.series); got != wanted {
			t.Errorf("%s holds %d series, want the catalogue's %d", definition.EvalID, got, wanted)
		}
		series += len(got.Series)
	}
	if state.versions != len(catalogue) || state.series != series {
		t.Errorf("%s holds %d versions and %d series versions, want one of each eval and series: %d and %d", project, state.versions, state.series, len(catalogue), series)
	}
	return state
}

// TestEnsureTwiceWritesOneVersionAndOneSectionPerEval: Ensure creates the project's evals-md with
// one section per eval in its first text and writes version 1 of each eval, by the seed, whose
// doc_block resolves, with its series; a second Ensure writes nothing: no version of an eval or a
// series, no section and no version of the document.
func TestEnsureTwiceWritesOneVersionAndOneSectionPerEval(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	first, err := r.registry.Ensure(t.Context(), r.project)
	if err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	if first.Versions != len(catalogue) || first.Sections != len(catalogue) || !first.Created {
		t.Fatalf("the first seed wrote %+v, want %d versions and sections in a new document", first, len(catalogue))
	}
	state := r.requireSeeded(t, r.project)
	second, err := r.registry.Ensure(t.Context(), r.project)
	if err != nil {
		t.Fatalf("seed %s again: %v", r.project, err)
	}
	again := r.seededState(t, r.project)
	if second != (Seeded{}) || again.versions != state.versions || again.series != state.series ||
		again.documentVersions != state.documentVersions || again.text != state.text {
		t.Fatalf("the second seed wrote %+v: versions %d -> %d, series %d -> %d, document versions %d -> %d; want nothing",
			second, state.versions, again.versions, state.series, again.series, state.documentVersions, again.documentVersions)
	}
	r.requireSeeded(t, r.project)
	t.Logf("first seed %+v; %d versions, %d series versions, %d document versions, %d blocks; the second seed wrote %+v",
		first, state.versions, state.series, state.documentVersions, len(state.blocks), second)
}

// TestEnsureAddsTheSectionsAProjectsDocumentLacks: a project whose evals-md exists with another
// eval's section gets every catalogue section added at its end, in one write of the document, and
// the section it held stands; an eval whose method block a person deleted gets a new section and a
// version naming it, carrying its other fields forward.
func TestEnsureAddsTheSectionsAProjectsDocumentLacks(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "review-latency", Caller: alice, Name: Set("Review latency"), DocText: Set("Alice's own method."),
	}); err != nil {
		t.Fatalf("register an eval of alice's: %v", err)
	}
	before := r.count(t, `select count(*) from artifact_versions where artifact_id = $1::uuid`, r.documentID(t, r.project))
	seeded, err := r.registry.Ensure(t.Context(), r.project)
	if err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	if seeded.Created || seeded.Sections != len(catalogue) || seeded.Versions != len(catalogue) {
		t.Fatalf("the seed wrote %+v, want %d sections added to the document it found", seeded, len(catalogue))
	}
	state := r.seededState(t, r.project)
	if headings(state.text, "review-latency") != 1 || !strings.Contains(state.text, "Alice's own method.") {
		t.Fatalf("the seed lost alice's section:\n%s", state.text)
	}
	if state.documentVersions != before+1 {
		t.Fatalf("the document went from %d versions to %d, want one write", before, state.documentVersions)
	}
	bySlug := map[string]Entry{}
	for _, entry := range r.current(t, r.project).Items {
		bySlug[entry.Definition.Slug] = entry
	}
	for _, want := range catalogue {
		slug := want.definition.Slug
		got, found := bySlug[slug]
		if !found || got.Definition.Version != 1 || got.Definition.WrittenVia != ViaSeed || !hasBlock(state.blocks, got.Definition.DocBlock) {
			t.Errorf("the seed into the existing document left %s as found %t, %+v; want version 1 by the seed naming a block of %s", slug, found, got.Definition, DocumentSlug)
		}
		if n := headings(state.text, slug); n != 1 {
			t.Errorf("%s holds %d headings of %s after the seed into the existing document, want 1", DocumentSlug, n, slug)
		}
	}
	target := 3.0
	slug := SlugLegionRework
	evalID := EvalID(r.project, slug)
	set, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: slug, Caller: alice, Target: Set(&target)})
	if err != nil {
		t.Fatalf("set a target on %s: %v", slug, err)
	}
	block := set.Definition.DocBlock
	r.deleteBlock(t, r.documentID(t, r.project), block)
	restored, err := r.registry.Ensure(t.Context(), r.project)
	if err != nil {
		t.Fatalf("seed %s after the delete: %v", r.project, err)
	}
	newest := r.versionsOf(t, evalID)
	state = r.seededState(t, r.project)
	if restored.Sections != 1 || restored.Versions != 1 || newest[0].Version != 3 || newest[0].DocBlock == block ||
		!hasBlock(state.blocks, newest[0].DocBlock) || newest[0].Target == nil || *newest[0].Target != target {
		t.Fatalf("after the delete the seed wrote %+v and %s's newest version is %+v, want one section and a version naming its block with alice's target", restored, slug, newest[0])
	}
	t.Logf("sections added to the found document: %d; the deleted block %s came back as %s at version %d, target %v carried", seeded.Sections, block, newest[0].DocBlock, newest[0].Version, *newest[0].Target)
}

// deleteBlock deletes the block id names from the document artifactID as alice, in a transaction
// the document service joins, as the edit route writes a person's edit.
func (r rig) deleteBlock(t *testing.T, artifactID, id string) {
	t.Helper()
	ctx := t.Context()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := r.documents.Join(ctx, tx)
	defer ledger.Discard()
	edit, err := r.documents.ApplyOps(joined, artifactID, []model.EditOp{{Op: "delete", Block: id}}, alice, nil)
	if err != nil || edit.Applied == 0 {
		t.Fatalf("delete block %s: %+v, %v", id, edit, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
}

// withCatalogue runs the rest of the test with the catalogue changed by change, as a later release
// would change it, and puts the catalogue back when the test ends.
func withCatalogue(t *testing.T, change func(entries []catalogueEntry)) {
	t.Helper()
	saved := catalogue
	changed := slices.Clone(catalogue)
	for i := range changed {
		changed[i].series = slices.Clone(changed[i].series)
	}
	change(changed)
	catalogue = changed
	t.Cleanup(func() { catalogue = saved })
}

// currentOf is project's current definition and series of slug, failing the test when it has
// none.
func (r rig) currentOf(t *testing.T, slug string) (Definition, map[string]Series) {
	t.Helper()
	page := r.current(t, r.project)
	for _, entry := range page.Items {
		if entry.Definition.Slug == slug {
			series := map[string]Series{}
			for _, one := range entry.Series {
				series[one.Series] = one
			}
			return entry.Definition, series
		}
	}
	t.Fatalf("%s holds no eval %s", r.project, slug)
	return Definition{}, nil
}

// showMinN is a series' min_n as a failure states it: "none" where it holds none.
func showMinN(minN *int) string {
	if minN == nil {
		return "none"
	}
	return fmt.Sprint(*minN)
}

// TestAReleaseThatChangesAnEvalCarriesForwardWhatOthersSet: alice sets the fields a person sets on
// a computed eval (R5): deploy frequency's target, person, links and method, and the min_n of
// pr-ci's job series, the one series a read ranks by a min_n. A release that changes deploy
// frequency's name and its series' unit writes one version of each: they hold every field the
// catalogue owns as the release states it, and carry forward alice's target, person, links and
// method block. The same release changes the min_n of pr-ci's job series, which the catalogue
// states for a series' first version alone, as it states the target: pr-ci gets no version, and
// alice's min_n stands. A later release that changes that series' unit writes its next version,
// holding the release's unit and alice's min_n. Alice's method text stands, and a second seed
// writes nothing.
func TestAReleaseThatChangesAnEvalCarriesForwardWhatOthersSet(t *testing.T) {
	// Not parallel: it changes the catalogue the whole process reads (withCatalogue).
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	target, minN, releasedMinN := 15.0, 3, 7
	link := []Link{{Rel: "describedby", Href: "https://dashboards.example/deploys", Label: "Deploys dashboard"}}
	person, err := r.registry.Register(ctx, Registration{
		Project: r.project, Slug: SlugDeployFrequency, Caller: alice, Target: Set(&target), Person: Set("alice"),
		Links: Set(link), DocText: Set("Alice's method: count the deploys."),
	})
	if err != nil {
		t.Fatalf("alice's register write of %s: %v", SlugDeployFrequency, err)
	}
	if _, err := r.registry.Register(ctx, Registration{
		Project: r.project, Slug: SlugPRCI, Caller: alice,
		Series: []SeriesChange{{Series: SeriesJobs, MinN: Set(&minN)}},
	}); err != nil {
		t.Fatalf("alice's register write of %s: %v", SlugPRCI, err)
	}
	jobsOf := func(entries []catalogueEntry, slug string) *Series {
		for i := range entries {
			if entries[i].definition.Slug == slug {
				for j := range entries[i].series {
					if entries[i].series[j].Series == SeriesJobs {
						return &entries[i].series[j]
					}
				}
			}
		}
		t.Fatalf("the catalogue's %s holds no series %s", slug, SeriesJobs)
		return nil
	}

	withCatalogue(t, func(entries []catalogueEntry) {
		for i := range entries {
			if entries[i].definition.Slug == SlugDeployFrequency {
				entries[i].definition.Name = "Deploys a day"
				entries[i].series[0].Unit = "deploy runs"
			}
		}
		jobsOf(entries, SlugPRCI).MinN = &releasedMinN
	})
	released, err := r.registry.Ensure(ctx, r.project)
	if err != nil {
		t.Fatalf("seed %s with the release's catalogue: %v", r.project, err)
	}
	versions := r.versionsOf(t, EvalID(r.project, SlugDeployFrequency))
	newest, series := r.currentOf(t, SlugDeployFrequency)
	deploys := series[measures.SeriesDeploys]
	t.Logf("the release's seed wrote %+v; versions %v; newest name %q unit %q target %s person %q links %d doc block %s; series %+v",
		released, versionNumbers(versions), newest.Name, newest.Unit, showValue(newest.Target), newest.Person, len(newest.Links), newest.DocBlock, deploys)
	if released.Versions != 1 || released.Series != 1 || released.Sections != 0 || len(versions) != 3 || newest.WrittenVia != ViaSeed {
		t.Fatalf("the release's seed wrote %+v and the eval holds versions %v, want one version and one series version by the seed", released, versionNumbers(versions))
	}
	if newest.Name != "Deploys a day" || newest.Unit != "deploys/day" || newest.Target == nil || *newest.Target != target ||
		newest.Person != "alice" || !sameLinks(newest.Links, person.Definition.Links) || newest.DocBlock != person.Definition.DocBlock {
		t.Errorf("the release's version is %+v, want the catalogue's name and unit with alice's target, person, links and method block", newest)
	}
	if deploys.Version != 2 || deploys.Unit != "deploy runs" {
		t.Errorf("the release's series is version %d with unit %q, want version 2 holding the catalogue's unit", deploys.Version, deploys.Unit)
	}
	untouched, untouchedSeries := r.currentOf(t, SlugPRCI)
	if jobs := untouchedSeries[SeriesJobs]; untouched.Version != 1 || jobs.Version != 2 || jobs.MinN == nil || *jobs.MinN != minN {
		t.Errorf("%s, whose job series' min_n alone the release changed, is version %d with its job series at version %d and min_n %s, want version 1 and alice's series version 2 with min_n %d",
			SlugPRCI, untouched.Version, jobs.Version, showMinN(jobs.MinN), minN)
	}

	withCatalogue(t, func(entries []catalogueEntry) { jobsOf(entries, SlugPRCI).Unit = "seconds" })
	later, err := r.registry.Ensure(ctx, r.project)
	if err != nil {
		t.Fatalf("seed %s with the later release's catalogue: %v", r.project, err)
	}
	_, laterSeries := r.currentOf(t, SlugPRCI)
	if jobs := laterSeries[SeriesJobs]; later.Versions != 1 || later.Series != 1 || jobs.Version != 3 || jobs.Unit != "seconds" || jobs.MinN == nil || *jobs.MinN != minN {
		t.Errorf("the later release's seed wrote %+v and left %s's job series at version %d with unit %q and min_n %s, want version 3 holding the release's unit and alice's min_n %d",
			later, SlugPRCI, jobs.Version, jobs.Unit, showMinN(jobs.MinN), minN)
	}
	text, _ := r.document(t)
	if !strings.Contains(text, "Alice's method: count the deploys.") || headings(text, SlugDeployFrequency) != 1 {
		t.Errorf("the release's seed wrote over alice's method:\n%s", text)
	}
	again, err := r.registry.Ensure(ctx, r.project)
	if err != nil || again != (Seeded{}) {
		t.Fatalf("a second seed of the release's catalogue wrote %+v, %v, want nothing", again, err)
	}
}

// TestASeedThatTakesOverAReadingsSlugSetsTheCataloguesTarget: in a project the seed has not
// reached, a reading under a slug of the catalogue registers it as a logged eval with no target,
// and a section whose method says no one has written one. The seed then makes it the catalogue's
// computed eval with the catalogue's target, its inclusiveness and its judges, as the version 1 of
// a project it seeds first holds them, since no person turned that target off, and with the
// catalogue's method in its section, since no person wrote one. An eval a person's register write
// wrote before the seed, naming no target, gets the catalogue's target too, and keeps the fields
// the write set; its section keeps the method the person wrote, where one did, and where none
// did, it gets the catalogue's (TestASeedFillsWhatNoWriteOfATakenOverEvalSet keeps a target a
// write set).
func TestASeedThatTakesOverAReadingsSlugSetsTheCataloguesTarget(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	r.write(t, SlugDeployFrequency, alice, row(t, map[string]any{"series": measures.SeriesDeploys, "row_kind": "deploy", "value": 3, "measured_at": at(4, 9, 0)}))
	for _, slug := range []string{SlugMergeToProduction, SlugPROpenToMerge} {
		r.write(t, slug, alice, row(t, map[string]any{"series": measures.SeriesMinutes, "row_kind": "pull_request", "value": 30, "measured_at": at(4, 9, 0)}))
	}
	if _, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugMergeToProduction, Caller: alice, Person: Set("bob")}); err != nil {
		t.Fatalf("alice's register write of %s: %v", SlugMergeToProduction, err)
	}
	if _, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugPROpenToMerge, Caller: alice, DocText: Set("Alice's own method.")}); err != nil {
		t.Fatalf("alice's register write of %s: %v", SlugPROpenToMerge, err)
	}
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	method := func(definition Definition) string {
		t.Helper()
		text, found, err := r.registry.Method(ctx, r.project, definition.DocBlock)
		if err != nil || !found {
			t.Fatalf("read the method of %s: found %t, %v", definition.EvalID, found, err)
		}
		return text
	}
	catalogued := entryOf(t, SlugDeployFrequency).definition
	taken, _ := r.currentOf(t, SlugDeployFrequency)
	t.Logf("%s after the seed: source %s, target %s inclusive %t judges %v", SlugDeployFrequency, taken.Source, showValue(taken.Target), taken.TargetInclusive, taken.TargetJudges)
	if !taken.Computed() || !sameTarget(taken.Target, catalogued.Target) || taken.TargetInclusive != catalogued.TargetInclusive ||
		!slices.Equal(taken.TargetJudges, catalogued.TargetJudges) {
		t.Errorf("%s, which a reading registered, holds source %s and target %s inclusive %t judges %v after the seed; want the catalogue's %s inclusive %t judges %v",
			SlugDeployFrequency, taken.Source, showValue(taken.Target), taken.TargetInclusive, taken.TargetJudges,
			showValue(catalogued.Target), catalogued.TargetInclusive, catalogued.TargetJudges)
	}
	kept, _ := r.currentOf(t, SlugMergeToProduction)
	if want := entryOf(t, SlugMergeToProduction).definition.Target; !kept.Computed() || !sameTarget(kept.Target, want) || kept.Person != "bob" {
		t.Errorf("%s, which alice's register write of its person alone wrote, holds source %s, target %s and person %q after the seed; want it computed with the catalogue's target %s and person bob",
			SlugMergeToProduction, kept.Source, showValue(kept.Target), kept.Person, showValue(want))
	}
	for _, definition := range []Definition{taken, kept} {
		if text := method(definition); !strings.Contains(text, entryOf(t, definition.Slug).definition.Question) {
			t.Errorf("%s's section, whose method no one wrote, holds %q after the seed; want the catalogue's method", definition.EvalID, text)
		}
	}
	if written, _ := r.currentOf(t, SlugPROpenToMerge); method(written) != "Alice's own method." {
		t.Errorf("%s's section holds %q after the seed, want alice's method", written.EvalID, method(written))
	}
	text, _ := r.document(t)
	for _, slug := range []string{SlugDeployFrequency, SlugMergeToProduction, SlugPROpenToMerge} {
		if headings(text, slug) != 1 {
			t.Errorf("evals-md holds %d sections of %s, want one", headings(text, slug), slug)
		}
	}
}

// TestASeedFillsWhatNoWriteOfATakenOverEvalSet: the seed gives each catalogue eval its target
// before any editor runs (the spec's Order), so where it takes over a slug a reading registered
// before any seed of its project, it writes the catalogue's target, its inclusiveness and its
// judges whatever other field a register write then set: merge-to-production, whose register
// write set its unit alone, holds the catalogue's target after the seed. A target a person's
// register write set is kept, pr-open-to-merge's 90, and so is one a person set and then turned
// off, deploy-frequency's none. The catalogue states pr-ci's job series' min_n as it states a
// target, and no write of a logged eval takes a min_n, so the job series the reading registered
// with none holds the catalogue's after the seed, as one the seed writes first does.
func TestASeedFillsWhatNoWriteOfATakenOverEvalSet(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	minutes := row(t, map[string]any{"series": measures.SeriesMinutes, "row_kind": "pull_request", "value": 30, "measured_at": at(4, 9, 0)})
	r.write(t, SlugMergeToProduction, alice, minutes)
	r.write(t, SlugPROpenToMerge, alice, minutes)
	r.write(t, SlugDeployFrequency, alice, row(t, map[string]any{"series": measures.SeriesDeploys, "row_kind": "deploy", "value": 3, "measured_at": at(4, 9, 0)}))
	r.write(t, SlugPRCI, alice, row(t, map[string]any{"series": SeriesJobs, "row_kind": "run", "value": 4, "measured_at": at(4, 9, 0)}))
	personal, turnedOff := 90.0, 10.0
	for _, registration := range []Registration{
		{Project: r.project, Slug: SlugMergeToProduction, Caller: alice, ComputationChange: ComputationChange{Unit: Set("hours")}},
		{Project: r.project, Slug: SlugPROpenToMerge, Caller: alice, Target: Set(&personal)},
		{Project: r.project, Slug: SlugDeployFrequency, Caller: alice, Target: Set(&turnedOff)},
		{Project: r.project, Slug: SlugDeployFrequency, Caller: alice, Target: Set[*float64](nil)},
	} {
		if _, err := r.registry.Register(ctx, registration); err != nil {
			t.Fatalf("alice's register write of %s: %v", registration.Slug, err)
		}
	}
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	catalogued := entryOf(t, SlugMergeToProduction).definition
	if filled, _ := r.currentOf(t, SlugMergeToProduction); !filled.Computed() || !sameTarget(filled.Target, catalogued.Target) ||
		filled.TargetInclusive != catalogued.TargetInclusive || !slices.Equal(filled.TargetJudges, catalogued.TargetJudges) {
		t.Errorf("%s, which a reading registered and a register write of its unit alone wrote, holds source %s and target %s inclusive %t judges %v after the seed; want the catalogue's %s inclusive %t judges %v",
			SlugMergeToProduction, filled.Source, showValue(filled.Target), filled.TargetInclusive, filled.TargetJudges,
			showValue(catalogued.Target), catalogued.TargetInclusive, catalogued.TargetJudges)
	}
	if kept, _ := r.currentOf(t, SlugPROpenToMerge); !kept.Computed() || !sameTarget(kept.Target, &personal) {
		t.Errorf("%s, whose register write set its target to %v, holds source %s and target %s after the seed; want it computed with that target",
			SlugPROpenToMerge, personal, kept.Source, showValue(kept.Target))
	}
	if off, _ := r.currentOf(t, SlugDeployFrequency); !off.Computed() || off.Target != nil {
		t.Errorf("%s, whose register writes set a target and then turned it off, holds source %s and target %s after the seed; want it computed with none",
			SlugDeployFrequency, off.Source, showValue(off.Target))
	}
	var minN *int
	for _, series := range entryOf(t, SlugPRCI).series {
		if series.Series == SeriesJobs {
			minN = series.MinN
		}
	}
	if _, series := r.currentOf(t, SlugPRCI); minN == nil || series[SeriesJobs].MinN == nil || *series[SeriesJobs].MinN != *minN {
		t.Errorf("%s's series %s, which a reading registered with no min_n, holds min_n %s after the seed; want the catalogue's %s",
			SlugPRCI, SeriesJobs, showMinN(series[SeriesJobs].MinN), showMinN(minN))
	}
}

// TestASeedLeavesATargetAPersonSetToNoneAsNone: a person who sets a target to none on an eval
// whose target is already none has chosen that the number has no target, and the seed leaves it
// alone. The write moves no stored value, so what keeps that choice is the version's own record
// of the fields a person set (set_by_person), which the write stores whether or not a value
// moved: deploy-frequency, a slug a reading registered in a project no seed has reached, holds
// no target after the seed, and the version alice's write left says she set the target and is
// written by her, through the register write. The seed still fills the catalogue's target on an
// eval no person set one on (merge-to-production) and keeps one a person set to a value
// (pr-open-to-merge's 90), and the record survives the seed's own version.
func TestASeedLeavesATargetAPersonSetToNoneAsNone(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	minutes := row(t, map[string]any{"series": measures.SeriesMinutes, "row_kind": "pull_request", "value": 30, "measured_at": at(4, 9, 0)})
	r.write(t, SlugDeployFrequency, alice, row(t, map[string]any{"series": measures.SeriesDeploys, "row_kind": "deploy", "value": 3, "measured_at": at(4, 9, 0)}))
	r.write(t, SlugMergeToProduction, alice, minutes)
	r.write(t, SlugPROpenToMerge, alice, minutes)
	if registered, _ := r.currentOf(t, SlugDeployFrequency); registered.Target != nil {
		t.Fatalf("%s holds target %s before alice's write, so the write would move a value", SlugDeployFrequency, showValue(registered.Target))
	}
	off, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugDeployFrequency, Caller: alice, Target: Set[*float64](nil)})
	if err != nil {
		t.Fatalf("alice's write of no target on %s: %v", SlugDeployFrequency, err)
	}
	personal := 90.0
	if _, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugPROpenToMerge, Caller: alice, Target: Set(&personal)}); err != nil {
		t.Fatalf("alice's write of a target on %s: %v", SlugPROpenToMerge, err)
	}
	t.Logf("alice's write of no target wrote %t, version %d, set by a person: %v", off.Wrote, off.Definition.Version, off.Definition.SetByPerson)
	if !off.Wrote || off.Definition.Version != 2 || !slices.Contains(off.Definition.SetByPerson, fieldTarget) ||
		off.Definition.WrittenVia != ViaRegister || off.Definition.WrittenBy.ID != alice.ID {
		t.Fatalf("alice's write of no target on a target already none left version %d, wrote %t, set by a person %v, written %s by %s; want version 2 recording target, written by alice through the register write",
			off.Definition.Version, off.Wrote, off.Definition.SetByPerson, off.Definition.WrittenVia, off.Definition.WrittenBy.ID)
	}
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	kept, _ := r.currentOf(t, SlugDeployFrequency)
	t.Logf("%s after the seed: target %s, written via %s, set by a person: %v", SlugDeployFrequency, showValue(kept.Target), kept.WrittenVia, kept.SetByPerson)
	if !kept.Computed() || kept.Target != nil || !slices.Contains(kept.SetByPerson, fieldTarget) {
		t.Errorf("%s, whose target alice set to none while it was already none, holds source %s and target %s after the seed, set by a person %v; want it computed with no target, the record kept, and not the catalogue's %s",
			SlugDeployFrequency, kept.Source, showValue(kept.Target), kept.SetByPerson, showValue(entryOf(t, SlugDeployFrequency).definition.Target))
	}
	catalogued := entryOf(t, SlugMergeToProduction).definition
	if filled, _ := r.currentOf(t, SlugMergeToProduction); !sameTarget(filled.Target, catalogued.Target) || catalogued.Target == nil {
		t.Errorf("%s, which no person wrote, holds target %s after the seed; want the catalogue's %s",
			SlugMergeToProduction, showValue(filled.Target), showValue(catalogued.Target))
	}
	if set, _ := r.currentOf(t, SlugPROpenToMerge); !sameTarget(set.Target, &personal) {
		t.Errorf("%s, whose target alice set to %v, holds target %s after the seed; want hers", SlugPROpenToMerge, personal, showValue(set.Target))
	}
}

// TestASeedLeavesAMethodAPersonSetAlone: the method is the other field of a definition the seed
// fills on an eval it takes over, where the eval's section still holds the sentence its
// registration wrote. A person who writes that sentence as the eval's method has chosen it, and
// the write records doc_text although it moves no block and no text, so the seed leaves the
// section as it is rather than writing the catalogue's method over it.
func TestASeedLeavesAMethodAPersonSetAlone(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	r.write(t, SlugDeployFrequency, alice, row(t, map[string]any{"series": measures.SeriesDeploys, "row_kind": "deploy", "value": 3, "measured_at": at(4, 9, 0)}))
	first, _ := r.currentOf(t, SlugDeployFrequency)
	sentence := registrationSentence(first.WrittenVia, first.WrittenBy, first.CreatedAt)
	written, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: SlugDeployFrequency, Caller: alice, DocText: Set(sentence)})
	if err != nil {
		t.Fatalf("alice's write of the method %s holds: %v", SlugDeployFrequency, err)
	}
	t.Logf("alice's write of the method the section holds wrote %t, version %d, set by a person: %v",
		written.Wrote, written.Definition.Version, written.Definition.SetByPerson)
	if !written.Wrote || !slices.Contains(written.Definition.SetByPerson, fieldDocText) || written.Definition.DocBlock != first.DocBlock {
		t.Fatalf("alice's write of the method the section holds left version %d naming block %s, set by a person %v; want a version recording doc_text and naming the block it held",
			written.Definition.Version, written.Definition.DocBlock, written.Definition.SetByPerson)
	}
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	kept, _ := r.currentOf(t, SlugDeployFrequency)
	method, found, err := r.registry.Method(ctx, r.project, kept.DocBlock)
	if err != nil || !found {
		t.Fatalf("read the method of %s: found %t, %v", kept.EvalID, found, err)
	}
	t.Logf("%s's method after the seed: %q", SlugDeployFrequency, method)
	if !sameMethod(method, sentence) {
		t.Errorf("%s's section, whose method alice wrote, holds %q after the seed; want the method she wrote, %q", SlugDeployFrequency, method, sentence)
	}
}

// TestTheSeedTakesNoCatalogueMinNAPersonSet: min_n is the field of a series the seed fills on a
// series it never wrote that holds none, which is how a series a reading registered takes the
// catalogue's (TestASeedFillsWhatNoWriteOfATakenOverEvalSet). A person who sets min_n to none
// moves no value, so the series' own record of the fields a person set is what keeps it, as the
// definition's keeps a target: seriesToWrite writes the catalogue's min_n over a series whose
// record names none and never over one whose record names min_n. The rule is tested here rather
// than through a register write because a register write takes a min_n on a computed eval's job
// series alone (rankedByMinN), and every series the seed takes over belongs to an eval that is
// still logged.
func TestTheSeedTakesNoCatalogueMinNAPersonSet(t *testing.T) {
	t.Parallel()
	entry := entryOf(t, SlugPRCI)
	var catalogued Series
	for _, series := range entry.series {
		if series.Series == SeriesJobs {
			catalogued = series
		}
	}
	if catalogued.MinN == nil {
		t.Fatalf("the catalogue states no min_n of %s's series %s, so the fill has nothing to write", SlugPRCI, SeriesJobs)
	}
	id := EvalID("EVAL", SlugPRCI)
	registered := Series{EvalID: id, Series: SeriesJobs, Version: 1, SeriesComputation: SeriesComputation{Role: RoleHeadline, Agg: AggLast}}
	for _, test := range []struct {
		what    string
		current Series
		want    *int
	}{
		{"a series a reading registered, which no person set a min_n on", registered, catalogued.MinN},
		{"a series whose min_n a person set to none", withPersonsMinN(registered), nil},
	} {
		writes := seriesToWrite(id, entry.series, map[string]Series{SeriesJobs: test.current}, nil)
		index := slices.IndexFunc(writes, func(series Series) bool { return series.Series == SeriesJobs })
		if index < 0 {
			t.Fatalf("%s: the seed writes no version of %s", test.what, SeriesJobs)
		}
		t.Logf("%s: the seed writes min_n %s, record %v", test.what, showMinN(writes[index].MinN), writes[index].SetByPerson)
		if !sameMinN(writes[index].MinN, test.want) {
			t.Errorf("%s: the seed writes min_n %s, want %s", test.what, showMinN(writes[index].MinN), showMinN(test.want))
		}
	}
}

// withPersonsMinN is series as a person's register write of `min_n: null` leaves it: no min_n,
// and the record that a person set it (SeriesChange.applyTo).
func withPersonsMinN(series Series) Series {
	SeriesChange{Series: series.Series, MinN: Set[*int](nil)}.applyTo(&series, time.Time{})
	return series
}

// seedPass runs the seeds of one refresher pass (refresh.load) with seeds, the loop's state, and
// answers the projects whose seed it ran, in the order it ran them.
func (r rig) seedPass(t *testing.T, seeds *seeding) []string {
	t.Helper()
	refresh := r.registry.newRefresh(testBuild, time.Now(), walkBatch)
	if err := refresh.load(t.Context(), seeds); err != nil {
		t.Fatalf("the refresher's load: %v", err)
	}
	return refresh.report.seeded
}

// TestTheRefreshersFirstPassSeedsEveryProjectAndLaterPassesTheOwed: the refresher's first pass in
// a process seeds every project, so the catalogue a release changed reaches a project seeded
// before it, and one made by SQL is seeded. Each later pass seeds the projects no seed has reached
// and those whose seed failed, and no other: EVAL's seed of the release fails once, and the next
// pass seeds EVAL alone, which then holds the release; a pass after it seeds nothing, and one
// after a project is made by SQL seeds that project alone.
func TestTheRefreshersFirstPassSeedsEveryProjectAndLaterPassesTheOwed(t *testing.T) {
	// Not parallel: it changes the catalogue the whole process reads (withCatalogue).
	r := newRig(t)
	ctx := t.Context()
	exec := func(what, statement string) {
		t.Helper()
		if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	exec("make project BYSQL", `insert into projects (key, name, default_assignee) values ('BYSQL', 'Made by SQL', 'alice')`)
	withCatalogue(t, func(entries []catalogueEntry) {
		for i := range entries {
			if entries[i].definition.Slug == SlugDeployFrequency {
				entries[i].definition.Name = "Deploys a day"
			}
		}
	})
	// A sequence is no part of the transaction it is read in, so the first seed of EVAL that
	// writes a version fails and every later one passes.
	exec("count EVAL's seeds", `create sequence evals_test_seeds`)
	exec("fail EVAL's next seed once", `create function evals_test_fail_once() returns trigger language plpgsql as $$
		begin
			if nextval('evals_test_seeds') = 1 then
				raise exception 'the test fails this seed once';
			end if;
			return new;
		end $$`)
	exec("fail EVAL's next seed once", `create trigger evals_test_fail_once before insert on eval_versions
		for each row when (new.project_key = 'EVAL') execute function evals_test_fail_once()`)

	seeds := &seeding{}
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{"BYSQL", r.project}) {
		t.Fatalf("the first pass seeded %v, want every project: BYSQL and %s", seeded, r.project)
	}
	if definition, _ := r.currentOf(t, SlugDeployFrequency); definition.Name == "Deploys a day" || !seeds.failed[r.project] {
		t.Fatalf("the first pass's seed of %s, which the test failed, left %s named %q and failed %v", r.project, SlugDeployFrequency, definition.Name, seeds.failed)
	}
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{r.project}) {
		t.Fatalf("the pass after the failure seeded %v, want %s alone, whose seed failed", seeded, r.project)
	}
	if definition, _ := r.currentOf(t, SlugDeployFrequency); definition.Name != "Deploys a day" || len(seeds.failed) != 0 {
		t.Errorf("after the pass that seeded %s again its %s is named %q and %v failed, want the release's name and none", r.project, SlugDeployFrequency, definition.Name, seeds.failed)
	}
	if seeded := r.seedPass(t, seeds); len(seeded) != 0 {
		t.Errorf("a pass with every project seeded seeded %v, want none", seeded)
	}
	exec("make project LATER", `insert into projects (key, name, default_assignee) values ('LATER', 'Made by SQL later', 'alice')`)
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{"LATER"}) {
		t.Errorf("the pass after LATER was made seeded %v, want LATER alone", seeded)
	}
	for _, project := range []string{"BYSQL", "LATER"} {
		state := r.requireSeeded(t, project)
		t.Logf("the refresher seeded %s: %d versions, %d series versions, %d blocks of %s", project, state.versions, state.series, len(state.blocks), DocumentSlug)
	}
}

// TestASeedRefusedForItsDocumentIsTriedAgainOnceItIsOne: a project whose evals-md is a file is
// refused EVALS_DOCUMENT_NOT_A_DOCUMENT by its seed, which is no failure a later pass seeds again
// as one. While the file stands, no later pass of the refresher runs a seed of it again; once the
// file is gone, the next pass seeds it. FILED is refused by the first pass, which seeds every
// project, and BYPASS, made by SQL with a file of its own after it, by a later pass.
func TestASeedRefusedForItsDocumentIsTriedAgainOnceItIsOne(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	makeFiled := func(project string) {
		t.Helper()
		for _, statement := range []string{
			`insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`,
			`insert into artifacts (project_key, slug, name, kind, created_by) values ($1, 'evals-md', 'evals.md', 'file', '{"kind":"user","id":"alice"}')`,
		} {
			if _, err := r.database.Pool.Exec(ctx, statement, project); err != nil {
				t.Fatalf("make %s with a file for its evals-md: %v", project, err)
			}
		}
	}
	removeFile := func(project string) {
		t.Helper()
		if _, err := r.database.Pool.Exec(ctx, `delete from artifacts where project_key = $1 and slug = 'evals-md'`, project); err != nil {
			t.Fatalf("remove the file of %s: %v", project, err)
		}
	}
	makeFiled("FILED")
	seeds := &seeding{}
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{r.project, "FILED"}) || !seeds.refused["FILED"] || len(seeds.failed) != 0 {
		t.Fatalf("the first pass seeded %v, holds %v refused and %v failed; want %s and FILED seeded, FILED refused and no failure to seed again",
			seeded, seeds.refused, seeds.failed, r.project)
	}
	makeFiled("BYPASS")
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{"BYPASS"}) || !seeds.refused["BYPASS"] {
		t.Fatalf("the pass after BYPASS was made seeded %v and holds %v refused; want BYPASS alone seeded and refused, FILED's file standing", seeded, seeds.refused)
	}
	if seeded := r.seedPass(t, seeds); len(seeded) != 0 {
		t.Errorf("a pass while both files stand seeded %v, want none", seeded)
	}
	removeFile("BYPASS")
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{"BYPASS"}) {
		t.Errorf("the pass after BYPASS's file went seeded %v, want BYPASS alone, FILED's file standing", seeded)
	}
	removeFile("FILED")
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{"FILED"}) || len(seeds.refused) != 0 {
		t.Errorf("the pass after FILED's file went seeded %v and holds %v refused, want FILED seeded and none held", seeded, seeds.refused)
	}
	for _, project := range []string{"FILED", "BYPASS"} {
		state := r.requireSeeded(t, project)
		t.Logf("%s was seeded once its file went: %d versions", project, state.versions)
	}
}

// TestADeletedProjectWhoseSeedFailedIsNotSeededAgain: seeding.note's third outcome, a refusal
// other than EVALS_DOCUMENT_NOT_A_DOCUMENT. A project whose seed failed is held to be seeded again
// by the next pass; once the project's row is gone, that next pass's seed is refused
// PROJECT_NOT_FOUND, which drops the project from the failed set rather than holding it there or
// in the document-refusal set, so no pass after it tries that project again.
func TestADeletedProjectWhoseSeedFailedIsNotSeededAgain(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('GONE', 'Removed later', 'alice')`,
		`create function evals_test_fail_gone() returns trigger language plpgsql as $$
		begin
			raise exception 'the test fails GONE''s seed';
		end $$`,
		`create trigger evals_test_fail_gone before insert on eval_versions
		for each row when (new.project_key = 'GONE') execute function evals_test_fail_gone()`,
	} {
		if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("stage GONE: %v", err)
		}
	}
	seeds := &seeding{}
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{r.project, "GONE"}) || !seeds.failed["GONE"] || len(seeds.refused) != 0 {
		t.Fatalf("the first pass seeded %v, failed %v, refused %v; want %s and GONE seeded, GONE failed and none refused",
			seeded, seeds.failed, seeds.refused, r.project)
	}
	if _, err := r.database.Pool.Exec(ctx, `delete from projects where key = 'GONE'`); err != nil {
		t.Fatalf("delete GONE: %v", err)
	}
	if seeded := r.seedPass(t, seeds); !slices.Equal(seeded, []string{"GONE"}) || len(seeds.failed) != 0 || len(seeds.refused) != 0 {
		t.Fatalf("the pass after GONE was deleted seeded %v, failed %v, refused %v; want GONE tried once and dropped from both sets",
			seeded, seeds.failed, seeds.refused)
	}
	if seeded := r.seedPass(t, seeds); len(seeded) != 0 {
		t.Errorf("the pass after GONE was refused PROJECT_NOT_FOUND seeded %v, want none", seeded)
	}
}

// storedSeries is every (eval slug, series) pair rows names, by slug.
func storedSeries(project string, rows []DayRow, into map[string]map[string]bool) {
	for _, row := range rows {
		slug := strings.TrimPrefix(row.EvalID, project+"/")
		if into[slug] == nil {
			into[slug] = map[string]bool{}
		}
		into[slug][row.Series] = true
	}
}

// seriesRow is the name of the eval_series row of slug's eval that defines the day rows stored
// under series: SeriesJobs for one of pr-ci's job series, and series itself for any other.
func seriesRow(slug, series string) string {
	if slug == SlugPRCI && strings.HasPrefix(series, measures.JobSeriesPrefix) {
		return SeriesJobs
	}
	return series
}

// requireCatalogueHolds fails the test unless every series written holds a series row of its
// eval in the catalogue (seriesRow), and every catalogue series of the evals in want is written
// but those unwritten names by eval, which by their rule no day row is ever written of and none
// may be; it answers how many series it compared.
func requireCatalogueHolds(t *testing.T, written map[string]map[string]bool, want []string, unwritten map[string][]string) int {
	t.Helper()
	compared := 0
	for _, slug := range slices.Sorted(maps.Keys(written)) {
		index := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug })
		if index < 0 {
			t.Errorf("day rows of %s are written, and the catalogue holds no such eval", slug)
			continue
		}
		rows := map[string]bool{}
		for series := range written[slug] {
			row := seriesRow(slug, series)
			rows[row] = true
			if !slices.Contains(seriesNames(catalogue[index]), row) {
				t.Errorf("%s: day rows of series %s are written, and the catalogue holds no series row %s of it", slug, series, row)
			}
			if slices.Contains(unwritten[slug], row) {
				t.Errorf("%s: day rows of series %s are written, which by its rule holds no input", slug, series)
			}
			compared++
		}
		if slices.Contains(want, slug) {
			for _, name := range seriesNames(catalogue[index]) {
				if !rows[name] && !slices.Contains(unwritten[slug], name) {
					t.Errorf("%s: the catalogue holds series %s, and no day row of it is written", slug, name)
				}
			}
		}
	}
	for _, slug := range want {
		if written[slug] == nil {
			t.Errorf("no day row of %s is written", slug)
		}
	}
	return compared
}

// TestEverySeriesTheDeliveryDaysWriteIsInTheCatalogue: over the golden delivery history
// (api/testdata/golden_delivery/fixture.json), every day's delivery and CI rows of every project,
// the zero day's and the unowned-p0 row are built, and every series they name is a series of its
// eval in the catalogue, pr-ci's job series through SeriesJobs; and each catalogue series of the
// evals with day rows is written.
func TestEverySeriesTheDeliveryDaysWriteIsInTheCatalogue(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "api", "testdata", "golden_delivery", "fixture.json"))
	if err != nil {
		t.Fatalf("read the golden delivery history: %v", err)
	}
	var recorded struct {
		Fixture struct {
			Spec deliverytest.Spec `json:"spec"`
		} `json:"fixture"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("decode the golden delivery history: %v", err)
	}
	r := newRig(t)
	ctx := t.Context()
	fixture := deliverytest.Seed(t, r.database.Pool, recorded.Fixture.Spec)
	settings, err := delivery.GetSettings(ctx, r.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	written := map[string]map[string]bool{}
	days, rows := 0, 0
	for day := fixture.From; day.Before(fixture.To); day = day.AddDate(0, 0, 1) {
		reads, err := r.registry.readDeliveryDays(ctx, settings, []keptRepo{{name: settings.DeployRepo, deploys: true}}, []time.Time{measures.Midnight(day)}, nil)
		if err != nil {
			t.Fatalf("read the delivery day %s: %v", day.Format(time.DateOnly), err)
		}
		read := reads[0]
		for _, project := range dayProjects(read) {
			built := read.Rows(project).Rows
			storedSeries(project, built, written)
			rows += len(built)
		}
		days++
	}
	storedSeries(fixture.Project, ZeroDeliveryDay(settings, fixture.Project, fixture.To, []string{settings.DeployRepo}).Rows, written)
	if err := r.registry.WriteUnownedP0Day(ctx, fixture.Project, fixture.To, testBuild); err != nil {
		t.Fatalf("write the unowned-p0 row: %v", err)
	}
	storedSeries(fixture.Project, []DayRow{r.day(t, EvalID(fixture.Project, SlugUnownedP0), SeriesUnownedP0, "", fixture.To)}, written)
	jobs := 0
	for series := range written[SlugPRCI] {
		if seriesRow(SlugPRCI, series) == SeriesJobs {
			jobs++
		}
	}
	compared := requireCatalogueHolds(t, written, append(slices.Clone(deliveryDaySlugs), SlugUnownedP0), nil)
	if jobs == 0 {
		t.Errorf("the history wrote no job series of %s, so the check of SeriesJobs compared nothing", SlugPRCI)
	}
	t.Logf("%s: %d days, %d day rows built; %d series of %d evals written, %d of them job series of %s, each a series of the catalogue",
		fixture.Spec.Project, days, rows, compared, len(written), jobs, SlugPRCI)
}

// TestEverySeriesTheLegionDaysWriteIsInTheCatalogue: over the golden Legion history
// (legion/metrics/testdata/golden_compute), every day's Legion rows and the zero day's are built,
// and every series they name is a series of its eval in the catalogue; and each catalogue series
// of the six Legion evals is written, but the children cut of the splitting eval's root figures
// (metrics.RootFigures), which count roots alone, so that cut holds no issue and no day row.
func TestEverySeriesTheLegionDaysWriteIsInTheCatalogue(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	windows, project, since := seedGoldenLegionHistory(t, r)
	first, last := goldenLegionDays(windows)
	written := map[string]map[string]bool{}
	days, rows := 0, 0
	for day := first; day.Before(last); day = day.AddDate(0, 0, 1) {
		read, err := r.registry.ReadLegionDay(t.Context(), project, day)
		if err != nil {
			t.Fatalf("read the Legion day %s: %v", day.Format(time.DateOnly), err)
		}
		built := read.Rows(since).Rows
		storedSeries(project, built, written)
		rows += len(built)
		days++
	}
	storedSeries(project, ZeroLegionDay(project, last, legionDaySlugs...).Rows, written)
	var rootChildren []string
	for _, figure := range metrics.RootFigures {
		rootChildren = append(rootChildren, metrics.CutSeries(figure, metrics.ScopeChildren))
	}
	compared := requireCatalogueHolds(t, written, legionDaySlugs, map[string][]string{SlugLegionSplitting: rootChildren})
	t.Logf("%s: %d days, %d day rows built; %d series of %d evals written, each a series of the catalogue", project, days, rows, compared, len(written))
}
