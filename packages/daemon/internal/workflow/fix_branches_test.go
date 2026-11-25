package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// besideFix1 is role's branch beside fix 1 of LEGION-208's task a at generation 1.
func besideFix1(t *testing.T, role claim.Role) string {
	t.Helper()
	branch, err := claim.BesideBranch("LEGION-208", 1, "a", 1, role)
	if err != nil {
		t.Fatalf("name the %s's branch beside fix 1: %v", role, err)
	}
	return branch
}

// boundBeside binds the open start of step place of LEGION-208's task a to role's branch beside fix
// 1 (record.Store.SetStepPushBranch), and answers that branch.
func (r *fixRig) boundBeside(place int, role claim.Role) string {
	r.t.Helper()
	branch := besideFix1(r.t, role)
	onStored(r.t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		at := record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}
		start, err := r.engine.store.StepStarts(ctx, tx, issue.Key, at)
		if err != nil {
			return err
		}
		return r.engine.store.SetStepPushBranch(ctx, tx, issue.Key, at, start.Starts, branch)
	})
	return branch
}

// updatePushes is where each role of LEGION-208 pushes at an update round's ask now (UpdatePushes).
func (r *fixRig) updatePushes() map[claim.Role]supervise.UpdatePush {
	r.t.Helper()
	var pushes map[claim.Role]supervise.UpdatePush
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pushes, err = r.engine.UpdatePushes(ctx, tx, "LEGION-208")
		return err
	})
	return pushes
}

// assertPushes fails unless the update round's ask of LEGION-208 names want, what naming the moment.
func (r *fixRig) assertPushes(what string, want map[claim.Role]supervise.UpdatePush) {
	r.t.Helper()
	if got := r.updatePushes(); !reflect.DeepEqual(got, want) {
		r.t.Fatalf("the update round's pushes %s = %+v, want %+v", what, got, want)
	}
}

// An update round's ask names each step whose open start is bound to a branch beside a fix by the
// role of the step's phase: the tester's in testing, and the owner's acceptance under the
// architect, the role RoleFor answers accepting for. The implementer pushes nothing while the fix
// reads its job, and to its own branch while the fix runs. Before a step is bound, while the fix
// reads its job, that step gets the zero push, main's ask.
func TestAnUpdateRoundNamesEachBoundStepByTheRoleOfItsPhase(t *testing.T) {
	own := besideFix1(t, claim.RoleImplementer)
	t.Run("the tester in testing", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.assertPushes("while the fix reads its job", map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: {FixReads: true}})
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		tester := r.boundBeside(2, claim.RoleTester)
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleTester:      {Branch: tester},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})

	t.Run("the owner in accepting", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		r.approves()
		r.fails(fixHead, "k-ci")
		r.assertPushes("while the fix reads its job", map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: {FixReads: true}})
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		owner := r.boundBeside(3, claim.RoleArchitect)
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleArchitect:   {Branch: owner},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})
}
