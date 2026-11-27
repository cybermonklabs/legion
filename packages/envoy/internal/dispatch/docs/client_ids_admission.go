package docs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// This file holds the client id rule's admission (client_ids.go): the ids a document socket's
// admission gives its page, kept or minted (admitClients, keepsClient, mintClient), what the
// socket may write and announce under from then on (socketClients), and the presentation that
// keeps the rows of the ids a socket holds live while it is open (keepPresenting).

// presenceIntervalForTest replaces clientPresenceInterval where it is set, in nanoseconds: a test
// of the give-back shortens it, so the sockets it holds open present their ids within the test's
// time. Zero, which leaves clientPresenceInterval, outside tests.
var presenceIntervalForTest atomic.Int64

// presenceInterval is clientPresenceInterval, or a test's in its place.
func presenceInterval() time.Duration {
	if interval := presenceIntervalForTest.Load(); interval > 0 {
		return time.Duration(interval)
	}
	return clientPresenceInterval
}

// socketClients is what one document socket may write and announce under: the client id its
// admission gave its page, and the ids its page has since shown are that person's too. legacy is
// a page that presented no client id at all, a build from before Dispatch gave them out, which
// has no id of its own here. stored is whether a row names own as the socket's person's: false
// for a legacy socket and for a read-only admission's id, drawn or kept and stored nowhere.
// landed is whether own's row says an edit landed under it: read at the admission (keepsClient),
// and set once this socket's mark of the row is written (recordLanded), after an apply that left
// the room holding a struct under own. marking is whether that mark is in flight, so the socket
// makes one at a time; a mark the store refused leaves landed false, so the socket goes on
// presenting the row (keepPresenting) and its next such apply marks it again (settleClients).
type socketClients struct {
	legacy  bool
	own     crdt.ClientID
	stored  bool
	landed  atomic.Bool
	marking atomic.Bool
	// mu guards confirmed, unmarked and claims: the hook changes them on the peer's read loop,
	// and reads confirmed inside the apply it waits for there (parkJudge), so the lock is never
	// contended but by a mark's end (endMark), the presentation's read (presenting), and the read
	// of the room's list of parked senders as the socket ends (parkedMarks).
	mu sync.Mutex
	// confirmed is every id this socket holds besides own: the ids its updates claimed and have
	// not given back, and those its updates showed the table names its person for. It keeps each
	// while it is open, and holds at most maxConfirmedClients (mayHold).
	confirmed map[crdt.ClientID]struct{}
	// unmarked is the ids of confirmed from mintedClientFloor up whose rows said no edit had
	// landed under them as this socket confirmed them - a closed tab's id whose saved rows the
	// page replays - until this socket's mark of each is written (recordLanded), each true while
	// its mark is in flight. The socket presents each of them every interval until then
	// (keepPresenting). A mark the store refused leaves its id here, and the next apply of the
	// socket's that writes under it marks it again (settleClients). own is never among them.
	unmarked map[crdt.ClientID]bool
	// claims is how many claims this socket's updates hold: made by its updates (judgeClients)
	// and not given back (settleClients). A socket holds at most maxClaimsASocket of them.
	claims int
}

// isOwn is whether client is the id this socket's admission gave its page. A legacy socket was
// given none, and its own names nothing, so no client is its own. Every comparison of a client
// with own goes through here, so none can take a legacy socket's zero own for an id.
func (c *socketClients) isOwn(client crdt.ClientID) bool {
	return !c.legacy && client == c.own
}

// holds is whether this socket holds client: the id its page was given, or one its updates showed
// is this person's (confirm). It keeps every one of them while it is open, so it holds every id
// its updates have written under.
func (c *socketClients) holds(client crdt.ClientID) bool {
	if c.isOwn(client) {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, held := c.confirmed[client]
	return held
}

// mayHold is whether this socket may hold n ids more besides its own (maxConfirmedClients).
func (c *socketClients) mayHold(n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.confirmed)+n <= maxConfirmedClients
}

// confirm remembers clients as this socket's person's, and unmarked, ids among them from
// mintedClientFloor up whose rows say no edit landed under them, as awaiting the socket's mark
// (settleClients). The judgement asks mayHold first.
func (c *socketClients) confirm(clients, unmarked []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range clients {
		if c.confirmed == nil {
			c.confirmed = make(map[crdt.ClientID]struct{})
		}
		c.confirmed[crdt.ClientID(client)] = struct{}{}
	}
	for _, client := range unmarked {
		if c.unmarked == nil {
			c.unmarked = make(map[crdt.ClientID]bool)
		}
		c.unmarked[crdt.ClientID(client)] = false
	}
}

