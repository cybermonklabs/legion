package docs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// admissionRendering is what a document socket's admission check made of the state it read: the
// tree it read from that state, and the rendering it made of that tree (replicaRendering: the
// markdown and its pmdoc.Layout). The two travel together from the check to the room's load, which
// takes them in place of reading and rendering the room again (onLoadDocument), so they are one
// value at every hop between (preloadedDocument, roomLoad, takePreload, offerPreload). A room's
// load renders the room itself unless it has both: a check that read no tree, and one whose render
// failed for a reason other than the schema, each leave one of them nil.
type admissionRendering struct {
	tree      *pmdoc.Node
	rendering *replicaRendering
}

// preloadedDocument is the durable state a document socket's admission check decoded, kept for the
// load ygo makes of the room next (servicePersistenceAdapter.LoadDoc), so a cold connection reads
// and decodes the history once, and what that check made of that state (admissionRendering), which
// the room's load takes in place of reading and rendering the room again (onLoadDocument) and
// hands its replica as the rendering of the state the replica starts from (keepReplica), so a cold
// connection renders the document once too. The map holds the first socket's offer of a room
// (offerPreload): a socket whose offer loses keeps no reference to its own, and an entry ends at
// the room's load whether or not that load took it - the load takes it (takePreload) or, for one
// offered while it ran, sweeps it (onLoadDocument), and an offer made once the room is loaded here
// is not stored at all - so no entry outlasts the room's load by the socket's whole lifetime.
type preloadedDocument struct {
	loaded   persistence.LoadResult
	rendered admissionRendering
}

// takePreload puts on load, room's load record, the update a socket's admission check loaded for
// room, with what that check read and rendered of it (admissionRendering, whose rendering is nil
// for an empty history), and reports whether it did, which it does while the durable head is
// still the one that check loaded: every write that changes a document's stored state raises its
// head (an append, a rebuild), and compaction keeps both the head and the state. A stale or absent
// preload is no answer, and the caller loads the room itself. Either way the entry gives up the
// state it held, and no preload outlives this load of the room: one offered while the load runs
// is swept where the load records itself (onLoadDocument), and one offered after is not stored
// (offerPreload).
//
// The head is read from the store in one statement on the pool that owns loads, under the bound
// ctx carries on the load's reads (LoadDoc), beside the document's pending-settlement row the
// room's load reads next (preloadCheckStatement). What it read of that row goes on the record
// whether or not the preload is current (roomLoad.readSettlement), with where the room's record
// stood as to its edit source just before the read (markEditSource), so the load's hook reads the
// row no second time (onLoadDocument). A load that takes a socket's preload therefore reads the
// store once: the socket's admission read the history, and the issue (admittedIssueRead). A read
// that fails is the load's failure, returned to its caller (LoadDoc): nothing was read to load
// from, and the pool that refused it is the one a read of the whole history would wait on next.
func (s *Service) takePreload(ctx context.Context, room string, load *roomLoad) (bool, error) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return false, nil
	}
	preload := value.(*preloadedDocument)
	loaded, rendered := preload.loaded, preload.rendered
	preload.loaded, preload.rendered = persistence.LoadResult{}, admissionRendering{}
	settlement := &owedSettlement{mark: s.markEditSource(room)}
	head, err := s.readPreloadCheck(ctx, room, settlement)
	if err != nil {
		return false, err
	}
	load.readSettlement(settlement)
	if head != loaded.Version {
		return false, nil
	}
	load.read(loaded.Version, loaded.Update, rendered)
	return true, nil
}

// preloadCheckStatement reads, for the room $1 names, what a load that takes a socket's preload
// reads of the store (takePreload): the head, as PgVersioned.Head reads it (headVersion), and
// whether the document's pending-settlement row stands, with the latest edit source it names, as
// readOwedSettlement reads that row.
const preloadCheckStatement = `
	select ` + headVersion + `, pending.artifact_id is not null, pending.last_actor
	from (values (1)) as one
		left join doc_settlements_pending pending on pending.artifact_id = $1`

// readPreloadCheck reads preloadCheckStatement for room on the pool that owns loads, where
// PgVersioned.Head reads the head, and puts what it read of the pending-settlement row on
// settlement.
func (s *Service) readPreloadCheck(ctx context.Context, room string, settlement *owedSettlement) (persistence.Version, error) {
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		return 0, err
	}
	var head int64
	var encoded []byte
	if err := rooms.QueryRow(ctx, preloadCheckStatement, room).Scan(&head, &settlement.owed, &encoded); err != nil {
		return 0, fmt.Errorf("read document head and pending settlement: %w", err)
	}
	if settlement.lastActor, err = decodeOwedEditSource(encoded); err != nil {
		return 0, err
	}
	return persistence.Version(head), nil
}

// documentSchemaCloseCode closes a document websocket whose room is outside the Proof schema
// (DOCUMENT_SCHEMA_CLOSE_CODE in packages/contracts), which the dashboard reads as the document's
// repair state rather than as a dropped connection to retry.
const documentSchemaCloseCode = contracts.DocumentSchemaCloseCode

