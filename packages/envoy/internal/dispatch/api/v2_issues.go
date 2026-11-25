package api

// v2_issues.go is the issues of /api/v2 (R1 of dispatch://INFRA-4071/spec): the
// list of issues across projects, which also answers for GET /api/v1/issues/resolve (ref=), and a
// project's own list of the same rows (R1.3 of dispatch://INFRA-2946/spec); the create; the lean
// read of one issue, whose include= gives what GET /api/v1/issues/{key} answered (the route map's
// J3); the patch, which takes the design gates PUT /api/v1/issues/{key}/design-gates wrote (J16);
// the claim; and an issue's collaborators, coverage, decisions and the keep of one, references,
// checks, results and the retraction of one, subscribers, the steps #4400 added to /api/v1 and the
// write of one, the rating of a retro's run and the plan of a run #4488 added, and the dry run of a
// result #4517 added. Every route runs the work its /api/v1 route runs (readIssueListing,
// createIssueFrom, readIssueParts, patchIssueAs, claimIssueAs, releaseIssueClaimAs,
// issueCollaboratorsOf, issueCoverage, issueDecisionsOf, writeDecisionKeep, issueClosure,
// readIssueChecks, recordCheckResultFrom, dryRunCheckResultFrom, retractCheckResultAs, subscribersOf,
// unsubscribeSession, issueStepTasks, putIssueStepFrom, putIssueStepRatingFrom, putIssuePlanFrom),
// each path object checked first and answered with its own 404, and a {session_id} keeps the answer
// /api/v1 gives it (R7).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// resultMissingRefusal is the refusal of a result id no check result has
// (dispatch://INFRA-4071/spec R7).
var resultMissingRefusal = apicode.Refusal{Condition: apicode.ResultMissing, Code: apicode.ResultNotFound}

// stepMissingRefusal is the refusal of a step record id no record holds, under a write that does
// not name the run's identity and so cannot make one (dispatch://INFRA-4071/spec R7).
var stepMissingRefusal = apicode.Refusal{Condition: apicode.StepMissing, Code: apicode.StepNotFound}

// stepRecordText is what every step route's description says of the record it answers.
const stepRecordText = "{id, issue, task, step, kind, role, type, agent, session, model, generation, attempt, " +
	"started_at, ended_at, result, returned_to, report, rating, rating_reason}, attempt its place among the " +
	"issue's runs of the same task, step and kind from 1, and rating and rating_reason a retro's alone"

// The refusals of a parent or blocked_by write that would make a loop, which /api/v2 answers with
// one code, 409 ISSUE_CYCLE, where /api/v1 answers PARENT_INPUT and DEPENDENCY_CYCLE (R7).
var (
	parentLoopRefusal     = apicode.Refusal{Condition: apicode.ParentLoop, Code: apicode.ParentCycle}
	dependencyLoopRefusal = apicode.Refusal{Condition: apicode.DependencyLoop, Code: apicode.DependencyCycle}
)

// The refusals of the members an issue's create and its patch both read (createIssueFrom,
// patchIssueAs), and of the filters both lists of issues read (answerIssueListV2): a priority and
// labels; and of an owner/repo#n reference of another shape (externalRef), which only POST
// /api/v2/issues (external) and GET /api/v2/issues (ref=) read.
var (
	priorityInvalidRefusal = apicode.Refusal{Condition: apicode.PriorityInvalid, Code: apicode.InvalidPriority}
	labelsInvalidRefusal   = apicode.Refusal{Condition: apicode.LabelsInvalid, Code: apicode.LabelsInput}
	issueRefInvalidRefusal = apicode.Refusal{Condition: apicode.IssueRefInvalid, Code: apicode.InvalidIssueRef}
)

// issueFieldRefusals are the refusals of the members an issue's create and its patch both take,
// with more, the route's own, after them: its advances, assignee, blocked_by, components,
// priority, labels and parent, a title past its bound, and the input either refuses whole.
func issueFieldRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append([]apicode.Refusal{
		undeclaredKeyRefusal, valuePastBoundRefusal, assigneeNotSignedInRefusal, priorityInvalidRefusal,
		labelsInvalidRefusal,
		{Condition: apicode.AdvancesInvalid, Code: apicode.AdvancesInput},
		{Condition: apicode.BlockedByInvalid, Code: apicode.BlockedByInput},
		{Condition: apicode.ComponentsInvalid, Code: apicode.ComponentsInput},
		{Condition: apicode.IssueInputInvalid, Code: apicode.InvalidIssue},
		{Condition: apicode.ParentInvalid, Code: apicode.ParentInput},
		{Condition: apicode.ParentChainTooDeep, Code: apicode.ParentTooDeep},
	}, more...)
}

// issueListRefusals are the refusals of both lists of issues (answerIssueListV2), with more, the
// route's own, after them: a list's, the filters it refuses, the listener a route filter or reach
// reads, and the acting person pinned=true reads.
func issueListRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return listRefusals(append(envoyListenerRefusals(
		priorityInvalidRefusal, labelsInvalidRefusal, actingSessionMismatchRefusal,
		apicode.Refusal{Condition: apicode.AdvancesFilterInvalid, Code: apicode.InvalidAdvances},
		apicode.Refusal{Condition: apicode.ClosedWindowInverted, Code: apicode.InvalidClosedWindow},
	), more...)...)
}

// The refusals of an issue's claim (claimCycle): a holder that moved twice, and a holder the
// caller may not displace.
var (
	claimHolderMovedRefusal   = apicode.Refusal{Condition: apicode.ClaimHolderMoved, Code: apicode.ClaimContended}
	issueHeldByAnotherRefusal = apicode.Refusal{Condition: apicode.IssueHeldByAnother, Code: apicode.IssueClaimed}
)

// claimCycleRefusals are the refusals of both claim routes (claimCycle), with more, the route's
// own, after them: an issue that does not exist, a holder that moved twice, a holder the caller
// may not displace, and the listener that judges a session holder's liveness.
func claimCycleRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append([]apicode.Refusal{
		undeclaredKeyRefusal, issueMissingRefusal, claimHolderMovedRefusal, issueHeldByAnotherRefusal,
		envoyUnreachableRefusal,
	}, more...)
}

// checkResultRefusals are the refusals a result's record and its dry run both answer before the
// record writes (readResultRequest, checkResultInput.refusal), with more, the route's own, after them,
// and the four of a document the document service could not serve, since both read the check's
// attributes out of the live document (resolveCheck, checkBlockAttributes).
func checkResultRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(documentRefusals([]apicode.Refusal{
		undeclaredKeyRefusal, issueMissingRefusal, gitHubReadFailedRefusal, resultInputInvalidRefusal,
		{Condition: apicode.ResultCountsContradict, Code: apicode.ResultContradicts},
		{Condition: apicode.CheckMissing, Code: apicode.UnknownCheck},
		{Condition: apicode.CheckRefAmbiguous, Code: apicode.CheckAmbiguous},
		{Condition: apicode.ResultOfCICheck, Code: apicode.CheckProvedByCI},
		{Condition: apicode.ResultCommitUnheld, Code: apicode.ResultCommitUnknown},
	}...), more...)
}

// The refusals of a result's input, and of a step's, a rating's and a plan's write
// (writeIssueStep, putIssuePlanFrom).
var (
	resultInputInvalidRefusal = apicode.Refusal{Condition: apicode.ResultInputInvalid, Code: apicode.ResultInput}
	stepInputInvalidRefusal   = apicode.Refusal{Condition: apicode.StepInputInvalid, Code: apicode.StepInput}
	stepOfAnotherIssueRefusal = apicode.Refusal{Condition: apicode.StepOfAnotherIssue, Code: apicode.StepOtherIssue}
	stepWriterRefusal         = apicode.Refusal{Condition: apicode.StepWriterNotTheDaemon, Code: apicode.StepWriter}
)

// stepWriteRefusals are the refusals of both step routes (requireIssueDaemon, writeIssueStep): an
// issue that does not exist, an id no record holds under a write that names no identity to make
// one, a value outside its form, a record of another issue, and a writer other than the session
// of its project's Legion daemon.
func stepWriteRefusals() []apicode.Refusal {
	return []apicode.Refusal{
		undeclaredKeyRefusal, issueMissingRefusal, stepMissingRefusal, stepInputInvalidRefusal,
		stepOfAnotherIssueRefusal, stepWriterRefusal,
	}
}

// issuePinnedAuthority is the authority of GET /api/v2/issues's pinned=true (authorities_v2.go):
// the pins are one person's, so reading them needs the person scope.
var issuePinnedAuthority = authorityRule{
	Where: authorityWhere{In: partQuery, Name: "pinned"}, Value: "true", Scopes: routeScopeSet{model.ScopePerson},
}

// The two reasons a check that compared with nothing names on /api/v2 in place of /api/v1's one,
// model.ReasonNoPullRequestOrBranch (R10 of dispatch://INFRA-4071/spec): the check's issue
// resolves to a repository and holds no pull request, or it resolves to no repository at all.
// /api/v1 keeps the one reason for both, since the deployed `dispatch` command keys a record by it.
const (
	reasonNoPullRequest = "no_pull_request"
	reasonNoRepository  = "no_repository"
)

// issueListText is what the descriptions of both lists of issues say of the filters, the order,
// the row and the bounds alike.
var issueListText = fmt.Sprintf("status=, repeatable, keeps the issues standing in any of those statuses (a "+
	"blank value names none, and a value is matched as written, so a status the lifecycle does not hold matches no "+
	"issue); parent=<key> an issue's children; label=, repeatable, the issues carrying every label named; priority=, "+
	"repeatable, 0 to 3 or none for unset; advances=, repeatable, a priority id, none for the work that advances no "+
	"live priority or any for the work that advances one, own or inherited, several a union; open=true the open "+
	"issues; updated_from and updated_to window updated_at, and closed_from and closed_to closed_at, RFC3339, each "+
	"from inclusive and to exclusive, so an open issue matches no closed window (a closed_to at or before closed_from "+
	"is 400 INVALID_CLOSED_WINDOW, an updated_to at or before updated_from 400 INVALID_QUERY); route_status=live, "+
	"holder_unresponsive, no_holder or unknown keeps the open issues whose route stands so (503 ENVOY_UNAVAILABLE or "+
	"ENVOY_REFILLING where the listener cannot judge a route); pinned=true the issues the caller pinned, which needs "+
	"the person scope; moved=true adds moved_at and moved_by to each issue standing in backlog or icebox, when it was "+
	"moved there and by whom. Issues come in lifecycle order of their status, then by rank, creation time and key. "+
	"Each is one row: its own fields, its route's reach (route_status, route_holder, route_status_reason), "+
	"closed_at, open_asks and progress {tasks, children}, each null when it has none. limit 1 to %d (default %d) and "+
	"offset; {items, total, limit, offset}, total counting every issue the filters match.",
	contracts.MaxIssuePageLimit, contracts.DefaultIssuePageLimit)

// issueClaimText is what both claim routes' descriptions say of their answer.
const issueClaimText = "The status does not move. Answers the issue with legion_managed_status beside it: true when " +
	"the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns " +
	"the status and a status another writer sets is undone. 409 CLAIM_CONTENDED means the holder changed twice while " +
	"the request ran, so nothing was applied and nobody's liveness was checked: read the issue and retry."

var parentStatusConflictRefusal = apicode.Refusal{Condition: apicode.ParentStatusRuleBroken, Code: apicode.ParentStatusConflict}

