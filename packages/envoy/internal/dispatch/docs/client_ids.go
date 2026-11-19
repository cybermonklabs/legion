package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// This file holds the client id rule itself and its storage: the rule's bounds, the claim of an
// id below mintedClientFloor (claimable, claimClients, releaseClaims), the stored history's names
// the claim reads (storedClients), the rows that say whose each id is (clientOwners), and the
// marks and the sweep of the rows given out (recordLanded, sweepUnusedClientIDs). The rest of the
// rule is in three files beside it: the admission that gives a page its id
// (client_ids_admission.go), the judgement of each browser update (client_ids_judgement.go), and
// the frame that hands the page its id (client_ids_handshake.go).

// A Yjs update names the client each of its structs belongs to, and the client id is the
// browser's own choice. Before this file nothing tied a client id to anyone, so a signed-in
// person could write under another's id at the clock that person typed next: the room took the
// forged structs, the other person's own keystroke at that clock then arrived as a duplicate and
// was dropped, the other person stayed connected and was told nothing, and their next keystroke
// landed inside the forged text (dispatch://INFRA-179).
//
// So Dispatch gives the client ids out, and a browser writes under no other. A document socket's
// admission (ServeHTTP) gives its page a client id for that document, stored with the person the
// socket is signed in as (document_client_ids, minted) where the page may write, before the page
// syncs or announces anything: the page presents the id it holds under DocumentClientIdParameter,
// and the admission gives it back where it was given to the same person, and gives out a fresh one
// otherwise. The answer is the first frame on the socket, a stateless message
// (DocumentClientIdMessagePrefix) written right after the upgrade's 101 (clientIDConn): the page
// cannot read the upgrade's headers, and y-protocols' awareness takes the document's client id as
// it is made, so the page needs the id before its provider sends anything of its own. The same
// tab keeps its id across every reconnect of one document - its offline edits are under it - and
// takes a fresh one only with a fresh document, after a refusal the editor rebuilds for.
//
// The rule, judged on each browser update before its apply (judgeClients): an update carrying
// structs under a client id stored for another person is refused whole, with
// DocumentClientIdRefusedCloseReason, and so is one carrying structs under an id at
// mintedClientFloor or above that no admission gave out. A delete of clocks the room holds is not
// judged: a person deletes other people's text, and a delete set names the deleted items' clients.
// A delete past the clocks the room holds of a client is judged where it would park, inside the
// apply (parkJudge, crdt.ApplyBudget.MayParkDeletes): it parks only under a client the socket
// holds, and under any other the whole update is refused with the same close, since what it
// parked would delete that person's next keystrokes as they land while that person is answered
// saved. The stored row is the binding, so a reload, a restart and every other process read the
// same one, and the person a row names never changes, so a binding read at any moment before an
// apply is the binding at that apply.
//
// A sync step 2 is judged as an incremental update is: ygo hands the hook both alike, and a large
// one is judged inside the load slot its apply takes (weighPeerUpdate). Structs the room already
// holds land nothing whoever sends them, so a step 2 that hands back what the room holds passes
// (beyondHeld, heldDuplicates). One that carries another person's structs past what the room
// holds - an edit the room relayed to the sender and then lost before its store - is refused
// whole, and nothing of it lands: the person whose edit it was resends it from their own browser,
// which kept it unsaved, and the sender's editor rebuilds the document from the room and replays
// its own saved edits. A step 2 answered applied with those structs taken out of it would tell
// the sender's editor that every edit it had recorded was saved, and the editor would delete them.
//
// Dispatch draws the ids it gives out at mintedClientFloor or above. A browser of a build from
// before this one drew its own below it, as Yjs does (lib0 random.uint32), and so does every
// write of the server's own: ygo draws a document's client id the same way (crdt.NewClientID),
// for a room, a write's fork, and the fresh id a document takes when a remote apply advances its
// own clock (reearth/ygo #340). So no id Dispatch gives a browser can be one the server writes
// under, and no server write can be judged here: it reaches the room by no browser's socket.
//
// The deploy left ids below the floor that are still browsers'. A tab of the earlier build, open
// across the deploy or reloaded by the reader once it is over, holds the edits it typed under its
// own id, saved in the browser (pending-sync.ts), and replays each saved row under the client id
// it was typed under; and a client that presents no DocumentClientIdParameter at all - a page of
// that build, agent-infra's measure job - is given no id (legacy) and writes under the one it
// drew for itself. So an id below the floor that no row names may be claimed - before the apply
// (claimClients), by the person whose browser update carries structs under it first, or at the
// admission of a page of this build that presents it (keepsClient) - and is that person's from
// then on; an update's claim is given back where its apply does not stand, and kept where it
// stands, its structs parked or not (settleClients, releaseClaims). Every claim, on every kind of
// socket, is counted from the stored rows among the person's claims in the document in the UTC
// day, and none is stored past maxClaimsADay; the claims one socket's updates hold are held to
// maxClaimsASocket besides. Past either an update is refused and nothing it carries is claimed,
// and an admission keeps no id it would claim, and gives its page one afresh. A claim is held to
// three rules besides (claimable):
//
//   - An id the stored history does not name, and the room does not hold, may be claimed by a
//     legacy socket at any time, so the clients deployed without an id keep writing: no edit the
//     document holds is under it, so a forged claim of it takes nothing from anyone. No window
//     ends these claims, and the two bounds above are what bound them.
//   - An id the stored history names, or the room holds, is a writer's. It may be claimed only
//     until claimWindow has passed since this database first served a build that gives ids out
//     (document_client_ids_since), the architect's bound on the deploy, and only by a person a
//     version or a pending author row lists as having edited the document since the history
//     first named it: the writer was credited for what it typed, so the race the claim leaves is
//     among the document's co-authors of that time and no longer every reader.
//   - A page of this build has an id of its own, and claims another - a saved row of the earlier
//     build that it replays, or, at its admission, the id its document drew while its first
//     socket reached a process of the earlier build - only until claimWindow has passed, whether
//     the history names the id or not.
//
// Which ids the stored history names is read once per room instance, at the first claim that
// needs it, and every commit the room takes from then on adds its own (storedClients): a claim
// into a loaded room reads no history once the room has read it, and only an admission's claim,
// made before its room loads, reads it afresh.
//
// The race a claim leaves is not refused, but it loses nothing in silence. Another person who
// claims a tab's id first, writing at the clock the tab writes next - a co-author reading the id
// off the history, or, for an id the history does not name, any reader reading it off the room's
// awareness, where a legacy page announces its presence under its own id before it types - leaves
// the room holding that clock, and the tab's own edit at it then arrives as a duplicate: the apply
// lands nothing of it and says nothing, and the tab would believe it saved. So an update carrying
// structs under a claimed id below the floor that is not its sender's, at clocks the room already
// holds from the claim on, has them compared with what the room holds there
// (crdt.HeldStructsDifferV1Limit, refusedDuplicates), and is refused
// DocumentClientIdRefusedCloseReason where they differ, from any socket and whether or not the
// claim window is open. An equal resend - any browser handing back what the room relayed -
// passes. The row keeps the id's clock in the document as the claim was made (claimed_from), so
// only clocks the claim could have written are compared, never the tab's own history before it.
// Each comparison decodes the whole update, under the apply's own value limit, so an update is
// compared only where it carries such structs under at most maxComparedClaims claimed ids, as a
// keystroke in flight, a resend or a saved row replayed does; a whole-state handoff of the
// history, which carries the structs of every claim the room holds, is not compared, and is
// refused whole from a legacy socket alone, since an honest legacy tab writes under one id. Only an
// update carrying structs under an id below the floor that its socket does not hold and that fall
// wholly at clocks the room holds pays the one lookup of their ids' rows that finds the claims
// among them, never a keystroke under the socket's own id.
//
// A row is stored only for a page that may write. A read-only admission - a closed issue, a page
// the live fleet does not admit - is decided before its id (ServeHTTP, decideWritable), and is
// given back an id the table names its person for or else sent one drawn at mintedClientFloor or
// above that is stored nowhere and claims nothing: its page cannot write under it, and a page
// admitted writable later presents it, finds no row and is given one afresh, which its document
// takes while it has written nothing under the first (connection.ts, takeClientId). A request
// that never becomes a socket - one ygo's upgrade refuses, whatever it refuses it for
// (refusedAtUpgrade) - stores nothing either.
//
// Every writable page load is given an id, and most never type. So a person holds at most
// maxUnlandedMintsADocument rows in a document that no edit landed under: a mint past that gives
// back, in the same statement, the person's row there least recently presented (mintClient), and
// where each of them is live the admission is refused for now, DocumentBusyCloseCode, which the
// editor retries. A row is live while an open socket holds it: the socket presents it again every
// clientPresenceInterval until an edit lands under it (keepPresenting), and a row presented within
// clientPresenceLapses intervals is no mint's to give back, on this process or another. A page
// whose row was given back is given a fresh id at its next admission, and loses only what it
// typed offline under the old one that never landed. A row no edit landed under that nothing has
// presented for unusedClientIDAge is deleted by the hourly compaction sweep
// (sweepUnusedClientIDs). A row whose id landed an edit is neither given back nor swept, and
// neither is a claimed row: a socket marks its own row once the room holds a struct under the id
// after one of its applies (recordLanded), which a frame that only parked leaves it holding none
// of, and a claim is stored as landed. A claim is bounded by the claim window and the claims'
// bounds rather than swept: a swept claim's id could be claimed by another person while the tab
// that holds it is still open and writing under it.

