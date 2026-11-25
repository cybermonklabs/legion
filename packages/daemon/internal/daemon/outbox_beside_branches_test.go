package daemon

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// besideRepo is the repository the issue of the rows here works, as runnerIn records it.
const besideRepo = "acme/widgets"

// The log lines of the deletion of a branch beside a fix: a deletion, an absence GitHub answered
// 404 to the delete, an absence the exact read confirmed after a 422, and a deletion that failed.
const (
	besideDeletedLine = "outbox deleted a branch beside a fix"
	beside404Line     = "outbox found a branch beside a fix already absent: GitHub answered its delete 404"
	beside422Line     = "outbox found a branch beside a fix already absent: GitHub answered its delete 422 and its ref 404"
	besideFailedLine  = "outbox could not delete a branch beside a fix; the row keeps it for its retry"
)

// besideBranches is the branches the rig plants: three beside fix 1 of the task default, one
// beside fix 2, one beside fix 1 of another task, and the issue's own branch.
type besideBranches struct {
	fix1        []string
	fix2, other string
	issue       string
}

func besideBranchesOf(t *testing.T) besideBranches {
	t.Helper()
	branch := func(task string, fix int, role claim.Role) string {
		t.Helper()
		name, err := claim.BesideBranch(ciIssue, 1, task, fix, role)
		if err != nil {
			t.Fatalf("the branch beside fix %d of %s for %s: %v", fix, task, role, err)
		}
		return name
	}
	return besideBranches{
		fix1: []string{branch("default", 1, claim.RoleArchitect), branch("default", 1, claim.RoleImplementer), branch("default", 1, claim.RoleTester)},
		fix2: branch("default", 2, claim.RoleTester), other: branch("other-task", 1, claim.RoleTester), issue: "legion/" + ciIssue,
	}
}

// besideRig is a ciRig whose GitHub is refs, holding the branches of besideBranchesOf: it reports
// UNKNOWN and fails where the planting holds no branch of fix 1, since a deletion of nothing would
// then pass every assertion below.
func besideRig(t *testing.T) (*ciRig, *githubtest.Refs, besideBranches) {
	t.Helper()
	r := newCIRig(t, phase.Reviewing)
	refs := githubtest.NewRefs()
	branches := besideBranchesOf(t)
	for i, name := range append(slices.Clone(branches.fix1), branches.fix2, branches.other, branches.issue) {
		refs.Set(besideRepo, name, string(rune('a'+i))+"000000000000000000000000000000000000000")
	}
	r.runner.githubAPI = refs.Serve(t)
	prefix, err := claim.BesideFixPrefix(ciIssue, 1, "default", 1)
	if err != nil {
		t.Fatalf("fix 1's prefix: %v", err)
	}
	if planted := refs.Branches(besideRepo, prefix); len(planted) != 3 {
		t.Fatalf("UNKNOWN: the planting holds %d branches beside fix 1, want 3: a deletion would delete nothing and every check would pass", len(planted))
	}
	return r, refs, branches
}

// standing is which of names refs still holds.
func standing(refs *githubtest.Refs, names ...string) []string {
	var held []string
	for _, name := range names {
		if slices.Contains(refs.Branches(besideRepo, name), name) {
			held = append(held, name)
		}
	}
	return held
}

// linesOf is the rig's lines of text that name branch.
func (r *ciRig) linesOf(text, branch string) int {
	n := 0
	for _, line := range r.lines(text) {
		if line["branch"] == branch {
			n++
		}
	}
	return n
}

