package docs

import (
	"context"
	"log/slog"

	"github.com/reearth/ygo/crdt"
)

// This file holds the service's side of the room budget: the charge a room's load takes on
// (chargeLoadedRoom), the re-base a compaction gives it (rebaseRoomState), and the sweep that
// closes the rooms the budget cannot hold (sweepRooms, closeIfOpenedOverBudget, roomApplied).

// chargeLoadedRoom charges room's newly loaded instance, whose replica has just published its
// load's rendering (keepReplica), at stateBytes, the bytes of the stored state that load
// decoded, and at the structs the room's stored state holds, and hands the replica the charge
// its drains weigh from then on (renderedReplica.chargeAdded). Both figures are in hand: the
// load read the state, decoded it into the room and decoded it again into the replica, and the
// struct figure is read from the room itself (anchorStructs), so the budget charges the room
// what its two copies were built from without encoding either (roomWeight). stateBytes is where
// the room's tracked state starts, which each apply's report raises from here
// (roomApplies.loaded, roomApplies.noteApply).
//
// It is one of the two paths with no report to read, and so one of the two anchors on the
// reports: the load's own decode of the stored state runs before the room's document is given
// the report at all (bootstrapLoadedState, keepReplica), so the structs the room starts at are
// this reading and no apply's. What the load parks is counted with what its store holds, as a
// report counts it: a load parks what its stored history parked, and those structs are in both
// copies and in the state the next load decodes.
func (s *Service) chargeLoadedRoom(room string, replica *renderedReplica, stateBytes int64) *roomCharge {
	charge := s.budget.load(room, stateBytes, int64(anchorStructs(room, replica.applies(), replica.live)))
	replica.applies().loaded(replica.live, stateBytes)
	replica.charge.Store(charge)
	return charge
}

// rebaseRoomState re-bases room's tracked state on stateBytes, what a compaction that folded the
// room's whole stored history into one row wrote (compactResult), and charges the room again
// from it; mark is what the compaction read of the room before it read the history
// (markRebase). A room this server does not hold, or one no load listed, is left alone.
//
// The tracked state is the library's own count of what each commit added (roomApplies.state),
// which follows the direct form of the encoded state, each struct where the store or the
// pending queue holds it. The reading replaces it only where the reading is exact: where the
// room holds nothing pending, which an encode reaches the direct form of, and nothing the store
// had not taken. That settles whatever the figure was owed and any margin it carried - the
// bytes a server's delete is counted at above what it took out (countCommit). Any other reading
// is a lower bound, and the figure keeps the larger of the two (roomApplies.take). A room whose
// pending queue or pending deletes hold anything encodes through a merge that can be shorter
// than the direct form, and taking that as the truth would put the figure below what the room
// holds once its gaps fill.
//
// The reading is of the store and not of the room, so it also falls short wherever the room
// holds a commit the store has not taken: one still in the room's persistence, or one the room
// made after the compaction read its rows. Taking such a reading as the truth dropped that
// commit's bytes from the figure until the room's next load. So the reading is taken as the
// truth only where mark covers it (rebaseMark.covers): the room held nothing the store lacked
// when the compaction began, and has committed nothing since.
//
// It is the second anchor on the reports (anchorStructs): a compaction is no apply and reports
// nothing, so the structs it charges the room at are read from the document, held against what
// the room's applies have reported, and taken as the truth.
func (s *Service) rebaseRoomState(room string, stateBytes int64, mark rebaseMark) {
	live := s.srv.GetDoc(room)
	if live == nil {
		return
	}
	s.rebaseResident(room, live, stateBytes, mark)
}

// rebaseResident is rebaseRoomState for live, the document room holds.
func (s *Service) rebaseResident(room string, live *crdt.Doc, stateBytes int64, mark rebaseMark) {
	weighing := s.roomWeighingOf(live)
	if weighing == nil {
		return
	}
	applies := weighing.applies()
	tracked := applies.rebase(stateBytes, func() bool { return mark.covers(live, applies) })
	// The bytes' half of the anchor: what the room's applies reported against what this re-base
	// read. The reports follow the direct form of the state, which an encode reaches or falls
	// short of, so a difference above zero is what the merged encode left out, what the store
	// had still to take or what a server write's floor carried; one below zero is a path that
	// added to the room without counting through the tally, which is the defect the anchor
	// exists to show (dispatch://INFRA-179, the architect's accumulator condition).
	if tracked != stateBytes {
		level := slog.LevelDebug
		if tracked < stateBytes {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, "dispatch: a room's tracked state differs from what its re-base read",
			"room", room, "tracked", tracked, "read", stateBytes, "difference", tracked-stateBytes)
	}
	weighing.roomCharge().weigh(applies.stateBytes(), int64(anchorStructs(room, applies, live)))
}