const (
	// mintedClientFloor and mintedClientCeiling bound the client ids an admission gives out:
	// above every id a browser of an earlier build or the server itself draws (a uint32), and
	// within the five bytes of varuint a Yjs id of 2^28 or more already takes, so a given id
	// costs the encoded state no byte more than the ids browsers drew for themselves.
	mintedClientFloor   = 1 << 32
	mintedClientCeiling = 1 << 35
	// mintTries is how many ids an admission draws before it gives up: a draw of an id the
	// document has given out already is refused by the table's key and drawn again, and with
	// 30 billion ids to draw from, a second try is already a rarity.
	mintTries = 8
	// clientCheckTimeout bounds each read and write of the bindings an admission or a browser's
	// update makes. The update's wait is on the peer's own read loop, so it holds that one
	// browser and no other; past it, the update is refused transiently and the browser retries.
	clientCheckTimeout = 5 * time.Second
	// maxConfirmedClients is the most client ids one socket remembers as its own beyond the one
	// it was given (socketClients.confirmed): the ids of the saved rows its page replays, each
	// read from the table once rather than once a frame. Past it an id is read again on each
	// update that carries it, which costs a query and changes no answer: it is the size of a
	// cache, and bounds nothing anyone stores.
	maxConfirmedClients = 64
	// maxClaimsASocket is the most claims one socket's updates hold (socketClients.claims), on
	// every kind of socket: each claim an update made that was not given back (settleClients). An
	// honest page claims one id for each tab of the earlier build whose saved rows its browser
	// replays, which is a few.
	maxClaimsASocket = 64
	// claimWindow is how long after document_client_ids_since an id below mintedClientFloor that
	// the stored history names may still be claimed, and any id by a page of this build: the
	// architect's bound on how long a tab of the earlier build, or the edits it saved, may take to
	// come back. A legacy socket's claim of an id the history does not name has no window.
	claimWindow = 30 * 24 * time.Hour
	// maxClaimsADay is the most claims one person makes in one document in one UTC day, counted
	// from the stored rows: every claim, on every kind of socket, an update's and an admission's
	// alike (claimClients). With maxClaimsASocket and claimWindow, the bound on the claimed rows,
	// which no sweep takes.
	maxClaimsADay = 64
	// maxUnlandedMintsADocument is the most rows one person holds in one document of ids an
	// admission gave out that no edit has landed under (mintClient). The most ids given to one
	// person in one document over every honest drive of round 9 was 5
	// (dispatch://INFRA-179/artifact/infra179-r9-caps-measure-md).
	maxUnlandedMintsADocument = 64
	// clientPresenceInterval is how often an open socket presents its own id again while no edit
	// has landed under it (keepPresenting), and clientPresenceLapses how many such intervals a row
	// may go unpresented and still be live: no mint gives back a row presented more recently than
	// that (mintClient). Three intervals leave room for two presentations that came late or
	// failed before the row of a socket still open could be given back.
	clientPresenceInterval = 5 * time.Minute
	clientPresenceLapses   = 3
	// maxComparedClaims is the most ids another person claimed whose structs at clocks the room
	// holds one update has compared (refusedDuplicates): a keystroke in flight, a resend or a saved
	// row replayed carries one. An update past it is a whole-state handoff of the history, which
	// would otherwise pay a read of itself for every claim the room holds.
	maxComparedClaims = 4
	// unusedClientIDAge is how long a row whose id never landed an edit stays after the last
	// admission that presented it.
	unusedClientIDAge = 30 * 24 * time.Hour
)

