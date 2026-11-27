package docs

// The lost commit end to end, with what an editor shows of it: a browser's edit typed after
// another's edit that the room relayed and then lost parks, is answered not applied, and is kept
// as unsaved; it clears once the commit that lands it is stored, and stays while nothing lands it
// (parked_resync.go). The pages keep the editor's row bookkeeping as pending-sync.ts keeps it:
// each local edit is a row; every SyncStep2 or Update the page writes is queued, and each
// SyncStatus pops one; an applied Update clears the row with its exact bytes (clearUpdate), an
// applied SyncStep2 clears every row recorded before it was written (clearThrough), and a
// not-applied answer clears nothing. A page rebuilt after a refusal is a fresh document from the
// room with no row of its own yet, which replays each row the browser saved as an Update of its
// own and records it again, as the dashboard's restore does (pending-sync.ts, admit).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/testwait"
)

type lcRow struct {
	ordinal int
	update  []byte
}

type lcSent struct {
	step2   bool
	through int
	update  []byte
	what    string
}

type lcPage struct {
	name       string
	artifactID string
	conn       *gws.Conn
	doc        *crdt.Doc
	start      time.Time
	writes     sync.Mutex

	mu          sync.Mutex
	nextOrdinal int
	rows        []lcRow
	sent        []lcSent
	synced      chan struct{}
	ended       chan struct{}
	lines       *[]string
	linesMu     *sync.Mutex
	// answered is each SyncStatus the page heard, as what it answered and whether it applied.
	answered []string
	// closed is the close frame the room ended the socket with, nil for any other end. It is
	// written before ended closes, and read after.
	closed *gws.CloseError
}

func (p *lcPage) logf(format string, args ...any) {
	p.linesMu.Lock()
	defer p.linesMu.Unlock()
	*p.lines = append(*p.lines, fmt.Sprintf("%7.1f ms %s: ", float64(time.Since(p.start).Microseconds())/1000, p.name)+fmt.Sprintf(format, args...))
}

func lcDial(t *testing.T, serverURL, artifactID, name string, prev *lcPage, start time.Time, lines *[]string, linesMu *sync.Mutex) *lcPage {
	t.Helper()
	var kept *crdt.Doc
	if prev != nil {
		kept = prev.doc
	}
	presented := ""
	if kept != nil {
		presented = strconv.FormatUint(uint64(kept.ClientID()), 10)
	}
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "=" + presented
	conn, _, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{name}})
	if err != nil {
		t.Fatal(err)
	}
	given, named, _, err := docstest.ReadGivenClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !named {
		t.Fatalf("the room gave %s's page no client id in the socket's first frame", name)
	}
	doc := kept
	if doc == nil {
		doc = crdt.New(crdt.WithClientID(given))
	}
	p := &lcPage{name: name, artifactID: artifactID, conn: conn, doc: doc, start: start,
		synced: make(chan struct{}, 1), ended: make(chan struct{}), lines: lines, linesMu: linesMu}
	if prev != nil {
		// The editor's rows outlive the socket (pending-sync.ts keeps one ordinal count per document).
		prev.mu.Lock()
		p.rows, p.nextOrdinal = append([]lcRow(nil), prev.rows...), prev.nextOrdinal
		prev.mu.Unlock()
	}
	p.logf("connected, given client %d (presented %q), %d row(s) saved", given, presented, len(p.rows))
	go p.read()
	t.Cleanup(func() { _ = conn.Close(); <-p.ended })
	p.write(ygsync.EncodeSyncStep1(doc), nil)
	return p
}

// write sends a sync message; queued says how the page counts it (nil for a SyncStep1, which no
// SyncStatus answers).
func (p *lcPage) write(message []byte, queued *lcSent) {
	frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarString(p.artifactID)
		e.WriteVarUint(0)
		e.WriteRaw(message)
	})
	p.writes.Lock()
	defer p.writes.Unlock()
	if queued != nil {
		p.mu.Lock()
		p.sent = append(p.sent, *queued)
		p.mu.Unlock()
	}
	if err := p.conn.WriteMessage(gws.BinaryMessage, frame); err != nil {
		p.logf("write failed: %v", err)
	}
}

