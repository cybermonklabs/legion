package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// workflowRouteHarness serves the daemon's routes with a workflow engine whose project offers
// offered, over LEGION-208's tree: the root standing at admitted, LEGION-209 at implementing,
// LEGION-210 at admitted and LEGION-211 held from implementing. It answers the harness and the
// root architect's live claim.
func workflowRouteHarness(t *testing.T, offered ...string) (*harness, liveClaim) {
	t.Helper()
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	h, _, _ := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Workflows: offered, Clock: func() time.Time { return now }})
	parent := "LEGION-208"
	issue := func(key string, at phase.Phase, hold *record.Hold) record.Issue {
		var parentOf *string
		if key != parent {
			parentOf = &parent
		}
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent, Project: testProject,
			Title: key, Parent: parentOf, Phase: at, Hold: hold, Generation: 1, Status: "in_progress"}
	}
	h.recordIssue(issue("LEGION-208", phase.Admitted, nil))
	h.recordIssue(issue("LEGION-209", phase.Implementing, nil))
	h.recordIssue(issue("LEGION-210", phase.Admitted, nil))
	h.recordIssue(issue("LEGION-211", phase.Held, &record.Hold{From: phase.Implementing}))
	return h, newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
}

// setWorkflowAs is architect's `set_workflow` of issue, as the `legion` tool sends it.
func setWorkflowAs(t *testing.T, h *harness, architect liveClaim, issue, name, reason string) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(http.MethodPost, "/legion/v1/workflow", map[string]any{
		"grantId": architect.grant(t), "issue": issue, "workflow": name, "reason": reason,
	}, nil)
}

// recordedWorkflow is the workflow and its reason issue's record names.
func recordedWorkflow(t *testing.T, h *harness, issue string) (string, string) {
	t.Helper()
	var name, reason string
	if err := h.store.Tx(h.ctx, func(tx pgx.Tx) error {
		recorded, err := record.NewStore().Issue(h.ctx, tx, issue)
		if err != nil || recorded == nil {
			return err
		}
		name, reason = recorded.Workflow, recorded.WorkflowReason
		return nil
	}); err != nil {
		t.Fatalf("read the record of %s: %v", issue, err)
	}
	return name, reason
}

// The owning architect chooses an issue's workflow, with a reason, while no phase of the issue has
// started (dispatch://INFRA-3570), and the issue's record then names both. A name the daemon's
// library does not hold is refused 400 WORKFLOW_UNKNOWN before any fact, naming the names it
// holds; a choice without a reason is refused as every write missing a field is; a name the
// project does not offer is refused 409 WORKFLOW_NOT_OFFERED, naming what it offers; and an issue
// whose first phase has started — one in implementing, and one held from implementing, whose hold
// a release takes back into that phase — is refused 409 WORKFLOW_AFTER_START, naming where it
// stands. No refusal changes the record.
func TestSetWorkflowIsTheOwningArchitectsBeforeTheFirstPhase(t *testing.T) {
	h, architect := workflowRouteHarness(t, "light", phase.DefaultSequenceName)

	refusedSaying(t, setWorkflowAs(t, h, architect, "LEGION-210", "heavy", "a small change"), http.StatusBadRequest, "WORKFLOW_UNKNOWN",
		"default, light, no-planner")
	assertFailure(t, setWorkflowAs(t, h, architect, "LEGION-210", "light", "  "), http.StatusBadRequest, "MISSING_FIELD")
	refusedSaying(t, setWorkflowAs(t, h, architect, "LEGION-210", "no-planner", "the plan is written"), http.StatusConflict, "WORKFLOW_NOT_OFFERED",
		"it offers light, default")
	for issue, standing := range map[string]string{"LEGION-209": "implementing", "LEGION-211": "held (held from implementing)"} {
		refusedSaying(t, setWorkflowAs(t, h, architect, issue, "light", "a small change"), http.StatusConflict, "WORKFLOW_AFTER_START",
			issue+" stands in "+standing)
	}
	for _, issue := range []string{"LEGION-209", "LEGION-210", "LEGION-211"} {
		if name, reason := recordedWorkflow(t, h, issue); name != "" || reason != "" {
			t.Fatalf("%s names workflow %q with reason %q after refusals alone, want neither", issue, name, reason)
		}
	}

	accepted := setWorkflowAs(t, h, architect, "LEGION-210", "light", "a one-line fix; nothing to plan, test or accept")
	if accepted.Code != http.StatusOK || strings.TrimSpace(accepted.Body.String()) != "{}" {
		t.Fatalf("set_workflow of LEGION-210 = %d %s, want 200 and the empty body", accepted.Code, accepted.Body)
	}
	if name, reason := recordedWorkflow(t, h, "LEGION-210"); name != "light" || reason != "a one-line fix; nothing to plan, test or accept" {
		t.Fatalf("LEGION-210 names workflow %q with reason %q, want light and the architect's reason", name, reason)
	}
	// The root, at admitted too, takes the default's own name, which the project offers.
	if got := setWorkflowAs(t, h, architect, "LEGION-208", phase.DefaultSequenceName, "the whole line"); got.Code != http.StatusOK {
		t.Fatalf("set_workflow of LEGION-208 = %d %s, want 200", got.Code, got.Body)
	}
}

