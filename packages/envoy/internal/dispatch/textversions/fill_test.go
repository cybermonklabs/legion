package textversions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheFillContinuesFromItsCursorAfterARestart writes 3,000 events of every kind a rule reads,
// and some no rule reads, and stops a fill as its second batch commits, as a process killed while
// it catches up stops. A fill in a second process, with a pool and a horizon of its own, starts at
// the cursor the first left: its first batch reads the event after the cursor first. At the end
// every event has exactly the rows it starts, none twice and none missing, and the cursor row says
// rules version 1 wrote them.
func TestTheFillContinuesFromItsCursorAfterARestart(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()
	var want []textversionstest.Written
	for n := 1; len(want) < 3000; n++ {
		want = append(want, textversionstest.WriteIssue(t, database, broker, n)...)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var firstLogs logtest.Buffer
	first := testFiller(database, events.NewHorizon(database.Pool), &firstLogs)
	committed := 0
	first.rest = func(ctx context.Context, _ time.Duration) error {
		if committed++; committed == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := first.toHorizon(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the first fill, stopped after its second batch, answered %v; want it cancelled", err)
	}
	left := readCursorRow(t, database)
	firstBatches := batchLines(t, &firstLogs)
	if len(firstBatches) != 2 || firstBatches[1].Cursor != left.eventID {
		t.Fatalf("the first fill logged batches %+v and left the cursor at %d; want two batches, the second ending at the cursor",
			firstBatches, left.eventID)
	}
	if last := want[len(want)-1].ID; left.eventID >= last {
		t.Fatalf("the first fill's cursor is %d, at or past the last event %d: it stopped with nothing left to write", left.eventID, last)
	}

	second := storetest.SecondProcess(t, database)
	var secondLogs logtest.Buffer
	if err := testFiller(second, events.NewHorizon(second.Pool), &secondLogs).toHorizon(t.Context()); err != nil {
		t.Fatalf("the second process's fill: %v", err)
	}
	var next int64
	if err := database.Pool.QueryRow(t.Context(), `select min(id) from events where id > $1`, left.eventID).Scan(&next); err != nil {
		t.Fatalf("read the event after the cursor: %v", err)
	}
	secondBatches := batchLines(t, &secondLogs)
	if len(secondBatches) == 0 || secondBatches[0].First != next {
		t.Fatalf("the second process's fill logged batches %+v; want its first to start at event %d, the one after the cursor %d",
			secondBatches, next, left.eventID)
	}
	textversionstest.RequireOnlyRowsOf(t, database, want)
	end := readCursorRow(t, database)
	if end.eventID != want[len(want)-1].ID || end.rules != rulesVersion {
		t.Fatalf("the cursor row ends at event %d with rules %d; want event %d and rules %d",
			end.eventID, end.rules, want[len(want)-1].ID, rulesVersion)
	}
}

// TestTheFillWritesANewEventWithinSixtySecondsOfTheHorizon holds the fill to 60 s from the moment
// the horizon passes an event to the event's rows, where nothing announces the moment the horizon
// moves: with the fill caught up, a transaction that holds a transaction id and writes no event
// stays open while three events commit, so the horizon stands below them, and then ends with no
// notification. The fill reads the horizon again on its own, 1 s and then up to 8 s apart, and
// the three events have their rows within 60 s of the moment Horizon.Await answers an id at or
// above them: under the fill's hold, and with no holder.
func TestTheFillWritesANewEventWithinSixtySecondsOfTheHorizon(t *testing.T) {
	for _, held := range []bool{true, false} {
		name := "under its hold"
		if !held {
			name = "with no holder"
		}
		t.Run(name, func(t *testing.T) {
			database := storetest.Open(t)
			textversionstest.SeedIssue(t, database)
			broker := events.NewBroker()
			horizon := events.NewHorizon(database.Pool)
			fill := testFiller(database, horizon, io.Discard)
			fill.rest = sleep
			holder := uuid.Nil
			if held {
				holder = takeFillHold(t, database)
				fill.holder = holder
			}
			ctx := t.Context()
			commitComment(t, database, broker, textversionstest.Issue)
			if err := fill.toHorizon(ctx); err != nil {
				t.Fatalf("catch the fill up: %v", err)
			}

			holding, _ := textversionstest.BeginTransaction(t, database)
			defer func() { _ = holding.Rollback(context.Background()) }()
			if _, err := holding.Exec(ctx, `select pg_current_xact_id()`); err != nil {
				t.Fatalf("take a transaction id: %v", err)
			}
			var ids []int64
			for range 3 {
				ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
			}
			done := make(chan error, 1)
			go func() { done <- fill.toHorizon(ctx) }()
			// The fill reads the horizon below the three events and reads it again, at 1 s and then
			// 2 s, while the transaction holds it there.
			time.Sleep(3 * time.Second)
			if rows := textversionstest.RowsOf(t, database, ids...); rows != 0 {
				t.Fatalf("the fill wrote %d rows of events the horizon had not passed", rows)
			}
			if err := holding.Rollback(ctx); err != nil {
				t.Fatalf("end the transaction that holds the horizon: %v", err)
			}
			safe, err := horizon.Await(ctx)
			if err != nil {
				t.Fatalf("await the horizon: %v", err)
			}
			if safe < ids[2] {
				t.Fatalf("Await answered %d, below the third event %d", safe, ids[2])
			}
			passed := time.Now()
			if !testwait.Holds(time.Minute, 50*time.Millisecond, func() bool {
				return textversionstest.RowsOf(t, database, ids...) >= len(ids)
			}) {
				t.Fatalf("the three events had %d rows a minute after the horizon passed them", textversionstest.RowsOf(t, database, ids...))
			}
			t.Logf("the rows came %s after the horizon passed the events", time.Since(passed).Round(time.Millisecond))
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the fill: %v", err)
				}
			case <-time.After(time.Minute):
				t.Fatal("the fill did not return once the horizon passed every event it saw")
			}
			if got := readCursorRow(t, database); got.eventID != ids[2] || got.holder != holderText(holder) {
				t.Fatalf("the cursor row is at event %d naming holder %q; want event %d and holder %q",
					got.eventID, got.holder, ids[2], holderText(holder))
			}
		})
	}
}

