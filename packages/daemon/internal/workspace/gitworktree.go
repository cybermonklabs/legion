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

	"golang.org/x/sys/unix"
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
// dir, own: each <clone>/.git/worktrees/<id> whose gitdir file names dir's .git, dir as given or
// with its symlinks resolved, as jj records it. git names an entry after the directory's base
// name, with a number on a collision, so the id is read, never derived, and each entry's gitdir
// is read bounded (readGitdir). A clone with no linked worktree, or a jj that colocates no
// workspace (0.44), has none. A clone whose .git or .git/worktrees is a symlink is refused, the
// refusal naming the clone's git directory: git never creates either as one, and each caller
// writes or removes files in the entries, which a symlink a tree agent planted would place
// outside the clone.
//
// An entry whose gitdir is missing or cannot be read (readGitdir) names no workspace and is
// passed over: one role that makes a sibling's gitdir unreadable — a fifo, a `chmod 000` — must
// not turn off a walk of every other entry. unread is answered only when own is empty, and says
// why no entry was dir's. Only then is dir's own .git read, the pointer (`gitdir:
// <clone>/.git/worktrees/<id>`, read through readGitdir, since a role can write it too, and
// parsed by gitPointerTarget), and only once (whyNoOwnEntry): an own entry writes no line
// whatever that file holds, and nothing that file is, a fifo among them, holds the call. One rule
// places every answer, one line per case: the line unread carries, what WorkspaceOf does, and
// what a pass removes — a clock pass of the launcher's sweep, which writes the line once while it
// lasts, and the init container's sweep (RemoveStaleGitLocks), which writes it once and fails
// nothing for it:
//
//	case own   the pointer                                           line written (unread)        WorkspaceOf             a pass removes
//	A    some  anything, a sibling's entry too; it is not read       none                         accepts                 .git and own
//	B    none  unreadable, or names no entry under .git/worktrees    the pointer's error          refuses with that line  .git
//	C    none  names an entry whose gitdir is missing or unreadable  that entry and the cause     accepts                 .git
//	D    none  names an entry whose gitdir names another path        no entry's gitdir names dir  refuses with that line  .git
//	-    none  missing: the workspace has no directory yet           none in the init container   refuses with B's line   .git
//	-    none  not read: the clone has no .git, so nothing is yet    none                         refuses: no git dir     nothing
//
// The last two rows are the init container's, which sweeps before it provisions anything. The
// last is its fresh disk: no clone is no entry of anyone's, and nothing to say. The one before is
// the disk of a first attempt the kubelet retries, killed after it made the clone and before it
// added the workspace: unread is case B's there (the workspace's .git cannot be read: no such
// file), and the init container writes no line for it, since a workspace not yet made has no
// entry to miss (RemoveStaleGitLocks).
//
// The pointer decides only which line is written, never which files are removed: what a sweep
// removes is .git and own, after the layout guard, in every case. That is why C is accepted: the
// clone is the layout's path and not the pointer's, the entry the pointer names is not walked,
// and a roles container restarted over a disk where a role broke its entry's gitdir goes on
// sweeping .git rather than having no workspace for its life. The causes C names are readGitdir's:
// a fifo, a symlink, a socket, a device, a directory, larger than 4096 bytes, missing, or the
// error.
func gitWorktreeEntries(cloneDir, dir string) (own []string, unread error, err error) {
	gitDir := filepath.Join(cloneDir, ".git")
	admin := filepath.Join(gitDir, "worktrees")
	for _, path := range []string{gitDir, admin} {
		info, err := os.Lstat(path)
		switch {
		case err == nil && info.Mode()&fs.ModeSymlink != 0:
			return nil, nil, fmt.Errorf("refusing the git worktrees of %s: %s is a symlink, which git never creates", gitDir, path)
		case errors.Is(err, fs.ErrNotExist) && path == gitDir:
			// The last row of the table, the init container's fresh disk: nothing is yet, and
			// nothing is said. The row above it, a clone whose workspace has no directory yet,
			// answers case B from here on, and the init container drops that line itself
			// (RemoveStaleGitLocks): the workspace's directory is no part of this walk.
			return nil, nil, nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return nil, nil, fmt.Errorf("read git worktrees: %w", err)
		}
	}
	entries, err := os.ReadDir(admin)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, nil, fmt.Errorf("read git worktrees: %w", err)
	default:
		// git writes a relative gitdir (jj asks for relative worktree paths; git 2.48 and later
		// honour it) between real paths, so it is resolved from the entry's real directory.
		if admin, err = filepath.EvalSymlinks(admin); err != nil {
			return nil, nil, fmt.Errorf("resolve git worktrees: %w", err)
		}
	}
	resolved, err := resolvedPath(dir)
	if err != nil {
		return nil, nil, err
	}
	want := []string{filepath.Join(filepath.Clean(dir), ".git"), filepath.Join(resolved, ".git")}
	// unreadable is each passed-over entry and why, which only case C reads.
	var unreadable map[string]error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entryDir := filepath.Join(admin, entry.Name())
		gitdir, err := readGitdir(filepath.Join(entryDir, "gitdir"))
		if err != nil {
			if unreadable == nil {
				unreadable = make(map[string]error)
			}
			unreadable[entryDir] = err
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
	if len(own) > 0 {
		return own, nil, nil
	}
	return nil, whyNoOwnEntry(dir, admin, entries, unreadable), nil
}

