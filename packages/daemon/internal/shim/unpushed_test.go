package shim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A project release deletes every issue pod with its disk, and asks a pod's roles to push first
// only when the pod's workspace holds the issue's unpushed work. Each case is a real workspace with
// a remote, read by the real shim as the release reads it, and the answer is held to the read's two
// facts: the line the shim wrote is the frame's own members and nothing else, so no commit, file or
// description of the workspace rides along. The read runs no git and sends no commit: every git
// the shim starts is recorded (recordingGit), and the read's record stays empty.
//
// The first cases are in the layout a pod's provisioning makes (podWorkspace). The first is what
// every pod reads when its roles have pushed everything: the pod's shared clone holds a workspace
// of its own whose working copy is an empty commit no remote holds, and a read that did not
// subtract each commit with no change and no description would count it.
//
// The cases after them are made as TestWorkspaceWorkReadsTheIssuePodLayout makes its own
// (workFixture), in a clone that keeps every head visible (git.abandon-unreachable-commits =
// false), and each first holds its head to be a visible head (requireVisibleHead), so a head read
// as nothing is one the read left out, not one jj hid. They hold the spec's rule for a head that no
// working copy and no bookmark reaches: it is the issue's work when it has a change and no
// description, or, until the issue's pull request merges, when its `Omp-Session` trailer names a
// session the read is given or its `Dispatch` trailer names the issue; any other is another's.
func TestTheShimReadsUnpushedWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		// work leaves the case's work in the pod's issue workspace, or in its shared clone.
		work func(f *podWorkspace)
		want shim.Unpushed
	}{
		{
			name: "a working copy with no change on the pushed issue branch reads as nothing unpushed",
			work: func(*podWorkspace) {},
		},
		{
			name: "a working-copy change reads as one commit with the working copy marked changed",
			work: func(f *podWorkspace) { f.write(filepath.Join(f.ws, "work.txt"), "work\n") },
			want: shim.Unpushed{Commits: 1, WorkingCopyChanged: true},
		},
		{
			name: "an unpushed commit under a working copy with no change reads as one commit",
			work: func(f *podWorkspace) {
				f.write(filepath.Join(f.ws, "work.txt"), "work\n")
				f.run(f.ws, "jj", "commit", "-R", f.ws, "-m", "unpushed work")
			},
			want: shim.Unpushed{Commits: 1},
		},
		{
			name: "a commit pushed to the issue's branch reads as nothing unpushed",
			work: func(f *podWorkspace) {
				f.write(filepath.Join(f.ws, "work.txt"), "work\n")
				f.run(f.ws, "jj", "commit", "-R", f.ws, "-m", "pushed work")
				f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, "legion/"+unpushedIssue, "-r", "@-")
				f.run(f.ws, "jj", "git", "push", "-R", f.ws, "--bookmark", "legion/"+unpushedIssue)
			},
		},
		{
			name: "a commit pushed to another branch of the remote reads as nothing unpushed",
			work: func(f *podWorkspace) {
				f.write(filepath.Join(f.ws, "work.txt"), "work\n")
				f.run(f.ws, "jj", "commit", "-R", f.ws, "-m", "work on another branch")
				f.run(f.ws, "jj", "git", "push", "-R", f.ws, "--named", "side=@-")
			},
		},
		{
			// Something wrote a file into the shared clone's own working copy: a commit is left
			// out of the count only when it has no change and no description, whichever
			// workspace's working copy it is, and this one has a change.
			name: "a change in the shared clone's own working copy reads as one commit",
			work: func(f *podWorkspace) {
				f.run(f.clone, "jj", "sparse", "set", "--add", "clone-only.txt", "-R", f.clone)
				f.write(filepath.Join(f.clone, "clone-only.txt"), "written in the shared clone\n")
				f.run(f.clone, "jj", "status", "-R", f.clone)
			},
			want: shim.Unpushed{Commits: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPodWorkspace(t)
			tc.work(f)
			requireUnpushed(t, f.home, f.ws, f.env, unpushedRequest("read-1"), tc.want)
		})
	}

	for _, tc := range []struct {
		name string
		// head leaves the case's head in the issue workspace's clone, with no working copy and no
		// bookmark on it, and answers its commit.
		head func(f *workFixture) string
		// merged is whether the read says the issue's pull request merged.
		merged bool
		want   int
	}{
		{
			// The trailer's session differs from unpushedSession only where that one has a `.`,
			// so a read that left the `.` unquoted in its pattern would count it.
			name: "a described head a fetch left after the remote deleted its branch, whose Omp-Session trailer names another session, reads as nothing unpushed",
			head: func(f *workFixture) string {
				return fetchLeftHead(f, "another issue's work\n\nOmp-Session: "+anotherSession)
			},
		},
		{
			name: "a described head with no bookmark and no working copy whose Omp-Session trailer names a session given for the issue reads as one commit",
			head: func(f *workFixture) string {
				return describedSideHead(f, "the issue's work\n\nOmp-Session: "+unpushedSession)
			},
			want: 1,
		},
		{
			name: "a described head with no bookmark and no working copy whose Dispatch trailer names the issue reads as one commit",
			head: func(f *workFixture) string {
				return describedSideHead(f, "the issue's work\n\nDispatch: "+workIssue)
			},
			want: 1,
		},
		{
			// Another issue whose key starts with this one's: the trailer's whole value must be
			// the issue's key.
			name: "a described head with no bookmark and no working copy whose Dispatch trailer names another issue reads as nothing unpushed",
			head: func(f *workFixture) string {
				return describedSideHead(f, "another issue's work\n\nDispatch: "+workIssue+"0")
			},
		},
		{
			name:   "the head whose Omp-Session trailer names a session given for the issue reads as nothing unpushed once the pull request merged",
			merged: true,
			head: func(f *workFixture) string {
				return describedSideHead(f, "the issue's work\n\nOmp-Session: "+unpushedSession)
			},
		},
		{
			name:   "the head whose Dispatch trailer names the issue reads as nothing unpushed once the pull request merged",
			merged: true,
			head: func(f *workFixture) string {
				return describedSideHead(f, "the issue's work\n\nDispatch: "+workIssue)
			},
		},
		{
			name: "a head with a change and no description, with no bookmark and no working copy, reads as one commit",
			head: func(f *workFixture) string { return f.sideHead() },
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnpushedWorkFixture(t)
			f.requireVisibleHead(tc.head(f), "the case's head")
			request := shimwire.ReadUnpushed{ID: "read-1", Issue: workIssue, Merged: tc.merged,
				Sessions: []string{unpushedSession, "ses_reviewer"}, TimeoutMs: 120_000}
			requireUnpushed(t, f.home, f.ws, unpushedEnv(f.home), request, shim.Unpushed{Commits: tc.want})
		})
	}

	t.Run("a shim with no workspace to read refuses the read by name", func(t *testing.T) {
		path := socketPath(t)
		daemon := listen(t, path)
		child := newOMP(t)
		cfg := config(t, path, child)
		cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker, "FAKE_OMP_LOG="+child.log)
		run(t, cfg, newClock())
		p := daemon.accept(t)
		p.open(t)
		got := readUnpushed(t, p, unpushedRequest("read-2"))
		const refusal = "worker-shim: LEGION_WORKSPACE is not set; no workspace to read for unpushed work"
		if got.OK || got.Error != refusal {
			t.Fatalf("the shim answered %#v, want a refusal reading %q and no answer that nothing is unpushed", got, refusal)
		}
	})
}

