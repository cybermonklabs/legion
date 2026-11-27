package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An issue's route says where its messages go; whether that reaches anyone is a separate fact,
// and an issue routed to a role nobody holds reads as owned while nobody is working it. So every
// issue read carries route_status beside the route, judged against the listener's live-session
// list (GET /v1/sessions, each row carrying the roles its session holds), never one lookup per
// issue. Every process judges against one stored copy of that list, the route_roster row
// (migration 0159), and GET /api/v1/agents (listAgents) lists the sessions of that same copy: a
// read uses it while it is fresh by the database's clock, so a route_status or an agents list can
// be up to routeRosterTTL (5 s) old, and inside that age every process reads a route the same way,
// and a filtered listing paged across processes is cut from one answer. The copy is refreshed
// ahead of that bound, apart from every read: each process runs one refresh loop
// (refreshRouteRosters), started by its first roster read, which asks the listener once the stored
// copy is within the fetch bound (routeRosterTimeout, 2 s) of its expiry, read or no read, and
// stores the answer for every process; so while the listener answers within that bound, no read
// finds the copy past its 5 s. No read waits on the listener: a read that finds the copy past its
// bound reads every route unknown at once, and an agents list answers 503 at once, save the first
// agents list of a process that has built no copy yet, which waits for the first fetch within
// the fetch bound (awaitFirstRouteRoster). Like fetchHolderLiveness, a listener call runs with no
// transaction open (envoy_resolve.go carries the reason): the loop reads and writes the row on the
// background pool, and a read that waits on the listener or asks it itself (that first agents
// list, the shutdown drain's, every read of a ListenerRosterUncached process) holds no admission
// unit and no pinned connection across the wait (awayOnNetwork). Since a read may
// still ask the listener so, listIssues and getIssue read the roster only once their pooled reads
// have finished, listAgents before its two aggregates run, and a new caller must do the same.
//
// no_holder is one read and says only "nobody right now": a session that restarts or moves
// between agent boxes keeps its id but is absent from the listener for minutes. The read does not
// try to tell that gap from a vacancy with a last-seen time, because no such time comes with the
// roster: GET /v1/roles/{role} does report a lapsed holder's last_seen, and takes nothing from
// that holder to do it (its claim_released is always false), but it answers for one role per
// call, which is the lookup per issue this file's whole design refuses, and the last-seen memory
// behind it is the answering listener's own process cache, not the shared registry. The owner
// audit (skills/legion-controller/references/backlog-stewardship.md) and
// `dispatch issues --help` (route_status) ask for two reads ten minutes apart instead.

// routeRoster is one listener answer about who is running. answered is false when the listener
// is unconfigured or gave no list; every route then reads unknown, and reason says why:
// model.RouteReasonRefilling for the listener's refilling answer, whose Retry-After rides in
// retryAfter, model.RouteReasonUnreachable for anything else. Both ride on the stored row with
// the answer they came with, so a refill one process met is a refill on every process
// (dispatch://INFRA-261). Its maps and its session slice are read-only once built, since every
// read in this process served from one stored answer shares them (rosterCache.built): a reader
// that sorts or edits the sessions copies them first, and never writes a session's Roles or
// Capabilities, which every copy shares too.
type routeRoster struct {
	answered bool
	// reason is why an unanswered roster gave no list: model.RouteReasonRefilling, with the
	// listener's Retry-After in retryAfter, or model.RouteReasonUnreachable. Both are stored on
	// the route_roster row beside the answer, so every process reads one silence the same way.
	reason     string
	retryAfter string
	// sessions is the listener's answer, in the listener's order. It carries the unresponsive
	// sessions as well as the live ones (the client asks with include=unresponsive), so a row's
	// presence is no longer liveness: every reader tests the row's State.
	sessions []envoy.Session
	// entries maps a session id to the index of its row in sessions, which holds the row itself:
	// a roster of a few hundred sessions keeps one copy of each. A session the registry holds no
	// record of, live or unresponsive, is simply absent, which is the one membership test left
	// in this file and means gone.
	entries map[string]int
	// holders maps a role to the session holding it, whatever that session's state. The listener
	// gives a role one holder (last claim wins); should two rows ever name one role, the most
	// recently seen session is named, and the id breaks a tie, so a read never flips between
	// them - and the most recently seen is the live one when a lapsed claim lingers beside it.
	holders map[string]string
	// err is why the roster did not answer: the listener client's own error when this process's
	// fetch got it, errListenerSilence or errListenerRefill when the silence was read from a row
	// another fetch stored, errRouteRosterPastBound when no stored answer is within its bound, or,
	// for a cold process's first agents list whose request ended, or whose first fetch did not
	// answer within the fetch bound, while it waited (awaitFirstRouteRoster), envoy.ErrUnavailable
	// wrapping which. It matches envoy.ErrRefilling when reason is model.RouteReasonRefilling and
	// envoy.ErrUnavailable otherwise. It is nil when the roster answered or the listener is
	// unconfigured.
	err error
}