// v2IssueRows are the issue routes: rows 80 and 82; 81; the project's
// list R1.3 adds; 83 and 115; 84 and 102; 95; 96; 97; 100; 101; 17; 110; 111; 112; 113; 114; and
// 173 of the route map; and five rows of /api/v1 the map predates (testdata/v1-only-routes.txt):
// #4400's two, GET /issues/{key}/steps and PUT /issues/{key}/steps/{id}, #4488's two, PUT
// /issues/{key}/steps/{id}/rating and PUT /issues/{key}/plans/{id}, and #4517's POST
// /issues/{key}/results/dry-run.
func (s *server) v2IssueRows() []routeRow {
	person := routeScopeSet{model.ScopePerson}
	issueList := listBounds{Default: contracts.DefaultIssuePageLimit, Max: contracts.MaxIssuePageLimit}
	issueTimes := []string{"updated_at", "closed_at"}
	claimRefusals := []routeAnswer{{Status: http.StatusConflict, Body: issueClaimedError{}}}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/issues", Auth: authAny, Versions: onlyV2,
			Description: "List the issues of every project, filtered. project=<key>, repeatable, keeps those projects' " +
				"issues, with no default project; GET /api/v2/projects/{key}/issues lists one project's. ref=<KEY or " +
				"owner/repo#n> keeps the issue a key names, or every issue linked to that GitHub issue or pull request (an " +
				"issue imported from it, or one a link names), none where nothing is; any other ref is 400 " +
				"INVALID_ISSUE_REF. " + issueListText,
			Handler:  s.listIssuesV2,
			Resource: "issues", Kind: kindCollection, Key: "{key}",
			TimeFields: issueTimes, CrossProject: true,
			Query:       issueListV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[model.IssueSummary]{}},
			List:        issueList,
			Authorities: []authorityRule{issuePinnedAuthority},
			Refusals:    issueListRefusals(issueRefInvalidRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/issues", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Create an issue {project, title, parent?, blocked_by?, advances?, external?, "+
				"force?, spec?, labels?, priority?, assignee?, components?, repo?}, a bearer naming its session in actor: a "+
				"native issue in project, or one imported from a GitHub owner/repo#n named in external into project. Every "+
				"create names its project, an import included: external with no project, or an empty one, is 400 "+
				"INVALID_ISSUE, and an external ref already linked to an issue of another project is 400 "+
				"EXTERNAL_PROJECT_MISMATCH. assignee defaults to the creating person, the personal token's owner, "+
				"the parent's assignee when the parent is of the issue's own project, or the project's default "+
				"assignee. A title over %d "+
				"characters is 400 CAP_EXCEEDED. An unforced create whose title is a near copy of another issue's is 409 "+
				"POSSIBLE_DUPLICATE with the candidates. Answers 201 the issue with advice beside it, and 200 the issue an "+
				"external ref already imported. After the write commits, a related-item check searches the project by "+
				"meaning and by its rarest words and waits at most 5 seconds; the advice's suggestions say whether it found "+
				"related items (outcome found, none or not_made with the reason in missing), how it searched, the three items "+
				"most like the new issue with each ask item's answer state, words to search with by hand (search_query), and "+
				"under force the near copies forced past (title_matches). A blocked_by that would make the new issue wait on "+
				"itself through its parent is 409 ISSUE_CYCLE.", contracts.IssueTitleMax),
			// A new child is checked against its parent's status under the same lock as /api/v1.
			Handler:  s.createIssueV2,
			Resource: "issues", Kind: kindCollection, Key: "{key}",
			Body: requestBody{Kind: bodyJSON, Type: createIssueInput{}},
			Shape: routeShape{
				Status: http.StatusCreated, Response: model.Issue{}, Advice: writeAdvice{},
				OtherAdvice: []any{suggestionsOnlyAdvice{}},
				// An import whose reference already has an issue answers that issue with the advice
				// and suggestions a create writes (issue_create.go, the existing-issue arm).
				Also: []routeAnswer{{
					Status: http.StatusOK, Body: model.Issue{},
					Advice: writeAdvice{}, OtherAdvice: []any{suggestionsOnlyAdvice{}},
				}},
				Refusals: []routeAnswer{
					{Status: http.StatusConflict, Body: issueDuplicateError{}},
					{Status: http.StatusConflict, Body: parentStatusConflictBody{}},
				},
			},
			Scope: scopeProjectInBody,
			// The spec is seeded straight into the store (docs.SeedText), which opens no room.
			Refusals: issueFieldRefusals(markdownWriteRefusals(
				dependencyLoopRefusal, issueRefInvalidRefusal,
				parentStatusConflictRefusal,
				apicode.Refusal{Condition: apicode.IssueRefAmbiguous, Code: apicode.AmbiguousIssueRef},
				apicode.Refusal{Condition: apicode.ExternalOfAnotherProject, Code: apicode.ExternalProjectMismatch},
				apicode.Refusal{Condition: apicode.TitleNearCopy, Code: apicode.PossibleDuplicate},
				apicode.Refusal{Condition: apicode.ParentOutsideCallerProject, Code: apicode.ProjectScope},
			)...),
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/issues", Auth: authAny, Versions: onlyV2,
			Description: "List one project's issues, filtered: the rows GET /api/v2/issues?project={key} answers for the " +
				"same filters. " + issueListText + " 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.listProjectIssuesV2,
			Resource: "issues", Kind: kindCollection, Key: "{key}",
			TimeFields:  issueTimes,
			Query:       projectIssuesV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[model.IssueSummary]{}},
			List:        issueList,
			Authorities: []authorityRule{issuePinnedAuthority},
			Refusals:    issueListRefusals(projectMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}", Auth: authAny, Versions: onlyV2,
			Description: "Read one issue: its own fields, and artifacts, each of its documents, files and images in " +
				"creation order as GET /api/v2/issues/{key}/artifacts lists them. include= adds, each as a member of its " +
				"name: blocks, the keys of the issues whose blocked_by names it, sorted, [] when none, which no row of GET " +
				"/api/v2/issues carries; comment_count, how many comments GET /api/v2/issues/{key}/comments lists with no " +
				"artifact=, 0 when it holds none; progress {tasks, children}, its spec's task-list items and its direct " +
				"children as {done, total}, each null when it has none; open_asks, its open asks, oldest first, each as GET " +
				"/api/v2/asks answers it; children, its direct children, each with a rollup of its subtree; design_gates, the " +
				"document each design-gate step records and whether the deployment's gate holds the issue's tree on it, null " +
				"where the daemon said " +
				"nothing, [{step, artifact_id, holds}] in step order; reach {route_status, route_holder, route_status_reason, " +
				"holder_state, last_seen, takeable_at}, where its route reaches now and what state the session holding its " +
				"claim is in, read from the Envoy listener, each null where there is none or the listener did not answer; " +
				"referenced_by_count, how many references to it stand; and legion {issue, project, configured, state, read, " +
				"runs_revision}, its Legion state from what the sampler recorded of the daemon of its project, the body a " +
				"/ws/live legion_issue view of it carries. In legion, configured is whether that project names a Legion " +
				"daemon, state is null where the daemon does not record the issue and otherwise {phase, phase_since, " +
				"phase_start_seen, roles, slot, waiting_line, blocked_by, waiting_on}, waiting_on in the shape GET " +
				"/api/v1/issues/{key}/legion answers it, read is the newest read of the daemon " +
				"{at, ok, error} or null where none was recorded, and runs_revision counts the tuples of its runs, 0 with no " +
				"run, and grows with each write of a run that stores something new. A project that names no daemon is not " +
				"refused: its issue answers configured false, state null and read null. 404 ISSUE_NOT_FOUND for an issue " +
				"that does not exist.",
			Handler:  s.getIssueV2,
			Resource: "issues", Kind: kindItem, Key: "{key}",
			Query:    issueReadV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: issueReadV2{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/issues/{key}", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Change an issue, a bearer naming its session in actor: title (over %d "+
				"characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the "+
				"issue holds when the write commits rather than replacing them; not with labels), route, parent, "+
				"blocked_by, advances (with advances_revision, the revision the read gave, for a whole-set write), rank, "+
				"assignee (the email of a person who has signed in, or null; anyone may reassign), external_links, repo or "+
				"components ({mode: inherit|explicit|none, ids, reason}); design_gates, the whole set of documents its "+
				"design-gate steps record [{step, artifact_id, holds}], step one of decisions, criteria and spec, "+
				"artifact_id the document that step's gate recorded and holds whether the deployment's gate holds the "+
				"issue's tree on that step (left out or null, stored as null, which the superseded_approval advice answers "+
				"by the project's gated_steps), or [] for none, which appends no event and notifies nobody; and "+
				"refuse_open_foreign_children, true beside a status of "+
				"retro or done, which the Legion daemon's acceptance writes send: while a child of another project is open "+
				"(any status but done and icebox), the write changes nothing and is 409 FOREIGN_CHILD_OPEN, naming each "+
				"such child in issues [{key, project, status}] in key order. A design_gates step named twice, one that is "+
				"none of the three, or an empty artifact_id, and refuse_open_foreign_children beside any other status, are "+
				"400 INVALID_ISSUE. A closed issue takes rank, components, design_gates, an "+
				"external_links write adding no URL and a reopening status, and is 409 ISSUE_CLOSED for anything else. "+
				"Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution "+
				"kind withdrawn naming the closer, and releases its claim; reopening it opens exactly those asks again. "+
				"expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer "+
				"that sequence. A parent that would make the issue its own ancestor, and a parent or blocked_by that would "+
				"make it wait on itself, are 409 ISSUE_CYCLE. With the parent-status rule on, a change that breaks it is "+
				"409 PARENT_STATUS_CONFLICT. Without status-change options, answers the issue with advice; a patch that "+
				"changes nothing appends no event. With include_children, unlink_children, include_parent or dry_run, "+
				"send status and optionally expected_sequence and actor, with no other issue fields. The server checks "+
				"the plan, then changes each affected issue in a separate transaction, keeping the sequence fence and "+
				"the progress of a keyed attempt. A dry run returns the plan without writing. This form answers "+
				"{issue, changed}; a refusal carries the changes that committed before it. Closing with include_children "+
				"requires a browser sign-in. 404 ISSUE_NOT_FOUND.", contracts.IssueTitleMax),
			Handler:  s.patchIssueV2,
			Resource: "issues", Kind: kindItem, Key: "{key}",
			Body: requestBody{Kind: bodyJSON, Type: patchIssueV2Input{}},
			Shape: routeShape{
				Status: http.StatusOK, Response: model.Issue{}, Advice: writeAdvice{},
				Also: []routeAnswer{{Status: http.StatusOK, Body: statusChangeAnswer{}}},
				Refusals: []routeAnswer{
					{Status: http.StatusConflict, Body: foreignChildOpenError{}},
					{Status: http.StatusConflict, Body: parentStatusConflictBody{}},
					{Status: http.StatusConflict, Body: statusChangeConflict{}},
					{Status: http.StatusConflict, Body: statusChangeRefusal{}},
					{Status: http.StatusBadRequest, Body: statusChangeRefusal{}},
					{Status: http.StatusForbidden, Body: statusChangeRefusal{}},
					{Status: http.StatusNotFound, Body: statusChangeRefusal{}},
					{Status: http.StatusUnprocessableEntity, Body: statusChangeRefusal{}},
					{Status: http.StatusInternalServerError, Body: statusChangeRefusal{}},
				},
			},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			KeyedFor: s.patchIssueKeyedRoute,
			// A close or a reopen writes the state of its asks into their blocks (docs.WriteIssueAskStates).
			Refusals: documentRefusals(issueFieldRefusals(
				issueMissingRefusal, parentLoopRefusal, dependencyLoopRefusal, issueClosedRefusal,
				apicode.Refusal{Condition: apicode.OpenForeignChild, Code: apicode.ForeignChildOpen},
				apicode.Refusal{Condition: apicode.AdvancesRevisionMoved, Code: apicode.AdvancesRevision},
				apicode.Refusal{Condition: apicode.ExternalLinkHeld, Code: apicode.ExternalLinkTaken},
				issueStatusInvalidRefusal,
				parentStatusConflictRefusal,
				apicode.Refusal{Condition: apicode.StatusChangeOptionsInvalid, Code: apicode.StatusChangeInput},
				apicode.Refusal{Condition: apicode.StatusCascadeNeedsBrowser, Code: apicode.BrowserSignInRequired},
				apicode.Refusal{Condition: apicode.ExternalURLInvalid, Code: apicode.InvalidURL},
				apicode.Refusal{Condition: apicode.IssueSequenceMoved, Code: apicode.IssueChanged},
				apicode.Refusal{Condition: apicode.ParentLockContended, Code: apicode.ParentContended},
				rankInvalidRefusal,
				apicode.Refusal{Condition: apicode.IssueRouteInvalid, Code: apicode.RouteInvalid},
			)...),
		},
		{
			Method: http.MethodDelete, Pattern: "/issues/{key}/claim", Auth: authAny, Versions: onlyV2,
			Description: "Release the issue's claim: its holder, a caller whose credential holds the person scope, or " +
				"anyone when the holding session is no longer live; a bearer names its session in actor. An issue nobody " +
				"holds answers the issue and writes nothing. " + issueClaimText + " 409 ISSUE_CLAIMED names the holder this " +
				"caller may not release; 404 ISSUE_NOT_FOUND.",
			Handler:  s.releaseIssueClaimV2,
			Resource: "claim", Kind: kindSingleton,
			Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
			Shape: routeShape{Status: http.StatusOK, Response: claimAnswer{}, Refusals: claimRefusals},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Refusals: claimCycleRefusals(),
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/claim", Auth: authAny, Versions: onlyV2,
			Description: "Claim the issue for the calling session or person {force?}, a bearer naming its session in actor: " +
				"records who is working it. The holder's own claim answers the issue and writes nothing, the claim keeping " +
				"the time work started. " + issueClaimText + " 409 ISSUE_CLAIMED names a live holder (or a person, whose " +
				"claim has no session to end); force true takes it anyway, which needs a credential holding the person scope " +
				"(403 OAUTH_SCOPE_REQUIRED otherwise). 409 ISSUE_CLOSED on a closed issue, 404 ISSUE_NOT_FOUND.",
			Handler:  s.claimIssueV2,
			Resource: "claim", Kind: kindSingleton,
			Body:  requestBody{Kind: bodyJSON, Type: claimIssueInput{}},
			Shape: routeShape{Status: http.StatusOK, Response: claimAnswer{}, Refusals: claimRefusals},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Refusals: claimCycleRefusals(
				issueClosedRefusal,
				apicode.Refusal{Condition: apicode.ForceWithoutPersonScope, Code: apicode.OAuthScopeRequired},
			),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/collaborators", Auth: authAny, Versions: onlyV2,
			Description: "Everyone who worked an issue, running or not, one row each: a person (kind user, login) is the " +
				"assignee or anyone who claimed it, posted a comment, message, ask or document version on it or its " +
				"documents, or answered one of its asks, newest first; an agent session (kind session, session_id, title?) " +
				"is such an event's actor that is a session (the Legion daemon's own writer left out), or a session the " +
				"listener's Legion agent registry records for the issue, which adds legion {role, role_token, started_at, " +
				"replaced_by, replaced_at}. The people come first, then the sessions. Each row carries reasons (assignee, " +
				"claimed, posted, answered) and last_at; no running state. limit 1 to 200 (default 50) and offset; {items, " +
				"total, limit, offset, issue_key, legion_registry}, legion_registry unavailable when the listener did not " +
				"answer, the rest still answered. 404 ISSUE_NOT_FOUND.",
			Handler:  s.listIssueCollaboratorsV2,
			Resource: "collaborators", Kind: kindCollection,
			Query:    issueCollaboratorsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: issueCollaboratorsV2{}},
			Refusals: listRefusals(issueMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/coverage", Auth: authAny, Versions: onlyV2,
			Description: "What each requirement of the issue's spec lacks: {issue, spec_artifact_id, has_plan, " +
				"requirements, gaps}. A requirement is a `requirement` block of the spec; requirements lists them in document " +
				"order, each with its reference, its words - a link's text, never its target - and how many `check` and " +
				"`plan_task` blocks of any document of the issue or of any descendant issue, at any status, name it in " +
				"serves. gaps is one line per gap - `<block id> has no check`, and `<block id> has no plan task` only once " +
				"the issue has a plan document - or the single line `the spec marks no requirements` for a spec that marks " +
				"none. Nothing is ever refused for a gap. 404 ISSUE_NOT_FOUND.",
			Handler:  s.getIssueCoverageV2,
			Resource: "coverage", Kind: kindReport,
			Shape:    routeShape{Status: http.StatusOK, Response: model.IssueCoverage{}},
			Refusals: documentRefusals(undeclaredKeyRefusal, issueMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/decisions", Auth: authAny, Versions: onlyV2,
			Description: "The issue's decisions: each `##` section of its decisions.md in document order - a `###` " +
				"section under `## Replaced` for a replaced one - with its heading block, ref and markdown, its text_sha256, " +
				"the ask ids its blocks mention (kind person when it has any, agent otherwise), whether a paragraph of it " +
				"starts `Unsure:`, the version its text last changed in, the keep a person recorded, whether it waits for " +
				"review, and one entry per link from its blocks to a spec block, each stale when the decision changed after " +
				"it first linked that block and the block has not changed since. limit 1 to 200 (default 50) and offset; " +
				"{items, total, limit, offset, document}, document the dispatch:// address of its decisions.md, null with no " +
				"decisions where it has none. include=open_questions adds open_questions beside them, its open asks whose " +
				"block is in its spec, oldest first, each as GET /api/v2/asks answers it. 404 ISSUE_NOT_FOUND.",
			Handler:  s.listIssueDecisionsV2,
			Resource: "decisions", Kind: kindCollection, Key: "{block_id}",
			Query:    issueDecisionsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: issueDecisionsV2{}},
			Refusals: documentRefusals(listRefusals(issueMissingRefusal)...),
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/decisions/{block_id}/state", Auth: authAny, Versions: onlyV2,
			Description: "Record that the caller, a person, agrees with a decision as the text they read it at {state: " +
				"kept, text_sha256} (the person and approve scopes): text_sha256 is the decision's own from GET " +
				"/api/v2/issues/{key}/decisions, and a decision whose text changed since is 409 DECISION_CHANGED with " +
				"nothing written (400 INVALID_KEEP for a body without it). Answers the keep {artifact_id, block_id, " +
				"changed_version, kept_by, kept_at} and appends decision.kept, which wakes nobody; a keep of the same " +
				"text_sha256 by the person who kept it answers the keep as it stands and writes nothing, and anybody " +
				"else's moves the keep to them. 400 STATE_INVALID for any other state; {block_id} is the heading block of " +
				"a decision of the issue's decisions.md outside `## Replaced`, and any other block is 404 BLOCK_NOT_FOUND; " +
				"404 ISSUE_NOT_FOUND.",
			Handler:  s.putDecisionStateV2,
			Resource: "state", Kind: kindState,
			Body:        requestBody{Kind: bodyStateUnion, Arms: map[string]any{"kept": keepDecisionStateInput{}}},
			Shape:       routeShape{Status: http.StatusOK, Response: model.DecisionKeep{}},
			OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
			Scope:       scopeProject, ProjectOf: projectPathIssue,
			IDs: &idRoute{Reason: "a decision's heading block id, which names a block of the issue's decisions document; " +
				"the path names no document to resolve a prefix within"},
			Refusals: documentRefusals(
				undeclaredKeyRefusal, issueMissingRefusal, blockMissingRefusal,
				apicode.Refusal{Condition: apicode.DecisionTextMoved, Code: apicode.DecisionChanged},
				apicode.Refusal{Condition: apicode.KeepWithoutDigest, Code: apicode.InvalidKeep},
			),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/references", Auth: authAny, Versions: onlyV2,
			Description: "An issue's references, one hop in each direction, one list at a time. The issue's own content " +
				"is its documents and their blocks, its asks and their parts, and its comments and messages and their " +
				"blocks; a link is a mention, or a typed pointer while GET /api/v2/references lists it. relation names " +
				"the list: links, every node the issue's own content links to; linked_from, every node that links to the " +
				"issue or to its own content; or members (the default), the rows of links whose node is a document, the " +
				"documents /api/v1 answers as members. Each list has one row for each node, a block or a part counted as " +
				"its document or item; the issue and its own content are in no list, nor is a node that only a " +
				"structural edge joins to the issue, such as its parent or a child. Each row is {node, name, kinds, via, " +
				"artifact}: the node as GET /api/v2/references names it, the words shown for it, the distinct kinds of " +
				"its links in order, the lowest of the issue's nodes at the near end, and on a row of members or a row " +
				"of links whose node is a document, the document's row (its own fields, latest_version, " +
				"latest_version_at, latest_version_size and version_count, and no approval; its versions are GET " +
				"/api/v2/artifacts/{id}/versions), null on every other row. Each list is in the order of the node's " +
				"kind, then its id. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}, total the " +
				"rows of the list. The answer carries an ETag over the page it answers, its relation and its rows " +
				"whole; If-None-Match answers 304. Every statement of the read shares one 2 s budget, and a read that " +
				"reaches it reads nothing and answers 503 REFERENCES_TIMEOUT, to be asked again later. 404 " +
				"ISSUE_NOT_FOUND.",
			Handler:  s.listIssueReferencesV2,
			Resource: "references", Kind: kindCollection,
			Query: issueReferencesV2Query{},
			Shape: routeShape{
				Status: http.StatusOK, Response: page[referenceRowV2]{},
				Also: []routeAnswer{{Status: http.StatusNotModified}},
			},
			Refusals: listRefusals(issueMissingRefusal, apicode.Refusal{Condition: apicode.ReferencesBudgetSpent, Code: apicode.ReferencesTimeout}),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/checks", Auth: authAny, Versions: onlyV2,
			Description: "The checks of the issue's Verification document in document order, each with its attributes " +
				"(when, surface, run, negative_control, ci_check), its text - a link's text, never its target - the version " +
				"its words last changed in and their digest, and results, every result recorded against it from any issue, " +
				"newest first by when the recording transaction began. Each check carries compared_with, the commits and " +
				"branches its results are compared with: for a before_merge check every pull request head Dispatch holds " +
				"for the check's issue, {kind: pull_request, commit, pull_request, learned_at, repo?}: the open and merged " +
				"pull requests attributed to the issue, and those its external links name " +
				"(https://github.com/<owner>/<repo>/pull/<n>) in a repository it does not resolve to, a linked one's head " +
				"carrying that repository as repo (owner/name); for an after_merge check " +
				"{kind: branch, branch} for the branch that issue's pull request merged into and the repository's default " +
				"branch; for a default_branch check the default branch alone. Where that list is empty the check carries " +
				"compared_nothing: no_pull_request (the check's issue holds no pull request), no_repository (the issue " +
				"resolves to no repository), repository_unreadable (Dispatch's GitHub App cannot read the repository) or " +
				"read_failed (a read Dispatch makes did not answer). Each check carries pass_stands {answer, roles}, yes " +
				"with the roles whose passes stand, no, or unknown, and each result commit_stands {answer, basis, reason, " +
				"repo} (at_basis, other_commit, or unknown). A result whose commit the record took to be of a linked " +
				"repository is decided by that repository: a before_merge one by the pull request heads compared with, and " +
				"an after_merge or default_branch one by that repository's branches (for after_merge the branch its merged " +
				"linked pull request merged into, then its default branch). A basis in a linked repository carries it as " +
				"repo, and an other_commit or unknown answer that repository decided names it as repo, so a reader can " +
				"tell it from the issue's own repository, which carries none. Each result also carries check_words_moved, " +
				"and retracted {reason, actor, created_at} where a " +
				"retraction stands against it, which makes it no evidence. A check CI proves carries ci {run, state, case, " +
				"detail, line}, and its one result is derived from that check run on every read. at=<commit> compares every " +
				"check with that one commit, {kind: named, commit}, 40 lowercase hex characters (400 INVALID_QUERY " +
				"otherwise). limit 1 to 200 (default 50) and offset; {items, total, limit, offset, issue, " +
				"verification_artifact_id}; an issue with no Verification document answers no checks. A caller that sent " +
				"Dispatch-Trees: 1 is also given each check's text_tree, its whole content as the tree a reader is shown, " +
				"links and references kept, and each result's observed_tree, the tree its observed text is shown as. " +
				"404 ISSUE_NOT_FOUND.",
			Handler:  s.listIssueChecksV2,
			Resource: "checks", Kind: kindCollection,
			Query:    issueChecksV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: issueChecksV2{}},
			Refusals: documentRefusals(listRefusals(issueMissingRefusal)...),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/results", Auth: authAny, Versions: onlyV2,
			Description: "Record one result of one check {check, role, verdict, commit, surface?, run?, observed?, " +
				"negative_control?, failed?, skipped?, excluded?, screenshots?}, a bearer naming its session in actor. " +
				"{key} is the issue the result is recorded on; check names a check block of its Verification document or " +
				"of one of its ancestors', by block id or, where a block id is not unique in that set, by its canonical " +
				"reference (409 CHECK_AMBIGUOUS); any other is 400 UNKNOWN_CHECK, and a check CI proves 409 " +
				"CHECK_PROVED_BY_CI. role is at most 32 characters of a-z and -, verdict pass or fail, commit 40 lowercase " +
				"hex (400 RESULT_INPUT otherwise), and the counts default to 0; a pass counting any failed, skipped or " +
				"excluded case is 400 RESULT_CONTRADICTS. The commit must be held by the repository the check's issue " +
				"resolves to, or by one in which the issue's external links name an open or merged GitHub pull request " +
				"(https://github.com/<owner>/<repo>/pull/<n>) and that Dispatch's GitHub App is installed on and can read; " +
				"a record reads at most 10 linked pull requests. A commit no such repository holds is 400 " +
				"RESULT_COMMIT_UNKNOWN, naming the commit, each repository asked and each linked pull request Dispatch " +
				"could not read or set aside. A read of a linked pull request or a linked repository that did not answer " +
				"(a rate limit, a 5xx, a transport failure), where no repository is shown to hold the commit, is 502 " +
				"GITHUB_UNREADABLE naming that repository, and records nothing: record the result again, after the " +
				"Retry-After a rate limit carries. An issue that resolves to no repository takes any commit, and one " +
				"whose repository Dispatch cannot read takes it unless such a linked read did not answer. A result at a " +
				"linked repository's commit keeps that repository, which GET /api/v2/issues/{key}/checks compares it in. " +
				"A closed issue takes a result only for an after_merge or default_branch check " +
				"(409 ISSUE_CLOSED). A check reworded by a document version at each of the two attempts this write makes " +
				"is 409 CHECK_CONTENDED with Retry-After: 1, and nothing recorded: read the check and retry. Answers 201 " +
				"{result, line}, line the pull-request body line the result implies, and appends result.recorded on the " +
				"check's issue. 404 ISSUE_NOT_FOUND.",
			Handler:  s.recordIssueResultV2,
			Resource: "results", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: checkResultInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: checkResultRecorded{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Refusals: checkResultRefusals(issueClosedRefusal, apicode.Refusal{Condition: apicode.CheckReworded, Code: apicode.CheckContended}),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/results/dry-run", Auth: authAny, Versions: onlyV2,
			Description: "The dry run of a result's record: the body POST /api/v2/issues/{key}/results takes, refused as " +
				"the record refuses it before it writes (400 RESULT_INPUT, 400 RESULT_CONTRADICTS, 400 UNKNOWN_CHECK, 409 " +
				"CHECK_AMBIGUOUS, 409 CHECK_PROVED_BY_CI, 400 RESULT_COMMIT_UNKNOWN, 502 GITHUB_UNREADABLE), and otherwise " +
				"answered 200 {line}, " +
				"the pull-request body line the record would answer, with nothing recorded and no event appended. A closed " +
				"issue, a screenshot and a check reworded while the record writes are refused by the record alone. 404 " +
				"ISSUE_NOT_FOUND.",
			Handler:  s.dryRunIssueResultV2,
			Resource: "dry-run", Kind: kindComputation,
			Body:  requestBody{Kind: bodyJSON, Type: checkResultInput{}},
			Shape: routeShape{Status: http.StatusOK, Response: checkResultDryRun{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Refusals: checkResultRefusals(),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/subscribers", Auth: authAny, Versions: onlyV2,
			Description: "The sessions subscribed to an issue's topics (the person scope), newest heartbeat first: each " +
				"{session_id, title, live, state?, last_seen, topics, removable, via?}, removable false where only a " +
				"broader topic matches, which via names. limit 1 to 200 (default 50) and offset; {items, total, limit, " +
				"offset}. 503 ENVOY_UNAVAILABLE or ENVOY_REFILLING when the listener cannot answer; 404 ISSUE_NOT_FOUND.",
			Handler:  s.listIssueSubscribersV2,
			Resource: "subscribers", Kind: kindCollection, Key: "{session_id}",
			Query:       issueSubscribersV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[subscriber]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(envoyListenerRefusals(issueMissingRefusal)...),
		},
		{
			Method: http.MethodDelete, Pattern: "/issues/{key}/subscribers/{session_id}", Auth: authAny, Versions: onlyV2,
			Description: "Unsubscribe a session from an issue's topics (the person scope): the topics scoped to this issue " +
				"alone are removed from the session's Envoy interests, and subscription.remove_requested and then " +
				"subscription.removed are appended on the issue. 204. 409 NOT_REMOVABLE where only a broader topic matches, " +
				"404 SUBSCRIBER_NOT_FOUND for a session that is not subscribed, 502 ENVOY_UNSUBSCRIBE_FAILED, 503 " +
				"ENVOY_UNAVAILABLE, 404 ISSUE_NOT_FOUND.",
			Handler:  s.unsubscribeIssueSessionV2,
			Resource: "subscribers", Kind: kindItem, Key: "{session_id}",
			Shape:       routeShape{Status: http.StatusNoContent},
			OAuthScopes: person,
			Scope:       scopeProject, ProjectOf: projectPathIssue, SessionPath: "session_id",
			Refusals: unsubscribeRefusals(undeclaredKeyRefusal, issueMissingRefusal),
		},
		{
			Method: http.MethodPut, Pattern: "/results/{id}/state", Auth: authAny, Versions: onlyV2,
			Description: "Retract one recorded result, with the reason it is wrong {state: retracted, reason}, a bearer " +
				"naming its session in actor. The retraction is a record of its own: the result is neither updated nor " +
				"deleted, and both stay readable. Three callers are admitted and every other is 403 RETRACTION_FORBIDDEN: " +
				"the session that recorded the result, a session holding the Dispatch claim of the issue it was recorded " +
				"on, and a caller whose credential carries a person. A closed issue and a check of any when are retracted " +
				"here. Answers the result with retracted {reason, actor, created_at}, and appends result.retracted on the " +
				"check's issue; the same retraction by the caller who made it answers the result and writes nothing, and " +
				"any other retraction of a retracted result is 409 RESULT_RETRACTED. A blank reason is 400 " +
				"RETRACTION_INPUT, any other state 400 STATE_INVALID; 404 RESULT_NOT_FOUND for an id no result has.",
			Handler:  s.putResultStateV2,
			Resource: "state", Kind: kindState,
			Body:  requestBody{Kind: bodyStateUnion, Arms: map[string]any{"retracted": retractResultStateInput{}}},
			Shape: routeShape{Status: http.StatusOK, Response: model.CheckResult{}},
			Scope: scopeProject, ProjectOf: projectPathResult,
			IDs: &idRoute{Reason: "a check result id"},
			Refusals: []apicode.Refusal{
				undeclaredKeyRefusal, resultMissingRefusal,
				{Condition: apicode.ResultRetractedBefore, Code: apicode.ResultRetracted},
				{Condition: apicode.RetractionByOther, Code: apicode.RetractionForbidden},
				{Condition: apicode.RetractionReasonBlank, Code: apicode.RetractionInput},
			},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/steps", Auth: authAny, Versions: onlyV2,
			Description: "Every run of every step of the issue's workflow, as the Legion daemon wrote them, task by task " +
				"in the order of each task's first run and each task's runs in the order they ran: each " + stepRecordText +
				". A closed issue's read too; an issue no step ran on reads []. limit 1 to 200 (default 50) and offset; " +
				"{items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.listIssueStepsV2,
			Resource: "steps", Kind: kindCollection, Key: "{id}",
			Query:    issueStepsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[model.StepRecord]{}},
			Refusals: listRefusals(issueMissingRefusal),
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Write one run of one step of the issue's workflow {task?, step?, kind?, role?, type?, agent?, " +
				"session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, report?}, a bearer naming " +
				"its session in actor: the Legion daemon writes it at the run's start and at its end under one id, the " +
				"uuid it derives for the run. Each field the record does not hold yet is set and every field it holds is " +
				"left, but returned_to, which only the write that sets the result sets, so a start and an end land in " +
				"either order; a field left out or null states nothing, and so does an empty text, while returned_to 0 " +
				"names place 0, the retro's. The first write of a run names its task, step, kind, role and generation. " +
				"The actor must be session legion-daemon:<PROJECT> of the issue's project, else 403 STEP_WRITER; before " +
				"the airlock a bearer caller names its own session, so any caller whose credential proves no session can " +
				"name that one. A closed issue takes a write too, and no write appends an issue event. Answers 200 the " +
				"record as it stands after the write, " + stepRecordText + "; a body every field of which the record " +
				"holds as sent answers the record and writes nothing. A value outside its form is 400 STEP_INPUT naming " +
				"the field, an id whose record is a run of another issue 409 STEP_OTHER_ISSUE, and a write that names no " +
				"identity to an id no record has 404 STEP_NOT_FOUND; 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.putIssueStepV2,
			Resource: "steps", Kind: kindItem, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: stepRecordInput{}},
			Shape: routeShape{Status: http.StatusOK, Response: model.StepRecord{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			IDs:      &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for one run of one step"},
			Refusals: stepWriteRefusals(),
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}/rating", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Write the rating of a retro's run {rating, rating_reason, task?, step?, kind?, "+
				"role?, type?, agent?, session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, "+
				"report?}, a bearer naming its session in actor: rating, %d to %d, is how well the plan the issue ran "+
				"fit its work in the view of the agent that did the retro, and rating_reason its reason, not blank and "+
				"at most %d characters counted as Unicode code points. The Legion daemon writes it under the id of the "+
				"retro's run, with the fields PUT /api/v2/issues/{key}/steps/{id} takes, which make the record where "+
				"the retro's start and end have not landed yet; each field the record does not hold yet is set and "+
				"every field it holds is left, but returned_to, which only the write that sets the result sets, so the "+
				"rating is written once and lands before or after the retro's end. The actor must be session "+
				"legion-daemon:<PROJECT> of the issue's project, else 403 STEP_WRITER; before the airlock a bearer "+
				"caller names its own session, so any caller whose credential proves no session can name that one. A "+
				"closed issue takes it too, and no write appends an issue event. Answers 200 the record as it stands "+
				"after the write, "+stepRecordText+"; a body every field of which the record holds as sent answers the "+
				"record and writes nothing. A rating or reason outside its form, and a record that is not a retro's, "+
				"are 400 STEP_INPUT naming the field, an id whose record is a run of another issue 409 "+
				"STEP_OTHER_ISSUE, and a write that names no identity to an id no record has 404 STEP_NOT_FOUND; 404 "+
				"ISSUE_NOT_FOUND for an issue that does not exist.",
				contracts.PlanRatingLeast, contracts.PlanRatingMost, contracts.PlanReasonMax),
			Handler:  s.putIssueStepRatingV2,
			Resource: "rating", Kind: kindSingleton,
			Body:  requestBody{Kind: bodyJSON, Type: stepRatingInput{}},
			Shape: routeShape{Status: http.StatusOK, Response: model.StepRecord{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			IDs:      &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for the run of a retro"},
			Refusals: stepWriteRefusals(),
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/plans/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Write the plan one run of the issue ran from {generation, source, artifact_id?, version?, " +
				"tasks: [{id, steps}]}, a bearer naming its session in actor: the Legion daemon writes it under the " +
				"uuid it derives for the run's plan, when the run's work starts and again at each later version of " +
				"the owner's plan that Legion takes. source is owner, with the document artifact_id and the version " +
				"of it Legion took, or default, the default steps of an issue whose owner gave no plan or whose " +
				"experiment's group takes none from its owner, with neither; each task's id is unique in the plan and " +
				"its steps are each a role or role:key, the role one a step record's role takes, since each run of the " +
				"step is a record of that role. source, artifact_id, version and tasks are one fact: a write whose " +
				"version is above the one the plan holds replaces all four, and every other write leaves them, a " +
				"default plan counting as version 0, so a write that arrives late, twice or out of order changes " +
				"nothing; the generation is the first write's. A body every member of which the plan holds as sent " +
				"answers the plan and writes nothing. The write makes the plan whatever id it names, so no plan is " +
				"missing to it. The actor " +
				"must be session legion-daemon:<PROJECT> of the issue's project, else 403 STEP_WRITER; before the " +
				"airlock a bearer caller names its own session, so any caller whose credential proves no session can " +
				"name that one. A closed issue takes it too, and no write appends an issue event. Answers 200 the plan " +
				"as it stands after the write {id, issue, generation, source, artifact_id, version, tasks}, artifact_id " +
				"and version null for a default plan. A value outside its form is 400 PLAN_INPUT naming the field, a " +
				"step whose role a step record refuses naming the step and its role, and an id whose plan is another " +
				"issue's 409 PLAN_OTHER_ISSUE; 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.putIssuePlanV2,
			Resource: "plans", Kind: kindItem, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: planRecordInput{}},
			Shape: routeShape{Status: http.StatusOK, Response: model.PlanRecord{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			IDs: &idRoute{Reason: "a plan id: the uuid the Legion daemon derives for the plan of one run of an issue"},
			Refusals: []apicode.Refusal{
				undeclaredKeyRefusal, issueMissingRefusal, stepWriterRefusal,
				{Condition: apicode.PlanInputInvalid, Code: apicode.PlanInput},
				{Condition: apicode.PlanOfAnotherIssue, Code: apicode.PlanOtherIssue},
			},
		},
	}
}

// issueFiltersV2 are the filters both lists of issues take, GET /api/v2/issues and GET
// /api/v2/projects/{key}/issues: GET /api/v1/issues's, with each time window `<field>_from` and
// `<field>_to` (R1.5; the route map's D6).
type issueFiltersV2 struct {
	pageQuery
	Status   []string `query:"status"`
	Parent   string   `query:"parent"`
	Label    []string `query:"label"`
	Priority []string `query:"priority"`
	Advances []string `query:"advances"`
	// RouteStatus keeps the open issues whose route stands so.
	RouteStatus string `query:"route_status" enum:"live,holder_unresponsive,no_holder,unknown"`
	Open        bool   `query:"open"`
	Moved       bool   `query:"moved"`
	// Pinned keeps the issues the caller pinned, which needs the person scope.
	Pinned bool `query:"pinned"`
	// UpdatedFrom and UpdatedTo window updated_at, ClosedFrom and ClosedTo closed_at, each from
	// inclusive and to exclusive.
	UpdatedFrom *time.Time `query:"updated_from"`
	UpdatedTo   *time.Time `query:"updated_to"`
	ClosedFrom  *time.Time `query:"closed_from"`
	ClosedTo    *time.Time `query:"closed_to"`
}

// issueListV2Query is GET /api/v2/issues's query.
type issueListV2Query struct {
	issueFiltersV2
	// Project keeps the issues of these projects; none is every project.
	Project []string `query:"project"`
	// Ref keeps the issues an issue reference names (issueRefKeys), which GET
	// /api/v1/issues/resolve answered the one key of.
	Ref *string `query:"ref"`
}

// projectIssuesV2Query is GET /api/v2/projects/{key}/issues's query.
type projectIssuesV2Query struct {
	issueFiltersV2
}

// issueReadV2Query is GET /api/v2/issues/{key}'s query.
type issueReadV2Query struct {
	Include []string `query:"include,comma" enum:"blocks,comment_count,open_asks,children,progress,design_gates,reach,referenced_by_count,legion"`
}

// issueReadV2 is GET /api/v2/issues/{key}'s answer (the route map's J3): the issue's own fields
// and its artifacts as every /api/v2 read of artifacts answers them, which is what GET
// /api/v1/issues/{key}/summary answered and more, and each member include= names, which together
// are what GET /api/v1/issues/{key} answered. It is a type of its own, so no /api/v1 shape's tags
// change (dispatch://INFRA-4071/spec R3).
type issueReadV2 struct {
	model.Issue
	// Blocks is the keys of the issues whose blocked_by names this one, sorted, [] when none, with
	// include=blocks: the issue_links rows blocked_by reads, read by their target
	// (issueBlocksStatement). It comes before the lists below, so a reader that keeps only the start
	// of a long read still has it.
	Blocks *[]string `json:"blocks,omitempty"`
	// CommentCount is how many comments GET /api/v2/issues/{key}/comments lists with no artifact=,
	// with include=comment_count (issueCommentCountStatement). It comes before the lists below, as
	// Blocks does.
	CommentCount *int         `json:"comment_count,omitempty"`
	Artifacts    []artifactV2 `json:"artifacts"`
	// Progress is the spec's task-list items and the direct children, with include=progress.
	Progress *model.IssueProgress `json:"progress,omitempty"`
	// OpenAsks are the issue's open asks, oldest first, with include=open_asks.
	OpenAsks *[]askV2 `json:"open_asks,omitempty"`
	// Children are the issue's direct children, with include=children.
	Children *[]model.IssueChild `json:"children,omitempty"`
	// DesignGates are the documents its design-gate steps record, with include=design_gates.
	DesignGates *[]issueDesignGate `json:"design_gates,omitempty"`
	// Reach is where its route reaches and the state of its claim's session, with include=reach.
	Reach *issueReachV2 `json:"reach,omitempty"`
	// ReferencedByCount is how many references to the issue stand, with include=referenced_by_count.
	ReferencedByCount *int `json:"referenced_by_count,omitempty"`
	// Legion is the issue's Legion state, with include=legion: the body GET
	// /api/v1/issues/{key}/legion answers and a legion_issue live view carries (readLegionIssue),
	// so its runs_revision is the one that view moves on.
	Legion *legionIssueResponse `json:"legion,omitempty"`
}

// issueReachV2 is what the Envoy listener says of an issue (include=reach): where its route reaches
// now, and what state the session holding its claim is in, each member null where there is none
// or the listener did not answer.
type issueReachV2 struct {
	model.IssueRouteReach
	model.IssueClaimLiveness
}

// patchIssueV2Input is PATCH /api/v2/issues/{key}'s body: updateIssueInput's members, and the
// issue's design gates, which PUT /api/v1/issues/{key}/design-gates wrote (the route map's J16).
type patchIssueV2Input struct {
	updateIssueInput
	// DesignGates is the whole set of the documents the issue's design-gate steps record, [] for
	// none; absent leaves the set as it stands. Each entry is the one PUT
	// /api/v1/issues/{key}/design-gates takes, holds left out or null stored as null.
	DesignGates *[]issueDesignGateInput `json:"design_gates"`
	// The presence of any status option selects the multi-issue status-change form.
	IncludeChildren *bool `json:"include_children,omitempty"`
	UnlinkChildren  *bool `json:"unlink_children,omitempty"`
	IncludeParent   *bool `json:"include_parent,omitempty"`
	DryRun          *bool `json:"dry_run,omitempty"`
}

func (body patchIssueV2Input) isStatusChange() bool {
	return body.IncludeChildren != nil || body.UnlinkChildren != nil || body.IncludeParent != nil || body.DryRun != nil
}

func (s *server) patchIssueKeyedRoute(r *http.Request) (keyedRoute, bool) {
	if !v2Body[patchIssueV2Input](r).isStatusChange() {
		return keyedRoute{}, false
	}
	return s.statusChangeKeyedRoute(), true
}

// statusChangeInput rejects other PATCH fields before any step runs; they would otherwise
// disappear from a write whose plan governs status and parent moves alone.
func (body patchIssueV2Input) statusChangeInput() (statusChangeInput, error) {
	fields := body.updateIssueInput
	if body.DesignGates != nil || fields.Title != nil || fields.Rank != nil || fields.Labels != nil ||
		fields.LabelsAdd != nil || fields.Priority != nil || fields.Route != nil ||
		fields.ExternalLinks != nil || fields.Assignee != nil || fields.Parent != nil ||
		fields.BlockedBy != nil || fields.Advances != nil || fields.AdvancesRevision != nil ||
		fields.Components != nil || fields.Repo != nil || fields.RefuseOpenForeignChildren {
		return statusChangeInput{}, errorf(apicode.StatusChangeInput, "send status-change options with status, expected_sequence and actor only; send other fields separately")
	}
	input := statusChangeInput{ExpectedSeq: fields.ExpectedSeq, Actor: fields.Actor}
	if fields.Status != nil {
		input.Status = *fields.Status
	}
	input.IncludeChildren = body.IncludeChildren != nil && *body.IncludeChildren
	input.UnlinkChildren = body.UnlinkChildren != nil && *body.UnlinkChildren
	input.IncludeParent = body.IncludeParent != nil && *body.IncludeParent
	input.DryRun = body.DryRun != nil && *body.DryRun
	return input, nil
}

// issueCollaboratorsV2Query is GET /api/v2/issues/{key}/collaborators's query: one page.
type issueCollaboratorsV2Query struct {
	pageQuery
}

// collaboratorV2 is one row of GET /api/v2/issues/{key}/collaborators: a person (kind user) or an
// agent session (kind session), each with the members of its kind, as GET /api/v1's people and
// agents list them apart.
type collaboratorV2 struct {
	Kind string `json:"kind" enum:"user,session"`
	// Login is a person's canonical login.
	Login string `json:"login,omitempty"`
	// SessionID is a session's id, and Title the title the registry or its own writes give it.
	SessionID string   `json:"session_id,omitempty"`
	Title     *string  `json:"title,omitempty"`
	Reasons   []string `json:"reasons" enum:"assignee,claimed,posted,answered"`
	// LastAt is the newest event that made them a collaborator; null for an assignee who did
	// nothing else on the issue, and for a registry entry with no such event.
	LastAt *string `json:"last_at"`
	// Legion is a session's Legion agent registry entry for the issue.
	Legion *collaboratorLegion `json:"legion,omitempty"`
}

// issueCollaboratorsV2 is GET /api/v2/issues/{key}/collaborators's answer: one envelope of people
// and sessions, the issue and the state of the listener's Legion agent registry beside it.
type issueCollaboratorsV2 struct {
	page[collaboratorV2]
	IssueKey       string `json:"issue_key"`
	LegionRegistry string `json:"legion_registry" enum:"ok,unavailable"`
}

// issueDecisionsV2Query is GET /api/v2/issues/{key}/decisions's query.
type issueDecisionsV2Query struct {
	pageQuery
	Include []string `query:"include,comma" enum:"open_questions"`
}

// issueDecisionsV2 is GET /api/v2/issues/{key}/decisions's answer: the envelope of decisions, the
// decisions document's address beside it, and the spec's open questions with
// include=open_questions.
type issueDecisionsV2 struct {
	page[model.Decision]
	Document      *string  `json:"document"`
	OpenQuestions *[]askV2 `json:"open_questions,omitempty"`
}

// keepDecisionStateInput is the kept arm of PUT /api/v2/issues/{key}/decisions/{block_id}/state:
// the decision's text_sha256 as the caller read it.
type keepDecisionStateInput struct {
	State      string `json:"state"`
	TextSHA256 string `json:"text_sha256" required:"true"`
}

// issueReferencesV2Query is GET /api/v2/issues/{key}/references's query: which list of the
// issue's references read the page is of, and the page. It takes no versions= as /api/v1's does,
// since a row carries no versions.
type issueReferencesV2Query struct {
	pageQuery
	// Relation is the list the page is of: members, the rows of links whose node is a document,
	// links or linked_from.
	Relation string `query:"relation" enum:"members,links,linked_from" default:"members"`
}

// referenceRowV2 is one row of an issue's references read as GET /api/v2/issues/{key}/references
// answers it: the row /api/v1 answers in links or linked_from (model.ReferenceLink), with
// Artifact, the document's row (artifactV2, in place of /api/v1's member with its versions,
// dispatch://INFRA-4071/spec R3), on a row of members and on a row of links whose node is a
// document, and null on every other row.
type referenceRowV2 struct {
	model.ReferenceLink
	Artifact *artifactV2 `json:"artifact"`
}

// referenceRowsV2Statement reads the documents whose id is in $1 as rows, each with its newest
// version's number, time and size and the count of its versions, in one statement that answers no
// version but the newest: each document's newest version and its count are read over the
// (artifact_id, number) index of artifact_versions. A document with no version is absent, as it is
// from GET /api/v1/issues/{key}/references (loadArtifactsByRefKey).
const referenceRowsV2Statement = `
	select ` + artifactColumns + `, latest.number, latest.written_at, latest.size, counted.versions
	from artifacts
	join lateral (
		select v.number, v.created_at as written_at, ` + model.VersionSize + ` as size
		from artifact_versions v where v.artifact_id = artifacts.id order by v.number desc limit 1
	) latest on true
	cross join lateral (select count(*) as versions from artifact_versions v where v.artifact_id = artifacts.id) counted
	where id = any($1::uuid[])`

// loadReferenceRowsV2 reads the documents ids name as referenceRowsV2Statement does, keyed by id.
func loadReferenceRowsV2(ctx context.Context, q queryer, ids []string) (map[string]artifactV2, error) {
	documents := make(map[string]artifactV2, len(ids))
	if len(ids) == 0 {
		return documents, nil
	}
	rows, err := q.Query(ctx, referenceRowsV2Statement, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var number, count int
		var writtenAt time.Time
		var size *int
		artifact, err := scanArtifact(rows, &number, &writtenAt, &size, &count)
		if err != nil {
			return nil, err
		}
		row := newArtifactV2(artifact)
		row.LatestVersion, row.LatestVersionAt, row.LatestVersionSize, row.VersionCount = &number, &writtenAt, size, count
		documents[artifact.ID] = row
	}
	return documents, rows.Err()
}

// issueReferenceRowsV2 is the list relation names of the references read of the issue key names,
// as GET /api/v2/issues/{key}/references pages it: the read one hop in each direction GET
// /api/v1/issues/{key}/references makes (issueReferenceMembers), under the same bound
// (issueClosure), its links or its linked_from, or members, the rows of links whose node is a
// document, which are the documents /api/v1 answers as members. Each list is in the order of the
// node's kind, then its id. The route has answered a key no issue carries 404 before this runs
// (requireIssueV2).
func (s *server) issueReferenceRowsV2(ctx context.Context, who model.Actor, key, relation string) ([]model.ReferenceLink, error) {
	closure, err := s.issueClosure(ctx, who, key)
	if err != nil {
		return nil, err
	}
	switch relation {
	case "links":
		return closure.Links, nil
	case "linked_from":
		return closure.LinkedFrom, nil
	}
	members := make([]model.ReferenceLink, 0, len(closure.Members))
	for _, link := range closure.Links {
		if link.Node.Kind == "artifact" {
			members = append(members, link)
		}
	}
	return members, nil
}

// referenceRowsV2 is the page's rows of the list relation names, each a document of links
// carrying its row (loadReferenceRowsV2): every row of members, and a row of links whose node is
// a document. A document of links the read named and whose row is gone since is the server's
// fault: /api/v2 answers it 500, as /api/v1 does.
func referenceRowsV2(ctx context.Context, q queryer, relation string, links []model.ReferenceLink) ([]referenceRowV2, error) {
	rows := make([]referenceRowV2, len(links))
	var documentRows []int
	var ids []string
	for index, link := range links {
		rows[index].ReferenceLink = link
		if relation != "linked_from" && link.Node.Kind == "artifact" {
			documentRows = append(documentRows, index)
			ids = append(ids, link.Node.ID)
		}
	}
	documents, err := loadReferenceRowsV2(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for _, index := range documentRows {
		document, ok := documents[rows[index].Node.ID]
		if !ok {
			return nil, fmt.Errorf("reference target %q disappeared while loading closure", rows[index].Node.ID)
		}
		rows[index].Artifact = &document
	}
	return rows, nil
}

// issueChecksV2Query is GET /api/v2/issues/{key}/checks's query.
type issueChecksV2Query struct {
	pageQuery
	// At compares every check with this one commit.
	At string `query:"at"`
}

// checkV2 is one check as GET /api/v2/issues/{key}/checks answers it: model.CheckBlock, its
// compared_nothing naming which of the two kinds of nothing stopped a check that compared with
// nothing because there was nothing to compare with (R10 of dispatch://INFRA-4071/spec), where
// /api/v1 names one reason for both. It is a type of its own, so no /api/v1 shape's tags change
// (R3).
type checkV2 struct {
	model.CheckBlock
	ComparedNothing string `json:"compared_nothing,omitempty" enum:"no_pull_request,no_repository,repository_unreadable,read_failed"`
}

// issueChecksV2 is GET /api/v2/issues/{key}/checks's answer: the envelope of checks, the issue and
// its Verification document beside it.
type issueChecksV2 struct {
	page[checkV2]
	Issue                  string `json:"issue"`
	VerificationArtifactID string `json:"verification_artifact_id"`
}

// issueSubscribersV2Query is GET /api/v2/issues/{key}/subscribers's query: one page.
type issueSubscribersV2Query struct {
	pageQuery
}

// issueStepsV2Query is GET /api/v2/issues/{key}/steps's query: one page.
type issueStepsV2Query struct {
	pageQuery
}

// retractResultStateInput is the retracted arm of PUT /api/v2/results/{id}/state: the reason the
// result is wrong.
type retractResultStateInput struct {
	State  string       `json:"state"`
	Reason string       `json:"reason" required:"true"`
	Actor  *model.Actor `json:"actor"`
}

// listIssuesV2 answers GET /api/v2/issues: the listing GET /api/v1/issues reads (readIssueListing),
// across the projects named, or every project, and the issues an issue reference names where ref=
// gives one.
func (s *server) listIssuesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[issueListV2Query](r)
	projects := make([]string, 0, len(query.Project))
	for _, project := range query.Project {
		projects = append(projects, strings.TrimSpace(project))
	}
	var keys []string
	if query.Ref != nil {
		named, err := s.issueRefKeys(r.Context(), *query.Ref)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		keys = named
	}
	s.answerIssueListV2(w, r, projects, keys, query.issueFiltersV2)
}

// listProjectIssuesV2 answers GET /api/v2/projects/{key}/issues: the rows GET
// /api/v2/issues?project={key} answers for the same filters, the project held to exist first.
func (s *server) listProjectIssuesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.answerIssueListV2(w, r, []string{key}, nil, v2Query[projectIssuesV2Query](r).issueFiltersV2)
}

// answerIssueListV2 answers one page of the issues of projects - every project where it names
// none - that filters keep, and of keys alone where keys is not nil (ref=), through the statement
// /api/v1 runs with the filters of /api/v2 (issueListFiltersV2). The caller has already been held
// to a credential that resolves.
func (s *server) answerIssueListV2(w http.ResponseWriter, r *http.Request, projects, keys []string, filters issueFiltersV2) {
	login := ""
	if filters.Pinned {
		actor, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		login = actor.ID
	}
	labels, err := normalizeLabels(filters.Label, labelFilterLimit)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	for index, label := range labels {
		labels[index] = strings.ToLower(label)
	}
	// Each window is judged as the caller wrote it, before rounding, as GET /api/v1/issues judges
	// its closed window (store.MicrosecondCeiling).
	if filters.UpdatedFrom != nil && filters.UpdatedTo != nil && !filters.UpdatedFrom.Before(*filters.UpdatedTo) {
		writeError(w, r, apicode.InvalidQuery, "updated_from must be before updated_to")
		return
	}
	if filters.ClosedFrom != nil && filters.ClosedTo != nil && !filters.ClosedFrom.Before(*filters.ClosedTo) {
		writeError(w, r, apicode.InvalidClosedWindow, "closed_from must be before closed_to")
		return
	}
	priorities, unsetPriority, err := parsePriorityFilter(filters.Priority)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	advances, noAdvances, anyAdvances, err := parseAdvancesFilter(filters.Advances)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// A route matters only while there is work to reach, so route_status reads open issues
	// alone, as GET /api/v1/issues reads them.
	routeStatus := filters.RouteStatus
	open := filters.Open || routeStatus != ""
	arguments := []any{projects, parseStatusFilter(filters.Status), strings.TrimSpace(filters.Parent),
		store.MicrosecondCeiling(filters.UpdatedFrom), labels, open, priorities, unsetPriority,
		store.MicrosecondCeiling(filters.ClosedFrom), store.MicrosecondCeiling(filters.ClosedTo), advances, noAdvances, anyAdvances,
		filters.Moved, store.MicrosecondCeiling(filters.UpdatedTo), keys}
	if filters.Pinned {
		arguments = append(arguments, login)
	}
	// SQL cuts the page only when no route_status filter runs after the read, as on /api/v1.
	inSQL := routeStatus == ""
	listQuery := listIssuesV2Query
	switch {
	case filters.Pinned && inSQL:
		listQuery = listPinnedIssuesV2PageQuery
	case filters.Pinned:
		listQuery = listPinnedIssuesV2Query
	case inSQL:
		listQuery = listIssuesV2PageQuery
	}
	if inSQL {
		arguments = append(arguments, filters.Limit, filters.Offset)
	}
	issues, total, ok := s.readIssueListing(w, r, listQuery, arguments, inSQL, routeStatus)
	if !ok {
		return
	}
	if inSQL {
		WriteJSON(w, http.StatusOK, newPage(issues, total, filters.pageQuery))
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(issues, filters.pageQuery))
}

// createIssueV2 answers POST /api/v2/issues: the create POST /api/v1/issues makes
// (createIssueFrom).
func (s *server) createIssueV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.createIssueFrom(w, r, v2Body[createIssueInput](r))
}

