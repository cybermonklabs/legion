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
// with a warning and exit 0), then the track of the row that fetch left, then the bookmark set at
// that commit, then its push. Each runs in the workspace, since jj reads the repository of the
// directory it runs in. branch is not empty: a caller with no branch names no command.
//
// The track is what lets a step push its branch again from the pod that replaced its own. A fetch
// leaves the row untracked in a clone that did not have the branch, `jj bookmark set` then makes a
// local bookmark of its own beside that row, and jj refuses to push a bookmark over a remote one
// it does not track ("Non-tracking remote bookmark <branch>@origin exists"), so the step's work
// since the round would stay on a disk the next round deletes. A track of a name origin does not
// hold, which is every first push of a branch, warns and exits 0, so the one recipe serves both.
func BesidePushCommand(branch string) string {
	return `cd -- "$LEGION_WORKSPACE" && ` +
		besideJJ + shellprefix.Command([]string{"git", "fetch", "--remote", "origin", "--branch", branch}) + " && " +
		besideJJ + shellprefix.Command([]string{"bookmark", "track", "exact:" + branch, "--remote", "exact:origin"}) + " && " +
		besideJJ + shellprefix.Command([]string{"bookmark", "set", branch, "-r", "@-"}) + " && " +
		besideJJ + shellprefix.Command([]string{"git", "push", "--remote", "origin", "--bookmark", branch})
}

// BesideTakeCommands are the commands with which a fix, in its working copy at dir, takes the
// commits the other steps beside it pushed, and the head of the task's branch taskBranch where it
// moved: a fetch of taskBranch and of every branch that starts with prefix (claim.BesideFixPrefix),
// then the take of takeCommands, in which a commit of the fix's own is one that no branch on origin
// other than the fix's own branch own holds. Those steps' commits come under the fix's own as they
// are, so the task's branch takes them at the fix's end push, and a step's commit is never
// rewritten. A fix whose adoption made its copy at own's head on origin tracks own already
// (fixStart), so its own commits there are its to rebase.
func BesideTakeCommands(dir, prefix, own, taskBranch string) []string {
	taken := `(::remote_bookmarks(glob:"` + prefix + `*", exact:"origin") ~ ::remote_bookmarks(exact:"` + own +
		`", exact:"origin")) ~ ::remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")`
	return []string{
		shellprefix.Command([]string{"jj", "-R", dir, "git", "fetch", "--remote", "origin", "--branch", prefix + "*"}),
		shellprefix.Command([]string{"jj", "-R", dir, "duplicate", taken, "--insert-before", "@"}),
	}
}

// KeptTakeCommands are the commands with which a step that a way runs again after a fix ended
// takes, in its workspace, the commits of its kept branch beside the fix, branch, that the task's
// branch taskBranch does not hold (`::<branch>@origin ~ ::<taskBranch>@origin`): a fetch of both,
// then the take of takeCommands, in which a commit of the step's own is one that no branch on origin
// holds. The fix's end push took some of the kept commits, as they are, and those are already
// ancestors of the task's branch; the rest come under the step's working copy as they are.
func KeptTakeCommands(branch, taskBranch string) []string {
	taken := `::remote_bookmarks(exact:"` + branch + `", exact:"origin") ~ ::remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")`
	return []string{
		besideJJ + shellprefix.Command([]string{"git", "fetch", "--remote", "origin", "--branch", taskBranch, "--branch", branch}),
		besideJJ + shellprefix.Command([]string{"duplicate", taken, "--insert-before", "@"}),
	}
}

// takenMerge is the description of the merge commit a take makes where the working copy has no
// commit of its own to carry the merge of two or more heads it takes.
const takenMerge = "merge: the work of the steps beside the fix"

// takeCommands is a take by ancestry, each command rendered by jj from its arguments: a fetch of
// each branch of fetched; then a rebase of the working copy's own commits, with the working copy
// (`jj rebase -b @`), onto the heads of the commits it stands on and the heads of taken and of
// taskBranch, among the commits others hold, a revset of the commits of branches that are not the
// working copy's own. A commit others hold is never a root of that rebase, since each is an
// ancestor of where the rebase goes, so no step's commit is rewritten and a merge forward keeps its
// parents. Where the working copy has no commit of its own and two heads are taken, the rebase
// leaves the working copy a merge with no description; a duplicate of it under the working copy
// and a description of that duplicate make the merge one commit under the working copy, so `@-`,
// the commit a push and a completion read, is one commit. A take run again finds each commit it
// took an ancestor of the working copy already: jj answers that the commits are already in place
// and that there are no revisions to duplicate or describe, exits 0 and changes nothing. So each
// commit is taken once however often the text that names the take is sent, and a take with
// nothing to take changes nothing.
func takeCommands(jj func(args ...string) string, fetched []string, taken, taskBranch, others string) []string {
	fetch := []string{"git", "fetch", "--remote", "origin"}
	for _, branch := range fetched {
		fetch = append(fetch, "--branch", branch)
	}
	onto := `heads((::@ | ` + taken + ` | remote_bookmarks(exact:"` + taskBranch + `", exact:"origin")) & ::(` + others + `))`
	const merge = `merges() & description(exact:"")`
	return []string{
		jj(fetch...),
		jj("rebase", "-b", "@", "--onto", onto),
		jj("duplicate", "@ & "+merge, "--insert-before", "@"),
		jj("describe", "-r", "@- & "+merge, "-m", takenMerge),
	}
}
