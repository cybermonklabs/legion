package api

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival"
	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The tests of the two code evals' rows of GET /api/v1/projects/{key}/evals/{slug}/items
// (evals_items_code.go): the changes the survival eval counts, the pairs the rework eval counts,
// and the reasons a range of either reads "no data" (dispatch://INFRA-3206 R8, R11, R13; k7, k10,
// k11, k16). Every result they read is written by the code survival run, measuring a branch the
// GitHub double serves, and by codesurvival.WriteMarks.

// The repositories of the code items' seed: the one the double serves, and the one it refuses the
// App.
const (
	codeItemsRepo   = "acme/code"
	codeItemsHidden = "acme/hidden"
)

// The projects of the code items' seed: CODE, whose issues the branch's pull requests name, under
// two groups; TWIN, of the same repository, whose one group takes every change; BARE, of the same
// repository, which names no group; HIDDEN, whose default repository the App cannot read; and
// LOOSE, with no default repository and no change.
const (
	codeItemsCode   = "CODE"
	codeItemsTwin   = "TWIN"
	codeItemsBare   = "BARE"
	codeItemsHide   = "HIDDEN"
	codeItemsLoose  = "LOOSE"
	codeItemsIssue1 = "CODE-1"
	codeItemsIssue2 = "CODE-2"
)

// The days of the code items' branch: 31 days of changes; day 3's commit that merged no pull
// request, whose patch the double withholds, blocks days 0 to 3; days 4 to 27 are measured; and
// codesurvival.WriteMarks marks days 6 and 7.
const (
	codeItemsDays       = 31
	codeItemsBlockedDay = 3
	codeItemsMeasured   = 28
)

// codeItemsMarked is the days the seed marks after the run, in order.
var codeItemsMarked = []int{6, 7}

// codeItemsD0 is the first day of the code items' branch, a UTC midnight.
var codeItemsD0 = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// codeItemsDay is the UTC midnight n days after codeItemsD0.
func codeItemsDay(n int) time.Time { return codeItemsD0.AddDate(0, 0, n) }

// codeItemsPass is when the run measures the branch: day 27 waits from day 60.
var codeItemsPass = codeItemsDay(60).Add(time.Hour)

// codeItemsHistory is the branch the double serves as codeItemsRepo, and what the tests read of
// it: day 3's withheld commit, each pull request by the commit it landed as, and the committer
// time of every commit of its first-parent chain, by SHA.
type codeItemsHistory struct {
	branch  *codesurvivaltest.Branch
	blocked string
	pulls   map[string]codesurvivaltest.Pull
	chain   map[string]time.Time
}

var codeItemsOnce struct {
	once sync.Once
	set  *codeItemsHistory
}

// codeItemsBranch is the code items' branch, built on the first call. Each day n lands two changes
// of the agents (head branch agent/<number>) and one of another author (feature/<number>), from
// 09:00, ten minutes apart. Each agents change adds four lines and removes the oldest line left of
// the agents change before it, and the other author's change removes one more line of the day's
// first agents change. The agents changes name CODE-1, CODE-1, CODE-2 and no issue, in turn, so
// their removals of each other are iteration, rework and no issue, and the other author's are
// other; the other author's changes name CODE-2 on even days and no issue on odd ones. On days 4,
// 9, 14 and so on, a commit that merged no pull request removes a line of the day before's other
// change; and on day 3 one adds two lines to each of twelve files.
func codeItemsBranch(t *testing.T) *codeItemsHistory {
	t.Helper()
	codeItemsOnce.once.Do(func() {
		h := codesurvivaltest.NewHistory(t)
		files := map[string][]string{}
		write := func(path string) { h.Repo.Write(path, strings.Join(files[path], "\n")+"\n") }
		add := func(path, what string, n int) []string {
			var added []string
			for i := range n {
				added = append(added, fmt.Sprintf("%s, line %d of %s, written once and kept as written", what, i, path))
			}
			files[path] = append(files[path], added...)
			write(path)
			return added
		}
		// pop removes from path the oldest line left of lines.
		pop := func(path string, lines *[]string) {
			if len(*lines) == 0 {
				return
			}
			files[path] = slices.DeleteFunc(files[path], func(held string) bool { return held == (*lines)[0] })
			*lines = (*lines)[1:]
			write(path)
		}
		set := &codeItemsHistory{}
		var agents, others [][]string
		number := 0
		land := func(head, author, title string, at time.Time) {
			number++
			h.Land(codesurvivaltest.Pull{Number: number, HeadRef: fmt.Sprintf("%s/%d", head, number), Author: author, Title: title}, at)
		}
		for n := range codeItemsDays {
			at := codeItemsDay(n).Add(9 * time.Hour)
			if n == codeItemsBlockedDay {
				for file := range 12 {
					add(fmt.Sprintf("data/table%02d.txt", file), fmt.Sprintf("day %d table %d", n, file), 2)
				}
				set.blocked = h.Push("twelve tables", at.Add(-3*time.Hour))
			}
			for j := range 2 {
				k := len(agents)
				agents = append(agents, add("src/agents.txt", fmt.Sprintf("agents change %d", k), 4))
				if k > 0 {
					pop("src/agents.txt", &agents[k-1])
				}
				title := fmt.Sprintf("agents change %d", k)
				switch k % 4 {
				case 0, 1:
					title = codeItemsIssue1 + ": " + title
				case 2:
					title = codeItemsIssue2 + ": " + title
				}
				land("agent", "agent-app[bot]", title, at.Add(time.Duration(j)*10*time.Minute))
			}
			others = append(others, add("src/others.txt", fmt.Sprintf("others change %d", n), 4))
			pop("src/agents.txt", &agents[2*n])
			title := fmt.Sprintf("others change %d", n)
			if n%2 == 0 {
				title = codeItemsIssue2 + ": " + title
			}
			land("feature", "someone", title, at.Add(20*time.Minute))
			if n%5 == 4 {
				pop("src/others.txt", &others[n-1])
				h.Push("tidy the other change", at.Add(30*time.Minute))
			}
		}
		set.branch = h.Branch()
		set.pulls, set.chain = map[string]codesurvivaltest.Pull{}, map[string]time.Time{}
		for _, pull := range set.branch.Pulls {
			set.pulls[pull.SHA] = pull
		}
		for _, commit := range set.branch.Chain {
			set.chain[commit.SHA] = commit.CommittedAt
		}
		codeItemsOnce.set = set
	})
	if codeItemsOnce.set == nil {
		t.Fatal("the code items' branch was not built: see the test that built it")
	}
	return codeItemsOnce.set
}

