package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// SpawnRequest is the operator's spawn: the claim on Role of Issue, in the tree Tree roots.
// Prompt is an optional test override; without it the daemon composes the role prompt itself.
// Task, when set, is its first delivery, queued before the agent is ready and sent once it is.
type SpawnRequest struct {
	Tree   string     `json:"tree"`
	Issue  string     `json:"issue"`
	Role   claim.Role `json:"role"`
	Prompt string     `json:"prompt,omitempty"`
	Task   string     `json:"task,omitempty"`
}

// refuseMovedSpawn answers a spawn whose launch of token, the claim it read as the worker on
// role's step of issue, found the step another claim's under the machine's lock
// (supervise.ErrStepLeft): nothing was launched or sent, and the answer names the claim the record
// names there now, read again here, so the operator can ask again of the step as it stands.
func (s *server) refuseMovedSpawn(w http.ResponseWriter, r *http.Request, issue string, role claim.Role, token claim.Token) {
	now, err := s.workerToken(r.Context(), issue, role)
	if err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	s.refuse(w, r, http.StatusConflict, fmt.Sprintf(
		"spawn refused: the %s's step of %s moved from claim %s to claim %s under the request; nothing was launched or sent, so ask again",
		role, issue, token, now))
}

// spawn creates the claim — or finds it, when it exists — and posts the launch to its machine,
// then the task, so a claim the machine will not launch is given no task either. While the project
// is paused it creates and launches nothing (refusedWhilePaused). The claim of the request's issue
// and role is the claim of the agent that works that role's step now (workerToken), a keyed agent
// on a step of another role included.
//
// That claim is read before any lock the launch takes, so the launch and the task are asked with
// its standing (worksStep, supervise.Standing): the launch judges again under the machine's lock
// that the record still names the claim on the role's step. A step that moved to another claim
// before it is refused with nothing launched and no task given (refuseMovedSpawn), and a claim this
// request stored for it is retired as the outbox retires a claim it stored for an issue that left
// the workflow, so no tree's cleanup waits on a claim nothing will run. A move after the launch
// finds the request under way, and its task goes as it would have before the move.
func (s *server) spawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if !s.readBody(w, r, &req, nil) || !s.requireFields(w, r,
		field{"tree", req.Tree}, field{"issue", req.Issue}, field{"role", string(req.Role)},
	) {
		return
	}
	for _, key := range []field{{"tree", req.Tree}, {"issue", req.Issue}} {
		if !claim.IsIssueKey(key.value) {
			s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("%s %q is not an issue key", key.name, key.value))
			return
		}
	}
	if !claim.IsRole(req.Role) {
		s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("role %q is not a role", req.Role))
		return
	}
	role := string(req.Role)
	if !claim.IsBuiltin(req.Role) && (plan.RunsAs(role) != phase.Step || !s.deployment.Defines(role)) {
		s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("role %q is not a spawnable role; deployment-defined roles: %s",
			req.Role, s.deploymentRoleNames()))
		return
	}
	token, err := s.workerToken(r.Context(), req.Issue, req.Role)
	if err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, r, "spawn") {
		return
	}
	c := supervise.Claim{
		Token: token, Project: s.project, Tree: req.Tree, Issue: req.Issue, Role: req.Role, State: supervise.StateQueued,
	}
	if s.refusedWhilePaused(w, r, "spawn", c) {
		return
	}
	if req.Tree == req.Issue && req.Role == claim.RoleArchitect {
		if _, err := s.trees.OpenOperatorTreeLifecycle(ctx, s.project, req.Tree, s.drawExperiments(req.Tree)); errors.Is(err, treelifecycle.ErrCleanupReserved) {
			s.refuse(w, r, http.StatusConflict, fmt.Sprintf("operator tree %s is waiting for durable cleanup: %v", req.Tree, err))
			return
		} else if err != nil {
			s.operatorFailure(w, r, "spawn", token, err)
			return
		}
	}
	m, created, err := s.supervisor.Create(ctx, c, req.Prompt)
	if err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	// The operator is waiting on this answer, so the wake pacing never holds this launch
	// (supervise's reserveWake): one person's spawn is not the wave the pacing exists for, and a
	// route that answered with a claim parked launching would read as a daemon that did nothing.
	judged := supervise.Standing(ctx, s.worksStep(req.Issue, req.Role, token))
	if err := m.Handle(judged, supervise.RequestSpawn{Claim: token, Exempt: true}); err != nil {
		if errors.Is(err, supervise.ErrStepLeft) {
			if created {
				if err := m.Handle(ctx, supervise.RequestTreeClose{Claim: token}); err != nil {
					s.logFailure("api: retire a claim the spawn stored for a step that moved", "claim", token, "error", err)
				}
			}
			s.refuseMovedSpawn(w, r, req.Issue, req.Role, token)
			return
		}
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	if req.Task != "" {
		if err := m.Handle(judged, supervise.RequestDeliver{Claim: token, Task: req.Task}); err != nil {
			s.operatorFailure(w, r, "deliver", token, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.operatorDone(r, token)
	s.writeUnparsedAnswer(w, r, status, s.operatorAnswer(ctx, m.Claim()))
}

// deploymentRoleNames lists the spawnable roles configured in addition to Legion's six.
func (s *server) deploymentRoleNames() string {
	var names []string
	for role := range s.deployment.Roles {
		// The plan's built-in steps, including owner acceptance, are not deployment roles.
		if plan.RunsAs(role) == phase.Step && !claim.IsBuiltin(claim.Role(role)) {
			names = append(names, role)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// drawExperiments draws the tree-unit experiments the project declares for tree, for the open
// that creates the tree's lifecycle row to run in its own transaction
// (store.Store.OpenOperatorTreeLifecycle, experiment.DrawAndRecord): one answer per experiment,
// under the mode the project declares it at and at the instant this server's clock reads,
// recorded against the tree's root issue key (record.Store.PutAssignments, which admission's own
// path draws through too). An operator's tree usually has no issue record at all, and then the
// draw writes nothing — nothing is refused and no record is invented.
func (s *server) drawExperiments(tree string) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		return experiment.DrawAndRecord(ctx, tx, s.records, tree, s.experiments, config.ExperimentTree, s.source, s.now())
	}
}
