package docs

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// schemaPage is the schema a page presents on its document socket: version is C, the
// schema_version it reads, and creatable is R, the schema_creatable it may create up to. A page
// that sends no schema_creatable is a page from before the manifest, every element of which has
// since 1, so its R is 1. A page that presents no number, a number that does not parse, or a
// schema_creatable below 1, which no manifest has, is not valid, and is admitted read-only: a
// live process of a build before the fleet columns counts as reading version 0 (schemaFleet), so
// an R at or below 0 would pass that bound.
type schemaPage struct {
	version, creatable int
	valid              bool
}

func schemaPageOf(query url.Values) schemaPage {
	version, err := strconv.Atoi(query.Get("schema_version"))
	if err != nil {
		return schemaPage{}
	}
	creatable := 1
	if query.Has("schema_creatable") {
		if creatable, err = strconv.Atoi(query.Get("schema_creatable")); err != nil || creatable < 1 {
			return schemaPage{}
		}
	}
	return schemaPage{version: version, creatable: creatable, valid: true}
}

// schemaFleet is what the server processes serving the database read and create: the lowest
// schema version and the highest creatable version among this process and every live process.
// A process of a build that predates the fleet columns registers neither number
// (processes.Process.PredatesFleetColumns), and its row's zeros are what it counts as: a process
// that reads version 0. Nothing in the registry says what such a build reads, and it may read
// less than a page creates, so no page is writable while one lives. That build's own server
// answers the same way from the other side: it knows no fleet and holds read-only every page
// whose version is not exactly its own (schemaReadOnly, in the builds that carry it). A build
// that writes no row at all (every release before migration 0135_server_processes) is in no
// fleet and cannot be counted.
type schemaFleet struct {
	minVersion, maxCreatable int
}

func readSchemaFleet(ctx context.Context, db processes.Querier) (schemaFleet, error) {
	fleet := schemaFleet{minVersion: pmdoc.SchemaVersion(), maxCreatable: pmdoc.SchemaCreatable()}
	live, err := processes.Live(ctx, db)
	if err != nil {
		return schemaFleet{}, err
	}
	for _, process := range live {
		fleet.minVersion = min(fleet.minVersion, process.SchemaVersion)
		fleet.maxCreatable = max(fleet.maxCreatable, process.SchemaCreatable)
	}
	return fleet, nil
}

// fleetCacheTTL is how long the document sockets' admissions share one read of the live server
// processes (admissionFleet). What that read decides in a socket's admission is its schema
// admission alone: authorize admits the page writable only where the fleet's numbers admit it
// (schemaFleet.admits: the lowest schema version and the highest creatable version among this
// process and every live process), the answer to the socket's token is that same admission
// (authorizeSchemaVersion), and nothing else the admission decides - the caller, the issue, the
// caps, the room's load - reads it. The answer is the same for every socket of the process, so
// reading it once per socket was a request-pool read per editor that a restart's reconnect lined
// up, every one of them answering alike.
//
// A second is sound because the registry is no fresher than that already
// (internal/dispatch/processes): a process writes its schema numbers once, as it registers, before
// it serves anything, and stamps its row every HeartbeatInterval (5 s); a row counts as live while
// that stamp is within LiveWindow (15 s) of the database's now(), so a process killed without
// deregistering stays in the fleet for up to 15 s after its last heartbeat, and the prune deletes
// its row only after PruneAge (10 min). A process stopped as a deploy stops it deletes its row as
// it deregisters. And every live window each process checks its writable sockets against the
// fleet again on a read of its own, uncached (RunSchemaAdmission), so a socket admitted against a
// fleet that has since changed is closed within 15 s whether it was admitted from this answer or a
// moment before the change. An answer up to a second old adds at most that second to a staleness
// of up to 15 s the admission already lives with.
const fleetCacheTTL = time.Second

// fleetCache is the Service's last read of the live server processes, which every document
// socket's admission takes while it is younger than ttl (fleetCacheTTL), stamped with when the
// read began, so an answer is never taken for fresher than it is; and the one read in flight for
// an answer gone stale, which every admission that finds none fresh waits on (flight). A read that
// failed is not kept: its socket is refused, as every socket that waited on it is, and the next
// socket reads again.
type fleetCache struct {
	ttl    time.Duration
	mu     sync.Mutex
	fleet  schemaFleet
	readAt time.Time
	held   bool
	flight singleflight.Group
}

