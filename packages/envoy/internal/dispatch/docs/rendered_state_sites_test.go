package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Four places where a document's rendering is made for one state and then kept, published or
// stored: a catch-up of another process's stored rows, a rebuild's seed and the version it writes,
// a drain of the room's pending queue the room's apply check passes or refuses or the apply's scan
// allowance stops, and a replica's recover. Each test below compares what the place leaves standing
// with a whole render of the state it stands for (requireRenderedAs, or the stored seed rendered
// whole), and where the replica follows the room, the replica's document with the room's
// (requireReplicaIsRoom).

// A catch-up applies another process's stored rows to this process's room as a remote apply of an
// origin of its own (remoteUpdateOrigin, catchUpRoom), which the room's apply check passes without
// rendering, so its report carries no rendering (roomApplies.noteApply): the replica measures the
// commit itself, on its own copy of the room (updateChangesMarkdown), and the room's record of the
// last browser's apply stays at that apply's commit, before the catch-up's, where neither the next
// apply's check (renderCheck.base) nor the replica (madeReport.checkedAt) takes it for a later
// commit. Here a browser's keystroke into task A's room leaves that record standing, a browser on
// task B then appends a paragraph and rewrites a run of an existing one, and task A's catch-up
// takes both in: what the room places at the catch-up's commit is a whole render of the room, and so
// is what it places at the next keystroke, which the check splices into the catch-up's rendering.
func TestACatchUpLeavesTheRoomPublishingAWholeRenderOfWhatItTookIn(t *testing.T) {
	pair := newProcessPair(t)
	ctx := context.Background()
	if err := pair.taskA.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	live := pair.taskA.srv.GetDoc(pair.artifactID)
	replica := pair.taskA.replicaOf(live)
	if live == nil || replica == nil {
		t.Fatal("task A's room is not resident with its replica")
	}
	room := &rcRoom{service: pair.taskA, live: live, replica: replica, origin: new(int)}
	keystroke := func(what, typed string) {
		t.Helper()
		update := room.edit(t, func(txn *crdt.Transaction, _ *crdt.Doc) {
			paragraphTextsOf(txn.GetXmlFragment(fragmentName))[0].Insert(txn, 0, typed, nil)
		})
		info := websocket.PeerUpdateInfo{Room: pair.artifactID, Doc: live, Update: update, Origin: room.origin}
		if err := pair.taskA.weighPeerUpdate(info, func() error { return crdt.ApplyUpdateV1(live, update, room.origin) }); err != nil {
			t.Fatalf("%s was answered %v, want it taken", what, err)
		}
		whole, err := renderDocument(live)
		if err != nil {
			t.Fatalf("%s: render the room: %v", what, err)
		}
		room.requireRenderedAs(t, what, whole)
	}

	keystroke("alice's keystroke before the catch-up", "Alice ")
	if record := replica.applies().checked.Load(); record == nil || record.rendering == nil {
		t.Fatal("the keystroke left the room no record of its rendering, so nothing here is tested")
	}
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob, appendBlocks(t, "Bob's paragraph.\n"))
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob, replaceRun("Second.", "Second, as bob rewrote it."))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	outcome, err := pair.taskA.catchUpRoom(ctx, pair.database.Pool, pair.artifactID, live)
	if err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task A's room up = %v, %v, want catchUpApplied", outcome, err)
	}
	whole, err := renderDocument(live)
	if err != nil {
		t.Fatalf("render the caught-up room: %v", err)
	}
	if !strings.Contains(whole, "Bob's paragraph.") || !strings.Contains(whole, "as bob rewrote it") || !strings.Contains(whole, "Alice ") {
		t.Fatalf("the caught-up room renders %q, want alice's keystroke and both of bob's edits, so nothing here is tested", whole)
	}
	room.requireRenderedAs(t, "task A's catch-up of bob's two edits", whole)
	keystroke("alice's keystroke after the catch-up", "again ")
}

