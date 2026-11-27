package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/production-events",
	Auth:        authAny,
	Description: "Post one deploy or failure {kind, source_ref, environment?, status?, url?, started_at?, finished_at?, detected_at?, repo?, commits?, pull_requests?, deploy_id?, commit_sha?, note?, actor?}, taking OpenTelemetry's deployment.environment.name, deployment.status, cicd.pipeline.run.url.full and cicd.pipeline.run.id beside environment, status and url (both names alike, or one): 201 with the new row, or 200 with the row the project already holds under source_ref, writing nothing; a system's repeat is answered so before its failure's deploy is looked up, so a retry after the deploy was dropped or stopped counting for the project is answered too. note is what the writer says of the event, kept as sent, at most 4,096 bytes. A deploy needs finished_at and an agent or service token; a credential holding the person scope is 403 PRODUCTION_EVENT_DEPLOY_FORBIDDEN. A deploy is one to production that succeeded, the deploys DORA counts: environment is production where it names none, status is succeeded where it names none, and a deploy naming another environment or another status is 400 PRODUCTION_EVENT_INPUT naming the field; a failure's environment and status are kept as sent. A failure names the deploy it blames one way, by deploy_id (the uuid of a deploy row), by cicd.pipeline.run.id (the GitHub run of repo) or by commit_sha (the earliest deploy holding it, by finished_at and then id, among the deploys that count for this project), and that deploy must count for this project, whichever project's row it is, so a deploy shared with another project is taken; a deploy no row matches, and one that counts for other projects alone, is 409 PRODUCTION_EVENT_NO_DEPLOY, one answer for both. The failure counts in this project's numbers alone; a system's failure is the signal api, and a person's is the signal person with the person as actor and the server's source_ref person:<deploy id>:<login>. A system's post without source_ref, or under one that starts with an automatic source's prefix (github:, person:, check:, revert: or incident:, the keys of the rows the GitHub adapter, a person's flag and the signals write), a finished_at before started_at, a finished_at or detected_at more than 5 minutes past the server's clock, a pull_requests entry that is not owner/repo#n, and any field no row can hold, is 400 PRODUCTION_EVENT_INPUT naming it; 404 PROJECT_NOT_FOUND an unknown project.",
	Handler:     (*server).postProductionEvent,
	// The record of deploys and failed changes: a posted event, a row of the project its path
	// names, so a credential scoped to another project is refused it.
	WriteClass: scopeProject,
	// A repeat of a source_ref the project holds answers that row, 200, and writes nothing.
	Shape: routeShape{
		Status: http.StatusCreated, Response: prodevents.Event{}, Request: productionEventInput{},
		Also: []routeAnswer{{Status: http.StatusOK, Body: prodevents.Event{}}},
	},
})
