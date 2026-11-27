package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// What the reconcile stand-in below serves: the project's default repository, lower-cased as
// it is stored, the name and id GitHub itself answers with, and the one open pull request's head
// the pass reads the checks of.
const (
	ciReconcileRow        = "acme/widgets"
	ciReconcileOwner      = "Acme"
	ciReconcileName       = "Widgets"
	ciReconcileRepository = ciReconcileOwner + "/" + ciReconcileName
	ciReconcileHeadSHA    = "abcdef1234567890abcdef1234567890abcdef12"
	// ciReconcileReviewerApp is DISPATCH_REVIEWER_APP_ID for the process, the App that created
	// the `tester` check run below; a pass that never read the variable would drop that run.
	ciReconcileReviewerApp = "5341243"
)

// The CI reconcile pass is a loop of the real envoy-dispatch binary, so this proves it there:
// the actual process, against a real NATS whose envoy_ci_state bucket the test made as the
// listener makes it (the pass creates no bucket), a loopback GitHub serving the installation
// lookup, the token mint and the four reads one pass makes, and one project whose default
// repository is the repository it serves. The evidence is the process's own log and the bucket:
// the duty loop's leased pass under the ci-reconcile lease, a pass line that read the one head
// whole, and the one head's record, which an ordinary completed check run, the review App's
// `tester` verdict and the head's check suite all write. The record is read from the bucket, not
// from a pass line's records count: each pass is bound at the one-second interval
// (Reconciler.PassTimeout), a write that bound cut off can land all the same, and the pass that
// reads the head again then changes nothing (recordHead), so on a loaded machine no pass line
// need count the record.
func TestTheCIReconcilePassRunsInTheDispatchProcess(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedCIReconcileDefault(t, database)
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
			// Five minutes is longer than any test may wait for a pass.
			"DISPATCH_TEST_CI_RECONCILE_INTERVAL": "1s",
		},
	})
	process.waitHealthy(t)
	process.WaitForOutput(t,
		`msg="dispatch: reconciling the CI records GitHub's webhook never delivered" client_id=Iv1.cireconcile interval=1s`,
		`msg="dispatch duty: leased pass ran" lease=ci-reconcile`,
	)
	waitForCIReconcilePasses(t, process, "a pass that read the one head whole", func(passes []map[string]string) bool {
		for _, pass := range passes {
			if pass["list_pages"] == "1" && pass["heads"] == "1" && pass["head_requests"] == "2" && pass["heads_not_modified"] == "0" &&
				pass["defaults_skipped"] == "0" && pass["heads_too_large"] == "0" && pass["rate_limited"] == "false" {
				return true
			}
		}
		return false
	})
	awaitCIReconcileHeadRecord(t, natsURL)
}

// awaitCIReconcileHeadRecord waits until the CI bucket's record of the stand-in's one head holds
// all it serves: the completed run 11 of build, the review App's completed `tester` run 12, and
// the completed check suite 21, each a success. A pass whose bound cut off its write of the runs
// writes no suite, so the suite can land a pass later than the runs.
func awaitCIReconcileHeadRecord(t *testing.T, natsURL string) {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	bucket, err := js.KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	key := cistore.Key(ciReconcileOwner, ciReconcileName, "7", ciReconcileHeadSHA)
	testwait.Until(t, "the CI bucket's record of the one head holding build's run, the tester verdict and the check suite", func() (string, bool) {
		entry, err := bucket.Get(key)
		if err != nil {
			return fmt.Sprintf("no record of %s: %v", key, err), false
		}
		var state cistore.State
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			return fmt.Sprintf("the record of %s does not decode: %v", key, err), false
		}
		build, tester := state.Checks["build"], state.Checks["tester"]
		suite := false
		for _, s := range state.Suites {
			suite = suite || (s.ID == "21" && s.Status == "completed" && s.Conclusion == "success")
		}
		held := uint64(build.CheckRunID) == 11 && build.Status == "completed" && build.Conclusion == "success" &&
			uint64(tester.CheckRunID) == 12 && tester.Status == "completed" && tester.Conclusion == "success" && suite
		return fmt.Sprintf("checks %+v, suites %+v", state.Checks, state.Suites), held
	})
}

// seedCIReconcileDefault makes the stand-in's repository the default repository of a project
// of its own, as PUT /api/v1/projects/{key}/default-repository sets it: the pass reads GitHub for
// every project's default repository and the repositories issues name as their own, and its
// callers record no issue.
func seedCIReconcileDefault(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name, default_assignee, default_repo) values ('CIR', 'CI reconcile', 'alice', $1)
	`, ciReconcileRow); err != nil {
		t.Fatalf("make %s the default repository of a project: %v", ciReconcileRow, err)
	}
}

// makeCIStateBucket makes envoy_ci_state on the test's NATS, as the Envoy listener makes it. The
// pass binds that bucket through cistore.OpenWriter and creates none, so a process whose NATS
// has no bucket writes nothing at all.
func makeCIStateBucket(t *testing.T, natsURL string) {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	if _, err := cistore.Open(t.Context(), conn, logging.New("ci-reconcile-test"), cistore.WithTTL(time.Hour)); err != nil {
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
			{"number": 7, "state": "open", "head": map[string]any{"sha": ciReconcileHeadSHA}},
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
