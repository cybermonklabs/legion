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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The windows TestTheDeliveryDayRowsFoldToWhatTheDeliveryRoutesAnswer compares: how many, the
// seed that picks them, and the widest the delivery routes allow (maxDeliveryWindow, 84 days).
const (
	evalDaysWindows    = 10
	evalDaysWindowSeed = 2916
	evalDaysMaxWindow  = 84
)

// refreshEvalDays writes the day rows of database as the server's refresher writes them, as of now
// (evals.Store.RefreshThrough, under testServerBuild): every project seeded, and every family's
// rows from its first input day through now's UTC day, each day with its zero rows.
func refreshEvalDays(t *testing.T, registry *evals.Store, database *store.Store, now time.Time) {
	t.Helper()
	deps := evals.RefresherDeps{Horizon: events.NewHorizon(database.Pool), Build: testServerBuild}
	if err := registry.RefreshThrough(t.Context(), deps, now); err != nil {
		t.Fatalf("refresh the day rows as of %s: %v", now.Format(time.RFC3339), err)
	}
}

// publishDefaultRepos publishes every project's default repository (projects.default_repo) as the
// evals refresher's page commit does (eval_days_defaults, evals.publishDefaults): the evals reads
// and the delivery routes take the defaults from that record, so a test's write of default_repo by
// SQL moves no read until it is published.
func publishDefaultRepos(t *testing.T, pool *store.Pool) {
	t.Helper()
	for _, statement := range []string{
		`delete from eval_days_defaults`,
		`insert into eval_days_defaults (project_key, repo) select key, default_repo from projects where default_repo is not null`,
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("publish the projects' default repositories: %v", err)
		}
	}
}