// fresh is the kept answer and whether it is younger than ttl at now. An answer stamped after now -
// a clock moved back under it - is not fresh.
func (c *fleetCache) fresh(now time.Time) (schemaFleet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := now.Sub(c.readAt)
	return c.fleet, c.held && age >= 0 && age < c.ttl
}

// keep records fleet, a read of the live server processes that began at readAt.
func (c *fleetCache) keep(fleet schemaFleet, readAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fleet, c.readAt, c.held = fleet, readAt, true
}

// admissionFleet is the live fleet a document socket's admission admits its page against
// (connectionState.fleet, authorize): the kept answer while it is younger than fleetCacheTTL,
// else the one read in flight, which the first socket to find no fresh answer makes on its own
// request's context and pool (queryFrom, the request pool for a socket) and the rest wait on, each
// until its own request ends. A socket whose wait ends with the read's own request - the browser
// that began it left, or the server ended that request - while its own request goes on reads again,
// so one browser's departure refuses no other socket; a read a pool's bound ended (store.IsPoolBusy)
// is every waiter's answer, and none of them is kept.
func (s *Service) admissionFleet(ctx context.Context, room string) (schemaFleet, error) {
	if fleet, fresh := s.fleets.fresh(s.now()); fresh {
		return fleet, nil
	}
	for {
		flight := s.fleets.flight.DoChan("fleet", func() (any, error) {
			if s.beforeSocketFleetRead != nil {
				s.beforeSocketFleetRead(room)
			}
			readAt := s.now()
			fleet, err := readSchemaFleet(ctx, s.queryFrom(ctx))
			if err == nil {
				s.fleets.keep(fleet, readAt)
			}
			return fleet, err
		})
		select {
		case <-ctx.Done():
			return schemaFleet{}, ctx.Err()
		case result := <-flight:
			if result.Err != nil && ctx.Err() == nil && endedWithItsRequest(result.Err) {
				continue
			}
			return result.Val.(schemaFleet), result.Err
		}
	}
}

// endedWithItsRequest is whether err is the end of the request a read ran on - a cancellation or a
// deadline of its own - rather than a bound of the server's (store.IsPoolBusy), which answers every
// request alike.
func endedWithItsRequest(err error) bool {
	return !store.IsPoolBusy(err) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

// admits reports whether page may write a document whose elements need schema version
// documentSince (pmdoc.DocumentSince) while fleet serves it: every process reads what the page
// creates (R_page <= min V), some process creates what the page creates (R_page <= max R), and
// the page represents what every process creates and what the document holds (C_page >= max(max
// R, D)). A page that cannot represent an element drops it from its copy and writes the result
// back, and a process that cannot read an element the page wrote refuses the document. The second
// bound keeps every writable page's R at or under every other writable page's C, so no writable
// page creates what another drops: after a rollback, a page of the newer build is read-only, and
// the re-check closes its socket once no live process creates what it creates.
func (f schemaFleet) admits(page schemaPage, documentSince int) bool {
	return page.valid && page.creatable <= min(f.minVersion, f.maxCreatable) && page.version >= max(f.maxCreatable, documentSince)
}

// schemaAdmissionKey is the room a document socket joined and the token its page presented, which
// is the schema_version it sends once the socket opens. It is what ygo's token hook is told about
// a socket (Service.authorizeSchemaVersion), and never identifies one: two sockets of one room can
// present one token.
type schemaAdmissionKey struct {
	room, token string
}

// schemaAdmission is one document socket's admission: what its page presented, the documentSince
// it was admitted against, whether it was admitted writable, and the cancel of the request context
// ygo serves that socket on. revoked is set once a re-check has closed the socket (revokeUnless).
// It travels on the socket's own request context (connectionState), which is how the answer to
// that socket's token finds it and no other's.
type schemaAdmission struct {
	key           schemaAdmissionKey
	page          schemaPage
	documentSince int
	writable      bool
	revoked       bool
	// cancel ends the socket's request context. Cancelling it with errSchemaRevoked makes ygo send
	// the peer that cause's close frame and close the connection (sjawhar/ygo
	// provider/websocket/server.go, ServeHTTP's context watcher).
	cancel context.CancelCauseFunc
}

// schemaAdmissions is the admission of every document socket this process serves, from authorize
// until ServeHTTP returns. Its zero value is ready to use. A token's answer never looks in here -
// it reads the admission its own socket's context carries - so this is the set a re-check walks
// (revokeUnless), and the lock is what orders a re-check's revoke against that answer.
type schemaAdmissions struct {
	mu      sync.Mutex
	sockets map[*schemaAdmission]struct{}
}

// admit records admission, once authorize has decided it, until its socket's ServeHTTP removes it.
func (a *schemaAdmissions) admit(admission *schemaAdmission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sockets == nil {
		a.sockets = make(map[*schemaAdmission]struct{})
	}
	a.sockets[admission] = struct{}{}
}

// remove forgets admission, once its socket has ended. Removing one already forgotten, or one no
// authorize ever recorded, does nothing.
func (a *schemaAdmissions) remove(admission *schemaAdmission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sockets, admission)
}

