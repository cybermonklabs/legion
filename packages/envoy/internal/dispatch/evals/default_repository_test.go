package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testwait"
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
	deployRepos, err := DeployReposIn(ctx, r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := DayWindowIn(ctx, r.database.Pool, []string{project}, entries, deployRepos, nil, Range{From: day, To: day.AddDate(0, 0, 1)}, nil)
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
// ships only PR4 (ALPHA-1's). deploy-frequency should count X, Y and Z for ALPHA (X and Z through
// ALPHA-1, Y through the default repository) and X and Y for BETA (each through the default
// repository: X ships PR2, and BETA is no project of X's issues), each deploy once in each, and
// production-deploy each deploy's production job once for each project that counts its deploy.
// The read the test makes tells only how many deploys each project counts and how many of them
// came through a default repository: each of the two evals counts three for ALPHA, one of them
// through the default repository, and two for BETA, both through it. Red under the rule that
// counts a row with no issue for no project: ALPHA counts X and Z and BETA nothing.
func TestADeployCountsOnceInEachProjectWhoseIssueOrDefaultRepositoryItReaches(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	day := refresherToday.AddDate(0, 0, -1)
	at := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 2, $1), ('BETA', 'Beta', 'alice', 1, $1)`, alphaRepo)
	r.exec(t, "the issue", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', $1, '{"kind":"system","id":"default-repository-test"}', $2)`,
		alphaRepo, day.AddDate(0, 0, -1))
	production := "production / apply"
	putSources(t, pool, "ALPHA", []string{"dev-ana"}, deliverytest.SourceOf(delivery.DeliverySettings{
		DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
	}, nil))
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
// deploy repository, which QUAY's one deploy source names (so no move of a default moves the
// source, and a move writes no day row of it), and acme/quay#1, merged yesterday, names PIER-1
// alone, an issue of PIER that owns no repository and has no parent, while PIER's default is
// acme/pier; a deploy run ships it and a PR-checks run checks it, each with one job.
func seedPierAndQuay(t *testing.T, r *refresherRig) time.Time {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	day := refresherToday.AddDate(0, 0, -1)
	at := func(hour int) time.Time { return day.Add(time.Duration(hour) * time.Hour) }
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('PIER', 'Pier', 'alice', 2, $1), ('QUAY', 'Quay', 'alice', 1, $2)`, pierRepo, quayRepo)
	r.exec(t, "the issue", `insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('PIER-1', 'PIER', 1, 'Pier work', 'in_progress', '00000001', '{"kind":"system","id":"default-repository-test"}', $1)`,
		day.AddDate(0, 0, -1))
	production := "production / apply"
	putSources(t, pool, "QUAY", []string{"dev-ana"}, deliverytest.SourceOf(delivery.DeliverySettings{
		DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
	}, new(quayRepo)))
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
// acme/quay, PIER-1 resolves to acme/quay: right after the page that publishes the move (pageAlone),
// and again once the refresher settles, PIER counts all three through its issue (via 0) and QUAY
// none, and each project's eval list read of every offered range answers the fold of its day rows;
// once it settles, no day row or member was written by the move, and the stored figures answer
// some of those reads and are the fold of the rows that stand. The move back restores the first
// answer the same way. Red where the day rows follow delivery_pull_requests.issue_key alone, which
// the move leaves null: PIER then reads the repository's rows as QUAY does, through the repository.
func TestADefaultMoveReattributesAPullRequestHeldThroughItWithNoDayRowWritten(t *testing.T) {
	t.Parallel()
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
	// project's eval list read of every offered range against the fold of its day rows; and, where
	// the refresher has settled, that the stored figures answer some of those reads and are the fold
	// of the rows that stand: right after the page that publishes a move, the cuts its commit marked
	// stale are not folded again yet.
	check := func(what, counting string, via int, settled bool) {
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
				fromFigures += len(figures)
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					if stated[id] != folded[id] {
						t.Errorf("%s: the read of %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, one.name, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
					}
				}
			}
			if settled && fromFigures == 0 {
				t.Errorf("%s: the stored figures answered none of %s's evals over the offered ranges", what, project)
			}
		}
		if settled {
			expectTruth(t, what, r.storedFigures(t), r.truthFigures(t))
		}
	}
	check("before the move", "QUAY", 1, true)

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
		r.pageAlone(t)
		if published, err := PublishedDefaultIn(ctx, pool, "PIER"); err != nil || published != move.repo {
			t.Fatalf("%s: PIER publishes %q as its default (%v), want %s", move.name, published, err, move.repo)
		}
		check(move.name+", right after the page that published it", move.counting, move.via, false)
		r.settle(t)
		if differ := sameValues(r.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
			t.Errorf("%s: the move wrote day rows, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if differ := sameValues(r.xmins(t, memberXmins), members, show); len(differ) > 0 {
			t.Errorf("%s: the move wrote members, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		check(move.name, move.counting, move.via, true)
	}
}

// webRepo is the repository the logged readings of TestADefaultMoveMarksOnlyTheCutsItTouches name
// (logReviews): no default of these histories is it, so no move of one moves its cuts.
const webRepo = "acme/web"

// logReviews registers project's logged eval review-latency, whose one series folds its readings'
// minutes as a quantile, and logs readings of it on day, two of each of repos and one of none:
// rows of the project's cut of each of repos beside its own.
func (r *refresherRig) logReviews(t *testing.T, project string, day time.Time, repos ...string) {
	t.Helper()
	if _, err := r.registry.Register(t.Context(), Registration{
		Project: project, Slug: "review-latency", Caller: alice, Kind: Set(KindExperiment),
		Series: []SeriesChange{{Series: "minutes", SeriesComputationChange: SeriesComputationChange{Agg: Set(AggQuantile)}}},
	}); err != nil {
		t.Fatalf("register %s's review-latency: %v", project, err)
	}
	var rows []json.RawMessage
	reading := func(minutes float64, repo string) {
		index := len(rows)
		fields := map[string]any{"series": "minutes", "row_kind": "review", "row_key": fmt.Sprint(index), "value": minutes,
			"measured_at": day.Add(time.Duration(9+index) * time.Hour)}
		if repo != "" {
			fields["attributes"] = map[string]any{"repo": repo}
		}
		rows = append(rows, row(t, fields))
	}
	for _, repo := range repos {
		reading(12, repo)
		reading(30, repo)
	}
	reading(7.5, "")
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
// pass: its refold of the cuts a move marked stale folds one batch, the first paint of the first
// cut whose mark owes one or else a batch of the first cut still marked, and stops there for the
// wake (refresh.yields).
func (r *refresherRig) owedPass(t *testing.T, batch *gapBatch) {
	t.Helper()
	owed := make(chan struct{}, 1)
	owed <- struct{}{}
	r.refreshIn(duty.WithWake(t.Context(), owed), t, batch)
}

// String is a stale cut's mark as a failure names it: its generation, its first paint's end day
// and its refold's, "-" for none, and its evals, "every" for every eval.
func (m cutMark) String() string {
	day := func(at *time.Time) string {
		if at == nil {
			return "-"
		}
		return at.Format(time.DateOnly)
	}
	evals := "every"
	if m.evals != nil {
		evals = fmt.Sprint(m.evals)
	}
	return fmt.Sprintf("generation %d, first paint %s, refold at %s, evals %s", m.generation, day(m.ready), day(m.next), evals)
}

// staleMarks is every stale cut's mark, by cut.
func (r *refresherRig) staleMarks(t *testing.T) map[staleCut]cutMark {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `select project_key, repo, generation, ready_through, next_day, evals from eval_figure_cuts_stale`)
	if err != nil {
		t.Fatalf("read the stale cuts: %v", err)
	}
	marks := map[staleCut]cutMark{}
	var cut staleCut
	var mark cutMark
	if _, err := pgx.ForEachRow(rows, []any{&cut.project, &cut.repo, &mark.generation, &mark.ready, &mark.next, &mark.evals}, func() error {
		kept := mark
		if mark.ready != nil {
			kept.ready = new(measures.Midnight(*mark.ready))
		}
		if mark.next != nil {
			kept.next = new(measures.Midnight(*mark.next))
		}
		kept.evals = slices.Clone(mark.evals)
		marks[cut] = kept
		return nil
	}); err != nil {
		t.Fatalf("read the stale cuts: %v", err)
	}
	return marks
}

// markedEvals is the evals marks names, each by its cut.
func markedEvals(marks map[staleCut]cutMark) map[staleCut][]string {
	named := make(map[staleCut][]string, len(marks))
	for cut, mark := range marks {
		named[cut] = mark.evals
	}
	return named
}

// marksOfMove is the marks a move of the published defaults names, by cut, read from every stored
// day row as the move's commit reads them (staleMarksOf): moves is each project whose own
// published default it moves, from and to ("" for none), and before and after each project's owner
// keys, with whether it reads each through the repository (readOwnerPairs), before and after the
// move. Of the cut of each of a project's two defaults, each of its evals that holds a listed row
// under legionfacts.DefaultRepo; of the cut of the repository of each owner key it gains, loses or
// reads otherwise, its eval of the slug of each listed row of that key, as it reads them; each in
// byte order, and no cut that holds no row the move moved.
func (r *refresherRig) marksOfMove(t *testing.T, moves map[string][2]string, before, after map[string]map[string]bool) map[staleCut][]string {
	t.Helper()
	defaultEvals, ownerSlugs := map[string]map[string]bool{}, map[string]map[string]bool{}
	add := func(sets map[string]map[string]bool, key, value string) {
		if sets[key] == nil {
			sets[key] = map[string]bool{}
		}
		sets[key][value] = true
	}
	for _, row := range r.dayRows(t) {
		switch {
		case !row.Listed:
		case isOwner(row.ProjectKey):
			add(ownerSlugs, row.ProjectKey, slugOf(row.ProjectKey, row.EvalID))
		case row.Repo == legionfacts.DefaultRepo:
			add(defaultEvals, row.ProjectKey, row.EvalID)
		}
	}
	named := map[staleCut]map[string]bool{}
	note := func(cut staleCut, evalID string) {
		if named[cut] == nil {
			named[cut] = map[string]bool{}
		}
		named[cut][evalID] = true
	}
	for project, move := range moves {
		for _, repo := range move {
			for evalID := range defaultEvals[project] {
				if repo != "" && move[0] != move[1] {
					note(staleCut{project: project, repo: repo}, evalID)
				}
			}
		}
	}
	for _, pairs := range []map[string]map[string]bool{before, after} {
		for project := range pairs {
			for _, owners := range []map[string]bool{before[project], after[project]} {
				for owner := range owners {
					viaBefore, readBefore := before[project][owner]
					viaAfter, readAfter := after[project][owner]
					if readBefore && readAfter && viaBefore == viaAfter {
						continue
					}
					repo, _, _, _ := parseOwner(owner)
					for slug := range ownerSlugs[owner] {
						note(staleCut{project: project, repo: repo}, EvalID(project, slug))
						note(staleCut{project: project}, EvalID(project, slug))
					}
				}
			}
		}
	}
	marks := make(map[staleCut][]string, len(named))
	for cut, evals := range named {
		marks[cut] = slices.Sorted(maps.Keys(evals))
	}
	return marks
}

// expectMarks checks that the marks the stale cuts hold name the evals want names, by cut, each at
// generation 1 with its first paint folded through the last end day of today, as the commit of a
// move folds it (foldMarkedFirstPaints), and its refold to begin. It goes on past a mark that names
// others, so a test's later checks say what those marks leave stale.
func (r *refresherRig) expectMarks(t *testing.T, what string, want map[staleCut][]string) {
	t.Helper()
	marks := r.staleMarks(t)
	if got := markedEvals(marks); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%s: the marks name %v, want %v", what, got, want)
	}
	through := refresherToday.AddDate(0, 0, FigureLead)
	for cut, mark := range marks {
		if mark.generation != 1 || mark.ready == nil || !mark.ready.Equal(through) || mark.next != nil {
			t.Errorf("%s: %v is marked at %s, want generation 1 with its first paint folded through %s and its refold to begin",
				what, cut, mark, through.Format(time.DateOnly))
		}
	}
}

// figureLineCut is the cut and the eval of the stored figures row line names (rangeLine,
// dayFigureLine; figureLineKey).
func figureLineCut(t *testing.T, line string) (staleCut, string) {
	t.Helper()
	cut, evalID, _, _ := figureLineKey(t, line)
	return cut, evalID
}

// staleFiguresOf is, by cut, the evals of each stored figures row of stored that is not truth's
// (expectTruth's missing, stale and extra rows): the evals whose stored figures a change moved,
// where truth is the fold of the rows that stand after it.
func staleFiguresOf(t *testing.T, stored, truth map[string]figureRow) map[staleCut]map[string]bool {
	t.Helper()
	moved := map[staleCut]map[string]bool{}
	note := func(line string) {
		cut, evalID := figureLineCut(t, line)
		if moved[cut] == nil {
			moved[cut] = map[string]bool{}
		}
		moved[cut][evalID] = true
	}
	for line, row := range truth {
		if held, found := stored[line]; !found || !held.same(row) {
			note(line)
		}
	}
	for line := range stored {
		if _, found := truth[line]; !found {
			note(line)
		}
	}
	return moved
}

// readShape is one offered read of a cut: its name, its window and its compare range.
type readShape struct {
	name            string
	window, compare Range
}

// moveShapes is two offered reads of a cut, each one a first paint reads (firstPaintOf): 28 days
// with a compare range as a GET, and the live 7 days with a compare range.
func moveShapes() []readShape {
	return []readShape{
		{"28 days", rangeOf(28, refresherToday), rangeOf(28, refresherToday.AddDate(0, 0, -28))},
		{"the live 7 days", rangeOf(8, refresherToday.AddDate(0, 0, 1)), rangeOf(7, refresherToday.AddDate(0, 0, -7))},
	}
}

// pastShapes is two offered reads of a cut no first paint reads (firstPaintOf), each over days the
// histories hold rows of: the 7 days before yesterday, whose window ends a day before the first
// paint's earliest window end, with the 7 days before them as its compare range; and 28 days as a
// GET, a first paint's window, with a compare range of the 7 days that end eleven days before it,
// no one of FigureLengths days before any window end of a first paint.
func pastShapes() []readShape {
	return []readShape{
		{"the 7 days before yesterday", rangeOf(7, refresherToday.AddDate(0, 0, -1)), rangeOf(7, refresherToday.AddDate(0, 0, -8))},
		{"28 days against a week long past", rangeOf(28, refresherToday), rangeOf(7, refresherToday.AddDate(0, 0, -11))},
	}
}

// figureXmins reads the transaction that last wrote each stored figure, by the row's key, for
// refresherRig.xmins.
const figureXmins = `
	select concat_ws(' ', 'range', project_key, repo, days, end_day, eval_id, deploy_repo), xmin::text from eval_range_figures
	union all
	select concat_ws(' ', 'day', project_key, repo, day, eval_id, deploy_repo), xmin::text from eval_day_figures`

// expectOneCommit checks that a page's publishing of a move of project's default repository wrote
// the project's published default, every stale mark and every stored figure written since figures
// (figureXmins, read before the move) in the transaction that moved the refresher's change cursor:
// the page's one commit (commitPage), so a read sees all of them or none. It answers how many stored
// figures that transaction wrote.
func (r *refresherRig) expectOneCommit(t *testing.T, what, project string, figures map[string]string) int {
	t.Helper()
	commit := r.xmins(t, `select name, xmin::text from eval_days_cursor where name = '`+changesCursor+`'`)[changesCursor]
	if commit == "" {
		t.Fatalf("%s: no page moved the refresher's change cursor", what)
	}
	for name, xmin := range r.xmins(t, `select 'the published default of ' || project_key, xmin::text from eval_days_defaults where project_key = '`+project+`'
		union all
		select 'the mark of ' || project_key || '''s cut ' || quote_literal(repo), xmin::text from eval_figure_cuts_stale`) {
		if xmin != commit {
			t.Errorf("%s: %s was written by transaction %s, and the page's commit is %s", what, name, xmin, commit)
		}
	}
	written := 0
	for name, xmin := range r.xmins(t, figureXmins) {
		if figures[name] == xmin {
			continue
		}
		written++
		if xmin != commit {
			t.Errorf("%s: the stored figure %s was written by transaction %s, and the page's commit is %s", what, name, xmin, commit)
		}
	}
	return written
}

// TestADefaultMoveMarksOnlyTheCutsItTouches: over seedPierAndQuay's history, with a logged eval of
// PIER and one of QUAY whose readings of acme/web are rows of each project's cut of it, PIER's
// default moves to acme/quay and a pass's pages publish it, the pass stopping before the rest of
// its work (pageAlone), so the refold of the cuts the move marks has not begun. Its commit marks
// stale the cuts the move moves rows of and no other, each naming the evals whose rows it moved
// there (marksOfMove), with its first paint folded in the same commit: PIER's and QUAY's acme/quay
// and own rows (""), whose owner keys of acme/quay#1 and the runs of it PIER gains and QUAY loses.
// The published default, every mark and every stored figure the move wrote are of the transaction
// that moved the refresher's change cursor (expectOneCommit), and it wrote some figures.
// PIER's old default's cut, acme/pier, holds no row the move moves (PIER holds none under
// ':default', and acme/pier no owner key), so no mark holds it. Every stored figure the move leaves
// other than the fold of the rows that stand is of an eval a mark names in its cut. Right after it,
// a first paint's read (moveShapes) of every cut states every eval from the stored figures, and a
// read of the past (pastShapes) of a touched cut states each eval its mark names from the fold of
// its day rows and every other eval from the stored figures, of an untouched cut of the same
// project and of the other (each one's acme/web) every eval from the stored figures, each answering
// the fold of its rows. Once the refresher settles no cut is marked, every eval of each touched
// cut is stated from the stored figures again, with the bytes it was stated in while the cut was
// stale, and the stored figures are the fold of the rows that stand. Red where a move marks every
// cut of the projects it moves, or none of them, the own rows among them, or names fewer evals than
// its moved rows hold; where a read of a stale cut reads the stored figures of an eval its mark
// names that the mark's generation has not folded, or of none of its evals; and where the defaults,
// the marks or the first paints land in a transaction of their own.
func TestADefaultMoveMarksOnlyTheCutsItTouches(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	day := seedPierAndQuay(t, r)
	for _, project := range []string{"PIER", "QUAY"} {
		r.logReviews(t, project, day, webRepo)
	}
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Fatalf("the first whole build left the cuts %v stale", marks)
	}
	before, err := readOwnerPairs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	figures := r.xmins(t, figureXmins)
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.pageAlone(t)
	if written := r.expectOneCommit(t, "the page that published PIER's move to "+quayRepo, "PIER", figures); written == 0 {
		t.Errorf("the page that published PIER's move to %s wrote no stored figure, and the check needs the first paints its commit folds", quayRepo)
	}
	after, err := readOwnerPairs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	want := r.marksOfMove(t, map[string][2]string{"PIER": {pierRepo, quayRepo}}, before, after)
	moves := []staleCut{{project: "PIER"}, {project: "PIER", repo: quayRepo}, {project: "QUAY"}, {project: "QUAY", repo: quayRepo}}
	if cuts := slices.SortedFunc(maps.Keys(want), compareStaleCuts); !slices.Equal(cuts, moves) {
		t.Fatalf("PIER's move to %s moves rows of the cuts %v, and the check needs PIER's and QUAY's own rows and %s alone", quayRepo, cuts, quayRepo)
	}
	r.expectMarks(t, "the page that published PIER's move to "+quayRepo, want)
	marked := markedEvals(r.staleMarks(t))
	for cut, evals := range staleFiguresOf(t, r.storedFigures(t), r.truthFigures(t)) {
		for _, evalID := range slices.Sorted(maps.Keys(evals)) {
			if !slices.Contains(marked[cut], evalID) {
				t.Errorf("the move left %s's stored figures of %s's cut %q other than the fold of the rows that stand, and no mark names it", evalID, cut.project, cut.repo)
			}
		}
	}
	reads := []staleCut{
		{project: "PIER", repo: quayRepo}, {project: "QUAY", repo: quayRepo},
		{project: "PIER", repo: webRepo}, {project: "QUAY", repo: webRepo}, {project: "PIER"}, {project: "QUAY"},
	}
	// stated is what each touched cut's read stated the first time it was made, by project,
	// repository, shape and eval.
	stated := map[string]string{}
	check := func(what string, shapes []readShape, stale bool) {
		t.Helper()
		for _, read := range reads {
			var filter []string
			if read.repo != "" {
				filter = []string{read.repo}
			}
			touched := marked[read] != nil
			for _, one := range shapes {
				said, folded, fromFigures := r.listValues(t, read.project, filter, one.window, &one.compare)
				folds := 0
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					fromRows := stale && slices.Contains(marked[read], id)
					if fromRows {
						folds++
					}
					if fromFigures[id] == fromRows {
						t.Errorf("%s: the read of %s's %q over %s states %s from the stored figures %t, want %t", what, read.project, read.repo, one.name, id, fromFigures[id], !fromRows)
					}
					if said[id] != folded[id] {
						t.Errorf("%s: the read of %s's %q over %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, read.project, read.repo, one.name, id, len(said[id]), len(folded[id]), firstDiffering(said[id], folded[id]))
					}
					key := read.project + " " + read.repo + " " + one.name + " " + id
					was, held := stated[key]
					switch {
					case !touched:
					case !held:
						stated[key] = said[id]
					case said[id] != was:
						t.Errorf("%s: the read of %s's %q over %s states %s's values in %d bytes that differ from the %d it stated while the cut was stale, at byte %d",
							what, read.project, read.repo, one.name, id, len(said[id]), len(was), firstDiffering(said[id], was))
					}
				}
				if stale && touched && (folds == 0 || len(fromFigures) == 0) {
					t.Fatalf("%s: the read of %s's %q over %s folds the day rows of %d evals and states %d from the stored figures, and the check needs both",
						what, read.project, read.repo, one.name, folds, len(fromFigures))
				}
			}
		}
	}
	check("right after the page that published the move, over a first paint", moveShapes(), false)
	check("right after the page that published the move, over the past", pastShapes(), true)

	r.settle(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the passes after the move settled with the cuts %v stale", marks)
	}
	check("once the refresher settled", slices.Concat(moveShapes(), pastShapes()), false)
	expectTruth(t, "once the refresher settled", r.storedFigures(t), r.truthFigures(t))
}

// TestAMoveDuringARefoldLeavesTheCutStaleUntilItsGenerationIsFolded: over seedPierAndQuay's
// history, with batches of one end day (refresherRig.batch) and every pass run under an owed wake
// (owedPass), so each pass folds one batch and stops for the wake, PIER's default moves to
// acme/quay and the pass a wake runs publishes it: the commit marks PIER's cut of acme/quay stale
// at generation 1 and folds its first paint (its acme/pier is not marked: no row of it moves), and
// the passes after it fold the cuts before it in the byte order of their keys and then the first
// end day of its refold, the last end day its figures are built through, newest first, and leave
// the rest marked, its refold begun. PIER's default then moves back, and the page that publishes it
// (pageAlone) marks the cut again in its commit, at the next generation, naming the evals of both
// moves (marksOfMove), its refold to begin again, and folds its first paint under the default it
// moved back to: each read of a first paint of the cut (moveShapes) right after the commit states
// every eval from the stored figures, each the fold of its day rows, and so at every pass until the
// refold of the new generation has come down to the cut's first end day and cleared the mark; the
// stored figures are then the fold of the rows that stand. Red where the commit of the move back
// folds no first paint, or records one it did not fold: the reads right after it then state the
// evals the mark names from their day rows, or from stored figures folded under the first move's
// default, which differ from the fold of their day rows. Red too where a move keeps the progress or
// the generation of a refold begun, which the mark then holds where the first generation's refold
// left it.
func TestAMoveDuringARefoldLeavesTheCutStaleUntilItsGenerationIsFolded(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	seedPierAndQuay(t, r)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	r.batch = 1
	cut := staleCut{project: "PIER", repo: quayRepo}
	through := refresherToday.AddDate(0, 0, FigureLead)
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	r.owedPass(t, nil)
	if first, held := r.staleMarks(t)[cut]; !held || first.generation != 1 || first.ready == nil || !first.ready.Equal(through) {
		t.Fatalf("the pass that published PIER's move to %s left its cut of it marked %t at %s, want generation 1 with its first paint folded through %s",
			quayRepo, held, first, through.Format(time.DateOnly))
	}
	for passes := 0; r.staleMarks(t)[cut].next == nil; passes++ {
		if passes > 100 {
			t.Fatalf("%d passes left the refold of PIER's cut of %s to begin: %v", passes, quayRepo, r.staleMarks(t))
		}
		r.owedPass(t, &gapBatch{})
	}
	begun := r.staleMarks(t)[cut]
	if begun.generation != 1 || !begun.next.Equal(through.AddDate(0, 0, -1)) {
		t.Fatalf("the first batch of PIER's cut of %s left it marked at %s, want generation 1 with its refold begun at %s, the last end day", quayRepo, begun,
			through.Format(time.DateOnly))
	}
	start, err := cutStart(ctx, pool, "PIER", quayRepo)
	if err != nil || start == nil {
		t.Fatalf("the first end day of PIER's cut of %s is %v (%v), want a day", quayRepo, start, err)
	}
	// whole is how many end days the refold of the cut folds: from the last end day the figures are
	// built through back to the cut's first, the bound of the passes the new generation's refold
	// takes.
	whole := int(through.Sub(*start)/(24*time.Hour)) + 1
	if whole < 2 {
		t.Fatalf("the refold of PIER's cut of %s folds %d end days, and the check needs more than the one its first batch folded", quayRepo, whole)
	}
	before, err := readOwnerPairs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	r.exec(t, "move PIER's default repository back", `update projects set default_repo = $1 where key = 'PIER'`, pierRepo)
	r.pageAlone(t)
	after, err := readOwnerPairs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	both := slices.Concat(begun.evals, r.marksOfMove(t, map[string][2]string{"PIER": {quayRepo, pierRepo}}, before, after)[cut])
	slices.Sort(both)
	both = slices.Compact(both)
	if len(both) == 0 {
		t.Fatalf("the moves of PIER's default name no eval of its cut of %s, and the check needs the evals they name", quayRepo)
	}
	again, held := r.staleMarks(t)[cut]
	if !held || again.generation != begun.generation+1 || again.ready == nil || !again.ready.Equal(through) || again.next != nil || !slices.Equal(again.evals, both) {
		t.Fatalf("the page that published PIER's move back to %s left its cut of %s marked %t at %s, want generation %d with its first paint folded through %s and its refold to begin again, naming %v",
			pierRepo, quayRepo, held, again, begun.generation+1, through.Format(time.DateOnly), both)
	}
	passes := 0
	for {
		// Each read of a first paint of the cut states every eval the mark names from the stored
		// figures, each the fold of its day rows.
		r.expectReads(t, fmt.Sprintf("%d passes after the commit of the move back", passes), map[staleCut][]string{cut: both}, readsOf(moveShapes()),
			func(staleCut) bool { return true })
		if _, held := r.staleMarks(t)[cut]; !held {
			break
		}
		if passes > 3*whole+100 {
			t.Fatalf("%d passes after the commit of the move back left PIER's cut of %s stale", passes, quayRepo)
		}
		r.owedPass(t, &gapBatch{})
		passes++
	}
	expectTruth(t, "once the refold of the new generation was done", r.storedFigures(t), r.truthFigures(t))
	t.Logf("the refold of PIER's cut of %s, begun again at generation %d, took %d passes of one batch after the commit of the move back", quayRepo, again.generation, passes)
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
	t.Parallel()
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

// TestAMoveOfADefaultRefoldsTheEvalsOfItsDefaultRowsAlone: the Legion project publishes
// acme/legion as its default repository, its Legion rows stand under acme/legion and under
// ':default' (seedLegionDefault), and a logged eval of it holds readings of acme/legion and of
// acme/other, rows of each cut that no move of the default moves. Its default moves to acme/other
// and a pass's pages publish it (pageAlone): the commit marks the two defaults' cuts alone, each
// naming the evals that hold a listed ':default' row, as the stored rows hold them (marksOfMove),
// the logged eval not among them, and every stored figure the move leaves other than the fold of
// the rows that stand is of an eval a mark names in its cut. Right after the move's commit, which
// folded the first paint of both cuts, and once the passes, one batch each, have folded one end
// day of the first cut's refold, the newest, a read of a first paint (moveShapes) of either cut
// states every eval from the stored figures and a read of the past (pastShapes) states the evals
// the marks name from their day rows and every other eval from the stored figures, each the fold
// of the rows that stand. The refresher then folds the marked evals again and settles: no cut is
// marked, every eval of both cuts is read from the stored figures, every eval's stored figures of
// every cut are the fold of the rows that stand, as a refold of every eval leaves them
// (expectTruth), and no stored row of an eval no mark names was written or deleted in the two cuts
// since the move (each row's last transaction, xmin, stands). Red where the move names one eval
// fewer than the rows call for: its stored figures in the two cuts stay those of the old default,
// which a read states meanwhile and the refold leaves. Red too where a read of a stale cut states
// none of its evals from the stored figures, or states a marked one from them before the stored
// rows it reads are folded again, and where the refold or the first paint deletes the stored rows
// of the evals it does not fold.
func TestAMoveOfADefaultRefoldsTheEvalsOfItsDefaultRowsAlone(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	defaultRepo, nextDefault := "acme/legion", "acme/other"
	project := r.seedLegionDefault(t, defaultRepo)
	logged := EvalID(project, "review-latency")
	r.logReviews(t, project, refresherToday.AddDate(0, 0, -1), defaultRepo, nextDefault)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	before, err := readOwnerPairs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	r.exec(t, "move the project's default repository", `update projects set default_repo = $2 where key = $1`, project, nextDefault)
	r.pageAlone(t)
	after, err := readOwnerPairs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	cuts := []staleCut{{project: project, repo: defaultRepo}, {project: project, repo: nextDefault}}
	want := r.marksOfMove(t, map[string][2]string{project: {defaultRepo, nextDefault}}, before, after)
	if got := slices.SortedFunc(maps.Keys(want), compareStaleCuts); !slices.Equal(got, cuts) || len(want[cuts[0]]) < 2 || slices.Contains(want[cuts[0]], logged) {
		t.Fatalf("the move moves the rows of %v, and the check needs the evals of %s's ':default' rows, at least two and not %s, in the cuts %v alone", want, project, logged, cuts)
	}
	r.expectMarks(t, "the page that published the move", want)
	marked := markedEvals(r.staleMarks(t))
	moved := r.storedFigures(t)
	staleFigures := staleFiguresOf(t, moved, r.truthFigures(t))
	if len(staleFigures) == 0 {
		t.Fatalf("the move left every stored figure the fold of the rows that stand, and the check needs it to move some")
	}
	for cut, evals := range staleFigures {
		for _, evalID := range slices.Sorted(maps.Keys(evals)) {
			if !slices.Contains(marked[cut], evalID) {
				t.Errorf("the move left %s's stored figures of %s's cut %q other than the fold of the rows that stand, and no mark names it", evalID, cut.project, cut.repo)
			}
		}
	}
	check := func(what string, shapes []readShape, stale bool) {
		t.Helper()
		for _, cut := range append(cuts, staleCut{project: project}) {
			var filter []string
			if cut.repo != "" {
				filter = []string{cut.repo}
			}
			for _, one := range shapes {
				stated, folded, fromFigures := r.listValues(t, project, filter, one.window, &one.compare)
				if !fromFigures[logged] {
					t.Errorf("%s: the read of %s's %q over %s states %s, which no mark names, from its day rows", what, project, cut.repo, one.name, logged)
				}
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					if fromRows := stale && slices.Contains(marked[cut], id); fromFigures[id] == fromRows {
						t.Errorf("%s: the read of %s's %q over %s states %s from the stored figures %t, want %t", what, project, cut.repo, one.name, id, fromFigures[id], !fromRows)
					}
					if stated[id] != folded[id] {
						t.Errorf("%s: the read of %s's %q over %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, project, cut.repo, one.name, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
					}
				}
			}
		}
	}
	check("right after the page that published the move, over a first paint", moveShapes(), false)
	check("right after the page that published the move, over the past", pastShapes(), true)
	r.batch = 1
	for passes := 0; r.staleMarks(t)[cuts[0]].next == nil; passes++ {
		if passes > 10 {
			t.Fatalf("%d passes of one batch each left the refold of %v to begin: %v", passes, cuts[0], r.staleMarks(t))
		}
		r.owedPass(t, &gapBatch{})
	}
	if marks := r.staleMarks(t); len(marks) != len(cuts) || marks[cuts[0]].ready == nil || marks[cuts[1]].ready == nil || marks[cuts[1]].next != nil {
		t.Fatalf("the passes that began the refold of %v left the marks %v, and the check needs %v marked, each with its first paint folded and the second's refold to begin",
			cuts[0], marks, cuts)
	}
	check("one batch into the refold, over a first paint", moveShapes(), false)
	check("one batch into the refold, over the past", pastShapes(), true)
	r.settle(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the passes after the move settled with the cuts %v stale", marks)
	}
	check("once the refresher settled", slices.Concat(moveShapes(), pastShapes()), false)
	settled := r.storedFigures(t)
	expectTruth(t, "once the refold of the marked evals was done", settled, r.truthFigures(t))
	unmarked := 0
	for line := range moved {
		if cut, evalID := figureLineCut(t, line); slices.Contains(cuts, cut) && !slices.Contains(marked[cut], evalID) {
			unmarked++
		}
	}
	if unmarked == 0 {
		t.Fatalf("the two cuts hold no stored row of an eval no mark names, and the check needs some")
	}
	for _, line := range writtenFigures(moved, settled) {
		if cut, evalID := figureLineCut(t, line); slices.Contains(cuts, cut) && !slices.Contains(marked[cut], evalID) {
			t.Errorf("the refold wrote or deleted the stored row of %s, an eval its cut's mark does not name", line)
		}
	}
	t.Logf("the move marked %v; the two cuts held %d stored rows of evals no mark names, and the refold left each as it stood", marked, unmarked)
}

// TestAMarkOfAMarkedCutNamesTheEvalsOfBoth: markStaleCuts marks a cut no mark holds at generation 1,
// naming the evals it is given, its first paint and its refold to begin, and leaves out the cut of
// a project no row holds; it marks a cut a mark holds already, whose first paint is folded and
// whose refold has begun, at the next generation with its first paint and its refold to begin
// again, naming the evals of both marks once each, in byte order; a cut whose mark names every eval
// (null), as the bench's worst case marks it, stays named for every eval; and it answers each mark
// it wrote as the mark reads back, the marks whose first paints the commit of a move folds
// (foldMarkedFirstPaints). Red where a mark that stands takes the second move's evals alone, which
// leaves the stored figures of the evals only the first move moved folded under the defaults
// before it, where a null mark takes the second move's, or where a mark keeps the first paint of
// the generation before it, whose stored figures a read would then state though they fold the rows
// under the defaults before the move.
func TestAMarkOfAMarkedCutNamesTheEvalsOfBoth(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	// same is whether two marks hold one generation, each a first paint and a refold or neither, and
	// the same evals, nil (every eval) or not.
	same := func(a, b cutMark) bool {
		return a.generation == b.generation && (a.ready == nil) == (b.ready == nil) && (a.next == nil) == (b.next == nil) && slices.Equal(a.evals, b.evals) &&
			(a.evals == nil) == (b.evals == nil)
	}
	mark := func(what string, marks map[staleCut][]string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		written, err := markStaleCuts(ctx, tx, marks)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if read := r.staleMarks(t); !maps.EqualFunc(written, read, same) {
			t.Errorf("%s: markStaleCuts answered the marks %+v, and they read back %+v", what, written, read)
		}
	}
	expect := func(what string, want map[staleCut]cutMark) {
		t.Helper()
		if got := r.staleMarks(t); !maps.EqualFunc(got, want, same) {
			t.Errorf("%s: the marks are %+v, want %+v", what, got, want)
		}
	}
	first, second := staleCut{project: r.project, repo: "acme/one"}, staleCut{project: r.project, repo: "acme/two"}
	evals := func(slugs ...string) []string {
		ids := make([]string, len(slugs))
		for index, slug := range slugs {
			ids[index] = EvalID(r.project, slug)
		}
		return ids
	}
	mark("the first move", map[staleCut][]string{first: evals("b", "c"), second: evals("a"), {project: "GONE", repo: "acme/one"}: {"GONE/a"}})
	expect("after the first move", map[staleCut]cutMark{first: {generation: 1, evals: evals("b", "c")}, second: {generation: 1, evals: evals("a")}})
	r.exec(t, "fold the first cut's first paint and begin its refold", `update eval_figure_cuts_stale set ready_through = $3, next_day = $3 where project_key = $1 and repo = $2`,
		r.project, first.repo, refresherToday)
	r.exec(t, "fold the second cut's first paint", `update eval_figure_cuts_stale set ready_through = $3 where project_key = $1 and repo = $2`, r.project, second.repo, refresherToday)
	r.exec(t, "name every eval of the second cut", `update eval_figure_cuts_stale set evals = null where project_key = $1 and repo = $2`, r.project, second.repo)
	mark("the second move", map[staleCut][]string{first: evals("a", "c"), second: evals("b")})
	expect("after the second move", map[staleCut]cutMark{first: {generation: 2, evals: evals("a", "b", "c")}, second: {generation: 2}})
}

// TestTheReadsOfTheEvalsAMoveMovesAreBoundedByIndexes: the reads of the evals a move of a default
// repository moves in a cut (staleMarksOf), which run in the move's commit inside R14's 2 s, read
// eval_days through bounded ranges of its indexes. Over a project's rows of four evals, two of
// which hold listed ':default' rows, one only unlisted ones, and an owner key's rows of two evals,
// each over 120 days of two series: the read of the project's ':default' evals is one range of
// eval_days_window bounded by the project and ':default', under the generic plan pgx's cached
// statement can settle on and under the plan of its values, each run, and reads exactly the 480
// listed ':default' rows; the walk of the owner
// key's evals reads eval_days_stamps bounded by the key and, after its first probe, by the eval id
// before, and reads one row for each eval, not the key's 480. The two answer the cut's evals,
// those of the ':default' rows and the owner key's under the project's key. Red where a read takes
// the project's or the key's every row and keeps those of the repository or the first of each eval.
func TestTheReadsOfTheEvalsAMoveMovesAreBoundedByIndexes(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	owner := OwnerKey("acme/main", nil, nil)
	r.exec(t, "seed the day rows", `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, built_by)
		select k.key, k.key || '/' || k.slug, s.series, k.repo, date '2026-07-01' + n, k.listed, 'test'
		from (values ($1::text, 'a', ':default', true), ($1, 'b', ':default', true), ($1, 'b', 'acme/main', true), ($1, 'c', ':default', false),
			($1, 'd', 'acme/main', true), ($1, 'd', '', true), ($2, 'x', 'acme/main', true), ($2, 'y', 'acme/main', true)) as k(key, slug, repo, listed)
		cross join (values ('n'), ('m')) as s(series)
		cross join generate_series(0, 119) as n`, r.project, owner)
	r.exec(t, "analyze the day rows", `analyze eval_days`)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	cut := staleCut{project: r.project, repo: "acme/main"}
	marks, err := staleMarksOf(ctx, tx, map[staleCut]*movedCut{cut: {ownDefault: true, owners: map[string]bool{owner: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{EvalID(r.project, "a"), EvalID(r.project, "b"), EvalID(r.project, "x"), EvalID(r.project, "y")}; !slices.Equal(marks[cut], want) {
		t.Errorf("the move names %v of the cut, want %v", marks[cut], want)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	keys := explaintest.NewIndexKeys(pool)
	for number, read := range []struct {
		name, statement, index, bound, literal string
		arg                                    []string
		rows                                   float64
	}{
		{"the read of the ':default' evals", DefaultRowEvalsQuery, "eval_days_window", "'" + legionfacts.DefaultRepo + "'", "array['" + r.project + "']", []string{r.project}, 480},
		{"the walk of the owner keys' evals", ownerEvalsQuery, "eval_days_stamps", "eval_id >", "array['" + owner + "']", []string{owner}, 2},
	} {
		valued, _ := explaintest.AnalyzedPlan(t, ctx, pool, read.statement, read.arg)
		for _, explained := range []struct {
			mode string
			plan json.RawMessage
		}{
			{"the generic plan", explaintest.AnalyzedGenericPlan(t, ctx, connection, fmt.Sprintf("move_evals_%d", number), read.statement,
				[]string{"text[]"}, []string{read.literal}, "set local enable_seqscan = off")},
			{"the plan of its values", valued},
		} {
			plan := explained.plan
			if explaintest.SeqScansRelation(t, plan, "eval_days") {
				t.Errorf("%s scans eval_days under %s:\n%s", read.name, explained.mode, explaintest.Outline(t, plan))
			}
			keys.RequireIndexKeyed(t, plan, read.index, read.name+" under "+explained.mode, explaintest.CondRequired)
			bounded := false
			for _, scan := range explaintest.IndexScans(t, plan) {
				if scan.Index != read.index && slices.Contains(evalDaysIndexes, scan.Index) {
					t.Errorf("%s reads eval_days through %s under %s, want %s alone:\n%s", read.name, scan.Index, explained.mode, read.index, explaintest.Outline(t, plan))
				}
				bounded = bounded || scan.Index == read.index && strings.Contains(scan.Cond, read.bound)
			}
			if !bounded {
				t.Errorf("%s reads %s bounded by no %s under %s:\n%s", read.name, read.index, read.bound, explained.mode, explaintest.Outline(t, plan))
			}
			readRows := evalDaysRowsRead(t, plan)
			if math.Abs(readRows-read.rows) > 0.5 {
				t.Errorf("%s read %.0f rows of eval_days under %s, want %.0f:\n%s", read.name, readRows, explained.mode, read.rows, explaintest.Outline(t, plan))
			}
			t.Logf("%s read %.0f rows of eval_days under %s:\n%s", read.name, readRows, explained.mode, explaintest.Outline(t, plan))
		}
	}
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
	t.Parallel()
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
	t.Parallel()
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

// A commit of a move of the default repositories locks every published default before it writes
// one, in the byte order of their keys (readPublishedDefaults), as the boot's rename pass locks
// those it moves (store's markRenamedCuts): the order the eval tables' lock order gives them.
// ALPHA's published default moves and BRAVO's is cleared, and a transaction holds ALPHA's row, as
// a rename pass that reached it would: the commit waits for it holding no lock of BRAVO's, and,
// once let on, publishes ALPHA's move and BRAVO's clear. Red where the commit deletes the defaults
// no project holds before it takes the rest in the order of their keys: it then holds BRAVO's row
// while it waits for ALPHA's, the other way round from the rename pass, and the two deadlock where
// the pass holds ALPHA's and waits for BRAVO's.
func TestAMoveLocksThePublishedDefaultsInTheOrderOfTheirKeys(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 'acme/next'), ('BRAVO', 'Bravo', 'alice', null)`)
	r.exec(t, "publish the defaults before the move", `insert into eval_days_defaults (project_key, repo) values ('ALPHA', 'acme/alpha'), ('BRAVO', 'acme/bravo')`)
	work := &pageWork{}
	if err := r.registry.newRefresh(r.build, r.now, r.batch).syncDefaults(ctx, work); err != nil || work.defaults == nil {
		t.Fatalf("the page reads no move of the defaults (%v)", err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of ALPHA's published default: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select from eval_days_defaults where project_key = 'ALPHA' for update`); err != nil {
		t.Fatalf("hold ALPHA's published default: %v", err)
	}
	committed := make(chan error, 1)
	go func() {
		committed <- pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			before, err := readSelection(ctx, tx)
			if err != nil {
				return err
			}
			_, err = publishDefaults(ctx, tx, r.build, nil, work.defaults, before, nil)
			return err
		})
	}()
	testwait.Until(t, "the commit of the move to wait for ALPHA's published default", func() (int, bool) {
		select {
		case err := <-committed:
			t.Fatalf("the commit of the move answered (%v) while ALPHA's published default was held, want it to wait for it", err)
		default:
		}
		waiting, err := storetest.LockWaiters(ctx, holder)
		if err != nil {
			t.Fatal(err)
		}
		return waiting, waiting > 0
	})
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe of BRAVO's published default: %v", err)
	}
	_, err = probe.Exec(ctx, `select from eval_days_defaults where project_key = 'BRAVO' for update nowait`)
	_ = probe.Rollback(ctx)
	if err != nil {
		t.Errorf("while the commit of the move waited for ALPHA's published default, a lock of BRAVO's answered %v; want it free, the commit holding no row of a later key", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the commit of the move on: %v", err)
	}
	if err := testwait.Receive(t, "the commit of the move", committed); err != nil {
		t.Fatalf("the commit of the move: %v", err)
	}
	for project, want := range map[string]string{"ALPHA": "acme/next", "BRAVO": ""} {
		if published, err := PublishedDefaultIn(ctx, pool, project); err != nil || published != want {
			t.Errorf("after the commit of the move %s publishes %q as its default (%v), want %q", project, published, err, want)
		}
	}
}

// The commit of a move of the default repositories takes no eval lock, whose place in the eval
// tables' lock order is before every row lock, so it never waits for a readings write while it
// holds the rows it wrote (foldMarkedFirstPaints). Over seedPierAndQuay's history, with a logged
// eval of PIER (logReviews), PIER's own cut ("") holds a mark that names every eval, its first
// paint folded, as the boot's rename pass and its fold leave a cut (store's markRenamedCuts), and
// PIER's default moves to acme/quay, which marks that cut again, still for every eval, the logged
// one among them. A transaction holds the logged eval's lock, as a readings write of it does, and a
// pass a wake runs publishes the move: the pass waits for the lock once the move is published, the
// first paint of PIER's own cut left owed by the commit and that of every other cut the move marked
// folded in it, in a transaction that holds no transaction id, no row locked before the eval's
// lock. Let go, the pass folds every marked cut to its end. Red where the commit folds the first
// paints of the logged evals under their locks: the pass then waits in the commit, the move
// unpublished and the commit's rows written.
func TestTheCommitOfAMoveTakesNoEvalLock(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	day := seedPierAndQuay(t, r)
	r.logReviews(t, "PIER", day, webRepo)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	through := refresherToday.AddDate(0, 0, FigureLead)
	own := staleCut{project: "PIER"}
	r.exec(t, "mark PIER's own cut for every eval, its first paint folded", `
		insert into eval_figure_cuts_stale (project_key, repo, generation, ready_through, next_day, evals) values ('PIER', '', 1, $1, null, null)`, through)
	logged := EvalID("PIER", "review-latency")
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of %s's lock: %v", logged, err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(hashtext('eval:' || $1))`, logged); err != nil {
		t.Fatalf("hold %s's lock: %v", logged, err)
	}
	r.exec(t, "move PIER's default repository", `update projects set default_repo = $1 where key = 'PIER'`, quayRepo)
	if _, err := r.horizon.Await(ctx); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	passed := make(chan error, 1)
	go func() {
		_, _, err := r.registry.refresh(ctx, RefresherDeps{Horizon: r.horizon, Build: r.build},
			refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}, r.seeds, nil)
		passed <- err
	}()
	// waiting is the backend that waits for the held eval lock, and whether it holds a transaction
	// id, a row it wrote, while it waits.
	type waiting struct {
		pid uint32
		xid bool
	}
	waiter := testwait.Until(t, "the pass to wait for "+logged+"'s lock", func() (waiting, bool) {
		select {
		case err := <-passed:
			t.Fatalf("the pass answered (%v) while %s's lock was held, want it to wait for the lock to fold the first paint of PIER's own cut", err, logged)
		default:
		}
		var w waiting
		if err := holder.QueryRow(ctx, `
			select coalesce((select pid from pg_locks where locktype = 'advisory' and not granted and pg_backend_pid() = any(pg_blocking_pids(pid)) limit 1), 0)
		`).Scan(&w.pid); err != nil {
			t.Fatalf("read the waits for %s's lock: %v", logged, err)
		}
		if w.pid == 0 {
			return w, false
		}
		if err := holder.QueryRow(ctx, `select exists (select 1 from pg_locks where pid = $1 and locktype = 'transactionid' and granted)`, w.pid).Scan(&w.xid); err != nil {
			t.Fatalf("read the transaction id of the wait for %s's lock: %v", logged, err)
		}
		return w, true
	})
	if published, err := PublishedDefaultIn(ctx, pool, "PIER"); err != nil || published != quayRepo {
		t.Errorf("while the pass waits for %s's lock PIER publishes %q as its default (%v), want %s: the commit of the move waits for the lock", logged, published, err, quayRepo)
	}
	if waiter.xid {
		t.Errorf("the pass waits for %s's lock holding a transaction id, a row it locked before the eval's lock", logged)
	}
	for cut, mark := range r.staleMarks(t) {
		owed := cut == own
		if owed && (mark.generation != 2 || mark.ready != nil || mark.evals != nil) || !owed && (mark.ready == nil || !mark.ready.Equal(through)) {
			t.Errorf("while the pass waits for %s's lock %s's cut %q is marked at %s, want its first paint owed %t", logged, cut.project, cut.repo, mark, owed)
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let go of %s's lock: %v", logged, err)
	}
	if err := testwait.Receive(t, "the pass", passed); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the pass left the cuts %v marked, want every marked cut folded to its end", marks)
	}
	expectTruth(t, "once the pass folded every marked cut", r.storedFigures(t), r.truthFigures(t))
}

// The deploy repositories whose judging project moves (judgingMoves) are judged by the projects'
// delivery settings as saved, whose sources name the repositories the delivery routes read the
// judging evals of (DeployRepoEvals), whichever rule the day rows stand built under (eval_days_rules,
// which lags the settings until the refresher's rewrite of a settings change commits): the base's
// judge by delivery_settings.deploy_repo over the singleton, carried to each source's repository.
// ALPHA's one source names no repository, so it reads ALPHA's default, acme/next, which ALPHA
// holds and publishes none of yet, while the rows stand recorded under acme/before for ALPHA: the
// page reads acme/next as the repository whose judge the move moves to ALPHA, and its commit logs
// the one change that marks the delivery views those evals judge (delivery.JudgingMoved). Red where
// the page judges by the rules the rows stand built under: it then moves no judge of acme/next, and
// logs no such change.
func TestAMoveJudgesByTheDeployRepositoryTheDeliverySettingsName(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	r.exec(t, "give ALPHA acme/next as its default", `
		insert into projects (key, name, default_assignee, default_repo) values ('ALPHA', 'Alpha', 'alice', 'acme/next')`)
	putSources(t, pool, "ALPHA", []string{"dev-ana"}, deliverytest.SourceOf(delivery.DeliverySettings{
		DeployRepo: "acme/next", DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{"production / apply"},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
	}, nil))
	r.exec(t, "stand ALPHA's rows built under another deploy repository", `
		insert into eval_days_rules (repo, project_key, production_job_names) values ('acme/before', 'ALPHA', '{production / apply}')`)
	work := &pageWork{}
	if err := r.registry.newRefresh(r.build, r.now, r.batch).syncDefaults(ctx, work); err != nil || work.defaults == nil {
		t.Fatalf("the page reads no move of the defaults (%v)", err)
	}
	if !slices.Equal(work.defaults.judging, []string{"acme/next"}) {
		t.Errorf("the move moves the judges of %v, want acme/next's alone, from none to ALPHA", work.defaults.judging)
	}
	seq := r.latestSeq(t)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		before, err := readSelection(ctx, tx)
		if err != nil {
			return err
		}
		_, err = publishDefaults(ctx, tx, r.build, nil, work.defaults, before, nil)
		return err
	}); err != nil {
		t.Fatalf("commit the move: %v", err)
	}
	if _, err := delivery.DrainPending(ctx, pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(ctx, pool, seq, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", seq, err)
	}
	judged := 0
	for _, change := range changes {
		if delivery.JudgingMoved(change) {
			judged++
		}
	}
	if judged != 1 {
		t.Errorf("the commit of the move logged %d changes that mark the delivery views its judge's evals judge, want one", judged)
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
	t.Parallel()
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

// sourceReaders is, by repository, the projects that read each of repos' source key (sourceKey) as
// readersOf answers them, each with whether eval_project_owners reads the key through the
// repository, `<project> via=<via>` joined by ", ", and "" where none reads it.
func (r *refresherRig) sourceReaders(t *testing.T, repos ...string) map[string]string {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	keys := make([]string, len(repos))
	for i, repo := range repos {
		keys[i] = sourceKey(repo)
	}
	readers, err := readersOf(ctx, pool, keys)
	if err != nil {
		t.Fatalf("read the readers of %v: %v", keys, err)
	}
	answers := map[string]string{}
	for i, repo := range repos {
		var read []string
		for _, project := range readers[keys[i]] {
			var via bool
			if err := pool.QueryRow(ctx, `select via from eval_project_owners where owner = $1 and project_key = $2`, keys[i], project).Scan(&via); err != nil {
				t.Fatalf("read whether %s reads %s through the repository: %v", project, keys[i], err)
			}
			read = append(read, fmt.Sprintf("%s via=%t", project, via))
		}
		answers[repo] = strings.Join(read, ", ")
	}
	return answers
}

// sourceProjects is, by repository, the project whose deploy source reads each of repos as a pass
// reads the saved settings (delivery.DeployRules, a repository compared without case), none where
// no source reads it: the reader the commit of the pass gives the repository's source key.
func (r *refresherRig) sourceProjects(t *testing.T, repos ...string) map[string]string {
	t.Helper()
	projects, err := delivery.ListProjectSettings(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	rules := delivery.DeployRules(projects)
	read := map[string]string{}
	for _, repo := range repos {
		if at := slices.IndexFunc(rules, func(rule delivery.DeliverySettings) bool { return strings.EqualFold(rule.DeployRepo, repo) }); at >= 0 {
			read[repo] = rules[at].Project
		}
	}
	return read
}

// recordSourceKeys records the source key of each of repos (recordOwners), as the first write of a
// row of one records it, so its readers are read whether or not a row of it is written yet.
func (r *refresherRig) recordSourceKeys(t *testing.T, repos ...string) {
	t.Helper()
	ctx := t.Context()
	keys := make([]string, len(repos))
	for i, repo := range repos {
		keys[i] = sourceKey(repo)
	}
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := recordOwners(ctx, tx, keys); err != nil {
		t.Fatalf("record the source keys %v: %v", keys, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// The readers of a source key (dispatch://INFRA-978, R12; plan T19 path 1): over the settled
// three-source history, the reader of each repository's source key (readersOf, through
// eval_project_owners) is the project the saved settings say a deploy source of reads the
// repository (delivery.DeployRules), through the repository, and no other: ALPHA's of acme/alpha-app
// and acme/alpha-ops, OMEGA's of acme/omega-app, none of acme/alpha-next, and a key that spells
// acme/alpha-ops in capitals is read alike. Once ALPHA's source on acme/alpha-ops is saved on
// acme/alpha-next, the readers stand as they were until the pass that records the move commits,
// and then agree with the saved settings again: acme/alpha-next's key is ALPHA's, and acme/alpha-ops'
// no project's. Red where no arm of eval_project_owners reads a source key: no key has a reader.
func TestTheReadersOfASourceKeyAreTheProjectWhoseSourceReadsItsRepository(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	upper := strings.ToUpper(alphaOps)
	repos := []string{alphaApp, alphaOps, upper, omegaApp, alphaNext}
	r.recordSourceKeys(t, repos...)
	check := func(what string, want map[string]string) {
		t.Helper()
		got := r.sourceReaders(t, repos...)
		for _, repo := range repos {
			if got[repo] != want[repo] {
				t.Errorf("%s: the source key of %s is read by [%s], want [%s]", what, repo, got[repo], want[repo])
			}
		}
	}
	// agree checks that the view's readers are what the saved settings give each repository.
	agree := func(what string) {
		t.Helper()
		want := map[string]string{}
		for repo, project := range r.sourceProjects(t, repos...) {
			want[repo] = project + " via=true"
		}
		check(what+", against the saved settings", want)
	}
	settled := map[string]string{alphaApp: "ALPHA via=true", alphaOps: "ALPHA via=true", upper: "ALPHA via=true", omegaApp: "OMEGA via=true"}
	check("the settled history", settled)
	agree("the settled history")
	putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"},
		deliverytest.SourceOf(ruleOfJob(alphaJob), nil), deliverytest.SourceOf(ruleOfJob(opsJob), new(alphaNext)))
	check("after the save that moves ALPHA's source, before the pass", settled)
	r.settle(t)
	check("after the pass", map[string]string{alphaApp: "ALPHA via=true", omegaApp: "OMEGA via=true", alphaNext: "ALPHA via=true"})
	agree("after the pass")
}

// releaseBeforesOwners is eval_project_owners as the release before states it (migration
// 20261117100006): a project's own key, a plain key's readers by the published defaults, and a key
// that carries lists read through eval_owner_readers alone.
const releaseBeforesOwners = `
	select key as project_key, key as owner, false as via from projects
	union all
	select r.project_key, o.owner, true
	from eval_project_repos r join eval_day_owners o on lower(o.repo) = lower(r.repo)
	where o.lists = '{}' and not (r.project_key = any(o.sharers))
	union all
	select reader.project_key, o.owner, reader.via
	from eval_day_owners o cross join lateral eval_owner_readers(o.repo, o.sharers, o.lists) as reader
	where o.lists <> '{}'`

// A source key under the release before's rule (plan T19 path 1; cmd/dispatch/README.md, "Two
// builds at once"): the key carries a list, '{>~}' in eval_day_owners, so the release before, whose
// view reads such a key through eval_owner_readers alone, reads it by no project, whose terminal '~'
// is no project's key, and neither does that function. Over the settled three-source history,
// acme/alpha-app's source key, which this release's view gives ALPHA through the repository, has no
// reader under the release before's view, where a key of the same repository whose list names ALPHA
// as a condition (ALPHA's default being acme/alpha-app) is ALPHA's under both. Red where this
// release's view gives the source key no reader either.
func TestASourceKeyHasNoReaderUnderTheReleaseBeforesRule(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	r.seedSources(t)
	r.settle(t)
	source, condition := sourceKey(alphaApp), OwnerKey(alphaApp, nil, []attributionList{{conditions: []string{"ALPHA"}}})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := recordOwners(ctx, tx, []string{source, condition}); err != nil {
		t.Fatalf("record the keys: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var lists []string
	if err := pool.QueryRow(ctx, `select lists from eval_day_owners where owner = $1`, source).Scan(&lists); err != nil {
		t.Fatalf("read the lists of %s: %v", source, err)
	}
	if !slices.Equal(lists, []string{">~"}) {
		t.Fatalf("eval_day_owners holds the lists %v of %s, want [>~]: a key with no list is a plain key, which the release before reads by default", lists, source)
	}
	readers := func(view, key string) string {
		t.Helper()
		var read string
		if err := pool.QueryRow(ctx, `
			select coalesce(string_agg(project_key || ' via=' || via, ', ' order by project_key), '') from (`+view+`) as readers where owner = $1
		`, key).Scan(&read); err != nil {
			t.Fatalf("read the readers of %s: %v", key, err)
		}
		return read
	}
	for _, c := range []struct{ name, view, key, want string }{
		{"the release before's view", releaseBeforesOwners, source, ""},
		{"the release before's view", releaseBeforesOwners, condition, "ALPHA via=false"},
		{"this release's view", `select * from eval_project_owners`, source, "ALPHA via=true"},
		{"this release's view", `select * from eval_project_owners`, condition, "ALPHA via=false"},
	} {
		if got := readers(c.view, c.key); got != c.want {
			t.Errorf("%s gives %s the readers [%s], want [%s]", c.name, c.key, got, c.want)
		}
	}
	if n := r.count(t, `
		select count(*) from eval_day_owners o cross join lateral eval_owner_readers(o.repo, o.sharers, o.lists) where o.owner = $1
	`, source); n != 0 {
		t.Errorf("eval_owner_readers answers %d readers of %s, want none", n, source)
	}
}

// foldedReads is, by project, shape and eval id, the fold of each of projects' day rows over each
// of shapes as the eval list folds them (listValues' fold of DayWindowIn's rows, over the deploy
// repositories recorded with the rows), with each series' version time left out, which a second
// database stamps with its own seed's moment: what a second database whose whole walk ran under the
// same settings answers alike.
func (r *refresherRig) foldedReads(t *testing.T, projects []string, shapes []readShape) map[string]string {
	t.Helper()
	reads := map[string]string{}
	for _, project := range projects {
		for _, shape := range shapes {
			state := r.readListState(t, project, nil, shape.window, shape.compare)
			for i := range state.entries {
				for j := range state.entries[i].Series {
					state.entries[i].Series[j].CreatedAt = time.Time{}
				}
			}
			_, folded, _ := state.values(t, shape.window, &shape.compare)
			for id, values := range folded {
				reads[shape.name+" "+id] = values
			}
		}
	}
	return reads
}

// ruleRecords is each repository eval_days_rules records, `<project or none> since <kept_since or
// none>`, by repository.
func (r *refresherRig) ruleRecords(t *testing.T) map[string]string {
	t.Helper()
	return r.xmins(t, `select repo, coalesce(project_key, 'none') || ' since ' || coalesce(kept_since::text, 'none') from eval_days_rules`)
}

// A source moved away from its repository and back (dispatch://INFRA-978, R12 and R14; plan T19
// path 1): over the three-source history, ALPHA's source on acme/alpha-ops is saved on
// acme/alpha-next, which holds runs of its own, and back, once so the rows of each stand built under
// its rule (eval_days_rules), and then again. Each repository keeps its rows and its rule once no
// source reads it (the rule with no project, its start kept: refresh.keptRepos keeps every
// repository that holds a recorded rule and delivery runs), and the source key of each stays
// recorded (eval_day_owners), where the delivery walk reads its owners (readOwners). So the second
// move and the move back each write and delete no day row and no member: a pass records the
// selection alone. ALPHA's and OMEGA's reads, before, between and after, fold to what a second
// database answers whose whole walk ran under the same settings, and ALPHA's deploy-frequency counts
// acme/alpha-ops' deploy 2001 before and after, and no deploy of acme/alpha-next, none of whose runs
// ran the source's production job, between. Red where a source leaving a repository deletes its
// rows and its rule, and one reaching it builds them again.
func TestASourceMoveAwayAndBackWritesNoDayRow(t *testing.T) {
	t.Parallel()
	onOps := []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(alphaJob), nil), deliverytest.SourceOf(ruleOfJob(opsJob), new(alphaOps))}
	onNext := []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(alphaJob), nil), deliverytest.SourceOf(ruleOfJob(opsJob), new(alphaNext))}
	projects, shapes := []string{"ALPHA", "OMEGA"}, slices.Concat(moveShapes(), pastShapes())
	// walked is the reads of a second database whose whole walk ran with ALPHA's sources.
	walked := func(sources []delivery.SourceInput) map[string]string {
		w := newRefresherRig(t)
		w.seedSources(t)
		putSources(t, w.database.Pool, "ALPHA", []string{"dev-ana"}, sources...)
		w.settle(t)
		return w.foldedReads(t, projects, shapes)
	}
	walks := map[string]map[string]string{"acme/alpha-ops": walked(onOps), "acme/alpha-next": walked(onNext)}
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	for _, sources := range [][]delivery.SourceInput{onNext, onOps} {
		putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"}, sources...)
		r.settle(t)
	}
	owners, err := readOwners(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{alphaOps, alphaNext} {
		if !slices.Contains(owners, sourceKey(repo)) {
			t.Errorf("the owners the delivery walk reads are %v, want %s's source key among them", owners, repo)
		}
	}
	show := func(xmin string) string { return "written by " + xmin }
	expect := func(what, reading string, deploys defaultTally, records map[string]string) {
		t.Helper()
		if got := r.ruleRecords(t); !maps.Equal(got, records) {
			t.Errorf("%s: eval_days_rules records %v, want %v", what, got, records)
		}
		if got := r.deploysOn(t, "ALPHA", sourcesDay); got != deploys {
			t.Errorf("%s: ALPHA's deploy-frequency counts %d deploys, %d through a repository; want %d and %d", what, got.n, got.via, deploys.n, deploys.via)
		}
		if differ := sameValues(r.foldedReads(t, projects, shapes), walks[reading], func(values string) string { return values }); len(differ) > 0 {
			t.Errorf("%s: the reads differ from those of a whole walk with ALPHA's source on %s, first ones:\n\t%s", what, reading, strings.Join(differ, "\n\t"))
		}
	}
	since := sourcesDay.Format(time.DateOnly)
	onOpsRecords := map[string]string{alphaApp: "ALPHA since " + since, alphaOps: "ALPHA since " + since, alphaNext: "none since " + since,
		omegaApp: "OMEGA since " + since, EventsRepo("OMEGA"): "OMEGA since none"}
	onNextRecords := maps.Clone(onOpsRecords)
	onNextRecords[alphaOps], onNextRecords[alphaNext] = "none since "+since, "ALPHA since "+since
	expect("before the move", alphaOps, defaultTally{n: 2, via: 2}, onOpsRecords)
	for _, move := range []struct {
		name, repo string
		sources    []delivery.SourceInput
		deploys    defaultTally
		records    map[string]string
	}{
		{"the move to " + alphaNext, alphaNext, onNext, defaultTally{n: 1, via: 1}, onNextRecords},
		{"the move back to " + alphaOps, alphaOps, onOps, defaultTally{n: 2, via: 2}, onOpsRecords},
	} {
		rows, members := r.xmins(t, dayRowXmins), r.xmins(t, memberXmins)
		putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"}, move.sources...)
		r.settle(t)
		if differ := sameValues(r.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
			t.Errorf("%s wrote or deleted day rows, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		if differ := sameValues(r.xmins(t, memberXmins), members, show); len(differ) > 0 {
			t.Errorf("%s wrote or deleted members, first ones:\n\t%s", move.name, strings.Join(differ, "\n\t"))
		}
		expect("after "+move.name, move.repo, move.deploys, move.records)
	}
}

// The items project's deploy evals count on day (CountedIn, of the delivery family, over the deploy
// repositories recorded with the rows), by row key.
func (r *refresherRig) deployItems(t *testing.T, project string, day time.Time) []string {
	t.Helper()
	repos, err := DeployReposIn(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	counted, err := CountedIn(t.Context(), r.database.Pool, project, KindDelivery, repos.Of(project), nil, Range{From: day, To: day.AddDate(0, 0, 1)})
	if err != nil {
		t.Fatalf("read the items %s counts on %s: %v", project, day.Format(time.DateOnly), err)
	}
	var keys []string
	for _, item := range counted {
		keys = append(keys, item.RowKey)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// A repository's source moved to another project (dispatch://INFRA-978, R12 and R14; plan T19 path
// 1): over the three-source history, acme/alpha-ops' source moves from ALPHA to OMEGA under the same
// rule. Its deploy 2001, which ships no pull request, is a row of the repository's source key, which
// the pass gives OMEGA as its reader (eval_project_owners, among the keys a read of OMEGA folds) and
// takes from ALPHA: ALPHA's deploy-frequency counts 2 deploys before and 1 after, OMEGA's 1 and then
// 2, each through its repository, and the items each counts follow. The move writes and deletes no
// day row and no member. Red where such a deploy is a row of its source's project, which the move
// rewrites.
func TestAnOwnerChangeOfARepositoryWritesNoDayRow(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	deploy, key := RunMember(alphaOps, 2001), sourceKey(alphaOps)
	check := func(what, reader, other string, deploys map[string]defaultTally) {
		t.Helper()
		for _, project := range []string{"ALPHA", "OMEGA"} {
			if got := r.deploysOn(t, project, sourcesDay); got != deploys[project] {
				t.Errorf("%s: %s's deploy-frequency counts %d deploys, %d through a repository; want %d and %d",
					what, project, got.n, got.via, deploys[project].n, deploys[project].via)
			}
		}
		for project, reads := range map[string]bool{reader: true, other: false} {
			if got := r.count(t, `select count(*) from eval_project_owners where project_key = $1 and owner = $2`, project, key) == 1; got != reads {
				t.Errorf("%s: %s reads %s %t, want %t", what, project, key, got, reads)
			}
			if got := slices.Contains(r.deployItems(t, project, sourcesDay), deploy); got != reads {
				t.Errorf("%s: %s counts %s among its deploys %t, want %t", what, project, deploy, got, reads)
			}
		}
	}
	check("before the move", "ALPHA", "OMEGA", map[string]defaultTally{"ALPHA": {n: 2, via: 2}, "OMEGA": {n: 1, via: 1}})
	rows, members := r.xmins(t, dayRowXmins), r.xmins(t, memberXmins)
	authors := []string{"dev-ana"}
	putSources(t, r.database.Pool, "ALPHA", authors, deliverytest.SourceOf(ruleOfJob(alphaJob), nil))
	putSources(t, r.database.Pool, "OMEGA", authors, deliverytest.SourceOf(ruleOfJob(omegaJob), nil), deliverytest.SourceOf(ruleOfJob(opsJob), new(alphaOps)), eventsSource)
	r.settle(t)
	show := func(xmin string) string { return "written by " + xmin }
	if differ := sameValues(r.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
		t.Errorf("the move wrote or deleted day rows, first ones:\n\t%s", strings.Join(differ, "\n\t"))
	}
	if differ := sameValues(r.xmins(t, memberXmins), members, show); len(differ) > 0 {
		t.Errorf("the move wrote or deleted members, first ones:\n\t%s", strings.Join(differ, "\n\t"))
	}
	repos, err := DeployReposIn(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := repos.Owned("OMEGA"); !slices.Equal(got, []string{alphaOps, omegaApp}) {
		t.Errorf("after the move OMEGA's sources stand recorded under %v, want %s and %s", got, alphaOps, omegaApp)
	}
	check("after the move", "OMEGA", "ALPHA", map[string]defaultTally{"ALPHA": {n: 1, via: 1}, "OMEGA": {n: 2, via: 2}})
}

// A move of the default repository a source naming no repository reads (dispatch://INFRA-978, R12
// and R14; plan T19 path 1): over the three-source history, ALPHA's first source names none, so it
// reads ALPHA's default. That default moves to acme/alpha-next and back once, so the rows of each
// stand built under its rule, and then again: each move is a selection the pass records, with the
// published default, and writes and deletes no day row and no member, and ALPHA's deploy-frequency
// counts acme/alpha-next's 4001 and 4002 with acme/alpha-ops' 2001 while its default is
// acme/alpha-next, and acme/alpha-app's 1001 with 2001 otherwise, each through its repository. Red
// where the repository the source leaves loses its rows, and the one it reads builds them again.
func TestADefaultMoveUnderASourceNamingNoRepositoryWritesNoDayRow(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	move := func(repo string) {
		t.Helper()
		r.exec(t, "move ALPHA's default repository", `update projects set default_repo = $1 where key = 'ALPHA'`, repo)
		r.settle(t)
		if published, err := PublishedDefaultIn(t.Context(), r.database.Pool, "ALPHA"); err != nil || published != repo {
			t.Fatalf("ALPHA publishes %q as its default (%v), want %s", published, err, repo)
		}
	}
	move(alphaNext)
	move(alphaApp)
	show := func(xmin string) string { return "written by " + xmin }
	for _, step := range []struct {
		repo    string
		deploys defaultTally
		owned   []string
	}{
		{alphaNext, defaultTally{n: 3, via: 3}, []string{alphaNext, alphaOps}},
		{alphaApp, defaultTally{n: 2, via: 2}, []string{alphaApp, alphaOps}},
	} {
		rows, members := r.xmins(t, dayRowXmins), r.xmins(t, memberXmins)
		move(step.repo)
		if differ := sameValues(r.xmins(t, dayRowXmins), rows, show); len(differ) > 0 {
			t.Errorf("the move to %s wrote or deleted day rows, first ones:\n\t%s", step.repo, strings.Join(differ, "\n\t"))
		}
		if differ := sameValues(r.xmins(t, memberXmins), members, show); len(differ) > 0 {
			t.Errorf("the move to %s wrote or deleted members, first ones:\n\t%s", step.repo, strings.Join(differ, "\n\t"))
		}
		repos, err := DeployReposIn(t.Context(), r.database.Pool)
		if err != nil {
			t.Fatal(err)
		}
		if got := repos.Owned("ALPHA"); !slices.Equal(got, step.owned) {
			t.Errorf("after the move to %s ALPHA's sources stand recorded under %v, want %v", step.repo, got, step.owned)
		}
		if got := r.deploysOn(t, "ALPHA", sourcesDay); got != step.deploys {
			t.Errorf("after the move to %s ALPHA's deploy-frequency counts %d deploys, %d through a repository; want %d and %d",
				step.repo, got.n, got.via, step.deploys.n, step.deploys.via)
		}
	}
}

// opsSources is ALPHA's two GitHub sources of the three-source history with its second on repo:
// the first names no repository (acme/alpha-app, its default, under alphaJob), the second names
// repo, under opsJob.
func opsSources(repo string) []delivery.SourceInput {
	return []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(alphaJob), nil), deliverytest.SourceOf(ruleOfJob(opsJob), new(repo))}
}

// seedOpsMoveBack writes the three-source history (seedSources) with an issue of OMEGA's that owns
// acme/alpha-ops, OMEGA-2, and a pull request of acme/alpha-ops that names it, merged on sourcesDay
// before acme/alpha-ops' deploy 2001, which ships it, so 2001 counts for OMEGA in its own rows under
// acme/alpha-ops' deploy repository (a deploy counts for the projects of the pull requests it ships,
// dispatch://INFRA-978 A8), a repository whose source is ALPHA's; and a posted deploy of OMEGA, so
// OMEGA's read selects its events repository; and builds it whole. ALPHA's second source is then
// saved on acme/alpha-next, whose first rule builds its rows in the commit that records it, and the
// refresher settles; and it is saved back on acme/alpha-ops, which kept its rows and its rule, and
// which no pass has read yet.
func (r *refresherRig) seedOpsMoveBack(t *testing.T) {
	t.Helper()
	r.seedSources(t)
	r.exec(t, "OMEGA's issue of acme/alpha-ops", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('OMEGA-2', 'OMEGA', 2, 'Omega work shipped through ops', 'in_progress', '00000002', $1, '{"kind":"system","id":"sources-test"}', $2)`,
		alphaOps, sourcesDay.AddDate(0, 0, -1))
	if err := delivery.UpsertPullRequest(t.Context(), r.database.Pool, delivery.DeliveryPullRequest{
		Repo: alphaOps, Number: 7, Title: "OMEGA-2: ship through ops", URL: "https://github.com/" + alphaOps + "/pull/7",
		Author: "dev-ana", CreatedAt: new(sourcesDay.Add(8 * time.Hour)), MergedAt: new(sourcesDay.Add(9 * time.Hour)), Sessions: []string{},
		Attribution: &delivery.AttributionInputs{TitleKeys: []string{"OMEGA-2"}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
	}); err != nil {
		t.Fatalf("write OMEGA-2's pull request of %s: %v", alphaOps, err)
	}
	r.post(t, "OMEGA", "release-1", sourcesDay.Add(15*time.Hour))
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"}, opsSources(alphaNext)...)
	r.settle(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Fatalf("the passes after ALPHA's move to %s settled with the cuts %v stale", alphaNext, marks)
	}
	putSources(t, r.database.Pool, "ALPHA", []string{"dev-ana"}, opsSources(alphaOps)...)
}

// sourceState is what the evals' reads select as the store holds it: the deploy repositories
// recorded with the rows (DeployReposIn) and the owner keys each project reads, with whether
// through the repository (readOwnerPairs).
type sourceState struct {
	repos  *DeployRepos
	owners map[string]map[string]bool
}

// sourceStateNow is the store's sourceState.
func (r *refresherRig) sourceStateNow(t *testing.T) sourceState {
	t.Helper()
	repos, err := DeployReposIn(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := readOwnerPairs(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return sourceState{repos: repos, owners: owners}
}

// marksOfSourceMove is the marks a commit that moves what the evals' reads select from before to
// after names, by cut, read from every stored day row as the commit reads them, fresh being the
// repositories whose every row the commit wrote (a repository's first rule or first runs): of the
// cuts of each owner key a project gains, loses or reads otherwise, those marksOfMove names, but
// for a fresh repository's source key; of each cut that holds a row of a deploy repository a
// project's read gains or loses (DeployRepos.Of, a fresh one aside) among the rows the project
// reads (its own and its owner keys'), its eval of each such row's slug that reads the deploy
// repository; and, of each project either of those marks, its failure evals in each cut their
// rows stand in. Each in byte order, and no cut that holds no row the commit moved.
func (r *refresherRig) marksOfSourceMove(t *testing.T, before, after sourceState, fresh ...string) map[staleCut][]string {
	t.Helper()
	skip := func(pairs map[string]map[string]bool) map[string]map[string]bool {
		kept := map[string]map[string]bool{}
		for project, owners := range pairs {
			kept[project] = maps.Clone(owners)
			for _, repo := range fresh {
				delete(kept[project], sourceKey(repo))
			}
		}
		return kept
	}
	named := map[staleCut]map[string]bool{}
	touched := map[string]bool{}
	note := func(cut staleCut, evalID string) {
		if named[cut] == nil {
			named[cut] = map[string]bool{}
		}
		named[cut][evalID] = true
		touched[cut.project] = true
	}
	for cut, evals := range r.marksOfMove(t, nil, skip(before.owners), skip(after.owners)) {
		for _, evalID := range evals {
			note(cut, evalID)
		}
	}
	projects, err := r.registry.Projects(t.Context())
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	rows := r.dayRows(t)
	for _, project := range projects {
		moved := map[string]bool{}
		for _, repo := range slices.Concat(before.repos.Of(project), after.repos.Of(project)) {
			moved[repo] = slices.Contains(before.repos.Of(project), repo) != slices.Contains(after.repos.Of(project), repo) && !slices.Contains(fresh, repo)
		}
		keys := map[string]bool{project: true}
		for _, owners := range []map[string]bool{before.owners[project], after.owners[project]} {
			for owner := range owners {
				keys[owner] = true
			}
		}
		for _, row := range rows {
			slug := slugOf(row.ProjectKey, row.EvalID)
			if keys[row.ProjectKey] && moved[row.DeployRepo] && row.Repo != legionfacts.DefaultRepo && readsDeployRepo(slug) {
				note(staleCut{project: project, repo: row.Repo}, EvalID(project, slug))
			}
		}
	}
	for _, row := range rows {
		if touched[row.ProjectKey] && slices.Contains(failureDaySlugs, slugOf(row.ProjectKey, row.EvalID)) {
			note(staleCut{project: row.ProjectKey, repo: row.Repo}, row.EvalID)
		}
	}
	marks := make(map[staleCut][]string, len(named))
	for cut, evals := range named {
		marks[cut] = slices.Sorted(maps.Keys(evals))
	}
	return marks
}

// expectSelectionCommit checks that the page that published a source move wrote the rules of repos
// it records (eval_days_rules), every stale mark and every stored figure written since figures
// (figureXmins, read before the page) in the transaction that moved the refresher's change cursor:
// the page's one commit (commitPage), so a read sees all of them or none. It answers how many
// stored figures that transaction wrote.
func (r *refresherRig) expectSelectionCommit(t *testing.T, what string, figures map[string]string, repos ...string) int {
	t.Helper()
	commit := r.xmins(t, `select name, xmin::text from eval_days_cursor where name = '`+changesCursor+`'`)[changesCursor]
	if commit == "" {
		t.Fatalf("%s: no page moved the refresher's change cursor", what)
	}
	for name, xmin := range r.xmins(t, `select 'the rule of ' || repo, xmin::text from eval_days_rules where repo = any('{`+strings.Join(repos, ",")+`}')
		union all
		select 'the mark of ' || project_key || '''s cut ' || quote_literal(repo), xmin::text from eval_figure_cuts_stale`) {
		if xmin != commit {
			t.Errorf("%s: %s was written by transaction %s, and the page's commit is %s", what, name, xmin, commit)
		}
	}
	written := 0
	for name, xmin := range r.xmins(t, figureXmins) {
		if figures[name] == xmin {
			continue
		}
		written++
		if xmin != commit {
			t.Errorf("%s: the stored figure %s was written by transaction %s, and the page's commit is %s", what, name, xmin, commit)
		}
	}
	return written
}

