// Package rescue finds the work an issue pod's workspaces hold that their remote lacks, and pushes
// it to rescue branches on that remote, before the daemon replaces the pod. The pod's disk is the
// pod's own and goes with it (docs/kubernetes.md, "The disk dies with its pod"), so this is the
// last place that work can be read from.
//
// It runs inside the pod, in a role's shim (`rescue-unpushed`, shim.rescue), as that role and with
// that role's own GitHub credential: the shared clone's credential helper answers git from the
// role's gh files, so the daemon handles no secret of its own for it. It writes no file of a
// workspace's own tree: no file a role wrote is changed, moved or deleted by it. What it writes
// is git objects, the branches it pushes to the remote with the remote-tracking ref git writes
// in the clone for each of them (refs/remotes/origin/<branch>), and, in a worktree jj holds,
// jj's own snapshot of that worktree, which is the read that finds the files no commit holds
// (edits).
//
// What that snapshot writes was measured in an issue pod on 2026-11-12 and 2026-11-13, on jj
// 0.45.1-sami.20260910 and git 2.47.3. A snapshot of a quiescent colocated worktree whose files
// changed since jj's last snapshot writes four things: one jj operation, a new working-copy
// commit, the colocated git index, and one refs/jj/keep/<commit> ref in the shared clone; one
// whose files did not prints `No snapshot needed.` and writes nothing. Where the worktree's git
// HEAD had moved behind jj's back, the snapshot prints `Reset the working copy parent to the new
// Git HEAD.` and writes three such refs: the new working-copy commit, the git HEAD it imported,
// and the empty commit it makes on that HEAD. Where another git ref moved behind jj's back while
// HEAD did not, a branch moved or made with plain git or a remote-tracking ref, this check's own
// earlier rescue push among them, it prints `Done importing changes from the underlying Git
// repo.` and writes one more jj operation, `import git refs`, before its own. Where a bookmark
// points at the working-copy commit, the snapshot moves that bookmark and exports it to
// refs/heads/<name> in the shared clone. No configuration stops that export, and the move is the
// move any jj command of any role makes in that same workspace. The worktree's git HEAD is
// unchanged in each case, and so is every other refs/heads/<name>. The shared clone, which holds
// those refs, is on the pod's own disk, and that disk goes with the pod when the pod is replaced.
// When this check refuses the replacement, which is the case it exists for, the pod and its disk
// stay, and so do these writes: the overwritten colocated index, an exported bookmark and the
// refs/jj/keep refs, each a write the role's own next jj command in that workspace would make too.
//
// The snapshot writes the colocated git index, so a role's own staged git index in that
// workspace can be overwritten. What the tree this check pushes holds is not changed by that:
// the check copies that index before the snapshot, and the carry into jj's tree reads the copy
// (snapshotTree, carried). The copy is the very input the `git add --all` read starts from, in
// each of indexTree's three cases: the worktree's index file where HEAD names a commit, HEAD's
// tree where that file is missing, and the empty index where HEAD is unborn. The carry takes that
// read's input and its rule: the copy and the worktree, read through git's own commands, over a
// base of jj's tree, which is read twice, once for its paths and gitlinks (`ls-tree`) and once
// into the scratch index (`read-tree`). From the copy come its records jj's tree lacks and its
// flagged records (the tracked set, seeds), and a record whose path is now a directory takes
// git's own update rule there (`add -u`) and git's answer for what that directory would be
// without its record; from git's own `ls-files --others --exclude-standard` over the copy, the
// one read of the whole worktree this route makes, which walks directories and hashes no file,
// come the untracked set and the embedded repositories; and each gitlink of jj's tree is written
// again from what git sees at its path (unlinked), but for one the copy flags or stages at
// another commit, which is kept as the copy records it. git's content conversion, eol rules,
// core.autocrlf, a clean filter and core.fileMode, has no read of its own: it applies to every
// file the carry adds, and jj's tree does not pass through it. For every path, that read records
// it, drops it or fails, and the route answers the same way: where that read fails, the route
// fails the workspace and never drops the path, but for one named exception, a tracked file made
// unreadable or failing a required clean filter while it is unchanged, where that read fails and
// the route reads clean, losing nothing, since the file holds HEAD's content. Where the route
// keeps less than that read, the case is carried when carrying is bounded by the paths involved,
// and named when carrying would cost the whole-worktree hash this route removes, or when that
// read gets it wrong too. Named: the content loss of a same-size rewrite jj's stat rule passes
// over (its mtime set back on purpose, or both writes inside one millisecond) of a file jj's
// last snapshot recorded as unchanged, one whose record in the index copy equals its entry in
// jj's tree; the same rewrite of a file the role had already edited is carried; and a change of
// form, where git's content conversion stores a file jj holds otherwise than jj does. The route
// keeps one shape that read loses: an ignored file jj tracks that the index the read starts from
// lacks (carried says each). A differential test compares the two trees in both directions over
// every shape found so far and over a generated matrix of index and worktree states, each
// difference declared by the row or cell that needs it under the word for its kind
// (differential_test.go, matrix_test.go).
package rescue

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The statuses a workspace's check ends in, the runtime's own words for them.
const (
	Clean   = runtime.RescueClean
	Rescued = runtime.RescueRescued
	Failed  = runtime.RescueFailed
)

// ownHeads is the jj revset of the work on a workspace's own line, reduced to its heads: the
// ancestors of its working-copy commit and of the bookmarks its issue owns (ownedBookmarks), less
// every empty commit with no description. It is the first of the two sources of a workspace's own
// work; the second is what was made in the pod: every visible head and every local git branch
// that no commit the clone held when it was made contains (visibleWorkHeads, madeInThePod). That
// is what holds a role's side line and the merge it left when its working copy moved back. Whether
// origin holds a head is git's question, asked of what the remote advertises now (unpushed), so
// nothing here subtracts what jj last saw remote.
//
// Neither source is the whole clone. A pod's shared clone keeps every commit it ever fetched
// visible (workspace-init sets git.abandon-unreachable-commits false), so jj's visible heads hold
// old merges, other trees' handoffs and the pre-rebase copies of rewritten branches, which origin
// deleted or rewrote and no role of the pod wrote: rescuing them pushed one branch for each, and
// the pod's own work was one of sixteen.
//
// An empty commit with no description — jj's working-copy commit before anything is written —
// holds no work, and dropping it makes the commit under it a head. An empty commit with a
// description is kept: an empty merge of two pushed commits is the one record of that merge, as it
// is when a worker merges, leaves the issue's bookmark behind and its push says "Nothing changed".
// refs/jj/keep is not read: it holds the obsolete predecessor of every rewritten commit.
func ownHeads(bookmarks []string) string {
	own := "@"
	for _, b := range bookmarks {
		own += ` | bookmarks(exact:` + jjString(b) + `)`
	}
	return `heads(::(` + own + `) ~ (empty() & description(exact:"")))`
}

// ownedBookmarks is the bookmarks the issue a workspace's directory names owns: its branch,
// `legion/<KEY>`, read back from the directory's name (workspace.Location lays it out as the issue
// key in lowercase). A directory whose name is no issue key owns none.
func ownedBookmarks(dir string) []string {
	issue := strings.ToUpper(filepath.Base(dir))
	if !claim.IsIssueKey(issue) {
		return nil
	}
	return []string{claim.IssueBranch(issue)}
}

// jjString is name as a jj string literal: quoted, with `\` and `"` escaped, so a bookmark name is
// the one name it is in a revset.
func jjString(name string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
}

// visibleWorkHeads is every head jj can see that holds something: with the clone's local git
// branches, what madeInThePod reduces to the work the pod made.
const visibleWorkHeads = `heads((::visible_heads()) ~ (empty() & description(exact:"")))`

// madeInThePod is the commits of made that the clone at clone did not hold when it was made: none
// of the commits provisioning recorded then (workspace.RecordHeld, read back with workspace.Held)
// contains them. That record is the one thing that tells a commit the pod made from one it came
// with, whatever the commit says of itself: its committer time is what git or jj was told
// (`git rebase` and `git am` with --committer-date-is-author-date keep the author's,
// GIT_COMMITTER_DATE and JJ_TIMESTAMP set any), a reflog expires and jj's export writes the same
// entry for a bookmark it fetched as for one a role moved, and the clone's operation log can be
// abandoned. Provisioning writes the record once, in the one place it creates the clone, so a
// re-creation of a workspace in the live pod changes nothing of it.
//
// A commit someone else made, which the pod fetched while it ran and origin deleted since, is
// among what this keeps, and is rescued: noise, and never a loss. A clone with no record, made
// by an earlier release, or one whose record cannot be read, keeps every one of made, as the check
// did before it read the record: noise too, and never a loss.
func (r runner) madeInThePod(clone string, made []string) ([]string, error) {
	recorded, ok, err := workspace.Held(clone)
	if err != nil || !ok {
		return made, nil
	}
	held, err := r.objects(clone, recorded, "commit")
	if err != nil {
		return nil, err
	}
	return r.unpushed(clone, made, held)
}

// gitPins is configuration every git this package starts reads last: no file-system monitor runs
// and no replacement object (`git replace`, a graft among them) stands in for a commit, so a
// commit grafted to have a role's work as its parent never makes that work look held, and the
// push sends the commits as they are.
//
// `--literal-pathspecs` is for the paths this package hands git: every one of them is a name a
// role wrote, read out of git's own NUL-separated answers, and never a pattern. Without it git
// reads each as a pathspec, where a name beginning with `:` is magic for what follows it and a
// name holding a glob character stands for every path it matches: measured on this build on
// 2026-11-12, over a worktree holding a refused 2 MiB file named `:aa` beside a tracked `aa`,
// `git add -f -- ':aa'` exited 0 with the index holding `aa` alone, and the same add with this
// option put `:aa` in it.
//
// `core.hooksPath=/dev/null` turns hooks off for every command here but one: the push, in a pod.
// There the git this package runs is the image's wrapper, which for a push alone puts
// `-c core.hooksPath=/opt/legion/githooks` after the caller's own global options and before the
// subcommand, and git takes the last `-c` of a name, so the wrapper's path outranks this one
// (packages/daemon/docker/git, which measured it on the image's git 2.47.3). That push therefore
// runs the root-owned pre-push hook, which runs the leak guard a second time, over the same
// updates guardPush has already cleared. It is not turned off and cannot be: no process in a pod
// may name a hooks path that outranks the wrapper's, which is what makes the hook a guard. What
// the second run costs is time, and the budget holds both runs (GuardReserve).
var gitPins = []string{"--no-replace-objects", "--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// Request is one check of every workspace on a pod's disk.
type Request struct {
	// StateDir is the disk's root: workspaces under <StateDir>/workspaces/<owner>/<repo>/<issue>,
	// and the shared clone each is a jj workspace of under <StateDir>/repos/github.com/<owner>/<repo>.
	StateDir string
	// Branch is the prefix of every rescue branch: the push of the n-th one is to <Branch>-<n>,
	// with n the least number from 1 that names no branch the remote has and none this check has
	// pushed already.
	Branch string
	// WorkspaceTimeout bounds one workspace's check and push.
	WorkspaceTimeout time.Duration
	// Deadline bounds the whole check: a workspace not reached by then is an error, not a pass.
	Deadline time.Time
	// Env is the environment git and jj run in: the role's own, which names its credential.
	Env []string
}

// StateDirOf is the disk's root a workspace directory is on: four directories up, as
// workspace.Location lays a workspace out (<state>/workspaces/<owner>/<repo>/<issue>).
func StateDirOf(workspace string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Clean(workspace)))))
}

// Report is a check's entries as the runtime boundary carries them (runtime.WorkspaceRescue).
func Report(workspaces []shimwire.RescueWorkspace) []runtime.WorkspaceRescue {
	report := make([]runtime.WorkspaceRescue, len(workspaces))
	for i, w := range workspaces {
		report[i] = runtime.WorkspaceRescue{Dir: w.Dir, Status: w.Status, Error: w.Error}
		for _, b := range w.Branches {
			report[i].Branches = append(report[i].Branches, runtime.RescuedBranch{Name: b.Name, Commit: b.Commit})
		}
	}
	return report
}

