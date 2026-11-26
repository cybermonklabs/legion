package survival

import (
	"slices"
	"strings"
)

// Replay is the forward replay of scripts-feasibility/replay.py: it is given a branch's
// first-parent commits one at a time, in chain order, each with its patch, and carries the lines
// of the commits it tracks as places, shifting each by the hunks after it. A line is its commit,
// its file and its line number when it was added, never its text (method-md, step 5); the commit
// whose hunk drops its place removed it. It needs no tree: a tracked line's place moves only by
// the hunks of the commits after it, so the lines of commits it does not track are not held, and
// its memory is proportional to the lines it tracks. Apply is the reference's walk, with one
// departure: replay.py follows every line of its window and Replay only those of the commits
// Track names, which gives each of those the same counts.
type Replay struct {
	track   map[string]struct{}
	index   map[string]int32
	origins []origin
	paths   map[string]*pathLines
	applied int
	lines   int
	peak    int
}

// origin is one tracked commit: its SHA, the lines it added, and the removals of those lines in
// the order of their removers.
type origin struct {
	sha      string
	added    int
	removals []Removal
}

// pathLines is the tracked lines of one path, in the order of their places. The paths map holds
// it by pointer, so a change of its lines leaves the map's key, a string of the replay's own, in
// place.
type pathLines struct{ lines []line }

// line is one tracked line: its place in its file now, its commit (an index of origins) and its
// line number when that commit added it.
type line struct{ at, origin, id int32 }

// Removal is the lines of one tracked commit that one later commit removed: By is that commit's
// index in the order of Apply, counted from 0.
type Removal struct {
	By    int
	Lines int
}

// Line is one tracked line as Replay answers it: its place in its file now (1-based), the SHA of
// the commit that added it, and its line number in that commit's file when it was added.
type Line struct {
	At     int
	Origin string
	ID     int
}

// NewReplay is a replay that has applied no commit and tracks none.
func NewReplay() *Replay {
	return &Replay{track: map[string]struct{}{}, index: map[string]int32{}, paths: map[string]*pathLines{}}
}

// Track names commits whose lines the replay follows, before they are applied; the commits it
// does not track are applied for their shifts alone.
func (r *Replay) Track(shas ...string) {
	for _, sha := range shas {
		r.track[sha] = struct{}{}
	}
}

// Drop stops following the lines of shas: those already applied are taken out of every file and
// their counts forgotten, and those not yet applied will be applied for their shifts alone.
// Measure drops the changes of the days a commit it cannot read blocks.
func (r *Replay) Drop(shas ...string) {
	dropped := map[int32]bool{}
	for _, sha := range shas {
		delete(r.track, sha)
		if at, ok := r.index[sha]; ok {
			r.origins[at] = origin{sha: sha}
			dropped[at] = true
		}
	}
	if len(dropped) == 0 {
		return
	}
	for path, held := range r.paths {
		kept := held.lines[:0]
		for _, tracked := range held.lines {
			if dropped[tracked.origin] {
				r.lines--
				continue
			}
			kept = append(kept, tracked)
		}
		if len(kept) == 0 {
			delete(r.paths, path)
			continue
		}
		held.lines = kept
	}
}

// Apply replays one commit's file diffs, in their order, as replay.py's walk: a binary file and
// a submodule are skipped; a copy is an add of its new path; a deleted file removes every line it
// holds; a rename moves its old path's lines to its new path (and the new path's lines, which git
// never gives a rename's target, are lost with no removal, as the reference's state.pop drops
// them); and each hunk removes the tracked lines of its removed range, shifts the lines after it
// by its delta, and, for a tracked commit, adds its lines. The commit's index for its removals is
// the number of commits applied before it.
func (r *Replay) Apply(commit Commit, files []FileDiff) {
	by := r.applied
	r.applied++
	from := int32(-1)
	if _, ok := r.track[commit.SHA]; ok {
		delete(r.track, commit.SHA)
		from = int32(len(r.origins))
		r.index[commit.SHA] = from
		r.origins = append(r.origins, origin{sha: commit.SHA})
	}
	for index := range files {
		r.apply(by, from, &files[index])
	}
}