// TestTheFillStartsAgainAfterTheEventLogIsReset resets the event log under a fill that wrote the
// rows of about 100 events, in the two forms that empty it: `truncate events restart identity`,
// after which the new log stands below the cursor, and the delete and sequence restart that
// e2e/seed.ts runs, here refilled past the cursor, so the row at the cursor's id is another
// event. The fill finds the reset, empties the table and its cursor and writes the new log from
// its first event: no row of the old log is left, and the new events have exactly their rows.
func TestTheFillStartsAgainAfterTheEventLogIsReset(t *testing.T) {
	for _, form := range []struct {
		name  string
		reset []string
		// issues is how many issues the new log holds the events of, 14 events each; pastCursor
		// is whether that reaches past the old log's cursor.
		issues     int
		pastCursor bool
	}{
		{name: "truncate restart identity", reset: []string{`truncate events restart identity cascade`}, issues: 2},
		{name: "delete and restart the sequence", reset: []string{`delete from events`, `alter sequence events_id_seq restart`}, issues: 9, pastCursor: true},
	} {
		t.Run(form.name, func(t *testing.T) {
			database := storetest.Open(t)
			textversionstest.SeedProject(t, database)
			broker := events.NewBroker()
			var logs logtest.Buffer
			fill := testFiller(database, events.NewHorizon(database.Pool), &logs)
			ctx := t.Context()
			const oldIssues = 8
			var before []textversionstest.Written
			for n := 1; n <= oldIssues; n++ {
				before = append(before, textversionstest.WriteIssue(t, database, broker, n)...)
			}
			if err := fill.toHorizon(ctx); err != nil {
				t.Fatalf("fill the old log: %v", err)
			}
			textversionstest.RequireOnlyRowsOf(t, database, before)
			left := readCursorRow(t, database)

			for _, statement := range form.reset {
				if _, err := database.Pool.Exec(ctx, statement); err != nil {
					t.Fatalf("reset the event log (%s): %v", statement, err)
				}
			}
			var after []textversionstest.Written
			for n := oldIssues + 1; n <= oldIssues+form.issues; n++ {
				after = append(after, textversionstest.WriteIssue(t, database, broker, n)...)
			}
			if past := after[len(after)-1].ID > left.eventID; past != form.pastCursor {
				t.Fatalf("the new log ends at event %d against the old cursor %d; the test meant it past the cursor: %t",
					after[len(after)-1].ID, left.eventID, form.pastCursor)
			}
			if err := fill.toHorizon(ctx); err != nil {
				t.Fatalf("fill the new log: %v", err)
			}

			var oldRows int
			oldKeys := make([]string, 0, oldIssues)
			for n := 1; n <= oldIssues; n++ {
				oldKeys = append(oldKeys, fmt.Sprintf("ACME-%d", n))
			}
			if err := database.Pool.QueryRow(ctx, `select count(*) from search_text_versions where issue_key = any($1)`, oldKeys).
				Scan(&oldRows); err != nil {
				t.Fatalf("count the old log's rows: %v", err)
			}
			if oldRows != 0 {
				t.Fatalf("%d rows of the old log's items are left", oldRows)
			}
			textversionstest.RequireOnlyRowsOf(t, database, after)
			end := readCursorRow(t, database)
			var lastAt time.Time
			if err := database.Pool.QueryRow(ctx, `select created_at from events where id = $1`, after[len(after)-1].ID).Scan(&lastAt); err != nil {
				t.Fatalf("read the new log's last event: %v", err)
			}
			if end.eventID != after[len(after)-1].ID || end.eventAt == nil || !end.eventAt.Equal(lastAt) || end.reach == nil {
				t.Fatalf("the cursor row is at event %d of %v with reach %v; want event %d of %s and a reach",
					end.eventID, end.eventAt, end.reach, after[len(after)-1].ID, lastAt)
			}
			// The batch after the reset reads the new log from its first event.
			var firstNew int64
			if err := database.Pool.QueryRow(ctx, `select min(id) from events`).Scan(&firstNew); err != nil {
				t.Fatalf("read the new log's first event: %v", err)
			}
			lines := logLines(t, &logs)
			reset := slices.IndexFunc(lines, func(line logLine) bool { return line.Msg == resetMessage })
			if reset < 0 {
				t.Fatalf("the fill logged no reset: %+v", lines)
			}
			next := slices.IndexFunc(lines[reset:], func(line logLine) bool { return line.Msg == batchMessage })
			if next < 0 || lines[reset+next].First != firstNew {
				t.Fatalf("after the reset the fill logged %+v; want a batch starting at the new log's first event %d", lines[reset:], firstNew)
			}
		})
	}
}

