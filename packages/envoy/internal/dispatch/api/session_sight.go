package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// session_sight.go holds the api's half of a session's sight (access.SessionSeesProjectClause):
// Dispatch gives no session work of a project that session may not be shown, and sends it none of
// that project's items. The outbox applies the same rule to every event it publishes (outbox:
// destinations.withheld), and records nothing for a session it leaves out.
//
// What a writer asks for, the api refuses before it writes anything, 403
// SESSION_OUTSIDE_PROJECT (refuseSessionOutsideProject), so the writer sees why and nothing of the
// write is stored: a session route set on an issue (patchIssue), a session added to an ask's
// followers (changeAskFollower), a claim made in a session's name (claimIssue), a comment's mention
// of a session, or of a role whose holder the mention resolves to such a session
// (createCommentFor, refuseMentionSessionsOutsideProject), and a targeted issue message to such a
// session, or to a role whose holder it is (createMessage, refuseMessageTargetOutsideProject;
// createStoredMessage for a reply's inherited session target).
//
// What no writer asks for at the moment of the send - a mention's or a message's send made after
// its write, by its sender's lane, a Retry or the delivery sweep, whose session the resolution then
// names - is judged again at the send itself (sendResolvedDelivery, queueForUnknownSession): such a
// session is sent nothing, and the attempt settles failed with why (withheldFromSession), which is
// what its card shows the person who wrote it.
//
// A write or a delivery of no project - a direct message, a file of an agent's conversation - is
// judged by nothing here, as before.

// projectWithheldPrefix begins the failure a delivery records when its session may not be shown
// the delivery's project (withheldFromSession): nothing was sent, and no later try sends it, so
// retryableDeliveryFailure does not retry it, and Dispatch queues nothing in the listener's place
// (mayQueueForUnknownSession reads only noLiveSessionPrefix).
const projectWithheldPrefix = "withheld: "

// sightUnreadPrefix begins the failure a delivery records when Dispatch could not read whether its
// session may be shown the delivery's project (withheldFromSession), as when its database failed
// for a moment: nothing was sent, and the next try reads the sight again, so
// retryableDeliveryFailure retries it.
const sightUnreadPrefix = "sight unread: "

