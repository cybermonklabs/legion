// landing.go is the one read a cold dashboard page makes: GET /api/v1/landing answers who the
// caller is, what every page of the dashboard draws whatever its route, and what the page the
// caller opened draws before it can paint. A landing of the issue page cost twenty-one reads in
// four waves before it (packages/dispatch/e2e/landing-reads.e2e.ts measures them), because each
// wave had to be answered before the next one knew what to ask for: the shell waited on whoami,
// the issue's documents waited on the issue, and the document's text and blocks waited on the
// document. This route knows all of that on the server, where each step costs a query rather
// than a round trip over the network.
//
// Nothing here reads the database itself. Each part calls the function the route it stands in
// for calls, so there is one query behind each part and the landing cannot answer something the
// route would not. A part that a reader must change belongs to that route's own file, never to
// this one.
//
// The database parts run one after another, never together. A request holds one pooled
// connection at a time (store.ErrNestedAcquire, which register's mark enforces), and two parts
// reading at once would take two; the reads are also short, and the waves this route removes are
// the cost that was paid in network round trips, not in query time.
//
// The parts a service outside the database answers are the exception: the agents and the
// subscribers, which the Envoy listener answers, and the credential requests, which the secrets
// broker answers. Each starts before the first database part, runs beside them on a goroutine
// and a connection mark of its own, and is left null when its service has not answered within
// landingServiceBound (landingServicePart). Read in line with the rest, a listener or a broker
// that accepted the connection and never answered held the whole landing for each client's own
// window in turn, and with it the first paint of every cold page, where before the landing only
// the one chip that service draws had waited (dispatch://INFRA-1109).
//
// The listener answers the route roster too, which the agents and every database part that shows
// a route read: the landing reads it once, under the same bound, for all of them
// (startLandingRoster).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The four pages a landing answers, by the ?page= value that names each. A value that is none of
// them is refused rather than answered with an empty page: a dashboard that asked for a landing
// this server does not know must read its page for itself, and a silently empty answer would
// have it draw nothing instead.
const (
	landingPageIssue    = "issue"
	landingPageBoard    = "board"
	landingPageInbox    = "inbox"
	landingPageDocument = "document"
)

// landingPages is those four in the order a refusal names them.
var landingPages = []string{landingPageIssue, landingPageBoard, landingPageInbox, landingPageDocument}

// landingServiceBound is how long the landing waits on a service outside the database - the Envoy
// listener behind the agents, the subscribers and the route roster, the secrets broker behind the
// credential requests - before it leaves that service's part null, or, for the roster, reads
// every route the landing shows as unknown.
//
// The dashboard draws nothing but its skeleton until the landing answers, so the bound is what one
// silent service costs every cold page, and the roster's bound is paid once however many parts
// show a route (startLandingRoster). A part left null costs far less: the page reads it from the
// route it stands in for once it has drawn, and that read waits on the service under the client's
// own window (envoy.defaultTimeout and agentsecrets.defaultTimeout, five seconds each) while it
// holds the one chip that part draws and nothing else, which is how every page read the part
// before the landing existed. A route read unknown is what the route's own read answers for a
// listener silent past routeRosterTimeout, two seconds. A listener that answers between one and
// two seconds is the case the bound trades away: the landing seeds the route chips as unknown
// where the route's own read would have shown the route, and the page shows them unknown until
// those keys go stale (the client's 30,000 ms staleTime) and a later read of them answers. So the
// bound trades a late page against a late or an unknown chip, and it is short. Read in line with
// the database parts, a listener and a broker that accept a connection and never answer held the
// landing for the broker's window, then the roster fetch's (routeRosterTimeout, two seconds),
// then the listener's window again for the subscribers. One second is below routeRosterTimeout,
// so the landing's roster read never waits a roster fetch out. It is no shorter because the bound
// must be one a healthy service does not meet: a part a healthy service misses is null on every
// landing, which buys back the read the landing exists to save and writes a warning about a
// service that was answering.
const landingServiceBound = time.Second

// landingResponse is the body GET /api/v1/landing answers: the viewer, the shell every page
// draws, and the page the caller opened.
//
// Every part but the viewer is answered null when it could not be read - an issue key that is
// no issue, a slug that is no document, a listener or a broker that did not answer within
// landingServiceBound, a read that failed - rather than refusing the whole landing. No GET route
// of this server is project-scoped (project_scope.go), so no null here says anything about what
// the caller may see. The dashboard reads anything the landing left
// null from the per-resource route it stands in for, so a null costs the page one wave and never
// draws a wrong page. The viewer is the one part that is not optional: a caller with no identity
// is answered the 401 the `me` routes answer, which the dashboard already reads as signed out,
// and a head the server cannot prove is a 503, since the event feed resumes from that id and a
// page that started its feed at an unproven head would miss the events committed under it.
type landingResponse struct {
	Viewer landingViewer `json:"viewer"`
	Shell  landingShell  `json:"shell"`
	Page   landingPage   `json:"page"`
}

// landingViewer is what GET /auth/whoami answers, at the same keys: the id at or below which
// every event is final, the kind of caller, and their login.
type landingViewer struct {
	// EventHead is the decimal id the page resumes its first events subscription from.
	EventHead string `json:"event_head"`
	// Kind is "user": this route resolves a person, as the `me` routes do.
	Kind string `json:"kind"`
	// Login is that person's canonical (lowercase) email.
	Login string `json:"login"`
}