// apply replays one file diff of the commit at index by, whose lines are origin from (or -1 for a
// commit not tracked).
func (r *Replay) apply(by int, from int32, file *FileDiff) {
	if file.Binary || file.Submodule {
		return
	}
	oldPath, newPath := file.OldPath, file.NewPath
	if file.Status == StatusCopied {
		oldPath = ""
	}
	if newPath == "" {
		if oldPath != "" {
			if held := r.take(oldPath); held != nil {
				for _, tracked := range held.lines {
					r.remove(by, tracked.origin)
				}
			}
		}
		return
	}
	inPlace := oldPath == "" || oldPath == newPath
	var held *pathLines
	if inPlace {
		held = r.paths[newPath]
	} else {
		held = r.take(oldPath)
		if lost := r.take(newPath); lost != nil {
			r.lines -= len(lost.lines)
		}
	}
	if len(file.Hunks) == 0 {
		if !inPlace && held != nil {
			r.paths[strings.Clone(newPath)] = held
		}
		return
	}
	var lines []line
	if held != nil {
		lines = held.lines
	}
	if len(lines) == 0 && from < 0 {
		return
	}
	// A commit that is not tracked adds no line, so its lines are written over the ones they
	// were read from, never ahead of them.
	out := lines[:0]
	if from >= 0 {
		added := 0
		for _, hunk := range file.Hunks {
			added += hunk.Added
		}
		out = make([]line, 0, len(lines)+added)
	}
	next, delta := 0, int32(0)
	for _, hunk := range file.Hunks {
		start, end := int32(hunk.OldStart), int32(hunk.OldStart+hunk.Removed)
		for ; next < len(lines) && lines[next].at < start; next++ {
			shifted := lines[next]
			shifted.at += delta
			out = append(out, shifted)
		}
		for ; next < len(lines) && lines[next].at < end; next++ {
			r.remove(by, lines[next].origin)
		}
		if from >= 0 {
			for offset := range hunk.Added {
				at := int32(hunk.NewStart + offset)
				out = append(out, line{at: at, origin: from, id: at})
			}
			r.origins[from].added += hunk.Added
			r.lines += hunk.Added
		}
		delta += int32(hunk.Added - hunk.Removed)
	}
	for ; next < len(lines); next++ {
		shifted := lines[next]
		shifted.at += delta
		out = append(out, shifted)
	}
	r.peak = max(r.peak, r.lines)
	switch {
	case len(out) == 0:
		if inPlace && held != nil {
			delete(r.paths, newPath)
		}
	case held == nil:
		r.paths[strings.Clone(newPath)] = &pathLines{lines: out}
	case inPlace:
		held.lines = out
	default:
		held.lines = out
		r.paths[strings.Clone(newPath)] = held
	}
}

// take takes path's lines out of the replay.
func (r *Replay) take(path string) *pathLines {
	held, ok := r.paths[path]
	if ok {
		delete(r.paths, path)
	}
	return held
}

// remove records that the commit at index by removed one line of origin from.
func (r *Replay) remove(by int, from int32) {
	removals := r.origins[from].removals
	if last := len(removals) - 1; last >= 0 && removals[last].By == by {
		removals[last].Lines++
	} else {
		r.origins[from].removals = append(removals, Removal{By: by, Lines: 1})
	}
	r.lines--
}

// Added is the lines the tracked commit sha added; 0 for a commit not tracked or dropped.
func (r *Replay) Added(sha string) int {
	if at, ok := r.index[sha]; ok {
		return r.origins[at].added
	}
	return 0
}

// Removals is every removal of the tracked commit sha's lines, in the order of their removers.
func (r *Replay) Removals(sha string) []Removal {
	return slices.Clone(r.removals(sha))
}

// removals is Removals with no copy, for the caller in this package that only reads it.
func (r *Replay) removals(sha string) []Removal {
	if at, ok := r.index[sha]; ok {
		return r.origins[at].removals
	}
	return nil
}

// Lines is how many tracked lines the replay holds now.
func (r *Replay) Lines() int { return r.lines }

// PeakLines is the most tracked lines the replay has held at once.
func (r *Replay) PeakLines() int { return r.peak }

// Paths is every path that holds a tracked line, in order.
func (r *Replay) Paths() []string {
	paths := make([]string, 0, len(r.paths))
	for path := range r.paths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// LinesOf is the tracked lines of path, in the order of their places.
func (r *Replay) LinesOf(path string) []Line {
	held := r.paths[path]
	if held == nil {
		return nil
	}
	out := make([]Line, len(held.lines))
	for index, tracked := range held.lines {
		out[index] = Line{At: int(tracked.at), Origin: r.origins[tracked.origin].sha, ID: int(tracked.id)}
	}
	return out
}
