package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// idleRigWorkspaces is the remote the rig's issue pods are provisioned from and the workspace of
// each pod while it runs: a local bare repository standing for GitHub, holding `main` and each
// issue's branch `legion/<KEY>`, and on each pod's disk a workspace where a pod's provisioning
// puts one (workspace.Location): one clone of the remote, and the issue's jj workspace of that
// clone on the issue's branch. The rig makes it with git and jj run on the remote directly
// (provision), not with workspace.Provision: since cybermonklabs/legion#5646 removed the feed a
// pod's provisioning could read from, Provision reaches the repository at
// https://github.com/<repo> alone (workspace.GitHubURL), and every process it starts may use no
// transport but https, so it can reach no local remote. So a pod's workspace holds what the
// remote holds when the pod starts, and nothing the test wrote by hand anywhere else; a pod the
// controller makes again on the same disk finds the workspace that disk kept and fetches the
// remote again, as a restarted pod's provisioning does over the workspace its disk kept.
//
// Every git and jj it runs, and every one the pods' role processes run, is under one home of its
// own (env) with an identity of the test's. The rig sets that home and identity on the test
// process too (fixtureenv.Home), so nothing it starts with the process's own environment reads
// the machine's. A push runs the git first on PATH, which in an issue pod is Legion's wrapper
// running the leak guard before it writes, so the environment names a stand-in of the shared
// check that clears every write, as the shim's own workspace tests name one (leakguardtest).
type idleRigWorkspaces struct {
	root string
	// remote is the bare repository standing for GitHub: the origin of each disk's clone, which
	// the pods' role processes push to. repo is the repository the rig's issues are on
	// (liveRepo), which places each disk's clone and workspace (workspace.Location).
	remote string
	repo   ghrepo.Repository
	home   string
	// leakCheck is the stand-in of the shared public leak check every push of the rig runs.
	leakCheck string

	mu sync.Mutex
	// live is each running pod's workspace, by pod name: where its role processes read and push.
	live map[string]idleRigWorkspace
}

// idleRigWorkspace is one running pod's workspace: the cluster that holds the pod, and its
// directory.
type idleRigWorkspace struct {
	cluster, dir string
}

