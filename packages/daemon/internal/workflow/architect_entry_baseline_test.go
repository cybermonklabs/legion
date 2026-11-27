package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root's architect is handed its entry at admission (StartArchitect, EnterArchitect), before any
// version of the design is approved, and that entry is no working phase of the root: the root's
// holder is the worker of its first phase past admitted, whose work starts from what is approved
// when that phase starts (dispatch://INFRA-175 meeting dispatch://INFRA-2614). So the architect's
// entry writes no holder row for its root (enter), and the root's first phase takes its holder's
// baseline at the version approved then, which is news to nobody. Written at the architect's
// entry instead, the row would know no version, the first phase would leave it as it stands, and
// that phase's worker would be owed as news the version its own work started from.
func TestARootArchitectsEntryTakesNoBaselineForTheRootsHolder(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)

	seedRecord(t, pool, func(tx pgx.Tx) error {
		root, err := engine.store.Issue(t.Context(), tx, gatedRoot)
		if err != nil {
			return err
		}
		return engine.StartArchitect(t.Context(), tx, *root)
	})
	if row := phaseRowOf(t, pool, claim.RoleArchitect); row.Entry == 0 {
		t.Fatalf("the architect's row of %s = %+v, want the entry the start raised: the case's own premise", gatedRoot, row)
	}
	if got := toldVersions(t, pool, gatedRoot); len(got) != 0 {
		t.Fatalf("the told rows after the architect's entry = %v, want none: no work of the root has started to take a version", got)
	}

	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
	apply("approve-4", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 7}))
	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
		t.Fatalf("%s = %s after the opening's pass, want planning: its first working phase", gatedRoot, got)
	}

	want := map[string]map[record.DesignStep]int{
		gatedRoot + "/architect": {record.DesignStepSpec: 4},
		gatedRoot + "/holder":    {record.DesignStepSpec: 4},
	}
	if got := toldVersions(t, pool, gatedRoot); !reflect.DeepEqual(got, want) {
		t.Fatalf("the told rows once the root's first phase started = %v, want %v: its holder takes the version approved when its work started", got, want)
	}
	if news := owedNewsOf(t, pool, engine, gatedRoot); len(news.Members) != 0 || len(news.Architect) != 0 {
		t.Fatalf("the tree owes %+v, want nothing: the version approved when the work started is news to nobody", news)
	}
}
