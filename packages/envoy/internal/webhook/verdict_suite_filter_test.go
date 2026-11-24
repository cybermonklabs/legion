package webhook

import (
	"fmt"
	"slices"
	"testing"
)

// ENVOY_VERDICT_APP_ID leaves out of the CI record the check suites of that App alone, in any
// state: another App's suite in a completed delivery reaches the CI recorder whether it is queued
// or failed, and with the setting unset every suite does. Whether a queued suite holds its head is
// the CI store's rule (cistore's suiteHolds).
func TestTheVerdictAppSettingLeavesOutOnlyThatAppsSuites(t *testing.T) {
	suite := func(id, app int, status, conclusion string) []byte {
		return fmt.Appendf(nil, `{
			"action": "completed",
			"check_suite": {"id": %d, "status": %q, "conclusion": %q, "updated_at": "2026-11-20T03:00:00Z",
				"head_sha": "abcdef1234567890abcdef1234567890abcdef12", "app": {"id": %d}, "pull_requests": [{"number": 42}]},
			"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
		}`, id, status, conclusion, app)
	}
	deliveries := []struct {
		id, app            int
		status, conclusion string
	}{
		{800, 5341240, "queued", ""},
		{801, 5341240, "completed", "failure"},
		{810, 5341243, "queued", ""},
		{811, 5341243, "completed", "failure"},
	}
	recorded := func(verdictAppID string) []string {
		t.Helper()
		rec := &mockRecorder{}
		handler := GitHubHandler("s", "@legion", "", verdictAppID, &mockPublisher{}, rec, NewBodyBudget(DefaultBodyBudget, nil))
		for i, d := range deliveries {
			postDelivery(t, handler, "check_suite", fmt.Sprintf("d-suite-%d", i), suite(d.id, d.app, d.status, d.conclusion), nil)
		}
		var suites []string
		for _, o := range rec.suiteCalls {
			suites = append(suites, fmt.Sprintf("%s app=%s %s/%s", o.SuiteID, o.AppID, o.Status, o.Conclusion))
		}
		return suites
	}
	if got, want := recorded("5341240"), []string{"810 app=5341243 queued/", "811 app=5341243 completed/failure"}; !slices.Equal(got, want) {
		t.Errorf("with ENVOY_VERDICT_APP_ID=5341240 the CI record took %q, want %q: another App's queued and failed suites, none of the verdict App's", got, want)
	}
	if got := recorded(""); len(got) != len(deliveries) {
		t.Errorf("with ENVOY_VERDICT_APP_ID unset the CI record took %q, want all %d suites", got, len(deliveries))
	}
}
