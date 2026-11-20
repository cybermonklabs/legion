package docs

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// A browser's frame that leaves anything of its own parked is answered not applied
// (websocket.Server.AnswerParkedNotApplied), so the editor keeps the row it saved for the frame
// (pending-sync.ts). The editor clears a row only on an applied answer to a frame that carries it:
// an Update of the row's very bytes, or a SyncStep2 written after the row was recorded, which
// carries everything the browser holds that the room lacks. The commit that later lands what the
// frame parked - another browser's frame that fills the gap, a catch-up - stores it, but carries
// none of the sender's frames, so the sender's row stays and its editor goes on saying the edit
// has not reached Dispatch after it has.
//
// So the room tells the sender, once the commit that landed what its frames parked is stored: it
// sends that browser the room's SyncStep1 (websocket.Server.ResyncPeer), the browser's provider
// answers with a SyncStep2 of what the room lacks, which is now nothing of those frames, and the
// room answers that SyncStep2 applied, which clears every row the browser recorded before it.
// What the frames parked is landed once the room holds every client they reach past what it held
// to the clock they reach (parkedNeeds); the replica, which stands at each commit as the room left
// it, marks the commit position at which a sender's frames have all landed (coverParked), and the
// room's persistence, once it has stored through that position, resyncs the sender
// (Service.resyncLanded). A sender is resynced once for each landing commit, whatever number of
// its frames that commit landed; only once the room's store holds the commit, so the applied
// answer the resync earns is the truth.
//
// What this keeps is bounded by the room's pending queue: a sender is listed only while a frame
// of its has left something parked, with one clock for each client its frames reach past the
// room, each of which waits behind a struct or a delete range the queue holds, which takes at most
// MaxPendingItems structs (maxParkedStructs) and MaxPendingDeleteRanges delete ranges
// (maxParkedDeleteRanges), the ranges only under clients the sender's socket holds (parkJudge). A
// sender is dropped once it is resynced, once its connection ends, and, while it waits on nothing
// the queue still holds, once the queue empties: what it parked was then discarded with a refused
// drain, which closes it (closeDiscardedSenders).

// parkedWaits is, for one room instance, each browser connection whose frames left something
// parked that has not landed yet, by the origin ygo applies its updates with.
type parkedWaits struct {
	// held is how many senders are listed, so a commit in a room listing none reads one atomic.
	held  atomic.Int32
	mu    sync.Mutex
	waits map[any]*parkedWait
}

// parkedWait is one sender's frames that parked: needs, the clock the room must hold each client
// to for all of them to have landed, coveredAt, the commit position at which the room first held
// them, zero until it has, and unwatch, which stops the watch that drops the sender as its
// connection ends, for a sender the list drops first.
type parkedWait struct {
	needs     crdt.StateVector
	coveredAt uint64
	unwatch   func() bool
}

// dropLocked takes origin, whose wait is wait, off the list. The caller holds w.mu.
func (w *parkedWaits) dropLocked(origin any, wait *parkedWait) {
	delete(w.waits, origin)
	w.held.Add(-1)
	if wait.unwatch != nil {
		wait.unwatch()
	}
}

// noteParked lists the sender of info's frame, which its apply stood with, where the frame left
// something parked: before and after are the room's pending queue around the apply. A frame into
// a room whose queue the apply left empty parked nothing, and the room drops every sender that
// waits on what the queue no longer holds.
func (a *roomApplies) noteParked(info websocket.PeerUpdateInfo, before, after crdt.PendingStats) {
	if a == nil {
		return
	}
	if after.Items == 0 && after.DeleteRanges == 0 {
		a.parked.dropUncovered()
		return
	}
	if needs := parkedNeeds(info.Doc, info.Update, after.DeleteRanges > before.DeleteRanges); len(needs) > 0 {
		a.parked.note(info.Context, info.Origin, needs)
	}
}