// state is the state the roster holds for a session: the row's own, or envoy.StateGone when the
// roster holds no row for it. The roster lists every session the registry holds a record of,
// live and unresponsive alike, so an absent row is a session the registry has no record of -
// which is exactly gone. It answers for an unanswered roster too, and a caller that has not
// checked answered must not read that as gone.
func (r routeRoster) state(sessionID string) string {
	index, held := r.entries[sessionID]
	if !held {
		return envoy.StateGone
	}
	return r.sessions[index].State
}

// errListenerSilence is the err of a stored silence this process did not fetch itself: the row
// says only that the listener failed or did not answer, not the error the fetching process saw.
var errListenerSilence = fmt.Errorf("%w: the listener did not answer Dispatch's last read of its sessions", envoy.ErrUnavailable)

// errListenerRefill is the err of a stored refilling answer this process did not fetch itself:
// the row says the listener refused its session list while it refills, not the error the
// fetching process saw. Like the listener's own refusal it matches envoy.ErrRefilling and not
// envoy.ErrUnavailable, and the Retry-After the listener sent is the roster's retryAfter.
var errListenerRefill = fmt.Errorf("%w: the listener was refilling its sessions at Dispatch's last read of them", envoy.ErrRefilling)

// errRouteRosterPastBound is the err of routeRosterPastBound: no stored answer within its bound,
// so the read says nothing about the listener's sessions rather than wait for a fresh one.
var errRouteRosterPastBound = fmt.Errorf("%w: Dispatch's stored copy of the listener's sessions is missing or older than its bound; it is refreshed apart from this read", envoy.ErrUnavailable)

// routeRosterPastBound is the roster a read is served when no stored answer is within its bound:
// unanswered, for the listener's unreachable reason, so an issue read reads every route unknown
// and an agents list answers 503 ENVOY_UNAVAILABLE, both at once. Its maps are nil, and like every
// roster it is read-only, so every read shares it.
var routeRosterPastBound = routeRoster{reason: model.RouteReasonUnreachable, err: errRouteRosterPastBound}

// routeRosterTimeout bounds the one listener read a roster fetch makes. The listener answers in
// milliseconds; a listener that has stopped answering would otherwise hold the fetch for the
// client's whole five-second window. It is the refresh's lead too (refreshLead): the loop asks the
// listener this long before the stored answer expires, so a fetch that answers within it replaces
// the answer before any read finds it past its TTL. And it bounds the one read that ever waits on
// a fetch, a cold process's first agents list (awaitFirstRouteRoster).
const routeRosterTimeout = 2 * time.Second

// routeRosterTTL is how long one listener answer serves every process's issue reads and agents
// lists. A route that reads no_holder already means only "nobody right now" (above), and the
// owner audit reads ten minutes apart, so five seconds of age changes no decision, nor does it
// for an agents list a human or a session reads to pick a target. The answer is refreshed
// routeRosterTimeout before it reaches that age, so while the listener answers within that bound
// every read finds an answer at most 5 s old, at a cost of one listener call per process every
// 3 s (the TTL less the bound) whatever the reads, and of one decode of the whole fleet's list per
// answer per process. A test process may set another (Deps.RouteRosterTTL).
// routeRosterSilenceTTL is how long a listener that failed or did not answer is left alone: every
// read in that window, on every process, answers at once, an issue read with every route unknown
// and an agents list with a 503 ENVOY_UNAVAILABLE, and the loop asks the listener again only
// routeRosterTimeout before the window ends. A failed fetch serves that failure, never an older
// answer, so this window changes no answer's age, and a listener that hangs is asked once per
// window (5 s) and holds no read: the reads during the hung fetch are served the silence it
// replaces. A route_status, unknown included, then lags the listener by at most routeRosterTTL. A
// listener refilling its list is left alone for the silence window like any other unanswered
// fetch: its reason, and the Retry-After it sent, are the roster's and reach every read the window
// serves.
const (
	routeRosterTTL        = 5 * time.Second
	routeRosterSilenceTTL = 5 * time.Second
)

// slowRouteRosterFetch is how long a fetch may take before it is logged (logSlowRouteRosterFetch):
// half the fetch bound, so a listener slowing towards that bound shows in the log before reads
// begin to find the answer past its TTL.
const slowRouteRosterFetch = time.Second

