package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// registerReviewTime registers EVL's review-time eval, with one quantile series, through the
// register write.
func registerReviewTime(t *testing.T, handler http.Handler) {
	t.Helper()
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", map[string]any{
		"slug": "review-time", "name": "Review time", "series": []any{map[string]any{"series": "minutes", "agg": "quantile"}},
	}, "review-time-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register review-time: status %d, body %s", registered.Code, registered.Body.String())
	}
}

// TestTheVersionsPageTakesAnyOffset: the eval's read takes any offset the page parser accepts, up
// to the largest integer, and answers an offset past its last version with an empty page and the
// total, where adding the offset to the limit would overflow.
func TestTheVersionsPageTakesAnyOffset(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registerReviewTime(t, handler)
	for _, offset := range []string{"1", "9223372036854775807"} {
		detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/EVL/evals/review-time?offset="+offset)
		if detail.Total != 1 || len(detail.Items) != 0 {
			t.Errorf("offset %s answers %d items of %d, want none of 1", offset, len(detail.Items), detail.Total)
		}
	}
}

// TestTheEvalReadStatesItsVersionsOfOneSnapshot: the eval's read takes its definition, its series
// and its versions from one snapshot, so a register write that commits while the read runs is
// either wholly in its answer or wholly out of it, and the newest version on the page is the
// definition it states the window's values for. The write's second version is inserted while a
// transaction holds eval_series, which the read waits on between its first statement and the
// version list, and commits once the read waits.
func TestTheEvalReadStatesItsVersionsOfOneSnapshot(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registerReviewTime(t, handler)
	ctx := t.Context()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the writer: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table eval_series in access exclusive mode`); err != nil {
		t.Fatalf("hold eval_series: %v", err)
	}
	if _, err := holder.Exec(ctx, `
		insert into eval_versions (eval_id, version, project_key, slug, name, kind, source, written_via, written_by)
		values ('EVL/review-time', 2, 'EVL', 'review-time', 'Review time, renamed', 'experiment', 'logged', 'register',
		        '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("write version 2: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- evalRequest(t, handler, http.MethodGet, "/api/v1/projects/EVL/evals/review-time", nil, "")
	}()
	storetest.WaitForLockWaiters(t, holder, 1)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit version 2: %v", err)
	}
	response := <-answered
	if response.Code != http.StatusOK {
		t.Fatalf("GET the eval: status %d, body %s", response.Code, response.Body.String())
	}
	detail := decodeBody[evalDetailResponse](t, response)
	if len(detail.Items) == 0 || detail.Items[0].Definition.Version != detail.Total || detail.Items[0].Series == nil {
		var versions []int
		for _, item := range detail.Items {
			versions = append(versions, item.Definition.Version)
		}
		t.Fatalf("the read answers versions %v of %d, the newest stating values %t; want the newest version of its own snapshot stating them",
			versions, detail.Total, len(detail.Items) > 0 && detail.Items[0].Series != nil)
	}
	t.Logf("the read answered %d versions, the newest %d (%q) with its values", detail.Total, detail.Items[0].Definition.Version, detail.Items[0].Definition.Name)
}

