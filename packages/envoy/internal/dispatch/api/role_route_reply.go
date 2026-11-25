package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/roles"
	envoystore "github.com/sjawhar/envoy/internal/store"
)

// RoleHolderReader is what the reply check resolves a role's holder through
// (roles.ResolveHolder): the role's claim, read from the role bucket stream's leader with the
// listener's one decoding of it (store.DecodeLeaderRoleClaim), and the holder's session record,
// judged by the windows the deployment gave its listeners. cmd/dispatch passes the outbox's
// roles.Buckets, so the reply check and the outbox read one holder.
type RoleHolderReader interface {
	roles.ClaimStore
	roles.SessionLookup
}

// roleHolderReply is what a reply route learned, before its transaction opened, of whether the
// session replying holds the role an attempt was sent for. route is the attempt's route as its
// row names it (a message's target, a mention's target), and holder is true only when the role's
// claim, read from the role bucket stream's leader, named that session and its record read live.
type roleHolderReply struct {
	route  string
	holder bool
}

// allows reports whether a session the attempt on route does not name may still answer it: it is
// the live holder of the role route names, by the read made for that same route.
func (r roleHolderReply) allows(route string) bool {
	return r.holder && r.route == route
}

// roleHolderReplyOf is lookUpRoleHolderReply for the route and the attempt's session that the one
// row query names, scanned as (route, session_id), with id - the message's or the comment's - as
// the query's $1 and args after it. An id that is no uuid, and one that no row has, asks nothing
// and names no holder, so the reply's transaction answers it as it always has. A read that failed
// (a statement timeout, a reset connection) is the error, which the route answers 500: whether the
// session replying holds the role was not read, and a refusal would end a reply that the role's
// live holder may make again.
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

// lookUpRoleHolderReply reads whether actor holds the role route names, for a reply to an attempt
// sent for a route to that role that names another session, or none yet (attemptSession). A
// role's message goes to the session its claim names when Dispatch sends it
// (resolveDeliveryTarget), and when a different session takes the role the listener moves what
// that holder had not read to the role's next holder (packages/envoy/cmd/listener), so the
// session that answers an attempt can be the role's holder now rather than the one the attempt
// names. The reply routes accept it from that session while the role's claim names it and its
// record reads live (roles.ResolveHolder over Deps.RoleHolders): a holder past the live window is
// lapsed, and its reply is refused, as any other session's is.
//
// The claim is read from the role bucket stream's leader, as every reader that decides which
// session a role's message reaches reads it (store.Registry.RoleClaimFromLeader), and not through
// the listener's role lookup (GET /v1/roles/<role>), a direct get: a direct get reads a message
// the NATS server stored under the role's key on its own as the claim of whichever session its
// body names, so a live session such a message names would have its reply taken for the
// holder's. Such a message, and any claim record no read can decode, names no holder here
// (store.ErrMalformedRoleClaim).
//
// It is the resolve half of the reply's read (envoy_resolve.go): it runs before the reply's
// transaction opens, with nothing held, and its answer is compared with the route the locked row
// names. An attempt pins its session once and never changes it, so a row that named actor when it
// was read here names actor under the lock too, and no read is made for it.
//
// A route that is not a role's, an attempt that names actor, a deployment with no reader of the
// role bucket (Deps.RoleHolders nil: no NATS), and a claim that names another session, or none,
// are no holder's reply. A read that did not answer - the claim's, the leader's included while the
// bucket has none, or the holder's session record's - is the error, which the route answers 503
// ENVOY_UNAVAILABLE: whether this session may answer was not read, and a refusal would end a reply
// its host could make again.
func (s *server) lookUpRoleHolderReply(ctx context.Context, route string, attemptSession *string, actor model.Actor) (roleHolderReply, error) {
	verdict := roleHolderReply{route: route}
	parsed, err := model.ParseRoute(route)
	if err != nil || parsed.Kind != "role" || s.deps.RoleHolders == nil || (attemptSession != nil && *attemptSession == actor.ID) {
		return verdict, nil
	}
	resolved, err := roles.ResolveHolder(ctx, s.deps.RoleHolders, s.deps.RoleHolders, parsed.ID)
	switch {
	case errors.Is(err, envoystore.ErrMalformedRoleClaim):
		return verdict, nil
	case err != nil:
		return verdict, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE",
			"Dispatch could not read who holds %s, so it cannot tell whether %s may answer this delivery: %s", route, actor.ID, err.Error())
	}
	verdict.holder = resolved.State == roles.Live && resolved.Holder == actor.ID
	return verdict, nil
}