// parkedNeeds is, for each client update reaches past what doc holds, the clock doc must hold the
// client to for all of update to have landed: where its structs end (crdt.ParseUpdateMeta, which
// reads their headers alone), and, where readDeletes says the apply left the room more delete
// ranges parked than it found, where each of its delete ranges ends, read from the update's delete
// set (eachV1Range, which passes the structs without decoding their content). Only a frame that
// grew the queue's delete ranges reads its delete set, and only a frame carrying structs or
// growing them reads doc's state vector: a delete that grew nothing answers nothing. A delete set
// the reader does not read to its end adds nothing, since what it read of it may not be the
// update's.
func parkedNeeds(doc *crdt.Doc, update []byte, readDeletes bool) crdt.StateVector {
	_, to, err := crdt.ParseUpdateMeta(update)
	if (err != nil || len(to) == 0) && !readDeletes {
		return nil
	}
	held := doc.StateVector()
	needs := crdt.StateVector{}
	if err == nil {
		for client, end := range to {
			if end > held[client] {
				needs[client] = end
			}
		}
	}
	if readDeletes {
		deletes := crdt.StateVector{}
		read := eachV1Range(update, func(crdt.ClientID, uint64, uint64) {}, func(client crdt.ClientID, clock, length uint64) {
			if end := clock + length; length > 0 && end > held[client] {
				deletes[client] = max(deletes[client], end)
			}
		})
		if read {
			for client, end := range deletes {
				needs[client] = max(needs[client], end)
			}
		}
	}
	return needs
}

// note lists origin as waiting on needs, beside what it already waits on, and starts its wait over:
// a frame that parks again has not landed. The connection's context drops it as the connection
// ends.
func (w *parkedWaits) note(connection context.Context, origin any, needs crdt.StateVector) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waits == nil {
		w.waits = make(map[any]*parkedWait)
	}
	wait, listed := w.waits[origin]
	if !listed {
		wait = &parkedWait{needs: make(crdt.StateVector, len(needs))}
		w.waits[origin] = wait
		w.held.Add(1)
		if connection != nil {
			wait.unwatch = context.AfterFunc(connection, func() { w.forget(origin) })
		}
	}
	for client, clock := range needs {
		wait.needs[client] = max(wait.needs[client], clock)
	}
	wait.coveredAt = 0
}

// forget drops origin.
func (w *parkedWaits) forget(origin any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wait, listed := w.waits[origin]; listed {
		w.dropLocked(origin, wait)
	}
}

// dropUncovered drops every sender whose frames have not landed: the room's queue holds nothing,
// so nothing they wait on is left to land.
func (w *parkedWaits) dropUncovered() {
	if w.held.Load() == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for origin, wait := range w.waits {
		if wait.coveredAt == 0 {
			w.dropLocked(origin, wait)
		}
	}
}

// coverParked marks, for each sender whose frames doc - the replica, standing at commit position
// seq as the room's commit left it - holds all of, seq as where they landed. A room listing no
// sender reads one atomic, and one whose every listed sender has landed already reads no state
// vector: doc's is read once, at the first sender still waiting. No holder of doc's lock takes
// w.mu, so reading it under w.mu waits on nothing that waits on this.
func (a *roomApplies) coverParked(doc *crdt.Doc, seq uint64) {
	if a == nil || a.parked.held.Load() == 0 {
		return
	}
	w := &a.parked
	w.mu.Lock()
	defer w.mu.Unlock()
	var held crdt.StateVector
	for _, wait := range w.waits {
		if wait.coveredAt != 0 {
			continue
		}
		if held == nil {
			held = doc.StateVector()
		}
		if storedClocksReached(held, wait.needs) {
			wait.coveredAt = seq
		}
	}
}

// landedParked takes out the senders whose frames landed at a commit position through, through
// which the room's store now holds the room's commits.
func (a *roomApplies) landedParked(through uint64) []any {
	if a == nil || a.parked.held.Load() == 0 {
		return nil
	}
	w := &a.parked
	w.mu.Lock()
	defer w.mu.Unlock()
	var landed []any
	for origin, wait := range w.waits {
		if wait.coveredAt != 0 && wait.coveredAt <= through {
			landed = append(landed, origin)
			w.dropLocked(origin, wait)
		}
	}
	return landed
}

// resyncLanded resyncs every browser whose parked frames landed at or before through, a commit
// position of instance through which the room's persistence has just stored the room's commits
// (StoreUpdateStored), so the answer to the SyncStep2 its provider sends back clears the rows the
// editor kept for those frames. A position of an instance the room no longer runs resyncs nothing.
func (s *Service) resyncLanded(room string, instance *roomInstance, through uint64) {
	live := s.srv.GetDoc(room)
	if live == nil || instance == nil || s.instanceOf(live) != instance {
		return
	}
	for _, origin := range s.roomWeighingOf(live).applies().landedParked(through) {
		s.srv.ResyncPeer(origin)
	}
}
