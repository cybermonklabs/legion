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

// provenSession is session id's actor under a credential that proved the session, a
// broker-issued token: its act names that same session and no personal token.
func provenSession(id string) model.Actor {
	return model.Actor{Kind: "session", ID: id, Owner: new("carol@example.com"), Act: &model.ActorAct{Session: id}}
}

// appendEach appends each event in a transaction of its own through the broker, as a route does.
func appendEach(t *testing.T, database *store.Store, list ...model.Event) {
	t.Helper()
	ctx := context.Background()
	broker := events.NewBroker()
	for _, event := range list {
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
}

// sessionPairs is every row of session_projects as "<session> <project>", sorted.
func sessionPairs(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select session_id || ' ' || project_key from session_projects order by 1`)
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

// filledRows counts session_projects_filled's rows.
func filledRows(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from session_projects_filled`).Scan(&count); err != nil {
		t.Fatalf("count session_projects_filled: %v", err)
	}
	return count
}

// runFill runs `envoy-dispatch backfill-session-projects` with args on database, answering its
// exit code and what it printed.
func runFill(t *testing.T, database *store.Store, args ...string) (int, string) {
	t.Helper()
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), append([]string{"backfill-session-projects"}, args...), env, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// envoy-dispatch backfill-session-projects records, for every event written before the live
// insert existed, the (session, project) pair the live insert would have recorded for it
// (events.Broker.QueueAppend): a proven session's events on an issue, on a project document and
// on a project, and none for a person's event or a personal token's, whose actor names no
// session. The events here are appended through the broker, its pairs read, and then deleted, so
// the fill must put back exactly what the live insert wrote. A second run writes no pair, and the
// run leaves the one session_projects_filled row. A fill in batches of two crosses every batch
// boundary and leaves the same pairs.
func TestTheSessionProjectsFillCoversEveryEvent(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
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

	onIssue := func(actor model.Actor) model.Event {
		return model.Event{IssueKey: new("WID-1"), Type: "issue.updated", Actor: actor, Payload: map[string]any{}}
	}
	onDocument := func(actor model.Actor) model.Event {
		return model.Event{ArtifactID: new(document), Type: "artifact.updated", Actor: actor, Payload: map[string]any{}}
	}
	onProject := func(actor model.Actor) model.Event {
		return model.Event{ProjectKey: new("GAD"), Type: "project.updated", Actor: actor, Payload: map[string]any{}}
	}
	appendEach(t, database,
		onIssue(provenSession("ses_issue")),
		onIssue(provenSession("ses_issue")),
		onDocument(provenSession("ses_document")),
		onProject(provenSession("ses_project")),
		onProject(provenSession("ses_issue")),
		onIssue(model.Actor{Kind: "user", ID: "olga@example.com"}),
		onIssue(model.Actor{Kind: "session", Owner: new("carol@example.com")}),
		onDocument(model.Actor{Kind: "session", Owner: new("carol@example.com")}),
	)

	forget := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `delete from session_projects; delete from session_projects_filled`); err != nil {
			t.Fatalf("forget the recorded pairs: %v", err)
		}
	}

	// The live insert's answer is the one the fill is held to; it is not empty, so a fill that
	// records nothing cannot match it.
	live := sessionPairs(t, database)
	if want := []string{"ses_document GAD", "ses_issue GAD", "ses_issue WID", "ses_project GAD"}; !slices.Equal(live, want) {
		t.Fatalf("the live insert recorded %q, want %q", live, want)
	}
	forget()

	if code, out := runFill(t, database); code != 0 {
		t.Fatalf("backfill-session-projects: exit %d, want 0:\n%s", code, out)
	}
	if got := sessionPairs(t, database); !slices.Equal(got, live) {
		t.Fatalf("after the fill session_projects holds %q, want what the live insert recorded, %q", got, live)
	}
	if filled := filledRows(t, database); filled != 1 {
		t.Fatalf("after the fill session_projects_filled holds %d rows, want 1", filled)
	}
	if code, out := runFill(t, database); code != 0 || !strings.Contains(out, "pairs=0") {
		t.Fatalf("backfill-session-projects again: exit %d, want 0 and no pair written:\n%s", code, out)
	}
	if got := sessionPairs(t, database); !slices.Equal(got, live) {
		t.Fatalf("after the second fill session_projects holds %q, want %q and no row more", got, live)
	}
	if filled := filledRows(t, database); filled != 1 {
		t.Fatalf("after the second fill session_projects_filled holds %d rows, want 1", filled)
	}

	forget()
	if _, err := fillSessionProjects(store.WithTransactionTracking(ctx), database.Pool, 2); err != nil {
		t.Fatalf("fill in batches of two: %v", err)
	}
	if got := sessionPairs(t, database); !slices.Equal(got, live) {
		t.Fatalf("after a fill in batches of two session_projects holds %q, want %q", got, live)
	}
	if filled := filledRows(t, database); filled != 1 {
		t.Fatalf("after a fill in batches of two session_projects_filled holds %d rows, want 1", filled)
	}
}

