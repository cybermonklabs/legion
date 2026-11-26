package docs

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// keeps reports whether fleet keeps admission's socket open: a read-only one always, and a
// writable one while fleet admits its page (admits).
func (f schemaFleet) keeps(admission *schemaAdmission) bool {
	return !admission.writable || f.admits(admission.page, admission.documentSince)
}

// schemaAdmissionKey is the room a document socket joined and the token its page presented, which
// is the schema_version it sends once the socket opens. It is what ygo's token hook is told about
// a socket (Service.authorizeSchemaVersion), and never identifies one: two sockets of one room can
// present one token.
type schemaAdmissionKey struct {
	room, token string
}

// schemaAdmission is one document socket's admission: what its page presented, the documentSince
// it was admitted against, whether it was admitted writable, the person whose cookie opened it, and
// the cancel of the request context ygo serves that socket on. revoked is set once a re-check has
// closed the socket (revokeUnless). It travels on the socket's own request context
// (connectionState), which is how the answer to that socket's token finds it and no other's.
type schemaAdmission struct {
	key           schemaAdmissionKey
	page          schemaPage
	documentSince int
	writable      bool
	revoked       bool
	// person is the login the socket's admission resolved (requestActor), and accessChecks how
	// many access re-checks had begun before that admission read whether the person may read the
	// room (schemaAdmissions.accessChecks).
	person       string
	accessChecks uint64
	// cancel ends the socket's request context. Cancelling it with errSchemaRevoked or
	// errAccessRevoked makes ygo send the peer that cause's close frame and close the connection
	// (sjawhar/ygo provider/websocket/server.go, ServeHTTP's context watcher).
	cancel context.CancelCauseFunc
}

// schemaAdmissions is the admission of every document socket this process serves, from authorize
// until ServeHTTP returns. Its zero value is ready to use. A token's answer never looks in here -
// it reads the admission its own socket's context carries - so this is the set the re-checks walk
// (revokeUnless), the schema's and the access one, and the lock is what orders a re-check's revoke
// against that answer.
type schemaAdmissions struct {
	mu      sync.Mutex
	sockets map[*schemaAdmission]struct{}
	// accessChecks counts the access re-checks begun (beginAccessCheck). A socket's admission reads
	// it before it reads whether its person may read the room, and admit reports a socket whose
	// count has moved by then: a re-check that began meanwhile walked the sockets before this one
	// was among them, so the socket's read may predate the access change that re-check is for, and
	// the socket's person is read again (Service.recheckAdmission).
	accessChecks uint64
}

// admit records admission, once authorize has decided it, until its socket's ServeHTTP removes it,
// and reports whether an access re-check began after the admission's read of the room: that
// re-check never walked this socket, so the caller reads its person's access again
// (Service.recheckAdmission).
func (a *schemaAdmissions) admit(admission *schemaAdmission) (missed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sockets == nil {
		a.sockets = make(map[*schemaAdmission]struct{})
	}
	a.sockets[admission] = struct{}{}
	return admission.accessChecks != a.accessChecks
}

// accessCheckCount is how many access re-checks have begun (accessChecks).
func (a *schemaAdmissions) accessCheckCount() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessChecks
}

