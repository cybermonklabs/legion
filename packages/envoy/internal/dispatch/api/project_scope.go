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
// the rule. What a credential's list decides is access.CredentialCovers': a full member's list
// decides only which private projects it reaches, so its write to a public project's object reaches
// the handler whatever it lists, and its write to a private project it does not list is one to an
// object it does not see; a restricted member's list narrows every project it reaches.
// TestNoWriteReachesAHiddenObject calls every write row that names an object, on PRIV's object and
// on one nobody holds, and holds the two to one answer; undecidableWrites refuses to mount a
// project's write that walk has no probe for.
//
// A read-only token, which carries `project_scope` `none` and the scope `read` alone and names a
// service and no person, never reaches these checks: the read-only gate register puts outside
// them answers that token 403 READ_ONLY_TOKEN on every route the read list does not name, every
// write among them (refuseReadOnlyToken, readScopeRoutes).
//
// A target a credential's list does not cover is the handler's missing object, so no refusal names
// the project an object it cannot see belongs to. The two writes that take their project from the
// body rather than the path, the issue create and the going request (scopeProjectInBody), refuse
// by name a project the list does not cover (refuseOutsideCallerProjects), since the name is the
// caller's own, and answer a project the caller does not see as they answer one nobody holds.
//
// One credential's writes are held further, whatever the deployment holds: a full member's broker
// token an operator's entry mapped to one project (model.Credential.NamedProject), whose writes
// were held to that project before projects could be private, and still are (namedProjectOf; a
// later change of that scope is not this file's). It reads as its list lets it, but a deployment's
// row is 403 PROJECT_SCOPE_DEPLOYMENT, a project's row whose object is of another project, of none,
// or of no row at all is 403 PROJECT_SCOPE naming what the object resolved to
// (writeOutsideTheBrokerProject), a body's project outside it is 403 PROJECT_SCOPE
// (refuseOutsideCallerProjects), and a blocked_by target outside it is 403 PROJECT_SCOPE
// (writeBlockedBy), each in the words it was answered before (R9). An object of a project the
// token does not see resolves to no project, so that refusal answers it exactly as one nobody
// holds, and no refusal names a project the token may not see. A restricted member's such token is
// held by its list instead, which covers that project alone, and an object outside it answers as
// missing.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/ciphertoken"
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
	// codeRestrictedMember refuses a restricted member a write that belongs to the deployment, and
	// an issue write that names repo (refuseRepoOfRestrictedMember).
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
	if _, presented := ciphertoken.Presented(r); !presented {
		return model.Actor{}, false
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, false
	}
	return actor, true
}

// listOf is the request's credential as access.CredentialCovers reads it: its list
// (model.Credential.Projects), whether its person is a restricted member, which reader's viewer
// says as access.Resolve answered it, and whether it is the person's own browser, which reader's
// kind and act say as access.Resolve reads them. A nil credential lists none.
func listOf(reader model.Actor, credential *model.Credential) access.Credential {
	cred := access.Credential{
		Browser:    reader.Kind == "user" && reader.Act == nil,
		Restricted: reader.Viewer != nil && reader.Viewer.Restricted,
	}
	if credential != nil {
		cred.Listed = credential.Projects
	}
	return cred
}

// writesHeldTo is the list of projects that holds reader's writes, sorted: the one project a full
// member's broker token scoped to one project writes (namedProjectOf); otherwise the credential's
// own list, which holds it to no row of the whole deployment (withinProjectScope): a list decides
// which private projects a full member's credential reads, and a write of the whole deployment
// stays a person's or an unlisted credential's. That holds in every state for a personal token's
// list, which is new with private projects, and for a restricted member's; a broker token acting as
// its person carries the list its trade asked for, which limited no write before projects could be
// private, so it holds that token's writes only once a project is private (R9). A list holds no
// write of a project's object by name: what it covers (access.CredentialCovers) is what the reads a
// write makes find.
func writesHeldTo(reader model.Actor, credential *model.Credential) []string {
	if project, named := namedProjectOf(reader.Viewer, credential); named {
		return []string{project}
	}
	viewer := reader.Viewer
	if viewer == nil || viewer.All || credential == nil || len(credential.Projects) == 0 {
		return nil
	}
	if credential.Session != "" && !viewer.AnyPrivate && !viewer.Restricted {
		return nil
	}
	return credential.Projects
}

