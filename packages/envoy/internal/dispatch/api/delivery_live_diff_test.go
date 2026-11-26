package api

//ci:reads packages/contracts/fixtures/live/* TestTheLiveFixturesAreWhatTheDiffWrites

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swaggest/jsonschema-go"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// updateFixtures is this package's one -update flag: a test binary takes one flag of a name, and
// four tests here write files the repository commits: packages/contracts/fixtures/live,
// packages/contracts/fixtures/ask-reply-recipients.json, testdata/routes.txt and
// testdata/status-change.
var updateFixtures = flag.Bool("update", false, "rewrite the fixtures the tests of this package commit: packages/contracts/fixtures/live, packages/contracts/fixtures/ask-reply-recipients.json, testdata/routes.txt and testdata/status-change")

// liveJSONField is one JSON field of a struct as encoding/json writes it: its name and type.
type liveJSONField struct {
	name string
	typ  reflect.Type
}

// liveJSONFields lists t's JSON fields: each exported field by its tag's name (or its Go name),
// skipping `json:"-"`, with an untagged embedded struct's fields flattened into its parent's.
func liveJSONFields(t reflect.Type) []liveJSONField {
	var fields []liveJSONField
	for i := range t.NumField() {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct:
			fields = append(fields, liveJSONFields(field.Type)...)
		case field.IsExported():
			if name == "" {
				name = field.Name
			}
			fields = append(fields, liveJSONField{name: name, typ: field.Type})
		}
	}
	return fields
}

func TestEveryResponseFieldHasOneClass(t *testing.T) {
	t.Parallel()
	// Every kind the hub serves, so a kind added to deliveryLiveKinds is held to this at once.
	for _, kind := range slices.Sorted(maps.Keys(deliveryLiveKinds)) {
		classes, ok := deliveryLiveKinds[kind]
		if !ok {
			t.Fatalf("%s: no classes", kind)
		}
		classOf := map[string][]string{}
		note := func(class string, names []string) {
			for _, name := range names {
				classOf[name] = append(classOf[name], class)
			}
		}
		note("lists", classes.lists)
		note("maps", slices.Collect(maps.Keys(classes.maps)))
		note("counts", classes.counts)
		note("whole", slices.Collect(maps.Keys(classes.whole)))
		note("withChanges", classes.withChanges)
		note("out", classes.out)

		fields := map[string]reflect.Type{}
		for _, field := range liveJSONFields(classes.body) {
			if _, twice := fields[field.name]; twice {
				t.Errorf("%s: %s writes %q twice", kind, classes.body, field.name)
			}
			fields[field.name] = field.typ
			switch got := classOf[field.name]; len(got) {
			case 0:
				t.Errorf("%s: field %q is in no class", kind, field.name)
			case 1:
			default:
				t.Errorf("%s: field %q is in %d classes: %v", kind, field.name, len(got), got)
			}
		}
		for name, in := range classOf {
			if _, ok := fields[name]; !ok {
				t.Errorf("%s: class %v names %q, which %s does not write", kind, in, name, classes.body)
			}
		}

		// Each class's shape: a list's items carry an id (each shape of an item that is one of
		// several, jsonschema.OneOfExposer, carries its own), a count map is two levels of counts, a
		// map is keyed by string, and the subfields a whole field's or a map entry's revision
		// leaves out exist.
		for _, list := range classes.lists {
			typ := fields[list]
			if typ == nil || typ.Kind() != reflect.Slice || typ.Elem().Kind() != reflect.Struct {
				t.Errorf("%s: list %q is %v, want a slice of structs", kind, list, typ)
				continue
			}
			shapes := []reflect.Type{typ.Elem()}
			if union, ok := reflect.Zero(typ.Elem()).Interface().(jsonschema.OneOfExposer); ok {
				shapes = shapes[:0]
				for _, shape := range union.JSONSchemaOneOf() {
					shapes = append(shapes, reflect.TypeOf(shape))
				}
			}
			for _, shape := range shapes {
				if !slices.ContainsFunc(liveJSONFields(shape), func(f liveJSONField) bool { return f.name == "id" }) {
					t.Errorf("%s: list %q's items of %s have no id", kind, list, shape)
				}
			}
		}
		for name, excluded := range classes.maps {
			typ := fields[name]
			if typ == nil || typ.Kind() != reflect.Map || typ.Key().Kind() != reflect.String {
				t.Errorf("%s: map %q is %v, want a map by string", kind, name, typ)
				continue
			}
			if len(excluded) == 0 {
				continue
			}
			if typ.Elem().Kind() != reflect.Struct {
				t.Errorf("%s: map %q's entries are %v, so its revision has no subfield %v to leave out", kind, name, typ.Elem(), excluded)
				continue
			}
			sub := map[string]bool{}
			for _, field := range liveJSONFields(typ.Elem()) {
				sub[field.name] = true
			}
			for _, left := range excluded {
				if !sub[left] {
					t.Errorf("%s: %q's entry revision leaves out %q, which an entry does not hold", kind, name, left)
				}
			}
		}
		for _, name := range classes.counts {
			if typ := fields[name]; typ != reflect.TypeFor[map[string]map[string]int]() {
				t.Errorf("%s: counts %q is %v, want map[string]map[string]int", kind, name, typ)
			}
		}
		for name, excluded := range classes.whole {
			if len(excluded) == 0 {
				continue
			}
			sub := map[string]bool{}
			for _, field := range liveJSONFields(fields[name]) {
				sub[field.name] = true
			}
			for _, left := range excluded {
				if !sub[left] {
					t.Errorf("%s: %q's revision leaves out %q, which it does not hold", kind, name, left)
				}
			}
		}
	}
}

// TestEveryLiveKindDeclaresItsBehaviourOnItsEntry: the hub reads a view's kind in one place, its
// entry in deliveryLiveKinds, so a kind added there is parsed, read and refused by its own entry
// rather than by an `if` in each of the hub's files. A new kind with no parse or no read would
// reach a nil call at the first subscription; a new kind that names nothing it reads would tell a
// sub nothing when its deployment is not configured (viewReads.refusal).
func TestEveryLiveKindDeclaresItsBehaviourOnItsEntry(t *testing.T) {
	t.Parallel()
	for _, kind := range slices.Sorted(maps.Keys(deliveryLiveKinds)) {
		classes := deliveryLiveKinds[kind]
		if classes.parse == nil {
			t.Errorf("%s: no parse: a subscription of this kind resolves nothing", kind)
		}
		if classes.read == nil {
			t.Errorf("%s: no read: a view of this kind answers nothing", kind)
		}
		// A view reads the deployment's delivery rows, one project's daemon, one project's eval
		// registry, or one eval's rows; the first names no project but the project timeline's
		// (oneProject), and every other view names the one project it shows
		// (deliveryLiveClasses.namesProject).
		if classes.reads != viewReadsDelivery && classes.reads != viewReadsLegion && classes.reads != viewReadsEvals && classes.reads != viewReadsEvalItems {
			t.Errorf("%s: reads = %d: a view reads the deployment's delivery rows, one project's daemon, one project's evals or one eval's rows", kind, classes.reads)
		}
		// The items view alone names an eval, by its slug, beside its project (viewReads.namesEval).
		if classes.reads.namesEval() != (kind == evalItemsView) {
			t.Errorf("%s: names an eval %t, want the items view alone to", kind, classes.reads.namesEval())
		}
	}
	if deliveryLiveKinds[legionBoardView].unownedP0 {
		t.Error("the Legion board shows no unowned P0 list, so a P0 write must not mark it by itself")
	}
	// The Legion board shows its daemon's state, which the sampler's read of that daemon marks by
	// the board's project (dirtyLegionProject), so the board names its project.
	if board := deliveryLiveKinds[legionBoardView]; board.reads != viewReadsLegion || !board.namesProject() {
		t.Error("the Legion board does not read its project's daemon, so the sampler's read of that daemon marks no board")
	}
	// The items view (dispatch://INFRA-2918, D9) answers the items route's body, its rows sent
	// whole, since they carry no id; it reads one eval's rows of one project, which it names, and
	// no P0 write nor any version of another eval marks it.
	items := deliveryLiveKinds[evalItemsView]
	if items.body != reflect.TypeFor[evalItemsResponse]() || len(items.lists)+len(items.maps)+len(items.counts) != 0 ||
		!slices.Equal(slices.Sorted(maps.Keys(items.whole)), []string{"compare", "items", "limit", "offset", "total", "window"}) {
		t.Errorf("the items view sends %v of %v whole, lists %v, maps %v, counts %v; want every field of the items route's body whole",
			slices.Sorted(maps.Keys(items.whole)), items.body, items.lists, items.maps, items.counts)
	}
	if items.reads != viewReadsEvalItems || !items.namesProject() || items.unownedP0 || len(items.evalVersions) != 0 {
		t.Errorf("the items view reads %d, names its project %t, shows the unowned P0 list %t and reads the versions of %v; want one eval's rows of the project it names alone",
			items.reads, items.namesProject(), items.unownedP0, items.evalVersions)
	}
	// The project timeline is one project's rows of the deployment's delivery rows: it names its
	// project, a delivery change marks it, its read holds the delivery settings, and a deployment
	// with none tells its subscribers so (viewReads.refusal). It carries no measures, so it shows
	// no unowned P0 list and reads no eval's version, and only it of the delivery views names a
	// project.
	projectTimeline := deliveryLiveKinds[deliveryLiveProjectTimeline]
	if !projectTimeline.namesProject() || projectTimeline.reads != viewReadsDelivery {
		t.Errorf("project-timeline: names a project = %t, reads = %d; want a view of one project's delivery rows", projectTimeline.namesProject(), projectTimeline.reads)
	}
	if projectTimeline.unownedP0 || len(projectTimeline.evalVersions) > 0 {
		t.Errorf("project-timeline: unownedP0 = %t, evalVersions = %v; want neither, since it carries no measures", projectTimeline.unownedP0, projectTimeline.evalVersions)
	}
	if code, _, refused := projectTimeline.reads.refusal(delivery.ErrNoSettings); !refused || code != deliveryNotConfiguredCode {
		t.Errorf("project-timeline: a deployment with no delivery settings is refused %q (%t), want %s", code, refused, deliveryNotConfiguredCode)
	}
	for _, kind := range []string{deliveryLiveTimeline, deliveryLiveMeasures, deliveryLivePipeline} {
		if deliveryLiveKinds[kind].namesProject() {
			t.Errorf("%s names a project, want the deployment's view it is", kind)
		}
	}
}

