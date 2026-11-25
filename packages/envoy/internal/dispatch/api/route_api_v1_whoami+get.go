package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/whoami",
	Auth:        authAny,
	Description: "Who the server takes the caller for: {kind: user, login, scopes} (login is the person's lowercase email) or {kind: agent, owner, service, session, project, scopes, cut, sign_in_required} (owner is a personal token's owner or a broker-issued token's person by lowercase email, null for the shared token and for a broker-issued token that names a service; service is a verified service-account token's Kubernetes subject, or, for a broker-issued token naming no person, the fleet its machine belongs to or the Kubernetes account an exchange entry lists, null for every other credential; session is the session the caller's credential proved, null for a credential that proves none, which every one but an access token the secrets broker issued is; project is the one project key that credential lets the caller write, null where it names none; cut is the past time a read-only credential reads at, an RFC 3339 time in UTC, null for a credential with no cut; sign_in_required is true where Dispatch refuses every write of the credential with 403 SIGN_IN_REQUIRED until a person signs in on its machine, which is a broker-issued token of a machine nobody has signed in on, and false for every other credential, an exchange entry's token naming its account and every read-only credential among them). scopes is the OAuth scopes the credential holds, an empty array for one that holds none: person acts as the person on every route the dashboard reaches, and approve settles what a person settles; a read-only credential, which may call only the GET routes Dispatch classes as reads, is answered read after them. A read-only token that carries a cut is answered here, and refused 403 AS_OF_TOKEN_NOT_SERVED on every other read. A Legion daemon's own broker token answers [\"legion-status\"]: with DISPATCH_LEGION_STATUS_SCOPE on, legion-status or person lets a credential make the three writes of a Legion issue that PATCH /api/v1/issues/{key} and POST /api/v1/issues/{key}/status-change refuse to every other credential, but for the owning architect's status write: a broker-issued token whose session the daemon's own token published as the architect of an issue above writes done, backlog, icebox or triage. A write records its actor with proven: true only where this credential proved that actor, the person at a browser or the session above, and never for a session a request names under a credential that proves none.",
	Handler:     (*server).whoami,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: whoamiUser{},
		// A caller the server takes for an agent is answered the other shape at the same
		// status (issue_assignee.go).
		Also: []routeAnswer{{Status: http.StatusOK, Body: whoamiAgent{}}},
	},
})
