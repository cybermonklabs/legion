// Package textversionstest holds what the tests of more than one package write and read around
// the keyword table, search_text_versions, which the fill of package textversions writes from
// the event log: a test's transactions (Commit, and for one held open BeginTransaction), the
// project and the issues the tests' events name (SeedProject, SeedIssue, InsertIssue), an issue's
// events (WrittenIssue.Event, WrittenIssue.InsertVersion) and their append through the broker
// (AppendEvents, and in a transaction of their own CommitEvents), an issue's run of events of
// every type a keyword rule reads (WriteIssue), the rows the fill wrote for given events (RowsOf,
// RequireRowsOf, RequireOnlyRowsOf, AwaitRows), the fill's cursor (Cursor) and the fill run as a
// serving process runs it (StartFill). A later rules version or a change of the schema changes
// them here, once, for the tests of textversions, of the API and of the dispatch command.
//
// It is an ordinary package rather than a _test.go file because Go cannot share test-only code
// across packages; only tests import it. It does not import textversions, whose own tests import
// it.
package textversionstest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Project is the project SeedProject writes, which the issues of InsertIssue, and so of
// WriteIssue and of SeedIssue, are in.
const Project = "ACME"

// Issue is the issue SeedIssue writes, ACME-1, on which the tests that write comment events alone
// write them. WriteIssue writes the issue of its number itself, so a test writes ACME-1 through
// one of the two.
const Issue = Project + "-1"

// author is the user the events of these helpers name as their actor.
var author = model.Actor{Kind: "user", ID: "alice"}

// SeedProject writes project ACME.
func SeedProject(t testing.TB, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(),
		`insert into projects (key, name, default_assignee) values ($1, 'Acme', 'alice')`, Project); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
}

// SeedIssue writes project ACME (SeedProject) and its issue ACME-1, Issue, with no primary
// document (InsertIssue).
func SeedIssue(t testing.TB, database *store.Store) {
	t.Helper()
	SeedProject(t, database)
	Commit(t, database, func(tx pgx.Tx) { InsertIssue(t, tx, 1, "Rotate the signing keys") })
}

// Commit runs write in a transaction of its own on database and commits it, failing the test on
// any error. The transaction ends before Commit returns, on every way out: one that write fails
// the test in, or that does not commit, is rolled back on context.Background(), so a test whose
// context is done still ends it. A test that holds a transaction open while others run begins it
// with BeginTransaction.
func Commit(t testing.TB, database *store.Store, write func(tx pgx.Tx)) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin a transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	write(tx)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the transaction: %v", err)
	}
}

// BeginTransaction begins a transaction on database for a test that holds it open while other
// transactions run, and answers it and its start (TransactionStart). The test ends it on every
// way out by deferring, at once,
//
//	defer func() { _ = tx.Rollback(context.Background()) }()
//
// which does nothing once the test committed or rolled the transaction back itself. A test that
// begins a transaction, writes and commits uses Commit.
func BeginTransaction(t testing.TB, database *store.Store) (pgx.Tx, time.Time) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin a transaction: %v", err)
	}
	// TransactionStart fails the test where it cannot read the start, before the caller has
	// deferred the transaction's end, so this ends it then.
	held := false
	defer func() {
		if !held {
			_ = tx.Rollback(context.Background())
		}
	}()
	startedAt := TransactionStart(t, tx)
	held = true
	return tx, startedAt
}

// TransactionStart is the start of tx, which is the xact_start pg_stat_activity shows for it and
// the created_at of every event it writes.
func TransactionStart(t testing.TB, tx pgx.Tx) time.Time {
	t.Helper()
	var startedAt time.Time
	if err := tx.QueryRow(t.Context(), `select now()`).Scan(&startedAt); err != nil {
		t.Fatalf("read the transaction's start: %v", err)
	}
	return startedAt
}

// Written is an event a test wrote and how many keyword versions it starts.
type Written struct {
	ID       int64
	Versions int
}

