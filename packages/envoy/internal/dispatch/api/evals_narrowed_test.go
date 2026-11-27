package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// armedStatements records the text of every statement started on the pool it traces while it is
// armed (arm), and nothing while the rig seeds and refreshes its histories.
type armedStatements struct {
	mu    sync.Mutex
	armed bool
	sent  []string
}

func (a *armedStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed {
		a.sent = append(a.sent, data.SQL)
	}
	return ctx
}

func (a *armedStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// arm drops what was recorded and records from now on.
func (a *armedStatements) arm() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed, a.sent = true, nil
}

// taken is every statement recorded since arm, in the order they started.
func (a *armedStatements) taken() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}

// narrowedEvalsRig is the server the narrowed eval reads (dispatch://INFRA-2918, D7a; R21) are
// checked over: INFRA-2916's seeded delivery history of 30 days, every pull request of it counted
// for one project, and the Legion history of the golden windows (seedEvalHistories), with their
// day rows and stored figures written by the refresher as of the rig's moment, caught up.
type narrowedEvalsRig struct {
	handler  http.Handler
	served   *server
	database *store.Store
	registry *evals.Store
	fixture  deliveryseed.Fixture
	legion   string
	now      time.Time
	today    time.Time
}

// newNarrowedEvalsRig seeds and refreshes the rig's histories on a server made with options.
func newNarrowedEvalsRig(t *testing.T, options testServerOptions) *narrowedEvalsRig {
	t.Helper()
	handler, database, deps, served := newRegisteredTestServer(t, options)
	// INFRA-2916's attribution rule (attributionListOf) holds an issue to the repository its chain
	// names, and the history's issues name none while their pull requests are in three repositories:
	// read by the issue alone (seedEvalHistoriesByIssue), every pull request counts for DELIV.
	fixture, windows := seedEvalHistoriesByIssue(t, database, deps, 30)
	end := windows[0].Spec.Now
	if fixture.To.After(end) {
		end = fixture.To
	}
	now := end.Add(-time.Minute)
	return &narrowedEvalsRig{
		handler: handler, served: served, database: database, registry: evals.New(database, deps.Docs, deps.Events, deps.Attributor),
		fixture: fixture, legion: windows[0].Spec.Project, now: now, today: measures.Midnight(now),
	}
}

// atRead is each of entries' series over window and compare as the read-time path states them:
// their rows built in one snapshot (evals.ComputeAtRead, with no narrowing), with the deploy
// repositories the stored rows stand recorded under and each series' first day read in it, and
// folded by the fold of the stored rows (evals.SeriesValuesOf), by eval id.
func (r *narrowedEvalsRig) atRead(t *testing.T, entries []evals.Entry, window evals.Range, compare *evals.Range) map[string][]evals.SeriesValues {
	t.Helper()
	ctx := t.Context()
	tx, err := r.registry.BeginSnapshot(ctx)
	if err != nil {
		t.Fatalf("begin the read-time read: %v", err)
	}
	defer tx.Rollback(ctx)
	deployRepos, err := evals.DeployReposIn(ctx, tx)
	if err != nil {
		t.Fatalf("read the deploy repositories: %v", err)
	}
	firsts, err := evals.FirstDaysIn(ctx, tx, entries, deployRepos)
	if err != nil {
		t.Fatalf("read the first days: %v", err)
	}
	rows, err := evals.ComputeAtRead(ctx, tx, evals.AtRead{Entries: entries, DeployRepos: deployRepos, Window: window, Compare: compare, Now: r.now,
		Attributor: r.served.deps.Attributor})
	if err != nil {
		t.Fatalf("build the rows at read time: %v", err)
	}
	byEval := map[string][]evals.WindowRow{}
	for _, row := range rows {
		byEval[row.EvalID] = append(byEval[row.EvalID], row)
	}
	values := make(map[string][]evals.SeriesValues, len(entries))
	for _, entry := range entries {
		id := entry.Definition.EvalID
		values[id] = evals.SeriesValuesOf(entry, byEval[id], firsts[id], window, compare)
	}
	return values
}

