package docs

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// The steps of TestTheTreeRendersAPostCutoverVersionFromItsTuples that hold R15's deferred version
// and the reconciliation's reading of an update its room has not stored. Their issues are of the
// project MISS (seedMismatchIssues) and of an issue room of their own, DOC-4, so no other step's
// documents share them. Each compares two readings of the census of the rendered versions, and
// reads no total, since every step runs on the one database, whose earlier steps leave rows of
// their own in each class.

// itemList is a bullet list of items items: one block for the list, and for each item the list
// item and its paragraph, 1 + 2*items blocks (deferredBlocks), so deferredItems of them are more
// blocks than a write indexes in its own request.
func itemList(items int) string { return strings.Repeat("- x\n", items) }

// uploadVersion writes the next version of the document spec as the upload route writes one (api's
// storeArtifact): in one transaction joined to the service, ReplaceText with the token of the
// version it replaces, the version queued through the ledger's QueueVersion,
// IndexDocumentReferences, the version's node and CarryUploadedApproval. It answers the version's
// number and the text the version stores.
func uploadVersion(t *testing.T, database *store.Store, service *Service, spec, markdown string) (int, string) {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload over %s: %v", spec, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	precondition, err := replacementPrecondition(joined, service, spec)
	if err != nil {
		t.Fatalf("read the token of %s: %v", spec, err)
	}
	stored, err := service.ReplaceText(joined, spec, markdown, alice, precondition)
	if err != nil {
		t.Fatalf("replace the text of %s: %v", spec, err)
	}
	version, err := ledger.QueueVersion(ctx, spec, stored, []model.Actor{alice}, nil)
	if err != nil {
		t.Fatalf("queue the upload's version of %s: %v", spec, err)
	}
	ledger.WroteVersion(spec, version, stored)
	_, stated, err := service.IndexDocumentReferences(joined, spec, version.Number, stored)
	if err != nil {
		t.Fatalf("index the upload's version of %s: %v", spec, err)
	}
	owner, err := ReadVersionOwner(joined, tx, spec)
	if err != nil {
		t.Fatalf("read the owner of the upload's version of %s: %v", spec, err)
	}
	node, err := WriteVersionNode(joined, tx, owner, version.Number, alice, version.CreatedAt)
	if err != nil {
		t.Fatalf("write the node of the upload's version of %s: %v", spec, err)
	}
	if err := CarryUploadedApproval(joined, tx, service.events, spec, version.Number, stored, nil, RecordCarry{}, service.serverURL); err != nil {
		t.Fatalf("carry the approval of %s to the upload's version: %v", spec, err)
	}
	if err := causeInWritePhase(joined, tx, graphstore.Joined(stated, node), graphstore.NoEvent); err != nil {
		t.Fatalf("append what the upload of %s stated: %v", spec, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload of %s: %v", spec, err)
	}
	return version.Number, stored
}

// requireRenderedAs fails, printing as the check words it, unless version number of the document
// spec, whose ref_key is refKey, renders from source as exactly the bytes it stores.
func (r *renderRig) requireRenderedAs(t *testing.T, spec, refKey string, number int, source RenderSource) {
	t.Helper()
	stored := storedVersionMarkdown(t, r.database, spec, number)
	if got := r.renderVersion(t, spec, number); got.Source != source || got.Markdown != stored {
		t.Errorf("version %d of %s renders %d bytes from %s, want its %d stored bytes from %s", number, refKey, len(got.Markdown), got.Source, len(stored), source)
	}
}

// approveThroughTheRoute records login's approval of version number of the document id as the
// approval route records one (api's recordArtifactReview and queueApprovalReviews): in a
// transaction joined to the service, under the document owner's lock, a person's review queued on
// the ledger's write phase (QueueReview), which appends its artifact.approved and writes its
// event_seq, and whose insert pins the digest artifact_version_text_sha256 answers. It answers the
// review as the write phase left it.
func approveThroughTheRoute(t *testing.T, service *Service, id string, number int, login string) model.ArtifactReview {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the approval of %s: %v", id, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, _, err := lockArtifactOwner(joined, tx, id); err != nil {
		t.Fatalf("lock the owner of %s: %v", id, err)
	}
	artifact, err := reviewedArtifact(joined, tx, id)
	if err != nil {
		t.Fatalf("read %s for its review: %v", id, err)
	}
	var review model.ArtifactReview
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		return QueueReview(b, service.events, artifact, number, "approved", ReviewKindPerson, model.Actor{Kind: "user", ID: login}, nil, nil,
			func(written model.ArtifactReview, _ model.Event) { review = written })
	}); err != nil {
		t.Fatalf("queue the approval of version %d of %s: %v", number, id, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the approval of version %d of %s: %v", number, id, err)
	}
	return review
}