// rosterCache is how this process reads and refreshes the stored listener answer: the TTLs it
// stores an answer under, its refresh loop and the fetch that loop runs, and the roster it last
// built from the stored row, so a read of a row that has not changed decodes nothing.
type rosterCache struct {
	// ttl and silenceTTL are how long an answered and an unanswered fetch's stored answer serves
	// reads; register sets routeRosterTTL (or Deps.RouteRosterTTL) and routeRosterSilenceTTL.
	// Zero keeps nothing past the fetch, so an answer of that kind has no refresh point ahead of
	// its expiry: the loop fetches it again only when a cold agents list asks (refreshRouteRosters),
	// never on its own, which would ask the listener without pause, and every read in between
	// finds no answer within its bound.
	ttl        time.Duration
	silenceTTL time.Duration
	// timeout overrides the bound on a fetch's listener read, and with it the refresh's lead and a
	// cold agents list's wait; zero, which every server the deployment builds leaves it, means
	// routeRosterTimeout. Only a test that holds a fetch open sets it, so how long it holds the
	// fetch is its own choice rather than a race against two seconds of wall clock.
	timeout time.Duration

	// refresh starts the refresh loop once, at the first roster read (startRouteRosterRefresh):
	// never when register builds the server, since a test sets ttl and timeout on the server
	// register built before its first read, and a loop running by then would read them as the
	// test writes them.
	refresh sync.Once
	// wake asks the loop for a fetch now: a cold agents list sends on it once it has asked for the
	// fetch it waits on (ask). startRouteRosterRefresh makes it, buffered one, before the loop runs.
	wake chan struct{}

	mu sync.Mutex
	// built is the roster this process last stored (storeRouteRoster) or built from a stored
	// answer within its bound, and builtSent that answer's sent_at, zero before any - a process
	// that is still cold (readAgentsRoster): a read of a row still carrying that sent_at takes
	// built rather than reading and decoding the row's sessions again.
	built     routeRoster
	builtSent time.Time
	// fetch is the fetch a cold agents list waits on: the loop's in flight, or one a list asked for
	// that the loop has yet to run; nil when there is neither.
	fetch *rosterFetch
}

// rosterFetch is one listener read of the refresh loop, which a cold agents list waits on
// (awaitFirstRouteRoster). roster is set before done closes.
type rosterFetch struct {
	done   chan struct{}
	roster routeRoster
}

// readRouteRoster answers who is running from the stored answer, and never waits on the listener:
// the stored answer while it is within its bound (fresh by the database's clock), else
// routeRosterPastBound, every route unknown at once, with an err that says the copy is past its
// bound. A stored silence within its bound is served as it stands: unanswered, with the reason
// the fetching process stored. Its first call starts this process's refresh loop
// (startRouteRosterRefresh), which keeps the stored answer within its bound from then on, read or
// no read. What an unanswered roster means is the caller's: an issue read reads every route
// unknown, with the roster's reason, and still succeeds, and a judged route_status filter refuses,
// while listAgents, which has nothing to list without the listener, reads through
// readAgentsRoster. A read whose own read of the row fails answers that error: the failure is the
// database's, and a caller that filters by a judged state or lists the sessions answers it as one
// rather than as the listener's silence, while a caller that only shows a route may read it
// unknown (routeRosterOrUnknown).
func (s *server) readRouteRoster(ctx context.Context) (routeRoster, error) {
	roster, _, err := s.currentRouteRoster(ctx)
	return roster, err
}

// currentRouteRoster is readRouteRoster answering also whether the roster stands within its
// bound: false only for routeRosterPastBound, and for a failed read of the row.
func (s *server) currentRouteRoster(ctx context.Context) (roster routeRoster, within bool, err error) {
	if s.deps.Envoy == nil {
		return routeRoster{reason: model.RouteReasonUnreachable}, true, nil
	}
	if s.deps.ListenerRosterUncached {
		// An end-to-end test process: every read asks the listener itself, with no stored answer,
		// no refresh loop and nothing kept.
		roster, err := s.listRouteRosterAway(ctx)
		return roster, true, err
	}
	s.startRouteRosterRefresh()
	roster, row, err := s.storedRouteRoster(ctx, s.deps.Store.Pool)
	if err != nil {
		return routeRoster{}, false, fmt.Errorf("read the stored listener answer: %w", err)
	}
	if !row.fresh() {
		return routeRosterPastBound, false, nil
	}
	return roster, true, nil
}

// readAgentsRoster is readRouteRoster for listAgents, which it differs from on a cold process
// alone: one that had built no roster (builtSent zero) when the list began and finds no stored
// answer within its bound - a process just started, before its first fetch has stored anything -
// waits for that first fetch (awaitFirstRouteRoster) rather than answer 503 for want of a copy it
// never had. Cold is read before the row, so a list whose read of the row the first store overtook
// still waits, and is served that stored answer. A process that has built one is served
// routeRosterPastBound at once, as every issue read is, and so is a process shutting down, whose
// loop has ended: listAgents asks the listener on its own request then.
func (s *server) readAgentsRoster(ctx context.Context) (routeRoster, error) {
	cache := &s.rosters
	cache.mu.Lock()
	cold := cache.builtSent.IsZero()
	cache.mu.Unlock()
	roster, within, err := s.currentRouteRoster(ctx)
	if err != nil || within || !cold || s.lifetime.Err() != nil {
		return roster, err
	}
	return s.awaitFirstRouteRoster(ctx)
}

