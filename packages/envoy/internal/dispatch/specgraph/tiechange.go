package specgraph

import "strings"

// tieChange is the first change that changeBetween finds, of these kinds, or the zero tieChange
// (noChange) when it finds none.
type tieChange struct {
	kind  changeKind
	tie   int    // tieUnknown, pathCovered: the place of the tie among the ties given
	why   string // tieUnknown: why that tie is not known
	path  string // pathCovered: the first changed path, in sorted order, that the tie reaches
	under string // pathCovered: the entry of the tie under path, when the tie does not cover path itself
}

// changeKind is the kind of a tieChange.
type changeKind int

const (
	noChange     changeKind = iota
	tieUnknown              // a tie is not known
	pathsUnknown            // the paths that changed are not known
	pathCovered             // a tie reaches a changed path
)

// changeBetween is the carry rule (R8): a result at one commit counts at another only when the
// check's tie is known at both commits and no path that differs between their trees reaches the
// tie at either, since a tie is a fact of a commit (R18: "At each commit, Dispatch derives four
// ties"), and a file that the test read at one commit and that the other deleted or added changed
// the code that the check runs. ties are the check's ties at the commits, the later commit's
// first, and changed answers the paths that differ, false when they are not known.
//
// A changed path reaches a tie that covers it, and also a tie that holds an entry under it read as
// a directory: git names a submodule that moved to another commit by its one path, the gitlink,
// and so it can name a directory that became a file or a link to a directory, while a tie names
// the files under such a path. So each changed path counts as a file and as a directory, and the
// rule needs to know of no path what it stands for.
//
// It names the first change of these: a tie that is not known, in the order of ties; the paths not
// known; a tie that a changed path reaches, in the order of ties, with the first such path. It
// asks changed only when each tie is known, so a caller that must derive a tie can give the ties
// before it alone first and derive it only when they find no change: a result carries only when
// the ties at both commits find none.
func changeBetween(changed func() ([]string, bool), ties ...Tie) tieChange {
	for i, tie := range ties {
		if !tie.Known {
			return tieChange{kind: tieUnknown, tie: i, why: tie.Why}
		}
	}
	paths, ok := changed()
	if !ok {
		return tieChange{kind: pathsUnknown}
	}
	for i, tie := range ties {
		var first tieChange
		for _, path := range paths {
			if first.kind == pathCovered && path >= first.path {
				continue
			}
			if under, reached := reaches(tie, path); reached {
				first = tieChange{kind: pathCovered, tie: i, path: path, under: under}
			}
		}
		if first.kind == pathCovered {
			return first
		}
	}
	return tieChange{}
}

// reaches is whether the changed path reaches tie: tie covers path, or holds an entry under path
// read as a directory, the first of which is under.
func reaches(tie Tie, path string) (under string, reached bool) {
	if tie.Covers(path) {
		return "", true
	}
	for _, entry := range tie.Paths {
		if strings.HasPrefix(entry, path+"/") {
			return entry, true
		}
	}
	return "", false
}