// firstChangeOf is the first commit of day n of the branch that landed a pull request.
func (h *codeItemsHistory) firstChangeOf(n int) string {
	for _, commit := range h.branch.Chain {
		if _, landed := h.pulls[commit.SHA]; landed && !commit.CommittedAt.Before(codeItemsDay(n)) {
			return commit.SHA
		}
	}
	return ""
}

// codeItemsRig is one test's server holding the code items' seed, measured by the code survival
// run and then marked, before any further run.
type codeItemsRig struct {
	handler http.Handler
	history *codeItemsHistory
}

// newCodeItemsRig is the code items' seed: the five projects, their default repositories and code
// groups, CODE's two issues, one run of the code survival measurement over the double, which serves
// codeItemsRepo with day 3's commit withheld and refuses codeItemsHidden the App, and then
// codesurvival.WriteMarks over days 6 and 7 of codeItemsRepo, as a force-push's pass marks them.
func newCodeItemsRig(t *testing.T) *codeItemsRig {
	t.Helper()
	history := codeItemsBranch(t)
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := t.Context()
	createTestProject(t, handler, codeItemsCode, codeItemsTwin, codeItemsBare, codeItemsHide, codeItemsLoose)
	for project, repo := range map[string]string{codeItemsCode: codeItemsRepo, codeItemsTwin: codeItemsRepo, codeItemsBare: codeItemsRepo, codeItemsHide: codeItemsHidden} {
		setDefaultRepository(t, handler, project, repo)
	}
	every := []delivery.CodeGroup{{Name: "all"}}
	for project, groups := range map[string][]delivery.CodeGroup{
		codeItemsCode:  {{Name: "agents", BranchPrefixes: []string{"agent/"}}, {Name: "others"}},
		codeItemsTwin:  every,
		codeItemsHide:  every,
		codeItemsLoose: every,
	} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the settings of %s: %v", project, err)
		}
		if _, err := delivery.PutProjectSettings(ctx, tx, delivery.ProjectSettingsInput{Project: project, CodeGroups: &groups}, model.Actor{Kind: "system", ID: "test"}); err != nil {
			t.Fatalf("save %s's code groups: %v", project, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the settings of %s: %v", project, err)
		}
	}
	for _, key := range []string{codeItemsIssue1, codeItemsIssue2} {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": codeItemsCode, "title": "the work of " + key}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", key, created.Code, created.Body.String())
		}
		if got := decodeBody[struct {
			Key string `json:"key"`
		}](t, created).Key; got != key {
			t.Fatalf("the issue created as %s is %s", key, got)
		}
	}

	double := codesurvivaltest.New(t)
	double.Serve(codeItemsRepo, history.branch)
	double.Unreadable(codeItemsHidden)
	double.Refuse(history.blocked, codesurvivaltest.Refusal{Status: 422, Body: "diff too large", Withheld: 3})
	run := &codesurvival.Run{Pool: database.Pool, Client: double.Client(t), Attributor: deps.Attributor, Build: testServerBuild,
		Now: func() time.Time { return codeItemsPass }}
	run.Pass(ctx)
	statuses, err := codesurvival.DayStatuses(ctx, database.Pool, codeItemsRepo, codeItemsDay(0), codeItemsDay(codeItemsMeasured-1))
	if err != nil {
		t.Fatalf("read the days of %s: %v", codeItemsRepo, err)
	}
	if len(statuses) != codeItemsMeasured {
		t.Fatalf("the run stored %d days of %s, want %d: %+v", len(statuses), codeItemsRepo, codeItemsMeasured, statuses)
	}
	for n, status := range statuses {
		want := survival.DayMeasured
		if n <= codeItemsBlockedDay {
			want = survival.DayBlocked
		}
		if status.State != want {
			t.Fatalf("day %d of %s is %s, want %s", n, codeItemsRepo, status.State, want)
		}
	}
	var days []time.Time
	for _, n := range codeItemsMarked {
		days = append(days, codeItemsDay(n))
	}
	marked, err := codesurvival.WriteMarks(ctx, database.Pool, codeItemsRepo, days, codeItemsPass.Add(time.Hour), testServerBuild)
	if err != nil || !slices.Equal(marked, days) {
		t.Fatalf("the marks of %v marked %v (%v)", days, marked, err)
	}
	return &codeItemsRig{handler: handler, history: history}
}

// codeWindow is the query of the window of days [from, to) of the code items' branch.
func codeWindow(from, to int) url.Values {
	return url.Values{"from": {codeItemsDay(from).Format(time.RFC3339)}, "to": {codeItemsDay(to).Format(time.RFC3339)}}
}

