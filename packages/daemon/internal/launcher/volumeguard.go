package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The three guards the cache trimmer asks before it deletes anything: what the pod's processes
// are using, how old an entry is, and whether an entry is the kind of file its rule names. They
// are written and tested before the sweep that calls them, because a sweep without them deletes
// a role's work: the pod gives six roles one volume, the roles write into the same four cache
// roots, and nothing on the volume says which role owns what.

// usedPaths is what every process of the pod is using, read once.
//
// The paths are one sorted list tested by prefix rather than a set tested per entry: a sweep
// asks about thousands of cache entries, and the pod's processes hold thousands of paths, so the
// cost is one pass over the processes and one binary search per entry instead of the product of
// the two. A path on the entry itself counts as much as one under it, because a `flock` is held
// on a directory and not on a file in it, and because an executing binary's own path is the
// entry (Covers).
type usedPaths struct {
	paths   []string // sorted
	Refused []string // one entry per process whose /proc the kernel refused, naming it
}

// readUsedPaths reads procRoot, leaving self's own process out.
//
// Every process is read through six entries, because no one of them is enough. An open file
// descriptor misses a running program's own binary and every library it mapped, which is exactly
// the Chromium case: the browser holds no descriptor for the executable under the pod's browser
// registry it is running from, and `exe` and `maps` are what name it. `cwd` and each thread's
// `task/<tid>/cwd` name a directory a build is running in, which holds no descriptor either, and
// a process's threads can sit in different directories from the process's own. `root` names a
// directory a chrooted process is confined to.
//
// The trimmer's own process is left out, and that is why self is an argument rather than read
// here: listing a root and removing an entry both hold a handle under a root, and a rule that
// read the trimmer's own handle on a root as a live use would make the sweep skip everything it
// was about to delete. procRoot is an argument for the same reason — a test drives a fixture
// /proc of its own — and the trimmer passes "/proc" and os.Getpid().
//
// A read the kernel refuses names the process it could not read and is carried back in Refused
// rather than dropped: a refused process is one whose open files are unknown, and the caller
// decides what that is worth. The sweep deletes nothing when the first read of a sweep was
// refused and ends at the next entry boundary when a later one was. A process that exited while
// this pass read it is no refusal — its paths are gone with it — so ENOENT and ESRCH are passed
// over. At most one refusal is recorded per process: a process whose /proc directory the kernel
// refuses refuses every entry of it, and six lines would say one thing.
func readUsedPaths(procRoot string, self int) usedPaths {
	used := usedPaths{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		// Not one process but all of them: the reading of /proc itself failed, so nothing is
		// known about any process of the pod. It is reported in the same list because it is the
		// same decision for the caller, and a sweep that read no process at all must delete
		// nothing.
		used.Refused = append(used.Refused, fmt.Sprintf("%s: %v", procRoot, err))
		return used
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		process := procProcess{dir: filepath.Join(procRoot, entry.Name()), pid: pid}
		process.read()
		used.paths = append(used.paths, process.paths...)
		if process.refusal != "" {
			used.Refused = append(used.Refused, process.refusal)
		}
	}
	slices.Sort(used.paths)
	used.paths = slices.Compact(used.paths)
	return used
}

// Covers is whether any process holds path itself or something under it.
//
// Both halves are one binary search each, which the sorted list is for: a path the list holds
// exactly sits where path sorts, and every path under it sits where path+"/" sorts, since
// strings with that prefix are contiguous in sorted order. A process holding an ancestor of path
// — a role whose working directory is the cache home — is not a use of path: the sweep would
// otherwise stop at the first role that stood in a cache root.
func (u usedPaths) Covers(path string) bool {
	path = filepath.Clean(path)
	if _, held := slices.BinarySearch(u.paths, path); held {
		return true
	}
	under := path + string(filepath.Separator)
	at, _ := slices.BinarySearch(u.paths, under)
	return at < len(u.paths) && strings.HasPrefix(u.paths[at], under)
}

// Holds is whether any process holds path itself, which Covers cannot be asked: Covers answers
// yes for a path something under it is in use, which of a reclaimable root is true the moment
// one role has one cache entry open.
//
// The sweep asks it of each root. A process holding the root's own inode is a tool that took the
// `flock` every rendered install takes on its cache root for the length of the install
// (installBlock and claudeCodeBlock, internal/runtime/sandbox/plugins.go), and the entries under
// it are the ones that install is filling, so the whole root is left alone until it lets go.
func (u usedPaths) Holds(path string) bool {
	_, held := slices.BinarySearch(u.paths, filepath.Clean(path))
	return held
}

