package survival

import "strings"

// Group is one of a project's code groups (dispatch://INFRA-3206 R10): a name, the head-branch
// prefixes and the author logins of its changes. A group with neither takes every change that no
// group before it took. The project names its groups in its delivery settings, in order; none is
// written here.
type Group struct {
	Name           string   `json:"name"`
	BranchPrefixes []string `json:"branch_prefixes"`
	AuthorLogins   []string `json:"author_logins"`
}

// ChangeFacts is what a commit's pull request says of it: its number (0 for a commit that merged
// no pull request, which is no change), its head branch, the login that opened it (GitHub's REST
// user.login, so an App reads `<name>[bot]`), and its issue. The issue is an input: the one the
// caller's attribution gave the pull request ("" where it gave none), never read here from a
// branch or a message.
type ChangeFacts struct {
	PR       int
	HeadRef  string
	Author   string
	IssueKey string
}

// Kind is the kind of a removal of a change's lines by a later change (R5).
type Kind string

// The four kinds of removal: by a change of the adder's group for the same issue (iteration), by
// one of that group for a different issue (rework), by one of that group where either has no
// issue (no issue), and by a change of another group or of none, or by a commit that merged no
// pull request (other). Only rework counts in the post-merge rework number.
const (
	KindRework    Kind = "rework"
	KindIteration Kind = "iteration"
	KindNoIssue   Kind = "noissue"
	KindOther     Kind = "other"
)

// GroupOf is the group a change belongs to: the first of groups with a prefix that starts its
// head branch or a login equal to its author, case aside; a group with no prefix and no login
// takes every change no earlier group took. A commit that merged no pull request is no change and
// has no group, and a change no group takes has none.
func GroupOf(change ChangeFacts, groups []Group) (name string, ok bool) {
	if change.PR == 0 {
		return "", false
	}
	for _, group := range groups {
		if len(group.BranchPrefixes) == 0 && len(group.AuthorLogins) == 0 {
			return group.Name, true
		}
		for _, prefix := range group.BranchPrefixes {
			if strings.HasPrefix(change.HeadRef, prefix) {
				return group.Name, true
			}
		}
		for _, login := range group.AuthorLogins {
			if strings.EqualFold(login, change.Author) {
				return group.Name, true
			}
		}
	}
	return "", false
}

// KindOf is the kind of the removal of the lines of adder, a change of group, by remover, as
// scripts/15_rework.py's join and replaylib.py's classify read a pair: other when the remover is
// in no group or in another than group; then no issue when either has none, iteration when both
// have the same, and rework when they differ.
func KindOf(group string, adder, remover ChangeFacts, groups []Group) Kind {
	if name, ok := GroupOf(remover, groups); !ok || name != group {
		return KindOther
	}
	switch {
	case adder.IssueKey == "" || remover.IssueKey == "":
		return KindNoIssue
	case adder.IssueKey == remover.IssueKey:
		return KindIteration
	}
	return KindRework
}
