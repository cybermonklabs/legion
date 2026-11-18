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
// Dispatch draws the ids it gives out at mintedClientFloor or above. A browser of a build from
// before this one drew its own below it, as Yjs does (lib0 random.uint32), and so does every
// write of the server's own: ygo draws a document's client id the same way (crdt.NewClientID),
// for a room, a write's fork, and the fresh id a document takes when a remote apply advances its
// own clock (reearth/ygo #340). So no id Dispatch gives a browser can be one the server writes
// under, and no server write can be judged here: it reaches the room by no browser's socket.
//
// The deploy is the one place an id below the floor is still a browser's. A tab of the earlier
// build, open across the deploy or reloaded by the reader once it is over, holds the edits it
// typed under its own id, saved in the browser (pending-sync.ts), and replays each saved row
// under the client id it was typed under. So an id below the floor that no row names may be
// claimed, before the apply (claimClients), by the person whose browser update carries structs
// under it first, and is that person's from then on; a claim whose apply does not stand is given
// back (releaseClaims). A claim is held to two rules (claimable). It is refused once
// claimWindow has passed since this database first served a build that gives ids out
// (document_client_ids_since), the architect's bound on the deploy. And where the stored history
// already names the id, only a person a version or a pending author row lists as having edited
// the document since the history first named it may claim it: the id is a writer's, and the
// writer was credited for what it typed, so the race the claim leaves is among the document's
// co-authors of that time and no longer every reader. An id the history does not name - a tab
// that typed offline from the moment it opened - no one can have read, so its first claim stands.
//
// That race is not refused, but it loses nothing in silence. A co-author who claims the tab's id
// first, writing at the clock the tab writes next, leaves the room holding that clock, and the
// tab's own edit at it then arrives as a duplicate: the apply lands nothing of it and says
// nothing, and the tab would believe it saved. So, while the claim window is open, an update
// carrying structs under a claimed id below the floor that is not its sender's, at clocks the room
// already holds from the claim on, has them compared with what the room holds there
// (crdt.HeldStructsDifferV1, refusedDuplicate), and is refused DocumentClientIdRefusedCloseReason
// where they differ. An equal resend - any browser handing back what the room relayed - passes.
// The row keeps the id's clock in the document as the claim was made (claimed_from), so only
// clocks the claim could have written are compared, never the tab's own history before it, and
// only an update carrying such a duplicate pays for the comparison, never a keystroke under the
// socket's own id.
//
// Every page load is given an id, and most never type, so a row that never landed an edit and
// that no admission has presented for unusedClientIDAge is deleted by the hourly compaction sweep
// (sweepUnusedClientIDs); a page that comes back after that is given a fresh id, which loses
// nothing. A row whose id landed an edit is never swept: the first edit a socket lands under its
// own id marks the row (recordLanded), and a claim is marked as it is made.

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
	// update that carries it, which costs a query and changes no answer.
	maxConfirmedClients = 64
	// claimWindow is how long after document_client_ids_since an id below mintedClientFloor may
	// still be claimed: the architect's bound on how long a tab of the earlier build, or the
	// edits it saved, may take to come back.
	claimWindow = 30 * 24 * time.Hour
	// unusedClientIDAge is how long a row whose id never landed an edit stays after the last
	// admission that presented it.
	unusedClientIDAge = 30 * 24 * time.Hour
	// hocuspocusStateless is the Hocuspocus message type of a stateless message, the frame that
	// carries a socket's client id to its page.
	hocuspocusStateless = 5
)

