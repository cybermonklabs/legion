//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's tests read the
// base release at $TMPDIR/base-dispatch (baseRelease), built from BASELINE_SHA, and are run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^(TestTheBaseReleasesRestoreAndASessionsReplyBothLandWhileThisHeadServes|TestTheBaseReleasesRestoreAndThisHeadsSessionReplyBothLandWhileThisHeadServes)$' -count=1 -v

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// While this head rolls, the release before serves beside it over the database this head migrated,
// where the trigger of 20261125053125 mirrors each delete of a mark of user_agent_thread_dismissed
// onto the mark's root (messages.agent_dismissed_at). That release's restore (POST
// /api/v1/me/inbox/dismiss with restore) takes the caller's marks and deletes them and takes no
// root, so under the mirror it holds the mark when it reaches the root. A session's reply into the
// thread takes the root before the marks it clears: the clear trigger 20261125053125 rewrote takes
// it `for no key update` for a reply the release before writes, and Recording.QueueRootLock for one
// this head writes. The restore and the reply then wait on each other, and Postgres breaks the cycle
// by failing one of them (40P01): a person's restore that answers 500, or a session's reply lost.
//
// The test widens the restore's window between its mark and the root with a trigger that pauses
// two seconds before each delete of a mark, which changes no lock either write takes or the order
// it takes them in. Seven tenths of a second into that pause it writes the session's reply as the
// release before writes one, and it requires both writes to land, and the thread back in this
// head's Inbox with no mark on its root.
func TestTheBaseReleasesRestoreAndASessionsReplyBothLandWhileThisHeadServes(t *testing.T) {
	ctx := context.Background()
	databaseURL, previous, current, database := serveBothBuilds(t, ctx)

	// One one-off message of alice's to a session, with the session's reply, written as the base
	// release writes them, and dismissed through the base release's own route.
	var root string
	if err := database.QueryRow(ctx, `
		insert into messages (author, body, target, created_at)
		values ('{"kind":"user","id":"alice"}', 'Can you look at the deploy?', 'session:s1', now() - interval '10 minutes')
		returning id::text
	`).Scan(&root); err != nil {
		t.Fatalf("write alice's message to s1: %v", err)
	}
	reply := sessionReplyAsTheBaseWritesIt(t, ctx, database, root)
	dismissAsTheBase(t, previous, root, reply)
	requireRootColumnIsAlicesMark(t, ctx, database, root, "the base release's dismissal while both serve")

	dropPause := pauseEachMarkDelete(t, ctx, database)

	replier, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect the session's reply: %v", err)
	}
	t.Cleanup(func() { replier.Close(ctx) })
	replied := make(chan error, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		_, err := replier.Exec(ctx, `
			insert into messages (author, body, in_reply_to, target)
			select '{"kind":"session","id":"s1"}', 'Still looking.', parent.id, parent.target
			from messages parent where parent.id = $1
		`, reply)
		replied <- err
	}()
	restored, err := previous.trySend(http.MethodPost, "/api/v1/me/inbox/dismiss", encodeJSON(t, map[string]any{
		"threads": []string{"agent:" + root}, "restore": true,
	}), asUser("alice"))
	replyErr := <-replied
	dropPause()
	if err != nil {
		t.Fatalf("the base release's restore of %s: %v", root, err)
	}
	if restored.status != http.StatusOK || replyErr != nil {
		t.Fatalf("while this head serves, the base release's restore of %s answered %s and the session's reply into that thread %v, want both to land: the restore holds alice's mark when the mirror of 20261125053125 updates the root, and the reply holds the root when it deletes that mark", root, restored, replyErr)
	}
	requireRootColumnIsAlicesMark(t, ctx, database, root, "the base release's restore and the session's reply")
	requireInbox(t, current, []string{root}, nil)
}