// The fixture world: one deploy repository, one author, sessions and issues by number.

var liveFrom = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

const liveRepo = "acme/widgets"

// livePR is a window pull request of the deploy repository, merged n hours into the window, by
// octocat in session s-1, naming ACME-1 (a P1 of the ACME/api component), waiting to deploy.
func livePR(n int) delivery.DeliveryPRView {
	merged := liveFrom.Add(time.Duration(n) * time.Hour)
	issue, title, priority, agent := "ACME-1", "First issue", "P1", "s-1"
	return delivery.DeliveryPRView{
		ID: liveRepo + "#" + strconv.Itoa(n), Repo: liveRepo, Number: n, Title: "feat: change " + strconv.Itoa(n),
		URL: "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(n), Author: "octocat",
		CreatedAt: &merged, MergedAt: &merged, Issue: &issue, IssueTitle: &title, Priority: &priority,
		Components: []string{"ACME/api"}, Sessions: []string{"s-1"}, ParentAgent: &agent,
		DeployedStatus: delivery.DeployedStatusWaiting,
	}
}

// withIssue is pr naming key, titled title.
func withIssue(pr delivery.DeliveryPRView, key, title string) delivery.DeliveryPRView {
	pr.Issue, pr.IssueTitle = &key, &title
	return pr
}

// shippedBy is pr shipped by run at.
func shippedBy(pr delivery.DeliveryPRView, run int64, at time.Time) delivery.DeliveryPRView {
	pr.DeployRun, pr.DeployedAt, pr.DeployedStatus = &run, &at, delivery.DeployedStatusDeployed
	return pr
}

// liveRun is a deploy run started n hours into the window whose production job succeeded an hour
// later, shipping prs.
func liveRun(id int64, n int, prs ...delivery.DeliveryPRView) delivery.DeliveryRunView {
	started := liveFrom.Add(time.Duration(n) * time.Hour)
	completed := started.Add(time.Hour)
	success := delivery.DeliveryJobConclusionSuccess
	runSuccess := delivery.DeliveryRunConclusionSuccess
	shipped := []delivery.DeliveryShippedPRView{}
	for _, pr := range prs {
		shipped = append(shipped, delivery.DeliveryShippedPRView{ID: pr.ID, Title: pr.Title})
	}
	return delivery.DeliveryRunView{
		ID: id, URL: "https://github.com/" + liveRepo + "/actions/runs/" + strconv.FormatInt(id, 10),
		HeadSHA: fmt.Sprintf("%040d", id), HeadAt: started.Add(-10 * time.Minute), StartedAt: started,
		CompletedAt: &completed, Conclusion: &runSuccess,
		Production: &delivery.DeliveryRunProductionView{Conclusion: &success, CompletedAt: &completed},
		FailedJobs: []delivery.DeliveryRunJobView{}, PRs: shipped,
	}
}

// liveTimelineOf is the timeline body for [liveFrom, to) holding prs, waiting and runs, its maps and
// counts computed as the GET route computes them (deliveryFacetCounts and deliveryColorCounts over
// the rows, no selection), its measures counting the runs as deploys.
func liveTimelineOf(to time.Time, prs []delivery.DeliveryPRView, waiting []delivery.DeliveryWaitingPRView, runs []delivery.DeliveryRunView) deliveryTimelineWithMeasures {
	rows := make([]deliveryRow, len(prs))
	issueTitles := map[string]string{}
	components := map[string]delivery.DeliveryComponentView{}
	sessionTitles := map[string]string{}
	sessionPRCounts := map[string]int{}
	for i, pr := range prs {
		rows[i] = newDeliveryRow(pr)
		if pr.Issue != nil {
			issueTitles[*pr.Issue] = *pr.IssueTitle
		}
		for _, component := range pr.Components {
			components[component] = delivery.DeliveryComponentView{Title: strings.TrimPrefix(component, "ACME/")}
		}
		if len(pr.Sessions) > 0 {
			sessionPRCounts[pr.Sessions[0]]++
			sessionTitles[pr.Sessions[0]] = "Session " + pr.Sessions[0]
		}
	}
	window := delivery.DeliveryWindowView{From: liveFrom, To: to}
	freshness := delivery.DeliveryFreshnessView{LastEventAt: &to, LastReconcileAt: &to}
	return deliveryTimelineWithMeasures{
		DeliveryTimelineResponse: delivery.DeliveryTimelineResponse{
			Window: window, PRs: prs, Waiting: waiting, Runs: runs,
			FacetCounts: deliveryFacetCounts(rows, deliverySelection{}), ColorCounts: deliveryColorCounts(rows),
			Components: components, IssueTitles: issueTitles, Freshness: freshness,
			SessionGroups: []delivery.SessionGroup{}, SessionTitles: sessionTitles, SessionPRCounts: sessionPRCounts,
		},
		Measures: liveMeasuresOf(window, to, len(runs)),
	}
}

// liveMeasuresOf is a measures body for window computed at computedAt, counting deploys.
func liveMeasuresOf(window delivery.DeliveryWindowView, computedAt time.Time, deploys int) deliveryMeasuresResponse {
	return deliveryMeasuresResponse{
		Window: window, ComputedAt: computedAt,
		Measures:  measures.Result{DeployFrequency: measures.DeployFrequency{SuccessfulDeploys: deploys}, Daily: []measures.DailyPoint{}},
		UnownedP0: []delivery.UnownedIssue{}, Targets: catalogueTargets(),
		Status:    measures.Status{UnownedP0: new(true)},
		Freshness: delivery.DeliveryFreshnessView{LastEventAt: &computedAt},
	}
}

// livePipelineOf is a pipeline body computed at computedAt whose production deploys timed n
// results.
func livePipelineOf(computedAt time.Time, n int) deliveryPipelineResponse {
	empty := measures.Timing{Days: []measures.Day{}}
	return deliveryPipelineResponse{
		Days:       []string{"2026-09-25", "2026-09-26", "2026-09-27", "2026-09-28", "2026-09-29", "2026-09-30", "2026-10-01"},
		ComputedAt: computedAt,
		PRChecks:   deliveryPipelinePRChecks{Run: empty, SlowestJobs: []measures.JobTiming{}, FailingJobs: []measures.FailingJob{}},
		ProductionApply: measures.DeployJobTiming{Job: "deploy / production", Timing: measures.Timing{
			Days: []measures.Day{}, Total: measures.Cell{N: n, Success: n},
		}},
		MergeToProduction: measures.LeadTimeTiming{Days: []measures.LeadTimeDay{}},
		OpenedToMerge:     measures.LeadTimeTiming{Days: []measures.LeadTimeDay{}},
		Targets:           catalogueTargets(),
		Freshness:         delivery.DeliveryFreshnessView{LastEventAt: &computedAt},
	}
}

