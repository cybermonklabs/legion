package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// The tests of the writes a build before this one makes that change no row this release marks
// (backfill_inbox_events.go) and of the people the text of a comment or an issue message that
// build wrote names (writeCommentPointers, writeMessages). Each writes rows and events as that
// build writes them - its events on a connection with no marker writer setting, which the trigger
// of migration 20261122100123 queues - and reads that the store ends holding what this release's
// writer of the same write states (the spec's R2, R4, R5 and R9), that the queue is empty, and that
// the census of user_threads, which holds every participation to a row or to the arm or the
// mention that explains it, reads no difference.

// writeAsTheReleaseBefore commits rows and the event they answer as writes.write commits them, in
// a transaction whose connection does not carry this build's marker writer setting
// (store.StoreMarkerWriterSetting), as a build of the release before this one writes.
func writeAsTheReleaseBefore(t *testing.T, writes addressingWrites, rows func(ctx context.Context, tx pgx.Tx) (model.Event, error)) model.Event {
	t.Helper()
	return writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if _, err := tx.Exec(ctx, `select set_config($1, 'off', true)`, store.StoreMarkerWriterSetting); err != nil {
			return model.Event{}, err
		}
		return rows(ctx, tx)
	})
}

// newestOf is whether the newest tuple of the relation (fromKind/fromID -kind-> toKind/toID)
// asserts it, and the event it was stated under; false and the sentinel where the store holds none.
func newestOf(t *testing.T, database *store.Store, fromKind, fromID, kind, toKind, toID string) (bool, int64) {
	t.Helper()
	var asserted bool
	var event int64
	err := database.Pool.QueryRow(context.Background(), `
		select asserted, source_event from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, fromKind, fromID, kind, toKind, toID).Scan(&asserted, &event)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, graphstore.NoEvent
	}
	if err != nil {
		t.Fatalf("read %s/%s %s %s/%s: %v", fromKind, fromID, kind, toKind, toID, err)
	}
	return asserted, event
}

// requireStands fails unless the newest tuple of the relation asserts it under event.
func requireStands(t *testing.T, database *store.Store, what string, event int64, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if asserted, under := newestOf(t, database, fromKind, fromID, kind, toKind, toID); !asserted || under != event {
		t.Errorf("%s: the newest %s/%s -%s-> %s/%s asserts %t under event %d, want it asserted under %d",
			what, fromKind, fromID, kind, toKind, toID, asserted, under, event)
	}
}

// requireEnded fails unless no tuple of the relation stands.
func requireEnded(t *testing.T, database *store.Store, what, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if asserted, under := newestOf(t, database, fromKind, fromID, kind, toKind, toID); asserted {
		t.Errorf("%s: %s/%s -%s-> %s/%s still stands, under event %d", what, fromKind, fromID, kind, toKind, toID, under)
	}
}

// requireRetracted fails unless the newest tuple of the relation retracts it under event.
func requireRetracted(t *testing.T, database *store.Store, what string, event int64, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if asserted, under := newestOf(t, database, fromKind, fromID, kind, toKind, toID); asserted || under != event {
		t.Errorf("%s: the newest %s/%s -%s-> %s/%s asserts %t under event %d, want it retracted under %d",
			what, fromKind, fromID, kind, toKind, toID, asserted, under, event)
	}
}

// requireCurrent fails unless the newest tuple of the relation asserts it, under whichever event.
func requireCurrent(t *testing.T, database *store.Store, what, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if asserted, under := newestOf(t, database, fromKind, fromID, kind, toKind, toID); !asserted {
		t.Errorf("%s: %s/%s -%s-> %s/%s does not stand (its newest tuple is under event %d), want it standing",
			what, fromKind, fromID, kind, toKind, toID, under)
	}
}

// requireMadeBy fails unless the newest tuple of the relation was made by want.
func requireMadeBy(t *testing.T, database *store.Store, what string, want model.Actor, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	var who []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select who from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, fromKind, fromID, kind, toKind, toID).Scan(&who); err != nil {
		t.Fatalf("%s: read who made the newest %s/%s -%s-> %s/%s: %v", what, fromKind, fromID, kind, toKind, toID, err)
	}
	var made model.Actor
	if err := json.Unmarshal(who, &made); err != nil {
		t.Fatalf("%s: read who made the newest %s/%s -%s-> %s/%s: %v", what, fromKind, fromID, kind, toKind, toID, err)
	}
	if !reflect.DeepEqual(made, want) {
		t.Errorf("%s: the newest %s/%s -%s-> %s/%s was made by %s, want %s",
			what, fromKind, fromID, kind, toKind, toID, who, actorJSON(t, want))
	}
}

// requireTheThreadsAgree fails unless the census of user_threads and of its version read no
// difference in either direction.
func requireTheThreadsAgree(t *testing.T, database *store.Store, what string) {
	t.Helper()
	report := storeCensusOf(t, database)
	for _, name := range []string{"user_threads", "user_threads_version"} {
		kind := censusKind(t, report, name)
		for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("%s: the census of %s reads a difference at %s: %s", what, name, difference.At, difference.Cause)
		}
		if kind.RowDifferences+kind.NodeDifferences != 0 {
			t.Errorf("%s: the census of %s reads %d row and %d node difference(s), want none", what, name, kind.RowDifferences, kind.NodeDifferences)
		}
	}
}

// catchUpTheTick runs the block-index tick's catch-up (CatchUpOwedRows) until it reads no full
// batch, failing the test, under what, on its error or on a hundredth full batch.
func catchUpTheTick(t *testing.T, database *store.Store, what string) {
	t.Helper()
	for round := 1; ; round++ {
		caught, err := CatchUpOwedRows(context.Background(), database)
		if err != nil {
			t.Fatalf("%s: catch the inbox's rows up: %v", what, err)
		}
		if !caught.More {
			return
		}
		if round == 100 {
			t.Fatalf("%s: the catch-up still reads a full batch after %d rounds, want it done", what, round)
		}
	}
}

// R2: an issue's reopen restores the asks its close withdrew, and each restored ask "reaches
// whoever it waits on as it is restored" (inboxwrite.Restored): this release's reopen moves its
// delivered_to and its participates_in from whom it waited on before the close to whom it waits on
// now. R9: what a build before this one writes takes its tuples once, from the backfill or the
// block-index tick's catch-up.
//
// alice asks on CENS-1, which bob holds, and the backfill states the ask reaching bob. A build of
// the release before then closes the issue, handing it to dave as it closes it - its close
// withdraws the ask and moves nothing - and reopens it, which restores the ask with an ask.opened.
// The trigger queues that event, which makes the tick owe its catch-up; the catch-up moves the ask
// to dave under the restore's event and by its actor, carol: dave holds the delivered_to and takes
// part, and bob, who took no other part in the ask, holds neither. The queue empties and the census
// of user_threads reads no difference. Its control is the queue's pass taken out of the passes
// (inboxPasses): the ask stays bob's.
func TestTheTickMovesAnAskAnOlderBuildsReopenRestoresToWhoeverItWaitsOn(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	if _, err := writes.database.Pool.Exec(ctx, `update issues set assignee = 'bob' where key = 'CENS-1'`); err != nil {
		t.Fatalf("give CENS-1 to bob: %v", err)
	}
	asked := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Ready?')
		returning id::text, created_at`, actorJSON(t, alice))
	backfillInboxOnce(t, writes.database)
	if asserted, _ := newestOf(t, writes.database, graphstore.KindAsk, asked.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, "bob"); !asserted {
		t.Fatalf("after the backfill alice's ask holds no delivered_to to bob, who holds its issue: this test proves nothing")
	}

	writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if _, err := tx.Exec(ctx, `update issues set assignee = 'dave', status = 'done', closed_at = now() where key = 'CENS-1'`); err != nil {
			return model.Event{}, err
		}
		_, err := tx.Exec(ctx, `update asks set state = 'resolved', resolution = '{"kind": "withdrawn"}' where id = $1::uuid`, asked.id)
		return model.Event{IssueKey: new("CENS-1"), Type: "issue.closed", Actor: carol, Payload: model.IssueEventPayload{
			Issue: model.Issue{Key: "CENS-1", Status: "done"}, Changed: []string{"status", "assignee", "closed_at"},
		}}, err
	})
	after := newestEventID(t, writes.database)
	reopened := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if _, err := tx.Exec(ctx, `update issues set status = 'todo', closed_at = null where key = 'CENS-1'`); err != nil {
			return model.Event{}, err
		}
		_, err := tx.Exec(ctx, `update asks set state = 'open', resolution = null where id = $1::uuid`, asked.id)
		return model.Event{IssueKey: new("CENS-1"), Type: "ask.opened", Actor: carol,
			Payload: model.AskEventPayload{Ask: model.Ask{ID: asked.id, IssueKey: new("CENS-1"), Author: alice, CreatedAt: asked.at}}}, err
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != reopened.ID {
		t.Fatalf("the queue holds %v after the older build's close and reopen, want the restore's ask.opened %d alone", queued, reopened.ID)
	}
	if !eventsOwe(t, writes.database, after, reopened.ID) {
		t.Errorf("the older build's restore of an ask owes the tick's catch-up nothing, want it owed")
	}

	catchUpTheTick(t, writes.database, "the restore's move")
	requireStands(t, writes.database, "the restore's move", reopened.ID,
		graphstore.KindAsk, asked.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, "dave")
	requireStands(t, writes.database, "the restore's move", reopened.ID,
		graphstore.KindPerson, "dave", graphstore.TupleParticipatesIn, graphstore.KindAsk, asked.id)
	requireEnded(t, writes.database, "the restore's move", graphstore.KindAsk, asked.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, "bob")
	requireEnded(t, writes.database, "the restore's move", graphstore.KindPerson, "bob", graphstore.TupleParticipatesIn, graphstore.KindAsk, asked.id)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the tick's catch-up, want none", queued)
	}
	if eventsOwe(t, writes.database, after, reopened.ID) {
		t.Errorf("the restore still owes the tick's catch-up once the queue is empty, want nothing owed")
	}
	requireTheThreadsAgree(t, writes.database, "after the tick's catch-up of the restore")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R2: "the list follows the assignment" - each move of an ask states its arm under its own event,
