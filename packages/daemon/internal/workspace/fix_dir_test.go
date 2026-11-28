package workspace

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// FixDir is the one owner of where a fix's working copy stands: beside the issue's own working copy,
// and nowhere where no working copy is placed, so the engine's task and the served state, which
// each ask it, can never name a fix's directory for an issue that has none. Its red is the rule
// before, kept by each caller: FixDir answered "-fix" for no directory at all.
func TestAFixsWorkingCopyStandsBesideTheIssuesAndNowhereElse(t *testing.T) {
	for _, c := range []struct{ dir, want string }{
		{"/work/acme/widgets/acme-42", "/work/acme/widgets/acme-42-fix"},
		{"", ""},
	} {
		if got := FixDir(c.dir); got != c.want {
			t.Errorf("FixDir(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// The fix's working copy sits beside each working copy Location places, in the same directory of
// workspaces, and is no other working copy Location names: the working copy of a task of the issue
// is named for the issue, a dot and the task's part, and the fix's name holds no dot; and it is no
// issue's own, since its name is no issue key.
func TestTheFixDirIsNoWorkingCopyLocationNames(t *testing.T) {
	for _, issue := range []string{"WIDGETS-42", "INFRA-7", "A-1"} {
		located, err := Location("/s", ghrepo.MustParse("acme/widgets"), issue)
		if err != nil {
			t.Fatalf("Location(%s): %v", issue, err)
		}
		fix := FixDir(located.Dir)
		if filepath.Dir(fix) != filepath.Dir(located.Dir) {
			t.Errorf("FixDir(%s) = %s, want a directory beside it", located.Dir, fix)
		}
		name := filepath.Base(fix)
		if strings.Contains(name, ".") {
			t.Errorf("FixDir(%s) = %s, whose name holds a dot, the form of a task's working copy beside the issue's", located.Dir, fix)
		}
		if claim.IsIssueKey(strings.ToUpper(name)) {
			t.Errorf("FixDir(%s) = %s, whose name is an issue key: Location names it for that issue", located.Dir, fix)
		}
	}
}
