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
// only when the pod's workspace holds unpushed work. Each case is a real workspace in the layout a
// pod's provisioning makes, read by the real shim as the release reads it, and the answer is held
// to the read's two facts: the line the shim wrote is the frame's own members and nothing else, so
// no commit, file or description of the workspace rides along. The read builds no bundle: every
// git the shim starts is recorded (recordingGit), a bundle is only ever written by `git bundle
// create`, and the read's record stays empty. The last case runs the shim's bundle builder on a
// workspace with nothing unpushed and finds a bundle that is not empty, which is why the release
// reads no bundle: one cannot tell such a workspace from one with work in it.
//
// The first case is what every pod reads when its roles have pushed everything: the pod's shared
// clone holds a workspace of its own whose working copy is an empty commit no remote holds, and a
// read that subtracted only its own working copy would count it.
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
			// Something wrote a file into the shared clone's own working copy: only an empty
			// working copy is left out of the count, whichever workspace it belongs to.
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
			p, record := f.shim(t)
			got := readUnpushed(t, p, "read-1")
			if !got.OK || got.Commits != tc.want.Commits || got.WorkingCopyChanged != tc.want.WorkingCopyChanged {
				t.Fatalf("the shim answered %#v, want %d unpushed commits and a working copy changed=%v", got, tc.want.Commits, tc.want.WorkingCopyChanged)
			}
			// The read a test's stand-in agent answers with is the shim's own.
			direct, err := shim.ReadUnpushed(context.Background(), f.ws, f.env, time.Minute)
			if err != nil || direct != tc.want {
				t.Fatalf("shim.ReadUnpushed = %#v, %v; want %#v", direct, err, tc.want)
			}
			if calls := gitRecord(t, record); calls != "" {
				t.Fatalf("the read ran git, which is what a bundle is written with:\n%s", calls)
			}
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
		got := readUnpushed(t, p, "read-2")
		const refusal = "worker-shim: LEGION_WORKSPACE is not set; no workspace to read for unpushed work"
		if got.OK || got.Error != refusal {
			t.Fatalf("the shim answered %#v, want a refusal reading %q and no answer that nothing is unpushed", got, refusal)
		}
	})

	t.Run("the bundle builder's bundle of a workspace with nothing unpushed is not empty", func(t *testing.T) {
		f := newPodWorkspace(t)
		p, record := f.shim(t)
		if got := readUnpushed(t, p, "read-3"); !got.OK || got.Commits != 0 || got.WorkingCopyChanged {
			t.Fatalf("the shim answered %#v, want nothing unpushed", got)
		}
		if calls := gitRecord(t, record); calls != "" {
			t.Fatalf("the read ran git, which is what a bundle is written with:\n%s", calls)
		}
		p.send(t, shimwire.BundleWorkspace{ID: "bundle-1", Issue: unpushedIssue, MaxBytes: 40 << 20, TimeoutMs: 120_000})
		bundle, _ := shimAnswer[shimwire.BundleWorkspaceResult](t, p, "bundle-1")
		if !bundle.OK || bundle.SizeBytes == 0 || len(bundle.Bundle) != bundle.SizeBytes || len(bundle.Tips) == 0 {
			t.Fatalf("the bundle builder answered ok=%v with %d bytes saying %d on heads %v (%s), want a bundle that is not empty",
				bundle.OK, len(bundle.Bundle), bundle.SizeBytes, bundle.Tips, bundle.Error)
		}
		// The record sees the git the bundle builder writes with, so a read whose record is empty
		// wrote no bundle.
		if calls := gitRecord(t, record); !strings.Contains(calls, " bundle create ") {
			t.Fatalf("the bundle builder's git is not in the record, so an empty record says nothing of a bundle:\n%s", calls)
		}
		t.Logf("a workspace with nothing unpushed bundles to %d bytes on %d heads", bundle.SizeBytes, len(bundle.Tips))
	})
}

// unpushedIssue is the issue the fixture's pod works on, whose branch the remote holds.
const unpushedIssue = "INFRA-945"

// podWorkspace is an issue pod's workspace in the layout provisioning makes, and the remote it was
// cloned from: a bare remote holding the default branch and the issue's branch; the pod's shared
// clone, made as workspace's feedClone makes one (`git clone --no-checkout`, `jj git init
// --colocate`, its sparse patterns cleared and its first snapshot abandoned), which leaves the
// clone's own `default` workspace on an empty working copy no remote holds; and the issue's
// workspace added to that clone on the issue's branch with no files and then checked out, as
// Provision adds one. Every command is real git or jj under fixtureenv.Isolated of a home of the
// fixture's own and names the directory it runs in (docs/testing.md), as launcher's bundleFixture
// runs its own.
type podWorkspace struct {
	t                       *testing.T
	home, remote, clone, ws string
	env                     []string
}

func newPodWorkspace(t *testing.T) *podWorkspace {
	t.Helper()
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	root := t.TempDir()
	f := &podWorkspace{
		t: t, home: filepath.Join(root, "home"), remote: filepath.Join(root, "remote"),
		clone: filepath.Join(root, "clone"), ws: filepath.Join(root, "ws"),
	}
	// The pushes run under the git first on PATH, which in an issue pod is Legion's wrapper and
	// runs the leak guard before it writes; the fixture carries a stand-in of the check that
	// clears every write, as launcher's bundleFixture does.
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

// shim is the real shim of a role of the pod, run on the fixture's issue workspace under the
// fixture's own home, and the daemon's end of its stream. record is where every git the shim starts
// is written (recordingGit).
func (f *podWorkspace) shim(t *testing.T) (p *peer, record string) {
	t.Helper()
	record = recordingGit(t)
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(f.home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+f.ws, "JJ_USER=tester", "JJ_EMAIL=tester@example.test")
	run(t, cfg, newClock())
	p = daemon.accept(t)
	p.open(t)
	return p, record
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

// readUnpushed sends the shim one read-unpushed and returns its answer, held to carry nothing but
// the members of its frame: the line the shim wrote is the line those members encode to.
func readUnpushed(t *testing.T, p *peer, id string) shimwire.ReadUnpushedResult {
	t.Helper()
	p.send(t, shimwire.ReadUnpushed{ID: id, Issue: unpushedIssue, TimeoutMs: 120_000})
	got, line := shimAnswer[shimwire.ReadUnpushedResult](t, p, id)
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
// to the record it answers, then runs the git that was first before it. The shim looks up the git
// it bundles with on that PATH, so the record holds every git the shim starts.
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