func mustEncodeLive(t *testing.T, kind string, body any) deliveryLiveEncoded {
	t.Helper()
	encoded, err := encodeDeliveryBody(kind, body)
	if err != nil {
		t.Fatalf("encode %s body: %v", kind, err)
	}
	return encoded
}

// liveDiff is what a subscriber holding held is sent so that it holds next, after checking the delta
// lands: applied to held's body and revisions, it gives exactly next's (lists compared by id, the
// fields out of change detection and the subfields a whole field's revision leaves out aside).
func liveDiff(t *testing.T, held, next deliveryLiveEncoded) (DeliveryLiveDelta, bool) {
	t.Helper()
	delta, ok := diffDeliveryBody(next, held.revs)
	if !ok {
		// A field sent only with changes (the window) never makes a delta by itself, so the bodies
		// agree but for it.
		got, want := liveComparable(t, held), liveComparable(t, next)
		for _, field := range deliveryLiveKinds[next.kind].withChanges {
			delete(got, field)
			delete(want, field)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("no delta, but the bodies differ:\nheld: %v\nnext: %v", got, want)
		}
		return delta, false
	}
	applied := liveApply(t, held, delta)
	if got, want := liveComparable(t, applied), liveComparable(t, next); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta does not land on the next body:\napplied: %v\nnext:    %v", got, want)
	}
	if got, want := liveFlatRevs(liveApplyRevs(held.revs, delta.Revs)), liveFlatRevs(next.revs); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta's revisions do not land on the next body's:\napplied: %v\nnext:    %v", got, want)
	}
	return delta, true
}

