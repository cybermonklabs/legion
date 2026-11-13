package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// What the reconcile stand-in below serves: the row repo_projects holds, lower-cased as every
// mapping is stored, the name and id GitHub itself answers with, and the one open pull request's
// head the pass reads the checks of.
const (
	ciReconcileRow         = "acme/widgets"
	ciReconcileOwner       = "Acme"
	ciReconcileName        = "Widgets"
	ciReconcileRepository  = ciReconcileOwner + "/" + ciReconcileName
	ciReconcilePullRequest = 7
	ciReconcileHeadSHA     = "abcdef1234567890abcdef1234567890abcdef12"
	// ciReconcileReviewerApp is DISPATCH_REVIEWER_APP_ID for the process, the App that created
	// the `tester` check run below; a pass that never read the variable would drop that run.
	ciReconcileReviewerApp = "5341243"
)

// ciReconcilePassInterval is the loop's period for the process of this row and of the cursor
// row beside it (ci_reconcile_cursor_test.go) and, with it, the bound every one of their passes
// runs under: the two are one setting (cmd/dispatch/cireconcile.go, ciReconciler), and a pass
// that outlives the bound ends where it stands, having read no head whole (cireconcile/pass.go,
// Pass and walk.visitHeadOverREST).
//
// The bound has to cover the gap the pass leaves between its REST reads, because that gap is
// cut from the bound itself: nine tenths of it divided by the REST work the pass before it
// learned there is, floored at cireconcile.FastestRequestPace and capped at the
// cireconcile.RequestPace the process configures (cireconcile/pacing.go, gap). This fixture's
// pass sends four REST reads -- the repository's identity, its open pull requests and the one
// head's two listings -- and the pass before it leaves two pages of work behind, so under an
// interval of 1.67s the cap never binds and the three gaps a pass waits are nine tenths of its
// own bound whatever that bound is. At the one second this test used to run the loop on, every
// pass after the first measured 925ms of its 1s bound with 900ms of it spent waiting: 7% of
// the bound left for the token mint, the shared rate-limit reads, the round trips and the KV
// writes, which a loaded machine ran past, and a pass cut short there leaves the one after it
// the same work and the same 7% (INFRA-1075).
//
// Five seconds is past that knee: the gaps stop at the 1.5s the cap allows and the work of a
// pass, 30ms here and 120ms under eight busy loops on two cores, has the other 3.5s. The cursor
// row's order is sized against this same bound, by the reads a pass of it may send
// (ciReconcileCursorPassReads), so the two rows move together.
const ciReconcilePassInterval = 5 * time.Second

// The CI reconcile pass is a loop of the real envoy-dispatch binary, so this proves it there:
// the actual process, against a real NATS whose envoy_ci_state bucket the test made as the
// listener makes it (the pass creates no bucket), a loopback GitHub serving the installation
// lookup, the token mint and the four reads one pass makes, and one repo_projects row for the
// repository it serves. The process's own log is the evidence: the duty loop's leased pass under
// the ci-reconcile lease, and the pass's own INFO line with the one record it changed -- the one
// head's record, which an ordinary completed check run, the review App's `tester` verdict and
// the head's check suite all write.
//
// The line that is read is the pass that names this fixture's own head, and never whichever
// pass the process logged first: a pass may run before NATS is connected, and one its bound cut
// short reads part of the head and says so in the very same fields, so a row matched by its
// numbers alone is a row that can read an earlier pass's work as this one's.
func TestTheCIReconcilePassRunsInTheDispatchProcess(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	mapCIReconcileRepository(t, database)
	_, natsURL := testnats.Start(t)
	makeCIStateBucket(t, natsURL)
	github := startCIReconcileGitHub(t)
	_, pemText := githubapptest.Key(t)

	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL,
		Env: map[string]string{
			"DISPATCH_APP_CLIENT_ID":   "Iv1.cireconcile",
			"DISPATCH_APP_PEM_B64":     pemText,
			"DISPATCH_GITHUB_API_BASE": github.URL,
			"DISPATCH_REVIEWER_APP_ID": ciReconcileReviewerApp,
			// The production five minutes is longer than any test may wait for a pass.
			"DISPATCH_TEST_CI_RECONCILE_INTERVAL": ciReconcilePassInterval.String(),
		},
	})
	process.waitHealthy(t)
	process.WaitForOutput(t,
		`msg="dispatch: reconciling the CI records GitHub's webhook never delivered" client_id=Iv1.cireconcile interval=`+
			ciReconcilePassInterval.String())

	// The pass this test is about: the one that read the fixture's one head, which it names as
	// both the first and the last head it reached.
	head := ciReconcileHeadName(ciReconcilePullRequest)
	passes := waitForCIReconcilePasses(t, process, "a pass that read "+head,
		func(passes []map[string]string) bool { return ciReconcilePassOfHead(passes, head) >= 0 })
	at := ciReconcilePassOfHead(passes, head)
	pass := passes[at]
	for _, want := range []struct{ field, value string }{
		// One page of open pull requests, the one head read whole over REST with both its
		// listings, nothing answered 304, no mapping passed over, no answer too large to read,
		// and no rate limit in the way.
		{"list_pages", "1"},
		{"heads", "1"},
		{"head_requests", "2"},
		{"heads_not_modified", "0"},
		{"mappings_skipped", "0"},
		{"heads_too_large", "0"},
		{"rate_limited", "false"},
	} {
		if pass[want.field] != want.value {
			t.Errorf("the pass that read %s reports %s = %q, want %q: %v", head, want.field, pass[want.field], want.value, pass)
		}
	}
	// That head's record is changed once in the life of this process: the pass that first reads
	// the head whole writes the run, the verdict and the suite into it, and every pass after
	// that reads the same two listings -- the stand-in answers no ETag, so nothing is
	// conditional -- and writes observations the store applies as no change. The passes up to
	// and including the one that named the head are counted together rather than that pass
	// alone because a pass whose bound fell between recording the head and counting it as read
	// changed the record and named no head (cireconcile/pass.go, walk.visitHeadOverREST).
	if changed := ciReconcileRecords(t, passes[:at+1]); changed != 1 {
		t.Errorf("the passes up to the one that read %s changed %d records, want 1, the one record that head has:\n%s",
			head, changed, process.Output.String())
	}
	process.WaitForOutput(t, `msg="dispatch duty: leased pass ran" lease=ci-reconcile`)
}

