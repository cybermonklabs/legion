package workspace

import "github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"

// The commands of a step beside a fix: while a fix of a task runs, every other step of the task
// pushes its commits to a branch of its own (claim.BesideBranch), never to the task's branch, and
// the fix takes them from there before its one push to the task's branch. Each is rendered as the
// daemon renders a command it prints (shellprefix.Command), as one shell command line, which the
// text that names it quotes.

// besideJJ is the start of every command a step beside a fix runs in its own workspace: the jj
// workspace that the runtime names in LEGION_WORKSPACE, which the agent's shell expands.
const besideJJ = `jj -R "$LEGION_WORKSPACE" `

// BesidePushCommand is the command that pushes the commit under the working copy of the role's
// workspace to branch, a branch beside a fix: a fetch of branch first, so the clone's record of it
// is current (a new pod's clone holds none, and jj answers a fetch of a branch origin does not hold
// with a warning and exit 0), then the bookmark set at that commit, then its push, which tracks a
// new bookmark at its first push. Each runs in the workspace, since jj reads the repository of the
// directory it runs in. branch is not empty: a caller with no branch names no command.
func BesidePushCommand(branch string) string {
	return `cd -- "$LEGION_WORKSPACE" && ` +
		besideJJ + shellprefix.Command([]string{"git", "fetch", "--remote", "origin", "--branch", branch}) + " && " +
		besideJJ + shellprefix.Command([]string{"bookmark", "set", branch, "-r", "@-"}) + " && " +
		besideJJ + shellprefix.Command([]string{"git", "push", "--remote", "origin", "--bookmark", branch})
}

// BesideTakeCommands are the commands with which a fix, in its working copy at dir, takes the
// commits the other steps beside it pushed: a fetch of every branch that starts with prefix
// (claim.BesideFixPrefix), then a copy under the fix's working copy (`jj duplicate --insert-before
// @`) of each commit of those branches that neither the fix's own branch own nor the task's branch
// taskBranch holds. The copy rebases and rewrites nothing, so no other working copy goes stale. A
// fetch that finds no branch and a copy of no commit each answer exit 0 and change nothing.
func BesideTakeCommands(dir, prefix, own, taskBranch string) []string {
	taken := `(::remote_bookmarks(glob:"` + prefix + `*", exact:"origin") ~ ::remote_bookmarks(exact:"` + own +
		`", exact:"origin")) ~ ::remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")`
	return []string{
		shellprefix.Command([]string{"jj", "-R", dir, "git", "fetch", "--remote", "origin", "--branch", prefix + "*"}),
		shellprefix.Command([]string{"jj", "-R", dir, "duplicate", taken, "--insert-before", "@"}),
	}
}