// A rebuild replaces a stored history with one seed, and what it writes is of the state it
// replaces or of the text it was given (RebuildDocument): a history that loads is seeded from its
// own tree, the room's edits no version holds included, with no version written beside it, since
// no version is of that state; supplied markdown seeds the tree it parses to and versions that same
// tree's rendering, so the version it writes is a whole render of the stored seed, whether the
// history it replaces loads or not. Here a resident room near the state cap holding a browser's
// paragraph no version holds is rebuilt from its own text and from supplied markdown, and a history
// that does not load is rebuilt from supplied markdown.
func TestARebuildSeedsTheStateItReplacesAndVersionsAWholeRenderOfItsSeed(t *testing.T) {
	supplied := "# Rebuilt\n\nA paragraph with *emphasis*, `code` and a [link](https://example.com).\n\n- one\n- two\n\n> quoted\n"
	alice := model.Actor{Kind: "user", ID: "alice"}
	ctx := context.Background()
	for _, row := range []struct {
		name     string
		markdown *string
		loads    bool
	}{
		{"a resident room near the state cap, rebuilt from its own text", nil, true},
		{"a resident room near the state cap, rebuilt from supplied markdown", &supplied, true},
		{"a history that does not load, rebuilt from supplied markdown", &supplied, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var service *Service
			var artifactID, before string
			if row.loads {
				service, artifactID = newTestService(t)
				service.settle = time.Hour
				seedServiceText(t, service, artifactID, "# Resident\n\nA room the hook refuses an ordinary edit in.\n")
				const unversioned = "Alice's paragraph no version holds."
				editAsConnectedPeer(t, service, artifactID, 1, alice, appendBlocks(t, unversioned+"\n"))
				waitForLandedAppends(t, service, artifactID)
				live := service.srv.GetDoc(artifactID)
				if live == nil {
					t.Fatal("the document's room is not resident")
				}
				rendered, err := renderDocument(live)
				if err != nil {
					t.Fatal(err)
				}
				before = rendered
				if latest, _ := latestVersionOf(t, service.store, artifactID); !strings.Contains(before, unversioned) || strings.Contains(latest, unversioned) {
					t.Fatalf("the room renders %q and its latest version %q, want alice's paragraph in the room alone, so nothing here is tested", before, latest)
				}
				service.roomWeighingOf(live).applies().loaded(maxRoomStateBytes - 1<<10)
			} else {
				database := storetest.Open(t)
				artifactID = createDocument(t, database, "before\n")
				service = New(Deps{Store: database, Persistence: &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}})
				t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			}
			_, written, err := rebuildDocument(t, service, artifactID, row.markdown)
			if err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			loaded, err := service.persistence.Load(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			seed, err := renderDocument(decodedHistory(t, loaded.Update))
			if err != nil {
				t.Fatalf("render the stored seed: %v", err)
			}
			if row.markdown == nil {
				if written.Wrote {
					t.Fatalf("the rebuild from the history's own text wrote version %d, want none", written.Version.Number)
				}
				if seed != before {
					t.Fatalf("the stored seed is not the state the rebuild replaced: %s", renderingDifference(seed, before))
				}
			} else {
				if !written.Wrote {
					t.Fatal("the rebuild from supplied markdown wrote no version")
				}
				if stored := versionMarkdown(t, service.store, artifactID, written.Version.Number); stored != seed {
					t.Fatalf("version %d stores a rendering that is not a whole render of the stored seed: %s", written.Version.Number, renderingDifference(stored, seed))
				}
			}
			if text, err := service.Text(ctx, artifactID); err != nil || text != seed {
				t.Fatalf("the rebuilt document reads %q (%v), want its seed's %q", text, err, seed)
			}
		})
	}
}

// gapNewlinesClient is the client the newlines of gapFrames, and of the settlement's own gap, are
// typed under: a browser of its own, so the room holds nothing of it until their gap fills and a
// drain lands them.
const gapNewlinesClient = 4242

