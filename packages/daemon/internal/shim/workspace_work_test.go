package shim_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// workIssue is the issue the workspace read's fixtures belong to: its branch on the remote is
// legion/<workIssue>, which the pod's clone tracks.
const workIssue = "INFRA-945"

// The read answers whether the issue workspace holds work that no branch or tag on origin holds,
// read by the real shim with real jj in an issue pod's layout: a bare remote, the pod's colocated
// clone with Legion's git.abandon-unreachable-commits = false, and the issue workspace added to
// that clone. Each subtest makes one state of the workspace with the command a role runs and
// holds the shim's answer to what spec R1 counts as work: a commit under a working copy or a
// local bookmark that origin does not hold, or a change in the working copy, where a commit with
// no change and no description is no work, and a commit a fetch left visible or a side head no
// working copy and no bookmark reaches is not counted. The read is the shim's own: Oh My Pi reads
// nothing of it.
func TestWorkspaceWorkReadsTheIssuePodLayout(t *testing.T) {
	for _, fixture := range []struct {
		name string
		// make leaves the workspace in the fixture's state and answers the work commits the read
		// must list, newest first, as revsets read once the shim has answered: a commit the read's
		// own snapshot makes has no id before it.
		make    func(f *workFixture) []string
		changed bool
	}{
		{name: "1 a freshly added workspace is no work", make: func(f *workFixture) []string { return nil }},
		{name: "2 an empty working copy after jj new is no work", make: func(f *workFixture) []string {
			f.jj(f.ws, "new", "-R", f.ws)
			return nil
		}},
		{name: "3 a described empty working copy is work", make: func(f *workFixture) []string {
			f.jj(f.ws, "describe", "-R", f.ws, "-m", "planned work")
			return []string{"@"}
		}},
		{name: "4 a change in the working copy is work", changed: true, make: func(f *workFixture) []string {
			f.write("change.txt")
			return []string{"@"}
		}},
		{name: "5 an unpushed commit under an empty working copy is work", make: func(f *workFixture) []string {
			f.write("unpushed.txt")
			f.jj(f.ws, "commit", "-R", f.ws, "-m", "unpushed work")
			return []string{"@-"}
		}},
		{name: "6 a pushed commit under an empty working copy is no work", make: func(f *workFixture) []string {
			f.write("pushed.txt")
			f.jj(f.ws, "commit", "-R", f.ws, "-m", "pushed work")
			f.jj(f.ws, "bookmark", "set", "-R", f.ws, "legion/"+workIssue, "-r", "@-")
			f.jj(f.ws, "git", "push", "-R", f.ws, "--bookmark", "legion/"+workIssue)
			return nil
		}},
		{name: "7 a side head with no bookmark beside a new working copy is no work", make: func(f *workFixture) []string {
			head := f.sideHead()
			f.requireVisibleHead(head, "the side head jj new left")
			return nil
		}},
		{name: "8 the same side head under a local bookmark is work", make: func(f *workFixture) []string {
			head := f.sideHead()
			f.jj(f.ws, "bookmark", "create", "-R", f.ws, "side", "-r", head)
			return []string{head}
		}},
		{name: "9 a commit a fetch made unreachable is no work", make: func(f *workFixture) []string {
			// A second clone pushes a branch, the pod's clone fetches it, the second clone moves
			// the branch off that commit, and the pod's clone fetches again: Legion's clones keep
			// the commit the branch left, visible and with no bookmark.
			other := filepath.Join(f.root, "other")
			f.git(f.root, "clone", "--quiet", f.remote, other)
			f.git(other, "checkout", "--quiet", "-b", "feature")
			f.writeIn(other, "foreign.txt")
			f.git(other, "add", "-A")
			f.git(other, "commit", "--quiet", "-m", "another issue's work")
			foreign := f.git(other, "rev-parse", "HEAD")
			f.git(other, "push", "--quiet", "origin", "feature")
			f.jj(f.clone, "git", "fetch", "-R", f.clone)
			f.git(other, "reset", "--quiet", "--hard", "origin/main")
			f.writeIn(other, "moved.txt")
			f.git(other, "add", "-A")
			f.git(other, "commit", "--quiet", "-m", "the branch moved")
			f.git(other, "push", "--quiet", "--force", "origin", "feature")
			f.jj(f.clone, "git", "fetch", "-R", f.clone)
			f.requireVisibleHead(foreign, "the commit the moved branch left")
			return nil
		}},
		{name: "10 a commit under a local bookmark that only a tag on the remote holds is no work", make: func(f *workFixture) []string {
			f.write("tagged.txt")
			f.jj(f.ws, "commit", "-R", f.ws, "-m", "tagged work")
			f.jj(f.ws, "bookmark", "create", "-R", f.ws, "tagged", "-r", "@-")
			tagged := f.commit("@-")
			f.git(f.clone, "push", "--quiet", "origin", tagged+":refs/tags/t1")
			f.jj(f.clone, "git", "fetch", "-R", f.clone)
			if got := f.jjRead("tags() & " + tagged); got != tagged {
				f.t.Fatalf("the fetch did not bring tag t1 on %s: tags() & it is %q", tagged, got)
			}
			return nil
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f := newWorkFixture(t)
			listed := fixture.make(f)

			got := f.read()
			if !got.OK {
				t.Fatalf("the shim refused the read: %s", got.Error)
			}
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, revset := range listed {
				want = append(want, f.commit(revset))
			}
			if head := f.commit("@"); got.Head != head {
				t.Errorf("the read names the working copy %s, want %s", got.Head, head)
			}
			if got.Changed != fixture.changed {
				t.Errorf("the read says the working copy changed is %v, want %v", got.Changed, fixture.changed)
			}
			if !slices.Equal(got.Commits, want) || got.Count != len(want) {
				t.Errorf("the read lists %v and counts %d, want %v", got.Commits, got.Count, want)
			}
		})
	}
}

