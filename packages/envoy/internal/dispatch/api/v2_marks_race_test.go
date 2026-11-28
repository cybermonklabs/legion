package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestV2BulkMarksOfTwoPeopleAtOnceBothLand: two people's writes of their marks under PATCH
// /api/v2/me/marks, lined up so that each could hold a lock the other waits for, both land. Two
// people's writes share no mark row and take no marks lock of each other's (lockPersonMarks), so
// what orders them is the owners of their threads and the projects their writes and events name,
// which writeMarksIn takes before the first item write, each set in one sorted order. Each row is
// one way two such writes could cross where those locks were taken as each item reached them:
//
//   - two issue marks each, in the two projects in opposite orders: each write takes its first
//     item's project as it writes it (writeUserIssueStateIn), and would wait for the second while
//     the other held it;
//   - one read mark each, of a thread both people are in: a read mark takes its thread's owner
//     exclusively (moveThreadReadIn), and two writes that each held that owner in shared mode first
//     would each wait for the other's shared hold to end;
//   - an issue mark in one project and an ask snooze in the other, or a dismissal of a thread in
//     the other: the snooze and the dismissal take their project only with the events, after the
//     issue mark took its own, so each write would hold one project and wait for the other's.
//
// The crossing is made rather than waited for. A transaction of the test's own holds every owner
// the row names; the first person's write queues behind it, then the second's, each watched by
// the backend holding that person's marks lock (awaitMarksWrite); and the hold ends at once, so
// each write is given the first lock it waits for and goes on to its next. Where the two take
// their locks in one order they queue at the same first lock and one follows the other; where
// they do not, Postgres breaks the cycle with a 40P01 that its caller answers 500.
func TestV2BulkMarksOfTwoPeopleAtOnceBothLand(t *testing.T) {
	t.Parallel()
	// No settlement runs during the test, so nothing but the two writes queues on what a row
	// holds, and the writes' statements are bounded past any run, since each waits behind that
	// hold for as long as a loaded machine takes to queue the other.
	handler, database, _ := newTestServer(t, testServerOptions{settle: pastTheRun, inboxStatementBound: pastTheRun})
	left := createInteractionIssue(t, handler, "LEFT", "Marks on the left", "A spec").Key
	right := createInteractionIssue(t, handler, "RIGHT", "Marks on the right", "A spec").Key
	leftAsk := createAnswerListAsk(t, handler, left, "Which way, on the left?").ID
	rightAsk := createAnswerListAsk(t, handler, right, "Which way, on the right?").ID
	// A thread both people wrote in, and so both have a user_threads row on.
	shared := createThreadComment(t, handler, left, map[string]any{"body": "Ready to ship?"}, "alice").ID
	createThreadComment(t, handler, left, map[string]any{"body": "Nearly.", "reply_to": shared}, "bob")
	// A thread each person alone wrote in, in the project the other's issue mark names.
	aliceOnTheRight := createThreadComment(t, handler, right, map[string]any{"body": "Is the runbook current?"}, "alice").ID
	bobOnTheLeft := createThreadComment(t, handler, left, map[string]any{"body": "Who reviews this?"}, "bob").ID

	issue := func(key string) map[string]any { return map[string]any{"kind": "issue", "id": key, "pinned": true} }
	snooze := func(ask string) map[string]any {
		return map[string]any{"kind": "ask", "id": ask, "snoozed_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	}
	read := func(thread string) map[string]any {
		return map[string]any{"kind": "thread", "id": thread, "read_through": time.Now().UTC().Format(time.RFC3339Nano)}
	}
	dismiss := func(t *testing.T, login, thread string) map[string]any {
		t.Helper()
		var version int64
		if err := database.Pool.QueryRow(context.Background(), `
			select version from user_threads where login = $1 and thread_id = $2
		`, login, thread).Scan(&version); err != nil {
			t.Fatalf("read %s's version of thread %s: %v", login, thread, err)
		}
		return map[string]any{"kind": "thread", "id": thread, "dismissed": true, "version": version}
	}
	projects := []store.Owner{store.ProjectOwner("LEFT"), store.ProjectOwner("RIGHT")}
	for _, row := range []struct {
		name string
		// held is what the test's own transaction holds while both writes queue behind it.
		held []store.Owner
		// alice and bob are each person's items, built when the row runs.
		alice, bob func(t *testing.T) []map[string]any
		// updated is how many marks each write answers it changed.
		updated int
	}{
		{
			name: "issue marks in two projects in opposite orders",
			held: projects,
			alice: func(*testing.T) []map[string]any {
				return []map[string]any{issue(left), issue(right)}
			},
			bob: func(*testing.T) []map[string]any {
				return []map[string]any{issue(right), issue(left)}
			},
			updated: 2,
		},
		{
			name: "read marks of one thread both people are in",
			held: []store.Owner{store.IssueOwner(left)},
			alice: func(*testing.T) []map[string]any {
				return []map[string]any{read(shared)}
			},
			bob: func(*testing.T) []map[string]any {
				return []map[string]any{read(shared)}
			},
			updated: 1,
		},
		{
			name: "an issue mark and an ask snooze in the other's project",
			held: projects,
			alice: func(*testing.T) []map[string]any {
				return []map[string]any{issue(left), snooze(rightAsk)}
			},
			bob: func(*testing.T) []map[string]any {
				return []map[string]any{issue(right), snooze(leftAsk)}
			},
			updated: 2,
		},
		{
			name: "an issue mark and a thread dismissal in the other's project",
			held: projects,
			alice: func(t *testing.T) []map[string]any {
				return []map[string]any{issue(left), dismiss(t, "alice", aliceOnTheRight)}
			},
			bob: func(t *testing.T) []map[string]any {
				return []map[string]any{issue(right), dismiss(t, "bob", bobOnTheLeft)}
			},
			updated: 2,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			hold := beginGateHold(t, database, "the owners both writes take")
			if err := store.LockOwners(context.Background(), hold.tx, row.held...); err != nil {
				t.Fatalf("hold %v: %v", row.held, err)
			}
			logins := []string{"alice", "bob"}
			answers := make([]<-chan *httptest.ResponseRecorder, 0, len(logins))
			for index, items := range []func(t *testing.T) []map[string]any{row.alice, row.bob} {
				answered := serveMarksWrite(t, handler, logins[index], items(t))
				if early := awaitMarksWrite(t, hold.tx, logins[index], answered); early != nil {
					t.Fatalf("%s's write answered %d %s before it queued behind the held owners", logins[index], early.Code, early.Body.String())
				}
				answers = append(answers, answered)
			}
			hold.release()
			for index, answered := range answers {
				response := awaitResponse(t, answered)
				if response.Code != http.StatusOK {
					t.Errorf("%s's write, sent beside the other's, answered %d %s, want 200", logins[index], response.Code, response.Body.String())
					continue
				}
				if updated := decodeBody[markUpdatesV2](t, response).Updated; updated != row.updated {
					t.Errorf("%s's write changed %d marks, want %d", logins[index], updated, row.updated)
				}
			}
		})
	}
}

// TestAV2ReadMarkOfAThreadWhoseRowLandsAfterItsWriteTookItsThreadsWritesNothing: a read mark under
// PATCH /api/v2/me/marks of a thread the caller had no row for when the write took its threads
// (lockMarkThreads) writes nothing, as one of a thread the caller has no row for does, even where
// the caller's row lands before the read mark's turn comes. By then the write holds its projects
// (lockMarkProjects), and taking that thread's owner there would wait for a writer of that owner
// that may itself wait for one of those projects.
//
// The window is made rather than waited for. A transaction of the test's own holds the project of
// Alice's issue mark, so her write queues there having taken its threads, and found no row of hers
// on the thread her read mark names. Her reply in that thread then commits, which writes her row
// on it (user_threads_record), and a second transaction of the test's, a writer of that thread's
// owner, takes the owner and keeps it. Once the project is let go, a write that went on to the
// thread's owner would queue behind that writer while holding the project; the writer then asks
// for the project, and Postgres breaks the cycle with a 40P01. The write must answer instead, its
// issue mark written and its read mark not.
func TestAV2ReadMarkOfAThreadWhoseRowLandsAfterItsWriteTookItsThreadsWritesNothing(t *testing.T) {
	t.Parallel()
	// As in TestV2BulkMarksOfTwoPeopleAtOnceBothLand: nothing but the write queues on what the
	// test holds, and its statements are bounded past any run.
	handler, database, _ := newTestServer(t, testServerOptions{settle: pastTheRun, inboxStatementBound: pastTheRun})
	left := createInteractionIssue(t, handler, "LEFT", "Marks on the left", "A spec").Key
	right := createInteractionIssue(t, handler, "RIGHT", "A thread on the right", "A spec").Key
	// A thread Bob wrote and Alice has not, so she has no row on it.
	thread := createThreadComment(t, handler, right, map[string]any{"body": "Who reviews this?"}, "bob").ID
	ctx := context.Background()

	project := beginGateHold(t, database, "the issue mark's project")
	if err := store.LockOwner(ctx, project.tx, store.ProjectOwner("LEFT")); err != nil {
		t.Fatalf("hold LEFT's owner lock: %v", err)
	}
	answered := serveMarksWrite(t, handler, "alice", []map[string]any{
		{"kind": "issue", "id": left, "pinned": true},
		{"kind": "thread", "id": thread, "read_through": time.Now().UTC().Format(time.RFC3339Nano)},
	})
	if early := awaitMarksWrite(t, project.tx, "alice", answered); early != nil {
		t.Fatalf("alice's write answered %d %s before it queued behind LEFT's owner lock", early.Code, early.Body.String())
	}
	// Her reply writes her row on the thread and commits: the write she has queued holds neither
	// the thread's owner nor RIGHT.
	createThreadComment(t, handler, right, map[string]any{"body": "I will.", "reply_to": thread}, "alice")
	owner := beginGateHold(t, database, "the thread's owner")
	if err := store.LockOwner(ctx, owner.tx, store.IssueOwner(right)); err != nil {
		t.Fatalf("hold %s's owner lock: %v", right, err)
	}
	project.release()
	response := awaitMarksWrite(t, owner.tx, "alice", answered)
	if response == nil {
		// The write waits for the thread's owner, holding LEFT. The owner's writer asks for LEFT
		// and the two meet; the writer is let go before the write's answer is read, so a write
		// the cycle left waiting behind a writer Postgres ended can answer.
		err := store.LockOwner(ctx, owner.tx, store.ProjectOwner("LEFT"))
		owner.release()
		response = awaitResponse(t, answered)
		t.Fatalf("alice's write waited for the owner of a thread it never took, holding LEFT: the owner's writer then "+
			"asking for LEFT answered %v, and the write %d %s", err, response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("alice's write answered %d %s, want 200", response.Code, response.Body.String())
	}
	if updated := decodeBody[markUpdatesV2](t, response).Updated; updated != 1 {
		t.Fatalf("alice's write changed %d marks, want 1: the issue mark, and no read mark on a thread it never took", updated)
	}
}

// TestAV2MessageDismissalOfAThreadBackInTheInboxAfterItsWriteTookItsRootsWritesNothing: a
// dismissal of an Agent thread under PATCH /api/v2/me/marks whose root was out of the caller's
// Inbox when the write took its roots (lockMarkThreads) writes nothing, even where the thread is
// back in the Inbox before the dismissals run. By then the write holds the caller's user_threads
// rows of the threads it names, and the Inbox's lock order takes a root before those rows
// (markInboxReadThreads).
//
// The window is made rather than waited for. A transaction of the test's own holds the owner of a
// thread Alice's write also dismisses, so her write queues there having read her Agent thread as
// dismissed. A session's reply in that thread then commits, whose trigger clears her dismissal
// (user_agent_thread_dismissed_clear), and a second transaction of the test's takes the root as
// that trigger takes it, `for key share`, and keeps it. Once the owner is let go, a write that
// went on to take the root `for update` would queue behind that hold while holding her thread's
// row; the holder, taking the two in the Inbox's order, then asks for that row, and Postgres
// breaks the cycle with a 40P01. The write must answer instead, its thread dismissed and its Agent
// thread not.
func TestAV2MessageDismissalOfAThreadBackInTheInboxAfterItsWriteTookItsRootsWritesNothing(t *testing.T) {
	t.Parallel()
	// A server whose session s1 is live, so the Agent thread's message reaches it and its replies
	// come back, as seedInboxRead builds one; and, as in TestV2BulkMarksOfTwoPeopleAtOnceBothLand,
	// nothing but the write queues on what the test holds, and its statements are bounded past any
	// run.
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database, _ := newTestServer(t, testServerOptions{settle: pastTheRun, inboxStatementBound: pastTheRun, envoyURL: listener.URL})
	left := createInteractionIssue(t, handler, "LEFT", "Marks on the left", "A spec").Key
	thread := createThreadComment(t, handler, left, map[string]any{"body": "Ready to ship?"}, "alice").ID
	root, reply := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
	if dismissed := dismissRows(t, handler, database, "alice", inboxDismissAgentPrefix+root.ID); dismissed != 1 {
		t.Fatalf("alice's dismissal of her Agent thread changed %d threads, want 1", dismissed)
	}
	ctx := context.Background()
	var version int64
	if err := database.Pool.QueryRow(ctx, `
		select version from user_threads where login = 'alice' and thread_id = $1
	`, thread).Scan(&version); err != nil {
		t.Fatalf("read alice's version of thread %s: %v", thread, err)
	}

	owner := beginGateHold(t, database, "the thread's owner")
	if err := store.LockOwner(ctx, owner.tx, store.IssueOwner(left)); err != nil {
		t.Fatalf("hold %s's owner lock: %v", left, err)
	}
	answered := serveMarksWrite(t, handler, "alice", []map[string]any{
		{"kind": "thread", "id": thread, "dismissed": true, "version": version},
		{"kind": "message", "id": root.ID, "dismissed": true, "newest_reply": reply.ID, "unread_replies": []string{}},
	})
	if early := awaitMarksWrite(t, owner.tx, "alice", answered); early != nil {
		t.Fatalf("alice's write answered %d %s before it queued behind %s's owner lock", early.Code, early.Body.String(), left)
	}
	// The session's reply clears her dismissal and commits: the write she has queued holds neither
	// the root nor her dismissal's row.
	if replied := replyTo(t, handler, root.ID, "One more thing."); replied.Code != http.StatusCreated {
		t.Fatalf("s1's reply under %s answered %d %s, want 201", root.ID, replied.Code, replied.Body.String())
	}
	held := beginGateHold(t, database, "the Agent thread's root")
	if _, err := held.tx.Exec(ctx, `select 1 from messages where id = $1 for key share`, root.ID); err != nil {
		t.Fatalf("hold the root %s: %v", root.ID, err)
	}
	owner.release()
	response := awaitMarksWrite(t, held.tx, "alice", answered)
	if response == nil {
		// The write waits for the root, holding her thread's row. The root's holder asks for that
		// row and the two meet; the holder is let go before the write's answer is read, so a write
		// the cycle left waiting behind a holder Postgres ended can answer.
		_, err := held.tx.Exec(ctx, `
			select 1 from user_threads where login = 'alice' and thread_id = $1 for no key update
		`, thread)
		held.release()
		response = awaitResponse(t, answered)
		t.Fatalf("alice's write waited for the root of an Agent thread it never took, holding her row of %s: the root's holder "+
			"then asking for that row answered %v, and the write %d %s", thread, err, response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("alice's write answered %d %s, want 200", response.Code, response.Body.String())
	}
	if updated := decodeBody[markUpdatesV2](t, response).Updated; updated != 1 {
		t.Fatalf("alice's write changed %d marks, want 1: the thread's dismissal, and no dismissal of an Agent thread it never took", updated)
	}
}

// TestAV1SnoozeAndAV2AskMarkOfOnePersonAtOnceBothLand: one person's snooze of an ask under PUT
// /api/v1/me/asks/{id}/snooze and the same person's mark of that ask under PATCH /api/v2/me/marks,
// lined up so that each could hold a lock the other waits for, both land. The snooze writes one
// mark row and takes no marks lock (lockPersonMarks), so that lock does not order the two. Each
// writes the person's user_ask_snooze row of the ask and appends its event to the ask's project:
// the mark takes the project before its item writes (lockMarkProjects), and the snooze takes it
// before its row (snoozeAskIn). A snooze that wrote its row first and took the project only with
// its event would hold the row the mark waits for while it waits for the project the mark holds.
//
// The crossing is made rather than waited for, as in TestV2BulkMarksOfTwoPeopleAtOnceBothLand. A
// transaction of the test's own holds the ask's project; the mark queues behind it, watched by the
// backend holding the person's marks lock (awaitMarksWrite), then the snooze; and the hold ends at
// once, so the mark is given the project first and goes on to the row. Where the snooze wrote the
// row before it queued, Postgres breaks the cycle with a 40P01 that its caller answers 500.
func TestAV1SnoozeAndAV2AskMarkOfOnePersonAtOnceBothLand(t *testing.T) {
	t.Parallel()
	// As in TestV2BulkMarksOfTwoPeopleAtOnceBothLand: nothing but the two writes queues on what the
	// test holds, and the mark's statements are bounded past any run.
	handler, database, _ := newTestServer(t, testServerOptions{settle: pastTheRun, inboxStatementBound: pastTheRun})
	issue := createInteractionIssue(t, handler, "LEFT", "Marks on the left", "A spec").Key
	ask := createAnswerListAsk(t, handler, issue, "Which way?").ID
	until := func() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }
	ctx := context.Background()

	hold := beginGateHold(t, database, "the ask's project")
	if err := store.LockOwner(ctx, hold.tx, store.ProjectOwner("LEFT")); err != nil {
		t.Fatalf("hold LEFT's owner lock: %v", err)
	}
	marked := serveMarksWrite(t, handler, "alice", []map[string]any{{"kind": "ask", "id": ask, "snoozed_until": until()}})
	if early := awaitMarksWrite(t, hold.tx, "alice", marked); early != nil {
		t.Fatalf("alice's mark answered %d %s before it queued behind LEFT's owner lock", early.Code, early.Body.String())
	}
	snoozed := serveRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+ask+"/snooze", map[string]any{"snoozed_until": until()}, "alice")
	// The snooze takes no marks lock to be watched by. It is the second backend to queue behind the
	// hold, since nothing but the two writes queues there.
	type seen struct {
		early   *httptest.ResponseRecorder
		waiting int
	}
	queued := testwait.Until(t, "alice's snooze to queue behind LEFT's owner lock after her mark, or to answer (the backends queued there last read)", func() (seen, bool) {
		select {
		case early := <-snoozed:
			return seen{early: early}, true
		default:
		}
		waiting, err := storetest.LockWaiters(ctx, hold.tx)
		if err != nil {
			t.Fatal(err)
		}
		return seen{waiting: waiting}, waiting >= 2
	})
	if queued.early != nil {
		t.Fatalf("alice's snooze answered %d %s before it queued behind LEFT's owner lock", queued.early.Code, queued.early.Body.String())
	}
	hold.release()
	for _, write := range []struct {
		name     string
		answered <-chan *httptest.ResponseRecorder
	}{{"mark under /api/v2", marked}, {"snooze under /api/v1", snoozed}} {
		if response := awaitResponse(t, write.answered); response.Code != http.StatusOK {
			t.Errorf("alice's %s, sent beside the other, answered %d %s, want 200", write.name, response.Code, response.Body.String())
		}
	}
}

// serveMarksWrite sends login's PATCH /api/v2/me/marks of items and answers the channel its
// answer lands on (serveRequest).
func serveMarksWrite(t *testing.T, handler http.Handler, login string, items []map[string]any) <-chan *httptest.ResponseRecorder {
	t.Helper()
	return serveRequest(t, handler, http.MethodPatch, "/api/v2/me/marks", map[string]any{"items": items}, login)
}

// serveRequest sends login's request of method on target, with body, and answers the channel its
// answer lands on. The request is built on the test's goroutine, which alone may fail the test,
// and served on a goroutine of its own.
func serveRequest(t *testing.T, handler http.Handler, method, target string, body any, login string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	request := newDispatchRequest(t, method, target, body, login)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		answered <- response
	}()
	return answered
}