// A project that offers no workflow refuses every choice, the default's own name included, and
// says it offers none: that is every project until an operator lists the workflows its
// architects may choose, so this release changes nothing for a project nobody opted in.
func TestAProjectThatOffersNoWorkflowRefusesEveryChoice(t *testing.T) {
	h, architect := workflowRouteHarness(t)
	for _, name := range phase.SequenceNames() {
		refusedSaying(t, setWorkflowAs(t, h, architect, "LEGION-210", name, "a small change"), http.StatusConflict, "WORKFLOW_NOT_OFFERED",
			"it offers none")
	}
	if name, reason := recordedWorkflow(t, h, "LEGION-210"); name != "" || reason != "" {
		t.Fatalf("LEGION-210 names workflow %q with reason %q, want neither", name, reason)
	}
}

// set_workflow is an architect operation like sign_off: a phase worker's grant is refused, an
// architect of another tree is refused, and a sub-architect may choose for its own issue but not
// for its sibling's.
func TestSetWorkflowRefusesWhoeverDoesNotOwnTheIssue(t *testing.T) {
	h, _ := workflowRouteHarness(t, "light")
	worker := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleImplementer)
	assertFailure(t, setWorkflowAs(t, h, worker, "LEGION-210", "light", "a small change"), http.StatusForbidden, "ARCHITECT_REQUIRED")
	h.recordIssue(record.Issue{Key: "LEGION-999", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-999",
		Project: testProject, Title: "LEGION-999", Phase: phase.Admitted, Generation: 1, Status: "in_progress"})
	foreign := newLiveClaim(t, h, "LEGION-999", claim.RoleArchitect)
	assertFailure(t, setWorkflowAs(t, h, foreign, "LEGION-210", "light", "a small change"), http.StatusForbidden, "ISSUE_OUTSIDE_TREE")
	sub := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	refusedSaying(t, setWorkflowAs(t, h, sub, "LEGION-210", "light", "a small change"), http.StatusForbidden, "ISSUE_NOT_OWNED",
		"set_workflow of LEGION-210")
	if name, _ := recordedWorkflow(t, h, "LEGION-210"); name != "" {
		t.Fatalf("LEGION-210 names workflow %q after refusals alone, want none", name)
	}
}

// refusedSaying asserts recorder is the refusal of status and code, and that its sentence says
// says. The body is read before assertFailure, which decodes it and so empties the recorder.
func refusedSaying(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, says string) {
	t.Helper()
	body := recorder.Body.String()
	assertFailure(t, recorder, status, code)
	if !strings.Contains(body, says) {
		t.Errorf("%s = %s, want its sentence to say %q", code, body, says)
	}
}

// workflowContract is the contract whose entry in version.go added `POST /legion/v1/workflow` and
// the state's `workflow` and `workflowReason`, and this daemon's own contract is never below it:
// a caller at this contract is served the two keys.
func TestTheWorkflowContractIsTheContractThatAddedTheKeys(t *testing.T) {
	// The literal pins the number the version.go entry that added the keys declares: a renumber
	// moves it with the other sites, and the test fails while workflowContract says another.
	const addedWorkflow = 145
	if workflowContract != addedWorkflow {
		t.Fatalf("workflowContract = %d, want %d, the contract whose entry in version.go added each issue's workflow", workflowContract, addedWorkflow)
	}
	if workflowContract > DaemonAPIVersion {
		t.Fatalf("workflowContract = %d, above DaemonAPIVersion %d: no caller could be served the keys", workflowContract, DaemonAPIVersion)
	}
}
