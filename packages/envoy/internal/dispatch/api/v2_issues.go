package api

// v2_issues.go is the issues of /api/v2 (R1 of dispatch://INFRA-4071/spec): the
// list of issues across projects, which also answers for GET /api/v1/issues/resolve (ref=), and a
// project's own list of the same rows (R1.3 of dispatch://INFRA-2946/spec); the create; the lean
// read of one issue, whose include= gives what GET /api/v1/issues/{key} answered (the route map's
// J3); the patch, which takes the design gates PUT /api/v1/issues/{key}/design-gates wrote (J16);
// the claim; and an issue's collaborators, coverage, decisions and the keep of one, references,
// checks, results and the retraction of one, subscribers, the steps #4400 added to /api/v1 and the
// write of one, the rating of a retro's run and the plan of a run #4488 added, the dry run of a
// result #4517 added, and the experiment and the variant of a run #5014 added. Every route runs
// the work its /api/v1 route runs (readIssueListing, createIssueFrom, readIssueParts, patchIssueAs,
// claimIssueAs, releaseIssueClaimAs, issueCollaboratorsOf, issueCoverage, issueDecisionsOf,
// writeDecisionKeep, issueClosure, readIssueChecks, recordCheckResultFrom, dryRunCheckResultFrom,
// retractCheckResultAs, subscribersOf, unsubscribeSession, issueStepTasks, putIssueStepFrom,
// putIssueStepRatingFrom, putIssueStepExperimentFrom, putIssuePlanFrom), each path object checked
// first and answered with its own 404, and a {session_id} keeps the answer /api/v1 gives it (R7).

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
	"started_at, ended_at, result, returned_to, report, rating, rating_reason, experiment, variant}, attempt its " +
	"place among the issue's runs of the same task, step and kind from 1, rating and rating_reason a retro's " +
	"alone, and experiment and variant the experiment a draw put the issue in and the variant it drew there, each " +
	"null where the run holds none"

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
// priority (a priority of 0 among them, which needs the p0 scope), labels and parent, a title past
// its bound, and the input either refuses whole.
func issueFieldRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append([]apicode.Refusal{
		undeclaredKeyRefusal, valuePastBoundRefusal, assigneeNotSignedInRefusal, priorityInvalidRefusal,
		labelsInvalidRefusal,
		{Condition: apicode.PriorityZeroWithoutP0Scope, Code: apicode.OAuthScopeRequired},
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

// stepWriteRefusals are the refusals of every step write route, the run's, its rating's and its
// experiment's (requireIssueDaemon, writeIssueStep): an issue that does not exist, an id no record
// holds under a write that names no identity to make one, a value outside its form, a record of
// another issue, and a writer other than the session of its project's Legion daemon.
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
	"live priority or any for the work that advances one, own or inherited, several a union; own=true keeps, of the "+
	"issues the advances values answer, those whose own row names a priority a value matches and no issue above them "+
	"carries it, so a child that only inherits a priority or restates one an issue above it carries is left out and "+
	"none keeps nothing, and without an advances value it filters nothing (advances=any&own=true over backlog and "+
	"icebox is the second fault of GET /api/v2/priorities); open=true the open "+
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

var (
	v2IssueList     = listBounds{Default: contracts.DefaultIssuePageLimit, Max: contracts.MaxIssuePageLimit}
	v2IssueTimes    = []string{"updated_at", "closed_at"}
	v2ClaimRefusals = []routeAnswer{{Status: http.StatusConflict, Body: issueClaimedError{}}}
)

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
	// Own keeps, of the issues the Advances values answer, those whose own row names a matched
	// priority no issue above them carries, as GET /api/v1/issues's own=true does
	// (advancesOriginTerms); without an Advances value it filters nothing.
	Own bool `query:"own"`
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
	// ComponentsReasonTree is the tree of the reason the issue's attachment gives for touching no
	// component (Components.Reason, of mode `none`), for a caller that sent Dispatch-Trees: 1, as
	// GET /api/v1/issues/{key} answers it (issueDetail).
	ComponentsReasonTree model.DisplayTree `json:"components_reason_tree,omitempty"`
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
	// A status option or dry_run set to true selects the multi-issue status-change form. One set
	// to false asks for nothing, as issueStatusOptionsOf (packages/contracts/src/dispatch-tools.ts)
	// reads the options: with none true, the plain PATCH runs on updateIssueInput, which declares
	// none of these members, so a false one is taken as absent.
	IncludeChildren *bool `json:"include_children,omitempty"`
	UnlinkChildren  *bool `json:"unlink_children,omitempty"`
	IncludeParent   *bool `json:"include_parent,omitempty"`
	DryRun          *bool `json:"dry_run,omitempty"`
}

func (body patchIssueV2Input) isStatusChange() bool {
	for _, option := range []*bool{body.IncludeChildren, body.UnlinkChildren, body.IncludeParent, body.DryRun} {
		if option != nil && *option {
			return true
		}
	}
	return false
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
// document, and null on every other row. NameTree is the tree of the row's name for a caller that
// sent Dispatch-Trees: 1, read where it stands as /api/v1's row reads it (referenceLinkReads).
type referenceRowV2 struct {
	model.ReferenceLink
	Artifact *artifactV2       `json:"artifact"`
	NameTree model.DisplayTree `json:"name_tree,omitempty"`
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
// fault: /api/v2 answers it 500, as /api/v1 does. With trees, each row's name carries its tree,
// read where it stands as newIssueReferencesRead reads /api/v1's.
func referenceRowsV2(ctx context.Context, q queryer, relation string, links []model.ReferenceLink, trees bool) ([]referenceRowV2, error) {
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
	shapeBuilt(trees, func(reader *displayTreeReader) {
		for index := range rows {
			reader.text(&rows[index].NameTree, rows[index].Name)
		}
	})
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
		labels[index] = labelKey(label)
	}
	labelsFrom, labelsTo := labelFoldMap(labels)
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
		filters.Moved, filters.Own, labelsFrom, labelsTo, store.MicrosecondCeiling(filters.UpdatedTo), keys}
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
	if parts.issue.Components.Mode == "none" {
		reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
		reader.textOf(&answer.ComponentsReasonTree, parts.issue.Components.Reason)
		reader.resolve()
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
	rows, err := referenceRowsV2(r.Context(), s.deps.Store.Pool, query.Relation, listed.Items, wantsDisplayTrees(r))
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
	plan, err := workflowstore.NewestPlan(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	records := make([]model.StepRecord, 0, len(steps))
	for _, task := range issueStepTasks(key, plan, steps).Tasks {
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

// putIssueStepExperimentV2 answers PUT /api/v2/issues/{key}/steps/{id}/experiment: the write PUT
// /api/v1/issues/{key}/steps/{id}/experiment makes (putIssueStepExperimentFrom), the issue held to
// exist first, once the no-op rule of dispatch://INFRA-4071/spec R8 has found the body is a
// change; a write that names no identity to an id no record has is 404 STEP_NOT_FOUND (R7), as the
// step write's is (putIssueStepV2).
func (s *server) putIssueStepExperimentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[stepExperimentInput](r)
	s.putIssueStepExperimentFrom(w, r, body, apicode.StepNotFound, func(stored workflowstore.StepRead) bool {
		return putChangesNothing(body, experimentHeld(body, stored), noSetterRecorded, "")
	})
}

// experimentHeld is the record stored in the experiment body's own type, as stepHeld writes a step
// body: the run's fields as stepHeld holds them, and the experiment and its variant, which the body
// always sends, as the record holds them, none where it holds no experiment.
func experimentHeld(sent stepExperimentInput, stored workflowstore.StepRead) stepExperimentInput {
	held := sent
	held.stepRecordInput = stepHeld(sent.stepRecordInput, stored)
	held.Experiment, held.Variant = nil, nil
	if stored.Experiment != nil {
		held.Experiment, held.Variant = &stored.Experiment.Experiment, &stored.Experiment.Variant
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
