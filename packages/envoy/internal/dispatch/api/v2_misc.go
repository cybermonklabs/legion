package api

// v2_misc.go is the routes of /api/v2 no other group holds (R1 of dispatch://INFRA-4071/spec):
// the agents and what people wrote to one of them, the block links, the reference targets (the
// row #4178 added, which no row of the route map names) and the block schema, search
// and its coverage, the going actions and requests, the limits, the users, the dashboard builds,
// the session groups, one deploy run, the markdown round trip and the CI reconcile passes. Every
// route runs the work its /api/v1 route runs (agentRows, sessionanswers.List, blockLinkDocuments,
// referenceTargets, searchAnswer, goingAsker, createGoingRequestFrom, signedInUsers,
// liveDashboardEntries, createDeliverySessionGroupFrom, updateDeliverySessionGroupFrom,
// deleteDeliverySessionGroupAs, markdownRoundTrip), or is that route's handler where it reads no
// query and answers the same, but the CI reconcile passes, which /api/v1 reads as the last 24
// hours whole
// (cireconcile.RecentPasses) and /api/v2 within a window it names (cireconcile.Passes). A list
// answers the envelope; a report, bytes, a stream and one object answer
// none (R1.4's departure). The {id} of a session group answers SESSION_GROUP_NOT_FOUND and of a
// going request GOING_REQUEST_NOT_FOUND (R7); {session_id} keeps the answer /api/v1 gives it
// (R7), and a deploy run keeps its own 404s, since its id is GitHub's and its repository the
// deployment's, which is also why its route is GET /api/v2/deploy-runs/{id} and no project's.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
)

// The refusals of the objects a route of this file names in its path (dispatch://INFRA-4071/spec
// R7).
var (
	sessionGroupMissingRefusal = apicode.Refusal{Condition: apicode.SessionGroupMissing, Code: apicode.SessionGroupNotFound}
	goingRequestMissingRefusal = apicode.Refusal{Condition: apicode.GoingRequestMissing, Code: apicode.GoingRequestNotFound}
	deployRunMissingRefusal    = apicode.Refusal{Condition: apicode.DeployRunMissing, Code: apicode.NotFound}
	runIDMalformedRefusal      = apicode.Refusal{Condition: apicode.RunIDMalformed, Code: apicode.RunIDInput}
)

// The refusals of what a search, a going request and a session group write name.
var (
	searchAuthorInvalidRefusal    = apicode.Refusal{Condition: apicode.SearchAuthorInvalid, Code: apicode.InvalidAuthor}
	numberProjectMalformedRefusal = apicode.Refusal{Condition: apicode.NumberProjectMalformed, Code: apicode.InvalidNumberProject}
	searchCoverageTimedOutRefusal = apicode.Refusal{Condition: apicode.SearchCoverageTimedOut, Code: apicode.SearchCoverageTimeout}
	goingAskerNamesNobodyRefusal  = apicode.Refusal{Condition: apicode.CredentialNamesNobody, Code: apicode.GoingAskerNamesNobody}
	goingWordsBlankRefusal        = apicode.Refusal{Condition: apicode.GoingWordsBlank, Code: apicode.GoingInput}
	bodyProjectMalformedRefusal   = apicode.Refusal{Condition: apicode.BodyProjectMalformed, Code: apicode.InvalidProject}
	sessionGroupInvalidRefusal    = apicode.Refusal{Condition: apicode.SessionGroupInvalid, Code: apicode.SessionGroupInput}
	sessionAlreadyGroupedRefusal  = apicode.Refusal{Condition: apicode.SessionAlreadyGrouped, Code: apicode.SessionInGroup}
	sessionGroupNameHeldRefusal   = apicode.Refusal{Condition: apicode.SessionGroupNameHeld, Code: apicode.SessionGroupNameTaken}
	sessionGroupNameTitledRefusal = apicode.Refusal{Condition: apicode.SessionGroupNameTitled, Code: apicode.SessionGroupNameIsSessionTitle}
)

