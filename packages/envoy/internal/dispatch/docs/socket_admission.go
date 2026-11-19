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
	"net/url"
	"path"
	"strings"
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
	closeAtUpgrade(w, r, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
}

// refuseNewerContent completes the upgrade only to close it with documentNewerContentCloseCode
// before anything of the document is sent.
func refuseNewerContent(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket holding an element this build cannot read; the provider reconnects", "room", room, "error", cause)
	closeAtUpgrade(w, r, documentNewerContentCloseCode, contracts.DocumentNewerContentCloseReason)
}

// refuseOverRoomBudget closes a socket the room budget has no room for (errRoomBudgetFull), with
// the transient code the editor retries, before any room is made. The line names the document and
// the budget's reason, which carries the weight the admission was refused against
// (roomBudget.refusal), so a drive can read every refusal the server made off its log
// (cmd/dispatch's budgetRefusalLine, roomRefusalLine).
func refuseOverRoomBudget(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket; the open documents fill their memory budget", "room", room, "error", cause)
	closeAtUpgrade(w, r, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
}

// refuseMintsLive closes a socket whose person holds every unlanded client id the document allows
// them, each held by an open socket (errMintsLive), with the transient code the editor retries,
// before any room is made: by its retry a page that has gone has let its id lapse, and no mint
// gives back an id an open page writes under.
func refuseMintsLive(w http.ResponseWriter, r *http.Request, room string, cause error) {
	slog.Warn("dispatch: refuse a document socket; its person's unlanded client ids in the document are all held by open sockets",
		"room", room, "most", maxUnlandedMintsADocument, "error", cause)
	closeAtUpgrade(w, r, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
}

// refusedAtUpgrade answers r as ygo's upgrade answers a request it refuses, and reports whether it
// did, without taking the connection. It asks gorilla's Upgrader itself, called as ygo's upgrade
// calls it - no response header, and the same-origin rule ygo keeps with no AllowedOrigins
// configured (sameOrigin) - so it refuses what that upgrade refuses, with the status that upgrade
// gives (Upgrader.Upgrade, gorilla/websocket server.go): 400 for no `Connection: upgrade`, 426 for
// no `Upgrade: websocket`, 405 for a method other than GET, 400 for a version other than 13, 403
// for another origin, and 400 for a Sec-WebSocket-Key that is not 16 bytes in base64. The
// upgrader checks the whole request before it takes the connection, and is handed a writer it
// cannot take (unhijackable), so a request that passes every check meets the one refusal after
// them, the 500 of a writer that is no http.Hijacker, where nothing is written and this reports
// false. A refusal is written as gorilla writes its own (returnError): its status, with
// Sec-WebSocket-Version 13 beside it. The admission asks this before it gives the page a client
// id, so a request that never becomes a socket stores nothing. It cannot see the connection: a
// client that sends bytes before the 101 arrives is dropped by the upgrade with no answer, after
// its id was given out.
func refusedAtUpgrade(w http.ResponseWriter, r *http.Request) bool {
	refused := false
	upgrader := gws.Upgrader{
		CheckOrigin: sameOrigin,
		Error: func(w http.ResponseWriter, _ *http.Request, status int, _ error) {
			if status == http.StatusInternalServerError {
				return // past every check of the request, at the writer it cannot take
			}
			refused = true
			w.Header().Set("Sec-Websocket-Version", "13")
			http.Error(w, http.StatusText(status), status)
		},
	}
	_, _ = upgrader.Upgrade(unhijackable{w}, r, nil)
	return refused
}

// unhijackable is a ResponseWriter that is no http.Hijacker, whatever the one it wraps is.
type unhijackable struct{ http.ResponseWriter }

// sameOrigin is the origin rule ygo's upgrade keeps with no AllowedOrigins configured
// (Server.checkOrigin): a request with no Origin passes, and one with an Origin passes where its
// host is the request's.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

// closeAtUpgrade completes the upgrade only to close the socket with code and reason. The zero
// upgrader keeps the same-origin rule ygo's own upgrade applies with no AllowedOrigins configured.
func closeAtUpgrade(w http.ResponseWriter, r *http.Request, code int, reason string) {
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
//
// The room budget admits the browser after that read, not before it: it joins a room the budget
// already charges, and is otherwise weighed carrying the weight of the room it is about to load,
// which the read has just measured exactly (roomBudget.refusal). It is refused while the open
// rooms and the other admissions' reservations leave no room for it and the sweeps it starts can
// close nothing. maxLiveRooms stays the count backstop above.
//
// The budget charges a room only when its load ends, so an admission weighed on the charges
// alone would read the same headroom as every other admission in flight and they would all
// pass: measured, twenty-four browsers arriving together were all admitted, their rooms
// charged 5,976,639,564 bytes against a 2,147,483,648-byte budget, and the pod's kernel
// charged 7,225 MiB of its 4,096 (INFRA-179). What closes that is the reservation each
// admission holds from its weighing until its room's load charges it (roomBudget.load), so each
// is weighed against every other's room as well as against the rooms already open. A refused
// socket is closed at its upgrade with 4503 DOC_BUSY, before any room is made
// (refuseOverRoomBudget), and the read it made is offered to no load.
//
// Last of all, once nothing above can refuse the socket, the admission decides whether the
// socket's page may write (decideWritable) and then its client id (admitClients), so a read-only
// admission stores no row. A request ygo's upgrade would refuse - no websocket upgrade, a method
// other than GET, a websocket version or key it does not take, or one from another origin - is
// answered as that upgrade answers it before either (refusedAtUpgrade), and stores nothing; and a
// page whose person holds every unlanded id the document allows them, each held by an open
// socket, is closed at its upgrade with 4503 DOC_BUSY (refuseMintsLive).
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
	if wait, holding := s.admissionHolds.LoadAndDelete(room); holding {
		select {
		case <-time.After(wait.(time.Duration)):
		case <-r.Context().Done():
			return
		}
	}
	// What the admission reads is what the budget weighs: the stored state it decodes is the
	// state the room is about to load, so its length is the weight of the room this admission
	// adds. That read is a whole-document decode, so it takes of the pod's load concurrency for
	// the read alone and gives it back before the weighing: no admission holds any of the pool
	// while it waits on the budget, which would couple it to every other holder. A socket to a
	// room this process already holds reads no stored state and takes nothing.
	//
	// It takes a whole slot, where every other taker takes what its own copy costs (copyWeight):
	// the admission has not read the state it is about to decode, and nothing here could learn
	// its size without reading the store a second time to measure what it is about to read. The
	// whole slot is also what keeps the cold loads of a reconnect storm one at a time at the
	// calibration, where the pool is one slot, so the room budget sees each room's weight before
	// the next load begins (MemoryBounds.LoadSlots). The decode the read makes inside it covers
	// this grant and takes nothing more (loadDecodedHistory).
	readSlot := newLoadSlot()
	if s.srv.GetDoc(room) == nil {
		if err := readSlot.take(r.Context(), loadSlotBytes); err != nil {
			slog.Warn("dispatch: a document socket's admission gave up waiting for a load slot; answered 503",
				"room", room, "error", err)
			http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	loaded, structs, rendered, documentSince, err := s.admitDocument(readSlot.carriedBy(ctx), room)
	readSlot.release()
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
	admitting := time.Now()
	var reservation *roomReservation
	if !s.budget.join(room) {
		// The weight is the room this admission is about to load, which the budget does not
		// charge until that load ends: the stored state the read above decoded and the structs
		// that decode counted, at roomWeight. A read that loaded nothing adds no room
		// (heldTree), and a room the budget already charges is joined above and weighed at
		// nothing. The reservation is held until that room's load charges it or this handler
		// returns, whichever comes first, so every other admission in flight is weighed against
		// this room too.
		weight := int64(0)
		if loaded != nil {
			weight = roomWeight(int64(len(loaded.Update)), structs)
		}
		reservation, err = s.budget.refusal(r.Context(), time.Now().Add(s.admissionWait), room, weight)
	}
	defer s.budget.release(reservation)
	if errors.Is(err, errRoomBudgetFull) {
		refuseOverRoomBudget(w, r, room, err)
		return
	}
	if err != nil {
		// Anything but the budget's refusal is answered 503, which the editor's backoff retries;
		// the line says why, and whether the browser had already gone.
		slog.Warn("dispatch: a document socket's admission failed; answered 503", "room", room, "error", err,
			"waited", time.Since(admitting), "request_ended", r.Context().Err() != nil)
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	// The page's client id is decided last, once nothing above can refuse the socket, so an id is
	// given out only to a socket that is served (admitClients, client_ids_admission.go): a request
	// ygo's upgrade would refuse is answered as it would answer it first, and the page's write is
	// decided before its id, so a read-only page is given one stored nowhere.
	if refusedAtUpgrade(w, r) {
		return
	}
	if err := s.decideWritable(r.Context(), room, connection, r.URL.Query(), documentSince); err != nil {
		slog.Warn("dispatch: a document socket's admission could not decide whether its page may write; answered 503",
			"room", room, "error", err)
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	clients, err := s.admitClients(ctx, room, actor, r, connection.admission.writable)
	if errors.Is(err, errMintsLive) {
		refuseMintsLive(w, r, room, err)
		return
	}
	if err != nil {
		slog.Warn("dispatch: a document socket's admission could not give its page a client id; answered 503",
			"room", room, "error", err)
		http.Error(w, ErrServiceUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	connection.clients = clients
	// An open socket presents its page's id again while no edit has landed under it, so no mint of
	// its person's gives that id back while the socket is open (keepPresenting).
	defer s.keepPresenting(room, actor, clients)()
	// The state the admission read is offered to the room's load only now, once the budget has
	// made room for that load: a socket refused above makes no room and leaves nothing behind
	// for one (offerPreload, takePreload).
	if loaded != nil {
		defer s.offerPreload(room, loaded, rendered)()
	}
	ctx = context.WithValue(ctx, connectionContextKey{}, connection)
	// A page that takes the client id Dispatch gives it is sent that id as the socket's first
	// frame (clientIDWriter).
	if !clients.legacy {
		w = &clientIDWriter{ResponseWriter: w, frame: clientIDFrame(room, clients.own)}
	}
	// The awareness clients the socket's peer may announce are bounded before ygo reads its
	// frames, to the ids the socket may announce under, and a read-only socket's awareness, which
	// ygo discards, is not read at all (awareness_limit.go). Every other frame reaches ygo unread, a
	// sync step 2 among them, which the peer update hook judges as it judges an update
	// (judgeClients).
	s.srv.ServeHTTP(&awarenessLimitWriter{ResponseWriter: w, room: room, clients: clients, connection: connection}, r.WithContext(ctx))
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
// document and makes no query of the stored history (heldTree).
//
// It answers the durable state a cold socket read, the structs that state decoded to, and what
// it rendered of it, rather than offering them to the room's load itself: that state and those
// structs are also the weight of the room this socket is about to make, which the budget weighs
// the socket against before anything is offered or any room is made (ServeHTTP, roomWeight,
// offerPreload). A socket to a room this server already holds reads no stored state and answers
// none.
func (s *Service) admitDocument(ctx context.Context, room string) (loaded *persistence.LoadResult, structs int64, rendered admissionRendering, documentSince int, err error) {
	verdict := s.residentVerdict(room)
	if verdict == verdictOutsideSchema {
		return nil, 0, admissionRendering{}, 0, ErrDocOutsideSchema
	}
	tree, loaded, structs, err := s.loadTree(ctx, room)
	rendered = admissionRendering{tree: tree}
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
		return nil, 0, admissionRendering{}, 0, err
	}
	return loaded, structs, rendered, pmdoc.DocumentSince(tree), nil
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

// decideWritable decides, before the page's client id, whether connection's socket to room may
// write: its issue is open as the admission read it and as the room's state records it while the
// admission holds that state, a close recorded since included (admittedIssueRead), and the live
// fleet admits the page query presents for a document whose elements need documentSince
// (schemaFleet.admits). It records the page and documentSince on the admission, which a re-check
// reads again (revokeUnless). authorize, which ygo calls next, only ever turns the admission
// read-only - for an issue the room's state records closed by then - so a socket decided
// read-only here, whose page admitClients gives an id stored nowhere, writes under none.
func (s *Service) decideWritable(ctx context.Context, room string, connection *connectionState, query url.Values, documentSince int) error {
	fleet, err := readSchemaFleet(ctx, s.queryFrom(ctx))
	if err != nil {
		return fmt.Errorf("read the live server processes: %w", err)
	}
	open := connection.issue.open
	if recorded, held := s.admittedIssueRead(room); held && !recorded.open {
		open = false
	}
	admission := connection.admission
	admission.page = schemaPageOf(query)
	admission.documentSince = documentSince
	admission.writable = open && fleet.admits(admission.page, documentSince)
	return nil
}

// authorize is ygo's authorization of a document socket ServeHTTP admitted, read-only unless its
// admission decided its page may write (decideWritable). It registers the browser as the caller
// ServeHTTP resolved, and takes the issue read ServeHTTP made, so it resolves and reads neither
// again. The connection is counted already: ServeHTTP took its place under the connection cap
// (reserveConnection) before it loaded anything, so registering it counts nothing more, and no cap
// refuses it here, where a refusal would reach the browser as ygo's 401, which says it is not
// signed in. The issue is closed for the socket when that read says so or the room's state
// records it closed: ServeHTTP's hold on the state (holdForAdmission) is where a close made while
// the socket loaded its document is recorded (setRoomIssueOpen), so such a socket is admitted
// read-only, as one opened after the close is. It only ever turns the admission read-only, never
// writable: a page decided read-only was given a client id stored nowhere (admitClients). A
// request no admission read the issue for is refused, as is a socket to a failed room.
//
// It records the admission ServeHTTP put on the socket's context with the service, so a re-check
// can close the socket again (schemaAdmissions); the answer to the socket's token is that same
// admission (authorizeSchemaVersion).
func (s *Service) authorize(r *http.Request) (websocket.ConnectionConfig, bool) {
	connection, _ := r.Context().Value(connectionContextKey{}).(*connectionState)
	if connection == nil {
		return websocket.ConnectionConfig{}, false
	}
	room := socketRoom(r)
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
	admission := connection.admission
	admission.key = schemaAdmissionKey{room: room, token: r.URL.Query().Get("schema_version")}
	admission.writable = admission.writable && open
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
