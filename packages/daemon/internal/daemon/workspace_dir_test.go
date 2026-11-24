package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The engine names, in each task's working copy line, the directory the runtime the daemon runs
// starts the issue's agents in: the pod's WorkspaceDir under Kubernetes, the pane's under tmux, for
// the repository the issue's record names (TestTheWorkspaceDirIsTheAgentContainersWorkingDirectory
// and TestTheWorkspaceDirIsThePanesStartDirectory hold each runtime's own process to that
// directory). The working copy of a task on a branch of its own is the one each runtime places
// under the task's working copy name (workspace.Name), beside the issue's. An issue whose recorded
// repository does not parse, and a branch that is no task's of the issue, name the branch alone.
func TestEachTaskNamesTheDirectoryItsRuntimeStartsTheAgentIn(t *testing.T) {
	t.Parallel()
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion"}
	repo := ghrepo.MustParse("sjawhar/legion")
	stateDir := t.TempDir()
	places := map[string]func(name string) (string, error){
		"kubernetes": func(name string) (string, error) { return sandbox.WorkspaceDir(repo, name) },
		"tmux":       func(name string) (string, error) { return tmux.WorkspaceDir(stateDir, repo, name) },
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
			dir := engineConfig(config.Config{Runtime: config.Runtime{Name: name}, StateDir: stateDir}, "", nil).WorkspaceDir
			own := want(issue.Key)
			if got := dir(issue, "legion/LEGION-208"); got != own {
				t.Fatalf("the engine's workspace of %s = %q, want the runtime's %q", issue.Key, got, own)
			}
			if got, want := workflow.WorkingCopy(issue, dir).String(), " Working copy: branch legion/LEGION-208, directory "+own+"."; got != want {
				t.Fatalf("the working copy line = %q, want %q", got, want)
			}
			for branch, working := range map[string]string{"legion/LEGION-208.b": "LEGION-208.b", "legion/LEGION-208.b.2": "LEGION-208.b.2"} {
				if got, task := dir(issue, branch), want(working); got != task || got == own {
					t.Fatalf("the engine's workspace of %s's task on %s = %q, want the runtime's %q, beside the issue's %q", issue.Key, branch, got, task, own)
				}
			}
			if got := dir(issue, "legion/LEGION-208.removing"); got != "" {
				t.Fatalf("the engine's workspace of a branch that is no task's = %q, want none", got)
			}
			unparsed := issue
			unparsed.Repo = "not a repository"
			if got, want := workflow.WorkingCopy(unparsed, dir).String(), " Working copy: branch legion/LEGION-208."; got != want {
				t.Fatalf("the working copy line of a repository that does not parse = %q, want %q", got, want)
			}
		})
	}
}
