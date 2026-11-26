package workspace

import (
	"fmt"
	"strings"
)

// The workspace read: whether the workspace every role of an issue shares holds work that no
// branch or tag on origin holds. A role's shim makes it in the workspace it works in, and the
// issue pod's launcher makes it where no role process runs (R6 of #5285). A pod
// update round that replaces the pod without waiting for every role reads it to name what goes
// with the pod, and the idle release, the release of a pod no claim owns and the close of a left
// pod read it to take the pod only when it holds no work. Both readers run ReadWork, so they count
// the same work.

// WorkRevset is the work the read counts: every commit under a workspace's working copy or a
// local bookmark, less three kinds of commit that are not work.
//
// A commit origin holds is not work: one under a remote bookmark or a tag is already pushed, and
// the replacement pod's fetch gets it back. Tags are subtracted beside remote bookmarks because a
// clone of this repository holds thousands of them, jj makes each tagged commit a visible head,
// and the release commits they point at are reachable from no remote bookmark: with remote
// bookmarks alone subtracted, every such commit under a working copy or a bookmark would count as
// work that origin already holds.
//
// A commit with no change and no description is not work: it carries nothing a replacement can
// lose, whether it is a working copy or the parent a bare `jj new` leaves. Every issue pod's clone
// keeps such a working copy, and jj 0.45.1 keeps the old empty working copy as the new one's
// parent after a bare `jj new` (measured). A described empty commit is work: a role wrote its
// description.
//
// A commit a fetch left visible, and a side head that no working copy and no bookmark reaches,
// are not counted, because Legion's clones keep unreachable commits
// (git.abandon-unreachable-commits = false, ensureFetchConfiguration): after any role's
// `jj git fetch`, the commits of every branch origin deleted or rewrote stay visible with no
// bookmark, hundreds of them other issues' work. Counting every visible head would name those as
// this issue's. The cost is a side head of this issue's own (a role ran `jj new` over a rewritten
// parent, bookmark.go), which goes uncounted: a round does not name it, and a release does not
// keep the pod for it.
//
// This read is the one the pod update round and every release make, so the work a round names as
// going with a pod is the work that keeps a release from taking it.
const WorkRevset = `(::(working_copies() | bookmarks()) ~ ::(remote_bookmarks() | tags())) ~ (empty() & description(exact:""))`

// ReadWork runs the read's two jj commands in the workspace dir with jj, through run, which runs
// one command and answers its trimmed standard output (RunCommand under the caller's budget and
// environment, which for the pod's launcher holds no configuration of anyone's). global are jj
// options the caller puts before each command's own. The first command
// reads the working copy's commit and whether it is empty, and it is the one jj snapshot the read
// makes; the second is --ignore-working-copy, so both describe one state of the workspace. It
// answers the working copy's commit, whether that commit holds a change, and every work commit
// (WorkRevset), newest first. The snapshot is what the read writes, as every jj command that reads
// the working copy does: jj records an operation and, where files of the working copy changed, a
// new working-copy commit and the git ref jj keeps for it (refs/jj/keep). The read changes no file
// of the working copy, moves no bookmark and pushes nothing, and it needs no git.
func ReadWork(run func(args ...string) (string, error), jj, dir string, global ...string) (head string, changed bool, work []string, err error) {
	command := func(args ...string) []string {
		return append(append([]string{jj}, global...), args...)
	}
	at, err := run(command("log", "-R", dir, "-r", "@", "--no-graph", "-T", `commit_id ++ " " ++ empty`)...)
	if err != nil {
		return "", false, nil, err
	}
	head, empty, _ := strings.Cut(at, " ")
	if head == "" || (empty != "true" && empty != "false") {
		return "", false, nil, fmt.Errorf("jj answered %q for the working copy, not its commit and whether it is empty", at)
	}
	listed, err := run(command("log", "-R", dir, "--ignore-working-copy", "-r", WorkRevset, "--no-graph", "-T", `commit_id ++ "\n"`)...)
	if err != nil {
		return "", false, nil, err
	}
	return head, empty == "false", OutputLines(listed), nil
}
