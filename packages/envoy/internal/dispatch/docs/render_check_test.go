package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// rcRoom is a room as a load makes one (keepReplica, chargeLoadedRoom), with its update observer
// draining each commit into the replica inside the apply, as observeRoomUpdates does, and its
// after-transaction drain taking a commit that only parked, as the load's does (onLoadDocument),
// unless held: a held room's replica takes nothing until the test releases it, which is the window
// between a commit and the observer that takes it: after a commit of the server's own, the room's
// apply check has no rendering standing at the commit an apply starts from (renderCheck.base), and
// after a browser's it has only its own record of that apply (roomApplies.checked).
type rcRoom struct {
	service *Service
	live    *crdt.Doc
	replica *renderedReplica
	held    atomic.Bool
	origin  *int
	// tookPast counts the frames the room took past the bound and its creep (frame), which only
	// a room without its check takes.
	tookPast int
}

func newRCRoom(t *testing.T, state []byte) *rcRoom {
	t.Helper()
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
		t.Fatal(err)
	}
	service := calibratedService()
	service.budget.limit = roomBudgetBytes
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
	replica := service.keepReplica(live, log, liveRendering(t, live), nil)
	service.chargeLoadedRoom("room", replica, int64(len(crdt.EncodeStateAsUpdateV1(live, nil))))
	room := &rcRoom{service: service, live: live, replica: replica, origin: new(int)}
	live.OnUpdate(func([]byte, any) {
		if !room.held.Load() {
			replica.observe("room")
		}
	})
	live.OnAfterTransaction(func(txn *crdt.Transaction) {
		if !room.held.Load() && log.Pending() > 0 && len(txn.ChangedTypes()) == 0 {
			replica.observe("room")
		}
	})
	return room
}

// weigh hands update to the hook as one browser's, with the room's own apply.
func (r *rcRoom) weigh(update []byte) error {
	info := websocket.PeerUpdateInfo{Room: "room", Doc: r.live, Update: update, Origin: r.origin}
	return r.service.weighPeerUpdate(info, func() error { return crdt.ApplyUpdateV1(r.live, update, r.origin) })
}

// release lets the replica take what the room committed while it was held.
func (r *rcRoom) release() {
	r.held.Store(false)
	r.replica.observe("room")
}

// frame weighs update and holds the invariant the check exists for: a refused update leaves the
// room byte for byte as it was; an admitted one leaves the room rendering within
// maxPeerRenderingBytes, or no larger than the room rendered before it. It holds the one the
// check's reuse rests on as well: an admitted update leaves every rendering the room places at its
// commit a whole render of the room there (requireRenderedAs), and after every frame, admitted or
// refused, the replica's document is the room's (requireReplicaIsRoom). It reports the hook's
// answer. what names the frame and the seed it came from in every failure.
func (r *rcRoom) frame(t *testing.T, what string, update []byte) error {
	t.Helper()
	before, beforeErr := renderDocument(r.live)
	state := crdt.EncodeStateAsUpdateV1(r.live, nil)
	err := r.weigh(update)
	if err != nil {
		if !bytes.Equal(crdt.EncodeStateAsUpdateV1(r.live, nil), state) {
			t.Fatalf("%s: the hook answered %v and the room changed", what, err)
		}
		// A refusal for a rendering past the line is held to what a whole render of the room with
		// the update applied writes: the check's splice must answer what that render answers. The
		// room is cloned, its pending queue with it, and the update is applied with any drain of
		// that queue taken back, since the check refused the update before any drain.
		var checked *renderRefusal
		if errors.As(err, &checked) && checked.cause == nil && !checked.unsure {
			copied := r.live.Clone(documentCopyOptions(maxUpdateItems)...)
			first := true
			copied.SetApplyCheck(func(any, *crdt.ApplyView) error {
				if first {
					first = false
					return nil
				}
				return errors.New("the drain is not the update's")
			})
			if applyErr := crdt.ApplyUpdateV1(copied, update, nil); applyErr != nil && !errors.Is(applyErr, crdt.ErrApplyCheckDrain) {
				t.Fatal(applyErr)
			}
			if rendered, renderErr := renderDocument(copied); renderErr != nil || int64(len(rendered)) != checked.after {
				t.Errorf("%s: the check refused the update as leaving %d bytes, and the room with it applied renders %d bytes (%v)",
					what, checked.after, len(rendered), renderErr)
			}
		}
		if whole, renderErr := renderDocument(r.live); renderErr == nil {
			r.requireReplicaIsRoom(t, what, whole)
		}
		return err
	}
	after, afterErr := renderDocument(r.live)
	if afterErr == nil {
		r.requireRenderedAs(t, what, after)
	}
	if afterErr == nil && len(after) > maxPeerRenderingBytes && (beforeErr != nil || len(after) > len(before)) {
		// Not fatal: the room goes on taking frames, so a run without the check says how many it
		// took past the line (tookPast).
		r.tookPast++
		t.Errorf("%s: the room took a %d-byte update that left it rendering %d bytes, past the %d a browser may grow it to, from %d (%v)",
			what, len(update), len(after), maxPeerRenderingBytes, len(before), beforeErr)
	}
	if !r.held.Load() {
		if size, measured := r.replica.size(); afterErr == nil && (!measured || size != int64(len(after))) {
			t.Fatalf("%s: the room publishes %d bytes (measured %t) and renders %d", what, size, measured, len(after))
		}
	}
	return nil
}

// requireRenderedAs requires every rendering the room places at the commit position it stands at
// to be whole, the room's markdown as a render of its whole state writes it there: the check's own
// record of what the last browser's apply it passed left (roomApplies.checked), which the next
// apply's check splices into and the replica takes for that commit (takeChecked), and the
// replica's published rendering (roomWeighing.rendered), which the next apply is weighed against
// and a write's read of the room takes as the rendering of the state it copies (replicaRead). A
// splice that misses or misplaces a change can leave the rendering the length a whole render
// writes, which the published size cannot show, so the markdown itself is compared. A held room's
// replica stands behind the room's position until it is released, so only the check's record is
// held to it there; an unheld room's replica must stand there, with its rendering once it holds a
// copy of the room, which a room whose load handed it none takes at the room's first commit
// (keepReplica), and with the room's own document (requireReplicaIsRoom).
func (r *rcRoom) requireRenderedAs(t *testing.T, what, whole string) {
	t.Helper()
	at, counted := r.replica.logged.now()
	if !counted {
		t.Fatalf("%s: the room's commit position cannot be read", what)
	}
	published := r.replica.rendered.Load()
	if !r.held.Load() && (published == nil || published.at != at || (published.rendering == nil && r.replica.doc != nil)) {
		t.Fatalf("%s: the room stands at commit %d and its replica publishes no rendering there (%+v)", what, at, published)
	}
	for _, placed := range []struct {
		name     string
		rendered *roomRendered
	}{
		{"the check's record of the room's rendering", r.replica.applies().checked.Load()},
		{"the replica's published rendering", published},
	} {
		if placed.rendered == nil || placed.rendered.at != at || placed.rendered.rendering == nil {
			continue
		}
		if held := placed.rendered.rendering.markdown; held != whole {
			t.Fatalf("%s: %s at commit %d is not a whole render of the room there: %s", what, placed.name, at, renderingDifference(held, whole))
		}
	}
	r.requireReplicaIsRoom(t, what, whole)
}