// rebaseMark is what a compaction reads of a resident room before it reads the room's stored
// history (markRebase): live, the room's document, commits, the commits the room's tracked
// state had taken (roomApplies.commits), and stored, whether the store then held everything the
// room did - no commit the room's replica had still to drain from its log, none of its instance's
// listed as unstored, and nothing pending.
type rebaseMark struct {
	live    *crdt.Doc
	commits uint64
	stored  bool
}

// markRebase is the mark a compaction of room takes before it reads the room's rows
// (tryCompaction), and no mark - one that covers nothing - for a room this server does not hold.
func (s *Service) markRebase(room string) rebaseMark {
	live := s.srv.GetDoc(room)
	if live == nil {
		return rebaseMark{}
	}
	return s.markResident(live)
}

// markResident is markRebase for live, the document a room holds. It is read holding the
// replica's lock, so no drain is between taking a commit from the log and listing it unstored
// (Service.classifyDrained): every commit the room has made is in the log still, listed on the
// instance until its append returns, or stored. A commit stored by the transaction that wrote it
// - a published live write, a catch-up's rows - is in the store before the room takes it, and a
// repair is appended under the document's advisory lock, which the compaction's write takes
// too (lockRoom), so a repair the room holds is a row above the fold point by the time the
// compaction checks there is none, and no re-base follows. A room whose load kept no instance
// lists nothing the mark can read, and is never marked stored.
func (s *Service) markResident(live *crdt.Doc) rebaseMark {
	replica := s.replicaOf(live)
	instance := s.instanceOf(live)
	if replica == nil || instance == nil {
		return rebaseMark{live: live}
	}
	replica.lockForUpdate()
	defer replica.mu.Unlock()
	mark := rebaseMark{live: live, commits: replica.applies().commits.Load()}
	mark.stored = (replica.log == nil || replica.log.Pending() == 0) && !instance.holdsUnstored() && !holdsPending(live)
	return mark
}

// covers is whether a reading of the store taken after the mark holds everything live holds:
// the mark is of live, the store then held all of it, and the room's tally has taken no commit
// since. A commit after that adds to the figure as it lands, which a re-base taking the reading
// keeps on top of it.
func (m rebaseMark) covers(live *crdt.Doc, applies *roomApplies) bool {
	return m.live == live && m.stored && applies.commits.Load() == m.commits
}