// clientClaimsLockNamespace is the first key of the lock every claim takes ("CLMS"), its second
// the hashtext of the document and the person, so two claims of one person in one document count
// each other's rows (claimClients). A mint takes the same first key with a second of its own, the
// hashtext of "mints", the document and the person, so two mints of one person in one document
// count each other's rows (mintClient): the two keys meet only where the hashes collide, which
// makes one wait for the other's statement and changes no count. It is the two-key form, which
// Postgres keeps apart from the single-key locks (a document's room, lockDocumentRoom, among
// them), and it is no other first key: neither conditionalEditQueueLockNamespace's own nor any of
// the others that lock's comment lists, which names this one among them. The statements that take
// it name themselves client_claims_lock and client_mints_lock and spell their key's text type, as
// the owner lock's do (store/ownerlock.go): the copy lock (projectCopiesLock) is taken by the same
// two-key call, and the copy-lock guard (api's TestCopyLockComesBeforeEveryPendingRowAndEvent) and
// the copy-lock waits a test reads off pg_stat_activity tell that lock by its statement's text,
// which these would otherwise share.
const clientClaimsLockNamespace int32 = 0x434C4D53

// clientOwner is the person a client id is stored for, and for an id claimed rather than given
// out, the clock the document held it to as the claim was made (claimedFrom).
type clientOwner struct {
	kind, id    string
	claimed     bool
	claimedFrom uint64
}

