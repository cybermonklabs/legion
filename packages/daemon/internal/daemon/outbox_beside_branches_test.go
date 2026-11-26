package daemon

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The log lines of the deletion of a branch beside a fix: a deletion, an absence GitHub answered
// 404 to the delete, an absence the exact read confirmed after a 422, a deletion that failed, and
// a ref GitHub listed outside the prefix asked, which is refused; and the outbox's line of a row
// whose run failed and that it keeps for its retry.
const (
	besideDeletedLine = "outbox deleted a branch beside a fix"
	beside404Line     = "outbox found a branch beside a fix already absent: GitHub answered its delete 404"
	beside422Line     = "outbox found a branch beside a fix already absent: GitHub answered its delete 422 and its ref 404"
	besideFailedLine  = "outbox could not delete a branch beside a fix; the row keeps it for its retry"
	besideRefusedLine = "outbox refused a ref GitHub listed outside the branches beside a fix; it deletes nothing for it, and the row keeps it for its retry"
	rowFailedLine     = "outbox row failed"
)

// besideBranches is the branches the rig plants: three beside fix 1 of the task default, one
// beside fix 2, one beside fix 1 of another task, and the issue's own branch.
type besideBranches struct {
	fix1        []string
	fix2, other string
	issue       string
}

func besideBranchesOf(t *testing.T, key string) besideBranches {
	t.Helper()
	branch := func(task string, fix int, role claim.Role) string {
		t.Helper()
		name, err := claim.BesideBranch(key, 1, task, fix, role)
		if err != nil {
			t.Fatalf("the branch beside fix %d of %s for %s: %v", fix, task, role, err)
		}
		return name
	}
	return besideBranches{
		fix1: []string{branch("default", 1, claim.RoleArchitect), branch("default", 1, claim.RoleImplementer), branch("default", 1, claim.RoleTester)},
		fix2: branch("default", 2, claim.RoleTester), other: branch("other-task", 1, claim.RoleTester), issue: "legion/" + key,
	}
}

// besideDaemon is a whole daemon (ciDaemon) whose GitHub holds the branches of besideBranchesOf
// on the issue's repository (refs, beside the reads every boot makes, ciGitHub), the issue in
// reviewing, booted. It reports UNKNOWN and fails where the planting holds no branch of fix 1,
// since a deletion of nothing would then pass every assertion below. seams sets seams of the
// daemon's outbox.
func besideDaemon(t *testing.T, seams func(*outbox)) (*ciDaemon, *githubtest.Refs, besideBranches) {
	t.Helper()
	d := newCIDaemon(t, seams)
	refs := githubtest.NewRefs()
	branches := besideBranchesOf(t, d.key)
	for i, name := range append(slices.Clone(branches.fix1), branches.fix2, branches.other, branches.issue) {
		refs.Set(liveRepo, name, string(rune('a'+i))+"000000000000000000000000000000000000000")
	}
	prefix, err := claim.BesideFixPrefix(d.key, 1, "default", 1)
	if err != nil {
		t.Fatalf("fix 1's prefix: %v", err)
	}
	if planted := refs.Branches(liveRepo, prefix); len(planted) != 3 {
		t.Fatalf("UNKNOWN: the planting holds %d branches beside fix 1, want 3: a deletion would delete nothing and every check would pass", len(planted))
	}
	d.o.githubAPI = ciGitHub(t, refs)
	d.seedIssue(phase.Reviewing, 1, ciHeadX)
	d.boot()
	return d, refs, branches
}

// standing is which of names refs still holds.
func standing(refs *githubtest.Refs, names ...string) []string {
	var held []string
	for _, name := range names {
		if slices.Contains(refs.Branches(liveRepo, name), name) {
			held = append(held, name)
		}
	}
	return held
}

// besideLines is the indexes, in the daemon's log, of the lines of msg that name branch, oldest
// first, so a test can read how many there are and in what order they came.
func (d *ciDaemon) besideLines(msg, branch string) []int {
	var at []int
	for i, line := range d.lines(msg) {
		if line["branch"] == branch {
			at = append(at, i)
		}
	}
	return at
}

