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
// task runs in the generation generation of issue: legion-beside/<issue>/<generation>/<task>/fix-<fix>/<role>,
// with the task's id written as besideTask writes it, since a task's id is a plan block's id and
// can hold any byte. The fix's own branch is the implementer's. It answers "" only where issue or
// role is empty or holds a byte outside [A-Za-z0-9_-], or where fix is below 1: an issue's key, a
// role and a fix's number never do, so a caller that gets "" passed a wrong value, and errors.
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
// the task's part and its `/`, or "" where issue is not a part BesideBranch takes.
func BesideTaskPrefix(issue string, generation uint64, task string) string {
	if !besidePart(issue) {
		return ""
	}
	return besideBranchPrefix + issue + "/" + strconv.FormatUint(generation, 10) + "/" + besideTask(task) + "/"
}

// IsBesideBranch is whether branch is a name BesideBranch makes (ParseBesideBranch).
func IsBesideBranch(branch string) bool {
	_, _, _, _, _, ok := ParseBesideBranch(branch)
	return ok
}

// ParseBesideBranch is BesideBranch read backwards: the parts of a branch it makes, the task's id
// as its very bytes, and ok only for such a branch, whose parts BesideBranch makes into the same
// name again.
func ParseBesideBranch(branch string) (issue string, generation uint64, task string, fix int, role Role, ok bool) {
	rest, found := strings.CutPrefix(branch, besideBranchPrefix)
	parts := strings.Split(rest, "/")
	if !found || len(parts) != 5 {
		return "", 0, "", 0, "", false
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	number, found := strings.CutPrefix(parts[3], "fix-")
	task, read := taskOfBesidePart(parts[2])
	if err != nil || !found || !read {
		return "", 0, "", 0, "", false
	}
	fix, err = strconv.Atoi(number)
	if err != nil || BesideBranch(parts[0], generation, task, fix, Role(parts[4])) != branch {
		return "", 0, "", 0, "", false
	}
	return parts[0], generation, task, fix, Role(parts[4]), true
}

// besidePart is whether part is one BesideBranch takes as it stands: not empty, and every byte
// in [A-Za-z0-9_-].
func besidePart(part string) bool {
	if part == "" {
		return false
	}
	for i := range len(part) {
		if c := part[i]; !besideByte(c) && c != '_' {
			return false
		}
	}
	return true
}

// besideByte is whether besideTask writes c as it is: [A-Za-z0-9-].
func besideByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// besideTask is a task's id as one component of a git ref that holds the id's bytes and can be
// read back to them (taskOfBesidePart): each byte in [A-Za-z0-9-] as it is, every other byte, `_`
// itself included, as `_` and its two uppercase hex digits, and the empty id as `_` alone. So the
// part holds no byte outside [A-Za-z0-9_-], which git takes in a component at any place, and two
// ids are never written alike.
func besideTask(task string) string {
	if task == "" {
		return "_"
	}
	const hex = "0123456789ABCDEF"
	written := make([]byte, 0, len(task))
	for i := range len(task) {
		c := task[i]
		if besideByte(c) {
			written = append(written, c)
			continue
		}
		written = append(written, '_', hex[c>>4], hex[c&0x0f])
	}
	return string(written)
}

// taskOfBesidePart reads a part besideTask could have written back to the id's bytes, and ok
// where every `_` is followed by two hex digits or stands alone as the whole part. It does not ask
// whether besideTask writes the id so: ParseBesideBranch writes it again and compares.
func taskOfBesidePart(part string) (string, bool) {
	if part == "_" {
		return "", true
	}
	task := make([]byte, 0, len(part))
	for i := 0; i < len(part); {
		if part[i] != '_' {
			task = append(task, part[i])
			i++
			continue
		}
		if i+3 > len(part) {
			return "", false
		}
		value, err := strconv.ParseUint(part[i+1:i+3], 16, 8)
		if err != nil {
			return "", false
		}
		task = append(task, byte(value))
		i += 3
	}
	return string(task), true
}
