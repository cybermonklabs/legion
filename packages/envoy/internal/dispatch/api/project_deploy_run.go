// project_deploy_run.go serves GET /api/v1/projects/{key}/deploy-runs/{id}: the drill-down's run
// detail under the project whose timeline row opened it (dispatch://INFRA-2918, D14). A run id is
// one repository's, and a project's deploy sources read several, so the route names the repository
// the run is in (repo, which each deploy and failed_run row of the project timeline carries). It
// answers the run where that repository is the source of a delivery-on project and the run counts
// for the project by the rule the project timeline keeps its run rows by (evals.Attribution), with
// the run's record (dispatch://INFRA-978): its deploy row and every failure that blames it, which
// the drill-down's Flag, Mark restored and Reject write (R14).
package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// projectDeployRunView is GET /api/v1/projects/{key}/deploy-runs/{id}'s body: the run with every
// job it ran, as GET /api/v1/delivery/runs/{id} answers it, and the run's record. Deploy is the
// deploy row the GitHub adapter wrote for the run (prodevents.DeployForRun), of whichever project
// owns its repository, and null where it wrote none (a failed run, or one the repository's rule no
// longer calls a deploy). Failures is every failure that blames that row, of any project and in
// any state (prodevents.FailuresOf), oldest detection first, each with its signal, its state and
// that state's reason and writer, its restore and its note: the failures the measures count
// against the deploy, a rejected one kept with its reason. Empty where Deploy is null.
type projectDeployRunView struct {
	delivery.DeliveryRunDetailView
	Deploy   *prodevents.Event  `json:"deploy"`
	Failures []prodevents.Event `json:"failures"`
}

// getProjectDeployRun answers GET /api/v1/projects/{key}/deploy-runs/{id}?repo=<owner/name>: the
// run's jobs as GET /api/v1/delivery/runs/{id} answers them, read from repo, and its record
// (readProjectDeployRun). 400 INVALID_PROJECT for a key that is not a project key, 400
// RUN_ID_INPUT for an id that is not a positive integer, 400 INVALID_QUERY for a query naming no
// repo or naming it twice.
func (s *server) getProjectDeployRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := strings.TrimSpace(r.PathValue("key"))
	if !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
		return
	}
	runID, err := parseDeliveryRunID(r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	query := r.URL.Query()
	if err := namedOnce(query, "repo"); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := strings.TrimSpace(query.Get("repo"))
	if repo == "" {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "repo is required: the owner/name of the repository the run is in, as the project timeline's run rows name it")
		return
	}
	detail, err := s.readProjectDeployRun(r.Context(), project, repo, runID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, detail)
}

// readProjectDeployRun is run runID of repo with every job it ran (deliveryRunDetail) and its
// record (deployRunRecord), for project: the project found (loadProject, 404 PROJECT_NOT_FOUND for
// a project no row holds); repo the repository of a github_workflow source of some delivery-on
// project (delivery.DeployRules over every project's settings, each source's repository by
// delivery.SourceRepoOf), read under that source's rule; the run one of that repository
// (readDeliveryRun); and the run counting for project (deployRunCountsFor). Each of the last three
// that fails is 404 NOT_FOUND.
func (s *server) readProjectDeployRun(ctx context.Context, project, repo string, runID int64) (projectDeployRunView, error) {
	pool := s.deps.Store.Pool
	if _, err := s.loadProject(ctx, pool, project); err != nil {
		return projectDeployRunView{}, err
	}
	projects, err := delivery.ListProjectSettings(ctx, pool)
	if err != nil {
		return projectDeployRunView{}, err
	}
	rules := delivery.DeployRules(projects)
	index := slices.IndexFunc(rules, func(rule delivery.DeliverySettings) bool { return strings.EqualFold(rule.DeployRepo, repo) })
	if index < 0 {
		return projectDeployRunView{}, errorf(http.StatusNotFound, "NOT_FOUND", "%s is the deploy source of no project with delivery on", repo)
	}
	rule := rules[index]
	run, err := readDeliveryRun(ctx, pool, []string{rule.DeployRepo}, runID)
	if err != nil {
		return projectDeployRunView{}, err
	}
	counts, err := s.deployRunCountsFor(ctx, rule, run, project)
	if err != nil {
		return projectDeployRunView{}, err
	}
	if !counts {
		return projectDeployRunView{}, errorf(http.StatusNotFound, "NOT_FOUND",
			"run %d of %s is no deploy project %s counts, so no row of its timeline lists it", runID, rule.DeployRepo, project)
	}
	detail, err := deliveryRunDetail(ctx, pool, run)
	if err != nil {
		return projectDeployRunView{}, err
	}
	deploy, failures, err := deployRunRecord(ctx, pool, run)
	if err != nil {
		return projectDeployRunView{}, err
	}
	return projectDeployRunView{DeliveryRunDetailView: detail, Deploy: deploy, Failures: failures}, nil
}

