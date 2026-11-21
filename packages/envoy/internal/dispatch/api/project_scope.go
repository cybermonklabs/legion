package api

// project_scope.go answers what a write is about to touch — which project does the object it
// addresses belong to, and which session does its path name? — and refuses a caller whose
// credential covers another project, or proves another session. owner.go is not this: it
// resolves collaboration ownership, the issue or unlinked document a comment, an ask or an
// event hangs from and whether that owner is still open, and decides no authority at all.
// These resolvers decide authority.
//
// Each check runs in one place — withinProjectScope and withinSessionScope, which register
// wraps around every mutating route after the caller is resolved and before the handler —
// rather than in each of the seventy routes that write. Two things make a pre-handler read
// sound, and both were read out of the tree rather than assumed:
//
//   - Nothing moves an object between projects. An issue's, an artifact's, an ask's, a comment's
//     and a message's project is written when the row is inserted and updated by no route: no
//     `update ... set project_key` and no `update ... set issue_key` exists outside the
//     migrations and the tests, and the three writes that take another issue's project into
//     account move nothing: a parent and a rank neighbour outside the issue's own project are
//     refused, while a `blocked_by` target may be an issue of any project, which the write
//     refuses only to a caller whose credential names one project (`PROJECT_SCOPE`, written
//     inside the write because this check reads the project of the object the path names and no
//     key in the body). So the project this check reads before the handler runs is the project
//     the handler will write, and the check needs no transaction of its own.
//   - The check is a no-op for a credential that carries no project: a browser cookie, a personal
//     agent token, a Kubernetes service-account token and the shared token name none, and so do
//     four access tokens the broker issues, each of which reaches brokerTokenActor with no
//     Project. Two of them the broker scopes to a person rather than to one project, and both
//     carry `project_scope` `person`: a dev box session's, unscoped here with no second mechanism
//     of its own, and a machine's that names a service and no person, whose every write
//     writeNeedsASignedInPerson refuses below. The third is the `named` token of an account
//     entry that names no project (`<service account>==<person>`, the shared public-session
//     account's), which names its person and writes wherever Dispatch's own rules let it
//     (brokerTokenActor). The fourth is a read-only token, which carries `project_scope` `none`
//     and the scope `read` alone, names a service and no person, and never reaches this check:
//     the read-only gate register puts outside it answers that token
//     403 READ_ONLY_TOKEN on every route the read list does not name, every write among them
//     (refuseReadOnlyToken, readScopeRoutes). That is the rule and not a transitional measure:
//     every caller that exists today writes exactly what it wrote before, which is what lets
//     this deploy before anything presents a broker token.
//
// For a credential that does name a project, the check fails closed: only a target that
// resolves to that credential's own project reaches the handler. A target that resolves to
// another project, to no project at all, or to nothing at all — no such row, or an id Postgres
// will not read as a uuid — is refused. Each resolver sends Postgres the value the prefix layer
// resolved, the one the handler behind it sends too: an id written as a unique prefix is the
// whole id by the time either reads it, and anything else is the path value as written
// (resolveIDPrefixes, which register wraps outside this check for exactly that reason). So the
// parser that decides what this check resolved is the parser the handler then addresses its row
// by, and the two cannot disagree about which object a path names: Go's uuid.Parse refuses
// spellings Postgres accepts (a hyphen after any group of four digits), and a check that
// resolved nothing where the handler resolves a row would be a write nothing judged.
//
// Failing closed does not close the existence oracle, and does not claim to. The two refusals
// still read differently — a target of another project is told which project that is, and one
// that resolved to nothing is told the write resolves to no project of that credential's — so a
// scoped caller can tell an object of another project from an id nobody holds. Nothing is read
// out that way: no GET route is project-scoped, so every answer this check can colour is a
// refusal of the caller's own write, and what it learns is which of its own refused writes
// addressed something that exists.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// brokerCredentialActor is the caller an access token the secrets broker issued decided, and
// whether this request presents one that resolved at all. Every other credential answers false
// without resolving anything: a browser cookie, a personal agent token, a Kubernetes
// service-account token and the shared token each name no project and prove no session, so a
// request presenting no DPoP-bound token is outside both checks. A credential that does not
// resolve is outside them too: its refusal is written further in, by the keyed layer for a keyed
// write and by the handler for any other.
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