// unpushedIssue is the issue the fixture's pod works on, whose branch the remote holds.
const unpushedIssue = "INFRA-945"

// unpushedSession is a session the read is given as one a claim of the issue holds. Its `.` is the
// one character a session id can hold (claim.SessionIDRule) that a regular expression reads as
// more than itself, and anotherSession is another issue's session that differs from it only there.
const (
	unpushedSession = "019a3f2e.implementer"
	anotherSession  = "019a3f2eXimplementer"
)

// unpushedRequest is the read-unpushed the release sends for the pod's issue: no session names,
// the pull request not merged, and a budget of two minutes.
func unpushedRequest(id string) shimwire.ReadUnpushed {
	return shimwire.ReadUnpushed{ID: id, Issue: unpushedIssue, TimeoutMs: 120_000}
}

// podWorkspace is an issue pod's workspace in the layout provisioning makes, and the remote it was
// cloned from: a bare remote holding the default branch and the issue's branch; the pod's shared
// clone, made as workspace's feedClone makes one (`git clone --no-checkout`, `jj git init
// --colocate`, its sparse patterns cleared and its first snapshot abandoned), which leaves the
// clone's own `default` workspace on an empty working copy no remote holds; and the issue's
// workspace added to that clone on the issue's branch with no files and then checked out, as
// Provision adds one. Every command is real git or jj under fixtureenv.Isolated of a home of the
// fixture's own and names the directory it runs in (docs/testing.md).
type podWorkspace struct {
	t                       *testing.T
	home, remote, clone, ws string
	env                     []string
}