// seedFailures turns on the failure measures of the rig's delivery project from 20 days before
// today, its settings saved again as the fixture saved them (deliveryseed.SaveRules) with every
// failure source, and writes its record: a GitHub deploy row of each production apply of the
// deploy repository that completed from that day on, shipping the pull requests it ships first
// (delivery.ContainingRun), and on every third of them a failure of each signal in turn, every
// other one restored by the next deploy and the fourth rejected. Then it refreshes the day rows
// again, as of the rig's moment. It answers how many failures it wrote.
func (r *narrowedEvalsRig) seedFailures(t *testing.T) int {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	project, rule := r.fixture.Project, r.fixture.Settings
	since := r.today.AddDate(0, 0, -20)
	saveProjectDelivery(t, pool, delivery.ProjectSettingsInput{
		Project: project, Delivery: true,
		Sources: []delivery.SourceInput{{
			Kind: delivery.SourceKindGitHubWorkflow, DeployWorkflowPath: new(rule.DeployWorkflowPath),
			ProductionJobNames: rule.ProductionJobNames, ProductionApplyStepName: rule.ProductionApplyStepName,
			StagingE2EJobName: rule.StagingE2EJobName, StagingApplyJobName: rule.StagingApplyJobName,
			PRChecksWorkflowPath: new(rule.PRChecksWorkflowPath), PRChecksRollupJobName: rule.PRChecksRollupJobName,
		}},
		PopulationAuthors: rule.PopulationAuthors, ExcludedRepos: rule.ExcludedRepos,
		FailureSources: delivery.FailureSources, FailuresMeasuredSince: &since,
	})
	shipped := map[int64][]string{}
	for _, pr := range r.fixture.PullRequests {
		if apply := delivery.ContainingRun(pr, r.fixture.DeployRepo, r.fixture.Applies); apply != nil {
			shipped[apply.RunID] = append(shipped[apply.RunID], fmt.Sprintf("%s#%d", pr.Repo, pr.Number))
		}
	}
	var deploys []prodevents.Event
	for _, apply := range r.fixture.Applies {
		if apply.CompletedAt.Before(since) || !apply.CompletedAt.Before(r.now) {
			continue
		}
		deploy, _, err := prodevents.UpsertGitHubDeploy(ctx, pool, prodevents.Event{
			ProjectKey: project, Kind: prodevents.KindDeploy, Source: prodevents.SourceGitHub, Status: prodevents.StatusSucceeded,
			SourceRef: prodevents.GitHubSourceRef(apply.RunID), Repo: r.fixture.DeployRepo,
			StartedAt: new(apply.CreatedAt), FinishedAt: new(apply.CompletedAt), PullRequests: shipped[apply.RunID],
			Actor: model.Actor{Kind: "system", ID: "github"},
		})
		if err != nil {
			t.Fatalf("write the deploy row of run %d: %v", apply.RunID, err)
		}
		deploys = append(deploys, deploy)
	}
	slices.SortStableFunc(deploys, func(a, b prodevents.Event) int { return a.FinishedAt.Compare(*b.FinishedAt) })
	signals := []struct{ signal, source string }{
		{prodevents.SignalCheckFail, prodevents.SourceCheckResults}, {prodevents.SignalRevert, prodevents.SourceReverts},
		{prodevents.SignalIncidentIssue, prodevents.SourceIncidentLabel}, {prodevents.SignalAPI, prodevents.SourceAPI},
		{prodevents.SignalPerson, prodevents.SourcePerson},
	}
	failures := 0
	for i := 0; i+1 < len(deploys); i += 3 {
		blamed, next := deploys[i], deploys[i+1]
		kind := signals[failures%len(signals)]
		failure := prodevents.Event{
			ProjectKey: project, Kind: prodevents.KindFailure, Source: kind.source, SourceRef: fmt.Sprintf("failure-%d", failures),
			DeployID: new(blamed.ID), Signal: new(kind.signal), DetectedAt: new(blamed.FinishedAt.Add(10 * time.Minute)),
		}
		if failures%2 == 0 {
			failure.RestoredAt, failure.RestoredBy, failure.RestoredByDeploy = next.FinishedAt, &model.Actor{Kind: "agent", ID: "release-bot"}, new(next.ID)
		}
		written := writeProductionEvent(t, pool, failure)
		if failures == 3 {
			if _, err := prodevents.SetState(ctx, pool, project, written.ID, prodevents.StateChange{
				State: prodevents.StateRejected, Reason: "not a failure of the deploy", Actor: model.Actor{Kind: "agent", ID: "release-bot"}, At: r.now,
			}); err != nil {
				t.Fatalf("reject failure %s: %v", written.SourceRef, err)
			}
		}
		failures++
	}
	if failures < len(signals) {
		t.Fatalf("the rig holds %d deploys from %s on, which carry %d failures, fewer than one of each of the %d signals",
			len(deploys), since.Format(time.DateOnly), failures, len(signals))
	}
	refreshEvalDays(t, r.registry, r.database, r.now)
	return failures
}

