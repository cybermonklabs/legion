package shim_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The tester's commit beside the fix: the file it adds and its subject.
const (
	redFile    = "red_test.txt"
	redSubject = "test: the red test"
	// redEdit is what the fix writes to the tester's file where it changes it.
	redEdit = "fix: the red test's case"
)

// takeCommands is the shape of workspace.BesideTakeCommands.
type takeCommands func(dir, prefix, own, taskBranch string) []string

// takeCase is one way a fix runs its take a second time in the same fix, after the first.
type takeCase struct {
	name   string
	tester bool // the tester commits red_test.txt and pushes its branch before the first take
	edit   bool // after the first take, the fix commits a change of red_test.txt
	push   bool // after the first take, the fix pushes its own branch F
	fresh  bool // a second clone, a fresh pod, adopts the fix at F and runs the second take
}

// takeReading is what a case reads in the working copy of the fix that ran the second take. It
// reads the tester's commit by its subject and by its change of red_test.txt, never by a commit
// id, since a copy has an id of its own.
type takeReading struct {
	exits        []int    // each command's exit status: the pushes and every command of both takes
	stack        []string // each commit from the task's head to @, as stackTemplate prints it
	bySubject    int      // the commits of stack with the tester's subject
	byContent    int      // the commits of stack whose own change of red_test.txt is the tester's
	conflicted   int      // the commits of stack with a conflict
	divergent    int      // the commits of stack whose change has another visible commit
	firstChanged bool     // whether the first take changed any commit of stack
	changed      bool     // whether the second take changed any commit of stack
	files        []string // the files of @-
	red          string   // red_test.txt at @-, "" where @- holds none
	shared       string   // what is wrong with the shared copy of the pod the tester ran in, "" for nothing
}

// stackTemplate prints a commit of the fix's stack on one line: its id, its change, whether it is
// empty, conflicted or divergent, and its subject.
const stackTemplate = `commit_id ++ "|" ++ change_id.short() ++ "|" ++ if(empty, "empty") ++ "|" ++ if(conflict, "conflict") ++ "|" ++ if(divergent, "divergent") ++ "|" ++ description.first_line() ++ "\n"`

// runTakeCase runs c with the take that take renders. A pod is made from a new origin, and the
// fix's copy is made at the task's head. Where c has a tester, it commits red_test.txt in the shared
// copy and pushes its branch B with BesidePushCommand. The fix commits fix.txt in its copy and runs
// the take; then c's steps; then the take again, in the same pod or in a fresh one. Each command
// runs as an agent's shell runs it, and the test logs what it printed.
func runTakeCase(t *testing.T, jj string, take takeCommands, c takeCase) takeReading {
	t.Helper()
	const key = "ACME-42"
	named := func(name string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	own := named(claim.BesideBranch(key, 1, "a", 1, claim.RoleImplementer))
	tester := named(claim.BesideBranch(key, 1, "a", 1, claim.RoleTester))
	prefix := named(claim.BesideFixPrefix(key, 1, "a", 1))
	taskBranch := claim.IssueBranch(key)
	overHead := `remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")..@`

	var reading takeReading
	run := func(r *copyRig, dir, command string) {
		t.Helper()
		output, status := r.shell(dir, command)
		t.Logf("exit %d: %s\n%s", status, command, output)
		reading.exits = append(reading.exits, status)
	}
	stackOf := func(r *copyRig) []string {
		t.Helper()
		listed := r.jjIn(r.fixDir, "log", "-r", overHead, "--no-graph", "-T", stackTemplate)
		if listed == "" {
			return nil
		}
		return strings.Split(listed, "\n")
	}
	runTake := func(r *copyRig) {
		t.Helper()
		for _, command := range take(r.fixDir, prefix, own, taskBranch) {
			run(r, r.fixDir, command)
		}
	}

	origin := newOrigin(t, key)
	pod := newPodRig(t, jj, origin, key)
	head := pod.head
	if got := pod.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: own}); !got.OK || got.FixDir != pod.fixDir {
		t.Fatalf("the fix's adoption answered %#v, want ok with fixDir %s", got, pod.fixDir)
	}
	red := head
	if c.tester {
		red = pod.commit(pod.workspace, redFile, redSubject)
		run(pod, pod.workspace, workspace.BesidePushCommand(tester))
	}
	pod.commit(pod.fixDir, "fix.txt", "fix: the failure")

	first := stackOf(pod)
	runTake(pod)
	afterFirst := stackOf(pod)
	reading.firstChanged = !slices.Equal(first, afterFirst)
	t.Logf("after the first take, the fix's commits over the task's head:\n%s", strings.Join(afterFirst, "\n"))

	if c.edit {
		pod.commit(pod.fixDir, redFile, redEdit)
	}
	if c.push {
		run(pod, pod.fixDir, `jj -R "$LEGION_WORKSPACE-fix" bookmark set `+own+` -r @-`)
		run(pod, pod.fixDir, `jj -R "$LEGION_WORKSPACE-fix" git push --remote origin --bookmark `+own)
	}
	taker := pod
	if c.fresh {
		taker = newPodRig(t, jj, origin, key)
		if got := taker.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: own}); !got.OK || got.FixDir != taker.fixDir {
			t.Fatalf("the fresh pod's adoption answered %#v, want ok with fixDir %s", got, taker.fixDir)
		}
	}

	before := stackOf(taker)
	runTake(taker)
	reading.stack = stackOf(taker)
	reading.changed = !slices.Equal(before, reading.stack)
	t.Logf("after the second take, the fix's commits over the task's head:\n%s", strings.Join(reading.stack, "\n"))
	t.Logf("jj log of the task's head and every commit over it:\n%s", taker.jjIn(taker.fixDir, "log", "-r", `remote_bookmarks(exact:"`+taskBranch+`", exact:"origin")::`))

	testers := ""
	if c.tester {
		testers = pod.jjIn(pod.workspace, "diff", "--git", "-r", red, redFile)
	}
	for _, line := range reading.stack {
		fields := strings.SplitN(line, "|", 6)
		if len(fields) != 6 {
			t.Fatalf("jj printed %q for a commit of the fix's stack, want six fields", line)
		}
		if fields[5] == redSubject {
			reading.bySubject++
		}
		if c.tester && taker.jjIn(taker.fixDir, "diff", "--git", "-r", fields[0], redFile) == testers {
			reading.byContent++
		}
		if fields[3] != "" {
			reading.conflicted++
		}
		if fields[4] != "" {
			reading.divergent++
		}
	}
	reading.files = taker.filesOf(taker.fixDir, "@-")
	if slices.Contains(reading.files, redFile) {
		reading.red = taker.jjIn(taker.fixDir, "file", "show", "-r", "@-", redFile)
	}

	// The shared copy stands on the tester's commit, and no take rewrote it: a rewrite of a commit
	// under the shared working copy leaves that copy stale (TestARewriteFromTheFixCopyIsBroughtUpToDate).
	if status, err := pod.try(pod.workspace, "status"); err != nil || strings.Contains(status, "stale") {
		reading.shared = "jj status: " + status
	} else if parent := pod.commitOf(pod.workspace, "@-"); parent != red {
		reading.shared = "it stands on " + parent + ", not on " + red
	}
	return reading
}

