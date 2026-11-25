package claim

import (
	"crypto/sha256"
	"encoding/hex"
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

// The bound of a task's part of a BesideBranch: an escaping of the id longer than
// besideTaskEscaped bytes is cut to at most that many, on an escape's edge, and followed by `_` and
// the first besideTaskHash lowercase hex digits of sha256 of the id. 222 + 1 + 32 is 255 bytes, the
// longest directory name git, jj and the clone of a fetch were measured to hold: a loose ref keeps
// each part of its name as a directory.
const (
	besideTaskEscaped = 222
	besideTaskHash    = 32
)

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

// IsBesideBranch is whether branch has the form of a name BesideBranch makes: its prefix, an issue
// and a role BesideBranch takes, a generation and a fix as it writes them, and a task's part
// besideTask can write. It says nothing of which task the name is of; MatchBesideBranch does.
func IsBesideBranch(branch string) bool {
	_, _, _, _, ok := besideParts(branch)
	return ok
}

// MatchBesideBranch is the parts of branch and the one of tasks it is of: the task whose
// BesideBranch of the name's issue, generation, fix and role is branch, and ok where one is. A name
// is not read back to its task, since a cut id is written with part of its hash, so the caller
// gives the ids of the tasks the name can be of, which it holds (the fixes of that issue,
// generation and number).
func MatchBesideBranch(branch string, tasks []string) (issue string, generation uint64, task string, fix int, role Role, ok bool) {
	issue, generation, fix, role, ok = besideParts(branch)
	if !ok {
		return "", 0, "", 0, "", false
	}
	for _, task := range tasks {
		if BesideBranch(issue, generation, task, fix, role) == branch {
			return issue, generation, task, fix, role, true
		}
	}
	return "", 0, "", 0, "", false
}

// besideParts is the parts of branch BesideBranch writes as they are, and ok where branch has the
// form IsBesideBranch names.
func besideParts(branch string) (issue string, generation uint64, fix int, role Role, ok bool) {
	rest, found := strings.CutPrefix(branch, besideBranchPrefix)
	parts := strings.Split(rest, "/")
	if !found || len(parts) != 5 || !besidePart(parts[0]) || !besideTaskPart(parts[2]) || !besidePart(parts[4]) {
		return "", 0, 0, "", false
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || strconv.FormatUint(generation, 10) != parts[1] {
		return "", 0, 0, "", false
	}
	number, found := strings.CutPrefix(parts[3], "fix-")
	fix, err = strconv.Atoi(number)
	if !found || err != nil || fix < 1 || strconv.Itoa(fix) != number {
		return "", 0, 0, "", false
	}
	return parts[0], generation, fix, Role(parts[4]), true
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

// besideTask is a task's id as one component of a git ref: its escaping, each byte in
// [A-Za-z0-9-] as it is and every other byte, `_` itself included, as `_` and its two uppercase hex
// digits, and the empty id as `_` alone. An escaping longer than besideTaskEscaped bytes is cut to
// the longest run of whole bytes' escapings within it and followed by `_` and the first
// besideTaskHash lowercase hex digits of sha256 of the id, so the part is at most 255 bytes. So
// the part holds no byte outside [A-Za-z0-9_-], which git takes in a component at any place, and
// two ids are written alike only where their cut escapings and their hashes' first 128 bits are.
func besideTask(task string) string {
	if task == "" {
		return "_"
	}
	const digits = "0123456789ABCDEF"
	written := make([]byte, 0, len(task))
	cut := 0
	for i := range len(task) {
		if len(written) <= besideTaskEscaped {
			cut = len(written)
		}
		if c := task[i]; besideByte(c) {
			written = append(written, c)
		} else {
			written = append(written, '_', digits[c>>4], digits[c&0x0f])
		}
	}
	if len(written) <= besideTaskEscaped {
		return string(written)
	}
	sum := sha256.Sum256([]byte(task))
	return string(written[:cut]) + "_" + hex.EncodeToString(sum[:])[:besideTaskHash]
}

// besideTaskPart is whether part is one besideTask can write: `_` alone, an escaping of at most
// besideTaskEscaped bytes, or an escaping cut to within the last three bytes of that bound followed
// by `_` and besideTaskHash lowercase hex digits.
func besideTaskPart(part string) bool {
	switch head := len(part) - besideTaskHash - 1; {
	case part == "_":
		return true
	case len(part) <= besideTaskEscaped:
		return part != "" && besideEscaping(part)
	case head < besideTaskEscaped-2 || head > besideTaskEscaped || part[head] != '_':
		return false
	default:
		for i := head + 1; i < len(part); i++ {
			if c := part[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return besideEscaping(part[:head])
	}
}

// besideEscaping is whether s is an escaping besideTask writes: bytes in [A-Za-z0-9-] as they are,
// and `_` followed by two uppercase hex digits that name a byte outside that set.
func besideEscaping(s string) bool {
	for i := 0; i < len(s); {
		if besideByte(s[i]) {
			i++
			continue
		}
		if s[i] != '_' || i+3 > len(s) || !upperHex(s[i+1]) || !upperHex(s[i+2]) {
			return false
		}
		if value, _ := strconv.ParseUint(s[i+1:i+3], 16, 8); besideByte(byte(value)) {
			return false
		}
		i += 3
	}
	return true
}

// upperHex is whether c is a hex digit as besideTask writes one: [0-9A-F].
func upperHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }
