package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A row parked over clocks a room already holds, stored before the keystrokes that later take
// those clocks. A typist types three characters, so the room holds the typist through clock 5;
// another browser sends one struct of deleted content over the typist's clocks 5 to 1004, whose
// right origin (77, 0) no document holds, so it parks and the store takes it as a row of its own
// (storeParked); the typist then types 997 more characters, clocks 5 to 1001, each a row after
// it. A document that applies the rows in the order they were stored holds all 1,000 characters
// and parks the struct. A merge of the rows (crdt.MergeUpdatesV1) keeps the struct, the longest
// at clock 5 and the first row to carry that clock, and drops every keystroke it covers, so a
// document the merge reaches holds 3 of the 1,000. Whether the struct parks is decided by the
// document that receives it, which a merge of updates does not have.
//
// Each test here reads the stored rows through one path a reader of them takes, and requires
// what the typist wrote back: its text and its state vector.

// parkedFirstTypist is the client that types the keystrokes the parked row is stored ahead of.
const parkedFirstTypist crdt.ClientID = 1000

// parkedFirstKeystrokes is how many characters the typist types, one row each.
const parkedFirstKeystrokes = 1000

// parkedFirstAhead is how many of them are typed before the parked row: clocks 2, 3 and 4, after
// the paragraph (0) and its text (1).
const parkedFirstAhead = 3

// parkedOverTheKeystrokes is one struct of deleted content 1,000 clocks long at the typist's clock
// 5, whose right origin (77, 0) no document holds: 11 bytes.
func parkedOverTheKeystrokes() []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1) // one client
		enc.WriteVarUint(1) // one struct
		enc.WriteVarUint(uint64(parkedFirstTypist))
		enc.WriteVarUint(2 + parkedFirstAhead)
		enc.WriteUint8(1 | 0x40) // deleted content, with a right origin
		enc.WriteVarUint(77)
		enc.WriteVarUint(0)
		enc.WriteVarUint(parkedFirstKeystrokes)
		enc.WriteVarUint(0) // no deletes
	})
}

// parkedFirstTyping is the typist's browser and the document it types into as parkedFirstTypist:
// the first keystroke types the paragraph and its first character, each later one character more,
// one frame each.
type parkedFirstTyping struct {
	browser *docstest.Peer
	writer  *crdt.Doc
	run     *crdt.YXmlText
	typed   int
}

func startParkedFirstTyping(t *testing.T, serverURL, artifactID string) *parkedFirstTyping {
	t.Helper()
	browser := connectPeer(t, serverURL, artifactID, "alice")
	waitFor(t, roomWait(t), "the typist to hold the document", func() bool {
		return strings.Contains(peerMarkdown(browser), "Seed.")
	})
	writer := newDocumentCopy(crdt.WithClientID(parkedFirstTypist))
	if err := crdt.ApplyUpdateV1(writer, crdt.EncodeStateAsUpdateV1(browser.Doc, nil), nil); err != nil {
		t.Fatalf("copy the typist's document: %v", err)
	}
	return &parkedFirstTyping{browser: browser, writer: writer}
}

func (c *parkedFirstTyping) typeUpTo(t *testing.T, characters int) {
	t.Helper()
	for ; c.typed < characters; c.typed++ {
		var update []byte
		if c.typed == 0 {
			update = docstest.Transact(c.writer, typeParagraph(c.writer, "a"))
			c.run = firstText(c.writer.GetXmlFragment(fragmentName))
		} else {
			at := c.typed
			update = docstest.Transact(c.writer, func(txn *crdt.Transaction) { c.run.Insert(txn, at, "a", nil) })
		}
		if err := c.browser.Write(ygsync.EncodeUpdate(update)); err != nil {
			t.Fatalf("send keystroke %d: %v", c.typed, err)
		}
	}
}

