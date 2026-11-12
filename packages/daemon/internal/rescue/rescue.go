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
// What that snapshot writes was measured in an issue pod on 2026-11-12, on jj
// 0.45.1-sami.20260910 and git 2.47.3. A snapshot of a quiescent colocated worktree writes four
// things: one jj operation, a new working-copy commit, the colocated git index, and one
// refs/jj/keep/<commit> ref in the shared clone. Where the worktree's git HEAD had moved behind
// jj's back, the snapshot prints `Reset the working copy parent to the new Git HEAD.` and writes
// three such refs: the new working-copy commit, the git HEAD it imported, and the empty commit
// it makes on that HEAD. Where a bookmark points at the working-copy commit, the snapshot moves
// that bookmark and exports it to refs/heads/<name> in the shared clone. No configuration stops
// that export, and the move is the move any jj command of any role makes in that same workspace.
// The worktree's git HEAD is unchanged in each case, and so is every other refs/heads/<name>.
// The shared clone, which holds those refs, is on the pod's own disk, and that disk goes with
// the pod.
//
// The snapshot writes the colocated git index, so a role's own staged git index in that
// workspace can be overwritten. One file of that index goes with it, and only one: a file
// .gitignore ignores that a role forced in with `git add -f`. jj never commits such a file,
// and the snapshot writes an index that does not name it, so it goes at the next jj command
// that checks out a new working-copy commit. The git read this snapshot replaces keeps it,
// so that file is the one this check loses where the git read would have carried it. Every
// other file of the worktree is read as it stands and carried, the paths the snapshot itself
// leaves out among them (snapshotTree).
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

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
// role wrote, read out of jj's own warning or off the worktree's own directory entries, and
// never a pattern. Without it git reads each as a pathspec, where a name beginning with `:` is
// magic for what follows it and a name holding a glob character stands for every path it
// matches: measured on this build on 2026-11-12, over a worktree holding a refused 2 MiB file
// named `:aa` beside a tracked `aa`, `git add -f -- ':aa'` exited 0 with the index holding `aa`
// alone, and the same add with this option put `:aa` in it.
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
// colocated index with no stat data at all — measured on jj 0.45.1-sami.20260910, every entry it
// writes carries ctime, mtime, dev, ino, uid, gid and size 0 — so that read over a worktree jj
// holds hashes every tracked file, whether or not one of them changed.
//
// What that costs, measured in issue pods on 2026-11-11, over jj workspaces read that way: on a
// 13 GB workspace of 291,957 tracked files, on two CPUs, git read every entry as changed (78 s
// to list them) and took 200 s to answer that no file had changed; the same read on other pods'
// workspaces took 9 minutes 13 seconds and up to 25 minutes. Refreshing the index first is that
// same read and buys nothing: it pays the hash once and jj writes the stat data away again at
// its next snapshot.
//
// The add itself was measured at 27 s on OSSV-202 and 36.8 s as a dry run on OSSV-349, each on
// a pod carrying that night's by-hand cure, which put .jj/ and .codegraph/ in the clone's
// exclude file and refreshed the index, so each of those two read less than a pod's own read
// does. On the architect's fixture the add alone cost 1.8 to 3.9 times the refresh alone of the
// same tree, which carried to the slowest of the thirteen suggests an add of 45 to 98 minutes.
//
// So 30 minutes is about nine times the slowest read measured directly on a pod, and under what
// that ratio suggests for the slowest: this term is not sized for the worst case, and a
// workspace whose read is slower than it fails as it does today and waits for a person. What it
// is sized against is the bound those thirteen pods met on 2026-11-11, which was two minutes:
// the read ran out, the check failed the workspace, no pod was replaced and every one of them
// waited on a person — while in all thirteen the files the read was after turned out to hold
// nothing the remote lacked.
//
// The term stays in a workspace's bound for two reads, and the measurements above are of the
// first of them. The daemon sends this bound to every pod, and a pod whose shim predates this
// build still makes the whole-worktree git read over a jj workspace, which is the read those
// pods measured. This build makes that git read for a worktree with no .jj directory (edits,
// indexTree), whose index git itself wrote with stat data it can use. A worktree jj holds takes
// jj's own snapshot here in its place, which costs what any jj command of a role costs.
const worktreeRead = 30 * time.Minute

