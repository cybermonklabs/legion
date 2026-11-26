package api

// v2_projects.go is the projects, architecture, repositories, priorities and deployment settings
// of /api/v2 (R1 of dispatch://INFRA-4071/spec): the projects and the patch of
// one, which takes the default assignee, the gated steps and the default repository three PUTs of
// /api/v1 wrote (the route map's J16); a project's architecture, its sources and the one sync
// route the three syncs of /api/v1 become (J15); the finished issues with their plans and
// ratings, a row #4488 added; the related-item search; the near-copy gate's records, in the
// envelope (two rows #4103 added, which no row of the route map names); the read-only repository
// mappings the last release left and the App's installation on one repository; the priorities and
// the acts of one (J29); the three deployment settings (J19); and the counts of the related-item
// checks (J30). Every route runs the work its /api/v1 route runs (createProjectFrom,
// writeProjectDefaultAssignee, writeProjectGatedSteps, writeProjectDefaultRepository,
// getArchitectureTree, querySources, putArchitectureSourceFrom, deleteArchitectureSourceAs,
// syncProjectSources, projectFinishedIssues, answerRelated, readNearCopyCheck, getAppInstallation,
// readPriorities, createPriorityFrom, patchPriorityFrom, deletePriorityAs, priorityActsOf,
// getNewIssueSettings, getCredentialCapabilities, getGoingCapability, relatedChecksReportOf), each
// path object checked first and answered with its own 404 (R7); the app-installation read's
// {owner}/{repo} keeps the answer /api/v1 gives it (R7).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// The refusals of the objects a project group route's path names (dispatch://INFRA-4071/spec R7).
var (
	priorityMissingRefusal           = apicode.Refusal{Condition: apicode.PriorityMissing, Code: apicode.PriorityNotFound}
	nearCopyCheckMissingRefusal      = apicode.Refusal{Condition: apicode.NearCopyCheckMissing, Code: apicode.NearCopyCheckNotFound}
	architectureSourceMissingRefusal = apicode.Refusal{Condition: apicode.ArchitectureSourceMissing, Code: apicode.SourceNotFound}
	projectHasNoSourceRefusal        = apicode.Refusal{Condition: apicode.ProjectHasNoSource, Code: apicode.SourceNotFound}
	sourceInputRefusal               = apicode.Refusal{Condition: apicode.SourceInputInvalid, Code: apicode.SourceInput}
	sourceUnreadableRefusal          = apicode.Refusal{Condition: apicode.SourceUnreadable, Code: apicode.SourceAccess}
	projectInputRefusal              = apicode.Refusal{Condition: apicode.ProjectInputInvalid, Code: apicode.InvalidProject}
	projectKeyHeldRefusal            = apicode.Refusal{Condition: apicode.ProjectKeyHeld, Code: apicode.ProjectExists}
	excludeIssueInvalidRefusal       = apicode.Refusal{Condition: apicode.ExcludeIssueInvalid, Code: apicode.ExcludeIssueInput}
	repositoryPathInvalidRefusal     = apicode.Refusal{Condition: apicode.RepositoryPathInvalid, Code: apicode.InvalidRepository}
	gitHubAppKeyMissingRefusal       = apicode.Refusal{Condition: apicode.GitHubAppKeyMissing, Code: apicode.GitHubAppUnconfigured}
	priorityInputRefusal             = apicode.Refusal{Condition: apicode.PriorityInputInvalid, Code: apicode.PriorityInput}
	priorityIDHeldRefusal            = apicode.Refusal{Condition: apicode.PriorityIDHeld, Code: apicode.PriorityExists}
	priorityWrittenRefusal           = apicode.Refusal{Condition: apicode.PriorityWrittenSinceRead, Code: apicode.PriorityChanged}
)

// projectDefaultAssigneeText is what the routes that read or write a project's default assignee
// say of it.
const projectDefaultAssigneeText = "the person a project's unowned work lands on: who an issue created with no " +
	"assignee, by nobody, under no parent or under a parent with nobody on it, goes to, and who a question on an issue " +
	"nobody holds reaches"

// architectureSourceText is what every route answering architecture sources says of the row.
const architectureSourceText = "each source {project, repo, branch, enabled, created_by, created_at, last_sync_at, " +
	"last_commit, last_error, components}, components how many components the project holds from it"

// projectDefaultRepositoryText is what the routes that read or write a project's default repository
// say of it.
const projectDefaultRepositoryText = "the repository, owner/name, an issue of the project resolves to when neither " +
	"the issue nor an issue above it names one"

// projectProvenSessionsText is what the routes that read or write a project's proven-sessions
// setting say of it.
const projectProvenSessionsText = "true while the project's proven-sessions setting is on, under which an " +
	"architect's write of an issue's spec or Verification document, and a role-holder publication, pass only on a " +
	"credential that proves the writer's session"