// requireReplicaIsRoom requires the replica's document, which the replica renders every later
// commit on and the room's reads and settlements walk, to be the room's, whatever the room parked,
// refused or discarded: the same integrated structs (the state vector) and the same whole render,
// whole being the room's. The pending queues are not compared: what the room parks is no part of
// its document until it lands (leaveParkedOut), and the replica applies only what each commit
// integrated, so it parks nothing the room parks and holds it once the commit that lands it
// carries it. A held room's replica stands behind the room until it is released, and a replica in
// its recover state holds no document, so neither is compared.
func (r *rcRoom) requireReplicaIsRoom(t *testing.T, what, whole string) {
	t.Helper()
	if r.held.Load() {
		return
	}
	r.replica.mu.Lock()
	defer r.replica.mu.Unlock()
	doc := r.replica.doc
	if doc == nil {
		return
	}
	if got, want := doc.StateVector(), r.live.StateVector(); !maps.Equal(got, want) {
		t.Fatalf("%s: the replica's document holds the state vector %v and the room %v", what, got, want)
	}
	if rendered, err := renderDocument(doc); err != nil || rendered != whole {
		t.Fatalf("%s: the replica's document renders %d bytes (%v) where the room renders %d: %s",
			what, len(rendered), err, len(whole), renderingDifference(rendered, whole))
	}
}

// renderingDifference says where held, a rendering the room placed, first leaves whole, the whole
// render of the same state, with a few bytes of each from there: two renderings of a document near
// the bound are too long to print whole.
func renderingDifference(held, whole string) string {
	at := 0
	for at < len(held) && at < len(whole) && held[at] == whole[at] {
		at++
	}
	excerpt := func(markdown string) string {
		return markdown[at:min(len(markdown), at+80)]
	}
	return fmt.Sprintf("%d bytes against %d, first differing at byte %d: %q against %q", len(held), len(whole), at, excerpt(held), excerpt(whole))
}

// requireClosed requires err to be the hook's close with code and reason.
func requireClosed(t *testing.T, what string, err error, code int, reason string) {
	t.Helper()
	var refusal *websocket.PeerClose
	if !errors.As(err, &refusal) || refusal.Code != code || refusal.Reason != reason {
		t.Fatalf("%s: the hook answered %v, want %d %s", what, err, code, reason)
	}
}

// browserCopy is a copy of the room as a browser holds it, built with options.
func (r *rcRoom) browserCopy(t *testing.T, options ...crdt.DocOption) *crdt.Doc {
	t.Helper()
	browser := newDocumentCopy(options...)
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(r.live, nil), nil); err != nil {
		t.Fatal(err)
	}
	return browser
}

// edit is the update a browser sends for change, made on its own copy of the room: its
// transaction's own update, as an editor sends it, so it carries nothing the copy took in before
// the change - which, where the room holds structs parked behind a gap that is filled, includes
// those structs, since the copy's decode of the room's state lands them.
func (r *rcRoom) edit(t *testing.T, change func(txn *crdt.Transaction, browser *crdt.Doc)) []byte {
	t.Helper()
	browser := r.browserCopy(t)
	before := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { change(txn, browser) })
	return crdt.EncodeStateAsUpdateV1(browser, before)
}

// nextEdit is the update that follows a refusal in each fixed row: a keystroke where the room
// renders within the creep cap, and a delete of two characters where it stands past it, where
// the byte rule leaves an update no content (peerApplyAllowance). Either is weighed afresh and
// must land.
func (r *rcRoom) nextEdit(t *testing.T, what string) {
	t.Helper()
	rendering, _ := renderDocument(r.live)
	var update []byte
	if len(rendering) >= maxPeerRenderingBytes {
		update = r.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
			allTexts(txn.GetXmlFragment(fragmentName))[0].Delete(txn, 0, 2)
		})
	} else {
		update = r.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
			allTexts(txn.GetXmlFragment(fragmentName))[0].Insert(txn, 0, "y", nil)
		})
	}
	if err := r.frame(t, what+", the edit after it", update); err != nil {
		t.Fatalf("%s: the edit after the refusal was answered %v, want it applied", what, err)
	}
}

// rcProse is a paragraph of about n bytes of words.
func rcProse(n int) string {
	return strings.TrimSpace(strings.Repeat("word ", n/5+1)[:n])
}

// rcShape is one of the five shapes measured on this branch before the check, whose few bytes
// grew the room's rendering past the bound and its creep, each as a room and the frame that did.
type rcShape struct {
	name  string
	room  func(t *testing.T) *rcRoom
	frame func(t *testing.T, room *rcRoom) []byte
}