func newPodWorkspace(t *testing.T) *podWorkspace {
	t.Helper()
	requireJJAndGit(t)
	root := t.TempDir()
	f := &podWorkspace{
		t: t, home: filepath.Join(root, "home"), remote: filepath.Join(root, "remote"),
		clone: filepath.Join(root, "clone"), ws: filepath.Join(root, "ws"),
	}
	// The pushes run under the git first on PATH, which in an issue pod is Legion's wrapper and
	// runs the leak guard before it writes; the fixture carries a stand-in of the check that
	// clears every write.
	f.env = append(fixtureenv.Isolated(f.home),
		"JJ_USER=tester", "JJ_EMAIL=tester@example.test",
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@example.test",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@example.test",
		"PUBLIC_LEAK_CHECK="+leakguardtest.Write(t, root, leakguardtest.Passes))
	seed := filepath.Join(root, "seed")
	for _, dir := range []string{f.remote, seed, f.home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.run(f.remote, fixturegit.InitBare(f.remote, "")...)
	f.run(f.remote, "git", "--git-dir="+f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	f.run(seed, fixturegit.InitWorkTree(seed, "main")...)
	f.write(filepath.Join(seed, "f.txt"), "one\n")
	f.run(seed, "git", "add", "-A")
	f.run(seed, "git", "commit", "--quiet", "-m", "one")
	f.run(seed, "git", "remote", "add", "origin", f.remote)
	f.run(seed, "git", "push", "--quiet", "origin", "main", "main:refs/heads/legion/"+unpushedIssue)

	f.run(root, "git", "clone", "--quiet", "--no-checkout", f.remote, f.clone)
	f.run(root, fixturegit.JJInit(f.clone, true)...)
	f.run(f.clone, "jj", "sparse", "set", "--clear", "-R", f.clone)
	f.run(f.clone, "jj", "abandon", "@", "-R", f.clone)
	f.run(f.clone, "jj", "bookmark", "track", "main@origin", "legion/"+unpushedIssue+"@origin", "-R", f.clone)
	f.run(f.clone, "jj", "workspace", "add", f.ws, "--name", unpushedIssue, "--revision", "legion/"+unpushedIssue+"@origin",
		"--sparse-patterns", "empty", "-R", f.clone)
	f.run(f.ws, "jj", "sparse", "reset", "-R", f.ws)
	return f
}

// unpushedShim is the real shim of a role of a pod, run on the issue workspace ws under the
// fixture's own home, and the daemon's end of its stream. record is where every git the shim starts
// is written (recordingGit).
func unpushedShim(t *testing.T, home, ws string) (p *peer, record string) {
	t.Helper()
	record = recordingGit(t)
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(unpushedEnv(home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+ws)
	run(t, cfg, newClock())
	p = daemon.accept(t)
	p.open(t)
	return p, record
}

// unpushedEnv is the environment a read of a fixture's workspace runs jj under: the fixture's
// isolated home and jj's identity, which unpushedShim gives the shim too.
func unpushedEnv(home string) []string {
	return append(fixtureenv.Isolated(home), "JJ_USER=tester", "JJ_EMAIL=tester@example.test")
}

// requireUnpushed holds a read of the issue workspace ws to answer want, through the real shim of
// a role run under the fixture's home (unpushedShim) and through shim.ReadUnpushed under env, and
// holds that the shim's read ran no git.
func requireUnpushed(t *testing.T, home, ws string, env []string, request shimwire.ReadUnpushed, want shim.Unpushed) {
	t.Helper()
	p, record := unpushedShim(t, home, ws)
	got := readUnpushed(t, p, request)
	if !got.OK || got.Commits != want.Commits || got.WorkingCopyChanged != want.WorkingCopyChanged {
		t.Fatalf("the shim answered %#v, want %d unpushed commits and a working copy changed=%v", got, want.Commits, want.WorkingCopyChanged)
	}
	// The read a test's stand-in agent answers with is the shim's own.
	if direct, err := shim.ReadUnpushed(context.Background(), ws, env, request); err != nil || direct != want {
		t.Fatalf("shim.ReadUnpushed = %#v, %v; want %#v", direct, err, want)
	}
	if calls := gitRecord(t, record); calls != "" {
		t.Fatalf("the read ran git, which it must not:\n%s", calls)
	}
}

// requireJJAndGit fails the test where jj or git is not on PATH, where newWorkFixture skips: a
// case of this test that skipped would read as one that held.
func requireJJAndGit(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
}

// newUnpushedWorkFixture is newWorkFixture, failing where jj or git is not on PATH rather than
// skipping (requireJJAndGit).
func newUnpushedWorkFixture(t *testing.T) *workFixture {
	t.Helper()
	requireJJAndGit(t)
	return newWorkFixture(t)
}

// describedSideHead leaves a side head with no bookmark beside a new working copy on main, as
// sideHead does, described with message, and answers the side head's commit.
func describedSideHead(f *workFixture, message string) string {
	f.t.Helper()
	f.jj(f.ws, "describe", "-R", f.ws, "-m", message)
	return f.sideHead()
}

// fetchLeftHead is a commit described with message that the pod's clone holds only because a
// fetch left it: a second clone pushes it on a branch of its own, the pod's clone fetches it, the
// second clone deletes the branch on the remote, and the pod's clone fetches again. Legion's clones
// keep the commit the deleted branch left, visible and with no bookmark, as they keep the one a
// moved branch leaves (case 9 of TestWorkspaceWorkReadsTheIssuePodLayout).
func fetchLeftHead(f *workFixture, message string) string {
	f.t.Helper()
	other := filepath.Join(f.root, "other")
	f.git(f.root, "clone", "--quiet", f.remote, other)
	f.git(other, "checkout", "--quiet", "-b", "feature")
	f.writeIn(other, "foreign.txt")
	f.git(other, "add", "-A")
	f.git(other, "commit", "--quiet", "-m", message)
	foreign := f.git(other, "rev-parse", "HEAD")
	f.git(other, "push", "--quiet", "origin", "feature")
	f.jj(f.clone, "git", "fetch", "-R", f.clone)
	f.git(other, "push", "--quiet", "--delete", "origin", "feature")
	f.jj(f.clone, "git", "fetch", "-R", f.clone)
	return foreign
}

func (f *podWorkspace) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// run runs one jj or git command of the fixture in dir, the directory it reads its repository from.
func (f *podWorkspace) run(dir string, args ...string) string {
	f.t.Helper()
	return fixturegit.Runner{TB: f.t, Env: f.env}.Run(dir, args...)
}

// readUnpushed sends the shim request and returns its answer, held to carry nothing but the
// members of its frame: the line the shim wrote is the line those members encode to.
func readUnpushed(t *testing.T, p *peer, request shimwire.ReadUnpushed) shimwire.ReadUnpushedResult {
	t.Helper()
	p.send(t, request)
	got, line := shimAnswer[shimwire.ReadUnpushedResult](t, p, request.ID)
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	members, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(members, line) {
		t.Fatalf("the shim answered\n%s\nwhich carries more than the frame's members\n%s", line, members)
	}
	return got
}

// shimAnswer is the shim's next answer of type T, to the request id, and the line it came on. The
// answer runs jj or git over a real workspace, which in a loaded pod can take longer than the
// waitLimit peer.next reads a frame within, so this waits for minutes.
func shimAnswer[T shimwire.Frame](t *testing.T, p *peer, id string) (T, []byte) {
	t.Helper()
	if err := p.conn.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	for {
		line, err := p.r.ReadLine()
		if err != nil {
			var none T
			t.Fatalf("read the shim's answer to %s: %v", id, err)
			return none, nil
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatalf("decode a frame from the shim: %v", err)
		}
		if answer, ok := frame.(T); ok {
			return answer, bytes.Clone(line)
		}
	}
}

// recordingGit puts first on the test process's PATH a git that writes each command it is given
// to the record it answers, then runs the git that was first before it. Any git the read starts,
// itself or through the jj it runs, is found on that PATH, so the record holds every git the read
// starts.
func recordingGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "git-record")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{record, real} {
		if strings.Contains(path, "'") {
			t.Fatalf("%s cannot be quoted in the recording git", path)
		}
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>'" + record + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the recording git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// gitRecord is every git command the recording git was given so far, one a line.
func gitRecord(t *testing.T, record string) string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the git record: %v", err)
	}
	return string(body)
}
