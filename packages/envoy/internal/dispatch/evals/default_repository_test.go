package evals

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The attribution of what a repository holds to the projects whose default repository it is
// (dispatch://INFRA-2916, D1 and R15 after the person's answer A; plan T7): a pull request with no
// issue, a CI job of such a pull request or of none, and a deploy run that ships such a pull
// request or none count for each project whose default repository (projects.default_repo) is
// their repository; a pull request whose issue is held only through its project's default
// repository counts for that project while that default is its repository, a selection the
// published defaults make at read time; a deploy counts once in each project, through an issue
// where any of the pull requests it ships reached that project through one.

// defaultTally is what one eval's series counts for a project over a read: its rows' n and how many
// of them came through a default repository.
type defaultTally struct{ n, via int }

// readTallies reads, through the read the eval routes fold (DayWindowIn, over the deploy
// repository the rows stand recorded under), project's rows of each eval of series (by slug, the
// series to tally) on day, and answers each eval's tally and how many rows the read answered.
func (r *refresherRig) readTallies(t *testing.T, project string, series map[string]string, day time.Time) (map[string]defaultTally, int) {
	t.Helper()
	ctx := t.Context()
	page, err := r.registry.Current(ctx, []string{project}, "", 200, 0)
	if err != nil {
		t.Fatalf("read %s's evals: %v", project, err)
	}
	var entries []Entry
	for _, entry := range page.Items {
		if _, held := series[entry.Definition.Slug]; held {
			entries = append(entries, entry)
		}
	}
	if len(entries) != len(series) {
		t.Fatalf("%s holds %d of the %d evals the read tallies", project, len(entries), len(series))
	}
	deployRepo, err := DeployRepoIn(ctx, r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := DayWindowIn(ctx, r.database.Pool, []string{project}, entries, deployRepo, nil, Range{From: day, To: day.AddDate(0, 0, 1)}, nil)
	if err != nil {
		t.Fatalf("read %s's day rows of %s: %v", project, day.Format(time.DateOnly), err)
	}
	tallies, held := map[string]defaultTally{}, 0
	for _, row := range rows {
		slug := slugOf(project, row.EvalID)
		if row.Part.Series != series[slug] {
			continue
		}
		held++
		tallies[slug] = defaultTally{n: tallies[slug].n + row.Part.N, via: tallies[slug].via + row.Part.ViaRepository}
	}
	return tallies, held
}

// One day in acme/alpha, the deploy repository and the default repository of both ALPHA and BETA:
// deploy X ships PR1 (ALPHA-1's) and PR2 (no issue), deploy Y ships PR3 (no issue), and deploy Z
// ships only PR4 (ALPHA-1's). deploy-frequency counts X, Y and Z for ALPHA (X and Z through
// ALPHA-1, Y through the default repository) and X and Y for BETA (each through the default
// repository: X ships PR2, and BETA is no project of X's issues), each deploy once in each; and
// production-deploy counts each deploy's production job once for each project that counts its
// deploy. Red under the rule that counts a row with no issue for no project: ALPHA counts X and Z
// and BETA nothing.
func TestADeployCountsOnceInEachProjectWhoseIssueOrDefaultRepositoryItReaches(t *testing.T) {
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	day := refresherToday.AddDate(0, 0, -1)
	at := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	actor := model.Actor{Kind: "system", ID: "default-repository-test"}
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 2, $1), ('BETA', 'Beta', 'alice', 1, $1)`, alphaRepo)
	r.exec(t, "the issue", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', $1, '{"kind":"system","id":"default-repository-test"}', $2)`,
		alphaRepo, day.AddDate(0, 0, -1))
	production := "production / apply"
	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: alphaRepo, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{},
	}, actor); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	for _, pr := range []struct {
		number int
		issue  *string
		merged time.Time
	}{
		{1, new("ALPHA-1"), at(9, 0)},
		{2, nil, at(9, 10)},
		{3, nil, at(11, 0)},
		{4, new("ALPHA-1"), at(13, 0)},
	} {
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: alphaRepo, Number: pr.number, Title: fmt.Sprintf("Change %d", pr.number),
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", alphaRepo, pr.number), Author: "dev-ana",
			CreatedAt: new(pr.merged.Add(-time.Hour)), MergedAt: new(pr.merged), IssueKey: pr.issue, Sessions: []string{},
		}); err != nil {
			t.Fatalf("write %s#%d: %v", alphaRepo, pr.number, err)
		}
	}
	// X is created after PR1 and PR2 merge, Y after PR3, Z after PR4, each reaching production
	// before the next merge, so each ships the pull requests merged since the one before it.
	for _, deploy := range []struct {
		id      int64
		created time.Time
	}{{301, at(10, 0)}, {302, at(12, 0)}, {303, at(14, 0)}} {
		started, completed := deploy.created.Add(time.Minute), deploy.created.Add(20*time.Minute)
		if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
			Repo: alphaRepo, RunID: deploy.id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", deploy.id),
			HeadCommitAt: deploy.created.Add(-time.Minute), StartedAt: started, CreatedAt: new(deploy.created), CompletedAt: &completed,
			Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", alphaRepo, deploy.id),
			HeadBranch: new("main"), Event: new("push"),
		}); err != nil {
			t.Fatalf("write deploy run %d: %v", deploy.id, err)
		}
		job := delivery.DeliveryRunJob{Name: production, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess)}
		if err := delivery.UpsertRunJobs(ctx, pool, alphaRepo, deploy.id, []delivery.DeliveryRunJob{job}, true); err != nil {
			t.Fatalf("write the job of deploy run %d: %v", deploy.id, err)
		}
	}
	r.settle(t)

	series := map[string]string{SlugDeployFrequency: "deploys", SlugProductionDeploy: "minutes"}
	want := map[string]map[string]defaultTally{
		"ALPHA": {SlugDeployFrequency: {n: 3, via: 1}, SlugProductionDeploy: {n: 3, via: 1}},
		"BETA":  {SlugDeployFrequency: {n: 2, via: 2}, SlugProductionDeploy: {n: 2, via: 2}},
	}
	for _, project := range []string{"ALPHA", "BETA"} {
		got, rows := r.readTallies(t, project, series, day)
		t.Logf("%s reads %d day rows of %s: %v", project, rows, day.Format(time.DateOnly), got)
		for slug, name := range series {
			if got[slug] != want[project][slug] {
				t.Errorf("%s's %s.%s counts %d, %d through a default repository; want %d and %d",
					project, slug, name, got[slug].n, got[slug].via, want[project][slug].n, want[project][slug].via)
			}
		}
	}
}

