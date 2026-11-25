package claim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// The bounds of a BesideBranch. GitHub refuses a ref longer than besideRefMax bytes, refs/heads/
// included (its GH005), and besideRefFixed is the bytes of such a ref that hold no part of the
// branch: refs/heads/, the prefix, and the separators with `fix-`. The task's part is one string for
// every fix and every step of the task, so its room leaves place for a fix of besideFixDigits digits
// and a role's part of besideRoleMax bytes. A part that does not fit is cut and followed by `_` and
// lowercase hex digits of sha256: besideTaskHash of them for a task's id, besideRoleHash for a role
// cut to besideRoleKept bytes.
const (
	besideRefMax    = 255
	besideRefFixed  = len("refs/heads/") + len(besideBranchPrefix) + len("/") + len("/") + len("/fix-") + len("/")
	besideFixDigits = 10
	besideRoleMax   = 32
	besideRoleKept  = 15
	besideRoleHash  = 16
	besideTaskHash  = 32
)

// BesideBranch is the GitHub branch that the step of role pushes its commits to while fix fix of
// task runs in the generation generation of issue: legion-beside/<issue>/<generation>/<T>/fix-<fix>/<R>,
// whose ref, refs/heads/ included, is at most 255 bytes. The fix's own branch is the implementer's.
// T is the task's id as besideTask writes it, since a task's id is a plan block's id and can hold
// any byte; R is the role as besideRole writes it. It answers an error only where issue or role is
// empty or holds a byte outside [A-Za-z0-9-], where fix is below 1 or above 10 digits, or where the
// issue's key and the generation leave the task's part no room for a cut id (besideTaskRoom): an
// issue's key, a role and a fix's number never do, so the error is the caller's wrong value.
func BesideBranch(issue string, generation uint64, task string, fix int, role Role) (string, error) {
	prefix, err := BesideFixPrefix(issue, generation, task, fix)
	if err != nil {
		return "", err
	}
	if !besidePart(string(role)) {
		return "", fmt.Errorf("the branch beside fix %d of %s's task %q: the role %q is empty or holds a byte outside [A-Za-z0-9-]", fix, issue, task, role)
	}
	return prefix + besideRole(role), nil
}

// BesideFixPrefix is the start of every BesideBranch of fix fix of task, up to and with
// `fix-<fix>/`, and BesideBranch's error where it would answer one for every role.
func BesideFixPrefix(issue string, generation uint64, task string, fix int) (string, error) {
	prefix, err := BesideTaskPrefix(issue, generation, task)
	if err != nil {
		return "", err
	}
	number := strconv.Itoa(fix)
	if fix < 1 || len(number) > besideFixDigits {
		return "", fmt.Errorf("the branch beside fix %d of %s's task %q: a fix is numbered from 1, in at most %d digits", fix, issue, task, besideFixDigits)
	}
	return prefix + "fix-" + number + "/", nil
}

// BesideTaskPrefix is the start of every BesideBranch of task in that generation, up to and with
// the task's part and its `/`, and an error where issue is empty or holds a byte outside
// [A-Za-z0-9-], or where the issue's key and the generation leave no room for a cut id.
func BesideTaskPrefix(issue string, generation uint64, task string) (string, error) {
	if !besidePart(issue) {
		return "", fmt.Errorf("the branch beside a fix of %q's task %q: the issue is empty or holds a byte outside [A-Za-z0-9-]", issue, task)
	}
	written := strconv.FormatUint(generation, 10)
	room := besideTaskRoom(issue, written)
	if room-besideTaskHash-1 < 0 {
		return "", fmt.Errorf("the branch beside a fix of %s at generation %s: the issue's key and the generation take %d bytes, which leaves the task's part %d bytes, under the %d a cut task id needs within GitHub's %d-byte ref",
			issue, written, len(issue)+len(written), room, besideTaskHash+1, besideRefMax)
	}
	return besideBranchPrefix + issue + "/" + written + "/" + besideTask(task, room) + "/", nil
}

// besideTaskRoom is the most bytes a task's part of a BesideBranch of issue at the generation
// written may hold: what GitHub's ref leaves after its fixed bytes, the issue's key, the generation,
// a fix of besideFixDigits digits and a role's part of besideRoleMax bytes. It is 169 for an issue
// key of 10 bytes at generation 1.
func besideTaskRoom(issue, generation string) int {
	return besideRefMax - (besideRefFixed + len(issue) + len(generation) + besideFixDigits + besideRoleMax)
}

// IsBesideBranch is whether branch has the form of a name BesideBranch makes: its prefix, an issue
// BesideBranch takes, a generation and a fix as it writes them, a task's part besideTask can write
// in the room the issue and the generation leave, and a role's part besideRole can write. It says
// nothing of which task the name is of; MatchBesideBranch does.
func IsBesideBranch(branch string) bool {
	_, _, _, _, ok := besideParts(branch)
	return ok
}