// TestTheMedianRankLeavesOutAJobWithNoTimedResult: pr-ci's job items sorted by median rank only
// the jobs with a timed result, even where a register write set the job series' min_n to 0: a job
// whose day rows count a cancelled result and no timed one has no median and is left out, and the
// timed job is answered.
func TestTheMedianRankLeavesOutAJobWithNoTimedResult(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", map[string]any{
		"slug": "pr-ci", "series": []any{map[string]any{"series": "job:*", "min_n": 0}},
	}, "pr-ci-min-n")
	if registered.Code != http.StatusCreated {
		t.Fatalf("set pr-ci's job min_n to 0: status %d, body %s", registered.Code, registered.Body.String())
	}
	rule := deployRule("acme/web", "release / release")
	saveProjectRules(t, database, "EVL", rule)
	today := measures.Midnight(time.Now())
	// pr-ci reads the deploy repository: its rows are kept under it (eval_days.deploy_repo), and a
	// read folds those of the repositories the refresher published (publishDeployRepo), here EVL's
	// one source, acme/web.
	publishDeployRepo(t, database, "EVL", rule, today)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, samples, counts, built_by)
		values ('EVL', 'EVL/pr-ci', 'acme/web', 'job:lint', '', $1, false, 0, '{}', '{"cancelled": 1}', 'test'),
		       ('EVL', 'EVL/pr-ci', 'acme/web', 'job:unit', '', $1, false, 2, '{3, 5}', '{"success": 2}', 'test')
	`, today); err != nil {
		t.Fatalf("write the job rows: %v", err)
	}
	window := url.Values{"from": {today.Format(time.RFC3339)}, "to": {today.AddDate(0, 0, 1).Format(time.RFC3339)}}.Encode()
	items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/pr-ci/items?row_kind=job&sort=median&"+window)
	var jobs []string
	for _, row := range items.Items {
		jobs = append(jobs, row.RowKey)
	}
	if strings.Join(jobs, ",") != "unit" {
		t.Errorf("the median rank answers the jobs %v, want unit alone", jobs)
	}
}

// TestAnItemsLinkKeepsItsRepositoryAndItsRange: over the most days a read takes (84), starting at
// noon, and for two repositories, a day row of one repository links to that repository's items of
// its day, and pr-ci's job row links to both repositories' day rows of the job over the range as
// it was asked for; following each link answers the rows the row was read from, and not every
// repository's. A job row once linked to the range's whole days, 85 of them for a range that
// starts inside a day, which the items route refuses 400 INVALID_QUERY.
func TestAnItemsLinkKeepsItsRepositoryAndItsRange(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	rule := deployRule("acme/web", "release / release")
	saveProjectRules(t, database, "EVL", rule)
	day := measures.Midnight(time.Now()).AddDate(0, 0, -1)
	// deploy-frequency and pr-ci read the deploy repository: their rows are kept under it
	// (eval_days.deploy_repo), and a read folds those of the repositories the refresher published
	// (publishDeployRepo), here EVL's one source, acme/web, whose rows cut by acme/web and acme/api.
	publishDeployRepo(t, database, "EVL", rule, day)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, samples, counts, built_by)
		values ('EVL', 'EVL/deploy-frequency', 'acme/web', 'deploys', 'acme/web', $1, true, 2, 2, null, '{}', 'test'),
		       ('EVL', 'EVL/deploy-frequency', 'acme/web', 'deploys', 'acme/api', $1, true, 1, 1, null, '{}', 'test'),
		       ('EVL', 'EVL/pr-ci', 'acme/web', 'job:unit', 'acme/web', $1, false, 2, null, '{3, 5}', '{"success": 2}', 'test'),
		       ('EVL', 'EVL/pr-ci', 'acme/web', 'job:unit', 'acme/api', $1, false, 1, null, '{4}', '{"success": 1}', 'test')
	`, day); err != nil {
		t.Fatalf("write the repositories' rows: %v", err)
	}
	repos := []string{"acme/web", "acme/api"}
	to := day.AddDate(0, 0, 1).Add(12 * time.Hour)
	window := url.Values{"from": {to.AddDate(0, 0, -84).Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}, "repo": repos}.Encode()
	date := day.Format(time.DateOnly)
	for _, read := range []struct {
		path string
		// rows is each row the read answers, by its key, with the repositories its link names and
		// the keys of the day rows following the link answers.
		rows map[string][2][]string
	}{
		{"deploy-frequency/items?row_kind=day&", map[string][2][]string{
			"deploys@" + date + "@acme/web": {{"acme/web"}, {"deploys@" + date + "@acme/web"}},
			"deploys@" + date + "@acme/api": {{"acme/api"}, {"deploys@" + date + "@acme/api"}},
		}},
		{"pr-ci/items?row_kind=job&", map[string][2][]string{
			"unit": {repos, {"job:unit@" + date + "@acme/api", "job:unit@" + date + "@acme/web"}},
		}},
	} {
		items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/"+read.path+window)
		var keys []string
		for _, row := range items.Items {
			keys = append(keys, row.RowKey)
		}
		if slices.Sort(keys); !slices.Equal(keys, slices.Sorted(maps.Keys(read.rows))) {
			t.Fatalf("%s answers the rows %v, want %v", read.path, keys, slices.Sorted(maps.Keys(read.rows)))
		}
		for _, row := range items.Items {
			want := read.rows[row.RowKey]
			link, err := url.Parse(row.Href)
			if err != nil {
				t.Fatalf("%s row %s links to %q: %v", read.path, row.RowKey, row.Href, err)
			}
			if named := link.Query()["repo"]; !slices.Equal(named, want[0]) {
				t.Errorf("%s row %s links to %s, whose repo is %v; want %v", read.path, row.RowKey, row.Href, named, want[0])
			}
			followed := evalRequest(t, handler, http.MethodGet, row.Href, nil, "")
			if followed.Code != http.StatusOK {
				t.Errorf("%s row %s links to %s, which answers %d %s; want 200", read.path, row.RowKey, row.Href, followed.Code, followed.Body.String())
				continue
			}
			var behind []string
			for _, item := range decodeBody[evalItemsResponse](t, followed).Items {
				if item.RowKind == evalRowDay {
					behind = append(behind, item.RowKey)
				}
			}
			if slices.Sort(behind); !slices.Equal(behind, want[1]) {
				t.Errorf("%s row %s links to %s, which answers the day rows %v; want %v", read.path, row.RowKey, row.Href, behind, want[1])
			}
		}
	}
}

