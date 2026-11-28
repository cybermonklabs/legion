package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// The issue these rows push the branches of, and the files each step writes in its working copy.
const (
	besideIssue = "WIDGETS-42"
	besideFirst = "red_test.txt"
	besideAgain = "red2_test.txt"
)

// besidePod is one pod of the issue as a node provisions it, with the real git and jj: a clone of
// origin holding its default branch and the issue's branch and nothing else of it, colocated with
// its git repository, and the issue's own workspace of that clone at the issue's branch, which is
// what LEGION_WORKSPACE names. Two of them stand for the pod before an update round and the pod
// after it, which share nothing but origin.
type besidePod struct {
	t     *testing.T
	jj    string
	home  string
	clone string
	dir   string // LEGION_WORKSPACE
}

// newBesidePod provisions one. jj is the real jj, read once by the row before any pod runs, and
// origin the bare repository both pods push to.
func newBesidePod(t *testing.T, jj, origin string) *besidePod {
	t.Helper()
	state := t.TempDir()
	p := &besidePod{
		t:     t,
		jj:    jj,
		home:  t.TempDir(),
		clone: filepath.Join(state, "repos", "github.com", "acme", "widgets"),
		dir:   filepath.Join(state, "workspaces", "acme", "widgets", strings.ToLower(besideIssue)),
	}
	for _, dir := range []string{p.clone, filepath.Dir(p.dir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git := fixturegit.Isolated(t, p.home)
	git.Run(p.clone, fixturegit.InitWorkTree("", "main")...)
	git.Run(p.clone, "git", "remote", "add", "origin", origin)
	git.Run(p.clone, "git", "fetch", "--quiet", "origin", "main", claim.IssueBranch(besideIssue))
	git.With("JJ_USER=setup", "JJ_EMAIL=setup@example.test").
		Run(p.clone, append([]string{jj}, fixturegit.JJInit(p.clone, true)[1:]...)...)
	p.jjIn(p.clone, "workspace", "add", p.dir, "--name", filepath.Base(p.dir), "--revision", claim.IssueBranch(besideIssue)+"@origin")
	return p
}

// newBesideOrigin is the repository on GitHub the pods clone, as a bare repository: its default
// branch main, the base, and the issue's branch, a commit of the task over it.
func newBesideOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin, seed := filepath.Join(dir, "widgets.git"), filepath.Join(dir, "seed")
	git := fixturegit.Isolated(t, t.TempDir())
	git.Run(dir, fixturegit.InitBare(origin, "main")...)
	git.Run(dir, fixturegit.InitWorkTree(seed, "main")...)
	for _, file := range []struct{ name, body, message string }{
		{"README.md", "widgets\n", "chore: the base"},
		{"feature.txt", "the task's change\n", "feat: the task's change"},
	} {
		if err := os.WriteFile(filepath.Join(seed, file.name), []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
		git.Run(seed, "git", "add", file.name)
		git.Run(seed, "git", "commit", "--quiet", "-m", file.message)
	}
	git.Run(seed, "git", "push", "--quiet", origin, "HEAD~1:refs/heads/main", "HEAD:refs/heads/"+claim.IssueBranch(besideIssue))
	return origin
}

// jjIn runs the real jj in dir on dir's own repository, under the pod's home and the test's own
// identity, and fails the test where jj does. It runs in dir, since jj reads the repository of the
// directory it runs in whatever -R names.
func (p *besidePod) jjIn(dir string, args ...string) string {
	p.t.Helper()
	command := exec.Command(p.jj, append(args, "-R", dir)...)
	command.Dir = dir
	command.Env = p.environment()
	output, err := command.CombinedOutput()
	if err != nil {
		p.t.Fatalf("jj %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

// environment is what a command of this pod runs with: nothing that reaches a workspace, remote or
// credential not the test's own, the pod's own home, the step's identity, and the workspace in
// LEGION_WORKSPACE, which the recipe's own commands read.
func (p *besidePod) environment() []string {
	return append(fixtureenv.Isolated(p.home), "JJ_USER=tester", "JJ_EMAIL=tester@example.test",
		"LEGION_WORKSPACE="+p.dir)
}

// shell runs command with sh in the pod's workspace, as an agent's shell runs a command a task's
// text gives it: the real jj first on PATH, since the recipe names a bare jj. It answers what the
// command printed and its exit status.
func (p *besidePod) shell(command string) (string, int) {
	p.t.Helper()
	run := exec.Command("sh", "-c", command)
	run.Dir = p.dir
	run.Env = append(p.environment(), "PATH="+filepath.Dir(p.jj)+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := run.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(string(output)), exit.ExitCode()
	}
	if err != nil {
		p.t.Fatalf("sh -c %s in %s: %v", command, p.dir, err)
	}
	return strings.TrimSpace(string(output)), 0
}

// commit writes name in the pod's working copy and commits it there, as a step beside a fix does.
func (p *besidePod) commit(name, message string) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.dir, name), []byte(message+"\n"), 0o644); err != nil {
		p.t.Fatal(err)
	}
	p.jjIn(p.dir, "commit", "-m", message)
}

// pushes runs the recipe the daemon gives a step beside a fix (BesidePushCommand) and holds its
// exit status to 0, logging everything it printed: the row reads the real jj's own answer, and a
// recipe this jj refuses is the fault these rows are about.
func (p *besidePod) pushes(branch string) {
	p.t.Helper()
	command := BesidePushCommand(branch)
	output, status := p.shell(command)
	p.t.Logf("exit %d: %s\n%s", status, command, output)
	if status != 0 {
		p.t.Fatalf("%s exited %d on this jj, want 0:\n%s", command, status, output)
	}
}

// besideBranchFiles is every file of branch's head on the bare origin.
func besideBranchFiles(t *testing.T, origin, branch string) []string {
	t.Helper()
	listed, err := fixturegit.Isolated(t, t.TempDir()).Try(origin, "git", "ls-tree", "-r", "--name-only", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("origin holds no %s: %v\n%s", branch, err, listed)
	}
	return strings.Fields(listed)
}

// A step beside a fix pushes to its own branch from the pod that replaced its own. An update round
// replaces the pod and saves nothing of its disk, so the step goes on in a clone that has never
// had its branch: its task tells it to fetch the branch and go on from <branch>@origin
// (prompts/go/worker-common.md), and then it runs this recipe again. A fetch leaves the row
// untracked, so without the recipe's track `jj bookmark set` makes a local bookmark of its own
// beside that row and the push is refused ("Non-tracking remote bookmark"), which leaves the
// step's work on a disk the next round deletes.
//
// Two pods of one origin stand for the pod before the round and the pod after it, each with a
// clone of its own holding origin's default branch and the issue's branch and nothing else. The
// first pod commits and runs the recipe, which creates the branch: that is the first-push path,
// where origin has no row to track. Its clone is then removed, as the round removes the pod. The
// second pod resumes as its task says and runs the same recipe, whose exit status is held to 0,
// after which origin's branch holds both steps' files. Every assertion reads files, never commit
// ids.
func TestAStepBesideAFixPushesItsBranchAgainFromAReplacementPod(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	branch, err := claim.BesideBranch(besideIssue, 1, "a", 1, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	origin := newBesideOrigin(t)

	first := newBesidePod(t, jj, origin)
	first.commit(besideFirst, "test: the red test")
	first.pushes(branch)
	if files := besideBranchFiles(t, origin, branch); !slices.Contains(files, besideFirst) {
		t.Fatalf("%s on origin holds %v after the first pod's push, want %s", branch, files, besideFirst)
	}
	for _, dir := range []string{first.dir, first.clone} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	second := newBesidePod(t, jj, origin)
	if held := second.jjIn(second.dir, "log", "-r", `remote_bookmarks(glob:"legion-beside/*")`, "--no-graph", "-T", `commit_id ++ "\n"`); held != "" {
		t.Fatalf("the replacement pod's clone already holds a branch beside the fix:\n%s", held)
	}
	// What the step's task tells it to do on a new pod: fetch its branch, then go on from its head.
	second.jjIn(second.dir, "git", "fetch", "--remote", "origin", "--branch", branch)
	second.jjIn(second.dir, "new", branch+"@origin")
	second.commit(besideAgain, "test: the second red test")
	second.pushes(branch)
	if files := besideBranchFiles(t, origin, branch); !slices.Contains(files, besideFirst) || !slices.Contains(files, besideAgain) {
		t.Errorf("%s on origin holds %v after the replacement pod's push, want %s and %s", branch, files, besideFirst, besideAgain)
	}
}