// pierRepo and quayRepo are the default repositories of PIER and QUAY (seedPierAndQuay).
const pierRepo, quayRepo = "acme/pier", "acme/quay"

// seedPierAndQuay writes the history of a pull request held only through its project's default
// repository, and answers its day, yesterday: acme/quay is QUAY's default repository and the
// deploy repository, and acme/quay#1, merged yesterday, names PIER-1 alone, an issue of PIER that
// owns no repository and has no parent, while PIER's default is acme/pier; a deploy run ships it
// and a PR-checks run checks it, each with one job.
func seedPierAndQuay(t *testing.T, r *refresherRig) time.Time {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	day := refresherToday.AddDate(0, 0, -1)
	at := func(hour int) time.Time { return day.Add(time.Duration(hour) * time.Hour) }
	actor := model.Actor{Kind: "system", ID: "default-repository-test"}
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('PIER', 'Pier', 'alice', 2, $1), ('QUAY', 'Quay', 'alice', 1, $2)`, pierRepo, quayRepo)
	r.exec(t, "the issue", `insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('PIER-1', 'PIER', 1, 'Pier work', 'in_progress', '00000001', '{"kind":"system","id":"default-repository-test"}', $1)`,
		day.AddDate(0, 0, -1))
	production := "production / apply"
	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: quayRepo, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{},
	}, actor); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	// The attribution's inputs, as GitHub gave them: the title names PIER-1, which resolves to
	// acme/pier, so no issue is held and the row names none.
	if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
		Repo: quayRepo, Number: 1, Title: "PIER-1: change the pier", URL: "https://github.com/" + quayRepo + "/pull/1", Author: "dev-ana",
		CreatedAt: new(at(8)), MergedAt: new(at(9)), Sessions: []string{},
		Attribution: &delivery.AttributionInputs{TitleKeys: []string{"PIER-1"}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
	}); err != nil {
		t.Fatalf("write %s#1: %v", quayRepo, err)
	}
	for _, run := range []struct {
		id      int64
		kind    delivery.DeliveryRunKind
		created time.Time
		job     string
	}{
		{301, delivery.DeliveryRunKindDeploy, at(10), production},
		{401, delivery.DeliveryRunKindPRChecks, at(8).Add(30 * time.Minute), "lint"},
	} {
		started, completed := run.created.Add(time.Minute), run.created.Add(20*time.Minute)
		stored := delivery.DeliveryRun{
			Repo: quayRepo, RunID: run.id, Kind: run.kind, HeadSHA: fmt.Sprintf("%040d", run.id), HeadCommitAt: run.created.Add(-time.Minute),
			StartedAt: started, CreatedAt: new(run.created), CompletedAt: &completed, Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", quayRepo, run.id), HeadBranch: new("main"), Event: new("push"),
		}
		if run.kind == delivery.DeliveryRunKindPRChecks {
			stored.PRNumber, stored.HeadBranch, stored.Event = new(1), new("feature/pier"), new("pull_request")
		}
		if err := delivery.UpsertRun(ctx, pool, stored); err != nil {
			t.Fatalf("write run %d: %v", run.id, err)
		}
		job := delivery.DeliveryRunJob{Name: run.job, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess)}
		if err := delivery.UpsertRunJobs(ctx, pool, quayRepo, run.id, []delivery.DeliveryRunJob{job}, true); err != nil {
			t.Fatalf("write the job of run %d: %v", run.id, err)
		}
	}
	return day
}

// A pull request whose issue is held only through its project's default repository counts by a
// read-time selection, which a move of that default makes within the page that publishes it and
// with no day row written (dispatch://INFRA-2916, R15 and R14), over seedPierAndQuay's history:
// no issue is held for the pull request, and QUAY counts it, the deploy run that ships it and its
// PR-checks run through the repository, and PIER counts none of them. Once PIER's default moves to
// acme/quay and a page publishes it, PIER-1 resolves to acme/quay: PIER counts all three through
// its issue (via 0) and QUAY none, no day row or member is written by the move, and each project's
// eval list read of every offered range answers the fold of its day rows, as do the stored
// figures. The move back restores the first answer the same way. Red where the day rows follow
// delivery_pull_requests.issue_key alone, which the move leaves null: PIER then reads the
// repository's rows as QUAY does, through the repository.
func TestADefaultMoveReattributesAPullRequestHeldThroughItWithNoDayRowWritten(t *testing.T) {
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	day := seedPierAndQuay(t, r)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)

	series := map[string]string{
		SlugPROpenToMerge: measures.SeriesMinutes, SlugReworkShare: measures.SeriesPRs,
		SlugDeployFrequency: measures.SeriesDeploys, SlugPRCI: measures.SeriesMinutes,
	}
	type shape struct {
		name            string
		window, compare Range
	}
	var shapes []shape
	for _, n := range []int{7, 14, 28, 84} {
		compare := rangeOf(n, refresherToday.AddDate(0, 0, -n))
		shapes = append(shapes, shape{fmt.Sprintf("%d days", n), rangeOf(n, refresherToday), compare},
			shape{fmt.Sprintf("the live %d days", n), rangeOf(n+1, refresherToday.AddDate(0, 0, 1)), compare})
	}
	// check reads both projects' tallies of the day against want (each of series counted once, by
	// the project that counts the pull request, through the repository where via), and each
	// project's eval list read of every offered range against the fold of its day rows.
	check := func(what, counting string, via int) {
		t.Helper()
		for _, project := range []string{"PIER", "QUAY"} {
			got, rows := r.readTallies(t, project, series, day)
			for slug, name := range series {
				want := defaultTally{}
				if project == counting {
					want = defaultTally{n: 1, via: via}
				}
				if got[slug] != want {
					t.Errorf("%s: %s's %s.%s counts %d, %d through a default repository; want %d and %d (%d day rows read)",
						what, project, slug, name, got[slug].n, got[slug].via, want.n, want.via, rows)
				}
			}
			fromFigures := 0
			for _, one := range shapes {
				stated, folded, figures := r.listValues(t, project, nil, one.window, &one.compare)
				fromFigures += figures
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					if stated[id] != folded[id] {
						t.Errorf("%s: the read of %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, one.name, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
					}
				}
			}
			if fromFigures == 0 {
				t.Errorf("%s: the stored figures answered none of %s's evals over the offered ranges", what, project)
			}
		}
		expectTruth(t, what, r.storedFigures(t), r.truthFigures(t))
	}
	check("before the move", "QUAY", 1)

	show := func(xmin string) string { return "written by " + xmin }
	for _, move := range []struct {
		name, repo, counting string
		via                  int
	}{
		{"after PIER's default moves to " + quayRepo, quayRepo, "PIER", 0},
		{"after PIER's default moves back to " + pierRepo, pierRepo, "QUAY", 1},
	} {
		rows, members := r.xmins(t, dayRowXmins), r.xmins(t, memberXmins)
		r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, move.repo)
		r.settle(t)
		if published, err := PublishedDefaultIn(ctx, pool, "PIER"); err != nil || published != move.repo {
			t.Fatalf("%s: PIER publishes %q as its default (%v), want %s", move.name, published, err, move.repo)
		}
		if differ := sameValues(r.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
			t.Errorf("%s: the move wrote day rows, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if differ := sameValues(r.xmins(t, memberXmins), members, show); len(differ) > 0 {
			t.Errorf("%s: the move wrote members, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		check(move.name, move.counting, move.via)
	}
}

// webRepo is the repository the logged readings of logAcmeWeb name: no default of these histories
// is it, so no move of one moves its cuts.
const webRepo = "acme/web"

// logAcmeWeb registers project's logged eval review-latency, whose one series folds its readings'
// minutes as a quantile, and logs three readings of it on day, two of them of acme/web: rows of the
// project's cut of acme/web beside its own.
func (r *refresherRig) logAcmeWeb(t *testing.T, project string, day time.Time) {
	t.Helper()
	if _, err := r.registry.Register(t.Context(), Registration{
		Project: project, Slug: "review-latency", Caller: alice, Kind: Set(KindExperiment),
		Series: []SeriesChange{{Series: "minutes", SeriesComputationChange: SeriesComputationChange{Agg: Set(AggQuantile)}}},
	}); err != nil {
		t.Fatalf("register %s's review-latency: %v", project, err)
	}
	var rows []json.RawMessage
	for index, minutes := range []float64{12, 30, 7.5} {
		fields := map[string]any{"series": "minutes", "row_kind": "review", "row_key": fmt.Sprint(index), "value": minutes,
			"measured_at": day.Add(time.Duration(9+index) * time.Hour)}
		if index < 2 {
			fields["attributes"] = map[string]any{"repo": webRepo}
		}
		rows = append(rows, row(t, fields))
	}
	if _, err := r.registry.WriteReadings(t.Context(), Batch{Project: project, Slug: "review-latency", Rows: rows, Caller: alice, BuiltBy: r.build}); err != nil {
		t.Fatalf("log %s's review-latency: %v", project, err)
	}
}

// wokenPass makes one refresher pass as of the rig's clock with no batch of the walks, as the pass
// a wake runs inside a RefresherGap does (refresh), with no wake owed: its pages publish what they
// read, a move of a default repository among it, it makes none of the walks' background work, and
// it folds again each cut a move marked stale to its end (refoldStaleCuts).
func (r *refresherRig) wokenPass(t *testing.T) {
	t.Helper()
	r.refreshIn(t.Context(), t, nil)
}

// owedPass makes one refresher pass as of the rig's clock with batch (nil for none, as wokenPass
// makes it) while a wake of its loop is owed (duty.WakeOwed), as when a change commits during the
// pass: its refold of the cuts a move marked stale folds one batch, of the first cut still marked,
// and stops there for the wake (refresh.yields).
func (r *refresherRig) owedPass(t *testing.T, batch *gapBatch) {
	t.Helper()
	owed := make(chan struct{}, 1)
	owed <- struct{}{}
	r.refreshIn(duty.WithWake(t.Context(), owed), t, batch)
}

// markedBefore reports whether marks hold a cut before cut in the byte order of their keys, whose
// refold a pass folds before cut's (refoldStaleCuts).
func markedBefore(marks map[staleCut]staleMark, cut staleCut) bool {
	for marked := range marks {
		if compareStaleCuts(marked, cut) < 0 {
			return true
		}
	}
	return false
}

// staleMark is a stale cut's mark as eval_figure_cuts_stale holds it: its generation, and the
// first end day its refold has not folded (nil while the refold is to begin).
type staleMark struct {
	generation int64
	next       *time.Time
}

// staleMarks is every stale cut's mark, by cut.
func (r *refresherRig) staleMarks(t *testing.T) map[staleCut]staleMark {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `select project_key, repo, generation, next_day from eval_figure_cuts_stale`)
	if err != nil {
		t.Fatalf("read the stale cuts: %v", err)
	}
	marks := map[staleCut]staleMark{}
	var cut staleCut
	var mark staleMark
	if _, err := pgx.ForEachRow(rows, []any{&cut.project, &cut.repo, &mark.generation, &mark.next}, func() error {
		kept := mark
		if mark.next != nil {
			kept.next = new(measures.Midnight(*mark.next))
		}
		marks[cut] = kept
		return nil
	}); err != nil {
		t.Fatalf("read the stale cuts: %v", err)
	}
	return marks
}

// moveShapes is two offered reads of a cut: 28 days with a compare range as a GET, and the live 7
// days with a compare range.
func moveShapes() []struct {
	name            string
	window, compare Range
} {
	return []struct {
		name            string
		window, compare Range
	}{
		{"28 days", rangeOf(28, refresherToday), rangeOf(28, refresherToday.AddDate(0, 0, -28))},
		{"the live 7 days", rangeOf(8, refresherToday.AddDate(0, 0, 1)), rangeOf(7, refresherToday.AddDate(0, 0, -7))},
	}
}

// TestADefaultMoveMarksOnlyTheCutsItTouches: over seedPierAndQuay's history, with a logged eval of
// PIER and one of QUAY whose readings of acme/web are rows of each project's cut of it, PIER's
// default moves to acme/quay and a pass's pages publish it, the pass stopping before the rest of
// its work (pageAlone), so the refold of the cuts the move marks has not begun: a whole pass folds
// at least one batch of it, which is the whole refold of PIER's acme/pier, even in batches of one
// end day, and clears its mark. Its commit marks stale the cuts the move moves and no other:
// PIER's old and new default's (acme/pier, acme/quay), and QUAY's acme/quay, whose owner keys of
// acme/quay#1 and the runs of it PIER gains and QUAY loses. Right after it, a read of a touched cut
// (PIER's and QUAY's acme/quay) answers the fold of its day rows and is stated from no stored
// figure, while a read of an untouched cut of the same project and of the other (each one's
// acme/web), and of each project's own rows, which the move's commit folded, is stated from the
// stored figures, each answering the fold of its rows. Once the refresher settles no cut is marked,
// each touched cut is stated from the stored figures again, with the bytes the fold of its day rows
// answered while it was stale, and the stored figures are the fold of the rows that stand. Red
// where a move marks every cut of the projects it moves, or none of them, or where a read of a
// stale cut reads its stored figures.
func TestADefaultMoveMarksOnlyTheCutsItTouches(t *testing.T) {
	r := newRefresherRig(t)
	day := seedPierAndQuay(t, r)
	for _, project := range []string{"PIER", "QUAY"} {
		r.logAcmeWeb(t, project, day)
	}
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Fatalf("the first whole build left the cuts %v stale", marks)
	}
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.pageAlone(t)
	want := []staleCut{{project: "PIER", repo: pierRepo}, {project: "PIER", repo: quayRepo}, {project: "QUAY", repo: quayRepo}}
	if marked := slices.SortedFunc(maps.Keys(r.staleMarks(t)), compareStaleCuts); !slices.Equal(marked, want) {
		t.Fatalf("the page that published PIER's move to %s marked the cuts %v stale, want %v", quayRepo, marked, want)
	}
	reads := []struct {
		project, repo string
		touched       bool
	}{
		{"PIER", quayRepo, true}, {"QUAY", quayRepo, true},
		{"PIER", webRepo, false}, {"QUAY", webRepo, false}, {"PIER", "", false}, {"QUAY", "", false},
	}
	// whileStale is what the fold of each touched cut's day rows answered while it was stale, by
	// project, repository, shape and eval.
	whileStale := map[string]string{}
	check := func(what string, stale bool) {
		t.Helper()
		for _, read := range reads {
			var filter []string
			if read.repo != "" {
				filter = []string{read.repo}
			}
			for _, one := range moveShapes() {
				stated, folded, fromFigures := r.listValues(t, read.project, filter, one.window, &one.compare)
				fromStale := stale && read.touched
				if fromStale && fromFigures != 0 || !fromStale && fromFigures == 0 {
					t.Errorf("%s: the read of %s's %q over %s stated %d evals from the stored figures, want them stated from them %t",
						what, read.project, read.repo, one.name, fromFigures, !fromStale)
				}
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					if stated[id] != folded[id] {
						t.Errorf("%s: the read of %s's %q over %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, read.project, read.repo, one.name, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
					}
					key := read.project + " " + read.repo + " " + one.name + " " + id
					switch {
					case !read.touched:
					case stale:
						whileStale[key] = folded[id]
					case stated[id] != whileStale[key]:
						t.Errorf("%s: the read of %s's %q over %s states %s's values in %d bytes that differ from the %d the fold of its day rows answered while it was stale, at byte %d",
							what, read.project, read.repo, one.name, id, len(stated[id]), len(whileStale[key]), firstDiffering(stated[id], whileStale[key]))
					}
				}
			}
		}
	}
	check("right after the page that published the move", true)

	r.settle(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the passes after the move settled with the cuts %v stale", marks)
	}
	check("once the refresher settled", false)
	expectTruth(t, "once the refresher settled", r.storedFigures(t), r.truthFigures(t))
}

// TestAMoveDuringARefoldLeavesTheCutStaleUntilItsGenerationIsFolded: over seedPierAndQuay's
// history, with batches of one end day (refresherRig.batch) and every pass run under an owed wake
// (owedPass), so each pass folds one end day of the first cut still marked and stops for the wake,
// PIER's default moves to acme/quay and the pass a wake runs publishes it, which marks PIER's cut
// of acme/quay stale; once the cut before it is folded, one pass folds the first end day of the cut
// again and leaves the rest marked, its refold begun. PIER's default then moves back, and the pass
// that publishes it marks the cut again, at the next generation, its refold to begin again. The
// mark then stands at every pass until the refold of the new generation has folded every end day of
// the cut from its first, the passes the whole cut takes once no cut before it is marked and not
// the fewer the first generation's refold had left, a read of the cut folding the day rows
// meanwhile and answering what they fold; once it is cleared, the read is stated from the stored
// figures, and they are the fold of the rows that stand. Red where a move keeps the progress or the
// generation of a refold begun: the refold then goes on from where the first generation stood,
// clears the mark a pass early, and leaves the stored figures of the cut's first end day folded
// under the default the second move left.
func TestAMoveDuringARefoldLeavesTheCutStaleUntilItsGenerationIsFolded(t *testing.T) {
	r := newRefresherRig(t)
	seedPierAndQuay(t, r)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	r.batch = 1
	cut := staleCut{project: "PIER", repo: quayRepo}
	// reach makes passes until no cut before cut is marked, each folding one end day of the first
	// cut that is, so the next pass folds the next end day of cut.
	reach := func(what string) {
		t.Helper()
		for passes := 0; markedBefore(r.staleMarks(t), cut); passes++ {
			if passes > 100 {
				t.Fatalf("%s: %d passes left a cut before PIER's cut of %s stale: %v", what, passes, quayRepo, r.staleMarks(t))
			}
			r.owedPass(t, &gapBatch{})
		}
	}
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.owedPass(t, nil)
	first, held := r.staleMarks(t)[cut]
	if !held || first.next != nil {
		t.Fatalf("the pass that published PIER's move to %s left its cut of it marked %t at %+v, want marked with its refold to begin", quayRepo, held, first)
	}
	reach("after the move")
	r.owedPass(t, &gapBatch{})
	begun, held := r.staleMarks(t)[cut]
	if !held || begun.next == nil || begun.generation != first.generation {
		t.Fatalf("a pass of one end day left PIER's cut of %s marked %t at %+v, want its refold begun and not done, at generation %d", quayRepo, held, begun, first.generation)
	}
	// whole is how many end days the refold of the cut folds: from the first one the begun refold
	// folded through the last end day the figures are built through.
	whole := int(refresherToday.AddDate(0, 0, FigureLead).Sub(begun.next.AddDate(0, 0, -1))/(24*time.Hour)) + 1
	if whole < 2 {
		t.Fatalf("the refold of PIER's cut of %s folds %d end days, and the check needs more than the one its first batch folded", quayRepo, whole)
	}
	r.exec(t, "move PIER's default repository back", `update projects set default_repo = $1 where key = 'PIER'`, pierRepo)
	r.owedPass(t, nil)
	if again, held := r.staleMarks(t)[cut]; !held || again.generation != begun.generation+1 || again.next != nil {
		t.Fatalf("the pass that published PIER's move back to %s left its cut of %s marked %t at %+v, want generation %d with its refold to begin again",
			pierRepo, quayRepo, held, again, begun.generation+1)
	}
	reach("after the move back")
	shape := moveShapes()[0]
	passes := 0
	for {
		if _, held := r.staleMarks(t)[cut]; !held {
			break
		}
		if passes > 3*whole {
			t.Fatalf("%d passes after the move back and the cuts before it left PIER's cut of %s stale", passes, quayRepo)
		}
		stated, folded, fromFigures := r.listValues(t, "PIER", []string{quayRepo}, shape.window, &shape.compare)
		if fromFigures != 0 {
			t.Errorf("%d passes into the refold after the move back, with the cut stale, the read of PIER's %s stated %d evals from the stored figures", passes, quayRepo, fromFigures)
		}
		for _, id := range slices.Sorted(maps.Keys(folded)) {
			if stated[id] != folded[id] {
				t.Errorf("%d passes into the refold after the move back, the read of PIER's %s states %s's values otherwise than the fold of its day rows", passes, quayRepo, id)
			}
		}
		r.owedPass(t, &gapBatch{})
		passes++
	}
	if passes < whole {
		t.Errorf("the mark of PIER's cut of %s was cleared %d passes after the move back and the cuts before it, where the refold of its whole %d end days takes %d",
			quayRepo, passes, whole, whole)
	}
	stated, folded, fromFigures := r.listValues(t, "PIER", []string{quayRepo}, shape.window, &shape.compare)
	if fromFigures == 0 {
		t.Errorf("once the mark was cleared the read of PIER's %s stated no eval from the stored figures", quayRepo)
	}
	for _, id := range slices.Sorted(maps.Keys(folded)) {
		if stated[id] != folded[id] {
			t.Errorf("once the mark was cleared the read of PIER's %s states %s's values otherwise than the fold of its day rows", quayRepo, id)
		}
	}
	expectTruth(t, "once the refold of the new generation was done", r.storedFigures(t), r.truthFigures(t))
	t.Logf("the refold of PIER's cut of %s, begun again at generation %d, took %d passes of one end day", quayRepo, begun.generation+1, passes)
}

// TestThePassThatPublishesAMoveFoldsItsStaleCutsToTheEnd: over seedPierAndQuay's history, with
// batches of one end day (refresherRig.batch), PIER's default moves to acme/quay, and one pass with
// no batch of the walks and no wake owed (wokenPass), as the move's own wake runs it inside a
// RefresherGap, publishes the move, marks the cuts it moves stale and folds every one of them again
// to its end, a batch of one end day after another, back to back, PIER's cut of acme/quay alone
// taking more than one: once the pass ends PIER publishes acme/quay, no cut is marked, and the
// stored figures are the fold of the rows that stand, every cut's (expectTruth). Red against the
// refold a gap paced, where a pass with no batch of the walks folds no stale cut and a pass with
// one folds one batch of each: the pass then leaves every cut the move marked stale, and their
// stored figures folded under the defaults before it.
func TestThePassThatPublishesAMoveFoldsItsStaleCutsToTheEnd(t *testing.T) {
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	seedPierAndQuay(t, r)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	r.batch = 1
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.wokenPass(t)
	if published, err := PublishedDefaultIn(ctx, pool, "PIER"); err != nil || published != quayRepo {
		t.Fatalf("the pass left PIER publishing %q as its default (%v), want %s", published, err, quayRepo)
	}
	first, err := cutStart(ctx, pool, "PIER", quayRepo)
	if err != nil || first == nil {
		t.Fatalf("the first end day of PIER's cut of %s is %v (%v), want a day", quayRepo, first, err)
	}
	if days := int(refresherToday.AddDate(0, 0, FigureLead).Sub(*first)/(24*time.Hour)) + 1; days < 2 {
		t.Fatalf("the refold of PIER's cut of %s folds %d end days, and the check needs more than one batch of one", quayRepo, days)
	}
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the pass that published PIER's move to %s left the cuts %v stale", quayRepo, marks)
	}
	expectTruth(t, "after the pass that published the move", r.storedFigures(t), r.truthFigures(t))
}

// TestARefoldBatchFoldsAtLeastOneEndDayAndAtMostThePassBatch: the next batch of a stale cut's
// refold (refoldBatchDays) folds one end day after a batch far past the server's budget
// (refoldBatchBudget), whether it folded one end day or a whole batch, so a refold whose every end
// day takes longer than the budget still moves on; it folds the pass's batch size after a batch
// that took next to no time or none the clock could tell, never more; it folds one end day where
// the pass's batch size is none; it folds as many end days as the budget it is given holds; and,
// given no budget, as a test's schedule and RefreshThrough's give it, it folds the pass's batch
// size however long the batch before took. Red where the sizing rounds a slow batch down to no end
// day, which folds nothing and never ends, lets a fast one grow past the pass's batch size, sizes
// by the server's budget whatever budget it is given, or holds a refold with no budget to one end
// day after each slow batch, which on a slow database folds every end day in a transaction of its
// own.
func TestARefoldBatchFoldsAtLeastOneEndDayAndAtMostThePassBatch(t *testing.T) {
	for _, tc := range []struct {
		what         string
		days, most   int
		took, budget time.Duration
		want         int
	}{
		{"one end day far past the budget", 1, walkBatch, time.Hour, refoldBatchBudget, 1},
		{"a whole batch far past the budget", walkBatch, walkBatch, time.Hour, refoldBatchBudget, 1},
		{"a whole batch in next to no time", walkBatch, walkBatch, time.Nanosecond, refoldBatchBudget, walkBatch},
		{"one end day in no time the clock could tell", 1, walkBatch, 0, refoldBatchBudget, walkBatch},
		{"one end day with a pass batch of none", 1, 0, time.Nanosecond, refoldBatchBudget, 1},
		{"four end days in twice a budget of four times the server's", 4, walkBatch, 8 * refoldBatchBudget, 4 * refoldBatchBudget, 2},
		{"one end day far past no budget", 1, walkBatch, time.Hour, 0, walkBatch},
	} {
		if got := refoldBatchDays(tc.days, tc.took, tc.budget, tc.most); got != tc.want {
			t.Errorf("%s: under a budget of %s, the next batch of %d end days at most after %d end days in %s folds %d end days, want %d",
				tc.what, tc.budget, tc.most, tc.days, tc.took, got, tc.want)
		}
	}
}

// Clearing the last default repository a project holds (PUT /api/v1/projects/{key}/default-repository
// with {"repo": null}) is published by one page: the page's commit drops the published default and
// logs one eval_days change of no eval for the project (delivery.EvalDaysWrittenByDefaults), and
// the passes after it find every default published and publish nothing. Red where the commit hands
// the published projects over as a nil slice, which the database reads as null: its delete of the
// defaults no project holds then deletes none, and every page publishes the clear again.
func TestClearingTheLastDefaultRepositoryIsPublishedOnce(t *testing.T) {
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	r.exec(t, "give EVAL a default repository", `update projects set default_repo = 'acme/web' where key = $1`, r.project)
	r.pass(t)
	if published, err := PublishedDefaultIn(ctx, pool, r.project); err != nil || published != "acme/web" {
		t.Fatalf("%s publishes %q as its default (%v), want acme/web", r.project, published, err)
	}
	published := func(after int64) int {
		t.Helper()
		changes, err := delivery.ChangesSince(ctx, pool, after, 100000)
		if err != nil {
			t.Fatalf("read the delivery changes after %d: %v", after, err)
		}
		logged := 0
		for _, change := range changes {
			if change.Kind == delivery.ChangeKindEvalDays && change.RunKind == delivery.EvalDaysWrittenByDefaults {
				if change.Repo != r.project || change.Ref != "" {
					t.Errorf("a page logged the defaults change %+v, want one of %s and of no eval", change, r.project)
				}
				logged++
			}
		}
		return logged
	}

	r.exec(t, "clear EVAL's default repository", `update projects set default_repo = null where key = $1`, r.project)
	seq := r.latestSeq(t)
	r.pass(t)
	if held, err := PublishedDefaultIn(ctx, pool, r.project); err != nil || held != "" {
		t.Errorf("after the pass %s publishes %q as its default (%v), want none", r.project, held, err)
	}
	if logged := published(seq); logged != 1 {
		t.Errorf("the pass after the clear logged %d defaults changes, want one", logged)
	}
	seq = r.latestSeq(t)
	for range 3 {
		r.pass(t)
	}
	if logged := published(seq); logged != 0 {
		t.Errorf("the three passes after the clear was published logged %d defaults changes, want none: the clear is published again", logged)
	}
}

// The readers of an owner key are decided at read time by the published defaults
// (eval_project_owners, eval_owner_readers): a plain key is read by each project whose default is
// its repository, through the repository, less its sharers; a key that carries lists is read by the
// project each list picks (its first condition whose default is the key's repository, else its
// terminal), through its issue, and, where a list picks none, by each project whose default is the
// repository, through the repository, less its sharers; a project reached both ways reads through
// its issue. Once GAMMA's default moves to the repository, each list that names it as a condition
// picks GAMMA, so a key whose every list does is read by GAMMA alone, one with another list by
// that list's pick as well, and the plain keys by GAMMA too, through the repository.
func TestTheReadersOfAnOwnerKeyAreTheProjectsThePublishedDefaultsPick(t *testing.T) {
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const repo, other = "acme/r", "acme/other"
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee) values
		('ALPHA', 'Alpha', 'alice'), ('BETA', 'Beta', 'alice'), ('GAMMA', 'Gamma', 'alice'), ('DELTA', 'Delta', 'alice')`)
	r.exec(t, "publish the defaults", `insert into eval_days_defaults (project_key, repo) values ('ALPHA', $1), ('BETA', upper($1)), ('GAMMA', $2)`,
		repo, other)
	keys := map[string]string{
		"plain":                 OwnerKey(repo, nil, nil),
		"plain with a sharer":   OwnerKey(repo, []string{"ALPHA"}, nil),
		"a condition picks":     OwnerKey(repo, nil, []attributionList{{conditions: []string{"GAMMA", "ALPHA"}}}),
		"the terminal picks":    OwnerKey(repo, nil, []attributionList{{conditions: []string{"GAMMA"}, terminal: "DELTA"}}),
		"none picks":            OwnerKey(repo, nil, []attributionList{{conditions: []string{"GAMMA"}}}),
		"a sharer and two ways": OwnerKey(repo, []string{"BETA"}, []attributionList{{conditions: []string{"GAMMA"}}, {conditions: []string{"ALPHA"}}}),
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := recordOwners(ctx, tx, slices.Collect(maps.Values(keys))); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	readers := func() map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, `select owner, project_key, via from eval_project_owners where owner <> project_key order by owner, project_key`)
		if err != nil {
			t.Fatalf("read the readers: %v", err)
		}
		read := map[string][]string{}
		var owner, project string
		var via bool
		if _, err := pgx.ForEachRow(rows, []any{&owner, &project, &via}, func() error {
			read[owner] = append(read[owner], fmt.Sprintf("%s via=%t", project, via))
			return nil
		}); err != nil {
			t.Fatalf("read the readers: %v", err)
		}
		byCase := map[string]string{}
		for name, key := range keys {
			byCase[name] = strings.Join(read[key], ", ")
		}
		return byCase
	}
	check := func(what string, want map[string]string) {
		t.Helper()
		got := readers()
		for _, name := range slices.Sorted(maps.Keys(keys)) {
			if got[name] != want[name] {
				t.Errorf("%s: %s (%s) is read by [%s], want [%s]", what, name, keys[name], got[name], want[name])
			}
		}
	}
	check("GAMMA's default elsewhere", map[string]string{
		"plain":                 "ALPHA via=true, BETA via=true",
		"plain with a sharer":   "BETA via=true",
		"a condition picks":     "ALPHA via=false",
		"the terminal picks":    "DELTA via=false",
		"none picks":            "ALPHA via=true, BETA via=true",
		"a sharer and two ways": "ALPHA via=false",
	})
	r.exec(t, "publish GAMMA's move", `update eval_days_defaults set repo = $1 where project_key = 'GAMMA'`, repo)
	check("GAMMA's default the repository", map[string]string{
		"plain":                 "ALPHA via=true, BETA via=true, GAMMA via=true",
		"plain with a sharer":   "BETA via=true, GAMMA via=true",
		"a condition picks":     "GAMMA via=false",
		"the terminal picks":    "GAMMA via=false",
		"none picks":            "GAMMA via=false",
		"a sharer and two ways": "ALPHA via=false, GAMMA via=false",
	})
}