// gapFrames is a frame of newlines one browser types into room's code block right after another
// browser's keystroke, and that keystroke: sent first, the newlines park behind the keystroke the
// room does not hold yet, and the keystroke then fills their gap, so its apply drains them.
func gapFrames(t *testing.T, room *rcRoom, newlines int) (parked, fill []byte) {
	t.Helper()
	browser := room.browserCopy(t)
	code := allTexts(browser.GetXmlFragment(fragmentName))[1]
	held := room.live.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { code.Insert(txn, 0, "FILL", nil) })
	fill = crdt.EncodeStateAsUpdateV1(browser, held)
	other := newDocumentCopy(crdt.WithClientID(gapNewlinesClient))
	if err := crdt.ApplyUpdateV1(other, crdt.EncodeStateAsUpdateV1(browser, nil), nil); err != nil {
		t.Fatal(err)
	}
	otherCode := allTexts(other.GetXmlFragment(fragmentName))[1]
	filled := other.StateVector()
	other.Transact(func(txn *crdt.Transaction) { otherCode.Insert(txn, 4, strings.Repeat("\n", newlines), nil) })
	return crdt.EncodeStateAsUpdateV1(other, filled), fill
}

// gapNewlines is the two frames of gapFrames written on sender, a browser's document of the gap
// room (gapRoom's state): the keystroke that fills the gap, and the 5,000 newlines a browser of
// its own, gapNewlinesClient, types right after it.
func gapNewlines(t *testing.T, sender *crdt.Doc) (fill, parked []byte) {
	t.Helper()
	code := allTexts(sender.GetXmlFragment(fragmentName))[1]
	fill = docstest.Transact(sender, func(txn *crdt.Transaction) { code.Insert(txn, 0, "FILL", nil) })
	other := crdt.New(crdt.WithClientID(gapNewlinesClient))
	if err := crdt.ApplyUpdateV1(other, crdt.EncodeStateAsUpdateV1(sender, nil), nil); err != nil {
		t.Fatal(err)
	}
	otherCode := allTexts(other.GetXmlFragment(fragmentName))[1]
	parked = docstest.Transact(other, func(txn *crdt.Transaction) { otherCode.Insert(txn, 4, strings.Repeat("\n", 5_000), nil) })
	return fill, parked
}

// gapRoom is a room of 300,000 bytes of prose and a code block nested 98 quotes deep, where each
// newline typed into the code block renders 197 bytes, and the frames of gapFrames into it: the
// newlines parked, with the replica drained as the room's load drains a commit that only parks
// (onLoadDocument) unless held, and the keystroke that fills their gap, not yet sent.
func gapRoom(t *testing.T, newlines int, held bool) (room *rcRoom, fill []byte) {
	t.Helper()
	prefix := strings.Repeat("> ", 98)
	room = newRCRoom(t, stateOf(t, rcProse(300_000)+"\n\n"+prefix+"```\n"+prefix+"x\n"+prefix+"```\n"))
	parked, fill := gapFrames(t, room, newlines)
	room.held.Store(held)
	if err := room.weigh(parked); err != nil {
		t.Fatalf("the parked newlines were answered %v, want them taken", err)
	}
	if parked := room.live.PendingStats(); parked.Items == 0 {
		t.Fatalf("the room parks %+v, want the newlines parked behind the keystroke it does not hold", parked)
	}
	if !held {
		room.replica.observe("room")
	}
	whole, err := renderDocument(room.live)
	if err != nil {
		t.Fatal(err)
	}
	room.requireRenderedAs(t, "the parked newlines", whole)
	return room, fill
}

