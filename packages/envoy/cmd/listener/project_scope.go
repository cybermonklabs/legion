package main

// project_scope.go is the listener's edge mode: the edge listener an outside person's agents
// reach Envoy through, set by ENVOY_EDGE_PROJECTS (config.Service.EdgeProjects), which names the
// projects it serves. It holds every session to the projects of the token that admitted it, also
// where each project is public, and gives a session no message of no project but the GitHub
// events of its projects' repositories.
//
// The rules, each held by a row of edge_mode_test.go, edge_roles_test.go or
// edge_repositories_test.go:
//
//   - Admission: a broker token alone, whose projects are all among the ones the edge serves, of
//     project scope named or person (EdgeAdmits), decided in apiAuth before any handler; a refusal
//     names what the token carries and no project the edge serves. The admitted request is marked
//     held to its token's projects (withEdgeHold), and every rule below reads them there
//     (heldProjects, edgeScopeOf), so a request that is not held is one this listener serves as
//     every listener does.
//   - The topics a session follows: its own agent subject, a topic of one of its projects, or a
//     GitHub topic of one repository that the broker's answer for its projects names, read fresh
//     for the registration (edgeTopicHold, edge_repositories.go). The hold judges the topics the
//     registration names before anything is planned, and every topic of each plan after, the
//     topics the stored row brings in among them, so a topic another listener stored for the
//     session is dropped from its row and its consumer's filter. At relay a GitHub frame reaches
//     the session only while a fresh answer names the repository its event names
//     (sessionRelays.edgeWithholds), with the gap mark on the next frame while no answer stands,
//     written on the session's entry before the withheld event is acknowledged and kept across the
//     session's relay runs (sessionRelays.noteRepositoryGap, session.SessionEntry.RepositoryGap),
//     and the refresh removes an interest in a repository that left (pruneEdgeInterests). What a
//     session publishes is narrower: a role subject of its project in the form that names it
//     (edgePublishRefusal). A refused registration names every topic it refused
//     (listenerwire.EdgeRefusal).
//   - Senders: a send or a publish is the session's own, source agent and the token's session
//     (edgeSenderRefusal).
//   - Roles: a claim or a read names a role that works for one of the token's projects
//     (edgeRoleRefusal), and a role held by a session of none of them reads as one no claim holds
//     in a role read, a role send's answers and a claim's 409, which name neither its holder nor
//     its claim revision (edgeScope.holderConfirmed, edgeScope.namesHolderConfirmed,
//     roleHeldElsewhere); a read of a holder's session that does not answer names no holder either
//     (edgeScope.unresolved). A registration first releases the session's claims of other
//     projects' roles, refused 503 when a release fails (edgeTopicHold.releaseRefusedRoles), renews
//     its token's projects' roles alone (edgeTopicHold.holdsRole), and a claimant is handed the
//     kept messages of those alone (edgeScope.roles).
//   - Sends: the target session's token lists one of the token's projects, else the send is
//     answered as to a missing session (edgeScope.hidesConfirmed).
//   - Reads: the session list, the registry, the session rows' roles and topics, a session's
//     successors (shownSuccessors) and the Legion agent routes answer only sessions, roles, topics
//     and agents of the token's projects (edgeScope), each session shown confirmed by its stored
//     record read from the stream's leader
//     (edgeScope.showsConfirmed, session.SessionRegistry.StoredByWriter), so a record the server
//     wrote at a session's key reads as no session's, and a read that did not answer is the
//     bus-unavailable 503, whose text names no session (edgeScope.confirmUnanswered); the interest
//     list and another session's interests are 403, and the Legion agent registry is not written
//     here.
//   - The start: refused beside every other credential, beside Dispatch's principal, beside
//     webhooks, and without the broker-token settings (validateEdgeMode).
//
// A project is one of the token's when the token's list names it: the admission admits no token
// listing none, so a list here is never empty.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// ErrEdgeNotAdmitted is every refusal EdgeAdmits answers; the refusal's text says which rule.
var ErrEdgeNotAdmitted = errors.New("this edge listener does not admit the access token")

// EdgeAdmits is the admission: whether an edge listener serving edgeProjects admits caller, the
// session a broker token names. It admits a token of project scope named or person whose projects
// are all among edgeProjects, and at least one, and refuses a read-only token. nil is admitted;
// every refusal wraps ErrEdgeNotAdmitted and names the rule and what the token carries, and never
// a project the edge serves: the refusal reaches the token's holder (apiAuth), who may not know
// another customer's project, so the projects served go to apiAuth's log line alone. It reads
// nothing but its arguments. apiAuth never hands it a read-only token: the listener's verifier
// refuses one as no session (ciphertoken.ErrNotASession), the 401 of a request with no session.
func EdgeAdmits(edgeProjects []string, caller ciphertoken.Caller) error {
	switch {
	case caller.ReadOnly:
		return fmt.Errorf("%w: a read-only token acts as no session here", ErrEdgeNotAdmitted)
	case caller.ProjectScope != ciphertoken.ProjectScopeNamed && caller.ProjectScope != ciphertoken.ProjectScopePerson:
		return fmt.Errorf("%w: its project scope is %q, and the edge admits only %s and %s",
			ErrEdgeNotAdmitted, caller.ProjectScope, ciphertoken.ProjectScopeNamed, ciphertoken.ProjectScopePerson)
	case len(caller.Projects) == 0:
		return fmt.Errorf("%w: it lists no project", ErrEdgeNotAdmitted)
	}
	for _, project := range caller.Projects {
		if !slices.Contains(edgeProjects, project) {
			return fmt.Errorf("%w: it lists project %s, which this edge does not serve", ErrEdgeNotAdmitted, project)
		}
	}
	return nil
}

// The reasons an edge refusal carries (listenerwire.EdgeRefusal.Reason).
const (
	edgeTokenNotAdmitted         = "token_not_admitted"
	edgeTopicNotInProject        = "topic_not_in_project"
	edgeRepositoryNotInProject   = "repository_not_in_project"
	edgeRepositoriesUnknown      = "repositories_unknown"
	edgeTopicNotPublished        = "topic_not_published"
	edgeRoleNotInProject         = "role_not_in_project"
	edgeSessionNotInProject      = "session_not_in_project"
	edgeLegionRegistryNotWritten = "legion_registry_not_written"
	edgeSenderNotSession         = "sender_not_session"
)

// edgeRefusal is one answer an edge rule gives a request it refuses.
type edgeRefusal struct {
	status int
	body   listenerwire.EdgeRefusal
}

// refuseEdge is the refusal of reason under status, its sentence message, about the request
// fields expected.
func refuseEdge(status int, reason, message string, expected ...string) *edgeRefusal {
	return &edgeRefusal{status: status, body: listenerwire.EdgeRefusal{Error: message, Reason: reason, Expected: expected}}
}