func rcShapes() []rcShape {
	return []rcShape{
		{
			// 35 bytes, +800,024 before the check: every line of the code block is indented by
			// the marker's width.
			name: "the start number 999,999,999 of an ordered list holding a 100,000-line code block",
			room: func(t *testing.T) *rcRoom {
				return newRCRoom(t, stateOf(t, "1. x\n\n   ```\n"+strings.Repeat("   l\n", 100_000)+"   ```\n"))
			},
			frame: func(t *testing.T, room *rcRoom) []byte {
				return room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
					list := txn.GetXmlFragment(fragmentName).Children()[0].(*crdt.YXmlElement)
					list.SetAttributeValue(txn, "order", int64(999_999_999))
				})
			},
		},
		{
			// 7 bytes, +2,799,998 before the check, into a room already past the creep cap: every
			// character the code span held literal takes an escape once the mark is gone.
			name: "a delete of an inline code mark's two format items over 560,000 pairs of ~a",
			room: func(t *testing.T) *rcRoom {
				base := newDocumentCopy()
				if err := crdt.ApplyUpdateV1(base, stateOf(t, "x\n"), nil); err != nil {
					t.Fatal(err)
				}
				written := firstText(base.GetXmlFragment(fragmentName))
				base.Transact(func(txn *crdt.Transaction) { written.Insert(txn, 0, strings.Repeat("~a", 560_000), nil) })
				marker := newDocumentCopy(crdt.WithClientID(777))
				if err := crdt.ApplyUpdateV1(marker, crdt.EncodeStateAsUpdateV1(base, nil), nil); err != nil {
					t.Fatal(err)
				}
				text := firstText(marker.GetXmlFragment(fragmentName))
				marker.Transact(func(txn *crdt.Transaction) {
					text.Format(txn, 0, text.Len(), crdt.Attributes{"inlineCode": map[string]any{}})
				})
				if err := crdt.ApplyUpdateV1(base, crdt.EncodeStateAsUpdateV1(marker, base.StateVector()), nil); err != nil {
					t.Fatal(err)
				}
				return newRCRoom(t, crdt.EncodeStateAsUpdateV1(base, nil))
			},
			frame: func(t *testing.T, room *rcRoom) []byte {
				return newV1Frame(0).uint(1).uint(777).uint(1).uint(0).uint(room.live.StateVector().Clock(777)).done()
			},
		},
		{
			// 43 bytes, +72,027 before the check, into a room just past the bound: the span adds
			// an empty cell to every row.
			name: "a rowspan and a colspan on the header cell of a table of 8,000 rows",
			room: func(t *testing.T) *rcRoom {
				return newRCRoom(t, stateOf(t, rcProse(1_000_000)+"\n\n| h |\n| - |\n"+strings.Repeat("| a |\n", 8_000)))
			},
			frame: func(t *testing.T, room *rcRoom) []byte {
				return room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
					table := txn.GetXmlFragment(fragmentName).Children()[1].(*crdt.YXmlElement)
					header := table.Children()[0].(*crdt.YXmlElement).Children()[0].(*crdt.YXmlElement)
					header.SetAttributeValue(txn, "rowspan", float64(8_001))
					header.SetAttributeValue(txn, "colspan", float64(3))
				})
			},
		},
		{
			// Escapes render up to 3.6 times the characters they escape: 8,000 bytes of ~a, inside
			// the 8 KiB the byte rule lets one update carry at the bound, render about 28,000.
			name: "8,000 bytes of escapable characters typed into a room past the bound",
			room: func(t *testing.T) *rcRoom {
				return newRCRoom(t, stateOf(t, rcProse(1_100_000)+"\n\nsecond paragraph\n"))
			},
			frame: func(t *testing.T, room *rcRoom) []byte {
				return room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
					paragraphTextsOf(txn.GetXmlFragment(fragmentName))[1].Insert(txn, 0, strings.Repeat("~a", 4_000), nil)
				})
			},
		},
		{
			// A newline typed into a code block nested in quotes renders the whole prefix again:
			// about 200 bytes a newline at 98 quotes deep.
			name: "5,000 newlines typed into a code block nested 98 quotes deep",
			room: func(t *testing.T) *rcRoom {
				prefix := strings.Repeat("> ", 98)
				return newRCRoom(t, stateOf(t, rcProse(300_000)+"\n\n"+prefix+"```\n"+prefix+"x\n"+prefix+"```\n"))
			},
			frame: func(t *testing.T, room *rcRoom) []byte {
				return room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
					allTexts(txn.GetXmlFragment(fragmentName))[1].Insert(txn, 0, strings.Repeat("\n", 5_000), nil)
				})
			},
		},
	}
}

