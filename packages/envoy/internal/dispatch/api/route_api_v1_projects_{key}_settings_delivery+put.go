package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/settings/delivery",
	Auth:        authAny,
	Description: "Update the project's delivery settings {delivery?, sources?, population_authors?, excluded_repos?, failure_sources?, failures_measured_since?, targets?} and answer them as GET does. Every top-level field is optional and keeps its stored value when the body leaves it out, the value it holds when the save is written, so a save that waited for another save of the project keeps what that one wrote; null clears a field that may be empty; sources, when present, replaces the list; targets sets each named eval's target, a number or \"off\", as a new version of the project's eval row in the same transaction, changing that target alone (a target left out, or one the eval holds already, writes nothing). A source whose repository and rule are unchanged keeps its id and its repository's reconcile progress; every other source's repository is imported again. 400 DELIVERY_SETTINGS_INPUT names the field, and the source's index for a source's field; with delivery on, 409 DELIVERY_SETTINGS_ACCESS names a source whose repository Dispatch's GitHub App cannot read, 409 DELIVERY_NO_DEFAULT_REPOSITORY a source naming no repository of a project with no default repository, and 409 DELIVERY_REPOSITORY_SHARED a repository another delivery-on project's source reads, naming that project; 409 DELIVERY_SETTINGS_CHANGED a save under each of whose two attempts another write moved the settings or a target it names; a refused save writes nothing. The settings belong to the deployment, so a project-scoped credential is refused them, the project this path names included. 404 PROJECT_NOT_FOUND for a project that does not exist.",
	Handler:     (*server).putProjectDeliverySettings,
	// A project's delivery settings: addressed under the project they serve, and not belonging to
	// it. Choosing what of the project's repositories counts as a deploy is the deployment's write
	// (dispatch://INFRA-978, R4), so a credential that names one project is refused it by name, the
	// project its path names included.
	WriteClass: scopeDeployment,
	// Every top-level field of projectDeliverySettingsInput is optional (a field the body leaves
	// out keeps its stored value; null clears a field that may be empty; sources present replaces
	// the list; a targets key changes that target alone). A refused save answers its own body
	// (delivery.SettingsError: the code, the reason, and the field, the source's index and the
	// other project where it names them), 400 for its input and 409 for every other refusal
	// (writeSettingsRefusal).
	Shape: routeShape{
		Status: http.StatusOK, Response: projectDeliverySettings{}, Request: projectDeliverySettingsInput{},
		Refusals: []routeAnswer{
			{Status: http.StatusBadRequest, Body: delivery.SettingsError{}},
			{Status: http.StatusConflict, Body: delivery.SettingsError{}},
		},
	},
})
