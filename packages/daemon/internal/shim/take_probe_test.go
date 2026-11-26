package shim_test

// Scratch probe, never merged: each candidate take against each case, with jj's whole output.

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

func besideHeads(prefix, own string) string {
	return `remote_bookmarks(glob:"` + prefix + `*", exact:"origin") ~ remote_bookmarks(exact:"` + own + `", exact:"origin")`
}

func fetchBeside(dir, prefix string) string {
	return shellprefix.Command([]string{"jj", "-R", dir, "git", "fetch", "--remote", "origin", "--branch", prefix + "*"})
}

var probeCandidates = []struct {
	name string
	take takeCommands
}{
	// Names: no shell or revset characters, since each names a temporary directory the shim's
	// recording jj writes to unquoted.
	{"P1 duplicate base", workspace.BesideTakeCommands},
	{"P7 P6 with the task fetched and its head among the heads", func(dir, prefix, own, task string) []string {
		onto := `heads((::@ | remote_bookmarks(glob:"` + prefix + `*", exact:"origin") | remote_bookmarks(exact:"` + task + `", exact:"origin")) & ::(remote_bookmarks(glob:"*", exact:"origin") ~ remote_bookmarks(exact:"` + own + `", exact:"origin")))`
		return []string{shellprefix.Command([]string{"jj", "-R", dir, "git", "fetch", "--remote", "origin", "--branch", task, "--branch", prefix + "*"}),
			shellprefix.Command([]string{"jj", "-R", dir, "bookmark", "track", "exact:" + own, "--remote", "exact:origin"}),
			shellprefix.Command([]string{"jj", "-R", dir, "rebase", "-b", "@", "--onto", onto})}
	}},
	{"P6 P5 with own tracked first", func(dir, prefix, own, task string) []string {
		onto := `heads((::@ | remote_bookmarks(glob:"` + prefix + `*", exact:"origin")) & ::(remote_bookmarks(glob:"*", exact:"origin") ~ remote_bookmarks(exact:"` + own + `", exact:"origin")))`
		return []string{fetchBeside(dir, prefix),
			shellprefix.Command([]string{"jj", "-R", dir, "bookmark", "track", "exact:" + own, "--remote", "exact:origin"}),
			shellprefix.Command([]string{"jj", "-R", dir, "rebase", "-b", "@", "--onto", onto})}
	}},
}

type probeCase struct {
	takeCase
	second       bool // a second step beside the fix pushes a commit forked at the head
	pushFirst    bool // the fix pushes F before the first take and again after it
	noOwn        bool // the fix commits nothing of its own
	mergeForward bool // main moves on origin and the fix's first act is a merge of it
	promptRebase bool // before each take, the fix runs its prompt's fetch and rebase onto the task's head
	later        bool // after F's push, the tester commits red2.txt and pushes B again, before the second take
	pushAgain    bool // after the second take, the fix that ran it pushes F
	taskMoves    bool // after the first take, the task's branch on origin moves to a new commit over its head
}

var probeCases = []probeCase{
	{takeCase: takeCases[0]},
	{takeCase: takeCases[1]},
	{takeCase: takeCases[2]},
	{takeCase: takeCases[3]},
	{takeCase: takeCases[4]},
	{takeCase: takeCase{name: "F: two steps beside the fix, then the take again", tester: true}, second: true},
	{takeCase: takeCase{name: "G: the fix pushes F before the take and after it, then the take again", tester: true}, pushFirst: true},
	{takeCase: takeCase{name: "H: G in a fresh pod", tester: true, fresh: true}, pushFirst: true},
	{takeCase: takeCase{name: "I: a merge forward in the fix's stack, then the take again", tester: true}, mergeForward: true},
	{takeCase: takeCase{name: "J: no commit of the fix's own, one step beside, then the take again", tester: true}, noOwn: true},
	{takeCase: takeCase{name: "K: no commit of the fix's own, two steps beside, then the take again", tester: true}, noOwn: true, second: true},
	{takeCase: takeCase{name: "M: the prompt's rebase before each take", tester: true}, promptRebase: true},
	{takeCase: takeCase{name: "N: I with the prompt's rebase before each take", tester: true}, mergeForward: true, promptRebase: true},
	{takeCase: takeCase{name: "O: C with a later tester commit, then the fresh pod's take and its push of F", tester: true, push: true, fresh: true}, later: true, pushAgain: true},
	{takeCase: takeCase{name: "Q: B with a later tester commit, then the take and a push of F", tester: true, push: true}, later: true, pushAgain: true},
	{takeCase: takeCase{name: "R: the task's branch moves after the first take, then the take again", tester: true}, taskMoves: true},
	{takeCase: takeCase{name: "S: R with the prompt's rebase before each take", tester: true}, taskMoves: true, promptRebase: true},
}

