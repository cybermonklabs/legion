package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
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
	t.Parallel()
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

// Two writers queued on one lock, the first holding it past the bound of a lock's wait (store's
// lockWait), as a holder whose database stopped answering it would: the second waits for the lock
// holding no transaction id, is refused 503 DATABASE_BUSY with Retry-After once that bound runs out
// rather than waiting for the first to let go, and writes nothing; once the first lets go, the same
// write goes through. One row per kind of lock a write waits on behind another writer: the issue's
// owner lock (store.LockOwner), the deployment's dependency lock, the project's rank-allocation
// lock, the priorities' rank lock, which a create and a rename that moves a priority each wait on,
// the session groups' lock and a document's own lock, which an edit and an upload each wait on
// (store.LockWithinBound; the upload, agent-upload and repository-mapping locks go through the
// same call, held to it by store's TestALockWithinBoundEndsAtItsBoundAndLeavesTheTransactionUsable,
// and docs' copy and conditional-edit queue locks by docs' own rows). The bound is the test's
// (store.WithLockWaitForTest) rather than production's ten seconds, and the first writer is a
// transaction of the test's own that holds the lock until the test lets it go.
func TestAWriterQueuedOnABusyLockIsRefusedOnceItsWaitRunsOut(t *testing.T) {
	t.Parallel()
	const bound = time.Second
	for _, row := range []struct {
		name string
		// hold takes the lock in the first writer's transaction, issue being the issue the test
		// writes and document that issue's spec.
		hold func(ctx context.Context, holder pgx.Tx, issue, document string) error
		// method, path and body are the second writer's request, and written its answer once the
		// lock is free. The path's {key} is the issue's key and its {doc} the spec's id, and the
		// body's {base} is the token the spec stands at before the lock is taken: an upload that
		// replaces the spec names it, so the write checks it under the document's own lock
		// (docs.Service.ReplaceText) rather than taking it unchecked and counting it
		// (noteUnguardedReplacement), and both of its sends are answered for the lock and the write.
		method, path, body string
		written            int
		// unchanged, when it is true, holds the refused write to having changed no row anywhere.
		// A create does more than its transaction before it is refused (its gates' reads and their
		// records), so its row counts the project's issues instead.
		unchanged bool
		// seed, when it is set, writes what the second writer's request names before the first
		// writer takes the lock.
		seed func(t *testing.T, handler http.Handler)
	}{
		{
			name: "a title change behind the issue's owner lock",
			hold: func(ctx context.Context, holder pgx.Tx, issue, _ string) error {
				return store.LockOwner(ctx, holder, store.IssueOwner(issue))
			},
			method: http.MethodPatch, path: "/api/v1/issues/{key}", body: `{"title":"Renamed behind the held lock"}`,
			written: http.StatusOK, unchanged: true,
		},
		{
			name: "a blocker change behind the deployment's dependency lock",
			hold: func(ctx context.Context, holder pgx.Tx, _, _ string) error {
				return lockDeploymentIssueDependencies(ctx, holder)
			},
			method: http.MethodPatch, path: "/api/v1/issues/{key}", body: `{"title":"Unblocked behind the held lock","blocked_by":[]}`,
			written: http.StatusOK, unchanged: true,
		},
		{
			name: "an issue create behind the project's rank-allocation lock",
			hold: func(ctx context.Context, holder pgx.Tx, _, _ string) error {
				return lockProjectRankAllocation(ctx, holder, "TEST")
			},
			method: http.MethodPost, path: "/api/v1/issues", body: `{"project":"TEST","title":"Ranked behind the held lock","force":true}`,
			written: http.StatusCreated,
		},
		{
			name: "a priority create behind the priority-rank lock",
			hold: func(ctx context.Context, holder pgx.Tx, _, _ string) error {
				_, err := prioritystore.RankAt(ctx, holder, "holding-the-rank", "", "")
				return err
			},
			method: http.MethodPost, path: "/api/v1/priorities", body: `{"id":"ranked-behind-the-held-lock","title":"Ranked behind the held lock"}`,
			written: http.StatusCreated, unchanged: true,
		},
		{
			// The move's write also renames the priority, which it writes after the lock: a refused
			// write leaves its title and its place as they were, and its wait holds no transaction id.
			name: "a priority rename and move behind the priority-rank lock",
			seed: func(t *testing.T, handler http.Handler) {
				createTestPriority(t, handler, map[string]any{"id": "moving-priority", "title": "Moving priority"}, "alice")
				createTestPriority(t, handler, map[string]any{"id": "neighbour-priority", "title": "Neighbour priority"}, "alice")
			},
			hold: func(ctx context.Context, holder pgx.Tx, _, _ string) error {
				_, err := prioritystore.RankAt(ctx, holder, "holding-the-rank", "", "")
				return err
			},
			method: http.MethodPatch, path: "/api/v1/priorities/moving-priority",
			body:    `{"title":"Renamed behind the held lock","rank":{"after":"neighbour-priority"}}`,
			written: http.StatusOK, unchanged: true,
		},
		{
			name: "a session group create behind the session-groups lock",
			hold: func(ctx context.Context, holder pgx.Tx, _, _ string) error {
				_, err := delivery.CreateSessionGroup(ctx, holder, "Holding the lock", []string{"s-holding"}, model.Actor{Kind: "user", ID: "alice"})
				return err
			},
			method: http.MethodPost, path: "/api/v1/delivery/session-groups", body: `{"name":"Grouped behind the held lock","sessions":["s-queued"]}`,
			written: http.StatusCreated, unchanged: true,
		},
		{
			name: "a document edit behind the document's lock",
			hold: func(ctx context.Context, holder pgx.Tx, _, document string) error {
				return docs.LockDocumentRoom(ctx, holder, document)
			},
			method: http.MethodPost, path: "/api/v1/artifacts/{doc}/edits", body: `{"ops":[{"op":"insert","markdown":"Edited behind the held lock.\n","after":"end"}]}`,
			written: http.StatusOK, unchanged: true,
		},
		{
			name: "a document upload behind the document's lock",
			hold: func(ctx context.Context, holder pgx.Tx, _, document string) error {
				return docs.LockDocumentRoom(ctx, holder, document)
			},
			method: http.MethodPost, path: "/api/v1/issues/{key}/artifacts",
			body:    `{"name":"spec.md","content":"Uploaded behind the held lock.\n","precondition":{base}}`,
			written: http.StatusCreated, unchanged: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The seeded spec's settlement takes the owner lock a row holds; keep it out of the queue.
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
			issue := createInteractionIssue(t, handler, "TEST", "Held past the bound", "A spec")
			if row.seed != nil {
				row.seed(t, handler)
			}
			// The seed's creates start a term-statistics refresh that writes search_term_stats and
			// search_term_totals after the create answers: let it end before the fingerprint is read.
			served.detached.wait()
			awaitTermStatsRefresh(t, served, "TEST")
			path := strings.NewReplacer("{key}", issue.Key, "{doc}", issue.PrimaryArtifactID).Replace(row.path)
			body := row.body
			if strings.Contains(body, "{base}") {
				body = strings.ReplaceAll(body, "{base}", encodeUploadBase(t, uploadBase(t, handler, issue.PrimaryArtifactID)))
			}
			send := func() *httptest.ResponseRecorder {
				request := httptest.NewRequest(row.method, path, strings.NewReader(body)).
					WithContext(store.WithLockWaitForTest(context.Background(), bound))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			issues := func() int {
				t.Helper()
				var count int
				if err := database.Pool.QueryRow(context.Background(), `select count(*) from issues where project_key = 'TEST'`).Scan(&count); err != nil {
					t.Fatalf("count the project's issues: %v", err)
				}
				return count
			}
			var before map[string]string
			if row.unchanged {
				before = databaseFingerprint(t, database)
			}
			issuesBefore := issues()
			ctx := context.Background()
			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the first writer: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if err := row.hold(ctx, holder, issue.Key, issue.PrimaryArtifactID); err != nil {
				t.Fatalf("take the lock: %v", err)
			}

			answered := make(chan *httptest.ResponseRecorder, 1)
			started := time.Now()
			go func() { answered <- send() }()
			if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
				t.Fatalf("the second writer never queued behind the lock the first holds")
			}
			var holdingXid int
			if err := holder.QueryRow(ctx, `
				select count(*) from pg_stat_activity
				where datname = current_database() and wait_event_type = 'Lock' and backend_xid is not null
				  and pid <> pg_backend_pid()
			`).Scan(&holdingXid); err != nil {
				t.Fatalf("read the waiters' transaction ids: %v", err)
			}
			if holdingXid != 0 {
				t.Errorf("the second writer waits on the lock holding a transaction id: it wrote, or its bounded wait assigned one, before the lock")
			}
			var response *httptest.ResponseRecorder
			select {
			case response = <-answered:
			case <-time.After(10 * time.Second):
				waited := time.Since(started)
				_ = holder.Rollback(ctx)
				<-answered
				t.Fatalf("the second writer was still waiting on the lock the first holds %s after it was sent, past its wait's "+
					"bound of %s: a writer queued on a busy lock waits until the holder lets go", waited.Round(time.Millisecond), bound)
			}
			waited := time.Since(started)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"`+admission.CodeDatabaseBusy+`"`) ||
				response.Header().Get("Retry-After") == "" {
				t.Fatalf("the second writer queued on the held lock: status=%d Retry-After=%q body=%s, want 503 %s with Retry-After",
					response.Code, response.Header().Get("Retry-After"), response.Body.String(), admission.CodeDatabaseBusy)
			}
			if waited < bound {
				t.Fatalf("the second writer was refused %s after it was sent, inside its wait's bound of %s", waited.Round(time.Millisecond), bound)
			}
			if waiting := storetest.WaitForLockWaiters(t, holder, 0); waiting != 0 {
				t.Fatalf("%d sessions still wait behind the held lock after the refusal, want none", waiting)
			}
			if row.unchanged {
				assertUnchanged(t, "a write refused at its lock's bound", before, databaseFingerprint(t, database))
			}
			if after := issues(); after != issuesBefore {
				t.Fatalf("the project holds %d issues after the refusal, %d before it", after, issuesBefore)
			}

			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("let the lock go: %v", err)
			}
			if again := send(); again.Code != row.written {
				t.Fatalf("the write sent again once the lock was free: status=%d body=%s, want %d", again.Code, again.Body.String(), row.written)
			}
		})
	}
}

// A person marking a thread read while a session's reply to it is in its read phase waits for that
// reply: the mark takes the thread owner's lock, which the reply holds from before its first read
// to its commit. Both succeed, neither is refused as a deadlock, and the mark, made after the
// reply was written, leaves the thread read.
func TestAThreadReadWaitsForAWritersReadPhase(t *testing.T) {
	t.Parallel()
	gate := &gatedProjectMark{entered: make(chan struct{}), release: make(chan struct{})}
	handler, _, database := pooledDocumentHandlerWith(t, 8, time.Hour, func(service *docs.Service) docs.API {
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

	// A reply to an anchored comment projects the thread onto its root's mark (ProjectMark), in
	// its read phase, after it has read its creation time and queued its insert.
	gate.armed.Store(true)
	replies := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replies <- sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
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
	go func() { marks <- markThreadRead(t, handler, "alice", root.ID, mark) }()
	if response, early, _ := storetest.AwaitOwnerLockWaiter(t, database, "the thread read", marks); early {
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

// transactionIDHolders is how many backends of this test's database hold a transaction id, and
// what each of them is running, so a holder this test does not drive is named rather than
// counted alone.
func transactionIDHolders(t *testing.T, database *store.Store) (int, string) {
	t.Helper()
	var holders int
	var running string
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(string_agg(query, ' | '), '') from pg_stat_activity
		where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid()
	`).Scan(&holders, &running); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return holders, running
}

// awaitNoTransactionIDHolder waits until the TEST project's search term statistics are committed
// and then until no backend holds a transaction id. An issue create leaves one write running past
// its answer: the refresh of the project's search term statistics its related-item check starts
// for a project that has none (ensureSearchTermStats), a background write with an id of its own.
// That refresh can begin after the create answers, and its rebuild takes its id only once it
// inserts, so a moment with no holder does not show it has ended; its row of search_term_totals,
// written in the same transaction, does. A test that counts every backend waits it out first, as
// the refusal tests wait it out, so what it counts afterwards is the writers it drives. Each of its
// two waits has one bound, the test binary's deadline (testwait.UntilPolling).
func awaitNoTransactionIDHolder(t *testing.T, database *store.Store) {
	t.Helper()
	testwait.UntilPolling(t, 5*time.Millisecond, "the TEST project's search term statistics to be refreshed (its search_term_totals row)", func() (bool, bool) {
		var refreshed bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from search_term_totals where project_key = 'TEST')
		`).Scan(&refreshed); err != nil {
			t.Fatalf("read search_term_totals: %v", err)
		}
		return refreshed, refreshed
	})
	testwait.UntilPolling(t, 5*time.Millisecond, "no backend to hold a transaction id (the holders and what they run)", func() (string, bool) {
		holders, running := transactionIDHolders(t, database)
		return fmt.Sprintf("%d: %s", holders, running), holders == 0
	})
}

// Two comments on one issue still run one at a time, and the second waits for the first holding no
// transaction id, so a transaction waiting its turn holds back no live update capped by the event
// horizon. Both land, their events in the order they arrived and the issue's sequence without a
// gap. The first is anchored on the spec and held in its read phase; the second is a comment on
// the issue with no anchor, which writes no document. Two comments anchored on the one spec would
// meet first at that document's writer gate (docs.Service.AcquireDocumentWriter, in memory, before
// either opens its transaction), which refuses the second 429 EDIT_QUEUE_FULL once its admission
// wait runs out while the first is held; a comment that writes no document takes no gate, so the
// two meet where this test watches them, at the issue's owner lock.
func TestTwoWritersOfOneOwnerSerializeWithoutATransactionID(t *testing.T) {
	t.Parallel()
	gate := &gatedMarkQuote{entered: make(chan struct{}), release: make(chan struct{})}
	handler, _, database := pooledDocumentHandlerWith(t, 8, time.Hour, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	handler = withDeadline(t, handler)
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Serialized writers", "alpha bravo charlie delta")
	awaitNoTransactionIDHolder(t, database)

	comment := func(anchor map[string]string, body string) *httptest.ResponseRecorder {
		request := map[string]any{"body": body}
		if anchor != nil {
			request["anchor"] = anchor
		}
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", request, "alice")
	}
	firsts := make(chan *httptest.ResponseRecorder, 1)
	go func() { firsts <- comment(map[string]string{"artifact": "spec", "quote": "alpha"}, "first") }()
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
	holders, running := transactionIDHolders(t, database)
	if holders != 0 {
		t.Fatalf("%d backends hold a transaction id while the first comment is in its read phase, want none: %s", holders, running)
	}

	seconds := make(chan *httptest.ResponseRecorder, 1)
	go func() { seconds <- comment(nil, "second") }()
	response, early, holdingXid := storetest.AwaitOwnerLockWaiter(t, database, "the second comment", seconds)
	if early {
		t.Fatalf("the second comment answered %d %s without waiting for the owner lock its writer holds", response.Code, response.Body.String())
	}
	if holdingXid != 0 {
		t.Fatalf("the second comment waits on the owner lock holding a transaction id")
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
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Two patches", "A spec")
	awaitNoTransactionIDHolder(t, database)

	holder, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if err := store.LockOwner(context.Background(), holder, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("take the issue's owner lock: %v", err)
	}

	addLabel := func(label string, answered chan<- *httptest.ResponseRecorder) {
		answered <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
			map[string]any{"labels_add": []string{label}}, "alice")
	}
	answers := make(chan *httptest.ResponseRecorder, 2)
	go addLabel("alpha", answers)
	go addLabel("bravo", answers)
	testwait.UntilPolling(t, 5*time.Millisecond, "two PATCHes to wait on the issue's owner lock (the waiters)", func() (int, bool) {
		select {
		case response := <-answers:
			t.Fatalf("a PATCH answered %d %s while another transaction held the issue's owner lock", response.Code, response.Body.String())
		default:
		}
		waiting, holdingXid := storetest.OwnerLockWaiters(t, database)
		if holdingXid != 0 {
			t.Fatalf("%d of %d PATCHes wait on the issue's owner lock holding a transaction id, want none", holdingXid, waiting)
		}
		return waiting, waiting >= 2
	})
	var lastSeqBefore int
	if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue.Key).Scan(&lastSeqBefore); err != nil {
		t.Fatalf("read the issue's sequence: %v", err)
	}
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("release the issue's owner lock: %v", err)
	}
	for range 2 {
		if response := awaitResponse(t, answers); response.Code != http.StatusOK {
			t.Fatalf("PATCH answered %d %s, want 200", response.Code, response.Body.String())
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
	t.Parallel()
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
		handler, database, _, served := newInteractionServer(t, func(database *store.Store) docs.API {
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
		// The issue's create starts the term-statistics refresh; it has committed before the
		// accept starts, so the fingerprint below holds none of its writes.
		awaitBackgroundWrites(t, served)

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
		handler, database, _, served := newInteractionServer(t, func(database *store.Store) docs.API {
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
		awaitBackgroundWrites(t, served)
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
