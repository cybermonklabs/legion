package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stagingInfix is the part of the name of the directory that a run of print builds beside its spec
// directory: "." and the spec directory's name, stagingInfix, and the inode of the spec directory
// before the run (stagingPath).
const stagingInfix = ".spec-check-print-"

// stagingPath is the directory that a run of print builds beside dir, whose inode is ino before the
// run: one run at a time holds the lock of dir, so one name serves each run.
func stagingPath(dir string, ino uint64) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+stagingInfix+strconv.FormatUint(ino, 10))
}

// recordSuffix ends the name of the record of a run beside its spec directory: the name of the
// run's directory and recordSuffix (recordPath).
const recordSuffix = ".texts"

// recordPath is the record of the run whose directory beside the spec directory is staging: the
// SHA-256 of the text of each spec file of the spec directory that the run read (textsRead), which
// the run writes before the exchange and removes when it has finished the old directory. A run that
// finishes the old directory of a run that it did not see end (cleanLeftovers) removes a spec file
// of it only where the record holds its text, so it removes no change that another program made
// there, also when the run that made the directory could not give it another name (keep).
// finishOld and undoNew also remove the record when they keep its directory under another name
// (keep).
func recordPath(staging string) string { return staging + recordSuffix }

// textsRead is the SHA-256 of each text of files, in hex, by the name of its file.
func textsRead(files map[string][]byte) map[string]string {
	read := make(map[string]string, len(files))
	for name, text := range files {
		sum := sha256.Sum256(text)
		read[name] = hex.EncodeToString(sum[:])
	}
	return read
}

// writeRecord writes the record of the run whose directory is staging: textsRead of files, the
// text of each spec file of the spec directory when the run read it.
func writeRecord(fsys fileSystem, staging string, files map[string][]byte) error {
	data, err := json.Marshal(textsRead(files))
	if err != nil {
		return err
	}
	return writeSynced(fsys, recordPath(staging), append(data, '\n'), 0o644, nil, "")
}

// readRecord is the record of the run whose directory is staging, and nil when the run left none
// or one that is not in the form of writeRecord.
func readRecord(staging string) map[string]string {
	data, err := os.ReadFile(recordPath(staging))
	if err != nil {
		return nil
	}
	var read map[string]string
	if json.Unmarshal(data, &read) != nil {
		return nil
	}
	return read
}

// fileSystem is each call by which print changes the file system or makes a change durable: print
// passes osFileSystem, and a test passes one that fails a call, or stops the run after a call.
type fileSystem interface {
	// mkdir makes the directory path with the mode perm.
	mkdir(path string, perm fs.FileMode) error
	// create makes the new file path, mode 0644, open to write, and refuses when path exists.
	create(path string) (*os.File, error)
	// write writes content to f.
	write(f *os.File, content []byte) error
	// sync puts f, a file or a directory, on the disk.
	sync(f *os.File) error
	// chmod gives path the mode mode.
	chmod(path string, mode fs.FileMode) error
	// lchown gives path the owner uid and the group gid, and does not follow a symbolic link.
	lchown(path string, uid, gid int) error
	// link makes newname a second name of the file oldname.
	link(oldname, newname string) error
	// lock takes the exclusive flock of the open directory f without waiting (tryLock).
	lock(f *os.File) error
	// exchange puts the directory a in the place of the directory b, and b in the place of a,
	// with one call.
	exchange(a, b string) error
	// moveNoReplace renames from to to, and refuses when an entry has the name to.
	moveNoReplace(from, to string) error
	// rename renames from to to, and replaces an entry of the name to that is of its kind, a
	// directory over an empty directory and a file over a file, as the system's rename does.
	// moveByHand renames only over the entry that it made itself at to.
	rename(from, to string) error
	// remove removes the file or the empty directory path.
	remove(path string) error
}

// osFileSystem is the file system of the process.
type osFileSystem struct{}

func (osFileSystem) mkdir(path string, perm fs.FileMode) error { return os.Mkdir(path, perm) }

func (osFileSystem) create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

