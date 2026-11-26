package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

var deployProgram = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

type deployInput struct {
	Repo       string     `json:"repo" required:"true"`
	Program    string     `json:"program" required:"true"`
	Commit     string     `json:"commit" required:"true"`
	DeployedAt *time.Time `json:"deployed_at"`
	URL        string     `json:"url"`
}
type deployResponse struct {
	Deploy model.Deploy `json:"deploy"`
}
type deploysResponse struct {
	Repo    string         `json:"repo"`
	Deploys []model.Deploy `json:"deploys"`
}

type deploysQuery struct {
	Repo string `query:"repo" required:"true"`
}

// deploysV2Query is GET /api/v2/deploys's query: the repository, as GET /api/v1/deploys reads it,
// and one page of its programs' current deploys.
type deploysV2Query struct {
	pageQuery
	// Repo is the repository the deploys are of, owner/repo; any other value, the empty one
	// among them, is 400 DEPLOY_INPUT.
	Repo string `query:"repo"`
}

// deployWriterOf records the credential's resolved identity, without accepting a body-selected
// writer or imposing a second caller list after the deployment-wide write scope.
func deployWriterOf(actor model.Actor) model.Actor {
	if actor.Kind == "session" && actor.ID == "" && actor.Owner != nil && actor.Credential != nil {
		credential := actor.Credential
		actor.Kind, actor.ID, actor.Owner = "user", *actor.Owner, nil
		actor.Proven = true
		actor.Act = &model.ActorAct{TokenID: credential.TokenID, TokenName: credential.TokenName, Scopes: credential.Scopes}
	}
	return actor.Recorded()
}

func (s *server) deployRepository(value string) (string, error) {
	owner, repo, err := refs.ParseGitHubRepo(value)
	if err != nil {
		return "", errorf(apicode.DeployInput, "repo must be owner/repo")
	}
	return s.deps.Renames.Canonical(strings.ToLower(owner + "/" + repo)), nil
}

// createDeploy is POST /api/v1/deploys: the record recordDeploy makes of the body, answered
// {deploy}.
func (s *server) createDeploy(w http.ResponseWriter, r *http.Request) {
	// The caller is resolved before the body is read, so a request with no credential is refused
	// whatever it sends.
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	var input deployInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, apicode.DeployInput, err.Error())
		return
	}
	deploy, created, err := s.recordDeploy(r.Context(), actor, input)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, deployStatus(created), deployResponse{Deploy: deploy})
}

// createDeployV2 is POST /api/v2/deploys: the record POST /api/v1/deploys makes (recordDeploy) of
// the body the row's decoder read, answered as the deploy itself.
func (s *server) createDeployV2(w http.ResponseWriter, r *http.Request) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	deploy, created, err := s.recordDeploy(r.Context(), actor, v2Body[deployInput](r))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, deployStatus(created), deploy)
}

// deployStatus is the status a deploy's record answers: 201 for a new deploy, 200 for the
// program's current build named again.
func deployStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

// recordDeploy records input as a program's deployed build, writer the credential's own identity
// (deployWriterOf), and answers the deploy and whether it is new. Both versions of POST deploys
// make it. It refuses 400 DEPLOY_INPUT for a repository, program, commit or deployed_at it does not
// take, 400 DEPLOY_COMMIT_UNKNOWN where GitHub proves the repository holds no such commit, and 409
// DEPLOY_NOT_NEWEST for a deploy older than the program's current one.
func (s *server) recordDeploy(ctx context.Context, actor model.Actor, input deployInput) (model.Deploy, bool, error) {
	repo, err := s.deployRepository(input.Repo)
	if err != nil {
		return model.Deploy{}, false, err
	}
	if !deployProgram.MatchString(input.Program) || !resultCommitSHA.MatchString(input.Commit) {
		return model.Deploy{}, false, errorf(apicode.DeployInput, "program must be 1 to 64 lowercase letters, digits or hyphens, and commit must be 40 lowercase hexadecimal characters")
	}
	now := time.Now().UTC()
	deployedAt := now
	if input.DeployedAt != nil {
		deployedAt = input.DeployedAt.UTC()
	}
	if deployedAt.After(now.Add(5 * time.Minute)) {
		return model.Deploy{}, false, errorf(apicode.DeployInput, "deployed_at must not be more than five minutes in the future")
	}
	held, readErr := delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, repo, input.Commit)
	if readErr == nil && !held {
		return model.Deploy{}, false, errorf(apicode.DeployCommitUnknown, "the repository holds no such commit")
	}
	// As refuseUnknownCommit does, refuse a proof of absence, not a read that failed.
	if readErr != nil {
		slog.Info("dispatch: GitHub could not prove a deploy's commit", "repo", repo, "commit", input.Commit, "error", readErr)
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return model.Deploy{}, false, errorf(apicode.DatabaseUnavailable, "database unavailable")
	}
	deploy, created, err := delivery.RecordDeploy(ctx, s.deps.Store.Pool, s.deps.Events, model.Deploy{
		Repo: repo, Program: input.Program, Commit: input.Commit, DeployedAt: deployedAt,
		Source: "route", URL: input.URL, RecordedBy: deployWriterOf(actor),
	})
	if late := (*delivery.DeployNotNewest)(nil); errors.As(err, &late) {
		return model.Deploy{}, false, errorf(apicode.DeployNotNewest, "%s", late.Error())
	}
	return deploy, created, err
}

