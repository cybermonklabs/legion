package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// messageReplyRoleHolderOf is the lookUpRoleHolderReply of a reply to attempt of message id: the
// message's target, as who reads it (leaveOutRoute), and the session the attempt names. A target
// who does not see is none, as the reply's transaction reads it, so no listener is asked about its
// role; an attempt pinned to a session who does not see is asked about as an attempt no row holds,
// as the transaction answers it (replyAttemptHidden). An id that is no uuid, and a message who does
// not see (access.MessageClause) or no row holds, ask nothing and name no holder, so the reply's
// transaction answers them as it always has. The read of what who sees is made whatever the message
// targets and its attempt names. A read that failed (a statement timeout, a reset connection) is
// the error, which the route answers 500: whether the session replying holds the role was not read,
// and a refusal would end a reply that the role's live holder may make again.
func (s *server) messageReplyRoleHolderOf(ctx context.Context, actor, who model.Actor, id string, attempt int) (roleHolderReply, error) {
	if _, err := uuid.Parse(id); err != nil {
		return roleHolderReply{}, nil
	}
	var target, attemptSession *string
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select m.target, d.session_id
		from messages m left join message_deliveries d on d.message_id = m.id and d.attempt = $2
		where m.id = $1 and `+access.MessageClause("m", "$3")+`
	`, id, attempt, refs.VisibilityArg(who)).Scan(&target, &attemptSession)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return roleHolderReply{}, fmt.Errorf("read the route and the attempt that the reply to %s answers: %w", id, err)
	}
	named := references{}
	named.nameRoute(target)
	named.name(sessionReference, pinnedSession(attemptSession))
	seen, err := seenReferences(ctx, s.deps.Store.Pool, who, named)
	if err != nil {
		return roleHolderReply{}, err
	}
	seen.leaveOutRoute(&target)
	if target == nil {
		return roleHolderReply{}, nil
	}
	if pinnedHidden(seen, attemptSession) {
		attemptSession = nil
	}
	return s.lookUpRoleHolderReply(ctx, *target, attemptSession, actor)
}

// replyRoleHolderOf is the lookUpRoleHolderReply of a reply to an attempt of comment id: the
// mention the reply answers is picked among the comment's mentions as a reply reads them
// (mentionsRepliedToBy), as the reply's transaction picks it - the one target names, or the only
// one when target is nil - and the attempt's session is attempt's of that mention. A mention whose
// target who does not see is none, as one the comment does not have, so no listener is asked about
// its role; an attempt pinned to a session who does not see is asked about as an attempt no row
// holds, as the transaction answers it (replyAttemptHidden). An id that is no uuid, a comment who
// does not see (access.ItemOwnerClause) or has not got, and a pick that finds no mention ask
// nothing and name no holder, so the reply's transaction answers them as it always has. Its one
// read of what who sees names every mention's target and every attempt's session, whatever they
// name. A read that failed is the error, which the route answers 500.
func (s *server) replyRoleHolderOf(
	ctx context.Context, actor, who model.Actor, id string, target *string, attempt int,
) (roleHolderReply, error) {
	if _, err := uuid.Parse(id); err != nil {
		return roleHolderReply{}, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select m.target, d.session_id
		from comment_mentions m
		left join comment_deliveries d on d.comment_id = m.comment_id and d.target = m.target and d.attempt = $2
		join comments c on c.id = m.comment_id
		where m.comment_id = $1 and `+access.ItemOwnerClause("c.issue_key", "c.artifact_id", "$3")+`
		order by m.target
	`, id, attempt, refs.VisibilityArg(who))
	if err != nil {
		return roleHolderReply{}, fmt.Errorf("read the mentions and the attempt that the reply to %s answers: %w", id, err)
	}
	var targets []string
	attemptSessions := map[string]*string{}
	named := references{}
	for rows.Next() {
		var mentioned string
		var attemptSession *string
		if err := rows.Scan(&mentioned, &attemptSession); err != nil {
			rows.Close()
			return roleHolderReply{}, fmt.Errorf("read the mentions and the attempt that the reply to %s answers: %w", id, err)
		}
		targets = append(targets, mentioned)
		attemptSessions[mentioned] = attemptSession
		named.nameRoute(&mentioned)
		named.name(sessionReference, pinnedSession(attemptSession))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return roleHolderReply{}, fmt.Errorf("read the mentions and the attempt that the reply to %s answers: %w", id, err)
	}
	seen, err := seenReferences(ctx, s.deps.Store.Pool, who, named)
	if err != nil {
		return roleHolderReply{}, err
	}
	replied := slices.DeleteFunc(targets, func(mentioned string) bool { return !seen.holdsRoute(mentioned) })
	route := ""
	switch {
	case target != nil:
		if slices.Contains(replied, *target) {
			route = *target
		}
	case len(replied) == 1:
		route = replied[0]
	}
	if route == "" {
		return roleHolderReply{}, nil
	}
	attemptSession := attemptSessions[route]
	if pinnedHidden(seen, attemptSession) {
		attemptSession = nil
	}
	return s.lookUpRoleHolderReply(ctx, route, attemptSession, actor)
}

