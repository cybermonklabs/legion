package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// another reason into another test. Every writer the census names is placed: it is a row here, or
// it is named in itemRowLockNoRow, this table's gap map, with why it has none. The gap map and
// TestTheLockTablePlacesEveryWriterTheCensusNames, which holds the two lists to the census and to
// each other, live beside the census (item_writer_census_test.go), since they read names alone; a
// census entry added or renamed on one side alone reds there rather than leaving this table
// quietly stale.
//
// Each row runs the writer on a store with one connection of its own, so the backend the wait
// watches (storetest.WaitForQueuedBehind) is that writer's and no other: a document settlement or
// a mention sender on the shared pool queues behind the same owner lock, and counting the
// backends behind it would pass a writer that never waited.
//
// A row may also hand the holder a write of its own to make before it lets the lock go
// (lockedItemWrite.meanwhile). That is the reader this table could not otherwise have: waiting is
// proved by waiting, but what a writer does with the row it finds once it has the lock - whether
// it decides on the row as the lock froze it, or on a row it read before it - shows only where the
// row changed underneath it while it waited. The anchor-block backfill is driven that way, and why
// is written at readyAnchorBlockBackfill.

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
	// meanwhile, where a row has one, is the write another writer of this owner makes while the
	// lock is held and the writer under test is queued behind it. It runs on the holder's own
	// transaction, which is the writer holding the owner here, and the holder commits rather
	// than rolls back, so what it wrote is what the queued writer finds when it is let through.
	meanwhile func(t *testing.T, holder pgx.Tx)
	// wrote fails the test unless the writer, run once the lock was given back, finished and
	// left the row it meant to write.
	wrote func(t *testing.T)
}

// itemRowLockCase is one row of the table: a serving writer of an item row, the census entry that
// names it, and how to ready it.
type itemRowLockCase struct {
	// name is the subtest's name.
	name string
	// file and function are the writer this row drives, spelled as the census spells it
	// (itemRowWriters): the path within the module, and the function within that file. They are
	// read, not only printed - TestTheLockTablePlacesEveryWriterTheCensusNames looks the pair up
	// in the census - so a writer renamed on one side alone reds there.
	file     string
	function string
	// ready stands up a server of this row's own, seeds what the writer needs and readies the
	// call.
	ready func(t *testing.T) lockedItemWrite
}

// writer is the row's writer as a failure prints it.
func (c itemRowLockCase) writer() string { return c.file + " " + c.function }