func (osFileSystem) write(f *os.File, content []byte) error {
	_, err := f.Write(content)
	return err
}

func (osFileSystem) sync(f *os.File) error                     { return f.Sync() }
func (osFileSystem) chmod(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }
func (osFileSystem) lchown(path string, uid, gid int) error    { return os.Lchown(path, uid, gid) }
func (osFileSystem) link(oldname, newname string) error        { return os.Link(oldname, newname) }
func (osFileSystem) rename(from, to string) error              { return renameReplacing(from, to) }
func (osFileSystem) lock(f *os.File) error                     { return tryLock(f) }
func (osFileSystem) remove(path string) error                  { return os.Remove(path) }

// writeDir changes the spec files of dir, files, to after in one step, so that a reader, and a
// kill, sees the spec files as they were or as the run leaves them. It builds a new directory
// beside dir (stagingPath) that holds each spec file of after: a file that changes written and
// synced, and each other as a hard link to it, the same file, or as a copy where the system
// refuses the link (linkOrCopy). The new directory takes the mode and the owner of dir. Then
// writeDir locks the new directory, so that a run that opens dir after the exchange waits for this
// one, exchanges the two directories with one call (exchange), syncs their parent, and finishes
// the old directory (finishOld): it moves each entry that is no spec file, a file of another name,
// a symbolic link or a subdirectory, which print neither reads nor changes, into dir, the same
// entry, and removes the rest. Before the exchange it writes its record beside dir (recordPath):
// the text of each spec file that it read, so that a later run removes from the old directory only
// what this run read. A step that fails before the exchange removes the new directory and its
// record and leaves dir as it was; a system that cannot exchange two directories refuses the
// exchange, so print refuses with dir as it was, and names the way out where it knows one
// (exchangeRefusal), and so does a system that refuses the move of an entry that print makes
// after the exchange, which print tries in the new directory first (probeMove). A run that is
// killed leaves the new or the old directory beside dir, with its record, which the next run
// finishes (cleanLeftovers). A directory that the process may not write
// is refused, as the files of such a directory were, and so is a parent that it may not write,
// where the new directory goes, and an owner that it may not give the new directory, each with the
// way out (writable, sameOwner). warnings is a line for each step after the exchange that did not
// finish, or for an entry that another program changed while the run built the new directory,
// which writeDir keeps beside dir; kept is whether it kept such an entry, which a person must
// compare with dir. Each change that it makes in the file system goes through fsys.
func writeDir(fsys fileSystem, dir string, lock *specDirLock, files, after map[string][]byte) (warnings []string, kept bool, err error) {
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(after)) {
		if old, held := files[name]; !held || !bytes.Equal(old, after[name]) {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return nil, false, nil
	}
	if err := writable(dir); err != nil {
		return nil, false, err
	}
	old, err := os.Lstat(dir)
	if err != nil {
		return nil, false, err
	}
	staging := stagingPath(dir, inode(old))
	if err := fsys.mkdir(staging, 0o700); err != nil {
		return nil, false, err
	}
	exchanged := false
	defer func() {
		if !exchanged {
			removeNew(fsys, staging)
		}
	}()
	for _, name := range slices.Sorted(maps.Keys(after)) {
		to, from := filepath.Join(staging, name), filepath.Join(dir, name)
		switch _, held := files[name]; {
		case !slices.Contains(changed, name):
			err = linkOrCopy(fsys, from, to, after[name])
		case !held:
			err = writeSynced(fsys, to, after[name], 0o644, old, dir)
		default:
			err = copyFile(fsys, from, to, after[name])
		}
		if err != nil {
			return nil, false, err
		}
	}
	if err := probeMove(fsys, staging); err != nil {
		return nil, false, err
	}
	if err := fsys.chmod(staging, old.Mode()&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return nil, false, err
	}
	if err := sameOwner(fsys, staging, "the new directory", dir, old); err != nil {
		return nil, false, err
	}
	built, err := os.Open(staging)
	if err != nil {
		return nil, false, err
	}
	lock.hold(built)
	if err := fsys.lock(built); err != nil {
		return nil, false, err
	}
	if err := fsys.sync(built); err != nil {
		return nil, false, err
	}
	if err := writeRecord(fsys, staging, files); err != nil {
		return nil, false, err
	}
	if err := fsys.exchange(staging, dir); err != nil {
		return nil, false, exchangeRefusal(err)
	}
	exchanged = true
	if err := syncDir(fsys, filepath.Dir(dir)); err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: the system did not sync the directory that holds it, so a power loss can undo the run: %s", dir, err))
	}
	lines, kept := finishOld(fsys, staging, dir, textsRead(files))
	return append(warnings, lines...), kept, nil
}

