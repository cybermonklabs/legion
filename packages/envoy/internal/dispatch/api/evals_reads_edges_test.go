package api

import (
	"encoding/json"
	"fmt"
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

// TestALegionItemsPageIsThatPageOfTheWholeRead: legion-splitting's issue rows, its filings and
// then its closed issues, read a page of two at a time from every offset, are the rows the whole
// read answers at those positions, field by field, under the same total: unfiltered, kept to the
// repository every issue resolves to, and kept to one no issue resolves to, which answers none.
// The pages cross from the filings to the closed issues.
func TestALegionItemsPageIsThatPageOfTheWholeRead(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	ctx := t.Context()
	actor := `{"kind":"user","id":"alice"}`
	if _, err := database.Pool.Exec(ctx, `insert into repo_projects (repo, project, created_by, is_default) values ('acme/web', 'EVL', $1, true)`, actor); err != nil {
		t.Fatalf("map EVL's default repository: %v", err)
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
			filings = append(filings, legionfacts.Filing{Project: "EVL", Issue: key, FiledAt: at, FilerRole: "architect", FilerIssue: "EVL-4",
				FilerPhase: "implementing", Class: "in_tree_at_design", RootKey: key})
			continue
		}
		rows = append(rows, legionfacts.Row{Project: "EVL", Issue: key, OpenedAt: at.Add(-time.Hour), ClosedAt: &at, RootKey: key, WorkHours: float64(number),
			HoursByPhase: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{}, Repo: "acme/web"})
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
	for _, repo := range []string{"", "acme/web", "acme/other"} {
		whole := read(repo, 200, 0)
		if want := map[string]int{"": 7, "acme/web": 7, "acme/other": 0}[repo]; whole.Total != want || len(whole.Items) != want {
			t.Fatalf("the whole read with repo %q answers %d rows of %d, want %d", repo, len(whole.Items), whole.Total, want)
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
// projects named out of key order answers their evals by project in the order named and then by
// slug, and a page of seven read from any offset is the slice of the whole read at that offset
// under the same total, for every kind and for one kind: the page runs across the projects, so a
// page holds the end of one project's evals and the start of the next one's.
func TestTheCrossProjectListPagesAcrossTheProjectsInTheirNamedOrder(t *testing.T) {
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
		whole := evalGet[evalListResponse](t, handler, "/api/v1/evals?limit=200&"+query.Encode())
		var order []string
		for _, item := range whole.Items {
			order = append(order, item.Definition.ProjectKey+"/"+item.Definition.Slug)
		}
		want := slices.SortedFunc(slices.Values(order), func(a, b string) int {
			projectA, slugA, _ := strings.Cut(a, "/")
			projectB, slugB, _ := strings.Cut(b, "/")
			if byProject := slices.Index(named, projectA) - slices.Index(named, projectB); byProject != 0 {
				return byProject
			}
			return strings.Compare(slugA, slugB)
		})
		if whole.Total != len(whole.Items) || whole.Total < 3 || !slices.Equal(order, want) {
			t.Fatalf("kind %q: the whole read answers %d of %d evals in the order %v, want them by project as named and then by slug", kind, len(whole.Items), whole.Total, order)
		}
		for offset := 0; offset <= whole.Total; offset += 3 {
			query.Set("limit", "7")
			query.Set("offset", fmt.Sprint(offset))
			page := evalGet[evalListResponse](t, handler, "/api/v1/evals?"+query.Encode())
			var got []string
			for _, item := range page.Items {
				got = append(got, item.Definition.ProjectKey+"/"+item.Definition.Slug)
			}
			if wanted := order[min(offset, len(order)):min(offset+7, len(order))]; page.Total != whole.Total || !slices.Equal(got, wanted) {
				t.Errorf("kind %q, offset %d: the page answers %v of %d, want %v of %d", kind, offset, got, page.Total, wanted, whole.Total)
			}
		}
	}
}

// TestAnEvalWindowWhoseDaysEndPastTheYear9999IsRefused: an eval read answers the whole UTC days
// its window covers, ending at the midnight after its last day, so a to in the last day of 9999,
// though it is a moment of that year, ends the days at 10000-01-01, which no answer can write.
// Each of the four reads refuses it 400 INVALID_QUERY, in the window and in the compare range
// (R18); a to at that day's midnight, whose days end there, is answered.
func TestAnEvalWindowWhoseDaysEndPastTheYear9999IsRefused(t *testing.T) {
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
	answered := url.Values{"from": {"9999-12-24T00:00:00Z"}, "to": {"9999-12-31T00:00:00Z"}}.Encode()
	for _, read := range reads {
		checkAnswered(t, "GET "+read+answered, evalRequest(t, handler, http.MethodGet, read+answered, nil, ""))
	}
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
	value := func(what string, body []byte) evalSeriesValues {
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
		return evalSeriesValues{}
	}
	stated := func(stats evals.Statistics, name string) string {
		if stats[name] == nil {
			return "null"
		}
		return fmt.Sprint(*stats[name])
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
		if series.Window["sum"] != nil || stated(series.Window, "n") != "2" || len(series.Days) != 2 || stated(series.Days[0].Statistics, "sum") != "1e+308" {
			t.Errorf("GET %s: the window's sum is %s over n %s with %d days, want null over 2 with each day's 1e+308", path,
				stated(series.Window, "sum"), stated(series.Window, "n"), len(series.Days))
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
	snapshot := conn.snapshot(t, 5*time.Second, "huge")
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