// socketClients is what one document socket may write and announce under: the client id its
// admission gave its page, and the ids its page has since shown are that person's too. legacy is
// a page that presented no client id at all, a build from before Dispatch gave them out, which
// has no id of its own here. landed is whether this socket has marked its own id's row as having
// landed an edit (recordLanded), which it does once.
type socketClients struct {
	legacy bool
	own    crdt.ClientID
	landed atomic.Bool
	// mu guards confirmed: the hook adds to it on the peer's read loop, and nothing else reads
	// it, so the lock is never contended.
	mu        sync.Mutex
	confirmed map[crdt.ClientID]struct{}
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

// errClaimRefused is a claim of an id below mintedClientFloor that claimable refuses.
var errClaimRefused = errors.New("dispatch: the client id may not be claimed")

// RecordClientIDsSince records, once, that this database now serves a build that gives client ids
// out: the first process of such a build to boot writes the one row of document_client_ids_since,
// and every later boot, of this process or another, leaves it as it is. Its time is where
// claimWindow counts from. A boot that cannot write it logs and serves on; earlier-build ids stay
// claimable until a boot records it.
func (s *Service) RecordClientIDsSince(ctx context.Context) error {
	if _, err := s.store.Pool.Exec(ctx, `
		insert into document_client_ids_since (since) values (now()) on conflict do nothing
	`); err != nil {
		return fmt.Errorf("record when this database first served a build that gives client ids out: %w", err)
	}
	return nil
}

// claimWindowOpen is whether an id below mintedClientFloor may still be claimed: no boot has
// recorded document_client_ids_since yet, or claimWindow has not passed since it.
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
// room, and errClaimRefused, saying why, where it may not: the claim window has closed, or the
// stored history names the id and neither a version holding the history's first row naming it
// nor a pending author row lists actor (historyNames). It also answers the clock the stored
// history holds client to, where a claim made now starts (claimed_from). It reads every stored
// row of the document, which only a claim pays for, and a claim is made once for each such id.
func (s *Service) claimable(ctx context.Context, room string, actor model.Actor, client uint64) (heldTo uint64, err error) {
	open, err := s.claimWindowOpen(ctx)
	if err != nil {
		return 0, err
	}
	if !open {
		return 0, fmt.Errorf("%w: its window closed %s after this database first gave client ids out", errClaimRefused, claimWindow)
	}
	// What the room took and has not stored yet is history too: an id a tab wrote under a moment
	// ago names nothing in the stored rows until the room's appends land, so they are asked for at
	// once, and waited for, before the history is read (waitForDurableAppends).
	if err := s.waitForDurableAppends(ctx, room); err != nil {
		return 0, err
	}
	named, from, heldTo, err := s.historyNames(ctx, room, crdt.ClientID(client))
	if err != nil || !named {
		return heldTo, err
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
	return heldTo, nil
}

// historyNames reports whether room's stored history names client; the doc_updates version the
// versions that may hold the first struct of client start from: the version of the first stored
// row whose structs name it, or 0 where that row is the history's first, which a compaction may
// have folded every earlier row into, so the struct can be older than the row; and the clock the
// history holds client to, the furthest any row's structs of it reach.
func (s *Service) historyNames(ctx context.Context, room string, client crdt.ClientID) (named bool, from int64, heldTo uint64, err error) {
	rows, err := s.store.Pool.Query(ctx, `select version, update from doc_updates where artifact_id = $1 order by version`, room)
	if err != nil {
		return false, 0, 0, fmt.Errorf("read the document's history: %w", err)
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var version int64
		var update []byte
		if err := rows.Scan(&version, &update); err != nil {
			return false, 0, 0, fmt.Errorf("read the document's history: %w", err)
		}
		structs, ends, metaErr := crdt.ParseUpdateMeta(update)
		if _, names := structs[client]; metaErr == nil && names {
			if !named && !first {
				from = version
			}
			named = true
			heldTo = max(heldTo, ends[client])
		}
		first = false
	}
	if err := rows.Err(); err != nil {
		return false, 0, 0, fmt.Errorf("read the document's history: %w", err)
	}
	return named, from, heldTo, nil
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
	if len(presented) > 0 && presented[0] != "" {
		if client, err := strconv.ParseUint(presented[0], 10, 64); err == nil {
			kept, err := s.keepsClient(ctx, room, actor, client)
			if err != nil {
				return nil, err
			}
			if kept {
				return &socketClients{own: crdt.ClientID(client)}, nil
			}
		}
	}
	client, err := s.mintClient(ctx, room, actor)
	if err != nil {
		return nil, err
	}
	return &socketClients{own: client}, nil
}

// keepsClient is whether a page may keep client, the id it presented: the table names actor for
// it, which this records as a presentation (presented_at) in the same statement, or it is an id
// below mintedClientFloor no row names that actor may claim, which this claims from the clock the
// document holds it to, in the stored history or the room where this server holds it.
func (s *Service) keepsClient(ctx context.Context, room string, actor model.Actor, client uint64) (bool, error) {
	tag, err := s.store.Pool.Exec(ctx, `
		update document_client_ids set presented_at = now()
		where artifact_id = $1 and client_id = $2 and actor_kind = $3 and actor_id = $4
	`, room, int64(client), actor.Kind, actor.ID)
	if err != nil {
		return false, fmt.Errorf("give the page its client id back: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	if client >= mintedClientFloor {
		return false, nil
	}
	owners, err := s.clientOwners(ctx, room, []uint64{client})
	if err != nil {
		return false, err
	}
	if _, bound := owners[client]; bound {
		return false, nil
	}
	heldTo, err := s.claimable(ctx, room, actor, client)
	if err != nil {
		if errors.Is(err, errClaimRefused) {
			return false, nil
		}
		return false, err
	}
	heldTo = max(heldTo, heldBy(s.srv.GetDoc(room))[crdt.ClientID(client)])
	claimed, err := s.claimClients(ctx, room, actor, []uint64{client}, []uint64{heldTo})
	return len(claimed) == 1, err
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
// the clock at the same index of from (claimed_from), marked as having landed an edit, since a
// claim is made only by one, and returns the ones this call stored: an id another call stored
// first is not among them, and the caller reads who that was.
func (s *Service) claimClients(ctx context.Context, room string, actor model.Actor, clients, from []uint64) ([]uint64, error) {
	rows, err := s.store.Pool.Query(ctx, `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed, claimed_from)
		select $1, claim.client, $3, $4, false, true, claim.claimed_from
		from unnest($2::bigint[], $5::bigint[]) as claim (client, claimed_from)
		on conflict do nothing
		returning client_id
	`, room, clientIDs(clients), actor.Kind, actor.ID, clientIDs(from))
	if err != nil {
		return nil, fmt.Errorf("claim client ids: %w", err)
	}
	defer rows.Close()
	var claimed []uint64
	for rows.Next() {
		var client int64
		if err := rows.Scan(&client); err != nil {
			return nil, fmt.Errorf("claim client ids: %w", err)
		}
		claimed = append(claimed, uint64(client))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim client ids: %w", err)
	}
	return claimed, nil
}

// releaseClaims gives back claims, the ids claimClients stored for actor for an update whose
// apply then did not stand, so an id is claimed only by an edit the room took. A release that
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
// so the sweep never deletes it (sweepUnusedClientIDs). A row the sweep took while the socket
// stayed open is written again for its person. It runs off the peer's read loop, once a socket,
// after the socket's first edit under its own id stood; a write that fails is logged, and the
// socket's next reconnect marks the row again on its first edit.
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
// for the sender, which the hook gives back if the apply does not stand (giveBackClaims), and
// whether the update writes under the socket's own given id, whose row the hook marks once the
// apply stands (noteLanded).
type clientJudgement struct {
	claimed   []uint64
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
// the ids its socket has not shown before that carry clocks the room lacks, and, while the claim
// window is open, about the ids below mintedClientFloor whose structs fall wholly at clocks the
// room holds (heldDuplicates, refusedDuplicate).
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
	if len(duplicated) > 0 {
		open, err := s.claimWindowOpen(ctx)
		if err != nil {
			return clientJudgement{}, s.clientCheckFailed(info, err)
		}
		if !open {
			duplicated = nil
		}
	}
	if len(asked) == 0 && len(duplicated) == 0 {
		return judged, nil
	}
	owners, err := s.clientOwners(ctx, info.Room, append(slices.Clone(asked), duplicated...))
	if err != nil {
		return clientJudgement{}, s.clientCheckFailed(info, err)
	}
	for _, client := range duplicated {
		if refusal := s.refusedDuplicate(info, connection, owners[client], client, from, to); refusal != nil {
			return clientJudgement{}, refusal
		}
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
			heldTo, err := s.claimable(ctx, info.Room, connection.actor, client)
			if errors.Is(err, errClaimRefused) {
				return clientJudgement{}, s.refuseClient(info, connection, client, err.Error())
			} else if err != nil {
				return clientJudgement{}, s.clientCheckFailed(info, err)
			}
			claims = append(claims, client)
			claimFrom = append(claimFrom, max(heldTo, held[crdt.ClientID(client)]))
		}
	}
	if len(claims) > 0 {
		if judged.claimed, err = s.claimClients(ctx, info.Room, connection.actor, claims, claimFrom); err != nil {
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
	}
	clients.confirm(asked)
	return judged, nil
}

// refusedDuplicate is the refusal of update's structs under client, an id below
// mintedClientFloor whose structs fall wholly at clocks the room holds (heldDuplicates), where
// owner, the row the table holds for it, is another person's claim and the structs differ from
// what the room holds at the same clocks from the claim on (crdt.HeldStructsDifferV1): an edit
// typed at a clock another person claimed first, which the apply would drop as a duplicate and
// say nothing of. It is nil for an id no row names, one the sender's own person claimed, and an
// update that hands back the room's own structs, as a browser relayed them. An update whose
// structs do not decode is the apply's to refuse.
func (s *Service) refusedDuplicate(info websocket.PeerUpdateInfo, connection *connectionState, owner clientOwner, client uint64, from, to crdt.StateVector) error {
	id := crdt.ClientID(client)
	if !owner.claimed || owner.is(connection.actor) || to[id] <= owner.claimedFrom {
		return nil
	}
	differs, err := crdt.HeldStructsDifferV1(info.Doc, info.Update, id, max(from[id], owner.claimedFrom))
	if err != nil || !differs {
		return nil
	}
	return s.refuseClient(info, connection, client, "it carries an edit at clocks another person claimed first that differs from what the room holds there")
}

// stepTwoCut is the cut the socket's filter makes of each sync step 2 it sends
// (awarenessLimitConn.collectStepTwo): the structs under client ids the table names another
// person for, of the clients whose clocks reach past what the room holds (beyondHeld), are taken
// out of the update with ygo's own diff (crdt.DiffUpdateV1, against a state vector that holds
// each such client complete), and the rest - the sender's own structs, those of ids no row names,
// structs the room holds already, and every deletion - goes on to the apply, which the peer
// update hook judges as any other (judgeClients). A whole-state answer carries what the room
// lacks, and that can be another person's edits the sender holds and the room does not: ones the
// room relayed and then lost, or ones it holds only parked. Those are not the sender's to give, so
// they are dropped rather than the answer refused, which would refuse an honest editor. An
// incremental update under another person's id is still refused whole: an editor sends no such
// update. An update whose header does not read, or whose ids the table cannot be read for, goes
// on as it is, and the hook refuses what it must of it.
func (s *Service) stepTwoCut(ctx context.Context, room string, actor model.Actor, clients *socketClients) func([]byte) []byte {
	return func(update []byte) []byte {
		from, to, err := crdt.ParseUpdateMeta(update)
		if err != nil {
			return update
		}
		asked := beyondHeld(heldBy(s.srv.GetDoc(room)), from, to, clients)
		if len(asked) == 0 {
			return update
		}
		ctx, cancel := context.WithTimeout(ctx, clientCheckTimeout)
		defer cancel()
		owners, err := s.clientOwners(ctx, room, asked)
		if err != nil {
			return update
		}
		complete := crdt.StateVector{}
		for _, client := range asked {
			if owner, bound := owners[client]; bound && !owner.is(actor) {
				complete[crdt.ClientID(client)] = to[crdt.ClientID(client)]
			}
		}
		if len(complete) == 0 {
			return update
		}
		cut, err := crdt.DiffUpdateV1(update, complete)
		if err != nil {
			return update
		}
		slog.Info("dispatch: cut another person's edits out of a browser's sync step 2",
			"room", room, "clients", len(complete), "update_bytes", len(update), "cut_bytes", len(cut))
		return cut
	}
}

// beyondHeld is the clients of an update, from and to as crdt.ParseUpdateMeta reads them, that
// clients does not hold as the socket's own and whose structs reach past held, what the room
// holds: the clients the update can land anything under. Structs the room holds already are
// duplicates that land nothing whoever sends them, so they are neither judged for their id nor
// cut, and the ids of a history a browser hands back whole cost no lookup.
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
// person claimed the id (refusedDuplicate). A client whose structs reach past held is beyondHeld's.
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

// settleClients is what the hook does with judged once the apply has returned err: an apply that
// did not stand gives back the claims it made (releaseClaims), and the first apply of a socket
// that stood with an edit under the socket's own id marks that id's row (recordLanded).
func (s *Service) settleClients(info websocket.PeerUpdateInfo, judged clientJudgement, stood bool) {
	connection := connectionOf(info.Context)
	if connection == nil || connection.clients == nil {
		return
	}
	if !stood {
		if len(judged.claimed) > 0 {
			connection.clients.forget(judged.claimed)
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
	refusal := &websocket.PeerClose{Code: contracts.DocumentBusyCloseCode, Reason: contracts.DocumentBusyCloseReason}
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
	written, err := c.Conn.Write(append(bytes.Clone(p), frame...))
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
