package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"golang.org/x/sys/unix"
)

// Provision updates an existing working copy, obtains the shared clone through a temporary
// sibling, protects unreachable worker commits before every fetch, resolves a bookmark before
// adding, and leaves pane credentials on the clone. The clone and fetch reach the repository
// through request.Source: GitHub for tmux, and the cache-plus-GitHub issue-pod source for
// Kubernetes. An issue with no branch starts at request.DefaultBranch, which the request must name.
// Every command on the shared clone runs under request.StoreLock; only the checkout (`jj sparse
// reset`), which writes the issue's own working copy and nothing of the clone, runs with the lock
// let go, holding one of request.CheckoutSlot's places instead.
func Provision(ctx context.Context, run Runner, request Request) (Workspace, error) {
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return Workspace{}, err
	}
	if request.CredentialHelper == "" {
		return Workspace{}, fmt.Errorf("workspace credential helper is required")
	}
	if request.Log == nil {
		return Workspace{}, errors.New("workspace request names no log")
	}
	if request.DefaultBranch == "" {
		return Workspace{}, errors.New("workspace default branch is required")
	}
	workspace.DefaultBranch = request.DefaultBranch
	if request.Source == nil {
		return Workspace{}, errors.New("workspace request names no repository source")
	}
	if err := request.Source.check(request.Repo); err != nil {
		return Workspace{}, err
	}

	store := &storeLock{locker: request.StoreLock}
	store.lock()
	defer store.unlock()
	exists, err := pathExists(workspace.Dir)
	if err != nil {
		return Workspace{}, err
	}
	if exists {
		if _, err := RunCheckedIn(ctx, run, workspace, []string{"jj", "workspace", "update-stale"}); err != nil {
			return Workspace{}, err
		}
	}
	source, err := request.Source.open(request.Repo, workspace.Bookmark, request.DefaultBranch)
	if err != nil {
		return Workspace{}, err
	}
	defer func() {
		_ = source.remove()
	}()
	if err := ensureRepoClone(ctx, run, clock.Real(), workspace.Clone, GitHubURL(request.Repo), source); err != nil {
		return Workspace{}, err
	}
	if err := ensureFetchConfiguration(ctx, run, workspace.Clone, source); err != nil {
		return Workspace{}, err
	}
	if err := source.remove(); err != nil {
		return Workspace{}, err
	}

	if exists {
		if err := restoreGitWorktree(ctx, run, workspace, request.Log); err != nil {
			return Workspace{}, err
		}
	} else if err := createWorkspace(ctx, run, workspace, request.Log); err != nil {
		return Workspace{}, err
	}
	checkout := !exists
	if exists {
		// A workspace whose checkout never finished — registered with no files, then the checkout
		// failed or the daemon stopped — is checked out now, or its pane would start in an empty
		// working copy.
		whole, err := CheckedOut(ctx, run, workspace)
		if err != nil {
			return Workspace{}, err
		}
		checkout = !whole
	}
	// Every workspace provisioning touches has its git worktree entry locked, one added before
	// provisioning locked any included, so a bare `git worktree prune` that cannot see it skips it.
	if err := LockGitWorktree(workspace.Clone, workspace.Dir); err != nil {
		return Workspace{}, err
	}
	if err := configureRepositoryCredential(ctx, run, workspace.Clone, request.CredentialHelper); err != nil {
		return Workspace{}, err
	}
	// Every provision runs these, so a clone made before a setting existed takes it at its next
	// provision.
	if err := configureCloneReads(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	if source.issue != nil {
		if err := configureIssueCloneMaintenance(ctx, run, workspace.Clone); err != nil {
			return Workspace{}, err
		}
	}
	if err := configurePackWindows(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	if err := configureCloneDefaultRepository(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	if err := removeRepositoryIdentity(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	// Keeps every future `codegraph init`/`index` in this workspace — the host warm-up's, and a
	// worker's own `codegraph` tool call inside a pod, which this function also provisions
	// (cmd/legion workspace-init) — from ever getting its `.codegraph/` tracked. CodeGraph's own
	// generated `.codegraph/.gitignore` is `*` then `!.gitignore`, so that one file stays visible
	// to git; without this, jj's default auto-track snapshots it into the workspace's own change
	// (confirmed empirically: `jj status` under an empty HOME showed `.codegraph/.gitignore`
	// newly added after a bare `codegraph init`). `.git/info/exclude` is local to this shared
	// clone, read by every workspace of it (git worktrees share one `info/exclude` through their
	// common git directory) and by jj (confirmed the same way: with the line added first, the
	// same `codegraph init` left `jj status` clean), so this needs no global git configuration
	// anywhere a workspace of this clone is used.
	if err := excludeCodegraphDirectory(workspace.Clone); err != nil {
		return Workspace{}, err
	}
	// The checkout writes the issue's working copy, every file of the tree, into the workspace the
	// registration made with no files: past the shared clone's own clone, whose size-following
	// steps share one CloneTimeout of their own (ensureRepoClone, above), it is the one step whose
	// time follows the repository's size, and it writes nothing of the shared clone's, so it runs
	// with the store lock let go, beside other issues' checkouts, holding one of the places the
	// daemon bounds checkouts by.
	//
	// It is also the one command past those steps that no change of this repository's makes fit
	// CommandTimeout's five minutes: one write of the largest repository a project then worked
	// in took nine minutes in an issue pod (CheckoutTimeout), and the runner killing it there
	// failed the whole provision and spent one of the claim's three launches. So this command
	// alone takes CheckoutTimeout (request.checkoutBound), and every provisioning command but it
	// and those steps keeps the runner's slow-command budget.
	store.unlock()
	if checkout {
		release, err := request.checkoutPlace(ctx)
		if err != nil {
			return Workspace{}, fmt.Errorf("check out workspace %s: wait for a checkout's place: %w", workspace.Dir, err)
		}
		_, err = runCheckedInWithin(ctx, run, workspace, request.checkoutBound(), []string{"jj", "sparse", "reset"})
		release()
		if err != nil {
			return Workspace{}, fmt.Errorf("check out workspace %s: %w", workspace.Dir, err)
		}
	}
	return workspace, nil
}

// CheckedOut is whether workspace's checkout finished: it holds at least one sparse pattern — the
// whole tree (`.`) after Provision's checkout, or whatever narrower set an agent chose since,
// which a launch must not undo. Provision registers a workspace with no pattern and no file (`jj
// workspace add --sparse-patterns empty`) and checks it out after (`jj sparse reset`), so a
// workspace whose checkout failed or was cut short holds no pattern until a provision checks it
// out. A clone itself intentionally has no sparse pattern, so CheckedOut refuses it and directories
// under its clone root rather than treating it as an unfinished workspace.
func CheckedOut(ctx context.Context, run Runner, workspace Workspace) (bool, error) {
	// A clone sits at <clone root>/<owner>/<name> (Location), so the root it sits in is its
	// grandparent, and the clone itself is under that root. The clone's own case is therefore
	// first, for the refusal that names what the directory is. Both come before jj runs at all.
	if workspace.Clone != "" {
		cloneRoot := filepath.Dir(filepath.Dir(workspace.Clone))
		switch {
		case workspace.Dir == workspace.Clone:
			return false, fmt.Errorf("CheckedOut cannot answer for %s: that directory is the shared clone itself, not a workspace of one."+whyACloneHasNoAnswer, workspace.Dir)
		case underDirectory(cloneRoot, workspace.Dir):
			return false, fmt.Errorf("CheckedOut cannot answer for %s: that directory is under the clone root %s, which holds a shared clone of each repository."+whyACloneHasNoAnswer, workspace.Dir, cloneRoot)
		}
	}
	listed, err := RunCheckedIn(ctx, run, workspace, []string{"jj", "sparse", "list", "--ignore-working-copy", "--color=never"})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(listed.Stdout) != "", nil
}

// whyACloneHasNoAnswer is the tail of both CheckedOut refusals.
const whyACloneHasNoAnswer = " A clone holds no sparse pattern by design, because it carries no working tree. An answer of false here reads as an unfinished checkout, and the caller then writes the whole tree into the clone"

// underDirectory is whether dir is root or lies below it, compared one path segment at a time
// (filepath.Rel, then filepath.IsLocal, as cmd/legion's sessions.go and runtime/sandbox's
// manifest.go compare a path with its root). A string prefix would call /a/bc a child of /a/b.
func underDirectory(root, dir string) bool {
	relative, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return filepath.IsLocal(relative)
}

// excludeLimit bounds how much of the shared clone's .git/info/exclude excludeCodegraphDirectory
// reads: an exclude file a person or a tool wrote can pass gitdirLimit's 4 KiB, and one past a
// MiB is no list of patterns anyone keeps, so it is refused rather than read whole into memory.
const excludeLimit = 1 << 20

// excludeCodegraphDirectory appends ".codegraph/" to cloneDir's ".git/info/exclude" unless a
// line already matches it exactly, so repeated provisioning of the same clone writes it once. A
// missing .git or info directory is made at 0700, and a missing exclude file at 0600.
//
// The file is on the issue's volume, which every role of the pod writes, and the call runs under
// whatever Provision's caller holds: the tmux daemon's outbox, with the repository's store lock
// held, which every later provision and removal of the repository waits on. So it is opened once,
// read and appended to through that one descriptor, and opened as rolefile.OpenRegular opens a file
// a role can write: a symlink there is refused rather than followed, which would grow whatever file
// the link names, and a fifo or a device is refused rather than waited on, in its open or its read.
// A directory, or a file larger than excludeLimit, is refused too, each naming the file. The clone
// and the two directories between it and the file are opened the same way, without following a
// link (rolefile.OpenDirectory), each under the descriptor of the one above it, so a link a role
// leaves at .git or at info is refused as one at the file is, and nothing below the clone is
// reached by a path a role could swap between two opens.
func excludeCodegraphDirectory(cloneDir string) error {
	excludePath := filepath.Join(cloneDir, ".git", "info", "exclude")
	dir, err := rolefile.OpenDirectory(unix.AT_FDCWD, cloneDir, cloneDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	path := cloneDir
	for _, name := range []string{".git", "info"} {
		path = filepath.Join(path, name)
		next, err := rolefile.OpenDirectory(dir, name, path)
		if errors.Is(err, fs.ErrNotExist) {
			if err := unix.Mkdirat(dir, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(dir)
				return fmt.Errorf("create %s: %w", path, err)
			}
			next, err = rolefile.OpenDirectory(dir, name, path)
		}
		_ = unix.Close(dir)
		if err != nil {
			return fmt.Errorf("read %s: %w", excludePath, err)
		}
		dir = next
	}
	defer unix.Close(dir)
	file, err := rolefile.OpenRegular(dir, "exclude", excludePath, unix.O_RDWR|unix.O_CREAT|unix.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	defer file.Close()
	existing, err := rolefile.ReadBounded(file, excludePath, excludeLimit)
	if err != nil {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if line == ".codegraph/" {
			return nil
		}
	}
	content := ".codegraph/\n"
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		content = "\n" + content
	}
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("write %s: %w", excludePath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write %s: %w", excludePath, err)
	}
	return nil
}

// Bookmark is the jj bookmark an issue's workspace is on: its branch (claim.IssueBranch).
func Bookmark(issue string) string { return claim.IssueBranch(issue) }

// layoutDepth is how many directories below the state directory a shared clone and a workspace
// each sit (Location): repos/<host>/<owner>/<name>, and workspaces/<owner>/<name>/<issue>.
const layoutDepth = 4

// CloneRoot is the directory under stateDir that holds every repository's shared clone, each at
// <owner>/<name> below it (Location).
func CloneRoot(stateDir string) string {
	return filepath.Join(stateDir, "repos", "github.com")
}

// Location is the deterministic workspace location Provision creates for one issue, and the shared
// clone it is a jj workspace of. The repository arrives parsed (ghrepo.Parse); a `.` or `..`
// issue is refused: joined under the repository's workspaces directory it names that directory,
// or its owner's, and Remove deletes a workspace directory whole.
func Location(stateDir string, repository ghrepo.Repository, issue string) (Workspace, error) {
	if repository.IsZero() {
		return Workspace{}, errors.New("workspace repository is required")
	}
	if stateDir == "" {
		return Workspace{}, fmt.Errorf("workspace state directory is required")
	}
	if issue == "." || issue == ".." {
		return Workspace{}, fmt.Errorf("workspace issue %q is a %q segment; want an issue key", issue, issue)
	}
	if issue == "" || issue != filepath.Base(issue) {
		return Workspace{}, fmt.Errorf("workspace issue must be a single path component")
	}
	return Workspace{
		Dir:      filepath.Join(stateDir, "workspaces", repository.Owner(), repository.Name(), strings.ToLower(issue)),
		Bookmark: Bookmark(issue),
		Clone:    filepath.Join(CloneRoot(stateDir), repository.Owner(), repository.Name()),
		Repo:     repository,
	}, nil
}

// locationOf is the workspace Location puts at dir, read out of dir's path alone: the owner and
// the name are the two directories above dir's own, the issue is dir's own base name, and the
// state directory is layoutDepth above it. A dir Location does not put exactly there — one too
// shallow for the layout, one whose owner and name are no repository, one whose issue Location
// would case differently — is in no layout this inverts, and is refused rather than guessed at.
//
// It reads no file and is the one inverse of Location the daemon keeps: located asks it whether
// the pair it was handed is a workspace Location names, and layoutWorkspace asks it for the clone
// of a directory a process stands in. A caller's own refusals — the derivation's absolute path
// among them — stay with that caller.
func locationOf(dir string) (Workspace, error) {
	stateDir := stateDirOf(dir)
	owner, name := filepath.Base(filepath.Dir(filepath.Dir(dir))), filepath.Base(filepath.Dir(dir))
	repository, err := ghrepo.Parse("workspace repository", owner+"/"+name)
	if err != nil {
		return Workspace{}, err
	}
	want, err := Location(stateDir, repository, filepath.Base(dir))
	if err != nil {
		return Workspace{}, err
	}
	if want.Dir != dir {
		return Workspace{}, fmt.Errorf("the workspace of %s under %s lies at %s", repository, stateDir, want.Dir)
	}
	return want, nil
}

// layoutWorkspace is the workspace at dir, cleaned, with the clone its place in the layout derives
// (locationOf), from dir's path and from nothing inside dir: the derivation that WorkspaceOf
// (gitworktree.go) and GuardJJ (config.go) both make for a directory a process other than the
// daemon's own runner stands in. A dir that is no absolute path, or that the layout does not
// account for, is refused, naming it.
func layoutWorkspace(dir string) (Workspace, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return Workspace{}, fmt.Errorf("the workspace %s is no absolute path, so the clone it belongs to cannot be derived", dir)
	}
	workspace, err := locationOf(dir)
	if err != nil {
		return Workspace{}, fmt.Errorf("the workspace %s is in no state directory's workspace layout: %w", dir, err)
	}
	return workspace, nil
}

// located is whether workspace's directory and clone are the pair Location derives for the issue
// its directory names: <state>/workspaces/<owner>/<repo>/<issue> beside
// <state>/repos/github.com/<owner>/<repo>. The directory is a string like any other input, so
// locationOf is asked what Location makes of it, and it answers only for a directory that is
// already the one Location would name; the clone it answers is what is left to compare.
func located(workspace Workspace) bool {
	want, err := locationOf(workspace.Dir)
	return err == nil && want.Clone == workspace.Clone
}

// stateDirOf is the state directory a workspace directory dir of Location's layout sits in,
// layoutDepth above it. It reads no file, and says nothing of whether dir is in that layout
// (locationOf).
func stateDirOf(dir string) string {
	for range layoutDepth {
		dir = filepath.Dir(dir)
	}
	return dir
}

// Name is the name of the working copy of the task whose branch is branch: the branch without
// legion/, so <KEY> for the issue's own branch and <KEY>.<part> for a task's, the part being what
// claim.BranchTask reads after the key, a slug or a slug and the number of a later give of it.
// Location takes the name in place of an issue key, which gives the working copy's directory, the
// name lower-cased, beside the issue's, and its bookmark, the branch itself. A branch that
// claim.BranchTask refuses is refused: one of no issue, of no task of one, or whose part is a word
// no slug may be, such as `removing`, which would put the working copy where Remove renames a
// working copy aside (removingSuffix) before it deletes it.
func Name(branch string) (string, error) {
	issue, part, ok := claim.BranchTask(branch)
	if !ok {
		return "", fmt.Errorf("%q is no branch of an issue or of a task of one", branch)
	}
	if part == "" {
		return issue, nil
	}
	return issue + "." + part, nil
}

// TaskDir is the directory of the working copy of the task whose branch is branch, derived from
// the directory of its issue's own working copy, issueWorkspace (a role's LEGION_WORKSPACE), and
// the branch, never from a path anyone sends: issueWorkspace itself for "" and for the issue's own
// branch, and the directory beside it that Location names for the task (Name) otherwise.
// issueWorkspace must be a working copy Location names, and branch one of its issue's: a branch of
// another issue is refused.
func TaskDir(issueWorkspace, branch string) (string, error) {
	issue, err := locationOf(issueWorkspace)
	if err != nil {
		return "", err
	}
	if branch == "" {
		return issue.Dir, nil
	}
	task, err := taskLocation(issue, branch)
	if err != nil {
		return "", err
	}
	return task.Dir, nil
}

// taskLocation is the working copy Location names for the task whose branch is branch, of the
// issue whose own working copy is issue: of the same clone, in the same directory of workspaces.
// locationOf reads the issue's key from its directory, which Location lower-cases, so the key is
// compared in lower case.
func taskLocation(issue Workspace, branch string) (Workspace, error) {
	name, err := Name(branch)
	if err != nil {
		return Workspace{}, err
	}
	key, _, _ := strings.Cut(name, ".")
	if strings.ToLower(key) != filepath.Base(issue.Dir) {
		return Workspace{}, fmt.Errorf("branch %s is of issue %s, not of the issue whose working copy is %s", branch, key, issue.Dir)
	}
	return Location(stateDirOf(issue.Dir), issue.Repo, name)
}

// TaskCopies is the working copy of each task of issue's plan that the directory of workspaces
// holds beside issue's own working copy (taskLocation), in name order: each directory whose name is
// the issue's, a dot and a task's part of a branch (claim.BranchTask, as Name reads it), and each
// such name with removingSuffix, the aside a Remove cut short left, which the next Remove of that
// working copy clears first. A task's working copy and its aside are one working copy. Only that
// one directory is read: no entry is followed, so a link or a file that bears a task's name is no
// working copy, and neither jj nor the record is asked, so a working copy is found whichever
// generation of the issue made it and whatever the plan holds now. A directory of workspaces that
// is not there holds none. issue must be the working copy Location names for its issue's key, on
// that issue's own branch; any other is refused, since a name read off anything else would answer
// no working copy for a reason that is not that there is none.
func TaskCopies(issue Workspace) ([]Workspace, error) {
	if !located(issue) {
		return nil, fmt.Errorf("the workspace %#v is not a workspace Location names", issue)
	}
	if key, part, ok := claim.BranchTask(issue.Bookmark); !ok || part != "" || strings.ToLower(key) != filepath.Base(issue.Dir) {
		return nil, fmt.Errorf("the workspace %s is on %q, which is not the own branch of its issue", issue.Dir, issue.Bookmark)
	}
	beside := filepath.Dir(issue.Dir)
	entries, err := os.ReadDir(beside)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the working copies beside %s: %w", issue.Dir, err)
	}
	prefix := filepath.Base(issue.Dir) + "."
	var copies []Workspace
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), removingSuffix)
		part, ok := strings.CutPrefix(name, prefix)
		if !ok || !entry.IsDir() {
			continue
		}
		task, err := taskLocation(issue, issue.Bookmark+"."+part)
		if err != nil || task.Dir != filepath.Join(beside, name) || slices.ContainsFunc(copies, func(w Workspace) bool { return w.Dir == task.Dir }) {
			continue
		}
		copies = append(copies, task)
	}
	return copies, nil
}

