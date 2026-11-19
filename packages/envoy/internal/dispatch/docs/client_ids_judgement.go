package docs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/provider/websocket"
)

// This file holds the client id rule's judgement of each browser update (client_ids.go): the
// refusal or the claims judgeClients makes before the apply, and what settleClients does with
// them once the apply has returned.

// clientJudgement is what judgeClients decided of an update it let through: the ids it claimed
// for the sender, which the hook gives back with the socket's count of them where the apply does
// not stand (settleClients, releaseClaims), and whether the update writes under the socket's own
// given id, whose row the hook marks once an apply has left the room holding a struct under it
// (settleClients, recordLanded).
type clientJudgement struct {
	claimed   []uint64
	writesOwn bool
}

// roomHolds is whether room holds a struct of client: whether its clock of client is past 0. It
// reads that one clock and no other: it asks whether room holds a GC struct of client one clock
// long at clock 0 (crdt.DocContainsUpdateV1Limit, which decodes the probe, a few bytes, and
// compares the struct's end with the store's clock of its client under the room's read lock),
// where the room's state vector would copy an entry for each client it holds - 40,001 in the
// tangled document the keystroke drive types into. The probe is this package's own one struct, so
// the apply's value limit it decodes under, as every decode outside an apply here does, never
// stops it. A parked struct is in the pending queue, not the store, so a room whose structs of
// client only parked holds none. A nil room holds nothing.
func roomHolds(room *crdt.Doc, client crdt.ClientID) bool {
	if room == nil {
		return false
	}
	probe := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1) // one client's structs
		encoder.WriteVarUint(1) // one struct of them
		encoder.WriteVarUint(uint64(client))
		encoder.WriteVarUint(0) // at clock 0
		encoder.WriteUint8(0)   // a GC struct
		encoder.WriteVarUint(1) // one clock long
		encoder.WriteVarUint(0) // and no deletions
	})
	held, err := crdt.DocContainsUpdateV1Limit(room, probe, maxDecodedValuesPerApply)
	return err == nil && held
}

