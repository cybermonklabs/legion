package leakguard

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// mergeChanges are the paths a merge adds and the files it changes that lines can be added at
// (fileChange), read from its files, not from its combined diff (the last paragraph says why).
// What a merge publishes of its own is in the files that differ from every parent's: a file one
// parent has as it is came with that parent, and a parent is either a commit this same push adds —
// read as records of its own, since Guard.commits lists every commit the destination does not
// reach — or a commit the destination already holds. A file that differs from every parent's is
// compared with each parent's file at that path (Guard.addedLines): a line one of them holds is
// text that parent brings, wherever the merge puts it, since the rules read a line's text alone,
// and a line none of them holds is the resolution's own. A parent's file that is absent or a
// submodule holds none, and a path the merge has no file at, or a submodule at, has no line to
// read. A path the merge adds is one at which no parent has anything, and it is read as a path
// too.
//
// The paths are those at which the merge's tree differs from every parent's, in the order the diff
// against its first parent lists them. One diff-tree reads every parent, from a line
// "<merge> <parent>" each on its standard input, which it reads as the merge with that parent
// alone: one git per parent would be 52 for one of cybermonklabs/oh-my-pi's release merges.
// --always writes the merge's id ahead of every parent's diff, an empty one too, so the answer
// splits into one diff per parent in the order they were asked for; without it a parent the merge
// matches would leave no mark, and the next parent's diff would be counted as its. Renames are
// off: a file renamed from a parent's path is a path that parent has no file at, and so a path
// whose every line is read.
//
// A merge is not read from its combined diff (-c), which would be its resolution but for a
// defect: combine-diff.c marks the parent a line was lost from as 1<<parent in an int, so from 33
// parents a "-" lands under parent n-32 and a hunk disagrees with its own counts, and from 64 a
// line only the merge adds is left out (sjawhar/dotfiles 0ab92325). Merges that wide are pushed:
// cybermonklabs/oh-my-pi's release merges on sami have 51 to 53 parents. On its 7ae9bc731cbc, of
// 52 parents, measured 2026-11-17, the combined diff read 2946 added lines in 14 files, where the
// merge's files read none: 14 paths differ from every parent, every line of each is held by some
// parent, and no path is added.
func (g *Guard) mergeChanges(ctx context.Context, dir, merge string, parents []string) (paths []string, changes []fileChange, err error) {
	var asked strings.Builder
	for _, parent := range parents {
		asked.WriteString(merge + " " + parent + "\n")
	}
	out, err := g.gitFed(ctx, dir, nil, asked.String(), "diff-tree", "--stdin", "--always", "-r", "--no-renames", "-z")
	if err != nil {
		return nil, nil, err
	}
	what := "merge " + short(merge)
	// listedPath is a path the first diff lists: change, the merge's file there and its parents'
	// files, each blob once however many parents have it; listed, how many of the diffs read so
	// far list the path, counted only while each one has, since a path one diff leaves out is one
	// the merge has as that parent has it; and added, whether each of those diffs lists the path as
	// one the merge adds (A).
	type listedPath struct {
		change fileChange
		listed int
		added  bool
	}
	// listedFirst are the paths the first diff lists, in its order, each at index.
	index := map[string]int{}
	var listedFirst []listedPath
	diffs := 0
	for entry, err := range rawEntries(out, what) {
		switch {
		case err != nil:
			return nil, nil, err
		case entry.commit != "":
			if entry.commit != merge {
				return nil, nil, fmt.Errorf("git diff-tree of merge %s headed a diff with another commit", short(merge))
			}
			diffs++
			continue
		case diffs == 0:
			return nil, nil, shapeError(what)
		}
		at, seen := index[entry.path]
		switch {
		case !seen && diffs == 1:
			change := fileChange{path: entry.path, from: entry.path}
			if entry.ourKind.blob() {
				change.blob = entry.ours
			}
			at = len(listedFirst)
			index[entry.path] = at
			listedFirst = append(listedFirst, listedPath{change: change, added: true})
		case !seen || listedFirst[at].listed != diffs-1:
			continue
		}
		listing := &listedFirst[at]
		listing.listed = diffs
		if entry.theirKind.blob() && !slices.Contains(listing.change.held, entry.theirs) {
			listing.change.held = append(listing.change.held, entry.theirs)
		}
		listing.added = listing.added && entry.status == 'A'
	}
	if diffs != len(parents) {
		return nil, nil, fmt.Errorf("git diff-tree of merge %s answered %d diffs for its %d parents", short(merge), diffs, len(parents))
	}
	for _, listing := range listedFirst {
		if listing.listed != diffs {
			continue
		}
		if listing.added {
			paths = append(paths, listing.change.path)
		}
		if listing.change.blob != "" {
			changes = append(changes, listing.change)
		}
	}
	return paths, changes, nil
}