// items is every row of slug's eval of project over query, read 200 at a time.
func (r *codeItemsRig) items(t *testing.T, project, slug string, query url.Values) []evalItemRow {
	t.Helper()
	var rows []evalItemRow
	for offset := 0; ; offset += evalPageMax {
		paged := url.Values{"limit": {fmt.Sprint(evalPageMax)}, "offset": {fmt.Sprint(offset)}}
		for key, values := range query {
			paged[key] = values
		}
		page := evalGet[evalItemsResponse](t, r.handler, evalItemsPath(project, slug)+"?"+paged.Encode())
		rows = append(rows, page.Items...)
		if offset+evalPageMax >= page.Total {
			return rows
		}
	}
}

// codeList is the list read of project's delivery evals over the window of days [from, to): each
// code eval by slug.
func (r *codeItemsRig) codeList(t *testing.T, project string, from, to int) map[string]evalItem {
	t.Helper()
	query := codeWindow(from, to)
	query.Set("kind", evals.KindDelivery)
	query.Set("limit", "200")
	list := evalGet[evalListResponse](t, r.handler, "/api/v1/projects/"+project+"/evals?"+query.Encode())
	return map[string]evalItem{
		evals.SlugCodeSurvival:    findEval(t, list.Items, project, evals.SlugCodeSurvival),
		evals.SlugPostMergeRework: findEval(t, list.Items, project, evals.SlugPostMergeRework),
	}
}

// rowsOfKind is the rows of kind among rows.
func rowsOfKind(rows []evalItemRow, kind string) []evalItemRow {
	return slices.DeleteFunc(slices.Clone(rows), func(row evalItemRow) bool { return row.RowKind != kind })
}

// itemRowKeys is each row's range, kind and key, in order.
func itemRowKeys(rows []evalItemRow) []string {
	keys := make([]string, len(rows))
	for index, row := range rows {
		keys[index] = row.Range + " " + row.RowKind + " " + row.RowKey
	}
	return keys
}

// The columns of a change row and of a pair row (plan, "The items rows"), and of an absent row of
// each cause, sorted.
var (
	codeChangeCols = []string{"added", "alive", "group", "issue", "pr_number", "sha", "share", "via_repository"}
	codePairCols   = []string{"adder_issue", "adder_pr", "days_between", "group", "kind", "lines", "remover_issue", "remover_pr"}
	codeAbsentCols = map[string][]string{
		"marked":          {"cause", "day", "repo", "sha"},
		"blocked":         {"cause", "days", "files", "reason", "repo", "sha"},
		"repo_unreadable": {"cause", "reason", "repo"},
		"no_default_repo": {"cause"},
		"no_group":        {"cause"},
	}
)

// codeSlugs is the two code evals, each with the row kind it answers of what its numbers count.
var codeSlugs = map[string]string{evals.SlugCodeSurvival: "change", evals.SlugPostMergeRework: "pair"}