// A fix's working copy can be stale: a jj command in another workspace of the clone rewrote the
// commit it stands on, and the copy's files on disk have not followed. An edit the fix's agent
// made in that copy after its last jj command is then on the disk alone. The read must not move
// it: bringing the stale copy up to date first (`jj workspace update-stale`) took the edit off the
// disk into a divergent commit that no working copy reaches and the count does not hold, so a
// release would delete it. The read takes no such step: it reads the stale copy, which jj refuses,
// and the read is refused, so the pod is kept, as a stale issue copy already keeps it. The edit is
// still on the disk afterwards, unchanged.
func TestAStaleFixCopysUnrecordedEditStaysOnTheDisk(t *testing.T) {
	f := newWorkFixture(t)
	fix := f.ws + "-fix"
	name := filepath.Base(fix)
	f.jj(f.ws, "workspace", "add", "--name", name, "-r", "main", fix, "-R", f.ws)
	// Another workspace rewrites the commit the fix's copy stands on: the copy is now stale.
	f.jj(f.ws, "describe", "-r", name+"@", "-m", "moved under the fix", "-R", f.ws)
	f.writeIn(fix, "fix-wip.txt")
	wip := filepath.Join(fix, "fix-wip.txt")
	before, err := os.ReadFile(wip)
	if err != nil {
		t.Fatal(err)
	}

	got := f.read()
	after, err := os.ReadFile(wip)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the read of a stale fix copy left fix-wip.txt %q (%v), want it on the disk as written, %q: the read moved an edit no jj command recorded (read %+v)",
			after, err, before, got)
	}
	if got.OK && got.Count == 0 {
		t.Fatalf("the read of a stale fix copy holding an unrecorded edit = %+v, want it refused or the edit counted: no work lets a release take the pod and the edit", got)
	}
}

// A pod is released, or replaced by an update round, only once the workspace read finds no work,
// and a fix works in a second working copy of the clone beside the issue's, workspace.FixDir. An
// edit the fix's agent made after its last jj command is in no commit until a jj command run in
// that copy snapshots it, and when the fix resumes in a new pod its copy is made again from the
// fix's branch, so a read that counted only the issue's own working copy let a release delete that
// edit. The read snapshots the fix's copy too, and counts the edit: the fix's working copy holding
// fix-wip.txt, with no jj command after it, reads as one work commit, the fix's working copy itself,
// so the release is declined. A fix's copy with no edit reads as no work, as the issue's own does:
// the copy alone keeps no pod.
func TestAReleaseKeepsTheRunningFixsCopy(t *testing.T) {
	f := newWorkFixture(t)
	fix := f.ws + "-fix"
	// The adoption makes the fix's copy so: a workspace of the clone, named for its directory, at
	// the commit the fix starts from.
	f.jj(f.ws, "workspace", "add", "--name", filepath.Base(fix), "-r", "main", fix, "-R", f.ws)
	if clean := f.read(); !clean.OK || clean.Changed || clean.Count != 0 {
		t.Fatalf("the read of a workspace beside a fix's copy with no edit = %+v, want no work", clean)
	}

	// The fix's agent edits a file and runs no jj command after it: nothing has recorded the edit.
	f.writeIn(fix, "fix-wip.txt")
	got := f.read()
	if !got.OK {
		t.Fatalf("the shim refused the read: %s", got.Error)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	at := f.jj(fix, "log", "-R", fix, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id")
	if got.Count != 1 || !slices.Equal(got.Commits, []string{at}) {
		t.Fatalf("the read of a fix's copy holding an edit no jj command recorded lists %v and counts %d, want the fix's working copy %s: the release would delete the edit",
			got.Commits, got.Count, at)
	}
	if files := f.jj(fix, "diff", "-R", fix, "--ignore-working-copy", "-r", at, "--name-only"); files != "fix-wip.txt" {
		t.Fatalf("the fix's working copy %s the read counted changes %q, want the fix's edit fix-wip.txt", at, files)
	}
	if head := f.commit("@"); got.Head != head || got.Changed {
		t.Fatalf("the read names the issue's working copy %s changed %v, want %s unchanged: the edit is the fix's", got.Head, got.Changed, head)
	}
}

// A workspace-work frame the shim cannot decode is refused, and one with no id to answer is
// logged and answered nothing: the daemon is the frame's only sender, so Oh My Pi, which would
// refuse it as an unknown command, never reads either.
func TestAMalformedWorkspaceWorkFrameReachesNoAgent(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	shim := run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)

	p.sendLine(t, `{"type":"workspace-work","id":7,"issue":"INFRA-945","timeoutMs":1000}`)
	shim.log.await(t, "refusing a malformed workspace-work frame", 1)
	p.send(t, shimwire.WorkspaceWork{Issue: workIssue, TimeoutMs: 1000})
	shim.log.await(t, "refusing a workspace-work frame", 1)
	p.send(t, shimwire.GetState{ID: "after"})
	if answer, ok := p.next(t).(shimwire.Response); !ok || answer.ID != "after" {
		t.Fatalf("the shim sent %#v, want OMP's answer to the get_state after the two frames", answer)
	}
	if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
		t.Fatalf("OMP read %v; a workspace-work frame is the shim's own", got)
	}
}

