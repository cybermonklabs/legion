package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
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

// beside is the branch of each fix-started row of LEGION-208 by the role it is to, "" for a row
// that names none.
func (r *fixRig) beside() map[claim.Role]string {
	r.t.Helper()
	branches := map[claim.Role]string{}
	for _, n := range r.newsOf() {
		if n.Purpose == record.CINewsFixStarted {
			branches[roleOf(r.t, n.Role)] = n.Branch
		}
	}
	return branches
}

// A step beside a fix is told to push to its own branch wherever it is told anything. Fix 1 starts
// while the tester runs: the tester's fix-started row names its own branch beside fix 1, which is
// no branch of the issue (claim.IsBesideBranch); a tester that starts during the fix holds fix 1
// and that branch in its start facts; and the update round's ask names that branch for the tester,
// F with OwnFix for the implementer and no branch for the reviewer. While the fix reads its job, the
// ask names FixReads and no branch for the implementer. The owner in accepting gets its own branch,
// under the architect, in both its notice and the ask. The control: an issue with no fix finds no
// branch beside a fix in any row, start or answer.
func TestAStepBesideAFixIsToldToPushToItsOwnBranch(t *testing.T) {
	own := besideFix1(t, claim.RoleImplementer)
	t.Run("a tester that runs beside fix 1", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.assertPushes("while the fix reads its job", map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: {FixReads: true}})
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		tester := besideFix1(t, claim.RoleTester)
		if told := r.beside(); !claim.IsBesideBranch(told[claim.RoleTester]) || !reflect.DeepEqual(told, map[claim.Role]string{claim.RoleTester: tester}) {
			t.Fatalf("the fix-started rows' branches = %v, want the tester's own branch %s alone", told, tester)
		}
		if facts := r.startFactsIn(phase.Testing); facts.Fix != 1 || facts.Branch != tester {
			t.Fatalf("a tester's start facts during the fix = %+v, want fix 1 on %s", facts, tester)
		}
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleTester:      {Branch: tester},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})

	t.Run("the owner in accepting", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		r.approves()
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		owner := besideFix1(t, claim.RoleArchitect)
		if told := r.beside(); told[claim.RoleArchitect] != owner || told[claim.RoleReviewer] != "" {
			t.Fatalf("the fix-started rows' branches = %v, want the owner's own branch %s and none for the reviewer", told, owner)
		}
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleArchitect:   {Branch: owner},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})

	t.Run("the control: no fix", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.assertPushes("with no fix", map[claim.Role]supervise.UpdatePush{})
		if told := r.beside(); len(told) != 0 {
			t.Fatalf("the fix-started rows' branches with no fix = %v, want none", told)
		}
		for _, at := range []phase.Phase{phase.Testing, phase.Reviewing} {
			if facts := r.startFactsIn(at); facts.Branch != "" {
				t.Fatalf("the start facts in %s with no fix = %+v, want no branch", at, facts)
			}
		}
	})
}
