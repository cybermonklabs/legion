package docs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A settlement of a closed issue's document saves the document and nothing beside it: it wakes
// nobody (commitVersion, planRepairs), it opens no ask for an ask block with no row, whatever the
// room's record of the issue reads (reconcileAskBlocks), and every write a person or a session
// makes on the closed issue is still refused after it (closed_settlement_test.go has the
// settlements themselves).

// appendedEvent is one event as the event log stores it: its type, its actor and whether it
// notifies anyone (events.Broker.Notify).
type appendedEvent struct {
	kind   string
	actor  model.Actor
	notify bool
}

// latestEventID is the id of the newest event the log holds, 0 for none.
func latestEventID(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&id); err != nil {
		t.Fatalf("read the newest event's id: %v", err)
	}
	return id
}

// eventsAfter is every event the log appended after the event whose id is after, oldest first.
func eventsAfter(t *testing.T, database *store.Store, after int64) []appendedEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type, actor, notify from events where id > $1 order by id
	`, after)
	if err != nil {
		t.Fatalf("read the events appended after %d: %v", after, err)
	}
	defer rows.Close()
	var appended []appendedEvent
	for rows.Next() {
		var event appendedEvent
		var actor []byte
		if err := rows.Scan(&event.kind, &actor, &event.notify); err != nil {
			t.Fatalf("read an appended event: %v", err)
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			t.Fatalf("decode an appended event's actor: %v", err)
		}
		appended = append(appended, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events appended after %d: %v", after, err)
	}
	return appended
}

// editNoticeRows counts the document's edit-notice rows, held ones (holdEditNotice) included when
// held is false, and held ones alone when it is true.
func editNoticeRows(t *testing.T, database *store.Store, artifactID string, held bool) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from document_edit_notices where artifact_id = $1 and (not $2 or last_editor is null)
	`, artifactID, held).Scan(&count); err != nil {
		t.Fatalf("count the document's edit notices: %v", err)
	}
	return count
}