// landingShell is what every page of the dashboard reads whatever its route. Each field names
// the route it stands in for, so a reader can compare the two.
type landingShell struct {
	// Inbox is GET /api/v1/inbox with no filter: the open asks waiting on somebody.
	Inbox []inboxAsk `json:"inbox"`
	// PinnedIssues is GET /api/v1/issues?pinned=true: the caller's own pinned issues.
	PinnedIssues []model.IssueSummary `json:"pinned_issues"`
	// Projects is GET /api/v1/projects.
	Projects []model.Project `json:"projects"`
	// AgentStates is GET /api/v1/me/agents/state: the caller's state per agent conversation.
	AgentStates userAgentStates `json:"agent_states"`
	// UnreadReplies is the `total` of GET /api/v1/me/replies?kind=reply,agent&limit=1, which is
	// the number the Inbox badge shows; null when that read failed.
	UnreadReplies *int `json:"unread_replies"`
	// CredentialRequests is GET /api/v1/credential-requests?approver=me. It is null both when a
	// deployment has no secrets broker, which is that route's own answer, and when the read
	// failed or the broker did not answer within landingServiceBound: the page draws no
	// Needs-you badge either way.
	CredentialRequests *wire.PendingResponse `json:"credential_requests"`
	// PrioritySummary is GET /api/v1/priorities?summary=true with no project: the two fault
	// counts the navigation rail's Priorities entry adds, and no row of either list.
	PrioritySummary *model.Priorities `json:"priority_summary"`
	// Agents is GET /api/v1/agents: the live sessions, null when the listener did not answer
	// within landingServiceBound or the read failed.
	Agents []agentResponse `json:"agents"`
}

// landingPage holds the one page the caller asked for; the other three are null. It is one
// object with four members rather than one member whose shape depends on ?page= so that a reader
// of the generated types reads each page's parts under its own name.
type landingPage struct {
	Issue    *landingIssuePage    `json:"issue"`
	Board    *landingBoardPage    `json:"board"`
	Inbox    *landingInboxPage    `json:"inbox"`
	Document *landingDocumentPage `json:"document"`
}

// landingIssuePage is what the issue page draws before it paints, each field naming its route.
type landingIssuePage struct {
	// Issue is GET /api/v1/issues/{key}; null for a key that is no issue, or a read that failed.
	Issue *issueDetail `json:"issue"`
	// UserState is GET /api/v1/me/state.
	UserState userState `json:"user_state"`
	// Comments is GET /api/v1/issues/{key}/comments.
	Comments []shapedComment `json:"comments"`
	// Asks is GET /api/v1/issues/{key}/asks?state=all, with no thread: every ask of the issue
	// with its newest reply. The page seeds this part for the reader that draws the asks
	// themselves (listIssueAsks in web/src/api/client.ts, which ProofDocument reads), and that
	// reader asks for no thread: the list is unpaged, so only a reader that draws the threads asks
	// for them (listIssueAskThreads), and it reads them from the route.
	Asks []issueAskRow `json:"asks"`
	// Subscribers is GET /api/v1/issues/{key}/subscribers; null when the listener did not answer
	// within landingServiceBound.
	Subscribers []subscriber `json:"subscribers"`
	// Decisions is GET /api/v1/issues/{key}/decisions.
	Decisions *issueDecisions `json:"decisions"`
	// Coverage is GET /api/v1/issues/{key}/coverage: what each requirement of the issue's spec
	// lacks, which the Spec tab draws beside the requirement. It is null for a key that is no
	// issue, which is the 404 that route answers for one.
	Coverage *model.IssueCoverage `json:"coverage"`
	// RepoProjects is GET /api/v1/settings/repo-projects: every repository mapped to a project.
	// The issue header's repository control reads the whole list and picks out the repositories
	// of this issue's project, so the landing carries the whole list too.
	RepoProjects []model.RepoProject `json:"repo_projects"`
	// ArchitectureSources is GET /api/v1/projects/{key}/architecture-sources for the issue's own
	// project, which the components line reads to learn whether the project has a model at all.
	// The project is the one the issue above names, so a caller asking for this landing names
	// only the issue; an issue that could not be read carries no project and leaves this null.
	ArchitectureSources []model.ArchitectureSource `json:"architecture_sources"`
	// Document is the issue's primary document - the artifact of its own `artifacts` whose id is
	// its `primary_artifact_id`, which is how the dashboard picks it (web/src/api/issue-cache.ts)
	// - with the three reads the Spec tab makes of it. It is null for an issue that has no
	// primary document, which is no failure: the page simply draws no spec.
	Document *landingIssueDocument `json:"document"`
}

// landingIssueDocument is the issue's primary document as the Spec tab reads it.
type landingIssueDocument struct {
	// Artifact is GET /api/v1/artifacts/{id}.
	Artifact *artifactRead `json:"artifact"`
	// Text is GET /api/v1/artifacts/{id}/text.
	Text *artifactText `json:"text"`
	// Blocks is GET /api/v1/artifacts/{id}/blocks.
	Blocks []model.ArtifactBlock `json:"blocks"`
	// Read is GET /api/v1/me/documents/{id}/read: the version the caller last read, which the
	// tab marks the changed blocks from.
	Read *model.DocumentRead `json:"read"`
}

