package evalfixture_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents/signals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// failuresTables are the tables the Failures mode writes, each digested as its rows with the
// moment of a write that changes no field (a settings save's updated_at) left out.
var failuresTables = []string{
	"projects", "issues", "events", "artifacts", "issue_external_links", "check_results",
	"project_delivery_settings", "project_deploy_sources", "delivery_pull_requests", "delivery_runs",
	"delivery_run_jobs", "delivery_reverts", "production_events",
}

// failuresDigests is each of failuresTables as one digest of its rows.
func failuresDigests(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for _, table := range failuresTables {
		var digest *string
		if err := database.Pool.QueryRow(t.Context(), fmt.Sprintf(`
			select md5(string_agg(row, ',' order by row))
			from (select (to_jsonb(t) - array['updated_at', 'recorded_at'])::text as row from %s t) rows`, table)).Scan(&digest); err != nil {
			t.Fatalf("digest %s: %v", table, err)
		}
		if digest == nil {
			t.Fatalf("the failures mode wrote no row of %s", table)
		}
		digests[table] = *digest
	}
	return digests
}

// near reports whether a measure's value is the value the result states, nil for "no data".
func near(got, want *float64) bool {
	return (got == nil) == (want == nil) && (got == nil || math.Abs(*got-*want) < 1e-9)
}

// show is a value as a failure message names it.
func show(value *float64) string {
	if value == nil {
		return "no data"
	}
	return fmt.Sprint(*value)
}

// serverSignals runs the production signals' loop beside the loads as a running server runs it
// (cmd/dispatch's signals.Run): under the production-signals lease, woken through a pglisten.Hub by
// the commit of each event, delivery change, settings save and revert, so it reads each input a
// load writes as soon as that input commits, before the load's later writes, as it does where a
// check loads -failures into a running dispatch:local. It returns once the hub listens, and answers
// the stop that ends the loop and the hub and waits for both, which the test's cleanup calls too.
func serverSignals(t *testing.T, pool *store.Pool) (stop func()) {
	t.Helper()
	callers, err := pool.Leases()
	if err != nil {
		t.Fatalf("the lease pool: %v", err)
	}
	leases, err := callers.For("evalfixture test server's production signals")
	if err != nil {
		t.Fatal(err)
	}
	hub := &pglisten.Hub{Owner: "evalfixture test server's production signals", Dial: pool.Listener}
	deps := signals.Deps{
		Pool: pool, Horizon: events.NewHorizon(pool), Leases: leases,
		Events: hub.Wake(api.EventsChannel), Deliveries: hub.Wake(delivery.ChangesChannel),
		Settings: hub.Wake(delivery.SettingsChannel), Reverts: hub.Wake(signals.RevertsChannel),
	}
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { hub.Run(ctx) })
	running.Go(func() { signals.Run(ctx, deps) })
	stop = sync.OnceFunc(func() {
		cancel()
		running.Wait()
	})
	t.Cleanup(stop)
	testwait.Receive(t, "the server's signals hub to listen", connected)
	return stop
}

