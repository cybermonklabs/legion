// Package rescue finds the issue's own work that an issue pod's workspaces hold and their remote
// lacks, and pushes it to the issue's one rescue branch on that remote, before the daemon replaces
// the pod. The pod's disk is the pod's own and goes with it (docs/kubernetes.md, "The disk dies
// with its pod"), so this is the last place that work can be read from.
//
// It runs inside the pod, in a role's shim (`rescue-unpushed`, shim.rescue), as that role and with
// that role's own GitHub credential: the shared clone's credential helper answers git from the
// role's gh files, so the daemon handles no secret of its own for it. It changes nothing in a
// workspace: no file, no index and no jj operation. In the clone it writes git objects, which
// change nothing anyone reads: the commits it makes, and the rescue branch's tip when it fetches
// that tip, which writes no ref. The one ref of the clone that changes is the remote-tracking ref
// of the rescue branch, which its push updates, as every push does.
package rescue

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// The trailers of a rescue commit's message (rescueCommit), which tell a later put-back what to
// set the new workspace to: TrailerWorkingCopy names the working-copy commit, and each
// TrailerBookmark names one bookmark of the issue's own line and its commit, as
// `<name> <commit>`. A put-back reads them with `git log --format=%(trailers:key=<key>,valueonly)`.
const (
	TrailerWorkingCopy = "Legion-Working-Copy"
	TrailerBookmark    = "Legion-Bookmark"
)

// ownLine is the jj revset of a workspace's own line: the ancestors of its working-copy commit and
// of its issue's bookmark, `legion/<KEY>`.
func ownLine(bookmark string) string {
	return `::(@ | bookmarks(exact:` + jjString(bookmark) + `))`
}

// ownHeads is the work on a workspace's own line (ownLine), reduced to its heads, less every empty
// commit with no description. Whether origin holds a head is git's question, asked of what the
// remote advertises now (unpushed), so nothing here subtracts what jj last saw remote.
//
// Nothing else jj can see is the workspace's work. A pod's shared clone keeps every commit it ever
// fetched visible (workspace-init sets git.abandon-unreachable-commits false), so jj's visible
// heads hold old merges, other trees' handoffs and the pre-rebase copies of rewritten branches,
// which origin deleted or rewrote and no role of the pod wrote: rescuing them pushed one branch for
// each, and the pod's own work was one of sixteen. A side line that a role left off its own line
// is not taken either. The worker's own push before it answers a round is what keeps such work.
//
// An empty commit with no description — jj's working-copy commit before anything is written —
// holds no work, and dropping it makes the commit under it a head. An empty commit with a
// description is kept: an empty merge of two pushed commits is the one record of that merge, as it
// is when a worker merges, leaves the issue's bookmark behind and its push says "Nothing changed".
// refs/jj/keep is not read: it holds the obsolete predecessor of every rewritten commit.
func ownHeads(bookmark string) string {
	return `heads(` + ownLine(bookmark) + ` ~ (empty() & description(exact:"")))`
}

// jjString is name as a jj string literal: quoted, with `\` and `"` escaped, so a bookmark name is
// the one name it is in a revset.
func jjString(name string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
}

