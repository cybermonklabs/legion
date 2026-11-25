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

// The engine names, in the working copy line each task opens with, the directory the runtime the
// daemon runs starts the issue's agents in: the pod's WorkspaceDir under Kubernetes, the pane's
// under tmux, for the repository the issue's record names
// (TestTheWorkspaceDirIsTheAgentContainersWorkingDirectory and
// TestTheWorkspaceDirIsThePanesStartDirectory hold each runtime's own process to that directory).
// An issue whose record holds no repository, or one that does not parse, names the branch alone on
// both runtimes: no launch of such a record reaches the runtime (specs.workerSpawnSpec refuses
// it). A send to a process whose launch named none is asked of the runtime for the zero
// Repository, as its launch was: tmux starts such a pane in `<state_dir>/workspaces/<issue>`, and
// the Sandbox runtime refuses it, so the line names the branch alone there.
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
	unnamedPane, err := tmux.WorkspaceDir(stateDir, ghrepo.Repository{}, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	const branchAlone = "Working copy: branch legion/LEGION-208."
	for name, tc := range map[string]struct {
		cfg  config.Config
		want string
		// unnamed is the working copy line of a send to a process whose launch named no repository.
		unnamed string
	}{
		"kubernetes": {config.Config{Runtime: config.Runtime{Name: "kubernetes"}, StateDir: stateDir}, pod, branchAlone},
		"tmux": {config.Config{Runtime: config.Runtime{Name: "tmux"}, StateDir: stateDir}, pane,
			"Working copy: branch legion/LEGION-208, directory " + unnamedPane + "."},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := engineConfig(tc.cfg, "", nil)
			dir := cfg.WorkspaceDir
			if got := dir(issue.Key, repo); got != tc.want || got == "" {
				t.Fatalf("the engine's workspace of %s = %q, want the runtime's %q", issue.Key, got, tc.want)
			}
			if got, want := workflow.WorkingCopy(issue, dir).String(), "Working copy: branch legion/LEGION-208, directory "+tc.want+"."; got != want {
				t.Fatalf("the working copy line = %q, want %q", got, want)
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
			const rest = " Continue root. Issue: LEGION-208. Phase: implementing."
			engine := workflow.New(nil, cfg, quietLogger())
			if got, want := engine.WorkingCopyAtSend(branchAlone+rest, issue.Key, ghrepo.Repository{}), tc.unnamed+rest; got != want {
				t.Fatalf("the task sent to a process whose launch named no repository = %q, want %q", got, want)
			}
		})
	}
}
