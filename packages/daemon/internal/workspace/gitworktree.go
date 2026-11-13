package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// gitWorktreeLockReason is the reason each workspace's git worktree is locked with: `git worktree
// prune` skips a locked entry, and whoever runs one on the shared clone may not see this
// workspace's directory (an isolated session on the same host that mounts only its own checkout),
// or may run one without asking (stock jj 0.45.1's `jj workspace forget` prunes the whole clone).
const gitWorktreeLockReason = "legion workspace: its directory may be invisible to other processes sharing this clone"

// rootCommitID is jj's sentinel commit id for the root commit: every workspace's history ends
// there, and it has no git tree because it isn't a real git commit (`git cat-file` reports no such
// object). A workspace whose working copy has no real parent yet -- its `@-` is the root -- prints
// this as restoreGitWorktree's head, and `git read-tree` on it fails ("failed to unpack tree object
// HEAD"); jj's own colocated `workspace add` writes HEAD as the unborn ref below and an empty
// index for exactly this case, which restoreGitWorktree matches instead of failing to restore a
// workspace that has never had a git-visible commit.
const rootCommitID = "0000000000000000000000000000000000000000"

// rootHeadRef is the unborn ref jj's own colocated `workspace add` writes to HEAD when a
// workspace's working copy has no real parent yet (verified against jj 0.45.1-sami).
const rootHeadRef = "ref: refs/jj/root"

// gitWorktreeEntries is the admin directories of the shared clone's git worktrees registered at
// dir: each <clone>/.git/worktrees/<id> whose gitdir file names dir's .git, dir as given or with its
// symlinks resolved, as jj records it. git names an entry after the directory's base name, with a
// number on a collision, so the id is read, never derived, and each entry's gitdir is read bounded
// (readGitdir). A clone with no linked worktree, or a jj that colocates no workspace (0.44), has
// none. A clone whose .git or .git/worktrees is a symlink is refused, the refusal naming the
// clone's git directory: git never creates either as one, and each caller writes or removes files
// in the entries, which a symlink a tree agent planted would place outside the clone.
func gitWorktreeEntries(cloneDir, dir string) ([]string, error) {
	gitDir := filepath.Join(cloneDir, ".git")
	admin := filepath.Join(gitDir, "worktrees")
	for _, path := range []string{gitDir, admin} {
		info, err := os.Lstat(path)
		switch {
		case err == nil && info.Mode()&fs.ModeSymlink != 0:
			return nil, fmt.Errorf("refusing the git worktrees of %s: %s is a symlink, which git never creates", gitDir, path)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("read git worktrees: %w", err)
		}
	}
	entries, err := os.ReadDir(admin)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read git worktrees: %w", err)
	}
	// git writes a relative gitdir (jj asks for relative worktree paths; git 2.48 and later honour
	// it) between real paths, so it is resolved from the entry's real directory.
	if admin, err = filepath.EvalSymlinks(admin); err != nil {
		return nil, fmt.Errorf("resolve git worktrees: %w", err)
	}
	resolved, err := resolvedPath(dir)
	if err != nil {
		return nil, err
	}
	want := []string{filepath.Join(filepath.Clean(dir), ".git"), filepath.Join(resolved, ".git")}
	var own []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entryDir := filepath.Join(admin, entry.Name())
		gitdir, names, err := readGitdir(filepath.Join(entryDir, "gitdir"))
		if err != nil {
			return nil, fmt.Errorf("read git worktree %s: %w", entryDir, err)
		}
		if !names {
			// No worktree git could name: nobody's to claim (readGitdir).
			continue
		}
		target := strings.TrimSuffix(gitdir, "\n")
		if !filepath.IsAbs(target) {
			target = filepath.Join(entryDir, target)
		}
		if slices.Contains(want, filepath.Clean(target)) {
			own = append(own, entryDir)
		}
	}
	return own, nil
}

// gitdirLimit bounds how much of a worktree entry's gitdir file readGitdir reads: one path git
// wrote and its newline fit in PATH_MAX (4096), so a file that fills one byte more is no gitdir
// any git wrote.
const gitdirLimit = 4096 + 1