// Every admitted frame leaves the room's rendering within the document bound and its creep, or no
// larger than it found it, over the five shapes measured on this branch, each a fixed row, and over
// generated documents and frames: lists of every kind with large start numbers, quotes, code
// blocks, tables, marks with inline code, escapable characters and hard breaks, under frames that
// insert, delete across format items, format and unformat, set start numbers, spans and levels,
// build deep nesting of their own and type newlines - with a write of the server's own landing
// before some frames while the replica has not taken it yet, the window where the check knows no
// rendering standing at the commit the frame starts from (renderCheck.base), with the replica
// held behind the room across runs of frames, so that one drain hands it several frames' commits,
// each with the rendering its check made (takeChecked), and with some frames sent behind a gap,
// parked until a later frame fills it (gapFrames), whose drain the room lands or takes back. After
// every admitted frame each rendering the room places at its commit is a whole render of the room
// there (requireRenderedAs), and after every frame the replica's document is the room's
// (requireReplicaIsRoom).
//
// The seed is printed and named in every failure; DISPATCH_RENDER_CHECK_SEED runs one again.
func TestEveryAdmittedFrameLeavesTheRenderingWithinTheBound(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	for _, shape := range rcShapes() {
		t.Run("fixed/"+shape.name, func(t *testing.T) {
			room := shape.room(t)
			frame := shape.frame(t, room)
			err := room.frame(t, shape.name, frame)
			requireClosed(t, shape.name, err, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
			t.Logf("%s: a %d-byte frame was refused %v", shape.name, len(frame), err)
			room.nextEdit(t, shape.name)
		})
	}
	seed := uint64(time.Now().UnixNano())
	if given := os.Getenv("DISPATCH_RENDER_CHECK_SEED"); given != "" {
		parsed, err := strconv.ParseUint(given, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seed = parsed
	}
	t.Logf("seed %d (DISPATCH_RENDER_CHECK_SEED=%d runs it again)", seed, seed)
	const documents, frames = 9, 24
	var admitted, refused, pastBound, unsure, serverWrites, tookPast, gaps, takenBack int
	for document := range documents {
		gen := rcGen{rng: rand.New(rand.NewPCG(seed, uint64(document)))}
		room := newRCRoom(t, gen.document(t, document%3))
		// The fill a frame sent behind a gap waits on (gapFrames), that frame's client and the
		// clock its change starts at, and whether the change parked.
		var fill []byte
		var parkedClient crdt.ClientID
		var parkedFrom uint64
		var changeParked bool
		for index := range frames {
			what := fmt.Sprintf("seed %d, document %d, frame %d", seed, document, index)
			switch gen.rng.IntN(5) {
			case 0:
				room.held.Store(true)
				gen.serverWrite(room)
				serverWrites++
			case 1:
				room.held.Store(true)
			}
			var update []byte
			var op string
			sentGap, filling := false, false
			switch {
			case fill != nil && (gen.rng.IntN(2) == 0 || index == frames-1):
				update, op, filling, fill = fill, "the fill of a parked frame's gap", true, nil
			case fill == nil && index < frames-1 && gen.rng.IntN(3) == 0:
				update, fill, op, parkedClient, parkedFrom = gen.gapFrames(t, room)
				op, sentGap = "behind a gap, "+op, true
				gaps++
			default:
				update, op = gen.frame(t, room)
			}
			parkedBefore := room.live.PendingStats().Items
			err := room.frame(t, what+" ("+op+")", update)
			switch {
			case sentGap:
				changeParked = err == nil && room.live.PendingStats().Items > parkedBefore
			case filling && err == nil && changeParked && room.live.StateVector().Clock(parkedClient) == parkedFrom:
				takenBack++
			}
			var close *websocket.PeerClose
			var checked *renderRefusal
			switch {
			case err == nil:
				admitted++
			case errors.As(err, &close) && close.Code == contracts.DocumentBusyCloseCode:
				unsure++
			case errors.As(err, &checked) && checked.cause == nil && checked.after > maxPeerRenderingBytes:
				pastBound++
				refused++
			default:
				refused++
			}
			if room.held.Load() && (gen.rng.IntN(2) == 0 || index == frames-1) {
				room.release()
				if rendering, err := renderDocument(room.live); err == nil {
					if size, _ := room.replica.size(); size != int64(len(rendering)) {
						t.Fatalf("%s: once released the room publishes %d bytes and renders %d", what, size, len(rendering))
					}
					room.requireRenderedAs(t, what+", once released", rendering)
				}
			}
		}
		tookPast += room.tookPast
	}
	t.Logf("seed %d: %d frames admitted, %d refused, %d of them by the check for a rendering past the bound and its creep, %d refused for now against a server write the replica had not taken (%d such writes); %d frames taken past the bound and its creep; %d frames sent behind a gap, %d of whose drains the room took back",
		seed, admitted, refused, pastBound, unsure, serverWrites, tookPast, gaps, takenBack)
	if admitted == 0 || pastBound == 0 {
		t.Fatalf("seed %d: %d admitted and %d refused past the bound, want both, or the generator tests nothing", seed, admitted, pastBound)
	}
	if takenBack == 0 {
		t.Fatalf("seed %d: the room took back none of the drains of %d frames sent behind a gap, so the generator tests no drain taken back", seed, gaps)
	}
}

// rcGen generates the documents and frames of the invariant test from one seed.
type rcGen struct{ rng *rand.Rand }

var rcStarts = []int{1, 7, 99_999, 999_999_999}

// document is the stored state of a document of generated blocks after a paragraph of prose that
// takes its rendering to a size near the bound: by mode, anywhere from 48 KiB short of the bound
// to 48 KiB past the bound and its creep (0); within 4 KiB of the bound itself (1); or within
// 4 KiB short of the bound and its creep (2), where a frame that grows the rendering at all can
// pass the line the check refuses at. Only the generated blocks go through the markdown parser,
// whose write budget refuses some markdown of this size; the prose is typed into the document's
// first paragraph as a browser types it.
func (g rcGen) document(t *testing.T, mode int) []byte {
	t.Helper()
	var last error
	for range 32 {
		var blocks []string
		for range 4 + g.rng.IntN(8) {
			blocks = append(blocks, g.block(4))
		}
		body := "x\n\n" + strings.Join(blocks, "\n\n") + "\n"
		tree, err := pmdoc.Parse(body)
		if err != nil {
			last = fmt.Errorf("parse: %w", err)
			continue
		}
		state, err := encodeDocumentTree(tree)
		if err != nil {
			last = fmt.Errorf("encode: %w", err)
			continue
		}
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatal(err)
		}
		rendering, err := renderDocument(doc)
		if err != nil {
			last = fmt.Errorf("render: %w", err)
			continue
		}
		target := pmdoc.MaxDocumentBytes - 48<<10 + g.rng.IntN(maxPeerByteCreep+96<<10)
		switch mode {
		case 1:
			target = pmdoc.MaxDocumentBytes - 4<<10 + g.rng.IntN(8<<10)
		case 2:
			target = maxPeerRenderingBytes - 4<<10 + g.rng.IntN(4<<10)
		}
		first := allTexts(doc.GetXmlFragment(fragmentName))[0]
		doc.Transact(func(txn *crdt.Transaction) { first.Insert(txn, 0, rcProse(max(target-len(rendering), 16)), nil) })
		return crdt.EncodeStateAsUpdateV1(doc, nil)
	}
	t.Fatalf("no generated document parsed and rendered in 32 tries; the last: %v", last)
	return nil
}

func (g rcGen) block(depth int) string {
	kinds := 8
	if depth <= 0 {
		kinds = 3
	}
	switch g.rng.IntN(kinds) {
	case 0:
		return g.inline()
	case 1:
		return strings.Repeat("#", 1+g.rng.IntN(6)) + " " + g.inline()
	case 2:
		var lines []string
		for range 1 + g.rng.IntN(40) {
			lines = append(lines, g.word())
		}
		return "```\n" + strings.Join(lines, "\n") + "\n```"
	case 3:
		return prefixed(g.block(depth-1), "> ", "> ")
	case 4:
		var items []string
		for range 1 + g.rng.IntN(3) {
			items = append(items, prefixed(g.block(depth-1), "- ", "  "))
		}
		return strings.Join(items, "\n")
	case 5:
		start := rcStarts[g.rng.IntN(len(rcStarts))]
		var items []string
		for item := range 1 + g.rng.IntN(3) {
			marker := fmt.Sprintf("%d. ", start+item)
			items = append(items, prefixed(g.block(depth-1), marker, strings.Repeat(" ", len(marker))))
		}
		return strings.Join(items, "\n")
	case 6:
		rows := []string{"| a | b |", "| - | - |"}
		for range 1 + g.rng.IntN(30) {
			rows = append(rows, "| "+g.word()+" | "+g.word()+" |")
		}
		return strings.Join(rows, "\n")
	default:
		breaks := []string{"  \n", "\\\n"}
		return g.inline() + breaks[g.rng.IntN(2)] + g.inline()
	}
}

// prefixed is markdown with its first line after first and every other after rest, as a
// container writes its children; a blank line takes rest without its trailing spaces.
func prefixed(markdown, first, rest string) string {
	lines := strings.Split(markdown, "\n")
	for index, line := range lines {
		prefix := rest
		if index == 0 {
			prefix = first
		}
		if line == "" {
			lines[index] = strings.TrimRight(prefix, " ")
			continue
		}
		lines[index] = prefix + line
	}
	return strings.Join(lines, "\n")
}

var rcEscapable = []string{"~", "*", "_", "`", "#", "[", "]", "<", ">", "\\", "|", "!", "&", "-", "+", "="}

func (g rcGen) word() string {
	var b strings.Builder
	for range 1 + g.rng.IntN(6) {
		if g.rng.IntN(3) == 0 {
			b.WriteString(rcEscapable[g.rng.IntN(len(rcEscapable))])
		} else {
			b.WriteByte(byte('a' + g.rng.IntN(26)))
		}
	}
	return b.String()
}

func (g rcGen) inline() string {
	var parts []string
	for range 2 + g.rng.IntN(10) {
		word := strings.Map(func(r rune) rune {
			if r == '`' || r == '|' {
				return 'x'
			}
			return r
		}, g.word())
		switch g.rng.IntN(6) {
		case 0:
			parts = append(parts, "**w"+word+"w**")
		case 1:
			parts = append(parts, "*w"+word+"w*")
		case 2:
			parts = append(parts, "`"+word+"`")
		case 3:
			parts = append(parts, "~~w"+word+"w~~")
		default:
			parts = append(parts, "w"+word)
		}
	}
	return strings.Join(parts, " ")
}

// rcMarks is what a generated frame formats or unformats with: every mark that writes markdown,
// inline code among them.
var rcMarks = []string{"inlineCode", "strong", "emphasis", "strike_through"}

// frame is a generated browser's update into room, and what it does.
func (g rcGen) frame(t *testing.T, room *rcRoom) ([]byte, string) {
	t.Helper()
	var op string
	update := room.edit(t, func(txn *crdt.Transaction, _ *crdt.Doc) { op = g.change(txn) })
	return update, op
}

// gapFrames is a generated change sent behind a gap: a browser starts a change the room does not
// hold yet (the fill), a change made after it reaches the room first and parks until the fill
// arrives, and the fill's apply then lands it. Half the time the change is another browser's
// nested code block (nestedCodeBlock) inserted right after a paragraph the fill starts: its
// rendering can pass the bound, and it waits on the fill's paragraph as its origin, so the fill's
// apply lands it in a drain of the pending queue, which the room discards where its apply check
// refuses what the drain lands or the apply's scan allowance cannot hold the drain
// (leaveParkedOut). Otherwise the fill is a keystroke into a text and the change any generated
// change (change) the same browser makes in a transaction of its own and sends first, whose
// structs follow the keystroke's clock. It returns the change and the fill, each as its browser
// sends its transaction, what the change does, and the change's client and the clock it starts
// at, which the room's state vector stays at for that client while the change is parked.
func (g rcGen) gapFrames(t *testing.T, room *rcRoom) (parked, fill []byte, op string, client crdt.ClientID, from uint64) {
	t.Helper()
	browser := room.browserCopy(t)
	before := browser.StateVector()
	fragment := browser.GetXmlFragment(fragmentName)
	if g.rng.IntN(2) == 0 {
		at := g.rng.IntN(len(fragment.Children()) + 1)
		browser.Transact(func(txn *crdt.Transaction) {
			paragraph := crdt.NewYXmlElement("paragraph")
			fragment.InsertElement(txn, at, paragraph)
			text := crdt.NewYXmlText()
			paragraph.InsertText(txn, 0, text)
			text.Insert(txn, 0, "f", nil)
		})
		other := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(other, crdt.EncodeStateAsUpdateV1(browser, nil), nil); err != nil {
			t.Fatal(err)
		}
		otherFrom := other.StateVector()
		other.Transact(func(txn *crdt.Transaction) { op = g.nestedCodeBlock(txn, txn.GetXmlFragment(fragmentName), at+1) })
		client = other.ClientID()
		return crdt.EncodeStateAsUpdateV1(other, otherFrom), crdt.EncodeStateAsUpdateV1(browser, before), op, client, otherFrom.Clock(client)
	}
	texts := allTexts(fragment)
	text := texts[g.rng.IntN(len(texts))]
	browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, g.rng.IntN(text.Len()+1), "f", nil) })
	filled := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { op = g.change(txn) })
	client = browser.ClientID()
	return crdt.EncodeStateAsUpdateV1(browser, filled), crdt.EncodeStateAsUpdateV1(browser, before), op, client, filled.Clock(client)
}

