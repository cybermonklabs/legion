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
// An earlier check here refused a credential that named one project a target of another,
// naming both. That refusal told the caller which project an object it could not see belonged
// to, so it is gone: the target outside the credential's list is now the handler's missing
// object. The two writes that take their project from the body rather than the path, the issue
// create and the going request (scopeProjectInBody), still refuse a project outside the list by
// name, since the name is the caller's own.

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

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
)

// brokerCredentialActor is the caller an access token the secrets broker issued decided, and
// whether this request presents one that resolved at all. Every other credential answers false
// without resolving anything: a browser cookie, a personal agent token, a Kubernetes
// service-account token and the shared token each prove no session, so a request presenting no
// DPoP-bound token is outside the session check. A credential that does not resolve is outside it
// too: its own refusal is the handler's to write.
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

// callerProject is the one project an access token the secrets broker issued for a named project
// carries (model.Actor.Project), and whether it carries one. It is deprecated, and goes with that
// field: callerProjects reads the list every credential carries.
func (s *server) callerProject(r *http.Request) (string, bool) {
	actor, resolved := s.brokerCredentialActor(r)
	if !resolved || actor.Project == nil || *actor.Project == "" {
		return "", false
	}
	return *actor.Project, true
}

// callerProjects is the list of projects this request's credential may use, sorted, and whether
// it lists any: a personal token's agent_tokens.projects, or the list of a token the secrets
// broker issued (model.Credential.Projects), each set from the credential and never from a body.
// A browser, the shared token and a service-account token list none, and neither does a token
// that sees every public project its person sees.
func (s *server) callerProjects(r *http.Request) ([]string, bool) {
	actor, _, err := s.optionalActor(r)
	if err != nil || actor.Credential == nil || len(actor.Credential.Projects) == 0 {
		return nil, false
	}
	return actor.Credential.Projects, true
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

// withinProjectScope runs in front of one mutating route's handler. It resolves the caller's
// viewer once (readerOf) and puts it in the request's context, where the handler and every
// loader it reads through find it (resolveReader), so a write reads the viewer once. A row that
// writes nothing and reads no row (scopeNoWrite) needs none and is returned unchanged.
//
// A row of the deployment (scopeDeployment) is refused to a viewer that is not open — a
// restricted member, or a credential that lists projects — as the read of the deployment's rows
// is (withinReadScope): what it writes belongs to no project such a viewer could be held to.
// Every other row reaches its handler, whose loaders answer an object the viewer does not see as
// missing (the file comment).
func (s *server) withinProjectScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if route.Scope == scopeNoWrite {
		return handler
	}
	deployment := route.Scope == scopeDeployment
	return func(w http.ResponseWriter, r *http.Request) {
		reader, ok := s.readerOf(w, r)
		if !ok {
			return
		}
		if deployment && !reader.Viewer.Open {
			s.refuseDeploymentWrite(w, r, route)
			return
		}
		handler(w, r.WithContext(withReader(r.Context(), reader)))
	}
}

// refuseDeploymentWrite names the row and why the caller may not write it: the projects its
// credential lists, which are the caller's own, or that it is a restricted member.
func (s *server) refuseDeploymentWrite(w http.ResponseWriter, r *http.Request, route apiRoute) {
	why := "this caller is a restricted member, who writes only the projects an access list names"
	if listed, ok := s.callerProjects(r); ok {
		why = "this request's credential may write " + projectsNamed(listed) + " and nothing else"
	}
	writeError(w, codeProjectScopeDeployment, http.StatusForbidden, fmt.Sprintf(
		"%s %s belongs to the deployment rather than to a project, which only a caller that sees every public project writes: %s",
		route.Method, route.Pattern, why))
}

// projectsNamed is a credential's list as a refusal names it.
func projectsNamed(projects []string) string {
	if len(projects) == 1 {
		return "project " + projects[0]
	}
	return "projects " + strings.Join(projects, ", ")
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
// The session itself is read as the caller sees it (access.SessionClause): a caller that is not
// open - a restricted member, or a credential that lists projects - is answered 404
// SESSION_NOT_FOUND for a session that is neither its own nor has acted in a project it sees,
// exactly as for a session nobody holds, which passes that clause for it no more. An open viewer
// sees every session, so it is answered as before and costs no read.
//
// Every other row returns the handler unchanged, so no route it does not cover pays for it.
func (s *server) withinSessionScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if !addressesOneSession(route) {
		return handler
	}
	personsOwnWrite := len(route.OAuthScopes) > 0
	return func(w http.ResponseWriter, r *http.Request) {
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
		if !reader.Viewer.Open {
			pool, err := s.readyPool()
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			var sees bool
			if err := pool.QueryRow(r.Context(), `select `+access.SessionClause("$1::text", "$2"),
				addressed, refs.VisibilityArg(reader)).Scan(&sees); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if !sees {
				writeError(w, codeSessionNotFound, http.StatusNotFound, "no session "+addressed)
				return
			}
		}
		handler(w, r.WithContext(withReader(r.Context(), reader)))
	}
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