// landingBoardPage is what a project's board draws before it paints.
type landingBoardPage struct {
	// Issues is GET /api/v1/issues?project=<project>&open=true.
	Issues []model.IssueSummary `json:"issues"`
	// Inbox is GET /api/v1/inbox?project=<project>.
	Inbox []inboxAsk `json:"inbox"`
	// UserState is GET /api/v1/me/state.
	UserState userState `json:"user_state"`
	// ArchitectureSources is GET /api/v1/projects/{project}/architecture-sources, which the
	// project page reads to decide which tab it opens on.
	ArchitectureSources []model.ArchitectureSource `json:"architecture_sources"`
}

// landingInboxPage is what the Inbox page draws before it paints.
type landingInboxPage struct {
	// Threads is GET /api/v1/me/replies?kind=reply,agent&state=inbox.
	Threads *model.MyRepliesResponse `json:"threads"`
}

// landingDocumentPage is what a project document's page draws before it paints.
type landingDocumentPage struct {
	// Artifact is GET /api/v1/projects/{project}/artifacts/{slug}; null for a slug that is no
	// document of that project, and with it every part below, which that document names.
	Artifact *artifactRead `json:"artifact"`
	// Text is GET /api/v1/artifacts/{id}/text.
	Text *artifactText `json:"text"`
	// Blocks is GET /api/v1/artifacts/{id}/blocks.
	Blocks []model.ArtifactBlock `json:"blocks"`
	// Asks is GET /api/v1/artifacts/{id}/asks?state=all.
	Asks []shapedAsk `json:"asks"`
	// Subscribers is GET /api/v1/artifacts/{id}/subscribers; null when the listener did not
	// answer within landingServiceBound.
	Subscribers []subscriber `json:"subscribers"`
	// Comments is GET /api/v1/artifacts/{id}/comments.
	Comments []shapedComment `json:"comments"`
}

// landingRequest is one landing as its query asks for it: the page, and the parameters that page
// needs. parseLandingRequest refuses anything else, so the reads below run only for a request
// that names a page this server can answer.
type landingRequest struct {
	page    string
	key     string
	project string
	slug    string
}

// parseLandingRequest reads ?page= and the parameters that page needs, and writes the refusal
// itself when it cannot. Every refusal is 400 LANDING_INPUT and names what the route takes: a
// landing answered empty instead would have the dashboard draw nothing, where a refusal has it
// read its page from the routes it already knows.
func parseLandingRequest(w http.ResponseWriter, r *http.Request) (landingRequest, bool) {
	query := r.URL.Query()
	read := landingRequest{
		page:    strings.TrimSpace(query.Get("page")),
		key:     strings.TrimSpace(query.Get("key")),
		project: strings.TrimSpace(query.Get("project")),
		slug:    strings.TrimSpace(query.Get("slug")),
	}
	pages := strings.Join(landingPages, ", ")
	switch read.page {
	case "":
		writeError(w, "LANDING_INPUT", http.StatusBadRequest, fmt.Sprintf("page is required: it is %s", pages))
		return landingRequest{}, false
	case landingPageIssue:
		if read.key == "" {
			writeError(w, "LANDING_INPUT", http.StatusBadRequest, "page=issue needs key, the issue's key")
			return landingRequest{}, false
		}
	case landingPageBoard:
		if read.project == "" {
			writeError(w, "LANDING_INPUT", http.StatusBadRequest, "page=board needs project, the project's key")
			return landingRequest{}, false
		}
	case landingPageDocument:
		if read.project == "" || read.slug == "" {
			writeError(w, "LANDING_INPUT", http.StatusBadRequest,
				"page=document needs project and slug, the project's key and the document's slug")
			return landingRequest{}, false
		}
	case landingPageInbox:
		// The Inbox is the caller's own; it needs nothing of the query.
	default:
		writeError(w, "LANDING_INPUT", http.StatusBadRequest,
			fmt.Sprintf("page %q is no landing of this dashboard: it is %s", read.page, pages))
		return landingRequest{}, false
	}
	return read, true
}

