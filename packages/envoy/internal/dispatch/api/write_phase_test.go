package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A write transaction runs a read phase holding no transaction id - its owner's advisory lock
// (store.LockOwner), its reads, its document work - and then a write phase of back-to-back
// statements, sent as one pipelined batch (docs.Ledger's queue, store.WriteBatch). The tests here
// pin what a caller sees of that split: a close committed while a writer waited still refuses it,
// a thread read still waits for a writer it would otherwise race, writers of one owner still run
// one at a time, and a write phase that fails anywhere in its batch leaves nothing behind.

// gatedProjectMark holds the first ProjectMark called once the test arms it, inside the writer's
// read phase: its handler holds the owner's lock and has queued its writes, none of which has run.
type gatedProjectMark struct {
	docs.API
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedProjectMark) ProjectMark(ctx context.Context, artifactID, markID string, record docs.MarkRecord, actor model.Actor) error {
	if g.armed.Load() {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return g.API.ProjectMark(ctx, artifactID, markID, record, actor)
}

// A close committed while a write waited for the issue's owner lock refuses that write: it reads
// the issue in the statement after the lock, whose snapshot holds the close. Read in the lock's
// own statement, the issue would be the row as it stood before the wait, and the write would land
// on a closed issue. The write is an ask, one of the writes a closed issue refuses
// (refuseClosed, refusedWhenClosed): a comment and a message it takes, so neither would say
// whether the close was seen.
func TestACloseCommittedWhileAWriterWaitsOnTheOwnerLockIsRefused(t *testing.T) {
	// The seeded spec's settlement takes the owner lock this test holds; keep it out of the queue.
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Closed while waiting", "A spec")
	// Creating the issue starts the project's term-statistics refresh, which commits its rows
	// whenever it finishes: waited out here, it cannot read as a write of the refused ask.
	awaitBackgroundWrites(t, served)
	before := databaseFingerprint(t, database)

	closer, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the close: %v", err)
	}
	defer func() { _ = closer.Rollback(context.Background()) }()
	// The close is a PATCH's: under the issue's owner lock.
	if err := store.LockOwner(context.Background(), closer, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("take the issue's owner lock: %v", err)
	}
	if _, err := closer.Exec(context.Background(), `
		update issues set status = 'done', closed_at = now() where key = $1
	`, issue.Key); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
			map[string]any{"question": "Opened while the issue closed?"}, "alice")
	}()
	waitForDatabaseLocks(t, closer, 1)
	if err := closer.Commit(context.Background()); err != nil {
		t.Fatalf("commit the close: %v", err)
	}

	response := awaitResponse(t, responses)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("ask that waited out the close: status=%d body=%s, want 409 ISSUE_CLOSED", response.Code, response.Body.String())
	}
	// The close changed the issue row; the refused ask changed nothing at all: no ask, no event,
	// no part row.
	after := databaseFingerprint(t, database)
	delete(before, "issues")
	delete(after, "issues")
	assertUnchanged(t, "an ask refused for a close it waited out", before, after)
}