// list is the body of the list route target, decoded.
func (r *narrowedEvalsRig) list(t *testing.T, target string) evalListResponse {
	t.Helper()
	var answer evalListResponse
	if err := json.Unmarshal(getBody(t, r.handler, target), &answer); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	return answer
}

// windowQuery is the query of the whole UTC days of window, with extra's keys beside them.
func windowQuery(window evals.Range, extra url.Values) url.Values {
	query := url.Values{"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)}}
	for key, values := range extra {
		query[key] = values
	}
	return query
}

// stated is how many statistics of series hold a value over the window.
func stated(series []evals.SeriesValues) int {
	count := 0
	for _, one := range series {
		for _, value := range one.Window {
			if value != nil {
				count++
			}
		}
	}
	return count
}

// TestANarrowedReadWithNoFacetEqualsTheStoredNumber: every delivery and CI eval of the catalogue
// but unowned-p0 has a read-time path (evals.ComputedAtRead), and unowned-p0, a count taken at
// refresh time, is the one that has none. Over INFRA-2916's seeded history, with the failure
// measures on from 20 days before today over a record of its deploys and their failures
// (seedFailures), and the refresher caught up, at 7, 14, 28 and 84 days, each with and without the
// compare range before it, and one custom range with a custom compare range, every statistic of
// every series the read-time path builds with no narrowing (its window, compare range, change,
// first day, via-repository count and per-day arrays) is the stored answer of the list read, which
// reads the stored figures for the presets and the stored day rows for the custom range; and the
// three failure measures hold a statistic in some shape.
func TestANarrowedReadWithNoFacetEqualsTheStoredNumber(t *testing.T) {
	r := newNarrowedEvalsRig(t, testServerOptions{})
	failures := r.seedFailures(t)
	project := r.fixture.Project
	var built []evals.Entry
	held := map[string]bool{}
	for _, entry := range seededEntries(t, r.registry, project) {
		definition := entry.Definition
		if !definition.Computed() || definition.Kind != evals.KindDelivery && definition.Kind != evals.KindCI {
			continue
		}
		held[definition.Slug] = true
		if definition.Slug == evals.SlugUnownedP0 {
			if evals.ComputedAtRead(definition) {
				t.Errorf("unowned-p0, a count of open P0 issues taken at refresh time, has a read-time path, and no stored record of a past day holds that count")
			}
			continue
		}
		if !evals.ComputedAtRead(definition) {
			t.Errorf("%s, a %s eval of the catalogue, has no read-time path; unowned-p0 is the one eval that may have none", definition.Slug, definition.Kind)
			continue
		}
		built = append(built, entry)
	}
	if !held[evals.SlugUnownedP0] || len(built) == 0 {
		t.Fatalf("the catalogue's delivery and CI evals are %v, which hold unowned-p0 %t and %d evals with a read-time path", held, held[evals.SlugUnownedP0], len(built))
	}

	var shapes []evalShape
	for _, n := range []int{7, 14, 28, 84} {
		window, compare := evalDaysEnding(n, r.today), evalDaysEnding(n, r.today.AddDate(0, 0, -n))
		shapes = append(shapes,
			evalShape{name: fmt.Sprintf("%d days", n), window: window},
			evalShape{name: fmt.Sprintf("%d days with a compare range", n), window: window, compare: &compare})
	}
	custom, customCompare := evalDaysEnding(10, r.today.AddDate(0, 0, -3)), evalDaysEnding(9, r.today.AddDate(0, 0, -15))
	shapes = append(shapes, evalShape{name: "a custom range of 10 days with a custom compare range of 9", window: custom, compare: &customCompare})

	statistics, compared, failureStatistics := 0, 0, 0
	for _, shape := range shapes {
		stored, _ := readEvalShape(t, r.served, []string{project}, r.now, shape)
		atRead := r.atRead(t, built, shape.window, shape.compare)
		for _, entry := range built {
			definition := entry.Definition
			want := findEval(t, stored.Items, project, definition.Slug).Series
			got := atRead[definition.EvalID]
			if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, want); gotJSON != wantJSON {
				t.Errorf("%s: %s at read time is\n%s\nand from its stored rows\n%s", shape.name, definition.Slug, gotJSON, wantJSON)
				continue
			}
			statistics += stated(got)
			if definition.Slug == evals.SlugChangeFailRate || definition.Slug == evals.SlugFailedDeployRecovery || definition.Slug == evals.SlugDeploymentReworkRate {
				failureStatistics += stated(got)
			}
			compared++
		}
	}
	if statistics == 0 {
		t.Fatal("no series of any shape holds a statistic over its window, so the two paths were compared over nothing")
	}
	if failureStatistics == 0 {
		t.Fatalf("no series of the three failure measures holds a statistic over its window in any shape, over %d failures", failures)
	}
	t.Logf("%d shapes, %d series of %d evals compared, %d window statistics with a value (%d of the failure measures, over %d failures): the read-time path answers the stored rows' answer for each",
		len(shapes), compared, len(built), statistics, failureStatistics, failures)
}