func TestProbeTakes(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("jj version: %s", must(exec.Command(jj, "--version").CombinedOutput()))
	var summary []string
	for _, candidate := range probeCandidates {
		for _, c := range probeCases {
			t.Run(candidate.name+"/"+c.name, func(t *testing.T) {
				line := probeRun(t, jj, candidate.take, c)
				summary = append(summary, fmt.Sprintf("PROBE %-45s | %-75s | %s", candidate.name, c.name, line))
			})
		}
	}
	t.Logf("summary:\n%s", strings.Join(summary, "\n"))
	t.Run("jj facts", func(t *testing.T) { probeFacts(t, jj) })
}

func must(out []byte, err error) string {
	if err != nil {
		return fmt.Sprintf("%v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func probeRun(t *testing.T, jj string, take takeCommands, c probeCase) string {
	const key = "ACME-42"
	own, _ := claim.BesideBranch(key, 1, "a", 1, claim.RoleImplementer)
	tester, _ := claim.BesideBranch(key, 1, "a", 1, claim.RoleTester)
	reviewer, _ := claim.BesideBranch(key, 1, "a", 1, claim.RoleReviewer)
	prefix, _ := claim.BesideFixPrefix(key, 1, "a", 1)
	taskBranch := claim.IssueBranch(key)
	overHead := `remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")..@`
	var exits []string
	run := func(r *copyRig, dir, command string) {
		output, status := r.shell(dir, command)
		t.Logf("exit %d: %s\n%s", status, command, output)
		exits = append(exits, fmt.Sprint(status))
	}
	stackOf := func(r *copyRig) []string {
		listed, err := r.try(r.fixDir, "log", "-r", overHead, "--no-graph", "-T", stackTemplate)
		if err != nil {
			return []string{"ERR " + listed}
		}
		if listed == "" {
			return nil
		}
		return strings.Split(listed, "\n")
	}
	opOf := func(r *copyRig) string {
		out, _ := r.try(r.fixDir, "op", "log", "-n1", "--no-graph", "-T", "id.short()")
		return out
	}
	runTake := func(r *copyRig) {
		if c.promptRebase {
			run(r, r.fixDir, `jj -R "$LEGION_WORKSPACE-fix" git fetch`)
			run(r, r.fixDir, `jj -R "$LEGION_WORKSPACE-fix" rebase -b @- -d '`+taskBranch+`@origin'`)
		}
		for _, command := range take(r.fixDir, prefix, own, taskBranch) {
			run(r, r.fixDir, command)
		}
	}
	pushOwn := func(r *copyRig) {
		run(r, r.fixDir, `jj -R "$LEGION_WORKSPACE-fix" bookmark set `+own+` -r @-`)
		run(r, r.fixDir, `jj -R "$LEGION_WORKSPACE-fix" git push --remote origin --bookmark `+own)
	}

	origin := newOrigin(t, key)
	pod := newPodRig(t, jj, origin, key)
	head := pod.head
	if got := pod.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: own}); !got.OK {
		t.Fatalf("adoption %#v", got)
	}
	red := head
	if c.tester {
		red = pod.commit(pod.workspace, redFile, redSubject)
		run(pod, pod.workspace, workspace.BesidePushCommand(tester))
	}
	if c.second {
		pod.jjIn(pod.clone, "new", head)
		pod.commit(pod.clone, "review.txt", "test: the reviewer's case")
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
	if c.pushFirst {
		pushOwn(pod)
	}
	first := stackOf(pod)
	runTake(pod)
	afterFirst := stackOf(pod)
	t.Logf("after the first take:\n%s", strings.Join(afterFirst, "\n"))
	if c.pushFirst {
		pushOwn(pod)
	}
	if c.edit {
		pod.commit(pod.fixDir, redFile, redEdit)
	}
	if c.push {
		pushOwn(pod)
	}
	standing := red
	if c.later {
		standing = pod.commit(pod.workspace, "red2.txt", "test: the later red test")
		run(pod, pod.workspace, workspace.BesidePushCommand(tester))
	}
	if c.taskMoves {
		pod.jjIn(pod.clone, "new", head)
		moved := pod.commit(pod.clone, "task2.txt", "feat: the task moves")
		fixturegit.Isolated(t, t.TempDir()).Run(pod.clone, "git", "push", "--quiet", origin, moved+":refs/heads/"+taskBranch)
	}
	taker := pod
	if c.fresh {
		taker = newPodRig(t, jj, origin, key)
		if got := taker.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: own}); !got.OK {
			t.Fatalf("fresh adoption %#v", got)
		}
		t.Logf("the fresh pod's fix copy over the task's head:\n%s", strings.Join(stackOf(taker), "\n"))
	}
	before, opBefore := stackOf(taker), opOf(taker)
	runTake(taker)
	after, opAfter := stackOf(taker), opOf(taker)
	if c.pushAgain {
		pushOwn(taker)
	}
	graph, _ := taker.try(taker.fixDir, "log", "-r", `remote_bookmarks(exact:"`+taskBranch+`", exact:"origin")::`)
	t.Logf("after the second take:\n%s\njj log:\n%s", strings.Join(after, "\n"), graph)
	testers := ""
	if c.tester {
		testers, _ = pod.try(pod.workspace, "diff", "--git", "-r", red, redFile)
	}
	bySubject, byContent, conflicted, divergent, empty := 0, 0, 0, 0, 0
	for _, line := range after {
		fields := strings.SplitN(line, "|", 6)
		if len(fields) != 6 {
			continue
		}
		if fields[5] == redSubject {
			bySubject++
			if fields[2] != "" {
				empty++
			}
		}
		if c.tester {
			if diff, _ := taker.try(taker.fixDir, "diff", "--git", "-r", fields[0], redFile); diff == testers {
				byContent++
			}
		}
		if fields[3] != "" {
			conflicted++
		}
		if fields[4] != "" {
			divergent++
		}
	}
	files, _ := taker.try(taker.fixDir, "file", "list", "-r", "@-")
	redAt, _ := taker.try(taker.fixDir, "file", "show", "-r", "@-", redFile)
	shared := "fresh"
	if status, err := pod.try(pod.workspace, "status"); err != nil || strings.Contains(status, "stale") {
		shared = "STALE: " + status
	} else if parent, _ := pod.try(pod.workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id"); parent != standing {
		shared = "MOVED to " + parent
	}
	ownAt, _ := taker.try(taker.fixDir, "file", "list", "-r", own+"@origin")
	return fmt.Sprintf("exits=%s bySubject=%d (empty %d) byContent=%d conflicted=%d divergent=%d firstChanged=%v secondChanged=%v opChanged=%v @-files=[%s] red@-=%q shared=%s F@origin-files=[%s]",
		strings.Join(exits, ","), bySubject, empty, byContent, conflicted, divergent, !slices.Equal(first, afterFirst), !slices.Equal(before, after), opBefore != opAfter,
		strings.Join(strings.Fields(files), " "), redAt, shared, strings.Join(strings.Fields(ownAt), " "))
}