func (e *edgeRefusal) write(w http.ResponseWriter) { writeJSON(w, e.status, e.body) }

// edgeHoldKey is the context key that marks a request an edge listener admitted.
type edgeHoldKey struct{}

// edgeHold is the mark withEdgeHold puts on a request an edge listener admitted: the logger that
// gets what the edge's answers hide (edgeScope.confirmUnanswered), apiAuth's own.
type edgeHold struct {
	logger *logging.Logger
}

// withEdgeHold marks a request an edge listener admitted (apiAuth): every rule here holds it to
// the projects of the broker token it was admitted on, which its broker caller carries. logger
// gets what an answer to it hides.
func withEdgeHold(ctx context.Context, logger *logging.Logger) context.Context {
	return context.WithValue(ctx, edgeHoldKey{}, edgeHold{logger: logger})
}

// heldProjects is the projects r is held to, its broker token's (ciphertoken.Caller.Projects, the
// caller's own copy), and whether it is held at all: true only for a request an edge listener
// admitted, false on every other listener.
func heldProjects(r *http.Request) ([]string, bool) {
	if _, held := r.Context().Value(edgeHoldKey{}).(edgeHold); !held {
		return nil, false
	}
	caller, _ := brokerCaller(r)
	return caller.Projects, true
}

// edgeScope is what a request an edge listener admitted may read: the projects of its token
// (heldProjects), and the repository reader that says which of their GitHub topics stand. It is
// nil for a request no edge listener admitted, and each method answers a nil scope as every
// listener answers, so a read route asks it unconditionally and writes no edge branch of its own;
// only the answers a session with no entry gets test the scope itself (hidesConfirmed). logger gets
// what its answers hide (confirmUnanswered).
type edgeScope struct {
	projects     []string
	repositories *edgeRepositories
	logger       *logging.Logger
}

// edgeScopeOf is r's scope, nil for a request no edge listener admitted. repositories may be nil
// for a route that reads no topic.
func edgeScopeOf(r *http.Request, repositories *edgeRepositories) *edgeScope {
	projects, held := heldProjects(r)
	if !held {
		return nil
	}
	hold, _ := r.Context().Value(edgeHoldKey{}).(edgeHold)
	return &edgeScope{projects: projects, repositories: repositories, logger: hold.logger}
}

// shows reports whether the scope shows the session whose entry is entry: its record names its own
// key (session.SessionEntry.SessionID) and the token that registered it
// (session.SessionEntry.Projects) lists one of the scope's projects. An entry no token's list was
// recorded on, one an earlier build wrote among them, is no project's, and so is one that names no
// key: a record naming no key cannot be told from another session's record the server copied under
// this one, so the edge grants no project on it (session.DecodeEntry reads such a record with no
// SessionID). The cache alone cannot say the record is a writer's; showsConfirmed adds the leader
// read that does. A nil scope shows every session.
func (s *edgeScope) shows(entry session.SessionEntry) bool {
	return s == nil || (entry.SessionID != "" && slices.ContainsFunc(entry.Projects, func(project string) bool { return slices.Contains(s.projects, project) }))
}

// showsConfirmed is shows, and for a session it would show, confirmed by sessionID's newest message
// on the bucket's stream, read from the stream's leader with the headers stored with it
// (session.SessionRegistry.StoredByWriter). sessionID is the key the decision is about - the list
// row's key, the send's target, the registry route's id - never the key a cached record names: a
// record the server wrote, or one a cached entry still shows under projects a newer write dropped,
// is judged by the record the leader holds for the key now. shows is re-judged on that leader record,
// so the session is granted only when the leader's newest message is a writer's registration the
// scope shows. A message the server wrote, one that does not decode as the key's registration, or
// none, reads as no session's, and so does a key the leader refused or held no message for
// (nats.ErrKeyNotFound, bus.ErrRefused). A read that did not answer is returned as an error, not as
// an absence, so a caller answers the bus-unavailable 503 it answers for any read that did not
// answer, never the 404 of a session it does not show nor a silent drop from a list; that error
// names no session (confirmUnanswered). A nil scope shows every session, and an entry the cache
// would not show, both cost no read.
func (s *edgeScope) showsConfirmed(ctx context.Context, sessions *session.SessionRegistry, sessionID string, entry session.SessionEntry) (bool, error) {
	if s == nil {
		return true, nil
	}
	if !s.shows(entry) {
		return false, nil
	}
	stored, ok, err := sessions.StoredByWriter(ctx, sessionID)
	if err != nil {
		return false, s.confirmUnanswered(sessionID, err)
	}
	return ok && s.shows(stored), nil
}

// hidesConfirmed answers a session read, as entry with err, hidden with showsConfirmed's leader read: a session the scope does not show,
// confirmed by the record the leader holds for sessionID, and one the bucket holds no record of
// (nats.ErrKeyNotFound), are hidden as a session no listener has heard of (unknownSession).
// sessionID is the key the decision is about. A read that did not answer, the confirmation's own
// among them, is returned as an error, not as hidden, so the route answers the bus-unavailable 503;
// the confirmation's names no session (confirmUnanswered).
// A nil scope hides nothing.
func (s *edgeScope) hidesConfirmed(ctx context.Context, sessions *session.SessionRegistry, sessionID string, entry session.SessionEntry, err error) (bool, error) {
	if s == nil {
		return false, nil
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return true, nil
	}
	if err != nil {
		return false, nil
	}
	shown, confErr := s.showsConfirmed(ctx, sessions, sessionID, entry)
	if confErr != nil {
		return false, confErr
	}
	return !shown, nil
}

// roles is the roles among roles, each named by its claim's key, that work for one of the scope's
// projects (roleOfProjects), in their order: roles itself for a nil scope.
func (s *edgeScope) roles(roles []string) []string {
	if s == nil {
		return roles
	}
	return slices.DeleteFunc(slices.Clone(roles), func(role string) bool { return !roleOfProjects(role, s.projects) })
}

// topics is the topics among topics, those of sessionID's interest record, that the scope shows
// (edgeShownTopics): topics itself for a nil scope.
func (s *edgeScope) topics(sessionID string, topics []string) []string {
	if s == nil {
		return topics
	}
	return edgeShownTopics(s.repositories, s.projects, sessionID, topics)
}