// readGitdir is the content of the gitdir file at path, and whether it names a worktree at all.
// The file is under <clone>/.git/worktrees on the issue's volume, which every role of the pod
// writes as the pod's uid, and both of the pod's sweeps read every entry's — the launcher's in
// PID 1 of the roles container, where a read that never ends stalls every pass behind the sweep's
// lock and a read that runs away ends the process. So it is opened without following a link,
// blocking on a special file or taking a terminal for PID 1's own, held to a regular file by the
// descriptor it opened, and read only up to gitdirLimit, as Created reads the workspace's creation
// record.
//
// Anything git does not write there — a symlink (which O_NOFOLLOW answers with ELOOP, a link to
// /dev/zero among them), a fifo, a socket (which open answers with ENXIO), a device, a directory,
// a file that fills the bound — names no worktree, exactly as a missing file does, and the entry
// is skipped rather than the walk refused. That keeps the sweep inside the clone: a skip only
// ever narrows the walk, so a pass removes less and never more; another workspace's entry skipped
// is one that was not this workspace's to sweep anyway; and this workspace's own entry skipped
// leaves its locks unswept for that pass, while the derivation (WorkspaceOf), which requires an
// entry naming the workspace, refuses it at the next derivation, which is the safe direction. A
// skip writes no line, so a planted fifo is not a line in PID 1's log a minute for the pod's
// life. Any other failure to read the file is answered as the error it is.
func readGitdir(path string) (string, bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENXIO):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	body, err := io.ReadAll(io.LimitReader(file, gitdirLimit))
	if err != nil {
		return "", false, err
	}
	if len(body) == gitdirLimit {
		return "", false, nil
	}
	return string(body), true, nil
}

// WorkspaceOf is the jj workspace at dir and the shared clone it is a workspace of, derived from
// dir's own place in the layout and from nothing inside dir. Location puts a workspace at
// <state>/workspaces/<owner>/<name>/<issue> and its clone at CloneRoot(<state>)/<owner>/<name>,
// so the state directory is the layoutDepth directories above dir — the inverse
// rescue.StateDirOf already takes — and the clone follows from the owner and the name dir
// carries (locationOf). What is deliberately not read is the pair jj resolves a git directory
// through, the workspace's .jj/repo and the repository's store/git_target: every process working
// the workspace can write them, and an absolute path in either is taken as it stands.
//
// It answers the whole location rather than the clone's git directory alone because a caller that
// sweeps the clone again and again needs the workspace and the clone on every pass, not once: the
// layout guard is what holds a sweep inside the clone, and it has to run before each walk
// (GitLocksOf). The text of a derived path is role-proof; the directories it names are on the
// issue's volume, which every role of the pod writes.
//
// The derivation holds where the runtime laid the workspace out with Location, which is the
// Kubernetes issue pod: its `workspace-init provision` created the workspace there and
// `internal/runtime/sandbox` mounts it at that path. A caller whose workspace came from anywhere
// else is refused by the checks below rather than answered a derived path. It also leaves the
// pod's two sweeps taking the clone from one source — the init container's takes it from
// Location, which this inverts, and both walk it through GitLocksOf — so the two cannot
// disagree about the directory they sweep.
//
// Four things are refused, each naming the path it derived, and none falls back to the
// workspace's own files: a dir the layout does not account for, a layout directory below the
// state directory that is a symlink (guardWorkspace, which every later pass runs again), a
// derived clone with no git directory, and a derived git directory serving no git worktree of
// dir. The last is a check on the derivation and not what holds anything out — the path is the
// layout's, not a file's — and it costs the two real layouts it refuses: a colocated clone whose
// own working copy is dir, which git keeps as the main worktree and registers under no entry,
// and a jj that colocates no workspace at all (0.44, gitWorktreeEntries). Such a workspace is one
// a caller is answered a reason for rather than one it sweeps.
func WorkspaceOf(dir string) (Workspace, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return Workspace{}, fmt.Errorf("the workspace %s is no absolute path, so the clone it belongs to cannot be derived", dir)
	}
	workspace, err := locationOf(dir)
	if err != nil {
		return Workspace{}, fmt.Errorf("the workspace %s is in no state directory's workspace layout: %w", dir, err)
	}
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return Workspace{}, err
	}
	own, err := gitWorktreeEntries(workspace.Clone, dir)
	if err != nil {
		return Workspace{}, err
	}
	if len(own) == 0 {
		gitDir := filepath.Join(workspace.Clone, ".git")
		if _, err := os.Stat(gitDir); err != nil {
			return Workspace{}, fmt.Errorf("the clone of %s has no git directory at %s: %w", dir, gitDir, err)
		}
		return Workspace{}, fmt.Errorf("the git directory %s of the clone of %s serves no git worktree of it", gitDir, dir)
	}
	return workspace, nil
}