// A read that fails answers its reason to the daemon on the connection it was asked on, however
// much jj wrote to its standard error. The reason holds all of that error, which nothing else
// bounds, so a jj that writes more than one line can hold would make an answer the writer
// refuses: the shim would close the connection and send that line first again at each
// reconnect, so the role's stream would stay down. The answer keeps the start of the reason,
// which names the command, its exit and jj's first words, and says that the shim cut the rest
// (ASVS v5.0.0-15.2.2). The connection then carries the next answer, and Oh My Pi reads nothing
// of the read.
func TestAWorkspaceWorkFailureWithAnErrorOverOneLineReachesTheDaemon(t *testing.T) {
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho 'Error: the working copy is stale' >&2\nhead -c %d /dev/zero | tr '\\0' x >&2\nexit 3\n",
		2*shimwire.MaxFrameBytes)
	if err := os.WriteFile(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake jj: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+t.TempDir())
	run(t, cfg, newClock())
	p := daemon.accept(t)
	p.open(t)

	p.send(t, shimwire.WorkspaceWork{ID: "read-2", Issue: workIssue, TimeoutMs: 120_000})
	got, ok := p.next(t).(shimwire.WorkspaceWorkResult)
	if !ok || got.ID != "read-2" || got.OK {
		t.Fatalf("the shim answered %#v, want the refusal of read-2", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Command failed (exit 3)", "jj log", "the working copy is stale", "cut"} {
		if !strings.Contains(got.Error, want) {
			t.Fatalf("the refusal reads %.300q, want it to say %q", got.Error, want)
		}
	}
	p.send(t, shimwire.GetState{ID: "after"})
	if answer, ok := p.next(t).(shimwire.Response); !ok || answer.ID != "after" {
		t.Fatalf("the shim sent %#v, want OMP's answer to the get_state after the read on the same connection", answer)
	}
	if types := frameTypes(child.received(t)); !reflect.DeepEqual(types, []string{shimwire.TypeGetState}) {
		t.Fatalf("OMP read %v; workspace-work is the shim's own", types)
	}
}

// workFixture is one remote and an issue pod laid out on it as provisioning lays it out: the
// pod's colocated clone, which tracks the remote's main and the issue's branch and keeps
// unreachable commits as Legion's clones do (workspace's ensureFetchConfiguration), and the issue
// workspace added to that clone. Every command is real git or jj under the fixture's isolated
// home, which the shim under test is given too (workspaceCommand, docs/testing.md).
type workFixture struct {
	t                             *testing.T
	root, home, remote, clone, ws string
}