// TestAFillThatLostItsHoldWritesNothing has another holder take the fill's hold over between two
// of its batches: the hold lapses and the other process takes it. The next batch finds that
// duty_holds no longer names its holder and writes nothing: no row and no move of the cursor,
// which still names the first holder, and the fill answers duty.ErrHoldLost.
func TestAFillThatLostItsHoldWritesNothing(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()
	var want []textversionstest.Written
	for n := 1; n <= 3; n++ {
		want = append(want, textversionstest.WriteIssue(t, database, broker, n)...)
	}
	ctx := t.Context()
	holder := uuid.New()
	hold, err := duty.TryHold(ctx, database.Pool, fillHoldName, holder, holdOptions)
	if err != nil || hold == nil {
		t.Fatalf("take the fill's hold: %v, %v", hold, err)
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	fill := testFiller(database, events.NewHorizon(database.Pool), io.Discard)
	fill.holder, fill.maxEvents = holder, 14
	var afterFirst cursorRow
	var rowsAfterFirst int
	fill.rest = func(ctx context.Context, _ time.Duration) error {
		if afterFirst.eventID != 0 {
			return nil
		}
		if err := hold.Lapse(ctx); err != nil {
			t.Fatalf("let the fill's hold lapse: %v", err)
		}
		taken, err := duty.TryHold(ctx, database.Pool, fillHoldName, uuid.New(), holdOptions)
		if err != nil || taken == nil {
			t.Fatalf("another holder takes the lapsed hold: %v, %v", taken, err)
		}
		t.Cleanup(func() { _ = taken.Release(context.Background()) })
		afterFirst, rowsAfterFirst = readCursorRow(t, database), countRows(t, database)
		return nil
	}
	if err := fill.toHorizon(ctx); !errors.Is(err, duty.ErrHoldLost) {
		t.Fatalf("the fill whose hold another holder took answered %v; want duty.ErrHoldLost", err)
	}
	if afterFirst.eventID == 0 || afterFirst.eventID >= want[len(want)-1].ID {
		t.Fatalf("the first batch left the cursor at %d; want a batch that leaves events before event %d", afterFirst.eventID, want[len(want)-1].ID)
	}
	if got := readCursorRow(t, database); !got.equal(afterFirst) {
		t.Fatalf("after the hold was taken over the cursor row is %+v; want it as the first batch left it, %+v", got, afterFirst)
	}
	if got := countRows(t, database); got != rowsAfterFirst {
		t.Fatalf("the table holds %d rows; the first batch left %d", got, rowsAfterFirst)
	}
	if afterFirst.holder != holder.String() {
		t.Fatalf("the cursor row names holder %q; want the first holder %s", afterFirst.holder, holder)
	}
}

// TestAFillStopsAtAnEventItsRulesCannotRead writes, between two issues' events, a comment.created
// whose payload carries no body, which no rule can read. The fill writes the rows of every event
// before it and stops there, with an error that names it: the cursor stands at the event before,
// and no event after it has a row, so none is passed by.
func TestAFillStopsAtAnEventItsRulesCannotRead(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()
	ctx := t.Context()
	before := textversionstest.WriteIssue(t, database, broker, 1)
	key := textversionstest.Project + "-1"
	var unreadable model.Event
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		appended, err := broker.Append(ctx, tx, model.Event{IssueKey: &key, Type: "comment.created",
			Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{"id": uuid.NewString(), "issue_key": key}})
		if err != nil {
			t.Fatalf("append the unreadable event: %v", err)
		}
		unreadable = appended
	})
	after := textversionstest.WriteIssue(t, database, broker, 2)

	err := FillToHorizon(ctx, database, events.NewHorizon(database.Pool))
	if !errors.Is(err, errUnreadablePayload) || !strings.Contains(err.Error(), fmt.Sprintf("event %d", unreadable.ID)) {
		t.Fatalf("the fill answered %v; want the rules' unreadable payload, naming event %d", err, unreadable.ID)
	}
	textversionstest.RequireOnlyRowsOf(t, database, before)
	if got := readCursorRow(t, database).eventID; got != before[len(before)-1].ID {
		t.Fatalf("the cursor is at event %d; want %d, the event before the unreadable one", got, before[len(before)-1].ID)
	}
	if rows := textversionstest.RowsOf(t, database, after[0].ID); rows != 0 {
		t.Fatalf("an event after the unreadable one has %d rows", rows)
	}
}

