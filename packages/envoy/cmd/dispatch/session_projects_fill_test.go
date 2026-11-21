package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
// exit code and what it printed, asking a stand-in listener that knows no session.
func runFill(t *testing.T, database *store.Store, args ...string) (int, string) {
	t.Helper()
	return runFillAsking(t, database, fillStandIn(t, nil), args...)
}

// runFillAsking is runFill asking the listener at listenerURL about the stored messages'
// recipients.
func runFillAsking(t *testing.T, database *store.Store, listenerURL string, args ...string) (int, string) {
	t.Helper()
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString(), "ENVOY_URL": listenerURL})
	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), append([]string{"backfill-session-projects"}, args...), env, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// fillStandIn is a listener that answers GET /v1/registry/<id> for each session of known with
// the projects listed there, and 404 for every other session, counting nothing else.
func fillStandIn(t *testing.T, known map[string]string) string {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, "/v1/registry/")
		projects, known := known[id]
		if !ok || !known {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + id + `","projects":` + projects + `,"updated_at":1700000000000}`))
	}))
	t.Cleanup(listener.Close)
	return listener.URL
}

// registrationPairs is every row of session_projects as "<session> <project> <proven_by>", sorted.
func registrationPairs(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select session_id || ' ' || project_key || ' ' || proven_by from session_projects order by 1`)
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

// TestAResetKeepsRegistrationPairs (backfill-session-projects --reset, fillRegistrations): a
// pair the listener's answer proved (proven_by `registration`) survives --reset, which no event
// could write back, and every pair an event proved is filled again. The fill asks the listener
// about each session a stored direct message names: ses_r, which registered under PRIV, is
// recorded (ses_r, PRIV, registration), and ses_gone, which the listener no longer knows, is printed
// with its count of messages.
//
// Control, in the test: the reset as it was, which empties session_projects whole, takes the
// registration pair away, and the check fails naming it.
func TestAResetKeepsRegistrationPairs(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email) values ('olga@example.com');
		insert into projects (key, name, default_assignee) values ('PUB', 'Public', 'olga@example.com');
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ('PUB-1', 'PUB', 1, 'Issue', 'todo', '{"kind":"user","id":"olga@example.com"}', 'U');
		insert into messages (issue_key, author, body, target) values
			(null, '{"kind":"user","id":"olga@example.com"}', 'to r', 'session:ses_r'),
			(null, '{"kind":"user","id":"olga@example.com"}', 'to the gone one', 'session:ses_gone'),
			(null, '{"kind":"user","id":"olga@example.com"}', 'to the gone one again', 'session:ses_gone');
	`); err != nil {
		t.Fatalf("seed a project, its issue and three direct messages: %v", err)
	}
	appendEach(t, database, model.Event{IssueKey: new("PUB-1"), Type: "issue.updated", Actor: provenSession("ses_s"), Payload: map[string]any{}})
	listener := fillStandIn(t, map[string]string{"ses_r": `["PRIV"]`})
	want := []string{"ses_r PRIV registration", "ses_s PUB event"}

	code, out := runFillAsking(t, database, listener, "--reset")
	if code != 0 || !strings.Contains(out, "the listener does not know session ses_gone, which 2 stored messages name") {
		t.Fatalf("backfill-session-projects --reset: exit %d, want 0 and ses_gone named with its two messages:\n%s", code, out)
	}
	if got := registrationPairs(t, database); !slices.Equal(got, want) {
		t.Fatalf("after the first --reset session_projects holds %q, want %q", got, want)
	}
	// The listener forgets ses_r: only the pair its answer recorded says it registered under PRIV.
	if code, out := runFillAsking(t, database, fillStandIn(t, nil), "--reset"); code != 0 {
		t.Fatalf("backfill-session-projects --reset again: exit %d, want 0:\n%s", code, out)
	}
	if got := registrationPairs(t, database); !slices.Equal(got, want) {
		t.Fatalf("after --reset session_projects holds %q, want %q: the registration's pair stays and the event's is filled again", got, want)
	}

	// Control: the reset as it was, every row deleted, loses the pair no event can write back.
	if _, err := database.Pool.Exec(ctx, `delete from session_projects`); err != nil {
		t.Fatalf("control: empty session_projects whole: %v", err)
	}
	if _, err := fillSessionProjects(store.WithTransactionTracking(ctx), database.Pool, sessionProjectsFillBatch); err != nil {
		t.Fatalf("control: fill the events' pairs: %v", err)
	}
	if got := registrationPairs(t, database); slices.Equal(got, want) || slices.Contains(got, "ses_r PRIV registration") {
		t.Fatalf("control: after emptying session_projects whole it holds %q, want ses_r's registration gone", got)
	}
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
// its marker, leaves only what the rule records.
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

	if code, out := runFill(t, database, "--reset", "extra"); code != 2 {
		t.Fatalf("backfill-session-projects --reset extra: exit %d, want 2:\n%s", code, out)
	}
}