// sessionGroupWriteRefusals are the refusals of a session group's create and of its edit, which
// write a group's name and sessions alike (createDeliverySessionGroupFrom,
// updateDeliverySessionGroupFrom), after more, the route's own.
func sessionGroupWriteRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(more, sessionGroupInvalidRefusal, sessionAlreadyGroupedRefusal, sessionGroupNameHeldRefusal, sessionGroupNameTitledRefusal)
}

// deliveryUnconfiguredRefusal is the refusal of a route that reads the deployment's delivery
// settings (deliverySettings) on a Dispatch that holds none.
var deliveryUnconfiguredRefusal = apicode.Refusal{Condition: apicode.DeliveryUnconfigured, Code: apicode.DeliveryNotConfigured}

// sessionGroupText is what the three session-group writes say of a group and of who may write
// one.
const sessionGroupText = "A group is a stable id, a name and the agent sessions it holds, and the Delivery page " +
	"shows it as the Parent agent of every pull request whose first session it holds. Any signed-in person or " +
	"agent writes one, a bearer naming its session in actor, and no event is appended; a credential scoped to " +
	"one project is refused, since a group belongs to none."

func v2SessionGroupIDs() *idRoute { return &idRoute{Reason: "a Delivery session group id"} }

// agentsV2Query is GET /api/v2/agents's query: one page.
type agentsV2Query struct {
	pageQuery
}

// listAgentsV2 answers GET /api/v2/agents: one page of the list GET /api/v1/agents answers whole
// (agentRows).
func (s *server) listAgentsV2(w http.ResponseWriter, r *http.Request) {
	rows, ok := s.agentRows(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(rows, v2Query[agentsV2Query](r).pageQuery))
}

// sessionAnswersV2Query is GET /api/v2/agents/{session_id}/answers's query: the window on at, the
// page, and the cap on the page's words.
type sessionAnswersV2Query struct {
	pageQuery
	From     *time.Time `query:"from"`
	To       *time.Time `query:"to"`
	MaxChars *int       `query:"max_chars" min:"1" max:"100000"`
}

// sessionAnswersV2 is GET /api/v2/agents/{session_id}/answers's answer: the envelope, beside the
// session and the window it was read over, to null where the request named none.
type sessionAnswersV2 struct {
	SessionID string     `json:"session_id"`
	From      time.Time  `json:"from"`
	To        *time.Time `json:"to"`
	page[model.SessionAnswer]
}

// listSessionAnswersV2 answers GET /api/v2/agents/{session_id}/answers: the items GET
// /api/v1/agents/{session_id}/answers reads (sessionanswers.List), over [from, to), one page of
// them, the page cut at max_chars as /api/v1 cuts its whole list (sessionAnswersWithin).
func (s *server) listSessionAnswersV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[sessionAnswersV2Query](r)
	now := time.Now().UTC()
	from := now.Add(-sessionAnswersWindow)
	if query.From != nil {
		if from, err = sessionAnswersStart("from", *query.From, now); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	if query.To != nil && !query.To.After(from) {
		writeError(w, r, apicode.InvalidQuery, "to must be after from")
		return
	}
	items, err := sessionanswers.List(r.Context(), s.deps.Store.Pool, session, from)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if query.To != nil {
		items = slices.DeleteFunc(items, func(item model.SessionAnswer) bool { return !item.At.Before(*query.To) })
	}
	answer := sessionAnswersV2{SessionID: session, From: from, To: query.To, page: pageOf(items, query.pageQuery)}
	if query.MaxChars != nil {
		answer.Items = sessionAnswersWithin(answer.Items, *query.MaxChars)
	}
	WriteJSON(w, http.StatusOK, answer)
}

// blockLinksV2Query is GET /api/v2/block-links's query: the two documents, and the page.
type blockLinksV2Query struct {
	pageQuery
	// From is the document whose blocks hold the links, as a dispatch:// reference.
	From string `query:"from"`
	// To is the document whose blocks the links reach.
	To string `query:"to"`
}