// A drain of the room's pending queue is checked on its own once the update that filled its gap
// has passed (crdt.ErrApplyCheckDrain): a drain the check refuses is discarded while the update
// stands (leaveParkedOut), so the rendering the room keeps for the commit is the update's own
// check's, and a drain it passes lands, so the rendering is the drain's (roomApplies.passed).
// Either way what the room places at the commit - its record of its rendering, and what the
// replica publishes once it takes the commit (takeChecked) - is a whole render of the room there:
// here 5,000 newlines, which would take the room past the bound and its creep, and 3, which would
// not, each parked behind a keystroke that then fills their gap, with the replica following the
// room and held behind it.
func TestADrainTheCheckPassesOrRefusesLeavesTheRenderingOfTheStateItsCommitStandsAt(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	for _, row := range []struct {
		name     string
		newlines int
		held     bool
		refused  bool
	}{
		{"a refused drain", 5_000, false, true},
		{"a refused drain, the replica held behind", 5_000, true, true},
		{"a landed drain", 3, false, false},
		{"a landed drain, the replica held behind", 3, true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			room, fill := gapRoom(t, row.newlines, row.held)
			if err := room.frame(t, row.name+", the keystroke filling the gap", fill); err != nil {
				t.Fatalf("the keystroke filling the gap was answered %v, want it taken", err)
			}
			if parked := room.live.PendingStats().Items; parked != 0 {
				t.Fatalf("the room parks %d structs after the keystroke, want the newlines landed or discarded", parked)
			}
			if landed := room.live.StateVector().Clock(gapNewlinesClient) > 0; landed == row.refused {
				t.Fatalf("the room holds the newlines %t after the keystroke, want %t", landed, !row.refused)
			}
			if row.held {
				room.release()
				whole, err := renderDocument(room.live)
				if err != nil {
					t.Fatal(err)
				}
				room.requireRenderedAs(t, row.name+", once released", whole)
			}
		})
	}
}

// The replica follows the room by what each commit integrated (crdt.LoggedUpdate.Update), so it
// must hold what the room holds. A drain the room's apply check refuses is discarded in the room,
// and the commit's update leaves it out; the replica never held the newlines, which parked in the
// room and in no commit's update, so its apply of the keystroke that filled the gap drains nothing
// of its own. The rendering the replica takes for that commit is the check's, of the room, but each
// one it makes after it is of its own document: a server write of one character into the code
// block is spliced from the replica's code block, so a replica holding the newlines would publish,
// at the room's commit, a rendering of 5,000 newlines the room does not hold - past the bound,
// where the room renders about 300 KB - and a browser's keystroke into the prose, weighed against
// it, would be refused as growing the document past the bound. The keystroke is what an editor
// sends, its transaction's own update (rcRoom.edit).
func TestARefusedDrainLeavesTheReplicaHoldingWhatTheRoomHolds(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	room, fill := gapRoom(t, 5_000, false)
	if err := room.frame(t, "the keystroke filling the gap", fill); err != nil {
		t.Fatalf("the keystroke filling the gap was answered %v, want it taken", err)
	}
	if room.live.StateVector().Clock(gapNewlinesClient) != 0 {
		t.Fatal("the room landed the newlines, so the check refused no drain and nothing here is tested")
	}
	if roomParks, replicaParks := room.live.PendingStats(), room.replica.doc.PendingStats(); roomParks.Items != 0 || replicaParks.Items != 0 {
		t.Errorf("after the refused drain the room parks %+v and the replica %+v, want the newlines discarded from both", roomParks, replicaParks)
	}
	roomRendering, roomErr := renderDocument(room.live)
	replicaRendering, replicaErr := renderDocument(room.replica.doc)
	if roomErr != nil || replicaErr != nil || replicaRendering != roomRendering {
		t.Errorf("after the refused drain the replica's document renders %d bytes (%v) and the room %d (%v): %s",
			len(replicaRendering), replicaErr, len(roomRendering), roomErr, renderingDifference(replicaRendering, roomRendering))
	}

	code := allTexts(room.live.GetXmlFragment(fragmentName))[1]
	room.live.Transact(func(txn *crdt.Transaction) { code.Insert(txn, 0, "y", nil) }, &struct{ server string }{"rendered state sites test"})
	keystroke := room.edit(t, func(txn *crdt.Transaction, _ *crdt.Doc) {
		allTexts(txn.GetXmlFragment(fragmentName))[0].Insert(txn, 0, "z", nil)
	})
	if err := room.frame(t, "a keystroke into the prose after a server write into the code block", keystroke); err != nil {
		rendering, _ := renderDocument(room.live)
		t.Errorf("a keystroke into the prose was answered %v, while the room renders %d bytes", err, len(rendering))
	}
	whole, err := renderDocument(room.live)
	if err != nil {
		t.Fatal(err)
	}
	room.requireRenderedAs(t, "after a server write into the code block", whole)
}

