package api

// project_scope.go holds what stands in front of every write: the caller's viewer, resolved once
// for the write and handed to its handler and loaders, the deployment's rows held to an open
// viewer, a session's own rows held to the session a credential proved, and no write by a
// credential that names no person. owner.go is not this: it resolves collaboration ownership, the
// issue or unlinked document a comment, an ask or an event hangs from, and decides no authority.
//
// Each check runs in one place — withinProjectScope and withinSessionScope, which register wraps
// around every mutating route after the caller is resolved and before the handler — rather than
// in each of the routes that write.
//
// A write to a project's object is decided where the object is read, not here. Every loader a
// write reads its object through binds the reader's project clause (access.ProjectClause), so an
// object of a project the caller does not see is a row the read does not find, and the write
// answers it exactly as it answers an object nobody holds: the same status and the same bytes,
// and nothing written. That holds for every caller alike — a person at a browser, a credential
// that lists the projects it may use, the shared token — because the viewer is what differs, not
// the rule. A credential that lists projects sees only those of them its person sees
// (access.Resolve), so its write to an object outside its list is one of those it does not see.
// TestNoWriteReachesAHiddenObject calls every write row that names an object, on PRIV's object and
// on one nobody holds, and holds the two to one answer; undecidableWrites refuses to mount a
// project's write that walk has no probe for.
//
// A read-only token, which carries `project_scope` `none` and the scope `read` alone and names a
// service and no person, never reaches these checks: the read-only gate register puts outside
// them answers that token 403 READ_ONLY_TOKEN on every route the read list does not name, every
// write among them (refuseReadOnlyToken, readScopeRoutes).
//
// A target outside a credential's list is the handler's missing object, so no refusal names the
// project an object it cannot see belongs to. The two writes that take their project from the body
// rather than the path, the issue create and the going request (scopeProjectInBody), refuse a
// project outside the list by name, since the name is the caller's own.
//
// One credential is held otherwise while the deployment holds no private project: a broker token
// an operator's entry mapped to one project (model.ListHoldsWrites), whose list held its writes
// and no read before projects could be private. It reads and writes as an open viewer then
// (access.Resolve), exactly as it did, and the writes it made are held to its project by the
// checks it was held by (writesHeldTo): a deployment's row is 403 PROJECT_SCOPE_DEPLOYMENT, a
// project's row whose object is of another project, of none, or of no row at all is 403
// PROJECT_SCOPE naming what the object resolved to (writeOutsideTheBrokerProject), a body's
// project outside it is 403 PROJECT_SCOPE (refuseOutsideCallerProjects), and a blocked_by target
// outside it is 403 PROJECT_SCOPE (writeBlockedBy), each in the words it was answered before (R9).
// Nothing is private then, so no refusal names an object a reader may not see, and no answer of a
// write it makes reads anything an open viewer would not; once a project is private it reads and
// writes within its list, and an object outside it answers as missing.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const (
	// codeProjectScope refuses a write whose body names a project the caller's credential does
	// not list: the issue create and the going request.
	codeProjectScope = "PROJECT_SCOPE"
	// codeProjectScopeDeployment refuses a viewer that is not open a write that belongs to the
	// deployment rather than to any one project.
	codeProjectScopeDeployment = "PROJECT_SCOPE_DEPLOYMENT"
	// codeSessionScope refuses a caller whose credential proved one session a write addressed
	// to another session's own rows.
	codeSessionScope = "SESSION_SCOPE"
	// codeSessionNotFound answers a write to a session the caller may not see as one nobody holds.
	codeSessionNotFound = "SESSION_NOT_FOUND"
	// codeRestrictedMember refuses a restricted member a write that belongs to the deployment.
	codeRestrictedMember = "RESTRICTED_MEMBER"
)

