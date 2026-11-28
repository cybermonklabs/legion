package shim_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The commits of the steps beside the fix: the tester's, and a second step's, each a file it adds
// and its subject.
const (
	redFile       = "red_test.txt"
	redSubject    = "test: the red test"
	reviewFile    = "review.txt"
	reviewSubject = "test: the reviewer's case"
	// redEdit is what the fix writes to the tester's file where it changes it.
	redEdit = "fix: the red test's case"
)

// takeCase is one way a fix runs its take a second time in the same fix, after the first.
type takeCase struct {
	name         string
	tester       bool // the tester commits red_test.txt and pushes its branch before the first take
	second       bool // a second step beside the fix commits review.txt at the task's head and pushes its branch
	noOwn        bool // the fix commits nothing of its own
	mergeForward bool // main moves on origin, and the fix's first act is a merge of it
	edit         bool // after the first take, the fix commits a change of red_test.txt
	push         bool // after the first take, the fix pushes its own branch F
	fresh        bool // a second clone, a fresh pod, adopts the fix at F and runs the second take
}

// takeReading is what a case reads in the working copy of the fix that ran the second take. It
// reads each step's commit by its subject and by its change of its file, never by a commit id,
// since a copy has an id of its own.
type takeReading struct {
	exits        []int          // each command's exit status: the pushes and every command of both takes
	stack        []string       // each commit from the task's head to @, as stackTemplate prints it
	bySubject    map[string]int // the commits of stack by their subject
	byContent    map[string]int // by file, the commits of stack whose own change of the file is its step's
	conflicted   int            // the commits of stack with a conflict
	divergent    int            // the commits of stack whose change has another visible commit
	firstChanged bool           // whether the first take changed any commit of stack
	changed      bool           // whether the second take changed any commit of stack
	under        string         // what jj answered where `@-` is not one commit, "" where it is
	files        []string       // the files of @-
	red          string         // red_test.txt at @-, "" where @- holds none
	shared       string         // what is wrong with the shared copy of the pod the tester ran in, "" for nothing
}

// stackTemplate prints a commit of the fix's stack on one line: its id, its change, whether it is
// empty, conflicted or divergent, and its subject.
const stackTemplate = `commit_id ++ "|" ++ change_id.short() ++ "|" ++ if(empty, "empty") ++ "|" ++ if(conflict, "conflict") ++ "|" ++ if(divergent, "divergent") ++ "|" ++ description.first_line() ++ "\n"`