// mentionsRepliedToBy is mentions less every one whose target who does not see
// (references.holdsRoute: a `session:<id>` target of a session who does not see, or a role of a
// private project who does not see), as a delivery's reply reads them (replyComment). It does not
// judge a mention by the session it resolved to when its comment was written, as every read of the
// comment and a Retry do (mentionsReadBy): that resolution (comment_mentions.resolved_session_id)
// is written once, and a role passes from session to session, so a role's mention resolved to a
// holder that acted in a project who does not see would refuse the reply of the role's holder now,
// to whom a Retry sent a later attempt. The session a reply may answer for is the attempt's own,
// which the reply judges apart (replyAttemptHidden). Its read is made whatever the mentions name,
// so a comment with a hidden mention costs what one without it costs; a reader that sees every
// object reads nothing (R9).
func mentionsRepliedToBy(ctx context.Context, q queryer, who model.Actor, mentions []model.Mention) ([]model.Mention, error) {
	if seesEveryObject(who) {
		return mentions, nil
	}
	named := references{}
	for _, mention := range mentions {
		named.nameRoute(&mention.Target)
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(mentions), func(mention model.Mention) bool { return !seen.holdsRoute(mention.Target) }), nil
}

// replyAttemptHidden reports whether who does not see pinned, the session a delivery attempt is
// pinned to (nil or "" for none yet), as every read of the item's deliveries leaves such an
// attempt out (leaveOutHiddenCommentSessions, leaveOutHiddenMessageSessions). A reply to it is
// answered as one to an attempt no row holds, whoever replies: the session the attempt is pinned
// to, through a credential that does not see it, or the role's holder now, whose answer would name
// that session. Its read is made whatever the attempt names, so an attempt pinned to a hidden
// session costs what one pinned to none costs; a reader that sees every object reads nothing (R9).
func replyAttemptHidden(ctx context.Context, q queryer, who model.Actor, pinned *string) (bool, error) {
	named := references{}
	named.name(sessionReference, pinnedSession(pinned))
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return false, err
	}
	return pinnedHidden(seen, pinned), nil
}

// pinnedSession is the session an attempt row names, "" for none.
func pinnedSession(pinned *string) string {
	if pinned == nil {
		return ""
	}
	return *pinned
}

// pinnedHidden reports whether seen leaves out the session an attempt is pinned to; an attempt
// pinned to none is never hidden.
func pinnedHidden(seen references, pinned *string) bool {
	session := pinnedSession(pinned)
	return session != "" && !seen.holds(sessionReference, session)
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
// Any other route, an attempt that names actor, and a listener that answers that another session
// holds the role, or none does, or that names actor among no successors, or names it for another
// principal than the one actor's credential acts for (actsFor, callerPrincipal), are no reply it
// allows. A
// lookup the listener did not answer (dispatchenvoy.ErrUnavailable) is the error, which the route
// answers 503 ENVOY_UNAVAILABLE: whether this session may answer is the listener's to say, and a
// refusal would end a reply its host could make again.
func (s *server) lookUpRoleHolderReply(ctx context.Context, route string, attemptSession *string, actor model.Actor, actsFor string) (roleHolderReply, error) {
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
		verdict.named = err == nil && slices.Contains(successors.Successors, actor.ID) && actsForSuccessorPrincipal(actor, actsFor, successors.Principal)
	}
	return verdict, nil
}

// actsForSuccessorPrincipal reports whether actor, on a credential acting for actsFor, may answer,
// as a successor the listener names, a delivery of a session that last registered as principal
// (listenerwire.SessionSuccessors). A credential that proved the replying session itself (a
// broker token, actor.Proven) acts for the principal its token names, in the form that names the
// token's kind and scope (ciphertoken.Caller.Principal, callerPrincipal) - the principal the
// listener records a registration on that token as (cmd/listener's requestPrincipal) - and must
// act for principal: the listener names a successor by the records of the two sessions' last
// registrations, and a session run on another principal's credential before its registration
// records that may not answer the earlier principal's deliveries. So an agent's token that an
// operator's entry gave a person for a project may not answer a delivery of that person's own
// session. A proven credential naming no principal, and a principal of none, which no proven
// credential acts for, allow nothing. A credential that proves no session - the shared agent
// token, a personal token, a service-account token - names whichever session its body names, the
// attempt's own among them, so nothing more is asked of it here.
func actsForSuccessorPrincipal(actor model.Actor, actsFor, principal string) bool {
	if !actor.Proven {
		return true
	}
	return actsFor != "" && actsFor == principal
}