// itemRowLockTable is the table: the serving writers of item rows this test holds behind an owner
// lock. Every writer the census names that takes its owner lock in a transaction of its own is
// here - the five comment routes, the four ask routes, the two approval-request writers, the two
// message writers and the anchor-block backfill - and such a writer added to the census is added
// here too, as a row of its own. Those that write inside a transaction their caller opened rest
// on that caller's lock, which no row here drives; the gap map (itemRowLockNoRow) names each with
// why, and TestTheLockTablePlacesEveryWriterTheCensusNames keeps the two lists one.
var itemRowLockTable = []itemRowLockCase{
	{
		name:     "the comment create route",
		file:     "internal/dispatch/api/comment_create.go",
		function: "createCommentFor",
		ready:    readyCommentCreate,
	},
	{
		name:     "the comment edit route",
		file:     "internal/dispatch/api/comment_actions.go",
		function: "editComment",
		ready:    readyCommentEdit,
	},
	{
		name:     "the comment resolve route",
		file:     "internal/dispatch/api/comment_actions.go",
		function: "commentAction",
		ready:    readyCommentResolve,
	},
	{
		name:     "the comment reopen route",
		file:     "internal/dispatch/api/comment_actions.go",
		function: "reopenComment",
		ready:    readyCommentReopen,
	},
	{
		name:     "the mention reply route",
		file:     "internal/dispatch/api/comment_delivery.go",
		function: "replyComment",
		ready:    readyCommentReply,
	},
	{
		name:     "the ask create route",
		file:     "internal/dispatch/api/asks.go",
		function: "createAskFor",
		ready:    readyAskCreate,
	},
	{
		name:     "the ask edit route",
		file:     "internal/dispatch/api/asks.go",
		function: "editAsk",
		ready:    readyAskEdit,
	},
	{
		name:     "the ask answer route",
		file:     "internal/dispatch/api/ask_transitions.go",
		function: "answerTransition",
		ready:    readyAskAnswer,
	},
	{
		name:     "the ask hand-on route",
		file:     "internal/dispatch/api/ask_asked_of.go",
		function: "putAskAskedOf",
		ready:    readyAskHandOn,
	},
	{
		name:     "the approval request route",
		file:     "internal/dispatch/api/reviews.go",
		function: "openApprovalAsk",
		ready:    readyApprovalRequest,
	},
	{
		name:     "the approval hand-back",
		file:     "internal/dispatch/api/reviews.go",
		function: "handBackApprovalAsk",
		ready:    readyApprovalHandBack,
	},
	{
		name:     "the issue message route",
		file:     "internal/dispatch/api/messages.go",
		function: "createStoredMessage",
		ready:    readyIssueMessage,
	},
	{
		name:     "the session reply route",
		file:     "internal/dispatch/api/message_delivery.go",
		function: "insertSessionReply",
		ready:    readySessionReply,
	},
	{
		name:     "the anchor-block backfill",
		file:     "internal/dispatch/docs/anchor_blocks.go",
		function: "backfillAnchorBlock",
		ready:    readyAnchorBlockBackfill,
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
// A row with a meanwhile gets a third question. The holder, which is the other writer of this
// owner, makes that write while the writer under test is queued, and commits it before it lets
// the lock go; what the writer does next says whether it read the row under the lock or before
// it. Waiting proves the lock is taken; only this proves the lock is worth taking.
//
// The census is the other half of this: it names every writer, this proves the lock of the
// writers it lists, and TestTheLockTablePlacesEveryWriterTheCensusNames keeps the two lists one.
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
					writer.writer(), answer, write.owner.LockKey(), write.writes)
			}
			if holdsTransactionID(t, holder, write.pid) {
				t.Fatalf("%s waits for %s, the owner lock of %s, with a transaction id already open: it "+
					"wrote before it locked, so what it wrote landed under a writer holding that owner. "+
					"Take the owner's lock in the read phase, before the first write.",
					writer.writer(), write.owner.LockKey(), write.writes)
			}

			if write.meanwhile != nil {
				write.meanwhile(t, holder)
			}
			// Committed, not rolled back: a row's meanwhile is the other writer's write, and the
			// writer queued behind this lock must meet it. A holder with no meanwhile wrote
			// nothing, so its commit and its rollback leave the same database behind.
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("release the owner lock: %v", err)
			}
			select {
			case <-answered:
			case <-time.After(itemRowOwnerLockFinish):
				t.Fatalf("%s never answered within %s of the owner lock being released",
					writer.writer(), itemRowOwnerLockFinish)
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

// itemRowSessionListener is an Envoy listener with one live session, s1, which takes every send
// and answers it: the fixture a row whose writer answers a delivery needs, since the delivery it
// answers is only made where a listener took it. capabilities are the session's, as the delivery
// the row drives needs them - a mention is steered, a targeted message is a btw.
func itemRowSessionListener(t *testing.T, capabilities ...string) *httptest.Server {
	t.Helper()
	sessions := `[{"session_id":"s1","title":"planner","capabilities":["` +
		strings.Join(capabilities, `","`) + `"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"item-row-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener
}

// readyCommentReply drives POST /api/v1/comments/{id}/reply, the mentioned agent's answer, which
// stores a comment of its own under the mentioned comment's owner lock (replyComment,
// lockedCommentForDelivery). The mention it answers needs a listener to resolve against, so this
// row stands one up and lets the create's delivery settle before the reply takes its attempt.
func readyCommentReply(t *testing.T) lockedItemWrite {
	t.Helper()
	listener := itemRowSessionListener(t, "steer")
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

// readyAskCreate drives POST /api/v1/issues/{key}/asks, which inserts the ask row under the
// owner's lock the route takes before it reads the owner (createAskFor, requireOpenOwnerStatus).
func readyAskCreate(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Ask create", "A spec.\n")
	const question = "Which transport, with the lock held?"
	return fixture.routeWrite(t, "the ask it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks",
				map[string]any{
					"question": question,
					"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
					"actor":    sessionActor(),
				})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("create the ask once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if stored := fixture.askQuestion(t, decodeBody[model.Ask](t, answer).ID); stored != question {
				t.Fatalf("the ask's stored question = %q, want %q", stored, question)
			}
		})
}

// readyAskEdit drives PATCH /api/v1/asks/{id}, which rewords the ask row under the ask's owner
// lock, taken before the document's (editAsk, requireOpenOwner).
func readyAskEdit(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Ask edit", "A spec.\n")
	askID := createIssueAsk(t, fixture.handler, fixture.issueKey, "Which transport?")
	const reworded = "Which transport, reworded while the lock was held?"
	return fixture.routeWrite(t, "the ask row it rewords",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
				"question": reworded,
				"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
				"actor":    sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("edit the ask once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if stored := fixture.askQuestion(t, askID); stored != reworded {
				t.Fatalf("the ask's stored question = %q, want %q", stored, reworded)
			}
		})
}

// readyAskAnswer drives POST /api/v1/asks/{id}/answer, the person's answer, which writes the
// ask's state under the owner lock transitionAskTx takes before the transition reads the ask
// (answerTransition, requireOpenOwner).
func readyAskAnswer(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Ask answer", "A spec.\n")
	askID := createIssueAsk(t, fixture.handler, fixture.issueKey, "Which transport?")
	return fixture.routeWrite(t, "the ask row it answers",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
				map[string]any{"selected": []string{"Yes"}}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("answer the ask once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if state := fixture.askState(t, askID); state != "answered" {
				t.Fatalf("the ask's stored state = %q, want answered", state)
			}
		})
}

// readyAskHandOn drives PUT /api/v1/asks/{id}/asked-of, which writes the person the ask waits on
// under the ask's owner lock, taken with the read of the owner it guards (putAskAskedOf,
// requireOpenOwner, store.LockOwnerAndRead).
func readyAskHandOn(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Ask hand-on", "A spec.\n")
	askID := createIssueAsk(t, fixture.handler, fixture.issueKey, "Which transport?")
	return fixture.routeWrite(t, "the asked_of columns of the ask row it hands on",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPut, "/api/v1/asks/"+askID+"/asked-of",
				map[string]any{"asked_of": "bob"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("hand the ask on once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			askedOf, by, at := askedOfColumns(t, fixture.database, askID)
			if askedOf == nil || *askedOf != "bob" || by == nil || *by != "alice" || at == nil {
				t.Fatalf("the ask holds asked_of=%v asked_of_by=%v asked_of_at=%v, want it waiting on bob",
					askedOf, by, at)
			}
		})
}

// readyApprovalRequest drives POST /api/v1/artifacts/{id}/approval-requests on a document with no
// request open, which inserts the approval ask under the document's owner lock (openApprovalAsk,
// requireOpenOwner).
func readyApprovalRequest(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Approval request", "A spec.\n")
	return fixture.routeWrite(t, "the approval ask it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost,
				"/api/v1/artifacts/"+fixture.artifactID+"/approval-requests",
				map[string]any{"summary": "Caps retries at three attempts.", "actor": sessionActor()})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("request approval once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if askID := decodeApprovalAskID(t, answer); !fixture.askIsApproval(t, askID) {
				t.Fatalf("the ask %s the route answered with carries no approval, so the row it meant "+
					"to write is not the row it wrote", askID)
			}
		})
}

// readyApprovalHandBack drives the same route on a document whose approval request is already
// open and waiting on its agent, which is the hand-back: the request's documents are written back
// at their current versions, and the reply it answers is recorded, under the document's owner
// lock the route took (handBackApprovalAsk, requireOpenOwner). The turn is what makes it a
// hand-back, so the fixture opens the request and has the human answer in its thread, which is
// what leaves it waiting on the agent.
func readyApprovalHandBack(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Approval hand-back", "A spec.\n")
	opened := sessionRequest(t, fixture.handler, http.MethodPost,
		"/api/v1/artifacts/"+fixture.artifactID+"/approval-requests",
		map[string]any{"summary": "Names the existing proposal.", "actor": sessionActor()})
	if opened.Code != http.StatusCreated {
		t.Fatalf("open the approval request this row hands back: status=%d body=%s",
			opened.Code, opened.Body.String())
	}
	askID := decodeApprovalAskID(t, opened)
	if human := dispatchRequest(t, fixture.handler, http.MethodPost,
		"/api/v1/issues/"+fixture.issueKey+"/comments",
		map[string]any{"ask_id": askID, "body": "Please clarify the rollout."}, "alice"); human.Code != http.StatusCreated {
		t.Fatalf("the human reply that leaves the request waiting on its agent: status=%d body=%s",
			human.Code, human.Body.String())
	}
	return fixture.routeWrite(t, "the approval ask row it hands back",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost,
				"/api/v1/artifacts/"+fixture.artifactID+"/approval-requests",
				map[string]any{"summary": "Clarifies the rollout.", "actor": sessionActor()})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("hand the approval request back once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			var handedBack bool
			if err := fixture.database.Pool.QueryRow(context.Background(),
				`select handed_back_reply_id is not null from asks where id = $1`, askID).Scan(&handedBack); err != nil {
				t.Fatalf("read the approval ask's hand-back: %v", err)
			}
			if !handedBack {
				t.Fatal("the approval ask records no hand-back: the route answered without writing the row it meant to write")
			}
		})
}

// readyIssueMessage drives POST /api/v1/issues/{key}/messages, which inserts the message row
// under the issue's owner lock (createStoredMessage, lockIssue).
func readyIssueMessage(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Issue message", "A spec.\n")
	const body = "The message the held lock delays."
	return fixture.routeWrite(t, "the message it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/issues/"+fixture.issueKey+"/messages", map[string]any{"body": body}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("create the message once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if stored := fixture.messageBody(t, decodeBody[model.Message](t, answer).ID); stored != body {
				t.Fatalf("the message's stored body = %q, want %q", stored, body)
			}
		})
}

// readySessionReply drives POST /api/v1/messages/{id}/reply, the session's answer to a message
// delivered to it, which inserts the reply message under the issue's owner lock the route takes
// before it plans anything (insertSessionReply, replyMessage's lockIssue). The delivery it
// answers needs a listener to take it, so this row stands one up and sends the message to that
// session before the lock is held.
func readySessionReply(t *testing.T) lockedItemWrite {
	t.Helper()
	listener := itemRowSessionListener(t, "btw")
	fixture := newItemRowFixture(t, testServerOptions{envoyURL: listener.URL}, "Session reply", "A spec.\n")
	message := createIssueMessage(t, fixture.handler, fixture.issueKey, map[string]any{
		"body": "Did this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if len(message.Deliveries) != 1 || message.Deliveries[0].Attempt != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the message this row is answered = %#v, want one attempt sent to the listener's session",
			message.Deliveries)
	}
	return fixture.routeWrite(t, "the reply message it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
				"actor":   map[string]any{"kind": "session", "id": "s1"},
				"attempt": 1,
				"body":    "The reply the held lock delays.",
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("reply to the message once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			var replies int
			if err := fixture.database.Pool.QueryRow(context.Background(),
				`select count(*) from messages where in_reply_to = $1`, message.ID).Scan(&replies); err != nil {
				t.Fatalf("count the message's replies: %v", err)
			}
			if replies != 1 {
				t.Fatalf("the message has %d replies, want the one the route answered with", replies)
			}
		})
}

// backfillAnchorQuote is the quote the backfilled comment is anchored to when the row is seeded,
// and backfillMovedQuote the one the holder moves it to while the backfill waits. They are two
// passages of this row's document, so they resolve to two blocks, which is what lets the row read
// which of them the backfill pinned.
const (
	backfillAnchorQuote = "Target passage"
	backfillMovedQuote  = "Introduction"
)

// readyAnchorBlockBackfill drives docs.BackfillAnchorBlocks, the command that pins the block of a
// legacy quote anchor, a row at a time, under that row's owner lock (backfillAnchorBlock,
// refs.LockItemOwner). It is a command, so it is driven through its function rather than through
// a handler, on a document service of its own whose store has one connection.
//
// The row it writes is made the way the rows the command exists for were made: a comment anchored
// to a quote whose block the create pinned, with the block taken back off, which is what an
// anchor written before the block identity existed holds (anchorBlockCandidates). It is the only
// such row in this fixture's database, so it is the row the command locks.
//
// This row has a meanwhile, and it is the only thing that reads the command's own lock. Waiting
// does not: resolving the quote is document work, and a document read joined to a transaction
// takes the document owner's lock itself (docs.joinRead, lockArtifactOwner), which for a comment
// on an issue's document is the same lock the row's owner lock is. A backfill with
// refs.LockItemOwner deleted therefore still queues behind this holder, and still queues with no
// transaction id, because what it has run by then are reads. What the deletion costs is the order
// of the re-read: the command's contract is that the row's anchor is read under the lock, so the
// quote it resolves is the quote the row holds now (backfillAnchorBlock). Without the lock the
// anchor is read before the wait, and the quote resolved is the one the row held before the
// writer ahead of it rewrote it.
//
// So the holder is that writer. While the command is queued it moves the anchor's quote to the
// document's other passage and commits, as a settlement that rewrote the anchor would
// (docs.refreshAnchors), and the row then asks which quote's block the command pinned. Under the
// lock it is the moved quote's, read under the lock it waited for; without the lock it is the old
// quote's, pinned over an anchor that no longer holds that quote.
func readyAnchorBlockBackfill(t *testing.T) lockedItemWrite {
	t.Helper()
	ctx := context.Background()
	fixture := newItemRowFixture(t, testServerOptions{}, "Anchor block backfill", "Introduction.\n\nTarget passage.\n")
	comment := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body":   "Please clarify this.",
		"anchor": map[string]any{"artifact": "spec", "quote": backfillAnchorQuote},
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
		meanwhile: func(t *testing.T, holder pgx.Tx) {
			if _, err := holder.Exec(ctx, `
				update comments set anchor = jsonb_set(anchor, '{quote}', to_jsonb($2::text), true)
				where id = $1
			`, comment.ID, backfillMovedQuote); err != nil {
				t.Fatalf("move the anchor's quote while the backfill waits: %v", err)
			}
		},
		wrote: func(t *testing.T) {
			if backfillErr != nil {
				t.Fatalf("backfill the anchor blocks once the lock was released: %v", backfillErr)
			}
			if report.Comments != 1 {
				t.Fatalf("the backfill pinned %+v, want the one comment this fixture unpinned", report)
			}
			moved, err := service.BlockForQuote(ctx, fixture.artifactID, backfillMovedQuote)
			if err != nil {
				t.Fatalf("resolve the block of the quote the anchor was moved to: %v", err)
			}
			if moved == pinned {
				t.Fatalf("both quotes of this row's document resolve to block %s, so the row cannot "+
					"tell the anchor the backfill read under the lock from the one it read before it",
					moved)
			}
			if blockID := fixture.anchorBlockID(t, comment.ID); blockID != moved {
				t.Fatalf("the comment's block_id = %q, want %q, the block of the quote the anchor held "+
					"when the backfill was let through: %q is the block of the quote it held before "+
					"this test's holder moved it, so the backfill read the anchor before it took the "+
					"row's owner lock and pinned a block for a quote the row no longer carries. Take "+
					"the owner's lock (refs.LockItemOwner) first and re-read the anchor under it "+
					"(backfillAnchorBlock).", blockID, moved, pinned)
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

// messageBody is the message's stored body, read from the pool.
func (f itemRowFixture) messageBody(t *testing.T, messageID string) string {
	t.Helper()
	var body string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select body from messages where id = $1`, messageID).Scan(&body); err != nil {
		t.Fatalf("read the message's body: %v", err)
	}
	return body
}