// colNames is the names of row's columns, sorted.
func colNames(row evalItemRow) []string {
	names := make([]string, 0, len(row.Cols))
	for name := range row.Cols {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// colNumber is row's column name, failing where it holds no number.
func colNumber(t *testing.T, row evalItemRow, name string) float64 {
	t.Helper()
	value, ok := row.Cols[name].(float64)
	if !ok {
		t.Fatalf("%s row %s holds %s %#v, want a number", row.RowKind, row.RowKey, name, row.Cols[name])
	}
	return value
}

// colText is row's column name, "" for null, failing where it holds neither a text nor null.
func colText(t *testing.T, row evalItemRow, name string) string {
	t.Helper()
	switch value := row.Cols[name].(type) {
	case nil:
		return ""
	case string:
		return value
	}
	t.Fatalf("%s row %s holds %s %#v, want a text or null", row.RowKind, row.RowKey, name, row.Cols[name])
	return ""
}

// linksOfRel is row's links of rel.
func linksOfRel(row evalItemRow, rel string) []evals.Link {
	var links []evals.Link
	for _, link := range row.Links {
		if link.Rel == rel {
			links = append(links, link)
		}
	}
	return links
}

// wantCommitURL is the address of sha's commit of repo on GitHub, and wantPullURL that of its pull
// request number.
func wantCommitURL(repo, sha string) string { return "https://github.com/" + repo + "/commit/" + sha }

func wantPullURL(repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}

// wantSettingsPage is the address of project's settings page.
func wantSettingsPage(project string) string { return "/projects/" + project + "/settings" }

// commitSHA is a commit's SHA as GitHub writes one.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// figureWords is a figure as a failure states it.
func figureWords(figure *float64) string {
	if figure == nil {
		return "no data"
	}
	return fmt.Sprint(*figure)
}

// checkCodeRow fails t unless row, a row of kind change, pair or absent of project, holds its
// kind's columns and links: a change's via link to its commit and its pull request's address; a
// pair's via link to the remover's commit, related link to the adder's and the remover's pull
// request's address; a marked day's first change or a blocked commit, with a via link to the
// blocked commit; for a marked day that holds no change, and for each setting, the project's
// settings page and no link. Each commit must be one of the branch's, each pull request the one
// that landed it. It answers the cause of an absent row, "" for any other.
func (r *codeItemsRig) checkCodeRow(t *testing.T, project string, row evalItemRow) string {
	t.Helper()
	commit := func(sha string) {
		t.Helper()
		if _, held := r.history.chain[sha]; !commitSHA.MatchString(sha) || !held {
			t.Errorf("%s row %s names the commit %q, which is no commit of %s's branch", row.RowKind, row.RowKey, sha, codeItemsRepo)
		}
	}
	via := func(sha string) {
		t.Helper()
		if links := linksOfRel(row, "via"); len(links) != 1 || links[0].Href != wantCommitURL(codeItemsRepo, sha) {
			t.Errorf("%s row %s links %+v, want one via link to %s", row.RowKind, row.RowKey, row.Links, wantCommitURL(codeItemsRepo, sha))
		}
	}
	pull := func(sha string, number int) {
		t.Helper()
		if landed, found := r.history.pulls[sha]; !found || landed.Number != number || row.Href != wantPullURL(codeItemsRepo, number) {
			t.Errorf("%s row %s is addressed %q with pull request %d, want the pull request that landed %s (%+v)", row.RowKind, row.RowKey, row.Href, number, sha, landed)
		}
	}
	switch row.RowKind {
	case "change":
		if !slices.Equal(colNames(row), codeChangeCols) {
			t.Errorf("change row %s holds the columns %v, want %v", row.RowKey, colNames(row), codeChangeCols)
			return ""
		}
		sha := colText(t, row, "sha")
		if row.RowKey != codeItemsRepo+"@"+sha || len(row.Links) != 1 {
			t.Errorf("change row %s of %s holds %d links, want it keyed %s@<sha> with one link", row.RowKey, sha, len(row.Links), codeItemsRepo)
		}
		commit(sha)
		via(sha)
		pull(sha, int(colNumber(t, row, "pr_number")))
		if !row.MeasuredAt.Equal(r.history.chain[sha]) || row.Source != "delivery/survival.Measure" {
			t.Errorf("change row %s is measured at %s from %q, want its commit's %s from delivery/survival.Measure", row.RowKey, row.MeasuredAt, row.Source, r.history.chain[sha])
		}
	case "pair":
		if !slices.Equal(colNames(row), codePairCols) {
			t.Errorf("pair row %s holds the columns %v, want %v", row.RowKey, colNames(row), codePairCols)
			return ""
		}
		repo, shas, found := strings.Cut(row.RowKey, "@")
		adder, remover, two := strings.Cut(shas, ">")
		if !found || !two || repo != codeItemsRepo {
			t.Errorf("pair row %s is not keyed %s@<adder>><remover>", row.RowKey, codeItemsRepo)
			return ""
		}
		commit(adder)
		commit(remover)
		via(remover)
		if related := linksOfRel(row, "related"); len(related) != 1 || related[0].Href != wantCommitURL(codeItemsRepo, adder) || len(row.Links) != 2 {
			t.Errorf("pair row %s links %+v, want a via link to the remover's commit and a related link to %s", row.RowKey, row.Links, wantCommitURL(codeItemsRepo, adder))
		}
		pull(remover, int(colNumber(t, row, "remover_pr")))
		if landed := r.history.pulls[adder]; landed.Number != int(colNumber(t, row, "adder_pr")) {
			t.Errorf("pair row %s names the adder's pull request %v, want %d", row.RowKey, row.Cols["adder_pr"], landed.Number)
		}
		if !row.MeasuredAt.Equal(r.history.chain[adder]) {
			t.Errorf("pair row %s is measured at %s, want its adder's commit's %s", row.RowKey, row.MeasuredAt, r.history.chain[adder])
		}
	case "absent":
		cause := colText(t, row, "cause")
		want, known := codeAbsentCols[cause]
		if !known || !slices.Equal(colNames(row), want) {
			t.Errorf("absent row %s of the cause %q holds the columns %v, want those of a known cause %v", row.RowKey, cause, colNames(row), want)
			return cause
		}
		switch cause {
		case "marked", "blocked":
			sha := colText(t, row, "sha")
			if sha == "" && cause == "marked" {
				if row.Href != wantSettingsPage(project) || len(row.Links) != 0 {
					t.Errorf("absent row %s, a marked day holding no change, is addressed %q with links %+v, want %s and none", row.RowKey, row.Href, row.Links, wantSettingsPage(project))
				}
				return cause
			}
			commit(sha)
			if row.Href != wantCommitURL(codeItemsRepo, sha) {
				t.Errorf("absent row %s is addressed %q, want the commit it names, %s", row.RowKey, row.Href, wantCommitURL(codeItemsRepo, sha))
			}
			if cause == "blocked" {
				via(sha)
			} else if day := colText(t, row, "day"); r.history.chain[sha].Format(time.DateOnly) != day || len(row.Links) != 0 {
				t.Errorf("absent row %s names the commit %s of %s for the marked day %s, with links %+v", row.RowKey, sha, r.history.chain[sha].Format(time.DateOnly), day, row.Links)
			}
		default:
			if row.Href != wantSettingsPage(project) || len(row.Links) != 0 {
				t.Errorf("absent row %s (%s) is addressed %q with links %+v, want %s and no link", row.RowKey, cause, row.Href, row.Links, wantSettingsPage(project))
			}
		}
		return cause
	default:
		t.Errorf("checkCodeRow reads no %s row (%s)", row.RowKind, row.RowKey)
	}
	return ""
}

// TestEveryCodeItemRowCarriesAViaLinkToItsCommit (R13, the inventory of the items rows): over the
// 28 measured days of the seed, for both code evals of every project of it, each change row and each
// pair row carries a via link to its commit on GitHub, a commit of the branch that row is of, and
// is addressed to the pull request that landed it; each absent row of a marked day is addressed to
// the day's first change, and each of a blocked commit to that commit with a via link to it; and
// each absent row of no group, no default repository or a repository the App cannot read is
// addressed to the project's settings page and carries no via link. It fails at zero rows, and at a
// row kind or a cause of absence that answers none.
func TestEveryCodeItemRowCarriesAViaLinkToItsCommit(t *testing.T) {
	t.Parallel()
	r := newCodeItemsRig(t)
	kinds := map[string]int{}
	causes := map[string]int{}
	for _, project := range []string{codeItemsCode, codeItemsTwin, codeItemsBare, codeItemsHide, codeItemsLoose} {
		for slug, counted := range codeSlugs {
			for _, row := range r.items(t, project, slug, codeWindow(0, codeItemsMeasured)) {
				if row.RowKind == evalRowDay {
					continue
				}
				if row.RowKind != counted && row.RowKind != "absent" {
					t.Errorf("%s/%s answers a %s row %s, want %s and absent rows beside its day rows", project, slug, row.RowKind, row.RowKey, counted)
					continue
				}
				kinds[row.RowKind]++
				if cause := r.checkCodeRow(t, project, row); cause != "" {
					causes[cause]++
				}
			}
		}
	}
	for _, kind := range []string{"change", "pair", "absent"} {
		if kinds[kind] == 0 {
			t.Errorf("no %s row was answered (%v)", kind, kinds)
		}
	}
	for cause := range codeAbsentCols {
		if causes[cause] == 0 {
			t.Errorf("no absent row names the cause %s (%v)", cause, causes)
		}
	}
	t.Logf("checked the rows %v, the absent rows of the causes %v", kinds, causes)
}

// TestTheCodeEvalItemsAnswerOneShape (k10): both code evals of CODE, over the 20 days after the
// marked ones, with a series and a limit of 7, answer the items route's envelope page by page: each
// page at most 7 rows and the same total, the pages together the rows of one read, a page past the
// last empty; every row in the one shape, its series' rows of the kind its numbers count beside the
// series' day rows; a change row's lines added and alive and its group, and a rework pair's two
// issues, different, its lines and its days between; and every href a GitHub address of the right
// shape or a route of the server, which answers it. It fails below 20 rows of changes and pairs.
func TestTheCodeEvalItemsAnswerOneShape(t *testing.T) {
	t.Parallel()
	r := newCodeItemsRig(t)
	const limit = 7
	counted := 0
	for slug, series := range map[string]string{evals.SlugCodeSurvival: "survival:agents", evals.SlugPostMergeRework: "rework:agents"} {
		query := codeWindow(8, codeItemsMeasured)
		query.Set("series", series)
		whole := r.items(t, codeItemsCode, slug, query)
		var paged []evalItemRow
		total := -1
		for offset := 0; ; offset += limit {
			pageQuery := url.Values{"limit": {fmt.Sprint(limit)}, "offset": {fmt.Sprint(offset)}}
			for key, values := range query {
				pageQuery[key] = values
			}
			response := evalRequest(t, r.handler, http.MethodGet, evalItemsPath(codeItemsCode, slug)+"?"+pageQuery.Encode(), nil, "")
			if response.Code != http.StatusOK {
				t.Fatalf("%s page at %d: status %d, body %s", slug, offset, response.Code, response.Body.String())
			}
			held := checkEvalEnvelope(t, slug+" items", response.Body.Bytes())
			page := decodeBody[evalItemsResponse](t, response)
			if total >= 0 && page.Total != total {
				t.Errorf("%s's page at %d totals %d, the first %d", slug, offset, page.Total, total)
			}
			total = page.Total
			if held > limit || page.Limit != limit || page.Offset != offset {
				t.Errorf("%s's page at %d holds %d rows under limit %d and offset %d, want at most %d", slug, offset, held, page.Limit, page.Offset, limit)
			}
			paged = append(paged, page.Items...)
			if offset >= total {
				if held != 0 {
					t.Errorf("%s's page past its %d rows holds %d", slug, total, held)
				}
				break
			}
		}
		if got, want := itemRowKeys(paged), itemRowKeys(whole); !slices.Equal(got, want) || len(whole) != total {
			t.Errorf("%s's pages hold %v, one read %v of %d", slug, got, want, total)
		}
		for _, row := range whole {
			r.checkShape(t, codeItemsCode, slug, series, row)
			if row.RowKind == codeSlugs[slug] {
				counted++
			}
		}
	}
	if counted < 20 {
		t.Fatalf("the two evals answered %d rows of changes and pairs, want at least 20", counted)
	}
	t.Logf("read %d rows of changes and pairs of %s under their series, %d a page", counted, codeItemsCode, limit)
}

// checkShape fails t unless row, a row of slug's eval of project read under series, is a day row of
// the series whose href the server answers, or a row of the kind the eval's numbers count, of the
// series, in its kind's shape (checkCodeRow) with the figures it states.
func (r *codeItemsRig) checkShape(t *testing.T, project, slug, series string, row evalItemRow) {
	t.Helper()
	if row.Range != evals.RangeWindow {
		t.Errorf("%s row %s is of the range %q, want the window", row.RowKind, row.RowKey, row.Range)
	}
	switch row.RowKind {
	case evalRowDay:
		if row.Cols["series"] != series {
			t.Errorf("%s's day row %s is of the series %v, want %s", slug, row.RowKey, row.Cols["series"], series)
		}
		if followed := evalRequest(t, r.handler, http.MethodGet, row.Href, nil, ""); !strings.HasPrefix(row.Href, "/api/v1/") || followed.Code != http.StatusOK {
			t.Errorf("%s's day row %s is addressed %q, which answers %d", slug, row.RowKey, row.Href, followed.Code)
		}
		return
	case codeSlugs[slug]:
	default:
		t.Errorf("%s answers a %s row %s under %s", slug, row.RowKind, row.RowKey, series)
		return
	}
	r.checkCodeRow(t, project, row)
	if group := colText(t, row, "group"); group != "agents" {
		t.Errorf("%s row %s of the series %s is of the group %q", row.RowKind, row.RowKey, series, group)
	}
	if row.RowKind == "change" {
		added, alive := colNumber(t, row, "added"), colNumber(t, row, "alive")
		share, _ := row.Cols["share"].(float64)
		if added <= 0 || alive < 0 || alive > added || share != alive/added {
			t.Errorf("change row %s states %v lines added, %v alive and the share %v", row.RowKey, added, alive, row.Cols["share"])
		}
		if via, _ := row.Cols["via_repository"].(bool); via != (colText(t, row, "issue") == "") {
			t.Errorf("change row %s of the issue %v states via_repository %v", row.RowKey, row.Cols["issue"], row.Cols["via_repository"])
		}
		return
	}
	adder, remover := colText(t, row, "adder_issue"), colText(t, row, "remover_issue")
	if colText(t, row, "kind") != "rework" || adder == "" || remover == "" || adder == remover {
		t.Errorf("pair row %s under rework:agents is of the kind %v between %q and %q, want rework between two issues", row.RowKey, row.Cols["kind"], adder, remover)
	}
	if lines, days := colNumber(t, row, "lines"), colNumber(t, row, "days_between"); lines <= 0 || days < 0 || days > 30 {
		t.Errorf("pair row %s states %v lines removed %v days after", row.RowKey, lines, days)
	}
}

// TestTheNoIssuePairsAnswerUnderTheirOwnSeriesAlone (control 7 of method-md, k10's negative
// control): over CODE's 20 measured days after the marked ones, each pair of the rework eval answers
// under exactly one of its group's four series, the one its kind names; no pair where either change
// has no issue answers under rework:<g> or iteration:<g>, and one of the same group answers under
// noissue:<g>; and each series' lines in the list read are those of its pairs, other:<g>'s at least
// those, so a pair with no issue counted as one of a different issue would raise rework:<g> above
// its rows. It fails when it finds no pair with no issue.
func TestTheNoIssuePairsAnswerUnderTheirOwnSeriesAlone(t *testing.T) {
	t.Parallel()
	r := newCodeItemsRig(t)
	const from, to = 8, codeItemsMeasured
	all := rowsOfKind(r.items(t, codeItemsCode, evals.SlugPostMergeRework, codeWindow(from, to)), "pair")
	listed := r.codeList(t, codeItemsCode, from, to)[evals.SlugPostMergeRework]
	answered := map[string]string{}
	noIssue := 0
	for _, group := range []string{"agents", "others"} {
		for _, base := range []string{fold.SeriesRework, fold.SeriesIteration, fold.SeriesNoIssue, fold.SeriesOther} {
			series := fold.GroupSeries(base, group)
			query := codeWindow(from, to)
			query.Set("series", series)
			lines := 0.0
			for _, row := range rowsOfKind(r.items(t, codeItemsCode, evals.SlugPostMergeRework, query), "pair") {
				if earlier, twice := answered[row.RowKey]; twice {
					t.Errorf("the pair %s answers under %s and %s", row.RowKey, earlier, series)
				}
				answered[row.RowKey] = series
				adder, remover := colText(t, row, "adder_issue"), colText(t, row, "remover_issue")
				if colText(t, row, "kind") != base || colText(t, row, "group") != group {
					t.Errorf("the pair %s under %s is of the kind %v and the group %v", row.RowKey, series, row.Cols["kind"], row.Cols["group"])
				}
				withNone := adder == "" || remover == ""
				switch {
				case withNone && (base == fold.SeriesRework || base == fold.SeriesIteration):
					t.Errorf("the pair %s, between %q and %q, answers under %s: a pair with no issue is counted as an issue's", row.RowKey, adder, remover, series)
				case !withNone && base == fold.SeriesNoIssue:
					t.Errorf("the pair %s, between %s and %s, answers under %s", row.RowKey, adder, remover, series)
				case withNone && base == fold.SeriesNoIssue:
					noIssue++
				}
				lines += colNumber(t, row, "lines")
			}
			figure := seriesValues(t, listed, series).Window["lines"]
			if base == fold.SeriesOther {
				// other:<g> also counts the lines that commits which merged no pull request removed,
				// which no pair stands for.
				if figure == nil || *figure < lines {
					t.Errorf("the list read's %s removes %s lines, its pairs %g", series, figureWords(figure), lines)
				}
				continue
			}
			if figure == nil || *figure != lines {
				t.Errorf("the list read's %s removes %s lines, its pairs %g", series, figureWords(figure), lines)
			}
		}
	}
	for _, row := range all {
		if answered[row.RowKey] == "" {
			t.Errorf("the pair %s answers under no series of its group", row.RowKey)
		}
	}
	if len(answered) != len(all) || noIssue == 0 {
		t.Fatalf("the series answer %d pairs, the read of every series %d, %d of them under noissue with no issue: want them alike and some with no issue", len(answered), len(all), noIssue)
	}
	t.Logf("%d pairs, each under its one series; %d with no issue, under noissue alone", len(all), noIssue)
}

// TestTheRepositoryNoteCountEqualsTheChangeRowsWithNoIssue (k11's API half, k16): for CODE and for
// TWIN, which reads CODE's repository with one group, over every window of seven days and the
// whole of the 20 days after the marked ones, each group's via_repository in the list read, in both
// code evals, is the count of the survival eval's change rows of that group with no issue, and its
// changes the count of its change rows; TWIN's rows are the changes with no issue alone. It fails
// when it counts no change with no issue.
func TestTheRepositoryNoteCountEqualsTheChangeRowsWithNoIssue(t *testing.T) {
	t.Parallel()
	r := newCodeItemsRig(t)
	windows := [][2]int{{8, codeItemsMeasured}}
	for from := 8; from+7 <= codeItemsMeasured; from++ {
		windows = append(windows, [2]int{from, from + 7})
	}
	counted := 0
	for _, project := range []string{codeItemsCode, codeItemsTwin} {
		groups := []string{"agents", "others"}
		if project == codeItemsTwin {
			groups = []string{"all"}
		}
		for _, window := range windows {
			listed := r.codeList(t, project, window[0], window[1])
			for _, group := range groups {
				query := codeWindow(window[0], window[1])
				query.Set("series", fold.GroupSeries(fold.SeriesSurvival, group))
				rows := rowsOfKind(r.items(t, project, evals.SlugCodeSurvival, query), "change")
				via := 0
				for _, row := range rows {
					if colText(t, row, "issue") == "" {
						via++
					} else if project == codeItemsTwin {
						t.Errorf("%s answers the change %s of the issue %v, an issue of %s", project, row.RowKey, row.Cols["issue"], codeItemsCode)
					}
				}
				counted += via
				for slug, base := range map[string]string{evals.SlugCodeSurvival: fold.SeriesSurvival, evals.SlugPostMergeRework: fold.SeriesRework} {
					values := seriesValues(t, listed[slug], fold.GroupSeries(base, group)).Window
					if values["via_repository"] == nil || *values["via_repository"] != float64(via) || values["changes"] == nil || *values["changes"] != float64(len(rows)) {
						t.Errorf("%s's %s:%s over days %d to %d states via_repository %s of %s changes; its change rows %d with no issue of %d",
							project, base, group, window[0], window[1], figureWords(values["via_repository"]), figureWords(values["changes"]), via, len(rows))
					}
				}
			}
		}
	}
	if counted == 0 {
		t.Fatal("no window counted a change with no issue")
	}
	t.Logf("%d windows of two projects, %d changes with no issue counted against the notes", len(windows), counted)
}

// TestAnAbsentRowNamesEachReason (R7 to R11): each reason a range reads "no data" other than days
// still waiting answers one absent row in both code evals, naming it: over the 28 measured days,
// CODE's blocked days name day 3's commit, the count of its three withheld files, the refusal and
// the days it blocks, and its two marked days each name the day and its first change; over the 20
// days after them CODE answers none, and BARE, which names no group, that alone; HIDDEN names its
// default repository, which the App cannot read, with the reason; and LOOSE names no default
// repository. A compare range's rows are of the range compare.
func TestAnAbsentRowNamesEachReason(t *testing.T) {
	t.Parallel()
	r := newCodeItemsRig(t)
	var blocked []string
	for n := range codeItemsBlockedDay + 1 {
		blocked = append(blocked, codeItemsDay(n).Format(time.DateOnly))
	}
	marked := func(n int) string {
		return fmt.Sprintf("marked day %s of %s, first change %s", codeItemsDay(n).Format(time.DateOnly), codeItemsRepo, r.history.firstChangeOf(n))
	}
	cases := []struct {
		project  string
		from, to int
		want     []string
	}{
		{codeItemsCode, 0, codeItemsMeasured, []string{
			fmt.Sprintf("blocked by %s of %s, 3 files, diff refused (422), files[] withheld 3 of 12 files, days %v", r.history.blocked, codeItemsRepo, blocked),
			marked(6), marked(7),
		}},
		{codeItemsCode, 8, codeItemsMeasured, nil},
		{codeItemsBare, 8, codeItemsMeasured, []string{"no group"}},
		{codeItemsHide, 0, codeItemsMeasured, []string{"unreadable " + codeItemsHidden}},
		{codeItemsLoose, 0, codeItemsMeasured, []string{"no default repository"}},
	}
	named := 0
	for _, c := range cases {
		for slug := range codeSlugs {
			query := codeWindow(c.from, c.to)
			query.Set("row_kind", "absent")
			var got []string
			for _, row := range r.items(t, c.project, slug, query) {
				if row.RowKind != "absent" {
					t.Errorf("%s/%s answers a %s row under row_kind=absent", c.project, slug, row.RowKind)
					continue
				}
				r.checkCodeRow(t, c.project, row)
				got = append(got, absentWords(t, row))
			}
			slices.Sort(got)
			if want := slices.Sorted(slices.Values(c.want)); !slices.Equal(got, want) {
				t.Errorf("%s/%s over days %d to %d names %q, want %q", c.project, slug, c.from, c.to, got, want)
			}
			named += len(got)
		}
	}
	query := codeWindow(8, codeItemsMeasured)
	query.Set("compare_from", codeItemsDay(0).Format(time.RFC3339))
	query.Set("compare_to", codeItemsDay(8).Format(time.RFC3339))
	query.Set("row_kind", "absent")
	compared := r.items(t, codeItemsCode, evals.SlugCodeSurvival, query)
	for _, row := range compared {
		if row.Range != evals.RangeCompare {
			t.Errorf("the absent row %s of CODE's window after the marked days, with a compare range holding them, is of the range %q", row.RowKey, row.Range)
		}
	}
	if len(compared) != 3 {
		t.Errorf("the compare range of days 0 to 8 names %d reasons, want the blocking commit and the two marked days", len(compared))
	}
	if named == 0 {
		t.Fatal("no absent row was answered")
	}
	t.Logf("%d absent rows named their reasons", named)
}

// absentWords is what an absent row names, in words a failure shows.
func absentWords(t *testing.T, row evalItemRow) string {
	t.Helper()
	switch cause := colText(t, row, "cause"); cause {
	case "marked":
		return fmt.Sprintf("marked day %s of %s, first change %s", colText(t, row, "day"), colText(t, row, "repo"), colText(t, row, "sha"))
	case "blocked":
		var days []string
		list, _ := row.Cols["days"].([]any)
		for _, day := range list {
			days = append(days, fmt.Sprint(day))
		}
		return fmt.Sprintf("blocked by %s of %s, %g files, %s, days %v", colText(t, row, "sha"), colText(t, row, "repo"), colNumber(t, row, "files"), colText(t, row, "reason"), days)
	case "repo_unreadable":
		if colText(t, row, "reason") == "" {
			t.Errorf("the absent row %s names no reason the App cannot read %s", row.RowKey, colText(t, row, "repo"))
		}
		return "unreadable " + colText(t, row, "repo")
	case "no_default_repo":
		return "no default repository"
	case "no_group":
		return "no group"
	default:
		return "cause " + cause
	}
}

// TestAForcePushMarkReadsNoDataOnBothReadPaths (k7, R8): once codesurvival.WriteMarks has marked
// days 6 and 7 of the measured repository, and before any further run, every window of 1, 2, 7 or
// 14 days, and the whole 28, that holds a marked day reads "no data" in both code evals of each
// project of the repository that names groups, on both read paths: the list read states no figure
// of any group series, and its status names the oldest marked day the window holds
// (first_marked_day); the items read answers an absent row naming each marked day it holds, the
// marked day's status row with marked 1 alone, and no change or pair row of a marked day. A window
// that holds no marked or blocked day reads a number, so the read can see one. It fails at a
// number.
func TestAForcePushMarkReadsNoDataOnBothReadPaths(t *testing.T) {
	t.Parallel()
	r := newCodeItemsRig(t)
	windows := [][2]int{{0, codeItemsMeasured}}
	for _, length := range []int{1, 2, 7, 14} {
		for from := 0; from+length <= codeItemsMeasured; from++ {
			windows = append(windows, [2]int{from, from + length})
		}
	}
	holding, numbers := 0, 0
	for _, project := range []string{codeItemsCode, codeItemsTwin} {
		group := fold.GroupSeries(fold.SeriesSurvival, "agents")
		if project == codeItemsTwin {
			group = fold.GroupSeries(fold.SeriesSurvival, "all")
		}
		for _, window := range windows {
			var held []string
			for n := window[0]; n < window[1]; n++ {
				if slices.Contains(codeItemsMarked, n) {
					held = append(held, codeItemsDay(n).Format(time.DateOnly))
				}
			}
			listed := r.codeList(t, project, window[0], window[1])
			if len(held) == 0 {
				if window[0] > codeItemsBlockedDay && seriesValues(t, listed[evals.SlugCodeSurvival], group).Window["share"] != nil {
					numbers++
				}
				continue
			}
			holding++
			oldest, _ := time.Parse(time.DateOnly, held[0])
			for slug, item := range listed {
				for _, values := range item.Series {
					if values.Series.Series == fold.SeriesStatus {
						if day := values.Window["first_marked_day"]; day == nil || *day != float64(oldest.Unix()) {
							t.Errorf("%s/%s over days %d to %d names the first marked day %s, want %s's", project, slug, window[0], window[1], figureWords(day), held[0])
						}
						continue
					}
					for figure, value := range values.Window {
						if value != nil {
							t.Errorf("%s/%s's %s over days %d to %d, which hold the marked days %v, reads %s %g", project, slug, values.Series.Series, window[0], window[1], held, figure, *value)
						}
					}
				}
				var names []string
				for _, row := range r.items(t, project, slug, codeWindow(window[0], window[1])) {
					switch row.RowKind {
					case "absent":
						if colText(t, row, "cause") == "marked" {
							names = append(names, colText(t, row, "day"))
						}
					case "change", "pair":
						if day := row.MeasuredAt.UTC().Format(time.DateOnly); slices.Contains(held, day) {
							t.Errorf("%s/%s answers the %s row %s of the marked day %s", project, slug, row.RowKind, row.RowKey, day)
						}
					case evalRowDay:
						if day, _ := row.Cols["day"].(string); !slices.Contains(held, day) {
							continue
						}
						counts, _ := row.Cols["counts"].(map[string]any)
						if row.Cols["series"] != fold.SeriesStatus || counts[fold.CountMarked] != float64(1) {
							t.Errorf("%s/%s holds the day row %s of a marked day, of %v with %v, want its status row alone, marked", project, slug, row.RowKey, row.Cols["series"], counts)
						}
					}
				}
				if slices.Sort(names); !slices.Equal(names, held) {
					t.Errorf("%s/%s over days %d to %d names the marked days %v, want %v", project, slug, window[0], window[1], names, held)
				}
			}
		}
	}
	if holding == 0 || numbers == 0 {
		t.Fatalf("%d windows held a marked day and %d others read a number: want some of each", holding, numbers)
	}
	t.Logf("%d windows held a marked day and read no data on both paths; %d others read a number", holding, numbers)
}