// logged is where in the daemon's log the first line of msg that names branch stands, -1 for none.
func (d *ciDaemon) logged(msg, branch string) int {
	at := -1
	for i, line := range strings.Split(d.logs.String(), "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) && strings.Contains(line, `"branch":"`+branch+`"`) {
			at = i
			break
		}
	}
	return at
}

// requestsOf is each status refs answered to method on the path that ends with branch, in order.
func requestsOf(refs *githubtest.Refs, method, branch string) []int {
	var statuses []int
	for _, request := range refs.Requests() {
		if request.Method == method && strings.HasSuffix(request.Path, "/"+branch) {
			statuses = append(statuses, request.Status)
		}
	}
	return statuses
}

// The deletion of the branches beside a fix deletes the three beside fix 1 and no other: the
// branch beside fix 2, the one beside another task and the issue's own branch stand. A delete
// GitHub answers 404 finishes that branch as absent with a line of its own. The row of fix 0
// through fix 2 then deletes the branch beside fix 2 too, and leaves the other task's and the
// issue's. A delete GitHub answers 422 is read again by the branch's exact ref: where the ref still
// stands, the row fails and is kept for its retry, logging a failure and no deletion of it, and its
// replay deletes it; where the ref reads 404, the branch finishes as absent, not as deleted. A 500
// keeps the row for its retry too. A ref the list answers outside the prefix it was asked for, the
// issue's own branch or a tag, is refused before either request: it is logged at WARN by its ref,
// GitHub is asked no delete of it, and the row fails and is kept for its retry, while the list's
// branches of fix 1 are deleted. Each runs on a whole daemon (besideDaemon), the row on its
// role-message lane.
//
// The controls, kept subtests: the row of fix 2 leaves fix 1's three branches, and with a 422 taken
// as gone (the seam delete422AsGone) the branch that still stands is finished and logged as
// deleted, which the subtest asserts. On the daemon of the row of fix 2, a row of every fix through
// fix 1 then deletes fix 1's three and leaves a branch pushed under fix 2 after it, as a fix the
// task opens later in the generation pushes one (a later version adds the task again, or a move
// back takes it out of its retro), and GitHub is asked no delete of it; the row of fix 0 through
// fix 2 in the first subtest is its control, which deletes fix 2's branch.
func TestTheBranchesBesideAFixAreDeleted(t *testing.T) {
	t.Parallel()
	t.Run("fix 1's branches go, a 404 is an absence, and fix 0 takes the task's rest", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, nil)
		refs.Fail(http.MethodDelete, liveRepo, b.fix1[0], http.StatusNotFound, true)
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		d.until("", "fix 1's row to finish", func() bool { return !d.rowLeft(id) })
		if held := standing(refs, b.fix1...); len(held) != 0 {
			t.Fatalf("after fix 1's row %v of fix 1 stand, want none", held)
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
		if len(d.besideLines(beside404Line, b.fix1[0])) != 1 || len(d.besideLines(besideDeletedLine, b.fix1[0])) != 0 ||
			len(d.besideLines(besideDeletedLine, b.fix1[1])) != 1 || len(d.besideLines(besideDeletedLine, b.fix1[2])) != 1 {
			t.Fatalf("the lines of fix 1's branches: absent %v %v %v, deleted %v %v %v; want the 404 one absent and the other two deleted",
				d.besideLines(beside404Line, b.fix1[0]), d.besideLines(beside404Line, b.fix1[1]), d.besideLines(beside404Line, b.fix1[2]),
				d.besideLines(besideDeletedLine, b.fix1[0]), d.besideLines(besideDeletedLine, b.fix1[1]), d.besideLines(besideDeletedLine, b.fix1[2]))
		}
		if held := standing(refs, b.fix2, b.other, b.issue); len(held) != 3 {
			t.Fatalf("after fix 1's row %v stand, want fix 2's, the other task's and the issue's", held)
		}
		task := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Through: 2})
		d.until("", "the task's row to finish", func() bool { return !d.rowLeft(task) })
		if held := standing(refs, b.fix2, b.other, b.issue); !slices.Equal(held, []string{b.other, b.issue}) {
			t.Fatalf("after the task's row %v stand, want the other task's and the issue's alone", held)
		}
	})
	t.Run("a 422 whose ref still stands keeps the row, and its replay deletes it", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, nil)
		refs.Fail(http.MethodDelete, liveRepo, b.fix1[2], http.StatusUnprocessableEntity, false)
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		d.until("", "the row's replay to finish it", func() bool { return !d.rowLeft(id) })
		if deletes, reads := requestsOf(refs, http.MethodDelete, b.fix1[2]), requestsOf(refs, http.MethodGet, b.fix1[2]); len(deletes) != 2 ||
			deletes[0] != http.StatusUnprocessableEntity || len(reads) == 0 || reads[0] != http.StatusOK {
			t.Fatalf("GitHub answered the deletes of %s %v and its exact reads %v, want a 422, a read that found it, then the replay's delete", b.fix1[2], deletes, reads)
		}
		failed, deleted := d.logged(besideFailedLine, b.fix1[2]), d.logged(besideDeletedLine, b.fix1[2])
		if failed < 0 || deleted < 0 || deleted < failed || len(d.besideLines(besideDeletedLine, b.fix1[2])) != 1 || len(d.ciLines(rowFailedLine, id)) == 0 {
			t.Fatalf("the lines of %s: its failure at %d, its deletion at %d (%d deletions), %d failures of the row; want the failure and the row kept for its retry, then one deletion at the replay",
				b.fix1[2], failed, deleted, len(d.besideLines(besideDeletedLine, b.fix1[2])), len(d.ciLines(rowFailedLine, id)))
		}
		if held := standing(refs, b.fix1...); len(held) != 0 {
			t.Fatalf("after the replay %v of fix 1 stand, want none", held)
		}
		if held := standing(refs, b.fix2, b.other, b.issue); len(held) != 3 {
			t.Fatalf("after the replay %v stand, want every branch outside fix 1's", held)
		}
	})
	t.Run("a 422 whose ref reads 404 finishes as absent", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, nil)
		refs.Fail(http.MethodDelete, liveRepo, b.fix1[2], http.StatusUnprocessableEntity, true)
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		d.until("", "the row to finish", func() bool { return !d.rowLeft(id) })
		if len(d.besideLines(beside422Line, b.fix1[2])) != 1 || len(d.besideLines(besideDeletedLine, b.fix1[2])) != 0 || len(d.ciLines(rowFailedLine, id)) != 0 {
			t.Fatalf("after a 422 of a branch gone: absence lines %v, deletion lines %v, %d failures of the row; want one absence line, no deletion line and no failure",
				d.besideLines(beside422Line, b.fix1[2]), d.besideLines(besideDeletedLine, b.fix1[2]), len(d.ciLines(rowFailedLine, id)))
		}
	})
	t.Run("a 500 keeps the row for its retry", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, nil)
		refs.Fail(http.MethodDelete, liveRepo, b.fix1[1], http.StatusInternalServerError, false)
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		d.until("", "the row's retry to finish it", func() bool { return !d.rowLeft(id) })
		if deletes := requestsOf(refs, http.MethodDelete, b.fix1[1]); len(deletes) != 2 || deletes[0] != http.StatusInternalServerError || len(d.ciLines(rowFailedLine, id)) == 0 {
			t.Fatalf("GitHub answered the deletes of %s %v, the row failed %d times; want a 500 that kept the row, then its retry's delete",
				b.fix1[1], deletes, len(d.ciLines(rowFailedLine, id)))
		}
		if held := standing(refs, b.fix1...); len(held) != 0 {
			t.Fatalf("after the retry %v of fix 1 stand, want none", held)
		}
	})
	t.Run("control: fix 2's row leaves fix 1's branches", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, nil)
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 2})
		d.until("", "fix 2's row to finish", func() bool { return !d.rowLeft(id) })
		if held := standing(refs, b.fix1...); len(held) != 3 || len(standing(refs, b.fix2)) != 0 {
			t.Fatalf("fix 2's row left %v of fix 1 and %v of fix 2; want fix 1's three and none of fix 2", held, standing(refs, b.fix2))
		}
		t.Run("a row of every fix through fix 1 leaves a later fix's branch", func(t *testing.T) {
			refs.Set(liveRepo, b.fix2, "e000000000000000000000000000000000000000")
			if planted := standing(refs, b.fix2); len(planted) != 1 {
				t.Fatalf("UNKNOWN: the planting holds %v beside fix 2, want its one branch: a row that never reached fix 2 would pass", planted)
			}
			id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Through: 1})
			d.until("", "the row through fix 1 to finish", func() bool { return !d.rowLeft(id) })
			if held := standing(refs, b.fix1...); len(held) != 0 {
				t.Fatalf("after the row through fix 1 %v of fix 1 stand, want none", held)
			}
			if held := standing(refs, b.fix2, b.other, b.issue); !slices.Equal(held, []string{b.fix2, b.other, b.issue}) {
				t.Fatalf("after the row through fix 1 %v stand, want fix 2's, the other task's and the issue's", held)
			}
			if deletes := requestsOf(refs, http.MethodDelete, b.fix2); len(deletes) != 1 {
				t.Fatalf("GitHub answered the deletes of %s %v, want the one of fix 2's row and none of the row through fix 1", b.fix2, deletes)
			}
		})
	})
	t.Run("a listed ref outside the prefix is refused, and nothing is deleted for it", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, nil)
		prefix, err := claim.BesideFixPrefix(d.key, 1, "default", 1)
		if err != nil {
			t.Fatalf("fix 1's prefix: %v", err)
		}
		issueRef, tagRef := "refs/heads/"+b.issue, "refs/tags/"+b.fix1[1]
		refs.ListAlso(liveRepo, prefix, issueRef, "f000000000000000000000000000000000000000")
		refs.ListAlso(liveRepo, prefix, tagRef, "f100000000000000000000000000000000000000")
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		d.until("", "the row's first run to end", func() bool { return !d.rowLeft(id) || len(d.ciLines(rowFailedLine, id)) > 0 })
		if held := standing(refs, b.issue); len(held) != 1 {
			t.Fatalf("after the row the issue's own branch %s stands %v, want it standing: GitHub listed it outside fix 1's prefix", b.issue, held)
		}
		var deletes []string
		for _, request := range refs.Requests() {
			if request.Method == http.MethodDelete {
				deletes = append(deletes, request.Path)
			}
		}
		var want []string
		for _, branch := range b.fix1 {
			want = append(want, "/git/refs/heads/"+branch)
		}
		slices.Sort(deletes)
		slices.Sort(want)
		if !slices.Equal(deletes, want) {
			t.Fatalf("GitHub was asked the deletes %v, want fix 1's three alone, %v", deletes, want)
		}
		for _, outside := range []string{issueRef, tagRef} {
			var refused []map[string]any
			for _, line := range d.ciLines(besideRefusedLine, id) {
				if line["ref"] == outside {
					refused = append(refused, line)
				}
			}
			if len(refused) == 0 || refused[0]["level"] != "WARN" {
				t.Fatalf("the refusal lines of %s = %v, want one at WARN naming the ref", outside, refused)
			}
		}
		if held := standing(refs, b.fix1...); len(held) != 0 {
			t.Fatalf("after the row %v of fix 1 stand, want none: the refusal leaves the list's own branches to their deletes", held)
		}
		if !d.rowLeft(id) || len(d.ciLines(rowFailedLine, id)) == 0 {
			t.Fatalf("after the refusal the row left %t with %d failures, want it failed and kept for its retry", d.rowLeft(id), len(d.ciLines(rowFailedLine, id)))
		}
	})
	t.Run("control: delete422AsGone finishes a branch that still stands as deleted", func(t *testing.T) {
		t.Parallel()
		d, refs, b := besideDaemon(t, func(o *outbox) { o.delete422AsGone = true })
		refs.Fail(http.MethodDelete, liveRepo, b.fix1[2], http.StatusUnprocessableEntity, false)
		id := d.queue(record.BesideBranchesDelete{Generation: 1, Task: "default", Fix: 1})
		d.until("", "the row to finish", func() bool { return !d.rowLeft(id) })
		if !slices.Equal(standing(refs, b.fix1...), []string{b.fix1[2]}) || len(d.besideLines(besideDeletedLine, b.fix1[2])) != 1 {
			t.Fatalf("with a 422 taken as gone: %v stand, deletion lines %v; want the branch standing and logged deleted, the fault the exact read takes out",
				standing(refs, b.fix1...), d.besideLines(besideDeletedLine, b.fix1[2]))
		}
	})
}
