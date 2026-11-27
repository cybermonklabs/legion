package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestEveryReadStateWriteIsATupleFromThePerson holds each writer of the six read-state tables to
// what the spec's R6 asks of it (api/read_marks.go): the mark it stored is a tuple from
// the writer's one person node, to the thread's root, the session, the reply, the root message or
// the ask, at the mark's own moment and under the event the write appended, and a row of the five
// tables other than user_threads that the write stored is marked at that moment. A mark that ends
// is a retraction, so its history reads back; every spelling of a login meets at one node; an
// older build's update of a marked row unmarks it; and an older build's prune of a reply read by
// id, which leaves its tuple standing, reads as the row would have (unreadDirectRepliesCTE).
func TestEveryReadStateWriteIsATupleFromThePerson(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}

	t.Run("a thread read, Mark all read, a dismissal and its restore are tuples to the thread's root", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "RSTH", "Read state of a thread", "A spec")
		root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship on Friday?"}, "alice")
		thread := graphstore.KindComment + ":" + root.ID
		// alice's own write moves her read mark, and the one writer of a thread states it.
		requireMarkMatchesRow(t, database, graphstore.TupleReadThrough, thread, writeEventOf(t, database, "comment.created", root.ID),
			threadRowMark(t, database, "read_through", root.ID))

		bob := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Friday works.", "reply_to": root.ID}, "bob")
		if read := markThreadRead(t, handler, "alice", root.ID, bob.CreatedAt); read.Code != http.StatusNoContent {
			t.Fatalf("read the thread: status=%d body=%s", read.Code, read.Body.String())
		}
		requireMarkMatchesRow(t, database, graphstore.TupleReadThrough, thread, newestEventOf(t, database, "user_thread_state.updated", "alice"),
			threadRowMark(t, database, "read_through", root.ID))

		createThreadComment(t, handler, issue.Key, map[string]any{"body": "Monday too.", "reply_to": root.ID}, "carol")
		if marked := postInboxRead(t, handler, "alice", map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)}); marked.Code != http.StatusOK || markedBy(t, marked) != 1 {
			t.Fatalf("Mark all read: status=%d body=%s, want alice's one thread marked", marked.Code, marked.Body.String())
		}
		requireMarkMatchesRow(t, database, graphstore.TupleReadThrough, thread, newestEventOf(t, database, "user_thread_state.updated", "alice"),
			threadRowMark(t, database, "read_through", root.ID))

		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissReplyPrefix+root.ID); dismissed != 1 {
			t.Fatalf("dismissed = %d, want alice's thread", dismissed)
		}
		dismissEvent := newestEventOf(t, database, "user_thread_state.updated", "alice")
		requireMarkMatchesRow(t, database, graphstore.TupleDismissed, thread, dismissEvent, threadRowMark(t, database, "dismissed_at", root.ID))
		// Dismissing marks the thread read, so the dismissal states the read mark the row holds.
		requireMarkMatchesRow(t, database, graphstore.TupleReadThrough, thread, dismissEvent, threadRowMark(t, database, "read_through", root.ID))

		if restored := restoreRows(t, handler, "alice", inboxDismissReplyPrefix+root.ID); restored != 1 {
			t.Fatalf("restored = %d, want alice's thread", restored)
		}
		restoreEvent := newestEventOf(t, database, "user_thread_state.updated", "alice")
		requireMarkHistory(t, database, graphstore.TupleDismissed, thread,
			fmt.Sprintf("@%d", dismissEvent), fmt.Sprintf("@%d retracted", restoreEvent))
	})

	t.Run("a Clear, a read mark and a reply read by its id are tuples to the session and the reply", func(t *testing.T) {
		handler, database, _, reply, _ := directConversationFrom(t, "alice")
		first := decodeBody[model.Message](t, reply("On it."))
		second := decodeBody[model.Message](t, reply("Done."))
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_replies": []string{first.ID}})
		stateEvent := newestEventOf(t, database, "user_agent_state.updated", "alice")
		requireMarkMatchesRow(t, database, graphstore.TupleReplyRead, graphstore.KindMessage+":"+first.ID, stateEvent,
			storedMarks(t, database, `select tuples_stored_at, tuples_stored_at from user_agent_reply_read where login = 'alice' and reply_id = $1::uuid`, first.ID))

		putAgentState(t, handler, "alice", "s1", map[string]any{"cleared_before": first.CreatedAt})
		requireMarkMatchesRow(t, database, graphstore.TupleCleared, "session:s1", newestEventOf(t, database, "user_agent_state.updated", "alice"),
			storedMarks(t, database, `select cleared_before, tuples_stored_at from user_agent_state where login = 'alice' and session_id = 's1'`))

		// The read mark passes the reply read by its id, so its row is pruned and its tuple retracted.
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_through": second.CreatedAt})
		readEvent := newestEventOf(t, database, "user_agent_state.updated", "alice")
		requireMarkMatchesRow(t, database, graphstore.TupleReadThrough, "session:s1", readEvent,
			storedMarks(t, database, `select read_through, tuples_stored_at from user_agent_read where login = 'alice' and session_id = 's1'`))
		requireMarkHistory(t, database, graphstore.TupleReplyRead, graphstore.KindMessage+":"+first.ID,
			fmt.Sprintf("@%d", stateEvent), fmt.Sprintf("@%d retracted", readEvent))
		if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 || got.ReadThrough == nil || got.ClearedBefore == nil {
			t.Errorf("alice reads %#v, want nothing unread, the read mark and the Clear", got)
		}
	})

	t.Run("a read mark that moves nothing states nothing", func(t *testing.T) {
		// A conversation's read mark sent again, and then short of the one its row holds, as a tab
		// that read less a moment ago sends it: the first is the one tuple of the three, so the
		// history directMarks reads grows with the marks that moved and no faster.
		handler, database, root, reply, _ := directConversationFrom(t, "alice")
		answer := decodeBody[model.Message](t, reply("On it."))
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_through": answer.CreatedAt})
		readEvent := newestEventOf(t, database, "user_agent_state.updated", "alice")
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_through": answer.CreatedAt})
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_through": root.CreatedAt})
		requireMarkHistory(t, database, graphstore.TupleReadThrough, "session:s1", fmt.Sprintf("@%d", readEvent))
		requireMarkMatchesRow(t, database, graphstore.TupleReadThrough, "session:s1", readEvent,
			storedMarks(t, database, `select read_through, tuples_stored_at from user_agent_read where login = 'alice' and session_id = 's1'`))

		// A dismissal through a moment the thread's read mark has passed marks nothing read, so it
		// states the dismissal alone; and a write of alice's own whose moment her read mark has
		// passed leaves her row's mark, and its tuple, as they were (inboxwrite.Record's queueKeepRow).
		threads, threadsDatabase := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, threads, "RSNM", "Marks that move nothing", "A spec")
		thread := createThreadComment(t, threads, issue.Key, map[string]any{"body": "Ship on Friday?"}, "alice")
		node := graphstore.KindComment + ":" + thread.ID
		written := writeEventOf(t, threadsDatabase, "comment.created", thread.ID)
		bob := createThreadComment(t, threads, issue.Key, map[string]any{"body": "Friday works.", "reply_to": thread.ID}, "bob")
		if read := markThreadRead(t, threads, "alice", thread.ID, time.Now().UTC()); read.Code != http.StatusNoContent {
			t.Fatalf("read the thread: status=%d body=%s", read.Code, read.Body.String())
		}
		threadRead := newestEventOf(t, threadsDatabase, "user_thread_state.updated", "alice")
		if dismissed := dismissThrough(t, threads, "alice", bob.CreatedAt, nil); dismissed != 1 {
			t.Fatalf("dismissed = %d, want alice's thread, whose newest reply is bob's", dismissed)
		}
		dismissEvent := newestEventOf(t, threadsDatabase, "user_thread_state.updated", "alice")
		requireMarkMatchesRow(t, threadsDatabase, graphstore.TupleDismissed, node, dismissEvent, threadRowMark(t, threadsDatabase, "dismissed_at", thread.ID))
		requireMarkHistory(t, threadsDatabase, graphstore.TupleReadThrough, node, fmt.Sprintf("@%d", written), fmt.Sprintf("@%d", threadRead))

		tx, err := threadsDatabase.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := inboxwrite.Record(ctx, tx, inboxwrite.Comment(thread.ID, alice, thread.CreatedAt, &issue.Key, nil, "RSNM"), graphstore.NoEvent); err != nil {
			t.Fatalf("record alice's write at its own moment again: %v", err)
		}
		var stated int
		if err := tx.QueryRow(ctx, `
			select count(*) from graph_tuples
			where from_kind = $1 and from_id = 'alice' and kind = $2 and to_kind = $3 and to_id = $4 and source_event = $5
		`, graphstore.KindPerson, graphstore.TupleReadThrough, graphstore.KindComment, thread.ID, graphstore.NoEvent).Scan(&stated); err != nil {
			t.Fatalf("count the read marks the write stated: %v", err)
		}
		if stated != 0 {
			t.Errorf("a write at %s, before alice's read mark, stated %d read_through of hers, want none", thread.CreatedAt, stated)
		}
	})

	t.Run("Mark all read and an Agent thread's dismissal read its replies by id", func(t *testing.T) {
		// Both write the Agent threads' unread replies by id (markInboxReadAgentReplies), each row
		// a reply_read tuple under the event of its session, marked at the moment of the reading.
		handler, database, _, reply, _ := directConversationFrom(t, "alice")
		first := decodeBody[model.Message](t, reply("On it."))
		if marked := postInboxRead(t, handler, "alice", map[string]any{"read_through": time.Now().UTC().Format(time.RFC3339Nano)}); marked.Code != http.StatusOK || markedBy(t, marked) != 1 {
			t.Fatalf("Mark all read: status=%d body=%s, want alice's one Agent thread marked", marked.Code, marked.Body.String())
		}
		requireMarkMatchesRow(t, database, graphstore.TupleReplyRead, graphstore.KindMessage+":"+first.ID,
			newestEventOf(t, database, "user_agent_state.updated", "alice"),
			storedMarks(t, database, `select tuples_stored_at, tuples_stored_at from user_agent_reply_read where login = 'alice' and reply_id = $1::uuid`, first.ID))

		created := postAgentMessage(t, handler, "s1", map[string]any{"body": "And the build?", "delivery": "aside"}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("a second direct message: status=%d body=%s", created.Code, created.Body.String())
		}
		second := decodeBody[model.Message](t, created)
		answered := replyTo(t, handler, second.ID, "Green.")
		if answered.Code != http.StatusCreated {
			t.Fatalf("s1's reply to the second message: status=%d body=%s", answered.Code, answered.Body.String())
		}
		answer := decodeBody[model.Message](t, answered)
		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissAgentPrefix+second.ID); dismissed != 1 {
			t.Fatalf("dismissed = %d, want alice's second Agent thread", dismissed)
		}
		requireMarkMatchesRow(t, database, graphstore.TupleReplyRead, graphstore.KindMessage+":"+answer.ID,
			newestEventOf(t, database, "user_agent_state.updated", "alice"),
			storedMarks(t, database, `select tuples_stored_at, tuples_stored_at from user_agent_reply_read where login = 'alice' and reply_id = $1::uuid`, answer.ID))
	})

	t.Run("the removal of a document retracts the marks of the Inbox rows it deletes", func(t *testing.T) {
		// Rows of the document's threads the untouched test lets through, as an older build or a
		// thread whose writes went leaves them: alice's dismissed, bob's not.
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "RSRM", "A removed document", "A spec")
		document := emptyDocument(t, handler, issue.Key, "Notes made by mistake")
		aliceThread, bobThread := uuid.NewString(), uuid.NewString()
		if _, err := database.Pool.Exec(ctx, `
			insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through, dismissed_at)
			values ('alice', $1::uuid, 'comment', 'RSRM', $3, $4::uuid, now(), now()),
			       ('bob', $2::uuid, 'comment', 'RSRM', $3, $4::uuid, now(), null)
		`, aliceThread, bobThread, issue.Key, document.ID); err != nil {
			t.Fatalf("seed the document's Inbox rows: %v", err)
		}
		removedDocument(t, handler, document.ID)
		removed := latestEventOf(t, database, "artifact.removed")
		if rows := threadRowLogins(t, database, aliceThread); len(rows) != 0 {
			t.Fatalf("the removal left the rows %v of the document's thread", rows)
		}
		for kind, want := range map[string][]string{
			graphstore.TupleReadThrough: {
				fmt.Sprintf("person:alice > comment:%s @%d retracted", aliceThread, removed),
				fmt.Sprintf("person:bob > comment:%s @%d retracted", bobThread, removed),
			},
			graphstore.TupleDismissed: {
				fmt.Sprintf("person:alice > comment:%s @%d retracted", aliceThread, removed),
			},
		} {
			got, err := readStrings(ctx, database.Pool, `
				select from_kind || ':' || from_id || ' > ' || to_kind || ':' || to_id || ' @' || source_event::text
				       || case when asserted then '' else ' retracted' end
				from graph_tuples
				where kind = $1 and to_kind = 'comment' and to_id = any($2)
				order by 1
			`, kind, []string{aliceThread, bobThread})
			if err != nil {
				t.Fatalf("read the %s tuples of the removed document's threads: %v", kind, err)
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("the %s tuples of the removed document's threads are %v, want %v", kind, got, want)
			}
		}
	})

	t.Run("an Agent thread's dismissal and its restore are tuples to its root message", func(t *testing.T) {
		live := true
		sent := []map[string]any{}
		listener := sessionListener(t, &live, &sent)
		t.Cleanup(listener.Close)
		handler, database := newTargetedMessageHandler(t, listener.URL)
		root, _ := agentThread(t, handler, "steer", "Can you look?", "Looking.")
		node := graphstore.KindMessage + ":" + root.ID
		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissAgentPrefix+root.ID); dismissed != 1 {
			t.Fatalf("dismissed = %d, want alice's Agent thread", dismissed)
		}
		dismissEvent := newestEventOf(t, database, "user_agent_state.updated", "alice")
		requireMarkMatchesRow(t, database, graphstore.TupleDismissed, node, dismissEvent,
			storedMarks(t, database, `select dismissed_at, tuples_stored_at from user_agent_thread_dismissed where login = 'alice' and message_id = $1::uuid`, root.ID))
		if restored := restoreRows(t, handler, "alice", inboxDismissAgentPrefix+root.ID); restored != 1 {
			t.Fatalf("restored = %d, want alice's Agent thread", restored)
		}
		requireMarkHistory(t, database, graphstore.TupleDismissed, node,
			fmt.Sprintf("@%d", dismissEvent), fmt.Sprintf("@%d retracted", newestEventOf(t, database, "user_agent_state.updated", "alice")))
	})

	t.Run("a snooze carries its snoozed_until and an unsnooze retracts it", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "RSSN", "Snoozed", "A spec")
		ask := createPersonAsk(t, handler, issue.Key, "bob", "Should we ship?")
		until := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
		if snoozed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+ask.ID+"/snooze", map[string]any{
			"snoozed_until": until.Format(time.RFC3339),
		}, "alice"); snoozed.Code != http.StatusOK {
			t.Fatalf("snooze: status=%d body=%s", snoozed.Code, snoozed.Body.String())
		}
		node := graphstore.KindAsk + ":" + ask.ID
		snoozeEvent := newestEventOf(t, database, "user_state.updated", "alice")
		stored := storedMarks(t, database, `select snoozed_until, tuples_stored_at from user_ask_snooze where login = 'alice' and ask_id = $1::uuid`, ask.ID)
		requireMarkMatchesRow(t, database, graphstore.TupleSnoozed, node, snoozeEvent, stored)
		if !stored.Equal(until) {
			t.Errorf("the snooze is stored at %s, want snoozed_until %s, the moment its tuple carries", stored, until)
		}
		if unsnoozed := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/asks/"+ask.ID+"/snooze", nil, "alice"); unsnoozed.Code != http.StatusNoContent {
			t.Fatalf("unsnooze: status=%d body=%s", unsnoozed.Code, unsnoozed.Body.String())
		}
		requireMarkHistory(t, database, graphstore.TupleSnoozed, node,
			fmt.Sprintf("@%d", snoozeEvent), fmt.Sprintf("@%d retracted", newestEventOf(t, database, "user_state.updated", "alice")))
	})

	t.Run("every spelling of a login meets at one person node, and no other writer states a mark", func(t *testing.T) {
		handler, database, root, reply, _ := directConversationFrom(t, "alice")
		answer := decodeBody[model.Message](t, reply("On it."))
		// A Clear stored under a raw actor id - a row keyed as user_agent_state keys it (0033) - and
		// its tuple, which FromPerson states from the canonical login whatever spelling it is handed.
		writeInTx(t, database, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				insert into user_agent_state (login, session_id, cleared_before, tuples_stored_at)
				values ('  Alice ', 's1', $1, $1)
			`, answer.CreatedAt); err != nil {
				return err
			}
			raw := model.Actor{Kind: "user", ID: "  Alice "}
			return graphstore.FromPerson(ctx, tx, answer.CreatedAt, []graphstore.Tuple{
				personMark(raw, graphstore.TupleCleared, graphstore.KindSession, "s1", answer.CreatedAt, true, graphstore.NoEvent),
				personMark(model.Actor{Kind: "user", ID: "ALICE"}, graphstore.TupleReplyRead, graphstore.KindMessage, root.ID, answer.CreatedAt, true, graphstore.NoEvent),
			})
		})
		froms, err := readStrings(ctx, database.Pool, `
			select distinct from_kind || ':' || from_id from graph_tuples where kind = any($1) order by 1
		`, []string{graphstore.TupleCleared, graphstore.TupleReplyRead})
		if err != nil || len(froms) != 1 || froms[0] != "person:alice" {
			t.Fatalf("the marks run from %v (err %v), want person:alice alone", froms, err)
		}
		// The read finds the Clear at the person node, though the row's key is not the caller's
		// raw actor id: the two keys end there.
		if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 || got.ClearedBefore == nil ||
			!parseTimestamp(t, *got.ClearedBefore).Equal(answer.CreatedAt) {
			t.Errorf("alice reads %#v, want the Clear stated under '  Alice ' at %s and nothing unread", got, answer.CreatedAt)
		}
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{
			personMark(alice, graphstore.TupleDismissed, graphstore.KindMessage, root.ID, answer.CreatedAt, true, graphstore.NoEvent),
		}); !errors.Is(err, graphstore.ErrMarkOutsideFromPerson) {
			t.Fatalf("graphstore.Write of a dismissed from a person = %v, want ErrMarkOutsideFromPerson", err)
		}
	})

	t.Run("an older build's update of a marked row unmarks it, and this build's keeps it", func(t *testing.T) {
		handler, database, root, reply, _ := directConversationFrom(t, "alice")
		answer := decodeBody[model.Message](t, reply("On it."))
		putAgentState(t, handler, "alice", "s1", map[string]any{"cleared_before": answer.CreatedAt, "read_replies": []string{answer.ID}})
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_through": root.CreatedAt})
		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissAgentPrefix+root.ID); dismissed != 1 {
			t.Fatalf("dismissed = %d, want alice's Agent thread", dismissed)
		}
		issue := createInteractionIssue(t, handler, "RSMK", "Marked rows", "A spec")
		ask := createPersonAsk(t, handler, issue.Key, "bob", "Should we ship?")
		if snoozed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+ask.ID+"/snooze", map[string]any{
			"snoozed_until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		}, "alice"); snoozed.Code != http.StatusOK {
			t.Fatalf("snooze: status=%d body=%s", snoozed.Code, snoozed.Body.String())
		}
		older := olderBuild(t, database)
		for _, table := range []string{"user_agent_read", "user_agent_state", "user_agent_reply_read", "user_agent_thread_dismissed", "user_ask_snooze"} {
			touch := `update ` + table + ` set tuples_stored_at = tuples_stored_at where login = 'alice'`
			marked := func() bool {
				t.Helper()
				var unmarked, rows int
				if err := database.Pool.QueryRow(ctx, `select count(*) filter (where tuples_stored_at is null), count(*) from `+table+` where login = 'alice'`).
					Scan(&unmarked, &rows); err != nil || rows == 0 {
					t.Fatalf("read alice's rows of %s: %d rows, %v", table, rows, err)
				}
				return unmarked == 0
			}
			if !marked() {
				t.Fatalf("this build wrote alice's rows of %s unmarked, so this case would prove nothing", table)
			}
			if _, err := database.Pool.Exec(ctx, touch); err != nil {
				t.Fatalf("update %s through this build's pool: %v", table, err)
			}
			if !marked() {
				t.Errorf("an update of %s through this build's pool unmarked a row", table)
			}
			if _, err := older.Exec(ctx, touch); err != nil {
				t.Fatalf("update %s as an older build: %v", table, err)
			}
			var stillMarked int
			if err := database.Pool.QueryRow(ctx, `select count(*) from `+table+` where login = 'alice' and tuples_stored_at is not null`).Scan(&stillMarked); err != nil {
				t.Fatalf("read alice's rows of %s: %v", table, err)
			}
			if stillMarked != 0 {
				t.Errorf("an older build's update of %s left %d row marked, want each handed back to its old arm", table, stillMarked)
			}
		}
	})

	t.Run("an older build's prune of a reply read leaves its tuple standing and the reply read", func(t *testing.T) {
		handler, database, root, reply, _ := directConversationFrom(t, "alice")
		first := decodeBody[model.Message](t, reply("On it."))
		second := decodeBody[model.Message](t, reply("Done."))
		putAgentState(t, handler, "alice", "s1", map[string]any{"read_through": root.CreatedAt, "read_replies": []string{first.ID}})
		if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
			t.Fatalf("before the older build's write: %#v, want the second reply alone unread", got)
		}
		// The release before this one moves the read mark through the first reply and prunes the
		// row its mark now passes, by its own statements (main's putUserAgentState and
		// pruneAgentReplies), on a connection that carries no marker writer setting.
		older := olderBuild(t, database)
		if _, err := older.Exec(ctx, `
			insert into user_agent_read (login, session_id, read_through)
			values ('alice', 's1', least($1::timestamptz, now()))
			on conflict (login, session_id) do update set
				read_through = greatest(user_agent_read.read_through, excluded.read_through)
		`, first.CreatedAt); err != nil {
			t.Fatalf("move the read mark as an older build: %v", err)
		}
		if _, err := older.Exec(ctx, `
			with locked as materialized (
				select user_agent_reply_read.reply_id
				from user_agent_reply_read, messages, user_agent_read
				where user_agent_reply_read.login = $1 and user_agent_reply_read.session_id = any($2::text[])
				  and user_agent_read.login = $1 and user_agent_read.session_id = user_agent_reply_read.session_id
				  and messages.id = user_agent_reply_read.reply_id
				  and messages.created_at <= user_agent_read.read_through
				order by user_agent_reply_read.reply_id
				for no key update of user_agent_reply_read
			)
			delete from user_agent_reply_read
			using locked
			where user_agent_reply_read.login = $1 and user_agent_reply_read.reply_id = locked.reply_id
		`, "alice", []string{"s1"}); err != nil {
			t.Fatalf("prune as an older build: %v", err)
		}
		var rows int
		if err := database.Pool.QueryRow(ctx, `select count(*) from user_agent_reply_read where login = 'alice'`).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("alice holds %d reply read rows after the older build's prune (err %v), want none", rows, err)
		}
		requireMarkHistory(t, database, graphstore.TupleReplyRead, graphstore.KindMessage+":"+first.ID,
			fmt.Sprintf("@%d", firstEventOf(t, database, "user_agent_state.updated", "alice")))
		// The tuple left standing says the first reply was read by its id, and the read mark the
		// older build moved, which its row now answers unmarked, passes it: the reply is read either
		// way, so the count is the second reply alone, as the rows would have it, and the read mark
		// is the older build's.
		got := unreadReplies(t, handler, "alice", "s1")
		if got.UnreadReplies != 1 || got.ReadThrough == nil || !parseTimestamp(t, *got.ReadThrough).Equal(first.CreatedAt) {
			t.Errorf("alice reads %#v, want the second reply %s alone unread and the read mark at %s", got, second.ID, first.CreatedAt)
		}
	})
}

// storedMark is a read-state row's mark as the row holds it, beside the marker it carries.
type storedMark struct {
	at, marker *time.Time
}

// Equal is whether the row's mark is at.
func (mark storedMark) Equal(at time.Time) bool { return mark.at != nil && mark.at.Equal(at) }

func (mark storedMark) String() string { return fmt.Sprintf("%v (marked %v)", mark.at, mark.marker) }

// storedMarks reads one row's mark and its marker with sql, which answers the two in that order.
func storedMarks(t *testing.T, database *store.Store, sql string, args ...any) storedMark {
	t.Helper()
	var mark storedMark
	if err := database.Pool.QueryRow(context.Background(), sql, args...).Scan(&mark.at, &mark.marker); err != nil {
		t.Fatalf("read a read-state row: %v", err)
	}
	return mark
}

// threadRowMark reads column of alice's user_threads row on threadID, whose marker is its one
// writer's (marked_seq), so the row's mark is held to the tuple alone.
func threadRowMark(t *testing.T, database *store.Store, column, threadID string) storedMark {
	t.Helper()
	return storedMarks(t, database, `select `+column+`, `+column+` from user_threads where login = 'alice' and thread_id = $1::uuid`, threadID)
}

// requireMarkMatchesRow fails unless alice's newest tuple of kind to node asserts, was caused by
// event and stands at the mark the row holds, and unless the row is marked at that same moment.
func requireMarkMatchesRow(t *testing.T, database *store.Store, kind, node string, event int64, row storedMark) {
	t.Helper()
	var at time.Time
	var cause int64
	var asserted bool
	if err := database.Pool.QueryRow(context.Background(), `
		select at, source_event, asserted from graph_tuples
		where from_kind = $1 and from_id = 'alice' and kind = $2 and to_kind || ':' || to_id = $3
		order by seq desc limit 1
	`, graphstore.KindPerson, kind, node).Scan(&at, &cause, &asserted); err != nil {
		t.Fatalf("read alice's %s to %s: %v", kind, node, err)
	}
	if !asserted || cause != event {
		t.Errorf("alice's newest %s to %s asserts %t under event %d, want it asserted under %d", kind, node, asserted, cause, event)
	}
	if !row.Equal(at) {
		t.Errorf("alice's %s to %s stands at %s, and the row holds %s", kind, node, at, row)
	}
	if row.marker == nil || !row.marker.Equal(at) {
		t.Errorf("the row of alice's %s to %s is marked %v, want it marked at its tuple's moment %s", kind, node, row.marker, at)
	}
}

// requireMarkHistory fails unless alice's tuples of kind to node, in the order they were written,
// are want, each "@<source event>" with " retracted" after a retraction (graphstore.History).
func requireMarkHistory(t *testing.T, database *store.Store, kind, node string, want ...string) {
	t.Helper()
	history, err := graphstore.History(context.Background(), database.Pool, graphstore.KindPerson, "alice", kind)
	if err != nil {
		t.Fatalf("read alice's history of %s: %v", kind, err)
	}
	var got []string
	for _, tuple := range history {
		if tuple.ToKind+":"+tuple.ToID != node {
			continue
		}
		entry := fmt.Sprintf("@%d", tuple.SourceEvent)
		if !tuple.Asserted {
			entry += " retracted"
		}
		got = append(got, entry)
	}
	if !slices.Equal(got, want) {
		t.Errorf("alice's history of %s to %s is %v, want %v", kind, node, got, want)
	}
}

// firstEventOf is the oldest event of eventType whose payload names login.
func firstEventOf(t *testing.T, database *store.Store, eventType, login string) int64 {
	t.Helper()
	var event int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(min(id), 0) from events where type = $1 and payload->>'login' = $2
	`, eventType, login).Scan(&event); err != nil || event == 0 {
		t.Fatalf("read the first %s of %s: event %d, %v", eventType, login, event, err)
	}
	return event
}
