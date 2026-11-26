package shim

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The project release's read: whether the workspace every role of the issue shares holds the
// issue's work that no remote branch or tag holds, which the release asks before it deletes the
// pod with its disk. Two facts cross the wire, a count and a flag; the read runs no git, sends no
// commit and writes no file. What it does write is jj's own record of the working copy's snapshot.

// unpushedCandidates is every commit that can be unpushed work: each visible commit that no remote
// branch and no tag holds, less the root commit and each commit with no change and no description.
// A commit a remote bookmark or a tag reaches is pushed already; tags are subtracted beside remote
// bookmarks for the reason workspaceWorkRevset subtracts them. A commit with no change and no
// description carries nothing a deletion can lose: each working copy left so, and the parent a bare
// `jj new` leaves (workspaceWorkRevset). Every such working copy is subtracted, not only this
// workspace's own: a pod's shared clone has a workspace of its own, `default`, whose working copy
// is an empty commit on the default branch that no remote holds (workspace's feedClone abandons the
// clone's first snapshot), so a read that subtracted only `@` would count one commit in every pod.
// A working copy with a change, whichever workspace's, stays in.
const unpushedCandidates = `~::(remote_bookmarks() | tags()) ~ root() ~ (empty() & description(exact:""))`

// unpushedRevset is the commits of the issue's unpushed work as read asks for them, the term
// "Unpushed work" of dispatch://INFRA-3583's spec: of unpushedCandidates, each commit under a working
// copy or a local bookmark, which is the set of the pod update round's read
// (workspaceWorkRevset); each commit with a change and no description; and, while the issue's pull
// request has not merged (read.Merged), each commit whose description has a line
// `Omp-Session: <id>` naming one of read.Sessions, the session names the issue's claims hold, or a
// line `Dispatch: <read.Issue>`. Any other such commit is another's, and is not counted.
//
// No other visible head is counted, because a pod's clone keeps every head visible
// (git.abandon-unreachable-commits = false, workspace's ensureFetchConfiguration): after any role's
// `jj git fetch`, the commits of each branch origin deleted or rewrote stay visible with no
// bookmark. The spec cites INFRA-3582's pod, which held 19 such heads, each with another issue's
// trailer or none: a read of every head would name them as this issue's work, and the release would
// ask the pod's roles about work that is not theirs.
//
// The trailers tell the issue's own side heads from those. A role's commits carry
// `Omp-Session: <id>` (cmd/legion's gitTrailers, and jj's commit_trailers that
// packages/pi-shared/src/jj-attribution.ts writes), and those jj makes in the workspace of an issue
// on a repository the deployment names also carry `Dispatch: <KEY>`. They count only until the pull
// request merges: the merge deletes the issue's branch, and the issue's own commits stay visible
// with their trailers, merged work a read after the merge would count again.
//
// A commit with a change and no description counts wherever it is: jj pushes no commit with no
// description unless the push says --allow-empty-description, so such a commit is the workspace's
// own, such as the change a role's `jj new <base>` left as a side head.
//
// The limits are the spec's. A described commit with no trailer that no working copy and no
// bookmark reaches is not counted. These are counted although they are not the issue's unpushed
// work: a commit someone pushed with no description, through --allow-empty-description or plain
// git, that no remote branch holds now; a commit of the issue that a person's rewrite of
// legion/<KEY> replaced before the merge; and, when the issue opens a second pull request after a
// merge, the first pull request's commits. Each makes the release ask the pod's roles once more,
// and none makes it lose work.
//
// It differs from the update round's set on purpose, and that set stays as it is: the round
// replaces the pod and names what goes with it, so it counts only what a working copy or a bookmark
// reaches and lets a side head of the issue's own go unnamed. The release deletes the pod with its
// disk, replaces it with nothing and asks the roles before the disk goes, so it also counts each
// side head it can tell is the issue's own.
func unpushedRevset(read shimwire.ReadUnpushed) string {
	issueWork := []string{`::(working_copies() | bookmarks())`, `(~empty() & description(exact:""))`}
	if !read.Merged {
		for _, session := range read.Sessions {
			issueWork = append(issueWork, trailerRevset("Omp-Session", session))
		}
		issueWork = append(issueWork, trailerRevset("Dispatch", read.Issue))
	}
	return "(" + unpushedCandidates + ") & (" + strings.Join(issueWork, " | ") + ")"
}