// typeAt types text into the first paragraph after offset characters, records the edit as a row
// and sends it as an Update frame.
func (p *lcPage) typeAt(offset int, text string) {
	update := docstest.Transact(p.doc, func(txn *crdt.Transaction) {
		paragraphTextsOf(txn.GetXmlFragment(fragmentName))[0].Insert(txn, offset, text, nil)
	})
	p.mu.Lock()
	p.nextOrdinal++
	p.rows = append(p.rows, lcRow{ordinal: p.nextOrdinal, update: update})
	p.mu.Unlock()
	p.logf("types %q at %d (row %d, %d bytes)", text, offset, p.nextOrdinal, len(update))
	p.write(ygsync.EncodeUpdate(update), &lcSent{update: update, what: fmt.Sprintf("Update %q", text)})
}

func (p *lcPage) read() {
	defer close(p.ended)
	for {
		_, message, err := p.conn.ReadMessage()
		if err != nil {
			var closed *gws.CloseError
			if errors.As(err, &closed) {
				p.closed = closed
				p.logf("closed %d %s", closed.Code, closed.Text)
			} else {
				p.logf("ended: %v", err)
			}
			return
		}
		d := encoding.NewDecoder(message)
		if _, err := d.ReadVarString(); err != nil {
			return
		}
		kind, err := d.ReadVarUint()
		if err != nil {
			continue
		}
		switch kind {
		case 0:
			payload := d.RemainingBytes()
			sub, content, err := ygsync.ReadSyncMessage(payload)
			if err != nil {
				return
			}
			beforeText := lcText(p.doc)
			reply, err := ygsync.ApplySyncMessage(p.doc, payload, "remote")
			if err != nil {
				p.logf("apply failed: %v", err)
				return
			}
			switch sub {
			case ygsync.MsgSyncStep1:
				p.mu.Lock()
				through := p.nextOrdinal
				p.mu.Unlock()
				p.logf("server's SyncStep1; answers a SyncStep2 of %d bytes (covers rows through %d)", len(reply), through)
				p.write(reply, &lcSent{step2: true, through: through, what: "SyncStep2"})
			case ygsync.MsgSyncStep2:
				p.logf("server's SyncStep2 (%d bytes); text now %q", len(content), lcText(p.doc))
				select {
				case p.synced <- struct{}{}:
				default:
				}
			case ygsync.MsgUpdate:
				if after := lcText(p.doc); after != beforeText {
					p.logf("relayed %d bytes; text now %q", len(content), after)
				} else {
					p.logf("relayed %d bytes; text unchanged %q", len(content), after)
				}
			}
		case 8:
			flag, _ := d.ReadVarUint()
			p.mu.Lock()
			if len(p.sent) == 0 {
				p.mu.Unlock()
				p.logf("SyncStatus %d for no frame", flag)
				continue
			}
			s := p.sent[0]
			p.sent = p.sent[1:]
			if flag == 1 {
				kept := p.rows[:0]
				for _, row := range p.rows {
					if s.step2 && row.ordinal <= s.through || !s.step2 && string(row.update) == string(s.update) {
						continue
					}
					kept = append(kept, row)
				}
				p.rows = kept
			}
			rows := len(p.rows)
			p.answered = append(p.answered, fmt.Sprintf("%s applied=%t", s.what, flag == 1))
			p.mu.Unlock()
			p.logf("%s answered applied=%t; rows saved in the browser now %d", s.what, flag == 1, rows)
		}
	}
}

func (p *lcPage) rowsHeld() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.rows)
}

// savedRows is a copy of the rows the page's browser holds.
func (p *lcPage) savedRows() []lcRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]lcRow(nil), p.rows...)
}

// heard is a copy of each SyncStatus the page heard, in order (answered).
func (p *lcPage) heard() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.answered...)
}