// liveDecoded is an encoded body's top-level fields decoded generically.
func liveDecoded(t *testing.T, encoded deliveryLiveEncoded) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(encoded.body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// liveApplied is a body a delta was applied to, standing in for an encoded one.
type liveApplied struct {
	kind string
	body map[string]any
}

// liveApply is held's body with delta applied, as a page applies it.
func liveApply(t *testing.T, held deliveryLiveEncoded, delta DeliveryLiveDelta) liveApplied {
	t.Helper()
	body := liveDecoded(t, held)
	var wire struct {
		Set   map[string]any `json:"set"`
		Items map[string]struct {
			Upsert []any
			Remove []string
		} `json:"items"`
		Maps   map[string]map[string]any            `json:"maps"`
		Counts map[string]map[string]map[string]any `json:"counts"`
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("encode delta: %v", err)
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	for list, changes := range wire.Items {
		byID := liveByID(t, body[list])
		for _, id := range changes.Remove {
			delete(byID, id)
		}
		for _, item := range changes.Upsert {
			byID[liveID(t, item)] = item
		}
		items := []any{}
		for _, id := range slices.Sorted(maps.Keys(byID)) {
			items = append(items, byID[id])
		}
		body[list] = items
	}
	for name, entries := range wire.Maps {
		held, _ := body[name].(map[string]any)
		for key, value := range entries {
			if value == nil {
				delete(held, key)
			} else {
				held[key] = value
			}
		}
	}
	for name, facets := range wire.Counts {
		held, _ := body[name].(map[string]any)
		for facet, counts := range facets {
			values, _ := held[facet].(map[string]any)
			if values == nil {
				values = map[string]any{}
				held[facet] = values
			}
			for key, count := range counts {
				if count == nil {
					delete(values, key)
				} else {
					values[key] = count
				}
			}
		}
	}
	for name, value := range wire.Set {
		body[name] = value
	}
	return liveApplied{kind: held.kind, body: body}
}

// liveComparable is a body as two bodies a page shows alike compare: lists keyed by id, and neither
// the fields out of change detection nor the subfields a whole field's or a map entry's revision
// leaves out.
func liveComparable(t *testing.T, of any) map[string]any {
	t.Helper()
	var kind string
	var body map[string]any
	switch of := of.(type) {
	case deliveryLiveEncoded:
		kind, body = of.kind, liveDecoded(t, of)
	case liveApplied:
		kind, body = of.kind, of.body
	}
	classes := deliveryLiveKinds[kind]
	for _, name := range classes.out {
		delete(body, name)
	}
	for name, excluded := range classes.whole {
		if object, ok := body[name].(map[string]any); ok {
			for _, left := range excluded {
				delete(object, left)
			}
		}
	}
	for name, excluded := range classes.maps {
		entries, _ := body[name].(map[string]any)
		for _, entry := range entries {
			if object, ok := entry.(map[string]any); ok {
				for _, left := range excluded {
					delete(object, left)
				}
			}
		}
	}
	for _, list := range classes.lists {
		body[list] = liveByID(t, body[list])
	}
	return body
}

func liveByID(t *testing.T, list any) map[string]any {
	t.Helper()
	byID := map[string]any{}
	items, _ := list.([]any)
	for _, item := range items {
		byID[liveID(t, item)] = item
	}
	return byID
}

func liveID(t *testing.T, item any) string {
	t.Helper()
	switch id := item.(map[string]any)["id"].(type) {
	case string:
		return id
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64)
	default:
		t.Fatalf("item without an id: %v", item)
		return ""
	}
}

// liveApplyRevs is held with a delta's revision changes applied, as a page applies them.
func liveApplyRevs(held DeliveryLiveRevs, changes DeliveryLiveRevChanges) DeliveryLiveRevs {
	flat := liveFlatRevs(held)
	for list, revs := range changes.Items {
		for id, rev := range revs {
			liveSetRev(flat, rev, "items", list, id)
		}
	}
	for name, revs := range changes.Maps {
		for key, rev := range revs {
			liveSetRev(flat, rev, "maps", name, key)
		}
	}
	for name, facets := range changes.Counts {
		for facet, revs := range facets {
			for key, rev := range revs {
				liveSetRev(flat, rev, "counts", name, facet, key)
			}
		}
	}
	for name, rev := range changes.Fields {
		flat[liveRevPath("fields", name)] = rev
	}
	return liveUnflatRevs(flat)
}

func liveRevPath(parts ...string) string { return strings.Join(parts, "\x00") }

func liveSetRev(flat map[string]string, rev *string, parts ...string) {
	if rev == nil {
		delete(flat, liveRevPath(parts...))
	} else {
		flat[liveRevPath(parts...)] = *rev
	}
}

// liveFlatRevs is every revision revs holds, by its path, so an empty section and a missing one
// compare alike.
func liveFlatRevs(revs DeliveryLiveRevs) map[string]string {
	flat := map[string]string{}
	for list, items := range revs.Items {
		for id, rev := range items {
			flat[liveRevPath("items", list, id)] = rev
		}
	}
	for name, entries := range revs.Maps {
		for key, rev := range entries {
			flat[liveRevPath("maps", name, key)] = rev
		}
	}
	for name, facets := range revs.Counts {
		for facet, values := range facets {
			for key, rev := range values {
				flat[liveRevPath("counts", name, facet, key)] = rev
			}
		}
	}
	for name, rev := range revs.Fields {
		flat[liveRevPath("fields", name)] = rev
	}
	return flat
}

func liveUnflatRevs(flat map[string]string) DeliveryLiveRevs {
	revs := DeliveryLiveRevs{Items: map[string]map[string]string{}, Maps: map[string]map[string]string{}, Counts: map[string]map[string]map[string]string{}, Fields: map[string]string{}}
	for path, rev := range flat {
		parts := strings.Split(path, "\x00")
		switch parts[0] {
		case "items":
			if revs.Items[parts[1]] == nil {
				revs.Items[parts[1]] = map[string]string{}
			}
			revs.Items[parts[1]][parts[2]] = rev
		case "maps":
			if revs.Maps[parts[1]] == nil {
				revs.Maps[parts[1]] = map[string]string{}
			}
			revs.Maps[parts[1]][parts[2]] = rev
		case "counts":
			if revs.Counts[parts[1]] == nil {
				revs.Counts[parts[1]] = map[string]map[string]string{}
			}
			if revs.Counts[parts[1]][parts[2]] == nil {
				revs.Counts[parts[1]][parts[2]] = map[string]string{}
			}
			revs.Counts[parts[1]][parts[2]][parts[3]] = rev
		case "fields":
			revs.Fields[parts[1]] = rev
		}
	}
	return revs
}

// liveCounts is a delta's count changes as plain values: -1 for a count that left.
func liveCounts(delta DeliveryLiveDelta, name string) map[string]map[string]int {
	out := map[string]map[string]int{}
	for facet, counts := range delta.Counts[name] {
		out[facet] = map[string]int{}
		for key, count := range counts {
			if count == nil {
				out[facet][key] = -1
			} else {
				out[facet][key] = *count
			}
		}
	}
	return out
}

// liveUpsertIDs is the ids of a delta's upserts to list, in order.
func liveUpsertIDs(t *testing.T, delta DeliveryLiveDelta, list string) []string {
	t.Helper()
	ids := []string{}
	for _, raw := range delta.Items[list].Upsert {
		id, err := deliveryLiveItemID(raw)
		if err != nil {
			t.Fatalf("%s upsert: %v", list, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// liveMapValues is a delta's entries of one map, decoded: nil for a key that left.
func liveMapValues(t *testing.T, delta DeliveryLiveDelta, name string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for key, raw := range delta.Maps[name] {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s[%s]: %v", name, key, err)
		}
		out[key] = value
	}
	return out
}

func liveSections(delta DeliveryLiveDelta) (set, items, maps_, counts []string) {
	return slices.Sorted(maps.Keys(delta.Set)), slices.Sorted(maps.Keys(delta.Items)),
		slices.Sorted(maps.Keys(delta.Maps)), slices.Sorted(maps.Keys(delta.Counts))
}

func TestAnUnchangedBodyDiffsToNothing(t *testing.T) {
	t.Parallel()
	to := liveFrom.Add(48 * time.Hour)
	body := liveTimelineOf(to, []delivery.DeliveryPRView{livePR(1), livePR(2)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{liveRun(7, 3)})
	held := mustEncodeLive(t, deliveryLiveTimeline, body)
	again := mustEncodeLive(t, deliveryLiveTimeline, body)
	if _, ok := liveDiff(t, held, again); ok {
		t.Fatal("an unchanged timeline sent a delta")
	}
	if _, ok := diffDeliveryBody(held, held.revs); ok {
		t.Fatal("a body diffed against its own revisions sent a delta")
	}
	measuresBody := mustEncodeLive(t, deliveryLiveMeasures, body.Measures)
	if _, ok := liveDiff(t, measuresBody, mustEncodeLive(t, deliveryLiveMeasures, body.Measures)); ok {
		t.Fatal("unchanged measures sent a delta")
	}
	pipeline := mustEncodeLive(t, deliveryLivePipeline, livePipelineOf(to, 3))
	if _, ok := liveDiff(t, pipeline, mustEncodeLive(t, deliveryLivePipeline, livePipelineOf(to, 3))); ok {
		t.Fatal("an unchanged pipeline sent a delta")
	}
}

func TestOneMergeSendsOnlyTheCountsItMoved(t *testing.T) {
	t.Parallel()
	to := liveFrom.Add(48 * time.Hour)
	// A pull request of another repository, by another author in another session: the merge moves
	// none of its counts.
	elsewhere := livePR(9)
	elsewhere.Repo, elsewhere.ID, elsewhere.Author = "acme/other", "acme/other#9", "hubot"
	elsewhere.Sessions, elsewhere.ParentAgent, elsewhere.DeployedStatus = []string{"s-2"}, new("s-2"), delivery.DeployedStatusNotTracked
	before := []delivery.DeliveryPRView{livePR(1), livePR(2), livePR(3), elsewhere}
	merged := withIssue(livePR(4), "ACME-2", "Second issue")
	after := append(slices.Clone(before), merged)

	held := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, before, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	next := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, after, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a merge sent no delta")
	}

	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#4"}) {
		t.Errorf("items.prs.upsert = %v, want the merged pull request alone", got)
	}
	if got := delta.Items["prs"].Remove; len(got) != 0 {
		t.Errorf("items.prs.remove = %v, want none", got)
	}
	// The counts the merge moved, and none it left alone: acme/other, hubot, s-2, ACME-1 and
	// not_tracked keep theirs.
	wantFacets := map[string]map[string]int{
		"repo": {liveRepo: 4}, "parent_agent": {"s-1": 4}, "session": {"s-1": 4}, "issue": {"ACME-2": 1},
		"priority": {"P1": 5}, "component": {"ACME/api": 5}, "author": {"octocat": 4}, "rework": {"value": 5},
		"deployed": {"waiting": 4},
	}
	if got := liveCounts(delta, "facet_counts"); !reflect.DeepEqual(got, wantFacets) {
		t.Errorf("counts.facet_counts = %v, want %v", got, wantFacets)
	}
	wantColors := map[string]map[string]int{
		"repo": {liveRepo: 4}, "author": {"octocat": 4}, "priority": {"P1": 5}, "component": {"ACME/api": 5},
		"parent_agent": {"s-1": 4},
	}
	if got := liveCounts(delta, "color_counts"); !reflect.DeepEqual(got, wantColors) {
		t.Errorf("counts.color_counts = %v, want %v", got, wantColors)
	}
	if got := liveMapValues(t, delta, "session_pr_counts"); !reflect.DeepEqual(got, map[string]any{"s-1": float64(4)}) {
		t.Errorf("maps.session_pr_counts = %v, want the one session", got)
	}
	if got := liveMapValues(t, delta, "issue_titles"); !reflect.DeepEqual(got, map[string]any{"ACME-2": "Second issue"}) {
		t.Errorf("maps.issue_titles = %v, want only the newly named issue", got)
	}
	set, items, mapNames, counts := liveSections(delta)
	if len(set) != 0 || !slices.Equal(items, []string{"prs"}) ||
		!slices.Equal(mapNames, []string{"issue_titles", "session_pr_counts"}) ||
		!slices.Equal(counts, []string{"color_counts", "facet_counts"}) {
		t.Errorf("delta sections: set %v, items %v, maps %v, counts %v; want nothing beyond the merge's", set, items, mapNames, counts)
	}
}

func TestAKeyThatLeftIsNull(t *testing.T) {
	t.Parallel()
	to := liveFrom.Add(48 * time.Hour)
	second := withIssue(livePR(2), "ACME-2", "Second issue")
	second.Author = "hubot"
	held := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, []delivery.DeliveryPRView{livePR(1), second}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	// #2 re-linked to ACME-1: no pull request names ACME-2 any more.
	relinked := withIssue(second, "ACME-1", "First issue")
	next := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, []delivery.DeliveryPRView{livePR(1), relinked}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a re-linked pull request sent no delta")
	}
	if got := liveCounts(delta, "facet_counts")["issue"]; !reflect.DeepEqual(got, map[string]int{"ACME-1": 2, "ACME-2": -1}) {
		t.Errorf("counts.facet_counts.issue = %v, want ACME-1 2 and ACME-2 null", got)
	}
	if raw := delta.Counts["facet_counts"]["issue"]["ACME-2"]; raw != nil {
		t.Errorf("the count that left is %d, want null", *raw)
	}
	if rev, ok := delta.Revs.Counts["facet_counts"]["issue"]["ACME-2"]; !ok || rev != nil {
		t.Errorf("revs.counts.facet_counts.issue.ACME-2 = %v (present %v), want null", rev, ok)
	}
	if got := liveMapValues(t, delta, "issue_titles"); !reflect.DeepEqual(got, map[string]any{"ACME-2": nil}) {
		t.Errorf("maps.issue_titles = %v, want ACME-2 null", got)
	}
	if rev, ok := delta.Revs.Maps["issue_titles"]["ACME-2"]; !ok || rev != nil {
		t.Errorf("revs.maps.issue_titles.ACME-2 = %v (present %v), want null", rev, ok)
	}
	encoded, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ACME-2":null`} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Errorf("the delta's JSON lacks %s: %s", want, encoded)
		}
	}
}

