package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/settings/delivery",
	Auth:        authAny,
	Description: "The project's delivery settings, or null when it has none: delivery (on or off), its deploy sources in order (each github_workflow, with an optional repo, null for the project's default repository, its deploy workflow, production jobs, optional production apply step, optional staging end-to-end and staging deploy jobs, pull-request checks workflow and optional roll-up job, or events, the project's posted deploys), each with its id, its resolved_repo (the repository it reads, null where it names none and the project has no default) and reconciled_through (how far the reconcile has imported that repository's deploy runs, pull-request check runs and merged pull requests; null for an events source, a project with delivery off, and a repository whose import has not finished), the population authors and excluded repositories, the failure sources with failures_measured_since (YYYY-MM-DD), and the targets of deploy-frequency, change-fail-rate, merge-to-production, pr-open-to-merge, deploy-run-success, failed-deploy-recovery and unowned-p0, each a number or \"off\", read from the project's eval rows. Every reader of the project may read them. 404 PROJECT_NOT_FOUND for a project that does not exist.",
	Handler:     (*server).getProjectDeliverySettings,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: projectDeliverySettings{},
		// A project with no delivery settings answers 200 and the JSON literal `null`
		// (project_settings_delivery.go).
		Nullable: true,
	},
})
