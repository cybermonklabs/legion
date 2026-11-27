package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// recordAsAnOlderOutbox adds reached to the published_destinations of event, and marks it
// published, as the outbox of a build before this release records an event it published: on a
// connection that carries none of this release's pool's startup parameters, the marker writer
// setting among them (store.StoreMarkerWriterSetting), and with no delivered_to tuple.
func recordAsAnOlderOutbox(t *testing.T, database *store.Store, event int64, reached []string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect as an older build's outbox: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `
		update events set published_destinations = published_destinations || $2::text[], published_at = coalesce(published_at, now())
		where id = $1
	`, event, reached); err != nil {
		t.Fatalf("record event %d as published to %v as an older build's outbox: %v", event, reached, err)
	}
}

// queuedEvents is the ids the queue table holds, in order: events_published_owed, the queue of
// what an older build's outbox recorded, or events_inbox_owed, the queue of the events of an older
// build's writes no row's marker records.
func queuedEvents(t *testing.T, database *store.Store, table string) []int64 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select event_id from `+table+` order by event_id`)
	if err != nil {
		t.Fatalf("read the queue %s: %v", table, err)
	}
	queued, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("read the queue %s: %v", table, err)
	}
	return queued
}

// publishedAt is the moment event was marked published.
func publishedAt(t *testing.T, database *store.Store, event int64) time.Time {
	t.Helper()
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(), `select published_at from events where id = $1`, event).Scan(&at); err != nil {
		t.Fatalf("read when event %d was published: %v", event, err)
	}
	return at
}

// requirePublishedAgrees fails unless the census checks rows rows of published_destinations, with
// no difference in either direction and no row of the queue (unmarked).
func requirePublishedAgrees(t *testing.T, report *StoreReport, rows int64, when string) {
	t.Helper()
	kind := censusKind(t, report, publishedDeliveriesKind)
	if kind.Rows != rows || kind.RowDifferences != 0 || kind.NodeDifferences != 0 || kind.Unmarked != 0 {
		t.Errorf("%s published_destinations reads %d row(s), %d rows no tuple states, %d tuples no row projects (%v %v) and %d queued, want %d rows, no difference and none queued",
			when, kind.Rows, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes, kind.Unmarked, rows)
	}
}

// Two issue messages a build before this release wrote, each published by that build's outbox to
// a session's topic and a role's: the outbox of the store's own release states no delivered_to of
// a message's event (its deliveredItem names a comment's event alone), and a build before it
// states none at all. The first was recorded before this release's schema reached the database, so
// no trigger queued it; the second after, so the trigger of migration 20261122100122 queued it.
// After the backfill the census checks the four destinations as delivered_to tuples out of each
// message's node, under its event, as this release's outbox states them (outbox's deliveredGraph),
// with no difference and no row of the queue, and a second run appends nothing. Its control is the
// published-delivery pass taken out of the backfill (inboxPasses): the four destinations stay rows
// no tuple states.
func TestTheBackfillStatesWhatAnOlderBuildsOutboxPublishedToASessionOrARole(t *testing.T) {
	f := newInboxCensus(t)
	writes := f.writes
	ctx := context.Background()
	reached := []string{contracts.AgentTopicPrefix + "ses-route", contracts.RoleTopicPrefix + "published-role"}
	var messages []olderWrite
	for _, body := range []string{"For whoever the issue is routed to.", "And again."} {
		messages = append(messages, writeAsAnOlderBuild(t, writes, "message.created", alice, `
			insert into messages (issue_key, author, body) values ('CENS-1', $1, $2)
			returning id::text, created_at`, actorJSON(t, alice), body))
	}
	for _, message := range messages {
		recordAsAnOlderOutbox(t, writes.database, message.event, reached)
	}
	// The first record landed before this release's schema reached the database.
	if _, err := writes.database.Pool.Exec(ctx, `delete from events_published_owed where event_id = $1`, messages[0].event); err != nil {
		t.Fatalf("take the first record off the queue: %v", err)
	}
	if queued := queuedEvents(t, writes.database, "events_published_owed"); len(queued) != 1 || queued[0] != messages[1].event {
		t.Fatalf("the queue holds %v before the backfill, want the second message's event %d alone", queued, messages[1].event)
	}

	backfilled := backfillInboxOnce(t, writes.database)
	report, _ := f.census(t, "after the backfill")
	requirePublishedAgrees(t, report, 4, "after the backfill")
	if queued := queuedEvents(t, writes.database, "events_published_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	for _, message := range messages {
		node := graphstore.KindMessage + ":" + message.id
		at := publishedAt(t, writes.database, message.event)
		requireTuples(t, writes.database, "the deliveries of message "+message.id, []string{graphstore.TupleDeliveredTo}, []string{node},
			innerTuple{from: node, kind: graphstore.TupleDeliveredTo, to: graphstore.KindSession + ":ses-route", event: message.event, at: at},
			innerTuple{from: node, kind: graphstore.TupleDeliveredTo, to: graphstore.KindRole + ":published-role", event: message.event, at: at},
		)
	}
	if done := reportedKind(t, backfilled, publishedDeliveriesKind); done.Rows != 2 || done.Tuples != 4 {
		t.Errorf("the published_destinations pass reports %#v, want 2 events and 4 tuples", done)
	}
	requireASecondRunAppendsNothing(t, writes.database)
}

