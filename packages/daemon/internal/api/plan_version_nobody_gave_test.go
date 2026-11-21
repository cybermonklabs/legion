package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A version of the plan document that nobody gave Legion changes nothing. Dispatch holds the
// plan document of LEGION-208 at two versions: version 1, `implementer reviewer`, which its owner
// gives with register_plan, and version 2, `planner implementer reviewer`, which is the newest and
// which nobody gives. The route reads the document at the version given (planAt), and the
// daemon's own workflow takes that version: once the first step of LEGION-208 starts, it stands in
// implementing, the first step of version 1, and its plan is version 1's alone. A route that read
// the newest version would start planning, the first step of version 2.
func TestAVersionNobodyGaveChangesNothing(t *testing.T) {
	const root = "LEGION-208"
	h, engine, _ := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()})
	seedIssueAt(t, h, root, phase.Admitted)
	h.handoffs.documents = map[string]string{planDoc: root}
	h.handoffs.approvals = map[string]dispatch.Approval{planDoc: {State: "none", LatestVersion: 2}}
	h.handoffs.plans = map[string]dispatch.DocumentVersion{}
	h.handoffs.givePlan(planDoc, 1, taskBlock("a", "implementer reviewer"))
	h.handoffs.givePlan(planDoc, 2, taskBlock("a", "planner implementer reviewer"))
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)

	if given := registerPlan(h, architect.grant(t), root, planDoc, 1); given.Code != http.StatusOK {
		t.Fatalf("the owner's version 1 = %d %s, want 200", given.Code, given.Body)
	}
	// The first step of LEGION-208 starts, as the admission pass starts it once its gate opens.
	ctx, committed := intake.WithCommitHooks(context.Background())
	ctx = engine.OpenFact(ctx)
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(ctx, tx, root)
		if err != nil {
			return err
		}
		if err := engine.StartFirstPhase(ctx, tx, *issue, nil); err != nil {
			return err
		}
		return engine.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("start the first step of %s: %v", root, err)
	}
	committed()

	if got := recordedChild(t, h, root); got == nil || got.Phase != phase.Implementing {
		t.Fatalf("%s once its first step started = %#v, want implementing, the first step of version 1", root, got)
	}
	var versions []int
	var steps []string
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		stored, err := record.NewStore().Plan(context.Background(), tx, root, 1)
		if err != nil || stored == nil {
			return err
		}
		versions = append(versions, stored.Version.Version)
		for _, step := range stored.Steps["a"] {
			steps = append(steps, step.Role)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the plan of %s: %v", root, err)
	}
	if want := []int{1}; !reflect.DeepEqual(versions, want) {
		t.Fatalf("the plan in force of %s is version %v, want %v", root, versions, want)
	}
	if want := []string{"implementer", "reviewer"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("task a of %s walks %v, want version 1's %v", root, steps, want)
	}
}