// The words of the refusals of print where a system or a file system cannot do what print does,
// each with its way out: noExchange, of a system that gives no call that exchanges two directories
// (exchange_other.go); noFlock, of a system that gives no flock (lock_other.go); noExchangeWayOut,
// after the refusal of the exchange by a file system that does not exchange two directories; and
// mountPointWayOut, the way out of a spec directory that is a mount point.
const (
	noExchange       = "print changes its directory in one step with renameat2 (Linux) or renamex_np (macOS), which this system does not give; run print on Linux or macOS"
	noFlock          = "print locks its directory with flock, which this system does not give; run print on Linux or macOS"
	noExchangeWayOut = ". This file system does not exchange two directories, as some network and FUSE file systems do not: run print on a checkout on a local file system of Linux or macOS"
	mountPointWayOut = "mount a directory above it instead, such as the checkout (docker run -v <checkout>:/work, not -v <checkout>/.dispatch/spec:/work/spec), then run print again"
)

// exchangeRefusal is the error of writeDir when the system refuses the exchange with err. For each
// refusal that a writer meets, it names the cause and the way out: EXDEV, which overlayfs answers
// for a directory that comes from the layers of a container image; EBUSY, which the system answers
// for a directory that is a mount point; and EINVAL (Linux) and ENOTSUP (macOS), which a file
// system that does not exchange two directories answers. A copy of the directory in its place,
// made in the container, or a mount of the checkout, or of the directory above the mount point,
// lets print run there, since each puts the spec directory and the new directory beside it on one
// file system and under one parent that the exchange can move them in; a checkout on a local file
// system gives the exchange.
func exchangeRefusal(err error) error {
	var wayOut string
	switch {
	case errors.Is(err, syscall.EXDEV):
		wayOut = ". The file system refuses so to move a directory that comes from the layers of a container image (overlayfs): put a copy of the directory in its place in the container (cp -a <dir> <dir>.copy && rm -rf <dir> && mv <dir>.copy <dir>), or mount the checkout into the container, then run print again"
	case errors.Is(err, syscall.EBUSY):
		wayOut = ". The system refuses so to move a directory that is a mount point: " + mountPointWayOut
	case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOTSUP):
		wayOut = noExchangeWayOut
	}
	return fmt.Errorf("print changes its files in one step, by an exchange of the directory with a new directory that it builds beside it, and the system refused the exchange: %w%s", err, wayOut)
}

// removeNew removes staging, a new directory that the run built and did not exchange, which holds
// only files that the run made: each file, then staging, then its record where the run wrote one.
// What it cannot remove, the next run removes (cleanLeftovers).
func removeNew(fsys fileSystem, staging string) {
	list, _ := os.ReadDir(staging)
	for _, entry := range list {
		_ = fsys.remove(filepath.Join(staging, entry.Name()))
	}
	_ = fsys.remove(staging)
	removeRecord(fsys, staging)
}

// removeRecord removes the record of the run whose directory is staging, where there is one.
func removeRecord(fsys fileSystem, staging string) {
	if _, err := os.Lstat(recordPath(staging)); err == nil {
		_ = fsys.remove(recordPath(staging))
	}
}

// linkOrCopy puts at to the spec file from, which the run does not change and whose text is
// content: a hard link to it, the same file, or, where the system refuses the link (a file of
// another user under fs.protected_hardlinks, a file on another file system, a file at its bound of
// links, a file system with no hard links), a copy (copyFile).
func linkOrCopy(fsys fileSystem, from, to string, content []byte) error {
	refused := fsys.link(from, to)
	if refused == nil {
		return nil
	}
	if err := copyFile(fsys, from, to, content); err != nil {
		return fmt.Errorf("%w, and print could not copy the file either: %w", refused, err)
	}
	return nil
}

