package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// testDrainLockWait is the bound the cases below hold a lock past: short, so a case meets it
// quickly, and long against the fill's and the drain's own work on a free lock.
const testDrainLockWait = 300 * time.Millisecond

// drainHook is where a case takes the issue's owner lock behind an edit's COMMIT: at the start of
// the edit's fill (docs.Service.HoldFills), the drain's first wait, or at the start of its drain,
// once the fill has committed (docs.Service.HoldDrains).
type drainHook func(*docs.Service, func(issueKey string))

var (
	atTheFill  drainHook = (*docs.Service).HoldFills
	atTheDrain drainHook = (*docs.Service).HoldDrains
)

// heldOwnerLock is the issue's owner lock a hook took in a transaction of the test's own: held is
// closed once the lock is taken or the take failed (err), and release rolls the transaction back.
type heldOwnerLock struct {
	held    chan struct{}
	err     error
	holder  pgx.Tx
	release func()
}

// holdOwnerLockAt makes hook take the owner lock of issueKey, once, in a transaction of the test's
// own, the first time it runs for that issue, on the request's goroutine, and keeps it until
// release, which the test's cleanup runs too. The hook is taken off once it has run.
func holdOwnerLockAt(t *testing.T, documents *docs.Service, database *store.Store, hook drainHook, issueKey string) *heldOwnerLock {
	t.Helper()
	lock := &heldOwnerLock{held: make(chan struct{})}
	var armed atomic.Bool
	armed.Store(true)
	var released atomic.Bool
	lock.release = func() {
		if released.CompareAndSwap(false, true) {
			<-lock.held
			if lock.holder != nil {
				_ = lock.holder.Rollback(context.Background())
			}
		}
	}
	hook(documents, func(key string) {
		if key != issueKey || !armed.CompareAndSwap(true, false) {
			return
		}
		defer close(lock.held)
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			lock.err = err
			return
		}
		lock.holder = tx
		lock.err = store.LockOwner(ctx, tx, store.IssueOwner(key))
	})
	t.Cleanup(func() {
		hook(documents, nil)
		if armed.CompareAndSwap(true, false) {
			close(lock.held)
		}
		lock.release()
	})
	return lock
}

// writeCriterionAsync writes, as alice through POST /api/v1/artifacts/{tree}/edits, a criterion id
// under a new person's comment of the rig's issue into the issue's tree artifact, which moves the
// Spec query's record and marks the issue's queries owed (docs' markRoomQueries), on a goroutine
// of its own, and answers the channel its answer arrives on. It answers too the Spec record's
// digest before the write and the spec's newest version then.
func writeCriterionAsync(t *testing.T, rig *digestRig, id, words string) (<-chan *httptest.ResponseRecorder, string, int) {
	t.Helper()
	key := rig.issue.Key
	tree, err := issueTreeOf(context.Background(), rig.database.Pool, key)
	if err != nil || tree == "" {
		t.Fatalf("read the tree artifact of %s: %q, %v", key, tree, err)
	}
	_, block := personComment(t, rig.handler, rig.database, key, "We want "+words+".\n\nAnd we want it soon.")
	awaitDigestsFilled(t, rig.database, rig.spec.id)
	was := rig.record(t, key, graphstore.QuerySpec).Digest
	before := newestVersionOf(t, rig.database, rig.spec.id)
	answered := make(chan *httptest.ResponseRecorder, 1)
	body := insertAtEnd(":::criterion{#" + id + " parent=\"" + block + "\"}\nI want " + words + ".\n:::\n")
	go func() { answered <- editDocument(t, rig.handler, tree, body) }()
	return answered, was, before
}