// A person marking a thread read while a session's reply to it is in its read phase waits for that
// reply: the mark takes the thread owner's lock, which the reply holds from before its first read
// to its commit. Both succeed, neither is refused as a deadlock, and the mark, made after the
// reply was written, leaves the thread read.
func TestAThreadReadWaitsForAWritersReadPhase(t *testing.T) {
	gate := &gatedProjectMark{entered: make(chan struct{}), release: make(chan struct{})}
	handler, _, database, deps := pooledDocumentHandlerWith(t, 8, time.Hour, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	handler = withDeadline(t, handler)
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Read while replying", "alpha bravo charlie")
	root := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Is alpha right?", "anchor": map[string]any{"artifact": "spec", "quote": "alpha"},
	}, "alice")

	// The reply and the mark each run on a connection of their own, so the wait watches the mark's
	// backend queue behind the reply's (storetest.WaitForQueuedBehindBackend): no other backend
	// queued on the thread owner's lock is taken for the mark.
	replyHandler, replyPID := oneConnectionHandler(t, deps)
	replyHandler = withDeadline(t, replyHandler)
	markHandler, markPID := oneConnectionHandler(t, deps)
	markHandler = withDeadline(t, markHandler)

	// A reply to an anchored comment projects the thread onto its root's mark (ProjectMark), in
	// its read phase, after it has read its creation time and queued its insert.
	gate.armed.Store(true)
	replies := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replies <- sessionRequest(t, replyHandler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Yes.", "reply_to": root.ID, "actor": sessionActor(),
		})
	}()
	testwait.Until(t, "the reply to reach its thread's projection", func() (bool, bool) {
		select {
		case <-gate.entered:
			return true, true
		case response := <-replies:
			t.Fatalf("the reply answered %d %s before projecting its thread", response.Code, response.Body.String())
		default:
		}
		return false, false
	})

	mark := time.Now()
	marks := make(chan *httptest.ResponseRecorder, 1)
	go func() { marks <- markThreadRead(t, markHandler, "alice", root.ID, mark) }()
	if response, early := storetest.WaitForQueuedBehindBackend(t, database, replyPID, markPID, marks); early {
		t.Fatalf("the thread read answered %d %s without waiting for the owner lock its writer holds", response.Code, response.Body.String())
	}
	release()

	reply := awaitResponse(t, replies)
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply: status=%d body=%s, want 201", reply.Code, reply.Body.String())
	}
	marked := awaitResponse(t, marks)
	if marked.Code != http.StatusNoContent {
		t.Fatalf("thread read: status=%d body=%s, want 204", marked.Code, marked.Body.String())
	}
	written := decodeBody[model.Comment](t, reply)
	through, repliedAt := readThrough(t, database, root.ID)
	if through.Before(mark.Truncate(time.Microsecond)) {
		t.Fatalf("read mark %s lost the mark %s", through, mark)
	}
	if repliedAt == nil || repliedAt.Before(written.CreatedAt) {
		t.Fatalf("replied_at %v lost the reply at %s", repliedAt, written.CreatedAt)
	}
	if listsThread(getMyReplies(t, handler, "alice", ""), root.ID) {
		t.Fatalf("the thread is still unread after a mark made after the reply (read through %s, replied at %s)", through, written.CreatedAt)
	}
}

