package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The item-writer census (item_writer_census_test.go) is a map of names: it catches a writer of a
// comment, a message, an ask or one of their block-state rows that no entry names, and an entry
// whose function no longer exists, and it reads no lock at all. This file is the other half. It
// takes an item's owner lock in a transaction of its own and asks each writer on its table what
// that lock does to it. A writer that takes the lock waits for it, having written nothing yet;
// one that writes on the pool walks straight past the holder, and one that locks only on its way
// out - every route writes its owner's event, and appending one takes that owner's lock - stops
// at the lock with its insert or update already run and a transaction id already open, which is
// what each row's second question asks. The holder is then given back, and the writer must
// finish and write what it meant to write, so no row is passed by a writer that merely fails.
//
// The table is the point. A serving writer of an item row is covered by adding a row that names
// it and drives it - a route through its handler, a command through its function - not by writing
// another reason into another test.
//
// Each row runs the writer on a store with one connection of its own, so the backend the wait
// watches (storetest.WaitForQueuedBehind) is that writer's and no other: a document settlement or
// a mention sender on the shared pool queues behind the same owner lock, and counting the
// backends behind it would pass a writer that never waited.
//
// What the table covers is the backfill and the five comment routes. What it does not cover, and
// what each would cost:
//
//   - this package's ask routes - createAskFor and editAsk (asks.go), answerTransition
//     (ask_transitions.go), putAskAskedOf (ask_asked_of.go), openApprovalAsk and
//     handBackApprovalAsk (reviews.go) - and its message routes, createStoredMessage
//     (messages.go) and insertSessionReply (message_delivery.go). Each is a row of this shape and
//     a fixture: an open ask for the three that change one, a document carrying an approval
//     request for the reviews pair, and for the session reply the stub listener and the delivered
//     message it answers, as the comment reply row here stands up for a comment. Nothing about
//     their shape resists the table; they are the rows this round did not pay for.
//   - the writers that run inside another writer's transaction: docs' refreshAnchors,
//     RewriteApprovalAsk, answerApprovalAsk, the ask-block writers, queueAskResolution and
//     WriteRestoredAsks, and refs' queueBlocks, queueBlockRefs, queueRecordIndexed,
//     queueStateLock, markItemBlocksOwed, clearItemWindowsOwed, queueStamp and
//     settleOwedStatement. None takes a lock of its own: the caller's transaction holds the
//     owner, so a row here would prove the caller's lock and say nothing about the callee. To
//     cover one is to drive its caller - a version write and the settlement behind it, or the
//     block-index tick - and then to tell the callee's statement from its caller's, which one
//     call on one backend does not tell.
//   - the writers that take no owner lock at all, which is their census entry's exception:
//     cmd/dispatch-seed's three loaders, refs.RebuildAll, api's writeBroadcast and docs'
//     restoreWordings at boot. There is nothing for them to wait for, and a row asserting they do
//     not wait would prove the exception, not the rule.

// itemRowOwnerLockQueued bounds the wait for a writer's backend to queue behind the held lock. A
// writer reaches its owner lock in milliseconds - the reads before it are the request's own - so
// this bound is reached only by a call stuck somewhere other than that lock, and the failure then
// names the backend rather than hanging the package until go test's -timeout.
const itemRowOwnerLockQueued = 30 * time.Second

// itemRowOwnerLockFinish bounds the wait for the writer to answer once the lock is given back. A
// writer that queued has only its own work left, so this is far longer than any row takes.
const itemRowOwnerLockFinish = 60 * time.Second

// lockedItemWrite is one writer readied to run against a held owner lock.
type lockedItemWrite struct {
	// database is the pool the holder's transaction runs on: the writer's own store is a
	// one-connection pool of its own onto the same database.
	database *store.Store
	// owner is the owner of the row the writer writes, whose lock the test holds
	// (store.OwnerOf): every row of this table writes a row of an issue's, so it is that issue's.
	owner store.Owner
	// pid is the backend the writer runs on, the one connection of its store.
	pid uint32
	// writes names the row the writer writes, for the failure text.
	writes string
	// call runs the writer on another goroutine and answers what it answered, in a form a
	// failure can print; it keeps the answer itself for wrote.
	call func() string
	// wrote fails the test unless the writer, run once the lock was given back, finished and
	// left the row it meant to write.
	wrote func(t *testing.T)
}

