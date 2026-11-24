package tmux

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A pane's Claude Code directory is named for its claim, which is its agent, never for the role of
// the step it runs: the implementer's agent on its tester step keeps the implementer's transcripts,
// and its tester generation resumes the transcript its implementer generation left there.
func TestATmuxClaudeAgentOnItsSecondRoleKeepsItsDirectory(t *testing.T) {
	stateDir := t.TempDir()
	host := Host{StateDir: stateDir, Legion: "/opt/legion"}
	spec := testSpec()
	spec.Claim, spec.Role = "legion-omp-legion-43-implementer", claim.RoleTester
	launcher := NewClaudeLauncher(testClaude)
	got, err := launcher.SessionsDir(spec, host, nil, "/state/workspaces/LEGION-43")
	if want := filepath.Join(ClaudeConfigDir(stateDir, spec.Claim), "projects"); err != nil || got != want {
		t.Fatalf("SessionsDir of %s on the role tester = %q, %v; want its own %s", spec.Claim, got, err, want)
	}
	if tester := filepath.Join(ClaudeConfigDir(stateDir, "legion-omp-legion-43-tester"), "projects"); got == tester {
		t.Fatalf("SessionsDir of %s on the role tester is the tester's %s", spec.Claim, tester)
	}
	const session = "9f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	spec.ResumeSessionFile = filepath.Join(got, "-state-workspaces-LEGION-43", session+".jsonl")
	inner, _, err := launcher.Launch(spec, host, filepath.Join(stateDir, "prompt.md"))
	if err != nil {
		t.Fatalf("launch the tester generation of %s resuming %s: %v", spec.Claim, spec.ResumeSessionFile, err)
	}
	if !strings.Contains(inner, session) {
		t.Errorf("the launch %q does not resume the session %s", inner, session)
	}
}