// probeFacts reads what jj records of a duplicate and how it answers an empty -A.
func probeFacts(t *testing.T, jj string) {
	r := newCopyRig(t)
	if got := r.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: r.head}); !got.OK {
		t.Fatalf("adoption %#v", got)
	}
	red := r.commit(r.workspace, redFile, redSubject)
	r.commit(r.fixDir, "fix.txt", "fix: the failure")
	out, err := r.try(r.fixDir, "duplicate", red, "--insert-before", "@")
	t.Logf("duplicate: %v\n%s", err, out)
	out, err = r.try(r.fixDir, "evolog", "-r", "@-")
	t.Logf("evolog of the copy: %v\n%s", err, out)
	out, err = r.try(r.fixDir, "log", "-r", "@-", "--no-graph", "-T", `change_id ++ " " ++ commit_id`)
	t.Logf("copy ids: %v %s; original %s", err, out, red)
	out, err = r.try(r.fixDir, "new", "--insert-after", "@-", "--insert-after", "none()", "--insert-before", "@")
	t.Logf("new -A @- -A none() -B @: %v\n%s", err, out)
	out, err = r.try(r.fixDir, "rebase", "-b", "@", "--onto", "@--")
	t.Logf("rebase -b @ --onto @-- (already in place): %v\n%s", err, out)
	out, err = r.try(r.fixDir, "rebase", "-b", "@", "--onto", "none()")
	t.Logf("rebase -b @ --onto none(): %v\n%s", err, out)
	out, err = r.try(r.fixDir, "log", "-r", "predecessors(@-)")
	t.Logf("a predecessors revset: %v\n%s", err, out)
	out, err = r.try(r.fixDir, "bookmark", "track", "exact:legion-beside/ACME-42/1/a/fix-1/implementer", "--remote", "exact:origin")
	t.Logf("bookmark track of a branch origin does not hold: %v\n%s", err, out)
	out, err = r.try(r.fixDir, "op", "log", "-n3")
	t.Logf("op log: %v\n%s", err, out)
}
