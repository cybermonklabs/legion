package api

import (
	"context"
	"errors"
	"net/http"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// roleHolderReply is what a reply route learned, before its transaction opened, of whether the
// session replying holds the role an attempt was sent for. route is the attempt's route as its
// row names it (a message's target, a mention's target), and holder is true only when the
// listener's role lookup named that session as the holder of route's role and called it live.
type roleHolderReply struct {
	route  string
	holder bool
}

// allows reports whether a session the attempt on route does not name may still answer it: it is
// the live holder of the role route names, by the lookup made for that same route.
func (r roleHolderReply) allows(route string) bool {
	return r.holder && r.route == route
}

// roleHolderReplyOf is lookUpRoleHolderReply for the route and the attempt's session that the one
// row query names, scanned as (route, session_id). A query that finds no row, or fails, asks
// nothing and names no holder, so the reply's transaction answers it as it always has.
func (s *server) roleHolderReplyOf(ctx context.Context, actor model.Actor, query string, args ...any) (roleHolderReply, error) {
	var route string
	var attemptSession *string
	if s.deps.Store.Pool.QueryRow(ctx, query, args...).Scan(&route, &attemptSession) != nil {
		return roleHolderReply{}, nil
	}
	return s.lookUpRoleHolderReply(ctx, route, attemptSession, actor)
}

// lookUpRoleHolderReply asks the listener whether actor holds the role route names, for a reply
// to an attempt sent for a route to that role that names another session, or none yet
// (attemptSession). A role's message goes to the session its claim names when Dispatch sends it
// (resolveDeliveryTarget), and when a different session takes the role the listener moves what
// that holder had not read to the role's next holder (packages/envoy/cmd/listener), so the
// session that answers an attempt can be the role's holder now rather than the one the attempt
// names. The reply routes accept it from that session while the listener's role lookup (GET
// /v1/roles/<role>, Client.Role) names it and calls it live: a holder past the live window is
// answered by that lookup as a lapsed one, and its reply is refused, as any other session's is.
//
// It is the resolve half of the reply's listener read (envoy_resolve.go): it runs before the
// reply's transaction opens, with nothing held, and its answer is compared with the route the
// locked row names. An attempt pins its session once and never changes it, so a row that named
// actor when it was read here names actor under the lock too, and no lookup is made for it.
//
// A route that is not a role's, an attempt that names actor, and a listener that answers that
// another session holds the role, or none does, are no holder's reply. A lookup the listener did
// not answer (dispatchenvoy.ErrUnavailable) is the error, which the route answers 503
// ENVOY_UNAVAILABLE: whether this session may answer is the listener's to say, and a refusal
// would end a reply its host could make again.
func (s *server) lookUpRoleHolderReply(ctx context.Context, route string, attemptSession *string, actor model.Actor) (roleHolderReply, error) {
	verdict := roleHolderReply{route: route}
	parsed, err := model.ParseRoute(route)
	if err != nil || parsed.Kind != "role" || s.deps.Envoy == nil || (attemptSession != nil && *attemptSession == actor.ID) {
		return verdict, nil
	}
	holder, err := s.deps.Envoy.Role(ctx, parsed.ID)
	if errors.Is(err, dispatchenvoy.ErrUnavailable) {
		return verdict, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE",
			"the Envoy listener did not say who holds %s, so Dispatch cannot tell whether %s may answer this delivery: %s", route, actor.ID, err.Error())
	}
	verdict.holder = err == nil && holder.SessionID == actor.ID && holder.State == dispatchenvoy.StateLive
	return verdict, nil
}
