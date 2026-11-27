package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// parseIssueAssignee decodes the tri-state `assignee` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it, which only an update does; a
// create reads a null as naming nobody, as it reads an absent field, and fills the assignee in
// (defaultAssignee). A string → its canonical email, which must name a person who has signed in
// (400 ASSIGNEE_NOT_ALLOWED otherwise). Anything else → 400 INVALID_ISSUE.
func (s *server) parseIssueAssignee(ctx context.Context, raw json.RawMessage) (assignee *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var login string
	if err := json.Unmarshal(raw, &login); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", "assignee must be a person's email or null")
	}
	canonical, err := s.allowedLogin(ctx, login)
	if err != nil {
		return nil, true, err
	}
	return &canonical, true, nil
}

// allowedLogin canonicalises login, a login the caller named, and checks that it names a person
// who has signed in. Its refusal quotes login back to the caller, whose own input it is.
func (s *server) allowedLogin(ctx context.Context, login string) (string, error) {
	canonical := model.CanonicalLogin(login)
	known, err := s.signedIn(ctx, canonical)
	if err != nil {
		return "", err
	}
	if !known {
		return "", errorf(http.StatusBadRequest, "ASSIGNEE_NOT_ALLOWED", "%q has not signed in to Dispatch", strings.TrimSpace(login))
	}
	return canonical, nil
}

// allowedActingPerson is allowedLogin for person, the canonical login the request's credential
// acts for (actorPerson) rather than one the caller typed. Its refusal names no login: the value
// is the request's own identity, and no error message of this API carries a signed-in person's
// login or session.
func (s *server) allowedActingPerson(ctx context.Context, person string) (string, error) {
	known, err := s.signedIn(ctx, person)
	if err != nil {
		return "", err
	}
	if !known {
		return "", errorf(http.StatusBadRequest, "ASSIGNEE_NOT_ALLOWED", "the person this credential acts for has not signed in to Dispatch")
	}
	return person, nil
}

// signedIn reports whether canonical, a canonical login, names a person who has signed in; a blank
// one names nobody. Its error names no login, since a handler logs a failure's error.
func (s *server) signedIn(ctx context.Context, canonical string) (bool, error) {
	if canonical == "" {
		return false, nil
	}
	var known bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `select exists(select 1 from people where email = $1)`, canonical).Scan(&known); err != nil {
		return false, fmt.Errorf("read whether a person has signed in: %w", err)
	}
	return known, nil
}

// actorPerson is the one rule for which person a credential acts for, read off the actor the
// request was authenticated as: a person at a browser is that person (a `user` actor names
// their login in ID), and any other credential acts for the person its Owner names - a personal
// token's owner, a broker-issued token's person. A credential that acts for nobody - the shared
// token, a pod's service-account token, a broker-issued token naming a service - names none,
// which is the false second result.
//
// The login is returned as model.CanonicalLogin leaves it, blank included: an actor that names
// the person it acts for with a blank login still named one, and the two callers decide what to
// do with that themselves. defaultAssignee (below) takes it as the new issue's assignee, and
// createProject (api/projects.go) falls through to its refusal, each as it did when both
// spelled these two steps out.
func actorPerson(actor model.Actor) (string, bool) {
	if actor.Kind == "user" {
		return model.CanonicalLogin(actor.ID), true
	}
	if actor.Owner != nil {
		return model.CanonicalLogin(*actor.Owner), true
	}
	return "", false
}

// defaultAssignee is who a new issue goes to when the create names nobody, with no assignee key
// or a JSON null, in order: the person the creating credential acts for (actorPerson - the human
// who created it, else the owner of the token that did), the parent's assignee when the parent is
// in the issue's own project, and the project's default assignee (projectDefault, the column a
// human sets through PUT /api/v1/projects/{key}/default-assignee). parent is nil for a parent of
// another project, which passes on no assignee (createIssue), so such an issue falls to its own
// project's default. A create that names a person never reaches here:
// parseIssueAssignee checks that login instead. So no create leaves an issue nobody holds, under
// a parent that holds nobody included; only PATCH /api/v1/issues/{key} with a null assignee does.
//
// The last step is the stored column as it stands, which every project names
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql): a shared token
// creating a root issue lands on that person rather than on nobody. The create's own update of
// the project row returns it (see the `returning` in createIssue), so no extra round trip runs
// under the project row lock, and a default left stale by a restore or a direct write is used
// as it stands rather than refusing the create.
func defaultAssignee(actor model.Actor, parent *string, projectDefault string) *string {
	if login, acts := actorPerson(actor); acts {
		return &login
	}
	if parent != nil {
		return parent
	}
	return &projectDefault
}

type dispatchUser struct {
	Login string `json:"login"`
}

// listUsersResponse is the body GET /api/v1/users answers.
type listUsersResponse struct {
	Users []dispatchUser `json:"users"`
}

// listUsers returns everyone who has signed in, sorted by email: the assignee picker's options.
func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `select email from people order by email`)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list people: %w", err))
		return
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dispatchUser, error) {
		var user dispatchUser
		err := row.Scan(&user.Login)
		return user, err
	})
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list people: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, listUsersResponse{Users: users})
}

// whoamiUser is the body GET /api/v1/whoami answers a person at a browser.
type whoamiUser struct {
	// Always "user": whoami writes this body for a person at a browser alone.
	Kind   string   `json:"kind" enum:"user"`
	Login  string   `json:"login"`
	Scopes []string `json:"scopes"`
}

