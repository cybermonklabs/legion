package api

// project_scope.go answers what a write is about to touch — which project does the object it
// addresses belong to, and which session does its path name? — and refuses a caller whose
// credential does not cover that project, or proves another session. owner.go is not this: it
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
//     account move nothing: a rank neighbour outside the issue's own project is refused, while
//     a parent and a `blocked_by` target may each be an issue of any project. The write asks
//     access.CredentialCovers about a parent or a target of another project itself, because
//     this check reads the project of the object the path names and no key in the body. So the
//     project this check reads before the handler runs is the project the handler will write,
//     and the check needs no transaction of its own.
//   - The check is a no-op for a credential that carries no project: a browser cookie, a personal
//     agent token, a Kubernetes service-account token and the shared token name none, and so do
//     six access tokens the broker issues, each of which reaches brokerTokenActor with no
//     Project. Two of them the broker scopes to a person rather than to one project, and both
//     carry `project_scope` `person`: a dev box session's, unscoped here with no second mechanism
//     of its own, and a machine's that names a service and no person, whose every write
//     writeNeedsASignedInPerson refuses below. The third is the `named` token of an account
//     entry that names no project (`<service account>==<person>`, the shared public-session
//     account's), which names its person and writes wherever Dispatch's own rules let it
//     (brokerTokenActor). The fourth is the `named` token of an account entry that names no
//     person either (`<service account>==`), which names the account as its service and writes
//     the same way, recorded under that account. The fifth is a job's `public` token, which
//     names its job's identity as its service and no person, carries the private projects its
//     list names, which no check here reads, and writes wherever Dispatch's own rules let a token
//     naming no project: writeNeedsASignedInPerson lets it write, and a write that belongs to the
//     deployment is not refused here. The sixth is the read entry's token, which carries
//     `project_scope` `none` and the scope `read` alone and names a service and no person. It
//     never reaches this check, and nor does any other read-only credential (a token narrowed to
//     reading, which may name the project it reads as, or an account it would have written as):
//     the read-only gate register puts outside it answers every such credential 403
//     READ_ONLY_TOKEN on every route outside the read class, every write among them
//     (refuseReadOnlyToken, routeDecl.Read). A job's `named` token is no such credential: it
//     carries the one project its entry names, and the check below judges it as it judges a
//     person's named token for that project. That is the rule and not a transitional measure:
//     every caller that exists today writes exactly what it wrote before, which is what lets this
//     deploy before anything presents a broker token.
//
// A credential that names one project writes every project's objects that a credential naming
// none may write. The one exception is a private project that the credential does not name.
// access.CredentialCovers holds that test. Each resolver reads the visibility of the target's
// project in the statement that reads that project, and the check passes it to the test. A
// target of a private project that the credential does not name gets the refusal of a target
// that resolves to nothing: an object that the caller cannot see answers as one that does not
// exist. A target that resolves to no project at all, or to nothing at all — no such row, or an
// id Postgres will not read as a uuid — is refused, so the check fails closed. Each resolver sends
// Postgres the value the prefix layer resolved, the one the handler behind it sends too: an id
// written as a unique prefix is the whole id by the time either reads it, and anything else is
// the path value as written (resolveIDPrefixes, which register wraps outside this check for
// exactly that reason). So the parser that decides what this check resolved is the parser the
// handler then addresses its row by, and the two cannot disagree about which object a path
// names: Go's uuid.Parse refuses spellings Postgres accepts (a hyphen after any group of four
// digits), and a check that resolved nothing where the handler resolves a row would be a write
// nothing judged.
//
// Failing closed does not hide which ids exist, and does not claim to. A target of another
// project that the credential covers reaches its handler. A target that resolves to nothing gets
// this refusal, and so does an object of a private project that the credential does not name. So
// a scoped caller can tell an object it may write from an id nobody holds, but not an id nobody
// holds from an object it cannot see. Nothing is read out that way: no GET route is
// project-scoped.
//
// A third check stands outside both, in front of every mutating row whatever its class
// (writeNeedsASignedInPerson): a broker credential that names a service and no person writes only
// when its project scope is `named` or `public`: the two shapes of a job's token, which the
// exchange mints from an entry that names the job's identity and its projects and no person, and
// under `named` the token of an account entry that names no person and no project, whose service
// is that account. Every other broker credential naming no person is refused SIGN_IN_REQUIRED. The
// rule names the scopes that write rather than the one that does not, so it fails closed: today the
// one other scope that reaches it is `person`, a machine nobody has signed in on, and a project
// scope the broker answers later is refused until this rule names it.

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
)