// takeCases are the ways a fix runs its take again in one fix, and the take with nothing to take.
// The fix's task says the take before its one end push, and sends that text again on a resumed
// turn and after an update round, so the fix may run the take again: in the same turn's copy, after
// it pushed its own branch, in a fresh pod that adopts the fix at that branch, and after it
// changed what it took.
var takeCases = []takeCase{
	{name: "a resumed turn runs the take again", tester: true},
	{name: "the fix pushes its own branch, then runs the take again", tester: true, push: true},
	{name: "a fresh pod adopts the fix at its own branch and runs the take again", tester: true, push: true, fresh: true},
	{name: "the fix changes the tester's file, then runs the take again", tester: true, edit: true},
	{name: "with nothing to take, the take changes nothing", tester: false},
}

// A fix may run its take more than once in one fix, and each run must leave the fix's stack over
// the task's head holding the tester's commit once, by its subject and by its change of
// red_test.txt, with no conflict, and the second run must change no commit of that stack. The
// commit under the fix's working copy, which it pushes and completes with, holds fix.txt and the
// tester's red_test.txt, as the fix last wrote it. No take rewrites the tester's commit: the shared
// copy still stands on it and is not stale. A take with nothing to take exits 0 and changes nothing.
func TestATakeRunAgainTakesEachCommitOnce(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	for _, c := range takeCases {
		t.Run(c.name, func(t *testing.T) {
			got := runTakeCase(t, jj, workspace.BesideTakeCommands, c)
			if slices.ContainsFunc(got.exits, func(status int) bool { return status != 0 }) {
				t.Errorf("the commands exited %v, want each 0", got.exits)
			}
			want := 0
			if c.tester {
				want = 1
			}
			if got.bySubject != want || got.byContent != want {
				t.Errorf("the fix's stack over the task's head holds the tester's commit %d times by its subject and %d by its content, want %d:\n%s",
					got.bySubject, got.byContent, want, strings.Join(got.stack, "\n"))
			}
			if got.conflicted != 0 || got.divergent != 0 {
				t.Errorf("the fix's stack holds %d conflicted and %d divergent commits, want none:\n%s", got.conflicted, got.divergent, strings.Join(got.stack, "\n"))
			}
			if got.changed {
				t.Errorf("the second take changed the fix's stack, want it unchanged:\n%s", strings.Join(got.stack, "\n"))
			}
			if !c.tester && got.firstChanged {
				t.Errorf("a take with nothing to take changed the fix's stack, want it unchanged:\n%s", strings.Join(got.stack, "\n"))
			}
			wantRed := ""
			switch {
			case c.edit:
				wantRed = redEdit
			case c.tester:
				wantRed = redSubject
			}
			if !slices.Contains(got.files, "fix.txt") || got.red != wantRed {
				t.Errorf("the commit under the fix's working copy holds %v with red_test.txt %q, want fix.txt and red_test.txt %q", got.files, got.red, wantRed)
			}
			if got.shared != "" {
				t.Errorf("the shared copy is not as the tester left it: %s", got.shared)
			}
		})
	}
}