// beginAccessCheck counts an access re-check begun (accessChecks) and answers, by person, every
// socket held here that no re-check has closed.
func (a *schemaAdmissions) beginAccessCheck() map[string][]*schemaAdmission {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessChecks++
	held := make(map[string][]*schemaAdmission)
	for admission := range a.sockets {
		if !admission.revoked {
			held[admission.person] = append(held[admission.person], admission)
		}
	}
	return held
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

// revokeUnless closes every socket no re-check has closed yet that keep answers false for: it
// cancels the socket's request context with cause, on which ygo sends the peer the close frame
// cause names and closes the connection. The admission stays here, marked revoked, until its own
// ServeHTTP removes it, so its token is answered read-only from then on and a later pass leaves it
// as it is: its connection is closing. It returns the sockets it closed.
func (a *schemaAdmissions) revokeUnless(keep func(*schemaAdmission) bool, cause error) []*schemaAdmission {
	a.mu.Lock()
	defer a.mu.Unlock()
	var revoked []*schemaAdmission
	for admission := range a.sockets {
		if !admission.revoked && !keep(admission) {
			admission.revoked = true
			revoked = append(revoked, admission)
		}
	}
	for _, admission := range revoked {
		admission.cancel(cause)
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
// A pass that cannot read the live processes closes nothing.
func (s *Service) recheckSchemaAdmissions(ctx context.Context) error {
	fleet, err := readSchemaFleet(ctx, s.store.Pool)
	if err != nil {
		return err
	}
	for _, admission := range s.schemaAdmissions.revokeUnless(fleet.keeps, errSchemaRevoked) {
		slog.Info("dispatch: close a writable document socket the live server processes no longer admit; the provider reconnects",
			"room", admission.key.room, "schema_version", admission.page.version, "schema_creatable", admission.page.creatable,
			"fleet_min_schema_version", fleet.minVersion, "fleet_max_schema_creatable", fleet.maxCreatable)
	}
	return nil
}

// accessChanged is store.AccessChangedChannel's handler, on its room-sync lane
// (roomSync.handlers): an owner, a visibility, an access list, a group or a person's member kind
// changed, in a transaction of any process, whose commit announced it. The notification names
// nothing, so the pass reads every socket again (recheckAccess).
func (s *Service) accessChanged(ctx context.Context, _ string) {
	s.recheckAccess(ctx)
}

// RunAccessRecheck runs the access re-check (recheckAccess) once every every, until ctx ends. The
// API starts it as it mounts the document socket (api.register), on the sockets' heartbeat
// (api.Deps.StreamHeartbeat); the live socket and the Delivery views resolve their readers again
// on that beat from S4 on. An access change made by SQL alone - a statement at a psql prompt -
// appends no event and announces nothing on store.AccessChangedChannel, so no notification reaches
// room sync's access lane, and it moves what a person sees all the same: this pass is what finds
// it, a heartbeat at most after it committed, and closes each socket whose person lost its room
// with the code an announced change closes it with (errAccessRevoked). Each beat moves the access
// epoch first (accessEpoch), so every socket asks its rule again before its next data frame too
// (socketStillReads). The pass's read of each person's rooms refreshes the stored listener answer
// first where something set a refresh (readableRooms, through refreshRosterForReader), as each
// socket's ask does. A pass while this process holds no socket reads nothing. ctx is a
// process-lifetime context, outside any transaction of the pool: the passes run on a hold mark of
// their own (store.ForConcurrentWorker), as each room-sync lane does.
func (s *Service) RunAccessRecheck(ctx context.Context, every time.Duration) {
	ctx = store.ForConcurrentWorker(ctx)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.accessEpoch.move()
			s.recheckAccess(ctx)
		}
	}
}

// recheckAccess closes each document socket this process holds whose person may no longer read its
// room (errAccessRevoked): for each person with a socket here it reads what they see now
// (access.Resolve) and which of their rooms they may read (access.DocumentClause), and
// revokes the rest (revokeUnless). It runs on each access change and after each LISTEN stands,
// since a change notified while the hub had no connection reached nobody, and on every heartbeat
// of the sockets (RunAccessRecheck), since a change made by SQL alone is notified to nobody at all.
// A socket whose admission read its room before this pass began and that is recorded only after
// the pass looked is read again as it is recorded (recheckAdmission). A person whose read fails has
// every socket of theirs closed too, as recheckAdmission closes a socket whose read fails: the pass
// cannot show they still read those rooms, and each reconnect's admission reads them again.
func (s *Service) recheckAccess(ctx context.Context) {
	if s.stopping.Load() {
		return
	}
	// hidden is each socket whose person's read answered that they no longer read its room, and
	// unread each socket whose person's read failed.
	hidden := make(map[*schemaAdmission]bool)
	unread := make(map[*schemaAdmission]bool)
	for person, admissions := range s.schemaAdmissions.beginAccessCheck() {
		readable, err := s.readableRooms(ctx, person, admissions)
		if err != nil {
			if s.stopping.Load() {
				return
			}
			if ctx.Err() == nil {
				slog.Warn("dispatch: read which documents a person with an open document socket still reads; their sockets close and the provider reconnects", "sockets", len(admissions), "error", err)
			}
			for _, admission := range admissions {
				unread[admission] = true
			}
			continue
		}
		for _, admission := range admissions {
			if !readable[admission.key.room] {
				hidden[admission] = true
			}
		}
	}
	if s.stopping.Load() || (len(hidden) == 0 && len(unread) == 0) {
		return
	}
	for _, admission := range s.schemaAdmissions.revokeUnless(func(admission *schemaAdmission) bool { return !hidden[admission] && !unread[admission] }, errAccessRevoked) {
		if hidden[admission] {
			slog.Info("dispatch: close a document socket whose person no longer reads its document; the provider reconnects", "room", admission.key.room)
		}
	}
}

// recheckAdmission reads again whether admission's person may read its room, for a socket an
// access re-check never walked (schemaAdmissions.admit), and closes the socket when they may not
// (errAccessRevoked): the provider reconnects, and the reconnect is refused as a room no document
// holds is. The read begins after that re-check began, and so after the access change it is for
// committed, and a re-check that begins during it walks this socket too, so no change is missed.
// A socket of a person who still reads the room is kept, whoever's access the re-check was for. A
// read that fails closes the socket: its admission's own read is what was missed.
func (s *Service) recheckAdmission(ctx context.Context, admission *schemaAdmission) {
	readable, err := s.readableRooms(ctx, admission.person, []*schemaAdmission{admission})
	if err == nil && readable[admission.key.room] {
		return
	}
	if err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: read again whether a document socket admitted during an access re-check still reads its document", "room", admission.key.room, "error", err)
	}
	s.schemaAdmissions.revokeUnless(func(other *schemaAdmission) bool { return other != admission }, errAccessRevoked)
}