// publishOpsMoveBack writes seedOpsMoveBack's history and has a page alone publish the move of
// ALPHA's source back to acme/alpha-ops (pageAlone), the pass stopping before the rest of its work,
// so the refold of the cuts the move marks has not begun. It answers the marks that move calls for
// (marksOfSourceMove, read from the rows around the page), and checks the fixture: the move moves
// OMEGA's own rows of acme/alpha-ops, in its own cut and in its cut of acme/alpha-ops, a cut of no
// repository OMEGA's own sources read, and ALPHA's cut of acme/alpha-ops, whose source key it
// reads again; and it moves no other cut of OMEGA's, its events repository's read standing as it
// stood, and not ALPHA's cut of acme/alpha-app.
func (r *refresherRig) publishOpsMoveBack(t *testing.T, figures *map[string]string) map[staleCut][]string {
	t.Helper()
	r.seedOpsMoveBack(t)
	before := r.sourceStateNow(t)
	if figures != nil {
		*figures = r.xmins(t, figureXmins)
	}
	r.pageAlone(t)
	want := r.marksOfSourceMove(t, before, r.sourceStateNow(t))
	for _, cut := range []staleCut{{project: "OMEGA"}, {project: "OMEGA", repo: alphaOps}, {project: "ALPHA", repo: alphaOps}} {
		if len(want[cut]) == 0 {
			t.Fatalf("the move of ALPHA's source back to %s moves no row of %s's cut %q (%v), and the check needs it", alphaOps, cut.project, cut.repo, want)
		}
	}
	for cut := range want {
		if cut.project == "OMEGA" && cut.repo != "" && cut.repo != alphaOps || cut == (staleCut{project: "ALPHA", repo: alphaApp}) {
			t.Fatalf("the move of ALPHA's source back to %s moves rows of %s's cut %q (%v), and the check needs it untouched", alphaOps, cut.project, cut.repo, want)
		}
	}
	return want
}