// getIssueV2 answers GET /api/v2/issues/{key}: the issue's row and artifacts, read as GET
// /api/v1/issues/{key} reads them (readIssueParts), and each member include= names, read only
// where it names it.
func (s *server) getIssueV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	include := v2Query[issueReadV2Query](r).Include
	wants := func(member string) bool { return slices.Contains(include, member) }
	parts, err := s.readIssueParts(r.Context(), s.requestReader(r), key, issueReadParts{
		blocks: wants("blocks"), children: wants("children"), commentCount: wants("comment_count"),
		designGates: wants("design_gates"), referencedByCount: wants("referenced_by_count"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeHandlerError(w, r, errorf(apicode.IssueNotFound, "no issue %s", key))
		return
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	artifacts, err := s.artifactRowsV2(r.Context(), r, parts.artifacts, artifactV2Include{})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	answer := issueReadV2{Issue: parts.issue, Artifacts: artifacts}
	if wants("blocks") {
		answer.Blocks = &parts.blocks
	}
	if wants("comment_count") {
		answer.CommentCount = &parts.commentCount
	}
	if wants("progress") {
		answer.Progress = &parts.progress
	}
	if wants("children") {
		answer.Children = &parts.children
	}
	if wants("design_gates") {
		answer.DesignGates = &parts.designGates
	}
	if wants("referenced_by_count") {
		answer.ReferencedByCount = &parts.referencedByCount
	}
	if wants("open_asks") {
		asks, err := s.openIssueAsksV2(r.Context(), r, key, "")
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer.OpenAsks = &asks
	}
	if wants("reach") {
		// One roster read serves both, as GET /api/v1/issues/{key} reads it, and an issue with
		// neither a route nor a session's claim costs no listener call.
		reach := issueReachV2{}
		if parts.issue.Route != nil || issueClaimSession(parts.issue.Claim) != "" {
			roster := s.routeRosterOrUnknown(r.Context())
			reach.IssueRouteReach = roster.reach(parts.issue.Route)
			reach.IssueClaimLiveness = roster.claimLiveness(parts.issue.Claim, time.Now())
		}
		answer.Reach = &reach
	}
	if wants("legion") {
		state, err := s.readLegionIssue(r.Context(), key)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer.Legion = &state
	}
	WriteJSON(w, http.StatusOK, answer)
}

// openIssueAsksV2 is every open ask of the issue key, oldest first, each as every /api/v2 read of
// asks answers it (askV2), those whose block is in the document inDocument alone where it is not
// "".
func (s *server) openIssueAsksV2(ctx context.Context, r *http.Request, key, inDocument string) ([]askV2, error) {
	read := askV2Read{order: "a.created_at, a.id"}
	read.where("a.issue_key = " + read.argument(key))
	read.where(askStateCondition("open", false))
	if inDocument != "" {
		read.where("a.block_artifact_id = " + read.argument(inDocument) + "::uuid")
	}
	rows, err := s.askV2Rows(ctx, s.deps.Store.Pool, read, nil)
	if err != nil {
		return nil, err
	}
	if err := s.completeAsksV2(ctx, r, rows, false, false); err != nil {
		return nil, err
	}
	return rows, nil
}

// patchIssueV2 answers PATCH /api/v2/issues/{key}: the write PATCH /api/v1/issues/{key} makes
// (patchIssueAs), with the design gates PUT /api/v1/issues/{key}/design-gates wrote stored in the
// same transaction where the body names them.
func (s *server) patchIssueV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[patchIssueV2Input](r)
	if body.isStatusChange() {
		input, err := body.statusChangeInput()
		if err != nil {
			s.writeStatusChangeRefusal(w, r, err, nil)
			return
		}
		s.changeIssueStatusFrom(w, r, input)
		return
	}
	var gates []issueDesignGate
	if body.DesignGates != nil {
		valid, err := validDesignGates(*body.DesignGates)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		gates = valid
	}
	s.patchIssueAs(w, r, body.updateIssueInput, gates)
}

// releaseIssueClaimV2 answers DELETE /api/v2/issues/{key}/claim: the release DELETE
// /api/v1/issues/{key}/claim makes (releaseIssueClaimAs), a bearer naming its session in actor.
func (s *server) releaseIssueClaimV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	if !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	if !human {
		if actor, err = bearerSessionActor(r, actor, v2Body[actorInput](r).Actor); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	s.releaseIssueClaimAs(w, r, actor)
}

// claimIssueV2 answers PUT /api/v2/issues/{key}/claim: the claim POST /api/v1/issues/{key}/claim
// makes (claimIssueAs), which keeps the holder's own claim as it stands.
func (s *server) claimIssueV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	s.claimIssueAs(w, r, v2Body[claimIssueInput](r))
}