// brokerCredentialActor is the caller an access token the secrets broker issued decided, and
// whether this request presents one that resolved at all. Every other credential answers false
// without resolving anything: a browser cookie, a personal agent token, a Kubernetes
// service-account token and the shared token each prove no session, so a request presenting no
// DPoP-bound token is outside the session check. A credential that does not resolve is outside it
// too: its refusal is written further in, by the keyed layer for a keyed write and by the handler
// for any other.
func (s *server) brokerCredentialActor(r *http.Request) (model.Actor, bool) {
	if _, presented := brokertoken.Presented(r); !presented {
		return model.Actor{}, false
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, false
	}
	return actor, true
}

// callerProjects is the list of projects this request's writes are held to, sorted, and whether
// it lists any (writesHeldTo): a personal token's agent_tokens.projects, or the list of a token
// the secrets broker issued while that list holds the token's writes, each set from the credential
// and never from a body. A browser, the shared token and a service-account token list none, and
// neither does a token that sees every public project its person sees, nor a broker token acting
// as its person while no project is private.
func (s *server) callerProjects(r *http.Request) ([]string, bool) {
	reader, err := s.resolveReader(r)
	if err != nil {
		return nil, false
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return nil, false
	}
	held := writesHeldTo(reader.Viewer, actor.Credential)
	return held, len(held) > 0
}

// writesHeldTo is the list credential's writes are held to, given viewer, what access.Resolve
// answered it may read: the viewer's own list where Resolve held it to one, and, for a credential
// whose list holds its writes and no read while no project is private (model.ListHoldsWrites, a
// broker token an operator's entry mapped to one project), its list then too, as it was before
// projects could be private. Every other credential's writes are held to no list: the open viewer's
// of a credential that lists none, and a broker token's acting as its person while no project is
// private, whose list held nothing before.
func writesHeldTo(viewer *model.Viewer, credential *model.Credential) []string {
	switch {
	case viewer == nil || viewer.All:
		return nil
	case len(viewer.Lists) > 0:
		return viewer.Lists
	case credential != nil && credential.ListHoldsWhileNoPrivate == model.ListHoldsWrites && !viewer.AnyPrivate:
		return credential.Projects
	}
	return nil
}

// callerSession is the session this request's credential proved, and whether it proved one at
// all. A broker-issued token is the only credential that does: the broker binds it to one
// session and brokerTokenActor takes that session from the token, never from a body. Every
// other credential proves none — a cookie, a personal token, a Kubernetes service-account token
// and the shared token each leave the session to the body — and is outside this check.
func (s *server) callerSession(r *http.Request) (string, bool) {
	actor, resolved := s.brokerCredentialActor(r)
	if !resolved || actor.Kind != "session" || actor.ID == "" {
		return "", false
	}
	return actor.ID, true
}

// withinProjectScope runs in front of one mutating route's handler. It resolves the caller once
// (withCallerResolved) and the caller's viewer once (readerOf), and puts both in the request's
// context, where the handler and every loader it reads through find them (optionalActor,
// resolveReader), so a write reads each once. A row that writes nothing and reads no row
// (scopeNoWrite) needs neither and is returned unchanged.
//
// A row of the deployment (scopeDeployment) is refused to a viewer that is not open — a
// restricted member, or a credential that lists projects — as the read of the deployment's rows
// is (withinReadScope): what it writes belongs to no project such a viewer could be held to; and
// to a credential whose writes are held to a list though it reads open (writesHeldTo). The rows
// personsOwnAtTheBroker names end what is the caller's own at the secrets broker, so a restricted
// member makes them as a full member does, and only a credential that lists projects, or whose
// writes are held to a list, is refused them; the broker decides whose each grant or sign-in is, for
// every caller alike. Every other row reaches its handler, whose loaders answer an object the
// viewer does not see as missing (the file comment), but for a broker token scoped to one project
// while no project is private, whose write to an object outside its project is refused here as it
// was before (writeOutsideTheBrokerProject).
func (s *server) withinProjectScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if route.Scope == scopeNoWrite {
		return handler
	}
	deployment := route.Scope == scopeDeployment
	theirOwn := personsOwnAtTheBroker[route.Method+" "+route.Pattern]
	resolve, addressed := s.projectResolverFor(route.Pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		r = s.withCallerResolved(r)
		reader, ok := s.readerOf(w, r)
		if !ok {
			return
		}
		if deployment {
			held := writesHeldTo(reader.Viewer, resolvedCredential(r.Context()))
			ownWithNoList := theirOwn && len(reader.Viewer.Lists) == 0 && len(held) == 0
			if (!reader.Viewer.Open || len(held) > 0) && !ownWithNoList {
				refuseDeploymentWrite(w, route, reader.Viewer.Restricted && !theirOwn, held)
				return
			}
		}
		if covers, scoped := brokerProjectWhileNoPrivate(r.Context(), reader.Viewer); scoped && route.Scope == scopeProject {
			belongsTo := ""
			if addressed {
				var err error
				if belongsTo, err = resolve(r.Context(), s.deps.Store.Pool, r); err != nil {
					s.writeHandlerError(w, err)
					return
				}
			}
			if !addressed || belongsTo != covers {
				writeOutsideTheBrokerProject(w, covers, belongsTo)
				return
			}
		}
		handler(w, r.WithContext(withReader(r.Context(), reader)))
	}
}

