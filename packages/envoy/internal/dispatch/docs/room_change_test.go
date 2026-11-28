package docs

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// dropBlock returns a live edit that removes the block whose text starts with prefix, as a
// browser does when it selects a paragraph and hits delete. A deletion creates no new Yjs
// structs, so the update it produces leaves the room's state vector exactly where it was.
// That is what makes it the case a clock-only signal cannot see.
func dropBlock(t *testing.T, prefix string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	return func(tree *pmdoc.Node) *pmdoc.Node {
		kept := tree.Children[:0]
		for _, child := range tree.Children {
			rendered, err := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{child}})
			if err != nil {
				t.Fatalf("render candidate block: %v", err)
			}
			if strings.HasPrefix(strings.TrimSpace(rendered), prefix) {
				continue
			}
			kept = append(kept, child)
		}
		tree.Children = kept
		return tree
	}
}

// A browser DELETE the room takes after a transaction's write read the document is not in the
// transaction's version: the version is the write's own state, the fold of the stored updates
// through the write's last (captureWrite), and the browser's update is stored after the write's.
// The settlement that follows the write's publish versions the deletion. A deletion creates no Yjs
// struct and advances no client's clock, the case a clock-only signal would miss.
func TestADeletionTheRoomTakesAfterAWritesReadIsVersionedAfterTheWrite(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n\ndoomed paragraph")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	testcleanup.Add(t, func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	testcleanup.Add(t, ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, dropBlock(t, "doomed paragraph"))

	captured, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	markdown := captured.markdown
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if !strings.Contains(markdown, "doomed paragraph") || !strings.Contains(markdown, "after") {
		t.Fatalf("snapshot markdown = %q, want the write's own state: its edit, and the paragraph the browser deleted after its read", markdown)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the joined edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if latest := latestVersionMarkdown(t, service, artifactID); strings.Contains(latest, "doomed paragraph") || !strings.Contains(latest, "after") {
		t.Fatalf("the settlement's version = %q, want the write's edit with the browser's deletion", latest)
	}
}

// A browser deletion the room takes after a transaction's write read the document is not in the
// transaction's version, which is the write's own state: its anchors stand as the write's own
// refresh left them, so the ask anchored to the deleted paragraph is still anchored there. The
// version the settlement then writes holds the deletion, and its refresh, the only one that sees
// the quote gone, must orphan the ask.
func TestAnAnchorWhoseQuoteTheRoomDeletedIsOrphanedByTheVersionWrite(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n\nthe anchored quote paragraph")

	askID := "00000000-0000-4000-8000-0000000000b1"
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedAnchoredAsk(t, service, artifactID, askID, "the anchored quote paragraph", "the anchored quote paragraph")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	testcleanup.Add(t, func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	testcleanup.Add(t, ledger.Discard)

	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, dropBlock(t, "the anchored quote paragraph"))

	if _, err := service.SnapshotVersion(joinedCtx, artifactID, alice); err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	anchorOf := func(q Queryer) model.Anchor {
		t.Helper()
		var encoded []byte
		if err := q.QueryRow(ctx, `select anchor from asks where id = $1`, askID).Scan(&encoded); err != nil {
			t.Fatalf("read the ask's anchor: %v", err)
		}
		var anchor model.Anchor
		if err := json.Unmarshal(encoded, &anchor); err != nil {
			t.Fatalf("decode the ask's anchor: %v", err)
		}
		return anchor
	}
	if anchor := anchorOf(tx); anchor.Orphaned {
		t.Fatalf("anchor = %#v after the write's version, want it anchored: that version still holds the quote", anchor)
	}
	// The version's anchor refresh is a write, which the transaction's write phase runs at Commit,
	// and the Commit that runs it ends the transaction, so every read after it goes to the pool.
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the joined edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if anchor := anchorOf(service.store.Pool); !anchor.Orphaned {
		t.Fatalf("anchor = %#v after the settlement's version, want it orphaned: the browser deleted the paragraph it quotes", anchor)
	}
}

// A room change after a transaction's write read the document costs its version no render: the
// version is the write's own state (captureWrite), which the write's latest operation rendered
// already, and the room's change is versioned by the settlement that follows. The contended case
// is a browser and an agent writing together, where every spurious invalidation is a full document
// render back under the issue row.
func TestARoomChangeAfterAWritesReadCostsItsVersionNoRender(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	testcleanup.Add(t, func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	testcleanup.Add(t, ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("first joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "gained in the room"))
	// The next operation runs on the transaction's fork, which takes in nothing the room gained.
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "after", With: "AFTER",
	}}, alice, nil); err != nil {
		t.Fatalf("second joined edit: %v", err)
	}
	write := ledger.liveWriteFor(artifactID)
	if write == nil || write.tree == nil {
		t.Fatal("the second edit recorded no rendering")
	}

	captured, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	tree, markdown := captured.tree, captured.markdown
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if tree != write.tree {
		t.Fatal("the snapshot rendered the document again though the write's own state had not moved")
	}
	if strings.Contains(markdown, "gained in the room") || !strings.Contains(markdown, "AFTER") {
		t.Fatalf("snapshot markdown = %q, want the transaction's content alone", markdown)
	}
}

// A delete moves the room without advancing its Yjs state vector. A warm fork must
// include any delete committed at its read point, even if the observer has not run.
// Later deletes must not reach the fork, or make capture reuse a stale rendering.
func TestACaptureNeverReusesARenderingTheForkHasMovedPast(t *testing.T) {
	t.Parallel()
	const doomed = "Doomed paragraph."
	type deleteAt int
	const (
		beforeEdit deleteAt = iota
		observerHeldAcrossForkRead
		afterForkRead
		afterCaptureFork
	)
	for _, test := range []struct {
		name       string
		at         deleteAt
		keepDoomed bool
	}{
		{name: "before the edit", at: beforeEdit},
		{name: "with its observer held across the fork read", at: observerHeldAcrossForkRead},
		{name: "after the fork read", at: afterForkRead, keepDoomed: true},
		{name: "after the capture takes the fork", at: afterCaptureFork, keepDoomed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			holdRoomsResident(t, service)
			seedServiceText(t, service, artifactID, "Before.\n\nKept paragraph.\n\n"+doomed)
			checks := checkRenderings(service)

			ctx := testwait.Context(t)
			if err := service.warmLiveDocument(ctx, artifactID); err != nil {
				t.Fatalf("warm the room before the fork read: %v", err)
			}
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin transactional edit: %v", err)
			}
			testcleanup.Add(t, func() { _ = tx.Rollback(context.Background()) })
			joinedCtx, ledger := service.Join(ctx, tx)
			testcleanup.Add(t, ledger.Discard)

			var browser sync.WaitGroup
			var deleteErr error
			deleteStarted := false
			startDelete := func() {
				deleteStarted = true
				browser.Go(func() {
					deleteErr = service.srv.Apply(ctx, artifactID,
						func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
							fragment := doc.GetXmlFragment(fragmentName)
							transact(func(txn *crdt.Transaction) { fragment.Delete(txn, 2, 1) })
						})
				})
			}
			waitForDelete := func() {
				testwait.Returns(t, "the browser's delete to return", browser.Wait)
				if deleteErr != nil {
					t.Fatalf("delete the last paragraph from the room: %v", deleteErr)
				}
			}
			deleteAndWait := func() {
				startDelete()
				waitForDelete()
			}
			switch test.at {
			case beforeEdit:
				deleteAndWait()
			case observerHeldAcrossForkRead:
				held, proceed := make(chan struct{}), make(chan struct{})
				release := sync.OnceFunc(func() { close(proceed) })
				testcleanup.Add(t, func() {
					release()
					testwait.Returns(t, "the released browser delete to return", browser.Wait)
				})
				service.beforeObserveUpdate = onFirst(artifactID, func() {
					close(held)
					<-proceed // The fork-read hook or the cleanup releases the observer.
				})
				service.afterForkRead = onFirst(artifactID, release)
				startDelete()
				testwait.Receive(t, "the committed delete's observer to reach the hold", held)
			case afterForkRead:
				service.afterForkRead = onFirst(artifactID, deleteAndWait)
			case afterCaptureFork:
				service.afterCaptureFork = onFirst(artifactID, deleteAndWait)
			}

			alice := model.Actor{Kind: "user", ID: "alice"}
			if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
				Op: "replace", Find: "Before.", With: "After.",
			}}, alice, nil); err != nil {
				t.Fatalf("joined edit: %v", err)
			}
			if test.at == observerHeldAcrossForkRead {
				waitForDelete()
			}
			write := ledger.liveWriteFor(artifactID)
			if write == nil || write.tree == nil {
				t.Fatal("the edit recorded no rendering")
			}
			if write.cold {
				t.Fatal("the edit forked the store instead of the warmed room")
			}

			base := readForkReads(service)
			captured, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
			if err != nil {
				t.Fatalf("capture live text: %v", err)
			}
			if !deleteStarted {
				t.Fatal("the browser's delete never started")
			}
			requireForkReads(t, "capture", readForkReads(service).minus(base), forkReads{renderingReused: 1})
			calls, failures := checks.take()
			if calls["capture"] != 1 {
				t.Errorf("capture checked %d reused renderings, want 1", calls["capture"])
			}
			for _, failure := range failures {
				t.Errorf("a write's rendering is not its fork's at %s", failure)
			}

			fresh, err := treeOf(write.fork)
			if err != nil {
				t.Fatalf("read the fork: %v", err)
			}
			freshMarkdown, err := renderTree(fresh)
			if err != nil {
				t.Fatalf("render the fork: %v", err)
			}
			if captured.markdown != freshMarkdown {
				t.Errorf("capture reused stale markdown:\n got %q\nwant %q", captured.markdown, freshMarkdown)
			}
			if !strings.Contains(captured.markdown, "After.") || strings.Contains(captured.markdown, doomed) != test.keepDoomed {
				t.Errorf("capture markdown = %q, want the agent edit and doomed paragraph present = %t", captured.markdown, test.keepDoomed)
			}
		})
	}
}