func (o clientOwner) is(actor model.Actor) bool {
	return o.kind == actor.Kind && o.id == actor.ID
}

// claimedAgainst is whether the row is a claim of another person's than actor's that an update's
// structs under the id, ending at clock to, reach into: structs from claimedFrom on are at clocks
// the claim could have written. An id no row names is no one's claim.
func (o clientOwner) claimedAgainst(actor model.Actor, to uint64) bool {
	return o.claimed && !o.is(actor) && to > o.claimedFrom
}

// errClaimRefused is a claim of an id below mintedClientFloor that the rule refuses (claimable,
// and claimClients for a person's daily bound).
var errClaimRefused = errors.New("dispatch: the client id may not be claimed")

// RecordClientIDsSince records, once, that this database now serves a build that gives client ids
// out: the first process of such a build to boot writes the one row of document_client_ids_since,
// and every later boot, of this process or another, leaves it as it is. Its time is where
// claimWindow counts from. A boot that cannot write it logs and serves on; the ids the window
// bounds stay claimable until a boot records it.
func (s *Service) RecordClientIDsSince(ctx context.Context) error {
	if _, err := s.store.Pool.Exec(ctx, `
		insert into document_client_ids_since (since) values (now()) on conflict do nothing
	`); err != nil {
		return fmt.Errorf("record when this database first served a build that gives client ids out: %w", err)
	}
	return nil
}

// claimWindowOpen is whether claimWindow is open: no boot has recorded document_client_ids_since
// yet, or claimWindow has not passed since it.
func (s *Service) claimWindowOpen(ctx context.Context) (bool, error) {
	since := s.clientIDsSince.Load()
	if since == nil {
		var read time.Time
		err := s.store.Pool.QueryRow(ctx, `select since from document_client_ids_since`).Scan(&read)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read when this database first served a build that gives client ids out: %w", err)
		}
		since = &read
		s.clientIDsSince.Store(since)
	}
	return time.Since(*since) < claimWindow, nil
}