// The same race with this head's session reply in place of the release before's: the reply is
// POST /api/v1/messages/{id}/reply to this head, whose writer takes the root `for no key update`
// before its insert (inboxwrite's Recording.QueueRootLock) and deletes the root's marks and then
// settles the root after it (queueAgentDismissalsClear). The root and its attempt to s1 are
// written as the base release writes them, and the reply answers that attempt. The base release's
// restore holds alice's mark when its delete's trigger reaches the root the reply holds, and the
// reply holds the root when its delete reaches that mark; the trigger owes the root rather than
// wait, the restore commits, and the reply's settle clears the column. It runs as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestTheBaseReleasesRestoreAndThisHeadsSessionReplyBothLandWhileThisHeadServes$' -count=1 -v
func TestTheBaseReleasesRestoreAndThisHeadsSessionReplyBothLandWhileThisHeadServes(t *testing.T) {
	ctx := context.Background()
	_, previous, current, database := serveBothBuilds(t, ctx)

	// One one-off message of alice's to a session, its attempt to s1 and the session's reply,
	// written as the base release writes them, and dismissed through the base release's own route.
	var root string
	if err := database.QueryRow(ctx, `
		with root as (
			insert into messages (author, body, target, created_at)
			values ('{"kind":"user","id":"alice"}', 'Can you look at the deploy?', 'session:s1', now() - interval '10 minutes')
			returning id
		), attempt as (
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
			select id, 1, 'btw', 's1', 'sent', '{"kind":"user","id":"alice"}' from root
		)
		select id::text from root
	`).Scan(&root); err != nil {
		t.Fatalf("write alice's message to s1 and its attempt: %v", err)
	}
	reply := sessionReplyAsTheBaseWritesIt(t, ctx, database, root)
	dismissAsTheBase(t, previous, root, reply)
	requireRootColumnIsAlicesMark(t, ctx, database, root, "the base release's dismissal while both serve")

	dropPause := pauseEachMarkDelete(t, ctx, database)
	body := encodeJSON(t, map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Still looking.",
	})
	type answered struct {
		answer httpAnswer
		err    error
	}
	replied := make(chan answered, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		answer, err := current.trySend(http.MethodPost, "/api/v1/messages/"+root+"/reply", body, agentBearer())
		replied <- answered{answer, err}
	}()
	restored, err := previous.trySend(http.MethodPost, "/api/v1/me/inbox/dismiss", encodeJSON(t, map[string]any{
		"threads": []string{"agent:" + root}, "restore": true,
	}), asUser("alice"))
	sessionReply := <-replied
	dropPause()
	if err != nil {
		t.Fatalf("the base release's restore of %s: %v", root, err)
	}
	if sessionReply.err != nil {
		t.Fatalf("this head's session reply into %s: %v", root, sessionReply.err)
	}
	if restored.status != http.StatusOK || sessionReply.answer.status != http.StatusCreated {
		t.Fatalf("while this head serves, the base release's restore of %s answered %s and this head's session reply into that thread %s, want both to land: the restore holds alice's mark when the trigger of 20261125053125 reaches the root, and the reply holds the root when it deletes that mark", root, restored, sessionReply.answer)
	}
	requireRootColumnIsAlicesMark(t, ctx, database, root, "the base release's restore and this head's session reply")
	requireInbox(t, current, []string{root}, nil)
}

// serveBothBuilds boots the base release and then this head over a run's database of its own, as a
// roll has both serve it, and connects to that database. It answers the database's URL, the two
// processes and the connection, which the test's cleanup closes.
func serveBothBuilds(t *testing.T, ctx context.Context) (string, *dispatchProcess, *dispatchProcess, *pgx.Conn) {
	t.Helper()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	head := cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp)

	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey})
	previous.waitHealthy(t)
	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey})
	current.waitHealthy(t)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	t.Cleanup(func() { database.Close(ctx) })
	return databaseURL, previous, current, database
}

// pauseEachMarkDelete widens a restore's window between its mark and the root with a trigger that
// pauses two seconds before each delete of a mark, which changes no lock either write takes or the
// order it takes them in, and answers the drop of that trigger, which the test's cleanup runs too.
func pauseEachMarkDelete(t *testing.T, ctx context.Context, database *pgx.Conn) func() {
	t.Helper()
	drop := func() {
		if _, err := database.Exec(ctx, `
			drop trigger if exists tester_pause_before_a_marks_delete on user_agent_thread_dismissed;
			drop function if exists tester_pause_before_a_marks_delete();
		`); err != nil {
			t.Errorf("drop the pause before a mark's delete: %v", err)
		}
	}
	if _, err := database.Exec(ctx, `
		create function tester_pause_before_a_marks_delete() returns trigger language plpgsql as $$
		begin
		  perform pg_sleep(2);
		  return old;
		end;
		$$;
		create trigger tester_pause_before_a_marks_delete before delete on user_agent_thread_dismissed
		  for each row execute function tester_pause_before_a_marks_delete();
	`); err != nil {
		t.Fatalf("pause each delete of a mark: %v", err)
	}
	t.Cleanup(drop)
	return drop
}