// judgeClients is the rule above for one browser update: it returns the refusal of an update
// carrying structs under a client id that is not its sender's, or carrying, under a claimed id
// that is not its sender's, an edit at clocks the room holds that differs from what it holds
// there, and otherwise what it decided (clientJudgement). An update whose context names no socket
// of this service's admission - a test's call of the hook - is judged by nothing here, and neither
// is one whose header does not read: the apply refuses its bytes.
//
// A keystroke costs a read of its header's first three numbers and nothing more: an update of
// one client's structs under the id the socket was given passes there, and only an update
// carrying another id - a saved row replayed, a sync step 2, a page of the earlier build - reads
// the whole header (crdt.ParseUpdateMeta) and the room's state vector, and asks the table about
// the ids its socket has not shown before that carry clocks the room lacks, and about the ids
// below mintedClientFloor whose structs fall wholly at clocks the room holds (heldDuplicates,
// refusedDuplicates).
func (s *Service) judgeClients(info websocket.PeerUpdateInfo) (judged clientJudgement, refusal error) {
	connection := connectionOf(info.Context)
	if connection == nil || connection.clients == nil {
		return clientJudgement{}, nil
	}
	clients := connection.clients
	if groups, client, read := soleClient(info.Update); read && (groups == 0 || groups == 1 && clients.holds(client)) {
		return clientJudgement{writesOwn: groups == 1 && clients.isOwn(client)}, nil
	}
	from, to, err := crdt.ParseUpdateMeta(info.Update)
	if err != nil {
		return clientJudgement{}, nil
	}
	// A client whose structs the room holds to their end adds nothing - a whole-state handoff
	// carries every client of the history back - so only the clients the update carries clocks of
	// that the room lacks are judged (beyondHeld), and of the rest only a claimed id's duplicates
	// are read (heldDuplicates).
	held := heldBy(info.Doc)
	asked := beyondHeld(held, from, to, clients)
	duplicated := heldDuplicates(held, from, to, clients)
	// The update writes under the socket's own id where its header names that id, and a legacy
	// socket has no id of its own to name (isOwn).
	_, namesOwn := from[clients.own]
	judged.writesOwn = namesOwn && clients.isOwn(clients.own)
	if len(asked) == 0 && len(duplicated) == 0 {
		return judged, nil
	}
	ctx, cancel := context.WithTimeout(info.Context, clientCheckTimeout)
	defer cancel()
	owners, err := s.clientOwners(ctx, info.Room, slices.Concat(asked, duplicated))
	if err != nil {
		return clientJudgement{}, s.clientCheckFailed(info, err)
	}
	if refusal := s.refusedDuplicates(info, connection, owners, duplicated, from, to); refusal != nil {
		return clientJudgement{}, refusal
	}
	var claims, claimFrom []uint64
	for _, client := range asked {
		owner, bound := owners[client]
		switch {
		case bound && !owner.is(connection.actor):
			return clientJudgement{}, s.refuseClient(info, connection, client, "it names another person")
		case !bound && client >= mintedClientFloor:
			return clientJudgement{}, s.refuseClient(info, connection, client, "no admission gave it out")
		case !bound:
			heldTo, err := s.claimable(ctx, info.Room, info.Doc, held, connection.actor, clients.legacy, client)
			if errors.Is(err, errClaimRefused) {
				return clientJudgement{}, s.refuseClient(info, connection, client, err.Error())
			} else if err != nil {
				return clientJudgement{}, s.clientCheckFailed(info, err)
			}
			claims = append(claims, client)
			claimFrom = append(claimFrom, heldTo)
		}
	}
	if len(claims) > 0 {
		if !clients.mayClaim(len(claims)) {
			return clientJudgement{}, s.refuseClient(info, connection, claims[0],
				fmt.Sprintf("its socket holds claims of %d ids, the most it may", maxClaimsASocket))
		}
		judged.claimed, err = s.claimClients(ctx, info.Room, connection.actor, claims, claimFrom)
		if errors.Is(err, errClaimRefused) {
			return clientJudgement{}, s.refuseClient(info, connection, claims[0], err.Error())
		} else if err != nil {
			return clientJudgement{}, s.clientCheckFailed(info, err)
		}
		if len(judged.claimed) < len(claims) {
			// Another socket claimed some of them between the read and this claim: they are
			// this update's only if that socket was this person's.
			owners, err := s.clientOwners(ctx, info.Room, claims)
			if err != nil {
				s.releaseClaims(info.Room, connection.actor, judged.claimed)
				return clientJudgement{}, s.clientCheckFailed(info, err)
			}
			for _, client := range claims {
				if owner := owners[client]; !owner.is(connection.actor) {
					s.releaseClaims(info.Room, connection.actor, judged.claimed)
					return clientJudgement{}, s.refuseClient(info, connection, client, "another person claimed it first")
				}
			}
		}
		clients.countClaims(len(judged.claimed))
	}
	clients.confirm(asked)
	return judged, nil
}

// refusedDuplicates is the refusal of update for its structs under duplicated, the ids below
// mintedClientFloor whose structs fall wholly at clocks the room holds (heldDuplicates), whose
// rows owners holds. Of those, the ids another person claimed whose structs reach into the claim
// (clientOwner.claimedAgainst) are what a tab typed at clocks someone else claimed first, which the
// apply would drop as duplicates and say nothing of. Where the update carries them under at most
// maxComparedClaims ids - a keystroke in flight, a saved row replayed, a resend - each is compared
// (refusedDuplicate). Past that it is a whole-state handoff of the history, which carries what the
// room relayed: a page of this build's is neither compared nor refused for it, and a legacy
// socket's is refused whole, since an honest legacy tab writes under one id. So no update pays
// more than maxComparedClaims comparisons, whatever the room's claims.
func (s *Service) refusedDuplicates(info websocket.PeerUpdateInfo, connection *connectionState, owners map[uint64]clientOwner, duplicated []uint64, from, to crdt.StateVector) error {
	var claimed []uint64
	for _, client := range duplicated {
		if owners[client].claimedAgainst(connection.actor, to[crdt.ClientID(client)]) {
			claimed = append(claimed, client)
		}
	}
	if len(claimed) > maxComparedClaims {
		if !connection.clients.legacy {
			return nil
		}
		return s.refuseClient(info, connection, claimed[0], fmt.Sprintf(
			"it carries structs at clocks the room holds under %d ids other people claimed, and a socket that presents no client id writes under one", len(claimed)))
	}
	for _, client := range claimed {
		if refusal := s.refusedDuplicate(info, connection, owners[client], client, from); refusal != nil {
			return refusal
		}
	}
	return nil
}

