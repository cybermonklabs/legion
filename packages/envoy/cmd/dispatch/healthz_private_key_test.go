package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// TestHealthzNamesNoPrivateProjectToACallerWithNoCredential (R2, R6; c-private-unseen):
// /healthz answers a caller with no credential (R6 keeps it open), so it names no private
// project's key, a project the deployment did not provision included: a person from outside, or
// any agent, reads the deployment's health and learns nothing of a project it cannot see. The
// operator's own reading of which project is unprovisioned is the ERROR line the boot and each
// maintenance pass log. Nor does it name a repository: a failed GitHub read's cause is GitHub's
// error text, which names the repository it read (the delivery reconcile reads each project's pull
// requests), a private project's or a repository GitHub keeps private, and whoever reaches the
// port reads /healthz, from outside too once the deployment has an outside origin. So in every
// deployment, with a private project or none, a failed read's cause is its class alone, the status
// GitHub answered ("status 502"), and its own text stays in github_read_attempts. Control: answer
// the recorded cause, and both rows fail naming secret-repo.
func TestHealthzNamesNoPrivateProjectToACallerWithNoCredential(t *testing.T) {
	for _, row := range []struct {
		name string
		seed string
	}{
		{"a private project", `insert into projects (key, name, default_assignee, visibility) values ('SECRET', 'Secret', 'alice@example.com', 'private')`},
		{"no private project", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			database := storetest.Open(t)
			if row.seed != "" {
				if _, err := database.Pool.Exec(context.Background(), row.seed); err != nil {
					t.Fatalf("seed %s: %v", row.name, err)
				}
			}
			state, err := startPrivateProjectsState(t.Context(), database.Pool, nil)
			if err != nil {
				t.Fatalf("the first read of the private projects: %v", err)
			}
			failedAt := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
			reads := &gitHubReads{
				recordCopy: loadedGitHubReadCopy(t, []githubreads.Recorded{{Read: githubreads.DeliveryReconcile, At: failedAt,
					Cause: "GET https://api.github.com/repos/acme/secret-repo/pulls: status 502: bad gateway"}}),
				runs: map[githubreads.Read]bool{githubreads.DeliveryReconcile: true},
			}
			response := httptest.NewRecorder()
			dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, "", nil, issuers{}, state, reads).
				ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			body := response.Body.String()
			if strings.Contains(body, "SECRET") {
				t.Fatalf("/healthz with no credential = %d %s: it names the private project SECRET", response.Code, body)
			}
			if strings.Contains(body, "secret-repo") {
				t.Fatalf("/healthz with no credential, %s = %d %s: it names secret-repo, the repository a failed GitHub read read", row.name, response.Code, body)
			}
			if !strings.Contains(body, `"state":"failed"`) || !strings.Contains(body, `"cause":"status 502"`) {
				t.Fatalf("/healthz with no credential, %s = %d %s: want the delivery reconcile failed, its cause the class \"status 502\"", row.name, response.Code, body)
			}
		})
	}
}
