package sandbox

import "testing"

// The directory a task names as its working copy (WorkspaceDir, which the daemon hands the
// workflow engine) is the working directory of the container the agent's process runs in, as the
// built manifest gives it: the two read one definition, so neither can move alone.
func TestTheWorkspaceDirIsTheAgentContainersWorkingDirectory(t *testing.T) {
	t.Parallel()
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	want, err := WorkspaceDir(spec.Repository, spec.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if got := workerOf(t, r, spec).WorkingDir; got != want || got == "" {
		t.Fatalf("the agent's container starts in %q, want the WorkspaceDir %q", got, want)
	}
}
