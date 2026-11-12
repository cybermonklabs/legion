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
// The parts run one after another, never together. A request holds one pooled connection at a
// time (store.ErrNestedAcquire, which register's mark enforces), and two parts reading at once
// would take two; the reads are also short, and the waves this route removes are the cost that
// was paid in network round trips, not in query time.
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

	"github.com/sjawhar/envoy/internal/broker/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// landingResponse is the body GET /api/v1/landing answers: the viewer, the shell every page
// draws, and the page the caller opened.
//
// Every part but the viewer is answered null when it could not be read - a caller who may not
// read it, an issue key that is no issue, a slug that is no document, a listener that did not
// answer - rather than refusing the whole landing. The dashboard reads anything the landing left
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
	// failed: the page draws no Needs-you badge either way.
	CredentialRequests *wire.PendingResponse `json:"credential_requests"`
	// Agents is GET /api/v1/agents: the live sessions, null when the listener did not answer.
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
	// Issue is GET /api/v1/issues/{key}; null for a key that is no issue the caller may read.
	Issue *issueDetail `json:"issue"`
	// UserState is GET /api/v1/me/state.
	UserState userState `json:"user_state"`
	// Comments is GET /api/v1/issues/{key}/comments.
	Comments []shapedComment `json:"comments"`
	// Asks is GET /api/v1/issues/{key}/asks?state=all&thread=1: every ask of the issue with the
	// thread its card draws.
	Asks []issueAskRow `json:"asks"`
	// Subscribers is GET /api/v1/issues/{key}/subscribers.
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
	// Subscribers is GET /api/v1/artifacts/{id}/subscribers.
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
	answer := landingResponse{
		Viewer: landingViewer{EventHead: strconv.FormatInt(head, 10), Kind: "user", Login: caller.ID},
		Shell:  s.landingShell(r.Context(), caller, trees),
	}
	switch read.page {
	case landingPageIssue:
		answer.Page.Issue = s.landingIssuePage(r.Context(), caller, read.key, trees)
	case landingPageBoard:
		answer.Page.Board = s.landingBoardPage(r.Context(), caller, read.project, trees)
	case landingPageInbox:
		answer.Page.Inbox = s.landingInboxPage(r.Context(), caller)
	case landingPageDocument:
		answer.Page.Document = s.landingDocumentPage(r.Context(), read.project, read.slug, trees)
	}
	WriteJSON(w, http.StatusOK, answer)
}

// landingPart keeps one part's failure out of the rest of the answer: the part is left at its
// zero value, which the response encodes as null, and the dashboard reads that part from the
// route it stands in for. The failure is logged with the part's name, so an operator reading the
// log sees which read failed on a landing that answered 200.
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
func landingPart[T any](name string, value T, err error) T {
	if err == nil {
		return value
	}
	slog.Warn("dispatch: the landing left a part out", "part", name, "error", err)
	var none T
	return none
}

// landingShell reads what every page draws.
func (s *server) landingShell(ctx context.Context, caller model.Actor, trees bool) landingShell {
	var shell landingShell

	// The inbox as the dashboard reads it on every page: no project and no assignee narrowing.
	inbox, err := s.loadInbox(ctx, caller, "", "", "", trees)
	shell.Inbox = landingPart("shell.inbox", inbox, err)

	pinned, _, err := s.loadIssueListing(ctx, issueListRead{pinnedBy: caller.ID})
	shell.PinnedIssues = landingPart("shell.pinned_issues", pinned, err)

	projects, err := s.loadProjects(ctx)
	shell.Projects = landingPart("shell.projects", projects, err)

	states, err := s.loadUserAgentStates(ctx, caller.ID, nil)
	shell.AgentStates = landingPart("shell.agent_states", states, err)

	// The badge's own read: one row of the unread state, whose total is every unread thread of
	// both kinds. The row itself is thrown away, as the page throws it away.
	one := 1
	replies, err := s.loadMyReplies(ctx, caller, myRepliesRead{
		wantReply: true, wantAgent: true, state: myRepliesStateUnread, limit: &one,
	})
	shell.UnreadReplies = landingPart("shell.unread_replies", &replies.Total, err)

	shell.CredentialRequests = s.landingCredentialRequests(ctx, caller)

	if s.deps.Envoy != nil {
		agents, _, err := s.loadAgents(ctx)
		shell.Agents = landingPart("shell.agents", agents, err)
	}
	return shell
}