// itemRowLockCase is one row of the table: a serving writer of an item row, the census entry that
// names it, and how to ready it.
type itemRowLockCase struct {
	// name is the subtest's name.
	name string
	// writer is the writer the row drives, as the census names it (itemRowWriters): the file
	// within the module and the function.
	writer string
	// ready stands up a server of this row's own, seeds what the writer needs and readies the
	// call.
	ready func(t *testing.T) lockedItemWrite
}

// itemRowLockTable is the table: the serving writers of item rows this test holds behind an owner
// lock. A writer added to the census is added here too, as a row of its own.
var itemRowLockTable = []itemRowLockCase{
	{
		name:   "the comment create route",
		writer: "internal/dispatch/api/comment_create.go createCommentFor",
		ready:  readyCommentCreate,
	},
	{
		name:   "the comment edit route",
		writer: "internal/dispatch/api/comment_actions.go editComment",
		ready:  readyCommentEdit,
	},
	{
		name:   "the comment resolve route",
		writer: "internal/dispatch/api/comment_actions.go commentAction",
		ready:  readyCommentResolve,
	},
	{
		name:   "the comment reopen route",
		writer: "internal/dispatch/api/comment_actions.go reopenComment",
		ready:  readyCommentReopen,
	},
	{
		name:   "the mention reply route",
		writer: "internal/dispatch/api/comment_delivery.go replyComment",
		ready:  readyCommentReply,
	},
	{
		name:   "the anchor-block backfill",
		writer: "internal/dispatch/docs/anchor_blocks.go backfillAnchorBlock",
		ready:  readyAnchorBlockBackfill,
	},
}

// TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites holds the owner lock of the
// row each writer of itemRowLockTable writes, in a transaction of its own, and holds the writer
// to two things: it waits for that lock, and when it waits it has written nothing. The advisory
// lock excludes exactly the writers that take it (store.LockOwner), so a writer of an item row
// that takes no owner lock is the one thing main's `for no key update` of the item row covered
// and this lock does not.
//
// The second half is what tells a writer that locks from one that merely meets the lock on its
// way out. Every route of this module writes its owner's event, and appending an event takes
// that owner's lock (events.Broker.Append), so a route whose own read went to the pool still
// stops at the lock - after its insert or update has run, with a transaction id already open.
// A writer takes its owner's lock in its read phase, before its first write
// (storetest.OwnerLockWaiters), so a waiter holding a transaction id wrote the row it writes
// before it locked it, which is the writer the advisory lock does not exclude.
//
// The census is the other half of this: it names every writer, this proves the lock of the
// writers it lists, and the gap between the two is named in this file's text.
func TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites(t *testing.T) {
	for _, writer := range itemRowLockTable {
		t.Run(writer.name, func(t *testing.T) {
			ctx := context.Background()
			write := writer.ready(t)
			holder, err := write.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the holder transaction: %v", err)
			}
			defer holder.Rollback(ctx)
			if err := store.LockOwner(ctx, holder, write.owner); err != nil {
				t.Fatalf("hold the owner lock %s: %v", write.owner.LockKey(), err)
			}

			answered := make(chan string, 1)
			go func() { answered <- write.call() }()
			if answer, early := storetest.WaitForQueuedBehind(t, holder, write.pid, answered, itemRowOwnerLockQueued); early {
				t.Fatalf("%s answered %s while this test held %s, the owner lock of %s: it writes that "+
					"row under no owner lock, so a writer holding the owner does not exclude it, and "+
					"main's `for no key update` of the item row did. Take the owner's lock "+
					"(store.LockOwner, refs.LockItemOwner) before the write.",
					writer.writer, answer, write.owner.LockKey(), write.writes)
			}
			if holdsTransactionID(t, holder, write.pid) {
				t.Fatalf("%s waits for %s, the owner lock of %s, with a transaction id already open: it "+
					"wrote before it locked, so what it wrote landed under a writer holding that owner. "+
					"Take the owner's lock in the read phase, before the first write.",
					writer.writer, write.owner.LockKey(), write.writes)
			}

			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("release the owner lock: %v", err)
			}
			select {
			case <-answered:
			case <-time.After(itemRowOwnerLockFinish):
				t.Fatalf("%s never answered within %s of the owner lock being released",
					writer.writer, itemRowOwnerLockFinish)
			}
			write.wrote(t)
		})
	}
}