// blockLinksV2 is GET /api/v2/block-links's answer: the links in the envelope, the two document
// nodes beside it.
type blockLinksV2 struct {
	From model.GraphNode `json:"from"`
	To   model.GraphNode `json:"to"`
	page[model.BlockLink]
}

// getBlockLinksV2 answers GET /api/v2/block-links: one page of the links GET /api/v1/block-links
// answers whole, a document that does not exist answered with its own 404.
func (s *server) getBlockLinksV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[blockLinksV2Query](r)
	who := s.requestReader(r)
	documents, err := s.blockLinkDocuments(r.Context(), who, [2]string{query.From, query.To}, apicode.ArtifactNotFound)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	links, err := refs.BlockLinks(r.Context(), s.deps.Store.Pool, who, documents[0].ID, documents[1].ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, blockLinksV2{From: documents[0], To: documents[1], page: pageOf(links, query.pageQuery)})
}

// referenceTargetsV2Query is GET /api/v2/reference-targets's query: the references to resolve,
// and the page of their members.
type referenceTargetsV2Query struct {
	pageQuery
	// Ref is one reference to resolve, given once per reference, at most referenceTargetsMax of
	// them; naming none is 400 INVALID_REFERENCE and naming more than the cap 400 CAP_EXCEEDED.
	// An item id or a block id written short in it is resolved in the scope the reference names.
	Ref []string `query:"ref"`
}

