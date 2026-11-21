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
//     in a role read, a role send's answers and a claim's 409 (edgeScope.holder,
//     edgeScope.namesHolder). A registration first releases the session's claims of other projects'
//     roles, refused 503 when a release fails (edgeTopicHold.releaseRefusedRoles), renews its
//     token's projects' roles alone (edgeTopicHold.holdsRole), and a claimant is handed the kept
//     messages of those alone (edgeScope.roles).
//   - Sends: the target session's token lists one of the token's projects, else the send is
//     answered as to a missing session (edgeScope.hides).
//   - Reads: the session list, the registry, the session rows' roles and topics and the Legion
//     agent routes answer only sessions, roles, topics and agents of the token's projects
//     (edgeScope), each session shown confirmed by its stored record read from the stream's leader
//     (edgeScope.showsConfirmed, session.SessionRegistry.StoredByWriter), so a record the server
//     wrote at a session's key reads as no session's, and a read that did not answer is the
//     bus-unavailable 503; the interest list and another session's interests are 403, and the
//     Legion agent registry is not written here.
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

// withEdgeHold marks a request an edge listener admitted (apiAuth): every rule here holds it to
// the projects of the broker token it was admitted on, which its broker caller carries.
func withEdgeHold(ctx context.Context) context.Context {
	return context.WithValue(ctx, edgeHoldKey{}, true)
}

// heldProjects is the projects r is held to, its broker token's (ciphertoken.Caller.Projects, the
// caller's own copy), and whether it is held at all: true only for a request an edge listener
// admitted, false on every other listener.
func heldProjects(r *http.Request) ([]string, bool) {
	if held, _ := r.Context().Value(edgeHoldKey{}).(bool); !held {
		return nil, false
	}
	caller, _ := brokerCaller(r)
	return caller.Projects, true
}

// edgeScope is what a request an edge listener admitted may read: the projects of its token
// (heldProjects), and the repository reader that says which of their GitHub topics stand. It is
// nil for a request no edge listener admitted, and each method answers a nil scope as every
// listener answers, so a read route asks it unconditionally and writes no edge branch of its own;
// only the answers a session with no entry gets test the scope itself (hides).
type edgeScope struct {
	projects     []string
	repositories *edgeRepositories
}

// edgeScopeOf is r's scope, nil for a request no edge listener admitted. repositories may be nil
// for a route that reads no topic.
func edgeScopeOf(r *http.Request, repositories *edgeRepositories) *edgeScope {
	projects, held := heldProjects(r)
	if !held {
		return nil
	}
	return &edgeScope{projects: projects, repositories: repositories}
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

// hides reports whether the scope answers a session read as entry with err as one no listener has
// heard of (unknownSession): a session it does not show, and one the bucket holds no record of
// (nats.ErrKeyNotFound), which names no project. A read that did not answer is not hidden: its
// route answers it as every listener does. A nil scope hides nothing.
func (s *edgeScope) hides(entry session.SessionEntry, err error) bool {
	return s != nil && (errors.Is(err, nats.ErrKeyNotFound) || (err == nil && !s.shows(entry)))
}

// showsConfirmed is shows, and for a session it would show, confirmed by the session's newest
// message on the bucket's stream, read from the stream's leader with the headers stored with it
// (session.SessionRegistry.StoredByWriter): a record the server wrote at the session's key - a
// RePublish of another stream naming the key's subject, whose body that stream's writer chose -
// reads as a record naming its key, a machine, a time and any projects it likes, so an entry that
// would grant the session is granted only when the leader's newest message is a writer's. A message
// the server wrote, or none, reads as no session's, and so does a key the leader refused or held no
// message for (nats.ErrKeyNotFound, bus.ErrRefused). A read that did not answer is returned as its
// error, not as an absence, so a caller answers the bus-unavailable 503 it answers for any read
// that did not answer, never the 404 of a session it does not show nor a silent drop from a list. A
// nil scope shows every session, and an entry it would not show, both cost no read.
func (s *edgeScope) showsConfirmed(ctx context.Context, sessions *session.SessionRegistry, entry session.SessionEntry) (bool, error) {
	if s == nil {
		return true, nil
	}
	if !s.shows(entry) {
		return false, nil
	}
	return sessions.StoredByWriter(ctx, entry.SessionID)
}

// hidesConfirmed is hides with showsConfirmed's leader read: a session the scope does not show,
// confirmed by its stored record, and one the bucket holds no record of (nats.ErrKeyNotFound), are
// hidden as a session no listener has heard of (unknownSession). A read that did not answer, the
// confirmation's own among them, is returned as its error, not as hidden, so the route answers the
// bus-unavailable 503. A nil scope hides nothing.
func (s *edgeScope) hidesConfirmed(ctx context.Context, sessions *session.SessionRegistry, entry session.SessionEntry, err error) (bool, error) {
	if s == nil {
		return false, nil
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return true, nil
	}
	if err != nil {
		return false, nil
	}
	shown, confErr := s.showsConfirmed(ctx, sessions, entry)
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

// showsAgent reports whether the scope shows a Legion agent registry entry: the issue it works is
// of one of the scope's projects. A nil scope shows every entry.
func (s *edgeScope) showsAgent(agent legionagents.Agent) bool {
	if s == nil {
		return true
	}
	project, ok := contracts.IssueProject(agent.Issue)
	return ok && slices.Contains(s.projects, project)
}

// agents is agents with only the entries the scope shows (showsAgent), in their order.
func (s *edgeScope) agents(agents []legionagents.Agent) []legionagents.Agent {
	if s == nil {
		return agents
	}
	return slices.DeleteFunc(agents, func(agent legionagents.Agent) bool { return !s.showsAgent(agent) })
}

// showsAgentConfirmed is showsAgent with the agent's session confirmed by its stored record: the
// issue the entry works is of one of the scope's projects, and the entry's session is a session of
// that issue's project, confirmed by its stored record (sessionInProject, through
// session.SessionRegistry.StoredByWriter), so a record the server wrote reads as no session. An
// entry is a record any writer of the registry's bucket can store, naming any session under any
// issue, so the issue it names says nothing of whose session it names: a chosen entry naming an
// in-house session under an outside project's issue would otherwise show that session's id, a title
// its writer chose and its live state to the outside session. Its replacement (replaced_by,
// replaced_at) is read from the other entries of its role token, records of the same kind, so it is
// shown only when it too is a session of that project, and answered as none otherwise. A
// confirmation read that did not answer is returned as its error, so the route answers the
// bus-unavailable 503. A nil scope shows every entry.
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
		return legionagents.Agent{}, false, err
	}
	if !shown {
		return legionagents.Agent{}, false, nil
	}
	if agent.ReplacedBy != nil {
		replaced, err := sessionInProject(ctx, sessions, *agent.ReplacedBy, project)
		if err != nil {
			return legionagents.Agent{}, false, err
		}
		if !replaced {
			agent.ReplacedBy, agent.ReplacedAt = nil, nil
		}
	}
	return agent, true, nil
}