// TestASourceMoveMarksOnlyTheCutsItTouches (dispatch://INFRA-978, R14; plan T19 path 3): over
// seedOpsMoveBack's history, ALPHA's second source moved to acme/alpha-next and is saved back on
// acme/alpha-ops, and a page alone publishes the move (publishOpsMoveBack). Its commit marks stale
// the cuts the move moves rows of and no other, each naming the evals whose rows it moved there
// (marksOfSourceMove), with its first paint folded in the same commit: ALPHA's cuts of the source
// keys it reads again and no longer, and OMEGA's own cut and its cut of acme/alpha-ops, whose rows
// of 2001, which ships OMEGA-2's pull request, OMEGA's read of the deploy evals selects again,
// though no source of OMEGA's reads acme/alpha-ops: OMEGA's events repository and acme/omega-app
// move nothing. The rules, every mark and every stored figure the page wrote are of the transaction
// that moved the refresher's change cursor (expectSelectionCommit), and it wrote some figures.
// Every stored figure the move leaves other than the fold of the rows that stand is of an eval a
// mark names in its cut. A page that then reads a posted deploy of OMEGA, whose events repository
// its read selects before and after, marks nothing. Once the refresher settles no cut is marked
// and the stored figures are the fold of the rows that stand. Red where the page that records a
// source move marks no cut (the union cuts `*` then stand folded over the repositories the read
// selected before the move), where it marks the cuts of the requesting project alone, or every
// cut, and where its marks or first paints land in a transaction of their own.
func TestASourceMoveMarksOnlyTheCutsItTouches(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	var figures map[string]string
	want := r.publishOpsMoveBack(t, &figures)
	what := "the page that published ALPHA's move back to " + alphaOps
	if written := r.expectSelectionCommit(t, what, figures, alphaOps, alphaNext); written == 0 {
		t.Errorf("%s wrote no stored figure, and the check needs the first paints its commit folds", what)
	}
	r.expectMarks(t, what, want)
	marked := markedEvals(r.staleMarks(t))
	for cut, evals := range staleFiguresOf(t, r.storedFigures(t), r.truthFigures(t)) {
		for _, evalID := range slices.Sorted(maps.Keys(evals)) {
			if !slices.Contains(marked[cut], evalID) {
				t.Errorf("the move left %s's stored figures of %s's cut %q other than the fold of the rows that stand, and no mark names it", evalID, cut.project, cut.repo)
			}
		}
	}
	r.settle(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the passes after the move settled with the cuts %v stale", marks)
	}
	r.post(t, "OMEGA", "release-2", refresherToday.Add(2*time.Hour))
	r.pageAlone(t)
	if marks := r.staleMarks(t); len(marks) != 0 {
		t.Errorf("the page that read a posted deploy of OMEGA, whose events repository its read selects before and after, marked the cuts %v", marks)
	}
	r.settle(t)
	expectTruth(t, "once the refresher settled", r.storedFigures(t), r.truthFigures(t))
}