func newWorkFixture(t *testing.T) *workFixture {
	t.Helper()
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required: %v", tool, err)
		}
	}
	root := t.TempDir()
	f := &workFixture{
		t: t, root: root, home: filepath.Join(root, "home"), remote: filepath.Join(root, "remote"),
		clone: filepath.Join(root, "clone"), ws: filepath.Join(root, "ws"),
	}
	seed := filepath.Join(root, "seed")
	for _, dir := range []string{f.home, f.remote, seed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.git(f.remote, fixturegit.InitBare(f.remote, "")[1:]...)
	f.git(f.remote, "--git-dir="+f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	f.git(seed, fixturegit.InitWorkTree(seed, "main")[1:]...)
	f.writeIn(seed, "f.txt")
	f.git(seed, "add", "-A")
	f.git(seed, "commit", "--quiet", "-m", "one")
	f.git(seed, "remote", "add", "origin", f.remote)
	f.git(seed, "push", "--quiet", "-u", "origin", "main")
	f.git(seed, "push", "--quiet", "origin", "main:refs/heads/legion/"+workIssue)
	// --no-local: a local clone hardlinks the whole object store, unreachable objects included,
	// where a pod clones over the network and gets only what the remote's refs reach.
	f.git(root, "clone", "--quiet", "--no-local", f.remote, f.clone)
	f.jj(f.clone, fixturegit.JJInit(f.clone, true)[1:]...)
	f.jj(f.clone, "config", "set", "-R", f.clone, "--repo", "git.abandon-unreachable-commits", "false")
	f.jj(f.clone, "bookmark", "track", "-R", f.clone, "main@origin", "legion/"+workIssue+"@origin")
	f.jj(f.clone, "workspace", "add", "-R", f.clone, "--name", filepath.Base(f.ws), f.ws)
	return f
}

// read is the real shim's answer to one workspace-work frame, asked over the daemon's end of the
// worker stream, with the shim's environment naming the fixture's workspace. It also holds that Oh
// My Pi read nothing of the read.
func (f *workFixture) read() shimwire.WorkspaceWorkResult {
	f.t.Helper()
	path := socketPath(f.t)
	daemon := listen(f.t, path)
	child := newOMP(f.t)
	cfg := config(f.t, path, child)
	cfg.Env = append(fixtureenv.Isolated(f.home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+f.ws, "JJ_USER=tester", "JJ_EMAIL=tester@example.test")
	run(f.t, cfg, newClock())
	p := daemon.accept(f.t)
	p.open(f.t)
	p.send(f.t, shimwire.WorkspaceWork{ID: "read-1", Issue: workIssue, TimeoutMs: 120_000})
	got, ok := p.next(f.t).(shimwire.WorkspaceWorkResult)
	if !ok || got.ID != "read-1" {
		f.t.Fatalf("the shim answered %#v, want the result of read-1", got)
	}
	p.send(f.t, shimwire.GetState{ID: "after"})
	p.next(f.t)
	if types := frameTypes(child.received(f.t)); !reflect.DeepEqual(types, []string{shimwire.TypeGetState}) {
		f.t.Fatalf("OMP read %v; workspace-work is the shim's own", types)
	}
	return got
}

// sideHead leaves a changed working copy as a side head with no bookmark, beside a new working
// copy on main, as a role does that runs `jj new <base>` over its own change, and answers the
// side head's commit.
func (f *workFixture) sideHead() string {
	f.t.Helper()
	f.write("side.txt")
	head := f.jj(f.ws, "log", "-R", f.ws, "-r", "@", "--no-graph", "-T", "commit_id")
	f.jj(f.ws, "new", "-R", f.ws, "main")
	return head
}

// requireVisibleHead fails the test unless commit is a visible head of the clone: the read must
// leave it out because of what it counts, not because jj hid it.
func (f *workFixture) requireVisibleHead(commit, what string) {
	f.t.Helper()
	if got := f.jjRead("visible_heads() & " + commit); got != commit {
		f.t.Fatalf("%s, %s, is not a visible head: visible_heads() & it is %q", what, commit, got)
	}
}

// commit is the one commit id revset names in the workspace, read without a snapshot, so it is
// the state the shim's read left.
func (f *workFixture) commit(revset string) string {
	f.t.Helper()
	id := f.jjRead(revset)
	if id == "" {
		f.t.Fatalf("%s names no commit in the workspace", revset)
	}
	return id
}

// jjRead is the commit ids revset names in the workspace, read without a snapshot.
func (f *workFixture) jjRead(revset string) string {
	f.t.Helper()
	return f.jj(f.ws, "log", "-R", f.ws, "--ignore-working-copy", "-r", revset, "--no-graph", "-T", "commit_id")
}

// write writes a file of its own name into the workspace: a change in its working copy.
func (f *workFixture) write(name string) {
	f.t.Helper()
	f.writeIn(f.ws, name)
}

func (f *workFixture) writeIn(dir, name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// jj and git run one command in dir, the directory it reads its repository from, under the
// fixture's home (workspaceCommand).
func (f *workFixture) jj(dir string, args ...string) string {
	f.t.Helper()
	return jjRun(f.t, f.home, dir, args...)
}

func (f *workFixture) git(dir string, args ...string) string {
	f.t.Helper()
	return jjGit(f.t, f.home, dir, args...)
}
