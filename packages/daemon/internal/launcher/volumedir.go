package launcher

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The directory handles every read of the trimmer goes through, and the one rule they keep: a
// handle is opened by a single element of the tree at a time, the open carries O_NONBLOCK, and
// what it opened is a directory because its own fstat says so rather than because O_DIRECTORY
// was asked for (volumedir_linux.go holds the open itself).
//
// Both halves of that rule are this cluster's kernel. Measured in an issue pod of this
// deployment on 2026-11-12, kernel 4.19.0-gvisor, under go1.27.3:
//
//   - `openat(dirfd, "link", O_RDONLY|O_NOFOLLOW|O_DIRECTORY|O_CLOEXEC)` on a name that is a
//     symbolic link to a directory opened the link's target and listed it, where Linux answers
//     ELOOP; the same open without O_DIRECTORY answered ELOOP ("too many levels of symbolic
//     links"). os.Root opens every non-final element of a name that way (os.rootOpenDir), and
//     follows a relative link that stays lexically inside the root and then resolves that link's
//     own elements the same way. So a name with a separator in it is resolved through whatever a
//     role left on the way, and so is a single name whose link target has one.
//   - os.OpenRoot of a FIFO, Root.OpenRoot of a name that is one, and fs.ReadDir of such a name
//     through a Root's FS were each still blocked three seconds after the call, because none of
//     the three opens carries O_NONBLOCK; the same open with O_NONBLOCK answered at once, with
//     the mode the fstat refuses. A role that puts a FIFO where a cache root belongs would
//     otherwise block the trimmer's goroutine for the life of the pod, and the launcher's Run
//     waits on that goroutine at the container's stop.
//
// So no read of the volume by the trimmer names more than one element, but the statfs of its
// mount point every reading takes (readVolumeUsage), and every directory of it is opened by
// openVolumeDir or dirHandle.open. The deleter's removal of an entry keeps the same rule below
// it: every directory it recurses into is opened by openElementAt, one name at a time, and every
// name is unlinked from its parent's descriptor (removeEntryAt), where Root.RemoveAll, which it
// replaces, re-opens the name it is removing with that same raw openat. The deletions' own mount
// namespace is the bound behind it (volumedelete.go).
type dirHandle struct {
	// file is the descriptor the open checked. Every element below this directory is opened
	// from it, and a sweep's deletions are made through it: the deleter child is handed this
	// very descriptor, so what it removes from is the directory this handle holds and never a
	// path resolved again (volumedelete.go).
	file *os.File
	// root is an os.Root over that same descriptor, which the lstat of a name and the listing
	// are read through. It is opened through /proc/self/fd/<descriptor> — this process's own
	// descriptor table, which is not the /proc the used-path guard reads (readUsedPaths takes
	// its root as an argument) — because the os package offers no way to make a Root of a
	// descriptor already open, and Root's own openers carry the two faults above. Nothing can
	// change what that path names between the two opens: the descriptor is this process's own
	// and names the inode the fstat read, whatever has happened to the path since.
	//
	// It costs one open and one descriptor per directory the trimmer holds, which is one per
	// reclaimable root, one per Go cache shard and one per level of a walk, never one per entry.
	root *os.Root
	// parent and name are how the handle was opened: the handle it was opened from by one name
	// (open) and that name, or no parent and the absolute path for a handle openVolumeDir
	// opened. The trimmer's log lines are read by a person, and a name under a descriptor names
	// nothing to one, so every error raised against the descriptor path is renamed before it is
	// logged: to the handle's path where the handle is a root, a shard or a child of the volume
	// root (named), and to the name alone below an entry or a target, which the walk lifts into
	// what it failed on from there (within, under). That path is built from these when an error
	// asks for it (path) and is kept nowhere: a handle that kept its own path held a copy of
	// every name above it, so a walk at the foot of a chain d levels deep held about d²/2 names'
	// bytes. Measured in a test process on 2026-11-15, kernel 4.19.0-gvisor, under go1.27.3, on
	// a chain 1500 directories deep of 250-byte names: the heap in use rose 290.2 MB over one
	// newestModTime and 290.2 MB over one measureTree with the path kept on every handle, and
	// 1.6 MB over each with the name alone.
	parent *dirHandle
	name   string
}

