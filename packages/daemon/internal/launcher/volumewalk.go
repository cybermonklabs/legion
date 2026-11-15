package launcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The size walk the trimmer's report carries: what is on the pod's volume, directory by
// directory, so that the issue's architect reading a report can see which tool filled it rather
// than only that it is full. It is apart from the sweep in every way that matters — the sweep's
// budget does not bound it, it is made at most once an hour, and it deletes nothing — so it
// lives in a file of its own beside the sweep's (volumetrim.go).

const (
	// volumeWalkBudget bounds one walk. The walk stats every file on a volume that holds a
	// module cache, a bun cache and the issue's workspace, which is millions of inodes, and it
	// runs in the process that is PID 1 of the roles container: a minute is what the pod can give
	// a measurement that only tells a reader where the bytes went.
	volumeWalkBudget = time.Minute
	// volumeWalkInterval is the least time between two walks. A pod that is full sweeps every
	// interval, and a walk per sweep would spend a minute of every interval on the same
	// numbers; a report made between walks carries the last walk's numbers with the time they
	// were read (LauncherVolumeReport.SizesAt), so no reader takes a stale measurement for this
	// sweep's.
	volumeWalkInterval = time.Hour
	// The two age splits each size carries. They come out of the same walk, which stats every
	// file anyway, and they are what tell a reader a working set from cold bytes: a hundred
	// gigabytes nothing has touched for a day is a cache to delete, the same bytes written in
	// the last hour are six roles at work.
	volumeSizeHour = time.Hour
	volumeSizeDay  = 24 * time.Hour
)

// The two immediate children of the volume root whose directories are measured first, because a
// walk that ends early must lose the least useful numbers. Everything the trimmer may delete is
// under one of them — $TMPDIR, the cache home and bun's install cache under scratch, the build
// cache under go (ReclaimableRoots) — and the rest of the volume is a role's work, which a
// reader of a trim report can do nothing about.
const (
	volumeScratchDir = "scratch"
	volumeGoDir      = "go"
)

// The order the walk measures in: the volume root's scratch children, then its go children, then
// everything else.
const (
	walkGroupScratch = iota
	walkGroupGo
	walkGroupRest
	walkGroups
)

// walkResult is one size walk as the report carries it.
type walkResult struct {
	Sizes []shimwire.LauncherVolumeSize
	// At is when the walk was made, which a report between walks carries with the last walk's
	// numbers.
	At time.Time
	// Partial is a walk its budget or the container's stop cut before it reached every target,
	// which is the one way a walk ends early: Missing then holds every target it had not
	// measured, and, where the cut came while it listed its targets, every immediate child of the
	// volume root it had not listed yet (targets). A directory the walk could not open or list,
	// and one it could read only in part (LauncherVolumeSize.Refused), end nothing, so a walk that
	// met only those reached its end and is not partial.
	Partial bool
	// Missing is what the walk did not measure: a directory it could not open or list, with the
	// read the kernel refused (LauncherVolumeMissing.Reason), and everything it had not reached
	// when the budget or the container's stop cut it. It holds at most volumeReportSizesMissing
	// of them, the most a report names, and MissingOmitted counts the rest (miss): the trimmer
	// keeps a walk for the hour its numbers are reported (trimmer.sizes), and a role may make any
	// number of directories under /legion/scratch.
	Missing        []shimwire.LauncherVolumeMissing
	MissingOmitted int
}

// miss names one directory the walk did not measure, and why where a read was refused, or counts
// it once Missing holds as many as a report names.
func (r *walkResult) miss(path, reason string) {
	if len(r.Missing) >= volumeReportSizesMissing {
		r.MissingOmitted++
		return
	}
	r.Missing = append(r.Missing, shimwire.LauncherVolumeMissing{Path: path, Reason: reason})
}

// measured is one directory as a measurement read it. The path a report carries is not here:
// run fills it from the root it opened and the names it listed, so a size always names the
// place on the volume whose bytes the number is, whatever a measurement met (run).
type measured struct {
	size shimwire.LauncherVolumeSize
	// cut is the budget or the container's stop ending this measurement where it stood. The
	// number is not the whole of the directory, so the walk reports it as missing rather than
	// as a size a reader would take for the directory's own.
	cut bool
	// unread is why the walk read nothing of the directory: the kernel refused its own lstat,
	// its open or its listing (unreadReason). Nothing of it was measured, so the walk names it
	// as missing with that reason rather than as a size of no bytes (run).
	unread string
	// refused is how many reads beneath the directory the kernel refused, and refusedName and
	// refusedDepth the first of them: the name refused and how many levels below the directory
	// it lay (refuse). A refusal makes the reading a lower bound of the directory and ends
	// nothing: /legion/feed is a read-only mount inside the same claim, so the walk reaches it
	// and its bytes are already in the statfs reading the marks are taken from. run carries them
	// on the size (refusedPath), where a reader is told the number is at least that many bytes
	// and where the read was refused.
	refused      int
	refusedName  string
	refusedDepth int
	// depth is how many levels below the directory the one being read lies: the
	// measurement's own scratch, which is all fail needs to tell the directory itself from one
	// below it and all a refusal needs of where it is, so that no path below the directory is
	// built or kept whatever the depth of the tree.
	depth int
}