// A settlement of a closed issue's document wakes nobody (commitVersion). Two people are
// connected when one of them edits, so the version over that edit names nobody and leaves the
// document a held notice (holdEditNotice); then alice edits alone, and her edit is still pending
// when DOC-1 closes. The settlement the resumption runs on the closed issue versions alice's edit
// and lists her, but records no edit notice and deletes the held one, and every event it appends
// notifies nobody, none of them an ask's or an approval's. DOC-1 then reopens inside what would
// have been the notice's quiet time, and once that time has passed the sweep sends no notice; bob's
// edit after the reopen opens a notice that starts at the settled version, so it never carries
// alice's edit, which the settlement saved while DOC-1 was closed.
func TestASettlementAfterACloseWakesNobody(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n\nThird.\n")
	settleCurrentGeneration(t, service, artifactID)

	adam := model.Actor{Kind: "user", ID: "adam@example.com"}
	zoe := model.Actor{Kind: "user", ID: "zoe@example.com"}
	service.addConnection(artifactID, 1, adam)
	service.addConnection(artifactID, 2, zoe)
	editAsPeer(t, service, artifactID, replaceRun("First.", "First, typed by one of two people."))
	settleCurrentGeneration(t, service, artifactID)
	service.removeConnection(artifactID, 1)
	service.removeConnection(artifactID, 2)
	if held := editNoticeRows(t, service.store, artifactID, true); held != 1 {
		t.Fatalf("the version over the two people's edit left %d held edit notices, want one", held)
	}

	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	const alicesEdit = "Second, typed by alice before the close."
	editAsConnectedPeer(t, service, artifactID, 3, alice, replaceRun("Second.", alicesEdit))
	waitForLandedAppends(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	closeTestIssue(t, service)
	if owed, err := settlementPending(ctx, service.store.Pool, artifactID); err != nil || !owed {
		t.Fatalf("the document owes a settlement at the close = %t (%v), want alice's edit pending", owed, err)
	}
	lastEvent := latestEventID(t, service.store)

	service.settle = 20 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlement: %v", err)
	}
	waitFor(t, roomWait(t), "the closed issue's document to settle", func() bool {
		owed, err := settlementPending(ctx, service.store.Pool, artifactID)
		return err == nil && !owed
	})
	settled, markdown, authors := latestDocumentVersion(t, service.store, artifactID)
	if settled <= before || !strings.Contains(markdown, alicesEdit) {
		t.Fatalf("the closed issue's settlement left version %d reading %q, want a version past %d holding alice's edit", settled, markdown, before)
	}
	if !slices.ContainsFunc(authors, alice.SameAs) {
		t.Fatalf("the settled version's authors are %+v, want alice, whose edit it saves", authors)
	}
	if rows := editNoticeRows(t, service.store, artifactID, false); rows != 0 {
		t.Fatalf("the closed issue's settlement left the document %d edit notices, want none: it records none and deletes the one it found", rows)
	}
	appended := eventsAfter(t, service.store, lastEvent)
	if len(appended) == 0 {
		t.Fatal("the closed issue's settlement appended no event, want at least its version's")
	}
	for _, event := range appended {
		if event.notify || strings.HasPrefix(event.kind, "ask.") || event.kind == "artifact.approved" {
			t.Fatalf("the closed issue's settlement appended %s by %+v (notify %t), want only events that notify nobody and none of an ask or an approval",
				event.kind, event.actor, event.notify)
		}
	}

	reopenTestIssue(t, service)
	// The quiet time has passed: a notice the settlement had left due would be due now.
	if _, err := service.store.Pool.Exec(ctx, `
		update document_edit_notices set due_at = now() - interval '1 second' where artifact_id = $1 and due_at is not null
	`, artifactID); err != nil {
		t.Fatalf("let the quiet time pass: %v", err)
	}
	sent, err := service.sendDueEditNotices(ctx)
	if err != nil {
		t.Fatalf("sweep the due edit notices: %v", err)
	}
	if edits := humanEdits(t, service.store, artifactID); sent != 0 || len(edits) != 0 {
		t.Fatalf("the sweep after the reopen sent %d notices (%d in the log), want none for the edit the closed issue's settlement saved", sent, len(edits))
	}

	bob := model.Actor{Kind: "user", ID: "bob@example.com"}
	editAsConnectedPeer(t, service, artifactID, 4, bob, replaceRun("Third.", "Third, typed by bob after the reopen."))
	settleCurrentGeneration(t, service, artifactID)
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("bob's settled edit after the reopen opened no edit notice")
	}
	if row.from < settled {
		t.Fatalf("bob's notice starts at version %d, want at or after %d, the version the closed issue's settlement wrote: it carries alice's edit", row.from, settled)
	}
}

// documentWritesRead is what a document holds that a write stores: its versions, and its stored
// updates, each counted and the updates' bytes digested in order.
type documentWritesRead struct {
	versions int
	updates  int
	digest   string
}

