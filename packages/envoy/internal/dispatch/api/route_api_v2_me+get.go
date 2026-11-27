package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me", Auth: authAny, Versions: onlyV2,
		Description: "Who the server takes the caller for, one shape for every credential: {kind, person, session, " +
			"service, project, project_scope, scopes, cut, sign_in_required}. kind is user for a person at a browser and agent for every " +
			"other credential, a personal token holding the person scope included, since the kind names the " +
			"credential and not the act. person is the lowercase email of the person the credential acts for: the " +
			"person at the browser, a personal token's owner, or a broker-issued token's person, null for the shared " +
			"token and for a broker-issued token that names a service. session is the session the credential proved, " +
			"null for every credential but an access token the secrets broker issued; service is a verified " +
			"service-account token's Kubernetes subject, or the service a broker-issued token naming no person " +
			"names: the fleet of a machine nobody has signed in on, a read entry's service, the Kubernetes account " +
			"an exchange entry lists, or a job's identity as its job entry lists it, null for every other; project " +
			"is the project key the credential's token names, null where it names none, a job's token " +
			"whose entry names public among them; a token that names a project writes other projects' objects as a " +
			"credential that names none does, a private project it does not name excepted. project_scope is the project scope of an access token the secrets " +
			"broker issued, null for every other credential: named for a token an operator's or a job's entry mapped " +
			"to its project, and for an exchange entry's token naming its account and no project, public for a job's " +
			"token that names no one project, person for a person's own token and for a machine's token that names " +
			"no person, and none for a read entry's token; a token narrowed to reading answers the word it narrows. " +
			"scopes is the " +
			"OAuth scopes the credential holds, [] for none: person acts as the person on every route the dashboard " +
			"reaches, approve settles what a person settles, and p0 makes an issue's own priority 0 (a person at a " +
			"browser holds all three, a personal token holds p0 only where its maker gave it, and no broker-issued " +
			"token, shared token or service-account token holds it). Read-only credentials list read after those scopes. " +
			"cut is their past read time in UTC, or null when there is no cut; this route reports the cut rather than " +
			"reading Dispatch at that time. A Legion daemon's own broker token answers [\"legion-status\"]: with " +
			"DISPATCH_LEGION_STATUS_SCOPE on, legion-status or person lets a credential make the three writes of a " +
			"Legion issue that PATCH /api/v2/issues/{key} refuses to every other credential, in both its forms, but " +
			"for the owning architect's status write: a broker-issued token whose session the daemon's own token " +
			"published as the architect of an issue above writes done, backlog, icebox or triage. sign_in_required " +
			"is true where Dispatch refuses " +
			"every write of the credential with a 403 until a person signs in on its machine, which " +
			"is a broker-issued token of a machine nobody has signed in on, and false for every other credential, " +
			"including a job's token, an exchange entry's token naming its account and every read-only credential.",
		Handler:  s.getMeV2,
		Resource: "me", Kind: kindSingleton,
		Shape:    routeShape{Status: http.StatusOK, Response: meV2{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal},
	}
})