// TestTheFailuresModeLoadsTwiceIntoTheSameRows: -failures (dispatch://INFRA-978) loaded twice
// with one -now, beside a server's production signals' loop that reads each input as it commits
// (serverSignals), writes the same rows of every table it writes and answers the same result, so a
// check can load it again into a running server's database. What it prints is what the rows hold:
// one failure row of each of the five signals, counted on arrival and naming the deploy it blames,
// the revert restored by its own deploy; no row from the passing check, the commit no deploy holds
// or the bug issue; and a deploy row of each run a source's rule calls a deploy and none of the run
// whose apply step failed or of the repository no source reads. The evals the refresher then
// builds answer each project's three failure measures and its deploy frequency over the window as
// the result states them, and each deploy counts in deploy frequency for the projects the result
// names, through what it names (R12). The pull requests and CI jobs among k12's rows count by
// INFRA-2916's attribution (R15), which TestTheAttributionModeWritesK15sCase holds; the test logs
// what the evals count of them beside what the result places, since a pull request or a job with
// no issue counts through a default repository only on a head whose attribution reads
// default_repo.
func TestTheFailuresModeLoadsTwiceIntoTheSameRows(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	stopServer := serverSignals(t, database.Pool)
	first, err := evalfixture.WriteFailures(ctx, database.Pool, "FAIL", now)
	if err != nil {
		t.Fatalf("load the failures mode: %v", err)
	}
	loaded := failuresDigests(t, database)
	second, err := evalfixture.WriteFailures(ctx, database.Pool, "FAIL", now)
	if err != nil {
		t.Fatalf("load the failures mode again: %v", err)
	}
	// The server's loop stops before the second digest, so whatever it wrote of the second load's
	// inputs is in it.
	stopServer()
	again := failuresDigests(t, database)
	for _, table := range failuresTables {
		if loaded[table] != again[table] {
			t.Errorf("the second load moved %s (%s, then %s)", table, loaded[table], again[table])
		}
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("the second load answered another result:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
	t.Logf("%s", firstJSON)

	// The rows each check counts.
	for _, deploy := range first.Deploys {
		if n := count(t, database, `select count(*) from production_events where id = $1 and kind = 'deploy' and project_key = $2 and source_ref = $3`,
			deploy.ID, deploy.ProjectKey, deploy.SourceRef); n != 1 {
			t.Errorf("deploy %s (%s of %s) is %d rows, want one", deploy.Name, deploy.SourceRef, deploy.ProjectKey, n)
		}
	}
	for _, run := range first.NoDeploys {
		if n := count(t, database, `select count(*) from production_events where source_ref = $1`, prodevents.GitHubSourceRef(run.RunID)); n != 0 {
			t.Errorf("run %s/%d, which %s, made %d deploy rows, want none", run.Repo, run.RunID, run.Why, n)
		}
	}
	deploys := count(t, database, `select count(*) from production_events where kind = 'deploy' and project_key = any($1)`, first.Projects)
	failures := count(t, database, `select count(*) from production_events where kind = 'failure' and project_key = any($1)`, first.Projects)
	if deploys != len(first.Deploys) || failures != first.Expects.Signals || first.Expects.Signals != 5 {
		t.Errorf("the record holds %d deploys and %d failures; the result names %d and %d, want 5 failures", deploys, failures, len(first.Deploys), first.Expects.Signals)
	}
	signals := map[string]bool{}
	for _, failure := range first.Failures {
		signals[failure.Signal] = true
		row, err := prodevents.Get(ctx, database.Pool, failure.ProjectKey, failure.ID)
		if err != nil {
			t.Fatalf("read the %s failure %s: %v", failure.Signal, failure.ID, err)
		}
		deploy := slices.IndexFunc(first.Deploys, func(d evalfixture.FailureDeploy) bool { return d.Name == failure.Deploy })
		if row.State != prodevents.StateCounted || row.Signal == nil || *row.Signal != failure.Signal || deploy < 0 ||
			row.DeployID == nil || *row.DeployID != first.Deploys[deploy].ID {
			t.Errorf("the %s failure %s is %+v, want it counted and blaming %s", failure.Signal, failure.SourceRef, row, failure.Deploy)
		}
		if failure.Signal == prodevents.SignalRevert && (failure.RestoredBy == nil || *failure.RestoredBy != "R7" || row.RestoredAt == nil) {
			t.Errorf("the revert is restored by %v at %v, want by R7, the deploy that ships it", failure.RestoredBy, row.RestoredAt)
		}
	}
	if len(signals) != 5 || first.Expects.CountedOnArrival != 5 {
		t.Errorf("the failures are of the signals %v, %d counted on arrival; want each of the five, every one counted", signals, first.Expects.CountedOnArrival)
	}
	// Each negative is stored, and the record holds the five failures alone, so none made a row.
	for _, negative := range first.Negatives {
		held := count(t, database, `select count(*) from check_results where id::text = $1`, negative.Ref) +
			count(t, database, `select count(*) from issues where key = $1 and 'bug' = any(labels)`, negative.Ref)
		if held != 1 {
			t.Errorf("%s (%s) is %d stored inputs, want one", negative.What, negative.Ref, held)
		}
	}
	if first.Expects.SourcesSaved["FAIL"] != 2 || first.Expects.DeploysByRepository[first.SecondRepo] == 0 ||
		first.Expects.DeploysByRepository[first.ThirdRepo] != 0 || first.Expects.RowsPlaced < 8 {
		t.Errorf("k2 and k12 expect %+v, want two sources of FAIL, a deploy of its second repository, none of the third, and eight rows at least", first.Expects)
	}

	// The evals the refresher builds from the rows, read as the eval routes fold them.
	registry := refresh(t, database)
	deployRepos, err := evals.DeployReposIn(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	window := evals.Range{From: first.Window.From, To: first.Window.To}
	series := map[string]string{
		evals.SlugPROpenToMerge: "minutes", evals.SlugDeployFrequency: "deploys", evals.SlugPRCI: "minutes",
		evals.SlugChangeFailRate: "per_deploy", evals.SlugFailedDeployRecovery: "minutes", evals.SlugDeploymentReworkRate: "deploys",
	}
	type tally struct{ n, via int }
	placed := map[string]map[string]tally{}
	for _, row := range first.Rows {
		eval := evals.SlugPRCI
		switch {
		case strings.HasPrefix(row.Row, "pull request"):
			eval = evals.SlugPROpenToMerge
		case strings.HasPrefix(row.Row, "deploy"):
			eval = evals.SlugDeployFrequency
		}
		for index, project := range row.CountsFor {
			if placed[project] == nil {
				placed[project] = map[string]tally{}
			}
			held := placed[project][eval]
			held.n++
			if row.ViaRepository[index] {
				held.via++
			}
			placed[project][eval] = held
		}
	}
	for _, project := range first.Projects {
		page, err := registry.Current(ctx, []string{project}, "", 200, 0)
		if err != nil {
			t.Fatalf("read %s's evals: %v", project, err)
		}
		var entries []evals.Entry
		for _, entry := range page.Items {
			if _, held := series[entry.Definition.Slug]; held {
				entries = append(entries, entry)
			}
		}
		if len(entries) != len(series) {
			t.Fatalf("%s holds %d of the %d evals the test reads", project, len(entries), len(series))
		}
		firsts, err := evals.FirstDaysIn(ctx, database.Pool, entries, deployRepos)
		if err != nil {
			t.Fatalf("read the first days of %s's evals: %v", project, err)
		}
		rows, err := evals.DayWindowIn(ctx, database.Pool, []string{project}, entries, deployRepos, nil, window, nil)
		if err != nil {
			t.Fatalf("read %s's day rows: %v", project, err)
		}
		values := map[string]evals.Statistics{}
		got := map[string]tally{}
		for _, entry := range entries {
			slug := entry.Definition.Slug
			own := slices.DeleteFunc(slices.Clone(rows), func(row evals.WindowRow) bool { return row.EvalID != entry.Definition.EvalID })
			values[slug] = evals.SeriesValuesOf(entry, own, firsts[entry.Definition.EvalID], window, nil)[0].Window
			for _, row := range own {
				if row.Part.Series == series[slug] {
					got[slug] = tally{n: got[slug].n + row.Part.N, via: got[slug].via + row.Part.ViaRepository}
				}
			}
		}
		want := first.Expects.Measures[project]
		whole := func(n int) *float64 { return new(float64(n)) }
		for _, stated := range []struct {
			name      string
			got, want *float64
		}{
			{"change-fail-rate.failed", values[evals.SlugChangeFailRate]["failed"], whole(want.Failed)},
			{"change-fail-rate.total", values[evals.SlugChangeFailRate]["total"], whole(want.Deploys)},
			{"change-fail-rate.rate", values[evals.SlugChangeFailRate]["rate"], want.ChangeFailRate},
			{"failed-deploy-recovery.n", values[evals.SlugFailedDeployRecovery]["n"], whole(want.Restored)},
			{"failed-deploy-recovery.median", values[evals.SlugFailedDeployRecovery]["median"], want.RecoveryMedian},
			{"failed-deploy-recovery.not_restored", values[evals.SlugFailedDeployRecovery]["not_restored"], whole(want.NotRestored)},
			{"deployment-rework-rate.restoring", values[evals.SlugDeploymentReworkRate]["restoring"], whole(want.Restoring)},
			{"deployment-rework-rate.total", values[evals.SlugDeploymentReworkRate]["total"], whole(want.Deploys)},
			{"deployment-rework-rate.rate", values[evals.SlugDeploymentReworkRate]["rate"], want.ReworkRate},
			{"deploy-frequency.deploys", values[evals.SlugDeployFrequency]["deploys"], whole(want.Deploys)},
		} {
			if !near(stated.got, stated.want) {
				t.Errorf("%s's %s over %s..%s is %s, the result states %s", project, stated.name, first.Window.From.Format("2006-01-02"),
					first.Window.To.Format("2006-01-02"), show(stated.got), show(stated.want))
			}
		}
		if deploys := got[evals.SlugDeployFrequency]; deploys != placed[project][evals.SlugDeployFrequency] || deploys.n == 0 {
			t.Errorf("%s's deploy-frequency.deploys counts %d deploys, %d through a repository; the result places %d and %d",
				project, deploys.n, deploys.via, placed[project][evals.SlugDeployFrequency].n, placed[project][evals.SlugDeployFrequency].via)
		}
		for _, slug := range []string{evals.SlugPROpenToMerge, evals.SlugPRCI} {
			t.Logf("%s's %s.%s counts %d rows, %d through a default repository; R15 places %d and %d", project, slug, series[slug],
				got[slug].n, got[slug].via, placed[project][slug].n, placed[project][slug].via)
		}
		stated, _ := json.Marshal(want)
		t.Logf("%s over the window: %s; day rows %+v", project, stated, got)
	}
}