// whoamiAgent is the body GET /api/v1/whoami answers every credential that is not a person's
// browser session. For a job's token, one the secrets broker's exchange minted from a job entry,
// Owner is null, since the entry names no person; Service is the identity the entry lists, a
// pod's Kubernetes service account (`system:serviceaccount:<namespace>:<name>`), a workflow
// (`<repository>/<workflow path>@<ref>`) or a Google account; Session is the session the token
// proved; Scopes is empty; Project is the one project an entry naming one project names, and
// null for an entry naming `public`, whose token names no one project and writes wherever a
// token naming no project may; ProjectScope is `named` or `public`; and SignInRequired is false,
// which is what tells a job's token from a machine's that names a service and no person too,
// whose ProjectScope is `person`, whose SignInRequired is true and whose every write Dispatch
// refuses (writeNeedsASignedInPerson).
type whoamiAgent struct {
	// Cut is the past time a read-only credential reads at (model.Credential.Cut), as an RFC 3339
	// time in UTC, and null for a credential that names no cut, which is every credential that
	// writes.
	Cut *string `json:"cut"`
	// Always "agent": whoami writes this body for every credential that is not a person's
	// browser session, the personal token holding the person scope included.
	Kind    string   `json:"kind" enum:"agent"`
	Owner   *string  `json:"owner"`
	Project *string  `json:"project"`
	Scopes  []string `json:"scopes"`
	Service *string  `json:"service"`
	Session *string  `json:"session"`
	// ProjectScope is the project scope of an access token the secrets broker issued, as the
	// broker answered it and read as its base word (model.Credential.ProjectScope): `named` for a
	// token an operator's or a job's entry mapped to its project, and for an account entry's token
	// naming no person and no project, `public` for a job's token that names no one project and so
	// writes wherever a token that names no project may, `person` for a person's own token and for
	// a machine's token that names no person, and `none` for a read entry's token; a token
	// narrowed to reading answers the word it narrows, with read among its Scopes. It is left out
	// for every other credential, which carries none, and by a Dispatch older than the field.
	ProjectScope string `json:"project_scope,omitempty"`
	// SignInRequired is whether Dispatch refuses every write this credential makes until a person
	// signs in on its machine (403 SIGN_IN_REQUIRED): the decision writeNeedsASignedInPerson acts
	// on, answered from that same function (signInRequired), so a client reads it rather than
	// guessing it from the owner and the service. A Dispatch that predates it omits the field.
	SignInRequired bool `json:"sign_in_required"`
}

// whoami tells any authenticated caller who the server takes it for: a human by login, or an
// agent with the lowercase login of the person it acts for — the personal token's owner, or a
// broker-issued token's person (null for the shared token and for a broker-issued token that
// names a service and no person) — the service it is, which is the verified Kubernetes subject
// of its service-account token or the service a broker-issued token naming no person names (a
// machine's fleet, a read entry's service, the account an operator's entry lists, which its
// writes are recorded under, or a job's identity; null for every other credential), the session
// its credential proved (null for a credential that proves none, which every one but a
// broker-issued token is), the project key that credential's token names (null where it names
// none, a person-scoped token and a job's public token included), a broker-issued token's project
// scope (left out for every other credential), and whether Dispatch refuses its every write until
// a person signs in (sign_in_required), which tells a job's token and an account entry's token
// naming no person, whose service writes, from a machine's, which writes nothing, and is false
// for every read-only credential, which writes nothing whoever signs in (signInRequired).
//
// Every caller is told the OAuth scopes its credential holds, which is what decides the routes it
// may call (gateRouteScopes) and whether it may make an issue's priority 0 (p0): all three for a
// person at a browser, the set its own row names for a personal token, and none for the shared
// token, a pod's service-account token and a broker-issued token that names a service and no
// person. The kind stays `agent` for a personal token holding the person scope, since the kind
// names the credential and not the act; the
// person that token acts for is its `owner`, which the dispatch command reads. A read-only
// credential is told read among its scopes (whoamiScopes) and the cut it reads at, null for
// none, so a caller can see that it may only read, and at what time.
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	scopes := whoamiScopes(actor.Credential)
	if human {
		WriteJSON(w, http.StatusOK, whoamiUser{Kind: "user", Login: actor.ID, Scopes: scopes})
		return
	}
	var owner *string
	if actor.Owner != nil {
		owner = new(model.CanonicalLogin(*actor.Owner))
	}
	var session *string
	if actor.Kind == "session" && actor.ID != "" {
		session = &actor.ID
	}
	var projectScope string
	if actor.Credential != nil {
		projectScope = actor.Credential.ProjectScope
	}
	var cut *string
	if credential := actor.Credential; credential != nil && !credential.Cut.IsZero() {
		cut = new(cutText(credential.Cut))
	}
	_, signInRequired := s.signInRequired(r)
	WriteJSON(w, http.StatusOK, whoamiAgent{
		Cut:            cut,
		Kind:           "agent",
		Owner:          owner,
		Project:        actor.Project,
		Scopes:         scopes,
		Service:        actor.Service,
		Session:        session,
		ProjectScope:   projectScope,
		SignInRequired: signInRequired,
	})
}

// whoamiScopes is the scopes a credential holds as whoami answers them: an empty array for a
// credential that holds none, never null, so a reader need not tell a credential with no scope
// from one whose scopes it could not read. A read-only credential is answered model.ScopeRead
// after the scopes it holds: the word marks the credential (model.Credential.ReadOnly) and is
// never stored among its scopes, so it is added here, on a copy, and never to the shared slice.
func whoamiScopes(credential *model.Credential) []string {
	switch {
	case credential == nil:
		return []string{}
	case credential.ReadOnly:
		return append(slices.Clip(credential.Scopes), model.ScopeRead)
	case len(credential.Scopes) == 0:
		return []string{}
	}
	return credential.Scopes
}