// storeParkedRowFirst has a typist on service's room at serverURL type parkedFirstAhead
// characters, another browser send parkedOverTheKeystrokes, and the typist type the rest, waiting
// at each step for the store, so the parked row is stored after the first keystrokes' rows and
// before every later one's. It returns the typist's document, which never took the parked row.
func storeParkedRowFirst(t *testing.T, service *Service, serverURL, artifactID string) *crdt.Doc {
	t.Helper()
	typing := startParkedFirstTyping(t, serverURL, artifactID)
	typing.typeUpTo(t, parkedFirstAhead)
	waitFor(t, roomWait(t), "the room to take the first keystrokes", func() bool {
		return strings.Count(currentLiveMarkdown(t, service, artifactID), "a") == parkedFirstAhead
	})
	waitForLandedAppends(t, service, artifactID)
	before := storedHead(t, service.store, artifactID)
	sender := connectPeer(t, serverURL, artifactID, "mallory")
	waitFor(t, roomWait(t), "the sender to hold the document", func() bool {
		return strings.Contains(peerMarkdown(sender), "Seed.")
	})
	if err := sender.Write(ygsync.EncodeUpdate(parkedOverTheKeystrokes())); err != nil {
		t.Fatalf("send the parked row: %v", err)
	}
	waitFor(t, roomWait(t), "the store to take the parked row", func() bool {
		return storedHead(t, service.store, artifactID) > before
	})
	if parked := service.srv.GetDoc(artifactID).PendingStats(); parked.Items != 1 {
		t.Fatalf("the room parks %+v after the parked row, want its one struct", parked)
	}
	typing.typeUpTo(t, parkedFirstKeystrokes)
	waitFor(t, roomWait(t), "the room to take every keystroke", func() bool {
		return strings.Count(currentLiveMarkdown(t, service, artifactID), "a") == parkedFirstKeystrokes
	})
	waitForLandedAppends(t, service, artifactID)
	return typing.writer
}

// parkedRowFirst is one process whose stored rows hold the parked row before the keystrokes it
// covers (storeParkedRowFirst), with the typist's document.
func parkedRowFirst(t *testing.T) (*Service, string, *crdt.Doc) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Seed.\n")
	writer := storeParkedRowFirst(t, service, newTestServer(t, service).URL, artifactID)
	return service, artifactID, writer
}

// heldKeystrokes is how many of the typist's characters doc holds, and the typist's clock in it.
func heldKeystrokes(doc *crdt.Doc) string {
	return fmt.Sprintf("%d of %d keystrokes, typist at clock %d",
		strings.Count(docMarkdown(doc), "a"), parkedFirstKeystrokes, doc.StateVector()[parkedFirstTypist])
}

// requireTheKeystrokes fails unless doc, what a reader got, renders as writer and holds writer's
// state vector.
func requireTheKeystrokes(t *testing.T, what string, doc, writer *crdt.Doc) {
	t.Helper()
	if doc == nil {
		t.Fatalf("%s holds no document", what)
	}
	if docMarkdown(doc) != docMarkdown(writer) || !maps.Equal(doc.StateVector(), writer.StateVector()) {
		t.Fatalf("%s holds %s, state vector %v; the writer's holds %s, state vector %v",
			what, heldKeystrokes(doc), doc.StateVector(), heldKeystrokes(writer), writer.StateVector())
	}
	t.Logf("%s holds %s, as the writer's does", what, heldKeystrokes(doc))
}

// readPath names the path read took to the stored rows' state by what it logged: a fold that
// read back otherwise served them applied in order (runsThrough).
func readPath(logs *storetest.LockedLog, read func()) string {
	before := strings.Count(logs.String(), misreadWarning+"; serving them applied in order")
	read()
	if strings.Count(logs.String(), misreadWarning+"; serving them applied in order") > before {
		return "the fold read back otherwise and served the rows applied in order (runsThrough)"
	}
	return "the fold read back"
}