// awaitFirstRouteRoster waits for the refresh loop's fetch - the one in flight, or one it asks the
// loop to run now (ask) - and answers its roster, for a cold process's agents list
// (readAgentsRoster). The wait is a wait on another server, so the request holds no admission
// unit and no pinned connection across it (awayOnNetwork): a listener slow to answer then holds
// no unit, and the reads that need no listener are not refused for want of one meanwhile. A
// take-back refused is the answer, which listAgents writes (writeHandlerError). The wait is
// bounded by the fetch bound and by the request: a first fetch that has not answered within the
// bound, and a request that ends first, leave the roster unanswered with an err that says which,
// and the fetch runs on to store its answer for every process.
func (s *server) awaitFirstRouteRoster(ctx context.Context) (routeRoster, error) {
	bound := s.rosters.fetchBound()
	fetch := s.rosters.ask()
	var roster routeRoster
	if back := awayOnNetwork(ctx, func(ctx context.Context) error {
		select {
		case <-fetch.done:
			roster = fetch.roster
		case <-ctx.Done():
			roster = routeRoster{reason: model.RouteReasonUnreachable, err: fmt.Errorf("%w: %v", envoy.ErrUnavailable, ctx.Err())}
		case <-time.After(bound):
			roster = routeRoster{reason: model.RouteReasonUnreachable,
				err: fmt.Errorf("%w: the listener did not answer Dispatch's first read of its sessions within %s", envoy.ErrUnavailable, bound)}
		}
		return nil
	}); back != nil {
		return routeRoster{}, back
	}
	return roster, nil
}

// ask answers the fetch a cold agents list waits on: the loop's in flight, or else a new one the
// loop is woken to run at once (wake).
func (c *rosterCache) ask() *rosterFetch {
	fetch := c.pending()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return fetch
}

// pending answers the fetch a cold agents list waits on, making one where there is none.
func (c *rosterCache) pending() *rosterFetch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetch == nil {
		c.fetch = &rosterFetch{done: make(chan struct{})}
	}
	return c.fetch
}

// asked is whether a cold agents list waits on a fetch the loop has yet to run.
func (c *rosterCache) asked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetch != nil
}

// settle hands roster to a cold agents list waiting on the loop's fetch, and clears that fetch:
// the answer a fetch stored (fetchRouteRoster), a stored answer within its bound another process
// stored, or the shutdown's silence once the loop has ended.
func (c *rosterCache) settle(roster routeRoster) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetch != nil {
		c.fetch.roster = roster
		close(c.fetch.done)
		c.fetch = nil
	}
}

// ttlOf is how long a stored answer of the kind answered says serves reads: ttl for the
// listener's list, silenceTTL for a silence.
func (c *rosterCache) ttlOf(answered bool) time.Duration {
	if answered {
		return c.ttl
	}
	return c.silenceTTL
}

// fetchBound is how long one listener read of the refresh loop may take (routeRosterTimeout unless
// the cache sets another).
func (c *rosterCache) fetchBound() time.Duration {
	return cmp.Or(c.timeout, routeRosterTimeout)
}

// refreshLead is how long before a stored answer of that kind expires the loop asks the listener
// again: the fetch bound, so a fetch that answers within it replaces the answer before any read
// finds it past its TTL, capped at half the TTL, so a TTL shorter than the bound (a test's) is
// still refreshed ahead of its expiry with the loop sleeping between fetches.
func (c *rosterCache) refreshLead(answered bool) time.Duration {
	return min(c.fetchBound(), c.ttlOf(answered)/2)
}

// startRouteRosterRefresh starts this process's refresh loop (refreshRouteRosters) on the server's
// lifetime at its first call; every later call costs one atomic load. The loop is counted in the
// server's roster loop work (rosterLoop), so the shutdown's wait sees it end before the pool closes
// (waitBackground), and apart from the work requests leave running (detached), which a test's
// barrier joins and which this loop, running for the server's whole lifetime, would hold open.
func (s *server) startRouteRosterRefresh() {
	cache := &s.rosters
	cache.refresh.Do(func() {
		cache.wake = make(chan struct{}, 1)
		s.rosterLoop.goDetached(func() { s.refreshRouteRosters(s.lifetime) })
	})
}

// refreshRouteRosters keeps the stored answer within its bound, apart from every read, until ctx -
// the server's lifetime - ends, one turn at a time (refreshRouteRosterTurn): a turn sleeps for as
// long as the turn before it said, and a cold agents list's ask wakes it at once. Once it ends, a
// cold agents list still waiting on a fetch it never ran is answered the shutdown's silence.
func (s *server) refreshRouteRosters(ctx context.Context) {
	cache := &s.rosters
	defer cache.settle(routeRoster{reason: model.RouteReasonUnreachable,
		err: fmt.Errorf("%w: Dispatch is shutting down", envoy.ErrUnavailable)})
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cache.wake:
		case <-timer.C:
		}
		// A select with the lifetime ended and a turn due takes either; the lifetime goes first.
		if ctx.Err() != nil {
			return
		}
		if next := s.refreshRouteRosterTurn(ctx); next >= 0 {
			timer.Reset(next)
		} else {
			timer.Stop()
		}
	}
}

