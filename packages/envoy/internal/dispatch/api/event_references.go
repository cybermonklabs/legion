package api

import (
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// routeSession is the session a `session:<id>` route names, "" for a route of any other kind.
func routeSession(route string) string {
	session, ok := strings.CutPrefix(route, "session:")
	if !ok {
		return ""
	}
	return session
}

// payloadReferenceKeys is every top-level payload key that names another row's object, with what it
// names and how a reference the reader does not see is left out:
//
//   - blocked_by: the issues an issue waits on, a list of keys; each hidden key is dropped.
//   - references_changed: the nodes a write's text moved, a list of {kind, id, issue_key,
//     artifact_id, ref}; each entry naming a hidden node is dropped (changedReferenceOwner).
//   - assignee, asked_of, suppressed_route_session_id: one person by login, or one session by id; a
//     hidden one is null.
//   - route, target, thread_target, suppressed_route: one route, `session:<id>` or `role:<name>`
//     (payloadStringReference); a route to a hidden session, or to a role of a private project the
//     reader does not see (routeRoleProject), is null.
//   - claim, previous_claim: the claim an issue's write left and the one a claim write replaced,
//     each {actor, at}, held by a session or a person (actorReference); a claim whose holder is
//     hidden is null, and a hidden previous_claim is left out, as a write that replaced no claim
//     records none.
//   - thread_logins: persons by login; each hidden one is dropped.
//   - mentions, deliveries: a comment's or a message's targets, each {target, session_id,
//     resolved_session_id}; each entry naming a hidden session, or a role of a hidden project, is
//     dropped.
var payloadReferenceKeys = map[string]struct{}{
	"blocked_by":                  {},
	model.ReferencesChangedKey:    {},
	"assignee":                    {},
	"asked_of":                    {},
	"thread_logins":               {},
	"route":                       {},
	"target":                      {},
	"thread_target":               {},
	"suppressed_route":            {},
	"suppressed_route_session_id": {},
	"claim":                       {},
	"previous_claim":              {},
	"mentions":                    {},
	"deliveries":                  {},
}