// claimable is nil where actor may claim client, an id below mintedClientFloor no row names, in
// room, whose live document holds held, and errClaimRefused, saying why, where it may not: the
// claim window has closed for a page of this build (legacy false), whatever the id, and for an id
// the stored history names (historyNames) or held holds; or the window is open, the id is one of
// those, and neither a version holding the history's first row naming it nor a pending author row
// lists actor. heldTo is the clock the document holds client to, where a claim made now starts
// (claimed_from).
//
// A claim the closed window refuses whatever the history says - a page of this build's, or one of
// an id the room holds - reads no history; every other claim reads it once per room instance
// (historyNames).
func (s *Service) claimable(ctx context.Context, room string, live *crdt.Doc, held crdt.StateVector, actor model.Actor, legacy bool, client uint64) (heldTo uint64, err error) {
	open, err := s.claimWindowOpen(ctx)
	if err != nil {
		return 0, err
	}
	id := crdt.ClientID(client)
	if !open && (!legacy || held[id] > 0) {
		return 0, claimWindowClosed()
	}
	from, storedTo, named, err := s.historyNames(ctx, room, live, id)
	if err != nil {
		return 0, err
	}
	if !named && held[id] == 0 {
		return 0, nil
	}
	if !open {
		return 0, claimWindowClosed()
	}
	// A co-author's edit names them in a pending author row once its append lands, so the room's
	// appends are asked for at once, and waited for, before the authors are read
	// (waitForDurableAppends): an edit the room took a moment ago lists its author.
	if err := s.waitForDurableAppends(ctx, room); err != nil {
		return 0, err
	}
	author, err := json.Marshal([]map[string]string{{"kind": actor.Kind, "id": actor.ID}})
	if err != nil {
		return 0, err
	}
	var listed bool
	if err := s.store.Pool.QueryRow(ctx, `
		select exists (
			select 1 from artifact_versions
			where artifact_id = $1 and doc_update_version >= $2 and authors @> $3::jsonb
		) or exists (
			select 1 from doc_pending_authors where artifact_id = $1 and actor_kind = $4 and actor_id = $5
		)
	`, room, from, string(author), actor.Kind, actor.ID).Scan(&listed); err != nil {
		return 0, fmt.Errorf("read the document's authors: %w", err)
	}
	if !listed {
		return 0, fmt.Errorf("%w: the document's history names it, and no version since lists the claimant", errClaimRefused)
	}
	return max(storedTo, held[id]), nil
}

// claimWindowClosed is the refusal of a claim claimWindow bounds once it has passed.
func claimWindowClosed() error {
	return fmt.Errorf("%w: its window closed %s after this database first gave client ids out", errClaimRefused, claimWindow)
}

// historyNames is what room's stored history says of client (storedClients.names): whether it,
// or a commit the room took since it was read, names client; the doc_updates version the versions
// that may hold client's first struct start from; and the furthest clock a stored or parked struct
// of client reaches. It is read once per room instance and kept with live's instance, where live
// is one a load listed (instanceOf); otherwise - an admission's claim, made before its room loads
// - it is read afresh and kept nowhere.
func (s *Service) historyNames(ctx context.Context, room string, live *crdt.Doc, client crdt.ClientID) (from int64, heldTo uint64, named bool, err error) {
	set := &storedClients{}
	if live != nil {
		if instance := s.instanceOf(live); instance != nil {
			set = &instance.storedClients
		}
	}
	if err := set.readOnce(ctx, s, room); err != nil {
		return 0, 0, false, err
	}
	from, heldTo, named = set.names(client)
	return from, heldTo, named, nil
}