const (
	// codeProjectScope refuses a project-scoped caller a write whose target resolves to no
	// project or to nothing. An object of a private project that the credential does not name
	// gets the same refusal, as an object that does not exist.
	codeProjectScope = "PROJECT_SCOPE"
	// codeProjectScopeDeployment refuses a project-scoped caller a write that belongs to the
	// deployment rather than to any one project.
	codeProjectScopeDeployment = "PROJECT_SCOPE_DEPLOYMENT"
	// codeSessionScope refuses a caller whose credential proved one session a write addressed
	// to another session's own rows.
	codeSessionScope = "SESSION_SCOPE"
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

// callerProject is the one project this request's credential names, and whether it names one
// at all. Only an access token the secrets broker issued for a named project carries one
// (model.Actor.Project, set by brokerTokenActor from the token and never from a body). Such a
// caller writes every project that access.CredentialCovers covers, not only the one it names.
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

// projectResolver answers the project a request's addressed object belongs to, and whether that
// project is private, read in one statement. An empty project with no error means the address
// resolved to no project: an object that belongs to none, a target that does not exist, or an id
// Postgres will not read as a uuid.
type projectResolver func(ctx context.Context, q queryer, r *http.Request) (project string, private bool, err error)

// projectResolverFor is the resolver a route's address calls for, read from the shape of its
// pattern, and whether the pattern names an object at all. Every row the table classes
// scopeProject has one; undecidableWrites holds the table to that and register refuses to
// mount a table that breaks it, so a mounted scopeProject row always resolves.
func (s *server) projectResolverFor(pattern string) (projectResolver, bool) {
	switch {
	case addresses(pattern, "/api/v1/issues/{key}"):
		return s.projectOfIssue, true
	case addresses(pattern, "/api/v1/projects/{key}"):
		return s.projectOfProject, true
	case addresses(pattern, "/api/v1/artifacts/{id}"):
		return s.projectOfArtifact, true
	case addresses(pattern, "/api/v1/asks/{id}"):
		return s.projectOfAsk, true
	case addresses(pattern, "/api/v1/comments/{id}"):
		return s.projectOfComment, true
	case addresses(pattern, "/api/v1/messages/{id}"):
		return s.projectOfMessage, true
	case addresses(pattern, "/api/v1/results/{id}"):
		return s.projectOfCheckResult, true
	}
	return nil, false
}

// secondProjectResolverFor is the resolver of a second project that a route's write is appended
// to, beside the project of the object its path names, or nil for a route that writes one
// project's objects alone. A retraction is appended to the result's check's issue, which a parent
// of another project can put in another project than the result's own issue
// (projectOfRetractedCheck). Asking it here, before the keyed layer and the handler, gives a check
// of a private project the answer of a result that does not exist, header and stored key alike.
func (s *server) secondProjectResolverFor(pattern string) projectResolver {
	if pattern == "/api/v1/results/{id}/retract" {
		return projectOfRetractedCheck
	}
	return nil
}

// addresses answers whether pattern addresses the object prefix names: that object itself, or
// anything under it.
func addresses(pattern, prefix string) bool {
	return pattern == prefix || strings.HasPrefix(pattern, prefix+"/")
}

// projectOfIssue reads the project of the issue the path names and that project's visibility
// (issueProject).
func (s *server) projectOfIssue(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return issueProject(ctx, q, r.PathValue("key"))
}

// issueProject reads the project of the issue key and whether that project is private, in one
// statement. An empty project with no error means that no issue has the key (scanProjectKey). It
// serves the pre-handler check of an issue's address (projectOfIssue), the results routes' check
// of a check's issue (checkIssueProject), a status change's check of its named issue under its
// locks (planStatusChangeUnderLocks), and the reads under the locks of a parent
// (validateIssueParent) and of a blocked_by target (writeBlockedBy). A write check that needs more
// of the issue's row reads the project and its visibility in a statement of its own, as a
// create's read of its parent under its locks does (readCreateParent).
func issueProject(ctx context.Context, q queryer, key string) (project string, private bool, err error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select i.project_key, p.visibility = 'private'
		from issues i
		join projects p on p.key = i.project_key
		where i.key = $1
	`, key))
}

// projectOfProject is the one-row read the other resolvers make, against the projects table
// itself: a project's own key is the project it belongs to, and a key no project has resolves
// to none. It reads the key and the visibility alone rather than going through loadProject, whose
// 404 this would only have to undo.
func (s *server) projectOfProject(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `select key, visibility = 'private' from projects where key = $1`, r.PathValue("key")))
}

// projectOfArtifact reads the project_key every artifact carries (artifactColumns), which is
// its issue's for an issue document and its own for a project document. An agent conversation's
// artifact carries none, so it resolves to no project and a project-scoped caller is refused
// it.
func (s *server) projectOfArtifact(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(a.project_key, ''), coalesce(p.visibility = 'private', false)
		from artifacts a
		left join projects p on p.key = a.project_key
		where a.id = $1
	`, r.PathValue("id")))
}

