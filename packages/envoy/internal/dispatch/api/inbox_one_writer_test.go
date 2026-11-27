package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTheOneWriterRecordsParticipationAndDelivery holds the one writer of the threads a person
// takes part in (inboxwrite.Record, this child's D3, R2, R4 and R9) to what each write
// records, every expected tuple written out by hand, each carrying the event of the write that
// appended it: who takes part in the thread, whom the write reached and never its own writer, the
// user_threads rows it moved and began, and the markers that hand a write an older build made to
// the triggers of 0095-0097 and to the backfill. It also holds graphstore.ToPerson to the rule it
// keeps: the one way a relation reaches a person, and never from a node reserved for its author to
// anybody else.
func TestTheOneWriterRecordsParticipationAndDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A listener that holds s1 live, so a direct message to s1 is delivered and s1 may answer it.
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "ONEW", "The one writer", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"

	t.Run("a reply in a thread of three people reaches the other two and not its writer", func(t *testing.T) {
		root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship on Friday?"}, "alice")
		bob := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Friday works.", "reply_to": root.ID}, "bob")
		carol := createThreadComment(t, handler, issue.Key, map[string]any{"body": "So does Monday.", "reply_to": root.ID}, "carol")
		rootEvent := writeEventOf(t, database, "comment.created", root.ID)
		bobEvent := writeEventOf(t, database, "comment.created", bob.ID)
		carolEvent := writeEventOf(t, database, "comment.created", carol.ID)
		thread := "comment:" + root.ID

		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, rootEvent),
			fmt.Sprintf("person:bob > %s @%d", thread, bobEvent),
			fmt.Sprintf("person:carol > %s @%d", thread, carolEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread)
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+bob.ID,
			fmt.Sprintf("comment:%s > person:alice @%d", bob.ID, bobEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+carol.ID,
			fmt.Sprintf("comment:%s > person:alice @%d", carol.ID, carolEvent),
			fmt.Sprintf("comment:%s > person:bob @%d", carol.ID, carolEvent))

		for login, version := range map[string]int64{"alice": 2, "bob": 1, "carol": 0} {
			row := readThreadRow(t, database, login, root.ID)
			if !row.marked || row.markedVersion != 0 || row.version != version {
				t.Errorf("%s's row = %+v, want a row this build began, marked at version 0, at version %d", login, row, version)
			}
			requireVersionCountsReplies(t, database, login, graphstore.KindComment, root.ID)
		}
		if row := threadRowWrites(t, database, "alice", root.ID); row.lastReply != carol.ID || !row.repliedAt.Equal(carol.CreatedAt) {
			t.Errorf("alice's row names %+v, want carol's reply, the newest write by somebody else", row)
		}
	})

	t.Run("a session takes part in a thread and keeps no row", func(t *testing.T) {
		root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Can a session look?"}, "alice")
		reply := sessionComment(t, handler, comments, map[string]any{"body": "Looking.", "reply_to": root.ID})
		rootEvent := writeEventOf(t, database, "comment.created", root.ID)
		replyEvent := writeEventOf(t, database, "comment.created", reply.ID)
		thread := "comment:" + root.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, rootEvent),
			fmt.Sprintf("session:%s > %s @%d", sessionActor()["id"], thread, replyEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+reply.ID,
			fmt.Sprintf("comment:%s > person:alice @%d", reply.ID, replyEvent))
		if logins := threadRowLogins(t, database, root.ID); !slices.Equal(logins, []string{"alice"}) {
			t.Errorf("the thread's rows are %v, want alice's alone: a session keeps no row", logins)
		}
	})

	t.Run("a thread of no project takes participation and delivery and keeps no row", func(t *testing.T) {
		root, answer := agentThread(t, handler, "steer", "Can you look at the build?", "Looking now.")
		rootEvent := writeEventOf(t, database, "message.created", root.ID)
		answerEvent := writeEventOf(t, database, "message.answered", answer.ID)
		thread := "message:" + root.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, rootEvent),
			fmt.Sprintf("session:s1 > %s @%d", thread, answerEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "message:"+answer.ID,
			fmt.Sprintf("message:%s > person:alice @%d", answer.ID, answerEvent))
		if logins := threadRowLogins(t, database, root.ID); len(logins) != 0 {
			t.Errorf("the direct thread's rows are %v, want none: a thread of no project keeps no row", logins)
		}
	})

	t.Run("a reply retracts the dismissal of everyone it reaches and clears their mark", func(t *testing.T) {
		root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Dismiss me later."}, "alice")
		bob := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Read this.", "reply_to": root.ID}, "bob")
		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissReplyPrefix+root.ID); dismissed != 1 {
			t.Fatalf("dismissed = %d, want alice's thread", dismissed)
		}
		if at := threadDismissal(t, database, "alice", root.ID); at == nil {
			t.Fatalf("alice's row carries no dismissal after her dismiss")
		}
		// The dismissal's tuple from the person, which the dismiss route states under the event it
		// appends (the spec's R6): the reply below retracts it. Each reply is also its
		// writer's own, so it dismisses the thread for its writer (the own-answer rule, R1): bob's
		// for bob, and carol's for carol, which brings bob's back with alice's.
		dismissEvent := newestEventOf(t, database, "user_thread_state.updated", "alice")
		thread := "comment:" + root.ID
		bobEvent := writeEventOf(t, database, "comment.created", bob.ID)
		carol := createThreadComment(t, handler, issue.Key, map[string]any{"body": "One more.", "reply_to": root.ID}, "carol")
		carolEvent := writeEventOf(t, database, "comment.created", carol.ID)
		requireRelationTuples(t, database, graphstore.TupleDismissed, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, dismissEvent),
			fmt.Sprintf("person:alice > %s @%d retracted", thread, carolEvent),
			fmt.Sprintf("person:bob > %s @%d", thread, bobEvent),
			fmt.Sprintf("person:bob > %s @%d retracted", thread, carolEvent),
			fmt.Sprintf("person:carol > %s @%d", thread, carolEvent))
		if at := threadDismissal(t, database, "alice", root.ID); at != nil {
			t.Errorf("alice's row still carries the dismissal %s after carol wrote in the thread", at)
		}
	})

	t.Run("an ask's create makes its asker take part, and an answer reaches the asker", func(t *testing.T) {
		asked := createPersonAsk(t, handler, issue.Key, "alice", "Should we ship?")
		openedEvent := writeEventOf(t, database, "ask.opened", asked.ID)
		thread := "ask:" + asked.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, openedEvent))
		if row := readThreadRow(t, database, "alice", asked.ID); !row.marked || row.version != 0 {
			t.Errorf("the asker's row = %+v, want one this build began, at version 0", row)
		}

		answer := answerAskForChange(t, handler, asked.ID, "bob", "Ship", nil)
		answeredEvent := writeEventOf(t, database, "ask.answered", asked.ID)
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, openedEvent),
			fmt.Sprintf("person:bob > %s @%d", thread, answeredEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:alice @%d", thread, answeredEvent))
		requireTuplesStoredAt(t, database, asked.ID, answer.Answer.At)
		if row := readThreadRow(t, database, "alice", asked.ID); !row.marked || row.version != 1 {
			t.Errorf("the asker's row = %+v, want it moved once by the answer", row)
		}
		requireVersionCountsReplies(t, database, "alice", graphstore.KindAsk, asked.ID)
		if row := readThreadRow(t, database, "bob", asked.ID); !row.marked || row.version != 0 {
			t.Errorf("the answerer's row = %+v, want one this build began, at version 0", row)
		}
	})

	t.Run("a decline takes part and delivers nothing", func(t *testing.T) {
		declined := createPersonAsk(t, handler, issue.Key, "alice", "Should we hold?")
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+declined.ID+"/decline",
			map[string]any{"note": "Not mine to answer."}, "bob"); response.Code != http.StatusOK {
			t.Fatalf("decline the ask: status=%d body=%s", response.Code, response.Body.String())
		}
		openedEvent := writeEventOf(t, database, "ask.opened", declined.ID)
		resolvedEvent := writeEventOf(t, database, "ask.resolved", declined.ID)
		thread := "ask:" + declined.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, openedEvent),
			fmt.Sprintf("person:bob > %s @%d", thread, resolvedEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread)
		if logins := threadRowLogins(t, database, declined.ID); !slices.Equal(logins, []string{"alice"}) {
			t.Errorf("the declined ask's rows are %v, want the asker's alone: a decline moves no row", logins)
		}
	})

	t.Run("an answer to an ask an older build created first appends what its create owes", func(t *testing.T) {
		older, openedEvent, createdAt := insertOlderAsk(t, database, issue.Key, "alice", "dana", "Ship the older build's way?")
		thread := "ask:" + older
		// The old trigger gave the asker an unmarked row (0096), and nothing reached the store.
		if row := readThreadRow(t, database, "alice", older); row.marked {
			t.Fatalf("the older build's ask gave its asker a marked row %+v", row)
		}
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread)

		answer := answerAskForChange(t, handler, older, "bob", "Ship", nil)
		answeredEvent := writeEventOf(t, database, "ask.answered", older)
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, openedEvent),
			fmt.Sprintf("person:bob > %s @%d", thread, answeredEvent),
			fmt.Sprintf("person:dana > %s @%d", thread, openedEvent))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:alice @%d", thread, answeredEvent),
			fmt.Sprintf("%s > person:dana @%d", thread, answeredEvent),
			fmt.Sprintf("%s > person:dana @%d", thread, openedEvent))
		var stale int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from graph_tuples
			where source_event = $1 and to_kind = $2 and to_id = $3 and at <> $4
		`, openedEvent, graphstore.KindAsk, older, createdAt).Scan(&stale); err != nil || stale != 0 {
			t.Errorf("%d of the create's tuples are dated away from the ask's created_at %s (err %v)", stale, createdAt, err)
		}
		requireTuplesStoredAt(t, database, older, answer.Answer.At)
		if row := readThreadRow(t, database, "alice", older); row.marked || row.version != 1 {
			t.Errorf("the asker's row = %+v, want it moved by the answer and still the backfill's", row)
		}
	})

	t.Run("an older build's comment is the trigger's, and this build's reply reaches its writer", func(t *testing.T) {
		older := insertOlderComment(t, database, `
			insert into comments (issue_key, author, body)
			values ($1, '{"kind":"user","id":"erin"}', 'Written by the release before.')
			returning id::text
		`, issue.Key)
		thread := "comment:" + older
		if row := readThreadRow(t, database, "erin", older); row.marked {
			t.Fatalf("the trigger gave the older build's writer a marked row %+v", row)
		}
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread)

		reply := createThreadComment(t, handler, issue.Key, map[string]any{"body": "This build's reply.", "reply_to": older}, "alice")
		replyEvent := writeEventOf(t, database, "comment.created", reply.ID)
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+reply.ID,
			fmt.Sprintf("comment:%s > person:erin @%d", reply.ID, replyEvent))
		if row := readThreadRow(t, database, "erin", older); row.marked || row.version != 1 {
			t.Errorf("erin's row = %+v, want it moved once by the reply and still the backfill's", row)
		}
		if row := threadRowWrites(t, database, "erin", older); row.lastReply != reply.ID {
			t.Errorf("erin's row names %+v, want this build's reply", row)
		}
		if row := readThreadRow(t, database, "alice", older); !row.marked || row.version != 0 {
			t.Errorf("alice's row = %+v, want one this build began", row)
		}
	})

	t.Run("an update of a marked row keeps its marker through this build's pool alone", func(t *testing.T) {
		root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Mark me."}, "alice")
		touch := `update user_threads set read_through = read_through where login = 'alice' and thread_id = $1::uuid`
		if _, err := database.Pool.Exec(ctx, touch, root.ID); err != nil {
			t.Fatalf("update the row through this build's pool: %v", err)
		}
		if row := readThreadRow(t, database, "alice", root.ID); !row.marked {
			t.Fatalf("an update through this build's pool unmarked the row: %+v", row)
		}
		if _, err := olderBuild(t, database).Exec(ctx, touch, root.ID); err != nil {
			t.Fatalf("update the row as an older build: %v", err)
		}
		if row := readThreadRow(t, database, "alice", root.ID); row.marked || row.markedVersionSet {
			t.Fatalf("an older build's update left the row marked: %+v", row)
		}
	})

	t.Run("a person a reply reached before their row began is marked above that delivery", func(t *testing.T) {
		// bob waits on the ask, so his part in its thread and its delivery to him are the hand-on's
		// and he holds no row of it; alice's comment in the thread then reaches him, a delivery a
		// reply caused, and his own write after it begins his row. The row is marked at the log's
		// position as his write saw it, above alice's delivery, so the version it begins at counts
		// no reply: a marked_seq of 0 would count alice's.
		seedPeople(t, database, "bob")
		asked := createPersonAsk(t, handler, issue.Key, "alice", "Who takes the rollout?")
		handAskOn(t, handler, asked.ID, "bob", "alice")
		thread := "ask:" + asked.ID
		handed := writeEventOf(t, database, "ask.asked_of", asked.ID)
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, handed))
		asking := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Can you take it?", "ask_id": asked.ID}, "alice")
		askingEvent := writeEventOf(t, database, "comment.created", asking.ID)
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+asking.ID,
			fmt.Sprintf("comment:%s > person:bob @%d", asking.ID, askingEvent))
		if logins := threadRowLogins(t, database, asked.ID); !slices.Equal(logins, []string{"alice"}) {
			t.Fatalf("the ask's rows are %v before bob wrote, want the asker's alone", logins)
		}

		taking := createThreadComment(t, handler, issue.Key, map[string]any{"body": "I can.", "ask_id": asked.ID}, "bob")
		takingEvent := writeEventOf(t, database, "comment.created", taking.ID)
		if row := readThreadRow(t, database, "bob", asked.ID); !row.marked || row.markedVersion != 0 || row.version != 0 {
			t.Errorf("bob's row = %+v, want one his write began, marked at version 0, at version 0", row)
		}
		var markedSeq, seen, delivered int64
		if err := database.Pool.QueryRow(ctx, `
			select u.marked_seq,
			       (select max(t.seq) from graph_tuples t where t.seq < (
			            select r.seq from graph_tuples r
			            where r.from_kind = $3 and r.from_id = 'bob' and r.kind = $4 and r.to_kind = $5 and r.to_id = $1
			              and r.source_event = $2)),
			       (select d.seq from graph_tuples d
			        where d.from_kind = $6 and d.from_id = $7 and d.kind = $8 and d.to_kind = $3 and d.to_id = 'bob')
			from user_threads u where u.login = 'bob' and u.thread_id = $1::uuid
		`, asked.ID, takingEvent, graphstore.KindPerson, graphstore.TupleReadThrough, graphstore.KindAsk,
			graphstore.KindComment, asking.ID, graphstore.TupleDeliveredTo).Scan(&markedSeq, &seen, &delivered); err != nil {
			t.Fatalf("read bob's marker: %v", err)
		}
		// The row's insert reads the newest position of the log its transaction sees, and bob's
		// read_through to the thread is the next tuple the write appends.
		if markedSeq != seen {
			t.Errorf("bob's row is marked at seq %d, want %d, the newest tuple before the read mark his write stated", markedSeq, seen)
		}
		if markedSeq < delivered {
			t.Errorf("bob's row is marked at seq %d, below alice's delivery to him at %d, which his row would count", markedSeq, delivered)
		}
		requireVersionCountsReplies(t, database, "bob", graphstore.KindAsk, asked.ID)
	})

	t.Run("ToPerson is the one way a relation reaches a person, and never past a node's author", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		alice := model.Actor{Kind: "user", ID: "alice"}
		now := time.Now().UTC()
		note := graphstore.Node{
			Kind: graphstore.KindComment, ID: uuid.NewString(), Supertag: graphstore.KindComment,
			Visibility: graphstore.VisibilityAuthor, State: graphstore.StateLive,
			MadeAt: now, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{note}, nil); err != nil {
			t.Fatalf("write a node reserved for its author: %v", err)
		}
		delivered := func(login string) graphstore.Tuple {
			return graphstore.Tuple{
				FromKind: note.Kind, FromID: note.ID, Kind: graphstore.TupleDeliveredTo,
				ToKind: graphstore.KindPerson, ToID: login,
				At: now, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
			}
		}
		// A node reserved for alice reaches nobody else, and the refusal names the node and its
		// visibility. The refusal is the callback of the read that is the query of ToPerson's one
		// statement (graphstore.QueueToPerson), whose write has run by the time it is answered: the
		// call runs in a savepoint, rolled back, and the transaction it ends commits nothing of it.
		// The other refusal below is made before anything is sent.
		refused, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the refused call's savepoint: %v", err)
		}
		err = graphstore.ToPerson(ctx, refused, []graphstore.Tuple{delivered("bob")})
		if !errors.Is(err, graphstore.ErrReservedForAuthor) || !strings.Contains(err.Error(), note.Kind+"/"+note.ID) ||
			!strings.Contains(err.Error(), "visibility "+graphstore.VisibilityAuthor) {
			t.Fatalf("ToPerson to bob from alice's reserved node = %v, want ErrReservedForAuthor naming the node and its visibility", err)
		}
		if err := refused.Rollback(ctx); err != nil {
			t.Fatalf("roll the refused call back: %v", err)
		}
		// No other writer of the store appends a person's relation.
		if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{delivered("alice")}); !errors.Is(err, graphstore.ErrPersonTupleOutsideToPerson) {
			t.Fatalf("graphstore.Write of a delivered_to to a person = %v, want ErrPersonTupleOutsideToPerson", err)
		}
		// The node's own author is reached, under the canonical login whatever spelling the
		// writer handed it.
		if err := graphstore.ToPerson(ctx, tx, []graphstore.Tuple{delivered("  Alice ")}); err != nil {
			t.Fatalf("ToPerson to the node's own author: %v", err)
		}
		reached, err := readStrings(ctx, tx, `
			select to_kind || ':' || to_id from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
		`, note.Kind, note.ID, graphstore.TupleDeliveredTo)
		if err != nil || !slices.Equal(reached, []string{"person:alice"}) {
			t.Fatalf("the reserved node reached %v (err %v), want its author alone, as person:alice", reached, err)
		}
	})
}

// writeEventOf is the newest event of eventType whose payload names id, the event a write of that
// item appended.
func writeEventOf(t *testing.T, database *store.Store, eventType, id string) int64 {
	t.Helper()
	var event int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(id), 0) from events where type = $1 and payload->>'id' = $2
	`, eventType, id).Scan(&event); err != nil || event == 0 {
		t.Fatalf("read the %s of %s: event %d, %v", eventType, id, event, err)
	}
	return event
}