// listDeploys is GET /api/v1/deploys: every program's current deploy in the repository ?repo=
// names (currentDeploysIn), answered {repo, deploys}.
func (s *server) listDeploys(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	repo, deploys, err := s.currentDeploysIn(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, deploysResponse{Repo: repo, Deploys: deploys})
}

// listDeploysV2 is GET /api/v2/deploys: one page of the current deploys GET /api/v1/deploys lists
// whole (currentDeploysIn), each naming its repository.
func (s *server) listDeploysV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[deploysV2Query](r)
	_, deploys, err := s.currentDeploysIn(r.Context(), query.Repo)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(deploys, query.pageQuery))
}

// currentDeploysIn answers the repository value names, as Dispatch stores it (deployRepository),
// and each of its programs' current deploy, ordered by program.
func (s *server) currentDeploysIn(ctx context.Context, value string) (string, []model.Deploy, error) {
	repo, err := s.deployRepository(value)
	if err != nil {
		return "", nil, err
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return "", nil, errorf(apicode.DatabaseUnavailable, "database unavailable")
	}
	deploys, err := delivery.CurrentDeploys(ctx, s.deps.Store.Pool, repo)
	return repo, deploys, err
}

type currentDeploysAnswer struct {
	deploys []model.Deploy
	err     error
}

func programOf(check model.CheckBlock, deploys []model.Deploy) (string, string) {
	if check.Program != "" {
		return check.Program, ""
	}
	switch len(deploys) {
	case 0:
		return "", "no_deploy"
	case 1:
		return deploys[0].Program, ""
	default:
		return "", "program_not_named"
	}
}

func (c *comparisonReader) withLiveDeploy(ctx context.Context, check model.CheckBlock, compared comparison) comparison {
	if (check.Subject != "production" && check.Subject != "graph") ||
		(check.When != checkWhenAfterMerge && check.When != checkWhenDefaultBranch) {
		return compared
	}
	compared.live = true
	repo, answered := c.repositoryOf(ctx, check.IssueKey)
	if !answered || repo == nil {
		compared.deployReason = "read_failed"
		return compared
	}
	if c.deploys == nil {
		c.deploys = map[string]currentDeploysAnswer{}
	}
	answer, held := c.deploys[*repo]
	if !held {
		answer.deploys, answer.err = delivery.CurrentDeploys(ctx, c.server.deps.Store.Pool, *repo)
		c.deploys[*repo] = answer
	}
	if answer.err != nil {
		compared.deployReason = "read_failed"
		return compared
	}
	compared.program, compared.deployReason = programOf(check, answer.deploys)
	if compared.deployReason != "" {
		return compared
	}
	for _, deploy := range answer.deploys {
		if deploy.Program == compared.program {
			compared.deploy = &model.CheckDeploy{Program: deploy.Program, Commit: deploy.Commit, DeployedAt: deploy.DeployedAt}
			compared.deployHolds = c.commitStands(ctx, check, compared, model.CheckResult{CommitSHA: deploy.Commit, CheckIssueKey: check.IssueKey})
			return compared
		}
	}
	compared.deployReason = "no_deploy"
	return compared
}

// markNotCounted bounds a live reading to one build and one observation interval. A named
// historical read never sets comparison.live and therefore never reads today's deploy.
func markNotCounted(check *model.CheckBlock, compared comparison) {
	if !compared.live || compared.deploy == nil {
		return
	}
	for index := range check.Results {
		result := &check.Results[index]
		switch {
		case result.CommitSHA != compared.deploy.Commit:
			result.NotCounted = "it names a commit that the current deploy of the program this check reads does not run"
		case result.CreatedAt.Before(compared.deploy.DeployedAt):
			result.NotCounted = "it was recorded before the current deploy of the program this check reads"
		}
	}
}
