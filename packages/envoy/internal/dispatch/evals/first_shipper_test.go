package evals

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// nineDays is one day's reads of acme/omega-app and of OMEGA's events repository, each holding
// acme/omega-app#9 of OMEGA, merged at 23:30 of sourcesDay, as the day builders build them, each
// from its own repository's deploys alone: the GitHub day shipped by run 3002 at run, the events
// day by a post that finished at post; each repository's history starting on its since.
func nineDays(run, post, githubSince, eventsSince time.Time) []DeliveryDay {
	merged := sourcesDay.Add(23*time.Hour + 30*time.Minute)
	nine := func(deployed time.Time, deploy string) dayPR {
		return dayPR{
			pr:   measures.PR{ID: omegaApp + "#9", CreatedAt: merged.Add(-time.Hour), MergedAt: merged, DeployedAt: &deployed},
			repo: omegaApp, member: PullRequestMember(omegaApp, 9), deploy: &dayItem{member: deploy, day: measures.Midnight(deployed)},
			counted: counted{project: "OMEGA"},
		}
	}
	return []DeliveryDay{
		{Day: sourcesDay, deployRepo: omegaApp, since: githubSince, prs: []dayPR{nine(run, RunMember(omegaApp, 3002))}},
		{Day: sourcesDay, deployRepo: EventsRepo("OMEGA"), since: eventsSince, posted: true, prs: []dayPR{nine(post, PostedMember("post-9"))}},
	}
}