// authorWhoseMedianDiffers is the population author whose merge-to-production median over window
// differs from every pull request's, with the minutes of each of that author's deployed pull
// requests, sorted: merge to its first shipping apply's completion, as the day builders time it
// (measures.MergeToProductionDay), over the fixture's own record of the applies.
func authorWhoseMedianDiffers(t *testing.T, fixture deliveryseed.Fixture, window evals.Range) (string, []float64) {
	t.Helper()
	minutes := func(author string) []float64 {
		var all []float64
		for _, pr := range fixture.PullRequests {
			if author != "" && pr.Author != author || pr.CreatedAt == nil || pr.MergedAt.Before(window.From) || !pr.MergedAt.Before(window.To) {
				continue
			}
			if apply := delivery.ContainingRun(pr, fixture.DeployRepo, fixture.Applies); apply != nil {
				all = append(all, float64(apply.CompletedAt.Sub(*pr.MergedAt))/float64(time.Minute))
			}
		}
		slices.Sort(all)
		return all
	}
	everyone := minutes("")
	for _, pr := range fixture.PullRequests {
		if own := minutes(pr.Author); len(own) > 1 && measures.Quantile(own, 0.5) != measures.Quantile(everyone, 0.5) {
			return pr.Author, own
		}
	}
	t.Fatalf("no author's merge-to-production median over %s differs from every pull request's", window.From.Format(time.DateOnly))
	return "", nil
}

