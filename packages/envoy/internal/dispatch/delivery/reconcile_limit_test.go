package delivery

// A rate limit is on one installation of the App. These tests hold that a limit of one
// installation ends no step of a reconcile pass for another installation, and that each limit
// names the installation that it is on.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The repositories of the limit tests. limitAcme is on installation 1, the fake's default.
// limitBeta and limitDeploy are on installation 2. limitDeploy is the deploy repository.
const (
	limitAcme   = "acme/widgets"
	limitBeta   = "beta/tools"
	limitDeploy = "beta/deploy"
)

// limitSearchHold is how long the fake holds the first search of installation 2 before it
// answers. A search that a limit of installation 1 cancels ends before this time.
const limitSearchHold = 5 * time.Second

// limitGitHub is GitHub for the limit tests. batchGitHub answers the batched query of facts. Each
// REST read of limitAcme answers 403 with no requests remaining. Each query of installation 1
// answers RATE_LIMITED (limitInstallation). The search of installation 2 finds found, after
// limitSearchHold the first time. Each REST read of another repository answers from pulls.
type limitGitHub struct {
	*batchGitHub
	found      map[string]any
	holdSearch atomic.Bool
}

func serveLimitGitHub(t *testing.T) *limitGitHub {
	t.Helper()
	fake := newFakeGitHub(t)
	fake.installations = []fakeInstallation{{id: 1, repos: []string{limitAcme}}, {id: 2, repos: []string{limitBeta, limitDeploy}}}
	fake.installationOf = map[string]int64{limitBeta: 2, limitDeploy: 2}
	g := &limitGitHub{batchGitHub: &batchGitHub{t: t, fake: fake, pulls: map[string]map[int]batchPull{}, gone: map[string]bool{}, pullReads: map[string]int{}}}
	g.holdSearch.Store(true)
	g.limitInstallation(1)
	fake.facts = g.answerQuery
	fake.handle("POST /graphql", g.answerGraphQL)
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", g.answerRESTPull)
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}/commits", g.answerRESTCommits)
	for _, workflow := range []string{".github%2Fworkflows%2Fdeploy.yml", ".github%2Fworkflows%2Fpr-checks.yml"} {
		fake.handle("GET /repos/"+limitDeploy+"/actions/workflows/"+workflow+"/runs", func(w http.ResponseWriter, _ *http.Request) {
			mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
		})
	}
	return g
}

// answerGraphQL answers the merged search and the absence query. A search of installation 1
// answers RATE_LIMITED. The first search of installation 2 waits limitSearchHold, or until the
// client cancels it. Then it answers found.
func (g *limitGitHub) answerGraphQL(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.t.Errorf("read a GraphQL request: %v", err)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if !bytes.Contains(body, []byte("search(query:")) {
		g.answerAbsence(w, r)
		return
	}
	if installationIDFromToken(r) == 1 {
		answerGraphQLRateLimit(w)
		return
	}
	if g.holdSearch.CompareAndSwap(true, false) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(limitSearchHold):
		}
	}
	mustEncode(g.t, w, searchResponseJSON(1, []map[string]any{g.found}, false, ""))
}

func (g *limitGitHub) answerRESTPull(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	number, _ := strconv.Atoi(r.PathValue("number"))
	if repo == limitAcme {
		answerRESTRateLimit(w)
		return
	}
	g.mu.Lock()
	g.pullReads[fmt.Sprintf("%s#%d", repo, number)]++
	pull, held := g.pulls[repo][number]
	g.mu.Unlock()
	if !held {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	mustEncode(g.t, w, limitRESTPull(repo, number, pull))
}

func (g *limitGitHub) answerRESTCommits(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	number, _ := strconv.Atoi(r.PathValue("number"))
	if repo == limitAcme {
		answerRESTRateLimit(w)
		return
	}
	g.mu.Lock()
	pull := g.pulls[repo][number]
	g.mu.Unlock()
	answerCommitPage(g.t, w, r, pull)
}

// answerCommitPage answers one page of the commits of pull, as GitHub pages them: per_page of
// them from page, oldest first. A request with no page asks for page 1, and one with no per_page
// asks for 30. Each commit has its message and its author's time.
func answerCommitPage(t *testing.T, w http.ResponseWriter, r *http.Request, pull batchPull) {
	page, perPage := 1, 30
	if value := r.URL.Query().Get("page"); value != "" {
		page, _ = strconv.Atoi(value)
	}
	if value := r.URL.Query().Get("per_page"); value != "" {
		perPage, _ = strconv.Atoi(value)
	}
	from, to := min(len(pull.messages), (page-1)*perPage), min(len(pull.messages), page*perPage)
	committed := pull.updatedAt.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	commits := make([]map[string]any, 0, to-from)
	for i, message := range pull.messages[from:to] {
		commits = append(commits, map[string]any{"sha": fakeSHA("d", from+i+1), "commit": map[string]any{
			"message": message, "author": map[string]any{"date": committed},
		}})
	}
	mustEncode(t, w, commits)
}

// limitRESTPull is pull as GET /repos/{owner}/{repo}/pulls/{number} answers it, with the fields
// that the completion of a timeline row reads: the author, the labels, the creation time and the
// counts of changed lines.
func limitRESTPull(repo string, number int, pull batchPull) map[string]any {
	answer := pull.rest(repo, number)
	answer["user"] = map[string]any{"login": "octocat"}
	answer["labels"] = []any{}
	answer["created_at"] = pull.updatedAt.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	answer["additions"], answer["deletions"] = 3, 1
	return answer
}

// answerRESTRateLimit answers a REST call as GitHub answers one past its installation's budget:
// 403, no requests remaining, and the time the budget resets.
func answerRESTRateLimit(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 1."}`)
}

// storePartialRow stores a partial timeline row of repo#number, as the merged search stores one:
// with no merge commit, no counts of changed lines and no first commit.
func storePartialRow(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) {
	t.Helper()
	created := lineEventsFrom.Add(time.Duration(number) * time.Second)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repo, Number: number, Title: "the title of the search", URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		Author: "octocat", CreatedAt: &created, Partial: true,
	}); err != nil {
		t.Fatalf("store the partial row of %s#%d: %v", repo, number, err)
	}
}

