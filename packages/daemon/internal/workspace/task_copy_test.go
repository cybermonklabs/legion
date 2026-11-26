package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The working copy of a task of an issue's plan is a jj workspace of the clone the issue's own
// working copy is a workspace of, beside it, on the task's branch as origin has it: a first give
// of a slug and a later give of one alike.
func TestATasksWorkingCopyIsAWorkspaceOfTheIssuesClone(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	issue, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	for _, task := range []struct{ branch, dir, file string }{
		{branch: "legion/WIDGETS-42.d", dir: "widgets-42.d", file: "d.txt"},
		{branch: "legion/WIDGETS-42.b.2", dir: "widgets-42.b.2", file: "b.txt"},
	} {
		head := pushRemoteBranch(t, run.remote, task.branch, task.file)
		working, err := ProvisionTask(context.Background(), run, issue.Dir, task.branch, req.Log)
		if err != nil {
			t.Fatalf("ProvisionTask(%s): %v", task.branch, err)
		}
		if want := filepath.Join(filepath.Dir(issue.Dir), task.dir); working.Dir != want {
			t.Errorf("the working copy of %s is at %s, want %s beside the issue's", task.branch, working.Dir, want)
		}
		if working.Clone != issue.Clone || working.Bookmark != task.branch {
			t.Errorf("the working copy of %s is a workspace of %s on %s, want of the issue's clone %s on its branch", task.branch, working.Clone, working.Bookmark, issue.Clone)
		}
		if parent := commitOf(t, working.Dir, "@-"); parent != head {
			t.Errorf("the working copy of %s starts at %s, want origin's head of the branch %s", task.branch, parent, head)
		}
		if _, err := os.Stat(filepath.Join(working.Dir, task.file)); err != nil {
			t.Errorf("the working copy of %s holds no checkout of its branch: %v", task.branch, err)
		}
	}
	listed := runSetup(t, issue.Clone, "jj", "workspace", "list", "--ignore-working-copy", "--color=never", "-R", issue.Clone)
	var names []string
	for _, line := range nonEmptyLines(listed) {
		name, _, _ := strings.Cut(line, ":")
		names = append(names, name)
	}
	for _, want := range []string{"widgets-42", "widgets-42.d", "widgets-42.b.2"} {
		if !slices.Contains(names, want) {
			t.Errorf("the clone's workspaces are %q, want %s among them", names, want)
		}
	}
	if parent, main := commitOf(t, issue.Dir, "@-"), commitOf(t, issue.Clone, "main@origin"); parent != main {
		t.Errorf("the issue's own working copy moved to %s, want it on main's %s", parent, main)
	}
}

// A task's branch that origin does not have makes no working copy: the branch is on GitHub from
// the task's start, so its absence is refused by name, and no workspace starts at the default
// branch in its place.
func TestATasksBranchThatOriginLacksMakesNoWorkingCopy(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	issue, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	_, err = ProvisionTask(context.Background(), run, issue.Dir, "legion/WIDGETS-42.e", req.Log)
	if err == nil || !strings.Contains(err.Error(), "legion/WIDGETS-42.e") {
		t.Fatalf("ProvisionTask of a branch origin lacks = %v, want a refusal naming the branch", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(issue.Dir), "widgets-42.e")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused working copy's directory = %v, want none", statErr)
	}
}