// TestAnIssueWithAnEmptyPrimaryDocumentIDHasVersionsThatNameNone writes an issue with no primary
// document as the API writes one: its issue.created, and an issue.updated that retitles it, each
// name "" as the issue's primary_artifact_id. Each starts a version that names no document
// version, and the fill writes both and moves its cursor past them.
func TestAnIssueWithAnEmptyPrimaryDocumentIDHasVersionsThatNameNone(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	ctx := t.Context()
	key := textversionstest.Issue
	event := func(eventType, title string) model.Event {
		return model.Event{IssueKey: &key, Type: eventType, Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: map[string]any{"key": key, "title": title, "primary_artifact_id": ""}}
	}
	ids := textversionstest.CommitEvents(t, database, broker,
		event("issue.created", "Rotate the signing keys"), event("issue.updated", "Rotate the signing keys, then audit"))

	if err := FillToHorizon(ctx, database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("the fill answered %v; want the issue's two versions written", err)
	}
	textversionstest.RequireOnlyRowsOf(t, database, textversionstest.OneEach(ids...))
	var named int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from search_text_versions where spec_artifact_id is not null or spec_version is not null
	`).Scan(&named); err != nil {
		t.Fatalf("count the versions that name a document version: %v", err)
	}
	if named != 0 {
		t.Fatalf("%d of the issue's versions name a document version; want none", named)
	}
	if got := readCursorRow(t, database).eventID; got != ids[len(ids)-1] {
		t.Fatalf("the cursor is at event %d; want %d, the issue's last", got, ids[len(ids)-1])
	}
}

// TestOnlyAnIDTheFillCastsMustBeAUUID holds each id a statement of the batch casts to uuid to
// the rules: an issue's primary_artifact_id, an artifact.version's artifact_id and a cut post's
// id. The rules pass only a uuid's canonical form, a subset of the forms Postgres's uuid input
// reads, which holds each form a Dispatch writer writes: Postgres reads as a uuid each id of the
// test that the rules pass, and the rules pass the canonical form in lower, upper and mixed case.
// Any other id is a payload the rules cannot read, so the fill writes the rows of every event
// before it and stops there, with an error that names it, rather than failing a statement on
// every retry: one that is no uuid, and one that holds a uuid in a form uuid.Parse reads and
// Postgres refuses (a urn:uuid:, or a uuid between two spaces, between two letters, or between a
// brace and a parenthesis). A whole post's id, which the fill never casts, need not be a uuid: its
// version is written, and the fill moves past it.
func TestOnlyAnIDTheFillCastsMustBeAUUID(t *testing.T) {
	const canonical = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	key := otherIssue
	actor := model.Actor{Kind: "user", ID: "alice"}
	issueEvent := func(eventType string) func(primary string) model.Event {
		return func(primary string) model.Event {
			return model.Event{IssueKey: &key, Type: eventType, Actor: actor,
				Payload: map[string]any{"key": key, "title": "Audit the verifiers", "primary_artifact_id": primary}}
		}
	}
	artifactVersion := func(artifactID string) model.Event {
		return model.Event{IssueKey: &key, Type: "artifact.version", Actor: actor,
			Payload: map[string]any{"artifact_id": artifactID, "version": map[string]any{"number": 1, "size": 40}}}
	}
	post := func(eventType string, cut bool) func(id string) model.Event {
		return func(id string) model.Event {
			return model.Event{IssueKey: &key, Type: eventType, Actor: actor, Payload: map[string]any{
				"id": id, "issue_key": key, "body": "The verifiers read the old key until Monday.", "body_truncated": cut}}
		}
	}
	type testCase struct {
		name  string
		event func(id string) model.Event
		id    string
		stops bool
	}
	tests := []testCase{
		{"an issue.created whose primary_artifact_id is no uuid", issueEvent("issue.created"), "spec.md", true},
		{"an artifact.version whose artifact_id is no uuid", artifactVersion, "spec.md", true},
		{"a cut comment.created whose id is no uuid", post("comment.created", true), "comment-1", true},
		{"a cut message.created whose id is no uuid", post("message.created", true), "message-1", true},
		{"a whole comment.created whose id is no uuid", post("comment.created", false), "comment-1", false},
	}
	for _, form := range []struct{ name, id string }{
		{"a urn:uuid:", "urn:uuid:" + canonical},
		{"a uuid between two spaces", " " + canonical + " "},
		{"a uuid between two letters", "x" + canonical + "y"},
		{"a uuid between a brace and a parenthesis", "{" + canonical + ")"},
	} {
		tests = append(tests,
			testCase{"an issue.updated whose primary_artifact_id is " + form.name, issueEvent("issue.updated"), form.id, true},
			testCase{"an artifact.version whose artifact_id is " + form.name, artifactVersion, form.id, true},
			testCase{"a cut comment.created whose id is " + form.name, post("comment.created", true), form.id, true})
	}

	t.Run("Postgres reads as a uuid each id the rules pass", func(t *testing.T) {
		database := storetest.Open(t)
		ids := []string{canonical, strings.ToUpper(canonical), "a0EEbc99-9C0b-4eF8-Bb6d-6bB9bd380A11"}
		for _, id := range ids {
			if !isUUID(id) {
				t.Errorf("the rules refuse %q, a uuid's canonical form", id)
			}
		}
		for _, test := range tests {
			ids = append(ids, test.id)
		}
		for _, id := range ids {
			var reads bool
			if err := database.Pool.QueryRow(t.Context(), `select pg_input_is_valid($1, 'uuid')`, id).Scan(&reads); err != nil {
				t.Fatalf("ask Postgres whether its uuid input reads %q: %v", id, err)
			}
			if isUUID(id) && !reads {
				t.Errorf("the rules pass %q, which Postgres's uuid input refuses", id)
			}
		}
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			textversionstest.SeedProject(t, database)
			broker := events.NewBroker()
			before := textversionstest.WriteIssue(t, database, broker, 1)
			seedOtherIssue(t, database)
			event := test.event(test.id)
			id := textversionstest.CommitEvents(t, database, broker, event)[0]
			after := textversionstest.WriteIssue(t, database, broker, 3)

			err := FillToHorizon(t.Context(), database, events.NewHorizon(database.Pool))
			if !test.stops {
				if err != nil {
					t.Fatalf("the fill answered %v; want every event's rows written", err)
				}
				textversionstest.RequireOnlyRowsOf(t, database, slices.Concat(before, textversionstest.OneEach(id), after))
				return
			}
			named := fmt.Sprintf("event %d (%s)", id, event.Type)
			if !errors.Is(err, errUnreadablePayload) || !strings.Contains(err.Error(), named) {
				t.Fatalf("the fill answered %v; want the rules' unreadable payload, naming %s", err, named)
			}
			textversionstest.RequireOnlyRowsOf(t, database, before)
			if got := readCursorRow(t, database).eventID; got != before[len(before)-1].ID {
				t.Fatalf("the cursor is at event %d; want %d, the event before the unreadable one", got, before[len(before)-1].ID)
			}
			if rows := textversionstest.RowsOf(t, database, after[0].ID); rows != 0 {
				t.Fatalf("an event after the unreadable one has %d rows", rows)
			}
		})
	}
}

// TestABatchCountsTheSpecMarkdownItsIssueRowsRead holds each batch, at the production bound of
// 2 MB (batchBytes), to the text its vectors read, in three logs the fill writes from its first
// event. What each batch reads the test measures itself, from the rows the batch wrote and the
// events between its first event and its cursor as its batch line names them (batchText): their
// payloads, the markdown of the primary document version each issue row names and the body of
// each post row whose event cut it. Each log has a batch of more than one event, and each such
// batch reads at most 2 MB of it; each batch commits under its statement_timeout (a batch that
// runs past it fails the fill), and every event has its rows.
//
//   - 24 issues created with a primary document of about 1 MiB, each then retitled, which
//     carries that version forward, and 4 of them given a new version of about 1 MiB: each of
//     their 52 rows reads about 1 MiB of markdown. A count of their payloads alone, as the fill
//     counted at a5078553, puts the 48 issue events in one batch.
//   - A comment of 10 KB, carried whole, and 400 comment.edited events that leave it as it was,
//     so they start no row: their payloads still count, about 4 MB, so they take two batches at
//     least.
//   - 30 comments created with bodies of about 100 KB, which their events cut, with their
//     revision 1 indexed in their own transactions, each then edited to a few words, all before
//     the fill runs: each comment.created version reads revision 1's blocks, about 3 MB in all,
//     where the comment's row holds the few words; a count of the row's body puts them in one
//     batch.
func TestABatchCountsTheSpecMarkdownItsIssueRowsRead(t *testing.T) {
	t.Run("the markdown each issue row reads", func(t *testing.T) {
		database := storetest.Open(t)
		textversionstest.SeedProject(t, database)
		broker := events.NewBroker()
		const issueCount, newVersions = 24, 4
		var want []textversionstest.Written
		issues := make([]textversionstest.WrittenIssue, 0, issueCount)
		for n := 1; n <= issueCount; n++ {
			issue, created := writeSpecIssue(t, database, broker, n)
			issues, want = append(issues, issue), append(want, created)
		}
		retitles := make([]model.Event, 0, len(issues))
		for _, issue := range issues {
			retitles = append(retitles, issue.Event("issue.updated", issue.Title+", then audit the verifiers"))
		}
		for _, id := range textversionstest.CommitEvents(t, database, broker, retitles...) {
			want = append(want, textversionstest.Written{ID: id, Versions: 1})
		}
		for _, issue := range issues[:newVersions] {
			want = append(want, writeSpecVersion(t, database, broker, issue, 2))
		}
		fillWithinTheBound(t, database, want)
	})

	t.Run("the payload of an event that starts no row", func(t *testing.T) {
		database := storetest.Open(t)
		textversionstest.SeedIssue(t, database)
		broker := events.NewBroker()
		body := rotationParagraphs(1, 10_000)
		if _, truncated := model.EventBody(body); truncated {
			t.Fatal("the comment's body is one an event cuts; the case needs one its events carry whole")
		}
		comment := writeComment(t, database, broker, body, true)
		want := []textversionstest.Written{{ID: comment.event, Versions: 1}}
		textversionstest.Commit(t, database, func(tx pgx.Tx) {
			startedAt := textversionstest.TransactionStart(t, tx)
			for range 400 {
				want = append(want, textversionstest.Written{ID: appendCommentEvent(t, tx, broker, "comment.edited", comment.id, body, startedAt)})
			}
		})
		if batches := fillWithinTheBound(t, database, want); len(batches) < 2 {
			t.Fatalf("the fill wrote the comment and its 400 edits in %d batch: their payloads, about 4 MB, take two at least", len(batches))
		}
	})

	t.Run("the revision 1 a cut comment reads", func(t *testing.T) {
		database := storetest.Open(t)
		textversionstest.SeedIssue(t, database)
		broker := events.NewBroker()
		const comments = 30
		var want []textversionstest.Written
		for n := 1; n <= comments; n++ {
			body := rotationParagraphs(n, 100_000)
			if _, truncated := model.EventBody(body); !truncated {
				t.Fatal("the comment's body is one an event carries whole; the case needs one its event cuts")
			}
			comment := writeComment(t, database, broker, body, true)
			edit := editComment(t, database, broker, comment.id, fmt.Sprintf("Rotated region %d; the plan is in the spec.", n))
			want = append(want, textversionstest.Written{ID: comment.event, Versions: 1}, textversionstest.Written{ID: edit, Versions: 1})
		}
		batches := fillWithinTheBound(t, database, want)
		var fromBlocks int
		if err := database.Pool.QueryRow(t.Context(),
			`select count(*) from search_text_versions where kind = 'comment' and body_from = $1`, bodyFromBlocks).Scan(&fromBlocks); err != nil {
			t.Fatalf("count the rows whose body is revision 1's blocks: %v", err)
		}
		if fromBlocks != comments {
			t.Fatalf("%d of the %d comment.created rows hold revision 1's blocks; the case tests the bodies the rules read from them", fromBlocks, comments)
		}
		if len(batches) < 2 {
			t.Fatalf("the fill wrote the %d comments and their edits in %d batch: their revision 1, about 3 MB, takes two at least", comments, len(batches))
		}
	})
}

// TestTheCursorKeepsTheLowestRulesVersionThatWroteUpToIt holds the cursor row's rules to the
// lowest rules version that wrote the rows up to the cursor. A fill that creates the cursor row
// writes this build's, 1. Set to 0, as a build of an older rules version leaves it (the column
// has no check), it stays 0 while this build's fill writes the next events and moves the cursor
// past them. After a reset of the event log the fill writes the cursor row again, with 1.
func TestTheCursorKeepsTheLowestRulesVersionThatWroteUpToIt(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()
	ctx := t.Context()
	fill := testFiller(database, events.NewHorizon(database.Pool), io.Discard)
	first := textversionstest.WriteIssue(t, database, broker, 1)
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("fill the first events: %v", err)
	}
	if created := readCursorRow(t, database); created.eventID != first[len(first)-1].ID || created.rules != rulesVersion {
		t.Fatalf("the cursor row the fill created is at event %d with rules %d; want event %d and rules %d",
			created.eventID, created.rules, first[len(first)-1].ID, rulesVersion)
	}

	if _, err := database.Pool.Exec(ctx, `update search_text_versions_fill set rules = 0`); err != nil {
		t.Fatalf("set the cursor row's rules to an older build's: %v", err)
	}
	later := textversionstest.WriteIssue(t, database, broker, 2)
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("fill the later events: %v", err)
	}
	textversionstest.RequireOnlyRowsOf(t, database, append(first, later...))
	if got := readCursorRow(t, database); got.eventID != later[len(later)-1].ID || got.rules != 0 {
		t.Fatalf("after this build wrote past an older build's cursor the row is at event %d with rules %d; want event %d and rules 0, the older build's",
			got.eventID, got.rules, later[len(later)-1].ID)
	}

	if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	after := textversionstest.WriteIssue(t, database, broker, 3)
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("fill the new log: %v", err)
	}
	textversionstest.RequireOnlyRowsOf(t, database, after)
	if got := readCursorRow(t, database); got.eventID != after[len(after)-1].ID || got.rules != rulesVersion {
		t.Fatalf("after a reset the cursor row is at event %d with rules %d; want event %d and rules %d",
			got.eventID, got.rules, after[len(after)-1].ID, rulesVersion)
	}
}

// fillWithinTheBound fills database from its first event with the production bounds and holds
// what it wrote: the fill returns no error, so every batch committed under its statement_timeout;
// every event of want has its rows and no other event has one; and each batch of more than one
// event reads at most batchBytes (batchText). It fails t when no batch holds more than one event,
// since then it checked no batch's bound. It answers the batches' lines, in order.
func fillWithinTheBound(t *testing.T, database *store.Store, want []textversionstest.Written) []logLine {
	t.Helper()
	var logs logtest.Buffer
	fill := testFiller(database, events.NewHorizon(database.Pool), &logs)
	started := time.Now()
	if err := fill.toHorizon(t.Context()); err != nil {
		t.Fatalf("the fill, which a batch past its statement_timeout fails: %v", err)
	}
	batches := batchLines(t, &logs)
	t.Logf("the fill wrote %d events in %d batches in %s", len(want), len(batches), time.Since(started).Round(time.Millisecond))
	textversionstest.RequireOnlyRowsOf(t, database, want)
	var measured int
	for _, b := range batches {
		if b.Events < 2 {
			continue
		}
		measured++
		if read := batchText(t, database, b.First, b.Cursor); read > batchBytes {
			t.Errorf("the batch of the %d events %d to %d reads %d bytes of text into its vectors and its payloads; want at most %d",
				b.Events, b.First, b.Cursor, read, batchBytes)
		}
	}
	if measured == 0 {
		t.Errorf("the fill wrote no batch of two or more events in its %d batches, so no batch's bound was checked", len(batches))
	}
	return batches
}

// batchTextQuery measures what the batch of the events $1 to $2 reads, from the events and the
// rows they started: the payload of each event of a type a rule reads ($3), the markdown of the
// primary document version each issue row names, and the body of each post row that does not
// hold its event's (body_from other than 'event'), which the rules read from another source.
const batchTextQuery = `
	select coalesce((select sum(octet_length(e.payload::text)) from events e
	                  where e.id between $1 and $2 and e.type = any($3)), 0)
	     + coalesce((select sum(case when v.kind = 'issue' then coalesce(octet_length(a.markdown), 0)
	                                 when v.body_from <> 'event' then octet_length(v.body)
	                                 else 0 end)
	                   from search_text_versions v
	                   left join artifact_versions a on a.artifact_id = v.spec_artifact_id and a.number = v.spec_version
	                  where v.event_id between $1 and $2), 0)`

// batchText is what the batch of the events first to last reads (batchTextQuery), measured from
// the rows it wrote, not from what the fill counted.
func batchText(t *testing.T, database *store.Store, first, last int64) int64 {
	t.Helper()
	var read int64
	if err := database.Pool.QueryRow(t.Context(), batchTextQuery, first, last, ruleTypes).Scan(&read); err != nil {
		t.Fatalf("measure the text of the batch of events %d to %d: %v", first, last, err)
	}
	return read
}

// specBytes is about the size of each primary document version specMarkdown makes: about 1 MiB,
// under pmdoc.MaxDocumentBytes, so two of them fit in a batch's 2 MB and three do not.
const specBytes = 1_000_000

// specMarkdown is version number of issue n's primary document: a heading and paragraphs of
// words, up to specBytes.
func specMarkdown(n, number int) string {
	const paragraph = "Each verifier of the region reads the signing key until the rotation moves it to its successor, and the audit confirms the move.\n\n"
	var b strings.Builder
	fmt.Fprintf(&b, "# Rotation %d, version %d\n\n", n, number)
	for b.Len()+len(paragraph) <= specBytes {
		b.WriteString(paragraph)
	}
	return b.String()
}

// rotationParagraphs is a post's body of about size bytes about region n: paragraphs of about
// 10 KB, each a block the create route takes (under contracts.DispatchBlockUnits).
func rotationParagraphs(n, size int) string {
	var paragraphs []string
	for total := 0; total < size; {
		var b strings.Builder
		for step := 1; b.Len() < 10_000 && total+b.Len() < size; step++ {
			fmt.Fprintf(&b, "Step %d of region %d moves one signing key to its successor. ", step, n)
		}
		paragraph := strings.TrimSpace(b.String())
		paragraphs = append(paragraphs, paragraph)
		total += len(paragraph) + 2
	}
	return strings.Join(paragraphs, "\n\n")
}

// writeSpecIssue writes issue ACME-n, its primary document with version 1 of specMarkdown, and
// its issue.created event, in one transaction, as the create route writes them, and answers the
// issue and its event, which starts one version.
func writeSpecIssue(t *testing.T, database *store.Store, broker *events.Broker, n int) (textversionstest.WrittenIssue, textversionstest.Written) {
	t.Helper()
	spec := specMarkdown(n, 1)
	if len(spec) >= pmdoc.MaxDocumentBytes {
		t.Fatalf("the spec is %d bytes, at or over pmdoc.MaxDocumentBytes, %d", len(spec), pmdoc.MaxDocumentBytes)
	}
	var issue textversionstest.WrittenIssue
	var created []int64
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		issue = textversionstest.InsertIssue(t, tx, n, fmt.Sprintf("Rotate the signing keys of region %d", n), spec)
		created = textversionstest.AppendEvents(t, tx, broker, issue.Event("issue.created", issue.Title))
	})
	return issue, textversionstest.Written{ID: created[0], Versions: 1}
}

// writeSpecVersion writes version number of issue's primary document, of specMarkdown, and its
// artifact.version event, in one transaction, and answers the event, which starts one version.
func writeSpecVersion(t *testing.T, database *store.Store, broker *events.Broker, issue textversionstest.WrittenIssue, number int) textversionstest.Written {
	t.Helper()
	var appended []int64
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		appended = textversionstest.AppendEvents(t, tx, broker, issue.InsertVersion(t, tx, number, specMarkdown(issue.N, number)))
	})
	return textversionstest.Written{ID: appended[0], Versions: 1}
}

// takeFillHold takes the fill's hold for a new holder, released once the test ends, and answers
// the holder.
func takeFillHold(t *testing.T, database *store.Store) uuid.UUID {
	t.Helper()
	holder := uuid.New()
	hold, err := duty.TryHold(t.Context(), database.Pool, fillHoldName, holder, holdOptions)
	if err != nil || hold == nil {
		t.Fatalf("take the fill's hold: %v, %v", hold, err)
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	return holder
}

// holderText is holder as the cursor row's holder column reads as text, "" for none.
func holderText(holder uuid.UUID) string {
	if holder == uuid.Nil {
		return ""
	}
	return holder.String()
}

// testFiller is a fill on database's shared pool up to horizon, logging to logs at debug level,
// that does not rest between batches, so a test of many batches is not twice their length.
func testFiller(database *store.Store, horizon *events.Horizon, logs io.Writer) *filler {
	fill := newFiller(database.Pool, horizon)
	fill.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fill.rest = func(context.Context, time.Duration) error { return nil }
	return fill
}

// committedEvent is an event a test committed and the start of the transaction that wrote it,
// which is its created_at.
type committedEvent struct {
	id        int64
	startedAt time.Time
}

// otherIssue is a second issue of textversionstest.Issue's project (seedOtherIssue): an event of it
// takes no lock an open transaction holds on textversionstest.Issue's events.
const otherIssue = "ACME-2"

// seedOtherIssue writes otherIssue, beside textversionstest.Issue, with no primary document
// (textversionstest.InsertIssue).
func seedOtherIssue(t *testing.T, database *store.Store) {
	t.Helper()
	textversionstest.Commit(t, database, func(tx pgx.Tx) { textversionstest.InsertIssue(t, tx, 2, "Audit the verifiers") })
}

// commitComment commits a comment.created event of a new comment on issue, in a transaction of
// its own, the event appended through the broker, and answers it. Two transactions that append an
// event of one issue take turns on that issue's lock, so a test that holds one open while another
// commits gives them two issues.
func commitComment(t *testing.T, database *store.Store, broker *events.Broker, issue string) committedEvent {
	t.Helper()
	var committed committedEvent
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		startedAt := textversionstest.TransactionStart(t, tx)
		actor := model.Actor{Kind: "user", ID: "alice"}
		appended, err := broker.Append(t.Context(), tx, model.Event{IssueKey: &issue, Type: "comment.created", Actor: actor,
			Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: uuid.NewString(), IssueKey: &issue, Author: actor, Body: "The signing keys rotate on Monday.", CreatedAt: startedAt,
				Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
			}}})
		if err != nil {
			t.Fatalf("append a comment.created of %s: %v", issue, err)
		}
		committed = committedEvent{id: appended.ID, startedAt: startedAt}
	})
	return committed
}

// countRows counts the keyword table's rows.
func countRows(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from search_text_versions`).Scan(&count); err != nil {
		t.Fatalf("count the keyword table's rows: %v", err)
	}
	return count
}