// holdsTransactionID reports whether the backend pid holds a transaction id: it has written
// something in the transaction it has open. A transaction takes one at its first write and holds
// an ExclusiveLock on it until it ends, which is how pg_locks says so. It reads through holder's
// own connection, as QueuedBehind does and for the same reason: the writer may have drained the
// shared pool while it waits.
func holdsTransactionID(t *testing.T, holder pgx.Tx, pid uint32) bool {
	t.Helper()
	var held bool
	if err := holder.QueryRow(context.Background(), `
		select exists (
			select 1 from pg_locks where pid = $1 and locktype = 'transactionid' and granted
		)
	`, pid).Scan(&held); err != nil {
		t.Fatalf("read the writer's transaction id: %v", err)
	}
	return held
}

// itemRowFixture is the server one row of the table seeds on: the handler the seeding goes
// through, the pool the holder's transaction runs on, the dependencies the writer's own
// one-connection surface is built from, and an issue with a spec, whose owner is the owner the
// row's write belongs to.
type itemRowFixture struct {
	handler    http.Handler
	database   *store.Store
	deps       Deps
	issueKey   string
	artifactID string
}

// newItemRowFixture stands up that server and files the issue every row of this table writes
// under.
func newItemRowFixture(t *testing.T, options testServerOptions, title, spec string) itemRowFixture {
	t.Helper()
	handler, database, deps := newTestServer(t, options)
	issue := createInteractionIssue(t, handler, "LOCK", title, spec)
	return itemRowFixture{
		handler:    handler,
		database:   database,
		deps:       deps,
		issueKey:   issue.Key,
		artifactID: issue.PrimaryArtifactID,
	}
}

// routeWrite readies a route of this package as a row's write: the request goes through a handler
// on a store with one connection of its own, so the backend the wait watches is the request's.
// send runs on the test's other goroutine and hands its answer to wrote, which runs on the test's
// own once the lock has been given back.
func (f itemRowFixture) routeWrite(
	t *testing.T,
	writes string,
	send func(handler http.Handler) *httptest.ResponseRecorder,
	wrote func(t *testing.T, answer *httptest.ResponseRecorder),
) lockedItemWrite {
	t.Helper()
	handler, pid := oneConnectionHandler(t, f.deps)
	var answer *httptest.ResponseRecorder
	return lockedItemWrite{
		database: f.database,
		owner:    store.IssueOwner(f.issueKey),
		pid:      pid,
		writes:   writes,
		call: func() string {
			answer = send(handler)
			return fmt.Sprintf("status=%d body=%s", answer.Code, answer.Body.String())
		},
		wrote: func(t *testing.T) { wrote(t, answer) },
	}
}

// readyCommentCreate drives POST /api/v1/issues/{key}/comments, which inserts the comment row
// under the owner's lock its own transaction takes (createCommentFor, lockCommentOwner).
func readyCommentCreate(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Comment create", "A spec.\n")
	const body = "The comment the held lock delays."
	return fixture.routeWrite(t, "the comment it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{"body": body}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("create the comment once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if stored := fixture.commentBody(t, decodeBody[mentionedCommentRead](t, answer).ID); stored != body {
				t.Fatalf("the comment's stored body = %q, want %q", stored, body)
			}
		})
}

