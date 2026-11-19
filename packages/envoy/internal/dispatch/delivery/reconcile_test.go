package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestReconcileCatchesAMissedPullRequest proves the plan's "a missed event appears by the next
// five-minute reconcile": a merged PR intake's NATS consumer never saw (simulating a dropped
// delivery -- no envelope is published anywhere in this test) is found and stored by reconcile's
// own merged-PR search, complete, through the same upsert intake uses.
func TestReconcileCatchesAMissedPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		node := searchNodeJSON(50, "feat: a missed merge", "octocat", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
		node["labels"] = map[string]any{"nodes": []any{map[string]any{"name": "non-task"}}}
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/50", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 50, "title": "feat: a missed merge", "html_url": "https://github.com/acme/widgets/pull/50",
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "deadbeef", "additions": 5, "deletions": 1, "body": "",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/50/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "feat: a missed merge", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}},
		})
	})
	// No workflow runs in this window: the test isolates the merged-PR path.
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 50))
	if err != nil {
		t.Fatalf("scan reconciled pull request: %v", err)
	}
	if pr.Title != "feat: a missed merge" || pr.Author != "octocat" {
		t.Fatalf("reconciled pull request = %+v", pr)
	}
	if pr.Partial {
		t.Error("pr.Partial = true after reconcile's own partial-completion pass, want false")
	}
	if pr.Additions == nil || *pr.Additions != 5 {
		t.Errorf("pr.Additions = %v, want 5", pr.Additions)
	}
	if pr.FirstCommitAt == nil {
		t.Error("pr.FirstCommitAt is nil, want the completing fetch to have filled it in")
	}

	got, err := GetSettings(ctx, pool)
	if err != nil || got.LastReconcileAt == nil || got.LastError != nil {
		t.Errorf("settings freshness after a healthy reconcile pass: %+v, %v", got, err)
	}
	_ = settings
}

// TestReconcileCatchesAMissedDeployRun proves the same for a deploy run intake never saw: it is
// found by the deploy workflow's run listing and upserted with its jobs, and with GitHub's
// created_at beside run_started_at, which a re-run moves past it.
func TestReconcileCatchesAMissedDeployRun(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	settings.ProductionApplyStepName = new("Run ./.github/actions/apply")
	if _, err := PutSettings(ctx, pool, settings, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("set apply step: %v", err)
	}
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{
					"id": 900, "head_sha": "cafef00d", "html_url": "https://github.com/acme/widgets/actions/runs/900",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T02:30:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
					"pull_requests": []any{},
				},
			},
		})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/900/jobs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"jobs": []map[string]any{
				{"name": "widgets-release / widgets-release", "started_at": "2024-01-01T02:00:00Z", "completed_at": "2024-01-01T02:10:00Z", "conclusion": "success", "steps": []map[string]any{{"name": "Run ./.github/actions/apply", "conclusion": "skipped"}}},
			},
		})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	run, err := ScanRun(pool.QueryRow(ctx, `select `+RunColumns+` from delivery_runs where repo = $1 and run_id = $2`, "acme/widgets", int64(900)))
	if err != nil {
		t.Fatalf("scan reconciled run: %v", err)
	}
	if run.Kind != DeliveryRunKindDeploy || run.Conclusion == nil || *run.Conclusion != DeliveryRunConclusionSuccess {
		t.Fatalf("reconciled run = %+v", run)
	}
	if run.CreatedAt == nil || !run.CreatedAt.Equal(time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)) || !run.StartedAt.Equal(time.Date(2024, 1, 1, 2, 30, 0, 0, time.UTC)) {
		t.Fatalf("reconciled run created_at %v, started_at %v, want GitHub's created_at 02:00 and run_started_at 02:30", run.CreatedAt, run.StartedAt)
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 900)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "widgets-release / widgets-release" {
		t.Fatalf("reconciled run jobs = %+v, %v", jobs, err)
	}
	if jobs[0].ApplyStep == nil || *jobs[0].ApplyStep != "Run ./.github/actions/apply" ||
		jobs[0].ApplyStepConclusion == nil || *jobs[0].ApplyStepConclusion != DeliveryJobConclusionSkipped {
		t.Fatalf("reconciled apply step = %+v, want the skipped configured step", jobs[0])
	}
}

// TestReconcileRecordsAMissingPermissionByNameAndDoesNotAdvanceLastReconcileAt proves LEGION-567's
// acceptance bar: a missing GitHub App permission (Actions here) is reported by name in
// delivery_settings.last_error, and last_reconcile_at is left exactly as it was -- never advanced
// as if the pass had actually done something. Blocking per the acceptance gate.
func TestReconcileRecordsAMissingPermissionByNameAndDoesNotAdvanceLastReconcileAt(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	before, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings before reconcile: %v", err)
	}

	fake := newFakeGitHub(t)
	// Contents is still granted, but Actions is missing -- a real misconfiguration shape, not a
	// transient failure.
	fake.permissions = map[string]string{"contents": "read", "pull_requests": "read"}
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError == nil || !strings.Contains(*after.LastError, "Actions") {
		t.Fatalf("settings.LastError = %v, want an error naming the missing Actions permission", after.LastError)
	}
	if !reflect.DeepEqual(after.LastReconcileAt, before.LastReconcileAt) {
		t.Fatalf("settings.LastReconcileAt changed from %v to %v on a pass that never ran a single GitHub call successfully", before.LastReconcileAt, after.LastReconcileAt)
	}
}