// landingCredentialRequests is GET /api/v1/credential-requests?approver=me, read through the
// same broker client that route relays. That route hands the broker's bytes on unparsed; the
// landing carries a modelled part instead, so the bytes are read here into the shape the broker
// declares (wire.PendingResponse) and the answer carries no unparsed JSON. A deployment with no
// broker answers null there and null here.
func (s *server) landingCredentialRequests(ctx context.Context, caller model.Actor) *wire.PendingResponse {
	if s.deps.AgentSecrets == nil {
		return nil
	}
	body, err := s.deps.AgentSecrets.Pending(ctx, model.CanonicalLogin(caller.ID))
	if err != nil {
		return landingPart[*wire.PendingResponse]("shell.credential_requests", nil, err)
	}
	var pending wire.PendingResponse
	if err := json.Unmarshal(body, &pending); err != nil {
		return landingPart[*wire.PendingResponse]("shell.credential_requests", nil,
			fmt.Errorf("decode the broker's pending list: %w", err))
	}
	return &pending
}

// landingIssuePage reads what the issue page draws. An issue the caller cannot read leaves every
// part of the page null but the ones that are not the issue's: their routes answer them anyway.
func (s *server) landingIssuePage(ctx context.Context, caller model.Actor, key string, trees bool) *landingIssuePage {
	page := &landingIssuePage{}

	read, err := s.loadIssueDetail(ctx, key, trees)
	page.Issue = landingPart("issue.detail", &read, err)

	state, err := s.loadUserState(ctx, caller.ID)
	page.UserState = landingPart("issue.user_state", state, err)

	// Shaped as the comments route shapes them, over a reader of their own.
	comments, err := s.loadOwnerComments(ctx, s.deps.Store.Pool, issueOwner(key), "")
	reader := s.readDisplayTrees(ctx, trees)
	page.Comments = shapedComments(landingPart("issue.comments", comments, err), reader)
	reader.resolve()

	asks, err := s.loadIssueAskRows(ctx, issueOwner(key), "all", true, trees)
	page.Asks = landingPart("issue.asks", asks, err)

	// The subscribers are the listener's to answer, and loadSubscribers refuses with
	// ENVOY_UNAVAILABLE where there is no listener configured. On such a deployment that refusal
	// is not a failed read but the shape of the deployment, so it is not read at all: the guard
	// is the one the shell above puts on its own listener part, and without it every issue
	// landing of a listener-less deployment wrote a warn line for a part that could never be
	// answered (dispatch://INFRA-1109). The part stays null either way, and the page reads the
	// subscribers route itself, which answers that same refusal.
	if s.deps.Envoy != nil {
		subscribers, err := s.loadSubscribers(ctx, issueSubscriberBase(key))
		page.Subscribers = landingPart("issue.subscribers", subscribers, err)
	}

	decisions, err := s.loadIssueDecisions(ctx, key, trees)
	page.Decisions = landingPart("issue.decisions", &decisions, err)

	// Every repository mapping there is, which is what the repository control reads whatever the
	// issue is: it needs no issue and is read even for a key that is none.
	repos, err := s.loadRepoProjects(ctx)
	page.RepoProjects = landingPart("issue.repo_projects", repos, err)

	if page.Issue != nil {
		// The coverage route answers 404 for an issue nobody has, so the landing reads it only
		// for an issue it has already read; a key that is no issue leaves this part null and the
		// page reads that 404 from the route itself.
		coverage, err := s.issueCoverage(ctx, s.deps.Store.Pool, key)
		page.Coverage = landingPart("issue.coverage", &coverage, err)

		// The sources of the issue's own project. The project is in the issue read above, so the
		// landing takes it from there rather than having the caller name it: a caller who named
		// it could name a project that is not this issue's, and the page would seed the
		// components line with another project's model.
		sources, err := s.loadProjectArchitectureSources(ctx, page.Issue.Project)
		page.ArchitectureSources = landingPart("issue.architecture_sources", sources, err)

		page.Document = s.landingIssueDocument(ctx, caller, page.Issue, trees)
	}
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
// is the whole issue: its artifacts, its labels, its relations and its events. Taken by value it
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
	document.Text = landingPart("issue.document.text", &text, err)

	blocks, err := s.loadArtifactBlocks(ctx, artifact.Artifact)
	document.Blocks = landingPart("issue.document.blocks", blocks, err)

	mark, err := s.loadDocumentRead(ctx, caller.ID, artifact.ID)
	document.Read = landingPart("issue.document.read", &mark, err)
	return document
}

