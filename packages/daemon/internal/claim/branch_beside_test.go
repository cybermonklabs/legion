package claim

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// A branch that a step beside a fix pushes to is no issue's branch and no task's: it does not start
// with legion/, the prefix every issue branch and every task branch has, so BranchIssue takes none
// of them, and no reader of the daemon takes it for the branch of a pull request. A task's id is a
// plan block's id, which can hold any byte, so the name writes each byte of it outside
// [A-Za-z0-9-] as `_` and its two uppercase hex digits, and the empty id as `_`: every id, one of
// each class git refuses in a ref's component among them, gives a name git takes as a branch, two
// ids give two names, and each name reads back as its parts, the id's very bytes included, and its
// two prefixes start it. A name of any other form reads as no such branch. The controls:
// BranchIssue takes an issue's own branch, and git refuses the names the raw ids would give, so no
// check here passes for want of the power to fail.
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

	named := map[string]string{}
	for _, c := range []struct {
		issue      string
		generation uint64
		task       string
		written    string // the task's part of the name
		fix        int
		role       Role
	}{
		{"INFRA-1", 1, "default", "default", 1, RoleImplementer},
		{"INFRA-5392", 3, "a", "a", 2, RoleTester},
		{"ACME-42", 12, "task_b-2", "task_5Fb-2", 10, RoleArchitect},
		{"ACME-42", 1, "", "_", 1, RoleTester},
		{"ACME-42", 1, "_", "_5F", 1, RoleTester},
		{"ACME-42", 1, "_5F", "_5F5F", 1, RoleTester},
		{"ACME-42", 1, ".hidden", "_2Ehidden", 1, RoleTester},
		{"ACME-42", 1, "x.lock", "x_2Elock", 1, RoleTester},
		{"ACME-42", 1, "a..b", "a_2E_2Eb", 1, RoleTester},
		{"ACME-42", 1, "a\x01b\x7f", "a_01b_7F", 1, RoleTester},
		{"ACME-42", 1, "a b", "a_20b", 1, RoleTester},
		{"ACME-42", 1, "~", "_7E", 1, RoleTester},
		{"ACME-42", 1, "^", "_5E", 1, RoleTester},
		{"ACME-42", 1, ":", "_3A", 1, RoleTester},
		{"ACME-42", 1, "?", "_3F", 1, RoleTester},
		{"ACME-42", 1, "*", "_2A", 1, RoleTester},
		{"ACME-42", 1, "[", "_5B", 1, RoleTester},
		{"ACME-42", 1, `\`, "_5C", 1, RoleTester},
		{"ACME-42", 1, "@{-1}", "_40_7B-1_7D", 1, RoleTester},
		{"ACME-42", 1, "q:1", "q_3A1", 1, RoleReviewer},
		{"ACME-42", 1, "décision", "d_C3_A9cision", 1, RoleImplementer},
		{"ACME-42", 1, "a/b", "a_2Fb", 1, RoleImplementer},
	} {
		name := BesideBranch(c.issue, c.generation, c.task, c.fix, c.role)
		want := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/%s", c.issue, c.generation, c.written, c.fix, c.role)
		if name != want {
			t.Errorf("BesideBranch(%q, %d, %q, %d, %s) = %q, want %q", c.issue, c.generation, c.task, c.fix, c.role, name, want)
			continue
		}
		if other, ok := named[name]; ok {
			t.Errorf("the task ids %q and %q both give %q", other, c.task, name)
		}
		named[name] = c.task
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
			t.Errorf("ParseBesideBranch(%q) = %q, %d, %q, %d, %s, %t; want its parts, the task %q", name, issue, generation, task, fix, role, ok, c.task)
		}
		fixPrefix := BesideFixPrefix(c.issue, c.generation, c.task, c.fix)
		taskPrefix := BesideTaskPrefix(c.issue, c.generation, c.task)
		if wantFix := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/", c.issue, c.generation, c.written, c.fix); fixPrefix != wantFix {
			t.Errorf("BesideFixPrefix of the task %q = %q, want %q", c.task, fixPrefix, wantFix)
		}
		if wantTask := fmt.Sprintf("legion-beside/%s/%d/%s/", c.issue, c.generation, c.written); taskPrefix != wantTask {
			t.Errorf("BesideTaskPrefix of the task %q = %q, want %q", c.task, taskPrefix, wantTask)
		}
		if !strings.HasPrefix(name, fixPrefix) || !strings.HasPrefix(fixPrefix, taskPrefix) {
			t.Errorf("%q does not start with its fix's prefix %q, or that with its task's %q", name, fixPrefix, taskPrefix)
		}
		if checked, err := checkRef(name); err != nil || checked != name {
			t.Errorf("git check-ref-format --branch %q = %q, %v; want the name taken as it is", name, checked, err)
		}
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
		"legion-beside/INFRA-1/1/a.b/fix-1/tester",
		"legion-beside/INFRA-1/1//fix-1/tester",
		"legion-beside/INFRA-1/1/_41/fix-1/tester",
		"legion-beside/INFRA-1/1/_2e/fix-1/tester",
		"legion-beside/INFRA-1/1/a_2/fix-1/tester",
		"legion-beside/INFRA-1/1/a_/fix-1/tester",
		"legion-beside/INFRA-1/1/__/fix-1/tester",
		"legion-beside/INFRA-1/1/_xy/fix-1/tester",
		"legion-beside//1/a/fix-1/tester",
		"legion-beside/INFRA-1/1/a/fix-1/",
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
	for _, raw := range []string{"a b", "a..b", ".hidden", "x.lock", "~", "a\x01b"} {
		unwritten := "legion-beside/ACME-42/1/" + raw + "/fix-1/tester"
		if checked, err := checkRef(unwritten); err == nil {
			t.Errorf("control: git check-ref-format --branch %q = %q and took it, want a refusal", unwritten, checked)
		}
	}
}