// cursorRow is the fill's cursor row, its holder as text ("" for none).
type cursorRow struct {
	eventID   int64
	eventAt   *time.Time
	reach     *time.Time
	holder    string
	rules     int
	updatedAt time.Time
}

// equal is whether r and other hold the same values.
func (r cursorRow) equal(other cursorRow) bool {
	sameTime := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == b
		}
		return a.Equal(*b)
	}
	return r.eventID == other.eventID && sameTime(r.eventAt, other.eventAt) && sameTime(r.reach, other.reach) &&
		r.holder == other.holder && r.rules == other.rules && r.updatedAt.Equal(other.updatedAt)
}

// readCursorRow reads the fill's cursor row, which must be there.
func readCursorRow(t *testing.T, database *store.Store) cursorRow {
	t.Helper()
	var row cursorRow
	if err := database.Pool.QueryRow(t.Context(), `
		select event_id, event_at, reach, coalesce(holder::text, ''), rules, updated_at
		  from search_text_versions_fill where id = 1
	`).Scan(&row.eventID, &row.eventAt, &row.reach, &row.holder, &row.rules, &row.updatedAt); err != nil {
		t.Fatalf("read the fill's cursor row: %v", err)
	}
	return row
}

// logLine is what the tests read of one line a fill logged.
type logLine struct {
	Msg    string
	First  int64
	Cursor int64
	Events int
}