func TestEvalReadsResolveRepositorySpellingsBeforeReadingTheirRows(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	ctx, pool := t.Context(), database.Pool
	day := measures.Midnight(time.Now())
	if _, err := pool.Exec(ctx, `insert into eval_days_defaults (project_key, repo) values ('EVL', 'acme/web')`); err != nil {
		t.Fatal(err)
	}
	// pr-ci reads the deploy repository: its rows are kept under it (eval_days.deploy_repo), and a
	// read folds those of the repositories the refresher published (publishDeployRepo), here EVL's
	// one source, acme/web.
	rule := deployRule("acme/web", "release / release")
	saveProjectRules(t, database, "EVL", rule)
	publishDeployRepo(t, database, "EVL", rule, day)
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, samples, built_by)
		values ('EVL', 'EVL/legion-work-time', '', 'hours', '', $1, true, 2, 12, '{4,8}', 'test'),
		       ('EVL', 'EVL/legion-work-time', '', 'hours', 'Acme/Web', $1, true, 1, 4, '{4}', 'test'),
		       ('EVL', 'EVL/legion-work-time', '', 'hours', ':default', $1, true, 1, 8, '{8}', 'test'),
		       ('EVL', 'EVL/pr-ci', 'acme/web', 'job:unit', 'Acme/Web', $1, false, 2, 8, '{3,5}', 'test')`, day); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/projects/EVL/evals/legion-work-time"
	query := url.Values{"from": {day.Format(time.RFC3339)}, "to": {day.AddDate(0, 0, 1).Format(time.RFC3339)}, "repo": {"ACME/WEB"}}
	detail := evalGet[evalDetailResponse](t, handler, path+"?"+query.Encode())
	got := seriesValues(t, detail.Items[0].evalItem, "hours").Window["median"]
	if got == nil {
		t.Error("detail median has no value, want 6 from both stored spellings")
	} else if *got != 6 {
		t.Errorf("detail median = %g, want 6 from both stored spellings", *got)
	}
	query.Set("row_kind", "day")
	query.Set("series", "hours")
	items := evalGet[evalItemsResponse](t, handler, path+"/items?"+query.Encode())
	var repos []string
	for _, row := range items.Items {
		repo, _ := row.Cols["repo"].(string)
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	if want := []string{"Acme/Web", "acme/web"}; !slices.Equal(repos, want) {
		t.Errorf("items repositories = %v, want %v (the row's storage or published spelling)", repos, want)
	}
	query.Set("row_kind", "job")
	query.Del("series")
	jobs := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/pr-ci/items?"+query.Encode())
	if len(jobs.Items) != 1 {
		t.Fatalf("job items = %v, want the unit job", jobs.Items)
	}
	link, err := url.Parse(jobs.Items[0].Href)
	if err != nil {
		t.Fatal(err)
	}
	if got := link.Query()["repo"]; !slices.Equal(got, []string{"ACME/WEB"}) {
		t.Errorf("job link echoes %v, want the requested spelling", got)
	}
	behind := evalGet[evalItemsResponse](t, handler, link.String())
	if len(behind.Items) != 1 || behind.Items[0].Cols["repo"] != "Acme/Web" {
		t.Errorf("job link answers %v, want the day row's stored spelling Acme/Web", behind.Items)
	}
}

// TestALegionItemShowsTheRepositoryItIsFilteredBy: a Legion eval's issue row shows the repository
// its repo filter keeps it by, the one the day rows cut it by. An issue that owns no repository
// (its own, or an ancestor's) is stored and cut under legionfacts.DefaultRepo, which stands for
// its project's published default repository (eval_days_defaults, which the refresher's page
// commit moves), so the items read shows and filters by that published repository: a person's
// write of projects.default_repo shows nothing until the commit publishes it, and the row then
// reads the new repository unfiltered, answers the new one's filter showing the new one, and
// answers the old one's filter with nothing. No row is rewritten by either.
func TestALegionItemShowsTheRepositoryItIsFilteredBy(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	ctx := t.Context()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	actor := `{"kind":"user","id":"alice"}`
	exec("give EVL a default repository", `update projects set default_repo = 'acme/old' where key = 'EVL'`)
	exec("publish it", `insert into eval_days_defaults (project_key, repo) values ('EVL', 'acme/old')`)
	exec("write the issue", `insert into issues (key, project_key, number, title, status, rank, created_by) values ('EVL-1', 'EVL', 1, 'Shipped', 'done', 'U', $1)`, actor)
	day := measures.Midnight(time.Now()).AddDate(0, 0, -1)
	closed := day.Add(12 * time.Hour)
	// The row as the facts pass wrote it: the issue owns no repository, so it is stored under
	// legionfacts.DefaultRepo and no default move rewrites it.
	if _, err := legionfacts.WriteProject(ctx, database.Pool, legionfacts.ProjectWrite{Project: "EVL", Whole: true, Rows: []legionfacts.Row{{
		Project: "EVL", Issue: "EVL-1", Repo: legionfacts.DefaultRepo, ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: closed.Add(-3 * time.Hour), ClosedAt: &closed, Root: true, Tree: "EVL-1", WorkHours: 2,
			PhaseHours: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{},
		}},
	}}}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	stamped := func(what string) string {
		var repo string
		if err := database.Pool.QueryRow(ctx, `select repo from issue_facts where project_key = 'EVL' and issue_key = 'EVL-1'`).Scan(&repo); err != nil {
			t.Fatalf("read the stored repository %s: %v", what, err)
		}
		return repo
	}
	before := stamped("before the move")

	// The person's write, which no page has published: the read answers the published default.
	exec("move EVL's default repository", `update projects set default_repo = 'acme/new' where key = 'EVL'`)
	reads := func(moment string, cases []struct{ filter, want string }) {
		t.Helper()
		for _, read := range cases {
			query := url.Values{"row_kind": {"issue"}, "from": {day.Format(time.RFC3339)}, "to": {day.AddDate(0, 0, 1).Format(time.RFC3339)}}
			if read.filter != "" {
				query.Set("repo", read.filter)
			}
			items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/legion-work-time/items?"+query.Encode())
			var shown []string
			for _, row := range items.Items {
				shown = append(shown, fmt.Sprintf("%s in %v", row.RowKey, row.Cols[legionfacts.ColumnRepo]))
			}
			want := []string{}
			if read.want != "" {
				want = []string{"EVL-1 in " + read.want}
			}
			if strings.Join(shown, ", ") != strings.Join(want, ", ") {
				t.Errorf("%s, the items read with repo %q answers %v, want %v", moment, read.filter, shown, want)
			}
		}
	}
	reads("before the page publishes the move", []struct{ filter, want string }{{"", "acme/old"}, {"acme/old", "acme/old"}, {"acme/new", ""}})

	// The page's commit publishes it.
	exec("publish the move", `update eval_days_defaults set repo = 'acme/new' where project_key = 'EVL'`)
	reads("once the page has published it", []struct{ filter, want string }{{"", "acme/new"}, {"acme/new", "acme/new"}, {"acme/old", ""}})
	if after := stamped("after the move"); after != before || after != legionfacts.DefaultRepo {
		t.Errorf("the stored row holds %q after the move, where it held %q; want %s either way: a default move rewrites no Legion row",
			after, before, legionfacts.DefaultRepo)
	}
}

// TestALegionItemsPageIsThatPageOfTheWholeRead: legion-splitting's issue rows over a day that
// filed EVL-1 to EVL-3 and closed EVL-3 to EVL-7 answer one row per issue (R24), its filings
// first in filing order and then its closed issues newest close first, EVL-3's one row its closed
// row stating its filing too, measured at its close; and read a page of two at a time from every
// offset, they are the rows the whole read answers at those positions, field by field, under the
// same total: unfiltered, kept to the repository every issue resolves to, and kept to one no issue
// resolves to, which answers none. The pages cross from the filings to the closed issues. The read
// once answered EVL-3 twice, its filing's row and its closed row.
func TestALegionItemsPageIsThatPageOfTheWholeRead(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	ctx := t.Context()
	actor := `{"kind":"user","id":"alice"}`
	if _, err := database.Pool.Exec(ctx, `update projects set default_repo = 'acme/web' where key = 'EVL'`); err != nil {
		t.Fatalf("give EVL a default repository: %v", err)
	}
	// The refresher's page publishes it, which is what a read of the items takes its repositories
	// from (evals.PublishedDefaultIn): every issue below owns none, so each is read under it.
	if _, err := database.Pool.Exec(ctx, `insert into eval_days_defaults (project_key, repo) values ('EVL', 'acme/web')`); err != nil {
		t.Fatalf("publish EVL's default repository: %v", err)
	}
	day := measures.Midnight(time.Now()).AddDate(0, 0, -1)
	var rows []legionfacts.Row
	var filings []legionfacts.Filing
	for number := 1; number <= 7; number++ {
		key := fmt.Sprintf("EVL-%d", number)
		if _, err := database.Pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, rank, created_by) values ($1, 'EVL', $2, $3, 'done', $4, $5)`,
			key, number, "Issue "+key, fmt.Sprintf("U%03d", number), actor); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
		at := day.Add(time.Duration(number) * time.Hour)
		if number <= 3 {
			filings = append(filings, legionfacts.Filing{Project: "EVL", IssueFiling: metrics.IssueFiling{FiledIssue: metrics.FiledIssue{
				Key: key, At: at, Filer: "architect", FilerIssue: "EVL-4", FilerPhase: "implementing", Class: "in_tree_at_design",
			}, Root: key}})
		}
		if number < 3 {
			continue
		}
		closed := at
		if number == 3 {
			closed = at.Add(30 * time.Minute)
		}
		rows = append(rows, legionfacts.Row{Project: "EVL", Issue: key, Repo: legionfacts.DefaultRepo, ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: at.Add(-time.Hour), ClosedAt: &closed, Root: true, Tree: key, WorkHours: float64(number),
			PhaseHours: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{},
		}}})
	}
	if _, err := legionfacts.WriteProject(ctx, database.Pool, legionfacts.ProjectWrite{Project: "EVL", Whole: true, Rows: rows}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	if err := legionfacts.WriteFilings(ctx, database.Pool, legionfacts.FilingsWrite{Project: "EVL", Whole: true, Filings: filings}); err != nil {
		t.Fatalf("write the filings: %v", err)
	}
	read := func(repo string, limit, offset int) evalItemsResponse {
		t.Helper()
		query := url.Values{"row_kind": {"issue"}, "from": {day.Format(time.RFC3339)}, "to": {day.AddDate(0, 0, 1).Format(time.RFC3339)},
			"limit": {fmt.Sprint(limit)}, "offset": {fmt.Sprint(offset)}}
		if repo != "" {
			query.Set("repo", repo)
		}
		return evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/legion-splitting/items?"+query.Encode())
	}
	order := []string{"EVL-1", "EVL-2", "EVL-7", "EVL-6", "EVL-5", "EVL-4", "EVL-3"}
	for _, repo := range []string{"", "acme/web", "acme/other"} {
		whole := read(repo, 200, 0)
		want := map[string][]string{"": order, "acme/web": order, "acme/other": {}}[repo]
		keys := []string{}
		for _, row := range whole.Items {
			keys = append(keys, row.RowKey)
		}
		if !slices.Equal(keys, want) || whole.Total != len(want) {
			t.Fatalf("the whole read with repo %q answers the rows %v of %d, want %v", repo, keys, whole.Total, want)
		}
		for _, row := range whole.Items {
			_, filing := row.Cols[legionfacts.ColumnFiledAt]
			_, closing := row.Cols[legionfacts.ColumnClosedAt]
			if number := row.RowKey[len("EVL-"):]; filing != (number <= "3") || closing != (number >= "3") {
				t.Errorf("repo %q: the row of %s states a filing %t and a close %t, want %t and %t", repo, row.RowKey, filing, closing, number <= "3", number >= "3")
			}
			if row.RowKey == "EVL-3" && !row.MeasuredAt.Equal(day.Add(3*time.Hour+30*time.Minute)) {
				t.Errorf("repo %q: EVL-3's row is measured at %s, want its close", repo, row.MeasuredAt)
			}
		}
		for offset := 0; offset <= whole.Total; offset++ {
			page := read(repo, 2, offset)
			want := whole.Items[min(offset, len(whole.Items)):min(offset+2, len(whole.Items))]
			got, err := json.Marshal(page.Items)
			if err != nil {
				t.Fatalf("encode the page: %v", err)
			}
			wanted, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("encode the whole read's rows: %v", err)
			}
			if page.Total != whole.Total || string(got) != string(wanted) {
				t.Errorf("repo %q, offset %d: the page answers %s of %d, want %s of %d", repo, offset, got, page.Total, wanted, whole.Total)
			}
		}
	}
}