// trailerRevset is the commits whose description has the line `<key>: <value>`. The value is
// matched as written, each regular-expression metacharacter of it quoted, so the `.` a session id
// can hold matches a `.` alone; the pattern then goes into a jj string literal, where each `\` of
// it is written `\\`. Measured with jj 0.45.1, `description(regex:"(?m)^Omp-Session: ab\\.c-1$")`
// matches a commit whose description has the line `Omp-Session: ab.c-1` and not one with
// `Omp-Session: abXc-1`, and `(?m)` lets `^` and `$` match at each line of the description, its
// first included.
func trailerRevset(key, value string) string {
	return `description(regex:` + jjStringLiteral(`(?m)^`+key+`: `+regexp.QuoteMeta(value)+`$`) + `)`
}

// jjStringEscapes escapes the two characters a double-quoted jj string literal cannot hold as
// they are. A name and an issue key the frame lets through hold neither
// (shimwire.ReadUnpushed.Validate), but the quoted `.` of a session id is a `\`.
var jjStringEscapes = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// jjStringLiteral is s as a double-quoted jj string literal.
func jjStringLiteral(s string) string {
	return `"` + jjStringEscapes.Replace(s) + `"`
}

// workingCopyChangedRevset is the workspace's own working copy when it holds a change, and nothing
// when it holds none.
const workingCopyChangedRevset = `@ & ~empty()`

// Unpushed is the issue's unpushed work in a workspace as ReadUnpushed counts it, and nothing of
// the work itself: Commits is how many commits of it no remote branch or tag holds
// (unpushedRevset), and WorkingCopyChanged is whether the workspace's own working copy holds a
// change.
type Unpushed struct {
	Commits            int
	WorkingCopyChanged bool
}

// ReadUnpushed reads the issue's unpushed work in the jj workspace at dir, as request asks for it
// (unpushedRevset), with the jj of PATH, the one the pane's or pod's own shell runs, under env and
// within the request's budget, which bounds ctx (openWorkspaceAt). A request the frame's rules
// refuse is refused here too (shimwire.ReadUnpushed.Validate), since its issue and its session
// names go into the revset. Its first jj runs without --ignore-working-copy, so jj snapshots the
// working copy before it reads whether the working copy holds a change: a change not yet
// snapshotted is unpushed work too. The count after it is --ignore-working-copy, so both facts
// describe one state of the workspace. Both commands run in dir itself (workspace.RunCommand),
// each started by start: the shim's guard (s.children.Starter) for the shim's own read, so the
// shim's reaper leaves their exits to them. It is exported for a test's stand-in agent, which
// answers the daemon's read-unpushed on a workspace of its own as this shim does, outside any
// shim, with procgroup.NoReaper.
func ReadUnpushed(ctx context.Context, start procgroup.Starter, dir string, env []string, request shimwire.ReadUnpushed) (Unpushed, error) {
	if err := request.Validate(); err != nil {
		return Unpushed{}, err
	}
	opened, done, err := openWorkspaceAt(ctx, start, dir, "read", env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return Unpushed{}, err
	}
	defer done()
	jj, run := opened.jj, opened.run
	changed, err := run(nil, jj, "log", "-R", dir, "-r", workingCopyChangedRevset, "--no-graph", "-T", "commit_id")
	if err != nil {
		return Unpushed{}, err
	}
	commits, err := run(nil, jj, "log", "-R", dir, "--ignore-working-copy", "-r", unpushedRevset(request), "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return Unpushed{}, err
	}
	return Unpushed{Commits: len(workspace.OutputLines(commits)), WorkingCopyChanged: changed != ""}, nil
}

// readUnpushed answers `read-unpushed` on the workspace the shim's environment names in
// LEGION_WORKSPACE, which the runtime sets on every role. A request the shim cannot act on is
// answered with the reason when it names an id to answer, and a shim with no workspace refuses by
// name rather than answering that nothing is unpushed. The budget bounds the shim's command
// context (s.commands), so the shim's own end ends the read's jj at once. The answer goes out as
// one line, so a refusal carries at most maxAnswerErrorBytes of its reason (cutAnswerError): jj's
// standard error has no bound of its own.
func (s *shim) readUnpushed(request shimwire.ReadUnpushed) {
	result := shimwire.ReadUnpushedResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing a read-unpushed frame: %v", err)
			return
		}
		result.OK, result.Error = false, cutAnswerError(err.Error())
	} else if dir := envValue(s.cfg.Env, "LEGION_WORKSPACE"); dir == "" {
		result.OK, result.Error = false, "worker-shim: LEGION_WORKSPACE is not set; no workspace to read for unpushed work"
	} else if read, err := ReadUnpushed(s.commands, s.children.Starter(), dir, s.cfg.Env, request); err != nil {
		result.OK, result.Error = false, cutAnswerError(err.Error())
	} else {
		result.Commits, result.WorkingCopyChanged = read.Commits, read.WorkingCopyChanged
	}
	s.toDaemon(result)
}