// change is a generated browser's change, made in txn, and what it does.
func (g rcGen) change(txn *crdt.Transaction) (op string) {
	fragment := txn.GetXmlFragment(fragmentName)
	texts := allTexts(fragment)
	elements := allElements(fragment)
	text := texts[g.rng.IntN(len(texts))]
	switch g.rng.IntN(8) {
	case 0:
		var b strings.Builder
		for b.Len() < g.rng.IntN(9_000) {
			b.WriteString(g.word())
			if g.rng.IntN(4) == 0 {
				b.WriteString("\n")
			}
		}
		op = fmt.Sprintf("insert %d bytes of text", b.Len())
		text.Insert(txn, g.rng.IntN(text.Len()+1), b.String(), nil)
	case 1:
		if text.Len() == 0 {
			op = "nothing"
			return
		}
		at := g.rng.IntN(text.Len())
		length := 1 + g.rng.IntN(min(text.Len()-at, 200_000))
		op = fmt.Sprintf("delete %d characters", length)
		text.Delete(txn, at, length)
	case 2:
		if text.Len() == 0 {
			op = "nothing"
			return
		}
		at := g.rng.IntN(text.Len())
		length := 1 + g.rng.IntN(text.Len()-at)
		mark := rcMarks[g.rng.IntN(len(rcMarks))]
		var value any = map[string]any{}
		if g.rng.IntN(2) == 0 {
			value = nil
		}
		op = fmt.Sprintf("format %d characters with %s=%v", length, mark, value)
		text.Format(txn, at, length, crdt.Attributes{mark: value})
	case 3:
		element := elements[g.rng.IntN(len(elements))]
		switch element.NodeName {
		case "ordered_list":
			op = "set an ordered list's start number"
			element.SetAttributeValue(txn, "order", int64(999_999_999))
		case "table_cell", "table_header":
			op = "set a cell's spans"
			element.SetAttributeValue(txn, "rowspan", float64(1+g.rng.IntN(60)))
			element.SetAttributeValue(txn, "colspan", float64(1+g.rng.IntN(8)))
		case "heading":
			op = "set a heading's level"
			element.SetAttributeValue(txn, "level", float64(1+g.rng.IntN(6)))
		default:
			op = "set a code block's language"
			element.SetAttributeValue(txn, "language", g.word())
		}
	case 4, 5:
		op = g.nestedCodeBlock(txn, fragment, g.rng.IntN(len(fragment.Children())+1))
	case 6:
		newlines := 1 + g.rng.IntN(8_000)
		op = fmt.Sprintf("type %d newlines", newlines)
		text.Insert(txn, g.rng.IntN(text.Len()+1), strings.Repeat("\n", newlines), nil)
	default:
		var paragraph *crdt.YXmlElement
		for _, element := range elements {
			if element.NodeName == "paragraph" && g.rng.IntN(3) == 0 {
				paragraph = element
			}
		}
		if paragraph == nil {
			op = "nothing"
			return
		}
		op = "insert a hard break"
		paragraph.InsertElement(txn, len(paragraph.Children()), crdt.NewYXmlElement("hardbreak"))
	}
	return op
}