// getLanding answers one page's whole cold read. The caller is resolved as the `me` routes
// resolve one, so a browser with no cookie reads the same 401 it reads from whoami.
//
// The dashboard asks every read for the display trees of the stored texts it answers
// (`Dispatch-Trees: 1`, wantsDisplayTrees), this one included, and each part is read with what
// this request asked for, so a part carries exactly the trees the route it stands in for would.
func (s *server) getLanding(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	read, ok := parseLandingRequest(w, r)
	if !ok {
		return
	}
	head, err := s.eventFeedHead(r.Context())
	if err != nil {
		// A read the page's own leaving cut short says nothing of the database.
		if r.Context().Err() == nil {
			slog.Warn("dispatch: the landing could not read the event log's head", "error", err)
		}
		// The two refusals GET /auth/whoami answers, by the same codes: the page asks again for
		// as long as EVENT_HEAD_UNAVAILABLE lasts, since it ends on its own, and shows
		// EVENT_HEAD_READ_FAILED as an error after a few tries.
		if errors.Is(err, ErrEventHeadUnproven) {
			writeError(w, "EVENT_HEAD_UNAVAILABLE", http.StatusServiceUnavailable,
				"no event id is proven final yet; ask again")
			return
		}
		writeError(w, "EVENT_HEAD_READ_FAILED", http.StatusServiceUnavailable,
			"the event log's head could not be read")
		return
	}
	trees := wantsDisplayTrees(r)
	// The landing's roster read and the parts a service answers are on their way before the first
	// database part is read. The service parts are taken after the last database part, so a slow
	// listener or broker costs them landingServiceBound beside the database reads; the roster's
	// bound is paid once, by the first part that shows a route, however many do
	// (startLandingRoster).
	ctx := s.startLandingRoster(r.Context())
	services := s.startLandingServices(ctx, caller, read)
	answer := landingResponse{
		Viewer: landingViewer{EventHead: strconv.FormatInt(head, 10), Kind: "user", Login: caller.ID},
		Shell:  s.landingShell(ctx, caller, trees),
	}
	switch read.page {
	case landingPageIssue:
		answer.Page.Issue = s.landingIssuePage(ctx, caller, read.key, trees)
		answer.Page.Issue.Subscribers = services.issueSubscribers.wait()
	case landingPageBoard:
		answer.Page.Board = s.landingBoardPage(ctx, caller, read.project, trees)
	case landingPageInbox:
		answer.Page.Inbox = s.landingInboxPage(ctx, caller)
	case landingPageDocument:
		answer.Page.Document = s.landingDocumentPage(ctx, read.project, read.slug, trees)
	}
	answer.Shell.Agents = services.agents.wait()
	answer.Shell.CredentialRequests = services.credentialRequests.wait()
	WriteJSON(w, http.StatusOK, answer)
}

// landingPart keeps one part's failure out of the rest of the answer: the part is left at its
// zero value, which the response encodes as null, and the dashboard reads that part from the
// route it stands in for.
//
// A failure is logged with the part's name, so an operator reading the log sees which read failed
// on a landing that answered 200. Two kinds of null are no failure and are not logged, since the
// routes the landing stands in for log neither, and a warn line an operator reads must be a read
// that went wrong:
//
//   - A not-found (landingNotFound): an issue key that is no issue, a slug that is no document of
//     the project, a slug that names a file, which has no text and no blocks. Each is the
//     caller's own miss, which the route answers 404 or 400 NOT_DOCUMENT and writes no line for;
//     logged, every visit to an issue that was never filed or a document since renamed would
//     write one.
//   - A part read once the request's own context had ended (ctx is the request's): the page went
//     away, and every part not yet read fails on that alone. The head read above the parts skips
//     such a request for the same reason (getLanding). Logged, one closed tab would write a line
//     for each part the landing had not reached: eight from one issue landing cancelled at its
//     first listener read, which TestLandingWarnsOfNoPartACancelledRequestLeftOut refuses.
//
// Every other failure is logged: a service's refusal, a query that failed, a broker answer that
// did not decode. A part whose service did not answer within landingServiceBound is logged once,
// as late, by landingServicePart.wait rather than here, so a slow service reads as slow.
//
// Every name below is prefixed with the arm of the answer it belongs to - `shell.`, `issue.`,
// `board.`, `inbox.`, and `document.` for a project document's page - and nothing else is. That
// is a change to what the log line says (dispatch://INFRA-1109): the issue page's own primary
// document and the project document page both read a document, and both logged
// `document.artifact`, `document.text` and `document.blocks`, so an operator could not tell
// which of the two pages had failed. One prefix per arm answers that for every part at once -
// the issue page's document is `issue.document.artifact` - rather than renaming the three
// colliding names and leaving the next pair to collide.
//
// The value is returned as it was read, so a part is the same whether it is logged or not. The
// zero value a failure returns is the nil the response encodes as null, which is why each caller
// assigns what this answers straight into its field: on an error the field is set to the nil it
// already held.
func landingPart[T any](ctx context.Context, name string, value T, err error) T {
	if err == nil {
		return value
	}
	if ctx.Err() == nil && !landingNotFound(err) {
		slog.Warn("dispatch: the landing left a part out", "part", name, "error", err)
	}
	var none T
	return none
}

// landingNotFound is whether err is a part's not-found: what the route the part stands in for
// answers 404 or 400 NOT_DOCUMENT for, which no route logs (writeHandlerError logs an apiError
// only when it is a 500). The loaders report one in three ways, and each is taken here: readIssue
// answers pgx.ErrNoRows for an issue key that is no issue, which writeHandlerError answers 404
// NOT_FOUND; loadArtifactByDocumentReference answers a 404 ARTIFACT_NOT_FOUND apiError for a slug
// that is no document; and loadArtifactText and loadArtifactBlocks answer a 400 NOT_DOCUMENT
// apiError for an artifact that is a file. A 404 apiError is taken whatever its code, since the
// code only names what was missing.
func landingNotFound(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var refused *apiError
	return errors.As(err, &refused) && (refused.status == http.StatusNotFound || refused.code == "NOT_DOCUMENT")
}