// The directory of a task's working copy is the issue's own for the issue's branch, and the
// directory beside it named for the branch otherwise; a branch of no task of that issue has none.
func TestTaskDirIsBesideTheIssuesWorkspace(t *testing.T) {
	issue, err := Location(filepath.Join(t.TempDir(), "state"), ghrepo.MustParse("acme/widgets"), "WIDGETS-42")
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Dir(issue.Dir)
	for branch, want := range map[string]string{
		"":                       issue.Dir,
		"legion/WIDGETS-42":      issue.Dir,
		"legion/WIDGETS-42.d":    filepath.Join(beside, "widgets-42.d"),
		"legion/WIDGETS-42.b.2":  filepath.Join(beside, "widgets-42.b.2"),
		"legion/WIDGETS-42.t-0a": filepath.Join(beside, "widgets-42.t-0a"),
	} {
		got, err := TaskDir(issue.Dir, branch)
		if err != nil || got != want {
			t.Errorf("TaskDir(%q) = %q, %v; want %q", branch, got, err, want)
		}
	}
	for _, branch := range []string{
		"main",
		"legion/WIDGETS-43.d",
		"legion/WIDGETS-42.d/../x",
		"legion/WIDGETS-42.D",
		"legion/WIDGETS-42.removing",
	} {
		if got, err := TaskDir(issue.Dir, branch); err == nil {
			t.Errorf("TaskDir(%q) = %q, want a refusal", branch, got)
		}
	}
	if got, err := TaskDir(filepath.Join(t.TempDir(), "elsewhere"), "legion/WIDGETS-42.d"); err == nil {
		t.Errorf("TaskDir of a directory that is no issue's working copy = %q, want a refusal", got)
	}
	for branch, want := range map[string]string{"legion/WIDGETS-42": "WIDGETS-42", "legion/WIDGETS-42.d": "WIDGETS-42.d", "legion/WIDGETS-42.b.2": "WIDGETS-42.b.2"} {
		if got, err := Name(branch); err != nil || got != want {
			t.Errorf("Name(%q) = %q, %v; want %q", branch, got, err, want)
		}
	}
	for _, branch := range []string{"", "main", "legion/WIDGETS-42.d/../x"} {
		if got, err := Name(branch); err == nil {
			t.Errorf("Name(%q) = %q, want a refusal", branch, got)
		}
	}
}