// resolvedPath is path, made absolute, with the symlinks of its longest existing prefix resolved.
func resolvedPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", path, err)
		}
		missing = filepath.Join(filepath.Base(path), missing)
		path = filepath.Dir(path)
	}
}

// removeGitWorktree deletes the shared clone's git worktree entry for dir, whose directory is gone:
// what `git worktree prune` does to that one entry, locked or not, and nothing to any other.
func removeGitWorktree(cloneDir, dir string) error {
	own, err := gitWorktreeEntries(cloneDir, dir)
	if err != nil {
		return err
	}
	for _, entry := range own {
		if err := os.RemoveAll(entry); err != nil {
			return fmt.Errorf("remove git worktree %s: %w", entry, err)
		}
	}
	return nil
}

// lockGitWorktree locks the shared clone's git worktree entry for dir, as `git worktree lock` does,
// so a bare `git worktree prune` from a process that cannot see dir skips it. An entry already
// locked keeps its lock and reason.
func lockGitWorktree(cloneDir, dir string) error {
	own, err := gitWorktreeEntries(cloneDir, dir)
	if err != nil {
		return err
	}
	for _, entry := range own {
		lock, err := os.OpenFile(filepath.Join(entry, "locked"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("lock git worktree %s: %w", entry, err)
		}
		_, err = lock.WriteString(gitWorktreeLockReason)
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("lock git worktree %s: %w", entry, err)
		}
	}
	return nil
}