// nestedCodeBlock inserts, in txn, a code block of up to 6,000 newlines nested up to 60 deep in
// quotes and lists of every kind at position at of fragment, the room's document fragment, and
// says what it inserted: every line of the code block renders every container's prefix again, so
// a few thousand bytes of newlines can render hundreds of thousands.
func (g rcGen) nestedCodeBlock(txn *crdt.Transaction, fragment *crdt.YXmlFragment, at int) string {
	depth := 1 + g.rng.IntN(60)
	lines := 1 + g.rng.IntN(6_000)
	parent := any(fragment)
	// Each element goes after what its parent holds, so a list item's leading
	// paragraph, which the schema wants first, stays first.
	insert := func(element *crdt.YXmlElement) {
		switch into := parent.(type) {
		case *crdt.YXmlFragment:
			into.InsertElement(txn, at, element)
		case *crdt.YXmlElement:
			into.InsertElement(txn, into.Len(), element)
		}
		parent = element
	}
	item := func() {
		listItem := crdt.NewYXmlElement("list_item")
		insert(listItem)
		paragraph := crdt.NewYXmlElement("paragraph")
		listItem.InsertElement(txn, 0, paragraph)
		text := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, text)
		text.Insert(txn, 0, "i", nil)
	}
	for level := range depth {
		switch g.rng.IntN(3) {
		case 0:
			insert(crdt.NewYXmlElement("blockquote"))
		case 1:
			list := crdt.NewYXmlElement("ordered_list")
			insert(list)
			if level%2 == 0 {
				list.SetAttributeValue(txn, "order", int64(rcStarts[g.rng.IntN(len(rcStarts))]))
			}
			item()
		default:
			insert(crdt.NewYXmlElement("bullet_list"))
			item()
		}
	}
	code := crdt.NewYXmlElement("code_block")
	insert(code)
	body := crdt.NewYXmlText()
	code.InsertText(txn, 0, body)
	body.Insert(txn, 0, strings.Repeat("\n", lines), nil)
	return fmt.Sprintf("insert a code block of %d lines nested %d deep", lines, depth)
}

// serverWrite is a write of the server's own into room's first text, a local transaction the
// room's apply check does not judge: a cut of up to 200,000 characters, which can shorten a room
// past the bound, or an insert of a few thousand.
func (g rcGen) serverWrite(room *rcRoom) {
	text := allTexts(room.live.GetXmlFragment(fragmentName))[0]
	room.live.Transact(func(txn *crdt.Transaction) {
		if length := text.Len(); length > 0 && g.rng.IntN(3) != 0 {
			text.Delete(txn, 0, 1+g.rng.IntN(min(length, 200_000)))
			return
		}
		text.Insert(txn, 0, rcProse(1+g.rng.IntN(4_000)), nil)
	}, &struct{ server string }{"render check test"})
}

// allElements is every element under fragment, in document order.
func allElements(fragment *crdt.YXmlFragment) []*crdt.YXmlElement {
	var elements []*crdt.YXmlElement
	var walk func(children []any)
	walk = func(children []any) {
		for _, child := range children {
			if element, ok := child.(*crdt.YXmlElement); ok {
				elements = append(elements, element)
				var nested []any
				for _, grandchild := range element.Children() {
					nested = append(nested, grandchild)
				}
				walk(nested)
			}
		}
	}
	var top []any
	for _, child := range fragment.Children() {
		top = append(top, child)
	}
	walk(top)
	return elements
}

// An update that shortens a room already past the bound and its creep, or leaves its rendering as
// it was, is never refused by the check: a delete of characters, and a delete of the format items
// of a mark no rendering writes, each into a room rendering past the creep cap.
func TestAnUpdateThatShrinksOrKeepsTheRenderingIsNeverRefused(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	base := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(base, stateOf(t, rcProse(1_200_000)+"\n\nmarked words here\n"), nil); err != nil {
		t.Fatal(err)
	}
	marker := newDocumentCopy(crdt.WithClientID(778))
	if err := crdt.ApplyUpdateV1(marker, crdt.EncodeStateAsUpdateV1(base, nil), nil); err != nil {
		t.Fatal(err)
	}
	marked := paragraphTexts(marker)[1]
	marker.Transact(func(txn *crdt.Transaction) {
		marked.Format(txn, 0, 6, crdt.Attributes{"proofComment": map[string]any{"id": "c1"}})
	})
	if err := crdt.ApplyUpdateV1(base, crdt.EncodeStateAsUpdateV1(marker, base.StateVector()), nil); err != nil {
		t.Fatal(err)
	}
	room := newRCRoom(t, crdt.EncodeStateAsUpdateV1(base, nil))
	for _, row := range []struct {
		name   string
		update func() []byte
	}{
		{"a delete of ten characters", func() []byte {
			return room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
				paragraphTextsOf(txn.GetXmlFragment(fragmentName))[0].Delete(txn, 0, 10)
			})
		}},
		{"a delete of a comment mark's format items", func() []byte {
			return newV1Frame(0).uint(1).uint(778).uint(1).uint(0).uint(room.live.StateVector().Clock(778)).done()
		}},
	} {
		before, err := renderDocument(room.live)
		if err != nil || len(before) <= maxPeerRenderingBytes {
			t.Fatalf("%s: the room renders %d bytes (%v), want it past %d", row.name, len(before), err, maxPeerRenderingBytes)
		}
		if err := room.frame(t, row.name, row.update()); err != nil {
			t.Fatalf("%s: the hook answered %v, want it applied", row.name, err)
		}
		after, _ := renderDocument(room.live)
		if len(after) > len(before) {
			t.Fatalf("%s: the room went from %d bytes to %d, want no more", row.name, len(before), len(after))
		}
		t.Logf("%s: applied, %d bytes to %d", row.name, len(before), len(after))
	}
}

