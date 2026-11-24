package tmux

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// The directory a task names as its working copy (WorkspaceDir, which the daemon hands the
// workflow engine) is the directory the pane starts the agent in: the directory spawn's
// workspaceDir gives, which the pane's shell command enters before it starts the worker shim
// (shimShellCommand), for a repository's provisioned workspace and for a configuration with no
// repository alike.
func TestTheWorkspaceDirIsThePanesStartDirectory(t *testing.T) {
	stateDir := t.TempDir()
	r := &Runtime{stateDir: stateDir}
	for name, repository := range map[string]ghrepo.Repository{
		"a repository's workspace": ghrepo.MustParse("sjawhar/legion"),
		"no repository":            {},
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			spec.Repository = repository
			want, err := WorkspaceDir(stateDir, repository, spec.Issue)
			if err != nil {
				t.Fatal(err)
			}
			if !repository.IsZero() {
				if err := os.MkdirAll(want, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.workspaceDir(spec)
			if err != nil || got != want {
				t.Fatalf("the pane's workspace = %q, %v; want the WorkspaceDir %q", got, err, want)
			}
			command := shimShellCommand("socket", "", got, "legion", "unix:///stream", "/token", "", "omp")
			if enter := "cd " + shellprefix.Word(want) + " && "; !strings.Contains(command, enter) {
				t.Fatalf("the pane's shell command %q does not enter %q before the worker shim", command, enter)
			}
		})
	}
}
