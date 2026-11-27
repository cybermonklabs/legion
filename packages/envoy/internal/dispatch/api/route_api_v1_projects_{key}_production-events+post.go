package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/production-events",
	Auth:        authAny,
	Description: "Post one deploy or failure {kind, source_ref, environment?, status?, url?, started_at?, finished_at?, detected_at?, repo?, commits?, pull_requests?, deploy_id?, commit_sha?, note?, actor?}, taking OpenTelemetry's deployment.environment.name, deployment.status, cicd.pipeline.run.url.full and cicd.pipeline.run.id beside environment, status and url (both names alike, or one): 201 with the new row, or 200 with the row the project already holds under source_ref, writing nothing. note is what the writer says of the event, kept as sent, at most 4,096 bytes. A deploy needs finished_at and an agent or service token; a credential holding the person scope is 403 PRODUCTION_EVENT_DEPLOY_FORBIDDEN. A failure names the deploy it blames one way, by deploy_id (the uuid of a deploy row, of any project), by cicd.pipeline.run.id (the GitHub run of repo) or by commit_sha (the first deploy that shipped it), 409 PRODUCTION_EVENT_NO_DEPLOY when no deploy row matches; a system's failure is the signal api, and a person's is the signal person with the person as actor and the server's source_ref person:<deploy id>:<login>. A system's post without source_ref, or under one that starts with an automatic source's prefix (github:, person:, check:, revert: or incident:, the keys of the rows the GitHub adapter, a person's flag and the signals write), and any field no row can hold, is 400 PRODUCTION_EVENT_INPUT naming it; 404 PROJECT_NOT_FOUND an unknown project.",
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
