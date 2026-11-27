package launcher

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// R6 of #5285: where no role process runs in an issue pod, the pod's launcher reads
// the issue workspace for the daemon, with the count a role's shim makes (workspace.ReadWork). The
// launcher's hello says it answers the read, and it answers on the connection the daemon asked
// through: a clean workspace reads as no work, and a change in the working copy reads as work, the
// changed working-copy commit counted.
//
// The repository's and the workspace's configuration are a role's to write, and the launcher holds
// every role's launcher token, so the read runs no program that any configuration names, whichever
// jj the image carries: it runs jj with no user, repository or workspace configuration at all
// (launcherReadEnv). The configuration this test writes would run three programs: a signing
// program that the snapshot of a signed working copy would run to sign the rewritten commit, the
// same program as the verifier that a template alias of `empty` calls through the working copy's
// signature, and a Watchman that the file monitor backend starts. It also holds a revset alias that
// would hide every working copy from the count. The launcher's own environment points at that
// configuration, as a launcher whose home a role could write would, and the read runs none of the
// programs and counts the work.
//
// jj also loads a configuration file inside the repository's own directories, the repository's
// `config.toml` and the workspace's `.jj/workspace-config.toml`, where the id file beside it is
// missing: it migrates that file into the configuration it reads. So the launcher runs the guards
// the daemon's own jj commands run first (workspace.GuardJJ, the Legion reviewer's point 6): the
// workspace and the shared clone that Location lays out beside it, and no legacy configuration
// file, each removed before jj runs. A role that deletes the id files and writes both legacy files
// with the signing program gets neither loaded: the read runs no program and counts the work, and
// the files are gone. A workspace the guards refuse, here one whose .jj/repo names another
// repository than the shared clone, is not read: the read is refused, and the daemon keeps the pod
// as for any read that cannot be made.
func TestTheLauncherReadsTheWorkspaceItRunsIn(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (the fixture's home and PATH).
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("this row drives the real %s, which the worker image and CI both install: %v", tool, err)
		}
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{home, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// marker is a program that leaves the file it names and refuses, as a program a configuration
	// names that must never run.
	marker := func(name string) (program, left string) {
		left = filepath.Join(root, name+"-ran")
		program = filepath.Join(root, name+".sh")
		if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch '"+left+"'\ncat >/dev/null\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return program, left
	}
	signer, signed := marker("sign")
	watchmanScript, watched := marker("watchman")
	if err := os.Rename(watchmanScript, filepath.Join(bin, "watchman")); err != nil {
		t.Fatal(err)
	}
	// okSigner signs: it answers an armored block, so the fixture can sign the working copy.
	okSigner := filepath.Join(root, "ok-sign.sh")
	if err := os.WriteFile(okSigner, []byte("#!/bin/sh\ncat >/dev/null\nprintf -- '-----BEGIN PGP SIGNATURE-----\\nfixture\\n-----END PGP SIGNATURE-----\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The launcher's own environment reaches the configuration the fixture writes: the home whose
	// config directory jj keeps each repository's and workspace's configuration in, and a PATH that
	// holds the Watchman.
	for _, entry := range fixtureenv.Home(home) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	run := fixturegit.Isolated(t, home)
	// The workspace and its shared clone where provisioning lays them out (workspace.Location),
	// since the read's guards derive the clone from the workspace's place in that layout.
	located, err := workspace.Location(filepath.Join(root, "state"), ghrepo.MustParse("acme/widgets"), "legion-208")
	if err != nil {
		t.Fatal(err)
	}
	remote, seed, clone, ws := filepath.Join(root, "remote"), filepath.Join(root, "seed"), located.Clone, located.Dir
	for _, dir := range []string{filepath.Dir(clone), filepath.Dir(ws)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run.Run(root, fixturegit.InitBare(remote, "main")...)
	run.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run.Run(seed, "git", "add", "base.txt")
	run.Run(seed, "git", "commit", "--quiet", "-m", "base")
	run.Run(seed, "git", "push", "--quiet", remote, "main")
	run.Run(root, "git", "clone", "--quiet", "--no-local", remote, clone)
	run.Run(clone, fixturegit.JJInit(clone, true)...)
	run.Run(clone, "jj", "workspace", "add", ws, "--name", "legion-208", "--revision", "main", "-R", clone)
	// The working copy is signed, empty and undescribed, so it is no work and a snapshot that
	// rewrites it would sign the rewrite where the configuration says so.
	run.Run(ws, "jj", "--config=signing.backend=gpg", "--config=signing.backends.gpg.program="+okSigner, "sign", "-r", "@", "-R", ws)

	// A configuration a role could write: sign every commit and verify signatures with the marker
	// program, an `empty` that verifies the working copy's signature, no working copies in any
	// revset, and a Watchman file monitor, set last so the fixture's own commands start no Watchman.
	for _, setting := range [][2]string{
		{"signing.behavior", "force"}, {"signing.backend", "gpg"}, {"signing.backends.gpg.program", signer},
		{"template-aliases.empty", `if(self.signature(), self.signature().status() == "good", self.empty())`},
		{"revset-aliases.'working_copies()'", "none()"},
		{"fsmonitor.backend", "watchman"},
	} {
		run.Run(ws, "jj", "config", "set", "--repo", setting[0], setting[1], "-R", ws)
	}
	ran := func() {
		t.Helper()
		for _, left := range []string{signed, watched} {
			if _, err := os.Stat(left); err == nil {
				t.Fatalf("the launcher's read ran %s, which the repository's configuration names", strings.TrimSuffix(filepath.Base(left), "-ran"))
			}
		}
	}

	g := newSessionRig(t, Config{Workspace: ws})
	if !g.roleConn.hello.ReadsWorkspace {
		t.Fatalf("hello = %#v, want it to say the launcher reads a workspace", g.roleConn.hello)
	}
	answer := func(id string) shimwire.WorkspaceWorkResult {
		t.Helper()
		got, ok := g.roleConn.answer(shimwire.WorkspaceWork{ID: id, Issue: "LEGION-208", TimeoutMs: 120_000}).(shimwire.WorkspaceWorkResult)
		if !ok || got.ID != id {
			t.Fatalf("the launcher answered %#v, want the result of %s", got, id)
		}
		return got
	}
	read := func(id string) shimwire.WorkspaceWorkResult {
		t.Helper()
		got := answer(id)
		if !got.OK {
			t.Fatalf("the launcher refused read %s: %s", id, got.Error)
		}
		ran()
		return got
	}

	if clean := read("clean"); clean.Count != 0 || clean.Changed || clean.Head == "" {
		t.Fatalf("the read of a clean workspace = %+v, want its working copy and no work", clean)
	}
	if err := os.WriteFile(filepath.Join(ws, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := read("changed")
	if !changed.Changed || changed.Count != 1 || len(changed.Commits) != 1 || changed.Commits[0] != changed.Head {
		t.Fatalf("the read of a workspace with a change = %+v, want the changed working copy counted as the one work commit", changed)
	}

	// The legacy configuration files a role can write inside the repository's own directories,
	// with the id file beside each deleted, which jj would migrate and load: the signing program
	// again. The read runs the guards first, so jj loads neither, the snapshot of a new change
	// signs nothing, and both files are gone.
	legacy := []string{filepath.Join(clone, ".jj", "repo", "config.toml"), filepath.Join(ws, ".jj", "workspace-config.toml")}
	for _, id := range []string{filepath.Join(clone, ".jj", "repo", "config-id"), filepath.Join(ws, ".jj", "workspace-config-id")} {
		// jj writes a workspace's id file only once something sets that workspace's
		// configuration, so the workspace's may be absent already, which is the shape a role wants.
		if err := os.Remove(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("delete the id file %s, as a role could: %v", id, err)
		}
	}
	for _, file := range legacy {
		if err := os.WriteFile(file, []byte("[signing]\nbehavior = \"force\"\nbackend = \"gpg\"\nbackends.gpg.program = \""+signer+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := read("legacy-config"); !again.Changed || again.Count != 1 {
		t.Fatalf("the read of a workspace that held legacy configuration files = %+v, want the changed working copy counted", again)
	}
	for _, file := range legacy {
		if _, err := os.Lstat(file); err == nil {
			t.Fatalf("the legacy configuration file %s is still there after the read, want it removed before jj ran", file)
		}
	}

	// A workspace the guards refuse: its .jj/repo names another repository than the shared clone
	// that the layout puts beside it.
	if err := os.WriteFile(filepath.Join(ws, ".jj", "repo"), []byte(filepath.Join(root, "another-repository")), 0o644); err != nil {
		t.Fatal(err)
	}
	if refused := answer("another-repository"); refused.OK || !strings.Contains(refused.Error, "refusing to run jj") {
		t.Fatalf("the read of a workspace whose .jj/repo names another repository = %+v, want it refused by the workspace guards", refused)
	}
	ran()
}

// An edit that a task's agent leaves in the working copy of its task, with no jj command after
// it, is work that the launcher's read counts, as a role's shim's read counts it (Medium 6 of the
// review of #5169). The read snapshots the working copy of each task beside the issue's own
// (workspace.TaskCopies) before it lists the work, so the edit is in that working copy's commit,
// and the read lists that commit. A release declines on such a read.
//
// The snapshot is a jj command in the task's working copy, and jj migrates and loads the legacy
// configuration file of the workspace it opens where the id file beside it is missing. So the
// launcher runs the guards in each task's working copy before jj runs there, as it does in the
// issue's (workspace.GuardJJ). A legacy workspace-config.toml that a role writes in the task's
// .jj, which names a signing program, is removed, and the program does not run. A task's working
// copy whose .jj/repo names another repository than the shared clone is refused by the guards,
// and is not read.
func TestTheLauncherReadCountsAnEditLeftInATasksWorkingCopy(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (the fixture's home).
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("this row drives the real %s, which the worker image and CI both install: %v", tool, err)
		}
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range fixtureenv.Home(home) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	// signer leaves the file signed and refuses: a program that a task's configuration names, which
	// the read must never run.
	signer, signed := filepath.Join(root, "sign.sh"), filepath.Join(root, "sign-ran")
	if err := os.WriteFile(signer, []byte("#!/bin/sh\ntouch '"+signed+"'\ncat >/dev/null\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	run := fixturegit.Isolated(t, home)
	located, err := workspace.Location(filepath.Join(root, "state"), ghrepo.MustParse("acme/widgets"), "LEGION-208")
	if err != nil {
		t.Fatal(err)
	}
	remote, seed, clone, ws := filepath.Join(root, "remote"), filepath.Join(root, "seed"), located.Clone, located.Dir
	for _, dir := range []string{filepath.Dir(clone), filepath.Dir(ws)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run.Run(root, fixturegit.InitBare(remote, "main")...)
	run.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run.Run(seed, "git", "add", "base.txt")
	run.Run(seed, "git", "commit", "--quiet", "-m", "base")
	run.Run(seed, "git", "push", "--quiet", remote, "main")
	run.Run(root, "git", "clone", "--quiet", "--no-local", remote, clone)
	run.Run(clone, fixturegit.JJInit(clone, true)...)
	run.Run(clone, "jj", "workspace", "add", ws, "--name", filepath.Base(ws), "--revision", "main", "-R", clone)
	// The working copy of a task of the issue, where workspace.ProvisionTask makes it: a workspace
	// of the clone beside the issue's own, named for the task's branch (workspace.TaskDir).
	task, err := workspace.TaskDir(ws, "legion/LEGION-208.b")
	if err != nil {
		t.Fatal(err)
	}
	run.Run(clone, "jj", "workspace", "add", task, "--name", filepath.Base(task), "--revision", "main", "-R", clone)
	// taskHead is the task's working-copy commit, read without a snapshot, so it is the state the
	// launcher's read left.
	taskHead := func() string {
		t.Helper()
		return run.Out(task, "jj", "log", "-R", task, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id")
	}

	g := newSessionRig(t, Config{Workspace: ws})
	answer := func(id string) shimwire.WorkspaceWorkResult {
		t.Helper()
		got, ok := g.roleConn.answer(shimwire.WorkspaceWork{ID: id, Issue: "LEGION-208", TimeoutMs: 120_000}).(shimwire.WorkspaceWorkResult)
		if !ok || got.ID != id {
			t.Fatalf("the launcher answered %#v, want the result of %s", got, id)
		}
		if _, err := os.Stat(signed); err == nil {
			t.Fatalf("the launcher's read %s ran the signing program that the task's working copy names", id)
		}
		return got
	}
	// counted fails the test unless read id counted the task's working copy, and nothing else, as
	// its one work commit, the issue's own working copy holding no change.
	counted := func(id string) {
		t.Helper()
		work, refused, err := runtime.WorkspaceWorkOf(answer(id))
		if err != nil || refused != "" {
			t.Fatalf("the launcher's read %s = %+v (refused %q, %v), want the work it read", id, work, refused, err)
		}
		head := taskHead()
		if !work.HoldsWork() || work.Changed || !slices.Equal(work.Commits, []string{head}) || work.Count != 1 {
			t.Fatalf("the launcher's read %s lists %v and counts %d, changed %t, holding work %t; want the task's working copy %s, which holds the edit its agent left: a release takes the pod on a read that holds no work, and deletes the edit with its disk",
				id, work.Commits, work.Count, work.Changed, work.HoldsWork(), head)
		}
	}

	// The edit, with no jj command after it.
	if err := os.WriteFile(filepath.Join(task, "task.txt"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	counted("task-edit")
	if files := run.Out(task, "jj", "file", "list", "-R", task, "--ignore-working-copy", "-r", "@"); !slices.Contains(strings.Fields(files), "task.txt") {
		t.Fatalf("the task's working copy %s holds %q after the read, want task.txt in it: the read did not snapshot the edit its agent left there", taskHead(), files)
	}

	// The legacy configuration file a role can write in the task's .jj, with the id file beside it
	// deleted, which jj would migrate and load: the signing program, which the snapshot of a new
	// change would run. The read runs the guards in the task's working copy first, so jj loads no
	// such file, the program does not run, and the file is gone.
	if err := os.Remove(filepath.Join(task, ".jj", "workspace-config-id")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("delete the task's workspace-config-id, as a role could: %v", err)
	}
	legacy := filepath.Join(task, ".jj", "workspace-config.toml")
	if err := os.WriteFile(legacy, []byte("[signing]\nbehavior = \"force\"\nbackend = \"gpg\"\nbackends.gpg.program = \""+signer+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	counted("task-legacy-config")
	if _, err := os.Lstat(legacy); err == nil {
		t.Fatalf("the task's legacy configuration file %s is still there after the read, want it removed before jj ran", legacy)
	}

	// A task's working copy the guards refuse: its .jj/repo names another repository than the
	// shared clone that the layout puts beside the issue's workspace.
	if err := os.WriteFile(filepath.Join(task, ".jj", "repo"), []byte(filepath.Join(root, "another-repository")), 0o644); err != nil {
		t.Fatal(err)
	}
	if refused := answer("task-another-repository"); refused.OK || !strings.Contains(refused.Error, "refusing to run jj in "+task) {
		t.Fatalf("the read of a workspace whose task's .jj/repo names another repository = %+v, want it refused by the workspace guards in %s", refused, task)
	}
}
