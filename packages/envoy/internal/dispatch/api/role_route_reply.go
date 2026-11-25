package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// replyForbiddenSession is the 403 REPLY_FORBIDDEN both reply routes give a session that may not
// answer the attempt it names (lookUpRoleHolderReply says who may).
const replyForbiddenSession = "session may reply only to its own delivery, to one sent for a role it holds while it is live, or to one the listener handed it from a session it replaced"

// roleHolderReply is what a reply route learned, before its transaction opened, of whether a
// session the attempt does not name may answer it. route is the attempt's route as its row names
// it (a message's target, a mention's target), and named is true only when the listener named the
// session replying for that route: as the holder of route's role, called live, for a route to a
// role; as a session that replaced the attempt's session, for a route to a session.
type roleHolderReply struct {
	route string
	named bool
}

// allows reports whether a session the attempt on route does not name may still answer it: the
// listener named it, by the lookup made for that same route.
func (r roleHolderReply) allows(route string) bool {
	return r.named && r.route == route
}

// roleHolderReplyOf is lookUpRoleHolderReply for the route and the attempt's session that the one
// row query names, scanned as (route, session_id), with id - the message's or the comment's - as
// the query's $1 and args after it. An id that is no uuid, and one that no row has, asks nothing
// and names no session, so the reply's transaction answers it as it always has. A read that failed
// (a statement timeout, a reset connection) is the error, which the route answers 500: whether the
// session replying may answer was not read, and a refusal would end a reply that the role's live
// holder, or the session that replaced the attempt's, may make again.
func (s *server) roleHolderReplyOf(ctx context.Context, actor model.Actor, id, query string, args ...any) (roleHolderReply, error) {
	if _, err := uuid.Parse(id); err != nil {
		return roleHolderReply{}, nil
	}
	var route string
	var attemptSession *string
	err := s.deps.Store.Pool.QueryRow(ctx, query, append([]any{id}, args...)...).Scan(&route, &attemptSession)
	if errors.Is(err, pgx.ErrNoRows) {
		return roleHolderReply{}, nil
	}
	if err != nil {
		return roleHolderReply{}, fmt.Errorf("read the route and the attempt that the reply to %s answers: %w", id, err)
	}
	return s.lookUpRoleHolderReply(ctx, route, attemptSession, actor)
}

// lookUpRoleHolderReply asks the listener whether actor may answer an attempt that names another
// session, or none yet (attemptSession), sent for route. The session that answers an attempt can
// be other than the one the attempt names, because the listener moves what a session had not read
// to another (packages/envoy/cmd/listener/role_takeover.go), and each copy keeps its delivery:
//
//   - For a route to a role, the role's holder now. A role's message goes to the session its claim
//     names when Dispatch sends it (resolveDeliveryTarget), and when a different session takes the
//     role the listener moves what that holder had not read to the role's next holder. The reply
//     routes accept it from that session while the listener's role lookup (GET /v1/roles/<role>,
//     Client.Role) names it and calls it live: a holder past the live window is answered by that
//     lookup as a lapsed one, and its reply is refused, as any other session's is.
//   - For a route to a session, a session that replaced the attempt's session. A claim that names
//     the session it replaces has the listener copy that session's unread direct messages to the
//     claimant (a handover), a person's Dispatch message among them. The reply routes accept the
//     reply from a session the listener's successor read (GET /v1/sessions/<session>/successors,
//     Client.Successors) names for the attempt's session, on a credential that acts for the
//     principal the listener names them for (actsForSuccessorPrincipal). An attempt that names
//     no session yet has no session replaced, and asks nothing.
//
// It is the resolve half of the reply's listener read (envoy_resolve.go): it runs before the
// reply's transaction opens, with nothing held, and its answer is compared with the route the
// locked row names. An attempt pins its session once and never changes it, so a row that named
// actor when it was read here names actor under the lock too, and no lookup is made for it.
//
// Any other route, an attempt that names actor, and a listener that answers that another session
// holds the role, or none does, or that names actor among no successors, or names it for a
// principal actor's credential does not act for, are no reply it allows. A
// lookup the listener did not answer (dispatchenvoy.ErrUnavailable) is the error, which the route
// answers 503 ENVOY_UNAVAILABLE: whether this session may answer is the listener's to say, and a
// refusal would end a reply its host could make again.
func (s *server) lookUpRoleHolderReply(ctx context.Context, route string, attemptSession *string, actor model.Actor) (roleHolderReply, error) {
	verdict := roleHolderReply{route: route}
	parsed, err := model.ParseRoute(route)
	if err != nil || s.deps.Envoy == nil || (attemptSession != nil && *attemptSession == actor.ID) {
		return verdict, nil
	}
	switch parsed.Kind {
	case "role":
		holder, err := s.deps.Envoy.Role(ctx, parsed.ID)
		if errors.Is(err, dispatchenvoy.ErrUnavailable) {
			return verdict, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE",
				"the Envoy listener did not say who holds %s, so Dispatch cannot tell whether %s may answer this delivery: %s", route, actor.ID, err.Error())
		}
		verdict.named = err == nil && holder.SessionID == actor.ID && holder.State == dispatchenvoy.StateLive
	case "session":
		if attemptSession == nil {
			return verdict, nil
		}
		successors, err := s.deps.Envoy.Successors(ctx, *attemptSession)
		if errors.Is(err, dispatchenvoy.ErrUnavailable) {
			return verdict, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE",
				"the Envoy listener did not say which sessions replaced %s, so Dispatch cannot tell whether %s may answer this delivery: %s", *attemptSession, actor.ID, err.Error())
		}
		verdict.named = err == nil && slices.Contains(successors.Successors, actor.ID) && actsForSuccessorPrincipal(actor, successors.Principal)
	}
	return verdict, nil
}

// actsForSuccessorPrincipal reports whether actor's credential may answer, as a successor the
// listener names, a delivery of a session that last registered as principal
// (listenerwire.SessionSuccessors). A credential that proved the replying session itself (a
// broker token, actor.Proven) acts for its person, else its service - the principal the listener
// records a registration on that token as (cmd/listener's requestPrincipal) - and must act for
// principal: the listener names a successor by the records of the two sessions' last
// registrations, and a session run on another principal's credential before its registration
// records that may not answer the earlier principal's deliveries. A proven credential acting for
// neither, and a principal of none, which no proven credential acts for, allow nothing. A
// credential that proves no session - the shared agent token, a personal token, a
// service-account token - names whichever session its body names, the attempt's own among them,
// so nothing more is asked of it here.
func actsForSuccessorPrincipal(actor model.Actor, principal string) bool {
	if !actor.Proven {
		return true
	}
	switch {
	case actor.Owner != nil && *actor.Owner != "":
		return *actor.Owner == principal
	case actor.Service != nil && *actor.Service != "":
		return *actor.Service == principal
	}
	return false
}
