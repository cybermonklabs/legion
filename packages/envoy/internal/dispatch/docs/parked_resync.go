package docs

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// answer the resync earns is the truth. The same store marks the rows of the ids the sender's
// frames wrote under that await its socket's mark (markParkedLanded): the landing was another
// apply's, which marks its own socket's ids alone, and the sender's answer to the resync writes
// under no id.
//
// What this keeps is bounded by the room's pending queue: a sender is listed only while a frame
// of its has left something parked, with one clock for each client its frames reach past the
// room, each of which waits behind a struct or a delete range the queue holds, which takes at most
// MaxPendingItems structs (maxParkedStructs) and MaxPendingDeleteRanges delete ranges
// (maxParkedDeleteRanges), the ranges only under clients the sender's socket holds (parkJudge). A
// sender is dropped once it is resynced; once a refused drain discards what it parked, which
// closes it (closeDiscardedSenders, dropDiscarded); and, while it waits on nothing the queue still
// holds, once the queue empties.
//
// A sender whose connection ends is resynced no more, but what its frames parked can still land:
// a tab closed while its frame waited behind another browser's lost edit, which that browser
// sends again. So where those frames reach ids whose rows await its socket's mark (parkedMarks),
// the list keeps its person and those ids alone, with the clocks they wait on, and lets go of its
// connection and its origin (parkedWaits.end); the store of the commit that lands them marks them
// for that person (markEndedLanded), or the row of an id the stored history names would stay
// unmarked, a give-back or the sweep would take it, and the person's next page replaying the
// closed tab's saved rows under it would be refused and drop them. ygo forgets a sender's origin
// as its connection ends (crdt.Doc.ForgetParkedOrigin), so a discard after that names it no more:
// such a record then waits on nothing the queue holds. Each record waits on a struct or a range
// of its own sender's the queue held as it ended, so the list keeps no more records still waiting
// than the queue holds, and drops the oldest past that after each browser apply (boundEnded).

// parkedWaits is, for one room instance, each browser connection whose frames left something
// parked that has not landed yet, by the origin ygo applies its updates with, and what it keeps
// of each such connection that has ended, by the record itself.
//
// The list is a Go map, which keeps the slots of the most entries it ever held: a room whose list
// reached the queue's thousands of records and fell to one would keep slots for thousands, which
// no room charge counts, for as long as the room runs. So the map is dropped once it empties, and
// replaced by a map made at its size and filled by a copy once it holds under a quarter of the
// most it has held since it was made (peak, shrinkLocked), as cybermonklabs/ygo v1.55.1-legion.4
// does for its record of who parked each struct: after each removal, and after each removal in
// bulk rather than inside it. A rebuild copies under a quarter of the peak after more than three
// quarters of it were removed, so it costs less than one copy for each removal.
type parkedWaits struct {
	// held is how many senders are listed, so a commit in a room listing none reads one atomic.
	held atomic.Int32
	// endedHeld is how many of them are connections that have ended (parkedWait.ended), so an
	// apply in a room listing no more than its queue holds reads one atomic (boundEnded).
	endedHeld atomic.Int32
	mu        sync.Mutex
	waits     map[any]*parkedWait
	// peak is the most entries waits has held since it was made (shrinkLocked).
	peak int
	// endings counts the connections that ended while listed, the order of their records
	// (endedSender.at).
	endings uint64
}

// parkedWait is one sender's frames that parked: origin, the origin ygo applies its updates with,
// which the resync goes to and the list keys it by; connection, the connection they came on, whose
// person the ids they reach are marked landed for once they land (markParkedLanded), nil where the
// apply's context named none; ended, set once that connection has ended (parkedWaits.end), its
// person and the order it ended in, with origin and connection let go and needs cut to the ids to
// mark; needs, the clock the room must hold each client to for all of them to have landed;
// coveredAt, the commit position at which the room first held them, zero until it has; and
// unwatch, which stops the watch that ends the sender as its connection ends, for a sender the
// list drops first.
type parkedWait struct {
	origin     any
	connection *connectionState
	ended      *endedSender
	needs      crdt.StateVector
	coveredAt  uint64
	unwatch    func() bool
}