// showsAgentConfirmed reports whether the scope shows a Legion agent registry entry with the agent's session confirmed by its stored record: the
// issue the entry works is of one of the scope's projects, and the entry's session is a session of
// that issue's project, confirmed by its stored record (sessionInProject, through
// session.SessionRegistry.StoredByWriter), so a record the server wrote reads as no session. An
// entry is a record any writer of the registry's bucket can store, naming any session under any
// issue, so the issue it names says nothing of whose session it names: a chosen entry naming an
// in-house session under an outside project's issue would otherwise show that session's id, a title
// its writer chose and its live state to the outside session. Its replacement (replaced_by,
// replaced_at) is read from the other entries of its role token, records of the same kind, so it is
// shown only when it too is a session of that project, and answered as none otherwise. A
// confirmation read that did not answer is returned as an error that names no session
// (confirmUnanswered), so the route answers the bus-unavailable 503. A nil scope shows every entry.
func (s *edgeScope) showsAgentConfirmed(ctx context.Context, sessions *session.SessionRegistry, agent legionagents.Agent) (legionagents.Agent, bool, error) {
	if s == nil {
		return agent, true, nil
	}
	project, ok := contracts.IssueProject(agent.Issue)
	if !ok || !slices.Contains(s.projects, project) {
		return legionagents.Agent{}, false, nil
	}
	shown, err := sessionInProject(ctx, sessions, agent.SessionID, project)
	if err != nil {
		return legionagents.Agent{}, false, s.confirmUnanswered(agent.SessionID, err)
	}
	if !shown {
		return legionagents.Agent{}, false, nil
	}
	if agent.ReplacedBy != nil {
		replaced, err := sessionInProject(ctx, sessions, *agent.ReplacedBy, project)
		if err != nil {
			return legionagents.Agent{}, false, s.confirmUnanswered(*agent.ReplacedBy, err)
		}
		if !replaced {
			agent.ReplacedBy, agent.ReplacedAt = nil, nil
		}
	}
	return agent, true, nil
}

// confirmUnanswered is err, the error of a leader read confirming sessionID's stored record
// (session.SessionRegistry.StoredByWriter, through showsConfirmed or sessionInProject) that did not
// answer, as an edge answer gives it. err's own text names sessionID, a session the read did not
// confirm and the request may not have named: a role's holder, a row of the session list, a Legion
// agent's session or its replacement, any of which can be another project's session whose record
// the server re-published under one of the token's projects. So the answer is in the listener's own
// words, which name no session (edgeHolderHidden), with err beneath, which still decides the
// answer's status and Retry-After (writeBusUnavailable, writeLegionAgentError), and err goes to the
// scope's log with the session it names.
func (s *edgeScope) confirmUnanswered(sessionID string, err error) error {
	s.logger.Warn("edge listener could not confirm a session's stored record from the stream's leader; its answer names no session",
		slog.String("session_id", sessionID), slog.String("error", err.Error()))
	return &edgeHolderHidden{text: "confirm a session's stored record from the bucket's stream leader", err: err}
}

// agentsConfirmed is agents with only the entries whose session the scope shows, each confirmed (showsAgentConfirmed), in their
// order, without those it shows not. A confirmation read that did not answer ends the listing with
// its error, which the route answers as the bus-unavailable 503. A nil scope returns agents as it
// is.
func (s *edgeScope) agentsConfirmed(ctx context.Context, sessions *session.SessionRegistry, agents []legionagents.Agent) ([]legionagents.Agent, error) {
	if s == nil {
		return agents, nil
	}
	shown := agents[:0]
	for _, agent := range agents {
		agent, ok, err := s.showsAgentConfirmed(ctx, sessions, agent)
		if err != nil {
			return nil, err
		}
		if ok {
			shown = append(shown, agent)
		}
	}
	return shown, nil
}