// newestEventOf is the newest event of eventType whose payload names login, the event a write of
// that person's own read state appended.
func newestEventOf(t *testing.T, database *store.Store, eventType, login string) int64 {
	t.Helper()
	var event int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(id), 0) from events where type = $1 and payload->>'login' = $2
	`, eventType, login).Scan(&event); err != nil || event == 0 {
		t.Fatalf("read the %s of %s: event %d, %v", eventType, login, event, err)
	}
	return event
}

// requireRelationTuples fails unless the tuples of kind that have node at either end are exactly
// want, each written "<from kind>:<from id> > <to kind>:<to id> @<source event>", with
// " retracted" after a retraction, in any order.
func requireRelationTuples(t *testing.T, database *store.Store, kind, node string, want ...string) {
	t.Helper()
	got, err := readStrings(context.Background(), database.Pool, `
		select from_kind || ':' || from_id || ' > ' || to_kind || ':' || to_id || ' @' || source_event::text
		       || case when asserted then '' else ' retracted' end
		from graph_tuples
		where kind = $1 and (from_kind || ':' || from_id = $2 or to_kind || ':' || to_id = $2)
	`, kind, node)
	if err != nil {
		t.Fatalf("read the %s tuples of %s: %v", kind, node, err)
	}
	// Both sides are sorted here, as Go orders bytes: SQL's order would follow the database's
	// collation, which need not agree.
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the %s tuples of %s are\n  %s\nwant\n  %s", kind, node, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// oneWriterRow is what a user_threads row says of its writes and its marker.
type oneWriterRow struct {
	version, markedVersion int64
	// marked is whether the row's marked_seq is set, and markedVersionSet whether its
	// marked_version is.
	marked, markedVersionSet bool
}

// readThreadRow reads login's row on threadID, which has to be there.
func readThreadRow(t *testing.T, database *store.Store, login, threadID string) oneWriterRow {
	t.Helper()
	var row oneWriterRow
	var markedVersion, markedSeq *int64
	if err := database.Pool.QueryRow(context.Background(), `
		select version, marked_version, marked_seq from user_threads where login = $1 and thread_id = $2::uuid
	`, login, threadID).Scan(&row.version, &markedVersion, &markedSeq); err != nil {
		t.Fatalf("read %s's row on thread %s: %v", login, threadID, err)
	}
	row.marked, row.markedVersionSet = markedSeq != nil, markedVersion != nil
	if markedVersion != nil {
		row.markedVersion = *markedVersion
	}
	return row
}

// threadRowLogins is every login holding a row on threadID, sorted.
func threadRowLogins(t *testing.T, database *store.Store, threadID string) []string {
	t.Helper()
	logins, err := readStrings(context.Background(), database.Pool,
		`select login from user_threads where thread_id = $1::uuid order by login`, threadID)
	if err != nil {
		t.Fatalf("read the rows of thread %s: %v", threadID, err)
	}
	return logins
}

// requireVersionCountsReplies holds login's marked row on the thread rooted at kind/id to D4's
// equation (P13): its version is its marked_version plus the delivered_to tuples into the person
// from that thread - from its root, or from a write whose replies_to names the root - with seq
// above its marked_seq, that a reply caused, which is a tuple whose event is the write of a
// comment, a message or an answer.
func requireVersionCountsReplies(t *testing.T, database *store.Store, login, kind, id string) {
	t.Helper()
	var version, markedVersion, counted int64
	if err := database.Pool.QueryRow(context.Background(), `
		select u.version, u.marked_version, (
			select count(*) from graph_tuples d
			join events e on e.id = d.source_event
			where d.kind = $3 and d.to_kind = $4 and d.to_id = u.login and d.asserted and d.seq > u.marked_seq
			  and e.type in ('comment.created', 'comment.answered', 'message.created', 'message.answered', 'ask.answered')
			  and ((d.from_kind = $5 and d.from_id = $6) or exists (
			      select 1 from graph_tuples r
			      where r.from_kind = d.from_kind and r.from_id = d.from_id and r.kind = $7
			        and r.to_kind = $5 and r.to_id = $6 and r.asserted))
		)
		from user_threads u
		where u.login = $1 and u.thread_id = $2::uuid and u.marked_seq is not null
	`, login, id, graphstore.TupleDeliveredTo, graphstore.KindPerson, kind, id, graphstore.TupleRepliesTo).
		Scan(&version, &markedVersion, &counted); err != nil {
		t.Fatalf("read %s's marked row on %s/%s: %v", login, kind, id, err)
	}
	if version != markedVersion+counted {
		t.Errorf("%s's row on %s/%s is at version %d, want marked_version %d plus the %d delivery a reply caused since it was marked",
			login, kind, id, version, markedVersion, counted)
	}
}

// requireTuplesStoredAt fails unless ask askID's marker holds at, the moment of the answer this
// build wrote.
func requireTuplesStoredAt(t *testing.T, database *store.Store, askID string, at time.Time) {
	t.Helper()
	var stored *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select tuples_stored_at from asks where id = $1::uuid`, askID).Scan(&stored); err != nil {
		t.Fatalf("read the marker of ask %s: %v", askID, err)
	}
	if stored == nil || !stored.Equal(at) {
		t.Errorf("ask %s's tuples_stored_at = %v, want the answer's own moment %s", askID, stored, at)
	}
}