// brokerProjectWhileNoPrivate is the one project the writes of ctx's caller are held to while it
// reads open, and whether they are: a broker token an operator's entry mapped to that project
// (model.ListHoldsWrites) while the deployment holds no private project, viewer being what it reads.
// Such a token's write of a project's object resolves the object's project before the handler, and
// its blocked_by targets theirs, as every such write did before projects could be private.
func brokerProjectWhileNoPrivate(ctx context.Context, viewer *model.Viewer) (string, bool) {
	if viewer == nil || len(viewer.Lists) > 0 {
		return "", false
	}
	held := writesHeldTo(viewer, resolvedCredential(ctx))
	if len(held) != 1 {
		return "", false
	}
	return held[0], true
}

// resolvedCredential is the credential of the caller withCallerResolved kept in ctx, and nil for a
// context that holds none or a caller that did not resolve.
func resolvedCredential(ctx context.Context) *model.Credential {
	resolved, ok := ctx.Value(resolvedCallerKey{}).(resolvedCaller)
	if !ok || resolved.err != nil {
		return nil
	}
	return resolved.actor.Credential
}

// withCallerResolved is r with its caller resolved once (optionalActor) and kept in its context,
// where every later layer and the handler read it rather than resolving it again. That keeps the
// access-log line naming the caller the handler names last (accesslog.Note: the last resolution
// wins), such as the session a shared-token write names in its body, rather than a layer's second
// resolution after the handler's.
func (s *server) withCallerResolved(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(resolvedCallerKey{}).(resolvedCaller); ok {
		return r
	}
	actor, human, err := s.optionalActor(r)
	return r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human, err: err}))
}

// refuseDeploymentWrite names the row and why the caller may not write it: a restricted member
// is 403 RESTRICTED_MEMBER, the refusal every write of the deployment a restricted member makes
// answers (creating a project or a group among them) but the revokes of their own grants and
// sign-ins (personsOwnAtTheBroker), and a credential whose writes are held to a list, held, is 403
// PROJECT_SCOPE_DEPLOYMENT, naming its list, which is the caller's own, in the words a broker
// token scoped to one project was answered before projects could be private (R9).
func refuseDeploymentWrite(w http.ResponseWriter, route apiRoute, restricted bool, held []string) {
	if restricted {
		writeError(w, codeRestrictedMember, http.StatusForbidden, fmt.Sprintf(
			"a restricted member cannot write %s %s, which belongs to the deployment rather than to a project", route.Method, route.Pattern))
		return
	}
	why := "this request's credential sees no project"
	if len(held) > 0 {
		why = "this request's credential may write " + projectsNamed(held) + " and nothing else"
	}
	writeError(w, codeProjectScopeDeployment, http.StatusForbidden, fmt.Sprintf(
		"%s; %s %s belongs to the deployment rather than to a project", why, route.Method, route.Pattern))
}