// refreshRouteRosterTurn is one turn of the refresh loop, answering how long the loop sleeps
// before its next turn, or a negative duration for no turn until a cold agents list asks. It
// reads the row on the background pool, its age by the database's clock (storedRouteRoster).
// While the row has more than the lead left (refreshLead), it sleeps until the row has the lead
// left and reads it again, since another process may have refreshed it meanwhile, and a cold
// agents list waiting on a fetch is served that stored answer. Otherwise it runs one fetch
// (fetchRouteRoster), its call counted sent at that read's now(), and stores the answer for every
// process, newer winning across processes (storeRouteRoster). No row is fetched for at once. A row
// whose kind's TTL is zero is fetched for only when a cold agents list asks, since such a row has
// no refresh point ahead of its expiry (rosterCache.ttl). After a fetch the loop sleeps until
// that answer's own refresh point, however its store went, so a store that failed cannot set the
// loop fetching back to back; and a read of the row that fails is logged and tried again after the
// fetch bound.
func (s *server) refreshRouteRosterTurn(ctx context.Context) time.Duration {
	cache := &s.rosters
	background, err := s.deps.Store.Pool.Background()
	var roster routeRoster
	var row rosterRow
	if err == nil {
		roster, row, err = s.storedRouteRoster(ctx, background)
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: read the stored listener answer to refresh it; the refresh tries again after the fetch bound", "error", err)
		}
		return cache.fetchBound()
	}
	if !row.sent.IsZero() {
		lead := cache.refreshLead(row.answered)
		if row.left > lead {
			cache.settle(roster)
			return row.left - lead
		}
		if cache.ttlOf(row.answered) == 0 && !cache.asked() {
			return -1
		}
	}
	started := time.Now()
	own := s.fetchRouteRoster(ctx, background, row.now)
	ttl := cache.ttlOf(own.answered)
	if ctx.Err() != nil || ttl == 0 {
		return -1
	}
	// A list's TTL counts from its send, a silence's from now, when it became known
	// (storeRouteRosterQuery).
	from := started
	if !own.answered {
		from = time.Now()
	}
	return max(time.Until(from.Add(ttl-cache.refreshLead(own.answered))), 0)
}

// routeRosterOrUnknown is readRouteRoster for a read that only shows a route: a failed read of the
// stored answer is logged and every route reads unknown, since the issue itself was read. Its
// reason is the listener's own unreachable: an unanswered roster always names one (reach), and a
// stored answer this process cannot read is the listener's answer out of reach, whatever put it
// there. A read refused the unit it gave back to ask the listener (ListenerRosterUncached,
// listRouteRosterAway) is not read on: it stops the request's run (stopOnRefusedTakeBack), and
// admitted answers the refusal.
func (s *server) routeRosterOrUnknown(ctx context.Context) routeRoster {
	roster, err := s.readRouteRoster(ctx)
	if err != nil {
		stopOnRefusedTakeBack(ctx)
		if ctx.Err() == nil {
			slog.Warn("dispatch: routes read unknown", "error", err)
		}
		roster.answered = false
		roster.reason = model.RouteReasonUnreachable
	}
	return roster
}

// routeRosterQuery reads the stored answer and the database's now() in one statement, which
// answers one row whether route_roster holds one or not; $1 is the sent_at of the roster this
// process last built, and the sessions are left out when the row still carries it. The reason
// and its Retry-After are read only while they belong to the stored answer, which is what
// reason_sent_at records: the release before those columns updates the row without naming them,
// so its silence moves sent_at alone and reads unreachable, as that release answers it, rather
// than taking the reason of the answer before it.
const routeRosterQuery = `
	select now(), coalesce(r.answered, false), r.sent_at, r.expires_at,
	       coalesce(case when r.reason_sent_at = r.sent_at then r.reason end, ''),
	       coalesce(case when r.reason_sent_at = r.sent_at then r.retry_after end, ''),
	       case when r.sent_at is distinct from $1::timestamptz then r.sessions end
	from (values (true)) as one (present)
	left join route_roster r on true`

// rosterRow is what one read of the route_roster row says of the row itself: whether its answer
// is the listener's list or a silence, how long it has left by the database's clock (none once it
// has expired, and none for no row), that clock's now(), and the row's sent_at, zero for no row.
type rosterRow struct {
	answered bool
	left     time.Duration
	now      time.Time
	sent     time.Time
}

// fresh is whether the row is within its bound: present, and not yet expired by the database's
// clock.
func (r rosterRow) fresh() bool { return r.left > 0 }