// readableRoomsStatement reads which of the documents $1 the reader bound at $2 may read.
var readableRoomsStatement = `select a.id::text from artifacts a where a.id = any($1::uuid[]) and ` + access.DocumentClause("a", "$2")

// readableRooms is the rooms of admissions, all person's sockets, that person may read now, as the
// person's browser sees them (model.BrowserActor): every document socket is a cookie's. The
// stored listener answer is refreshed first (refreshRosterForReader), since a session's file
// follows the session content rule, which reads a listener list only while that answer is fresh.
func (s *Service) readableRooms(ctx context.Context, person string, admissions []*schemaAdmission) (map[string]bool, error) {
	who := model.Actor{Kind: "user", ID: person}
	viewer, err := access.Resolve(ctx, s.store.Pool, model.BrowserActor(person))
	if err != nil {
		return nil, err
	}
	who.Viewer = &viewer
	s.refreshRosterForReader(ctx, who)
	rooms := make([]string, len(admissions))
	for i, admission := range admissions {
		rooms[i] = admission.key.room
	}
	rows, err := s.store.Pool.Query(ctx, readableRoomsStatement, rooms, refs.VisibilityArg(who))
	if err != nil {
		return nil, err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	readable := make(map[string]bool, len(held))
	for _, room := range held {
		readable[room] = true
	}
	return readable, nil
}

// refreshRosterForReader refreshes the stored listener answer (SetRosterRefresh) before a read of
// who's documents, unless who reads every document (readsEveryDocument): a session's file follows
// the session content rule (access.DocumentClause through access.PartyClause), which reads a
// listener list only from a stored answer that has not passed its expires_at and judges a listed
// session of an expired one as a session with no list. It is the refresh the api makes on resolving
// a reader (readerFor) from S3c-issues on, made where this service resolves one: a socket's
// admission and each read of which rooms a person still reads. Before that slice nothing sets it,
// and this refreshes nothing. An open reader of a deployment that holds no private project reads
// nothing for it (R9).
func (s *Service) refreshRosterForReader(ctx context.Context, who model.Actor) {
	if readsEveryDocument(who) {
		return
	}
	if refresh := s.rosterRefresh.Load(); refresh != nil {
		(*refresh)(ctx)
	}
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

// errAccessRevoked is the cause a socket's request context is cancelled with once its person may no
// longer read its room, or a re-check of that person's access cannot read it (recheckAccess,
// recheckAdmission). ygo sends the peer the close frame a revoked admission gets, on which the
// provider reconnects, and the reconnect's admission refuses the room as a room no document holds
// (readIssue) when the person no longer reads it.
var errAccessRevoked error = accessRevoked{}

// accessRevoked is that cause's type.
type accessRevoked struct{}

func (accessRevoked) Error() string {
	return "this document socket's person no longer reads its document"
}

// CloseCode is the close frame ygo sends the peer, schemaRevoked's.
func (accessRevoked) CloseCode() (int, string) { return schemaRevoked{}.CloseCode() }