// storedClients is the client ids the stored history behind one room instance names, kept on
// the instance (roomInstance.storedClients) for the claim rule: read once, at the first claim
// that needs it (readOnce), and from then on added to by every commit the instance takes (took),
// as the room's update observer classifies it, which is before the append of every update the
// room stores itself - so it covers what the room took and has not stored yet as well. It goes
// with the instance: the room's unload; a rebuild, which replaces the history only once no room of
// it is resident here (evictBeforeRebuild closes one near a cap) and whose catch-up closes the
// room of another process (roomCatchUps.closeReplaced); and a reload, which is a new instance and
// reads the history afresh.
//
// The room budget already counts what bounds it. An entry is a client id with at least one struct
// in the stored history the instance holds or in a commit the instance took, and the instance
// holds every such struct, in its store or parked, charged bytesOfHeapAStruct in each of its
// roomCopies copies (roomWeight). An entry, a map slot of a client id and a namedClient, measured
// at most 66 heap bytes (56 from a thousand entries on, Go 1.27), under 6% of the 1,260 bytes that
// charge puts on each struct.
type storedClients struct {
	// reading is held by the read of the stored history, so a claim that finds a read under way
	// waits for it instead of reading the history a second time.
	reading sync.Mutex
	// mu guards the rest: the room's update observer adds to named while a claim reads it.
	mu sync.Mutex
	// read says the stored history has been read into named, through version through.
	read    bool
	through int64
	// named is nil until a claim needs the set, and from then on each client id it names.
	named map[crdt.ClientID]namedClient
}

// namedClient is what the stored history says of one client id: from, the doc_updates version
// the versions that may hold its first struct start from - the version of the first stored row
// whose structs name it, or 0 where that row is the history's first, which a compaction may have
// folded every earlier row into, so the struct can be older than the row, or -1 for an id only a
// commit the room took since the set was made names - and heldTo, the furthest clock a stored row
// or a parked struct of it reaches.
type namedClient struct {
	from   int64
	heldTo uint64
}

// readOnce reads the stored history into the set, once: the commits the room takes from the
// first call on are added as they come (took), and the read's rows are merged in when it returns,
// so a commit made since the first call is among the rows read or added, or both. A read that
// fails leaves no set, and the next claim reads again.
func (h *storedClients) readOnce(ctx context.Context, s *Service, room string) error {
	h.reading.Lock()
	defer h.reading.Unlock()
	h.mu.Lock()
	done := h.read
	if !done && h.named == nil {
		h.named = make(map[crdt.ClientID]namedClient)
	}
	h.mu.Unlock()
	if done {
		return nil
	}
	names, through, err := s.readHistoryClients(ctx, room)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.named = nil
		return err
	}
	for client, took := range h.named {
		entry, stored := names[client]
		if !stored {
			entry = took
		}
		entry.heldTo = max(entry.heldTo, took.heldTo)
		names[client] = entry
	}
	h.named, h.read, h.through = names, true, through
	return nil
}

// names is what the set says of client (historyNames). An id only a commit since the read names
// is stored past the read, if at all, and so is the first struct of one the set does not name, so
// either's versions start after the last version read.
func (h *storedClients) names(client crdt.ClientID) (from int64, heldTo uint64, named bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, named := h.named[client]
	if !named || entry.from < 0 {
		entry.from = h.through + 1
	}
	return entry.from, entry.heldTo, named
}

// took adds the client ids one commit of the room's instance names - update, what it integrated,
// and parked, what it parked (crdt.LoggedUpdate) - once a claim has made the set (readOnce).
// Before then nothing is added: the read that makes the set finds a commit stored by then, and
// the room's state vector, which a claim reads beside the set (claimable), holds what any commit
// integrated. A keystroke under an id the set names costs a read of its header's first three
// numbers (soleClient) and a map lookup.
func (h *storedClients) took(update, parked []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.named == nil {
		return
	}
	h.addLocked(update, true)
	if len(parked) > 0 {
		h.addLocked(parked, false)
	}
}

// addLocked adds the clients update's structs name. integrated says the room holds them in its
// store, whose state vector a claim reads for their clocks (claimable), so only a parked struct's
// end is kept (heldTo). Its caller holds h.mu.
func (h *storedClients) addLocked(update []byte, integrated bool) {
	if groups, client, read := soleClient(update); read && integrated && groups <= 1 {
		if _, named := h.named[client]; groups == 1 && !named {
			h.named[client] = namedClient{from: -1}
		}
		return
	}
	from, to, err := crdt.ParseUpdateMeta(update)
	if err != nil {
		return
	}
	for client := range from {
		entry, named := h.named[client]
		if !named {
			entry.from = -1
		}
		if !integrated {
			entry.heldTo = max(entry.heldTo, to[client])
		}
		h.named[client] = entry
	}
}