// callerProject is the one project this request's credential lets the caller write, and whether
// it names one at all. Only an access token the secrets broker issued for a named project
// carries one (model.Actor.Project, set by brokerTokenActor from the token and never from a
// body).
func (s *server) callerProject(r *http.Request) (string, bool) {
	actor, resolved := s.brokerCredentialActor(r)
	if !resolved || actor.Project == nil || *actor.Project == "" {
		return "", false
	}
	return *actor.Project, true
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

// projectResolver answers the project a request's addressed object belongs to. An empty project
// with no error means the address resolved to no project: an object that belongs to none, a
// target that does not exist, or an id Postgres will not read as a uuid.
type projectResolver func(ctx context.Context, q queryer, r *http.Request) (string, error)

// resolverOf is the resolver of the object a route's path names (routeRow.ProjectOf), and whether
// it names one at all. Every row the table classes scopeProject names one; undecidableWrites
// holds the table to that and register refuses to mount a table that breaks it, so a mounted
// scopeProject row always resolves.
func (s *server) resolverOf(of projectPath) (projectResolver, bool) {
	switch of {
	case projectPathIssue:
		return s.projectOfIssue, true
	case projectPathProject:
		return s.projectOfProject, true
	case projectPathArtifact:
		return s.projectOfArtifact, true
	case projectPathAsk:
		return s.projectOfAsk, true
	case projectPathComment:
		return s.projectOfComment, true
	case projectPathMessage:
		return s.projectOfMessage, true
	case projectPathResult:
		return s.projectOfCheckResult, true
	}
	return nil, false
}

// projectPathOf is the object a /api/v1 pattern names, read from its shape, which routes() copies
// onto a v1 route's ProjectOf column (v1Route): the issue of /api/v1/issues/{key}, the project of
// /api/v1/projects/{key}, and the artifact, ask, comment, message or check result of
// /api/v1/<kind>/{id}, each with anything under it.
func projectPathOf(pattern string) projectPath {
	switch {
	case addresses(pattern, "/api/v1/issues/{key}"):
		return projectPathIssue
	case addresses(pattern, "/api/v1/projects/{key}"):
		return projectPathProject
	case addresses(pattern, "/api/v1/artifacts/{id}"):
		return projectPathArtifact
	case addresses(pattern, "/api/v1/asks/{id}"):
		return projectPathAsk
	case addresses(pattern, "/api/v1/comments/{id}"):
		return projectPathComment
	case addresses(pattern, "/api/v1/messages/{id}"):
		return projectPathMessage
	case addresses(pattern, "/api/v1/results/{id}"):
		return projectPathResult
	}
	return projectPathNone
}

// addresses answers whether pattern addresses the object prefix names: that object itself, or
// anything under it.
func addresses(pattern, prefix string) bool {
	return pattern == prefix || strings.HasPrefix(pattern, prefix+"/")
}

// projectOfIssue reads the project column of the issue the path names, the same one-row read
// every handler that needs an issue's project makes (issue_patch.go, issue_parent.go).
func (s *server) projectOfIssue(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `select project_key from issues where key = $1`, r.PathValue("key")))
}

// projectOfProject is the one-row read the other resolvers make, against the projects table
// itself: a project's own key is the project it belongs to, and a key no project has resolves
// to none. It reads the key alone rather than going through loadProject, whose 404 this would
// only have to undo.
func (s *server) projectOfProject(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `select key from projects where key = $1`, r.PathValue("key")))
}

// projectOfArtifact reads the project_key every artifact carries (artifactColumns), which is
// its issue's for an issue document and its own for a project document. An agent conversation's
// artifact carries none, so it resolves to no project and a project-scoped caller is refused
// it.
func (s *server) projectOfArtifact(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(project_key, '') from artifacts where id = $1
	`, r.PathValue("id")))
}

// projectOfAsk resolves an ask through the join askSnoozeEvent already makes: an ask hangs from
// an issue or from a document, and takes that owner's project.
func (s *server) projectOfAsk(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from asks a
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where a.id = $1
	`, r.PathValue("id")))
}

// projectOfComment is one hop to the comment's issue or document, since the comments table holds
// no project of its own (store/migrations/0001_init.up.sql, 0009_project_artifacts.up.sql).
func (s *server) projectOfComment(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts ar on ar.id = c.artifact_id
		where c.id = $1
	`, r.PathValue("id")))
}

// projectOfMessage is the same one hop for a message, which hangs from an issue or from nothing
// at all: a direct message between a person and a session belongs to no project, so it resolves
// to none here, and the two routes a session answers its own delivery on are classed
// scopeSession rather than this.
func (s *server) projectOfMessage(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, '')
		from messages m
		left join issues i on i.key = m.issue_key
		where m.id = $1
	`, r.PathValue("id")))
}