// timelineRowOf reads the timeline row of repo#number, and found false where there is none.
func timelineRowOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) (row DeliveryPullRequest, found bool) {
	t.Helper()
	row, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, repo, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryPullRequest{}, false
	}
	if err != nil {
		t.Fatalf("read the timeline row of %s#%d: %v", repo, number, err)
	}
	return row, true
}

// A rate limit of one installation ends no step of a reconcile pass for another installation.
// GitHub answers each REST call of acme/widgets (installation 1) with 403 and no requests
// remaining, and each query of installation 1 with RATE_LIMITED. Five pull requests of beta/tools
// (installation 2) wait: an owed read (#11), an open stale head (#12), a merged row with no facts
// (#13), a partial timeline row (#14), and a pull request that only the merged search finds
// (#15). acme/widgets has a row of each kind too, with reconcilePartialConcurrency partial rows,
// which the completions list before #14. The client keeps GitHub's limits, as the server's does.
//
// After one runOnce, want each row of beta/tools read and written, and each partial row of
// acme/widgets still partial. Want the pass's failure to name each of the three steps and the limit
// of installation 1. Want the head pass's line to name the scope of its first limit, the REST limit
// of installation 1. A step that ends every installation at the first limit fails: the completions
// leave #14 partial, and the merged search does not store #15.
func TestALimitOfOneInstallationEndsNoStepForAnother(t *testing.T) {
	fastPageRetries(t)
	logs := captureLogsAt(t, slog.LevelDebug)
	pool, ctx := deliveryTestPool(t)
	if _, err := PutSettings(ctx, pool, DeliverySettings{
		DeployRepo: limitDeploy, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{"deploy / deploy"},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "delivery-test"}); err != nil {
		t.Fatalf("store the delivery settings: %v", err)
	}
	github := serveLimitGitHub(t)
	client := github.fake.newTestClient(githubapp.WithLimits(ratelimit.New(pool)))
	now := time.Now().UTC()
	updated := now.Add(-time.Hour).Truncate(time.Second)

	const owed, open, noFacts, partial, found = 11, 12, 13, 14, 15
	for _, number := range []int{owed, noFacts, partial, found} {
		github.set(limitBeta, number, mergedPull(number, updated))
	}
	github.set(limitBeta, open, openPull(open, updated))
	github.found = map[string]any{
		"number": found, "title": changeTitle(found), "url": fmt.Sprintf("https://github.com/%s/pull/%d", limitBeta, found),
		"author": map[string]any{"login": "octocat"}, "createdAt": updated.Add(-24 * time.Hour).Format(time.RFC3339),
		"mergedAt": updated.Format(time.RFC3339), "labels": map[string]any{"nodes": []any{}},
	}
	limitedPartials := numbersFrom(30, reconcilePartialConcurrency)
	for repo, numbers := range map[string][]int{limitBeta: {owed, open, noFacts, partial}, limitAcme: {21, 22, 23}} {
		if err := owePullRequestRead(ctx, pool, repo, numbers[0], owedFailure, ""); err != nil {
			t.Fatalf("owe the read of %s#%d: %v", repo, numbers[0], err)
		}
		storeOpenRow(t, ctx, pool, repo, numbers[1], now.Add(-time.Hour))
		storeRowsWithNoFacts(t, ctx, pool, repo, []int{numbers[2]}, nil, changeTitle)
	}
	storePartialRow(t, ctx, pool, limitBeta, partial)
	for _, number := range limitedPartials {
		storePartialRow(t, ctx, pool, limitAcme, number)
	}

	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)

	if rows, left := headRowsOf(t, ctx, pool, limitBeta, owed), owedCount(t, ctx, pool, limitBeta, owed); rows != 1 || left != 0 {
		t.Errorf("the owed read %s#%d has %d results rows and %d owed reads after the pass, want 1 and 0", limitBeta, owed, rows, left)
	}
	if head := readHead(t, ctx, pool, limitBeta, open); head.HeadSHA != fakeSHA("h", open) {
		t.Errorf("the open row %s#%d holds the head %q after the pass, want GitHub's %q", limitBeta, open, head.HeadSHA, fakeSHA("h", open))
	}
	if read := readNumbers(t, ctx, pool, limitBeta); !slices.Contains(read, noFacts) {
		t.Errorf("the pass stored the facts of %s %v, want #%d among them", limitBeta, read, noFacts)
	}
	if row, stored := timelineRowOf(t, ctx, pool, limitBeta, partial); !stored || row.Partial || row.FirstCommitAt == nil {
		t.Errorf("the partial row %s#%d after the pass: stored %v, partial %v, first commit %v; want it complete", limitBeta, partial, stored, row.Partial, row.FirstCommitAt)
	}
	if row, stored := timelineRowOf(t, ctx, pool, limitBeta, found); !stored || row.Partial {
		t.Errorf("the searched pull request %s#%d after the pass: stored %v, partial %v; want it stored and complete", limitBeta, found, stored, row.Partial)
	}
	for _, number := range limitedPartials {
		if row, stored := timelineRowOf(t, ctx, pool, limitAcme, number); !stored || !row.Partial {
			t.Errorf("the partial row %s#%d after the pass: stored %v, partial %v; want it partial, since GitHub answered each read with a limit", limitAcme, number, stored, row.Partial)
		}
	}

	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	scope := githubapp.InstallationScope(1)
	switch {
	case settings.LastError == nil:
		t.Errorf("the pass recorded no failure, want the limit of %s", scope)
	case !strings.Contains(*settings.LastError, scope):
		t.Errorf("the pass recorded the failure %q, want one that names the limit of %s", *settings.LastError, scope)
	default:
		for _, step := range []string{"reconcile merged pull requests: ", "reconcile partial pull requests: ", "reconcile pull request heads: "} {
			if !strings.Contains(*settings.LastError, step) {
				t.Errorf("the pass recorded the failure %q, want one that names the step %q", *settings.LastError, step)
			}
		}
	}
	if settings.LastReconcileAt != nil {
		t.Errorf("the pass recorded a success at %v, want none: it met a limit", settings.LastReconcileAt)
	}
	lines := attributionLines(logs())
	if len(lines) != 1 || lines[0]["rate_limit_scope"] != scope {
		t.Errorf("the head pass logged the lines %v, want one that names rate_limit_scope %s", lines, scope)
	}
}

