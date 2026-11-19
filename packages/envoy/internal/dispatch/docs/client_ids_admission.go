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
// keeps a minted id's row live while its socket is open (keepPresenting).

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
// for a legacy socket and for a read-only admission's id, drawn and stored nowhere. landed is
// whether own's row says an edit landed under it: read at the admission (keepsClient), and set
// once this socket has marked the row after its first apply that left the room holding a struct
// under own (recordLanded), which it does once.
type socketClients struct {
	legacy bool
	own    crdt.ClientID
	stored bool
	landed atomic.Bool
	// mu guards confirmed and claims: the hook changes them on the peer's read loop, and nothing
	// else reads them, so the lock is never contended.
	mu        sync.Mutex
	confirmed map[crdt.ClientID]struct{}
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

// holds is whether client is one this socket may write under without asking the table: the id
// its page was given, or one an earlier update showed is this person's.
func (c *socketClients) holds(client crdt.ClientID) bool {
	if c.isOwn(client) {
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
// no id here, and carries awareness only for ids below mintedClientFloor, which no admission
// gives out: never for an id Dispatch gave anyone.
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
// earlier build and kept the id its document drew); and in every other case an id given out
// afresh (mintClient) to a page that may write, and to one that may not an id drawn as a mint
// draws it that is stored nowhere and claims nothing (drawClient). A read-only page writes under
// no id - ygo hands the hook nothing it sends - so a read-only admission adds no row, and the
// page that is admitted writable later presents that id, finds no row and is given one afresh.
func (s *Service) admitClients(ctx context.Context, room string, actor model.Actor, r *http.Request, writable bool) (*socketClients, error) {
	presented, takes := r.URL.Query()[contracts.DocumentClientIdParameter]
	if !takes {
		return &socketClients{legacy: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, clientCheckTimeout)
	defer cancel()
	if presented[0] != "" {
		if client, err := strconv.ParseUint(presented[0], 10, 64); err == nil {
			kept, err := s.keepsClient(ctx, room, actor, client, writable)
			if err != nil || kept != nil {
				return kept, err
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
	client, err := s.mintClient(ctx, room, actor)
	if err != nil {
		return nil, err
	}
	return &socketClients{own: client, stored: true}, nil
}

// keepsClient is the socket's client ids where a page may keep client, the id it presented, and
// nil where it may not. It may keep an id the table names actor for, which this records as a
// presentation (presented_at) in the same statement, reading back whether an edit landed under
// it (socketClients.landed), so a socket whose id has landed an edit already writes nothing more
// on its first edit; a read-only page keeps such an id too, which adds no row. And a page that
// may write (writable) may keep an id below mintedClientFloor no row names that actor may claim
// (claimable, as a page of this build), which this claims from the clock the document holds it
// to, in the stored history or the room where this server holds it, held to maxClaimsADay with
// actor's other claims in the document (claimClients): a claim it refuses keeps nothing, and the
// page is given an id afresh. An id at mintedClientCeiling or above is neither: no admission
// gives one out and no claim takes one, so it is not kept and the table is not asked, as a
// presented id that does not read is not.
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
	case client >= mintedClientFloor || !writable:
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
// document that no edit landed under and every one of them is live (mintClient). The admission is
// refused with the transient DocumentBusyCloseCode, which the editor retries on its backoff: by
// then a row whose socket has gone has lapsed, or the person's open pages have typed.
var errMintsLive = errors.New("dispatch: every client id the person holds in the document unlanded is held by an open socket")

// mintClient gives actor a client id for room, stored before it is returned: a random id in
// [mintedClientFloor, mintedClientCeiling) the document has not given out (drawClient).
//
// It is held to maxUnlandedMintsADocument: where actor holds that many rows in room of ids an
// admission gave out that no edit landed under, the same statement gives back the one presented
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
// not given back. A draw of an id the document gave out already stores nothing and is drawn
// again, a row given back with it or not.
func (s *Service) mintClient(ctx context.Context, room string, actor model.Actor) (crdt.ClientID, error) {
	lapse := clientPresenceLapses * presenceInterval()
	for range mintTries {
		client, err := drawClient()
		if err != nil {
			return 0, err
		}
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
			return 0, fmt.Errorf("give out a client id: %w", err)
		}
		if givenBack != nil {
			slog.Info("dispatch: gave back a person's least recently presented client id no edit landed under, to give out another",
				"room", room, "actor", ActorRef(actor), "given_back", *givenBack, "most", maxUnlandedMintsADocument)
		}
		switch {
		case minted != nil:
			return client, nil
		case atCap && givenBack == nil:
			return 0, errMintsLive
		}
	}
	return 0, fmt.Errorf("give out a client id: %d draws were all taken", mintTries)
}

// keepPresenting presents clients' own id again every presenceInterval while its socket is open
// and no edit has landed under it (presentClient), so its row stays live and no mint gives it
// back (mintClient), on this process or another. A socket whose id no row of its own names - a
// legacy socket, a read-only admission's drawn id - presents nothing, and nor does one whose row
// says an edit landed already, which no mint gives back. Each presentation is one timer and one
// statement, and the timer is set again only once the statement has returned and while no edit
// has landed under the id. It returns what the socket's end runs, which stops it.
func (s *Service) keepPresenting(room string, actor model.Actor, clients *socketClients) (end func()) {
	if !clients.stored || clients.landed.Load() {
		return func() {}
	}
	interval := presenceInterval()
	var mu sync.Mutex
	ended := false
	mu.Lock()
	defer mu.Unlock()
	var timer *time.Timer
	timer = time.AfterFunc(interval, func() {
		if clients.landed.Load() {
			return
		}
		s.presentClient(room, actor, clients.own)
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

// presentClient records that an open socket still holds client, actor's id in room that no edit
// landed under, as an admission that gives an id back records it (presented_at). A row an edit
// has landed under since is left as it is. A write that fails is logged, and the next interval
// presents the row again.
func (s *Service) presentClient(room string, actor model.Actor, client crdt.ClientID) {
	ctx, cancel := context.WithTimeout(context.Background(), clientCheckTimeout)
	defer cancel()
	if _, err := s.store.Pool.Exec(ctx, `
		update document_client_ids set presented_at = now()
		where artifact_id = $1 and client_id = $2 and actor_kind = $3 and actor_id = $4 and not landed
	`, room, int64(client), actor.Kind, actor.ID); err != nil {
		slog.Warn("dispatch: could not present an open socket's client id again; the next interval presents it",
			"room", room, "client", uint64(client), "error", err)
	}
}