// requireEditAnswersPastItsDrainsBound is the case of an owner lock held past the bound at hook:
// the edit's drain, or its fill before it, waits for the issue's owner lock at most wait
// (docs.Service.SetAfterCommitLockWaitForTest; testDrainLockWait here, and a negative value, which
// takes the bound out, in a control), so the edit answers 200 while the lock is still held - the
// test lets it go only after the answer - with no version of the Spec query written, the issue's
// mark standing and one Warn line naming the issue and the bound. Once the lock goes, the tick of
// the block-index reconciliation writes the version and clears the mark. A drain with no bound
// waits for the lock until the test's deadline, and the wait for the answer then fails by name.
func requireEditAnswersPastItsDrainsBound(t *testing.T, rig *digestRig, logs interface{ Lines(string) string }, hook drainHook, id string, wait time.Duration) {
	t.Helper()
	key := rig.issue.Key
	rig.documents.SetAfterCommitLockWaitForTest(wait)
	t.Cleanup(func() { rig.documents.SetAfterCommitLockWaitForTest(0) })
	lock := holdOwnerLockAt(t, rig.documents, rig.database, hook, key)
	const gaveUp = "waited past its bound for a lock and left them owed to the block-index tick"
	logged := len(logs.Lines(gaveUp))
	answered, was, before := writeCriterionAsync(t, rig, id, "the urgent asks first, "+id)
	testwait.Receive(t, "the hook to take the owner lock of "+key+" behind the edit's COMMIT", lock.held)
	if lock.err != nil {
		t.Fatalf("take the owner lock of %s behind the edit's COMMIT: %v", key, lock.err)
	}
	took := time.Now()
	edited := testwait.Receive(t, "the edit's answer while the owner lock of "+key+" is held past its drain's bound", answered)
	t.Logf("the edit answered %s after the owner lock was taken behind its COMMIT, the bound being %s", time.Since(took), wait)
	if edited.Code != http.StatusOK {
		t.Fatalf("write the criterion %s: status=%d body=%s", id, edited.Code, edited.Body.String())
	}
	if _, held := readQueryMarkRow(t, rig.database, key); !held {
		t.Errorf("the edit's drain met its bound and no mark of %s stands: the tick has nothing to drain", key)
	}
	if newest := newestVersionOf(t, rig.database, rig.spec.id); newest != before {
		t.Errorf("the edit's drain met its bound and left version %d of the spec of %s newest, want %d, the version before the edit", newest, key, before)
	}
	line := logs.Lines(gaveUp)[logged:]
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "issue="+key+" ") || !strings.Contains(line, "bound="+wait.String()) || !strings.Contains(line, "SQLSTATE 55P03") {
		t.Errorf("the log gained %q for the drain that met its bound, want one line saying it %s, naming issue=%s, bound=%s and the lock timeout (55P03)", line, gaveUp, key, wait)
	}
	lock.release()
	tick, err := rig.documents.ReconcileBlockIndex(context.Background(), docs.BlockIndexCursors{}, nil)
	if err != nil {
		t.Fatalf("a tick of the block-index reconciliation: %v", err)
	}
	if tick.Queries != 1 {
		t.Errorf("the tick drained %d marks, want the one the edit's drain left on %s", tick.Queries, key)
	}
	requireQueryVersionFollows(t, rig, "the tick after the edit's drain met its bound", key, graphstore.QuerySpec, rig.spec.id, before+1, was)
}

// TestTheDrainAfterAnEditsCommitWaitsForTheOwnerLockOnlyUpToItsBound holds R7's drain after an
// edit's COMMIT to the bound its lock waits keep (docs' afterCommitLockWait), on an issue born
// converted, through the edit route of a server newTestServer builds. A person writes a criterion
// into the issue's tree artifact, which moves the Spec query's record, so the edit marks the
// issue's queries owed and drains them before it answers (docs.Ledger.Commit). The test takes the
// issue's owner lock behind the edit's COMMIT in a transaction of its own:
//
//   - held past the bound at the drain, after the fill committed, and
//   - held past the bound at the fill, the drain's first wait:
//     the edit answers while the lock is still held, the mark stands, and one Warn line names the
//     issue and the bound (requireEditAnswersPastItsDrainsBound); the tick then writes the version.
//   - let go inside the bound, once the drain waits on it: the drain writes the Spec query's version
//     and clears the mark before the edit answers, as it does with no lock held.
//
// Not parallel: it reads what the default logger writes (captureLogs).
func TestTheDrainAfterAnEditsCommitWaitsForTheOwnerLockOnlyUpToItsBound(t *testing.T) {
	logs := captureLogs(t)
	rig := newQueryMarkRig(t)

	t.Run("a lock held past the bound at the drain", func(t *testing.T) {
		requireEditAnswersPastItsDrainsBound(t, rig, logs, atTheDrain, "k1", testDrainLockWait)
	})
	t.Run("a lock held past the bound at the fill", func(t *testing.T) {
		requireEditAnswersPastItsDrainsBound(t, rig, logs, atTheFill, "k2", testDrainLockWait)
	})
	t.Run("a lock released inside the bound", func(t *testing.T) {
		key := rig.issue.Key
		lock := holdOwnerLockAt(t, rig.documents, rig.database, atTheDrain, key)
		answered, was, before := writeCriterionAsync(t, rig, "k3", "the urgent asks first, k3")
		testwait.Receive(t, "the hook to take the owner lock of "+key+" behind the edit's COMMIT", lock.held)
		if lock.err != nil {
			t.Fatalf("take the owner lock of %s behind the edit's COMMIT: %v", key, lock.err)
		}
		awaitLockWaiters(t, rig.database, 1)
		select {
		case edited := <-answered:
			t.Fatalf("the edit answered %d while its drain waited for the owner lock inside its bound: body=%s", edited.Code, edited.Body.String())
		default:
		}
		lock.release()
		edited := testwait.Receive(t, "the edit's answer once the owner lock of "+key+" went inside its drain's bound", answered)
		if edited.Code != http.StatusOK {
			t.Fatalf("write the criterion k3: status=%d body=%s", edited.Code, edited.Body.String())
		}
		requireQueryVersionFollows(t, rig, "the edit whose drain waited inside its bound", key, graphstore.QuerySpec, rig.spec.id, before+1, was)
	})
}