// walkTarget is one directory the walk measures, named the way every read of it is made: the
// immediate child of the volume root it lies in, and its own name in that child — "" when the
// target is the child itself, which is what an immediate child holding no directory of its own
// is measured as.
//
// The walk carries names and not handles because a volume may hold thousands of targets — a
// role may make any number of directories under /legion/scratch, and the report is bounded for
// that very reason (volumeReportSizes, volumetrim.go) — and a handle each would be thousands of
// descriptors held open for the length of a walk. Each is opened in turn, one name at a time,
// when it is measured (run).
type walkTarget struct {
	child  string
	inside string
}

// sizeWalk is one walk of the pod's volume.
type sizeWalk struct {
	root string
	now  func() time.Time
	// after is the budget's clock, the trimmer's own ticker seam (trimmer.ticks). The budget
	// cannot be a deadline a loop reads, because what overruns it is one measurement inside the
	// loop — a walk of a million-file module cache — so it arrives as a channel that cancels
	// the walk's context, which every measurement reads.
	after   func(time.Duration) (<-chan time.Time, func())
	budget  time.Duration
	measure func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured
}

// run measures the directories two levels under the volume root and answers what it read.
//
// Every read of the walk goes through a handle the walk opened, and nothing it carries from one
// read to the next is a path. The listing used to answer absolute paths and each measurement
// resolved one of them again, so a role that renamed a child of the root and left a symbolic
// link in its place between the two aimed every target under that child at a tree of its own:
// /legion/scratch is writable by the role uid (`drwx--S--- legion legion`, measured in an issue
// pod of this deployment at 2026-11-11T08:05Z), and the launcher walks in the container's own
// mount namespace, where /var/run/legion/roles holds every role's private, state and
// agent-secrets directories — a path each role's own namespace covers with an empty tmpfs
// (roleinit_linux.go). The names and the bytes read there went into the notice the daemon
// publishes to the issue's architect (INFRA-2009, review 6). It is the fault the sweep had, on
// the read side, and the sweep's fix is sweepDir (volumetrim.go).
//
// What a handle guarantees, and the one rule that makes it hold: a name with no separator in it
// is resolved by the descriptor the walk holds and nothing else, and the open refuses a name
// that is a symbolic link, whatever that link names. A name with a separator is resolved
// component by component, and that resolution is not safe on this cluster's kernel. Measured in
// an issue pod of this deployment at 2026-11-12T06:30Z, kernel 4.19.0-gvisor: `openat(dirfd,
// "scratch", O_RDONLY|O_DIRECTORY|O_NOFOLLOW)` on a directory holding `scratch -> /elsewhere`
// opens /elsewhere rather than failing with ELOOP, and os.Root opens every non-final component
// that way (os.rootOpenDir, go1.27.3) — so `fs.Stat(root.FS(), "scratch/bun")` read
// /elsewhere/bun.
//
// What os.Root did refuse was a final component that is a link out of the root, which it
// resolves itself: `root.OpenRoot("scratch")` and `fs.ReadDir(root.FS(), "scratch")` answered
// ELOOP for `scratch -> /elsewhere`. It is a narrower refusal than it reads as, and the sweep's
// own cases measured where it stops: a final component whose link stays lexically inside the
// root is followed, and that link's own elements are then resolved with the open above, so
// `0f -> zz/sh` beside `zz -> /bait` reached /bait/sh through one single-element call
// (TestAShardSwappedForARelativeLinkCrossingAnAbsoluteOneIsSkipped, measured on 2026-11-12).
// The same rule is why the sweep names an entry by its own name in the directory that holds it
// (sweepDir), and it holds here for reads: the walk opens the root, then the child, then the
// target, each by a single name through a handle of its own (dirHandle, volumedir.go), and
// descends by a single name at a time below it (measureTree).
//
// What it does not rule out: a child swapped for another directory of the volume between the
// listing and the measurement. Those are bytes of the volume this report is about, measured
// under the name they stand at. The path the root itself is opened through is no longer among
// them: every element of it is opened in turn and one that is a link is refused
// (openVolumeDir), where os.OpenRoot followed one.
//
// The budget is armed before anything is opened, so the listing of the targets is inside it as
// the measurements are: a role may make any number of directories under /legion/scratch, and
// each is listed (targets).
func (w sizeWalk) run(ctx context.Context) walkResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	budget, stopBudget := w.after(w.budget)
	defer stopBudget()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-budget:
			cancel()
		case <-done:
		}
	}()

	handle, err := openVolumeDir(w.root)
	if err != nil {
		// Nothing can be measured at all, as for a root that cannot be listed (targets): the
		// report still goes, with the volume root named as what was not measured. Nothing cut
		// the walk, so it is not partial (walkResult.Partial).
		result := walkResult{At: w.now()}
		result.miss(w.root, unreadReason(err))
		return result
	}
	defer func() { _ = handle.Close() }()
	targets, result := w.targets(ctx, handle)
	result.At = w.now()
	if result.Partial {
		return result
	}

	for from, target := range targets {
		if ctx.Err() != nil {
			w.cut(&result, targets[from:])
			return result
		}
		dir, name, err := w.openTarget(handle, target)
		if err != nil {
			// The child the target lies in is gone, or is no longer a directory this walk may
			// read through the handle it holds on the root. It is named as unmeasured and the
			// walk goes on, as a directory it cannot list is (targets).
			result.miss(w.pathOf(target), unreadReason(err))
			continue
		}
		one := w.measure(ctx, dir, name, result.At)
		if dir != handle {
			// The handle on the child, which is this target's alone; the root's own is the
			// walk's and is closed when the walk ends.
			_ = dir.Close()
		}
		if one.cut {
			w.cut(&result, targets[from:])
			return result
		}
		if one.unread != "" {
			// Nothing of the target was read, so it is no size at all, not even a lower bound
			// of nothing: it is named as unmeasured with the read the kernel refused, and the
			// walk goes on (measured.unread).
			result.miss(w.pathOf(target), one.unread)
			continue
		}
		one.size.Path = w.pathOf(target)
		if one.refused > 0 {
			one.size.Refused = refusedPath(one.size.Path, one.refusedName, one.refusedDepth)
			one.size.Refusals = one.refused
		}
		result.Sizes = append(result.Sizes, one.size)
	}
	return result
}

