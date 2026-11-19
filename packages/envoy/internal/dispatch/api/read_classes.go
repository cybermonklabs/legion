package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// readEntry is what one GET row reads: its classes, why its rows are of them, and its Probe.
type readEntry struct {
	Classes []readclass.Class
	Reason  string
	// Probe names, for each wildcard of the row's pattern, the kind of seeded object a driver
	// fills it with when it reads the row as each caller: the private project's own object of
	// that kind, or an id of that kind nobody holds, whose two answers must be alike. A row with
	// no wildcard has a Probe that fills none.
	Probe readProbe
}

// readProbe is a row's Probe: the object each wildcard of its pattern is filled with.
type readProbe map[string]probeObject

// probeObject is a kind of object a Probe fills a wildcard with.
type probeObject string

const (
	probeProject         probeObject = "project"          // a project's key
	probeIssue           probeObject = "issue"            // an issue's key
	probeDocument        probeObject = "document"         // a document's or file's uuid
	probeDocumentSlug    probeObject = "document-slug"    // the slug of a document of the issue, project or conversation the path names
	probeDocumentVersion probeObject = "document-version" // a version number of that document
	probeBlock           probeObject = "block"            // a block id of that document
	probeAsk             probeObject = "ask"              // an ask's uuid
	probeComment         probeObject = "comment"          // a comment's uuid
	probeMessage         probeObject = "message"          // a message's uuid
	probeHandoff         probeObject = "handoff"          // a handoff's uuid
	probeHandoffPhase    probeObject = "handoff-phase"    // a phase the issue the path names has a handoff of
	probeSession         probeObject = "session"          // an agent session's id
	probePerson          probeObject = "person"           // a person's login
	probePriority        probeObject = "priority"         // a priority's id
	probeDeliveryRun     probeObject = "delivery-run"     // a deploy-repository run's id
	probeBroadcast       probeObject = "broadcast"        // a broadcast's uuid
	probeGoingRun        probeObject = "going-run"        // a going request's uuid
	probeRepositoryOwner probeObject = "repository-owner" // the owner of a repository a project maps
	probeRepositoryName  probeObject = "repository-name"  // that repository's name
	probeLivePause       probeObject = "live-pause"       // a live-write pause point a test hook arms
	probeNearCopyCheck   probeObject = "near-copy-check"  // one near-copy gate answer's uuid, of the project its create named
)

// probeObjects is every probeObject: the read rows' and the write rows' (write_probes.go).
var probeObjects = []probeObject{
	probeProject, probeIssue, probeDocument, probeDocumentSlug, probeDocumentVersion, probeBlock,
	probeAsk, probeComment, probeMessage, probeHandoff, probeHandoffPhase, probeSession, probePerson,
	probePriority, probeDeliveryRun, probeBroadcast, probeGoingRun, probeRepositoryOwner,
	probeRepositoryName, probeLivePause, probeNearCopyCheck,
	probeResult, probeThread, probeAttempt, probeEmail, probeGroup, probeAgentToken, probeSessionGroup,
	probeCredentialRequest, probeCredentialGrant, probeMachineLogin, probeSignin, probeMappedRepository, probeStep,
}

// reads is one entry of readClasses.
func reads(reason string, probe readProbe, classes ...readclass.Class) readEntry {
	return readEntry{Classes: classes, Reason: reason, Probe: probe}
}

// The Probes most rows share, by the one object their path names.
var (
	byIssue    = readProbe{"key": probeIssue}
	byProject  = readProbe{"key": probeProject}
	byDocument = readProbe{"id": probeDocument}
	bySession  = readProbe{"session_id": probeSession}
)