// TestTheReadCountsAPullRequestAtTheFirstDeployOfTheRowsThatHoldItsDay
// (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8): acme/omega-app#9 of OMEGA
// merges at 23:30, acme/omega-app's run 3002 ships it at 01:00 the next day, and a deploy posted to
// OMEGA names it. Each repository's merge-to-production row of the day times #9 by its own deploy,
// and names it as each sample's key; a read of both rows (shippedOnce) keeps the first deploy's
// sample alone: the post's 60 minutes where it finished at 00:30, the run's 90 where the post
// finished at 01:30, and the run's where both finished at 01:00. A row of a day before its
// repository's history is none: where the events repository's history starts the next day (its
// first post finished then), the run's 90 minutes count; where acme/omega-app's does, the post's
// 120. Red where the GitHub row names no keys, or where a day before its repository's history holds
// a row: #9 counts twice, or nowhere.
func TestTheReadCountsAPullRequestAtTheFirstDeployOfTheRowsThatHoldItsDay(t *testing.T) {
	t.Parallel()
	next := sourcesDay.AddDate(0, 0, 1)
	at := func(hour, minute int) time.Time {
		return next.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	events := EventsRepo("OMEGA")
	for _, row := range []struct {
		name                     string
		post                     time.Time
		githubSince, eventsSince time.Time
		// minutes is, by deploy repository, the samples the read keeps of each merge-to-production
		// row of the day.
		minutes map[string][]float64
	}{
		{"both hold the day, the post first", at(0, 30), sourcesDay, sourcesDay, map[string][]float64{omegaApp: {}, events: {60}}},
		{"both hold the day, the run first", at(1, 30), sourcesDay, sourcesDay, map[string][]float64{omegaApp: {90}, events: {}}},
		{"both hold the day, the post and the run at once", at(1, 0), sourcesDay, sourcesDay, map[string][]float64{omegaApp: {90}, events: {}}},
		{"the events repository's history starts the next day", at(0, 30), sourcesDay, next, map[string][]float64{omegaApp: {90}}},
		{"acme/omega-app's history starts the next day", at(1, 30), next, sourcesDay, map[string][]float64{events: {120}}},
	} {
		var rows []WindowRow
		for _, day := range nineDays(at(1, 0), row.post, row.githubSince, row.eventsSince) {
			for _, built := range day.Rows("OMEGA").Rows {
				if built.EvalID == EvalID("OMEGA", SlugMergeToProduction) && built.Repo == "" && built.Series == measures.SeriesMinutes {
					rows = append(rows, WindowRow{Range: RangeWindow, EvalID: built.EvalID, Part: built.DayPart, owner: built.ProjectKey,
						repo: built.Repo, deployRepo: built.DeployRepo, sampleKeys: built.sampleKeys})
				}
			}
		}
		minutes := map[string][]float64{}
		for index, part := range shippedOnce(rows) {
			minutes[rows[index].deployRepo] = append(minutes[rows[index].deployRepo], part.Samples...)
		}
		if !maps.EqualFunc(minutes, row.minutes, func(a, b []float64) bool { return slices.Equal(a, b) }) {
			t.Errorf("%s: the read keeps %v of OMEGA's merge-to-production rows, want %v", row.name, minutes, row.minutes)
		}
	}
}

// mergeNine writes acme/omega-app#9 of OMEGA-1, first committed two hours and opened an hour before
// merged, and merged at merged.
func (r *refresherRig) mergeNine(t *testing.T, merged time.Time) {
	t.Helper()
	if err := delivery.UpsertPullRequest(t.Context(), r.database.Pool, delivery.DeliveryPullRequest{
		Repo: omegaApp, Number: 9, Title: "OMEGA-1: change 9", URL: "https://github.com/" + omegaApp + "/pull/9", Author: "dev-ana",
		FirstCommitAt: new(merged.Add(-2 * time.Hour)), CreatedAt: new(merged.Add(-time.Hour)), MergedAt: new(merged),
		IssueKey: new("OMEGA-1"), Sessions: []string{},
	}); err != nil {
		t.Fatalf("write %s#9: %v", omegaApp, err)
	}
}

// putOmega saves OMEGA's sources: its GitHub source, which reads its default repository
// acme/omega-app under omegaJob, where github is set, and an events source where events is.
func (r *refresherRig) putOmega(t *testing.T, github, events bool) {
	t.Helper()
	var sources []delivery.SourceInput
	if github {
		sources = append(sources, deliveryseed.SourceOf(ruleOfJob(omegaJob), nil))
	}
	if events {
		sources = append(sources, eventsSource)
	}
	putSources(t, r.database.Pool, "OMEGA", []string{"dev-ana"}, sources...)
}

// leadValues is project's slug over window as an eval read states it, its minutes series: its
// stored rows of the deploy repositories recorded with them folded with its series' first day
// (DeployReposIn, FirstDaysIn, DayWindowIn, SeriesValuesOf), as mergeValues reads
// merge-to-production.
func (r *refresherRig) leadValues(t *testing.T, project, slug string, window Range) SeriesValues {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	page, err := r.registry.Current(ctx, []string{project}, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
	at := slices.IndexFunc(page.Items, func(entry Entry) bool { return entry.Definition.Slug == slug })
	if at < 0 {
		t.Fatalf("%s holds no %s", project, slug)
	}
	entry := page.Items[at]
	deployRepos, err := DeployReposIn(ctx, pool)
	if err != nil {
		t.Fatalf("read the recorded deploy repositories: %v", err)
	}
	firsts, err := FirstDaysIn(ctx, pool, []Entry{entry}, deployRepos)
	if err != nil {
		t.Fatalf("read the first days of %s: %v", entry.Definition.EvalID, err)
	}
	rows, err := DayWindowIn(ctx, pool, []string{project}, []Entry{entry}, deployRepos, nil, window, nil)
	if err != nil {
		t.Fatalf("read the day rows of %s: %v", entry.Definition.EvalID, err)
	}
	for _, values := range SeriesValuesOf(entry, rows, firsts[entry.Definition.EvalID], window, nil) {
		if values.Series.Series == measures.SeriesMinutes {
			return values
		}
	}
	t.Fatalf("%s states no %s series", entry.Definition.EvalID, measures.SeriesMinutes)
	return SeriesValues{}
}

// countOf is the n values state, 0 where they state none ("no data").
func countOf(values SeriesValues) int {
	if n := values.Window["n"]; n != nil {
		return int(*n)
	}
	return 0
}

// shippers is, by deploy repository, the deploy each of project's members of the pull request
// member on day names as the one that ships it (eval_day_members.shipped_by), of its members under
// the deploy repositories the project's reads read (DeployReposIn), as the items route reads them
// (CountedIn): the deploys it times the pull request to, merge-to-production to the first of them,
// the other two lead times to the first GitHub deploy. Every repository's day holds each pull
// request of the day, unshipped where it is not that repository's (attribution.dayAs), so a member
// that names no deploy is left out: it times the pull request in no eval.
func (r *refresherRig) shippers(t *testing.T, project, member string, day time.Time) map[string]string {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	deployRepos, err := DeployReposIn(ctx, pool)
	if err != nil {
		t.Fatalf("read the recorded deploy repositories: %v", err)
	}
	rows, err := pool.Query(ctx, `
		select deploy_repo, shipped_by from eval_day_members
		where project_key = $1 and family = $2 and row_key = $3 and day = $4 and deploy_repo = any($5) and shipped_by <> ''
	`, project, KindDelivery, member, day, deployRepos.Of(project))
	if err != nil {
		t.Fatalf("read %s's members of %s: %v", project, member, err)
	}
	held := map[string]string{}
	var deployRepo, shippedBy string
	if _, err := pgx.ForEachRow(rows, []any{&deployRepo, &shippedBy}, func() error {
		held[deployRepo] = shippedBy
		return nil
	}); err != nil {
		t.Fatalf("read %s's members of %s: %v", project, member, err)
	}
	return held
}

// expectNineOnce fails unless OMEGA's merge-to-production over day counts acme/omega-app#9 once,
// at minutes from its merge (n 1 and max minutes), and its members of #9 under the deploy
// repositories it reads name the deploys of shippers, by deploy repository, the first of which is
// the one minutes is timed to, so its merge-to-production items list #9 once at the minutes its
// number counts; unless its first-commit-to-production and open-to-production count #9 once where
// a GitHub repository's member names a deploy of it and not at all where none does, as their items
// list it (an events repository's member times it in merge-to-production alone); unless no day is
// left owed a rewrite; and unless the stored figures are the fold of the rows.
func (r *refresherRig) expectNineOnce(t *testing.T, what string, day time.Time, minutes float64, shippers map[string]string) {
	t.Helper()
	window := Range{From: day, To: day.AddDate(0, 0, 1)}
	wantFigures(t, what, map[string]SeriesValues{SlugMergeToProduction: r.leadValues(t, "OMEGA", SlugMergeToProduction, window)}, map[string]*float64{
		SlugMergeToProduction + ".n": new(1.0), SlugMergeToProduction + ".max": new(minutes),
	})
	got := r.shippers(t, "OMEGA", PullRequestMember(omegaApp, 9), day)
	if !maps.Equal(got, shippers) {
		t.Errorf("%s: OMEGA's members of %s#9 on %s name %v, want %v", what, omegaApp, day.Format(time.DateOnly), got, shippers)
	}
	listed := 0
	for deployRepo := range got {
		if !IsEventsRepo(deployRepo) {
			listed = 1
		}
	}
	for _, slug := range []string{SlugFirstCommitToProduction, SlugOpenToProduction} {
		if n := countOf(r.leadValues(t, "OMEGA", slug, window)); n != listed {
			t.Errorf("%s: OMEGA's %s counts %d pull requests on %s, want %d, as its items list", what, slug, n, day.Format(time.DateOnly), listed)
		}
	}
	if n := r.count(t, `select count(*) from eval_days_owed`); n != 0 {
		t.Errorf("%s: %d days are owed a rewrite after the passes, want none", what, n)
	}
	expectTruth(t, what, r.storedFigures(t), r.truthFigures(t))
}

// TestAPullRequestMergedBeforeARepositorysHistoryCountsOnceAtTheOtherRepositorysDeploy
// (dispatch://INFRA-978, R12): over the three-source history, acme/omega-app#9 of OMEGA-1 merges at
// 23:30 of the day before sourcesDay, and a GitHub deploy of acme/omega-app and a deploy posted to
// OMEGA both ship it on sourcesDay, the repository of the one that finished first holding no history
// of the merge day. Where OMEGA's first post, at 00:30, starts its events repository on sourcesDay
// and acme/omega-app's history holds the merge day (run 3000), run 3002's 01:00 times #9, 90
// minutes, in all three lead times. Where acme/omega-app's first run, 3002 at 01:00, starts its
// history on sourcesDay and OMEGA's events repository's holds the merge day (a post naming none),
// the post at 01:30 times #9, 120 minutes, in merge-to-production alone. In each, OMEGA's
// merge-to-production counts #9 once and its members of it name that deploy alone. Red where the
// repository whose history starts after the merge day takes the first shipper: #9 counts nowhere,
// while the other repository's member still lists it.
func TestAPullRequestMergedBeforeARepositorysHistoryCountsOnceAtTheOtherRepositorysDeploy(t *testing.T) {
	t.Parallel()
	mergeDay := sourcesDay.AddDate(0, 0, -1)
	at := func(day time.Time, hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	for _, row := range []struct {
		name string
		// seed writes the row's deploys beside #9 and answers OMEGA's members of #9 as shippers
		// holds them.
		seed    func(t *testing.T, r *refresherRig) map[string]string
		minutes float64
	}{
		{"the events repository's history starts after the merge day", func(t *testing.T, r *refresherRig) map[string]string {
			r.deployRun(t, omegaApp, 3000, at(mergeDay, 1, 0), omegaJob)
			r.post(t, "OMEGA", "release-9", at(sourcesDay, 0, 30), omegaApp+"#9")
			r.deployRun(t, omegaApp, 3002, at(sourcesDay, 0, 41), omegaJob)
			return map[string]string{omegaApp: RunMember(omegaApp, 3002)}
		}, 90},
		{"acme/omega-app's history starts after the merge day", func(t *testing.T, r *refresherRig) map[string]string {
			r.post(t, "OMEGA", "release-0", at(mergeDay, 1, 0))
			r.deployRun(t, omegaApp, 3002, at(sourcesDay, 0, 41), omegaJob)
			posted := r.post(t, "OMEGA", "release-9", at(sourcesDay, 1, 30), omegaApp+"#9")
			return map[string]string{EventsRepo("OMEGA"): PostedMember(posted.ID)}
		}, 120},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRefresherRig(t)
			r.seedSources(t)
			r.mergeNine(t, at(mergeDay, 23, 30))
			shippers := row.seed(t, r)
			r.settle(t)
			r.expectNineOnce(t, row.name, mergeDay, row.minutes, shippers)
		})
	}
}

// TestAnEventsSourceAddedAfterItsPullRequestsMergeDayLeavesThatDayTimedToTheGitHubDeploy
// (dispatch://INFRA-978, R12): over the three-source history, OMEGA reads acme/omega-app alone,
// whose run 3000 starts its history the day before sourcesDay; acme/omega-app#9 of OMEGA-1 merges
// at 23:30 that day, a deploy posted to OMEGA names it and finishes at 00:30 of sourcesDay, OMEGA's
// first post, and run 3002 ships it at 01:00, 90 minutes. Once OMEGA saves an events source, its
// events repository starts on sourcesDay, so the merge day keeps #9 at run 3002; and so it does once
// a second post naming #9 rewrites that day whole. Red where the events repository's day before its
// history takes the first shipper when the day is written again: #9 counts nowhere.
func TestAnEventsSourceAddedAfterItsPullRequestsMergeDayLeavesThatDayTimedToTheGitHubDeploy(t *testing.T) {
	t.Parallel()
	mergeDay := sourcesDay.AddDate(0, 0, -1)
	r := newRefresherRig(t)
	r.seedSources(t)
	r.putOmega(t, true, false)
	r.deployRun(t, omegaApp, 3000, mergeDay.Add(time.Hour), omegaJob)
	r.mergeNine(t, mergeDay.Add(23*time.Hour+30*time.Minute))
	r.post(t, "OMEGA", "release-9", sourcesDay.Add(30*time.Minute), omegaApp+"#9")
	r.deployRun(t, omegaApp, 3002, sourcesDay.Add(41*time.Minute), omegaJob)
	shippers := map[string]string{omegaApp: RunMember(omegaApp, 3002)}
	r.settle(t)
	r.expectNineOnce(t, "before the events source", mergeDay, 90, shippers)

	r.putOmega(t, true, true)
	r.settle(t)
	r.expectNineOnce(t, "once the events source is saved", mergeDay, 90, shippers)

	r.post(t, "OMEGA", "release-9b", sourcesDay.Add(5*time.Hour), omegaApp+"#9")
	_, report := r.settle(t)
	if !report.days[builtDay{family: walkDelivery, project: "OMEGA", day: mergeDay}] {
		t.Errorf("the passes after the second post built %v, want OMEGA's day %s, #9's merge day, among them", report.sorted(), mergeDay.Format(time.DateOnly))
	}
	r.expectNineOnce(t, "once a second post rewrote the merge day", mergeDay, 90, shippers)
}

// TestAnEventsSourceComingAndGoingCountsAPullRequestOnce (dispatch://INFRA-978, R12): over the
// three-source history, acme/omega-app#9 of OMEGA-1 merges at 09:00 of sourcesDay, a deploy posted
// to OMEGA names it and finishes at 09:30, and OMEGA's GitHub deploy 3001 ships it at 10:19. While
// OMEGA has an events source, its merge-to-production counts #9 once at the post, 30 minutes, and
// its members of #9 name 3001 under acme/omega-app and the post under its events repository; while
// it has none, once at 3001, 79 minutes, and its member names 3001 alone; its other two lead times
// count #9 at 3001 throughout. So it stays as OMEGA saves the events source and then removes it,
// and as it removes it and then saves it again, each after the passes that follow it, which leave no
// day owed. Red where adding the events source leaves acme/omega-app's row timing #9 (n 2), or
// removing it leaves that row keeping #9 for the post that no row times any more (n 0).
func TestAnEventsSourceComingAndGoingCountsAPullRequestOnce(t *testing.T) {
	t.Parallel()
	for _, first := range []bool{false, true} {
		t.Run(fmt.Sprintf("events source at first %t", first), func(t *testing.T) {
			t.Parallel()
			r := newRefresherRig(t)
			r.seedSources(t)
			r.putOmega(t, true, first)
			r.mergeNine(t, sourcesDay.Add(9*time.Hour))
			posted := r.post(t, "OMEGA", "release-9", sourcesDay.Add(9*time.Hour+30*time.Minute), omegaApp+"#9")
			expect := func(what string, events bool) {
				t.Helper()
				minutes, shippers := 79.0, map[string]string{omegaApp: RunMember(omegaApp, 3001)}
				if events {
					minutes, shippers[EventsRepo("OMEGA")] = 30, PostedMember(posted.ID)
				}
				r.expectNineOnce(t, what, sourcesDay, minutes, shippers)
			}
			r.settle(t)
			expect(fmt.Sprintf("with the events source %t", first), first)
			r.putOmega(t, true, !first)
			r.settle(t)
			expect(fmt.Sprintf("once the events source is %t", !first), !first)
			r.putOmega(t, true, first)
			r.settle(t)
			expect(fmt.Sprintf("once the events source is %t again", first), first)
		})
	}
}

// TestAPullRequestCountsOnceAtThePostOnceItsGitHubSourceIsRemoved
// (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8): over the three-source
// history, acme/omega-app#9 of OMEGA-1 merges at 09:00 of sourcesDay, OMEGA's GitHub deploy 3001
// ships it at 10:19, and a deploy posted to OMEGA names it and finishes at 11:00. OMEGA's
// merge-to-production counts #9 once at 3001, 79 minutes, and its other two lead times at 3001
// too. Once OMEGA removes its GitHub source and no source reads acme/omega-app any more, OMEGA reads
// none of that repository's rows, and #9 counts once at the post, 120 minutes, in
// merge-to-production alone: its member under the events repository is the one OMEGA reads. Red
// where the events repository's row left #9 out for the GitHub deploy: #9 counts nowhere.
func TestAPullRequestCountsOnceAtThePostOnceItsGitHubSourceIsRemoved(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedSources(t)
	r.mergeNine(t, sourcesDay.Add(9*time.Hour))
	posted := r.post(t, "OMEGA", "release-9", sourcesDay.Add(11*time.Hour), omegaApp+"#9")
	r.settle(t)
	r.expectNineOnce(t, "with the GitHub source", sourcesDay, 79,
		map[string]string{omegaApp: RunMember(omegaApp, 3001), EventsRepo("OMEGA"): PostedMember(posted.ID)})
	r.putOmega(t, false, true)
	r.settle(t)
	r.expectNineOnce(t, "once the GitHub source is removed", sourcesDay, 120, map[string]string{EventsRepo("OMEGA"): PostedMember(posted.ID)})
}

// TestAPullRequestCountsOnceWhileItsRepositorysSourceMovesToAnotherProjectAndBack
// (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8): over the three-source
// history, acme/omega-app#9 of OMEGA-1 merges at 09:00 of sourcesDay, OMEGA's GitHub deploy 3001
// ships it at 10:19, and a deploy posted to OMEGA names it and finishes at 11:00. OMEGA's
// merge-to-production counts #9 once at 3001, 79 minutes, its other two lead times at 3001 too,
// and ALPHA holds no member of it. Once acme/omega-app's source moves to ALPHA, OMEGA still reads
// the repository, which a source reads, and #9 still counts for OMEGA, its issue's project, once at
// 3001, read through ALPHA's rule; ALPHA still counts none of it. So it stays once the source moves
// back.
func TestAPullRequestCountsOnceWhileItsRepositorysSourceMovesToAnotherProjectAndBack(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedSources(t)
	r.mergeNine(t, sourcesDay.Add(9*time.Hour))
	posted := r.post(t, "OMEGA", "release-9", sourcesDay.Add(11*time.Hour), omegaApp+"#9")
	shippers := map[string]string{omegaApp: RunMember(omegaApp, 3001), EventsRepo("OMEGA"): PostedMember(posted.ID)}
	alpha := []delivery.SourceInput{deliveryseed.SourceOf(ruleOfJob(alphaJob), nil), deliveryseed.SourceOf(ruleOfJob(opsJob), new(alphaOps))}
	expect := func(what string) {
		t.Helper()
		r.expectNineOnce(t, what, sourcesDay, 79, shippers)
		if got := r.shippers(t, "ALPHA", PullRequestMember(omegaApp, 9), sourcesDay); len(got) != 0 {
			t.Errorf("%s: ALPHA's members of %s#9 name %v, want none", what, omegaApp, got)
		}
	}
	r.settle(t)
	expect("with OMEGA's source")
	r.putOmega(t, false, true)
	putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"}, append(slices.Clone(alpha), deliveryseed.SourceOf(ruleOfJob(omegaJob), new(omegaApp)))...)
	r.settle(t)
	expect("once the source moved to ALPHA")
	putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"}, alpha...)
	r.putOmega(t, true, true)
	r.settle(t)
	expect("once the source moved back to OMEGA")
}

// TestAPullRequestAGitHubDeployAndAPostShipAtOnceCountsAtTheGitHubDeploy
// (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8): over the three-source
// history, acme/omega-app#9 of OMEGA-1 merges at 09:00 of sourcesDay, and OMEGA's GitHub deploy
// 3001 and a deploy posted to OMEGA naming it both finish at 10:19. Each repository's row times #9
// at 79 minutes, and OMEGA's merge-to-production counts it once, at the GitHub deploy, which the
// tie goes to (Shipping.Before), as its items time it; its other two lead times count it at 3001.
func TestAPullRequestAGitHubDeployAndAPostShipAtOnceCountsAtTheGitHubDeploy(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedSources(t)
	r.mergeNine(t, sourcesDay.Add(9*time.Hour))
	posted := r.post(t, "OMEGA", "release-9", sourcesDay.Add(10*time.Hour+19*time.Minute), omegaApp+"#9")
	r.settle(t)
	r.expectNineOnce(t, "a tie", sourcesDay, 79, map[string]string{omegaApp: RunMember(omegaApp, 3001), EventsRepo("OMEGA"): PostedMember(posted.ID)})
	if n := r.count(t, `select count(*) from eval_days where eval_id = $1 and repo = '' and day = $2 and series = 'minutes' and samples = '{79}'`,
		EvalID("OMEGA", SlugMergeToProduction), sourcesDay); n != 2 {
		t.Errorf("%d of OMEGA's merge-to-production rows time #9 at 79 minutes, want acme/omega-app's and its events repository's", n)
	}
}