// logLines reads every line of logs, a JSON handler's output, in order (logLineOf).
func logLines(t *testing.T, logs *logtest.Buffer) []logLine {
	t.Helper()
	var lines []logLine
	for _, record := range logtest.JSONLines(t, logs.String()) {
		lines = append(lines, logLineOf(record))
	}
	return lines
}

// batchLines is the batch lines of logs, in order. Each must hold a JSON number at events,
// first_event and cursor, and one that does not fails t, naming its line, the keys it lacks and
// the line itself: logLineOf reads a number a line lacks as 0, and fillWithinTheBound skips a
// batch of fewer than 2 events, so a batch line that dropped or renamed its count would pass the
// bound with no batch checked.
func batchLines(t *testing.T, logs *logtest.Buffer) []logLine {
	t.Helper()
	written := logs.String()
	var batches []logLine
	for i, record := range logtest.JSONLines(t, written) {
		line := logLineOf(record)
		if line.Msg != batchMessage {
			continue
		}
		var missing []string
		for _, key := range []string{"events", "first_event", "cursor"} {
			if _, ok := record[key].(float64); !ok {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			// JSONLines answers one record for each line of written, so record i is line i.
			t.Fatalf("line %d of the log, a batch line, holds no number at %s:\n%s",
				i+1, strings.Join(missing, ", "), strings.TrimSuffix(slices.Collect(strings.Lines(written))[i], "\n"))
		}
		batches = append(batches, line)
	}
	return batches
}

// logLineOf is what the tests read of record, one line of a fill's log as JSONLines decodes it:
// its msg, first_event, cursor and events, each zero where the line holds no string or no JSON
// number there. JSONLines decodes a number to a float64, which holds an event id exactly.
func logLineOf(record map[string]any) logLine {
	msg, _ := record["msg"].(string)
	first, _ := record["first_event"].(float64)
	cursor, _ := record["cursor"].(float64)
	batched, _ := record["events"].(float64)
	return logLine{Msg: msg, First: int64(first), Cursor: int64(cursor), Events: int(batched)}
}
