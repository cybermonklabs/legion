package claim

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// issueBranchPrefix begins the GitHub branch of every issue Legion works.
const issueBranchPrefix = "legion/"

// JoinBranchSuffix ends the join branch of a task that needs several tasks, its branch and this
// suffix: the branch the work of its needs is joined on, from which the task's branch starts, so
// that the task's pull request shows its own work alone. A join branch is no task's branch
// (notSlugs), and this is the one source of its name.
const JoinBranchSuffix = ".join"

// IssueBranch is the GitHub branch issue is worked on, legion/<KEY>: the jj bookmark of its
// workspace, the branch the daemon creates for it, and the head branch of its pull request.
func IssueBranch(issue string) string { return issueBranchPrefix + issue }

// BranchIssue is the issue whose branch branch is, and whether it is one: the issue's own branch
// (IssueBranch) and the branch of each task of its plan (TaskBranch) are the issue's. A branch of
// any other shape (BranchTask) is no issue's, and no pull request Legion records is on it, so a
// GitHub event about it changes nothing the daemon keeps.
func BranchIssue(branch string) (string, bool) {
	issue, _, ok := BranchTask(branch)
	return issue, ok
}

// taskSlug is the shape of a task's slug in its branch and its directory: a lower-case letter or
// digit, then at most 39 lower-case letters, digits and hyphens. It holds no dot, so the first dot
// of a task's branch ends the issue key, and a slug is a component of a git ref and of a path. A
// word of that shape can still be no slug (notSlugs).
var taskSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// notSlugs are the words of a slug's shape (taskSlug) that no slug may be, each for its reason.
var notSlugs = []string{
	// git refuses a ref whose name ends in `.lock` (git check-ref-format), so legion/<KEY>.lock
	// is no branch GitHub or jj can hold.
	"lock",
	// internal/workspace renames a working copy it removes to its directory and `.removing`
	// (removingSuffix), beside the working copies of the other tasks of its issue, so the working
	// copy of a task of this slug, `<key>.removing`, is the path the issue's own working copy is
	// renamed to and deleted from.
	"removing",
	// The join branch of the task that holds the issue's own branch is legion/<KEY>.join
	// (JoinBranchSuffix), so a task of this slug would work on that task's join.
	strings.TrimPrefix(JoinBranchSuffix, "."),
}

// laterGive is the part of a task's branch after the issue key for a later give of a slug in one
// generation (TaskBranchOfGive): the slug, a dot and the give's number, 2 or more, written with no
// leading zero. A slug holds no dot, so this part holds exactly one and a first give's part none:
// no first give of any slug is a later give of another.
var laterGive = regexp.MustCompile(`^([^.]+)\.([2-9]|[1-9][0-9]+)$`)

// IsTaskSlug reports whether slug is a task's slug: of a slug's shape (taskSlug) and none of the
// words no slug may be (notSlugs).
func IsTaskSlug(slug string) bool {
	return taskSlug.MatchString(slug) && !slices.Contains(notSlugs, slug)
}

// TaskBranch is the GitHub branch of a task of issue's plan: the issue's own branch (IssueBranch)
// for slug "", legion/<issue>.<slug> otherwise.
func TaskBranch(issue, slug string) string {
	if slug == "" {
		return IssueBranch(issue)
	}
	return IssueBranch(issue) + "." + slug
}

// TaskBranchOfGive is the branch of the give-th name slug takes in one generation of issue's plan:
// the slug's own branch (TaskBranch) for the first give, legion/<issue>.<slug>.<give> from the
// second on. A branch given to a task is never given to another task of its generation, so a task
// whose slug's branch went to a task that was then removed takes a later give.
func TaskBranchOfGive(issue, slug string, give int) string {
	if give <= 1 {
		return TaskBranch(issue, slug)
	}
	return TaskBranch(issue, slug) + "." + strconv.Itoa(give)
}

// BranchTask is the issue of a task's branch and the task's part of it, and whether branch is one:
// slug is "" for the issue's own branch, the slug for a slug's first give (TaskBranch) and the
// slug, a dot and the number for a later give (TaskBranchOfGive), so TaskBranch(issue, slug) is
// branch again. An issue key holds no dot, so the first dot after legion/<KEY> ends the key; a
// branch whose part after it is neither a slug (IsTaskSlug) nor a later give of one is no task's.
func BranchTask(branch string) (issue, slug string, ok bool) {
	name, ok := strings.CutPrefix(branch, issueBranchPrefix)
	if !ok {
		return "", "", false
	}
	key, slug, dotted := strings.Cut(name, ".")
	if !IsIssueKey(key) || dotted && !isTaskPart(slug) {
		return "", "", false
	}
	return key, slug, true
}

// isTaskPart reports whether part, a task's branch after legion/<KEY>., is a slug (IsTaskSlug) or
// a later give of one (laterGive).
func isTaskPart(part string) bool {
	if IsTaskSlug(part) {
		return true
	}
	given := laterGive.FindStringSubmatch(part)
	return given != nil && IsTaskSlug(given[1])
}