// TestReconcileNeverWipesACompletePullRequestsAttribution proves blocking #2 end-to-end through
// the real reconcile path (not just store.go's upsert in isolation): a PR intake already
// completed with real session/issue attribution survives a reconcile pass that re-finds the same
// PR via its merged-PR search, which carries no session data at all.
func TestReconcileNeverWipesACompletePullRequestsAttribution(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 60, Title: "feat: already complete", URL: "https://github.com/acme/widgets/pull/60",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Sessions: []string{"01a1-real-session"}, Partial: false,
	}); err != nil {
		t.Fatalf("seed complete pull request: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		// The search re-finds the same PR, exactly as a real reconcile pass's overlap window
		// would, carrying no session data (search results never do) but a real label so
		// classification succeeds and the upsert actually runs (not short-circuited by an
		// unclassified-PR error, which would prove nothing about the upsert guard).
		node := searchNodeJSON(60, "feat: already complete", "octocat", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
		node["labels"] = map[string]any{"nodes": []any{map[string]any{"name": "non-task"}}}
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 60))
	if err != nil {
		t.Fatalf("scan pull request: %v", err)
	}
	if pr.Partial {
		t.Error("pr.Partial = true after a reconcile pass that re-found an already-complete PR, want false (never regressed)")
	}
	if len(pr.Sessions) != 1 || pr.Sessions[0] != "01a1-real-session" {
		t.Fatalf("pr.Sessions = %v, want [01a1-real-session] (attribution wiped by reconcile's re-discovery)", pr.Sessions)
	}
}

// TestReconcileClearsLastErrorOnceItSucceedsAgain proves the freshness row recovers: a prior
// failing pass's named error must not linger once a later pass actually succeeds --
// RecordReconcileSuccess's own "last_error = null" is exercised here through the real reconcile
// path, not just the bare SQL statement.
func TestReconcileClearsLastErrorOnceItSucceedsAgain(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	if err := RecordReconcileError(ctx, pool, "the installation lacks Actions: read on acme/widgets"); err != nil {
		t.Fatalf("seed a prior failure: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError != nil {
		t.Fatalf("settings.LastError = %v, want nil once a pass has actually succeeded", *after.LastError)
	}
	if after.LastReconcileAt == nil {
		t.Fatal("settings.LastReconcileAt is nil, want it set by the successful pass")
	}
}

// TestReconcileMarksA404PullRequestUnfetchableAndSkipsItUntilItRecovers proves S3: a partial row
// whose completing fetch answers a permanent 404 is marked unfetchable (not retried every pass
// forever), and a later successful fetch of the same pull request clears it again. The 404 alone
// is no proof: GitHub's answer to the absence query (pullRequestAbsent) shows the repository
// readable and the pull request absent, and only that answer marks the row. The head pass's facts
// line reads the row by a batched query (readFactsBatch), which the fake counts apart; the test
// counts the REST reads alone. A completion that reads a marked row makes it fail.
// TestA404ThatGitHubDoesNotProveLeavesAPartialRowForTheNextPass is its control.
func TestReconcileMarksA404PullRequestUnfetchableAndSkipsItUntilItRecovers(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 70, Title: "feat: gone", URL: "https://github.com/acme/widgets/pull/70",
		Author: "octocat", CreatedAt: &merged, Partial: true,
	}); err != nil {
		t.Fatalf("seed partial pull request: %v", err)
	}

	requests := 0
	fake := newFakeGitHub(t)
	// The head pass's facts line reads #70 too, by the batched query, which reads this route with
	// the fake's header. It gets no answer there (422), so the facts line stamps the read refused,
	// and the completion's REST read is the one that meets the 404.
	fake.handle("GET /repos/acme/widgets/pulls/70", func(w http.ResponseWriter, r *http.Request) {
		if !restRead(r) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		requests++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	fake.handle("POST /graphql", answerSearchOrAbsence(t, "acme/widgets", 70, true))
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient()
	reconcile := NewReconcile(pool, client, events.NewBroker())

	reconcile.runOnce(ctx)
	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 70))
	if err != nil {
		t.Fatalf("scan after first pass: %v", err)
	}
	if pr.UnfetchableAt == nil {
		t.Fatal("pr.UnfetchableAt is nil after a 404, want it set")
	}
	if pr.UnfetchableReason == nil || !strings.Contains(*pr.UnfetchableReason, "not found or gone") {
		t.Errorf("pr.UnfetchableReason = %v, want it to name the 404", pr.UnfetchableReason)
	}
	if requests != 1 {
		t.Fatalf("REST requests to GET the pull request = %d, want 1", requests)
	}

	// A second pass must not retry it: ListPartialPullRequests excludes a marked row.
	reconcile.runOnce(ctx)
	if requests != 1 {
		t.Fatalf("REST requests after a second pass = %d, want still 1 (an unfetchable row is never retried)", requests)
	}

	// A later successful fetch of the same pull request (a live webhook retry, here simulated
	// directly through the shared upsert) clears it.
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 70, Title: "feat: gone", URL: "https://github.com/acme/widgets/pull/70",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Partial: false,
	}); err != nil {
		t.Fatalf("simulate a later successful fetch: %v", err)
	}
	recovered, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 70))
	if err != nil {
		t.Fatalf("scan after recovery: %v", err)
	}
	if recovered.UnfetchableAt != nil || recovered.UnfetchableReason != nil {
		t.Errorf("recovered.UnfetchableAt = %v, UnfetchableReason = %v, want both nil after a later successful write", recovered.UnfetchableAt, recovered.UnfetchableReason)
	}
}

// answerSearchOrAbsence answers POST /graphql: the absence query (absenceQuery) about repo#number,
// and an empty search for every other query. With proven true the absence query answers as GitHub
// answers for a readable repository that holds no such pull request: a count of its pull requests
// beside NOT_FOUND on the path repository.pullRequest. With proven false it answers a null
// repository, which proves nothing: GitHub answers so alike for a repository the token does not
// read and for one that does not exist.
func answerSearchOrAbsence(t *testing.T, repo string, number int, proven bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, variables := decodeGraphQLRequest(t, r)
		if !strings.Contains(query, "pullRequest(number: $number)") {
			mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
			return
		}
		owner, _ := variables["owner"].(string)
		name, _ := variables["name"].(string)
		asked, _ := variables["number"].(float64)
		if owner+"/"+name != repo || int(asked) != number || !proven {
			mustEncode(t, w, nullRepositoryAnswer(owner+"/"+name))
			return
		}
		mustEncode(t, w, map[string]any{
			"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"totalCount": 3}, "pullRequest": nil}},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository", "pullRequest"},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)}},
		})
	}
}

