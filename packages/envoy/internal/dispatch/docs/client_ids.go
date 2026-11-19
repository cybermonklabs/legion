package docs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A Yjs update names the client each of its structs belongs to, and the client id is the
// browser's own choice. Before this file nothing tied a client id to anyone, so a signed-in
// person could write under another's id at the clock that person typed next: the room took the
// forged structs, the other person's own keystroke at that clock then arrived as a duplicate and
// was dropped, the other person stayed connected and was told nothing, and their next keystroke
// landed inside the forged text (dispatch://INFRA-179).
//
// So Dispatch gives the client ids out, and a browser writes under no other. A document socket's
// admission (ServeHTTP) gives its page a client id for that document, stored with the person the
// socket is signed in as (document_client_ids, minted), before the page syncs or announces
// anything: the page presents the id it holds under DocumentClientIdParameter, and the admission
// gives it back where it was given to the same person, and gives out a fresh one otherwise. The
// answer is the first frame on the socket, a stateless message (DocumentClientIdMessagePrefix)
// written right after the upgrade's 101 (clientIDConn): the page cannot read the upgrade's
// headers, and y-protocols' awareness takes the document's client id as it is made, so the page
// needs the id before its provider sends anything of its own. The same tab keeps its id across
// every reconnect of one document - its offline edits are under it - and takes a fresh one only
// with a fresh document, after a refusal the editor rebuilds for.
//
// The rule, judged on each browser update before its apply (judgeClients): an update carrying
// structs under a client id stored for another person is refused whole, with
// DocumentClientIdRefusedCloseReason, and so is one carrying structs under an id at
// mintedClientFloor or above that no admission gave out. Deletions are not judged: a person
// deletes other people's text, and a delete set names the deleted items' clients. The stored row
// is the binding, so a reload, a restart and every other process read the same one, and the person
// a row names never changes, so a binding read at any moment before an apply is the binding at
// that apply.
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
// then on; a claim an update made whose apply does not stand is given back (releaseClaims). Every
// claim is counted, from the stored rows, among the person's claims in the document in the UTC
// day (maxClaimsADay), and a claim is held to three rules (claimable):
//
//   - An id the stored history does not name, and the room does not hold, may be claimed by a
//     legacy socket at any time, so the clients deployed without an id keep writing: no edit the
//     document holds is under it, so a forged claim of it takes nothing from anyone. Those claims
//     are bounded, as no window ends them: a socket holds at most maxConfirmedClients of them,
//     and none is stored where the person's claims in the document since the start of the UTC
//     day, counted from the stored rows, would come to more than maxClaimsADay (claimClients).
//     Past either the update is refused, and nothing it carries is claimed.
//   - An id the stored history names, or the room holds, is a writer's. It may be claimed only
//     until claimWindow has passed since this database first served a build that gives ids out
//     (document_client_ids_since), the architect's bound on the deploy, and only by a person a
//     version or a pending author row lists as having edited the document since the history
//     first named it: the writer was credited for what it typed, so the race the claim leaves is
//     among the document's co-authors of that time and no longer every reader.
//   - A page of this build has an id of its own, and claims another - a saved row of the earlier
//     build that it replays, or, at its admission, the id its document drew while its first
//     socket reached a process of the earlier build - only until claimWindow has passed, whether
//     the history names the id or not. An admission's claim is held to maxClaimsADay as well:
//     past it the id is not kept, and the page is given one afresh.
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
// (crdt.HeldStructsDifferV1, refusedDuplicates), and is refused DocumentClientIdRefusedCloseReason
// where they differ, from any socket and whether or not the claim window is open. An equal resend
// - any browser handing back what the room relayed - passes. The row keeps the id's clock in the
// document as the claim was made (claimed_from), so only clocks the claim could have written are
// compared, never the tab's own history before it. Each comparison reads the whole update, so an
// update is compared only where it carries such structs under at most maxComparedClaims claimed
// ids, as a keystroke in flight, a resend or a saved row replayed does; a whole-state handoff of
// the history, which carries the structs of every claim the room holds, is not compared, and is
// refused whole from a legacy socket alone, since an honest legacy tab writes under one id. Only an
// update carrying structs under an id below the floor that its socket does not hold and that fall
// wholly at clocks the room holds pays the one lookup of their ids' rows that finds the claims
// among them, never a keystroke under the socket's own id.
//
// Every page load is given an id, and most never type, so a row that never landed an edit and
// that no admission has presented for unusedClientIDAge is deleted by the hourly compaction sweep
// (sweepUnusedClientIDs); a page that comes back after that is given a fresh id, which loses
// nothing. A row whose id landed an edit is never swept, and neither is a claimed row: the first
// edit a socket lands under its own given id marks the row (recordLanded), and a claim is stored
// as landed. An update's claim is given back unless its apply stands. An admission's claim is
// bounded by the claim window and maxClaimsADay rather than swept: a swept claim's id could be
// claimed by another person while the tab that holds it is still open and writing under it.

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
	// update that carries it, which costs a query and changes no answer. It is also the most
	// claims of ids no stored history names that one legacy socket holds (socketClients.claims).
	maxConfirmedClients = 64
	// claimWindow is how long after document_client_ids_since an id below mintedClientFloor that
	// the stored history names may still be claimed, and any id by a page of this build: the
	// architect's bound on how long a tab of the earlier build, or the edits it saved, may take to
	// come back. A legacy socket's claim of an id the history does not name has no window.
	claimWindow = 30 * 24 * time.Hour
	// maxClaimsADay is the most claims one person makes in one document in one UTC day, counted
	// from the stored rows, wherever a legacy socket claims an id no stored history names or a
	// page of this build claims one at its admission (claimClients, keepsClient): with
	// maxConfirmedClients and claimWindow, the bound on the claimed rows, which no sweep takes.
	maxClaimsADay = 64
	// maxComparedClaims is the most ids another person claimed whose structs at clocks the room
	// holds one update has compared (refusedDuplicates): a keystroke in flight, a resend or a saved
	// row replayed carries one. An update past it is a whole-state handoff of the history, which
	// would otherwise pay a read of itself for every claim the room holds.
	maxComparedClaims = 4
	// unusedClientIDAge is how long a row whose id never landed an edit stays after the last
	// admission that presented it.
	unusedClientIDAge = 30 * 24 * time.Hour
	// hocuspocusStateless is the Hocuspocus message type of a stateless message, the frame that
	// carries a socket's client id to its page.
	hocuspocusStateless = 5
)