// openTarget is the handle on the directory that holds one target and the target's own name in
// it, which is all a measurement is given: the root's handle for a target that is an immediate
// child of the root, and a handle opened on that child by its single name for one a level
// below. The caller closes the handle when it is not the root's own.
func (w sizeWalk) openTarget(handle *dirHandle, target walkTarget) (*dirHandle, string, error) {
	if target.inside == "" {
		return handle, target.child, nil
	}
	child, err := handle.open(target.child)
	if err != nil {
		return nil, "", err
	}
	return child, target.inside, nil
}

// pathOf is the absolute path of one target, which the report's sizes and its missing list are
// read by: they go to a person — the issue's architect reading the notice, an operator reading
// the pod's log — and a name under a handle names nothing to either, as sweepDir.pathOf answers
// the sweep's log the same way. It is built from the root the walk opened and the names it
// listed, so it names the place on the volume the walk measured whatever a measurement met.
func (w sizeWalk) pathOf(target walkTarget) string {
	return filepath.Join(w.root, target.child, target.inside)
}

// cut is the walk ended by its budget or the container's stop with targets still to measure:
// it is partial, and each of them is named as unmeasured as far as Missing holds them and
// counted beyond that, with no path built for one it only counts (miss).
func (w sizeWalk) cut(result *walkResult, targets []walkTarget) {
	result.Partial = true
	for at, target := range targets {
		if len(result.Missing) >= volumeReportSizesMissing {
			result.MissingOmitted += len(targets) - at
			return
		}
		result.miss(w.pathOf(target), "")
	}
}

// unreadReason is why the walk read nothing of a directory, as its entry in Missing carries it:
// the read the kernel refused and its answer, `openat: permission denied`, with no path, since
// the entry names the directory already. An error that carries no answer of the kernel's is
// written whole.
func unreadReason(err error) string {
	op := ""
	var quoted *quotedPathError
	var failed *fs.PathError
	switch {
	case errors.As(err, &quoted):
		op = quoted.op
	case errors.As(err, &failed):
		op = failed.Op
	}
	var errno syscall.Errno
	if op == "" || !errors.As(err, &errno) {
		return err.Error()
	}
	return op + ": " + errno.Error()
}

