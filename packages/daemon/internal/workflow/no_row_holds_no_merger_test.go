package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A gated step that nobody registered holds no merger (R9 of dispatch://INFRA-2040). The
// deployment gates the Verification document and the spec, the architect registers the spec, and
// nobody registers the criteria step. The registration closes the standing gate, which holds the
// merger on the spec's row, and the spec's approval opens it again: the opening message calls open
// every gated step that someone registered, and the hold agrees with the message. The criteria
// step still holds the tree's work (record.TreeGate.Open), but it names no document, so it holds
// no merger and no message names it. The merger's live session is told the closing (R8); the idle
// release then takes its pod during the hold, so the approval writes the gate's own start of the
// merger beside the opening message it sends the root's architect.
func TestNoRowHoldsNoMergerAndTheSpecsApprovalSendsTheOpeningMessage(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	seedTreeLifecycle(t, pool, gatedRoot)
	engine := stepsEngine(twoSteps(), nil)

	applyMirrored(t, pool, engine, "register-spec-1", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 1, DispatchVersion: 1, Since: record.EventMark{Seq: 11}})
	if held, holds := treeMergerHold(t, pool, engine); !holds || held.Step != record.DesignStepSpec || held.ArtifactID != "artifact-208" ||
		held.LatestVersion != 1 {
		t.Fatalf("the hold after the spec's registration = %+v, %t; want the spec's row at version 1, with its reviewer", held, holds)
	}
	if _, err := pool.Exec(t.Context(), `update claims set state = 'suspended', suspended_by = 'released' where token = $1`,
		string(mergerToken(t, gatedRoot))); err != nil {
		t.Fatalf("release the merger's pod: %v", err)
	}
	applyMirrored(t, pool, engine, "spec-approved-1", stepApproval(gatedRoot, "artifact-208", 1, record.EventMark{Seq: 12}))

	closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 1, false)
	opened := gateMovedBody(record.DesignStepSpec, "artifact-208", 1, true)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: mergerClosedNote(gatedRoot, record.DesignStepSpec, "artifact-208", 1)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: closed},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: opened},
	})
	if !strings.Contains(opened, "every gated step that someone registered now stands open") {
		t.Fatalf("the opening message = %q, want it to call open every gated step that someone registered", opened)
	}
	if starts := mergerStartsOf(t, pool, gatedRoot); len(starts) != 1 || !starts[0].request.GateMessage || starts[0].request.Phase != "" ||
		starts[0].request.Task != gateMovedTask(gatedRoot) {
		t.Fatalf("the merger's starts after the spec's approval = %+v, want the gate's one opening start", starts)
	}
	if held, holds := treeMergerHold(t, pool, engine); holds {
		t.Fatalf("the hold after the spec's approval = %+v, want none: the criteria step has no row", held)
	}
	if gate := treeGateOf(t, pool, engine); gate.Open() {
		t.Fatalf("the tree's gate = %+v reads open, want the criteria step, which has no row, to hold the tree's work", gate.Rows)
	}
}

// treeMergerHold is the hold of the merger of the gated root, in merging, as the record holds it
// now (HeldByDesignGate).
func treeMergerHold(t *testing.T, pool *pgxpool.Pool, engine *Engine) (record.DesignGate, bool) {
	t.Helper()
	return HeldByDesignGate(storedIssue(t, pool, gatedRoot), claim.RoleMerger, treeGateOf(t, pool, engine))
}

// treeGateOf is the gated root's design gate as the record holds it, under the steps engine's
// deployment gates.
func treeGateOf(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.TreeGate {
	t.Helper()
	var rows map[record.DesignStep]record.DesignGate
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		rows, err = record.NewStore().GatesOf(t.Context(), tx, gatedRoot)
		return err
	}); err != nil {
		t.Fatalf("read the design gate of %s: %v", gatedRoot, err)
	}
	return record.TreeGate{Steps: engine.cfg.Steps, Rows: rows}
}
