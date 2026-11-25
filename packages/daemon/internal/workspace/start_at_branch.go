package workspace

import (
	"fmt"
	"strings"
)

// JJ runs one jj command in the workspace a role works in and answers its trimmed standard output.
// args are the command's arguments without the jj itself, and each names the workspace with -R. A
// command that fails is an error naming it (RunCommand, which the shim's commands run through).
type JJ func(args ...string) (string, error)

// StartedAt is what StartAtBranch did with a workspace's working copy: whether it started a new
// working copy on the bookmark (Moved), the working copy's commit before the call (From), the
// bookmark's commit the working copy now stands on (At), and whether origin's base holds a commit
// the bookmark does not (BehindBase), which the role then merges forward before it works.
type StartedAt struct {
	Moved      bool
	From       string
	At         string
	BehindBase bool
}

// startedAtTemplate prints the working copy's commit and its parents' commits, "<commit>|<parent>,…".
const startedAtTemplate = `commit_id ++ "|" ++ parents.map(|c| c.commit_id()).join(",")`

// StartAtBranch starts the working copy of workspace, the jj workspace a role of an issue works in,
// at the commit of bookmark, the issue's branch, once the branch moved on origin for the issue's
// next change. The daemon moved the branch to the head of base, the branch the issue's last pull
// request merged into. Its four steps:
//
//  1. It fetches bookmark and base from origin, with the environment jj runs in, which is the role's
//     own, so the fetch uses the role's git credential as the role's own fetch does. A tracked local
//     bookmark that did not move since its last push moves to origin's commit.
//  2. It reads the bookmark's rows as readBookmark does. A local bookmark in conflict moved locally
//     and was never pushed, while origin's moved too: it is refused with its sides and its ways out,
//     and nothing moves. A bookmark with no local row is refused too.
//  3. Where the working copy's one parent is already the bookmark's commit, it changes nothing. That
//     is a fresh workspace, which provisioning starts at the bookmark, and a second call.
//  4. Else it runs `jj new` on that commit. The old working copy stays as it was, holding what it
//     held. jj itself abandons an old working copy that holds no change and no description, which
//     holds nothing to lose.
//
// BehindBase is read from origin's base after the fetch, before anything moves. A base GitHub no
// longer has answers false: there is nothing of it to merge.
func StartAtBranch(jj JJ, workspace, bookmark, base string) (StartedAt, error) {
	if _, err := jj("git", "fetch", "--remote", "origin", "--branch", exactName(bookmark), "--branch", exactName(base), "-R", workspace); err != nil {
		return StartedAt{}, err
	}
	listed, err := jj("bookmark", "list", "--all-remotes", exactName(bookmark), "-T", bookmarkRowTemplate, "--ignore-working-copy", "--color=never", "-R", workspace)
	if err != nil {
		return StartedAt{}, err
	}
	var rows bookmarkRows
	seen := map[string]bool{}
	for _, line := range nonEmptyLines(listed) {
		where, row, ok := parseBookmarkRow(line)
		if !ok || seen[where] {
			return StartedAt{}, fmt.Errorf("Bookmark %s's row %q is not the shape jj bookmark list prints. The working copy of %s did not move.", bookmark, line, workspace)
		}
		seen[where] = true
		switch where {
		case "local":
			rows.local = row
		case "origin":
			rows.origin = row
		}
	}
	switch local, origin := rows.local, rows.origin; {
	case local.conflict:
		return StartedAt{}, conflictAtBranch(workspace, bookmark, local)
	case !local.present && origin.present && !origin.tracked:
		return StartedAt{}, fmt.Errorf("Bookmark %s is not tracked in %s. The working copy did not move. Track origin's branch with %s.",
			bookmark, workspace, wayOut(workspace, "bookmark", "track", originRow(bookmark)))
	case !local.present:
		return StartedAt{}, fmt.Errorf("Bookmark %s is not in %s after its fetch. The working copy did not move.", bookmark, workspace)
	}
	at := rows.local.added[0]

	before, err := jj("log", "-r", "@", "--no-graph", "-T", startedAtTemplate, "--color=never", "-R", workspace)
	if err != nil {
		return StartedAt{}, err
	}
	from, parents, ok := strings.Cut(before, "|")
	if !ok || !commitID.MatchString(from) {
		return StartedAt{}, fmt.Errorf("jj answered %q for the working copy of %s, not its commit and its parents. The working copy did not move.", before, workspace)
	}
	ahead, err := jj("log", "-r", "remote_bookmarks("+exactName(base)+", "+exactName("origin")+") ~ ::"+at,
		"--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy", "--color=never", "-R", workspace)
	if err != nil {
		return StartedAt{}, err
	}
	started := StartedAt{From: from, At: at, BehindBase: len(nonEmptyLines(ahead)) > 0}
	if parents == at {
		return started, nil
	}
	if _, err := jj("new", at, "--color=never", "-R", workspace); err != nil {
		return StartedAt{}, err
	}
	started.Moved = true
	return started, nil
}

// conflictAtBranch refuses a bookmark in conflict after the fetch: it moved locally and was never
// pushed, and origin's moved too. It names the sides and two ways out, to start at origin's branch
// or to keep a local commit, as conflictRefusal does for a workspace provisioning would create.
func conflictAtBranch(workspace, bookmark string, local bookmarkRow) error {
	return fmt.Errorf("Bookmark %s is conflicted. It adds %s and removes %s. The working copy of %s did not move. "+
		"To start at origin's branch, run %s. To keep a local commit instead, run %s with that commit.",
		bookmark, listed(local.added), listed(local.removed), workspace,
		wayOut(workspace, "bookmark", "set", jjString(bookmark), "-r", originRow(bookmark), "--allow-backwards"),
		wayOut(workspace, "bookmark", "set", jjString(bookmark), "-r", "<commit>"))
}
