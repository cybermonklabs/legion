package daemon

import (
	"context"
	"fmt"
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
// issue's branch `legion/<KEY>`, and on each pod's disk the workspace the real provisioning makes,
// as `legion workspace-init` makes it in a pod. Its first init container fills the pod's feed from
// GitHub (`workspace-init fetch`); here a mirror of the remote stands for that fetch, which needs
// GitHub itself. Its second (`workspace-init provision`) runs workspace.Provision from the feed,
// which is what this runs: the shared clone from the feed, then the issue's workspace on its
// branch. So a pod's workspace holds what the remote holds when the pod started, and nothing the
// test wrote by hand anywhere else; a pod the controller makes again on the same disk is
// provisioned again over the workspace that disk kept, as the kubelet runs a restarted pod's init
// containers again.
//
// Every git and jj it runs, and every one the pods' role processes run, is under one home of its
// own (fixtureenv.Home, which the rig sets on the test process, since workspace.Provision reads
// the process's environment) with an identity of the test's. A push runs the git first on PATH,
// which in an issue pod is Legion's wrapper running the leak guard before it writes, so the
// environment names a stand-in of the shared check that clears every write, as the shim's own
// workspace tests name one (leakguardtest).
type idleRigWorkspaces struct {
	root string
	// remote is the bare repository standing for GitHub; repo is the repository the rig's issues
	// are on (liveRepo), whose GitHub URL each clone's origin names.
	remote string
	repo   ghrepo.Repository
	home   string
	// leakCheck is the stand-in of the shared public leak check every push of the rig runs.
	leakCheck string
	// tools is the git and jj provisioning runs, as `workspace-init` resolves them on PATH.
	tools map[string]string

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
	tools := map[string]string{}
	for _, tool := range []string{"git", "jj"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s is required for the rig's workspaces: %v", tool, err)
		}
		tools[tool] = path
	}
	root := t.TempDir()
	w := &idleRigWorkspaces{root: root, remote: filepath.Join(root, "remote.git"), repo: repo,
		home: filepath.Join(root, "home"), leakCheck: leakguardtest.Write(t, root, leakguardtest.Passes), tools: tools,
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

// provision readies the pod's disk as its init containers do: the feed filled from the remote,
// then workspace.Provision of the issue's workspace from that feed. It runs on the pod's own
// goroutine (idleRigPods.podReady), so it answers its failure rather than failing the test.
func (w *idleRigWorkspaces) provision(ctx context.Context, cluster, disk, pod, key string) error {
	if disk == "" {
		return fmt.Errorf("the pod %s has no disk", pod)
	}
	diskRoot := filepath.Join(w.root, "disks", disk)
	feed := filepath.Join(diskRoot, "feed")
	feedRepo, err := workspace.FeedRepository(feed, w.repo)
	if err != nil {
		return err
	}
	if _, err := os.Stat(feedRepo); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(feedRepo), 0o755); err != nil {
			return err
		}
		if err := w.run(ctx, diskRoot, "git", "clone", "--quiet", "--mirror", w.remote, feedRepo); err != nil {
			return err
		}
	} else if err := w.run(ctx, feedRepo, "git", "--git-dir="+feedRepo, "fetch", "--quiet", "--prune"); err != nil {
		return err
	}
	provisioned, err := workspace.Provision(ctx, workspace.NewRunner(workspace.CommandTimeout, w.tools), workspace.Request{
		StateDir: diskRoot, Repo: w.repo, Issue: key, DefaultBranch: "main", CredentialHelper: "!false",
		Source: workspace.FromFeed(feed), Log: func(string) {},
	})
	if err != nil {
		return fmt.Errorf("provision %s's workspace on disk %s: %w", key, disk, err)
	}
	w.mu.Lock()
	w.live[pod] = idleRigWorkspace{cluster: cluster, dir: provisioned.Dir}
	w.mu.Unlock()
	return nil
}

// run runs one git of a provision in dir, answering its failure with its output.
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