// ProvisionTask makes the working copy of the task whose branch is branch, beside its issue's own
// working copy issueWorkspace (TaskDir), and returns it: a jj workspace of the clone the issue's is
// a workspace of, on the task's branch as origin has it. The branch is on GitHub from the task's
// start, made by the daemon before any step of the task starts, so ProvisionTask needs no default
// branch, and a branch that neither the clone nor origin has is refused by name (createWorkspace)
// rather than started anywhere else. A pane's or a pod's shim calls it, holding no credential of
// provisioning's: the fetch goes through the clone's own origin and its credential helper, which
// the issue's provisioning wrote, and brings that one branch.
//
// It runs the steps of Provision that make a workspace of a clone that is there, and none that
// make or configure the clone: under the clone's lock across processes (CloneLock), which the
// daemon's store lock takes too, the fetch, the workspace's registration with no files
// (createWorkspace) and the lock of its git worktree entry; then, with the lock let go, the
// checkout, which writes the task's working copy and nothing of the clone's. A working copy found
// there under the lock — another process made it after its caller looked — is brought up to date
// as Provision brings an issue's (update-stale, its git worktree entry, a checkout that never
// finished). The issue's own branch is refused: its working copy is Provision's.
func ProvisionTask(ctx context.Context, run Runner, issueWorkspace, branch string, log func(string)) (Workspace, error) {
	issue, err := locationOf(issueWorkspace)
	if err != nil {
		return Workspace{}, err
	}
	task, err := taskLocation(issue, branch)
	if err != nil {
		return Workspace{}, err
	}
	if task.Dir == issue.Dir {
		return Workspace{}, fmt.Errorf("branch %s is the issue's own, whose working copy %s is provisioned with the issue", branch, issue.Dir)
	}
	if log == nil {
		return Workspace{}, errors.New("the provision of a task's working copy names no log")
	}
	if _, err := guardWorkspace(task.Dir, task.Clone, false); err != nil {
		return Workspace{}, err
	}
	if cloned, err := pathExists(filepath.Join(task.Clone, ".jj")); err != nil {
		return Workspace{}, err
	} else if !cloned {
		return Workspace{}, fmt.Errorf("the shared clone %s is not there; the working copy of branch %s is a workspace of it, which the issue's provisioning makes", task.Clone, branch)
	}

	store := &storeLock{locker: CloneLock(task.Clone)}
	store.lock()
	defer store.unlock()
	exists, err := pathExists(task.Dir)
	if err != nil {
		return Workspace{}, err
	}
	if exists {
		if _, err := RunCheckedIn(ctx, run, task, []string{"jj", "workspace", "update-stale"}); err != nil {
			return Workspace{}, err
		}
		if err := restoreGitWorktree(ctx, run, task, log); err != nil {
			return Workspace{}, err
		}
	} else {
		// The fetch takes no snapshot of the clone's working copy (onClone), as provisioning's own
		// fetch takes none.
		if _, err := runCheckedOnClone(ctx, run, task.Clone, "git", "fetch", "--branch", exactName(task.Bookmark)); err != nil {
			return Workspace{}, err
		}
		if err := createWorkspace(ctx, run, task, log); err != nil {
			return Workspace{}, err
		}
	}
	checkout := !exists
	if exists {
		whole, err := CheckedOut(ctx, run, task)
		if err != nil {
			return Workspace{}, err
		}
		checkout = !whole
	}
	if err := LockGitWorktree(task.Clone, task.Dir); err != nil {
		return Workspace{}, err
	}
	store.unlock()
	if checkout {
		if _, err := runCheckedInWithin(ctx, run, task, CheckoutTimeout, []string{"jj", "sparse", "reset"}); err != nil {
			return Workspace{}, fmt.Errorf("check out workspace %s: %w", task.Dir, err)
		}
	}
	return task, nil
}
