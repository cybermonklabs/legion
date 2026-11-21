package events

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// provenSession is the actor of session id's write under a credential that proved the session, a
// broker-issued token: its act names that same session and no personal token
// (api.bearerSessionActor).
func provenSession(id string) model.Actor {
	return model.Actor{Kind: "session", ID: id, Owner: new("carol@example.com"), Act: &model.ActorAct{Session: id}}
}

// appendAll appends events in tx as one write phase, as a write with several events does. Append
// writes one event a phase, and TestASessionsEventRecordsItsProject needs several in one: a later
// statement of the phase sees the session's record an earlier one queued and writes none.
func appendAll(ctx context.Context, tx pgx.Tx, broker *Broker, events []model.Event) ([]model.Event, error) {
	appended := make([]model.Event, len(events))
	err := store.RunWrite(ctx, tx, func(batch *store.WriteBatch) error {
		for index, event := range events {
			if err := broker.QueueAppend(batch, event, func(written model.Event) { appended[index] = written }); err != nil {
				return err
			}
		}
		return nil
	})
	return appended, err
}

// appendCommitted appends events in one transaction of their own and commits it.
func appendCommitted(t *testing.T, database *store.Store, broker *Broker, events ...model.Event) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := appendAll(ctx, tx, broker, events); err != nil {
		t.Fatalf("append %d events: %v", len(events), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// sessionProjectPairs is every row of session_projects as "<session> <project>", sorted.
func sessionProjectPairs(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select session_id, project_key from session_projects order by session_id, project_key`)
	if err != nil {
		t.Fatalf("read session_projects: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var sessionID, project string
		if err := rows.Scan(&sessionID, &project); err != nil {
			t.Fatalf("scan session_projects: %v", err)
		}
		got = append(got, sessionID+" "+project)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read session_projects: %v", err)
	}
	return got
}

// TestASessionsEventRecordsItsProject: every event a proven session appends on an owner of a
// project records that the session acted in the project (session_projects), once however many
// events it appends there, whatever owner kind it appends on. An event a person appends records
// nothing, and neither does one of a credential that names no session (a personal token's actor,
// whose id is empty) or an ownerless event, which belongs to no project.
func TestASessionsEventRecordsItsProject(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := seedEventOwners(t, database)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('QQ', 'Other', 'alice')`); err != nil {
		t.Fatalf("seed a second project: %v", err)
	}
	broker := NewBroker()
	session := provenSession("ses_writer")
	tokenOwner := "carol@example.com"
	write := func(events ...model.Event) {
		t.Helper()
		appendCommitted(t, database, broker, events...)
	}

	issueEvent := func(key string, actor model.Actor) model.Event {
		return model.Event{IssueKey: new(key), Type: "issue.updated", Actor: actor, Payload: map[string]any{}}
	}
	write(issueEvent("PP-1", session), issueEvent("PP-2", session))
	write(issueEvent("PP-1", session))
	write(model.Event{ArtifactID: new(artifactID), Type: "artifact.version", Actor: provenSession("ses_documents"), Payload: map[string]any{}})
	write(model.Event{ProjectKey: new("QQ"), Type: "project.created", Actor: session, Payload: map[string]any{}})
	write(issueEvent("PP-1", model.Actor{Kind: "user", ID: "alice"}))
	write(issueEvent("PP-1", model.Actor{Kind: "session", Owner: &tokenOwner}))
	write(model.Event{Type: "access_group.changed", Actor: session, Payload: model.AccessGroupEventPayload{Name: "reviewers", Change: model.AccessGroupCreated}})

	got := sessionProjectPairs(t, database)
	if want := []string{"ses_documents PP", "ses_writer PP", "ses_writer QQ"}; !slices.Equal(got, want) {
		t.Fatalf("session_projects = %v, want %v", got, want)
	}
}

// TestOnlyAProvenSessionRecordsItsProject: an event records its session's project only when the
// credential behind it proved that session: its act names the actor's own session and no personal
// token. A credential that proves no session names the session in the request body
// (api.bearerSessionActor), so the shared token naming T, whose actor carries no act, and a
// personal token naming U, whose act carries its token id, each record nothing: neither may make
// another session look like one of a project's agents (access.SessionClause).
func TestOnlyAProvenSessionRecordsItsProject(t *testing.T) {
	database := storetest.Open(t)
	seedEventOwners(t, database)
	broker := NewBroker()
	for _, actor := range []model.Actor{
		{Kind: "session", ID: "ses_s", Act: &model.ActorAct{Session: "ses_s"}},
		{Kind: "session", ID: "ses_t"},
		{Kind: "session", ID: "ses_u", Owner: new("carol@example.com"), Act: &model.ActorAct{Session: "ses_u", TokenID: "t1"}},
	} {
		appendCommitted(t, database, broker, model.Event{IssueKey: new("PP-1"), Type: "issue.updated", Actor: actor, Payload: map[string]any{}})
	}
	if got, want := sessionProjectPairs(t, database), []string{"ses_s PP"}; !slices.Equal(got, want) {
		t.Fatalf("session_projects = %v, want %v: only the session its credential proved records its project", got, want)
	}
}

// Two writes that are a session's first in one project, holding crossed issue locks, both
// commit. The first write records (session, PP) and holds PP-1; the second, on PP-2, records the
// same pair and commits; then the first takes PP-2. A record that waited on the first's
// uncommitted row would hold PP-2 while the first waits for PP-2: a deadlock Postgres breaks by
// aborting one of them. Recording the pair waits on nothing, so the second commits while the first
// is still open, and the first then takes PP-2 and commits.
func TestTwoFirstRecordsOfAPairWithCrossedIssueLocksBothCommit(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedEventOwners(t, database)
	broker := NewBroker()
	session := provenSession("ses_crossed")
	issueEvent := func(key string) model.Event {
		return model.Event{IssueKey: new(key), Type: "issue.updated", Actor: session, Payload: map[string]any{}}
	}

	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first write: %v", err)
	}
	defer first.Rollback(ctx)
	if _, err := broker.Append(ctx, first, issueEvent("PP-1")); err != nil {
		t.Fatalf("the first write on PP-1: %v", err)
	}

	// The second write's backend is read before it writes, so the test can watch that one backend
	// queue behind the first write's transaction (storetest.QueuedBehind): a record of the pair
	// that waited on the first's uncommitted row queues there, and fails the test the moment it
	// does. The 30 s bound only turns a write that neither commits nor queues into a failure.
	pids := make(chan uint32, 1)
	second := make(chan error, 1)
	go func() {
		second <- func() error {
			tx, err := database.Pool.Begin(context.Background())
			if err != nil {
				pids <- 0
				return err
			}
			pids <- tx.Conn().PgConn().PID()
			defer tx.Rollback(context.Background())
			if _, err := broker.Append(context.Background(), tx, issueEvent("PP-2")); err != nil {
				return err
			}
			return tx.Commit(context.Background())
		}()
	}()
	pid := <-pids
	deadline := time.Now().Add(30 * time.Second)
	for committed := false; !committed; {
		select {
		case err := <-second:
			if err != nil {
				t.Fatalf("the second write on PP-2 = %v, want it committed", err)
			}
			committed = true
			continue
		default:
		}
		queued, err := storetest.QueuedBehind(ctx, first, pid)
		if err != nil {
			t.Fatalf("read whether the second write queues behind the first: %v", err)
		}
		if queued {
			t.Fatal("the second write on PP-2 waits on a lock the first write holds: its record of the pair waits for the first's uncommitted row")
		}
		if time.Now().After(deadline) {
			t.Fatal("the second write on PP-2 neither committed nor queued behind the first within 30s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := broker.Append(ctx, first, issueEvent("PP-2")); err != nil {
		t.Fatalf("the first write, taking PP-2 after the second committed: %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first write: %v", err)
	}
	// Neither write saw the other's row before it committed, so the pair is stored twice: the
	// one shape the table's missing unique key allows, which every reader answers as once
	// (access.TestAPairRecordedTwiceReadsAsOnce).
	var rows int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from session_projects where session_id = 'ses_crossed' and project_key = 'PP'
	`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("(ses_crossed, PP) rows = %d, %v, want 2: one from each write open at once", rows, err)
	}
}

// A change of a project's access and a change of a group are bookkeeping: retained on their log
// and carried by the event feed, but nobody is woken by them. A group belongs to no project, so
// its event names no owner and is keyed by the group.
func TestAccessEventsAreQuiet(t *testing.T) {
	broker := NewBroker()
	person := model.Actor{Kind: "user", ID: "alice"}
	for _, event := range []model.Event{
		{Type: "project.access_changed", ProjectKey: new("PP"), Actor: person, Payload: model.ProjectAccessEventPayload{Change: model.AccessPersonAdded}},
		{Type: "access_group.changed", Actor: person, Payload: model.AccessGroupEventPayload{Name: "reviewers", Change: model.AccessGroupUpdated}},
	} {
		if !Quiet(event) || broker.Notify(event) {
			t.Errorf("%s: quiet=%t notify=%t, want quiet and no wake", event.Type, Quiet(event), broker.Notify(event))
		}
	}
	key, err := eventOwnerKey(model.Event{Type: "access_group.changed", Payload: model.AccessGroupEventPayload{Name: "reviewers"}})
	if err != nil || key != "access_group:reviewers" {
		t.Fatalf("eventOwnerKey(access_group.changed) = %q, %v, want access_group:reviewers", key, err)
	}
	if _, err := eventOwnerKey(model.Event{Type: "issue.updated", Payload: model.AccessGroupEventPayload{Name: "reviewers"}}); err == nil {
		t.Fatal("an ownerless group payload under another type was keyed; want it refused")
	}
}
