package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The engine names, in the working copy line each task opens with, the directory the runtime the
// daemon runs starts the issue's agents in: the pod's WorkspaceDir under Kubernetes, the pane's
// under tmux, for the repository the issue's record names
// (TestTheWorkspaceDirIsTheAgentContainersWorkingDirectory and
// TestTheWorkspaceDirIsThePanesStartDirectory hold each runtime's own process to that directory).
// The working copy of a task on a branch of its own is the one each runtime places under the
// task's working copy name (workspace.Name), beside the issue's, and a branch that is no task's of
// the issue has no working copy name. An issue whose record holds no repository, or one that does
// not parse, names the branch alone on both runtimes: no launch of such a record reaches the
// runtime (specs.workerSpawnSpec refuses it). A send to a process whose launch named none is asked
// of the runtime for the zero Repository, as its launch was: tmux starts such a pane in
// `<state_dir>/workspaces/<issue>`, and the Sandbox runtime refuses it, so the line names the
// branch alone there.
func TestEachTaskNamesTheDirectoryItsRuntimeStartsTheAgentIn(t *testing.T) {
	t.Parallel()
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion"}
	repo := ghrepo.MustParse("sjawhar/legion")
	stateDir := t.TempDir()
	places := map[string]func(name string) (string, error){
		"kubernetes": func(name string) (string, error) { return sandbox.WorkspaceDir(repo, name) },
		"tmux":       func(name string) (string, error) { return tmux.WorkspaceDir(stateDir, repo, name) },
	}
	unnamedPane, err := tmux.WorkspaceDir(stateDir, ghrepo.Repository{}, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	const branchAlone = "Working copy: branch legion/LEGION-208."
	// unnamed is each runtime's working copy line of a send to a process whose launch named no
	// repository.
	unnamed := map[string]string{
		"kubernetes": branchAlone,
		"tmux":       "Working copy: branch legion/LEGION-208, directory " + unnamedPane + ".",
	}
	for name, place := range places {
		t.Run(name, func(t *testing.T) {
			want := func(working string) string {
				t.Helper()
				dir, err := place(working)
				if err != nil || dir == "" {
					t.Fatalf("the %s runtime's directory of %s = %q, %v", name, working, dir, err)
				}
				return dir
			}
			cfg := engineConfig(config.Config{Runtime: config.Runtime{Name: name}, StateDir: stateDir}, "", nil)
			dir := cfg.WorkspaceDir
			own := want(issue.Key)
			if got := dir(issue.Key, repo); got != own {
				t.Fatalf("the engine's workspace of %s = %q, want the runtime's %q", issue.Key, got, own)
			}
			if got, want := workflow.WorkingCopy(issue, dir).String(), "Working copy: branch legion/LEGION-208, directory "+own+"."; got != want {
				t.Fatalf("the working copy line = %q, want %q", got, want)
			}
			for branch, working := range map[string]string{"legion/LEGION-208.b": "LEGION-208.b", "legion/LEGION-208.b.2": "LEGION-208.b.2"} {
				named, err := workspace.Name(branch)
				if err != nil || named != working {
					t.Fatalf("the working copy name of %s's task on %s = %q, %v, want %q", issue.Key, branch, named, err, working)
				}
				if got, task := dir(named, repo), want(working); got != task || got == own {
					t.Fatalf("the engine's workspace of %s's task on %s = %q, want the runtime's %q, beside the issue's %q", issue.Key, branch, got, task, own)
				}
			}
			if named, err := workspace.Name("legion/LEGION-208.removing"); err == nil {
				t.Fatalf("the working copy name of a branch that is no task's = %q, want none", named)
			}
			unparsed := issue
			unparsed.Repo = "not a repository"
			if got := workflow.WorkingCopy(unparsed, dir).String(); got != branchAlone {
				t.Fatalf("the working copy line of a repository that does not parse = %q, want %q", got, branchAlone)
			}
			unfilled := issue
			unfilled.Repo = ""
			if got := workflow.WorkingCopy(unfilled, dir).String(); got != branchAlone {
				t.Fatalf("the working copy line of a record that holds no repository = %q, want %q", got, branchAlone)
			}
			const rest = "\nContinue root. Issue: LEGION-208. Phase: implementing."
			engine := workflow.New(nil, cfg, quietLogger())
			if got, want := engine.WorkingCopyAtSend(branchAlone+rest, issue.Key, ghrepo.Repository{}), unnamed[name]+rest; got != want {
				t.Fatalf("the task sent to a process whose launch named no repository = %q, want %q", got, want)
			}
		})
	}
}