// A render that fails or panics refuses that update alone, DOC_UPDATE_REFUSED, with the room as it
// was, and the same browser's next keystroke is weighed afresh and lands: an update leaving an
// empty callout, which no build renders, and one whose whole render panics.
func TestARenderThatFailsOrPanicsRefusesThatUpdateAloneAndTheNextLands(t *testing.T) {
	room := newRCRoom(t, stateOf(t, "# Title\n\nOne paragraph of the room.\n\nAnother.\n"))
	unrenderable := room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
		txn.GetXmlFragment(fragmentName).InsertElement(txn, 1, crdt.NewYXmlElement("callout"))
	})
	requireClosed(t, "an empty callout", room.frame(t, "an empty callout", unrenderable),
		contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
	room.nextEdit(t, "an empty callout")

	room.replica.check.panicsForTest.Store(true)
	whole := room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
		paragraph := crdt.NewYXmlElement("paragraph")
		txn.GetXmlFragment(fragmentName).InsertElement(txn, 1, paragraph)
		text := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, text)
		text.Insert(txn, 0, "A paragraph of its own.", nil)
	})
	requireClosed(t, "a render that panics", room.frame(t, "a render that panics", whole),
		contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
	room.replica.check.panicsForTest.Store(false)
	room.nextEdit(t, "a render that panics")
}

// A keystroke pays one render: the room's apply check splices the block it touched before the
// commit, and the update observer takes that rendering in place of one of its own. The room's
// first update after its load is typed first: a load that hands the replica no copy publishes the
// size of its rendering alone, so that first update is rendered whole (keepReplica).
func TestAKeystrokePaysOneRenderOfTheRoom(t *testing.T) {
	room := newRCRoom(t, stateOf(t, "# Title\n\nOne paragraph of the room.\n\nAnother.\n"))
	room.nextEdit(t, "the room's first keystroke")
	before := readRenderPaths(room.service)
	room.nextEdit(t, "a keystroke")
	if took, want := readRenderPaths(room.service).minus(before), (renderPaths{checkIncremental: 1, observerChecked: 1}); took != want {
		t.Fatalf("the keystroke took the paths %+v, want %+v", took, want)
	}
}