// OneEach is the events ids, each starting one version, as the comment.created of a new comment
// does.
func OneEach(ids ...int64) []Written {
	written := make([]Written, len(ids))
	for index, id := range ids {
		written[index] = Written{ID: id, Versions: 1}
	}
	return written
}

// WriteIssue writes issue ACME-n of Project, which must be there (SeedProject), with its primary
// document at versions 1 and 2, and seventeen events of it in id order, in one transaction, the
// events appended through broker as the routes append them: one of every type a rule reads, each
// changing the text it reads, so each starts one version, and two that start none, an
// issue.updated that keeps the title and an issue.claimed, a type no rule reads. It answers each
// event with the versions it starts.
func WriteIssue(t testing.TB, database *store.Store, broker *events.Broker, n int) []Written {
	t.Helper()
	var written []Written
	Commit(t, database, func(tx pgx.Tx) { written = writeIssue(t, tx, broker, n) })
	return written
}

// writeIssue writes WriteIssue's issue ACME-n, its primary document and its events in tx, and
// answers each event with the versions it starts.
func writeIssue(t testing.TB, tx pgx.Tx, broker *events.Broker, n int) []Written {
	t.Helper()
	title := fmt.Sprintf("Rotate the signing keys of region %d", n)
	issue := InsertIssue(t, tx, n, title, fmt.Sprintf("# Rotation %d\n\nMove each signing key to its successor.", n))
	second := issue.InsertVersion(t, tx, 2,
		fmt.Sprintf("# Rotation %d\n\nMove each signing key of region %d to its successor, then audit the verifiers.", n, n))

	key := issue.Key
	comment, message, ask := uuid.NewString(), uuid.NewString(), uuid.NewString()
	post := func(eventType, id, body string) model.Event {
		return model.Event{IssueKey: &key, Type: eventType, Actor: author,
			Payload: map[string]any{"id": id, "issue_key": key, "body": body}}
	}
	askEvent := func(eventType, question string, answer *model.AskAnswer) model.Event {
		return model.Event{IssueKey: &key, Type: eventType, Actor: author, Payload: model.AskEventPayload{Ask: model.Ask{
			ID: ask, IssueKey: &key, Author: author, Kind: "question", Question: question, State: "open",
			Options: []model.AskOption{{Label: "Rotate now"}, {Label: "Wait a week"}}, Answer: answer,
		}}}
	}
	note := "Rotate them before the audit."
	steps := []struct {
		event    model.Event
		versions int
	}{
		{issue.Event("issue.created", title), 1},
		{second, 1},
		{issue.Event("issue.updated", title+", then audit"), 1},
		{issue.Event("issue.updated", title+", then audit"), 0},
		{model.Event{IssueKey: &key, Type: "issue.claimed", Actor: author, Payload: map[string]any{"key": key}}, 0},
		{issue.Event("issue.closed", title+", audited"), 1},
		{post("comment.created", comment, "The verifiers read the old key until Monday."), 1},
		{post("comment.answered", uuid.NewString(), "Monday works; the audit starts Tuesday."), 1},
		{post("comment.edited", comment, "The verifiers read the old key until Tuesday."), 1},
		{post("message.created", message, "Can the rotation wait for the freeze to end?"), 1},
		{post("message.answered", uuid.NewString(), "It can; the freeze ends Friday."), 1},
		{post("message.edited", message, "Can the rotation wait until the freeze ends on Friday?"), 1},
		{post("comment.deleted", comment, ""), 1},
		{post("message.deleted", message, ""), 1},
		{askEvent("ask.opened", "Rotate the keys now?", nil), 1},
		{askEvent("ask.edited", "Rotate the signing keys now?", nil), 1},
		{askEvent("ask.answered", "Rotate the signing keys now?",
			&model.AskAnswer{User: "alice", Selected: []string{"Rotate now"}, Text: &note, At: time.Now()}), 1},
	}
	appended := make([]model.Event, len(steps))
	for index, step := range steps {
		appended[index] = step.event
	}
	ids := AppendEvents(t, tx, broker, appended...)
	out := make([]Written, len(ids))
	for index, id := range ids {
		out[index] = Written{ID: id, Versions: steps[index].versions}
	}
	return out
}