// endedSender is what the list keeps of a sender whose connection ended with its frames still
// parked (parkedWaits.end), beside the clocks its record waits on, which are those of the ids its
// frames reach past the room whose rows awaited its socket's mark then (parkedMarks): the person
// it was signed in as, for whom the store of their landing marks those ids (markEndedLanded), and
// at, the order it ended in.
type endedSender struct {
	actor model.Actor
	at    uint64
}

// dropLocked takes wait off the list. The caller holds w.mu, and shrinks the list (shrinkLocked)
// once it has taken off all it takes off, after any range over w.waits it drops from.
func (w *parkedWaits) dropLocked(wait *parkedWait) {
	if wait.ended != nil {
		delete(w.waits, wait)
		w.endedHeld.Add(-1)
	} else {
		delete(w.waits, wait.origin)
	}
	w.held.Add(-1)
	if wait.unwatch != nil {
		wait.unwatch()
	}
}

// shrinkLocked drops the list's map once it is empty, and replaces it with a map made at its size
// and filled by a copy once it holds under a quarter of peak, the most it has held since it was
// made (parkedWaits). The check is one comparison. The caller holds w.mu.
func (w *parkedWaits) shrinkLocked() {
	switch n := len(w.waits); {
	case n == 0:
		w.waits, w.peak = nil, 0
	case n < w.peak/4:
		fresh := make(map[any]*parkedWait, n)
		maps.Copy(fresh, w.waits)
		w.waits, w.peak = fresh, n
	}
}

// noteParked lists the sender of info's frame, which its apply stood with, where the frame left
// something parked: before and after are the room's pending queue around the apply. A frame into
// a room whose queue the apply left empty parked nothing, and the room drops every sender that
// waits on what the queue no longer holds. The records of connections that ended are held to what
// the queue holds after the apply (boundEnded).
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
	a.parked.boundEnded(after.Items + after.DeleteRanges)
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
// a frame that parks again has not landed. The connection's context ends it as the connection
// ends (end), and names the socket whose person the ids its frames reach are marked for
// (connectionOf).
func (w *parkedWaits) note(connection context.Context, origin any, needs crdt.StateVector) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waits == nil {
		w.waits = make(map[any]*parkedWait)
	}
	wait, listed := w.waits[origin]
	if !listed {
		wait = &parkedWait{origin: origin, connection: connectionOf(connection), needs: make(crdt.StateVector, len(needs))}
		w.waits[origin] = wait
		w.peak = max(w.peak, len(w.waits))
		w.held.Add(1)
		if connection != nil {
			wait.unwatch = context.AfterFunc(connection, func() { w.end(origin) })
		}
	}
	for client, clock := range needs {
		wait.needs[client] = max(wait.needs[client], clock)
	}
	wait.coveredAt = 0
}

// end is the end of origin's connection while origin is listed: the room resyncs it no more.
// Where its frames reach ids whose rows await its socket's mark (parkedMarks), the list keeps,
// keyed by the record itself, its person and the clocks those ids wait on, and lets go of its
// connection and origin, until the store of the commit that lands them marks them (resyncLanded,
// markEndedLanded) or the list drops it; otherwise it drops origin.
func (w *parkedWaits) end(origin any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wait, listed := w.waits[origin]
	if !listed {
		return
	}
	marks := parkedMarks(wait.connection, wait.needs)
	if len(marks) == 0 {
		w.dropLocked(wait)
		w.shrinkLocked()
		return
	}
	needs := make(crdt.StateVector, len(marks))
	for _, client := range marks {
		needs[client] = wait.needs[client]
	}
	delete(w.waits, origin)
	w.endings++
	wait.ended = &endedSender{actor: wait.connection.actor, at: w.endings}
	wait.origin, wait.connection, wait.needs, wait.unwatch = nil, nil, needs, nil
	w.waits[wait] = wait
	w.endedHeld.Add(1)
}