// restoreGitWorktree re-creates the shared clone's git worktree entry for the workspace at dir when
// the entry its .git names is gone, as a bare `git worktree prune` from a process that could not see
// dir leaves it: git fails there while jj keeps working, and `git worktree repair` cannot rebuild a
// missing entry. It writes what `git worktree add` would -- gitdir, commondir, and HEAD at the
// working-copy commit's first parent, as jj keeps it, then the index from HEAD with `git read-tree`,
// which writes no working-tree file -- into a temporary sibling directory, and renames it onto
// target only once every step succeeds, so target is either the complete entry or still absent even
// across a kill (a SIGKILL, an OOM, a pod eviction, a daemon restart) that runs no Go defer. A
// workspace with no .git (jj 0.44), or whose entry exists, is left alone; a pointer outside the
// clone's git worktrees is refused, since a tree agent can write the workspace's .git.
func restoreGitWorktree(ctx context.Context, run Runner, workspace Workspace, log func(string)) error {
	pointer, err := os.ReadFile(filepath.Join(workspace.Dir, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the workspace's git pointer: %w", err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(pointer)), "gitdir: ")
	if !ok {
		return fmt.Errorf("%s/.git names no git worktree: %q", workspace.Dir, pointer)
	}
	dir, err := filepath.EvalSymlinks(workspace.Dir)
	if err != nil {
		return fmt.Errorf("resolve the workspace: %w", err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if target, err = resolvedPath(target); err != nil {
		return err
	}
	present, err := pathExists(target)
	if err != nil || present {
		return err
	}
	// target is resolved through every symlink on its path (resolvedPath, above), so a symlinked
	// worktrees component inside it is already followed there. worktrees, in contrast, is resolved
	// only as far as .git: "worktrees" is joined on as a literal, unresolved path segment. A tree
	// agent that can write the shared clone's .git can replace .git/worktrees with a symlink to any
	// directory, and resolving that symlink here too would make both sides of this comparison agree
	// wherever it points -- git never creates .git/worktrees as a symlink, so resolving one is never
	// a legitimate case, only ever that replacement.
	gitDir, err := filepath.EvalSymlinks(filepath.Join(workspace.Clone, ".git"))
	if err != nil {
		return fmt.Errorf("resolve the shared clone's git directory: %w", err)
	}
	worktrees := filepath.Join(gitDir, "worktrees")
	if filepath.Dir(target) != worktrees {
		return fmt.Errorf("workspace %s's .git, which a tree agent can write, names %s outside the shared clone's %s; provisioning refuses to create or write it. Remove the workspace so the next provisioning adds it again", workspace.Dir, target, worktrees)
	}
	parents, err := RunCheckedIn(ctx, run, workspace, []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `parents.map(|c| c.commit_id()).join("\n")`})
	if err != nil {
		return err
	}
	head, _, _ := strings.Cut(strings.TrimSpace(parents.Stdout), "\n")
	if !commitID.MatchString(head) {
		return fmt.Errorf("workspace %s: jj printed no parent commit for its working copy: %q", workspace.Dir, parents.Stdout)
	}
	// git removes .git/worktrees along with its last entry, so the directory may be absent when the
	// temporary entry is built; it is (re)created first. A random suffix under a leading dot: git
	// assigns a worktree id from its directory's own base name, and never assigns one beginning with
	// a dot (verified: a directory named ".x" gets the id "-x"), so no real worktree can ever
	// collide with this temporary one. The random suffix also keeps two concurrent restores of the
	// same workspace from writing into, and renaming, the same temporary directory.
	if err := os.MkdirAll(worktrees, 0o755); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	tmp, err := os.MkdirTemp(worktrees, "."+filepath.Base(target)+".restore-")
	if err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	// MkdirTemp creates tmp at 0700; git's own worktree entries land at 0755 (a plain mkdir under
	// the ordinary 022 umask), and this restore's doc comment says it writes what `git worktree add`
	// would, so the mode matches too.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	headContent, readTreeArg, where := head, "HEAD", "at "+head
	if head == rootCommitID {
		headContent, readTreeArg, where = rootHeadRef, "--empty", "fresh, with no real commit yet"
	}
	readTree := []string{"git", "--git-dir=" + tmp, "--work-tree=" + dir, "read-tree", readTreeArg}
	write := func(name, content string) error {
		return os.WriteFile(filepath.Join(tmp, name), []byte(content+"\n"), 0o644)
	}
	if err := write("gitdir", filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	if err := write("commondir", filepath.Join("..", "..")); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	if err := write("HEAD", headContent); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	if _, err := RunChecked(ctx, run, readTree, nil, workspace.Clone); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	// No lock makes two concurrent restores of this workspace impossible on every path that
	// reaches here: under Kubernetes one issue pod alone provisions its own clone, on a volume no
	// other pod mounts, and the Go daemon's own tmux-runtime path (outbox.go) serializes
	// provisioning only incidentally, through a single daemon process running one strictly
	// sequential goroutine, not a dedicated per-workspace claim. So a stale sibling from an earlier
	// kill is swept only now, after this restore's own rename already succeeded: at this point our own
	// temporary directory is gone (renamed away), target exists complete, and any sibling still
	// naming this workspace is provably not still in use by us -- a still-racing concurrent
	// restore's own eventual rename onto target can now only fail, never corrupt what this one
	// just wrote, so sweeping it costs that race nothing this restore did not already win.
	if stale, globErr := filepath.Glob(filepath.Join(worktrees, "."+filepath.Base(target)+".restore-*")); globErr != nil {
		log(fmt.Sprintf("Workspace %s: could not list stale restore entries under %s: %v", workspace.Dir, worktrees, globErr))
	} else {
		for _, entry := range stale {
			if err := os.RemoveAll(entry); err != nil {
				log(fmt.Sprintf("Workspace %s: could not remove the stale restore entry %s: %v", workspace.Dir, entry, err))
			}
		}
	}
	log(fmt.Sprintf("Workspace %s had lost its git worktree entry %s (a git worktree prune that could not see the workspace deletes it): restored it %s, the working copy untouched", workspace.Dir, target, where))
	return nil
}
