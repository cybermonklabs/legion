// priority_patch_lock_order_test.go proves the order PATCH /api/v1/priorities/{id} takes its locks
// in (priorities.go, patchPriority): the order's allocation lock, then the priority's row. A
// rank-only patch takes them so (prioritystore.Move: RankAt's lock, then moveNode's update of the
// row), and a patch naming expected and a rank reads the row under its lock before it moves
// (prioritystore.ReadForWrite), so it takes the allocation lock before that read: taking the row
// first, as main did, leaves it holding the row while it waits for a lock a rank-only patch holds
// while it waits for that row.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/testwait"
)

// patchAnswer is one of the row's patches and what the server answered it.
type patchAnswer struct {
	patch    string
	response *httptest.ResponseRecorder
}

// Three patches of one priority P meet: A names expected (the title) and a rank (a place after the
// first priority), B a rank alone (a place before the third), and C expected alone (the
// description). A is held where its check has passed (server.priorityPatchChecked), holding what
// the check took: the allocation lock and P's row. The row starts B and waits until the database
// shows it waiting on a lock, then starts C and waits until it waits too, and only then lets A go.
// B waits on the allocation lock and C on the row. A's write finds both locks already its own and
// commits; B and C then each take the lock they waited for, one after the other: C reads P's row,
// which holds what A committed, and B's move updates the same row. All three are answered 200, and
// P holds A's title, C's description and the rank of whichever move committed last, each move's key
// in the place its patch named: a rank given on one side only is a valid place whichever of the
// moves lands first.
//
// Taken the other way (main's: the row, then the allocation lock), A holds P's row while B takes
// the allocation lock and waits for that row; A's move then waits for B's lock and B's update for
// A's row, and Postgres ends the cycle by failing one of them ("deadlock detected", answered 500).
// The row builds that situation by the order of events, not by a machine's speed: it starts each
// patch only once the one before is held or waiting, and its one bound is the test binary's
// deadline. Each request's lock waits are past any run (statementBoundsPastTheRun), so a wait that
// ran out its ten seconds on a slow machine cannot refuse a request and read as one the order
// caused.
func TestThreePatchesOfOnePriorityNeverDeadlockOrRunOutALockWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var (
		parked  atomic.Bool
		reached = make(chan struct{})
		release = make(chan struct{})
		letGo   = sync.OnceFunc(func() { close(release) })
	)
	// The first patch to pass its check is A's, which the row starts alone; it is held until the
	// row lets it go, or until the test ends, which cancels t.Context before the cleanups run.
	hold := func(string) {
		if !parked.CompareAndSwap(false, true) {
			return
		}
		close(reached)
		select {
		case <-release:
		case <-t.Context().Done():
		}
	}
	served, database, _ := newTestServer(t, testServerOptions{priorityPatchChecked: hold})
	handler := statementBoundsPastTheRun(served)
	t.Cleanup(letGo)

	const id = "lock-order-patched"
	first := createTestPriority(t, handler, map[string]any{"id": "lock-order-first", "title": "First"}, "alice")
	second := createTestPriority(t, handler, map[string]any{"id": "lock-order-second", "title": "Second"}, "alice")
	third := createTestPriority(t, handler, map[string]any{"id": "lock-order-third", "title": "Third"}, "alice")
	createTestPriority(t, handler, map[string]any{"id": id, "title": "Patched", "description": "Described"}, "alice")
	acts := len(readPriorityHistory(t, handler, id))

	answers := make(chan patchAnswer, 3)
	start := func(patch, login string, body map[string]any) {
		go func() {
			answers <- patchAnswer{patch, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/"+id, body, login)}
		}()
	}
	// early fails a row whose patch was answered before the row let A go: a patch answered then
	// is waiting on nothing, so the wait the row is in would never end.
	early := func(step string) {
		t.Helper()
		select {
		case answer := <-answers:
			t.Fatalf("patch %s was answered status=%d body=%s %s", answer.patch, answer.response.Code, answer.response.Body.String(), step)
		default:
		}
	}
	// waiting is how many backends of the database wait on a lock a statement of a priority asked
	// for, whichever lock: the allocation lock's advisory wait or a wait on the row.
	waiting := func() int {
		var count int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid()
				and wait_event_type = 'Lock' and query like '%priority%'
		`).Scan(&count); err != nil {
			t.Fatalf("read the patches waiting on a lock: %v", err)
		}
		return count
	}
	awaitWaiting := func(want int, what string) {
		t.Helper()
		testwait.UntilPolling(t, 10*time.Millisecond, what+" (the backends waiting on a lock a priority's statement asked for)", func() (int, bool) {
			early(what + ": a patch answered while the row still held A")
			count := waiting()
			return count, count == want
		})
	}

	start("A", "alice", map[string]any{
		"title": "Title from A", "expected": map[string]any{"title": "Patched"}, "rank": map[string]any{"after": first.ID},
	})
	testwait.UntilPolling(t, 10*time.Millisecond, "patch A to pass its check and be held", func() (bool, bool) {
		select {
		case <-reached:
			return true, true
		default:
		}
		early("before it was held")
		return false, false
	})
	start("B", "bob", map[string]any{"rank": map[string]any{"before": third.ID}})
	awaitWaiting(1, "patch B to wait on a lock while A is held")
	start("C", "alice", map[string]any{
		"description": "Description from C", "expected": map[string]any{"description": "Described"},
	})
	awaitWaiting(2, "patches B and C to wait on a lock while A is held")
	letGo()

	byPatch := map[string]*httptest.ResponseRecorder{}
	for range 3 {
		answer := testwait.Receive(t, "a patch of priority "+id+" to be answered once A was let go", answers)
		byPatch[answer.patch] = answer.response
	}
	for _, patch := range []string{"A", "B", "C"} {
		if response := byPatch[patch]; response.Code != http.StatusOK {
			t.Errorf("patch %s: status=%d body=%s, want 200: three patches of one priority, one holding the row and the order's lock, wait for one another without a cycle",
				patch, response.Code, response.Body.String())
		}
	}
	if t.Failed() {
		return
	}

	answerA, answerB := decodeBody[model.Priority](t, byPatch["A"]), decodeBody[model.Priority](t, byPatch["B"])
	if answerA.Rank <= first.Rank || answerA.Rank >= second.Rank {
		t.Errorf("patch A, a place after %s, answered rank %q, want one between %q and %q", first.ID, answerA.Rank, first.Rank, second.Rank)
	}
	if answerB.Rank <= second.Rank || answerB.Rank >= third.Rank {
		t.Errorf("patch B, a place before %s, answered rank %q, want one between %q and %q", third.ID, answerB.Rank, second.Rank, third.Rank)
	}
	history := readPriorityHistory(t, handler, id)
	if got := len(history) - acts; got != 4 {
		t.Errorf("the three patches left %d acts in the history, want 4: A's title and rank, B's rank and C's description", got)
	}
	lastRank := ""
	for _, act := range history {
		if act.Field == prioritystore.AttributeRank {
			lastRank = act.Value
			break
		}
	}
	stored := readTestPriority(t, handler, id)
	if stored.Title != "Title from A" || stored.Description != "Description from C" {
		t.Errorf("the priority reads title %q description %q, want A's title and C's description", stored.Title, stored.Description)
	}
	if stored.Rank != lastRank || (lastRank != answerA.Rank && lastRank != answerB.Rank) {
		t.Errorf("the priority reads rank %q, the last rank its history states is %q, and the moves answered %q and %q: want the rank of the move that committed last",
			stored.Rank, lastRank, answerA.Rank, answerB.Rank)
	}
}
