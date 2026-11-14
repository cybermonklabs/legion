package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
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
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, time.Minute); waiting < 1 {
		t.Fatalf("%d reads wait on eval_series, want the eval's read", waiting)
	}
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
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", map[string]any{
		"slug": "pr-ci", "series": []any{map[string]any{"series": "job:*", "min_n": 0}},
	}, "pr-ci-min-n")
	if registered.Code != http.StatusCreated {
		t.Fatalf("set pr-ci's job min_n to 0: status %d, body %s", registered.Code, registered.Body.String())
	}
	today := measures.Midnight(time.Now())
	if _, err := database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, samples, counts, built_by)
		values ('EVL', 'EVL/pr-ci', 'job:lint', '', $1, false, 0, '{}', '{"cancelled": 1}', 'test'),
		       ('EVL', 'EVL/pr-ci', 'job:unit', '', $1, false, 2, '{3, 5}', '{"success": 2}', 'test')
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

// TestAnItemsLinkKeepsItsRepository: a day row of one repository links to that repository's items
// of its day, and a job row read for named repositories links to those repositories' day rows of
// the job, so following either link answers the rows the row was read from and not every
// repository's.
func TestAnItemsLinkKeepsItsRepository(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	today := measures.Midnight(time.Now())
	if _, err := database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, samples, counts, built_by)
		values ('EVL', 'EVL/deploy-frequency', 'deploys', 'acme/web', $1, true, 2, 2, null, '{}', 'test'),
		       ('EVL', 'EVL/pr-ci', 'job:unit', 'acme/web', $1, false, 2, null, '{3, 5}', '{"success": 2}', 'test')
	`, today); err != nil {
		t.Fatalf("write the repository's rows: %v", err)
	}
	window := url.Values{"from": {today.Format(time.RFC3339)}, "to": {today.AddDate(0, 0, 1).Format(time.RFC3339)}, "repo": {"acme/web"}}.Encode()
	for _, read := range []string{"deploy-frequency/items?row_kind=day&", "pr-ci/items?row_kind=job&"} {
		items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/"+read+window)
		if len(items.Items) == 0 {
			t.Fatalf("%s answers no row", read)
		}
		for _, row := range items.Items {
			link, err := url.Parse(row.Href)
			if err != nil {
				t.Fatalf("%s row %s links to %q: %v", read, row.RowKey, row.Href, err)
			}
			if repos := link.Query()["repo"]; len(repos) != 1 || repos[0] != "acme/web" {
				t.Errorf("%s row %s links to %s, whose repo is %v; want acme/web", read, row.RowKey, row.Href, repos)
			}
		}
	}
}

// TestALegionItemShowsTheRepositoryItIsFilteredBy: a Legion eval's issue row shows the repository
// its repo filter keeps it by, the one the day rows cut it by: the repository the issue resolves
// to when the route reads it (legionfacts.ReadRepos). The stored row holds the repository its
// facts pass resolved, so a move of the project's default mapping that no pass has carried into
// the row yet would show one repository while filtering by another. After the move the row reads
// the new repository unfiltered, answers the new one's filter showing the new one, and answers
// the old one's filter with nothing.
func TestALegionItemShowsTheRepositoryItIsFilteredBy(t *testing.T) {
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
	exec("map EVL's default repository", `insert into repo_projects (repo, project, created_by, is_default) values ('acme/old', 'EVL', $1, true)`, actor)
	exec("write the issue", `insert into issues (key, project_key, number, title, status, rank, created_by) values ('EVL-1', 'EVL', 1, 'Shipped', 'done', 'U', $1)`, actor)
	day := measures.Midnight(time.Now()).AddDate(0, 0, -1)
	closed := day.Add(12 * time.Hour)
	// The row as the facts pass wrote it while acme/old was the default.
	if _, err := legionfacts.WriteProject(ctx, database.Pool, legionfacts.ProjectWrite{Project: "EVL", Whole: true, Rows: []legionfacts.Row{{
		Project: "EVL", Issue: "EVL-1", OpenedAt: closed.Add(-3 * time.Hour), ClosedAt: &closed, RootKey: "EVL-1", WorkHours: 2,
		HoursByPhase: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{}, Repo: "acme/old",
	}}}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	// The default moves, and no facts pass runs here to carry it into the stored row.
	exec("unmap acme/old", `delete from repo_projects where repo = 'acme/old'`)
	exec("map EVL's new default repository", `insert into repo_projects (repo, project, created_by, is_default) values ('acme/new', 'EVL', $1, true)`, actor)

	for _, read := range []struct{ filter, want string }{{"", "acme/new"}, {"acme/new", "acme/new"}, {"acme/old", ""}} {
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
			t.Errorf("the items read with repo %q answer %v, want %v", read.filter, shown, want)
		}
	}
}