// made by its own writer, and a person who moves an ask onto themselves takes part in it and draws
// no delivery (inboxwrite.OwnMove). A build of the release before moves alice's ask twice before
// any pass reads either move: dave gives CENS-1 to carol, then bob gives it to dave. The ask waits
// on dave, moved there by bob, so this release's writer would have stated dave's delivered_to and
// participates_in under bob's move. The pass reads who each ask waits on as it runs, after both
// moves, so it states the ask's arm by the later move alone (laterMoveOf): dave holds the
// delivered_to under bob's event, and bob, whom the ask waited on before, holds neither tuple. Its
// control is the later moves left out of the test (laterMoveOf answering the stored marker alone):
// the first move, dave's, states dave's participation as his own move onto himself, with no
// delivery, and the second appends nothing.
func TestTheLastOfAnOlderBuildsMovesOfAnAskStatesItsArm(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	if _, err := writes.database.Pool.Exec(ctx, `update issues set assignee = 'bob' where key = 'CENS-1'`); err != nil {
		t.Fatalf("give CENS-1 to bob: %v", err)
	}
	asked := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Ready?')
		returning id::text, created_at`, actorJSON(t, alice))
	backfillInboxOnce(t, writes.database)

	reassign := func(to string, by model.Actor) model.Event {
		return writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `update issues set assignee = $1 where key = 'CENS-1'`, to)
			return model.Event{IssueKey: new("CENS-1"), Type: "issue.updated", Actor: by, Payload: model.IssueEventPayload{
				Issue: model.Issue{Key: "CENS-1", Status: "triage"}, Changed: []string{"assignee"},
			}}, err
		})
	}
	first := reassign("carol", dave)
	second := reassign("dave", bob)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 2 || queued[0] != first.ID || queued[1] != second.ID {
		t.Fatalf("the queue holds %v after the older build's two changes of CENS-1's assignee, want %d and %d", queued, first.ID, second.ID)
	}

	backfillInboxOnce(t, writes.database)
	requireStands(t, writes.database, "the later move", second.ID,
		graphstore.KindAsk, asked.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, "dave")
	requireStands(t, writes.database, "the later move", second.ID,
		graphstore.KindPerson, "dave", graphstore.TupleParticipatesIn, graphstore.KindAsk, asked.id)
	requireEnded(t, writes.database, "the later move", graphstore.KindAsk, asked.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, "bob")
	requireEnded(t, writes.database, "the later move", graphstore.KindPerson, "bob", graphstore.TupleParticipatesIn, graphstore.KindAsk, asked.id)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the two moves")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R2: each move of an ask states its arm "made by its own writer" (inboxwrite.QueueMoveWaiting's
// Move.who). A build of the release before hands two of alice's asks on before any pass reads
// either hand-on: bob, proven by his browser's cookie and writing from his laptop, hands the first
// to carol, and then bob, under a credential that proves nobody and names no origin, hands the
// second to dave. One batch of the queue reads both events (writeQueuedInboxEvents), and each
// hand-on's tuples carry its own event's actor as their maker: carol's delivered_to and
// participates_in the proven bob from his laptop, and dave's the bare bob, with neither the mark
// nor the origin of the event the batch read before it.
func TestEachOfAnOlderBuildsMovesInOneBatchIsMadeByItsOwnActor(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	if _, err := writes.database.Pool.Exec(ctx, `update issues set assignee = 'bob' where key = 'CENS-1'`); err != nil {
		t.Fatalf("give CENS-1 to bob: %v", err)
	}
	ask := func(question string) olderWrite {
		return writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
			insert into asks (issue_key, author, question) values ('CENS-1', $1, $2)
			returning id::text, created_at`, actorJSON(t, alice), question)
	}
	first, second := ask("Ready?"), ask("Shipped?")
	backfillInboxOnce(t, writes.database)

	handOn := func(asked olderWrite, to string, by model.Actor) model.Event {
		return writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			var at time.Time
			err := tx.QueryRow(ctx, `
				update asks set asked_of = $2, asked_of_by = $3, asked_of_at = now() where id = $1::uuid
				returning asked_of_at`, asked.id, to, by.ID).Scan(&at)
			return model.Event{IssueKey: new("CENS-1"), Type: "ask.asked_of", Actor: by, Payload: model.AskEventPayload{Ask: model.Ask{
				ID: asked.id, IssueKey: new("CENS-1"), Author: alice, CreatedAt: asked.at, AskedOf: &to, AskedOfBy: &by.ID, AskedOfAt: &at,
			}}}, err
		})
	}
	fromLaptop := model.Actor{Kind: "user", ID: "bob", Origin: &model.ActorOrigin{Host: "laptop"}, Proven: true}
	toCarol := handOn(first, "carol", fromLaptop)
	toDave := handOn(second, "dave", bob)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 2 || queued[0] != toCarol.ID || queued[1] != toDave.ID {
		t.Fatalf("the queue holds %v after the older build's two hand-ons, want %d and %d", queued, toCarol.ID, toDave.ID)
	}

	backfillInboxOnce(t, writes.database)
	for _, moved := range []struct {
		ask, to string
		event   int64
		by      model.Actor
	}{{first.id, "carol", toCarol.ID, fromLaptop}, {second.id, "dave", toDave.ID, bob}} {
		what := "the hand-on to " + moved.to
		requireStands(t, writes.database, what, moved.event,
			graphstore.KindAsk, moved.ask, graphstore.TupleDeliveredTo, graphstore.KindPerson, moved.to)
		requireMadeBy(t, writes.database, what, moved.by,
			graphstore.KindAsk, moved.ask, graphstore.TupleDeliveredTo, graphstore.KindPerson, moved.to)
		requireStands(t, writes.database, what, moved.event,
			graphstore.KindPerson, moved.to, graphstore.TupleParticipatesIn, graphstore.KindAsk, moved.ask)
		requireMadeBy(t, writes.database, what, moved.by,
			graphstore.KindPerson, moved.to, graphstore.TupleParticipatesIn, graphstore.KindAsk, moved.ask)
	}
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the two hand-ons")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R5: a mention "runs from the comment, issue message or document block that holds it to the
// identity it points at", and R4: participation "holds every thread they were mentioned or
// addressed in without writing in it". This release's edit of a comment states the mentions tuple
// to each identity its new text names and each newly named person's participates_in in the
// comment's thread, under the edit's event, and delivers nothing (api's editComment).
//
// alice's comment is stated by the backfill. A build of the release before then edits it to name
// dave and an agent's session; that build states no mention of either and stores no row of dave's,
// and the comment's marker marks its pointers, which no edit moves, so only the edit's event, which
// the trigger queues, tells the backfill. The backfill states the comment's mentions tuples to dave
// and to the session and dave's participation in the comment's thread under the edit's event, no
// delivered_to to either, and no participation of alice's from the edit, who wrote it. A second run
// appends nothing. Its control is the queue's edits left out (writeQueuedInboxEvents): the comment
// names nobody.
func TestTheBackfillStatesThePeopleAnOlderBuildsEditOfACommentNames(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	backfillInboxOnce(t, writes.database)

	const body = "Is the copy final? Over to dispatch://person/" + daveAddress + " for it, with dispatch://agent/" + mentionedSession + "."
	edited := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		_, err := tx.Exec(ctx, `update comments set body = $2, edited_at = now() where id = $1::uuid`, thread.id, body)
		return model.Event{IssueKey: new("CENS-1"), Type: "comment.edited", Actor: alice,
			Payload: map[string]any{"id": thread.id, "body": body}}, err
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != edited.ID {
		t.Fatalf("the queue holds %v after the older build's edit, want its comment.edited %d alone", queued, edited.ID)
	}

	backfillInboxOnce(t, writes.database)
	for _, named := range []struct{ kind, id string }{{graphstore.KindPerson, daveAddress}, {graphstore.KindSession, mentionedSession}} {
		requireStands(t, writes.database, "the edit's mention", edited.ID,
			graphstore.KindComment, thread.id, graphstore.TupleMentions, named.kind, named.id)
		if asserted, under := newestOf(t, writes.database, graphstore.KindComment, thread.id, graphstore.TupleDeliveredTo, named.kind, named.id); asserted {
			t.Errorf("the edit delivered comment %s to %s %s under event %d, want no delivery: an edit delivers nothing", thread.id, named.kind, named.id, under)
		}
	}
	requireStands(t, writes.database, "the edit's mention", edited.ID,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindComment, thread.id)
	var aliceUnderEdit int
	if err := writes.database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = 'alice' and kind = $2 and source_event = $3
	`, graphstore.KindPerson, graphstore.TupleParticipatesIn, edited.ID).Scan(&aliceUnderEdit); err != nil {
		t.Fatalf("read alice's participation under the edit: %v", err)
	}
	if aliceUnderEdit != 0 {
		t.Errorf("the edit stated %d participates_in of alice, who wrote it, want none", aliceUnderEdit)
	}
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the edit")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R4: participation "holds every thread they were mentioned or addressed in without writing in
// it", whichever pass states the mention first. A re-index of a comment's text at the sentinel -
// the scope re-index a text that writes a `#id` owes when its document's question blocks move
// (refs.ReindexScopeSource) - states the mention of each person the text names that the store
// holds none of, and no participation, since no event records it.
//
// alice's comment is stated by the backfill. A build of the release before then edits it to name
// dave and to write a `#id`; the trigger queues the edit's event. The scope re-index of the comment
// runs before any pass reads that event and states dave's mention at the sentinel. The backfill's
// pass over the queue then finds dave's mention standing and dave taking no part in the comment's
// thread, and states his participation in it under the edit's event, beside his mention under that
// event, which explains it to the census of user_threads (mentioned_alone). That census reads no
// difference, the queue empties, and a second run appends nothing. Its control is the pass
// deciding on the standing mention alone (writeOlderEdit): it states nothing of dave, who never
// takes part.
func TestAPersonAnOlderBuildsEditNamesTakesPartThoughAReindexStatedTheMentionFirst(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	backfillInboxOnce(t, writes.database)

	const body = "Is the copy final? Over to dispatch://person/" + daveAddress + " for it, as #q1 asks."
	edited := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		_, err := tx.Exec(ctx, `update comments set body = $2, edited_at = now() where id = $1::uuid`, thread.id, body)
		return model.Event{IssueKey: new("CENS-1"), Type: "comment.edited", Actor: alice,
			Payload: map[string]any{"id": thread.id, "body": body}}, err
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != edited.ID {
		t.Fatalf("the queue holds %v after the older build's edit, want its comment.edited %d alone", queued, edited.ID)
	}
	if indexed, err := refs.ReindexScopeSource(ctx, writes.database.Pool, graphstore.KindComment, thread.id, writersSizingURL); err != nil || !indexed {
		t.Fatalf("the scope re-index of the edited comment = %v (%v), want it re-indexed", indexed, err)
	}
	requireStands(t, writes.database, "the scope re-index", graphstore.NoEvent,
		graphstore.KindComment, thread.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	if asserted, under := newestOf(t, writes.database, graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindComment, thread.id); asserted {
		t.Fatalf("dave takes part in the comment's thread under event %d before the backfill reads the edit: this test proves nothing", under)
	}

	backfillInboxOnce(t, writes.database)
	requireStands(t, writes.database, "the edit's participation", edited.ID,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindComment, thread.id)
	requireStands(t, writes.database, "the edit's mention", edited.ID,
		graphstore.KindComment, thread.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the scope re-index and the backfill of the edit")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R5 and R4 of an issue message, as of a comment: this release's edit of an issue message states
// the mentions tuple to each identity its new text names and each newly named person's
// participates_in in the message's thread, under the edit's event, and delivers nothing (api's
// editMessage, editMentionParticipation).
//
// alice's issue message and bob's reply to it, written as a build of the release before writes
// them, are stated by the backfill, which names each one's node on its row (writeMessages): the
// message's marker, which no edit moves. That build then edits bob's reply to name dave and an
// agent's session, and states no mention of either; only the edit's event, which the trigger
// queues, tells the backfill. The backfill states the reply's mentions tuples to dave and to the
// session under the edit's event, dave's participation in the reply's thread, whose root is
// alice's message, and in no thread whose root is the reply, and no delivered_to to either. The
// queue empties, the census of user_threads reads no difference, and a second run appends nothing.
// Its control is the queue's message edits left out of the pass (olderEditedItems holding
// comment.edited alone): the reply names nobody and dave takes part in nothing.
func TestTheBackfillStatesThePeopleAnOlderBuildsEditOfAnIssueMessageNames(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "message.answered", bob, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CENS-1', $1, 'Nearly.', $2::uuid)
		returning id::text, created_at`, actorJSON(t, bob), root.id)
	backfillInboxOnce(t, writes.database)
	var named bool
	if err := writes.database.Pool.QueryRow(ctx, `select node_kind is not null from messages where id = $1::uuid`, reply.id).Scan(&named); err != nil || !named {
		t.Fatalf("after the backfill bob's reply %s names its node %t (%v), want it named: this test proves nothing", reply.id, named, err)
	}

	const body = "Nearly. Over to dispatch://person/" + daveAddress + " for it, with dispatch://agent/" + mentionedSession + "."
	edited := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		_, err := tx.Exec(ctx, `update messages set body = $2, edited_at = now() where id = $1::uuid`, reply.id, body)
		return model.Event{IssueKey: new("CENS-1"), Type: "message.edited", Actor: bob,
			Payload: map[string]any{"id": reply.id, "body": body}}, err
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != edited.ID {
		t.Fatalf("the queue holds %v after the older build's edit, want its message.edited %d alone", queued, edited.ID)
	}

	backfillInboxOnce(t, writes.database)
	for _, mentioned := range []struct{ kind, id string }{{graphstore.KindPerson, daveAddress}, {graphstore.KindSession, mentionedSession}} {
		requireStands(t, writes.database, "the edit's mention", edited.ID,
			graphstore.KindMessage, reply.id, graphstore.TupleMentions, mentioned.kind, mentioned.id)
		if asserted, under := newestOf(t, writes.database, graphstore.KindMessage, reply.id, graphstore.TupleDeliveredTo, mentioned.kind, mentioned.id); asserted {
			t.Errorf("the edit delivered message %s to %s %s under event %d, want no delivery: an edit delivers nothing", reply.id, mentioned.kind, mentioned.id, under)
		}
	}
	requireStands(t, writes.database, "the edit's participation", edited.ID,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindMessage, root.id)
	requireEnded(t, writes.database, "the edit's participation", graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindMessage, reply.id)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the message's edit")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R5: a mention follows the text that holds it, and R4: participation outlives the mention that
// brought it. This release's edit of a comment retracts the mention of each identity its new text
// no longer names, and its delete, which leaves the comment no text, the mention of each identity
// its text named, each under the write's own event; neither retracts the mention of a delivery
// route of the comment (its comment_mentions rows), delivers anything, or ends anybody's part in
// the thread (api's editComment and deleteComment, refs' unnamedMentions).
//
// alice's comment, written as a build of the release before writes it, names dave and the reviewer
// role and routes its delivery to an agent's session; the backfill states the three mentions, and
// dave's delivery and part in the comment's thread, under the comment's event. That build then
// edits the comment to name dave alone, and then deletes it, each event carrying the comment as the
// write left it - the delete's with an empty body - and the trigger queues each. Once the edit is
// caught up, the newest mention of the role is a retraction under the edit's event, and dave's and
// the session's stand; once the delete is, the newest mention of dave is a retraction under the
// delete's event, the session's route still stands, and dave keeps the comment's delivery and his
// part in its thread. The queue empties, the census of user_threads reads no difference, and a
// second run appends nothing. Its control is writeOlderEdit appending no retraction (its
// refs.ItemUnnamedMentions left out): the role's mention stays current after the edit, and dave's
// after the delete.
func TestTheCatchUpRetractsWhatAnOlderBuildsEditAndDeleteOfACommentTakeOut(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	const named = "Over to dispatch://person/" + daveAddress + " and dispatch://role/reviewer for the copy."
	routes := []model.Mention{{Target: "session:" + routedSession, Delivery: "btw"}}
	var comment olderWrite
	created := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if err := tx.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ('CENS-1', $1, $2)
			returning id::text, created_at`, actorJSON(t, alice), named).Scan(&comment.id, &comment.at); err != nil {
			return model.Event{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into comment_mentions (comment_id, target, delivery) values ($1::uuid, $2, $3)
		`, comment.id, routes[0].Target, routes[0].Delivery); err != nil {
			return model.Event{}, err
		}
		return model.Event{IssueKey: new("CENS-1"), Type: "comment.created", Actor: alice, Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: comment.id, IssueKey: new("CENS-1"), Author: alice, Body: named, CreatedAt: comment.at, Mentions: routes,
		}}}, nil
	})
	comment.event = created.ID
	backfillInboxOnce(t, writes.database)
	for _, identity := range []struct{ kind, id string }{{graphstore.KindPerson, daveAddress}, {graphstore.KindRole, "reviewer"}} {
		requireStands(t, writes.database, "the backfill of the comment", comment.event,
			graphstore.KindComment, comment.id, graphstore.TupleMentions, identity.kind, identity.id)
	}
	requireCurrent(t, writes.database, "the backfill of the comment's route",
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindSession, routedSession)

	const kept = "Over to dispatch://person/" + daveAddress + " for the copy."
	edited := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		var editedAt time.Time
		if err := tx.QueryRow(ctx, `
			update comments set body = $2, edited_at = now() where id = $1::uuid returning edited_at
		`, comment.id, kept).Scan(&editedAt); err != nil {
			return model.Event{}, err
		}
		stamp := editedAt.UTC().Format(time.RFC3339Nano)
		return model.Event{IssueKey: new("CENS-1"), Type: "comment.edited", Actor: alice, Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: comment.id, IssueKey: new("CENS-1"), Author: alice, Body: kept, EditedAt: &stamp, CreatedAt: comment.at, Mentions: routes,
		}}}, nil
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != edited.ID {
		t.Fatalf("the queue holds %v after the older build's edit, want its comment.edited %d alone", queued, edited.ID)
	}
	backfillInboxOnce(t, writes.database)
	requireRetracted(t, writes.database, "the edit's retraction", edited.ID,
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindRole, "reviewer")
	requireStands(t, writes.database, "the edit, which names dave still", comment.event,
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	requireCurrent(t, writes.database, "the comment's route after the edit",
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindSession, routedSession)

	deleted := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		var deletedAt time.Time
		if err := tx.QueryRow(ctx, `
			update comments set body = '', deleted_at = now(), deleted_by = $2 where id = $1::uuid returning deleted_at
		`, comment.id, actorJSON(t, alice)).Scan(&deletedAt); err != nil {
			return model.Event{}, err
		}
		deletedBy := alice
		return model.Event{IssueKey: new("CENS-1"), Type: "comment.deleted", Actor: alice, Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: comment.id, IssueKey: new("CENS-1"), Author: alice, CreatedAt: comment.at, DeletedAt: &deletedAt, DeletedBy: &deletedBy, Mentions: routes,
		}}}, nil
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != deleted.ID {
		t.Fatalf("the queue holds %v after the older build's delete, want its comment.deleted %d alone", queued, deleted.ID)
	}
	backfillInboxOnce(t, writes.database)
	requireRetracted(t, writes.database, "the delete's retraction", deleted.ID,
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	requireRetracted(t, writes.database, "the edit's retraction, which the delete leaves", edited.ID,
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindRole, "reviewer")
	requireCurrent(t, writes.database, "the comment's route after the delete",
		graphstore.KindComment, comment.id, graphstore.TupleMentions, graphstore.KindSession, routedSession)
	requireStands(t, writes.database, "dave's delivery after the delete", comment.event,
		graphstore.KindComment, comment.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
	requireStands(t, writes.database, "dave's part in the thread after the delete", comment.event,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindComment, comment.id)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the edit and the delete")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R5 and R4 of an issue message, as of a comment: this release's edit of an issue message retracts
// the mention of each identity its new text no longer names, and its delete, which leaves the
// message no text, the mention of each identity its text named, each under the write's own event,
// with no delivery and no participation ended (api's editMessage and deleteMessage).
//
// bob's issue message, written as a build of the release before writes it, names dave and an
// agent's session; the backfill states both mentions, and dave's delivery and part in the message's
// thread, under the message's event. That build then edits the message to name dave alone, and then
// deletes it, each event carrying the message as the write left it - the delete's with an empty body
// - and the trigger queues each. Once the edit is caught up, the newest mention of the session is a
// retraction under the edit's event and dave's stands; once the delete is, the newest mention of
// dave is a retraction under the delete's event, and dave keeps the message's delivery and his part
// in its thread. The queue empties, the census of user_threads reads no difference, and a second run
// appends nothing. Its control is writeOlderEdit appending no retraction (its
// refs.ItemUnnamedMentions left out): the session's mention stays current after the edit, and
// dave's after the delete.
func TestTheCatchUpRetractsWhatAnOlderBuildsEditAndDeleteOfAnIssueMessageTakeOut(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	const named = "For dispatch://person/" + daveAddress + " to read, with dispatch://agent/" + mentionedSession + "."
	message := writeAsAnOlderBuild(t, writes, "message.created", bob, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, $2)
		returning id::text, created_at`, actorJSON(t, bob), named)
	backfillInboxOnce(t, writes.database)
	for _, identity := range []struct{ kind, id string }{{graphstore.KindPerson, daveAddress}, {graphstore.KindSession, mentionedSession}} {
		requireStands(t, writes.database, "the backfill of the message", message.event,
			graphstore.KindMessage, message.id, graphstore.TupleMentions, identity.kind, identity.id)
	}

	const kept = "For dispatch://person/" + daveAddress + " to read."
	edited := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		var editedAt time.Time
		if err := tx.QueryRow(ctx, `
			update messages set body = $2, edited_at = now() where id = $1::uuid returning edited_at
		`, message.id, kept).Scan(&editedAt); err != nil {
			return model.Event{}, err
		}
		return model.Event{IssueKey: new("CENS-1"), Type: "message.edited", Actor: bob, Payload: model.MessageEventPayload{Message: model.Message{
			ID: message.id, IssueKey: new("CENS-1"), Author: bob, Body: kept, CreatedAt: message.at, EditedAt: &editedAt,
		}}}, nil
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != edited.ID {
		t.Fatalf("the queue holds %v after the older build's edit, want its message.edited %d alone", queued, edited.ID)
	}
	backfillInboxOnce(t, writes.database)
	requireRetracted(t, writes.database, "the edit's retraction", edited.ID,
		graphstore.KindMessage, message.id, graphstore.TupleMentions, graphstore.KindSession, mentionedSession)
	requireStands(t, writes.database, "the edit, which names dave still", message.event,
		graphstore.KindMessage, message.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)

	deleted := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		var deletedAt time.Time
		if err := tx.QueryRow(ctx, `
			update messages set body = '', deleted_at = now(), deleted_by = $2 where id = $1::uuid returning deleted_at
		`, message.id, actorJSON(t, bob)).Scan(&deletedAt); err != nil {
			return model.Event{}, err
		}
		deletedBy := bob
		return model.Event{IssueKey: new("CENS-1"), Type: "message.deleted", Actor: bob, Payload: model.MessageEventPayload{Message: model.Message{
			ID: message.id, IssueKey: new("CENS-1"), Author: bob, CreatedAt: message.at, DeletedAt: &deletedAt, DeletedBy: &deletedBy,
		}}}, nil
	})
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != deleted.ID {
		t.Fatalf("the queue holds %v after the older build's delete, want its message.deleted %d alone", queued, deleted.ID)
	}
	backfillInboxOnce(t, writes.database)
	requireRetracted(t, writes.database, "the delete's retraction", deleted.ID,
		graphstore.KindMessage, message.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	requireRetracted(t, writes.database, "the edit's retraction, which the delete leaves", edited.ID,
		graphstore.KindMessage, message.id, graphstore.TupleMentions, graphstore.KindSession, mentionedSession)
	requireStands(t, writes.database, "dave's delivery after the delete", message.event,
		graphstore.KindMessage, message.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
	requireStands(t, writes.database, "dave's part in the thread after the delete", message.event,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindMessage, message.id)
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("the queue holds %v after the backfill, want none", queued)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the message's edit and delete")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R4: a person a thread's text names takes part in it, and R2: an ask that falls through to its
// project's default reaches whoever that default is. This release's edit of a comment in the thread
// of such an ask takes the project's owner lock shared before it reads who takes part in the thread
// (api's editMentionParticipation, inboxwrite.HoldAskDefault), and a move of the default takes it
// exclusive before it reads anything (putProjectDefaultAssignee): so the move reads the
// participation the edit states, or the edit runs once the move has committed. The catch-up of an
// older build's edit of such a comment takes the same lock, before any asks row
// (writeQueuedInboxEvents' step 2).
//
// carol's question on CENS-1, which nobody holds, falls through to project CENS's default, dave, and
// bob writes a comment in its thread, both as a build of the release before writes them, and the
// backfill states them; that build then edits bob's comment to name dave, and the trigger queues the
// edit. The catch-up of the edit and this release's move of the default to erin race, the
// interleaving forced rather than hoped for, as api's
// TestAMoveOfTheDefaultTakesTurnsWithEveryWriteThatDecidesWhoAnAskWaitsOn forces its own: each runs on
// a connection of its own, so the backend it runs on is known; a transaction holds what the first
// write needs after it has read what it decides on and before it commits, and the second is then
// started and has to queue behind the first. Either way round, dave, whom the edit names, takes part
// in the ask's thread from the edit, and the ask stands delivered to erin and not to dave. A catch-up
// that took no hold on the project lets the second write answer while the first had not committed,
// and the test fails there: the move, run in the catch-up's middle, would end dave's part in the
// thread with a retraction newer than the edit's participation, which it could not read yet.
func TestAMoveOfTheDefaultTakesTurnsWithTheCatchUpOfAnOlderBuildsEditInItsAsksThread(t *testing.T) {
	ctx := context.Background()
	const erinAddress = "erin@example.com"
	type raced struct {
		writes       addressingWrites
		ask, comment olderWrite
		edited       model.Event
	}
	// fallingThrough writes, on a database of its own, carol's question and bob's comment in its
	// thread, backfills them, and writes that build's edit of the comment naming dave.
	fallingThrough := func(t *testing.T) raced {
		t.Helper()
		race := raced{writes: newAddressingWrites(t)}
		if _, err := race.writes.database.Pool.Exec(ctx, `update projects set default_assignee = $1 where key = 'CENS'`, daveAddress); err != nil {
			t.Fatalf("make dave project CENS's default: %v", err)
		}
		race.writes.createIssue(t, "CENS-1", 1, "U", alice)
		race.ask = writeAsAnOlderBuild(t, race.writes, "ask.opened", carol, `
			insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Is the copy final?')
			returning id::text, created_at`, actorJSON(t, carol))
		race.comment = writeAsAnOlderBuild(t, race.writes, "comment.created", bob, `
			insert into comments (issue_key, author, body, ask_id) values ('CENS-1', $1, 'Checking.', $2::uuid)
			returning id::text, created_at`, actorJSON(t, bob), race.ask.id)
		backfillInboxOnce(t, race.writes.database)
		requireStands(t, race.writes.database, "the backfill of carol's question", race.ask.event,
			graphstore.KindAsk, race.ask.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
		const body = "Checking with dispatch://person/" + daveAddress + "."
		race.edited = writeAsTheReleaseBefore(t, race.writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			var editedAt time.Time
			if err := tx.QueryRow(ctx, `
				update comments set body = $2, edited_at = now() where id = $1::uuid returning edited_at
			`, race.comment.id, body).Scan(&editedAt); err != nil {
				return model.Event{}, err
			}
			stamp := editedAt.UTC().Format(time.RFC3339Nano)
			return model.Event{IssueKey: new("CENS-1"), Type: "comment.edited", Actor: bob, Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: race.comment.id, IssueKey: new("CENS-1"), AskID: new(race.ask.id), Author: bob, Body: body, EditedAt: &stamp,
				CreatedAt: race.comment.at,
			}}}, nil
		})
		if queued := queuedEvents(t, race.writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != race.edited.ID {
			t.Fatalf("the queue holds %v after the older build's edit, want its comment.edited %d alone", queued, race.edited.ID)
		}
		return race
	}
	// catchUp starts the pass over the queue alone on a connection of its own (startRun), and
	// answers its backend and the error it ends with.
	catchUp := func(t *testing.T, database *store.Store) func() (uint32, <-chan error) {
		pass := inboxPassOf(t, inboxEventsKind)
		return func() (uint32, <-chan error) {
			pid, answered := startRun(t, database, func(ctx context.Context, database *store.Store) (BackfillReport, error) {
				done, err := runInboxPass(ctx, database, pass, &storeGrowth{})
				return BackfillReport{Kinds: []KindBackfill{done}}, err
			})
			ended := make(chan error, 1)
			go func() { ended <- (<-answered).err }()
			return pid, ended
		}
	}
	// moveToErin starts this release's move of project CENS's default to erin on a connection of its
	// own (moveTheDefault), and answers its backend and the error it ends with.
	moveToErin := func(t *testing.T, writes addressingWrites) func() (uint32, <-chan error) {
		return func() (uint32, <-chan error) {
			mover := storetest.SingleConnection(t, writes.database)
			var pid uint32
			if err := mover.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatalf("read the move's backend: %v", err)
			}
			ended := make(chan error, 1)
			go func() { ended <- moveTheDefault(context.Background(), mover, writes.broker, erinAddress, alice) }()
			return pid, ended
		}
	}
	// takeTurns starts first, waits for it to queue behind holder, starts second, waits for that to
	// queue too - behind holder, or behind first, which holds what second must take turns at - lets
	// holder go, and waits for both to end. early says what second answering before holder went
	// means. holder goes however takeTurns ends, so a write still queued behind it when a check fails
	// finishes before its connection's cleanup closes it.
	takeTurns := func(t *testing.T, holder pgx.Tx, first, second func() (uint32, <-chan error), early string) {
		t.Helper()
		defer func() { _ = holder.Rollback(context.Background()) }()
		firstPid, firstEnded := first()
		if answer, answered := storetest.WaitForQueuedBehind(t, holder, firstPid, firstEnded); answered {
			t.Fatalf("the first write answered (%v) before it reached the hold", answer)
		}
		secondPid, secondEnded := second()
		if answer, answered := storetest.WaitForQueuedBehind(t, holder, secondPid, secondEnded); answered {
			t.Fatalf("%s answered (%v)", early, answer)
		}
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("let the first write go: %v", err)
		}
		for index, ended := range []<-chan error{firstEnded, secondEnded} {
			select {
			case err := <-ended:
				if err != nil {
					t.Fatalf("write %d failed: %v", index+1, err)
				}
			case <-time.After(lockstep.Wait):
				t.Fatalf("write %d did not answer within %s of the hold going", index+1, lockstep.Wait)
			}
		}
	}
	begin := func(t *testing.T, database *store.Store) pgx.Tx {
		t.Helper()
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder: %v", err)
		}
		t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
		return holder
	}
	// requireTheEditsPartStands fails unless dave takes part in the ask's thread from the edit,
	// whose mention of him stands, and the ask stands delivered to erin, whom the move made its
	// default, and not to dave.
	requireTheEditsPartStands := func(t *testing.T, race raced) {
		t.Helper()
		database := race.writes.database
		requireStands(t, database, "the edit's mention", race.edited.ID,
			graphstore.KindComment, race.comment.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
		requireStands(t, database, "dave's part in the ask's thread, which the edit states", race.edited.ID,
			graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindAsk, race.ask.id)
		var moved int64
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(max(id), 0) from events where type = 'settings.project_default_assignee.updated'
		`).Scan(&moved); err != nil || moved == 0 {
			t.Fatalf("read the move's event: %d (%v)", moved, err)
		}
		requireStands(t, database, "the move's delivery", moved,
			graphstore.KindAsk, race.ask.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, erinAddress)
		requireEnded(t, database, "the move's delivery", graphstore.KindAsk, race.ask.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
		if queued := queuedEvents(t, database, "events_inbox_owed"); len(queued) != 0 {
			t.Errorf("the queue holds %v after the catch-up, want none", queued)
		}
	}

	// The catch-up stops at its last statement, the delete of the queue rows it took, after it
	// stated dave's mention and his part in the ask's thread: the holder takes the queue in a mode
	// that admits the batch's `for no key update` of its rows and not their delete.
	t.Run("the catch-up of the edit, then a move of the default", func(t *testing.T) {
		race := fallingThrough(t)
		holder := begin(t, race.writes.database)
		if _, err := holder.Exec(ctx, `lock table events_inbox_owed in share mode`); err != nil {
			t.Fatalf("hold the deletes of the queue: %v", err)
		}
		takeTurns(t, holder, catchUp(t, race.writes.database), moveToErin(t, race.writes),
			"the move of the default, sent while the catch-up of the edit, which stated dave's part in the ask's thread, had not committed,")
		requireTheEditsPartStands(t, race)
	})

	// The move stops at the ask's row, which it locks once it holds the project's lock exclusive and
	// before it reads who takes part in the ask's thread (inboxwrite.QueueMoveWaiting); the catch-up
	// then waits for it at that lock, before it reads who takes part, and states dave's part once the
	// move has ended the part the default gave him.
	t.Run("a move of the default, then the catch-up of the edit", func(t *testing.T) {
		race := fallingThrough(t)
		holder := begin(t, race.writes.database)
		if _, err := holder.Exec(ctx, `select 1 from asks where id = $1::uuid for no key update`, race.ask.id); err != nil {
			t.Fatalf("hold the ask's row: %v", err)
		}
		takeTurns(t, holder, moveToErin(t, race.writes), catchUp(t, race.writes.database),
			"the catch-up of the edit, sent while the move of the default its ask falls through to had not committed,")
		requireTheEditsPartStands(t, race)
	})
}

// moveTheDefault moves project CENS's default assignee to login, made by by, as this release's
// putProjectDefaultAssignee moves it, in one transaction on database: the project's owner lock,
// exclusive, and the default it moves from with the transaction's moment, read under it
// (store.LockOwnerAndRead); the row's update; the settings.project_default_assignee.updated that
// records it; and right after that event the move of who each open ask that falls through to the
// default waits on (inboxwrite.QueueMoveWaiting, inboxwrite.DefaultMoved). It answers the write's
// error rather than failing a test, so it runs off the test's goroutine.
func moveTheDefault(ctx context.Context, database *store.Store, broker *events.Broker, login string, by model.Actor) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the move of the default: %w", err)
	}
	defer tx.Rollback(ctx)
	var previous string
	var at time.Time
	if err := store.LockOwnerAndRead(ctx, tx, store.ProjectOwner("CENS"), `
		select default_assignee, now() from projects where key = $1
	`, []any{"CENS"}, &previous, &at); err != nil {
		return fmt.Errorf("read the default the move moves from: %w", err)
	}
	if _, err := tx.Exec(ctx, `update projects set default_assignee = $2 where key = $1`, "CENS", login); err != nil {
		return fmt.Errorf("move the default: %w", err)
	}
	moved, err := broker.Append(ctx, tx, model.Event{ProjectKey: new("CENS"), Type: "settings.project_default_assignee.updated", Actor: by,
		Payload: map[string]any{"project": "CENS", "assignee": login, "thread_logins": []string{previous, login}}})
	if err != nil {
		return fmt.Errorf("append the move of the default: %w", err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		return inboxwrite.QueueMoveWaiting(b, inboxwrite.DefaultMoved("CENS", at, by), moved.ID)
	}); err != nil {
		return fmt.Errorf("move who the asks that fall through to the default wait on: %w", err)
	}
	return tx.Commit(ctx)
}

// R9's tick catch-up is the backfill's for a row it reaches only once a run of this build's
// backfill has recorded its horizon (backfillRan): cmd/dispatch/README.md says "a build of rule 2
// reads no cursor or horizon of rule 1". The release before this one is of the marking rule
// before this build's, and its backfill-graph-store records a horizon of that rule
// (graph_backfill_horizon) having walked none of the inbox's passes, so a database whose one
// horizon is of that rule has had no run of this backfill: what a build before this one left owed
// is still the backfill's, which takes the tables in one walk.
//
// A build of the release before edits alice's comment, and the trigger of migration 20261122100123
// queues the edit's event. With a horizon of the rule before alone, the edit owes the block-index
// tick nothing (EventsOweObjectPass) and the tick's catch-up (CatchUpOwedRows) marks nothing and
// leaves the queue as it was. Once a horizon of this build's rule is recorded, the same edit owes
// the tick its catch-up, which takes the event off the queue. Its control is backfillRan reading a
// horizon of any rule: the edit owes the tick at once, and the first catch-up takes it.
func TestAHorizonOfTheMarkingRuleBeforeIsNoRunOfThisBuildsBackfill(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	comment := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	after := newestEventID(t, writes.database)
	const body = "Is the copy final? Over to dispatch://person/" + daveAddress + " for it."
	edited := writeAsTheReleaseBefore(t, writes, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		_, err := tx.Exec(ctx, `update comments set body = $2, edited_at = now() where id = $1::uuid`, comment.id, body)
		return model.Event{IssueKey: new("CENS-1"), Type: "comment.edited", Actor: alice,
			Payload: map[string]any{"id": comment.id, "body": body}}, err
	})
	recordHorizon := func(rule int) {
		t.Helper()
		if _, err := writes.database.Pool.Exec(ctx, `
			insert into graph_backfill_horizon (marking_rule, event_id) values ($1, $2)
			on conflict (marking_rule) do update set event_id = excluded.event_id, recorded_at = now()
		`, rule, edited.ID); err != nil {
			t.Fatalf("record a horizon of marking rule %d: %v", rule, err)
		}
	}

	recordHorizon(refs.BlockIndexMarkingRule - 1)
	if eventsOwe(t, writes.database, after, edited.ID) {
		t.Errorf("with a horizon of marking rule %d alone the older build's edit owes the tick's catch-up, want nothing owed before this build's backfill ran",
			refs.BlockIndexMarkingRule-1)
	}
	caught, err := CatchUpOwedRows(ctx, writes.database)
	if err != nil {
		t.Fatalf("catch the inbox's rows up over a horizon of the rule before: %v", err)
	}
	if caught.Rows != 0 || caught.More {
		t.Errorf("with a horizon of marking rule %d alone the tick's catch-up reports %#v, want nothing read", refs.BlockIndexMarkingRule-1, caught)
	}
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 1 || queued[0] != edited.ID {
		t.Errorf("with a horizon of marking rule %d alone the queue holds %v after the tick's catch-up, want the older build's edit %d still there",
			refs.BlockIndexMarkingRule-1, queued, edited.ID)
	}

	recordHorizon(refs.BlockIndexMarkingRule)
	if !eventsOwe(t, writes.database, after, edited.ID) {
		t.Errorf("with a horizon of this build's marking rule %d the older build's edit owes the tick's catch-up nothing, want it owed", refs.BlockIndexMarkingRule)
	}
	catchUpTheTick(t, writes.database, "over a horizon of this build's rule")
	if queued := queuedEvents(t, writes.database, "events_inbox_owed"); len(queued) != 0 {
		t.Errorf("with a horizon of this build's marking rule the queue holds %v after the tick's catch-up, want none", queued)
	}
}

// R5, R2 and R4 of a comment and an issue message a build before this one wrote before this
// release's schema reached the database, or before any backfill ran: such a build states no
// mention of an identity and stores no row of a person's (refs.TargetOf), so the item's text is
// the one record of whom it names, and the backfill reads it once, beside what each item owed its
// thread (writeCommentPointers, writeMessages), as this release's write of the same text states it.
//
// alice opens a thread, and bob replies naming dave; bob writes an issue message naming dave, an
// agent's session and a role; and carol writes a comment that names erin only in code, which names
// nobody. A person's address is an email address (text's personLoginPattern). The backfill states,
// under each item's own event: the mentions tuple to dave out of bob's reply, and to dave, the
// session and the role out of his message; the reply's delivered_to to dave, whom no row of the
// thread reached, and to alice, whom the old trigger reached; the message's delivered_to to dave;
// dave's participation in the thread's root and in the message; no delivered_to to the session or
// the role, which a mention reaches nothing of by itself in this release; and nothing to erin. A
// second run appends nothing, and the census of user_threads reads no difference: dave holds no
// row of either thread, and his participation is the mention's.
func TestTheBackfillStatesThePeopleAnOlderBuildsCommentsAndIssueMessagesName(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to)
		values ('CENS-1', $1, 'Nearly: over to dispatch://person/`+daveAddress+` to check.', $2)
		returning id::text, created_at`, actorJSON(t, bob), thread.id)
	message := writeAsAnOlderBuild(t, writes, "message.created", bob, `
		insert into messages (issue_key, author, body)
		values ('CENS-1', $1, 'For dispatch://person/`+daveAddress+` to read, with dispatch://agent/`+mentionedSession+` and dispatch://role/reviewer.')
		returning id::text, created_at`, actorJSON(t, bob))
	coded := writeAsAnOlderBuild(t, writes, "comment.created", carol, "insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Spelled `dispatch://person/erin@example.com`.') returning id::text, created_at",
		actorJSON(t, carol))

	backfillInboxOnce(t, writes.database)
	requireStands(t, writes.database, "bob's reply", reply.event,
		graphstore.KindComment, reply.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	for _, reached := range []string{daveAddress, "alice"} {
		requireStands(t, writes.database, "bob's reply", reply.event,
			graphstore.KindComment, reply.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, reached)
	}
	requireStands(t, writes.database, "bob's reply", reply.event,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindComment, thread.id)
	requireStands(t, writes.database, "bob's message", message.event,
		graphstore.KindMessage, message.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
	requireStands(t, writes.database, "bob's message", message.event,
		graphstore.KindMessage, message.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
	requireStands(t, writes.database, "bob's message", message.event,
		graphstore.KindPerson, daveAddress, graphstore.TupleParticipatesIn, graphstore.KindMessage, message.id)
	for _, identity := range []struct{ kind, id string }{{graphstore.KindSession, mentionedSession}, {graphstore.KindRole, "reviewer"}} {
		requireStands(t, writes.database, "bob's message", message.event,
			graphstore.KindMessage, message.id, graphstore.TupleMentions, identity.kind, identity.id)
		if asserted, under := newestOf(t, writes.database, graphstore.KindMessage, message.id, graphstore.TupleDeliveredTo, identity.kind, identity.id); asserted {
			t.Errorf("bob's message %s, which names %s %s, delivered to it under event %d, want no delivery a mention makes", message.id, identity.kind, identity.id, under)
		}
	}
	if asserted, _ := newestOf(t, writes.database, graphstore.KindComment, coded.id, graphstore.TupleMentions, graphstore.KindPerson, "erin@example.com"); asserted {
		t.Errorf("carol's comment %s, which spells erin's address only in code, mentions erin", coded.id)
	}
	requireTheThreadsAgree(t, writes.database, "after the backfill of the items naming people")
	requireASecondRunAppendsNothing(t, writes.database)
}

// R5's mention is made by the maker of the node that holds it (refs.ItemIdentityMentions), and
// what an item owed its thread by the person who wrote it (inboxwrite.Comment, inboxwrite.Message):
// the backfill reads the two off a comment's or an issue message's node and row
// (writeCommentPointers, writeMessages), each into an actor of its own. bob, proven by his
// browser's cookie and writing from his laptop, writes as a build of the release before a comment
// and an issue message that name dave, and each item's node is made by bob from his desk under a
// credential that proves nobody. No pass of the backfill makes a node whose maker differs from its
// row's author, so the test writes each node itself before the backfill runs, as a writer that
// wrote it meanwhile would, and the backfill keeps a node it finds as it finds it. Each item's
// mentions tuple to dave is made by its node's maker, with neither the author's mark nor his
// laptop, and its delivered_to to dave by its author, with his laptop and his mark.
func TestTheBackfillTellsTheMakerOfAnOlderBuildsItemsNodeFromItsAuthor(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	fromLaptop := model.Actor{Kind: "user", ID: "bob", Origin: &model.ActorOrigin{Host: "laptop"}, Proven: true}
	fromDesk := model.Actor{Kind: "user", ID: "bob", Origin: &model.ActorOrigin{Host: "desk"}}
	comment := writeAsAnOlderBuild(t, writes, "comment.created", fromLaptop, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Over to dispatch://person/`+daveAddress+` to check.')
		returning id::text, created_at`, actorJSON(t, fromLaptop))
	message := writeAsAnOlderBuild(t, writes, "message.created", fromLaptop, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, 'For dispatch://person/`+daveAddress+` to read.')
		returning id::text, created_at`, actorJSON(t, fromLaptop))
	commentNode, _ := graphstore.CommentFacts(nil, comment.id, fromDesk, comment.at, comment.event, "", "", "CENS", nil)
	messageNode := graphstore.MessageNode(message.id, "CENS", nil, fromDesk, message.at)
	if err := pgx.BeginFunc(ctx, writes.database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{commentNode, messageNode}, nil)
	}); err != nil {
		t.Fatalf("write the items' nodes, made by bob from his desk: %v", err)
	}

	backfillInboxOnce(t, writes.database)
	for _, item := range []struct {
		what, kind, id string
		event          int64
	}{{"bob's comment", graphstore.KindComment, comment.id, comment.event}, {"bob's message", graphstore.KindMessage, message.id, message.event}} {
		requireStands(t, writes.database, item.what, item.event,
			item.kind, item.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
		requireMadeBy(t, writes.database, item.what, fromDesk,
			item.kind, item.id, graphstore.TupleMentions, graphstore.KindPerson, daveAddress)
		requireStands(t, writes.database, item.what, item.event,
			item.kind, item.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
		requireMadeBy(t, writes.database, item.what, fromLaptop,
			item.kind, item.id, graphstore.TupleDeliveredTo, graphstore.KindPerson, daveAddress)
	}
}

// mentionedSession is the agent's session the texts of these tests name, routedSession the one a
// comment's delivery route names and its text does not, and daveAddress the address of the person
// they name.
const (
	mentionedSession = "019a1c0b-3333-7000-8000-000000000042"
	routedSession    = "019a1c0b-4444-7000-8000-000000000043"
	daveAddress      = "dave@example.com"
)