// stateDocument is state decoded into a document as a room decodes its load.
func stateDocument(t *testing.T, state []byte) *crdt.Doc {
	t.Helper()
	doc := newDocumentCopy()
	if err := decodeStoredState(doc, state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	return doc
}

// A document load (LoadDocument, documentThrough) folds the rows in order and decodes the state
// the fold reads back, or, when it reads back otherwise, applies the rows' runs in order to one
// document (runsThrough, runsDocument).
func TestADocumentLoadOfAParkedRowStoredBeforeTheKeystrokesItCoversHoldsTheKeystrokes(t *testing.T) {
	logs := captureWarnings(t)
	service, artifactID, writer := parkedRowFirst(t)
	var loaded LoadedDocument
	path := readPath(logs, func() {
		var err error
		if loaded, err = NewPgVersioned(service.store).LoadDocument(context.Background(), artifactID); err != nil {
			t.Fatalf("load the document: %v", err)
		}
	})
	requireTheKeystrokes(t, "LoadDocument ("+path+")", loaded.Doc, writer)
}

// The state a room load decodes (Load, stateThrough) is the fold's, or, when the fold reads back
// otherwise, the encoding of the document the rows' runs make applied in order (runsThrough).
func TestARoomLoadsStateOfAParkedRowStoredBeforeTheKeystrokesItCoversHoldsTheKeystrokes(t *testing.T) {
	logs := captureWarnings(t)
	service, artifactID, writer := parkedRowFirst(t)
	var loaded persistence.LoadResult
	path := readPath(logs, func() {
		var err error
		if loaded, err = NewPgVersioned(service.store).Load(context.Background(), artifactID); err != nil {
			t.Fatalf("load the room's state: %v", err)
		}
	})
	requireTheKeystrokes(t, "Load ("+path+")", stateDocument(t, loaded.Update), writer)
}

// A version's state (MaterializeAt, stateThrough) at the head is what Load folds.
func TestAMaterializedHeadOfAParkedRowStoredBeforeTheKeystrokesItCoversHoldsTheKeystrokes(t *testing.T) {
	logs := captureWarnings(t)
	service, artifactID, writer := parkedRowFirst(t)
	head := storedHead(t, service.store, artifactID)
	var state []byte
	path := readPath(logs, func() {
		var err error
		if state, err = NewPgVersioned(service.store).MaterializeAt(context.Background(), artifactID, persistence.Version(head)); err != nil {
			t.Fatalf("materialize the head: %v", err)
		}
	})
	requireTheKeystrokes(t, fmt.Sprintf("MaterializeAt(%d) (%s)", head, path), stateDocument(t, state), writer)
}

// A room closed and loaded again takes the state Load folds.
func TestAReloadedRoomOfAParkedRowStoredBeforeTheKeystrokesItCoversHoldsTheKeystrokes(t *testing.T) {
	logs := captureWarnings(t)
	service, artifactID, writer := parkedRowFirst(t)
	ctx := context.Background()
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("close the room: %v", err)
	}
	path := readPath(logs, func() {
		if err := service.warmLiveDocument(ctx, artifactID); err != nil {
			t.Fatalf("load the room again: %v", err)
		}
	})
	requireTheKeystrokes(t, "the reloaded room ("+path+")", service.srv.GetDoc(artifactID), writer)
}

// The rows as a load serves them when the fold reads back otherwise: cut into runs (runsThrough) -
// the first keystroke stored after the parked row carries clocks below that row's end, so a run
// starts there - and applied in order to one document (runsDocument).
func TestTheMergedRowsOfAParkedRowStoredBeforeTheKeystrokesItCoversHoldTheKeystrokes(t *testing.T) {
	service, artifactID, writer := parkedRowFirst(t)
	ctx := context.Background()
	rows := storedRows(t, service.store, artifactID)
	updates := make([][]byte, len(rows))
	for i, row := range rows {
		updates[i] = row.update
	}
	runs, err := runsThrough(memoryUpdates(ctx, updates))
	if err != nil {
		t.Fatalf("cut the stored rows into runs: %v", err)
	}
	doc, err := runsDocument(ctx, runs, maxUpdateItems)
	if err != nil {
		t.Fatalf("apply the runs: %v", err)
	}
	requireTheKeystrokes(t, fmt.Sprintf("the %d stored rows applied as %d runs", len(rows), len(runs)), doc, writer)
}