// listIssueCollaboratorsV2 answers GET /api/v2/issues/{key}/collaborators: one page of the people
// and the sessions GET /api/v1/issues/{key}/collaborators reads (issueCollaboratorsOf), the people
// first.
func (s *server) listIssueCollaboratorsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	collaborators, err := s.issueCollaboratorsOf(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	rows := make([]collaboratorV2, 0, len(collaborators.People)+len(collaborators.Agents))
	for _, person := range collaborators.People {
		rows = append(rows, collaboratorV2{Kind: person.Kind, Login: person.Login, Reasons: person.Reasons, LastAt: person.LastAt})
	}
	for _, agent := range collaborators.Agents {
		rows = append(rows, collaboratorV2{
			Kind: agent.Kind, SessionID: agent.SessionID, Title: agent.Title, Reasons: agent.Reasons, LastAt: agent.LastAt,
			Legion: agent.Legion,
		})
	}
	WriteJSON(w, http.StatusOK, issueCollaboratorsV2{
		page:           pageOf(rows, v2Query[issueCollaboratorsV2Query](r).pageQuery),
		IssueKey:       key,
		LegionRegistry: collaborators.LegionRegistry,
	})
}

// getIssueCoverageV2 answers GET /api/v2/issues/{key}/coverage: the report GET
// /api/v1/issues/{key}/coverage answers (issueCoverage), each requirement's text_tree with
// Dispatch-Trees: 1 as there.
func (s *server) getIssueCoverageV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	coverage, err := s.issueCoverage(r.Context(), s.deps.Store.Pool, s.requestReader(r), key, trees)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	trees.resolve()
	WriteJSON(w, http.StatusOK, coverage)
}