// documentNewerContentCloseCode closes a document websocket whose room holds an element this
// build does not know (DOCUMENT_NEWER_CONTENT_CLOSE_CODE in packages/contracts). A newer build
// wrote it while two builds serve one database, and a browser editor would drop it and write the
// result back. In the pinned @hocuspocus/provider 2.15, onClose
// (packages/dispatch/node_modules/@hocuspocus/provider/src/HocuspocusProviderWebsocket.ts:483-545)
// stops reconnecting only on 4401 and 1009, which set shouldConnect to false, and on 4403, where
// it returns before it reconnects unless the provider is quiet. A socket closed before any message
// rejects its connection attempt, which the provider retries with its backoff (:265-288). So it
// reconnects on this code, and a later attempt can reach a build that reads the document.
const documentNewerContentCloseCode = contracts.DocumentNewerContentCloseCode

// refuseOutsideSchema completes the upgrade only to close it with documentSchemaCloseCode before
// anything of the document is sent: a browser reads a close code, never the status of a refused
// upgrade.
func refuseOutsideSchema(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket outside Proof schema; a replacement from markdown repairs it", "room", room, "error", cause)
	closeRefusedSocket(w, r, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
}

// refuseNewerContent completes the upgrade only to close it with documentNewerContentCloseCode
// before anything of the document is sent.
func refuseNewerContent(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket holding an element this build cannot read; the provider reconnects", "room", room, "error", cause)
	closeRefusedSocket(w, r, documentNewerContentCloseCode, contracts.DocumentNewerContentCloseReason)
}

// closeRefusedSocket completes the upgrade only to close it with code and reason. The zero
// upgrader keeps the same-origin rule ygo's own upgrade applies with no AllowedOrigins
// configured.
func closeRefusedSocket(w http.ResponseWriter, r *http.Request, code int, reason string) {
	var upgrader gws.Upgrader
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has answered the request
	}
	defer connection.Close()
	deadline := time.Now().Add(time.Second)
	if err := connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(code, reason), deadline); err != nil {
		return
	}
	// The client's close in reply completes the handshake; whatever it sent before that is
	// discarded unread.
	_ = connection.SetReadDeadline(deadline)
	for {
		if _, _, err := connection.NextReader(); err != nil {
			return
		}
	}
}

// socketRoom is the document a socket request names: its route's room, else its path's last
// segment.
func socketRoom(r *http.Request) string {
	if room := r.PathValue("room"); room != "" {
		return room
	}
	return path.Base(r.URL.Path)
}