// v2ProjectRows are the project, architecture, repository, priority and settings routes: rows 147
// and 182; 148; 166 and 167, with the default repository's PUT #4457 added; 149; 150, 152 and 178;
// 154; 155; 151, 153 and 156; 168 and 169; the plans row of #4488 and the two near-copy rows of
// #4103; 183; 73; 142; 143; 144; 145; 146; 181; 63; 74; and 172 of the route map. Rows 184 and 185,
// the repository mapping writes, are gone with /api/v1's, since #4457 Dispatch writes no mapping.
func (s *server) v2ProjectRows() []routeRow {
	person := routeScopeSet{model.ScopePerson}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/projects", Auth: authAny, Versions: onlyV2,
			Description: "List every project by key: {key, name, open_asks, gated_steps, default_repository?, " +
				"proven_sessions, created_at}, open_asks the open asks on its open issues and its documents, gated_steps " +
				"the design-gate steps the Legion deployment that serves it gates, in the order it gates them ([] for " +
				"none), default_repository " + projectDefaultRepositoryText + ", absent for a project with none, " +
				"proven_sessions " + projectProvenSessionsText + ". include=default_assignee adds " +
				"default_assignee, " + projectDefaultAssigneeText + ", which needs the person scope (403 " +
				"OAUTH_SCOPE_REQUIRED otherwise): the default names a person. limit 1 to 200 (default 50) and offset; " +
				"{items, total, limit, offset}.",
			Handler:  s.listProjectsV2,
			Resource: "projects", Kind: kindCollection, Key: "{key}",
			Query: projectListV2Query{},
			Shape: routeShape{Status: http.StatusOK, Response: page[projectV2]{}},
			Authorities: []authorityRule{{
				Where: authorityWhere{In: partQuery, Name: "include"}, Value: "default_assignee", Scopes: person,
			}},
			Refusals: listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/projects", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Create a project {key, name, default_assignee?}, a bearer naming its session in "+
				"actor. Every project names a default assignee, %s. default_assignee is the email of a person who has "+
				"signed in, and a call that names none takes the person creating the project, else the person its "+
				"credential acts for. A login nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED. A credential that "+
				"acts for nobody and names no default_assignee, a key that is not a project key, an empty name, a name "+
				"holding a control character or a bidi embedding, override or isolate, or a name with no visible "+
				"character is 400 INVALID_PROJECT; a name over %d characters is 400 CAP_EXCEEDED; a key that already "+
				"exists is 409 "+
				"PROJECT_EXISTS. A credential scoped to one project is refused, since a project belongs to the deployment. "+
				"Answers 201 the project as GET /api/v2/projects lists it, with the default_assignee the create stored, "+
				"and appends project.created.",
				projectDefaultAssigneeText, contracts.ProjectNameMax),
			Handler:  s.createProjectV2,
			Resource: "projects", Kind: kindCollection, Key: "{key}",
			Body:  requestBody{Kind: bodyJSON, Type: createProjectInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: projectV2{}},
			Scope: scopeDeployment,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectInputRefusal, assigneeNotSignedInRefusal,
				valuePastBoundRefusal, projectKeyHeldRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/projects/{key}", Auth: authAny, Versions: onlyV2,
			Description: "Change a project {default_assignee?, gated_steps?, default_repository?, proven_sessions?}, a " +
				"bearer naming its session in actor. default_assignee moves " + projectDefaultAssigneeText + " to another " +
				"person, the email of a person who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise), and appends " +
				"settings.project_default_assignee.updated naming both people; a body naming default_assignee at all, null " +
				"included, needs the person scope (403 OAUTH_SCOPE_REQUIRED otherwise), and since a project always names a " +
				"person, the default cannot be cleared (null leaves it as it stands). gated_steps publishes the " +
				"design-gate steps the Legion deployment that serves the project gates, a list of decisions, criteria and " +
				"spec in the order it gates them or [] for none, which needs no scope: the deployment's daemon writes it at " +
				"every boot. default_repository sets " + projectDefaultRepositoryText + ": \"owner/name\" sets it, read " +
				"lowercase with one trailing .git trimmed and stored under its new name where the repository was renamed, " +
				"and null clears it. The default " +
				"belongs to the deployment and needs no scope, so a credential scoped to one project is refused it (403 " +
				"PROJECT_SCOPE_DEPLOYMENT), the project this path names included. proven_sessions turns the project's " +
				"proven-sessions setting on (true) or off (false): with it on, an architect's write of an issue's spec or " +
				"Verification document passes only on a credential that proves the architect's session, and a role-holder " +
				"publication only on one that proves legion-daemon:<project>, or legion-daemon:<P> of another project's daemon " +
				"on a broker token that names P. Turning it on makes a check stronger, so any credential that may write " +
				"the project's issues turns it on and it needs no scope, a token scoped to another project included; a " +
				"token scoped to one project gets 403 PROJECT_SCOPE for a project that does not exist and for a private " +
				"project that it does not name. Turning it off weakens " +
				"the check, so proven_sessions false needs the person scope (403 OAUTH_SCOPE_REQUIRED otherwise), and its " +
				"event names the person. A list, a default repository or a setting the project already holds changes " +
				"nothing and appends no event; another appends project.updated. A body that names none of the four " +
				"fields, a blank default_assignee, a step that is none of the three or is named twice, and a " +
				"default_repository that is not a repository GitHub could hold are 400 INVALID_PROJECT. Every field is " +
				"written in one transaction. Answers the project as GET /api/v2/projects lists it, with default_assignee " +
				"where the patch named it. 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.patchProjectV2,
			Resource: "projects", Kind: kindItem, Key: "{key}",
			Body:  requestBody{Kind: bodyJSON, Type: patchProjectV2Input{}},
			Shape: routeShape{Status: http.StatusOK, Response: projectV2{}},
			Authorities: []authorityRule{
				{Where: authorityWhere{In: partBody, Name: "default_assignee"}, Scopes: person},
				{Where: authorityWhere{In: partBody, Name: "proven_sessions"}, Value: "false", Scopes: person},
			},
			Scope: scopeProject, ProjectOf: projectPathProject,
			Refusals: []apicode.Refusal{
				undeclaredKeyRefusal, projectMissingRefusal, projectInputRefusal, assigneeNotSignedInRefusal,
				{Condition: apicode.DeploymentMemberScoped, Code: apicode.ProjectScopeDeployment},
			},
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/architecture", Auth: authAny, Versions: onlyV2,
			Description: "A project's component tree with the work attached: {sources, totals, components, unassigned, " +
				"not_architectural, retired_links}, its sources' sync bookkeeping, each component with its source repository, " +
				"per-component counts and issue rows, and the issues no component counts - unassigned, declared not " +
				"architectural, or linked only to retired components. With Dispatch-Trees: 1 each component's prose carries " +
				"its display tree. A read-only token may call it. 404 SOURCE_NOT_FOUND for a project with no architecture " +
				"source, 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.getArchitectureTreeV2,
			Resource: "architecture", Kind: kindReport,
			Shape:    routeShape{Status: http.StatusOK, Response: architectureTreeRead{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, projectHasNoSourceRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/plans", Auth: authAny, Versions: onlyV2,
			Description: "Each finished issue of the project, one whose status is done and whose closed_at is set, " +
				"newest close first, its close within from and to on closed_at, RFC3339, from inclusive and to exclusive " +
				"(400 INVALID_QUERY when from is not before to): each {issue, title, closed_at, plan, ratings, " +
				"started_at, ended_at, duration_seconds, failed_steps, experiment, variant}. plan is the plan of the " +
				"issue's newest run as the Legion daemon wrote it {id, issue, generation, source, artifact_id, version, " +
				"tasks: [{id, steps}]}, null for an issue with none; ratings is the rating of each of its retros, in the " +
				"order they ran, {rating, reason, step_id, ended_at}; started_at is the earliest start of its step " +
				"records, ended_at the latest end and duration_seconds the whole seconds between them, each null where " +
				"no record holds one, and duration_seconds null too where that end is before that start, a time " +
				"Dispatch cannot count; failed_steps counts its step records of kind step that failed; experiment and " +
				"variant are the experiment a draw put the issue in and the variant it drew there, as the step records " +
				"of its newest run name them, each null where they name none. It writes nothing, and a read-only token " +
				"may call it. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. 404 " +
				"PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.listProjectPlansV2,
			Resource: "plans", Kind: kindCollection, TimeFields: []string{"closed_at"},
			Query:    projectPlansV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[model.FinishedIssue]{}},
			Refusals: listRefusals(projectMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/architecture-sources", Auth: authAny, Versions: onlyV2,
			Description: "A project's architecture sources, one per repository, in the order they were added: " +
				architectureSourceText + ". The project's component model is every source's components together. limit 1 to " +
				"200 (default 50) and offset; {items, total, limit, offset}, no item for a project with no source. 404 " +
				"PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.listProjectArchitectureSourcesV2,
			Resource: "architecture-sources", Kind: kindCollection, Key: "{owner}/{repo}",
			Query:    architectureSourcesV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[model.ArchitectureSource]{}},
			Refusals: listRefusals(projectMissingRefusal),
		},
		{
			Method: http.MethodDelete, Pattern: "/projects/{key}/architecture-sources/{owner}/{repo}", Auth: authAny, Versions: onlyV2,
			Description: "Remove one architecture source of a project and the components it projected, a bearer naming " +
				"its session in actor; every other source's components stay. Appends settings.architecture_source.updated " +
				"with deleted true. 204. A repository that is not owner/name is 400 SOURCE_INPUT, one the project has no " +
				"source from 404 SOURCE_NOT_FOUND; 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.deleteArchitectureSourceV2,
			Resource: "architecture-sources", Kind: kindItem, Key: "{owner}/{repo}",
			Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
			Shape: routeShape{Status: http.StatusNoContent},
			Scope: scopeProject, ProjectOf: projectPathProject,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, sourceInputRefusal, architectureSourceMissingRefusal},
		},
		{
			Method: http.MethodPut, Pattern: "/projects/{key}/architecture-sources/{owner}/{repo}", Auth: authAny, Versions: onlyV2,
			Description: "Add a repository as an architecture source of a project, or change the branch of the source it " +
				"is {branch}, a bearer naming its session in actor, after proving the GitHub App can read it and that branch " +
				"is the name of a branch it has; a missing branch, a commit SHA, a relative ref (main~1, HEAD) or a refs/ path " +
				"is 409 SOURCE_ACCESS naming the value and the repository, as a credential problem is, and nothing is " +
				"written. A blank branch and a repository that is not owner/name are 400 SOURCE_INPUT. A save resets the " +
				"source's sync bookkeeping, keeps its components and appends settings.architecture_source.updated. A source " +
				"records no setter, so a save of the branch an enabled source already has, by any caller, answers the source " +
				"as it stands and writes nothing, the access check and the reset included. Answers 200 the source: " +
				architectureSourceText + ". 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.putArchitectureSourceV2,
			Resource: "architecture-sources", Kind: kindItem, Key: "{owner}/{repo}",
			Body:  requestBody{Kind: bodyJSON, Type: putArchitectureSourceInput{}},
			Shape: routeShape{Status: http.StatusOK, Response: model.ArchitectureSource{}},
			Scope: scopeProject, ProjectOf: projectPathProject,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, sourceInputRefusal, sourceUnreadableRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/architecture-syncs", Auth: authAny, Versions: onlyV2,
			Description: "Import a project's architecture sources now {repos?}: repos names the sources to import, each " +
				"owner/name (400 SOURCE_INPUT for one that is not), and absent or [] imports every source of the project. " +
				"Each source is imported whatever another's import did, and each failure - an access failure, a " +
				"credential or branch problem, included - is on its own row's last_error, the previous projection kept " +
				"up. 404 SOURCE_NOT_FOUND answers three cases: a named source the project has no source from, naming it, " +
				"before any is imported; a named source removed while the sync runs, naming it, once the sources before " +
				"it are imported; and absent or [] on a project with none. An import stores the source's sync " +
				"bookkeeping and, on a change, its " +
				"components, and appends architecture.synced or architecture.sync_failed only when the source's state " +
				"changes. Answers 200 {items, total, limit, offset}, every source imported in the order they were added: " +
				architectureSourceText + ". 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.syncArchitectureV2,
			Resource: "architecture-syncs", Kind: kindCollection,
			Body:  requestBody{Kind: bodyJSON, Type: architectureSyncV2Input{}, Optional: true},
			Shape: routeShape{Status: http.StatusOK, Response: page[model.ArchitectureSource]{}},
			Scope: scopeProject, ProjectOf: projectPathProject,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, sourceInputRefusal,
				architectureSourceMissingRefusal, projectHasNoSourceRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/related-searches", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Run the related-item check an issue create runs over a title and a text as long "+
				"as an issue's spec {title?, text?, exclude_issue?, exclude_issues?, keywords_only?} (checked as the title, a "+
				"newline and the text; either alone is enough), writing nothing - no issue, ask, suggestion or "+
				"related_checks record - and waiting at most 5 seconds. A title over %d characters is 400 CAP_EXCEEDED, "+
				"and the text is held to the body's 1 MiB bound. exclude_issue leaves that issue of the project and "+
				"everything it holds out of the results and out of the word statistics; exclude_issues leaves each of up to "+
				"%d issues of the project out of the results alone (400 EXCLUDE_ISSUE_INPUT for a key the project does not "+
				"hold, or a longer list); keywords_only true skips meaning search. Answers the create's advice.suggestions "+
				"shape: outcome found, none or not_made (a blank text is not_made, the reason in missing), search, "+
				"search_note, search_query and related, with up to 3 open issues before any item of another kind and "+
				"everything a done issue owns last. include=other_projects adds other_projects beside related: at most 2 "+
				"answered question asks of other projects, each with its ask state, its answer and the project it stands in, "+
				"ranked by meaning alone and offered with no claim that they answer the question. 404 PROJECT_NOT_FOUND for "+
				"a project that does not exist. It answers for both forms of /api/v1/projects/{key}/related, so the Link of "+
				"that route's GET names this POST: the GET's query fields go in this route's JSON body.",
				contracts.IssueTitleMax, relatedExcludeLimit),
			Handler:  s.postRelatedSearchV2,
			Resource: "related-searches", Kind: kindComputation,
			Query: relatedSearchV2Query{},
			Body:  requestBody{Kind: bodyJSON, Type: relatedRequest{}},
			Shape: routeShape{Status: http.StatusOK, Response: model.Suggestions{}},
			Scope: scopeProject, ProjectOf: projectPathProject,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, valuePastBoundRefusal, excludeIssueInvalidRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/near-copy-checks", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("List a project's near-copy gate answers, one row for each create the gate "+
				"answered (an external create runs no gate and has none), recorded when the create was refused or once it "+
				"was written, oldest first: those within from and to on created_at, RFC3339, from inclusive and to "+
				"exclusive (400 INVALID_QUERY when from is not before to). A row written in its create's transaction holds "+
				"that transaction's start as created_at and is seen only once it commits, so a window should end some time "+
				"before now. A row is {id, created_at, project, actor_kind, actor_id, outcome, reason, force, title, "+
				"spec_sha256, parent, issue, candidates, refuse_score, nudge_score, build_commit, read_ms, read_stage}, with "+
				"no spec: GET /api/v2/near-copy-checks/{id} reads one row with it. outcome is refused (nothing was written; "+
				"every issue create the gate refused has this row, written before the answer), forced, nudged, passed or "+
				"not_made, and "+
				"reason bound, failed or unrecorded for not_made and null otherwise; candidates are the issues the gate read "+
				"at or above nudge_score, at most %d, best first; read_ms is how long the create waited on the gate's read, "+
				"in milliseconds, and read_stage the step that read ended in (connect, terms, keys or score), both null on a "+
				"row written before they were recorded. limit 1 to %d (default %d) and offset; {items, total, "+
				"limit, offset}. 404 PROJECT_NOT_FOUND for a project that does not exist.",
				duplicateCandidateLimit, nearCopyChecksPageMax, nearCopyChecksPageMax),
			Handler:  s.listNearCopyChecksV2,
			Resource: "near-copy-checks", Kind: kindCollection, Key: "{id}",
			TimeFields: []string{"created_at"},
			Query:      nearCopyChecksV2Query{},
			Shape:      routeShape{Status: http.StatusOK, Response: page[model.NearCopyCheck]{}},
			List:       listBounds{Default: nearCopyChecksPageMax, Max: nearCopyChecksPageMax},
			Refusals:   listRefusals(projectMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/near-copy-checks/{id}", Auth: authAny, Versions: onlyV2,
			Description: "One near-copy gate answer by id: the row GET /api/v2/projects/{key}/near-copy-checks lists, with " +
				"spec, the spec as sent, on a refused row alone (absent on every other, whose issue holds it; empty for a " +
				"refused create that sent none). 404 NEAR_COPY_CHECK_NOT_FOUND for an id no answer has, a value that is no " +
				"uuid among them.",
			Handler:  s.getNearCopyCheckV2,
			Resource: "near-copy-checks", Kind: kindItem, Key: "{id}",
			Shape:    routeShape{Status: http.StatusOK, Response: model.NearCopyCheck{}},
			IDs:      &idRoute{Reason: "a near-copy check id: one answer of the near-copy gate"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, nearCopyCheckMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/repositories", Auth: authAny, Versions: onlyV2,
			Description: "Read only: the GitHub repository to project mappings as the last release left them, by " +
				"repository, of every project: {repo, project, default, renamed_to, created_by, created_at}, default " +
				"whether it was its project's default repository and renamed_to the owner/name it is called today, null " +
				"for one that was never renamed. Nothing writes them: PATCH /api/v2/projects/{key} sets a project's " +
				"default repository (default_repository). project=<key>, repeatable, keeps the mappings of those " +
				"projects, with no default project. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listRepositoriesV2,
			Resource: "repositories", Kind: kindCollection, Key: "{owner}/{repo}",
			CrossProject: true,
			Query:        repositoriesV2Query{},
			Shape:        routeShape{Status: http.StatusOK, Response: page[model.RepoProject]{}},
			Refusals:     listRefusals(),
		},
		{
			Method: http.MethodGet, Pattern: "/repositories/{owner}/{repo}/app-installation", Auth: authAny, Versions: onlyV2,
			Description: "Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is " +
				"installed on the repository, read from GitHub on every call: {installed}, false when GitHub answers 404, " +
				"for a repository no mapping names too. 400 INVALID_REPOSITORY, before GitHub is asked anything, when " +
				"the path's owner or name has a character GitHub does not allow or the name is . or .. or empty or longer " +
				"than 100 characters (a trailing .git trimmed). 503 GITHUB_APP_UNCONFIGURED when Dispatch holds no App " +
				"key, 502 GITHUB_UNREADABLE naming the repository and GitHub's error on any other failure.",
			Handler:  s.getAppInstallation,
			Resource: "app-installation", Kind: kindSingleton,
			Shape:    routeShape{Status: http.StatusOK, Response: appInstallationResponse{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, repositoryPathInvalidRefusal, gitHubAppKeyMissingRefusal, gitHubReadFailedRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/priorities", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Every live priority, in rank order: id, title, description (markdown of any "+
				"length), owner, rank (its place in the one order), state live, and when and by whom it was made, each "+
				"beside the work that advances it (work): by_status, the open issues in each lifecycle status, claimed, "+
				"open_asks, closed_last_7_days and newest_event_at. The work under a priority is the issues that state the "+
				"relation and every issue below one of them. Beside the envelope it answers the two faults: "+
				"work_under_no_priority {count, by_project}, the open issues in flight (in_progress, testing, needs_review, "+
				"retro) that advance no live priority, and priority_work_parked {count, by_priority}, the open issues in "+
				"backlog or icebox whose own row names a live priority no issue above them carries (a child that only "+
				"inherits a priority, or restates one an issue above it carries, is no such fault), by_priority counting "+
				"each under every priority it so names; include=fault_issues adds to each its issues, the newest %d rows, each "+
				"carrying advances as the issue read does. empty is the sentence saying why the list is empty where no "+
				"priority is live. A priority belongs to no project, so any authenticated caller reads this; project=<key>, "+
				"repeatable, narrows the work counted and both faults to those projects and never the priorities "+
				"themselves. limit 0 to 200 (default 50) and offset; {items, total, limit, offset}, total every live "+
				"priority. limit=0 answers no priority and, without include=fault_issues, the two fault counts alone, with "+
				"no by_project and no by_priority, which is what the navigation rail reads.", priorityFaultRows),
			Handler:  s.listPrioritiesV2,
			Resource: "priorities", Kind: kindCollection, Key: "{id}",
			CrossProject: true,
			Query:        prioritiesV2Query{},
			Shape:        routeShape{Status: http.StatusOK, Response: prioritiesV2{}},
			List:         listBounds{Default: defaultListLimit, Max: maxListLimit, Counts: true},
			Refusals:     listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/priorities", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Write one priority {id?, title, description?, owner?}, a bearer naming its session "+
				"in actor: title is required, and a title with nothing in it or over %[1]d characters is 400 "+
				"PRIORITY_INPUT. A body that names no id takes the title written as a slug (lowercase ASCII letters and "+
				"digits, each other run of characters one hyphen), and a slug over %[2]d characters is cut to its longest "+
				"start of at most %[2]d characters that ends at the end of a word, or to its first %[2]d characters when its "+
				"first word is longer, so a long title is written whole under a cut id. A title with no ASCII letter or "+
				"digit, or one the cut leaves as none or any, takes the id priority- and the first 12 lower-case hex "+
				"characters of the SHA-256 of the title's UTF-8 bytes instead, so every title gives an id. A given id that "+
				"is no slug of lowercase letters, digits and single hyphens, one over %[2]d characters, and none and any "+
				"(the issue listing's keywords) are each 400 PRIORITY_INPUT; an id the store already holds is 409 "+
				"PRIORITY_EXISTS. The new priority takes the last place of the order. A credential scoped to one project "+
				"is refused, since a priority belongs to none. Answers 201 the priority.", contracts.IssueTitleMax, priorityIDMax),
			Handler:  s.createPriorityV2,
			Resource: "priorities", Kind: kindCollection, Key: "{id}",
			Body:     requestBody{Kind: bodyJSON, Type: createPriorityInput{}},
			Shape:    routeShape{Status: http.StatusCreated, Response: model.Priority{}},
			Scope:    scopeDeployment,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, priorityInputRefusal, priorityIDHeldRefusal},
		},
		{
			Method: http.MethodDelete, Pattern: "/priorities/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Delete a priority, a bearer naming its session in actor: its state is written gone and no row is " +
				"removed, so the listing answers without it while its acts still name who ended it and when. A priority " +
				"already gone is 404 PRIORITY_NOT_FOUND, as is an id no priority carries; 204 otherwise. A credential scoped " +
				"to one project is refused.",
			Handler:  s.deletePriorityV2,
			Resource: "priorities", Kind: kindItem, Key: "{id}",
			Body:     requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
			Shape:    routeShape{Status: http.StatusNoContent},
			Scope:    scopeDeployment,
			IDs:      &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, priorityMissingRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/priorities/{id}", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Write one priority's title, description or owner, and move its order {title?, "+
				"description?, owner?, rank?, expected?}, a bearer naming its session in actor: each field named is "+
				"written and each left out is left alone. rank is {before, after}, the priority this one lands above and "+
				"the one it lands below; a rank naming neither, a neighbour that is this priority, and one no live "+
				"priority carries are each 400 RANK_INPUT. A title with nothing in it or over %[1]d characters and a body "+
				"naming nothing to write are 400 PRIORITY_INPUT. expected is {title?, description?}, the value the "+
				"writer's edit of each started from, for the title and the description this same write writes; owner "+
				"and rank take none. An expected naming an attribute the write does not write is 400 PRIORITY_INPUT. One "+
				"that differs from the value the priority stores now (the title compared with the stored title exactly, "+
				"which is stored trimmed, so a writer sends the title it read; the description exactly) is 409 "+
				"PRIORITY_CHANGED, whose message names each attribute that differs and its value now, quoted and cut "+
				"past %[2]d characters with a trailing …; that write writes nothing and appends no priority.updated. The "+
				"compare and the write are one transaction holding the priority's row, so a write naming expected waits "+
				"for any other write of the priority in flight and compares against what that one wrote. A body with no "+
				"expected writes over whatever is stored. Answers the priority and appends priority.updated. A "+
				"credential scoped to one project is refused. 404 PRIORITY_NOT_FOUND for an id no priority carries, or "+
				"one whose priority is gone.", contracts.IssueTitleMax, priorityChangedQuoteMax),
			Handler:  s.patchPriorityV2,
			Resource: "priorities", Kind: kindItem, Key: "{id}",
			Body:     requestBody{Kind: bodyJSON, Type: patchPriorityInput{}},
			Shape:    routeShape{Status: http.StatusOK, Response: model.Priority{}},
			Scope:    scopeDeployment,
			IDs:      &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, priorityMissingRefusal, priorityInputRefusal, rankInvalidRefusal, priorityWrittenRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/priorities/{id}/acts", Auth: authAny, Versions: onlyV2,
			Description: "One priority's own acts: every act stated about it as {field, value, issue, asserted, actor, at}. " +
				"field is what was stated - title, description, owner and rank, the four a person writes, state (live when " +
				"it was made, gone when it was deleted) and visibility; advances is the relation an issue states on it. " +
				"value is what an attribute write wrote and is empty on an advances act; issue is the issue at the other end " +
				"of an advances act and is empty on an attribute write; asserted is false on the act that retracted an " +
				"advances and true on every other. sort=-at, the default, lists the newest first, and sort=at the oldest. A " +
				"deleted priority still answers its acts, the delete its newest. limit 1 to 200 (default 50) and offset; " +
				"{items, total, limit, offset}. 404 PRIORITY_NOT_FOUND for an id no priority carries.",
			Handler:  s.listPriorityActsV2,
			Resource: "acts", Kind: kindCollection,
			Query:    priorityActsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[model.PriorityAct]{}},
			IDs:      &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
			Refusals: listRefusals(priorityMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/settings/new-issue", Auth: authAny, Versions: onlyV2,
			Description: "What the dashboard's search bar files a person's unmatched typed words as: {label, session}, the " +
				"label that new issue carries and the session it is routed to and its first message goes to, as this " +
				"deployment configured them (DISPATCH_NEW_ISSUE_LABEL, DISPATCH_NEW_ISSUE_SESSION). Each is \"\" where it " +
				"configured none: no label on the issue, and no conversation started on it. The bar writes the issue itself, " +
				"through POST /api/v2/issues and the issue's own artifact, patch and message routes.",
			Handler:  s.getNewIssueSettings,
			Resource: "new-issue", Kind: kindSingleton,
			Shape:    routeShape{Status: http.StatusOK, Response: newIssueSettings{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/settings/credential-broker", Auth: authAny, Versions: onlyV2,
			Description: "What the credential pages may offer against this deployment's secrets broker, relayed from the " +
				"broker's own RFC 8414 metadata rather than from any Dispatch setting: {machine_code_page}, whether the " +
				"broker still serves the device grant, so whether a machine ever prints a code for a person to type; false " +
				"for a Dispatch with no broker. Needs the person scope.",
			Handler:  s.getCredentialCapabilities,
			Resource: "credential-broker", Kind: kindSingleton,
			Shape:       routeShape{Status: http.StatusOK, Response: credentialCapabilities{}},
			OAuthScopes: person,
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/settings/text-model", Auth: authAny, Versions: onlyV2,
			Description: "Whether this deployment has a text model configured: {model_configured} and nothing else, so no " +
				"endpoint, deployment name or credential can appear in it. Any caller Dispatch admits reads it, a pod's own " +
				"agent token included.",
			Handler:  s.getGoingCapability,
			Resource: "text-model", Kind: kindSingleton,
			Shape:    routeShape{Status: http.StatusOK, Response: goingCapability{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/related-check-stats", Auth: authAny, Versions: onlyV2,
			Description: "Count the related-item checks recorded when an issue, an ask or a new decision block was " +
				"written: those within from and to on created_at, RFC3339, from inclusive and to exclusive (400 INVALID_QUERY " +
				"when from is not before to); project=<key>, repeatable, keeps those projects' checks, with no default " +
				"project (400 INVALID_PROJECT for a value that is not a key); source_kind=issue, ask or decision_block keeps " +
				"that kind's. {project, source_kind, from, to} echo the filters (project [] and the rest null when absent), " +
				"total, by_source_kind, by_outcome (found, none, not_made), by_search (meaning_and_keywords, keywords_only, " +
				"not_made), by_reason (each reason a check was not made or searched by keywords only), completed (found or " +
				"none), skipped_for_time (reason timeout), other_project_offers, and keyword_ms and total_ms each {count, " +
				"p50, p95, max, over_300ms}: the checks holding that time, its discrete percentiles and largest value (null " +
				"with no check), and how many took over 300 ms. other_project_offers is how many ask creates of the window " +
				"offered at least one answered question of another project, a count of writes and not of items, read from " +
				"write_suggestions over the same window and filters, and zero for a source kind no such row carries; the two " +
				"records are written at different moments and only the check's own row is retried, so a count at a window's " +
				"edge can name one fewer write than the window's total.",
			Handler:  s.getRelatedCheckStatsV2,
			Resource: "related-check-stats", Kind: kindReport,
			TimeFields: []string{"created_at"}, CrossProject: true,
			Query:    relatedCheckStatsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: relatedCheckStatsV2{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, queryProjectMalformedRefusal},
		},
	}
}

// projectListV2Query is GET /api/v2/projects's query.
type projectListV2Query struct {
	pageQuery
	// Include default_assignee adds each project's default assignee, which needs the person scope.
	Include []string `query:"include,comma" enum:"default_assignee"`
}

// projectV2 is a project as every /api/v2 read of projects answers it: model.Project's members,
// with its default repository under the name PATCH /api/v2/projects/{key} writes it by, and its
// default assignee where the request asks for it (include=default_assignee, or a patch that wrote
// it), since the default names a person and only a caller holding the person scope reads it, and
// on the create's answer, which tells the creator the default the create stored as POST
// /api/v1/projects does (createdProject). It is a type of its own, so no /api/v1 shape's tags
// change (dispatch://INFRA-4071/spec R3).
type projectV2 struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	OpenAsks   int      `json:"open_asks"`
	GatedSteps []string `json:"gated_steps"`
	// DefaultRepository is model.Project's DefaultRepo: the repository an issue of the project
	// resolves to when neither the issue nor an issue above it names one. A project with none
	// carries no member, as GET /api/v1/projects answers it.
	DefaultRepository *string `json:"default_repository,omitempty"`
	// ProvenSessions is model.Project's ProvenSessions: the project's proven-sessions setting,
	// which PATCH /api/v2/projects/{key} writes.
	ProvenSessions  bool      `json:"proven_sessions"`
	CreatedAt       time.Time `json:"created_at"`
	DefaultAssignee *string   `json:"default_assignee,omitempty"`
}

// projectV2Query reads the projects GET /api/v2/projects pages through, by key, each with its open
// asks counted as GET /api/v1/projects counts them (projectOpenAsksJoin), its default repository,
// its proven-sessions setting and its default assignee: every project, or the one $1 names, $2 of
// them from $3.
const projectV2Query = `
		select p.key, p.name, coalesce(open_asks.total, 0), p.gated_steps, p.default_repo, p.proven_sessions, p.created_at, p.default_assignee
		from projects p` + projectOpenAsksJoin + `
		where $1::text is null or p.key = $1
		order by p.key
		limit $2 offset $3`

// readProjectsV2 is limit projects from offset, by key: every project, or the one key names where
// it is not nil, each with its default assignee where withDefault says so.
func (s *server) readProjectsV2(r *http.Request, key *string, limit, offset int, withDefault bool) ([]projectV2, error) {
	rows, err := s.deps.Store.Pool.Query(r.Context(), projectV2Query, key, limit, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (projectV2, error) {
		var project projectV2
		var assignee string
		if err := row.Scan(&project.Key, &project.Name, &project.OpenAsks, &project.GatedSteps, &project.DefaultRepository, &project.ProvenSessions, &project.CreatedAt, &assignee); err != nil {
			return projectV2{}, err
		}
		if withDefault {
			project.DefaultAssignee = &assignee
		}
		return project, nil
	})
}

// listProjectsV2 answers GET /api/v2/projects: one page of the projects GET /api/v1/projects lists,
// each with its default assignee where include=default_assignee asks for it, which GET
// /api/v1/settings/project-default-assignees answered (the route map's J17).
func (s *server) listProjectsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[projectListV2Query](r)
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select count(*) from projects`).Scan(&total); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	projects, err := s.readProjectsV2(r, nil, query.Limit, query.Offset, slices.Contains(query.Include, "default_assignee"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(projects, total, query.pageQuery))
}

// createProjectV2 answers POST /api/v2/projects: the create POST /api/v1/projects makes
// (createProjectFrom), answered as GET /api/v2/projects lists the project, with the default
// assignee the create stored, so one project has one shape at /api/v2 (dispatch://INFRA-4071/spec
// R1).
func (s *server) createProjectV2(w http.ResponseWriter, r *http.Request) {
	created, ok := s.createProjectFrom(w, r, v2Body[createProjectInput](r))
	if !ok {
		return
	}
	WriteJSON(w, http.StatusCreated, projectV2{
		Key: created.Key, Name: created.Name, OpenAsks: created.OpenAsks, GatedSteps: created.GatedSteps,
		DefaultRepository: created.DefaultRepo, ProvenSessions: created.ProvenSessions, CreatedAt: created.CreatedAt,
		DefaultAssignee: &created.DefaultAssignee,
	})
}

// patchProjectV2Input is PATCH /api/v2/projects/{key}'s body: the four columns of a project the
// PUTs and the DELETE of /api/v1 wrote (the route map's J16, and the proven-sessions setting #4725
// added), each absent to leave it as it stands.
type patchProjectV2Input struct {
	// DefaultAssignee is the email of the person the project's unowned work lands on, which
	// needs the person scope; null leaves it as it stands, and blank is refused, since the default
	// cannot be cleared.
	DefaultAssignee *string `json:"default_assignee"`
	// GatedSteps is the design-gate steps the deployment that serves the project gates, in the
	// order it gates them, [] for none.
	GatedSteps *[]string `json:"gated_steps" enum:"decisions,criteria,spec"`
	// DefaultRepository is the repository an issue of the project resolves to when neither the
	// issue nor an issue above it names one: "owner/name" sets it and null clears it. It is held as
	// its bytes because absent, null and a value are its three states (parseIssueRepo).
	DefaultRepository json.RawMessage `json:"default_repository"`
	// ProvenSessions turns the project's proven-sessions setting on (true), as PUT
	// /api/v1/projects/{key}/proven-sessions does, or off (false), as its DELETE does, which needs
	// the person scope (the row's authority rule).
	ProvenSessions *bool        `json:"proven_sessions"`
	Actor          *model.Actor `json:"actor"`
}

// patchProjectV2 answers PATCH /api/v2/projects/{key}: the writes PUT
// /api/v1/projects/{key}/default-assignee, PUT /api/v1/projects/{key}/gated-steps, PUT
// /api/v1/projects/{key}/default-repository and PUT and DELETE
// /api/v1/projects/{key}/proven-sessions make (writeProjectDefaultAssignee,
// writeProjectProvenSessions, writeProjectGatedSteps, writeProjectDefaultRepository), each held to
// its own rule, in one transaction, answered as the project GET /api/v2/projects lists. The
// default assignee is the person's own call, and turning the proven-sessions setting off weakens a
// check, so a patch naming either writes its events as the person the credential acts for
// (actingPerson), whose person scope the authority gate has already held it to; every other patch
// names the caller as any write does (requireActor). The default repository belongs to the
// deployment, so a credential scoped to one project is refused it here, the project this path
// names included, as the PUT's write class refuses it (scopeDeployment); the row's own class,
// scopeProject, is the other three fields'.
func (s *server) patchProjectV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	project, err := s.loadProject(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	body := v2Body[patchProjectV2Input](r)
	if covers, scoped := s.callerProject(r); scoped && len(body.DefaultRepository) > 0 {
		writeError(w, r, apicode.ProjectScopeDeployment, fmt.Sprintf(
			"this request's credential names project %s, and a project's default_repository belongs to the deployment rather than to a project",
			covers))
		return
	}
	repo, repoNamed, err := parseIssueRepo(body.DefaultRepository, "default_repository", apicode.InvalidProject)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if body.DefaultAssignee == nil && body.GatedSteps == nil && !repoNamed && body.ProvenSessions == nil {
		writeError(w, r, apicode.InvalidProject, "a patch of a project names default_assignee, gated_steps, default_repository, proven_sessions or more than one of them")
		return
	}
	var actor model.Actor
	var ok bool
	if body.DefaultAssignee != nil || (body.ProvenSessions != nil && !*body.ProvenSessions) {
		actor, ok = s.actingPerson(w, r)
	} else {
		actor, ok = s.requireActor(w, r, body.Actor)
	}
	if !ok {
		return
	}
	var assignee string
	if body.DefaultAssignee != nil {
		if assignee, ok = s.allowedDefaultAssignee(w, r, "default_assignee", *body.DefaultAssignee); !ok {
			return
		}
	}
	var steps []string
	if body.GatedSteps != nil {
		if steps, ok = validGatedSteps(w, r, "gated_steps", body.GatedSteps); !ok {
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	var events []model.Event
	if body.DefaultAssignee != nil {
		event, err := s.writeProjectDefaultAssignee(r.Context(), tx, actor, key, assignee)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		events = append(events, event)
	}
	// The setting before the gated steps and the default repository: each of their writes reads
	// the project row again under its owner lock, so its event carries the setting this patch
	// wrote.
	if body.ProvenSessions != nil {
		event, err := s.writeProjectProvenSessions(r.Context(), tx, actor, &project, *body.ProvenSessions)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	if body.GatedSteps != nil {
		event, err := s.writeProjectGatedSteps(r.Context(), tx, actor, &project, steps)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	// The default repository last: its write reads the project row back in tx, so its event
	// carries the row as every field of this patch left it.
	if repoNamed {
		_, event, err := s.writeProjectDefaultRepository(r.Context(), tx, actor, key, s.renamedRepo(repo))
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), events...)
	patched, err := s.readProjectsV2(r, &key, 1, 0, body.DefaultAssignee != nil)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if len(patched) == 0 {
		s.writeHandlerError(w, r, errorf(apicode.ProjectNotFound, "project not found"))
		return
	}
	WriteJSON(w, http.StatusOK, patched[0])
}

// requireProjectV2 answers whether the project key names exists, and where it does not writes 404
// PROJECT_NOT_FOUND (dispatch://INFRA-4071/spec R7).
func (s *server) requireProjectV2(w http.ResponseWriter, r *http.Request, key string) bool {
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	return true
}

// getArchitectureTreeV2 answers GET /api/v2/projects/{key}/architecture: the tree GET
// /api/v1/projects/{key}/architecture answers (getArchitectureTree), the project held to exist
// first.
func (s *server) getArchitectureTreeV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireProjectV2(w, r, r.PathValue("key")) {
		return
	}
	s.getArchitectureTree(w, r)
}

// projectPlansV2Query is GET /api/v2/projects/{key}/plans's query: one page of the finished
// issues, their close within [from, to), each bound optional.
type projectPlansV2Query struct {
	pageQuery
	From *time.Time `query:"from"`
	To   *time.Time `query:"to"`
}

// listProjectPlansV2 answers GET /api/v2/projects/{key}/plans: one page of the finished issues GET
// /api/v1/projects/{key}/plans lists (projectFinishedIssues), newest close first, the project held
// to exist first and its close within from and to, which /api/v1 names since and until.
func (s *server) listProjectPlansV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireProjectV2(w, r, key) {
		return
	}
	query := v2Query[projectPlansV2Query](r)
	if query.From != nil && query.To != nil && !query.From.Before(*query.To) {
		writeError(w, r, apicode.InvalidQuery, "from must be before to")
		return
	}
	finished, err := projectFinishedIssues(r.Context(), s.deps.Store.Pool, key, workflowstore.Window{Since: query.From, Until: query.To})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(finished, query.pageQuery))
}

// architectureSourcesV2Query is GET /api/v2/projects/{key}/architecture-sources's query: one page.
type architectureSourcesV2Query struct {
	pageQuery
}

// listProjectArchitectureSourcesV2 answers GET /api/v2/projects/{key}/architecture-sources: one
// page of the sources GET /api/v1/projects/{key}/architecture-sources lists (querySources).
func (s *server) listProjectArchitectureSourcesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireProjectV2(w, r, key) {
		return
	}
	sources, err := querySources(r.Context(), s.deps.Store.Pool, `where project_key = $1 order by id`, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(sources, v2Query[architectureSourcesV2Query](r).pageQuery))
}

// deleteArchitectureSourceV2 answers DELETE /api/v2/projects/{key}/architecture-sources/{owner}/{repo}:
// the removal DELETE /api/v1 makes there (deleteArchitectureSourceAs), a bearer naming its session
// in actor.
func (s *server) deleteArchitectureSourceV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireProjectV2(w, r, r.PathValue("key")) {
		return
	}
	actor, ok := s.requireActor(w, r, v2Body[actorInput](r).Actor)
	if !ok {
		return
	}
	s.deleteArchitectureSourceAs(w, r, actor)
}

// putArchitectureSourceV2 answers PUT /api/v2/projects/{key}/architecture-sources/{owner}/{repo}:
// the save PUT /api/v1 makes there (putArchitectureSourceFrom), under the no-op rule of
// dispatch://INFRA-4071/spec R8 (architectureSourceSaveChangesNothing).
func (s *server) putArchitectureSourceV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireProjectV2(w, r, r.PathValue("key")) {
		return
	}
	s.putArchitectureSourceFrom(w, r, v2Body[putArchitectureSourceInput](r), architectureSourceSaveChangesNothing)
}

// architectureSourceSaveChangesNothing reports whether a save of input, its branch trimmed, would
// leave the stored source exactly as it stands: an enabled source of the same branch. A disabled
// source is enabled by the save, which is a change. A source records no setter, so the save that
// changes nothing answers the source as it stands whoever sends it (noSetterRecorded).
func architectureSourceSaveChangesNothing(stored model.ArchitectureSource, input putArchitectureSourceInput) bool {
	return stored.Enabled &&
		putChangesNothing(input, putArchitectureSourceInput{Branch: stored.Branch, Actor: input.Actor}, noSetterRecorded, "")
}

// architectureSyncV2Input is POST /api/v2/projects/{key}/architecture-syncs's body.
type architectureSyncV2Input struct {
	// Repos names the sources to import, each owner/name; absent or [] imports every source of
	// the project.
	Repos []string `json:"repos"`
}

// syncArchitectureV2 answers POST /api/v2/projects/{key}/architecture-syncs, the one route the
// three syncs of /api/v1 become (the route map's J15): every source of the project, as POST
// /api/v1/projects/{key}/architecture-sources/sync imports them (syncProjectSources), or the ones
// repos names, each as POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync imports
// it (Importer.Sync). Every named source is held to exist before any is imported, and each is
// imported whatever another's import did. A failure recorded on its row - an access failure
// among them, which the one-source route of /api/v1 answers 409 SOURCE_ACCESS - is that row's
// last_error at 200; only one that could not be recorded is a 500, and a named source removed
// while the sync runs is the 404 of a source the project does not hold, once those before it are
// imported.
func (s *server) syncArchitectureV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireProjectV2(w, r, key) {
		return
	}
	repos := v2Body[architectureSyncV2Input](r).Repos
	if len(repos) == 0 {
		sources, ok := s.syncProjectSources(w, r)
		if !ok {
			return
		}
		WriteJSON(w, http.StatusOK, pageOf(sources, pageQuery{}))
		return
	}
	named := make([]string, 0, len(repos))
	for _, raw := range repos {
		owner, name, err := parseSourceRepo(raw)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		if repo := owner + "/" + name; !slices.Contains(named, repo) {
			named = append(named, repo)
		}
	}
	held, err := querySources(r.Context(), s.deps.Store.Pool, `where project_key = $1 and repo = any($2) order by id`, key, named)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	for _, repo := range named {
		if !slices.ContainsFunc(held, func(source model.ArchitectureSource) bool { return source.Repo == repo }) {
			writeSourceNotFound(w, r, key, repo)
			return
		}
	}
	sources := make([]model.ArchitectureSource, 0, len(held))
	for _, source := range held {
		synced, err := s.deps.Architecture.Sync(r.Context(), key, source.Repo)
		switch {
		case errors.Is(err, architecture.ErrNoSource):
			writeSourceNotFound(w, r, key, source.Repo)
			return
		case err != nil && synced.Project == "":
			// Sync could not even record the failure on the row: the database, not the model
			// or GitHub, is what broke.
			s.writeHandlerError(w, r, err)
			return
		}
		sources = append(sources, synced)
	}
	WriteJSON(w, http.StatusOK, pageOf(sources, pageQuery{}))
}

// relatedSearchV2Query is POST /api/v2/projects/{key}/related-searches's query.
type relatedSearchV2Query struct {
	// Include other_projects adds the answered questions of other projects beside related.
	Include []string `query:"include,comma" enum:"other_projects"`
}

// postRelatedSearchV2 answers POST /api/v2/projects/{key}/related-searches: the check POST
// /api/v1/projects/{key}/related runs (answerRelated), its title capped as an issue's is, which
// offers the answered questions of other projects only where include=other_projects asks for them
// (the route map's J30); /api/v1's POST always offered them.
func (s *server) postRelatedSearchV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireProjectV2(w, r, r.PathValue("key")) {
		return
	}
	request := v2Body[relatedRequest](r)
	if length := itemblocks.Units16(request.Title); length > contracts.IssueTitleMax {
		capExceeded(w, r, "title", length, contracts.IssueTitleMax)
		return
	}
	s.answerRelated(w, r, request, slices.Contains(v2Query[relatedSearchV2Query](r).Include, "other_projects"))
}

// nearCopyChecksV2Query is GET /api/v2/projects/{key}/near-copy-checks's query: one page, within a
// window of created_at.
type nearCopyChecksV2Query struct {
	pageQuery
	// From and To window created_at, From inclusive and To exclusive.
	From *time.Time `query:"from"`
	To   *time.Time `query:"to"`
}

// The two reads of GET /api/v2/projects/{key}/near-copy-checks: one page of a project's rows ($1),
// within [$2, $3) when given, oldest first, $4 of them from $5, and how many rows the window
// holds.
const (
	nearCopyChecksV2PageQuery = `
select ` + nearCopyCheckColumns + `, null::text
  from near_copy_checks
 where project_key = $1
   and ($2::timestamptz is null or created_at >= $2)
   and ($3::timestamptz is null or created_at < $3)
 order by created_at, id
 limit $4 offset $5`
	nearCopyChecksV2CountQuery = `
select count(*)
  from near_copy_checks
 where project_key = $1
   and ($2::timestamptz is null or created_at >= $2)
   and ($3::timestamptz is null or created_at < $3)`
)

// listNearCopyChecksV2 answers GET /api/v2/projects/{key}/near-copy-checks: the rows GET
// /api/v1/near-copy-checks?project={key} reads, in the envelope, paged by limit and offset in
// place of /api/v1's after and next_after, since this read is none of the five logs of
// dispatch://INFRA-4071/spec R5.
func (s *server) listNearCopyChecksV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	// Ahead of the project check, which reads the pool too: a server with no store is answered
	// DATABASE_UNAVAILABLE rather than a read of a pool it does not have.
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	key := r.PathValue("key")
	if !s.requireProjectV2(w, r, key) {
		return
	}
	query := v2Query[nearCopyChecksV2Query](r)
	if query.From != nil && query.To != nil && !query.From.Before(*query.To) {
		writeError(w, r, apicode.InvalidQuery, "from must be before to")
		return
	}
	var total int
	if err := pool.QueryRow(r.Context(), nearCopyChecksV2CountQuery, key, query.From, query.To).Scan(&total); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	rows, err := pool.Query(r.Context(), nearCopyChecksV2PageQuery, key, query.From, query.To, query.Limit, query.Offset)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	checks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.NearCopyCheck, error) {
		var check model.NearCopyCheck
		err := scanNearCopyCheck(row, &check)
		return check, err
	})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(checks, total, query.pageQuery))
}

// getNearCopyCheckV2 answers GET /api/v2/near-copy-checks/{id}: the row GET
// /api/v1/near-copy-checks/{id} reads (readNearCopyCheck), an id that is no uuid naming no row
// either (dispatch://INFRA-4071/spec R7).
func (s *server) getNearCopyCheckV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	id := r.PathValue("id")
	err = pgx.ErrNoRows
	var check model.NearCopyCheck
	if _, parsed := uuid.Parse(id); parsed == nil {
		check, err = readNearCopyCheck(r.Context(), pool, id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, apicode.NearCopyCheckNotFound, "no near-copy check has the id "+id)
		return
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, check)
}

// repositoriesV2Query is GET /api/v2/repositories's query.
type repositoriesV2Query struct {
	pageQuery
	// Project keeps the mappings of these projects; none is every project.
	Project []string `query:"project"`
}

// The two reads of GET /api/v2/repositories: one page of the mappings of the projects $1 names, or
// of every project where it is null, by repository, $2 of them from $3, and how many there are.
const (
	repositoriesV2PageQuery = `
		select ` + repoProjectColumns + `
		from repo_projects
		where $1::text[] is null or project = any($1)
		order by repo
		limit $2 offset $3`
	repositoriesV2CountQuery = `
		select count(*) from repo_projects where $1::text[] is null or project = any($1)`
)

// listRepositoriesV2 answers GET /api/v2/repositories: one page of the mappings GET
// /api/v1/settings/repo-projects lists, of the projects project= names (the route map's J18).
func (s *server) listRepositoriesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[repositoriesV2Query](r)
	var projects []string
	for _, project := range query.Project {
		if named := strings.TrimSpace(project); named != "" {
			projects = append(projects, named)
		}
	}
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), repositoriesV2CountQuery, projects).Scan(&total); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), repositoriesV2PageQuery, projects, query.Limit, query.Offset)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	mappings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.RepoProject, error) { return scanRepoProject(row) })
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(mappings, total, query.pageQuery))
}

// prioritiesV2Query is GET /api/v2/priorities's query.
type prioritiesV2Query struct {
	pageQuery
	// Project narrows the work counted and both faults to these projects, never the priorities.
	Project []string `query:"project"`
	// Include fault_issues adds each fault's newest rows.
	Include []string `query:"include,comma" enum:"fault_issues"`
}

// prioritiesV2 is GET /api/v2/priorities's answer: the envelope of priorities, each beside the work
// under it, and the two faults beside the envelope, each with its rows only where
// include=fault_issues asks for them. It is a type of its own, so no /api/v1 shape's tags change
// (dispatch://INFRA-4071/spec R3).
type prioritiesV2 struct {
	page[model.PriorityRow]
	WorkUnderNoPriority workUnderNoPriorityV2 `json:"work_under_no_priority"`
	PriorityWorkParked  priorityWorkParkedV2  `json:"priority_work_parked"`
	// Empty is the sentence saying why the list is empty, where no priority is live.
	Empty string `json:"empty,omitempty"`
}

// workUnderNoPriorityV2 is the first fault (model.WorkUnderNoPriority): its count, its counts by
// project but on the read of the counts alone (limit=0 with no include=fault_issues), which does
// not group them, and its rows with include=fault_issues.
type workUnderNoPriorityV2 struct {
	Count     int                    `json:"count"`
	ByProject *map[string]int        `json:"by_project,omitempty"`
	Issues    *[]model.PriorityIssue `json:"issues,omitempty"`
}

// priorityWorkParkedV2 is the second fault (model.PriorityWorkParked): its count, its counts by
// priority but on the read of the counts alone, and its rows with include=fault_issues.
type priorityWorkParkedV2 struct {
	Count      int                    `json:"count"`
	ByPriority *map[string]int        `json:"by_priority,omitempty"`
	Issues     *[]model.PriorityIssue `json:"issues,omitempty"`
}

// listPrioritiesV2 answers GET /api/v2/priorities: one page of what GET /api/v1/priorities reads
// (readPriorities). limit=0 is /api/v1's summary=true: the read of the two fault counts alone,
// which fills no priority's row and reads no fault row, unless include=fault_issues asks for the
// rows, which only the whole read reads.
func (s *server) listPrioritiesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[prioritiesV2Query](r)
	projects := make([]string, 0, len(query.Project))
	for _, project := range query.Project {
		if named := strings.TrimSpace(project); named != "" {
			projects = append(projects, named)
		}
	}
	withIssues := slices.Contains(query.Include, "fault_issues")
	summary := query.Limit == 0 && !withIssues
	body, live, err := s.readPriorities(r.Context(), projects, summary)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	answer := prioritiesV2{
		page:                newPage[model.PriorityRow](nil, live, query.pageQuery),
		WorkUnderNoPriority: workUnderNoPriorityV2{Count: body.WorkUnderNoPriority.Count},
		PriorityWorkParked:  priorityWorkParkedV2{Count: body.PriorityWorkParked.Count},
		Empty:               body.Empty,
	}
	if query.Limit > 0 {
		answer.page = pageOf(body.Priorities, query.pageQuery)
		// Each description on the page carries its display tree for a caller that sent
		// `Dispatch-Trees: 1`, as GET /api/v1/priorities answers every one (readPriorityTrees).
		s.readPriorityTrees(r, answer.Items)
	}
	if !summary {
		answer.WorkUnderNoPriority.ByProject = &body.WorkUnderNoPriority.ByProject
		answer.PriorityWorkParked.ByPriority = &body.PriorityWorkParked.ByPriority
	}
	if withIssues {
		answer.WorkUnderNoPriority.Issues = &body.WorkUnderNoPriority.Issues
		answer.PriorityWorkParked.Issues = &body.PriorityWorkParked.Issues
	}
	WriteJSON(w, http.StatusOK, answer)
}

// createPriorityV2 answers POST /api/v2/priorities: the write POST /api/v1/priorities makes
// (createPriorityFrom).
func (s *server) createPriorityV2(w http.ResponseWriter, r *http.Request) {
	s.createPriorityFrom(w, r, v2Body[createPriorityInput](r))
}

// requirePriorityV2 answers whether the priority id names exists, gone or live, and where it does
// not writes 404 PRIORITY_NOT_FOUND (dispatch://INFRA-4071/spec R7). A gone priority's writes are
// refused with the same code by the store itself (refusePriorityWrite).
func (s *server) requirePriorityV2(w http.ResponseWriter, r *http.Request, id string) bool {
	_, held, err := prioritystore.Read(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	if !held {
		writeError(w, r, apicode.PriorityNotFound, fmt.Sprintf("no priority of the id %q", id))
	}
	return held
}

// deletePriorityV2 answers DELETE /api/v2/priorities/{id}: the delete DELETE /api/v1/priorities/{id}
// makes (deletePriorityAs), a bearer naming its session in actor.
func (s *server) deletePriorityV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requirePriorityV2(w, r, r.PathValue("id")) {
		return
	}
	actor, ok := s.requireActor(w, r, v2Body[actorInput](r).Actor)
	if !ok {
		return
	}
	s.deletePriorityAs(w, r, actor)
}

// patchPriorityV2 answers PATCH /api/v2/priorities/{id}: the write PATCH /api/v1/priorities/{id}
// makes (patchPriorityFrom).
func (s *server) patchPriorityV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requirePriorityV2(w, r, r.PathValue("id")) {
		return
	}
	s.patchPriorityFrom(w, r, v2Body[patchPriorityInput](r))
}

// priorityActsV2Query is GET /api/v2/priorities/{id}/acts's query.
type priorityActsV2Query struct {
	pageQuery
	Sort querySort `query:"sort" enum:"at" default:"-at"`
}

// listPriorityActsV2 answers GET /api/v2/priorities/{id}/acts: one page of the acts GET
// /api/v1/priorities/{id}/history answers (priorityActsOf), newest first, or oldest first with
// sort=at (the route map's J29).
func (s *server) listPriorityActsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requirePriorityV2(w, r, id) {
		return
	}
	query := v2Query[priorityActsV2Query](r)
	acts, err := priorityActsOf(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !query.Sort.Descending {
		slices.Reverse(acts)
	}
	WriteJSON(w, http.StatusOK, pageOf(acts, query.pageQuery))
}

// relatedCheckStatsV2Query is GET /api/v2/related-check-stats's query.
type relatedCheckStatsV2Query struct {
	// Project keeps the checks of these projects; none is every project.
	Project []string `query:"project"`
	// SourceKind keeps the checks of the writes of one kind.
	SourceKind string `query:"source_kind" enum:"issue,ask,decision_block"`
	// From and To window created_at, From inclusive and To exclusive.
	From *time.Time `query:"from"`
	To   *time.Time `query:"to"`
}

// relatedCheckStatsV2 is GET /api/v2/related-check-stats's answer: model.RelatedChecksReport, its
// project echo the list project= names, where /api/v1's names one project. It is a type of its
// own, so no /api/v1 shape's tags change (dispatch://INFRA-4071/spec R3).
type relatedCheckStatsV2 struct {
	Project            []string                 `json:"project"`
	SourceKind         *string                  `json:"source_kind"`
	From               *time.Time               `json:"from"`
	To                 *time.Time               `json:"to"`
	Total              int                      `json:"total"`
	BySourceKind       map[string]int           `json:"by_source_kind"`
	ByOutcome          map[string]int           `json:"by_outcome"`
	BySearch           map[string]int           `json:"by_search"`
	ByReason           map[string]int           `json:"by_reason"`
	Completed          int                      `json:"completed"`
	SkippedForTime     int                      `json:"skipped_for_time"`
	OtherProjectOffers int                      `json:"other_project_offers"`
	KeywordMS          model.RelatedCheckTiming `json:"keyword_ms"`
	TotalMS            model.RelatedCheckTiming `json:"total_ms"`
}

// getRelatedCheckStatsV2 answers GET /api/v2/related-check-stats: the counts GET
// /api/v1/related-checks answers (relatedChecksReportOf), of every project project= names.
func (s *server) getRelatedCheckStatsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[relatedCheckStatsV2Query](r)
	projects := []string{}
	for _, project := range query.Project {
		project = strings.TrimSpace(project)
		if !projectKeyPattern.MatchString(project) {
			writeError(w, r, apicode.InvalidProject, projectKeyProblem("project", project))
			return
		}
		if !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	if query.From != nil && query.To != nil && !query.From.Before(*query.To) {
		writeError(w, r, apicode.InvalidQuery, "from must be before to")
		return
	}
	report := model.RelatedChecksReport{From: query.From, To: query.To}
	if query.SourceKind != "" {
		report.SourceKind = &query.SourceKind
	}
	var filter []string
	if len(projects) > 0 {
		filter = projects
	}
	if err := s.relatedChecksReportOf(r.Context(), &report, filter); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, relatedCheckStatsV2{
		Project: projects, SourceKind: report.SourceKind, From: report.From, To: report.To,
		Total: report.Total, BySourceKind: report.BySourceKind, ByOutcome: report.ByOutcome, BySearch: report.BySearch,
		ByReason: report.ByReason, Completed: report.Completed, SkippedForTime: report.SkippedForTime,
		OtherProjectOffers: report.OtherProjectOffers, KeywordMS: report.KeywordMS, TotalMS: report.TotalMS,
	})
}
