package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// wholeAccessPayload is a project.access_changed payload in the shape the release of migration
// 20261116015736_project_access stored: the project's whole access, its emails included, the
// change and the principal it named.
func wholeAccessPayload(project, change, principal string) map[string]any {
	return map[string]any{
		"project": project, "owner": "olga@example.com", "visibility": "public",
		"people": []string{"carol@example.com"}, "groups": []string{}, "slack_channels": []string{}, "not_signed_in": []string{},
		"change": change, "principal": principal,
	}
}

// storedPayload is event id's payload as stored, compacted.
func storedPayload(t *testing.T, database *store.Store, id int64) string {
	t.Helper()
	var stored []byte
	if err := database.Pool.QueryRow(context.Background(), `select payload::text from events where id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read event %d's payload: %v", id, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, stored); err != nil {
		t.Fatalf("compact %s: %v", stored, err)
	}
	return compact.String()
}

// TestTheAccessEventsScrubRewritesWholeAccessPayloads: envoy-dispatch scrub-access-events rewrites
// every project.access_changed event stored in the shape the release of migration
// 20261116015736_project_access stored, with the whole ProjectAccess and carol's email, to
// {project, change}, reading events in id order a batch at a time, and then writes the
// access_events_scrubbed marker. What the payload named moves into the server's own record of the
// event (project_access_changes), which a live resume reads and no route serves. An event of
// another type is left as it was. Afterwards no stored access event holds carol's email, which is
// what GET /api/v1/events/log serves, and a second run rewrites nothing. A run in batches of one
// crosses every batch boundary.
func TestTheAccessEventsScrubRewritesWholeAccessPayloads(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email) values ('olga@example.com'), ('carol@example.com');
		insert into projects (key, name, default_assignee) values ('PUB', 'Public', 'olga@example.com');
	`); err != nil {
		t.Fatalf("seed people and a project: %v", err)
	}
	olga := model.Actor{Kind: "user", ID: "olga@example.com"}
	appendEach(t, database,
		model.Event{ProjectKey: new("PUB"), Type: "project.access_changed", Actor: olga, Payload: wholeAccessPayload("PUB", "person_added", "carol@example.com")},
		model.Event{ProjectKey: new("PUB"), Type: "project.updated", Actor: olga, Payload: map[string]any{"note": "carol@example.com"}},
	)
	var access, other int64
	if err := database.Pool.QueryRow(ctx, `
		select (select id from events where type = 'project.access_changed'), (select id from events where type = 'project.updated')
	`).Scan(&access, &other); err != nil {
		t.Fatalf("read the events' ids: %v", err)
	}
	otherBefore := storedPayload(t, database, other)
	markers := func() int64 {
		t.Helper()
		var count int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from access_events_scrubbed`).Scan(&count); err != nil {
			t.Fatalf("count access_events_scrubbed: %v", err)
		}
		return count
	}
	run := func() (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, []string{"scrub-access-events"}, env, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}

	code, out := run()
	if code != 0 {
		t.Fatalf("scrub-access-events: exit %d, want 0:\n%s", code, out)
	}
	var leaking []string
	if err := database.Pool.QueryRow(ctx, `
		select array(select payload::text from events where type = 'project.access_changed' and payload::text like '%carol@example.com%')
	`).Scan(&leaking); err != nil || len(leaking) != 0 {
		t.Fatalf("after the scrub the event log still serves carol@example.com in %q, %v; want no access event holding it", leaking, err)
	}
	if got, want := storedPayload(t, database, access), `{"change":"person_added","project":"PUB"}`; got != want || !strings.Contains(out, "rewritten=1") {
		t.Fatalf("the access event after the scrub is %s, want %s, and one event rewritten:\n%s", got, want, out)
	}
	var principal, owner, visibility string
	if err := database.Pool.QueryRow(ctx, `select principal, owner, visibility from project_access_changes where event_id = $1`, access).
		Scan(&principal, &owner, &visibility); err != nil || principal != "carol@example.com" || owner != "olga@example.com" || visibility != "public" {
		t.Fatalf("the scrubbed event's record = %q, %q, %q, %v, want what its payload named: carol@example.com, olga@example.com, public", principal, owner, visibility, err)
	}
	if got := storedPayload(t, database, other); got != otherBefore {
		t.Fatalf("the project.updated event after the scrub is %s, want it as it was, %s", got, otherBefore)
	}
	if count := markers(); count != 1 {
		t.Fatalf("access_events_scrubbed holds %d rows, want 1", count)
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "rewritten=0") || markers() != 1 {
		t.Fatalf("scrub-access-events again: exit %d, %d markers, want 0, nothing rewritten and one marker:\n%s", code, markers(), out)
	}

	appendEach(t, database,
		model.Event{ProjectKey: new("PUB"), Type: "project.access_changed", Actor: olga, Payload: wholeAccessPayload("PUB", "person_removed", "carol@example.com")},
		model.Event{ProjectKey: new("PUB"), Type: "project.access_changed", Actor: olga, Payload: wholeAccessPayload("PUB", "owner", "")},
	)
	report, err := scrubAccessEvents(store.WithTransactionTracking(ctx), database.Pool, 1)
	if err != nil || report.rewritten != 2 || report.events != 4 {
		t.Fatalf("a scrub in batches of one = %+v, %v, want 4 events read and 2 rewritten", report, err)
	}
	if code := runSubcommand(ctx, []string{"scrub-access-events", "extra"}, env, new(bytes.Buffer), new(bytes.Buffer)); code != 2 {
		t.Fatalf("scrub-access-events extra: exit %d, want 2", code)
	}
}
