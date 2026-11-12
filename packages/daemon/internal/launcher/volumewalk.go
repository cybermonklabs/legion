package launcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	// module cache, a bun cache and six workspaces, which is millions of inodes, and it runs in
	// the process that is PID 1 of the roles container: a minute is what the pod can give a
	// measurement that only tells a reader where the bytes went.
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
	At      time.Time
	Partial bool
	// Missing is what the walk did not measure: a directory it could not list or could not
	// open, and everything it had not reached when the budget or the container's stop cut it.
	Missing []string
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
	// refused is a read beneath the directory the kernel refused, which makes the reading
	// partial and ends nothing: /legion/feed is a read-only mount inside the same claim, so the
	// walk reaches it and its bytes are already in the statfs reading the marks are taken from.
	refused bool
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
	measure func(ctx context.Context, dir *os.Root, name string, at time.Time) measured
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
// is resolved by the descriptor the walk holds and nothing else, and os.Root refuses one that
// leaves the root. A name with a separator is resolved component by component, and that
// resolution is not safe on this cluster's kernel. Measured in an issue pod of this deployment
// at 2026-11-12T06:30Z, kernel 4.19.0-gvisor: `openat(dirfd, "scratch",
// O_RDONLY|O_DIRECTORY|O_NOFOLLOW)` on a directory holding `scratch -> /elsewhere` opens
// /elsewhere rather than failing with ELOOP, and os.Root opens every non-final component that
// way (os.rootOpenDir, go1.27.3) — so `fs.Stat(root.FS(), "scratch/bun")` reads /elsewhere/bun,
// while `root.OpenRoot("scratch")` and `fs.ReadDir(root.FS(), "scratch")`, which resolve the
// final component with an open that carries no O_DIRECTORY and so does fail with ELOOP, refuse
// it. The same rule is why the sweep names an entry by its own name in the directory that holds
// it (sweepDir), and it holds here for reads: the walk opens the root, then the child, then the
// target, each by a single name, and descends by a single name at a time below it (measureTree).
//
// What it does not rule out: the path the handle is opened through, since os.OpenRoot follows a
// symbolic link there. The sweep answers that with an lstat of each root it opens (list),
// because its roots are directories under /legion/scratch that a role owns; this one is the
// volume's own mount point (VolumeRoot), above everything the pod gives a role. Nor does it
// rule out a child swapped for another directory of the volume between the listing and the
// measurement: those are bytes of the volume this report is about, measured under the name they
// stand at.
func (w sizeWalk) run(ctx context.Context) walkResult {
	handle, err := os.OpenRoot(w.root)
	if err != nil {
		// Nothing can be measured at all, as for a root that cannot be listed (targets): the
		// report still goes, with the volume root named as what was not measured.
		return walkResult{At: w.now(), Partial: true, Missing: []string{w.root}}
	}
	defer func() { _ = handle.Close() }()
	targets, result := w.targets(handle)
	result.At = w.now()

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

	for from, target := range targets {
		if ctx.Err() != nil {
			result.Partial = true
			result.Missing = append(result.Missing, w.pathsOf(targets[from:])...)
			return result
		}
		dir, name, err := w.openTarget(handle, target)
		if err != nil {
			// The child the target lies in is gone, or is no longer a directory this walk may
			// read through the handle it holds on the root. It is named as unmeasured and the
			// walk goes on, as a directory it cannot list is (targets).
			result.Partial = true
			result.Missing = append(result.Missing, w.pathOf(target))
			continue
		}
		one := w.measure(ctx, dir, name, result.At)
		if dir != handle {
			// The handle on the child, which is this target's alone; the root's own is the
			// walk's and is closed when the walk ends.
			_ = dir.Close()
		}
		if one.cut {
			result.Partial = true
			result.Missing = append(result.Missing, w.pathsOf(targets[from:])...)
			return result
		}
		one.size.Path = w.pathOf(target)
		result.Sizes = append(result.Sizes, one.size)
		if one.refused {
			result.Partial = true
		}
	}
	return result
}

