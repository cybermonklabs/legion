package shim_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The adoption of a step of a task whose branch is not the issue's runs in that task's working
// copy, beside the issue's own, and makes it first when it is not there: a workspace of the
// pod's clone on the task's branch as origin has it, whose working copy then carries the frame's
// identity. The issue's own working copy is left as it was, so no task's commits land in another
// task's working copy. Each working copy the shim makes is warmed for CodeGraph. A first give of
// a slug and a later give of one alike: the part after the key is read whole.
func TestTheAdoptionOfATasksBranchMakesItsWorkingCopy(t *testing.T) {
	testbin.RequireJJFilters(t)
	pod := newTaskPod(t)
	warmed := &warmedDirs{}
	p := pod.shim(t, warmed.record)

	for _, branch := range []string{"legion/INFRA-1.d", "legion/INFRA-1.b.2"} {
		got := pod.adopt(t, p, branch)
		if !got.OK {
			t.Fatalf("the adoption of %s answered %#v", branch, got)
		}
		dir, err := workspace.TaskDir(pod.issue, branch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("the adoption of %s made no working copy at %s: %v", branch, dir, err)
		}
		if author := pod.jj(t, dir, "log", "-R", dir, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "author.email()"); author != adoptEmail {
			t.Fatalf("the working copy of %s is authored by %q, want the frame's %s", branch, author, adoptEmail)
		}
		if parent := pod.jj(t, dir, "log", "-R", dir, "--ignore-working-copy", "-r", "@-", "--no-graph", "-T", "commit_id"); parent != pod.heads[branch] {
			t.Fatalf("the working copy of %s starts at %s, want the branch's head on origin %s", branch, parent, pod.heads[branch])
		}
		warmed.require(t, dir)
	}
	if author := pod.jj(t, pod.issue, "log", "-R", pod.issue, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "author.email()"); author != pod.email {
		t.Fatalf("the issue's own working copy is authored by %q after the adoptions of two tasks' steps, want %q as it was", author, pod.email)
	}
}

// The adoption of a step of the task that holds the issue's own branch — a frame with no branch,
// as every daemon before the branch sent, or one naming legion/<KEY> — runs in LEGION_WORKSPACE as
// it always has, and makes no working copy.
func TestTheAdoptionOfTheIssuesBranchIsTodays(t *testing.T) {
	testbin.RequireJJFilters(t)
	for _, branch := range []string{"", "legion/INFRA-1"} {
		t.Run("branch "+branch, func(t *testing.T) {
			pod := newTaskPod(t)
			p := pod.shim(t, nil)
			if got := pod.adopt(t, p, branch); !got.OK {
				t.Fatalf("the adoption answered %#v", got)
			}
			if author := pod.jj(t, pod.issue, "log", "-R", pod.issue, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "author.email()"); author != adoptEmail {
				t.Fatalf("the issue's own working copy is authored by %q, want the frame's %s", author, adoptEmail)
			}
			pod.requireNoTaskCopy(t)
		})
	}
}

// A frame naming a branch of another issue is refused by name, and makes nothing.
func TestTheAdoptionRefusesABranchOfAnotherIssue(t *testing.T) {
	testbin.RequireJJFilters(t)
	pod := newTaskPod(t)
	p := pod.shim(t, nil)
	if got := pod.adopt(t, p, "legion/OTHER-2.d"); got.OK || !strings.Contains(got.Error, "legion/OTHER-2.d") {
		t.Fatalf("the adoption of another issue's branch answered %#v, want a refusal naming it", got)
	}
	pod.requireNoTaskCopy(t)
}

const (
	adoptUser  = "legion-implementer[bot]"
	adoptEmail = "implementer@example.test"
)

// taskPod is an issue pod as provisioning lays it out (workspace.Location): the shared clone of a
// remote and the issue's working copy, a workspace of it, with the task branches the daemon made
// on the remote when each task started.
type taskPod struct {
	home, issue string
	user, email string
	heads       map[string]string
}