func newIdleRigWorkspaces(t *testing.T, keys []string) *idleRigWorkspaces {
	t.Helper()
	repo, err := ghrepo.Parse("--repo", liveRepo)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required for the rig's workspaces: %v", tool, err)
		}
	}
	root := t.TempDir()
	w := &idleRigWorkspaces{root: root, remote: filepath.Join(root, "remote.git"), repo: repo,
		home: filepath.Join(root, "home"), leakCheck: leakguardtest.Write(t, root, leakguardtest.Passes),
		live: map[string]idleRigWorkspace{}}
	for _, dir := range []string{w.home, filepath.Join(root, "disks")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range append(fixtureenv.Home(w.home), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid") {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	git := fixturegit.Runner{TB: t, Env: w.env()}
	git.Run(root, fixturegit.InitBare(w.remote, "main")...)
	git.Run(seed, fixturegit.InitWorkTree(seed, "main")...)
	writeRigFile(t, filepath.Join(seed, "README.md"), "The project the rig's issues are on.\n")
	git.Run(seed, "git", "add", "-A")
	git.Run(seed, "git", "commit", "--quiet", "-m", "The project's main")
	git.Run(seed, "git", "remote", "add", "origin", w.remote)
	git.Run(seed, "git", "push", "--quiet", "origin", "main")
	// Each issue's branch holds a commit of its own, so a workspace provisioned on it is told
	// from one on main or on another issue's branch.
	for _, key := range keys {
		git.Run(seed, "git", "checkout", "--quiet", "-b", "legion/"+key, "main")
		writeRigFile(t, filepath.Join(seed, idleRigBranchFile(key)), "The branch of "+key+".\n")
		git.Run(seed, "git", "add", "-A")
		git.Run(seed, "git", "commit", "--quiet", "-m", "The branch of "+key)
		git.Run(seed, "git", "push", "--quiet", "origin", "legion/"+key)
	}
	return w
}

// idleRigBranchFile is the file the remote's branch of issue key holds and main does not.
func idleRigBranchFile(key string) string { return strings.ToLower(key) + ".txt" }

// env is the environment every git and jj of the rig's workspaces runs under, the role processes'
// included (idleRigAgents' workspace answer): the home of the workspaces, an identity, no
// transport but local files, and the leak check every push runs.
func (w *idleRigWorkspaces) env() []string {
	return append(fixtureenv.Isolated(w.home), "PATH="+os.Getenv("PATH"), "PUBLIC_LEAK_CHECK="+w.leakCheck)
}

// writeRigFile writes content to path, failing t when it cannot: the caller's own test, so a step
// that fails here fails as that step.
func writeRigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// provision readies the pod's disk for the issue's workspace (readyDisk) and records that
// workspace as the pod's. It runs on the pod's own goroutine (idleRigPods.podReady), so it answers
// its failure rather than failing the test.
func (w *idleRigWorkspaces) provision(ctx context.Context, cluster, disk, pod, key string) error {
	if disk == "" {
		return fmt.Errorf("the pod %s has no disk", pod)
	}
	diskRoot := filepath.Join(w.root, "disks", disk)
	ws, err := workspace.Location(diskRoot, w.repo, key)
	if err != nil {
		return err
	}
	if err := w.readyDisk(ctx, diskRoot, ws); err != nil {
		return fmt.Errorf("provision %s's workspace on disk %s: %w", key, disk, err)
	}
	w.mu.Lock()
	w.live[pod] = idleRigWorkspace{cluster: cluster, dir: ws.Dir}
	w.mu.Unlock()
	return nil
}

// readyDisk readies ws on the disk at diskRoot with git and jj run on the remote, as a pod's
// provisioning readies it from GitHub. On a disk that kept the workspace, the workspace stays as
// it is, with its commits, bookmarks and working-copy change, and is brought up to date as
// workspace.Provision brings a kept one: `jj workspace update-stale`, then a fetch of the remote.
// On a disk with none, the disk's clone of the remote is made, colocated as a pod's is, or
// fetched where a provision cut short left one; the issue's branch is tracked from origin, which
// makes the bookmark `legion/<KEY>` at origin's commit; and the issue's jj workspace is added on
// that bookmark with a working copy of its own on top, where createWorkspace starts the workspace
// of a branch GitHub has. The remote holds every issue's branch (newIdleRigWorkspaces), so no
// workspace starts on `main`, as createWorkspace starts one whose branch GitHub does not have.
func (w *idleRigWorkspaces) readyDisk(ctx context.Context, diskRoot string, ws workspace.Workspace) error {
	kept, err := idleRigPathExists(ws.Dir)
	if err != nil {
		return err
	}
	if kept {
		if err := w.run(ctx, ws.Dir, "jj", "workspace", "update-stale"); err != nil {
			return err
		}
		return w.run(ctx, ws.Dir, "jj", "git", "fetch")
	}
	// `jj git clone` makes the directories above the clone, and `jj workspace add` does not make
	// those above the workspace.
	if err := os.MkdirAll(filepath.Dir(ws.Dir), 0o755); err != nil {
		return err
	}
	cloned, err := idleRigPathExists(ws.Clone)
	if err != nil {
		return err
	}
	if cloned {
		err = w.run(ctx, ws.Clone, "jj", "git", "fetch")
	} else {
		err = w.run(ctx, diskRoot, "jj", "git", "clone", "--colocate", w.remote, ws.Clone)
	}
	if err != nil {
		return err
	}
	if err := w.run(ctx, ws.Clone, "jj", "bookmark", "track", ws.Bookmark, "--remote", "origin"); err != nil {
		return err
	}
	return w.run(ctx, ws.Clone, "jj", "workspace", "add", ws.Dir, "--name", filepath.Base(ws.Dir), "--revision", ws.Bookmark)
}

// idleRigPathExists is whether path exists, answering a stat that fails for any other reason.
func idleRigPathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// run runs one git or jj of a provision in dir under the rig's environment, bounded at two
// minutes, answering its failure with its output.
func (w *idleRigWorkspaces) run(ctx context.Context, dir string, argv ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, w.env()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	return nil
}

// podGone is the pod of cluster gone: its workspace is no running pod's any more. The workspace
// stays on the disk, which only the Sandbox's own delete takes.
func (w *idleRigWorkspaces) podGone(cluster, pod string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if live, ok := w.live[pod]; ok && live.cluster == cluster {
		delete(w.live, pod)
	}
}

// running is the workspace of the pod while it runs.
func (w *idleRigWorkspaces) running(pod string) (idleRigWorkspace, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	live, ok := w.live[pod]
	return live, ok
}

// dir is the workspace of the running pod, failing t when the pod runs none.
func (w *idleRigWorkspaces) dir(t *testing.T, pod string) string {
	t.Helper()
	live, ok := w.running(pod)
	if !ok {
		t.Fatalf("the pod %s runs no workspace", pod)
	}
	return live.dir
}

// jj runs one jj command in the pod's workspace, as an agent of the pod would, and answers its
// output.
func (w *idleRigWorkspaces) jj(t *testing.T, pod string, args ...string) string {
	t.Helper()
	dir := w.dir(t, pod)
	return fixturegit.Runner{TB: t, Env: w.env()}.Run(dir, append([]string{"jj", "-R", dir}, args...)...)
}

// work leaves work in the pod's workspace as its role would: each of commits a commit of its own
// file, described by its name, and then a change of the working copy when change names a file.
func (w *idleRigWorkspaces) work(t *testing.T, pod string, commits []string, change string) {
	t.Helper()
	dir := w.dir(t, pod)
	for _, name := range commits {
		writeRigFile(t, filepath.Join(dir, name), "The work "+name+" holds.\n")
		w.jj(t, pod, "commit", "-m", name)
	}
	if change != "" {
		writeRigFile(t, filepath.Join(dir, change), "The working copy's change "+change+".\n")
	}
}

// holds is whether the pod's workspace holds the file in its working copy.
func (w *idleRigWorkspaces) holds(t *testing.T, pod, file string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(w.dir(t, pod), file))
	return err == nil
}

// remoteHolds is whether the remote's branch of issue key holds the file.
func (w *idleRigWorkspaces) remoteHolds(t *testing.T, key, file string) bool {
	t.Helper()
	out, err := fixturegit.Runner{TB: t, Env: w.env()}.Try(w.root, "git", "--git-dir="+w.remote, "cat-file", "-e", "refs/heads/legion/"+key+":"+file)
	return err == nil && strings.TrimSpace(out) == ""
}