// projectsNamed is a credential's list as a refusal names it.
func projectsNamed(projects []string) string {
	if len(projects) == 1 {
		return "project " + projects[0]
	}
	return "projects " + strings.Join(projects, ", ")
}

// writeOutsideTheBrokerProject is the refusal of a write by a broker token scoped to project covers
// while the deployment holds no private project, to an object that belongs to project belongsTo,
// or that resolved to no project (""): an object of none, a row that is not there, or an id
// Postgres will not read. It names covers and the project the write belongs to where one resolved,
// in the words such a token was answered before projects could be private, and names no project
// for a write that resolved to none, since an invented one would say something about what exists.
func writeOutsideTheBrokerProject(w http.ResponseWriter, covers, belongsTo string) {
	if belongsTo == "" {
		writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
			"this request's credential may write project %s; this write resolves to no project of that credential's",
			covers))
		return
	}
	writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
		"this request's credential may write project %s; this write belongs to project %s", covers, belongsTo))
}

// projectResolver answers the project a request's addressed object belongs to. An empty project
// with no error means the address resolved to no project: an object that belongs to none, a
// target that does not exist, or an id Postgres will not read as a uuid.
type projectResolver func(ctx context.Context, q queryer, r *http.Request) (string, error)

// projectResolverFor is the resolver a project's row calls for its object, read from the shape of
// its pattern, and whether the pattern names an object it knows: the read a broker token scoped to
// one project has its write checked by while no project is private (writeOutsideTheBrokerProject).
// A row it knows no object of is refused to that token, so a row added later fails closed. Each
// resolver sends Postgres the value the prefix layer resolved, the one the handler behind it
// addresses its row by (resolveIDPrefixes, which register wraps outside this check), so the two
// cannot disagree about which object a path names. Each reads every project's rows: it runs only
// while no project is private.
func (s *server) projectResolverFor(pattern string) (projectResolver, bool) {
	switch {
	case addresses(pattern, "/api/v1/issues/{key}"):
		return projectOfRow(`select project_key from issues where key = $1`, "key"), true
	case addresses(pattern, "/api/v1/projects/{key}"):
		return projectOfRow(`select key from projects where key = $1`, "key"), true
	case addresses(pattern, "/api/v1/artifacts/{id}"):
		return projectOfRow(`select coalesce(project_key, '') from artifacts where id = $1`, "id"), true
	case addresses(pattern, "/api/v1/asks/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, ar.project_key, '')
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where a.id = $1`, "id"), true
	case addresses(pattern, "/api/v1/comments/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, ar.project_key, '')
			from comments c
			left join issues i on i.key = c.issue_key
			left join artifacts ar on ar.id = c.artifact_id
			where c.id = $1`, "id"), true
	case addresses(pattern, "/api/v1/messages/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, '')
			from messages m
			left join issues i on i.key = m.issue_key
			where m.id = $1`, "id"), true
	case addresses(pattern, "/api/v1/results/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, '')
			from check_results c
			left join issues i on i.key = c.issue_key
			where c.id = $1`, "id"), true
	}
	return nil, false
}

// addresses answers whether pattern addresses the object prefix names: that object itself, or
// anything under it.
func addresses(pattern, prefix string) bool {
	return pattern == prefix || strings.HasPrefix(pattern, prefix+"/")
}

// projectOfRow is the resolver that reads one column, the object's project, by the path value
// named parameter, answering no project for a target that does not resolve rather than an error:
// no such row, or a path value Postgres will not read as a uuid (SQLSTATE 22P02, which it reports
// as it binds the parameter).
func projectOfRow(sql, parameter string) projectResolver {
	return func(ctx context.Context, q queryer, r *http.Request) (string, error) {
		var project string
		if err := q.QueryRow(ctx, sql, r.PathValue(parameter)).Scan(&project); err != nil {
			var malformed *pgconn.PgError
			if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &malformed) && malformed.Code == "22P02") {
				return "", nil
			}
			return "", err
		}
		return project, nil
	}
}