// Run checks every workspace on the disk, in path order, and pushes what each holds that its remote
// lacks. Every workspace gets one entry. A disk with no workspace yields none, which is no work to
// lose.
func Run(ctx context.Context, req Request) []shimwire.RescueWorkspace {
	dirs, err := workspaces(req.StateDir)
	if err != nil {
		return []shimwire.RescueWorkspace{{Dir: req.StateDir, Status: Failed, Error: err.Error()}}
	}
	used := map[string]bool{}
	results := make([]shimwire.RescueWorkspace, 0, len(dirs))
	// seen is every worktree a check has taken already: each workspace is its own check's, and a
	// further worktree of a clone (git worktree list) is taken by the first check of that clone.
	seen := map[string]bool{}
	for _, dir := range dirs {
		seen[dir] = true
	}
	for _, dir := range dirs {
		if !req.Deadline.IsZero() && !time.Now().Before(req.Deadline) {
			results = append(results, shimwire.RescueWorkspace{Dir: dir, Status: Failed,
				Error: "not checked: the check's whole time ran out before it reached this workspace"})
			continue
		}
		bound := req.WorkspaceTimeout
		if !req.Deadline.IsZero() {
			bound = min(bound, time.Until(req.Deadline))
		}
		one, cancel := context.WithTimeout(ctx, bound)
		result := check(one, req, dir, used, seen)
		if one.Err() != nil && result.Status == Failed {
			result.Error = fmt.Sprintf("%s; the check ran out its %s", result.Error, bound.Round(time.Second))
		}
		cancel()
		results = append(results, result)
	}
	return results
}

// workspaces is every workspace directory on the disk: a real directory three levels below
// workspaces/, never a symlink and never a file an agent left beside its workspace.
func workspaces(stateDir string) ([]string, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("the disk's root %q is not an absolute path", stateDir)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "workspaces", "*", "*", "*"))
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, match := range matches {
		if info, err := os.Lstat(match); err == nil && info.IsDir() {
			dirs = append(dirs, match)
		}
	}
	slices.Sort(dirs)
	return dirs, nil
}

// step is one step of a workspace's check, with the time that step started. Every failure of
// check is reported through one, so the entry names the step that failed and how long that step
// ran. Without the step a reader tells this check's own fault from the fault of the push it
// makes (dispatch://INFRA-2718) only by reading the command text.
type step struct {
	what    string
	started time.Time
}

// began is the step the check has reached, started now.
func began(what string) step { return step{what: what, started: time.Now()} }

// failed is err as a workspace's entry reports it: the step, how long that step ran, rounded to
// the millisecond, and the error itself.
func (s step) failed(err error) error {
	return fmt.Errorf("%s failed after %s: %w", s.what, time.Since(s.started).Round(time.Millisecond), err)
}

// folded is two of a workspace's failures as the one text its entry carries, the earlier first.
// Either may be nil. An entry is one field of a log line and one line of a notice, and
// errors.Join would write a newline between the two, so they are joined with "; " here instead:
// the step the first of them names is then still what a reader reads first (rescuetest.Step).
func folded(earlier, later error) error {
	switch {
	case earlier == nil:
		return later
	case later == nil:
		return earlier
	}
	return fmt.Errorf("%w; %w", earlier, later)
}

// check is one workspace's check and push. The workspace's repository is the one its git names
// (its common git directory), so a directory under workspaces/ that is no git worktree is an error
// rather than a pass: whatever it holds, nothing can push it.
//
// What the remote holds is read from the remote alone: a remote-tracking ref is only as fresh as
// the pod's last fetch, so a commit of the workspace's own that only a stale one names — the
// issue's branch since force-pushed or deleted on the remote — is a commit the remote may no
// longer have, and is rescued.
//
// The candidates are the work that is the workspace's own, and never a commit the clone held when
// it was made. They come from two sources. The workspace's own line: the ancestors of its
// working-copy commit and of its issue's bookmark (ownHeads, ownedBookmarks), and in git the
// issue's branch, every local tag, every entry of the stash, and the HEAD of the workspace and of
// every other worktree of the repository (git worktree list), each a worktree on the pod's disk;
// the clone's own working tree is left out, which is no agent's workspace and, in a pod, no tree
// at all. And what was made in the pod: every visible jj head and every local git branch that no
// commit the clone held when it was made contains (madeInThePod), a side line a role left, a
// merge it left behind when its working copy moved back, and a commit git made in the workspace's
// detached HEAD that jj then moved away from among them. A candidate that is jj's empty
// working-copy commit stands for the commit under it (holdingWork). Each worktree's files that no
// commit holds become one commit on that worktree's HEAD.
//
// The report also names each rescue branch of this prefix the remote holds already: an earlier
// check of the same pod pushed it, in a round whose replacement did not happen then, and the issue
// must still be told of it once the pod goes.
func check(ctx context.Context, req Request, dir string, used, seen map[string]bool) shimwire.RescueWorkspace {
	result := shimwire.RescueWorkspace{Dir: dir}
	// at is the step the check is in, and unread is what the walk of the worktrees below could
	// not read. Every failure of this check is reported through fail, which writes both: the
	// step that failed and, after it, what an earlier worktree left unread. Writing them here
	// is what keeps the two together, since a call site that named only its own failure would
	// drop the other half of the entry and no reader would learn of it.
	at := began("the read of the workspace's own repository")
	var unread error
	fail := func(err error) shimwire.RescueWorkspace {
		result.Status, result.Branches, result.Error = Failed, nil, folded(at.failed(err), unread).Error()
		return result
	}
	run := runner{ctx: ctx, env: req.Env}
	common, err := run.git(dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fail(fmt.Errorf("%s is not a git worktree, so nothing in it can be pushed: %w", dir, err))
	}
	clone := common
	if filepath.Base(common) == ".git" {
		clone = filepath.Dir(common)
	}

	at = began("the read of what the remote holds")
	listed, err := run.git(clone, nil, "ls-remote", "origin")
	if err != nil {
		return fail(err)
	}
	remoteBranches := map[string]bool{}
	var advertised []string
	for line := range strings.Lines(listed) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		advertised = append(advertised, sha)
		if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			remoteBranches[branch] = true
			if strings.HasPrefix(branch, req.Branch+"-") {
				result.Branches = append(result.Branches, shimwire.RescueBranch{Name: branch, Commit: sha})
			}
		}
	}
	held, err := run.objects(clone, advertised, "commit", "tag")
	if err != nil {
		return fail(err)
	}

	at = began("the reads that find the workspace's unpushed commits")
	owned := ownedBookmarks(dir)
	branches, err := run.git(clone, nil, "for-each-ref", "--format=%(objectname)", "refs/heads")
	if err != nil {
		return fail(err)
	}
	made := fields(branches)
	refs, err := run.git(clone, nil, "for-each-ref",
		"--format=%(if)%(*objectname)%(then)%(*objectname)%(else)%(objectname)%(end)", "refs/tags")
	if err != nil {
		return fail(err)
	}
	candidates := fields(refs)
	for _, b := range owned {
		if commit, err := run.git(clone, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+b+"^{commit}"); err == nil {
			candidates = append(candidates, commit)
		}
	}
	if _, err := run.git(clone, nil, "rev-parse", "--verify", "--quiet", "refs/stash"); err == nil {
		stash, err := run.git(clone, nil, "log", "--walk-reflogs", "--format=%H", "refs/stash")
		if err != nil {
			return fail(err)
		}
		candidates = append(candidates, fields(stash)...)
	}
	worktrees, err := run.worktrees(clone)
	if err != nil {
		return fail(err)
	}
	// The workspace itself, then every worktree of the repository no check has taken yet, with the
	// clone's own working tree left out of the walk. That tree is no agent's workspace: every role
	// of the issue works in a workspace of its own. A pod's shared clone holds no working tree
	// at all — workspace.feedClone clones it with `git clone --no-checkout` and then clears its
	// sparse patterns and abandons jj's first snapshot — while its git HEAD still names the
	// default branch's commit and its index still names every file of it, so a walk that took it
	// would have edits build the deletion of the whole tree (git's empty tree against that HEAD)
	// and push it as a rescue branch. What the exclusion costs is the clone's own git HEAD as a
	// candidate, which takes nothing an agent wrote: every local branch of the clone is a
	// candidate already (for-each-ref refs/heads above), and everything jj can see is one through
	// visibleWorkHeads. The clone's path is not marked seen, since seen records the worktrees a
	// check took and no check takes this one: every check of this disk reads the same common git
	// directory and leaves it out here, whichever workspace it is of.
	cloneTree := filepath.Clean(clone)
	mine := []string{dir}
	for _, w := range worktrees {
		if w == cloneTree || seen[w] {
			continue
		}
		seen[w] = true
		mine = append(mine, w)
	}
	heads := map[string]string{}
	for _, w := range mine {
		head, err := run.git(w, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
		if err != nil {
			continue // an unborn HEAD: a worktree with no git-visible commit yet
		}
		heads[w] = head
		candidates = append(candidates, head)
	}
	if heldByJJ(dir) {
		jjHeadsOut, err := run.jj(dir, "log", "--no-graph", "-r", ownHeads(owned), "-T", `commit_id ++ "\n"`)
		if err != nil {
			return fail(err)
		}
		candidates = append(candidates, fields(jjHeadsOut)...)
		visible, err := run.jj(dir, "log", "--no-graph", "-r", visibleWorkHeads, "-T", `commit_id ++ "\n"`)
		if err != nil {
			return fail(err)
		}
		made = append(made, fields(visible)...)
	}
	made, err = run.objects(clone, made, "commit")
	if err != nil {
		return fail(err)
	}
	made, err = run.holdingWork(clone, made)
	if err != nil {
		return fail(err)
	}
	made, err = run.madeInThePod(clone, made)
	if err != nil {
		return fail(err)
	}
	candidates = append(candidates, made...)
	candidates, err = run.objects(clone, candidates, "commit")
	if err != nil {
		return fail(err)
	}
	candidates, err = run.holdingWork(clone, candidates)
	if err != nil {
		return fail(err)
	}

	tips, err := run.unpushed(clone, candidates, held)
	if err != nil {
		return fail(err)
	}
	// The read of each worktree's files is the one step of this check whose cost follows the size
	// of the worktree rather than its history, and a workspace's bound covers it (worktreeRead,
	// which holds what that read costs and why). It is cut here with the guard's own run and the
	// reserve it leaves the push still in hand (pushReserve), so a read that still cannot finish
	// loses the files it was reading and not the commits this check has already found: those are
	// pushed, and the entry fails naming the step that did not finish, which leaves the pod where
	// it is for a person to read.
	//
	// The reduction of the tips below is reported as this step as well. It reads the commits the
	// files of this step were added to, it runs before the push, and it is not the push.
	at = began("the read of the files a worktree holds that no commit does")
	// cut is a read the workspace's bound stopped, and it is the one failure that ends the walk:
	// every worktree after it has no more of that bound than this one had. Every other failure —
	// a stale working copy, a narrowed workspace, a path the snapshot left out that git could not
	// carry — is kept in unread and the walk goes on to the next worktree, whose files the bound
	// still has the time to read. The entry fails either way, and unread may hold more than one
	// failure. Every failure that ends the check carries what unread holds with it (fail), so an
	// earlier worktree's is still named in the entry.
	cut := false
	readRun := run
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - pushReserve
		if left <= 0 {
			unread = at.failed(fmt.Errorf("the files %s holds that no commit does could not be read: the workspace's bound had no time left for them", dir))
			cut = true
		} else {
			bounded, cancel := context.WithTimeout(ctx, left)
			defer cancel()
			readRun.ctx = bounded
		}
	}
	for _, w := range mine {
		if cut {
			break
		}
		edits, err := readRun.edits(w, heads[w])
		if err != nil {
			why := "could not be read"
			if readRun.ctx.Err() != nil {
				why, cut = "could not be read in the time the workspace's bound left for them", true
			}
			unread = folded(unread, at.failed(
				fmt.Errorf("the files %s holds that no commit does %s, so they are not rescued: %w", w, why, err)))
			continue
		}
		if edits == "" {
			continue
		}
		tree, err := run.git(clone, nil, "rev-parse", edits+"^{tree}")
		if err != nil {
			return fail(err)
		}
		same := false
		for _, tip := range tips {
			if t, err := run.git(clone, nil, "rev-parse", tip+"^{tree}"); err == nil && t == tree {
				same = true
				break
			}
		}
		if !same {
			tips = append(tips, edits)
		}
	}
	if len(tips) > 1 {
		independent, err := run.git(clone, nil, append([]string{"merge-base", "--independent"}, tips...)...)
		if err != nil {
			return fail(err)
		}
		tips = fields(independent)
		slices.Sort(tips)
	}
	if len(tips) > 0 {
		refspecs := make([]string, 0, len(tips))
		var pushed []shimwire.RescueBranch
		for _, tip := range tips {
			name := ""
			for n := 1; ; n++ {
				name = req.Branch + "-" + strconv.Itoa(n)
				if !remoteBranches[name] && !used[name] {
					break
				}
			}
			used[name] = true
			refspecs = append(refspecs, tip+":refs/heads/"+name)
			pushed = append(pushed, shimwire.RescueBranch{Name: name, Commit: tip})
		}
		at = began("the leak guard's check of the push")
		if err := guardPush(ctx, req, run, clone, pushed); err != nil {
			return fail(err)
		}
		at = began("the push of the rescue branches to the remote")
		if _, err := run.git(clone, []string{"GIT_TERMINAL_PROMPT=0"}, append([]string{"push", "--atomic", "--quiet", "origin"}, refspecs...)...); err != nil {
			return fail(fmt.Errorf("push %s: %w", strings.Join(refspecs, " "), err))
		}
		result.Branches = append(result.Branches, pushed...)
	}
	if unread != nil {
		// The branches this check pushed stay on the entry: that work is on the remote and the
		// issue must be told of it, whatever the step that did not finish leaves unread. The
		// reader is told so here, where the push has happened, and never by the read's own
		// error, which is written before it.
		result.Status, result.Error = Failed, unread.Error()
		if len(result.Branches) > 0 {
			result.Error = unread.Error() + "; the work this check did find is on the remote, in the branches of this entry"
		}
		return result
	}
	result.Status = Clean
	if len(result.Branches) > 0 {
		result.Status = Rescued
	}
	return result
}

