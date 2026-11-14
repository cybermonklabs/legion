package api

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedActivityFixture seeds project DEMO's two issues and OTHER's one, with events this week, a
// week ago and two weeks ago, and one sixty days ago that no default span reaches. The events are
// placed by the database's clock, whole weeks apart, so each lands in the ISO week it names
// whatever day the test runs on.
func seedActivityFixture(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('DEMO', 'Demo', 'alice'), ('OTHER', 'Other', 'alice')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank) values
			('DEMO-1', 'DEMO', 1, 'Ship the read', 'todo', '{"kind":"system","id":"test"}', 'U'),
			('DEMO-2', 'DEMO', 2, 'Draw the chart', 'done', '{"kind":"system","id":"test"}', 'V'),
			('OTHER-1', 'OTHER', 1, 'Elsewhere', 'todo', '{"kind":"system","id":"test"}', 'U')`,
		`insert into events (issue_key, seq, type, actor, payload, notify, created_at) values
			('DEMO-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false, now() - interval '14 days'),
			('DEMO-1', 2, 'comment.created', '{"kind":"session","id":"ses-a"}', '{}', false, now() - interval '7 days'),
			('DEMO-2', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false, now() - interval '7 days'),
			('DEMO-2', 2, 'comment.created', '{"kind":"session","id":"ses-a"}', '{}', false, now()),
			('DEMO-2', 3, 'comment.created', '{"kind":"session","id":"ses-b"}', '{}', false, now()),
			('DEMO-1', 3, 'issue.updated', '{"kind":"user","id":"alice"}', '{}', false, now() - interval '60 days'),
			('OTHER-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false, now())`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed activity: %v\n%s", err, statement)
		}
	}
}

// The activity of a project is its own issues' events counted by kind per ISO week, every week of
// the span listed oldest first with a quiet week as zero, and nothing of another project's issues
// or of a week before the span.
func TestProjectActivityCountsTheProjectsEventsByKindPerWeek(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedActivityFixture(t, database)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/DEMO/activity?weeks=4", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	activity := decodeBody[projectActivity](t, response)
	if activity.Project != "DEMO" || len(activity.Weeks) != 4 {
		t.Fatalf("activity = %+v, want DEMO over four weeks", activity)
	}
	type week struct {
		events int
		kinds  map[string]int
	}
	got := make([]week, 0, 4)
	for index, each := range activity.Weeks {
		got = append(got, week{each.Events, each.Kinds})
		if !strings.Contains(each.Week, "-W") || each.Start.Weekday() != 1 || each.Start.Hour() != 0 {
			t.Errorf("week %d is %q starting %s, want an ISO week starting Monday 00:00 UTC", index, each.Week, each.Start)
		}
		if index > 0 && !each.Start.After(activity.Weeks[index-1].Start) {
			t.Errorf("week %d starts %s, not after the week before it: want oldest first", index, each.Start)
		}
	}
	want := []week{
		{0, map[string]int{}},
		{1, map[string]int{"issue.created": 1}},
		{2, map[string]int{"comment.created": 1, "issue.created": 1}},
		{2, map[string]int{"comment.created": 2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("weeks = %+v, want %+v", got, want)
	}

	defaulted := decodeBody[projectActivity](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/DEMO/activity", nil, "alice"))
	if len(defaulted.Weeks) != projectActivityWeeksDefault {
		t.Errorf("no weeks named answered %d weeks, want %d", len(defaulted.Weeks), projectActivityWeeksDefault)
	}
	total := 0
	for _, each := range defaulted.Weeks {
		total += each.Events
	}
	if total != 5 {
		t.Errorf("the default span counts %d events, want 5: the sixty-day-old event is outside it", total)
	}
}

// A weeks outside 1 to 52 is 400 INVALID_QUERY, a key that is no project key 400 INVALID_PROJECT,
// and a project Dispatch does not hold 404 PROJECT_NOT_FOUND.
func TestProjectActivityRefusesABadSpanKeyOrProject(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedActivityFixture(t, database)
	for target, code := range map[string]string{
		"/api/v1/projects/DEMO/activity?weeks=0":    "INVALID_QUERY",
		"/api/v1/projects/DEMO/activity?weeks=53":   "INVALID_QUERY",
		"/api/v1/projects/DEMO/activity?weeks=many": "INVALID_QUERY",
		"/api/v1/projects/demo/activity":            "INVALID_PROJECT",
		"/api/v1/projects/NOPE/activity":            "PROJECT_NOT_FOUND",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		want := http.StatusBadRequest
		if code == "PROJECT_NOT_FOUND" {
			want = http.StatusNotFound
		}
		if response.Code != want || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("GET %s: status=%d body=%s, want %d %s", target, response.Code, response.Body.String(), want, code)
		}
	}
}