// copyFile writes content at to, a new file in the place of the spec file from, with the mode of
// from and, where the process may give them, its owner and its group (writeSynced). It refuses a
// from that is no regular file, such as a link that another program put in the place of the file
// after the run read it, so that no new file takes the mode of a link, 0777.
func copyFile(fsys fileSystem, from, to string, content []byte) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is no regular file now, though the run read it as one, so print does not take its mode: another program changed it; run print again", from)
	}
	return writeSynced(fsys, to, content, specMode(info), info, from)
}

// specMode is the mode of a spec file that print writes in the place of the file that info
// describes: its permission bits and its setuid, setgid and sticky bits.
func specMode(info fs.FileInfo) fs.FileMode {
	return info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// writeSynced writes content to a new file at path, made by fsys, syncs it, gives it the owner
// and the group of owner, the entry that it takes the place of, named of (sameOwner; nil for a
// file that keeps the writer's), then the mode mode, after the owner, since a change of the owner
// can clear the setuid and setgid bits. So a file that print writes keeps the mode of the file
// that it replaces, and its owner and group where the process may give them; a new file gets the
// owner and the group of the spec directory, where the process may give them. A run as root over
// a checkout of another user leaves that user's files; a run as a user writes as itself a file
// whose owner it may not give, as git does.
func writeSynced(fsys fileSystem, path string, content []byte, mode fs.FileMode, owner fs.FileInfo, of string) error {
	f, err := fsys.create(path)
	if err != nil {
		return err
	}
	err = fsys.write(f, content)
	if err == nil {
		err = fsys.sync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && owner != nil {
		err = sameOwner(fsys, path, "the new file "+filepath.Base(path), of, owner)
	}
	if err == nil {
		err = fsys.chmod(path, mode)
	}
	return err
}

// syncDir syncs the directory dir, so that the names it holds are on the disk.
func syncDir(fsys fileSystem, dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = fsys.sync(f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// finishOld finishes old, the directory that a run of print exchanged out of dir: it moves each
// entry that dir does not hold into dir, the same entry, and removes each entry of old that is a
// second name of the entry of its name in dir (a file, or a link that a stop between the two steps
// of moveByHand left under two names) and each spec file of old that dir holds in its new text,
// then old and the record of its run (recordPath). read is textsRead of the text of each spec file
// of dir when the run read it: a spec file of old is removed when read holds its text. The run
// passes what it read; a run that finishes a killed run (cleanLeftovers) passes the killed run's
// record, or nil where it has none, and then removes only second names. An entry that it can
// neither move nor remove, which another program made or changed in old while the run built the
// new directory, or whose move or removal the system refused, it keeps, with old under another
// name (keep), and it answers a line that gives each cause with its entries (keptCause), and kept
// true. Where it cannot give old another name, old keeps the name of print's own directory and its
// record, so the next run finishes it with the same record and removes no such change. It answers
// no line when it finished old.
func finishOld(fsys fileSystem, old, dir string, read map[string]string) (lines []string, kept bool) {
	list, err := os.ReadDir(old)
	if err != nil {
		return []string{fmt.Sprintf("print could not read %s, which held %s before the run, so it leaves it: %s", old, dir, err)}, false
	}
	var changed, refused []string
	var refusal error // the first refusal of the system of a move back or a removal
	refuse := func(name string, err error) {
		refused = append(refused, name)
		if refusal == nil {
			refusal = err
		}
	}
	for _, entry := range list {
		name := entry.Name()
		path, now := filepath.Join(old, name), filepath.Join(dir, name)
		current, err := os.Lstat(now)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			switch err := move(fsys, path, now); {
			case errors.Is(err, fs.ErrExist):
				changed = append(changed, name) // another program made the name in dir
			case err != nil:
				refuse(name, err)
			}
			continue
		case err != nil:
			refuse(name, err)
			continue
		}
		info, err := os.Lstat(path)
		switch {
		// A second name of the entry of dir: a file, or a link that a stop between the two steps
		// of moveByHand left under two names.
		case err == nil && !info.IsDir() && os.SameFile(info, current):
		// An entry of dir that holds nothing, an empty file or an empty directory, where old holds
		// an entry of that name: the name that a stop after moveByHand took it and before its
		// rename left, which this run finishes. Such an entry holds nothing, so a move over one
		// that another program made loses nothing either.
		case err == nil && holdsNothing(now, current) && fsys.remove(now) == nil:
			if err := move(fsys, path, now); err != nil {
				refuse(name, err)
			}
			continue
		case !entry.Type().IsRegular() || !current.Mode().IsRegular() || !removable(path, current, read, name):
			changed = append(changed, name)
			continue
		}
		if err := fsys.remove(path); err != nil {
			refuse(name, err)
		}
	}
	_, path, removed, err := leave(fsys, old, slices.Concat(changed, refused))
	if removed {
		return nil, false
	}
	cause := keptCause(changed, refused, refusal, read == nil)
	if err != nil {
		return []string{fmt.Sprintf("print could not remove %s, which held %s before the run: %s: %s", old, dir, cause, err)}, true
	}
	return []string{fmt.Sprintf("print kept %s, which held %s before the run: %s, so compare each with the entry of that name in %s",
		path, dir, cause, dir)}, true
}

// holdsNothing reports whether the entry at path, which info describes, holds nothing: an empty
// regular file or a directory with no entry. moveByHand takes the name of its move as such an
// entry, and a stop between that step and its rename leaves one (finishOld).
func holdsNothing(path string, info fs.FileInfo) bool {
	switch {
	case info.Mode().IsRegular():
		return info.Size() == 0
	case info.IsDir():
		list, err := os.ReadDir(path)
		return err == nil && len(list) == 0
	}
	return false
}

// keptCause is why finishOld kept an old directory, each cause once with its own entries: the
// entries that another program made or changed there while print ran, or, with no record of the
// run that left it (unread), the entries that print cannot tell from such a change; and the
// entries whose move back or removal the system refused, with the first refusal. An old directory
// that print could not remove though it kept no entry holds an entry that another program made
// after print read it.
func keptCause(changed, refused []string, refusal error, unread bool) string {
	var causes []string
	switch {
	case len(changed) > 0 && unread:
		causes = append(causes, "it holds "+strings.Join(changed, ", ")+", and print has no record of the texts that the run that left it read")
	case len(changed) > 0:
		causes = append(causes, "another program made or changed "+strings.Join(changed, ", ")+" there while print ran")
	}
	if len(refused) > 0 {
		causes = append(causes, fmt.Sprintf("the system refused print's move or removal of %s (%s)", strings.Join(refused, ", "), refusal))
	}
	if len(causes) == 0 {
		causes = append(causes, "another program made an entry there after print read it")
	}
	return strings.Join(causes, "; ")
}

// removable reports whether finishOld may remove the regular file path of an old directory, whose
// name in dir is the regular file that current describes: it is a second name of that file, or
// read holds its text, the text of name that the run read.
func removable(path string, current fs.FileInfo, read map[string]string, name string) bool {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return false
	case os.SameFile(info, current):
		return true
	}
	sum, held := read[name]
	content, err := os.ReadFile(path)
	return held && err == nil && textsRead(map[string][]byte{name: content})[name] == sum
}

// undoNew removes staging, a new directory that a killed run of print built beside dir and did not
// exchange, and its record: it holds only new files, second names of the spec files of dir, and the
// entry of its probe (probeName, probeMoved). An entry that it cannot remove, it keeps, with
// staging under another name (keep), and answers a line that names it and where it kept it, and
// kept true. It answers no line when it removed staging.
func undoNew(fsys fileSystem, staging, dir string) (lines []string, kept bool) {
	list, err := os.ReadDir(staging)
	if err != nil {
		return []string{fmt.Sprintf("print could not read %s, so it leaves it: %s", staging, err)}, false
	}
	var names []string
	for _, entry := range list {
		name := entry.Name()
		if !entry.Type().IsRegular() && name != probeName && name != probeMoved || fsys.remove(filepath.Join(staging, name)) != nil {
			names = append(names, name)
		}
	}
	what, path, removed, err := leave(fsys, staging, names)
	switch {
	case removed:
		return nil, false
	case err != nil:
		return []string{fmt.Sprintf("print could not remove %s, which a killed run of print left beside %s, and holds %s, which print did not make: %s", staging, dir, what, err)}, true
	}
	return []string{fmt.Sprintf("print kept %s, which a killed run of print left beside %s: it holds %s, which print did not make", path, dir, what)}, true
}

// leave removes left, a directory that a run of print built or exchanged out of the spec
// directory, and its record, when names, the entries of left that print could neither move nor
// remove, is empty, and answers removed. Else it keeps left under another name (keep), and removes
// its record once it kept it; it answers what, which names those entries ("an entry" for one made
// in left after the run read it, so left was not empty), the name that it kept left under, and the
// refusal of keep.
func leave(fsys fileSystem, left string, names []string) (what, kept string, removed bool, err error) {
	if len(names) == 0 && fsys.remove(left) == nil {
		removeRecord(fsys, left)
		return "", "", true, nil
	}
	what = strings.Join(names, ", ")
	if what == "" {
		what = "an entry"
	}
	if kept, err = keep(fsys, left); err == nil {
		removeRecord(fsys, left)
	}
	return what, kept, false, err
}

// keep gives path, a directory that print leaves, a name that cleanLeftovers does not take as its
// own, and answers that name.
func keep(fsys fileSystem, path string) (string, error) {
	kept := path + "-kept-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	return kept, move(fsys, path, kept)
}

// move renames from to to, and refuses when an entry has the name to: a rename that keeps an
// existing name (moveNoReplace), or, where the system answers that it has no such rename (EINVAL,
// ENOSYS or ENOTSUP), a move that refuses the name another way (moveByHand). A pod's gVisor volume
// (v9fs) is such a system: it exchanges two directories with renameat2 and RENAME_EXCHANGE, and
// answers EINVAL for RENAME_NOREPLACE.
func move(fsys fileSystem, from, to string) error {
	err := fsys.moveNoReplace(from, to)
	if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	if byHand := moveByHand(fsys, from, to); byHand != nil {
		return fmt.Errorf("%w, and a move that refuses the name another way failed too: %w", err, byHand)
	}
	return nil
}

// moveByHand renames from to to, and refuses an entry of the name to, with no rename that keeps an
// existing name: a file or a link by a second name, which the system refuses where an entry has
// the name, and then a removal of the first name; a directory, or a file of a system that refuses
// a second name, by taking the name first, an empty directory for a directory and a new file for a
// file, which the system refuses where an entry has the name, and a rename over what it made. The
// rename replaces an entry of its own kind, so print takes the name before it renames: a check of
// the name and then a rename would replace an entry that another program made between the two. The
// caller holds the lock of the spec directory, so no run of print makes that name meanwhile.
func moveByHand(fsys fileSystem, from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		switch err := fsys.link(from, to); {
		case err == nil:
			return fsys.remove(from)
		case errors.Is(err, fs.ErrExist):
			return err
		}
	}
	if info.IsDir() {
		err = fsys.mkdir(to, 0o700)
	} else if f, made := fsys.create(to); made == nil {
		err = f.Close()
	} else {
		err = made
	}
	if err != nil {
		return err
	}
	if err := fsys.rename(from, to); err != nil {
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			_ = fsys.remove(to) // the entry that this move made, which it did not rename over
		}
		return err
	}
	return nil
}

