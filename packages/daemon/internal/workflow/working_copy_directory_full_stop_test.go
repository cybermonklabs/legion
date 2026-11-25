package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The working copy line names a directory the deployment chose. Under tmux it stands under the
// operator's state directory (tmux.WorkspaceDir, workspace.Location), and that path is any path
// the operator's disk holds: it may hold a full stop followed by a space, as
// `/srv/legion v2. state` does. A send finds the kept line to write it again (workingCopyLine), and
// it must find the whole of a line whose directory holds those two bytes. A send that takes the
// line to end at them writes the new line and leaves the rest of the composed directory after it,
// so the worker is told a directory followed by a stray path. It does so at every send, a send
// with no correction included, and each send of the same task adds the stray path once more.
//
// A task of this release's shape, which opens with the line, is written whole. A task an earlier
// release composed, which holds the line after its continue line, is written whole or left as it
// was kept: nothing says where such a line ends but the directory itself, so leaving it is no
// defect, and a stray path after it is.
func TestAWorkingCopyDirectoryThatHoldsAFullStopAndASpaceIsWrittenWhole(t *testing.T) {
	const (
		root = "/srv/legion v2. state/workspaces/"
		old  = "Working copy: branch legion/LEGION-208, directory " + root + "acme/widgets/LEGION-208."
		now  = "Working copy: branch legion/LEGION-208, directory " + root + "zeta/tools/LEGION-208."
		// earlierOpening is the continue line a task an earlier release composed opens with, and the
		// space after it, where that release put the working copy line.
		earlierOpening = "Continue root. Issue: LEGION-208. Phase: reviewing. "
	)
	engine := stepEngine(nil)
	engine.cfg.WorkspaceDir = func(key string, repo ghrepo.Repository) string { return root + repo.String() + "/" + key }
	composed := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", Title: "root", Phase: phase.Reviewing}
	left, moved := ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("zeta/tools")
	at, err := standingText(composed, engine.cfg.WorkspaceDir, todaysWalk(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		kept string
		// at is where the daemon's working copy line stands in the kept task.
		at int
		// earlier is whether an earlier release composed the task, which a send may leave as kept.
		earlier bool
	}{
		{name: "a start", kept: task(composed, at, record.PhaseRow{}, nil, "")},
		{name: "a resume", kept: resumeText(composed, at)},
		{name: "a task an earlier release composed", kept: earlierOpening + old + " Resume the existing phase work.",
			at: len(earlierOpening), earlier: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.kept) < tc.at+len(old) || tc.kept[tc.at:tc.at+len(old)] != old {
				t.Fatalf("the kept task %q, want it to name %q at byte %d: the case's own premise", tc.kept, old, tc.at)
			}
			if same := engine.WorkingCopyAtSend(tc.kept, composed.Key, left); same != tc.kept {
				t.Errorf("the task sent with no correction = %q,\nwant it as it was composed, %q: the line names the directory the process works already", same, tc.kept)
			}
			whole := tc.kept[:tc.at] + now + tc.kept[tc.at+len(old):]
			sent := engine.WorkingCopyAtSend(tc.kept, composed.Key, moved)
			if sent != whole && (!tc.earlier || sent != tc.kept) {
				t.Errorf("the task sent after the correction = %q,\nwant %q: the working copy of zeta/tools in the daemon's line and every other byte as composed", sent, whole)
			}
			if again := engine.WorkingCopyAtSend(sent, composed.Key, moved); again != sent {
				t.Errorf("the same task sent a second time = %q,\nwant it as the first send left it, %q: a send of a line that already names the process's directory changes nothing", again, sent)
			}
		})
	}
}