// handleOf is the handle over a descriptor already opened and checked, by name from parent or,
// with no parent, by the absolute path name: it takes the descriptor over, closing it if the
// Root it needs cannot be made, so a caller that gets an error holds nothing to close. Its error
// names name, as the open's own does (dirHandle.open).
func handleOf(file *os.File, parent *dirHandle, name string) (*dirHandle, error) {
	handle := &dirHandle{file: file, parent: parent, name: name}
	root, err := rootOverDescriptor(file)
	if err != nil {
		_ = file.Close()
		return nil, &quotedPathError{op: "open", path: name, err: err}
	}
	handle.root = root
	return handle, nil
}

// path is the absolute path the handle was opened through: the path of the handle at the top
// of its chain and every name opened below it, joined once. Every name of the chain is one name
// (open refuses any other before it makes a handle), so this is the path each open used to join
// a level at a time, and it is built in one pass of the chain into one string. named is its one
// caller.
func (d *dirHandle) path() string {
	depth := 0
	for at := d; at.parent != nil; at = at.parent {
		depth++
	}
	names := make([]string, depth+1)
	for at := d; at != nil; at = at.parent {
		names[depth] = at.name
		depth--
	}
	return filepath.Join(names...)
}

// descriptorPath is one descriptor of this process in its own /proc, which is how a Root is made
// over a descriptor already open (rootOverDescriptor) and how the sweep's deleter opens the
// directories the listing passed it (removeOneRequest, volumedelete.go). It is here rather than
// beside the opens because the deleter is built for every platform this binary is, and refuses
// off Linux before it reads a request (containDeletions).
func descriptorPath(fd int) string {
	return "/proc/self/fd/" + strconv.Itoa(fd)
}

// isOneName is whether name is one name in one directory: not empty, not "." or "..", and with no
// separator in it. It is the rule every open and every removal of the trimmer's checks a name
// against before it is resolved (openElementAt, treeRemoval.remove, removeOneRequest).
func isOneName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

// Close closes both descriptors of the handle.
func (d *dirHandle) Close() error {
	err := d.root.Close()
	if closed := d.file.Close(); err == nil {
		err = closed
	}
	return err
}

// Lstat is one name in this directory, read through the descriptor and following no link: the
// kind, the mode and the time every guard judges by (volumeguard.go). Root.Lstat of a name with
// no separator in it is an fstatat of that descriptor and opens nothing, so it neither follows
// what the kernel would follow nor blocks on what it would block on.
func (d *dirHandle) Lstat(name string) (os.FileInfo, error) {
	info, err := d.root.Lstat(name)
	return info, d.named(err, name)
}

// ReadDir is every name in this directory with the lstat of each, read through the descriptor:
// a File opened in a Root loads each entry with an lstatat of the directory's own descriptor
// rather than with a lazy lstat of a path (os.newUnixDirent, go1.27.3), so the kind an entry
// reports is the inode this listing read. Only "." is ever named: the directory is the one the
// handle holds, and a name is opened by open below instead.
//
// The entries come back in the order their names sort, which is what fs.ReadDir answers and
// what os.ReadDir answered before the handles.
func (d *dirHandle) ReadDir() ([]os.DirEntry, error) {
	entries, err := fs.ReadDir(d.root.FS(), ".")
	return entries, d.named(err, "")
}

// named is err with the descriptor path os.Root raises it against replaced by the path this
// handle was opened through, so a line in the pod's log names a directory of the volume, and
// quoted (quotedPathError). It builds that path (path), so it is asked only of the handles the
// sweep and the size walk hold on a root, a shard or a child of the volume root and of a name in
// one; a walk below an entry or a target names what it failed on with within instead.
func (d *dirHandle) named(err error, name string) error {
	var failed *fs.PathError
	if err != nil && errors.As(err, &failed) {
		return &quotedPathError{op: failed.Op, path: filepath.Join(d.path(), name), err: failed.Err}
	}
	return err
}