// noSHA is git's "this ref is not there", which a pre-push line carries as the remote side of a
// ref the push creates. Every branch this check pushes is created by it: the name it picks names
// no branch the remote advertises and none an earlier workspace of the same check took.
const noSHA = "0000000000000000000000000000000000000000"

// CheckRun is one run of the shared public leak check's push mode, measured in a pod: sixty
// seconds asking the remote what it holds and another sixty in its credential path.
const CheckRun = 120 * time.Second

// pushAlone is the push itself, which is the whole of what GuardReserve was while the pre-push
// hook was believed off for it.
const pushAlone = 30 * time.Second

// GuardReserve is what the guard's own call leaves of a workspace's remaining time for the push
// it clears, and it holds two things rather than one. The push itself is thirty seconds, which
// is all this reserve was while hooks were believed off for it. In a pod that push also runs the
// shared public leak check a second time, in the pre-push hook the image's git wrapper points it
// at (gitPins), and one run of that check's push mode is CheckRun. A smaller reserve has the
// workspace's bound end inside the hook's run, and work this check could have rescued is then
// lost to a bound rather than refused by anyone — which is the one failure this reserve exists
// to prevent, and which 30 s did not prevent in a pod. It is exported with the two bounds below
// because they are derived from it: a reserve that grew and bounds that did not would be the
// same lost work again.
const GuardReserve = CheckRun + pushAlone

// readsBeforeThePush is the jj and git reads that find a workspace's unpushed work, which the
// four minutes a workspace's bound once was left 90 s for. It does not hold the read of the
// worktree's own files, which is worktreeRead below.
const readsBeforeThePush = 90 * time.Second

// worktreeRead is the read of one worktree's files that finds what no commit holds (edits): the
// one step of this check whose cost follows the size of the worktree rather than its history.
// git hashes again every file whose stat data its index does not match, and jj writes a
// colocated index with no stat data at all — measured on jj 0.45.1-sami.20260910, every entry
// read carries ctime, mtime, dev, ino, uid, gid and size 0 — so that read over a worktree jj holds
// hashes every tracked file, whether or not one of them changed.
//
// What that costs, measured in issue pods on 2026-11-11, over jj workspaces read that way: on a
// 13 GB workspace of 291,957 tracked files, on two CPUs, git read every entry as changed (78 s
// to list them) and took 200 s to answer that no file had changed. `git update-index -q
// --refresh`, which hashes every tracked file the same way, took 9 minutes 13 seconds on one
// other pod's workspace and up to 25 minutes across the thirteen pods. Refreshing the index
// first is therefore that same read and buys nothing: it pays the hash once and jj writes the
// stat data away again at its next snapshot.
//
// The add itself was measured at 27 s on OSSV-202 and 36.8 s as a dry run on OSSV-349, each on
// a pod carrying that night's by-hand cure, which put .jj/ and .codegraph/ in the clone's
// exclude file and refreshed the index, so each of those two read less than a pod's own read
// does. On the architect's fixture the add alone cost 1.8 to 3.9 times the refresh alone of the
// same tree, which carried to the slowest of the thirteen suggests an add of 45 to 98 minutes.
//
// So 30 minutes is about nine times the slowest read measured directly on a pod, above every
// refresh measured (25 minutes at the most), and under what that ratio suggests for the slowest:
// this term is not sized for the worst case, and a workspace whose read is slower than it fails
// as it does today and waits for a person. What it is sized against is the bound those thirteen
// pods met on 2026-11-11, which was two minutes: the read ran out, the check failed the
// workspace, no pod was replaced and every one of them waited on a person — while in all
// thirteen the files the read was after turned out to hold nothing the remote lacked.
//
// The term stays in a workspace's bound for two reads, and the measurements above are of the
// first of them. The daemon sends this bound to every pod, and a pod whose shim predates this
// build still makes the whole-worktree git read over a jj workspace, which is the read those
// pods measured. This build makes that git read for a worktree with no .jj directory (edits,
// indexTree), whose index git itself wrote with stat data it can use. A worktree jj holds takes
// jj's own snapshot here in its place, and one `git ls-files --others --exclude-standard` over
// the worktree, which walks its directories and hashes no file (snapshotTree). Measured on
// 2026-11-13 in this change's round 12 (the c5 run at 6ab55389), on a fixture of over 290,000
// tracked files and 3.6 GB whose index carries no stat data, the snapshot took 22.4 to 26.3 s and
// that read 1.7 to 1.8 s, and every read before the push 29.3 s on the clean tree and 30.2 s with
// 200 new files of 1 MB, where `git add --all` over a copy of the index took 118.4 s and 246.9 s.
// The first snapshot after a colocated `jj git init` took 62.5 s on that fixture on 2026-11-12,
// because it also imports the git repository (it prints `Done importing changes from the
// underlying Git repo.`); the live check imports wherever a git ref moved behind jj's back, HEAD,
// a branch or a remote-tracking ref (the package doc), and in the same round-12 run a snapshot
// after a branch moved with HEAD unchanged took 24.2 s, and after a new remote-tracking ref
// 23.8 s.
const worktreeRead = 30 * time.Minute

// pushReserve is what a workspace's bound keeps back from the read of its files: the guard's own
// run of the shared check (CheckRun) and what that guard leaves the push (GuardReserve). A read
// that runs past it would take the commits this check has already found down with it, which is
// the loss check stops the read to prevent.
const pushReserve = CheckRun + GuardReserve

// wholeDiskMargin is what the bound of a whole disk's check keeps over one workspace's: the three
// minutes it has always kept. Nothing measured it, and the whole disk's bound rests on one
// workspace per pod: a second workspace that follows a slow first one runs out of time and is
// refused, not lost, since the replacement then waits for a person.
const wholeDiskMargin = 3 * time.Minute

// WorkspaceTimeout and Timeout are this check's two bounds: one workspace's check and push, and
// the whole check of a pod's disk. The daemon sets them on every request it makes
// (internal/daemon's rescueWorkspaceTimeout and rescueTimeout are these), and a fixture that
// drives this package as a pod's shim does bounds its own run by them (rescuetest.Disk.Check,
// internal/shim's tests): a fixture bounded under GuardReserve would have every push refused
// for want of time rather than checked.
//
// A workspace's bound holds four things in a pod: the reads that find the unpushed work
// (readsBeforeThePush), the read of the worktree's own files (worktreeRead), the guard's own run
// of the shared check (CheckRun), and what that guard leaves the push, which in a pod runs the
// same check a second time in the pre-push hook (GuardReserve). worktreeRead and CheckRun come
// from measurements; GuardReserve is CheckRun and pushAlone, the 30 s the push had before the
// hook was known to run the check again, which is a reserve kept as it was and no measurement;
// and readsBeforeThePush is the share an earlier bound left those reads, which were measured on
// 2026-11-13 at 29.3 s on a fixture of the production shape. 90 + 1800 + 120 + 150 is 2160 s, so
// a workspace's bound is 36 minutes and the whole disk's, keeping wholeDiskMargin over it, 39.
// Each is a ceiling and not a wait: a workspace whose files git can answer for at once has them
// read in seconds, and its push then waits on the leak check, one run of which was measured in a
// pod at 120 s (CheckRun), run twice there. Most of this package's tests take seconds; measured
// on 2026-11-13, the read cut by its bound and the snapshot refused jj's lock took about 11 s
// each, the differential test about two minutes, and the matrix test 190 to 202 s.
//
// A bound that fits one run of the check and not two ends inside the hook's run and loses the
// dying pod's work to a timeout rather than refusing it; a bound that does not fit the worktree
// read never replaces the pod at all. Both are the failure these numbers exist to prevent.
const (
	WorkspaceTimeout = readsBeforeThePush + worktreeRead + CheckRun + GuardReserve
	Timeout          = WorkspaceTimeout + wholeDiskMargin
)

// guardPush runs the leak guard over the push this check is about to make, and is the error that
// fails the workspace where the guard refuses that push or could not be made to run: either way
// nothing is pushed, and the runtime hands the failure back having written nothing. The guard
// runs the shared public leak check in its pre-push shape, in the clone, and then this
// repository's own private-name rules over each ref name and over every commit the push would
// add (leakguard.Push).
//
// The destination repository is read from the remote's URL, which this package does not hold:
// it asks the clone for it with the package's own runner, under the same bound and the same
// environment as everything else here, and a remote whose URL it cannot read fails the
// workspace rather than being pushed unchecked.
//
// The updates are the refspecs the push is about to send, as git would hand them to a pre-push
// hook: the local sha, the branch it creates on the remote, and the all-zero sha for what the
// remote holds there, since every one of these branches is new.
//
// Env is the role's own environment, the one the push itself runs in, which is what names the
// credential the check reads the remote with.
func guardPush(ctx context.Context, req Request, run runner, clone string, pushed []shimwire.RescueBranch) error {
	what := leakguard.PushName("origin")
	url, err := run.git(clone, nil, "remote", "get-url", "origin")
	if err == nil && url == "" {
		err = errors.New("git named no URL for origin")
	}
	if err != nil {
		return &leakguard.SetupFailure{What: what, Why: "the URL of the remote this push is made to could not be read", Err: err}
	}
	updates := make([]leakguard.Update, 0, len(pushed))
	for _, branch := range pushed {
		updates = append(updates, leakguard.Update{
			LocalRef: branch.Commit, LocalSHA: branch.Commit,
			RemoteRef: "refs/heads/" + branch.Name, RemoteSHA: noSHA,
		})
	}
	bound := ctx
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - GuardReserve
		if left <= 0 {
			return &leakguard.SetupFailure{What: what, Why: "the workspace's bound has no time left to check this push in"}
		}
		checking, cancel := context.WithTimeout(ctx, left)
		defer cancel()
		bound = checking
	}
	return leakguard.New(leakguard.Options{Env: req.Env}).Push(bound, leakguard.PushRequest{
		Dir: clone, Remote: "origin", URL: url, Updates: updates,
	})
}

