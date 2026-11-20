package launcher

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
)

// The three guards the cache trimmer asks before it deletes anything: what the pod's processes
// are using, how old an entry is, and whether an entry is the kind of file its rule names. They
// are written and tested before the sweep that calls them, because a sweep without them deletes
// a role's work: the pod gives six roles one volume, the roles write into the same four cache
// roots, and nothing on the volume says which role owns what.

// usedPaths is what every process of the pod is using under the roots the sweep asks about,
// read once.
//
// The paths are one sorted list tested by prefix rather than a set tested per entry: a sweep
// asks about thousands of cache entries, and the pod's processes hold thousands of paths, so the
// cost is one pass over the processes and one binary search per entry instead of the product of
// the two. A path on the entry itself counts as much as one under it, because a `flock` is held
// on a directory and not on a file in it, and because an executing binary's own path is the
// entry (Covers).
type usedPaths struct {
	paths []string // sorted
	// held is every root a process holds a descriptor open on, sorted: the one kind of path a
	// `flock` on a root is, kept apart from paths because a root is held by nothing else
	// (Holds).
	held []string
	// Refused is one entry per process whose /proc the kernel refused, naming it, or one entry
	// naming /proc itself when the list of processes could not be read at all.
	Refused []string
}

// readUsedPaths reads procRoot, leaving the sweep's own processes out, and keeps the paths that
// are one of under or lie below one. It lists procRoot through hostproc.PIDs, so in a test a
// procRoot that is a /proc of the machine ends the test binary unless the caller gave reason: the
// trimmer gives none, and its tests give it a fixture /proc of their own.
//
// Every process is read through six entries, because no one of them is enough. An open file
// descriptor misses a running program's own binary and every library it mapped, which is exactly
// the Chromium case: the browser holds no descriptor for the executable under the pod's browser
// registry it is running from, and `exe` and `maps` are what name it. `cwd` and each thread's
// `task/<tid>/cwd` name a directory a build is running in, which holds no descriptor either, and
// a process's threads can sit in different directories from the process's own. `root` names a
// directory a chrooted process is confined to.
//
// under is the reclaimable roots, which the trimmer passes, and a path anywhere else is dropped
// as it is read: the guard is asked only whether a root is held (Holds) and whether an entry
// under a root is used (Covers), and no path outside every root answers either. The set is read
// again as often as once a second while a sweep deletes, and what a process maps and holds open
// is the role's to choose, so the pass keeps no more of it than those two questions can match:
// the libraries every process maps and the files of a workspace are none of them.
//
// The sweep's own processes are left out, and that is why own is an argument rather than read
// here: listing a root and removing an entry both hold a handle under a root, and a rule that
// read the sweep's own handle on a root as a live use would make the sweep skip everything it
// was about to delete. They are the trimmer's process and, once a sweep has started its deleter,
// that child too, which holds every directory the listing opened for as long as the sweep runs
// (startContainedRemover, volumedelete.go). Read as a use, the child was a hold on the root it
// deletes from at the first refresh of the set (volumeUsedPathsMaxAge, sweep): a sweep of one
// root of 2000 entries deleted none of them, on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3
// (TestASweepsOwnDeleterIsNoHoldOnTheRootsItDeletesFrom). procRoot is an argument for the same
// reason — a test drives a fixture /proc of its own — and the trimmer passes "/proc" and
// os.Getpid(), and its deleter's pid too from the first refresh, which is the first reading
// taken with a deleter running.
//
// A read the kernel refuses names the process it could not read and is carried back in Refused
// rather than dropped: a refused process is one whose open files are unknown, and the caller
// decides what that is worth. The sweep deletes nothing when the first read of a sweep was
// refused and ends at the next entry boundary when a later one was. A process that exited while
// this pass read it is no refusal — its paths are gone with it — so ENOENT and ESRCH are passed
// over. At most one refusal is recorded per process: a process whose /proc directory the kernel
// refuses refuses every entry of it, and six lines would say one thing.
func readUsedPaths(procRoot string, reason hostproc.Reason, under []string, own ...int) usedPaths {
	used := usedPaths{}
	pids, err := hostproc.PIDs("the cache trimmer's used-path read", procRoot, reason)
	if err != nil {
		// Not one process but all of them: the reading of /proc itself failed, so nothing is
		// known about any process of the pod. It is reported in the same list because it is the
		// same decision for the caller, and a sweep that read no process at all must delete
		// nothing.
		used.Refused = append(used.Refused, fmt.Sprintf("%s: %v", procRoot, err))
		return used
	}
	lines := bufio.NewReaderSize(nil, procMapsReadBytes)
	for _, pid := range pids {
		if slices.Contains(own, pid) {
			continue
		}
		process := procProcess{dir: filepath.Join(procRoot, strconv.Itoa(pid)), pid: pid, under: under, lines: lines}
		process.read()
		used.paths = append(used.paths, process.paths...)
		used.held = append(used.held, process.held...)
		if process.refusal != "" {
			used.Refused = append(used.Refused, process.refusal)
		}
	}
	slices.Sort(used.paths)
	used.paths = slices.Compact(used.paths)
	slices.Sort(used.held)
	used.held = slices.Compact(used.held)
	return used
}

