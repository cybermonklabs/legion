package api

// The Agent thread's root carries its author's dismissal (messages.agent_dismissed_at, migration
// 20261125052902), which the trigger of 20261125053125 moves in the statement that writes each
// mark, so every writer of a mark - a dismissal, a restore and a session's reply that clears its
// root's marks - updates the root's row. One lock order holds them: the root first, `for no key
// update` or stronger, then the mark (the order written on api/inbox_read.go's
// markInboxReadThreads). The windows below are made as api/inbox_dismiss_race_test.go makes its
// own: a transaction of the test's holds a row a request's order takes, the request queues behind
// it holding everything before that step, and the test sends the other writer into the window and
// reads where it waits.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// agentRootLock is the statement a session's reply takes its thread's root with, before its
// attempt and its insert (inboxwrite.Recording.QueueRootLock), as pg_stat_activity shows it.
const agentRootLock = `select 1 from messages where id = $1::uuid for no key update`

// agentRootDismissedAt is the root's own mark of its author's dismissal, nil where it holds none.
func agentRootDismissedAt(t *testing.T, database *store.Store, root string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(), `select agent_dismissed_at from messages where id = $1`, root).Scan(&at); err != nil {
		t.Fatalf("read the agent_dismissed_at of %s: %v", root, err)
	}
	return at
}

// requireRootMirrorsTheMark fails unless the root's column holds what its author's mark holds:
// the mark's moment, or nothing where no mark stands.
func requireRootMirrorsTheMark(t *testing.T, database *store.Store, root, what string) {
	t.Helper()
	mark, column := agentThreadDismissal(t, database, "alice", root), agentRootDismissedAt(t, database, root)
	if (mark == nil) != (column == nil) || (mark != nil && !mark.Equal(*column)) {
		t.Fatalf("%s: the root %s holds agent_dismissed_at %v, and alice's mark is %v", what, root, column, mark)
	}
}

// heldIn is how many table locks of the given modes the backend pid holds on table. A row lock
// takes the row share mode on its table and a write the row exclusive mode, each once the
// statement has run or been parsed (parseTimeMode), so a backend holding neither on a table has
// locked no row of it and written none.
func heldIn(t *testing.T, holder queryer, pid uint32, table string, modes ...string) int {
	t.Helper()
	var held int
	if err := holder.QueryRow(context.Background(), `
		select count(*) from pg_locks l join pg_class c on c.oid = l.relation
		where l.pid = $1 and c.relname = $2 and l.mode = any($3)
	`, int32(pid), table, modes).Scan(&held); err != nil {
		t.Fatalf("read the locks backend %d holds on %s: %v", pid, table, err)
	}
	return held
}

// A dismissal and a session's reply into the same Agent thread, the dismissal first: it holds the
// thread's root `for update` (step 2) and is queued at the caller's own row of a Reply thread it
// names beside it (step 3). The reply then queues at the root with the statement that takes it
// before anything else of the thread, holding no attempt row and no write of messages, since it
// takes the root before its attempt and its insert (inboxwrite.Recording.QueueRootLock). Once the
// gate is let go the dismissal writes its mark, which moves the root's column, and commits; the
// reply then deletes the mark, which moves the column back, and the thread is in the Inbox,
// unread, with no mark and no column.
//
// With the reply's lock taken only where its clear runs, after its insert, it queues at the root
// holding the attempt row and the insert's lock on messages, and the reads of what it holds red.
func TestASessionReplyTakesTheAgentRootBeforeItsAttemptWhileADismissHoldsIt(t *testing.T) {
	t.Parallel()
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{
		inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
	})
	held := holdGateThread(t, database, seed.document)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockThreads)
	replied := make(chan *httptest.ResponseRecorder, 1)
	go func() { replied <- replyTo(t, handler, seed.agent, "And another thing.") }()
	replying := storetest.WaitForStatementBehind(t, database, held.tx, agentRootLock)
	// The root's own lock takes the row share mode on messages; the attempt's takes it on
	// message_deliveries, and the insert the row exclusive mode on messages.
	if locks := heldIn(t, held.tx, replying, "message_deliveries", rowShareMode, rowExclusiveMode); locks > 0 {
		t.Errorf("the reply waits at the agent thread's root holding its attempt (%d locks on message_deliveries): it took the attempt before the root", locks)
	}
	if locks := heldIn(t, held.tx, replying, "messages", rowExclusiveMode); locks > 0 {
		t.Errorf("the reply waits at the agent thread's root holding a write of messages (%d locks): it wrote before it took the root", locks)
	}
	held.release()

	response := <-answered
	if reply := <-replied; reply.Code != http.StatusCreated {
		t.Fatalf("the reply answered %d %s", reply.Code, reply.Body.String())
	}
	dismissRanAndLeftTheThread(t, seed, response, 2, seed.agent, agentThreadDismissal(t, database, "alice", seed.agent))
	requireRootMirrorsTheMark(t, database, seed.agent, "after the dismissal and the reply")
}

