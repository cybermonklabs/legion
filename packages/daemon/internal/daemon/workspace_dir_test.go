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
// directory). An issue whose recorded repository does not parse names the branch alone.
func TestEachTaskNamesTheDirectoryItsRuntimeStartsTheAgentIn(t *testing.T) {
	t.Parallel()
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion"}
	repo := ghrepo.MustParse("sjawhar/legion")
	stateDir := t.TempDir()
	pod, err := sandbox.WorkspaceDir(repo, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tmux.WorkspaceDir(stateDir, repo, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		cfg  config.Config
		want string
	}{
		"kubernetes": {config.Config{Runtime: config.Runtime{Name: "kubernetes"}, StateDir: stateDir}, pod},
		"tmux":       {config.Config{Runtime: config.Runtime{Name: "tmux"}, StateDir: stateDir}, pane},
	} {
		t.Run(name, func(t *testing.T) {
			dir := engineConfig(tc.cfg, "", nil).WorkspaceDir
			if got := dir(issue); got != tc.want || got == "" {
				t.Fatalf("the engine's workspace of %s = %q, want the runtime's %q", issue.Key, got, tc.want)
			}
			if got, want := workflow.WorkingCopy(issue, dir).String(), " Working copy: branch legion/LEGION-208, directory "+tc.want+"."; got != want {
				t.Fatalf("the working copy line = %q, want %q", got, want)
			}
			unparsed := issue
			unparsed.Repo = "not a repository"
			if got, want := workflow.WorkingCopy(unparsed, dir).String(), " Working copy: branch legion/LEGION-208."; got != want {
				t.Fatalf("the working copy line of a repository that does not parse = %q, want %q", got, want)
			}
		})
	}
}
