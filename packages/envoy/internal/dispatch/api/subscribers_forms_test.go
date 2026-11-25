package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	t.Parallel()
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
	s.writeSubscribers(response, httptest.NewRequest(http.MethodGet, "/api/v1/issues/TEST-1/subscribers", nil), everyReader, issueSubscriberBase("TEST-1"), false)
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

// A topic scoped to one issue names its key in either form, so to a caller that does not see the
// issue's project it is a topic of a key no issue holds: the old form
// (notifications.dispatch.issue.PRIV-1.>) and the form that names the project too
// (notifications.dispatch.issue.PRIV.PRIV-1.>) alike. GET /api/v1/issues/PRIV-1/subscribers then
// answers alice, an open viewer who does not see PRIV, the bytes GET
// /api/v1/issues/NOPE-1/subscribers answers her: only the subscriber of a broader wildcard, which
// delivers both keys' events. carol, who sees PRIV, reads all three subscribers of PRIV-1.
func TestAHiddenIssuesSubscriberInEitherFormAnswersAsAKeyNobodyHolds(t *testing.T) {
	envoyURL, _ := newFakeListener(t, map[string][]string{
		"old-form": {"notifications.dispatch.issue.PRIV-1.>"},
		"new-form": {"notifications.dispatch.issue.PRIV.PRIV-1.>"},
		"wide":     {"notifications.dispatch.>"},
	}, []map[string]any{
		{"session_id": "old-form", "title": "Old form", "projects": []string{}, "last_seen": 1800000000002},
		{"session_id": "new-form", "title": "New form", "projects": []string{}, "last_seen": 1800000000001},
		{"session_id": "wide", "title": "Wide", "projects": []string{}, "last_seen": 1800000000000},
	})
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: envoyURL})
	if key := seed.issue[seedPrivate]; key != "PRIV-1" {
		t.Fatalf("PRIV's issue is %s; the listener's topics name PRIV-1", key)
	}
	// Dispatch holds the listener's record of each session, none naming a project, as it does once
	// a list of the sessions has read them: while a project is private an open viewer reaches a
	// session that has acted in no project only through that record (access.SessionClause), so
	// without it alice would see no subscriber of either key.
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	sessions := func(response *httptest.ResponseRecorder) []string {
		t.Helper()
		ids := []string{}
		for _, row := range decodeBody[[]subscriber](t, response) {
			ids = append(ids, row.SessionID)
		}
		return ids
	}
	hidden := seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/PRIV-1/subscribers", nil, http.StatusOK)
	missing := seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/NOPE-1/subscribers", nil, http.StatusOK)
	if hidden.Body.String() != missing.Body.String() {
		t.Errorf("alice reads PRIV-1's subscribers as %s, and NOPE-1's, a key nobody holds, as %s",
			strings.TrimSpace(hidden.Body.String()), strings.TrimSpace(missing.Body.String()))
	}
	if got := sessions(missing); len(got) != 1 || got[0] != "wide" {
		t.Errorf("alice reads NOPE-1's subscribers as %v, want the broader wildcard's alone, [wide]", got)
	}
	got := sessions(seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/issues/PRIV-1/subscribers", nil, http.StatusOK))
	if want := []string{"old-form", "new-form", "wide"}; !slices.Equal(got, want) {
		t.Errorf("carol reads PRIV-1's subscribers as %v, want %v", got, want)
	}
}
