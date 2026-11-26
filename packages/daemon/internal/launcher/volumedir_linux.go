//go:build linux

package launcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// volumeOpenFlags is every open the trimmer makes of a name on the volume, the os.Root each
// handle's lstats and listing are read through aside (rootOverDescriptor). O_NONBLOCK so that no
// open of the trimmer's can wait on what it meets — a FIFO where a cache root belongs answers at
// once here and blocks for ever without it. O_NOFOLLOW, and deliberately no O_DIRECTORY, so that
// a name that is a symbolic link is refused: on this kernel the two flags together are what makes
// a link followed rather than refused, and the fstat below is what makes the open a directory's
// (volumedir.go, where both are measured).
const volumeOpenFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

// openVolumeDir opens an absolute path as a directory, one element at a time from the root of
// the filesystem: the volume root the size walk measures and the four reclaimable roots the
// sweep lists are reached this way, and nothing else of the trimmer's resolves a path on the
// volume but the statfs of its mount point (readVolumeUsage).
//
// It is one open per element because the elements above a reclaimable root are the roles' own:
// /legion is the pod's mount point, and scratch, go, scratch/bun and scratch/bun/install under
// it are directories a role may rename and leave a link in the place of, with no race at all. A
// path resolved in one call follows that link — and so does an lstat of the same path, which is
// why the handle and the path agreed and the sweep went on to delete in the link's target
// (INFRA-2009, review 7). An element that is a link is refused here instead, naming the element.
func openVolumeDir(path string) (*dirHandle, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, &quotedPathError{op: "open", path: path, err: syscall.EINVAL}
	}
	// The root of the filesystem is the one name no role can be between: it is never a link and
	// never anything but a directory, and every element after it is opened from a descriptor.
	fd, err := openRetrying(func() (int, error) {
		return unix.Open("/", volumeOpenFlags|syscall.O_DIRECTORY, 0)
	})
	if err != nil {
		return nil, &quotedPathError{op: "open", path: "/", err: err}
	}
	at := "/"
	for _, element := range strings.Split(strings.Trim(clean, "/"), "/") {
		if element == "" {
			break // the path is the root of the filesystem itself, which fd already holds
		}
		at = filepath.Join(at, element)
		next, err := openElementAt(fd, element, at)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return handleOf(os.NewFile(uintptr(fd), clean), nil, clean)
}

// open is one element of the tree, opened from the descriptor this handle holds: the one read
// that is resolved by that descriptor and nothing else. A name with a separator in it, or one
// that is "." or "..", is refused rather than resolved, which is this change's own rule made a
// check instead of a convention (volumedir.go). The handle it answers keeps this one and the
// name, and no path (dirHandle.path), and its error names the name alone, as the deleter's open
// of a name does (treeRemoval.remove): a walk below an entry lifts it into what it failed on
// from the entry a level at a time (walkEntries, under), so no error of an open is a path of a
// size set by the depth of the tree, and a caller that names the directory the name is in does
// so itself (listShard, volumetrim.go).
func (d *dirHandle) open(name string) (*dirHandle, error) {
	fd, err := openElementAt(int(d.file.Fd()), name, name)
	if err != nil {
		return nil, err
	}
	return handleOf(os.NewFile(uintptr(fd), name), d, name)
}