// TestAFacetNarrowsThePullRequestEvalsAlone: an author facet on the cross-project read of the
// delivery project and the Legion project moves merge-to-production's median to the median of
// that author's pull requests, merge to deploy, and every series of the five evals that count pull
// requests names the facet; deploy-frequency, pr-ci, every other delivery and CI eval and every
// Legion eval answer what the read with no facet answers, naming none.
func TestAFacetNarrowsThePullRequestEvalsAlone(t *testing.T) {
	r := newNarrowedEvalsRig(t, testServerOptions{})
	project := r.fixture.Project
	window := evalDaysEnding(14, r.today)
	author, minutes := authorWhoseMedianDiffers(t, r.fixture, window)
	base := windowQuery(window, url.Values{"project": {project, r.legion}, "limit": {"200"}})
	plain := r.list(t, "/api/v1/evals?"+base.Encode())
	narrowed := r.list(t, "/api/v1/evals?"+windowQuery(window, url.Values{"project": {project, r.legion}, "limit": {"200"}, "author": {author}}).Encode())
	if len(narrowed.Items) != len(plain.Items) {
		t.Fatalf("the narrowed read answers %d evals, the read with no facet %d", len(narrowed.Items), len(plain.Items))
	}
	plainOf := map[string]evalListItem{}
	for _, item := range plain.Items {
		plainOf[item.ID] = item
	}
	pullRequestEvals, kept := 0, map[string]bool{}
	for _, item := range narrowed.Items {
		definition := item.Definition
		was, found := plainOf[item.ID]
		if !found {
			t.Fatalf("the narrowed read answers %s, which the read with no facet does not", item.ID)
		}
		if evals.CountsPullRequests(definition) {
			pullRequestEvals++
			for _, series := range item.Series {
				if !slices.Equal(series.Narrowed, []string{"author"}) {
					t.Errorf("%s's %s names %v as narrowing it, want [author]", item.ID, series.Series.Series, series.Narrowed)
				}
			}
			continue
		}
		kept[definition.Kind+" "+definition.Slug] = true
		for _, series := range item.Series {
			if series.Narrowed != nil {
				t.Errorf("%s's %s, which counts no pull request, names %v as narrowing it, want null", item.ID, series.Series.Series, series.Narrowed)
			}
		}
		if got, want := mustJSON(t, item.Series), mustJSON(t, was.Series); got != want {
			t.Errorf("the author facet moves %s, which counts no pull request:\nnarrowed %s\nplain    %s", item.ID, got, want)
		}
	}
	if pullRequestEvals != 2*5 {
		t.Errorf("%d evals of the two projects count pull requests, want each project's merge-to-production, first-commit-to-production, open-to-production, pr-open-to-merge and rework-share", pullRequestEvals)
	}
	for _, want := range []string{evals.KindDelivery + " " + evals.SlugDeployFrequency, evals.KindCI + " " + evals.SlugPRCI,
		evals.KindLegion + " " + evals.SlugLegionWorkTime, evals.KindLegion + " " + evals.SlugLegionRework} {
		if !kept[want] {
			t.Errorf("the read holds no %s to hold to its stored answer", want)
		}
	}

	got := seriesValues(t, findEval(t, narrowed.Items, project, evals.SlugMergeToProduction), measures.SeriesMinutes).Window
	all := seriesValues(t, findEval(t, plain.Items, project, evals.SlugMergeToProduction), measures.SeriesMinutes).Window
	want := measures.Quantile(minutes, 0.5)
	if got["median"] == nil || *got["median"] != want || got["n"] == nil || *got["n"] != float64(len(minutes)) {
		t.Fatalf("with author=%s merge-to-production's median is %v of %v pull requests, want %v of %d, the median of that author's", author,
			mustJSON(t, got["median"]), mustJSON(t, got["n"]), want, len(minutes))
	}
	if all["median"] == nil || *all["median"] == want {
		t.Fatalf("the read with no facet answers merge-to-production's median %v, which the facet should move off", mustJSON(t, all["median"]))
	}
	t.Logf("author=%s moves merge-to-production's median from %v to %v minutes (%d pull requests), and leaves %d other evals as stored", author, *all["median"], want, len(minutes), len(kept))
}

// leadTimeFigures is the figures of a lead time the measures route states (a Spread's median, p90
// and max, the count staying off its wire), under the names its eval states them by.
func leadTimeFigures(spread measures.Spread) evals.Statistics {
	return evals.Statistics{"median": spread.MedianMinutes, "p90": spread.P90Minutes, "max": spread.MaxMinutes}
}