// A compaction folds the rows into the head row, which every later read takes in their place, or
// leaves them as stored when the fold reads back otherwise.
func TestACompactedDocumentOfAParkedRowStoredBeforeTheKeystrokesItCoversHoldsTheKeystrokes(t *testing.T) {
	logs := captureWarnings(t)
	service, artifactID, writer := parkedRowFirst(t)
	ctx := context.Background()
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("close the room: %v", err)
	}
	versioned := NewPgVersioned(service.store)
	before := storedUpdateCount(t, service.store, artifactID)
	result := compactAged(t, versioned, service.store, artifactID)
	t.Logf("the compaction answered outcome %d, rows %d -> %d", result.outcome, before, storedUpdateCount(t, service.store, artifactID))
	var loaded LoadedDocument
	path := readPath(logs, func() {
		var err error
		if loaded, err = versioned.LoadDocument(ctx, artifactID); err != nil {
			t.Fatalf("load the compacted document: %v", err)
		}
	})
	requireTheKeystrokes(t, "LoadDocument after the compaction ("+path+")", loaded.Doc, writer)
}

// A share of a room's not-yet-stored updates (unstoredShare) merges them for a reader - a read of
// a room behind the head (storedWithUpdates) and another process's room query - and it cannot
// merge a parked row: a commit's parked bytes leave the room's update log in Parked, beside an
// Update holding only what the commit integrated (classifyDrained), and the commit that parks the
// row and nothing else lists no update at all. So the share holds the keystrokes, never the
// parked struct, and the stored document it is applied to reads whole. Here the keystrokes typed
// past the parked row are held from the store, so the share is all of them: ten, since a room
// whose appends are held did not take 997 of a browser's frames within the wait.
func TestAShareOfTheRoomsUnstoredUpdatesCarriesNoParkedRow(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, gate := newGatedServiceInstance(t, database)
	server := newTestServer(t, service)
	seedServiceText(t, service, artifactID, "Seed.\n")
	typing := startParkedFirstTyping(t, server.URL, artifactID)
	typing.typeUpTo(t, parkedFirstAhead)
	waitFor(t, roomWait(t), "the room to take the first keystrokes", func() bool {
		return strings.Count(currentLiveMarkdown(t, service, artifactID), "a") == parkedFirstAhead
	})
	waitForLandedAppends(t, service, artifactID)
	gate.hold()
	sender := connectPeer(t, server.URL, artifactID, "mallory")
	waitFor(t, roomWait(t), "the sender to hold the document", func() bool {
		return strings.Contains(peerMarkdown(sender), "Seed.")
	})
	if err := sender.Write(ygsync.EncodeUpdate(parkedOverTheKeystrokes())); err != nil {
		t.Fatalf("send the parked row: %v", err)
	}
	live := service.srv.GetDoc(artifactID)
	waitFor(t, roomWait(t), "the room to park the parked row", func() bool { return live.PendingStats().Items == 1 })
	typed := parkedFirstAhead + 10
	typing.typeUpTo(t, typed)
	waitFor(t, roomWait(t), "the room to take every keystroke", func() bool {
		return strings.Count(currentLiveMarkdown(t, service, artifactID), "a") == typed
	})

	share, err := service.unstoredShare(artifactID, live)
	if err != nil {
		t.Fatalf("share the room's unstored updates: %v", err)
	}
	if len(share) == 0 {
		t.Fatal("the room shares nothing while every keystroke past the parked row is held from the store, so nothing here is tested")
	}
	from, to, err := crdt.ParseUpdateMeta(share)
	if err != nil {
		t.Fatalf("read the share's clocks: %v", err)
	}
	t.Logf("the share carries the typist's clocks %d to %d", from[parkedFirstTypist], to[parkedFirstTypist])
	if to[parkedFirstTypist] != typing.writer.StateVector()[parkedFirstTypist] {
		t.Fatalf("the share carries the typist's clocks %d to %d, past the %d the typist wrote: it carries the parked struct",
			from[parkedFirstTypist], to[parkedFirstTypist], typing.writer.StateVector()[parkedFirstTypist])
	}
	tree, err := service.storedWithUpdates(context.Background(), artifactID, share)
	if err != nil {
		t.Fatalf("read the stored document with the share: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the stored document with the share: %v", err)
	}
	if want := docMarkdown(typing.writer); markdown != want {
		t.Fatalf("the stored document with the share holds %d of %d keystrokes", strings.Count(markdown, "a"), typed)
	}
	t.Logf("the stored document with the share holds %d of %d keystrokes", strings.Count(markdown, "a"), typed)
}