// listIssueDecisionsV2 answers GET /api/v2/issues/{key}/decisions: one page of the decisions GET
// /api/v1/issues/{key}/decisions reads (issueDecisionsOf), and with include=open_questions the
// spec's open asks as ask rows.
func (s *server) listIssueDecisionsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	query := v2Query[issueDecisionsV2Query](r)
	spec, specFound, err := s.issueSpecDocument(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	document, decisions, err := s.issueDecisionsOf(r.Context(), s.requestReader(r), key, spec, specFound, reader)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reader.resolve()
	answer := issueDecisionsV2{page: pageOf(decisions, query.pageQuery), Document: document}
	if slices.Contains(query.Include, "open_questions") {
		questions := []askV2{}
		if specFound {
			if questions, err = s.openIssueAsksV2(r.Context(), r, key, spec.ID); err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
		}
		answer.OpenQuestions = &questions
	}
	WriteJSON(w, http.StatusOK, answer)
}

// putDecisionStateV2 answers PUT /api/v2/issues/{key}/decisions/{block_id}/state: the keep POST
// /api/v1/artifacts/{id}/blocks/{block_id}/keep records (writeDecisionKeep), of the decision the
// block opens in the issue's decisions document, once the no-op rule of dispatch://INFRA-4071/spec
// R8 has found the body is a change: a keep of the same text_sha256 by the person who kept it
// answers the keep as it stands and writes nothing (putChangesNothing).
func (s *server) putDecisionStateV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	key, blockID := r.PathValue("key"), r.PathValue("block_id")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	body := v2Body[keepDecisionStateInput](r)
	if body.TextSHA256 == "" {
		writeError(w, r, apicode.InvalidKeep, "text_sha256 is the text_sha256 the decision was read at")
		return
	}
	document, found, err := s.issueDecisionsDocument(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !found {
		writeError(w, r, apicode.BlockNotFound, fmt.Sprintf("issue %s has no decisions document holding a decision %q", key, blockID))
		return
	}
	decision, err := s.keptDecision(r.Context(), actor, document, blockID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if decision.TextSHA256 != body.TextSHA256 {
		writeError(w, r, apicode.DecisionChanged,
			"the decision changed since it was read; read it again and keep what it says now")
		return
	}
	if kept := decision.Kept; kept != nil {
		present := body
		present.TextSHA256 = kept.TextSHA256
		keeper := setBy(actorIdentity(model.Actor{Kind: "user", ID: kept.KeptBy}))
		if putChangesNothing(body, present, keeper, actorIdentity(actor)) {
			WriteJSON(w, http.StatusOK, *kept)
			return
		}
	}
	keep, err := s.writeDecisionKeep(r.Context(), actor, document, blockID, decision)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, keep)
}

