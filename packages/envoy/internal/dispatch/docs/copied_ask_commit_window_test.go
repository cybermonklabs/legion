package docs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The settlement that opens a copy's own ask forgets the copy room's owed author of the block
// under the room's lock it holds across the commit (consumeAskAuthorsLocked, commitSettlement), so
// no reader that finds the ask row committed finds the room still owing the block's author. The
// hook reads both at the first moment after the commit that another reader can (the room's lock
// released, the settlement not yet finished). TestACopiedAskBlocksAuthorIsKeptForTheAskItOpens-
// OnceItsSourceIsRetracted reads the room only once the settlement has ended, so it cannot see
// that moment; this test does.
func TestNoReaderFindsACopiedAsksRowWhileItsRoomStillOwesTheAuthor(t *testing.T) {
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	service, original := newTestService(t)
	shared := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	seedServiceText(t, service, original, "Context\n\n"+shared)
	settleCurrentGeneration(t, service, original)

	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Context\n")
	settleCurrentGeneration(t, service, copied)
	if _, err := joinedApplyOps(service, copied, []model.EditOp{
		{Op: "insert", After: "end", Markdown: shared},
	}, session, nil); err != nil {
		t.Fatalf("the session's copy: %v", err)
	}
	settleCurrentGeneration(t, service, copied)
	state := service.room(copied)
	state.mu.Lock()
	owed, recorded := state.askAuthors["shared"]
	state.mu.Unlock()
	if !recorded || owed != session {
		t.Fatalf("the copy's room owes the block to %#v (recorded %v), want the session", owed, recorded)
	}

	// The hook runs on the copy's settlement's goroutine, so it records what it read and the test
	// goroutine judges it. found counts the reads that found the copy's ask row committed; owing
	// counts those of them that found the room still owing the block's author.
	var found, owing atomic.Int64
	var readErrors sync.Map
	service.afterSettlementCommit = func(room string) {
		if room != copied {
			return
		}
		var count int
		if err := service.store.Pool.QueryRow(context.Background(),
			`select count(*) from asks where block_artifact_id = $1`, copied).Scan(&count); err != nil {
			readErrors.Store(err.Error(), true)
			return
		}
		if count == 0 {
			return
		}
		found.Add(1)
		state.mu.Lock()
		_, still := state.askAuthors["shared"]
		state.mu.Unlock()
		if still {
			owing.Add(1)
		}
	}
	t.Cleanup(func() { service.afterSettlementCommit = nil })

	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	requireOneOpenAskOn(t, service, "shared", copied)
	waitFor(t, 10*time.Second, "the copy's settlement that opened its ask to end", func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.settling == 0
	})
	readErrors.Range(func(message, _ any) bool {
		t.Errorf("the hook's read of the copy's asks: %s", message)
		return true
	})
	if found.Load() == 0 {
		t.Fatalf("no settlement of the copy committed its ask row with the hook set, so the test read nothing")
	}
	if n := owing.Load(); n != 0 {
		t.Errorf("%d of %d reads after the commit found the copy's ask row committed and its room still owing the block's author", n, found.Load())
	}
}