// probeName and probeMoved are the names of the entry that probeMove makes in the new directory
// of a run, and moves; no spec file has such a name, and a killed run's new directory may hold
// one, a file or an empty directory, which undoNew removes.
const (
	probeName  = ".probe"
	probeMoved = ".probe-moved"
)

// probeMove makes a file and then a directory in staging and moves each to another name there,
// as the run moves each entry that is no spec file back into the spec directory after the exchange
// (move), then removes it. A system that refuses such a move refuses the run here, before the
// record and the exchange change anything, with its refusal and the way out: after the exchange,
// such a refusal would leave those entries beside the spec directory.
func probeMove(fsys fileSystem, staging string) error {
	from, to := filepath.Join(staging, probeName), filepath.Join(staging, probeMoved)
	for _, directory := range []bool{false, true} {
		var err error
		if directory {
			err = fsys.mkdir(from, 0o700)
		} else if f, created := fsys.create(from); created != nil {
			err = created
		} else {
			err = f.Close()
		}
		if err == nil {
			err = move(fsys, from, to)
		}
		if err != nil {
			return fmt.Errorf("print moves each entry of the directory that is no spec file back into it after the exchange, and the system refused such a move (%w): run print on a checkout on a local file system of Linux or macOS", err)
		}
		if err := fsys.remove(to); err != nil {
			return err
		}
	}
	return nil
}