// A REST rate limit names the installation of the token that its read used. Each case answers one
// REST read of a partial row's completion with 403 and no requests remaining: the read of the
// pull request, the read of its first commit, or a page of its commit messages. The client keeps
// no limits, so each limit is GitHub's answer, and not a held limit, which names its scope already.
// Want the completions to answer that limit under the scope of installation 1. A read that answers
// a limit with no scope fails (readGitHubPage).
func TestARestRateLimitNamesTheInstallationOfItsToken(t *testing.T) {
	for _, limitedRead := range []string{"pull", "first_commit", "messages_page"} {
		t.Run(limitedRead, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 41
			storePartialRow(t, ctx, pool, limitAcme, number)
			pull := mergedPull(number, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
			fake := newFakeGitHub(t)
			var answered atomic.Int32
			fake.handle(fmt.Sprintf("GET /repos/%s/pulls/%d", limitAcme, number), func(w http.ResponseWriter, r *http.Request) {
				if limitedRead == "pull" {
					answered.Add(1)
					answerRESTRateLimit(w)
					return
				}
				mustEncode(t, w, limitRESTPull(limitAcme, number, pull))
			})
			fake.handle(fmt.Sprintf("GET /repos/%s/pulls/%d/commits", limitAcme, number), func(w http.ResponseWriter, r *http.Request) {
				first := r.URL.Query().Get("per_page") == "1"
				if limitedRead == "first_commit" && first || limitedRead == "messages_page" && !first {
					answered.Add(1)
					answerRESTRateLimit(w)
					return
				}
				answerCommitPage(t, w, r, pull)
			})

			err := NewReconcile(pool, fake.newTestClient(), events.NewBroker()).reconcilePartialPullRequests(ctx, newAbsenceBudget())

			if answered.Load() == 0 {
				t.Fatalf("GitHub answered no read with a limit, want the %s read limited", limitedRead)
			}
			limited, ok := githubapp.AsRateLimit(err)
			if want := githubapp.InstallationScope(1); !ok || limited.Held || limited.Scope != want {
				t.Errorf("the completions answered %v (a limit %v), want GitHub's limit of the %s read under %s", err, ok, limitedRead, want)
			}
		})
	}
}