// targets is what the walk measures, in order: every directory exactly two levels under the
// volume root, and, for an immediate child of the root that holds no directory of its own, that
// child itself, so that nothing on the volume is left unaccounted. Two levels is the depth at
// which the numbers mean something to a reader — `/legion/scratch/tmp` and `/legion/go/build`
// rather than `/legion/scratch`, which is every cache of the pod in one figure — and no deeper,
// because the level below that is one directory per cache entry.
//
// The root's listing goes through the handle on the volume root and each child's through a
// handle opened on that child by its single name in it, so what becomes a target is an entry of
// the root the walk opened (run). `fs.ReadDir(root.FS(), child)` named one element too, but
// os.Root resolved that name itself: a child a role had replaced with a link that stayed
// lexically inside the root was followed and its own elements resolved with the open this
// kernel lets through a link (run). The open here refuses a child that is a link, whatever it
// names. The order is the order the names sort in, which is what a handle's listing answers, as
// os.ReadDir did.
//
// ctx is the walk's, which its budget and the container's stop end, and it is asked before each
// child is listed: a cut listing makes the walk partial, with every target it had listed and
// every child it had not named as unmeasured (cut), and nothing is measured. The root's own
// listing is one read and is not cut.
func (w sizeWalk) targets(ctx context.Context, handle *dirHandle) ([]walkTarget, walkResult) {
	result := walkResult{}
	children, err := handle.ReadDir()
	if err != nil {
		// Nothing can be measured at all. The report still goes, with the volume root named as
		// what was not measured: a sweep that ran is worth telling the daemon about whether or
		// not the walk could say where the bytes are.
		result.miss(w.root, unreadReason(err))
		return nil, result
	}
	groups := make([][]walkTarget, walkGroups)
	listed := func() []walkTarget {
		var targets []walkTarget
		for _, group := range groups {
			targets = append(targets, group...)
		}
		return targets
	}
	for at, child := range children {
		if ctx.Err() != nil {
			rest := listed()
			for _, unlisted := range children[at:] {
				rest = append(rest, walkTarget{child: unlisted.Name()})
			}
			w.cut(&result, rest)
			return nil, result
		}
		group := walkGroupRest
		switch child.Name() {
		case volumeScratchDir:
			group = walkGroupScratch
		case volumeGoDir:
			group = walkGroupGo
		}
		if !child.IsDir() {
			groups[group] = append(groups[group], walkTarget{child: child.Name()})
			continue
		}
		inside, err := w.readChild(handle, child.Name())
		if err != nil {
			result.miss(w.pathOf(walkTarget{child: child.Name()}), unreadReason(err))
			continue
		}
		found := false
		for _, entry := range inside {
			if entry.IsDir() {
				groups[group] = append(groups[group], walkTarget{child: child.Name(), inside: entry.Name()})
				found = true
			}
		}
		if !found {
			groups[group] = append(groups[group], walkTarget{child: child.Name()})
		}
	}
	return listed(), result
}

// readChild is every name in one immediate child of the volume root, read through a handle
// opened on that child by its own name in the root the walk holds open: the one read no element
// of the path is resolved by. The handle is this listing's alone and is closed with it; a
// measurement opens the child again when it reaches a target under it (openTarget), because the
// walk holds names and not handles between the listing and the measurements (walkTarget).
func (w sizeWalk) readChild(handle *dirHandle, name string) ([]os.DirEntry, error) {
	child, err := handle.open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = child.Close() }()
	return child.ReadDir()
}

// measureTree measures one target: the bytes of every regular file under it, and of those the
// bytes whose modification time is more than an hour and more than a day old. The three numbers
// come out of one walk because it stats every file for the first of them anyway.
//
// name is the target's own name in dir, the handle the walk holds on the directory that holds
// it (run), and the walk below it opens one name at a time in a directory it holds open the
// same way: that is the single read a handle resolves by its descriptor alone, and the one this
// cluster's kernel does not let through a link (run). Nothing here is read by a path, so a
// rename made while the walk runs cannot aim it out of the volume.
//
// It follows no symbolic link and counts no directory's own blocks: what it reports is the
// apparent size of the regular files under the tree, once per name, which is the number a reader
// of a trim report is after and not always the bytes a deletion of the tree would free — a file
// linked from elsewhere, as bun links its cache into node_modules, is counted though deleting
// the tree frees none of it, and a sparse file counts at its apparent size. A read the kernel
// refuses below the target makes the reading a lower bound, counted and the first of them named
// (refuse), and ends nothing; one it refuses of the target itself — its lstat, its open or its
// listing — leaves nothing of it read, and the reading carries why (unread). A file that is gone
// by the time it is stat'ed is nothing at all: the sweep that made this report was deleting in
// these very directories moments ago.
func measureTree(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
	out := measured{}
	hour, day := at.Add(-volumeSizeHour), at.Add(-volumeSizeDay)
	info, err := dir.Lstat(name)
	if err != nil {
		out.fail(err, name)
		return out
	}
	if !info.IsDir() {
		countFile(&out, info, hour, day)
		return out
	}
	measureUnder(ctx, dir, name, hour, day, &out)
	return out
}