// The one replica that holds anything parked is one whose load decoded it - rows an earlier build
// stored of what parked, which the load decodes into the room and into the copy it hands the
// replica (startFromLoad) - and that replica's document refuses a drain of its own that lands any
// of it (holdParkedIn), as gapRoom's replica, holding nothing parked, needs no check to. Here the
// stored rows hold gapFrames' 5,000 newlines parked behind the keystroke the room lacks: the
// keystroke fills their gap, the room's check refuses the drain it releases and the room discards
// it, and the replica, whose own drain of the same newlines its check refuses, discards them too
// and holds what the room holds. A replica's copy given no check would land the newlines and
// render a megabyte the room does not hold.
func TestAReplicaLoadedHoldingParkedRowsRefusesTheDrainTheRoomDiscards(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	prefix := strings.Repeat("> ", 98)
	state := stateOf(t, rcProse(300_000)+"\n\n"+prefix+"```\n"+prefix+"x\n"+prefix+"```\n")
	parked, fill := gapFrames(t, newRCRoom(t, state), 5_000)
	load := func() *crdt.Doc {
		doc := newDocumentCopy()
		for _, row := range [][]byte{state, parked} {
			if err := crdt.ApplyUpdateV1(doc, row, nil); err != nil {
				t.Fatal(err)
			}
		}
		return doc
	}
	live, decoded := load(), load()
	if held := decoded.PendingStats(); held.Items == 0 {
		t.Fatalf("the load's copy parks %+v, want the newlines parked behind the keystroke it lacks", held)
	}
	room := loadedRCRoom(t, live, decoded)
	if room.replica.doc != decoded {
		t.Fatal("the replica did not take the load's copy, so nothing here is tested")
	}
	if err := room.frame(t, "the keystroke filling the gap", fill); err != nil {
		t.Fatalf("the keystroke filling the gap was answered %v, want it taken", err)
	}
	if room.live.StateVector().Clock(gapNewlinesClient) != 0 {
		t.Fatal("the room landed the newlines, so the check refused no drain and nothing here is tested")
	}
	if landed := room.replica.doc.StateVector().Clock(gapNewlinesClient); landed != 0 {
		t.Errorf("the replica landed the newlines the room discarded, to clock %d", landed)
	}
	roomRendering, roomErr := renderDocument(room.live)
	replicaRendering, replicaErr := renderDocument(room.replica.doc)
	if roomErr != nil || replicaErr != nil || replicaRendering != roomRendering {
		t.Errorf("after the refused drain the replica's document renders %d bytes (%v) and the room %d (%v): %s",
			len(replicaRendering), replicaErr, len(roomRendering), roomErr, renderingDifference(replicaRendering, roomRendering))
	}
}