// replay is the restore of a rebuilt page: each of rows, saved by an earlier page of the browser,
// is applied to the page's document, recorded again as a row of this page and sent as an Update
// of its own. A row that waits on content the room lacks parks in the document as it does in the
// room, and is sent all the same: the room holds it back and answers it not applied.
func (p *lcPage) replay(rows []lcRow) {
	for _, saved := range rows {
		if err := crdt.ApplyUpdateV1(p.doc, saved.update, "restored"); err != nil {
			p.logf("apply the saved row %d failed: %v", saved.ordinal, err)
			continue
		}
		p.mu.Lock()
		p.nextOrdinal++
		p.rows = append(p.rows, lcRow{ordinal: p.nextOrdinal, update: saved.update})
		ordinal := p.nextOrdinal
		p.mu.Unlock()
		p.logf("replays the saved row %d as row %d (%d bytes)", saved.ordinal, ordinal, len(saved.update))
		p.write(ygsync.EncodeUpdate(saved.update), &lcSent{update: saved.update, what: "replayed row"})
	}
}

// lcText is the first paragraph's text of doc, a page's or the room's. It reads a copy of doc's
// state: the page's reader and the room's apply write to their documents while the drive reads
// them, and ygo walks a type's children under no lock of its own (YXmlFragment.Children), so the
// state is encoded under the document's lock and read where nothing else writes.
func lcText(doc *crdt.Doc) string {
	copied := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(copied, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		return ""
	}
	texts := paragraphTextsOf(copied.GetXmlFragment(fragmentName))
	if len(texts) == 0 {
		return ""
	}
	return texts[0].ToString()
}

func lcWait(t *testing.T, what string, within time.Duration, done func() bool) bool {
	t.Helper()
	if testwait.Holds(within, testwait.Poll, done) {
		return true
	}
	t.Logf("TIMEOUT waiting for %s", what)
	return false
}