func newTaskPod(t *testing.T) *taskPod {
	t.Helper()
	root := t.TempDir()
	pod := &taskPod{home: filepath.Join(root, "home"), user: "tester", email: "tester@example.test", heads: map[string]string{}}
	remote, seed := filepath.Join(root, "remote"), filepath.Join(root, "seed")
	located, err := workspace.Location(filepath.Join(root, "state"), ghrepo.MustParse("acme/widgets"), "INFRA-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{pod.home, remote, seed, filepath.Dir(located.Clone), filepath.Dir(located.Dir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ProvisionTask runs its jj and git under the process's own environment (workspace's
	// execRunner), so the process reads the fixture's home and identity, as the shim does.
	for _, entry := range append(fixtureenv.Home(pod.home), "JJ_USER="+pod.user, "JJ_EMAIL="+pod.email) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	// That runner reaches a remote over https alone, and the fixture's remote is a local path, which
	// git reaches over its file transport: the git and jj the shim finds on PATH add that transport,
	// as the tools of the workspace package's own tests do (its testTools).
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"git", "jj"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is required: %v", tool, err)
		}
		script := "#!/bin/sh\n[ -z \"${GIT_ALLOW_PROTOCOL+set}\" ] || export GIT_ALLOW_PROTOCOL=\"$GIT_ALLOW_PROTOCOL:file\"\nexec '" + path + "' \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	pod.git(t, remote, fixturegit.InitBare(remote, "")[1:]...)
	pod.git(t, seed, fixturegit.InitWorkTree(seed, "main")[1:]...)
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pod.git(t, seed, "add", "-A")
	pod.git(t, seed, "commit", "--quiet", "-m", "one")
	pod.git(t, seed, "push", "--quiet", remote, "main", "main:refs/heads/legion/INFRA-1")
	pod.git(t, root, "clone", "--quiet", "--no-local", remote, located.Clone)
	pod.jj(t, located.Clone, fixturegit.JJInit(located.Clone, true)[1:]...)
	pod.jj(t, located.Clone, "bookmark", "track", "main@origin", "legion/INFRA-1@origin")
	pod.jj(t, located.Clone, "workspace", "add", "--name", filepath.Base(located.Dir), located.Dir)
	pod.issue = located.Dir
	// Each task's branch is made on the remote at its start, after the pod cloned it, with a
	// commit of its own.
	for _, task := range []string{"d", "b.2"} {
		branch := "legion/INFRA-1." + task
		if err := os.WriteFile(filepath.Join(seed, task+".txt"), []byte(task+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pod.git(t, seed, "add", "-A")
		pod.git(t, seed, "commit", "--quiet", "-m", "start "+task)
		pod.git(t, seed, "push", "--quiet", remote, "HEAD:refs/heads/"+branch)
		pod.heads[branch] = pod.git(t, seed, "rev-parse", "HEAD")
	}
	return pod
}

// shim runs a shim whose LEGION_WORKSPACE is the issue's working copy, with warm as its CodeGraph
// warm-up, and answers the daemon's end of its stream.
func (pod *taskPod) shim(t *testing.T, warm func(context.Context, procgroup.Starter, string, time.Time)) *peer {
	t.Helper()
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(pod.home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+pod.issue)
	cfg.WarmCodegraph = warm
	run(t, cfg, newClock())
	p := daemon.accept(t)
	p.open(t)
	return p
}

// adopt sends one adoption of branch under the fixture's App identity and answers the shim's
// result. Making a working copy fetches and checks out, so its answer is waited for longer than a
// frame the shim answers at once.
func (pod *taskPod) adopt(t *testing.T, p *peer, branch string) shimwire.AdoptWorkingCopyResult {
	t.Helper()
	p.send(t, shimwire.AdoptWorkingCopy{ID: "adopt " + branch, JJUser: adoptUser, JJEmail: adoptEmail, TimeoutMs: 120_000, Branch: branch})
	if err := p.conn.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	line, err := p.r.ReadLine()
	if err != nil {
		t.Fatalf("read the shim's answer to the adoption of %q: %v", branch, err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := frame.(shimwire.AdoptWorkingCopyResult)
	if !ok {
		t.Fatalf("the shim answered the adoption of %q with %#v", branch, frame)
	}
	return got
}

// requireNoTaskCopy fails the test unless the issue's working copy is still the only one in its
// directory of workspaces.
func (pod *taskPod) requireNoTaskCopy(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(pod.issue))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(pod.issue) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the workspaces beside the issue's are %v, want the issue's alone", names)
	}
}

func (pod *taskPod) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return pod.run(t, dir, append([]string{"git"}, args...))
}

func (pod *taskPod) jj(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return pod.run(t, dir, append([]string{"jj"}, args...))
}

func (pod *taskPod) run(t *testing.T, dir string, args []string) string {
	t.Helper()
	return fixturegit.Isolated(t, pod.home).With(
		"JJ_USER="+pod.user, "JJ_EMAIL="+pod.email,
		"GIT_AUTHOR_NAME="+pod.user, "GIT_AUTHOR_EMAIL="+pod.email,
		"GIT_COMMITTER_NAME="+pod.user, "GIT_COMMITTER_EMAIL="+pod.email,
	).Run(dir, args...)
}

// warmedDirs records each directory a shim's CodeGraph warm-up was started for.
type warmedDirs struct {
	mu   sync.Mutex
	dirs []string
}

func (w *warmedDirs) record(_ context.Context, _ procgroup.Starter, dir string, _ time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirs = append(w.dirs, dir)
}

// require fails the test unless a warm-up was started for dir exactly once: it waits for the first
// start (testwait.Until), then counts the starts for dir.
func (w *warmedDirs) require(t *testing.T, dir string) {
	t.Helper()
	dirs := testwait.Until(t, "the CodeGraph warm-up to start for "+dir, func() ([]string, bool) {
		w.mu.Lock()
		defer w.mu.Unlock()
		return slices.Clone(w.dirs), slices.Contains(w.dirs, dir)
	})
	if count := len(slices.DeleteFunc(slices.Clone(dirs), func(d string) bool { return d != dir })); count != 1 {
		t.Fatalf("the CodeGraph warm-up was started for %v, want %s once", dirs, dir)
	}
}
