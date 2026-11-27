package workspace

import "testing"

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