// This release's outbox, which states the delivered_to of each event it records, queues nothing:
// every connection of this release's pool carries the marker writer setting, which the trigger of
// migration 20261122100122 is gated on, as the marker triggers are. An older build's outbox record
// of the same event, adding a topic, is queued, and the census counts it and reads its destination
// as a row no tuple states; the record of an event about neither a comment nor a message is not.
// The tick's catch-up (CatchUpOwedRows), which the queued event makes the tick owe
// (EventsOweObjectPass), states that destination as this release's outbox states it and empties
// the queue, after which the census reads zero and the tick owes nothing. Its controls are the
// trigger's gate taken off (this build's own record is queued) and the queue's read taken out of
// the tick's catch-up (rollPasses: the destination stays owed).
func TestThisBuildsOutboxQueuesNothingAndAnOlderOnesRecordIsQueuedForTheTick(t *testing.T) {
	rig := newRoleCensusRig(t)
	ctx := context.Background()
	rig.createIssue(t, "CENS-1", 1, "U", alice)
	routed := rig.route(t, "CENS-1", "published-route", alice)
	var message string
	created := rig.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		err := tx.QueryRow(ctx, `
			insert into messages (issue_key, author, body) values ('CENS-1', $1, 'For whoever the issue is routed to.')
			returning id::text
		`, actorJSON(t, alice)).Scan(&message)
		return model.Event{IssueKey: new("CENS-1"), Type: "message.created", Actor: alice, Payload: map[string]any{"id": message}}, err
	})
	rig.published(t)
	rig.stop()
	if queued := queuedEvents(t, rig.database, "events_published_owed"); len(queued) != 0 {
		t.Fatalf("this build's outbox queued events %v as it recorded them, want none", queued)
	}
	var stated int
	if err := rig.database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = 'published-route' and source_event = $5
	`, graphstore.KindMessage, message, graphstore.TupleDeliveredTo, graphstore.KindRole, created.ID).Scan(&stated); err != nil {
		t.Fatalf("read this build's outbox's delivery of the message: %v", err)
	}
	if stated != 1 {
		t.Fatalf("this build's outbox stated %d delivered_to of the message to the route's role, want 1, so its record proves nothing", stated)
	}
	backfillInboxOnce(t, rig.database)
	report, err := StoreCensus(ctx, rig.database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("take the store census after the backfill: %v", err)
	}
	before := censusKind(t, report, publishedDeliveriesKind).Rows

	after := newestEventID(t, rig.database)
	recordAsAnOlderOutbox(t, rig.database, created.ID, []string{contracts.AgentTopicPrefix + "ses-late"})
	recordAsAnOlderOutbox(t, rig.database, routed.ID, []string{contracts.AgentTopicPrefix + "ses-late"})
	if queued := queuedEvents(t, rig.database, "events_published_owed"); len(queued) != 1 || queued[0] != created.ID {
		t.Fatalf("the queue holds %v after an older build's outbox recorded the message's event and the route's, want the message's %d alone", queued, created.ID)
	}
	report, err = StoreCensus(ctx, rig.database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("take the store census after the older build's record: %v", err)
	}
	if kind := censusKind(t, report, publishedDeliveriesKind); kind.Unmarked != 1 || kind.RowDifferences != 1 {
		t.Errorf("after an older build's outbox recorded the message's event, published_destinations reads %d queued and %d row(s) no tuple states (%v), want 1 and 1",
			kind.Unmarked, kind.RowDifferences, kind.ReportedRows)
	}
	// The record appends no event, so the tick owes its catch-up by the queue, whatever the events
	// after its cursor are: here a route's issue.updated, which leaves no inbox row owed.
	appended := rig.route(t, "CENS-1", "published-route-again", alice)
	if !eventsOwe(t, rig.database, after, appended.ID) {
		t.Errorf("an event after the older build's record leaves the catch-up owed nothing while the queue holds the message's event, want it owed")
	}
	catchUpTheTick(t, rig.database, "after the older build's record")
	if queued := queuedEvents(t, rig.database, "events_published_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the tick's catch-up, want none", queued)
	}
	if eventsOwe(t, rig.database, after, appended.ID) {
		t.Errorf("the events after the older build's record still owe the catch-up once it has emptied the queue, want nothing owed")
	}
	report, err = StoreCensus(ctx, rig.database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("take the store census after the tick's catch-up: %v", err)
	}
	requirePublishedAgrees(t, report, before+1, "after the tick's catch-up")
}