// The check knows what an update found only where a rendering stands at the commit the update
// starts from. After a write of the server's own that the replica has not taken yet, it does not,
// and a document the server write shortened could be grown back past the bound by an update
// weighed against the size before that write: here a room of 1.3 MB, of which 20,002 bytes are an
// inline code span over 10,000 pairs of ~a, is cut by 100,000 characters, and the removal of the
// code mark - two format items deleted, no content carried, so the byte rule admits it - would
// grow what is left by about 50,000 bytes, to under what the room rendered before the cut. So
// there an update that leaves the rendering past the bound and its creep is refused for now, with
// the transient DOC_BUSY the editor keeps its edit through and sends again. Once the replica has
// caught up the same updates are weighed exactly: the mark's removal grows the room past the bound
// and is refused DOC_TOO_LARGE, and a delete, refused for now in the window, lands on its retry.
func TestAnUpdateAfterAServerWriteTheReplicaHasNotTakenIsRefusedOnlyForNow(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	base := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(base, stateOf(t, rcProse(1_280_000)+"\n\ncode\n"), nil); err != nil {
		t.Fatal(err)
	}
	written := allTexts(base.GetXmlFragment(fragmentName))[1]
	base.Transact(func(txn *crdt.Transaction) { written.Insert(txn, 0, strings.Repeat("~a", 10_000), nil) })
	marker := newDocumentCopy(crdt.WithClientID(779))
	if err := crdt.ApplyUpdateV1(marker, crdt.EncodeStateAsUpdateV1(base, nil), nil); err != nil {
		t.Fatal(err)
	}
	coded := allTexts(marker.GetXmlFragment(fragmentName))[1]
	marker.Transact(func(txn *crdt.Transaction) {
		coded.Format(txn, 0, coded.Len(), crdt.Attributes{"inlineCode": map[string]any{}})
	})
	if err := crdt.ApplyUpdateV1(base, crdt.EncodeStateAsUpdateV1(marker, base.StateVector()), nil); err != nil {
		t.Fatal(err)
	}
	room := newRCRoom(t, crdt.EncodeStateAsUpdateV1(base, nil))
	unmark := newV1Frame(0).uint(1).uint(779).uint(1).uint(0).uint(room.live.StateVector().Clock(779)).done()
	stale, _ := renderDocument(room.live)
	room.held.Store(true)
	room.live.Transact(func(txn *crdt.Transaction) {
		paragraphTextsOf(txn.GetXmlFragment(fragmentName))[0].Delete(txn, 0, 100_000)
	}, &struct{ server string }{"shortening"})
	requireClosed(t, "the code mark's removal after the server's write", room.frame(t, "the code mark's removal after the server's write", unmark),
		contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	shrink := room.edit(t, func(txn *crdt.Transaction, browser *crdt.Doc) {
		paragraphTextsOf(txn.GetXmlFragment(fragmentName))[0].Delete(txn, 0, 10)
	})
	requireClosed(t, "a delete before the replica caught up", room.frame(t, "a delete before the replica caught up", shrink),
		contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	room.release()
	if err := room.frame(t, "the delete's retry once the replica caught up", shrink); err != nil {
		t.Fatalf("the delete's retry once the replica caught up was answered %v, want it applied", err)
	}
	before, _ := renderDocument(room.live)
	err := room.frame(t, "the code mark's removal once the replica caught up", unmark)
	requireClosed(t, "the code mark's removal once the replica caught up", err,
		contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	var checked *renderRefusal
	if !errors.As(err, &checked) || checked.after <= int64(len(before)) || checked.after > int64(len(stale)) {
		t.Fatalf("the removal was refused %v, want it refused for growing the room past %d bytes to under the %d it rendered before the server's cut, the growth a stale weighing would admit",
			err, len(before), len(stale))
	}
	t.Logf("before the cut %d bytes, after it %d; the code mark's removal would leave %d", len(stale), len(before), checked.after)
}

// A drain the check refuses closes the browser that parked what it would land and nobody else: one
// client's frame of newlines, typed into a code block nested 98 quotes deep right after a
// keystroke of another client's that the room does not hold yet, parks behind it, sent from a tab
// of its own; landing the newlines would take the room past the bound. Then a browser sends that
// keystroke, which fills the gap, and ygo drains the queue in the same apply: the room's apply
// check passes the keystroke and refuses the drain, which is discarded (leaveParkedOut). The
// keystroke stands - its sender stays connected, and a second browser is sent it, which the room
// does only for an update it answered applied (ygo's updateStood) - the tab that parked the
// newlines is closed with the check's own refusal (closeDiscardedSenders), and the room holds
// none of them.
func TestARefusedDrainClosesTheBrowserThatParkedItAndTheUpdateIsAnsweredApplied(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	service, artifactID := newTestService(t)
	prefix := strings.Repeat("> ", 98)
	storeHistory(t, service, artifactID, stateOf(t, rcProse(300_000)+"\n\n"+prefix+"```\n"+prefix+"x\n"+prefix+"```\n"))
	closeResidentRoom(t, service, artifactID)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	alice := http.Header{"X-Dispatch-User": []string{"alice"}}
	sender := docstest.Dial(t, url, alice, artifactID, crdt.New())
	awaitAnswer(t, sender, "the document")
	parker := docstest.Dial(t, url, alice, artifactID, crdt.New())
	awaitAnswer(t, parker, "the document in the parking tab")
	watcher := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"bob"}}, artifactID, crdt.New())
	awaitAnswer(t, watcher, "the document")

	code := allTexts(sender.Doc.GetXmlFragment(fragmentName))[1]
	fill := docstest.Transact(sender.Doc, func(txn *crdt.Transaction) { code.Insert(txn, 0, "FILL", nil) })
	const parkingClient = 4242
	other := crdt.New(crdt.WithClientID(parkingClient))
	if err := crdt.ApplyUpdateV1(other, crdt.EncodeStateAsUpdateV1(sender.Doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	otherCode := allTexts(other.GetXmlFragment(fragmentName))[1]
	parked := docstest.Transact(other, func(txn *crdt.Transaction) { otherCode.Insert(txn, 4, strings.Repeat("\n", 5_000), nil) })
	if err := parker.Write(ygsync.EncodeUpdate(parked)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Minute, "the room to park the newlines", func() bool {
		live := service.srv.GetDoc(artifactID)
		return live != nil && live.PendingStats().Items > 0
	})
	if err := sender.Write(ygsync.EncodeUpdate(fill)); err != nil {
		t.Fatal(err)
	}
	if err := watcher.WaitForText(func(p *docstest.Peer) string {
		if rendering, _ := renderDocument(p.Doc); strings.Contains(rendering, "FILL") {
			return "FILL"
		}
		return ""
	}, "FILL", time.Minute); err != nil {
		t.Fatalf("the second browser was never sent the keystroke that filled the gap: %v", err)
	}
	select {
	case <-parker.Ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the tab that parked the newlines is still connected after their drain was discarded")
	}
	if closed := parker.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentTooLargeCloseReason {
		t.Fatalf("the parking tab was closed %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	}
	select {
	case <-sender.Ended:
		t.Fatalf("the sender was closed %v, want it connected", sender.Closed())
	case <-time.After(2 * time.Second):
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room is not resident")
	}
	if pending, held := live.PendingStats(), live.StateVector().Clock(parkingClient); pending.Items != 0 || held != 0 {
		t.Fatalf("the room parks %+v and holds the parking client to clock %d, want the newlines discarded", pending, held)
	}
	rendering, err := renderDocument(live)
	if err != nil || len(rendering) > maxPeerRenderingBytes {
		t.Fatalf("the room renders %d bytes (%v), want it within %d", len(rendering), err, maxPeerRenderingBytes)
	}
	t.Logf("the keystroke stood and reached the second browser, the parking tab was closed %v; the room renders %d bytes", parker.Closed(), len(rendering))
}

// A load is no browser's apply: a document stored past the bound and its creep loads as it is
// stored, a browser opening it is sent all of it, and that browser can shorten it.
func TestADocumentStoredPastTheBoundLoadsAndCanBeShortened(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, stateOf(t, rcProse(1_300_000)+"\n"))
	closeResidentRoom(t, service, artifactID)
	stored, err := service.Text(context.Background(), artifactID)
	if err != nil || len(stored) <= maxPeerRenderingBytes {
		t.Fatalf("the stored document reads %d bytes (%v), want it past %d", len(stored), err, maxPeerRenderingBytes)
	}
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	browser := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID, crdt.New())
	awaitAnswer(t, browser, "the document stored past the bound")
	if rendering, err := renderDocument(browser.Doc); err != nil || rendering != stored {
		t.Fatalf("the browser holds a %d-byte rendering (%v), want the stored %d", len(rendering), err, len(stored))
	}
	into := allTexts(browser.Doc.GetXmlFragment(fragmentName))[0]
	if _, err := browser.Send(func(txn *crdt.Transaction) { into.Delete(txn, 0, 1_000) }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Minute, "the shortened document to be stored", func() bool {
		text, err := service.Text(context.Background(), artifactID)
		return err == nil && len(text) < len(stored)
	})
	select {
	case <-browser.Ended:
		t.Fatalf("the browser was closed %v, want it connected", browser.Closed())
	default:
	}
}

// paragraphTextsOf is paragraphTexts of fragment, for a transaction on its document, where
// GetXmlFragment would wait for the lock the transaction holds: the first text of each paragraph
// at the fragment's top level.
func paragraphTextsOf(fragment *crdt.YXmlFragment) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	for _, block := range fragment.Children() {
		paragraph, ok := block.(*crdt.YXmlElement)
		if !ok || paragraph.NodeName != "paragraph" {
			continue
		}
		for _, child := range paragraph.Children() {
			if text, ok := child.(*crdt.YXmlText); ok {
				texts = append(texts, text)
				break
			}
		}
	}
	return texts
}