// Alice's edit is integrated, relayed to bob and lost before it is stored: the database refuses
// its append, the room fails, and alice is answered not applied and keeps her row. Bob, offline,
// types right after her lost text, and his browser saves the row. His reconnect's SyncStep2
// carries her lost structs beside his own, so the room refuses it whole, visibly: it is answered
// not applied and the socket is closed 4413 DOC_CLIENT_ID_REFUSED, and nothing of it lands. His
// row stays, and his editor rebuilds as the dashboard does, a fresh page from the room that replays
// his saved row as an Update of its own. The row names alice's lost text, so it parks, is answered
// not applied and stays unsaved in his browser. When alice comes back and resends her row, the
// commit that lands it lands bob's row too, and once it is stored the room resyncs bob, whose
// answer clears his row: the store holds both people's text, and nothing of either was lost or
// dropped in silence. While she never comes back, bob's row is neither stored nor cleared, and
// nothing answers it applied: it stays, and so does the notice, truthfully, across his own
// reconnect.
func TestAnEditParkedBehindALostCommitIsUnsavedUntilTheCommitThatLandsItIsStored(t *testing.T) {
	for _, aliceReturns := range []bool{true, false} {
		t.Run(map[bool]string{true: "alice resends", false: "alice never returns"}[aliceReturns], func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = 2 * time.Second
			var lose atomic.Bool
			// The append lost is that of a commit the room has relayed. The room hands each append
			// on as it takes the commit, with no linger before the store, so an append refused at
			// once can fail the room, and close bob, before the relay of alice's edit reaches him.
			// The hook holds the append it loses until bob holds that edit (relayed), as a database
			// that refuses the transaction at its commit would.
			relayed := make(chan struct{})
			service.beforeRoomAppend = func(string) error {
				if lose.CompareAndSwap(true, false) {
					select {
					case <-relayed:
					case <-time.After(10 * time.Second):
					}
					return errors.New("scratch: the database refused the append")
				}
				return nil
			}
			seedServiceText(t, service, artifactID, "# Title\n\nParagraph.\n")
			serverURL := newTestServer(t, service).URL
			var lines []string
			var linesMu sync.Mutex
			t.Cleanup(func() {
				if t.Failed() {
					for _, line := range lines {
						t.Log(line)
					}
				}
			})
			start := time.Now()
			note := func(format string, args ...any) {
				linesMu.Lock()
				defer linesMu.Unlock()
				lines = append(lines, fmt.Sprintf("%7.1f ms drive: ", float64(time.Since(start).Microseconds())/1000)+fmt.Sprintf(format, args...))
			}
			room := func() string {
				live := service.srv.GetDoc(artifactID)
				if live == nil {
					return "(no room)"
				}
				return fmt.Sprintf("text %q, parks %d", lcText(live), live.PendingStats().Items)
			}
			stored := func(client crdt.ClientID) uint64 {
				loaded, err := service.persistence.Load(t.Context(), artifactID)
				if err != nil {
					t.Fatal(err)
				}
				return copyOf(t, loaded.Update).StateVector().Clock(client)
			}

			alice := lcDial(t, serverURL, artifactID, "alice", nil, start, &lines, &linesMu)
			bob := lcDial(t, serverURL, artifactID, "bob", nil, start, &lines, &linesMu)
			<-alice.synced
			<-bob.synced
			aliceID, bobID := alice.doc.ClientID(), bob.doc.ClientID()

			lose.Store(true)
			alice.typeAt(4, "L")
			heldL := lcWait(t, "bob to be relayed alice's L", 5*time.Second, func() bool { return strings.Contains(lcText(bob.doc), "L") })
			close(relayed)
			if !heldL {
				t.Fatal("bob was never relayed alice's L while its append waited, so the commit lost is not one the room relayed")
			}
			lcWait(t, "both sockets to end", 10*time.Second, func() bool {
				select {
				case <-alice.ended:
				default:
					return false
				}
				select {
				case <-bob.ended:
					return true
				default:
					return false
				}
			})
			note("after the lost append: room %s; stored holds alice to clock %d of %d", room(), stored(aliceID), alice.doc.StateVector().Clock(aliceID))
			if rows := alice.rowsHeld(); rows != 1 {
				t.Fatalf("alice holds %d rows after her append was lost, want her edit kept", rows)
			}

			// Bob, offline with the document he kept (holding alice's L), types right after her lost
			// text; his browser saves the row, and the frame goes nowhere.
			at := strings.Index(lcText(bob.doc), "L") + 1
			bob.typeAt(at, "D")
			// He reconnects first: his SyncStep2 answers the room's SyncStep1 with alice's L and his
			// D, which the room lacks.
			bob2 := lcDial(t, serverURL, artifactID, "bob", bob, start, &lines, &linesMu)
			if !lcWait(t, "bob's reconnect to be refused", 10*time.Second, func() bool {
				select {
				case <-bob2.ended:
					return true
				default:
					return false
				}
			}) {
				t.Fatal("bob's reconnect, whose SyncStep2 carries alice's lost edit, stayed connected")
			}
			note("bob's reconnect was refused: bob holds %d row(s); room %s", bob2.rowsHeld(), room())
			if closed := bob2.closed; closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentClientIdRefusedCloseReason {
				t.Fatalf("bob's reconnect ended with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentClientIdRefusedCloseReason)
			}
			if heard, want := bob2.heard(), []string{"SyncStep2 applied=false"}; !slices.Equal(heard, want) {
				t.Fatalf("bob's reconnect heard %q, want %q", heard, want)
			}
			if live := service.srv.GetDoc(artifactID); live != nil && strings.ContainsAny(lcText(live), "LD") {
				t.Fatalf("the room reads %q after bob's refused SyncStep2: something of it landed", lcText(live))
			}
			if clock := stored(bobID); clock != 0 {
				t.Fatalf("the store holds bob to clock %d after his refused SyncStep2, want 0", clock)
			}
			if rows := bob2.rowsHeld(); rows != 1 {
				t.Fatalf("bob holds %d rows after his refused SyncStep2, want his edit kept", rows)
			}

			// The rebuild: a fresh page from the room, which replays bob's saved row.
			bob3 := lcDial(t, serverURL, artifactID, "bob", nil, start, &lines, &linesMu)
			<-bob3.synced
			lcWait(t, "bob's rebuilt page's SyncStep2 to be answered", 5*time.Second, func() bool {
				bob3.mu.Lock()
				defer bob3.mu.Unlock()
				return len(bob3.sent) == 0
			})
			bob3.replay(bob2.savedRows())
			lcWait(t, "bob's replayed row to be answered", 5*time.Second, func() bool {
				bob3.mu.Lock()
				defer bob3.mu.Unlock()
				return len(bob3.sent) == 0
			})
			note("bob's rebuilt page replayed his row: it reads %q and holds %d row(s); room %s; stored holds bob to clock %d of %d",
				lcText(bob3.doc), bob3.rowsHeld(), room(), stored(bobID), bob2.doc.StateVector().Clock(bobID))
			if heard := bob3.heard(); len(heard) == 0 || heard[len(heard)-1] != "replayed row applied=false" {
				t.Fatalf("bob's rebuilt page heard %q, want its replayed row, which waits on alice's lost text, answered not applied", heard)
			}
			if rows := bob3.rowsHeld(); rows != 1 {
				t.Fatalf("bob's rebuilt page holds %d rows, want his edit typed after alice's lost text kept, answered not applied", rows)
			}
			if parked := service.srv.GetDoc(artifactID).PendingStats(); parked.Items == 0 {
				t.Fatalf("the room parks nothing (%+v), want bob's replayed row waiting on alice's lost text", parked)
			}

			if aliceReturns {
				alice2 := lcDial(t, serverURL, artifactID, "alice", alice, start, &lines, &linesMu)
				<-alice2.synced
				lcWait(t, "alice's SyncStep2 to be answered", 10*time.Second, func() bool {
					alice2.mu.Lock()
					defer alice2.mu.Unlock()
					return len(alice2.sent) == 0
				})
				landed := time.Now()
				note("alice resent: alice holds %d row(s); room %s; stored holds alice to %d, bob to %d of %d",
					alice2.rowsHeld(), room(), stored(aliceID), stored(bobID), bob2.doc.StateVector().Clock(bobID))
				if rows := alice2.rowsHeld(); rows != 0 {
					t.Fatalf("alice holds %d rows after her resend was answered, want her edit stored and cleared", rows)
				}
				if !lcWait(t, "bob's rows to clear", 5*time.Second, func() bool { return bob3.rowsHeld() == 0 }) {
					t.Fatal("bob's edit landed with alice's resent row and is stored, and his browser still holds it as unsaved")
				}
				note("%.1f ms after alice's resend was answered: bob holds %d row(s)", float64(time.Since(landed).Microseconds())/1000, bob3.rowsHeld())
				if clock, wrote := stored(aliceID), alice.doc.StateVector().Clock(aliceID); clock != wrote {
					t.Fatalf("the store holds alice to clock %d of the %d she wrote, and her row cleared", clock, wrote)
				}
				if clock, wrote := stored(bobID), bob2.doc.StateVector().Clock(bobID); clock != wrote {
					t.Fatalf("the store holds bob to clock %d of the %d he wrote, and his row cleared", clock, wrote)
				}
				if text := lcText(service.srv.GetDoc(artifactID)); !strings.Contains(text, "LD") {
					t.Fatalf("the room reads %q, want alice's L and bob's D after it", text)
				}
			} else {
				time.Sleep(time.Second)
				note("alice never returns: bob holds %d row(s); room %s; stored holds bob to %d of %d",
					bob3.rowsHeld(), room(), stored(bobID), bob2.doc.StateVector().Clock(bobID))
				if rows := bob3.rowsHeld(); rows != 1 {
					t.Fatalf("bob holds %d rows while his edit waits on alice's lost text, want it kept", rows)
				}
				if clock := stored(bobID); clock != 0 {
					t.Fatalf("the store holds bob to clock %d while his edit waits on alice's lost text, want 0", clock)
				}
				for _, heard := range bob3.heard() {
					if heard == "replayed row applied=true" {
						t.Fatalf("bob's replayed row was answered applied while it waits on alice's lost text: %q", bob3.heard())
					}
				}
			}
			// Bob's next socket: his SyncStep2 carries his edit again where it has not landed, and
			// the room answers it as it answered the edit.
			_ = bob3.conn.Close()
			<-bob3.ended
			bob4 := lcDial(t, serverURL, artifactID, "bob", bob3, start, &lines, &linesMu)
			<-bob4.synced
			lcWait(t, "bob's next SyncStep2 to be answered", 5*time.Second, func() bool {
				bob4.mu.Lock()
				defer bob4.mu.Unlock()
				return len(bob4.sent) == 0
			})
			note("after bob's next reconnect: bob holds %d row(s); room %s", bob4.rowsHeld(), room())
			if want := map[bool]int{true: 0, false: 1}[aliceReturns]; bob4.rowsHeld() != want {
				t.Fatalf("after bob's reconnect he holds %d rows, want %d", bob4.rowsHeld(), want)
			}
		})
	}
}