// deliveryProject is the project a comment's or a message's delivery is of: its issue's, else its
// document's (graphstore.ScopeOf), and "" for one of no project - a direct message, a file of an
// agent's conversation.
func (s *server) deliveryProject(ctx context.Context, issueKey, artifactID *string) (string, error) {
	if issueKey == nil && artifactID == nil {
		return "", nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return "", errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	project, _, err := graphstore.ScopeOf(ctx, s.deps.Store.Pool, issueKey, artifactID)
	return project, err
}

// withheldFromSession is the failure a delivery of project to session records instead of its
// send, and "" when the send may go: session sees project (access.SessionSeesProjectClause), or
// project is "", a delivery of no project. A sight that could not be read withholds the send too,
// and its failure says so: a send Dispatch cannot judge is not made. A read that failed fails under
// sightUnreadPrefix, which a later try outlasts, rather than being withheld for good.
//
// Where the verdict can depend on a listener list and the stored listener record has expired, the
// record is refreshed first (refreshedSessionSight): a send of the sender's lane, a Retry or the
// delivery sweep has no request whose reader refreshed it. A record still expired after the
// refresh, as when the listener does not answer, is a list Dispatch could
// not read, so the send fails under listenerUnreachablePrefix, which a later try outlasts
// (retryableDeliveryFailure), rather than being withheld for good. Where no verdict depends on a
// list, and with no listener configured, which lists nothing, the record is judged as it stands.
func (s *server) withheldFromSession(ctx context.Context, session, project string) string {
	if project == "" {
		return ""
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return projectWithheldPrefix + "Dispatch has no database to read whether session " + session + " sees project " + project
	}
	sees, expired, err := s.refreshedSessionSight(ctx, session, project)
	switch {
	case err != nil:
		return sightUnreadPrefix + "could not read whether session " + session + " sees project " + project + ", so nothing was sent: " + err.Error()
	case expired:
		return listenerUnreachablePrefix + "the stored listener record has expired and the listener did not answer again, " +
			"so Dispatch could not read whether session " + session + " sees project " + project + " and sent nothing"
	case !sees:
		return fmt.Sprintf("%ssession %s does not see project %s, so nothing was sent", projectWithheldPrefix, session, project)
	}
	return ""
}

// sessionSight is whether session sees project (access.SessionSeesProject), read through q, and
// whether that verdict depends on a listener list of a stored listener record that has expired
// (expired), which no caller judges by. With no listener configured, which lists nothing, the
// record is judged as it stands, and no verdict is expired.
func (s *server) sessionSight(ctx context.Context, q access.RowsQuerier, session, project string) (sees, expired bool, err error) {
	sees, expired, err = access.SessionSeesProject(ctx, q, session, project)
	return sees, s.deps.Envoy != nil && expired, err
}

// refreshedSessionSight is sessionSight on the pool, where a verdict that depends on a list of a
// stored listener record that has expired has the record refreshed first, as the session content
// rule's readers refresh it (refreshRosterForContent), and is read again: a send judged at the
// send (withheldFromSession) and a write judged before its transaction opens
// (refuseVisibleSessionOutsideProject). expired is a record still expired after the refresh, as
// when the listener does not answer. Where no verdict depends on a list it asks the listener
// nothing.
func (s *server) refreshedSessionSight(ctx context.Context, session, project string) (sees, expired bool, err error) {
	sees, expired, err = s.sessionSight(ctx, s.deps.Store.Pool, session, project)
	if err == nil && expired {
		s.refreshRosterForContent(ctx)
		sees, expired, err = s.sessionSight(ctx, s.deps.Store.Pool, session, project)
	}
	return sees, expired, err
}

// refuseSessionOutsideProject is the refusal of a write that gives session work of project, or
// sends it one of project's items - what names the work - when the session may not be shown that
// project (sessionSight): 403 SESSION_OUTSIDE_PROJECT, naming the session, the
// project and what proves a session's access to it, so the agent that wrote can put that right.
// Every caller names a project the writer reads, which the refusal may therefore name: an issue
// read as the writer reads it (issueProjectSeenBy; createStoredMessage's issue, read so before),
// an ask or a comment's owner the write has already read as the writer reads it
// (changeAskFollower's loadAskOwner, refuseOwnerBeforeResolving). A write of no project, and one
// whose session sees the project, are refused nothing. It reads through q, the write's
// transaction where it holds one, so it refreshes no listener record: the write refreshes it before
// its transaction opens (refreshRosterBeforeSight), and a verdict that still depends on a list of
// an expired record is refused 503 ENVOY_UNAVAILABLE (sessionSightRefusal).
func (s *server) refuseSessionOutsideProject(ctx context.Context, q access.RowsQuerier, session, project, what string) error {
	if project == "" {
		return nil
	}
	sees, expired, err := s.sessionSight(ctx, q, session, project)
	return sessionSightRefusal(sees, expired, err, session, project, what)
}

// sessionSightRefusal is the refusal of a write by one read of whether session sees project
// (sessionSight): the read's err; nil where session sees project; 503 ENVOY_UNAVAILABLE, which a
// later try outlasts, where the verdict depends on a listener list Dispatch could not read
// (expired), as when the listener does not answer, since neither a refusal nor the write may rest
// on that list; and 403 SESSION_OUTSIDE_PROJECT otherwise.
func sessionSightRefusal(sees, expired bool, err error, session, project, what string) error {
	switch {
	case err != nil:
		return err
	case expired:
		return errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE", sessionSightListExpiredText, session, project)
	case sees:
		return nil
	}
	return errorf(http.StatusForbidden, codeSessionOutsideProject, sessionOutsideProjectText, session, project, what, project, project)
}

// sessionSightListExpiredText is the message of sessionSightRefusal's 503 ENVOY_UNAVAILABLE, with
// the session and the project: the listener did not answer, so nothing was written.
const sessionSightListExpiredText = "the Envoy listener did not answer Dispatch's read of its sessions, so Dispatch cannot tell " +
	"whether session %s may see project %s; nothing was written: try again"

// refreshRosterBeforeSight refreshes the stored listener record (refreshRosterForContent) before a
// write opens the transaction in which it judges a session's sight (refuseSessionOutsideProject):
// no refresh runs inside a transaction, and the session the write judges may be read only in it,
// as a reply's inherited target is. It refreshes wherever a verdict of the rule can depend on a
// listener list (access.SightMayDependOnAListSQL), so a deployment that holds no private project
// and no restricted member, and a server with no listener, ask the listener nothing.
func (s *server) refreshRosterBeforeSight(ctx context.Context) {
	if s.deps.Envoy == nil || s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	var depends bool
	if err := s.deps.Store.Pool.QueryRow(ctx, access.SightMayDependOnAListSQL).Scan(&depends); err == nil && depends {
		s.refreshRosterForContent(ctx)
	}
}

// sessionOutsideProjectText is SESSION_OUTSIDE_PROJECT's message, with the session, the project,
// what the write asked of the session, and the project twice more: what proves a session's access
// to a project, by access.SessionSeesProjectClause - every person the session acts for sees it,
// and the project list its token carries, where the listener has one, names it.
const sessionOutsideProjectText = "session %s cannot see project %s, so it cannot %s: " +
	"tie the session to a person who sees %s (a broker-issued token of that person proves the session), " +
	"and let the session's token list %s where it lists projects"

// refuseVisibleSessionOutsideProject is refuseSessionOutsideProject, read on the pool with the
// stored listener record refreshed where the verdict depends on a list of a record that has
// expired (refreshedSessionSight), for a session writer may see (sessionHiddenFrom). A session
// hidden from writer is refused nothing here: a refusal, or its absence, would tell writer what
// that session sees, and such a session is sent nothing by the api already (resolveDeliveryTarget
// answers it as one nobody holds), while what the outbox would send it is withheld there (outbox:
// destinations.withheld). Whether it is hidden is read only for a session the rule refuses.
func (s *server) refuseVisibleSessionOutsideProject(ctx context.Context, writer model.Actor, session, project, what string) error {
	if project == "" {
		return nil
	}
	sees, expired, err := s.refreshedSessionSight(ctx, session, project)
	err = sessionSightRefusal(sees, expired, err, session, project, what)
	var refusal *apiError
	if !errors.As(err, &refusal) {
		return err
	}
	hidden, hiddenErr := s.sessionHiddenFrom(ctx, writer, session)
	switch {
	case hiddenErr != nil:
		return hiddenErr
	case hidden:
		return nil
	}
	return err
}

// issueProjectSeenBy is the project of issue key as who reads it (access.ProjectClause), and ""
// for an issue who does not see or no row holds: the write that asks answers such an issue itself,
// as it answers one nobody holds, before it writes anything.
func issueProjectSeenBy(ctx context.Context, q queryer, who model.Actor, key string) (string, error) {
	var project string
	err := q.QueryRow(ctx, `select project_key from issues where key = $1 and `+
		access.ProjectClause("project_key", "$2"), key, refs.VisibilityArg(who)).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return project, err
}

// sessionTargets is the session each of targets names (session:<id>), in order; a role names none
// until a resolution names its holder.
func sessionTargets(targets []string) []string {
	var sessions []string
	for _, target := range targets {
		if route, err := model.ParseRoute(target); err == nil && route.Kind == "session" {
			sessions = append(sessions, route.ID)
		}
	}
	return sessions
}

// resolvedRoleHolders is the session each role mention of mentions resolved to, in order: the
// holders a comment's mentions name only once they are resolved (resolveCommentDeliveries).
func resolvedRoleHolders(mentions []ResolvedMention) []string {
	var holders []string
	for _, mention := range mentions {
		if route, err := model.ParseRoute(mention.Target); err == nil && route.Kind == "role" && mention.SessionID != nil {
			holders = append(holders, *mention.SessionID)
		}
	}
	return holders
}

// refuseMentionSessionsOutsideProject refuses a comment whose mention names a session that may not
// be shown project, the project the comment's owner is of, and that writer may see
// (refuseVisibleSessionOutsideProject): a mention sends the session the comment, and the outbox's
// routes the comment's later thread. The comment asks it twice before it writes anything: of the
// sessions its mentions name, once its owner is known to be one the writer reads
// (refuseOwnerBeforeResolving), so a refused comment costs no listener call; and of the holders its
// role mentions resolve to (resolvedRoleHolders), in the resolution itself, which runs with nothing
// held (resolveThenLock). A role mention whose resolution names no holder is judged when its send
// names one (sendResolvedDelivery).
func (s *server) refuseMentionSessionsOutsideProject(ctx context.Context, writer model.Actor, project string, sessions []string) error {
	for _, session := range sessions {
		if err := s.refuseVisibleSessionOutsideProject(ctx, writer, session, project, "be mentioned on it"); err != nil {
			return err
		}
	}
	return nil
}

// refuseMessageTargetOutsideProject refuses a targeted message on issue key whose role target's
// holder - the session the listener names for the role now (resolveMentionTarget), which the
// message's send reaches - may not be shown the issue's project, as reader reads the issue
// (issueProjectSeenBy), before the message is written (refuseVisibleSessionOutsideProject). It runs
// before the message's transaction opens, the role's resolution being a listener call. A role that
// no session holds now, or whose holder reader may not see, names nobody to judge: the listener
// keeps such a message for the role's next holder, whose send is judged as every send is
// (sendResolvedDelivery). A session target is judged in the message's transaction, where a reply
// that names no target inherits its thread's (createStoredMessage).
//
// It answers the resolution it made, nil where it made none, which the message's first send takes
// as its own rather than asking the listener a second time (deliverMessageWith): a role-targeted
// message makes one role lookup for its first attempt, as it did before this check.
func (s *server) refuseMessageTargetOutsideProject(
	ctx context.Context, reader model.Actor, key string, target *string, delivery string,
) (*ResolvedMention, error) {
	if target == nil {
		return nil, nil
	}
	route, err := model.ParseRoute(*target)
	if err != nil || route.Kind != "role" {
		return nil, nil
	}
	project, err := issueProjectSeenBy(ctx, s.deps.Store.Pool, reader, key)
	if err != nil || project == "" {
		return nil, err
	}
	resolved := s.resolveMentionTarget(ctx, *target, delivery, 0)
	if resolved.SessionID == nil {
		return &resolved, nil
	}
	if err := s.refuseVisibleSessionOutsideProject(ctx, reader, *resolved.SessionID, project, "be sent this message"); err != nil {
		return nil, err
	}
	return &resolved, nil
}