// landingBoardPage reads what a project's board draws.
func (s *server) landingBoardPage(ctx context.Context, caller model.Actor, project string, trees bool) *landingBoardPage {
	page := &landingBoardPage{}

	issues, _, err := s.loadIssueListing(ctx, issueListRead{project: project, open: true})
	page.Issues = landingPart("board.issues", issues, err)

	inbox, err := s.loadInbox(ctx, caller, project, "", "", trees)
	page.Inbox = landingPart("board.inbox", inbox, err)

	state, err := s.loadUserState(ctx, caller.ID)
	page.UserState = landingPart("board.user_state", state, err)

	sources, err := s.loadProjectArchitectureSources(ctx, project)
	page.ArchitectureSources = landingPart("board.architecture_sources", sources, err)
	return page
}

// landingInboxPage reads what the Inbox page draws: every thread of its two kinds, unpaged, as
// the page itself asks for them.
func (s *server) landingInboxPage(ctx context.Context, caller model.Actor) *landingInboxPage {
	threads, err := s.loadMyReplies(ctx, caller, myRepliesRead{
		wantReply: true, wantAgent: true, state: myRepliesStateInbox,
	})
	page := &landingInboxPage{}
	page.Threads = landingPart("inbox.threads", &threads, err)
	return page
}

// landingDocumentPage reads what a project document's page draws. The document is the part every
// other part names, so a slug that is no document of the project leaves them all null and the
// page reads the document itself, which is the refusal it already draws.
//
// The document, its asks and its comments are shaped as their own routes shape them, over one
// reader for the page, so the three are one read of the stored trees.
func (s *server) landingDocumentPage(ctx context.Context, project, slug string, trees bool) *landingDocumentPage {
	page := &landingDocumentPage{}

	artifact, err := s.loadArtifactByDocumentReference(ctx, s.deps.Store.Pool, artifactTarget{Project: project}, slug)
	if landingPart("document.artifact", &artifact, err) == nil {
		return page
	}
	reader := s.readDisplayTrees(ctx, trees)
	page.Artifact = &artifactRead{Artifact: artifact}
	page.Artifact.shape(reader)

	// No blocks beside the text: the page's blocks are a part of their own, below.
	text, err := s.loadArtifactText(ctx, artifact, false, trees)
	page.Text = landingPart("document.text", &text, err)

	blocks, err := s.loadArtifactBlocks(ctx, artifact)
	page.Blocks = landingPart("document.blocks", blocks, err)

	asks, _, err := s.queryOwnerAsks(ctx, s.deps.Store.Pool, documentOwner(artifact.ID), "all")
	page.Asks = shapedAsks(landingPart("document.asks", asks, err), reader)

	// Guarded as the issue page's subscribers are, for the reason given there: a deployment with
	// no listener has nothing to answer this part with, and reading it anyway wrote a warn line
	// on every document landing (dispatch://INFRA-1109).
	if s.deps.Envoy != nil {
		subscribers, err := s.loadSubscribers(ctx, documentSubscriberBase(artifact.Project, artifact.Slug))
		page.Subscribers = landingPart("document.subscribers", subscribers, err)
	}

	comments, err := s.loadOwnerComments(ctx, s.deps.Store.Pool, documentOwner(artifact.ID), "")
	page.Comments = shapedComments(landingPart("document.comments", comments, err), reader)
	reader.resolve()
	return page
}