// Two anchored comments on one spec still run one at a time, and the second waits for the first
// holding no transaction id, so a transaction waiting its turn holds back no live update capped by
// the event horizon. Both land, their events in the order they arrived and the issue's sequence
// without a gap.
func TestTwoWritersOfOneOwnerSerializeWithoutATransactionID(t *testing.T) {
	gate := &gatedMarkQuote{entered: make(chan struct{}), release: make(chan struct{})}
	handler, _, database, deps := pooledDocumentHandlerWith(t, 8, time.Hour, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	handler = withDeadline(t, handler)
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Serialized writers", "alpha bravo charlie delta")

	// Each comment runs on a connection of its own, so each read below names that comment's
	// backend: whether the first holds a transaction id in its read phase
	// (storetest.HoldsTransactionID), and whether the second queues behind the first
	// (storetest.WaitForQueuedBehindBackend) holding none. No other backend, the server's own
	// background writes among them, is taken for either.
	comment := func(quote, body string) (uint32, chan *httptest.ResponseRecorder) {
		callHandler, pid := oneConnectionHandler(t, deps)
		callHandler = withDeadline(t, callHandler)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- dispatchRequest(t, callHandler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"anchor": map[string]string{"artifact": "spec", "quote": quote}, "body": body,
			}, "alice")
		}()
		return pid, answered
	}
	firstPID, firsts := comment("alpha", "first")
	testwait.Until(t, "the first comment to reach its quote's mark", func() (bool, bool) {
		select {
		case <-gate.entered:
			return true, true
		case response := <-firsts:
			t.Fatalf("the first comment answered %d %s before marking its quote", response.Code, response.Body.String())
		default:
		}
		return false, false
	})
	if holdingXid, err := storetest.HoldsTransactionID(context.Background(), database, firstPID); err != nil {
		t.Fatal(err)
	} else if holdingXid {
		t.Fatal("the first comment holds a transaction id in its read phase, want none")
	}

	secondPID, seconds := comment("bravo", "second")
	if response, early := storetest.WaitForQueuedBehindBackend(t, database, firstPID, secondPID, seconds); early {
		t.Fatalf("the second comment answered %d %s without waiting for the owner lock its writer holds", response.Code, response.Body.String())
	}
	if holdingXid, err := storetest.HoldsTransactionID(context.Background(), database, secondPID); err != nil {
		t.Fatal(err)
	} else if holdingXid {
		t.Fatal("the second comment waits on the owner lock holding a transaction id")
	}
	release()

	first := awaitResponse(t, firsts)
	second := awaitResponse(t, seconds)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("comments: first %d %s, second %d %s, want 201 each", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	firstID := decodeBody[model.Comment](t, first).ID
	secondID := decodeBody[model.Comment](t, second).ID

	rows, err := database.Pool.Query(context.Background(), `
		select seq, type, coalesce(payload->>'id', '') from events where issue_key = $1 order by seq
	`, issue.Key)
	if err != nil {
		t.Fatalf("read the issue's events: %v", err)
	}
	defer rows.Close()
	created := []string{}
	expected := 1
	for rows.Next() {
		var seq int
		var eventType, commentID string
		if err := rows.Scan(&seq, &eventType, &commentID); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		if seq != expected {
			t.Fatalf("event %s has seq %d, want %d: the issue's sequence has a gap", eventType, seq, expected)
		}
		expected++
		if eventType == "comment.created" {
			created = append(created, commentID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issue's events: %v", err)
	}
	if len(created) != 2 || created[0] != firstID || created[1] != secondID {
		t.Fatalf("comment.created events = %v, want the first comment %s then the second %s", created, firstID, secondID)
	}
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue.Key).Scan(&lastSeq); err != nil {
		t.Fatalf("read the issue's sequence: %v", err)
	}
	if lastSeq != expected-1 {
		t.Fatalf("issue last_seq = %d, want %d, the last event's", lastSeq, expected-1)
	}
}

// Two PATCHes of one issue that each add a label serialize on the issue's owner lock, the point
// that replaced the issue row's `for no key update` in their read phase. Adding a label reads the
// issue's labels and writes the union back, so two PATCHes that read the same labels would drop
// one another's label. Both wait for the lock's holder with no transaction id, then each reads
// what the one before it committed: both labels land, and the issue's sequence has no gap.
func TestTwoPatchesOfOneIssueSerializeOnItsOwnerLockAndLoseNoUpdate(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Two patches", "A spec")

	holder, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if err := store.LockOwner(context.Background(), holder, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("take the issue's owner lock: %v", err)
	}

	// Each PATCH runs on a connection of its own, so the wait watches each one's backend
	// (storetest.WaitForQueuedBehind) and the transaction id read is that backend's: no other
	// backend queued on the issue's owner lock or holding a transaction id, the server's own among
	// them, is taken for a PATCH.
	type labelPatch struct {
		label    string
		pid      uint32
		answered chan *httptest.ResponseRecorder
	}
	var patches []labelPatch
	for _, label := range []string{"alpha", "bravo"} {
		callHandler, pid := oneConnectionHandler(t, deps)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- dispatchRequest(t, callHandler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
				map[string]any{"labels_add": []string{label}}, "alice")
		}()
		patches = append(patches, labelPatch{label: label, pid: pid, answered: answered})
	}
	for _, patch := range patches {
		if response, early := storetest.WaitForQueuedBehind(t, holder, patch.pid, patch.answered); early {
			t.Fatalf("the %s PATCH answered %d %s while another transaction held the issue's owner lock",
				patch.label, response.Code, response.Body.String())
		}
		if holdingXid, err := storetest.HoldsTransactionID(context.Background(), database, patch.pid); err != nil {
			t.Fatal(err)
		} else if holdingXid {
			t.Fatalf("the %s PATCH waits on the issue's owner lock holding a transaction id, want none", patch.label)
		}
	}
	var lastSeqBefore int
	if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue.Key).Scan(&lastSeqBefore); err != nil {
		t.Fatalf("read the issue's sequence: %v", err)
	}
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("release the issue's owner lock: %v", err)
	}
	for _, patch := range patches {
		if response := awaitResponse(t, patch.answered); response.Code != http.StatusOK {
			t.Fatalf("the %s PATCH answered %d %s, want 200", patch.label, response.Code, response.Body.String())
		}
	}

	var labels []string
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(), `select labels, last_seq from issues where key = $1`, issue.Key).Scan(&labels, &lastSeq); err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	if !slices.Contains(labels, "alpha") || !slices.Contains(labels, "bravo") {
		t.Fatalf("issue labels = %v, want both alpha and bravo: one PATCH overwrote the other's label", labels)
	}
	if lastSeq < lastSeqBefore+2 {
		t.Fatalf("issue last_seq = %d, want at least %d: two PATCHes, an event each", lastSeq, lastSeqBefore+2)
	}
	var events, maxSeq int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(max(seq), 0) from events where issue_key = $1
	`, issue.Key).Scan(&events, &maxSeq); err != nil {
		t.Fatalf("read the issue's events: %v", err)
	}
	if events != maxSeq || maxSeq != lastSeq {
		t.Fatalf("issue events = %d with max seq %d and last_seq %d, want one event per seq up to last_seq", events, maxSeq, lastSeq)
	}
}

// A write whose write phase fails part way answers what that failure answers and leaves nothing:
// no row of its own, no document update in the room or the store, no event. Accepting a suggestion
// queues the document's replacement first and its events last, so a failure anywhere in between
// shows in every one of them.
func TestAFailedWritePhaseLeavesNothing(t *testing.T) {
	suggest := func(t *testing.T, handler http.Handler) (issueKey, artifactID, commentID string) {
		t.Helper()
		issue := createInteractionIssue(t, handler, "TEST", "Failed write phase", "foo")
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Replace foo.", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
			"suggestion": map[string]string{"replace_with": "foo2"}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
		}
		return issue.Key, issue.PrimaryArtifactID, decodeBody[model.Comment](t, created).ID
	}
	assertDocumentUnchanged := func(t *testing.T, service *docs.Service, artifactID string) {
		t.Helper()
		for _, where := range []string{"live room", "reloaded room"} {
			if where == "reloaded room" {
				if err := service.Evict(context.Background(), artifactID); err != nil {
					t.Fatalf("evict the document: %v", err)
				}
			}
			markdown, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the %s: %v", where, err)
			}
			if markdown != "foo\n" {
				t.Fatalf("%s after the failed accept = %q, want foo", where, markdown)
			}
		}
	}

	// The suggestion is decided, by a writer that does not hold the owner's lock, after the accept
	// read it undecided and before its write phase: the queued update's guard finds it decided and
	// refuses with the 409 a decided suggestion answers.
	t.Run("a refused guard", func(t *testing.T) {
		var service *docs.Service
		var gate *firstReplaceGate
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			gate = &firstReplaceGate{
				API: service, firstEntered: make(chan struct{}), releaseFirst: make(chan struct{}), secondEntered: make(chan struct{}),
			}
			return gate
		})
		handler = withDeadline(t, handler)
		var released sync.Once
		release := func() { released.Do(func() { close(gate.releaseFirst) }) }
		defer release()
		_, artifactID, commentID := suggest(t, handler)

		responses := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
		}()
		testwait.Until(t, "the accept to reach the replacement", func() (bool, bool) {
			select {
			case <-gate.firstEntered:
				return true, true
			case response := <-responses:
				t.Fatalf("the accept answered %d %s before replacing the quote", response.Code, response.Body.String())
			default:
			}
			return false, false
		})
		// The update takes no lock the accept holds in its read phase. It runs on a goroutine of its
		// own, so that the wait for it ends on its answer, bounded by the test binary's deadline.
		decided := make(chan error, 1)
		go func() {
			_, err := database.Pool.Exec(t.Context(), `
			update comments set suggestion = jsonb_set(suggestion, '{accepted}', 'false'::jsonb) where id = $1
		`, commentID)
			decided <- err
		}()
		if err := testwait.Receive(t, "the update that decides the suggestion behind the accept", decided); err != nil {
			t.Fatalf("decide the suggestion behind the accept: %v", err)
		}
		// The accept's read phase wrote nothing, so this is the database as it stood before it.
		before := databaseFingerprint(t, database)
		release()

		response := awaitResponse(t, responses)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ALREADY_ACTIONED"`) {
			t.Fatalf("accept of a suggestion decided behind it: status=%d body=%s, want 409 ALREADY_ACTIONED", response.Code, response.Body.String())
		}
		assertUnchanged(t, "an accept whose guard refused in its write phase", before, databaseFingerprint(t, database))
		assertDocumentUnchanged(t, service, artifactID)
	})

	// The issue's next sequence number is taken by a row its counter does not know of, so the
	// accept's first event append, in its write phase, is a unique violation: the server's failure,
	// a 500, after the replacement, the comment's update and the named version were already
	// written in the transaction.
	t.Run("a unique violation", func(t *testing.T) {
		var service *docs.Service
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			return service
		})
		handler = withDeadline(t, handler)
		issueKey, artifactID, commentID := suggest(t, handler)
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select key, last_seq + 1, 'test.seq_taken', '{"kind":"session","id":"seq-taken"}'::jsonb, '{}'::jsonb, false
			from issues where key = $1
		`, issueKey); err != nil {
			t.Fatalf("take the issue's next sequence number: %v", err)
		}
		before := databaseFingerprint(t, database)

		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
			t.Fatalf("accept whose event append collides: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
		}
		assertUnchanged(t, "an accept whose event append failed in its write phase", before, databaseFingerprint(t, database))
		assertDocumentUnchanged(t, service, artifactID)
	})

	// takeNextSeq inserts a row at the issue's next sequence number, which its counter does not
	// know of, so the next event appended on the issue is a unique violation.
	takeNextSeq := func(t *testing.T, database *store.Store, issueKey string) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select key, last_seq + 1, 'test.seq_taken', '{"kind":"session","id":"seq-taken"}'::jsonb, '{}'::jsonb, false
			from issues where key = $1
		`, issueKey); err != nil {
			t.Fatalf("take the issue's next sequence number: %v", err)
		}
	}

	// An anchored comment's write phase is one batch: the document's update, the comment row, its
	// references, then comment.created and its stamp. The event's insert collides, after every
	// other statement of the batch ran on the server, and the whole transaction goes.
	t.Run("a comment whose event collides", func(t *testing.T) {
		var service *docs.Service
		handler, database, _, served := newInteractionServer(t, func(database *store.Store) docs.API {
			service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			return service
		})
		handler = withDeadline(t, handler)
		issue := createInteractionIssue(t, handler, "TEST", "Failed comment write phase", "foo")
		takeNextSeq(t, database, issue.Key)
		// Creating the issue starts the project's term-statistics refresh, which drains
		// search_term_queue and commits search_term_stats and search_term_totals whenever it
		// finishes: waited out here, it cannot read as a write of the refused comment.
		awaitBackgroundWrites(t, served)
		before := databaseFingerprint(t, database)

		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "See dispatch://" + issue.Key + ".", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
			"actor": sessionActor(),
		})
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
			t.Fatalf("comment whose event append collides: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
		}
		assertUnchanged(t, "a comment whose event append failed in its write phase", before, databaseFingerprint(t, database))
		assertDocumentUnchanged(t, service, issue.PrimaryArtifactID)
	})

	// A message writes without a document ledger, its insert, references, event and stamp one batch
	// of their own (store.RunWrite): the event's collision takes the message's row with it.
	t.Run("a message whose event collides", func(t *testing.T) {
		handler, database, _, served := newInteractionServer(t, func(database *store.Store) docs.API {
			service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			return service
		})
		handler = withDeadline(t, handler)
		issue := createInteractionIssue(t, handler, "TEST", "Failed message write phase", "foo")
		takeNextSeq(t, database, issue.Key)
		// The same refresh the comment's row above waits out.
		awaitBackgroundWrites(t, served)
		before := databaseFingerprint(t, database)

		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
			"body": "Status: see dispatch://" + issue.Key + ".", "actor": sessionActor(),
		})
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
			t.Fatalf("message whose event append collides: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
		}
		assertUnchanged(t, "a message whose event append failed in its write phase", before, databaseFingerprint(t, database))
	})
}