// awaitsMark is whether client is an id this socket holds whose row awaits its mark (unmarked).
func (c *socketClients) awaitsMark(client crdt.ClientID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, awaits := c.unmarked[client]
	return awaits
}

// awaitingMarks is the ids whose rows await this socket's mark (unmarked) among writes, the
// clients an update writes structs under (crdt.ParseUpdateMeta): nil where none of them awaits
// one, which is every update once the socket's marks are written.
func (c *socketClients) awaitingMarks(writes crdt.StateVector) []crdt.ClientID {
	c.mu.Lock()
	defer c.mu.Unlock()
	var awaiting []crdt.ClientID
	for client := range c.unmarked {
		if _, written := writes[client]; written {
			awaiting = append(awaiting, client)
		}
	}
	return awaiting
}

// reachedMarks is what of reached, the clients a browser connection's parked frames reach past
// the room (parkedNeeds), awaits this socket's mark, read once for markParkedLanded and
// parkedMarks: own, whether reached names the socket's own id, and marks, each other id of
// reached whose row awaits the mark (awaitingMarks). Whether own's mark is written is the
// caller's to read (landed).
func (c *socketClients) reachedMarks(reached crdt.StateVector) (own bool, marks []crdt.ClientID) {
	_, reachesOwn := reached[c.own]
	return reachesOwn && c.isOwn(c.own), c.awaitingMarks(reached)
}

// presenting is the ids this socket presents again each interval while it is open
// (keepPresenting), as the table's queries take them: own, where a row names it as the socket's
// person's and the socket has not written its mark (stored, landed), and each id whose row awaits
// the socket's mark (unmarked). It is empty while every row the socket holds says an edit landed.
func (c *socketClients) presenting() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []int64
	if c.stored && !c.landed.Load() {
		ids = append(ids, int64(c.own))
	}
	for client := range c.unmarked {
		ids = append(ids, int64(client))
	}
	return ids
}

// startMark is whether this call starts the mark of client, an id whose row awaits this socket's
// mark and whose mark is not in flight; it is in flight from then until endMark.
func (c *socketClients) startMark(client crdt.ClientID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	inFlight, awaits := c.unmarked[client]
	if !awaits || inFlight {
		return false
	}
	c.unmarked[client] = true
	return true
}