const (
	// procMapsReadBytes is the buffer one pass reads every process's maps through, a line at a
	// time, and the most of one line it holds (mapped). A line is five short fields and a path,
	// and a path whose line is longer than this is far deeper than any entry the sweep asks
	// about: an entry is one name in a root or in a shard of one, and a name is at most NAME_MAX,
	// 255 bytes.
	procMapsReadBytes = 4096
	// procNamesBatch is how many names of a process's fd or task directory are read at a time
	// (eachName), so that a process holding a great many descriptors costs the pass a batch of
	// names at a time rather than a list of all of them.
	procNamesBatch = 256
)

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

// Holds is whether any process holds a descriptor open on path itself, which Covers cannot be
// asked: Covers answers yes for a path something under it is in use, which of a reclaimable root
// is true the moment one role has one cache entry open.
//
// The sweep asks it of each root, and it counts an open descriptor on the root and nothing else,
// because that is what a `flock` on the root holds: the rendered install takes one on bun's
// install cache root for the length of the install (installBlock,
// internal/runtime/sandbox/plugins.go), the entries under it are the ones that install is
// filling, and the whole root is left alone until it lets go. A process standing in the root —
// its working directory or a thread's, or its root directory — holds no lock on it: a role's
// bash keeps one shell for its whole session, and one that ran `cd "$TMPDIR"` would otherwise
// keep every Go build tree of the pod from being swept for as long as it stood there
// (TestAProcessStandingInARootIsNoHoldOnIt). What such a process stands in under a root is still
// a use of the entry it lies in (Covers).
func (u usedPaths) Holds(path string) bool {
	_, held := slices.BinarySearch(u.held, filepath.Clean(path))
	return held
}

// procProcess is one process of the pod as this pass reads it: the paths it was found to use
// under the roots it is given, the roots it holds a descriptor open on, and the first /proc read
// of it the kernel refused. lines is the pass's one reader its maps are read through.
type procProcess struct {
	dir     string
	pid     int
	under   []string
	lines   *bufio.Reader
	paths   []string
	held    []string
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

// link adds what one /proc symbolic link names, and answers it as add kept it.
func (p *procProcess) link(what, path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		p.refuse(what, err)
		return ""
	}
	return p.add(target)
}

// descriptors adds every file the process has open, and keeps a descriptor on a root itself as
// a hold of that root (Holds), once per root however many descriptors the process holds on it,
// so held grows by at most one entry per root for each process. A descriptor closed between the
// listing and the reading of its link is gone rather than refused (refuse).
func (p *procProcess) descriptors() {
	dir := filepath.Join(p.dir, "fd")
	if err := eachName(dir, func(name string) {
		if kept := p.link("fd/"+name, filepath.Join(dir, name)); slices.Contains(p.under, kept) && !slices.Contains(p.held, kept) {
			p.held = append(p.held, kept)
		}
	}); err != nil {
		p.refuse("fd", err)
	}
}

// mapped adds every file the process has mapped: its own executable, every library it loaded and
// every file it mapped by hand, none of which it need hold a descriptor for. A mapping's path is
// the sixth field of its line and may itself hold spaces, so the five fields before it are
// stepped over rather than the line split, and the anonymous mappings ([heap], [stack], an empty
// field) are left out by the absolute-path rule in add.
//
// The file is read a line at a time through the pass's one buffer, and a line becomes a string
// only when its path, as the kernel wrote it, is one of the roots or lies under one: the set is
// read again as often as once a second while a sweep deletes, and a browser or a node process
// maps thousands of regions, nearly all of them libraries and anonymous memory that no root
// holds. A line longer than the buffer (procMapsReadBytes) is taken as far as the buffer holds
// it, cut back to its last whole name, and the rest of it is read past: that much of a path
// already lies under every entry the whole path lies under, since an entry's path is far shorter,
// so the guard answers for it as it would for the whole path, and a role that maps a file deep in
// a tree costs the pass no more than the buffer.
func (p *procProcess) mapped() {
	file, err := os.Open(filepath.Join(p.dir, "maps"))
	if err != nil {
		p.refuse("maps", err)
		return
	}
	defer func() { _ = file.Close() }()
	p.lines.Reset(file)
	defer p.lines.Reset(nil)
	for {
		line, err := p.lines.ReadSlice('\n')
		long := errors.Is(err, bufio.ErrBufferFull)
		p.mappedLine(line, long)
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = p.lines.ReadSlice('\n')
		}
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			p.refuse("maps", err)
			return
		}
	}
}