// listReferenceTargetsV2 answers GET /api/v2/reference-targets: one page of the members GET
// /api/v1/reference-targets answers whole (referenceTargets), read from the decoded query, under
// an ETag of its own. The trees are resolved before the tag is made, as the v1 read resolves them,
// because the tag hashes them.
func (s *server) listReferenceTargetsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[referenceTargetsV2Query](r)
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	targets, err := s.referenceTargets(r.Context(), s.requestReader(r), query.Ref, trees)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	trees.resolve()
	answer := pageOf(targets, query.pageQuery)
	if answerNotModified(w, r, referenceTargetsV2ETag(answer)) {
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// referenceTargetsV2ETag is GET /api/v2/reference-targets's validator: the page's members, hashed
// as the v1 read hashes its whole answer (hashReferenceTargets), after the version and the
// page's bounds, so a page and the v1 answer of the same set never share a tag.
func referenceTargetsV2ETag(answer page[model.ReferenceTarget]) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\ntotal=%d\nlimit=%d\noffset=%d\n", answer.Total, answer.Limit, answer.Offset)
	hashReferenceTargets(hash, answer.Items)
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

// searchResultsV2Description is GET /api/v2/search-results's description.
var searchResultsV2Description = fmt.Sprintf("Full-text search q= across issues, documents, asks, comments and "+
	"messages: every match ranked on one list across the kinds by one score, the keyword leg's length-normalised "+
	"rank and the meaning leg's cosine weighted onto one scale and summed, with total, every match, and reachable, "+
	"how many the pages can reach (each leg's best %d). A comment's, a message's and an ask's hit carries author, "+
	"the actor that wrote it. A q that is one issue key in any form a person writes it - "+
	"INFRA-1042, infra-1042, INFRA 1042, or the number alone, which names that issue of the project the number is "+
	"read in, or of every project at once, all of them then heading the list in project order - puts that issue "+
	"first. The number is read in the project the search is narrowed to where it names one, and otherwise in "+
	"number_project, the project of the page the search was made from, which narrows nothing. A q is searched as "+
	"typed, websearch_to_tsquery's AND of its words; only a q that matches nothing at all is searched a second time "+
	"for its %d rarest words ORed, and the answer's query {operators, broadened} names the forms it read and whether "+
	"that second search answered. project=<key>, repeatable, narrows every leg to those projects, with no default "+
	"project; status= the status of the issue a row belongs to (a row with no issue matches none); author= the "+
	"actor id that wrote the item a row names; from and to, RFC3339, from inclusive and to exclusive, when that "+
	"item was last written. filters echoes {project, status, author, from, to}, project [] for none. "+
	"kind=, repeatable, one of issue, document, comment, ask and message, keeps the rows of the kinds named alone, "+
	"before anything is ranked, counted or paged, and none is every kind, as GET /api/v1/search/results reads it. "+
	"keywords_only=true runs the keyword leg alone and makes no embedding call, and its answer is not stamped "+
	"degraded; degraded names why a search that wanted meaning answered by keywords alone. Ordered by score, then "+
	"recency, then id. limit 1 to %d (default %d) and offset; {items, total, limit, offset, reachable, took_ms, "+
	"degraded?, query, filters}. A q under 2 characters and a from not before to are 400 INVALID_QUERY, and a q over "+
	"%d characters 400 CAP_EXCEEDED; a project or number_project that is not a project key is 400 INVALID_PROJECT or "+
	"INVALID_NUMBER_PROJECT, and a status or an author that is none is 400 INVALID_STATUS or INVALID_AUTHOR.",
	contracts.SearchKindDepth, searchBroadenWords, searchMaxLimit,
	searchDefaultLimit, contracts.SearchQueryMax)

// searchResultsV2Query is GET /api/v2/search-results's query.
type searchResultsV2Query struct {
	pageQuery
	Q string `query:"q"`
	// Project narrows every leg to these projects; none is every project.
	Project []string `query:"project"`
	// NumberProject is the project a bare issue number is read in where the search names none.
	NumberProject string     `query:"number_project"`
	Status        *string    `query:"status"`
	Author        *string    `query:"author"`
	From          *time.Time `query:"from"`
	To            *time.Time `query:"to"`
	KeywordsOnly  bool       `query:"keywords_only"`
	// Kind keeps the rows of the kinds named alone; none is every kind (searchKinds).
	Kind []string `query:"kind" enum:"issue,document,comment,ask,message"`
}

// searchFiltersV2 is what a /api/v2 search narrowed every leg by: the projects, [] for every
// project, and the status, the author and the window, each left out where the request named none.
type searchFiltersV2 struct {
	Project []string `json:"project"`
	Status  string   `json:"status,omitempty"`
	Author  string   `json:"author,omitempty"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
}

// searchResultsV2 is GET /api/v2/search-results's answer: GET /api/v1/search/results's, its
// results the envelope's items, and what describes the page beside them.
type searchResultsV2 struct {
	page[model.SearchResult]
	Reachable int                      `json:"reachable"`
	TookMS    int64                    `json:"took_ms"`
	Degraded  string                   `json:"degraded,omitempty"`
	Query     model.SearchQueryReading `json:"query"`
	Filters   searchFiltersV2          `json:"filters"`
}

// searchResultsV2 answers GET /api/v2/search-results: the search GET /api/v1/search/results runs
// (searchAnswer), over the projects the request names, read as the acting person (actingReader),
// as /api/v1's is.
func (s *server) searchResultsV2(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	query := v2Query[searchResultsV2Query](r)
	parameters, problem := searchParametersV2(r, query)
	if problem != nil {
		s.writeHandlerError(w, r, problem)
		return
	}
	parameters.who = who
	response, err := s.searchAnswer(r, parameters)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	filters := searchFiltersV2{Project: parameters.projects, Status: parameters.status, Author: parameters.author}
	switch {
	case parameters.project != "":
		filters.Project = []string{parameters.project}
	case filters.Project == nil:
		filters.Project = []string{}
	}
	if parameters.since != nil {
		filters.From = parameters.since.Format(time.RFC3339Nano)
	}
	if parameters.until != nil {
		filters.To = parameters.until.Format(time.RFC3339Nano)
	}
	WriteJSON(w, http.StatusOK, searchResultsV2{
		page:      newPage(response.Results, response.Total, query.pageQuery),
		Reachable: response.Reachable, TookMS: response.TookMS, Degraded: response.Degraded,
		Query: response.Query, Filters: filters,
	})
}

// searchParametersV2 is the search a /api/v2 query asks for, or the refusal of a value no version
// of the search route takes, with /api/v1's code for each.
func searchParametersV2(r *http.Request, query searchResultsV2Query) (searchParameters, *apiError) {
	text := strings.TrimSpace(query.Q)
	if problem := searchTextProblem(text); problem != nil {
		return searchParameters{}, problem
	}
	var projects []string
	for _, value := range query.Project {
		project := strings.TrimSpace(value)
		if !projectKeyPattern.MatchString(project) {
			return searchParameters{}, errorf(apicode.InvalidProject, "%s", projectKeyProblem("project", project))
		}
		if !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	numberProject := strings.TrimSpace(query.NumberProject)
	if numberProject != "" && !projectKeyPattern.MatchString(numberProject) {
		return searchParameters{}, errorf(apicode.InvalidNumberProject, "%s", projectKeyProblem("number_project", numberProject))
	}
	// The decoder holds each kind to searchIndexedKinds, the enum above, so every value is known.
	kinds, _ := parseSearchKinds(query.Kind)
	parameters := searchParameters{text: text, numberProject: numberProject, since: query.From, until: query.To,
		kinds: kinds, limit: query.Limit, offset: query.Offset, keywordsOnly: query.KeywordsOnly, trees: wantsDisplayTrees(r)}
	if query.Status != nil {
		if parameters.status = strings.TrimSpace(*query.Status); !model.IsIssueStatus(parameters.status) {
			return searchParameters{}, searchStatusRefusal()
		}
	}
	if query.Author != nil {
		if parameters.author = strings.TrimSpace(*query.Author); !searchAuthorTaken(parameters.author) {
			return searchParameters{}, searchAuthorRefusal()
		}
	}
	if query.From != nil && query.To != nil && !query.From.Before(*query.To) {
		return searchParameters{}, errorf(apicode.InvalidQuery, "from must be before to")
	}
	if len(projects) == 1 {
		parameters.project = projects[0]
	} else {
		parameters.projects = projects
	}
	reading := readSearchQuery(text)
	parameters.reading = &reading
	return parameters, nil
}

// goingActionsV2Query is GET /api/v2/going-actions's query: a page of the catalogue, which is read
// whole where the request names no limit.
type goingActionsV2Query struct {
	pageQuery
}

// listGoingActionsV2 answers GET /api/v2/going-actions: the /api/v2 catalogue (goingToolRoutesV2),
// as GET /api/v1/going/catalogue answers the /api/v1 one.
func (s *server) listGoingActionsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(s.goingV2.actionsV2, v2Query[goingActionsV2Query](r).pageQuery))
}

// createGoingRequestV2 answers POST /api/v2/going-requests: the question POST
// /api/v1/going/requests records and answers (createGoingRequestFrom).
func (s *server) createGoingRequestV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.goingAsker(w, r)
	if !ok {
		return
	}
	s.createGoingRequestFrom(w, r, actor, v2Body[createGoingRequestInput](r))
}

// usersV2Query is GET /api/v2/users's query: one page.
type usersV2Query struct {
	pageQuery
}

// listUsersV2 answers GET /api/v2/users: one page of the people GET /api/v1/users lists whole
// (signedInUsers).
func (s *server) listUsersV2(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	users, err := s.signedInUsers(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(users, v2Query[usersV2Query](r).pageQuery))
}

// dashboardBuildsV2Query is GET /api/v2/dashboard-builds's query: one page.
type dashboardBuildsV2Query struct {
	pageQuery
}

// dashboardBuildV2 is one dashboard build a live server process serves: its entry chunk, and
// whether it is the build this process serves.
type dashboardBuildV2 struct {
	Entry   string `json:"entry"`
	Serving bool   `json:"serving"`
}

// listDashboardBuildsV2 answers GET /api/v2/dashboard-builds: the builds GET
// /api/v1/dashboard/builds answers as live (liveDashboardEntries), each marked whether this process
// serves it.
func (s *server) listDashboardBuildsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	entries, err := s.liveDashboardEntries(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	builds := make([]dashboardBuildV2, len(entries))
	for index, entry := range entries {
		builds[index] = dashboardBuildV2{Entry: entry, Serving: entry == s.deps.DashboardEntry}
	}
	WriteJSON(w, http.StatusOK, pageOf(builds, v2Query[dashboardBuildsV2Query](r).pageQuery))
}

// createSessionGroupV2 answers POST /api/v2/session-groups: the group POST
// /api/v1/delivery/session-groups creates (createDeliverySessionGroupFrom).
func (s *server) createSessionGroupV2(w http.ResponseWriter, r *http.Request) {
	s.createDeliverySessionGroupFrom(w, r, v2Body[createDeliverySessionGroupInput](r))
}

// requireSessionGroupV2 answers whether the group id names exists, and where it does not writes 404
// SESSION_GROUP_NOT_FOUND (requireHeldV2).
func (s *server) requireSessionGroupV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "session group", `select exists (select 1 from delivery_session_groups where id = $1::uuid)`,
		apicode.SessionGroupNotFound, "no session group "+id)
}

// deleteSessionGroupV2 answers DELETE /api/v2/session-groups/{id}: the delete DELETE
// /api/v1/delivery/session-groups/{id} makes (deleteDeliverySessionGroupAs), the group held to
// exist first.
func (s *server) deleteSessionGroupV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireSessionGroupV2(w, r, r.PathValue("id")) {
		return
	}
	s.deleteDeliverySessionGroupAs(w, r, v2Body[actorInput](r))
}

// updateSessionGroupV2 answers PATCH /api/v2/session-groups/{id}: the edit PATCH
// /api/v1/delivery/session-groups/{id} writes (updateDeliverySessionGroupFrom), the group held to
// exist first.
func (s *server) updateSessionGroupV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireSessionGroupV2(w, r, r.PathValue("id")) {
		return
	}
	s.updateDeliverySessionGroupFrom(w, r, v2Body[updateDeliverySessionGroupInput](r))
}

// postMarkdownRoundTripV2 answers POST /api/v2/markdown-round-trips: what POST
// /api/v1/markdown/round-trip answers (answerMarkdownRoundTrip), its body read by the decoder
// within the row's own bound and without its refusal of U+0000, which the answer makes in the
// upload's words (requestBody.TextChecked).
func (s *server) postMarkdownRoundTripV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.answerMarkdownRoundTrip(w, r, v2Body[markdownRoundTripInput](r))
}

// postFileRoundTripV2 answers POST /api/v2/markdown-file-round-trips: what POST
// /api/v1/markdown/file-round-trip answers (answerFileRoundTrip), its body read by the decoder
// within the row's own bound and, as /api/v1 reads it, without its refusal of U+0000: the answer
// refuses a U+0000 in the file's bytes in the upload's words, and one in a member the body does
// not declare is ignored (requestBody.TextChecked).
func (s *server) postFileRoundTripV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.answerFileRoundTrip(w, r, v2Body[fileRoundTripInput](r))
}

// ciReconcilePassesV2Query is GET /api/v2/ci-reconcile-passes's query: the window on started_at,
// and the page.
type ciReconcilePassesV2Query struct {
	pageQuery
	From *time.Time `query:"from"`
	To   *time.Time `query:"to"`
}

// listCIReconcilePassesV2 answers GET /api/v2/ci-reconcile-passes: the passes that started within
// [from, to) (cireconcile.Passes), one page of them, where GET /api/v1/ci-reconcile/passes answers
// the last 24 hours whole (cireconcile.RecentPasses). A request naming neither bound reads from 24
// hours before the database's clock on, that instant included, which /api/v1's window leaves out;
// one naming to alone reads the 24 hours before it.
func (s *server) listCIReconcilePassesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[ciReconcilePassesV2Query](r)
	from := query.From
	if from == nil && query.To != nil {
		from = new(query.To.Add(-cireconcile.PassWindow))
	}
	if from != nil && query.To != nil && !from.Before(*query.To) {
		writeError(w, r, apicode.InvalidQuery, "from must be before to")
		return
	}
	passes, err := cireconcile.Passes(r.Context(), s.deps.Store.Pool, from, query.To)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(passes, query.pageQuery))
}
