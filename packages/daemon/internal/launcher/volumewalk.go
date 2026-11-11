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
	// Missing is what the walk did not measure: a directory it could not list, and everything
	// it had not reached when the budget or the container's stop cut it.
	Missing []string
}

// measured is one directory as a measurement read it.
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

// sizeWalk is one walk of the pod's volume.
type sizeWalk struct {
	root string
	now  func() time.Time
	// after is the budget's clock, the trimmer's own ticker seam (trimmer.ticks). The budget
	// cannot be a deadline a loop reads, because what overruns it is one measurement inside the
	// loop — a WalkDir of a million-file module cache — so it arrives as a channel that cancels
	// the walk's context, which every measurement reads.
	after   func(time.Duration) (<-chan time.Time, func())
	budget  time.Duration
	measure func(ctx context.Context, path string, at time.Time) measured
}

// run measures the directories two levels under the volume root and answers what it read.
func (w sizeWalk) run(ctx context.Context) walkResult {
	at := w.now()
	targets, result := w.targets()
	result.At = at

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

	for at, path := range targets {
		if ctx.Err() != nil {
			result.Partial = true
			result.Missing = append(result.Missing, targets[at:]...)
			return result
		}
		one := w.measure(ctx, path, result.At)
		if one.cut {
			result.Partial = true
			result.Missing = append(result.Missing, targets[at:]...)
			return result
		}
		result.Sizes = append(result.Sizes, one.size)
		if one.refused {
			result.Partial = true
		}
	}
	return result
}

// targets is what the walk measures, in order: every directory exactly two levels under the
// volume root, and, for an immediate child of the root that holds no directory of its own, that
// child itself, so that nothing on the volume is left unaccounted. Two levels is the depth at
// which the numbers mean something to a reader — `/legion/scratch/tmp` and `/legion/go/build`
// rather than `/legion/scratch`, which is every cache of the pod in one figure — and no deeper,
// because the level below that is one directory per cache entry.
func (w sizeWalk) targets() ([]string, walkResult) {
	result := walkResult{}
	children, err := os.ReadDir(w.root)
	if err != nil {
		// Nothing can be measured at all. The report still goes, with the volume root named as
		// what was not measured: a sweep that ran is worth telling the daemon about whether or
		// not the walk could say where the bytes are.
		result.Partial = true
		result.Missing = []string{w.root}
		return nil, result
	}
	groups := make([][]string, walkGroups)
	for _, child := range children {
		path := filepath.Join(w.root, child.Name())
		group := walkGroupRest
		switch child.Name() {
		case volumeScratchDir:
			group = walkGroupScratch
		case volumeGoDir:
			group = walkGroupGo
		}
		if !child.IsDir() {
			groups[group] = append(groups[group], path)
			continue
		}
		inside, err := os.ReadDir(path)
		if err != nil {
			result.Partial = true
			result.Missing = append(result.Missing, path)
			continue
		}
		found := false
		for _, entry := range inside {
			if entry.IsDir() {
				groups[group] = append(groups[group], filepath.Join(path, entry.Name()))
				found = true
			}
		}
		if !found {
			groups[group] = append(groups[group], path)
		}
	}
	var targets []string
	for _, group := range groups {
		targets = append(targets, group...)
	}
	return targets, result
}

// measureTree measures one directory: the bytes of every regular file under it, and of those the
// bytes whose modification time is more than an hour and more than a day old. The three numbers
// come out of one walk because it stats every file for the first of them anyway.
//
// It follows no symbolic link and counts no directory's own blocks, so what it reports is the
// bytes a deletion of the tree would free, which is the number a reader of a trim report is
// after. A read the kernel refuses makes the reading partial and ends nothing, and a file that
// is gone by the time it is stat'ed is nothing at all: the sweep that made this report was
// deleting in these very directories moments ago.
func measureTree(ctx context.Context, path string, at time.Time) measured {
	out := measured{size: shimwire.LauncherVolumeSize{Path: path}}
	hour, day := at.Add(-volumeSizeHour), at.Add(-volumeSizeDay)
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			out.cut = true
			return filepath.SkipAll
		default:
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			out.refused = true
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				out.refused = true
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		out.size.Bytes += size
		if info.ModTime().Before(hour) {
			out.size.BytesOverHour += size
		}
		if info.ModTime().Before(day) {
			out.size.BytesOverDay += size
		}
		return nil
	})
	if err != nil {
		out.refused = true
	}
	return out
}