// projectOfAsk resolves an ask's owner project and visibility through askedOfPersonJoins. An ask
// belongs to an issue or a document, and takes that owner's project.
func (s *server) projectOfAsk(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, ''), coalesce(p.visibility = 'private', false)
		from asks a`+askedOfPersonJoins+`
		where a.id = $1
	`, r.PathValue("id")))
}

// projectOfComment is one hop to the comment's issue or document, since the comments table holds
// no project of its own (store/migrations/0001_init.up.sql, 0009_project_artifacts.up.sql).
func (s *server) projectOfComment(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, ''), coalesce(p.visibility = 'private', false)
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts ar on ar.id = c.artifact_id`+ownerProjectJoin+`
		where c.id = $1
	`, r.PathValue("id")))
}

// projectOfMessage is the same one hop for a message, which hangs from an issue or from nothing
// at all: a direct message between a person and a session belongs to no project, so it resolves
// to none here, and the two routes a session answers its own delivery on are classed
// scopeSession rather than this.
func (s *server) projectOfMessage(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ''), coalesce(p.visibility = 'private', false)
		from messages m
		left join issues i on i.key = m.issue_key
		left join projects p on p.key = i.project_key
		where m.id = $1
	`, r.PathValue("id")))
}

// projectOfCheckResult is one hop to the project of the issue the result was recorded on,
// since the check_results table holds no project of its own
// (store/migrations/20261102122200_check_results.up.sql). It reads issue_key and not
// check_issue_key: a result recorded on a child against a parent's check belongs to the child.
// The two issues can be of different projects, since an issue's parent may be of another
// project, so a retraction asks access.CredentialCovers about a check of another project itself
// (retractCheckResult), as the record route does (readResultRequest).
func (s *server) projectOfCheckResult(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select coalesce(i.project_key, ''), coalesce(p.visibility = 'private', false)
		from check_results c
		left join issues i on i.key = c.issue_key
		left join projects p on p.key = i.project_key
		where c.id = $1
	`, r.PathValue("id")))
}

// checkIssueProject reads the project of a check's issue, checkIssue, and whether that project is
// private, for a caller whose credential names one project, caller. An issue with no row resolves
// to no project and is refused, failing closed as the pre-handler check does.
func checkIssueProject(ctx context.Context, q queryer, caller, checkIssue string) (string, bool, error) {
	project, private, err := issueProject(ctx, q, checkIssue)
	if err != nil {
		return "", false, err
	}
	if project == "" {
		return "", false, errorf(http.StatusForbidden, codeProjectScope,
			"this request's credential names project %s, and the check's issue %s belongs to no project", caller, checkIssue)
	}
	return project, private, nil
}