// entryUnreadable is gitWorktreeEntries' unread in case C of its table: dir's .git names an entry
// whose gitdir cannot be read. It is the one case its callers tell apart, with errors.As:
// WorkspaceOf accepts it, and lockGitWorktree and removeGitWorktree answer it as their error. B
// and D are plain errors, the line and nothing more.
type entryUnreadable struct{ err error }

func (e *entryUnreadable) Error() string { return e.err.Error() }
func (e *entryUnreadable) Unwrap() error { return e.err }

// whyNoOwnEntry is gitWorktreeEntries' unread when no entry of admin is dir's, by the case of its
// table: the one read of dir's own .git, through readGitdir, parsed by gitPointerTarget and
// resolved as the entries are, and the entry it names among those listed. unreadable is why each
// passed-over entry could not be read.
func whyNoOwnEntry(dir, admin string, entries []fs.DirEntry, unreadable map[string]error) error {
	pointerPath := filepath.Join(dir, ".git")
	none := "no git worktree entry's gitdir names the workspace " + filepath.Clean(dir)
	pointer, err := readGitdir(pointerPath)
	if err != nil {
		return fmt.Errorf("%s, and its .git, which would name its entry, cannot be read: %w", none, err)
	}
	named, _, err := gitPointerTarget(dir, []byte(pointer))
	if err != nil {
		return fmt.Errorf("%s, and its .git, which would name its entry, names none: %w", none, err)
	}
	if cause, ok := unreadable[named]; ok {
		return &entryUnreadable{fmt.Errorf("the git worktree entry %s, which %s names, cannot be read: %w", named, pointerPath, cause)}
	}
	listed := filepath.Dir(named) == admin && slices.ContainsFunc(entries, func(entry fs.DirEntry) bool {
		return entry.IsDir() && entry.Name() == filepath.Base(named)
	})
	if listed {
		return fmt.Errorf("%s, not even that of the entry %s, which its .git names", none, named)
	}
	return fmt.Errorf("%s, and its .git, which would name its entry, names %s, which is no entry under %s", none, named, admin)
}