// MatchBesideBranch is the parts of branch and the one of tasks it is of: the task whose
// BesideBranch of the name's issue, generation, fix and role is branch, and ok where one is. A name
// is not read back to its task, since a cut id is written with part of its hash, so the caller
// gives the ids of the tasks the name can be of, which it holds (the fixes of that issue,
// generation and number). A role's part that besideRole cut names no role Legion has, since each
// is at most 32 bytes, and matches nothing.
func MatchBesideBranch(branch string, tasks []string) (issue string, generation uint64, task string, fix int, role Role, ok bool) {
	issue, generation, fix, role, ok = besideParts(branch)
	if !ok {
		return "", 0, "", 0, "", false
	}
	for _, task := range tasks {
		if name, err := BesideBranch(issue, generation, task, fix, role); err == nil && name == branch {
			return issue, generation, task, fix, role, true
		}
	}
	return "", 0, "", 0, "", false
}

// besideParts is the parts of branch that BesideBranch writes as they are, the role's part as the
// role, and ok where branch has the form IsBesideBranch names.
func besideParts(branch string) (issue string, generation uint64, fix int, role Role, ok bool) {
	rest, found := strings.CutPrefix(branch, besideBranchPrefix)
	parts := strings.Split(rest, "/")
	if !found || len(parts) != 5 || len("refs/heads/")+len(branch) > besideRefMax || !besidePart(parts[0]) || !besideRolePart(parts[4]) {
		return "", 0, 0, "", false
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || strconv.FormatUint(generation, 10) != parts[1] || !besideTaskPart(parts[2], besideTaskRoom(parts[0], parts[1])) {
		return "", 0, 0, "", false
	}
	number, found := strings.CutPrefix(parts[3], "fix-")
	fix, err = strconv.Atoi(number)
	if !found || err != nil || fix < 1 || strconv.Itoa(fix) != number || len(number) > besideFixDigits {
		return "", 0, 0, "", false
	}
	return parts[0], generation, fix, Role(parts[4]), true
}

// besidePart is whether part is one BesideBranch takes as it stands: not empty, and every byte
// in [A-Za-z0-9-]. An issue's key and a role are.
func besidePart(part string) bool {
	if part == "" {
		return false
	}
	for i := range len(part) {
		if !besideByte(part[i]) {
			return false
		}
	}
	return true
}

// besideByte is whether besideTask writes c as it is: [A-Za-z0-9-].
func besideByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// besideRole is the role's part of a BesideBranch: the role where it is at most besideRoleMax
// bytes, else its first besideRoleKept bytes, `_` and the first besideRoleHash lowercase hex digits
// of sha256 of the role. A role holds no `_`, so a cut role is never a role's own name.
func besideRole(role Role) string {
	if len(role) <= besideRoleMax {
		return string(role)
	}
	sum := sha256.Sum256([]byte(role))
	return string(role[:besideRoleKept]) + "_" + hex.EncodeToString(sum[:])[:besideRoleHash]
}

// besideRolePart is whether part is one besideRole can write.
func besideRolePart(part string) bool {
	if len(part) <= besideRoleMax && besidePart(part) {
		return true
	}
	return len(part) == besideRoleMax && besidePart(part[:besideRoleKept]) && part[besideRoleKept] == '_' &&
		lowerHex(part[besideRoleKept+1:])
}

// besideTask is a task's id as one component of a git ref of at most room bytes: its escaping,
// each byte in [A-Za-z0-9-] as it is and every other byte, `_` itself included, as `_` and its two
// uppercase hex digits, and the empty id as `_` alone. An escaping longer than room is cut to the
// longest run of whole bytes' escapings within room - 33 bytes and followed by `_` and the first
// besideTaskHash lowercase hex digits of sha256 of the id's bytes. So the part holds no byte
// outside [A-Za-z0-9_-], which git takes in a component at any place, and two ids are written
// alike only where their cut escapings and their hashes' first 128 bits are.
func besideTask(task string, room int) string {
	if task == "" {
		return "_"
	}
	const digits = "0123456789ABCDEF"
	kept := room - besideTaskHash - 1
	written := make([]byte, 0, len(task))
	cut := 0
	for i := range len(task) {
		if len(written) <= kept {
			cut = len(written)
		}
		if c := task[i]; besideByte(c) {
			written = append(written, c)
		} else {
			written = append(written, '_', digits[c>>4], digits[c&0x0f])
		}
	}
	if len(written) <= room {
		return string(written)
	}
	sum := sha256.Sum256([]byte(task))
	return string(written[:cut]) + "_" + hex.EncodeToString(sum[:])[:besideTaskHash]
}

// besideTaskPart is whether part is one besideTask can write in room bytes: `_` alone, an escaping
// of at most room bytes, or an escaping of at most room - 33 bytes followed by `_` and
// besideTaskHash lowercase hex digits.
func besideTaskPart(part string, room int) bool {
	head := len(part) - besideTaskHash - 1
	switch {
	case part == "_":
		return true
	case part == "" || len(part) > room:
		return false
	case besideEscaping(part):
		return true
	default:
		return head >= 0 && head <= room-besideTaskHash-1 && part[head] == '_' && lowerHex(part[head+1:]) &&
			besideEscaping(part[:head])
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

// upperHex is whether c is a hex digit as besideTask writes one in an escape: [0-9A-F].
func upperHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }

// lowerHex is whether s is hex digits as a cut part's hash is written: [0-9a-f], not empty.
func lowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