// issueReferencesV2ETag is the validator of one page of an issue's /api/v2 references: the page
// whole, as its JSON encoding, under lines naming the version and the relation, so no validator
// one version or one list gave matches another's answer. A row carries its node's name, last change
// and parent, and a document's newest version, and another node's new link to the issue adds a
// row: none of that moves the issue's own last_seq, so the page is hashed whole.
func issueReferencesV2ETag(relation string, answer page[referenceRowV2]) (string, error) {
	encoded, err := json.Marshal(answer)
	if err != nil {
		return "", fmt.Errorf("hash the %s page of an issue's references: %w", relation, err)
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\nrelation=%s\n", relation)
	_, _ = hash.Write(encoded)
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`, nil
}

// listIssueReferencesV2 answers GET /api/v2/issues/{key}/references: one page of the list the
// relation names of the read GET /api/v1/issues/{key}/references makes (issueReferenceRowsV2),
// each document of links carrying its row (referenceRowsV2), under a validator of its own.
func (s *server) listIssueReferencesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	query := v2Query[issueReferencesV2Query](r)
	links, err := s.issueReferenceRowsV2(r.Context(), s.requestReader(r), key, query.Relation)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	listed := pageOf(links, query.pageQuery)
	rows, err := referenceRowsV2(r.Context(), s.deps.Store.Pool, query.Relation, listed.Items)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	answer := page[referenceRowV2]{Items: rows, Total: listed.Total, Limit: listed.Limit, Offset: listed.Offset}
	etag, err := issueReferencesV2ETag(query.Relation, answer)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if answerNotModified(w, r, etag) {
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// listIssueChecksV2 answers GET /api/v2/issues/{key}/checks: one page of the checks GET
// /api/v1/issues/{key}/results reads (readIssueChecks), each check that compared with nothing for
// want of anything to compare with naming which of the two cases stopped it
// (comparedNothingV2), and no result answering /api/v1's compatibility currency.
func (s *server) listIssueChecksV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	query := v2Query[issueChecksV2Query](r)
	if query.At != noCommitNamed && !resultCommitSHA.MatchString(query.At) {
		writeError(w, r, apicode.InvalidQuery, "at must be a commit of 40 lowercase hex characters")
		return
	}
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	read, reader, err := s.readIssueChecks(r.Context(), key, query.At, trees)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// Each check's tree is filled in the row the read registered it in, so the rows are copied
	// into the page's shape only once it is.
	trees.resolve()
	checks := make([]checkV2, len(read.Checks))
	for index, check := range read.Checks {
		checks[index] = checkV2{CheckBlock: check, ComparedNothing: comparedNothingV2(r.Context(), reader, check)}
	}
	WriteJSON(w, http.StatusOK, issueChecksV2{
		page: pageOf(checks, query.pageQuery), Issue: read.Issue, VerificationArtifactID: read.VerificationArtifactID,
	})
}

// comparedNothingV2 is the reason /api/v2 names for a check that compared with nothing: the one
// the read named, but for model.ReasonNoPullRequestOrBranch, which /api/v2 parts into
// reasonNoRepository, the check's issue resolving to no repository, and reasonNoPullRequest, an
// issue that resolves to one and holds no pull request. Which it is the reader that compared the
// check already holds (comparisonReader.repositoryOf, read once per issue), so the parting reads
// nothing more.
func comparedNothingV2(ctx context.Context, reader *comparisonReader, check model.CheckBlock) string {
	if check.ComparedNothing != model.ReasonNoPullRequestOrBranch {
		return check.ComparedNothing
	}
	if repo, answered := reader.repositoryOf(ctx, check.IssueKey); answered && repo == nil {
		return reasonNoRepository
	}
	return reasonNoPullRequest
}

// recordIssueResultV2 answers POST /api/v2/issues/{key}/results: the record POST
// /api/v1/issues/{key}/results makes (recordCheckResultFrom).
func (s *server) recordIssueResultV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	s.recordCheckResultFrom(w, r, v2Body[checkResultInput](r))
}

// dryRunIssueResultV2 answers POST /api/v2/issues/{key}/results/dry-run: the dry run POST
// /api/v1/issues/{key}/results/dry-run makes (dryRunCheckResultFrom).
func (s *server) dryRunIssueResultV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	s.dryRunCheckResultFrom(w, r, v2Body[checkResultInput](r))
}

// listIssueSubscribersV2 answers GET /api/v2/issues/{key}/subscribers: one page of the sessions GET
// /api/v1/issues/{key}/subscribers lists (subscribersOf), the issue held to exist first.
func (s *server) listIssueSubscribersV2(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	subscribers, ok := s.subscribersOf(w, r, issueSubscriberBase(key))
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(subscribers, v2Query[issueSubscribersV2Query](r).pageQuery))
}

// unsubscribeIssueSessionV2 answers DELETE /api/v2/issues/{key}/subscribers/{session_id}: the
// unsubscribe DELETE /api/v1/issues/{key}/subscribers/{session_id} makes (unsubscribeSession), the
// issue held to exist first; the {session_id} keeps the answer /api/v1 gives it
// (dispatch://INFRA-4071/spec R7).
func (s *server) unsubscribeIssueSessionV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	s.unsubscribeSession(w, r, actor, issueOwner(key), issueSubscriberBase(key))
}

// requireResultV2 answers whether the check result id names exists, and where it does not writes
// 404 RESULT_NOT_FOUND (requireHeldV2).
func (s *server) requireResultV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "check result", `select exists (select 1 from check_results where id = $1)`,
		apicode.ResultNotFound, "no check result has the id "+id)
}

// putResultStateV2 answers PUT /api/v2/results/{id}/state: the retraction POST
// /api/v1/results/{id}/retract records (retractCheckResultAs), answered as the result with its
// retraction rather than the retraction alone (the route map's row 173). A retraction already
// standing by the caller, with the same reason, answers the result and writes nothing
// (dispatch://INFRA-4071/spec R8, putChangesNothing); every other retraction of a retracted result
// stays 409 RESULT_RETRACTED.
func (s *server) putResultStateV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireResultV2(w, r, id) {
		return
	}
	body := v2Body[retractResultStateInput](r)
	actor, ok := s.requireActor(w, r, body.Actor)
	if !ok {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" {
		writeError(w, r, apicode.RetractionInput,
			"reason says why the result is wrong, and the retraction keeps it for every later reader")
		return
	}
	unchanged := func(standing model.CheckResultRetracted) bool {
		present := body
		present.Reason = standing.Reason
		return putChangesNothing(body, present, setBy(actorIdentity(standing.Actor)), actorIdentity(actor))
	}
	if _, ok := s.retractCheckResultAs(w, r, actor, id, body.Reason, unchanged); !ok {
		return
	}
	result, err := scanCheckResult(s.deps.Store.Pool.QueryRow(r.Context(),
		`select `+checkResultColumns+` from check_results where id = $1`, id))
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the retracted result %s: %w", id, err))
		return
	}
	retraction, err := standingRetraction(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	result.Retracted = &retraction
	WriteJSON(w, http.StatusOK, result)
}

// listIssueStepsV2 answers GET /api/v2/issues/{key}/steps: one page of the runs GET
// /api/v1/issues/{key}/steps lists (issueStepTasks), task after task in its order, the issue held
// to exist first.
func (s *server) listIssueStepsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	steps, err := workflowstore.IssueSteps(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	records := make([]model.StepRecord, 0, len(steps))
	for _, task := range issueStepTasks(key, steps).Tasks {
		records = append(records, task.Steps...)
	}
	WriteJSON(w, http.StatusOK, pageOf(records, v2Query[issueStepsV2Query](r).pageQuery))
}

// putIssueStepV2 answers PUT /api/v2/issues/{key}/steps/{id}: the write PUT
// /api/v1/issues/{key}/steps/{id} makes (putIssueStepFrom), the issue held to exist first, once the
// no-op rule of dispatch://INFRA-4071/spec R8 has found the body is a change; a write that names no
// identity to an id no record has is 404 STEP_NOT_FOUND (R7). A record has one writer, the daemon
// of its issue's project, which the write holds every caller to, so a body every member of which
// the record holds as sent answers the record and writes nothing (noSetterRecorded).
func (s *server) putIssueStepV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[stepRecordInput](r)
	s.putIssueStepFrom(w, r, body, apicode.StepNotFound, func(stored workflowstore.StepRead) bool {
		return putChangesNothing(body, stepHeld(body, stored), noSetterRecorded, "")
	})
}

// stepHeld is the record stored in the body's own type, for the members sent states alone: a
// member sent leaves out, or sends null or "", states nothing (stepRecordInput), and stands as
// sent, and so does its actor, which is no field of the record.
func stepHeld(sent stepRecordInput, stored workflowstore.StepRead) stepRecordInput {
	held := sent
	text := func(member *string, value string) *string {
		if member == nil || *member == "" {
			return member
		}
		return &value
	}
	held.Task = text(sent.Task, stored.Task)
	held.Kind = text(sent.Kind, stored.Kind)
	held.Role = text(sent.Role, stored.Role)
	held.Type = text(sent.Type, stored.Type)
	held.Agent = text(sent.Agent, stored.Agent)
	held.Session = text(sent.Session, stored.Session)
	held.Model = text(sent.Model, stored.Model)
	held.Result = text(sent.Result, stored.Result)
	held.Report = text(sent.Report, stored.Report)
	if sent.Step != nil {
		held.Step = &stored.Place
	}
	if sent.Generation != nil {
		held.Generation = &stored.Generation
	}
	if sent.StartedAt != nil {
		held.StartedAt = stored.StartedAt
	}
	if sent.EndedAt != nil {
		held.EndedAt = stored.EndedAt
	}
	if sent.ReturnedTo != nil {
		held.ReturnedTo = stored.ReturnedTo
	}
	return held
}

// putIssueStepRatingV2 answers PUT /api/v2/issues/{key}/steps/{id}/rating: the write PUT
// /api/v1/issues/{key}/steps/{id}/rating makes (putIssueStepRatingFrom), the issue held to exist
// first, once the no-op rule of dispatch://INFRA-4071/spec R8 has found the body is a change; a
// write that names no identity to an id no record has is 404 STEP_NOT_FOUND (R7), as the step
// write's is (putIssueStepV2).
func (s *server) putIssueStepRatingV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[stepRatingInput](r)
	s.putIssueStepRatingFrom(w, r, body, apicode.StepNotFound, func(stored workflowstore.StepRead) bool {
		return putChangesNothing(body, ratingHeld(body, stored), noSetterRecorded, "")
	})
}

// ratingHeld is the record stored in the rating body's own type, as stepHeld writes a step body:
// the run's fields as stepHeld holds them, and the rating and its reason, which the body always
// sends, as the record holds them, none where it holds no rating.
func ratingHeld(sent stepRatingInput, stored workflowstore.StepRead) stepRatingInput {
	held := sent
	held.stepRecordInput = stepHeld(sent.stepRecordInput, stored)
	held.Rating, held.RatingReason = nil, nil
	if stored.Rating != nil {
		held.Rating, held.RatingReason = &stored.Rating.Rating, &stored.Rating.Reason
	}
	return held
}

// putIssuePlanV2 answers PUT /api/v2/issues/{key}/plans/{id}: the write PUT
// /api/v1/issues/{key}/plans/{id} makes (putIssuePlanFrom), the issue held to exist first, once the
// no-op rule of dispatch://INFRA-4071/spec R8 has found the body is a change. A plan has one
// writer, the daemon of its issue's project, which the write holds every caller to, so a body every
// member of which the plan holds as sent answers the plan and writes nothing (noSetterRecorded).
func (s *server) putIssuePlanV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[planRecordInput](r)
	s.putIssuePlanFrom(w, r, body, func(stored workflowstore.Plan) bool {
		return putChangesNothing(body, planHeld(body, stored), noSetterRecorded, "")
	})
}

// planHeld is the plan stored in the body's own type, for the members sent states alone: a
// document half the body leaves out, or sends null, "" or 0, states nothing
// (planRecordInput.document) and stands as sent, and so does its actor, which is no field of the
// plan.
func planHeld(sent planRecordInput, stored workflowstore.Plan) planRecordInput {
	held := sent
	if sent.Generation != nil {
		held.Generation = &stored.Generation
	}
	if sent.Source != nil {
		held.Source = &stored.Source
	}
	var document workflowstore.PlanDocument
	if stored.Document != nil {
		document = *stored.Document
	}
	if sent.ArtifactID != nil && *sent.ArtifactID != "" {
		held.ArtifactID = &document.ArtifactID
	}
	if sent.Version != nil && *sent.Version != 0 {
		held.Version = &document.Version
	}
	if sent.Tasks != nil {
		held.Tasks = make([]planTaskInput, len(stored.Tasks))
		for index, task := range stored.Tasks {
			held.Tasks[index] = planTaskInput(task)
		}
	}
	return held
}