// TestTheNarrowedValueEqualsTheBaseMeasuresRoute: for each facet of the delivery timeline and
// for its search, over the same window, the eval list read's merge-to-production,
// first-commit-to-production, open-to-production and pr-open-to-merge state the base measures
// route's lead times of the same name (each figure the route states: median, p90 and max), and
// rework-share its rework share, every pull request of the history counting for the one project.
func TestTheNarrowedValueEqualsTheBaseMeasuresRoute(t *testing.T) {
	r := newNarrowedEvalsRig(t, testServerOptions{})
	project := r.fixture.Project
	window := evalDaysEnding(14, r.today)
	first := r.fixture.PullRequests[len(r.fixture.PullRequests)-1]
	short := r.fixture.Repos[1][strings.LastIndex(r.fixture.Repos[1], "/")+1:]
	checks := []url.Values{
		{"author": {first.Author}},
		{"rework": {deliveryReworkRework}},
		{"deployed": {string(delivery.DeployedStatusDeployed)}},
		{"priority": {"P2", "__no_priority__"}},
		{"parent_agent": {deliveryNoSession}},
		{"q": {"to " + short}},
		{"author": {first.Author}, "rework": {deliveryReworkValue}},
	}
	for _, facets := range checks {
		name := facets.Encode()
		var base deliveryMeasuresResponse
		if err := json.Unmarshal(getBody(t, r.handler, "/api/v1/delivery/measures?"+windowQuery(window, facets).Encode()), &base); err != nil {
			t.Fatalf("%s: decode the measures: %v", name, err)
		}
		list := r.list(t, "/api/v1/projects/"+project+"/evals?"+windowQuery(window, url.Values{"kind": {evals.KindDelivery}}).Encode()+"&"+name)
		lead := base.Measures.LeadTime
		for slug, want := range map[string]measures.Spread{
			evals.SlugMergeToProduction: lead.MergeToProduction, evals.SlugFirstCommitToProduction: lead.FirstCommitToProduction,
			evals.SlugOpenToProduction: lead.OpenedToProduction, evals.SlugPROpenToMerge: lead.OpenedToMerge,
		} {
			got := seriesValues(t, findEval(t, list.Items, project, slug), measures.SeriesMinutes)
			if got.Narrowed == nil {
				t.Errorf("%s: %s names no narrowing", name, slug)
			}
			stated := evals.Statistics{"median": got.Window["median"], "p90": got.Window["p90"], "max": got.Window["max"]}
			if gotJSON, wantJSON := mustJSON(t, stated), mustJSON(t, leadTimeFigures(want)); gotJSON != wantJSON {
				t.Errorf("%s: %s states %s, the measures route %s", name, slug, gotJSON, wantJSON)
			}
		}
		share := seriesValues(t, findEval(t, list.Items, project, evals.SlugReworkShare), measures.SeriesPRs).Window["rate"]
		if share == nil || *share != base.Measures.ReworkShare {
			t.Errorf("%s: rework-share states %s, the measures route %v", name, mustJSON(t, share), base.Measures.ReworkShare)
		}
		counted := seriesValues(t, findEval(t, list.Items, project, evals.SlugPROpenToMerge), measures.SeriesMinutes).Window["n"]
		if counted == nil || *counted == 0 {
			t.Errorf("%s selects no pull request of the window, so the two answers agree on nothing", name)
			continue
		}
		t.Logf("%s: %v pull requests, merge to production's median %s minutes, as the measures route states it", name, *counted, mustJSON(t, lead.MergeToProduction.MedianMinutes))
	}
}