// unappliedKeystrokes is how many characters the typist types in the history
// runsPastThePendingQueue stores: past the parked struct's first clock, in far fewer rows than
// parkedFirstKeystrokes.
const unappliedKeystrokes = 20

// runsPastThePendingQueue is a history whose runs do not apply in order within the pending queue
// shrinkPendingItems sets: a seed, the typist's unappliedKeystrokes keystrokes with the parked
// row (parkedOverTheKeystrokes) stored after the first parkedFirstAhead of them when parkedFirst
// and after all of them otherwise, then another client's two rows of testPendingItems/2+1 inserts
// each, which wait on an insert of that client's own that no row before them holds, and last that
// insert. Applied in order, the second of those rows parks past the queue. Merged, the rows park
// only the parked struct, since the insert the others wait on is in the merge, and the merge
// keeps that struct over every keystroke from clock 5 on.
func runsPastThePendingQueue(t *testing.T, parkedFirst bool) [][]byte {
	t.Helper()
	seed := newDocumentCopy(crdt.WithClientID(1))
	seedRow := docstest.Transact(seed, typeParagraph(seed, "Seed."))
	copyOf := func(client crdt.ClientID) *crdt.Doc {
		doc := newDocumentCopy(crdt.WithClientID(client))
		if err := crdt.ApplyUpdateV1(doc, seedRow, nil); err != nil {
			t.Fatalf("copy the seed: %v", err)
		}
		return doc
	}
	typist := copyOf(parkedFirstTypist)
	var run *crdt.YXmlText
	rows := [][]byte{seedRow}
	for typed := range unappliedKeystrokes {
		if parkedFirst && typed == parkedFirstAhead {
			rows = append(rows, parkedOverTheKeystrokes())
		}
		if typed == 0 {
			rows = append(rows, docstest.Transact(typist, typeParagraph(typist, "a")))
			run = firstText(typist.GetXmlFragment(fragmentName))
			continue
		}
		rows = append(rows, docstest.Transact(typist, func(txn *crdt.Transaction) { run.Insert(txn, typed, "a", nil) }))
	}
	if !parkedFirst {
		rows = append(rows, parkedOverTheKeystrokes())
	}
	other := copyOf(7)
	text := firstText(other.GetXmlFragment(fragmentName))
	awaited := docstest.Transact(other, func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	for range 2 {
		rows = append(rows, docstest.Transact(other, func(txn *crdt.Transaction) {
			for range testPendingItems/2 + 1 {
				text.Insert(txn, 0, "y", nil)
			}
		}))
	}
	return append(rows, awaited)
}

// decodedAsARoom is what a room's load makes of state (decodeStoredCopy) under the pending queue
// shrinkPendingItems sets: its refusal, or how many of the typist's keystrokes it holds.
func decodedAsARoom(state []byte) string {
	decoded, refused := decodeStoredCopy(state, testPendingItems)
	if refused != nil {
		return fmt.Sprintf("which a room's load refuses (%v)", refused)
	}
	return fmt.Sprintf("which a room's load decodes holding %d of the %d keystrokes",
		strings.Count(docMarkdown(decoded.doc), "a"), unappliedKeystrokes)
}

// A history whose runs do not apply in order within the store's pending queue
// (runsPastThePendingQueue) makes no state: Load and MaterializeAt answer ErrDocumentUnloadable
// and no bytes, and so do LoadDocument, a cold read, a socket's admission and a room's load, since
// a merge of those runs is what a room's load would have decoded, and the merge keeps the parked
// struct over the keystrokes it covers. Its rebuild retires every client its rows name, at the
// clock each reached, which it reads from the runs themselves. The parked row stored first and
// stored last both make such a history, under ygo v1.55.1-theorem.3 and with the fix of its parked
// merge alike: the fold of either parks past the queue whatever that merge does.
func TestAHistoryWhoseRunsDoNotApplyInOrderIsUnloadableAndItsRebuildRetiresItsClients(t *testing.T) {
	for _, order := range []struct {
		name        string
		parkedFirst bool
	}{
		{"the parked row first", true},
		{"the parked row last", false},
	} {
		t.Run(order.name, func(t *testing.T) {
			service, versioned, _ := roomLoadService(t)
			shrinkPendingItems(t, service, versioned)
			ctx := context.Background()
			artifactID := createDocument(t, service.store, "before\n")
			rows := runsPastThePendingQueue(t, order.parkedFirst)
			appendUpdates(t, versioned, artifactID, rows...)
			runs, err := runsThrough(memoryUpdates(ctx, rows))
			if err != nil {
				t.Fatalf("cut the stored rows into runs: %v", err)
			}
			if _, err := runsDocument(ctx, runs, testPendingItems); !errors.Is(err, ErrDocumentUnloadable) {
				t.Fatalf("the %d rows' %d runs applied in order = %v, want ErrDocumentUnloadable: nothing here is tested", len(rows), len(runs), err)
			}
			if loaded, err := versioned.Load(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) {
				t.Errorf("Load = %d bytes, %s (%v); want ErrDocumentUnloadable", len(loaded.Update), decodedAsARoom(loaded.Update), err)
			}
			head := storedHead(t, service.store, artifactID)
			if state, err := versioned.MaterializeAt(ctx, artifactID, persistence.Version(head)); !errors.Is(err, ErrDocumentUnloadable) {
				t.Errorf("MaterializeAt(%d) = %d bytes, %s (%v); want ErrDocumentUnloadable", head, len(state), decodedAsARoom(state), err)
			}
			if _, err := versioned.LoadDocument(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) {
				t.Errorf("LoadDocument = %v, want ErrDocumentUnloadable", err)
			}
			if text, err := service.Text(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
				t.Errorf("a cold read of the document = %q holding %d of the %d keystrokes (%v), want ErrDocumentUnloadable and not ErrServiceUnavailable",
					text, strings.Count(text, "a"), unappliedKeystrokes, err)
			}
			if tree, _, _, err := service.loadTree(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
				var markdown string
				if tree != nil {
					markdown, _ = renderTree(tree)
				}
				t.Errorf("a socket's admission read = %q holding %d of the %d keystrokes (%v), want ErrDocumentUnloadable and not ErrServiceUnavailable",
					markdown, strings.Count(markdown, "a"), unappliedKeystrokes, err)
			}
			if err := service.warmLiveDocument(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) {
				held := "no room"
				if live := service.srv.GetDoc(artifactID); live != nil {
					held = fmt.Sprintf("a room holding %d of the %d keystrokes", strings.Count(currentLiveMarkdown(t, service, artifactID), "a"), unappliedKeystrokes)
				}
				t.Errorf("a room's load = %v and left %s, want ErrDocumentUnloadable", err, held)
			}
			if t.Failed() {
				t.FailNow()
			}

			named := make(map[crdt.ClientID]uint64)
			for _, row := range rows {
				_, to, err := crdt.ParseUpdateMeta(row)
				if err != nil {
					t.Fatalf("read a stored row's clocks: %v", err)
				}
				for client, clock := range to {
					named[client] = max(named[client], clock)
				}
			}
			if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
				t.Fatalf("rebuild the document: %v", err)
			}
			rebuilt, err := versioned.LoadDocument(ctx, artifactID)
			if err != nil {
				t.Fatalf("load the rebuilt document: %v", err)
			}
			if retired := retiredClientsOf(rebuilt.Doc); !maps.Equal(retired, named) {
				t.Fatalf("the rebuild retired %v, want every client the stored rows name at the clock each reached, %v", retired, named)
			}
			if text, err := service.Text(ctx, artifactID); err != nil || text != "before\n" {
				t.Fatalf("a read of the rebuilt document = %q (%v), want its latest version's markdown", text, err)
			}
			t.Logf("%d rows in %d runs: every read is ErrDocumentUnloadable, and the rebuild retired %v", len(rows), len(runs), retiredClientsOf(rebuilt.Doc))
		})
	}
}