// askQuestion is the ask's stored question, read from the pool.
func (f itemRowFixture) askQuestion(t *testing.T, askID string) string {
	t.Helper()
	var question string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select question from asks where id = $1`, askID).Scan(&question); err != nil {
		t.Fatalf("read the ask's question: %v", err)
	}
	return question
}

// askState is the ask's stored state, read from the pool.
func (f itemRowFixture) askState(t *testing.T, askID string) string {
	t.Helper()
	var state string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select state from asks where id = $1`, askID).Scan(&state); err != nil {
		t.Fatalf("read the ask's state: %v", err)
	}
	return state
}

// askIsApproval reports whether the ask's row carries an approval, which is what the approval
// request route writes and an ordinary ask has none of.
func (f itemRowFixture) askIsApproval(t *testing.T, askID string) bool {
	t.Helper()
	var approval bool
	if err := f.database.Pool.QueryRow(context.Background(),
		`select approval is not null from asks where id = $1`, askID).Scan(&approval); err != nil {
		t.Fatalf("read the ask's approval: %v", err)
	}
	return approval
}

// decodeApprovalAskID is the id of the ask an approval request answered with.
func decodeApprovalAskID(t *testing.T, answer *httptest.ResponseRecorder) string {
	t.Helper()
	return decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, answer).Ask.ID
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
// backend it holds, so a call made on it is told from every other by its pid (QueuedBehind). A
// writer that is not a route runs on it directly; a route runs on the handler built over it
// (oneConnectionHandler, ask_turn_test.go), which is this store with deps' routes registered on
// it.
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
	// Registered before anything the caller registers over this store - a server's wait, a
	// document service's shutdown - so that those run first and the pool closes after them.
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
