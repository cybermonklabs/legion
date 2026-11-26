package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// liveViewReads names, for every view kind /ws/live serves, the GET row whose rule the view keeps:
// the row that reads what the view sends. A view answers a caller as that row would: refused with
// the row's code where the row refuses (liveReadRefusal), and of an object the row hides, as the
// row answers a missing one (an agent view of a session the caller may not see, sessionHiddenFrom).
// TestEveryLiveViewKeepsTheRuleOfItsHTTPRead holds every kind the socket serves to an entry here.
var liveViewReads = map[string]string{
	liveEventsView:       "GET /api/v1/events/log",
	liveAgentView:        "GET /api/v1/agents/{session_id}/messages",
	deliveryLiveTimeline: "GET /api/v1/delivery/timeline",
	deliveryLiveMeasures: "GET /api/v1/delivery/measures",
	deliveryLivePipeline: "GET /api/v1/delivery/pipeline",
	legionBoardView:      "GET /api/v1/projects/{key}/legion/board",
}

// liveReadRefusal is the error frame a view of kind answers sub's caller where the view's GET row
// (liveViewReads) refuses that caller before it reads anything, or nil: the row's first OAuth scope
// the socket's credential does not hold (holds; scopeReadRefusal), then, for a row of the
// deployment, a viewer that is not open (deploymentReadRefusal). reader is read only for a row of
// the deployment, so a view of any other row costs no read here. A kind no entry names is refused
// as the server's own fault, never served.
func liveReadRefusal(ctx context.Context, sub, kind string, holds func(string) bool, reader func(context.Context) (model.Actor, error)) *LiveError {
	route, named := liveViewReads[kind]
	if !named {
		refusal := liveRefusal(sub, codeInternal, "this view keeps the rule of no read")
		return &refusal
	}
	method, pattern, _ := strings.Cut(route, " ")
	if refused := scopeReadRefusal(routeScopes[route], holds); refused != nil {
		refusal := liveRefusal(sub, refused.code, refused.message)
		return &refusal
	}
	classes := readSources[route].Classes
	if !slices.Contains(classes, readclass.Deployment) {
		return nil
	}
	resolved, err := reader(ctx)
	if err != nil {
		refusal := liveRefusal(sub, codeInternal, "the subscription could not resolve what its caller sees")
		return &refusal
	}
	if refused := deploymentReadRefusal(method, pattern, classes, resolved.Viewer); refused != nil {
		refusal := liveRefusal(sub, refused.code, refused.message)
		return &refusal
	}
	return nil
}

// sessionHiddenFrom reports whether reader may not see session id (access.SessionClause): the rule
// every read of one session keeps, over HTTP (sessionHiddenFromReader, and listAgentMessages's
// statement) and on the live socket's agent view. An open reader of a deployment that holds no
// private project sees every session and costs no read (R9); a server with no database sees none,
// failing closed.
func (s *server) sessionHiddenFrom(ctx context.Context, reader model.Actor, id string) (bool, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return false, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return true, nil
	}
	return sessionHiddenThrough(ctx, s.deps.Store.Pool, reader, id)
}

// sessionHiddenThrough is sessionHiddenFrom's read made through q, which a write's read phase
// makes on the transaction it holds (createStoredMessage): one statement, none for an open reader
// of a deployment that holds no private project (R9).
func sessionHiddenThrough(ctx context.Context, q queryer, reader model.Actor, id string) (bool, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return false, nil
	}
	var sees bool
	err := q.QueryRow(ctx, `select `+access.SessionClause("$1::text", "$2"), id, refs.VisibilityArg(reader)).Scan(&sees)
	return !sees, err
}

// sessionContentHiddenFrom reports whether reader may not read session id's whole conversation
// (access.SessionContentClause): the rule the live agent view keeps, since it relays the session's
// raw transcript, not one message at a time as the HTTP read filters it. A viewer that is not open
// reads only a session its own person's credential proved. An open reader of a deployment that
// holds no private project reads every session and costs no read (R9); a server with no database
// reads none, failing closed. The rule reads a listener list only from a stored listener answer
// that has not passed its expires_at, so the answer is refreshed first (refreshRosterForContent).
func (s *server) sessionContentHiddenFrom(ctx context.Context, reader model.Actor, id string) (bool, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return false, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return true, nil
	}
	s.refreshRosterForContent(ctx)
	var reads bool
	err := s.deps.Store.Pool.QueryRow(ctx, `select `+access.SessionContentClause("$1::text", "$2"), id, refs.VisibilityArg(reader)).Scan(&reads)
	return !reads, err
}

// refreshRosterForContent refreshes the stored listener answer before the session content rule
// reads it (readRouteRosterLeaving: the answer this process and every other keeps for
// routeRosterTTL, fetched once when it has gone stale), since the rule reads a listener list only
// from a stored answer that has not passed its expires_at, and judges a session of an expired answer
// by its ties alone. It counts the answer stale while it has the margin or less of its life left
// (rosterCache.contentMargin, three seconds), not only once it has expired, so the reads that
// follow the refresh in the request read an answer that is still fresh. A refresh that fails
// leaves the stored answer as it was, which the rule judges by the ties once it has expired: it
// fails closed. While the listener does not answer, the refresh waits for the one fetch in flight,
// which routeRosterTimeout (two seconds) bounds, and the fetch then stores the silence for every
// process to read. Every resolution of a reader that does not see every object runs it
// (readerFor), so each request, and each re-check of a live socket's reader, refreshes the answer
// before it reads; a request that waits on the listener after that refreshes again before it reads
// back (refreshRosterBeforeReadBack); the live agent view's asks and the document service's reads
// of its rooms (docs.Service.SetRosterRefresh) run it too.
func (s *server) refreshRosterForContent(ctx context.Context) {
	if _, err := s.readRouteRosterLeaving(ctx, s.rosters.contentMargin()); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: refresh the listener's sessions before the session content rule reads them; the rule reads an expired answer as no list", "error", err)
	}
}

// refreshRosterBeforeReadBack refreshes the stored listener answer again (refreshRosterForContent)
// for who, a reader that does not see every object, in a handler that waited on the listener after
// readerFor refreshed it and reads the session content rule after that wait. The margin of the
// first refresh covers a request's own reads, not a wait on the listener: a role lookup is bounded
// by the listener client's five seconds, and a send by the take-over window. The handlers that wait
// so are a delivery's reply (replyComment and replyMessage, after the role lookup), a comment that
// mentions a target (createCommentFor, after its mention lookups) and the repeat of a keyed send
// (recoveredMessage, after the send it owes). A reader that sees every object, and a server with no
// database, read nothing for it (R9).
func (s *server) refreshRosterBeforeReadBack(ctx context.Context, who model.Actor) {
	if seesEveryObject(who) || s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	s.refreshRosterForContent(ctx)
}

// liveReader resolves what stream's caller sees now (readerFor), for liveReadRefusal.
func (s *server) liveReader(stream liveStreamer) func(context.Context) (model.Actor, error) {
	return func(ctx context.Context) (model.Actor, error) { return s.readerFor(ctx, stream.Caller()) }
}
