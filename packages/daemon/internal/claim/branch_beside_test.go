package claim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
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

// besideName is BesideBranch that fails the test on an error.
func besideName(t *testing.T, issue string, generation uint64, task string, fix int, role Role) string {
	t.Helper()
	name, err := BesideBranch(issue, generation, task, fix, role)
	if err != nil || name == "" {
		t.Fatalf("BesideBranch(%q, %d, %q, %d, %s) = %q, %v; want a name", issue, generation, task, fix, role, name, err)
	}
	return name
}

// partsOf is the five parts of a name BesideBranch made, after its prefix.
func partsOf(t *testing.T, name string) []string {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(name, besideBranchPrefix), "/")
	if len(parts) != 5 {
		t.Fatalf("%q has %d parts after %s, want 5", name, len(parts), besideBranchPrefix)
	}
	return parts
}

// hashed is a part cut to head and followed by `_` and the first digits lowercase hex digits of
// sha256 of whole, as BesideBranch writes a task's id or a role too long for its room.
func hashed(head, whole string, digits int) string {
	sum := sha256.Sum256([]byte(whole))
	return head + "_" + hex.EncodeToString(sum[:])[:digits]
}

// roomOf is the bytes a task's part may hold in a branch of issue at generation: GitHub's 255-byte
// ref less refs/heads/, the prefix and the separators (33 bytes), the issue's key, the generation, a
// fix of 10 digits and a role of 32 bytes.
func roomOf(issue string, generation uint64) int {
	return 255 - (33 + len(issue) + len(strconv.FormatUint(generation, 10)) + 10 + 32)
}