func TestAPullRequestThatLeftTheWindowIsRemoved(t *testing.T) {
	t.Parallel()
	heldTo := liveFrom.Add(48 * time.Hour)
	held := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(heldTo, []delivery.DeliveryPRView{livePR(1), livePR(2), livePR(3)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	// A preset's window moved past #1's merge: the window and its pull requests both moved.
	nextBody := liveTimelineOf(heldTo.Add(90*time.Minute), []delivery.DeliveryPRView{livePR(2), livePR(3)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{})
	nextBody.Window.From = liveFrom.Add(90 * time.Minute)
	next := mustEncodeLive(t, deliveryLiveTimeline, nextBody)
	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a pull request leaving the window sent no delta")
	}
	if got := delta.Items["prs"]; !slices.Equal(got.Remove, []string{"acme/widgets#1"}) || len(got.Upsert) != 0 {
		t.Errorf("items.prs = %+v, want #1 removed and nothing upserted", got)
	}
	if rev, ok := delta.Revs.Items["prs"]["acme/widgets#1"]; !ok || rev != nil {
		t.Errorf("revs.items.prs[#1] = %v (present %v), want null", rev, ok)
	}
	if got := liveCounts(delta, "facet_counts")["repo"]; !reflect.DeepEqual(got, map[string]int{liveRepo: 2}) {
		t.Errorf("counts.facet_counts.repo = %v, want 2", got)
	}
	var window delivery.DeliveryWindowView
	if err := json.Unmarshal(delta.Set["window"], &window); err != nil || !window.From.Equal(nextBody.Window.From) {
		t.Errorf("set.window = %s (%v), want the moved window", delta.Set["window"], err)
	}
}

func TestADeployFlipsEveryWaitingPullRequestItShipped(t *testing.T) {
	t.Parallel()
	to := liveFrom.Add(72 * time.Hour)
	prs := make([]delivery.DeliveryPRView, 20)
	for i := range prs {
		prs[i] = livePR(i + 1)
	}
	waitingMerged := liveFrom.Add(-2 * time.Hour)
	waiting := []delivery.DeliveryWaitingPRView{{MergedAt: waitingMerged}, {MergedAt: waitingMerged.Add(time.Hour)}}
	old := liveRun(99, 0)
	held := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, prs, waiting, []delivery.DeliveryRunView{old}))

	deployedAt := liveFrom.Add(30 * time.Hour)
	shipped := make([]delivery.DeliveryPRView, len(prs))
	for i, pr := range prs {
		shipped[i] = shippedBy(pr, 100, deployedAt)
	}
	flipped := []delivery.DeliveryWaitingPRView{{MergedAt: waitingMerged, DeployedAt: &deployedAt}, {MergedAt: waitingMerged.Add(time.Hour), DeployedAt: &deployedAt}}
	next := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, shipped, flipped, []delivery.DeliveryRunView{old, liveRun(100, 29, shipped...)}))

	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a deploy sent no delta")
	}
	if got := liveUpsertIDs(t, delta, "prs"); len(got) != 20 {
		t.Errorf("items.prs.upsert holds %d pull requests, want the 20 it shipped", len(got))
	}
	if got := liveUpsertIDs(t, delta, "runs"); !slices.Equal(got, []string{"100"}) {
		t.Errorf("items.runs.upsert = %v, want the deploy alone", got)
	}
	set, items, mapNames, counts := liveSections(delta)
	if !slices.Equal(set, []string{"measures", "waiting"}) {
		t.Errorf("set = %v, want waiting whole and the measures the deploy moved", set)
	}
	if !slices.Equal(items, []string{"prs", "runs"}) || len(mapNames) != 0 || !slices.Equal(counts, []string{"facet_counts"}) {
		t.Errorf("items %v, maps %v, counts %v; want prs and runs, no map, the facet counts", items, mapNames, counts)
	}
	if got := liveCounts(delta, "facet_counts"); !reflect.DeepEqual(got, map[string]map[string]int{"deployed": {"deployed": 20, "waiting": -1}}) {
		t.Errorf("counts.facet_counts = %v, want deployed 20 and waiting null", got)
	}
}

func TestAWindowThatMovedAloneSendsNothing(t *testing.T) {
	t.Parallel()
	prs := []delivery.DeliveryPRView{livePR(1), livePR(2)}
	heldTo := liveFrom.Add(48 * time.Hour)
	held := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(heldTo, prs, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	nextTo := heldTo.Add(time.Minute)
	moved := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(nextTo, prs, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	if delta, ok := diffDeliveryBody(moved, held.revs); ok {
		t.Fatalf("a window that moved alone sent %+v", delta)
	}
	window := delivery.DeliveryWindowView{From: liveFrom, To: heldTo}
	heldMeasures := mustEncodeLive(t, deliveryLiveMeasures, liveMeasuresOf(window, heldTo, 1))
	window.To = nextTo
	if delta, ok := diffDeliveryBody(mustEncodeLive(t, deliveryLiveMeasures, liveMeasuresOf(window, nextTo, 1)), heldMeasures.revs); ok {
		t.Fatalf("a measures window that moved alone sent %+v", delta)
	}

	// With a change beside it, the window is included.
	retitled := slices.Clone(prs)
	retitled[1].Title = "feat: renamed"
	next := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(nextTo, retitled, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))
	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a retitled pull request sent no delta")
	}
	if set, _, _, _ := liveSections(delta); !slices.Equal(set, []string{"window"}) {
		t.Errorf("set = %v, want the window included with the change", set)
	}
	var got delivery.DeliveryWindowView
	if err := json.Unmarshal(delta.Set["window"], &got); err != nil || !got.To.Equal(nextTo) {
		t.Errorf("set.window = %s (%v), want to %s", delta.Set["window"], err, nextTo)
	}
	// A subscriber already holding the moved window is not sent it again.
	afterMove, ok := diffDeliveryBody(next, moved.revs)
	if !ok {
		t.Fatal("a retitled pull request sent no delta to a page holding the moved window")
	}
	if _, included := afterMove.Set["window"]; included {
		t.Error("set.window was sent to a page that already holds it")
	}
}

func TestComputedAtAndFreshnessAloneAreNoChange(t *testing.T) {
	t.Parallel()
	heldAt := liveFrom.Add(48 * time.Hour)
	nextAt := heldAt.Add(5 * time.Minute)
	window := delivery.DeliveryWindowView{From: liveFrom, To: heldAt}
	failure := "rate limited"

	timeline := liveTimelineOf(heldAt, []delivery.DeliveryPRView{livePR(1)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{})
	held := mustEncodeLive(t, deliveryLiveTimeline, timeline)
	timeline.Freshness = delivery.DeliveryFreshnessView{LastEventAt: &nextAt, LastError: &failure, UnfetchableCount: 3}
	timeline.Measures.ComputedAt = nextAt
	timeline.Measures.Freshness = timeline.Freshness
	if delta, ok := diffDeliveryBody(mustEncodeLive(t, deliveryLiveTimeline, timeline), held.revs); ok {
		t.Errorf("timeline: freshness and computed_at alone sent %+v", delta)
	}

	measuresBody := liveMeasuresOf(window, heldAt, 1)
	heldMeasures := mustEncodeLive(t, deliveryLiveMeasures, measuresBody)
	measuresBody.ComputedAt, measuresBody.Freshness = nextAt, delivery.DeliveryFreshnessView{LastError: &failure}
	if delta, ok := diffDeliveryBody(mustEncodeLive(t, deliveryLiveMeasures, measuresBody), heldMeasures.revs); ok {
		t.Errorf("measures: freshness and computed_at alone sent %+v", delta)
	}

	pipeline := livePipelineOf(heldAt, 3)
	heldPipeline := mustEncodeLive(t, deliveryLivePipeline, pipeline)
	pipeline.ComputedAt, pipeline.Freshness = nextAt, delivery.DeliveryFreshnessView{LastError: &failure}
	if delta, ok := diffDeliveryBody(mustEncodeLive(t, deliveryLivePipeline, pipeline), heldPipeline.revs); ok {
		t.Errorf("pipeline: freshness and computed_at alone sent %+v", delta)
	}
}

func TestAChangedMeasureSendsTheMeasuresWhole(t *testing.T) {
	t.Parallel()
	at := liveFrom.Add(48 * time.Hour)
	window := delivery.DeliveryWindowView{From: liveFrom, To: at}
	held := mustEncodeLive(t, deliveryLiveMeasures, liveMeasuresOf(window, at, 1))
	nextBody := liveMeasuresOf(window, at.Add(time.Minute), 2)
	next := mustEncodeLive(t, deliveryLiveMeasures, nextBody)
	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a changed measure sent no delta")
	}
	if set, items, mapNames, counts := liveSections(delta); !slices.Equal(set, []string{"measures"}) || len(items)+len(mapNames)+len(counts) != 0 {
		t.Errorf("set %v, items %v, maps %v, counts %v; want the measures alone", set, items, mapNames, counts)
	}
	var sent measures.Result
	if err := json.Unmarshal(delta.Set["measures"], &sent); err != nil || !reflect.DeepEqual(sent, nextBody.Measures) {
		t.Errorf("set.measures = %s (%v), want the new measures whole", delta.Set["measures"], err)
	}

	// An unowned P0 moves unowned_p0 and its status, not the measures.
	nextBody = liveMeasuresOf(window, at, 1)
	nextBody.UnownedP0 = []delivery.UnownedIssue{{Key: "ACME-7", Title: "Outage"}}
	nextBody.Status.UnownedP0 = new(false)
	if delta, ok := liveDiff(t, held, mustEncodeLive(t, deliveryLiveMeasures, nextBody)); !ok {
		t.Error("an unowned P0 sent no delta")
	} else if set, _, _, _ := liveSections(delta); !slices.Equal(set, []string{"status", "unowned_p0"}) {
		t.Errorf("set = %v, want status and unowned_p0", set)
	}

	// On the timeline, the measures are sent whole, their computed_at included.
	prs := []delivery.DeliveryPRView{livePR(1)}
	timeline := liveTimelineOf(at, prs, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{})
	heldTimeline := mustEncodeLive(t, deliveryLiveTimeline, timeline)
	timeline.Measures = liveMeasuresOf(timeline.Window, at.Add(time.Minute), 3)
	delta, ok = liveDiff(t, heldTimeline, mustEncodeLive(t, deliveryLiveTimeline, timeline))
	if !ok {
		t.Fatal("the timeline's changed measures sent no delta")
	}
	var sentWhole deliveryMeasuresResponse
	if err := json.Unmarshal(delta.Set["measures"], &sentWhole); err != nil || !reflect.DeepEqual(sentWhole.Measures, timeline.Measures.Measures) || !sentWhole.ComputedAt.Equal(timeline.Measures.ComputedAt) {
		t.Errorf("set.measures = %s (%v), want the timeline's new measures whole", delta.Set["measures"], err)
	}
}