// sessionInProject reports whether sessionID is a live session of project, confirmed by the record
// the stream's leader holds for its key (session.SessionRegistry.StoredByWriter): the cached entry
// lists project and is not gone (a cheap pre-check), and the leader's own record is a writer's
// registration of sessionID that lists project. sessionID is the key the decision is about, never
// the key a cached record names. A session the registry reads gone is no session this answers, as
// the README and the changelog say the edge shows no gone Legion agent even of the token's own
// project. A read that did not answer is returned as its error.
func sessionInProject(ctx context.Context, sessions *session.SessionRegistry, sessionID, project string) (bool, error) {
	entry, state := sessions.Lookup(sessionID)
	if entry.SessionID == "" || state == session.Gone || !slices.Contains(entry.Projects, project) {
		return false, nil
	}
	stored, ok, err := sessions.StoredByWriter(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return ok && stored.SessionID != "" && slices.Contains(stored.Projects, project), nil
}

// holderConfirmed is result, the resolution of a role's holder, as the scope answers it with the
// leader read every other edge decision uses: a role no claim holds (roles.Unclaimed) is returned
// as it is, and a claimed role's holder is named only when the stream's leader confirms the record
// it holds for the holder's key is a writer's registration the scope shows (showsConfirmed,
// session.SessionRegistry.StoredByWriter). So a record the server wrote at the holder's key, and
// one a cache that trails the bucket still shows under projects a newer write dropped, each read as
// a role no claim holds, which names no holder, process, state, last registration, hand-back or
// claim revision: a holder outside the scope's projects is another project's session, whose id and
// activity the scope shows nobody. A lapsed holder whose session record is gone has no projects to
// show, so it reads as unclaimed too, even where it was a session of one of the scope's projects:
// the rule fails closed, and a claim of the role still takes it from the holder that stopped
// answering. The holder's key is result.Holder, the id the claim names, never the key a cached
// record names. A read that did not answer is returned as an error that names no session
// (confirmUnanswered), so the role read answers the bus-unavailable 503. A nil scope, and a role no
// claim holds, both cost no read.
func (s *edgeScope) holderConfirmed(ctx context.Context, sessions *session.SessionRegistry, result roles.Resolution) (roles.Resolution, error) {
	if result.State == roles.Unclaimed {
		return result, nil
	}
	shown, err := s.showsConfirmed(ctx, sessions, result.Holder, result.Entry)
	if err != nil {
		return roles.Resolution{}, err
	}
	if !shown {
		return roles.Resolution{State: roles.Unclaimed}, nil
	}
	return result, nil
}

// namesHolderConfirmed reports whether a 409 may name holderID, the holder of a role an answer
// names with no resolution beside it: the record the stream's leader holds for holderID is a
// writer's registration the scope shows (showsConfirmed, session.SessionRegistry.StoredByWriter),
// judged on the entry the cache holds of holderID. So a record the server wrote at the holder's
// key, and one a cache that trails the bucket still shows under projects a newer write dropped,
// each name no holder. holderID is the id the claim names, the key the decision is about. A read
// that did not answer is returned as an error that names no session (confirmUnanswered). A nil
// scope names every holder with no read.
func (s *edgeScope) namesHolderConfirmed(ctx context.Context, sessions *session.SessionRegistry, holderID string) (bool, error) {
	entry, _ := sessions.Lookup(holderID)
	return s.showsConfirmed(ctx, sessions, holderID, entry)
}

// unresolved is err, the error of a read of role's holder that failed, as an answer the scope
// holds gives it: a resolution's read of the holder's session that did not answer
// (roles.ReadError at roles.SessionRead, from GET /v1/roles/<role> and a role send), and a soft
// claim's read of the holder's liveness or of the declared predecessor's registration
// (holderSessionUnread, from POST /v1/roles/set). Each names that session in its text, a session
// the scope may not show, so the scope answers it in the listener's own words, which name the
// role and no holder, and logger gets the read's own error (hideHolder). Every other error names
// no holder, and is answered as it is. A nil scope answers err as it is.
func (s *edgeScope) unresolved(role string, err error, logger *logging.Logger) error {
	if s == nil {
		return err
	}
	var resolution *roles.ReadError
	var claimRead *holderSessionUnread
	holder := ""
	switch {
	case errors.As(err, &resolution) && resolution.Step == roles.SessionRead:
		holder = resolution.Holder
	case errors.As(err, &claimRead):
		holder = claimRead.holder
	default:
		return err
	}
	return hideHolder(logger, "edge listener could not read a role holder's session; its answer names no holder",
		role, holder, "read the holder session of role "+role, err)
}

// hideHolder is err, whose text names holder, a holder of role an edge answer may not name, as
// that answer gives it (edgeHolderHidden): text, the listener's own words, and err beneath, which
// decides the answer's status and Retry-After (writeBusUnavailable). logger gets line with err.
func hideHolder(logger *logging.Logger, line, role, holder, text string, err error) error {
	logger.Warn(line, slog.String("role", role), slog.String("holder_session_id", holder), slog.String("error", err.Error()))
	return &edgeHolderHidden{text: text, err: err}
}

// edgeHolderHidden is the error an edge answer gives a failure whose own text names a holder or a
// session the scope may not show (hideHolder, edgeScope.confirmUnanswered): its text is the
// listener's own, and it unwraps to the failure, which decides how the answer is written.
type edgeHolderHidden struct {
	text string
	err  error
}

func (e *edgeHolderHidden) Error() string { return e.text }

func (e *edgeHolderHidden) Unwrap() error { return e.err }

// roleOfProjects reports whether role, named by its claim's key, works for one of projects: the
// project its key gives (contracts.ParseRoleClaimKey) is one of them. It is the edge's rule for a
// role the session already holds, as roleNameOfProjects is for one it names in either form.
func roleOfProjects(role string, projects []string) bool {
	project, _ := contracts.ParseRoleClaimKey(role)
	return slices.Contains(projects, project)
}

// roleNameOfProjects is the claim role, named in either form, names (contracts.RoleClaimOf), and
// whether that claim works for one of projects. It is the edge's rule for a role a session claims,
// reads, publishes to or follows the lane of (edgeRoleRefusal, edgePublishRefusal, edgeTopicRule).
func roleNameOfProjects(role string, projects []string) (contracts.RoleClaimName, bool) {
	claim, err := contracts.RoleClaimOf(role)
	return claim, err == nil && slices.Contains(projects, claim.Project)
}

// edgeReleasedRoleWarning is the warning a registration's answer names a role by, one the edge
// released from the session because it works for none of the token's projects
// (edgeTopicHold.releaseRefusedRoles).
func edgeReleasedRoleWarning(role string, projects []string) string {
	return fmt.Sprintf("role %s was released and not renewed: a session at the edge holds only roles of %s", role, strings.Join(projects, ", "))
}

// edgeReleaseFailedPrefix opens the 503 of a registration the edge refused because the bus
// answered the release of the session's claim of another project's role with a record no read can
// decode, or refused that release (writeEdgeReleaseRefusal), so one string matches in a test and
// in a deployed log filter. It is deliberately not busUnansweredPrefix: the bus answered, and an
// operator reading "the message bus did not answer" would go looking for an outage instead of at
// the claim record the rest of the text names.
const edgeReleaseFailedPrefix = "envoy: a role claim of a project the session's token does not list could not be released and stands, so the session is not registered here"

// writeEdgeReleaseRefusal answers a registration whose release of the session's claims of other
// projects' roles failed (edgeTopicHold.releaseRefusedRoles, err) with the retryable 503 either
// way, since the claim stands and the session may not be served while it does. A bus that did not
// answer is the bus-unavailable 503 (writeBusUnavailable); a release the bus answered - a claim
// record no read can decode, a refused key - names that record after edgeReleaseFailedPrefix,
// with Retry-After: 1 as well, since another listener may rewrite or free the claim meanwhile.
func writeEdgeReleaseRefusal(ctx context.Context, w http.ResponseWriter, err error) {
	if !storeAnswered(ctx, err) {
		writeBusUnavailable(w, err)
		return
	}
	w.Header().Set("Retry-After", "1")
	writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s: %s", edgeReleaseFailedPrefix, err))
}

// holdsRole reports whether the hold's session may hold role, named by its claim's key: a role of
// one of the token's projects (roleOfProjects), and every role for a nil hold, the registration
// of a request no edge listener admitted. It is the one rule the release
// (releaseRefusedRoles) and the renewal (store.Registry.RenewRoleClaims) of a registration read.
func (h *edgeTopicHold) holdsRole(role string) bool {
	return h == nil || roleOfProjects(role, h.projects)
}

// releaseRefusedRoles releases every claim the hold's session holds of a role holdsRole refuses
// (store.Registry.ReleaseRefusedRoleClaims, which weighs the role lanes of the session's stored row
// as this listener's copy of it holds them and the roles the role claim copy names, and reads the
// role bucket while that copy cannot be served), and answers the roles it released. Its error is
// every release that failed. A nil hold releases nothing.
func (h *edgeTopicHold) releaseRefusedRoles(ctx context.Context, registry *store.Registry) ([]string, error) {
	if h == nil {
		return nil, nil
	}
	released, err := registry.ReleaseRefusedRoleClaims(ctx, h.sessionID, h.holdsRole)
	for _, role := range released {
		h.logger.Warn("edge listener released a role claim of a project the session's token does not list",
			slog.String("session_id", h.sessionID), slog.String("role", role), slog.Any("projects", h.projects))
	}
	return released, err
}

// unknownSession is the 404 a session the edge does not show gets: the answer of a session no
// listener has heard of, naming no state and no last registration, so it tells the caller
// nothing of a session of another project.
func unknownSession(sessionID string) listenerwire.NoLiveSessionRefusal {
	return listenerwire.NoLiveSessionRefusal{
		Error: fmt.Sprintf("no live session %s; this listener does not know when it last registered", sessionID),
		State: session.Gone,
	}
}

// edgeTopicHold holds the topics of one registration of sessionID at the edge to projects, those
// of the token that made it: each topic is the session's own agent subject in either form, a
// topic contracts.SubjectProject gives one of projects, or a GitHub topic of one repository that
// the broker's answer for projects names, read fresh for the registration once, however many of
// its topics and plans ask (answer). It judges the topics the registration names before anything
// is planned (refusal), and every topic of each plan after (holdPlan): a plan merges the row's
// stored topics in, which another listener may have written under a token of other projects, or
// this one under a token that listed more. A nil hold holds nothing: the registration of a
// request no edge listener admitted.
type edgeTopicHold struct {
	sessionID    string
	projects     []string
	repositories *edgeRepositories
	logger       *logging.Logger
	// answered is the repositories the registration's fresh read named, once read is set.
	answered []string
	read     bool
}

// newEdgeTopicHold is the hold of r's registration of sessionID, nil for a request no edge
// listener admitted. logger names the cause of a read that gave no whole answer, which the
// client's 503 does not (answerRepositoryOutage).
func newEdgeTopicHold(r *http.Request, repositories *edgeRepositories, sessionID string, logger *logging.Logger) *edgeTopicHold {
	projects, held := heldProjects(r)
	if !held {
		return nil
	}
	return &edgeTopicHold{sessionID: sessionID, projects: projects, repositories: repositories, logger: logger}
}

// edgeTopicRule is the edge's rule for one topic of sessionID's under projects, so far as it
// needs no repository answer: the reason a refusal of it carries, "" for a topic it takes, and for
// a GitHub topic of one repository, which only an answer can take, that repository's owner and
// name as the topic's tokens write them. A GitHub topic naming no single repository is refused
// repository_not_in_project; another session's agent subject, the listener's own
// notifications.envoy. lanes and every subject of no project are refused topic_not_in_project. A
// role lane naming one role is read as the claim it names (contracts.RoleClaimOf), as the claim,
// the publish and the role lane read it, so a lane naming under one of projects a Legion role of
// another project is refused; a role lane with a wildcard is read for its project as every other
// topic is (contracts.SubjectProject), a wildcard where the project would be naming none.
func edgeTopicRule(sessionID string, projects []string, topic string) (reason, owner, name string) {
	switch {
	case ownAgentSubject(sessionID, projects, topic):
		return "", "", ""
	case strings.HasPrefix(topic, contracts.GithubTopicPrefix):
		if owner, name, single := githubSubjectRepository(topic); single {
			return "", owner, name
		}
		return edgeRepositoryNotInProject, "", ""
	case strings.HasPrefix(topic, contracts.AgentTopicPrefix), strings.HasPrefix(topic, listenerLanePrefix):
		return edgeTopicNotInProject, "", ""
	}
	if lane, isRole := strings.CutPrefix(topic, contracts.RoleTopicPrefix); isRole && !strings.ContainsAny(lane, "*>") {
		if _, ok := roleNameOfProjects(lane, projects); !ok {
			return edgeTopicNotInProject, "", ""
		}
		return "", "", ""
	}
	if project, _, ok := contracts.SubjectProject(topic); !ok || !slices.Contains(projects, project) {
		return edgeTopicNotInProject, "", ""
	}
	return "", "", ""
}

// refused is the topics among topics the hold refuses, each once, in their order, and the reason
// a refusal naming them carries: repository_not_in_project when each is a GitHub topic,
// topic_not_in_project otherwise (edgeTopicRule). A GitHub topic of one repository is refused
// repository_not_in_project when the answer does not name its repository (repositoryAmong). A read
// that gave no whole answer is the error (errRepositoriesUnknown), and no topic is judged.
func (h *edgeTopicHold) refused(ctx context.Context, topics []string) ([]string, string, error) {
	type githubTopic struct{ topic, owner, name string }
	reasons := make(map[string]string, len(topics))
	var github []githubTopic
	for _, topic := range topics {
		if _, judged := reasons[topic]; judged {
			continue
		}
		reason, owner, name := edgeTopicRule(h.sessionID, h.projects, topic)
		reasons[topic] = reason
		if owner != "" {
			github = append(github, githubTopic{topic: topic, owner: owner, name: name})
		}
	}
	if len(github) > 0 {
		answered, err := h.answer(ctx)
		if err != nil {
			return nil, "", err
		}
		for _, topic := range github {
			if !repositoryAmong(answered, topic.owner, topic.name) {
				reasons[topic.topic] = edgeRepositoryNotInProject
			}
		}
	}
	var refused []string
	reason := edgeRepositoryNotInProject
	for _, topic := range topics {
		if reasons[topic] == "" || slices.Contains(refused, topic) {
			continue
		}
		refused = append(refused, topic)
		if reasons[topic] == edgeTopicNotInProject {
			reason = edgeTopicNotInProject
		}
	}
	return refused, reason, nil
}

// answer is the repositories the broker's answer for the hold's projects names, read fresh within
// ctx the first time a topic asks and kept for the rest of the registration. The read ends
// edgeRegistrationReserve before ctx does, so a broker that does not answer leaves the
// registration the time its own writes need: the renewal of the session's entry a refused
// registration still makes (renewThroughOutage).
func (h *edgeTopicHold) answer(ctx context.Context) ([]string, error) {
	if h.read {
		return h.answered, nil
	}
	if h.repositories == nil {
		return nil, fmt.Errorf("%w: this listener reads no repository answer", errRepositoriesUnknown)
	}
	readCtx := ctx
	if deadline, bounded := ctx.Deadline(); bounded {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithDeadline(ctx, deadline.Add(-edgeRegistrationReserve))
		defer cancel()
	}
	answered, err := h.repositories.readFresh(readCtx, h.projects)
	if err != nil {
		return nil, err
	}
	h.answered, h.read = answered, true
	return answered, nil
}

// edgeRegistrationReserve is how much of a registration's budget (sendRequestBudget) the edge's
// repository read leaves unspent: a bucket read and a write of the session's entry, each of which
// answers in milliseconds on a bus that answers at all.
const edgeRegistrationReserve = time.Second

// refusal is the hold of the topics the registration names, or nil: a 403 naming every refused
// topic (listenerwire.EdgeRefusal.Refused), so the client drops exactly those and registers the
// rest again. A read that gave no whole answer is the error (errRepositoriesUnknown), as holdPlan
// returns it, and the route answers it with answerRepositoryOutage. Nothing is stored for a
// refused registration.
func (h *edgeTopicHold) refusal(ctx context.Context, topics []string) (*edgeRefusal, error) {
	if h == nil {
		return nil, nil
	}
	refused, reason, err := h.refused(ctx, topics)
	if err != nil || len(refused) == 0 {
		return nil, err
	}
	refusal := refuseEdge(http.StatusForbidden, reason, fmt.Sprintf("%s not followed here: %s",
		strings.Join(refused, ", "), edgeFollowedTopics(h.projects)), "topics")
	refusal.body.Refused = refused
	return refusal, nil
}

// holdPlan is plan without the topics the hold refuses, and those topics: out of the session
// consumer's filter, and out of the row its write stores (store.InterestPlan.Withheld), which
// takes them out of the row it reads before it merges the plan in. The topics the registration
// named passed refusal already, so what it drops is a topic of the stored row, which the answer's
// warnings name (registeredSession.dropped). It is asked of every plan of the registration, the
// one made again after a conflict among them, whose row another writer stored since. A read that
// gave no whole answer is the error, which the route answers with answerRepositoryOutage.
func (h *edgeTopicHold) holdPlan(ctx context.Context, plan store.InterestPlan) (store.InterestPlan, []string, error) {
	if h == nil {
		return plan, nil, nil
	}
	refused, _, err := h.refused(ctx, plan.Topics)
	if err != nil {
		return store.InterestPlan{}, nil, err
	}
	if len(refused) > 0 {
		plan.Topics = slices.DeleteFunc(slices.Clone(plan.Topics), func(topic string) bool { return slices.Contains(refused, topic) })
		plan.Withheld = refused
	}
	return plan, refused, nil
}

// answerRepositoryOutage answers a registration the edge refused because its repository read,
// err, gave no whole answer (refusal or holdPlan, errRepositoriesUnknown). Such a registration
// renews no role claim, which is recorded as every registration that renewed nothing is
// (store.Registry.RecordRenewalFailures), and renews the session's entry alone
// (renewThroughOutage), so the session outlives the outage. The answer is the 503
// repositories_unknown: it names no topic, since the edge cannot say which of the GitHub topics
// to refuse, and its text is the listener's own, since the broker's answer, which err may quote,
// goes to the log alone.
func (h *edgeTopicHold) answerRepositoryOutage(ctx context.Context, w http.ResponseWriter, registry *store.Registry, sessions *session.SessionRegistry, machineID string, err error) {
	h.logger.Warn("edge listener could not read the repositories of the session's projects; the registration is refused 503",
		slog.String("session_id", h.sessionID), slog.Any("projects", h.projects), slog.String("error", err.Error()))
	registry.RecordRenewalFailures(h.sessionID, err)
	h.renewThroughOutage(ctx, sessions, machineID)
	refuseEdge(http.StatusServiceUnavailable, edgeRepositoriesUnknown,
		fmt.Sprintf("the repositories of %s's issues could not be read, so no GitHub topic is followed now; register again shortly", strings.Join(h.projects, ", "))).write(w)
}

// renewThroughOutage renews the session entry of a registration the edge refused for want of a
// repository answer (answerRepositoryOutage), so a session whose row follows a GitHub topic stays
// registered, and keeps receiving its other messages, for as long as the broker cannot answer:
// every heartbeat it sends meanwhile names that topic and is refused, and an entry no heartbeat
// renews is gone at the session bucket's maximum age. Its GitHub frames are withheld meanwhile
// (sessionRelays.edgeWithholds), and nothing else of the registration is stored. The entry is
// written back as the session bucket holds it, stamped now, with its gap mark, at the revision
// read (session.SessionRegistry.RenewWithin), and only while it is this listener's own
// registration of the session under the same projects: an entry another listener wrote, one no
// registration wrote, and one another write replaced since the read and that fails the check when
// it is read again are left as they are. A read or a write that fails is logged, and the refusal
// stands either way.
func (h *edgeTopicHold) renewThroughOutage(ctx context.Context, sessions *session.SessionRegistry, machineID string) {
	projects := slices.Sorted(slices.Values(h.projects))
	owns := func(entry session.SessionEntry) bool {
		return entry.MachineID == machineID && slices.Equal(slices.Sorted(slices.Values(entry.Projects)), projects)
	}
	if err := sessions.RenewWithin(ctx, h.sessionID, owns); err != nil {
		h.logger.Warn("edge listener could not renew the session's entry through the repository outage",
			slog.String("session_id", h.sessionID), slog.String("error", err.Error()))
	}
}

// edgeFollowedTopics is the sentence that says what a session at the edge follows, which a
// refused registration and a dropped stored topic name.
func edgeFollowedTopics(projects []string) string {
	list := strings.Join(projects, ", ")
	return fmt.Sprintf("a session at the edge follows its own agent subject, the topics of %s, and the GitHub topics of one repository each that an issue of %s resolves to", list, list)
}

// edgeDroppedWarning is the warning a registration's answer names a stored topic by, one the hold
// dropped from the session's interests (holdPlan).
func edgeDroppedWarning(topic string, projects []string) string {
	return fmt.Sprintf("%s was removed from the session's interests: %s", topic, edgeFollowedTopics(projects))
}

// edgeShownTopics is the topics among topics, those of sessionID's interest record, that a read
// held to projects shows: each topic edgeTopicRule takes for sessionID under projects, a GitHub
// topic of one repository only while a fresh answer of projects names it
// (edgeRepositories.standing). A topic another listener stored for the session under another
// project is shown to no reader at the edge. It returns a new slice and leaves topics as it is.
func edgeShownTopics(repositories *edgeRepositories, projects []string, sessionID string, topics []string) []string {
	var standing []string
	if repositories != nil {
		standing, _ = repositories.standing(projects)
	}
	shown := make([]string, 0, len(topics))
	for _, topic := range topics {
		reason, owner, name := edgeTopicRule(sessionID, projects, topic)
		if reason == "" && (owner == "" || repositoryAmong(standing, owner, name)) {
			shown = append(shown, topic)
		}
	}
	return shown
}

// listenerLanePrefix begins every lane of the listener's own (the kept, resume and exception
// lanes, the relay subjects and their receipts), which no session follows at the edge.
const listenerLanePrefix = bus.NotificationTopicPrefix + "envoy."

// ownAgentSubject reports whether topic is sessionID's own agent subject in either form: the old
// form, the new form under every project (contracts.CanonicalTopic writes it with a wildcard), or
// the new form under one of projects.
func ownAgentSubject(sessionID string, projects []string, topic string) bool {
	if topic == contracts.LegacyAgentSubject(sessionID) {
		return true
	}
	rest, found := strings.CutPrefix(topic, contracts.AgentTopicPrefix)
	if !found {
		return false
	}
	project, target, found := strings.Cut(rest, ".")
	return found && target == sessionID && (project == "*" || slices.Contains(projects, project))
}

// edgePublishRefusal is the edge's publish rule, or nil: a session at the edge publishes only a
// role subject in the form that names the project (notifications.role.<KEY>.<role>), of one of
// its token's projects, for a role that works for that project (contracts.RoleClaimOf): a Legion
// role of the project, or a free-form role under it, whose claim is keyed on the project. A Legion
// role of another project named under this one is refused, since its claim is its own project's.
// A message to a session goes through the send route, and nothing else is published here: a
// Dispatch event of the project, a GitHub, Slack or team subject, and a lane of the listener's own
// are 403 topic_not_published.
func edgePublishRefusal(r *http.Request, topic string) *edgeRefusal {
	projects, held := heldProjects(r)
	if !held {
		return nil
	}
	if lane, isRole := strings.CutPrefix(topic, contracts.RoleTopicPrefix); isRole {
		if claim, ok := roleNameOfProjects(lane, projects); ok && !claim.Legacy {
			return nil
		}
	}
	return refuseEdge(http.StatusForbidden, edgeTopicNotPublished,
		fmt.Sprintf("%s is not published here: a session at the edge publishes only a role subject of %s, notifications.role.<project>.<role> for a role of that project, and sends to a session through /v1/messages/send",
			topic, strings.Join(projects, ", ")), "topic")
}

// edgeSenderRefusal is the edge's rule for who a send or a publish says it is from, or nil: a
// session at the edge sends as itself and as nothing else, so the message's source is agent, or
// left out (contracts.MessageEnvelope writes agent for none), and its source_session is the
// session the token names. A message under another source (dispatch, human, github and the
// rest), or with no source session, would reach its reader as an event or a person's message no
// session sent; the edge refuses it 403 sender_not_session. Every other listener lets a message
// name no source session, and sessionNotBound alone holds one that names another.
func edgeSenderRefusal(r *http.Request, source, sourceSession string) *edgeRefusal {
	if _, held := heldProjects(r); !held {
		return nil
	}
	caller, _ := brokerCaller(r)
	if (source == "" || source == "agent") && sourceSession != "" && sourceSession == caller.Session {
		return nil
	}
	return refuseEdge(http.StatusForbidden, edgeSenderNotSession,
		fmt.Sprintf("a session at the edge sends as itself: source agent, or none, and source_session %s", caller.Session), "source", "source_session")
}

// edgeRoleRefusal is the edge's role rule for a claim or a read of role, in either form, or nil:
// the project the claim works for (contracts.RoleClaimOf) is one of the token's. What passes is
// claimed and read under the key of that project: the bare name for a Legion role, and
// <project>.<role> for a free-form role under the project.
func edgeRoleRefusal(r *http.Request, role string) *edgeRefusal {
	projects, held := heldProjects(r)
	if !held {
		return nil
	}
	if _, ok := roleNameOfProjects(role, projects); ok {
		return nil
	}
	return refuseEdge(http.StatusForbidden, edgeRoleNotInProject,
		fmt.Sprintf("role %s is not claimed or read here: it works for no project of %s", role, strings.Join(projects, ", ")), "role")
}

// edgeInterestReadRefusal is the edge's rule for GET /v1/interests/ and GET
// /v1/interests/<session_id>, or nil: a session at the edge reads its own interest record and no
// other, and lists none; the record it reads shows the topics of its token's projects alone
// (edgeShownTopics).
func edgeInterestReadRefusal(r *http.Request, sessionID string) *edgeRefusal {
	if _, held := heldProjects(r); !held {
		return nil
	}
	if caller, ok := brokerCaller(r); ok && sessionID != "" && caller.Session == sessionID {
		return nil
	}
	return refuseEdge(http.StatusForbidden, edgeSessionNotInProject, "a session at the edge reads its own interests and no other session's")
}

// edgeLegionWriteRefusal is the edge's rule for a write of the Legion agent registry, or nil: the
// edge records no Legion agent, since a Legion daemon writes the registry through a listener of
// its own.
func edgeLegionWriteRefusal(r *http.Request) *edgeRefusal {
	if _, held := heldProjects(r); !held {
		return nil
	}
	return refuseEdge(http.StatusForbidden, edgeLegionRegistryNotWritten, "the Legion agent registry is not written at the edge")
}

// registeringProjects is the projects the registration r makes records on its session's entry
// (session.SessionEntry.Projects): its broker token's list, sorted and each once as the broker
// answers it (the caller's own copy, ciphertoken.Caller.Projects), and [] for a registration that
// presented no broker token or one listing none.
func registeringProjects(r *http.Request) []string {
	if caller, ok := brokerCaller(r); ok && len(caller.Projects) > 0 {
		return caller.Projects
	}
	return []string{}
}

// The variables an edge listener refuses to start beside: each admits a caller by something other
// than a broker token, or serves a route the edge does not. ENVOY_DISPATCH_PRINCIPAL is among
// them: it names the one identity-token caller whose send may carry a role mark (sendHandler), and
// Dispatch sends through a shared listener, as no session of the edge sends as Dispatch.
var edgeRefusedSettings = []string{
	"ENVOY_API_TOKEN",
	"ENVOY_DISPATCH_PRINCIPAL",
	"ENVOY_WEBHOOKS",
	"ENVOY_GITHUB_WEBHOOK_SECRET",
	"ENVOY_SLACK_SIGNING_SECRET",
	"ENVOY_GHOSTWISPR_SIGNING_SECRET",
}

// edgeRefusedPrefix begins every setting of the identity-token credentials (ENVOY_OIDC_ISSUER,
// ENVOY_OIDC_AUDIENCE, ENVOY_OIDC_ACCOUNTS, ENVOY_OIDC_GOOGLE_AUDIENCE, ENVOY_OIDC_GOOGLE_ACCOUNTS
// and any later one), which an edge listener refuses to start beside, as a group.
const edgeRefusedPrefix = "ENVOY_OIDC_"

// validateEdgeMode is the edge's start rule: an edge listener, one serving edgeProjects, refuses
// to start beside the shared token, any ENVOY_OIDC_* setting, ENVOY_DISPATCH_PRINCIPAL, the
// webhooks or a webhook secret, or with the broker-token settings incomplete or unset, since it
// admits a broker token alone; each refusal names the variable. environ is the process's
// environment as os.Environ writes it. A listener serving no project is not checked.
func validateEdgeMode(edgeProjects []string, environ []string) error {
	if len(edgeProjects) == 0 {
		return nil
	}
	env := make(map[string]string, len(environ))
	var oidc []string
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
		if strings.HasPrefix(name, edgeRefusedPrefix) && strings.TrimSpace(value) != "" {
			oidc = append(oidc, name)
		}
	}
	slices.Sort(oidc)
	refuse := func(names ...string) error {
		return fmt.Errorf("%s set beside %s=%s: an edge listener admits a broker token alone and serves no webhook, so unset it",
			strings.Join(names, ", "), config.EdgeProjectsEnvVar, strings.Join(edgeProjects, ","))
	}
	for _, name := range edgeRefusedSettings {
		if strings.TrimSpace(env[name]) != "" {
			return refuse(name)
		}
	}
	if len(oidc) > 0 {
		return refuse(oidc...)
	}
	broker, err := resolveListenerBrokerConfig(func(name string) string { return env[name] })
	if err != nil {
		return fmt.Errorf("%s: %w", config.EdgeProjectsEnvVar, err)
	}
	if !broker.configured() {
		return fmt.Errorf("%s is set and %s is not: an edge listener admits a broker token alone, so it needs %s, %s or %s, and %s",
			config.EdgeProjectsEnvVar, EnvBrokerURL, EnvBrokerURL, EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvPublicURL)
	}
	return nil
}

