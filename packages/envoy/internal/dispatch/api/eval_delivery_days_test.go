package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// evalDaysBuild is the build the day rows these tests write record.
const evalDaysBuild = "eval-days-test"

// The windows TestTheDeliveryDayRowsFoldToWhatTheDeliveryRoutesAnswer compares: how many, the
// seed that picks them, and the widest the delivery routes allow (maxDeliveryWindow, 84 days).
const (
	evalDaysWindows    = 10
	evalDaysWindowSeed = 2916
	evalDaysMaxWindow  = 84
)

// writeDeliveryDays writes the delivery and CI day rows of every UTC day of [from, to), each for
// every project its rows count for, and answers the projects each day wrote.
func writeDeliveryDays(t *testing.T, registry *evals.Store, settings delivery.DeliverySettings, from, to time.Time) map[string][]string {
	t.Helper()
	written := map[string][]string{}
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		projects, err := registry.WriteDeliveryDay(t.Context(), settings, day, "", evalDaysBuild)
		if err != nil {
			t.Fatalf("write the day rows of %s: %v", day.Format(time.DateOnly), err)
		}
		written[day.Format(time.DateOnly)] = projects
	}
	return written
}

// storedDayRows reads every eval_days row of project, listed or not, every repository's, in key
// order: the rows the partition is checked over, and the unlisted job series DayWindow leaves out.
func storedDayRows(t *testing.T, pool *store.Pool, project string) []evals.DayRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		select eval_id, series, repo, day, listed, n, sum, num, den, max, samples, counts, via_repository
		from eval_days where project_key = $1 order by eval_id, series, repo, day
	`, project)
	if err != nil {
		t.Fatalf("read the day rows of %s: %v", project, err)
	}
	defer rows.Close()
	var stored []evals.DayRow
	for rows.Next() {
		row := evals.DayRow{ProjectKey: project}
		var counts []byte
		if err := rows.Scan(&row.EvalID, &row.Series, &row.Repo, &row.Day, &row.Listed, &row.N, &row.Sum, &row.Num, &row.Den,
			&row.Max, &row.Samples, &counts, &row.ViaRepository); err != nil {
			t.Fatalf("read the day rows of %s: %v", project, err)
		}
		if err := json.Unmarshal(counts, &row.Counts); err != nil {
			t.Fatalf("decode the counts of %s %s: %v", row.EvalID, row.Series, err)
		}
		row.Day = row.Day.UTC()
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the day rows of %s: %v", project, err)
	}
	return stored
}

// checkDayRowPartition is k16 over rows, one project's: for every eval, series and day, the rows
// of its repositories add up to its all-repositories row (n, sum, num, den, every count and
// via_repository; sums within 1e-9 of each other, relative to the larger), and their samples, put
// together, are its samples. A repository's row with no all-repositories row beside it, or an
// all-repositories row with no repository's row, fails too: every counted row is in one
// repository's rows; and so does a row whose via_repository is more than its n, the rows it
// counts. It answers how many series days it checked and how many repository rows they held.
func checkDayRowPartition(t *testing.T, rows []evals.DayRow) (seriesDays, repositoryRows int) {
	t.Helper()
	type key struct {
		evalID, series string
		day            time.Time
	}
	all := map[key]evals.DayRow{}
	byRepo := map[key][]evals.DayRow{}
	for _, row := range rows {
		if row.ViaRepository > row.N {
			t.Errorf("%s %s of %q on %s counts %d rows through a repository among its %d", row.EvalID, row.Series, row.Repo,
				row.Day.Format(time.DateOnly), row.ViaRepository, row.N)
		}
		k := key{row.EvalID, row.Series, row.Day}
		if row.Repo == "" {
			all[k] = row
			continue
		}
		byRepo[k] = append(byRepo[k], row)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b)) }
	value := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}
	for k := range byRepo {
		if _, held := all[k]; !held {
			t.Errorf("%s %s on %s has repository rows and no all-repositories row", k.evalID, k.series, k.day.Format(time.DateOnly))
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(all), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.evalID, b.evalID), cmp.Compare(a.series, b.series), a.day.Compare(b.day))
	}) {
		whole, parts := all[k], byRepo[k]
		name := fmt.Sprintf("%s %s on %s", k.evalID, k.series, k.day.Format(time.DateOnly))
		if len(parts) == 0 {
			t.Errorf("%s has an all-repositories row and no repository's row", name)
			continue
		}
		seriesDays, repositoryRows = seriesDays+1, repositoryRows+len(parts)
		n, via := 0, 0
		var sum, num, den float64
		counts := map[string]float64{}
		var samples []float64
		for _, part := range parts {
			n, via = n+part.N, via+part.ViaRepository
			sum, num, den = sum+value(part.Sum), num+value(part.Num), den+value(part.Den)
			for name, count := range part.Counts {
				counts[name] += count
			}
			samples = append(samples, part.Samples...)
		}
		wholeCounts := map[string]float64{}
		for name, count := range whole.Counts {
			wholeCounts[name] = count
		}
		wholeSamples := slices.Clone(whole.Samples)
		slices.Sort(samples)
		slices.Sort(wholeSamples)
		if n != whole.N || via != whole.ViaRepository || !near(sum, value(whole.Sum)) || !near(num, value(whole.Num)) ||
			!near(den, value(whole.Den)) || !maps.Equal(counts, wholeCounts) || !slices.Equal(samples, wholeSamples) {
			t.Errorf("%s: the %d repositories' rows add up to n %d, via %d, sum %g, num %g, den %g, counts %v, %d samples; the all-repositories row holds n %d, via %d, sum %g, num %g, den %g, counts %v, %d samples",
				name, len(parts), n, via, sum, num, den, counts, len(samples), whole.N, whole.ViaRepository, value(whole.Sum), value(whole.Num),
				value(whole.Den), wholeCounts, len(wholeSamples))
		}
	}
	return seriesDays, repositoryRows
}

// partsByEval is rows as the folds read them, by the slug of their eval.
func partsByEval(project string, rows []evals.DayRow) map[string][]measures.DayPart {
	parts := map[string][]measures.DayPart{}
	for _, row := range rows {
		slug := strings.TrimPrefix(row.EvalID, project+"/")
		parts[slug] = append(parts[slug], row.Part())
	}
	return parts
}

// jobParts is the unlisted job series of pr-ci among stored (every one of the project's rows),
// those of repo's cut on the days of [from, to): the parts the slowest-jobs table folds, which a
// DayWindow read, of listed rows alone, leaves out.
func jobParts(project string, stored []evals.DayRow, repo string, from, to time.Time) []measures.DayPart {
	var parts []measures.DayPart
	for _, row := range stored {
		if row.EvalID == evals.EvalID(project, evals.SlugPRCI) && !row.Listed && row.Repo == repo && !row.Day.Before(from) && row.Day.Before(to) {
			parts = append(parts, row.Part())
		}
	}
	return parts
}

// dayTiming is parts folded by measures.FoldTiming on each of days alone, the last partial, and
// over all of them: the shape of a Pipeline card the route answers.
func dayTiming(parts []measures.DayPart, days []string) measures.Timing {
	byDay := map[string][]measures.DayPart{}
	for _, part := range parts {
		byDay[part.Day.Format(time.DateOnly)] = append(byDay[part.Day.Format(time.DateOnly)], part)
	}
	timing := measures.Timing{Days: make([]measures.Day, len(days))}
	var shown []measures.DayPart
	for i, d := range days {
		timing.Days[i] = measures.Day{Day: d, Partial: i == len(days)-1, Cell: measures.FoldTiming(byDay[d])}
		shown = append(shown, byDay[d]...)
	}
	timing.Total = measures.FoldTiming(shown)
	return timing
}

// dayLeadTiming is parts folded by measures.FoldLeadTime on each of days alone and over all of them.
func dayLeadTiming(parts []measures.DayPart, days []string) measures.LeadTimeTiming {
	byDay := map[string][]measures.DayPart{}
	for _, part := range parts {
		byDay[part.Day.Format(time.DateOnly)] = append(byDay[part.Day.Format(time.DateOnly)], part)
	}
	timing := measures.LeadTimeTiming{Days: make([]measures.LeadTimeDay, len(days))}
	var shown []measures.DayPart
	for i, d := range days {
		timing.Days[i] = measures.LeadTimeDay{Day: d, Partial: i == len(days)-1, LeadTimeCell: measures.FoldLeadTime(byDay[d])}
		shown = append(shown, byDay[d]...)
	}
	timing.Total = measures.FoldLeadTime(shown)
	return timing
}

func leadSpread(parts []measures.DayPart) measures.Spread {
	cell := measures.FoldLeadTime(parts)
	return measures.Spread{MedianMinutes: cell.MedianMinutes, P90Minutes: cell.P90Minutes, MaxMinutes: cell.MaxMinutes}
}

// foldedMeasures is the measures route's measures folded from parts, one cut's day rows of a
// window, by the folds the route answers through; ok false, with what is missing, where the parts
// hold no deploy-frequency or deploy-run-success part, which read "no data".
func foldedMeasures(parts map[string][]measures.DayPart, window measures.Window) (measures.Result, string) {
	frequency := measures.FoldDeployFrequency(parts[evals.SlugDeployFrequency], window)
	success := measures.FoldDeployRunSuccess(parts[evals.SlugDeployRunSuccess])
	if frequency == nil || success == nil {
		return measures.Result{}, fmt.Sprintf("the day rows read no data for deploy-frequency (%v) or deploy-run-success (%v)", frequency != nil, success != nil)
	}
	share := measures.FoldReworkShare(parts[evals.SlugReworkShare])
	if share == nil {
		share = new(0.0)
	}
	return measures.Result{
		DeployFrequency: *frequency,
		LeadTime: measures.LeadTime{
			MergeToProduction:       leadSpread(parts[evals.SlugMergeToProduction]),
			FirstCommitToProduction: leadSpread(parts[evals.SlugFirstCommitToProduction]),
			OpenedToProduction:      leadSpread(parts[evals.SlugOpenToProduction]),
			OpenedToMerge:           leadSpread(parts[evals.SlugPROpenToMerge]),
		},
		ReworkShare:      *share,
		DeployRunSuccess: *success,
		Daily:            measures.DailySeries(parts[evals.SlugDeployFrequency], parts[evals.SlugDeployRunSuccess], window),
	}, ""
}

// foldedPipeline is the Pipeline route's day cards folded from parts and jobs (one cut's day rows
// and its job series), by the folds the route answers through.
func foldedPipeline(parts map[string][]measures.DayPart, jobs []measures.DayPart, settings delivery.DeliverySettings, days []string) deliveryPipelineResponse {
	cards := map[measures.DeployJobCardID]measures.DeployJobTiming{}
	slugs := map[measures.DeployJobCardID]string{
		measures.CardProductionApply: evals.SlugProductionDeploy, measures.CardStagingE2E: evals.SlugStagingE2E, measures.CardStagingApply: evals.SlugStagingDeploy,
	}
	for _, card := range measures.DeployCards(settings) {
		cards[card.CardID] = measures.DeployJobTiming{Job: strings.Join(card.JobNames, " or "), Timing: dayTiming(parts[slugs[card.CardID]], days)}
	}
	return deliveryPipelineResponse{
		Days:              days,
		PRChecks:          deliveryPipelinePRChecks{Run: dayTiming(parts[evals.SlugPRCI], days), SlowestJobs: measures.FoldSlowestJobs(jobs, days)},
		StagingE2E:        optionalDeployJobTiming(cards, measures.CardStagingE2E),
		StagingApply:      optionalDeployJobTiming(cards, measures.CardStagingApply),
		ProductionApply:   cards[measures.CardProductionApply],
		MergeToProduction: dayLeadTiming(parts[evals.SlugMergeToProduction], days),
		OpenedToMerge:     dayLeadTiming(parts[evals.SlugPROpenToMerge], days),
	}
}

// oracleTree is value as JSON, read as compareDeliveryOracleTrees reads an answer.
func oracleTree(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	return decodeDeliveryOracleTree(t, encoded)
}

// subtree is the value at path (keys joined by dots, "" for the root) of tree, with each of drop
// (paths below it) taken out of it in place.
func subtree(tree any, path string, drop ...string) any {
	node := tree
	if path != "" {
		for key := range strings.SplitSeq(path, ".") {
			node = node.(map[string]any)[key]
		}
	}
	for _, dropped := range drop {
		parent := node
		keys := strings.Split(dropped, ".")
		for _, key := range keys[:len(keys)-1] {
			parent = parent.(map[string]any)[key]
		}
		delete(parent.(map[string]any), keys[len(keys)-1])
	}
	return node
}

// evalDaysWindow is one window the day rows are compared over, what it compared, and the
// deploys with pull requests the day rows and the route both count in it.
type evalDaysWindow struct {
	from, to time.Time
	compared int
	withPRs  int
}

// compareDayRowsWithRoutes folds the day rows of window, read by DayWindow for the project's cut
// and each repository's, through the folds the delivery routes answer through, and compares each
// with the route's answer for that window: GET /api/v1/delivery/measures and
// GET /api/v1/delivery/pipeline for the all-repositories cut; the measures route's pull-request
// measures (lead_time, rework_share) under repo= for each repository's cut, the measures a
// delivery repo facet narrows; and the deploy repository's deploy, run and CI measures against the
// route's whole answer, since every run is the deploy repository's (every other repository's cut
// reads no data for them). Every value of those answers but the failure-flag measures' is
// compared, deploys_with_prs and with_prs_per_day included: a deploy with pull requests is one
// that ships any, whenever it merged, on both sides (delivery.Deploys.ShippedBy). It answers the
// window's tally and every difference found.
func compareDayRowsWithRoutes(t *testing.T, handler http.Handler, registry *evals.Store, fixture deliverytest.Fixture, settings delivery.DeliverySettings, stored []evals.DayRow, from, to time.Time) (evalDaysWindow, []string) {
	t.Helper()
	tally := evalDaysWindow{from: from, to: to}
	var differences []string
	differ := func(format string, args ...any) {
		differences = append(differences, fmt.Sprintf("%s..%s: ", from.Format(time.DateOnly), to.Format(time.DateOnly))+fmt.Sprintf(format, args...))
	}
	compare := func(what string, got, want any) {
		compared, difference := compareDeliveryOracleTrees("", got, want)
		tally.compared += compared
		if difference != "" {
			differ("%s differs from the route at %s", what, difference)
		}
	}
	window := measures.Window{From: from, To: to}
	days := pipelineWindowBetween(from, to).days()
	query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	read := func(repos []string) map[string][]measures.DayPart {
		rows, err := registry.DayWindow(t.Context(), fixture.Project, nil, repos, evals.Range{From: from, To: to}, nil)
		if err != nil {
			t.Fatalf("read the day rows of %s..%s for %v: %v", from.Format(time.DateOnly), to.Format(time.DateOnly), repos, err)
		}
		return partsByEval(fixture.Project, rows)
	}
	route := func(path string, values url.Values) any {
		response := dispatchRequest(t, handler, http.MethodGet, path+"?"+values.Encode(), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s?%s: status = %d, body = %s", path, values.Encode(), response.Code, response.Body.String())
		}
		return decodeDeliveryOracleTree(t, response.Body.Bytes())
	}
	// The failure-flag measures read no flag (no failure source exists), and their evals get no day
	// row: both are left out.
	measuresDropped := []string{"change_failure_rate", "time_to_restore"}
	pipelineDropped := []string{"computed_at", "freshness", "targets", "pr_checks.failing_jobs"}

	wholeMeasures := route("/api/v1/delivery/measures", query)
	wholePipeline := route("/api/v1/delivery/pipeline", query)
	all := read(nil)
	if folded, missing := foldedMeasures(all, window); missing != "" {
		differ("all repositories: %s", missing)
	} else {
		compare("the measures of all repositories", subtree(oracleTree(t, folded), "", measuresDropped...), subtree(wholeMeasures, "measures", measuresDropped...))
		tally.withPRs = folded.DeployFrequency.DeploysWithPRs
	}
	compare("the pipeline of all repositories",
		subtree(oracleTree(t, foldedPipeline(all, jobParts(fixture.Project, stored, "", from, to), settings, days)), "", pipelineDropped...),
		subtree(wholePipeline, "", pipelineDropped...))

	for _, repo := range fixture.Repos {
		cut := read([]string{repo})
		faceted := route("/api/v1/delivery/measures", url.Values{"from": query["from"], "to": query["to"], "repo": {repo}})
		folded, missing := foldedMeasures(cut, window)
		if repo != fixture.DeployRepo {
			if missing == "" {
				differ("%s, which holds no run, reads deploy measures from its day rows", repo)
			}
			// The pull-request measures alone, with the deploy measures every repository but the
			// deploy repository reads no data for.
			folded.LeadTime = measures.LeadTime{
				MergeToProduction: leadSpread(cut[evals.SlugMergeToProduction]), FirstCommitToProduction: leadSpread(cut[evals.SlugFirstCommitToProduction]),
				OpenedToProduction: leadSpread(cut[evals.SlugOpenToProduction]), OpenedToMerge: leadSpread(cut[evals.SlugPROpenToMerge]),
			}
			folded.ReworkShare = 0
			if share := measures.FoldReworkShare(cut[evals.SlugReworkShare]); share != nil {
				folded.ReworkShare = *share
			}
		} else if missing != "" {
			differ("%s: %s", repo, missing)
			continue
		}
		tree := oracleTree(t, folded)
		compare("the lead times of "+repo, subtree(tree, "lead_time"), subtree(faceted, "measures.lead_time"))
		compare("the rework share of "+repo, subtree(tree, "rework_share"), subtree(faceted, "measures.rework_share"))
		if repo != fixture.DeployRepo {
			continue
		}
		for _, field := range []string{"deploy_frequency", "deploy_run_success", "daily"} {
			compare("the "+field+" of "+repo, subtree(oracleTree(t, folded), field, dropBelow(field, measuresDropped)...),
				subtree(wholeMeasures, "measures."+field, dropBelow(field, measuresDropped)...))
		}
		cutPipeline := subtree(oracleTree(t, foldedPipeline(cut, jobParts(fixture.Project, stored, repo, from, to), settings, days)), "", pipelineDropped...)
		for _, field := range []string{"pr_checks", "staging_e2e", "staging_apply", "production_apply"} {
			compare("the "+field+" of "+repo, subtree(cutPipeline, field), subtree(subtree(wholePipeline, "", pipelineDropped...), field))
		}
	}
	return tally, differences
}

// dropBelow is the paths of dropped below field, relative to it.
func dropBelow(field string, dropped []string) []string {
	var below []string
	for _, path := range dropped {
		if rest, found := strings.CutPrefix(path, field+"."); found {
			below = append(below, rest)
		}
	}
	return below
}

// TestTheDeliveryDayRowsFoldToWhatTheDeliveryRoutesAnswer: over a deliverytest history whose every
// pull request counts for one project (Spec.OneProject: each names an issue of DELIV or none, and
// every repository is DELIV's), so the project's day rows hold every row the deployment-wide
// delivery routes count, the day rows of every day of the history are written; their partition by
// repository is checked (k16); and for evalDaysWindows seeded windows of 1 to 84 whole days, the
// rows of each cut, read by evals.Store.DayWindow, folded by the folds the routes answer through,
// equal the routes' answers (compareDayRowsWithRoutes): integers exact, floats within 1e-9. Then
// one day's rows of the first window are deleted, and the same comparison must differ.
func TestTheDeliveryDayRowsFoldToWhatTheDeliveryRoutesAnswer(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916, OneProject: true})
	ctx := t.Context()
	settings, err := delivery.GetSettings(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	registry := evals.New(database, nil, nil)

	started := time.Now()
	written := writeDeliveryDays(t, registry, settings, fixture.From, fixture.To)
	for day, projects := range written {
		if !slices.Equal(projects, []string{fixture.Project}) {
			t.Fatalf("the day rows of %s were written for %v, want %s alone: every pull request names an issue of it or none", day, projects, fixture.Project)
		}
	}
	stored := storedDayRows(t, database.Pool, fixture.Project)
	for _, row := range stored {
		if slug := strings.TrimPrefix(row.EvalID, fixture.Project+"/"); slug == evals.SlugChangeFailRate || slug == evals.SlugFailedDeployRecovery {
			t.Fatalf("%s holds a day row (%s on %s), want none: no failure source exists", row.EvalID, row.Series, row.Day.Format(time.DateOnly))
		}
	}
	if other := storedDayRows(t, database.Pool, fixture.SecondProject); len(other) != 0 {
		t.Fatalf("%s holds %d day rows, want none: no row of this history counts for it", fixture.SecondProject, len(other))
	}
	seriesDays, repositoryRows := checkDayRowPartition(t, stored)
	t.Logf("wrote %d days of %s's day rows (%d rows) in %s; the partition holds for %d series days over %d repository rows",
		len(written), fixture.Project, len(stored), time.Since(started).Round(time.Millisecond), seriesDays, repositoryRows)

	picker := rand.New(rand.NewPCG(evalDaysWindowSeed, 0))
	historyDays := int(fixture.To.Sub(fixture.From) / (24 * time.Hour))
	windows := make([][2]time.Time, evalDaysWindows)
	for i := range windows {
		length := 1 + picker.IntN(evalDaysMaxWindow)
		from := fixture.From.AddDate(0, 0, picker.IntN(historyDays-length+1))
		windows[i] = [2]time.Time{from, from.AddDate(0, 0, length)}
	}
	total := 0
	for _, window := range windows {
		tally, differences := compareDayRowsWithRoutes(t, handler, registry, fixture, settings, stored, window[0], window[1])
		for _, difference := range differences {
			t.Error(difference)
		}
		total += tally.compared
		t.Logf("%s..%s (%d days): %d values compared; deploys_with_prs %d in the day rows and the route alike",
			tally.from.Format(time.DateOnly), tally.to.Format(time.DateOnly), int(tally.to.Sub(tally.from).Hours()/24), tally.compared,
			tally.withPRs)
	}
	t.Logf("%d windows, every cut of each: %d values compared", len(windows), total)

	// The control: the first window with one of its days' rows deleted no longer folds to the
	// routes' answer.
	gone := windows[0][0]
	deleted, err := database.Pool.Exec(ctx, `delete from eval_days where project_key = $1 and day = $2`, fixture.Project, gone)
	if err != nil {
		t.Fatalf("delete the day rows of %s: %v", gone.Format(time.DateOnly), err)
	}
	_, differences := compareDayRowsWithRoutes(t, handler, registry, fixture, settings, storedDayRows(t, database.Pool, fixture.Project), windows[0][0], windows[0][1])
	if len(differences) == 0 {
		t.Fatalf("with the %d day rows of %s deleted, %s..%s still folds to the routes' answers, so the comparison cannot see a day it lacks",
			deleted.RowsAffected(), gone.Format(time.DateOnly), windows[0][0].Format(time.DateOnly), windows[0][1].Format(time.DateOnly))
	}
	t.Logf("the control: with the %d day rows of %s deleted, %d of the window's comparisons differ, the first: %s",
		deleted.RowsAffected(), gone.Format(time.DateOnly), len(differences), differences[0])
}

// TestADeployShippingBothProjectsCountsOnceDeploymentWideAndOnceInEachProject (the architect's
// condition): in deliverytest's default history, whose pull requests name issues of DELIV and, one
// in twenty, of DELIVX, the deploys that ship pull requests of both are counted once by
// GET /api/v1/delivery/measures over a window holding one, and once in each project's day rows,
// so the two projects' folded deploy-frequency deploys exceed the deployment-wide count by exactly
// the number of such deploys in the window. A delivery route that answered the sum of the
// projects' rows would count those deploys twice and fail here.
func TestADeployShippingBothProjectsCountsOnceDeploymentWideAndOnceInEachProject(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916})
	ctx := t.Context()
	settings, err := delivery.GetSettings(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	registry := evals.New(database, nil, nil)

	// The projects each deploy ships, by D1 over the fixture's own containment: a pull request's
	// issue's project, or the project every repository of the history is mapped to.
	issueProject := map[string]string{}
	for project, keys := range fixture.Issues {
		for _, key := range keys {
			issueProject[key] = project
		}
	}
	ships := map[int64]map[string]bool{}
	for _, pr := range fixture.PullRequests {
		apply := delivery.ContainingRun(pr, fixture.DeployRepo, fixture.Applies)
		if apply == nil {
			continue
		}
		project := fixture.Project
		if pr.IssueKey != nil {
			project = issueProject[*pr.IssueKey]
		}
		if ships[apply.RunID] == nil {
			ships[apply.RunID] = map[string]bool{}
		}
		ships[apply.RunID][project] = true
	}
	startedAt := map[int64]time.Time{}
	for _, run := range fixture.Runs {
		startedAt[run.RunID] = run.StartedAt
	}
	var target int64
	for _, apply := range fixture.Applies {
		if ships[apply.RunID][fixture.Project] && ships[apply.RunID][fixture.SecondProject] && startedAt[apply.RunID].After(fixture.From.AddDate(0, 0, 1)) {
			target = apply.RunID
			break
		}
	}
	if target == 0 {
		t.Fatalf("the default history holds no deploy shipping pull requests of both %s and %s", fixture.Project, fixture.SecondProject)
	}
	from := startedAt[target].UTC().Truncate(24 * time.Hour)
	to := from.AddDate(0, 0, 7)
	if to.After(fixture.To) {
		to = fixture.To
	}

	deploys, both, inFirst, inSecond := 0, 0, 0, 0
	for _, apply := range fixture.Applies {
		if at := startedAt[apply.RunID]; at.Before(from) || !at.Before(to) {
			continue
		}
		deploys++
		first, second := ships[apply.RunID][fixture.Project], ships[apply.RunID][fixture.SecondProject]
		if first || len(ships[apply.RunID]) == 0 {
			// A deploy that ships none counts for the deploy repository's project.
			inFirst++
		}
		if second {
			inSecond++
		}
		if first && second {
			both++
		}
	}

	query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
	wide := getDeliveryMeasures(t, handler, query).Measures.DeployFrequency.SuccessfulDeploys
	if wide != deploys {
		t.Fatalf("GET /api/v1/delivery/measures?%s counts %d deploys, want the %d deploys the window holds, each once", query, wide, deploys)
	}
	writeDeliveryDays(t, registry, settings, from, to)
	folded := map[string]int{}
	for _, project := range []string{fixture.Project, fixture.SecondProject} {
		rows, err := registry.DayWindow(ctx, project, []string{evals.EvalID(project, evals.SlugDeployFrequency)}, nil, evals.Range{From: from, To: to}, nil)
		if err != nil {
			t.Fatalf("read the deploy-frequency rows of %s: %v", project, err)
		}
		frequency := measures.FoldDeployFrequency(partsByEval(project, rows)[evals.SlugDeployFrequency], measures.Window{From: from, To: to})
		if frequency == nil {
			t.Fatalf("%s's deploy-frequency reads no data over %s..%s", project, from.Format(time.DateOnly), to.Format(time.DateOnly))
		}
		folded[project] = frequency.SuccessfulDeploys
	}
	if folded[fixture.Project] != inFirst || folded[fixture.SecondProject] != inSecond {
		t.Errorf("the day rows count %d deploys for %s and %d for %s, want %d and %d", folded[fixture.Project], fixture.Project,
			folded[fixture.SecondProject], fixture.SecondProject, inFirst, inSecond)
	}
	if both == 0 || folded[fixture.Project]+folded[fixture.SecondProject]-wide != both {
		t.Errorf("%s's %d deploys and %s's %d exceed the deployment-wide %d by %d, want by the %d deploys that ship pull requests of both",
			fixture.Project, folded[fixture.Project], fixture.SecondProject, folded[fixture.SecondProject], wide,
			folded[fixture.Project]+folded[fixture.SecondProject]-wide, both)
	}

	// Run target itself: one item of each project's delivery family, on its start day.
	rows, err := database.Pool.Query(ctx, `
		select project_key || ' ' || day::text from eval_day_members
		where family = $1 and row_key = $2 order by project_key
	`, evals.KindDelivery, evals.RunMember(fixture.DeployRepo, target))
	if err != nil {
		t.Fatalf("read the items of run %d: %v", target, err)
	}
	var counted []string
	for rows.Next() {
		var where string
		if err := rows.Scan(&where); err != nil {
			t.Fatalf("read the items of run %d: %v", target, err)
		}
		counted = append(counted, where)
	}
	rows.Close()
	day := startedAt[target].UTC().Format(time.DateOnly)
	if want := []string{fixture.Project + " " + day, fixture.SecondProject + " " + day}; !slices.Equal(counted, want) {
		t.Errorf("run %d is counted in %v, want once in each of %v", target, counted, want)
	}
	t.Logf("%s..%s: run %d ships pull requests of %s and %s; the route counts %d deploys, %s's day rows %d and %s's %d, %d deploys of the window shipping both",
		from.Format(time.DateOnly), to.Format(time.DateOnly), target, fixture.Project, fixture.SecondProject, wide,
		fixture.Project, folded[fixture.Project], fixture.SecondProject, folded[fixture.SecondProject], both)
}