// ServeHTTP serves the Hocuspocus-framed document websocket endpoint. A caller that does not
// resolve is refused 401, except a person whose lapsed confirmation the sign-in issuer could not
// renew (identity.ErrSignInUnavailable), who is still signed in and is refused 503
// SIGNIN_UNAVAILABLE as every other route refuses them (identity.WriteError), and a caller whose
// session the database did not serve - the pool busy, the database stalled, the connection lost
// under the read (store.UnservedRead) - whom nothing has found signed out and who is refused as
// the admission refuses any read it could not make (refuseAdmission). A request that presents no
// credential the identity service could resolve - no sign-in cookie, or one whose signature does
// not verify (identity.Presents) - is refused 401 before it takes anything. A socket's
// admission resolves the caller and reads the document's issue once each, here, takes the live
// server processes from the one read the sockets of a second share (admissionFleet), and hands
// all three to ygo's authorization of the same request (authorize) on the connection's state; a
// warm room's schema verdict is the one its update observer recorded (residentVerdict), so
// admitting a socket to a warm room walks no rooms and renders nothing, and a cold one renders its
// document once (admitDocument).
//
// Before it loads anything the socket takes its places under the caps, a connection's
// (reserveConnection) and its room's (reserveRoom), and a socket that cannot take either is
// refused 503 with Retry-After (admission.RetryAfter) before the upgrade, as every socket the
// server cannot serve now is. Each place is taken in the same step that checks the cap, so
// sockets that open at once never pass it, and each is a count, so the check walks no rooms. The
// socket holds both until it ends. The connection's place is the connection's count once
// authorize registers it, and removeConnection, which forgets the browser as the socket closes,
// ends it; a socket that ends unregistered - its admission or its authorization refused, its room's
// load failed or panicked - gives it back. The room's place the socket gives back however it ends,
// and a room ygo loaded keeps a place of its own until it unloads (releaseUnloadedRoom).
//
// A socket whose document's issue, document, or the live server processes its page is admitted
// against could not be read, or whose request ended while it waited for its failed room's recovery,
// is refused 503 with Retry-After before the upgrade (refuseAdmission, refuseUnservedSocket), a
// document that is not there alone being 404, and so is one whose room's load, once ygo joins the
// socket to the room, found a pool busy, ran past its reads' own bound or met a request the server
// ended, and one ygo's authorization refuses for a room a busy pool failed: ygo answers those with
// a 500 or a 401 of its own, which the socket's writer answers in its place (socketAnswerWriter).
// A cold socket's load reads the store once, in the statement that checks its preload against the
// head (takePreload), so it is refused within one acquire bound of the pool it found busy.
//
// The room's name has to be the document's id as Dispatch writes it, lower case and hyphenated,
// and a name uuid.Parse takes in any other spelling - upper case, braced, a dash-free run of hex,
// urn:uuid:<id> - is refused below rather than rewritten. That is what makes one document one
// room, and therefore one lock: a durable write takes its lock on the room's own name
// (lockDocumentRoom) while the stream it appends to is keyed by the document's uuid
// (doc_updates.artifact_id), so two rooms of one id would be two editor states writing one
// stream, neither browser seeing the other's typing and the stored markdown ending as text that
// matched neither. Rewriting the name instead would admit a browser that cannot use what it was
// given: its client routes every frame by the name it asked for, so a connection answered under
// another name syncs nothing while holding a room's place. The rule holds for this endpoint
// alone - another transport, or another handler that reaches a room name by a path of its own,
// opens a room under whatever name it passes, and no code here can stop that.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hostname != "" {
		w.Header().Set(ProcessHeader, s.hostname)
		w = &processHeaderWriter{ResponseWriter: w, line: []byte(ProcessHeader + ": " + s.hostname + "\r\n")}
	}
	room := socketRoom(r)
	// A request that can only be refused costs no place: one that presents no credential the
	// identity service could resolve - no sign-in cookie, or one whose signature does not verify,
	// or behind IAP no assertion - is told so from the request alone, with nothing read
	// (identity.Presents), and refused 401 as the caller's resolution would refuse it.
	if !identity.Presents(s.identity, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The admission's reads of the request pool - the caller's session (requestActor), the
	// document's issue and, for the socket that finds no answer of the last second to share, the
	// live server processes (admissionFleet) - hold one place of the pool's work outside admission
	// between them (admission.Outside), from before the first to right after the last, so a
	// restart's reconnect of every editor holds no more of the pool than admission leaves.
	// The place goes back before the wait for a failed room's recovery and the document's load,
	// neither of which reads the request pool; the release on the way out gives it back for a
	// socket refused before that. A socket that finds every place held waits for one, behind the
	// work that asked before it, for at most a second; one refused a place - the queue for one full,
	// or its second run out - reads nothing and is refused as one the server cannot serve now, and
	// one whose browser leaves while it waits is answered nothing (refuseAdmission).
	place, err := s.outside.Hold(r.Context(), admission.SocketAdmission)
	defer place.Release()
	if err != nil {
		refuseAdmission(w, r, "dispatch: a document socket was refused: the request pool's places outside admission are all held", room, store.RequestsPool, err)
		return
	}
	// The caller's resolution carries the place (admission.WithPlace): where the caller's sign-in
	// confirmation has lapsed, it is renewed with the sign-in issuer, and behind IAP the assertion's
	// verification may read IAP's key set again; neither reads the pool, so the resolution gives
	// the place back for that wait and holds it only for its own reads and writes
	// (identity.Membership.Confirm, identity.IAPIdentity.Login). The issue's read takes it again
	// below.
	actor, err := s.requestActor(r.WithContext(admission.WithPlace(r.Context(), place)))
	if err != nil {
		if errors.Is(err, identity.ErrSignInUnavailable) {
			identity.WriteError(w, r, err)
			return
		}
		// A read of the caller's session the database did not serve - the pool busy, the database
		// stalled, or the connection lost under it (store.UnservedRead) - says nothing of whether
		// the caller is signed in, and a 401 tells a signed-in browser it is not: it is the
		// admission's refusal, as every other read of it the database does not serve is. A caller
		// the read found not signed in is still 401.
		if store.UnservedRead(r.Context(), err) {
			refuseAdmission(w, r, "dispatch: a document socket was refused: its caller's session could not be read", room, store.RequestsPool, err)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(room)
	if err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	if id.String() != room {
		http.Error(w, "document not found: a document's address is its id as Dispatch writes it, lower case with dashes, here "+id.String(), http.StatusNotFound)
		return
	}
	// A resolution that renewed its caller's confirmation gave the place back (requestActor), and
	// the socket takes it again for this read and the next, waiting for it as it waited first and
	// refused as it would have been refused then; every other socket still holds its place, and
	// takes nothing here.
	if err := place.Retake(r.Context()); err != nil {
		refuseAdmission(w, r, "dispatch: a document socket was refused: the request pool's places outside admission are all held", room, store.RequestsPool, err)
		return
	}
	// Only a document that is not there is not found: the read finds no document row (no artifact
	// of that id, or one that is no document). Any other failure of the read - under load, the
	// request pool with no connection free - is the admission's refusal, since the document may well
	// be there.
	issue, err := s.readIssue(r.Context(), s.queryFrom(r.Context()), room)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	if err != nil {
		refuseAdmission(w, r, "dispatch: a document socket was refused: its document's issue could not be read", room, store.RequestsPool, err)
		return
	}
	// The live server processes are taken here, before ygo's authorization of the socket
	// (authorize) admits its page against them (connectionState.fleet), so a read of them that
	// fails - under load, the request pool with no connection free - is this admission's refusal:
	// a refusal of authorize's reaches the browser as ygo's 401, which says it is not signed in.
	// The answer is every socket's alike, so the sockets of a second share one read of it
	// (admissionFleet, fleetCacheTTL), made on the request's own pool (queryFrom), the request pool
	// here, by the socket that finds no fresh answer; it needs nothing the document's load makes,
	// so it is taken beside the issue's read, under the same place.
	fleet, err := s.admissionFleet(r.Context(), room)
	if err != nil {
		refuseAdmission(w, r, "dispatch: a document socket was refused: the live server processes could not be read", room, store.RequestsPool, err)
		return
	}
	// The admission's reads of the request pool are done: the place goes back now, not after the
	// recovery wait or the document's load, and not when the handler returns.
	place.Release()
	// A socket holds no connection while it waits for a failed room's recovery, so the wait ends
	// only with the request's context: the browser left, or the server ended the request.
	if err := s.awaitRoomRecovery(r.Context(), room); err != nil {
		refuseAdmission(w, r, "dispatch: a document socket was refused: its failed room did not recover while it waited", room, "", err)
		return
	}
	// A cap with no place for the socket is a refusal of the server's for now, which tells the
	// browser when to ask again as every other one does (refuseUnservedSocket).
	if !s.reserveConnection() {
		w.Header().Set("Retry-After", admission.RetryAfter)
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if !s.reserveRoom(room) {
		s.releaseConnection()
		w.Header().Set("Retry-After", admission.RetryAfter)
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	// Last of all, once the socket has let go of everything it held on the room's record: the
	// settlement its browser's departure ran (settleLastPeer) may have loaded a room of its own,
	// which is retired if nothing has taken it up (retireSettlementRoom).
	defer s.retireSettlementRoom(room)
	// The socket's admission lives on a request context of its own: the answer to the Hocuspocus
	// token this socket sends is that admission and no other's (authorizeSchemaVersion), since two
	// sockets of one room can present one token, and cancelling that context with errSchemaRevoked
	// is how a re-check closes the socket (RunSchemaAdmission).
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	connection := &connectionState{actor: actor, issue: issue, fleet: fleet, admission: &schemaAdmission{cancel: cancel}}
	defer func() {
		s.schemaAdmissions.remove(connection.admission)
		s.releaseRoom(room)
		if connection.added {
			s.removeConnection(connection.room, connection.id)
		} else {
			s.releaseConnection()
		}
	}()
	held := s.holdForAdmission(room, issue)
	defer s.releaseAdmission(room, held)
	// A load of the room that failed before this socket held its record failed no join of this
	// socket's (socketAnswerWriter.refuseFailedLoad).
	failedBefore := held.latestLoadFailure()
	if s.afterSocketIssueRead != nil {
		s.afterSocketIssueRead(room)
	}
	withdraw, documentSince, err := s.admitDocument(ctx, room)
	if errors.Is(err, ErrDocNewerContent) {
		refuseNewerContent(w, r, room, err)
		return
	}
	if errors.Is(err, ErrDocOutsideSchema) {
		refuseOutsideSchema(w, r, room, err)
		return
	}
	if err != nil {
		refuseAdmission(w, r, "dispatch: a document socket was refused: its document could not be admitted", room, loadPoolName(r.Context()), err)
		return
	}
	defer withdraw()
	connection.admission.documentSince = documentSince
	ctx = context.WithValue(ctx, connectionContextKey{}, connection)
	// The awareness clients the socket's peer may announce are bounded before ygo reads its
	// frames (awareness_limit.go), and a join whose room's load fails, or that authorize refuses
	// for a room a busy pool failed, is answered with the socket's own refusal where it has one
	// (socketAnswerWriter).
	s.srv.ServeHTTP(&socketAnswerWriter{
		awarenessLimitWriter: awarenessLimitWriter{ResponseWriter: w, room: room},
		service:              s,
		request:              r,
		socket:               ctx,
		connection:           connection,
		held:                 held,
		failedBefore:         failedBefore,
	}, r.WithContext(ctx))
}

// refuseAdmission answers a document socket whose admission failed with err: a read - of its
// caller's session, of its document's issue, of its document, or of the live server processes -
// through the pool pool names, or the wait for its failed room's recovery, which names none. The
// browser left while it ran when the request's context ended with net/http's own
// context.Canceled (accesslog.ClientLeft), which ends the read or the wait with the request's own
// cancellation (loadTree): nothing refused the socket, and nobody is left to read an answer, so
// none is written and the line carries the client gone, as a request whose client left in
// admission's queue does (accesslog.ClientGone). A context the server ended under the socket
// carries a cause of its own - the heartbeat's re-resolution of the caller failed (api's
// whileCallerResolves), which under load is a busy pool and not a browser that left - and that
// socket's browser is still waiting, so it is refused like any other (refuseUnservedSocket), with
// message, and with that cause's class.
func refuseAdmission(w http.ResponseWriter, r *http.Request, message, room, pool string, err error) {
	if accesslog.ClientLeft(r.Context()) {
		accesslog.ClientGone(r.Context())
		return
	}
	refuseUnservedSocket(w, message, room, pool, err, context.Cause(r.Context()))
}

// refuseUnservedSocket refuses a document socket the server could not serve now, before any
// upgrade: 503 with Retry-After (admission.RetryAfter), as the API's 503s carry it, and why at
// WARN. The 503 alone names neither the room nor what refused it, so a refusal that reached a
// person could be placed only from a neighbouring line: the line carries message, which says
// what refused the socket, the room, the pool the read that failed went through (loadPoolName for
// a document's; none for a wait that read no pool, or a room that failed), the class of err, and
// the class of cause, what ended the request's context, when the server ended it.
//
// The line names no person: err and cause go on it as their class (refusalClass), never as their
// text. The read of the caller's session fails with the store's own error, which quotes the login
// it read for (store.PgSessionStore.CurrentSessionGeneration, PgPeopleStore.Membership), and the
// heartbeat's cause wraps that same read's error (api's whileCallerResolves); one rule for every
// line means no line has to be judged by where its error came from. The access-log line names
// the caller (accesslog.Note), and the class and the room place the refusal. The errors
// themselves are left whole: what chose this answer read them (errors.Is, store.Unserved) before
// it was called.
func refuseUnservedSocket(w http.ResponseWriter, message, room, pool string, err, cause error) {
	attrs := []any{"room", room}
	if pool != "" {
		attrs = append(attrs, "pool", pool)
	}
	attrs = append(attrs, "error", refusalClass(err))
	if cause != nil {
		attrs = append(attrs, "cause", refusalClass(cause))
	}
	slog.Warn(message, attrs...)
	w.Header().Set("Retry-After", admission.RetryAfter)
	http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
}

// refusalClass is what a refused document socket's WARN line says of err, an error that refused
// the socket or the cause that ended its request: a word of a fixed set, which names no person
// whatever err's text names (refuseUnservedSocket). An error that wraps more than one is the
// first class below that it matches: a busy pool's acquire wraps context.DeadlineExceeded too, and
// the sign-in issuer's failure can be a timeout of its own.
func refusalClass(err error) string {
	var outside *admission.OutsideRefusal
	switch {
	case errors.Is(err, errSchemaRevoked):
		// A re-check of the live server processes revoked the socket's admission
		// (RunSchemaAdmission).
		return "schema_revoked"
	case store.IsPoolBusy(err):
		// A bound of this server's ended a call before the database answered it: no connection
		// of the pool came free within its acquire bound, or the database stopped answering on a
		// call that commits nothing.
		return "pool_busy"
	case errors.As(err, &outside):
		// The request pool's places outside admission were all held (admission.Outside): the
		// queue for one was full, or no place came free within the wait.
		return string(outside.Reason)
	case errors.Is(err, identity.ErrNoIdentity):
		// The heartbeat's re-resolution found the caller no longer signed in.
		return "no_identity"
	case errors.Is(err, identity.ErrSignInUnavailable):
		// The heartbeat's re-resolution met a lapsed confirmation the sign-in issuer could not
		// renew.
		return "signin_unavailable"
	case errors.Is(err, context.Canceled):
		// The request's context ended: the line's cause, when it has one, says what ended it.
		return "request_ended"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errLoadReadsBound):
		// A bound on the request's context, on the heartbeat's check, or on a room's load's reads
		// (errLoadReadsBound), passed.
		return "deadline_passed"
	case store.Unserved(context.Background(), err):
		// The database stopped answering on a connection under a bound of this server's
		// (store.Unserved's stall); a context's own end is one of the cases above.
		return "database_stalled"
	case store.UnservedRead(context.Background(), err):
		// The connection ended under a read (store.UnservedRead's lost connection; its stall is the
		// case above): pgconn closed it under the call, or Postgres ended the session.
		return "connection_lost"
	case errors.Is(err, ErrDocumentUnloadable):
		// The document's stored history does not decode.
		return "document_unloadable"
	case errors.Is(err, ErrServiceUnavailable):
		// The room was failed, reloading or behind its durable head, or the store's load of the
		// document failed and failed the room (loadTree).
		return "room_unavailable"
	default:
		// Any other failure: a statement the database refused, a socket to it that broke, a
		// document that did not render. Its text is not logged either, since what an error of no
		// known class carries is not known.
		return "other"
	}
}

// socketAnswerWriter is a document socket's response writer while ygo serves the socket: the
// awareness bound's writer (awarenessLimitWriter), whose Hijack holds the upgraded socket to its
// clients, and the socket's own answer to the two refusals of ygo's that a busy pool reaches. ygo
// answers a join whose room did not load 500 "room unavailable", whatever ended the load
// (sjawhar/ygo v1.55.1-sami.2, provider/websocket/server.go:2088-2096), and a socket its
// authorization refused 401 "unauthorized" (server.go:2066-2070), which authorize does only for a
// room that has failed, and offers no hook on either answer, so the answer is decided where ygo
// writes it. A WriteHeader of 500 is the load's, or the upgrader's own failure to hijack the
// connection (gorilla/websocket's returnError), which no load recorded and refuseFailedLoad
// leaves as it is; one of 401 is authorize's (refuseFailedRoom). When the socket answers in ygo's
// place, the body ygo writes next is dropped. Every other answer, and the upgrade, pass through as
// ygo wrote them.
type socketAnswerWriter struct {
	awarenessLimitWriter
	service *Service
	// request is the socket's request as ServeHTTP took it, whose context tells a browser that
	// left from a request the server ended, socket the context ygo serves the join on, and
	// connection the socket's state, on which authorize records why it refused the socket. held
	// is the room's record the socket's admission holds, and failedBefore the load failure that
	// record held as the admission took it (latestLoadFailure).
	request      *http.Request
	socket       context.Context
	connection   *connectionState
	held         *roomState
	failedBefore *roomLoadFailure
	// answered is set once the socket has answered in ygo's place.
	answered bool
}

func (w *socketAnswerWriter) WriteHeader(status int) {
	if !w.answered && w.refuse(status) {
		w.answered = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *socketAnswerWriter) Write(p []byte) (int, error) {
	if w.answered {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// refuse gives the socket's own answer in place of ygo's status when it has one, and reports
// whether it did.
func (w *socketAnswerWriter) refuse(status int) bool {
	switch status {
	case http.StatusInternalServerError:
		return w.refuseFailedLoad()
	case http.StatusUnauthorized:
		return w.refuseFailedRoom()
	}
	return false
}

// refuseFailedLoad gives the socket's own answer in place of ygo's 500 to a join whose room did
// not load, when it has one, and reports whether it did:
//
//   - the browser left while the room loaded (the request's context ended with net/http's own
//     context.Canceled, accesslog.ClientLeft): nothing is written, nobody being left to read it,
//     and the access line carries the client gone (accesslog.ClientGone), as for a browser that
//     left while its admission loaded the document (refuseAdmission);
//   - the server ended the socket's request while its join waited on the room's load - the
//     heartbeat's re-resolution of the caller failed (api's whileCallerResolves), which under
//     load is a busy pool, or a re-check revoked the socket's admission (RunSchemaAdmission):
//     refused, with the class of what ended it, as the admission refuses a request the server
//     ended under it. The load's own reads run detached from any one request's end
//     (onLoadDocument);
//   - a load of the room failed, since the admission took the room's record, on a bound of this
//     server's: a pool that could not serve it (store.Unserved: no connection came free within the
//     pool's acquire bound, or the database stopped answering under it), or its reads' own bound,
//     a cold read's, passing before the database answered them (errLoadReadsBound): refused, with
//     the class of the load's error. ygo hands every join waiting on a load that load's error, so
//     this is the socket's own load's failure or that of the load its join waited on - a
//     settlement's warm-up, a committed write's publish, another socket's - and the load recorded
//     it on the room's record (noteLoadFailed, loadFailedSince).
//
// Any other failure keeps ygo's 500, since no bound of this server's made it: a history that does
// not decode, a room being rebuilt, a load that panicked, a room that failed for one of those.
func (w *socketAnswerWriter) refuseFailedLoad() bool {
	if accesslog.ClientLeft(w.request.Context()) {
		accesslog.ClientGone(w.request.Context())
		return true
	}
	const message = "dispatch: a document socket was refused: its room could not load"
	if err := w.socket.Err(); err != nil {
		refuseUnservedSocket(w.ResponseWriter, message, w.room, store.RoomsPool, err, context.Cause(w.socket))
		return true
	}
	if failure := w.service.loadFailedSince(w.room, w.held, w.failedBefore); loadEndedByOwnBound(w.socket, failure) {
		refuseUnservedSocket(w.ResponseWriter, message, w.room, store.RoomsPool, failure, nil)
		return true
	}
	return false
}

// refuseFailedRoom gives the socket's own answer in place of ygo's 401 to a socket authorize
// refused because its room had failed (connectionState.refusal), when a pool's bound failed the
// room - a busy room-appends pool failing the room's append, a busy rooms pool failing its load
// (store.Unserved) - and reports whether it did. The browser is signed in, and the room reloads
// once its eviction has run, so it is refused 503 like any socket the server cannot serve now. A
// room that failed any other way keeps ygo's 401, as it always has.
func (w *socketAnswerWriter) refuseFailedRoom() bool {
	failure := w.connection.refusal
	if !store.Unserved(w.socket, failure) {
		return false
	}
	refuseUnservedSocket(w.ResponseWriter, "dispatch: a document socket was refused: its room failed on a busy pool", w.room, "", failure, nil)
	return true
}

// ProcessHeader names, on every answer to a document socket's request and on its upgrade above
// all, the host name of the process serving it - the pod's name in production - so a check that
// opens sockets through a load balancer can record which process each one reached (plan D10).
const ProcessHeader = "X-Dispatch-Process"

// processHeaderWriter puts its line, ProcessHeader's, on a websocket upgrade made through it. The
// upgrader (gorilla/websocket, which ygo's ServeHTTP and refuseOutsideSchema both use) takes the
// connection and writes its 101 answer on it in one write, reading no header of the writer's, and
// ygo passes it none of its own; so the connection this Hijack hands out adds the line to that
// first write, right after its status line. Every other answer carries the header the ordinary
// way (ServeHTTP sets it).
type processHeaderWriter struct {
	http.ResponseWriter
	line []byte
}

func (w *processHeaderWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &processHeaderConn{Conn: conn, line: w.line}, buffered, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *processHeaderWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// processHeaderConn is a hijacked connection whose first write, the upgrade's answer, gains line
// after its status line. line is cleared by that write, which the upgrade makes before it hands
// the connection to any other goroutine, so every later write passes straight through.
type processHeaderConn struct {
	net.Conn
	line []byte
}

func (c *processHeaderConn) Write(p []byte) (int, error) {
	line := c.line
	if line == nil {
		return c.Conn.Write(p)
	}
	c.line = nil
	status := bytes.Index(p, []byte("\r\n"))
	if status < 0 || !bytes.HasPrefix(p, []byte("HTTP/1.1 101 ")) {
		return c.Conn.Write(p)
	}
	status += len("\r\n")
	answer := make([]byte, 0, len(p)+len(line))
	answer = append(answer, p[:status]...)
	answer = append(answer, line...)
	answer = append(answer, p[status:]...)
	written, err := c.Conn.Write(answer)
	// What was written, counted in p's bytes.
	switch {
	case written <= status:
	case written < status+len(line):
		written = status
	default:
		written -= len(line)
	}
	return written, err
}

// admitDocument decides whether a socket may join room's document, and reports the document's own
// schema since (pmdoc.DocumentSince), which is what the page's admission is judged against
// (schemaFleet.admits). A browser editor normalizes a tree it cannot represent and writes the
// result back, so no connection - a first one, or a provider's reconnect - joins a room outside
// the Proof schema until it is replaced from markdown, nor one holding an element this build does
// not know, which a newer build wrote. An ask holding blocks off its content rule is inside the
// schema: the editor keeps such an ask rather than normalizing it away, and settlement marks it
// `invalid`. The server decides it for every client at once, by the same document and rendering
// `/text` answers with (readDocument), so a socket is refused exactly when that read is
// ErrDocOutsideSchema or ErrDocNewerContent.
//
// A resident room's update observer has rendered the room after every update it took, as that
// read renders it, and recorded the verdict (residentVerdict), which is taken as it stands: a
// room it found inside the schema renders nothing here, and one it found outside is refused
// without a read. Only a room with no verdict - not resident, or with a replica that lost the
// room's state - is rendered here. The tree is read either way, since the document's since is
// read off it and no rendering records it; for a resident room that read holds the room's own
// document and makes no query of the stored history (heldTree). A cold socket offers the state it
// read, and what it rendered of it, to the room's load (offerPreload), and withdraw is what the
// socket's end runs to take that offer back.
func (s *Service) admitDocument(ctx context.Context, room string) (withdraw func(), documentSince int, err error) {
	verdict := s.residentVerdict(room)
	if verdict == verdictOutsideSchema {
		return nil, 0, ErrDocOutsideSchema
	}
	tree, loaded, err := s.loadTree(ctx, room)
	rendered := admissionRendering{tree: tree}
	if err == nil && tree != nil && verdict != verdictInsideSchema {
		if s.admissionRendered != nil {
			s.admissionRendered(room)
		}
		var renderErr error
		rendered.rendering, renderErr = replicaRenderingOf(tree)
		if errors.Is(renderErr, ErrDocOutsideSchema) || errors.Is(renderErr, ErrDocNewerContent) {
			err = renderErr
		}
	}
	if err != nil {
		return nil, 0, err
	}
	documentSince = pmdoc.DocumentSince(tree)
	if loaded == nil {
		return func() {}, documentSince, nil
	}
	return s.offerPreload(room, loaded, rendered), documentSince, nil
}

// offerPreload offers loaded, the durable state a cold socket's admission read for room, and
// rendered, the tree it read from that state with what it rendered of that tree (whose rendering is
// nil when the render failed for a reason other than the schema, which the room's load then renders
// again), to the room's load (takePreload). A preload is of use only until its room is loaded on
// this process, so an offer to a room loaded here, or one whose earlier load's unload has not
// arrived yet (roomLoadedHere), is dropped on the spot, holds nothing and leaves the socket's end
// nothing to withdraw. The offer is decided under the room's state record (lockState), where the
// load that overtakes an offer stored before it counted itself sweeps that offer (onLoadDocument),
// so an offer and a load that race leave no preload behind. Of the offers to a room not yet loaded
// the first stands (LoadOrStore): every offer is of a state the load checks against the head before
// it takes it, and a socket whose offer lost keeps no reference to it, so its whole history is
// released at once rather than when the socket ends, which can be hours later. It returns what the
// socket's end runs: withdraw its own offer if no load took or swept it. A withdraw whose preload
// has gone finds the entry no longer its own (CompareAndDelete) and withdraws nothing.
func (s *Service) offerPreload(room string, loaded *persistence.LoadResult, rendered admissionRendering) (withdraw func()) {
	preload := &preloadedDocument{loaded: *loaded, rendered: rendered}
	state := s.lockState(room)
	resident := s.roomLoadedHere(room)
	var lost bool
	if !resident {
		_, lost = s.preloads.LoadOrStore(room, preload)
	}
	s.unlockState(room, state)
	if s.afterSocketPreload != nil {
		s.afterSocketPreload(room, loaded)
	}
	if resident || lost {
		return func() {}
	}
	return func() { s.preloads.CompareAndDelete(room, preload) }
}

// authorize is ygo's authorization of a document socket ServeHTTP admitted, read-only unless its
// page may write (schemaFleet.admits). It registers the browser as the caller ServeHTTP resolved,
// and takes the issue read and the read of the live server processes ServeHTTP made, so it
// resolves and reads nothing again. The connection is counted already: ServeHTTP took its place
// under the connection cap (reserveConnection) before it loaded anything, so registering it
// counts nothing more, and no cap refuses it here, where a refusal would reach the browser as
// ygo's 401, which says it is not signed in. The issue is closed for the socket when that read
// says so or the room's state records it closed: ServeHTTP's hold on the state
// (holdForAdmission) is where a close made while the socket loaded its document is recorded
// (setRoomIssueOpen), so such a socket is admitted read-only, as one opened after the close is. A
// closed issue, or a page the live fleet does not admit, admits the socket read-only; a request no
// admission read the issue for is refused, as is a socket to a failed room, whose failure goes on
// the connection's state (connectionState.refusal) for the socket to answer its own refusal in
// place of ygo's 401 when a busy pool failed the room (socketAnswerWriter.refuseFailedRoom).
//
// It decides the admission ServeHTTP put on the socket's context and records it with the service,
// so a re-check can close the socket again (schemaAdmissions); the answer to the socket's token
// is that same admission (authorizeSchemaVersion).
func (s *Service) authorize(r *http.Request) (websocket.ConnectionConfig, bool) {
	connection, _ := r.Context().Value(connectionContextKey{}).(*connectionState)
	if connection == nil {
		return websocket.ConnectionConfig{}, false
	}
	room := socketRoom(r)
	state := s.lockState(room)
	if state.failed != nil {
		connection.refusal = state.failed
		s.unlockState(room, state)
		return websocket.ConnectionConfig{}, false
	}
	state.recordIssueLocked(connection.issue)
	open := connection.issue.open && !state.closed
	connection.room = room
	connection.id = s.nextConnection.Add(1)
	connection.added = true
	s.registerConnectionLocked(state, connection.id, connection.actor)
	s.unlockState(room, state)
	query := r.URL.Query()
	admission := connection.admission
	admission.key = schemaAdmissionKey{room: room, token: query.Get("schema_version")}
	admission.page = schemaPageOf(query)
	admission.writable = open && connection.fleet.admits(admission.page, admission.documentSince)
	s.schemaAdmissions.admit(admission)
	// A reader joining a room already resident here asks the other processes for their readers'
	// awareness, so their cursors reach it at once; a room this join has yet to load asks once its
	// load finishes (awarenessRelay.RoomActivated), and the connection registered above is what
	// lets the answers in (holdsRoomForReaders).
	if s.srv.GetDoc(room) != nil {
		s.awarenessRelay.query(room)
	}
	return websocket.ConnectionConfig{ReadOnly: !admission.writable}, true
}

// authorizeSchemaVersion is ygo's OnTokenAuthContext: the answer to the Hocuspocus token a
// socket's provider sends once it opens, which the provider takes as the socket's scope and ygo as
// its read-only state (sjawhar/ygo provider/websocket/peer.go, handleAuth). ctx is the request
// context ServeHTTP served that socket on, so the answer is the admission that socket carries and
// no other's: the page's token is its schema_version alone - a server that predates the fleet
// columns compares the whole token with its own version - so two sockets of one room can send one
// token, and neither the key nor the order they arrive in tells them apart. It is read-write only
// when this socket was admitted writable, no re-check has revoked it, and the room and token are
// the ones it was admitted with (schemaAdmissions.writable); read-only in every other case. It
// never refuses: a refusal closes the socket with 4401, after which the provider never reconnects.
func (s *Service) authorizeSchemaVersion(ctx context.Context, room, token string) (websocket.ConnectionConfig, error) {
	var admission *schemaAdmission
	if connection, ok := ctx.Value(connectionContextKey{}).(*connectionState); ok {
		admission = connection.admission
	}
	return websocket.ConnectionConfig{ReadOnly: !s.schemaAdmissions.writable(admission, room, token)}, nil
}

// requestActor is the person a document connection is: the identity service's answer, and nothing
// else. The socket once took the shared agent token too, with the session named in a header the
// caller wrote; that was a second, weaker door into Dispatch, and only the browser - a same-origin
// cookie - ever connected through it (INFRA-264). The cookie proved the login, so the person is
// Proven, as the API's cookie caller is.
func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if s.identity == nil {
		return model.Actor{}, errors.New("document identity required")
	}
	login, err := s.identity.Login(r)
	if err != nil {
		return model.Actor{}, err
	}
	accesslog.Note(r.Context(), accesslog.KindUser, login)
	return model.Actor{Kind: "user", ID: login, Proven: true}, nil
}