// awaitMarksWrite waits for login's write of their marks to queue behind a lock holder's
// transaction owns, or to answer, and answers the write's answer where that came first and nil
// where the write queued. The write is the backend that holds login's marks lock, which such a
// write takes first (lockPersonMarks), and it has queued once it waits on the holder directly or
// behind an earlier waiter (storetest.QueuedBehind). Watching that backend tells the write apart
// from any other session queued on the same owners. It reads pg_locks through holder's own
// connection, as storetest's waits do.
func awaitMarksWrite(t *testing.T, holder pgx.Tx, login string, answered <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	return testwait.UntilPolling(t, 5*time.Millisecond, login+"'s write of marks to queue behind the held lock, or to answer", func() (*httptest.ResponseRecorder, bool) {
		select {
		case early := <-answered:
			return early, true
		default:
		}
		var pid uint32
		err := holder.QueryRow(ctx, `
			select pid from pg_locks
			where granted and locktype = 'advisory' and objsubid = 2
			  and database = (select oid from pg_database where datname = current_database())
			  and classid = ($1::bigint & 4294967295)::oid
			  and objid = (hashtext($2)::bigint & 4294967295)::oid
		`, int64(personMarksLockNamespace), login).Scan(&pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false
		}
		if err != nil {
			t.Fatalf("read the backend holding %s's marks lock: %v", login, err)
		}
		queued, err := storetest.QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatal(err)
		}
		return nil, queued
	})
}