// writable is the answer to a token asked of the socket admission belongs to: read-write only when
// that socket was admitted writable, no re-check has revoked it, and the room and token asked
// about are the ones it was admitted with. Every other case is read-only: a context carrying no
// admission, a socket whose page presented another token, a socket a re-check has closed.
func (a *schemaAdmissions) writable(admission *schemaAdmission, room, token string) bool {
	if admission == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return admission.writable && !admission.revoked && admission.key == schemaAdmissionKey{room: room, token: token}
}

// revokeUnless closes every writable socket that fleet no longer admits: it cancels the socket's
// request context with errSchemaRevoked, on which ygo sends the peer that close frame and closes
// the connection. The admission stays here, marked revoked, until its own ServeHTTP removes it, so
// its token is answered read-only from then on and a later pass leaves it as it is: its connection
// is closing. It returns the sockets it closed.
func (a *schemaAdmissions) revokeUnless(fleet schemaFleet) []*schemaAdmission {
	a.mu.Lock()
	defer a.mu.Unlock()
	var revoked []*schemaAdmission
	for admission := range a.sockets {
		if admission.writable && !admission.revoked && !fleet.admits(admission.page, admission.documentSince) {
			admission.revoked = true
			revoked = append(revoked, admission)
		}
	}
	for _, admission := range revoked {
		admission.cancel(errSchemaRevoked)
	}
	return revoked
}

// RunSchemaAdmission checks, every live window of the registry (processes.LiveWindow), each
// writable document socket this process serves against the live fleet again, and closes each one
// the fleet no longer admits, until ctx ends (recheckSchemaAdmissions). A roll that follows the
// manifest's rule never changes a decision, so editors do not flip; a process that creates what an
// open page cannot represent, or that cannot read what it creates, ends that page's edits within
// a window.
func (s *Service) RunSchemaAdmission(ctx context.Context) {
	ticker := time.NewTicker(processes.LiveWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.recheckSchemaAdmissions(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("dispatch: re-check the document sockets' schema admission", "error", err)
			}
		}
	}
}

// recheckSchemaAdmissions is one pass of RunSchemaAdmission. A socket it closes gets
// contracts.DocumentAdmissionRevokedCloseCode, on which the provider reconnects, and the
// reconnect's admission decides again: read-only, unless a process that admits the page serves it.
// A pass that cannot read the live processes closes nothing. It reads them on the background pool:
// no request waits on a re-check, so it takes none of the connections admission counts for
// requests.
func (s *Service) recheckSchemaAdmissions(ctx context.Context) error {
	background, err := s.backgroundPool()
	if err != nil {
		return err
	}
	fleet, err := readSchemaFleet(ctx, background)
	if err != nil {
		return err
	}
	for _, admission := range s.schemaAdmissions.revokeUnless(fleet) {
		slog.Info("dispatch: close a writable document socket the live server processes no longer admit; the provider reconnects",
			"room", admission.key.room, "schema_version", admission.page.version, "schema_creatable", admission.page.creatable,
			"fleet_min_schema_version", fleet.minVersion, "fleet_max_schema_creatable", fleet.maxCreatable)
	}
	return nil
}

// errSchemaRevoked is the cause a re-check cancels a revoked socket's request context with. ygo
// reads CloseCode off the cause and sends the peer that close frame before it closes the
// connection; what that send waits for and what a peer too slow to take it reads instead are in
// cmd/dispatch/README.md, "Two builds at once".
var errSchemaRevoked error = schemaRevoked{}

// schemaRevoked is that cause's type: an error whose CloseCode ygo sends.
type schemaRevoked struct{}

func (schemaRevoked) Error() string {
	return "the live server processes no longer admit this document socket's page"
}

// CloseCode is the close frame ygo sends the peer: the Dispatch code on which the provider
// reconnects, and the reconnect is admitted again.
func (schemaRevoked) CloseCode() (int, string) {
	return contracts.DocumentAdmissionRevokedCloseCode, contracts.DocumentAdmissionRevokedCloseReason
}