// TestTheCrossProjectListPagesAcrossTheProjectsInTheirNamedOrder: GET /api/v1/evals over three
// projects named out of key order answers, for every kind and for one kind, the evals each
// project's own list read answers, all of them, by project in the order named and then by slug;
// and a page of seven read from any offset is the slice of the whole read at that offset under the
// same total: the page runs across the projects, so some page holds the end of one project's evals
// and the start of the next one's.
func TestTheCrossProjectListPagesAcrossTheProjectsInTheirNamedOrder(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	for _, project := range []string{"EVA", "EVB", "EVC"} {
		createTestProject(t, handler, project)
	}
	named := []string{"EVC", "EVA", "EVB"}
	for _, kind := range []string{"", evals.KindDelivery} {
		query := url.Values{"project": named}
		if kind != "" {
			query.Set("kind", kind)
		}
		// want is each named project's own evals, in the order named, each project's by slug.
		var want []string
		for _, project := range named {
			ownQuery := url.Values{"limit": {"200"}}
			if kind != "" {
				ownQuery.Set("kind", kind)
			}
			own := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?"+ownQuery.Encode())
			if own.Total == 0 || own.Total != len(own.Items) {
				t.Fatalf("kind %q: %s's own read answers %d of %d evals, want all of at least one", kind, project, len(own.Items), own.Total)
			}
			slugs := make([]string, len(own.Items))
			for index, item := range own.Items {
				slugs[index] = project + "/" + item.Definition.Slug
			}
			want = append(want, slices.Sorted(slices.Values(slugs))...)
		}
		whole := evalGet[evalListResponse](t, handler, "/api/v1/evals?limit=200&"+query.Encode())
		var order []string
		for _, item := range whole.Items {
			order = append(order, item.Definition.ProjectKey+"/"+item.Definition.Slug)
		}
		if whole.Total != len(want) || !slices.Equal(order, want) {
			t.Fatalf("kind %q: the whole read answers %d of %d evals in the order %v, want the %d of the projects' own reads by project as named and then by slug: %v",
				kind, len(whole.Items), whole.Total, order, len(want), want)
		}
		spanning := 0
		for offset := 0; offset <= whole.Total; offset += 3 {
			query.Set("limit", "7")
			query.Set("offset", fmt.Sprint(offset))
			page := evalGet[evalListResponse](t, handler, "/api/v1/evals?"+query.Encode())
			var got []string
			projects := map[string]bool{}
			for _, item := range page.Items {
				got = append(got, item.Definition.ProjectKey+"/"+item.Definition.Slug)
				projects[item.Definition.ProjectKey] = true
			}
			if wanted := order[min(offset, len(order)):min(offset+7, len(order))]; page.Total != whole.Total || !slices.Equal(got, wanted) {
				t.Errorf("kind %q, offset %d: the page answers %v of %d, want %v of %d", kind, offset, got, page.Total, wanted, whole.Total)
			}
			if len(projects) > 1 {
				spanning++
			}
		}
		if spanning == 0 {
			t.Errorf("kind %q: no page of seven holds the evals of two projects, so no page was read across them", kind)
		}
	}
}