// mappedLine adds the path of one line of maps, which is the buffer's own bytes and is read
// before the buffer is read into again. cut is a line longer than the buffer, whose path is kept
// up to its last whole name (mapped).
func (p *procProcess) mappedLine(line []byte, cut bool) {
	if end := len(line) - 1; end >= 0 && line[end] == '\n' {
		line = line[:end]
	}
	for range 5 {
		line = bytes.TrimLeft(line, " \t")
		end := bytes.IndexAny(line, " \t")
		if end < 0 {
			return
		}
		line = line[end:]
	}
	path := bytes.TrimLeft(line, " \t")
	if cut {
		at := bytes.LastIndexByte(path, '/')
		if at <= 0 {
			return
		}
		path = path[:at]
	}
	if p.keepsBytes(path) {
		p.add(string(path))
	}
}

// threads adds each thread's working directory, which the process's own `cwd` does not cover: a
// thread may chdir on its own, and a program's worker threads routinely do.
func (p *procProcess) threads() {
	dir := filepath.Join(p.dir, "task")
	if err := eachName(dir, func(name string) {
		p.link("task/"+name+"/cwd", filepath.Join(dir, name, "cwd"))
	}); err != nil {
		p.refuse("task", err)
	}
}

// eachName calls each for every name in dir, read procNamesBatch names at a time.
func eachName(dir string, each func(name string)) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	for {
		names, err := file.Readdirnames(procNamesBatch)
		for _, name := range names {
			each(name)
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// add keeps an absolute path that is one of the roots or lies under one, and drops everything
// else: a descriptor on a pipe, a socket or an anonymous inode reads as `pipe:[12345]` and names
// no file, and a file outside every root is none the guard is asked about (readUsedPaths). A
// target the kernel marks ` (deleted)` is kept as it reads: it is the path of no entry, since
// that name is already unlinked, and it is a use of every entry it lies under, whose tree the
// process still holds an inode of. It answers the path as it kept it, and "" for one it dropped.
func (p *procProcess) add(target string) string {
	if !strings.HasPrefix(target, "/") {
		return ""
	}
	target = filepath.Clean(target)
	for _, root := range p.under {
		if strings.HasPrefix(target, root) && (len(target) == len(root) || target[len(root)] == '/') {
			p.paths = append(p.paths, target)
			return target
		}
	}
	return ""
}

// keepsBytes is whether path, a line's path as the kernel wrote it, is one of the roots or lies
// under one: add's test, asked of the line's own bytes before any string is made of them.
func (p *procProcess) keepsBytes(path []byte) bool {
	for _, root := range p.under {
		if len(path) >= len(root) && string(path[:len(root)]) == root && (len(path) == len(root) || path[len(root)] == '/') {
			return true
		}
	}
	return false
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
//
// halt is asked before every directory of the tree is opened and again before every batch of a
// directory's names after its first (walkEntries), and the first error it answers ends the walk
// and is this one's: the listing passes the sweep's three endings (trimmer.list), since one
// entry a role made deep or wide would otherwise hold the listing, and the sweep with it, past
// the budget and the container's stop for as long as its walk takes. A file has no tree to
// walk, so its one lstat asks nothing.
func newestModTime(dir *dirHandle, name string, halt func() error) (time.Time, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return time.Time{}, err
	}
	newest := info.ModTime()
	if !info.IsDir() {
		return newest, nil
	}
	err = walkEntries(dir, name, halt, func(info os.FileInfo) error {
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
//
// halt is asked as newestModTime asks it, before every directory of the tree is opened and
// before every batch of a directory's names after its first, and for the same reason: between
// the listing and this re-read a role may grow the entry into a tree as deep or wide as it likes
// and set its times back, and a walk that asked nothing would hold the sweep past its budget and
// the container's stop for as long as that tree took to read. The re-read comes before the
// entry's removal begins (trimmer.remove), so an ending it meets leaves the entry whole and ends
// the sweep there.
func newerThan(dir *dirHandle, name string, cutoff time.Time, halt func() error) (bool, error) {
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
	err = walkEntries(dir, name, halt, func(info os.FileInfo) error {
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
// it is and the tree it points into is never walked, exactly as filepath.WalkDir read it. halt,
// where there is one, is asked before each directory is opened and again before each batch of
// its names after the first, and the walk ends with the first error it answers.
//
// A level is listed walkEntriesBatch names at a time, as eachName reads a process's
// descriptors, and the names of one batch are visited in the order they sort, which for a
// directory of no more names than that is the order every name of it sorts in (nextBatch). A
// level of N names costs the walk a batch of them and not all N: a role may make a cache entry
// with a directory of any number of names, and the walk runs in PID 1 on every sweep. For the
// same reason halt is asked between the batches and not only before the directory is opened: a
// walk that asked only there read a directory it had opened to its end, however long the stop
// had been asked for (TestTheGuardWalkAsksItsStopBetweenTheBatchesOfOneDirectory).
//
// It holds one handle per level of the tree until that level is done, which is three
// descriptors — the handle's two and the listing's own — and a batch of names, and no path: a
// handle keeps the one above it and its own name (dirHandle.path). A cache entry is shallow —
// Go's own build trees and bun's install cache are two or three levels — but a role may make one
// of any depth, and every descriptor the walk holds counts against the one limit every open of
// PID 1 shares, a role's start among them. So it descends no more than volumeWalkDepth levels
// below name, as the size walk descends below a target, and holds sixty-five levels open at
// most: a directory deeper than that is not opened, and is the error the guard answers with
// (errWalkTooDeep), as a level the kernel refuses for any reason is. Either way the entry's age
// is not read, and the sweep passes the entry over for this sweep and deletes nothing of it
// (trimmer.list, trimmer.remove).
//
// That error names what failed from name, as the deleter's removal names what it failed on from
// the entry (treeRemoval.remove): name itself where its own open or listing failed, and
// otherwise name, the name that failed and how many levels below name it lay (removalFailure),
// lifted a level at a time on the way back up (under). No level builds the path above it, so
// the line the sweep writes of an entry whose age could not be read is of a size set by two
// names and not by the depth a role nested the entry to
// (TestAnAgeThatCannotBeReadIsLoggedAtASizeItsDepthDoesNotSet). A halt's error and a visit's
// errWalkDone are answered as they are.
func walkEntries(dir *dirHandle, name string, halt func() error, visit func(info os.FileInfo) error) error {
	return walkLevel(dir, name, 0, halt, visit)
}

// walkLevel is walkEntries for name, which lies depth levels below the name the walk began at.
func walkLevel(dir *dirHandle, name string, depth int, halt func() error, visit func(info os.FileInfo) error) error {
	if halt != nil {
		if err := halt(); err != nil {
			return err
		}
	}
	level, err := dir.open(name)
	if err != nil {
		return err
	}
	defer func() { _ = level.Close() }()
	// The listing is a File opened in the handle's Root, so each entry it reads carries the
	// lstat its own descriptor made (os.newUnixDirent, go1.27.3), as dirHandle.ReadDir's do.
	listing, err := level.root.Open(".")
	if err != nil {
		return under(name, within(err, ""))
	}
	defer func() { _ = listing.Close() }()
	for batch := 0; ; batch++ {
		// The first batch is read right after the halt above; each later one asks again, so a
		// directory of any number of names is read no further than the batch the stop came in.
		if batch > 0 && halt != nil {
			if err := halt(); err != nil {
				return err
			}
		}
		entries, read := nextBatch(listing)
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				// Not met under go1.27.3: an entry listed through a handle carries the lstat
				// its listing made, and Info answers that with no error (dirHandle.ReadDir).
				// Were it to fail, a name gone since the listing holds nothing back, and any
				// other error names the entry by its name below name, quoted, as every other
				// read here does.
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return under(name, within(err, entry.Name()))
			}
			if err := visit(info); err != nil {
				return err
			}
			if !entry.IsDir() {
				continue
			}
			if depth+1 > volumeWalkDepth {
				return under(name, &quotedPathError{op: "walk", path: entry.Name(), err: errWalkTooDeep})
			}
			if err := walkLevel(level, entry.Name(), depth+1, halt, visit); err != nil {
				return under(name, err)
			}
		}
		switch {
		case errors.Is(read, io.EOF):
			return nil
		case read != nil:
			return under(name, within(read, ""))
		}
	}
}

// walkEntriesBatch is how many names of one level walkEntries and the size walk's measureUnder
// and readBatched read at a time: procNamesBatch, the batch eachName reads a process's
// descriptors in.
const walkEntriesBatch = procNamesBatch

// nextBatch is the next walkEntriesBatch names of one level's listing, in the order they sort,
// and the error the read answered beside them: io.EOF once the listing has no more.
func nextBatch(listing *os.File) ([]os.DirEntry, error) {
	entries, err := listing.ReadDir(walkEntriesBatch)
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
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