// TestTheSessionProjectsFillRecordsOnlyProvenSessions: the fill holds a stored event to the live
// insert's rule (events.provedSession), so of three events on a PUB issue - session S under a
// credential that proved it, session T named in the body of the shared token, whose actor carries
// no act, and session U named by a personal token, whose act carries its token id - it records
// (S, PUB) alone. A pair recorded under an earlier rule is never taken away by a fill: (T, PUB),
// written by hand, stays after a plain fill, and --reset, which first empties session_projects and
// its marker, leaves only what the rule records. --reset leaves session_reached_projects as it was:
// what a session's transcript holds stays in it.
func TestTheSessionProjectsFillRecordsOnlyProvenSessions(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email) values ('olga@example.com');
		insert into projects (key, name, default_assignee) values ('PUB', 'Public', 'olga@example.com');
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ('PUB-1', 'PUB', 1, 'Issue', 'todo', '{"kind":"user","id":"olga@example.com"}', 'U');
	`); err != nil {
		t.Fatalf("seed a project and its issue: %v", err)
	}
	onIssue := func(actor model.Actor) model.Event {
		return model.Event{IssueKey: new("PUB-1"), Type: "issue.updated", Actor: actor, Payload: map[string]any{}}
	}
	appendEach(t, database,
		onIssue(model.Actor{Kind: "session", ID: "ses_s", Act: &model.ActorAct{Session: "ses_s"}}),
		onIssue(model.Actor{Kind: "session", ID: "ses_t"}),
		onIssue(model.Actor{Kind: "session", ID: "ses_u", Owner: new("carol@example.com"), Act: &model.ActorAct{Session: "ses_u", TokenID: "t1"}}),
	)
	if _, err := database.Pool.Exec(ctx, `delete from session_projects`); err != nil {
		t.Fatalf("forget the live insert's pairs: %v", err)
	}
	proven := []string{"ses_s PUB"}

	if code, out := runFill(t, database); code != 0 {
		t.Fatalf("backfill-session-projects: exit %d, want 0:\n%s", code, out)
	}
	if got := sessionPairs(t, database); !slices.Equal(got, proven) {
		t.Fatalf("after the fill session_projects holds %q, want %q: only the session its credential proved", got, proven)
	}

	if _, err := database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ('ses_t', 'PUB')`); err != nil {
		t.Fatalf("record (ses_t, PUB) as the earlier rule did: %v", err)
	}
	withEarlier := []string{"ses_s PUB", "ses_t PUB"}
	if code, out := runFill(t, database); code != 0 {
		t.Fatalf("backfill-session-projects again: exit %d, want 0:\n%s", code, out)
	}
	if got := sessionPairs(t, database); !slices.Equal(got, withEarlier) {
		t.Fatalf("after a fill without --reset session_projects holds %q, want %q: a fill takes no pair away", got, withEarlier)
	}
	if _, err := database.Pool.Exec(ctx, `insert into session_reached_projects (session_id, project_key) values ('ses_t', 'PUB')`); err != nil {
		t.Fatalf("record (ses_t, PUB) as a project ses_t reached: %v", err)
	}
	code, out := runFill(t, database, "--reset")
	if code != 0 || !strings.Contains(out, "pairs=1") {
		t.Fatalf("backfill-session-projects --reset: exit %d, want 0 and the one proven pair written again:\n%s", code, out)
	}
	if got := sessionPairs(t, database); !slices.Equal(got, proven) {
		t.Fatalf("after --reset session_projects holds %q, want %q", got, proven)
	}
	if filled := filledRows(t, database); filled != 1 {
		t.Fatalf("after --reset session_projects_filled holds %d rows, want 1", filled)
	}
	var reached int
	if err := database.Pool.QueryRow(ctx, `select count(*) from session_reached_projects where session_id = 'ses_t' and project_key = 'PUB'`).Scan(&reached); err != nil || reached != 1 {
		t.Fatalf("after --reset session_reached_projects holds (ses_t, PUB) %d times (err %v), want once: nothing removes a reached pair", reached, err)
	}

	if code, out := runFill(t, database, "--reset", "extra"); code != 2 {
		t.Fatalf("backfill-session-projects --reset extra: exit %d, want 2:\n%s", code, out)
	}
}

