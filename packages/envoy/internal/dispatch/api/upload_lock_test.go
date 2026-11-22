package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A project document's owner is the artifact itself, and the upload has to take its owner lock
// before the document's room lock. A settlement holds that owner lock and then takes the room; an
// upload that took the room first - locking no owner until the event it appends at the end - would
// close a cycle with it, which Postgres breaks with `deadlock detected`, a 500 from this path under
// load. Holding the owner lock here must therefore stop the upload before the room lock, leaving
// the room free to take behind it.
func TestProjectDocumentUploadTakesItsOwnerRowBeforeTheRoomLock(t *testing.T) {
	ctx := uploadLockContext(t)
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "LOCK", "name": "Lock order",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "LOCK", "Notes", "# Notes\n")
	base := uploadBase(t, handler, document.ID)

	settling, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settlement-shaped transaction: %v", err)
	}
	defer settling.Rollback(context.Background())
	if err := store.LockOwner(ctx, settling, store.ArtifactOwner(document.ID)); err != nil {
		t.Fatalf("hold the document owner: %v", err)
	}

	// The upload runs on a connection of its own, and the wait watches its backend
	// (storetest.WaitForQueuedBehind): the upload blocks on the owner lock this transaction holds,
	// and no other session queued there is taken for it.
	uploadHandler, uploadPID := oneConnectionHandler(t, deps)
	uploaded := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		uploaded <- dispatchRequest(t, uploadHandler, http.MethodPost, "/api/v1/projects/LOCK/artifacts", map[string]any{
			"name": "Notes", "content": "# Notes\n\nsecond\n", "precondition": base,
		}, "alice")
	}()
	if response, early := storetest.WaitForQueuedBehind(t, settling, uploadPID, uploaded); early {
		t.Fatalf("the upload answered %d %s before it queued behind the document's owner lock", response.Code, response.Body.String())
	}

	if _, err := settling.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, document.ID); err != nil {
		t.Fatalf("take the room lock behind the upload: %v", err)
	}
	if err := settling.Rollback(context.Background()); err != nil {
		t.Fatalf("release the document owner: %v", err)
	}

	response := <-uploaded
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
}

