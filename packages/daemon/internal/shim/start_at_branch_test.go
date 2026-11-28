package shim_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The start of a next change's working copy, answered by the real shim with real jj in an issue
// pod's layout. The pod's working copy holds a change of the role's own on the issue's branch. Then
// GitHub merges the branch into main, and the daemon moves the branch to main's new head. The
// daemon's frame arrives, the shim fetches and starts a new working copy on the moved branch in
// the workspace its environment names, and its answer carries the old working copy's commit and
// the branch's. The start is the shim's own: Oh My Pi reads nothing of it. A frame with an id that
// the shim cannot act on is answered with its reason.
func TestStartAtBranchAnswersTheDaemon(t *testing.T) {
	f := newWorkFixture(t)
	branch := "legion/" + workIssue
	f.write("notes.txt")
	old := f.jj(f.ws, "log", "-R", f.ws, "-r", "@", "--no-graph", "-T", "commit_id")
	seed := filepath.Join(f.root, "seed")
	f.writeIn(seed, "merged.txt")
	f.git(seed, "add", "-A")
	f.git(seed, "commit", "--quiet", "-m", "feat: the change (#1)")
	f.git(seed, "push", "--quiet", "origin", "main")
	f.git(seed, "push", "--quiet", "--force", "origin", "main:refs/heads/"+branch)
	merged := f.git(seed, "rev-parse", "HEAD")

	got := f.startAt(shimwire.StartAtBranch{ID: "start-1", Bookmark: branch, Base: "main", TimeoutMs: 120_000},
		shimwire.StartAtBranch{ID: "start-2", Bookmark: branch, TimeoutMs: 120_000})
	if want := (shimwire.StartAtBranchResult{ID: "start-1", OK: true, Moved: true, From: old, At: merged}); got[0] != want {
		t.Fatalf("the shim answered %#v, want %#v", got[0], want)
	}
	if err := got[0].Validate(); err != nil {
		t.Fatal(err)
	}
	if parents := f.jjRead("parents(@)"); parents != merged {
		t.Fatalf("the working copy's parents are %q after the start, want main's head %s", parents, merged)
	}
	if refused := got[1]; refused.ID != "start-2" || refused.OK || !strings.Contains(refused.Error, "names no bookmark or no base") {
		t.Fatalf("the shim answered the frame with no base %#v, want a refusal naming it", refused)
	}
}

// startAt is the real shim's answer to each request, asked in order over the daemon's end of the
// worker stream, with the shim's environment naming the fixture's workspace. It also holds that Oh
// My Pi read nothing of them.
func (f *workFixture) startAt(requests ...shimwire.StartAtBranch) []shimwire.StartAtBranchResult {
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
	var answers []shimwire.StartAtBranchResult
	for _, request := range requests {
		p.send(f.t, request)
		got, ok := p.next(f.t).(shimwire.StartAtBranchResult)
		if !ok || got.ID != request.ID {
			f.t.Fatalf("the shim answered %#v, want the result of %s", got, request.ID)
		}
		answers = append(answers, got)
	}
	p.send(f.t, shimwire.GetState{ID: "after"})
	p.next(f.t)
	if types := frameTypes(child.received(f.t)); !reflect.DeepEqual(types, []string{shimwire.TypeGetState}) {
		f.t.Fatalf("OMP read %v; start-at-branch is the shim's own", types)
	}
	return answers
}