// worktrees is every worktree of the repository whose directory is there, as `git worktree list`
// names it, the clone's own working tree included; a bare repository's entry has none.
func (r runner) worktrees(clone string) ([]string, error) {
	out, err := r.git(clone, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var dirs []string
	current, bare := "", false
	flush := func() {
		if current != "" && !bare {
			if info, err := os.Lstat(current); err == nil && info.IsDir() {
				dirs = append(dirs, filepath.Clean(current))
			}
		}
		current, bare = "", false
	}
	for line := range strings.Lines(out + "\n") {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = strings.TrimPrefix(line, "worktree ")
		case line == "bare":
			bare = true
		case line == "":
			flush()
		}
	}
	flush()
	return dirs, nil
}

// runner runs git and jj under one workspace's bound, in the role's environment.
type runner struct {
	ctx context.Context
	env []string
}

func (r runner) git(dir string, env []string, args ...string) (string, error) {
	return r.command(dir, env, nil, "git", append(slices.Clone(gitPins), args...)...)
}

// gitPaths is the paths one NUL-separated git read answers, each as it stands. A path may hold
// any byte but NUL — a space at either end among them — so the read is split on NUL alone and
// nothing of it is trimmed (carried and seeds, which read the worktree's own names and the
// index's records this way). Every other read of this package comes through git above, which
// trims what it answers: a sha, a ref name, and in worktrees the directories of the
// repository's worktrees, which are paths too, read a line at a time out of `git worktree list
// --porcelain` rather than this way.
func (r runner) gitPaths(dir string, env []string, args ...string) ([]string, error) {
	out, err := r.output(dir, env, nil, "git", append(slices.Clone(gitPins), args...)...)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(out, "\x00")
	if last := len(paths) - 1; last >= 0 && paths[last] == "" {
		paths = paths[:last] // git writes a NUL after the last path, never between paths alone
	}
	return paths, nil
}

// jj runs a read of jj's view that writes nothing: no snapshot of the working copy, and no new
// operation, since it reads at the operation the store is at.
func (r runner) jj(dir string, args ...string) (string, error) {
	return r.command(dir, nil, nil, "jj", append([]string{"--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never", "-R", dir}, args...)...)
}

// snapshot takes jj's own snapshot of the worktree at dir: jj stats the files it tracks against
// the per-file state it keeps and hashes only what changed, which costs what any jj command of
// a role costs, where the git read it replaces hashes every tracked file of a pod's workspace
// (worktreeRead). Only its exit status is read: jj refuses a file above the workspace's size
// limit and skips a path whose name is not valid UTF-8, each with a warning on its standard
// error and an exit status of 0, and what it left out that way is found by the carry from git's
// own read of the worktree, not from those warnings (snapshotTree, carried).
//
// It carries neither --ignore-working-copy nor --at-op, since either one suppresses the
// snapshot; that is why it cannot go through runner.jj.
//
// Every pin is on the command line, which outranks the repository's, the workspace's and the
// user's configuration, so none of theirs changes what the snapshot does, and that was measured
// on this build on 2026-11-12: with a repository configuring the other value of each of the
// last two below, the snapshot recorded what the command line pinned. Eight hold this build's
// own default. No file-system monitor is started (fsmonitor.backend), no filter of git's runs
// over a file (git.filter.enabled), nothing is signed and no signature is remade
// (signing.backend, signing.behavior), and every file of the tree is tracked
// (snapshot.auto-track, quoted because this build reads an unquoted all() as a bare string).
// snapshot.auto-update-stale is a default as well, and is pinned because a configuration that
// turns it on has jj write the worktree's own files, which this check never does. So are the
// two that say what a file's own bytes and mode are recorded as: working-copy.eol-conversion,
// which under any other value would record other bytes than the worktree holds — a file of
// CRLF lines recorded with LF — and working-copy.exec-bit-change, which under `ignore` would
// record the mode the commit under it carries rather than the mode the file has. The ninth is
// no default: the git jj starts.
//
// The size limit is not among them. jj refuses a new file above the workspace's own
// snapshot.max-new-file-size, and a pin above that limit would have jj track the file in the
// role's own working-copy commit, where it stays tracked long after this check is over. So the
// snapshot runs under the workspace's own limit, and what it refused is carried into the tree
// this check pushes by a git that writes nothing of the role's (snapshotTree, carried).
func (r runner) snapshot(dir string) error {
	git, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("the git this check runs is not on its PATH, so jj could not snapshot %s: %w", dir, err)
	}
	args := []string{"--no-pager", "--color=never", "-R", dir}
	for _, pin := range []string{
		`fsmonitor.backend="none"`,
		"git.filter.enabled=false",
		`signing.backend="none"`,
		`signing.behavior="keep"`,
		`snapshot.auto-track="all()"`,
		"snapshot.auto-update-stale=false",
		`working-copy.eol-conversion="none"`,
		`working-copy.exec-bit-change="auto"`,
		"git.executable-path=" + jjString(git),
	} {
		args = append(args, "--config", pin)
	}
	_, err = r.command(dir, nil, nil, "jj", append(args, "util", "snapshot")...)
	return err
}

// observeCommand is nil outside this package's tests, where export_test.go sets it: the one
// place a test can read every git and jj command this check runs, with its arguments, since
// no test may put a stand-in binary on PATH.
var observeCommand func(dir, name string, args []string)

func (r runner) command(dir string, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	out, err := r.output(dir, env, stdin, name, args...)
	return strings.TrimSpace(out), err
}

