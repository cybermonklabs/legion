package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

// A session that follows an issue in the form naming the issue's project
// (notifications.dispatch.issue.TEST.TEST-1.>) receives that issue's events: the listener filters
// its consumer on both forms of each topic (bus.SessionConsumerFilters), and its subscribe route
// answers every old-form registration with a warning telling the session to register this form.
// Dispatch's subscriber list is a reader of the same interest names, so it must show such a
// session as the issue's subscriber, and as one a person can remove (Removable: its topic is
// scoped to this issue alone), exactly as it shows a session following the old form.
//
// Seen live on the serve stack of scripts/e2e/envoy-session-delivery.sh at f565979e50: GET
// /api/v1/issues/LIVE-1/subscribers listed the old-form follower and not the new-form one, and
// DELETE /api/v1/issues/LIVE-1/subscribers/<new-form follower> answered 404 SUBSCRIBER_NOT_FOUND
// while that session still followed LIVE-1.
func TestASubscriberFollowingTheNewIssueFormIsListedAndRemovable(t *testing.T) {
	const (
		oldForm = "notifications.dispatch.issue.TEST-1.>"
		newForm = "notifications.dispatch.issue.TEST.TEST-1.>"
	)
	envoyURL, _ := newFakeListener(t, map[string][]string{
		"old-form": {oldForm},
		"new-form": {newForm},
		// Neither form of another issue, nor the new form under a project that is not the key's,
		// follows TEST-1: these stay off the list.
		"other-issue":   {"notifications.dispatch.issue.TEST.TEST-2.>"},
		"wrong-project": {"notifications.dispatch.issue.OTHER.TEST-1.>"},
	}, []map[string]any{
		{"session_id": "old-form", "last_seen": 1800000000000},
		{"session_id": "new-form", "last_seen": 1800000000001},
	})
	s := &server{deps: Deps{Envoy: envoy.New(envoyURL)}}

	response := httptest.NewRecorder()
	s.writeSubscribers(response, httptest.NewRequest(http.MethodGet, "/api/v1/issues/TEST-1/subscribers", nil), issueSubscriberBase("TEST-1"))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]subscriber](t, response)
	got := make(map[string]subscriber, len(rows))
	for _, row := range rows {
		got[row.SessionID] = row
	}
	for _, id := range []string{"old-form", "new-form"} {
		row, ok := got[id]
		if !ok {
			t.Errorf("subscriber %s is not listed for TEST-1; rows %+v", id, rows)
			continue
		}
		if !row.Removable {
			t.Errorf("subscriber %s is listed as not removable (via %q); rows %+v", id, row.Via, rows)
		}
	}
	for _, id := range []string{"other-issue", "wrong-project"} {
		if _, ok := got[id]; ok {
			t.Errorf("subscriber %s is listed for TEST-1, which it does not follow; rows %+v", id, rows)
		}
	}
}