// renderStepDeferred: a version the reconciliation has not stated yet renders its stored bytes and
// is deferred, and its pass gives it its time and its record's digest and carries the approval
// taken before the pass (R14, R15). The text is 2,001 list items, written into version 1 of the
// spec of MISS-4 as the issue create writes a spec's version 1 (writeFirstVersion): the head has no
// hook that lowers deferBlockIndexBlocks. The version's row holds no time and no digest, the
// version renders from the source stored, the census counts it deferred and reads no difference at
// it, and alice's approval, made as the approval route makes one (approveThroughTheRoute), pins the
// sha256 of its stored markdown and is the document's newest review. After drainOwed the row holds a
// time no earlier than every text value of the version, a seq and the record's digest, the version
// renders from the tree as its stored bytes, and the newest review is the carry of alice's approval
// to version 1, reading restoredReason and pinning the record's digest. Last, an upload writes
// version 2 of 4,002 items, which defers by its size, and one writes version 3 of one paragraph,
// whose own write indexes the document: the reconciliation leaves version 2's row with no time
// for good, renders its stored bytes and counts it deferred.
func renderStepDeferred(t *testing.T, rig *renderRig, service *Service) {
	ctx := context.Background()
	database := rig.database
	seedMismatchIssues(t, database)
	spec, refKey := createSpecOf(t, database, "MISS-4")
	baseline := rig.readRenderCensus(t)
	writeFirstVersion(t, database, service, spec, refKey, itemList(deferredItems), func(seeded string) string { return seeded })
	if err := service.settleOwedNow(ctx, spec, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s: %v", refKey, err)
	}
	if at := blockIndexAt(t, database, spec); at != nil {
		t.Fatalf("the write of %s indexed its version of %d items in its own transaction, at version %d, want it left to the reconciliation", refKey, deferredItems, *at)
	}
	address := "version " + refKey + "@v1"
	if row := readRenderRow(t, database, spec, 1); row.at != nil || row.digest != nil || row.readSeq != nil || row.mismatch {
		t.Fatalf("the row of %s@v1 holds %v, %v and %v, tree_mismatch %t before any pass, want no time, no digest and no seq", refKey, row.at, row.digest, row.readSeq, row.mismatch)
	}
	rig.requireRenderedAs(t, spec, refKey, 1, StoredBytes)
	deferred := rig.readRenderCensus(t)
	if deferred.Deferred != baseline.Deferred+1 || deferred.TreeMismatch != baseline.TreeMismatch {
		t.Errorf("the census counts deferred=%d tree_mismatch=%d after the write of %s@v1, want %d and %d: the version is deferred and no mismatch",
			deferred.Deferred, deferred.TreeMismatch, refKey, baseline.Deferred+1, baseline.TreeMismatch)
	}
	if cause := differenceAt(deferred, address); cause != "" {
		t.Errorf("the census reads a difference at %s: %s, want none for a version no render describes", address, cause)
	}
	stored := storedVersionMarkdown(t, database, spec, 1)
	approval := approveThroughTheRoute(t, service, spec, 1, "alice")
	if approval.ContentSHA256 == nil || *approval.ContentSHA256 != reviewTextHash(stored) || approval.EventSeq == nil {
		t.Fatalf("alice's approval of %s@v1 through the approval route = %+v; want it pinning the stored markdown's %s, with the seq of its artifact.approved",
			refKey, approval, reviewTextHash(stored))
	}
	if newest, err := NewestReview(ctx, database.Pool, spec); err != nil || newest == nil || newest.ID != approval.ID {
		t.Fatalf("the newest review of %s after alice's approval = %+v (%v), want her approval %s", refKey, newest, err, approval.ID)
	}

	drainOwed(t, database, service)
	row := readRenderRow(t, database, spec, 1)
	if row.at == nil || row.digest == nil || row.readSeq == nil || row.mismatch {
		t.Fatalf("version 1 of %s still holds no time after the reconciliation: its row holds %v, %v and %v, tree_mismatch %t", refKey, row.at, row.digest, row.readSeq, row.mismatch)
	}
	requireStatedRow(t, database, "MISS-4", graphstore.QuerySpec, spec, 1)
	var latest time.Time
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(max(t.at), 'epoch'::timestamptz) from graph_tuples t
		where t.from_kind = $1 and t.from_id like $2 and t.kind = any($3)`,
		refs.BlockNodeKind, refKey+"#%", []string{graphstore.AttributeText, refs.TupleTextParent, refs.AttributeTextRank}).Scan(&latest); err != nil {
		t.Fatalf("read the newest text value of %s: %v", refKey, err)
	}
	if row.at.Before(latest) {
		t.Errorf("the row of %s@v1 holds %s, before the text value its pass wrote at %s", refKey, row.at, latest)
	}
	rig.requireRenderedAs(t, spec, refKey, 1, RenderedFromTree)
	carried, err := NewestReview(ctx, database.Pool, spec)
	if err != nil || carried == nil || carried.ID == approval.ID || carried.State != "approved" || carried.Version != 1 || carried.Reason == nil ||
		*carried.Reason != restoredReason(1, 1) || carried.ContentSHA256 == nil || *carried.ContentSHA256 != *row.digest {
		t.Errorf("the approval of %s at version 1, taken before its pass, reads stale after a pass whose render equals its stored bytes: the newest review is %+v (%v), want a carry to version 1 reading %q and pinning the record's %s",
			refKey, carried, err, restoredReason(1, 1), *row.digest)
	}
	stated := rig.readRenderCensus(t)
	if stated.Deferred != baseline.Deferred || stated.Differences != baseline.Differences {
		t.Errorf("the census counts deferred=%d differences=%d after the pass of %s@v1, want %d and %d: the version is rendered, with no difference",
			stated.Deferred, stated.Differences, refKey, baseline.Deferred, baseline.Differences)
	}

	if number, _ := uploadVersion(t, database, service, spec, itemList(2*deferredItems)); number != 2 {
		t.Fatalf("the upload over %s wrote version %d, want 2", refKey, number)
	}
	if number, _ := uploadVersion(t, database, service, spec, "One paragraph.\n"); number != 3 {
		t.Fatalf("the upload over %s wrote version %d, want 3", refKey, number)
	}
	drainOwed(t, database, service)
	if row := readRenderRow(t, database, spec, 2); row.at != nil || row.digest != nil || row.mismatch {
		t.Errorf("version 2 of %s holds %v and %v, tree_mismatch %t after the reconciliation, want no time and no mark: a later version replaced it before its pass", refKey, row.at, row.digest, row.mismatch)
	}
	rig.requireRenderedAs(t, spec, refKey, 2, StoredBytes)
	if census := rig.readRenderCensus(t); !slices.Contains(census.DeferredAt, "version "+refKey+"@v2") {
		t.Errorf("the census counts version 2 of %s as no deferred version, want deferred: %v", refKey, census.DeferredAt)
	}
}

// raceWithAnUnclassifiedUpdate holds the one moment R15's reconciliation must not read as a
// stored version: a browser's update the room has committed and not classified yet. It connects a
// browser to document's room, whose key is room, and finds the room's replica; in a transaction of
// its own it takes the room's advisory lock (lockDocumentRoom), which every durable append of the
// room takes first, and it takes the replica's lock (renderedReplica.mu), and holds both. The
// browser then types a paragraph into fragment. The room commits the update and appends it to its
// update log, and the room's update observer waits for the replica's lock, which the test reads as
// the replica's waiting count at 1, with the log holding one update it has not classified and the
// room counting no pending update and no durable append. It runs one tick of the reconciliation
// on a goroutine of its own: the tick takes the owner lock and no room lock, reads a copy of the
// room, which holds the paragraph, and its drain of the update log waits for the replica's lock
// behind the observer. The test waits until the waiting count reads 2 or the tick ends, and then
// releases the replica's lock: the drain classifies the update, the room counts its durable
// append, which waits behind the room's lock, and the tick states nothing of what it read. It
// returns once the tick has ended, and the function that releases the room's lock.
func raceWithAnUnclassifiedUpdate(t *testing.T, service *Service, serverURL, document, room, fragment string) (releaseRoom func()) {
	t.Helper()
	ctx := context.Background()
	browser := connectBrowser(t, serverURL, document, "alice")
	_, listed := listedReplica(t, service, room)
	replica := listed.Value()
	if replica == nil {
		t.Fatalf("the replica of the room %s went while a browser holds it", room)
	}
	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the lock of the room %s: %v", room, err)
	}
	heldRoom := true
	releaseRoom = func() {
		if heldRoom {
			heldRoom = false
			holder.Rollback(ctx)
		}
	}
	testcleanup.Add(t, releaseRoom)
	if err := lockDocumentRoom(ctx, holder, room); err != nil {
		t.Fatalf("take the advisory lock of the room %s: %v", room, err)
	}
	replica.mu.Lock()
	heldReplica := true
	releaseReplica := func() {
		if heldReplica {
			heldReplica = false
			replica.mu.Unlock()
		}
	}
	testcleanup.Add(t, releaseReplica)

	if _, err := browser.Send(writeParagraph(browser.Doc, fragment, "The browser's words.")); err != nil {
		t.Fatalf("type the browser's paragraph into the room %s: %v", room, err)
	}
	waitFor(t, roomWait(t), "the room's update observer to wait for the replica's lock", func() bool { return replica.waiting.Load() == 1 })
	if pending := replica.log.Pending(); pending != 1 {
		t.Fatalf("the replica of the room %s holds %d committed updates it has not classified, want the browser's one", room, pending)
	}
	if service.hasPendingUpdates(room) || service.hasDurableAppend(room) {
		t.Fatalf("the room %s counts a pending update or a durable append before its observer classified the browser's update, want neither", room)
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		done <- err
	}()
	var tickErr error
	ended := false
	waitFor(t, roomWait(t), "the tick's drain to wait for the replica's lock, or the tick to end", func() bool {
		if replica.waiting.Load() >= 2 {
			return true
		}
		select {
		case tickErr = <-done:
			ended = true
			return true
		default:
			return false
		}
	})
	releaseReplica()
	if !ended {
		select {
		case tickErr = <-done:
		case <-time.After(roomWait(t)):
			t.Fatalf("the tick did not end within roomWait of the release of the replica's lock")
		}
	}
	if tickErr != nil {
		t.Fatalf("the tick of the block-index reconciliation: %v", tickErr)
	}
	return releaseRoom
}

// requireTheRaceLeftTheCensusAlone fails unless the census of the rendered versions reads, after
// a race (raceWithAnUnclassifiedUpdate) and its settlement, the tree_mismatch and
// tree_mismatch_repaired counts of the reading before the browser connected, prints no
// tree_mismatch line at a version of the document refKey, and counts its version deferred.
func (r *renderRig) requireTheRaceLeftTheCensusAlone(t *testing.T, before graphcensus.VersionRenderCensus, refKey string, deferred int) {
	t.Helper()
	after := r.readRenderCensus(t)
	if after.TreeMismatch != before.TreeMismatch {
		t.Errorf("the census reads tree_mismatch=%d after the race, want the %d it read before", after.TreeMismatch, before.TreeMismatch)
	}
	if after.TreeMismatchRepaired != before.TreeMismatchRepaired {
		t.Errorf("the census reads tree_mismatch_repaired=%d after the race, want the %d it read before", after.TreeMismatchRepaired, before.TreeMismatchRepaired)
	}
	for _, marked := range after.TreeMismatchAt {
		if strings.HasPrefix(marked, "version "+refKey+"@v") {
			t.Errorf("the census marks %s tree_mismatch after the race", strings.TrimPrefix(marked, "version "))
		}
	}
	if address := "version " + refKey + "@v" + strconv.Itoa(deferred); !slices.Contains(after.DeferredAt, address) {
		t.Errorf("the census counts version %d of %s as no deferred version, want deferred: %v", deferred, refKey, after.DeferredAt)
	}
}

// renderStepRaceOfADocument: the reconciliation reads an update of a room that is not stored yet as
// live ahead, and marks no row, also where no observer has classified the update yet (R15). The
// spec of MISS-5 has version 1 through the real version write and version 2 by an unchanged
// upload, which leaves version 2's row with no time and the document owing its index. A browser
// types a paragraph while the room's advisory lock and the replica's lock are held
// (raceWithAnUnclassifiedUpdate), and a tick runs: version 2's row still holds no time and no
// tree_mismatch. The room's lock is released, the document settles, writing version 3 with the
// browser's paragraph and indexing it in its own transaction, and the census reads the counts it
// read before the browser connected, no mark at a version of the spec, and version 2 deferred.
func renderStepRaceOfADocument(t *testing.T, rig *renderRig, service *Service, serverURL string) {
	ctx := context.Background()
	database := rig.database
	seedMismatchIssues(t, database)
	spec, refKey := createSpecOf(t, database, "MISS-5")
	seedServiceText(t, service, spec, "The words of the race.\n")
	writeVersionThroughTheRealPath(t, service, spec, "The first version")
	settle(t, database, spec)
	if number, _ := uploadVersion(t, database, service, spec, storedVersionMarkdown(t, database, spec, 1)); number != 2 {
		t.Fatalf("the unchanged upload over %s wrote version %d, want 2", refKey, number)
	}
	if row := readRenderRow(t, database, spec, 2); row.at != nil {
		t.Fatalf("version 2 of %s holds a time before its pass", refKey)
	}
	before := rig.readRenderCensus(t)

	releaseRoom := raceWithAnUnclassifiedUpdate(t, service, serverURL, spec, spec, fragmentName)
	row := readRenderRow(t, database, spec, 2)
	if row.mismatch {
		t.Errorf("the tick marked version 2 of %s tree_mismatch while its room held an update not yet stored", refKey)
	} else if row.at != nil {
		t.Errorf("the tick stated version 2 of %s while its room held an update not yet stored", refKey)
	}
	releaseRoom()
	if err := service.settleOwedNow(ctx, spec, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s: %v", refKey, err)
	}
	if number := savedVersion(t, database, spec); number != 3 {
		t.Errorf("the settlement of %s wrote version %d, want version 3 with the browser's paragraph", refKey, number)
	}
	drainOwed(t, database, service)
	rig.requireTheRaceLeftTheCensusAlone(t, before, refKey, 2)
}

// renderStepRaceOfARoom: the same of the reconciliation's pass over an issue room
// (indexOwedRoom, R15). The issue room DOC-4 is written as createIssueRoom writes one: each
// document's version 1 is empty and the plan's fragment holds planSeed. Version 2 of its spec, 2,001
// list items, is an upload that changes the spec's fragment, so it leaves the room's pending row
// and owes the room its index, since the plan's fragment reads otherwise than the plan's version 1;
// the room's settlement writes version 2 of the plan and no version of the spec, states the room
// and deletes the pending row, and drainOwed leaves every fragment reading as its document's
// newest version. Version 3 of the spec is an upload of version 2's stored markdown: it changes
// nothing, so it writes no tree, appends no update and writes no pending row, and its room, of more
// than 2,000 blocks over every fragment, is left to the reconciliation, with version 3's row
// holding no time. A browser then types a paragraph into the spec's fragment while the room's
// lock and the replica's are held (raceWithAnUnclassifiedUpdate), and a tick runs: version 3's row
// still holds no time and no tree_mismatch. After the room's settlement, which writes version 4 of
// the spec with the browser's paragraph, the census reads the counts it read before the browser
// connected, no mark at a version of the room's spec, and version 3 deferred.
func renderStepRaceOfARoom(t *testing.T, rig *renderRig, service *Service, serverURL string) {
	ctx := context.Background()
	database := rig.database
	room := createIssueRoom(t, database, 4)
	writeRoomQueryNodes(t, database, room)
	refKey := refKeyOf(t, database, room.spec)
	if number, _ := uploadVersion(t, database, service, room.spec, itemList(deferredItems)); number != 2 {
		t.Fatalf("the upload over %s wrote version %d, want 2", refKey, number)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the room of %s: %v", room.issue, err)
	}
	drainOwed(t, database, service)
	if number, _ := uploadVersion(t, database, service, room.spec, storedVersionMarkdown(t, database, room.spec, 2)); number != 3 {
		t.Fatalf("the unchanged upload over %s wrote version %d, want 3", refKey, number)
	}
	if pending, err := settlementPending(ctx, database.Pool, room.tree); err != nil || pending {
		t.Fatalf("the room of the race case holds a pending row before the race (%v)", err)
	}
	if owedCount(t, database) == 0 {
		t.Fatalf("the room of the race case owes no index before the race")
	}
	if row := readRenderRow(t, database, room.spec, 3); row.at != nil {
		t.Fatalf("version 3 of %s holds a time before its pass", refKey)
	}
	before := rig.readRenderCensus(t)

	releaseRoom := raceWithAnUnclassifiedUpdate(t, service, serverURL, room.spec, room.tree, room.spec)
	row := readRenderRow(t, database, room.spec, 3)
	if row.mismatch {
		t.Errorf("the tick marked version 3 of %s tree_mismatch while its room held an update not yet stored", refKey)
	} else if row.at != nil {
		t.Errorf("the tick stated version 3 of %s while its room held an update not yet stored", refKey)
	}
	releaseRoom()
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the room of %s: %v", room.issue, err)
	}
	if number := savedVersion(t, database, room.spec); number != 4 {
		t.Errorf("the settlement of the room of %s wrote version %d of its spec, want version 4 with the browser's paragraph", room.issue, number)
	}
	drainOwed(t, database, service)
	rig.requireTheRaceLeftTheCensusAlone(t, before, refKey, 3)
}
