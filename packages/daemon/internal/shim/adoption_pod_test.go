package shim_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// podRepository is the repository a pod workspace's provision clones, from a bare remote of the
// test's own that stands in for its GitHub URL (podRunner).
var podRepository = ghrepo.MustParse("acme/widgets")

// podWorkspace is an issue pod's workspace, as `legion workspace-init` makes one: dir is the
// workspace, a git worktree of the issue's clone registered with `jj workspace add
// --sparse-patterns empty` and then checked out with `jj sparse reset` (workspace.Provision), and
// home is the home every jj and git of the fixture, the provision's and the shim's read.
type podWorkspace struct {
	t         *testing.T
	home, dir string
	jj        string
}

// newPodWorkspace provisions WIDGETS-42's workspace of acme/widgets with workspace.Provision and
// the issue pod's source (workspace.ForIssuePod, with no repository cache), whose GitHub is a bare
// remote of the test's own holding README.md on main. jj is the jj every command of the fixture
// and the provision runs.
func newPodWorkspace(t *testing.T, jj string) podWorkspace {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	for _, dir := range []string{home, seed} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(seed, fixturegit.InitWorkTree(seed, "main")...)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.Run(seed, "git", "add", "README.md")
	fixture.Run(seed, "git", "commit", "--quiet", "-m", "the default branch")
	fixture.Run(seed, "git", "push", "--quiet", remote, "main")

	// Provisioning reaches a remote over https alone (GIT_ALLOW_PROTOCOL=https on each process it
	// starts), so its git is a wrapper that adds the file transport the stand-in remote takes.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the pod workspace's provision runs a real git: %v", err)
	}
	tools := map[string]string{}
	bin := t.TempDir()
	for name, path := range map[string]string{"git": git, "jj": jj} {
		wrapper := filepath.Join(bin, name)
		script := "#!/bin/sh\n[ -z \"${GIT_ALLOW_PROTOCOL+set}\" ] || export GIT_ALLOW_PROTOCOL=\"$GIT_ALLOW_PROTOCOL:file\"\nexec '" + path + "' \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		tools[name] = wrapper
	}
	const budget = 2 * time.Minute
	run := podRunner{remote: remote, Runner: workspace.NewRunnerIn(fixtureenv.Isolated(home), budget, tools)}
	logf := func(line string) { t.Logf("provisioning logged: %s", line) }
	token := func() (string, error) { return "ghs_test", nil }
	provisioned, err := workspace.Provision(context.Background(), run, workspace.Request{
		StateDir: filepath.Join(root, "legion"), Repo: podRepository, Issue: "WIDGETS-42", DefaultBranch: "main",
		CredentialHelper: workspace.GitHubCredentialHelper,
		Source:           workspace.ForIssuePod(token, t.TempDir(), "", "", clock.Real(), 0, logf),
		Log:              logf,
	})
	if err != nil {
		t.Fatalf("provision the pod workspace: %v", err)
	}
	return podWorkspace{t: t, home: home, dir: provisioned.Dir, jj: jj}
}

// podRunner runs provisioning's commands with the bare remote in place of GitHub: a git fetch or
// ls-remote that names the repository's GitHub URL names the remote's file URL instead.
type podRunner struct {
	remote string
	workspace.Runner
}

func (r podRunner) Run(ctx context.Context, command workspace.Command) (workspace.Result, error) {
	if command.Argv[0] == "git" && (slices.Contains(command.Argv, "fetch") || slices.Contains(command.Argv, "ls-remote")) {
		command.Argv = slices.Clone(command.Argv)
		for i, arg := range command.Argv {
			if arg == workspace.GitHubURL(podRepository) {
				command.Argv[i] = "file://" + r.remote
			}
		}
	}
	return r.Runner.Run(ctx, command)
}

// as runs one of the workspace's commands, the fixture's jj for "jj", in the workspace, under
// user's identity for both jj and git, and answers its standard output, failing the test when it
// fails.
func (w podWorkspace) as(user string, argv ...string) string {
	w.t.Helper()
	return strings.TrimSpace(w.runner(user).Out(w.dir, w.argv(argv)...))
}

// try is as for a command whose failure the caller reads: its combined output and its error.
func (w podWorkspace) try(user string, argv ...string) (string, error) {
	w.t.Helper()
	return w.runner(user).Try(w.dir, w.argv(argv)...)
}

