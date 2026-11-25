package claim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// checkRefFormat is git's own judgement of name as a branch: what `git check-ref-format --branch`
// prints, and its error where git refuses the name.
func checkRefFormat(t *testing.T, name string) (string, error) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	command := exec.Command(git, "check-ref-format", "--branch", name)
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// taskPartOf is the task's part of a name BesideBranch made: the third part after the prefix.
func taskPartOf(t *testing.T, name string) string {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(name, besideBranchPrefix), "/")
	if len(parts) != 5 {
		t.Fatalf("%q has %d parts after %s, want 5", name, len(parts), besideBranchPrefix)
	}
	return parts[2]
}

// hashed is the task's part BesideBranch writes for an id whose escaping is longer than 222 bytes:
// the escaping cut to head bytes, `_`, and the first 32 lowercase hex digits of sha256 of the id.
func hashed(head, id string) string {
	sum := sha256.Sum256([]byte(id))
	return head + "_" + hex.EncodeToString(sum[:])[:32]
}

// A branch that a step beside a fix pushes to is no issue's branch and no task's: it does not start
// with legion/, the prefix every issue branch and every task branch has, so BranchIssue takes none
// of them. A task's id is a plan block's id, which can hold any byte, so the name writes each byte
// of it outside [A-Za-z0-9-] as `_` and two uppercase hex digits, the empty id as `_`, and an
// escaping longer than 222 bytes cut on an escape's edge, then `_` and 32 hex digits of the id's
// sha256: every id gives a name git takes as a branch, whose task part is at most 255 bytes, the
// longest directory name the measured filesystems hold. Each name is matched back to its id among
// the ids it is given, and its two prefixes start it. The controls: BranchIssue takes an issue's
// own branch, git refuses the names the raw ids would give, and a name is matched to no id it was
// not made from, so no check here passes for want of the power to fail.
func TestABesideBranchIsNoIssueOrTaskBranch(t *testing.T) {
	plain255 := strings.Repeat("a", 255)
	accents := strings.Repeat("é", 90)
	cases := []struct {
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
		{"ACME-42", 1, "q:1", "q_3A1", 1, RoleReviewer},
		{"ACME-42", 1, "décision", "d_C3_A9cision", 1, RoleImplementer},
		{"ACME-42", 1, "a b", "a_20b", 1, RoleTester},
		{"ACME-42", 1, "_", "_5F", 1, RoleTester},
		{"ACME-42", 1, "_5F", "_5F5F", 1, RoleTester},
		{"ACME-42", 1, "a/b", "a_2Fb", 1, RoleImplementer},
		{"ACME-42", 1, "@{-1}", "_40_7B-1_7D", 1, RoleTester},
		{"ACME-42", 1, strings.Repeat("a", 222), strings.Repeat("a", 222), 1, RoleTester},
		{"ACME-42", 1, plain255, hashed(strings.Repeat("a", 222), plain255), 1, RoleImplementer},
		{"ACME-42", 1, accents, hashed(strings.Repeat("_C3_A9", 37), accents), 1, RoleImplementer},
		{"ACME-42", 1, strings.Repeat("a", 221) + "é", hashed(strings.Repeat("a", 221), strings.Repeat("a", 221)+"é"), 1, RoleTester},
		{"ACME-42", 1, strings.Repeat("a", 220) + "é", hashed(strings.Repeat("a", 220), strings.Repeat("a", 220)+"é"), 1, RoleTester},
		{"ACME-42", 1, strings.Repeat("a", 223), hashed(strings.Repeat("a", 222), strings.Repeat("a", 223)), 1, RoleTester},
	}
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = c.task
	}

	t.Run("each id gives a branch git takes, and the branch matches back to its id", func(t *testing.T) {
		named := map[string]string{}
		for _, c := range cases {
			name := BesideBranch(c.issue, c.generation, c.task, c.fix, c.role)
			want := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/%s", c.issue, c.generation, c.written, c.fix, c.role)
			if name != want {
				t.Errorf("BesideBranch(%q, %d, %q, %d, %s) = %q, want %q", c.issue, c.generation, c.task, c.fix, c.role, name, want)
				continue
			}
			if part := taskPartOf(t, name); len(part) > 255 {
				t.Errorf("the task part of %q is %d bytes, over 255", c.task, len(part))
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
			issue, generation, task, fix, role, ok := MatchBesideBranch(name, ids)
			if !ok || issue != c.issue || generation != c.generation || task != c.task || fix != c.fix || role != c.role {
				t.Errorf("MatchBesideBranch(%q) = %q, %d, %q, %d, %s, %t; want its parts and the task %q", name, issue, generation, task, fix, role, ok, c.task)
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
			if checked, err := checkRefFormat(t, name); err != nil || checked != name {
				t.Errorf("git check-ref-format --branch %q = %q, %v; want the name taken as it is", name, checked, err)
			}
		}
	})

	t.Run("each class of id git refuses in a ref gives a part git takes", func(t *testing.T) {
		for _, c := range []struct{ class, task, written string }{
			{"empty", "", "_"},
			{"a leading dot", ".hidden", "_2Ehidden"},
			{"a trailing .lock", "x.lock", "x_2Elock"},
			{"two dots", "a..b", "a_2E_2Eb"},
			{"a control byte", "a\x01b\x7f", "a_01b_7F"},
			{"a space", "a b", "a_20b"},
			{"a tilde", "~", "_7E"},
			{"a caret", "^", "_5E"},
			{"a colon", ":", "_3A"},
			{"a question mark", "?", "_3F"},
			{"an asterisk", "*", "_2A"},
			{"an open bracket", "[", "_5B"},
			{"a backslash", `\`, "_5C"},
		} {
			name := BesideBranch("ACME-42", 1, c.task, 1, RoleTester)
			if part := taskPartOf(t, name); part != c.written || len(part) > 255 {
				t.Errorf("%s: the task %q is written %q, want %q", c.class, c.task, part, c.written)
			}
			if checked, err := checkRefFormat(t, name); err != nil || checked != name {
				t.Errorf("%s: git check-ref-format --branch %q = %q, %v; want the name taken as it is", c.class, name, checked, err)
			}
			if _, _, task, _, _, ok := MatchBesideBranch(name, []string{"other", c.task}); !ok || task != c.task {
				t.Errorf("%s: MatchBesideBranch(%q) = %q, %t; want the task %q", c.class, name, task, ok, c.task)
			}
			// The control: git refuses the name the raw id gives, so the escaping is what it takes.
			raw := "legion-beside/ACME-42/1/" + c.task + "/fix-1/tester"
			if checked, err := checkRefFormat(t, raw); err == nil {
				t.Errorf("control, %s: git check-ref-format --branch %q = %q and took it, want a refusal", c.class, raw, checked)
			}
		}
	})

	t.Run("two ids alike in their first 222 escaped bytes each match their own branch", func(t *testing.T) {
		shared := strings.Repeat("a", 222)
		one, two := shared+"x", shared+"y"
		first, second := BesideBranch("ACME-42", 1, one, 1, RoleTester), BesideBranch("ACME-42", 1, two, 1, RoleTester)
		if first == second || !strings.HasPrefix(taskPartOf(t, first), shared+"_") || !strings.HasPrefix(taskPartOf(t, second), shared+"_") {
			t.Fatalf("the ids give %q and %q, want two names that each cut the shared 222 bytes and differ in their hash", first, second)
		}
		for name, want := range map[string]string{first: one, second: two} {
			if _, _, task, _, _, ok := MatchBesideBranch(name, []string{one, two}); !ok || task != want {
				t.Errorf("MatchBesideBranch(%q, both ids) = %q, %t; want %q", name, task, ok, want)
			}
			// The control: a name is matched to no id it was not made from.
			other := two
			if want == two {
				other = one
			}
			if _, _, task, _, _, ok := MatchBesideBranch(name, []string{other}); ok {
				t.Errorf("control: MatchBesideBranch(%q, the other id alone) matched %q", name, task)
			}
		}
	})

	t.Run("a name of any other form is no branch beside a fix", func(t *testing.T) {
		long := "legion-beside/INFRA-1/1/" + strings.Repeat("a", 223) + "/fix-1/tester"
		upper := "legion-beside/INFRA-1/1/" + strings.Repeat("a", 222) + "_" + strings.Repeat("AB", 16) + "/fix-1/tester"
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
			long,
			upper,
		} {
			if IsBesideBranch(branch) {
				t.Errorf("IsBesideBranch(%q) = true, want false", branch)
			}
			if _, _, task, _, _, ok := MatchBesideBranch(branch, append([]string{"a", "", "A", "a b"}, ids...)); ok {
				t.Errorf("MatchBesideBranch(%q) matched the task %q, want no beside branch", branch, task)
			}
		}
		// The control: BranchIssue takes an issue's own branch.
		if issue, ok := BranchIssue("legion/INFRA-1"); !ok || issue != "INFRA-1" {
			t.Errorf("control: BranchIssue(%q) = %q, %t; want INFRA-1, true", "legion/INFRA-1", issue, ok)
		}
	})
}