// clientClaimsLockNamespace is the first key of the lock a claim held to maxClaimsADay takes
// ("CLMS"), its second the hashtext of the document and the person, so two such claims of one
// person in one document count each other's rows (claimClients). It is the two-key form, which
// Postgres keeps apart from the single-key locks (a document's room, lockDocumentRoom, among
// them), and it is no other first key: neither conditionalEditQueueLockNamespace's own nor any of
// the others that lock's comment lists, which names this one among them. The statement that takes
// it names itself client_claims_lock and spells its key's text type, as the owner lock's do
// (store/ownerlock.go): the copy lock (projectCopiesLock) is taken by the same two-key call, and
// the copy-lock guard (api's TestCopyLockComesBeforeEveryPendingRowAndEvent) and the copy-lock
// waits a test reads off pg_stat_activity tell that lock by its statement's text, which this one
// would otherwise share.
const clientClaimsLockNamespace int32 = 0x434C4D53

// socketClients is what one document socket may write and announce under: the client id its
// admission gave its page, and the ids its page has since shown are that person's too. legacy is
// a page that presented no client id at all, a build from before Dispatch gave them out, which
// has no id of its own here. landed is whether own's row says an edit landed under it: read at
// the admission (keepsClient), and set once this socket has marked the row on its first edit
// under own (recordLanded), which it does once.
type socketClients struct {
	legacy bool
	own    crdt.ClientID
	landed atomic.Bool
	// mu guards confirmed and claims: the hook changes them on the peer's read loop, and nothing
	// else reads them, so the lock is never contended.
	mu        sync.Mutex
	confirmed map[crdt.ClientID]struct{}
	// claims is how many claims of ids no stored history names this socket holds: made by its
	// updates (judgeClients) and not given back (settleClients). A legacy socket holds at most
	// maxConfirmedClients of them.
	claims int
}