// deployRunRecord is run's record: the deploy row the GitHub adapter wrote for it, nil where it
// wrote none or dropped it, and every failure that blames that row, of any project and in any
// state, oldest detection first. A failure is written under the project its writer names (a
// person's flag under the deploy row's project, a check's failure under its issue's), and the
// measures count each one against the deploy it blames whatever its project
// (evals' failure_days.go, prodevents.FailuresOf), so the drill-down lists them all.
func deployRunRecord(ctx context.Context, pool *store.Pool, run delivery.DeliveryRun) (*prodevents.Event, []prodevents.Event, error) {
	deploy, found, err := prodevents.DeployForRun(ctx, pool, run.Repo, run.RunID)
	if err != nil || !found {
		return nil, []prodevents.Event{}, err
	}
	failures, err := prodevents.FailuresOf(ctx, pool, []string{deploy.ID})
	if err != nil {
		return nil, nil, err
	}
	return &deploy, failures, nil
}

// deployRunCountsFor is whether run, a run of rule's repository, counts for project as the project
// timeline keeps its run rows (projectTimelinePopulation): a deploy run on main counts by the rule
// the eval registry's day rows count a deploy by (evals.Attribution.CountsDeploy), through the pull
// requests it ships first, or, shipping none, as the project whose deploy source reads its
// repository. What it ships first is delivery.Deploys.ShippedBy over the pull requests merged in a
// window holding the run's start alone (delivery.ListPullRequestsInWindow) and the runs
// delivery.ReadWindowRuns reads of rule's repository for it, which reads the deploys before it and
// the pull requests they leave to it as a wider window's read does. Any other run is no deploy of a
// window, which no row of a timeline lists, so it counts for no project.
func (s *server) deployRunCountsFor(ctx context.Context, rule delivery.DeliverySettings, run delivery.DeliveryRun, project string) (bool, error) {
	pool := s.deps.Store.Pool
	// Postgres keeps microseconds, so the window ends a microsecond past the start it holds.
	from, to := run.StartedAt, run.StartedAt.Add(time.Microsecond)
	prs, err := delivery.ListPullRequestsInWindow(ctx, pool, from, to)
	if err != nil {
		return false, fmt.Errorf("read the pull requests merged as run %d of %s started: %w", run.RunID, run.Repo, err)
	}
	applies, deploys, err := delivery.ReadWindowRuns(ctx, pool, rule, from, to, false, true)
	if err != nil {
		return false, fmt.Errorf("read what run %d of %s ships: %w", run.RunID, run.Repo, err)
	}
	if !slices.ContainsFunc(deploys.Started.Runs, func(started delivery.DeliveryRun) bool { return started.RunID == run.RunID }) {
		return false, nil
	}
	shipped := deploys.ShippedBy(delivery.MergedRows{PullRequests: prs, ApplyRuns: applies.Runs, ApplyJobs: applies.Jobs}, rule)[run.RunID]
	attribution, err := s.registry().ReadAttribution(ctx, nil, []evals.AttributedDeploy{{Repo: run.Repo, Shipped: shipped}}, []delivery.DeliverySettings{rule})
	if err != nil {
		return false, fmt.Errorf("read the projects run %d of %s counts for: %w", run.RunID, run.Repo, err)
	}
	return attribution.CountsDeploy(run.Repo, shipped, project), nil
}