// A 404 of a partial row's completing read is no proof that the pull request is gone. Where
// GitHub's absence query does not prove it, the row stays partial and unmarked, and the next pass
// reads it again: no single refusal ends a read.
func TestA404ThatGitHubDoesNotProveLeavesAPartialRowForTheNextPass(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 71, Title: "feat: refused", URL: "https://github.com/acme/widgets/pull/71",
		Author: "octocat", CreatedAt: &merged, Partial: true,
	}); err != nil {
		t.Fatalf("seed partial pull request: %v", err)
	}
	requests := 0
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/71", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			requests++
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	fake.handle("POST /graphql", answerSearchOrAbsence(t, "acme/widgets", 71, false))
	noRuns(t, fake)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())

	for pass := 1; pass <= 2; pass++ {
		reconcile.runOnce(ctx)
		pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 71))
		if err != nil {
			t.Fatalf("scan after pass %d: %v", pass, err)
		}
		if pr.UnfetchableAt != nil || !pr.Partial {
			t.Fatalf("after pass %d #71 has partial %v and unfetchable_at %v, want partial and unmarked: GitHub did not prove it absent",
				pass, pr.Partial, pr.UnfetchableAt)
		}
		if requests != pass {
			t.Fatalf("after pass %d the passes read #71 %d times, want %d: each pass reads an unmarked partial row again", pass, requests, pass)
		}
	}
}

// TestReconcileWorkflowFailureStopsThePassFromReportingSuccess proves S1: a per-run failure (here,
// the jobs listing for a run the runs listing itself found successfully) must not be swallowed as
// a healthy pass -- last_reconcile_at must not advance past a window this pass left incompletely
// fetched.
func TestReconcileWorkflowFailureStopsThePassFromReportingSuccess(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	before, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings before reconcile: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{
					"id": 900, "head_sha": "cafef00d", "html_url": "https://github.com/acme/widgets/actions/runs/900",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
					"pull_requests": []any{},
				},
			},
		})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	// The run listing succeeds, but its jobs listing fails: a run reconcile already knows about,
	// upserted, yet never finished.
	fake.handle("GET /repos/acme/widgets/actions/runs/900/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	run, err := ScanRun(pool.QueryRow(ctx, `select `+RunColumns+` from delivery_runs where repo = $1 and run_id = $2`, "acme/widgets", int64(900)))
	if err != nil {
		t.Fatalf("the run itself must still be upserted even though its jobs failed: %v", err)
	}
	if run.RunID != 900 {
		t.Fatalf("run = %+v", run)
	}

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError == nil {
		t.Fatal("settings.LastError is nil, want the jobs-listing failure recorded by name")
	}
	if !strings.Contains(*after.LastError, "900") {
		t.Errorf("settings.LastError = %q, want it to name run 900", *after.LastError)
	}
	if !reflect.DeepEqual(after.LastReconcileAt, before.LastReconcileAt) {
		t.Fatalf("settings.LastReconcileAt changed from %v to %v on a pass with a real per-run failure", before.LastReconcileAt, after.LastReconcileAt)
	}
}

// TestReconcileMarksA404RunJobsUnfetchableAndSkipsItUntilItRecovers proves Deep's finding: unlike
// TestReconcileWorkflowFailureStopsThePassFromReportingSuccess's transient 500 (which must keep
// failing the pass), a run whose jobs listing answers a permanent 404 must not fail the pass at
// all -- it is marked unfetchable instead, the same treatment S3 gives a permanently 404ing pull
// request -- and a later pass must not even re-call the jobs endpoint for it. reconcileRun itself
// never retries a run it has marked unfetchable (there is no "the window reopened" signal to
// retry on, unlike a partial pull request's own completing fetch); what clears the mark is an
// independent successful write of its jobs -- here, standing in for intake.go's own live-webhook
// UpsertRunJobs call, which already runs through the same shared upsert.
func TestReconcileMarksA404RunJobsUnfetchableAndSkipsItUntilItRecovers(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{
					"id": 901, "head_sha": "deadbeef", "html_url": "https://github.com/acme/widgets/actions/runs/901",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
					"pull_requests": []any{},
				},
			},
		})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	var jobsCalls atomic.Int32
	fake.handle("GET /repos/acme/widgets/actions/runs/901/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobsCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	client := fake.newTestClient()
	reconcile := NewReconcile(pool, client, events.NewBroker())

	reconcile.runOnce(ctx)
	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after first pass: %v", err)
	}
	if after.LastError != nil {
		t.Fatalf("settings.LastError = %q, want nil: a permanently 404ing run's jobs must not fail the whole pass", *after.LastError)
	}
	if after.LastReconcileAt == nil {
		t.Fatal("settings.LastReconcileAt is nil, want it advanced: the pass must be reported healthy")
	}
	if jobsCalls.Load() != 1 {
		t.Fatalf("jobs endpoint called %d times, want 1", jobsCalls.Load())
	}
	unfetchable, err := RunJobsUnfetchable(ctx, pool, "acme/widgets", 901)
	if err != nil {
		t.Fatalf("RunJobsUnfetchable after first pass: %v", err)
	}
	if !unfetchable {
		t.Fatal("RunJobsUnfetchable = false after a permanent 404, want true")
	}

	// A second pass must skip the jobs listing entirely: the run is already marked unfetchable.
	reconcile.runOnce(ctx)
	if jobsCalls.Load() != 1 {
		t.Fatalf("jobs endpoint called %d times after a second pass, want still 1 (skipped once marked unfetchable)", jobsCalls.Load())
	}

	// An independent successful write of this run's jobs (a live webhook retry's own
	// UpsertRunJobs call, intake.go's own write path) clears the mark.
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 901, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 901, Name: "build", StartedAt: ptrTime("2024-01-01T02:00:00Z"), CompletedAt: ptrTime("2024-01-01T02:05:00Z")},
	}, true); err != nil {
		t.Fatalf("UpsertRunJobs (simulating a live webhook retry): %v", err)
	}
	unfetchable, err = RunJobsUnfetchable(ctx, pool, "acme/widgets", 901)
	if err != nil {
		t.Fatalf("RunJobsUnfetchable after recovery: %v", err)
	}
	if unfetchable {
		t.Fatal("RunJobsUnfetchable = true after a successful UpsertRunJobs, want false")
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 901)
	if err != nil {
		t.Fatalf("ListRunJobs after recovery: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "build" {
		t.Fatalf("jobs = %+v, want one job named build once the listing recovers", jobs)
	}
}