// refusedDuplicate is the refusal of update's structs under client, an id another person claimed
// whose structs in update fall wholly at clocks the room holds, where they differ from what the
// room holds at the same clocks from the claim on (crdt.HeldStructsDifferV1Limit): an edit typed
// at a clock another person claimed first. It is nil for an update that hands back the room's own
// structs, as a browser relayed them. An update whose structs do not decode is the apply's to
// refuse.
//
// The comparison decodes the whole update into a document of its own, which no apply's budget
// reaches, so it decodes under the apply's own value limit (maxDecodedValuesPerApply), and an
// update past it (crdt.ErrValueLimit) is refused as the apply refuses one past it,
// DOC_TOO_LARGE (ruleDecodedValues): the apply would decode the same values and stop at the same
// count.
func (s *Service) refusedDuplicate(info websocket.PeerUpdateInfo, connection *connectionState, owner clientOwner, client uint64, from crdt.StateVector) error {
	id := crdt.ClientID(client)
	differs, err := crdt.HeldStructsDifferV1Limit(info.Doc, info.Update, id, max(from[id], owner.claimedFrom), maxDecodedValuesPerApply)
	if errors.Is(err, crdt.ErrValueLimit) {
		refusal := ruleDecodedValues.refusal(false)
		logRefusal(info, refusal, "dispatch: refused a browser's update its apply may not take", "rule", ruleDecodedValues.why(),
			"decode_limit", maxDecodedValuesPerApply, "decoded_by", "the comparison of a claimed id's structs with the room's")
		return refusedWith(refusal, err)
	}
	if err != nil || !differs {
		return nil
	}
	return s.refuseClient(info, connection, client, "it carries an edit at clocks another person claimed first that differs from what the room holds there")
}

// beyondHeld is the clients of an update, from and to as crdt.ParseUpdateMeta reads them, that
// clients does not hold as the socket's own and whose structs reach past held, what the room
// holds: the clients the update can land anything under. Structs the room holds already are
// duplicates that land nothing whoever sends them, so they are not judged for their id, and the
// ids of a history a browser hands back whole cost no lookup.
func beyondHeld(held, from, to crdt.StateVector, clients *socketClients) []uint64 {
	var beyond []uint64
	for client := range from {
		if !clients.holds(client) && to[client] > held[client] {
			beyond = append(beyond, uint64(client))
		}
	}
	return beyond
}

// heldDuplicates is the clients of an update below mintedClientFloor that clients does not hold
// as the socket's own and whose structs all fall at clocks held, what the room holds, already:
// duplicates the apply would land nothing of, which are compared with the room's where another
// person claimed the id (refusedDuplicates). A client whose structs reach past held is
// beyondHeld's.
func heldDuplicates(held, from, to crdt.StateVector, clients *socketClients) []uint64 {
	var duplicated []uint64
	for client, start := range from {
		if client < mintedClientFloor && start < held[client] && to[client] <= held[client] && !clients.holds(client) {
			duplicated = append(duplicated, uint64(client))
		}
	}
	return duplicated
}

// heldBy is what room holds, its state vector; a nil room, one not loaded, holds nothing.
func heldBy(room *crdt.Doc) crdt.StateVector {
	if room == nil {
		return nil
	}
	return room.StateVector()
}