// runEdgeRepositories runs the edge listener's repository refresh until ctx ends: one read of
// every project's answer at once, then a pass on each tick (every edgeRepositoryRefresh in main).
// A pass first removes the interests that the answers read before it no longer cover
// (pruneEdgeInterests), then reads every answer again, so a repository that left a project stops
// reaching its sessions at the pass that reads the new answer (the relay's verdict) and leaves
// their interests at the pass after: within two refreshes of the change, and never on the word of
// an answer that is not fresh.
func runEdgeRepositories(ctx context.Context, d *listenerDeps, machineID string, ticks <-chan time.Time) {
	d.repositories.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		pruneEdgeInterests(ctx, d, machineID)
		d.repositories.refresh(ctx)
		d.repositories.passes.Add(1)
	}
}

// pruneEdgeInterests removes, from every interest record this listener registered, each GitHub
// topic whose repository no fresh answer of the projects the token this replica admitted the
// session on names (edgeAdmissions, never the stored record, which a RePublish can list another
// project on), and refollows that session's consumer, as an unsubscribe does. A session this
// replica admitted none of is skipped, since nothing here registered it. A session one of whose
// admitted projects has no fresh answer keeps every topic: an answer that is not there says nothing
// of a repository's leaving, and the relay withholds the session's GitHub frames meanwhile. A
// record it cannot list, or a removal that fails, is left to the next pass. Each run of such
// failures is logged when it starts rather than at every pass: a failed list after a list that
// answered (notePruneList), and a session's failed removal after a pass that weighed the session's
// topics and removed them, found none leaving, or did not list the session
// (edgeRepositories.unremoved).
func pruneEdgeInterests(ctx context.Context, d *listenerDeps, machineID string) {
	logger := d.repositories.logger
	items, err := d.registry.List()
	if !d.repositories.notePruneList(err) {
		return
	}
	d.repositories.mu.Lock()
	failing := d.repositories.unremoved
	d.repositories.mu.Unlock()
	unremoved, listed := map[string]bool{}, map[string]bool{}
	// ended logs the end of sessionID's run of failed removals that no removal ended, and why.
	ended := func(sessionID, why string) {
		logger.Info("edge listener no longer removes the interests of a repository that left the session's projects, after its removals failed",
			slog.String("session_id", sessionID), slog.String("why", why))
	}
	for _, item := range items {
		if failing[item.SessionID] {
			listed[item.SessionID] = true
		}
		if item.MachineID != machineID {
			if failing[item.SessionID] {
				ended(item.SessionID, "the session's interests are another listener's")
			}
			continue
		}
		projects, admitted := d.consumers.admissions.of(item.SessionID)
		repositories, complete := d.repositories.standing(projects)
		if !admitted || !complete {
			// Nothing of the session's topics was weighed, so a run of failed removals goes on.
			if failing[item.SessionID] {
				unremoved[item.SessionID] = true
			}
			continue
		}
		var leaving []string
		for _, topic := range item.Topics {
			if !strings.HasPrefix(topic, contracts.GithubTopicPrefix) {
				continue
			}
			if owner, name, single := githubSubjectRepository(topic); !single || !repositoryAmong(repositories, owner, name) {
				leaving = append(leaving, topic)
			}
		}
		if len(leaving) == 0 {
			// Nothing leaving: the answer names the repository again, or the session itself dropped
			// the topic (an unsubscribe, or a registration after a refusal). The pass cannot tell
			// which, so the why says what it found.
			if failing[item.SessionID] {
				ended(item.SessionID, "no interest of the session is outside its projects' repositories now")
			}
			continue
		}
		removeCtx, cancel := context.WithTimeout(ctx, sendRequestBudget)
		removed, err := d.registry.Remove(removeCtx, item.SessionID, leaving, store.RoleReleaseAuthority{})
		if err != nil {
			cancel()
			unremoved[item.SessionID] = true
			if !failing[item.SessionID] {
				logger.Warn("edge listener could not remove the interests of a repository that left the session's projects; every refresh tries again while the session holds them, and the end of the run is logged, a removal's line or one saying why no removal is needed",
					slog.String("session_id", item.SessionID), slog.Any("topics", leaving), slog.String("error", err.Error()))
			}
			continue
		}
		recordFollowedTopics(removeCtx, d, item.SessionID, logger)
		cancel()
		d.consumers.refollow(ctx, d, item.SessionID)
		logger.Info("edge listener removed the interests of a repository that left the session's projects",
			slog.String("session_id", item.SessionID), slog.Any("topics", removed))
	}
	for sessionID := range failing {
		if !listed[sessionID] {
			ended(sessionID, "the session's interest record is gone")
		}
	}
	d.repositories.mu.Lock()
	d.repositories.unremoved = unremoved
	d.repositories.mu.Unlock()
}

// notePruneList reports whether a refresh pass's list of the interest records, which ended with
// err, answered, and logs what it starts or ends: a WARN for a failed list after one that answered,
// an INFO for a list that answers after failed ones, and nothing otherwise, so a copy of the
// interest bucket that stays stale logs two lines, not one at every refresh.
func (e *edgeRepositories) notePruneList(err error) bool {
	e.mu.Lock()
	failed := e.unlisted
	if err != nil {
		e.unlisted++
	} else {
		e.unlisted = 0
	}
	e.mu.Unlock()
	switch {
	case err != nil && failed == 0:
		e.logger.Warn("edge listener could not list the interests a repository may have left; it lists them again at every refresh and logs again once a list answers",
			slog.String("error", err.Error()))
	case err == nil && failed > 0:
		e.logger.Info("edge listener lists the interests again; a repository that left a session's projects leaves its interests from this pass on",
			slog.Int("failed_passes", failed))
	}
	return err == nil
}