// limitRun is a completed run id of acme/widgets, as GitHub's listing of a workflow's runs gives it.
func limitRun(id int64) map[string]any {
	return map[string]any{
		"id": id, "head_sha": fakeSHA("r", int(id)), "html_url": fmt.Sprintf("https://github.com/%s/actions/runs/%d", limitAcme, id),
		"status": "completed", "conclusion": "success",
		"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z", "updated_at": "2024-01-01T02:10:00Z",
		"head_commit": map[string]any{"timestamp": "2024-01-01T01:55:00Z"}, "pull_requests": []any{},
	}
}

// The error of a page names the attempts that GitHub was really asked, and no more. Three
// workflows of acme/widgets each end at a rate limit. The listing of runs-limited.yml answers its
// first page with a limit. The one run of jobs-limited.yml answers its jobs page with a limit. The
// one run of jobs-retried.yml answers its jobs page with 504 first, which is asked again, and then
// with a limit. Want last_error to name the first two pages with no count of attempts, and the
// third "after 2 attempts". An error that names the retry budget, "after 3 attempts", for a page
// asked once or twice fails (pageAttemptsNote).
func TestAnErrorOfAPageNamesTheAttemptsThatWereMade(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	var runsAsked, limitedJobsAsked, retriedJobsAsked atomic.Int32
	workflow := func(name string) string {
		return "GET /repos/" + limitAcme + "/actions/workflows/.github%2Fworkflows%2F" + name + "/runs"
	}
	fake.handle(workflow("runs-limited.yml"), func(w http.ResponseWriter, _ *http.Request) {
		runsAsked.Add(1)
		answerRESTRateLimit(w)
	})
	fake.handle(workflow("jobs-limited.yml"), func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 1, "workflow_runs": []map[string]any{limitRun(921)}})
	})
	fake.handle(workflow("jobs-retried.yml"), func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 1, "workflow_runs": []map[string]any{limitRun(922)}})
	})
	fake.handle("GET /repos/"+limitAcme+"/actions/runs/921/jobs", func(w http.ResponseWriter, _ *http.Request) {
		limitedJobsAsked.Add(1)
		answerRESTRateLimit(w)
	})
	fake.handle("GET /repos/"+limitAcme+"/actions/runs/922/jobs", func(w http.ResponseWriter, _ *http.Request) {
		if retriedJobsAsked.Add(1) == 1 {
			w.WriteHeader(http.StatusGatewayTimeout)
			fmt.Fprint(w, `{"message":"We couldn't respond to your request in time."}`)
			return
		}
		answerRESTRateLimit(w)
	})
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	var failures []error
	for _, name := range []string{"runs-limited.yml", "jobs-limited.yml", "jobs-retried.yml"} {
		err := reconcile.reconcileWorkflow(ctx, "acme", "widgets", limitAcme, ".github/workflows/"+name, DeliveryRunKindDeploy, Production{}, until)
		if _, ok := githubapp.AsRateLimit(err); !ok {
			t.Errorf("the runs of %s answered %v, want GitHub's rate limit", name, err)
		}
		failures = append(failures, err)
	}
	reconcile.fail(ctx, errors.Join(failures...))

	if runs, limited, retried := runsAsked.Load(), limitedJobsAsked.Load(), retriedJobsAsked.Load(); runs != 1 || limited != 1 || retried != 2 {
		t.Fatalf("GitHub was asked the limited runs page %d times, the limited jobs page %d and the retried jobs page %d, want 1, 1 and 2", runs, limited, retried)
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	if settings.LastError == nil {
		t.Fatal("the pass recorded no failure, want the three limits")
	}
	for _, want := range []string{
		"page 1: rate-limited by GitHub",
		"list jobs of acme/widgets run 921 (page 1): rate-limited by GitHub",
		"list jobs of acme/widgets run 922 (page 1) (after 2 attempts): rate-limited by GitHub",
	} {
		if !strings.Contains(*settings.LastError, want) {
			t.Errorf("last_error = %q, want it to hold %q", *settings.LastError, want)
		}
	}
	if strings.Contains(*settings.LastError, "after 3 attempts") {
		t.Errorf("last_error = %q, want no page named after 3 attempts: no page was asked 3 times", *settings.LastError)
	}
}