// openElementAt opens one name in the directory parent holds open and answers the descriptor,
// which is a directory because its own fstat says so. path is what the error names the name by:
// the path an open from the root of the filesystem has reached (openVolumeDir), or the name
// alone (dirHandle.open, treeRemoval.remove).
func openElementAt(parent int, name, path string) (int, error) {
	if !isOneName(name) {
		return -1, &quotedPathError{op: "openat", path: path, err: syscall.EINVAL}
	}
	fd, err := openRetrying(func() (int, error) {
		return unix.Openat(parent, name, volumeOpenFlags, 0)
	})
	if err != nil {
		return -1, &quotedPathError{op: "openat", path: path, err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, &quotedPathError{op: "fstat", path: path, err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		// Asked of the descriptor and not of the open, because O_DIRECTORY is the flag this
		// kernel follows a link under, and because an open that has already happened is the
		// only reading of a name no role can move under it.
		_ = unix.Close(fd)
		return -1, &quotedPathError{op: "openat", path: path, err: syscall.ENOTDIR}
	}
	return fd, nil
}

// openRetrying is open again on an interrupted call, as the os package's own opens are
// (ignoringEINTR): a signal the launcher takes — the SIGCHLD of every generation it reaps — must
// not make the trimmer skip a root.
func openRetrying(open func() (int, error)) (int, error) {
	for {
		fd, err := open()
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

// removeEntryAt removes name from the directory dir holds open, whatever it holds, and follows no
// symbolic link on the way: it is the removal the sweep's deleter makes for every entry it is
// asked for (removeOneRequest, volumedelete.go), in place of Root.RemoveAll, which opens every
// directory it recurses into with the raw `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a
// link under (volumedir.go).
//
// It works bottom up through descriptors, as Rust's std::fs::remove_dir_all does since
// CVE-2022-21658. Every name is first unlinked as the name it is, which removes a file, a link or
// a pipe and opens nothing. A name the kernel will not unlink that way is opened as one element of
// the directory it is in (openElementAt: O_NOFOLLOW and no O_DIRECTORY, a directory because the
// descriptor's own fstat says so), its names are read through that descriptor and removed the
// same way, and it is removed last with unlinkat(AT_REMOVEDIR) from the descriptor of the
// directory it is in. A name that became a link or a file between the unlink and the open is
// refused by the open (ELOOP, or the fstat's ENOTDIR) and unlinked again as what it is now
// (TestARemovalUnlinksWhatWasSwappedInBetweenItsUnlinkAndItsOpen, which makes that swap in the
// window every time through betweenUnlinkAndOpen). A directory renamed away while its names are
// removed is emptied where it went, through the descriptor already open on it, and the link left
// in its place is refused by the last unlinkat, which answers ENOTDIR and leaves the link
// standing: raced so in a test process with no containment, 80 trials of 80, on 2026-11-14,
// kernel 4.19.0-gvisor, under go1.27.3 (TestTheDeletersRemovalFollowsNoLinkWithNoContainment).
//
// The error names what it failed on below dir by the entry's name, the name that failed and how
// deep that name lay (removalFailure), so a line in the pod's log is the entry's path followed by
// those two. A failure of the entry itself names it by that name alone (quotedPathError), so
// every error of the removal quotes the names it carries. names is the buffer every
// directory's names are read into, which the deleter makes once for all its removals
// (VolumeTrimDelete): a directory's names are all read before any of them is removed, so one
// buffer serves the whole tree.
func removeEntryAt(dir *os.File, name string, names []byte) error {
	removal := treeRemoval{names: names}
	return removal.remove(int(dir.Fd()), name)
}

// treeRemoval is one removeEntryAt: the buffer every directory's names are read into.
type treeRemoval struct {
	names []byte
}

// remove removes name from the directory parent holds open. It is given the name alone and
// never the path above it: an error names what it failed on by the name that failed and its
// depth below parent, counted on the way back up a level at a time (under), so that no level of
// a deep tree holds a copy of the whole path above it while the levels below it are removed.
// Measured in a test process on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3, on a chain
// 1500 directories deep of 250-byte names: the heap in use rose 289 MB over the removal with the
// path carried down to every level, and 1.5 MB with the name alone.
func (r *treeRemoval) remove(parent int, name string) error {
	if !isOneName(name) {
		return &quotedPathError{op: "unlinkat", path: name, err: syscall.EINVAL}
	}
	if err := unlinkRetrying(parent, name, 0); err == nil || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if betweenUnlinkAndOpen != nil {
		betweenUnlinkAndOpen(name)
	}
	fd, err := openElementAt(parent, name, name)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return nil
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		// Not a directory now, whatever the unlink met: unlinked as the name it is, or
		// refused with the reason the kernel gives for that.
		if err := unlinkRetrying(parent, name, 0); err != nil && !errors.Is(err, syscall.ENOENT) {
			return &quotedPathError{op: "unlinkat", path: name, err: err}
		}
		return nil
	case err != nil:
		return err
	}
	removed := r.removeDir(parent, name, fd)
	_ = unix.Close(fd)
	return removed
}

// removeDir empties the directory fd holds open, name in parent, and removes it. empty answers
// nil at a read that finds no names, and a name a writer creates after that read and before the
// rmdir is refused as ENOTEMPTY: names are still appearing, which is what errStillFilling says,
// and not a reason of the directory's own (stillStanding). So an rmdir refused that way reads the
// directory again, within the passes empty has left of volumeRemovalPasses: a read that finds
// names makes a pass over them, and answers errStillFilling once the passes are spent, as a
// writer ahead of every pass makes empty answer; a read that finds none means no name appeared,
// so the refusal is the directory's own and is answered as it is. Each read again either spends a
// pass or answers, so the removal ends after volumeRemovalPasses+1 rmdirs at most
// (TestARemovalOfADirectoryStillBeingWrittenInEnds, TestARemovalOfADirectoryRefilledBeforeItsRmdirReadsItAgain).
func (r *treeRemoval) removeDir(parent int, name string, fd int) error {
	left := volumeRemovalPasses
	var refused error
	for {
		made, emptied := r.empty(fd, left)
		if emptied != nil {
			return under(name, emptied)
		}
		if refused != nil && made == 0 {
			return &quotedPathError{op: "unlinkat", path: name, err: refused}
		}
		left -= made
		err := beforeRemoveDirAnswer(name)
		if err == nil {
			err = unlinkRetrying(parent, name, unix.AT_REMOVEDIR)
		}
		switch {
		case err == nil, errors.Is(err, syscall.ENOENT):
			return nil
		case !errors.Is(err, syscall.ENOTEMPTY):
			return &quotedPathError{op: "unlinkat", path: name, err: err}
		}
		refused = err
	}
}

// beforeRemoveDir is called, where a case sets it, with the name removeDir is about to rmdir,
// between empty's read that found no names and the rmdir. It is nil in the pod and in every
// deleter. A case makes a name in the directory there, as a writer does in that window, which a
// case racing it from another goroutine meets only by chance; or answers an error, which removeDir
// takes as the rmdir's answer without making it, as ENOTEMPTY from a name no read lists
// (TestARemovalOfADirectoryRefilledBeforeItsRmdirReadsItAgain).
var beforeRemoveDir func(name string) error

// beforeRemoveDirAnswer is beforeRemoveDir's answer for name, nil where no case set it.
func beforeRemoveDirAnswer(name string) error {
	if beforeRemoveDir == nil {
		return nil
	}
	return beforeRemoveDir(name)
}

// betweenUnlinkAndOpen is called, where a case sets it, with the name remove is removing,
// between the unlink of that name that failed and the open that follows it. It is nil in the pod
// and in every deleter, where that window is a role's to race and no test's: the window is the
// span between two syscalls, which a case racing it from another goroutine meets only by chance,
// so the case that holds the open to its refusal makes its swap here instead, in the window
// every time (TestARemovalUnlinksWhatWasSwappedInBetweenItsUnlinkAndItsOpen).
var betweenUnlinkAndOpen func(name string)

// volumeRemovalPasses is how many times a removal removes the names it reads from one directory,
// the reads after a refused rmdir included (removeDir): the first pass, and one more for the names
// created while the first ran.
const volumeRemovalPasses = 2

// errStillFilling is a directory whose names were still appearing once empty had made all its
// passes over it: something is creating names in it as fast as they are removed. The sweep
// passes such an entry over as a removal that failed part way (trimmer.remove).
var errStillFilling = fmt.Errorf("names were still appearing in it after %d passes over its names", volumeRemovalPasses)

// empty removes every name of the directory fd holds open, in limit passes at most, and answers
// how many passes it made. It reads the names again after each pass, for what was created while
// it removed, and stops at a pass that removed nothing. A directory that still has names after
// its limit is answered rather than read again, since a writer that keeps creating names would
// otherwise hold the removal, and the sweep waiting on it, for as long as it writes. Either answer
// is the first error its names met that is not errStillFilling, and errStillFilling only where no
// other error was met (stillStanding): the names of the pass that removed nothing, or of every
// pass where the passes ran out, which answers errStillFilling of the directory itself where its
// names met no error at all. A name that cannot be removed is what would still stand once the
// writer stopped, so its error is preferred to a subdirectory's errStillFilling from below
// whichever the directory lists first (TestARemovalOfADirectoryStillBeingWrittenInNamesTheErrorItMet,
// TestARemovalNamesASiblingsErrorBeforeASubdirectoryStillFilling). Its errors name what they
// failed on from this directory, and the directory itself by no path at all (under).
func (r *treeRemoval) empty(fd, limit int) (int, error) {
	var failed error
	for pass := 1; ; pass++ {
		names, err := r.read(fd)
		if err != nil {
			return pass - 1, &quotedPathError{op: "getdents", err: err}
		}
		if len(names) == 0 {
			return pass - 1, nil
		}
		if pass > limit {
			if failed != nil {
				return pass - 1, failed
			}
			return pass - 1, &quotedPathError{op: "getdents", err: errStillFilling}
		}
		var first error
		removed := false
		for _, name := range names {
			if err := r.remove(fd, name); err != nil {
				first = stillStanding(first, err)
				continue
			}
			removed = true
		}
		if !removed {
			return pass, first
		}
		failed = stillStanding(failed, first)
	}
}

// stillStanding is which of two errors a removal answers: held, the one it holds so far, unless
// held is nil, or is errStillFilling and next, the one it met after, is an error of another kind.
// A name that failed for a reason of its own is what would still stand once a writer stopped,
// and a directory still being written in is not (empty).
func stillStanding(held, next error) error {
	if held == nil || next != nil && errors.Is(held, errStillFilling) && !errors.Is(next, errStillFilling) {
		return next
	}
	return held
}

// read is every name of the directory fd holds open but "." and "..", read from its start
// through that descriptor.
func (r *treeRemoval) read(fd int) ([]string, error) {
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return nil, err
	}
	var names []string
	for {
		n, err := unix.Getdents(fd, r.names)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(r.names[:n], -1, names)
	}
}

// unlinkRetrying is unlinkat again on an interrupted call, as openRetrying is open again.
func unlinkRetrying(parent int, name string, flags int) error {
	for {
		if err := unix.Unlinkat(parent, name, flags); err != syscall.EINTR {
			return err
		}
	}
}

// rootOverDescriptor is an os.Root over a descriptor already opened and checked, which is how
// the lstats and the listing of a handle are read (volumedir.go says why it goes through
// /proc/self/fd and what that costs).
func rootOverDescriptor(file *os.File) (*os.Root, error) {
	return os.OpenRoot(descriptorPath(int(file.Fd())))
}
