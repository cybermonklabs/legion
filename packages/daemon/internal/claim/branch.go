package claim

import (
	"strconv"
	"strings"
)

// issueBranchPrefix begins the GitHub branch of every issue Legion works.
const issueBranchPrefix = "legion/"

// IssueBranch is the GitHub branch issue is worked on, legion/<KEY>: the jj bookmark of its
// workspace, the branch the daemon creates for it, and the head branch of its pull request.
func IssueBranch(issue string) string { return issueBranchPrefix + issue }

// BranchIssue is the issue whose branch (IssueBranch) branch is, and whether it is one: a branch
// that is not legion/ followed by an issue key is no issue's, and no pull request Legion records is
// on it, so a GitHub event about it changes nothing the daemon keeps.
func BranchIssue(branch string) (string, bool) {
	issue, ok := strings.CutPrefix(branch, issueBranchPrefix)
	if !ok || !IsIssueKey(issue) {
		return "", false
	}
	return issue, true
}

// besideBranchPrefix begins every branch a step beside a fix pushes to (BesideBranch). It is not
// issueBranchPrefix, so no reader of an issue's or a task's branch takes such a branch for one.
const besideBranchPrefix = "legion-beside/"

// BesideBranch is the GitHub branch that the step of role pushes its commits to while fix fix of
// task runs in the generation generation of issue: legion-beside/<issue>/<generation>/<task>/fix-<fix>/<role>.
// The fix's own branch is the implementer's. It answers "" where issue, task or role is empty or
// holds a byte outside [A-Za-z0-9_-], or where fix is below 1: a task's id is a plan block's id,
// which can hold any character, and such a part would be no one component of a git ref. A caller
// that gets "" has no branch to name, and errors.
func BesideBranch(issue string, generation uint64, task string, fix int, role Role) string {
	prefix := BesideFixPrefix(issue, generation, task, fix)
	if prefix == "" || !besidePart(string(role)) {
		return ""
	}
	return prefix + string(role)
}

// BesideFixPrefix is the start of every BesideBranch of fix fix of task, up to and with
// `fix-<fix>/`, or "" where BesideBranch would answer "" for every role.
func BesideFixPrefix(issue string, generation uint64, task string, fix int) string {
	prefix := BesideTaskPrefix(issue, generation, task)
	if prefix == "" || fix < 1 {
		return ""
	}
	return prefix + "fix-" + strconv.Itoa(fix) + "/"
}

// BesideTaskPrefix is the start of every BesideBranch of task in that generation, up to and with
// `<task>/`, or "" where issue or task is not a part BesideBranch takes.
func BesideTaskPrefix(issue string, generation uint64, task string) string {
	if !besidePart(issue) || !besidePart(task) {
		return ""
	}
	return besideBranchPrefix + issue + "/" + strconv.FormatUint(generation, 10) + "/" + task + "/"
}

// IsBesideBranch is whether branch is a name BesideBranch makes (ParseBesideBranch).
func IsBesideBranch(branch string) bool {
	_, _, _, _, _, ok := ParseBesideBranch(branch)
	return ok
}

// ParseBesideBranch is BesideBranch read backwards: the parts of a branch it makes, and ok only
// for such a branch, whose parts BesideBranch makes into the same name again.
func ParseBesideBranch(branch string) (issue string, generation uint64, task string, fix int, role Role, ok bool) {
	rest, found := strings.CutPrefix(branch, besideBranchPrefix)
	parts := strings.Split(rest, "/")
	if !found || len(parts) != 5 {
		return "", 0, "", 0, "", false
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	number, found := strings.CutPrefix(parts[3], "fix-")
	if err != nil || !found {
		return "", 0, "", 0, "", false
	}
	fix, err = strconv.Atoi(number)
	if err != nil || BesideBranch(parts[0], generation, parts[2], fix, Role(parts[4])) != branch {
		return "", 0, "", 0, "", false
	}
	return parts[0], generation, parts[2], fix, Role(parts[4]), true
}

// besidePart is whether part is one BesideBranch takes: not empty, and every byte in [A-Za-z0-9_-].
func besidePart(part string) bool {
	if part == "" {
		return false
	}
	for i := range len(part) {
		c := part[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
