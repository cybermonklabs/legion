package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/results",
	Auth:        authAny,
	Description: "Record one result of one check of the spec files, written by the CI job that ran it {commit, check, verdict, observed}: only a results token, which the secrets broker mints for one GitHub Actions run, writes it (403 RESULTS_SCOPE_REQUIRED for every other credential), and any other body member is 400 RESULT_INPUT naming it. Dispatch reads the rest from the token, from GitHub and from .dispatch/spec at the run's commit: commit must be the run's (403 COMMIT_NOT_THE_RUNS), the run's repository must be the project's default repository, in any case and under the name the repository has now (403 REPOSITORY_NOT_THE_PROJECTS), GitHub must name the run's check run at that commit (403 CHECK_RUN_NOT_THE_RUNS; 403 CHECK_RUN_NOT_SHOWN when GitHub answers 403 or 404 for it), the graph logic must take the files as spec-check does (409 SPEC_FILES_REFUSED naming the first problem), and the files must hold the check (409 CHECK_NOT_IN_FILES) with its job the check run's job (409 CHECK_NOT_THIS_JOBS): a check run is the job's when its name is the job's name, with no leg, or when its name is the job's name and a leg in parentheses and the run's workflow file at the commit makes the job a matrix job (409 WORKFLOW_NOT_SHOWN when GitHub answers 403 or 404 for that file); 503 CHECK_RUN_UNREADABLE, FILES_UNREADABLE or WORKFLOW_UNREADABLE when GitHub does not answer. A result keeps 2000 characters of observed: a longer text is its first 500 characters, a line that says the middle was cut, and its last characters. Answers 201 with the stored row {id, seq, kind, attestation}, an in-toto statement of the run whose recorder is the run's session and principal, and no person. Each result is a new row, and none is changed.",
	Handler:     (*server).recordSpecCheckResult,
	// A CI job's result of a check of the spec files writes the results of the project its path
	// names, which must be the one project its results token names.
	WriteClass:   scopeProject,
	ResultsScope: true,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.SpecCheckResult{}, Request: specCheckResultInput{},
	},
})