// within is err, raised by a read through a handle a walk opened below an entry, named by name
// alone, the name in that handle's directory the read failed on, or "" for the directory itself:
// the level above lifts it into what it failed on from the entry (under), so no level builds the
// path above it (dirHandle.path) and the error a walk answers is of a size set by two names.
func within(err error, name string) error {
	var failed *fs.PathError
	if err != nil && errors.As(err, &failed) {
		return &quotedPathError{op: failed.Op, path: name, err: failed.Err}
	}
	return err
}

// quotedPathError is an operation of the trimmer's that failed on a path of the volume. It reads
// as the fs.PathError of the same failure would, with the path quoted: every name below a cache
// root is one a role chose, and a name holding a newline would otherwise begin a line of its own
// in the pod's log (TestAnAgeThatCannotBeReadIsLoggedWithTheRoleChosenNameQuoted). It unwraps to
// the error the operation failed with, so errors.Is asks it what the kernel answered.
type quotedPathError struct {
	op   string
	path string
	err  error
}

func (e *quotedPathError) Error() string { return quotedPathText(e.op, e.path, "", e.err) }

func (e *quotedPathError) Unwrap() error { return e.err }

// quotedPathText is how the trimmer's two path errors read, quotedPathError and removalFailure:
// the operation, the path quoted, what is said of the path if anything (removalFailure's depth),
// and the error. The opens, reads and removals made through a handle answer one of the two
// whenever their error names a path of the volume (named, within, openElementAt, under); the few
// other errors that name a name below a cache root quote it with %q where they are made
// (containedRemover.removeAll, removeOneRequest, volumedelete.go).
func quotedPathText(op, path, said string, err error) string {
	return op + " " + strconv.Quote(path) + said + ": " + err.Error()
}

// under is err, raised below the directory name, renamed to what it failed on from the directory
// name is in: the error of a name in name becomes a removalFailure under it, the error of name
// itself names name (quotedPathError), and a removalFailure already under name is moved up a
// level in place. An error that names the directory itself carries no path of its own. Any other
// error — a walk's halt, a visit's errWalkDone — is answered as it is. The deleter's removal
// renames its errors this way on its way back up (treeRemoval.remove), and so does the guards'
// walk of an entry's tree (walkEntries).
func under(name string, err error) error {
	switch failed := err.(type) {
	case *removalFailure:
		failed.top = name
		failed.depth++
		return failed
	case *quotedPathError:
		if failed.path == "" {
			failed.path = name
			return failed
		}
		return &removalFailure{op: failed.op, top: name, name: failed.path, depth: 1, err: failed.err}
	}
	return err
}

// removalFailure is a removal or a guard's walk that failed in an entry's tree, named by the
// directory it is in (top), the name that failed and how many levels below top that name lay
// (depth, 1 for a name in top itself), and never by the path between them: the deleter answers
// it as its reply, the guards' walk answers it for an entry whose age it could not read, and the
// sweep logs both (trimmer.list, trimmer.remove), so a failure deep in a tree is an error of a
// size set by two names rather than by the tree's depth
// (TestARemovalThatFailsDeepInATreeAnswersABoundedError,
// TestAnAgeThatCannotBeReadIsLoggedAtASizeItsDepthDoesNotSet). It reads as quotedPathError does,
// with the levels between top and name written as "..." and the depth beside them where there
// are any: every name in it is one a role chose (TestARemovalErrorQuotesTheNamesARoleChose).
type removalFailure struct {
	op    string
	top   string
	name  string
	depth int
	err   error
}

func (f *removalFailure) Error() string {
	if f.depth == 1 {
		return quotedPathText(f.op, f.top+"/"+f.name, "", f.err)
	}
	return quotedPathText(f.op, f.top+"/.../"+f.name, " (depth "+strconv.Itoa(f.depth)+")", f.err)
}

func (f *removalFailure) Unwrap() error { return f.err }
