package docs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
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
// resolves the caller and reads the document's issue once each, here, and hands both to ygo's
// authorization of the same request (authorize) on the connection's state; a warm room's schema
// verdict is the one its update observer recorded (residentVerdict), so admitting a socket to a
// warm room walks no rooms and renders nothing, and a cold one renders its document once
// (admitDocument).
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
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hostname != "" {
		w.Header().Set(ProcessHeader, s.hostname)
		w = &processHeaderWriter{ResponseWriter: w, line: []byte(ProcessHeader + ": " + s.hostname + "\r\n")}
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
	if _, err := uuid.Parse(room); err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	issue, err := s.readIssue(r.Context(), s.queryFrom(r.Context()), room)
	if err != nil {
		http.Error(w, "document not found", http.StatusNotFound)
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
	// The socket's admission lives on a request context of its own: the answer to the Hocuspocus
	// token this socket sends is that admission and no other's (authorizeSchemaVersion), since two
	// sockets of one room can present one token, and cancelling that context with errSchemaRevoked
	// is how a re-check closes the socket (RunSchemaAdmission).
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	connection := &connectionState{actor: actor, issue: issue, admission: &schemaAdmission{cancel: cancel}}
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
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	defer withdraw()
	connection.admission.documentSince = documentSince
	ctx = context.WithValue(ctx, connectionContextKey{}, connection)
	// The awareness clients the socket's peer may announce are bounded before ygo reads its
	// frames (awareness_limit.go).
	s.srv.ServeHTTP(&awarenessLimitWriter{ResponseWriter: w, room: room}, r.WithContext(ctx))
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
// cookie - ever connected through it (INFRA-264).
func (s *Service) requestActor(r *http.Request) (model.Actor, error) {
	if s.identity == nil {
		return model.Actor{}, errors.New("document identity required")
	}
	login, err := s.identity.Login(r)
	if err != nil {
		return model.Actor{}, err
	}
	accesslog.Note(r.Context(), accesslog.KindUser, login)
	return model.Actor{Kind: "user", ID: login}, nil
}