// holds is whether client is one this socket may write under without asking the table: the id
// its page was given, or one an earlier update showed is this person's.
func (c *socketClients) holds(client crdt.ClientID) bool {
	if !c.legacy && client == c.own {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, held := c.confirmed[client]
	return held
}

// confirm remembers clients as this socket's person's, up to maxConfirmedClients.
func (c *socketClients) confirm(clients []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range clients {
		if len(c.confirmed) >= maxConfirmedClients {
			return
		}
		if c.confirmed == nil {
			c.confirmed = make(map[crdt.ClientID]struct{})
		}
		c.confirmed[crdt.ClientID(client)] = struct{}{}
	}
}

// forget takes clients back out of what this socket remembers, for claims given back.
func (c *socketClients) forget(clients []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range clients {
		delete(c.confirmed, crdt.ClientID(client))
	}
}

// mayClaim is whether this socket may hold n claims more of ids no stored history names.
func (c *socketClients) mayClaim(n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims+n <= maxConfirmedClients
}

// countClaims counts n claims of ids no stored history names as this socket's, or, for a
// negative n, takes -n of them back.
func (c *socketClients) countClaims(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims += n
}

// announces is whether this socket may carry awareness for client, the awareness filter's rule
// (awarenessLimitConn.allowed): a page announces its presence under its own client id alone, so
// a socket given one carries awareness for that id and no other, and the echo of another
// reader's state a provider sends back is dropped with the rest. A page of an earlier build has
// no id here, and carries awareness only for ids below mintedClientFloor, which no admission
// gives out: never for an id Dispatch gave anyone.
func (c *socketClients) announces(client uint64) bool {
	if c.legacy {
		return client < mintedClientFloor
	}
	return client == uint64(c.own)
}

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
// lists actor. unnamed says the id is neither, so that a legacy socket's claim of it is held to
// its bounds (judgeClients). heldTo is the clock the document holds client to, where a claim made
// now starts (claimed_from).
//
// A claim the closed window refuses whatever the history says - a page of this build's, or one of
// an id the room holds - reads no history; every other claim reads it once per room instance
// (historyNames).
func (s *Service) claimable(ctx context.Context, room string, live *crdt.Doc, held crdt.StateVector, actor model.Actor, legacy bool, client uint64) (heldTo uint64, unnamed bool, err error) {
	open, err := s.claimWindowOpen(ctx)
	if err != nil {
		return 0, false, err
	}
	id := crdt.ClientID(client)
	if !open && (!legacy || held[id] > 0) {
		return 0, false, claimWindowClosed()
	}
	from, storedTo, named, err := s.historyNames(ctx, room, live, id)
	if err != nil {
		return 0, false, err
	}
	if !named && held[id] == 0 {
		return 0, true, nil
	}
	if !open {
		return 0, false, claimWindowClosed()
	}
	// A co-author's edit names them in a pending author row once its append lands, so the room's
	// appends are asked for at once, and waited for, before the authors are read
	// (waitForDurableAppends): an edit the room took a moment ago lists its author.
	if err := s.waitForDurableAppends(ctx, room); err != nil {
		return 0, false, err
	}
	author, err := json.Marshal([]map[string]string{{"kind": actor.Kind, "id": actor.ID}})
	if err != nil {
		return 0, false, err
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
		return 0, false, fmt.Errorf("read the document's authors: %w", err)
	}
	if !listed {
		return 0, false, fmt.Errorf("%w: the document's history names it, and no version since lists the claimant", errClaimRefused)
	}
	return max(storedTo, held[id]), false, nil
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

// admitClients is a document socket's client ids, decided by its admission: none for a page that
// presented no DocumentClientIdParameter, and otherwise the id the page presented where the
// table names the same person for it, or one below mintedClientFloor that no row names and the
// page may claim (claimable: a page of this build whose first socket reached a process of the
// earlier build and kept the id its document drew); and an id given out afresh in every other
// case.
func (s *Service) admitClients(ctx context.Context, room string, actor model.Actor, r *http.Request) (*socketClients, error) {
	presented, takes := r.URL.Query()[contracts.DocumentClientIdParameter]
	if !takes {
		return &socketClients{legacy: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, clientCheckTimeout)
	defer cancel()
	if presented[0] != "" {
		if client, err := strconv.ParseUint(presented[0], 10, 64); err == nil {
			kept, err := s.keepsClient(ctx, room, actor, client)
			if err != nil || kept != nil {
				return kept, err
			}
		}
	}
	client, err := s.mintClient(ctx, room, actor)
	if err != nil {
		return nil, err
	}
	return &socketClients{own: client}, nil
}

// keepsClient is the socket's client ids where a page may keep client, the id it presented, and
// nil where it may not. It may keep an id the table names actor for, which this records as a
// presentation (presented_at) in the same statement, reading back whether an edit landed under
// it (socketClients.landed), so a socket whose id has landed an edit already writes nothing more
// on its first edit. And it may keep an id below mintedClientFloor no row names that actor may
// claim (claimable, as a page of this build), which this claims from the clock the document holds
// it to, in the stored history or the room where this server holds it, held to maxClaimsADay
// with actor's other claims in the document (claimClients, capped): a claim it refuses keeps
// nothing, and the page is given an id afresh. An id at mintedClientCeiling or above is neither:
// no admission gives one out and no claim takes one, so it is not kept and the table is not
// asked, as a presented id that does not read is not.
func (s *Service) keepsClient(ctx context.Context, room string, actor model.Actor, client uint64) (*socketClients, error) {
	if client >= mintedClientCeiling {
		return nil, nil
	}
	kept := &socketClients{own: crdt.ClientID(client)}
	var landed bool
	err := s.store.Pool.QueryRow(ctx, `
		update document_client_ids set presented_at = now()
		where artifact_id = $1 and client_id = $2 and actor_kind = $3 and actor_id = $4
		returning landed
	`, room, int64(client), actor.Kind, actor.ID).Scan(&landed)
	switch {
	case err == nil:
		kept.landed.Store(landed)
		return kept, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("give the page its client id back: %w", err)
	case client >= mintedClientFloor:
		return nil, nil
	}
	owners, err := s.clientOwners(ctx, room, []uint64{client})
	if err != nil {
		return nil, err
	}
	if _, bound := owners[client]; bound {
		return nil, nil
	}
	live := s.srv.GetDoc(room)
	heldTo, _, err := s.claimable(ctx, room, live, heldBy(live), actor, false, client)
	var claimed []uint64
	if err == nil {
		claimed, err = s.claimClients(ctx, room, actor, []uint64{client}, []uint64{heldTo}, true)
	}
	switch {
	case errors.Is(err, errClaimRefused):
		return nil, nil
	case err != nil || len(claimed) == 0:
		return nil, err
	}
	kept.landed.Store(true)
	return kept, nil
}

// mintClient gives actor a client id for room, stored before it is returned: a random id in
// [mintedClientFloor, mintedClientCeiling) the document has not given out.
func (s *Service) mintClient(ctx context.Context, room string, actor model.Actor) (crdt.ClientID, error) {
	for range mintTries {
		var drawn [8]byte
		if _, err := rand.Read(drawn[:]); err != nil {
			return 0, err
		}
		client := mintedClientFloor + binary.BigEndian.Uint64(drawn[:])%(mintedClientCeiling-mintedClientFloor)
		tag, err := s.store.Pool.Exec(ctx, `
			insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted)
			values ($1, $2, $3, $4, true)
			on conflict do nothing
		`, room, int64(client), actor.Kind, actor.ID)
		if err != nil {
			return 0, fmt.Errorf("give out a client id: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return crdt.ClientID(client), nil
		}
	}
	return 0, fmt.Errorf("give out a client id: %d draws were all taken", mintTries)
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
// takes it: an update's claim is given back unless its edit lands (releaseClaims), and an
// admission's is bounded by the claim window and maxClaimsADay (keepsClient). It returns the ones
// this call stored: an id another call stored first is not among them, and the caller reads who
// that was.
//
// capped holds the claims to maxClaimsADay, as a legacy socket's claims of ids no stored history
// names are (judgeClients) and an admission's claim is (keepsClient): where actor's claims stored
// in room since the start of the UTC day (created_at, not minted) - every claim, capped or not -
// and these come to more, none of them is stored and the call answers errClaimRefused. The count
// and the insert are one statement behind the lock of actor's claims in room
// (clientClaimsLockNamespace), in the one implicit transaction a batch runs in, so two capped
// calls of one person count each other's rows; the statement reads the count off
// document_client_ids_claims.
func (s *Service) claimClients(ctx context.Context, room string, actor model.Actor, clients, from []uint64, capped bool) ([]uint64, error) {
	batch := &pgx.Batch{}
	if capped {
		batch.Queue(`select pg_advisory_xact_lock($1, hashtext($2::text)) as client_claims_lock`, clientClaimsLockNamespace, room+" "+ActorRef(actor))
	}
	var today int64
	var stored []int64
	batch.Queue(`
		with today as (
			select count(*) as claims from document_client_ids
			where $6::boolean and artifact_id = $1 and actor_kind = $3 and actor_id = $4 and not minted
				and created_at >= date_trunc('day', now() at time zone 'UTC') at time zone 'UTC'
		), claimed as (
			insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed, claimed_from)
			select $1, claim.client, $3, $4, false, true, claim.claimed_from
			from unnest($2::bigint[], $5::bigint[]) as claim (client, claimed_from)
			where not $6::boolean or (select claims from today) + cardinality($2::bigint[]) <= $7::bigint
			on conflict do nothing
			returning client_id
		)
		select (select claims from today), coalesce((select array_agg(client_id) from claimed), '{}')
	`, room, clientIDs(clients), actor.Kind, actor.ID, clientIDs(from), capped, int64(maxClaimsADay)).QueryRow(func(row pgx.Row) error {
		return row.Scan(&today, &stored)
	})
	if err := s.store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, fmt.Errorf("claim client ids: %w", err)
	}
	if capped && today+int64(len(clients)) > maxClaimsADay {
		return nil, fmt.Errorf("%w: its claimant made %d claims in the document today, and makes at most %d a day", errClaimRefused, today, maxClaimsADay)
	}
	claimed := make([]uint64, len(stored))
	for i, client := range stored {
		claimed[i] = uint64(client)
	}
	return claimed, nil
}

// releaseClaims gives back claims, the ids claimClients stored for actor for an update whose
// apply then did not stand, so an update's claim stands only where its edit landed. A release
// that fails leaves the claim, which is still actor's own: it is logged and nothing else.
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
// so the sweep never deletes it (sweepUnusedClientIDs). A row the sweep took while the socket
// stayed open is written again for its person. It runs off the peer's read loop, once a socket,
// after the socket's first edit under its own id stood, and only where the row did not say an edit
// had landed as the socket was admitted, which a claimed row does; a write that fails is logged,
// and the socket's next reconnect marks the row again on its first edit.
func (s *Service) recordLanded(room string, actor model.Actor, client crdt.ClientID) {
	ctx, cancel := context.WithTimeout(context.Background(), clientCheckTimeout)
	defer cancel()
	if _, err := s.store.Pool.Exec(ctx, `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed)
		values ($1, $2, $3, $4, true, true)
		on conflict (artifact_id, client_id) do update set landed = true
		where document_client_ids.actor_kind = excluded.actor_kind and document_client_ids.actor_id = excluded.actor_id
			and not document_client_ids.landed
	`, room, int64(client), actor.Kind, actor.ID); err != nil {
		slog.Warn("dispatch: could not mark a client id as having landed an edit", "room", room, "client", uint64(client), "error", err)
	}
}

// sweepUnusedClientIDs deletes every row whose id never landed an edit and that no admission has
// presented for unusedClientIDAge, and returns how many it deleted.
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

// clientJudgement is what judgeClients decided of an update it let through: the ids it claimed
// for the sender, which the hook gives back if the apply does not stand (settleClients,
// releaseClaims), how many of them the socket's bound on its claims of ids no stored history names
// counts (socketClients.claims), which go back with them, and whether the update writes under the
// socket's own given id, whose row the hook marks once the apply stands (settleClients,
// recordLanded).
type clientJudgement struct {
	claimed   []uint64
	counted   int
	writesOwn bool
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
		return clientJudgement{writesOwn: groups == 1 && !clients.legacy && client == clients.own}, nil
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
	_, judged.writesOwn = from[clients.own]
	judged.writesOwn = judged.writesOwn && !clients.legacy
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
	// capped are the claims of ids no stored history names that a legacy socket makes: the claims
	// no window ends, which its bounds count (socketClients.mayClaim, claimClients).
	var claims, claimFrom, capped []uint64
	for _, client := range asked {
		owner, bound := owners[client]
		switch {
		case bound && !owner.is(connection.actor):
			return clientJudgement{}, s.refuseClient(info, connection, client, "it names another person")
		case !bound && client >= mintedClientFloor:
			return clientJudgement{}, s.refuseClient(info, connection, client, "no admission gave it out")
		case !bound:
			heldTo, unnamed, err := s.claimable(ctx, info.Room, info.Doc, held, connection.actor, clients.legacy, client)
			if errors.Is(err, errClaimRefused) {
				return clientJudgement{}, s.refuseClient(info, connection, client, err.Error())
			} else if err != nil {
				return clientJudgement{}, s.clientCheckFailed(info, err)
			}
			claims = append(claims, client)
			claimFrom = append(claimFrom, heldTo)
			if unnamed && clients.legacy {
				capped = append(capped, client)
			}
		}
	}
	if len(claims) > 0 {
		if len(capped) > 0 && !clients.mayClaim(len(capped)) {
			return clientJudgement{}, s.refuseClient(info, connection, capped[0],
				fmt.Sprintf("its socket holds claims of %d ids no history names, the most it may", maxConfirmedClients))
		}
		judged.claimed, err = s.claimClients(ctx, info.Room, connection.actor, claims, claimFrom, len(capped) > 0)
		if errors.Is(err, errClaimRefused) {
			return clientJudgement{}, s.refuseClient(info, connection, capped[0], err.Error())
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
		for _, client := range judged.claimed {
			if slices.Contains(capped, client) {
				judged.counted++
			}
		}
		clients.countClaims(judged.counted)
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
// room holds at the same clocks from the claim on (crdt.HeldStructsDifferV1): an edit typed at a
// clock another person claimed first. It is nil for an update that hands back the room's own
// structs, as a browser relayed them. An update whose structs do not decode is the apply's to
// refuse.
func (s *Service) refusedDuplicate(info websocket.PeerUpdateInfo, connection *connectionState, owner clientOwner, client uint64, from crdt.StateVector) error {
	id := crdt.ClientID(client)
	differs, err := crdt.HeldStructsDifferV1(info.Doc, info.Update, id, max(from[id], owner.claimedFrom))
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
// whether its update stood: an apply that did not stand gives back the claims it made
// (releaseClaims), with the socket's count of them, and the first apply of a socket that stood
// with an edit under the socket's own id marks that id's row (recordLanded), unless the row said
// an edit had landed as the socket was admitted.
func (s *Service) settleClients(info websocket.PeerUpdateInfo, judged clientJudgement, stood bool) {
	connection := connectionOf(info.Context)
	if connection == nil || connection.clients == nil {
		return
	}
	if !stood {
		if len(judged.claimed) > 0 {
			connection.clients.forget(judged.claimed)
			connection.clients.countClaims(-judged.counted)
			s.releaseClaims(info.Room, connection.actor, judged.claimed)
		}
		return
	}
	if judged.writesOwn && connection.clients.landed.CompareAndSwap(false, true) {
		go s.recordLanded(info.Room, connection.actor, connection.clients.own)
	}
}

// refuseClient is the refusal of an update carrying structs under client, which is not the
// sender's for why.
func (s *Service) refuseClient(info websocket.PeerUpdateInfo, connection *connectionState, client uint64, why string) error {
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

// clientIDWriter is a document socket's response writer under ygo's upgrade: the connection the
// upgrade hijacks through it writes frame, the stateless message naming the client id the
// socket's admission gave its page, right after the upgrade's answer (clientIDConn), so it is
// the first frame the page reads.
type clientIDWriter struct {
	http.ResponseWriter
	frame []byte
}

func (w *clientIDWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &clientIDConn{Conn: conn, frame: w.frame}, buffers, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *clientIDWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// clientIDConn is a hijacked connection whose first write, the upgrade's 101 answer, is followed
// in the same write by frame. gorilla writes that answer in one write before it hands the
// connection to any other goroutine (processHeaderWriter), so frame is cleared by it and every
// later write passes straight through. A first write that is no 101 answer upgrades nothing, and
// frame goes nowhere.
type clientIDConn struct {
	net.Conn
	frame []byte
}

func (c *clientIDConn) Write(p []byte) (int, error) {
	frame := c.frame
	if frame == nil {
		return c.Conn.Write(p)
	}
	c.frame = nil
	if !bytes.HasPrefix(p, []byte("HTTP/1.1 101 ")) || !bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		return c.Conn.Write(p)
	}
	written, err := c.Conn.Write(slices.Concat(p, frame))
	return min(written, len(p)), err
}

// clientIDFrame is the websocket frame a server sends carrying the Hocuspocus stateless message
// that names client as the id room's page writes under (DocumentClientIdMessagePrefix).
func clientIDFrame(room string, client crdt.ClientID) []byte {
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(hocuspocusStateless)
		encoder.WriteVarString(contracts.DocumentClientIdMessagePrefix + strconv.FormatUint(uint64(client), 10))
	})
	return append(frameHeader(gws.BinaryMessage, true, int64(len(message)), nil), message...)
}