// readHistoryClients reads every stored row of room and answers the client ids their structs name
// (namedClient) and the last version it read. A row whose header does not read names nothing.
func (s *Service) readHistoryClients(ctx context.Context, room string) (map[crdt.ClientID]namedClient, int64, error) {
	rows, err := s.store.Pool.Query(ctx, `select version, update from doc_updates where artifact_id = $1 order by version`, room)
	if err != nil {
		return nil, 0, fmt.Errorf("read the document's history: %w", err)
	}
	defer rows.Close()
	names := make(map[crdt.ClientID]namedClient)
	var through int64
	read := 0
	for rows.Next() {
		var version int64
		// The row's bytes are the driver's until the next row: the header is read and dropped.
		var update pgtype.DriverBytes
		if err := rows.Scan(&version, &update); err != nil {
			return nil, 0, fmt.Errorf("read the document's history: %w", err)
		}
		if from, to, metaErr := crdt.ParseUpdateMeta(update); metaErr == nil {
			for client := range from {
				entry, named := names[client]
				if !named && read > 0 {
					entry.from = version
				}
				entry.heldTo = max(entry.heldTo, to[client])
				names[client] = entry
			}
		}
		read++
		through = version
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read the document's history: %w", err)
	}
	if s.onHistoryRead != nil {
		s.onHistoryRead(room, read)
	}
	return names, through, nil
}

// clientOwners is the person the table names for each of clients in room, with where a claimed
// id's claim started; a client no row names is absent.
func (s *Service) clientOwners(ctx context.Context, room string, clients []uint64) (map[uint64]clientOwner, error) {
	rows, err := s.store.Pool.Query(ctx, `
		select client_id, actor_kind, actor_id, minted, claimed_from from document_client_ids
		where artifact_id = $1 and client_id = any($2)
	`, room, clientIDs(clients))
	if err != nil {
		return nil, fmt.Errorf("read the client ids' owners: %w", err)
	}
	defer rows.Close()
	owners := make(map[uint64]clientOwner, len(clients))
	for rows.Next() {
		var client int64
		var minted bool
		var claimedFrom *int64
		var owner clientOwner
		if err := rows.Scan(&client, &owner.kind, &owner.id, &minted, &claimedFrom); err != nil {
			return nil, fmt.Errorf("read the client ids' owners: %w", err)
		}
		owner.claimed = !minted
		if claimedFrom != nil {
			owner.claimedFrom = uint64(*claimedFrom)
		}
		owners[uint64(client)] = owner
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the client ids' owners: %w", err)
	}
	return owners, nil
}

// claimClients stores clients, ids no admission gave out, as actor's in room, each claimed from
// the clock at the same index of from (claimed_from), marked as having landed an edit, so no sweep
// takes it: an update's claim is given back unless its apply stands (settleClients,
// releaseClaims), and an admission's is bounded by the claim window and maxClaimsADay
// (keepsClient). It returns the ones this call stored: an id another call stored first is not
// among them, and the caller reads who that was.
//
// Every claim is held to maxClaimsADay, on every kind of socket: where actor's claims stored in
// room since the start of the UTC day (created_at, not minted) and these come to more, none of
// them is stored and the call answers errClaimRefused. The count and the insert are one statement
// behind the lock of actor's claims in room (clientClaimsLockNamespace), in the one implicit
// transaction a batch runs in, so two calls of one person count each other's rows; the statement
// reads the count off document_client_ids_claims.
func (s *Service) claimClients(ctx context.Context, room string, actor model.Actor, clients, from []uint64) ([]uint64, error) {
	batch := &pgx.Batch{}
	batch.Queue(`select pg_advisory_xact_lock($1, hashtext($2::text)) as client_claims_lock`, clientClaimsLockNamespace, room+" "+ActorRef(actor))
	var today int64
	var stored []int64
	batch.Queue(`
		with today as (
			select count(*) as claims from document_client_ids
			where artifact_id = $1 and actor_kind = $3 and actor_id = $4 and not minted
				and created_at >= date_trunc('day', now() at time zone 'UTC') at time zone 'UTC'
		), claimed as (
			insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed, claimed_from)
			select $1, claim.client, $3, $4, false, true, claim.claimed_from
			from unnest($2::bigint[], $5::bigint[]) as claim (client, claimed_from)
			where (select claims from today) + cardinality($2::bigint[]) <= $6::bigint
			on conflict do nothing
			returning client_id
		)
		select (select claims from today), coalesce((select array_agg(client_id) from claimed), '{}')
	`, room, clientIDs(clients), actor.Kind, actor.ID, clientIDs(from), int64(maxClaimsADay)).QueryRow(func(row pgx.Row) error {
		return row.Scan(&today, &stored)
	})
	if err := s.store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, fmt.Errorf("claim client ids: %w", err)
	}
	if today+int64(len(clients)) > maxClaimsADay {
		return nil, fmt.Errorf("%w: its claimant made %d claims in the document today, and makes at most %d a day", errClaimRefused, today, maxClaimsADay)
	}
	claimed := make([]uint64, len(stored))
	for i, client := range stored {
		claimed[i] = uint64(client)
	}
	return claimed, nil
}

