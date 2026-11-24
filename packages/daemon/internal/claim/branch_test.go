package claim

import (
	"fmt"
	"testing"
)

// An issue's branch and the issue of a branch are one rule read in both directions: every branch
// IssueBranch builds names its issue again, and a branch of any other shape is no issue's, so the
// daemon neither records nor acts on a pull request or a push there.
func TestIssueBranchAndBranchIssueAreOneRule(t *testing.T) {
	for _, issue := range []string{"ACME-1", "ACME-208", "A1-9000"} {
		branch := IssueBranch(issue)
		if branch != "legion/"+issue {
			t.Fatalf("IssueBranch(%q) = %q, want legion/%s", issue, branch, issue)
		}
		if got, ok := BranchIssue(branch); !ok || got != issue {
			t.Fatalf("BranchIssue(%q) = %q, %t; want %q, true", branch, got, ok, issue)
		}
	}
	for _, branch := range []string{
		"main",
		"legion",
		"legion/",
		"legion/main",
		"legion/acme-1",
		"legion/ACME-1/x",
		"legion/ACME-",
		"legion/-1",
		"release/legion/ACME-1",
		"",
	} {
		if issue, ok := BranchIssue(branch); ok {
			t.Fatalf("BranchIssue(%q) = %q, true; want no issue", branch, issue)
		}
	}
}

// A task of an issue's plan has a branch of its own: the issue's branch for no slug, the issue's
// branch, a dot and the slug otherwise, and for a later give of the same slug in one generation
// that branch, a dot and the give's number, 2 or more. Every form names its issue, so each GitHub
// event on a task's branch reaches its issue, and a branch whose part after the key is none of the
// forms is no task's and no issue's: a capital letter, a second word, a number below 2 or with a
// leading zero, and the words no slug may be (notSlugs), `lock`, `removing` and `join`.
func TestATaskBranchNamesItsIssueAndSlug(t *testing.T) {
	if got := TaskBranch("ACME-1", ""); got != "legion/ACME-1" {
		t.Fatalf("TaskBranch(no slug) = %q", got)
	}
	if got := TaskBranch("ACME-1", "d"); got != "legion/ACME-1.d" {
		t.Fatalf("TaskBranch(d) = %q", got)
	}
	for branch, want := range map[string][3]string{
		"legion/ACME-1":                {"ACME-1", "", "true"},
		"legion/ACME-1.d":              {"ACME-1", "d", "true"},
		"legion/ACME-1.t-0123456789ab": {"ACME-1", "t-0123456789ab", "true"},
		"legion/ACME-1.d.2":            {"ACME-1", "d.2", "true"},
		"legion/ACME-1.d-2":            {"ACME-1", "d-2", "true"},
		"legion/ACME-1.d.10":           {"ACME-1", "d.10", "true"},
		"legion/ACME-1.":               {"", "", "false"},
		"legion/ACME-1.D":              {"", "", "false"},
		"legion/ACME-1.d.e":            {"", "", "false"},
		"legion/ACME-1.d.1":            {"", "", "false"},
		"legion/ACME-1.d.02":           {"", "", "false"},
		"legion/ACME-1.d.2.3":          {"", "", "false"},
		"legion/ACME-1..2":             {"", "", "false"},
		"legion/ACME-1.lock":           {"", "", "false"},
		"legion/ACME-1.removing":       {"", "", "false"},
		"legion/ACME-1.removing.2":     {"", "", "false"},
		"legion/ACME-1.join":           {"", "", "false"},
		"legion/ACME-1.join.2":         {"", "", "false"},
		"main":                         {"", "", "false"},
	} {
		issue, slug, ok := BranchTask(branch)
		if issue != want[0] || slug != want[1] || fmt.Sprint(ok) != want[2] {
			t.Errorf("BranchTask(%q) = %q, %q, %v; want %v", branch, issue, slug, ok, want)
		}
		key, isIssue := BranchIssue(branch)
		if isIssue != (want[2] == "true") || key != want[0] {
			t.Errorf("BranchIssue(%q) = %q, %v", branch, key, isIssue)
		}
	}
}

// The first give of a slug in a generation is the slug's own branch, and each later give carries
// its number after a second dot. A slug holds no dot, so no first give of any slug, `d-2` among
// them, is a later give of another, and BranchTask reads each give back as the part TaskBranch
// writes.
func TestALaterGiveOfASlugIsANameNoFirstGiveHas(t *testing.T) {
	if got := TaskBranchOfGive("ACME-1", "d", 1); got != "legion/ACME-1.d" {
		t.Fatalf("TaskBranchOfGive(d, 1) = %q, want the slug's own branch", got)
	}
	later := TaskBranchOfGive("ACME-1", "d", 2)
	if later != "legion/ACME-1.d.2" {
		t.Fatalf("TaskBranchOfGive(d, 2) = %q", later)
	}
	if first := TaskBranchOfGive("ACME-1", "d-2", 1); first == later {
		t.Fatalf("the first give of slug d-2 and the second of d are both %q", first)
	}
	for _, give := range []int{1, 2, 3, 12} {
		branch := TaskBranchOfGive("ACME-1", "d", give)
		issue, part, ok := BranchTask(branch)
		if !ok || issue != "ACME-1" || TaskBranch(issue, part) != branch {
			t.Errorf("BranchTask(%q) = %q, %q, %v; want the give read back", branch, issue, part, ok)
		}
	}
}