// readClasses is the read class of every GET row, by "<method> <pattern>". It stands beside the
// table, as writeClasses does, and routes() stamps each row's classes onto apiRoute.Reads.
// undecidedReads holds the two in exact correspondence: a GET row this table does not name, and a
// name that is no GET row, each fail TestEveryGETRouteHasAReadClass and refuse to mount
// (register).
var readClasses = map[string]readEntry{
	// What the server answers of itself: no row any caller's access decides.
	"GET /api/v1":                          reads("the route index, read from this table and naming no row", nil, readclass.None),
	"GET /api/v1/schema/blocks":            reads("the block schema this build serves, alike for every caller", nil, readclass.None),
	"GET /api/v1/limits":                   reads("the bounds a write is held to, constants of the build", nil, readclass.None),
	"GET " + goingCataloguePath:            reads("the catalogue of operations, built from this table at start", nil, readclass.None),
	"GET /api/v1/going/capability":         reads("whether the deployment configured a text model, which names no row", nil, readclass.None),
	"GET /api/v1/dashboard/builds":         reads("the dashboard builds the server processes serve, which name no row of a project", nil, readclass.None),
	"GET /api/v1/credentials/capabilities": reads("the secrets broker's own metadata, relayed whole", nil, readclass.None),
	"GET /api/v1/github/app-installation/{owner}/{repo}": reads("whether the GitHub App is installed on a repository, asked of GitHub and read from no row: a repository's name is no project's content",
		readProbe{"owner": probeRepositoryOwner, "repo": probeRepositoryName}, readclass.None),

	// The caller's own rows, keyed on its credential or its person.
	"GET /api/v1/whoami":                 reads("who the caller's own credential names", nil, readclass.Caller),
	"GET /api/v1/me/agent-tokens":        reads("the caller's own personal tokens", nil, readclass.Caller),
	"GET /api/v1/signins":                reads("the caller's own sign-ins", nil, readclass.Caller),
	"GET /api/v1/credential-grants":      reads("the credential grants the caller may revoke, as their approver", nil, readclass.Caller),
	"GET /api/v1/me/answers":             reads("the caller's own answers and replies, each on an ask of a project", nil, readclass.Caller, readclass.Project),
	"GET /api/v1/me/replies":             reads("the caller's own Inbox threads, each on an issue or document of a project or a conversation", nil, readclass.Caller, readclass.Project),
	"GET /api/v1/me/state":               reads("the caller's read state, kept per issue of a project", nil, readclass.Caller, readclass.Project),
	"GET /api/v1/me/documents/{id}/read": reads("the caller's read mark on one document of a project", byDocument, readclass.Caller, readclass.Project),
	"GET /api/v1/going/requests/{id}/stream": reads("the caller's own going run, whose answer quotes rows of projects",
		readProbe{"id": probeGoingRun}, readclass.Caller, readclass.Project),
	"GET /api/v1/me/agents/state": reads("the caller's own state of each agent session it talks to", nil, readclass.Caller, readclass.Agents),

	// Agent sessions, and what reached them.
	"GET /api/v1/agents":                       reads("the live sessions of the listener's roster", nil, readclass.Agents),
	"GET /api/v1/agents/{session_id}/answers":  reads("what reached one session: answers, comments and messages of projects or of its own conversations", bySession, readclass.Agents, readclass.Project),
	"GET /api/v1/agents/{session_id}/messages": reads("one session's targeted messages, on issues of projects or in its own conversations", bySession, readclass.Agents, readclass.Project),
	"GET /api/v1/agents/{session_id}/artifacts/{slug}": reads("a file of one agent conversation, which belongs to no project and is held to its parties",
		readProbe{"session_id": probeSession, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": reads("a version of a file of one agent conversation, held to its parties",
		readProbe{"session_id": probeSession, "slug": probeDocumentSlug, "number": probeDocumentVersion}, readclass.Project),

	// Projects, and what each holds.
	"GET /api/v1/projects":                                reads("the projects, each a row of its own project", nil, readclass.Project),
	"GET /api/v1/projects/{key}/artifacts":                reads("one project's documents", byProject, readclass.Project),
	"GET /api/v1/projects/{key}/related":                  reads("the related-item check over one project's issues, documents and asks", byProject, readclass.Project),
	"GET /api/v1/projects/{key}/architecture-sources":     reads("one project's architecture sources", byProject, readclass.Project),
	"GET /api/v1/projects/{key}/architecture-source":      reads("one project's first architecture source", byProject, readclass.Project),
	"GET /api/v1/projects/{key}/architecture":             reads("one project's components and the issues attached to them", byProject, readclass.Project),
	"GET /api/v1/projects/{key}/access":                   reads("one project's access list, which names people", byProject, readclass.Project, readclass.People),
	"GET /api/v1/projects/{key}/legion/daemon":            reads("which Legion daemon the deployment reads one project's state from: the deployment's setting, as its writes are, under a project the caller must see", byProject, readclass.Deployment, readclass.Project),
	"GET /api/v1/projects/{key}/legion/board":             reads("one project's Legion board: its issues, as its daemon reports them, and the issues a blocker link holds each on", byProject, readclass.Project),
	"GET /api/v1/near-copy-checks":                        reads("one project's near-copy gate answers, each naming issues of that project", nil, readclass.Project),
	"GET /api/v1/near-copy-checks/{id}":                   reads("one near-copy gate answer, of one project", readProbe{"id": probeNearCopyCheck}, readclass.Project),
	"GET /api/v1/settings/repo-projects":                  reads("the repository mappings, each a row of the project it maps to", nil, readclass.Project),
	"GET /api/v1/settings/architecture-sources":           reads("every project's architecture sources, each a row of its project", nil, readclass.Project),
	"GET /api/v1/settings/project-default-assignees":      reads("every project's default assignee, each a row of its project", nil, readclass.Project),
	"GET /api/v1/related-checks":                          reads("the related-item checks recorded, each of the project it searched", nil, readclass.Project),
	"GET /api/v1/projects/{key}/artifacts/{slug}":         reads("one project document", readProbe{"key": probeProject, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary": reads("what a reference to one project document shows", readProbe{"key": probeProject, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/projects/{key}/artifacts/{slug}/text":    reads("one project document's markdown", readProbe{"key": probeProject, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks":  reads("one project document's blocks", readProbe{"key": probeProject, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}": reads("a version of one project document",
		readProbe{"key": probeProject, "slug": probeDocumentSlug, "number": probeDocumentVersion}, readclass.Project),

	// Issues, and what each holds.
	"GET /api/v1/issues":                               reads("the issues, each a row of its project", nil, readclass.Project),
	"GET /api/v1/issues/resolve":                       reads("the issue a key or a GitHub reference names, a row of its project", nil, readclass.Project),
	"GET /api/v1/issues/{key}":                         reads("one issue with its asks, documents and children, rows of its project, and its progress read through refs", byIssue, readclass.Project, readclass.Graph),
	"GET /api/v1/issues/{key}/summary":                 reads("what a reference to one issue shows", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/events":                  reads("one issue's event log", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/references":              reads("the closure of documents one issue's content references, through refs", byIssue, readclass.Graph),
	"GET /api/v1/issues/{key}/collaborators":           reads("everyone who worked one issue: its people and its agent sessions", byIssue, readclass.Project, readclass.People, readclass.Agents),
	"GET /api/v1/issues/{key}/subscribers":             reads("the sessions subscribed to one issue's topics", byIssue, readclass.Project, readclass.Agents),
	"GET /api/v1/issues/{key}/role-holders":            reads("the sessions that hold one issue's Legion roles", byIssue, readclass.Project, readclass.Agents),
	"GET /api/v1/issues/{key}/steps":                   reads("the runs of each step of one issue's workflow, rows of its project", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/artifacts":               reads("one issue's documents and files", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/results":                 reads("the checks of one issue's Verification document and their results", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/coverage":                reads("what each requirement of one issue's spec lacks", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/messages":                reads("one issue's message threads", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/messages/{id}":           reads("one message of one issue", readProbe{"key": probeIssue, "id": probeMessage}, readclass.Project),
	"GET /api/v1/issues/{key}/handoffs":                reads("one issue's handoffs", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest": reads("the newest handoff of one phase of one issue", readProbe{"key": probeIssue, "phase": probeHandoffPhase}, readclass.Project),
	"GET /api/v1/issues/{key}/asks":                    reads("one issue's asks", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/decisions":               reads("one issue's decision records", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/comments":                reads("one issue's comments", byIssue, readclass.Project),
	"GET /api/v1/issues/{key}/artifacts/{slug}":        reads("one issue document", readProbe{"key": probeIssue, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/issues/{key}/artifacts/{slug}/text":   reads("one issue document's markdown", readProbe{"key": probeIssue, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks": reads("one issue document's blocks", readProbe{"key": probeIssue, "slug": probeDocumentSlug}, readclass.Project),
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}": reads("a version of one issue document",
		readProbe{"key": probeIssue, "slug": probeDocumentSlug, "number": probeDocumentVersion}, readclass.Project),
	"GET /api/v1/handoffs/{id}": reads("one handoff, of its issue's project", readProbe{"id": probeHandoff}, readclass.Project),

	// Documents by id, and what each holds.
	"GET /api/v1/artifacts/{id}":                   reads("one document's metadata and current version", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/asks":              reads("one document's asks", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/comments":          reads("one document's comments", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/events":            reads("one document's event log", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/reviews":           reads("one document's approval reviews", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/blocks":            reads("one document's blocks", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/blocks/{block_id}": reads("where one block stands in one document", readProbe{"id": probeDocument, "block_id": probeBlock}, readclass.Project),
	"GET /api/v1/artifacts/{id}/text":              reads("one document's markdown", byDocument, readclass.Project),
	"GET /api/v1/artifacts/{id}/versions/{number}": reads("a version of one document", readProbe{"id": probeDocument, "number": probeDocumentVersion}, readclass.Project),
	"GET /api/v1/artifacts/{id}/references":        reads("the cross-references to and from one document, through refs", byDocument, readclass.Graph),
	"GET /api/v1/artifacts/{id}/subscribers":       reads("the sessions subscribed to one document's topics", byDocument, readclass.Project, readclass.Agents),

	// Asks, comments and messages by id.
	"GET /api/v1/asks/open":            reads("the open asks of one session or one project", nil, readclass.Project),
	"GET /api/v1/asks/{id}":            reads("one ask with its thread", readProbe{"id": probeAsk}, readclass.Project),
	"GET /api/v1/asks/{id}/followers":  reads("the sessions one ask reaches", readProbe{"id": probeAsk}, readclass.Project),
	"GET /api/v1/comments/{id}":        reads("one comment with its thread", readProbe{"id": probeComment}, readclass.Project),
	"GET /api/v1/comments/{id}/blocks": reads("one comment's blocks", readProbe{"id": probeComment}, readclass.Project),
	"GET /api/v1/messages/{id}":        reads("one message's conversation: an issue's, of its project, or a direct one, held to its parties", readProbe{"id": probeMessage}, readclass.Project),
	"GET /api/v1/messages/{id}/blocks": reads("one message's blocks, under its conversation's rule", readProbe{"id": probeMessage}, readclass.Project),
	"GET /api/v1/events/log":           reads("the whole event log, each event owned by a project or a conversation, or by neither", nil, readclass.Project),
	"GET /api/v1/inbox":                reads("the open asks waiting on the caller, each of a project, grouped by the person each waits on", nil, readclass.Project, readclass.People),

	// The graph, read through refs.
	"GET /api/v1/references":           reads("one node's edges in the reference graph, through refs", nil, readclass.Graph),
	"GET /api/v1/references/questions": reads("the question map of one reference source's document, through refs and the asks of its project", nil, readclass.Graph, readclass.Project),
	"GET /api/v1/block-links":          reads("the links between two documents' blocks, through refs", nil, readclass.Graph),
	"GET /api/v1/reference-targets":    reads("what many references point at, through refs", nil, readclass.Graph),
	"GET /api/v1/people/{login}/nodes": reads("what one person wrote and what reached them, through refs", readProbe{"login": probePerson}, readclass.Graph, readclass.People),

	// Search, over every project's rows and the graph.
	"GET /api/v1/search":          reads("both legs of search over every project's issues, documents, asks, comments and messages", nil, readclass.Project, readclass.Graph),
	"GET /api/v1/search/results":  reads("both legs of search over every project's issues, documents, asks, comments and messages", nil, readclass.Project, readclass.Graph),
	"GET /api/v1/search/coverage": reads("how much of every project's searchable content meaning search covers", nil, readclass.Project, readclass.Graph),

	// People.
	"GET /api/v1/users": reads("everyone who has signed in", nil, readclass.People),

	// The deployment's own rows, beside the project rows some of them count.
	"GET /api/v1/priorities":              reads("the deployment's priorities, which belong to no project, with the work of projects counted under each", nil, readclass.Deployment, readclass.Project),
	"GET /api/v1/priorities/{id}/history": reads("one priority's history, which belongs to no project, with the issues of projects it names", readProbe{"id": probePriority}, readclass.Deployment, readclass.Project),
	"GET /api/v1/delivery/timeline":       reads("the delivery timeline of the pull requests whose issue the caller sees, with the runs and counts of them", nil, readclass.Project),
	"GET /api/v1/delivery/measures":       reads("the delivery measures of the pull requests whose issue the caller sees, with the issues of projects they count", nil, readclass.Project),
	"GET /api/v1/ci-reconcile/passes":     reads("the deployment's CI reconcile passes, each over repositories of many projects and naming no issue", nil, readclass.Deployment),
	"GET /api/v1/broadcasts":              reads("the deployment's broadcasts, each to sessions, with their replies", nil, readclass.Deployment, readclass.Agents, readclass.Project),
	"GET /api/v1/broadcasts/{id}":         reads("one broadcast, to sessions, with its replies", readProbe{"id": probeBroadcast}, readclass.Deployment, readclass.Agents, readclass.Project),
	"GET /api/v1/delivery/pipeline":       reads("the deployment's Pipeline page, read from its deploy repository's runs and the pull requests the timeline's rule shows the caller", nil, readclass.Deployment, readclass.Project),
	"GET /api/v1/delivery/runs/{id}":      reads("one run of the deployment's deploy repository, answered as missing where the timeline's rule does not show it", readProbe{"id": probeDeliveryRun}, readclass.Deployment, readclass.Project),
	"GET /api/v1/settings/delivery":       reads("the deployment's delivery configuration", nil, readclass.Deployment),
	"GET /api/v1/settings/new-issue":      reads("the deployment's new-issue label and session", nil, readclass.Deployment),
	"GET /api/v1/credential-requests":     reads("the deployment's pending credential requests a signed-in person decides", nil, readclass.Deployment),
	"GET /api/v1/machine-logins":          reads("the deployment's machine logins, a service's among them", nil, readclass.Deployment),
	"GET /api/v1/access-groups":           reads("the deployment's access groups, which no project owns", nil, readclass.Deployment),

	// The membership answer the secrets broker reads.
	"GET /api/v1/access/projects/{key}":   reads("the membership answer for one project and person, read by the broker's service account alone", byProject, readclass.AccessAccount),
	"GET /api/v1/access/person":           reads("one person's member kind, read by the broker's service account alone", nil, readclass.AccessAccount),
	"GET /api/v1/access/private-projects": reads("every private project's Slack channels and when each list changed, read by the broker's service account alone", nil, readclass.AccessAccount),

	// The test hooks, mounted only where a deployment turns them on.
	"GET /api/v1/artifacts/_test/rooms":              reads("the document rooms this process holds", nil, readclass.TestHook),
	"GET /api/v1/artifacts/{id}/_test/room-counters": reads("this process's counters of one document's room", byDocument, readclass.TestHook),
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": reads("whether a live write of one document parked at an armed pause",
		readProbe{"id": probeDocument, "point": probeLivePause}, readclass.TestHook),
	"GET /api/v1/messages/_test/queued": reads("the targeted messages this process queued for a session no store held", nil, readclass.TestHook),
}

// withinReadScope runs the read check in front of one GET row's handler, decided once at mount
// from the row's classes (apiRoute.Reads), as withinProjectScope decides a write's:
//
//   - accessAccount: only a service account DISPATCH_ACCESS_ACCOUNTS names reads the row
//     (requireAccessAccount); every other caller is 403 ACCESS_ACCOUNT_REQUIRED before anything
//     is read.
//   - project, graph, people, agents and deployment need the viewer: the caller is resolved once
//     (withCallerResolved) and its reader once (readerOf), and both are put in the request's
//     context, where every read of the row finds them, so no handler verifies the credential or
//     resolves the viewer a second time. A caller whose credential does not resolve is refused as
//     every gate refuses one.
//   - deployment besides: what the row reads belongs to the whole deployment and to no project a
//     viewer could be held to, so a viewer that is not open - a restricted member, or a credential
//     that lists projects - is 403 DEPLOYMENT_READ_REFUSED, naming the row.
//   - none, caller and testHook need neither, and the handler runs as it is.
//
// A server with no database, a test's, reads every caller as the closed viewer, which sees no
// project and is not open: readerOf decides that, as it decides every other viewer, so the check
// fails closed there.
func (s *server) withinReadScope(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	if slices.Contains(route.Reads, readclass.AccessAccount) {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.requireAccessAccount(w, r) {
				handler(w, r)
			}
		}
	}
	if !slices.ContainsFunc(route.Reads, readsForAViewer) {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r = s.withCallerResolved(r)
		reader, ok := s.readerOf(w, r)
		if !ok {
			return
		}
		if refused := deploymentReadRefusal(route.Method, route.Pattern, route.Reads, reader.Viewer); refused != nil {
			writeError(w, refused.code, http.StatusForbidden, refused.message)
			return
		}
		handler(w, r.WithContext(withReader(r.Context(), reader)))
	}
}

// readRefusal is how a read refuses its caller before anything is read: its code and message,
// and, for a scope the credential does not hold, that scope.
type readRefusal struct {
	code, message, scope string
}

// scopeReadRefusal is the refusal of the first of scopes, a row's OAuth scopes in the order it
// declares them (routeScopes), that a credential does not hold, or nil: 403 OAUTH_SCOPE_REQUIRED,
// as gateRouteScopes answers it over HTTP and the live socket answers it as a view's error frame
// (liveReadRefusal). holds nil holds no scope.
func scopeReadRefusal(scopes []string, holds func(string) bool) *readRefusal {
	for _, scope := range scopes {
		if holds == nil || !holds(scope) {
			return &readRefusal{code: codeScopeRequired, scope: scope, message: fmt.Sprintf(
				"this request's credential does not hold the %s scope, which this route requires", scope)}
		}
	}
	return nil
}

// codeDeploymentReadRefused refuses a read of a row of the deployment to a viewer that is not open
// (deploymentReadRefusal).
const codeDeploymentReadRefused = "DEPLOYMENT_READ_REFUSED"

// deploymentReadRefusal is the refusal of a read of a row of the deployment (readclass.Deployment
// among classes) to a viewer that is not open, or nil: 403 DEPLOYMENT_READ_REFUSED, naming the
// row, which withinReadScope answers over HTTP and the live socket as a view's error frame. A nil
// viewer is the closed one, which is not open.
func deploymentReadRefusal(method, pattern string, classes []readclass.Class, viewer *model.Viewer) *readRefusal {
	if !slices.Contains(classes, readclass.Deployment) || (viewer != nil && viewer.Open) {
		return nil
	}
	return &readRefusal{code: codeDeploymentReadRefused, message: fmt.Sprintf(
		"%s %s reads the whole deployment, which only a caller that sees every public project reads: this caller is a restricted member, or its credential lists the projects it may use",
		method, pattern)}
}

// readsForAViewer reports whether a read of class answers what its caller may see, so needs the
// caller's viewer.
func readsForAViewer(class readclass.Class) bool {
	switch class {
	case readclass.Project, readclass.Graph, readclass.People, readclass.Agents, readclass.Deployment:
		return true
	}
	return false
}

// undecidedReads names every GET row of routes that classes gives no class, and every entry of
// classes that names no GET row of the table, test hooks included, sorted. register refuses to
// mount a table it names anything in, so a GET row added later cannot go unclassed.
func undecidedReads(routes []apiRoute, classes map[string]readEntry) []string {
	var undecided []string
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		if route.Method == http.MethodGet && len(classes[name].Classes) == 0 {
			undecided = append(undecided, name+": no read class (readClasses)")
		}
	}
	var getNames []string
	for name := range classes {
		if strings.HasPrefix(name, http.MethodGet+" ") {
			getNames = append(getNames, name)
			continue
		}
		undecided = append(undecided, name+": a read class for a row that is no GET (readClasses)")
	}
	for _, name := range routesNotMounted(slices.Values(getNames)) {
		undecided = append(undecided, name+": a read class for a route the table does not serve (readClasses)")
	}
	slices.Sort(undecided)
	return undecided
}