// agentsConfirmed is agents with each entry's session confirmed (showsAgentConfirmed), in their
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

// sessionInProject reports whether sessionID's entry in sessions is a session of project, confirmed
// by its stored record (session.SessionRegistry.StoredByWriter): the cached entry names its own key
// and lists project, and the stream's leader confirms its newest message is a writer's. A read that
// did not answer is returned as its error.
func sessionInProject(ctx context.Context, sessions *session.SessionRegistry, sessionID, project string) (bool, error) {
	entry, _ := sessions.Lookup(sessionID)
	if entry.SessionID == "" || !slices.Contains(entry.Projects, project) {
		return false, nil
	}
	return sessions.StoredByWriter(ctx, entry.SessionID)
}

// holder is result, the resolution of a role's holder, as the scope answers it: whole for a holder
// whose session entry it shows (shows), and for any other, live or lapsed, the resolution of a
// role no claim holds (roles.Unclaimed), which names no holder, process, state, last registration,
// hand-back or claim revision. A role of one of the scope's projects can be claimed by any
// session, and its claim record written by any writer of the role bucket, so the claim says
// nothing of whose session it names: a holder outside the scope's projects is another project's
// session, whose id and activity the scope shows nobody, as it shows no such session in a read. GET
// /v1/roles/<role> and a role send through the publish route answer by it; the send still
// forwards to the holder. A nil scope answers result as it is.
func (s *edgeScope) holder(result roles.Resolution) roles.Resolution {
	if result.State == roles.Unclaimed || s.shows(result.Entry) {
		return result
	}
	return roles.Resolution{State: roles.Unclaimed}
}

// namesHolder reports whether an answer the scope holds may name holderID, the holder of a role an
// answer names with no resolution beside it (a claim's 409): the entry sessions holds of it is one
// the scope shows, as holder decides for a resolution. A nil scope names every holder.
func (s *edgeScope) namesHolder(sessions *session.SessionRegistry, holderID string) bool {
	if s == nil {
		return true
	}
	entry, _ := sessions.Lookup(holderID)
	return s.shows(entry)
}

// roleOfProjects reports whether role, named by its claim's key, works for one of projects: the
// project its key gives (contracts.ParseRoleClaimKey) is one of them. It is the edge's rule for a
// role the session already holds, as edgeRoleRefusal is for one it claims or reads.
func roleOfProjects(role string, projects []string) bool {
	project, _ := contracts.ParseRoleClaimKey(role)
	return slices.Contains(projects, project)
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
		if claim, err := contracts.RoleClaimOf(lane); err != nil || !slices.Contains(projects, claim.Project) {
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
		if claim, err := contracts.RoleClaimOf(lane); !claim.Legacy && err == nil && slices.Contains(projects, claim.Project) {
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
	if claim, err := contracts.RoleClaimOf(role); err == nil && slices.Contains(projects, claim.Project) {
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
// topic whose repository no fresh answer of its session's projects names, and refollows that
// session's consumer, as an unsubscribe does. A session one of whose projects has no fresh
// answer keeps every topic: an answer that is not there says nothing of a repository's leaving,
// and the relay withholds the session's GitHub frames meanwhile. A record it cannot list, or a
// removal that fails, is logged and left to the next pass.
func pruneEdgeInterests(ctx context.Context, d *listenerDeps, machineID string) {
	logger := d.repositories.logger
	items, err := d.registry.List()
	if err != nil {
		logger.Warn("edge listener could not list the interests a repository may have left", slog.String("error", err.Error()))
		return
	}
	for _, item := range items {
		if item.MachineID != machineID {
			continue
		}
		entry, _ := d.sessions.Lookup(item.SessionID)
		repositories, complete := d.repositories.standing(entry.Projects)
		if !complete {
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
			continue
		}
		removeCtx, cancel := context.WithTimeout(ctx, sendRequestBudget)
		removed, err := d.registry.Remove(removeCtx, item.SessionID, leaving, store.RoleReleaseAuthority{})
		if err != nil {
			cancel()
			logger.Warn("edge listener could not remove the interests of a repository that left the session's projects",
				slog.String("session_id", item.SessionID), slog.Any("topics", leaving), slog.String("error", err.Error()))
			continue
		}
		recordFollowedTopics(removeCtx, d, item.SessionID, logger)
		cancel()
		d.consumers.refollow(ctx, d, item.SessionID)
		logger.Info("edge listener removed the interests of a repository that left the session's projects",
			slog.String("session_id", item.SessionID), slog.Any("topics", removed))
	}
}