// TestReconcilePartialPullRequestsStopsOnRateLimitInsteadOfRetryingAtFullConcurrency proves the
// rate-limit stop end to end: among several partial rows of one installation, the first that
// answers a 403 rate-limit response stops every later completion of that installation from asking
// GitHub (never more than the ones already in flight when it happened), and the pass must be
// reported failed, not successful. The completion keeps a limit and goes on, since a limit is on
// one installation (passRateLimit). The shared limits of the client, as the server's client holds
// them (githubapp.WithLimits), refuse each later read of that installation before it is sent. The
// head pass's facts line reads the rows by a batched query (readFactsBatch), and the fake counts
// it apart. The test counts the completions' REST reads alone. A completion that asks GitHub
// again past the first limit makes it fail.
func TestReconcilePartialPullRequestsStopsOnRateLimitInsteadOfRetryingAtFullConcurrency(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	const total = 20
	for i := 1; i <= total; i++ {
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: "acme/widgets", Number: i, Title: "feat: x", URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", i),
			Author: "octocat", CreatedAt: &merged, Partial: true,
		}); err != nil {
			t.Fatalf("seed partial pull request #%d: %v", i, err)
		}
	}

	var attempts atomic.Int64
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			attempts.Add(1)
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient(githubapp.WithLimits(ratelimit.New(pool)))

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)

	got := attempts.Load()
	if got < 1 {
		t.Fatal("expected at least the first batch of completions to have been attempted")
	}
	if got > reconcilePartialConcurrency {
		t.Errorf("GET pulls attempts = %d, want at most reconcilePartialConcurrency (%d): the rate-limit stop must not let more than one wave start after the first limit response", got, reconcilePartialConcurrency)
	}
	if got >= total {
		t.Errorf("GET pulls attempts = %d out of %d partial rows, want it to stop well short of retrying every row at full concurrency", got, total)
	}

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError == nil {
		t.Fatal("settings.LastError is nil, want the rate limit recorded and the pass reported failed")
	}
}