// output runs one command and answers its standard output as it stands, where it exits 0: a
// name git writes may hold a space at either end, so nothing is trimmed here and each caller
// trims what it reads, or does not (command, gitPaths). It is the one implementation the
// package runs every git and jj through. One caller comes here directly: gitPaths, whose read's
// standard output must reach its caller untrimmed. Every other caller comes through command,
// which trims the output. A command that could not be started or that exits non-zero is an
// error naming the command, the status and its standard error, so nothing of what it said is
// lost on any path. Each argument of the command is written with %q (quoted): an argument may
// be a path a role wrote, and a space inside one would otherwise read as the gap between two
// arguments.
func (r runner) output(dir string, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	if observeCommand != nil {
		observeCommand(dir, name, args)
	}
	cmd := exec.CommandContext(r.ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(r.env), env...)
	cmd.Stdin = stdin
	procgroup.Configure(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := procgroup.Err(cmd.Run()); err != nil {
		return "", fmt.Errorf("%s %s: %v", name, quoted(args), err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return "", fmt.Errorf("%s %s: exit %d: %s", name, quoted(args), code, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// objects is the shas the clone has an object of one of kinds for, in the order given: what the
// remote advertises is held only where the clone has it, since a local commit can be an ancestor of
// nothing else, and a candidate is a commit or nothing rev-list may be handed.
func (r runner) objects(clone string, shas []string, kinds ...string) ([]string, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	out, err := r.command(clone, nil, strings.NewReader(strings.Join(shas, "\n")+"\n"), "git",
		append(slices.Clone(gitPins), "cat-file", "--batch-check=%(objectname) %(objecttype)")...)
	if err != nil {
		return nil, err
	}
	var kept []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		sha, kind, _ := strings.Cut(scanner.Text(), " ")
		if slices.Contains(kinds, kind) {
			kept = append(kept, sha)
		}
	}
	return kept, nil
}

// unpushed is the candidates no held commit contains, each once, in the order given.
func (r runner) unpushed(clone string, candidates, held []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, c := range candidates {
		input.WriteString(c + "\n")
	}
	for _, h := range held {
		input.WriteString("^" + h + "\n")
	}
	out, err := r.command(clone, nil, strings.NewReader(input.String()), "git", append(slices.Clone(gitPins), "rev-list", "--stdin")...)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, sha := range fields(out) {
		listed[sha] = true
	}
	var tips []string
	for _, c := range candidates {
		if listed[c] && !slices.Contains(tips, c) {
			tips = append(tips, c)
		}
	}
	return tips, nil
}

// holdingWork is candidates, each once, with every commit that has no message and the tree of its
// one parent replaced by that parent, as often as it takes. That commit is jj's working-copy commit
// before anything is written, as ownHeads drops it on jj's side, and a git ref can name it: jj
// exports a bookmark set on it, as provisioning sets the issue's on the working copy of a workspace
// it starts at the default branch. A merge is kept whatever it holds.
func (r runner) holdingWork(clone string, candidates []string) ([]string, error) {
	for {
		if len(candidates) == 0 {
			return nil, nil
		}
		listed, err := r.git(clone, nil, append([]string{"log", "--no-walk=unsorted", "--format=%H %T %P%x09%s"}, candidates...)...)
		if err != nil {
			return nil, err
		}
		replaced := map[string]string{}
		for line := range strings.Lines(listed) {
			ids, subject, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
			commit := strings.Fields(ids)
			if subject != "" || len(commit) != 3 {
				continue
			}
			parentTree, err := r.git(clone, nil, "rev-parse", commit[2]+"^{tree}")
			if err != nil {
				return nil, err
			}
			if parentTree == commit[1] {
				replaced[commit[0]] = commit[2]
			}
		}
		var next []string
		for _, c := range candidates {
			if p, ok := replaced[c]; ok {
				c = p
			}
			if !slices.Contains(next, c) {
				next = append(next, c)
			}
		}
		if len(replaced) == 0 {
			return next, nil
		}
		candidates = next
	}
}

// heldByJJ is whether jj holds the worktree at dir, which is a .jj directory of its own: a
// colocated jj workspace has one, a plain git worktree a role added beside it has none. It is
// what decides which of the two reads of a worktree's files the check makes (edits), and the
// shape of the candidates it takes from jj at all (check).
func heldByJJ(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".jj"))
	return err == nil && info.IsDir()
}

// edits is one commit holding the workspace's files as they stand, on top of head, when they
// differ from head's tree; "" when they do not. A file .gitignore ignores that the index a read
// starts from does not track is left out by the git read, as a role's own push would leave it;
// the jj route leaves it out too, but for one: a file jj already tracks that the index the read
// starts from lacks (after a `git reset`, with the index file missing, or under an unborn HEAD)
// stays in jj's tree, so the route keeps it where the git read does not, the one gain the
// differential test declares (carried).
//
// Where the worktree holds a .jj directory, the tree is jj's own: the check copies the
// worktree's git index, or reads head's tree where the worktree has no index file, or starts from
// the empty index where HEAD is unborn, as the git read does, takes jj's snapshot of the
// worktree, reads the tree of the working-copy commit that snapshot wrote, and then carries into
// it what the git read would have kept and that tree lacks, taken from the index copy and from
// git's own read of the files that copy does not track (snapshotTree,
// carried). Where it does not, the tree is git's, read into an index of its own from a copy of
// the worktree's where it has one, so that neither the worktree's index nor any ref moves
// (indexTree). The two answer the same question, and the jj one is the one a pod can afford:
// the git read hashes every tracked file there (worktreeRead).
//
// Of the two reads, only the snapshot writes anything of that worktree's own: its colocated git
// index, over a role's own staged index there. What that costs the tree this check pushes is
// nothing the git read would have kept, since the carry reads the copy of that index taken
// before the snapshot: a file .gitignore ignores that a role forced in with `git add -f` is an
// entry of the copy, and is carried. The git read writes no index of the worktree at all, since
// it builds its tree in an index of its own.
//
// A conflicted working copy is no failure. jj writes a conflict into the worktree itself, as
// .jjconflict-base-0/, .jjconflict-side-0/, .jjconflict-side-1/, JJ-CONFLICT-README and the
// one-sided file, so the snapshot carries every side of it and no content is lost; failing there
// would hold a pod replacement back for work the branch already holds.
//
// An untracked nested git repository is read by both as git reads it: where it has a commit,
// `git add --all` records it as a gitlink at its HEAD, whose commit no object of this repository
// holds, and carries none of its files, and the route records the same gitlink and carries none
// of them either, so what is in it goes with the pod, as it would have gone had no check run;
// where it has no commit, `git add --all` refuses it, and so does the route, so the workspace
// fails rather than read clean and lose what that repository holds (carried).
func (r runner) edits(dir, head string) (string, error) {
	var tree string
	var err error
	if heldByJJ(dir) {
		tree, err = r.snapshotTree(dir, head)
	} else {
		tree, err = r.indexTree(dir, head)
	}
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=Legion rescue", "-c", "user.email=rescue@invalid", "commit-tree", tree,
		"-m", "Edits left in the workspace " + dir + " when its pod was replaced"}
	if head != "" {
		was, err := r.git(dir, nil, "rev-parse", head+"^{tree}")
		if err != nil {
			return "", err
		}
		if was == tree {
			return "", nil
		}
		args = append(args, "-p", head)
	} else if tree == emptyTree {
		return "", nil
	}
	return r.command(dir, nil, nil, "git", append(slices.Clone(gitPins), args...)...)
}

// indexTree is the tree of the worktree's files as git reads them, built in an index of its own,
// from a copy of the worktree's index where it has one, so neither that index nor any ref moves.
// This is the read whose cost follows the size of the worktree (worktreeRead), and it is made
// for a worktree jj does not hold, which has no snapshot of its own to take.
func (r runner) indexTree(dir, head string) (string, error) {
	scratch, err := os.MkdirTemp("", "legion-rescue-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	index := filepath.Join(scratch, "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	own, err := r.git(dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	copied := false
	if head != "" {
		if body, err := os.ReadFile(own); err == nil {
			copied = os.WriteFile(index, body, 0o600) == nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if !copied {
		base := []string{"read-tree", "--empty"}
		if head != "" {
			base = []string{"read-tree", head}
		}
		if _, err := r.git(dir, env, base...); err != nil {
			return "", err
		}
	}
	if _, err := r.git(dir, env, "add", "--all", "--", "."); err != nil {
		return "", err
	}
	return r.git(dir, env, "write-tree")
}

// snapshotTree is the tree of the worktree's files as jj holds them once jj has snapshotted
// them, with what the `git add --all` this route replaces would have kept and that tree lacks
// carried into it (carried). The things that can lose a file are read in this order.
//
// The workspace's sparse patterns come first, before anything is written: a workspace narrowed
// to part of its tree would have jj snapshot only that part, while the git read kept every file
// outside it, so such a workspace fails having had nothing snapshotted. A set holding `.` is no
// narrowing, whatever else it holds: `.` is the whole tree, a workspace checked out whole holds
// it alone, and `jj sparse set --add d` over such a workspace leaves the two patterns `.` and
// `d`, which together are still every file of the tree (measured on this build on 2026-11-12).
// A set holding nothing is no narrowing either, but a workspace that holds no file of its tree
// at all — what an unfinished checkout leaves, and the same set workspace.CheckedOut reads it
// by — so it fails in words of its own.
//
// The worktree's own git index is copied next, still before anything is written, into a
// scratch directory this removes again: that copy is the input the git read starts from
// (indexTree), and the snapshot that follows rewrites the index it is copied from. It is made
// in the git read's three cases. Where head, the worktree's git HEAD, names a commit and the
// worktree has an index file, the copy is that file. Where head names a commit and the worktree
// has no index file, the copy is read from head's tree with `read-tree <head>`. Where HEAD is
// unborn, the copy is `read-tree --empty`, whatever the worktree's index holds: the git read
// starts from an empty index there and reads no record of the worktree's own index, so the
// carry reads none either.
//
// The snapshot itself fails the workspace where it fails, and is not made twice: it leaves the
// working copy as the last jj command of a role wrote it, and no retry of ours makes a lock or
// a stale working copy any newer. A stale one is the single case whose cure is a jj command, so
// the failure says what a person runs, since nothing else tells them. A directory a role made
// unreadable is another such failure, and a deliberate one: jj's snapshot fails on it (measured
// on this build on 2026-11-12: `Failed to read directory …: Permission denied`), where
// the `git add --all` this replaces warned `could not open directory`, exited 0 and lost what
// was inside, so the failure is this check telling a person rather than a regression. Nothing
// the snapshot prints is read: what it left out is found by the carry, from the index copy and
// git's own rule.
//
// Then jj's tree, the tree of the working-copy commit the snapshot wrote, and the carry into it.
func (r runner) snapshotTree(dir, head string) (string, error) {
	patterns, err := r.jj(dir, "sparse", "list")
	if err != nil {
		return "", err
	}
	var held []string
	for line := range strings.Lines(patterns) {
		if p := strings.TrimSpace(line); p != "" {
			held = append(held, p)
		}
	}
	switch {
	case len(held) == 0:
		return "", fmt.Errorf("the jj workspace %s holds no sparse pattern, so it holds no file of its tree and jj's snapshot of it would carry nothing the git read kept: that is what an unfinished checkout leaves, and a person clears it by running `jj sparse reset` in %s", dir, dir)
	case !slices.Contains(held, "."):
		return "", fmt.Errorf("the jj workspace %s holds part of its tree and not all of it, so jj's snapshot would leave out every file the git read kept: its sparse patterns are %s", dir, quoted(held))
	}
	scratch, err := os.MkdirTemp("", "legion-rescue-carry-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	tracked := filepath.Join(scratch, "tracked")
	base := []string{"read-tree", "--empty"}
	if head != "" {
		own, err := r.git(dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
		if err != nil {
			return "", err
		}
		switch body, err := os.ReadFile(own); {
		case err == nil:
			if err := os.WriteFile(tracked, body, 0o600); err != nil {
				return "", err
			}
			base = nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		default:
			base = []string{"read-tree", head}
		}
	}
	if base != nil {
		if _, err := r.git(dir, []string{"GIT_INDEX_FILE=" + tracked}, base...); err != nil {
			return "", err
		}
	}
	if err := r.snapshot(dir); err != nil {
		if strings.Contains(err.Error(), staleWorkingCopy) {
			return "", fmt.Errorf("%w; a person clears that by running `jj workspace update-stale` in %s, and this failure is the only thing that tells them", err, dir)
		}
		return "", err
	}
	commit, err := r.jj(dir, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	if err != nil {
		return "", err
	}
	tree, err := r.git(dir, nil, "rev-parse", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	return r.carried(dir, tracked, filepath.Join(scratch, "index"), tree)
}

// carried is tree with everything the `git add --all` this route replaces would have kept and
// tree lacks added to it. It is built in the scratch index at index, so that neither the
// worktree's own index nor any ref moves and jj's record of what the role's working copy tracks
// is untouched: jj's tree is read into that index, the entries below are written to it, and the
// tree it writes is the answer. tracked is the copy of the worktree's own index snapshotTree
// made before the snapshot, in the git read's own three cases, so it is the index the git read
// starts from (indexTree).
//
// `git add --all` over that copy does two things: it updates every entry the copy holds from the
// worktree, whatever an ignore rule says, but for an entry marked assume-unchanged or
// skip-worktree, whose file it does not read; and it adds as new every file the copy does not
// hold that no ignore rule covers, which is what `ls-files --others --exclude-standard` against
// the copy lists. The carry takes that read's own input and its own rule, read through git's own
// commands below, over a base of jj's tree, which is read twice (ls-tree, then read-tree); git's
// content conversion has no read of its own. The untracked set is read against the index the git
// read walks, entry for entry: the copy itself, by git's own rule, with no index derived from it.
// The tracked set is that index's entries jj's tree lacks or holds at another entry and the
// worktree still holds, and its flagged entries (seeds). The gitlinks of jj's tree are checked
// against the copy's record and what the worktree holds at each (unlinked), and each embedded
// repository the untracked read lists is recorded as the git read records it. A record of the
// copy whose worktree path is now a directory is read by git's own rule too, against the copy
// with those records taken out (nowDirectories), since the untracked read against the copy lists
// nothing there. jj's tree is the base: its entry for a file the copy records as jj's tree holds
// it is kept as jj wrote it, while every file the carry adds or updates, a new file jj already
// holds among them, is written again by git and so passes through git's content conversion,
// which jj's tree does not. For every path the git read records it, drops it or fails, and the
// route answers the same way: where that read fails, as it does on an embedded repository with no
// commit, the route fails the workspace and never drops the path. One exception is named: a
// tracked file made unreadable (`chmod 000`), or one a required clean filter fails on, while it is
// unchanged makes the git read exit 128 (`open("t.txt"): Permission denied`), where jj answers `No
// snapshot needed.` and the route reads it clean; nothing is lost, since the file holds HEAD's
// content, and once the file is edited both reads fail. Where the route keeps less than
// the git read, the case is carried when carrying is bounded by the paths involved, and named when
// it would cost the whole-worktree hash this route removes or when the git read gets it wrong
// too; each named case is a row of the differential test, declared under the word for its kind
// (differential_test.go).
//
// Two named cases are content losses, each of a file jj's tree holds at the copy's own record:
// jj writes the colocated index with HEAD's blob for a file edited in @ and with the empty blob
// for a file new in @, so that is a file HEAD holds that no edit jj snapshotted has changed. Such
// a file rewritten at its size with its mtime set back on purpose to the one jj recorded (`touch
// -r` or `-d`, `cp -p`, `rsync -t`, an archive tool writing stored times) is one jj's stat rule
// passes over, so jj's tree keeps the blob jj read before, where the git read, hashing every file
// of an index with no stat data, reads the new one. A same-size rewrite whose two writes fall
// inside one millisecond can be another: jj trusts a recorded stat (type, mtime in milliseconds,
// size) only where that mtime is strictly earlier than the mtime of its own tree_state file
// (jj-vcs/jj lib/src/local_working_copy.rs:2009-2010 at 47254941, read on jj's main branch; the
// pod's fork carried the same rewrite with its recorded mtime later than tree_state, as that
// rule says). So a loss nobody forced needs jj to read the file between two same-size writes,
// both writes inside one millisecond, and tree_state written in a later one. Measured on
// 2026-11-13, a role's `jj status` walking 20,000 files in 6.5 s while a writer rewrote files in
// same-size pairs lost none of 5957 pairs, 2866 of them inside one millisecond: rare, and not
// proven never to happen. On this volume a plain open, write and close left the mtime unchanged
// on 75.3% of 2000 back-to-back rewrites, in steps of about 1 ms, where a write followed by an
// fsync moved it on all 2000. In both cases the role's own jj commit misses the edit by the same
// rule, so its own branch would not hold it either. Carrying them would cost the whole-worktree
// hash, since nothing but the file's bytes tells such a file from one unchanged. A file jj's tree
// holds at another entry than the copy's, an edit or a new file jj snapshotted, is carried
// whatever its mtime: the carry updates it from the worktree (seeds). Measured on this build on
// 2026-11-13, over an e.txt committed, edited, snapshotted by `jj status` and then rewritten at
// its size with its mtime set back, the git read kept `100644 469c470f` and the route, before it
// updated such a file, jj's `100644 c669c18b`.
//
// One named case is a change of form: git's content conversion (an eol rule of .gitattributes,
// core.autocrlf, a clean filter) stores a file jj's tree holds at the copy's own record otherwise
// than HEAD holds it, since the git read hashes the file again, and the route keeps the bytes
// HEAD holds, as CRLF lines committed before an eol rule was written. A file jj's tree holds at
// another entry is updated by the carry's `git add -f`, which converts it as the git read does
// and keeps the copy's mode under core.fileMode=false. Beside these, one is a file the route
// keeps and the git read loses: a file jj tracks that the copy does not hold, `git reset` having
// taken it out, and that an ignore rule covers, which jj keeps tracking whatever an ignore rule
// says and the git read, adding no ignored file as new, does not add.
//
// The tracked half is the seed (seeds), written into the scratch index as the copy records it
// with `update-index -z --index-info`. Each file of it the copy holds unflagged — a file jj's
// tree lacks or holds at another entry and the worktree still holds — is then updated from the
// worktree with `git add -f -- <file>...`. -f is right here and only here: these are files the
// index tracks, which `git add --all` updates whatever an ignore rule says, so -f adds nothing
// that read would leave out. A file .gitignore ignores that a role forced into the index with
// `git add -f` is one of them, and is carried; so is a tracked file under a directory that has
// since become a jj or git repository of its own, which jj's snapshot leaves out: `git add -f --
// <file>` over an index that holds that file updates it to the worktree's content even under a
// directory holding its own .git. So is a gitlink only the copy holds, which the add records as
// the git read does (seeds). A file jj's tree holds at another entry than the copy's is the
// role's own uncommitted diff, an edit or a new file jj snapshotted, since jj writes the
// colocated index with HEAD's blob for the one and the empty blob for the other: those files are
// the ones this hashes beyond what jj's tree lacks, and on a clean tree there are none. An
// unflagged record whose worktree path is now a directory is seeded too and updated with `git add
// -u -- <path>...` instead, git's own update rule for a tracked path, which records the gitlink
// of a repository there whose HEAD resolves and otherwise takes the record out, and adds nothing
// under the directory (seeds). An entry the copy marks assume-unchanged or skip-worktree is
// seeded and not added, since the git read keeps the blob it records whatever the worktree holds
// there. A copy that holds a record and another under it, both of which the git read keeps,
// fails the workspace before anything is written, as that read fails (keptUnderAKeptPath).
//
// jj's tree keeps a gitlink whatever the copy and the worktree hold at its path; the git read
// does not. Where the copy holds the gitlink and that path is a regular file or a symlink, the
// git read records it; where nothing is there, the git read drops the gitlink, as it drops a
// deleted file; where it is a directory holding a repository, the git read records that
// repository's HEAD, which a role may have moved with a commit inside it; and where it is a
// directory holding no repository whose HEAD git resolves, it keeps the copy's gitlink, which a
// role may have staged at another commit than HEAD's. Measured on this build on 2026-11-13, over a
// committed submodule sub: replaced by a file, the git read recorded `100644 848e6a43` where jj's
// tree held `160000 a2ef393b`; replaced by a symlink, `120000 8286e959` against the gitlink;
// removed, nothing against the gitlink; a commit made inside it and staged, the new HEAD `160000
// 391e84e7` against jj's `160000 1cf8353f`, and unstaged, `160000 181a99d1` against `160000
// ae7c3619`; its directory emptied, as an uninitialized submodule's is, or stripped of its .git,
// the gitlink as jj's tree holds it; and bumped, as a role bumps a submodule jj checked out as an
// empty directory, with `update-index --cacheinfo` over that empty directory, `160000 0ed1e570`
// where jj's tree held `160000 8a186d6f`, or with `git add sub` and then stripped of its .git,
// `160000 9cf9733c` against `160000 8e131d17`. So each gitlink of tree is carried by the copy's
// record at its path (unlinked). Where the copy holds it unflagged, at another commit than jj's
// tree, the seed writes the copy's; then a path git sees nothing at is removed with `update-index
// --force-remove -- <path>...`, which `update-index --add` will not do (`does not exist and
// --remove not passed`, exit 128), and every other path is added by name with `update-index
// --add --replace -- <path>...`, which records each of the others as the git read records it,
// measured on the same shapes: the file, the symlink, the repository's HEAD, and over a
// directory holding no repository whose HEAD git resolves nothing new, exit 0, which keeps the
// gitlink the seed or jj's tree holds there. Where the copy holds no gitlink at the path, the git
// read holds none there to update, so jj's gitlink is removed before the seed, and the path is
// what the copy's own records there and the untracked half make it: measured on this build on
// 2026-11-13, over a committed submodule emptied, taken out of the index with `git rm --cached`
// and with a file then added under its path, the git read recorded that file and no gitlink. A
// gitlink the copy marks assume-unchanged or skip-worktree is none of these: the git read reads
// nothing at its path and keeps the copy's record, so unlinked passes over every path the copy
// flags, and the record is the copy's, as the seed writes it or as jj's tree already holds it
// (seeds). Measured on this build on 2026-11-13, over a committed sub marked either way:
// removed, a commit made inside it, stripped of its .git, replaced by a file or by a symlink,
// the git read kept HEAD's `160000` record in each, where the gitlink carried as an unflagged one
// was removed, moved to the new HEAD, or became the file or the symlink.
//
// The untracked half is one `ls-files -z --others --exclude-standard -- .` over the whole
// worktree against the index copy (GIT_INDEX_FILE=tracked): git's own rule for what `git add
// --all` adds as new, over the index that read adds to. So git walks into a directory holding a
// .git exactly where that read does, which is where the copy holds an entry under it, a deleted
// one among them, whatever the seed holds. It is the one whole-tree read of the worktree this
// route makes, and it walks the worktree's directories and compares names: it hashes no file and
// refreshes no entry, so its cost does not follow the size of the tracked tree the way the git
// read's does (worktreeRead). A file jj refused for its size, a name that is not valid UTF-8 and
// the files of a directory holding a .jj of its own are all among what it lists, as any other
// file the copy does not hold. So are files jj's tree already holds, the new files jj
// snapshotted that the copy lacks; adding them again is harmless, since it writes for each the
// blob the git read writes for it. Under an unborn HEAD the copy holds nothing (snapshotTree), so
// this lists every file of the worktree no ignore rule covers and the carry adds each of them
// again, which hashes it: the cost the git read pays there too, since it starts from the same
// empty index.
//
// A record of the copy that is no gitlink and whose worktree path is now a directory, not beyond
// a symbolic link, is one this read lists nothing at, since the copy holds it as a file, while
// `git add --all` over the copy replaces the record by what the directory holds. So those
// directories alone are read again by the same rule against a copy of the copy without those
// records (nowDirectories), and what that lists joins this read's answer: the files under them,
// which it lists already where the directory is a plain one, and each embedded repository among
// them, which it does not. The update half of that read, which records the gitlink of a
// repository with a commit there whatever an ignore rule says, is the seeds' `git add -u` (seeds).
//
// Each entry is added by name with `update-index --add --replace -- <file>...`, which adds the
// files it names and walks no directory; it adds no file an ignore rule covers, because it is
// handed none. It is not `git add -- <file>...`, whose walk over the scratch index would not be
// the git read's: that index holds jj's tree and the seed and nothing of a deleted file, and
// over an index holding no entry under a directory that holds a .git, `git add` exits 0 having
// added nothing there. Measured on this build on 2026-11-12, over a d whose tracked d/old.txt
// was deleted and which then became a git repository with a commit of d/loose.txt and a new
// d/new.txt: `git add -- d/loose.txt d/new.txt` over an index holding nothing under d exited 0
// and added neither, and `update-index --add` added both at the blobs `git add --all` over the
// copy wrote. --replace lets an added file take the place of an entry the scratch index holds at
// one of its directories, as `git add` lets it: the worktree holds a directory at that path, so
// the git read keeps no entry there either. That takes a seeded flagged entry out too where the
// git read takes it out: measured on this build on 2026-11-13, a file x marked assume-unchanged
// that became a directory holding x/k.txt was dropped by the git read, which added x/k.txt, and
// kept at its record where the directory held nothing or only an ignored file.
//
// An entry git lists with a trailing slash is an embedded git repository (embeddedRepository),
// and it is added as its directory, with no trailing slash, by `update-index --add --replace --
// <dir>...`, which records it as the git read records it or refuses it where that read refuses
// it. Measured on this build on 2026-11-13: over a repository e with a commit, `git add --all`
// and that add both recorded `160000` at e's HEAD; over one with no commit, made with `git init
// e` or a fresh `jj git init --colocate` inside e, `git add --all` exited 128 (`'e/' does not
// have a commit checked out`) and that add exited 128 (`e: is a directory - add files inside
// instead`), and its failure fails the workspace naming the repositories and git's own words,
// where dropping e would have read the workspace clean and lost what e holds. The route adds a
// directory only where this read itself answers a name ending in a slash, and it has no check
// of its own for a .git or a .jj: a directory holding a .git that git walks into, because the
// copy holds an entry under it, is no embedded repository to git and its files are carried, as
// the git read keeps them. Adding such a directory itself would take down the tracked files
// under it: measured on this build on 2026-11-12, over a d that HEAD tracks d/x.log in and that
// a role then made a colocated jj repository, `git add -- d` exited 128 ("does not have a commit
// checked out"), or once d had a commit recorded d as a gitlink, after which `git add -f --
// d/x.log` exited 128 ("is in submodule 'd'").
//
// Every read of paths here is NUL-separated, since a name may hold any byte but NUL; every add
// names the files it adds, in chunks (inChunks); and nothing here runs a bare add, `add --all`,
// `update-index --refresh` or `status` over the worktree.
//
// Then each file of the untracked set and each embedded repository is read back by its name
// (notCarried), and a path the scratch index does not hold is missed: the workspace fails
// naming those paths, since they are in no commit this check can push, and a workspace reported
// clean would have the pod replaced and them lost with it. That is what catches a name git
// refuses, which `update-index --add` passes over with exit 0 (notCarried). The failure names ten
// of them and counts the rest, which is what keeps the message inside what Dispatch takes, and
// writes each with %q, so that a name which is not valid UTF-8 and a name holding the separator
// are both readable. The seeded half is not read back: `update-index --index-info` writes each
// seeded entry before its add, so a read of it by name cannot fail, and the add's exit status is
// that half's guard. Nor is a gitlink of jj's tree, a name that tree already held.
//
// Every read the carry needs is made before jj's tree is read into the scratch index: where
// nothing is seeded, jj's tree holds no gitlink and the untracked read lists nothing to add,
// jj's tree is the answer as it is, with no read-tree, no write-tree and no read-back.
func (r runner) carried(dir, tracked, index, tree string) (string, error) {
	listedTree, err := r.gitPaths(dir, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return "", err
	}
	held := make(map[string]string, len(listedTree))
	var gitlinks []string
	for _, line := range listedTree {
		entry, file, ok := strings.Cut(line, "\t")
		meta := strings.Fields(entry)
		if !ok || len(meta) != 3 {
			return "", fmt.Errorf("git ls-tree answered %q for jj's tree %s, which is no tree entry", line, tree)
		}
		held[file] = meta[0] + " " + meta[2]
		if meta[0] == gitlinkMode {
			gitlinks = append(gitlinks, file)
		}
	}
	half, err := r.seeds(dir, tracked, filepath.Join(filepath.Dir(index), "without"), held)
	if err != nil {
		return "", err
	}
	readded, removed := unlinked(dir, gitlinks, half.copied)
	listed, err := r.gitPaths(dir, []string{"GIT_INDEX_FILE=" + tracked}, "ls-files", "-z", "--others", "--exclude-standard", "--", ".")
	if err != nil {
		return "", err
	}
	untracked, repositories := embeddedRepositories(listed)
	untracked, repositories = joined(untracked, half.untracked), joined(repositories, half.repositories)
	if half.seed == "" && len(readded) == 0 && len(removed) == 0 && len(untracked) == 0 && len(repositories) == 0 {
		return tree, nil
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := r.git(dir, env, "read-tree", tree); err != nil {
		return "", err
	}
	if err := inChunks(removed, func(paths []string) error {
		_, err := r.git(dir, env, append([]string{"update-index", "--force-remove", "--"}, paths...)...)
		return err
	}); err != nil {
		return "", err
	}
	if half.seed != "" {
		if _, err := r.command(dir, env, strings.NewReader(half.seed), "git",
			append(slices.Clone(gitPins), "update-index", "-z", "--index-info")...); err != nil {
			return "", err
		}
	}
	add := []string{"update-index", "--add", "--replace", "--"}
	for _, step := range []struct {
		paths []string
		args  []string
		// failed is the step's error as the workspace's failure says it.
		failed func(paths []string, err error) error
	}{
		{paths: half.updated, args: []string{"add", "-f", "--"}},
		{paths: half.refreshed, args: []string{"add", "-u", "--"}},
		{paths: readded, args: add},
		{paths: repositories, args: add, failed: func(paths []string, err error) error {
			return fmt.Errorf("git could not record the embedded git repositories %s of %s as the gitlinks the git read records, so what they hold is in no commit this check can push: %w", named(paths), dir, err)
		}},
		{paths: untracked, args: add},
	} {
		if err := inChunks(step.paths, func(files []string) error {
			_, err := r.git(dir, env, append(slices.Clone(step.args), files...)...)
			if err != nil && step.failed != nil {
				return step.failed(files, err)
			}
			return err
		}); err != nil {
			return "", err
		}
	}
	missed, err := r.notCarried(dir, env, append(untracked, repositories...))
	if err != nil {
		return "", err
	}
	if len(missed) > 0 {
		return "", fmt.Errorf("jj's snapshot of %s left out paths that git could not carry into the tree this check pushes, which are therefore in no commit it can push: %s", dir, named(missed))
	}
	return r.git(dir, env, "write-tree")
}

// gitlinkMode is the mode git records an embedded repository's commit under.
const gitlinkMode = "160000"

// seeds is the tracked half of the carry, read from the index copy at tracked (trackedHalf): the
// records the carry writes into the scratch index, each as that index records it; the files
// among them that `git add -f` then updates from the worktree; the unflagged records among them
// whose worktree path is now a directory, which `git add -u` updates; what git lists at every
// record that is no gitlink, whose path jj's tree lacks or holds at another entry and whose
// worktree path is now a directory, split into the files and the embedded repositories among
// them (nowDirectories), read against a copy of the copy at without; and every path of the copy,
// with its mode and whether it is flagged, by which unlinked decides each gitlink of jj's tree.
// Each path is named as git names it relative to dir. held is every path of jj's tree with its
// mode and object, as carried reads them. It fails where the copy holds a record and another
// under it that the git read keeps both of (keptUnderAKeptPath).
//
// An unflagged record is seeded where jj's tree lacks its file or holds it at another entry, and
// the worktree at dir still holds it: the files the `git add --all` carried stands in for would
// have updated as tracked and jj's tree lacks or holds otherwise than the copy records them. A
// file jj's tree holds at another entry is the role's own uncommitted diff, an edit jj
// snapshotted, which the copy records at HEAD's blob, or a new file it snapshotted, which the copy
// records at the empty blob (carried). Each of them is updated after, with `git add -f`, or with
// `git add -u` where its path is now a directory (below), over the copy's record, so git updates
// it as the git read does, keeping the record's mode under core.fileMode=false; and a file the
// index holds at more than one stage is seeded once: the add replaces the record's blob with the
// worktree's, so which stage's record seeds it decides nothing. A record jj's tree holds at the
// copy's own mode and object is left as that tree holds it: the git read hashes that file again,
// and carrying it would hash every file of the worktree (carried's named content losses).
//
// A record marked assume-unchanged or skip-worktree is seeded whatever the worktree holds and
// whether or not jj's tree holds its path, but for a directory holding a repository (below), and
// is not updated after: the git read reads no file of it and keeps the blob it records, where
// jj's snapshot reads the file and holds its edit or nothing. `ls-files -v` tags such a record
// with a lower-case letter or with S (flagged).
// Measured on this build on 2026-11-13: tags h for assume-unchanged, S for skip-worktree and s
// for both; a file under either flag, deleted or edited, kept at its record by the git read,
// where jj's tree held no file or the edit; and assume-unchanged d/p.txt and skip-worktree
// e/p.txt, d then a symlink and e a file each an ignore rule covers, kept at their records by
// the git read, which reads no file of either. Where the git read adds an entry that takes a
// flagged record's place, the carry's add of that entry with --replace takes it out as well
// (carried). A flagged record jj's tree holds at the same mode and object is left as that tree
// holds it. A flagged gitlink is seeded and left by the same rule, and unlinked passes over every
// path the copy flags, so that the gitlink is the copy's record whatever the worktree holds at
// its path, as the git read keeps it (carried).
//
// Otherwise, a file git sees nothing at in the worktree (updatedAs) is a deletion: the git read
// records it as one, and jj's tree, missing the file, already does, or holds a gitlink there,
// which unlinked removes. That holds a file one of whose directories is now a symbolic link:
// measured on this build on 2026-11-13, over a tracked d/p.txt whose d became a symlink to a
// directory holding a p.txt, both reads recorded d at 120000 and no d/p.txt, and a seed of
// d/p.txt failed `git add -f` with `pathspec 'd/p.txt' is beyond a symbolic link`: os.Lstat of
// the whole path follows the link and finds the file.
//
// A record that is no gitlink and whose worktree path is now a directory is two things to the
// git read. Its update half updates the record by git's rule for a tracked path: to the gitlink
// of the repository the directory holds where that repository's HEAD resolves, whatever an
// ignore rule says, and to nothing otherwise, but for a flagged record, which it leaves as it
// records it. Its walk then adds what is under the directory as new by the ignore rules, an
// embedded repository there among them, which fails where it has no commit. The walk is read for
// each such path by nowDirectories. The update half is `git add -u -- <path>...` over the seeded
// unflagged record, which updates the tracked paths it names and adds nothing new. It is not
// `git add -f`, which would take the directory whole, ignore rules overridden: measured on this
// build on 2026-11-12, seeding a file x that had become a directory holding x/k.txt and an
// ignored x/i.log and adding it with -f put x/i.log into the index. Measured on this build on
// 2026-11-13, over a committed unflagged p that became a directory, `git add -u -- p` over a copy
// of the index and `git add --all -- .` over another recorded the same at p: `160000` at the
// HEAD of a repository with a commit, ignored or not; nothing where the repository had no
// commit, an ignore rule covering it, where `git add --all` then exited 0, and not covering it,
// where it exited 128 (`'p/' does not have a commit checked out`) and nowDirectories lists `p/`,
// so the route fails there too; and nothing at p for a directory holding a file, which `git add
// --all` added as p/k.txt and the untracked half adds, an empty one, or one holding only ignored
// files. A flagged record there is seeded unless nowDirectories lists a repository at its path,
// whose gitlink then takes its place, or fails, as in the git read; where it lists files, their
// add with --replace takes the record out, and where it lists nothing the record stays (carried).
//
// Any other failure to stat a file keeps it, so its add fails and the workspace with it rather
// than the file being passed over.
//
// A gitlink the copy holds and jj's tree lacks — staged with `git add sub` over an embedded
// repository, or by an uncommitted `git submodule add` — is seeded where its path is still there,
// and updated as a file is: `git add -f -- sub` records the HEAD of the repository its directory
// holds and, where the directory holds no repository whose HEAD git resolves, nothing new, so
// the record stays as the copy records it. Measured on this build on 2026-11-13, over a sub
// staged either way and then stripped of its .git, the git read kept the copy's `160000`
// record, and so did the seed and that add, exit 0, with none of the files left in sub. A
// gitlink the copy holds unflagged at another commit than the gitlink jj's tree holds there — a
// bump staged with `update-index --cacheinfo` or with `git add sub` — is seeded where its path
// is still there and not added: unlinked re-adds that path, which records the HEAD of a
// repository there where one resolves and otherwise keeps the seeded record, as the git read
// keeps it (carried).
//
// What is seeded decides nothing of what git walks into: the untracked half is read against the
// copy itself, which holds a deleted file's record whether or not it is seeded (carried).
func (r runner) seeds(dir, tracked, without string, held map[string]string) (trackedHalf, error) {
	records, err := r.gitPaths(dir, []string{"GIT_INDEX_FILE=" + tracked}, "ls-files", "-s", "-v", "-z")
	if err != nil {
		return trackedHalf{}, err
	}
	var seed strings.Builder
	half := trackedHalf{copied: make(map[string]copyRecord, len(records))}
	var directories []string
	// marked is the flagged records whose worktree path is now a directory, each with its seed.
	var marked []markedRecord
	var files []string
	for _, record := range records {
		entry, file, ok := strings.Cut(record, "\t")
		meta := strings.Fields(entry)
		if !ok || len(meta) != 4 {
			return trackedHalf{}, fmt.Errorf("git ls-files -s -v answered %q for the copy of the worktree's index, which is no index record", record)
		}
		tag, mode, object := meta[0], meta[1], meta[2]
		if _, ok := half.copied[file]; ok {
			continue
		}
		half.copied[file] = copyRecord{mode: mode, flagged: flagged(tag)}
		files = append(files, file)
		line := mode + " " + object + " 0\t" + file + "\x00"
		kept, inTree := held[file]
		switch {
		case inTree && kept == mode+" "+object:
			continue
		case inTree && flagged(tag):
			seed.WriteString(line)
			continue
		}
		gone, directory := updatedAs(dir, file, mode)
		if directory {
			directories = append(directories, file)
		}
		switch {
		case directory && flagged(tag):
			marked = append(marked, markedRecord{file: file, seed: line})
		case flagged(tag):
			seed.WriteString(line)
		case gone:
		case directory:
			half.refreshed = append(half.refreshed, file)
			seed.WriteString(line)
		case inTree && mode == gitlinkMode && strings.HasPrefix(kept, gitlinkMode+" "):
			seed.WriteString(line)
		default:
			half.updated = append(half.updated, file)
			seed.WriteString(line)
		}
	}
	if err := keptUnderAKeptPath(dir, files, half.copied); err != nil {
		return trackedHalf{}, err
	}
	listed, err := r.nowDirectories(dir, tracked, without, directories)
	if err != nil {
		return trackedHalf{}, err
	}
	half.untracked, half.repositories = embeddedRepositories(listed)
	for _, record := range marked {
		if !slices.Contains(half.repositories, record.file) {
			seed.WriteString(record.seed)
		}
	}
	half.seed = seed.String()
	return half, nil
}

// markedRecord is a flagged record of the copy whose worktree path is now a directory, with the
// line that seeds it, which seeds writes unless nowDirectories lists a repository at that path.
type markedRecord struct {
	file, seed string
}

// trackedHalf is the tracked half of the carry, as seeds reads it from the index copy (carried).
type trackedHalf struct {
	// seed is the records written into the scratch index, each at stage 0 and NUL-separated as
	// `update-index -z --index-info` reads them.
	seed string
	// updated is the files of the seed that `git add -f` updates from the worktree.
	updated []string
	// refreshed is the unflagged records of the seed whose worktree path is now a directory, which
	// `git add -u` updates by git's own rule for a tracked path.
	refreshed []string
	// untracked and repositories are what git lists at every record that is no gitlink and whose
	// worktree path is now a directory, flagged or not (nowDirectories), split into the files and
	// the embedded repositories among them (embeddedRepositories).
	untracked, repositories []string
	// copied is every path of the copy, with the mode it records and whether it is marked
	// assume-unchanged or skip-worktree, by which unlinked decides each gitlink of jj's tree.
	copied map[string]copyRecord
}

// nowDirectories is what git's own `ls-files -z --others --exclude-standard` lists at each of
// directories, the copy's records at tracked that are no gitlink and whose worktree path is now
// a directory (seeds), as it would list them if the copy held none of those records: the read is
// made against a copy of the copy at without, with exactly those records taken out by
// `update-index --force-remove`, so it is git's own answer for what each directory is to the git
// read's walk, and not a model of a .git. It answers nothing where directories is empty, and
// reads nothing then.
//
// The whole-worktree listing against the copy lists nothing at such a path: the copy holds it as
// a file, and git then lists no directory there, where `git add --all` over the copy replaces the
// record by what the directory holds. Measured on this build on 2026-11-13, over a committed p,
// regular, marked assume-unchanged or marked skip-worktree, that became a directory holding a git
// repository: `ls-files -z --others --exclude-standard -- .` against the index listed nothing for
// each, and the same against the index with p force-removed listed `p/`; `git add --all` over a
// copy of the index recorded `160000` at the repository's HEAD where it had a commit, the marked
// records among them, and exited 128 (`'p/' does not have a commit checked out`) where it had
// none. And over a committed p that became a plain directory, regular or marked
// assume-unchanged, both listings answered `p/k.txt` for a directory holding k.txt and nothing
// for one empty or holding only an ignored i.log. So an entry listed with a trailing slash is an
// embedded repository, which carried records as the git read does or fails the workspace where
// that read fails; a file is one of the untracked set; and a directory that lists nothing adds
// nothing, so a marked record seeded there stays as the git read keeps it.
func (r runner) nowDirectories(dir, tracked, without string, directories []string) ([]string, error) {
	if len(directories) == 0 {
		return nil, nil
	}
	body, err := os.ReadFile(tracked)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(without, body, 0o600); err != nil {
		return nil, err
	}
	env := []string{"GIT_INDEX_FILE=" + without}
	if err := inChunks(directories, func(paths []string) error {
		_, err := r.git(dir, env, append([]string{"update-index", "--force-remove", "--"}, paths...)...)
		return err
	}); err != nil {
		return nil, err
	}
	var listed []string
	err = inChunks(directories, func(paths []string) error {
		entries, err := r.gitPaths(dir, env, append([]string{"ls-files", "-z", "--others", "--exclude-standard", "--"}, paths...)...)
		listed = append(listed, entries...)
		return err
	})
	return listed, err
}

// joined is listed with each entry of more that listed lacks after it, in order.
func joined(listed, more []string) []string {
	if len(more) == 0 {
		return listed
	}
	seen := make(map[string]bool, len(listed))
	for _, entry := range listed {
		seen[entry] = true
	}
	for _, entry := range more {
		if !seen[entry] {
			seen[entry] = true
			listed = append(listed, entry)
		}
	}
	return listed
}

// copyRecord is one path of the index copy as seeds reads it: the mode it records, and whether
// it is marked assume-unchanged or skip-worktree (flagged).
type copyRecord struct {
	mode    string
	flagged bool
}

// keptUnderAKeptPath is the failure of a copy of the index that holds a record at a path and
// another under it, both of which `git add --all` keeps, and nil for any other copy; files is
// the copy's paths in its own order and copied its records by path. git itself writes no such
// index, but jj's index writer can: measured on this build on 2026-11-13, over a sub staged with
// `git add sub` as a gitlink and then stripped of its .git, a role's `jj status` snapshotted
// sub/inner.txt and wrote it into the colocated index beside the gitlink. `git add --all` over
// that copy exited 0 keeping both, and its write-tree exited 128 (`You have both sub and
// sub/inner.txt`), so the git read fails the workspace there; the carry's seed would replace
// sub/inner.txt with the gitlink and answer a tree. So the route fails it too, by the rule that
// read keeps a record by: a flagged record whatever the worktree holds, and an unflagged one
// where its update keeps it (updatedAs).
func keptUnderAKeptPath(dir string, files []string, copied map[string]copyRecord) error {
	kept := func(file string) bool {
		record := copied[file]
		if record.flagged {
			return true
		}
		gone, directory := updatedAs(dir, file, record.mode)
		return !gone && !directory
	}
	for _, file := range files {
		for at := strings.LastIndexByte(file, '/'); at > 0; at = strings.LastIndexByte(file[:at], '/') {
			if _, ok := copied[file[:at]]; ok && kept(file[:at]) && kept(file) {
				return fmt.Errorf("the git index of %s holds both %q and %q under it, which the git read keeps both of and then refuses to write a tree of (`You have both %s and %s`), so the workspace fails here as it fails there", dir, file[:at], file, file[:at], file)
			}
		}
	}
	return nil
}

// updatedAs is git's keep rule for an unflagged index record of mode at file, which git names
// relative to dir: what `git add --all` finds at its worktree path. gone is where git sees
// nothing there (inWorktree), which that read records as a deletion; directory is where a
// record that is no gitlink is now a directory, which that read replaces by what the directory
// holds (nowDirectories); and where it is neither, that read keeps the record and updates it from
// what is there, a path os.Lstat fails on for any other reason among them, whose add then fails
// the workspace rather than the path being passed over. seeds and keptUnderAKeptPath each decide
// by it.
func updatedAs(dir, file, mode string) (gone, directory bool) {
	info, gone, err := inWorktree(dir, file)
	return gone, !gone && err == nil && info.IsDir() && mode != gitlinkMode
}

// flagged is whether tag, the letter `ls-files -v` puts before an index record, marks the
// record assume-unchanged, a lower-case letter, or skip-worktree, S: a record whose file `git add
// --all` does not read.
func flagged(tag string) bool {
	return tag == "S" || tag != strings.ToUpper(tag)
}

// inWorktree is os.Lstat of file, which git names relative to dir, with gone set where git sees
// nothing at that path: no such file (ENOENT), a directory on its path that is now a file
// (ENOTDIR), or a directory on its path that is a symbolic link (beyondSymlink), which git reads
// nothing through while os.Lstat of the whole path follows it. Any other error is answered with
// gone false, so that a caller keeps the path and git's own read of it fails the workspace
// rather than the path being passed over. updatedAs and unlinked each decide by it.
func inWorktree(dir, file string) (info fs.FileInfo, gone bool, err error) {
	info, err = os.Lstat(filepath.Join(dir, file))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || beyondSymlink(dir, file) {
		return nil, true, nil
	}
	return info, false, err
}

// beyondSymlink is whether a directory on the path of file, which git names relative to dir, is
// a symbolic link. git reads no file through one, so to git such a path holds nothing, while
// os.Lstat of the whole path follows the link.
func beyondSymlink(dir, file string) bool {
	for at := strings.IndexByte(file, '/'); at >= 0; {
		info, err := os.Lstat(filepath.Join(dir, file[:at]))
		if err != nil {
			return false
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return true
		}
		next := strings.IndexByte(file[at+1:], '/')
		if next < 0 {
			return false
		}
		at += 1 + next
	}
	return false
}

// unlinked is the gitlinks of jj's tree, which git names relative to dir, split as the git read
// records them (carried), by copied, the copy's records by path (seeds). jj's tree keeps a
// gitlink whatever the copy and the worktree hold at its path, and the git read starts from the
// copy, so each is decided by the copy's record at its path.
//
// A path the copy marks assume-unchanged or skip-worktree is in neither list: the git read reads
// nothing at it and keeps the copy's record, which the seed writes where jj's tree holds another
// (seeds).
//
// A path the copy holds as an unflagged gitlink is updated as the git read updates that record.
// removed is each such path git sees nothing at (inWorktree), which the git read drops, and
// readded is every other, which carried passes through `update-index --add --replace --
// <path>...` over the copy's record, which the seed writes where jj's tree holds another
// gitlink (seeds). That records what the git read records at each: the file or symlink now in
// the gitlink's place; the HEAD of the repository the directory holds, which a role may have
// moved; and, where the directory holds no repository whose HEAD git resolves, an uninitialized
// submodule's empty directory among them, nothing new, which keeps the copy's gitlink, a bump a
// role staged with `update-index --cacheinfo` or `git add` among them. A path os.Lstat fails on
// for any other reason is readded, so that its add fails and the workspace with it rather than
// the gitlink being passed over.
//
// A path the copy holds no gitlink at is removed, whatever the worktree holds there: the git
// read holds no gitlink there to update, and what it records at the path or under it is the
// copy's own records there, which the seed writes and the adds update, and what its walk adds as
// new, which the untracked half adds, an embedded repository's gitlink among them. carried takes
// these out before the seed is written, so that the seed and the adds write over a scratch index
// that no longer holds jj's gitlink. Measured on this build on 2026-11-13, over a committed
// submodule sub emptied, taken out of the index with `git rm --cached sub`, and with
// sub/new.txt written and added: the git read recorded sub/new.txt and no gitlink, and a re-add
// of sub after the seed of sub/new.txt had taken jj's gitlink out exited 128 (`sub: is a
// directory - add individual files instead`).
func unlinked(dir string, gitlinks []string, copied map[string]copyRecord) (readded, removed []string) {
	for _, link := range gitlinks {
		record, ok := copied[link]
		switch {
		case record.flagged:
		case !ok || record.mode != gitlinkMode:
			removed = append(removed, link)
		default:
			if _, gone, _ := inWorktree(dir, link); gone {
				removed = append(removed, link)
			} else {
				readded = append(readded, link)
			}
		}
	}
	return readded, removed
}

// embeddedRepository is the directory of an embedded git repository where entry, as `ls-files
// --others` lists it, is one, and false where it is not: git lists such a directory as itself,
// with a trailing slash, and never lists a file in it.
func embeddedRepository(entry string) (string, bool) {
	return strings.CutSuffix(entry, "/")
}

// embeddedRepositories is entries split into the files among them and the embedded git
// repositories, each repository named as its directory, with no trailing slash, which is how
// `update-index --add` names the gitlink it records there (carried).
func embeddedRepositories(entries []string) (files, repositories []string) {
	for _, entry := range entries {
		if repository, ok := embeddedRepository(entry); ok {
			repositories = append(repositories, repository)
		} else {
			files = append(files, entry)
		}
	}
	return files, repositories
}

// pathspecBudget is how many bytes of paths one git command this carry runs over a set of files
// names at a time (inChunks). Either half of the carry can be as large as the worktree — a
// directory holding a nested .jj can hold a whole jj store, and every file of it is new to git —
// and one command line takes only so many bytes, so each such command is made in chunks of this
// size and no file of the set goes unadded or unread for want of room on it.
const pathspecBudget = 64 << 10

// inChunks calls each with paths in order, in runs of at most pathspecBudget bytes, a run
// holding one path at the least, and stops at the first error, which it answers.
func inChunks(paths []string, each func(chunk []string) error) error {
	for rest := paths; len(rest) > 0; {
		n, size := 0, 0
		for n < len(rest) && (n == 0 || size+len(rest[n]) <= pathspecBudget) {
			size += len(rest[n])
			n++
		}
		if err := each(rest[:n]); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// notCarried is every file of untracked that the index at env does not hold: each is read by its
// own name with `ls-files -z -- <file>...`, in chunks, and a file is held only where git answers
// that exact name. It is a read of named paths, not a second read of the whole worktree.
//
// It catches a name git refuses, which `update-index --add` passes over with exit 0 having added
// nothing. Measured on this build on 2026-11-13, `update-index --add --replace --` over new files
// named GIT~1, `.git. `, `git~1` and `.GIT` printed `Ignoring path GIT~1` and the same for each
// of the others, exited 0 and added none of them, while `ls-files --others` listed all four. jj's
// snapshot takes such a file where it is small enough, and then the `read-tree` of jj's tree into
// the scratch index refuses it (`invalid path 'GIT~1'`, exit 128, measured on this build on
// 2026-11-13) and fails the workspace; one above jj's size limit is in neither, and only this
// read-back finds it missing (differential_test.go). The `git add --all` this route replaces
// refuses the same new names, with `invalid path 'GIT~1'` and exit 128, so the workspace fails by
// either read. A file the add cannot read is another matter: `update-index --add` exits
// non-zero for it, and that exit fails the workspace before the read-back runs
// (TestAFileTheCarryCannotAddFailsTheWorkspaceAndPushesNothing). The read-back covers the files
// the untracked half adds and the embedded repositories it records, since the seeded half
// writes each entry before its add and a gitlink of jj's tree is a name that tree already held
// (carried).
func (r runner) notCarried(dir string, env []string, untracked []string) ([]string, error) {
	var missing []string
	err := inChunks(untracked, func(files []string) error {
		held, err := r.gitPaths(dir, env, append([]string{"ls-files", "-z", "--"}, files...)...)
		if err != nil {
			return err
		}
		holds := make(map[string]bool, len(held))
		for _, file := range held {
			holds[file] = true
		}
		for _, file := range files {
			if !holds[file] {
				missing = append(missing, file)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return missing, nil
}

// quoted is paths as one text a person reads, each written with %q. A path, a sparse pattern and
// an argument of a command that names a path may all hold any byte, the separator this list is
// written with among them, so none is written bare: quoting is what keeps a list of them one
// list where a reader can tell its members apart, and it keeps a name that is not valid UTF-8
// readable.
func quoted(paths []string) string {
	one := make([]string, len(paths))
	for i, path := range paths {
		one[i] = fmt.Sprintf("%q", path)
	}
	return strings.Join(one, ", ")
}

// staleWorkingCopy is what jj says of a working copy an operation elsewhere moved on from: it
// refuses the snapshot and exits 1, so this text reaches the check inside the command's error.
const staleWorkingCopy = "working copy is stale"

// maxNamedPaths is how many paths a failure names: a workspace of many of them would otherwise
// make a message longer than Dispatch takes.
const maxNamedPaths = 10

// named is up to maxNamedPaths of paths as a person reads them, with a count of the rest.
func named(paths []string) string {
	if len(paths) > maxNamedPaths {
		return fmt.Sprintf("%s, and %d more", quoted(paths[:maxNamedPaths]), len(paths)-maxNamedPaths)
	}
	return quoted(paths)
}

// emptyTree is git's tree with nothing in it.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// fields is out's lines, each trimmed, the empty ones dropped.
func fields(out string) []string {
	return strings.Fields(out)
}