// leaveAttributionInputsUnread clears every stored pull request's attribution inputs, as a row
// stored before its GitHub facts were read for them holds none, so the issue its row names
// (issue_key) stands as its attribution: no inputs were read, and a row with none counts by the
// one list its issue_key gives (evals/delivery_days.go's attribution). A test whose oracle is
// deliverytest's issue_key, the project of the issue it names each pull request after, reads its
// rows this way: the history's issues own no repository and only its project's default is the
// deploy repository, so with the inputs read most of its pull requests would count by the
// published defaults (under owner keys whose lists name the issue's project), and those of its
// other repositories, and every one naming an issue of its second project, for no project the
// issue names.
func leaveAttributionInputsUnread(t *testing.T, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		update delivery_pull_requests
		set attribution_title_keys = null, attribution_cited_issues = null, attribution_branch_keys = null, attribution_commit_keys = null
	`); err != nil {
		t.Fatalf("leave the pull requests' attribution inputs unread: %v", err)
	}
}

// storedDayRows reads every eval_days row of project, listed or not, every repository's, in key
// order: the rows the partition is checked over, the unowned-p0 rows of each repository among them,
// and the unlisted job series DayWindowIn leaves out.
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

// checkDayRowPartition is the repository partition over rows, one project's: for every eval,
// series and day, the rows of its repositories add up to its all-repositories row (n, sum, num,
// den, every count and via_repository; sums within 1e-9 of each other, relative to the larger),
// and their samples, put together, are its samples. A repository's row with no all-repositories
// row beside it, or an all-repositories row that counts a row with no repository's row, fails too:
// every counted row is in one repository's rows (a zero row, which counts none, has none); and so
// does a row whose via_repository is more than its n, the rows it counts. It answers how many
// series days it checked and how many repository rows they held.
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
			if whole.N != 0 {
				t.Errorf("%s has an all-repositories row of %d rows and no repository's row", name, whole.N)
			}
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
func partsByEval(project string, rows []evals.WindowRow) map[string][]measures.DayPart {
	parts := map[string][]measures.DayPart{}
	for _, row := range rows {
		slug := strings.TrimPrefix(row.EvalID, project+"/")
		parts[slug] = append(parts[slug], row.Part)
	}
	return parts
}

// seededEntries seeds project's evals as the project's creation does (evals.Store.Ensure) and
// answers every one as the list read holds it (evals.Store.Current): the entries a DayWindowIn read
// of the project's evals names.
func seededEntries(t *testing.T, registry *evals.Store, project string) []evals.Entry {
	t.Helper()
	if _, err := registry.Ensure(t.Context(), project); err != nil {
		t.Fatalf("seed the evals of %s: %v", project, err)
	}
	page, err := registry.Current(t.Context(), []string{project}, "", 200, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
	return page.Items
}

// jobParts is the unlisted job series of pr-ci among stored (every one of the project's rows),
// those of repo's cut on the days of [from, to): the parts the slowest-jobs table folds, which a
// DayWindowIn read, of listed rows alone, leaves out.
func jobParts(project string, stored []evals.DayRow, repo string, from, to time.Time) []measures.DayPart {
	var parts []measures.DayPart
	for _, row := range stored {
		if row.EvalID == evals.EvalID(project, evals.SlugPRCI) && !row.Listed && row.Repo == repo && !row.Day.Before(from) && row.Day.Before(to) {
			parts = append(parts, row.DayPart)
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
	return measures.Spread{N: cell.N, MedianMinutes: cell.MedianMinutes, P90Minutes: cell.P90Minutes, MaxMinutes: cell.MaxMinutes}
}

// foldedMeasures is the measures route's measures folded from parts, one cut's day rows of a
// window, by the folds the route answers through, change-fail-rate's and failed-deploy-recovery's
// over their own day rows, of which no failure source writes any; ok false, with what is missing,
// where the parts hold no deploy-frequency or deploy-run-success part, which read "no data".
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
		ChangeFailureRate: measures.FoldChangeFailureRate(parts[evals.SlugChangeFailRate]),
		TimeToRestore:     measures.FoldTimeToRestore(parts[evals.SlugFailedDeployRecovery]),
		ReworkShare:       *share,
		DeployRunSuccess:  *success,
		Daily:             measures.DailySeries(parts[evals.SlugDeployFrequency], parts[evals.SlugDeployRunSuccess], window),
	}, ""
}

// foldedPipeline is the Pipeline route's day cards folded from parts and jobs (one cut's day rows
// and its job series), by the folds the route answers through, its slowest jobs the first
// measures.SlowestJobs of the ranking by minN timed results, and its targets and its lead-time
// cards' verdicts stated as the route states them (deliveryTargets, deliveryVerdict over
// evals.LeadTimeStatistics of each card's total) by deployRepoEvals, the evals the route reads for
// the deploy repository (evals.Store.DeployRepoEvals).
func foldedPipeline(parts map[string][]measures.DayPart, jobs []measures.DayPart, settings delivery.DeliverySettings, days []string, minN int, deployRepoEvals map[string]evals.Entry) deliveryPipelineResponse {
	cards := map[measures.DeployJobCardID]measures.DeployJobTiming{}
	slugs := map[measures.DeployJobCardID]string{
		measures.CardProductionApply: evals.SlugProductionDeploy, measures.CardStagingE2E: evals.SlugStagingE2E, measures.CardStagingApply: evals.SlugStagingDeploy,
	}
	for _, card := range measures.DeployCards(settings) {
		cards[card.CardID] = measures.DeployJobTiming{Job: strings.Join(card.JobNames, " or "), Timing: dayTiming(parts[slugs[card.CardID]], days)}
	}
	ranked := measures.FoldSlowestJobs(jobs, days, minN)
	mergeToProduction, openedToMerge := dayLeadTiming(parts[evals.SlugMergeToProduction], days), dayLeadTiming(parts[evals.SlugPROpenToMerge], days)
	verdict := func(slug string, timing measures.LeadTimeTiming) *bool {
		return deliveryVerdict(deployRepoEvals, slug, map[string]evals.Statistics{measures.SeriesMinutes: evals.LeadTimeStatistics(timing.Total)})
	}
	return deliveryPipelineResponse{
		Days: days,
		PRChecks: deliveryPipelinePRChecks{
			Run: dayTiming(parts[evals.SlugPRCI], days), SlowestJobs: ranked[:min(len(ranked), measures.SlowestJobs)], SlowestMinResults: minN,
		},
		StagingE2E:        optionalDeployJobTiming(cards, measures.CardStagingE2E),
		StagingApply:      optionalDeployJobTiming(cards, measures.CardStagingApply),
		ProductionApply:   cards[measures.CardProductionApply],
		MergeToProduction: mergeToProduction,
		OpenedToMerge:     openedToMerge,
		Targets:           deliveryTargets(deployRepoEvals),
		Status: deliveryPipelineStatus{
			MergeToProduction: verdict(evals.SlugMergeToProduction, mergeToProduction),
			OpenedToMerge:     verdict(evals.SlugPROpenToMerge, openedToMerge),
		},
	}
}

// prCIJobsMinN is the min_n of pr-ci's job series among entries, as the Pipeline reads it
// (evals.SlowestJobsMinN), the threshold the Pipeline of a deploy repository mapped to their
// project ranks by.
func prCIJobsMinN(t *testing.T, project string, entries []evals.Entry) int {
	t.Helper()
	for _, entry := range entries {
		if entry.Definition.EvalID != evals.EvalID(project, evals.SlugPRCI) {
			continue
		}
		for _, series := range entry.Series {
			if series.Series == evals.SeriesJobs {
				return evals.SlowestJobsMinN(series.MinN)
			}
		}
	}
	t.Fatalf("%s holds no pr-ci job series among its %d evals", project, len(entries))
	return 0
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

// compareDayRowsWithRoutes folds the day rows of window, read by DayWindowIn on pool for the
// project's cut and each repository's, through the folds the delivery routes answer through, and
// compares each with the route's answer: GET /api/v1/delivery/measures for that window, and
// GET /api/v1/delivery/pipeline at the window's last instant, which shows the seven days ending on
// it whatever the window's length and so is compared with the rows of those seven days, for the
// all-repositories cut; the measures route's pull-request measures (lead_time, rework_share)
// under repo= for each repository's cut, the measures a delivery repo facet narrows; and the
// deploy repository's deploy, run and CI measures against the route's whole answer, since every
// run is the deploy repository's (every other repository's cut reads no data for them). Every
// value of those answers is compared, the failure-flag measures' no value on both sides and
// deploys_with_prs and with_prs_per_day included: a deploy with pull requests is one that ships
// any, whenever it merged, on both sides (delivery.Deploys.ShippedBy). It answers the window's
// tally and every difference found.
func compareDayRowsWithRoutes(t *testing.T, handler http.Handler, registry *evals.Store, pool *store.Pool, fixture deliverytest.Fixture, settings delivery.DeliverySettings, entries []evals.Entry, stored []evals.DayRow, from, to time.Time) (evalDaysWindow, []string) {
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
	// The Pipeline reads at alone: the seven days ending on the window's last instant.
	pipelineAt := to.Add(-time.Second)
	pipelineSpan := evals.Range{From: measures.WindowStart(pipelineAt), To: to}
	pipelineDays := measures.ShownDays(pipelineAt)
	query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	read := func(repos []string, span evals.Range) map[string][]measures.DayPart {
		rows, err := evals.DayWindowIn(t.Context(), pool, []string{fixture.Project}, entries, fixture.DeployRepo, repos, span, nil)
		if err != nil {
			t.Fatalf("read the day rows of %s..%s for %v: %v", span.From.Format(time.DateOnly), span.To.Format(time.DateOnly), repos, err)
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
	pipelineDropped := []string{"computed_at", "freshness", "pr_checks.failing_jobs"}
	// The evals each route judges its figures by, read as each reads them for the deploy
	// repository.
	targetEvals, err := registry.DeployRepoEvals(t.Context(), settings.DeployRepo, evals.DeliveryTargetSlugs)
	if err != nil {
		t.Fatalf("read the measures' evals of %s: %v", settings.DeployRepo, err)
	}
	pipelineEvals, err := registry.DeployRepoEvals(t.Context(), settings.DeployRepo, evals.PipelineSlugs)
	if err != nil {
		t.Fatalf("read the Pipeline's evals of %s: %v", settings.DeployRepo, err)
	}

	wholeMeasures := route("/api/v1/delivery/measures", query)
	wholePipeline := route("/api/v1/delivery/pipeline", url.Values{"at": {pipelineAt.Format(time.RFC3339)}})
	all := read(nil, evals.Range{From: from, To: to})
	if folded, missing := foldedMeasures(all, window); missing != "" {
		differ("all repositories: %s", missing)
	} else {
		compare("the measures of all repositories", oracleTree(t, folded), subtree(wholeMeasures, "measures"))
		// The targets and each KPI's verdict, stated from the folded measures as the route states
		// them from its own (deliveryTargets, measuresStatus over evals.ResultStatistics), the
		// unowned P0 issues being those open now, which no day row of a past window counts.
		compare("the targets of the measures of all repositories", oracleTree(t, deliveryTargets(targetEvals)), subtree(wholeMeasures, "targets"))
		compare("the status of the measures of all repositories",
			oracleTree(t, measuresStatus(targetEvals, evals.ResultStatistics(folded, len(fixture.UnownedP0)))), subtree(wholeMeasures, "status"))
		tally.withPRs = folded.DeployFrequency.DeploysWithPRs
	}
	minN := prCIJobsMinN(t, fixture.Project, entries)
	compare("the pipeline of all repositories",
		subtree(oracleTree(t, foldedPipeline(read(nil, pipelineSpan), jobParts(fixture.Project, stored, "", pipelineSpan.From, pipelineSpan.To), settings, pipelineDays, minN, pipelineEvals)), "", pipelineDropped...),
		subtree(wholePipeline, "", pipelineDropped...))

	for _, repo := range fixture.Repos {
		cut := read([]string{repo}, evals.Range{From: from, To: to})
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
			compare("the "+field+" of "+repo, subtree(oracleTree(t, folded), field), subtree(wholeMeasures, "measures."+field))
		}
		cutPipeline := subtree(oracleTree(t, foldedPipeline(read([]string{repo}, pipelineSpan), jobParts(fixture.Project, stored, repo, pipelineSpan.From, pipelineSpan.To), settings, pipelineDays, minN, pipelineEvals)), "", pipelineDropped...)
		for _, field := range []string{"pr_checks", "staging_e2e", "staging_apply", "production_apply"} {
			compare("the "+field+" of "+repo, subtree(cutPipeline, field), subtree(subtree(wholePipeline, "", pipelineDropped...), field))
		}
	}
	return tally, differences
}

// TestTheDeliveryDayRowsFoldToWhatTheDeliveryRoutesAnswer: over a deliverytest history whose every
// pull request names an issue of one project (Spec.OneProject), its attribution inputs left unread
// so the issue each row names is its attribution (leaveAttributionInputsUnread), and whose deploy
// repository is that project's default repository, so the project's read holds every row the
// deployment-wide delivery
// routes count (what an issue attributes in its own rows, and what no issue attributes — a deploy
// that ships none, a CI run of no population pull request — in the rows of the repository it
// publishes as its default, R15), the day rows of every day of the history are written as the
// server's refresher writes them (refreshEvalDays); their partition by repository is checked; and
// for evalDaysWindows seeded windows of 1 to 84 whole days, the rows of each cut, read by
// evals.DayWindowIn, folded by the folds the routes answer through, equal the routes' answers
// (compareDayRowsWithRoutes): integers exact, floats within 1e-9. Then one day's rows of the first
// window are deleted, and the same comparison must differ.
func TestTheDeliveryDayRowsFoldToWhatTheDeliveryRoutesAnswer(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916, OneProject: true})
	leaveAttributionInputsUnread(t, database.Pool)
	ctx := t.Context()
	settings, err := delivery.GetSettings(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	entries := seededEntries(t, registry, fixture.Project)

	started := time.Now()
	refreshEvalDays(t, registry, database, fixture.To.Add(-time.Minute))
	stored := storedDayRows(t, database.Pool, fixture.Project)
	for _, row := range stored {
		if slug := strings.TrimPrefix(row.EvalID, fixture.Project+"/"); slug == evals.SlugChangeFailRate || slug == evals.SlugFailedDeployRecovery {
			t.Fatalf("%s holds a day row (%s on %s), want none: no failure source exists", row.EvalID, row.Series, row.Day.Format(time.DateOnly))
		}
	}
	// The refresher writes every project's unowned-p0 row of today, which counts none of the
	// second project's issues: this history holds no open P0 issue of it.
	for _, row := range storedDayRows(t, database.Pool, fixture.SecondProject) {
		if row.EvalID != evals.EvalID(fixture.SecondProject, evals.SlugUnownedP0) || row.Repo != "" || row.N != 0 {
			t.Errorf("%s holds a day row of %s %s in %q on %s, n %d; want its unowned-p0 row of 0 alone: no row of this history counts for it",
				fixture.SecondProject, row.EvalID, row.Series, row.Repo, row.Day.Format(time.DateOnly), row.N)
		}
	}
	unownedRepoRows := 0
	for _, row := range stored {
		if row.EvalID == evals.EvalID(fixture.Project, evals.SlugUnownedP0) && row.Repo != "" {
			unownedRepoRows++
		}
	}
	if unownedRepoRows == 0 {
		t.Errorf("%s holds no repository's unowned-p0 row beside its %d open P0 issues with no owner, so the partition checks none", fixture.Project, len(fixture.UnownedP0))
	}
	seriesDays, repositoryRows := checkDayRowPartition(t, stored)
	written := map[time.Time]bool{}
	for _, row := range stored {
		written[row.Day] = true
	}
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
		tally, differences := compareDayRowsWithRoutes(t, handler, registry, database.Pool, fixture, settings, entries, stored, window[0], window[1])
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
	_, differences := compareDayRowsWithRoutes(t, handler, registry, database.Pool, fixture, settings, entries, storedDayRows(t, database.Pool, fixture.Project), windows[0][0], windows[0][1])
	if len(differences) == 0 {
		t.Fatalf("with the %d day rows of %s deleted, %s..%s still folds to the routes' answers, so the comparison cannot see a day it lacks",
			deleted.RowsAffected(), gone.Format(time.DateOnly), windows[0][0].Format(time.DateOnly), windows[0][1].Format(time.DateOnly))
	}
	t.Logf("the control: with the %d day rows of %s deleted, %d of the window's comparisons differ, the first: %s",
		deleted.RowsAffected(), gone.Format(time.DateOnly), len(differences), differences[0])
}

// TestADeployShippingBothProjectsCountsOnceDeploymentWideAndOnceInEachProject (the architect's
// condition): in deliverytest's default history, whose pull requests name issues of DELIV, one in
// twenty of DELIVX, and some none, their attribution inputs left unread so the issue each row names
// is its attribution (leaveAttributionInputsUnread), and whose deploy repository is DELIV's default
// repository, the deploys that ship pull requests of both are counted once by
// GET /api/v1/delivery/measures over a
// window holding one, and once in each project's read; a deploy that ships no pull request of
// either project counts for DELIV alone, through the repository it publishes as its default (R15),
// and one that ships a pull request of a project through its issue counts in that project once,
// not twice (its repository's row names the project among its sharers, which the project's read
// skips). So the two projects' folded deploy-frequency deploys, less the deploys that ship both,
// are the deployment-wide count. A delivery route that answered the sum of the projects' rows
// would count the deploys of both twice, and fail here.
func TestADeployShippingBothProjectsCountsOnceDeploymentWideAndOnceInEachProject(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916})
	leaveAttributionInputsUnread(t, database.Pool)
	ctx := t.Context()
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)

	// The projects each deploy ships, by the attribution rule over the fixture's own containment:
	// a pull request's issue's project, and none for one that names no issue.
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
		if pr.IssueKey == nil {
			continue
		}
		project := issueProject[*pr.IssueKey]
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

	deploys, both, inFirst, inSecond, none := 0, 0, 0, 0, 0
	for _, apply := range fixture.Applies {
		if at := startedAt[apply.RunID]; at.Before(from) || !at.Before(to) {
			continue
		}
		deploys++
		first, second := ships[apply.RunID][fixture.Project], ships[apply.RunID][fixture.SecondProject]
		if first {
			inFirst++
		}
		if second {
			inSecond++
		}
		if first && second {
			both++
		}
		if !first && !second {
			// It ships no pull request of a project, so it counts, through its repository, for the
			// project whose default repository that is: the first.
			none++
		}
	}

	query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
	wide := getDeliveryMeasures(t, handler, query).Measures.DeployFrequency.SuccessfulDeploys
	if wide != deploys {
		t.Fatalf("GET /api/v1/delivery/measures?%s counts %d deploys, want the %d deploys the window holds, each once", query, wide, deploys)
	}
	refreshEvalDays(t, registry, database, fixture.To.Add(-time.Minute))
	folded := map[string]int{}
	for _, project := range []string{fixture.Project, fixture.SecondProject} {
		entries := seededEntries(t, registry, project)
		entries = slices.DeleteFunc(entries, func(entry evals.Entry) bool {
			return entry.Definition.EvalID != evals.EvalID(project, evals.SlugDeployFrequency)
		})
		rows, err := evals.DayWindowIn(ctx, database.Pool, []string{project}, entries, fixture.DeployRepo, nil, evals.Range{From: from, To: to}, nil)
		if err != nil {
			t.Fatalf("read the deploy-frequency rows of %s: %v", project, err)
		}
		frequency := measures.FoldDeployFrequency(partsByEval(project, rows)[evals.SlugDeployFrequency], measures.Window{From: from, To: to})
		if frequency == nil {
			t.Fatalf("%s's deploy-frequency reads no data over %s..%s", project, from.Format(time.DateOnly), to.Format(time.DateOnly))
		}
		folded[project] = frequency.SuccessfulDeploys
	}
	if folded[fixture.Project] != inFirst+none || folded[fixture.SecondProject] != inSecond {
		t.Errorf("the day rows count %d deploys for %s and %d for %s, want %d (the %d of its issues and the %d of no project, through its default repository) and %d",
			folded[fixture.Project], fixture.Project, folded[fixture.SecondProject], fixture.SecondProject, inFirst+none, inFirst, none, inSecond)
	}
	if both == 0 || none == 0 || folded[fixture.Project]+folded[fixture.SecondProject]-both != wide {
		t.Errorf("%s's %d deploys and %s's %d, less the %d that ship pull requests of both, are %d, want the deployment-wide %d",
			fixture.Project, folded[fixture.Project], fixture.SecondProject, folded[fixture.SecondProject], both,
			folded[fixture.Project]+folded[fixture.SecondProject]-both, wide)
	}

	// Run target itself: one item of each project's delivery family, on its start day. Its
	// repository's own item of it, where it ships a pull request no issue attributes, is under
	// that repository's owner key (evals.OwnerKey), which names no project.
	rows, err := database.Pool.Query(ctx, `
		select project_key || ' ' || day::text from eval_day_members
		where family = $1 and row_key = $2 and not starts_with(project_key, ':') order by project_key
	`, evals.KindDelivery, evals.RunMember(fixture.DeployRepo, target))
	if err != nil {
		t.Fatalf("read the items of run %d: %v", target, err)
	}
	counted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the items of run %d: %v", target, err)
	}
	day := startedAt[target].UTC().Format(time.DateOnly)
	if want := []string{fixture.Project + " " + day, fixture.SecondProject + " " + day}; !slices.Equal(counted, want) {
		t.Errorf("run %d is counted in %v, want once in each of %v", target, counted, want)
	}
	t.Logf("%s..%s: run %d ships pull requests of %s and %s; the route counts %d deploys, %s's day rows %d and %s's %d, %d deploys of the window shipping both and %d none of a project",
		from.Format(time.DateOnly), to.Format(time.DateOnly), target, fixture.Project, fixture.SecondProject, wide,
		fixture.Project, folded[fixture.Project], fixture.SecondProject, folded[fixture.SecondProject], both, none)
}