// An upload of a project document holding a copied ask block and an answer to the block's source
// meet on the copy's pending-settlement row, and both take it before their events. The upload
// takes it with its document write (appendUpdateTxClass), before it appends its event; the answer
// takes it under the project's copy lock (docs.SettleCopiesOf), before it appends its own. The test
// holds the upload between its document write and its event and the answer just before its event,
// lets the answer reach the row, then lets the upload's event go first and the answer's after it:
// both finish. Their events share no lock: each append locks only its own owner's row, the copy
// for the upload and the source for the answer. So even an upload that took the row only at its
// commit (Ledger.Commit's recordLatestEditSource), after its event, would wait for the answer and
// then finish; what fails this test is a lock the two come to share in opposite orders.
func TestAnUploadOfACopyAndAnAnswerToItsSourceBothTakeTheCopysPendingRowBeforeTheirEvents(t *testing.T) {
	ctx := uploadLockContext(t)
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	handler, database, deps := newTestServer(t, testServerOptions{})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "COPY", "name": "Copies",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	block := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	source := createProjectDocument(t, handler, "COPY", "Source", "Context\n\n"+block)
	awaitIndexedAskBlock(t, handler, source.ID, "shared", "Which region?")
	var ask string
	if err := database.Pool.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1`, source.ID).Scan(&ask); err != nil {
		t.Fatalf("read the source ask: %v", err)
	}
	copied := createProjectDocument(t, handler, "COPY", "Copy", "Kept\n\n"+block)
	// The copy settles as a copy of the source's ask, and no document owes a settlement once it has.
	awaitCopyShows(t, ctx, database, copied.ID, `state="open" copied_from="`+ask+`"`)

	// Each hold is an advisory lock the test takes, which a trigger makes one statement wait for: the
	// upload's version insert, after its document write and before its event, and the answer's write
	// of its ask row, after it marked the copy owed and before its event.
	const holdUpload, holdAnswer = int64(4815162343), int64(4815162344)
	for _, statement := range []string{
		`create function dispatch_test_hold() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(tg_argv[0]::bigint);
			return new;
		end;
		$$`,
		fmt.Sprintf(`create trigger dispatch_test_hold_upload before insert on artifact_versions for each row
			when (new.artifact_id = '%s') execute function dispatch_test_hold('%d')`, copied.ID, holdUpload),
		fmt.Sprintf(`create trigger dispatch_test_hold_answer before update on asks for each row
			when (new.id = '%s' and new.state = 'answered') execute function dispatch_test_hold('%d')`, ask, holdAnswer),
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the upload and the answer before their events: %v", err)
		}
	}
	hold := func(key int64) pgx.Tx {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin a hold: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, key); err != nil {
			t.Fatalf("take a hold: %v", err)
		}
		return tx
	}
	release := func(tx pgx.Tx) {
		t.Helper()
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("release a hold: %v", err)
		}
	}
	copyBase := uploadBase(t, handler, copied.ID)
	uploadHold, answerHold := hold(holdUpload), hold(holdAnswer)
	// The upload and the answer each run on a connection of their own, and each wait below watches
	// those two backends (storetest.QueuedBehind), so no other session the holds stop - a document
	// settlement writing a version of the copy, whose insert the same trigger holds - is taken for
	// either.
	uploadHandler, uploadPID := oneConnectionHandler(t, deps)
	answerHandler, answerPID := oneConnectionHandler(t, deps)
	queued := func(holder pgx.Tx, pid uint32) bool {
		t.Helper()
		waiting, err := storetest.QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatal(err)
		}
		return waiting
	}
	upload := startRequest(func() *httptest.ResponseRecorder {
		return dispatchRequest(t, uploadHandler, http.MethodPost, "/api/v1/projects/COPY/artifacts", map[string]any{
			"name": "Copy", "content": "Kept again\n\n" + block, "precondition": copyBase,
		}, "alice")
	})
	// Only the trigger on the copy's version insert waits on the upload's hold, so the upload
	// queued behind it is between its document write and its event.
	testwait.UntilPolling(t, 2*time.Millisecond, "the upload to wait between its document write and its event (queued behind its hold)", func() (bool, bool) {
		held := upload.finished() || queued(uploadHold, uploadPID)
		return held, held
	})
	if upload.finished() {
		t.Fatalf("the upload finished before its event could be held: %s", upload)
	}
	answer := startRequest(func() *httptest.ResponseRecorder {
		return dispatchRequest(t, answerHandler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{
			"text": "eu-west-1",
		}, "alice")
	})
	// The answer waits on the copy's pending row the upload holds, and so queues behind the
	// upload's hold; had the upload not taken it, the answer would mark it and wait before its own
	// event instead, behind its own hold.
	testwait.UntilPolling(t, 2*time.Millisecond, "the answer to reach the copy's pending row (queued behind a hold)", func() (bool, bool) {
		held := answer.finished() || queued(uploadHold, answerPID) || queued(answerHold, answerPID)
		return held, held
	})
	release(uploadHold)
	// The upload's event goes first: the upload commits, or it waits on the row the answer marked
	// while the answer waits before its event, both then queued behind the answer's hold.
	testwait.UntilPolling(t, 2*time.Millisecond, "the upload to finish or wait on the copy's pending row (queued behind the answer's hold)", func() (bool, bool) {
		held := upload.finished() || (queued(answerHold, uploadPID) && queued(answerHold, answerPID))
		return held, held
	})
	release(answerHold)
	for _, request := range []*pendingRequest{upload, answer} {
		select {
		case <-request.done:
		case <-ctx.Done():
			t.Fatalf("the upload (%s) and the answer (%s) never both finished", upload, answer)
		}
	}
	if upload.response.Code != http.StatusCreated || answer.response.Code != http.StatusOK {
		t.Fatalf("upload: %s\nanswer: %s\nwant 201 and 200; the server logged:\n%s", upload, answer, logs.Lines("API handler failed"))
	}
	awaitCopyShows(t, ctx, database, copied.ID, `state="answered" answered_by="alice"`)
}

// pendingRequest is a request a test runs on a goroutine of its own.
type pendingRequest struct {
	done     chan struct{}
	response *httptest.ResponseRecorder
}

func startRequest(send func() *httptest.ResponseRecorder) *pendingRequest {
	request := &pendingRequest{done: make(chan struct{})}
	go func() {
		request.response = send()
		close(request.done)
	}()
	return request
}

func (r *pendingRequest) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *pendingRequest) String() string {
	if !r.finished() {
		return "still running"
	}
	return fmt.Sprintf("status=%d body=%s", r.response.Code, strings.TrimSpace(r.response.Body.String()))
}

// uploadLockContext is the context of an upload-lock row's own database calls and waits, some of
// which wait on a lock the code under test may hold: it ends binaryDeadlineMargin before the test
// binary's deadline, the bound of every wait of the package's tests, so such a call fails the row
// by name rather than outlasting the binary, and it ends with the test.
func uploadLockContext(t *testing.T) context.Context {
	t.Helper()
	ctx := t.Context()
	if deadline, bounded := t.Deadline(); bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-binaryDeadlineMargin))
		t.Cleanup(cancel)
	}
	return ctx
}

// awaitLockWaits waits until reached holds, given how many sessions of the test's database wait on
// a lock whose statement matches a LIKE pattern (storetest.CountLockWaits), asking every 2 ms;
// what names the fact. Its one bound is the test binary's deadline (testwait.UntilPolling).
func awaitLockWaits(t *testing.T, ctx context.Context, database *store.Store, what string, reached func(waiting func(like string) int) bool) {
	t.Helper()
	waiting := func(like string) int {
		count, err := storetest.CountLockWaits(ctx, database, like)
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	testwait.UntilPolling(t, 2*time.Millisecond, what, func() (bool, bool) {
		held := reached(waiting)
		return held, held
	})
}

// awaitCopyShows waits for the latest version of copy to hold want, with no document owing a
// settlement, reading both every 20 ms. Its one bound is the test binary's deadline
// (testwait.UntilPolling).
func awaitCopyShows(t *testing.T, ctx context.Context, database *store.Store, copy, want string) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("the copy's latest version to hold %s with nothing owed (owed, then the version)", want), func() (string, bool) {
		var markdown string
		var pending int
		if err := database.Pool.QueryRow(ctx, `
			select (select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1),
				(select count(*) from doc_settlements_pending)
		`, copy).Scan(&markdown, &pending); err != nil {
			t.Fatalf("read the copy's latest version: %v", err)
		}
		return fmt.Sprintf("%d owed:\n%s", pending, markdown), strings.Contains(markdown, want) && pending == 0
	})
}

// The upload holds the document's owner lock across the room lock it then waits for, so that lock
// must not block the key share a durable writer holding the room takes on the same row through
// doc_updates' foreign key: while the owner lock was a `for update` row lock that key share
// conflicted and the two deadlocked. The owner's advisory lock takes no row lock at all.
func TestProjectDocumentUploadOwnerLockLeavesTheForeignKeyFree(t *testing.T) {
	ctx := uploadLockContext(t)
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "LEVEL", "name": "Lock level",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "LEVEL", "Notes", "# Notes\n")
	base := uploadBase(t, handler, document.ID)

	appending, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the durable writer: %v", err)
	}
	defer appending.Rollback(context.Background())
	if _, err := appending.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, document.ID); err != nil {
		t.Fatalf("hold the document room lock: %v", err)
	}

	// The upload runs on a connection of its own, and the wait watches its backend
	// (storetest.WaitForQueuedBehind): the upload takes the owner lock, then waits here for the
	// room, and no other session queued on the room is taken for it.
	uploadHandler, uploadPID := oneConnectionHandler(t, deps)
	uploaded := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		uploaded <- dispatchRequest(t, uploadHandler, http.MethodPost, "/api/v1/projects/LEVEL/artifacts", map[string]any{
			"name": "Notes", "content": "# Notes\n\nsecond\n", "precondition": base,
		}, "alice")
	}()
	if response, early := storetest.WaitForQueuedBehind(t, appending, uploadPID, uploaded); early {
		t.Fatalf("the upload answered %d %s before it queued behind the document's room lock", response.Code, response.Body.String())
	}

	if _, err := appending.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed)
		values ($1, (select coalesce(max(version), 0) + 1 from doc_updates where artifact_id = $1), $2, true)
	`, document.ID, []byte{0}); err != nil {
		t.Fatalf("append while the upload holds the document owner: %v", err)
	}
	if err := appending.Rollback(context.Background()); err != nil {
		t.Fatalf("release the room lock: %v", err)
	}

	response := <-uploaded
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
}