// gitPins is configuration every git this package starts reads last: no file-system monitor runs
// and no replacement object (`git replace`, a graft among them) stands in for a commit, so a
// commit grafted to have a role's work as its parent never makes that work look held, and the
// push sends the commits as they are.
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
var gitPins = []string{"--no-replace-objects", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// Target is the issue's one rescue branch, `legion/<KEY>-rescue`, which the check pushes each
// workspace's own work to (Request.Target) and which only moves forward. It is no issue's branch
// (claim.BranchIssue), so nothing the daemon keeps follows it.
func Target(issue string) string {
	return claim.IssueBranch(issue) + "-rescue"
}

// Request is one check of every workspace on a pod's disk.
type Request struct {
	// StateDir is the disk's root: workspaces under <StateDir>/workspaces/<owner>/<repo>/<issue>,
	// and the shared clone each is a jj workspace of under <StateDir>/repos/github.com/<owner>/<repo>.
	StateDir string
	// Branch is the per-pod prefix an older shim numbers its branches from,
	// `legion/<KEY>-rescue-<pod>`. The check reads it only when Target is empty, which is a request
	// of an older daemon: it then pushes to that name itself, with no number, by the same
	// forward-only rule. An older daemon lists whatever branch the check reports, so its message
	// names the right branch.
	Branch string
	// Target is the issue's one rescue branch, `legion/<KEY>-rescue`, which the check pushes each
	// workspace's own work to and which only moves forward. The daemon names it beside Branch
	// (shimwire.RescueUnpushed), and an older daemon names none.
	Target string
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
			branch := runtime.RescuedBranch{Name: b.Name, Commit: b.Commit, Previous: b.Previous, Pushed: b.Pushed}
			if len(b.Commits) > 0 {
				branch.Commits = make([]runtime.RescuedCommit, len(b.Commits))
				for j, c := range b.Commits {
					branch.Commits[j] = runtime.RescuedCommit(c)
				}
			}
			report[i].Branches = append(report[i].Branches, branch)
		}
	}
	return report
}

// Run checks every workspace on the disk, in path order, and pushes the issue's own work each
// holds that its remote lacks. Every workspace gets one entry. A disk with no workspace yields
// none, which is no work to lose.
func Run(ctx context.Context, req Request) []shimwire.RescueWorkspace {
	dirs, err := workspaces(req.StateDir)
	if err != nil {
		return []shimwire.RescueWorkspace{{Dir: req.StateDir, Status: Failed, Error: err.Error()}}
	}
	results := make([]shimwire.RescueWorkspace, 0, len(dirs))
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
		result := check(one, req, dir)
		if one.Err() != nil && result.Status == Failed {
			result.Error = fmt.Sprintf("the check ran out its %s: %s", bound.Round(time.Second), result.Error)
		}
		cancel()
		results = append(results, result)
	}
	return results
}

// workspaces is every workspace directory on the disk: a real directory three levels below
// workspaces/, never a symlink, never a file an agent left beside its workspace, and never a
// directory workspace.Remove renamed aside to delete (workspace.RemovingSuffix), whose removal
// already let its work go.
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
		if strings.HasSuffix(match, workspace.RemovingSuffix) {
			continue
		}
		if info, err := os.Lstat(match); err == nil && info.IsDir() {
			dirs = append(dirs, match)
		}
	}
	slices.Sort(dirs)
	return dirs, nil
}

// pushBranch is the branch the check pushes dir's work to, and the issue whose workspace dir is:
// Target, or Branch when Target is empty (Request). The branch must be that issue's:
// `legion/<KEY>-rescue`, or a per-pod `legion/<KEY>-rescue-<pod>`. Any other branch is an error
// that names both keys, and so is a directory whose name is no issue key. A rescue branch must
// never hold the work of another issue. Provisioning lays out one workspace for each issue key
// (workspace.Location), so such a directory is a fault to show, not a case to serve.
func pushBranch(req Request, dir string) (branch, issue string, err error) {
	branch = cmp.Or(req.Target, req.Branch)
	owner := "no issue's"
	if base, _, ok := strings.Cut(branch, "-rescue"); ok {
		if key, ok := claim.BranchIssue(base); ok {
			owner = "issue " + key + "'s"
		}
	}
	issue = strings.ToUpper(filepath.Base(dir))
	if !claim.IsIssueKey(issue) {
		return "", "", fmt.Errorf("the workspace directory %s names no issue key, and the rescue branch %q is %s: nothing of it is pushed", dir, branch, owner)
	}
	one := Target(issue)
	if branch != one && (!strings.HasPrefix(branch, one+"-") || len(branch) == len(one)+1) {
		return "", "", fmt.Errorf("the workspace %s is issue %s's, and the rescue branch %q is %s: its work is not pushed under the name of another issue", dir, issue, branch, owner)
	}
	return branch, issue, nil
}