// refuseOutsideCallerProjects refuses a write whose body names a project the request's
// credential does not list, when it lists any, and answers whether it did; an empty project is a
// write over every project, which is outside any list. It names the list, which is the caller's
// own, and the project the body named, which the caller wrote; it reads nothing, so it says
// nothing of whether that project exists.
func (s *server) refuseOutsideCallerProjects(w http.ResponseWriter, r *http.Request, project string) bool {
	listed, ok := s.callerProjects(r)
	if !ok || slices.Contains(listed, project) {
		return false
	}
	belongsTo := "belongs to project " + project
	if project == "" {
		belongsTo = "is over every project"
	}
	writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
		"this request's credential may write %s; this write %s", projectsNamed(listed), belongsTo))
	return true
}

// withinSessionScope runs the session check in front of one mutating route's handler. A row
// classed scopeSession whose pattern addresses a session by id belongs to that session alone,
// so a caller whose credential proved a session — a broker-issued token, the one credential
// that proves one — may address only its own. Every other credential proves no session and
// names one in the body where a route needs it, so the check leaves it to the handler, which is
// the rule those routes have always kept.
//
// A row that declares OAuth scopes passes whatever session the credential proved, because those
// rows are the person's own writes into someone else's conversation:
// `POST /api/v1/agents/{session_id}/messages` and `PUT /api/v1/me/agents/{session_id}/state`
// need the `person` scope (routeScopes), and a person writing to a session is by design writing
// into a session that is not theirs. Without this a person's own broker-issued credential could
// only ever address the session it was issued for, which is nobody's conversation but its own.
// The row decides it, not the request: gateRouteScopes stands ahead of this wrapper and has
// already refused every credential that does not hold the row's whole set, so on a row that
// declares any, the caller holds them. A row that declares none is open to every authenticated
// caller, and this check refuses a session other than the one it proved as it always did.
//
// The session itself is read as the caller sees it (access.SessionClause, sessionHiddenFrom): a
// caller that is not open - a restricted member, or a credential that lists projects - is answered
// 404 SESSION_NOT_FOUND for a session that is neither its own nor has acted in a project it sees,
// and an open viewer for a session that has acted in a project it does not see, exactly as for a
// session nobody holds. An open viewer of a deployment that holds no private project is answered
// as before and costs no read.
//
// Every other row returns the handler unchanged, so no route it does not cover pays for it.
func (s *server) withinSessionScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if !addressesOneSession(route) {
		return handler
	}
	personsOwnWrite := len(route.OAuthScopes) > 0
	return func(w http.ResponseWriter, r *http.Request) {
		r = s.withCallerResolved(r)
		addressed := r.PathValue("session_id")
		if proved, provedOne := s.callerSession(r); provedOne && addressed != proved && !personsOwnWrite {
			writeError(w, codeSessionScope, http.StatusForbidden, fmt.Sprintf(
				"this request's credential acts for session %s; %s %s writes into session %s",
				proved, route.Method, route.Pattern, addressed))
			return
		}
		reader, ok := s.readerOf(w, r)
		if !ok {
			return
		}
		r = r.WithContext(withReader(r.Context(), reader))
		hidden, err := s.sessionHiddenFromReader(r.Context(), addressed)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if hidden {
			writeError(w, codeSessionNotFound, http.StatusNotFound, "no session "+addressed)
			return
		}
		handler(w, r)
	}
}

// sessionHiddenFromReader reports whether the request's reader - the one a check put in ctx
// (withReader) - may not see session id (sessionHiddenFrom, access.SessionClause): a session that
// is not its own and has acted in a project it does not see, or, for a restricted member's or a
// listing credential's reader, in none it sees. Work with no reader in ctx, which the server does
// for itself, sees every session.
func (s *server) sessionHiddenFromReader(ctx context.Context, id string) (bool, error) {
	reader, ok := readerFromContext(ctx)
	if !ok {
		return false, nil
	}
	return s.sessionHiddenFrom(ctx, reader, id)
}

