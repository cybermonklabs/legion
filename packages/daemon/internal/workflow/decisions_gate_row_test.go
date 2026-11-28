package workflow

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The `decisions` step is gone, and the rows an earlier release wrote of it stay in the store until
// the root's pull request deletes them with the checks that still admit them. This release reads
// none of them. The store holds, beside the root's `criteria` and `spec` rows, a closed
// design_gates row of the `decisions` step, an issues.ready_pending_step reading `decisions` (with
// the version its shape check pairs it with), and the approval a re-admission kept of the
// `decisions` step. The gate opens on the two remaining steps, and the root starts its first phase.
// No gate write is refused: each registration publishes the changed set, and that set leaves the
// `decisions` row out, since a publication naming it is one the outbox refuses to queue; it says
// each of the two rows it names holds the tree, since the deployment gates both steps. The
// architect's start task names no approval of the `decisions` step. Afterwards each of the three
// stored values still stands, since this release deletes none of them.
func TestADecisionsGateRowIsIgnored(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepsEngine(twoSteps(), nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	if _, err := pool.Exec(t.Context(), `insert into design_gates (issue, step, artifact_id, latest_version)
		values ($1, 'decisions', 'artifact-decisions', 2)`, gatedRoot); err != nil {
		t.Fatalf("seed the decisions row of %s: %v", gatedRoot, err)
	}
	if _, err := pool.Exec(t.Context(), `update issues set ready_pending_version = 2, ready_pending_step = 'decisions'
		where key = $1`, gatedRoot); err != nil {
		t.Fatalf("seed the pending step of %s: %v", gatedRoot, err)
	}
	if _, err := pool.Exec(t.Context(), `insert into readmitted_approvals (issue, step, approved_version, latest_version)
		values ($1, 'decisions', 1, 2)`, gatedRoot); err != nil {
		t.Fatalf("seed the approval kept of the decisions step of %s: %v", gatedRoot, err)
	}
	bindStep(t, pool, record.DesignStepCriteria, criteriaDoc)

	reviewer := dispatch.Actor{Kind: "user", ID: "alice"}
	apply("register-criteria", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, Version: 2,
		ApprovedVersion: 2, ReviewedBy: reviewer})
	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
	if got := phaseOf(t, pool, gatedRoot); got != phase.Admitted {
		t.Fatalf("the root = %s with the spec step closed, want it admitted", got)
	}
	apply("approve-spec", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 7}))

	gates := gatesOf(t, pool, gatedRoot)
	if len(gates) != 2 {
		t.Fatalf("the gates read = %#v, want the criteria and spec rows alone", gates)
	}
	for _, step := range twoSteps() {
		if gate := gates[step]; gate.ApprovedVersion == nil || *gate.ApprovedVersion != gate.LatestVersion {
			t.Fatalf("the %s step = %#v, want it open", step, gate)
		}
	}
	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
		t.Fatalf("the root = %s with both remaining steps approved, want planning: the decisions row holds nothing", got)
	}

	var payload []byte
	if err := pool.QueryRow(t.Context(), `select payload from outbox where kind = 'design_gates_publish' and issue = $1`,
		gatedRoot).Scan(&payload); err != nil {
		t.Fatalf("read the design-gates publication of %s: %v", gatedRoot, err)
	}
	var published record.DesignGatesPublish
	if err := json.Unmarshal(payload, &published); err != nil {
		t.Fatalf("decode the design-gates publication of %s: %v", gatedRoot, err)
	}
	// The deployment gates both steps (twoSteps), so the steps the tree is gated on are that list
	// (record.TreeSteps) and each pair is published holding the tree.
	want := []record.DesignGateDocument{{Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, Holds: new(true)},
		{Step: record.DesignStepSpec, ArtifactID: gateDoc, Holds: new(true)}}
	if !reflect.DeepEqual(published.Gates, want) {
		wanted, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("encode the wanted set: %v", err)
		}
		t.Fatalf("the published payload = %s, want the gates %s: the decisions row is left out, and both rows hold the tree",
			payload, wanted)
	}

	seedRecord(t, pool, func(tx pgx.Tx) error {
		task, kept, err := engine.ParkedDecisionTask(t.Context(), tx, recordedRoot(t, pool))
		if err == nil && (task != "" || kept != nil) {
			t.Errorf("the start task = %q naming %#v, want nothing: the kept approval is of the decisions step", task, kept)
		}
		return err
	})

	var decisionsRows int
	if err := pool.QueryRow(t.Context(), `select count(*) from design_gates where issue = $1 and step = 'decisions'
		and artifact_id = 'artifact-decisions' and latest_version = 2 and approved_version is null`, gatedRoot).Scan(&decisionsRows); err != nil {
		t.Fatalf("count the decisions rows of %s: %v", gatedRoot, err)
	}
	if decisionsRows != 1 {
		t.Fatalf("%s holds %d decisions rows as seeded, want the one: nothing of this release deletes or writes it", gatedRoot, decisionsRows)
	}
	var pendingStep string
	var pendingVersion int
	if err := pool.QueryRow(t.Context(), `select ready_pending_step, ready_pending_version from issues where key = $1`,
		gatedRoot).Scan(&pendingStep, &pendingVersion); err != nil {
		t.Fatalf("read the pending step of %s: %v", gatedRoot, err)
	}
	if pendingStep != "decisions" || pendingVersion != 2 {
		t.Fatalf("the pending step of %s = %q at version %d, want 'decisions' at 2 as seeded", gatedRoot, pendingStep, pendingVersion)
	}
	var keptStep string
	if err := pool.QueryRow(t.Context(), `select step from readmitted_approvals where issue = $1`, gatedRoot).Scan(&keptStep); err != nil {
		t.Fatalf("read the approval kept for %s: %v", gatedRoot, err)
	}
	if keptStep != "decisions" {
		t.Fatalf("the approval kept for %s is of step %q, want 'decisions' as seeded", gatedRoot, keptStep)
	}
}