// readyCommentEdit drives PATCH /api/v1/comments/{id}, which rewrites the comment row under its
// owner's lock (editComment, lockedComment).
func readyCommentEdit(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Comment edit", "A spec.\n")
	comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{"body": "The first wording."}, "alice")
	const edited = "The wording the held lock delays."
	return fixture.routeWrite(t, "the comment row it rewrites",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch,
				"/api/v1/comments/"+comment.ID, map[string]any{"body": edited}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("edit the comment once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if stored := fixture.commentBody(t, comment.ID); stored != edited {
				t.Fatalf("the comment's stored body = %q, want %q", stored, edited)
			}
		})
}

// readyCommentResolve drives POST /api/v1/comments/{id}/resolve, the action route that writes the
// comment's resolution under its owner's lock (commentAction, lockedComment).
func readyCommentResolve(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Comment resolve", "A spec.\n")
	comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{"body": "Resolve this."}, "alice")
	return fixture.routeWrite(t, "the comment row it resolves",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/comments/"+comment.ID+"/resolve", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("resolve the comment once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if !fixture.commentResolved(t, comment.ID) {
				t.Fatal("the comment is not resolved: the route answered without writing the row it meant to write")
			}
		})
}

// readyCommentReopen drives POST /api/v1/comments/{id}/reopen, which clears that resolution under
// the same lock (reopenComment, lockedComment).
func readyCommentReopen(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Comment reopen", "A spec.\n")
	comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{"body": "Reopen this."}, "alice")
	if resolved := dispatchRequest(t, fixture.handler, http.MethodPost,
		"/api/v1/comments/"+comment.ID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve the comment the reopen clears: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	return fixture.routeWrite(t, "the comment row it reopens",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/comments/"+comment.ID+"/reopen", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("reopen the comment once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if fixture.commentResolved(t, comment.ID) {
				t.Fatal("the comment is still resolved: the route answered without writing the row it meant to write")
			}
		})
}

// readyCommentReply drives POST /api/v1/comments/{id}/reply, the mentioned agent's answer, which
// stores a comment of its own under the mentioned comment's owner lock (replyComment,
// lockedCommentForDelivery). The mention it answers needs a listener to resolve against, so this
// row stands one up and lets the create's delivery settle before the reply takes its attempt.
func readyCommentReply(t *testing.T) lockedItemWrite {
	t.Helper()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	fixture := newItemRowFixture(t, testServerOptions{envoyURL: listener.URL}, "Mention reply", "A spec.\n")
	comment := decodeMentionedComment(t, postMentionedComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body":     "The session this mention names must answer.",
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, fixture.database, comment.ID)
	return fixture.routeWrite(t, "the reply comment it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
				"actor":   map[string]any{"kind": "session", "id": "s1"},
				"target":  "session:s1",
				"attempt": 1,
				"body":    "The reply the held lock delays.",
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("reply to the mention once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			var replies int
			if err := fixture.database.Pool.QueryRow(context.Background(),
				`select count(*) from comments where reply_to = $1`, comment.ID).Scan(&replies); err != nil {
				t.Fatalf("count the mention's replies: %v", err)
			}
			if replies != 1 {
				t.Fatalf("the mentioned comment has %d replies, want the one the route answered with", replies)
			}
		})
}