// releaseClaims gives back claims, the ids claimClients stored for actor for an update whose
// apply then did not stand, so an update's claim stands only where its update did. A release that
// fails leaves the claim, which is still actor's own: it is logged and nothing else.
func (s *Service) releaseClaims(room string, actor model.Actor, claims []uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), clientCheckTimeout)
	defer cancel()
	if _, err := s.store.Pool.Exec(ctx, `
		delete from document_client_ids
		where artifact_id = $1 and client_id = any($2) and actor_kind = $3 and actor_id = $4 and not minted
	`, room, clientIDs(claims), actor.Kind, actor.ID); err != nil {
		slog.Warn("dispatch: could not give back the client ids a refused browser update claimed",
			"room", room, "clients", len(claims), "error", err)
	}
}

// recordLanded marks the row of client, the id room's socket was given, as having landed an edit,
// so no mint gives it back and the sweep never deletes it (mintClient, sweepUnusedClientIDs), and
// reports whether the mark was written. A row the sweep took while the socket stayed open is
// written again for its person. It runs off the peer's read loop, one mark at a time for a socket,
// after an apply of the socket's that stood and left the room holding a struct under client
// (settleClients) - not one whose structs under it only parked - and only where the row did not
// say an edit had landed as the socket was admitted, which a claimed row does. A write that fails
// is logged, and the socket goes on presenting the row (keepPresenting) until its next such apply
// marks it.
func (s *Service) recordLanded(room string, actor model.Actor, client crdt.ClientID) bool {
	ctx, cancel := context.WithTimeout(context.Background(), clientCheckTimeout)
	defer cancel()
	if _, err := s.store.Pool.Exec(ctx, `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed)
		values ($1, $2, $3, $4, true, true)
		on conflict (artifact_id, client_id) do update set landed = true
		where document_client_ids.actor_kind = excluded.actor_kind and document_client_ids.actor_id = excluded.actor_id
			and not document_client_ids.landed
	`, room, int64(client), actor.Kind, actor.ID); err != nil {
		slog.Warn("dispatch: could not mark a client id as having landed an edit; its socket presents it until a later landing marks it",
			"room", room, "client", uint64(client), "error", err)
		return false
	}
	return true
}

// sweepUnusedClientIDs deletes every row whose id never landed an edit and that nothing has
// presented for unusedClientIDAge - no admission, and no open socket (keepPresenting) - and
// returns how many it deleted.
func sweepUnusedClientIDs(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		delete from document_client_ids where not landed and presented_at < $1
	`, time.Now().Add(-unusedClientIDAge))
	if err != nil {
		return 0, fmt.Errorf("sweep the client ids no edit used: %w", err)
	}
	return tag.RowsAffected(), nil
}

// clientIDs is clients as the bigint array the table's queries take.
func clientIDs(clients []uint64) []int64 {
	ids := make([]int64, len(clients))
	for i, client := range clients {
		ids[i] = int64(client)
	}
	return ids
}