// The deletion of the branches beside a fix deletes the three beside fix 1 and no other: the
// branch beside fix 2, the one beside another task and the issue's own branch stand. A delete
// GitHub answers 404 finishes that branch as absent with a line of its own. The row of fix 0 then
// deletes the branch beside fix 2 too, and leaves the other task's and the issue's. A delete GitHub
// answers 422 is read again by the branch's exact ref: where the ref still stands, the row fails
// with its retry, logs no deletion of it and a failure, and its replay deletes it; where the ref
// reads 404, the branch finishes as absent, not as deleted. A 500 keeps the row for its retry too.
//
// The controls, kept subtests: the row of fix 2 leaves fix 1's three branches, and with a 422 taken
// as gone (the seam delete422AsGone) the branch that still stands is finished and logged as
// deleted, which the subtest asserts.
func TestTheBranchesBesideAFixAreDeleted(t *testing.T) {
	t.Parallel()
	t.Run("fix 1's branches go, a 404 is an absence, and fix 0 takes the task's rest", func(t *testing.T) {
		t.Parallel()
		r, refs, b := besideRig(t)
		refs.Fail(http.MethodDelete, besideRepo, b.fix1[0], http.StatusNotFound, true)
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		r.attempt(time.Minute)
		if held := standing(refs, b.fix1...); len(held) != 0 || r.left(record.OutboxKindBesideBranchesDelete) != 0 {
			t.Fatalf("after fix 1's row: %v of fix 1 stand and %d rows are left; want none of either", held, r.left(record.OutboxKindBesideBranchesDelete))
		}
		deletes := 0
		for _, request := range refs.Requests() {
			if request.Method == http.MethodDelete {
				deletes++
			}
		}
		if deletes != 3 {
			t.Fatalf("the row asked %d deletes, want the 3 the list named", deletes)
		}
		if r.linesOf(beside404Line, b.fix1[0]) != 1 || r.linesOf(besideDeletedLine, b.fix1[0]) != 0 ||
			r.linesOf(besideDeletedLine, b.fix1[1]) != 1 || r.linesOf(besideDeletedLine, b.fix1[2]) != 1 {
			t.Fatalf("the lines = %s, want the 404 one absent and the other two deleted", r.logs.String())
		}
		if held := standing(refs, b.fix2, b.other, b.issue); len(held) != 3 {
			t.Fatalf("after fix 1's row %v stand, want fix 2's, the other task's and the issue's", held)
		}
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default"})
		r.attempt(3 * time.Minute)
		if held := standing(refs, b.fix2, b.other, b.issue); !slices.Equal(held, []string{b.other, b.issue}) {
			t.Fatalf("after the task's row %v stand, want the other task's and the issue's alone", held)
		}
	})
	t.Run("a 422 whose ref still stands keeps the row, and its replay deletes it", func(t *testing.T) {
		t.Parallel()
		r, refs, b := besideRig(t)
		refs.Fail(http.MethodDelete, besideRepo, b.fix1[2], http.StatusUnprocessableEntity, false)
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		r.attempt(time.Minute)
		var attempts int
		var lastError string
		if err := r.pool.QueryRow(context.Background(), `select attempts, last_error from outbox where kind = 'beside_branches_delete'`).Scan(&attempts, &lastError); err != nil {
			t.Fatalf("read the row after the 422: %v", err)
		}
		if attempts != 1 || lastError == "" || !slices.Equal(standing(refs, b.fix1...), []string{b.fix1[2]}) {
			t.Fatalf("after the 422: attempts %d, error %q, standing %v; want the row kept with its failure and the branch standing", attempts, lastError, standing(refs, b.fix1...))
		}
		if r.linesOf(besideDeletedLine, b.fix1[2]) != 0 || r.linesOf(besideFailedLine, b.fix1[2]) != 1 {
			t.Fatalf("the lines = %s, want a failure of %s and no deletion of it", r.logs.String(), b.fix1[2])
		}
		r.attempt(5 * time.Minute)
		if held := standing(refs, b.fix1...); len(held) != 0 || r.left(record.OutboxKindBesideBranchesDelete) != 0 || r.linesOf(besideDeletedLine, b.fix1[2]) != 1 {
			t.Fatalf("after the replay: %v stand, %d rows; want the branch deleted and the row finished", held, r.left(record.OutboxKindBesideBranchesDelete))
		}
		if held := standing(refs, b.fix2, b.other, b.issue); len(held) != 3 {
			t.Fatalf("after the replay %v stand, want every branch outside fix 1's", held)
		}
	})
	t.Run("a 422 whose ref reads 404 finishes as absent", func(t *testing.T) {
		t.Parallel()
		r, refs, b := besideRig(t)
		refs.Fail(http.MethodDelete, besideRepo, b.fix1[2], http.StatusUnprocessableEntity, true)
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		r.attempt(time.Minute)
		if r.left(record.OutboxKindBesideBranchesDelete) != 0 || r.linesOf(beside422Line, b.fix1[2]) != 1 || r.linesOf(besideDeletedLine, b.fix1[2]) != 0 {
			t.Fatalf("after a 422 of a branch gone: %d rows, lines %s; want the row finished with the absence line and no deletion line",
				r.left(record.OutboxKindBesideBranchesDelete), r.logs.String())
		}
	})
	t.Run("a 500 keeps the row for its retry", func(t *testing.T) {
		t.Parallel()
		r, refs, b := besideRig(t)
		refs.Fail(http.MethodDelete, besideRepo, b.fix1[1], http.StatusInternalServerError, false)
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		r.attempt(time.Minute)
		if r.left(record.OutboxKindBesideBranchesDelete) != 1 || !slices.Equal(standing(refs, b.fix1...), []string{b.fix1[1]}) {
			t.Fatalf("after the 500: %d rows, %v stand; want the row kept and the branch standing", r.left(record.OutboxKindBesideBranchesDelete), standing(refs, b.fix1...))
		}
		r.attempt(5 * time.Minute)
		if r.left(record.OutboxKindBesideBranchesDelete) != 0 || len(standing(refs, b.fix1...)) != 0 {
			t.Fatalf("after the retry: %d rows, %v stand; want both gone", r.left(record.OutboxKindBesideBranchesDelete), standing(refs, b.fix1...))
		}
	})
	t.Run("control: fix 2's row leaves fix 1's branches", func(t *testing.T) {
		t.Parallel()
		r, refs, b := besideRig(t)
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 2})
		r.attempt(time.Minute)
		if held := standing(refs, b.fix1...); len(held) != 3 || len(standing(refs, b.fix2)) != 0 {
			t.Fatalf("fix 2's row left %v of fix 1 and %v of fix 2; want fix 1's three and none of fix 2", held, standing(refs, b.fix2))
		}
	})
	t.Run("control: delete422AsGone finishes a branch that still stands as deleted", func(t *testing.T) {
		t.Parallel()
		r, refs, b := besideRig(t)
		r.runner.delete422AsGone = true
		refs.Fail(http.MethodDelete, besideRepo, b.fix1[2], http.StatusUnprocessableEntity, false)
		r.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		r.attempt(time.Minute)
		if r.left(record.OutboxKindBesideBranchesDelete) != 0 || !slices.Equal(standing(refs, b.fix1...), []string{b.fix1[2]}) || r.linesOf(besideDeletedLine, b.fix1[2]) != 1 {
			t.Fatalf("with a 422 taken as gone: %d rows, %v stand, lines %s; want the row finished, the branch standing and logged deleted, the fault the exact read takes out",
				r.left(record.OutboxKindBesideBranchesDelete), standing(refs, b.fix1...), r.logs.String())
		}
	})
}