// projectOfCheckResult is one hop to the project of the issue the result was recorded on,
// since the check_results table holds no project of its own
// (store/migrations/20261102122200_check_results.up.sql). It reads issue_key and not
// check_issue_key: a result recorded on a child against a parent's check belongs to the child,
// and both issues are of one project anyway, since an issue's parent is of its own project.
func (s *server) projectOfCheckResult(ctx context.Context, q queryer, r *http.Request) (string, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, '')
		from check_results c
		left join issues i on i.key = c.issue_key
		where c.id = $1
	`, r.PathValue("id")))
}

// scanProjectKey reads a resolver's one column, answering no project for a target that does not
// resolve rather than an error: no such row, or a path value Postgres will not read as a uuid
// (SQLSTATE 22P02, which it reports as it binds the parameter). Those are the ids a handler
// answers 404 or 400 for, and withinProjectScope refuses them to a project-scoped caller
// rather than letting that answer out.
func scanProjectKey(row pgx.Row) (string, error) {
	var project string
	if err := row.Scan(&project); err != nil {
		if unaddressed(err) {
			return "", nil
		}
		return "", err
	}
	return project, nil
}

// unaddressed answers whether err means the target resolves to no row at all, which is the
// handler's answer to make for an unscoped caller and a refusal for a scoped one. Every
// resolver reads its one column itself, so what reaches this is Postgres's own answer and never
// a refusal some loader already wrote a status onto.
func unaddressed(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var malformed *pgconn.PgError
	return errors.As(err, &malformed) && malformed.Code == "22P02"
}

// withinProjectScope runs the project check in front of one mutating route's handler. The row's
// class is decided once here, at mount, as withinSessionScope decides its own: a class this
// check does not cover returns the handler unchanged, so no route it leaves alone pays for it.
//
// For a caller whose credential names no project — every caller that exists today — a covered
// route costs one header read and calls the handler. For one that names a project it fails
// closed: only a target this check resolved to that same project reaches the handler.
func (s *server) withinProjectScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	switch route.Scope {
	case scopeProject:
		resolve, addressed := s.resolverOf(route.ProjectOf)
		if !addressed {
			// Nothing mounts such a row: undecidableWrites names it and register refuses the
			// table holding it. A project-scoped caller is refused it here all the same, so
			// what stands between that caller and a write nothing judged is this check rather
			// than a panic somewhere else.
			return func(w http.ResponseWriter, r *http.Request) {
				covers, scoped := s.callerProject(r)
				if !scoped {
					handler(w, r)
					return
				}
				refuseWriteOutsideProjectScope(w, r, covers, "")
			}
		}
		return func(w http.ResponseWriter, r *http.Request) {
			covers, scoped := s.callerProject(r)
			if !scoped {
				handler(w, r)
				return
			}
			belongsTo, err := resolve(r.Context(), s.deps.Store.Pool, r)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			if belongsTo != covers {
				// Including a target that resolved to no project at all: an object belonging
				// to none, a row that is not there, and an id Postgres will not read.
				// Refusing those too is what makes the check fail closed — not what hides
				// which ids exist, which this does not claim to do (the file comment).
				refuseWriteOutsideProjectScope(w, r, covers, belongsTo)
				return
			}
			handler(w, r)
		}
	case scopeDeployment:
		return func(w http.ResponseWriter, r *http.Request) {
			covers, scoped := s.callerProject(r)
			if !scoped {
				handler(w, r)
				return
			}
			writeError(w, r, apicode.ProjectScopeDeployment, fmt.Sprintf(
				"this request's credential may write project %s and nothing else; %s %s belongs to the deployment rather than to a project",
				covers, route.Method, route.Pattern))
		}
	default:
		// A write that belongs to the calling session (scopeSession, held to its own session by
		// withinSessionScope), the issue create, whose project its handler reads from the body
		// (scopeProjectInBody), and a row that writes nothing at all (scopeNoWrite).
		return handler
	}
}

// refuseWriteOutsideProjectScope names the project the caller's credential covers and the
// project the write belongs to where one resolved, and nothing of the credential itself. It
// names no project at all for a write that resolved to none, since there is none to name and
// an invented one would say something about what exists.
func refuseWriteOutsideProjectScope(w http.ResponseWriter, r *http.Request, covers, belongsTo string) {
	if belongsTo == "" {
		writeError(w, r, apicode.ProjectScope, fmt.Sprintf(
			"this request's credential may write project %s; this write resolves to no project of that credential's",
			covers))
		return
	}
	writeError(w, r, apicode.ProjectScope, fmt.Sprintf(
		"this request's credential may write project %s; this write belongs to project %s", covers, belongsTo))
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
// Every other row returns the handler unchanged, so no route it does not cover pays for it. A
// /api/v2 row whose path names a session for one kind of object alone, or whose body names
// several, is held by gateV2Authorities (sessionAddressing) to the session the credential proved,
// and there the row's OAuth scopes exempt nothing (refusesSessionOtherThanProved): the agent
// marks of PATCH /api/v2/me/marks/{kind}/{id} and PATCH /api/v2/me/marks need the person scope,
// and a broker token of one session that holds it is still refused 403 SESSION_SCOPE for another
// session's agent mark (c-authority of dispatch://INFRA-4071).
func (s *server) withinSessionScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if !addressesOneSession(route) {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.refusesAnotherSession(w, r, route, r.PathValue(route.SessionPath)) {
			return
		}
		handler(w, r)
	}
}

// refusesAnotherSession holds one session a route's write goes into, addressed, to the rule
// withinSessionScope's comment gives: on a row that declares no OAuth scopes it refuses as
// refusesSessionOtherThanProved does, and on a row that declares any it writes nothing and
// reports false.
func (s *server) refusesAnotherSession(w http.ResponseWriter, r *http.Request, route apiRoute, addressed string) bool {
	return len(route.OAuthScopes) == 0 && s.refusesSessionOtherThanProved(w, r, route, addressed)
}

// refusesSessionOtherThanProved holds one session a route's write goes into, addressed, to the
// session the caller's credential proved, whatever OAuth scopes the row declares: it answers 403
// SESSION_SCOPE and reports true when the credential proved a session other than addressed, and
// otherwise writes nothing and reports false. A credential that proves no session (callerSession)
// is held to none. gateV2Authorities holds each session a /api/v2 body names to it
// (sessionAddressing), and refusesAnotherSession holds a path's session to it on a row that
// declares no OAuth scopes.
func (s *server) refusesSessionOtherThanProved(w http.ResponseWriter, r *http.Request, route apiRoute, addressed string) bool {
	proved, provedOne := s.callerSession(r)
	if !provedOne || addressed == proved {
		return false
	}
	writeError(w, r, apicode.SessionScope, fmt.Sprintf(
		"this request's credential acts for session %s; %s %s writes into session %s",
		proved, route.Method, route.Pattern, addressed))
	return true
}

// addressesOneSession answers whether route is a write to the rows of the one session its path
// names: the scopeSession rows whose path names a session (routeRow.SessionPath; a v1 route's
// {session_id}). The other scopeSession rows address no session of their own — the `me` token rows
// are the calling person's, and the two message rows are held to the session their delivery
// attempt names.
func addressesOneSession(route apiRoute) bool {
	return route.Scope == scopeSession && route.SessionPath != ""
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
		writeError(w, r, apicode.SignInRequired, fmt.Sprintf(
			"this request's credential acts for service %s and no person, so there is nobody to "+
				"record this write under; run `agent-secrets login` on this machine and try again",
			*actor.Service))
	}
}

// undecidableWrites names every mutating row the project check cannot decide: one that names no
// write class - in writeClasses for a route of the v1 table, in its Scope column for a routeRow -
// and one classed scopeProject whose path names no object to resolve (ProjectOf). Every other
// class is decided: scopeDeployment and scopeProjectInBody by the class itself, scopeSession by
// withinSessionScope and the handlers behind it, and scopeNoWrite by writing nothing at all.
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
			if _, addressed := s.resolverOf(route.ProjectOf); !addressed {
				undecidable = append(undecidable, name+": classed as a project's write, but its address names no object whose project can be resolved")
			}
		default:
			source := "writeClasses"
			if route.columns {
				source = "its Scope column"
			}
			undecidable = append(undecidable, name+": no write class ("+source+")")
		}
	}
	return undecidable
}