// TestAnEvalWindowWhoseDaysEndPastTheYear9999IsRefused: an eval read answers the whole UTC days
// its window covers, ending at the midnight after its last day, so a to in the last day of 9999,
// though it is a moment of that year, ends the days at 10000-01-01, which no answer can write.
// Each of the four reads refuses it 400 INVALID_QUERY, in the window and in the compare range
// (R18); a to or a compare_to at that day's midnight, whose days end there, is answered.
func TestAnEvalWindowWhoseDaysEndPastTheYear9999IsRefused(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registerReviewTime(t, handler)
	reads := []string{"/api/v1/projects/EVL/evals?", "/api/v1/evals?", "/api/v1/projects/EVL/evals/review-time?", "/api/v1/projects/EVL/evals/review-time/items?"}
	for _, window := range []url.Values{
		{"from": {"9999-12-30T00:00:00Z"}, "to": {"9999-12-31T23:00:00Z"}},
		{"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-11-02T00:00:00Z"}, "compare_from": {"9999-12-30T00:00:00Z"}, "compare_to": {"9999-12-31T00:00:01Z"}},
	} {
		for _, read := range reads {
			target := read + window.Encode()
			response := evalRequest(t, handler, http.MethodGet, target, nil, "")
			var refusal evalRefusal
			if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil || refusal.Code != "INVALID_QUERY" {
				t.Errorf("GET %s: status %d, body %.200s; want 400 INVALID_QUERY", target, response.Code, response.Body.String())
			}
		}
	}
	for _, window := range []url.Values{
		{"from": {"9999-12-24T00:00:00Z"}, "to": {"9999-12-31T00:00:00Z"}},
		{"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-11-02T00:00:00Z"}, "compare_from": {"9999-12-30T00:00:00Z"}, "compare_to": {"9999-12-31T00:00:00Z"}},
	} {
		for _, read := range reads {
			target := read + window.Encode()
			checkAnswered(t, "GET "+target, evalRequest(t, handler, http.MethodGet, target, nil, ""))
		}
	}
}