// storedRouteRoster reads the route_roster row on pool, outside any transaction, and answers the
// roster it holds while the row is within its bound, with what the read says of the row
// (rosterRow). A row past its bound is not decoded, since no read is served it, and leaves this
// process's built roster as it was, so builtSent stays zero - the process cold - until it has
// stored an answer or built a roster from one within its bound. A request reads it on the request
// pool, through the connection it already holds; the refresh loop, which runs on the server's
// lifetime, reads and writes it on the background pool (refreshRouteRosterTurn).
func (s *server) storedRouteRoster(ctx context.Context, pool *store.Pool) (routeRoster, rosterRow, error) {
	cache := &s.rosters
	cache.mu.Lock()
	built, builtSent := cache.built, cache.builtSent
	cache.mu.Unlock()
	var known *time.Time
	if !builtSent.IsZero() {
		known = &builtSent
	}
	var row rosterRow
	var reason, retryAfter string
	var sentAt, expiresAt *time.Time
	var sessions []byte
	if err := pool.QueryRow(ctx, routeRosterQuery, known).
		Scan(&row.now, &row.answered, &sentAt, &expiresAt, &reason, &retryAfter, &sessions); err != nil {
		return routeRoster{}, rosterRow{}, err
	}
	if sentAt == nil {
		return routeRoster{}, row, nil
	}
	row.sent, row.left = *sentAt, expiresAt.Sub(row.now)
	if !row.fresh() {
		return routeRoster{}, row, nil
	}
	roster := built
	if sessions != nil {
		var err error
		if roster, err = cache.remember(row.sent, row.answered, reason, retryAfter, sessions); err != nil {
			return routeRoster{}, rosterRow{}, err
		}
	}
	return roster, row, nil
}

// remember builds the roster a stored answer holds and keeps it as this process's built roster.
// The stored sessions are envoy.Session rows; a field a row lacks reads as its zero value. A
// stored silence carries the reason the fetching process saw and the Retry-After that came with
// it, so a refill that process met reads as a refill here too; a row that carries no reason of
// its own - one the release before the reason was stored wrote - reads
// model.RouteReasonUnreachable, which is how that release answers it.
//
// A listed session that names no state is one the release before this change stored: that
// release asked the listener for the live list alone, having no include=unresponsive selector,
// so every session on its row is one it read live, and live is what that row means. Left at the
// zero state, every route built from such a row would read no_holder and every claim holder
// gone, for as long as that row serves every process - a rolling deploy reporting the whole
// fleet absent. It is the rule the listener client already applies to a listener that predates
// the field (envoy.Session's normalize), so a stateless answer means one thing wherever it is
// read. The field alone tells the two apart: a row this release stores names a state on every
// session it carries, and gone is never one of them, since a session the registry holds no
// record of is absent from the list rather than listed as gone.
func (c *rosterCache) remember(sent time.Time, answered bool, reason, retryAfter string, sessions []byte) (routeRoster, error) {
	roster := routeRoster{
		reason:     cmp.Or(reason, model.RouteReasonUnreachable),
		retryAfter: retryAfter,
		err:        errListenerSilence,
	}
	if roster.reason == model.RouteReasonRefilling {
		roster.err = errListenerRefill
	}
	if answered {
		var listed []envoy.Session
		if err := json.Unmarshal(sessions, &listed); err != nil {
			return routeRoster{}, err
		}
		for index := range listed {
			listed[index].State = cmp.Or(listed[index].State, envoy.StateLive)
		}
		roster = buildRouteRoster(listed)
	}
	c.keep(sent, roster)
	return roster, nil
}

// keep makes roster, built from the answer sent at sent, this process's built roster, unless the
// one it holds came from a later answer.
func (c *rosterCache) keep(sent time.Time, roster routeRoster) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sent.After(c.builtSent) {
		c.built, c.builtSent = roster, sent
	}
}

// fetchRouteRoster runs the refresh loop's one listener read, stores its answer for every process
// through background, and hands a cold agents list waiting on it (rosterCache.fetch) the stored
// answer, answering the listener's own. An answer's TTL counts from sent, the database's clock
// before the call, so no read is served a list older than the TTL however long the listener took;
// a silence's counts from the moment it was known, so the listener is left alone for the whole
// window after a call that timed out. A fetch that takes over slowRouteRosterFetch is logged
// (logSlowRouteRosterFetch): nothing else records the listener's pace.
//
// It is no request's work: it runs on the server's lifetime, outside admission, so it stores its
// answer through the background pool rather than the request pool, whose connections admission
// counts.
func (s *server) fetchRouteRoster(ctx context.Context, background *store.Pool, sent time.Time) routeRoster {
	cache := &s.rosters
	// A cold agents list that asks while the listener answers waits on this fetch (ask).
	cache.pending()
	started := time.Now()
	own := s.listRouteRoster(ctx)
	if took := time.Since(started); took > slowRouteRosterFetch {
		logSlowRouteRosterFetch(ctx, took, own)
	}
	cache.settle(s.storeRouteRoster(ctx, background, sent, own))
	return own
}