// TestAReadOfAMovedSourcesCutBeforeItsRefoldFoldsTheDayRows (dispatch://INFRA-978, R14; plan T19
// path 3): right after the page that publishes the move of ALPHA's source back to acme/alpha-ops
// (publishOpsMoveBack), the refold of the cuts it marks not begun, a read of the past (pastShapes)
// of each touched cut states each eval the move moved there from the fold of its day rows, and a
// first paint's read (moveShapes) states it from the stored figures its commit folded, each the
// fold of the rows that stand; a cut the move does not touch (OMEGA's acme/omega-app, ALPHA's
// acme/alpha-app) states every eval from the stored figures throughout. Red where the move leaves
// the cuts unmarked: a read of the past then states OMEGA's deploy evals, and ALPHA's, from union
// cuts still folded over the repositories the read selected before the move.
func TestAReadOfAMovedSourcesCutBeforeItsRefoldFoldsTheDayRows(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	want := r.publishOpsMoveBack(t, nil)
	never, always := func(staleCut) bool { return false }, func(staleCut) bool { return true }
	r.expectReads(t, "right after the page that published the move, of the past", want, readsOf(pastShapes()), never)
	r.expectReads(t, "right after the page that published the move, over a first paint", want, readsOf(moveShapes()), always)
	untouched := map[staleCut][]string{{project: "OMEGA", repo: omegaApp}: nil, {project: "ALPHA", repo: alphaApp}: nil}
	r.expectReads(t, "right after the page that published the move, of a cut it does not touch", untouched, readsOf(slices.Concat(moveShapes(), pastShapes())), always)
}