// landingServicePart is one part a service outside the database answers, read on a goroutine of
// its own while the landing reads its database parts, and ended at landingServiceBound.
//
// The bound is a deadline on the context the read itself runs on, so it stops the outbound call
// rather than only the landing's wait for it: the listener client (envoy.Client.get) and the
// broker client (agentsecrets.Client.do) each build their request with
// http.NewRequestWithContext and send it with http.Client.Do, and net/http abandons a request
// whose context has ended and answers that context's error, whether it was dialling, sending,
// waiting for the answer or reading its body; and the roster read behind the agents
// (readRouteRoster) stops waiting on the landing's roster read (startLandingRoster) once its own
// context ends. So the read has returned by the bound, and wait, which blocks on the read rather
// than on a timer of its own, has answered by then too and leaves no goroutine writing into a
// part while the response encodes it. TestLandingDoesNotWaitOnASilentListenerOrBroker holds both
// services silent for the whole test and reads each landing back within the bound and its
// margin.
//
// The read's context carries a connection mark of its own (store.ForConcurrentRead). The agents
// read takes the open-ask counts and the last activity from the database beside the listener, as
// the landing's roster read takes the roster row; on the request's one mark, either's open cursor
// would have the pool refuse the database part read beside it, or have that part's refuse it
// (store.ErrNestedAcquire).
type landingServicePart[T any] struct {
	name string
	// request is the request's own context: its end is the caller going away, which is no
	// failure of the part.
	request context.Context
	// done closes once the read has returned and value, err and late hold what it answered.
	done  chan struct{}
	value T
	err   error
	// late is whether the bound ended the read: it failed once the bound's deadline had passed.
	late bool
}

// startLandingServicePart starts read under landingServiceBound and answers the part wait takes
// its answer from. ctx is the request's, and must hold no pooled connection, which is how every
// caller starts one: before the first database part, or between two of them.
func startLandingServicePart[T any](ctx context.Context, name string, read func(context.Context) (T, error)) *landingServicePart[T] {
	part := &landingServicePart[T]{name: name, request: ctx, done: make(chan struct{})}
	marked, err := store.ForConcurrentRead(ctx)
	if err != nil {
		// A caller that held a connection here is a bug, not a slow service: the part is left out
		// and logged as any read that failed.
		part.err = err
		close(part.done)
		return part
	}
	go func() {
		defer close(part.done)
		bounded, cancel := context.WithTimeout(marked, landingServiceBound)
		defer cancel()
		part.value, part.err = read(bounded)
		part.late = part.err != nil && errors.Is(bounded.Err(), context.DeadlineExceeded)
	}()
	return part
}

// wait answers the part once its read has returned, at landingServiceBound at the latest, and
// null for a read that failed. A read the bound ended is logged once, naming the part and the
// bound, so an operator sees which service is slow on a landing that answered 200 without it;
// a request that had already ended is logged as nothing, as landingPart logs it, and every other
// failure goes through landingPart. A part never started - a service this deployment does not
// have, or a page that reads nothing from it - is nil, and answers null at once.
func (p *landingServicePart[T]) wait() T {
	var none T
	if p == nil {
		return none
	}
	<-p.done
	if p.late && p.request.Err() == nil {
		slog.Warn("dispatch: the landing left a part out: its service did not answer within the bound",
			"part", p.name, "bound", landingServiceBound)
		return none
	}
	return landingPart(p.request, p.name, p.value, p.err)
}

// landingRosterKey is the context key a landing's roster read (startLandingRoster) is found under.
type landingRosterKey struct{}

// startLandingRoster starts the landing's one read of the route roster, a service part under
// landingServiceBound, and answers ctx carrying it, which getLanding reads every part through.
// Every part of the landing that asks for the roster takes that read: the agents, and the
// database parts that show a route - the inbox's asks, the pinned issues, the board's issues and
// the issue itself. readRouteRoster finds it in the part's context (landingRosterKey) and waits
// on it instead of reading the roster itself; the read runs on ctx as it came, which carries
// none, so it reads the roster.
//
// It is one read rather than one per part for two reasons. Every part of one landing shows the
// same route statuses: read part by part, the stored answer could turn stale or be replaced
// between two of them, and one page could show a route live in its inbox and no_holder on its
// board. And a silent listener costs the landing the bound once, paid by the first part that
// shows a route, however many do: a part that reads the roster for itself waits on the listener's
// shared fetch for up to routeRosterTimeout, outside any bound, and read that way a board landing
// with an open ask on a routed issue answered after 2.077 s behind a silent listener
// (TestLandingDoesNotWaitOnASilentListenerForARoutedIssue). A roster the bound left unanswered
// reads every route unknown, which is what a part's own read answers once it has waited the
// listener out, and no database part logs it, since a silent listener is no failed read
// (routeRosterOrUnknown). The agents part takes it as the listener's silence and is left null and
// named once in the log, as for a silence the roster row stored; this read's bound starts a
// moment before the agents part's own, so it usually ends first, and the line then names the
// silence rather than the bound.
//
// ctx is the request's, and must hold no pooled connection: getLanding starts the read before the
// first database part, and the read takes a connection mark of its own, since it reads the stored
// roster row while the database parts read theirs. A ctx that held one is a bug, which every part
// that takes the read logs as a failed read. A deployment with no listener starts nothing and
// answers ctx as it is, since readRouteRoster answers every route unknown there at once, with
// nothing to wait on and so nothing to share.
//
// The part's name, landing.route_roster, takes no arm's prefix, against the rule above
// landingPart, because no log line ever names it: nothing calls wait on this part.
func (s *server) startLandingRoster(ctx context.Context) context.Context {
	if s.deps.Envoy == nil {
		return ctx
	}
	part := startLandingServicePart(ctx, "landing.route_roster", func(bounded context.Context) (routeRoster, error) {
		roster, err := s.readRouteRoster(bounded)
		if err != nil && bounded.Err() != nil {
			// The bound, or the request's end, cut the read of the stored roster row short. That
			// is the listener's answer out of this landing's reach, not a failed read of the
			// database, so it answers what a read the bound ended while it waited on the fetch
			// answers: unanswered, with no error, which no database part logs as a failure.
			return endedRouteRoster(bounded), nil
		}
		return roster, err
	})
	return context.WithValue(ctx, landingRosterKey{}, part)
}

