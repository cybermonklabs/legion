package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// envoy-dispatch backfill-session-projects records, for every event written before the live
// insert existed, the (session, project) pair the live insert would have recorded for it
// (events.Broker.QueueAppend): a session's events on an issue, on a project document and on a
// project, and none for a person's event or a personal token's, whose actor names no session. The
// events here are appended through the broker, its pairs read, and then deleted, so the fill must
// put back exactly what the live insert wrote. A second run writes no pair, and the run leaves the
// one session_projects_filled row. A fill in batches of two crosses every batch boundary and
// leaves the same pairs.
func TestTheSessionProjectsFillCoversEveryEvent(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email) values ('olga@example.com'), ('carol@example.com');
		insert into projects (key, name, default_assignee) values
			('WID', 'Widgets', 'olga@example.com'), ('GAD', 'Gadgets', 'olga@example.com');
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ('WID-1', 'WID', 1, 'Issue', 'todo', '{"kind":"user","id":"olga@example.com"}', 'U');
	`); err != nil {
		t.Fatalf("seed people, projects and an issue: %v", err)
	}
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('GAD', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"olga@example.com"}') returning id::text
	`).Scan(&document); err != nil {
		t.Fatalf("seed a project document: %v", err)
	}

	session := func(id string) model.Actor { return model.Actor{Kind: "session", ID: id} }
	onIssue := func(actor model.Actor) model.Event {
		return model.Event{IssueKey: new("WID-1"), Type: "issue.updated", Actor: actor, Payload: map[string]any{}}
	}
	onDocument := func(actor model.Actor) model.Event {
		return model.Event{ArtifactID: new(document), Type: "artifact.updated", Actor: actor, Payload: map[string]any{}}
	}
	onProject := func(actor model.Actor) model.Event {
		return model.Event{ProjectKey: new("GAD"), Type: "project.updated", Actor: actor, Payload: map[string]any{}}
	}
	broker := events.NewBroker()
	for _, event := range []model.Event{
		onIssue(session("ses_issue")),
		onIssue(session("ses_issue")),
		onDocument(session("ses_document")),
		onProject(session("ses_project")),
		onProject(session("ses_issue")),
		onIssue(model.Actor{Kind: "user", ID: "olga@example.com"}),
		onIssue(model.Actor{Kind: "session", Owner: new("carol@example.com")}),
		onDocument(model.Actor{Kind: "session", Owner: new("carol@example.com")}),
	} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := broker.Append(ctx, tx, event); err != nil {
			t.Fatalf("append %s by %+v: %v", event.Type, event.Actor, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	pairs := func() []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `select session_id || ' ' || project_key from session_projects order by 1`)
		if err != nil {
			t.Fatalf("read session_projects: %v", err)
		}
		defer rows.Close()
		var read []string
		for rows.Next() {
			var pair string
			if err := rows.Scan(&pair); err != nil {
				t.Fatalf("read session_projects: %v", err)
			}
			read = append(read, pair)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read session_projects: %v", err)
		}
		return read
	}
	filledRows := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from session_projects_filled`).Scan(&count); err != nil {
			t.Fatalf("count session_projects_filled: %v", err)
		}
		return count
	}
	forget := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `delete from session_projects; delete from session_projects_filled`); err != nil {
			t.Fatalf("forget the recorded pairs: %v", err)
		}
	}

	// The live insert's answer is the one the fill is held to; it is not empty, so a fill that
	// records nothing cannot match it.
	live := pairs()
	if want := []string{"ses_document GAD", "ses_issue GAD", "ses_issue WID", "ses_project GAD"}; !slices.Equal(live, want) {
		t.Fatalf("the live insert recorded %q, want %q", live, want)
	}
	forget()

	run := func() (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, []string{"backfill-session-projects"}, env, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	if code, out := run(); code != 0 {
		t.Fatalf("backfill-session-projects: exit %d, want 0:\n%s", code, out)
	}
	if got := pairs(); !slices.Equal(got, live) {
		t.Fatalf("after the fill session_projects holds %q, want what the live insert recorded, %q", got, live)
	}
	if filled := filledRows(); filled != 1 {
		t.Fatalf("after the fill session_projects_filled holds %d rows, want 1", filled)
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "pairs=0") {
		t.Fatalf("backfill-session-projects again: exit %d, want 0 and no pair written:\n%s", code, out)
	}
	if got := pairs(); !slices.Equal(got, live) {
		t.Fatalf("after the second fill session_projects holds %q, want %q and no row more", got, live)
	}
	if filled := filledRows(); filled != 1 {
		t.Fatalf("after the second fill session_projects_filled holds %d rows, want 1", filled)
	}

	forget()
	if _, err := fillSessionProjects(store.WithTransactionTracking(ctx), database.Pool, 2); err != nil {
		t.Fatalf("fill in batches of two: %v", err)
	}
	if got := pairs(); !slices.Equal(got, live) {
		t.Fatalf("after a fill in batches of two session_projects holds %q, want %q", got, live)
	}
	if filled := filledRows(); filled != 1 {
		t.Fatalf("after a fill in batches of two session_projects_filled holds %d rows, want 1", filled)
	}
}