// TestTheRepositoryFilterAloneReadsTheStoredDayRows: a list read naming a repository and no other
// facet reads the stored rows (eval_days, or the stored figures of a preset's range) and no pull
// request, deploy or run row, and answers what the stored path answers for that repository; with
// the repository and an author named, it reads the pull requests, and its evals that count pull
// requests count only that repository's pull requests of that author.
func TestTheRepositoryFilterAloneReadsTheStoredDayRows(t *testing.T) {
	log := &armedStatements{}
	r := newNarrowedEvalsRig(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), log)})
	project, repo := r.fixture.Project, r.fixture.DeployRepo
	deliveryRows := []string{"delivery_pull_requests", "delivery_runs", "delivery_run_jobs"}
	read := func(window evals.Range, facets url.Values) (evalListResponse, []string) {
		t.Helper()
		log.arm()
		answer := r.list(t, "/api/v1/projects/"+project+"/evals?"+windowQuery(window, facets).Encode())
		return answer, log.taken()
	}
	reads := func(sent []string, table string) bool {
		return slices.ContainsFunc(sent, func(sql string) bool { return strings.Contains(sql, table) })
	}

	for _, shape := range []struct {
		name   string
		window evals.Range
		stored string
	}{
		{"28 days, the stored figures' range", evalDaysEnding(28, r.today), "eval_range_figures"},
		{"10 days, the day rows' range", evalDaysEnding(10, r.today), "from eval_days"},
	} {
		answer, sent := read(shape.window, url.Values{"repo": {repo}, "limit": {"200"}})
		if !reads(sent, shape.stored) {
			t.Errorf("%s of %s: the read sent no statement reading %s:\n%s", shape.name, repo, shape.stored, strings.Join(sent, "\n---\n"))
		}
		for _, table := range deliveryRows {
			if reads(sent, table) {
				t.Errorf("%s of %s: the read with the repository alone reads %s", shape.name, repo, table)
			}
		}
		stored, _ := readEvalShape(t, r.served, []string{project}, r.now, evalShape{name: shape.name, window: shape.window, repos: []string{repo}})
		if got, want := mustJSON(t, answer.Items), mustJSON(t, stored.Items); got != want {
			t.Errorf("%s of %s: the route answers\n%s\nand the stored path\n%s", shape.name, repo, got, want)
		}
	}

	window := evalDaysEnding(14, r.today)
	author := r.fixture.PullRequests[len(r.fixture.PullRequests)-1].Author
	answer, sent := read(window, url.Values{"repo": {repo}, "author": {author}, "kind": {evals.KindDelivery}})
	if !reads(sent, "from delivery_pull_requests") {
		t.Errorf("the read with a repository and an author reads no pull request:\n%s", strings.Join(sent, "\n---\n"))
	}
	opened, deployed := 0, 0
	for _, pr := range r.fixture.PullRequests {
		if pr.Repo != repo || pr.Author != author || pr.CreatedAt == nil || pr.MergedAt.Before(window.From) || !pr.MergedAt.Before(window.To) {
			continue
		}
		opened++
		if delivery.ContainingRun(pr, r.fixture.DeployRepo, r.fixture.Applies) != nil {
			deployed++
		}
	}
	if opened == 0 {
		t.Fatalf("%s merged no pull request of %s in the window", author, repo)
	}
	for slug, want := range map[string]int{evals.SlugPROpenToMerge: opened, evals.SlugMergeToProduction: deployed} {
		got := seriesValues(t, findEval(t, answer.Items, project, slug), measures.SeriesMinutes)
		if got.Window["n"] == nil || *got.Window["n"] != float64(want) || !slices.Equal(got.Narrowed, []string{"author"}) {
			t.Errorf("with repo=%s and author=%s, %s counts %s pull requests narrowed by %v, want %d by [author]", repo, author, slug, mustJSON(t, got.Window["n"]), got.Narrowed, want)
		}
	}
	t.Logf("repo=%s alone reads the stored rows; with author=%s the pull-request evals count %d of its pull requests, %d deployed", repo, author, opened, deployed)
}

