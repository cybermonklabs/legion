package webhook

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A check_suite delivery whose action is requested or rerequested is GitHub asking the suite's App
// to run its checks, never a CI result: the route calls no RecordSuite for it and publishes
// nothing, so the queued suite it carries never enters the record. A completed delivery of the same
// suite calls RecordSuite once.
func TestGitHubHandlerRecordsNoCheckSuiteRequest(t *testing.T) {
	for _, tc := range []struct {
		action, status, conclusion string
		calls                      int
	}{
		{"requested", "queued", "", 0},
		{"rerequested", "queued", "", 0},
		{"completed", "completed", "success", 1},
	} {
		t.Run(tc.action, func(t *testing.T) {
			body := fmt.Sprintf(`{
				"action": %q,
				"check_suite": {"id": 9200, "status": %q, "conclusion": %q, "updated_at": "2026-11-22T03:00:00Z",
					"latest_check_runs_count": 0, "head_sha": "abcdef1234567890abcdef1234567890abcdef12",
					"app": {"id": 5789269}, "pull_requests": [{"number": 42}]},
				"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
			}`, tc.action, tc.status, tc.conclusion)
			pub := &mockPublisher{}
			rec := &mockRecorder{}
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
			req.Header.Set("X-GitHub-Delivery", "d-suite-"+tc.action)
			req.Header.Set("X-GitHub-Event", "check_suite")
			req.Header.Set("X-Hub-Signature-256", githubSign("s", []byte(body)))
			rr := httptest.NewRecorder()
			GitHubHandler("s", "@legion", "", "", pub, rec, NewBodyBudget(DefaultBodyBudget, nil)).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
			}
			if len(rec.suiteCalls) != tc.calls || len(rec.calls) != 0 {
				t.Fatalf("a %s delivery made %d RecordSuite calls (%+v) and %d Record calls, want %d and none", tc.action, len(rec.suiteCalls), rec.suiteCalls, len(rec.calls), tc.calls)
			}
			if len(pub.published) != 0 {
				t.Fatalf("a %s delivery published %+v, want nothing", tc.action, pub.published)
			}
		})
	}
}