// landingRosterRead answers the landing's roster read (startLandingRoster) that ctx carries,
// with taken true, once that read has returned; a ctx that ends first answers endedRouteRoster,
// as readRouteRoster answers a read whose own context ended while it waited on the shared fetch.
// A ctx that carries no landing's read answers taken false, and the caller reads the roster
// itself. The answer is taken from the part's fields, never through wait, which would log the
// roster read as a part of its own: each part that takes the roster logs what it makes of it.
func landingRosterRead(ctx context.Context) (roster routeRoster, taken bool, err error) {
	part, ok := ctx.Value(landingRosterKey{}).(*landingServicePart[routeRoster])
	if !ok {
		return routeRoster{}, false, nil
	}
	select {
	case <-part.done:
		return part.value, true, part.err
	case <-ctx.Done():
		return endedRouteRoster(ctx), true, nil
	}
}

// landingServices is the landing's parts a service outside the database answers that it can ask
// for before it has read anything, each on its way while the database parts are read. A nil
// member is a part not started, which wait answers null.
type landingServices struct {
	agents             *landingServicePart[[]agentResponse]
	credentialRequests *landingServicePart[*wire.PendingResponse]
	issueSubscribers   *landingServicePart[[]subscriber]
}

// startLandingServices starts the shell's agents and credential requests, and the issue page's
// subscribers, which need only the key the request names. The document page's subscribers need
// the document the page reads first, so landingDocumentPage starts them itself. ctx carries the
// landing's roster read, which the agents part takes (startLandingRoster).
func (s *server) startLandingServices(ctx context.Context, caller model.Actor, read landingRequest) landingServices {
	var started landingServices
	// The agents and the subscribers are the listener's to answer, and loadSubscribers refuses
	// with ENVOY_UNAVAILABLE where there is no listener configured. On such a deployment that
	// refusal is not a failed read but the shape of the deployment, so neither is read at all:
	// without this guard every issue landing of a listener-less deployment wrote a warn line for a
	// part that could never be answered (dispatch://INFRA-1109). The parts stay null either way,
	// and the page reads their routes itself, which answer that same refusal.
	if s.deps.Envoy != nil {
		started.agents = startLandingServicePart(ctx, "shell.agents", func(ctx context.Context) ([]agentResponse, error) {
			agents, _, err := s.loadAgents(ctx)
			return agents, err
		})
		if read.page == landingPageIssue {
			started.issueSubscribers = startLandingServicePart(ctx, "issue.subscribers", func(ctx context.Context) ([]subscriber, error) {
				return s.loadSubscribers(ctx, issueSubscriberBase(read.key))
			})
		}
	}
	// A deployment with no broker answers null, which is GET /api/v1/credential-requests' own
	// answer there.
	if s.deps.AgentSecrets != nil {
		started.credentialRequests = startLandingServicePart(ctx, "shell.credential_requests",
			func(ctx context.Context) (*wire.PendingResponse, error) {
				return s.landingCredentialRequests(ctx, caller)
			})
	}
	return started
}

// landingShell reads what every page draws from the database. The agents and the credential
// requests, which a service answers, are startLandingServices' to read.
func (s *server) landingShell(ctx context.Context, caller model.Actor, trees bool) landingShell {
	var shell landingShell

	// The inbox as the dashboard reads it on every page: no project and no assignee narrowing.
	inbox, err := s.loadInbox(ctx, caller, "", "", "", trees)
	shell.Inbox = landingPart(ctx, "shell.inbox", inbox, err)

	pinned, _, err := s.loadIssueListing(ctx, issueListRead{pinnedBy: caller.ID})
	shell.PinnedIssues = landingPart(ctx, "shell.pinned_issues", pinned, err)

	projects, err := s.loadProjects(ctx)
	shell.Projects = landingPart(ctx, "shell.projects", projects, err)

	states, err := s.loadUserAgentStates(ctx, caller.ID, nil)
	shell.AgentStates = landingPart(ctx, "shell.agent_states", states, err)

	// The badge's own read: one row of the unread state, whose total is every unread thread of
	// both kinds. The row itself is thrown away, as the page throws it away.
	one := 1
	replies, err := s.loadMyReplies(ctx, caller, myRepliesRead{
		wantReply: true, wantAgent: true, state: myRepliesStateUnread, limit: &one,
	})
	shell.UnreadReplies = landingPart(ctx, "shell.unread_replies", &replies.Total, err)

	// The rail's own read: the two fault counts over every project, as the rail asks for them.
	// The narrowing is the empty list rather than nil, as the route passes it: a nil slice is
	// sent as NULL, which the project filter reads as no project at all rather than every one.
	priorities, err := s.readPriorities(ctx, []string{}, true)
	shell.PrioritySummary = landingPart(ctx, "shell.priority_summary", &priorities, err)
	return shell
}