// projectVisibility reads whether the project key is private, and whether any project has that
// key: the one-statement read of a write whose project its body names (createIssue,
// createGoingRequest).
func projectVisibility(ctx context.Context, q queryer, key string) (private, found bool, err error) {
	err = q.QueryRow(ctx, `select visibility = 'private' from projects where key = $1`, key).Scan(&private)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return private, true, nil
}

// scanProjectKey reads a resolver's two columns, the project and whether it is private, answering
// no project for a target that does not resolve rather than an error: no such row, or a path
// value Postgres will not read as a uuid (SQLSTATE 22P02, which it reports as it binds the
// parameter). Those are the ids a handler answers 404 or 400 for, and withinProjectScope refuses
// them to a project-scoped caller rather than letting that answer out.
func scanProjectKey(row pgx.Row) (string, bool, error) {
	var project string
	var private bool
	if err := row.Scan(&project, &private); err != nil {
		if unaddressed(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return project, private, nil
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
// For a caller whose credential names no project, a covered route costs one header read and
// calls the handler. For one that names a project, the check resolves the target's project and
// that project's visibility. A target of a project that access.CredentialCovers covers reaches
// the handler, whichever project the credential names. A target that resolves to no project or
// to nothing is refused, so the check fails closed, and an object of a private project that the
// credential does not name gets that same refusal.
func (s *server) withinProjectScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	switch route.WriteClass {
	case scopeProject:
		resolve, addressed := s.projectResolverFor(route.Pattern)
		second := s.secondProjectResolverFor(route.Pattern)
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
				s.writeHandlerError(w, outsideProjectScopeError(covers))
			}
		}
		return func(w http.ResponseWriter, r *http.Request) {
			covers, scoped := s.callerProject(r)
			if !scoped {
				handler(w, r)
				return
			}
			belongsTo, private, err := resolve(r.Context(), s.deps.Store.Pool, r)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			// An object that belongs to no project, a row that is not there, or an id Postgres
			// will not read: refusing these is what makes the check fail closed. An object of a
			// private project that the credential does not name gets the same answer, so the
			// caller cannot tell it from an id nobody holds. The check does not hide which ids
			// exist, and it does not claim to (the file comment).
			if belongsTo == "" || !access.CredentialCovers(access.Credential{Listed: []string{covers}}, belongsTo, private) {
				s.writeHandlerError(w, outsideProjectScopeError(covers))
				return
			}
			if second != nil {
				checkProject, checkPrivate, err := second(r.Context(), s.deps.Store.Pool, r)
				if err != nil {
					s.writeHandlerError(w, err)
					return
				}
				if checkProject == "" || !access.CredentialCovers(access.Credential{Listed: []string{covers}}, checkProject, checkPrivate) {
					s.writeHandlerError(w, outsideProjectScopeError(covers))
					return
				}
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
			writeError(w, codeProjectScopeDeployment, http.StatusForbidden, fmt.Sprintf(
				"this request's credential names project %s, and %s %s belongs to the deployment rather than to a project",
				covers, route.Method, route.Pattern))
		}
	default:
		// A write that belongs to the calling session (scopeSession, held to its own session by
		// withinSessionScope), the issue create and the going request, whose project each
		// handler reads from the body (scopeProjectInBody), and a row that writes nothing at
		// all (scopeNoWrite).
		return handler
	}
}

// outsideProjectScopeError is the refusal of a write whose target resolves to no project or to
// nothing, to a caller whose credential names the one project covers. It names that project and
// nothing of the credential itself. It names no project of the target, since there is none to
// name and an invented one would say something about what exists. withinProjectScope answers it
// before a handler runs, also for an object of a private project that the credential does not
// name, retractCheckResult answers it for a check of such a project
// (refuseRetractionOfUncoveredCheck), and planStatusChangeUnderLocks answers it for a named issue
// whose project turned private after withinProjectScope, so each answers as for a target that does
// not exist. Each writes this one value through writeHandlerError, so the answers cannot drift.
func outsideProjectScopeError(covers string) *apiError {
	return errorf(http.StatusForbidden, codeProjectScope,
		"this request's credential names project %s, and this write resolves to no project", covers)
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
// need the `person` scope (its route's OAuthScopes), and a person writing to a session is by design writing
// into a session that is not theirs. Without this a person's own broker-issued credential could
// only ever address the session it was issued for, which is nobody's conversation but its own.
// The row decides it, not the request: gateRouteScopes stands ahead of this wrapper and has
// already refused every credential that does not hold the row's whole set, so on a row that
// declares any, the caller holds them. A row that declares none is open to every authenticated
// caller, and this check refuses a session other than the one it proved as it always did.
//
// Every other row returns the handler unchanged, so no route it does not cover pays for it.
func (s *server) withinSessionScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if !addressesOneSession(route) {
		return handler
	}
	personsOwnWrite := len(route.OAuthScopes) > 0
	return func(w http.ResponseWriter, r *http.Request) {
		proved, provedOne := s.callerSession(r)
		if !provedOne {
			handler(w, r)
			return
		}
		if addressed := r.PathValue("session_id"); addressed != proved && !personsOwnWrite {
			writeError(w, codeSessionScope, http.StatusForbidden, fmt.Sprintf(
				"this request's credential acts for session %s; %s %s writes into session %s",
				proved, route.Method, route.Pattern, addressed))
			return
		}
		handler(w, r)
	}
}

// addressesOneSession answers whether route is a write to the rows of the one session its path
// names: the scopeSession rows whose pattern carries {session_id}. The other scopeSession rows
// address no session of their own — the `me` token rows are the calling person's, and the two
// message rows are held to the session their delivery attempt names.
func addressesOneSession(route apiRoute) bool {
	return route.WriteClass == scopeSession && strings.Contains(route.Pattern, "{session_id}")
}

// writeNeedsASignedInPerson refuses every write by a caller whose broker-issued credential names
// a service and no person, unless its project scope is `named` or `public`: the two shapes of a
// job's token, which the secrets broker's exchange mints from a job entry that names the job's
// identity, its projects and no person, and, under `named`, the token of an exchange account
// entry that names no person and no project, whose service is that account (brokerTokenActor).
// Such a token writes as its session, for its service, and the project check judges a `named` one
// naming a project as it judges a person's named token for that project. The refusal is here, in
// front of every mutating row whatever its class, rather than in each route, for the reason the
// two checks above are: a route added later is covered by the table it joins and not by a line
// someone remembered to write.
//
// The rule names the scopes that write and refuses every other, so it fails closed. The one
// other scope the verifier lets reach it is `person` (ciphertoken.Caller): an access token the
// broker issued to a machine nobody has signed in on. Such a caller may read everything its
// session may read. What it may not do is write, because nobody signed in on that machine and no
// entry named it a job's; a session that accepted a delivery or answered an ask under it would
// write for a machine nobody stands behind. The refusal's message names `agent-secrets login`,
// the command that signs a person in on that machine, since running it is the whole remedy. A
// project scope the broker answers later is refused the same way until this rule names it.
//
// A token naming no person that writes under one of the named scopes has its writes recorded
// under its service (model.Actor.Service), so it reaches the handler as a credential naming a
// person does, and it is held to the routes its lack of OAuth scope leaves it (gateRouteScopes).
// The test is on the project scope the broker answered, one of writingServiceScopes and nothing
// else, so a scope this check does not know is refused as a machine's is.
//
// A read-only token may name a service and no person too, an account entry's narrowed to
// reading among them, but it is no such caller: it may only read, and signing a person in is no
// remedy for it. It never reaches this check, since the read-only gate register puts outside it
// has already answered it 403 READ_ONLY_TOKEN, as it answers a read-only token on every write
// (refuseReadOnlyToken), and signInRequired answers it false.
//
// Every other credential is unaffected: a cookie, a personal token, a service-account token, the
// shared token and a broker token that does name a person each reach the handler as before, at
// the cost of the one header read brokerCredentialActor makes.
//
// The decision is signInRequired's, which whoami answers as sign_in_required, so a client reads
// it from Dispatch rather than guessing it from the shape of who it is.
func (s *server) writeNeedsASignedInPerson(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		service, required := s.signInRequired(r)
		if !required {
			handler(w, r)
			return
		}
		writeError(w, contracts.DispatchSignInRequired, http.StatusForbidden, fmt.Sprintf(
			"this request's credential acts for service %s and no person, so there is nobody to "+
				"record this write under; run `agent-secrets login` on this machine and try again",
			service))
	}
}