// cleanLeftovers finishes each directory that a killed run of print left beside dir, whose inode
// is ino, and answers a line for each, and whether it kept an entry beside dir that a person must
// compare with dir. The caller holds the lock of dir, and a run holds that lock from before it
// makes its directory beside dir until it has removed it, so each such directory is a dead run's.
// A directory is print's own when its name is the name that stagingPath gives and the inode in that
// name is the inode of dir, then it is a new directory that the run did not exchange (undoNew), or
// its own inode, then it is the old directory that the run exchanged out of dir, which it finishes
// with the record of that run (finishOld, readRecord); the line says that print removed or finished
// it, or, where it could not, what it left and where. Each other entry of that form it passes over
// with a line that says so. A record whose directory is gone, which a run killed after it finished
// that directory left, it removes.
func cleanLeftovers(fsys fileSystem, dir string, ino uint64) (lines []string, kept bool) {
	parent, prefix := filepath.Dir(dir), "."+filepath.Base(dir)+stagingInfix
	list, err := os.ReadDir(parent)
	if err != nil {
		return []string{fmt.Sprintf("print could not read %s, so it cannot finish what a killed run of print left there: %s", parent, err)}, false
	}
	for _, entry := range list {
		rest, ok := strings.CutPrefix(entry.Name(), prefix)
		rest, record := strings.CutSuffix(rest, recordSuffix)
		made, err := strconv.ParseUint(rest, 10, 64)
		if !ok || err != nil || strconv.FormatUint(made, 10) != rest {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if record {
			staging := strings.TrimSuffix(path, recordSuffix)
			if _, err := os.Lstat(staging); errors.Is(err, fs.ErrNotExist) && entry.Type().IsRegular() {
				if _, err := os.Lstat(path); err == nil {
					if err := fsys.remove(path); err != nil {
						lines = append(lines, fmt.Sprintf("print could not remove %s, the record of a killed run of print on %s: %s", path, dir, err))
					}
				}
			}
			continue
		}
		info, err := entry.Info()
		switch {
		case err != nil || !entry.IsDir():
			lines = append(lines, fmt.Sprintf("print passes over %s: it has the name of a directory that print builds beside %s, and it is no directory", path, dir))
		case made == ino:
			left, k := undoNew(fsys, path, dir)
			if len(left) == 0 {
				left = []string{fmt.Sprintf("print removed %s, a new directory that a killed run of print built and did not put in the place of %s", path, dir)}
			}
			lines, kept = append(lines, left...), kept || k
		case made == inode(info):
			left, k := finishOld(fsys, path, dir, readRecord(path))
			if len(left) == 0 {
				left = []string{fmt.Sprintf("print finished %s, the directory that a killed run of print put out of the place of %s: it moved each entry that is no spec file into %s", path, dir, dir)}
			}
			lines, kept = append(lines, left...), kept || k
		default:
			lines = append(lines, fmt.Sprintf("print passes over %s: it has the name of a directory that print builds beside %s, and no run of print on %s made it", path, dir, dir))
		}
	}
	return lines, kept
}