// runTakeCase runs c with the take BesideTakeCommands renders. A pod is made from a new origin, and
// the fix's copy is made at the task's head. Where c has a tester, it commits red_test.txt in the
// shared copy and pushes its branch B with BesidePushCommand; where it has a second step, that step
// commits review.txt at the task's head and pushes its own branch. The fix commits fix.txt in its
// copy and runs the take; then c's steps; then the take again, in the same pod or in a fresh one.
// Each command runs as an agent's shell runs it, and the test logs what it printed.
func runTakeCase(t *testing.T, jj string, c takeCase) takeReading {
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
	reviewer := named(claim.BesideBranch(key, 1, "a", 1, claim.RoleReviewer))
	prefix := named(claim.BesideFixPrefix(key, 1, "a", 1))
	taskBranch := claim.IssueBranch(key)
	overHead := `remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")..@`

	reading := takeReading{bySubject: map[string]int{}, byContent: map[string]int{}}
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
		for _, command := range workspace.BesideTakeCommands(r.fixDir, prefix, own, taskBranch) {
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
	changes := map[string]string{} // by file, its step's commit
	if c.tester {
		red = pod.commit(pod.workspace, redFile, redSubject)
		changes[redFile] = red
		run(pod, pod.workspace, workspace.BesidePushCommand(tester))
	}
	if c.second {
		pod.jjIn(pod.clone, "new", head)
		changes[reviewFile] = pod.commit(pod.clone, reviewFile, reviewSubject)
		run(pod, pod.clone, "jj bookmark set "+reviewer+" -r @- && jj git push --remote origin --bookmark "+reviewer)
	}
	if c.mergeForward {
		pod.jjIn(pod.clone, "new", "main@origin")
		moved := pod.commit(pod.clone, "base.txt", "chore: the base moves")
		fixturegit.Isolated(t, t.TempDir()).Run(pod.clone, "git", "push", "--quiet", origin, moved+":refs/heads/main")
		run(pod, pod.fixDir, `jj -R "$LEGION_WORKSPACE-fix" git fetch --remote origin --branch main`)
		run(pod, pod.fixDir, `jj -R "$LEGION_WORKSPACE-fix" new @- main@origin -m 'merge: the base into the task'`)
		run(pod, pod.fixDir, `jj -R "$LEGION_WORKSPACE-fix" new`)
	}
	if !c.noOwn {
		pod.commit(pod.fixDir, "fix.txt", "fix: the failure")
	}

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

	steps := map[string]string{} // by file, its step's own change of it
	for file, commit := range changes {
		steps[file] = pod.jjIn(pod.workspace, "diff", "--git", "-r", commit, file)
	}
	for _, line := range reading.stack {
		fields := strings.SplitN(line, "|", 6)
		if len(fields) != 6 {
			t.Fatalf("jj printed %q for a commit of the fix's stack, want six fields", line)
		}
		reading.bySubject[fields[5]]++
		for file, change := range steps {
			if taker.jjIn(taker.fixDir, "diff", "--git", "-r", fields[0], file) == change {
				reading.byContent[file]++
			}
		}
		if fields[3] != "" {
			reading.conflicted++
		}
		if fields[4] != "" {
			reading.divergent++
		}
	}
	if output, err := taker.try(taker.fixDir, "log", "-r", "exactly(@-, 1)", "--no-graph", "-T", "commit_id"); err != nil {
		reading.under = output
	} else {
		reading.files = taker.filesOf(taker.fixDir, "@-")
		if slices.Contains(reading.files, redFile) {
			reading.red = taker.jjIn(taker.fixDir, "file", "show", "-r", "@-", redFile)
		}
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
// changed what it took. The take runs again too where two steps beside the fix pushed, with and
// without a commit of the fix's own, and where the fix's first act was a merge forward.
var takeCases = []takeCase{
	{name: "a resumed turn runs the take again", tester: true},
	{name: "the fix pushes its own branch, then runs the take again", tester: true, push: true},
	{name: "a fresh pod adopts the fix at its own branch and runs the take again", tester: true, push: true, fresh: true},
	{name: "the fix changes the tester's file, then runs the take again", tester: true, edit: true},
	{name: "with nothing to take, the take changes nothing"},
	{name: "two steps beside the fix pushed, then the take again", tester: true, second: true},
	{name: "two steps beside the fix pushed and the fix committed nothing, then the take again", tester: true, second: true, noOwn: true},
	{name: "the fix merged the base forward, then runs the take again", tester: true, mergeForward: true},
}

// A fix may run its take more than once in one fix, and each run must leave the fix's stack over
// the task's head holding each step's commit once, by its subject and by its change of its file,
// with no conflict, and the second run must change no commit of that stack. The commit under the
// fix's working copy, which it pushes and completes with, is one commit, and holds fix.txt, each
// step's file, red_test.txt as the fix last wrote it, and the base's file where the fix merged the
// base forward. No take rewrites the tester's commit: the shared copy still stands on it and is not
// stale. A take with nothing to take exits 0 and changes nothing.
func TestATakeRunAgainTakesEachCommitOnce(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	for _, c := range takeCases {
		t.Run(c.name, func(t *testing.T) {
			got := runTakeCase(t, jj, c)
			stack := strings.Join(got.stack, "\n")
			if slices.ContainsFunc(got.exits, func(status int) bool { return status != 0 }) {
				t.Errorf("the commands exited %v, want each 0", got.exits)
			}
			for _, step := range []struct {
				pushed        bool
				file, subject string
			}{{c.tester, redFile, redSubject}, {c.second, reviewFile, reviewSubject}} {
				want := 0
				if step.pushed {
					want = 1
				}
				if got.bySubject[step.subject] != want || got.byContent[step.file] != want {
					t.Errorf("the fix's stack over the task's head holds the commit of %s %d times by its subject and %d by its content, want %d:\n%s",
						step.file, got.bySubject[step.subject], got.byContent[step.file], want, stack)
				}
			}
			if got.conflicted != 0 || got.divergent != 0 {
				t.Errorf("the fix's stack holds %d conflicted and %d divergent commits, want none:\n%s", got.conflicted, got.divergent, stack)
			}
			if got.changed {
				t.Errorf("the second take changed the fix's stack, want it unchanged:\n%s", stack)
			}
			if !c.tester && got.firstChanged {
				t.Errorf("a take with nothing to take changed the fix's stack, want it unchanged:\n%s", stack)
			}
			if got.under != "" {
				t.Fatalf("the working copy of the fix stands on no one commit:\n%s\nits stack:\n%s", got.under, stack)
			}
			want := map[string]bool{"fix.txt": !c.noOwn, reviewFile: c.second, "base.txt": c.mergeForward}
			for file, held := range want {
				if slices.Contains(got.files, file) != held {
					t.Errorf("the commit under the fix's working copy holds %v, want %s in it: %v", got.files, file, held)
				}
			}
			wantRed := ""
			switch {
			case c.edit:
				wantRed = redEdit
			case c.tester:
				wantRed = redSubject
			}
			if got.red != wantRed {
				t.Errorf("the commit under the fix's working copy holds red_test.txt as %q, want %q", got.red, wantRed)
			}
			if got.shared != "" {
				t.Errorf("the shared copy is not as the tester left it: %s", got.shared)
			}
		})
	}
}

// A step that a way runs again after a fix ended takes the commits of its kept branch B that the
// task's branch does not hold (KeptTakeCommands), and its task is sent again on a resumed turn, so
// the step may run that take twice. Here the tester's first commit is under the fix's end head,
// as the fix's take leaves it, and the task's branch is at that head; the tester's later commit,
// red2.txt, is on B alone. The adoption moves the shared copy to the end head, and the step runs its
// take twice. Its stack over the task's head then holds the later commit once and the first commit
// not at all, with no conflict and no divergent commit; the second take changes nothing; and the
// commit under its working copy holds both files and the fix's.
func TestAKeptTakeRunAgainTakesEachCommitOnce(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	const (
		key           = "ACME-42"
		laterFile     = "red2.txt"
		laterSubject  = "test: the later red test"
		stackOverHead = `remote_bookmarks(exact:"legion/ACME-42", exact:"origin")..@`
	)
	tester, err := claim.BesideBranch(key, 1, "a", 1, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	taskBranch := claim.IssueBranch(key)
	origin := newOrigin(t, key)
	pod := newPodRig(t, jj, origin, key)
	var exits []int
	run := func(command string) {
		t.Helper()
		output, status := pod.shell(pod.workspace, command)
		t.Logf("exit %d: %s\n%s", status, command, output)
		exits = append(exits, status)
	}
	stackOf := func() []string {
		t.Helper()
		listed := pod.jjIn(pod.workspace, "log", "-r", stackOverHead, "--no-graph", "-T", stackTemplate)
		if listed == "" {
			return nil
		}
		return strings.Split(listed, "\n")
	}

	first := pod.commit(pod.workspace, redFile, redSubject)
	run(workspace.BesidePushCommand(tester))
	pod.jjIn(pod.clone, "new", first)
	end := pod.commit(pod.clone, "fix.txt", "fix: the failure")
	fixturegit.Isolated(t, t.TempDir()).Run(pod.clone, "git", "push", "--quiet", origin, end+":refs/heads/"+taskBranch)
	later := pod.commit(pod.workspace, laterFile, laterSubject)
	run(workspace.BesidePushCommand(tester))
	if got := pod.adopt("rerun", shimwire.AdoptWorkingCopy{MoveTo: end}); !got.OK || got.MovedTo != end {
		t.Fatalf("the rerun's adoption answered %#v, want ok with movedTo %s", got, end)
	}

	for _, command := range workspace.KeptTakeCommands(tester, taskBranch) {
		run(command)
	}
	before := stackOf()
	t.Logf("after the first take, the step's commits over the task's head:\n%s", strings.Join(before, "\n"))
	for _, command := range workspace.KeptTakeCommands(tester, taskBranch) {
		run(command)
	}
	stack := stackOf()
	listed := strings.Join(stack, "\n")
	t.Logf("after the second take, the step's commits over the task's head:\n%s\njj log:\n%s", listed,
		pod.jjIn(pod.workspace, "log", "-r", `remote_bookmarks(exact:"`+taskBranch+`", exact:"origin")::`))

	if slices.ContainsFunc(exits, func(status int) bool { return status != 0 }) {
		t.Errorf("the commands exited %v, want each 0", exits)
	}
	change := pod.jjIn(pod.workspace, "diff", "--git", "-r", later, laterFile)
	bySubject, byContent, conflicted, divergent := map[string]int{}, 0, 0, 0
	for _, line := range stack {
		fields := strings.SplitN(line, "|", 6)
		if len(fields) != 6 {
			t.Fatalf("jj printed %q for a commit of the step's stack, want six fields", line)
		}
		bySubject[fields[5]]++
		if pod.jjIn(pod.workspace, "diff", "--git", "-r", fields[0], laterFile) == change {
			byContent++
		}
		if fields[3] != "" {
			conflicted++
		}
		if fields[4] != "" {
			divergent++
		}
	}
	if bySubject[laterSubject] != 1 || byContent != 1 || bySubject[redSubject] != 0 {
		t.Errorf("the step's stack over the task's head holds the later commit %d times by its subject and %d by its content, and the first %d times, want 1, 1 and 0:\n%s",
			bySubject[laterSubject], byContent, bySubject[redSubject], listed)
	}
	if conflicted != 0 || divergent != 0 {
		t.Errorf("the step's stack holds %d conflicted and %d divergent commits, want none:\n%s", conflicted, divergent, listed)
	}
	if !slices.Equal(before, stack) {
		t.Errorf("the second take changed the step's stack, want it unchanged:\n%s", listed)
	}
	if output, err := pod.try(pod.workspace, "log", "-r", "exactly(@-, 1)", "--no-graph", "-T", "commit_id"); err != nil {
		t.Fatalf("the step's working copy stands on no one commit:\n%s", output)
	}
	if files := pod.filesOf(pod.workspace, "@-"); !slices.Contains(files, redFile) || !slices.Contains(files, laterFile) || !slices.Contains(files, "fix.txt") {
		t.Errorf("the commit under the step's working copy holds %v, want red_test.txt, red2.txt and fix.txt", files)
	}
}
