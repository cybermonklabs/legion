package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The first start task of a next change names each reason the change carries, whatever its kind: a
// defect a role reported and a check Dispatch computes as failed, each by its id and kind, the check
// by its block id. The task names them as the change's other texts name a reason (reasonLines), so
// the change's work starts from what the change is for.
func TestANextChangesFirstTaskNamesEachReasonItCarries(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.seedDefect(waitRoot, "The page is blank after a save.")
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	var carried []record.Defect
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		open, err := record.NewStore().OpenDefects(t.Context(), tx, waitRoot)
		for _, reason := range open {
			if reason.CarriedBy == changeGeneration {
				carried = append(carried, reason)
			}
		}
		return err
	})
	kinds := map[record.DefectKind]bool{}
	for _, reason := range carried {
		kinds[reason.Kind] = true
	}
	if len(carried) != 2 || !kinds[record.DefectKindDefect] || !kinds[record.DefectKindCheck] {
		t.Fatalf("the reasons change %d of %s carries = %+v, want the reported defect and the check c-after", changeGeneration, waitRoot, carried)
	}

	firstStep(t, rig.pool, rig.engine, waitRoot)

	_, start, held := rig.workerStart(waitRoot, changeGeneration)
	if !held {
		t.Fatalf("%s holds no first start of a phase worker at generation %d", waitRoot, changeGeneration)
	}
	for place, line := range reasonLines(carried) {
		if !strings.Contains(start.Task, line) {
			t.Errorf("the first task of change %d of %s names no reason %d of kind %s (check %q): %q",
				changeGeneration, waitRoot, carried[place].ID, carried[place].Kind, carried[place].Check, line)
		}
	}
}