// dropDiscarded drops senders, the browsers whose parked frames a refused drain discarded
// (crdt.DrainDiscardedError.Senders), before closeDiscardedSenders closes them: what they parked
// will not land, so neither a resync nor a mark is owed them, and their connections' end keeps no
// record of them (end). A sender's unwatch does not wait for an end already running, which waits
// for w.mu and then finds nothing listed.
func (a *roomApplies) dropDiscarded(senders []any) {
	if a == nil || a.parked.held.Load() == 0 {
		return
	}
	w := &a.parked
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, sender := range senders {
		if wait, listed := w.waits[sender]; listed {
			w.dropLocked(wait)
		}
	}
	w.shrinkLocked()
}

// boundEnded drops the oldest records of ended connections still waiting (parkedWaits.end) past
// most, the structs and delete ranges the room's queue holds. Each waited on one of those of its
// own sender's as it ended, so more of them than the queue holds wait on what a refused drain
// discarded after their senders had gone, which names none of them (ygo forgets a gone sender's
// origin, crdt.Doc.ForgetParkedOrigin): they would otherwise stay until the queue empties. A room
// keeping no more of them than its queue holds reads one atomic.
func (w *parkedWaits) boundEnded(most int) {
	if int(w.endedHeld.Load()) <= most {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var waiting []*parkedWait
	for _, wait := range w.waits {
		if wait.ended != nil && wait.coveredAt == 0 {
			waiting = append(waiting, wait)
		}
	}
	if len(waiting) <= most {
		return
	}
	slices.SortFunc(waiting, func(a, b *parkedWait) int { return cmp.Compare(a.ended.at, b.ended.at) })
	for _, wait := range waiting[:len(waiting)-most] {
		w.dropLocked(wait)
	}
	w.shrinkLocked()
}

// dropUncovered drops every sender whose frames have not landed: the room's queue holds nothing,
// so nothing they wait on is left to land.
func (w *parkedWaits) dropUncovered() {
	if w.held.Load() == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, wait := range w.waits {
		if wait.coveredAt == 0 {
			w.dropLocked(wait)
		}
	}
	w.shrinkLocked()
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
func (a *roomApplies) landedParked(through uint64) []*parkedWait {
	if a == nil || a.parked.held.Load() == 0 {
		return nil
	}
	w := &a.parked
	w.mu.Lock()
	defer w.mu.Unlock()
	var landed []*parkedWait
	for _, wait := range w.waits {
		if wait.coveredAt != 0 && wait.coveredAt <= through {
			landed = append(landed, wait)
			w.dropLocked(wait)
		}
	}
	w.shrinkLocked()
	return landed
}

// resyncLanded resyncs every browser whose parked frames landed at or before through, a commit
// position of instance through which the room's persistence has just stored the room's commits
// (StoreUpdateStored), so the answer to the SyncStep2 its provider sends back clears the rows the
// editor kept for those frames, and marks the rows of the ids those frames wrote under that await
// its socket's mark (markParkedLanded); for the connections that ended before, it marks the rows
// the room kept of them, all of them together (markEndedLanded). A position of an instance the
// room no longer runs resyncs and marks nothing.
func (s *Service) resyncLanded(room string, instance *roomInstance, through uint64) {
	live := s.srv.GetDoc(room)
	if live == nil || instance == nil || s.instanceOf(live) != instance {
		return
	}
	var ended []*parkedWait
	for _, wait := range s.roomWeighingOf(live).applies().landedParked(through) {
		if wait.ended != nil {
			ended = append(ended, wait)
			continue
		}
		s.srv.ResyncPeer(wait.origin)
		s.markParkedLanded(room, live, wait.connection, wait.needs)
	}
	if len(ended) > 0 {
		s.markEndedLanded(room, ended)
	}
}