// logSlowRouteRosterFetch logs, once, a fetch whose listener read took over slowRouteRosterFetch:
// how long it took, and how it ended - answered with how many sessions, refilling with the
// Retry-After the listener sent, unreachable with the error, or ended by the shutdown.
func logSlowRouteRosterFetch(ctx context.Context, took time.Duration, own routeRoster) {
	const message = "dispatch: the listener's session list took over a second"
	switch {
	case own.answered:
		slog.Warn(message, "duration", took, "outcome", "answered", "sessions", len(own.sessions))
	case ctx.Err() != nil:
		slog.Warn(message, "duration", took, "outcome", "shutdown")
	case own.reason == model.RouteReasonRefilling:
		slog.Warn(message, "duration", took, "outcome", "refilling", "retry_after", own.retryAfter)
	default:
		slog.Warn(message, "duration", took, "outcome", "unreachable", "error", own.err)
	}
}

// storeRouteRosterQuery stores one listener answer unless the stored one was sent later: a call
// that answers after a later call on another process never replaces that newer answer. A
// silence's reason and the Retry-After that came with it are stored beside it, under the sent_at
// they belong to (routeRosterQuery). It returns a row only when it stored this one. Every
// parameter is cast, since $1 appears twice and $3 three times.
const storeRouteRosterQuery = `
	insert into route_roster (answered, sessions, sent_at, expires_at, reason, retry_after, reason_sent_at)
	values ($1::boolean, $2::jsonb, $3::timestamptz,
	        case when $1::boolean then $3::timestamptz + make_interval(secs => $4::float8)
	             else now() + make_interval(secs => $5::float8) end,
	        $6::text, $7::text, $3::timestamptz)
	on conflict (id) do update
	set answered = excluded.answered, sessions = excluded.sessions,
	    sent_at = excluded.sent_at, expires_at = excluded.expires_at,
	    reason = excluded.reason, retry_after = excluded.retry_after,
	    reason_sent_at = excluded.reason_sent_at
	where route_roster.sent_at < excluded.sent_at
	returning true`

// storeRouteRoster stores own, the answer of a call sent at sent, and answers the roster a cold
// agents list waiting on the fetch is served: own when it was stored, the newer stored one when
// it was not. A failed call is stored too (answered false, for the silence, with the reason it
// failed for and the Retry-After a refilling listener sent), so every process reads unknown
// together, and reads one refill as a refill. Should the database refuse the write, the list
// waiting on the fetch is served own, which no other read sees.
func (s *server) storeRouteRoster(ctx context.Context, pool *store.Pool, sent time.Time, own routeRoster) routeRoster {
	sessions := []byte("[]")
	if len(own.sessions) > 0 {
		encoded, err := json.Marshal(own.sessions)
		if err != nil {
			return own
		}
		sessions = encoded
	}
	var stored bool
	err := pool.QueryRow(ctx, storeRouteRosterQuery,
		own.answered, sessions, sent, s.rosters.ttl.Seconds(), s.rosters.silenceTTL.Seconds(),
		own.reason, own.retryAfter).Scan(&stored)
	if err == nil {
		s.rosters.keep(sent, own)
		return own
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if newer, row, err := s.storedRouteRoster(ctx, pool); err == nil && row.fresh() {
			return newer
		}
		return own
	}
	if ctx.Err() == nil {
		slog.Warn("dispatch: store the listener's answer; no read but an agents list waiting on it reads it", "error", err)
	}
	return own
}

// listRouteRoster asks the listener who is running, within routeRosterTimeout, and answers its
// roster, unanswered with the listener client's error when it did not answer.
func (s *server) listRouteRoster(ctx context.Context) routeRoster {
	ctx, cancel := context.WithTimeout(ctx, s.rosters.fetchBound())
	defer cancel()
	sessions, err := s.deps.Envoy.Sessions(ctx)
	var refilling *envoy.RefillingError
	switch {
	case errors.As(err, &refilling):
		return routeRoster{reason: model.RouteReasonRefilling, retryAfter: refilling.RetryAfter, err: err}
	case err != nil:
		return routeRoster{reason: model.RouteReasonUnreachable, err: err}
	}
	return buildRouteRoster(sessions)
}

// listRouteRosterAway is listRouteRoster for a read that holds an admission unit: it holds no
// unit and no pinned connection across the listener call (awayOnNetwork), as awaitFirstRouteRoster
// does across its wait, and answers a take-back refused as its error. The shutdown drain's agents
// list and every read of a ListenerRosterUncached process ask the listener so.
func (s *server) listRouteRosterAway(ctx context.Context) (routeRoster, error) {
	var roster routeRoster
	if back := awayOnNetwork(ctx, func(ctx context.Context) error {
		roster = s.listRouteRoster(ctx)
		return nil
	}); back != nil {
		return routeRoster{}, back
	}
	return roster, nil
}