// TestANarrowedViewIsMarkedByAPullRequestChangeInItsWindow: an evals view whose query sets a
// facet of the delivery timeline is narrowed, and a change of a pull request merged on its days,
// or of a deploy run, marks it, as such a change marks a measures view, while it marks no evals
// view that sets none and no narrowed view whose days end before it; a PR-checks run marks
// neither. Live, over the seeded history: moving one merge of the view's author within its days
// sends the narrowed view a delta within R14's 2 s, with no refresher pass, which lands on the
// list route's body, and the same view with no facet is sent nothing.
func TestANarrowedViewIsMarkedByAPullRequestChangeInItsWindow(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 28)
	viewOf := func(query string) *deliveryView {
		spec, refusal := parseDeliveryLiveView(evalsLiveView("INFRA", evalsDays(from, to), query))
		if refusal != nil {
			t.Fatalf("read the view of %q: %+v", query, refusal)
		}
		return &deliveryView{spec: spec}
	}
	narrowed, plain, searched, repoAlone := viewOf("author=dev-ana"), viewOf(""), viewOf("q=change&repo=acme/web"), viewOf("repo=acme/web")
	if !narrowed.spec.narrowed || !searched.spec.narrowed || plain.spec.narrowed || repoAlone.spec.narrowed {
		t.Fatalf("narrowed: author %t, q and repo %t, none %t, repo alone %t; want true, true, false, false",
			narrowed.spec.narrowed, searched.spec.narrowed, plain.spec.narrowed, repoAlone.spec.narrowed)
	}
	in, after := from.AddDate(0, 0, 3), to.AddDate(0, 0, 2)
	change := func(kind, runKind string, at time.Time) delivery.Change {
		return delivery.Change{Seq: 1, Kind: kind, RunKind: runKind, Repo: "acme/api", Ref: "7", TNew: &at}
	}
	for _, step := range []struct {
		what   string
		change delivery.Change
		marks  bool
	}{
		{"a pull request merged on the view's days", change(delivery.ChangeKindPullRequest, "", in), true},
		{"a pull request merged after the view's days", change(delivery.ChangeKindPullRequest, "", after), false},
		{"a deploy run", change(delivery.ChangeKindRun, string(delivery.DeliveryRunKindDeploy), in), true},
		{"a PR-checks run", change(delivery.ChangeKindRun, string(delivery.DeliveryRunKindPRChecks), in), false},
	} {
		if got := narrowed.touchedBy(step.change, to); got != step.marks {
			t.Errorf("%s marks the narrowed view %t, want %t", step.what, got, step.marks)
		}
		if plain.touchedBy(step.change, to) {
			t.Errorf("%s marks the view that sets no facet, whose every number is a stored row", step.what)
		}
	}

	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	// INFRA-2916's attribution rule (attributionListOf) counts no project for a pull request of a
	// repository its issue does not name; read by the issue alone, the moved merge counts for DELIV.
	fixture, _ := seedEvalHistoriesByIssue(t, database, deps, 30)
	rig := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	end := measures.Midnight(fixture.To)
	window := evalsDays(end.AddDate(0, 0, -28), end)
	var moved delivery.DeliveryPullRequest
	for _, pr := range fixture.PullRequests {
		if !pr.MergedAt.Before(end.AddDate(0, 0, -27)) && pr.MergedAt.Before(end.AddDate(0, 0, -20)) {
			moved = pr
			break
		}
	}
	if moved.MergedAt == nil {
		t.Fatal("no pull request merged in the first week of the view's days")
	}
	query := "author=" + url.QueryEscape(moved.Author)
	conns := map[string]*fakeLiveConn{"n": newFakeLiveConn(), "p": newFakeLiveConn()}
	frames := map[string]DeliveryLiveView{"n": evalsLiveView(fixture.Project, window, query), "p": evalsLiveView(fixture.Project, window, "")}
	held := map[string]json.RawMessage{}
	for _, sub := range []string{"n", "p"} {
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		held[sub] = conns[sub].snapshot(t, sub).Body
	}
	rig.settle(t)

	mergedAt := moved.MergedAt.AddDate(0, 0, 2)
	written := time.Now()
	if _, err := database.Pool.Exec(t.Context(), `update delivery_pull_requests set merged_at = $3 where repo = $1 and number = $2`,
		moved.Repo, moved.Number, mergedAt); err != nil {
		t.Fatalf("move the merge of %s#%d: %v", moved.Repo, moved.Number, err)
	}
	delta := conns["n"].delta(t, "n")
	took := time.Since(written)
	upserted := liveUpsertIDs(t, delta, "items")
	if !slices.Contains(upserted, evals.EvalID(fixture.Project, evals.SlugPROpenToMerge)) {
		t.Errorf("the narrowed view's delta upserts %v, want %s among them", upserted, evals.EvalID(fixture.Project, evals.SlugPROpenToMerge))
	}
	for _, id := range upserted {
		if slug := strings.TrimPrefix(id, fixture.Project+"/"); !slices.Contains([]string{evals.SlugMergeToProduction, evals.SlugFirstCommitToProduction,
			evals.SlugOpenToProduction, evals.SlugPROpenToMerge, evals.SlugReworkShare}, slug) {
			t.Errorf("the narrowed view's delta upserts %s, which counts no pull request and whose stored rows no pass rewrote", id)
		}
	}
	applied := applyLiveDelta(t, evalsView, held["n"], delta)
	route := getBody(t, handler, evalsTarget(t, fixture.Project, applied, query))
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, applied)), liveComparable(t, mustEncodeLive(t, evalsView, route)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the narrowed view's delta does not land on the list route's body:\napplied: %v\nGET:     %v", got, want)
	}
	conns["p"].none(t, time.Second, "a pull request's change, which moves no stored row until the refresher's pass")
	t.Logf("moving %s#%d's merge by two days sent the narrowed view (%s) a delta in %s, upserting %v", moved.Repo, moved.Number, query, took.Round(time.Millisecond), upserted)
}