// sessionOwnerPairs is every row of session_owners as "<session> <login>", sorted.
func sessionOwnerPairs(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select session_id || ' ' || login from session_owners order by 1`)
	if err != nil {
		t.Fatalf("read session_owners: %v", err)
	}
	defer rows.Close()
	var read []string
	for rows.Next() {
		var pair string
		if err := rows.Scan(&pair); err != nil {
			t.Fatalf("read session_owners: %v", err)
		}
		read = append(read, pair)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read session_owners: %v", err)
	}
	return read
}

// TestTheSessionProjectsFillTiesEachProvedSessionToItsPerson: the fill records in session_owners
// the (session, person) pair the live insert records (events.queueSessionProject), the person
// canonical by model.CanonicalLogin's rule: white space trimmed, ASCII letters lowered and no other
// letter folded, so " Carol@Example.COM " is carol@example.com and Ève@Example.com keeps its È.
// A session two people's credentials proved holds both pairs. A session named in a body (no act),
// one a personal token's act names, and a proved session whose actor names no person record
// nothing. The events are appended through the broker and its pairs deleted, so the fill must put
// back exactly what the live insert wrote, and a second fill writes nothing more.
//
// Control: the fill's owner read with lower() in place of CanonicalLoginSQL; it records
// ève@example.com, and the test fails naming it.
func TestTheSessionProjectsFillTiesEachProvedSessionToItsPerson(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email) values ('olga@example.com');
		insert into projects (key, name, default_assignee) values ('PUB', 'Public', 'olga@example.com');
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ('PUB-1', 'PUB', 1, 'Issue', 'todo', '{"kind":"user","id":"olga@example.com"}', 'U');
	`); err != nil {
		t.Fatalf("seed a project and its issue: %v", err)
	}
	onIssue := func(actor model.Actor) model.Event {
		return model.Event{IssueKey: new("PUB-1"), Type: "issue.updated", Actor: actor, Payload: map[string]any{}}
	}
	proved := func(session, owner string) model.Actor {
		return model.Actor{Kind: "session", ID: session, Owner: new(owner), Act: &model.ActorAct{Session: session}}
	}
	appendEach(t, database,
		onIssue(proved("ses_a", " Carol@Example.COM ")),
		onIssue(proved("ses_a", "carol@example.com")),
		onIssue(proved("ses_a", "bob@outside.example")),
		onIssue(proved("ses_e", "Ève@Example.com")),
		onIssue(model.Actor{Kind: "session", ID: "ses_t", Owner: new("carol@example.com")}),
		onIssue(model.Actor{Kind: "session", ID: "ses_u", Owner: new("carol@example.com"), Act: &model.ActorAct{Session: "ses_u", TokenID: "t1"}}),
		onIssue(model.Actor{Kind: "session", ID: "ses_n", Act: &model.ActorAct{Session: "ses_n"}}),
	)
	live := sessionOwnerPairs(t, database)
	if want := []string{"ses_a bob@outside.example", "ses_a carol@example.com", "ses_e Ève@example.com"}; !slices.Equal(live, want) {
		t.Fatalf("the live insert recorded %q, want %q", live, want)
	}
	if _, err := database.Pool.Exec(ctx, `delete from session_owners`); err != nil {
		t.Fatalf("forget the live insert's pairs: %v", err)
	}
	for _, run := range []string{"the fill", "a second fill"} {
		if code, out := runFill(t, database); code != 0 {
			t.Fatalf("backfill-session-projects (%s): exit %d, want 0:\n%s", run, code, out)
		}
		if got := sessionOwnerPairs(t, database); !slices.Equal(got, live) {
			t.Fatalf("after %s session_owners holds %q, want what the live insert recorded, %q", run, got, live)
		}
	}
}