// namedProjectOf is the one project a full member's broker token an operator's entry mapped to
// one project may write, and whether credential is such a token (model.Credential.NamedProject),
// viewer being what access.Resolve answered it may read. Its writes were held to that project
// before projects could be private, and still are, whatever the deployment holds (R9): its write
// of a project's object resolves the object's project before the handler (withinProjectScope), and
// its body's project and its blocked_by targets are held to it (refuseOutsideCallerProjects,
// writeBlockedBy). A restricted member's such token is held by its list instead, which covers that
// project alone, so an object outside it is one its reads do not find.
func namedProjectOf(viewer *model.Viewer, credential *model.Credential) (string, bool) {
	if credential == nil || credential.NamedProject == "" || viewer == nil || viewer.All || viewer.Restricted {
		return "", false
	}
	return credential.NamedProject, true
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
// restricted member — as the read of the deployment's rows is (withinReadScope): what it writes
// belongs to no project such a viewer could be held to; and to a credential whose writes a list
// holds (writesHeldTo), a full member's included, which reads those rows as an open viewer but does
// not write them. The rows personsOwnAtTheBroker names end what is the caller's own at the secrets
// broker, so a restricted member makes them as a full member does, and only a credential whose
// writes a list holds is refused them; whose each sign-in is the broker decides, and whose each
// grant the broker decides for a full member and the handler for a restricted member
// (revokeCredentialGrant, memberGrants). The rows that decide, list or end a machine login
// (restrictedOwnLoginRoutes) are a restricted member's at a browser alone
// (restrictedOwnLoginAtBrowser, the rule the read gate reads too), whose handlers hold that member
// to a login of their own: never their personal token and never a broker token, so no agent of
// theirs decides or ends a credential; any other call of theirs there is refused
// restrictedOwnLoginRefusal. Every other row reaches its handler, whose loaders answer an object
// the viewer does not see as missing (the file comment), but for a full member's broker token
// scoped to one project, whose write to an object outside its project is refused here as it was
// before (writeOutsideTheBrokerProject).
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
		credential := resolvedCredential(r.Context())
		covers, named := namedProjectOf(reader.Viewer, credential)
		if deployment {
			governed, allowed := restrictedOwnLoginAtBrowser(route, reader, r)
			if governed && !allowed {
				restrictedOwnLoginRefusal(w, route)
				return
			}
			held := writesHeldTo(reader, credential)
			if (reader.Viewer.Restricted || len(held) > 0) && !(theirOwn && len(held) == 0) && !allowed {
				refuseDeploymentWrite(w, route, reader.Viewer.Restricted && !theirOwn, held)
				return
			}
		}
		if named && route.Scope == scopeProject {
			belongsTo := ""
			if addressed {
				var err error
				if belongsTo, err = resolve(r.Context(), s.deps.Store.Pool, r, refs.VisibilityArg(reader)); err != nil {
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

// atBrowser reports whether the caller withCallerResolved resolved is a person at a browser, whose
// sign-in proved them, rather than any credential an agent holds.
func atBrowser(r *http.Request) bool {
	caller, ok := r.Context().Value(resolvedCallerKey{}).(resolvedCaller)
	return ok && caller.err == nil && caller.human
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
// sign-ins (personsOwnAtTheBroker), and a credential whose writes a list holds, held, is 403
// PROJECT_SCOPE_DEPLOYMENT, naming its list, which is the caller's own, in the words a broker
// token scoped to one project was answered before projects could be private (R9).
func refuseDeploymentWrite(w http.ResponseWriter, route apiRoute, restricted bool, held []string) {
	if restricted {
		writeError(w, codeRestrictedMember, http.StatusForbidden, fmt.Sprintf(
			"a restricted member cannot write %s %s, which belongs to the deployment rather than to a project", route.Method, route.Pattern))
		return
	}
	writeError(w, codeProjectScopeDeployment, http.StatusForbidden, fmt.Sprintf(
		"this request's credential may write %s and nothing else; %s %s belongs to the deployment rather than to a project",
		projectsNamed(held), route.Method, route.Pattern))
}

// projectsNamed is a credential's list as a refusal names it.
func projectsNamed(projects []string) string {
	if len(projects) == 1 {
		return "project " + projects[0]
	}
	return "projects " + strings.Join(projects, ", ")
}

// writeOutsideTheBrokerProject is the refusal of a write by a full member's broker token scoped to
// project covers (namedProjectOf), to an object that belongs to project belongsTo, or that resolved
// to no project (""): an object of none, a row that is not there, an object of a project the token
// does not see, or an id Postgres will not read. It names covers and the project the write belongs
// to where one resolved, in the words such a token was answered before projects could be private,
// and names no project for a write that resolved to none, since an invented one would say
// something about what exists.
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

// projectResolver answers the project a request's addressed object belongs to, as the reader
// whose visibility argument is viewer sees it. An empty project with no error means the address
// resolved to no project: an object that belongs to none, an object of a project the reader does
// not see, a target that does not exist, or an id Postgres will not read as a uuid.
type projectResolver func(ctx context.Context, q queryer, r *http.Request, viewer any) (string, error)

// projectResolverFor is the resolver a project's row calls for its object, read from the shape of
// its pattern, and whether the pattern names an object it knows: the read a full member's broker
// token scoped to one project has its write checked by (writeOutsideTheBrokerProject). A row it
// knows no object of is refused to that token, so a row added later fails closed. Each resolver
// sends Postgres the value the prefix layer resolved, the one the handler behind it addresses its
// row by (resolveIDPrefixes, which register wraps outside this check), so the two cannot disagree
// about which object a path names. Each binds the project clause (access.ProjectClause) over the
// object's project, so an object of a project the reader does not see is a row it does not find,
// read by the very statement an id nobody holds is: the refusal says nothing of it.
func (s *server) projectResolverFor(pattern string) (projectResolver, bool) {
	switch {
	case addresses(pattern, "/api/v1/issues/{key}"):
		return projectOfRow(`select project_key from issues where key = $1 and ($2::jsonb is not null or true)`, "key"), true
	case addresses(pattern, "/api/v1/projects/{key}"):
		return projectOfRow(`select key from projects where key = $1 and ($2::jsonb is not null or true)`, "key"), true
	case addresses(pattern, "/api/v1/artifacts/{id}"):
		return projectOfRow(`select coalesce(project_key, '') from artifacts where id = $1 and ($2::jsonb is not null or true)`, "id"), true
	case addresses(pattern, "/api/v1/asks/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, ar.project_key, '')
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where a.id = $1 and ($2::jsonb is not null or true)`, "id"), true
	case addresses(pattern, "/api/v1/comments/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, ar.project_key, '')
			from comments c
			left join issues i on i.key = c.issue_key
			left join artifacts ar on ar.id = c.artifact_id
			where c.id = $1 and ($2::jsonb is not null or true)`, "id"), true
	case addresses(pattern, "/api/v1/messages/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, '')
			from messages m
			left join issues i on i.key = m.issue_key
			where m.id = $1 and ($2::jsonb is not null or true)`, "id"), true
	case addresses(pattern, "/api/v1/results/{id}"):
		return projectOfRow(`
			select coalesce(i.project_key, '')
			from check_results c
			left join issues i on i.key = c.issue_key
			where c.id = $1 and ($2::jsonb is not null or true)`, "id"), true
	}
	return nil, false
}

// addresses answers whether pattern addresses the object prefix names: that object itself, or
// anything under it.
func addresses(pattern, prefix string) bool {
	return pattern == prefix || strings.HasPrefix(pattern, prefix+"/")
}

// projectOfRow is the resolver that reads one column, the object's project, by the path value
// named parameter and the reader's visibility argument at $2, answering no project for a target
// that does not resolve rather than an error: no such row, a row the reader does not see, or a path
// value Postgres will not read as a uuid (SQLSTATE 22P02, which it reports as it binds the
// parameter).
func projectOfRow(sql, parameter string) projectResolver {
	return func(ctx context.Context, q queryer, r *http.Request, viewer any) (string, error) {
		var project string
		if err := q.QueryRow(ctx, sql, r.PathValue(parameter), viewer).Scan(&project); err != nil {
			var malformed *pgconn.PgError
			if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &malformed) && malformed.Code == "22P02") {
				return "", nil
			}
			return "", err
		}
		return project, nil
	}
}

// refuseOutsideCallerProjects refuses a write whose body names project, which the request's
// credential may not write, and answers whether it did. A full member's broker token scoped to
// one project may write that project alone (namedProjectOf), and is refused any other, an empty
// project, a write over every project, included, in the words it was answered before projects
// could be private. Any other credential that lists projects is refused a project its list does
// not cover (access.CredentialCovers), a project its reader does not see read as a private one, so
// a project a hidden project's key names and one no project holds are refused alike; an empty
// project is a write of no one project, which only a restricted member's list does not cover. A
// credential that lists none is refused nothing here, and a project it does not see is the
// handler's missing project. The refusal names the list or the one project, which is the caller's
// own, and the project the body named, which the caller wrote, so it says nothing of whether that
// project exists.
func (s *server) refuseOutsideCallerProjects(w http.ResponseWriter, r *http.Request, project string) bool {
	reader, err := s.resolveReader(r)
	if err != nil {
		return false
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return false
	}
	if covers, named := namedProjectOf(reader.Viewer, actor.Credential); named {
		if project == covers {
			return false
		}
		writeOutsideTheBrokerProject(w, covers, project)
		return true
	}
	cred := listOf(reader, actor.Credential)
	if len(cred.Listed) == 0 {
		return false
	}
	private := project != "" && reader.Viewer != nil && reader.Viewer.AnyPrivate && !slices.Contains(reader.Viewer.Sees, project)
	if access.CredentialCovers(cred, project, private) {
		return false
	}
	belongsTo := "belongs to project " + project
	if project == "" {
		belongsTo = "is over every project"
	}
	writeError(w, codeProjectScope, http.StatusForbidden, fmt.Sprintf(
		"this request's credential may write %s; this write %s", projectsNamed(cred.Listed), belongsTo))
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
// caller that is not open - a restricted member - is answered
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
// is not its own and has acted in a project it does not see, or, for a restricted member's
// reader, in none it sees. A context with no reader is answered hidden,
// failing closed: every request's check puts its reader in ctx, and the server's own work names
// the server's reader (everyReader) where it reads every session, as the delivery's resolve of a
// session route does (resolveDeliveryTarget).
func (s *server) sessionHiddenFromReader(ctx context.Context, id string) (bool, error) {
	reader, ok := readerFromContext(ctx)
	if !ok {
		return true, nil
	}
	return s.sessionHiddenFrom(ctx, reader, id)
}

// personHiddenFromReader reports, through q, whether the request's reader may not see the person
// login names (access.PersonClause), as sessionHiddenFromReader does for a session: a context with
// no reader is answered hidden, and a reader with no viewer reads as the closed viewer it binds
// (refs.VisibilityArg). An open reader sees every person and costs no read.
func personHiddenFromReader(ctx context.Context, q queryer, login string) (bool, error) {
	reader, ok := readerFromContext(ctx)
	if !ok {
		return true, nil
	}
	if reader.Viewer != nil && reader.Viewer.Open {
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