func (w podWorkspace) runner(user string) fixturegit.Runner {
	email := user + "@example.test"
	return fixturegit.Isolated(w.t, w.home).With("JJ_USER="+user, "JJ_EMAIL="+email,
		"GIT_AUTHOR_NAME="+user, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_NAME="+user, "GIT_COMMITTER_EMAIL="+email)
}

func (w podWorkspace) argv(argv []string) []string {
	if argv[0] == "jj" {
		return append([]string{w.jj}, argv[1:]...)
	}
	return argv
}

// write writes each file of files, by its path in the workspace.
func (w podWorkspace) write(files map[string]string) {
	w.t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(w.dir, name), []byte(body), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
}

// adopt has the real shim adopt the workspace as the daemon asks it to before a phase task, under
// user's identity, with the fixture's jj first on PATH, and fails the test unless the shim answers
// that the adoption succeeded.
func (w podWorkspace) adopt(user string) {
	w.t.Helper()
	bin := w.t.TempDir()
	if err := os.Symlink(w.jj, filepath.Join(bin, "jj")); err != nil {
		w.t.Fatal(err)
	}
	w.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := socketPath(w.t)
	daemon := listen(w.t, path)
	child := newOMP(w.t)
	cfg := config(w.t, path, child)
	cfg.Env = append(fixtureenv.Isolated(w.home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+w.dir)
	run(w.t, cfg, newClock())
	p := daemon.accept(w.t)
	p.open(w.t)
	p.send(w.t, shimwire.AdoptWorkingCopy{ID: "adopt", JJUser: user, JJEmail: user + "@example.test", TimeoutMs: 60_000})
	if got, ok := p.next(w.t).(shimwire.AdoptWorkingCopyResult); !ok || !got.OK {
		w.t.Fatalf("the shim answered %#v, want a successful adoption", got)
	}
}

// The adoption snapshots nothing (--ignore-working-copy), so what a role wrote on disk and no jj
// command recorded is still on disk after it, and the next ordinary jj command takes it in. In a
// workspace an issue pod provisions, a git worktree of the issue's clone, both of the adoption's
// branches lose no such edit, a changed tracked file and a new file: on an undescribed @ the
// metaedit keeps @'s tree, and the next `jj status` takes both edits into @, authored by the
// adoption's identity; on a described @, `jj new` starts the incoming role on an empty commit of
// that tree, whose parent git's HEAD then names, and the next `jj status` takes both edits into
// that new commit, leaving the described one as it was. Either way that status finds the working
// copy fresh. The adoption imports no git HEAD either: a role's own `git commit` before it reaches
// jj at that status, the next ordinary jj command.
func TestAdoptWorkingCopyInAPodWorkspaceLosesNoEdit(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	edits := map[string]string{"README.md": "smoke, edited by a role\n", "new.txt": "a file no jj command recorded\n"}
	for _, tc := range []struct {
		name string
		// before is what the previous role ran once it wrote previous.txt, nil for nothing.
		before []string
		// gitCommit is whether a role commits the edits with git before the adoption.
		gitCommit bool
		// described is whether the adoption finds @ described and so runs `jj new`.
		described bool
	}{
		{name: "the metaedit of an undescribed working copy"},
		{name: "jj new over a described working copy", before: []string{"jj", "describe", "-m", "feat: the previous role's change"}, described: true},
		{name: "a role's git commit before the adoption", gitCommit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPodWorkspace(t, jj)
			var previous string
			if tc.before != nil {
				w.as("implementer", "jj", "metaedit", "--update-author")
				w.write(map[string]string{"previous.txt": "the previous role's work\n"})
				w.as("implementer", tc.before...)
				previous = w.as("implementer", "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "commit_id")
			}
			w.write(edits)
			var committed string
			if tc.gitCommit {
				w.as("implementer", "git", "add", "README.md", "new.txt")
				w.as("implementer", "git", "commit", "--quiet", "-m", "a role's git commit")
				committed = w.as("implementer", "git", "rev-parse", "HEAD")
			}

			w.adopt("tester")
			if tc.described {
				head := w.as("tester", "git", "rev-parse", "HEAD")
				parent := w.as("tester", "jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "commit_id")
				if head != previous || parent != previous {
					t.Fatalf("after jj new, git's HEAD is %s and the new @'s parent %s, want both the described commit %s", head, parent, previous)
				}
			}

			status, err := w.try("tester", "jj", "status")
			if err != nil || strings.Contains(strings.ToLower(status), "stale") {
				t.Fatalf("jj status after the adoption ended %v and printed:\n%s\nwant exit 0 and no stale working copy", err, status)
			}
			if tc.gitCommit {
				if got := w.as("tester", "jj", "--ignore-working-copy", "log", "-r", committed+" & ::@", "--no-graph", "-T", "commit_id"); got != committed {
					t.Fatalf("after jj status, jj finds %q of the role's git commit %s among @'s ancestors, want that commit", got, committed)
				}
				return
			}
			for name, body := range edits {
				if got := w.as("tester", "jj", "--ignore-working-copy", "file", "show", "-r", "@", "--", name); got != strings.TrimSpace(body) {
					t.Errorf("after jj status, @ holds %s as %q, want the edit %q that no jj command recorded before the adoption", name, got, strings.TrimSpace(body))
				}
			}
			if author := w.as("tester", "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", `author.name() ++ " " ++ author.email()`); author != "tester tester@example.test" {
				t.Errorf("@'s author after the adoption and jj status is %q, want the adoption's identity, tester tester@example.test", author)
			}
			if !tc.described {
				return
			}
			kept := w.as("tester", "jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", `commit_id ++ " " ++ author.name() ++ " " ++ description.first_line()`)
			if want := previous + " implementer feat: the previous role's change"; kept != want {
				t.Errorf("the new @'s parent after jj status is %q, want the described commit kept as it was, %q", kept, want)
			}
			if got := w.as("tester", "jj", "--ignore-working-copy", "file", "show", "-r", "@-", "--", "README.md"); got != "smoke" {
				t.Errorf("the described commit holds README.md as %q after jj status, want it as the previous role left it, %q", got, "smoke")
			}
		})
	}
}