// gitPointerTarget is the git worktree entry a workspace's .git file names, pointer being that
// file's content and dir the workspace: the path after `gitdir: `, a relative one taken from dir's
// real directory, with the symlinks of its longest existing prefix resolved. It answers dir's real
// directory too, which restoreGitWorktree writes into the entry it restores. Both readers parse it
// with this function.
func gitPointerTarget(dir string, pointer []byte) (target, realDir string, err error) {
	target, ok := strings.CutPrefix(strings.TrimSpace(string(pointer)), "gitdir: ")
	if !ok {
		return "", "", fmt.Errorf("%s/.git names no git worktree: %q", dir, pointer)
	}
	if realDir, err = filepath.EvalSymlinks(dir); err != nil {
		return "", "", fmt.Errorf("resolve the workspace: %w", err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(realDir, target)
	}
	if target, err = resolvedPath(target); err != nil {
		return "", "", err
	}
	return target, realDir, nil
}

// gitdirLimit bounds how much readGitdir reads of a pointer file a role can write: one path git or
// jj wrote and its newline fit in PATH_MAX (4096), so a file that fills one byte more is no
// pointer either of them wrote.
const gitdirLimit = 4096 + 1

// readGitdir is the content of the pointer file at path, read bounded, or why it holds no path git
// or jj wrote: each worktree entry's gitdir, the workspace's own .git, and its .jj/repo, all on
// the issue's volume, which every role of the pod writes as the pod's uid. Both of the pod's
// sweeps read every entry's gitdir — the launcher's in PID 1 of the roles container, where a read
// that never ends stalls every pass behind the sweep's lock and a read that runs away ends the
// process. So it is opened without following a link, blocking on a special file or taking a
// terminal for PID 1's own, held to a regular file by the descriptor it opened (openRegular), and
// read only up to gitdirLimit (readBounded), as Created reads the workspace's creation record.
// The workspace's .git is the pointer to its entry (whyNoOwnEntry, and restoreGitWorktree, which a
// provision of an existing workspace runs), and its .jj/repo the pointer to its repository
// (config.go's workspaceRepository, which the runner reads before each jj command). Every one of
// these reads also runs in the tmux daemon's outbox, under the repository's store lock, which
// every later provision and removal of the repository waits on (Provision, Remove).
//
// Anything git does not write there is refused in words, naming the file: a symlink (which
// O_NOFOLLOW answers with ELOOP, a link to /dev/zero among them), a socket (which open answers
// with ENXIO), a fifo, a device, a directory, or a file larger than the bound. A missing file is
// the open's own error, and any other failure to read it is the error it is. None of them is a
// refusal of the walk: gitWorktreeEntries passes such an entry over, and names it only when no
// entry is the workspace's own and the workspace's .git names that one (its case C).
func readGitdir(path string) (string, error) {
	file, err := openRegular(unix.AT_FDCWD, path, path, unix.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	body, err := readBounded(file, path, gitdirLimit-1)
	return string(body), err
}

// openRegular opens name under the directory dirfd (unix.AT_FDCWD for a path) with flag and,
// when flag creates it, perm, and answers it only when the descriptor it opened is a regular
// file; path is name as the errors print it. It is how this package opens a file a role can
// write, for a caller that holds something others wait on: it never follows a link at name
// (O_NOFOLLOW, which answers ELOOP), never waits on a fifo or a device (O_NONBLOCK, which a
// regular file ignores), never takes a terminal (O_NOCTTY), and refuses each in words naming the
// file — a symlink, a socket (which open answers with ENXIO), a directory (EISDIR, for a flag that
// writes), or whatever else the descriptor is. Any other failure is open's own error, as
// os.OpenFile words it.
func openRegular(dirfd int, name, path string, flag int, perm uint32) (*os.File, error) {
	fd, err := openat(dirfd, name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, perm)
	switch {
	case errors.Is(err, unix.ELOOP):
		return nil, fmt.Errorf("%s is a symlink", path)
	case errors.Is(err, unix.ENXIO):
		return nil, fmt.Errorf("%s is a socket", path)
	case errors.Is(err, unix.EISDIR):
		return nil, fmt.Errorf("%s is a directory", path)
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%s is a %s", path, fileKind(info.Mode()))
	}
	return file, nil
}

// readBounded reads file, which openRegular opened as path, from where it stands to its end, and
// refuses it rather than read on once it holds more than limit bytes: a file a role wrote can be
// any size, and a reader that holds it whole in memory is one a role can end.
func readBounded(file *os.File, path string, limit int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return body, nil
}

// openDirectory is a descriptor of the directory name under dirfd (unix.AT_FDCWD for a path),
// path being name as the errors print it, for a caller that writes under it: a symlink at name is
// refused in words rather than followed (O_NOFOLLOW, which answers ELOOP), and anything at name
// but a directory, a fifo or a device among them, is opened without a wait (O_NONBLOCK, O_NOCTTY)
// and refused by what its descriptor is. O_DIRECTORY is deliberately not asked for: gVisor, the
// kernel this tree's tests run under, follows a link at name when O_DIRECTORY and O_NOFOLLOW are
// both set (measured: the open succeeds through the link), where O_NOFOLLOW alone answers ELOOP.
func openDirectory(dirfd int, name, path string) (int, error) {
	fd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP):
		return -1, fmt.Errorf("%s is a symlink", path)
	case errors.Is(err, unix.ENXIO):
		return -1, fmt.Errorf("%s is a socket", path)
	case err != nil:
		return -1, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, &fs.PathError{Op: "fstat", Path: path, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%s is no directory", path)
	}
	return fd, nil
}

// openat is unix.Openat, asked again when a signal interrupted it, as os.OpenFile asks.
func openat(dirfd int, name string, flag int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flag, perm)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

// createFile creates the file at path, which must not exist yet, holding content: an existing one
// is open's own EEXIST, never opened, so a fifo a role planted there holds no write open and a
// symlink sends no write anywhere (O_EXCL). Its directory is opened first, refused when it is a
// symlink (openDirectory), and the file is created under that descriptor, so the directory
// cannot be swapped for a link between the two. Both of its callers write under the shared
// clone's .git/worktrees, which a role can write: lockGitWorktree, into each own entry it listed,
// and restoreGitWorktree, into the temporary entry it made.
func createFile(path, content string) error {
	dir := filepath.Dir(path)
	parent, err := openDirectory(unix.AT_FDCWD, dir, dir)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := openat(parent, filepath.Base(path), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	_, err = file.WriteString(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// chmodDirectory sets the mode of the directory at path through a descriptor of it opened without
// following a link (openDirectory), so a symlink swapped in at path changes the mode of nothing
// else.
func chmodDirectory(path string, mode uint32) error {
	fd, err := openDirectory(unix.AT_FDCWD, path, path)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchmod(fd, mode); err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	return nil
}

// WorkspaceOf is the jj workspace at dir and the shared clone it is a workspace of, derived from
// dir's own place in the layout and from nothing inside dir. Location puts a workspace at
// <state>/workspaces/<owner>/<name>/<issue> and its clone at CloneRoot(<state>)/<owner>/<name>,
// so the state directory is the layoutDepth directories above dir — the inverse
// rescue.StateDirOf already takes — and the clone follows from the owner and the name dir
// carries (LocationOf). What is deliberately not read is the pair jj resolves a git directory
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
// Five things are refused, each naming the path it derived, and none falls back to the
// workspace's own files: a dir the layout does not account for, a layout directory below the
// state directory that is a symlink (guardWorkspace, which every later pass runs again), a
// derived clone with no git directory, and the two cases of gitWorktreeEntries' table it refuses
// when no entry's gitdir names dir, each named with the line unread carries: dir's .git that
// cannot be read or names no entry (B), and dir's .git naming an entry whose gitdir names another
// path (D). A git directory that appeared after the walk found none, the one way no entry comes
// with no unread, is refused as serving no git worktree of dir. The .git that names an entry
// whose gitdir cannot be read (C) is accepted: the clone is still the layout's, a pass removes
// nothing under that entry, and a roles container restarted over such a disk goes on sweeping the
// clone's git directory, its first pass writing that entry and the cause. The two refusals are a
// check on the derivation and not what holds anything out — the path is the layout's, not a
// file's — and they cost the two real layouts they refuse: a colocated clone whose own working
// copy is dir, which git keeps as the main worktree and registers under no entry, and a jj that
// colocates no workspace at all (0.44). Such a workspace is one a caller is answered a reason for
// rather than one it sweeps.
func WorkspaceOf(dir string) (Workspace, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return Workspace{}, fmt.Errorf("the workspace %s is no absolute path, so the clone it belongs to cannot be derived", dir)
	}
	workspace, err := LocationOf(dir)
	if err != nil {
		return Workspace{}, fmt.Errorf("the workspace %s is in no state directory's workspace layout: %w", dir, err)
	}
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return Workspace{}, err
	}
	own, unread, err := gitWorktreeEntries(workspace.Clone, dir)
	if err != nil {
		return Workspace{}, err
	}
	var unreadable *entryUnreadable
	if len(own) > 0 || errors.As(unread, &unreadable) {
		return workspace, nil
	}
	gitDir := filepath.Join(workspace.Clone, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return Workspace{}, fmt.Errorf("the clone of %s has no git directory at %s: %w", dir, gitDir, err)
	}
	if unread != nil {
		return Workspace{}, fmt.Errorf("the clone of %s: %w", dir, unread)
	}
	return Workspace{}, fmt.Errorf("the git directory %s of the clone of %s serves no git worktree of it", gitDir, dir)
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
// what `git worktree prune` does to that one entry, locked or not, and nothing to any other. An
// entry dir's .git names whose gitdir cannot be read (gitWorktreeEntries' case C) is that error:
// the entry is dir's, and removing nothing would say nothing. No entry for dir at all (B, D) is
// nothing to remove, and no error.
func removeGitWorktree(cloneDir, dir string) error {
	own, unread, err := gitWorktreeEntries(cloneDir, dir)
	if err != nil {
		return err
	}
	var unreadable *entryUnreadable
	if errors.As(unread, &unreadable) {
		return unread
	}
	for _, entry := range own {
		// No row covers removeTree here: the window it closes is a swap of .git/worktrees for a
		// fifo between gitWorktreeEntries' ReadDir and this removal, which no test can land in
		// deterministically.
		if err := removeTree(entry); err != nil {
			return fmt.Errorf("remove git worktree %s: %w", entry, err)
		}
	}
	return nil
}

// removeTree removes path and everything under it, as os.RemoveAll does, without ever waiting on
// the directory path is in. Every path it is handed is under the shared clone's .git/worktrees,
// which a role can swap for a fifo: os.RemoveAll, when path is no file it can unlink outright,
// opens path's parent with a plain open(2) to remove what is under path (os/removeall_at.go), and
// a plain open of a fifo waits for a writer for as long as none comes, under whatever its caller
// holds (the tmux daemon's store lock among them). So path is named through its parent's own "."
// entry: os.RemoveAll opens `<parent>/.` instead, which the kernel resolves only through a
// directory, and a parent that is no directory is ENOTDIR at once. Below path os.RemoveAll opens
// every directory with O_DIRECTORY and O_NOFOLLOW already, so nothing it walks there can hold it.
func removeTree(path string) error {
	parent, name := filepath.Split(filepath.Clean(path))
	return os.RemoveAll(parent + "." + string(filepath.Separator) + name)
}

// lockGitWorktree locks the shared clone's git worktree entry for dir, as `git worktree lock` does,
// so a bare `git worktree prune` from a process that cannot see dir skips it. An entry already
// locked keeps its lock and reason. An entry dir's .git names whose gitdir cannot be read
// (gitWorktreeEntries' case C) is that error, as for removeGitWorktree: under the tmux runtime
// nothing else would say the entry went unlocked. Every later provision of the workspace fails on
// it the same way and nothing repairs it, so the error says what does, as restoreGitWorktree's
// refusal of a pointer outside the clone says it.
//
// The lock is written with createFile, which opens the entry's directory without following a
// link and creates the file under that descriptor: the entries were listed by gitWorktreeEntries'
// ReadDir, and a role that swapped an entry for a symlink after that would otherwise have the lock
// written wherever the link points. No row covers it: the window is a swap between that ReadDir
// and this open, which no test can land in deterministically.
func lockGitWorktree(cloneDir, dir string) error {
	own, unread, err := gitWorktreeEntries(cloneDir, dir)
	if err != nil {
		return err
	}
	var unreadable *entryUnreadable
	if errors.As(unread, &unreadable) {
		return fmt.Errorf("%w; provisioning cannot lock it. Remove the workspace so the next provisioning adds it again", unread)
	}
	for _, entry := range own {
		err := createFile(filepath.Join(entry, "locked"), gitWorktreeLockReason)
		if errors.Is(err, fs.ErrExist) {
			continue
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
//
// That .git, the pointer to the entry, is read through readGitdir, the bounded read with no link
// followed and no wait that the sweeps make of the same file: every process working the
// workspace can write it, and this read runs under whatever its caller holds — the tmux daemon's
// outbox calls Provision, and so this, with the repository's store lock held, which every later
// provision of the repository waits on, and a pod's `workspace-init provision` runs it before any
// container of the pod starts. A .git readGitdir answers as no file git wrote (a fifo, a symlink,
// a socket, a device, a directory, larger than 4096 bytes) is an error naming the cause:
// provisioning restores no entry whose pointer it never read, and does not report one restored.
func restoreGitWorktree(ctx context.Context, run Runner, workspace Workspace, log func(string)) error {
	pointer, err := readGitdir(filepath.Join(workspace.Dir, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the workspace's git pointer, which a tree agent can write: %w", err)
	}
	target, dir, err := gitPointerTarget(workspace.Dir, []byte(pointer))
	if err != nil {
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
	cloneGit := filepath.Join(workspace.Clone, ".git")
	gitDir, err := filepath.EvalSymlinks(cloneGit)
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
	//
	// Before that first write the shared clone's .git and .git/worktrees are each refused when
	// they are a symlink: git never creates either as one (gitWorktreeEntries refuses them too),
	// and every directory and file below is made through them by path, so a link at either would
	// have it all made wherever the link points. The comparison above refuses a link at
	// .git/worktrees that was there when it ran, but never one at .git: both of its sides are
	// resolved through .git, so a link there moves both alike. This check is what refuses a link at
	// .git, whether it was there before the restore or swapped in while jj ran, a window a role can
	// use (TestRestoreGitWorktreeRefusesAGitDirectorySymlinkedAway). What it leaves open runs from
	// this check to the last open of the temporary entry by path — MkdirTemp, the chmod, the three
	// creates, git read-tree and the rename — which no test can land in deterministically.
	for _, path := range []string{cloneGit, worktrees} {
		if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to restore git worktree %s: %s is a symlink, which git never creates", target, path)
		}
	}
	if err := os.MkdirAll(worktrees, 0o755); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	tmp, err := os.MkdirTemp(worktrees, "."+filepath.Base(target)+".restore-")
	if err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	defer func() { _ = removeTree(tmp) }()
	// MkdirTemp creates tmp at 0700; git's own worktree entries land at 0755 (a plain mkdir under
	// the ordinary 022 umask), and this restore's doc comment says it writes what `git worktree add`
	// would, so the mode matches too. The mode is set through a descriptor of tmp opened without
	// following a link (openDirectory), never by its path: a role that swapped tmp for a symlink
	// would otherwise have this chmod any directory the link names. No row covers it: the window is
	// a swap between two syscalls (MkdirTemp and this open), which no test can land in
	// deterministically.
	if err := chmodDirectory(tmp, 0o755); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	headContent, readTreeArg, where := head, "HEAD", "at "+head
	if head == rootCommitID {
		headContent, readTreeArg, where = rootHeadRef, "--empty", "fresh, with no real commit yet"
	}
	readTree := []string{"git", "--git-dir=" + tmp, "--work-tree=" + dir, "read-tree", readTreeArg}
	// Each file is created with createFile: under a descriptor of tmp opened without following a
	// link, and with O_EXCL, never opened if something is already there. tmp is under
	// .git/worktrees, which a role can write: a fifo it planted at tmp/<name> would hold a plain
	// O_WRONLY open for as long as no reader came, with the store lock held, and a tmp it swapped
	// for a symlink would have the file written wherever the link points. No row covers either:
	// the window is a swap between two syscalls (MkdirTemp and these opens), which no test can land
	// in deterministically.
	write := func(name, content string) error {
		return createFile(filepath.Join(tmp, name), content+"\n")
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
	//
	// The stale entries are listed with os.ReadDir, which opens worktrees with O_DIRECTORY, rather
	// than filepath.Glob, which stats it and then opens it with a plain open: a role that swapped
	// .git/worktrees for a fifo between Glob's two syscalls would hold that open with the store
	// lock held. No row covers it, for the same reason: the window is a swap between two
	// syscalls. The name is matched as a literal prefix, so a workspace name holding a glob
	// metacharacter matches its own entries alone.
	if listed, listErr := os.ReadDir(worktrees); listErr != nil {
		log(fmt.Sprintf("Workspace %s: could not list stale restore entries under %s: %v", workspace.Dir, worktrees, listErr))
	} else {
		prefix := "." + filepath.Base(target) + ".restore-"
		for _, listedEntry := range listed {
			if !strings.HasPrefix(listedEntry.Name(), prefix) {
				continue
			}
			entry := filepath.Join(worktrees, listedEntry.Name())
			if err := removeTree(entry); err != nil {
				log(fmt.Sprintf("Workspace %s: could not remove the stale restore entry %s: %v", workspace.Dir, entry, err))
			}
		}
	}
	log(fmt.Sprintf("Workspace %s had lost its git worktree entry %s (a git worktree prune that could not see the workspace deletes it): restored it %s, the working copy untouched", workspace.Dir, target, where))
	return nil
}