// check is one workspace's check and push. The workspace's repository is the one its git names
// (its common git directory), so a directory under workspaces/ that is no git worktree is an error
// rather than a pass: whatever it holds, nothing can push it.
//
// The branch is the issue's (pushBranch), and the work is the issue's own: the heads of the
// workspace's own line in jj (ownHeads), the issue's git branch, the workspace's git HEAD, and the
// workspace's files that no commit holds, as one commit on that HEAD (edits). Nothing else on the
// disk is pushed: no other local branch or bookmark, no visible head off that line, no tag, no
// stash entry and no other worktree. A candidate that is jj's empty working-copy commit stands for
// the commit under it (holdingWork).
//
// What the remote holds is read from the remote alone: a remote-tracking ref is only as fresh as
// the pod's last fetch, so a commit of the workspace's own that only a stale one names — the
// issue's branch since force-pushed or deleted on the remote — is a commit the remote may no
// longer have, and is pushed.
//
// The branch only moves forward. Its tip on the remote is held like every commit the remote
// advertises, and when there is work to push and the clone lacks that tip, the tip is fetched
// first (fetchTip), so work the branch holds already is not pushed again. What is left goes in
// one rescue commit at the branch's new tip (rescueCommit), whose parents are the heads of that
// work and the old tip when no head holds it, so the push needs no force.
//
// With nothing left to push, the branch is still reported when the clone holds its tip and the
// workspace's own work holds commits that no other ref of the remote holds: an earlier check of
// this same pod pushed them, in a round whose replacement did not happen then, and the issue must
// still be told of them once the pod goes. A tip the clone lacks came from another pod, whose
// replacement told the issue of it.
func check(ctx context.Context, req Request, dir string) shimwire.RescueWorkspace {
	result := shimwire.RescueWorkspace{Dir: dir}
	fail := func(err error) shimwire.RescueWorkspace {
		result.Status, result.Branches, result.Error = Failed, nil, err.Error()
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
	branch, issue, err := pushBranch(req, dir)
	if err != nil {
		return fail(err)
	}
	own := claim.IssueBranch(issue)

	listed, err := run.git(clone, nil, "ls-remote", "origin")
	if err != nil {
		return fail(err)
	}
	remoteBranches := map[string]string{}
	var advertised []string
	for line := range strings.Lines(listed) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		advertised = append(advertised, sha)
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			remoteBranches[name] = sha
		}
	}
	previous := remoteBranches[branch]
	held, err := run.objects(clone, advertised, "commit", "tag")
	if err != nil {
		return fail(err)
	}

	var candidates []string
	info, err := os.Lstat(filepath.Join(dir, ".jj"))
	jj := err == nil && info.IsDir()
	if jj {
		heads, err := run.jj(dir, "log", "--no-graph", "-r", ownHeads(own), "-T", `commit_id ++ "\n"`)
		if err != nil {
			return fail(err)
		}
		candidates = append(candidates, fields(heads)...)
	}
	ownBranch, err := run.git(clone, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+own+"^{commit}")
	if err != nil {
		ownBranch = ""
	} else {
		candidates = append(candidates, ownBranch)
	}
	head, err := run.git(dir, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		head = "" // an unborn HEAD: a worktree with no git-visible commit yet
	} else {
		candidates = append(candidates, head)
	}
	edits, err := run.edits(dir, head)
	if err != nil {
		return fail(err)
	}
	if edits != "" {
		candidates = append(candidates, edits)
	}

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
	if len(tips) > 0 && previous != "" && !slices.Contains(held, previous) {
		if err := run.fetchTip(clone, branch, previous); err != nil {
			return fail(err)
		}
		if held, err = run.objects(clone, advertised, "commit", "tag"); err != nil {
			return fail(err)
		}
		if tips, err = run.unpushed(clone, candidates, held); err != nil {
			return fail(err)
		}
	}

	// An edits commit whose tree is another tip's holds nothing that tip does not: the working
	// copy's files are that commit's, as jj last snapshotted them.
	sameTree := false
	if at := slices.Index(tips, edits); at >= 0 && len(tips) > 1 {
		args := []string{"rev-parse"}
		for _, tip := range tips {
			args = append(args, tip+"^{tree}")
		}
		out, err := run.git(clone, nil, args...)
		if err != nil {
			return fail(err)
		}
		trees := fields(out)
		if len(trees) != len(tips) {
			return fail(fmt.Errorf("git named %d trees for the %d tips %v", len(trees), len(tips), tips))
		}
		for i := range tips {
			if i != at && trees[i] == trees[at] {
				sameTree = true
				tips = slices.Delete(tips, at, at+1)
				break
			}
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

	if len(tips) == 0 {
		result.Status = Clean
		if previous == "" || !slices.Contains(held, previous) {
			return result
		}
		var others []string
		for _, h := range held {
			if h != previous {
				others = append(others, h)
			}
		}
		only, err := run.revList(clone, candidates, others)
		if err != nil {
			return fail(err)
		}
		if len(only) == 0 {
			return result
		}
		commits, err := run.subjects(clone, only)
		if err != nil {
			return fail(err)
		}
		result.Status = Rescued
		result.Branches = []shimwire.RescueBranch{{Name: branch, Commit: previous, Commits: commits}}
		return result
	}

	// The working-copy commit is the commit that holds the working copy's content: the edits commit
	// when it is kept, else jj's @ as holdingWork maps it, else the workspace's git HEAD. Like every
	// commit a trailer names, it must be one the push carries or the remote holds (findable).
	workingCopy := head
	switch {
	case edits != "" && !sameTree:
		workingCopy = edits
	case jj:
		at, err := run.jj(dir, "log", "--no-graph", "-r", "@", "-T", "commit_id")
		if err != nil {
			return fail(err)
		}
		mapped, err := run.holdingWork(clone, []string{at})
		if err != nil {
			return fail(err)
		}
		if len(mapped) > 0 {
			workingCopy = mapped[0]
		}
	}
	// The local bookmarks of the workspace's own line: jj's listing, or, in a workspace with no jj,
	// the issue's git branch alone.
	var local []bookmark
	switch {
	case jj:
		out, err := run.jj(dir, "bookmark", "list", "-r", ownLine(own), "-T", bookmarkTemplate)
		if err != nil {
			return fail(err)
		}
		for line := range strings.Lines(out) {
			if name, commit, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
				local = append(local, bookmark{name: name, commit: commit})
			}
		}
	case ownBranch != "":
		local = []bookmark{{name: own, commit: ownBranch}}
	}
	bookmarks, err := run.bookmarks(clone, own, local, remoteBranches)
	if err != nil {
		return fail(err)
	}
	parents, err := run.parents(clone, tips, workingCopy, previous)
	if err != nil {
		return fail(err)
	}
	workingCopy, bookmarks, err = run.findable(clone, parents, held, workingCopy, bookmarks)
	if err != nil {
		return fail(err)
	}
	commit, err := run.rescueCommit(clone, dir, parents, workingCopy, bookmarks)
	if err != nil {
		return fail(err)
	}
	added, err := run.revList(clone, []string{commit}, held)
	if err != nil {
		return fail(err)
	}
	commits, err := run.subjects(clone, added)
	if err != nil {
		return fail(err)
	}
	pushed := shimwire.RescueBranch{Name: branch, Commit: commit, Previous: previous, Pushed: true, Commits: commits}
	if err := guardPush(ctx, req, run, clone, pushed); err != nil {
		return fail(err)
	}
	refspec := commit + ":refs/heads/" + branch
	if _, err := run.git(clone, []string{"GIT_TERMINAL_PROMPT=0"}, "push", "--quiet", "origin", refspec); err != nil {
		return fail(fmt.Errorf("push %s: %w", refspec, err))
	}
	result.Status, result.Branches = Rescued, []shimwire.RescueBranch{pushed}
	return result
}