// TestReconcileWorkflowStopsAtTheFirstRateLimitAndCapsLastError proves the per-run rate-limit
// stop end to end: among three completed runs in one window, the second's jobs listing
// answering a rate limit must stop the third's jobs listing from ever being requested (the
// un-fixed loop would send every remaining run's request after the first limit), the returned
// error must still satisfy errors.As for a *githubapp.RateLimitError (not stringified away into
// the per-run aggregate), and fail()'s own maxLastErrorLength bound still caps what reaches
// delivery_settings.last_error. Removing reconcileWorkflow's unconditional per-run stop on a
// rate limit (reverting to "log and keep going") makes this red: the third run's jobs endpoint
// would be called, defeating the "no further GitHub request" assertion below.
func TestReconcileWorkflowStopsAtTheFirstRateLimitAndCapsLastError(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 3,
			"workflow_runs": []map[string]any{
				{
					"id": 910, "head_sha": "cafef00d", "html_url": "https://github.com/acme/widgets/actions/runs/910",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
					"pull_requests": []any{},
				},
				{
					"id": 911, "head_sha": "deadbeef", "html_url": "https://github.com/acme/widgets/actions/runs/911",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T03:00:00Z", "created_at": "2024-01-01T03:00:00Z",
					"updated_at":    "2024-01-01T03:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T02:55:00Z"},
					"pull_requests": []any{},
				},
				{
					"id": 912, "head_sha": "f00dcafe", "html_url": "https://github.com/acme/widgets/actions/runs/912",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T04:00:00Z", "created_at": "2024-01-01T04:00:00Z",
					"updated_at":    "2024-01-01T04:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T03:55:00Z"},
					"pull_requests": []any{},
				},
			},
		})
	})
	var firstJobsCalls, secondJobsCalls, thirdJobsCalls atomic.Int32
	fake.handle("GET /repos/acme/widgets/actions/runs/910/jobs", func(w http.ResponseWriter, r *http.Request) {
		firstJobsCalls.Add(1)
		mustEncode(t, w, map[string]any{"jobs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/911/jobs", func(w http.ResponseWriter, r *http.Request) {
		secondJobsCalls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/912/jobs", func(w http.ResponseWriter, r *http.Request) {
		thirdJobsCalls.Add(1)
		mustEncode(t, w, map[string]any{"jobs": []any{}})
	})
	client := fake.newTestClient()
	reconcile := NewReconcile(pool, client, events.NewBroker())

	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	err := reconcile.reconcileWorkflow(ctx, "acme", "widgets", "acme/widgets", ".github/workflows/deploy.yml", DeliveryRunKindDeploy, Production{}, until)
	if err == nil {
		t.Fatal("reconcileWorkflow: err = nil, want the second run's rate limit to fail the pass")
	}
	var limited *githubapp.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("errors.As(err, &limited) = false, want true (the typed rate limit must survive the per-run aggregation); err = %v", err)
	}

	if firstJobsCalls.Load() != 1 {
		t.Fatalf("first run's jobs endpoint called %d times, want 1", firstJobsCalls.Load())
	}
	if secondJobsCalls.Load() != 1 {
		t.Fatalf("second (rate-limited) run's jobs endpoint called %d times, want 1", secondJobsCalls.Load())
	}
	if thirdJobsCalls.Load() != 0 {
		t.Fatalf("third run's jobs endpoint called %d times, want 0: the rate limit must stop every further request", thirdJobsCalls.Load())
	}

	reconcile.fail(ctx, err)
	settings, getErr := GetSettings(ctx, pool)
	if getErr != nil {
		t.Fatalf("GetSettings after fail: %v", getErr)
	}
	if settings.LastError == nil {
		t.Fatal("settings.LastError is nil, want the rate limit recorded")
	}
	if len(*settings.LastError) > maxLastErrorLength {
		t.Fatalf("len(settings.LastError) = %d, want capped at maxLastErrorLength (%d)", len(*settings.LastError), maxLastErrorLength)
	}
}

// The head pass keeps a `before_merge` result's answer honest, and it reads no delivery
// setting: the stored heads, the issues and the App are all it needs. A Dispatch whose
// delivery is not configured still has pull requests, checks and results, so a pass there
// must still move a head a push moved — otherwise every result recorded at the head before it
// stands at no basis for good. Driven through runOnce, the entry point that reads the
// settings. reconcilePullRequestHeads never sees them, so calling it would prove nothing. The
// population steps, which are the settings' own, must stay idle in the same pass.
func TestReconcileRefreshesAPullRequestHeadWithNoDeliverySettings(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 5)

	const (
		headWas = "1515151515151515151515151515151515151515"
		headNow = "2525252525252525252525252525252525252525"
	)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/55", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 55, "title": key + " the work", "body": "", "html_url": "https://github.com/acme/widgets/pull/55",
			"state": "open", "merged": false, "updated_at": "2026-02-06T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/" + key, "sha": headNow,
				"repo": map[string]any{"full_name": "acme/widgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/55/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{})
	})

	stale := time.Now().Add(-2 * PullRequestHeadStaleAfter)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 55, HeadSHA: headWas, HeadRef: "legion/" + key, BaseRef: "main",
		State: "open", Title: key + " the work", URL: "https://github.com/acme/widgets/pull/55", CheckedAt: &stale,
	}); err != nil {
		t.Fatalf("store the stale head: %v", err)
	}
	seedStoredIssue(t, ctx, pool, "acme/widgets", 55, &key, "title_body")

	NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)

	head := readHead(t, ctx, pool, "acme/widgets", 55)
	if head.HeadSHA != headNow {
		t.Fatalf("the head after a pass with no delivery settings = %q, want %q: the head pass reads no setting",
			head.HeadSHA, headNow)
	}
	atOld, err := HeadAttributedTo(ctx, pool, key, headWas)
	if err != nil || atOld {
		t.Fatalf("HeadAttributedTo(%s, the head it left) = %v, %v; want false: that commit is no basis",
			key, atOld, err)
	}
	atNew, err := HeadAttributedTo(ctx, pool, key, headNow)
	if err != nil || !atNew {
		t.Fatalf("HeadAttributedTo(%s, the head it moved to) = %v, %v; want true", key, atNew, err)
	}

	// The population is the settings' own: with none, the pass moved none of it.
	var stored int
	if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests`).Scan(&stored); err != nil {
		t.Fatalf("count the stored pull requests: %v", err)
	}
	if stored != 0 {
		t.Fatalf("the pass stored %d delivery pull requests, want none: it has no population to search", stored)
	}
	if deployed := deployedEvents(t, ctx, pool, key); len(deployed) != 0 {
		t.Fatalf("the pass wrote %d issue.deployed events for %s, want none", len(deployed), key)
	}
}

// An earlier pass left a merged pull request credited to nobody, and its row has no texts. The
// facts line of the next pass reads it and attributes it. One batched query (readFactsBatch) reads
// it, and the pass writes the statement's answer to the stored row (SyncAttributions). The pass
// after it reads nothing of the row: its texts are current and its messages are read. The fake
// counts the batched queries apart from the REST reads. A facts line that skips a merged row, a
// write that stores no texts, or a second read makes this test fail.
func TestReconcileAttributesAMergedHeadAnEarlierPassLeftCreditedToNobody(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	foreign := seedHeadIssue(t, ctx, pool, 60)
	own := seedGadgetsIssue(t, ctx, pool, 60)
	const sha = "6060606060606060606060606060606060606060"
	title := foreign + ": the other repository's issue, and " + own + " (this one)"

	var restReads atomic.Int32
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/gadgets/pulls/60", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			restReads.Add(1)
		}
		mustEncode(t, w, map[string]any{
			"number": 60, "title": title, "body": "", "html_url": "https://github.com/acme/gadgets/pull/60",
			"state": "closed", "merged": true, "updated_at": "2026-02-07T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/" + own, "sha": sha,
				"repo": map[string]any{"full_name": "acme/gadgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/gadgets/pulls/60/commits", func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			restReads.Add(1)
		}
		mustEncode(t, w, []any{})
	})

	// The row as an earlier pass left it: merged, credited to nobody, stamped read a day ago, and
	// with no texts.
	stamped := time.Now().Add(-24 * time.Hour)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/gadgets", Number: 60, HeadSHA: sha, HeadRef: "legion/" + own, BaseRef: "main",
		State: "merged",
		Title: title, URL: "https://github.com/acme/gadgets/pull/60", CheckedAt: &stamped,
	}); err != nil {
		t.Fatalf("store the merged head an earlier pass left: %v", err)
	}
	if before := readHead(t, ctx, pool, "acme/gadgets", 60); before.IssueKey != nil {
		t.Fatalf("the merged head before the pass = %v, want nobody: the row has no texts and stores no issue", before.IssueKey)
	}

	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())
	reconcile.runOnce(ctx)

	head := readHead(t, ctx, pool, "acme/gadgets", 60)
	if head.IssueKey == nil || *head.IssueKey != own {
		t.Fatalf("the merged head after a pass = %v, want %s: %s resolves to acme/widgets, %s to this repository",
			head.IssueKey, own, foreign, own)
	}
	if stored := storedIssueOf(t, ctx, pool, "acme/gadgets", 60); stored == nil || *stored != own {
		t.Fatalf("the merged row stores %v after a pass, want %s: the pass writes each answer to the stored row", stored, own)
	}
	attributed, err := HeadAttributedTo(ctx, pool, own, sha)
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, its merged head) = %v, %v; want true: a result stands at it",
			own, attributed, err)
	}
	queries := fake.factsQueries.Load()
	if rest := restReads.Load(); queries != 1 || rest != 0 {
		t.Fatalf("the pass asked %d batched queries and made %d REST reads, want 1 and 0: the facts line reads the row with no texts once",
			queries, rest)
	}

	reconcile.runOnce(ctx)
	if again, rest := fake.factsQueries.Load()-queries, restReads.Load(); again != 0 || rest != 0 {
		t.Fatalf("the next pass asked %d batched queries, and the two passes made %d REST reads; want none: the row has current texts and read messages",
			again, rest)
	}
	if again := readHead(t, ctx, pool, "acme/gadgets", 60); again.IssueKey == nil || *again.IssueKey != own {
		t.Fatalf("the merged head after the next pass = %v, want %s still", again.IssueKey, own)
	}
}

// readRecorder is a githubreads.Recorder that keeps each attempt it is handed, in the order the
// passes recorded them, so a test reads what a pass gave GET /healthz to answer.
type readRecorder struct {
	mu       sync.Mutex
	recorded []recordedAttempt
}

// recordedAttempt is one attempt a readRecorder was handed.
type recordedAttempt struct {
	read      githubreads.Read
	succeeded bool
	cause     string
}

func (r *readRecorder) record(_ context.Context, read githubreads.Read, succeeded bool, cause string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, recordedAttempt{read: read, succeeded: succeeded, cause: cause})
	return nil
}

// take answers the attempts recorded since the last take, and forgets them.
func (r *readRecorder) take() []recordedAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := r.recorded
	r.recorded = nil
	return taken
}

// The routes of the run listings of seedDeliverySettings' two workflows.
const (
	deployRunsRoute   = "GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs"
	prChecksRunsRoute = "GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs"
)

// answerGitHub installs handlers on fake, and on every route handlers does not name GitHub having
// nothing new for a reconcile pass: an empty merged-PR search, and no run of either workflow.
func answerGitHub(t *testing.T, fake *fakeGitHub, handlers map[string]http.HandlerFunc) {
	t.Helper()
	noRuns := func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	}
	answers := map[string]http.HandlerFunc{
		"POST /graphql": func(w http.ResponseWriter, _ *http.Request) {
			mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
		},
		deployRunsRoute:   noRuns,
		prChecksRunsRoute: noRuns,
	}
	maps.Copy(answers, handlers)
	for pattern, handler := range answers {
		fake.handle(pattern, handler)
	}
}

// leasedReconciles answers the pass the reconcile's loop runs under its lease (leasedPass), each
// call due one ReconcileInterval after the one before so the lease's record of the last period
// served never skips it, and the recorder reconcile records each pass's attempt at reading GitHub
// into.
func leasedReconciles(t *testing.T, ctx context.Context, pool *store.Pool, reconcile *Reconcile) (pass func(), reads *readRecorder) {
	t.Helper()
	leases := deliveryTestLeases(t, pool)
	reads = &readRecorder{}
	reconcile.RecordGitHubReads(reads.record)
	due := time.Now()
	return func() {
		reconcile.leasedPass(ctx, leases, due)
		due = due.Add(ReconcileInterval)
	}, reads
}

// Each leased reconcile pass records its attempt at reading GitHub, which GET /healthz answers: a
// pass with no delivery settings and no pull request head to read asks GitHub nothing and records
// nothing; a pass whose first request GitHub answered 503 -- the permission check's installation
// lookup -- records failed, with that answer as its cause; and the next pass, which GitHub
// answered, records succeeded.
func TestEachLeasedReconcilePassRecordsItsAttemptAtReadingGitHub(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	answerGitHub(t, fake, nil)
	pass, reads := leasedReconciles(t, ctx, pool, NewReconcile(pool, fake.newTestClient(), events.NewBroker()))

	pass()
	if got := reads.take(); len(got) != 0 {
		t.Fatalf("a pass with no delivery settings and no head to read recorded %+v, want nothing", got)
	}
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Fatalf("the pass with nothing to read minted %d installation tokens, want none", mints)
	}

	seedDeliverySettings(t, ctx, pool)
	fake.answerInstallation("acme/widgets", http.StatusServiceUnavailable)
	pass()
	installation := "/repos/acme/widgets/installation"
	got := reads.take()
	if len(got) != 1 || got[0].read != githubreads.DeliveryReconcile || got[0].succeeded ||
		!strings.Contains(got[0].cause, installation) || !strings.Contains(got[0].cause, "status 503") {
		t.Fatalf("the pass GitHub answered 503 recorded %+v, want one failed %s attempt whose cause is that 503", got, githubreads.DeliveryReconcile)
	}

	fake.answerInstallation("acme/widgets", 0)
	pass()
	if got := reads.take(); len(got) != 1 || got[0].read != githubreads.DeliveryReconcile || !got[0].succeeded || got[0].cause != "" {
		t.Fatalf("the pass GitHub answered recorded %+v, want one succeeded %s attempt with no cause", got, githubreads.DeliveryReconcile)
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastError != nil || settings.LastReconcileAt == nil {
		t.Fatalf("the settings after the pass GitHub answered = %+v, %v; want a successful pass", settings, err)
	}
}

// A leased reconcile pass records failed when it gives up part of its read of GitHub on an answer
// the client leaves to it and notes nothing of -- a 404, a 422, a 403 that carries no limit, a
// permission the installation lacks, a GraphQL answer that carries errors -- so in those rows
// only the pass's own note can make the record say so, and its cause names what the pass gave
// up. Two rows are failures the client notes in a held scope (githubreads.Hold) that the page
// read keeps: a search a recorded GraphQL limit holds, and a page GitHub answers 502 on every
// try; in each the cause is the client's, which reaches the attempt before the pass's own note.
// A page GitHub answers 502 once and then reads is a page read, whose dropped note is no failure,
// and a pull request of a deleted repository is an answer the pass takes as data, stamping its
// refusal (facts_refused_at): each records succeeded. The rows of a head or of a stored pull
// request send their 422 to a read of the head pass. That read is the batched query of facts
// (readFactsOf), or the read of the commit messages after it (factsRead.answer).
func TestALeasedReconcilePassRecordsAReadOfGitHubItGaveUpAsFailed(t *testing.T) {
	fastPageRetries(t)
	answer := func(status int, message string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":%q}`, message)
		}
	}
	for _, row := range []struct {
		name string
		// arrange seeds the database and sets the stand-in's answers before the pass, and answers
		// the routes it answers in place of GitHub having nothing new (answerGitHub).
		arrange func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc
		// graphQLLimited records a limit on the GraphQL budget of the fake's installation in the
		// limits the client obeys. The search queries with that installation's token.
		graphQLLimited bool
		succeeded      bool
		// causeHas is what the cause of a failed attempt must name, and causeStarts what it must
		// begin with, for a row whose first failure is the client's own note.
		causeHas    []string
		causeStarts string
		// after checks what the pass left behind.
		after func(t *testing.T, ctx context.Context, pool *store.Pool)
	}{{
		name: "a deploy repository the App is not installed on",
		arrange: func(_ *testing.T, _ context.Context, _ *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc {
			fake.answerInstallation("acme/widgets", http.StatusNotFound)
			return nil
		},
		causeHas: []string{"not installed", "acme/widgets"},
	}, {
		name: "an installation lacking Actions read",
		arrange: func(_ *testing.T, _ context.Context, _ *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc {
			fake.permissions = map[string]string{"contents": "read", "pull_requests": "read"}
			return nil
		},
		causeHas: []string{"lacks Actions: read", "acme/widgets"},
	}, {
		name: "an installation lacking Pull requests read",
		arrange: func(_ *testing.T, _ context.Context, _ *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc {
			fake.permissions = map[string]string{"contents": "read", "actions": "read"}
			return nil
		},
		causeHas: []string{"lacks Pull requests: read", "acme/widgets"},
	}, {
		name: "a deploy workflow path GitHub answers 404 for",
		arrange: func(*testing.T, context.Context, *store.Pool, *fakeGitHub) map[string]http.HandlerFunc {
			return map[string]http.HandlerFunc{deployRunsRoute: answer(http.StatusNotFound, "Not Found")}
		},
		causeHas: []string{"reconcile deploy workflow runs", "status 404"},
	}, {
		name: "a merged pull request search whose GraphQL answer carries errors",
		arrange: func(t *testing.T, _ context.Context, _ *store.Pool, _ *fakeGitHub) map[string]http.HandlerFunc {
			return map[string]http.HandlerFunc{"POST /graphql": func(w http.ResponseWriter, _ *http.Request) {
				mustEncode(t, w, map[string]any{"data": nil, "errors": []any{
					map[string]any{"message": "Something went wrong while executing your query."},
				}})
			}}
		},
		causeHas: []string{"reconcile merged pull requests", "Something went wrong"},
	}, {
		name:           "a merged pull request search a recorded GraphQL limit holds",
		graphQLLimited: true,
		causeStarts:    "GitHub rate limit on " + githubapp.GraphQLInstallationScope(githubapp.InstallationScope(1)),
	}, {
		name: "a partial pull request GitHub answers 403 with no limit",
		arrange: func(t *testing.T, ctx context.Context, pool *store.Pool, _ *fakeGitHub) map[string]http.HandlerFunc {
			merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
			if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
				Repo: "acme/widgets", Number: 7, Title: "feat: refused", URL: "https://github.com/acme/widgets/pull/7",
				Author: "octocat", CreatedAt: &merged, Partial: true,
			}); err != nil {
				t.Fatalf("seed the partial pull request: %v", err)
			}
			return map[string]http.HandlerFunc{
				"GET /repos/acme/widgets/pulls/7": answer(http.StatusForbidden, "Resource not accessible by integration"),
			}
		},
		causeHas: []string{"complete partial pull request acme/widgets #7"},
	}, {
		name: "a merged partial pull request whose commit messages GitHub answers 422",
		arrange: func(t *testing.T, ctx context.Context, pool *store.Pool, _ *fakeGitHub) map[string]http.HandlerFunc {
			merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
			if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
				Repo: "acme/widgets", Number: 11, Title: "feat: commits refused", URL: "https://github.com/acme/widgets/pull/11",
				Author: "octocat", CreatedAt: &merged, Partial: true,
			}); err != nil {
				t.Fatalf("seed the partial pull request: %v", err)
			}
			return map[string]http.HandlerFunc{
				"GET /repos/acme/widgets/pulls/11": func(w http.ResponseWriter, _ *http.Request) {
					mustEncode(t, w, map[string]any{
						"number": 11, "title": "feat: commits refused", "html_url": "https://github.com/acme/widgets/pull/11",
						"user": map[string]any{"login": "octocat"}, "labels": []any{},
						"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
						"merge_commit_sha": "abc", "additions": 1, "deletions": 1, "body": "",
						// The head is this repository's own, so each read of the pull request reads
						// its commit messages.
						"head": map[string]any{
							"ref": "feature", "sha": "1111111111111111111111111111111111111111",
							"repo": map[string]any{"full_name": "acme/widgets"},
						},
					})
				},
				// The completion's read of the first commit answers. Each read of the commit
				// messages is refused, and the head pass reads them first.
				"GET /repos/acme/widgets/pulls/11/commits": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("per_page") == "1" {
						mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "x", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}}})
						return
					}
					answer(http.StatusUnprocessableEntity, "Validation Failed")(w, r)
				},
			}
		},
		causeHas: []string{"read the commit messages of acme/widgets PR #11", "422"},
	}, {
		name: "a pull request head whose re-read GitHub answers 422",
		arrange: func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc {
			seedUnreadHead(t, ctx, pool, 8)
			// The head pass reads an open head again by the batched query of facts.
			fake.facts = answer(http.StatusUnprocessableEntity, "Validation Failed")
			return nil
		},
		causeHas: []string{"read the facts of 1 pull requests", "the first acme/widgets PR #8", "status 422"},
	}, {
		name: "a pull request head whose commit messages GitHub answers 422",
		arrange: func(t *testing.T, ctx context.Context, pool *store.Pool, _ *fakeGitHub) map[string]http.HandlerFunc {
			seedUnreadHead(t, ctx, pool, 9)
			return map[string]http.HandlerFunc{
				// Neither the title nor the branch names an issue, and the head is this
				// repository's own, so the attribution reads the commit messages.
				"GET /repos/acme/widgets/pulls/9": func(w http.ResponseWriter, _ *http.Request) {
					mustEncode(t, w, map[string]any{
						"number": 9, "title": "Tidy the walk", "body": "", "html_url": "https://github.com/acme/widgets/pull/9",
						"state": "open", "merged": false, "updated_at": "2026-02-06T00:00:00Z",
						"head": map[string]any{
							"ref": "topic", "sha": "9999999999999999999999999999999999999999",
							"repo": map[string]any{"full_name": "acme/widgets"},
						},
						"base": map[string]any{"ref": "main"},
					})
				},
				"GET /repos/acme/widgets/pulls/9/commits": answer(http.StatusUnprocessableEntity, "Validation Failed"),
			}
		},
		causeHas: []string{"read the commit messages of acme/widgets PR #9", "422"},
	}, {
		name: "a stored pull request whose attribution read GitHub answers 422",
		arrange: func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc {
			merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
			// Complete, and stored before its attribution inputs were read.
			if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
				Repo: "acme/widgets", Number: 10, Title: "feat: unread", URL: "https://github.com/acme/widgets/pull/10",
				Author: "octocat", CreatedAt: &merged, MergedAt: &merged,
			}); err != nil {
				t.Fatalf("seed the stored pull request: %v", err)
			}
			// The head pass reads the facts of a stored pull request with no results row by the
			// batched query of facts.
			fake.facts = answer(http.StatusUnprocessableEntity, "Validation Failed")
			return nil
		},
		causeHas: []string{"read the facts of 1 pull requests", "the first acme/widgets PR #10", "status 422"},
	}, {
		name: "a page GitHub answers 502 on every try",
		arrange: func(*testing.T, context.Context, *store.Pool, *fakeGitHub) map[string]http.HandlerFunc {
			return map[string]http.HandlerFunc{deployRunsRoute: answer(http.StatusBadGateway, "Server Error")}
		},
		causeStarts: "GET ",
		causeHas:    []string{"deploy.yml", "status 502"},
	}, {
		name: "a page GitHub answers 502 once and then reads",
		arrange: func(t *testing.T, _ context.Context, _ *store.Pool, _ *fakeGitHub) map[string]http.HandlerFunc {
			// The merged-PR search's first query and the deploy runs listing's first page, one
			// page of each reader (queryGitHubPage and readGitHubPage).
			var queries, listings atomic.Int32
			return map[string]http.HandlerFunc{
				"POST /graphql": func(w http.ResponseWriter, _ *http.Request) {
					if queries.Add(1) == 1 {
						answer(http.StatusBadGateway, "Server Error")(w, nil)
						return
					}
					mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
				},
				deployRunsRoute: func(w http.ResponseWriter, _ *http.Request) {
					if listings.Add(1) == 1 {
						answer(http.StatusBadGateway, "Server Error")(w, nil)
						return
					}
					mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
				},
			}
		},
		succeeded: true,
		after: func(t *testing.T, ctx context.Context, pool *store.Pool) {
			if settings, err := GetSettings(ctx, pool); err != nil || settings.LastError != nil || settings.LastReconcileAt == nil {
				t.Fatalf("the settings after the pass = %+v, %v; want a successful pass: each page was read on its second try", settings, err)
			}
		},
	}, {
		name: "a pass whose only 404 is on a pull request of a deleted repository",
		arrange: func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub) map[string]http.HandlerFunc {
			merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
			if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
				Repo: "acme/gone", Number: 13, Title: "feat: of a deleted repository", URL: "https://github.com/acme/gone/pull/13",
				Author: "octocat", CreatedAt: &merged, Partial: true,
			}); err != nil {
				t.Fatalf("seed the partial pull request: %v", err)
			}
			// GitHub answers the installation lookup of a deleted repository 404.
			fake.answerInstallation("acme/gone", http.StatusNotFound)
			return nil
		},
		succeeded: true,
		after: func(t *testing.T, ctx context.Context, pool *store.Pool) {
			// GitHub proves no absence of a pull request in a repository it answers 404 for. So the
			// pass stamps the refusal and marks nothing unfetchable.
			if refused, unfetchable := timelineStampsOf(t, ctx, pool, "acme/gone", 13); !refused || unfetchable {
				t.Fatalf("the deleted repository's pull request holds facts_refused_at %v and unfetchable_at %v, want facts_refused_at only", refused, unfetchable)
			}
		},
	}} {
		t.Run(row.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			fake := newFakeGitHub(t)
			var handlers map[string]http.HandlerFunc
			if row.arrange != nil {
				handlers = row.arrange(t, ctx, pool, fake)
			}
			answerGitHub(t, fake, handlers)
			var options []githubapp.Option
			if row.graphQLLimited {
				limits := ratelimit.New(pool)
				if err := limits.Record(ctx, githubapp.GraphQLInstallationScope(githubapp.InstallationScope(fake.installID)), time.Hour); err != nil {
					t.Fatalf("record a limit on GitHub's GraphQL budget: %v", err)
				}
				options = append(options, githubapp.WithLimits(limits))
			}
			pass, reads := leasedReconciles(t, ctx, pool, NewReconcile(pool, fake.newTestClient(options...), events.NewBroker()))

			pass()

			got := reads.take()
			if len(got) != 1 || got[0].read != githubreads.DeliveryReconcile {
				t.Fatalf("the pass recorded %+v, want one %s attempt", got, githubreads.DeliveryReconcile)
			}
			if row.after != nil {
				row.after(t, ctx, pool)
			}
			if row.succeeded {
				if !got[0].succeeded || got[0].cause != "" {
					t.Fatalf("the pass recorded %+v, want succeeded with no cause", got[0])
				}
				return
			}
			if got[0].succeeded {
				t.Fatal("the pass recorded succeeded, want failed")
			}
			if !strings.HasPrefix(got[0].cause, row.causeStarts) {
				t.Fatalf("the recorded cause %q does not begin with %q", got[0].cause, row.causeStarts)
			}
			for _, text := range row.causeHas {
				if !strings.Contains(got[0].cause, text) {
					t.Fatalf("the recorded cause %q does not name %q", got[0].cause, text)
				}
			}
		})
	}
}

// seedUnreadHead stores the head of acme/widgets' open pull request number, which no read of
// GitHub has reached, so the next pass re-reads it first (ListStalePullRequestHeads).
func seedUnreadHead(t *testing.T, ctx context.Context, pool *store.Pool, number int) {
	t.Helper()
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: "cccccccccccccccccccccccccccccccccccccccc",
		HeadRef: "topic", BaseRef: "main", State: "open",
	}); err != nil {
		t.Fatalf("store the head of #%d: %v", number, err)
	}
}