// ciReconcilePassOfHead is the index of the first pass that read exactly the one head named,
// which is the pass that names it as both the first and the last head it reached, and -1 while
// no pass has.
func ciReconcilePassOfHead(passes []map[string]string, head string) int {
	for i, pass := range passes {
		if pass["first_head"] == head && pass["last_head"] == head {
			return i
		}
	}
	return -1
}

// ciReconcileRecords is the CI records these passes changed between them.
func ciReconcileRecords(t *testing.T, passes []map[string]string) int {
	t.Helper()
	records := 0
	for _, pass := range passes {
		count, err := strconv.Atoi(pass["records"])
		if err != nil {
			t.Fatalf("a pass line reports records = %q, which is no count: %v", pass["records"], pass)
		}
		records += count
	}
	return records
}

// mapCIReconcileRepository maps the stand-in's repository to a project of its own, as Settings →
// Repositories does: the pass reads GitHub for the repositories repo_projects names and no other.
func mapCIReconcileRepository(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name, default_assignee) values ('CIR', 'CI reconcile', 'alice') on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := database.Pool.Exec(t.Context(), `
		insert into repo_projects (repo, project, created_by)
		values ($1, 'CIR', '{"kind":"system","id":"test"}'::jsonb)
	`, ciReconcileRow); err != nil {
		t.Fatalf("map %s to a project: %v", ciReconcileRow, err)
	}
}

// makeCIStateBucket makes envoy_ci_state on the test's NATS, as the Envoy listener makes it. The
// pass binds that bucket through cistore.OpenWriter and creates none, so a process whose NATS
// has no bucket writes nothing at all.
func makeCIStateBucket(t *testing.T, natsURL string) {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	if _, err := cistore.Open(conn, logging.New("ci-reconcile-test"), cistore.WithTTL(time.Hour)); err != nil {
		t.Fatalf("make the CI state bucket: %v", err)
	}
}

// startCIReconcileGitHub is the loopback GitHub the process's App calls go to, so none leaves
// this machine: the installation lookup and the token mint every installation token needs, and
// the four reads one pass makes of one repository. The webhook redelivery sweep's own listing
// answers empty, and every other path answers GitHub's 404, as GitHub answers a path the App
// cannot see.
func startCIReconcileGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	installation := map[string]any{
		"id":          1,
		"app_slug":    "cireconcile",
		"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"},
	}
	write := func(w http.ResponseWriter, status int, payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("encode the stand-in's answer: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, installation)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusCreated, map[string]any{
			"token":      "ghs_cireconcile",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		// The name GitHub itself gives the repository, which every record is keyed by, rather
		// than the lower-cased row the pass read it from.
		write(w, http.StatusOK, map[string]any{"full_name": ciReconcileRepository, "id": 4242})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, []map[string]any{
			{"number": ciReconcilePullRequest, "state": "open", "head": map[string]any{"sha": ciReconcileHeadSHA}},
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{
				"id": 11, "name": "build", "head_sha": ciReconcileHeadSHA,
				"status": "completed", "conclusion": "success",
				"completed_at": "2026-01-02T03:04:05Z",
				"app":          map[string]any{"id": 777},
			},
			{
				"id": 12, "name": "tester", "head_sha": ciReconcileHeadSHA,
				"status": "completed", "conclusion": "success",
				"completed_at": "2026-01-02T03:04:06Z",
				"app":          map[string]any{"id": ciReconcileReviewerApp},
			},
		}})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-suites", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"total_count": 1, "check_suites": []map[string]any{
			{
				"id": 21, "head_sha": ciReconcileHeadSHA,
				"status": "completed", "conclusion": "success",
				"updated_at": "2026-01-02T03:04:07Z",
			},
		}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/hook/deliveries" {
			write(w, http.StatusOK, []any{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	return githubtest.Serve(t, githubtest.Spec{Handle: mux.ServeHTTP})
}