// createPersonAsk opens an ask with the options Ship and Hold on the issue as login, a person.
func createPersonAsk(t *testing.T, handler http.Handler, issueKey, login, question string) model.Ask {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"options":  []map[string]string{{"label": "Ship"}, {"label": "Hold"}},
	}, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("open an ask as %s: status=%d body=%s", login, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// insertOlderAsk writes an ask as a build of the release before writes one: its row, with no
// marker and no node, asked of askedOf, and its ask.opened event, numbered under its issue as that
// build's broker numbers it. It answers the ask's id, the event's id and the ask's created_at.
func insertOlderAsk(t *testing.T, database *store.Store, issueKey, author, askedOf, question string) (string, int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	actor := fmt.Sprintf(`{"kind":"user","id":%q}`, author)
	var id string
	var createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, options, asked_of)
		values ($1, $2::jsonb, $3, '[{"label":"Ship"},{"label":"Hold"}]', $4)
		returning id::text, created_at
	`, issueKey, actor, question, askedOf).Scan(&id, &createdAt); err != nil {
		t.Fatalf("write an ask as the release before writes one: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"id": id, "issue_key": issueKey, "question": question, "state": "open",
		"author": map[string]string{"kind": "user", "id": author}, "created_at": createdAt,
	})
	if err != nil {
		t.Fatalf("encode the older ask's ask.opened: %v", err)
	}
	var event int64
	if err := database.Pool.QueryRow(ctx, `
		with numbered as (update issues set last_seq = last_seq + 1 where key = $1 returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, numbered.last_seq, 'ask.opened', $2::jsonb, $3::jsonb, false from numbered
		returning id
	`, issueKey, actor, payload).Scan(&event); err != nil {
		t.Fatalf("append the older ask's ask.opened: %v", err)
	}
	return id, event, createdAt
}

// writeInTx runs write in a transaction of the test's database and commits it.
func writeInTx(t *testing.T, database *store.Store, write func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