// readDocumentWrites reads the document's documentWritesRead.
func readDocumentWrites(t *testing.T, database *store.Store, artifactID string) documentWritesRead {
	t.Helper()
	var read documentWritesRead
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from artifact_versions where artifact_id = $1),
			(select count(*) from doc_updates where artifact_id = $1),
			(select coalesce(md5(string_agg(md5(update), ',' order by version)), '') from doc_updates where artifact_id = $1)
	`, artifactID).Scan(&read.versions, &read.updates, &read.digest); err != nil {
		t.Fatalf("read what the document stores: %v", err)
	}
	return read
}

// requireRefusedStoringNothing fails unless err, what a write answered, is the closed issue's
// refusal (ErrIssueClosed), and the document stores what it stored before: no version and no
// update of the write's.
func (f closedSettlementFixture) requireRefusedStoringNothing(t *testing.T, what string, err error, before documentWritesRead) {
	t.Helper()
	if !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("%s on the closed issue = %v, want ErrIssueClosed", what, err)
	}
	f.requireNothingStored(t, what, before)
}

// requireNothingStored fails unless the document stores what it stored before: no version and no
// update since.
func (f closedSettlementFixture) requireNothingStored(t *testing.T, what string, before documentWritesRead) {
	t.Helper()
	if after := readDocumentWrites(t, f.database, f.artifactID); after != before {
		t.Fatalf("%s on the closed issue left the document %d versions and %d stored updates (digest %s), want %d and %d as before (digest %s)",
			what, after.versions, after.updates, after.digest, before.versions, before.updates, before.digest)
	}
}

// A closed issue's document still refuses every write that adds a person's or a session's text
// once a settlement has run on the closed issue (R4). alice's browser is admitted writable while
// CORE-1 is open, and CORE-1 then closes two minutes back in the database alone, as on a process
// that has not heard the close, so the room's record still reads the issue open. The resumption
// settles the document on the closed issue. After it, each of these answers ErrIssueClosed and
// stores no version and no update: the edit of the document mutation route (ApplyOps), a named
// version, a snapshot version without the owner-verified mark, and an injection through the
// document socket without it (allowInject). alice's keystroke, which her room takes past the close
// margin, is refused at the store (gateAppend), and the refusal closes her room. Once no room holds
// the document, a rebuild carrying markdown of a history that does not load is refused too.
func TestAClosedIssuesDocumentStillRefusesAPersonsWrite(t *testing.T) {
	f := newClosedSettlementFixture(t)
	ctx := context.Background()
	alice := f.admitBeforeTheClose(t)
	before := latestVersionNumber(t, f.service, f.artifactID)
	f.resume(t)
	f.requireSettledFromTheStore(t, before)
	f.waitForNoSettlement(t)
	person := model.Actor{Kind: "user", ID: "alice"}
	stored := readDocumentWrites(t, f.database, f.artifactID)

	_, err := joinedApplyOps(f.service, f.artifactID, []model.EditOp{
		{Op: "replace", Find: closedSettlementParagraph, With: "Edited through the mutation route."},
	}, person, nil)
	f.requireRefusedStoringNothing(t, "the document mutation route's edit (ApplyOps)", err, stored)
	_, err = namedVersion(t, f.service, f.artifactID, "Named after the close", person)
	f.requireRefusedStoringNothing(t, "a named version", err, stored)
	err = joinedWrite(f.service, func(ctx context.Context) error {
		_, err := f.service.SnapshotVersion(ctx, f.artifactID, person)
		return err
	})
	f.requireRefusedStoringNothing(t, "a snapshot version without the owner-verified mark", err, stored)
	err = f.service.srv.Apply(ctx, f.artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement("paragraph"))
		})
	})
	f.requireRefusedStoringNothing(t, "an injection through the document socket without the owner-verified mark", err, stored)

	typeOver(t, alice, closedSettlementParagraph, refusedKeystroke)
	requireEnds(t, alice, "the refusal of alice's keystroke past the close margin closed her room")
	waitForRefusedAppendClose(t, f.service)
	f.requireNothingStored(t, "alice's keystroke past the close margin", stored)
	if markdown := storedMarkdown(t, f.database, f.artifactID); markdown == "" || strings.Contains(markdown, refusedKeystroke) {
		t.Fatalf("the store holds %q, want the settled text without the keystroke the closed issue refused", markdown)
	}

	// A rebuild is for a history that does not load, so the document's is made one, with no room
	// holding it: the refusal of a closed issue's rebuild comes after both checks (RebuildDocument),
	// and the first refuses a document any room of this service holds (ErrDocumentLive), so no room
	// at all may hold it here, not only alice's. The settlement her keystroke armed runs on the
	// closed issue as on an open one, and its warm-up can load the document into a room of its own
	// once hers has closed, which would stay resident until it idles out (roomIdleTimeout). So the
	// test waits for that settlement to end, as nothing here arms another, and evicts the room it
	// may have left (Evict closes nothing where no room is).
	f.waitForNoSettlement(t)
	if err := f.service.Evict(ctx, f.artifactID); err != nil {
		t.Fatalf("evict a room the settlement of alice's keystroke may have loaded: %v", err)
	}
	waitFor(t, roomWait(t), "no room to hold the document", func() bool { return !roomResident(f.service, f.artifactID) })
	if _, err := f.database.Pool.Exec(ctx, `update doc_updates set update = '\xff'::bytea where artifact_id = $1`, f.artifactID); err != nil {
		t.Fatalf("make the document's history one that does not load: %v", err)
	}
	if _, err := NewPgVersioned(f.database).LoadDocument(ctx, f.artifactID); !errors.Is(err, ErrDocumentUnloadable) {
		t.Fatalf("load the document's history = %v, want ErrDocumentUnloadable", err)
	}
	unloadable := readDocumentWrites(t, f.database, f.artifactID)
	markdown := "Rebuilt from a person's markdown after the close.\n"
	_, _, err = rebuildDocument(t, f.service, f.artifactID, &markdown)
	f.requireRefusedStoringNothing(t, "a rebuild carrying markdown", err, unloadable)
}

// An ask block a browser writes into a room whose process has not heard its issue close gets no
// ask row, whatever that room reads (R2): the settlement goes by the issue row it locks
// (settlement.issueClosed), not by the room's record (roomState.closed). Task B's room loads while
// DOC-1 is open and runs no room sync, so nothing tells B of the close, which is written by SQL a
// minute ahead: the issue row reads closed from then on, and the store's close margin admits B's
// browser's block however long the run takes (observedBeforeClose). B's own timer settles the
// document; its record reads the issue open when the settlement holds the document's owner row,
// and the block is saved resolved with no row, its author recorded for the reopen.
func TestAnAskBlockOnAServiceThatHasNotHeardTheCloseGetsNoRow(t *testing.T) {
	pair := newProcessPair(t)
	ctx := context.Background()
	b := pair.taskB
	if err := b.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("load task B's room while DOC-1 is open: %v", err)
	}
	var readOpen, readClosed atomic.Int64
	b.afterSettleLock = func(room string) {
		if room != pair.artifactID {
			return
		}
		if b.roomClosed(room) {
			readClosed.Add(1)
		} else {
			readOpen.Add(1)
		}
	}
	b.settle = 20 * time.Millisecond
	if _, err := pair.database.Pool.Exec(ctx, `update issues set closed_at = now() + interval '1 minute' where key = 'DOC-1'`); err != nil {
		t.Fatalf("close DOC-1 in the database alone: %v", err)
	}

	bob := model.Actor{Kind: "user", ID: "bob"}
	b.addConnection(pair.artifactID, 1, bob)
	editAsPeer(t, b, pair.artifactID, appendBlocks(t, ":::ask{#unheard-close urgency=\"med\" multiple=\"false\"}\nAsked where the close is not heard?\n:::\n"))
	b.removeConnection(pair.artifactID, 1)
	const resolved = `#unheard-close urgency="med" multiple="false" state="resolved"`
	waitFor(t, roomWait(t), "task B's timer to settle the document on the closed issue", func() bool {
		_, markdown, _ := latestDocumentVersion(t, pair.database, pair.artifactID)
		owed, err := settlementPending(ctx, pair.database.Pool, pair.artifactID)
		return strings.Contains(markdown, "Asked where the close is not heard?") && err == nil && !owed
	})
	if readOpen.Load() == 0 || readClosed.Load() != 0 {
		t.Fatalf("task B's settlements read its room's record open %d times and closed %d times at the owner lock, want open every time: its process has not heard the close",
			readOpen.Load(), readClosed.Load())
	}
	var rows int
	if err := pair.database.Pool.QueryRow(ctx, `
		select count(*) from asks where block_artifact_id = $1 and block_id = 'unheard-close'
	`, pair.artifactID).Scan(&rows); err != nil {
		t.Fatalf("count the block's ask rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the block written on the closed issue has %d ask rows, want none", rows)
	}
	if _, markdown, _ := latestDocumentVersion(t, pair.database, pair.artifactID); !strings.Contains(markdown, resolved) {
		t.Fatalf("the settled version reads:\n%s\nwant the block saved resolved: %s", markdown, resolved)
	}
	recorded, err := readRecordedAskAuthors(ctx, pair.database.Pool, pair.artifactID)
	if err != nil {
		t.Fatalf("read the document's recorded ask block authors: %v", err)
	}
	if author, ok := recorded["unheard-close"]; !ok || !author.SameAs(bob) {
		t.Fatalf("the block's recorded author is %+v (recorded %t), want bob, for the ask the reopen's settlement opens", author, ok)
	}
}