// A drain the apply's scan allowance cannot hold is discarded in the room while the update that
// filled its gap stands (roomScans, leaveParkedOut), as a drain the room's apply check refuses is,
// and what it scanned is not charged to the room's scan total (roomScans.settle): here 20 items
// of one client, each naming the first of 2,000 characters a browser typed one a keystroke as its
// left origin, park behind the client's missing first clock, and the item that fills it is
// admitted with its drain discarded, the drain's scans passing what the filler's allowance leaves.
// The replica's document is the room's after the filler (requireReplicaIsRoom, which frame holds),
// and so is what the replica renders after it: a server write into the paragraph is spliced from
// the replica's paragraph, which a replica holding the 20 items would render with them.
func TestADrainTheScanAllowanceStopsLeavesTheReplicaHoldingWhatTheRoomHolds(t *testing.T) {
	const typed, waiting = 2_000, 20
	room := newRCRoom(t, stateOf(t, "# Title\n\nOne paragraph of ordinary prose.\n"))
	typeOnto(t, room.service, room.live, tangledTypist, typed)
	parked := newV1Frame(1).group(waiting, 9, 1)
	for range waiting {
		parked.raw(structStringRef | infoHasOrigin).uint(uint64(tangledTypist)).uint(0).str("z")
	}
	if err := room.frame(t, "the parked items", parked.noDeletes()); err != nil {
		t.Fatalf("the parked items were answered %v, want them admitted", err)
	}
	if pending := room.live.PendingStats(); pending.Items != waiting {
		t.Fatalf("the room parks %+v, want the %d items", pending, waiting)
	}
	filler := newV1Frame(1).group(1, 9, 0).
		raw(structStringRef | infoHasOrigin).uint(uint64(tangledTypist)).uint(typed - 1).str("f").noDeletes()
	if allowance := browserScanCeiling(filler); waiting*(typed-1) <= allowance {
		t.Fatalf("the parked items would scan %d items, want past the filler's %d allowance", waiting*(typed-1), allowance)
	}
	usedBefore, _ := room.replica.roomScans().figures()
	if err := room.frame(t, "the filler, whose drain the scan allowance discards", filler); err != nil {
		t.Fatalf("the filler was answered %v, want it admitted with its drain discarded", err)
	}
	if held, pending := room.live.StateVector().Clock(9), room.live.PendingStats(); held != 1 || pending.Items != 0 {
		t.Fatalf("the room holds client 9 to clock %d and parks %+v, want the filler alone landed and the %d items discarded, so no drain was discarded and nothing here is tested",
			held, pending, waiting)
	}
	if usedAfter, _ := room.replica.roomScans().figures(); usedAfter-usedBefore >= int64(waiting*(typed-1)) {
		t.Errorf("the room's scan total grew by %d over the filler, want less than the %d its discarded drain scanned", usedAfter-usedBefore, waiting*(typed-1))
	}
	text := paragraphTexts(room.live)[0]
	room.live.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "s", nil) }, &struct{ server string }{"rendered state sites test"})
	whole, err := renderDocument(room.live)
	if err != nil {
		t.Fatal(err)
	}
	room.requireRenderedAs(t, "a server write into the paragraph after the drain was discarded", whole)
}

// A settlement versions the room as its replica holds it at the point it reads (atPoint): the
// replica's document and its rendering of it (replicaRead). After a drain the room's apply check
// refused and discarded, the version it stores is a whole render of the room: right after the
// refused drain, where the replica holds the check's rendering of the commit (takeChecked), and
// after a write of the server's own into the code block the newlines would have gone into, which
// the replica renders from its own document (updateChangesMarkdown), so a replica holding the
// newlines the room discarded would version them.
func TestASettlementAfterARefusedDrainVersionsAWholeRenderOfTheRoom(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	for _, row := range []struct {
		name        string
		serverWrite bool
	}{
		{"settled after the refused drain", false},
		{"settled after a server write into the code block", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			prefix := strings.Repeat("> ", 98)
			storeHistory(t, service, artifactID, stateOf(t, rcProse(300_000)+"\n\n"+prefix+"```\n"+prefix+"x\n"+prefix+"```\n"))
			closeResidentRoom(t, service, artifactID)
			url := socketURL(newTestServer(t, service).URL, artifactID)
			sender := docstest.Dial(t, url, pageHeader("alice"), artifactID, crdt.New())
			awaitAnswer(t, sender, "the document")

			fill, parked := gapNewlines(t, sender.Doc)
			if err := sender.Write(ygsync.EncodeUpdate(parked)); err != nil {
				t.Fatal(err)
			}
			if err := sender.Write(ygsync.EncodeUpdate(fill)); err != nil {
				t.Fatal(err)
			}
			client, typed := sender.Doc.ClientID(), sender.Doc.StateVector().Clock(sender.Doc.ClientID())
			waitFor(t, time.Minute, "the room to take the keystroke that fills the gap", func() bool {
				live := service.srv.GetDoc(artifactID)
				return live != nil && live.StateVector().Clock(client) == typed
			})
			live := roomDocument(t, service, artifactID)
			if live.StateVector().Clock(gapNewlinesClient) != 0 {
				t.Fatal("the room drained the newlines, so the check refused no drain and nothing here is tested")
			}
			if row.serverWrite {
				if err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
					text := allTexts(doc.GetXmlFragment(fragmentName))[1]
					transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "y", nil) })
				}); err != nil {
					t.Fatal(err)
				}
			}
			settleNow(service, artifactID)
			stored, _ := latestVersionOf(t, service.store, artifactID)
			whole, err := renderDocument(live)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stored, "FILL") {
				t.Fatalf("the latest version (%d bytes) holds no keystroke, so the settlement versioned nothing after it and nothing here is tested", len(stored))
			}
			if stored != whole {
				t.Fatalf("the settlement stored a version that is not a whole render of the room: %s", renderingDifference(stored, whole))
			}
		})
	}
}