// readyAnchorBlockBackfill drives docs.BackfillAnchorBlocks, the command that pins the block of a
// legacy quote anchor, a row at a time, under that row's owner lock (backfillAnchorBlock,
// refs.LockItemOwner). It is a command, so it is driven through its function rather than through
// a handler, on a document service of its own whose store has one connection.
//
// The row it writes is made the way the rows the command exists for were made: a comment anchored
// to a quote whose block the create pinned, with the block taken back off, which is what an
// anchor written before the block identity existed holds (anchorBlockCandidates). It is the only
// such row in this fixture's database, so it is the row the command locks.
func readyAnchorBlockBackfill(t *testing.T) lockedItemWrite {
	t.Helper()
	ctx := context.Background()
	fixture := newItemRowFixture(t, testServerOptions{}, "Anchor block backfill", "Introduction.\n\nTarget passage.\n")
	comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body":   "Please clarify this.",
		"anchor": map[string]any{"artifact": "spec", "quote": "Target passage"},
	}, "alice")
	pinned := fixture.anchorBlockID(t, comment.ID)
	if pinned == "" {
		t.Fatal("the anchored comment pinned no block, so the backfill would have nothing to write")
	}
	if _, err := fixture.database.Pool.Exec(ctx,
		`update comments set anchor = anchor - 'block_id' where id = $1`, comment.ID); err != nil {
		t.Fatalf("unpin the comment's anchor block: %v", err)
	}

	service, pid := oneConnectionDocs(t, fixture.deps)
	var report docs.AnchorBlockBackfill
	var backfillErr error
	return lockedItemWrite{
		database: fixture.database,
		owner:    store.IssueOwner(fixture.issueKey),
		pid:      pid,
		writes:   "the comment anchor it pins",
		call: func() string {
			report, backfillErr = service.BackfillAnchorBlocks(ctx)
			return fmt.Sprintf("%+v with error %v", report, backfillErr)
		},
		wrote: func(t *testing.T) {
			if backfillErr != nil {
				t.Fatalf("backfill the anchor blocks once the lock was released: %v", backfillErr)
			}
			if report.Comments != 1 {
				t.Fatalf("the backfill pinned %+v, want the one comment this fixture unpinned", report)
			}
			if blockID := fixture.anchorBlockID(t, comment.ID); blockID != pinned {
				t.Fatalf("the comment's block_id = %q, want %q: the backfill must still pin the block "+
					"once it holds the lock", blockID, pinned)
			}
		},
	}
}

// commentBody is the comment's stored body, read from the pool.
func (f itemRowFixture) commentBody(t *testing.T, commentID string) string {
	t.Helper()
	var body string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select body from comments where id = $1`, commentID).Scan(&body); err != nil {
		t.Fatalf("read the comment's body: %v", err)
	}
	return body
}

// commentResolved reports whether the comment's row is resolved, read from the pool.
func (f itemRowFixture) commentResolved(t *testing.T, commentID string) bool {
	t.Helper()
	var resolved bool
	if err := f.database.Pool.QueryRow(context.Background(),
		`select resolved from comments where id = $1`, commentID).Scan(&resolved); err != nil {
		t.Fatalf("read the comment's resolution: %v", err)
	}
	return resolved
}

// anchorBlockID is the block the comment's anchor pins, empty where it pins none.
func (f itemRowFixture) anchorBlockID(t *testing.T, commentID string) string {
	t.Helper()
	var blockID *string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select anchor->>'block_id' from comments where id = $1`, commentID).Scan(&blockID); err != nil {
		t.Fatalf("read the comment's anchor block: %v", err)
	}
	if blockID == nil {
		return ""
	}
	return *blockID
}

// oneConnectionStore opens a store of one connection onto deps' database and answers it with the
// backend it holds, so a call made on it is told from every other by its pid (QueuedBehind). It
// is the store half of oneConnectionHandler (ask_turn_test.go), which a writer that is not a
// route needs.
func oneConnectionStore(t *testing.T, deps Deps) (*store.Store, uint32) {
	t.Helper()
	config := deps.Store.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the call's one-connection pool: %v", err)
	}
	callStore := &store.Store{Pool: store.NewPool(opened)}
	t.Cleanup(callStore.Pool.Close)
	conn, err := callStore.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the call's connection: %v", err)
	}
	pid := conn.Conn().PgConn().PID()
	conn.Release()
	return callStore, pid
}

// oneConnectionDocs is a document service on such a store, for a document command driven through
// its function: it serves the same database and the same event broker as the fixture's server,
// and keeps the command to the one backend the wait watches.
func oneConnectionDocs(t *testing.T, deps Deps) (*docs.Service, uint32) {
	t.Helper()
	callStore, pid := oneConnectionStore(t, deps)
	service := docs.New(docs.Deps{
		Store:     callStore,
		Events:    deps.Events,
		Identity:  deps.Identity,
		ServerURL: deps.ServerURL,
	})
	// Registered after the store's close and so run before it, cleanups running last registered
	// first, which leaves the service its connection while it shuts down.
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown the command's document service: %v", err)
		}
	})
	return service, pid
}