// buildRouteRoster indexes the listed sessions by id and by the roles they hold.
func buildRouteRoster(listed []envoy.Session) routeRoster {
	roster := routeRoster{
		answered: true,
		sessions: listed,
		entries:  make(map[string]int, len(listed)),
		holders:  map[string]string{},
	}
	lastSeen := map[string]int64{}
	for index, session := range listed {
		roster.entries[session.SessionID] = index
		for _, role := range session.Roles {
			current, held := roster.holders[role]
			if held && (lastSeen[current] > session.LastSeen ||
				(lastSeen[current] == session.LastSeen && current < session.SessionID)) {
				continue
			}
			roster.holders[role] = session.SessionID
			lastSeen[session.SessionID] = session.LastSeen
		}
	}
	return roster
}

// reach judges one route against the roster: null for no route, unknown with the roster's reason
// when the listener gave no list, live with the holder when a live session receives it,
// holder_unresponsive with that same holder when the session that receives it has missed its
// heartbeats, no_holder otherwise. A stored route that no longer parses reaches nobody.
//
// An unresponsive holder is named, not dropped: the message is stored for it and waits, so a
// reader that wants "nobody is working this" must ask for no_holder, which excludes it.
func (r routeRoster) reach(route *string) model.IssueRouteReach {
	if route == nil {
		return model.IssueRouteReach{}
	}
	if !r.answered {
		return model.IssueRouteReach{
			RouteStatus: new(model.RouteUnknown), RouteStatusReason: new(r.reason),
		}
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		return model.IssueRouteReach{RouteStatus: new(model.RouteNoHolder)}
	}
	holder := ""
	switch parsed.Kind {
	case "role":
		holder = r.holders[parsed.ID]
	case "session":
		holder = parsed.ID
	}
	switch r.state(holder) {
	case envoy.StateLive:
		return model.IssueRouteReach{RouteStatus: new(model.RouteLive), RouteHolder: &holder}
	case envoy.StateUnresponsive:
		return model.IssueRouteReach{
			RouteStatus: new(model.RouteHolderUnresponsive), RouteHolder: &holder,
		}
	}
	return model.IssueRouteReach{RouteStatus: new(model.RouteNoHolder)}
}

// claimLiveness is what an issue read says about the session holding its claim: the state this
// roster read for it, when it was last seen, and from when another agent may take the claim
// without a person's force. It answers the zero value - every field null - for an issue nobody
// holds and for one a person holds, which has no session state to read.
//
// What it says about taking the claim is the taking rule itself (judgeClaim in issue_claim.go),
// never a second copy of it: the refusal a take gets and the instant this read shows a person
// are one rule read twice, so a page cannot name an instant the write then disagrees with.
//
// This is the read side of the two sources that rule has, and the slacker of them: the roster is
// the server's copy of the listener's session list, up to routeRosterTTL (5 s) old, while a
// claim write re-reads the holder's own record from the listener (fetchHolderLiveness) and
// decides on that. The two can therefore disagree by that much; a read that cannot judge at all
// answers model.ClaimHolderUnknown and never fails, because an issue page must render while the
// listener is down, and only the write refuses.
func (r routeRoster) claimLiveness(claim *model.IssueClaim, now time.Time) model.IssueClaimLiveness {
	if claim == nil || claim.Actor.Kind != "session" {
		return model.IssueClaimLiveness{}
	}
	if !r.answered {
		return model.IssueClaimLiveness{HolderState: new(model.ClaimHolderUnknown)}
	}
	held := r.state(claim.Actor.ID)
	var lastSeen int64
	if index, found := r.entries[claim.Actor.ID]; found {
		lastSeen = r.sessions[index].LastSeen
	}
	judged := judgeClaim(held, lastSeen, now)
	liveness := model.IssueClaimLiveness{HolderState: new(held)}
	if !judged.lastSeen.IsZero() {
		liveness.LastSeen = &judged.lastSeen
	}
	if !judged.takeableAt.IsZero() {
		liveness.TakeableAt = &judged.takeableAt
	}
	return liveness
}

// parseRouteStatusFilter reads the `route_status` list filter: empty for none, or one of live,
// holder_unresponsive, no_holder and unknown.
func parseRouteStatusFilter(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	switch value {
	case "", model.RouteLive, model.RouteHolderUnresponsive, model.RouteNoHolder, model.RouteUnknown:
		return value, nil
	}
	return "", errorf(http.StatusBadRequest, "INVALID_ROUTE_STATUS",
		"route_status must be %s, %s, %s or %s, got %q",
		model.RouteLive, model.RouteHolderUnresponsive, model.RouteNoHolder, model.RouteUnknown, raw)
}
