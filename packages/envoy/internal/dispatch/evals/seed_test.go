package evals

import (
	"context"
	"encoding/json"
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
	page, err := r.registry.Current(t.Context(), project, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
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
	newest, err := r.registry.Versions(t.Context(), evalID)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", evalID, err)
	}
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
	page, err := r.registry.Current(t.Context(), r.project, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", r.project, err)
	}
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

// TestAReleaseThatChangesAnEvalCarriesForwardWhatOthersSet: alice sets deploy frequency's target,
// person, links, unit and method, and min_n and a link on its series, and changes merge to
// production's unit and its series' min_n. A release that changes deploy frequency's name and its
// series' unit writes one version of each: they hold every field the catalogue owns as the
// release states it, and carry forward alice's target, person, links and method block, and the
// series' link. Merge to production, which the release did not change, gets no version, so
// alice's unit and min_n stand; alice's method text stands; and a second seed writes nothing.
func TestAReleaseThatChangesAnEvalCarriesForwardWhatOthersSet(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	target, minN := 15.0, 3
	link := []Link{{Rel: "describedby", Href: "https://dashboards.example/deploys", Label: "Deploys dashboard"}}
	person, err := r.registry.Register(ctx, Registration{
		Project: r.project, Slug: SlugDeployFrequency, Caller: alice, Target: Set(&target), Person: Set("alice"),
		Links: Set(link), Unit: Set("deploys a day"), DocText: Set("Alice's method: count the deploys."),
		Series: []SeriesChange{{Series: measures.SeriesDeploys, MinN: Set(&minN), Links: Set(link)}},
	})
	if err != nil {
		t.Fatalf("alice's register write of %s: %v", SlugDeployFrequency, err)
	}
	if _, err := r.registry.Register(ctx, Registration{
		Project: r.project, Slug: SlugMergeToProduction, Caller: alice, Unit: Set("mins"),
		Series: []SeriesChange{{Series: measures.SeriesMinutes, MinN: Set(&minN)}},
	}); err != nil {
		t.Fatalf("alice's register write of %s: %v", SlugMergeToProduction, err)
	}

	withCatalogue(t, func(entries []catalogueEntry) {
		for i := range entries {
			if entries[i].definition.Slug == SlugDeployFrequency {
				entries[i].definition.Name = "Deploys a day"
				entries[i].series[0].Unit = "deploy runs"
			}
		}
	})
	released, err := r.registry.Ensure(ctx, r.project)
	if err != nil {
		t.Fatalf("seed %s with the release's catalogue: %v", r.project, err)
	}
	versions, err := r.registry.Versions(ctx, EvalID(r.project, SlugDeployFrequency))
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	newest, series := r.currentOf(t, SlugDeployFrequency)
	deploys := series[measures.SeriesDeploys]
	t.Logf("the release's seed wrote %+v; versions %v; newest name %q unit %q target %v person %q links %d doc block %s; series %+v",
		released, versionNumbers(versions), newest.Name, newest.Unit, *newest.Target, newest.Person, len(newest.Links), newest.DocBlock, deploys)
	if released.Versions != 1 || released.Series != 1 || released.Sections != 0 || len(versions) != 3 || newest.WrittenVia != ViaSeed {
		t.Fatalf("the release's seed wrote %+v and the eval holds versions %v, want one version and one series version by the seed", released, versionNumbers(versions))
	}
	if newest.Name != "Deploys a day" || newest.Unit != "deploys/day" || newest.Target == nil || *newest.Target != target ||
		newest.Person != "alice" || !sameLinks(newest.Links, person.Definition.Links) || newest.DocBlock != person.Definition.DocBlock {
		t.Errorf("the release's version is %+v, want the catalogue's name and unit with alice's target, person, links and method block", newest)
	}
	if deploys.Version != 3 || deploys.Unit != "deploy runs" || deploys.MinN != nil || len(deploys.Links) != 1 {
		t.Errorf("the release's series is %+v, want version 3 holding the catalogue's fields and alice's link", deploys)
	}
	untouched, untouchedSeries := r.currentOf(t, SlugMergeToProduction)
	if minutes := untouchedSeries[measures.SeriesMinutes]; untouched.Version != 2 || untouched.Unit != "mins" || minutes.MinN == nil || *minutes.MinN != minN {
		t.Errorf("%s, which the release did not change, is version %d with unit %q and min_n %v, want alice's version 2 as she left it", SlugMergeToProduction, untouched.Version, untouched.Unit, minutes.MinN)
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

// TestTheBootPassSeedsProjectsMadeBySQL: two projects inserted by SQL hold no seeded eval, which
// UnseededProjects answers; the boot pass, under its lease, seeds both, after which none is
// unseeded.
func TestTheBootPassSeedsProjectsMadeBySQL(t *testing.T) {
	r := newRig(t)
	if _, err := r.database.Pool.Exec(t.Context(), `insert into projects (key, name) values ('BYSQL', 'Made by SQL')`); err != nil {
		t.Fatalf("make project BYSQL: %v", err)
	}
	unseeded, err := r.registry.UnseededProjects(t.Context())
	if err != nil || !slices.Equal(unseeded, []string{"BYSQL", r.project}) {
		t.Fatalf("the unseeded projects are %v, %v, want BYSQL and %s", unseeded, err, r.project)
	}
	leases, err := r.database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.registry.SeedEveryProject(ctx, leases)
	}()
	deadline := time.Now().Add(time.Minute)
	for len(unseeded) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if unseeded, err = r.registry.UnseededProjects(t.Context()); err != nil {
			t.Fatalf("read the unseeded projects: %v", err)
		}
	}
	cancel()
	<-stopped
	if len(unseeded) != 0 {
		t.Fatalf("a minute after the boot pass started, %v hold no seeded eval", unseeded)
	}
	for _, project := range []string{"BYSQL", r.project} {
		state := r.requireSeeded(t, project)
		t.Logf("the boot pass seeded %s: %d versions, %d series versions, %d blocks of %s", project, state.versions, state.series, len(state.blocks), DocumentSlug)
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

// requireCatalogueHolds fails the test unless every series written holds a series row of its
// eval in the catalogue (SeriesRow), and every catalogue series of the evals in want is written
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
			row := SeriesRow(slug, series)
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
		read, err := r.registry.ReadDeliveryDay(ctx, settings, day)
		if err != nil {
			t.Fatalf("read the delivery day %s: %v", day.Format(time.DateOnly), err)
		}
		for _, project := range read.Projects() {
			built := read.Rows(project).Rows
			storedSeries(project, built, written)
			rows += len(built)
		}
		days++
	}
	storedSeries(fixture.Project, ZeroDeliveryDay(settings, fixture.Project, fixture.To).Rows, written)
	if err := r.registry.WriteUnownedP0Day(ctx, fixture.Project, fixture.To, testBuild); err != nil {
		t.Fatalf("write the unowned-p0 row: %v", err)
	}
	storedSeries(fixture.Project, []DayRow{r.day(t, EvalID(fixture.Project, SlugUnownedP0), SeriesUnownedP0, "", fixture.To)}, written)
	jobs := 0
	for series := range written[SlugPRCI] {
		if SeriesRow(SlugPRCI, series) == SeriesJobs {
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