// WrittenIssue is an issue InsertIssue wrote: its number, its key, the title it was written with
// and its primary document's id, "" for an issue written with none.
type WrittenIssue struct {
	N                    int
	Key, Title, Artifact string
}

// InsertIssue writes issue ACME-n of Project, which must be there (SeedProject), with title, in
// tx, and with markdown its primary document too, at versions 1 onwards, one for each of markdown
// in order. They are all made in tx, so version 1 is made at the created_at of an issue.created
// tx appends, as the create route writes them. It answers the issue.
func InsertIssue(t testing.TB, tx pgx.Tx, n int, title string, markdown ...string) WrittenIssue {
	t.Helper()
	ctx := t.Context()
	issue := WrittenIssue{N: n, Key: fmt.Sprintf("%s-%d", Project, n), Title: title}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, $3, $4, '{"kind":"user","id":"alice"}', 'A')
	`, issue.Key, Project, n, title); err != nil {
		t.Fatalf("insert issue %s: %v", issue.Key, err)
	}
	if len(markdown) == 0 {
		return issue
	}
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue.Key, Project).Scan(&issue.Artifact); err != nil {
		t.Fatalf("insert the primary document of %s: %v", issue.Key, err)
	}
	for index, text := range markdown {
		issue.insertVersion(t, tx, index+1, text)
	}
	return issue
}

// Event is an issue.* event of type eventType of the issue with title, as the routes write one:
// its payload names the issue's key, the title and the issue's primary document.
func (issue WrittenIssue) Event(eventType, title string) model.Event {
	key := issue.Key
	return model.Event{IssueKey: &key, Type: eventType, Actor: author,
		Payload: map[string]any{"key": issue.Key, "title": title, "primary_artifact_id": issue.Artifact}}
}

// InsertVersion writes version number of the issue's primary document, of markdown, in tx, and
// answers the artifact.version event that the route that writes a version appends with it.
func (issue WrittenIssue) InsertVersion(t testing.TB, tx pgx.Tx, number int, markdown string) model.Event {
	t.Helper()
	issue.insertVersion(t, tx, number, markdown)
	key := issue.Key
	return model.Event{IssueKey: &key, Type: "artifact.version", Actor: author,
		Payload: map[string]any{"artifact_id": issue.Artifact, "version": map[string]any{"number": number, "size": len(markdown)}}}
}

// insertVersion writes version number of the issue's primary document, of markdown, in tx.
func (issue WrittenIssue) insertVersion(t testing.TB, tx pgx.Tx, number int, markdown string) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1::uuid, $2, $3, '[]')`,
		issue.Artifact, number, markdown); err != nil {
		t.Fatalf("insert version %d of %s's primary document: %v", number, issue.Key, err)
	}
}

// AppendEvents appends events through broker in tx, in order, queued in one write as the routes
// queue theirs (store.RunWrite), and answers their ids in the same order.
func AppendEvents(t testing.TB, tx pgx.Tx, broker *events.Broker, appended ...model.Event) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(appended))
	if err := store.RunWrite(t.Context(), tx, func(batch *store.WriteBatch) error {
		for _, e := range appended {
			if err := broker.QueueAppend(batch, e, func(written model.Event) { ids = append(ids, written.ID) }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("append %d events: %v", len(appended), err)
	}
	return ids
}

// CommitEvents appends events through broker in a transaction of their own (Commit), in order
// (AppendEvents), and answers their ids in the same order.
func CommitEvents(t testing.TB, database *store.Store, broker *events.Broker, appended ...model.Event) []int64 {
	t.Helper()
	var ids []int64
	Commit(t, database, func(tx pgx.Tx) { ids = AppendEvents(t, tx, broker, appended...) })
	return ids
}
