package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/deploy-runs/{id}",
	Auth:        authAny,
	Description: "One run of a deploy source's repository with every job it ran, by start, each with its result and timings, as GET /api/v1/delivery/runs/{id} answers a run, for the project its path names: ?repo= (required) names the repository the run is in, as the project timeline's deploy and failed_run rows do, and must be the repository of a github_workflow deploy source of a project with delivery on; the run is answered where it counts for the project as the project timeline's rows are kept (a deploy run on main counts for the projects the pull requests it ships first count for, or, shipping none, for the project whose deploy source reads its repository). Beside its jobs it answers its record: deploy, the deploy row the GitHub adapter wrote for the run under the project that owns its repository (null where it wrote none), and failures, every failure of any project and in any state that blames that row, oldest detection first, each with its signal, its state with that state's reason and writer, its restore and its note. 400 INVALID_PROJECT for a key that is no project key, 400 RUN_ID_INPUT for an id that is not a positive integer, 400 INVALID_QUERY for no repo or repo named twice, 404 PROJECT_NOT_FOUND, 404 NOT_FOUND for a repository no delivery-on project's source reads, no such run of it, or a run that counts for no project with this key.",
	Handler:     (*server).getProjectDeployRun,
	ID: &idRoute{
		Reason: "a run id of the repository the query's repo names, a positive integer and no uuid at all",
	},
	Read: readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: projectDeployRunView{}, Params: projectDeployRunQuery{},
	},
})