// sweepRooms is the budget's sweep (roomBudget.sweep): while the budget is over its limit
// (roomBudget.short) it
// closes the charged rooms that hold nothing outlasting them, the heaviest first, so as few as it
// can, through the eviction a failed room takes (evictRoom), whose close flushes the room before
// it goes. Unused is the service's own test (unusedLocked), not the weaker "no browser
// connected": a room with no browser can still hold an open writer, an armed settlement or an
// update its persistence has not appended. A room still loading is not closed either: its load
// has not returned, and the opener waiting on it would find it gone. The close's unload returns
// the room's charge (releaseUnloadedRoom), and ygo's CloseRoom fires that unload before it returns
// wherever it is the one that removed the room (sjawhar/ygo v1.55.1-sami.8, closeRoom,
// provider/websocket/inject.go:723-740), so the next look at the budget sees the charge gone. Each
// close is logged with the budget's own figures before and after it, which is how an operator, or
// a test of the real process, tells a room the budget closed from one that retired on its own.
//
// A room it leaves because something other than a connected browser holds it - a settlement, a
// writer, an append, a load - is pending: it will be closable once that ends, which starts a sweep
// of its own (Service.unlockState), so an admission waiting on the budget waits for it (await).
// So is a room whose browsers have all gone though their handlers have not yet forgotten them
// (roomState.departedPeers): its last browser's leaving runs the settlement it owes before they do.
// Once nothing more can be closed it raises full if the budget is still over its limit
// (roomBudget.swept).
//
// It runs on a goroutine of its own, as failRoomLocked's eviction does, which Shutdown joins
// (evictWG); a service stopping sweeps nothing, since its Shutdown closes every room itself.
func (s *Service) sweepRooms() {
	pending := false
	defer func() { s.budget.swept(pending) }()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	defer s.evictWG.Done()
	for _, room := range s.budget.heaviest() {
		if !s.budget.short() {
			return
		}
		state := s.lockExistingState(room)
		if state == nil {
			pending = true
			slog.Info("dispatch: the room budget's sweep found no state for a charged room",
				"room", room)
			continue
		}
		if s.srv.GetDoc(room) == nil || !s.unusedLocked(state) {
			// Left as it is: unlockState's look at the budget would only ask for this sweep again.
			// The line names what holds it, which is the one reading that says why a sweep over a
			// budget past its limit closed nothing: a browser still connected, a browser gone
			// whose handler has not forgotten it, the room's own work (a settlement, a writer, an
			// append), or no room at all (INFRA-179).
			pending = pending || state.onlyDepartedLocked()
			slog.Info("dispatch: the room budget's sweep left a charged room open",
				"room", room, "held_by", s.holdingLocked(state), "pending", pending)
			state.mu.Unlock()
			continue
		}
		// As Evict does: a settlement read under the generation before the close is one the close
		// has invalidated. The state is unlocked without unlockState's look at the budget, which
		// would take this room, unused, for one to sweep or close again.
		state.roomGeneration++
		state.mu.Unlock()
		before, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			// An Apply is inside its callback on the room (closeUnused): the room will be
			// closable once that apply ends, which hands it to closeIfOpenedOverBudget and kicks
			// the budget, so the admissions waiting on it wait for that rather than being refused.
			pending = true
			continue
		}
		after, limit := s.budget.figures()
		slog.Info("dispatch: the room budget closed an unused document",
			"room", room, "charged_before", before, "charged", after, "limit", limit)
	}
}

// closeIfOpenedOverBudget closes room once it is unused (unusedLocked) when its charge is marked
// closeOnceUnused: a room the server's own work loaded while the budget was over its limit
// (roomBudget.load), which no browser has joined since (roomBudget.joinedLocked). It is called
// whenever room's state may have stopped holding anything (Service.unlockState) and once each
// Server.Apply on it has returned (roomServer.Apply), which between them see the end of every
// opener's use: a writer's, a settlement's and an append's end in the state, a read's in its
// Apply. A room still loading, or one a browser holds, is left: a browser connected is a holder
// unusedLocked names, so a room a browser holds is never closed this way. The close is the
// eviction the sweep makes (evictUnusedRoom), on a goroutine of its own, which Shutdown joins
// (evictWG), and is logged with the budget's figures before and after it as the sweep's are. A
// room an Apply is inside its callback on is left marked, for the end of that Apply to look at
// again.
//
// A room the budget's opening still names is left marked too (roomBudget.opened), whatever its
// state reads: the work that booked the opening has not let the room go, and it records what
// holds the room inside its own Apply, after the load that marks it. ygo takes and releases the
// room's state in between - the awareness relay's RoomActivated runs inside getOrCreateRoom,
// during the load and before any caller's callback - and this is called from each such release,
// which would otherwise read the room as unused and close the room its opener is about to use.
func (s *Service) closeIfOpenedOverBudget(room string) {
	if s.budget.marked.Load() == 0 || !s.budget.isMarked(room) {
		return
	}
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if state.released || s.srv.GetDoc(room) == nil || !s.unusedLocked(state) ||
		s.budget.opened(room) || !s.budget.takeMark(room) {
		state.mu.Unlock()
		return
	}
	state.roomGeneration++
	state.mu.Unlock()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	go func() {
		defer s.evictWG.Done()
		before, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			s.budget.markAgain(room)
			return
		}
		after, limit := s.budget.figures()
		slog.Info("dispatch: the room budget closed a document the server opened over it",
			"room", room, "charged_before", before, "charged", after, "limit", limit)
	}()
}

// roomApplied is what the service does with a room once a Server.Apply on it has returned
// (roomServer.applied). An apply holds the room against the budget's closes while it is inside
// its callback and holds nothing in the room's state, so those closes give up in front of it
// (closeUnused) and nothing else reports its end: this is where a room the sweep passed over, and
// one the server opened over the budget, are looked at again. A room the budget does not charge
// starts no sweep (roomBudget.kick) and carries no mark.
func (s *Service) roomApplied(room string) {
	s.budget.kick(room)
	s.closeIfOpenedOverBudget(room)
}