// procProcess is one process of the pod as this pass reads it: the paths it was found to use and
// the first /proc read of it the kernel refused.
type procProcess struct {
	dir     string
	pid     int
	paths   []string
	refusal string
}

// read fills the process's paths from its six /proc entries.
func (p *procProcess) read() {
	for _, name := range []string{"cwd", "exe", "root"} {
		p.link(name, filepath.Join(p.dir, name))
	}
	p.descriptors()
	p.mapped()
	p.threads()
}

// link adds what one /proc symbolic link names.
func (p *procProcess) link(what, path string) {
	target, err := os.Readlink(path)
	if err != nil {
		p.refuse(what, err)
		return
	}
	p.add(target)
}

// descriptors adds every file the process has open. A descriptor closed between the listing and
// the reading of its link is gone rather than refused (refuse).
func (p *procProcess) descriptors() {
	dir := filepath.Join(p.dir, "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.refuse("fd", err)
		return
	}
	for _, entry := range entries {
		p.link("fd/"+entry.Name(), filepath.Join(dir, entry.Name()))
	}
}

// mapped adds every file the process has mapped: its own executable, every library it loaded and
// every file it mapped by hand, none of which it need hold a descriptor for. A mapping's path is
// the sixth field of its line and may itself hold spaces, so the five fields before it are
// stepped over rather than the line split, and the anonymous mappings ([heap], [stack], an empty
// field) are left out by the absolute-path rule in add.
//
// The body is cut line by line rather than split into one: the used-path set is read again every
// second for the whole of a sweep, and a browser or a node process maps thousands of regions,
// nearly all anonymous and dropped at once. Cutting leaves every line a slice of the one buffer,
// so the pass allocates that buffer and the paths it keeps and nothing per line.
func (p *procProcess) mapped() {
	body, err := os.ReadFile(filepath.Join(p.dir, "maps"))
	if err != nil {
		p.refuse("maps", err)
		return
	}
	for rest := string(body); rest != ""; {
		line, after, _ := strings.Cut(rest, "\n")
		rest = after
		for range 5 {
			line = strings.TrimLeft(line, " \t")
			end := strings.IndexAny(line, " \t")
			if end < 0 {
				line = ""
				break
			}
			line = line[end:]
		}
		p.add(strings.TrimLeft(line, " \t"))
	}
}

// threads adds each thread's working directory, which the process's own `cwd` does not cover: a
// thread may chdir on its own, and a program's worker threads routinely do.
func (p *procProcess) threads() {
	dir := filepath.Join(p.dir, "task")
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.refuse("task", err)
		return
	}
	for _, entry := range entries {
		p.link("task/"+entry.Name()+"/cwd", filepath.Join(dir, entry.Name(), "cwd"))
	}
}

// add keeps an absolute path and drops everything else: a descriptor on a pipe, a socket or an
// anonymous inode reads as `pipe:[12345]` and names no file. A target the kernel marks
// ` (deleted)` is kept as it reads, and so matches nothing the sweep lists, which is right: the
// name is already unlinked, so no entry of that name is there to protect.
func (p *procProcess) add(target string) {
	if !strings.HasPrefix(target, "/") {
		return
	}
	p.paths = append(p.paths, filepath.Clean(target))
}

// refuse records the first read of this process the kernel refused. A process that exited under
// the pass is no refusal: the kernel answers ENOENT for its whole directory and ESRCH for a read
// of a file in it, and its paths went with it.
func (p *procProcess) refuse(what string, err error) {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) || p.refusal != "" {
		return
	}
	p.refusal = fmt.Sprintf("process %d: %s: %v", p.pid, what, err)
}