// openTarget is the handle on the directory that holds one target and the target's own name in
// it, which is all a measurement is given: the root's handle for a target that is an immediate
// child of the root, and a handle opened on that child by its single name for one a level
// below. The caller closes the handle when it is not the root's own.
func (w sizeWalk) openTarget(handle *os.Root, target walkTarget) (*os.Root, string, error) {
	if target.inside == "" {
		return handle, target.child, nil
	}
	child, err := handle.OpenRoot(target.child)
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

// pathsOf is pathOf over the targets a cut walk did not reach.
func (w sizeWalk) pathsOf(targets []walkTarget) []string {
	paths := make([]string, len(targets))
	for at, target := range targets {
		paths[at] = w.pathOf(target)
	}
	return paths
}

// targets is what the walk measures, in order: every directory exactly two levels under the
// volume root, and, for an immediate child of the root that holds no directory of its own, that
// child itself, so that nothing on the volume is left unaccounted. Two levels is the depth at
// which the numbers mean something to a reader — `/legion/scratch/tmp` and `/legion/go/build`
// rather than `/legion/scratch`, which is every cache of the pod in one figure — and no deeper,
// because the level below that is one directory per cache entry.
//
// Both listings go through the handle on the volume root, the second one by the child's own
// name in it, so what becomes a target is an entry of the root the walk opened (run). The order
// is the order the names sort in, which is what fs.ReadDir answers, as os.ReadDir did.
func (w sizeWalk) targets(handle *os.Root) ([]walkTarget, walkResult) {
	result := walkResult{}
	children, err := fs.ReadDir(handle.FS(), ".")
	if err != nil {
		// Nothing can be measured at all. The report still goes, with the volume root named as
		// what was not measured: a sweep that ran is worth telling the daemon about whether or
		// not the walk could say where the bytes are.
		result.Partial = true
		result.Missing = []string{w.root}
		return nil, result
	}
	groups := make([][]walkTarget, walkGroups)
	for _, child := range children {
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
		inside, err := fs.ReadDir(handle.FS(), child.Name())
		if err != nil {
			result.Partial = true
			result.Missing = append(result.Missing, w.pathOf(walkTarget{child: child.Name()}))
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
	var targets []walkTarget
	for _, group := range groups {
		targets = append(targets, group...)
	}
	return targets, result
}

// measureTree measures one target: the bytes of every regular file under it, and of those the
// bytes whose modification time is more than an hour and more than a day old. The three numbers
// come out of one walk because it stats every file for the first of them anyway.
//
// name is the target's own name in dir, the handle the walk holds on the directory that holds
// it (run), and the walk below it opens one name at a time in a directory it holds open the
// same way: that is the single read os.Root resolves by the descriptor alone, and the one this
// cluster's kernel does not let through a link (run). Nothing here is read by a path, so a
// rename made while the walk runs cannot aim it out of the volume.
//
// It follows no symbolic link and counts no directory's own blocks, so what it reports is the
// bytes a deletion of the tree would free, which is the number a reader of a trim report is
// after. A read the kernel refuses makes the reading partial and ends nothing, and a file that
// is gone by the time it is stat'ed is nothing at all: the sweep that made this report was
// deleting in these very directories moments ago.
func measureTree(ctx context.Context, dir *os.Root, name string, at time.Time) measured {
	out := measured{}
	hour, day := at.Add(-volumeSizeHour), at.Add(-volumeSizeDay)
	info, err := dir.Lstat(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			out.refused = true
		}
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
// are the inode the listing read.
//
// A directory it cannot open or list is a refused reading and ends nothing: the walk goes on
// with the rest of the entries, since /legion/feed is a read-only mount inside the same claim
// and a sweep is deleting in these directories moments before.
//
// It costs one descriptor per level of the tree, held until the level is done, where the walk
// it replaced held one at a time and resolved the paths below it again at every step: a child
// can only be opened from a handle on the directory that holds it, so a safe descent is a
// descent that keeps its ancestors open. What bounds the cost is the walk's own budget and the
// pod's limit, which is 1048576 descriptors (`ulimit -n`, measured in an issue pod of this
// deployment at 2026-11-12T06:30Z); a level the kernel refuses for any reason, that one
// included, is a refused reading like any other and the walk goes on.
func measureUnder(ctx context.Context, dir *os.Root, name string, hour, day time.Time, out *measured) {
	under, err := dir.OpenRoot(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			out.refused = true
		}
		return
	}
	defer func() { _ = under.Close() }()
	entries, err := fs.ReadDir(under.FS(), ".")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			out.refused = true
		}
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
			measureUnder(ctx, under, entry.Name(), hour, day, out)
			if out.cut {
				return
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				out.refused = true
			}
			continue
		}
		countFile(out, info, hour, day)
	}
}

// countFile adds one file's bytes to out, and its bytes to each age split it falls in. A file
// that is not a regular file is no bytes a deletion of the tree would free: a symbolic link is
// counted nowhere and followed nowhere, and a device or a socket holds nothing.
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
