package claim

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// A branch that a step beside a fix pushes to is no issue's branch and no task's: it does not start
// with legion/, the prefix every issue branch and every task branch has, so BranchIssue takes none
// of them, and no reader of the daemon takes it for the branch of a pull request. Each name reads
// back as its parts, its two prefixes start it, and git takes it as a branch name. A task id is a
// plan block's id, which can hold a space, and BesideBranch answers no name for it. The controls:
// BranchIssue takes an issue's own branch, and git refuses the name a task id with a space would
// give, so neither check passes for want of the power to fail.
func TestABesideBranchIsNoIssueOrTaskBranch(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	checkRef := func(name string) (string, error) {
		command := exec.Command(git, "check-ref-format", "--branch", name)
		command.Dir = t.TempDir()
		output, err := command.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}

	for _, c := range []struct {
		issue      string
		generation uint64
		task       string
		fix        int
		role       Role
	}{
		{"INFRA-1", 1, "default", 1, RoleImplementer},
		{"INFRA-5392", 3, "a", 2, RoleTester},
		{"ACME-42", 12, "task_b-2", 10, RoleArchitect},
	} {
		name := BesideBranch(c.issue, c.generation, c.task, c.fix, c.role)
		want := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/%s", c.issue, c.generation, c.task, c.fix, c.role)
		if name != want {
			t.Fatalf("BesideBranch(%q, %d, %q, %d, %s) = %q, want %q", c.issue, c.generation, c.task, c.fix, c.role, name, want)
		}
		if issue, ok := BranchIssue(name); ok {
			t.Errorf("BranchIssue(%q) = %q, true; want no issue's branch", name, issue)
		}
		if strings.HasPrefix(name, issueBranchPrefix) {
			t.Errorf("%q starts with %s, the prefix of every issue's and every task's branch", name, issueBranchPrefix)
		}
		if !IsBesideBranch(name) {
			t.Errorf("IsBesideBranch(%q) = false, want true", name)
		}
		issue, generation, task, fix, role, ok := ParseBesideBranch(name)
		if !ok || issue != c.issue || generation != c.generation || task != c.task || fix != c.fix || role != c.role {
			t.Errorf("ParseBesideBranch(%q) = %q, %d, %q, %d, %s, %t; want its parts", name, issue, generation, task, fix, role, ok)
		}
		fixPrefix := BesideFixPrefix(c.issue, c.generation, c.task, c.fix)
		taskPrefix := BesideTaskPrefix(c.issue, c.generation, c.task)
		if wantFix := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/", c.issue, c.generation, c.task, c.fix); fixPrefix != wantFix {
			t.Errorf("BesideFixPrefix = %q, want %q", fixPrefix, wantFix)
		}
		if wantTask := fmt.Sprintf("legion-beside/%s/%d/%s/", c.issue, c.generation, c.task); taskPrefix != wantTask {
			t.Errorf("BesideTaskPrefix = %q, want %q", taskPrefix, wantTask)
		}
		if !strings.HasPrefix(name, fixPrefix) || !strings.HasPrefix(fixPrefix, taskPrefix) {
			t.Errorf("%q does not start with its fix's prefix %q, or that with its task's %q", name, fixPrefix, taskPrefix)
		}
		if checked, err := checkRef(name); err != nil || checked != name {
			t.Errorf("git check-ref-format --branch %q = %q, %v; want the name taken as it is", name, checked, err)
		}
	}

	if name := BesideBranch("INFRA-1", 1, "a b", 1, RoleTester); name != "" {
		t.Errorf("BesideBranch of the task id %q = %q, want no name", "a b", name)
	}
	if prefix := BesideFixPrefix("INFRA-1", 1, "a b", 1); prefix != "" {
		t.Errorf("BesideFixPrefix of the task id %q = %q, want no prefix", "a b", prefix)
	}
	if prefix := BesideTaskPrefix("INFRA-1", 1, "a b"); prefix != "" {
		t.Errorf("BesideTaskPrefix of the task id %q = %q, want no prefix", "a b", prefix)
	}
	for _, branch := range []string{
		"",
		"legion/INFRA-1",
		"legion-beside/",
		"legion-beside/INFRA-1/1/a/fix-1",
		"legion-beside/INFRA-1/1/a/fix-1/tester/more",
		"legion-beside/INFRA-1/01/a/fix-1/tester",
		"legion-beside/INFRA-1/x/a/fix-1/tester",
		"legion-beside/INFRA-1/1/a/fix-0/tester",
		"legion-beside/INFRA-1/1/a/fix-+1/tester",
		"legion-beside/INFRA-1/1/a/1/tester",
		"legion-beside/INFRA-1/1/a b/fix-1/tester",
		"legion-beside/INFRA-1/1//fix-1/tester",
		"other/legion-beside/INFRA-1/1/a/fix-1/tester",
	} {
		if IsBesideBranch(branch) {
			t.Errorf("IsBesideBranch(%q) = true, want false", branch)
		}
		if _, _, _, _, _, ok := ParseBesideBranch(branch); ok {
			t.Errorf("ParseBesideBranch(%q) answered ok, want no beside branch", branch)
		}
	}

	// The controls.
	if issue, ok := BranchIssue("legion/INFRA-1"); !ok || issue != "INFRA-1" {
		t.Errorf("control: BranchIssue(%q) = %q, %t; want INFRA-1, true", "legion/INFRA-1", issue, ok)
	}
	spaced := "legion-beside/INFRA-1/1/a b/fix-1/tester"
	if checked, err := checkRef(spaced); err == nil {
		t.Errorf("control: git check-ref-format --branch %q = %q and took it, want a refusal", spaced, checked)
	}
}