func TestAPipelineCardThatMovedIsTheOnlyField(t *testing.T) {
	t.Parallel()
	at := liveFrom.Add(12 * time.Hour)
	held := mustEncodeLive(t, deliveryLivePipeline, livePipelineOf(at, 3))
	next := mustEncodeLive(t, deliveryLivePipeline, livePipelineOf(at.Add(time.Minute), 4))
	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("a production deploy's timing moved and sent no delta")
	}
	if set, items, mapNames, counts := liveSections(delta); !slices.Equal(set, []string{"production_apply"}) || len(items)+len(mapNames)+len(counts) != 0 {
		t.Errorf("set %v, items %v, maps %v, counts %v; want production_apply alone", set, items, mapNames, counts)
	}
	if got := slices.Sorted(maps.Keys(delta.Revs.Fields)); !slices.Equal(got, []string{"production_apply"}) {
		t.Errorf("revs.fields = %v, want production_apply alone", got)
	}
}

func TestDiffAgainstAPagesRevsAfterReconnect(t *testing.T) {
	t.Parallel()
	to := liveFrom.Add(48 * time.Hour)
	page := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(to, []delivery.DeliveryPRView{livePR(1), livePR(2), livePR(3)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{}))

	// While the page was away: #1 left the window, #2 was retitled, #5 merged, and a group formed.
	retitled := livePR(2)
	retitled.Title = "feat: renamed"
	nowBody := liveTimelineOf(to, []delivery.DeliveryPRView{retitled, livePR(3), livePR(5)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{})
	nowBody.SessionGroups = []delivery.SessionGroup{{
		ID: "8b1f0e64-58c4-4a3b-9b1e-6f4b2a8d1c11", Name: "Planners", Sessions: []string{"s-1"},
		CreatedBy: model.Actor{Kind: "user", ID: "alice"}, CreatedAt: to, UpdatedBy: model.Actor{Kind: "user", ID: "alice"}, UpdatedAt: to,
	}}
	now := mustEncodeLive(t, deliveryLiveTimeline, nowBody)

	delta, ok := liveDiff(t, page, now)
	if !ok {
		t.Fatal("a reconnecting page was sent nothing")
	}
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2", "acme/widgets#5"}) {
		t.Errorf("items.prs.upsert = %v, want #2 and #5 in the body's order", got)
	}
	if got := delta.Items["prs"].Remove; !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Errorf("items.prs.remove = %v, want #1", got)
	}
	if got := liveUpsertIDs(t, delta, "session_groups"); !slices.Equal(got, []string{nowBody.SessionGroups[0].ID}) {
		t.Errorf("items.session_groups.upsert = %v, want the new group", got)
	}
	if _, sent := delta.Items["runs"]; sent {
		t.Error("items.runs was sent, though no run changed")
	}
	// Once the page holds what it was sent, nothing more.
	if extra, ok := diffDeliveryBody(now, liveApplyRevs(page.revs, delta.Revs)); ok {
		t.Errorf("after the resume's delta the page is sent %+v more", extra)
	}
}

func TestAListWithARepeatedIDIsRefused(t *testing.T) {
	t.Parallel()
	body := liveTimelineOf(liveFrom.Add(time.Hour), []delivery.DeliveryPRView{livePR(1), livePR(1)}, []delivery.DeliveryWaitingPRView{}, []delivery.DeliveryRunView{})
	if _, err := encodeDeliveryBody(deliveryLiveTimeline, body); err == nil || !strings.Contains(err.Error(), `"acme/widgets#1" twice`) {
		t.Fatalf("encode = %v, want a refusal naming the repeated id", err)
	}
}

// The evals fixture world: project ACME's logged evals over the 28 days from liveFrom, compared
// with the 28 days before them.
var (
	liveEvalsWindow  = evals.Range{From: liveFrom, To: liveFrom.AddDate(0, 0, 28)}
	liveEvalsCompare = evals.Range{From: liveFrom.AddDate(0, 0, -28), To: liveFrom}
)

// liveEval is one logged eval of ACME as the list read states it: version version of its
// definition, and its one series, whose mean over the window is mean.
func liveEval(slug string, version int, mean float64) evalListItem {
	id := evals.EvalID("ACME", slug)
	definition := evals.Definition{
		EvalID: id, Version: version, ProjectKey: "ACME", Slug: slug, Name: slug,
		FailureModes: []evals.FailureMode{}, Unit: "count", TargetJudges: []string{}, Person: "alice",
		Kind: evals.KindExperiment, Source: evals.SourceLogged, State: evals.StateActive, Links: []evals.Link{},
		WrittenVia: evals.ViaReading, WrittenBy: model.Actor{Kind: "user", ID: "alice"}, CreatedAt: liveFrom,
	}
	series := evals.SeriesValues{
		Series: evals.Series{
			EvalID: id, Series: "value", Version: 1, Unit: "count", Role: evals.RoleHeadline, IsPrimary: true,
			Listed: true, Agg: evals.AggSum, Links: []evals.Link{}, CreatedAt: liveFrom,
		},
		Window: evals.Statistics{"mean": &mean}, Compare: evals.Statistics{}, Change: evals.Statistics{},
		DataSince: new(liveFrom.Format(time.DateOnly)), Days: []evals.DayPoint{}, CompareDays: []evals.DayPoint{},
	}
	return evalListItem{ID: id, evalItem: evalItem{
		Definition: definition, Owner: evalOwnerOf(definition), Series: []evals.SeriesValues{series},
	}}
}

// liveEvalsOf is ACME's list read of items, the rows it folded last updated at updated, the
// refresher's last pass age seconds before the read.
func liveEvalsOf(updated time.Time, age int, items ...evalListItem) evalListResponse {
	return evalListResponse{
		Items: items, Total: len(items), Limit: evalPageDefault,
		Window: newEvalRange(liveEvalsWindow), Compare: evalCompareRange(&liveEvalsCompare),
		Freshness: evalFreshness{RowsUpdatedAt: &updated, RefresherAgeSeconds: &age},
	}
}

// liveFailingJob is one of pr-ci's jobs that failed in the 24 hours before the end of the evals
// fixture's window, as the items read answers it with sort=failures: a job of liveRepo's workflow,
// keyed by both (failingJobKey), failures of its results failed, the last of them in run, hours
// before that end.
func liveFailingJob(name string, failures, results int, run int64, hours int) evalItemRow {
	at := liveEvalsWindow.To.Add(-time.Duration(hours) * time.Hour)
	runURL := "https://github.com/" + liveRepo + "/actions/runs/" + strconv.FormatInt(run, 10)
	return evalItemRow{
		RowKey: failingJobKey(liveRepo, name), RowKind: evalRowJob,
		Cols: map[string]any{
			"name": name, "repo": liveRepo, "failures": failures, "results": results, "last_failed_at": at, "last_run_url": runURL,
		},
		MeasuredAt: at, Source: "measures.FailingJobs", Href: runURL, Links: []evals.Link{}, Range: evals.RangeWindow,
	}
}

// liveItemsOf is the items read of ACME's pr-ci with sort=failures over the evals fixture's window
// and compare range, answering rows.
func liveItemsOf(rows ...evalItemRow) evalItemsResponse {
	return evalItemsResponse{
		Items: rows, Total: len(rows), Limit: evalPageMax,
		Window: newEvalRange(liveEvalsWindow), Compare: evalCompareRange(&liveEvalsCompare),
	}
}

