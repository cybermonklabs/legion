package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

var deployProgram = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// deployAhead is how far past now a deploy's deployed_at may be, and deployAheadRefusal the words
// a later one is refused in: by the route (createDeploy) and by the bar's rule for its call
// (goingDeployProblem), which refuses one as the route does.
const (
	deployAhead        = 5 * time.Minute
	deployAheadRefusal = "deployed_at must not be more than five minutes in the future"
)

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
		return "", errorf(http.StatusBadRequest, "DEPLOY_INPUT", "repo must be owner/repo")
	}
	return s.deps.Renames.Canonical(strings.ToLower(owner + "/" + repo)), nil
}

func (s *server) createDeploy(w http.ResponseWriter, r *http.Request) {
	// The caller is resolved before the body is read, so a request with no credential is refused
	// whatever it sends.
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input deployInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, "DEPLOY_INPUT", http.StatusBadRequest, err.Error())
		return
	}
	repo, err := s.deployRepository(input.Repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !deployProgram.MatchString(input.Program) || !resultCommitSHA.MatchString(input.Commit) {
		writeError(w, "DEPLOY_INPUT", http.StatusBadRequest, "program must be 1 to 64 lowercase letters, digits or hyphens, and commit must be 40 lowercase hexadecimal characters")
		return
	}
	now := time.Now().UTC()
	deployedAt := now
	if input.DeployedAt != nil {
		deployedAt = input.DeployedAt.UTC()
	}
	if deployedAt.After(now.Add(deployAhead)) {
		writeError(w, "DEPLOY_INPUT", http.StatusBadRequest, deployAheadRefusal)
		return
	}
	held, readErr := delivery.RepositoryHoldsCommit(r.Context(), s.deps.GitHub, repo, input.Commit)
	if readErr == nil && !held {
		writeError(w, "DEPLOY_COMMIT_UNKNOWN", http.StatusBadRequest, "the repository holds no such commit")
		return
	}
	// As refuseUnknownCommit does, refuse a proof of absence, not a read that failed.
	if readErr != nil {
		slog.Info("dispatch: GitHub could not prove a deploy's commit", "repo", repo, "commit", input.Commit, "error", readErr)
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		writeError(w, "DATABASE_UNAVAILABLE", http.StatusServiceUnavailable, "database unavailable")
		return
	}
	deploy, created, err := delivery.RecordDeploy(r.Context(), s.deps.Store.Pool, s.deps.Events, model.Deploy{
		Repo: repo, Program: input.Program, Commit: input.Commit, DeployedAt: deployedAt,
		Source: "route", URL: input.URL, RecordedBy: deployWriterOf(actor),
	})
	if err != nil {
		var late *delivery.DeployNotNewest
		if errors.As(err, &late) {
			writeError(w, "DEPLOY_NOT_NEWEST", http.StatusConflict, late.Error())
		} else {
			s.writeHandlerError(w, err)
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	WriteJSON(w, status, deployResponse{Deploy: deploy})
}

func (s *server) listDeploys(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	repo, err := s.deployRepository(r.URL.Query().Get("repo"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		writeError(w, "DATABASE_UNAVAILABLE", http.StatusServiceUnavailable, "database unavailable")
		return
	}
	deploys, err := delivery.CurrentDeploys(r.Context(), s.deps.Store.Pool, repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deploysResponse{Repo: repo, Deploys: deploys})
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