// measureUnder adds every regular file under one directory of dir to out, by its single name in
// a handle on that directory, and so on down the tree. It is the whole of the walk below a
// target: the entries come from one listing of the open directory, which loads each entry's
// lstat through that directory's own descriptor rather than by a path (os.newUnixDirent reads a
// directory opened in a Root with lstatat, go1.27.3), so the kind and the bytes counted here
// are the inode the listing read, and the open of each level refuses a name that is a link and
// waits on nothing (dirHandle.open, volumedir.go).
//
// A directory below the target it cannot open or list is a refused reading and ends nothing:
// the walk goes on with the rest of the entries, since /legion/feed is a read-only mount inside
// the same claim and a sweep is deleting in these directories moments before. The target itself
// refused is the reading unread (fail).
//
// It costs one handle per level of the tree — two descriptors and the level's listing, and no
// path (dirHandle.path, within) — held until the level is done, where the walk it replaced held
// one descriptor at a time and resolved the paths below it again at every step: a child can only
// be opened from a handle on the directory that holds it, so a safe descent is a descent that
// keeps its ancestors open. What bounds the cost is the walk's own budget, asked before each
// entry, and the pod's limit, which is 1048576 descriptors (`ulimit -n`, measured in an issue pod
// of this deployment at 2026-11-12T06:30Z); a level the kernel refuses for any reason, that one
// included, is a refused reading like any other and the walk goes on.
func measureUnder(ctx context.Context, dir *dirHandle, name string, hour, day time.Time, out *measured) {
	under, err := dir.open(name)
	if err != nil {
		out.fail(err, name)
		return
	}
	defer func() { _ = under.Close() }()
	entries, err := under.readDir()
	if err != nil {
		out.fail(err, name)
		return
	}
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			out.cut = true
			return
		default:
		}
		if entry.IsDir() {
			out.depth++
			measureUnder(ctx, under, entry.Name(), hour, day, out)
			out.depth--
			if out.cut {
				return
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			out.refuse(err, entry.Name(), out.depth+1)
			continue
		}
		countFile(out, info, hour, day)
	}
}

// fail is a read of the directory being read, name, that the kernel refused: of the target
// itself, which leaves nothing of it read and names why (unread), or of a directory below it,
// which is a refusal (refuse). A name that is gone by the time it is read is neither, for the
// reason a file that is gone is nothing at all (measureTree).
func (out *measured) fail(err error, name string) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case out.depth == 0:
		out.unread = unreadReason(err)
	default:
		out.refuse(err, name, out.depth)
	}
}

// refuse counts one read the kernel refused beneath the directory measured, and keeps the first
// of them: the name refused and how many levels below the directory it lay. A name that is gone
// by the time it is read is no refusal, for the reason a file that is gone is nothing at all
// (measureTree).
func (out *measured) refuse(err error, name string, depth int) {
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if out.refused == 0 {
		out.refusedName, out.refusedDepth = name, depth
	}
	out.refused++
}

// refusedPath is how a size names the first read refused beneath it, as the deleter's removal
// failure names what it failed on (removalFailure, volumedir.go): the directory measured, the
// name refused and how many levels below the directory that name lay, with the levels between
// written as "..." where there are any, and never the path between them. So a refusal at the
// foot of a chain of directories thousands deep, which a role can make under /legion/scratch, is
// a string of a size set by two names and not by the depth of the chain, and the walk keeps it
// for the hour its numbers are reported (trimmer.sizes).
func refusedPath(dir, name string, depth int) string {
	if depth == 1 {
		return dir + "/" + name
	}
	return dir + "/.../" + name + " (depth " + strconv.Itoa(depth) + ")"
}

// countFile adds one file's bytes to out, and its bytes to each age split it falls in. A file
// that is not a regular file is counted nowhere: a symbolic link is counted nowhere and followed
// nowhere, and a device or a socket holds nothing.
func countFile(out *measured, info os.FileInfo, hour, day time.Time) {
	if !info.Mode().IsRegular() {
		return
	}
	size := info.Size()
	out.size.Bytes += size
	if info.ModTime().Before(hour) {
		out.size.BytesOverHour += size
	}
	if info.ModTime().Before(day) {
		out.size.BytesOverDay += size
	}
}