// TestAnEvalsLiveViewWhoseDaysEndPastTheYear9999IsRefused: an evals view whose own custom window
// ends its days past the year 9999 is refused INVALID_QUERY when it is subscribed, as the list
// route refuses the same window, and nothing is sent after the refusal once the hub owes nothing
// (another page holds a view of the last 28 days, so the hub runs a generation to settle). Its
// window is the view's, not its query's, so a check of the query alone passed it, and the hub held
// a view whose every read failed and was tried again every deliveryLiveRetry, its subscriber told
// nothing.
func TestAnEvalsLiveViewWhoseDaysEndPastTheYear9999IsRefused(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "EVL")
	from, to := "9999-12-30T00:00:00Z", "9999-12-31T23:00:00Z"
	route := evalRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/EVL/evals?"+url.Values{"from": {from}, "to": {to}}.Encode(), nil, "")
	if response := decodeBody[evalRefusal](t, route); route.Code != http.StatusBadRequest || response.Code != "INVALID_QUERY" {
		t.Fatalf("GET the list over %s..%s: status %d, body %s; want 400 INVALID_QUERY", from, to, route.Code, route.Body.String())
	}
	held := newFakeLiveConn()
	rig.hub.Subscribe(held, subscribeFrame("held", evalsLiveView("EVL", DeliveryLiveWindow{Preset: new(28)}, "")))
	held.snapshot(t, "held")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("late", evalsLiveView("EVL", DeliveryLiveWindow{From: &from, To: &to}, "")))
	refusal := conn.refusal(t, "late", "INVALID_QUERY")
	rig.requireNothingSent(t, conn, "the refusal of a window whose days end past the year 9999")
	t.Logf("the view over %s..%s: %s %q", from, to, refusal.Code, refusal.Message)
}