// endMark ends the mark of client that startMark started: a mark written takes client out of
// unmarked, and one the store refused leaves it awaiting the next.
func (c *socketClients) endMark(client crdt.ClientID, written bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if written {
		delete(c.unmarked, client)
	} else {
		c.unmarked[client] = false
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

// mayClaim is whether this socket may hold n claims more (maxClaimsASocket).
func (c *socketClients) mayClaim(n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims+n <= maxClaimsASocket
}

// countClaims counts n claims as this socket's, or, for a negative n, takes -n of them back.
func (c *socketClients) countClaims(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims += n
}

// announces is whether this socket may carry awareness for client, the awareness filter's rule
// (awarenessLimitConn.allowed): a page announces its presence under its own client id alone, so
// a socket given one carries awareness for that id and no other, and the echo of another
// reader's state a provider sends back is dropped with the rest. A page of an earlier build has
// no id here, and carries awareness only for ids below mintedClientFloor, which no mint gives
// out: never for an id a mint gave anyone. Below it nothing is refused, an id another person
// claimed included - a claim is known to the table alone, which nothing reads for an awareness
// frame - so such a page can show a cursor under another person's claimed id, though its edit
// under that id is refused (judgeClients).
func (c *socketClients) announces(client uint64) bool {
	if c.legacy {
		return client < mintedClientFloor
	}
	return c.isOwn(crdt.ClientID(client))
}

// admitClients is a document socket's client ids, decided by its admission once writable says
// whether its page may write (decideWritable): none for a page that presented no
// DocumentClientIdParameter; otherwise the id the page presented where the table names the same
// person for it, or, for a page that may write, one below mintedClientFloor that no row names and
// the page may claim (claimable: a page of this build whose first socket reached a process of the
// earlier build and kept the id its document drew), or, for a page that may not, one from
// mintedClientFloor up that no row names (keepsClient: the id an earlier read-only admission sent
// it); and in every other case an id given out afresh (mintClient) to a page that may write - the
// one it presented first, where that is in [mintedClientFloor, mintedClientCeiling) - and to one
// that may not an id drawn as a mint draws it that is stored nowhere and claims nothing
// (drawClient). A read-only page writes under no id - ygo hands the hook nothing it sends - so a
// read-only admission adds no row, and the page that is admitted writable later presents that
// id, finds no row and is minted it, so it keeps the id its document has written under as it
// read, and what it wrote lands (client_ids.go says why minting a presented id gives the page
// nothing of anyone's).
func (s *Service) admitClients(ctx context.Context, room string, actor model.Actor, r *http.Request, writable bool) (*socketClients, error) {
	presented, takes := r.URL.Query()[contracts.DocumentClientIdParameter]
	if !takes {
		return &socketClients{legacy: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, clientCheckTimeout)
	defer cancel()
	// asked is the presented id a mint gives out first, where it is one a mint could draw.
	var asked crdt.ClientID
	if presented[0] != "" {
		if client, err := strconv.ParseUint(presented[0], 10, 64); err == nil {
			kept, err := s.keepsClient(ctx, room, actor, client, writable)
			if err != nil || kept != nil {
				return kept, err
			}
			if client >= mintedClientFloor && client < mintedClientCeiling {
				asked = crdt.ClientID(client)
			}
		}
	}
	if !writable {
		client, err := drawClient()
		if err != nil {
			return nil, err
		}
		return &socketClients{own: client}, nil
	}
	client, err := s.mintClient(ctx, room, actor, asked)
	if err != nil {
		return nil, err
	}
	return &socketClients{own: client, stored: true}, nil
}

// keepsClient is the socket's client ids where a page may keep client, the id it presented, and
// nil where it may not. It may keep an id the table names actor for, which this records as a
// presentation (presented_at) in the same statement, reading back whether an edit landed under
// it (socketClients.landed), so a socket whose id has landed an edit already writes nothing more
// on its first edit; a read-only page keeps such an id too, which adds no row. A page that may
// write (writable) may keep an id below mintedClientFloor no row names that actor may claim
// (claimable, as a page of this build), which this claims from the clock the document holds it
// to, in the stored history or the room where this server holds it, held to maxClaimsADay with
// actor's other claims in the document (claimClients): a claim it refuses keeps nothing, and the
// page is given an id afresh.
//
// A page that may not write keeps an id from mintedClientFloor up that no row names: the one an
// earlier read-only admission drew for it (drawClient), stored nowhere and claiming nothing. The
// page's document writes under the id it holds as it reads - the dashboard's editor gives the
// document's headings ids (e2e/doc.e2e.ts) - and a read-only room takes none of that, so a
// read-only reconnect that drew it another id would find the document had written under the
// first, and the page would rebuild it at every such reconnect, telling a reader who typed
// nothing that it held edits under an editing identity it may not write under (takeClientId, in
// the dashboard's connection.ts; e2e/doc-reload-line.e2e.ts). A read-only socket carries nothing
// under the id it keeps: ygo hands the hook none of its updates and discards its awareness
// (awareness_limit.go). One a row names for another person is not kept all the same.
//
// An id at mintedClientCeiling or above is none of these: no admission gives one out and no
// claim takes one, so it is not kept and the table is not asked, as a presented id that does not
// read is not.
func (s *Service) keepsClient(ctx context.Context, room string, actor model.Actor, client uint64, writable bool) (*socketClients, error) {
	if client >= mintedClientCeiling {
		return nil, nil
	}
	kept := &socketClients{own: crdt.ClientID(client), stored: true}
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
	case client >= mintedClientFloor && writable, client < mintedClientFloor && !writable:
		// A writable page's id from mintedClientFloor up that no row of actor's names is a mint's
		// to give out, which takes it first where no row names it (admitClients, mintClient).
		// Below it, only a page that may write claims an id.
		return nil, nil
	}
	owners, err := s.clientOwners(ctx, room, []uint64{client})
	if err != nil {
		return nil, err
	}
	if _, bound := owners[client]; bound {
		return nil, nil
	}
	if !writable {
		return &socketClients{own: crdt.ClientID(client)}, nil
	}
	live := s.srv.GetDoc(room)
	heldTo, err := s.claimable(ctx, room, live, heldBy(live), actor, false, client)
	var claimed []uint64
	if err == nil {
		claimed, err = s.claimClients(ctx, room, actor, []uint64{client}, []uint64{heldTo})
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

// drawClient is a random id in [mintedClientFloor, mintedClientCeiling), as a mint draws one.
func drawClient() (crdt.ClientID, error) {
	var drawn [8]byte
	if _, err := rand.Read(drawn[:]); err != nil {
		return 0, err
	}
	return crdt.ClientID(mintedClientFloor + binary.BigEndian.Uint64(drawn[:])%(mintedClientCeiling-mintedClientFloor)), nil
}

// errMintsLive is a mint refused because actor holds maxUnlandedMintsADocument rows in the
// document that no edit landed under and every one of them is live (storeMint). The admission, or
// the browser update whose id the judgement mints (judgeClients), is refused with the transient
// DocumentBusyCloseCode, which the editor retries on its backoff: by then a row whose socket has
// gone has lapsed, or the person's open pages have typed.
var errMintsLive = errors.New("dispatch: every client id the person holds in the document unlanded is held by an open socket")

// mintClient gives actor a client id for room, stored before it is returned (storeMint): asked,
// where it is not 0 - the id the page presented, in [mintedClientFloor, mintedClientCeiling),
// which no row of actor's names (admitClients) - and otherwise, or where another person's row
// names asked, a random id in that range the document has not given out (drawClient). An asked
// id no row names is the one an earlier read-only admission drew for the page, or the page's own
// whose row a give-back or the sweep took; one another person's row names stores nothing, as a
// draw the document gave out already does, and the mint draws.
func (s *Service) mintClient(ctx context.Context, room string, actor model.Actor, asked crdt.ClientID) (crdt.ClientID, error) {
	for try := range mintTries {
		client := asked
		if try > 0 || asked == 0 {
			drawn, err := drawClient()
			if err != nil {
				return 0, err
			}
			client = drawn
		}
		stored, err := s.storeMint(ctx, room, actor, client)
		if err != nil {
			return 0, err
		}
		if stored {
			return client, nil
		}
	}
	return 0, fmt.Errorf("give out a client id: %d draws were all taken", mintTries)
}

// storeMint stores client, an id in [mintedClientFloor, mintedClientCeiling), as actor's in room,
// minted as an admission gives one out, and reports whether it did: an id a row names already,
// actor's or another person's, stores nothing. The row's key is the binding's own lock, so of two
// mints of one id, by an admission or a browser update's judgement (judgeClients), the first
// stores it and the second stores nothing.
//
// It is held to maxUnlandedMintsADocument: where actor holds that many rows in room of ids a
// mint gave out that no edit landed under, the same statement gives back the one presented
// least recently and stores the new one, and where none of them is old enough to give back,
// stores nothing and answers errMintsLive. A row is old enough once clientPresenceLapses
// intervals have passed since it was last presented: every open socket presents its row again
// each interval until an edit lands under it (keepPresenting), so no row an open socket holds,
// on this process or another, is given back, and a row whose socket has gone lapses within those
// intervals. Both sides read Postgres's own clock, so no process's clock skew moves the line. The
// count, the give-back and the insert are one statement behind the lock of actor's mints in room
// (clientClaimsLockNamespace, with a key of its own), in the one implicit transaction a batch
// runs in, so two mints of one person in one document count each other's rows; the statement
// reads them off document_client_ids_unlanded_mints. The delete reads its row's presentation and
// landing again as it deletes, so a row presented or landed since the statement's snapshot is
// not given back. An id the document gave out already stores nothing, a row given back with it
// or not.
//
// A landed row is not held to any count of its own: it is the record of whose structs the stored
// history holds under its id, and is never given back or swept. A person adds one for each page
// that writes under a fresh id, and the history gains that client's structs beside it, so such
// rows grow with the history and stop with it at the decode cap (maxUpdateItems).
func (s *Service) storeMint(ctx context.Context, room string, actor model.Actor, client crdt.ClientID) (bool, error) {
	lapse := clientPresenceLapses * presenceInterval()
	var atCap bool
	var givenBack, minted *int64
	batch := &pgx.Batch{}
	batch.Queue(`select pg_advisory_xact_lock($1, hashtext($2::text)) as client_mints_lock`,
		clientClaimsLockNamespace, "mints "+room+" "+ActorRef(actor))
	batch.Queue(`
		with unlanded as (
			select client_id, presented_at from document_client_ids
			where artifact_id = $1 and actor_kind = $3 and actor_id = $4 and minted and not landed
		), cap as (
			select count(*) >= $5::bigint as reached from unlanded
		), given_back as (
			delete from document_client_ids
			where artifact_id = $1 and actor_kind = $3 and actor_id = $4 and minted and not landed
				and presented_at < now() - $6::bigint * interval '1 microsecond'
				and client_id = (
					select client_id from unlanded
					where (select reached from cap) and presented_at < now() - $6::bigint * interval '1 microsecond'
					order by presented_at, client_id
					limit 1
				)
			returning client_id
		), minted as (
			insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted)
			select $1, $2::bigint, $3, $4, true
			where not (select reached from cap) or exists (select 1 from given_back)
			on conflict do nothing
			returning client_id
		)
		select (select reached from cap), (select client_id from given_back), (select client_id from minted)
	`, room, int64(client), actor.Kind, actor.ID, int64(maxUnlandedMintsADocument), lapse.Microseconds()).QueryRow(func(row pgx.Row) error {
		return row.Scan(&atCap, &givenBack, &minted)
	})
	if err := s.store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return false, fmt.Errorf("give out a client id: %w", err)
	}
	if givenBack != nil {
		slog.Info("dispatch: gave back a person's least recently presented client id no edit landed under, to give out another",
			"room", room, "given_back", *givenBack, "most", maxUnlandedMintsADocument)
	}
	if minted == nil && atCap && givenBack == nil {
		return false, errMintsLive
	}
	return minted != nil, nil
}

// keepPresenting presents, every presenceInterval while clients' socket is open, each id it holds
// whose row does not say an edit landed under it (socketClients.presenting): its own id, and each
// other id whose row awaits its mark (socketClients.unmarked) - a closed tab's id its page replays
// saved rows under, minted for it by that replay (mintForSender) or named its person's
// (judgeClients) - so each such row stays live and no mint gives it back (storeMint), on this
// process or another, while the socket that writes under it is open; the judgement presents such
// an id as the socket takes it up (presentRows), so its row is live from then to the first
// interval. Otherwise the row of such an id would lapse while the socket kept writing under it,
// a mint of its person's would give it back, and another person's update could be minted the id.
// A socket whose own id no row names - a legacy socket, a read-only admission's drawn id -
// presents no own id, and a read-only socket holds no other id to present, since ygo hands the
// hook none of its updates, so ServeHTTP starts this for a read-only socket only where it presents
// its own id. Each presentation is one timer and, where any id awaits it, one statement for all
// of them; the timer is set again only once the statement has returned, while the socket is open,
// since an id it holds can await a mark whatever it held before: a mark in flight, or one the
// store refused, leaves the id presented. It returns what the socket's end runs, which stops it.
func (s *Service) keepPresenting(room string, actor model.Actor, clients *socketClients) (end func()) {
	interval := presenceInterval()
	var mu sync.Mutex
	ended := false
	mu.Lock()
	defer mu.Unlock()
	var timer *time.Timer
	timer = time.AfterFunc(interval, func() {
		if presented := clients.presenting(); len(presented) > 0 {
			s.presentClients(room, actor, presented)
		}
		mu.Lock()
		defer mu.Unlock()
		if !ended {
			timer.Reset(interval)
		}
	})
	return func() {
		mu.Lock()
		defer mu.Unlock()
		ended = true
		timer.Stop()
	}
}

// presentClients records that an open socket still holds clients, actor's ids in room that no
// edit landed under, as the table's queries take them (presentRows). A write that fails is
// logged, and the next interval presents the rows again.
func (s *Service) presentClients(room string, actor model.Actor, clients []int64) {
	ctx, cancel := context.WithTimeout(context.Background(), clientCheckTimeout)
	defer cancel()
	if _, err := s.presentRows(ctx, room, actor, clients); err != nil {
		slog.Warn("dispatch: could not present an open socket's client ids again; the next interval presents them",
			"room", room, "clients", len(clients), "error", err)
	}
}

// presentRows records that a socket holds clients, actor's ids in room that no edit landed under,
// as an admission that gives an id back records it (presented_at), in one statement, and returns
// how many rows it moved: a row an edit has landed under since is left as it is, and a row a
// give-back took is not there to move. An open socket's presentation each interval runs it
// (presentClients), and so does the judgement of the update a socket takes an id up with
// (judgeClients).
func (s *Service) presentRows(ctx context.Context, room string, actor model.Actor, clients []int64) (int64, error) {
	tag, err := s.store.Pool.Exec(ctx, `
		update document_client_ids set presented_at = now()
		where artifact_id = $1 and client_id = any($2) and actor_kind = $3 and actor_id = $4 and not landed
	`, room, clients, actor.Kind, actor.ID)
	return tag.RowsAffected(), err
}
