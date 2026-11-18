package docs

import (
	"context"
	"errors"
	"log/slog"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// retireSettlementRoom retires the room a late departure's settlement loaded (roomState.warmed)
// once nothing holds the room's record, unless a browser or a writer has taken the room up since
// (takeUpSettlementRoom, openLiveWrite). That settlement loads a room only after a close took the
// document's room out of ygo's map from under the departing browser (settleLastPeer,
// withSettlementLoad); ygo would otherwise keep the room it puts back resident, and holding a place
// under the room cap, for its idle minute (roomIdleTimeout), though no one asked for it. A room a
// timer's settlement loads - a cold write's (publishLiveWrite) - is no such room: it idles out as
// any room does, and the writes after it publish into it. Its callers are the ends of what can run
// that settlement or a retry of it, each once it has let go of every hold it had on the room's
// record: a document socket, whose browser's departure ran the settlement before the socket ends
// and whose registration and admission hold the record until then (ServeHTTP), a timer's
// settlement (scheduleSettleAfterLocked) and an issue's synchronous one (settleOwedNow). Shutdown's
// closes every room itself.
//
// Whether the room was the settlement's own is what its load recorded, not a look at the name: a
// settlement that found no room may still find one another caller's load published, which is that
// caller's (withSettlementLoad). Whether that room has gone is read off its load's token
// (warmedRoom): a room still loading keeps its mark and is not retired here, since the settlement
// whose load it is retires it as that settlement ends, and a room that has left ygo's map is
// forgotten by its unload. What holds the record is read under its lock (unusedLocked) - a
// browser registered, a socket being admitted, an open writer, a settlement running or armed, an
// update not yet stored - first here and again with the room's close gate held, from inside an
// Apply on the room. A room something holds stays the settlement's: another settlement, or a
// retry armed for this one, retires it as it ends. The second look takes a reservation on the
// record (reserveRetirement), which a writer or a settlement that would take the room up from then
// on waits for (awaitRetirement), and which ends once ygo's close has returned (endRetirement): the
// close cannot be made under the record's lock, which ygo's flush and the room's unload take, so
// the reservation is what keeps those holders off the room between the look and the close. The
// close is ygo's CloseRoomWithin without force, which checks again, under its own locks and where
// it takes the room out of its map, that no peer is on the room and no other caller inside it
// (roomServer.retireInstance): a browser joining, or a caller entering the room, is ygo's to keep
// it for. Two retirements inside the room at once, as when several browsers' sockets end after a
// forced close, refuse the first one's close (websocket.ErrRoomInUse): the room keeps its mark,
// and the second waits for the first's reservation to end, by which time the first has left the
// room, and closes it then (reserveRetirement). A room that retired between the first look and the
// Apply is loaded again by the Apply, carrying the same mark, and retired the same way.
func (s *Service) retireSettlementRoom(room string) {
	if s.stopping.Load() {
		return
	}
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	warmed := state.warmed
	switch {
	case warmed == nil:
	case s.loadEnded(warmed.load):
		state.warmed, warmed = nil, nil
	case s.srv.GetDoc(room) != warmed.doc:
		// Still loading, or out of ygo's map with its unload to come.
		warmed = nil
	}
	unused := s.unusedLocked(state)
	s.unlockState(room, state)
	if warmed == nil || !unused {
		return
	}
	var reserved *roomState
	var retireErr error
	ctx := withSettlementLoad(withOwnerVerified(WithoutRecoveryWait(context.Background())))
	err := s.srv.Apply(ctx, room, func(held *crdt.Doc, _ func(func(*crdt.Transaction))) {
		if s.retireEntered != nil {
			s.retireEntered(room)
		}
		_, retireErr = s.srv.retireInstance(room, held, func() bool {
			if reserved = s.reserveRetirement(room, held); reserved != nil && s.afterRetireReserved != nil {
				s.afterRetireReserved(room)
			}
			return reserved != nil
		})
	})
	if reserved != nil {
		if s.afterRetireLeft != nil {
			s.afterRetireLeft(room)
		}
		s.endRetirement(room, reserved)
	}
	if err == nil || errors.Is(err, websocket.ErrNoChanges) {
		err = retireErr
	}
	if err != nil {
		slog.Warn("dispatch: retire the room a settlement loaded; it stays until its idle minute runs out", "room", room, "error", err)
	}
}

// reserveRetirement takes a retirement's reservation on room's record (roomState.retiring) when
// held is still the room a late departure's settlement loaded and nothing holds the record
// (unusedLocked), and answers that record; it answers nil, and takes nothing, when held is not to
// retire (retireSettlementRoom). It keeps the room's mark: ygo may still refuse the close, while
// another caller is inside the room, and the mark is what the next retirement closes the room by;
// once the close has taken the room, the room's unload forgets the mark (releaseUnloadedRoom).
// Another retirement's reservation it waits out (awaitRetirement) rather than reads as a holder:
// that retirement leaves the room (its Server.Apply returns) before it ends its reservation, so
// once the reservation ends this retirement's close finds no other retirement inside the room.
// The wait holds the room's close gate, which the other retirement let go of once its close
// returned, and no lock of the record's.
func (s *Service) reserveRetirement(room string, held *crdt.Doc) *roomState {
	for {
		state := s.lockExistingState(room)
		if state == nil {
			return nil
		}
		if retiring := state.retiring; retiring != nil {
			s.unlockState(room, state)
			_ = s.awaitRetirement(context.Background(), room, retiring)
			continue
		}
		if state.warmed == nil || state.warmed.doc != held || !s.unusedLocked(state) {
			s.unlockState(room, state)
			return nil
		}
		state.retiring = make(chan struct{})
		s.unlockState(room, state)
		return state
	}
}

// endRetirement ends the reservation a retirement took on state, room's record
// (reserveRetirement), once ygo's close of the room has returned: the writers and settlements
// waiting for it go on (awaitRetirement). The reservation held the record, so this is the record
// it was taken on.
func (s *Service) endRetirement(room string, state *roomState) {
	state.mu.Lock()
	retiring := state.retiring
	state.retiring = nil
	s.unlockState(room, state)
	close(retiring)
}

// awaitRetirement waits, holding no lock of the record's, for retiring, a retirement's
// reservation on room's record (roomState.retiring), to end, or for ctx to end, which it answers.
// Its callers, a writer taking the writer slot (openLiveWrite), a settlement taking the room
// (settleRoomWithin) and another retirement (reserveRetirement), then look at the record again. A
// writer waits holding its document's owner lock (beside an earlier build, its owner row too) and
// no lock of the document's own, which a taker of both takes after the owner lock
// (lockDocumentRoom): the close it waits for takes neither, and its
// compaction leaves the document's lock to a later one when another holder has it
// (servicePersistenceAdapter.Compact). A retirement waits holding the room's close gate, which no
// step from the other retirement's close to the end of its reservation takes.
func (s *Service) awaitRetirement(ctx context.Context, room string, retiring <-chan struct{}) error {
	if s.awaitingRetirement != nil {
		s.awaitingRetirement(room)
	}
	select {
	case <-retiring:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// takeUpSettlementRoom is ygo's OnFirstPeer: a browser has joined the room, which is no longer a
// room a settlement loaded for itself, and idles out as any room a browser left
// (retireSettlementRoom).
func (s *Service) takeUpSettlementRoom(_ context.Context, room string) {
	if state := s.lockExistingState(room); state != nil {
		state.warmed = nil
		s.unlockState(room, state)
	}
}

// warmedRoom is a room instance a late departure's settlement loaded (roomState.warmed): the
// document ygo's load hook was handed, and the token that load counted under (roomLoaded). Whether
// the instance has gone is read off the token (loadEnded), not off the name: ygo's GetDoc answers
// nil for a room still loading as for one that has gone (sjawhar/ygo v1.55.1-sami.2,
// provider/websocket/server.go:1709-1734), so an earlier instance's late unload, run while this one
// loads, would read as this one's end. The token ends only once its room has left ygo's map: an
// unload ends the name's oldest load, never that of the room ygo holds or is loading
// (releaseUnloadedRoom says why), and a load that fails ends its own (onLoadDocument).
type warmedRoom struct {
	doc  *crdt.Doc
	load *residentLoad
}