// newestModTime is the newest modification time anywhere in name, which dir holds: name's own
// for a file, and the newest in its tree for a directory.
//
// A directory's own time changes only when its entries are added or removed, so a build writing
// into a file inside a `go-build` tree leaves the tree's own time as old as the directory
// creation that made it. Judging such a tree by its own time would delete the cache a live build
// is writing, which is the one deletion that costs more than the sweep saves.
//
// This and the two guards below read through an open handle on the directory the sweep listed
// (dirHandle) and name the entry inside it, never by a path of their own: a path string is
// resolved again at every call, and the directories above a cache root are the role's own to
// rename, so two reads of one string are two chances for a role to put a symbolic link in it
// and have the second read answer for somewhere else (sweepDir, volumetrim.go). Nothing below
// the entry is named by a path either: the tree is walked by opening one element at a time
// (walkEntries), where fs.WalkDir over a Root's FS named each entry by its path below the root
// and had os.Root resolve every element of it but the last with the open this kernel follows a
// link under (volumedir.go).
func newestModTime(dir *dirHandle, name string) (time.Time, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return time.Time{}, err
	}
	newest := info.ModTime()
	if !info.IsDir() {
		return newest, nil
	}
	err = walkEntries(dir, name, func(info os.FileInfo) error {
		if at := info.ModTime(); at.After(newest) {
			newest = at
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return newest, nil
}

// newerThan stops at the first file under name, which dir holds, newer than cutoff, which is
// all the pre-delete re-read needs: a second full walk of a go-build tree would cost as much as
// the listing. It reads through dir's handle and follows no symbolic link, as newestModTime
// above does and for the reasons given there.
func newerThan(dir *dirHandle, name string, cutoff time.Time) (bool, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return false, err
	}
	if info.ModTime().After(cutoff) {
		return true, nil
	}
	if !info.IsDir() {
		return false, nil
	}
	newer := false
	err = walkEntries(dir, name, func(info os.FileInfo) error {
		if info.ModTime().After(cutoff) {
			newer = true
			return errWalkDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errWalkDone) {
		return false, err
	}
	return newer, nil
}

// walkEntries calls visit for every entry of the tree name holds, which dir holds, depth first,
// and stops where a visit answers errWalkDone. Every level is opened by one name in the level
// above and listed through that descriptor, and the time and the kind a visit reads are the
// lstat that listing made through it (dirHandle), so a link inside a tree is read as the link
// it is and the tree it points into is never walked, exactly as filepath.WalkDir read it.
//
// It holds one handle per level of the tree, which is two descriptors, until that level is
// done. A cache entry is shallow — Go's own build trees and bun's install cache are two or
// three levels — and the pod's limit is 1048576 descriptors (`ulimit -n`, measured in an issue
// pod of this deployment at 2026-11-12T06:30Z); a level the kernel refuses for that or any
// other reason is the error the guard answers with, and the entry is passed over.
func walkEntries(dir *dirHandle, name string, visit func(info os.FileInfo) error) error {
	under, err := dir.open(name)
	if err != nil {
		return err
	}
	defer func() { _ = under.Close() }()
	entries, err := under.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// The entry was removed between the listing and the stat: a cache is a live
			// directory, and an entry that is gone holds nothing back.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if err := visit(info); err != nil {
			return err
		}
		if entry.IsDir() {
			if err := walkEntries(under, entry.Name(), visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// errWalkDone ends a walk from inside a visit, as fs.SkipAll ended the walk this replaced.
var errWalkDone = errors.New("the walk has what it was asked for")

// entryKindOK reads name, which dir holds, with lstat and reports whether it is the kind of
// file rule names, with the reason it is not when it is not.
//
// Every entry is read this way and no symbolic link is ever followed, so what the trimmer
// deletes is what it looked at. Two things this passes over are known to appear on a pod's
// volume: the regular file an interrupted `flock` leaves where a cache directory belongs,
// which the rendered install scripts themselves work around (`[ -d "$cache" ] || rm -f --
// "$cache"`, installBlock, internal/runtime/sandbox/plugins.go), and a link a role left in a
// cache root, which a trimmer that followed links would delete into the workspace through. Both
// are reported rather than silently skipped: a cache root that fills with them is a pod the
// trimmer cannot hold under its mark, and the operator must be told why.
//
// The read goes through dir's handle, as the two guards above do: the lstat is of a name in the
// directory the sweep opened, so the kind this answers for is the kind of the inode the removal
// reaches (newestModTime, and remove in volumetrim.go).
func entryKindOK(dir *dirHandle, name string, rule EntryRule) (ok bool, why string, err error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return false, "", err
	}
	mode := info.Mode()
	var wanted string
	switch rule {
	case GoBuildTrees, ImmediateChildren:
		if mode.IsDir() {
			return true, "", nil
		}
		wanted = "a directory"
	case GoCacheEntries:
		// Go's own trim deletes both forms of an entry: a file with os.Remove and the directory
		// form with RemoveAll (trimSubdir, cmd/go/internal/cache/cache.go).
		if mode.IsRegular() || mode.IsDir() {
			return true, "", nil
		}
		wanted = "a regular file or a directory"
	default:
		return false, fmt.Sprintf("no entry rule named %q", string(rule)), nil
	}
	why = fmt.Sprintf("%s names %s, and this is %s", rule, wanted, kindOf(mode))
	if mode&fs.ModeSymlink != 0 {
		why += ", which the trimmer never follows"
	}
	return false, why, nil
}

// kindOf is what lstat read, named as the reason of entryKindOK reads it.
func kindOf(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode.IsDir():
		return "a directory"
	case mode.IsRegular():
		return "a regular file"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0:
		return "a device"
	default:
		return "a file of mode " + mode.String()
	}
}