// signInRequired is the one decision writeNeedsASignedInPerson refuses a write on and whoami
// answers as sign_in_required: whether r presents a broker-issued token that resolved, may write,
// and names a service and no person, and whose project scope is not one under which such a token
// writes as its service (writesAsItsService), which is a machine nobody has signed in on, and the
// service that token names. Every other caller answers false: a credential that is no broker
// token, one that did not resolve (its refusal is written further in), one that names a person, a
// job's token and an account entry's token naming no person, whose writes are recorded under
// their service, and a read-only token, whatever it names. A read-only token writes nothing
// whoever signs in: the read-only gate answers its every write 403 READ_ONLY_TOKEN before
// writeNeedsASignedInPerson runs, and whoami tells it read among its scopes rather than that a
// person must sign in.
func (s *server) signInRequired(r *http.Request) (string, bool) {
	actor, resolved := s.brokerCredentialActor(r)
	if !resolved || actor.Owner != nil || actor.Service == nil || writesAsItsService(actor.Credential) || (actor.Credential != nil && actor.Credential.ReadOnly) {
		return "", false
	}
	return *actor.Service, true
}

// writingServiceScopes holds the project scopes under which a broker-issued token that names a
// service and no person writes as that service: `named`, which a job's token naming its one
// project and an exchange account entry's naming no person and no project (whose service is that
// account) carry, and `public`, a job's. Dispatch refuses every other such token every write with
// SIGN_IN_REQUIRED, a project scope the broker answers later among them, until this list names
// it.
var writingServiceScopes = []string{ciphertoken.ProjectScopeNamed, ciphertoken.ProjectScopePublic}