// A replica that cannot place a commit gives its log back (renderedReplica.recover): the log
// resets, hands over what it held, and records a fresh base at the room's next commit. What the
// room's apply check rendered of the commits handed over stays where it was made - the room's
// record of its rendering (roomApplies.checked) and those commits' reports - and each names the
// commit position it is of, which the reset does not move: the replica counts every commit the
// reset hands over as it counts a drained one (recover, loggedCommits.reset), so the next commit
// stands past every one of them, and a report the reset left unpaired - one made between the
// recover's take of the reports and the reset, whose commit the reset handed over - is of a commit
// before that one and is taken for none after it (madeReport.checkedAt). Here two frames land while
// the replica is held behind, the replica cannot place them, a report of the last of them stands
// unpaired across the reset, as that race leaves one: the next frame is spliced into the room's
// record at the reset, and the replica, started again from the fresh base, places a whole render
// of the room at that frame's commit and at the one after it.
func TestARecoveredReplicaPublishesNoCheckedRenderingAtACommitItIsNotOf(t *testing.T) {
	room := newRCRoom(t, stateOf(t, "# Title\n\nParagraph one of the room.\n\nParagraph two of the room.\n"))
	keystroke := func(what string, paragraph int, typed string) {
		t.Helper()
		update := room.edit(t, func(txn *crdt.Transaction, _ *crdt.Doc) {
			paragraphTextsOf(txn.GetXmlFragment(fragmentName))[paragraph].Insert(txn, 0, typed, nil)
		})
		if err := room.frame(t, what, update); err != nil {
			t.Fatalf("%s was answered %v, want it taken", what, err)
		}
	}
	keystroke("the frame the replica follows", 0, "a")
	room.held.Store(true)
	keystroke("the first frame the replica is held behind", 1, "b")
	keystroke("the second frame the replica is held behind", 0, "c")
	forceRecover(room.replica)
	room.release()
	if doc, _, _, _ := replicaHolds(room.replica); doc {
		t.Fatal("the replica placed the frames it was held behind, so it did not recover and nothing here is tested")
	}
	at, counted := room.replica.logged.now()
	record := room.replica.applies().checked.Load()
	if !counted || record == nil || record.rendering == nil || record.at != at {
		t.Fatalf("across the reset the room stands at commit %d (counted %t) with its record %+v, want the second held frame's record standing there", at, counted, record)
	}
	whole, err := renderDocument(room.live)
	if err != nil {
		t.Fatal(err)
	}
	// The replica publishes nothing until the fresh base starts it, so the record alone is held to
	// the room here.
	room.held.Store(true)
	room.requireRenderedAs(t, "across the reset", whole)
	room.held.Store(false)
	applies := room.replica.applies()
	applies.mu.Lock()
	applies.unpaired = append(applies.unpaired, madeReport{origin: room.origin, checked: record})
	applies.mu.Unlock()
	keystroke("the frame after the reset", 1, "d")
	keystroke("the frame after that", 0, "e")
}