// personHiddenFromReader reports, through q, whether the request's reader may not see the person
// login names (access.PersonClause), as sessionHiddenFromReader does for a session.
func personHiddenFromReader(ctx context.Context, q queryer, login string) (bool, error) {
	reader, ok := readerFromContext(ctx)
	if !ok || reader.Viewer == nil || reader.Viewer.Open {
		return false, nil
	}
	var sees bool
	err := q.QueryRow(ctx, `select `+access.PersonClause("$1::text", "$2"), model.CanonicalLogin(login), refs.VisibilityArg(reader)).Scan(&sees)
	return !sees, err
}

// addressesOneSession answers whether route is a write to the rows of the one session its path
// names: the scopeSession rows whose pattern carries {session_id}. The other scopeSession rows
// address no session of their own — the `me` token rows are the calling person's, and the two
// message rows are held to the session their delivery attempt names.
func addressesOneSession(route apiRoute) bool {
	return route.Scope == scopeSession && strings.Contains(route.Pattern, "{session_id}")
}

// writeNeedsASignedInPerson refuses every write by a caller whose credential names a service and
// no person: an access token the secrets broker issued to a machine nobody has signed in on. The
// refusal is here, in front of every mutating row whatever its class, rather than in each route,
// for the reason the two checks above are: a route added later is covered by the table it joins
// and not by a line someone remembered to write.
//
// Such a caller may read everything its session may read. What it may not do is write, because
// Dispatch records a write under the person who made it and this credential names none; a
// session that accepted a delivery or answered an ask under it would write as nobody. The
// refusal's message names `agent-secrets login`, the command that signs a person in on that
// machine, since running it is the whole remedy.
//
// A read-only token names a service and no person too, but it is no such caller: it reads only
// what the read list names, and signing a person in is no remedy for it. It never reaches this
// check, since the read-only gate register puts outside it has already answered it 403
// READ_ONLY_TOKEN (refuseReadOnlyToken).
//
// Every other credential is unaffected: a cookie, a personal token, a service-account token, the
// shared token and a broker token that does name a person each reach the handler as before, at
// the cost of the one header read brokerCredentialActor makes.
func (s *server) writeNeedsASignedInPerson(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, resolved := s.brokerCredentialActor(r)
		if !resolved || actor.Owner != nil || actor.Service == nil {
			handler(w, r)
			return
		}
		writeError(w, contracts.DispatchSignInRequired, http.StatusForbidden, fmt.Sprintf(
			"this request's credential acts for service %s and no person, so there is nobody to "+
				"record this write under; run `agent-secrets login` on this machine and try again",
			*actor.Service))
	}
}

// undecidableWrites names every mutating row that nothing decides: one writeClasses gives no
// class, and one classed scopeProject whose address names no object writeProbes probes, which
// TestNoWriteReachesAHiddenObject therefore cannot hold to answering a hidden object as missing.
// Every other class is decided: scopeDeployment by the open viewer it needs, scopeProjectInBody
// by its handler's check of the body's project, scopeSession by withinSessionScope and the
// handlers behind it, and scopeNoWrite by writing nothing at all.
// TestEveryMutatingRouteIsClassed walks the table through it, and register refuses to mount a
// table it names anything in, so a row added later cannot be left undecided. The other direction,
// a writeClasses name that is no row of the table, is the same test's (nothing is served by it,
// so it cannot admit a write).
func (s *server) undecidableWrites(routes []apiRoute) []string {
	var undecidable []string
	for _, route := range routes {
		if route.Method == http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		switch route.Scope {
		case scopeSession, scopeDeployment, scopeProjectInBody, scopeNoWrite:
		case scopeProject:
			if _, probed := writeProbes[name]; !probed {
				undecidable = append(undecidable, name+": classed as a project's write, but its address names no object writeProbes probes")
			}
		default:
			undecidable = append(undecidable, name+": no write class (writeClasses)")
		}
	}
	return undecidable
}