// A sender is listed while its frames wait, resynced once for the commit that lands them all and
// only once that commit is stored, and dropped as its connection ends or as the room's queue
// empties with its frames still waiting, so what the room keeps is bounded by what its queue
// holds and the browsers connected to it.
func TestParkedSendersAreResyncedOnceForTheStoredCommitThatLandsThem(t *testing.T) {
	var applies roomApplies
	room := crdt.New(crdt.WithClientID(1))
	connection, end := context.WithCancel(context.Background())
	applies.parked.note(connection, "bob", crdt.StateVector{7: 3})
	applies.parked.note(context.Background(), "carol", crdt.StateVector{8: 2})
	applies.parked.note(connection, "bob", crdt.StateVector{7: 2, 9: 1})
	if got := applies.parked.held.Load(); got != 2 {
		t.Fatalf("the room lists %d senders, want bob and carol", got)
	}
	held := func(clocks crdt.StateVector) *crdt.Doc {
		doc := crdt.New(crdt.WithClientID(99))
		for client, clock := range clocks {
			update := newV1Frame(1).group(1, uint64(client), 0).gc(clock).noDeletes()
			if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatal(err)
			}
		}
		return doc
	}
	applies.coverParked(held(crdt.StateVector{7: 3}), 4)
	if landed := applies.landedParked(10); len(landed) != 0 {
		t.Fatalf("senders %v landed while bob still waits on client 9 and carol on client 8", landed)
	}
	applies.coverParked(held(crdt.StateVector{7: 3, 9: 1}), 5)
	applies.coverParked(held(crdt.StateVector{7: 4, 9: 2}), 6)
	if landed := applies.landedParked(4); len(landed) != 0 {
		t.Fatalf("senders %v were resynced before the store held the commit that landed them", landed)
	}
	if landed := applies.landedParked(5); len(landed) != 1 || landed[0].origin != "bob" {
		t.Fatalf("the store through commit 5 resynced %v, want bob alone, once", landed)
	}
	if landed := applies.landedParked(6); len(landed) != 0 {
		t.Fatalf("a later store resynced %v again", landed)
	}
	applies.parked.note(connection, "bob", crdt.StateVector{7: 9})
	end()
	waitFor(t, time.Second, "bob's connection's end to drop him", func() bool {
		applies.parked.mu.Lock()
		defer applies.parked.mu.Unlock()
		_, listed := applies.parked.waits["bob"]
		return !listed
	})
	applies.noteParked(ygws.PeerUpdateInfo{Doc: room}, crdt.PendingStats{}, crdt.PendingStats{})
	if got := applies.parked.held.Load(); got != 0 {
		t.Fatalf("the room lists %d senders once its queue is empty and carol's frames never landed, want none", got)
	}
}