// pushReserve is what a workspace's bound keeps back from the read of its files: the guard's own
// run of the shared check (CheckRun) and what that guard leaves the push (GuardReserve). A read
// that runs past it would take the commits this check has already found down with it, which is
// the loss check stops the read to prevent.
const pushReserve = CheckRun + GuardReserve

// wholeDiskMargin is what the bound of a whole disk's check keeps over one workspace's: enough
// for the workspaces a disk holds past the first, and the three minutes it has always kept.
const wholeDiskMargin = 3 * time.Minute

// WorkspaceTimeout and Timeout are this check's two bounds: one workspace's check and push, and
// the whole check of a pod's disk. The daemon sets them on every request it makes
// (internal/daemon's rescueWorkspaceTimeout and rescueTimeout are these), and a fixture that
// drives this package as a pod's shim does bounds its own run by them (rescuetest.Disk.Check,
// internal/shim's tests): a fixture bounded under GuardReserve would have every push refused
// for want of time rather than checked.
//
// A workspace's bound holds four things in a pod, and each is measured: the reads that find the
// unpushed work (readsBeforeThePush), the read of the worktree's own files (worktreeRead), the
// guard's own run of the shared check (CheckRun), and what that guard leaves the push, which in
// a pod runs the same check a second time in the pre-push hook (GuardReserve). 90 + 1800 + 120 +
// 150 is 2160 s, so a workspace's bound is 36 minutes and the whole disk's, keeping
// wholeDiskMargin over it, 39. Each is a ceiling and not a wait: a workspace whose files git can
// answer for at once is checked and pushed in seconds, as every one of this package's tests is.
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
// nothing of it is trimmed (carried, which reads the worktree's own names this way). Every other
// read of this package comes through git above, which trims what it answers: a sha, a ref name,
// and in worktrees the directories of the repository's worktrees, which are paths too, read a
// line at a time out of `git worktree list --porcelain` rather than this way.
func (r runner) gitPaths(dir string, env []string, args ...string) ([]string, error) {
	out, _, err := r.output(dir, env, nil, "git", append(slices.Clone(gitPins), args...)...)
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
// (worktreeRead). It returns what jj wrote to its standard error, which its caller must read:
// jj refuses a file above the workspace's size limit and skips a path whose name is not valid
// UTF-8, each with a warning there and an exit status of 0, so what it left out reaches a
// caller nowhere else (snapshotTree, leftOut).
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
func (r runner) snapshot(dir string) (string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("the git this check runs is not on its PATH, so jj could not snapshot %s: %w", dir, err)
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
	_, stderr, err := r.output(dir, nil, nil, "jj", append(args, "util", "snapshot")...)
	return strings.TrimSpace(stderr), err
}

// observeCommand is nil outside this package's tests, where export_test.go sets it: the one
// place a test can read every git and jj command this check runs, with its arguments, since
// no test may put a stand-in binary on PATH.
var observeCommand func(dir, name string, args []string)

func (r runner) command(dir string, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	out, _, err := r.output(dir, env, stdin, name, args...)
	return strings.TrimSpace(out), err
}

// output runs one command and answers its standard output and its standard error as they
// stand, where it exits 0: a name git writes may hold a space at either end, so nothing is
// trimmed here and each caller trims what it reads, or does not (command, snapshot,
// gitPaths). It is the one implementation the package runs every git and jj through. Two
// callers come here directly: snapshot, whose command's standard error holds what its caller
// must read, and gitPaths, whose read's standard output must reach its caller untrimmed and
// which drops the standard error. Every other caller comes through command, which trims the
// output and drops the standard error. A command that could not be started or that exits
// non-zero is an error naming the command, the status and that standard error, so nothing of
// what it said is lost on any path. Each argument of the command is written with %q (quoted):
// an argument may be a path a role wrote, and a space inside one would otherwise read as the
// gap between two arguments.
func (r runner) output(dir string, env []string, stdin io.Reader, name string, args ...string) (string, string, error) {
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
		return "", "", fmt.Errorf("%s %s: %v", name, quoted(args), err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return "", "", fmt.Errorf("%s %s: exit %d: %s", name, quoted(args), code, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
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
// differ from head's tree; "" when they do not. A file .gitignore ignores that head does not
// track is left out, as a role's own push would leave it: by both reads below, and by the carry
// that follows jj's own for the paths its snapshot left out, which adds under each of them what
// `git add --all` would have added there and no more (carried).
//
// Where the worktree holds a .jj directory, the tree is jj's own: the check takes jj's snapshot
// of the worktree, then the tree of the working-copy commit that snapshot wrote, and then
// carries into it every path that snapshot left out (snapshotTree). Where it does not, the
// tree is git's, read into an index of its own from a copy of the worktree's where it has one,
// so that neither the worktree's index nor any ref moves (indexTree). The two answer the same
// question, and the jj one is the one a pod can afford: the git read hashes every tracked file
// there (worktreeRead).
//
// Of the two reads, only the snapshot writes anything of that worktree's own: its colocated git
// index, over a role's own staged index there, which costs the one file this package's doc
// names and no other. The git read writes no index of the worktree at all, since it builds its
// tree in an index of its own.
//
// A conflicted working copy is no failure. jj writes a conflict into the worktree itself, as
// .jjconflict-base-0/, .jjconflict-side-0/, .jjconflict-side-1/, JJ-CONFLICT-README and the
// one-sided file, so the snapshot carries every side of it and no content is lost; failing there
// would hold a pod replacement back for work the branch already holds.
//
// An untracked nested git repository is carried by neither read and fails neither: `git add
// --all` records a gitlink, whose commit no object of this repository holds, and jj does not
// snapshot it at all. What is in it goes with the pod, as it would have gone had no check run.
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
// them, with every path that snapshot left out carried into it. The things that can lose a file
// are read in this order.
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
// The snapshot itself fails the workspace where it fails, and is not made twice: it leaves the
// working copy as the last jj command of a role wrote it, and no retry of ours makes a lock or
// a stale working copy any newer. A stale one is the single case a person can clear, so the
// failure says what they run, since nothing else tells them.
//
// What the snapshot left out comes last (leftOut): a file above the workspace's own size limit,
// a path whose name is not valid UTF-8, and a directory holding a .jj of its own. Each is
// carried into the tree this check pushes, as far as the `git add --all` this read replaces
// would have carried it and no further (carried), since a commit missing what that read kept
// would be pushed as the whole of a role's work. head is the worktree's git HEAD, "" where it
// is unborn, which is what tells the carry the files that read would have updated as tracked.
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
	stderr, err := r.snapshot(dir)
	if err != nil {
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
	left, err := leftOut(dir, stderr)
	if err != nil {
		return "", err
	}
	if len(left) == 0 {
		return tree, nil
	}
	return r.carried(dir, head, tree, left)
}

// carried is tree with each of paths added to it, as the `git add --all` this check's other
// read makes over a copy of the worktree's own index would have added them. It is built in a
// scratch index of its own, so that neither the worktree's own index nor any ref moves and jj's
// record of what the role's working copy tracks is untouched: jj's tree is read into that index,
// each path is added to it, and the tree that index writes is the answer. Measured on this
// build on 2026-11-12: such an index carried a file jj had refused at its full 2 MiB and
// carried a directory holding a nested .jj whole, and `jj st` in the workspace afterwards still
// reported the large file untracked.
//
// A path the worktree does not hold is missed before anything else is read of it. Every path
// handed here is one jj's warning named or one the walk for a nested .jj found, so a path that
// is not there is a misread of that warning: the path jj named is not the one this check read.
// Nothing later would say so, since `ls-files --others` over a path that does not exist exits 0
// and answers nothing, which reads as nothing to carry.
//
// What a path that is there carries is read before it is added, and it is two sets, which
// together are the path's expected set: what the read this one replaces would have carried
// for it. The untracked set is what `ls-files --others --exclude-standard` over the path
// answers against the scratch index: the files under it that index does not hold already and no
// ignore rule covers, which is the rule `git add --all` follows for a file the index does not
// track. The tracked set is the files head's tree holds under the path that the worktree still
// holds (trackedUnder), since `git add --all` updates every file the index tracks whatever an
// ignore rule says, and the untracked set names none of them where an ignore rule covers them.
// Measured on this build on 2026-11-12, over a d/x.log committed before `*.log` was ignored and
// then edited under a d that gained a nested .jj: jj's snapshot answered `D d/x.log`, the
// untracked set of d was d/.jj/f alone, and an add of d put no d/x.log in the index. Both sets
// are read NUL-separated because a name may hold a newline.
//
// An expected set holding nothing is nothing to carry, and the path, which is there, is passed
// over with no add and no failure. Three shapes answer that way, and each is right to pass
// over. An untracked ignored file and an untracked ignored directory are content `git add
// --all` leaves out, so a commit of this check holding them would push what a role's own push
// would not (edits), and a `git add` that names an ignored file outright fails besides — exit
// 1, "The following paths are ignored by one of your .gitignore files", measured on this build
// on 2026-11-12. A path inside an embedded git repository is carried by neither read: `git add
// --all` records such a repository as a gitlink, whose commit no object of this repository
// holds, and jj does not snapshot it at all. The architect's ruling for that one is that it
// fails neither read, since failing the workspace here would hold a pod replacement back for
// content no read of this check can carry.
//
// Otherwise the untracked set is added file by file with `git add -- <file>...`, with no -f —
// what -f would add there is what an ignore rule covers, and that is the content this carry
// must not push — and an entry of it that git lists with a trailing slash is left out: that is
// an embedded git repository (embeddedRepository), which `git add --all` records as a gitlink
// at most and carries no file of. Adding the directory itself would have git record that
// gitlink, or refuse it where the repository has no commit checked out, and either way take
// down the tracked files under it: measured on this build on 2026-11-12, over a d that HEAD
// tracks d/x.log in and that a role then made a colocated jj repository, `git add -- d` exited
// 128 ("does not have a commit checked out"), or once d had a commit recorded d as a gitlink,
// after which `git add -f -- d/x.log` exited 128 ("is in submodule 'd'").
//
// The tracked set is first put into the scratch index as head holds it, with `update-index
// --index-info` fed head's own `ls-tree` records, and then updated with `git add -f -- <file>...`.
// That is exactly how the read this one replaces carries a tracked file, and it is the one add
// here -f is right for: `git add --all` updates every file the index tracks whatever an ignore
// rule says, and these are exactly files head tracks, so -f adds nothing that read would have
// left out. The seeding is what makes a tracked file under a nested repository carried at all:
// measured on this build on 2026-11-12, `git add -f -- d/x.log` over an index that did not hold
// d/x.log, with d/.git in the worktree, exited 0 and added nothing, and the same add over an
// index seeded with head's d/x.log recorded the worktree's edit. Then what was added is read
// back (notCarried). Reading the add back is what git's exit status does not give: `git add`
// exits 0 for a path it added nothing of, which that shape and a path read as a pathspec rather
// than as the name it is both do (gitPins, which is where the second is kept from happening).
//
// A path the worktree does not hold, a file of an expected set that was not carried, and a
// path whose add failed are missed, and the workspace fails naming them: those files are in no
// commit this check can push, and a workspace reported clean would have the pod replaced and
// them lost with it. The failure names ten of them and counts the rest, which is what keeps the
// message inside what Dispatch takes, and writes each with %q, so that a name which is not
// valid UTF-8 and a name holding the separator are both readable.
func (r runner) carried(dir, head, tree string, paths []string) (string, error) {
	scratch, err := os.MkdirTemp("", "legion-rescue-carry-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index")}
	if _, err := r.git(dir, env, "read-tree", tree); err != nil {
		return "", err
	}
	var missed []string
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(dir, path)); err != nil {
			missed = append(missed, path)
			continue
		}
		listed, err := r.gitPaths(dir, env, "ls-files", "-z", "--others", "--exclude-standard", "--", path)
		if err != nil {
			return "", err
		}
		untracked := withoutEmbeddedRepositories(listed)
		tracked, seed, err := r.trackedUnder(dir, head, path)
		if err != nil {
			return "", err
		}
		if len(untracked) == 0 && len(tracked) == 0 {
			continue
		}
		if err := inChunks(untracked, func(files []string) error {
			_, err := r.git(dir, env, append([]string{"add", "--"}, files...)...)
			return err
		}); err != nil {
			missed = append(missed, path)
			continue
		}
		if len(tracked) > 0 {
			if _, err := r.command(dir, env, strings.NewReader(seed), "git",
				append(slices.Clone(gitPins), "update-index", "-z", "--index-info")...); err != nil {
				missed = append(missed, path)
				continue
			}
			if err := inChunks(tracked, func(files []string) error {
				_, err := r.git(dir, env, append([]string{"add", "-f", "--"}, files...)...)
				return err
			}); err != nil {
				missed = append(missed, path)
				continue
			}
		}
		lost, err := r.notCarried(dir, env, path, tracked)
		if err != nil {
			return "", err
		}
		missed = append(missed, lost...)
	}
	if len(missed) > 0 {
		return "", fmt.Errorf("jj's snapshot of %s left out paths that git could not carry into the tree this check pushes, which are therefore in no commit it can push: %s", dir, named(missed))
	}
	return r.git(dir, env, "write-tree")
}

// embeddedRepository is whether an entry `ls-files --others` lists is an embedded git repository:
// git lists such a directory as itself, with a trailing slash, and never lists a file in it. The
// `git add --all` this carry stands in for records it as a gitlink at most and carries no file of
// it, which is the one entry of that read this carry does not make (carried).
func embeddedRepository(entry string) bool {
	return strings.HasSuffix(entry, "/")
}

// withoutEmbeddedRepositories is entries with every embedded git repository left out.
func withoutEmbeddedRepositories(entries []string) []string {
	kept := entries[:0:0]
	for _, entry := range entries {
		if !embeddedRepository(entry) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// trackedUnder is the files head's tree holds under path that the worktree at dir still holds,
// each named as git names it relative to dir: the files under path that the `git add --all`
// carried stands in for would have updated as tracked. With them it answers head's own records
// of those files, NUL-separated as `update-index -z --index-info` reads them, so the carry can
// put them into its index as head holds them before it updates them. An unborn head, "", tracks
// nothing. A file head holds that the worktree no longer does is a deletion, which that read
// records too and which jj's tree, missing the file, already holds, so it is no file to carry; a
// file one of whose directories is now a file of its own is one such deletion. Any other failure
// to stat a file keeps it, so the add of it fails and the path is missed rather than the file
// passed over.
func (r runner) trackedUnder(dir, head, path string) ([]string, string, error) {
	if head == "" {
		return nil, "", nil
	}
	records, err := r.gitPaths(dir, nil, "ls-tree", "-r", "-z", head, "--", path)
	if err != nil {
		return nil, "", err
	}
	var held []string
	var seed strings.Builder
	for _, record := range records {
		_, file, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, "", fmt.Errorf("git ls-tree answered %q for %s, which holds no path", record, path)
		}
		if _, err := os.Lstat(filepath.Join(dir, file)); errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		held = append(held, file)
		seed.WriteString(record)
		seed.WriteByte(0)
	}
	return held, seed.String(), nil
}

// pathspecBudget is how many bytes of paths one git command this carry runs over a set of files
// names at a time (inChunks). A tracked set is as large as the directory it was read for — a
// directory holding a nested .jj can hold a whole jj store — and one command line takes only so
// many bytes, so each such command is made in chunks of this size and no file of the set goes
// unadded or unread for want of room on it.
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

// notCarried is every file of path's expected set that the index at env does not hold after
// the adds of it, which is every file of it the carry lost, read by one rule. Whatever
// `ls-files --others --exclude-standard` over path still lists after the adds is not in the
// index: that is the read the untracked set came from, made again, with an embedded git
// repository left out of it by the same rule it was left out of the adds by
// (withoutEmbeddedRepositories), since git lists one as itself whatever the index holds beside
// it. And every file of tracked that `ls-files` over its own name does not answer by that exact
// name is not in the index either. Nothing is matched as a pathspec against what the index
// holds, which is what reading the untracked set again buys.
//
// This read-back is defence in depth, and no test of this package reaches its failing branch:
// git gives each set immediately before the add of the same path, and an embedded repository,
// the one shape whose add git answers with 0 having added nothing, gets an empty set and is
// passed over before any add. Measured on 2026-11-12 with this function made to answer nothing,
// every test of this package passed. It stays for a later change that takes a set from
// somewhere other than git's own answer, which is where a set and an add could disagree.
func (r runner) notCarried(dir string, env []string, path string, tracked []string) ([]string, error) {
	listed, err := r.gitPaths(dir, env, "ls-files", "-z", "--others", "--exclude-standard", "--", path)
	if err != nil {
		return nil, err
	}
	missing := withoutEmbeddedRepositories(listed)
	err = inChunks(tracked, func(files []string) error {
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

// refusedHeader and skippedHeader are the lines jj writes before the paths its snapshot left
// out, each with an exit status of 0, and maxNamedPaths is how many paths a failure names: a
// workspace of many of them would otherwise make a message longer than Dispatch takes.
const (
	refusedHeader = "Refused to snapshot some files:"
	skippedHeader = "Skipped some paths because they are not valid UTF-8:"
	maxNamedPaths = 10
)

// leftOut is every path of the worktree at dir that jj's snapshot did not carry and the `git
// add --all` it replaces would have kept, each relative to dir and each once. There are three
// kinds, and the first two are read out of the standard error of the snapshot itself, which is
// where jj says it left them and where it says nothing else of them.
//
// A new file above the workspace's own snapshot.max-new-file-size is refused, one to a line
// under refusedHeader, in the one shape jj writes such a line in (refusedLine). The path is
// read as the longest text that leaves the rest of that shape standing after it, so a path
// holding `: ` of its own is read whole.
//
// A path whose name is not valid UTF-8 is skipped, one to a line under skippedHeader, as the
// directory it is in, then `: `, then the name as Rust debug-quotes it (`  .: "bad\xFD.txt"`).
// The quoted name is cut off the end of the line and decoded back to the bytes it holds
// (skippedPath, unquoted), and joined to that directory, whose `.` is the worktree's own root.
// A Go string holds those bytes whatever they are and exec.Command passes them to git as they
// are, so nothing here re-encodes a name.
//
// A line of either block that is not the shape of its block is a path this cannot read, and
// the workspace fails naming those lines rather than passing them over: a path read wrongly is
// a file left out of the commit this check pushes while the workspace is reported clean. jj
// writes a refused path raw, so a name holding a newline is written over two lines, in one of
// two shapes. A bare newline, one the name does not follow with two spaces, starts a line with
// no two-space indent, which ends jj's block (warned): the first fragment is then the block's
// last line, and it has no shape. Measured on this build on 2026-11-12, that is what such a
// file makes, and a cut at the first `: ` read it as a path of its own. A newline the name
// follows with two spaces keeps the next line inside the block, where the first fragment still
// has no shape and the second may read as a path of the refusal shape: a file named
// `refused-01\n  spill.bin` is written as `  refused-01` and `  spill.bin: 4.7KiB (4800 bytes);
// the maximum size allowed is 1.0KiB (1024 bytes)`, whose second line names spill.bin, which is
// not the file jj refused. Such a path is not there unless the worktree holds a file of that
// name besides, and a path that is not there fails the workspace (carried).
//
// A directory holding a .jj of its own is left out whole, and jj says nothing at all: measured
// on this build on 2026-11-12, a worktree holding d/.jj/f and d/plain.txt snapshotted to a
// tree with neither of them in it, where `git add --all` keeps both. So the worktree is walked
// for one (nestedJJ) and the directory itself is the path carried.
func leftOut(dir, stderr string) ([]string, error) {
	var paths, unread []string
	add := func(path string) {
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	for _, line := range warned(stderr, refusedHeader) {
		if found := refusedLine.FindStringSubmatch(line); found != nil && found[1] != "" {
			add(found[1])
			continue
		}
		unread = append(unread, line)
	}
	for _, line := range warned(stderr, skippedHeader) {
		if path, ok := skippedPath(line); ok {
			add(path)
			continue
		}
		unread = append(unread, line)
	}
	if len(unread) > 0 {
		return nil, fmt.Errorf("jj's snapshot of %s said it left out paths in lines this check cannot read, so what they name is in no commit it can push: %s", dir, named(unread))
	}
	nested, err := nestedJJ(dir)
	if err != nil {
		return nil, err
	}
	for _, path := range nested {
		add(path)
	}
	return paths, nil
}

// refusedLine is the one shape jj writes under refusedHeader for a file its snapshot refused:
// the path, then its size, that size in bytes and the limit it was refused for. Measured on
// this build on 2026-11-12, over a 2 MiB file named `:aa` in a workspace whose limit is 1 MiB:
// `  :aa: 2.0MiB (2097152 bytes); the maximum size allowed is 1.0MiB (1048576 bytes)`. The
// path is greedy, which is what takes the longest path the rest of the shape leaves standing.
var refusedLine = regexp.MustCompile(`^(.*): [^ ]+ \([0-9]+ bytes\); the maximum size allowed is .+$`)

// skippedPath is the path a line under skippedHeader names, and whether the line has the shape
// that names one. jj writes the directory, then `: `, then the name as Rust debug-quotes it,
// and that name is always quoted, so the cut is at the line's last `: "`: a directory holding
// `: ` of its own is then read whole, and the cut falls in no name, since jj writes a `"` of a
// name as `\"` and no bare `: "` stands inside one. What follows the cut is read by unquoted,
// which is the one place the quotes are checked; a name it cannot read, or one quoted around
// nothing, is no shape this can read.
func skippedPath(line string) (string, bool) {
	at := strings.LastIndex(line, `: "`)
	if at < 0 {
		return "", false
	}
	name, ok := unquoted(line[at+len(": "):])
	if !ok || name == "" {
		return "", false
	}
	return filepath.Join(line[:at], name), true
}

// named is up to maxNamedPaths of paths as a person reads them, with a count of the rest: a
// workspace holding many of them would otherwise make a message longer than Dispatch takes.
func named(paths []string) string {
	if len(paths) > maxNamedPaths {
		return fmt.Sprintf("%s, and %d more", quoted(paths[:maxNamedPaths]), len(paths)-maxNamedPaths)
	}
	return quoted(paths)
}

// warned is what jj wrote under header on its standard error: the lines after it that start
// with two spaces, up to the first line that does not, which leaves jj's Hint: block and the
// ` *` lines under it out. Each is answered with that indent taken off and nothing else, since
// a path may hold a space of its own at either end.
func warned(stderr, header string) []string {
	var lines []string
	found := false
	for line := range strings.Lines(stderr) {
		line = strings.TrimRight(line, "\r\n")
		if !found {
			found = strings.Contains(line, header)
			continue
		}
		rest, ok := strings.CutPrefix(line, "  ")
		if !ok {
			break
		}
		lines = append(lines, rest)
	}
	return lines
}

// nestedJJ is every directory under root that holds a .jj of its own, as a path relative to
// root. jj leaves such a directory out of its snapshot whole (leftOut), so the directory is
// what the carry adds, and what it adds under it is what `git add --all` would have added
// there: the nested .jj's own files as the plain files they are to git, the files beside them,
// the files head tracks there whatever an ignore rule says, and nothing else an ignore rule
// covers (carried).
//
// A .jj of any kind has its directory left out, and not a .jj directory alone: measured on
// this build on 2026-11-12, with e/.jj a regular file, h/.jj a symlink and d/.jj a directory,
// each beside a keep.txt, the snapshot's tree held none of the three keep.txt and did hold the
// file at the worktree's root. So the name of every entry is read, whatever kind of file that
// entry is, and a directory is all that is skipped over, since a directory is all that can be
// descended into.
//
// The worktree's own .jj is no nesting and is not one of them. Neither it, nor a nested one,
// nor any .git is descended into: jj's store and a nested git repository both hold more than
// the worktree does, and what a nested git repository holds is carried by neither read (edits).
// What is left is a walk of the worktree's directory entries, which hashes nothing and reads no
// file.
func nestedJJ(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		name := entry.Name()
		if name != ".jj" && name != ".git" {
			return nil
		}
		if name == ".jj" {
			holder, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			if holder != "." {
				found = append(found, holder)
			}
		}
		if !entry.IsDir() {
			return nil // a .jj or a .git that is no directory has nothing to descend into
		}
		return fs.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("the worktree %s could not be walked for the nested .jj directories jj's snapshot leaves out: %w", root, err)
	}
	return found, nil
}

// unquoted is the bytes a Rust debug-quoted string stands for, and whether text is one: jj
// writes the name of a path that is not valid UTF-8 that way (`"bad\xFD.txt"`), and those bytes
// are the name on disk. It is the one place the quotes are checked (skippedPath). Text that is
// not quoted at both ends, a bare `"` inside the quotes, a `\` that ends the text, and an escape
// this does not know are none of them what that debug format writes, so each answers false and
// the line it came from fails the workspace as a line this check cannot read (leftOut). The
// escapes that format writes are all read here: `\\`, `\"`, `\'`, `\n`, `\r`, `\t`, `\0`,
// `\xNN` and `\u{...}`.
func unquoted(text string) (string, bool) {
	if len(text) < 2 || text[0] != '"' || text[len(text)-1] != '"' {
		return "", false
	}
	body := text[1 : len(text)-1]
	var name []byte
	for i := 0; i < len(body); {
		switch {
		case body[i] == '"' || (body[i] == '\\' && i+1 == len(body)):
			return "", false
		case body[i] != '\\':
			name, i = append(name, body[i]), i+1
			continue
		}
		switch escape := body[i+1]; escape {
		case '\\', '"', '\'':
			name, i = append(name, escape), i+2
			continue
		case 'n':
			name, i = append(name, '\n'), i+2
			continue
		case 'r':
			name, i = append(name, '\r'), i+2
			continue
		case 't':
			name, i = append(name, '\t'), i+2
			continue
		case '0':
			name, i = append(name, 0), i+2
			continue
		case 'x':
			if i+4 <= len(body) {
				if b, err := strconv.ParseUint(body[i+2:i+4], 16, 8); err == nil {
					name, i = append(name, byte(b)), i+4
					continue
				}
			}
		case 'u':
			// 21 bits, because the largest rune is 0x10FFFF and 21 bits holds it: a wider parse
			// takes a value no rune can hold and hands it to a conversion that wraps it
			// negative, which utf8.ValidRune then refuses. The answer is the same either way,
			// and this way nothing out of range is converted at all.
			if braced, ok := strings.CutPrefix(body[i+2:], "{"); ok {
				if digits, after, closed := strings.Cut(braced, "}"); closed {
					if point, err := strconv.ParseUint(digits, 16, 21); err == nil && utf8.ValidRune(rune(point)) {
						name, i = utf8.AppendRune(name, rune(point)), len(body)-len(after)
						continue
					}
				}
			}
		}
		return "", false
	}
	return string(name), true
}

// emptyTree is git's tree with nothing in it.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// fields is out's lines, each trimmed, the empty ones dropped.
func fields(out string) []string {
	return strings.Fields(out)
}
