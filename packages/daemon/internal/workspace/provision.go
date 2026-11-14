package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"golang.org/x/sys/unix"
)

// Provision ports provisionIssueWorkspace (packages/workspace/src/workspace.ts). It updates an
// existing working copy, obtains the shared clone through a temporary sibling, protects unreachable
// worker commits before every fetch, resolves a bookmark before adding, and leaves pane credentials
// on the clone.
// The clone and the fetch reach the repository through request.Source: a pod's feed with no
// credential, or GitHub with the one-shot credential. An issue with no branch starts at
// request.DefaultBranch, which the request must name. Every command on the shared clone runs under
// request.StoreLock, the `jj workspace add` that registers the issue's workspace with no files
// among them. Only the checkout (`jj sparse reset`), which writes the issue's own working copy and
// nothing of the clone's, runs with the lock let go, holding one of request.CheckoutSlot's places
// instead.
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
		return Workspace{}, errors.New("workspace request names no way to the repository: FromFeed or FromGitHub")
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
	if err := ensureRepoClone(ctx, run, WallClock{}, workspace.Clone, GitHubURL(request.Repo), request.DefaultBranch, source); err != nil {
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
	if err := lockGitWorktree(workspace.Clone, workspace.Dir); err != nil {
		return Workspace{}, err
	}
	if err := configureRepositoryCredential(ctx, run, workspace.Clone, request.CredentialHelper); err != nil {
		return Workspace{}, err
	}
	// Every provision writes it, so a clone made before this setting existed takes it at its next
	// provision.
	if err := configureCloneReads(ctx, run, workspace.Clone); err != nil {
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
	// CommandTimeout's five minutes: one write of the largest repository mapped took nine minutes
	// in an issue pod (CheckoutTimeout), and the runner killing it there failed the whole
	// provision and spent one of the claim's three launches. So this command alone takes
	// CheckoutTimeout (request.checkoutBound), and every provisioning command but it and those
	// steps keeps the runner's slow-command budget.
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
// whole tree (`.`) after Provision's checkout, or whatever narrower set an agent chose since, which
// a launch must not undo. Provision registers a workspace with no pattern and no file (`jj
// workspace add --sparse-patterns empty`) and checks it out after (`jj sparse reset`), so a
// workspace whose checkout failed or was cut short — a jj error, a full disk, a daemon stopped
// mid-checkout — holds no pattern until a provision checks it out. A shared clone made from a
// pod's feed permanently holds none either, its patterns cleared when it was made so it carries no
// working tree (feedClone's `jj sparse set --clear`), and that is no unfinished checkout. So
// CheckedOut refuses two directories rather than answering for them: the shared clone itself, and
// any directory under the clone root that clone sits in (CloneRoot). A clone's empty pattern set
// would answer false, the caller would read that as an unfinished checkout, and its `jj sparse
// reset` would write the whole tree into the clone. The read never snapshots the working copy
// (--ignore-working-copy): a pane may be writing it, and a snapshot would run the working-copy
// filters, fsmonitor and signing programs a tree agent can configure. A directory with no `.jj` is
// refused, as every jj command in a workspace is (guardWorkspace).
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

// whyACloneHasNoAnswer is the tail of both of CheckedOut's refusals: the reason a clone's pattern
// set answers nothing, which is the same whichever clone directory was asked about.
const whyACloneHasNoAnswer = " A clone holds no sparse pattern by design, because it carries no working tree (feedClone's `jj sparse set --clear`). An answer of false here reads as an unfinished checkout, and the caller then writes the whole tree into the clone"

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
// read and appended to through that one descriptor, and opened as openRegular opens a file a role
// can write: a symlink there is refused rather than followed, which would grow whatever file the
// link names, and a fifo or a device is refused rather than waited on, in its open or its read. A
// directory, or a file larger than excludeLimit, is refused too, each naming the file. The clone
// and the two directories between it and the file are opened the same way, without following a
// link (openDirectory), each under the descriptor of the one above it, so a link a role leaves at
// .git or at info is refused as one at the file is, and nothing below the clone is reached by a
// path a role could swap between two opens.
func excludeCodegraphDirectory(cloneDir string) error {
	excludePath := filepath.Join(cloneDir, ".git", "info", "exclude")
	dir, err := openDirectory(unix.AT_FDCWD, cloneDir, cloneDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	path := cloneDir
	for _, name := range []string{".git", "info"} {
		path = filepath.Join(path, name)
		next, err := openDirectory(dir, name, path)
		if errors.Is(err, fs.ErrNotExist) {
			if err := unix.Mkdirat(dir, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(dir)
				return fmt.Errorf("create %s: %w", path, err)
			}
			next, err = openDirectory(dir, name, path)
		}
		_ = unix.Close(dir)
		if err != nil {
			return fmt.Errorf("read %s: %w", excludePath, err)
		}
		dir = next
	}
	defer unix.Close(dir)
	file, err := openRegular(dir, "exclude", excludePath, unix.O_RDWR|unix.O_CREAT|unix.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	defer file.Close()
	existing, err := readBounded(file, excludePath, excludeLimit)
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

// LocationOf is the workspace Location puts at dir, read out of dir's path alone: the owner and
// the name are the two directories above dir's own, the issue is dir's own base name, and the
// state directory is layoutDepth above it. A dir Location does not put exactly there — one too
// shallow for the layout, one whose owner and name are no repository, one whose issue Location
// would case differently — is in no layout this inverts, and is refused rather than guessed at.
//
// It reads no file and is the one inverse of Location the daemon keeps: located asks it whether
// the pair it was handed is a workspace Location names, and WorkspaceOf (gitworktree.go) asks it
// for the clone of the directory a process stands in. A caller's own refusals — the derivation's
// absolute path among them — stay with that caller.
func LocationOf(dir string) (Workspace, error) {
	stateDir := dir
	for range layoutDepth {
		stateDir = filepath.Dir(stateDir)
	}
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

// located is whether workspace's directory and clone are the pair Location derives for the issue
// its directory names: <state>/workspaces/<owner>/<repo>/<issue> beside
// <state>/repos/github.com/<owner>/<repo>. The directory is a string like any other input, so
// LocationOf is asked what Location makes of it, and it answers only for a directory that is
// already the one Location would name; the clone it answers is what is left to compare.
func located(workspace Workspace) bool {
	want, err := LocationOf(workspace.Dir)
	return err == nil && want.Clone == workspace.Clone
}