// TestTheRefoldOfAMovedSourceEndsWithEveryCutsFiguresEqualToItsDayRows (dispatch://INFRA-978,
// R14; plan T19 path 3): after the page that publishes the move of ALPHA's source back to
// acme/alpha-ops (publishOpsMoveBack), with batches of one end day (refresherRig.batch) and every
// pass run under an owed wake (owedPass), so each pass folds one batch, the refold of the cuts the
// move marked goes on pass after pass, each first paint read from the stored figures throughout,
// until no mark is left; then the stored figures of every cut, the union cuts `*` among them, are
// the fold of the rows that stand, and every read of each cut the move touched, of a first paint
// and of the past, states every eval from them. Red where the refold of a cut leaves its union cut
// out, or the marks are cleared before it is folded.
func TestTheRefoldOfAMovedSourceEndsWithEveryCutsFiguresEqualToItsDayRows(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	want := r.publishOpsMoveBack(t, nil)
	r.batch = 1
	always := func(staleCut) bool { return true }
	for passes := 0; len(r.staleMarks(t)) > 0; passes++ {
		if passes > 1000 {
			t.Fatalf("%d passes left the cuts %v stale", passes, r.staleMarks(t))
		}
		r.expectReads(t, fmt.Sprintf("%d passes after the move's commit, over a first paint", passes), want, readsOf(moveShapes()), always)
		r.owedPass(t, &gapBatch{})
	}
	expectTruth(t, "once the refold of the move was done", r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "once the refold of the move was done", want, readsOf(slices.Concat(moveShapes(), pastShapes())), always)
}

