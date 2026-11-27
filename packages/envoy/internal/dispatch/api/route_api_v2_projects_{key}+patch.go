package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
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
			{Where: authorityWhere{In: partBody, Name: "default_assignee"}, Scopes: v2PersonScope},
			{Where: authorityWhere{In: partBody, Name: "proven_sessions"}, Value: "false", Scopes: v2PersonScope},
		},
		Scope: scopeProject, ProjectOf: projectPathProject,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, projectMissingRefusal, projectInputRefusal, assigneeNotSignedInRefusal,
			{Condition: apicode.DeploymentMemberScoped, Code: apicode.ProjectScopeDeployment},
		},
	}
})
