package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// allowedLogin canonicalises login and checks that it names a person who has signed in and whom the
// request's reader may see (access.PersonClause, with the reader the request's check put in ctx):
// a person a restricted member or a credential acting for one may not see is refused exactly as
// one who has not signed in. Without a reader in ctx - work the server does for itself - it
// reads as everyReader, so every person who has signed in is one.
func (s *server) allowedLogin(ctx context.Context, login string) (string, error) {
	canonical := model.CanonicalLogin(login)
	var known bool
	if canonical != "" {
		reader, ok := readerFromContext(ctx)
		if !ok {
			reader = everyReader
		}
		viewer := refs.VisibilityArg(reader)
		if err := s.deps.Store.Pool.QueryRow(ctx, `select exists(select 1 from people where email = $1 and `+
			access.PersonClause("people.email", "$2")+`)`, canonical, viewer).Scan(&known); err != nil {
			// The error reaches a log line, so it names no person (TestAnAssigneeReadFailureNamesNoPerson).
			return "", fmt.Errorf("read whether the login may be assigned: %w", err)
		}
	}
	if !known {
		return "", errorf(http.StatusBadRequest, "ASSIGNEE_NOT_ALLOWED", "%q has not signed in to Dispatch", strings.TrimSpace(login))
	}
	return canonical, nil
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

// listUsers returns the people who have signed in that the caller may see (access.PersonClause),
// sorted by email: the assignee picker's options. An open viewer, a full member's credential
// whatever it lists among them, sees every one of them; a restricted member, and a credential
// acting for one, sees itself and the people of the projects it sees.
func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	reader, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select person.email from people person
		where `+access.PersonClause("person.email", "$1")+`
		order by person.email`, refs.VisibilityArg(reader))
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

// whoamiUser is the body GET /api/v1/whoami answers a person at a browser. A browser's cookie lists
// no projects, so the shape carries no list (KF4): a credential that lists projects is an agent
// credential, and answers whoamiAgent.
type whoamiUser struct {
	// Always "user": whoami writes this body for a person at a browser alone.
	Kind  string `json:"kind" enum:"user"`
	Login string `json:"login"`
	// MemberKind is "restricted" for a restricted member and absent for everyone else, so a full
	// member's answer is the one it always was.
	MemberKind string   `json:"member_kind,omitempty" enum:"restricted"`
	Scopes     []string `json:"scopes"`
}

// whoamiAgent is the body GET /api/v1/whoami answers every credential that is not a person's
// browser session.
type whoamiAgent struct {
	// Cut is the past time a read-only credential reads at (model.Credential.Cut), as an RFC 3339
	// time in UTC, and null for a credential that names no cut, which is every credential that
	// writes.
	Cut *string `json:"cut"`
	// Always "agent": whoami writes this body for every credential that is not a person's
	// browser session, the personal token holding the person scope included.
	Kind  string  `json:"kind" enum:"agent"`
	Owner *string `json:"owner"`
	// MemberKind is "restricted" when the person the credential acts for is a restricted member,
	// and absent otherwise.
	MemberKind string `json:"member_kind,omitempty" enum:"restricted"`
	// Projects is the credential's list as Resolve answers it (Viewer.Lists), sorted: a personal
	// token's in every state, and a broker token's while some project is private or its person is a
	// restricted member; absent when it lists none (whoamiProjects), so a credential with no list,
	// and a broker token while no project is private, answers what it always did. A full member's
	// list decides only which private projects it reads (access.CredentialCovers), and a restricted
	// member's every project.
	Projects []string `json:"projects,omitempty"`
	// PrivateProjects lists, sorted, the keys of Projects the caller must treat as private: a
	// private project's, one the credential no longer sees whatever its visibility, and one that
	// names no project; absent when there is none (whoamiPrivateProjects, KF4). A key is public
	// exactly when it is in Projects and not here.
	PrivateProjects []string `json:"private_projects,omitempty"`
	// Project is deprecated: the one key of a one-key list the credential's writes are held to by
	// name (writesHeldTo), and null for any other list, none included.
	Project *string  `json:"project"`
	Scopes  []string `json:"scopes"`
	Service *string  `json:"service"`
	Session *string  `json:"session"`
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
// of its service-account token, or, for a broker-issued token naming no person, the fleet its
// machine belongs to or the account an operator's entry lists, which its writes are recorded
// under (null for every other credential), the session its credential proved (null for a
// credential that proves none, which every one but a broker-issued token is), the projects it
// lists (absent where there are none) and, deprecated, the one project its writes are held to by
// name (null otherwise, a person-scoped token included). A broker token's list is answered only
// while some project is private or its person is a restricted member (access.Resolve), so while
// no project is private a full member's broker token scoped to one project answers that project
// and no projects, and a token acting as its person answers neither, as each always did (R9).
//
// Every caller is told the OAuth scopes its credential holds, which is what decides the routes it
// may call (gateRouteScopes): both for a person at a browser, the set its own row names for a
// personal token, and none for a restricted member's token whatever its row names
// (personalTokenActor), the shared token, a pod's service-account token and a
// broker-issued token that names a service and no person. The kind stays `agent` for a personal
// token holding the person scope, since the kind names the credential and not the act; the
// person that token acts for is its `owner`, which the dispatch command reads. A read-only
// credential is told read among its scopes (whoamiScopes) and the cut it reads at, null for
// none, so a caller can see that it may only read, and at what time.
//
// An agent is also told whether Dispatch refuses its every write until a person signs in on its
// machine (sign_in_required): true for a broker-issued token of a machine nobody has signed in
// on, and false for every other credential, an account entry's token naming no person among them,
// whose writes are recorded under its account, and every read-only credential, which writes
// nothing whoever signs in (signInRequired).
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	scopes := whoamiScopes(actor.Credential)
	person := actor.ID
	if !human {
		person = ""
		if actor.Owner != nil {
			person = *actor.Owner
		}
	}
	memberKind, ok := s.whoamiMemberKind(w, r, person)
	if !ok {
		return
	}
	if human {
		WriteJSON(w, http.StatusOK, newWhoamiUser(actor, memberKind, scopes))
		return
	}
	// A credential that lists projects is answered from what it may see now, which its list and its
	// person decide; one that lists none resolves nothing, as it never did.
	var viewer *model.Viewer
	if actor.Credential != nil && len(actor.Credential.Projects) > 0 && s.deps.Store != nil && s.deps.Store.Pool != nil {
		reader, err := s.readerFor(r.Context(), actor)
		if err != nil {
			s.writeHandlerError(w, fmt.Errorf("read which of the credential's projects are private: %w", err))
			return
		}
		viewer = reader.Viewer
	}
	answer := newWhoamiAgent(actor, memberKind, scopes, viewer)
	if answer.PrivateProjects, err = s.whoamiPrivateProjects(r.Context(), viewer, answer.Projects); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read which of the credential's projects are private: %w", err))
		return
	}
	_, signInRequired := s.signInRequired(r)
	answer.SignInRequired = signInRequired
	WriteJSON(w, http.StatusOK, answer)
}

// newWhoamiUser is whoami's answer to a person at a browser.
func newWhoamiUser(actor model.Actor, memberKind string, scopes []string) whoamiUser {
	return whoamiUser{Kind: "user", Login: actor.ID, MemberKind: memberKind, Scopes: scopes}
}

// newWhoamiAgent is whoami's answer to a credential that is not a person's browser session, viewer
// being what Resolve answered it may see now (nil where nothing was resolved: a credential that
// lists none, or a server with no store, which answers the credential's list as it stands). Its
// projects are its list where that list decides its reads (Viewer.Lists); its project, deprecated,
// is the one key of a one-key list its writes are held to by name (writesHeldTo), and null for any
// other list, none included.
func newWhoamiAgent(actor model.Actor, memberKind string, scopes []string, viewer *model.Viewer) whoamiAgent {
	var owner *string
	if actor.Owner != nil {
		owner = new(model.CanonicalLogin(*actor.Owner))
	}
	var session *string
	if actor.Kind == "session" && actor.ID != "" {
		session = &actor.ID
	}
	var read, written []string
	if viewer != nil {
		reader := actor
		reader.Viewer = viewer
		read = whoamiProjects(&model.Credential{Projects: viewer.Lists})
		written = whoamiProjects(&model.Credential{Projects: writesHeldTo(reader, actor.Credential)})
	} else {
		read, written = whoamiProjects(actor.Credential), whoamiProjects(actor.Credential)
	}
	var project *string
	if len(written) == 1 {
		project = &written[0]
	}
	var cut *string
	if credential := actor.Credential; credential != nil && !credential.Cut.IsZero() {
		cut = new(cutText(credential.Cut))
	}
	return whoamiAgent{
		Cut:        cut,
		Kind:       "agent",
		Owner:      owner,
		MemberKind: memberKind,
		Projects:   read,
		Project:    project,
		Scopes:     scopes,
		Service:    actor.Service,
		Session:    session,
	}
}

// whoamiMemberKind is whoami's member_kind for person: "restricted" for a restricted member, and
// "" for everyone else and for no person, never "full", which would change every full member's
// answer. A kind it cannot read is 503 IDENTITY_UNAVAILABLE, never an answer without it.
func (s *server) whoamiMemberKind(w http.ResponseWriter, r *http.Request, person string) (string, bool) {
	if person == "" || s.deps.Store == nil || s.deps.Store.Pool == nil {
		return "", true
	}
	kind, err := access.MemberKind(r.Context(), s.deps.Store.Pool, person)
	if err != nil {
		writeError(w, "IDENTITY_UNAVAILABLE", http.StatusServiceUnavailable, "the person's member kind could not be read")
		return "", false
	}
	if kind == model.MemberRestricted {
		return model.MemberAnswerRestricted, true
	}
	return "", true
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

// whoamiProjects is the projects a credential lists as whoami's agent shape answers them: a sorted
// copy, and nil, which the answer leaves out, for a credential that lists none (KF4: avenue F
// routes a session's transcripts by this list).
func whoamiProjects(credential *model.Credential) []string {
	if credential == nil || len(credential.Projects) == 0 {
		return nil
	}
	return slices.Sorted(slices.Values(credential.Projects))
}

// whoamiPrivateProjects answers which of projects, a credential's sorted list, the caller must treat
// as private (KF4): every key but those of a public project its viewer sees, read in one statement
// over the list and intersected with Viewer.Sees, so a key the credential no longer sees answers
// private whatever its project's visibility, and the answer tells nothing of a project the caller
// cannot see. viewer is the one whoami resolved for the caller, so the credential is verified once.
// A list of none answers nil; with no store, or no viewer, every key answers private (fail closed).
func (s *server) whoamiPrivateProjects(ctx context.Context, viewer *model.Viewer, projects []string) ([]string, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return projects, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx,
		`select key from projects where key = any($1) and visibility = 'public'`, projects)
	if err != nil {
		return nil, err
	}
	public, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var private []string
	for _, key := range projects {
		if !slices.Contains(public, key) || viewer == nil || !slices.Contains(viewer.Sees, key) {
			private = append(private, key)
		}
	}
	return private, nil
}