// TestAnEvalWhoseNumbersMovedIsTheOneItemSent: an evals view sends its list eval by eval, by id. A
// reading that moves one eval's numbers sends that eval alone; a registered eval is upserted with
// the total that counts it; a new version of a definition sends its eval whole; a day that turned
// sends the window and the compare range with the numbers it moved. The refresher's age, which
// every read measures afresh, sends nothing by itself, and goes with the freshness when the
// folded rows' own age moves.
func TestAnEvalWhoseNumbersMovedIsTheOneItemSent(t *testing.T) {
	updated := liveEvalsWindow.To.Add(-time.Hour)
	three := func(prSize float64, reviewVersion int) []evalListItem {
		return []evalListItem{liveEval("deploy-frequency", 1, 3), liveEval("pr-size", 1, prSize), liveEval("review-time", reviewVersion, 30)}
	}
	held := mustEncodeLive(t, evalsView, liveEvalsOf(updated, 10, three(120, 1)...))

	// A reading moved pr-size's mean, and the refresher's last pass is a minute further back.
	delta, ok := liveDiff(t, held, mustEncodeLive(t, evalsView, liveEvalsOf(updated, 70, three(140, 1)...)))
	if !ok {
		t.Fatal("an eval whose mean moved sent no delta")
	}
	set, items, mapNames, counts := liveSections(delta)
	if got := liveUpsertIDs(t, delta, "items"); !slices.Equal(got, []string{"ACME/pr-size"}) || len(delta.Items["items"].Remove) != 0 {
		t.Errorf("items.items = upsert %v, remove %v; want ACME/pr-size alone upserted", got, delta.Items["items"].Remove)
	}
	if len(set) != 0 || !slices.Equal(items, []string{"items"}) || len(mapNames)+len(counts) != 0 {
		t.Errorf("set %v, items %v, maps %v, counts %v; want the one list alone", set, items, mapNames, counts)
	}

	// The refresher's age alone.
	if delta, ok := diffDeliveryBody(mustEncodeLive(t, evalsView, liveEvalsOf(updated, 130, three(120, 1)...)), held.revs); ok {
		t.Errorf("the refresher's age alone sent %+v", delta)
	}

	// lead-time registered, which sorts between deploy-frequency and pr-size.
	delta, ok = liveDiff(t, held, mustEncodeLive(t, evalsView, liveEvalsOf(updated, 5, liveEval("deploy-frequency", 1, 3),
		liveEval("lead-time", 1, 45), liveEval("pr-size", 1, 120), liveEval("review-time", 1, 30))))
	if !ok {
		t.Fatal("a registered eval sent no delta")
	}
	if got := liveUpsertIDs(t, delta, "items"); !slices.Equal(got, []string{"ACME/lead-time"}) {
		t.Errorf("items.items.upsert = %v, want the registered eval", got)
	}
	if set, _, _, _ := liveSections(delta); !slices.Equal(set, []string{"total"}) {
		t.Errorf("set = %v, want the total that counts it", set)
	}

	// The rows a pass rewrote are younger: the freshness goes whole, its refresher age with it.
	delta, ok = liveDiff(t, held, mustEncodeLive(t, evalsView, liveEvalsOf(updated.Add(2*time.Hour), 5, three(120, 1)...)))
	if !ok {
		t.Fatal("younger rows sent no delta")
	}
	var freshness evalFreshness
	if err := json.Unmarshal(delta.Set["freshness"], &freshness); err != nil || freshness.RefresherAgeSeconds == nil ||
		*freshness.RefresherAgeSeconds != 5 || !freshness.RowsUpdatedAt.Equal(updated.Add(2*time.Hour)) {
		t.Errorf("set.freshness = %s (%v), want the rows' new age with the refresher's", delta.Set["freshness"], err)
	}

	// A target set: review-time's third version.
	delta, ok = liveDiff(t, held, mustEncodeLive(t, evalsView, liveEvalsOf(updated, 10, three(120, 3)...)))
	if !ok {
		t.Fatal("a new version sent no delta")
	}
	if got := liveUpsertIDs(t, delta, "items"); !slices.Equal(got, []string{"ACME/review-time"}) {
		t.Errorf("items.items.upsert = %v, want the eval whose definition moved", got)
	}

	// The day turned: the window and the compare range are a day on, and pr-size's mean moved.
	turned := liveEvalsOf(updated, 10, three(150, 1)...)
	turned.Window = newEvalRange(evals.Range{From: liveEvalsWindow.From.AddDate(0, 0, 1), To: liveEvalsWindow.To.AddDate(0, 0, 1)})
	turned.Compare = evalCompareRange(&evals.Range{From: liveEvalsCompare.From.AddDate(0, 0, 1), To: liveEvalsCompare.To.AddDate(0, 0, 1)})
	delta, ok = liveDiff(t, held, mustEncodeLive(t, evalsView, turned))
	if !ok {
		t.Fatal("a turned day sent no delta")
	}
	if set, _, _, _ := liveSections(delta); !slices.Equal(set, []string{"compare", "window"}) {
		t.Errorf("set = %v, want the window and the compare range", set)
	}
}