// landingCredentialRequests is GET /api/v1/credential-requests?approver=me, read through the
// same broker client that route relays. That route hands the broker's bytes on unparsed; the
// landing carries a modelled part instead, so the bytes are read here into the shape the broker
// declares (wire.PendingResponse) and the answer carries no unparsed JSON. A deployment with no
// broker answers null there and null here, since startLandingServices reads this only where there
// is a broker.
func (s *server) landingCredentialRequests(ctx context.Context, caller model.Actor) (*wire.PendingResponse, error) {
	body, err := s.deps.AgentSecrets.Pending(ctx, model.CanonicalLogin(caller.ID))
	if err != nil {
		return nil, err
	}
	var pending wire.PendingResponse
	if err := json.Unmarshal(body, &pending); err != nil {
		return nil, fmt.Errorf("decode the broker's pending list: %w", err)
	}
	return &pending, nil
}

// landingIssuePage reads what the issue page draws, each part through landingPart of its own,
// so a read that failed leaves that part null and no other. A key that is no issue leaves the
// issue null and with it the parts read through it - its document, its coverage and its
// project's architecture sources; every other part is read by the key alone and is answered as
// its own route answers that key (landing_test.go,
// TestLandingLeavesAnUnknownIssueNullAndAnswersTheShell). Its subscribers, which the listener
// answers, are read beside it (startLandingServices) and set into it by getLanding.
func (s *server) landingIssuePage(ctx context.Context, caller model.Actor, key string, trees bool) *landingIssuePage {
	page := &landingIssuePage{}

	read, err := s.loadIssueDetail(ctx, key, trees)
	page.Issue = landingPart(ctx, "issue.detail", &read, err)

	state, err := s.loadUserState(ctx, caller.ID)
	page.UserState = landingPart(ctx, "issue.user_state", state, err)

	// Shaped as the comments route shapes them, over the page's reader, which the coverage part
	// below registers with too and which resolves at the end, so the page reads the stored trees
	// once for both.
	comments, err := s.loadOwnerComments(ctx, s.deps.Store.Pool, issueOwner(key), "")
	reader := s.readDisplayTrees(ctx, trees)
	page.Comments = shapedComments(landingPart(ctx, "issue.comments", comments, err), reader)

	// With no thread, as the reader the page seeds this part for asks for it: the list is
	// unpaged, so a thread read here is read for every ask of the issue and drawn by no one.
	asks, err := s.loadIssueAskRows(ctx, issueOwner(key), "all", false, trees)
	page.Asks = landingPart(ctx, "issue.asks", asks, err)

	decisions, err := s.loadIssueDecisions(ctx, key, trees)
	page.Decisions = landingPart(ctx, "issue.decisions", &decisions, err)

	// Every repository mapping there is, which is what the repository control reads whatever the
	// issue is: it needs no issue and is read even for a key that is none.
	repos, err := s.loadRepoProjects(ctx)
	page.RepoProjects = landingPart(ctx, "issue.repo_projects", repos, err)

	if page.Issue != nil {
		// The coverage route answers 404 for an issue nobody has, so the landing reads it only
		// for an issue it has already read; a key that is no issue leaves this part null and the
		// page reads that 404 from the route itself. Each requirement registers its tree with the
		// page's reader, as the route's do with the route's (issueCoverage).
		coverage, err := s.issueCoverage(ctx, s.deps.Store.Pool, key, reader)
		page.Coverage = landingPart(ctx, "issue.coverage", &coverage, err)

		// The sources of the issue's own project. The project is in the issue read above, so the
		// landing takes it from there rather than having the caller name it: a caller who named
		// it could name a project that is not this issue's, and the page would seed the
		// components line with another project's model.
		sources, err := s.loadProjectArchitectureSources(ctx, page.Issue.Project)
		page.ArchitectureSources = landingPart(ctx, "issue.architecture_sources", sources, err)

		page.Document = s.landingIssueDocument(ctx, caller, page.Issue, trees)
	}
	reader.resolve()
	return page
}