// blockAskOf is the ask row of the document's ask block blockID.
func blockAskOf(t *testing.T, database *store.Store, artifactID, blockID string) model.Ask {
	t.Helper()
	ask, err := ScanAsk(database.Pool.QueryRow(context.Background(), `
		select `+AskColumns+` from asks a where a.block_artifact_id = $1 and a.block_id = $2
	`, artifactID, blockID))
	if err != nil {
		t.Fatalf("read the ask of block %s: %v", blockID, err)
	}
	return ask
}

// An ask a settlement retracted when its block left is restored once its issue reopens, though
// its block came back while the issue was closed (R2). alice deletes the block of an open ask,
// and the settlement retracts the ask. DOC-1 then closes, written by SQL a minute ahead with
// nothing telling the room, so the store's close margin admits alice's undo, which brings the
// block back (observedBeforeClose). The settlement the resumption runs on the closed issue leaves
// the ask retracted and saves its block resolved, and deletes the document's pending row. The
// reopen marks the document owed (MarkUnindexedAskDocumentsOwed), and one resumption pass settles
// it on the open issue, which restores the ask as a returning block's ask is restored.
func TestARetractedAsksBlockThatReturnsWhileClosedIsRestoredAtTheReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	const block = ":::ask{#returning-ask urgency=\"med\" multiple=\"false\"}\nWill it come back?\n:::\n"
	seedServiceText(t, service, artifactID, "Context.\n\n"+block)
	settleCurrentGeneration(t, service, artifactID)
	if ask := blockAskOf(t, service.store, artifactID, "returning-ask"); ask.State != "open" {
		t.Fatalf("the seeded ask block's ask reads %s, want open", ask.State)
	}
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, withoutAskBlocks)
	settleCurrentGeneration(t, service, artifactID)
	if ask := blockAskOf(t, service.store, artifactID, "returning-ask"); !settlementRetracted(ask) {
		t.Fatalf("the ask whose block alice deleted reads %s %+v, want the settlement's retraction", ask.State, ask.Resolution)
	}

	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the document's room while DOC-1 is open: %v", err)
	}
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = now() + interval '1 minute' where key = 'DOC-1'`); err != nil {
		t.Fatalf("close DOC-1 in the database alone: %v", err)
	}
	service.addConnection(artifactID, 2, alice)
	editAsPeer(t, service, artifactID, appendBlocks(t, block))
	service.removeConnection(artifactID, 2)
	waitForLandedAppends(t, service, artifactID)
	settled := func(what string) {
		t.Helper()
		waitFor(t, roomWait(t), what, func() bool {
			owed, err := settlementPending(ctx, service.store.Pool, artifactID)
			return err == nil && !owed
		})
	}
	service.settle = 20 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlement: %v", err)
	}
	settled("the closed issue's document to settle the returned block")
	const resolved = `#returning-ask urgency="med" multiple="false" state="resolved"`
	if _, markdown, _ := latestDocumentVersion(t, service.store, artifactID); !strings.Contains(markdown, resolved) {
		t.Fatalf("the closed issue's settlement left the latest version reading:\n%s\nwant the returned block saved resolved: %s", markdown, resolved)
	}
	if ask := blockAskOf(t, service.store, artifactID, "returning-ask"); !settlementRetracted(ask) {
		t.Fatalf("the closed issue's settlement left the ask %s %+v, want it still retracted while DOC-1 is closed", ask.State, ask.Resolution)
	}

	reopenTestIssue(t, service)
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	settled("the reopened issue's document to settle")
	if ask := blockAskOf(t, service.store, artifactID, "returning-ask"); ask.State != "open" || ask.Resolution != nil {
		t.Fatalf("after the reopen and a resumption pass the ask reads %s %+v, want it open again: its block is back in the document", ask.State, ask.Resolution)
	}
}