// TestTheFirstPaintOfAMovedSourceIsFoldedInItsCommit (dispatch://INFRA-978, R14; plan T19 path
// 3): the commit of the page that publishes the move of ALPHA's source back to acme/alpha-ops
// (publishOpsMoveBack) folds the first paint of every cut it marks, in the transaction that moves
// the refresher's change cursor (expectSelectionCommit), with no later batch of the refold: every
// stored row of each first paint is the fold of the rows that stand under the new selection
// (expectPaintTruth), and every read a first paint of each cut names (firstPaintReads, and
// windowBackReads one window back) states every eval from the stored figures, each the fold of its
// day rows. Red where the commit folds no first paint, or folds it in a transaction of its own:
// the reads then fold the day rows, or, served the stored figures anyway, state the union cuts as
// they were folded before the move.
func TestTheFirstPaintOfAMovedSourceIsFoldedInItsCommit(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	var figures map[string]string
	want := r.publishOpsMoveBack(t, &figures)
	through := refresherToday.AddDate(0, 0, FigureLead)
	r.expectSelectionCommit(t, "the page that published the move", figures, alphaOps, alphaNext)
	r.expectMarks(t, "the page that published the move", want)
	expectPaintTruth(t, "right after the move's commit", through, want, r.storedFigures(t), r.truthFigures(t))
	r.expectReads(t, "right after the move's commit", want, append(firstPaintReads(through), windowBackReads(through)...), func(staleCut) bool { return true })
}