// A branch that a step beside a fix pushes to is no issue's branch and no task's: it does not start
// with legion/, the prefix every issue branch and every task branch has, so BranchIssue takes none
// of them. A task's id is a plan block's id, which can hold any byte, so the name writes each byte
// of it outside [A-Za-z0-9-] as `_` and two uppercase hex digits, the empty id as `_`, and an
// escaping longer than the room GitHub's 255-byte ref leaves cut on an escape's edge to 33 bytes
// less, then `_` and 32 lowercase hex digits of the id's sha256; a role over 32 bytes is cut to 15,
// then `_` and 16 hex digits. Every id gives a name git takes as a branch whose ref is at most 255
// bytes, with the longest fix and role too; each name matches back to its id among the ids it is
// given, and its two prefixes start it. An issue's key long enough to leave the task no room is
// an error. The controls: BranchIssue takes an issue's own branch, git refuses the names the raw
// ids would give, a name matches no id it was not made from, and the longest issue's key that
// leaves room still gives a name, so no check here passes for want of the power to fail.
func TestABesideBranchIsNoIssueOrTaskBranch(t *testing.T) {
	const issue = "ACME-42"
	room := roomOf(issue, 1)
	kept := room - 33
	plain255, accents := strings.Repeat("a", 255), strings.Repeat("é", 90)
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
		{issue, 12, "task_b-2", "task_5Fb-2", 10, RoleArchitect},
		{issue, 1, "q:1", "q_3A1", 1, RoleReviewer},
		{issue, 1, "décision", "d_C3_A9cision", 1, RoleImplementer},
		{issue, 1, "a b", "a_20b", 1, RoleTester},
		{issue, 1, "_", "_5F", 1, RoleTester},
		{issue, 1, "_5F", "_5F5F", 1, RoleTester},
		{issue, 1, "a/b", "a_2Fb", 1, RoleImplementer},
		{issue, 1, "@{-1}", "_40_7B-1_7D", 1, RoleTester},
		{issue, 1, strings.Repeat("a", room), strings.Repeat("a", room), 1, RoleTester},
		{issue, 1, strings.Repeat("a", room+1), hashed(strings.Repeat("a", kept), strings.Repeat("a", room+1), 32), 1, RoleTester},
		{issue, 1, plain255, hashed(strings.Repeat("a", kept), plain255, 32), 1, RoleImplementer},
		{issue, 1, accents, hashed(strings.Repeat("_C3_A9", kept/6), accents, 32), 1, RoleImplementer},
		{issue, 1, strings.Repeat("a", kept-1) + strings.Repeat("é", 20),
			hashed(strings.Repeat("a", kept-1), strings.Repeat("a", kept-1)+strings.Repeat("é", 20), 32), 1, RoleTester},
		{issue, 1, strings.Repeat("a", kept-2) + strings.Repeat("é", 20),
			hashed(strings.Repeat("a", kept-2), strings.Repeat("a", kept-2)+strings.Repeat("é", 20), 32), 1, RoleTester},
		// INFRA-5392 at generation 1 leaves 169 bytes, 136 of them before the hash: 45 escapes, the
		// last one the first byte of an é, with the longest fix and role.
		{"INFRA-5392", 1, strings.Repeat("é", 200),
			hashed(strings.Repeat("_C3_A9", 22)+"_C3", strings.Repeat("é", 200), 32), 2147483647, Role(strings.Repeat("r", 32))},
	}
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = c.task
	}
	// wellFormed holds what every name must be: its ref at most 255 bytes, taken by git as a
	// branch, read by IsBesideBranch as one, and no issue's branch.
	wellFormed := func(t *testing.T, name string) {
		t.Helper()
		if ref := "refs/heads/" + name; len(ref) > 255 {
			t.Errorf("%q is %d bytes, over GitHub's 255", ref, len(ref))
		}
		if checked, err := checkRefFormat(t, name); err != nil || checked != name {
			t.Errorf("git check-ref-format --branch %q = %q, %v; want the name taken as it is", name, checked, err)
		}
		if !IsBesideBranch(name) {
			t.Errorf("IsBesideBranch(%q) = false, want true", name)
		}
		if issue, ok := BranchIssue(name); ok || strings.HasPrefix(name, issueBranchPrefix) {
			t.Errorf("BranchIssue(%q) = %q, %t; want no issue's branch, and no name under %s", name, issue, ok, issueBranchPrefix)
		}
	}

	t.Run("each id gives a branch within GitHub's ref, and the branch matches back to its id", func(t *testing.T) {
		named := map[string]string{}
		for _, c := range cases {
			name := besideName(t, c.issue, c.generation, c.task, c.fix, c.role)
			want := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/%s", c.issue, c.generation, c.written, c.fix, c.role)
			if name != want {
				t.Errorf("BesideBranch(%q, %d, %q, %d, %s) = %q, want %q", c.issue, c.generation, c.task, c.fix, c.role, name, want)
				continue
			}
			wellFormed(t, name)
			if other, ok := named[name]; ok {
				t.Errorf("the task ids %q and %q both give %q", other, c.task, name)
			}
			named[name] = c.task
			issue, generation, task, fix, role, ok := MatchBesideBranch(name, ids)
			if !ok || issue != c.issue || generation != c.generation || task != c.task || fix != c.fix || role != c.role {
				t.Errorf("MatchBesideBranch(%q) = %q, %d, %q, %d, %s, %t; want its parts and the task %q", name, issue, generation, task, fix, role, ok, c.task)
			}
			fixPrefix, fixErr := BesideFixPrefix(c.issue, c.generation, c.task, c.fix)
			taskPrefix, taskErr := BesideTaskPrefix(c.issue, c.generation, c.task)
			if wantFix := fmt.Sprintf("legion-beside/%s/%d/%s/fix-%d/", c.issue, c.generation, c.written, c.fix); fixPrefix != wantFix || fixErr != nil {
				t.Errorf("BesideFixPrefix of the task %q = %q, %v; want %q", c.task, fixPrefix, fixErr, wantFix)
			}
			if wantTask := fmt.Sprintf("legion-beside/%s/%d/%s/", c.issue, c.generation, c.written); taskPrefix != wantTask || taskErr != nil {
				t.Errorf("BesideTaskPrefix of the task %q = %q, %v; want %q", c.task, taskPrefix, taskErr, wantTask)
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
			name := besideName(t, issue, 1, c.task, 1, RoleTester)
			if part := partsOf(t, name)[2]; part != c.written {
				t.Errorf("%s: the task %q is written %q, want %q", c.class, c.task, part, c.written)
			}
			wellFormed(t, name)
			if _, _, task, _, _, ok := MatchBesideBranch(name, []string{"other", c.task}); !ok || task != c.task {
				t.Errorf("%s: MatchBesideBranch(%q) = %q, %t; want the task %q", c.class, name, task, ok, c.task)
			}
			// The control: git refuses the name the raw id gives, so the escaping is what it takes.
			raw := "legion-beside/" + issue + "/1/" + c.task + "/fix-1/tester"
			if checked, err := checkRefFormat(t, raw); err == nil {
				t.Errorf("control, %s: git check-ref-format --branch %q = %q and took it, want a refusal", c.class, raw, checked)
			}
		}
	})

	t.Run("two ids alike in their first cut bytes each match their own branch", func(t *testing.T) {
		shared := strings.Repeat("a", room+10)
		one, two := shared+"x", shared+"y"
		first, second := besideName(t, issue, 1, one, 1, RoleTester), besideName(t, issue, 1, two, 1, RoleTester)
		head := strings.Repeat("a", kept) + "_"
		if first == second || !strings.HasPrefix(partsOf(t, first)[2], head) || !strings.HasPrefix(partsOf(t, second)[2], head) {
			t.Fatalf("the ids give %q and %q, want two names that each cut the shared bytes and differ in their hash", first, second)
		}
		for name, want := range map[string]string{first: one, second: two} {
			wellFormed(t, name)
			if _, _, task, _, _, ok := MatchBesideBranch(name, []string{one, two}); !ok || task != want {
				t.Errorf("MatchBesideBranch(%q, both ids) = %q, %t; want %q", name, task, ok, want)
			}
			// The control: a name matches no id it was not made from.
			other := two
			if want == two {
				other = one
			}
			if _, _, task, _, _, ok := MatchBesideBranch(name, []string{other}); ok {
				t.Errorf("control: MatchBesideBranch(%q, the other id alone) matched %q", name, task)
			}
		}
	})

	t.Run("a role over 32 bytes is cut with its hash", func(t *testing.T) {
		long := Role("a-role-of-more-than-thirty-two-bytes")
		name := besideName(t, issue, 1, "a", 1, long)
		if part, want := partsOf(t, name)[4], hashed(string(long[:15]), string(long), 16); part != want || len(part) != 32 {
			t.Errorf("the role %q is written %q, want %q, 32 bytes", long, part, want)
		}
		wellFormed(t, name)
	})

	t.Run("an issue's key that leaves the task no room is an error", func(t *testing.T) {
		over := strings.Repeat("A", 145) + "-1" // 147 bytes, with the generation's 1 byte 148
		if name, err := BesideBranch(over, 1, "a", 1, RoleTester); err == nil || name != "" || !strings.Contains(err.Error(), over) {
			t.Errorf("BesideBranch of a %d-byte key = %q, %v; want no name and an error naming the key", len(over), name, err)
		}
		if prefix, err := BesideFixPrefix(over, 1, "a", 1); err == nil || prefix != "" {
			t.Errorf("BesideFixPrefix of a %d-byte key = %q, %v; want an error", len(over), prefix, err)
		}
		if prefix, err := BesideTaskPrefix(over, 1, "a"); err == nil || prefix != "" {
			t.Errorf("BesideTaskPrefix of a %d-byte key = %q, %v; want an error", len(over), prefix, err)
		}
		// The control: a key one byte shorter leaves the task the 33 bytes a cut id needs, and
		// gives a name within GitHub's ref even for an id that must be cut.
		under := strings.Repeat("A", 144) + "-1"
		name := besideName(t, under, 1, plain255, 1234567890, Role(strings.Repeat("r", 32)))
		if part := partsOf(t, name)[2]; part != hashed("", plain255, 32) {
			t.Errorf("the task's part under a %d-byte key is %q, want the hash alone", len(under), part)
		}
		wellFormed(t, name)
	})

	t.Run("a name of any other form is no branch beside a fix", func(t *testing.T) {
		at := func(task string) string { return "legion-beside/INFRA-1/1/" + task + "/fix-1/tester" }
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
			"legion-beside/INFRA-1/1/a/fix-12345678901/tester",
			"legion-beside/INFRA-1/1/a/1/tester",
			"legion-beside/INFRA_1/1/a/fix-1/tester",
			"legion-beside/INFRA-1/1/a/fix-1/test_er",
			"legion-beside//1/a/fix-1/tester",
			"legion-beside/INFRA-1/1/a/fix-1/",
			"legion-beside/INFRA-1/1/a/fix-1/" + strings.Repeat("r", 33),
			"other/legion-beside/INFRA-1/1/a/fix-1/tester",
			at("a b"), at("a.b"), at(""), at("_41"), at("_2e"), at("a_2"), at("a_"), at("__"), at("_xy"),
			at(strings.Repeat("a", roomOf("INFRA-1", 1)+1)),
			at(strings.Repeat("a", roomOf("INFRA-1", 1)-33) + "_" + strings.Repeat("ab", 15) + "a"),
			at(strings.Repeat("a", roomOf("INFRA-1", 1)-32) + "_" + strings.Repeat("ab", 16)),
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