// noSHA is git's "this ref is not there", which a pre-push line carries as the remote side of a
// ref the push creates. The branch this check pushes is new, and then its remote side is noSHA, or
// it moves forward from the tip the remote advertised.
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
// four minutes a workspace's bound once was left 90 s for.
const readsBeforeThePush = 90 * time.Second

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
// A workspace's bound holds three things in a pod, and each is measured: the reads that find
// the unpushed work (readsBeforeThePush), the guard's own run of the shared check (CheckRun),
// and what that guard leaves the push, which in a pod runs the same check a second time in the
// pre-push hook (GuardReserve). 90 + 120 + 150 is 360 s, so a workspace's bound is six minutes
// and the whole disk's, keeping wholeDiskMargin over it, nine. A bound that fits one run of the
// check and not two ends inside the hook's run and loses the dying pod's work to a timeout
// rather than refusing it, which is the failure these numbers exist to prevent.
const (
	WorkspaceTimeout = readsBeforeThePush + CheckRun + GuardReserve
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
// The update is the refspec the push is about to send, as git would hand it to a pre-push hook:
// the local sha, the branch on the remote, and what the remote holds there, which is the tip the
// branch moves forward from (branch.Previous), or noSHA for a branch the push creates.
//
// Env is the role's own environment, the one the push itself runs in, which is what names the
// credential the check reads the remote with.
func guardPush(ctx context.Context, req Request, run runner, clone string, branch shimwire.RescueBranch) error {
	what := leakguard.PushName("origin")
	url, err := run.git(clone, nil, "remote", "get-url", "origin")
	if err == nil && url == "" {
		err = errors.New("git named no URL for origin")
	}
	if err != nil {
		return &leakguard.SetupFailure{What: what, Why: "the URL of the remote this push is made to could not be read", Err: err}
	}
	updates := []leakguard.Update{{
		LocalRef: branch.Commit, LocalSHA: branch.Commit,
		RemoteRef: "refs/heads/" + branch.Name, RemoteSHA: cmp.Or(branch.Previous, noSHA),
	}}
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

// runner runs git and jj under one workspace's bound, in the role's environment.
type runner struct {
	ctx context.Context
	env []string
}

func (r runner) git(dir string, env []string, args ...string) (string, error) {
	return r.command(dir, env, nil, "git", append(slices.Clone(gitPins), args...)...)
}

// jj runs a read of jj's view that writes nothing: no snapshot of the working copy, and no new
// operation, since it reads at the operation the store is at.
func (r runner) jj(dir string, args ...string) (string, error) {
	return r.command(dir, nil, nil, "jj", append([]string{"--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never", "-R", dir}, args...)...)
}

// command runs name in dir. Its environment is the role's with env after it: Go's exec takes the
// last value of a name, so env outranks the role's.
func (r runner) command(dir string, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(r.ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(r.env), env...)
	cmd.Stdin = stdin
	procgroup.Configure(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := procgroup.Err(cmd.Run()); err != nil {
		return "", fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return "", fmt.Errorf("%s %s: exit %d: %s", name, strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
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

// revList is every commit an ancestor of one of from, from included, that is no ancestor of any
// of not, in rev-list's order.
func (r runner) revList(clone string, from, not []string) ([]string, error) {
	if len(from) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, c := range from {
		input.WriteString(c + "\n")
	}
	for _, h := range not {
		input.WriteString("^" + h + "\n")
	}
	out, err := r.command(clone, nil, strings.NewReader(input.String()), "git", append(slices.Clone(gitPins), "rev-list", "--stdin")...)
	if err != nil {
		return nil, err
	}
	return fields(out), nil
}

// unpushed is the candidates no held commit contains, each once, in the order given.
func (r runner) unpushed(clone string, candidates, held []string) ([]string, error) {
	out, err := r.revList(clone, candidates, held)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]bool, len(out))
	for _, sha := range out {
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

// holdingWork is candidates, each once, each replaced by the commit that holds its work
// (standsFor).
func (r runner) holdingWork(clone string, candidates []string) ([]string, error) {
	stand, err := r.standsFor(clone, candidates)
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, c := range candidates {
		if s := stand[c]; !slices.Contains(kept, s) {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

// standsFor maps each of commits to the commit that holds its work: itself, or, for a commit that
// has no message and the tree of its one parent, what that parent stands for, as often as it
// takes. That commit is jj's working-copy commit before anything is written, as ownHeads drops it
// on jj's side, and a git ref can name it: jj exports a bookmark set on it, as provisioning sets
// the issue's on the working copy of a workspace it starts at the default branch. A merge stands
// for itself whatever it holds.
func (r runner) standsFor(clone string, commits []string) (map[string]string, error) {
	stand := make(map[string]string, len(commits))
	for _, c := range commits {
		stand[c] = c
	}
	pending := slices.Sorted(maps.Keys(stand))
	for len(pending) > 0 {
		listed, err := r.git(clone, nil, append([]string{"log", "--no-walk=unsorted", "--format=%H %T %P%x09%s"}, pending...)...)
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
		for c, s := range stand {
			if p, ok := replaced[s]; ok {
				stand[c] = p
			}
		}
		// Only a parent that took a commit's place is read again: every other commit was read and
		// holds its own work.
		next := map[string]bool{}
		for _, p := range replaced {
			next[p] = true
		}
		pending = slices.Sorted(maps.Keys(next))
	}
	return stand, nil
}

// fetchTip fetches tip, the commit the remote's branch holds, into the clone, which lacks it. It
// writes no ref: no remote-tracking ref (`--refmap=` with no destination), no tag and no
// FETCH_HEAD. A fetch that fails, or after which the clone still lacks tip, is the workspace's
// error, and nothing is pushed. It has no bound of its own: it is one of the reads before the push,
// under the workspace's bound (readsBeforeThePush).
func (r runner) fetchTip(clone, branch, tip string) error {
	if _, err := r.git(clone, []string{"GIT_TERMINAL_PROMPT=0"},
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--refmap=", "origin", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("fetch the tip %s of %s, which the clone lacks: %w", tip, branch, err)
	}
	if _, err := r.git(clone, nil, "cat-file", "-e", tip+"^{commit}"); err != nil {
		return fmt.Errorf("the clone lacks the tip %s of %s after it fetched the branch: %w", tip, branch, err)
	}
	return nil
}

// bookmark is one bookmark a rescue commit's trailers name, and a commit it is on.
type bookmark struct{ name, commit string }

// bookmarkTemplate is jj's listing of each local bookmark as `<name> <commit>`. A conflicted
// bookmark is on each of its added targets, so it gets one line for each of them.
const bookmarkTemplate = `if(remote, "", if(conflict, ` +
	`added_targets.map(|c| name ++ " " ++ c.commit_id() ++ "\n").join(""), ` +
	`if(normal_target, name ++ " " ++ normal_target.commit_id() ++ "\n")))`

// bookmarks is the bookmarks of listed — the local bookmarks of the workspace's own line
// (ownLine) — that a rescue commit's trailers name, in name order: the issue's own, own, always,
// and each other less each that the remote advertises under the same name at the same commit. A
// put-back that set such a bookmark would only move it to where the remote has it, which can be
// behind what the new pod fetched, and `main` is one of them. A bookmark ahead of the remote, or
// one the remote does not have, is the issue's work.
//
// Each bookmark is named at the commit that holds its work (standsFor), as the working copy is,
// and only then compared with the remote. `legion/<KEY>` on jj's empty working-copy commit with no
// description, where provisioning sets it and where a read that takes no snapshot still sees it
// while the role's edits are in no commit, is so named at the commit under it: the empty commit
// is in no push and on no remote, and a put-back could not find it.
func (r runner) bookmarks(clone, own string, listed []bookmark, remoteBranches map[string]string) ([]bookmark, error) {
	commits := make([]string, len(listed))
	for i, b := range listed {
		commits[i] = b.commit
	}
	stand, err := r.standsFor(clone, commits)
	if err != nil {
		return nil, err
	}
	var named []bookmark
	for _, b := range listed {
		b.commit = stand[b.commit]
		if b.name != own && remoteBranches[b.name] == b.commit {
			continue
		}
		named = append(named, b)
	}
	slices.SortFunc(named, func(a, b bookmark) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.commit, b.commit))
	})
	// Two added targets of a conflicted bookmark can stand for the same commit.
	return slices.Compact(named), nil
}

// findable is workingCopy and bookmarks held to the rule every trailer keeps: a put-back must find
// each commit a trailer names, so the commit is an ancestor of one of parents or of a held commit,
// which the push carries or the remote holds. A working-copy commit that is neither is jj's @ as
// an empty merge with no description: holdingWork keeps a merge, and ownHeads drops the empty
// commit, so the merge is no tip. The rescue commit stands for that merge, since its parents are
// the merge's heads, and the working copy is named at its first parent. A bookmark that is neither
// is left out.
func (r runner) findable(clone string, parents, held []string, workingCopy string, bookmarks []bookmark) (string, []bookmark, error) {
	var named []string
	if workingCopy != "" {
		named = append(named, workingCopy)
	}
	for _, b := range bookmarks {
		named = append(named, b.commit)
	}
	lost, err := r.unpushed(clone, named, slices.Concat(parents, held))
	if err != nil {
		return "", nil, err
	}
	if slices.Contains(lost, workingCopy) {
		workingCopy = parents[0]
	}
	kept := slices.DeleteFunc(slices.Clone(bookmarks), func(b bookmark) bool { return slices.Contains(lost, b.commit) })
	return workingCopy, kept, nil
}

// parents is the rescue commit's parents: the tips, sorted, with the one that holds the working-copy
// commit first, else the first in sorted order; and then previous, the branch's old tip, when no
// tip holds it. No tip is held by previous, so `merge-base --independent` keeps previous exactly
// when no tip holds it.
func (r runner) parents(clone string, tips []string, workingCopy, previous string) ([]string, error) {
	parents := slices.Clone(tips)
	if workingCopy != "" {
		for i, tip := range parents {
			outside, err := r.git(clone, nil, "rev-list", "--count", workingCopy, "^"+tip)
			if err != nil {
				return nil, err
			}
			if outside == "0" {
				parents = append([]string{tip}, slices.Delete(parents, i, i+1)...)
				break
			}
		}
	}
	if previous != "" {
		independent, err := r.git(clone, nil, append([]string{"merge-base", "--independent", previous}, tips...)...)
		if err != nil {
			return nil, err
		}
		if slices.Contains(fields(independent), previous) {
			parents = append(parents, previous)
		}
	}
	return parents, nil
}

// rescueCommit is the commit at the rescue branch's new tip. Its parents are parents, and its tree
// is the first parent's, so it adds no content that no parent holds: the guard's read of what the
// push adds finds nothing new in it. Its message is `Rescue of <dir> before its pod was replaced`,
// then the trailers, TrailerWorkingCopy and then a TrailerBookmark for each of bookmarks. Its
// identity is fixed and its date is the newest committer date of its parents (fixedIdentity), so
// a later check of the same disk makes the same commit.
func (r runner) rescueCommit(clone, dir string, parents []string, workingCopy string, bookmarks []bookmark) (string, error) {
	tree, err := r.git(clone, nil, "rev-parse", parents[0]+"^{tree}")
	if err != nil {
		return "", err
	}
	date, err := r.newestDate(clone, parents...)
	if err != nil {
		return "", err
	}
	var trailers []string
	if workingCopy != "" {
		trailers = append(trailers, TrailerWorkingCopy+": "+workingCopy)
	}
	for _, b := range bookmarks {
		trailers = append(trailers, TrailerBookmark+": "+b.name+" "+b.commit)
	}
	message := "Rescue of " + dir + " before its pod was replaced"
	if len(trailers) > 0 {
		message += "\n\n" + strings.Join(trailers, "\n")
	}
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return r.git(clone, fixedIdentity(date), append(args, "-m", message)...)
}

// The author and committer of every commit this check makes.
const (
	rescuerName  = "Legion rescue"
	rescuerEmail = "rescue@invalid"
)

// noDate is the date of a commit this check makes with no parent: git's first second, in UTC.
const noDate = "@0 +0000"

// fixedIdentity is the environment a commit this check makes is written in: one identity, and date
// for both of its dates. It goes in the commit-tree call's own environment, which runner.command
// puts after the role's. Every role's environment sets the GIT_AUTHOR_* and GIT_COMMITTER_* names
// and emails of its own (runtime.GitIdentity.Env), and those outrank `-c user.*`, so a commit made
// through one role would differ from the same commit made through another. With all six fixed, a
// second check of the same disk, through any role, makes the same commits, finds them on the
// branch, and pushes nothing.
func fixedIdentity(date string) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + rescuerName, "GIT_AUTHOR_EMAIL=" + rescuerEmail, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + rescuerName, "GIT_COMMITTER_EMAIL=" + rescuerEmail, "GIT_COMMITTER_DATE=" + date,
	}
}

// newestDate is the newest committer date of commits, as git reads a date back (`@<seconds>
// <zone>`), or noDate when there are none.
func (r runner) newestDate(dir string, commits ...string) (string, error) {
	if len(commits) == 0 {
		return noDate, nil
	}
	out, err := r.git(dir, nil, append([]string{"log", "--no-walk=unsorted", "--format=%ct %cz"}, commits...)...)
	if err != nil {
		return "", err
	}
	date, newest, found := noDate, int64(0), false
	for line := range strings.Lines(out) {
		seconds, zone, _ := strings.Cut(strings.TrimSpace(line), " ")
		at, err := strconv.ParseInt(seconds, 10, 64)
		if err != nil {
			return "", fmt.Errorf("git printed %q as a committer date: %w", line, err)
		}
		if !found || at > newest {
			date, newest, found = "@"+seconds+" "+zone, at, true
		}
	}
	return date, nil
}

// subjectLimit is the most bytes of a commit's subject a report carries (shimwire.RescueCommit).
const subjectLimit = 200

// subjects is each of commits with its subject, in the order given, cut at subjectLimit on a rune
// boundary. None is none: git log with no commit would read HEAD.
func (r runner) subjects(clone string, commits []string) ([]shimwire.RescueCommit, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	out, err := r.command(clone, nil, strings.NewReader(strings.Join(commits, "\n")+"\n"), "git",
		append(slices.Clone(gitPins), "log", "--no-walk=unsorted", "--stdin", "--format=%H%x09%s")...)
	if err != nil {
		return nil, err
	}
	listed := make([]shimwire.RescueCommit, 0, len(commits))
	for line := range strings.Lines(out) {
		id, subject, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		if len(subject) > subjectLimit {
			cut := subjectLimit
			for cut > 0 && !utf8.RuneStart(subject[cut]) {
				cut--
			}
			subject = subject[:cut]
		}
		listed = append(listed, shimwire.RescueCommit{ID: id, Subject: subject})
	}
	return listed, nil
}

// edits is one commit holding the workspace's files as they stand, on top of head, when they
// differ from head's tree; "" when they do not. It is built in an index of its own, from a copy of
// the workspace's index where it has one, so neither the workspace's index nor any ref moves. A
// file .gitignore ignores is left out, as a role's own push would leave it. Its identity is fixed
// and its date is head's committer date (fixedIdentity), so the same files on the same head make
// the same commit at every check.
func (r runner) edits(dir, head string) (string, error) {
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
	tree, err := r.git(dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", "Edits left in the workspace " + dir + " when its pod was replaced"}
	date := noDate
	if head != "" {
		was, err := r.git(dir, nil, "rev-parse", head+"^{tree}")
		if err != nil {
			return "", err
		}
		if was == tree {
			return "", nil
		}
		if date, err = r.newestDate(dir, head); err != nil {
			return "", err
		}
		args = append(args, "-p", head)
	} else if tree == emptyTree {
		return "", nil
	}
	return r.git(dir, fixedIdentity(date), args...)
}

// emptyTree is git's tree with nothing in it.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// fields is out's lines, each trimmed, the empty ones dropped.
func fields(out string) []string {
	return strings.Fields(out)
}