// writesAsItsService answers whether a broker-issued credential carries one of the project
// scopes under which a token naming a service and no person writes as that service,
// writingServiceScopes: `named`, a job's token naming its one project or an exchange account
// entry's naming no person and no project, whose service is that account, and `public`, a job's.
// whoami answers the decision built on it as sign_in_required (signInRequired), which `dispatch
// whoami` reads. A nil credential, an empty scope and every other word answer false, `person`
// among them, a machine's nobody signed in on.
func writesAsItsService(credential *model.Credential) bool {
	return credential != nil && slices.Contains(writingServiceScopes, credential.ProjectScope)
}

// undecidableWrites names every mutating route the project check cannot decide: one whose
// declaration states no class, and one classed scopeProject whose address names no object to
// resolve. Every other class is decided: scopeDeployment and scopeProjectInBody by the class
// itself, scopeSession by withinSessionScope and the handlers behind it, and scopeNoWrite by
// writing nothing at all. TestEveryMutatingRouteIsClassed walks the table through it, and
// register refuses to mount a table it names anything in, so a route added later cannot be left
// undecided. The other direction the map beside the table needed, a class naming no route, cannot
// happen now: a class is a field of the route's own declaration.
func (s *server) undecidableWrites(routes []apiRoute) []string {
	var undecidable []string
	for _, route := range routes {
		if route.Method == http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		switch route.WriteClass {
		case scopeSession, scopeDeployment, scopeProjectInBody, scopeNoWrite:
		case scopeProject:
			if _, addressed := s.projectResolverFor(route.Pattern); !addressed {
				undecidable = append(undecidable, name+": classed as a project's write, but its address names no object whose project can be resolved")
			}
		default:
			undecidable = append(undecidable, name+": no write class (routeDecl.WriteClass)")
		}
	}
	return undecidable
}