// TestTheLiveFixturesAreWhatTheDiffWrites holds packages/contracts/fixtures/live, which the
// contracts' and the dashboard's Bun tests read, to the frames this package writes: a timeline
// snapshot, the delta of one deploy, a merge and a pull request leaving the window, the snapshot
// a page subscribing afterwards gets (applying the delta to the first gives its body, but for the
// freshness a delta never carries), a freshness verdict, an error, the event feed's start, one of
// its events and its end, an agent view's responder verdict, the one agent frame whose data
// Dispatch writes itself, an evals view's snapshot, the delta of a reading and a registration,
// and the snapshot after them, which applying that delta to the first gives whole, and an items
// view's snapshot of pr-ci's failing jobs, the delta of a job's second failure and another job's
// first, and the snapshot after them.
// `go test ./internal/dispatch/api -run TestTheLiveFixturesAreWhatTheDiffWrites -update`
// rewrites them.
func TestTheLiveFixturesAreWhatTheDiffWrites(t *testing.T) {
	heldTo := liveFrom.Add(48 * time.Hour)
	second := withIssue(livePR(2), "ACME-2", "Second issue")
	second.Author = "hubot"
	held := mustEncodeLive(t, deliveryLiveTimeline, liveTimelineOf(heldTo,
		[]delivery.DeliveryPRView{livePR(1), second, livePR(3)},
		[]delivery.DeliveryWaitingPRView{{MergedAt: liveFrom.Add(-3 * time.Hour)}},
		[]delivery.DeliveryRunView{liveRun(41, 0)}))

	// Ninety minutes on: #1 left the window, so did hubot and ACME-2 with #2, a deploy shipped #3
	// and the waiting entry, and #4 merged.
	nextTo := heldTo.Add(90 * time.Minute)
	deployedAt := liveFrom.Add(47 * time.Hour)
	shipped := shippedBy(livePR(3), 42, deployedAt)
	nextBody := liveTimelineOf(nextTo,
		[]delivery.DeliveryPRView{shipped, livePR(4)},
		[]delivery.DeliveryWaitingPRView{{MergedAt: liveFrom.Add(-3 * time.Hour), DeployedAt: &deployedAt}},
		[]delivery.DeliveryRunView{liveRun(41, 0), liveRun(42, 46, shipped)})
	nextBody.Window.From = liveFrom.Add(90 * time.Minute)
	nextBody.Measures.Window = nextBody.Window
	next := mustEncodeLive(t, deliveryLiveTimeline, nextBody)

	delta, ok := liveDiff(t, held, next)
	if !ok {
		t.Fatal("the fixture's change sent no delta")
	}
	snapshotAt := heldTo.Add(-time.Minute)
	delta.Sub, delta.Seq, delta.ChangedAt = "timeline-1", "42", &deployedAt

	// ACME's evals: a reading moved pr-size's mean, and lead-time was registered, which sorts
	// between deploy-frequency and pr-size; the pass that wrote their rows left the folded rows
	// younger, so the freshness goes whole.
	evalsAt := liveEvalsWindow.To.Add(-time.Hour)
	evalsHeld := mustEncodeLive(t, evalsView, liveEvalsOf(evalsAt.Add(-time.Hour), 40,
		liveEval("deploy-frequency", 1, 3), liveEval("pr-size", 1, 120), liveEval("review-time", 2, 30)))
	evalsNext := mustEncodeLive(t, evalsView, liveEvalsOf(evalsAt, 5,
		liveEval("deploy-frequency", 1, 3), liveEval("lead-time", 1, 45), liveEval("pr-size", 1, 140), liveEval("review-time", 2, 30)))
	evalsDelta, ok := liveDiff(t, evalsHeld, evalsNext)
	if !ok {
		t.Fatal("the evals fixture's change sent no delta")
	}
	evalsDelta.Sub, evalsDelta.Seq, evalsDelta.ChangedAt = "evals-1", "44", &evalsAt
	evalsHeldAt := evalsAt.Add(-time.Hour)

	// ACME's pr-ci failing jobs: build failed again in run 52, and lint failed for the first time
	// in it; the rows carry no id, so they go whole, with the total that counts them.
	itemsHeld := mustEncodeLive(t, evalItemsView, liveItemsOf(liveFailingJob("build / unit", 1, 3, 51, 5)))
	itemsNext := mustEncodeLive(t, evalItemsView, liveItemsOf(liveFailingJob("build / unit", 2, 4, 52, 1), liveFailingJob("lint", 1, 4, 52, 1)))
	itemsDelta, ok := liveDiff(t, itemsHeld, itemsNext)
	if !ok {
		t.Fatal("the items fixture's change sent no delta")
	}
	itemsAt := liveEvalsWindow.To.Add(-time.Hour)
	itemsHeldAt := itemsAt.Add(-4 * time.Hour)
	itemsDelta.Sub, itemsDelta.Seq, itemsDelta.ChangedAt = "items-1", "46", &itemsAt

	// ACME's project timeline, grown from its two newest rows to all four: the growth's delta
	// carries the two rows the page lacked, its new limit, and the waiting line the page reaches
	// now that it holds the window's oldest row.
	shipped42 := shippedBy(livePR(2), 42, liveFrom.Add(4*time.Hour))
	created42 := liveFrom.Add(3*time.Hour - 30*time.Second)
	timeline := liveTimelineOf(liveFrom.Add(48*time.Hour),
		[]delivery.DeliveryPRView{livePR(1), shipped42, livePR(5)},
		[]delivery.DeliveryWaitingPRView{{MergedAt: liveFrom.Add(-3 * time.Hour)}},
		[]delivery.DeliveryRunView{liveRun(42, 3, shipped42)}).DeliveryTimelineResponse
	timelineRuns := map[int64]delivery.DeliveryRun{42: {Repo: liveRepo, RunID: 42, StartedAt: liveFrom.Add(3 * time.Hour), CreatedAt: &created42}}
	timelineHeld := mustEncodeLive(t, deliveryLiveProjectTimeline, projectTimelinePage(timeline, timelineRuns, 2, 0))
	timelineNext := mustEncodeLive(t, deliveryLiveProjectTimeline, projectTimelinePage(timeline, timelineRuns, 4, 0))
	timelineDelta, ok := liveDiff(t, timelineHeld, timelineNext)
	if !ok {
		t.Fatal("the project timeline fixture's growth sent no delta")
	}
	timelineAt := liveFrom.Add(47 * time.Hour)
	timelineDelta.Sub, timelineDelta.Seq, timelineDelta.ChangedAt = "project-timeline-1", "46", &timelineAt
	event := liveFixtureEventFrame(t)
	frames := map[string]any{
		"snapshot.json":  held.snapshot("timeline-1", 41, &snapshotAt),
		"delta.json":     delta,
		"after.json":     next.snapshot("timeline-1", 42, &deployedAt),
		"heartbeat.json": liveHeartbeat,
		// The reconcile last ran two hours before the snapshot, past deliveryStaleAfter, so the
		// sources it reconciles are stale.
		"freshness.json": DeliveryLiveFreshness{
			Type:      "freshness",
			Freshness: delivery.DeliveryFreshnessView{LastEventAt: &snapshotAt, LastReconcileAt: new(snapshotAt.Add(-2 * time.Hour))},
			Stale:     deliveryReconciledSources,
		},
		"error.json": LiveError{
			Type: "error", Sub: "timeline-1", Code: deliveryNotConfiguredCode,
			Message: deliveryNotConfiguredMessage,
		},
		"events_start.json":   liveEventsStart("events-1", 41, true),
		"event.json":          event,
		"ended.json":          liveEnded("events-1", liveEndedBehind),
		"agent.json":          liveAgentFrame("agent-1", agentEventResponder, responderEvent(true)),
		"evals_snapshot.json": evalsHeld.snapshot("evals-1", 43, &evalsHeldAt),
		"evals_delta.json":    evalsDelta,
		"evals_after.json":    evalsNext.snapshot("evals-1", 44, &evalsAt),
		"items_snapshot.json": itemsHeld.snapshot("items-1", 45, &itemsHeldAt),
		"items_delta.json":    itemsDelta,
		"items_after.json":    itemsNext.snapshot("items-1", 46, &itemsAt),
		// The growth is the same sub subscribed again with the higher limit and a resume: its
		// answer is the delta, and the snapshot after it is what a new subscription is sent.
		"project_timeline_snapshot.json": timelineHeld.snapshot("project-timeline-1", 46, &timelineAt),
		"project_timeline_delta.json":    timelineDelta,
		"project_timeline_after.json":    timelineNext.snapshot("project-timeline-1", 46, &timelineAt),
	}
	for name, frame := range frames {
		got, err := json.MarshalIndent(frame, "", "  ")
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		got = append(got, '\n')
		path := filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "live", name)
		if *updateFixtures {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run with -update to write it): %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the frame the diff writes (run with -update):\n%s", path, got)
		}
	}
}

// TestTheClientLiveFixturesAreFramesTheSocketReads holds the page's frames in
// packages/contracts/fixtures/live, which the contracts' Bun tests hold to LiveSubscribe and
// LiveUnsubscribe key for key, to what the socket reads: each decodes into the struct the socket
// decodes it into, and each subscribe's resume decodes as its view's kind's. The decode here
// refuses a member the struct does not declare, where the socket ignores one
// (decodeSocketClientFrame, so a newer build's page is served): a key the contract gains that the
// server does not read fails here, rather than reaching every page as a member the server quietly
// drops.
func TestTheClientLiveFixturesAreFramesTheSocketReads(t *testing.T) {
	t.Parallel()
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "live", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return data
	}
	for name, kind := range map[string]string{
		"subscribe_timeline.json":         deliveryLiveTimeline,
		"subscribe_evals.json":            evalsView,
		"subscribe_items.json":            evalItemsView,
		"subscribe_project_timeline.json": deliveryLiveProjectTimeline,
		"subscribe_events.json":           liveEventsView,
		"subscribe_agent.json":            liveAgentView,
	} {
		frame := decodeSocketFrameAs[LiveSubscribe](t, read(name))
		if frame.Type != "subscribe" || frame.Sub == "" || frame.View.Kind != kind {
			t.Errorf("%s = %+v, want a subscribe to a %s view", name, frame, kind)
			continue
		}
		// Each fixture carries every member its view's kind takes: a delivery or events view its
		// resume, an agent view its session_id and no resume. A delivery view's is one the hub
		// holds (parseDeliveryLiveView), an evals view's names its project, and an items view's
		// its project and its eval's slug.
		var err error
		complete := false
		switch kind {
		case deliveryLiveTimeline, evalsView, evalItemsView, deliveryLiveProjectTimeline:
			var resume *DeliveryLiveResume
			resume, err = decodeLiveResume[DeliveryLiveResume](frame.Resume)
			complete = resume != nil && frame.View.Window != nil && frame.View.Query != nil &&
				(kind == deliveryLiveTimeline || frame.View.Project != nil) && (kind != evalItemsView || frame.View.Slug != nil)
			if _, refusal := parseDeliveryLiveView(frame.View.DeliveryLiveView); refusal != nil {
				t.Errorf("%s: the hub refuses its view: %+v", name, refusal)
			}
		case liveEventsView:
			var resume *EventsLiveResume
			resume, err = decodeLiveResume[EventsLiveResume](frame.Resume)
			complete = resume != nil
		case liveAgentView:
			complete = !hasLiveResume(frame.Resume) && frame.View.SessionID != nil
		}
		if err != nil || !complete {
			t.Errorf("%s = %+v with resume %s, want every member its view's kind takes, as that kind's (%v)", name, frame, frame.Resume, err)
		}
	}
	unsubscribe := decodeSocketFrameAs[LiveUnsubscribe](t, read("unsubscribe.json"))
	if unsubscribe.Type != "unsubscribe" || unsubscribe.Sub == "" {
		t.Errorf("unsubscribe.json = %+v, want an unsubscribe the socket reads", unsubscribe)
	}
}