// The working copies of an issue's tasks are the ones beside the issue's own (TaskCopies), and
// Remove takes one of them whole and nothing of the issue's or another task's: of an issue with
// the working copies of legion/WIDGETS-42.b and legion/WIDGETS-42.c.2 beside its own, and the
// directory of another issue's task (widgets-420.b), the two are listed, the issue's own and the
// other issue's are not; Remove of b's deletes its directory, forgets its jj workspace and deletes
// its git worktree entry, and c's working copy and the issue's own stand with their files, their
// workspaces and their entries. A workspace that is not on its issue's own branch is refused.
//
// The fault it catches is the working copy of a task left on the host once its issue is removed,
// and a removal that reaches past one working copy (its control: TaskCopies comparing the name
// with the issue's key and no dot, which lists widgets-420.b too).
func TestTheWorkingCopiesOfAnIssuesTasksAreListedAndEachIsRemovedAlone(t *testing.T) {
	ctx := context.Background()
	run := newLocalRunner(t)
	req := provisionRequest(t)
	issue, err := Provision(ctx, run, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	for _, task := range []struct{ branch, file string }{{"legion/WIDGETS-42.b", "b.txt"}, {"legion/WIDGETS-42.c.2", "c.txt"}} {
		pushRemoteBranch(t, run.remote, task.branch, task.file)
		if _, err := ProvisionTask(ctx, run, issue.Dir, task.branch, req.Log); err != nil {
			t.Fatalf("ProvisionTask(%s): %v", task.branch, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(issue.Dir), "widgets-420.b"), 0o755); err != nil {
		t.Fatal(err)
	}
	names := func(copies []Workspace) []string {
		var names []string
		for _, working := range copies {
			names = append(names, filepath.Base(working.Dir))
		}
		return names
	}
	copies, err := TaskCopies(issue)
	if err != nil || !slices.Equal(names(copies), []string{"widgets-42.b", "widgets-42.c.2"}) {
		t.Fatalf("TaskCopies = %v, %v; want widgets-42.b and widgets-42.c.2", names(copies), err)
	}
	if copies[0].Clone != issue.Clone || copies[0].Bookmark != "legion/WIDGETS-42.b" {
		t.Fatalf("the working copy of b is listed as %+v, want a workspace of the issue's clone %s on legion/WIDGETS-42.b", copies[0], issue.Clone)
	}

	if err := Remove(ctx, run, copies[0], nil); err != nil {
		t.Fatalf("Remove the working copy of b: %v", err)
	}
	if _, err := os.Lstat(copies[0].Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the working copy of b after its removal = %v, want no directory", err)
	}
	listed := runSetup(t, issue.Clone, "jj", "workspace", "list", "--ignore-working-copy", "--color=never", "-R", issue.Clone)
	var workspaces []string
	for _, line := range nonEmptyLines(listed) {
		name, _, _ := strings.Cut(line, ":")
		workspaces = append(workspaces, name)
	}
	if slices.Contains(workspaces, "widgets-42.b") || !slices.Contains(workspaces, "widgets-42") || !slices.Contains(workspaces, "widgets-42.c.2") {
		t.Errorf("the clone's workspaces after the removal of b's = %q, want widgets-42 and widgets-42.c.2 and not widgets-42.b", workspaces)
	}
	entries := gitWorktreeLocks(t, issue.Clone)
	if _, registered := entries[copies[0].Dir]; registered {
		t.Errorf("the clone's git worktree entries after the removal of b's = %v, want none of %s", entries, copies[0].Dir)
	}
	for _, dir := range []string{issue.Dir, copies[1].Dir} {
		if _, registered := entries[dir]; !registered {
			t.Errorf("the clone's git worktree entries after the removal of b's = %v, want %s's kept", entries, dir)
		}
	}
	if _, err := os.Stat(filepath.Join(copies[1].Dir, "c.txt")); err != nil {
		t.Errorf("the working copy of c after the removal of b's: %v, want its checkout whole", err)
	}
	if again, err := TaskCopies(issue); err != nil || !slices.Equal(names(again), []string{"widgets-42.c.2"}) {
		t.Errorf("TaskCopies after the removal of b's = %v, %v; want widgets-42.c.2 alone", names(again), err)
	}

	odd := issue
	odd.Bookmark = "legion/WIDGETS-42.b"
	if got, err := TaskCopies(odd); err == nil {
		t.Errorf("TaskCopies of a workspace on a task's branch = %v, want a refusal", names(got))
	}
}

// The variables TestHelperProvision reads: which of the two processes it is, and what it works on.
// Each begins LEGION_TEST_, which the package's TestMain leaves to a child (fixtureenv.Guard).
const (
	helperRoleEnv    = "LEGION_TEST_TASK_COPY_ROLE"
	helperHomeEnv    = "LEGION_TEST_TASK_COPY_HOME"
	helperLogEnv     = "LEGION_TEST_TASK_COPY_LOG"
	helperReleaseEnv = "LEGION_TEST_TASK_COPY_RELEASE"
	helperIssueEnv   = "LEGION_TEST_TASK_COPY_ISSUE"
	helperBranchEnv  = "LEGION_TEST_TASK_COPY_BRANCH"
)

// Two processes that add a working copy to one clone take turns: a process that holds the clone's
// lock (CloneLock), as a pane's shim or the daemon does for the length of what it adds, keeps a
// second process's ProvisionTask of the same clone waiting until it lets go. The second is read as
// waiting from the kernel's own list of waiting locks (/proc/locks), so the row does not rest on a
// time the second could take under load.
func TestTwoProvisionsOfOneCloneTakeTurns(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	issue, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	pushRemoteBranch(t, run.remote, "legion/WIDGETS-42.d", "d.txt")
	scratch := t.TempDir()
	log, release := filepath.Join(scratch, "log"), filepath.Join(scratch, "release")
	helper := func(role string) *exec.Cmd {
		command := exec.Command(os.Args[0], "-test.run=^TestHelperProvision$", "-test.count=1")
		command.Env = append(os.Environ(), helperRoleEnv+"="+role, helperHomeEnv+"="+os.Getenv("HOME"), helperLogEnv+"="+log,
			helperReleaseEnv+"="+release, helperIssueEnv+"="+issue.Dir, helperBranchEnv+"=legion/WIDGETS-42.d")
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		return command
	}

	// A failure on the way still lets the holder go, so neither process outlives the row.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	first := helper("hold")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, log, "the first holding the lock", func(lines []string) bool { return slices.Contains(lines, "first holds") })
	second := helper("provision")
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	// Either the second is seen waiting on a lock, or it went on without one: the line it writes
	// once it has ended, its working copy there or not, ends the wait too, and the order below
	// fails.
	waitForLog(t, log, "the second waiting on a lock or ended", func(lines []string) bool {
		return slices.ContainsFunc(lines, func(line string) bool { return line == "second added" || strings.HasPrefix(line, "second failed") }) ||
			slices.Contains(lines, "second asks") && waitsOnALock(t, second.Process.Pid)
	})
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Errorf("the process that held the lock: %v", err)
	}
	if err := second.Wait(); err != nil {
		t.Errorf("the process that provisioned: %v", err)
	}
	written, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nonEmptyLines(string(written)), []string{"first holds", "second asks", "first lets go", "second added"}; !slices.Equal(got, want) {
		t.Errorf("the two processes logged %q, want %q: the second added its working copy while the first held the clone's lock", got, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(issue.Dir), "widgets-42.d")); err != nil {
		t.Errorf("the second's working copy: %v", err)
	}
}

// TestHelperProvision is the child process of TestTwoProvisionsOfOneCloneTakeTurns: "hold" takes
// the clone's lock and keeps it until the release file is there, and "provision" adds the task's
// working copy with ProvisionTask. Run any other way, it does nothing.
func TestHelperProvision(t *testing.T) {
	role := os.Getenv(helperRoleEnv)
	if role == "" {
		t.Skip("the child process of TestTwoProvisionsOfOneCloneTakeTurns")
	}
	for _, entry := range fixtureenv.Home(os.Getenv(helperHomeEnv)) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	t.Setenv("JJ_USER", "Legion test")
	t.Setenv("JJ_EMAIL", "legion-test@example.invalid")
	log := os.Getenv(helperLogEnv)
	issue, branch := os.Getenv(helperIssueEnv), os.Getenv(helperBranchEnv)
	clone, err := locationOf(issue)
	if err != nil {
		t.Fatal(err)
	}
	switch role {
	case "hold":
		lock := CloneLock(clone.Clone)
		lock.Lock()
		appendLine(t, log, "first holds")
		deadline := time.Now().Add(5 * time.Minute)
		for {
			if _, err := os.Stat(os.Getenv(helperReleaseEnv)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no release came")
			}
			time.Sleep(10 * time.Millisecond)
		}
		appendLine(t, log, "first lets go")
		lock.Unlock()
	case "provision":
		appendLine(t, log, "second asks")
		if _, err := ProvisionTask(context.Background(), NewRunner(procgroup.NoReaper, testTimeout, testTools(t)), issue, branch, func(line string) { t.Log(line) }); err != nil {
			appendLine(t, log, "second failed: "+err.Error())
			t.Fatalf("ProvisionTask: %v", err)
		}
		appendLine(t, log, "second added")
	default:
		t.Fatalf("no helper role %q", role)
	}
}

// appendLine appends line to the log the two processes share; each write is one append, so the
// file holds the lines in the order they were written.
func appendLine(t *testing.T, log, line string) {
	t.Helper()
	file, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// waitForLog waits until done answers true of the lines log holds, for at most two minutes, the
// time a child process of a loaded machine may take to start; what names what it waits for.
func waitForLog(t *testing.T, log, what string, done func(lines []string) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var lines []string
	for time.Now().Before(deadline) {
		written, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if lines = nonEmptyLines(string(written)); done(lines) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited two minutes for %s; the log holds %q", what, lines)
}

// waitsOnALock is whether the process pid waits for a flock, as the kernel lists it: a line of
// /proc/locks whose lock is marked `->`, a request blocked behind another's.
func waitsOnALock(t *testing.T, pid int) bool {
	t.Helper()
	locks, err := os.ReadFile("/proc/locks")
	if err != nil {
		t.Fatalf("read the kernel's list of locks: %v", err)
	}
	for _, line := range strings.Split(string(locks), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 5 && fields[1] == "->" && fields[2] == "FLOCK" && fields[5] == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}