// TestAWindowWhoseSumPassesWhatAFloatHoldsAnswersItNull: readings of 1e308 on two days of a sum
// series are each accepted, since each day's row holds a finite sum, but a window over both days
// sums past the largest float, which no answer can write. Every read of that window - the
// project's list, the cross-project read with the project named and with none, the eval's own
// read, its items, and the evals live view's snapshot - answers the envelope, the window's sum
// null and its count 2, each day's sum stated in the per-day array. The change from a compare
// range holding a reading of -1e308 to a window of one 1e308 day is past the largest float too,
// and is null where both ranges' sums are stated.
func TestAWindowWhoseSumPassesWhatAFloatHoldsAnswersItNull(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	handler := rig.handler
	createTestProject(t, handler, "EVL")
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "huge", "series": []any{map[string]any{"series": "value", "agg": "sum"}}}, "huge-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register huge: status %d, body %s", registered.Code, registered.Body.String())
	}
	// Every day the test writes or reads ends at least two days before today's midnight, so a
	// midnight that passes while it runs moves none of them.
	today := measures.Midnight(time.Now().UTC())
	day := func(offset int) time.Time { return today.AddDate(0, 0, offset) }
	for index, reading := range []struct {
		day   int
		value float64
	}{{-5, -1e308}, {-4, 1e308}, {-3, 1e308}} {
		written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/huge/readings", map[string]any{"readings": []any{
			map[string]any{"series": "value", "row_kind": "run", "row_key": fmt.Sprint(index), "value": reading.value,
				"measured_at": day(reading.day).Add(9 * time.Hour).Format(time.RFC3339)},
		}}, fmt.Sprintf("huge-%d", index))
		if written.Code != http.StatusCreated {
			t.Fatalf("log %g on %s: status %d, body %s", reading.value, day(reading.day).Format(time.DateOnly), written.Code, written.Body.String())
		}
	}
	// value is huge's series as an answer states it, failing where the answer holds none.
	value := func(what string, body []byte) evals.SeriesValues {
		t.Helper()
		var page struct {
			Items []evalItem `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("%s: decode %.200s: %v", what, body, err)
		}
		for _, item := range page.Items {
			if item.Definition.EvalID != "EVL/huge" {
				continue
			}
			for _, series := range item.Series {
				if series.Series.Series == "value" {
					return series
				}
			}
		}
		t.Fatalf("%s answers no series value of EVL/huge: %.300s", what, body)
		return evals.SeriesValues{}
	}
	stated := func(stats evals.Statistics, name string) string {
		if stats[name] == nil {
			return "null"
		}
		return fmt.Sprint(*stats[name])
	}
	// daySums is each day's sum the per-day array states.
	daySums := func(series evals.SeriesValues) []string {
		sums := make([]string, len(series.Days))
		for index, point := range series.Days {
			sums[index] = stated(point.Statistics, "sum")
		}
		return sums
	}
	both := url.Values{"from": {day(-4).Format(time.RFC3339)}, "to": {day(-2).Format(time.RFC3339)}}.Encode()
	for _, path := range []string{
		"/api/v1/projects/EVL/evals?" + both,
		"/api/v1/evals?project=EVL&" + both,
		"/api/v1/evals?" + both,
		"/api/v1/projects/EVL/evals/huge?" + both,
		"/api/v1/projects/EVL/evals/huge/items?" + both,
	} {
		response := evalRequest(t, handler, http.MethodGet, path, nil, "")
		checkAnswered(t, "GET "+path, response)
		if response.Code != http.StatusOK || strings.Contains(path, "/items") {
			continue
		}
		series := value("GET "+path, response.Body.Bytes())
		if series.Window["sum"] != nil || stated(series.Window, "n") != "2" || !slices.Equal(daySums(series), []string{"1e+308", "1e+308"}) {
			t.Errorf("GET %s: the window's sum is %s over n %s with the days' sums %v, want null over 2 with each day's 1e+308", path,
				stated(series.Window, "sum"), stated(series.Window, "n"), daySums(series))
		}
	}

	compared := url.Values{"from": {day(-3).Format(time.RFC3339)}, "to": {day(-2).Format(time.RFC3339)},
		"compare_from": {day(-5).Format(time.RFC3339)}, "compare_to": {day(-4).Format(time.RFC3339)}}.Encode()
	response := evalRequest(t, handler, http.MethodGet, "/api/v1/projects/EVL/evals?"+compared, nil, "")
	checkAnswered(t, "GET the compared list", response)
	if response.Code == http.StatusOK {
		series := value("the compared list", response.Body.Bytes())
		if stated(series.Window, "sum") != "1e+308" || stated(series.Compare, "sum") != "-1e+308" || series.Change["sum"] != nil {
			t.Errorf("the compared list states the sums %s and %s and the change %s, want 1e+308, -1e+308 and null",
				stated(series.Window, "sum"), stated(series.Compare, "sum"), stated(series.Change, "sum"))
		}
	}

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("huge", evalsLiveView("EVL", evalsDays(day(-4), day(-2)), "")))
	snapshot := conn.snapshot(t, "huge")
	requireTheListRead(t, handler, "EVL", snapshot.Body, "")
	if series := value("the evals view's snapshot", snapshot.Body); series.Window["sum"] != nil {
		t.Errorf("the evals view's snapshot states the window's sum %s, want null", stated(series.Window, "sum"))
	}
}

// TestAnAnswerThatCannotBeEncodedIsAnInternalRefusal: WriteJSON encodes a body whole before it
// writes its status, so a body no JSON can hold (a +Inf, a time of the year 10000) answers 500
// INTERNAL with its {code, error} body, never the status it was given with an empty body; a body
// that encodes is written as json.Encoder writes it, its newline included.
func TestAnAnswerThatCannotBeEncodedIsAnInternalRefusal(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]any{
		"an infinite figure": map[string]any{"sum": math.Inf(1)},
		"a far time":         map[string]any{"at": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		response := httptest.NewRecorder()
		WriteJSON(response, http.StatusCreated, body)
		var refusal evalRefusal
		if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &refusal) != nil || refusal.Code != codeInternal {
			t.Errorf("%s: status %d, body %q; want 500 %s", name, response.Code, response.Body.String(), codeInternal)
		}
	}
	response := httptest.NewRecorder()
	WriteJSON(response, http.StatusCreated, map[string]any{"sum": 1.5, "html": "<b>"})
	if want := "{\"html\":\"\\u003cb\\u003e\",\"sum\":1.5}\n"; response.Code != http.StatusCreated || response.Body.String() != want ||
		response.Header().Get("Content-Type") != "application/json" {
		t.Errorf("an encodable body answers %d %q (%s), want 201 %q as application/json", response.Code, response.Body.String(), response.Header().Get("Content-Type"), want)
	}
}

// TestALastReadOverTwoRepositoriesTakesTheLaterWriteOfATie: two readings of a `last` series
// measured at the same moment on one day, under two repositories, are a tie the readings write
// breaks by write order (105bfbb9): the project's day row holds the later write's value. A read of
// the same day that names both repositories folds their two rows, and must answer the same value
// whatever order the repositories come back in; at 0ea9989f it answered the value of the
// repository whose key sorts last, because the window fold keeps the later row in the read's own
// order for an equal last_at and the read sets none (dispatch://INFRA-2916, the testing round of
// 2026-11-18, review v2's R2-8; WritesChecks/repotie.r10.py drove it on a server).
func TestALastReadOverTwoRepositoriesTakesTheLaterWriteOfATie(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "tie", "series": []any{map[string]any{"series": "v", "agg": "last"}}}, "tie-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register tie: status %d, body %s", registered.Code, registered.Body.String())
	}
	day := measures.Midnight(time.Now().UTC()).AddDate(0, 0, -3)
	at := day.Add(12 * time.Hour).Format(time.RFC3339)
	// b's reading first, then a's: the later write is a's, whose key sorts first.
	for _, reading := range []struct {
		repo  string
		value float64
	}{{"acme/b", 2}, {"acme/a", 1}} {
		written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/tie/readings", map[string]any{"readings": []any{
			map[string]any{"series": "v", "row_kind": "run", "row_key": strings.ReplaceAll(reading.repo, "/", "-"), "value": reading.value, "measured_at": at,
				"attributes": map[string]any{"repo": reading.repo}},
		}}, "tie-"+strings.ReplaceAll(reading.repo, "/", "-"))
		if written.Code != http.StatusCreated {
			t.Fatalf("log %g under %s: status %d, body %s", reading.value, reading.repo, written.Code, written.Body.String())
		}
	}
	last := func(query url.Values) string {
		t.Helper()
		query.Set("from", day.Format(time.RFC3339))
		query.Set("to", day.AddDate(0, 0, 1).Format(time.RFC3339))
		detail := decodeBody[evalDetailResponse](t, evalRequest(t, handler, http.MethodGet, "/api/v1/projects/EVL/evals/tie?"+query.Encode(), nil, ""))
		for _, item := range detail.Items {
			for _, series := range item.Series {
				if series.Series.Series == "v" {
					if series.Window["value"] == nil {
						return "null"
					}
					return fmt.Sprint(*series.Window["value"])
				}
			}
		}
		t.Fatalf("the read of EVL/tie answers no series v")
		return ""
	}
	if project := last(url.Values{}); project != "1" {
		t.Fatalf("the project's read answers %s, want 1, the later write's value", project)
	}
	for _, repos := range [][]string{{"acme/a", "acme/b"}, {"acme/b", "acme/a"}} {
		if got := last(url.Values{"repo": repos}); got != "1" {
			t.Errorf("the read of repositories %v answers %s; the project's answers 1, the later write's value", repos, got)
		}
	}
}