// A dismissal and a restore of the same Agent thread, the dismissal first. The dismissal reads the
// thread in the Inbox and queues at the owner of a Reply thread it names beside it (step 1); a
// dismissal of the agent thread alone, from another tab, commits meanwhile, so the thread has a
// mark. Once that gate is let go the held dismissal takes the root `for update` (step 2), its
// check passes - the thread holds the reply it drew, read since - and it queues at the caller's
// own row of the Reply thread (step 3), holding the root. The restore of the thread, which reads
// the mark the other tab wrote, then queues at the root before it takes the mark
// (inboxRestoreLockAgentRoots). Let go, the dismissal finds the other tab's later mark and leaves
// it, and commits; the restore deletes the mark, which clears the root's column, and the thread
// is in the Inbox with no mark and no column.
//
// With the restore's root lock gone it takes the mark first and deletes it, and the trigger's
// update of the root waits for the dismissal, which then waits for the mark the restore holds: a
// cycle Postgres breaks by failing one of the two. The restore is not found waiting at the root
// before that, and that is this row's red.
func TestARestoreTakesTheAgentRootBeforeTheMarkWhileADismissHoldsIt(t *testing.T) {
	t.Parallel()
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	agent := inboxDismissAgentPrefix + seed.agent
	body := dismissBody(t, database, "alice", []string{inboxDismissReplyPrefix + seed.document, agent})
	document := holdGateDocument(t, database, threadArtifact(t, database, "alice", seed.document))
	thread := holdGateThread(t, database, seed.document)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	storetest.WaitForStatementBehind(t, database, document.tx, store.OwnersLockShared)
	if dismissed := dismissRows(t, handler, database, "alice", agent); dismissed != 1 {
		t.Fatalf("the other tab's dismissal of the agent thread changed %d threads, want it", dismissed)
	}
	requireRootMirrorsTheMark(t, database, seed.agent, "after the other tab's dismissal")
	if agentRootDismissedAt(t, database, seed.agent) == nil {
		t.Fatal("the other tab's dismissal left the agent thread's root no mark")
	}
	document.release()
	storetest.WaitForStatementBehind(t, database, thread.tx, inboxReadLockThreads)
	restored := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		restored <- postInboxDismiss(t, handler, "alice", map[string]any{"threads": []string{agent}, "restore": true})
	}()
	storetest.WaitForStatementBehind(t, database, thread.tx, inboxRestoreLockAgentRoots)
	thread.release()

	response, restore := <-answered, <-restored
	if response.Code != http.StatusOK || restore.Code != http.StatusOK {
		t.Fatalf("the dismiss answered %d %s and the restore %d %s, want both to land",
			response.Code, response.Body.String(), restore.Code, restore.Body.String())
	}
	if dismissed := dismissedBy(t, response); dismissed != 1 {
		t.Errorf("the dismiss changed %d threads, want the document thread alone: the other tab's mark is later than its own", dismissed)
	}
	if put := dismissedBy(t, restore); put != 1 {
		t.Errorf("the restore changed %d threads, want the agent thread", put)
	}
	if mark := agentThreadDismissal(t, database, "alice", seed.agent); mark != nil {
		t.Fatalf("the agent thread carries the mark %s after the restore", mark)
	}
	requireRootMirrorsTheMark(t, database, seed.agent, "after the dismissal and the restore")
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the restored agent thread", inbox)
	}
}

// The root's column through the routes, one write at a time: a dismissal sets it to the mark it
// writes, a restore clears it, a second dismissal sets it again, and a session's reply, which
// brings the thread back, clears it with the mark. The Inbox's two states read the root by that
// column (myAgentLiveRoot), so each step is read there too.
func TestTheAgentRootsColumnFollowsItsDismissalsThroughTheRoutes(t *testing.T) {
	t.Parallel()
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	agent := inboxDismissAgentPrefix + seed.agent
	inInbox := func(what string, want bool) {
		t.Helper()
		requireRootMirrorsTheMark(t, database, seed.agent, what)
		if held := slices.Contains(threadsInState(t, handler, "alice", myRepliesStateInbox), seed.agent); held != want {
			t.Fatalf("%s: state=inbox holds the agent thread %t, want %t", what, held, want)
		}
	}
	inInbox("before any dismissal", true)
	if dismissed := dismissRows(t, handler, database, "alice", agent); dismissed != 1 {
		t.Fatalf("the dismissal changed %d threads, want the agent thread", dismissed)
	}
	inInbox("after the dismissal", false)
	if restored := restoreRows(t, handler, "alice", agent); restored != 1 {
		t.Fatalf("the restore changed %d threads, want the agent thread", restored)
	}
	inInbox("after the restore", true)
	if dismissed := dismissRows(t, handler, database, "alice", agent); dismissed != 1 {
		t.Fatalf("the second dismissal changed %d threads, want the agent thread", dismissed)
	}
	inInbox("after the second dismissal", false)
	if reply := replyTo(t, handler, seed.agent, "One more thing."); reply.Code != http.StatusCreated {
		t.Fatalf("the reply answered %d %s", reply.Code, reply.Body.String())
	}
	inInbox("after the session's reply", true)
}
