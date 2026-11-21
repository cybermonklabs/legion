package api

import (
	"context"
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
	classes := readClasses[route].Classes
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
	var sees bool
	err := s.deps.Store.Pool.QueryRow(ctx, `select `+access.SessionClause("$1::text", "$2"), id, refs.VisibilityArg(reader)).Scan(&sees)
	return !sees, err
}

// sessionContentHiddenFrom reports whether reader may not read session id's whole conversation
// (access.SessionContentClause): the rule the live agent view keeps, since it relays the session's
// raw transcript, not one message at a time as the HTTP read filters it. A viewer that is not open
// reads only a session its own person's credential proved. An open reader of a deployment that
// holds no private project reads every session and costs no read (R9); a server with no database
// reads none, failing closed.
func (s *server) sessionContentHiddenFrom(ctx context.Context, reader model.Actor, id string) (bool, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return false, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return true, nil
	}
	var reads bool
	err := s.deps.Store.Pool.QueryRow(ctx, `select `+access.SessionContentClause("$1::text", "$2"), id, refs.VisibilityArg(reader)).Scan(&reads)
	return !reads, err
}

// liveReader resolves what stream's caller sees now (readerFor), for liveReadRefusal.
func (s *server) liveReader(stream liveStreamer) func(context.Context) (model.Actor, error) {
	return func(ctx context.Context) (model.Actor, error) { return s.readerFor(ctx, stream.Caller()) }
}