// settleClients is what the hook does with judged once the apply has returned, stood saying
// whether its update stood. An apply that did not stand gives back the claims its update made
// (releaseClaims), with the socket's count of them. A claim whose apply stood is kept, whether its
// structs integrated or only parked behind a clock the room lacks: it binds the id to its claimant,
// which is what keeps another person from parking a struct under that id at a clock its tab types
// next and having the tab's own keystroke there dropped as a duplicate once the gap fills. The
// rows such claims add are bounded by maxClaimsASocket and maxClaimsADay, on every kind of socket.
//
// The first apply of a socket after which the room holds a struct under the socket's own id, from
// an update writing under it, marks that id's row (recordLanded), unless the row said an edit had
// landed as the socket was admitted: an update whose structs under the id only parked leaves the
// room holding none of them and the row unmarked, so a later mint may still give it back once its
// socket has gone. It asks the room for that one client's clock (roomHolds), never for its state
// vector, which would copy every client the room holds on every keystroke, and only until the
// mark is written. A socket makes one mark at a time (socketClients.marking), and only a written
// one sets landed: a mark the store refused leaves the socket presenting its row (keepPresenting),
// and the next such apply marks it again.
func (s *Service) settleClients(info websocket.PeerUpdateInfo, judged clientJudgement, stood bool) {
	connection := connectionOf(info.Context)
	if connection == nil || connection.clients == nil {
		return
	}
	clients := connection.clients
	if !stood {
		if len(judged.claimed) > 0 {
			clients.forget(judged.claimed)
			clients.countClaims(-len(judged.claimed))
			s.releaseClaims(info.Room, connection.actor, judged.claimed)
		}
		return
	}
	if judged.writesOwn && !clients.landed.Load() && roomHolds(info.Doc, clients.own) &&
		clients.marking.CompareAndSwap(false, true) {
		go func() {
			defer clients.marking.Store(false)
			if s.recordLanded(info.Room, connection.actor, clients.own) {
				clients.landed.Store(true)
			}
		}()
	}
}

// refuseClient is the refusal of an update carrying an edit under client - structs, or a delete
// range that would park (parkJudge) - which is not the sender's for why.
func (s *Service) refuseClient(info websocket.PeerUpdateInfo, connection *connectionState, client uint64, why string) *websocket.PeerClose {
	refusal := documentClientIDRefused()
	logRefusal(info, refusal, "dispatch: refused a browser's update carrying an edit under a client id that is not its sender's",
		"client", client, "why", why, "actor", ActorRef(connection.actor), "own_client", uint64(connection.clients.own),
		"legacy_page", connection.clients.legacy)
	return refusal
}

// clientCheckFailed is the refusal of an update whose client ids the table could not be read or
// written for: transient, DocumentBusyCloseCode, so the editor keeps the edit and sends it again.
func (s *Service) clientCheckFailed(info websocket.PeerUpdateInfo, err error) error {
	refusal := documentBusy()
	logRefusal(info, refusal, "dispatch: could not judge a browser's update's client ids; refused it for a retry",
		"error", err.Error())
	return refusedWith(refusal, err)
}

// soleClient reads the start of a V1 update - how many client groups its structs come in and the
// client of the first - without decoding a struct or allocating. read is false where those
// numbers do not read, which leaves the update to crdt.ParseUpdateMeta.
func soleClient(update []byte) (groups uint64, client crdt.ClientID, read bool) {
	groups, rest, ok := readUvarint(update)
	if !ok || groups != 1 {
		return groups, 0, ok
	}
	if _, rest, ok = readUvarint(rest); !ok {
		return groups, 0, false
	}
	first, _, ok := readUvarint(rest)
	return groups, crdt.ClientID(first), ok
}

// readUvarint reads one lib0 varuint from the start of b, as binary.Uvarint does (the two
// encodings are the same), and the bytes after it.
func readUvarint(b []byte) (uint64, []byte, bool) {
	value, read := binary.Uvarint(b)
	if read <= 0 {
		return 0, nil, false
	}
	return value, b[read:], true
}

// connectionOf is the document socket ctx was served on, as its admission recorded it, or nil
// for a context no admission made.
func connectionOf(ctx context.Context) *connectionState {
	if ctx == nil {
		return nil
	}
	connection, _ := ctx.Value(connectionContextKey{}).(*connectionState)
	return connection
}
