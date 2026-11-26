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
	"slices"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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

// takePreload is the update a socket's admission check loaded for room, with what that check read
// and rendered of it (admissionRendering, whose rendering is nil for an empty history), while the
// durable head is still the one it loaded: every write that changes a document's stored state
// raises its head (an append, a rebuild), and compaction keeps both the head and the state. The
// head is read from the store, one query. A stale or absent preload is no answer, and the caller
// loads the room itself. Either way the entry gives up the state it held, and no preload outlives
// this load of the room: one offered while the load runs is swept where the load records itself
// (onLoadDocument), and one offered after is not stored (offerPreload).
func (s *Service) takePreload(room string) (persistence.LoadResult, admissionRendering, bool) {
	value, ok := s.preloads.LoadAndDelete(room)
	if !ok {
		return persistence.LoadResult{}, admissionRendering{}, false
	}
	preload := value.(*preloadedDocument)
	loaded, rendered := preload.loaded, preload.rendered
	preload.loaded, preload.rendered = persistence.LoadResult{}, admissionRendering{}
	head, err := s.persistence.Head(context.Background(), room)
	if err != nil || head != loaded.Version {
		return persistence.LoadResult{}, admissionRendering{}, false
	}
	return loaded, rendered, true
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

// schemaSwitchClose is what a document socket's context is cancelled with when the schema this
// process serves moves under it (CloseDocumentSockets). ygo's ServeHTTP reads CloseCode off the
// cancel cause and sends that close frame before it closes the socket, so the browser reads the
// code it already reads for a room outside the schema: read the document's text again and
// connect again, where admission decides afresh.
type schemaSwitchClose struct{ reason string }

func (c schemaSwitchClose) Error() string { return c.reason }

func (c schemaSwitchClose) CloseCode() (int, string) {
	return documentSchemaCloseCode, contracts.DocumentSchemaCloseReason
}

// holdDocumentSocket records a live document socket, so the schema switch can close it, and
// returns what the socket's end runs to forget it. The key is the record's own address, so two
// sockets of one room are two records.
func (s *Service) holdDocumentSocket(close context.CancelCauseFunc) (release func()) {
	held := &close
	s.documentSockets.Store(held, close)
	return func() { s.documentSockets.Delete(held) }
}

// refuseSwitchedAdmission closes a socket whose admission a schema switch overtook: the switch
// came after its admission read the document against the installed schema and before the socket
// was held where the switch closes it (ServeHTTP), so the switch's own close missed it. It is the
// close the switch gives the sockets it holds, so the browser reads it the one way it reads that
// code: it admits the document again, against the schema installed now.
func refuseSwitchedAdmission(w http.ResponseWriter, r *http.Request, room string) {
	slog.Info("dispatch: close a document socket whose admission a schema switch overtook; the provider reconnects", "room", room)
	closeRefusedSocket(w, r, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
}

// CloseDocumentSockets closes every document socket this process holds, with the close code the
// browser reads as "the schema moved" (documentSchemaCloseCode), and answers how many it closed.
// The server runs it after an install that moved the schema it serves: a socket was admitted on
// the schema version its browser presented, against the schema this process served as it
// connected, and the fleet's re-check (RunSchemaAdmission) closes only a writable socket the live
// processes no longer admit, a live window later, so a socket kept open across a switch would be
// writing against types this process no longer holds. The browser reads the close, reads the
// document's text again and connects again presenting the schema it was built with, and that
// connection's admission (schemaFleet.admits) decides against the schema this process now serves.
//
// It counts the switch (schemaSwitches) before it closes anything, so a socket whose admission
// ran before the switch and which is held only after this has passed it sees the count move and
// is closed by its own ServeHTTP instead (refuseSwitchedAdmission). It runs on the installer's
// own goroutine and closes each socket by cancelling the socket's context, the one a re-check
// cancels too, which is the one way a close reaches a peer with a code of its own; the socket's
// own goroutine tears it down and forgets it.
func (s *Service) CloseDocumentSockets() int {
	s.schemaSwitches.Add(1)
	reason := fmt.Sprintf("the installed block schema is version %d", pmdoc.SchemaVersion())
	closed := 0
	s.documentSockets.Range(func(_, value any) bool {
		// holdDocumentSocket is the one writer of this map and stores a
		// context.CancelCauseFunc every time, so the value is one.
		value.(context.CancelCauseFunc)(schemaSwitchClose{reason: reason})
		closed++
		return true
	})
	slog.Info("dispatch: closed the document sockets of a schema this process no longer serves",
		"sockets", closed, "reason", reason)
	return closed
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
// SIGNIN_UNAVAILABLE as every other route refuses them (identity.WriteError). A socket's admission
// resolves the caller, what that person sees (access.Resolve) and the document's issue once each,
// here, and hands the caller and the issue read to ygo's authorization of the same request
// (authorize) on the connection's state; a room the person may not read is refused exactly as a
// room no document holds is (readIssue). A warm room's schema verdict is the one its update
// observer recorded (residentVerdict), so admitting a socket to a warm room walks no rooms and
// renders nothing, and a cold one renders its document once (admitDocument).
//
// Before it loads anything the socket takes its places under the caps, a connection's
// (reserveConnection) and its room's (reserveRoom), and a socket that cannot take either is
// refused 503 before the upgrade. Each place is taken in the same step that checks the cap, so
// sockets that open at once never pass it, and each is a count, so the check walks no rooms. The
// socket holds both until it ends. The connection's place is the connection's count once
// authorize registers it, and removeConnection, which forgets the browser as the socket closes,
// ends it; a socket that ends unregistered - its admission or its authorization refused, its room's
// load failed or panicked - gives it back. The room's place the socket gives back however it ends,
// and a room ygo loaded keeps a place of its own until it unloads (releaseUnloadedRoom).
//
// A schema switch closes every socket this process holds (CloseDocumentSockets), and one that
// comes after a socket's admission read its document and before the socket is held is caught as
// the socket is held: the admission reads the count of switches first and again once the socket
// is held, and a count that moved closes it the same way (refuseSwitchedAdmission).
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
	var line []byte
	if s.hostname != "" {
		w.Header().Set(ProcessHeader, s.hostname)
		line = []byte(ProcessHeader + ": " + s.hostname + "\r\n")
	}
	for _, value := range w.Header().Values(access.PrivateProjectsHeader) {
		line = append(line, access.PrivateProjectsHeader+": "+value+"\r\n"...)
	}
	if line != nil {
		w = &upgradeHeadersWriter{ResponseWriter: w, line: line}
	}
	room := socketRoom(r)
	actor, err := s.requestActor(r)
	if err != nil {
		if errors.Is(err, identity.ErrSignInUnavailable) {
			identity.WriteError(w, err)
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
	// The count of access re-checks is read before the person's read of the room, so a re-check
	// that begins before this socket is recorded has the person read again once it is
	// (schemaAdmissions.admit, recheckAdmission).
	// The person is a cookie's (requestActor), so what they see is resolved as their browser's;
	// the actor the room records stays without the credential (model.Actor.Recorded).
	accessChecks := s.schemaAdmissions.accessCheckCount()
	viewer, err := access.Resolve(r.Context(), s.queryFrom(r.Context()), model.BrowserActor(actor.ID))
	if err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	reader := actor
	reader.Viewer = &viewer
	// The room's read follows the session content rule for a session's file, which reads the
	// listener's lists only while their stored answer is fresh (refreshRosterForReader).
	s.refreshRosterForReader(r.Context(), reader)
	issue, err := s.readIssue(r.Context(), s.queryFrom(r.Context()), room, reader)
	if err != nil {
		refuseHiddenRoom(w)
		return
	}
	if err := s.awaitRoomRecovery(r.Context(), room); err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if !s.reserveConnection() {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if !s.reserveRoom(room) {
		s.releaseConnection()
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
	connection := &connectionState{actor: actor, issue: issue, admission: &schemaAdmission{cancel: cancel, person: actor.ID, accessChecks: accessChecks}}
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
	if s.afterSocketIssueRead != nil {
		s.afterSocketIssueRead(room)
	}
	switches := s.schemaSwitches.Load()
	withdraw, documentSince, err := s.admitDocument(ctx, reader, room)
	if errors.Is(err, pgx.ErrNoRows) {
		// The person may not read the document as the admission's own read of it found
		// (readStoredHead): it answers as readIssue answers a room it may not read.
		refuseHiddenRoom(w)
		return
	}
	if errors.Is(err, ErrDocNewerContent) {
		refuseNewerContent(w, r, room, err)
		return
	}
	if errors.Is(err, ErrDocOutsideSchema) {
		refuseOutsideSchema(w, r, room, err)
		return
	}
	if err != nil {
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	defer withdraw()
	connection.admission.documentSince = documentSince
	ctx = context.WithValue(ctx, connectionContextKey{}, connection)
	if s.afterSocketAdmitted != nil {
		s.afterSocketAdmitted(room)
	}
	// The schema switch ends every socket this process holds on a schema it no longer serves
	// through the same context a re-check closes it by, with a cause of its own
	// (CloseDocumentSockets). ygo closes the peer when this context ends, with the close code
	// the cause names.
	defer s.holdDocumentSocket(cancel)()
	// A switch counted since the admission read the document was decided against a schema this
	// process may no longer serve, and its close ran before this socket was held, or ran while
	// it was being held: either way this socket is closed here, before ygo sends any of it.
	if s.schemaSwitches.Load() != switches {
		refuseSwitchedAdmission(w, r, room)
		return
	}
	// The awareness clients the socket's peer may announce are bounded before ygo reads its
	// frames (awareness_limit.go), a socket whose person loses the room before ygo has upgraded
	// it is refused as a hidden room is, and once upgraded each data frame of the room asks first
	// whether its person still reads the room, which reads the database once an access change
	// has been heard since the socket last read it (revocableSocketWriter, recheckedConn,
	// socketAsk).
	stillReads := s.socketAsk(ctx, connection.admission, viewer)
	s.srv.ServeHTTP(&revocableSocketWriter{ResponseWriter: &awarenessLimitWriter{ResponseWriter: w, room: room}, ctx: ctx, stillReads: stillReads}, r.WithContext(ctx))
}

// refuseHiddenRoom answers a document socket whose person may not read its room - a document of a
// project they do not see, or of a conversation they are no party to - exactly as a room no
// document holds is answered, so the answer tells them nothing of the room (readIssue).
func refuseHiddenRoom(w http.ResponseWriter) {
	http.Error(w, "document not found", http.StatusNotFound)
}

// revocableSocketWriter is the response writer ygo serves a document socket on, up to its upgrade.
// Once an access re-check has ended the socket's request context with errAccessRevoked - its person
// lost the room while the socket was being admitted - whatever ygo would answer is replaced with a
// hidden room's refusal (refuseHiddenRoom), which is what a person who cannot read the room gets at
// admission, and ygo's upgrade is refused. ygo's own answer would be its 401, from an authorization
// that refused the socket (authorize), or its 500 "room unavailable", from a join whose context had
// ended; an upgrade would join the person to a room they no longer read, and send them its
// document. A socket revoked after its upgrade is closed with the revocation's close code, as every
// open socket is (revokeUnless). ygo writes through it from the socket's own request alone, and
// never once it has hijacked the connection, so it takes no lock.
type revocableSocketWriter struct {
	http.ResponseWriter
	ctx context.Context
	// stillReads asks, before each data frame on the upgraded connection, whether the socket's
	// person still reads its room (recheckedConn, Service.socketAsk); nil asks nothing.
	stillReads func() bool
	refused    bool
}

// hidden reports whether the socket's admission was revoked for access, and the first time it
// does, answers the request as a hidden room is answered.
func (w *revocableSocketWriter) hidden() bool {
	if !w.refused && errors.Is(context.Cause(w.ctx), errAccessRevoked) {
		w.refused = true
		refuseHiddenRoom(w.ResponseWriter)
	}
	return w.refused
}

func (w *revocableSocketWriter) WriteHeader(code int) {
	if !w.hidden() {
		rw := w.ResponseWriter
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.WriteHeader(code)
	}
}

func (w *revocableSocketWriter) Write(body []byte) (int, error) {
	if w.hidden() {
		return len(body), nil
	}
	rw := w.ResponseWriter
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	return rw.Write(body)
}

// Hijack takes the connection for ygo's upgrade, unless the socket's person has lost the room, and
// hands ygo a connection that asks before each data frame whether they still read it
// (recheckedConn).
func (w *revocableSocketWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.hidden() {
		return nil, nil, errAccessRevoked
	}
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil || w.stillReads == nil {
		return conn, rw, err
	}
	return &recheckedConn{Conn: conn, stillReads: w.stillReads}, rw, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *revocableSocketWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ProcessHeader names, on every answer to a document socket's request and on its upgrade above
// all, the host name of the process serving it - the pod's name in production - so a check that
// opens sockets through a load balancer can record which process each one reached (plan D10).
const ProcessHeader = "X-Dispatch-Process"

// upgradeHeadersWriter puts its line - ProcessHeader's, and the Dispatch-Private-Projects value the
// process's middleware set (access.PrivateProjectsHeader) - on a websocket upgrade made through it.
// The upgrader (gorilla/websocket, which ygo's ServeHTTP and refuseOutsideSchema both use) takes
// the connection and writes its 101 answer on it in one write, reading no header of the writer's,
// and ygo passes it none of its own; so the connection this Hijack hands out adds the line to that
// first write, right after its status line. Every other answer carries the headers the ordinary
// way (ServeHTTP and the middleware set them).
type upgradeHeadersWriter struct {
	http.ResponseWriter
	line []byte
}

func (w *upgradeHeadersWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &upgradeHeadersConn{Conn: conn, line: w.line}, buffered, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *upgradeHeadersWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// upgradeHeadersConn is a hijacked connection whose first write, the upgrade's answer, gains line
// after its status line. line is cleared by that write, which the upgrade makes before it hands
// the connection to any other goroutine, so every later write passes straight through.
type upgradeHeadersConn struct {
	net.Conn
	line []byte
}

func (c *upgradeHeadersConn) Write(p []byte) (int, error) {
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
	answer := slices.Concat(p[:status], line, p[status:])
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
func (s *Service) admitDocument(ctx context.Context, reader model.Actor, room string) (withdraw func(), documentSince int, err error) {
	verdict := s.residentVerdict(room)
	if verdict == verdictOutsideSchema {
		return nil, 0, ErrDocOutsideSchema
	}
	tree, loaded, err := s.loadTree(ctx, reader, room)
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
// and takes the issue read ServeHTTP made, so it resolves and reads neither again. The connection
// is counted already: ServeHTTP took its place under the connection cap (reserveConnection)
// before it loaded anything, so registering it counts nothing more, and no cap refuses it here,
// where a refusal would reach the browser as ygo's 401, which says it is not signed in. The issue
// is closed for the socket when that read says so or the room's state records it closed:
// ServeHTTP's hold on the state (holdForAdmission) is where a close made while the socket loaded
// its document is recorded (setRoomIssueOpen), so such a socket is admitted read-only, as one
// opened after the close is. A closed issue, or a page the live fleet does not admit, admits the
// socket read-only; a request no admission read the issue for is refused, as is a socket to a
// failed room.
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
	fleet, err := readSchemaFleet(r.Context(), s.queryFrom(r.Context()))
	if err != nil {
		slog.Warn("dispatch: refuse a document socket: read the live server processes", "room", room, "error", err)
		return websocket.ConnectionConfig{}, false
	}
	state := s.lockState(room)
	if state.failed != nil {
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
	admission.writable = open && fleet.admits(admission.page, admission.documentSince)
	if s.schemaAdmissions.admit(admission) {
		s.recheckAdmission(r.Context(), admission)
	}
	// A socket a re-check has revoked by now - its person lost the room while it was admitted - is
	// refused before ygo joins or loads the room for it, and answered as a hidden room is
	// (revocableSocketWriter).
	if errors.Is(context.Cause(r.Context()), errAccessRevoked) {
		return websocket.ConnectionConfig{}, false
	}
	// A reader joining a room already resident here asks the other processes for their readers'
	// awareness, so their cursors reach it at once; a room this join has yet to load asks once its
	// load finishes (awarenessRelay.RoomActivated), and the connection registered above is what
	// lets the answers in (holdsRoomForReaders).
	if s.srv.GetDoc(room) != nil {
		s.awarenessRelay.query(room)
	}
	if s.afterSocketRecorded != nil {
		s.afterSocketRecorded(room)
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