// landingIssueDocument reads the issue's primary document: the artifact of the issue's own
// `artifacts` whose id is its `primary_artifact_id`, which is the document the dashboard's own
// primarySpec picks, so the two cannot open different documents. An issue with no primary
// document has no document part at all.
//
// The artifact itself is the row the issue read already carries, and is not read a second time
// (dispatch://INFRA-1109). The two rows are the same row by construction, not by luck: readIssue
// selects the issue's artifacts with issueArtifactsStatement and builds each through
// collectArtifacts -> scanArtifact, queueing the detail batch queueArtifactDetails fills, while
// loadArtifact selects one row and builds it through the same scanArtifact and then
// loadArtifactDetails - the same columns, the same scan and the same details either way. So the
// second read answered what this function was already holding, which is what its first sentence
// says it answers.
//
// The issue arrives by pointer because this reads two of its fields and the detail behind them
// is the whole issue: its artifacts, its labels and its relations. Taken by value it
// was all copied to read an id and a list. Its artifacts are already shaped as the artifact
// route shapes one (readIssue), so the document part carries the same trees that route would.
func (s *server) landingIssueDocument(ctx context.Context, caller model.Actor, issue *issueDetail, trees bool) *landingIssueDocument {
	primary := issue.PrimaryArtifactID
	if primary == "" {
		return nil
	}
	index := slices.IndexFunc(issue.Artifacts, func(artifact artifactRead) bool {
		return artifact.ID == primary
	})
	if index < 0 {
		return nil
	}
	// Into the issue's own slice, which the answer carries anyway, so the artifact is encoded
	// twice and copied never.
	artifact := &issue.Artifacts[index]
	document := &landingIssueDocument{Artifact: artifact}

	// No blocks beside the text: the document's blocks are a part of their own, below.
	text, err := s.loadArtifactText(ctx, artifact.Artifact, false, trees)
	document.Text = landingPart(ctx, "issue.document.text", &text, err)

	blocks, err := s.loadArtifactBlocks(ctx, artifact.Artifact)
	document.Blocks = landingPart(ctx, "issue.document.blocks", blocks, err)

	mark, err := s.loadDocumentRead(ctx, caller.ID, artifact.ID)
	document.Read = landingPart(ctx, "issue.document.read", &mark, err)
	return document
}

// landingBoardPage reads what a project's board draws.
func (s *server) landingBoardPage(ctx context.Context, caller model.Actor, project string, trees bool) *landingBoardPage {
	page := &landingBoardPage{}

	issues, _, err := s.loadIssueListing(ctx, issueListRead{project: project, open: true})
	page.Issues = landingPart(ctx, "board.issues", issues, err)

	inbox, err := s.loadInbox(ctx, caller, project, "", "", trees)
	page.Inbox = landingPart(ctx, "board.inbox", inbox, err)

	state, err := s.loadUserState(ctx, caller.ID)
	page.UserState = landingPart(ctx, "board.user_state", state, err)

	sources, err := s.loadProjectArchitectureSources(ctx, project)
	page.ArchitectureSources = landingPart(ctx, "board.architecture_sources", sources, err)
	return page
}

// landingInboxPage reads what the Inbox page draws: every thread of its two kinds, unpaged, as
// the page itself asks for them.
func (s *server) landingInboxPage(ctx context.Context, caller model.Actor) *landingInboxPage {
	threads, err := s.loadMyReplies(ctx, caller, myRepliesRead{
		wantReply: true, wantAgent: true, state: myRepliesStateInbox,
	})
	page := &landingInboxPage{}
	page.Threads = landingPart(ctx, "inbox.threads", &threads, err)
	return page
}

// landingDocumentPage reads what a project document's page draws. The document is the part every
// other part names, so a slug that is no document of the project leaves them all null and the
// page reads the document itself, which is the refusal it already draws.
//
// The document, its asks and its comments are shaped as their own routes shape them, over one
// reader for the page, so the three are one read of the stored trees. The subscribers, which the
// listener answers, are read beside the database parts (landingServicePart), started here rather
// than with the shell's because they name the document this reads first.
func (s *server) landingDocumentPage(ctx context.Context, project, slug string, trees bool) *landingDocumentPage {
	page := &landingDocumentPage{}

	artifact, err := s.loadArtifactByDocumentReference(ctx, s.deps.Store.Pool, artifactTarget{Project: project}, slug)
	if landingPart(ctx, "document.artifact", &artifact, err) == nil {
		return page
	}
	// Guarded as the issue page's subscribers are, for the reason given there
	// (startLandingServices): a deployment with no listener has nothing to answer this part with,
	// and reading it anyway wrote a warn line on every document landing (dispatch://INFRA-1109).
	var subscribers *landingServicePart[[]subscriber]
	if s.deps.Envoy != nil {
		subscribers = startLandingServicePart(ctx, "document.subscribers", func(ctx context.Context) ([]subscriber, error) {
			return s.loadSubscribers(ctx, documentSubscriberBase(artifact.Project, artifact.Slug))
		})
	}
	reader := s.readDisplayTrees(ctx, trees)
	page.Artifact = &artifactRead{Artifact: artifact}
	page.Artifact.shape(reader)

	// No blocks beside the text: the page's blocks are a part of their own, below.
	text, err := s.loadArtifactText(ctx, artifact, false, trees)
	page.Text = landingPart(ctx, "document.text", &text, err)

	blocks, err := s.loadArtifactBlocks(ctx, artifact)
	page.Blocks = landingPart(ctx, "document.blocks", blocks, err)

	asks, _, err := s.queryOwnerAsks(ctx, s.deps.Store.Pool, documentOwner(artifact.ID), "all")
	page.Asks = shapedAsks(landingPart(ctx, "document.asks", asks, err), reader)

	comments, err := s.loadOwnerComments(ctx, s.deps.Store.Pool, documentOwner(artifact.ID), "")
	page.Comments = shapedComments(landingPart(ctx, "document.comments", comments, err), reader)
	reader.resolve()
	page.Subscribers = subscribers.wait()
	return page
}
